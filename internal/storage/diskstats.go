package storage

import (
	"strconv"
	"strings"
	"time"
)

// /proc/diskstats 是内核的稳定接口（Documentation/admin-guide/iostats.rst），
// 不是本地化的列对齐文本：字段顺序固定、永远用空格分隔，因此可以直接解析。
//
// 本文件只负责两件事：把一行计数解析出来，以及用相邻两次采样算速率。
// 速率字段的 null 语义由 CONTRACTS.md 第 6.2 节规定：
// 计数器重置、首次采样、采样间隔异常时**本次速率为空**，而不是 0 ——
// 0 的意思是“这段时间没有 I/O”，null 的意思是“我不知道”。

// sectorBytes 是 /proc/diskstats 里扇区的固定单位。
//
// 内核在这个接口里**总是**用 512 字节扇区，与设备的逻辑扇区大小无关
// （iostats 文档明确写了这一点）。拿 logical_sector_bytes 去乘会算错 4K 盘的字节数。
const sectorBytes = 512

// DiskCounters 是一条 /proc/diskstats 记录里本阶段用得到的计数。
//
// 全部是自启动以来单调递增的累计值（in-flight 除外）。设备替换或内核重启后
// 会从零开始，这正是速率必须能被判定为“不可计算”的原因。
type DiskCounters struct {
	Name            string
	ReadsCompleted  uint64
	ReadMerged      uint64
	ReadSectors     uint64
	ReadMS          uint64
	WritesCompleted uint64
	WritesMerged    uint64
	WriteSectors    uint64
	WriteMS         uint64
	InFlight        uint64
	IOMS            uint64
	WeightedMS      uint64
}

// ReadBytes 是累计读字节数。
func (c DiskCounters) ReadBytes() uint64 { return c.ReadSectors * sectorBytes }

// WriteBytes 是累计写字节数。
func (c DiskCounters) WriteBytes() uint64 { return c.WriteSectors * sectorBytes }

// DiskstatsLine 是一条解析结果或一条无法解析的行。
type DiskstatsLine struct {
	Counters DiskCounters
	// Warning 非空表示这一行被跳过，内容是原因（行号 + 字段数）。
	// 单行坏掉不该让整次采集失败：其它设备的计数仍然有效。
	Warning string
}

// minDiskstatsFields 是内核 2.6.25 以来最少的值字段数：
// 读完成、读合并、读扇区、读耗时、写完成、写合并、写扇区、写耗时、在途、I/O 耗时、加权耗时。
// 之后的 discard（4 项）与 flush（2 项）是可选的，本阶段不参与计算。
const minDiskstatsFields = 11

// ParseDiskstats 解析 /proc/diskstats 的全部内容。
//
// 返回两条 lane：能用的计数，与坏行的原因。调用方把坏行记为一次采集警告，
// 继续使用其余设备的数据——一块设备的行格式异常不代表整台机器读不出来。
func ParseDiskstats(text string) (map[string]DiskCounters, []string) {
	out := map[string]DiskCounters{}
	var warnings []string
	lineNo := 0
	for _, line := range strings.Split(text, "\n") {
		lineNo++
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		// major、minor、name 之后至少有 11 个值。
		if len(fields) < 3+minDiskstatsFields {
			warnings = append(warnings, "第 "+strconv.Itoa(lineNo)+" 行只有 "+
				strconv.Itoa(len(fields))+" 个字段，少于 "+strconv.Itoa(3+minDiskstatsFields)+" 个")
			continue
		}
		name := fields[2]
		// major/minor 不参与计算，但仍要校验：它们不是数字说明这一行的列错位了，
		// 后面的数字很可能也是别人的。
		if _, err := strconv.ParseUint(fields[0], 10, 32); err != nil {
			warnings = append(warnings, "第 "+strconv.Itoa(lineNo)+" 行的 major 不是数字："+fields[0])
			continue
		}
		if _, err := strconv.ParseUint(fields[1], 10, 32); err != nil {
			warnings = append(warnings, "第 "+strconv.Itoa(lineNo)+" 行的 minor 不是数字："+fields[1])
			continue
		}

		values := make([]uint64, minDiskstatsFields)
		bad := ""
		for i := 0; i < minDiskstatsFields; i++ {
			v, err := strconv.ParseUint(fields[3+i], 10, 64)
			if err != nil {
				bad = "第 " + strconv.Itoa(lineNo) + " 行（" + name + "）的第 " +
					strconv.Itoa(4+i) + " 个字段不是非负整数：" + fields[3+i]
				break
			}
			values[i] = v
		}
		if bad != "" {
			warnings = append(warnings, bad)
			continue
		}

		out[name] = DiskCounters{
			Name:            name,
			ReadsCompleted:  values[0],
			ReadMerged:      values[1],
			ReadSectors:     values[2],
			ReadMS:          values[3],
			WritesCompleted: values[4],
			WritesMerged:    values[5],
			WriteSectors:    values[6],
			WriteMS:         values[7],
			InFlight:        values[8],
			IOMS:            values[9],
			WeightedMS:      values[10],
		}
	}
	return out, warnings
}

