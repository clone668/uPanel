package network

// P14.2：改一张物理网卡的 IPv4。这一份文件只放**类型与判断**，不碰系统：
// 参数怎么算合法、地址能不能用、候选文件长什么样。真正写盘与装载在 applyengine.go。
//
// 边界（CONTRACTS.md 13.6）：DHCP 或"一个静态地址 + 可选网关 + 可选 DNS"，IPv4 only。
// 其它一切——第二个地址、IPv6、桥接、bond、VLAN、按策略分流——一律 `unsupported`。
// "不支持"必须是明确答案：静默降级成"我尽力改了一点"是这一类动作最坏的失败形态，
// 因为用户以为自己改成功了。

import (
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strings"
	"time"
)

// 动作名。与 CONTRACTS.md 第 5 节的登记名逐字一致。
const (
	ActionApply    = "network.apply"
	ActionConfirm  = "network.confirm"
	ActionRollback = "network.rollback"
)

// 配置模式的闭集。
const (
	ModeDHCP   = "dhcp"
	ModeStatic = "static"
)

// RollbackWindow 是"改完到自动退回"的时间。
//
// 120 秒是 STAGES 写的验收条件（"120 秒后仍能自动恢复"），也是一个人从"页面断了"到
// "确认自己还活着"够用的时间。它不是一个可配置项：可配置的救生索等于没有救生索，
// 出事的时候没人有空去调它。
const RollbackWindow = 120 * time.Second

// 动作的状态闭集（回滚记录里的 state）。
const (
	StatePending    = "pending"
	StateConfirmed  = "confirmed"
	StateRolledBack = "rolled_back"
	StateFailed     = "failed"
)

// 阻塞项与告警项的 code 闭集。前端按 code 做分支，所以这里是闭集而不是自由文本。
const (
	// CodeUnsupported：这台机器（或这张网卡）不在可改范围内。
	CodeUnsupported = "NETWORK_UNSUPPORTED"
	// CodeConfigChanged：`expected_config_hash` 与现场不符（别人改过）。
	CodeConfigChanged = "NETWORK_CONFIG_CHANGED"
	// CodeTargetChanged：网卡没了，或者它已经不是当初那张（永久 MAC 变了）。
	CodeTargetChanged = "NETWORK_TARGET_CHANGED"
	// CodeDefinitionInvalid：定义越出闭集。
	CodeDefinitionInvalid = "NETWORK_DEFINITION_INVALID"
	// CodeRollbackPending：已有一条未确认的回滚记录，不许叠加。
	CodeRollbackPending = "NETWORK_ROLLBACK_PENDING"
	// CodeManagementPath：面板的访问路径就在这张网卡上（告警，不拦）。
	CodeManagementPath = "NETWORK_MANAGEMENT_PATH"
	// CodeNoCarrier：这张网卡现在没有载波（告警，不拦）。
	CodeNoCarrier = "NETWORK_NO_CARRIER"
	// CodeConfirmWrongAddress：确认不是从候选地址来的。
	CodeConfirmWrongAddress = "NETWORK_CONFIRM_WRONG_ADDRESS"
	// CodeRollbackExpired：确认来得太晚，窗口已经过去了。
	CodeRollbackExpired = "NETWORK_ROLLBACK_EXPIRED"
	// CodeNoPendingRollback：没有待确认的记录（已经确认过、已经回滚、或根本没改过）。
	CodeNoPendingRollback = "NETWORK_NO_PENDING_ROLLBACK"
)

// Notice 是预检里的一条阻塞项或告警项。
type Notice struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Impact 是预检里的一条影响说明。
type Impact struct {
	Kind        string `json:"kind"`
	Description string `json:"description"`
}

// IPv4Setting 是静态配置。
type IPv4Setting struct {
	// Address 不带前缀长度（前缀在 PrefixLength 里），与 P14.1 的 Address 形状一致。
	Address      string `json:"address"`
	PrefixLength int    `json:"prefix_length"`
	// Gateway 可空：先配地址、路由另说，是合法的中间状态。
	Gateway string `json:"gateway"`
	// DNS 0—3 个。超过三个不是"更多信息"，是配置里开始出现人也不知道为什么在那的条目。
	DNS []string `json:"dns"`
}

// ApplyDefinition 是"要改成什么样"。
type ApplyDefinition struct {
	Mode string       `json:"mode"`
	IPv4 *IPv4Setting `json:"ipv4"`
}

// ApplyParams 是 network.apply 的参数（CONTRACTS.md 5 + 13.6）。
type ApplyParams struct {
	InterfaceID string `json:"interface_id"`
	// ExpectedConfigHash 是预检时看到的现场指纹；不一致就拒绝，不接受"差不多"。
	ExpectedConfigHash string           `json:"expected_config_hash"`
	Definition         *ApplyDefinition `json:"definition"`
}

// ConfirmParams 是 network.confirm 的参数。
type ConfirmParams struct {
	InterfaceID string `json:"interface_id"`
	Token       string `json:"token"`
	// ArrivedOn 是这次请求**到达的本地地址**，由 Web 从 socket 取（不是前端填的）。
	// 它的作用是把"从候选地址确认"这件事变成每次请求都重新发生的现场事实。
	ArrivedOn string `json:"arrived_on"`
}

