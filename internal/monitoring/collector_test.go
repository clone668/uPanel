package monitoring

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeReader 是按路径返回内容的 FileReader。map 里没有的路径当成不存在，
// 这样“来源缺失”的分支与操作系统上的真实行为一致（ENOENT）。
type fakeReader struct {
	mu    sync.Mutex
	files map[string]string
}

func newFakeReader(t *testing.T) *fakeReader {
	t.Helper()
	paths := DefaultPaths()
	files := map[string]string{
		paths.Stat:                            string(fixture(t, "stat-cpu-2404.txt")),
		paths.Meminfo:                         string(fixture(t, "meminfo-2404.txt")),
		paths.Loadavg:                         string(fixture(t, "loadavg-2404.txt")),
		paths.NetDev:                          string(fixture(t, "netdev-2404.txt")),
		paths.OSRelease:                       string(fixture(t, "os-release-2404.txt")),
		paths.Hostname:                        "upanel-2404\n",
		paths.KernelRelease:                   "6.8.0-71-generic\n",
		paths.BootID:                          "3f6c1d92-0000-4000-8000-000000000001\n",
		paths.Uptime:                          string(fixture(t, "uptime-2404.txt")),
		paths.CPUInfo:                         "processor\t: 0\nmodel name\t: QEMU Virtual CPU version 2.5+\n",
		paths.SysNetClass + "/lo/operstate":   "unknown\n",
		paths.SysNetClass + "/ens4/operstate": "up\n",
	}
	return &fakeReader{files: files}
}

func (r *fakeReader) ReadFile(name string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	content, ok := r.files[name]
	if !ok {
		return nil, &os.PathError{Op: "open", Path: name, Err: os.ErrNotExist}
	}
	return []byte(content), nil
}

func (r *fakeReader) set(name, content string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.files[name] = content
}

func (r *fakeReader) remove(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.files, name)
}

// fakeClock 是可推进的时钟，避免测试里 sleep。
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newTestCollector(t *testing.T, reader FileReader, clock *fakeClock) *Collector {
	t.Helper()
	return NewCollector(Options{
		Interval: DefaultInterval,
		Now:      clock.Now,
		Reader:   reader,
	})
}

