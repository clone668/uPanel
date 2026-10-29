package agent

// 定时触发路由（P12.4）：POST /internal/v1/schedules/:id/trigger。
//
// 它只有一种调用者：root 所有的 `/usr/local/bin/upanelctl schedule-trigger`，
// 由 uPanel 自己的 systemd timer 拉起（CONTRACTS §15）。判定在 peer.go 里按路径
// 形状做——同一前缀下的读路由仍然只给 Web 账号，所以这里不能用前缀判定。
//
// 这条路由不是"另一条执行路径"，它是同一条：动作从代理库里已批准的计划取，
// 参数由计划自己决定（调用方只能给触发 ID），然后走与交互提交完全相同的
// 预检 → 计划落库 → 接受操作 → 后台执行。三处刻意的区别，各有理由：
//
//	再认证   定时触发没有人在键盘前。取而代之的是"管理员保存计划时的那次显式授权"
//	         （契约原话）；计划里只有 backup.run / certificate.renew / script.run，
//	         格式化与网络变更永远走不到这里。
//	确认短语 同理：短语是给人看的，定时触发时没有人在看。计划能表达的动作里
//	         没有一个要求短语，所以这条路上短语恒为空，而不是"跳过检查"。
//	触发 ID  由代理按（计划, UTC 分钟槽）自己算并核对，调用方给的只是被验证的候选。
//	         同一个槽无论触发几次，operation_id 都是同一个——幂等来自算法，不来自信任。
//
// 触发记录先于执行落库（schedule_triggers 只追加）：某天凌晨到底跑没跑过、
// 判成什么、产生的是哪个 operation，都能事后回答，而不是只剩 journal 里的一段文本。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"upanel/internal/agentstore"
	"upanel/internal/httpx"
	"upanel/internal/schedules"
)

// SchedulerUserID 是定时触发在操作与预检记录里的操作者 ID。
//
// 值本身定义在 internal/schedules（Web 侧对账导入时要用同一个数——两个进程各写一份
// 常量就是两个真相，迟早会有一天对不上）。这里只做别名，让代理侧的代码读起来还是
// “那个调度者”。
const SchedulerUserID int64 = schedules.SystemUserID

// maxTriggerRequestBytes 是这条路由的请求体上限：只有一个触发 ID。
const maxTriggerRequestBytes = 4 << 10

// schedulerActor 是"系统自己"的操作者上下文。
//
// 会话哈希由计划 id 派生：它既满足 64 位小写的形态，也保证一份定时触发的计划
// 只能被这条路由（以及同一个计划的下一次触发）使用——不是任何人的会话。
func schedulerActor(ctx context.Context, scheduleID string) ActorContext {
	return ActorContext{
		UserID:        SchedulerUserID,
		SessionIDHash: hashHex([]byte("scheduler:" + scheduleID)),
	}
}

// scheduledOperationRequestHash 是定时触发那次操作的请求指纹。
//
// 与交互路径的 operationRequestHash 差一处：不含预检引用，改含操作者本人。
// 理由是这个槽的幂等必须由**确定的输入**回答——每次重试都会现生成一份预检
// （120 秒后过期，没有别人要用它），把它的随机引用算进指纹，同一个时间槽的第二次
// 触发就会被判成"两个意图挤在一个标识上"，于是 systemd 的一次重试变成一次 409。
//
// 四样输入对一个时间槽恒定：动作、参数哈希（从代理镜像里现算）、操作者与会话哈希
// （都由计划 id 派生）。计划在这一分钟内被改过时参数哈希会变，于是照常冲突——
// 那正是要报出来的情况。
func scheduledOperationRequestHash(plan agentstore.PreflightPlan, actor ActorContext) string {
	parts := []string{
		plan.Action,
		plan.ParamsHash,
		strconv.FormatInt(actor.UserID, 10),
		actor.SessionIDHash,
	}
	return hashHex([]byte(strings.Join(parts, "\n")))
}

