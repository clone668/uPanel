// Package appruntime 把应用元数据与主机上的运行事实合起来（P09.4）。
//
// 分工很硬：internal/apps 只回答“这个应用是什么、绑在哪个对象上”，运行状态、
// HTTP 状态和依赖保护都在它之外产生。这个包就是那层“之外”——它读 systemd 的
// 单元状态、docker 的容器状态和受管理卷的观测状态，把三者拼成一条应用的摘要。
//
// 三条纪律：
//
//  1. **读不到就说 unknown，不猜。** 服务快照采集失败、docker 守护进程没应答、
//     绑定的容器 ID 已经不在本机列表里……这些都不是“停止了”，混用会把人骗去点
//     一个本来就在跑的东西的启动按钮。
//  2. **能力来自底层对象，不来自类型。** systemd 应用的启停能力就是
//     services.Unit.Capabilities（已包含状态与保护判定），docker 应用的能力就是
//     containers.Container.Capabilities。这一层不复述那些规则，只做搬运——把
//     “类型允许什么”填成能力会直接产出点不动的按钮。
//  3. **依赖保护只做诚实判定。** managed 的含义是“面板是唯一的自动启动者，且
//     那条启动路径上有实测的门禁”，不声称能阻止管理员在 shell 里手工 docker start。
//     做不到这一点的应用一律 unmanaged，并写清是哪条路径绕开了面板。
package appruntime

import (
	"context"

	"upanel/internal/apps"
	"upanel/internal/containers"
	"upanel/internal/services"
	"upanel/internal/storage"
)

// ServicesSource 读 systemd 的服务快照（生产实现是 *services.Collector）。
type ServicesSource interface {
	Snapshot(ctx context.Context) (services.Snapshot, error)
}

// ContainersSource 读容器清单（生产实现是 *containers.Fetcher）。
type ContainersSource interface {
	List(ctx context.Context) (containers.ListResponse, error)
}

// VolumeSource 读受管理卷的观测状态（生产实现是 *storage.Fetcher）。
type VolumeSource interface {
	Volumes(ctx context.Context) ([]storage.Volume, error)
}

// Facts 是一次批量解析用到的运行事实。
//
// 由每一轮请求读一次，然后给同一次响应里的所有应用用：一台机器上几十个应用逐个
// 去问一次 systemctl / docker inspect，既慢又会因为两次读取之间状态变化而自相矛盾。
type Facts struct {
	servicesOK  bool
	servicesErr string
	units       map[string]services.Unit

	containersOK  bool
	containersErr string
	byID          map[string]containers.Container
}

// NoFacts 返回一份“什么都读不到”的事实，reason 会出现在每一条应用的 note 里。
//
// 演示模式（没有配置代理）用它：每一条应用的运行状态都是 unknown、能力全为 false，
// 附带的理由就是这里传进来的那句话。它存在的意义是让“没有数据来源”这件事在
// 上层只出现一次，而不是让每个调用点各写一份空结构。
func NoFacts(reason string) *Facts {
	return &Facts{
		units:         map[string]services.Unit{},
		byID:          map[string]containers.Container{},
		servicesErr:   reason,
		containersErr: reason,
	}
}

// Info 是一条应用的运行摘要（对应 AppSummary 里那四个字段）。
type Info struct {
	// RuntimeState 是 runtime_state 枚举（apps.Runtime*）。
	RuntimeState string
	// RuntimeNote 说明为什么是这个状态；无话可说时为空串（序列化成 null）。
	// 它只解释已观测到的事实，不预测。
	RuntimeNote string
	// Capabilities 是此刻真的能做到的动作。
	Capabilities apps.Capabilities
	// Protection 是 dependency_protection 枚举（apps.Protection*）。
	Protection string
	// ProtectionNote 说明 managed/unmanaged 的判断依据。
	ProtectionNote string
}

// Resolver 读一次主机状态，产出所有应用的运行摘要。
type Resolver struct {
	Services   ServicesSource
	Containers ContainersSource
}

// Snapshot 读一轮运行事实。任何一路读失败都降级成“这一路不可信”，不返回错误：
// /apps 是纯数据库元数据也能返回的接口（CONTRACTS.md 第 8.1 节），
// 代理掉了不该让整个应用列表变成 503——那是把“这台机器上有哪些入口”这件事一起弄丢了。
func (r *Resolver) Snapshot(ctx context.Context) *Facts {
	f := &Facts{units: map[string]services.Unit{}, byID: map[string]containers.Container{}}

	if r.Services == nil {
		f.servicesErr = "未配置代理连接"
	} else if snap, err := r.Services.Snapshot(ctx); err != nil {
		f.servicesErr = "读取服务状态失败：" + err.Error()
	} else if snap.Status != services.StatusOK {
		// 快照失败时 units 里是**上一次成功的内容**（采集器保留旧值），拿它当现状
		// 会把一小时的旧状态说成此刻的状态。
		f.servicesErr = snap.Error
		if f.servicesErr == "" {
			f.servicesErr = "最近一次服务状态采集没有成功"
		}
	} else {
		f.servicesOK = true
		for _, u := range snap.Units {
			f.units[u.Unit] = u
		}
	}

	if r.Containers == nil {
		f.containersErr = "未配置代理连接"
	} else if resp, err := r.Containers.List(ctx); err != nil {
		f.containersErr = "读取容器列表失败：" + err.Error()
	} else if !resp.Available {
		f.containersErr = resp.Notice
		if f.containersErr == "" {
			f.containersErr = "Docker 不可用"
		}
	} else {
		f.containersOK = true
		for _, c := range resp.Containers {
			f.byID[c.ID] = c
		}
	}
	return f
}

