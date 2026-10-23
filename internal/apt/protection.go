package apt

// 包的保护名单（P11.2，PLAN.md 第 14 节 / STAGES §13 的验收条款）。
//
// 卸载这一类动作最容易出的不是"命令失败"，是"命令成功但机器回不来了"：
// 删掉 openssh-server，当前会话照旧跑，直到你断开才发现再也连不上；删掉
// netplan.io，重启之后没有网络；删掉 ca-certificates，面板的 HTTPS 与证书
// 模块一起断。这些都不会在 `apt-get remove` 的退出码里体现。
//
// 所以这张表**只能通过改代码扩充**：不接受配置、不接受 API 写入、不接受临时豁免，
// 与 services/protection.go 是同一个道理。命中的包出现在模拟的删除列表里就拦下，
// 并点名"是谁、为什么"。
//
// 刻意保留的边界：
//   - 这里只判"apt 自己要删什么"。别的东西（比如 systemd 单元、卷依赖）各有各的门禁。
//   - 不用前缀猜（`lib*` 之类）：猜出来的保护会让正常的库升级也过不去。

import (
	"sort"
	"strings"
)

// 保护类别（预检里展示的分类）。
const (
	// ProtectEssential：dpkg 里标了 `Essential: yes` 的包。删掉它们 dpkg 自己都不干。
	ProtectEssential = "essential"
	// ProtectPanel：面板自身依赖的软件（证书签发、反向代理、TLS 信任锚）。
	// 面板本体是静态二进制，不是 dpkg 包；这里护的是它**调用的**那些工具。
	ProtectPanel = "panel"
	// ProtectSSH：远程登录。
	ProtectSSH = "ssh"
	// ProtectNetwork：网络配置与链路。
	ProtectNetwork = "network"
	// ProtectBoot：启动链与正在运行的内核。
	ProtectBoot = "boot"
	// ProtectCore：init、包管理器与基础运行时。
	ProtectCore = "core"
)

// protectionRule 是一条固定保护规则。
type protectionRule struct {
	category string
	reason   string
	// exact 是精确匹配的**二进制包名**（不带 :arch 限定）。
	exact map[string]bool
	// prefixes 是按前缀匹配的包名（只给 grub / linux-image 这类成族的用）。
	prefixes []string
}

// protectionRules 是固定名单。每一条都写清"删了会怎样"。
var protectionRules = []protectionRule{
	{
		category: ProtectPanel,
		reason:   "面板的证书签发依赖它：删掉之后 HTTPS 与证书续期全部失效",
		exact:    map[string]bool{"certbot": true},
	},
	{
		category: ProtectPanel,
		reason:   "面板的反向代理模块通过它工作：删掉之后所有站点立刻不可用",
		exact:    map[string]bool{"nginx": true, "nginx-core": true, "nginx-common": true},
	},
	{
		category: ProtectPanel,
		reason:   "TLS 信任锚：删掉之后对外请求与证书校验一起失效",
		exact:    map[string]bool{"ca-certificates": true},
	},
	{
		category: ProtectSSH,
		reason:   "SSH 远程登录：删掉之后当前会话照旧，断开就再也连不上",
		exact:    map[string]bool{"openssh-server": true, "openssh-client": true, "ssh": true},
	},
	{
		category: ProtectNetwork,
		reason:   "网络配置：删掉之后重启即失去网络（当前链路看起来还是通的）",
		exact: map[string]bool{
			"netplan.io": true, "netplan-generator": true, "network-manager": true,
			"systemd-networkd": true, "ifupdown": true, "wpasupplicant": true,
			"iproute2": true,
		},
	},
	{
		category: ProtectBoot,
		reason:   "启动链：删掉之后这台机器可能起不来",
		exact:    map[string]bool{"shim-signed": true, "grub-efi-amd64-signed": true},
		// 前缀只给"整族都属于启动链"的名字。
		//
		// 刻意**不**用裸 `grub`：`grub-customizer` 是个图形配置工具，删掉它不会让
		// 机器起不来，而被拦下的人只会得到一个错误的理由。过度的保护同样是缺陷。
		prefixes: []string{"grub-common", "grub-efi", "grub-pc", "grub2-", "linux-image", "linux-modules", "linux-generic", "linux-virtual"},
	},
	{
		category: ProtectCore,
		reason:   "基础运行时与包管理器：删掉之后系统与面板都失去基础能力",
		exact: map[string]bool{
			"systemd": true, "systemd-sysv": true, "init": true, "dpkg": true, "apt": true,
			"libc6": true, "libc-bin": true, "util-linux": true, "sudo": true,
			"python3": true, "python3-minimal": true,
		},
	},
}

