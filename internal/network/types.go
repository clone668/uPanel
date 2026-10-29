// Package network 读取本机的网络现状：网卡与地址、路由、监听端口、防火墙。
//
// P14.1 的范围是**只读**：这一包不写任何配置，也不碰 netplan、UFW 或 systemd 单元。
// 它要回答的问题只有两个——"现在是什么样"，以及"这份现状我们将来能不能安全地改"。
// 第二个问题比第一个难：能读到地址不等于知道该由谁去改，能改也不等于改得回来。
//
// 三条贯穿全包的口径：
//
//   - **查不到不等于没有**。`ip` 不在、`/proc` 读不动、netplan 目录不存在，都必须在
//     结果里说清是哪一节的哪一步失败，而不是回一个空数组让人读成"这台机器没有网卡"。
//   - **不猜**。网卡是不是网桥、这台机器的地址由谁渲染，都从内核与文件系统的事实推
//     （`/sys/class/net/*/bridge`、`/run/systemd/network/*.network`），不从名字猜、不从
//     进程名猜。推不出来的写 `unknown`，不写成默认值。
//   - **只读也不许慢**。每一次采集都有上限，`ip` 跑不动就如实报告"读不到"，不无限等。
package network

// 后端（谁在渲染网络配置）的闭集。
//
// 这不是"装了什么软件"的清单，而是"这份配置由谁负责"的判定：P14.2 的写路径只能在
// netplan + systemd-networkd 这一种组合上打开，其余一律只读。
const (
	// BackendNetplanNetworkd：netplan 是配置源，渲染给 systemd-networkd。
	BackendNetplanNetworkd = "netplan-networkd"
	// BackendNetplanNetworkManager：netplan 是配置源，但渲染给 NetworkManager。
	BackendNetplanNetworkManager = "netplan-networkmanager"
	// BackendNetworkManager：没有 netplan 配置，NetworkManager 自己管。
	BackendNetworkManager = "networkmanager"
	// BackendUnknown：认不出来。认不出来就只能只读，不许"试试看"。
	BackendUnknown = "unknown"
)

// 网卡类型的闭集。判定依据全部来自 /sys/class/net/<name>/ 下的目录与属性。
const (
	KindLoopback = "loopback"
	KindEthernet = "ethernet"
	KindBridge   = "bridge"
	KindBond     = "bond"
	KindVLAN     = "vlan"
	KindWifi     = "wifi"
	KindTun      = "tun"
	KindOther    = "other"
)

// 地址家族的闭集。只认这两种：P14 的写路径只处理 IPv4，IPv6 先只读展示。
const (
	FamilyIPv4 = "ipv4"
	FamilyIPv6 = "ipv6"
)

// Issue 是一次"没读到"的如实记录。
//
// 它存在的理由很实际：把"读不到"折成一个空数组，页面上就是"这台机器没有监听端口"，
// 管理员会据此去查为什么服务没起来——而真相是 `ss`/`/proc` 没读动。所以每一节都要能
// 说出自己缺了哪块，缺的原因用闭集 code，detail 里是给人看的那句话。
type Issue struct {
	// Source 是来源标识：`sysfs`、`ip`、`proc-net`、`netplan`、`ufw`、`systemd`。
	Source string `json:"source"`
	// Code 是闭集的原因码，见下面 Issue* 常量。
	Code string `json:"code"`
	// Detail 是人类可读的一句话，可以为空。
	Detail string `json:"detail,omitempty"`
}

// 原因码闭集。新增要改代码，不接受配置或调用方自定义。
const (
	// IssueMissing：来源本身不存在（`ip` 不在 PATH、netplan 目录不存在）。
	IssueMissing = "missing"
	// IssuePermission：来源在，但没有权限读。
	IssuePermission = "permission_denied"
	// IssueTimeout：读取超时。
	IssueTimeout = "timeout"
	// IssueUnparsable：读到了，但格式不认识（`ip -j` 的输出不是预期形状）。
	IssueUnparsable = "unparsable"
	// IssueFailed：其他失败，detail 里有原始错误。
	IssueFailed = "failed"
)

