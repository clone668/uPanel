package auth

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Reauthenticate 校验当前会话账号的密码，成功后把会话的再认证有效期推到 now + 5 分钟。
//
// 依据 PLAN.md 第 5 节“危险操作必须在最近五分钟内完成密码再认证”，以及 CONTRACTS.md
// 第 2 节的 POST /auth/reauth。与登录共用同一个限速器：再认证失败同样计入失败次数，
// 避免把这里变成绕过登录限速的密码爆破入口。
//
// 只有 admin 会走到这里（路由层已拦截 viewer），因此不在这里判断角色。
func (s *Service) Reauthenticate(ctx context.Context, sess Session, password, remoteAddr string) (time.Time, error) {
	userKey := "user:" + sess.User.Username
	ipKey := "ip:" + remoteAddr

	if retry, ok := s.limiter.Allow(userKey, ipKey); !ok {
		return time.Time{}, &RateLimitedError{RetryAfter: retry}
	}

	creds, err := s.credentialsByUsername(ctx, sess.User.Username)
	if err != nil {
		if errors.Is(err, ErrUserNotFound) {
			// 会话指向的账号已被删除：与密码错误同样处理，并保持等价耗时。
			s.burnPasswordTime(password)
			s.limiter.RecordFailure(userKey, ipKey)
			return time.Time{}, ErrInvalidCredentials
		}
		return time.Time{}, err
	}

	ok, err := VerifyPassword(password, creds.hash)
	if err != nil {
		return time.Time{}, fmt.Errorf("auth: 账号 %s 的密码哈希不可用: %w", sess.User.Username, err)
	}
	if !ok {
		s.limiter.RecordFailure(userKey, ipKey)
		return time.Time{}, ErrInvalidCredentials
	}
	if creds.user.Disabled {
		s.limiter.RecordFailure(userKey, ipKey)
		return time.Time{}, ErrUserDisabled
	}
	s.limiter.ResetSuccess(userKey)

	until := s.nowUTC().Add(ReauthWindow)
	if _, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET reauth_until = ? WHERE id = ?`, formatTime(until), sess.ID); err != nil {
		return time.Time{}, fmt.Errorf("auth: 记录再认证时间失败: %w", err)
	}
	return until, nil
}

// ReauthValid 判断会话的再认证是否仍在有效期内。
//
// 与 Authenticate 共用注入的时钟，因此测试可以把时间拨到窗口之外。
func (s *Service) ReauthValid(sess Session) bool {
	if sess.ReauthUntil.IsZero() {
		return false
	}
	return s.nowUTC().Before(sess.ReauthUntil)
}
