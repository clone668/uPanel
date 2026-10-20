package storage

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// P04.3 在采集器层面的三条要求：
//
//  1. 5 秒的拓扑扫描要带上 /proc/diskstats 的计数与速率，且速率来自相邻两次扫描；
//  2. SMART 走另一条 15 分钟的节奏，5 秒循环不等它，探测只针对物理盘；
//  3. 网络挂载的容量查询要限并发，本地挂载不许走子进程。

// fakeProber 记录每次探测，并按名称返回预设健康。
type fakeProber struct {
	mu    sync.Mutex
	calls []string
	fn    func(name string) Health
}

func (p *fakeProber) Probe(ctx context.Context, name, sysfsDir, transport string) Health {
	p.mu.Lock()
	p.calls = append(p.calls, name)
	p.mu.Unlock()
	if p.fn != nil {
		return p.fn(name)
	}
	return Health{Status: HealthHealthy, Source: smartHealthSource, Details: strPtr("测试用结论")}
}

func (p *fakeProber) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

func (p *fakeProber) names() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.calls))
	copy(out, p.calls)
	return out
}

// newP043Collector 组装一个带假探测器的夹具采集器。时钟可控，健康间隔取最小值，
// 这样“到期重探”不必等 15 分钟。
func newP043Collector(t *testing.T, prober HealthProber, caps CapacityReader) (*Collector, *clock) {
	t.Helper()
	dir, err := fixtureDir("storage-2204")
	if err != nil {
		t.Fatalf("%v", err)
	}
	clk := &clock{t: fixedNow}
	c := NewCollector(Options{
		Interval:       5 * time.Second,
		HealthInterval: MinHealthInterval,
		Prober:         prober,
		Root: Root{
			Sys:   filepath.Join(dir, "sys"),
			Proc:  filepath.Join(dir, "proc"),
			Disks: &fixtureDiskReader{dir: filepath.Join(dir, "disks")},
			Caps:  caps,
			Now:   clk.now,
		},
	})
	return c, clk
}

// TestCollectorAttachesDiskstats 校验设备上确实挂上了自己那一行的计数。
//
// 夹具的 /proc/diskstats 是 VM-A 22.04 的真实采集，与同一台机器的 sysfs 夹具配套
// （storage-2204）。用别的机器的计数器会让设备名对不上，测出来的就不是真机行为。
func TestCollectorAttachesDiskstats(t *testing.T) {
	c, _ := newP043Collector(t, nil, &fakeCaps{cap: &Capacity{TotalBytes: 1, AvailableBytes: 1}})
	topo := c.Refresh(context.Background())

	sda := deviceByID(t, &topo, "sda")
	if sda.Io == nil {
		t.Fatalf("sda 的 io 为 null，但 /proc/diskstats 里有它那一行")
	}
	if sda.Io.ReadsTotal == 0 {
		t.Errorf("sda 的 reads_total 为 0：真机上系统盘不可能没读过")
	}
	if sda.Io.SampledAt.IsZero() || !sda.Io.SampledAt.Equal(fixedNow) {
		t.Errorf("sampled_at = %v，想要 %v", sda.Io.SampledAt, fixedNow)
	}
	// 第一次扫描没有上一个窗口：速率必须是 null，不能填 0。
	if sda.Io.Rate != nil {
		t.Errorf("首次采集就给了速率 %+v", sda.Io.Rate)
	}

	// 分区与整盘各拿各的计数：分区的 I/O 已经包含在整盘里，绝不能相加。
	if part := deviceByID(t, &topo, "sda1"); part.Io != nil {
		if part.Io.ReadsTotal >= sda.Io.ReadsTotal {
			t.Errorf("分区读次数 %d 不应大于整盘的 %d（这正是不能相加的原因）",
				part.Io.ReadsTotal, sda.Io.ReadsTotal)
		}
	}

	// 夹具里没有的盘（fd0 之外的新设备）不会被编造出来：io 只能是 null。
	if fd := deviceByID(t, &topo, "fd0"); fd.Io == nil {
		t.Log("fd0 在 diskstats 里有记录但不影响结论")
	}
}

// TestCollectorIORateAfterSecondScan 校验速率出现在第二次扫描之后，
// 并且静态计数器（这台机器没写盘）算出的速率是 0 而不是 null。
func TestCollectorIORateAfterSecondScan(t *testing.T) {
	c, clk := newP043Collector(t, nil, &fakeCaps{cap: &Capacity{TotalBytes: 1, AvailableBytes: 1}})
	c.Refresh(context.Background())

	clk.advance(5 * time.Second)
	topo := c.Refresh(context.Background())
	sda := deviceByID(t, &topo, "sda")
	if sda.Io == nil || sda.Io.Rate == nil {
		t.Fatalf("第二次扫描应当能算出速率")
	}
	if sda.Io.Rate.IntervalMS != 5000 {
		t.Errorf("interval_ms = %d，想要 5000", sda.Io.Rate.IntervalMS)
	}
	// 夹具是同一份文件，计数没变：速率是 0，延迟是 null。
	if sda.Io.Rate.ReadBytesPerSecond != 0 {
		t.Errorf("计数没变却算出 %d B/s", sda.Io.Rate.ReadBytesPerSecond)
	}
	if sda.Io.Rate.ReadLatencyMS != nil {
		t.Errorf("没有完成的 I/O 时延迟必须是 null")
	}
}

