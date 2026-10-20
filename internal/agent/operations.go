package agent

// P03.2：持久操作、提交与取消（CONTRACTS.md 4.2、4.3、14.2、15）。
//
// 这一层是“系统写操作真正开始的地方”，规则只有三条，但每一条都必须成立：
//
//  1. **接受即持久**。POST /internal/v1/operations 在返回之前把操作记录写进代理库：
//     进程随后崩溃，重启后仍能按 operation_id 查到“接受过、但结果未知”，
//     而不是像没发生过一样。预检的消费与操作记录的写入在同一个事务里。
//  2. **接受不等于执行**。返回 201 之后执行在后台进行；执行前重新计算预检
//     （重新核对目标身份与状态版本），任何变化都拒绝，即使操作已经被接受。
//  3. **什么都不自动重跑**。启动时把上次运行遗留的非终态操作标成 interrupted，
//     让人去看真实状态——破坏性动作自动重试等于把“可能已经格式化”变成“再格式化一次”。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"upanel/internal/agentstore"
	"upanel/internal/httpx"
)

const (
	// maxOperationRequestBytes 是 POST /internal/v1/operations 的请求体上限。
	// 只有标识、短语与操作者上下文，不需要参数空间。
	maxOperationRequestBytes = 16 << 10

	// ReauthWindow 是再认证的有效窗口（CONTRACTS.md 第 2 节：5 分钟）。
	ReauthWindow = 5 * time.Minute

	// OperationTimeout 是单次执行的时限上限。
	//
	// P03.2 先用一个统一上限，保证“执行器卡住”不会永久占着 worker 与资源锁；
	// 按动作细化时限属于 P04（每个动作的登记项会带自己的时限）。
	OperationTimeout = 10 * time.Minute
)

// operationRequest 是 POST /internal/v1/operations 的请求体（CONTRACTS.md 第 15 节）。
//
// 没有 action、没有 params：提交时不能替换目标，参数只从预检里取（CONTRACTS.md 4.2）。
type operationRequest struct {
	OperationID  string       `json:"operation_id"`
	PreflightRef string       `json:"preflight_ref"`
	Confirmation string       `json:"confirmation"`
	Actor        ActorContext `json:"actor_context"`
}

// operationTargetResponse 是操作视图里的目标信息（用于对账时确认“是同一个对象”）。
type operationTargetResponse struct {
	DisplayName         string `json:"display_name"`
	IdentityFingerprint string `json:"identity_fingerprint"`
}

// operationResponse 是操作视图（内部协议）。
//
// 刻意不含 state_version 与 agent_ref 之类的内部字段：Web 需要的是“现在到哪一步了、
// 结果是什么、还能不能取消”，把这些混进去只会让公开 DTO 慢慢长出内部字段。
type operationResponse struct {
	OperationID           string                  `json:"operation_id"`
	Action                string                  `json:"action"`
	PreflightRef          string                  `json:"preflight_ref"`
	Stage                 string                  `json:"stage"`
	Step                  string                  `json:"step"`
	ProgressPercent       *int                    `json:"progress_percent"`
	Cancellable           bool                    `json:"cancellable"`
	CancellationRequested bool                    `json:"cancellation_requested"`
	Target                operationTargetResponse `json:"target"`
	Result                json.RawMessage         `json:"result"`
	Error                 json.RawMessage         `json:"error"`
	CreatedAt             string                  `json:"created_at"`
	StartedAt             string                  `json:"started_at,omitempty"`
	FinishedAt            string                  `json:"finished_at,omitempty"`
	UpdatedAt             string                  `json:"updated_at"`
	// Duplicate 表示这次提交是重放（同一个 operation_id 与同一份请求）。
	// 响应体里带上它，让 Web 能区分“我刚创建”和“我回来了”，而不用比对时间戳。
	Duplicate bool `json:"duplicate"`
}

// operationEventsResponse 是 GET /internal/v1/operations/:id/events 的响应体。
type operationEventsResponse struct {
	OperationID string                 `json:"operation_id"`
	Events      []operationEventRecord `json:"events"`
}

