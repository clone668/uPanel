package storage

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeCaps 是一个可控的容量读取器：记录被问过的挂载点，可以按需返回错误。
//
// fsTypes 记下每次查询声明的文件系统类型：网络挂载要交给隔离查询，
// 这个字段让测试能验证“文件系统类型确实传下去了”。
type fakeCaps struct {
	mu      sync.Mutex
	calls   []string
	fsTypes map[string]string
	err     error
	errFor  map[string]error
	hangs   map[string]bool
	cap     *Capacity
}

func (f *fakeCaps) Capacity(ctx context.Context, mountPath, fsType string) (*Capacity, error) {
	f.mu.Lock()
	f.calls = append(f.calls, mountPath)
	if f.fsTypes == nil {
		f.fsTypes = map[string]string{}
	}
	f.fsTypes[mountPath] = fsType
	err := f.err
	if e, ok := f.errFor[mountPath]; ok {
		err = e
	}
	capv := f.cap
	f.mu.Unlock()

	if err != nil {
		return nil, err
	}
	if capv == nil {
		return nil, ErrCapacityUnavailable
	}
	if f.hangOn(ctx, mountPath) {
		return nil, ErrCapacityTimeout
	}
	c := *capv
	return &c, nil
}

// hangOn 模拟一个永远不返回的远端：只有 ctx 结束才放手。
func (f *fakeCaps) hangOn(ctx context.Context, mountPath string) bool {
	f.mu.Lock()
	hang := f.hangs[mountPath]
	f.mu.Unlock()
	if !hang {
		return false
	}
	<-ctx.Done()
	return true
}

// fsTypeOf 返回某个挂载点被查询时报给读取器的文件系统类型。
func (f *fakeCaps) fsTypeOf(mountPath string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fsTypes[mountPath]
}

func (f *fakeCaps) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeCaps) countFor(mountPath string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, p := range f.calls {
		if p == mountPath {
			n++
		}
	}
	return n
}

// clock 是可控时钟，用来验证 5 秒扫描与 30 秒容量缓存。
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newFixtureCollector(t *testing.T, fixture string) (*Collector, *fakeCaps, *clock) {
	t.Helper()
	dir, err := fixtureDir(fixture)
	if err != nil {
		t.Fatalf("%v", err)
	}
	clk := &clock{t: fixedNow}
	caps := &fakeCaps{cap: &Capacity{
		TotalBytes: 41874865664, AvailableBytes: 30000000000, UsedPercent: 28.3,
		InodeTotal: 2621440, InodeAvailable: 2500000, InodeUsedPercent: 4.6,
	}}
	c := NewCollector(Options{
		Interval: 5 * time.Second,
		Root: Root{
			Sys:   filepath.Join(dir, "sys"),
			Proc:  filepath.Join(dir, "proc"),
			Disks: &fixtureDiskReader{dir: filepath.Join(dir, "disks")},
			Caps:  caps,
			Now:   clk.now,
		},
	})
	return c, caps, clk
}

func TestCollectorFirstScanFillsTopology(t *testing.T) {
	c, caps, _ := newFixtureCollector(t, "storage-2404")

	if _, ok := c.Snapshot(); ok {
		t.Fatalf("还没扫描过就给了快照")
	}
	topo := c.Topology(context.Background())
	if topo.Status != StatusOK {
		t.Fatalf("Status = %q（%s），想要 ok", topo.Status, topo.Error)
	}
	if topo.IntervalMS != 5000 {
		t.Errorf("IntervalMS = %d，想要 5000", topo.IntervalMS)
	}
	if len(topo.Nodes) == 0 {
		t.Fatalf("没有采集到任何设备")
	}
	if caps.count() == 0 {
		t.Errorf("有挂载点却没有读取容量")
	}

	// 挂载点容量必须挂到对应的挂载上，而且带自己的采集时间。
	root := deviceByID(t, &topo, "sda1")
	if root.Filesystem == nil || len(root.Filesystem.Mounts) == 0 {
		t.Fatalf("sda1 应有挂载点")
	}
	m := root.Filesystem.Mounts[0]
	if m.Path != "/" {
		t.Fatalf("sda1 的挂载点 = %q，想要 /", m.Path)
	}
	if m.Capacity == nil {
		t.Fatalf("容量读取成功了，mount.capacity 却是 null")
	}
	if m.Capacity.UsedPercent != 28.3 {
		t.Errorf("UsedPercent = %v，想要 28.3", m.Capacity.UsedPercent)
	}
	if !m.Capacity.CollectedAt.Equal(fixedNow) {
		t.Errorf("容量时间 = %v，想要 %v", m.Capacity.CollectedAt, fixedNow)
	}
}

