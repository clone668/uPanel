package auth

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestLoginIssueAndAuthenticate(t *testing.T) {
	clock := newFakeClock()
	svc := testService(t, Options{Now: clock.Now})
	ctx := context.Background()
	user := mustCreateAdmin(t, svc, "admin")

	issued, err := svc.Login(ctx, LoginInput{
		Username:   "Admin",
		Password:   testPassword,
		RemoteAddr: "192.0.2.10",
	})
	if err != nil {
		t.Fatalf("登录失败: %v", err)
	}
	if issued.Token == "" || issued.CSRF == "" {
		t.Fatal("登录必须同时签发会话令牌与 CSRF 令牌")
	}
	if issued.Token == issued.CSRF {
		t.Fatal("会话令牌与 CSRF 令牌不能相同")
	}
	if want := clock.Now().Add(DefaultIdleTimeout); !issued.ExpiresAt.Equal(want) {
		t.Fatalf("过期时间 = %s，期望 %s", issued.ExpiresAt, want)
	}

	// 库里只能有哈希，不能有明文令牌。
	var storedHash, storedCSRF string
	if err := svc.db.SQL().QueryRowContext(ctx,
		`SELECT id_hash, csrf_hash FROM sessions WHERE user_id = ?`, user.ID).
		Scan(&storedHash, &storedCSRF); err != nil {
		t.Fatalf("读取会话失败: %v", err)
	}
	if storedHash == issued.Token || storedCSRF == issued.CSRF {
		t.Fatal("库里不能保存明文令牌")
	}
	if storedHash != HashToken(issued.Token) || storedCSRF != HashToken(issued.CSRF) {
		t.Fatal("会话表应保存令牌的 SHA-256 哈希")
	}

	sess, err := svc.Authenticate(ctx, issued.Token)
	if err != nil {
		t.Fatalf("校验会话失败: %v", err)
	}
	if sess.User.ID != user.ID || sess.User.Username != "admin" {
		t.Fatalf("会话账号 = %+v", sess.User)
	}

	if _, err := svc.Authenticate(ctx, "not-a-real-token"); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("伪造令牌应返回 ErrSessionInvalid，实际 %v", err)
	}
	if _, err := svc.Authenticate(ctx, ""); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("空令牌应返回 ErrSessionInvalid，实际 %v", err)
	}
}

func TestLoginRejectsWrongPasswordAndUnknownUser(t *testing.T) {
	svc := testService(t, Options{})
	ctx := context.Background()
	mustCreateAdmin(t, svc, "admin")

	if _, err := svc.Login(ctx, LoginInput{Username: "admin", Password: "wrong-password-1"}); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("错误密码应返回 ErrInvalidCredentials，实际 %v", err)
	}
	// 账号不存在与密码错误必须是同一个错误：否则可以用来枚举账号。
	if _, err := svc.Login(ctx, LoginInput{Username: "nobody", Password: testPassword}); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("不存在的账号应返回 ErrInvalidCredentials，实际 %v", err)
	}
}

func TestLoginRotatesPreviousSession(t *testing.T) {
	svc := testService(t, Options{})
	ctx := context.Background()
	mustCreateAdmin(t, svc, "admin")

	first, err := svc.Login(ctx, LoginInput{Username: "admin", Password: testPassword})
	if err != nil {
		t.Fatalf("首次登录失败: %v", err)
	}
	second, err := svc.Login(ctx, LoginInput{
		Username:      "admin",
		Password:      testPassword,
		PreviousToken: first.Token,
	})
	if err != nil {
		t.Fatalf("再次登录失败: %v", err)
	}
	if second.Token == first.Token || second.CSRF == first.CSRF {
		t.Fatal("重新登录必须签发全新的会话与 CSRF 令牌")
	}
	if _, err := svc.Authenticate(ctx, first.Token); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("旧会话应被作废，实际 %v", err)
	}
	if _, err := svc.Authenticate(ctx, second.Token); err != nil {
		t.Fatalf("新会话应有效: %v", err)
	}
}