func TestCollectorFirstSampleHasNoRates(t *testing.T) {
	reader := newFakeReader(t)
	clock := newFakeClock()
	c := newTestCollector(t, reader, clock)

	sum := c.Refresh()

	// 第一份快照必须完整：主机、内存、负载、网络都是真数据。
	if sum.Host.Status != StatusOK || sum.Host.Hostname != "upanel-2404" ||
		sum.Host.OSID != "ubuntu" || sum.Host.OSVersion != "24.04" ||
		sum.Host.KernelRelease != "6.8.0-71-generic" ||
		sum.Host.BootID != "3f6c1d92-0000-4000-8000-000000000001" {
		t.Fatalf("主机段不符合预期: %+v", sum.Host)
	}
	if sum.Host.UptimeSeconds == nil || *sum.Host.UptimeSeconds != 2076.61 {
		t.Fatalf("开机时长 = %v", sum.Host.UptimeSeconds)
	}
	if sum.CPU.LogicalCores != 2 {
		t.Fatalf("逻辑核 = %d，期望 2", sum.CPU.LogicalCores)
	}
	if sum.CPU.Model != "QEMU Virtual CPU version 2.5+" {
		t.Fatalf("CPU 型号 = %q", sum.CPU.Model)
	}
	if sum.Memory.TotalBytes == nil || *sum.Memory.TotalBytes != 4009864*1024 {
		t.Fatalf("内存总量 = %v", sum.Memory.TotalBytes)
	}
	if sum.Memory.UsedBytes == nil || *sum.Memory.UsedBytes != (4009864-3637424)*1024 {
		t.Fatalf("已用内存 = %v，期望 Total - Available", sum.Memory.UsedBytes)
	}
	if sum.Load.Load1 == nil || *sum.Load.Load1 != 0.10 {
		t.Fatalf("负载 = %v", sum.Load.Load1)
	}
	if len(sum.Network.Interfaces) != 2 || sum.Network.Interfaces[0].Name != "ens4" {
		t.Fatalf("网卡 = %+v", sum.Network.Interfaces)
	}
	if got := sum.Network.Interfaces[0].OperState; got != "up" {
		t.Fatalf("ens4 operstate = %q", got)
	}
	if !sum.Network.Interfaces[1].Loopback {
		t.Fatal("lo 必须标成 loopback")
	}

	// 没有任何前一次样本：使用率与速率必须是 null，不能是 0。
	if sum.CPU.UsagePercent != nil {
		t.Fatalf("首次采样 usage_percent = %v，应为 null", *sum.CPU.UsagePercent)
	}
	if sum.CPU.SampleWindowMS != nil {
		t.Fatal("首次采样不应有 sample_window_ms")
	}
	for _, iface := range sum.Network.Interfaces {
		if iface.RXBytesPerSecond != nil || iface.TXBytesPerSecond != nil {
			t.Fatalf("%s 首次采样速率为 %v/%v，应为 null",
				iface.Name, iface.RXBytesPerSecond, iface.TXBytesPerSecond)
		}
	}
	if sum.Degraded {
		t.Fatalf("全部来源正常时不应标成 degraded: %+v", sum)
	}
	if !sum.CollectedAt.Equal(clock.Now()) {
		t.Fatalf("collected_at = %v，期望 %v", sum.CollectedAt, clock.Now())
	}
	if sum.IntervalMS != DefaultInterval.Milliseconds() {
		t.Fatalf("interval_ms = %d", sum.IntervalMS)
	}
}

func TestCollectorComputesRatesFromDeltas(t *testing.T) {
	reader := newFakeReader(t)
	clock := newFakeClock()
	c := newTestCollector(t, reader, clock)
	paths := DefaultPaths()

	c.Refresh()
	clock.Advance(5 * time.Second)

	// 合计：user +100、idle +800 → 总量 +900，空闲 +800 → 使用率 11.11%。
	// 单核 cpu0：user +50、idle +400 → 450 中空闲 400 → 11.11%。
	reader.set(paths.Stat, strings.Join([]string{
		"cpu  20527 0 8476 379780 5878 0 1022 0 0 0",
		"cpu0 10358 0 4246 189738 2969 0 520 0 0 0",
		"cpu1 10169 0 4230 190042 2908 0 502 0 0 0",
		"",
	}, "\n"))
	// lo: rx/tx +1000 字节、packets +10，5 秒 → 200 B/s。
	reader.set(paths.NetDev, strings.Join([]string{
		"Inter-|   Receive                                                |  Transmit",
		" face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed",
		"    lo: 3023016   10320    0    0    0     0          0         0  3023016   10320    0    0    0     0       0          0",
		"  ens4: 402930897  283011    0    0    0     0          0         0  3350163   21162    0    0    0     0       0          0",
		"",
	}, "\n"))

	sum := c.Refresh()

	if sum.CPU.UsagePercent == nil {
		t.Fatal("第二次采样应能算出使用率")
	}
	if got := *sum.CPU.UsagePercent; got < 11.0 || got > 11.2 {
		t.Fatalf("使用率 = %v，期望约 11.11", got)
	}
	if sum.CPU.SampleWindowMS == nil || *sum.CPU.SampleWindowMS != 5000 {
		t.Fatalf("sample_window_ms = %v，期望 5000", sum.CPU.SampleWindowMS)
	}
	if len(sum.CPU.PerCPU) != 2 || sum.CPU.PerCPU[0].UsagePercent == nil {
		t.Fatalf("按核使用率缺失: %+v", sum.CPU.PerCPU)
	}

	lo := sum.Network.Interfaces[1]
	if lo.Name != "lo" {
		t.Fatalf("网卡顺序变化: %+v", sum.Network.Interfaces)
	}
	if lo.RXBytesPerSecond == nil || *lo.RXBytesPerSecond != 200 {
		t.Fatalf("lo rx 速率 = %v，期望 200", lo.RXBytesPerSecond)
	}
	if lo.TXBytesPerSecond == nil || *lo.TXBytesPerSecond != 200 {
		t.Fatalf("lo tx 速率 = %v，期望 200", lo.TXBytesPerSecond)
	}
	// 没有变化的网卡速率是真实的 0，不是 null：计数器确实增加了 0。
	ens := sum.Network.Interfaces[0]
	if ens.RXBytesPerSecond == nil || *ens.RXBytesPerSecond != 0 {
		t.Fatalf("ens4 rx 速率 = %v，期望 0（没有流量的真实零）", ens.RXBytesPerSecond)
	}
}

