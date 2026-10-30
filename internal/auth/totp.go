package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"upanel/internal/totp"
)

// 二次验证（P14.6）的**状态**层：这里只管"这个账号开没开、上一步是哪一步、
// 哪些恢复码已经用掉"。种子的生成、校验与销毁一概不在这里——种子在代理的加密库里，
// 由 internal/api 通过 agentclient 去问代理。分层的原因见 0014 迁移的头注释：
// 面板库是 Web 进程读得到的，而 Web 进程不是 root。

// 二次验证相关错误。
var (
	// ErrTotpNotEnrolled 表示这个账号没有启用二次验证。
	ErrTotpNotEnrolled = errors.New("auth: 这个账号没有启用二次验证")
	// ErrTotpAlreadyEnrolled 表示这个账号已经启用了二次验证。
	ErrTotpAlreadyEnrolled = errors.New("auth: 这个账号已经启用了二次验证")
	// ErrTotpReplay 表示这个验证码所在的时间步已经被用过了。
	ErrTotpReplay = errors.New("auth: 这个验证码已经用过了")
	// ErrRecoveryCodeInvalid 表示恢复码不对（不存在、已用过、或格式不对）。
	// 三种原因对外不区分：区分了就等于告诉攻击者"这个码存在但已经用过"。
	ErrRecoveryCodeInvalid = errors.New("auth: 恢复码无效")
)

// TotpState 是一个账号的二次验证状态。**不含任何种子材料**。
type TotpState struct {
	UserID int64
	// SecretRef 是代理侧那条种子的引用。
	SecretRef string
	Enabled   bool
	// LastStep 是最后一次被接受的时间步；-1 表示还没有用过任何一个。
	LastStep  int64
	UpdatedAt time.Time
}

// TotpState 读取一个账号的二次验证状态。第二个返回值表示"有没有这条记录"。
func (s *Service) TotpState(ctx context.Context, userID int64) (TotpState, bool, error) {
	var (
		st      TotpState
		enabled int
		updated string
	)
	err := s.db.SQL().QueryRowContext(ctx, `
		SELECT user_id, secret_ref, enabled, last_step, updated_at
		FROM user_totp WHERE user_id = ?`, userID).
		Scan(&st.UserID, &st.SecretRef, &enabled, &st.LastStep, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return TotpState{}, false, nil
	}
	if err != nil {
		return TotpState{}, false, fmt.Errorf("auth: 查询二次验证状态失败: %w", err)
	}
	st.Enabled = enabled != 0
	if st.UpdatedAt, err = parseTime(updated); err != nil {
		return TotpState{}, false, err
	}
	return st, true, nil
}