func TestLoginRateLimit(t *testing.T) {
	clock := newFakeClock()
	svc := testService(t, Options{Now: clock.Now})
	ctx := context.Background()
	mustCreateAdmin(t, svc, "admin")

	const remote = "192.0.2.20"
	for i := 0; i < DefaultLimits().PerUsername.Failures; i++ {
		if _, err := svc.Login(ctx, LoginInput{Username: "admin", Password: "wrong-password-1", RemoteAddr: remote}); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("第 %d 次失败应返回 ErrInvalidCredentials，实际 %v", i+1, err)
		}
	}

	_, err := svc.Login(ctx, LoginInput{Username: "admin", Password: testPassword, RemoteAddr: remote})
	var limited *RateLimitedError
	if !errors.As(err, &limited) {
		t.Fatalf("超过失败次数后应被限速，实际 %v", err)
	}
	if limited.RetryAfter <= 0 {
		t.Fatalf("限速应给出等待时间，实际 %s", limited.RetryAfter)
	}

	// 窗口到期后自动恢复：不存在永久锁定。
	clock.Advance(DefaultLimits().PerUsername.Window + time.Second)
	if _, err := svc.Login(ctx, LoginInput{Username: "admin", Password: testPassword, RemoteAddr: remote}); err != nil {
		t.Fatalf("窗口到期后应允许登录: %v", err)
	}
}

func TestLoginRateLimitUsesBothDimensions(t *testing.T) {
	clock := newFakeClock()
	svc := testService(t, Options{Now: clock.Now})
	ctx := context.Background()
	mustCreateAdmin(t, svc, "admin")
	mustCreateAdmin(t, svc, "second")

	// 用户名维度：连续失败后，即使换来源也应被拦。
	for i := 0; i < DefaultLimits().PerUsername.Failures; i++ {
		_, _ = svc.Login(ctx, LoginInput{Username: "admin", Password: "wrong-password-1", RemoteAddr: "192.0.2.30"})
	}
	if _, err := svc.Login(ctx, LoginInput{Username: "admin", Password: testPassword, RemoteAddr: "192.0.2.31"}); err == nil {
		t.Fatal("用户名维度被拦后，换来源不应立即放行")
	}

	// 来源维度：同一来源横扫多个不存在的账号，累计到上限后该来源被拦。
	for i := 0; i < DefaultLimits().PerIP.Failures; i++ {
		_, _ = svc.Login(ctx, LoginInput{
			Username:   fmt.Sprintf("ghost%d", i),
			Password:   "wrong-password-1",
			RemoteAddr: "192.0.2.40",
		})
	}
	_, err := svc.Login(ctx, LoginInput{Username: "second", Password: testPassword, RemoteAddr: "192.0.2.40"})
	var limited *RateLimitedError
	if !errors.As(err, &limited) {
		t.Fatalf("同一来源横扫多个账号后应被限速，实际 %v", err)
	}

	// 同样的账号换个来源仍然可以登录：限速不是账号锁定。
	clock.Advance(DefaultLimits().PerUsername.Window + time.Second)
	if _, err := svc.Login(ctx, LoginInput{Username: "second", Password: testPassword, RemoteAddr: "192.0.2.41"}); err != nil {
		t.Fatalf("正常来源应可登录: %v", err)
	}
}

func TestIdleTimeoutAndSlidingTouch(t *testing.T) {
	clock := newFakeClock()
	svc := testService(t, Options{Now: clock.Now})
	ctx := context.Background()
	mustCreateAdmin(t, svc, "admin")

	issued, err := svc.Login(ctx, LoginInput{Username: "admin", Password: testPassword})
	if err != nil {
		t.Fatalf("登录失败: %v", err)
	}

	// 半程访问一次：会话应滑动续期。
	clock.Advance(20 * time.Minute)
	if _, err := svc.Authenticate(ctx, issued.Token); err != nil {
		t.Fatalf("空闲 20 分钟后会话仍应有效: %v", err)
	}
	// 再空闲 20 分钟（累计 40 分钟 > 30 分钟），但因为刚才续期过，仍然有效。
	clock.Advance(20 * time.Minute)
	if _, err := svc.Authenticate(ctx, issued.Token); err != nil {
		t.Fatalf("续期后应继续有效: %v", err)
	}

	// 超过空闲超时且期间无访问 → 失效。
	clock.Advance(DefaultIdleTimeout + time.Minute)
	if _, err := svc.Authenticate(ctx, issued.Token); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("超过空闲超时应失效，实际 %v", err)
	}
}

func TestTouchIsThrottledToOncePerMinute(t *testing.T) {
	clock := newFakeClock()
	svc := testService(t, Options{Now: clock.Now})
	ctx := context.Background()
	mustCreateAdmin(t, svc, "admin")

	issued, err := svc.Login(ctx, LoginInput{Username: "admin", Password: testPassword})
	if err != nil {
		t.Fatalf("登录失败: %v", err)
	}

	read := func() string {
		var lastSeen string
		if err := svc.db.SQL().QueryRowContext(ctx,
			`SELECT last_seen_at FROM sessions WHERE id_hash = ?`, HashToken(issued.Token)).Scan(&lastSeen); err != nil {
			t.Fatalf("读取 last_seen_at 失败: %v", err)
		}
		return lastSeen
	}

	before := read()
	clock.Advance(10 * time.Second)
	if _, err := svc.Authenticate(ctx, issued.Token); err != nil {
		t.Fatalf("校验失败: %v", err)
	}
	if read() != before {
		t.Fatal("一分钟内的多次访问不应每次都写库")
	}

	clock.Advance(time.Minute)
	if _, err := svc.Authenticate(ctx, issued.Token); err != nil {
		t.Fatalf("校验失败: %v", err)
	}
	if read() == before {
		t.Fatal("超过一分钟应更新 last_seen_at")
	}
}

