package storage

// P07.1：存储写动作的**预检判决**。
//
// 这一层只做判断，不做任何写操作：给定一份拓扑快照、一个目标设备和一个动作，
// 回答“现在能不能做、为什么不能、会影响什么、要锁哪块盘、绑在什么身份与版本上”。
// 真正的分区、格式化、挂载在 P07.2—P07.4 实现，执行前会用同一个函数重算并比对
// 身份指纹与状态版本（Agent 的 revalidate），因此这里的每个判断在执行时都会再走一遍。
//
// 三条硬规则（PLAN.md 第 9 节，STAGES.md 第 9 节）：
//
//   - **拿不准就不放行**。身份、签名、占用、拓扑任何一项判定失败都变成阻塞项，
//     不是默认 false。磁盘写错一次是不可逆的，宁可按“不能做”处理。
//   - **整盘保护沿依赖递归**。目标自己的保护标记不算数，要沿 partition_of /
//     depends_on 一直向上追到物理盘：根在 LVM 上时，LV 分区、PV 分区、物理盘
//     三者必须同时被拒。锁也加在整块物理盘上，避免两个分区任务同时改一块盘。
//   - **未知即拒绝**。不认识的设备类型、读不出来的签名、只从 sysfs 拿到而没读盘的
//     分区表，都给出“不能做”而不是“看起来没问题”。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// 存储写动作的固定名称（CONTRACTS.md 第 5 节）。
const (
	ActionPartitionTableInitialize = "storage.partition_table.initialize"
	ActionPartitionCreate          = "storage.partition.create"
	ActionPartitionDelete          = "storage.partition.delete"
	ActionFilesystemFormat         = "storage.filesystem.format"
)

// 分区表类型只支持 GPT（PLAN.md 第 9.1 节：初期不修改 MBR 分区表）。
const TableTypeGPT = "gpt"

// 支持格式化的文件系统（PLAN.md 第 9.1 节）。
const (
	FilesystemExt4 = "ext4"
	FilesystemXFS  = "xfs"
)

// 对齐与尺寸限制。
const (
	// AlignBytes 是分区起始与大小的对齐边界（PLAN.md 第 9.1 节：按 1 MiB 边界对齐）。
	AlignBytes = uint64(1 << 20)
	// MinPartitionBytes 是最小分区大小。小于 1 MiB 的分区既不对齐也没有意义。
	MinPartitionBytes = AlignBytes
	// MaxPartitionNameRunes 是 GPT 分区名的上限（36 个 UTF-16 码元；只接受 ASCII 时等价于 36 字节）。
	MaxPartitionNameRunes = 36
	// MaxExt4LabelBytes / MaxXFSLabelBytes 是两种文件系统各自的标签上限。
	// 通用 255 字符的上限对它们都是错的：ext4 是 16 字节，XFS 是 12 字节。
	MaxExt4LabelBytes = 16
	MaxXFSLabelBytes  = 12
)

// 阻塞项与告警项的码。这是给人和前端分类用的固定字符串，不是 HTTP 错误码。
//
// 每个码都对应一种“必须被拒”的事实，测试按码断言；message 才给人读。
const (
	// NoticeTopologyUnavailable 表示拓扑采集失败，无法确认设备现状。
	NoticeTopologyUnavailable = "TOPOLOGY_UNAVAILABLE"
	// NoticeDeviceMissing 表示目标设备不在当前拓扑里（拔了、改名了、或者 id 写错）。
	NoticeDeviceMissing = "DEVICE_NOT_FOUND"
	// NoticeSystemDisk 表示目标（或它的任一底层设备）承载了不能碰的系统数据。
	NoticeSystemDisk = "SYSTEM_DISK_PROTECTED"
	// NoticeDeviceMounted 表示目标设备当前挂载着。
	NoticeDeviceMounted = "DEVICE_MOUNTED"
	// NoticeSwapActive 表示目标设备是活动交换区。
	NoticeSwapActive = "SWAP_ACTIVE"
	// NoticeHasHolders 表示有 dm/md 等设备正在消费这台设备。
	NoticeHasHolders = "DEVICE_HAS_HOLDERS"
	// NoticeBusy 表示有进程正打开这台设备。
	NoticeBusy = "DEVICE_BUSY"
	// NoticeOffline 表示设备当前不在线（离线、无介质）。
	NoticeOffline = "DEVICE_OFFLINE"
	// NoticeReadOnly 表示设备本身被标记为只读。
	NoticeReadOnly = "DEVICE_READ_ONLY"
	// NoticeUnsupportedKind 表示设备类型不在本阶段的支持范围里。
	NoticeUnsupportedKind = "UNSUPPORTED_DEVICE_KIND"
	// NoticeIdentityUnknown 表示这台设备没有可用的稳定身份，无法证明“执行时还是它”。
	NoticeIdentityUnknown = "IDENTITY_UNKNOWN"
	// NoticeIdentityConflict 表示同一个身份指纹出现在两台设备上（克隆盘、伪造 WWN）。
	NoticeIdentityConflict = "IDENTITY_CONFLICT"
	// NoticeTablePresent 表示盘上已有分区表，初始化会覆盖它。
	NoticeTablePresent = "PARTITION_TABLE_PRESENT"
	// NoticeTableUnknown 表示分区表类型判不出来。
	NoticeTableUnknown = "PARTITION_TABLE_UNKNOWN"
	// NoticeTableNotGPT 表示要修改的分区表不是 GPT。
	NoticeTableNotGPT = "PARTITION_TABLE_NOT_GPT"
	// NoticeTableUnreadable 表示没有读到盘上的分区表，只剩内核的分区列表。
	NoticeTableUnreadable = "PARTITION_TABLE_UNREADABLE"
	// NoticePartitionMissing 表示目标分区不在整盘的分区列表里。
	NoticePartitionMissing = "PARTITION_NOT_FOUND"
	// NoticeSignaturePresent 表示盘上已有文件系统/卷签名（初始化会覆盖它）。
	NoticeSignaturePresent = "FILESYSTEM_SIGNATURE_PRESENT"
	// NoticeSignatureUnreadable 表示读不出盘上的签名，无法证明它是空的。
	NoticeSignatureUnreadable = "SIGNATURE_UNREADABLE"
	// NoticeStackedDevice 表示目标位于 LVM/RAID/加密设备之上。
	NoticeStackedDevice = "DEVICE_IS_STACKED"
	// NoticeRegionUnaligned 表示分区起点或大小没有按 1 MiB 对齐。
	NoticeRegionUnaligned = "REGION_UNALIGNED"
	// NoticeRegionNotFree 表示请求的区间不在任何空闲区里（重叠或越界）。
	NoticeRegionNotFree = "REGION_NOT_FREE"
	// NoticeRegionTooSmall 表示请求的区间小于最小分区大小。
	NoticeRegionTooSmall = "REGION_TOO_SMALL"
	// NoticeNoFreeRegion 表示这块盘上没有可用的空闲区。
	NoticeNoFreeRegion = "NO_FREE_REGION"
	// NoticeParentUnknown 表示找不到目标所属的物理盘。
	NoticeParentUnknown = "PARENT_DISK_UNKNOWN"
	// NoticeLabelInvalid 表示文件系统标签不合法。
	NoticeLabelInvalid = "LABEL_INVALID"

	// NoticeIdentityLowConfidence 是告警：身份靠“型号 + 拓扑地址”这类弱依据，
	// 换槽位就认不出来。它不阻止执行，但必须让人看见。
	NoticeIdentityLowConfidence = "IDENTITY_LOW_CONFIDENCE"
	// NoticeTableWarning 是告警：采集时发现盘上分区表与内核分区列表不一致。
	NoticeTableWarning = "PARTITION_TABLE_WARNING"
	// NoticeExistingData 是告警：目标上已有数据，这次操作会清除它。
	NoticeExistingData = "EXISTING_DATA_WILL_BE_LOST"
)

