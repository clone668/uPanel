package network

// P14.2：读懂 /etc/netplan，只为一件事——判断"能不能安全地在这张网卡上加一份面板自己的
// 配置文件"，以及在改之前把"我看到的那份配置"钉成一个哈希。
//
// 这里有意识地和 P14.1 的取舍相反。P14.1 **不解析** netplan，因为它回答的是"现在生效的是
// 什么"，而生效的是渲染产物（`/run/systemd/network/*.network`），YAML 只是输入；按 YAML
// 去说现状等于把"我以为的 netplan 语义"当成事实（DECISIONS Q432）。P14.2 问的是另一个
// 问题：**我能不能覆盖它**。这个问题只有读配置本身才能回答——netplan 的合并是按文件顺序
// 逐键覆盖的，一个我们看不懂的键（`macaddress`、`routing-policy`、`gateway4`）可能正是
// 那条链路的全部理由。所以：
//
//   - 读不懂的配置 ⇒ `unsupported`，不是"试试看"；
//   - 出现我们不认识的键 ⇒ `unsupported`，把文件名与键名原样说出来；
//   - 认得的部分只做"能不能覆盖"的判断，**不**据此描述现状（现状仍然来自 P14.1）。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/goccy/go-yaml"
)

// PanelNetplanPrefix 是面板自己那份 netplan 文件的前缀。
//
// 用 90- 是有讲究的：netplan 按文件名字典序合并，cloud-init 的是 50-，别的管理员多半也
// 用比 90 小的号；取一个靠后的号，我们的键才真的覆盖得上去，而不是"看起来写了、其实被
// 后面的文件盖掉"。
const PanelNetplanPrefix = "90-upanel-"

// panelNetplanMode 是面板那份文件的权限。netplan 对 world-readable 的配置文件会告警
// （里面有 Wi-Fi 口令的场景），0600 是不给自己找麻烦的取值。
const panelNetplanMode = 0o600

// netplan 里"提到目标网卡"时允许出现的键。
//
// 这个集合刻意小：每多一个键，就多一种"我们以为覆盖住了、其实没覆盖"的可能。
// 尤其是 `macaddress`（改写网卡 MAC）与 `routing-policy`（把流量按规则分流），
// 面板既不改也不懂，出现即拒绝。
var netplanAllowedKeys = map[string]string{
	"match":           "按 MAC 匹配网卡（cloud-init 的常规写法）",
	"set-name":        "把网卡改成固定名字",
	"dhcp4":           "IPv4 由 DHCP 决定",
	"dhcp6":           "IPv6 由 DHCPv6 决定",
	"addresses":       "地址列表",
	"routes":          "路由",
	"nameservers":     "DNS",
	"optional":        "开机时不必等这张网卡",
	"dhcp4-overrides": "DHCPv4 的客户端行为",
	"dhcp6-overrides": "DHCPv6 的客户端行为",
	"accept-ra":       "是否接受路由通告",
	"link-local":      "链路本地地址",
	"mtu":             "MTU",
}

// netplanFile 是一个配置文件的名字与内容哈希。
type netplanFile struct {
	Name   string `json:"name"`
	Sha256 string `json:"sha256"`
}

// netplanDeclaration 是"某个文件提到了目标网卡，用了这些键"。
type netplanDeclaration struct {
	File string   `json:"file"`
	Keys []string `json:"keys"`
}

// netplanView 是一次 netplan 巡检的结果。
//
// 它同时也是 `expected_config_hash` 的输入：`configFingerprint` 直接由它拼出来，
// 所以"预检时看到的"和"提交时比对的"必然是同一份东西，不存在两处各算一遍的机会。
type netplanView struct {
	Dir          string
	Interface    string
	PermanentMAC string
	// Files 是目录里全部配置文件（按名字排序），含面板自己那份。
	Files []netplanFile
	// PanelName 是面板那份文件的名字（不带目录），Present 表示它现在在不在。
	PanelName    string
	PanelPresent bool
	// PanelAnswers 是面板那份文件里目标网卡那一节的内容（解析后的原样），
	// 用于回滚与"这次到底改了什么"的对照；不在时为空。
	Declarations []netplanDeclaration
	// Unsupported 是"不能安全覆盖"的原因，人话，逐条可贴到页面上。
	Unsupported []string
}

