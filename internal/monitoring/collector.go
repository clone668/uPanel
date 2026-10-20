package monitoring

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultInterval 是基础资源（CPU/内存/负载/网络）的采集间隔。
	// PLAN.md 第 8 节：基础资源与磁盘计数器每 5 秒。
	DefaultInterval = 5 * time.Second

	// staleFactor 决定“多久没采集算陈旧”：3 个间隔。
	// 差一两个间隔是正常抖动，不该让整个首页闪成陈旧；连着三个间隔没有新样本，
	// 说明采集循环真的卡住了。
	staleFactor = 3
)

// FileReader 按绝对路径读一个文件。测试用它替换 /proc 与 /sys。
type FileReader interface {
	ReadFile(name string) ([]byte, error)
}

type osReader struct{}

func (osReader) ReadFile(name string) ([]byte, error) { return os.ReadFile(name) }

// Paths 是采集读取的全部固定路径。
//
// 全部集中在这里，是为了让“采集读了哪些文件”是一个可审计的常量列表：
// 没有拼接的用户输入，也没有随环境变化的路径。
type Paths struct {
	Stat          string
	Meminfo       string
	Loadavg       string
	NetDev        string
	OSRelease     string
	Hostname      string
	KernelRelease string
	BootID        string
	Uptime        string
	CPUInfo       string
	// SysNetClass 是网卡目录；每块网卡的 operstate 在 <SysNetClass>/<name>/operstate。
	SysNetClass string
}

// DefaultPaths 返回 Linux 上的标准路径。
func DefaultPaths() Paths {
	return Paths{
		Stat:          "/proc/stat",
		Meminfo:       "/proc/meminfo",
		Loadavg:       "/proc/loadavg",
		NetDev:        "/proc/net/dev",
		OSRelease:     "/etc/os-release",
		Hostname:      "/proc/sys/kernel/hostname",
		KernelRelease: "/proc/sys/kernel/osrelease",
		BootID:        "/proc/sys/kernel/random/boot_id",
		Uptime:        "/proc/uptime",
		CPUInfo:       "/proc/cpuinfo",
		SysNetClass:   "/sys/class/net",
	}
}

// Options 是采集器参数。零值可用：默认 5 秒、真实时钟、真实文件系统。
type Options struct {
	Interval time.Duration
	Now      func() time.Time
	Reader   FileReader
	Paths    Paths
	Logger   *slog.Logger
}

// cpuSample 是用于算差值的一次 CPU 采样。
type cpuSample struct {
	at     time.Time
	bootID string
	agg    cpuTimes
	perCPU []cpuTimes
}

// Collector 周期采集基础资源并缓存最近一次快照。
//
// 并发模型：刷新与读取共用一把锁。采集本身是读几个 /proc 小文件，耗时可忽略，
// 用一把锁换来“任何时刻只有一个刷新在写状态”，比按段加锁简单且不会读到半份快照。
type Collector struct {
	interval time.Duration
	now      func() time.Time
	reader   FileReader
	paths    Paths
	logger   *slog.Logger

	mu       sync.Mutex
	cached   Summary
	hasCache bool
	bootID   string

	prevCPU   *cpuSample
	prevNet   map[string]netCounters
	prevNetAt time.Time

	// lastDegraded 只在日志里用：状态翻转时记一条，不让每 5 秒的采集刷满日志。
	lastDegraded bool
	loggedOnce   bool
}

// NewCollector 构造采集器。参数缺省按默认值补齐，不返回错误：
// 采集失败是运行期状态（段报 error），不是启动条件。
func NewCollector(opts Options) *Collector {
	interval := opts.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	reader := opts.Reader
	if reader == nil {
		reader = osReader{}
	}
	paths := opts.Paths
	if paths.Stat == "" {
		paths = DefaultPaths()
	}
	return &Collector{
		interval: interval,
		now:      now,
		reader:   reader,
		paths:    paths,
		logger:   opts.Logger,
	}
}