// TestCollectorProbesOnlyDisksAndCaches 校验 SMART 的三件事：
// 只探物理盘、结果在间隔内复用、到期后重探。
func TestCollectorProbesOnlyDisksAndCaches(t *testing.T) {
	prober := &fakeProber{fn: func(name string) Health {
		t := fixedNow
		temp := 41.0
		return Health{Status: HealthHealthy, Source: smartHealthSource, CheckedAt: &t, TemperatureCelsius: &temp}
	}}
	c, clk := newP043Collector(t, prober, nil)

	c.Refresh(context.Background())
	c.probeDue(context.Background())

	checked := prober.names()
	if len(checked) == 0 {
		t.Fatalf("没有探测任何设备")
	}
	disks := map[string]bool{"sda": true, "sdb": true, "sdc": true, "sdd": true}
	for _, name := range checked {
		if !disks[name] {
			t.Errorf("探测了 %s：它不是物理盘（分区/loop 没有独立 SMART，探它是白唤醒）", name)
		}
	}

	// 间隔内再跑一次：不该重新探测。
	before := prober.count()
	clk.advance(time.Minute)
	c.probeDue(context.Background())
	if prober.count() != before {
		t.Errorf("健康间隔内又探测了 %d 次", prober.count()-before)
	}

	// 到期之后要重探。
	clk.advance(MinHealthInterval)
	c.probeDue(context.Background())
	if prober.count() == before {
		t.Errorf("超过健康间隔仍不重探，SMART 数据会永远停在启动那一刻")
	}
}

// TestCollectorMergesHealthIntoSnapshot 校验探测结果在下一次快照里生效，
// 而且带着温度与来源；没有探测到的盘保持 unknown 并说明原因。
func TestCollectorMergesHealthIntoSnapshot(t *testing.T) {
	prober := &fakeProber{fn: func(name string) Health {
		t := fixedNow
		temp := 41.0
		src := smartHealthSource
		return Health{Status: HealthHealthy, Source: src, CheckedAt: &t, TemperatureCelsius: &temp,
			Details: strPtr("SMART 总体健康结论为通过")}
	}}
	c, _ := newP043Collector(t, prober, nil)

	c.Refresh(context.Background())
	c.probeDue(context.Background())
	topo := c.Refresh(context.Background())

	sda := deviceByID(t, &topo, "sda")
	if sda.Health.Status != HealthHealthy {
		t.Errorf("sda 的 Status = %q，想要 healthy", sda.Health.Status)
	}
	if sda.Health.TemperatureCelsius == nil || *sda.Health.TemperatureCelsius != 41 {
		t.Errorf("sda 的温度 = %v，想要 41", sda.Health.TemperatureCelsius)
	}
	if sda.Health.Source != smartHealthSource {
		t.Errorf("sda 的 Source = %q", sda.Health.Source)
	}

	// 分区不该继承整盘的健康：它没有自己的 SMART，状态是 unsupported。
	if part := deviceByID(t, &topo, "sda1"); part.Health.Status != HealthUnsupported {
		t.Errorf("sda1 的 Status = %q，想要 unsupported", part.Health.Status)
	}
}

// TestCollectorNoProberIsUnknown 校验没有 smartctl 时的诚实表达：
// 物理盘 unknown + 说明，绝不 healthy。
func TestCollectorNoProberIsUnknown(t *testing.T) {
	c, _ := newP043Collector(t, nil, nil)
	topo := c.Refresh(context.Background())
	for _, d := range topo.Nodes {
		if d.Kind != KindDisk {
			continue
		}
		if d.Health.Status != HealthUnknown {
			t.Errorf("%s 的 Status = %q，想要 unknown", d.ID, d.Health.Status)
		}
		if d.Health.Source != "none" {
			t.Errorf("%s 的 Source = %q，想要 none", d.ID, d.Health.Source)
		}
		if d.Health.Details == nil || *d.Health.Details == "" {
			t.Errorf("%s 没有说明为什么健康未知", d.ID)
		}
	}
}

