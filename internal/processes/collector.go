package processes

import (
	"context"
	"log/slog"
	"os"
	"sort"
	"sync"
	"time"
)

// Options 是采集器参数。零值可用：5 秒采集、真实 /proc、真实时钟、真实信号。
type Options struct {
	// Interval 是采集间隔；0 用 DefaultInterval。
	Interval time.Duration
	// FS 是文件系统读取端；nil 用真实的操作系统调用。
	FS FileSystem
	// Paths 是固定路径；零值用 DefaultPaths()。
	Paths Paths
	// Logger 的默认值是 slog.Default()。
	Logger *slog.Logger
	// Now 是可替换时钟（测试用）；nil 用 time.Now。
	Now func() time.Time
	// Killer 是发送信号的实现；nil 用 syscall.Kill（见 kill_unix.go）。
	//
	// 单独抽出来是为了让“PID 重用后绝不发信号”这类路径可以在测试里被证明：
	// 假实现会记录自己是否被调用，而“没被调用”正是那条验收要求。
	Killer Killer
	// SelfPID 是代理自身进程号；0 用 os.Getpid()。它用于“不能杀自己”的判定。
	SelfPID int64
	// VerifyTerm/VerifyKill 是发信号后等待进程消失的上限；0 用默认值 10s / 5s。
	VerifyTerm time.Duration
	VerifyKill time.Duration
	// VerifyInterval 是等待期间的轮询间隔；0 用 100ms。
	VerifyInterval time.Duration
}

// 发信号后等待进程消失的默认参数。
const (
	// verifyTermTimeout：SIGTERM 允许进程清理后退出，给 10 秒。
	verifyTermTimeout = 10 * time.Second
	// verifyKillTimeout：SIGKILL 由内核立即处理；5 秒足够看到它消失（或变成僵尸）。
	verifyKillTimeout = 5 * time.Second
	// verifyInterval 是等待期间的轮询间隔。
	verifyInterval = 100 * time.Millisecond
)

// Collector 周期采集进程列表并缓存最近一份快照。
//
// 失败策略与 storage/services 一致：采集失败**不清空**列表，只把 Status 改成 error、
// 记下原因，CollectedAt 停在上一次成功的时间。“读不到”不等于“机器上没有进程”。
type Collector struct {
	interval time.Duration
	fs       FileSystem
	paths    Paths
	logger   *slog.Logger
	now      func() time.Time
	killer   Killer
	selfPID  int64

	verifyTerm     time.Duration
	verifyKill     time.Duration
	verifyInterval time.Duration

	mu     sync.Mutex
	cached Snapshot
	has    bool
	// lastError 只在状态翻转时写日志，避免每 5 秒刷一条同样的错误。
	lastError string

	// prev 是上一次采集的 CPU 基线：pid → (starttime, 累计滴答数)。
	// 换了 starttime 就是另一个进程，旧基线不得套用。
	prev map[int64]cpuSample
	// prevTotal 是上一次 /proc/stat 的总滴答数与 CPU 数。
	prevTotal uint64
	prevCPUs  int
}

// cpuSample 是一个进程的 CPU 基线。
type cpuSample struct {
	startTick uint64
	ticks     uint64
}

// NewCollector 构造采集器。参数缺省按默认值补齐；采集失败是运行期状态，不是启动条件。
func NewCollector(opts Options) *Collector {
	interval := opts.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	fs := opts.FS
	if fs == nil {
		fs = osFS{}
	}
	paths := opts.Paths
	if !paths.valid() {
		paths = DefaultPaths()
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	killer := opts.Killer
	if killer == nil {
		killer = osKiller{}
	}
	selfPID := opts.SelfPID
	if selfPID <= 0 {
		selfPID = int64(os.Getpid())
	}
	verifyTerm := opts.VerifyTerm
	if verifyTerm <= 0 {
		verifyTerm = verifyTermTimeout
	}
	verifyKill := opts.VerifyKill
	if verifyKill <= 0 {
		verifyKill = verifyKillTimeout
	}
	verify := opts.VerifyInterval
	if verify <= 0 {
		verify = verifyInterval
	}
	return &Collector{
		interval:       interval,
		fs:             fs,
		paths:          paths,
		logger:         logger,
		now:            now,
		killer:         killer,
		selfPID:        selfPID,
		verifyTerm:     verifyTerm,
		verifyKill:     verifyKill,
		verifyInterval: verify,
	}
}

// Interval 返回采集间隔。
func (c *Collector) Interval() time.Duration { return c.interval }

// Run 立即采集一次，之后按间隔循环，直到 ctx 结束。
func (c *Collector) Run(ctx context.Context) {
	c.refresh(ctx)
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.refresh(ctx)
		}
	}
}