// 影响项的 kind 闭集。
const (
	// ImpactDataLoss 表示不可恢复的数据丢失。
	ImpactDataLoss = "data_loss"
	// ImpactTableChange 表示分区表被改动（可重建，但仍然是破坏性的）。
	ImpactTableChange = "partition_table_change"
)

// Blocker 是阻塞项/告警项。code 是固定枚举，message 给人读。
type Blocker struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Impact 是这次操作的影响。
type Impact struct {
	Kind        string `json:"kind"`
	Description string `json:"description"`
}

// Assessment 是一次预检判决的结果。
//
// IdentityFingerprint 与 StateVersion 是**绑定**：执行前会用同一个判决函数重算并
// 逐字比较，指纹变了是 DEVICE_IDENTITY_CHANGED（换盘了），版本变了是 PREFLIGHT_STALE
// （同一块盘的状态变了）。因此这两项必须由设备本身的事实决定，不能掺入时间戳。
type Assessment struct {
	Action           string
	NormalizedParams any

	// TargetID 是用户点的那台设备（分区动作时是分区）。
	TargetID string
	// DiskIDs 是这次操作涉及的整块物理盘（锁与身份都按它算）。
	DiskIDs []string

	DisplayName         string
	IdentityFingerprint string
	StateVersion        string

	Allowed  bool
	Blockers []Blocker
	Warnings []Blocker
	Impacts  []Impact
	Locks    []string
}

// PartitionTableInitializeParams 是 storage.partition_table.initialize 的参数。
//
// 只有两个字段：目标设备与表类型。没有“强制/清空签名/忽略占用”这类开关——
// 有开关就会有绕过检查的路。
type PartitionTableInitializeParams struct {
	DeviceID  string `json:"device_id"`
	TableType string `json:"table_type"`
}

// PartitionCreateParams 是 storage.partition.create 的参数。
//
// start_bytes / size_bytes 是十进制字符串（CONTRACTS.md 第 1 节：字节一律用字符串，
// 2^53 以上的整数用 JSON number 会悄悄丢位）。
type PartitionCreateParams struct {
	DeviceID   string `json:"device_id"`
	StartBytes uint64 `json:"start_bytes,string"`
	SizeBytes  uint64 `json:"size_bytes,string"`
	Name       string `json:"name,omitempty"`
}

// PartitionDeleteParams 是 storage.partition.delete 的参数。目标就是分区本身。
type PartitionDeleteParams struct {
	DeviceID string `json:"device_id"`
}

// FilesystemFormatParams 是 storage.filesystem.format 的参数。
type FilesystemFormatParams struct {
	DeviceID   string `json:"device_id"`
	Filesystem string `json:"filesystem"`
	Label      string `json:"label,omitempty"`
}

// Env 是判决需要的外部输入。两个探针都是可注入的：单元测试给假实现，
// 生产实现读真实 /dev 与 /proc。
//
// 探针为 nil 时不是“跳过检查”，而是**阻塞**：读不到签名就证明不了盘是空的。
type Env struct {
	// Signatures 读取设备前部/尾部的文件系统与卷签名。
	Signatures SignatureProber
	// Busy 列出正打开这台设备的进程。
	Busy BusyScanner
}

// SystemEnv 返回指向真实 /dev、/sys、/proc 的生产探针。
//
// 装配点只有一个（cmd/upanel-agent/main.go）：探针没接线时设备能力保持空数组，
// 而不是被当成“什么都能做”。
func SystemEnv() Env {
	return Env{Signatures: SystemSignatureProber(), Busy: SystemBusyScanner()}
}

