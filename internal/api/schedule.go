package api

// 计划任务与受管理脚本的只读接口（P12.4，CONTRACTS.md 第 12 节）。
//
// 与 /backup/* 同一个理由：计划与脚本**只有代理库里那一份**。systemd 单元是按那份
// 定义写出来的，触发也要回代理取动作——面板若存第二份，就会有一刻两边不一致，而
// 那一刻正好是"界面说 03:30 会跑、systemd 说不会"。所以这四个接口都是代理镜像的
// 直读：代理不在时回 503 AGENT_UNAVAILABLE，**不给空列表**——空列表会被读成
// "这台机器上没有定时任务"。
//
// 这一组一个字节都不写：建立/修改/删除是 schedule.apply、schedule.delete、
// script.apply、script.delete 的预检 → 任务，执行是 script.run，全部要再认证与审计。
// 在这里开一个"直接写定义"的路由，等于把那条链路上的检查全绕过去。
//
// 权限：整组 admin。计划里写着这台机器什么时间会做什么（备份到哪儿、续哪张证书、
// 以 upanel-shell 身份跑哪段脚本），viewer 不需要知道；脚本正文更是如此。

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"upanel/internal/agentclient"
	"upanel/internal/httpx"
	"upanel/internal/schedules"
	"upanel/internal/scripts"
)

// SchedulesReader 是计划任务镜像的调用端（P12.4）。生产实现是 *agentclient.Client。
//
// 允许为 nil（演示模式不连代理）：那时 /schedules* 明确回 503，而不是给一份空列表。
type SchedulesReader interface {
	ListSchedules(ctx context.Context) ([]agentclient.Schedule, error)
	GetSchedule(ctx context.Context, id string) (agentclient.ScheduleDetail, error)
}

// ScriptsReader 是受管理脚本镜像的调用端（P12.4）。生产实现是 *agentclient.Client。
type ScriptsReader interface {
	ListScripts(ctx context.Context) ([]agentclient.Script, error)
	GetScript(ctx context.Context, id string) (agentclient.Script, error)
}

// scheduleDTO 是一条计划的对外形状。
//
// 意图（enabled/version/timer_ref）与事实（installed/unit_file_state/active_state/
// next_elapse）**分开写**，不合成一句"已启用"。界面因此能如实说出"定义是启用的，
// 但单元没装"这种状态，而不是替 systemd 编一个答案。
type scheduleDTO struct {
	ID         string                `json:"id"`
	Name       string                `json:"name"`
	Kind       string                `json:"kind"`
	TargetID   string                `json:"target_id"`
	Definition scheduleDefinitionDTO `json:"definition"`
	// Enabled 是意图：下一次 apply 会不会写启用的单元。
	Enabled bool  `json:"enabled"`
	Version int64 `json:"version"`
	// TimerRef 未安装时为空字符串。
	TimerRef  string `json:"timer_ref"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	// Installed / UnitFileState / ActiveState 是 systemd 的原话。
	Installed     bool   `json:"installed"`
	UnitFileState string `json:"unit_file_state"`
	ActiveState   string `json:"active_state"`
	// NextElapse 读不到时**不出现**（而不是写零值时间）。
	NextElapse string `json:"next_elapse,omitempty"`
	// Blockers 永不为 null：[] 表示"现在没有阻塞项"，null 会被读成"没检查过"。
	Blockers []noticeDTO `json:"blockers"`
}

// scheduleDefinitionDTO 是 ScheduleDefinition（CONTRACTS §12 的字段，平铺）。
type scheduleDefinitionDTO struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	TargetID string `json:"target_id"`
	// Certificate 只在证书续期计划上出现（P12.5）：定时触发那一天 systemd 里没有
	// 面板可问，所以"续哪个域名、用哪个 ACME 环境"必须随定义一起存下来。
	Certificate        *certificateParamsDTO `json:"certificate,omitempty"`
	CalendarExpression string                `json:"calendar_expression"`
	Timezone           string                `json:"timezone"`
	// Persistent 是错过执行策略：停机期间错过的触发在开机后补跑一次。
	Persistent bool `json:"persistent"`
	// MaxRuntimeSeconds 是单次运行时长上限。
	MaxRuntimeSeconds int  `json:"max_runtime_seconds"`
	Enabled           bool `json:"enabled"`
}

// certificateParamsDTO 是证书续期计划的种类参数。
type certificateParamsDTO struct {
	Domain  string `json:"domain"`
	Staging bool   `json:"staging"`
}

// noticeDTO 是一条阻塞项/提示。
type noticeDTO struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// scheduleTriggerDTO 是一次触发记录。
type scheduleTriggerDTO struct {
	TriggerID   string `json:"trigger_id"`
	SlotAt      string `json:"slot_at"`
	Action      string `json:"action"`
	OperationID string `json:"operation_id"`
	// Outcome 是触发当时的判决闭集。
	Outcome   string `json:"outcome"`
	Reason    string `json:"reason,omitempty"`
	CreatedAt string `json:"created_at"`
}

// scheduleListResponse / scheduleDetailResponse 是响应体。
type scheduleListResponse struct {
	Items []scheduleDTO `json:"items"`
}

type scheduleDetailResponse struct {
	Schedule scheduleDTO          `json:"schedule"`
	Triggers []scheduleTriggerDTO `json:"triggers"`
}

// scriptDTO 是一条受管理脚本。列表不带 content，详情才带。
type scriptDTO struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	ContentHash string `json:"content_hash"`
	// Interpreter 由代理决定，调用方不能指定。
	Interpreter string `json:"interpreter"`
	Version     int64  `json:"version"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
	// Content 只在详情里出现（CONTRACTS §12：列表只返回元数据）。
	Content string `json:"content,omitempty"`
}

