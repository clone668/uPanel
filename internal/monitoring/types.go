// Package monitoring 采集系统基础资源：CPU、内存、负载、网络与主机信息（P04.1）。
//
// 采集全部只读，来源是 /proc、/sys 与 /etc/os-release，不执行任何外部命令：
// 既不需要命令执行器，也就没有命令注入面（PLAN.md 第 8 节）。
//
// 本包有三条硬规则，直接对应 P04 的验收条件：
//
//   - 不解析本地化的列对齐文本。用到的文件都是内核固定的“名字: 值”或
//     “名字=值”结构，与 locale 无关。
//   - 不用零值冒充失败。某个段采集失败时，段的 status 变成 error/unavailable，
//     值保持上一次成功的（或 null），绝不给出一份看起来正常的 0。
//   - 计数型指标只报差值与时间窗。没有前一次样本、计数器归零或设备出现在
//     两次采样之间时，速率是 null 而不是 0，也不能是负数。
package monitoring

import "time"

// 采集段状态（闭集）。
const (
	// StatusOK 表示最近一次采集成功。
	StatusOK = "ok"
	// StatusError 表示数据源存在但这次读/解析失败。
	StatusError = "error"
	// StatusUnavailable 表示数据源在这台机器上不存在（例如受限容器里没有某个 /proc 文件）。
	StatusUnavailable = "unavailable"
)

// Section 是每个采集段共有的部分。
//
// Status 是**最近一次采集尝试**的结果；CollectedAt 是段里当前这份值的时间。
// 两者刻意分开：最近一次失败时，段保留上一次成功的值并按 PLAN.md 第 8 节
// “监控展示最近有效值及其时间”如实给出旧时间，而不是清空成 0。
// 没有成功过时值为 null、CollectedAt 也为 null。
type Section struct {
	Status      string     `json:"status"`
	Error       string     `json:"error,omitempty"`
	Source      string     `json:"source,omitempty"`
	CollectedAt *time.Time `json:"collected_at,omitempty"`
}

// Summary 是 system.summary 查询的结果，也是 GET /api/v1/system/summary 的响应体。
type Summary struct {
	// CollectedAt 是这份快照的采集（尝试）时间，UTC。
	CollectedAt time.Time `json:"collected_at"`
	// IntervalMS 是采集循环的间隔，便于前端解释“多久之前的数”。
	IntervalMS int64 `json:"interval_ms"`
	// Stale 表示快照时间已经超过 3 个采集间隔：采集循环卡住或代理刚从挂起中恢复。
	// 它不为 true 只说明“这份快照够新”，不说明各段都采集成功（那看 Degraded）。
	Stale bool `json:"stale"`
	// Degraded 表示至少有一个段的 status 不是 ok。
	Degraded bool `json:"degraded"`

	Host    HostSection    `json:"host"`
	CPU     CPUSection     `json:"cpu"`
	Memory  MemorySection  `json:"memory"`
	Load    LoadSection    `json:"load"`
	Network NetworkSection `json:"network"`
}

// HostSection 是主机与系统信息（/etc/os-release、/proc/sys/kernel/*、/proc/uptime）。
type HostSection struct {
	Section
	Hostname      string `json:"hostname,omitempty"`
	OSID          string `json:"os_id,omitempty"`
	OSName        string `json:"os_name,omitempty"`
	OSVersion     string `json:"os_version,omitempty"`
	OSPrettyName  string `json:"os_pretty_name,omitempty"`
	KernelRelease string `json:"kernel_release,omitempty"`
	// Arch 是内核报告的机器架构（uname -m 的等价物）。
	Arch string `json:"arch,omitempty"`
	// BootID 是本轮启动的标识：换了一次启动它就会变，用来判断计数器是否失效。
	BootID string `json:"boot_id,omitempty"`
	// UptimeSeconds 来自 /proc/uptime；读不到时为 null。
	UptimeSeconds *float64 `json:"uptime_seconds"`
}

// CPUSection 是 CPU 使用率与规模。
type CPUSection struct {
	Section
	// Model 来自 /proc/cpuinfo（x86 的 model name / ARM 的 Hardware）。
	// 读不到就省略：它不是判断 CPU 是否采集成功的依据。
	Model string `json:"model,omitempty"`
	// LogicalCores 是 /proc/stat 里 cpuN 行的数量。
	LogicalCores int `json:"logical_cores"`
	// UsagePercent 是所有核的合计使用率。没有前一次样本时为 null（不是 0）。
	UsagePercent *float64 `json:"usage_percent"`
	// SampleWindowMS 是 UsagePercent 对应的采样时间窗；与 UsagePercent 同时为 null。
	SampleWindowMS *int64 `json:"sample_window_ms,omitempty"`
	// PerCPU 按核给出使用率；CPU 热插拔导致索引对不上时，对应核为 null。
	PerCPU []CPUCore `json:"per_cpu,omitempty"`
}

// CPUCore 是单个逻辑核的使用率。
type CPUCore struct {
	Index        int      `json:"index"`
	UsagePercent *float64 `json:"usage_percent"`
}

// MemorySection 是内存与 swap（/proc/meminfo）。单位一律字节。
//
// 字节数与计数器在 JSON 里是**十进制字符串**（`json:",string"`），与
// CONTRACTS.md 第 1 节一致：前端不能用 Number 做精确运算，2^53 之后会悄悄丢位。
// 百分比、负载、核数、毫秒这些量级安全的字段仍是数字。
type MemorySection struct {
	Section
	TotalBytes     *uint64 `json:"total_bytes,string"`
	FreeBytes      *uint64 `json:"free_bytes,string"`
	AvailableBytes *uint64 `json:"available_bytes,string"`
	BuffersBytes   *uint64 `json:"buffers_bytes,string"`
	CachedBytes    *uint64 `json:"cached_bytes,string"`
	// UsedBytes 是 Total - Available（可用内存口径），不是 Total - Free。
	UsedBytes      *uint64 `json:"used_bytes,string"`
	SwapTotalBytes *uint64 `json:"swap_total_bytes,string"`
	SwapFreeBytes  *uint64 `json:"swap_free_bytes,string"`
}

