package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"upanel/internal/schedules"
)

// Role 是账号角色（PLAN.md 第 5 节：0.1 只有 admin 与 viewer）。
type Role string

const (
	// RoleAdmin 可执行全部已实现的管理功能。
	RoleAdmin Role = "admin"
	// RoleViewer 只读，服务端用脱敏 DTO 返回数据。
	RoleViewer Role = "viewer"
)

// Valid 判断角色是否合法。
func (r Role) Valid() bool { return r == RoleAdmin || r == RoleViewer }

// 用户名与显示名约束。
const (
	MinUsernameLength = 3
	MaxUsernameLength = 32
	MaxDisplayNameLen = 64
)

// usernamePattern：小写字母开头，后续为小写字母、数字、点、下划线、连字符。
// 用户名会出现在审计与日志里，限制字符集可以避免引号、空白与控制字符带来的歧义。
var usernamePattern = regexp.MustCompile(`^[a-z][a-z0-9._-]{2,31}$`)

// 账号相关错误。
var (
	// ErrUserNotFound 表示账号不存在。
	ErrUserNotFound = errors.New("auth: 账号不存在")
	// ErrUsernameTaken 表示用户名已被占用（大小写不敏感）。
	ErrUsernameTaken = errors.New("auth: 用户名已存在")
	// ErrInvalidCredentials 表示用户名或密码错误。对外不区分两者，避免账号枚举。
	ErrInvalidCredentials = errors.New("auth: 用户名或密码错误")
	// ErrUserDisabled 表示账号已被禁用。
	ErrUserDisabled = errors.New("auth: 账号已禁用")
	// ErrNoUsers 表示数据库中还没有任何账号。
	ErrNoUsers = errors.New("auth: 还没有任何账号")
	// ErrUsersExist 表示已经初始化过账号，不能再创建“第一个管理员”。
	ErrUsersExist = errors.New("auth: 已存在账号，不能重复初始化")
)

