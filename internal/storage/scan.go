package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Scan 采集一次完整拓扑。
//
// 失败时返回错误，调用方（Collector）保留上一次成功的快照——
// 拓扑读不到不等于“服务器上没有磁盘”，把节点清空是更坏的答案。
func Scan(ctx context.Context, root Root) (*Topology, error) {
	if root.Sys == "" || root.Proc == "" {
		return nil, fmt.Errorf("storage: Root.Sys 与 Root.Proc 不能为空")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	now := root.now()
	devices := map[string]*Device{}
	// devDirs 是设备名 → 它在 sysfs 里的目录。分区的目录嵌在整盘下面，
	// 不能由名字拼出来，所以统一记在这里。
	devDirs := map[string]string{}
	holders := map[string][]string{}
	order := []string{}

	blockDir := joinPath(root.Sys, "block")
	names := dirNames(blockDir)
	if names == nil {
		return nil, fmt.Errorf("storage: 读不到 %s（没有 sysfs？）", blockDir)
	}
	sort.Strings(names)

	// 第一步：顶层块设备本身。
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		dir := joinPath(blockDir, name)
		devDirs[name] = dir
		dev := readDevice(root.Sys, dir, name, KindDisk, now)
		devices[name] = dev
		order = append(order, name)
		holders[name] = linkNames(joinPath(dir, "holders"))
	}

	edges := []Edge{}

	// 第二步：分区。分区识别只看 sysfs 的目录嵌套与 partition 属性，不看名字前缀是否“像”。
	for _, diskName := range order {
		disk := devices[diskName]
		// 只有整盘（以及 md/dm 这类可容纳分区的设备）才有分区；
		// 但即便内核把分区挂在别的设备下，下面的循环也会照样发现。
		partNames := partitionChildren(blockDir, diskName)
		if len(partNames) == 0 {
			continue
		}
		table := PartitionTable{
			Type:       TableUnknown,
			Partitions: make([]Partition, 0, len(partNames)),
			// 空数组而不是 null：前端不必区分“没有空闲区”和“字段缺失”。
			FreeRegions: []FreeRegion{},
			Source:      "kernel",
		}
		for _, pn := range partNames {
			pdir := joinPath(blockDir, diskName, pn)
			devDirs[pn] = pdir
			pdev := readDevice(root.Sys, pdir, pn, KindPartition, now)
			devices[pn] = pdev
			holders[pn] = linkNames(joinPath(pdir, "holders"))
			startSectors, _ := readUint(joinPath(pdir, "start"))
			number, _ := readUint(joinPath(pdir, "partition"))
			table.Partitions = append(table.Partitions, Partition{
				DeviceID:   pn,
				Number:     int(number),
				StartBytes: startSectors * 512,
				SizeBytes:  pdev.SizeBytes,
			})
			edges = append(edges, Edge{FromDeviceID: pn, ToDeviceID: diskName, Relation: RelationPartitionOf})
		}
		sort.Slice(table.Partitions, func(i, j int) bool {
			return table.Partitions[i].Number < table.Partitions[j].Number
		})
		disk.PartitionTable = &table
	}

	// 第三步：dm / md / loop 的依赖关系（holders/slaves）。
	edgeSeen := map[string]bool{}
	for _, e := range edges {
		edgeSeen[e.FromDeviceID+"->"+e.ToDeviceID] = true
	}
	for _, name := range order {
		slaves := linkNames(joinPath(blockDir, name, "slaves"))
		for _, s := range slaves {
			if _, ok := devices[s]; !ok {
				continue
			}
			key := name + "->" + s
			if edgeSeen[key] {
				continue
			}
			edgeSeen[key] = true
			edges = append(edges, Edge{FromDeviceID: name, ToDeviceID: s, Relation: RelationDependsOn})
		}
		// holders 是 slaves 的反向视图。有些内核/驱动只在一侧给出信息，
		// 两边都看，缺的那一侧补上（关系本身不依赖名字猜测）。
		for _, h := range linkNames(joinPath(blockDir, name, "holders")) {
			if _, ok := devices[h]; !ok {
				continue
			}
			key := h + "->" + name
			if edgeSeen[key] {
				continue
			}
			edgeSeen[key] = true
			edges = append(edges, Edge{FromDeviceID: h, ToDeviceID: name, Relation: RelationDependsOn})
		}
	}

	// 第四步：挂载、稳定链接、交换区，然后填进设备。
	mounts, err := readMounts(joinPath(root.Proc, "self", "mountinfo"))
	if err != nil {
		return nil, err
	}
	byDev := map[string]string{}
	known := make(map[string]bool, len(devDirs))
	for name, dir := range devDirs {
		known[name] = true
		if majmin, ok := readFile(joinPath(dir, "dev")); ok {
			byDev[majmin] = name
		}
	}
	devDirRoot := joinPath(root.Sys, "..", "dev")
	uuidByDev := readDiskLinks(devDirRoot, known, "by-uuid")
	labelByDev := readDiskLinks(devDirRoot, known, "by-label")

	mountsByDev := map[string][]Mount{}
	for _, m := range mounts {
		devName := byDev[m.majorMinor]
		if devName == "" {
			// 没有对应的块设备：tmpfs、proc、cgroup、网络文件系统等。
			// 它们不进设备列表，但如果是关键挂载点，保护判断仍然要知道它。
			continue
		}
		mountsByDev[devName] = append(mountsByDev[devName], Mount{
			Path:     m.path,
			Source:   m.source,
			FSType:   m.fstype,
			Options:  m.options,
			ReadOnly: m.readOnly,
			// 容量在 Collector 里填（30 秒缓存，见 collector.go）。
			Capacity: nil,
		})
	}

	// 没有块设备的网络挂载：独立列进快照（PLAN.md 第 8 节）。
	//
	// tmpfs、proc、cgroup 之类同样没有块设备，但它们和磁盘管理无关，不进拓扑；
	// 这里只收网络文件系统，判断依据是 fstype 而不是挂载点路径写法。
	networkMounts := selectNetworkMounts(mounts, byDev)

	swaps := readSwaps(joinPath(root.Proc, "swaps"))
	swapByDev := map[string]*SwapUsage{}
	for _, sw := range swaps {
		if sw.DevName != "" {
			swapByDev[sw.DevName] = sw.Usage
		}
	}

	// mountOwner 记录每个挂载点属于哪个设备，供保护判断使用。
	mountOwner := map[string]string{}
	for _, m := range mounts {
		if devName := byDev[m.majorMinor]; devName != "" {
			mountOwner[m.path] = devName
		}
	}

	for _, name := range order {
		dev := devices[name]
		if ms := mountsByDev[name]; len(ms) > 0 {
			sort.Slice(ms, func(i, j int) bool { return ms[i].Path < ms[j].Path })
			fs := &Filesystem{Mounts: ms, UUID: pick(uuidByDev[name]), Label: pick(labelByDev[name])}
			t := ms[0].FSType
			fs.Type = &t
			dev.Filesystem = fs
		} else if uuidByDev[name] != "" || labelByDev[name] != "" {
			// 没挂载，但有稳定链接：UUID/LABEL 是已知的，类型不知道（不调用 blkid）。
			dev.Filesystem = &Filesystem{
				UUID:   pick(uuidByDev[name]),
				Label:  pick(labelByDev[name]),
				Mounts: []Mount{},
			}
		}
		if sw, ok := swapByDev[name]; ok {
			dev.Swap = sw
		}
	}

	// 分区设备也要拿到自己的挂载与 UUID（上面只处理了顶层设备）。
	for name, dev := range devices {
		if dev.Kind != KindPartition {
			continue
		}
		if ms := mountsByDev[name]; len(ms) > 0 {
			sort.Slice(ms, func(i, j int) bool { return ms[i].Path < ms[j].Path })
			fs := &Filesystem{Mounts: ms, UUID: pick(uuidByDev[name]), Label: pick(labelByDev[name])}
			t := ms[0].FSType
			fs.Type = &t
			dev.Filesystem = fs
		}
		if sw, ok := swapByDev[name]; ok {
			dev.Swap = sw
		}
	}

	// 第五步：设备身份与指纹。by-id 只读一次，所有盘共用。
	byID := readDiskLinks(devDirRoot, known, "by-id")
	for _, dev := range devices {
		fp, conf := deriveIdentity(devDirs[dev.Name], dev, byID)
		dev.IdentityFingerprint = fp
		dev.IdentityConfidence = conf
	}

	// 第六步：分区表探测（读盘头部）。只在盘是 online 且不在休眠时读。
	for _, name := range order {
		dev := devices[name]
		if dev.Kind != KindDisk && dev.Kind != KindRAID && dev.Kind != KindLVM {
			continue
		}
		probePartitionTable(ctx, root, devDirs[name], dev)
	}

	// 第七步：holders 落库。
	for name, hs := range holders {
		if dev, ok := devices[name]; ok {
			sorted := append([]string(nil), hs...)
			sort.Strings(sorted)
			dev.Holders = sorted
		}
	}

	// 第八步：系统盘保护（沿边递归）。
	applyProtection(devices, edges, mounts, mountOwner, swaps)

	// 第九步：排序与版本。
	list := make([]Device, 0, len(devices))
	for _, dev := range devices {
		list = append(list, *dev)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Kind != list[j].Kind {
			return kindRank(list[i].Kind) < kindRank(list[j].Kind)
		}
		return list[i].Name < list[j].Name
	})
	edges = dedupeEdges(edges)

	topo := &Topology{
		Status:        StatusOK,
		CollectedAt:   now,
		Nodes:         list,
		Edges:         edges,
		NetworkMounts: networkMounts,
	}
	version, err := snapshotVersion(list, edges, networkMounts)
	if err != nil {
		return nil, err
	}
	topo.SnapshotVersion = version
	return topo, nil
}

