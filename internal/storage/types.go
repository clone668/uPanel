// Package storage 采集块设备拓扑（P04.2）：物理盘、分区、LVM/RAID/crypt 逻辑设备、
// 挂载与容量、交换区，以及设备身份与系统盘保护判断。
//
// 采集全部只读：读 /sys/block、/proc/self/mountinfo、/proc/swaps、/dev/disk/by-*，
// 以及整盘最前面 34 个扇区（MBR + GPT 头 + 分区条目表）来判断分区表类型与空闲区。
// 不执行任何外部命令（不调用 lsblk/blkid/udevadm），因此没有命令注入面。
//
// 三条硬规则：
//
//   - 不靠设备名字猜关系。父子关系来自 sysfs 的目录嵌套、holders/slaves 与 dm/md
//     属性，不来自 "sda1 一定属于 sda" 这类假设；dm/md 的层级更不是名字能表达的。
//   - 不把“不知道”写成“正常”。健康状态在 P04.2 一律是 unknown（SMART 属 P04.3），
//     身份可信度在缺 WWN/序列号时如实降级，分区表读不出来时说 source=kernel 而不是编一个类型。
//   - 不为了拿到信息而唤醒磁盘。休眠或不可读的盘只报 sysfs 已有的信息，
//     分区表探测标记为 skipped，读数带超时，绝不让采集循环挂在 I/O 上。
package storage

import "time"

// 设备类型闭集（CONTRACTS.md 第 6 节）。
const (
	KindDisk      = "disk"
	KindPartition = "partition"
	KindLVM       = "lvm"
	KindRAID      = "raid"
	KindCrypt     = "crypt"
	KindLoop      = "loop"
	KindROM       = "rom"
	KindOther     = "other"
)

// 健康状态闭集（CONTRACTS.md 第 6 节）。P04.2 只产出 unknown，
// 其余取值由 P04.3 的 SMART/NVMe 解析填充。
const (
	HealthHealthy     = "healthy"
	HealthWarning     = "warning"
	HealthCritical    = "critical"
	HealthUnknown     = "unknown"
	HealthUnsupported = "unsupported"
	HealthSleeping    = "sleeping"
)

// 身份可信度闭集。它回答的是“这个指纹有多可信”，不是“这块盘好不好”。
const (
	// ConfidenceHigh：有 WWN 或序列号。同一块盘换槽位、换启动后仍然对得上。
	ConfidenceHigh = "high"
	// ConfidenceMedium：没有 sysfs 序列号，但有 udev 的 scsi-S… 稳定名（含 VPD 序列号）。
	ConfidenceMedium = "medium"
	// ConfidenceLow：只有型号+容量+拓扑地址。能把同一台机器上的两块同名盘区分开，
	// 但换槽位就认不出来了。
	ConfidenceLow = "low"
	// ConfidenceUnknown：连型号都没有（例如某些虚拟设备）。
	ConfidenceUnknown = "unknown"
)

// 分区表类型闭集。
const (
	TableGPT     = "gpt"
	TableMBR     = "mbr" // 也被称为 dos
	TableNone    = "none"
	TableUnknown = "unknown"
)

// TableSourceDevice 表示分区表类型与空闲区是**读了盘**得到的（其余取值见 PartitionTable.Source）。
//
// P07.2 起它是写后核验的前置条件：Source 不是 device 就说明盘上的表没读到，
// 这时既不能判断“盘上写着什么”，也就不能把写动作判成成功。
const TableSourceDevice = "device"

// 采集状态闭集（与 monitoring 包同一套语义，便于前端复用组件）。
const (
	StatusOK    = "ok"
	StatusError = "error"
)

// 拓扑关系闭集（CONTRACTS.md 第 6 节 TopologyEdge）。
const (
	// RelationPartitionOf：from 是 to 上的分区。
	RelationPartitionOf = "partition_of"
	// RelationDependsOn：from 在 to 之上（LVM/RAID/crypt/loop 依赖底层设备）。
	RelationDependsOn = "depends_on"
)

