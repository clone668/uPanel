package storage

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"upanel/internal/testsupport"
)

// 这些用例分成两类，来源在名字里写清楚：
//
//   - 真实采集：tests/fixtures/diskstats-2404.txt（VM-B 24.04）与
//     diskstats-2204.txt（VM-A 22.04），见 tests/fixtures/PROVENANCE.md；
//   - 合成：文件名带 .synthetic.，用来构造坏行、计数器重置、异常间隔这类
//     真机上不好复现的输入。

func readFixtureText(t *testing.T, name string) string {
	t.Helper()
	raw, err := testsupport.Fixture(name)
	if err != nil {
		t.Fatalf("读夹具 %s 失败: %v", name, err)
	}
	return string(raw)
}

// TestParseDiskstatsRealCapture 解析两份真机采集，逐字段核对。
func TestParseDiskstatsRealCapture(t *testing.T) {
	for _, name := range []string{"diskstats-2404.txt", "diskstats-2204.txt", "diskstats-smart-2404.txt"} {
		t.Run(name, func(t *testing.T) {
			counters, warnings := ParseDiskstats(readFixtureText(t, name))
			if len(warnings) > 0 {
				t.Errorf("真机采集不该有坏行，却报了 %v", warnings)
			}
			if len(counters) == 0 {
				t.Fatalf("一条记录都没解析出来")
			}

			// 整盘、分区、loop、软盘都要在：接口按内核设备名索引，
			// 缺了哪一类都会让对应设备在快照里 io 为 null。
			for _, want := range []string{"sda", "sda1", "loop0", "fd0"} {
				if _, ok := counters[want]; !ok {
					t.Errorf("缺少 %s 的计数", want)
				}
			}

			sda, ok := counters["sda"]
			if !ok {
				t.Fatalf("没有 sda")
			}
			if sda.Name != "sda" {
				t.Errorf("Name = %q，想要 sda", sda.Name)
			}
			if sda.ReadsCompleted == 0 {
				t.Errorf("sda 的读完成次数为 0：真机上系统盘不可能没读过")
			}
			if sda.ReadBytes() != sda.ReadSectors*sectorBytes {
				t.Errorf("ReadBytes() 与扇区数不符")
			}
			if sda.WriteBytes() == 0 {
				t.Errorf("sda 的累计写字节为 0：真机上系统盘必然写过")
			}
			// 内核在 diskstats 里固定用 512 字节扇区，与设备逻辑扇区无关。
			if sectorBytes != 512 {
				t.Fatalf("sectorBytes = %d，内核这个接口固定 512", sectorBytes)
			}
		})
	}

	// 三份夹具来自三台不同的机器，覆盖面正是它们的差别：
	//   - 2404（VM-B，24.04）：四块 SCSI 盘，没有 NVMe；
	//   - 2204（VM-A，22.04）：四块 SCSI 盘 + snap 的 loop；
	//   - smart-2404（VM-E，24.04）：系统 SCSI 盘 + 一块 NVMe 命名空间。
	c2404, _ := ParseDiskstats(readFixtureText(t, "diskstats-2404.txt"))
	if _, ok := c2404["nvme0n1"]; ok {
		t.Errorf("VM-B 没有 NVMe，夹具里不该出现 nvme0n1")
	}
	for _, name := range []string{"sda", "sdb", "sdc", "sdd"} {
		if _, ok := c2404[name]; !ok {
			t.Errorf("VM-B 夹具缺少 %s", name)
		}
	}
	c2204, _ := ParseDiskstats(readFixtureText(t, "diskstats-2204.txt"))
	if _, ok := c2204["nvme0n1"]; ok {
		t.Errorf("22.04 夹具里不该有 nvme0n1")
	}
	if _, ok := c2204["sdd"]; !ok {
		t.Errorf("22.04 夹具里应有 sdd（四块数据盘）")
	}
	cnvme, _ := ParseDiskstats(readFixtureText(t, "diskstats-smart-2404.txt"))
	nvme, ok := cnvme["nvme0n1"]
	if !ok {
		t.Fatalf("带 NVMe 的夹具里应有 nvme0n1")
	}
	if nvme.Name != "nvme0n1" || nvme.ReadsCompleted == 0 {
		t.Errorf("nvme0n1 的计数没解析对: %+v", nvme)
	}
}