// kindRank 决定列表里不同类设备的先后：整盘 → 分区 → 逻辑设备 → 其它。
// 只影响展示顺序，不参与任何判断。
func kindRank(kind string) int {
	switch kind {
	case KindDisk:
		return 0
	case KindPartition:
		return 1
	case KindLVM:
		return 2
	case KindRAID:
		return 3
	case KindCrypt:
		return 4
	case KindLoop:
		return 5
	case KindROM:
		return 6
	default:
		return 7
	}
}

// readDevice 读取一个设备的 sysfs 属性。kind 只是初判，dm/md 会被重新分类。
//
// dir 是这块设备在 sysfs 里的目录。分区**不是** /sys/block/sda1——
// 分区的目录嵌在整盘下面（/sys/block/sda/sda1），所以目录必须由调用方给出，
// 不能由名字拼出来；sysRoot 用来把 sysfs 符号链接还原成相对 /sys 的路径。
func readDevice(sysRoot, dir, name, kind string, now time.Time) *Device {
	dev := &Device{
		ID:          name,
		Kind:        kind,
		Name:        name,
		CollectedAt: now,
		// 健康在 P04.3 由 SMART 采集填充（见 collector.go 的 healthLoop）。
		// 扫描阶段只给出“按设备类型讲得通的缺省值”：物理盘是 unknown（还没问 SMART），
		// 分区/回环/逻辑设备是 unsupported（它们本来就没有独立的 SMART）。
		// 这里绝不能写 healthy —— 那是采集成功后的结论。
		Health:       defaultHealth(kind),
		Capabilities: []Capability{},
	}

	sectors, ok := readUint(joinPath(dir, "size"))
	if ok {
		dev.SizeBytes = sectors * 512
	}
	if v, ok := readUint(joinPath(dir, "ro")); ok {
		dev.ReadOnly = v == 1
	}
	removable := uint64(0)
	if v, ok := readUint(joinPath(dir, "removable")); ok {
		removable = v
	}
	if v, ok := readUint(joinPath(dir, "queue", "logical_block_size")); ok && v > 0 {
		dev.LogicalSectorBytes = int(v)
	} else {
		dev.LogicalSectorBytes = 512
	}
	if v, ok := readUint(joinPath(dir, "queue", "physical_block_size")); ok && v > 0 {
		dev.PhysicalSectorBytes = int(v)
	} else {
		dev.PhysicalSectorBytes = dev.LogicalSectorBytes
	}
	if v, ok := readUint(joinPath(dir, "queue", "rotational")); ok {
		dev.Rotational = v == 1
	}

	if s, ok := readFile(joinPath(dir, "device", "model")); ok {
		dev.Model = strPtr(s)
	}
	if s, ok := readFile(joinPath(dir, "device", "serial")); ok {
		dev.Serial = strPtr(s)
	}
	if s, ok := readFile(joinPath(dir, "device", "wwid")); ok {
		dev.WWN = strPtr(s)
	}
	if t := detectTransport(sysRoot, dir); t != "" {
		dev.Transport = strPtr(t)
	}

	// 在线判断：sysfs 说 offline 就是离线；可移动设备没有介质（size=0）也算“不在”。
	online := true
	if s, ok := readFile(joinPath(dir, "device", "state")); ok {
		switch strings.ToLower(s) {
		case "offline", "not-present", "not present", "failed", "quiesced":
			online = false
		}
	}
	if removable == 1 && dev.SizeBytes == 0 {
		online = false
	}
	dev.Online = online

	// 重新分类：dm 是 LVM/crypt 还是别的，看 dm/uuid；md 看 md-present。
	dev.Kind = classifyKind(dir, name, kind, removable, dev.ReadOnly)
	// 健康占位跟着**实际类型**走：dm/md/loop 在顶层枚举时都是 disk，
	// 但重新分类后它们没有独立的 SMART，占位要改成 unsupported。
	dev.Health = defaultHealth(dev.Kind)
	return dev
}

