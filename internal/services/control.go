package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// 阻塞项代码（预检里展示给管理员的固定枚举）。
const (
	// CodeUnitNotFound：目标单元不在这台机器上。
	CodeUnitNotFound = "SERVICE_NOT_FOUND"
	// CodeProtected：受保护单元（面板/代理/SSH/核心/网络/其他模块）。
	CodeProtected = "SERVICE_PROTECTED"
	// CodeStateConflict：当前状态与该操作矛盾（已 active 还 start、已 inactive 还 stop…）。
	CodeStateConflict = "SERVICE_STATE_CONFLICT"
	// CodeNotEnableable：单元文件没有 [Install] 段或被 mask，改不了开机启动。
	CodeNotEnableable = "SERVICE_NOT_ENABLEABLE"
)

// 告警项代码。
const (
	// WarningInterruption：stop/restart 会中断这个服务提供的功能。
	WarningInterruption = "service_interruption"
	// WarningScheduledStart：disable 只改开机启动，不会停止当前运行中的服务。
	WarningScheduledStart = "scheduled_start_removed"
)

// 影响类别（与 CONTRACTS.md 4.1 的 impacts.kind 一致）。
const (
	ImpactAvailability = "service_availability"
	ImpactNone         = "none"
)

// verifyTimeout 是执行后等待状态收敛的上限。
//
// systemctl 同步返回时状态通常已经收敛；但 stop 可能等 Type=oneshot 的 ExecStop、
// restart 会先 stop 再 start，给 15 秒余量。超过就如实报“命令成功但状态未收敛”。
const verifyTimeout = 15 * time.Second

// verifyInterval 是收敛轮询间隔。
const verifyInterval = 150 * time.Millisecond

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

// Assessment 是一次服务操作的预检结论。
//
// 它不执行任何写操作：只读 systemctl 的当前状态，得出“能不能做、为什么不能”。
type Assessment struct {
	// Found=false 表示单元不在这台机器上（预检拒绝，不是内部错误）。
	Found bool
	// Unit 是现场读到的单元状态。
	Unit Unit
	// DisplayName 是给管理员看的目标名。
	DisplayName string
	// Identity 是单元身份的指纹（单元名本身；执行前复核它没有变）。
	Identity string
	// StateVersion 是当前状态的版本（预检到执行之间变了就失效）。
	StateVersion string
	// Allowed 表示当前状态下是否允许执行。
	Allowed bool
	// Blockers 阻止执行的原因（Allowed=false 时非空）。
	Blockers []Reason
	// Warnings 不阻止执行但必须告知的后果。
	Warnings []Reason
	// Impacts 这次操作会造成的后果。
	Impacts []Impact
	// Locks 执行时要持有的资源锁。
	Locks []string
}

// ControlResult 是一次服务操作的执行结果。
type ControlResult struct {
	Op     string
	Unit   string
	Before Unit
	After  Unit
	// Stdout/Stderr/ExitCode 是 systemctl 的原始结果（审计与排错用）。
	Stdout   string
	Stderr   string
	ExitCode int
	// Verified 只有读回的状态与操作期望一致时才为 true。
	Verified bool
	// Expectation 是这次操作期望达到的状态（中文），未收敛时用于解释。
	Expectation string
	Duration    time.Duration
}