func TestCollectorFailedScanKeepsLastGood(t *testing.T) {
	c, _, _ := newFixtureCollector(t, "storage-2404")
	good := c.Refresh(context.Background())
	if good.Status != StatusOK {
		t.Fatalf("首次扫描失败: %s", good.Error)
	}

	// 把 sys 指到一个空目录：扫描必然失败。
	c.root.Sys = t.TempDir()
	bad := c.Refresh(context.Background())

	if bad.Status != StatusError {
		t.Fatalf("Status = %q，想要 error", bad.Status)
	}
	if bad.Error == "" {
		t.Errorf("失败时必须给出原因")
	}
	if len(bad.Nodes) != len(good.Nodes) {
		t.Errorf("扫描失败后节点数从 %d 变成 %d：失败不等于机器被拔了盘",
			len(good.Nodes), len(bad.Nodes))
	}
	if !bad.CollectedAt.Equal(good.CollectedAt) {
		t.Errorf("失败后 collected_at 从 %v 变成 %v：数据还是上一次的，时间不能改",
			good.CollectedAt, bad.CollectedAt)
	}
	if bad.SnapshotVersion != good.SnapshotVersion {
		t.Errorf("内容没变，快照版本却变了")
	}
}

func TestCollectorCapacityTTL(t *testing.T) {
	c, caps, clk := newFixtureCollector(t, "storage-2404")

	c.Refresh(context.Background())
	first := caps.count()
	if first == 0 {
		t.Fatalf("第一次扫描没有读容量")
	}

	// 5 秒后又扫了一次：拓扑要更新，容量还在 30 秒缓存里，不能再 statfs。
	clk.advance(5 * time.Second)
	topo := c.Refresh(context.Background())
	if caps.count() != first {
		t.Errorf("5 秒后就重读了容量（%d → %d），PLAN 第 8 节要求缓存 30 秒",
			first, caps.count())
	}
	// 缓存复用不等于没数据：容量仍在。
	if m := deviceByID(t, &topo, "sda1").Filesystem.Mounts[0]; m.Capacity == nil {
		t.Errorf("缓存命中时容量不该变成 null")
	}

	// 31 秒后：容量必须重读，而且时间戳更新为这次扫描的时间。
	clk.advance(26 * time.Second)
	topo = c.Refresh(context.Background())
	if caps.count() == first {
		t.Errorf("超过 30 秒仍用旧缓存")
	}
	m := deviceByID(t, &topo, "sda1").Filesystem.Mounts[0]
	if m.Capacity == nil {
		t.Fatalf("重读后容量为 null")
	}
	if !m.Capacity.CollectedAt.Equal(fixedNow.Add(31 * time.Second)) {
		t.Errorf("容量时间 = %v，想要 %v", m.Capacity.CollectedAt, fixedNow.Add(31*time.Second))
	}
}

func TestCollectorCapacityErrorIsNull(t *testing.T) {
	c, caps, _ := newFixtureCollector(t, "storage-2404")
	caps.err = errors.New("statfs: permission denied")
	topo := c.Refresh(context.Background())

	for _, d := range topo.Nodes {
		if d.Filesystem == nil {
			continue
		}
		for _, m := range d.Filesystem.Mounts {
			if m.Capacity != nil {
				t.Errorf("%s 的挂载点 %s：容量读失败却给了 %+v，必须是 null",
					d.ID, m.Path, m.Capacity)
			}
		}
	}
	// 拓扑本身仍然成功：容量读不到不该让整份快照变成错误。
	if topo.Status != StatusOK {
		t.Errorf("Status = %q，容量失败不该影响拓扑", topo.Status)
	}
}

func TestCollectorDropsStaleCapacityOnError(t *testing.T) {
	c, caps, clk := newFixtureCollector(t, "storage-2404")
	c.Refresh(context.Background())
	if caps.count() == 0 {
		t.Fatalf("第一次没读容量")
	}

	// 缓存过期后读取失败：必须把旧数字丢掉，而不是继续展示 30 秒前的容量。
	caps.err = errors.New("statfs: stale file handle")
	clk.advance(31 * time.Second)
	topo := c.Refresh(context.Background())
	m := deviceByID(t, &topo, "sda1").Filesystem.Mounts[0]
	if m.Capacity != nil {
		t.Errorf("读取失败后仍返回旧容量 %+v", m.Capacity)
	}
}

func TestCollectorSnapshotIsCopy(t *testing.T) {
	c, _, _ := newFixtureCollector(t, "storage-2404")
	c.Refresh(context.Background())

	snap, ok := c.Snapshot()
	if !ok {
		t.Fatalf("没有快照")
	}
	snap.Nodes[0].Protection.Reasons = append(snap.Nodes[0].Protection.Reasons, "被外部改坏")
	snap.Nodes[0].Name = "hacked"
	if len(snap.Edges) > 0 {
		snap.Edges[0].Relation = "hacked"
	}

	again, _ := c.Snapshot()
	if again.Nodes[0].Name == "hacked" {
		t.Errorf("调用方改了返回的快照，缓存被污染了")
	}
	for _, r := range again.Nodes[0].Protection.Reasons {
		if r == "被外部改坏" {
			t.Errorf("保护理由切片被调用方改动污染")
		}
	}
	for _, e := range again.Edges {
		if e.Relation == "hacked" {
			t.Errorf("边被调用方改动污染")
		}
	}
}

func TestCollectorRunScansImmediately(t *testing.T) {
	c, _, _ := newFixtureCollector(t, "storage-2404")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		c.Run(ctx)
		close(done)
	}()

	deadline := time.After(5 * time.Second)
	for {
		if _, ok := c.Snapshot(); ok {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("Run 启动后没有立刻扫描")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("ctx 取消后 Run 没有退出")
	}
}
