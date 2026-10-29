package network

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Interfaces 采集网卡与地址。
//
// 两条来源各有分工：`ip -j addr show` 给的是**内核此刻的地址**（/proc 里没有 IPv4 地址
// 的干净视图），sysfs 给的是**这张网卡是什么**（类型、物理与否、载波）。任何一条断掉都
// 不能把整份结果变成空的：断哪条就在 issues 里说哪条，另一条照常给出。
func Interfaces(ctx context.Context, deps Deps) InterfaceList {
	collectedAt := deps.now().Format(time.RFC3339)
	out := InterfaceList{
		CollectedAt: collectedAt,
		Interfaces:  []Interface{},
		Issues:      []Issue{},
	}

	fromIP, ipIssues := readIPAddrs(ctx, deps)
	out.Issues = append(out.Issues, ipIssues...)

	names := make([]string, 0, len(fromIP))
	for name := range fromIP {
		names = append(names, name)
	}
	if len(fromIP) == 0 {
		// `ip` 没给出网卡清单时，从 sysfs 自己列。这不是"退而求其次的猜测"：sysfs 目录
		// 就是内核眼里的网卡清单，只是没有地址。地址那一节会带着 ip 的失败记录一起出现。
		sysNames, err := listSysInterfaces(deps)
		if err != nil {
			out.Issues = append(out.Issues, Issue{
				Source: "sysfs", Code: issueCode(err),
				Detail: "列 /sys/class/net 失败：" + err.Error(),
			})
		}
		names = sysNames
	}
	sort.Strings(names)

	for _, name := range names {
		iface := Interface{Name: name, Addresses: []Address{}}
		if ip, ok := fromIP[name]; ok {
			iface.Index = ip.Index
			iface.MTU = ip.MTU
			iface.MAC = strings.ToLower(ip.Address)
			iface.State = strings.ToLower(ip.OperState)
			iface.Master = ip.Master
			for _, ai := range ip.AddrInfo {
				fam := FamilyIPv4
				if ai.Family == "inet6" {
					fam = FamilyIPv6
				} else if ai.Family != "inet" {
					// 未知家族（如 mpls）不硬塞进两个闭集里。
					continue
				}
				iface.Addresses = append(iface.Addresses, Address{
					Family: fam, Address: ai.Local, PrefixLen: ai.PrefixLen,
					Scope: ai.Scope, Type: ai.Kind,
				})
			}
		}
		enrichFromSysfs(deps, &iface, &out.Issues)
		out.Interfaces = append(out.Interfaces, iface)
	}

	out.Capability = detectCapability(deps, out.Interfaces, &out.Issues)
	return out
}

// ---- sysfs ----

func listSysInterfaces(deps Deps) ([]string, error) {
	return deps.fs().ListDir(path.Join(deps.sysRoot(), "class/net"))
}

// enrichFromSysfs 把 sysfs 里的事实补进 Interface。读不到的属性留空/null，不猜。
func enrichFromSysfs(deps Deps, iface *Interface, issues *[]Issue) {
	base := path.Join(deps.sysRoot(), "class/net", iface.Name)
	fsys := deps.fs()

	if mtu, err := fsys.ReadFile(path.Join(base, "mtu")); err == nil {
		if n, conv := strconv.Atoi(strings.TrimSpace(string(mtu))); conv == nil {
			iface.MTU = n
		}
	}
	if hw, err := fsys.ReadFile(path.Join(base, "address")); err == nil {
		iface.MAC = strings.ToLower(strings.TrimSpace(string(hw)))
	}
	if carrier, err := fsys.ReadFile(path.Join(base, "carrier")); err == nil {
		v := strings.TrimSpace(string(carrier)) == "1"
		iface.Carrier = &v
	}
	// 物理网卡才在 /sys/class/net/<name>/device 下面有实体设备的链接。
	// 没有这个链接的（veth、docker0、tun）都是虚拟的——P14.2 只改物理网卡。
	if _, err := fsys.Readlink(path.Join(base, "device")); err == nil {
		iface.Physical = true
	}
	if iface.Master == "" {
		if target, err := fsys.Readlink(path.Join(base, "master")); err == nil {
			iface.Master = path.Base(target)
		}
	}
	iface.Kind = detectKind(deps, iface.Name, base)
}

// detectKind 用 /sys 与 /proc 里"这个目录存在"来判类型，不按名字猜。
//
// 顺序有讲究：网桥和 bond 底下也可能挂着物理网卡，而无线网卡在网桥里也不少见，
// 所以先判最具体的（无线、网桥、bond、vlan、tun），最后才是普通以太网。
func detectKind(deps Deps, name, base string) string {
	fsys := deps.fs()
	dirHas := func(sub string) bool {
		_, err := fsys.ListDir(path.Join(base, sub))
		return err == nil
	}
	fileHas := func(p string) bool {
		_, err := fsys.ReadFile(p)
		return err == nil
	}
	switch {
	case name == "lo" || readType(deps, base) == "772":
		return KindLoopback
	case dirHas("wireless"), dirHas("phy80211"):
		return KindWifi
	case dirHas("bridge"):
		return KindBridge
	case dirHas("bonding"):
		return KindBond
	case fileHas(path.Join(deps.procRoot(), "net/vlan", name)):
		return KindVLAN
	case fileHas(path.Join(base, "tun_flags")):
		return KindTun
	case readType(deps, base) == "1":
		return KindEthernet
	default:
		return KindOther
	}
}

func readType(deps Deps, base string) string {
	data, err := deps.fs().ReadFile(path.Join(base, "type"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// ---- ip -j addr ----

// ipAddr 是 `ip -j addr show` 的一个元素。只声明用得到的字段：多出来的字段
// （flags、link_type、qdisc…）由 encoding/json 忽略，但缺失的字段不会变成零值以外的东西。
type ipAddr struct {
	Index     int          `json:"ifindex"`
	Name      string       `json:"ifname"`
	MTU       int          `json:"mtu"`
	OperState string       `json:"operstate"`
	Address   string       `json:"address"`
	Master    string       `json:"master"`
	AddrInfo  []ipAddrInfo `json:"addr_info"`
}

type ipAddrInfo struct {
	Family    string `json:"family"`
	Local     string `json:"local"`
	PrefixLen int    `json:"prefixlen"`
	Scope     string `json:"scope"`
	Kind      string `json:"type"`
}

func readIPAddrs(ctx context.Context, deps Deps) (map[string]ipAddr, []Issue) {
	out := map[string]ipAddr{}
	if deps.Runner == nil {
		return out, []Issue{{Source: "ip", Code: IssueMissing, Detail: "没有配置命令执行端"}}
	}
	ctx, cancel := context.WithTimeout(ctx, deps.timeout())
	defer cancel()
	stdout, code, err := deps.Runner.Run(ctx, "ip", "-j", "addr", "show")
	if err != nil {
		return out, []Issue{{Source: "ip", Code: issueCode(err), Detail: "跑 ip 失败：" + err.Error()}}
	}
	if code != 0 {
		return out, []Issue{{Source: "ip", Code: IssueFailed,
			Detail: fmt.Sprintf("ip -j addr show 退出码 %d", code)}}
	}
	var list []ipAddr
	if err := json.Unmarshal(stdout, &list); err != nil {
		return out, []Issue{{Source: "ip", Code: IssueUnparsable, Detail: "ip 的输出不是预期 JSON：" + err.Error()}}
	}
	for _, item := range list {
		if item.Name != "" {
			out[item.Name] = item
		}
	}
	return out, nil
}
