// Package metrics 保存并查询历史监控样本（P13.1；PLAN.md 第 18 节）。
//
// 分工很窄，也很明确：
//
//   - 采集仍然只在代理侧（internal/monitoring 与 internal/storage）。本包**不读
//     /proc**，也没有自己的采集间隔——它消费 Web 已经能拿到的两份快照，把它们变成
//     时序样本存进库里。
//   - 当前值任何时候都来自快照（GET /system/summary），历史只回答“过去怎么走的”。
//     两份数据不互相兜底：历史断了不会让首页显示旧值，快照失败也不会伪造历史。
//   - **缺失就是缺失**。某个段采集失败、代理暂时连不上、进程停了一小时——这段时间
//     在库里就是没有行。CONTRACTS.md 与 PLAN.md 第 18 节要求丢失的数据不能当零，
//     所以本包没有任何补零、插值或“用上一次的值填空”的路径。
//
// 三个粒度按 PLAN.md 第 18 节：原始 5 秒留 24 小时，1 分钟聚合留 30 天，
// 1 小时聚合留 365 天。聚合只处理**已经完整**的桶，重算是幂等的（见
// store.AggregateMetricBucket），因此重启、崩溃、时钟小幅回拨都能自愈。
package metrics

import (
	"time"

	"upanel/internal/monitoring"
	"upanel/internal/storage"
	"upanel/internal/store"
)

// 指标名。命名遵循 CONTRACTS.md 第 1 节：百分比带 _percent，速率带 _per_sec，
// 字节量带 _bytes。同一个名字在不同 scope 下有不同含义时以 scope 区分（库里
// 主键是 scope + target_id + metric + resolution + ts），但这里刻意不复用名字：
// filesystem 下的 used_percent 与 host 下的 memory_used_percent 一眼能分开。
const (
	// MetricCPUUsagePercent 是所有逻辑核合计的 CPU 使用率（0—100）。
	MetricCPUUsagePercent = "cpu_usage_percent"
	// MetricLoad1 / MetricLoad5 / MetricLoad15 是 /proc/loadavg 的三个负载值。
	MetricLoad1  = "load1"
	MetricLoad5  = "load5"
	MetricLoad15 = "load15"
	// MetricMemoryUsedPercent 是可用内存口径的使用率（Total-Available）/Total×100。
	MetricMemoryUsedPercent = "memory_used_percent"

	// MetricNetRXBytesPerSec / MetricNetTXBytesPerSec 是单块网卡的收发速率。
	MetricNetRXBytesPerSec = "rx_bytes_per_sec"
	MetricNetTXBytesPerSec = "tx_bytes_per_sec"

	// MetricFSUsedPercent 是挂载点的空间使用率，MetricFSInodeUsedPercent 是 inode 使用率。
	// PLAN.md 第 18 节的“空间与 inode 不足”告警各用一条（P13.2）。
	MetricFSUsedPercent      = "used_percent"
	MetricFSInodeUsedPercent = "inode_used_percent"
	// MetricFSAvailableBytes 是挂载点剩余字节。空间阈值支持“使用率”和“剩余 GiB”两种口径，
	// 后者必须查得到字节数，不能只靠百分比反推——120 GiB 的盘剩 12 GiB 和 4 TiB 的盘
	// 剩 12 GiB 是同一句“剩 12 GiB”，但对运维不是一回事。
	MetricFSAvailableBytes = "available_bytes"
)

// Definition 是一个登记过的指标：它是“产品支持哪些曲线”的唯一答案。
//
// 查询接口只接受这里出现过的名字（未知名字回 400 而不是空结果）：请求一个不存在的
// 指标却得到 200 加空数组，会让人以为“过去没有数据”，而真相是“这个指标根本不存在”。
type Definition struct {
	Scope       string `json:"scope"`
	Metric      string `json:"metric"`
	Unit        string `json:"unit"`
	Description string `json:"description"`
}

// 单位（闭集）。前端画图时的纵轴标题与格式都用它。
const (
	UnitPercent     = "percent"
	UnitBytesPerSec = "bytes_per_sec"
	UnitBytes       = "bytes"
	UnitCount       = "count"
)