// ValidTableType 判断分区表类型是否被支持。
func ValidTableType(t string) bool { return t == TableTypeGPT }

// ValidFilesystem 判断文件系统类型是否被支持。
func ValidFilesystem(fs string) bool { return fs == FilesystemExt4 || fs == FilesystemXFS }

// ValidateLabel 按文件系统的实际限制校验标签。
//
// 不能用“通用 255 字符”糊过去：ext4 的上限是 16 字节、XFS 是 12 字节，
// 超了 mkfs 会失败，而预检不该把一个注定失败的任务算成“允许”。
func ValidateLabel(filesystem, label string) error {
	if label == "" {
		return nil
	}
	limit := MaxExt4LabelBytes
	if filesystem == FilesystemXFS {
		limit = MaxXFSLabelBytes
	}
	if len(label) > limit {
		return fmt.Errorf("%s 的标签最多 %d 字节，收到 %d 字节", filesystem, limit, len(label))
	}
	for i := 0; i < len(label); i++ {
		c := label[i]
		// mkfs 把标签写进超级块，'/' 与不可打印字节会让字体/挂载工具出问题；
		// 这里只接受可打印 ASCII，空格允许（它是合法的盘标）。
		if c < 0x20 || c > 0x7e || c == '/' {
			return fmt.Errorf("%s 的标签只能包含可打印 ASCII 且不能含 '/'", filesystem)
		}
	}
	return nil
}

// ValidatePartitionName 校验 GPT 分区名。
//
// 除了“可打印 ASCII、不超过 36 个字符”，还拒绝双引号与反斜杠——这不是审美，是实测出来的
// 限制：sfdisk 的分区脚本用双引号括起名字，util-linux 2.39.3 上 `name="a\"b"` 会被判成
// `line 1: unsupported command` 直接失败，而反斜杠会走它自己的转义处理（`a\b` 落盘后变成
// `a\x5cb` 这样的另一种东西）。名字是唯一一个进入命令模板的用户字符串，因此在这里就把
// 它限制成“放进引号里没有任何歧义”的字符集，而不是赌转义语义。
func ValidatePartitionName(name string) error {
	if name == "" {
		return nil
	}
	if len([]rune(name)) > MaxPartitionNameRunes {
		return fmt.Errorf("分区名最多 %d 个字符", MaxPartitionNameRunes)
	}
	for _, r := range name {
		if r < 0x20 || r > 0x7e {
			return fmt.Errorf("分区名只能包含可打印 ASCII")
		}
		if r == '"' || r == '\\' {
			return fmt.Errorf("分区名不能包含双引号或反斜杠（sfdisk 的脚本语法无法安全表达它们）")
		}
	}
	return nil
}

// Assess 对一次存储写动作给出预检判决。
//
// 返回值 error 只用于**编程错误**（动作名不认识、参数类型不匹配）：业务上的“不能做”
// 一律是 Allowed=false + Blockers，因为界面要能说清原因。
func Assess(ctx context.Context, topo Topology, action string, params any, env Env) (Assessment, error) {
	in, err := inputFor(action, params)
	if err != nil {
		return Assessment{}, err
	}
	return assess(ctx, topo, in, env), nil
}

// assessmentInput 是参数解出来的内部形态。region 为 nil 表示“还没有具体区间”，
// 这时只做通用判断（能力视图用它回答“这块盘上现在能不能建分区”）。
type assessmentInput struct {
	action     string
	deviceID   string
	normalized any

	tableType  string
	region     *PartitionCreateParams
	name       string
	filesystem string
	label      string
}

// inputFor 把动作参数转成内部形态。
func inputFor(action string, params any) (assessmentInput, error) {
	switch action {
	case ActionPartitionTableInitialize:
		p, ok := params.(PartitionTableInitializeParams)
		if !ok {
			return assessmentInput{}, fmt.Errorf("storage: %s 的参数类型不匹配: %T", action, params)
		}
		return assessmentInput{action: action, deviceID: p.DeviceID, normalized: p, tableType: p.TableType}, nil
	case ActionPartitionCreate:
		p, ok := params.(PartitionCreateParams)
		if !ok {
			return assessmentInput{}, fmt.Errorf("storage: %s 的参数类型不匹配: %T", action, params)
		}
		return assessmentInput{action: action, deviceID: p.DeviceID, normalized: p, region: &p, name: p.Name}, nil
	case ActionPartitionDelete:
		p, ok := params.(PartitionDeleteParams)
		if !ok {
			return assessmentInput{}, fmt.Errorf("storage: %s 的参数类型不匹配: %T", action, params)
		}
		return assessmentInput{action: action, deviceID: p.DeviceID, normalized: p}, nil
	case ActionFilesystemFormat:
		p, ok := params.(FilesystemFormatParams)
		if !ok {
			return assessmentInput{}, fmt.Errorf("storage: %s 的参数类型不匹配: %T", action, params)
		}
		return assessmentInput{action: action, deviceID: p.DeviceID, normalized: p, filesystem: p.Filesystem, label: p.Label}, nil
	default:
		return assessmentInput{}, fmt.Errorf("storage: 未知的存储动作 %q", action)
	}
}

