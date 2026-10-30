package agent

// 定时操作的对账读口（P12.4）。
//
// CONTRACTS §15 要求"代理产生的定时 operation 会被 Web 对账导入任务表，不能只存在于
// journal"。这个 GET 就是那条路的代理端：给出**某个操作者**在某个时刻之后创建的操作。
//
// 为什么必须由代理给：定时触发那条路径 Web 没参与（systemd → upanelctl →
// POST /internal/v1/schedules/:id/trigger），operation_id 是代理按确定性规则算出来的。
// 所以 Web 不可能凭本地记录知道"这两天夜里跑了什么"。
//
// 为什么只认 actor=scheduler：一个能"列出全部操作"的读口等于把代理的执行历史
// 变成一张随便翻的流水账——谁在什么时候动过这台机器的什么，全在里面。这里只回答
// 一个问题，其余一律 400。

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"upanel/internal/agentstore"
	"upanel/internal/httpx"
)

const (
	// scheduledOperationDefaultLimit 是默认批量。
	scheduledOperationDefaultLimit = 50
	// scheduledOperationMaxLimit 是上限（与 agentstore.MaxActorsBatch 一致）。
	scheduledOperationMaxLimit = agentstore.MaxActorsBatch
)

// scheduledOperationView 是一条定时操作的对外形状。
//
// 字段多，是因为 Web 要凭它**如实**落一条预检与一条任务：动作、规范化参数与哈希、
// 目标、会话哈希、阶段、结果。少一个，Web 就得猜；猜出来的任务记录比没有还坏。
type scheduledOperationView struct {
	OperationID string `json:"operation_id"`
	Action      string `json:"action"`
	// PreflightRef 是代理侧那份预检的引用。Web 会为它落一条本地预检记录，
	// 因为 jobs.preflight_id 是 NOT NULL 外键。
	PreflightRef string `json:"preflight_ref"`
	// Params 是规范化参数原文；ParamsHash 是它的 SHA-256。
	Params     json.RawMessage `json:"params"`
	ParamsHash string          `json:"params_hash"`
	// StateVersion / Target 是执行时冻结的目标快照。
	StateVersion string          `json:"state_version"`
	Target       json.RawMessage `json:"target"`
	// SessionHash 是定时触发的会话指纹（代理侧的 scheduler 会话哈希）。
	SessionHash string          `json:"session_hash"`
	Stage       string          `json:"stage"`
	Step        string          `json:"step"`
	Result      json.RawMessage `json:"result"`
	Error       json.RawMessage `json:"error"`
	CreatedAt   string          `json:"created_at"`
	// StartedAt / FinishedAt 未开始时为空串（不编一个时间）。
	StartedAt  string `json:"started_at,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`
	UpdatedAt  string `json:"updated_at"`
}

// scheduledOperationListResponse 是响应体。
type scheduledOperationListResponse struct {
	Operations []scheduledOperationView `json:"operations"`
}

// handleListScheduledOperations 处理 GET /internal/v1/operations?actor=scheduler&since=&since_id=&limit=。
func (s *Server) handleListScheduledOperations(w http.ResponseWriter, r *http.Request) {
	actor := strings.TrimSpace(r.URL.Query().Get("actor"))
	if actor != "scheduler" {
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationError,
			"actor 只接受 scheduler：这里只回答“定时触发跑了什么”，不提供列出全部操作的通道")
		return
	}
	var since time.Time
	if raw := strings.TrimSpace(r.URL.Query().Get("since")); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationError,
				"since 必须是 RFC3339 时间")
			return
		}
		since = t.UTC()
	}
	sinceID := strings.TrimSpace(r.URL.Query().Get("since_id"))
	if sinceID != "" && (since.IsZero() || !agentstore.ValidOperationID(sinceID)) {
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationError,
			"since_id 必须配合 since 使用，且是 op_ 加 32 位小写十六进制")
		return
	}
	limit := scheduledOperationDefaultLimit
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationError,
				"limit 必须是正整数")
			return
		}
		limit = n
		if limit > scheduledOperationMaxLimit {
			limit = scheduledOperationMaxLimit
		}
	}

	ops, err := s.opts.Operations.ListByActor(r.Context(), SchedulerUserID, since, sinceID, limit)
	if err != nil {
		s.internalError(w, r, "读取定时操作失败", err)
		return
	}
	out := scheduledOperationListResponse{Operations: make([]scheduledOperationView, 0, len(ops))}
	for _, op := range ops {
		out.Operations = append(out.Operations, scheduledOperationViewOf(op))
	}
	httpx.WriteData(w, r, http.StatusOK, out)
}

// scheduledOperationViewOf 把一条操作记录翻成对外形状。
func scheduledOperationViewOf(op agentstore.Operation) scheduledOperationView {
	out := scheduledOperationView{
		OperationID:  op.ID,
		Action:       op.Action,
		PreflightRef: op.PreflightRef,
		Params:       rawOrEmptyObject(op.Params),
		ParamsHash:   op.ParamsHash,
		StateVersion: op.StateVersion,
		Target:       rawOrEmptyObject(op.Target),
		SessionHash:  op.SessionHash,
		Stage:        op.Stage,
		Step:         op.Step,
		Result:       rawOrEmptyObject(op.Result),
		Error:        rawOrEmptyObject(op.ErrorJSON),
		CreatedAt:    op.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:    op.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if !op.StartedAt.IsZero() {
		out.StartedAt = op.StartedAt.UTC().Format(time.RFC3339)
	}
	if !op.FinishedAt.IsZero() {
		out.FinishedAt = op.FinishedAt.UTC().Format(time.RFC3339)
	}
	return out
}

// rawOrEmptyObject 把空字节换成 {}，让读取方拿到的一定是合法 JSON。
func rawOrEmptyObject(b []byte) json.RawMessage {
	if len(b) == 0 {
		return json.RawMessage("{}")
	}
	return json.RawMessage(b)
}
