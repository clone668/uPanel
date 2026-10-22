// Package apphealth 按每个应用自己的 interval_seconds 触发健康探测，把结论写进
// app_health_results（P09.2）。
//
// 三件事刻意不在这里做：
//
//  1. **不自己发 HTTP 请求**。探测由代理执行（apps.probe），目标地址由代理从它自己的
//     发现结果里解析，永远只走 127.0.0.1。Web 进程要是有 HTTP 出站能力，那它就成了
//     一个能访问任意地址的东西，而“不能访问任意地址”正是这一层的安全边界。
//  2. **不在内存里留第二份结果**。结论落库，接口读库。两处各存一份必然出现
//     “页面上的时间比数据库里的新”，而后来的读者无从判断哪一份算数。
//  3. **不把失败当成不存在**。代理不可用时写的是 unknown + 原因，不是 error：
//     “我看不到”和“它坏了”是两件事，前者等一会儿就好，后者要人去修。
package apphealth

import (
	"context"
	"errors"

	"log/slog"
	"time"

	"upanel/internal/apps"
	"upanel/internal/discovery"
)

// 轮询节流。
const (
	// DefaultTick 是调度器检查“有没有到点的应用”的间隔。它比最小探测间隔（30 秒）
	// 小一个数量级：多查几次库不贵，而让一次探测迟到 25 秒会让人以为调度器停了。
	DefaultTick = 5 * time.Second
	// StepTimeout 是一轮探测的总时限。超过就放弃这一轮，下一轮再来——
	// 一个卡住的探测不该把整个调度器拖死。
	StepTimeout = 60 * time.Second
	// MinGap 是同一个应用两次**尝试**之间的最小间隔，用来兜住“代理挂了”这种
	// 全员失败的场景：不会因为配置里写着 30 秒就每 5 秒重试一次。
	MinGap = 5 * time.Second
)

// Prober 是探测执行端（生产实现是 *discovery.Fetcher）。
type Prober interface {
	Probe(ctx context.Context, params discovery.ProbeParams) (discovery.ProbeResult, error)
}

// Scheduler 按间隔触发探测。
type Scheduler struct {
	store  *apps.Service
	prober Prober
	logger *slog.Logger
	now    func() time.Time
	tick   time.Duration
}

// New 构造调度器。store 与 prober 都是必需依赖：缺任何一个都不该悄悄跑一个空循环。
func New(store *apps.Service, prober Prober, logger *slog.Logger) (*Scheduler, error) {
	if store == nil {
		return nil, errors.New("apphealth: 需要应用元数据服务")
	}
	if prober == nil {
		return nil, errors.New("apphealth: 需要探测执行端")
	}
	return &Scheduler{
		store:  store,
		prober: prober,
		logger: logger,
		now:    func() time.Time { return time.Now().UTC() },
		tick:   DefaultTick,
	}, nil
}

// Run 持续调度，直到 ctx 结束。
func (s *Scheduler) Run(ctx context.Context) {
	ticker := time.NewTicker(s.tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			probed, err := s.Step(ctx)
			if err != nil && s.logger != nil {
				s.logger.Warn("健康探测轮次失败", "error", err, "probed", probed)
			}
		}
	}
}

// Step 探一轮“到点的”应用，返回这一轮实际探测的个数。
//
// 到点的判定：从没探过，或距上次探测已经超过这个应用自己的 interval_seconds。
// 秒级取整，因此 30 秒的间隔不会因为几十毫秒的抖动变成 29 秒一次。
func (s *Scheduler) Step(ctx context.Context) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, StepTimeout)
	defer cancel()

	targets, err := s.store.HealthTargets(ctx)
	if err != nil {
		return 0, err
	}
	probed := 0
	for _, target := range targets {
		if ctx.Err() != nil {
			break
		}
		if !due(target, s.now()) {
			continue
		}
		unavailable := s.probeOne(ctx, target)
		probed++
		if !unavailable {
			continue
		}
		// 代理不可用时剩下的目标会以同样的理由失败：到此为止，别把每条应用都写一遍
		// 同样的 unknown。这一条如实记进日志，因为未探测的那些此刻**没有**新结论。
		if len(targets) > probed && s.logger != nil {
			s.logger.Warn("健康探测提前结束：代理不可用，本轮剩余目标未探测",
				"remaining", len(targets)-probed)
		}
		break
	}
	return probed, nil
}

