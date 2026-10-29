package auth

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"upanel/internal/schedules"
)

// 账号管理相关错误（P02.2）。
var (
	// ErrVersionConflict 表示提交的 version 与库里不一致：别人改过，必须在界面上刷新后重试。
	ErrVersionConflict = errors.New("auth: 账号已被其他人修改")
	// ErrLastAdmin 表示该操作会让系统失去最后一个可用管理员。
	ErrLastAdmin = errors.New("auth: 不能移除最后一个可用管理员")
	// ErrSelfModification 表示管理员试图改动自己的角色或启用状态。
	ErrSelfModification = errors.New("auth: 不能修改自己的角色或启用状态")
)

// isSystemAccount 判断 id 是否是定时触发的保留账号（P12.4）。
//
// 那一行在 users 里存在，只因为 jobs.user_id 是 NOT NULL 外键：定时触发需要一个
// "操作者"，而它不可能是任何真实管理员。它对账号管理**不存在**——列表里不出现、
// 按 id 查是"没有这个账号"、改和删都以同样的回答拒绝。这样就没有一条路径能让
// 它变成一个能登录的账号，也没有一条路径能删掉它把历史任务的外键拔掉。
func isSystemAccount(id int64) bool { return id == schedules.SystemUserID }

// ListUsers 返回当页账号（按用户名排序）与下一页游标。
//
// CONTRACTS.md 第 1 节：列表默认 limit=50、最大 200，cursor 不透明且排序稳定。
// 游标是上一页最后一个用户名的不透明编码，客户端不得解析。
func (s *Service) ListUsers(ctx context.Context, limit int, cursor string) ([]User, string, error) {
	if limit == 0 {
		limit = DefaultUserListLimit
	}
	if limit < 0 || limit > MaxUserListLimit {
		return nil, "", ErrInvalidLimit
	}
	after, err := decodeUserCursor(cursor)
	if err != nil {
		return nil, "", err
	}

	rows, err := s.db.SQL().QueryContext(ctx, `
		SELECT id, username, display_name, role, disabled, version, created_at, updated_at
		FROM users WHERE username > ? AND id <> ? ORDER BY username LIMIT ?`,
		after, schedules.SystemUserID, limit+1)
	if err != nil {
		return nil, "", fmt.Errorf("auth: 查询账号列表失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]User, 0, limit)
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("auth: 读取账号列表失败: %w", err)
	}

	// 多取一条只用于判断是否还有下一页，不返回给调用方。
	next := ""
	if len(out) > limit {
		out = out[:limit]
		next = encodeUserCursor(out[len(out)-1].Username)
	}
	return out, next, nil
}

// 账号列表分页常量。
const (
	// DefaultUserListLimit 是未指定 limit 时的条数。
	DefaultUserListLimit = 50
	// MaxUserListLimit 是单页条数上限。
	MaxUserListLimit = 200
)

var (
	// ErrInvalidLimit 表示 limit 超出 1—200。
	ErrInvalidLimit = errors.New("auth: 列表条数必须在 1 到 200 之间")
	// ErrInvalidCursor 表示 cursor 不是本服务签发的游标。
	ErrInvalidCursor = errors.New("auth: 翻页游标无效")
)

// encodeUserCursor 把用户名编码成不透明游标。
func encodeUserCursor(username string) string {
	return base64.RawURLEncoding.EncodeToString([]byte("user:" + username))
}

// decodeUserCursor 解析游标；空串表示第一页。
func decodeUserCursor(cursor string) (string, error) {
	if cursor == "" {
		return "", nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return "", ErrInvalidCursor
	}
	username, ok := strings.CutPrefix(string(raw), "user:")
	if !ok {
		return "", ErrInvalidCursor
	}
	return username, nil
}

// GetUser 按 id 查询账号。
//
// 保留账号在这里等同于"不存在"：它不是一个可以被查看、授予角色或启用的账号。
func (s *Service) GetUser(ctx context.Context, id int64) (User, error) {
	if isSystemAccount(id) {
		return User{}, ErrUserNotFound
	}
	return scanUser(s.db.SQL().QueryRowContext(ctx, `
		SELECT id, username, display_name, role, disabled, version, created_at, updated_at
		FROM users WHERE id = ?`, id))
}

// UpdateUserInput 是修改账号的输入。指针字段为 nil 表示“不改这一项”，
// 这样 PATCH 才能区分“没提供”和“显式设为空/假”。
type UpdateUserInput struct {
	// ActorID 是发起修改的管理员 id，用于拦截自我提权与自我禁用。
	ActorID int64
	UserID  int64
	Version int

	DisplayName *string
	Role        *Role
	Disabled    *bool
}

// UpdateUser 修改账号的显示名、角色或启用状态。
//
// 规则（CONTRACTS.md 第 2 节）：
//   - 不能自我提权，也不能自我禁用：管理员不能通过管理员接口改动自己的角色或启用状态；
//   - 不得让系统失去最后一个可用管理员（demote 或禁用最后一个 admin 都拒绝）；
//   - version 不匹配返回 ErrVersionConflict，调用方映射为 409。
func (s *Service) UpdateUser(ctx context.Context, in UpdateUserInput) (User, error) {
	if isSystemAccount(in.UserID) {
		// 改它没有意义也没有好结果：启用它等于造出一个没有密码的账号，
		// 提成 admin 等于给"定时任务"一份不该有的身份。答案同"不存在"。
		return User{}, ErrUserNotFound
	}
	var updated User
	err := s.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var (
			target    User
			disabled  int
			created   string
			updatedAt string
		)
		err := tx.QueryRowContext(ctx, `
			SELECT id, username, display_name, role, disabled, version, created_at, updated_at
			FROM users WHERE id = ?`, in.UserID).
			Scan(&target.ID, &target.Username, &target.DisplayName, &target.Role, &disabled,
				&target.Version, &created, &updatedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUserNotFound
		}
		if err != nil {
			return fmt.Errorf("auth: 读取账号失败: %w", err)
		}
		target.Disabled = disabled != 0
		if target.CreatedAt, err = parseTime(created); err != nil {
			return err
		}
		if target.UpdatedAt, err = parseTime(updatedAt); err != nil {
			return err
		}

		roleChanged := in.Role != nil && *in.Role != target.Role
		disabledChanged := in.Disabled != nil && *in.Disabled != target.Disabled
		if in.ActorID == target.ID && (roleChanged || disabledChanged) {
			return ErrSelfModification
		}

		displayName := target.DisplayName
		if in.DisplayName != nil {
			displayName = strings.TrimSpace(*in.DisplayName)
			if err := ValidateDisplayName(displayName); err != nil {
				return invalidInput(err)
			}
		}
		role := target.Role
		if in.Role != nil {
			if !in.Role.Valid() {
				return invalidInput(fmt.Errorf("未知角色 %q", string(*in.Role)))
			}
			role = *in.Role
		}
		disabledValue := target.Disabled
		if in.Disabled != nil {
			disabledValue = *in.Disabled
		}

		wasEnabledAdmin := target.Role == RoleAdmin && !target.Disabled
		stillEnabledAdmin := role == RoleAdmin && !disabledValue
		if wasEnabledAdmin && !stillEnabledAdmin {
			n, err := countEnabledAdmins(ctx, tx)
			if err != nil {
				return err
			}
			if n <= 1 {
				return ErrLastAdmin
			}
		}

		now := formatTime(s.nowUTC())
		res, err := tx.ExecContext(ctx, `
			UPDATE users SET display_name = ?, role = ?, disabled = ?, version = version + 1, updated_at = ?
			WHERE id = ? AND version = ?`,
			displayName, string(role), boolToInt(disabledValue), now, target.ID, in.Version)
		if err != nil {
			return fmt.Errorf("auth: 修改账号失败: %w", err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("auth: 修改账号失败: %w", err)
		}
		if affected == 0 {
			return ErrVersionConflict
		}

		target.DisplayName = displayName
		target.Role = role
		target.Disabled = disabledValue
		target.Version++
		target.UpdatedAt = s.nowUTC()
		updated = target
		return nil
	})
	if err != nil {
		return User{}, err
	}

	// 禁用账号时必须同时作废它的会话：否则已登录的浏览器还能再用一会儿。
	if updated.Disabled {
		if _, err := s.RevokeAllForUser(ctx, updated.ID); err != nil {
			return User{}, err
		}
	}
	return updated, nil
}