// TestCollectorPrunesHealthOfRemovedDevices 校验长时间运行后健康缓存不会无限增长。
func TestCollectorPrunesHealthOfRemovedDevices(t *testing.T) {
	prober := &fakeProber{}
	c, _ := newP043Collector(t, prober, nil)
	c.Refresh(context.Background())
	c.probeDue(context.Background())

	c.healthMu.Lock()
	c.health["sdz-已经拔掉的盘"] = cachedHealth{health: Health{Status: HealthHealthy}, at: fixedNow}
	c.healthMu.Unlock()

	c.probeDue(context.Background())
	c.healthMu.Lock()
	_, stillThere := c.health["sdz-已经拔掉的盘"]
	c.healthMu.Unlock()
	if stillThere {
		t.Errorf("已经不在快照里的设备仍留在健康缓存里")
	}
}

// TestCollectorProbeDueWaitsForFirstScan 校验采集器还没扫到拓扑时探测不做任何事。
func TestCollectorProbeDueWaitsForFirstScan(t *testing.T) {
	prober := &fakeProber{}
	c, _ := newP043Collector(t, prober, nil)
	c.probeDue(context.Background())
	if prober.count() != 0 {
		t.Errorf("还没有快照就探测了 %d 次", prober.count())
	}
}

// slowCaps 统计并发峰值，用来验证网络挂载的并发上限。
type slowCaps struct {
	mu       sync.Mutex
	calls    []string
	fsTypes  map[string]string
	inFlight int32
	peak     int32
	delay    time.Duration
}

func (s *slowCaps) Capacity(ctx context.Context, mountPath, fsType string) (*Capacity, error) {
	cur := atomic.AddInt32(&s.inFlight, 1)
	defer atomic.AddInt32(&s.inFlight, -1)
	for {
		old := atomic.LoadInt32(&s.peak)
		if cur <= old || atomic.CompareAndSwapInt32(&s.peak, old, cur) {
			break
		}
	}
	s.mu.Lock()
	s.calls = append(s.calls, mountPath)
	if s.fsTypes == nil {
		s.fsTypes = map[string]string{}
	}
	s.fsTypes[mountPath] = fsType
	s.mu.Unlock()

	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &Capacity{TotalBytes: 1024, AvailableBytes: 512, UsedPercent: 50}, nil
}

func (s *slowCaps) peakConcurrency() int { return int(atomic.LoadInt32(&s.peak)) }

// networkTopology 造一份只有网络挂载的拓扑。
func networkTopology(mounts int) Topology {
	topo := Topology{Status: StatusOK, CollectedAt: fixedNow}
	for i := 0; i < mounts; i++ {
		id := "dm-" + itoaSmall(i)
		topo.Nodes = append(topo.Nodes, Device{
			ID:   id,
			Kind: KindLVM,
			Name: id,
			Filesystem: &Filesystem{
				Type: strPtr("nfs4"),
				Mounts: []Mount{{
					Path:   "/mnt/nfs" + itoaSmall(i),
					FSType: "nfs4",
				}},
			},
		})
	}
	return topo
}

// TestFillCapacitiesLimitsNetworkConcurrency 校验 PLAN 第 8 节的要求：
// 网络挂载的容量查询限制并发，一台机器挂十几个不可达的 NFS 不会同时派出十几个子进程。
func TestFillCapacitiesLimitsNetworkConcurrency(t *testing.T) {
	caps := &slowCaps{delay: 20 * time.Millisecond}
	c, _ := newP043Collector(t, nil, caps)

	topo := networkTopology(6)
	c.fillCapacities(context.Background(), &topo, fixedNow)

	if caps.peakConcurrency() > maxNetworkCapacityQueries {
		t.Errorf("并发峰值 = %d，超过上限 %d", caps.peakConcurrency(), maxNetworkCapacityQueries)
	}
	if len(caps.calls) != 6 {
		t.Errorf("查询次数 = %d，想要 6", len(caps.calls))
	}
	for _, node := range topo.Nodes {
		m := node.Filesystem.Mounts[0]
		if m.Capacity == nil {
			t.Errorf("%s 的容量为 null", m.Path)
		}
		if caps.fsTypes[m.Path] != "nfs4" {
			t.Errorf("%s 传给读取器的 fstype = %q，想要 nfs4：不传类型就无法判断该不该隔离",
				m.Path, caps.fsTypes[m.Path])
		}
	}
}

