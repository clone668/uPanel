package storage

import (
	"sort"
	"strings"
)

// criticalPath 是一条“不能碰”的系统路径，以及它在界面上出现的理由。
var criticalPaths = []struct {
	Path   string
	Reason string
}{
	{"/", "承载根文件系统"},
	{"/boot", "承载 /boot（启动所需）"},
	{"/boot/efi", "承载 EFI 系统分区（启动所需）"},
	{"/usr", "承载 /usr（系统程序）"},
	{"/etc", "承载 /etc（系统配置）"},
	{"/var", "承载 /var（系统数据）"},
	{"/var/lib/upanel", "承载面板数据库"},
	{"/var/lib/upanel-agent", "承载代理数据库"},
	{"/etc/upanel", "承载面板配置与密钥"},
}

// applyProtection 计算系统盘保护。
//
// 从“不能碰的路径”出发，找到它落在哪个挂载点、哪个设备上，再沿
// partition_of / depends_on 递归向上把所有底层设备一起标记。于是：
//
//   - 根在普通分区上 → 分区与整盘都受保护；
//   - 根在 LVM 上 → LV、PV 分区、物理盘一层层都受保护；
//   - 根在软 RAID 上 → md 设备与它所有成员盘都受保护。
//
// 不做的事：不按名字猜（不假设 sda 是系统盘），也不因为“看起来像数据盘”就放行。
func applyProtection(devices map[string]*Device, edges []Edge, mounts []mountRecord,
	mountOwner map[string]string, swaps []swapEntry) {

	// child → 父设备集合（沿边向上）。
	parents := map[string][]string{}
	for _, e := range edges {
		parents[e.FromDeviceID] = append(parents[e.FromDeviceID], e.ToDeviceID)
	}

	reasons := map[string]map[string]bool{}
	sources := map[string]map[string]bool{}
	mark := func(devID, reason string) {
		if _, ok := devices[devID]; !ok {
			return
		}
		if reasons[devID] == nil {
			reasons[devID] = map[string]bool{}
			sources[devID] = map[string]bool{}
		}
		reasons[devID][reason] = true
		sources[devID][devID] = true
	}

	// 1) 关键路径 → 挂载点 → 设备。
	for _, cp := range criticalPaths {
		if mountPath, ok := mountForPath(mounts, cp.Path); ok {
			if devID := mountOwner[mountPath]; devID != "" {
				mark(devID, cp.Reason)
			}
		}
	}

	// 2) 交换区：块设备直接标记；交换文件按“它落在哪个文件系统上”标记。
	for _, sw := range swaps {
		if sw.DevName != "" {
			if _, ok := devices[sw.DevName]; ok {
				mark(sw.DevName, "作为活动交换区（"+sw.Path+"）")
				continue
			}
		}
		if mountPath, ok := mountForPath(mounts, sw.Path); ok {
			if devID := mountOwner[mountPath]; devID != "" {
				mark(devID, "承载交换文件（"+sw.Path+"）")
			}
		}
	}

	// 3) 向上递归：任何底层设备一旦被标记，就在整条链上标记。
	//    用固定点迭代而不是一次性 BFS：一条链上可能先标记后补边，
	//    迭代到不再变化为止（最大迭代次数受设备数限制，天然收敛）。
	for changed := true; changed; {
		changed = false
		for devID, rs := range reasons {
			for _, parent := range parents[devID] {
				if _, ok := devices[parent]; !ok {
					continue
				}
				if reasons[parent] == nil {
					reasons[parent] = map[string]bool{}
					sources[parent] = map[string]bool{}
				}
				for r := range rs {
					if !reasons[parent][r] {
						reasons[parent][r] = true
						changed = true
					}
				}
				for s := range sources[devID] {
					if !sources[parent][s] {
						sources[parent][s] = true
						changed = true
					}
				}
			}
		}
	}

	// 4) 写回设备。没有理由的设备也要给出空数组，前端不必区分 null 与空。
	for id, dev := range devices {
		rs := reasons[id]
		if len(rs) == 0 {
			dev.Protection = Protection{SystemDisk: false, Reasons: []string{}, Sources: []string{}}
			continue
		}
		list := make([]string, 0, len(rs))
		for r := range rs {
			list = append(list, r)
		}
		sort.Strings(list)
		src := make([]string, 0, len(sources[id]))
		for s := range sources[id] {
			src = append(src, s)
		}
		sort.Strings(src)
		dev.Protection = Protection{SystemDisk: true, Reasons: list, Sources: src}
	}
}

// mountForPath 找 path 所在的挂载点：取**最长**的、路径边界正确的那个。
// 边界正确很重要：/boot 不能被 / 覆盖，/bootxyz 也不能被算成在 /boot 里。
func mountForPath(mounts []mountRecord, path string) (string, bool) {
	best := ""
	found := false
	for _, m := range mounts {
		if !pathHasPrefix(path, m.path) {
			continue
		}
		if !found || len(m.path) > len(best) {
			best = m.path
			found = true
		}
	}
	return best, found
}

// pathHasPrefix 按目录边界判断前缀。
func pathHasPrefix(path, prefix string) bool {
	if prefix == "" {
		return false
	}
	if prefix == "/" {
		return strings.HasPrefix(path, "/")
	}
	if path == prefix {
		return true
	}
	return strings.HasPrefix(path, prefix+"/")
}
