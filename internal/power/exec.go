package power

// 预检与执行。
//
// 预检**不写系统**（CONTRACTS.md 4.1）：它只读 boot_id、uptime、systemd 的存在性，
// 再问一次“有没有别的写操作在跑”。执行才动手，而且分两段：先倒计时（可取消），
// 再下发（不可收回）。

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// CommandResult 是一条外部命令的输出。字段与 syscmd.Result 一一对应，
// 由代理那边翻译；测试可以直接造。
type CommandResult struct {
	Program  string
	ExitCode int
	Signal   string
	Stdout   string
	Stderr   string
	TimedOut bool
}

// issueTimeout 是下发 systemctl 的上限。
//
// 它不是“重启要多久”，而是“systemd 多久没回应就算失败”。systemctl 把作业交给 PID 1
// 就返回（通常几十毫秒）；15 秒还没回来，说明总线或 PID 1 出了问题，那时候说
// “下发失败，请检查 systemd”比继续等诚实。
const issueTimeout = 15 * time.Second

// Assess 计算预检结论。
//
// req.ExcludeOperationID 非空表示这是执行前复核（见 AssessRequest 的注释）。
func (e *Executor) Assess(_ context.Context, req AssessRequest) (Assessment, error) {
	params := req.Params
	if err := params.Validate(); err != nil {
		return Assessment{}, err
	}
	facts := e.Facts()
	a := Assessment{
		Op:        params.Operation,
		Params:    params,
		Facts:     facts,
		Locks:     []string{LockName},
		CheckedAt: e.now(),
	}
	a.Target = Target{
		DisplayName:         "这台服务器（boot_id " + shortID(facts.BootID) + "）",
		IdentityFingerprint: "boot:" + facts.BootID,
		// 状态版本就是 boot_id：预检之后机器要是重启过（哪怕不是我们干的），
		// 这次预检说的“当前这次启动”已经不存在了，提交时必须重新预检。
		StateVersion: facts.BootID,
	}

	// 一、没有 boot_id 就没有“重启确认”。做可以，但做完说不清有没有发生——
	// 那正是 PLAN §16 禁止的姿态，所以直接拦。
	if facts.BootIDError != "" {
		a.Blockers = append(a.Blockers, Reason{
			Code:    CodeBootIDUnreadable,
			Message: "读不到 boot_id，这次操作结束后无法确认重启是否发生：" + facts.BootIDError,
		})
	}

	// 二、systemd 不在就没有合法的下发路径。不用 shutdown 顶替：shutdown 自己也是
	// 通过 systemd 干活的，绕过它去 kill 1 号进程不是面板该做的事。
	if !facts.Systemd || !facts.SystemctlPresent {
		a.Blockers = append(a.Blockers, Reason{
			Code: CodeUnsupported,
			Message: fmt.Sprintf("这台机器没有可用的 systemd（%s 或 %s 不存在），"+
				"面板不提供替代路径", e.paths.SystemdRun, e.paths.Systemctl),
		})
	}

	// 三、“检查关键任务”。查不到来源就是查不到，不是“没有”。
	if e.inFlight == nil {
		a.Blockers = append(a.Blockers, Reason{
			Code:    CodeJobsUnknown,
			Message: "查不到正在执行的任务，无法确认此刻重启是否会打断写操作",
		})
	} else {
		inFlight, err := e.inFlight.InFlight(req.ExcludeOperationID)
		if err != nil {
			a.Blockers = append(a.Blockers, Reason{
				Code:    CodeJobsUnknown,
				Message: "查不到正在执行的任务，无法确认此刻重启是否会打断写操作：" + err.Error(),
			})
		} else {
			a.InFlight = inFlight
			if len(inFlight) > 0 {
				a.Blockers = append(a.Blockers, Reason{
					Code:    CodeJobsRunning,
					Message: "有写操作正在执行，现在" + params.Operation.DisplayName() + "会把它断在半路：" + describeInFlight(inFlight),
				})
			}
		}
	}

	// 告警：与阻塞分开写。它们不改变“这次能不能做”，但都是管理员该知道的事。
	if params.Operation == OpPoweroff {
		a.Warnings = append(a.Warnings, Reason{
			Code:    "poweroff_needs_manual_start",
			Message: "关机之后这台机器不会自己回来：面板也连不上，界面上只能如实显示“已下发，连接已断开”",
		})
	}
	if params.Operation == OpReboot && e.stat("/run/reboot-required") {
		a.Warnings = append(a.Warnings, Reason{
			Code:    "reboot_pending_elsewhere",
			Message: "系统本来就有一个待生效的重启标志（/run/reboot-required）；这次重启会一并把它清掉",
		})
	}
	if facts.UptimeError == "" && facts.Uptime < 10*time.Minute {
		a.Warnings = append(a.Warnings, Reason{
			Code:    "recently_booted",
			Message: fmt.Sprintf("这台机器 %.0f 分钟前才启动，确认不是正要重启第二次", facts.Uptime.Minutes()),
		})
	}

	a.Impacts = []Impact{{
		Kind: ImpactPowerState,
		Description: fmt.Sprintf("%d 秒后执行 %s（在这之前可以在任务页取消）",
			params.DelaySeconds, params.Operation.DisplayName()),
	}, {
		Kind: ImpactConnection,
		Description: "执行的一瞬间面板与所有会话会断开；重启后需要重新登录，" +
			"关机后要人工开机",
	}}
	if params.Operation == OpReboot {
		a.Impacts = append(a.Impacts, Impact{
			Kind:        "reboot_confirmation",
			Description: "重启回来之后，面板会用 boot_id 的变化确认它真的重启了（同一个 boot_id 说明没有）",
		})
	}

	a.Allowed = len(a.Blockers) == 0
	sort.Slice(a.InFlight, func(i, j int) bool { return a.InFlight[i].OperationID < a.InFlight[j].OperationID })
	return a, nil
}