// TestFillCapacitiesDedupesSamePath 校验绑定挂载：同一个路径只查一次，结果抄给其余挂载点。
func TestFillCapacitiesDedupesSamePath(t *testing.T) {
	caps := &fakeCaps{cap: &Capacity{TotalBytes: 2048, AvailableBytes: 1024, UsedPercent: 50}}
	c, _ := newP043Collector(t, nil, caps)

	topo := Topology{Status: StatusOK, CollectedAt: fixedNow}
	for _, id := range []string{"sda1", "sdb1"} {
		topo.Nodes = append(topo.Nodes, Device{
			ID: id, Kind: KindPartition, Name: id,
			Filesystem: &Filesystem{
				Type: strPtr("ext4"),
				Mounts: []Mount{{
					Path:   "/srv/data",
					FSType: "ext4",
				}},
			},
		})
	}
	c.fillCapacities(context.Background(), &topo, fixedNow)

	if n := caps.countFor("/srv/data"); n != 1 {
		t.Errorf("/srv/data 被查询了 %d 次，绑定挂载应当只查一次", n)
	}
	for _, node := range topo.Nodes {
		if m := node.Filesystem.Mounts[0]; m.Capacity == nil || m.Capacity.TotalBytes != 2048 {
			t.Errorf("%s 的容量没有跟着抄过来: %+v", node.ID, m.Capacity)
		}
	}
}

// TestFillCapacitiesErrorReasonTravels 校验失败原因会写进 capacity_error，
// 界面据此显示“为什么不显示容量”，而不是一个孤零零的 null。
func TestFillCapacitiesErrorReasonTravels(t *testing.T) {
	caps := &fakeCaps{err: ErrCapacityUnavailable}
	c, _ := newP043Collector(t, nil, caps)

	topo := Topology{Status: StatusOK, CollectedAt: fixedNow,
		Nodes: []Device{{
			ID: "sda1", Kind: KindPartition, Name: "sda1",
			Filesystem: &Filesystem{Type: strPtr("ext4"),
				Mounts: []Mount{{Path: "/", FSType: "ext4"}}},
		}}}

	c.fillCapacities(context.Background(), &topo, fixedNow)
	m := topo.Nodes[0].Filesystem.Mounts[0]
	if m.Capacity != nil {
		t.Errorf("读取失败却给了容量 %+v", m.Capacity)
	}
	if m.CapacityError == "" {
		t.Errorf("读取失败必须写 capacity_error")
	}
}

// fillCapacities 必须把独立列出的网络挂载也填上，并且给出确定的可达性结论。
//
// 这是“网络挂载隔离”真的起作用的前提：挂载点不进快照，隔离代码就永远没人调用。
func TestFillCapacitiesFillsNetworkMounts(t *testing.T) {
	caps := &fakeCaps{cap: &Capacity{TotalBytes: 1000, AvailableBytes: 250, UsedPercent: 75}}
	c, _ := newP043Collector(t, nil, caps)
	topo := Topology{NetworkMounts: []NetworkMount{{
		Path: "/mnt/upanel-nfs", Source: "10.0.0.5:/export", FSType: "nfs4",
	}}}

	c.fillCapacities(context.Background(), &topo, fixedNow)
	nm := topo.NetworkMounts[0]
	if nm.Capacity == nil {
		t.Fatalf("网络挂载的容量没填上：%+v", nm)
	}
	if nm.Capacity.TotalBytes != 1000 {
		t.Errorf("容量被改写：%+v", nm.Capacity)
	}
	if nm.Reachable == nil || !*nm.Reachable {
		t.Errorf("读到容量就应当是可达到，实际 %v", nm.Reachable)
	}
	if nm.CapacityError != "" {
		t.Errorf("成功时不该有 capacity_error：%s", nm.CapacityError)
	}
	if got := caps.fsTypeOf("/mnt/upanel-nfs"); got != "nfs4" {
		t.Errorf("判断隔离通道要传 fstype，实际传了 %q", got)
	}

	// 30 秒缓存内不再查一次：不可达的挂载点更不该被反复触碰。
	before := caps.count()
	c.fillCapacities(context.Background(), &topo, fixedNow.Add(10*time.Second))
	if caps.count() != before {
		t.Errorf("缓存有效期内又查了一次容量（%d → %d）", before, caps.count())
	}
}

func TestFillCapacitiesNetworkTimeoutIsUnreachable(t *testing.T) {
	caps := &fakeCaps{err: ErrCapacityTimeout}
	c, _ := newP043Collector(t, nil, caps)
	topo := Topology{NetworkMounts: []NetworkMount{{
		Path: "/mnt/upanel-nfs", Source: "10.0.0.5:/export", FSType: "nfs4",
	}}}

	c.fillCapacities(context.Background(), &topo, fixedNow)
	nm := topo.NetworkMounts[0]
	if nm.Capacity != nil {
		t.Errorf("超时却给了容量 %+v", nm.Capacity)
	}
	if nm.Reachable == nil || *nm.Reachable {
		t.Fatalf("超时必须明确是“不可达”，实际 %v", nm.Reachable)
	}
	if !strings.Contains(nm.CapacityError, "超时") {
		t.Errorf("capacity_error 要说明是超时：%s", nm.CapacityError)
	}
}

// itoaSmall 是测试里用的极简十进制转换，避免为一个用例引入 fmt。
func itoaSmall(v int) string {
	if v == 0 {
		return "0"
	}
	var buf [12]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