// LoadSection 是负载（/proc/loadavg）。
type LoadSection struct {
	Section
	Load1    *float64 `json:"load1"`
	Load5    *float64 `json:"load5"`
	Load15   *float64 `json:"load15"`
	Runnable *uint64  `json:"runnable"`
	Threads  *uint64  `json:"threads"`
}

// NetworkSection 是网卡计数器（/proc/net/dev + /sys/class/net/*/operstate）。
type NetworkSection struct {
	Section
	// Interfaces 按名字排序，循环设备也列出（标 loopback=true），不做主观裁剪。
	Interfaces []NetInterface `json:"interfaces,omitempty"`
}

// NetInterface 是一块网卡。
type NetInterface struct {
	Name      string `json:"name"`
	OperState string `json:"oper_state,omitempty"`
	Loopback  bool   `json:"loopback"`

	RXBytes   uint64 `json:"rx_bytes,string"`
	TXBytes   uint64 `json:"tx_bytes,string"`
	RXPackets uint64 `json:"rx_packets,string"`
	TXPackets uint64 `json:"tx_packets,string"`
	RXErrors  uint64 `json:"rx_errors,string"`
	TXErrors  uint64 `json:"tx_errors,string"`
	RXDropped uint64 `json:"rx_dropped,string"`
	TXDropped uint64 `json:"tx_dropped,string"`

	// 速率是按相邻两次计数器差算出来的。以下情况为 null（不是 0）：
	// 还没有前一次样本、两次采样之间的时间窗不为正、计数器变小（重置/换设备）。
	RXBytesPerSecond *float64 `json:"rx_bytes_per_second"`
	TXBytesPerSecond *float64 `json:"tx_bytes_per_second"`
}

// anyDegraded 判断是否有段不在 ok 状态。
func anyDegraded(sum Summary) bool {
	for _, s := range []string{sum.Host.Status, sum.CPU.Status, sum.Memory.Status, sum.Load.Status, sum.Network.Status} {
		if s != StatusOK {
			return true
		}
	}
	return false
}

// cloneSummary 返回一份深拷贝。
//
// 快照会给调用方（查询处理器、Web 客户端）带出去，调用方之间不能共享切片与指针，
// 否则一个阅读者的序列化会看到另一个人的半途修改。
func cloneSummary(in Summary) Summary {
	out := in
	out.Host.Section = cloneSection(in.Host.Section)
	out.CPU = cloneCPUSection(in.CPU)
	out.Memory = cloneMemorySection(in.Memory)
	out.Load = cloneLoadSection(in.Load)
	out.Network = cloneNetworkSection(in.Network)
	return out
}

func cloneTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := *t
	return &v
}

func cloneFloat(f *float64) *float64 {
	if f == nil {
		return nil
	}
	v := *f
	return &v
}

func cloneUint(v *uint64) *uint64 {
	if v == nil {
		return nil
	}
	x := *v
	return &x
}

func cloneSection(s Section) Section {
	s.CollectedAt = cloneTime(s.CollectedAt)
	return s
}

func cloneCPUSection(s CPUSection) CPUSection {
	out := s
	out.Section = cloneSection(s.Section)
	out.UsagePercent = cloneFloat(s.UsagePercent)
	if s.SampleWindowMS != nil {
		v := *s.SampleWindowMS
		out.SampleWindowMS = &v
	}
	if s.PerCPU != nil {
		out.PerCPU = make([]CPUCore, len(s.PerCPU))
		for i, core := range s.PerCPU {
			out.PerCPU[i] = CPUCore{Index: core.Index, UsagePercent: cloneFloat(core.UsagePercent)}
		}
	}
	return out
}

func cloneMemorySection(s MemorySection) MemorySection {
	out := s
	out.Section = cloneSection(s.Section)
	out.TotalBytes = cloneUint(s.TotalBytes)
	out.FreeBytes = cloneUint(s.FreeBytes)
	out.AvailableBytes = cloneUint(s.AvailableBytes)
	out.BuffersBytes = cloneUint(s.BuffersBytes)
	out.CachedBytes = cloneUint(s.CachedBytes)
	out.UsedBytes = cloneUint(s.UsedBytes)
	out.SwapTotalBytes = cloneUint(s.SwapTotalBytes)
	out.SwapFreeBytes = cloneUint(s.SwapFreeBytes)
	return out
}

func cloneLoadSection(s LoadSection) LoadSection {
	out := s
	out.Section = cloneSection(s.Section)
	out.Load1 = cloneFloat(s.Load1)
	out.Load5 = cloneFloat(s.Load5)
	out.Load15 = cloneFloat(s.Load15)
	out.Runnable = cloneUint(s.Runnable)
	out.Threads = cloneUint(s.Threads)
	return out
}

func cloneNetworkSection(s NetworkSection) NetworkSection {
	out := s
	out.Section = cloneSection(s.Section)
	if s.Interfaces != nil {
		out.Interfaces = make([]NetInterface, len(s.Interfaces))
		for i, iface := range s.Interfaces {
			iface.RXBytesPerSecond = cloneFloat(iface.RXBytesPerSecond)
			iface.TXBytesPerSecond = cloneFloat(iface.TXBytesPerSecond)
			out.Interfaces[i] = iface
		}
	}
	return out
}
