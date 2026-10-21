package storage

import (
	"context"
	"errors"
	"log/slog"
	"strings"
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

	// DefaultHealthInterval 是 SMART 的采集间隔。PLAN.md 第 8 节：15 分钟，
	// 允许配置为 5—60 分钟；运行期钳制在这个区间里，配错了也不会变成 5 秒一次。
	DefaultHealthInterval = 15 * time.Minute
	MinHealthInterval     = 5 * time.Minute
	MaxHealthInterval     = 60 * time.Minute
)

// Options 是采集器参数。零值可用：5 秒扫描、30 秒容量缓存、真实系统根、
// 不做 SMART 采集（Health 为 nil 时物理盘健康保持 unknown）。
type Options struct {
	Interval       time.Duration
	CapacityTTL    time.Duration
	HealthInterval time.Duration
	// Prober 读取 SMART 健康；nil 表示本机没有可用的采集手段（健康保持 unknown）。
	Prober HealthProber
	// NetworkCapacityTimeout 是网络挂载容量查询的时限；0 时用默认值。
	NetworkCapacityTimeout time.Duration
	// Env 是存储写动作预检需要的外部探针（签名读取、占用检查）。
	//
	// 零值表示**不填能力列表**：设备上的 capabilities 保持空数组，界面据此不显示
	// 破坏性按钮。这是刻意的默认——忘了接线时得到的是“不知道能做什么”，
	// 而不是“什么都能做”。生产装配见 cmd/upanel-agent/main.go。
	Env Env
	// Root 指向真实的 /sys、/proc、/dev；测试时指向夹具。
	Root   Root
	Logger *slog.Logger
}

// HealthProber 读取一台设备的健康。生产实现是 *SmartProber。
//
// 约定：永远返回一个 Health，不返回 error。采集失败要变成 unknown 并写清原因，
// 而不是让调用方用“零值 Health”顶上——零值就是 healthy 之前最危险的那种默认。
type HealthProber interface {
	Probe(ctx context.Context, name, sysfsDir, transport string) Health
}

// cachedCapacity 是一个挂载点的容量缓存。
type cachedCapacity struct {
	cap *Capacity
	at  time.Time
}

// cachedHealth 是一台设备的 SMART 结果与采集时间。
type cachedHealth struct {
	health Health
	at     time.Time
}

// Collector 周期扫描块设备拓扑并缓存最近一份快照。
//
// 失败策略：扫描失败**不清空**节点，只把 status 改成 error、记下原因，
// nodes/edges/collected_at 停在上一次成功的样子。理由是“读不到”并不等于
// “服务器上没有磁盘”，把列表清空会让界面显示成机器被拔了盘。
type Collector struct {
	interval       time.Duration
	capacityTTL    time.Duration
	healthInterval time.Duration
	capabilityTTL  time.Duration
	prober         HealthProber
	root           Root
	env            Env
	logger         *slog.Logger

	mu       sync.Mutex
	cached   Topology
	hasCache bool

	// 容量缓存在网络挂载并发查询时会被多个 goroutine 读写，单独一把锁。
	capsMu sync.Mutex
	caps   map[string]cachedCapacity
	// deviceCaps 是设备能力的缓存（与容量共用一把锁：两者都只在 Refresh 里写，
	// 而且都是“观测值的缓存”，没有理由让它们互相等）。
	deviceCaps map[string]cachedCapability

	// 计数器与健康各自一把锁：它们由不同的 goroutine 写（扫描循环与探测循环），
	// 不能让它们互相等。健康探测可能持续几秒，扫描不能被它挡住。
	ioMu         sync.Mutex
	lastCounters map[string]DiskCounters
	lastCountAt  time.Time
	ioWarned     bool

	healthMu sync.Mutex
	health   map[string]cachedHealth

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
	healthInterval := opts.HealthInterval
	if healthInterval <= 0 {
		healthInterval = DefaultHealthInterval
	}
	if healthInterval < MinHealthInterval {
		healthInterval = MinHealthInterval
	}
	if healthInterval > MaxHealthInterval {
		healthInterval = MaxHealthInterval
	}
	root := opts.Root
	if root.Sys == "" || root.Proc == "" {
		root = SystemRoot()
	}
	if opts.NetworkCapacityTimeout > 0 {
		if caps, ok := root.Caps.(*OSCapacityReader); ok {
			caps.Timeout = opts.NetworkCapacityTimeout
		}
	}
	return &Collector{
		interval:       interval,
		capacityTTL:    ttl,
		healthInterval: healthInterval,
		capabilityTTL:  DefaultCapabilityTTL,
		prober:         opts.Prober,
		root:           root,
		env:            opts.Env,
		logger:         opts.Logger,
		caps:           map[string]cachedCapacity{},
		deviceCaps:     map[string]cachedCapability{},
		lastCounters:   map[string]DiskCounters{},
		health:         map[string]cachedHealth{},
	}
}