// SetPassword 重置账号密码，并作废该账号的全部会话。
//
// 不要求 version：密码重置是无损操作，契约里也没有 version 字段；但成功后
// 目标账号的现有会话必须立即失效（CONTRACTS.md 第 2 节）。
func (s *Service) SetPassword(ctx context.Context, userID int64, newPassword string) (User, error) {
	user, err := s.GetUser(ctx, userID)
	if err != nil {
		return User{}, err
	}
	if err := ValidatePassword(user.Username, newPassword); err != nil {
		return User{}, invalidInput(err)
	}
	hash, err := HashPasswordWith(s.params, newPassword)
	if err != nil {
		return User{}, err
	}

	err = s.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		now := formatTime(s.nowUTC())
		res, err := tx.ExecContext(ctx, `
			UPDATE users SET password_hash = ?, version = version + 1, updated_at = ?
			WHERE id = ?`, hash, now, userID)
		if err != nil {
			return fmt.Errorf("auth: 重置密码失败: %w", err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("auth: 重置密码失败: %w", err)
		}
		if affected == 0 {
			return ErrUserNotFound
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID); err != nil {
			return fmt.Errorf("auth: 作废账号会话失败: %w", err)
		}
		return nil
	})
	if err != nil {
		return User{}, err
	}
	return s.GetUser(ctx, userID)
}

