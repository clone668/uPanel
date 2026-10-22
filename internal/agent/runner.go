package agent

// 操作执行器（P03.2）：接受之后在后台跑，所有状态变化都落在代理库里。
//
// 关键约束（CONTRACTS.md 4.2、4.3、14.2）：
//   - 执行前按预检重算计划 → 重新核对目标身份与状态版本（接受时已做过一次，
//     这里再做一次：排队等锁的时间也能让设备被换掉）。
//   - 多个资源按规范字符串顺序获取，30 秒拿不到就 RESOURCE_BUSY，不无限占 worker。
//   - 不可取消的动作拿不到取消信号；可取消的动作在可中断点返回 ErrCanceled，
//     结果记 canceled —— 而不是假装 succeeded。
//   - 进程重启把遗留的非终态操作标成 interrupted，绝不自动重跑。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"upanel/internal/agentstore"
	"upanel/internal/httpx"
)

// runner 管理正在执行的操作。
type runner struct {
	srv     *Server
	locks   *lockManager
	mu      sync.Mutex
	cancels map[string]context.CancelFunc
	wg      sync.WaitGroup
}

func newRunner(s *Server) *runner {
	return &runner{srv: s, locks: newLockManager(), cancels: make(map[string]context.CancelFunc)}
}

// start 开始执行一个已接受的操作。
//
// 用 context.Background 而不是请求的 ctx：HTTP 请求返回后它就结束了，
// 但操作必须继续跑——《浏览器关闭任务继续》这条验收要求就靠这里。
func (r *runner) start(op agentstore.Operation) {
	ctx, cancel := context.WithTimeout(context.Background(), OperationTimeout)
	r.mu.Lock()
	r.cancels[op.ID] = cancel
	r.mu.Unlock()

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer func() {
			r.mu.Lock()
			delete(r.cancels, op.ID)
			r.mu.Unlock()
			cancel()
		}()
		r.execute(ctx, op)
	}()
}

// cancel 把取消信号递给正在执行的操作。已经结束或不可取消时什么也不做。
func (r *runner) cancel(operationID string) {
	r.mu.Lock()
	cancel, ok := r.cancels[operationID]
	r.mu.Unlock()
	if ok {
		cancel()
	}
}

// stopAll 取消所有在跑的操作并等待它们收尾（优雅停机用）。
func (r *runner) stopAll() {
	r.mu.Lock()
	for _, cancel := range r.cancels {
		cancel()
	}
	r.mu.Unlock()
	r.wg.Wait()
}

// running 返回正在执行的操作数量（测试与诊断用）。
func (r *runner) running() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.cancels)
}