// triggerResponse 是触发结果。
type triggerResponse struct {
	ScheduleID  string `json:"schedule_id"`
	TriggerID   string `json:"trigger_id"`
	OperationID string `json:"operation_id"`
	SlotAt      string `json:"slot_at"`
	Action      string `json:"action"`
	Outcome     string `json:"outcome"`
	// MaxRuntimeSeconds 是这次触发允许跑多久（计划定义里的数）。
	//
	// 为什么不给 root 一条"读计划"的路让他自己去查：root CLI 只被允许碰调度这一小块，
	// 读计划详情是 Web 账号的路由（root 调它 403）。等多久是**这次触发**的属性，
	// 直接随答复给出去，upanectl 不必再读一次计划，也不会把"读不到计划"变成"等无上限"。
	// 它和单元的 TimeoutStartSec 同源（CONTRACTS §15：TimeoutStartSec = 这个数 + 余量）。
	MaxRuntimeSeconds int `json:"max_runtime_seconds"`
	// Duplicate 表示这个时间槽已经触发过；operation_id 是当时那一次。
	Duplicate bool `json:"duplicate"`
}

// handleScheduleTrigger 处理一次定时触发。
func (s *Server) handleScheduleTrigger(w http.ResponseWriter, r *http.Request, scheduleID string) {
	svc := s.opts.Schedules
	if svc == nil {
		httpx.WriteError(w, r, http.StatusNotFound, httpx.CodeNotFound, "代理未启用计划任务")
		return
	}
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
	body, ok := readLimitedBody(w, r, maxTriggerRequestBytes)
	if !ok {
		return
	}
	var req schedules.TriggerParams
	if err := decodeStrictObject(body, &req); err != nil {
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationError, err.Error())
		return
	}
	triggerID := strings.TrimSpace(req.TriggerID)
	if triggerID == "" {
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationError, "trigger_id 不能为空")
		return
	}

	now := s.nowUTC().Truncate(time.Second)

	// 判决：触发 ID 与时间槽对不对得上、计划还启不启用、目标还在不在、要跑什么。
	decision, err := svc.DecideTrigger(r.Context(), scheduleID, triggerID)
	if err != nil {
		// 唯一会走到这里的错误是"触发 ID 与当前时间槽不符"：那是调用方算错了槽，
		// 或者有别的程序在伪造触发。两种情况都不该悄悄受理。
		s.opts.Logger.Warn("拒绝定时触发",
			"schedule_id", scheduleID, "trigger_id", triggerID,
			"error", err, "request_id", httpx.RequestID(r))
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationError, err.Error())
		return
	}
	if decision.Blocked() {
		code := decision.Blockers[0].Code
		reason := decision.Blockers[0].Message
		if _, _, cerr := svc.ClaimTrigger(r.Context(), decision, schedules.TriggerOutcomeBlocked, code+": "+reason); cerr != nil {
			s.opts.Logger.Error("写入触发记录失败", "schedule_id", scheduleID,
				"trigger_id", triggerID, "error", cerr)
		}
		s.opts.Logger.Warn("定时触发被拦下",
			"schedule_id", scheduleID, "trigger_id", triggerID, "code", code,
			"reason", reason, "request_id", httpx.RequestID(r))
		httpx.WriteError(w, r, httpx.StatusForCode(httpx.CodeValidationError), httpx.CodeValidationError,
			reason)
		return
	}

	plan, planErr := s.buildTriggerPlan(r.Context(), decision, now)
	if planErr != nil {
		var rej *rejection
		if errors.As(planErr, &rej) {
			if _, _, cerr := svc.ClaimTrigger(r.Context(), decision, schedules.TriggerOutcomeBlocked, string(rej.code)+": "+rej.message); cerr != nil {
				s.opts.Logger.Error("写入触发记录失败", "schedule_id", scheduleID,
					"trigger_id", triggerID, "error", cerr)
			}
			httpx.WriteError(w, r, rej.status, rej.code, rej.message)
			return
		}
		if _, _, cerr := svc.ClaimTrigger(r.Context(), decision, schedules.TriggerOutcomeFailed, planErr.Error()); cerr != nil {
			s.opts.Logger.Error("写入触发记录失败", "schedule_id", scheduleID, "error", cerr)
		}
		s.internalError(w, r, "定时触发的预检失败", planErr)
		return
	}

	// 操作者在这里确定一次：操作记录与请求指纹必须用同一份身份，
	// 否则"同一个槽、同一份参数"会在重试时算出一个不同的指纹。
	actor := schedulerActor(r.Context(), scheduleID)
	record := agentstore.Operation{
		ID:                  decision.OperationID,
		RequestHash:         scheduledOperationRequestHash(plan.record, actor),
		Action:              plan.record.Action,
		PreflightRef:        plan.record.ID,
		Params:              plan.record.Params,
		ParamsHash:          plan.record.ParamsHash,
		IdentityFingerprint: plan.record.IdentityFingerprint,
		StateVersion:        plan.record.StateVersion,
		Target:              plan.record.Target,
		Locks:               plan.record.Locks,
		UserID:              actor.UserID,
		SessionHash:         actor.SessionIDHash,
		RequestID:           httpx.RequestID(r),
		Stage:               agentstore.StageAccepted,
		CreatedAt:           now,
		UpdatedAt:           now,
	}
	accepted, duplicate, err := s.opts.Operations.Accept(r.Context(), record)
	if err != nil {
		// 计划落库了但操作没被接受：把这次触发记成 failed，而不是让它看起来"跑过了"。
		if _, _, cerr := svc.ClaimTrigger(context.WithoutCancel(r.Context()), decision,
			schedules.TriggerOutcomeFailed, "接受操作失败: "+err.Error()); cerr != nil {
			s.opts.Logger.Error("写入触发记录失败", "schedule_id", scheduleID, "error", cerr)
		}
		if errors.Is(err, agentstore.ErrOperationConflict) {
			httpx.WriteError(w, r, http.StatusConflict, httpx.CodeIdempotencyConflict,
				"这个时间槽已经产生过不同的操作")
			return
		}
		s.internalError(w, r, "接受定时触发的操作失败", err)
		return
	}

	// 重放：同一个 operation_id 已经存在（接受是幂等的），说明这个槽跑过了。
	// 不再起执行，把第一次的 operation_id 还回去——"当时跑的是哪一个"必须可查。
	if duplicate {
		s.opts.Logger.Info("定时触发是重放，返回已有操作",
			"schedule_id", scheduleID, "trigger_id", triggerID,
			"operation_id", accepted.ID, "stage", accepted.Stage,
			"request_id", httpx.RequestID(r))
		httpx.WriteData(w, r, http.StatusOK, triggerResponse{
			ScheduleID: scheduleID, TriggerID: triggerID, OperationID: accepted.ID,
			SlotAt: decision.Slot.UTC().Format(time.RFC3339), Action: record.Action,
			Outcome:           schedules.TriggerOutcomeDuplicate,
			MaxRuntimeSeconds: decision.Schedule.Definition.MaxRuntimeSeconds,
			Duplicate:         true,
		})
		return
	}

	// 认领时间槽（只追加的巡检记录）。它不作为是否执行的闸门：真正的幂等是
	// 上面那个确定性的 operation_id。这里返回 duplicate 只说明"上一次也认领过
	// 这个槽"——那种情况下库里没有对应的操作，说明上次在认领之后、落库之前断了，
	// 而这一次已经把操作建出来了，照常执行并把本次触发记下来。
	if _, claimedBefore, cerr := svc.ClaimTrigger(r.Context(), decision, schedules.TriggerOutcomeTriggered, ""); cerr != nil {
		s.opts.Logger.Error("写入触发记录失败", "schedule_id", scheduleID, "trigger_id", triggerID, "error", cerr)
	} else if claimedBefore {
		s.opts.Logger.Warn("这个时间槽此前已被认领，但没有对应的操作记录",
			"schedule_id", scheduleID, "trigger_id", triggerID, "operation_id", accepted.ID)
	}

	s.opts.Logger.Info("定时触发已受理",
		"schedule_id", scheduleID, "trigger_id", triggerID,
		"operation_id", accepted.ID, "action", record.Action,
		"slot_at", decision.Slot.UTC().Format(time.RFC3339),
		"locks", len(decodeLocks(record.Locks)), "request_id", httpx.RequestID(r))

	s.runner.start(accepted)
	httpx.WriteData(w, r, http.StatusCreated, triggerResponse{
		ScheduleID: scheduleID, TriggerID: triggerID, OperationID: accepted.ID,
		SlotAt: decision.Slot.UTC().Format(time.RFC3339), Action: record.Action,
		Outcome:           schedules.TriggerOutcomeTriggered,
		MaxRuntimeSeconds: decision.Schedule.Definition.MaxRuntimeSeconds,
	})
}