// inspectNetplan 读目录里全部 netplan 配置并判断目标网卡能不能被覆盖。
//
// 读不到目录不算错：一台没装 netplan 的机器 `/etc/netplan` 不存在，那是 `capability`
// 该说的话（`editable=false`），不是这里要报的错。读得到但读不动（权限）才算错。
func inspectNetplan(fsys FileSystem, dir, iface, mac string) (netplanView, error) {
	view := netplanView{
		Dir:          dir,
		Interface:    iface,
		PermanentMAC: strings.ToLower(mac),
		PanelName:    PanelNetplanPrefix + iface + ".yaml",
	}
	names, err := fsys.ListDir(dir)
	if err != nil {
		return view, fmt.Errorf("列 %s 失败：%w", dir, err)
	}
	sort.Strings(names)
	for _, name := range names {
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
			// netplan 只认 .yaml（后缀 .yml 在旧版本也认）。别的东西放在这里本来
			// 也不会被读，我们也不去描述它。
			continue
		}
		data, err := fsys.ReadFile(path.Join(dir, name))
		if err != nil {
			return view, fmt.Errorf("读 %s 失败：%w", name, err)
		}
		sum := sha256.Sum256(data)
		view.Files = append(view.Files, netplanFile{Name: name, Sha256: hex.EncodeToString(sum[:])})
		if name == view.PanelName {
			view.PanelPresent = true
		}
		inspectOneNetplan(view.Dir+"/"+name, data, iface, mac, &view)
	}
	if !view.PanelPresent {
		// 面板文件不在时也要有一条"目标网卡现在由谁管"的记录（比如由 cloud-init 配）。
		// 上面的循环已经覆盖了这一点；这里只是把 PanelName 明确留在结果里。
		view.PanelName = PanelNetplanPrefix + iface + ".yaml"
	}
	return view, nil
}

// inspectOneNetplan 检查单个文件。所有"不支持"都以人话追加到 view.Unsupported。
func inspectOneNetplan(fileName string, data []byte, iface, mac string, view *netplanView) {
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		view.Unsupported = append(view.Unsupported,
			fmt.Sprintf("%s 解析不了（%v）：读不懂的配置不许覆盖", path.Base(fileName), err))
		return
	}
	network := asMap(doc["network"])
	if network == nil {
		// 没有 network 键的 YAML 不是 netplan 配置（可能是别的工具误放的），不影响我们。
		return
	}
	if v, ok := network["version"]; ok {
		if n, ok := asInt(v); !ok || n != 2 {
			view.Unsupported = append(view.Unsupported,
				fmt.Sprintf("%s 的 network.version 不是 2：面板只写 netplan v2", path.Base(fileName)))
		}
	}
	base := path.Base(fileName)

	// 桥接、bond、VLAN、Wi-Fi 里出现这张网卡，说明它是一组虚拟设备的成员：
	// 面板改它的地址会连带改掉那组设备的形状，不是"改个 IP"那么简单。
	for _, kind := range []string{"bridges", "bonds", "vlans", "wifis"} {
		for name, spec := range asMap(network[kind]) {
			if referencesInterface(spec, iface) {
				view.Unsupported = append(view.Unsupported,
					fmt.Sprintf("%s 把 %s 用作 %s %q 的一部分：面板只改独立网卡的 IPv4", base, iface, kind, name))
			}
		}
	}

	spec := asMap(asMap(network["ethernets"])[iface])
	if spec == nil {
		// 这个文件没提到目标网卡，与能不能覆盖无关。
		return
	}
	keys := make([]string, 0, len(spec))
	for key := range spec {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	decl := netplanDeclaration{File: base, Keys: keys}
	view.Declarations = append(view.Declarations, decl)

	for _, key := range keys {
		if _, ok := netplanAllowedKeys[key]; !ok {
			view.Unsupported = append(view.Unsupported, fmt.Sprintf(
				"%s 在 %s 上用了面板不认识的键 %q：覆盖它有可能改坏那条配置的理由",
				base, iface, key))
		}
	}
	if match := asMap(spec["match"]); match != nil {
		if declared, ok := match["macaddress"]; ok && mac != "" {
			if got := strings.ToLower(strings.TrimSpace(fmt.Sprint(declared))); got != strings.ToLower(mac) {
				view.Unsupported = append(view.Unsupported, fmt.Sprintf(
					"%s 里 match.macaddress=%s 与这张网卡的 %s 不一致：这份配置本来就不属于它",
					base, got, strings.ToLower(mac)))
			}
		}
	}
	// addresses 里出现 IPv6 或第二个地址，说明"只有一个 IPv4"这个前提不成立。
	if addrs, ok := spec["addresses"].([]any); ok {
		for _, a := range addrs {
			s := strings.TrimSpace(fmt.Sprint(a))
			if strings.Contains(s, ":") {
				view.Unsupported = append(view.Unsupported,
					fmt.Sprintf("%s 在 %s 上配了 IPv6 地址 %s：面板只改 IPv4", base, iface, s))
				continue
			}
			if !strings.Contains(s, "/") {
				view.Unsupported = append(view.Unsupported,
					fmt.Sprintf("%s 在 %s 上的地址 %q 没有前缀长度：读不准的写法不许覆盖", base, iface, s))
			}
		}
	}
}