// TestParseDiskstatsIgnoresExtraColumns 钉住“后面还有 discard/flush 列”。
// 内核 4.18 起每行多了 7 个字段；解析只取前 11 个，多了不能失败。
func TestParseDiskstatsIgnoresExtraColumns(t *testing.T) {
	line := "   8       0 sda 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17"
	counters, warnings := ParseDiskstats(line)
	if len(warnings) > 0 {
		t.Fatalf("多出列不该报警告: %v", warnings)
	}
	c := counters["sda"]
	if c.WeightedMS != 11 {
		t.Errorf("WeightedMS = %d，想要第 11 个值 11", c.WeightedMS)
	}
	if c.IOMS != 10 {
		t.Errorf("IOMS = %d，想要 10", c.IOMS)
	}
}

// TestParseDiskstatsBadLineKeepsOthers 校验坏行隔离：
// 一行坏掉只记警告，其余设备的计数照常返回。
func TestParseDiskstatsBadLineKeepsOthers(t *testing.T) {
	text := strings.Join([]string{
		"   8       0 sda 10 0 80 4 20 0 160 8 0 12 12", // 好行
		"   8       1 sda1 10 0",                        // 字段太少
		"garbage 16 sdb 1 2 3 4 5 6 7 8 9 10 11",        // major 不是数字
		"   8      17 sdc 1 2 3 4 5 6 7 x 9 10 11",      // 值是字母
		"", // 空行跳过，不算警告
		"   8      32 sdd 1 2 3 4 5 6 7 8 9 10 11", // 好行
	}, "\n")

	counters, warnings := ParseDiskstats(text)
	if len(warnings) != 3 {
		t.Fatalf("warnings = %v，想要 3 条", warnings)
	}
	if _, ok := counters["sda"]; !ok {
		t.Errorf("坏行不该影响 sda")
	}
	if _, ok := counters["sdd"]; !ok {
		t.Errorf("坏行不该影响 sdd")
	}
	if _, ok := counters["sda1"]; ok {
		t.Errorf("字段不足的行不该被当成有效计数")
	}
	if _, ok := counters["sdb"]; ok {
		t.Errorf("major 不是数字的行不该被解析")
	}
	if _, ok := counters["sdc"]; ok {
		t.Errorf("值不是数字的行不该被解析")
	}
	for _, w := range warnings {
		if !strings.Contains(w, "行") {
			t.Errorf("警告 %q 没有指出行号，运维无从排查", w)
		}
	}
}

// TestParseDiskstatsEmpty 空输入返回空表而不是 panic。
func TestParseDiskstatsEmpty(t *testing.T) {
	counters, warnings := ParseDiskstats("")
	if len(counters) != 0 || len(warnings) != 0 {
		t.Errorf("空输入得到 %d 条计数、%d 条警告", len(counters), len(warnings))
	}
}

// TestComputeRateBasic 校验一次正常 5 秒窗口的速率与延迟。
func TestComputeRateBasic(t *testing.T) {
	prev := DiskCounters{ReadsCompleted: 100, ReadSectors: 2000, ReadMS: 500,
		WritesCompleted: 50, WriteSectors: 400, WriteMS: 250}
	cur := DiskCounters{ReadsCompleted: 200, ReadSectors: 6000, ReadMS: 900,
		WritesCompleted: 60, WriteSectors: 1200, WriteMS: 290}
	prevAt := fixedNow
	curAt := fixedNow.Add(5 * time.Second)

	rate := ComputeRate(prev, prevAt, cur, curAt, 5*time.Second)
	if rate == nil {
		t.Fatalf("正常窗口却算不出速率")
	}
	if rate.IntervalMS != 5000 {
		t.Errorf("IntervalMS = %d，想要 5000", rate.IntervalMS)
	}
	// 5 秒内多了 4000 个扇区 = 2048000 字节 → 409600 B/s。
	if want := uint64(4000 * 512 / 5); rate.ReadBytesPerSecond != want {
		t.Errorf("ReadBytesPerSecond = %d，想要 %d", rate.ReadBytesPerSecond, want)
	}
	if want := uint64(800 * 512 / 5); rate.WriteBytesPerSecond != want {
		t.Errorf("WriteBytesPerSecond = %d，想要 %d", rate.WriteBytesPerSecond, want)
	}
	if rate.ReadIOPS != 20 {
		t.Errorf("ReadIOPS = %v，想要 20", rate.ReadIOPS)
	}
	if rate.WriteIOPS != 2 {
		t.Errorf("WriteIOPS = %v，想要 2", rate.WriteIOPS)
	}
	// 读延迟：400 ms / 100 次 = 4 ms；写延迟：40 ms / 10 次 = 4 ms。
	if rate.ReadLatencyMS == nil || *rate.ReadLatencyMS != 4 {
		t.Errorf("ReadLatencyMS = %v，想要 4", rate.ReadLatencyMS)
	}
	if rate.WriteLatencyMS == nil || *rate.WriteLatencyMS != 4 {
		t.Errorf("WriteLatencyMS = %v，想要 4", rate.WriteLatencyMS)
	}
}