// defaultHealth 给出设备在“还没问过 SMART”时的健康占位。
//
// 关键是区分两种“不知道”：
//   - 物理盘：SMART 是能读的，只是还没读 → unknown，采集完成后会被真实结果替换；
//   - 分区/回环/光驱/逻辑设备：它们本身没有 SMART → unsupported。
//
// 这两种都不能写成 healthy，也不能互相顶替：把分区标成 unknown 会让人以为采集出了问题，
// 把分区标成 healthy 更糟——那是在替一块盘的健康下结论。
func defaultHealth(kind string) Health {
	switch kind {
	case KindDisk:
		return Health{
			Status:    HealthUnknown,
			Source:    "smartctl",
			CheckedAt: nil,
			Details:   strPtr("SMART 采集尚未完成"),
		}
	case KindPartition:
		return Health{
			Status:    HealthUnsupported,
			Source:    "none",
			CheckedAt: nil,
			Details:   strPtr("分区没有独立的健康信息，请查看它所在的物理盘"),
		}
	case KindLoop:
		return Health{
			Status:    HealthUnsupported,
			Source:    "none",
			CheckedAt: nil,
			Details:   strPtr("回环设备没有硬件健康信息"),
		}
	case KindROM:
		return Health{
			Status:    HealthUnsupported,
			Source:    "none",
			CheckedAt: nil,
			Details:   strPtr("光驱没有 SMART 健康信息"),
		}
	default:
		return Health{
			Status:    HealthUnsupported,
			Source:    "none",
			CheckedAt: nil,
			Details:   strPtr("逻辑设备不直接提供 SMART，健康取决于它下面的物理盘"),
		}
	}
}