func TestAbsoluteTimeoutCannotBeExtended(t *testing.T) {
	clock := newFakeClock()
	svc := testService(t, Options{Now: clock.Now})
	ctx := context.Background()
	mustCreateAdmin(t, svc, "admin")

	issued, err := svc.Login(ctx, LoginInput{Username: "admin", Password: testPassword})
	if err != nil {
		t.Fatalf("登录失败: %v", err)
	}

	// 每 20 分钟访问一次，持续 13 小时：滑动续期不能突破 12 小时绝对上限。
	deadline := clock.Now().Add(DefaultAbsoluteTimeout)
	invalidAt := time.Time{}
	for i := 0; i < 40; i++ {
		clock.Advance(20 * time.Minute)
		_, err := svc.Authenticate(ctx, issued.Token)
		if errors.Is(err, ErrSessionInvalid) {
			invalidAt = clock.Now()
			break
		}
		if err != nil {
			t.Fatalf("校验出错: %v", err)
		}
	}
	if invalidAt.IsZero() {
		t.Fatal("活跃会话应在 12 小时后被绝对超时终止")
	}
	if invalidAt.Sub(deadline) > 20*time.Minute {
		t.Fatalf("绝对超时发生在 %s，明显晚于 %s", invalidAt, deadline)
	}
}

func TestCSRFVerificationAndRotation(t *testing.T) {
	svc := testService(t, Options{})
	ctx := context.Background()
	mustCreateAdmin(t, svc, "admin")

	issued, err := svc.Login(ctx, LoginInput{Username: "admin", Password: testPassword})
	if err != nil {
		t.Fatalf("登录失败: %v", err)
	}
	sess, err := svc.Authenticate(ctx, issued.Token)
	if err != nil {
		t.Fatalf("校验会话失败: %v", err)
	}

	if err := svc.VerifyCSRF(sess, issued.CSRF); err != nil {
		t.Fatalf("正确 CSRF 令牌应通过: %v", err)
	}
	if err := svc.VerifyCSRF(sess, "wrong-token"); !errors.Is(err, ErrCSRF) {
		t.Fatalf("错误 CSRF 令牌应返回 ErrCSRF，实际 %v", err)
	}
	if err := svc.VerifyCSRF(sess, ""); !errors.Is(err, ErrCSRF) {
		t.Fatalf("空 CSRF 令牌应返回 ErrCSRF，实际 %v", err)
	}
	// 会话令牌本身不能当 CSRF 令牌用（两者独立）。
	if err := svc.VerifyCSRF(sess, issued.Token); !errors.Is(err, ErrCSRF) {
		t.Fatalf("会话令牌不能充当 CSRF 令牌，实际 %v", err)
	}

	rotated, err := svc.RotateCSRF(ctx, sess.ID)
	if err != nil {
		t.Fatalf("轮换 CSRF 失败: %v", err)
	}
	if rotated == issued.CSRF {
		t.Fatal("轮换后令牌必须变化")
	}
	sess2, err := svc.Authenticate(ctx, issued.Token)
	if err != nil {
		t.Fatalf("校验会话失败: %v", err)
	}
	if err := svc.VerifyCSRF(sess2, rotated); err != nil {
		t.Fatalf("轮换后的令牌应通过: %v", err)
	}
	if err := svc.VerifyCSRF(sess2, issued.CSRF); !errors.Is(err, ErrCSRF) {
		t.Fatalf("旧令牌应失效，实际 %v", err)
	}
}