// assess 是判决主体。
func assess(ctx context.Context, topo Topology, in assessmentInput, env Env) Assessment {
	a := Assessment{
		Action:           in.action,
		NormalizedParams: in.normalized,
		TargetID:         in.deviceID,
		DisplayName:      in.deviceID,
		Blockers:         []Blocker{},
		Warnings:         []Blocker{},
		Impacts:          []Impact{},
		Locks:            []string{},
		DiskIDs:          []string{},
		// 版本缺省是拓扑版本：连拓扑都没有时也要给出一个可比较的字符串，
		// 否则预检会在“缺少状态版本”这一步被当成实现错误（见 validatePlanResult）。
		StateVersion: topo.SnapshotVersion,
	}
	if a.StateVersion == "" {
		a.StateVersion = "unavailable"
	}
	// 采集失败的快照不能作为“现状”：里面的内容可能是上一次的，设备可能已经被换掉。
	// 这时拒绝，而不是拿旧快照放行。
	if topo.Status != StatusOK {
		reason := "存储拓扑采集失败，无法确认设备现状"
		if topo.Error != "" {
			reason += "：" + topo.Error
		}
		a.block(NoticeTopologyUnavailable, reason)
		a.finish()
		return a
	}

	index := indexTopology(topo)
	target, ok := index.devices[in.deviceID]
	if !ok {
		a.block(NoticeDeviceMissing, "设备 "+in.deviceID+" 不在当前拓扑里")
		a.finish()
		return a
	}
	a.DisplayName = displayNameFor(target, index)

	chain, disks, ok := index.chain(target.ID)
	if !ok || len(disks) == 0 {
		a.block(NoticeParentUnknown, "找不到设备 "+target.ID+" 所属的物理盘")
		a.finish()
		return a
	}
	a.DiskIDs = disks
	for _, d := range disks {
		a.Locks = append(a.Locks, "disk:"+d)
	}
	a.IdentityFingerprint = targetIdentity(target, index)
	if a.IdentityFingerprint == "" {
		// 没有可靠身份的设备仍然要被拒绝（下面的 IDENTITY_UNKNOWN），但预检必须绑定
		// 一个非空的指纹，否则预检会在“缺少身份指纹”这一步变成实现错误（502），
		// 而不是一条能读懂的拒绝。用一个稳定的占位值把两者都满足。
		a.IdentityFingerprint = "unidentified|" + target.ID
	}

	// 目标自己的身份是否可用。指纹为空或可信度为 unknown 时，“执行时还是不是它”
	// 无法证明，直接拒绝——这正是 PLAN.md 第 8 节“没有可靠身份的设备只读”的落点。
	confidence := target.IdentityConfidence
	if target.Kind == KindPartition {
		// 分区的身份不是它自己的（内核不给分区发行号），而是“哪块盘的哪个位置”。
		confidence = index.devices[diskOf(index, target.ID)].IdentityConfidence
	}
	switch confidence {
	case ConfidenceHigh, ConfidenceMedium:
	case ConfidenceLow:
		a.warn(NoticeIdentityLowConfidence,
			"设备身份依据较弱（型号 + 拓扑地址），换槽位后无法确认是同一块盘")
	default:
		a.block(NoticeIdentityUnknown, "设备没有可用的稳定身份（缺少 WWN、序列号与 by-id 链接）")
	}

	// 身份冲突：同一个指纹出现在两台设备上（克隆盘、伪造 WWN）。只读处理。
	if clash := index.fingerprintClash(target); len(clash) > 0 {
		a.block(NoticeIdentityConflict,
			"同一身份指纹出现在多台设备上："+strings.Join(clash, "、")+"，无法区分目标")
	}

	// 整条链上的系统盘保护：任一环节受保护就拒绝。
	if reasons := protectionReasons(chain); len(reasons) > 0 {
		a.block(NoticeSystemDisk, "目标承载系统数据，禁止写入："+strings.Join(reasons, "；"))
	} else if len(disks) > 1 {
		// 一块盘上的写操作只会动它自己，但跨多块盘说明目标在 RAID/dm 之上，
		// 真正的写入会同时影响所有成员盘；本阶段不支持。
		a.block(NoticeStackedDevice, "目标位于多块物理盘之上（RAID/dm），本阶段不支持")
	}

	// 目标位于 LVM/RAID/加密/回环设备之上时一律拒绝：这些层有自己的语义
	// （快照、镜像、加密头），直接在其上建分区或格式化会破坏它们。
	for _, d := range chain {
		switch d.Kind {
		case KindLVM, KindRAID, KindCrypt, KindLoop, KindROM, KindOther:
			if d.ID == target.ID && (in.action == ActionPartitionDelete || in.action == ActionFilesystemFormat) {
				// 目标自身就是这类设备：动作与设备类型不匹配，下面按动作再判。
				continue
			}
			a.block(NoticeStackedDevice,
				"目标位于 "+d.ID+"（"+kindLabel(d.Kind)+"）之上，本阶段不支持在其上写入")
		}
	}

	// 设备可用性。
	if !target.Online {
		a.block(NoticeOffline, "设备当前不在线（离线、无介质或被移除）")
	}
	if target.ReadOnly {
		a.block(NoticeReadOnly, "设备本身被标记为只读")
	}

	// 挂载与交换区：挂着的盘不能改分区表，也不能格式化。
	if target.Filesystem != nil && len(target.Filesystem.Mounts) > 0 {
		paths := make([]string, 0, len(target.Filesystem.Mounts))
		for _, m := range target.Filesystem.Mounts {
			paths = append(paths, m.Path)
		}
		a.block(NoticeDeviceMounted, "设备正在挂载："+strings.Join(paths, "、"))
	}
	if target.Swap != nil {
		a.block(NoticeSwapActive, "设备是活动交换区（"+target.Swap.Type+"，已用 "+
			humanBytes(target.Swap.UsedBytes)+"）")
	}

	// holders：有 dm/md 在消费它，说明它已经是别人的“成员”。
	if len(target.Holders) > 0 {
		a.block(NoticeHasHolders, "有设备正依赖它："+strings.Join(target.Holders, "、"))
	}

	// 繁忙：有进程打开着设备节点。格式化一块被人 mmap 着的盘会毁掉正在运行的服务，
	// 所以这里是阻塞项而不是告警。整盘也要查——分区表的写入落在整盘上。
	busyTargets := []string{target.ID}
	if disk := diskOf(index, target.ID); disk != "" && disk != target.ID {
		busyTargets = append(busyTargets, disk)
	}
	for _, id := range busyTargets {
		if blocker, ok := busyBlocker(ctx, id, env); !ok {
			a.appendBlocker(blocker)
		}
	}

	// 按动作做各自的判断。
	switch in.action {
	case ActionPartitionTableInitialize:
		assessInitialize(&a, target, index, in, ctx, env)
	case ActionPartitionCreate:
		assessCreate(&a, target, index, in, ctx, env)
	case ActionPartitionDelete:
		assessDelete(&a, target, index, in, ctx, env)
	case ActionFilesystemFormat:
		assessFormat(&a, target, index, in, ctx, env)
	}

	a.finish()
	return a
}