// TestComputeRateNoIOIsZeroLatencyNull 是 null 语义的正面用例：
// 这 5 秒里确实没有任何 I/O。速率是 0（真的没动），但平均延迟是 null，
// 因为“没有样本”和“等了 0 毫秒”是两回事。
func TestComputeRateNoIOIsZeroLatencyNull(t *testing.T) {
	same := DiskCounters{ReadsCompleted: 100, ReadSectors: 2000, ReadMS: 500,
		WritesCompleted: 50, WriteSectors: 400, WriteMS: 250}
	rate := ComputeRate(same, fixedNow, same, fixedNow.Add(5*time.Second), 5*time.Second)
	if rate == nil {
		t.Fatalf("计数没变是一个合法窗口，速率应为 0 而不是 null")
	}
	if rate.ReadBytesPerSecond != 0 || rate.WriteBytesPerSecond != 0 {
		t.Errorf("没有 I/O 时字节速率应为 0，得到 %d/%d",
			rate.ReadBytesPerSecond, rate.WriteBytesPerSecond)
	}
	if rate.ReadIOPS != 0 || rate.WriteIOPS != 0 {
		t.Errorf("没有 I/O 时 IOPS 应为 0，得到 %v/%v", rate.ReadIOPS, rate.WriteIOPS)
	}
	if rate.ReadLatencyMS != nil || rate.WriteLatencyMS != nil {
		t.Errorf("完成次数为 0 时延迟必须是 null，得到 %v/%v",
			rate.ReadLatencyMS, rate.WriteLatencyMS)
	}
}

// TestComputeRateNullOnReset 计数器变小（重启、换盘、回绕）时速率必须是 null。
func TestComputeRateNullOnReset(t *testing.T) {
	prev := DiskCounters{ReadsCompleted: 1000, ReadSectors: 900000, ReadMS: 5000,
		WritesCompleted: 500, WriteSectors: 4000, WriteMS: 2000}
	cur := DiskCounters{ReadsCompleted: 3, ReadSectors: 40, ReadMS: 1}
	if rate := ComputeRate(prev, fixedNow, cur, fixedNow.Add(5*time.Second), 5*time.Second); rate != nil {
		t.Errorf("计数器变小却算出了速率 %+v", rate)
	}

	// 只重置一个字段也要判定为重置：盘的六个计数是一起清零的，
	// 只有一个变小说明看到的其实是另一块同名设备。
	partial := prev
	partial.ReadsCompleted += 10
	partial.ReadSectors += 10
	partial.ReadMS += 10
	partial.WritesCompleted += 1
	partial.WriteMS += 1
	partial.WriteSectors -= 1 // 只有写扇区回退了
	if rate := ComputeRate(prev, fixedNow, partial, fixedNow.Add(5*time.Second), 5*time.Second); rate != nil {
		t.Errorf("写扇区回退却算出了速率 %+v", rate)
	}
}

// TestComputeRateNullOnFirstSample 没有上一次采样时速率必须是 null：
// 从 0 到累计值除以 5 秒会算出“开机以来的平均速率”，伪装成当前速率。
func TestComputeRateNullOnFirstSample(t *testing.T) {
	cur := DiskCounters{ReadsCompleted: 100, ReadSectors: 2000}
	if rate := ComputeRate(DiskCounters{}, time.Time{}, cur, fixedNow, 5*time.Second); rate != nil {
		t.Errorf("首次采样却算出了速率 %+v", rate)
	}
}

