package network

import "context"

// Collector 把 Deps 收成一个可调用的采集端（P14.1）。
//
// 它只做转发，不加缓存：网络状态的变化速度是"管理员敲一条 netplan apply"，
// 缓存窗口里的旧结果会被读成"我改的东西没生效"。四条查询各自一次采集，
// 单次上限由 Deps.Timeout 兜住。
type Collector struct {
	Deps Deps
}

// NewCollector 构造采集端。deps 的零值即生产默认（真实 /proc、/sys，无命令执行端）。
func NewCollector(deps Deps) *Collector { return &Collector{Deps: deps} }

// Interfaces 读网卡、地址与"能不能改"的判定。
func (c *Collector) Interfaces(ctx context.Context) InterfaceList {
	return Interfaces(ctx, c.Deps)
}

// Routes 读路由表与默认网关。
func (c *Collector) Routes(ctx context.Context) RouteList {
	return Routes(ctx, c.Deps)
}

// Listeners 读监听端口（数据来自 /proc，不跑命令，因此不看 ctx 的时限——
// procfs 读不会阻塞；还是收着 ctx，让调用方签名一致）。
func (c *Collector) Listeners(_ context.Context) ListenerList {
	return Listeners(c.Deps)
}

// Firewall 读防火墙现状与管理者判定。
func (c *Collector) Firewall(ctx context.Context) FirewallState {
	return Firewall(ctx, c.Deps)
}

// SystemDeps 是生产依赖：真实文件系统、真实 /proc 与 /sys、无命令执行端。
//
// Runner 由装配方注入（代理进程把它接到 syscmd 上）。这里不填 Runner，
// 是为了让"没有命令执行端"这件事在能力判定里如实反映成 missing，
// 而不是在包里偷偷开一个 os/exec 的口子。
func SystemDeps() Deps { return Deps{} }