// LockName 是这个动作持有的资源锁（CONTRACTS.md 第 5 节）。
const LockName = "power-global"

// Execution 是一次执行的结果。
type Execution struct {
	// Outcome 永远是 "accepted"：详见包注释第 4 条。
	Outcome string
	// Params 是实际执行的参数（规范化之后）。
	Params Params
	// BootIDBefore 是下发之前的 boot_id；确认重启就靠它。
	BootIDBefore string
	// IssuedAt 是真正下发命令的时间。倒计时取消时为零值。
	IssuedAt time.Time
	// Canceled 表示倒计时期间被取消，命令**没有**发出去。
	Canceled bool
	// Command 是实际执行的那条命令（人话形式，不是 argv 转义过的）。
	Command string
	// IssuedResult 是 systemctl 的退出码与输出摘要。
	IssuedResult CommandResult
}

// ProgressFunc 回报一次进度。与代理的 ExecuteInput.Progress 同形。
type ProgressFunc func(step string, fields map[string]any)

// ExecuteRequest 是一次执行的输入。
type ExecuteRequest struct {
	Params      Params
	OperationID string
	Progress    ProgressFunc
	// Cancel 在收到安全取消请求时关闭。只在倒计时阶段有意义：
	// 命令一旦发出去，取消就只是一句好听的话了。
	Cancel <-chan struct{}
}

// ErrJobsStarted 表示倒计时结束前有写任务开始跑，因此主动放弃。
var ErrJobsStarted = errors.New("power: 倒计时期间有写任务开始执行")

// Execute 执行一次电源动作。
//
// 返回的 Execution 里的 Outcome 恒为 accepted；出错时返回错误，由代理翻译成
// 任务的 error（失败任务的 result 按契约是空对象）。
func (e *Executor) Execute(ctx context.Context, req ExecuteRequest) (Execution, error) {
	if err := req.Params.Validate(); err != nil {
		return Execution{}, err
	}
	bootID, err := e.ReadBootID()
	if err != nil {
		// 预检已经拦过这一条；走到这里说明预检之后它还坏了，或者有人换了参数。
		return Execution{}, fmt.Errorf("%s: %w", CodeBootIDUnreadable, err)
	}
	ex := Execution{Outcome: OutcomeAccepted, Params: req.Params, BootIDBefore: bootID}

	if err := e.countdown(ctx, req, bootID); err != nil {
		if errors.Is(err, errCanceled) {
			ex.Canceled = true
			return ex, nil
		}
		return ex, err
	}

	// 倒计时结束：最后再看一眼有没有写任务开始了。
	//
	// 预检时看过一次，但那是几分钟前（最长的延迟是 300 秒）。这段时间里
	// 一次 package.remove 完全有可能开跑，而重启会把它断在 dpkg 写库的中间——
	// 那台机器下次开机要人工修。宁可让这次重启失败，也不制造那个现场。
	if e.inFlight != nil {
		running, err := e.inFlight.InFlight(req.OperationID)
		if err != nil {
			// 最后一次检查失败就不发：宁可这次重启失败，也不在“不知道有没有人在写”
			// 的情况下把机器关掉。
			return ex, fmt.Errorf("%s: 结束倒计时前查不到在跑的任务：%w", CodeJobsUnknown, err)
		}
		if len(running) > 0 {
			// 用 %w 包一层 ErrJobsStarted：代理要把“被别人的任务挤掉”与“systemctl 失败”
			// 分成两个码报给管理员，光有文本里的码串它识别不了。
			return ex, fmt.Errorf("%s: %w: %s", CodeAbortedByJob, ErrJobsStarted, describeInFlight(running))
		}
	}

	if e.run == nil {
		// 没接上命令执行器就动手，只会得到一次 panic。宁可明确失败。
		return Execution{}, errors.New(CodeIssueFailed + ": 没有配置命令执行器")
	}

	verb := req.Params.Operation.SystemctlVerb()
	res, runErr := e.run(ctx, e.paths.Systemctl, []string{verb}, issueTimeout)
	ex.Command = e.paths.Systemctl + " " + verb
	ex.IssuedResult = res
	ex.IssuedAt = e.now()
	if runErr != nil || res.ExitCode != 0 {
		return ex, fmt.Errorf("%s: %s", CodeIssueFailed, describeIssueFailure(ex.Command, res, runErr))
	}
	return ex, nil
}

