package agent

// 计划与脚本的代理镜像读路由（P12.4）。
//
// 为什么需要这些路由：代理库是计划与脚本的**唯一一份**（P12.1/P12.2 定的规矩）。
// Web 只是渲染，所以它的 GET /schedules、GET /schedules/:id、GET /scripts、
// GET /scripts/:id 只能从这里取——Web 若再存一份，两边一定会有一刻不一致，
// 而那一刻正好是"界面说 03:30 会跑、systemd 说不会"。
//
// 写路径不在这里：计划与脚本的修改走 schedule.apply / script.apply 预检与任务
// （CONTRACTS §5），因为它们是系统写操作，要再认证、要落任务、要审计。
// 这两类路由反过来只读，且只允许 Web 账号（对端 UID 校验在最外层 gate）。

import (
	"context"
	"net/http"
	"strings"
	"time"

	"upanel/internal/httpx"
	"upanel/internal/schedules"
	"upanel/internal/scripts"
)

// SchedulesService 是计划任务对代理暴露的全部能力（生产实现是 *schedules.Module）。
type SchedulesService interface {
	scheduleSource
	Views(ctx context.Context) ([]schedules.View, error)
	View(ctx context.Context, id string) (schedules.View, bool, error)
	Triggers(ctx context.Context, id string, limit int) ([]schedules.TriggerRecord, error)
	DecideTrigger(ctx context.Context, scheduleID, triggerID string) (schedules.TriggerDecision, error)
	ClaimTrigger(ctx context.Context, d schedules.TriggerDecision, outcome, reason string) (string, bool, error)
}

// ScriptsService 是受管理脚本对代理暴露的全部能力（生产实现是 *scripts.Module）。
type ScriptsService interface {
	scriptSource
	List(ctx context.Context) ([]scripts.Record, error)
	Get(ctx context.Context, id string) (scripts.Record, bool, error)
}

// scheduleViewResponse 是一条计划对外的形状。
//
// 它把两件事并排放在一起：**意图**（定义、版本、启用与否）与**事实**
// （systemd 说它 enabled/active 吗、下次什么时候触发）。界面因此不需要猜，
// 也不需要在"已启用"与"真的在跑"之间自己编一句话。
type scheduleViewResponse struct {
	ID         string               `json:"id"`
	Name       string               `json:"name"`
	Kind       string               `json:"kind"`
	TargetID   string               `json:"target_id"`
	Definition schedules.Definition `json:"definition"`
	Enabled    bool                 `json:"enabled"`
	Version    int64                `json:"version"`
	TimerRef   string               `json:"timer_ref"`
	CreatedAt  string               `json:"created_at"`
	UpdatedAt  string               `json:"updated_at"`
	Installed  bool                 `json:"installed"`
	// UnitFileState / ActiveState 是 systemctl 的原话，不做二次解释。
	UnitFileState string `json:"unit_file_state"`
	ActiveState   string `json:"active_state"`
	// NextElapse 是下次触发的绝对时刻（来自 systemd-analyze）。
	NextElapse string   `json:"next_elapse,omitempty"`
	Blockers   []Notice `json:"blockers"`
}

// scheduleListResponse / scheduleDetailResponse 是响应体。
type scheduleListResponse struct {
	Schedules []scheduleViewResponse `json:"schedules"`
}

type scheduleDetailResponse struct {
	Schedule scheduleViewResponse  `json:"schedule"`
	Triggers []triggerViewResponse `json:"triggers"`
}

// triggerViewResponse 是一次触发的对外形状。
type triggerViewResponse struct {
	TriggerID   string `json:"trigger_id"`
	SlotAt      string `json:"slot_at"`
	Action      string `json:"action"`
	OperationID string `json:"operation_id"`
	Outcome     string `json:"outcome"`
	Reason      string `json:"reason,omitempty"`
	CreatedAt   string `json:"created_at"`
}

