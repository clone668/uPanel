package processes

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Killer 把信号发给一个 PID。名字里没有“kill”以外的语义：它就是一个 syscall。
//
// 单独抽成接口是为了让测试能证明“某些情况下根本没有调用它”——
// PID 重用、受保护、已经退出这三种路径必须连一次信号都不发。
type Killer interface {
	Signal(pid int, signal string) error
}

// ErrSignalRejected 表示执行前复核发现目标已经不允许发信号（预检之后状态变了）。
var ErrSignalRejected = errors.New("processes: 目标当前状态不允许发送信号")

// ErrSignalUnsupported 表示当前平台没有实现发送信号。
var ErrSignalUnsupported = errors.New("processes: 当前平台不支持向进程发送信号")

// 阻塞项代码（预检里展示给管理员的固定枚举）。
const (
	// CodeNotFound：这个 PID 上没有进程（已经退出）。
	CodeNotFound = "PROCESS_NOT_FOUND"
	// CodeIdentityMismatch：PID 还在，但 starttime 变了——这是另一个进程。
	CodeIdentityMismatch = "PROCESS_IDENTITY_MISMATCH"
	// CodeProtected：PID 1、内核线程、面板/代理自身、受保护系统服务进程。
	CodeProtected = "PROCESS_PROTECTED"
	// CodeAlreadyExited：进程已经是僵尸或已死，没有可发送的信号。
	CodeAlreadyExited = "PROCESS_ALREADY_EXITED"
)

// 告警项代码。
const (
	// WarningTerminate：SIGTERM 可以被捕获、清理、也可以被忽略。
	WarningTerminate = "process_terminate"
	// WarningForceKill：SIGKILL 无法被捕获或忽略，进程没有清理机会。
	WarningForceKill = "process_force_kill"
)

// 影响类别。
const (
	ImpactTermination = "process_termination"
	ImpactDataLoss    = "process_data_loss"
	ImpactNone        = "none"
)

// Reason 是阻塞项/告警项（与 agent.Notice 结构一致，由 agent 层转换）。
type Reason struct {
	Code    string
	Message string
}

// Impact 是操作影响。
type Impact struct {
	Kind        string
	Description string
}

// Assessment 是一次进程信号操作的预检结论（只读）。
type Assessment struct {
	// Found=false 表示这个 PID 上没有进程。
	Found bool
	// Process 是现场读到的进程视图。
	Process Process
	// DisplayName 是给管理员看的目标名（PID + 用户 + 脱敏命令）。
	DisplayName string
	// Identity 是进程身份指纹（PID + starttime）；不存在时是 AbsentIdentity。
	Identity string
	// StateVersion 是状态版本（与身份同源，刻意不含 CPU/内存/运行状态）。
	StateVersion string
	// Allowed 表示当前状态下是否允许发送这个信号。
	Allowed bool
	// Blockers 阻止执行的原因（Allowed=false 时非空）。
	Blockers []Reason
	// Warnings 不阻止执行但必须告知的后果。
	Warnings []Reason
	// Impacts 这次操作会造成的后果。
	Impacts []Impact
	// Locks 执行时要持有的资源锁（process:<pid>:<starttime>）。
	Locks []string
}

// SignalResult 是一次信号操作的结果。
type SignalResult struct {
	PID            int64
	StartTimeTicks uint64
	Signal         string
	// Delivered 表示信号真的发出去过（ESRCH 表示发给内核时进程已经不在）。
	Delivered bool
	// Gone 表示读回确认进程已经结束（消失，或变成僵尸等待父进程回收）。
	Gone bool
	// StateBefore/StateAfter 是发信号前后的状态。
	StateBefore string
	StateAfter  string
	Before      Process
	Duration    time.Duration
}

// DisplayName 生成目标的展示名。
func displayName(p Process) string {
	return fmt.Sprintf("PID %d（%s：%s）", p.PID, p.Owner, p.Command)
}

// signalType 返回信号在审计与文案里用的中文名。
func signalTitle(signal string) string {
	if signal == SignalKILL {
		return "SIGKILL（强制结束）"
	}
	return "SIGTERM（请求退出）"
}