// Address 是一张网卡上的一个地址。
type Address struct {
	Family string `json:"family"`
	// Address 不带前缀长度，前缀长度在 PrefixLen 里（`192.168.1.10` + `24`）。
	Address   string `json:"address"`
	PrefixLen int    `json:"prefix_len"`
	// Scope 是内核的口径：`global`、`link`、`host`。回环地址是 host，v6 链路本地是 link。
	Scope string `json:"scope"`
	// Type 是 `ip` 报告的类型，如 `primary`/`secondary`；读不到时为空。
	Type string `json:"type,omitempty"`
}

// Interface 是一张网卡。
type Interface struct {
	Name  string `json:"name"`
	Index int    `json:"index"`
	Kind  string `json:"kind"`
	MAC   string `json:"mac,omitempty"`
	MTU   int    `json:"mtu"`
	// State 是内核的 operstate：`up`/`down`/`unknown`/`lowerlayerdown` 等，原样透传。
	State string `json:"state"`
	// Carrier 是物理链路：true 有载波、false 没有、nil 读不到（虚拟网卡常常读不到）。
	Carrier *bool `json:"carrier"`
	// Master 是它挂在哪个主设备下（网桥或 bond 的名字）；空表示不挂。
	Master string `json:"master,omitempty"`
	// Physical 表示这张网卡落在真实硬件上（/sys/class/net/<name>/device 存在）。
	// P14.2 只允许改唯一的物理网卡，所以这个判定要摆在数据里，不能留给页面去猜。
	Physical bool `json:"physical"`
	// Addresses 是这张网卡上的地址；读不到地址时为空数组且 issues 里有记录。
	Addresses []Address `json:"addresses"`
}

// InterfaceList 是 GET /network/interfaces 的返回体。
type InterfaceList struct {
	// CollectedAt 是这次采集的时刻（RFC3339，UTC）。
	CollectedAt string      `json:"collected_at"`
	Interfaces  []Interface `json:"interfaces"`
	// Capability 说明"将来能不能改"，P14.1 只读、但判定现在就要给出来。
	Capability Capability `json:"capability"`
	Issues     []Issue    `json:"issues"`
}

// Capability 是"这份配置能不能被本面板安全地改"的判定结果。
//
// 只读阶段就把话说清楚，是因为"改不了"和"不知道能不能改"在界面上必须长得不一样：
// 前者可以灰掉按钮并给出原因，后者只能让人等着——而 P14.2 的按钮一旦出现，人就默认
// 它点了会生效。
type Capability struct {
	// Backend 是判定出来的配置归属，闭集见 Backend* 常量。
	Backend string `json:"backend"`
	// Editable 表示 P14.2 的写路径能不能在这台机器上打开。
	Editable bool `json:"editable"`
	// Target 是可以改的那张网卡（Editable 为 true 时必有）；名字为空表示没有。
	Target string `json:"target,omitempty"`
	// Reasons 是 Editable 为 false 的原因，每条都是给人看的一句话；Editable 为 true 时为空。
	Reasons []string `json:"reasons"`
}

// Route 是一条路由。
type Route struct {
	Family string `json:"family"`
	// Destination 是目标网段（`0.0.0.0/0`、`192.168.1.0/24`），default 也写成网段。
	Destination string `json:"destination"`
	// Gateway 是下一跳；直连路由没有下一跳，为空。
	Gateway string `json:"gateway,omitempty"`
	// Device 是出接口名。
	Device string `json:"device,omitempty"`
	// Metric 是优先级，越小越先；读不到时为 0 且 PrefSet 为 false。
	Metric int `json:"metric"`
	// PrefSet 表示 Metric 是不是真的读到了——0 也是合法优先级，不能拿 0 当"没有"。
	PrefSet bool `json:"pref_set"`
	// Protocol 是内核里的来源（`static`/`kernel`/`dhcp`/`ra` 等），原样透传。
	Protocol string `json:"protocol,omitempty"`
	// Scope 是路由作用域：`global`/`link`/`host`。
	Scope string `json:"scope,omitempty"`
	// Default 是"这是默认路由"的便捷标记，等于 Destination 是 0.0.0.0/0 或 ::/0。
	Default bool `json:"default"`
}

// RouteList 是 GET /network/routes 的返回体。
type RouteList struct {
	CollectedAt string  `json:"collected_at"`
	Routes      []Route `json:"routes"`
	// Gateways 是去重后的默认网关，按家族分开——页面顶部要显示"这台机器从哪儿出去"。
	Gateways []Gateway `json:"gateways"`
	Issues   []Issue   `json:"issues"`
}