// classifyKind 依据 sysfs 的结构信号判断设备类型。
//
// 顺序是刻意的：先看只有某一类设备才有的属性（md-*、dm-*、loop 目录、
// SCSI 设备类型），最后才退到名字。退到名字的只有 loop/sr/fd 这三类——
// 它们的名字由内核子系统按序号分配（loop0、sr0、fd0），不是厂商字符串，
// 也不随槽位变化，因此可以安全地当作类型信号；"sda 是 SATA" 那种推断仍然不做。
func classifyKind(dir, name, topKind string, removable uint64, readOnly bool) string {
	if topKind == KindPartition {
		return KindPartition
	}

	// 1) loop：/sys/block/loopN/loop/ 只有 loop 设备才有。
	if st, err := os.Stat(joinPath(dir, "loop")); err == nil && st.IsDir() {
		return KindLoop
	}
	if strings.HasPrefix(name, "loop") && isAllDigits(name[len("loop"):]) {
		return KindLoop
	}

	// 2) 软 RAID（md）：真机的属性在 /sys/block/md0/md/ 里（level、array_state…），
	//    这个目录只有 md 设备才有。名字 mdN 也由内核按序号分配，作兜底。
	if st, err := os.Stat(joinPath(dir, "md")); err == nil && st.IsDir() {
		return KindRAID
	}
	if strings.HasPrefix(name, "md") && isAllDigits(name[len("md"):]) {
		return KindRAID
	}

	// 3) device-mapper：/sys/block/dm-0/dm/{name,uuid}。
	//    dm-uuid 的 LVM-/CRYPT- 前缀是标准命名，其余 dm（multipath、thin-pool 等）
	//    不猜，归 other。
	if uuid, ok := readFile(joinPath(dir, "dm", "uuid")); ok {
		switch {
		case strings.HasPrefix(uuid, "LVM-"):
			return KindLVM
		case strings.HasPrefix(uuid, "CRYPT-"):
			return KindCrypt
		}
		return KindOther
	}
	if _, ok := readFile(joinPath(dir, "dm", "name")); ok {
		return KindOther
	}
	if strings.HasPrefix(name, "dm-") && isAllDigits(name[len("dm-"):]) {
		return KindOther
	}

	// 4) 光学介质：SCSI 设备类型 5 就是 CD/DVD。名字 srN 同样由内核分配。
	if t, ok := readFile(joinPath(dir, "device", "type")); ok && t == "5" {
		return KindROM
	}
	if strings.HasPrefix(name, "sr") && isAllDigits(name[len("sr"):]) {
		return KindROM
	}
	// 软驱（fd0）既不是磁盘也不是光驱：没有介质、也不承载文件系统，归 other。
	if strings.HasPrefix(name, "fd") && isAllDigits(name[len("fd"):]) {
		return KindOther
	}

	if topKind == KindDisk {
		return KindDisk
	}
	return KindOther
}