// Assess 计算一次进程信号操作的预检结论（只读）。
//
// 顺序：参数 → 现场读取 → 身份（PID + starttime）→ 保护 → 是否已退出。
// 身份判定必须先于保护与状态：一个被重用的 PID 不是“受保护的进程”，
// 而是“不是你要找的那个进程”，这两件事的处置完全不同。
func (c *Collector) Assess(ctx context.Context, params SignalParams) (Assessment, error) {
	p := Normalize(params)
	if err := ValidateParams(p); err != nil {
		return Assessment{}, err
	}
	if err := ctx.Err(); err != nil {
		return Assessment{}, err
	}

	assessment := Assessment{
		Locks: []string{SignalLock(p.PID, p.StartTimeTicks)},
	}
	row, found := readRow(c.fs, c.paths, p.PID)
	if !found {
		assessment.Found = false
		assessment.DisplayName = fmt.Sprintf("PID %d（不存在）", p.PID)
		assessment.Identity = AbsentIdentity(p.PID)
		assessment.StateVersion = "absent"
		assessment.Blockers = []Reason{{
			Code:    CodeNotFound,
			Message: fmt.Sprintf("PID %d 上没有进程：它可能已经退出，或者这个进程号已经被系统回收", p.PID),
		}}
		assessment.Impacts = []Impact{{Kind: ImpactNone, Description: "没有目标，不会发送任何信号"}}
		return assessment, nil
	}

	process := c.toProcess(row, nil, c.loadUsers(), &liveSource{collector: c})
	assessment.Found = true
	assessment.Process = process
	assessment.DisplayName = displayName(process)

	if row.startTick != p.StartTimeTicks {
		// PID 重用：这是本次最重要的拒绝路径。目标信息按**现存进程**填，
		// 但身份指纹与请求的不同，因此旧预检在提交时也会被判成“身份已变化”。
		assessment.Identity = Identity(p.PID, row.startTick)
		assessment.StateVersion = StateVersion(p.PID, row.startTick)
		assessment.Blockers = []Reason{{
			Code: CodeIdentityMismatch,
			Message: fmt.Sprintf(
				"PID %d 现在是另一个进程：预检记录的是 starttime=%d，而这个 PID 上现存进程的 starttime=%d。"+
					"进程身份是 PID + starttime，拿旧预检去杀新进程是绝对不允许的",
				p.PID, p.StartTimeTicks, row.startTick),
		}}
		assessment.Impacts = []Impact{{Kind: ImpactNone, Description: "身份不匹配，不会发送任何信号"}}
		return assessment, nil
	}

	assessment.Identity = Identity(p.PID, p.StartTimeTicks)
	assessment.StateVersion = StateVersion(p.PID, p.StartTimeTicks)

	// 保护名单先于“已经退出”：受保护的理由必须是“不能碰”，而不是“它已经死了”。
	if process.Protection.Protected {
		assessment.Blockers = append(assessment.Blockers, Reason{
			Code: CodeProtected,
			Message: "受保护的进程，面板不允许向它发送任何信号：" +
				strings.Join(process.Protection.Reasons, "；"),
		})
		assessment.Impacts = []Impact{{Kind: ImpactNone, Description: "受保护的进程不会被改动"}}
		return assessment, nil
	}

	if exited(row.state) {
		assessment.Blockers = append(assessment.Blockers, Reason{
			Code: CodeAlreadyExited,
			Message: "进程已经结束（状态 " + row.state + "）：僵尸进程要由它的父进程回收，" +
				"发送信号不会让它更快消失",
		})
		assessment.Impacts = []Impact{{Kind: ImpactNone, Description: "进程已经结束，不会再变化"}}
		return assessment, nil
	}

	assessment.Warnings, assessment.Impacts = signalConsequences(p.Signal, process)
	assessment.Allowed = len(assessment.Blockers) == 0
	return assessment, nil
}

// signalConsequences 返回某个信号的告警与影响。
func signalConsequences(signal string, process Process) ([]Reason, []Impact) {
	if signal == SignalKILL {
		return []Reason{{
				Code: WarningForceKill,
				Message: "SIGKILL 无法被捕获或忽略：" + process.Command +
					" 不会执行任何清理逻辑，也不会写回缓冲区",
			}}, []Impact{
				{Kind: ImpactTermination, Description: "进程立即终止；它负责的工作会中断"},
				{Kind: ImpactDataLoss, Description: "未落盘的数据、未提交的事务、未清理的临时文件可能留下不一致状态"},
			}
	}
	return []Reason{{
			Code: WarningTerminate,
			Message: "SIGTERM 只是请求退出：" + process.Command +
				" 可以捕获它先清理再退出，也可以忽略它",
		}}, []Impact{
			{Kind: ImpactTermination, Description: "进程会退出；它正在处理的请求会中断"},
			{Kind: ImpactNone, Description: "如果进程忽略 SIGTERM，需要单独发起强制结束（SIGKILL，需再认证与确认短语）"},
		}
}