// Topology 是一次扫描的完整结果，也是代理查询 storage.topology 的返回值。
//
// Nodes 与 Edges 是同一份快照：节点里的 protection 与边是一致的，
// 不会出现“某个分区被标成系统盘的一部分，但它到磁盘的边却不在”的情况。
type Topology struct {
	// Status 是**最近一次扫描尝试**的结果。失败时 Nodes 保留上一次成功的快照，
	// CollectedAt 也停在上一次成功的时间——界面据此显示“多久之前的数据”。
	Status string `json:"status"`
	// Error 只在 Status=error 时出现，内容是失败原因（不含路径以外的敏感信息）。
	Error string `json:"error,omitempty"`
	// SnapshotVersion 是这份拓扑内容的指纹：内容不变则版本不变。
	// 写动作的预检会把它与执行前的版本对比（P07），因此它不能包含采集时间。
	SnapshotVersion string `json:"snapshot_version"`
	// CollectedAt 是这份快照的采集时间（UTC）。失败时停在上一次成功的时间。
	CollectedAt time.Time `json:"collected_at"`
	// IntervalMS 是扫描循环的间隔，供前端解释数据新旧。
	IntervalMS int64 `json:"interval_ms"`

	Nodes []Device `json:"nodes"`
	Edges []Edge   `json:"edges"`
	// NetworkMounts 是没有块设备的网络挂载（NFS/CIFS/sshfs……）。
	//
	// 它们没法挂到某个 Device 上——远端机器不在本机 sysfs 里——所以独立列出。
	// PLAN.md 第 8 节要的就是这个形态：网络挂载单独显示可达性与容量，
	// 不去推断远端硬件健康，也不和本地盘的容量混在一起。
	NetworkMounts []NetworkMount `json:"network_mounts"`
}

// NetworkMount 是一个没有块设备支撑的网络挂载点。
type NetworkMount struct {
	Path     string `json:"path"`
	Source   string `json:"source"`
	FSType   string `json:"fstype"`
	ReadOnly bool   `json:"read_only"`
	// Reachable 是对“远端还能不能用”的结论：
	//   - true：容量读到了；
	//   - false：查询明确失败（超时、statfs 报错）；
	//   - null：还没查过（代理刚启动、刚挂上，容量缓存里没有）。
	// 不能用 false 表示“不知道”：那会让界面把刚挂上的盘显示成断开。
	Reachable *bool `json:"reachable"`
	// Capacity 与设备挂载点同义：读不到就是 null，不是 0。
	Capacity *Capacity `json:"capacity"`
	// CapacityError 说明 capacity 为什么是 null（含“多久之后才会重试”）。
	CapacityError string `json:"capacity_error,omitempty"`
}

// Device 是一个块设备节点（含分区与逻辑设备）。
//
// 字节字段是十进制字符串（`json:",string"`，CONTRACTS.md 第 1 节）：
// 前端不能用 Number 做精确运算，2^53 之后会悄悄丢位。
type Device struct {
	// ID 是内核设备名（sda、sda1、dm-0……）。它在一次启动内唯一，用作 API 路径参数；
	// 判断“是不是同一块盘”要用 IdentityFingerprint，而不是这个。
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Name string `json:"name"`
	// SizeBytes 是设备容量（整盘或分区的大小），来自 sysfs 的扇区数 × 512。
	SizeBytes uint64 `json:"size_bytes,string"`
	// LogicalSectorBytes / PhysicalSectorBytes 来自 /sys/block/<dev>/queue。
	LogicalSectorBytes  int `json:"logical_sector_bytes"`
	PhysicalSectorBytes int `json:"physical_sector_bytes"`

	Model  *string `json:"model"`
	Serial *string `json:"serial"`
	// WWN 是 World Wide Name（sysfs 的 wwid）。虚拟盘上通常为空。
	WWN *string `json:"wwn"`
	// Transport 来自 sysfs 真实路径（virtio/nvme/usb/scsi/unknown），不看名字前缀。
	Transport *string `json:"transport"`
	// Rotational 表示介质是否旋转（HDD vs SSD）。虚拟盘上不可靠，仅作展示。
	Rotational bool `json:"rotational"`

	// ReadOnly 来自 sysfs 的 ro：设备本身被标记为只读（光盘、写保护开关）。
	ReadOnly bool `json:"read_only"`
	// Online 表示设备当前是否存在且没有报离线。休眠的盘仍然是 online。
	Online bool `json:"online"`

	// IdentityConfidence 是身份指纹的可信度；IdentityFingerprint 是对同一物理设备的稳定标识。
	IdentityConfidence  string `json:"identity_confidence"`
	IdentityFingerprint string `json:"identity_fingerprint"`

	Filesystem     *Filesystem     `json:"filesystem"`
	PartitionTable *PartitionTable `json:"partition_table"`
	Health         Health          `json:"health"`
	// Capabilities 列出这块设备当前**可以**执行的动作。P04.2 里恒为空：
	// 生产动作登记表是空的（写动作自 P07 起逐项登记），这里不能预告未实现的能力。
	Capabilities []Capability `json:"capabilities"`

	// Holders 是直接依赖这块设备的设备 id（sysfs 的 holders：dm/md 消费它）。
	// 与拓扑边重复，但详情页需要“谁占着它”这条最小信息时不必自己走图。
	Holders []string `json:"holders,omitempty"`
	// Swap 在这块设备作为活动交换区时非空（/proc/swaps）。
	Swap *SwapUsage `json:"swap,omitempty"`
	// Protection 是系统盘保护判断。它跟着设备走，列表页也能直接显示“受保护”标记。
	Protection Protection `json:"protection"`

	// Io 是这块设备自己的 I/O 计数与速率（/proc/diskstats 里它那一行）。
	// P04.3 起填充；读不到 /proc/diskstats 时为 null。
	// 每个设备只报自己的计数，从不相加——分区的 I/O 已经包含在整盘里。
	Io *IOStats `json:"io"`

	// CollectedAt 是这块设备信息所属的采集时间，与快照的 CollectedAt 相同。
	CollectedAt time.Time `json:"collected_at"`

	// counters 是本次采集读到的原始计数，只用于算下一次的速率。
	// 不导出、不入 JSON：它是宿主的内部状态，不该成为契约的一部分。
	counters    DiskCounters
	hasCounters bool
}

