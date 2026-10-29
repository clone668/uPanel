package network

import (
	"path"
	"sort"
	"strings"
)

// detectCapability 判定"这份配置由谁管"以及"P14.2 的写路径能不能打开"。
//
// 判定只依赖两类事实：netplan 自己的文件（配置源）与渲染出来的产物（结果）。不去解析
// netplan 的 YAML——P14.1 是只读阶段，写路径的预检在 P14.2 会重新做一遍并逐字段核对；
// 这里只回答"要不要把编辑入口显示出来"，宁可判成不可改，也不要判成可改然后改坏。
func detectCapability(deps Deps, ifaces []Interface, issues *[]Issue) Capability {
	cap := Capability{Reasons: []string{}}

	netplanFiles, netplanErr := netplanConfigs(deps)
	rendered := rendererArtifacts(deps, issues)

	switch {
	case len(netplanFiles) > 0 && rendered.networkd:
		cap.Backend = BackendNetplanNetworkd
	case len(netplanFiles) > 0 && rendered.networkManager:
		cap.Backend = BackendNetplanNetworkManager
	case len(netplanFiles) > 0:
		cap.Backend = BackendUnknown
		cap.Reasons = append(cap.Reasons,
			"/etc/netplan 下有配置，但看不到 netplan 渲染给 systemd-networkd 或 NetworkManager 的产物："+
				"无法确认这份配置由谁负责，不做修改")
	case netplanErr == nil && len(netplanFiles) == 0:
		if rendered.networkManager || rendered.networkManagerRunning {
			cap.Backend = BackendNetworkManager
			cap.Reasons = append(cap.Reasons,
				"本机没有 netplan 配置，网络由 NetworkManager 自己管理；PLAN §19 只允许改 netplan + systemd-networkd")
		} else {
			cap.Backend = BackendUnknown
			cap.Reasons = append(cap.Reasons,
				"既没有 netplan 配置，也没有 NetworkManager 在管；无法确认网络配置的归属，不做修改")
		}
	default:
		// /etc/netplan 根本列不出来（权限或不存在）。这不是"没有配置"，是"不知道"。
		cap.Backend = BackendUnknown
		cap.Reasons = append(cap.Reasons, "读不到 /etc/netplan，无法确认网络配置的归属，不做修改")
	}

	if cap.Backend != BackendNetplanNetworkd {
		return cap
	}

	// 到这里才知道是 netplan + networkd。接着看"这张机器上有没有超出支持范围的东西"。
	// 三个判据都来自运行时真实存在的网卡，不来自配置文本：
	complex := map[string][]string{}
	for _, iface := range ifaces {
		switch iface.Kind {
		case KindWifi:
			complex["Wi-Fi"] = append(complex["Wi-Fi"], iface.Name)
		case KindBridge:
			complex["网桥"] = append(complex["网桥"], iface.Name)
		case KindBond:
			complex["bond"] = append(complex["bond"], iface.Name)
		case KindVLAN:
			complex["VLAN"] = append(complex["VLAN"], iface.Name)
		}
	}
	if len(complex) > 0 {
		kinds := make([]string, 0, len(complex))
		for kind := range complex {
			kinds = append(kinds, kind)
		}
		sort.Strings(kinds)
		for _, kind := range kinds {
			names := complex[kind]
			sort.Strings(names)
			cap.Reasons = append(cap.Reasons,
				"本机存在"+kind+"接口（"+strings.Join(names, "、")+"）：PLAN §19 规定 1.0 只改单个物理网卡的 IPv4，其余先只读")
		}
	}

	physical := make([]Interface, 0, 2)
	for _, iface := range ifaces {
		if iface.Physical && iface.Kind == KindEthernet {
			physical = append(physical, iface)
		}
	}
	switch len(physical) {
	case 0:
		cap.Reasons = append(cap.Reasons, "找不到物理以太网卡，没有可改的目标")
	case 1:
		if physical[0].Master != "" {
			cap.Reasons = append(cap.Reasons,
				"唯一的物理网卡 "+physical[0].Name+" 挂在 "+physical[0].Master+" 下（已被别的配置接管），不做修改")
			break
		}
		cap.Target = physical[0].Name
	default:
		names := make([]string, 0, len(physical))
		for _, iface := range physical {
			names = append(names, iface.Name)
		}
		sort.Strings(names)
		cap.Reasons = append(cap.Reasons,
			"本机有 "+strings.Join(names, "、")+" 多张物理网卡：1.0 只支持单网卡，选择哪张要走主机控制台")
	}

	if rendered.networkManagerRunning {
		cap.Reasons = append(cap.Reasons,
			"NetworkManager 正在运行：两个管理者同时碰一张网卡时，改完的状态说不清是谁的")
	}

	cap.Editable = cap.Target != "" && len(cap.Reasons) == 0
	return cap
}

// rendererFacts 是从 /run 下看出来的渲染事实。
type rendererFacts struct {
	// networkd：/run/systemd/network 下有 netplan 渲染出来的 .network 文件。
	networkd bool
	// networkManager：/run/NetworkManager/system-connections 下有 netplan 渲染出来的连接。
	networkManager bool
	// networkManagerRunning：/run/NetworkManager 目录在（NM 启动时会建它）。
	networkManagerRunning bool
}

func rendererArtifacts(deps Deps, issues *[]Issue) rendererFacts {
	var facts rendererFacts
	fsys := deps.fs()
	run := deps.runDir()

	if names, err := fsys.ListDir(path.Join(run, "systemd/network")); err == nil {
		for _, name := range names {
			if strings.HasPrefix(name, "10-netplan-") && strings.HasSuffix(name, ".network") {
				facts.networkd = true
				break
			}
		}
	}
	if names, err := fsys.ListDir(path.Join(run, "NetworkManager/system-connections")); err == nil {
		for _, name := range names {
			if strings.HasPrefix(name, "netplan-") {
				facts.networkManager = true
				break
			}
		}
	}
	if _, err := fsys.ListDir(path.Join(run, "NetworkManager")); err == nil {
		facts.networkManagerRunning = true
	}
	return facts
}

// netplanConfigs 列出 /etc/netplan 下的 .yaml。只数个数，不解析内容。
func netplanConfigs(deps Deps) ([]string, error) {
	names, err := deps.fs().ListDir(deps.netplanDir())
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(names))
	for _, name := range names {
		if strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml") {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}