// scriptViewResponse 是一条脚本的对外形状。
//
// 列表不带 content（CONTRACTS §12：GET /scripts 只返回元数据）；详情才把正文交给
// 管理员——内容不是秘密，但列表里塞满脚本正文没任何用处。
type scriptViewResponse struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	ContentHash string `json:"content_hash"`
	Interpreter string `json:"interpreter"`
	Version     int64  `json:"version"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
	Content     string `json:"content,omitempty"`
}

// scriptListResponse / scriptDetailResponse 是响应体。
type scriptListResponse struct {
	Scripts []scriptViewResponse `json:"scripts"`
}

type scriptDetailResponse struct {
	Script scriptViewResponse `json:"script"`
}

// handleSchedules 处理 /internal/v1/schedules 与其子路径（只读）。
func (s *Server) handleSchedules(w http.ResponseWriter, r *http.Request) {
	svc := s.opts.Schedules
	if svc == nil {
		httpx.WriteError(w, r, http.StatusNotFound, httpx.CodeNotFound, "代理未启用计划任务")
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/internal/v1/schedules")
	rest = strings.Trim(rest, "/")
	if rest == "" {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			httpx.WriteError(w, r, http.StatusMethodNotAllowed, httpx.CodeValidationError, "只支持 GET")
			return
		}
		views, err := svc.Views(r.Context())
		if err != nil {
			s.internalError(w, r, "读取计划任务失败", err)
			return
		}
		out := scheduleListResponse{Schedules: make([]scheduleViewResponse, 0, len(views))}
		for _, v := range views {
			out.Schedules = append(out.Schedules, viewOfSchedule(v))
		}
		httpx.WriteData(w, r, http.StatusOK, out)
		return
	}

	// 子路径：<id>、<id>/trigger 或 <id>/trigger-runs/<op>。
	id, sub, _ := strings.Cut(rest, "/")
	if !schedules.ValidID(id) {
		httpx.WriteError(w, r, http.StatusNotFound, httpx.CodeNotFound, "计划任务不存在")
		return
	}
	if sub == "trigger" {
		s.handleScheduleTrigger(w, r, id)
		return
	}
	if opID, ok := strings.CutPrefix(sub, "trigger-runs/"); ok {
		s.handleScheduleTriggerRun(w, r, id, opID)
		return
	}
	if sub != "" {
		httpx.WriteError(w, r, http.StatusNotFound, httpx.CodeNotFound, "代理接口不存在")
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		httpx.WriteError(w, r, http.StatusMethodNotAllowed, httpx.CodeValidationError, "只支持 GET")
		return
	}
	view, found, err := svc.View(r.Context(), id)
	if err != nil {
		s.internalError(w, r, "读取计划任务失败", err)
		return
	}
	if !found {
		httpx.WriteError(w, r, http.StatusNotFound, httpx.CodeNotFound, "计划任务不存在")
		return
	}
	triggers, err := svc.Triggers(r.Context(), id, 20)
	if err != nil {
		s.internalError(w, r, "读取触发记录失败", err)
		return
	}
	out := scheduleDetailResponse{Schedule: viewOfSchedule(view), Triggers: make([]triggerViewResponse, 0, len(triggers))}
	for _, t := range triggers {
		out.Triggers = append(out.Triggers, triggerViewOf(t))
	}
	httpx.WriteData(w, r, http.StatusOK, out)
}

// handleScripts 处理 /internal/v1/scripts 与其子路径（只读）。
func (s *Server) handleScripts(w http.ResponseWriter, r *http.Request) {
	svc := s.opts.Scripts
	if svc == nil {
		httpx.WriteError(w, r, http.StatusNotFound, httpx.CodeNotFound, "代理未启用受管理脚本")
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		httpx.WriteError(w, r, http.StatusMethodNotAllowed, httpx.CodeValidationError, "只支持 GET")
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/internal/v1/scripts"), "/")
	if rest == "" {
		records, err := svc.List(r.Context())
		if err != nil {
			s.internalError(w, r, "读取脚本列表失败", err)
			return
		}
		out := scriptListResponse{Scripts: make([]scriptViewResponse, 0, len(records))}
		for _, rec := range records {
			out.Scripts = append(out.Scripts, viewOfScript(rec, false))
		}
		httpx.WriteData(w, r, http.StatusOK, out)
		return
	}
	if !scripts.ValidID(rest) {
		httpx.WriteError(w, r, http.StatusNotFound, httpx.CodeNotFound, "脚本不存在")
		return
	}
	rec, found, err := svc.Get(r.Context(), rest)
	if err != nil {
		s.internalError(w, r, "读取脚本失败", err)
		return
	}
	if !found {
		httpx.WriteError(w, r, http.StatusNotFound, httpx.CodeNotFound, "脚本不存在")
		return
	}
	httpx.WriteData(w, r, http.StatusOK, scriptDetailResponse{Script: viewOfScript(rec, true)})
}

// viewOfSchedule 把一条计划翻成对外形状。
func viewOfSchedule(v schedules.View) scheduleViewResponse {
	out := scheduleViewResponse{
		ID:            v.Record.ID,
		Name:          v.Record.Name,
		Kind:          string(v.Record.Kind),
		TargetID:      v.Record.TargetID,
		Definition:    v.Record.Definition,
		Enabled:       v.Record.Enabled,
		Version:       v.Record.Version,
		TimerRef:      v.Record.TimerRef,
		CreatedAt:     v.Record.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:     v.Record.UpdatedAt.UTC().Format(time.RFC3339),
		Installed:     v.Units.Present,
		UnitFileState: v.Units.UnitFileState,
		ActiveState:   v.Units.ActiveState,
		Blockers:      nonNilNotices(scheduleNoticeSlice(v.Blockers)),
	}
	if v.NextElapse != nil {
		out.NextElapse = v.NextElapse.UTC().Format(time.RFC3339)
	}
	return out
}

// triggerViewOf 把一次触发翻成对外形状。
func triggerViewOf(t schedules.TriggerRecord) triggerViewResponse {
	return triggerViewResponse{
		TriggerID:   t.TriggerID,
		SlotAt:      t.SlotAt.UTC().Format(time.RFC3339),
		Action:      t.Action,
		OperationID: t.OperationID,
		Outcome:     t.Outcome,
		Reason:      t.Reason,
		CreatedAt:   t.CreatedAt.UTC().Format(time.RFC3339),
	}
}

// viewOfScript 把一条脚本翻成对外形状（withContent 决定是否带正文）。
func viewOfScript(rec scripts.Record, withContent bool) scriptViewResponse {
	out := scriptViewResponse{
		ID:          rec.ID,
		Name:        rec.Name,
		ContentHash: rec.ContentHash,
		Interpreter: rec.Interpreter,
		Version:     rec.Version,
		CreatedAt:   rec.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:   rec.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if withContent {
		out.Content = rec.Content
	}
	return out
}