// EnableTotp 启用二次验证：写入种子引用、确认时匹配到的时间步，与十枚一次性恢复码。
//
// lastStep 是**确认用的那个六位数**匹配到的时间步，必须记下来。确认那一刻代理已经验过
// 这个码；若这里仍写 -1，同一个码在同一个 30 秒窗口里还能再用一次登录——一个被看到过的
// 确认码等于一把能立刻用的钥匙。真机第一轮就是在这里读到 `last_step = -1` 的
// （docs/DECISIONS.md Q480）。
//
// 恢复码的**明文**只在这一刻返回一次，库里只留哈希。十枚码的生成与落库在同一笔事务里：
// 启用成功却只剩九枚码（或者一枚都没写进去）会让"十枚"变成一句没人能验证的话。
func (s *Service) EnableTotp(ctx context.Context, userID int64, secretRef string, lastStep int64) ([]string, error) {
	if secretRef == "" {
		return nil, fmt.Errorf("auth: 启用二次验证需要代理返回的 secret_ref")
	}
	if lastStep < 0 {
		// 没有步就是没有防重放：宁可让这次启用失败（用户重扫一次码），
		// 也不要留下一行"看起来启用了、实际上第一个码能反复用"的状态。
		return nil, fmt.Errorf("auth: 启用二次验证需要确认时匹配到的时间步（收到 %d）", lastStep)
	}
	if _, exists, err := s.TotpState(ctx, userID); err != nil {
		return nil, err
	} else if exists {
		return nil, ErrTotpAlreadyEnrolled
	}

	codes, err := totp.GenerateRecoveryCodes()
	if err != nil {
		return nil, fmt.Errorf("auth: 生成恢复码失败: %w", err)
	}
	hashes := make([]string, len(codes))
	for i, code := range codes {
		if hashes[i], err = totp.HashRecoveryCode(code); err != nil {
			return nil, fmt.Errorf("auth: 计算恢复码哈希失败: %w", err)
		}
	}

	now := formatTime(s.nowUTC())
	err = s.InTransaction(ctx, func(ctx context.Context) error {
		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO user_totp (user_id, secret_ref, enabled, last_step, created_at, updated_at)
			VALUES (?, ?, 1, ?, ?, ?)`, userID, secretRef, lastStep, now, now); err != nil {
			return fmt.Errorf("auth: 写入二次验证状态失败: %w", err)
		}
		for _, h := range hashes {
			if _, err := s.db.ExecContext(ctx, `
				INSERT INTO user_totp_recovery_codes (user_id, code_hash, used_at, created_at)
				VALUES (?, ?, NULL, ?)`, userID, h, now); err != nil {
				return fmt.Errorf("auth: 写入恢复码失败: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.db.Logger().Info("已启用二次验证", "user_id", userID, "recovery_codes", len(codes))
	return codes, nil
}

// DisableTotp 关闭二次验证：删掉状态行与全部恢复码（外键 CASCADE）。
//
// 代理侧那条种子由调用方在此之前销毁（或者之后补一刀）；这里只负责面板库这一半。
// 删除而不是置 enabled=0：留着一条"停用的登记"意味着种子的引用还在，
// 而下一个人的问题是"它到底还能不能用来登录"——没有行就没有这个问题。
func (s *Service) DisableTotp(ctx context.Context, userID int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM user_totp WHERE user_id = ?`, userID)
	if err != nil {
		return fmt.Errorf("auth: 关闭二次验证失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("auth: 读取二次验证删除结果失败: %w", err)
	}
	if n == 0 {
		return ErrTotpNotEnrolled
	}
	s.db.Logger().Info("已关闭二次验证", "user_id", userID)
	return nil
}

// AcceptTotpStep 把"这个时间步已经被用掉"记下来，并拒绝不比自己大的步。
//
// 一条 UPDATE 完成判断与写入：`last_step < ?` 让两个并发的同一个码只能有一个成功，
// 读一次再写一次做不到这一点。重放防护因此不是"我记得我验过"，而是库里的一个事实。
func (s *Service) AcceptTotpStep(ctx context.Context, userID, step int64) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE user_totp SET last_step = ?, updated_at = ?
		WHERE user_id = ? AND enabled = 1 AND last_step < ?`,
		step, formatTime(s.nowUTC()), userID, step)
	if err != nil {
		return fmt.Errorf("auth: 记录已用时间步失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("auth: 读取时间步写入结果失败: %w", err)
	}
	if n == 0 {
		// 零行有两种可能：没启用（不该走到这里），或者步不比上一次大（重放）。
		return ErrTotpReplay
	}
	return nil
}

// ConsumeRecoveryCode 用掉一枚恢复码，返回剩下的可用枚数。
//
// 一次性由 `used_at IS NULL` 与一条带条件的 UPDATE 共同保证：两处同时用同一个码，
// 只有一处拿到 affected=1。剩下几枚要返回给界面——"还剩一枚"和"还剩九枚"是两件事，
// 前者该有人去补印一份新的。
func (s *Service) ConsumeRecoveryCode(ctx context.Context, userID int64, code string) (int, error) {
	hash, err := totp.HashRecoveryCode(code)
	if err != nil {
		return 0, ErrRecoveryCodeInvalid
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE user_totp_recovery_codes SET used_at = ?
		WHERE user_id = ? AND code_hash = ? AND used_at IS NULL`,
		formatTime(s.nowUTC()), userID, hash)
	if err != nil {
		return 0, fmt.Errorf("auth: 使用恢复码失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("auth: 读取恢复码写入结果失败: %w", err)
	}
	if n == 0 {
		return 0, ErrRecoveryCodeInvalid
	}
	remaining, err := s.RecoveryCodesRemaining(ctx, userID)
	if err != nil {
		return 0, err
	}
	s.db.Logger().Info("已使用一枚恢复码", "user_id", userID, "remaining", remaining)
	return remaining, nil
}

// RecoveryCodesRemaining 返回还有几枚恢复码没用。
func (s *Service) RecoveryCodesRemaining(ctx context.Context, userID int64) (int, error) {
	var remaining int
	err := s.db.SQL().QueryRowContext(ctx, `
		SELECT COUNT(*) FROM user_totp_recovery_codes
		WHERE user_id = ? AND used_at IS NULL`, userID).Scan(&remaining)
	if err != nil {
		return 0, fmt.Errorf("auth: 统计恢复码失败: %w", err)
	}
	return remaining, nil
}