// detectTransport 用 sysfs 的真实路径判断传输类型。
//
// 夹具里路径已经由采集脚本写成 device/realpath（相对 /sys）；真实运行时
// device 是个符号链接，这里解析它并把它变成相对 /sys 的路径。
// 两种情况用同一套子串判断，规则一致：不看名字前缀（sda 不等于 SATA）。
func detectTransport(sysRoot, dir string) string {
	p := ""
	if s, ok := readFile(joinPath(dir, "device", "realpath")); ok {
		p = s
	} else {
		link := joinPath(dir, "device")
		if resolved, err := resolveSymlink(link); err == nil {
			p = resolved
			if rel, err := relativeTo(sysRoot, resolved); err == nil {
				p = rel
			}
		}
	}
	if p == "" {
		return ""
	}
	lower := strings.ToLower(p)
	switch {
	case strings.Contains(lower, "/nvme"):
		return "nvme"
	case strings.Contains(lower, "/virtio"):
		return "virtio"
	case strings.Contains(lower, "/usb"):
		return "usb"
	case strings.Contains(lower, "/mmc"):
		return "mmc"
	case strings.Contains(lower, "/scsi"), strings.Contains(lower, "/target"):
		return "scsi"
	case strings.Contains(lower, "/ata"):
		return "ata"
	}
	return "unknown"
}