// Interval 返回扫描间隔。
func (c *Collector) Interval() time.Duration { return c.interval }

// HealthInterval 返回 SMART 的采集间隔（已按 5—60 分钟钳制）。
func (c *Collector) HealthInterval() time.Duration { return c.healthInterval }

// Run 立刻扫描一次，然后按间隔轮询，直到 ctx 结束。
//
// SMART 探测跑在**另一个** goroutine 里：一块慢盘最多要等十几秒的超时，
// 放在扫描循环里会让 5 秒的拓扑刷新被一块盘拖死。
func (c *Collector) Run(ctx context.Context) {
	c.Refresh(ctx)
	go c.healthLoop(ctx)

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
	c.attachIO(topo, now)
	c.applyHealth(topo)
	c.fillCapacities(ctx, topo, now)
	// 能力在容量之后填：判决会用到挂载点（挂着的盘不能格式化），
	// 而挂载信息来自扫描本身，与容量无关。顺序只是为了让人读起来顺。
	c.fillCapabilities(ctx, topo, now)

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

// Assess 用**当前快照**与生产探针对一次存储写动作做预检判决（P07.1）。
//
// 为什么用缓存快照而不是现场重扫：预检必须绑定用户看到的那份拓扑。现场重扫会得到
// 一个新的 snapshot_version，于是每一份预检在提交时都对不上——预检就变成了摆设。
// 状态真变了的时候，提交期的复核会拿新快照重算，得到 PREFLIGHT_STALE，那正是
// 这条机制该有的表现。
func (c *Collector) Assess(ctx context.Context, action string, params any) (Assessment, error) {
	return Assess(ctx, c.Topology(ctx), action, params, c.env)
}

// attachIO 读取 /proc/diskstats 并给每台设备补上计数与速率。
//
// 速率来自**相邻两次扫描**的差。三个“算不出来就说算不出来”的情况在 ComputeRate 里处理：
// 首次采样、计数器重置、采样间隔异常。这里只负责把上一次的计数留住。
//
// 读 /proc/diskstats 失败不影响拓扑：设备列表照常可用，只是 io 为 null。
// 前端要能区分“没有 I/O”和“没有计数器”，所以失败时不是填零。
func (c *Collector) attachIO(topo *Topology, now time.Time) {
	path := joinPath(c.root.Proc, "diskstats")
	text, ok := readFile(path)
	if !ok {
		c.warnIOOnce("读不到 " + path + "，本次不提供 I/O 速率")
		return
	}
	counters, warnings := ParseDiskstats(text)
	if len(warnings) > 0 {
		c.warnIOOnce(strings.Join(warnings, "；"))
	}

	c.ioMu.Lock()
	defer c.ioMu.Unlock()

	prevAt := c.lastCountAt
	prev := c.lastCounters
	for i := range topo.Nodes {
		dev := &topo.Nodes[i]
		cur, exists := counters[dev.Name]
		if !exists {
			// 分区与逻辑设备在 /proc/diskstats 里也有自己的一行；没有就只能报 null。
			continue
		}
		dev.Io = NewIOStats(cur, now)
		dev.counters = cur
		dev.hasCounters = true
		if p, ok := prev[dev.Name]; ok {
			dev.Io.Rate = ComputeRate(p, prevAt, cur, now, c.interval)
		}
	}
	c.lastCounters = counters
	c.lastCountAt = now
}

// warnIOOnce 只在第一次（或原因变化时）记一条日志。
// 这个调用每 5 秒一次，不能每次都写一行日志。
func (c *Collector) warnIOOnce(msg string) {
	c.ioMu.Lock()
	repeated := c.ioWarned
	c.ioWarned = true
	c.ioMu.Unlock()
	if repeated || c.logger == nil {
		return
	}
	c.logger.Warn("磁盘计数器不可用，I/O 速率将为空", "error", msg)
}

// applyHealth 把最近一次 SMART 结果合进快照。
//
// 只覆盖**已经采集过**的设备：还没轮到的物理盘保持 unknown（带“尚未完成”的说明），
// 不会被这里改成 healthy 或 0。
func (c *Collector) applyHealth(topo *Topology) {
	c.healthMu.Lock()
	defer c.healthMu.Unlock()

	for i := range topo.Nodes {
		dev := &topo.Nodes[i]
		if h, ok := c.health[dev.Name]; ok {
			dev.Health = h.health
			continue
		}
		if dev.Kind == KindDisk && c.prober == nil {
			dev.Health = Health{
				Status:  HealthUnknown,
				Source:  "none",
				Details: strPtr("SMART 采集未启用（没有可用的 smartctl）"),
			}
		}
	}
}

// healthLoop 按 SMART 的节奏（默认 15 分钟）逐块读取物理盘的健康。
//
// 一块一块来是刻意的：同时唤醒八块盘既慢又会掩盖“某一块盘特别慢”，
// 而且并发查询会让机械盘的寻道互相干扰。
func (c *Collector) healthLoop(ctx context.Context) {
	if c.prober == nil {
		return
	}
	// 检查频率：间隔的四分之一，钳在 15 秒—2 分钟之间。
	// 检查本身只读内存里的快照，很便宜；真正的代价在到期之后的那次 smartctl。
	tick := c.healthInterval / 4
	if tick < 15*time.Second {
		tick = 15 * time.Second
	}
	if tick > 2*time.Minute {
		tick = 2 * time.Minute
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			c.probeDue(ctx)
			timer.Reset(tick)
		}
	}
}

