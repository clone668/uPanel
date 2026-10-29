package agent

// 备份期间的停机/拉起（P12.2）：backup.AppController 的生产实现。
//
// 这一层刻意做得极薄：它不复述“哪些单元受保护、哪些容器危险”，而是把判决交给
// 已有的两个模块（internal/services 与 internal/containers 的 AssessControl /
// ExecuteControl）。理由很直接——如果这里再写一份保护名单，两份名单迟早会分叉，
// 而分叉的方向一定是“备份能停掉 service.control 停不掉的东西”。
//
// 三个不变量：
//
//  1. **只停"刚才在跑"的。** 拉起是把状态改回我们动它之前的样子，不是"把应用启动"。
//     一个本来就是停的应用，备份结束后不该被我们悄悄启动。
//  2. **停与起都要读回确认。** systemd 说 "Job ... done" 不算，容器 exit 0 不算；
//     Verified 为 false 的动作会被上层当成失败（备份不成立、拉起不成立都要说出来）。
//  3. **一个停不下来的就整体不备份。** 半停下来的机器上做出来的快照，既不是
//     files_only 也不是 stop_managed_apps，它是一份没人说得清的东西。

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"upanel/internal/backup"
	"upanel/internal/containers"
	"upanel/internal/services"
)

// BackupAppController 把服务与容器两个模块合成备份需要的应用控制能力。
//
// 两个来源都可以为 nil：nil 的 Services 让所有 systemd 目标报“读不到”，
// nil 的 Containers 让所有容器目标报“读不到”。它们不是"这台机器上没有"——
// 那由 Inspect 返回 Found=false 表达——而是"这个代理没有那部分接线"。
type BackupAppController struct {
	Services   servicesSource
	Containers ContainersSource
}

var _ backup.AppController = (*BackupAppController)(nil)

// Inspect 读这些运行对象的现状（只读，不改任何状态）。
func (c *BackupAppController) Inspect(ctx context.Context, targets []backup.AppTarget) ([]backup.AppRuntime, error) {
	out := make([]backup.AppRuntime, 0, len(targets))
	for _, t := range targets {
		rt, err := c.inspectOne(ctx, t)
		if err != nil {
			return nil, err
		}
		out = append(out, rt)
	}
	return out, nil
}

func (c *BackupAppController) inspectOne(ctx context.Context, t backup.AppTarget) (backup.AppRuntime, error) {
	switch t.Kind {
	case backup.AppKindSystemd:
		if c.Services == nil {
			return backup.AppRuntime{}, fmt.Errorf("app %s: 这个代理没有服务采集端", t.Label)
		}
		if err := services.ValidateUnitName(t.Object); err != nil {
			return backup.AppRuntime{}, fmt.Errorf("app %s: %w", t.Label, err)
		}
		unit, found, err := c.Services.Unit(ctx, t.Object)
		if err != nil {
			return backup.AppRuntime{}, fmt.Errorf("app %s: 读不到单元 %s 的状态: %w", t.Label, t.Object, err)
		}
		rt := backup.AppRuntime{Target: t, Found: found}
		if !found {
			return rt, nil
		}
		rt.Running = unit.ActiveState == services.ActiveActive
		rt.StateText = unit.ActiveState
		if unit.SubState != "" {
			rt.StateText += "/" + unit.SubState
		}
		// 能不能停不在这里判：交给 case 里那套保护规则。
		assessment, err := c.Services.AssessControl(ctx, t.Object, services.OpStop)
		if err != nil {
			return backup.AppRuntime{}, fmt.Errorf("app %s: 预检 %s 失败: %w", t.Label, t.Object, err)
		}
		rt.Blockers = reasonsOf(assessment.Blockers)
		return rt, nil
	case backup.AppKindContainer:
		if c.Containers == nil {
			return backup.AppRuntime{}, fmt.Errorf("app %s: 这个代理没有容器采集端", t.Label)
		}
		if !containers.ValidContainerID(t.Object) {
			return backup.AppRuntime{}, fmt.Errorf("app %s: 容器 id 必须是完整的 64 位十六进制（收到 %q）", t.Label, t.Object)
		}
		container, err := c.Containers.Get(ctx, t.Object)
		if err != nil {
			if errors.Is(err, containers.ErrNotFound) {
				// 绑定过的容器已经不在了：这是"找不到"，不是"读不到"。
				return backup.AppRuntime{Target: t}, nil
			}
			return backup.AppRuntime{}, fmt.Errorf("app %s: 读不到容器 %s: %w", t.Label, t.Object, err)
		}
		rt := backup.AppRuntime{
			Target:    t,
			Found:     true,
			Running:   container.State == containers.StateRunning,
			StateText: container.StateText,
		}
		assessment, err := c.Containers.AssessControl(ctx, containers.ControlParams{
			ContainerID: t.Object, Operation: containers.OpStop,
		})
		if err != nil {
			return backup.AppRuntime{}, fmt.Errorf("app %s: 预检容器 %s 失败: %w", t.Label, t.Object, err)
		}
		rt.Blockers = append(rt.Blockers, containerBlockersOf(assessment.Blockers)...)
		return rt, nil
	}
	return backup.AppRuntime{}, fmt.Errorf("app %s: 未知的运行对象类型 %q", t.Label, t.Kind)
}

