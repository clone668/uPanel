package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"upanel/internal/apps"
	"upanel/internal/discovery"
	"upanel/internal/httpx"
)

// 应用发现与健康检查结果（P09.2）。
//
// 两件事都在“读”这一侧：
//  1. GET /apps/discovery 把代理发现的本机服务/容器列成候选，并标出“这个目标已经被哪个
//     应用绑定了”“哪个应用的绑定对象已经不在了”。**管理员确认候选**不是一个新接口：
//     确认就是 POST /apps（P09.1 的元数据写入），在这里再开一个 /apps/discovery/:id/confirm
//     等于把同一件事做两遍，还会让“创建应用”有两条代码路径。
//  2. GET /apps/:id/health 返回最近一次探测结论。探测本身由 Web 进程里的调度器按
//     interval_seconds 触发，结果写在 app_health_results 里，这个接口只读。
//
// 权限：发现列表是 admin（它暴露本机所有监听端口与容器，是侦察材料）；健康结果是 V
// （它只是“这个入口通不通”，任何已登录用户都能看到那个入口）。

// DiscoveryReader 是发现与探测的读取端（生产实现是 *discovery.Fetcher）。
//
// 允许为 nil（演示模式不连代理）：那时 /apps/discovery 与探测都明确回 503，
// 而不是给一份空候选列表——空列表会被读成“这台机器上没有任何服务”。
type DiscoveryReader interface {
	Snapshot(ctx context.Context) (discovery.Snapshot, error)
	Probe(ctx context.Context, params discovery.ProbeParams) (discovery.ProbeResult, error)
}

// appDiscoveryCandidate 是候选列表里的一项。
//
// 它是“发现到了什么” + “面板里已经有什么”的合并结果：前者来自代理，后者来自元数据表。
// 合并放在 Web 侧是因为代理不知道面板库里的事，而元数据表在 Web 库里。
type appDiscoveryCandidate struct {
	TargetID        string               `json:"target_id"`
	Kind            string               `json:"kind"`
	Source          string               `json:"source"`
	Name            string               `json:"name"`
	Title           string               `json:"title"`
	Reason          string               `json:"reason"`
	ObservedState   string               `json:"observed_state"`
	Listeners       []discovery.Listener `json:"listeners"`
	SuggestedScheme string               `json:"suggested_scheme"`
	SuggestedPort   int                  `json:"suggested_port"`
	ProbeAvailable  bool                 `json:"probe_available"`
	ProbeNote       string               `json:"probe_note"`
	Unit            string               `json:"unit,omitempty"`
	ContainerID     string               `json:"container_id,omitempty"`
	// BoundAppID / BoundAppName 非空表示这个目标已经被某个应用登记过了。
	// 一个目标只能属于一个应用（库层唯一索引），所以这里只可能有一个值。
	BoundAppID   *string `json:"bound_app_id"`
	BoundAppName *string `json:"bound_app_name"`
}