// Protection 是一个包命中的保护结论。
type Protection struct {
	// Protected 表示这个包不能被卸载。
	Protected bool `json:"protected"`
	// Category 是命中的类别（ProtectEssential 等）。
	Category string `json:"category"`
	// Reason 是给人看的一句话。
	Reason string `json:"reason"`
}

// BaseName 去掉架构限定：`bind9-libs:amd64` → `bind9-libs`。
//
// dpkg 的多架构限定与保护名单无关：`openssh-server:amd64` 与 `openssh-server` 是
// 同一个软件，保护判定按基础名做。
func BaseName(name string) string {
	if i := strings.IndexByte(name, ':'); i > 0 {
		return name[:i]
	}
	return name
}

// Protect 判断一个包（可以是 `name:arch`）是否受保护。
//
// essential 是这台机器上证了 `Essential: yes` 的包名集合（由 Writer 现场读）。
// 它必须由调用方给：写死一份"Ubuntu 的 Essential 列表"会在下一个发行版上悄悄变旧，
// 而 dpkg 数据库里那一位是唯一权威。
func Protect(name string, essential map[string]bool) Protection {
	base := BaseName(name)
	if essential[base] {
		return Protection{
			Protected: true,
			Category:  ProtectEssential,
			Reason:    "dpkg 标记为 Essential：删掉之后系统无法正常工作",
		}
	}
	for _, rule := range protectionRules {
		if rule.exact[base] {
			return Protection{Protected: true, Category: rule.category, Reason: rule.reason}
		}
		for _, prefix := range rule.prefixes {
			if strings.HasPrefix(base, prefix) {
				return Protection{Protected: true, Category: rule.category, Reason: rule.reason}
			}
		}
	}
	return Protection{}
}

// ProtectedRemovals 返回这次模拟里"将被删除且受保护"的包名（升序）。
//
// 它只回答一个问题：apt 自己算出来的删除列表里有没有受保护的包。依赖链由 apt 的
// 解析器负责——`apt-get -s remove ca-certificates` 会顺手列出 26 个包，这件事
// 面板既不复刻也不猜，它读 apt 的结论。
func ProtectedRemovals(sim Simulation, essential map[string]bool) []string {
	var hits []string
	for _, name := range sim.Remove {
		if Protect(name, essential).Protected {
			hits = append(hits, name)
		}
	}
	sort.Strings(hits)
	return hits
}

// CollateralRemovals 返回"这次模拟会删除、但调用方没有点名要删"的包（升序）。
//
// 它们不是错误：apt 为了满足依赖可能必须删掉冲突的包。但"我选了 1 个包，它删了
// 26 个"这件事必须在预检里说清楚，并且作为告警交给管理员确认。
//
// 注意它**不能**参与权限判定：agent 的预检顺序是先定策略（PolicyFor）、再跑
// Plan，所以"要不要输入确认短语"只能由动作种类决定，不可能由模拟结果决定。
// 连带删除是告警，不是门禁；门禁是保护名单。
func CollateralRemovals(sim Simulation, requested []string) []string {
	want := make(map[string]bool, len(requested))
	for _, name := range requested {
		want[BaseName(name)] = true
	}
	var out []string
	for _, name := range sim.Remove {
		if !want[BaseName(name)] {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}