// execute 是执行一个操作的全过程。
func (r *runner) execute(ctx context.Context, op agentstore.Operation) {
	s := r.srv
	action, found := s.registry.Action(op.Action)
	if !found || action.Execute == nil {
		s.finishFailed(ctx, op.ID, "unavailable", httpxCodeNotImplemented,
			"动作当前不可执行", "登记表里没有这个动作的执行器")
		return
	}

	if op.CancellationRequested {
		// 排队期间已经被要求取消：不再开始。
		s.finishStage(ctx, op.ID, agentstore.StageCanceled, "canceled", nil, nil)
		return
	}

	// 锁：按预检里已排序的字符串顺序获取。
	locks := decodeLocks(op.Locks)
	release, err := r.locks.acquire(ctx, op.ID, locks, LockQueueTimeout)
	if err != nil {
		if errors.Is(err, ErrResourceBusy) {
			s.finishFailed(ctx, op.ID, "lock_timeout", httpxCodeResourceBusy,
				"资源正被其他操作占用，排队超时", "稍后重新提交")
			return
		}
		// ctx 结束：取消或停机。
		s.finishCanceledOrInterrupted(ctx, op.ID, "queued_canceled")
		return
	}
	defer release()

	params, err := action.Decode(op.Params)
	if err != nil {
		s.finishFailed(ctx, op.ID, "decode", httpxCodeInternalError,
			"预检参数无法解码", "重新预检后再提交")
		return
	}

	// 执行前的第二次复核：排队这段时间里目标可能变了。
	plan := agentstore.PreflightPlan{
		ID:                  op.PreflightRef,
		Action:              op.Action,
		Params:              op.Params,
		ParamsHash:          op.ParamsHash,
		IdentityFingerprint: op.IdentityFingerprint,
		StateVersion:        op.StateVersion,
		UserID:              op.UserID,
		SessionHash:         op.SessionHash,
	}
	now := s.nowUTC().Truncate(time.Second)
	if err := s.revalidate(ctx, action, plan, now); err != nil {
		var rej *rejection
		if errors.As(err, &rej) {
			s.finishFailed(ctx, op.ID, "revalidate", string(rej.code), rej.message, "重新预检后再提交")
			return
		}
		s.finishFailed(ctx, op.ID, "revalidate", httpxCodeInternalError,
			"执行前复核失败", "重新预检后再提交")
		return
	}

	if err := s.opts.Operations.MarkRunning(ctx, op.ID, "executing", now); err != nil {
		if errors.Is(err, agentstore.ErrOperationFinished) {
			// 在等锁期间被取消了：状态已经终态，不再执行。
			return
		}
		s.opts.Logger.Error("推进操作状态失败", "operation_id", op.ID, "error", err)
		return
	}

	cancelCh := make(chan struct{})
	if action.Policy.Cancellable {
		stop := make(chan struct{})
		defer close(stop)
		go func() {
			select {
			case <-ctx.Done():
				close(cancelCh)
			case <-stop:
			}
		}()
	}

	in := ExecuteInput{
		Params:       params,
		Actor:        ActorContext{UserID: op.UserID, SessionIDHash: op.SessionHash},
		OperationID:  op.ID,
		PreflightRef: op.PreflightRef,
		Target: Target{
			// 目标三元组里执行前复核真正要用的两项：还是那块盘、还是那个状态。
			// DisplayName 只是人读的名字，执行器不靠它判断，因此不在这里拼。
			IdentityFingerprint: op.IdentityFingerprint,
			StateVersion:        op.StateVersion,
		},
		Now: now,
		Progress: func(step string, fields map[string]any) {
			payload := map[string]any{"step": step}
			for k, v := range fields {
				payload[k] = v
			}
			if err := s.opts.Operations.AppendEvent(context.WithoutCancel(ctx), op.ID, "progress", payload, s.nowUTC()); err != nil {
				s.opts.Logger.Warn("写入进度事件失败", "operation_id", op.ID, "step", step, "error", err)
			}
		},
		Cancel: cancelCh,
	}

	result, err := action.Execute(ctx, in)
	if err != nil {
		if errors.Is(err, ErrCanceled) || errors.Is(err, context.Canceled) {
			s.finishCanceledOrInterrupted(ctx, op.ID, "canceled")
			return
		}
		if errors.Is(err, context.DeadlineExceeded) {
			s.finishFailed(ctx, op.ID, "timeout", httpxCodeCommandFailed,
				"执行超过时限", "先确认系统实际状态，再决定是否重新提交")
			return
		}
		// 动作自己的拒绝优先：file.* 与其它知道判决的动作会用 files.HTTPStatusOf
		// 包一层 statusCarrier。把它压成一句“动作执行失败”会让管理员看不到
		// VERSION_CONFLICT / PATH_OUTSIDE_ROOT 这类能自己处理的原因。
		if carrier, ok := statusCarrierOf(err); ok {
			_, code := carrier.HTTPStatus()
			s.finishFailed(ctx, op.ID, "failed", string(code), err.Error(),
				recoveryHintForRejection(code))
			s.opts.Logger.Warn("动作被拒绝",
				"operation_id", op.ID, "action", op.Action, "code", string(code), "error", err)
			return
		}
		s.finishFailed(ctx, op.ID, "failed", httpxCodeCommandFailed,
			"动作执行失败", "先确认系统实际状态，再决定是否重新提交")
		s.opts.Logger.Error("动作执行失败",
			"operation_id", op.ID, "action", op.Action,
			"error", fmt.Errorf("%w: %v", ErrExecuteFailed, err))
		return
	}
	if verr := validateResult(result); verr != nil {
		s.finishFailed(ctx, op.ID, "result", httpxCodeInternalError,
			"执行结果不合法", "查看代理日志")
		s.opts.Logger.Error("执行结果不合法", "operation_id", op.ID, "error", verr)
		return
	}

	body, merr := marshalResult(result)
	if merr != nil {
		s.finishFailed(ctx, op.ID, "result", httpxCodeInternalError, "执行结果无法序列化", "查看代理日志")
		return
	}
	if err := s.opts.Operations.Finish(ctx, op.ID, agentstore.StageSucceeded, "done", body, nil, s.nowUTC()); err != nil {
		s.opts.Logger.Error("写入成功终态失败", "operation_id", op.ID, "error", err)
		return
	}
	s.opts.Logger.Info("操作完成",
		"operation_id", op.ID, "action", op.Action, "outcome", result.Outcome)
}