// assessInitialize 判断“初始化 GPT”。
//
// 允许的条件来自 PLAN.md 第 9.1 节：没有分区、没有文件系统签名、没有其他占用的空白盘。
// 判不出来（读不到分区表、读不到签名、只有 sysfs 的降级信息）一律拒绝。
func assessInitialize(a *Assessment, target *Device, index topologyIndex, in assessmentInput, ctx context.Context, env Env) {
	if !ValidTableType(in.tableType) {
		a.block(NoticeTableUnknown, fmt.Sprintf("不支持的分区表类型 %q（只支持 gpt）", in.tableType))
		// 继续往下判：把“这块盘现在是什么状态”一并说清楚。
	}
	if target.Kind != KindDisk {
		a.block(NoticeUnsupportedKind, "只有整盘才能初始化分区表，当前目标是"+kindLabel(target.Kind))
		a.impact(ImpactDataLoss, "初始化分区表会覆盖盘上原有的分区元数据")
		return
	}

	// 分区表必须是“没有”，而且是**读了盘**得出的“没有”。
	table := target.PartitionTable
	switch {
	case table == nil:
		a.block(NoticeTableUnknown, "采不到这块盘的分区表信息")
	case table.Type == TableGPT, table.Type == TableMBR:
		a.block(NoticeTablePresent,
			"盘上已有 "+strings.ToUpper(table.Type)+" 分区表，初始化会覆盖它（不自动清除已有分区表）")
	case table.Type == TableUnknown:
		a.block(NoticeTableUnknown, "盘上首扇区的内容不是已知分区表，也不是全零，不能当作空白盘")
	}
	if table != nil && table.Source != "device" {
		a.block(NoticeTableUnreadable,
			"没有读到盘上的分区表（"+tableSourceLabel(table.Source)+"），无法证明它是空白盘")
	}
	if table != nil && len(table.Partitions) > 0 {
		ids := make([]string, 0, len(table.Partitions))
		for _, p := range table.Partitions {
			ids = append(ids, p.DeviceID)
		}
		a.block(NoticeTablePresent, "内核当前认出 "+strconv.Itoa(len(ids))+" 个分区："+strings.Join(ids, "、"))
	}
	if table != nil && table.Warning != "" {
		a.warn(NoticeTableWarning, table.Warning)
	}

	// 签名：盘上可能没有分区表却有文件系统（直接 mkfs 在整盘上），或者有 LVM/RAID/加密头。
	sig, ok := probeSignature(ctx, a, target, env)
	if ok {
		switch sig.Kind {
		case SignatureBlank:
		default:
			a.block(NoticeSignaturePresent,
				"盘上检测到 "+signatureLabel(sig)+"，初始化会覆盖它（不自动清除未知签名）")
		}
	}

	// 影响：PLAN.md 第 9.2 节要求初始化页面明确写出“覆盖分区元数据”的影响，
	// 并且不能用“没检测到签名”暗示盘上一定没有数据。
	a.impact(ImpactDataLoss,
		"将覆盖盘上的分区元数据（GPT 主表、备份表与分区条目）；未检测到已知签名不代表盘上绝无原始数据")
	a.impact(ImpactTableChange, "盘上会写入新的 GPT 分区表")
}

// assessCreate 判断“在 GPT 空闲区里建分区”。
func assessCreate(a *Assessment, target *Device, index topologyIndex, in assessmentInput, ctx context.Context, env Env) {
	if target.Kind != KindDisk {
		a.block(NoticeUnsupportedKind, "分区只能建在整盘上，当前目标是"+kindLabel(target.Kind))
		return
	}
	table := target.PartitionTable
	if table == nil {
		a.block(NoticeTableUnknown, "采不到这块盘的分区表信息")
		return
	}
	if table.Type != TableGPT {
		a.block(NoticeTableNotGPT,
			"只支持在 GPT 空闲区里建分区（当前 "+tableTypeLabel(table.Type)+"）；初期不修改 MBR 分区表")
	}
	if table.Source != "device" {
		a.block(NoticeTableUnreadable,
			"没有读到盘上的 GPT 分区表（"+tableSourceLabel(table.Source)+"），算不出空闲区")
	}
	if len(table.FreeRegions) == 0 {
		a.block(NoticeNoFreeRegion, "这块盘上没有可用的空闲区")
	}
	if in.name != "" {
		if err := ValidatePartitionName(in.name); err != nil {
			a.block(NoticeLabelInvalid, err.Error())
		}
	}
	// 区间为 nil 表示还没有具体请求（能力视图）：到这里只回答“这块盘现在能不能建分区”。
	if in.region == nil {
		a.impact(ImpactTableChange, "会在空闲区中写入一条新的 GPT 分区条目")
		return
	}

	start, size := in.region.StartBytes, in.region.SizeBytes
	if start%AlignBytes != 0 || size%AlignBytes != 0 {
		a.block(NoticeRegionUnaligned,
			fmt.Sprintf("起点与大小必须按 1 MiB（%d 字节）对齐，收到 start=%d size=%d",
				AlignBytes, start, size))
	}
	if size < MinPartitionBytes {
		a.block(NoticeRegionTooSmall,
			fmt.Sprintf("分区至少 %d 字节，收到 %d 字节", MinPartitionBytes, size))
	}
	if size > target.SizeBytes {
		a.block(NoticeRegionNotFree, "请求的大小超过整盘容量")
	}
	if !regionInsideFree(table.FreeRegions, start, size) {
		a.block(NoticeRegionNotFree,
			"请求的区间不在任何一块空闲区里（会与已有分区重叠或越界）")
	}
	a.impact(ImpactTableChange,
		fmt.Sprintf("会在 %d—%d 字节区间写入一条新的 GPT 分区条目（占用 %s）",
			start, start+size, humanBytes(size)))
}