// definitions 是登记表。顺序固定，接口与文档都按这个顺序输出。
var definitions = []Definition{
	{
		Scope: store.MetricScopeHost, Metric: MetricCPUUsagePercent, Unit: UnitPercent,
		Description: "所有逻辑核合计的 CPU 使用率（按相邻两次 /proc/stat 采样算出的窗口均值）",
	},
	{
		Scope: store.MetricScopeHost, Metric: MetricLoad1, Unit: UnitCount,
		Description: "1 分钟平均负载（/proc/loadavg）",
	},
	{
		Scope: store.MetricScopeHost, Metric: MetricLoad5, Unit: UnitCount,
		Description: "5 分钟平均负载",
	},
	{
		Scope: store.MetricScopeHost, Metric: MetricLoad15, Unit: UnitCount,
		Description: "15 分钟平均负载",
	},
	{
		Scope: store.MetricScopeHost, Metric: MetricMemoryUsedPercent, Unit: UnitPercent,
		Description: "内存使用率（(总量-可用)/总量×100，不是 (总量-空闲)/总量）",
	},
	{
		Scope: store.MetricScopeInterface, Metric: MetricNetRXBytesPerSec, Unit: UnitBytesPerSec,
		Description: "网卡接收速率（按相邻两次 /proc/net/dev 计数器差算出）",
	},
	{
		Scope: store.MetricScopeInterface, Metric: MetricNetTXBytesPerSec, Unit: UnitBytesPerSec,
		Description: "网卡发送速率",
	},
	{
		Scope: store.MetricScopeFilesystem, Metric: MetricFSUsedPercent, Unit: UnitPercent,
		Description: "挂载点空间使用率（statfs）",
	},
	{
		Scope: store.MetricScopeFilesystem, Metric: MetricFSInodeUsedPercent, Unit: UnitPercent,
		Description: "挂载点 inode 使用率（statfs）",
	},
	{
		Scope: store.MetricScopeFilesystem, Metric: MetricFSAvailableBytes, Unit: UnitBytes,
		Description: "挂载点剩余字节（普通用户可用口径，statfs 的 bavail×bsize）",
	},
}

// CapacityCadence 是容量序列的写入节奏。
//
// 容量的 statvfs 结果有 30 秒缓存（storage.DefaultCapacityTTL），时间戳用的也是
// 缓存里的 collected_at——采集循环跑得再勤，30 秒里也只换得出一行。这个数不是
// "配置里的采集间隔"，是这条曲线本身多久会有一个新点。
const CapacityCadence = 30 * time.Second

// WriteCadence 返回一条序列的写入节奏。
//
// 这是"这条曲线多久会有一个新点"的唯一答案，采集侧与判定侧都该问它，而不是各自
// 把 5 秒写死一遍。写到第二份的那个数一旦对不上容量缓存，症状是"规则配好了却永远
// 不响"，而这类问题最难往"节奏"上想：数据明明是有的。
func WriteCadence(metric string) time.Duration {
	switch metric {
	case MetricFSUsedPercent, MetricFSInodeUsedPercent, MetricFSAvailableBytes:
		return CapacityCadence
	default:
		// 其余序列每个采集周期都写一次，落库时对齐到 raw 桶宽（5 秒）。
		return time.Duration(store.MetricBucketSeconds(store.MetricResolutionRaw)) * time.Second
	}
}

// Definitions 返回全部登记的指标。
func Definitions() []Definition {
	out := make([]Definition, len(definitions))
	copy(out, definitions)
	return out
}

// DefinitionsFor 返回某个 scope 下的指标，未知 scope 返回空。
func DefinitionsFor(scope string) []Definition {
	var out []Definition
	for _, d := range definitions {
		if d.Scope == scope {
			out = append(out, d)
		}
	}
	return out
}

// IsKnownMetric 判断 (scope, metric) 是否登记过。
func IsKnownMetric(scope, metric string) bool {
	for _, d := range definitions {
		if d.Scope == scope && d.Metric == metric {
			return true
		}
	}
	return false
}

// IsKnownScope 判断 scope 是否属于闭集。
func IsKnownScope(scope string) bool {
	switch scope {
	case store.MetricScopeHost, store.MetricScopeInterface, store.MetricScopeFilesystem:
		return true
	default:
		return false
	}
}

// Scopes 返回全部 scope，顺序固定（host、interface、filesystem）。
func Scopes() []string {
	return []string{store.MetricScopeHost, store.MetricScopeInterface, store.MetricScopeFilesystem}
}