// handleScheduleTriggerRun 处理 GET /internal/v1/schedules/:id/trigger-runs/:op。
//
// 这是 `upanectl schedule-trigger --wait` 唯一的状态来源（CONTRACTS §15）。
// 它读的是与 Web 侧同一种操作视图，但**只在一种情况下**读得到：那条操作就是这条
// 计划自己触发的那一次。
//
// 判据是两件都由计划 id 派生的东西——操作者 ID（系统调度者）与会话哈希
// （hashHex("scheduler:"+计划 id)），也就是 schedulerActor 给出的那一份。核对不上就
// 当作操作不存在（404），不给"它存在但你不能看"这种可枚举的差别，也不把这条路由变成
// "root 能读任意操作"——那样一来，一个按计划 id 命名的读路由就变成了越权的入口。
func (s *Server) handleScheduleTriggerRun(w http.ResponseWriter, r *http.Request, scheduleID, operationID string) {
	svc := s.opts.Schedules
	if svc == nil {
		httpx.WriteError(w, r, http.StatusNotFound, httpx.CodeNotFound, "代理未启用计划任务")
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		httpx.WriteError(w, r, http.StatusMethodNotAllowed, httpx.CodeValidationError, "只支持 GET")
		return
	}
	if !agentstore.ValidOperationID(operationID) {
		httpx.WriteError(w, r, http.StatusNotFound, httpx.CodeNotFound, "操作不存在")
		return
	}
	op, err := s.opts.Operations.Get(r.Context(), operationID)
	if err != nil {
		s.writeOperationLookupError(w, r, operationID, err)
		return
	}
	actor := schedulerActor(r.Context(), scheduleID)
	if op.UserID != actor.UserID || op.SessionHash != actor.SessionIDHash {
		s.opts.Logger.Warn("拒绝读取不属于这条计划的操作",
			"schedule_id", scheduleID, "operation_id", operationID,
			"operation_user_id", op.UserID, "request_id", httpx.RequestID(r))
		httpx.WriteError(w, r, http.StatusNotFound, httpx.CodeNotFound, "操作不存在")
		return
	}
	s.writeOperation(w, r, op, false, http.StatusOK)
}