// probeDue 对到期的物理盘读一次 SMART。
func (c *Collector) probeDue(ctx context.Context) {
	snap, ok := c.Snapshot()
	if !ok {
		return
	}
	now := c.root.now()

	seen := map[string]bool{}
	for _, dev := range snap.Nodes {
		if dev.Kind != KindDisk {
			continue
		}
		seen[dev.Name] = true
		if c.healthFresh(dev.Name, now) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return
		}
		if !dev.Online {
			c.storeHealth(dev.Name, Health{
				Status:    HealthUnknown,
				Source:    "none",
				CheckedAt: &now,
				Details:   strPtr("设备当前离线或没有介质，未读取 SMART"),
			}, now)
			continue
		}
		transport := ""
		if dev.Transport != nil {
			transport = *dev.Transport
		}
		h := c.prober.Probe(ctx, dev.Name, joinPath(c.root.Sys, "block", dev.Name), transport)
		c.storeHealth(dev.Name, h, now)
	}
	c.pruneHealth(seen)
}

// healthFresh 判断某台设备的结果还在有效期内。
func (c *Collector) healthFresh(name string, now time.Time) bool {
	c.healthMu.Lock()
	defer c.healthMu.Unlock()
	entry, ok := c.health[name]
	if !ok {
		return false
	}
	age := now.Sub(entry.at)
	return age >= 0 && age < c.healthInterval
}

