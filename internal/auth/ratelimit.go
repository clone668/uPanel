package auth

import (
	"sync"
	"time"
)

// 登录限速（PLAN.md 第 5 节：按 IP 和用户名双重限速，不做永久锁定）。
//
// 实现是**进程内**固定窗口计数器：不落库、不跨进程共享。这样做的取舍写在
// docs/DECISIONS.md（Q23）：只有单实例 Web 服务，落库会引入写放大与清理负担，
// 而进程内计数在服务重启后清零，恰好对应“不做永久锁定”的要求。
//
// 计数键只在内存里存在，不进日志、不进数据库。

// Limit 是单个键在窗口内允许的失败次数。
type Limit struct {
	// Failures 为窗口内允许的失败次数上限；超过即拒绝该键的后续尝试。
	Failures int
	// Window 为固定窗口长度。
	Window time.Duration
}

// Limits 是两组限速参数。
type Limits struct {
	// PerUsername 按用户名限速：防针对某个账号的暴力破解。
	PerUsername Limit
	// PerIP 按来源地址限速：防同一来源横扫多个账号。
	PerIP Limit
}

// DefaultLimits 返回默认限速参数。
//
// 用户名 5 次 / 15 分钟，来源 20 次 / 15 分钟：正常用户输错几次不会触发，
// 自动化猜解很快撞墙；窗口到期自动恢复，因此不存在永久锁定。
func DefaultLimits() Limits {
	return Limits{
		PerUsername: Limit{Failures: 5, Window: 15 * time.Minute},
		PerIP:       Limit{Failures: 20, Window: 15 * time.Minute},
	}
}

// normalize 把非法（零值/负数）参数换成默认值，避免“0 次上限”把所有人锁死。
func (l Limits) normalize() Limits {
	def := DefaultLimits()
	if l.PerUsername.Failures <= 0 {
		l.PerUsername.Failures = def.PerUsername.Failures
	}
	if l.PerUsername.Window <= 0 {
		l.PerUsername.Window = def.PerUsername.Window
	}
	if l.PerIP.Failures <= 0 {
		l.PerIP.Failures = def.PerIP.Failures
	}
	if l.PerIP.Window <= 0 {
		l.PerIP.Window = def.PerIP.Window
	}
	return l
}

// bucket 是一个键的固定窗口计数。
type bucket struct {
	failures int
	resetAt  time.Time
}

// fixedWindowPruneAfter 是触发清理的内存阈值：超过这个键数量就顺手扫一遍过期键。
const fixedWindowPruneAfter = 4096

// Limiter 是进程内固定窗口限速器。并发安全。
type Limiter struct {
	mu      sync.Mutex
	limits  Limits
	now     func() time.Time
	buckets map[string]*bucket
}

// NewLimiter 构造限速器。now 为 nil 时使用 time.Now。
func NewLimiter(limits Limits, now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{
		limits:  limits.normalize(),
		now:     now,
		buckets: make(map[string]*bucket, 16),
	}
}

// Allow 判断本次尝试是否可以放行。
//
// 不放行时返回还需等待多久（取被拦住的键里较长的剩余时间）。
// 只读：不消耗配额；只有 RecordFailure 才计数。
func (l *Limiter) Allow(userKey, ipKey string) (time.Duration, bool) {
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	var wait time.Duration
	blocked := false
	for _, item := range []struct {
		key   string
		limit Limit
	}{
		{userKey, l.limits.PerUsername},
		{ipKey, l.limits.PerIP},
	} {
		if item.key == "" {
			continue
		}
		b, ok := l.buckets[item.key]
		if !ok {
			continue
		}
		if !now.Before(b.resetAt) {
			// 窗口已过：这条记录不再算数（真正的删除交给清理或下次 RecordFailure 覆盖）。
			delete(l.buckets, item.key)
			continue
		}
		if b.failures >= item.limit.Failures {
			blocked = true
			if remain := b.resetAt.Sub(now); remain > wait {
				wait = remain
			}
		}
	}
	if blocked {
		if wait < time.Second {
			wait = time.Second
		}
		return wait, false
	}
	return 0, true
}

// RecordFailure 记录一次登录失败，按用户名与来源各计一次。
func (l *Limiter) RecordFailure(userKey, ipKey string) {
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	if len(l.buckets) > fixedWindowPruneAfter {
		l.pruneLocked(now)
	}
	for _, item := range []struct {
		key   string
		limit Limit
	}{
		{userKey, l.limits.PerUsername},
		{ipKey, l.limits.PerIP},
	} {
		if item.key == "" {
			continue
		}
		b, ok := l.buckets[item.key]
		if !ok || !now.Before(b.resetAt) {
			l.buckets[item.key] = &bucket{failures: 1, resetAt: now.Add(item.limit.Window)}
			continue
		}
		b.failures++
	}
}

// ResetSuccess 在登录成功后清除该用户名的失败计数。
//
// 只清用户名键，**不清**来源键：否则攻击者用一个已知正确账号即可反复重置
// 来源计数，把按 IP 的限速变成摆设。来源计数按窗口自然过期。
func (l *Limiter) ResetSuccess(userKey string) {
	if userKey == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.buckets, userKey)
}

// Prune 删除已过期的计数，返回删除条数。定期清理与测试使用。
func (l *Limiter) Prune() int {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	before := len(l.buckets)
	l.pruneLocked(now)
	return before - len(l.buckets)
}

func (l *Limiter) pruneLocked(now time.Time) {
	for key, b := range l.buckets {
		if !now.Before(b.resetAt) {
			delete(l.buckets, key)
		}
	}
}