type scriptListResponse struct {
	Items []scriptDTO `json:"items"`
}

type scriptDetailResponse struct {
	Script scriptDTO `json:"script"`
}

// handleListSchedules 处理 GET /api/v1/schedules（admin）。
func (s *Server) handleListSchedules(c *gin.Context) {
	reader := s.opts.Schedules
	if reader == nil {
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"计划任务不可用：未配置代理连接（计划只存在代理里，面板没有本地副本）")
		return
	}
	list, err := reader.ListSchedules(c.Request.Context())
	if err != nil {
		s.writeSchedulesError(c, "读取计划任务列表失败", err)
		return
	}
	out := scheduleListResponse{Items: make([]scheduleDTO, 0, len(list))}
	for _, item := range list {
		out.Items = append(out.Items, scheduleDTOOf(item))
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, out)
}

// handleGetSchedule 处理 GET /api/v1/schedules/:id（admin）。
func (s *Server) handleGetSchedule(c *gin.Context) {
	reader := s.opts.Schedules
	if reader == nil {
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"计划任务不可用：未配置代理连接（计划只存在代理里，面板没有本地副本）")
		return
	}
	id := c.Param("id")
	// 先拦形状：`../../etc/passwd` 这种应当回 400 说"参数不对"，而不是让代理
	// 回一个 404 让它看起来像"这条计划不存在"。
	if !schedules.ValidID(id) {
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError,
			"计划 id 形态不合法（应为 sch_ 加 32 位小写十六进制）")
		return
	}
	detail, err := reader.GetSchedule(c.Request.Context(), id)
	if err != nil {
		s.writeSchedulesError(c, "读取计划任务详情失败", err)
		return
	}
	out := scheduleDetailResponse{
		Schedule: scheduleDTOOf(detail.Schedule),
		Triggers: make([]scheduleTriggerDTO, 0, len(detail.Triggers)),
	}
	for _, t := range detail.Triggers {
		out.Triggers = append(out.Triggers, scheduleTriggerDTOOf(t))
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, out)
}

// handleListScripts 处理 GET /api/v1/scripts（admin）。
func (s *Server) handleListScripts(c *gin.Context) {
	reader := s.opts.Scripts
	if reader == nil {
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"脚本列表不可用：未配置代理连接（脚本只存在代理里，面板没有本地副本）")
		return
	}
	list, err := reader.ListScripts(c.Request.Context())
	if err != nil {
		s.writeScriptsError(c, "读取脚本列表失败", err)
		return
	}
	out := scriptListResponse{Items: make([]scriptDTO, 0, len(list))}
	for _, item := range list {
		out.Items = append(out.Items, scriptDTOOf(item, false))
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, out)
}

// handleGetScript 处理 GET /api/v1/scripts/:id（admin，含正文）。
func (s *Server) handleGetScript(c *gin.Context) {
	reader := s.opts.Scripts
	if reader == nil {
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"脚本不可用：未配置代理连接（脚本只存在代理里，面板没有本地副本）")
		return
	}
	id := c.Param("id")
	if !scripts.ValidID(id) {
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError,
			"脚本 id 形态不合法（应为 scr_ 加 32 位小写十六进制）")
		return
	}
	item, err := reader.GetScript(c.Request.Context(), id)
	if err != nil {
		s.writeScriptsError(c, "读取脚本详情失败", err)
		return
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, scriptDetailResponse{Script: scriptDTOOf(item, true)})
}