func (c *Collector) storeHealth(name string, h Health, at time.Time) {
	c.healthMu.Lock()
	defer c.healthMu.Unlock()
	c.health[name] = cachedHealth{health: h, at: at}
}

// pruneHealth 丢掉已经不在快照里的设备，避免长期运行后健康缓存无限增长。
func (c *Collector) pruneHealth(seen map[string]bool) {
	c.healthMu.Lock()
	defer c.healthMu.Unlock()
	for name := range c.health {
		if !seen[name] {
			delete(c.health, name)
		}
	}
}

// fillCapacities 按 30 秒缓存补齐挂载点容量。
//
// 容量只在实际挂载成功、statfs 也成功时才存在；读不到就是 null，
// 不是 0 —— “这个盘满了”和“我不知道这个盘多满”是两件事。
func (c *Collector) fillCapacities(ctx context.Context, topo *Topology, now time.Time) {
	if c.root.Caps == nil {
		return
	}

	// 第一步：把要查的挂载点摊平成一张表。设备上的挂载点与独立列出的网络挂载
	// 都要查容量，但同一路径只查一次——绑定挂载会让多个设备指向同一个挂载点。
	//
	// apply 是“把结果写回字段”的闭包：设备挂载点与网络挂载的结构不同，
	// 查询与缓存逻辑却完全一样，没有理由写两遍。
	type capacityTarget struct {
		path   string
		fsType string
		apply  func(*Capacity, string)
		// dupes 是同路径的其它落点，查完之后把同一份结果抄给它们。
		dupes []func(*Capacity, string)
	}
	index := map[string]int{}
	var targets []*capacityTarget
	addTarget := func(path, fsType string, apply func(*Capacity, string)) {
		if i, ok := index[path]; ok {
			targets[i].dupes = append(targets[i].dupes, apply)
			return
		}
		index[path] = len(targets)
		targets = append(targets, &capacityTarget{path: path, fsType: fsType, apply: apply})
	}
	for i := range topo.Nodes {
		fs := topo.Nodes[i].Filesystem
		if fs == nil {
			continue
		}
		for j := range fs.Mounts {
			m := &fs.Mounts[j]
			addTarget(m.Path, m.FSType, func(capv *Capacity, reason string) {
				m.Capacity = capv
				m.CapacityError = reason
			})
		}
	}
	for i := range topo.NetworkMounts {
		nm := &topo.NetworkMounts[i]
		addTarget(nm.Path, nm.FSType, func(capv *Capacity, reason string) {
			nm.Capacity = capv
			nm.CapacityError = reason
			// 可达性只写确定的结论：读到容量就是可达，明确失败就是不可达，
			// 还没查过则保持 null。把“不知道”写成 false 会让刚挂上的盘显示成断开。
			switch {
			case capv != nil:
				v := true
				nm.Reachable = &v
			case reason != "":
				v := false
				nm.Reachable = &v
			}
		})
	}

	// 第二步：分成“本地直接查”与“网络隔离查”两组；缓存还在有效期内的直接用。
	var local, network []*capacityTarget
	settle := func(t *capacityTarget, capv *Capacity, reason string) {
		t.apply(capv, reason)
		for _, d := range t.dupes {
			d(capv, reason)
		}
	}
	for _, t := range targets {
		if cc, ok := c.cachedCapacityFor(t.path, now); ok {
			settle(t, cc, "")
			continue
		}
		if IsNetworkFS(t.fsType) {
			network = append(network, t)
			continue
		}
		local = append(local, t)
	}

	// 第三步：本地挂载逐个查。statfs 是微秒级的，串行反而更省事。
	for _, t := range local {
		capv, reason := c.fillCapacity(ctx, t.path, t.fsType, now)
		settle(t, capv, reason)
	}

	// 第四步：网络挂载并发查，但**限制并发数**。PLAN.md 第 8 节要求限制并发，
	// 否则一台机器上挂十几个不可达的 NFS，采集器会被同时派出的十几个子进程拖住。
	if len(network) > 0 {
		sem := make(chan struct{}, maxNetworkCapacityQueries)
		var wg sync.WaitGroup
		for _, t := range network {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				wg.Wait()
				return
			}
			wg.Add(1)
			go func(t *capacityTarget) {
				defer wg.Done()
				defer func() { <-sem }()
				capv, reason := c.fillCapacity(ctx, t.path, t.fsType, now)
				settle(t, capv, reason)
			}(t)
		}
		wg.Wait()
	}
}