// assessDelete 判断“删除分区”。
//
// PLAN.md 第 9.1 节：删除前检查文件系统、挂载、swap、holders、LVM/RAID、应用、共享
// 与备份依赖。本阶段覆盖前四项与整盘保护；应用/共享/备份的依赖在 P08/P11/P17 接入。
func assessDelete(a *Assessment, target *Device, index topologyIndex, in assessmentInput, ctx context.Context, env Env) {
	if target.Kind != KindPartition {
		a.block(NoticeUnsupportedKind, "只有分区能删除，当前目标是"+kindLabel(target.Kind))
		return
	}
	diskID := diskOf(index, target.ID)
	disk := index.devices[diskID]
	if disk == nil {
		a.block(NoticeParentUnknown, "找不到这个分区所属的整盘")
		return
	}
	if _, ok := partitionEntry(disk, target.ID); !ok {
		a.block(NoticePartitionMissing,
			target.ID+" 不在整盘的分区列表里（可能分区表刚被改动），不按旧信息删除")
	}
	if disk.PartitionTable == nil {
		a.block(NoticeTableUnknown, "采不到整盘的分区表信息")
	} else {
		if disk.PartitionTable.Type != TableGPT {
			a.block(NoticeTableNotGPT,
				"只支持修改 GPT 分区表（当前 "+tableTypeLabel(disk.PartitionTable.Type)+"）；初期不修改 MBR 分区表")
		}
		if disk.PartitionTable.Source != "device" {
			a.block(NoticeTableUnreadable,
				"没有读到盘上的分区表（"+tableSourceLabel(disk.PartitionTable.Source)+"），无法确认分区条目")
		}
		if disk.PartitionTable.Warning != "" {
			a.warn(NoticeTableWarning, disk.PartitionTable.Warning)
		}
	}

	// 签名同样要看：分区上有 LUKS/LVM/RAID 头时，它属于别的层，不能当普通分区删。
	if sig, ok := probeSignature(ctx, a, target, env); ok {
		switch {
		case signatureBlocksFormat(sig.Kind):
			a.block(NoticeSignaturePresent,
				"分区上有 "+signatureLabel(sig)+"，它属于加密/LVM/RAID 层，不能当作普通分区删除")
		case signatureHasData(sig.Kind):
			// 包括认不出来的非零内容：删掉分区一样会让那部分数据无法访问。
			a.warn(NoticeExistingData, "分区上检测到 "+signatureLabel(sig)+"，删除分区会让这块数据无法访问")
			a.impact(ImpactDataLoss, "删除分区会丢弃它的分区条目，盘上的数据将无法直接访问")
		}
	}
	a.impact(ImpactTableChange, "会从 GPT 分区表里移除 "+target.ID+" 这一条")
}

// assessFormat 判断“格式化一个普通分区”。
func assessFormat(a *Assessment, target *Device, index topologyIndex, in assessmentInput, ctx context.Context, env Env) {
	if target.Kind != KindPartition {
		a.block(NoticeUnsupportedKind,
			"只支持格式化分区，不支持对整盘直接 mkfs（当前目标是"+kindLabel(target.Kind)+"）")
		return
	}
	if disk := index.devices[diskOf(index, target.ID)]; disk != nil {
		if _, ok := partitionEntry(disk, target.ID); !ok {
			a.block(NoticePartitionMissing,
				target.ID+" 不在整盘的分区列表里（可能分区表刚被改动），不按旧信息格式化")
		}
	}
	if in.filesystem != "" && !ValidFilesystem(in.filesystem) {
		a.block(NoticeUnsupportedKind,
			fmt.Sprintf("只支持 ext4 与 xfs，收到 %q", in.filesystem))
	}
	if in.filesystem != "" {
		if err := ValidateLabel(in.filesystem, in.label); err != nil {
			a.block(NoticeLabelInvalid, err.Error())
		}
	}

	sig, ok := probeSignature(ctx, a, target, env)
	if ok {
		switch {
		case signatureBlocksFormat(sig.Kind):
			a.block(NoticeSignaturePresent,
				"分区上有 "+signatureLabel(sig)+"，说明它属于加密/LVM/RAID 层，不能直接格式化")
		case signatureHasData(sig.Kind):
			// 认不出来的非零内容也算“有数据”：格式化会把它抹掉，必须让人看见。
			a.warn(NoticeExistingData,
				"分区上检测到 "+signatureLabel(sig)+"，格式化会清除它")
			a.impact(ImpactDataLoss, "分区上原有的 "+signatureLabel(sig)+" 数据会被清除，不可恢复")
		}
	}
	if in.filesystem != "" {
		a.impact(ImpactDataLoss,
			"会把 "+target.ID+" 重新格式化为 "+in.filesystem+"，原有内容全部丢失")
	}
}

// topologyIndex 是拓扑的查询索引。
type topologyIndex struct {
	devices map[string]*Device
	// parents 是 child → 父设备（沿 partition_of / depends_on 向上）。
	parents map[string][]string
	order   []string
}