// Stop 停掉给定的运行对象，逐条读回确认。
func (c *BackupAppController) Stop(ctx context.Context, runtimes []backup.AppRuntime) ([]backup.AppAction, error) {
	return c.act(ctx, runtimes, "stop", services.OpStop, containers.OpStop)
}

// Start 按原状态拉起停过的运行对象。
func (c *BackupAppController) Start(ctx context.Context, runtimes []backup.AppRuntime) ([]backup.AppAction, error) {
	return c.act(ctx, runtimes, "start", services.OpStart, containers.OpStart)
}

// act 是停与起的共同实现。
//
// 全部试一遍再汇总错误：停到第三个失败时，前两个已经停了——那两条结果必须出现在
// 返回值里，否则上层没法把它们拉回来。
func (c *BackupAppController) act(ctx context.Context, runtimes []backup.AppRuntime,
	verb, serviceOp, containerOp string) ([]backup.AppAction, error) {
	actions := make([]backup.AppAction, 0, len(runtimes))
	var failures []string
	for _, rt := range runtimes {
		action := backup.AppAction{Target: rt.Target, Operation: verb}
		switch rt.Target.Kind {
		case backup.AppKindSystemd:
			if c.Services == nil {
				action.Note = "这个代理没有服务采集端"
				failures = append(failures, rt.Target.Label+"："+action.Note)
				actions = append(actions, action)
				continue
			}
			res, err := c.Services.ExecuteControl(ctx, rt.Target.Object, serviceOp, nil)
			action.Note = controlNote(err, res.Verified, res.Expectation, res.Stderr)
			action.Done = err == nil
			// Verified 来自执行器读回的状态；err 与 Verified 是两件事：
			// 命令可能成功返回，而状态没到位（例如 stop 之后还在 deactivating）。
			action.Verified = res.Verified
			if err != nil {
				failures = append(failures, rt.Target.Label+"："+action.Note)
			}
		case backup.AppKindContainer:
			if c.Containers == nil {
				action.Note = "这个代理没有容器采集端"
				failures = append(failures, rt.Target.Label+"："+action.Note)
				actions = append(actions, action)
				continue
			}
			// 执行前重读一次：预检之后容器可能已经被重建（id 相同、状态不同）。
			before, err := c.Containers.Get(ctx, rt.Target.Object)
			if err != nil {
				action.Note = "读不到容器：" + err.Error()
				failures = append(failures, rt.Target.Label+"："+action.Note)
				actions = append(actions, action)
				continue
			}
			res, err := c.Containers.ExecuteControl(ctx,
				containers.ControlParams{ContainerID: rt.Target.Object, Operation: containerOp}, before, nil)
			action.Note = controlNote(err, res.Verified, res.Expectation, res.Stderr)
			action.Done = err == nil
			action.Verified = res.Verified
			if err != nil {
				failures = append(failures, rt.Target.Label+"："+action.Note)
			}
		default:
			action.Note = fmt.Sprintf("未知的运行对象类型 %q", rt.Target.Kind)
			failures = append(failures, rt.Target.Label+"："+action.Note)
		}
		actions = append(actions, action)
	}
	if len(failures) == 0 {
		return actions, nil
	}
	return actions, fmt.Errorf("%s 应用失败（%d/%d）：%s", verb, len(failures), len(runtimes),
		strings.Join(failures, "；"))
}

// controlNote 把一次控制的结果写成一句人话（Note 会进审计与结果页）。
func controlNote(err error, verified bool, expectation, stderr string) string {
	switch {
	case err != nil:
		msg := err.Error()
		if s := strings.TrimSpace(stderr); s != "" && !strings.Contains(msg, s) {
			msg += "：" + s
		}
		return msg
	case verified:
		return "已读回确认（" + expectation + "）"
	default:
		if strings.TrimSpace(expectation) == "" {
			return "命令返回成功，但没有读回状态确认"
		}
		return "期望 " + expectation + "，没有读回确认"
	}
}

// reasonsOf 把服务的阻塞项翻成备份层的形状。
func reasonsOf(in []services.Reason) []backup.Reason {
	out := make([]backup.Reason, 0, len(in))
	for _, r := range in {
		out = append(out, backup.Reason{Code: r.Code, Message: r.Message})
	}
	return out
}

// containerBlockersOf 把容器的阻塞项翻成备份层的形状。
func containerBlockersOf(in []containers.Block) []backup.Reason {
	out := make([]backup.Reason, 0, len(in))
	for _, b := range in {
		out = append(out, backup.Reason{Code: b.Code, Message: b.Message})
	}
	return out
}
