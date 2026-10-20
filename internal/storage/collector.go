package storage

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

const (
	// DefaultInterval 是拓扑扫描间隔。PLAN.md 第 8 节：磁盘与计数器每 5 秒。
	DefaultInterval = 5 * time.Second

	// DefaultCapacityTTL 是文件系统容量的缓存时长。PLAN.md 第 8 节：容量 30 秒。
	// 容量比拓扑变得慢，每次扫描都 statfs 一遍没有意义，而且会让 5 秒的扫描
	// 顺带把网络/熔断文件系统也摸一遍。
	DefaultCapacityTTL = 30 * time.Second
)

// Options 是采集器参数。零值可用：5 秒扫描、30 秒容量缓存、真实系统根。
type Options struct {
	Interval    time.Duration
	CapacityTTL time.Duration
	// Root 指向真实的 /sys、/proc、/dev；测试时指向夹具。
	Root   Root
	Logger *slog.Logger
}

// cachedCapacity 是一个挂载点的容量缓存。
type cachedCapacity struct {
	cap *Capacity
	at  time.Time
}

// Collector 周期扫描块设备拓扑并缓存最近一份快照。
//
// 失败策略：扫描失败**不清空**节点，只把 status 改成 error、记下原因，
// nodes/edges/collected_at 停在上一次成功的样子。理由是“读不到”并不等于
// “服务器上没有磁盘”，把列表清空会让界面显示成机器被拔了盘。
type Collector struct {
	interval    time.Duration
	capacityTTL time.Duration
	root        Root
	logger      *slog.Logger

	mu       sync.Mutex
	cached   Topology
	hasCache bool
	caps     map[string]cachedCapacity

	// lastError 只在日志里用：状态翻转时记一条，不刷屏。
	lastError  string
	loggedOnce bool
}

// NewCollector 构造采集器。参数缺省按默认值补齐，不返回错误：
// 采集失败是运行期状态（快照报 error），不是启动条件。
func NewCollector(opts Options) *Collector {
	interval := opts.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	ttl := opts.CapacityTTL
	if ttl <= 0 {
		ttl = DefaultCapacityTTL
	}
	root := opts.Root
	if root.Sys == "" || root.Proc == "" {
		root = SystemRoot()
	}
	return &Collector{
		interval:    interval,
		capacityTTL: ttl,
		root:        root,
		logger:      opts.Logger,
		caps:        map[string]cachedCapacity{},
	}
}

// Interval 返回扫描间隔。
func (c *Collector) Interval() time.Duration { return c.interval }

// Run 立刻扫描一次，然后按间隔轮询，直到 ctx 结束。
func (c *Collector) Run(ctx context.Context) {
	c.Refresh(ctx)
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.Refresh(ctx)
		}
	}
}

// Refresh 同步扫描一次，更新缓存并返回这份快照。
func (c *Collector) Refresh(ctx context.Context) Topology {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.root.now()
	topo, err := Scan(ctx, c.root)
	if err != nil {
		out := Topology{
			Status:     StatusError,
			Error:      err.Error(),
			IntervalMS: c.interval.Milliseconds(),
			Nodes:      []Device{},
			Edges:      []Edge{},
		}
		if c.hasCache {
			// 保留上一次成功的快照内容，只改状态与原因。
			out = c.cached
			out.Status = StatusError
			out.Error = err.Error()
			out.IntervalMS = c.interval.Milliseconds()
		}
		c.cached = out
		c.logTransition(out)
		return cloneTopology(out)
	}

	topo.Status = StatusOK
	topo.Error = ""
	topo.IntervalMS = c.interval.Milliseconds()
	c.fillCapacities(ctx, topo, now)

	c.cached = cloneTopology(*topo)
	c.hasCache = true
	c.logTransition(c.cached)
	return cloneTopology(c.cached)
}

// Snapshot 返回最近一次快照。还没有成功扫描过时第二个返回值为 false。
func (c *Collector) Snapshot() (Topology, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.hasCache {
		return Topology{}, false
	}
	return cloneTopology(c.cached), true
}

// Topology 返回一份可直接返回给调用方的快照。
//
// 还没有任何快照时（代理刚启动、轮询还没跑第一轮）同步扫描一次，
// 而不是返回空结构：空结构在界面上与“这台机器没有磁盘”没有区别。
func (c *Collector) Topology(ctx context.Context) Topology {
	if snap, ok := c.Snapshot(); ok {
		return snap
	}
	return c.Refresh(ctx)
}