func indexTopology(topo Topology) topologyIndex {
	idx := topologyIndex{
		devices: make(map[string]*Device, len(topo.Nodes)),
		parents: map[string][]string{},
		order:   make([]string, 0, len(topo.Nodes)),
	}
	for i := range topo.Nodes {
		dev := &topo.Nodes[i]
		idx.devices[dev.ID] = dev
		idx.order = append(idx.order, dev.ID)
	}
	for _, e := range topo.Edges {
		idx.parents[e.FromDeviceID] = append(idx.parents[e.FromDeviceID], e.ToDeviceID)
	}
	for k := range idx.parents {
		sort.Strings(idx.parents[k])
	}
	return idx
}

// chain 返回目标自身与它所有的祖先设备（父设备、父设备的父设备……）。
//
// 用 visited 防环：拓扑来自内核，正常不会成环，但一旦成环（夹具写错、驱动异常），
// 递归会直接卡死采集循环。这里宁可少走一条边也不要死循环。
// 第二个返回值是这条链上的整块物理盘 id（去重、排序）；第三个是“链是否成立”。
func (idx topologyIndex) chain(id string) ([]*Device, []string, bool) {
	visited := map[string]bool{}
	queue := []string{id}
	chain := make([]*Device, 0, 4)
	disks := map[string]bool{}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if visited[cur] {
			continue
		}
		visited[cur] = true
		dev, ok := idx.devices[cur]
		if !ok {
			continue
		}
		chain = append(chain, dev)
		if dev.Kind == KindDisk {
			disks[dev.ID] = true
		}
		queue = append(queue, idx.parents[cur]...)
	}
	if len(chain) == 0 {
		return nil, nil, false
	}
	list := make([]string, 0, len(disks))
	for d := range disks {
		list = append(list, d)
	}
	sort.Strings(list)
	return chain, list, true
}

// diskOf 返回一台设备所属的第一块物理盘（没有时返回空串）。
func diskOf(idx topologyIndex, id string) string {
	_, disks, ok := idx.chain(id)
	if !ok || len(disks) == 0 {
		return ""
	}
	return disks[0]
}

