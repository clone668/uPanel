package auth

import (
	"testing"
	"time"
)

func TestLimiterBlocksUntilWindowExpires(t *testing.T) {
	clock := newFakeClock()
	limiter := NewLimiter(DefaultLimits(), clock.Now)

	const user, ip = "user:admin", "ip:192.0.2.10"
	for i := 0; i < DefaultLimits().PerUsername.Failures; i++ {
		if wait, ok := limiter.Allow(user, ip); !ok {
			t.Fatalf("第 %d 次尝试就已被拦（等待 %s）", i+1, wait)
		}
		limiter.RecordFailure(user, ip)
	}

	wait, ok := limiter.Allow(user, ip)
	if ok {
		t.Fatal("达到失败上限后应被拦")
	}
	if wait <= 0 || wait > DefaultLimits().PerUsername.Window {
		t.Fatalf("等待时间 = %s，应在窗口之内", wait)
	}

	clock.Advance(DefaultLimits().PerUsername.Window + time.Second)
	if _, ok := limiter.Allow(user, ip); !ok {
		t.Fatal("窗口到期后应自动放行（不做永久锁定）")
	}
}

func TestLimiterResetSuccessOnlyClearsUsername(t *testing.T) {
	clock := newFakeClock()
	limiter := NewLimiter(DefaultLimits(), clock.Now)

	const user, ip = "user:admin", "ip:192.0.2.10"
	for i := 0; i < DefaultLimits().PerUsername.Failures; i++ {
		limiter.RecordFailure(user, ip)
	}
	limiter.ResetSuccess(user)

	if _, ok := limiter.Allow(user, ip); !ok {
		t.Fatal("成功登录后应清除该用户名的失败计数")
	}
	// 来源计数必须保留：否则攻击者用一个已知正确账号就能反复重置来源计数。
	// 这里用别的用户名继续累加同一个来源，避免用户名维度先被触发。
	for i := DefaultLimits().PerUsername.Failures; i < DefaultLimits().PerIP.Failures; i++ {
		limiter.RecordFailure("user:noise", ip)
	}
	if _, ok := limiter.Allow(user, ip); ok {
		t.Fatal("来源维度计数不应被 ResetSuccess 清除")
	}
}

func TestLimiterPrune(t *testing.T) {
	clock := newFakeClock()
	limiter := NewLimiter(DefaultLimits(), clock.Now)

	limiter.RecordFailure("user:a", "ip:1.2.3.4")
	limiter.RecordFailure("user:b", "ip:1.2.3.5")
	if n := limiter.Prune(); n != 0 {
		t.Fatalf("未过期时不应清理，实际 %d", n)
	}

	clock.Advance(DefaultLimits().PerIP.Window + time.Second)
	if n := limiter.Prune(); n != 4 {
		t.Fatalf("应清理 4 个过期键，实际 %d", n)
	}
	if _, ok := limiter.Allow("user:a", "ip:1.2.3.4"); !ok {
		t.Fatal("清理后应放行")
	}
}

func TestLimiterIgnoresEmptyKeys(t *testing.T) {
	limiter := NewLimiter(DefaultLimits(), nil)
	// 空键不能被当成一个共享桶：否则所有未识别来源会互相干扰。
	for i := 0; i < 100; i++ {
		limiter.RecordFailure("", "")
	}
	if _, ok := limiter.Allow("", ""); !ok {
		t.Fatal("空键不应被计数")
	}
}

func TestLimiterTreatsZeroLimitsAsDefaults(t *testing.T) {
	limiter := NewLimiter(Limits{}, newFakeClock().Now)
	for i := 0; i < DefaultLimits().PerUsername.Failures; i++ {
		if _, ok := limiter.Allow("user:x", "ip:1"); !ok {
			t.Fatalf("零值参数应退回默认值，第 %d 次尝试被拦", i+1)
		}
		limiter.RecordFailure("user:x", "ip:1")
	}
	if _, ok := limiter.Allow("user:x", "ip:1"); ok {
		t.Fatal("零值参数退回默认值后仍应生效")
	}
}