// probeOne 探测一个应用并写入结论。返回 true 表示这次失败的原因是代理不可用。
func (s *Scheduler) probeOne(ctx context.Context, target apps.HealthTarget) bool {
	health := target.App.Health
	params := discovery.ProbeParams{
		TargetID:            health.TargetID,
		Scheme:              health.Scheme,
		Port:                health.Port,
		Path:                health.Path,
		ExpectedStatusCodes: health.ExpectedStatusCodes,
		TimeoutSeconds:      health.TimeoutSeconds,
	}
	now := s.now()

	result, err := s.prober.Probe(ctx, params)
	if err != nil {
		unavailable := errors.Is(err, discovery.ErrAgentUnavailable)
		message := err.Error()
		if unavailable {
			message = "代理不可用，本轮没有发起探测：" + err.Error()
		}
		s.record(target, apps.HealthResult{
			AppID:      target.App.ID,
			CheckedAt:  now,
			HTTPState:  apps.HTTPUnknown,
			TargetID:   health.TargetID,
			Scheme:     health.Scheme,
			Port:       health.Port,
			Path:       health.Path,
			DurationMS: 0,
			Error:      truncate(message, 1000),
		})
		return unavailable
	}

	if err := result.Valid(); err != nil {
		// 代理返回了一条缺地址的结论：不能拿定义里的值补上——那会把“不知道探的是哪儿”
		// 写成“探过了”。如实记 unknown。
		s.record(target, apps.HealthResult{
			AppID:      target.App.ID,
			CheckedAt:  now,
			HTTPState:  apps.HTTPUnknown,
			TargetID:   health.TargetID,
			Scheme:     health.Scheme,
			Port:       health.Port,
			Path:       health.Path,
			DurationMS: 0,
			Error:      truncate("代理返回的探测结论不完整："+err.Error(), 1000),
		})
		return false
	}

	s.record(target, apps.HealthResult{
		AppID:      target.App.ID,
		CheckedAt:  result.CheckedAt,
		HTTPState:  result.HTTPState,
		StatusCode: result.StatusCode,
		DurationMS: result.DurationMS,
		TargetID:   result.TargetID,
		Scheme:     result.Scheme,
		Port:       result.Port,
		Path:       result.Path,
		Address:    result.Address,
		URL:        result.URL,
		Error:      truncate(result.Error, 1000),
	})
	return false
}

// record 写入一条结论。
//
// 写失败只记日志：探测结果是最新的观测，不是用户提交的数据；一次写失败下一轮会再写，
// 而把调度器停下来只会让所有应用一起变成“很久没探测”。
func (s *Scheduler) record(target apps.HealthTarget, result apps.HealthResult) {
	if target.Last != nil && !result.CheckedAt.After(target.Last.CheckedAt) {
		// 时间没有前进（同一秒内的重复轮次）：改写它没有新信息，跳过。
		return
	}
	if err := s.store.SaveHealthResult(context.Background(), result); err != nil {
		if errors.Is(err, apps.ErrNotFound) {
			// 应用在探测期间被删了：正常竞态，不是错误。
			return
		}
		if s.logger != nil {
			s.logger.Warn("写入健康探测结果失败", "error", err, "app_id", result.AppID)
		}
		return
	}
	if s.logger != nil && result.HTTPState == apps.HTTPError {
		s.logger.Info("健康探测失败", "app_id", result.AppID, "target", result.TargetID,
			"port", result.Port, "status_code", result.StatusCode, "error", result.Error)
	}
}

// due 判断一个应用是否到了该探测的时候。
func due(target apps.HealthTarget, now time.Time) bool {
	if target.Last == nil {
		return true
	}
	interval := time.Duration(target.App.Health.IntervalSeconds) * time.Second
	if interval < MinGap {
		interval = MinGap
	}
	since := now.Sub(target.Last.CheckedAt)
	if since < 0 {
		// 库里的时间在未来（时钟被调过）：按到点处理，否则这条应用会卡到那个时间为止。
		return true
	}
	return since >= interval
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	// 按字节截断会把多字节字符切坏；退到 rune 边界。
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit])
}