// refresh 采集一次并记日志（状态翻转才记 Warn，恢复时记 Info）。
func (c *Collector) refresh(ctx context.Context) {
	snap := c.Collect(ctx)
	if snap.Status == StatusError {
		if snap.Error != c.lastError {
			c.lastError = snap.Error
			c.logger.Warn("进程采集失败，保留上一次成功快照", "error", snap.Error)
		}
		return
	}
	if c.lastError != "" {
		c.lastError = ""
		c.logger.Info("进程采集已恢复", "processes", len(snap.Processes))
	}
}

// Snapshot 返回最近一次采集结果。还没采集过时返回一份明确标为失败的快照，
// 而不是一份“看起来正常”的空列表。
func (c *Collector) Snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.has {
		return c.cached
	}
	return Snapshot{
		Status:     StatusError,
		Error:      "进程状态尚未完成第一次采集",
		IntervalMS: c.interval.Milliseconds(),
		Processes:  []Process{},
	}
}

// Collect 现场采集一次（不读缓存），并更新缓存。
func (c *Collector) Collect(ctx context.Context) Snapshot {
	now := c.now().UTC().Truncate(time.Second)
	names, err := c.fs.ListDir(c.paths.ProcRoot)
	if err != nil {
		return c.fail(now, "读取 "+c.paths.ProcRoot+" 失败: "+err.Error())
	}

	rows := make(map[int64]procRow, len(names))
	pids := make([]int64, 0, len(names))
	for _, name := range names {
		pid, ok := ParsePID(name)
		if !ok {
			continue
		}
		row, ok := readRow(c.fs, c.paths, pid)
		if !ok {
			// 采集期间退出的进程：跳过，不是错误。
			continue
		}
		rows[pid] = row
		pids = append(pids, pid)
	}
	sort.Slice(pids, func(i, j int) bool { return pids[i] < pids[j] })

	users := c.loadUsers()
	source := mapSource(rows)
	total := len(pids)
	truncated := false
	if len(pids) > MaxProcesses {
		pids = pids[:MaxProcesses]
		truncated = true
	}

	percent := c.cpuPercent(rows, pids)
	list := make([]Process, 0, len(pids))
	for _, pid := range pids {
		list = append(list, c.toProcess(rows[pid], percent[pid], users, source))
	}

	snap := Snapshot{
		Status:      StatusOK,
		CollectedAt: now,
		IntervalMS:  c.interval.Milliseconds(),
		Total:       total,
		Truncated:   truncated,
		Processes:   list,
	}
	c.store(snap)
	return snap
}

// cpuPercent 用两次采集之间的差值算每个进程的 CPU 占用。
//
// 为什么不用 HZ：进程的 utime/stime 是时钟滴答，/proc/stat 的 cpu 行也是滴答，
// 二者和 HZ（100 / 250 / 1000）无关地自洽，所以只要两个差值同单位就能算出百分比，
// 不必猜本机 HZ。第一次采集没有基线，返回空表——那一轮 cpu_percent 是 null。
func (c *Collector) cpuPercent(rows map[int64]procRow, pids []int64) map[int64]*float64 {
	total, cpus := c.readTotal()
	out := make(map[int64]*float64, len(pids))

	c.mu.Lock()
	prev := c.prev
	prevTotal := c.prevTotal
	prevCPUs := c.prevCPUs
	c.mu.Unlock()

	next := make(map[int64]cpuSample, len(pids))
	for _, pid := range pids {
		next[pid] = cpuSample{startTick: rows[pid].startTick, ticks: rows[pid].cpuTicks()}
	}
	c.mu.Lock()
	c.prev = next
	c.prevTotal = total
	c.prevCPUs = cpus
	c.mu.Unlock()

	if prev == nil || prevTotal == 0 || total <= prevTotal || prevCPUs <= 0 {
		return out
	}
	deltaTotal := float64(total - prevTotal)
	// ncpu 取上一次与这一次的较小值：热插拔 CPU 时用新的核数会把百分比放大。
	n := prevCPUs
	if cpus > 0 && cpus < n {
		n = cpus
	}
	for _, pid := range pids {
		before, ok := prev[pid]
		if !ok || before.startTick != rows[pid].startTick {
			continue
		}
		nowTicks := rows[pid].cpuTicks()
		if nowTicks < before.ticks {
			// 计数器回绕或进程刚被重置：不猜，这一轮不给值。
			continue
		}
		pct := 100 * float64(nowTicks-before.ticks) * float64(n) / deltaTotal
		if pct < 0 {
			pct = 0
		}
		max := float64(n) * 100
		if pct > max {
			pct = max
		}
		rounded := round1(pct)
		out[pid] = &rounded
	}
	return out
}