type operationEventRecord struct {
	Seq       int64           `json:"seq"`
	Kind      string          `json:"kind"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt string          `json:"created_at"`
}

// OperationStore 是代理需要的执行记录能力（由 agentstore.Operations 实现）。
type OperationStore interface {
	Accept(ctx context.Context, op agentstore.Operation) (agentstore.Operation, bool, error)
	Get(ctx context.Context, id string) (agentstore.Operation, error)
	MarkRunning(ctx context.Context, id, step string, now time.Time) error
	Finish(ctx context.Context, id string, stage, step string, result, errJSON []byte, now time.Time) error
	RequestCancel(ctx context.Context, id string, now time.Time) (agentstore.Operation, error)
	AppendEvent(ctx context.Context, id, kind string, payload map[string]any, now time.Time) error
	Events(ctx context.Context, id string) ([]agentstore.OperationEvent, error)
	ListUnfinished(ctx context.Context) ([]agentstore.Operation, error)
}

// handleOperations 处理 POST /internal/v1/operations：接受一次操作。
func (s *Server) handleOperations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		httpx.WriteError(w, r, http.StatusMethodNotAllowed, httpx.CodeValidationError, "只支持 POST")
		return
	}
	if ct := r.Header.Get("Content-Type"); ct != "" && !jsonContentType(ct) {
		httpx.WriteError(w, r, http.StatusUnsupportedMediaType, httpx.CodeValidationError,
			"请求体必须是 application/json")
		return
	}
	body, ok := readLimitedBody(w, r, maxOperationRequestBytes)
	if !ok {
		return
	}
	var req operationRequest
	if err := decodeStrictObject(body, &req); err != nil {
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationError, err.Error())
		return
	}
	if !agentstore.ValidOperationID(req.OperationID) {
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationError,
			"operation_id 必须是 op_ 加 32 位小写十六进制")
		return
	}
	if !agentstore.ValidPlanRef(req.PreflightRef) {
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationError,
			"preflight_ref 必须是 pf_ 加 32 位小写十六进制")
		return
	}
	if err := validateActorContext(req.Actor); err != nil {
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationError, err.Error())
		return
	}

	now := s.nowUTC().Truncate(time.Second)

	plan, err := s.opts.Plans.Get(r.Context(), req.PreflightRef)
	if err != nil {
		if errors.Is(err, agentstore.ErrPlanNotFound) {
			s.opts.Logger.Warn("提交引用了不存在的预检",
				"preflight_ref", req.PreflightRef, "operation_id", req.OperationID,
				"request_id", httpx.RequestID(r))
			httpx.WriteError(w, r, http.StatusConflict, httpx.CodePreflightExpired,
				"预检不存在、已过期或已被使用")
			return
		}
		s.internalError(w, r, "读取预检失败", err)
		return
	}

	// 绑定关系逐个复核：预检属于谁、属于哪个会话、还能不能用、允不允许执行。
	if plan.UserID != req.Actor.UserID {
		s.opts.Logger.Warn("拒绝使用他人的预检",
			"preflight_ref", req.PreflightRef, "operation_id", req.OperationID,
			"plan_user_id", plan.UserID, "actor_user_id", req.Actor.UserID,
			"request_id", httpx.RequestID(r))
		httpx.WriteError(w, r, http.StatusForbidden, httpx.CodeForbidden, "该预检不属于当前用户")
		return
	}
	if plan.SessionHash != req.Actor.SessionIDHash {
		httpx.WriteError(w, r, http.StatusConflict, httpx.CodePreflightStale,
			"预检绑定的会话已变化，请重新预检")
		return
	}
	if !now.Before(plan.ExpiresAt) {
		httpx.WriteError(w, r, http.StatusConflict, httpx.CodePreflightExpired,
			"预检已过期，请重新预检")
		return
	}
	if !plan.Allowed {
		httpx.WriteError(w, r, http.StatusConflict, httpx.CodeValidationError,
			"预检结果为不允许执行："+blockerCodes(plan.Blockers))
		return
	}
	if plan.PhraseHash != "" {
		if hashHex([]byte(req.Confirmation)) != plan.PhraseHash {
			httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeConfirmationMismatch,
				"确认短语不正确")
			return
		}
	}
	if plan.RequiresReauth {
		if err := checkReauth(req.Actor.ReauthAt, now); err != nil {
			httpx.WriteError(w, r, http.StatusForbidden, httpx.CodeReauthRequired, err.Error())
			return
		}
	}

	action, found := s.registry.Action(plan.Action)
	if !found {
		// 预检里记着动作，登记表却没有：登记表变了（降级/回滚）。不能执行。
		s.opts.Logger.Error("预检指向未登记的动作",
			"action", plan.Action, "preflight_ref", req.PreflightRef, "request_id", httpx.RequestID(r))
		httpx.WriteError(w, r, http.StatusConflict, httpx.CodeValidationError,
			fmt.Sprintf("动作 %q 当前不可用", plan.Action))
		return
	}
	if action.Execute == nil {
		s.opts.Logger.Error("动作缺少执行实现", "action", plan.Action, "request_id", httpx.RequestID(r))
		httpx.WriteError(w, r, http.StatusNotImplemented, httpx.CodeNotImplemented,
			fmt.Sprintf("动作 %q 尚未实现执行", plan.Action))
		return
	}

	// 执行前复核：拿预检里的规范化参数重新算一遍计划，比对目标身份与状态版本。
	// 这一步在**接受之前**做，所以“预检过期/目标变了”会以 4xx 拒绝，不会留下一个注定失败的任务。
	if err := s.revalidate(r.Context(), action, plan, now); err != nil {
		var rej *rejection
		if errors.As(err, &rej) {
			httpx.WriteError(w, r, rej.status, rej.code, rej.message)
			return
		}
		s.internalError(w, r, "执行前复核失败", err)
		return
	}

	requestHash := operationRequestHash(plan, req.Actor)
	record := agentstore.Operation{
		ID:                  req.OperationID,
		RequestHash:         requestHash,
		Action:              plan.Action,
		PreflightRef:        req.PreflightRef,
		Params:              plan.Params,
		ParamsHash:          plan.ParamsHash,
		IdentityFingerprint: plan.IdentityFingerprint,
		StateVersion:        plan.StateVersion,
		Target:              plan.Target,
		Locks:               plan.Locks,
		UserID:              req.Actor.UserID,
		SessionHash:         req.Actor.SessionIDHash,
		RequestID:           httpx.RequestID(r),
		Stage:               agentstore.StageAccepted,
		CreatedAt:           now,
		UpdatedAt:           now,
	}

	accepted, duplicate, err := s.opts.Operations.Accept(r.Context(), record)
	if err != nil {
		switch {
		case errors.Is(err, agentstore.ErrPreflightExpired):
			httpx.WriteError(w, r, http.StatusConflict, httpx.CodePreflightExpired,
				"预检不存在、已过期或已被使用")
		case errors.Is(err, agentstore.ErrOperationConflict):
			s.opts.Logger.Warn("同一个 operation_id 被用于不同请求",
				"operation_id", req.OperationID, "preflight_ref", req.PreflightRef,
				"request_id", httpx.RequestID(r))
			httpx.WriteError(w, r, http.StatusConflict, httpx.CodeIdempotencyConflict,
				"operation_id 已用于另一个请求")
		default:
			s.internalError(w, r, "接受操作失败", err)
		}
		return
	}

	if duplicate {
		s.opts.Logger.Info("操作提交是重放，返回已有记录",
			"operation_id", accepted.ID, "stage", accepted.Stage,
			"preflight_ref", accepted.PreflightRef, "request_id", httpx.RequestID(r))
		s.writeOperation(w, r, accepted, true, http.StatusOK)
		return
	}

	s.opts.Logger.Info("已接受操作",
		"operation_id", accepted.ID,
		"action", accepted.Action,
		"preflight_ref", accepted.PreflightRef,
		"user_id", accepted.UserID,
		"locks", len(plan.Locks),
		"request_id", httpx.RequestID(r))

	// 接受之后才起后台执行：返回 201 时操作已经落库，执行成败都不影响“它存在过”。
	s.runner.start(accepted)
	s.writeOperation(w, r, accepted, false, http.StatusCreated)
}

// handleOperationByID 处理 /internal/v1/operations/<id>、/events 与 /cancel。
func (s *Server) handleOperationByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/internal/v1/operations/")
	if rest == "" || strings.HasSuffix(rest, "/") {
		httpx.WriteError(w, r, http.StatusNotFound, httpx.CodeNotFound, "代理接口不存在")
		return
	}
	parts := strings.Split(rest, "/")
	id := parts[0]
	if !agentstore.ValidOperationID(id) {
		httpx.WriteError(w, r, http.StatusNotFound, httpx.CodeNotFound, "操作不存在")
		return
	}

	if len(parts) == 1 {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			httpx.WriteError(w, r, http.StatusMethodNotAllowed, httpx.CodeValidationError, "只支持 GET")
			return
		}
		op, err := s.opts.Operations.Get(r.Context(), id)
		if err != nil {
			s.writeOperationLookupError(w, r, id, err)
			return
		}
		s.writeOperation(w, r, op, false, http.StatusOK)
		return
	}

	switch parts[1] {
	case "events":
		if len(parts) != 2 {
			httpx.WriteError(w, r, http.StatusNotFound, httpx.CodeNotFound, "代理接口不存在")
			return
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			httpx.WriteError(w, r, http.StatusMethodNotAllowed, httpx.CodeValidationError, "只支持 GET")
			return
		}
		s.handleOperationEvents(w, r, id)
	case "cancel":
		if len(parts) != 2 {
			httpx.WriteError(w, r, http.StatusNotFound, httpx.CodeNotFound, "代理接口不存在")
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			httpx.WriteError(w, r, http.StatusMethodNotAllowed, httpx.CodeValidationError, "只支持 POST")
			return
		}
		s.handleOperationCancel(w, r, id)
	default:
		httpx.WriteError(w, r, http.StatusNotFound, httpx.CodeNotFound, "代理接口不存在")
	}
}

// handleOperationEvents 返回操作的代理侧事件流水。
func (s *Server) handleOperationEvents(w http.ResponseWriter, r *http.Request, id string) {
	if _, err := s.opts.Operations.Get(r.Context(), id); err != nil {
		s.writeOperationLookupError(w, r, id, err)
		return
	}
	events, err := s.opts.Operations.Events(r.Context(), id)
	if err != nil {
		s.internalError(w, r, "读取操作事件失败", err)
		return
	}
	out := operationEventsResponse{OperationID: id, Events: make([]operationEventRecord, 0, len(events))}
	for _, ev := range events {
		out.Events = append(out.Events, operationEventRecord{
			Seq:       ev.Seq,
			Kind:      ev.Kind,
			Payload:   ev.Payload,
			CreatedAt: ev.CreatedAt.Format(time.RFC3339),
		})
	}
	httpx.WriteData(w, r, http.StatusOK, out)
}

// handleOperationCancel 请求安全取消。
//
// 拒绝的三种情形都返回 409 而不是 200：不可取消的动作、已经结束的操作、
// 以及“取消请求已被记录但操作还在跑”——最后一种返回 202 并带 cancellation_requested=true。
func (s *Server) handleOperationCancel(w http.ResponseWriter, r *http.Request, id string) {
	op, err := s.opts.Operations.Get(r.Context(), id)
	if err != nil {
		s.writeOperationLookupError(w, r, id, err)
		return
	}
	if agentstore.IsTerminalStage(op.Stage) {
		httpx.WriteError(w, r, http.StatusConflict, httpx.CodeVersionConflict,
			fmt.Sprintf("操作已结束（%s），不能取消", op.Stage))
		return
	}
	action, found := s.registry.Action(op.Action)
	if !found || !action.Policy.Cancellable {
		httpx.WriteError(w, r, http.StatusConflict, httpx.CodeValidationError,
			fmt.Sprintf("动作 %q 不支持安全取消", op.Action))
		return
	}

	updated, err := s.opts.Operations.RequestCancel(r.Context(), id, s.nowUTC().Truncate(time.Second))
	if err != nil {
		if errors.Is(err, agentstore.ErrOperationFinished) {
			httpx.WriteError(w, r, http.StatusConflict, httpx.CodeVersionConflict, "操作已结束，不能取消")
			return
		}
		s.internalError(w, r, "请求取消失败", err)
		return
	}

	// running 的操作：把取消信号递给执行器，剩下的由它自己收尾。
	if updated.Stage == agentstore.StageRunning || updated.CancellationRequested {
		s.runner.cancel(id)
	}
	s.opts.Logger.Info("已处理取消请求",
		"operation_id", id, "stage", updated.Stage,
		"cancellation_requested", updated.CancellationRequested,
		"request_id", httpx.RequestID(r))

	s.writeOperation(w, r, updated, false, http.StatusAccepted)
}

// writeOperationLookupError 统一处理“查不到”的分支。
func (s *Server) writeOperationLookupError(w http.ResponseWriter, r *http.Request, id string, err error) {
	if errors.Is(err, agentstore.ErrOperationNotFound) {
		httpx.WriteError(w, r, http.StatusNotFound, httpx.CodeNotFound, "操作不存在")
		return
	}
	s.internalError(w, r, "读取操作失败", err)
}

// writeOperation 输出一个操作视图。
func (s *Server) writeOperation(w http.ResponseWriter, r *http.Request, op agentstore.Operation, duplicate bool, status int) {
	httpx.WriteData(w, r, status, s.operationView(op, duplicate))
}

func (s *Server) operationView(op agentstore.Operation, duplicate bool) operationResponse {
	cancellable := false
	if action, found := s.registry.Action(op.Action); found {
		cancellable = action.Policy.Cancellable && !agentstore.IsTerminalStage(op.Stage)
	}
	view := operationResponse{
		OperationID:           op.ID,
		Action:                op.Action,
		PreflightRef:          op.PreflightRef,
		Stage:                 op.Stage,
		Step:                  op.Step,
		ProgressPercent:       nil, // P03.2 不报百分比：不知道就不编（CONTRACTS.md 4.3）
		Cancellable:           cancellable,
		CancellationRequested: op.CancellationRequested,
		Result:                nonEmptyJSON(op.Result),
		Error:                 nonEmptyJSON(op.ErrorJSON),
		CreatedAt:             op.CreatedAt.Format(time.RFC3339),
		FinishedAt:            formatOptionalTime(op.FinishedAt),
		UpdatedAt:             op.UpdatedAt.Format(time.RFC3339),
		Duplicate:             duplicate,
	}
	if !op.StartedAt.IsZero() {
		view.StartedAt = op.StartedAt.Format(time.RFC3339)
	}
	if target, err := decodePlanTarget(op); err == nil {
		view.Target = target
	}
	return view
}

// decodePlanTarget 从预检里读出目标展示信息；预检行已被清理时留空。
func decodePlanTarget(op agentstore.Operation) (operationTargetResponse, error) {
	var t operationTargetResponse
	raw, err := planTargetOf(op)
	if err != nil {
		return t, err
	}
	if err := json.Unmarshal(raw, &t); err != nil {
		return t, err
	}
	return t, nil
}

func nonEmptyJSON(raw []byte) json.RawMessage {
	if len(raw) == 0 || !json.Valid(raw) {
		return json.RawMessage("{}")
	}
	return raw
}

func formatOptionalTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

// rejection 是“执行前复核拒绝执行”的结构化原因。
type rejection struct {
	status  int
	code    httpx.Code
	message string
}

func (r *rejection) Error() string { return r.message }

// revalidate 用预检里的规范化参数重算计划，核对目标身份与状态版本。
//
// 这一步是 PREFLIGHT_STALE / DEVICE_IDENTITY_CHANGED 的唯一来源：预检只是“当时”的快照，
// 从预检到执行之间设备可能被换掉、分区表可能被改过，此时执行旧计划就是在改一个
// 计划里没有的对象。重新规划比“相信 120 秒前的快照”贵，但它挡住的是真实的数据损失。
func (s *Server) revalidate(ctx context.Context, action Action, plan agentstore.PreflightPlan, now time.Time) error {
	params, err := action.Decode(plan.Params)
	if err != nil {
		return fmt.Errorf("预检参数无法解码（动作实现与预检不一致）: %w", err)
	}
	fresh, err := action.Plan(ctx, PlanInput{
		Params:       params,
		Actor:        ActorContext{UserID: plan.UserID, SessionIDHash: plan.SessionHash},
		PreflightRef: plan.ID,
		Now:          now,
		ExpiresAt:    plan.ExpiresAt,
	})
	if err != nil {
		return fmt.Errorf("%w: action=%s: %v", ErrPlanFailed, action.Name, err)
	}
	if fresh.Target.IdentityFingerprint != plan.IdentityFingerprint {
		return &rejection{
			status:  http.StatusConflict,
			code:    httpx.CodeDeviceIdentityChanged,
			message: "目标身份已变化（" + fresh.Target.DisplayName + "），请重新预检",
		}
	}
	if fresh.Target.StateVersion != plan.StateVersion {
		return &rejection{
			status:  http.StatusConflict,
			code:    httpx.CodePreflightStale,
			message: "目标状态在预检之后发生变化，请重新预检",
		}
	}
	if !fresh.Allowed {
		codes := make([]string, 0, len(fresh.Blockers))
		for _, b := range fresh.Blockers {
			codes = append(codes, b.Code)
		}
		return &rejection{
			status:  http.StatusConflict,
			code:    httpx.CodeValidationError,
			message: "当前状态不允许执行：" + strings.Join(codes, ","),
		}
	}
	return nil
}

// blockerCodes 把阻塞项压成 "code1,code2" 供错误信息使用。
func blockerCodes(raw []byte) string {
	var notices []Notice
	if err := json.Unmarshal(raw, &notices); err != nil || len(notices) == 0 {
		return "未知原因"
	}
	codes := make([]string, 0, len(notices))
	for _, n := range notices {
		codes = append(codes, n.Code)
	}
	return strings.Join(codes, ",")
}

// checkReauth 校验再认证窗口。
func checkReauth(reauthAt string, now time.Time) error {
	if strings.TrimSpace(reauthAt) == "" {
		return errors.New("该操作需要 5 分钟内的密码再认证")
	}
	at, err := time.Parse(time.RFC3339, reauthAt)
	if err != nil {
		return errors.New("actor_context.reauth_at 必须是 RFC3339 时间")
	}
	at = at.UTC()
	if at.After(now.Add(time.Minute)) {
		// 未来时间不认：时钟偏差不能变成“永远在窗口内”。
		return errors.New("再认证时间晚于当前时间，拒绝执行")
	}
	if now.Sub(at) > ReauthWindow {
		return errors.New("再认证已超过 5 分钟，请重新输入密码")
	}
	return nil
}

// operationRequestHash 计算“这一次请求”的指纹。
//
// 组成：动作 + 参数哈希 + 预检引用 + 用户 + 会话（与迁移里的列注释一致）。
// 同一个 operation_id 换了任何一项，就是另一个意图——不能被当成重放。
func operationRequestHash(plan agentstore.PreflightPlan, actor ActorContext) string {
	parts := []string{
		plan.Action,
		plan.ParamsHash,
		plan.ID,
		strconv.FormatInt(actor.UserID, 10),
		actor.SessionIDHash,
	}
	return hashHex([]byte(strings.Join(parts, "\n")))
}

// planTargetOf 返回操作记录里的目标 JSON。
func planTargetOf(op agentstore.Operation) ([]byte, error) {
	if len(op.Target) == 0 {
		return []byte("{}"), nil
	}
	return op.Target, nil
}