func TestCollectorCounterResetYieldsNullRates(t *testing.T) {
	reader := newFakeReader(t)
	clock := newFakeClock()
	c := newTestCollector(t, reader, clock)
	paths := DefaultPaths()

	c.Refresh()
	clock.Advance(5 * time.Second)

	// 计数器变小：只可能是重置或设备被替换，此时差值没有意义。
	reader.set(paths.NetDev, strings.Join([]string{
		"Inter-|   Receive                                                |  Transmit",
		" face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed",
		"    lo: 10   1    0    0    0     0          0         0  10   1    0    0    0     0       0          0",
		"  ens4: 402930897  283011    0    0    0     0          0         0  3350163   21162    0    0    0     0       0          0",
		"",
	}, "\n"))
	reader.set(paths.Stat, strings.Join([]string{
		"cpu  1 0 1 1 0 0 0 0 0 0",
		"cpu0 1 0 0 1 0 0 0 0 0 0",
		"cpu1 0 0 1 0 0 0 0 0 0 0",
		"",
	}, "\n"))

	sum := c.Refresh()

	if sum.CPU.UsagePercent != nil {
		t.Fatalf("CPU 计数器变小后使用率应为 null，实际 %v", *sum.CPU.UsagePercent)
	}
	if sum.CPU.SampleWindowMS != nil {
		t.Fatal("计数器变小后不应给出采样时间窗")
	}
	lo := sum.Network.Interfaces[1]
	if lo.RXBytesPerSecond != nil || lo.TXBytesPerSecond != nil {
		t.Fatalf("计数器变小的网卡速率应为 null，实际 %v/%v",
			lo.RXBytesPerSecond, lo.TXBytesPerSecond)
	}
	// 负数绝不能出现：速率是 uint 差值算出来的 float，这里顺带钉住符号。
	if lo.RXBytesPerSecond != nil && *lo.RXBytesPerSecond < 0 {
		t.Fatal("速率出现了负数")
	}
}

func TestCollectorRebootInvalidatesCPUWindow(t *testing.T) {
	reader := newFakeReader(t)
	clock := newFakeClock()
	c := newTestCollector(t, reader, clock)
	paths := DefaultPaths()

	c.Refresh()
	clock.Advance(5 * time.Second)
	// boot_id 变了：两次样本跨越了一次重启，计数器不可比。
	reader.set(paths.BootID, "9af0c3d1-0000-4000-8000-000000000002\n")
	reader.set(paths.Stat, strings.Join([]string{
		"cpu  30000 0 9000 400000 6000 0 1200 0 0 0",
		"cpu0 15000 0 4500 200000 3000 0 600 0 0 0",
		"cpu1 15000 0 4500 200000 3000 0 600 0 0 0",
		"",
	}, "\n"))

	sum := c.Refresh()
	if sum.Host.BootID != "9af0c3d1-0000-4000-8000-000000000002" {
		t.Fatalf("boot_id 未更新: %q", sum.Host.BootID)
	}
	if sum.CPU.UsagePercent != nil {
		t.Fatalf("跨重启的两次样本不能算使用率，实际 %v", *sum.CPU.UsagePercent)
	}
}