// User 是账号的公开形态（不含密码哈希）。
type User struct {
	ID          int64
	Username    string
	DisplayName string
	Role        Role
	Disabled    bool
	Version     int
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// CreateUserInput 是创建账号的输入。
type CreateUserInput struct {
	Username    string
	DisplayName string
	Password    string
	Role        Role
}

// NormalizeUsername 归一化用户名：去掉首尾空白并统一小写。
//
// 用户名大小写不敏感（users.username 使用 COLLATE NOCASE），统一小写是为了让
// 审计、日志与界面显示只有一个形态。
func NormalizeUsername(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

// ValidateUsername 校验归一化后的用户名。
func ValidateUsername(username string) error {
	if !usernamePattern.MatchString(username) {
		return fmt.Errorf("用户名必须为 %d—%d 个字符，以小写字母开头，只含小写字母、数字、点、下划线或连字符",
			MinUsernameLength, MaxUsernameLength)
	}
	return nil
}

// ValidateDisplayName 校验显示名：可为空，最长 64 个字符，不允许控制字符。
func ValidateDisplayName(name string) error {
	name = strings.TrimSpace(name)
	if utf8.RuneCountInString(name) > MaxDisplayNameLen {
		return fmt.Errorf("显示名不能超过 %d 个字符", MaxDisplayNameLen)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return errors.New("显示名不能包含控制字符")
		}
	}
	return nil
}

// CountUsers 返回账号总数。
//
// 不数定时触发的保留账号（P12.4）：它不是一个人，"系统里有几个账号"这个问题
// 问的是人。首个管理员的创建判断也走这里，数进来就会变成"一个账号都没有，却不让
// 创建第一个管理员"。
func (s *Service) CountUsers(ctx context.Context) (int, error) {
	var n int
	if err := s.db.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE id <> ?`,
		schedules.SystemUserID).Scan(&n); err != nil {
		return 0, fmt.Errorf("auth: 统计账号失败: %w", err)
	}
	return n, nil
}

// InputError 表示调用方传入的参数不合法。
//
// 它与“系统错误”必须分开：HTTP 层要把它映射成 400 VALIDATION_ERROR，而不是 500。
// 只包装可预期的输入校验失败，数据库与哈希错误不套这一层。
type InputError struct {
	Err error
}

func (e *InputError) Error() string { return e.Err.Error() }

// Unwrap 支持 errors.Is/As 继续向内判断。
func (e *InputError) Unwrap() error { return e.Err }

func invalidInput(err error) error {
	if err == nil {
		return nil
	}
	return &InputError{Err: err}
}

// CreateUser 创建账号。密码在这里完成策略校验与 Argon2id 哈希，调用方不再接触明文。
func (s *Service) CreateUser(ctx context.Context, in CreateUserInput) (User, error) {
	username := NormalizeUsername(in.Username)
	if err := ValidateUsername(username); err != nil {
		return User{}, invalidInput(err)
	}
	if err := ValidateDisplayName(in.DisplayName); err != nil {
		return User{}, invalidInput(err)
	}
	if !in.Role.Valid() {
		return User{}, invalidInput(fmt.Errorf("未知角色 %q", string(in.Role)))
	}
	if err := ValidatePassword(username, in.Password); err != nil {
		return User{}, invalidInput(err)
	}

	hash, err := HashPasswordWith(s.params, in.Password)
	if err != nil {
		return User{}, err
	}

	now := s.nowUTC()
	// ExecContext 跟着 ctx 里的事务走：接口层把“创建账号 + 审计”合成一笔事务时，
	// 这条 INSERT 必须落在同一事务里，否则审计失败就回滚不掉账号。
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO users (username, display_name, password_hash, role, disabled, version, created_at, updated_at)
		VALUES (?, ?, ?, ?, 0, 1, ?, ?)`,
		username, strings.TrimSpace(in.DisplayName), hash, string(in.Role), formatTime(now), formatTime(now))
	if err != nil {
		if isUniqueViolation(err) {
			return User{}, ErrUsernameTaken
		}
		return User{}, fmt.Errorf("auth: 创建账号失败: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return User{}, fmt.Errorf("auth: 读取新账号 id 失败: %w", err)
	}
	return User{
		ID:          id,
		Username:    username,
		DisplayName: strings.TrimSpace(in.DisplayName),
		Role:        in.Role,
		Version:     1,
		CreatedAt:   now,
		UpdatedAt:   now,
	}, nil
}

// CreateFirstAdmin 创建第一个管理员；已有任何账号时返回 ErrUsersExist。
//
// 这是唯一“不需要已登录管理员”的创建路径，只由本机 CLI 调用（PLAN.md 第 5 节：
// 首个管理员由服务器本地 CLI 交互创建，没有默认密码与公开注册）。
func (s *Service) CreateFirstAdmin(ctx context.Context, username, displayName, password string) (User, error) {
	err := s.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE id <> ?`,
			schedules.SystemUserID).Scan(&n); err != nil {
			return fmt.Errorf("auth: 统计账号失败: %w", err)
		}
		if n > 0 {
			return ErrUsersExist
		}
		return nil
	})
	if err != nil {
		return User{}, err
	}
	// 事务外创建：Argon2 计算耗时较长，不占用写事务。
	return s.CreateUser(ctx, CreateUserInput{
		Username:    username,
		DisplayName: displayName,
		Password:    password,
		Role:        RoleAdmin,
	})
}

// FindUserByUsername 按用户名查询账号（大小写不敏感）。
func (s *Service) FindUserByUsername(ctx context.Context, username string) (User, error) {
	row := s.db.SQL().QueryRowContext(ctx, `
		SELECT id, username, display_name, role, disabled, version, created_at, updated_at
		FROM users WHERE username = ?`, NormalizeUsername(username))
	return scanUser(row)
}

// credentials 是校验密码时需要的最小内部结构，绝不外传。
type credentials struct {
	user User
	hash string
}

func (s *Service) credentialsByUsername(ctx context.Context, username string) (credentials, error) {
	var (
		c        credentials
		disabled int
		created  string
		updated  string
	)
	err := s.db.SQL().QueryRowContext(ctx, `
		SELECT id, username, display_name, password_hash, role, disabled, version, created_at, updated_at
		FROM users WHERE username = ?`, NormalizeUsername(username)).
		Scan(&c.user.ID, &c.user.Username, &c.user.DisplayName, &c.hash, &c.user.Role,
			&disabled, &c.user.Version, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return credentials{}, ErrUserNotFound
	}
	if err != nil {
		return credentials{}, fmt.Errorf("auth: 查询账号失败: %w", err)
	}
	c.user.Disabled = disabled != 0
	if c.user.CreatedAt, err = parseTime(created); err != nil {
		return credentials{}, err
	}
	if c.user.UpdatedAt, err = parseTime(updated); err != nil {
		return credentials{}, err
	}
	return c, nil
}

// updatePasswordHash 在登录成功后按需把哈希升级到当前参数。
func (s *Service) updatePasswordHash(ctx context.Context, userID int64, hash string) error {
	now := formatTime(s.nowUTC())
	_, err := s.db.SQL().ExecContext(ctx,
		`UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?`, hash, now, userID)
	if err != nil {
		return fmt.Errorf("auth: 更新密码哈希失败: %w", err)
	}
	return nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanUser(row rowScanner) (User, error) {
	var (
		u        User
		disabled int
		created  string
		updated  string
	)
	err := row.Scan(&u.ID, &u.Username, &u.DisplayName, &u.Role, &disabled,
		&u.Version, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrUserNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("auth: 读取账号失败: %w", err)
	}
	u.Disabled = disabled != 0
	if u.CreatedAt, err = parseTime(created); err != nil {
		return User{}, err
	}
	if u.UpdatedAt, err = parseTime(updated); err != nil {
		return User{}, err
	}
	return u, nil
}