// readTotal 读取 /proc/stat 的总 CPU 滴答数与 CPU 数。
//
// 只用汇总行 "cpu" 的数值列：它是所有 cpuN 的和，再把 cpuN 也加进来会把分母
// 直接翻倍（每个进程的百分比因此只剩一半）。cpuN 行只用来数核数。
//
// 读不到时返回零值：CPU 百分比随后会保持 null，而不是显示一个编出来的 0。
func (c *Collector) readTotal() (uint64, int) {
	data, err := c.fs.ReadFile(c.paths.statFile())
	if err != nil {
		return 0, 0
	}
	var total uint64
	cpus := 0
	for _, line := range splitLines(data) {
		fields := fieldsOf(line)
		if len(fields) == 0 {
			continue
		}
		name := fields[0]
		if isCPUName(name) {
			cpus++
			continue
		}
		if name != "cpu" {
			continue
		}
		// guest 已含在 user，guest_nice 已含在 nice；再相加会虚增分母。
		for _, field := range fields[1:min(len(fields), 9)] {
			total += parseUint(field)
		}
	}
	if cpus == 0 && total > 0 {
		// 只有汇总行（极老的内核）：按 1 个 CPU 处理，百分比至少方向正确。
		cpus = 1
	}
	return total, cpus
}

// loadUsers 读 /etc/passwd；读不到就返回空表（界面显示 uid:<数字>）。
func (c *Collector) loadUsers() map[int64]string {
	data, err := c.fs.ReadFile(c.paths.Passwd)
	if err != nil {
		return map[int64]string{}
	}
	return parsePasswd(data)
}

// toProcess 把一行原始信息转成对外 DTO。
func (c *Collector) toProcess(row procRow, cpu *float64, users map[int64]string, source rowSource) Process {
	protection := Protect(row, source, c.selfPID)
	var memory *string
	if row.rssBytes != nil {
		text := formatBytes(*row.rssBytes)
		memory = &text
	}
	return Process{
		PID:            row.pid,
		StartTimeTicks: row.startTick,
		PPID:           row.ppid,
		Owner:          ownerName(users, row.uid),
		State:          row.state,
		CPUPercent:     cpu,
		MemoryBytes:    memory,
		Command:        SanitizeCommand(row.argv, row.comm),
		Unit:           row.unit,
		Protection:     protection,
		Capabilities:   capabilitiesFor(row.state, protection.Protected),
	}
}

// Live 现场读取一个 PID（不走缓存）。found=false 表示这个 PID 上没有进程。
//
// 预检、详情与执行前复核都走它：快照最多 5 秒旧，而“现在这个 PID 上是不是那个
// 进程”必须看现在。
func (c *Collector) Live(pid int64) (Process, bool, error) {
	row, ok := readRow(c.fs, c.paths, pid)
	if !ok {
		return Process{}, false, nil
	}
	source := &liveSource{collector: c}
	return c.toProcess(row, nil, c.loadUsers(), source), true, nil
}

// liveSource 是现场读取用的行来源：每次都读 /proc，不做缓存。
type liveSource struct {
	collector *Collector
}

func (s *liveSource) rowOf(pid int64) (procRow, bool) {
	return readRow(s.collector.fs, s.collector.paths, pid)
}

// fail 构造一份失败快照：内容保留上一次成功的结果，时间也停在那一刻。
func (c *Collector) fail(now time.Time, reason string) Snapshot {
	c.mu.Lock()
	prev := c.cached
	has := c.has
	c.mu.Unlock()

	snap := Snapshot{
		Status:     StatusError,
		IntervalMS: c.interval.Milliseconds(),
		Processes:  []Process{},
	}
	if has {
		snap.CollectedAt = prev.CollectedAt
		snap.Total = prev.Total
		snap.Truncated = prev.Truncated
		snap.Processes = prev.Processes
	} else {
		snap.CollectedAt = time.Time{}
	}
	// 失败不推进 CollectedAt，但要把“这次尝试的时间”记在错误里，便于判断卡了多久。
	snap.Error = reason + "（采集尝试于 " + now.Format(time.RFC3339) + "）"
	c.store(snap)
	return snap
}

// store 写入缓存。
func (c *Collector) store(snap Snapshot) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cached = snap
	c.has = true
}