func TestCollectorFailedSectionKeepsLastGoodValue(t *testing.T) {
	reader := newFakeReader(t)
	clock := newFakeClock()
	c := newTestCollector(t, reader, clock)
	paths := DefaultPaths()

	first := c.Refresh()
	if first.Memory.Status != StatusOK {
		t.Fatalf("首次采集内存应成功: %+v", first.Memory.Section)
	}
	goodAt := *first.Memory.CollectedAt

	clock.Advance(5 * time.Second)
	reader.set(paths.Meminfo, string(fixture(t, "meminfo.synthetic.missing-available.txt")))

	sum := c.Refresh()
	if sum.Memory.Status != StatusError {
		t.Fatalf("解析失败时内存段 status = %q，期望 error", sum.Memory.Status)
	}
	if !strings.Contains(sum.Memory.Error, "MemAvailable") {
		t.Fatalf("错误说明应点名缺失字段，实际 %q", sum.Memory.Error)
	}
	// 最近有效值仍然在，但时间戳停在成功那一刻：界面据此显示“陈旧”。
	if sum.Memory.TotalBytes == nil || *sum.Memory.TotalBytes != 4009864*1024 {
		t.Fatalf("失败后应保留最近有效值，实际 %v", sum.Memory.TotalBytes)
	}
	if sum.Memory.CollectedAt == nil || !sum.Memory.CollectedAt.Equal(goodAt) {
		t.Fatalf("失败后值的时间应为上次成功时间 %v，实际 %v", goodAt, sum.Memory.CollectedAt)
	}
	if !sum.Degraded {
		t.Fatal("有段失败时必须标 degraded")
	}
	// 其他段不受影响：一次失败不能把整份快照打成空。
	if sum.CPU.Status != StatusOK || sum.Host.Status != StatusOK || sum.Network.Status != StatusOK {
		t.Fatalf("单段失败不应牵连其他段: cpu=%s host=%s net=%s",
			sum.CPU.Status, sum.Host.Status, sum.Network.Status)
	}

	// 恢复后值和时间都回到最新。
	clock.Advance(5 * time.Second)
	reader.set(paths.Meminfo, string(fixture(t, "meminfo-2404.txt")))
	sum = c.Refresh()
	if sum.Memory.Status != StatusOK || sum.Memory.Error != "" {
		t.Fatalf("恢复后内存段应回到 ok: %+v", sum.Memory.Section)
	}
	if sum.Memory.CollectedAt == nil || !sum.Memory.CollectedAt.Equal(clock.Now()) {
		t.Fatalf("恢复后 collected_at 应更新，实际 %v", sum.Memory.CollectedAt)
	}
	if sum.Degraded {
		t.Fatal("全部恢复后不应再标 degraded")
	}
}

func TestCollectorMissingSourceIsUnavailableNotZero(t *testing.T) {
	reader := newFakeReader(t)
	clock := newFakeClock()
	c := newTestCollector(t, reader, clock)
	paths := DefaultPaths()

	reader.remove(paths.NetDev)

	sum := c.Refresh()
	if sum.Network.Status != StatusUnavailable {
		t.Fatalf("来源不存在时 status = %q，期望 unavailable", sum.Network.Status)
	}
	if sum.Network.Error == "" {
		t.Fatal("来源不存在时必须说明原因，而不是静默")
	}
	if sum.Network.Interfaces != nil {
		t.Fatalf("没有采集到网卡时不能给出空壳列表冒充数据: %+v", sum.Network.Interfaces)
	}
	if !sum.Degraded {
		t.Fatal("网卡段不可用时整体应标 degraded")
	}

	// 恢复来源后重新有值。
	reader.set(paths.NetDev, string(fixture(t, "netdev-2404.txt")))
	clock.Advance(5 * time.Second)
	sum = c.Refresh()
	if sum.Network.Status != StatusOK || len(sum.Network.Interfaces) != 2 {
		t.Fatalf("恢复后应重新采到网卡: %+v", sum.Network)
	}
}