// IORate 是两个相邻采样之间算出来的速率（CONTRACTS.md 第 6.2 节）。
//
// 延迟只在对应的完成次数大于 0 时给出：没有 I/O 就谈不上“平均每次等多久”，
// 报 0 ms 会让人以为“很快”，实际是“没有样本”。
type IORate struct {
	// IntervalMS 是这两个采样之间的真实间隔。
	IntervalMS int64 `json:"interval_ms"`

	ReadBytesPerSecond  uint64  `json:"read_bytes_per_second,string"`
	WriteBytesPerSecond uint64  `json:"write_bytes_per_second,string"`
	ReadIOPS            float64 `json:"read_iops"`
	WriteIOPS           float64 `json:"write_iops"`

	ReadLatencyMS  *float64 `json:"read_latency_ms"`
	WriteLatencyMS *float64 `json:"write_latency_ms"`
}

// IOStats 是一台设备的 I/O 计数与速率（CONTRACTS.md 第 6.2 节）。
//
// 每个设备只拿自己那一行计数，从不相加：分区的 I/O 已经包含在整盘里，
// dm/LVM 的 I/O 又与底层盘重复。把它们加起来就是双重计数（PLAN.md 第 8 节）。
type IOStats struct {
	SampledAt time.Time `json:"sampled_at"`

	ReadBytesTotal  uint64 `json:"read_bytes_total,string"`
	WriteBytesTotal uint64 `json:"write_bytes_total,string"`
	ReadsTotal      uint64 `json:"reads_total,string"`
	WritesTotal     uint64 `json:"writes_total,string"`

	// Rate 为 null 表示本次没有可用的采样窗口：
	// 首次采样、计数器重置（重启/换盘）、或采样间隔异常（进程被挂起、机器休眠）。
	Rate *IORate `json:"rate"`
}

// NewIOStats 由本次计数构造 IOStats（不含速率）。
func NewIOStats(c DiskCounters, at time.Time) *IOStats {
	return &IOStats{
		SampledAt:       at.UTC(),
		ReadBytesTotal:  c.ReadBytes(),
		WriteBytesTotal: c.WriteBytes(),
		ReadsTotal:      c.ReadsCompleted,
		WritesTotal:     c.WritesCompleted,
	}
}

// resetDetected 判断计数器是否变小。
//
// 内核重启、设备被替换（同名新盘）、或 32 位计数器回绕都会让计数变小。
// 这种情况下本次差值没有意义：算出负数会被截断成 0，看起来像“这段没读写”，
// 那是在编造数据。
func resetDetected(prev, cur DiskCounters) bool {
	pairs := [...][2]uint64{
		{prev.ReadsCompleted, cur.ReadsCompleted},
		{prev.ReadSectors, cur.ReadSectors},
		{prev.ReadMS, cur.ReadMS},
		{prev.WritesCompleted, cur.WritesCompleted},
		{prev.WriteSectors, cur.WriteSectors},
		{prev.WriteMS, cur.WriteMS},
	}
	for _, p := range pairs {
		if p[1] < p[0] {
			return true
		}
	}
	return false
}

// ComputeRate 用相邻两次采样算速率。返回 nil 表示本次速率不可用：
//
//   - 还没有上一次采样（代理刚启动、设备刚出现）；
//   - 计数器变小（重置/换盘/回绕）；
//   - 采样间隔非正（时钟回拨）或超过 expected 的 3 倍（采集被挂起过）。
//
// expected 是配置的采集间隔；0 表示不做“间隔异常”判断。
func ComputeRate(prev DiskCounters, prevAt time.Time, cur DiskCounters, curAt time.Time, expected time.Duration) *IORate {
	if prevAt.IsZero() || curAt.Before(prevAt) {
		return nil
	}
	window := curAt.Sub(prevAt)
	if window <= 0 {
		return nil
	}
	if expected > 0 && window > 3*expected {
		return nil
	}
	if resetDetected(prev, cur) {
		return nil
	}

	ms := float64(window.Milliseconds())
	if ms <= 0 {
		return nil
	}
	dReads := float64(cur.ReadsCompleted - prev.ReadsCompleted)
	dWrites := float64(cur.WritesCompleted - prev.WritesCompleted)
	dReadBytes := float64(cur.ReadBytes() - prev.ReadBytes())
	dWriteBytes := float64(cur.WriteBytes() - prev.WriteBytes())

	rate := &IORate{
		IntervalMS:          window.Milliseconds(),
		ReadBytesPerSecond:  uint64(dReadBytes * 1000 / ms),
		WriteBytesPerSecond: uint64(dWriteBytes * 1000 / ms),
		ReadIOPS:            round2(dReads * 1000 / ms),
		WriteIOPS:           round2(dWrites * 1000 / ms),
	}
	// 平均延迟 = 这段时间花在 I/O 上的毫秒数 / 完成的次数。
	// 完成的次数为 0 时没有样本，留 null。
	if dReads > 0 {
		v := round2(float64(cur.ReadMS-prev.ReadMS) / dReads)
		rate.ReadLatencyMS = &v
	}
	if dWrites > 0 {
		v := round2(float64(cur.WriteMS-prev.WriteMS) / dWrites)
		rate.WriteLatencyMS = &v
	}
	return rate
}

// round2 保留两位小数。速率与延迟都不需要比这更细，保留更多位只会让
// 每次采样都不同，前端无法做“数值没变就不重绘”的判断。
func round2(v float64) float64 {
	if v < 0 {
		return 0
	}
	return float64(int64(v*100+0.5)) / 100
}