// Interval 返回采集间隔。
func (c *Collector) Interval() time.Duration { return c.interval }

// Run 立刻采集一次，然后按间隔轮询，直到 ctx 结束。
func (c *Collector) Run(ctx context.Context) {
	c.Refresh()
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.Refresh()
		}
	}
}

// Refresh 同步采集一次，更新缓存并返回这份快照。
func (c *Collector) Refresh() Summary {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now().UTC()
	prev := c.cached

	next := Summary{
		CollectedAt: now,
		IntervalMS:  c.interval.Milliseconds(),
	}
	// 顺序有意义：Host 先采集，CPU 的“是否重启过”判断要用到刚读出的 boot_id。
	next.Host = c.collectHost(prev.Host, now)
	next.CPU = c.collectCPU(prev.CPU, now)
	next.Memory = c.collectMemory(prev.Memory, now)
	next.Load = c.collectLoad(prev.Load, now)
	next.Network = c.collectNetwork(prev.Network, now)
	next.Degraded = anyDegraded(next)
	next.Stale = false

	c.cached = cloneSummary(next)
	c.hasCache = true
	c.logTransition(next)
	return next
}

// Snapshot 返回最近一次快照（含陈旧标记）。没有快照时第二个返回值为 false。
func (c *Collector) Snapshot() (Summary, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.hasCache {
		return Summary{}, false
	}
	out := cloneSummary(c.cached)
	out.Stale = c.now().UTC().Sub(out.CollectedAt) > staleFactor*c.interval
	return out, true
}

// Summary 返回一份可直接返回给调用方的快照。
//
// 还没有任何快照时（代理刚启动、轮询循环还没跑第一轮）同步采集一次，
// 而不是返回一份空结构：空结构在界面上与“全是 0”没有区别。
func (c *Collector) Summary() Summary {
	if snap, ok := c.Snapshot(); ok {
		return snap
	}
	return c.Refresh()
}

// logTransition 只在采集整体健康状态翻转时记日志。
func (c *Collector) logTransition(sum Summary) {
	if c.logger == nil {
		return
	}
	if !c.loggedOnce || sum.Degraded != c.lastDegraded {
		c.loggedOnce = true
		c.lastDegraded = sum.Degraded
		if sum.Degraded {
			c.logger.Warn("系统采集部分失败",
				"host", sum.Host.Status, "cpu", sum.CPU.Status, "memory", sum.Memory.Status,
				"load", sum.Load.Status, "network", sum.Network.Status)
		} else {
			c.logger.Info("系统采集恢复", "collected_at", sum.CollectedAt)
		}
	}
}