// maxNetworkCapacityQueries 是同时进行的网络容量查询上限。
// 2 是刻意的低值：网络挂载查询的目的不是快，而是**不要拖住别人**。
const maxNetworkCapacityQueries = 2

// cachedCapacityFor 返回仍在有效期内的容量缓存。
func (c *Collector) cachedCapacityFor(path string, now time.Time) (*Capacity, bool) {
	c.capsMu.Lock()
	defer c.capsMu.Unlock()
	cc, ok := c.caps[path]
	if !ok || now.Sub(cc.at) >= c.capacityTTL || now.Before(cc.at) {
		return nil, false
	}
	return cc.cap, true
}

// fillCapacity 查询一个挂载路径的容量并写回缓存。
//
// 成功时返回容量（错误说明为空），失败时返回 nil 与给人看的原因。
// 调用方决定结果落到哪个字段上——设备挂载点与网络挂载都走这一条路径。
func (c *Collector) fillCapacity(ctx context.Context, path, fsType string, now time.Time) (*Capacity, string) {
	capv, err := c.root.Caps.Capacity(ctx, path, fsType)
	if err != nil {
		// 缓存作废：挂载点可能已经卸载或文件系统出了问题，
		// 继续用旧数字会让人以为容量是新的。
		c.capsMu.Lock()
		delete(c.caps, path)
		c.capsMu.Unlock()
		return nil, capacityReason(err)
	}
	// 读取器没打时间戳时用本次扫描时间补上：容量必须带时间，
	// 否则前端无法判断这个数字有多旧。
	if capv.CollectedAt.IsZero() {
		capv.CollectedAt = now
	}
	c.capsMu.Lock()
	c.caps[path] = cachedCapacity{cap: capv, at: now}
	c.capsMu.Unlock()
	return capv, ""
}

// capacityReason 把容量读取失败翻译成一句人能看懂的话。
// 超时与“文件系统还在但问不出数字”要分开：前者的意思是“远端不可达”。
func capacityReason(err error) string {
	switch {
	case errors.Is(err, ErrCapacityTimeout):
		return "查询超时：" + err.Error()
	case errors.Is(err, ErrCapacityUnavailable):
		return "无法读取容量：" + err.Error()
	default:
		return err.Error()
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
	out.NetworkMounts = make([]NetworkMount, len(t.NetworkMounts))
	for i, nm := range t.NetworkMounts {
		cp := nm
		cp.Reachable = cloneBool(nm.Reachable)
		if nm.Capacity != nil {
			c := *nm.Capacity
			cp.Capacity = &c
		}
		out.NetworkMounts[i] = cp
	}
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
	if d.Io != nil {
		io := *d.Io
		if d.Io.Rate != nil {
			rate := *d.Io.Rate
			rate.ReadLatencyMS = cloneFloat(d.Io.Rate.ReadLatencyMS)
			rate.WriteLatencyMS = cloneFloat(d.Io.Rate.WriteLatencyMS)
			io.Rate = &rate
		}
		out.Io = &io
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

func cloneBool(b *bool) *bool {
	if b == nil {
		return nil
	}
	v := *b
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