// Protection 是系统盘保护判断的结果。
//
// 判断不是“名字像不像系统盘”，而是从 /proc/self/mountinfo 与 /proc/swaps 出发，
// 找到承载根文件系统、/boot、EFI 分区、面板数据目录与交换区的设备，
// 再沿 partition_of / depends_on 递归向上，把所有底层盘一起标记。
// 于是“根在 LVM 上”“根在软 RAID 上”这类情况自然覆盖到物理盘。
type Protection struct {
	// SystemDisk 为 true 表示这块设备（或它的任一上层设备）承载了不能碰的东西。
	SystemDisk bool `json:"system_disk"`
	// Reasons 是人类可读的理由，去重且排序固定（便于测试与界面稳定展示）。
	Reasons []string `json:"reasons"`
	// Sources 是触发保护的具体设备 id（例如承载 / 的 sda1），便于解释“为什么”。
	Sources []string `json:"sources"`
}

// Filesystem 是设备上的文件系统信息。没有文件系统时为 null。
type Filesystem struct {
	// Type 来自挂载表；未挂载的设备为 null（P04.2 不调用 blkid，不猜）。
	Type  *string `json:"type"`
	UUID  *string `json:"uuid"`
	Label *string `json:"label"`
	// Mounts 是这块设备当前的挂载点（一个设备可以挂多处）。
	Mounts []Mount `json:"mounts"`
}

// Mount 是一个挂载点。
type Mount struct {
	Path    string   `json:"path"`
	Source  string   `json:"source"`
	FSType  string   `json:"fstype"`
	Options []string `json:"options"`
	// ReadOnly 来自挂载选项里的 ro（不是设备本身的只读标记）。
	ReadOnly bool `json:"read_only"`
	// Capacity 只在挂载点 statfs 成功时存在；失败时是 null，而不是 0。
	Capacity *Capacity `json:"capacity"`
	// CapacityError 说明为什么 capacity 是 null。
	//
	// 网络挂载不可达时这里是超时说明（含“多久之后才会重试”）。
	// 空字符串表示这一次没有报错——但 capacity 仍可能是 null（例如挂载点刚出现、
	// 容量还没采到）。界面不能把 null 当成“磁盘满了”。
	CapacityError string `json:"capacity_error,omitempty"`
}

// Capacity 是文件系统的容量与 inode 使用情况。
type Capacity struct {
	TotalBytes     uint64 `json:"total_bytes,string"`
	AvailableBytes uint64 `json:"available_bytes,string"`
	// UsedPercent = (total - available) / total × 100，保留一位小数。
	UsedPercent      float64 `json:"used_percent"`
	InodeTotal       uint64  `json:"inode_total,string"`
	InodeAvailable   uint64  `json:"inode_available,string"`
	InodeUsedPercent float64 `json:"inode_used_percent"`
	// CollectedAt 是这次 statfs 的时间。容量按 30 秒缓存（PLAN.md 第 8 节），
	// 因此它可能比拓扑的 CollectedAt 旧，页面要按这个时间展示。
	CollectedAt time.Time `json:"collected_at"`
}