// scheduleDTOOf 把一条计划翻成对外形状。
func scheduleDTOOf(v agentclient.Schedule) scheduleDTO {
	out := scheduleDTO{
		ID:       v.ID,
		Name:     v.Name,
		Kind:     v.Kind,
		TargetID: v.TargetID,
		Definition: scheduleDefinitionDTO{
			Name:               v.Def.Name,
			Kind:               string(v.Def.Kind),
			TargetID:           v.Def.TargetID,
			CalendarExpression: v.Def.CalendarExpression,
			Timezone:           v.Def.Timezone,
			Persistent:         v.Def.Persistent,
			MaxRuntimeSeconds:  v.Def.MaxRuntimeSeconds,
			Enabled:            v.Def.Enabled,
		},
		Enabled:       v.Enabled,
		Version:       v.Version,
		TimerRef:      v.TimerRef,
		CreatedAt:     backupTime(v.CreatedAt),
		UpdatedAt:     backupTime(v.UpdatedAt),
		Installed:     v.Installed,
		UnitFileState: v.UnitFileState,
		ActiveState:   v.ActiveState,
		NextElapse:    backupTime(v.NextElapse),
		Blockers:      make([]noticeDTO, 0, len(v.Blockers)),
	}
	if c := v.Def.Certificate; c != nil {
		out.Definition.Certificate = &certificateParamsDTO{Domain: c.Domain, Staging: c.Staging}
	}
	for _, b := range v.Blockers {
		out.Blockers = append(out.Blockers, noticeDTO{Code: b.Code, Message: b.Message})
	}
	return out
}

// scheduleTriggerDTOOf 把一次触发翻成对外形状。
func scheduleTriggerDTOOf(t agentclient.ScheduleTrigger) scheduleTriggerDTO {
	return scheduleTriggerDTO{
		TriggerID:   t.TriggerID,
		SlotAt:      backupTime(t.SlotAt),
		Action:      t.Action,
		OperationID: t.OperationID,
		Outcome:     t.Outcome,
		Reason:      t.Reason,
		CreatedAt:   backupTime(t.CreatedAt),
	}
}

// scriptDTOOf 把一条脚本翻成对外形状（withContent 决定是否带正文）。
func scriptDTOOf(rec agentclient.Script, withContent bool) scriptDTO {
	out := scriptDTO{
		ID:          rec.ID,
		Name:        rec.Name,
		ContentHash: rec.ContentHash,
		Interpreter: rec.Interpreter,
		Version:     rec.Version,
		CreatedAt:   backupTime(rec.CreatedAt),
		UpdatedAt:   backupTime(rec.UpdatedAt),
	}
	if withContent {
		out.Content = rec.Content
	}
	return out
}

// writeSchedulesError / writeScriptsError 把读取错误翻成 HTTP 状态。
//
// 与备份那组同一个翻译表：代理不可用 → 503（等一会儿会好）；代理明确说没有 →
// 404（这条计划真的不在）；其余 → 500，日志里留完整错误。**没有**"降级成空列表"
// 的分支：读不到和没有在界面上必须能分开。
func (s *Server) writeSchedulesError(c *gin.Context, what string, err error) {
	s.writeMirrorError(c, what, "计划任务", err)
}

func (s *Server) writeScriptsError(c *gin.Context, what string, err error) {
	s.writeMirrorError(c, what, "脚本", err)
}

func (s *Server) writeMirrorError(c *gin.Context, what, noun string, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	var unavailable *agentclient.UnavailableError
	var agentErr *agentclient.Error
	switch {
	case errors.As(err, &unavailable):
		s.opts.Logger.Warn(what+"：代理不可用", "error", err,
			"request_id", c.GetString(ctxKeyRequestID))
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"特权代理不可用，暂时读不到"+noun)
	case errors.As(err, &agentErr):
		code := httpx.Code(agentErr.Code)
		if !knownCode(code) {
			s.opts.Logger.Error("代理返回未知错误码", "code", agentErr.Code,
				"request_id", c.GetString(ctxKeyRequestID))
			writeError(c, http.StatusInternalServerError, httpx.CodeInternalError,
				"代理返回未知错误码")
			return
		}
		writeError(c, httpx.StatusForCode(code), code, agentErr.Message)
	default:
		s.internalError(c, what, err)
	}
}

// 审计用的目标形状：一条计划 / 一条脚本，而不是"某个用户"。
func scheduleAuditTarget(id string) string { return "schedule:" + id }

func scriptAuditTarget(id string) string { return "script:" + id }