// partitionChildren 返回 disk 名下的分区名。
//
// 判断依据是内核给的 partition 属性（分区号），**不是**名字前缀：
// 名字可能被 udev/驱动改成任何样子，partition 属性却只有真分区才有。
// 只读 sysfs 的目录嵌套与属性，不猜测。
func partitionChildren(blockDir, diskName string) []string {
	dir := joinPath(blockDir, diskName)
	names := dirNames(dir)
	if names == nil {
		return nil
	}
	type part struct {
		name string
		num  int
	}
	parts := []part{}
	for _, child := range names {
		num, ok := readUint(joinPath(dir, child, "partition"))
		if !ok {
			continue
		}
		parts = append(parts, part{name: child, num: int(num)})
	}
	sort.Slice(parts, func(i, j int) bool {
		if parts[i].num != parts[j].num {
			return parts[i].num < parts[j].num
		}
		return parts[i].name < parts[j].name
	})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, p.name)
	}
	return out
}

// mountRecord 是从 /proc/self/mountinfo 里读出来的一行（只保留用得到的字段）。
type mountRecord struct {
	majorMinor string
	path       string
	source     string
	fstype     string
	options    []string
	readOnly   bool
}

// readMounts 解析 /proc/self/mountinfo。
//
// 用 mountinfo 而不是 /proc/mounts：它第 3 个字段是 major:minor，
// 可以直接和 sysfs 的 dev 对上，不必靠 “/dev/sda1” 这类可能被改写过的名字匹配。
// 字段里的空格用八进制转义（\040），要还原。
func readMounts(path string) ([]mountRecord, error) {
	raw, ok := readFile(path)
	if !ok {
		return nil, fmt.Errorf("storage: 读不到挂载表 %s", path)
	}
	out := []mountRecord{}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 7 {
			continue
		}
		sep := -1
		for i := 6; i < len(fields); i++ {
			if fields[i] == "-" {
				sep = i
				break
			}
		}
		if sep < 0 || sep+1 >= len(fields) {
			continue
		}
		rec := mountRecord{
			majorMinor: fields[2],
			path:       unescapeMountField(fields[4]),
			options:    strings.Split(fields[5], ","),
			fstype:     fields[sep+1],
		}
		if sep+2 < len(fields) {
			rec.source = unescapeMountField(fields[sep+2])
		}
		for _, opt := range rec.options {
			if opt == "ro" {
				rec.readOnly = true
			}
		}
		out = append(out, rec)
	}
	return out, nil
}

