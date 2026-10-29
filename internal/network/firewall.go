package network

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 防火墙管理者的闭集。
const (
	// ManagerUFW：UFW 在管。只有这一种允许 P14.3 打开写路径。
	ManagerUFW = "ufw"
	// ManagerFirewalld：firewalld 在。UFW 的写路径必须关掉——两套规则同存时，
	// "我加了一条放行"在另一套的默认拒绝面前什么都不是。
	ManagerFirewalld = "firewalld"
	// ManagerNftables：只有 nftables 的静态规则文件，没有能安全表达"加一条规则"的管理器。
	ManagerNftables = "nftables"
	// ManagerIptablesPersistent：iptables-persistent 之类的直写规则文件。
	ManagerIptablesPersistent = "iptables-persistent"
	// ManagerNone：一种都没发现，机器上裸着。
	ManagerNone = "none"
	// ManagerUnknown：看到了互相矛盾的迹象，判不出来。
	ManagerUnknown = "unknown"
)

// Firewall 采集防火墙现状。**只读**：只跑 `ufw status`，不改规则、不 reload。
func Firewall(ctx context.Context, deps Deps) FirewallState {
	out := FirewallState{
		CollectedAt:    deps.now().Format(time.RFC3339),
		ManagerReasons: []string{},
		Rules:          []FirewallRule{},
		Reasons:        []string{},
		Issues:         []Issue{},
	}

	ufwInstalled := programPresent(deps, "ufw")
	other := detectOtherManagers(deps)

	switch {
	case ufwInstalled && len(other) == 0:
		out.Manager = ManagerUFW
		out.ManagerReasons = append(out.ManagerReasons, "发现 ufw，且没有别的防火墙管理器在")
	case ufwInstalled:
		out.Manager = ManagerUnknown
		out.ManagerReasons = append(out.ManagerReasons,
			"ufw 在，同时还发现了 "+strings.Join(other, "、")+"：两套规则同时存在时判不出谁在生效")
	default:
		switch {
		case len(other) == 1:
			out.Manager = other[0]
			out.ManagerReasons = append(out.ManagerReasons, "没有 ufw，发现 "+other[0])
		case len(other) > 1:
			out.Manager = ManagerUnknown
			out.ManagerReasons = append(out.ManagerReasons,
				"没有 ufw，但发现了 "+strings.Join(other, "、")+"：判不出谁在生效")
		default:
			out.Manager = ManagerNone
			out.ManagerReasons = append(out.ManagerReasons, "没有 ufw，也没有别的防火墙管理器")
		}
	}
	out.Installed = ufwInstalled

	if ufwInstalled {
		parseUFW(ctx, deps, &out)
	} else {
		out.Issues = append(out.Issues, Issue{
			Source: "ufw", Code: IssueMissing,
			Detail: "本机没有 ufw：防火墙状态读不到，不等于这台机器没有防火墙",
		})
	}

	// 能不能改：只有 UFW 独占时才可能。别的管理器在场一律只读，这是 PLAN §19 的原文。
	if out.Manager != ManagerUFW {
		out.Reasons = append(out.Reasons, "防火墙由 "+out.Manager+" 管理（或判不出来）：P14.3 只对 UFW 打开写路径")
	}
	out.Editable = out.Manager == ManagerUFW && out.Installed && len(out.Reasons) == 0
	return out
}

// programPresent 判断一个程序在不在。不用 exec.LookPath，是因为 PATH 里可能有
// /usr/local 之类可写目录；这里只认系统目录下的固定位置。
func programPresent(deps Deps, name string) bool {
	for _, dir := range []string{"/usr/sbin", "/sbin", "/usr/bin", "/bin"} {
		names, err := deps.fs().ListDir(dir)
		if err != nil {
			continue
		}
		for _, candidate := range names {
			if candidate == name {
				return true
			}
		}
	}
	return false
}

// detectOtherManagers 找 UFW 之外的防火墙管理者。返回值是闭集常量，已排序去重。
func detectOtherManagers(deps Deps) []string {
	fsys := deps.fs()
	found := map[string]bool{}
	dirHas := func(p string) bool {
		_, err := fsys.ListDir(p)
		return err == nil
	}
	fileHas := func(p string) bool {
		_, err := fsys.ReadFile(p)
		return err == nil
	}

	if programPresent(deps, "firewall-cmd") || programPresent(deps, "firewalld") ||
		dirHas(path.Join(deps.runDir(), "firewalld")) {
		found[ManagerFirewalld] = true
	}
	if nftablesManaged(deps) && !found[ManagerFirewalld] {
		found[ManagerNftables] = true
	}
	if fileHas("/etc/iptables/rules.v4") || fileHas("/etc/iptables/rules.v6") {
		found[ManagerIptablesPersistent] = true
	}

	out := make([]string, 0, len(found))
	for manager := range found {
		out = append(out, manager)
	}
	sort.Strings(out)
	return out
}