// marshalResult 把执行结果序列化成 result_json。
func marshalResult(res Result) ([]byte, error) {
	observed := res.ObservedState
	if observed == nil {
		observed = map[string]any{}
	}
	data := res.Data
	if data == nil {
		data = map[string]any{}
	}
	payload := map[string]any{
		"outcome":        res.Outcome,
		"observed_state": observed,
		"data":           data,
	}
	if !res.VerifiedAt.IsZero() {
		payload["verified_at"] = res.VerifiedAt.UTC().Format(time.RFC3339)
	} else {
		payload["verified_at"] = nil
	}
	if res.RecoveryHint != "" {
		payload["recovery_hint"] = res.RecoveryHint
	} else {
		payload["recovery_hint"] = nil
	}
	return json.Marshal(payload)
}

// finishFailed 记录一次失败终态。
func (s *Server) finishFailed(ctx context.Context, id, step, code, message, hint string) {
	errBody, _ := json.Marshal(map[string]any{
		"code":          code,
		"message":       message,
		"recovery_hint": hint,
	})
	// 用 WithoutCancel：ctx 可能已经因为停机/超时结束，但失败记录必须落库。
	if err := s.opts.Operations.Finish(context.WithoutCancel(ctx), id,
		agentstore.StageFailed, step, nil, errBody, s.nowUTC()); err != nil {
		s.opts.Logger.Error("写入失败终态失败", "operation_id", id, "error", err)
	}
}

// finishStage 记录一个不带错误的终态（取消）。
func (s *Server) finishStage(ctx context.Context, id, stage, step string, result []byte, errObj map[string]string) {
	var errJSON []byte
	if len(errObj) > 0 {
		errJSON, _ = json.Marshal(errObj)
	}
	if err := s.opts.Operations.Finish(context.WithoutCancel(ctx), id, stage, step, result, errJSON, s.nowUTC()); err != nil {
		if !errors.Is(err, agentstore.ErrOperationFinished) {
			s.opts.Logger.Error("写入终态失败", "operation_id", id, "stage", stage, "error", err)
		}
	}
}

// finishCanceledOrInterrupted 记录“被中断”的收尾。
//
// 区分两种来源：收到了取消请求（canceled，用户意图达成）与进程/上下文结束
// （interrupted，实际状态未知，需要人工确认）。把后者记成 canceled 会让人以为
// “什么都没做”，而它可能正好停在写磁盘的中间。
func (s *Server) finishCanceledOrInterrupted(ctx context.Context, id, step string) {
	op, err := s.opts.Operations.Get(context.WithoutCancel(ctx), id)
	if err != nil {
		s.opts.Logger.Error("读取操作失败，无法记录中断", "operation_id", id, "error", err)
		return
	}
	if op.CancellationRequested || op.Stage == agentstore.StageAccepted {
		s.finishStage(ctx, id, agentstore.StageCanceled, step, nil, nil)
		return
	}
	errBody, _ := json.Marshal(map[string]any{
		"code":          httpxCodeOperationInterrupted,
		"message":       "执行被中断，实际状态未知",
		"recovery_hint": "先查看系统实际状态，不要直接重新提交",
	})
	if err := s.opts.Operations.Finish(context.WithoutCancel(ctx), id,
		agentstore.StageInterrupted, step, nil, errBody, s.nowUTC()); err != nil {
		if !errors.Is(err, agentstore.ErrOperationFinished) {
			s.opts.Logger.Error("写入中断终态失败", "operation_id", id, "error", err)
		}
	}
}

