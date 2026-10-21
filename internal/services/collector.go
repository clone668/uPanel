package services

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
)

// Options 是采集器参数。零值可用：10 秒采集、用 /usr/bin/systemctl（找不到则报依赖缺失）。
type Options struct {
	// Interval 是采集间隔；0 用 DefaultInterval。
	Interval time.Duration
	// Systemctl 是调用器；nil 表示本机没有 systemctl（采集会如实报依赖缺失）。
	Systemctl *Systemctl
	Logger    *slog.Logger
}

// Collector 周期采集服务单元列表并缓存最近一份快照。
//
// 失败策略与 storage 一致：采集失败**不清空**单元列表，只把 Status 改成 error、
// 记下原因，CollectedAt 停在上一次成功的时间。“读不到”不等于“机器上没有服务”。
type Collector struct {
	interval time.Duration
	ctl      *Systemctl
	logger   *slog.Logger

	// verifyTimeout/verifyInterval 是执行后等待状态收敛的参数；测试里调小，
	// 生产用 control.go 里的默认值。
	verifyTimeout  time.Duration
	verifyInterval time.Duration

	mu     sync.Mutex
	cached Snapshot
	has    bool
	// lastError 只在状态翻转时写日志，避免每 10 秒刷一条同样的错误。
	lastError string
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
	return &Collector{
		interval:       interval,
		ctl:            opts.Systemctl,
		logger:         logger,
		verifyTimeout:  verifyTimeout,
		verifyInterval: verifyInterval,
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

// refresh 采集一次并记日志（状态翻转才记 Error，恢复时记 Info）。
func (c *Collector) refresh(ctx context.Context) {
	snap := c.Collect(ctx)
	if snap.Status == StatusError {
		if snap.Error != c.lastError {
			c.lastError = snap.Error
			c.logger.Warn("服务采集失败，保留上一次成功快照", "error", snap.Error)
		}
		return
	}
	if c.lastError != "" {
		c.lastError = ""
		c.logger.Info("服务采集已恢复", "units", len(snap.Units))
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
		Error:      "服务状态尚未完成第一次采集",
		IntervalMS: c.interval.Milliseconds(),
		Units:      []Unit{},
	}
}

// Collect 现场采集一次（不读缓存），并更新缓存。
func (c *Collector) Collect(ctx context.Context) Snapshot {
	now := time.Now().UTC().Truncate(time.Second)
	if c.ctl == nil {
		return c.fail(now, "本机没有可用的 systemctl，无法读取服务状态")
	}

	names, err := c.ctl.ListUnitNames(ctx)
	if err != nil {
		return c.fail(now, err.Error())
	}
	sort.Strings(names)
	truncated := false
	if len(names) > MaxUnits {
		names = names[:MaxUnits]
		truncated = true
	}

	raws, err := c.ctl.Show(ctx, names)
	if err != nil {
		return c.fail(now, err.Error())
	}

	units := make([]Unit, 0, len(raws))
	for _, raw := range raws {
		u, ok := unitFromRaw(raw, now)
		if !ok {
			continue
		}
		units = append(units, u)
	}
	sort.Slice(units, func(i, j int) bool { return units[i].Unit < units[j].Unit })

	snap := Snapshot{
		Status:          StatusOK,
		CollectedAt:     now,
		IntervalMS:      c.interval.Milliseconds(),
		SnapshotVersion: ComputeSnapshotVersion(units),
		Units:           units,
		Truncated:       truncated,
	}
	c.store(snap)
	return snap
}

// fail 构造一份失败快照：内容保留上一次成功的结果，时间也停在那一刻。
func (c *Collector) fail(now time.Time, reason string) Snapshot {
	c.mu.Lock()
	prev := c.cached
	has := c.has
	c.mu.Unlock()

	snap := Snapshot{
		Status:     StatusError,
		Error:      reason,
		IntervalMS: c.interval.Milliseconds(),
		Units:      []Unit{},
	}
	if has {
		snap.CollectedAt = prev.CollectedAt
		snap.SnapshotVersion = prev.SnapshotVersion
		snap.Units = prev.Units
		snap.Truncated = prev.Truncated
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

// Unit 现场读取单个单元（不走缓存）。
//
// 详情页与预检都用它：列表可能是 10 秒前的，而“现在能不能 stop”必须看现在的状态。
//
// 模板单元（`foo@.service`）直接返回 found=false：systemctl 不会为这种名字返回属性，
// 而它又确实不是“可以操作的服务”。分类说明由接口层补一句“需要指定实例”，
// 这里不编造一份 LoadState。
func (c *Collector) Unit(ctx context.Context, name string) (Unit, bool, error) {
	if err := ValidateUnitName(name); err != nil {
		return Unit{}, false, err
	}
	if IsTemplate(name) {
		return Unit{}, false, nil
	}
	if c.ctl == nil {
		return Unit{}, false, ErrSystemctlUnavailable
	}
	raw, found, err := c.ctl.ShowOne(ctx, name)
	if err != nil {
		return Unit{}, false, err
	}
	if !found {
		// 不存在也返回一份带 not-found 的单元，便于界面说清楚“这个单元不在这台机器上”。
		u, ok := unitFromRaw(raw, time.Now().UTC().Truncate(time.Second))
		if !ok {
			return Unit{}, false, nil
		}
		return u, false, nil
	}
	u, ok := unitFromRaw(raw, time.Now().UTC().Truncate(time.Second))
	if !ok {
		return Unit{}, false, nil
	}
	return u, true, nil
}

// unitFromRaw 把一次 show 的属性转成 Unit。
//
// 第二个返回值为 false 表示这行数据没有单元名（解析不到 Id），整条丢弃——
// 宁可少一行，也不要造一个名字是空的单元出来。
func unitFromRaw(raw RawUnit, now time.Time) (Unit, bool) {
	name := strings.TrimSpace(raw.Get("Id"))
	if name == "" {
		return Unit{}, false
	}
	if err := ValidateUnitName(name); err != nil {
		return Unit{}, false
	}
	u := Unit{
		Unit:          name,
		Description:   raw.Get("Description"),
		LoadState:     normalizeLoadState(raw.Get("LoadState")),
		ActiveState:   normalizeActiveState(raw.Get("ActiveState")),
		SubState:      raw.Get("SubState"),
		UnitFileState: raw.Get("UnitFileState"),
		Enablement:    normalizeEnablement(raw.Get("UnitFileState")),
		Template:      IsTemplate(name),
		MainPID:       parsePIDPtr(raw.Get("MainPID")),
		StartedAt:     parseTimestamp(raw.Get("ActiveEnterTimestamp")),
		RestartCount:  parseNumPtr(raw.Get("NRestarts")),
		MemoryBytes:   parseBytesPtr(raw.Get("MemoryCurrent")),
		TasksCurrent:  parseNumPtr(raw.Get("TasksCurrent")),
		FragmentPath:  raw.Get("FragmentPath"),
		Protection:    Protect(name),
		CheckedAt:     now,
	}
	u.Capabilities = capabilityFor(u)
	return u, true
}

// normalizeLoadState 把 LoadState 收进闭集；未知取值保留原词但按 error 处理更危险，
// 因此这里只做闭集映射，未识别的值原样返回（界面显示原词，不假装正常）。
func normalizeLoadState(state string) string {
	switch state {
	case LoadLoaded, LoadNotFound, LoadBadSetting, LoadError, LoadMerged, LoadMasked:
		return state
	}
	return state
}

// normalizeActiveState 同理。
func normalizeActiveState(state string) string {
	switch state {
	case ActiveActive, ActiveReloading, ActiveInactive, ActiveFailed,
		ActiveActivating, ActiveDeactivating, ActiveMaintenance:
		return state
	}
	return state
}