// ServiceIdentity 返回单元的身份指纹：单元名是唯一身份，不做名字猜测。
//
// 用完整的 SHA-256（64 位十六进制）：storage 那边同一套前缀格式也是 64 位，
// 截断到 16 字节只会让“指纹”在不同模块里长得不一样，没有换来任何好处。
func ServiceIdentity(unit string) string {
	sum := sha256.Sum256([]byte("service/v1|" + unit))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// AssessControl 计算一次服务操作的预检结论（只读）。
func (c *Collector) AssessControl(ctx context.Context, unit, op string) (Assessment, error) {
	if err := ValidateUnitName(unit); err != nil {
		return Assessment{}, err
	}
	if !ValidOperation(op) {
		return Assessment{}, fmt.Errorf("services: 不支持的操作 %q", op)
	}
	// 模板单元没有实例：systemctl 不接受这种名字，也就不存在“它现在的状态”。
	// 这不是“不存在”，而是“你给的名字不完整”，所以是状态冲突而不是 NOT_FOUND。
	if IsTemplate(unit) {
		return Assessment{
			Found:        false,
			DisplayName:  unit + "（模板单元，需要实例）",
			Identity:     ServiceIdentity(unit),
			StateVersion: "absent",
			Allowed:      false,
			Blockers: []Reason{{
				Code: CodeStateConflict,
				Message: "模板单元 " + unit + " 需要指定实例（例如 " +
					strings.Replace(unit, "@.service", "@<实例名>.service", 1) +
					"）；面板不替你挑选实例，也不按前缀批量操作",
			}},
			Impacts: []Impact{{Kind: ImpactNone, Description: "没有目标实例，不会做任何变更"}},
			Locks:   []string{"service:" + unit},
		}, nil
	}
	if c.ctl == nil {
		return Assessment{}, ErrSystemctlUnavailable
	}
	raw, found, err := c.ctl.ShowOne(ctx, unit)
	if err != nil {
		return Assessment{}, err
	}

	now := time.Now().UTC().Truncate(time.Second)
	u, ok := unitFromRaw(raw, now)
	if !found || !ok {
		return Assessment{
			Found:        false,
			DisplayName:  unit + "（不存在）",
			Identity:     ServiceIdentity(unit),
			StateVersion: "absent",
			Allowed:      false,
			Blockers: []Reason{{
				Code:    CodeUnitNotFound,
				Message: "这台机器上没有服务单元 " + unit + "（不会把它当成命令片段去执行）",
			}},
			Impacts: []Impact{{Kind: ImpactNone, Description: "没有目标，不会做任何变更"}},
			Locks:   []string{"service:" + unit},
		}, nil
	}

	a := Assessment{
		Found:        true,
		Unit:         u,
		DisplayName:  displayName(u),
		Identity:     ServiceIdentity(unit),
		StateVersion: StateVersion(u),
		Locks:        []string{"service:" + unit},
	}

	// 保护名单先判：受保护单元不进入“状态冲突”的讨论——拒绝理由必须是“不能碰”，
	// 而不是“已经启动了所以不用启动”。
	if u.Protection.Protected {
		a.Blockers = append(a.Blockers, Reason{
			Code:    CodeProtected,
			Message: "受保护的服务，面板不允许对它执行任何操作：" + strings.Join(u.Protection.Reasons, "；"),
		})
		a.Impacts = []Impact{{Kind: ImpactNone, Description: "受保护单元不会被改动"}}
		return a, nil
	}
	if u.Template {
		a.Blockers = append(a.Blockers, Reason{
			Code:    CodeStateConflict,
			Message: "这是模板单元（" + unit + "），必须指定实例（例如 " + strings.Replace(unit, "@.", "@实例名.", 1) + "）",
		})
		return a, nil
	}

	switch op {
	case OpStart:
		if u.ActiveState == ActiveActive || u.ActiveState == ActiveReloading || u.ActiveState == ActiveActivating {
			a.Blockers = append(a.Blockers, Reason{
				Code:    CodeStateConflict,
				Message: "服务当前已经是 " + u.ActiveState + "，不需要再启动",
			})
		}
		if u.Enablement == EnablementMasked {
			a.Blockers = append(a.Blockers, Reason{
				Code:    CodeStateConflict,
				Message: "服务已被 mask，无法启动；需要先在主机上解除 mask",
			})
		}
	case OpStop:
		if u.ActiveState == ActiveInactive {
			a.Blockers = append(a.Blockers, Reason{
				Code:    CodeStateConflict,
				Message: "服务当前已经是 inactive，不需要停止",
			})
		}
	case OpRestart:
		if u.ActiveState == ActiveInactive {
			a.Blockers = append(a.Blockers, Reason{
				Code:    CodeStateConflict,
				Message: "服务当前没有运行，不能重启（请使用启动）",
			})
		}
	case OpEnable:
		if u.Enablement == EnablementEnabled {
			a.Blockers = append(a.Blockers, Reason{
				Code:    CodeStateConflict,
				Message: "服务已经设置为开机启动",
			})
		}
		if !canEnable(u) {
			a.Blockers = append(a.Blockers, Reason{
				Code:    CodeNotEnableable,
				Message: "该单元没有可安装的 [Install] 段（" + u.Enablement + "），无法设置为开机启动",
			})
		}
	case OpDisable:
		if u.Enablement == EnablementDisabled {
			a.Blockers = append(a.Blockers, Reason{
				Code:    CodeStateConflict,
				Message: "服务本来就不是开机启动",
			})
		}
		if !canDisable(u) {
			a.Blockers = append(a.Blockers, Reason{
				Code:    CodeNotEnableable,
				Message: "该单元的开机启用状态是 " + u.Enablement + "，不能通过面板取消",
			})
		}
	}

	if op == OpStop || op == OpRestart {
		a.Warnings = append(a.Warnings, Reason{
			Code:    WarningInterruption,
			Message: "操作期间该服务提供的能力会中断（" + displayName(u) + "）",
		})
		a.Impacts = append(a.Impacts, Impact{
			Kind:        ImpactAvailability,
			Description: "服务在操作期间不可用；已经建立的连接可能被断开",
		})
	}
	if op == OpDisable {
		a.Warnings = append(a.Warnings, Reason{
			Code:    WarningScheduledStart,
			Message: "只改变开机启动设置，不会停止当前运行中的服务",
		})
		a.Impacts = append(a.Impacts, Impact{
			Kind:        ImpactNone,
			Description: "下次开机不再自动启动；当前进程不受影响",
		})
	}
	if len(a.Impacts) == 0 {
		a.Impacts = append(a.Impacts, Impact{
			Kind:        ImpactNone,
			Description: "不改变其他服务；只影响目标单元自身",
		})
	}

	a.Allowed = len(a.Blockers) == 0
	return a, nil
}

// canEnable 判断能否设置开机启动。
func canEnable(u Unit) bool {
	return u.Enablement == EnablementDisabled
}

// canDisable 判断能否取消开机启动。
func canDisable(u Unit) bool {
	return u.Enablement == EnablementEnabled
}

// displayName 是“单元名（描述）”形式，描述为空时只有单元名。
func displayName(u Unit) string {
	if strings.TrimSpace(u.Description) == "" {
		return u.Unit
	}
	return u.Unit + "（" + u.Description + "）"
}

// ErrControlRejected 表示执行前复核发现目标已经不允许操作（预检之后状态变了）。
var ErrControlRejected = errors.New("services: 目标当前状态不允许该操作")

// ExecuteControl 执行一次服务操作并**读回验证**。
//
// 顺序：现场复核 → 执行 systemctl → 轮询读回 → 只有状态符合期望才 Verified。
// “systemctl 返回 0” 不是验证：Type=oneshot 的 ExecStart 可能失败而 systemctl 仍返回 0，
// 执行前也可能已经有别的进程改了状态。
func (c *Collector) ExecuteControl(ctx context.Context, unit, op string, progress func(step string, fields map[string]any)) (ControlResult, error) {
	if err := ValidateUnitName(unit); err != nil {
		return ControlResult{}, err
	}
	if !ValidOperation(op) {
		return ControlResult{}, fmt.Errorf("services: 不支持的操作 %q", op)
	}
	if c.ctl == nil {
		return ControlResult{}, ErrSystemctlUnavailable
	}
	if IsTemplate(unit) {
		// 预检已经拦过；这里再拦一次，避免任何路径把模板名交给 systemctl。
		return ControlResult{}, fmt.Errorf("%w: %s 是模板单元，需要指定实例", ErrControlRejected, unit)
	}

	// 执行前复核：保护名单与状态都可能在上游复核之后又变了（例如别人手工改过）。
	before, found, err := c.Unit(ctx, unit)
	if err != nil {
		return ControlResult{}, err
	}
	if !found {
		return ControlResult{}, fmt.Errorf("%w: %s 不存在", ErrControlRejected, unit)
	}
	if before.Protection.Protected {
		return ControlResult{}, fmt.Errorf("%w: %s 受保护（%s）", ErrControlRejected, unit,
			strings.Join(before.Protection.Reasons, "；"))
	}
	res := ControlResult{Op: op, Unit: unit, Before: before, Expectation: expectation(op)}

	started := time.Now()
	cmdRes, cmdErr := c.ctl.Control(ctx, op, unit)
	res.Stdout = cmdRes.Stdout
	res.Stderr = cmdRes.Stderr
	res.ExitCode = cmdRes.ExitCode
	if progress != nil {
		progress("systemctl_"+op, map[string]any{
			"unit":      unit,
			"exit_code": cmdRes.ExitCode,
			"duration":  cmdRes.Duration.Milliseconds(),
		})
	}
	if cmdErr != nil {
		res.Duration = time.Since(started)
		if strings.TrimSpace(res.Stderr) != "" {
			return res, fmt.Errorf("services: systemctl %s %s 失败（退出状态 %d）：%s",
				op, unit, res.ExitCode, strings.TrimSpace(res.Stderr))
		}
		return res, fmt.Errorf("services: systemctl %s %s 失败: %w", op, unit, cmdErr)
	}

	// 读回验证。轮询到超时为止；每次都是现场读取，不看缓存。
	deadline := time.Now().Add(c.verifyTimeout)
	var last Unit
	for {
		after, found, err := c.Unit(ctx, unit)
		if err != nil {
			res.Duration = time.Since(started)
			return res, fmt.Errorf("services: 执行后读取状态失败: %w", err)
		}
		if !found {
			res.Duration = time.Since(started)
			return res, fmt.Errorf("services: 执行后单元 %s 不见了（被删除或被 mask？）", unit)
		}
		last = after
		if converged(op, after, started) {
			res.After = after
			res.Verified = true
			res.Duration = time.Since(started)
			if progress != nil {
				progress("verified", map[string]any{
					"unit":         unit,
					"active_state": after.ActiveState,
					"sub_state":    after.SubState,
					"enablement":   after.Enablement,
				})
			}
			return res, nil
		}
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			res.After = last
			res.Duration = time.Since(started)
			return res, fmt.Errorf("%w: 等待状态收敛时被中断", ErrControlRejected)
		case <-time.After(c.verifyInterval):
		}
	}

	res.After = last
	res.Duration = time.Since(started)
	if progress != nil {
		progress("verify_timeout", map[string]any{
			"unit":         unit,
			"active_state": last.ActiveState,
			"sub_state":    last.SubState,
			"enablement":   last.Enablement,
			"expected":     res.Expectation,
		})
	}
	return res, fmt.Errorf("services: 命令已返回成功，但 %s 没有在 %s 内收敛到期望状态（%s，当前 %s/%s）",
		unit, c.verifyTimeout, res.Expectation, last.ActiveState, last.SubState)
}