// DeleteUserInput 是删除账号的输入。
type DeleteUserInput struct {
	ActorID int64
	UserID  int64
	Version int
}

// DeleteUser 删除账号。不能删除自己，也不能删除最后一个可用管理员。
func (s *Service) DeleteUser(ctx context.Context, in DeleteUserInput) error {
	if in.ActorID == in.UserID {
		return ErrSelfModification
	}
	if isSystemAccount(in.UserID) {
		// 删它等于让历史任务失去操作者（jobs.user_id 是 ON DELETE RESTRICT，库也会拒绝）。
		// 答案与"没有这个账号"一致：它从来不是一个账号。
		return ErrUserNotFound
	}
	return s.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var (
			role     Role
			disabled int
		)
		err := tx.QueryRowContext(ctx, `SELECT role, disabled FROM users WHERE id = ?`, in.UserID).
			Scan(&role, &disabled)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUserNotFound
		}
		if err != nil {
			return fmt.Errorf("auth: 读取账号失败: %w", err)
		}
		if role == RoleAdmin && disabled == 0 {
			n, err := countEnabledAdmins(ctx, tx)
			if err != nil {
				return err
			}
			if n <= 1 {
				return ErrLastAdmin
			}
		}

		res, err := tx.ExecContext(ctx, `DELETE FROM users WHERE id = ? AND version = ?`, in.UserID, in.Version)
		if err != nil {
			return fmt.Errorf("auth: 删除账号失败: %w", err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("auth: 删除账号失败: %w", err)
		}
		if affected == 0 {
			return ErrVersionConflict
		}
		return nil
	})
}

// countEnabledAdmins 统计“可用管理员”：role=admin 且未被禁用。
func countEnabledAdmins(ctx context.Context, tx *sql.Tx) (int, error) {
	var n int
	err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM users WHERE role = 'admin' AND disabled = 0`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("auth: 统计可用管理员失败: %w", err)
	}
	return n, nil
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