// triggerPlan 是一次定时触发的预检产物。
type triggerPlan struct {
	record agentstore.PreflightPlan
}

// buildTriggerPlan 为定时触发生成并落库一份预检计划。
//
// 它做的是交互路径上 handlePreflights 做的同一件事，只是操作者换成了"系统自己"：
// 解码计划指定的动作参数 → 跑该动作的 Plan → 落库。任何拒绝都用 rejection 表达，
// 于是调用点只有一条处理分支。
func (s *Server) buildTriggerPlan(ctx context.Context, decision schedules.TriggerDecision, now time.Time) (triggerPlan, error) {
	action, found := s.registry.Action(decision.Action.Action)
	if !found {
		return triggerPlan{}, &rejection{status: http.StatusConflict, code: httpx.CodeValidationError,
			message: "计划指向的动作当前不可用：" + decision.Action.Action}
	}
	if action.Plan == nil || action.Execute == nil {
		return triggerPlan{}, &rejection{status: http.StatusNotImplemented, code: httpx.CodeNotImplemented,
			message: "计划指向的动作尚未实现：" + decision.Action.Action}
	}
	params, err := action.Decode(decision.Action.Params)
	if err != nil {
		return triggerPlan{}, &rejection{status: http.StatusBadRequest, code: httpx.CodeValidationError,
			message: "计划指向的动作参数不合法：" + err.Error()}
	}
	policy, err := action.ResolvePolicy(params)
	if err != nil {
		return triggerPlan{}, err
	}
	if policy.RequiresPhrase {
		// 定时触发没有人在键盘前，短语在这里没有意义。计划能表达的动作里没有一个
		// 要求短语；真要求了，说明计划指向了一个不该被定时的动作——拒绝，而不是跳过。
		return triggerPlan{}, &rejection{status: http.StatusConflict, code: httpx.CodeValidationError,
			message: "计划指向的动作需要交互确认，不能定时触发"}
	}
	ref, err := newPlanRef()
	if err != nil {
		return triggerPlan{}, err
	}
	actor := schedulerActor(ctx, decision.Schedule.ID)
	planCtx, cancel := planBudgetContext(ctx, action)
	defer cancel()
	plan, err := action.Plan(planCtx, PlanInput{
		Params:       params,
		Actor:        actor,
		PreflightRef: ref,
		Now:          now,
		ExpiresAt:    now.Add(PreflightTTL),
		OperationID:  decision.OperationID,
	})
	if err != nil {
		return triggerPlan{}, err
	}
	if verr := validatePlanResult(plan); verr != nil {
		return triggerPlan{}, verr
	}
	if !plan.Allowed {
		return triggerPlan{}, &rejection{status: http.StatusConflict, code: httpx.CodeValidationError,
			message: "计划此刻不能执行：" + blockerCodesOf(plan.Blockers)}
	}

	normalized, err := json.Marshal(plan.NormalizedParams)
	if err != nil {
		return triggerPlan{}, err
	}
	normalized, err = canonicalJSON(normalized)
	if err != nil {
		return triggerPlan{}, err
	}
	targetBytes, err := json.Marshal(preflightTargetResponse{
		DisplayName:         plan.Target.DisplayName,
		IdentityFingerprint: plan.Target.IdentityFingerprint,
		StateVersion:        plan.Target.StateVersion,
	})
	if err != nil {
		return triggerPlan{}, err
	}
	blockers, err := marshalList(plan.Blockers)
	if err != nil {
		return triggerPlan{}, err
	}
	warnings, err := marshalList(plan.Warnings)
	if err != nil {
		return triggerPlan{}, err
	}
	impacts, err := marshalList(plan.Impacts)
	if err != nil {
		return triggerPlan{}, err
	}
	locks := normalizeLocks(plan.Locks)
	locksBytes, err := json.Marshal(locks)
	if err != nil {
		return triggerPlan{}, err
	}

	record := agentstore.PreflightPlan{
		ID:                  ref,
		Action:              action.Name,
		Params:              normalized,
		ParamsHash:          hashHex(normalized),
		Target:              targetBytes,
		IdentityFingerprint: plan.Target.IdentityFingerprint,
		StateVersion:        plan.Target.StateVersion,
		Allowed:             true,
		Blockers:            blockers,
		Warnings:            warnings,
		Impacts:             impacts,
		Locks:               locksBytes,
		// 没有交互再认证，也就没有短语：见文件头。
		RequiresReauth: false,
		PhraseHash:     "",
		UserID:         SchedulerUserID,
		SessionHash:    actor.SessionIDHash,
		CreatedAt:      now,
		ExpiresAt:      now.Add(PreflightTTL),
	}
	if err := s.opts.Plans.Insert(ctx, record); err != nil {
		return triggerPlan{}, err
	}
	return triggerPlan{record: record}, nil
}

// blockerCodesOf 把阻塞项拼成一句可读的话。
func blockerCodesOf(notices []Notice) string {
	if len(notices) == 0 {
		return "（没有给出原因）"
	}
	parts := make([]string, 0, len(notices))
	for _, n := range notices {
		if n.Code == "" {
			parts = append(parts, n.Message)
			continue
		}
		parts = append(parts, n.Code+"："+n.Message)
	}
	return strings.Join(parts, "；")
}