func TestCollectorSnapshotStaleness(t *testing.T) {
	reader := newFakeReader(t)
	clock := newFakeClock()
	c := newTestCollector(t, reader, clock)

	if _, ok := c.Snapshot(); ok {
		t.Fatal("还没采集过就不该有快照")
	}
	c.Refresh()

	snap, ok := c.Snapshot()
	if !ok {
		t.Fatal("采集后应有快照")
	}
	if snap.Stale {
		t.Fatal("刚采集完不应标陈旧")
	}

	clock.Advance(2 * DefaultInterval)
	if snap, _ = c.Snapshot(); snap.Stale {
		t.Fatal("两个间隔内不算陈旧")
	}
	clock.Advance(2 * DefaultInterval)
	if snap, _ = c.Snapshot(); !snap.Stale {
		t.Fatal("超过 3 个间隔没有新样本必须标陈旧")
	}
	// 陈旧标记只在读取时计算，不能写回缓存把旧数据“续命”。
	if !snap.CollectedAt.Equal(clock.Now().Add(-4 * DefaultInterval)) {
		t.Fatalf("陈旧不应改写 collected_at，实际 %v", snap.CollectedAt)
	}
}

func TestCollectorSummaryCollectsOnColdStart(t *testing.T) {
	reader := newFakeReader(t)
	clock := newFakeClock()
	c := newTestCollector(t, reader, clock)

	sum := c.Summary()
	if sum.CollectedAt.IsZero() {
		t.Fatal("冷启动时 Summary 应同步采集一次，而不是返回空结构")
	}
	if sum.Host.Status != StatusOK {
		t.Fatalf("冷启动采集失败: %+v", sum.Host.Section)
	}
	if _, ok := c.Snapshot(); !ok {
		t.Fatal("冷启动采集后应留下快照")
	}
}

func TestCollectorSnapshotIsCopy(t *testing.T) {
	reader := newFakeReader(t)
	clock := newFakeClock()
	c := newTestCollector(t, reader, clock)
	c.Refresh()

	first, _ := c.Snapshot()
	if len(first.CPU.PerCPU) == 0 || len(first.Network.Interfaces) == 0 {
		t.Fatal("测试前提不成立：快照里应有按核与网卡数据")
	}
	first.CPU.PerCPU[0].Index = 99
	first.Network.Interfaces[0].Name = "tampered"
	first.Host.Hostname = "tampered"

	second, _ := c.Snapshot()
	if second.CPU.PerCPU[0].Index != 0 {
		t.Fatal("调用方改动了快照里的切片，缓存被污染")
	}
	if second.Network.Interfaces[0].Name != "ens4" {
		t.Fatal("调用方改动了网卡列表，缓存被污染")
	}
	if second.Host.Hostname != "upanel-2404" {
		t.Fatal("调用方改动了主机名，缓存被污染")
	}
}

func TestCollectorRunCollectsImmediately(t *testing.T) {
	reader := newFakeReader(t)
	clock := newFakeClock()
	c := NewCollector(Options{Interval: time.Hour, Now: clock.Now, Reader: reader})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Run(ctx)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, ok := c.Snapshot(); ok {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("Run 没有在第一轮就采集")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ctx 取消后 Run 没有退出")
	}
}

func TestCollectorDegradedLoggingDoesNotPanic(t *testing.T) {
	// 没有 Logger 时也不能 panic：采集器在测试与工具里可能不接日志。
	reader := newFakeReader(t)
	reader.remove(DefaultPaths().Loadavg)
	c := newTestCollector(t, reader, newFakeClock())
	sum := c.Refresh()
	if sum.Load.Status != StatusUnavailable {
		t.Fatalf("loadavg 缺失时 status = %q", sum.Load.Status)
	}
}