// App 产出一条应用的运行摘要。
func (f *Facts) App(a apps.App) Info {
	info := Info{
		Protection: apps.ProtectionNotApplicable,
		Capabilities: apps.Capabilities{
			Open: a.PublicURL != "",
		},
	}
	switch a.Type {
	case apps.TypeLink:
		// link 只有入口，没有运行时：没有可启动的东西，也就没有依赖可守。
		info.RuntimeState = apps.RuntimeNotApplicable
		info.ProtectionNote = ""
		return info
	case apps.TypeSystemd:
		f.systemd(&info, a)
	case apps.TypeDocker:
		f.docker(&info, a)
	case apps.TypeCompose:
		info.RuntimeState = apps.RuntimeUnknown
		info.RuntimeNote = "关联的是面板部署（compose），部署能力要在 1.0 才落地：此刻面板读不到它的运行状态"
		if len(a.VolumeDependencies) > 0 {
			info.Protection = apps.ProtectionUnmanaged
			info.ProtectionNote = "部署还没有落地，面板无法接管它和依赖卷之间的启动顺序"
		}
		return info
	default:
		info.RuntimeState = apps.RuntimeUnknown
		return info
	}
	if len(a.VolumeDependencies) == 0 {
		info.Protection = apps.ProtectionNotApplicable
		info.ProtectionNote = ""
	}
	return info
}

// systemd 部分：运行时状态来自 ActiveState，能力来自 Unit.Capabilities。
func (f *Facts) systemd(info *Info, a apps.App) {
	if !f.servicesOK {
		info.RuntimeState = apps.RuntimeUnknown
		info.RuntimeNote = "服务状态这一轮没有读到（" + f.servicesErr + "），运行状态未确认"
		f.systemdProtection(info, a, services.Unit{}, false)
		return
	}
	unit, ok := f.units[a.Binding.Unit]
	if !ok {
		// 绑定在库里，但本机服务列表里没有它。不写“已删除”：可能只是改名、
		// 被 mask、或者单元文件坏了——观测到的事实只支持“看不到”。
		info.RuntimeState = apps.RuntimeUnknown
		info.RuntimeNote = "绑定的单元不在本机服务列表里：可能已改名或被卸载，请重新确认绑定"
		f.systemdProtection(info, a, services.Unit{}, false)
		return
	}
	info.Capabilities.Start, info.Capabilities.Stop, info.Capabilities.Restart =
		unitControl(unit)
	// 看日志不是写操作：受保护单元（面板自己、SSH、核心服务）也能读，
	// 所以 logs 不受 Protection 影响。单元文件都读不出来时不给日志按钮。
	info.Capabilities.Logs = unit.LoadState != services.LoadNotFound && unit.LoadState != services.LoadError
	if unit.LoadState == services.LoadNotFound || unit.LoadState == services.LoadError {
		info.RuntimeState = apps.RuntimeUnknown
		info.RuntimeNote = "单元文件读不出来（load_state=" + unit.LoadState + "）"
	} else {
		switch unit.ActiveState {
		case services.ActiveActive, services.ActiveReloading:
			info.RuntimeState = apps.RuntimeRunning
		case services.ActiveFailed:
			info.RuntimeState = apps.RuntimeFailed
			info.RuntimeNote = "systemd 报告单元进入 failed" + substateNote(unit.SubState)
		case services.ActiveActivating, services.ActiveDeactivating:
			// 正在启动/停止既不是 running 也不是 stopped，中间态说成任何一头都在撒谎。
			info.RuntimeState = apps.RuntimeUnknown
			info.RuntimeNote = "单元处于过渡状态（" + unit.ActiveState + substateNote(unit.SubState) + "）"
		default:
			info.RuntimeState = apps.RuntimeStopped
		}
	}
	if unit.Template {
		info.RuntimeNote = joinNote(info.RuntimeNote, "这是模板单元（foo@.service），没有实例就没有可操作的对象")
	}
	f.systemdProtection(info, a, unit, true)
}