// DeriveHost 把一份系统快照变成主机级样本。
//
// 规则只有两条，都很死板：
//   - 段的状态不是 ok 就不写。采集失败时段里保留的是**上一次成功**的值，
//     把它当成这一刻的值写进历史，等于在历史上伪造一段“当时就是这样”的平坦曲线。
//   - 值为 null 就不写。首次采样没有 CPU 使用率、计数器刚重置没有速率——这些是
//     “不知道”，不是 0。
//
// 时间戳用**段自己的 collected_at**（值真正被读取的时刻），不是请求时刻：
// 采集循环卡住时，值的时间比请求时间旧，历史必须记那个旧时间。
func DeriveHost(sum monitoring.Summary) []store.MetricSample {
	var out []store.MetricSample
	add := func(metric string, ts time.Time, value float64) {
		if ts.IsZero() {
			return
		}
		out = append(out, store.MetricSample{
			Scope: store.MetricScopeHost, TargetID: "", Metric: metric,
			Resolution: store.MetricResolutionRaw, TS: ts,
			Value: value, MinValue: value, MaxValue: value, SampleCount: 1,
		})
	}

	if sum.CPU.Status == monitoring.StatusOK && sum.CPU.UsagePercent != nil {
		add(MetricCPUUsagePercent, sectionTime(sum.CPU.Section, sum.CollectedAt), *sum.CPU.UsagePercent)
	}
	if sum.Load.Status == monitoring.StatusOK {
		ts := sectionTime(sum.Load.Section, sum.CollectedAt)
		if sum.Load.Load1 != nil {
			add(MetricLoad1, ts, *sum.Load.Load1)
		}
		if sum.Load.Load5 != nil {
			add(MetricLoad5, ts, *sum.Load.Load5)
		}
		if sum.Load.Load15 != nil {
			add(MetricLoad15, ts, *sum.Load.Load15)
		}
	}
	if sum.Memory.Status == monitoring.StatusOK &&
		sum.Memory.TotalBytes != nil && *sum.Memory.TotalBytes > 0 &&
		sum.Memory.AvailableBytes != nil {
		total := float64(*sum.Memory.TotalBytes)
		used := total - float64(*sum.Memory.AvailableBytes)
		if used < 0 {
			// 可用内存大于总量：内核统计口径在极少数情况下会这样（缓存回收中），
			// 说明这一份样本不可信。宁可少一个点，也不要写一个负的使用率。
			return out
		}
		add(MetricMemoryUsedPercent, sectionTime(sum.Memory.Section, sum.CollectedAt), used/total*100)
	}
	return out
}

// DeriveNetwork 把系统快照里的网卡速率变成样本。
//
// 回环（lo）不记：它的流量是本机自己跟自己的对话（面板查代理、nginx 连上游都在上面），
// 量级与真实出入带宽无关，混在一张“网络流量”图上只会让人误判。PLAN.md 第 8 节要求
// 展示时不裁剪网卡列表，那是**展示**；历史曲线只留对外接口。
//
// 速率是 null 就不写（首次采样、计数器重置、采样窗口非正）。
func DeriveNetwork(sum monitoring.Summary) []store.MetricSample {
	if sum.Network.Status != monitoring.StatusOK {
		return nil
	}
	ts := sectionTime(sum.Network.Section, sum.CollectedAt)
	if ts.IsZero() {
		return nil
	}
	var out []store.MetricSample
	add := func(iface, metric string, value float64) {
		out = append(out, store.MetricSample{
			Scope: store.MetricScopeInterface, TargetID: iface, Metric: metric,
			Resolution: store.MetricResolutionRaw, TS: ts,
			Value: value, MinValue: value, MaxValue: value, SampleCount: 1,
		})
	}
	for _, iface := range sum.Network.Interfaces {
		if iface.Loopback {
			continue
		}
		if iface.RXBytesPerSecond != nil {
			add(iface.Name, MetricNetRXBytesPerSec, *iface.RXBytesPerSecond)
		}
		if iface.TXBytesPerSecond != nil {
			add(iface.Name, MetricNetTXBytesPerSec, *iface.TXBytesPerSecond)
		}
	}
	return out
}

// DeriveFilesystems 把存储拓扑里的挂载点容量变成样本。
//
// 只看**读到了容量**的挂载点：capacity 为 null 时（网络挂载不可达、statfs 失败、
// 刚挂上还没采到）不写任何点。把 null 记成 0 会让“盘掉了”看起来像“盘空了”。
//
// 时间戳用 capacity.collected_at：容量有 30 秒缓存，它可能比拓扑本身旧。
func DeriveFilesystems(top storage.Topology) []store.MetricSample {
	var out []store.MetricSample
	seen := make(map[string]bool)

	add := func(path string, cap *storage.Capacity) {
		if cap == nil || path == "" || seen[path] || cap.CollectedAt.IsZero() {
			return
		}
		seen[path] = true
		for _, item := range []struct {
			metric string
			value  float64
		}{
			{MetricFSUsedPercent, cap.UsedPercent},
			{MetricFSInodeUsedPercent, cap.InodeUsedPercent},
			{MetricFSAvailableBytes, float64(cap.AvailableBytes)},
		} {
			out = append(out, store.MetricSample{
				Scope: store.MetricScopeFilesystem, TargetID: path, Metric: item.metric,
				Resolution: store.MetricResolutionRaw, TS: cap.CollectedAt,
				Value: item.value, MinValue: item.value, MaxValue: item.value, SampleCount: 1,
			})
		}
	}

	for _, node := range top.Nodes {
		if node.Filesystem == nil {
			continue
		}
		for _, mount := range node.Filesystem.Mounts {
			add(mount.Path, mount.Capacity)
		}
	}
	for _, mount := range top.NetworkMounts {
		add(mount.Path, mount.Capacity)
	}
	return out
}

// sectionTime 取段自己的采集时间；段没说时回落到快照时间。
func sectionTime(s monitoring.Section, fallback time.Time) time.Time {
	if s.CollectedAt != nil && !s.CollectedAt.IsZero() {
		return *s.CollectedAt
	}
	return fallback
}