// PartitionTable 是分区表信息。
type PartitionTable struct {
	Type string `json:"type"`
	// Partitions 来自 sysfs（内核当前认定的分区），不是从盘上读出来的——
	// 内核才是“现在存在哪些分区”的权威。
	Partitions  []Partition  `json:"partitions"`
	FreeRegions []FreeRegion `json:"free_regions"`
	// Source 说明类型与空闲区是怎么得到的：
	//   device       —— 读了盘上的 MBR/GPT；
	//   kernel       —— 只用了 sysfs 的分区列表，类型未知；
	//   skipped      —— 盘在休眠或读失败，没有去打扰它；
	//   none         —— 盘上没有分区表。
	Source string `json:"source"`
	// Warning 记录“盘上分区表与内核分区不一致”之类的情况，不隐藏。
	Warning string `json:"warning,omitempty"`
	// DiskGUID 是盘上 GPT 头里的 Disk GUID（读不到时为空串）。
	//
	// P07.2 起：写动作做完要把盘上的表**读回来**核对，Disk GUID 与逐条条目就是
	// 读回来的内容。它与 Partitions 是两个来源——一个来自盘上的字节，一个来自 sysfs。
	DiskGUID string `json:"disk_guid,omitempty"`
	// DiskEntries 是盘上 GPT 条目表的逐条解读。**只在读到 GPT 时出现**。
	//
	// 与 Partitions 并列而不是替代：Partitions 是内核现在认可的分区（有 /dev 节点、
	// 能挂载的那些），DiskEntries 是盘上写着什么。两者不一致时谁都不能被静默采信。
	//
	// 为什么是指针：一张刚初始化、还没有任何分区的 GPT 的条目表是**空数组**，
	// 而“没读到盘上的表”（kernel/skipped/MBR）根本没有这个键。用 []GPTEntry 加
	// omitempty 会把前者也省略掉，于是“读到了，零条”与“没读到”在 JSON 里变成同一件事
	// ——而验收正是用“零条”来证明初始化真的清空了条目表。
	DiskEntries *[]GPTEntry `json:"disk_entries,omitempty"`
}

// Partition 是一个分区。
type Partition struct {
	DeviceID string `json:"device_id"`
	Number   int    `json:"number"`
	// StartBytes / SizeBytes 从 sysfs 的 start/size（单位 512 字节扇区）换算成字节。
	StartBytes uint64 `json:"start_bytes,string"`
	SizeBytes  uint64 `json:"size_bytes,string"`
}

// FreeRegion 是分区表里未被占用的区间，用于让“还能建多大分区”有据可依。
type FreeRegion struct {
	StartBytes uint64 `json:"start_bytes,string"`
	SizeBytes  uint64 `json:"size_bytes,string"`
}

// Health 是设备健康。P04.2 只给出 unknown，SMART/NVMe 属 P04.3。
type Health struct {
	Status string `json:"status"`
	// TemperatureCelsius 为 null 表示没有采集，绝不是 0 °C。
	TemperatureCelsius *float64   `json:"temperature_celsius"`
	Source             string     `json:"source"`
	CheckedAt          *time.Time `json:"checked_at"`
	Details            *string    `json:"details"`
}

// Capability 是设备上可执行的一个动作。P04.2 的设备没有可执行动作（列表为空）。
type Capability struct {
	Action string `json:"action"`
	// State 取 available / blocked / unsupported。
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
}

// Edge 是拓扑里的一条关系（CONTRACTS.md 第 6 节 TopologyEdge）。
//
// 约定 from 是**子设备/消费者**，to 是**父设备/底层提供者**：
// sda1 --partition_of--> sda，dm-0 --depends_on--> sda2。
// 保护与锁沿这个方向递归向上追，不靠设备名字推断层级。
type Edge struct {
	FromDeviceID string `json:"from_device_id"`
	ToDeviceID   string `json:"to_device_id"`
	Relation     string `json:"relation"`
}

// SwapUsage 是活动交换区信息（/proc/swaps）。
type SwapUsage struct {
	Type      string `json:"type"`
	SizeBytes uint64 `json:"size_bytes,string"`
	UsedBytes uint64 `json:"used_bytes,string"`
	Priority  int    `json:"priority"`
}