// TestComputeRateNullOnAbnormalInterval 采集被挂起（机器睡眠、调试器停住、负载风暴）
// 之后窗口会远超配置间隔，这时除以整个窗口得到的“平均值”不是当前速率。
func TestComputeRateNullOnAbnormalInterval(t *testing.T) {
	prev := DiskCounters{ReadsCompleted: 100, ReadSectors: 2000, ReadMS: 500}
	cur := DiskCounters{ReadsCompleted: 200, ReadSectors: 6000, ReadMS: 900}

	// 恰好 3 倍仍然接受（边界包含在窗口内）。
	if rate := ComputeRate(prev, fixedNow, cur, fixedNow.Add(15*time.Second), 5*time.Second); rate == nil {
		t.Errorf("3 倍间隔仍应计算")
	}
	if rate := ComputeRate(prev, fixedNow, cur, fixedNow.Add(16*time.Second), 5*time.Second); rate != nil {
		t.Errorf("超过 3 倍间隔却算出了速率 %+v：那是平均值，不是当前速率", rate)
	}
	// 时钟回拨：curAt 早于 prevAt。
	if rate := ComputeRate(prev, fixedNow.Add(5*time.Second), cur, fixedNow, 5*time.Second); rate != nil {
		t.Errorf("时间倒流却算出了速率 %+v", rate)
	}
	// 同一时刻两次采样：窗口为 0，除法无意义。
	if rate := ComputeRate(prev, fixedNow, cur, fixedNow, 5*time.Second); rate != nil {
		t.Errorf("零窗口却算出了速率 %+v", rate)
	}
	// expected 为 0 表示不做间隔判断，但仍要能算。
	if rate := ComputeRate(prev, fixedNow, cur, fixedNow.Add(16*time.Second), 0); rate == nil {
		t.Errorf("expected=0 时不该做间隔判断")
	}
}

// TestIOStatsJSONShape 校验 CONTRACTS §1 的表示法：
// 累计字节与计数是非负十进制字符串，速率可以为 null，时间带 Z。
func TestIOStatsJSONShape(t *testing.T) {
	cur := DiskCounters{ReadsCompleted: 12, ReadSectors: 34, WritesCompleted: 56, WriteSectors: 78}
	stats := NewIOStats(cur, fixedNow)

	raw, err := json.Marshal(stats)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	for _, key := range []string{"read_bytes_total", "write_bytes_total", "reads_total", "writes_total"} {
		s, ok := obj[key].(string)
		if !ok {
			t.Errorf("%s 不是字符串而是 %T（CONTRACTS §1 要求十进制字符串）", key, obj[key])
			continue
		}
		if strings.HasPrefix(s, "-") {
			t.Errorf("%s 为负数 %q", key, s)
		}
	}
	if v, ok := obj["rate"]; !ok || v != nil {
		t.Errorf("rate = %v，首次采样必须显式 null", v)
	}
	if got := obj["sampled_at"]; got != "2026-10-20T17:40:00Z" {
		t.Errorf("sampled_at = %v，想要 RFC3339 UTC 带 Z", got)
	}

	// 有速率时字段名也要与契约一致。
	stats.Rate = ComputeRate(cur, fixedNow, cur, fixedNow.Add(5*time.Second), 5*time.Second)
	raw, _ = json.Marshal(stats)
	for _, key := range []string{"read_bytes_per_second", "write_bytes_per_second",
		"read_iops", "write_iops", "read_latency_ms", "write_latency_ms", "interval_ms"} {
		if !strings.Contains(string(raw), `"`+key+`"`) {
			t.Errorf("缺少字段 %s：%s", key, raw)
		}
	}
}

// TestRound2NeverNegative round2 把负值钳成 0：
// 计数器校验之后再出现负数只可能是内核回绕的边角，写进界面会变成“负延迟”。
func TestRound2NeverNegative(t *testing.T) {
	if got := round2(-1.5); got != 0 {
		t.Errorf("round2(-1.5) = %v，想要 0", got)
	}
	if got := round2(1.234); got != 1.23 {
		t.Errorf("round2(1.234) = %v，想要 1.23", got)
	}
}