// referencesInterface 判断某个虚拟设备的定义里有没有引用目标网卡。
func referencesInterface(spec any, iface string) bool {
	m := asMap(spec)
	if m == nil {
		return false
	}
	list, ok := m["interfaces"].([]any)
	if !ok {
		return false
	}
	for _, item := range list {
		if strings.TrimSpace(fmt.Sprint(item)) == iface {
			return true
		}
	}
	return false
}

// asMap 把 YAML 解出来的值归一成 map[string]any。
//
// goccy 在嵌套里给的是 map[string]any，别的实现可能给 map[any]any；两种都接住，
// 免得"能不能读"取决于库的内部选择。
func asMap(v any) map[string]any {
	switch m := v.(type) {
	case map[string]any:
		return m
	case map[any]any:
		out := make(map[string]any, len(m))
		for k, val := range m {
			out[fmt.Sprint(k)] = val
		}
		return out
	default:
		return nil
	}
}

// asInt 把 YAML 里的数字归一成 int（goccy 可能给 int/uint64/float64）。
func asInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case uint64:
		return int(n), true
	case float64:
		return int(n), n == float64(int(n))
	default:
		return 0, false
	}
}

// ---- 指纹与内容哈希 ----

// configFingerprint 是 `expected_config_hash` 的原像。
//
// 字段顺序就是 json.Marshal 的输出顺序（Go 按结构体字段写），map 的键由 encoding/json
// 排序，所以这份 JSON 是**逐字节确定**的：同一份现场在同一台机器上永远算出同一个哈希，
// 不受文件遍历顺序、时间、随机数影响。
type configFingerprint struct {
	Interface    string            `json:"interface"`
	PermanentMAC string            `json:"permanent_mac"`
	Netplan      map[string]string `json:"netplan"`
	PanelFile    *panelFileRef     `json:"panel_file"`
}

// panelFileRef 是面板那份文件的现状；不存在时整个字段为 null。
type panelFileRef struct {
	Name   string `json:"name"`
	Sha256 string `json:"sha256"`
}

// configHash 由巡检结果算出 `sha256:` 开头的指纹。
func (v netplanView) configHash() (string, string, error) {
	return v.configHashWithMAC(v.PermanentMAC)
}

// configHashWithMAC 用指定的 MAC 算指纹。
//
// 存在的理由只有一个：把"预检之后配置被改了"与"这张网卡的永久 MAC 变了"分开。
// 指纹里带着 MAC，所以两种情况都会让哈希对不上；而提交上来的只有哈希本身，
// MAC 是算进去的、取不出来。用空 MAC 再算一次——对上了，说明差别只在 MAC 那一项。
func (v netplanView) configHashWithMAC(mac string) (string, string, error) {
	fp := configFingerprint{
		Interface:    v.Interface,
		PermanentMAC: strings.ToLower(mac),
		Netplan:      make(map[string]string, len(v.Files)),
	}
	for _, f := range v.Files {
		fp.Netplan[f.Name] = f.Sha256
		if f.Name == v.PanelName {
			ref := panelFileRef{Name: f.Name, Sha256: f.Sha256}
			fp.PanelFile = &ref
		}
	}
	raw, err := json.Marshal(fp)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), string(raw), nil
}