// readFile 读一个来源文件。错误保留原始信息（含路径与 errno），便于定位。
func (c *Collector) readFile(path string) ([]byte, error) {
	raw, err := c.reader.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// sectionFailure 把读取错误翻译成段的 status 与说明。
func sectionFailure(err error) (string, string) {
	if errors.Is(err, fs.ErrNotExist) {
		return StatusUnavailable, err.Error()
	}
	return StatusError, err.Error()
}

// markOK 把段标成成功并把值的采集时间设为 now。
func markOK(sec *Section, now time.Time) {
	t := now
	sec.Status = StatusOK
	sec.Error = ""
	sec.CollectedAt = &t
}

func (c *Collector) collectHost(prev HostSection, now time.Time) HostSection {
	sec := prev
	sec.Source = "/etc/os-release, /proc/sys/kernel/*, /proc/uptime"

	var readErr error
	read := func(path, what string) []byte {
		raw, err := c.readFile(path)
		if err != nil && readErr == nil {
			readErr = fmt.Errorf("读取%s失败: %w", what, err)
		}
		return raw
	}
	hostnameRaw := read(c.paths.Hostname, "主机名")
	kernelRaw := read(c.paths.KernelRelease, "内核版本")
	osRaw := read(c.paths.OSRelease, "系统发行版信息")
	bootRaw := read(c.paths.BootID, "boot_id")
	uptimeRaw := read(c.paths.Uptime, "开机时长")
	if readErr != nil {
		sec.Status, sec.Error = sectionFailure(readErr)
		return sec
	}

	hostname := strings.TrimSpace(string(hostnameRaw))
	kernel := strings.TrimSpace(string(kernelRaw))
	bootID := strings.TrimSpace(string(bootRaw))
	if hostname == "" {
		sec.Status, sec.Error = StatusError, "主机名为空"
		return sec
	}
	if kernel == "" {
		sec.Status, sec.Error = StatusError, "内核版本为空"
		return sec
	}
	if bootID == "" {
		sec.Status, sec.Error = StatusError, "boot_id 为空（无法判断计数器是否因重启失效）"
		return sec
	}
	rel, err := ParseOSRelease(osRaw)
	if err != nil {
		sec.Status, sec.Error = StatusError, err.Error()
		return sec
	}
	uptime, err := ParseUptime(uptimeRaw)
	if err != nil {
		sec.Status, sec.Error = StatusError, err.Error()
		return sec
	}

	markOK(&sec.Section, now)
	sec.Hostname = hostname
	sec.KernelRelease = kernel
	sec.BootID = bootID
	sec.OSID = rel.id
	sec.OSName = rel.name
	sec.OSVersion = rel.version
	sec.OSPrettyName = rel.prettyName
	sec.Arch = machineArch()
	up := uptime
	sec.UptimeSeconds = &up
	c.bootID = bootID
	return sec
}

func (c *Collector) collectCPU(prev CPUSection, now time.Time) CPUSection {
	sec := prev
	sec.Source = "/proc/stat"

	raw, err := c.readFile(c.paths.Stat)
	if err != nil {
		sec.Status, sec.Error = sectionFailure(err)
		return sec
	}
	agg, per, err := ParseStat(raw)
	if err != nil {
		sec.Status, sec.Error = StatusError, err.Error()
		return sec
	}

	markOK(&sec.Section, now)
	sec.LogicalCores = len(per)
	if modelRaw, err := c.readFile(c.paths.CPUInfo); err == nil {
		if model := ParseCPUModel(modelRaw); model != "" {
			sec.Model = model
		}
	}

	// 先按“没有前一次样本”填成 null，再由差值覆盖：这样任何提前返回都不会留下 0。
	sec.UsagePercent = nil
	sec.SampleWindowMS = nil
	sec.PerCPU = make([]CPUCore, len(per))
	for i := range per {
		sec.PerCPU[i] = CPUCore{Index: i}
	}

	if prevSample := c.prevCPU; prevSample != nil && prevSample.bootID == c.bootID {
		window := now.Sub(prevSample.at)
		if window > 0 && !agg.decreased(prevSample.agg) {
			if usage := usagePercent(prevSample.agg, agg); usage != nil {
				ms := window.Milliseconds()
				sec.UsagePercent = usage
				sec.SampleWindowMS = &ms
			}
		}
		for i := range per {
			if i < len(prevSample.perCPU) {
				sec.PerCPU[i].UsagePercent = usagePercent(prevSample.perCPU[i], per[i])
			}
		}
	}

	c.prevCPU = &cpuSample{
		at:     now,
		bootID: c.bootID,
		agg:    agg,
		perCPU: append([]cpuTimes(nil), per...),
	}
	return sec
}

func (c *Collector) collectMemory(prev MemorySection, now time.Time) MemorySection {
	sec := prev
	sec.Source = "/proc/meminfo"

	raw, err := c.readFile(c.paths.Meminfo)
	if err != nil {
		sec.Status, sec.Error = sectionFailure(err)
		return sec
	}
	m, err := ParseMeminfo(raw)
	if err != nil {
		sec.Status, sec.Error = StatusError, err.Error()
		return sec
	}

	markOK(&sec.Section, now)
	total, free, available := m.total, m.free, m.available
	buffers, cached := m.buffers, m.cached
	swapTotal, swapFree := m.swapTotal, m.swapFree
	used := uint64(0)
	if available <= total {
		used = total - available
	}
	sec.TotalBytes = &total
	sec.FreeBytes = &free
	sec.AvailableBytes = &available
	sec.BuffersBytes = &buffers
	sec.CachedBytes = &cached
	sec.UsedBytes = &used
	sec.SwapTotalBytes = &swapTotal
	sec.SwapFreeBytes = &swapFree
	return sec
}

func (c *Collector) collectLoad(prev LoadSection, now time.Time) LoadSection {
	sec := prev
	sec.Source = "/proc/loadavg"

	raw, err := c.readFile(c.paths.Loadavg)
	if err != nil {
		sec.Status, sec.Error = sectionFailure(err)
		return sec
	}
	l, err := ParseLoadavg(raw)
	if err != nil {
		sec.Status, sec.Error = StatusError, err.Error()
		return sec
	}

	markOK(&sec.Section, now)
	l1, l5, l15 := l.load1, l.load5, l.load15
	runnable, threads := l.runnable, l.threads
	sec.Load1 = &l1
	sec.Load5 = &l5
	sec.Load15 = &l15
	sec.Runnable = &runnable
	sec.Threads = &threads
	return sec
}

func (c *Collector) collectNetwork(prev NetworkSection, now time.Time) NetworkSection {
	sec := prev
	sec.Source = "/proc/net/dev, /sys/class/net/*/operstate"

	raw, err := c.readFile(c.paths.NetDev)
	if err != nil {
		sec.Status, sec.Error = sectionFailure(err)
		return sec
	}
	counters, err := ParseNetDev(raw)
	if err != nil {
		sec.Status, sec.Error = StatusError, err.Error()
		return sec
	}

	window := time.Duration(0)
	useRates := false
	if !c.prevNetAt.IsZero() {
		window = now.Sub(c.prevNetAt)
		useRates = window > 0
	}

	names := netNames(counters)
	ifaces := make([]NetInterface, 0, len(names))
	for _, name := range names {
		cur := counters[name]
		iface := NetInterface{
			Name:      name,
			Loopback:  name == "lo",
			RXBytes:   cur.rxBytes,
			TXBytes:   cur.txBytes,
			RXPackets: cur.rxPackets,
			TXPackets: cur.txPackets,
			RXErrors:  cur.rxErrors,
			TXErrors:  cur.txErrors,
			RXDropped: cur.rxDropped,
			TXDropped: cur.txDropped,
		}
		if stateRaw, err := c.readFile(c.paths.SysNetClass + "/" + name + "/operstate"); err == nil {
			iface.OperState = strings.TrimSpace(string(stateRaw))
		}
		if useRates {
			if prevCounters, ok := c.prevNet[name]; ok && !netDecreased(prevCounters, cur) {
				iface.RXBytesPerSecond = ratePerSecond(cur.rxBytes-prevCounters.rxBytes, window)
				iface.TXBytesPerSecond = ratePerSecond(cur.txBytes-prevCounters.txBytes, window)
			}
		}
		ifaces = append(ifaces, iface)
	}

	c.prevNet = counters
	c.prevNetAt = now
	markOK(&sec.Section, now)
	sec.Interfaces = ifaces
	return sec
}

// netDecreased 判断计数器是否变小：变小只可能是重置（或网卡被替换后同名），
// 此时相邻两次采样的差没有意义，速率为 null。
func netDecreased(prev, cur netCounters) bool {
	return cur.rxBytes < prev.rxBytes || cur.txBytes < prev.txBytes ||
		cur.rxPackets < prev.rxPackets || cur.txPackets < prev.txPackets
}

func ratePerSecond(delta uint64, window time.Duration) *float64 {
	v := float64(delta) / window.Seconds()
	return &v
}