// fillCapacities 按 30 秒缓存补齐挂载点容量。
//
// 容量只在实际挂载成功、statfs 也成功时才存在；读不到就是 null，
// 不是 0 —— “这个盘满了”和“我不知道这个盘多满”是两件事。
func (c *Collector) fillCapacities(ctx context.Context, topo *Topology, now time.Time) {
	if c.root.Caps == nil {
		return
	}
	done := map[string]bool{}
	for i := range topo.Nodes {
		fs := topo.Nodes[i].Filesystem
		if fs == nil {
			continue
		}
		for j := range fs.Mounts {
			m := &fs.Mounts[j]
			if done[m.Path] {
				continue // 同一挂载点会被多个设备共用（绑定挂载），只 statfs 一次
			}
			done[m.Path] = true

			if cc, ok := c.caps[m.Path]; ok && now.Sub(cc.at) < c.capacityTTL {
				m.Capacity = cc.cap
				continue
			}
			capv, err := c.root.Caps.Capacity(ctx, m.Path)
			if err != nil {
				// 缓存作废：挂载点可能已经卸载或文件系统出了问题，
				// 继续用旧数字会让人以为容量是新的。
				delete(c.caps, m.Path)
				m.Capacity = nil
				continue
			}
			// 读取器没打时间戳时用本次扫描时间补上：容量必须带时间，
			// 否则前端无法判断这个数字有多旧。
			if capv.CollectedAt.IsZero() {
				capv.CollectedAt = now
			}
			c.caps[m.Path] = cachedCapacity{cap: capv, at: now}
			m.Capacity = capv
		}
	}
}

// logTransition 只在扫描状态翻转时记日志。
func (c *Collector) logTransition(topo Topology) {
	if c.logger == nil {
		return
	}
	if !c.loggedOnce || topo.Error != c.lastError {
		c.loggedOnce = true
		c.lastError = topo.Error
		if topo.Error != "" {
			c.logger.Warn("存储拓扑扫描失败，保留上一次快照",
				"error", topo.Error, "collected_at", topo.CollectedAt)
		} else {
			c.logger.Info("存储拓扑扫描恢复",
				"collected_at", topo.CollectedAt, "devices", len(topo.Nodes))
		}
	}
}

// cloneTopology 深拷贝一份快照。
//
// 必须深拷贝：调用方拿到的是可以随意读的对象，如果它和采集器共用同一份
// 指针，下游一次修改就会污染缓存里的“上一次成功快照”。
func cloneTopology(t Topology) Topology {
	out := t
	out.Nodes = make([]Device, len(t.Nodes))
	for i := range t.Nodes {
		out.Nodes[i] = cloneDevice(t.Nodes[i])
	}
	out.Edges = append([]Edge{}, t.Edges...)
	return out
}

func cloneDevice(d Device) Device {
	out := d
	out.Model = cloneString(d.Model)
	out.Serial = cloneString(d.Serial)
	out.WWN = cloneString(d.WWN)
	out.Transport = cloneString(d.Transport)

	out.Capabilities = append([]Capability{}, d.Capabilities...)
	out.Holders = append([]string{}, d.Holders...)
	out.Protection.Reasons = append([]string{}, d.Protection.Reasons...)
	out.Protection.Sources = append([]string{}, d.Protection.Sources...)

	if d.Swap != nil {
		s := *d.Swap
		out.Swap = &s
	}
	if d.Filesystem != nil {
		fs := *d.Filesystem
		fs.Type = cloneString(d.Filesystem.Type)
		fs.UUID = cloneString(d.Filesystem.UUID)
		fs.Label = cloneString(d.Filesystem.Label)
		fs.Mounts = make([]Mount, len(d.Filesystem.Mounts))
		for j, m := range d.Filesystem.Mounts {
			nm := m
			nm.Options = append([]string{}, m.Options...)
			if m.Capacity != nil {
				cp := *m.Capacity
				nm.Capacity = &cp
			}
			fs.Mounts[j] = nm
		}
		out.Filesystem = &fs
	}
	if d.PartitionTable != nil {
		pt := *d.PartitionTable
		pt.Partitions = append([]Partition{}, d.PartitionTable.Partitions...)
		pt.FreeRegions = append([]FreeRegion{}, d.PartitionTable.FreeRegions...)
		out.PartitionTable = &pt
	}
	out.Health.TemperatureCelsius = cloneFloat(d.Health.TemperatureCelsius)
	out.Health.CheckedAt = cloneTime(d.Health.CheckedAt)
	out.Health.Details = cloneString(d.Health.Details)
	return out
}

func cloneString(s *string) *string {
	if s == nil {
		return nil
	}
	v := *s
	return &v
}

func cloneFloat(f *float64) *float64 {
	if f == nil {
		return nil
	}
	v := *f
	return &v
}

func cloneTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := *t
	return &v
}