func TestRevokeAndRevokeAllForUser(t *testing.T) {
	svc := testService(t, Options{})
	ctx := context.Background()
	user := mustCreateAdmin(t, svc, "admin")

	first, err := svc.Login(ctx, LoginInput{Username: "admin", Password: testPassword})
	if err != nil {
		t.Fatalf("登录失败: %v", err)
	}
	second, err := svc.Login(ctx, LoginInput{Username: "admin", Password: testPassword})
	if err != nil {
		t.Fatalf("登录失败: %v", err)
	}

	if err := svc.Revoke(ctx, first.Token); err != nil {
		t.Fatalf("作废会话失败: %v", err)
	}
	if _, err := svc.Authenticate(ctx, first.Token); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("退出后会话应立即失效，实际 %v", err)
	}
	// 幂等：重复作废不算错误。
	if err := svc.Revoke(ctx, first.Token); err != nil {
		t.Fatalf("重复作废应成功: %v", err)
	}

	if _, err := svc.Authenticate(ctx, second.Token); err != nil {
		t.Fatalf("其他会话不应受影响: %v", err)
	}

	n, err := svc.RevokeAllForUser(ctx, user.ID)
	if err != nil {
		t.Fatalf("作废账号全部会话失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("应作废 1 个会话，实际 %d", n)
	}
	if _, err := svc.Authenticate(ctx, second.Token); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("账号会话应全部失效，实际 %v", err)
	}
}

func TestDisabledUserCannotLoginAndExistingSessionDies(t *testing.T) {
	svc := testService(t, Options{})
	ctx := context.Background()
	user := mustCreateAdmin(t, svc, "admin")

	issued, err := svc.Login(ctx, LoginInput{Username: "admin", Password: testPassword})
	if err != nil {
		t.Fatalf("登录失败: %v", err)
	}

	if _, err := svc.db.SQL().ExecContext(ctx, `UPDATE users SET disabled = 1 WHERE id = ?`, user.ID); err != nil {
		t.Fatalf("禁用账号失败: %v", err)
	}

	if _, err := svc.Login(ctx, LoginInput{Username: "admin", Password: testPassword}); !errors.Is(err, ErrUserDisabled) {
		t.Fatalf("被禁用的账号应无法登录，实际 %v", err)
	}
	if _, err := svc.Authenticate(ctx, issued.Token); !errors.Is(err, ErrUserDisabled) {
		t.Fatalf("被禁用的账号已有会话应失效，实际 %v", err)
	}
	// 会话记录应被顺手删除，不留在库里。
	var count int
	if err := svc.db.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions WHERE user_id = ?`, user.ID).Scan(&count); err != nil {
		t.Fatalf("统计会话失败: %v", err)
	}
	if count != 0 {
		t.Fatalf("禁用账号的会话应被删除，实际仍有 %d 条", count)
	}
}

func TestPurgeExpired(t *testing.T) {
	clock := newFakeClock()
	svc := testService(t, Options{Now: clock.Now})
	ctx := context.Background()
	mustCreateAdmin(t, svc, "admin")

	for i := 0; i < 3; i++ {
		if _, err := svc.Login(ctx, LoginInput{Username: "admin", Password: testPassword}); err != nil {
			t.Fatalf("登录失败: %v", err)
		}
	}
	if n, err := svc.PurgeExpired(ctx); err != nil || n != 0 {
		t.Fatalf("未过期时不应删除，实际 %d（错误 %v）", n, err)
	}

	clock.Advance(DefaultIdleTimeout + time.Minute)
	n, err := svc.PurgeExpired(ctx)
	if err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	if n != 3 {
		t.Fatalf("应清理 3 条过期会话，实际 %d", n)
	}
}

func TestLoginUpgradesLegacyHash(t *testing.T) {
	svc := testService(t, Options{})
	ctx := context.Background()
	user := mustCreateAdmin(t, svc, "admin")

	// 换成一个旧参数（更弱）的哈希，模拟历史数据。
	weak := PasswordParams{Memory: 8 * 1024, Time: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32}
	weakHash, err := HashPasswordWith(weak, testPassword)
	if err != nil {
		t.Fatalf("生成弱哈希失败: %v", err)
	}
	if err := svc.updatePasswordHash(ctx, user.ID, weakHash); err != nil {
		t.Fatalf("写入弱哈希失败: %v", err)
	}

	if _, err := svc.Login(ctx, LoginInput{Username: "admin", Password: testPassword}); err != nil {
		t.Fatalf("登录失败: %v", err)
	}

	creds, err := svc.credentialsByUsername(ctx, "admin")
	if err != nil {
		t.Fatalf("读取凭据失败: %v", err)
	}
	if creds.hash == weakHash {
		t.Fatal("登录成功后应把旧参数哈希升级为当前默认参数")
	}
	if NeedsRehash(creds.hash) {
		t.Fatal("升级后的哈希不应再被判为需要升级")
	}
	// 升级后密码仍然可用。
	ok, err := VerifyPassword(testPassword, creds.hash)
	if err != nil || !ok {
		t.Fatalf("升级后的哈希应能校验通过（err=%v ok=%v）", err, ok)
	}
}