// expectation 返回操作期望达到的状态（中文，供界面与错误信息使用）。
func expectation(op string) string {
	switch op {
	case OpStart:
		return "服务处于 active"
	case OpStop:
		return "服务处于 inactive"
	case OpRestart:
		return "服务重新进入 active"
	case OpEnable:
		return "开机启动已启用"
	case OpDisable:
		return "开机启动已关闭"
	}
	return "状态符合预期"
}

// converged 判断读回的状态是否满足操作期望。
//
// restart 额外比较 ActiveEnterTimestamp：只看 active 会把“本来就在 active、restart 实际没生效”
// 判成成功——那正是最需要发现的情况。
func converged(op string, u Unit, started time.Time) bool {
	switch op {
	case OpStart:
		return u.ActiveState == ActiveActive || u.ActiveState == ActiveReloading
	case OpStop:
		return u.ActiveState == ActiveInactive
	case OpRestart:
		if u.ActiveState != ActiveActive && u.ActiveState != ActiveReloading {
			return false
		}
		if u.StartedAt == nil {
			return false
		}
		// 允许 2 秒时钟粒度误差：systemd 的时间戳是秒级。
		return !u.StartedAt.Before(started.Add(-2 * time.Second))
	case OpEnable:
		return u.Enablement == EnablementEnabled
	case OpDisable:
		return u.Enablement == EnablementDisabled
	}
	return false
}
