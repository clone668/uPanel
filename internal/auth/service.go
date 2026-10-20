package auth

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"upanel/internal/store"
)

// 会话策略（CONTRACTS.md 第 2 节）：默认 30 分钟空闲超时、12 小时绝对超时。
const (
	DefaultIdleTimeout     = 30 * time.Minute
	DefaultAbsoluteTimeout = 12 * time.Hour
	// ReauthWindow 是危险操作再认证的有效期（PLAN.md 第 5 节：最近五分钟）。
	// P02.1 只建列，判定逻辑随 P02.2 的再认证接口一起实现。
	ReauthWindow = 5 * time.Minute
	// touchInterval 控制滑动超时的写库频率：同一个会话每分钟最多写一次，
	// 避免每个请求都产生一次写事务。
	touchInterval = time.Minute
)

// Options 是 Service 的构造参数。
type Options struct {
	// Now 注入时钟，测试用；为 nil 时用 time.Now。
	Now func() time.Time
	// IdleTimeout 为滑动空闲超时；0 表示使用默认 30 分钟。
	IdleTimeout time.Duration
	// AbsoluteTimeout 为绝对超时；0 表示使用默认 12 小时。
	AbsoluteTimeout time.Duration
	// PasswordParams 为零值时使用 DefaultPasswordParams。
	PasswordParams PasswordParams
	// Limiter 为登录限速器；nil 时使用 DefaultLimits。
	Limiter *Limiter
}

// Service 提供账号与会话操作。它是唯一的密码与会话入口，HTTP 层不得直接碰表。
type Service struct {
	db       *store.DB
	now      func() time.Time
	idle     time.Duration
	absolute time.Duration
	params   PasswordParams
	limiter  *Limiter
}

// NewService 构造 Service。
func NewService(db *store.DB, opts Options) (*Service, error) {
	if db == nil {
		return nil, errors.New("auth: 数据库句柄不能为空")
	}
	s := &Service{
		db:       db,
		now:      opts.Now,
		idle:     opts.IdleTimeout,
		absolute: opts.AbsoluteTimeout,
		params:   opts.PasswordParams,
		limiter:  opts.Limiter,
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.idle <= 0 {
		s.idle = DefaultIdleTimeout
	}
	if s.absolute <= s.idle {
		s.absolute = DefaultAbsoluteTimeout
	}
	if s.params.Memory == 0 {
		s.params = DefaultPasswordParams
	}
	if s.limiter == nil {
		s.limiter = NewLimiter(DefaultLimits(), s.now)
	}
	return s, nil
}

// Limiter 返回登录限速器。
func (s *Service) Limiter() *Limiter { return s.limiter }

// PasswordParams 返回当前使用的哈希参数。
func (s *Service) PasswordParams() PasswordParams { return s.params }

// nowUTC 返回秒级截断的 UTC 时间：数据库里存 RFC3339 文本，截断避免亚秒精度丢失
// 导致“刚创建就比数据库里的值新”这类比较错误。
func (s *Service) nowUTC() time.Time { return s.now().UTC().Truncate(time.Second) }

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func parseTime(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("auth: 时间格式无效: %w", err)
	}
	return t.UTC(), nil
}

// isUniqueViolation 判断错误是否来自 UNIQUE 约束。
//
// modernc.org/sqlite 直接返回 SQLite 的错误文本，没有可用的错误码常量，
// 因此这里只做文本匹配，并在调用处再检查一次语义（例如用户名已存在）。
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}