// isAllDigits 判断 s 是否全为 0-9（空串返回 false）。
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// unescapeMountField 还原 mountinfo 里的 \040 \011 \012 \134 转义。
func unescapeMountField(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) && isAllDigits(s[i+1:i+4]) {
			switch s[i+1 : i+4] {
			case "040":
				b.WriteByte(' ')
				i += 3
				continue
			case "011":
				b.WriteByte('\t')
				i += 3
				continue
			case "012":
				b.WriteByte('\n')
				i += 3
				continue
			case "134":
				b.WriteByte('\\')
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// swapEntry 是 /proc/swaps 的一行。DevName 只有在该行指向块设备时非空；
// 交换文件（/swapfile）走 Path，由保护判断按“它落在哪个文件系统上”处理。
type swapEntry struct {
	Path    string
	DevName string
	Usage   *SwapUsage
}

// readSwaps 解析 /proc/swaps。
//
// 文件格式是四列表头 + 数据行，大小单位是 KiB。
func readSwaps(path string) []swapEntry {
	raw, ok := readFile(path)
	if !ok {
		return nil
	}
	out := []swapEntry{}
	lines := strings.Split(raw, "\n")
	for i, line := range lines {
		if i == 0 {
			continue // 表头
		}
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		sizeKiB, err1 := strconv.ParseUint(fields[2], 10, 64)
		usedKiB, err2 := strconv.ParseUint(fields[3], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		priority := 0
		if len(fields) >= 5 {
			if p, err := strconv.Atoi(fields[4]); err == nil {
				priority = p
			}
		}
		file := unescapeMountField(fields[0])
		out = append(out, swapEntry{
			Path:    file,
			DevName: devNameFromPath(file),
			Usage: &SwapUsage{
				Type:      fields[1],
				SizeBytes: sizeKiB * 1024,
				UsedBytes: usedKiB * 1024,
				Priority:  priority,
			},
		})
	}
	return out
}

// devNameFromPath 把 /dev/sda1、/dev/mapper/vg-lv、/dev/dm-0 之类变成内核设备名。
func devNameFromPath(p string) string {
	if p == "" || !strings.HasPrefix(p, "/dev/") {
		return ""
	}
	name := strings.TrimPrefix(p, "/dev/")
	if strings.HasPrefix(name, "mapper/") {
		// /dev/mapper/<name> 是 dm 的别名，真实设备名要从符号链接解析；
		// 这里没有解析（见 readDiskLinks 的做法），因此先跳过。
		return ""
	}
	// /dev/disk/by-uuid/... 之类不是设备名。
	if strings.Contains(name, "/") {
		return ""
	}
	return name
}

// devNameIn 在给定的设备目录里把链接目标换算成内核设备名。
//
// devDir 在真实系统上是 /dev，所以 /dev/sda1、/dev/disk/by-id/../../sda 解析出来都直接可用。
// 但夹具里的 devDir 是采集目录下的 dev/，符号链接解析出来是 /var/tmp/xxx/dev/sda1 这种
// 临时绝对路径，devNameFromPath 的 /dev/ 前缀检查会把它判掉——于是先把 devDir 剥掉，
// 按“devDir 就是这台机器的 /dev”来算，测试才和真实语义一致。
func devNameIn(devDir, target string) string {
	if devDir != "" {
		if rel, err := relativeTo(devDir, target); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
			return devNameFromPath(joinPath("/dev", rel))
		}
	}
	return devNameFromPath(target)
}

// readDiskLinks 读取 /dev/disk/by-<kind>/ 下的稳定链接，返回 设备名 → 值。
//
// 真实运行时这些条目是符号链接（指向 /dev/sda1 之类的块设备），夹具里是写着目标路径的普通文件。
// 两种都要读，而且**必须先判断是不是符号链接**：对块设备调用 os.ReadFile 会把整块盘读进内存，
// 详见 linkTarget 的注释（那是真实发生过的事故）。
//
// known 是本次扫描枚举到的设备名集合：链接只对**已知块设备**生效。不按名字长相过滤——
// /dev 下任何文件都可能被链接指向（例如 loop 镜像文件），只有 sysfs 里认得的才是设备。
func readDiskLinks(devDir string, known map[string]bool, kind string) map[string]string {
	out := map[string]string{}
	dir := joinPath(devDir, "disk", kind)
	names := dirNames(dir)
	for _, n := range names {
		target := linkTarget(joinPath(dir, n))
		if target == "" {
			continue
		}
		dev := devNameIn(devDir, target)
		if dev == "" || !known[dev] {
			continue
		}
		// 一块盘在 by-id 下常有两个链接（scsi-<vendor>_<model>_… 和
		// scsi-S<serial>_…）。按信息量取最好的那个，而不是靠目录遍历顺序
		// 决定谁覆盖谁——那样排序一变，身份可信度就跟着变。
		if cur, ok := out[dev]; ok && linkRank(cur) >= linkRank(n) {
			continue
		}
		out[dev] = n
	}
	return out
}

// linkRank 给 /dev/disk/by-id 的链接名打分：越能唯一确定物理设备分越高。
func linkRank(link string) int {
	switch {
	case strings.HasPrefix(link, "wwn-"), strings.HasPrefix(link, "nvme-"):
		return 5
	case strings.HasPrefix(link, "scsi-S"), strings.HasPrefix(link, "ata-"):
		return 4
	case strings.HasPrefix(link, "scsi-"):
		return 3
	case strings.HasPrefix(link, "dm-uuid-"):
		return 2
	default:
		return 1
	}
}

// snapshotVersion 计算拓扑的内容指纹（CONTRACTS §6：写动作预检拿它比对执行前版本）。
//
// 指纹只由**内容**决定，所以先把两类“会自己变但不是内容”的字段清掉：
//   - 每个节点上的 collected_at（每 5 秒一次扫描都会变）；
//   - 挂载点容量（由 Collector 后填，statfs 每次都可能不同）；
//   - 网络挂载的可达性与容量（同样是后填的观测值，不是配置内容）；
//   - 设备能力（由 Collector 后填，占用状态一变就变——那是观测值，不是拓扑内容）。
//
// 不清掉这几样，“内容没变版本就不变”不成立，预检会把一次没有任何变化的写操作判成
// “拓扑已改变”，或者反过来把真正的变化混在噪声里看不出来。
func snapshotVersion(nodes []Device, edges []Edge, networkMounts []NetworkMount) (string, error) {
	normalized := make([]Device, len(nodes))
	for i, n := range nodes {
		n.CollectedAt = time.Time{}
		n.Health.CheckedAt = nil
		// 能力列表是判决的投影，不是拓扑内容：一个进程打开了盘就会变。
		// 把它算进版本会让版本随占用抖动，预检的“版本未变”就失去意义。
		n.Capabilities = nil
		if n.Filesystem != nil {
			fs := *n.Filesystem
			fs.Mounts = make([]Mount, len(n.Filesystem.Mounts))
			for j, m := range n.Filesystem.Mounts {
				m.Capacity = nil
				m.CapacityError = ""
				fs.Mounts[j] = m
			}
			n.Filesystem = &fs
		}
		normalized[i] = n
	}
	normalizedNetwork := make([]NetworkMount, len(networkMounts))
	for i, nm := range networkMounts {
		nm.Capacity = nil
		nm.CapacityError = ""
		nm.Reachable = nil
		normalizedNetwork[i] = nm
	}
	payload := struct {
		Nodes         []Device       `json:"nodes"`
		Edges         []Edge         `json:"edges"`
		NetworkMounts []NetworkMount `json:"network_mounts"`
	}{Nodes: normalized, Edges: edges, NetworkMounts: normalizedNetwork}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("storage: 计算拓扑版本失败: %w", err)
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func dedupeEdges(edges []Edge) []Edge {
	seen := map[string]bool{}
	out := make([]Edge, 0, len(edges))
	for _, e := range edges {
		key := e.FromDeviceID + "->" + e.ToDeviceID + "/" + e.Relation
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].FromDeviceID != out[j].FromDeviceID {
			return out[i].FromDeviceID < out[j].FromDeviceID
		}
		if out[i].ToDeviceID != out[j].ToDeviceID {
			return out[i].ToDeviceID < out[j].ToDeviceID
		}
		return out[i].Relation < out[j].Relation
	})
	return out
}

func pick(s string) *string {
	if s == "" {
		return nil
	}
	v := s
	return &v
}

func strPtr(s string) *string { return &s }

// selectNetworkMounts 从挂载表里挑出**没有块设备**的网络挂载。
//
// byDev 是 major:minor → 设备名。挂载点能找到块设备，说明它是本地文件系统，
// 容量归那台设备的 filesystem.mounts；找不到才是“远端的东西”，
// 例如 NFS 的 0:xx 伪设备号——远端机器不在本机 sysfs 里，没有块设备可挂。
//
// 只收网络文件系统：tmpfs、proc、cgroup 同样没有块设备，但它们与磁盘管理无关。
// 路径去重是因为同一路径可能被绑定挂载多次，容量只该查一次。
func selectNetworkMounts(mounts []mountRecord, byDev map[string]string) []NetworkMount {
	out := []NetworkMount{}
	seen := map[string]bool{}
	for _, m := range mounts {
		if byDev[m.majorMinor] != "" || !IsNetworkFS(m.fstype) || seen[m.path] {
			continue
		}
		seen[m.path] = true
		out = append(out, NetworkMount{
			Path:     m.path,
			Source:   m.source,
			FSType:   m.fstype,
			ReadOnly: m.readOnly,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}
