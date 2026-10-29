package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"upanel/internal/network"
	"upanel/internal/syscmd"
)

// 网络与防火墙的只读查询（P14.1）。四个名字都是"给事实"，不是"给判断"：
//
//   - network.interfaces：网卡、地址、以及"这份配置能不能通过面板改"的判定依据；
//   - network.routes：路由表与默认网关（v4 与 v6 各一份）；
//   - network.listeners：谁在监听哪个地址端口（来自 /proc，不跑 ss）；
//   - firewall.state：防火墙管理器、默认策略与现有规则。
//
// **这一阶段没有任何写入口**：改地址是 P14.2 的 network.apply，改规则是 P14.3 的
// firewall.apply，都要走预检 → 任务。在这四个查询背后藏一个"顺便应用"的开关，
// 等于把确认、回滚与审计一起绕过去。
const (
	// NetworkInterfacesQueryName 是网卡与地址查询。
	NetworkInterfacesQueryName = "network.interfaces"
	// NetworkRoutesQueryName 是路由查询。
	NetworkRoutesQueryName = "network.routes"
	// NetworkListenersQueryName 是监听端口查询。
	NetworkListenersQueryName = "network.listeners"
	// FirewallStateQueryName 是防火墙现状查询。
	FirewallStateQueryName = "firewall.state"
)

// NetworkSource 是四条只读查询的实现端（生产实现是 *network.Collector）。
//
// 定义在消费方：测试给假实现，生产里没有"固定成功"的替身。
type NetworkSource interface {
	Interfaces(ctx context.Context) network.InterfaceList
	Routes(ctx context.Context) network.RouteList
	Listeners(ctx context.Context) network.ListenerList
	Firewall(ctx context.Context) network.FirewallState
}

// networkQueryParams 是这四条查询共用的参数：空。
//
// 空结构 + DisallowUnknownFields（DecodeParams 的规矩）让 {"command":"..."} 这样的
// 多余字段被拒绝，而不是被静默忽略。带参的只有将来的写入动作，它们不走这里。
type networkQueryParams struct{}

// newNetworkQuery 构造一条无参只读查询。
func newNetworkQuery(name string, run func(ctx context.Context) any) Query {
	return Query{
		Name: name,
		Decode: func(raw json.RawMessage) (any, error) {
			var params networkQueryParams
			if err := DecodeParams(raw, &params); err != nil {
				return nil, err
			}
			return params, nil
		},
		Run: func(ctx context.Context, _ any) (any, error) {
			return run(ctx), nil
		},
	}
}

// newNetworkInterfacesQuery 登记 network.interfaces。
func newNetworkInterfacesQuery(src NetworkSource) Query {
	return newNetworkQuery(NetworkInterfacesQueryName, func(ctx context.Context) any {
		return src.Interfaces(ctx)
	})
}

// newNetworkRoutesQuery 登记 network.routes。
func newNetworkRoutesQuery(src NetworkSource) Query {
	return newNetworkQuery(NetworkRoutesQueryName, func(ctx context.Context) any {
		return src.Routes(ctx)
	})
}

// newNetworkListenersQuery 登记 network.listeners。
func newNetworkListenersQuery(src NetworkSource) Query {
	return newNetworkQuery(NetworkListenersQueryName, func(ctx context.Context) any {
		return src.Listeners(ctx)
	})
}

// newFirewallStateQuery 登记 firewall.state。
func newFirewallStateQuery(src NetworkSource) Query {
	return newNetworkQuery(FirewallStateQueryName, func(ctx context.Context) any {
		return src.Firewall(ctx)
	})
}

// ---- 生产依赖 ----

// networkRunner 是 network.Runner 的生产实现：把"程序名 + 参数"接到 syscmd 上。
//
// 程序名到绝对路径的解析在**固定目录**里做（/usr/sbin、/sbin、/usr/bin、/bin），
// 不走 exec.LookPath：PATH 里可能有 /usr/local 之类可写目录，而这两个程序
// 要读的是内核与防火墙的真实状态，不该由 PATH 决定跑的是谁。
type networkRunner struct{}

// networkProgramDirs 是允许找到 ip/ufw 的目录，顺序固定。
var networkProgramDirs = []string{"/usr/sbin", "/sbin", "/usr/bin", "/bin"}

// networkPrograms 是允许通过 Runner 执行的程序。**闭集**：network 包只会问这两个，
// 任何人加第三个都必须先改这里——不允许把 HTTP 参数拼成程序名。
var networkPrograms = map[string]bool{"ip": true, "ufw": true}

// networkCommandTimeout 是单条命令的时限，比采集总预算（8 秒）略紧：
// 总预算掐下来时，结果里应该是"命令超时"，而不是"命令没跑过"。
const networkCommandTimeout = 6 * time.Second

func (networkRunner) Run(ctx context.Context, program string, args ...string) ([]byte, int, error) {
	if !networkPrograms[program] {
		return nil, 0, fmt.Errorf("%w: 不允许执行 %q", network.ErrProgramMissing, program)
	}
	path := ""
	for _, dir := range networkProgramDirs {
		candidate := dir + "/" + program
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			path = candidate
			break
		}
	}
	if path == "" {
		return nil, 0, network.ErrProgramMissing
	}

	res, err := syscmd.Run(ctx, syscmd.Spec{Program: path, Args: args}, syscmd.Limits{
		Timeout:   networkCommandTimeout,
		MaxStdout: 1 << 20,
		MaxStderr: 64 << 10,
	})
	if err != nil {
		// 非零退出码是"命令说了话"，交给调用方按语义解释（ufw 对"没启用"就是这样）。
		var exitErr *syscmd.ExitError
		if errors.As(err, &exitErr) {
			return []byte(res.Stdout), res.ExitCode, nil
		}
		// 超时翻译成 context.DeadlineExceeded：采集侧据此把这条记成 timeout，
		// 而不是笼统的 failed——"这台机器 6 秒没答出 ip"与"ip 用错了"要分开。
		if errors.Is(err, syscmd.ErrTimeout) {
			return []byte(res.Stdout), res.ExitCode, fmt.Errorf("跑 %s 超时: %w", program, context.DeadlineExceeded)
		}
		return []byte(res.Stdout), res.ExitCode, err
	}
	return []byte(res.Stdout), res.ExitCode, nil
}

// NewNetworkSource 组装生产用的网络只读采集端。
//
// 装配方只需决定"接不接命令执行端"：文件系统与 /proc、/sys 的读取是进程内的，
// 不需要额外权限。ip 与 ufw 不在时，采集仍能给出网卡清单（来自 sysfs）与监听端口
// （来自 /proc），只是地址或防火墙状态那一块会带一条 missing——这比整页报错有用。
func NewNetworkSource() NetworkSource {
	deps := network.SystemDeps()
	deps.Runner = networkRunner{}
	return network.NewCollector(deps)
}