// appBindingIssue 描述一条“绑定对象已经不在了”的记录。
//
// 这对应 STAGES 第 11 节的验收项：容器重建后 ID 会变，旧绑定必须要求重新确认——
// 面板不会因为“名字看起来一样”就自动把它接到新容器上。
type appBindingIssue struct {
	AppID   string `json:"app_id"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Binding string `json:"binding"`
	Reason  string `json:"reason"`
}

// appDiscoveryResponse 是 GET /api/v1/apps/discovery 的响应体。
type appDiscoveryResponse struct {
	Status          string                  `json:"status"`
	Error           string                  `json:"error,omitempty"`
	CollectedAt     string                  `json:"collected_at"`
	Sources         []discovery.SourceState `json:"sources"`
	Candidates      []appDiscoveryCandidate `json:"candidates"`
	MissingBindings []appBindingIssue       `json:"missing_bindings"`
	Truncated       bool                    `json:"truncated"`
}

// handleAppDiscovery 返回本机候选列表，标出已绑定与失效绑定。
func (s *Server) handleAppDiscovery(c *gin.Context) {
	if s.opts.Discovery == nil {
		httpx.WriteError(c.Writer, c.Request, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"应用发现需要连接代理：当前部署没有可用的代理连接")
		return
	}
	snap, err := s.opts.Discovery.Snapshot(c.Request.Context())
	if err != nil {
		if errors.Is(err, discovery.ErrAgentUnavailable) {
			httpx.WriteError(c.Writer, c.Request, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
				"无法读取应用发现结果：代理不可用")
			return
		}
		s.opts.Logger.Error("读取应用发现结果失败", "error", err,
			"request_id", c.GetString(ctxKeyRequestID))
		httpx.WriteError(c.Writer, c.Request, http.StatusInternalServerError, httpx.CodeInternalError,
			"读取应用发现结果失败")
		return
	}

	bindings, err := s.opts.Apps.BindingIndex(c.Request.Context())
	if err != nil {
		s.opts.Logger.Error("读取应用绑定索引失败", "error", err,
			"request_id", c.GetString(ctxKeyRequestID))
		httpx.WriteError(c.Writer, c.Request, http.StatusInternalServerError, httpx.CodeInternalError,
			"读取应用绑定失败")
		return
	}
	bound, issues := mergeDiscoveryBindings(snap, bindings)

	resp := appDiscoveryResponse{
		Status:          snap.Status,
		Error:           snap.Error,
		CollectedAt:     snap.CollectedAt.UTC().Format(time.RFC3339),
		Sources:         snap.Sources,
		Candidates:      make([]appDiscoveryCandidate, 0, len(snap.Candidates)),
		MissingBindings: issues,
		Truncated:       snap.Truncated,
	}
	if resp.Sources == nil {
		resp.Sources = []discovery.SourceState{}
	}
	if resp.MissingBindings == nil {
		resp.MissingBindings = []appBindingIssue{}
	}
	for _, cand := range snap.Candidates {
		item := appDiscoveryCandidate{
			TargetID:        cand.TargetID,
			Kind:            cand.Kind,
			Source:          cand.Source,
			Name:            cand.Name,
			Title:           cand.Title,
			Reason:          cand.Reason,
			ObservedState:   cand.ObservedState,
			Listeners:       cand.Listeners,
			SuggestedScheme: cand.SuggestedScheme,
			SuggestedPort:   cand.SuggestedPort,
			ProbeAvailable:  cand.ProbeAvailable,
			ProbeNote:       cand.ProbeNote,
			Unit:            cand.Unit,
			ContainerID:     cand.ContainerID,
		}
		if item.Listeners == nil {
			item.Listeners = []discovery.Listener{}
		}
		if ref, ok := bound[candidateKey(cand)]; ok {
			id := ref.AppID
			name := ref.Name
			item.BoundAppID = &id
			item.BoundAppName = &name
		}
		resp.Candidates = append(resp.Candidates, item)
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, resp)
}

// mergeDiscoveryBindings 把应用绑定映射到候选上，并列出绑定对象已经不在了的应用。
//
// 匹配规则只看**身份**：systemd 用单元名，docker 用完整容器 ID。
// 不用容器名，也不做“名字差不多”的模糊匹配——容器重建后 ID 会变，用名字匹配会把
// 一个旧应用的入口悄悄接到新容器上，而那个人从没确认过这件事。
func mergeDiscoveryBindings(snap discovery.Snapshot, bindings []apps.BindingRef) (map[string]apps.BindingRef, []appBindingIssue) {
	bound := make(map[string]apps.BindingRef, len(bindings))
	units := make(map[string]struct{}, len(snap.Candidates))
	containers := make(map[string]struct{}, len(snap.Candidates))
	for _, cand := range snap.Candidates {
		if cand.Unit != "" {
			units[cand.Unit] = struct{}{}
		}
		if cand.ContainerID != "" {
			containers[cand.ContainerID] = struct{}{}
		}
	}

	issues := make([]appBindingIssue, 0)
	// 只有在对应来源这一轮真的采成了的时候，才敢说“这个绑定对不上”。没采成还报
	// “单元没在监听 / 容器不在了”，就是把“没看”说成“没有”：用户会照着这句话
	// 去删一条其实好好的应用。整个快照的 sources 里已经写着哪个来源失败了，
	// 这里不再补一条更肯定的假结论。
	unitChecked := sourceCollected(snap, discovery.SourceSystemd)
	dockerChecked := sourceCollected(snap, discovery.SourceDocker)
	for _, ref := range bindings {
		switch ref.Type {
		case apps.TypeSystemd:
			// 单元存在但**没有监听端口**时它不是候选，却也不该被报成“绑定失效”：
			// 一个跑着但不监听的服务仍然是有效的绑定对象，状态由服务页面负责。
			// 所以这里的判定是“发现结果里有没有这个对象”，不是“有没有这个候选”。
			if ref.Unit == "" {
				continue
			}
			if _, ok := units[ref.Unit]; ok {
				bound["systemd:"+ref.Unit] = ref
				continue
			}
			if !unitChecked {
				continue
			}
			issues = append(issues, appBindingIssue{
				AppID: ref.AppID, Name: ref.Name, Type: string(ref.Type), Binding: ref.Unit,
				Reason: "这个单元当前没有在监听任何端口：面板看不到它的候选。" +
					"如果它已经被停用或改名，请重新绑定或删除这条应用",
			})
		case apps.TypeDocker:
			if ref.ContainerID == "" {
				continue
			}
			if _, ok := containers[ref.ContainerID]; ok {
				bound["docker:"+ref.ContainerID] = ref
				continue
			}
			if !dockerChecked {
				continue
			}
			issues = append(issues, appBindingIssue{
				AppID: ref.AppID, Name: ref.Name, Type: string(ref.Type), Binding: ref.ContainerID,
				Reason: "容器 ID 已经不在本机的容器列表里（容器可能被重建过）。" +
					"重建出来的容器是一张新记录，需要重新确认后再绑定",
			})
		}
	}
	return bound, issues
}

// sourceCollected 判断某个发现来源这一轮是不是真的采到了东西。
//
// 没采成（error / unavailable）时不能拿“结果里没有”当成“对象不存在”：这两句话方向
// 相反，一句让人去修，一句让人去删。
func sourceCollected(snap discovery.Snapshot, source string) bool {
	for _, s := range snap.Sources {
		if s.Source == source {
			return s.Status == discovery.StatusOK
		}
	}
	return false
}

// candidateKey 是候选与应用绑定之间的匹配键。
func candidateKey(cand discovery.Candidate) string {
	switch cand.Kind {
	case discovery.KindDocker:
		return "docker:" + cand.ContainerID
	case discovery.KindSystemd:
		return "systemd:" + cand.Unit
	}
	// 裸监听没有可绑定的身份（进程名会变、端口会换），它只能作为“新建应用时的候选”：
	// 面板不会把一个应用的绑定系在一个 PID 或进程名上。
	return ""
}

// appHealthStateDTO 是 GET /api/v1/apps/:id/health 的响应体。
//
// 字段集合固定，读回来不需要猜缺了哪个键：没探过时 checked_at 是 null、
// http_state 是 unknown，而不是把字段整个省掉。
type appHealthStateDTO struct {
	AppID      string  `json:"app_id"`
	Enabled    bool    `json:"enabled"`
	HTTPState  string  `json:"http_state"`
	CheckedAt  *string `json:"checked_at"`
	StatusCode *int    `json:"status_code"`
	DurationMS *int64  `json:"duration_ms"`
	TargetID   string  `json:"target_id"`
	Scheme     string  `json:"scheme"`
	Port       int     `json:"port"`
	Path       string  `json:"path"`
	Address    *string `json:"address"`
	URL        *string `json:"url"`
	LastError  string  `json:"last_error"`
	// ExpectedStatusCodes 是定义里的期望集合；空数组表示默认 200—399。
	ExpectedStatusCodes []int `json:"expected_status_codes"`
	// Note 解释“为什么现在是这个状态”，尤其是 unknown：
	// 「还没有探测过」和「探测过但没得出结果」是两件事，界面必须能分开说。
	Note string `json:"note"`
}

// handleGetAppHealth 返回一个应用最近一次的健康探测结论。
func (s *Server) handleGetAppHealth(c *gin.Context) {
	sess := sessionFrom(c)
	app, err := s.opts.Apps.Get(c.Request.Context(), sess.User.ID, c.Param("id"))
	if err != nil {
		status, code, message, _, internal := mapAppError(err)
		if internal {
			s.opts.Logger.Error("读取应用失败", "error", err,
				"request_id", c.GetString(ctxKeyRequestID))
		}
		httpx.WriteError(c.Writer, c.Request, status, code, message)
		return
	}

	dto := appHealthStateDTO{
		AppID:               app.ID,
		Enabled:             app.Health.Enabled,
		HTTPState:           apps.ObservedHTTPState(app.Health, app.HealthResult),
		TargetID:            app.Health.TargetID,
		Scheme:              app.Health.Scheme,
		Port:                app.Health.Port,
		Path:                app.Health.Path,
		ExpectedStatusCodes: app.Health.ExpectedStatusCodes,
	}
	if dto.ExpectedStatusCodes == nil {
		dto.ExpectedStatusCodes = []int{}
	}
	if !app.Health.Enabled {
		dto.Note = "这个应用没有启用健康检查：面板不会向它发起任何请求"
		httpx.WriteData(c.Writer, c.Request, http.StatusOK, dto)
		return
	}
	if app.HealthResult == nil {
		dto.Note = "还没有探测结果：调度器会在下一个周期探测这个目标"
		httpx.WriteData(c.Writer, c.Request, http.StatusOK, dto)
		return
	}

	r := app.HealthResult
	checkedAt := r.CheckedAt.UTC().Format(time.RFC3339)
	dto.CheckedAt = &checkedAt
	dto.StatusCode = r.StatusCode
	duration := r.DurationMS
	dto.DurationMS = &duration
	dto.LastError = r.Error
	// 空串表示“这次没有真的发出请求”（例如代理不可用）。那种时候不填 address/url，
	// 而不是填一个 127.0.0.1 让人以为探测过。
	if r.Address != "" {
		address := r.Address
		dto.Address = &address
	}
	if r.URL != "" {
		url := r.URL
		dto.URL = &url
	}
	// 结论里的 target/scheme/port/path 是**上次实际探测用的值**：如果有人在这之后
	// 改了定义，这里显示的是历史事实，而定义在 app.health_check 里。
	dto.TargetID = r.TargetID
	dto.Scheme = r.Scheme
	dto.Port = r.Port
	dto.Path = r.Path
	if r.HTTPState == apps.HTTPUnknown {
		dto.Note = "最近一次探测没有得出结论，原因见 last_error"
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, dto)
}