// errCanceled 是内部信号：倒计时被取消。
var errCanceled = errors.New("power: 倒计时被取消")

// countdown 走完倒计时，期间按固定间隔回报剩余秒数。
func (e *Executor) countdown(ctx context.Context, req ExecuteRequest, bootID string) error {
	total := req.Params.Delay()
	start := e.now()
	if req.Progress != nil {
		req.Progress("countdown", map[string]any{
			"remaining_seconds": req.Params.DelaySeconds,
			"total_seconds":     req.Params.DelaySeconds,
			"operation":         string(req.Params.Operation),
			"boot_id_before":    bootID,
		})
	}
	if total <= 0 {
		return nil
	}

	// 每 5 秒回报一次：30—300 秒的窗口里最多 60 条事件。每秒报会把事件表灌满，
	// 而管理员要的只是“还剩多久”，不是逐秒的流水账。
	const step = 5 * time.Second
	remaining := total
	next := step
	if remaining < step {
		next = remaining
	}
	for remaining > 0 {
		d := next
		if d > remaining {
			d = remaining
		}
		select {
		case <-req.Cancel:
			if req.Progress != nil {
				req.Progress("canceled", map[string]any{
					"at_seconds_left": int(remaining.Seconds()),
				})
			}
			return errCanceled
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if err := e.sleep(ctx, d); err != nil {
			return err
		}
		remaining -= d
		if remaining < 0 {
			remaining = 0
		}
		if req.Progress != nil {
			req.Progress("countdown", map[string]any{
				"remaining_seconds": int(remaining.Seconds()),
				"total_seconds":     req.Params.DelaySeconds,
				"operation":         string(req.Params.Operation),
				"elapsed_seconds":   int(e.now().Sub(start).Seconds()),
				"boot_id_before":    bootID,
			})
		}
		next = step
	}
	return nil
}

// describeInFlight 把在跑的操作写成一句能读懂的话（最多列三个）。
func describeInFlight(ops []InFlight) string {
	const max = 3
	parts := make([]string, 0, max)
	for i, op := range ops {
		if i == max {
			parts = append(parts, fmt.Sprintf("等共 %d 个", len(ops)))
			break
		}
		parts = append(parts, op.Action+"（"+op.OperationID+"）")
	}
	return strings.Join(parts, "、")
}

// describeIssueFailure 把一次失败的下发写成管理员能照做的话。
func describeIssueFailure(command string, res CommandResult, err error) string {
	if res.TimedOut {
		return fmt.Sprintf("%s 超过 %s 没有返回，systemd 可能没有响应；请先确认机器还能正常关机",
			command, issueTimeout)
	}
	stderr := strings.TrimSpace(res.Stderr)
	if stderr != "" {
		return fmt.Sprintf("%s 失败（退出码 %d）：%s", command, res.ExitCode, truncate(stderr, 300))
	}
	if err != nil {
		return fmt.Sprintf("%s 没能执行：%v", command, err)
	}
	return fmt.Sprintf("%s 失败（退出码 %d），没有输出", command, res.ExitCode)
}

// truncate 把一段文本缩到 n 个字节以内（只用于错误消息，避免整屏输出进任务记录）。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// shortID 取 boot_id 的前 8 位，够人区分又不占地方。
func shortID(id string) string {
	if id == "" {
		return "读不到"
	}
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