// RollbackParams 是 network.rollback 的参数。
type RollbackParams struct {
	InterfaceID string `json:"interface_id"`
	Token       string `json:"token"`
}

// Assessment 是预检判决。
type Assessment struct {
	// Normalized 是规范化之后的参数（提交时不再接受新参数，只用这一份）。
	Normalized          any
	DisplayName         string
	IdentityFingerprint string
	StateVersion        string
	Allowed             bool
	Blockers            []Notice
	Warnings            []Notice
	Impacts             []Impact
	Locks               []string
}

// NetworkLockName 是这张机器上所有网络写动作共用的资源锁名。
//
// 一把锁而不是"每个网卡一把"：改地址和改防火墙都会让人失去面板，它们之间没有并发的
// 理由；而"两张网卡同时改"在 1.0 里根本不存在（只有一张可改）。
const NetworkLockName = "network-global"

// 网卡名的允许形状。内核允许的字符比这多，但面板只改"ens4"这类普通名字：
// 放宽到完整的 ifname 语法（含点、冒号、@）只会让 netplan 的渲染更难讲清。
var ifaceNamePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]{0,14}$`)

// ValidInterfaceName 判断网卡名是否在面板愿意处理的形状里。
func ValidInterfaceName(name string) bool { return ifaceNamePattern.MatchString(name) }

// 面板不接受的目标网段（见 CONTRACTS 13.6）。这些地址写进 netplan 不会让机器变得可用，
// 只会让它以更隐蔽的方式坏掉。
var forbiddenIPv4Prefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
}

// ValidateDefinition 校验定义，并把可以归一的部分归一（去空白、DNS 去重）。
//
// 它同时被动作登记表（解码时）与判决层调用：两处必须给出同一份结论，否则"预检能过、
// 执行时说参数不合法"这种事迟早会发生。
func ValidateDefinition(def *ApplyDefinition) (*ApplyDefinition, error) {
	if def == nil {
		return nil, fmt.Errorf("%w: definition 不能为空", ErrDefinitionInvalid)
	}
	out := &ApplyDefinition{Mode: strings.ToLower(strings.TrimSpace(def.Mode))}
	switch out.Mode {
	case ModeDHCP:
		if def.IPv4 != nil {
			return nil, fmt.Errorf("%w: mode=dhcp 时 ipv4 必须是 null（不是空对象）", ErrDefinitionInvalid)
		}
	case ModeStatic:
		if def.IPv4 == nil {
			return nil, fmt.Errorf("%w: mode=static 时必须给 ipv4", ErrDefinitionInvalid)
		}
		v4, err := validateIPv4(*def.IPv4)
		if err != nil {
			return nil, err
		}
		out.IPv4 = &v4
	default:
		return nil, fmt.Errorf("%w: mode 只有 dhcp 与 static 两种，收到 %q", ErrDefinitionInvalid, def.Mode)
	}
	return out, nil
}

// ErrDefinitionInvalid 是"定义不在闭集里"的哨兵错误。
var ErrDefinitionInvalid = errors.New("network: 定义不合法")

func validateIPv4(in IPv4Setting) (IPv4Setting, error) {
	out := IPv4Setting{Address: strings.TrimSpace(in.Address), PrefixLength: in.PrefixLength,
		Gateway: strings.TrimSpace(in.Gateway)}
	addr, err := netip.ParseAddr(out.Address)
	if err != nil || !addr.Is4() {
		return IPv4Setting{}, fmt.Errorf("%w: ipv4.address 必须是点分十进制 IPv4，收到 %q",
			ErrDefinitionInvalid, out.Address)
	}
	if out.PrefixLength < 1 || out.PrefixLength > 32 {
		return IPv4Setting{}, fmt.Errorf("%w: ipv4.prefix_length 必须在 1—32 之间，收到 %d",
			ErrDefinitionInvalid, out.PrefixLength)
	}
	for _, bad := range forbiddenIPv4Prefixes {
		if bad.Contains(addr) {
			return IPv4Setting{}, fmt.Errorf("%w: ipv4.address %s 属于 %s，面板不接受",
				ErrDefinitionInvalid, addr, bad)
		}
	}
	if addr == netip.AddrFrom4([4]byte{255, 255, 255, 255}) {
		return IPv4Setting{}, fmt.Errorf("%w: 广播地址不能当本机地址", ErrDefinitionInvalid)
	}
	prefix := netip.PrefixFrom(addr, out.PrefixLength)
	if prefix.Masked().Addr() == addr && out.PrefixLength < 31 {
		return IPv4Setting{}, fmt.Errorf(
			"%w: %s/%d 是网段地址（主机位全 0），不能配给网卡", ErrDefinitionInvalid, addr, out.PrefixLength)
	}

	if out.Gateway != "" {
		gw, gerr := netip.ParseAddr(out.Gateway)
		if gerr != nil || !gw.Is4() {
			return IPv4Setting{}, fmt.Errorf("%w: ipv4.gateway 必须是点分十进制 IPv4，收到 %q",
				ErrDefinitionInvalid, out.Gateway)
		}
		if out.PrefixLength == 32 {
			return IPv4Setting{}, fmt.Errorf(
				"%w: /32 没有同网段，不能带网关（要默认路由请用 /31 或更小的前缀）", ErrDefinitionInvalid)
		}
		if !prefix.Contains(gw) {
			return IPv4Setting{}, fmt.Errorf("%w: 网关 %s 不在 %s 网段内",
				ErrDefinitionInvalid, gw, prefix.Masked())
		}
		if gw == addr {
			return IPv4Setting{}, fmt.Errorf("%w: 网关不能等于本机地址", ErrDefinitionInvalid)
		}
	}

	if len(in.DNS) > 3 {
		return IPv4Setting{}, fmt.Errorf("%w: dns 最多 3 个，收到 %d 个", ErrDefinitionInvalid, len(in.DNS))
	}
	seen := map[string]bool{}
	for _, raw := range in.DNS {
		s := strings.TrimSpace(raw)
		ip, derr := netip.ParseAddr(s)
		if derr != nil || !ip.Is4() {
			return IPv4Setting{}, fmt.Errorf("%w: dns 里的 %q 不是 IPv4 地址", ErrDefinitionInvalid, raw)
		}
		if seen[ip.String()] {
			continue
		}
		seen[ip.String()] = true
		out.DNS = append(out.DNS, ip.String())
	}
	return out, nil
}

// normalizeArrivedOn 把"这次请求到达的本地地址"归一成纯 IP 字符串。
//
// 它容忍 `1.2.3.4:8443`、`[::1]:8443` 与裸地址三种写法：Web 侧拿到的形状取决于
// 监听方式（IPv4/IPv6、带不带方括号），而这里只需要地址本身。
func normalizeArrivedOn(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return ap.Addr().String()
	}
	if addr, err := netip.ParseAddr(strings.Trim(s, "[]")); err == nil {
		return addr.String()
	}
	return s
}

// renderPanelNetplan 渲染面板那份 netplan 文件。
//
// 手写而不是 yaml.Marshal：这份文件的每一行都会被人在故障现场读，格式固定、注释在
// 有用处的地方，比"库认为合适的排版"更值得。所有会进模板的值都已经过校验（IPv4 字面量
// 与形状受限的网卡名），所以这里不需要再做转义——这一点由 validateIPv4 与
// ValidInterfaceName 共同保证，并且有单测钉住。
func renderPanelNetplan(iface string, def ApplyDefinition) []byte {
	var b strings.Builder
	b.WriteString("# 由 uPanel 生成（P14.2）。下一次 apply 会整体覆盖本文件。\n")
	b.WriteString("# 手工修改不会保留；要交回自管，请删除本文件后自行 netplan apply。\n")
	b.WriteString("network:\n  version: 2\n  ethernets:\n")
	fmt.Fprintf(&b, "    %s:\n", iface)
	if def.Mode == ModeDHCP {
		b.WriteString("      dhcp4: true\n")
		return []byte(b.String())
	}
	b.WriteString("      dhcp4: false\n")
	if def.IPv4 != nil {
		fmt.Fprintf(&b, "      addresses:\n        - %s/%d\n", def.IPv4.Address, def.IPv4.PrefixLength)
		if def.IPv4.Gateway != "" {
			b.WriteString("      routes:\n        - to: default\n")
			fmt.Fprintf(&b, "          via: %s\n", def.IPv4.Gateway)
		}
		if len(def.IPv4.DNS) > 0 {
			b.WriteString("      nameservers:\n        addresses:\n")
			for _, d := range def.IPv4.DNS {
				fmt.Fprintf(&b, "          - %s\n", d)
			}
		}
	}
	return []byte(b.String())
}

// describeDefinition 把定义说成一句人话，用于影响说明与确认短语。
func describeDefinition(iface string, def ApplyDefinition) string {
	if def.Mode == ModeDHCP {
		return iface + "：IPv4 改由 DHCP 分配"
	}
	if def.IPv4 == nil {
		return iface + "：IPv4 静态配置"
	}
	s := fmt.Sprintf("%s：静态 %s/%d", iface, def.IPv4.Address, def.IPv4.PrefixLength)
	if def.IPv4.Gateway != "" {
		s += "，网关 " + def.IPv4.Gateway
	}
	if len(def.IPv4.DNS) > 0 {
		s += "，DNS " + strings.Join(def.IPv4.DNS, "/")
	}
	return s
}

// candidateAddress 返回这次改动后"应该出现在网卡上的那个地址"。
//
// dhcp 模式没有候选地址（地址由 DHCP 给），调用方据此把确认门槛降级为"网卡上有地址"。
func (d ApplyDefinition) candidateAddress() string {
	if d.Mode == ModeStatic && d.IPv4 != nil {
		return d.IPv4.Address
	}
	return ""
}