// nftablesManaged 判断 nftables 是不是真的在管这台机器的规则集。
//
// 只看 `/etc/nftables.conf` 在不在会把**每一台 Ubuntu**都判成"nftables 在管"：这个文件
// 是 nftables 包自带的默认规则集，装包就有一份，而 `nftables.service` 默认是 disabled。
// 22.04 验收机上就是这么红出来的——ufw 独占，判定却说"ufw 与 nftables 同存"，于是 P14.3
// 的写路径在一台最标准的机器上永远打不开。
//
// 所以判据落到"它会不会在开机时把这份规则装进内核"：`systemctl enable` 会在
// `/etc/systemd/system/<target>.wants/` 下留一个 `nftables.service` 符号链接。
// 已知限制：手工 `nft -f` 载入规则、但没 enable 服务的机器判不出来——那种情况下内核里
// 确实另有规则，而面板只能看到"没有开机加载的证据"。这条写进 CONTRACTS §13.5。
func nftablesManaged(deps Deps) bool {
	fsys := deps.fs()
	if _, err := fsys.ReadFile("/etc/nftables.conf"); err != nil {
		return false
	}
	names, err := fsys.ListDir("/etc/systemd/system")
	if err != nil {
		return false
	}
	for _, name := range names {
		if !strings.HasSuffix(name, ".wants") {
			continue
		}
		links, err := fsys.ListDir(path.Join("/etc/systemd/system", name))
		if err != nil {
			continue
		}
		for _, link := range links {
			if link == "nftables.service" {
				return true
			}
		}
	}
	return false
}

// ruleLine 匹配 `ufw status numbered` 里的一行规则：`[ 1] 22/tcp  ALLOW IN  Anywhere`。
var ruleLine = regexp.MustCompile(`^\[\s*(\d+)\]\s+(.*)$`)

// columnSplit 按两列以上的空白切分表格列。UFW 的输出是靠空格对齐的表格，
// 单个空格会出现在 `Anywhere on eth0` 这样的值里，所以只能用"连续空白"当分隔。
var columnSplit = regexp.MustCompile(`\s{2,}`)

func parseUFW(ctx context.Context, deps Deps, out *FirewallState) {
	if deps.Runner == nil {
		out.Issues = append(out.Issues, Issue{Source: "ufw", Code: IssueMissing, Detail: "没有配置命令执行端"})
		return
	}
	runCtx, cancel := context.WithTimeout(ctx, deps.timeout())
	defer cancel()
	stdout, code, err := deps.Runner.Run(runCtx, "ufw", "status", "verbose", "numbered")
	if err != nil {
		out.Issues = append(out.Issues, Issue{Source: "ufw", Code: issueCode(err),
			Detail: "跑 ufw status 失败：" + err.Error()})
		return
	}
	if code != 0 {
		out.Issues = append(out.Issues, Issue{Source: "ufw", Code: IssueFailed,
			Detail: fmt.Sprintf("ufw status 退出码 %d（多半是没有权限读：它需要 root）", code)})
		return
	}
	parseUFWOutput(string(stdout), out)
}

// parseUFWOutput 解析 `ufw status verbose numbered` 的正文。
//
// 认得的行只有三种：
//
//	Status: active
//	Logging: on (low)
//	Default: deny (incoming), allow (outgoing), disabled (routed)
//
// 其余非空行若不是规则行（表头、分隔线、`New profiles` 提示），跳过但不报错——UFW 的
// 版本之间表头措辞变过，把表头当错误会让整个解析在升级后失效。
func parseUFWOutput(text string, out *FirewallState) {
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimRight(raw, " \t\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		switch {
		case strings.HasPrefix(trimmed, "Status:"):
			out.Enabled = strings.EqualFold(strings.TrimSpace(strings.TrimPrefix(trimmed, "Status:")), "active")
		case strings.HasPrefix(trimmed, "Logging:"):
			out.Logging = strings.TrimSpace(strings.TrimPrefix(trimmed, "Logging:"))
		case strings.HasPrefix(trimmed, "Default:"):
			parseUFWDefaults(strings.TrimSpace(strings.TrimPrefix(trimmed, "Default:")), out)
		default:
			rule, ok := parseUFWRule(trimmed)
			if !ok {
				continue
			}
			out.Rules = append(out.Rules, rule)
		}
	}
}

// parseUFWDefaults 解析 `deny (incoming), allow (outgoing), disabled (routed)`。
func parseUFWDefaults(spec string, out *FirewallState) {
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		policy, rest, ok := strings.Cut(part, " ")
		if !ok {
			continue
		}
		switch strings.Trim(rest, "()") {
		case "incoming":
			out.DefaultIncoming = policy
		case "outgoing":
			out.DefaultOutgoing = policy
		case "routed":
			out.DefaultRouted = policy
		}
	}
}

// parseUFWRule 解析一行规则。读不准的字段留空，Raw 里保留原话。
func parseUFWRule(line string) (FirewallRule, bool) {
	match := ruleLine.FindStringSubmatch(line)
	if match == nil {
		return FirewallRule{}, false
	}
	number, err := strconv.Atoi(match[1])
	if err != nil || number <= 0 {
		return FirewallRule{}, false
	}
	rule := FirewallRule{Number: number, Raw: line}
	body := match[2]
	cols := columnSplit.Split(body, -1)
	if len(cols) >= 3 {
		rule.To = strings.TrimSpace(cols[0])
		action := strings.Fields(strings.TrimSpace(cols[1]))
		if len(action) > 0 {
			rule.Action = strings.ToUpper(action[0])
		}
		if len(action) > 1 {
			rule.Direction = strings.ToUpper(action[1])
		}
		rule.From = strings.TrimSpace(strings.Join(cols[2:], "  "))
	}
	// `Anywhere on eth0 ALLOW IN Anywhere` 这种把网卡写在 To 列里的形式。
	if on := strings.Index(rule.To, " on "); on >= 0 {
		rule.Interface = strings.TrimSpace(rule.To[on+4:])
		rule.To = strings.TrimSpace(rule.To[:on])
	}
	if rule.From == "" {
		rule.From = body
	}
	return rule, true
}