// decodeLocks 把 locks_json 还原成锁名列表。
func decodeLocks(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	var locks []string
	if err := json.Unmarshal(raw, &locks); err != nil {
		return nil
	}
	return normalizeLocks(locks)
}

// reconcileInterrupted 在启动时把上次运行遗留的非终态操作标成 interrupted。
//
// **不重跑任何东西**（CONTRACTS.md 4.3：代理异常退出后未决任务标为 interrupted；
// 先查询实际状态，不自动重新执行）。这条规则对不可取消的破坏性动作尤其重要：
// 自动重试会把“可能已经格式化”变成“再格式化一次”。
func (s *Server) reconcileInterrupted(ctx context.Context) {
	ops, err := s.opts.Operations.ListUnfinished(ctx)
	if err != nil {
		s.opts.Logger.Error("启动时读取非终态操作失败（不自动重试任何操作）", "error", err)
		return
	}
	for _, op := range ops {
		stage := agentstore.StageInterrupted
		step := "restart"
		if op.CancellationRequested {
			stage = agentstore.StageCanceled
		}
		errBody, _ := json.Marshal(map[string]any{
			"code":          httpxCodeOperationInterrupted,
			"message":       "代理重启时该操作尚未结束，实际状态未知",
			"recovery_hint": "先查看系统实际状态，不要直接重新提交",
		})
		if err := s.opts.Operations.Finish(ctx, op.ID, stage, step, nil, errBody, s.nowUTC()); err != nil {
			s.opts.Logger.Error("标记中断操作失败", "operation_id", op.ID, "error", err)
			continue
		}
		s.opts.Logger.Warn("启动时发现未结束的操作，已标记为中断（未重试）",
			"operation_id", op.ID, "action", op.Action, "stage_before", op.Stage, "stage_now", stage)
	}
	if len(ops) > 0 {
		s.opts.Logger.Warn("启动时对账完成", "unfinished", len(ops))
	}
}

// 内部协议里用到的 httpx 码在这里集中引用，避免字符串散落在执行路径上。
const (
	httpxCodeNotImplemented       = "NOT_IMPLEMENTED"
	httpxCodeResourceBusy         = "RESOURCE_BUSY"
	httpxCodeCommandFailed        = "COMMAND_FAILED"
	httpxCodeInternalError        = "INTERNAL_ERROR"
	httpxCodeOperationInterrupted = "OPERATION_INTERRUPTED"
)

// recoveryHintForRejection 给业务拒绝配一句可执行的下一步提示。
//
// 这些码都表示“这次没做成，但你可以自己处理”，所以提示必须具体：
// 一句“请重试”对版本冲突毫无帮助。
func recoveryHintForRejection(code httpx.Code) string {
	switch code {
	case httpx.CodeVersionConflict:
		return "目标已被修改或已存在不同内容；重新读取当前状态后再发起预检"
	case httpx.CodePathOutsideRoot:
		return "这个路径在面板的边界之外（符号链接、硬链接或越级）；换一个路径或调整文件根配置"
	case httpx.CodeForbidden:
		return "文件根登记为只读；在代理配置里把它改成 writable 才允许写入"
	case httpx.CodeRootUnavailable:
		return "文件根此刻不可用（挂载未就位或身份变化）；确认挂载后重新载入代理配置"
	case httpx.CodeNotFound:
		return "目标不在了；重新加载目录后再操作"
	case httpx.CodeFileNotText:
		return "这个文件不是 UTF-8 文本；改用下载/上传的方式替换它"
	case httpx.CodeValidationError:
		return "参数或体积不被接受；检查上限（单个文件、目录树条目数）后重试"
	case httpx.CodeNotImplemented:
		return "这台机器或这个版本不支持该操作；不要重试，先确认代理版本与平台"
	default:
		return "先确认系统实际状态，再决定是否重新提交"
	}
}