// Gateway 是默认路由上的下一跳。
type Gateway struct {
	Family  string `json:"family"`
	Address string `json:"address"`
	Device  string `json:"device,omitempty"`
	Metric  int    `json:"metric"`
}

// Listener 是一个监听中的套接字。
type Listener struct {
	Family string `json:"family"`
	// Protocol 是 `tcp` 或 `udp`（IPv6 不单列协议，靠 Family 区分）。
	Protocol string `json:"protocol"`
	// Address 是绑定地址（`0.0.0.0`、`::`、`127.0.0.1`）。
	Address string `json:"address"`
	Port    int    `json:"port"`
	// Wildcard 表示绑在通配地址上，也就是对外可达（除非被防火墙挡住）。
	Wildcard bool `json:"wildcard"`
	// Loopback 表示只有本机能连上（回环地址或通配的语义见 discovery 包）。
	Loopback bool `json:"loopback"`
	// PID 与 Process 是占用它的进程；读不到时为 0 与空（不是"没有进程"）。
	PID     int64  `json:"pid"`
	Process string `json:"process,omitempty"`
	// Unit 是进程所属的 systemd 单元（读不到为空）。
	Unit string `json:"unit,omitempty"`
	// inode 是内部字段：反查进程用的套接字 inode，出响应前会被清零，不上线。
	inode uint64
}

// ListenerList 是 GET /network/listeners 的返回体。
type ListenerList struct {
	CollectedAt string     `json:"collected_at"`
	Listeners   []Listener `json:"listeners"`
	Issues      []Issue    `json:"issues"`
	// Truncated 表示结果被上限截断了（监听表可能有几千条，页面不该因此卡死）。
	Truncated bool `json:"truncated"`
}

// FirewallRule 是一条防火墙规则。
type FirewallRule struct {
	// Number 是 UFW 的规则序号（`ufw status numbered` 的第一列）；读不到时为 0。
	Number int `json:"number"`
	// Action 是 `ALLOW`/`DENY`/`REJECT`/`LIMIT`，原样透传（大写）。
	Action string `json:"action"`
	// Direction 是 `IN`/`OUT`/`FWD`。
	Direction string `json:"direction"`
	// Interface 是限定网卡（`Anywhere on eth0` 里的 eth0），可空。
	Interface string `json:"interface,omitempty"`
	// To 是目标（`22/tcp`、`Anywhere`）。
	To string `json:"to"`
	// From 是来源（`Anywhere`、`192.168.1.0/24`、`Anywhere (v6)`）。
	From string `json:"from"`
	// Raw 是 UFW 输出的原话，读不准的规则靠它兜底。
	Raw string `json:"raw"`
}

// FirewallState 是 GET /firewall 的返回体。
type FirewallState struct {
	CollectedAt string `json:"collected_at"`
	// Manager 是判定出来的防火墙管理者：`ufw`/`firewalld`/`nftables`/`none`/`unknown`。
	// 只有 `ufw` 允许 P14.3 的写路径，其余一律只读。
	Manager string `json:"manager"`
	// ManagerReasons 说明 Manager 判定的依据（哪条事实让它这么判）。
	ManagerReasons []string `json:"manager_reasons"`
	// Installed 表示 UFW 这个软件在不在（在但没启用，与不在是两件事）。
	Installed bool `json:"installed"`
	// Enabled 是 UFW 自己报告的启用状态；Installed 为 false 时无意义，恒为 false。
	Enabled bool `json:"enabled"`
	// DefaultIncoming/DefaultOutgoing/DefaultRouted 是默认策略，读不到为空。
	DefaultIncoming string `json:"default_incoming,omitempty"`
	DefaultOutgoing string `json:"default_outgoing,omitempty"`
	DefaultRouted   string `json:"default_routed,omitempty"`
	// Logging 是 UFW 的日志级别（`on`/`off`/`low`/`medium`/`high`/`full`）。
	Logging string         `json:"logging,omitempty"`
	Rules   []FirewallRule `json:"rules"`
	// Editable 表示 P14.3 的写路径能不能开：只有 manager=ufw 且 Installed 才可能为 true。
	Editable bool     `json:"editable"`
	Reasons  []string `json:"reasons"`
	Issues   []Issue  `json:"issues"`
}
