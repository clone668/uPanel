package auth

import (
	"context"
	"errors"
	"testing"
	"time"
)

// loginAs 登录并返回会话与明文令牌（令牌只在测试里用于重新校验）。
func loginAs(t *testing.T, svc *Service, username string, addr string) (Session, string) {
	t.Helper()
	ctx := context.Background()
	issued, err := svc.Login(ctx, LoginInput{Username: username, Password: testPassword, RemoteAddr: addr})
	if err != nil {
		t.Fatalf("登录失败: %v", err)
	}
	sess, err := svc.Authenticate(ctx, issued.Token)
	if err != nil {
		t.Fatalf("校验会话失败: %v", err)
	}
	return sess, issued.Token
}

func TestReauthenticateSetsFiveMinuteWindow(t *testing.T) {
	clock := newFakeClock()
	svc := testService(t, Options{Now: clock.Now})
	ctx := context.Background()
	mustCreateAdmin(t, svc, "admin")
	sess, token := loginAs(t, svc, "admin", "192.0.2.50")

	if svc.ReauthValid(sess) {
		t.Fatal("刚登录的会话不应已经有再认证有效期")
	}

	until, err := svc.Reauthenticate(ctx, sess, testPassword, "192.0.2.50")
	if err != nil {
		t.Fatalf("再认证失败: %v", err)
	}
	if want := clock.Now().Add(ReauthWindow); !until.Equal(want) {
		t.Fatalf("再认证有效期 = %s，期望 %s", until, want)
	}

	// 必须落库：重新校验会话时能读到 reauth_until。
	again, err := svc.Authenticate(ctx, token)
	if err != nil {
		t.Fatalf("重新校验会话失败: %v", err)
	}
	if !again.ReauthUntil.Equal(until) {
		t.Fatalf("库里的 reauth_until = %s，期望 %s", again.ReauthUntil, until)
	}
	if !svc.ReauthValid(again) {
		t.Fatal("窗口内应判定为已再认证")
	}

	// 窗口边界：4 分钟后仍有效，6 分钟后失效。
	clock.Advance(4 * time.Minute)
	if !svc.ReauthValid(again) {
		t.Fatal("4 分钟后仍应在窗口内")
	}
	clock.Advance(2 * time.Minute)
	if svc.ReauthValid(again) {
		t.Fatal("6 分钟后应超出窗口")
	}
}

func TestReauthenticateRejectsWrongPassword(t *testing.T) {
	clock := newFakeClock()
	svc := testService(t, Options{Now: clock.Now})
	ctx := context.Background()
	mustCreateAdmin(t, svc, "admin")
	sess, _ := loginAs(t, svc, "admin", "192.0.2.51")

	if _, err := svc.Reauthenticate(ctx, sess, "wrong-password-1", "192.0.2.51"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("错误密码应返回 ErrInvalidCredentials，实际 %v", err)
	}
	if svc.ReauthValid(sess) {
		t.Fatal("失败的再认证不能产生有效期")
	}
}

func TestReauthenticateIsRateLimited(t *testing.T) {
	clock := newFakeClock()
	svc := testService(t, Options{Now: clock.Now})
	ctx := context.Background()
	mustCreateAdmin(t, svc, "admin")
	sess, _ := loginAs(t, svc, "admin", "192.0.2.52")

	limited := false
	for i := 0; i < 10; i++ {
		_, err := svc.Reauthenticate(ctx, sess, "wrong-password-1", "192.0.2.52")
		var rl *RateLimitedError
		if errors.As(err, &rl) {
			limited = true
			if rl.RetryAfter <= 0 {
				t.Fatalf("retry_after 必须为正数，实际 %s", rl.RetryAfter)
			}
			break
		}
	}
	if !limited {
		t.Fatal("连续失败的再认证必须触发限速（不能成为绕过登录限速的爆破入口）")
	}

	// 限速生效后，即使密码正确也必须先被挡住。
	if _, err := svc.Reauthenticate(ctx, sess, testPassword, "192.0.2.52"); err == nil {
		t.Fatal("限速窗口内即使密码正确也应被拒绝")
	}
}

func TestReauthenticateOnDeletedUserFails(t *testing.T) {
	svc := testService(t, Options{})
	ctx := context.Background()
	admin := mustCreateAdmin(t, svc, "admin")
	viewer := createViewer(t, svc, "alice")
	sess, _ := loginAs(t, svc, "alice", "192.0.2.53")

	if err := svc.DeleteUser(ctx, DeleteUserInput{
		ActorID: admin.ID, UserID: viewer.ID, Version: viewer.Version,
	}); err != nil {
		t.Fatalf("删除账号失败: %v", err)
	}

	// 会话已随账号删除，此时使用旧会话对象做再认证必须失败且不留有效期。
	if _, err := svc.Reauthenticate(ctx, sess, testPassword, "192.0.2.53"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("账号不存在时应返回 ErrInvalidCredentials，实际 %v", err)
	}
}