// systemdProtection 判定 systemd 应用的依赖保护。
//
// P09.4 **不改写单元文件**：没有写入 RequiresMountsFor/BindsTo，也没有包装单元。
// 因此开机时谁先起来，仍然由这个单元自己（以及 enable 出来的依赖）决定，
// 面板只能拦住“经面板发起的启动”。这就是 unmanaged，理由要写清楚是哪一条。
func (f *Facts) systemdProtection(info *Info, a apps.App, unit services.Unit, loaded bool) {
	if len(a.VolumeDependencies) == 0 {
		return
	}
	info.Protection = apps.ProtectionUnmanaged
	switch {
	case !loaded:
		info.ProtectionNote = "单元状态没有读到，面板无法判断它会不会在依赖卷挂载之前自己起来"
	case unit.Enablement == services.EnablementEnabled:
		info.ProtectionNote = "单元处于 enabled：开机会由 systemd 直接拉起，不经过面板的依赖卷检查；面板没有改写它的 RequiresMountsFor/BindsTo"
	default:
		info.ProtectionNote = "面板没有改写这个单元的开机依赖（RequiresMountsFor/BindsTo），它被别的单元拉起时不会经过面板的依赖卷检查"
	}
}

// docker 部分：运行时状态来自容器状态，能力来自 ControlCapabilities。
func (f *Facts) docker(info *Info, a apps.App) {
	if !f.containersOK {
		info.RuntimeState = apps.RuntimeUnknown
		info.RuntimeNote = "容器状态这一轮没有读到（" + f.containersErr + "），运行状态未确认"
		f.dockerProtection(info, a, containers.Container{}, false)
		return
	}
	c, ok := f.byID[a.Binding.ContainerID]
	if !ok {
		// P09 验收里那条“容器重建 ID 变化后旧绑定要求重新确认”就是这里：
		// 面板不会因为新容器的名字一样就自动把旧应用接过去。
		info.RuntimeState = apps.RuntimeUnknown
		info.RuntimeNote = "绑定的容器 ID 不在本机容器列表里：容器可能已被重建（重建出来的是新 ID），请重新确认绑定"
		f.dockerProtection(info, a, containers.Container{}, false)
		return
	}
	info.Capabilities.Start = c.Capabilities.Start
	info.Capabilities.Stop = c.Capabilities.Stop
	info.Capabilities.Restart = c.Capabilities.Restart
	info.Capabilities.Logs = true
	switch c.State {
	case containers.StateRunning:
		info.RuntimeState = apps.RuntimeRunning
	case containers.StateStopped:
		info.RuntimeState = apps.RuntimeStopped
	default:
		info.RuntimeState = apps.RuntimeUnknown
		info.RuntimeNote = "容器处于 " + dockerStateText(c) + "：既不是稳定的运行，也不是稳定的停止"
	}
	if !c.Capabilities.Managed {
		info.RuntimeNote = joinNote(info.RuntimeNote,
			"容器被判定为高风险（"+riskSummary(c)+"），面板不提供它的启停")
	}
	f.dockerProtection(info, a, c, true)
}

// dockerProtection 判定 docker 应用的依赖保护。
//
// 判据是重启策略：restart=no（docker 默认值）意味着容器不会自己启动——开机不会、
// 崩溃也不会。起来只能靠人，而人经面板发起的启动会被依赖卷门禁拦住。这种情形下
// 面板确实是“受管理的启动路径上唯一的自动启动者”，可以标 managed。
//
// restart=always / unless-stopped / on-failure 时 docker 会自己把它拉起来
// （开机时、崩溃后），那条路径上没有任何依赖卷检查，因此只能 unmanaged。
func (f *Facts) dockerProtection(info *Info, a apps.App, c containers.Container, found bool) {
	if len(a.VolumeDependencies) == 0 {
		return
	}
	if !found {
		info.Protection = apps.ProtectionUnmanaged
		info.ProtectionNote = "容器状态没有读到，面板无法判断它会不会在依赖卷挂载之前自己启动"
		return
	}
	switch c.RestartPolicy {
	case "", "no":
		info.Protection = apps.ProtectionManaged
		info.ProtectionNote = "容器重启策略是 no：它不会自己启动，经面板发起的启动会被依赖卷门禁拦住"
	default:
		info.Protection = apps.ProtectionUnmanaged
		info.ProtectionNote = "容器重启策略是 " + c.RestartPolicy + "：开机或崩溃后由 Docker 自行拉起，不经过面板的依赖卷检查"
	}
}

// unitControl 从单元的能力集合里取出启停三项。
func unitControl(u services.Unit) (start, stop, restart bool) {
	for _, op := range u.Capabilities {
		switch op {
		case services.OpStart:
			start = true
		case services.OpStop:
			stop = true
		case services.OpRestart:
			restart = true
		}
	}
	return start, stop, restart
}

func substateNote(sub string) string {
	if sub == "" {
		return ""
	}
	return "（sub_state=" + sub + "）"
}

func dockerStateText(c containers.Container) string {
	if c.StateText != "" {
		return "「" + c.StateText + "」"
	}
	return "未知状态"
}

func riskSummary(c containers.Container) string {
	if len(c.Risk.Reasons) == 0 {
		return "风险等级 " + c.Risk.Level
	}
	out := ""
	for i, r := range c.Risk.Reasons {
		if i > 0 {
			out += "；"
		}
		out += r.Message
	}
	return out
}

// joinNote 把两句话接起来，空的那句不留痕。
func joinNote(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + "；" + b
}