// fingerprintClash 找出与目标共用身份指纹的其它设备。
func (idx topologyIndex) fingerprintClash(target *Device) []string {
	if target.IdentityFingerprint == "" {
		return nil
	}
	out := []string{}
	for _, id := range idx.order {
		if id == target.ID {
			continue
		}
		other := idx.devices[id]
		if other.IdentityFingerprint == target.IdentityFingerprint {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// targetIdentity 给出目标的身份指纹。
//
// 分区的身份必须由**整盘身份 + 起始位置**合成：内核不给分区独立序列号，sysfs 里
// 分区目录也没有 device/serial，直接取 Device.IdentityFingerprint 会得到
// “unknown（name:sdb1）”——那既区分不了两块盘上的同名分区，也挡不住“换盘后
// sdb1 还是 sdb1”这种情况。用整盘指纹加分区起点之后：
//   - 换盘（不同物理盘）→ 指纹不同；
//   - 分区表被重建、1 号分区换了位置 → 指纹不同。
func targetIdentity(target *Device, idx topologyIndex) string {
	if target.Kind != KindPartition {
		return target.IdentityFingerprint
	}
	diskID := diskOf(idx, target.ID)
	disk := idx.devices[diskID]
	if disk == nil {
		return target.IdentityFingerprint
	}
	start := uint64(0)
	if entry, ok := partitionEntry(disk, target.ID); ok {
		start = entry.StartBytes
	}
	return partitionFingerprint(disk.IdentityFingerprint, disk.ID, start)
}

// partitionEntry 在整盘的分区列表里找一台分区。
func partitionEntry(disk *Device, id string) (Partition, bool) {
	if disk == nil || disk.PartitionTable == nil {
		return Partition{}, false
	}
	for _, p := range disk.PartitionTable.Partitions {
		if p.DeviceID == id {
			return p, true
		}
	}
	return Partition{}, false
}

// partitionFingerprint 合成分区的身份指纹。
func partitionFingerprint(diskFingerprint, diskID string, startBytes uint64) string {
	sum := sha256.Sum256([]byte("upanel-partition-v1|" + diskFingerprint + "|" + diskID + "|" +
		strconv.FormatUint(startBytes, 10)))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// protectionReasons 汇总链上所有设备的保护理由（去重、排序）。
func protectionReasons(chain []*Device) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, d := range chain {
		if !d.Protection.SystemDisk {
			continue
		}
		for _, r := range d.Protection.Reasons {
			if seen[r] {
				continue
			}
			seen[r] = true
			out = append(out, r)
		}
	}
	sort.Strings(out)
	return out
}

// busyBlocker 查一台设备是否被进程打开。
//
// 返回 (blocker, true) 表示“不能判定或确实被占”，调用方据此记一条阻塞项。
func busyBlocker(ctx context.Context, devID string, env Env) (Blocker, bool) {
	if env.Busy == nil {
		return Blocker{
			Code:    NoticeBusy,
			Message: "没有可用的占用检查，无法确认 " + devID + " 是否正被进程使用",
		}, false
	}
	procs, err := env.Busy.Scan(ctx, devID)
	if err != nil {
		return Blocker{
			Code:    NoticeBusy,
			Message: "检查 " + devID + " 的占用情况失败：" + err.Error(),
		}, false
	}
	if len(procs) == 0 {
		return Blocker{}, true
	}
	return Blocker{
		Code:    NoticeBusy,
		Message: "有进程正打开 " + devID + "：" + stringsJoinPIDs(procs, 5),
	}, false
}

// probeSignature 读设备签名。返回 (signature, true) 表示读到了可信结论；
// 读失败时已经往阻塞项里记了一条，调用方不用再管。
func probeSignature(ctx context.Context, a *Assessment, dev *Device, env Env) (Signature, bool) {
	if env.Signatures == nil {
		a.block(NoticeSignatureUnreadable, "没有可用的签名读取器，无法证明设备上没有文件系统或卷签名")
		return Signature{}, false
	}
	sig, err := env.Signatures.Probe(ctx, dev.ID, dev.SizeBytes)
	if err != nil {
		a.block(NoticeSignatureUnreadable, "读取 "+dev.ID+" 的签名失败："+err.Error())
		return Signature{}, false
	}
	return sig, true
}

// regionInsideFree 判断 [start, start+size) 是否完整落在一块空闲区里。
func regionInsideFree(free []FreeRegion, start, size uint64) bool {
	if size == 0 {
		return false
	}
	end := start + size
	if end < start { // 溢出
		return false
	}
	for _, f := range free {
		fEnd := f.StartBytes + f.SizeBytes
		if fEnd < f.StartBytes {
			continue
		}
		if start >= f.StartBytes && end <= fEnd {
			return true
		}
	}
	return false
}

// block 记一条阻塞项。
func (a *Assessment) block(code, message string) {
	a.appendBlocker(Blocker{Code: code, Message: message})
}

func (a *Assessment) appendBlocker(b Blocker) {
	for _, cur := range a.Blockers {
		if cur.Code == b.Code && cur.Message == b.Message {
			return
		}
	}
	a.Blockers = append(a.Blockers, b)
}

func (a *Assessment) warn(code, message string) {
	for _, cur := range a.Warnings {
		if cur.Code == code && cur.Message == message {
			return
		}
	}
	a.Warnings = append(a.Warnings, Blocker{Code: code, Message: message})
}

func (a *Assessment) impact(kind, description string) {
	for _, cur := range a.Impacts {
		if cur.Kind == kind && cur.Description == description {
			return
		}
	}
	a.Impacts = append(a.Impacts, Impact{Kind: kind, Description: description})
}

// finish 收尾：定结论、排序锁。
func (a *Assessment) finish() {
	a.Allowed = len(a.Blockers) == 0
	sort.Strings(a.Locks)
	if a.Blockers == nil {
		a.Blockers = []Blocker{}
	}
	if a.Warnings == nil {
		a.Warnings = []Blocker{}
	}
	if a.Impacts == nil {
		a.Impacts = []Impact{}
	}
}

// displayNameFor 给出目标给人的名字：型号、容量、序列号，便于在确认框里核对。
func displayNameFor(dev *Device, idx topologyIndex) string {
	var b strings.Builder
	b.WriteString(dev.Name)
	b.WriteString("（")
	b.WriteString(humanBytes(dev.SizeBytes))
	if dev.Kind == KindPartition {
		diskID := diskOf(idx, dev.ID)
		if disk := idx.devices[diskID]; disk != nil {
			b.WriteString("，")
			b.WriteString(diskID)
			b.WriteString(" 上的分区")
		}
	}
	if dev.Serial != nil && *dev.Serial != "" {
		b.WriteString("，序列号 ")
		b.WriteString(*dev.Serial)
	} else if dev.Model != nil && *dev.Model != "" {
		b.WriteString("，")
		b.WriteString(*dev.Model)
	}
	b.WriteString("）")
	return b.String()
}

func kindLabel(kind string) string {
	switch kind {
	case KindDisk:
		return "整盘"
	case KindPartition:
		return "分区"
	case KindLVM:
		return "LVM 逻辑卷"
	case KindRAID:
		return "软 RAID"
	case KindCrypt:
		return "加密设备"
	case KindLoop:
		return "回环设备"
	case KindROM:
		return "光驱"
	default:
		return "其它类型设备"
	}
}

func tableTypeLabel(t string) string {
	switch t {
	case TableGPT:
		return "GPT"
	case TableMBR:
		return "MBR"
	case TableNone:
		return "没有分区表"
	default:
		return "未知类型"
	}
}

func tableSourceLabel(src string) string {
	switch src {
	case "device":
		return "读了盘"
	case "kernel":
		return "只用了内核的分区列表"
	case "skipped":
		return "跳过了读盘"
	default:
		return src
	}
}

func signatureLabel(sig Signature) string {
	switch sig.Kind {
	case SignatureExt:
		return "ext 系列文件系统签名"
	case SignatureXFS:
		return "XFS 签名"
	case SignatureBtrfs:
		return "Btrfs 签名"
	case SignatureVFAT:
		return "FAT 签名"
	case SignatureNTFS:
		return "NTFS 签名"
	case SignatureExFAT:
		return "exFAT 签名"
	case SignatureSwap:
		return "交换区签名"
	case SignatureLUKS:
		return "LUKS 加密头"
	case SignatureLVM2:
		return "LVM2 物理卷标签"
	case SignatureMD:
		return "软 RAID 超级块"
	case SignatureBlank:
		return "没有已知签名"
	default:
		return "无法识别的签名"
	}
}

// humanBytes 把字节数说成人话（只用于提示文本，不参与判断）。
func humanBytes(n uint64) string {
	const (
		kib = 1024
		mib = 1024 * kib
		gib = 1024 * mib
		tib = 1024 * gib
	)
	switch {
	case n >= tib:
		return fmt.Sprintf("%.1f TiB", float64(n)/float64(tib))
	case n >= gib:
		return fmt.Sprintf("%.1f GiB", float64(n)/float64(gib))
	case n >= mib:
		return fmt.Sprintf("%.1f MiB", float64(n)/float64(mib))
	case n >= kib:
		return fmt.Sprintf("%.1f KiB", float64(n)/float64(kib))
	default:
		return strconv.FormatUint(n, 10) + " 字节"
	}
}