// ExecuteSignal 执行一次信号发送并**读回验证**。
//
// 真正发信号之前必须重新读一次 /proc：预检到执行之间可能过了几十秒，PID 可能
// 已经被别的进程重用。starttime 不一致就一个信号都不发——这不是“尽量”，
// 而是 syscall.Kill 之前唯一能拦住误杀的东西。
func (c *Collector) ExecuteSignal(ctx context.Context, params SignalParams, progress func(step string, fields map[string]any)) (SignalResult, error) {
	p := Normalize(params)
	if err := ValidateParams(p); err != nil {
		return SignalResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return SignalResult{}, err
	}

	row, found := readRow(c.fs, c.paths, p.PID)
	if !found {
		return SignalResult{}, fmt.Errorf("%w: PID %d 上已经没有进程（预检之后它退出了）", ErrSignalRejected, p.PID)
	}
	if row.startTick != p.StartTimeTicks {
		return SignalResult{}, fmt.Errorf(
			"%w: PID %d 现在是另一个进程（starttime 从 %d 变成 %d），拒绝向它发送信号",
			ErrSignalRejected, p.PID, p.StartTimeTicks, row.startTick)
	}
	process := c.toProcess(row, nil, c.loadUsers(), &liveSource{collector: c})
	if process.Protection.Protected {
		return SignalResult{}, fmt.Errorf("%w: PID %d 受保护（%s）", ErrSignalRejected, p.PID,
			strings.Join(process.Protection.Reasons, "；"))
	}
	if exited(row.state) {
		return SignalResult{}, fmt.Errorf("%w: PID %d 已经结束（状态 %s）", ErrSignalRejected, p.PID, row.state)
	}

	started := time.Now()
	res := SignalResult{
		PID:            p.PID,
		StartTimeTicks: p.StartTimeTicks,
		Signal:         p.Signal,
		StateBefore:    row.state,
		Before:         process,
	}

	if err := c.killer.Signal(int(p.PID), p.Signal); err != nil {
		res.Duration = time.Since(started)
		if isNoSuchProcess(err) {
			// 检查与发送之间进程自己退出了：意图已经达成，但要说清楚信号没发出去。
			res.Gone = true
			res.StateAfter = StateDead
			if progress != nil {
				progress("already_gone", map[string]any{"pid": p.PID, "signal": p.Signal})
			}
			return res, nil
		}
		return res, fmt.Errorf("processes: 向 PID %d 发送 %s 失败: %w", p.PID, p.Signal, err)
	}
	res.Delivered = true
	if progress != nil {
		progress("signal_"+strings.ToLower(p.Signal), map[string]any{
			"pid":              p.PID,
			"start_time_ticks": p.StartTimeTicks,
			"signal":           p.Signal,
		})
	}

	timeout := c.verifyTerm
	if p.Signal == SignalKILL {
		timeout = c.verifyKill
	}
	deadline := time.Now().Add(timeout)
	lastState := row.state
	for {
		after, ok := readRow(c.fs, c.paths, p.PID)
		if !ok {
			res.Gone = true
			res.StateAfter = "gone"
			res.Duration = time.Since(started)
			if progress != nil {
				progress("verified", map[string]any{
					"pid": p.PID, "signal": p.Signal, "state": "gone",
				})
			}
			return res, nil
		}
		if after.startTick != p.StartTimeTicks {
			// 进程退出后 PID 立刻被别的进程拿走：原来那个进程确实已经结束。
			res.Gone = true
			res.StateAfter = "gone"
			res.Duration = time.Since(started)
			if progress != nil {
				progress("verified", map[string]any{
					"pid": p.PID, "signal": p.Signal, "state": "gone",
					"note": "PID 已被新进程复用",
				})
			}
			return res, nil
		}
		lastState = after.state
		if exited(after.state) {
			res.Gone = true
			res.StateAfter = after.state
			res.Duration = time.Since(started)
			if progress != nil {
				progress("verified", map[string]any{
					"pid": p.PID, "signal": p.Signal, "state": after.state,
				})
			}
			return res, nil
		}
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			res.StateAfter = lastState
			res.Duration = time.Since(started)
			return res, fmt.Errorf("%w: 等待 PID %d 退出时被中断", ErrSignalRejected, p.PID)
		case <-time.After(c.verifyInterval):
		}
	}

	res.StateAfter = lastState
	res.Duration = time.Since(started)
	if progress != nil {
		progress("verify_timeout", map[string]any{
			"pid": p.PID, "signal": p.Signal, "state": lastState,
		})
	}
	if p.Signal == SignalKILL {
		return res, fmt.Errorf(
			"processes: SIGKILL 已发出，但 PID %d 在 %s 内仍然存在（状态 %s）；"+
				"它可能处于不可中断的内核等待（D 状态），请到主机上确认",
			p.PID, timeout, lastState)
	}
	return res, fmt.Errorf(
		"processes: %s 已发出，但 PID %d 在 %s 内仍在运行（状态 %s）；"+
			"面板不会自动升级为 SIGKILL——如果确实需要强制结束，请单独发起一次 KILL 预检（需要再认证与确认短语）",
		signalTitle(p.Signal), p.PID, timeout, lastState)
}
