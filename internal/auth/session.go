package auth

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// 会话相关错误。
var (
	// ErrSessionInvalid 表示 Cookie 里没有可用会话（缺失、被改、过期、已退出）。
	ErrSessionInvalid = errors.New("auth: 会话无效或已过期")
	// ErrCSRF 表示 CSRF 令牌与会话不匹配。
	ErrCSRF = errors.New("auth: CSRF 令牌缺失或不匹配")
)

// RateLimitedError 表示登录尝试被限速拒绝。
type RateLimitedError struct {
	// RetryAfter 建议客户端等待多久再试。
	RetryAfter time.Duration
}

func (e *RateLimitedError) Error() string {
	return fmt.Sprintf("auth: 登录尝试过于频繁，请 %s 后再试", e.RetryAfter.Round(time.Second))
}

// Session 是一次已认证的会话；User 为当前账号。
//
// csrfHash 与 idHash 不导出：CSRF 令牌本身只在签发时返回一次，之后只做比对；
// 会话令牌的哈希只用于把预检/任务绑定到具体会话。
type Session struct {
	ID          int64
	User        User
	CreatedAt   time.Time
	LastSeenAt  time.Time
	ExpiresAt   time.Time
	ReauthUntil time.Time

	csrfHash string
	idHash   string
	// extended 记录本次校验是否把空闲期往后推了（每 touchInterval 至多一次）。
	// HTTP 层必须据此同步 Cookie 的 Expires，否则库里会话还在滑、浏览器里的
	// Cookie 按登录时的时间到期，长任务跑到一半就被踢出去。
	extended bool
}

// Extended 报告本次校验是否续期了会话。
func (s Session) Extended() bool { return s.extended }

// BindingHash 返回该会话令牌的 SHA-256 十六进制值。
//
// 用途：把预检与任务绑定到“这一个会话”。重新登录会轮换会话令牌，哈希随之改变，
// 旧预检因此自然失效（CONTRACTS.md 4.1：预检绑定用户与会话）。
// 返回值不是秘密材料：它是哈希，且只在服务端用于绑定比较。
func (s Session) BindingHash() string { return s.idHash }

// Issued 是成功登录的结果：明文令牌只在这里出现一次。
type Issued struct {
	Token     string
	CSRF      string
	ExpiresAt time.Time
	User      User
}

// LoginInput 是登录输入。RemoteAddr 用于按来源限速（不解析代理头）。
type LoginInput struct {
	Username string
	Password string
	// RemoteAddr 为连接来源（IP:port 或仅 IP）；为空时按固定键计数。
	RemoteAddr string
	// PreviousToken 是请求携带的旧会话令牌（如有）：登录成功后一并作废，完成会话轮换。
	PreviousToken string
}

// Login 校验密码并签发新会话。
//
// 顺序：限速检查 → 查账号 → 校验密码 → 需要时升级哈希 → 作废旧会话 → 签发新会话。
// 账号不存在与密码错误返回同一个错误，避免账号枚举。
//
// P14.6 起 HTTP 层不再直接调它：登录中间多了一步二次验证，所以拆成
// VerifyCredentials（只验口令）+ FinishLogin（验完之后签发）。这个方法保留给
// "不需要二次验证"的调用方——它现在的语义就是那两步依次执行。
func (s *Service) Login(ctx context.Context, in LoginInput) (Issued, error) {
	user, err := s.VerifyCredentials(ctx, in)
	if err != nil {
		return Issued{}, err
	}
	return s.FinishLogin(ctx, user, in.PreviousToken)
}

// VerifyCredentials 只校验口令，**不签发会话**。
//
// 抽出来的原因（P14.6）：口令对了还要过二次验证，而二次验证要问代理。把签发放在
// "口令 + 验证码都通过"之后，是"未通过的尝试不会得到任何会话"的唯一保证——先发一个
// 半路的会话再补校验，等于给了一条绕过路径。
//
// 失败计数在这里记，**成功计数不在这里清**：口令对但验证码连错十次同样必须能被限速，
// 所以清零留到 FinishLogin（或调用方确认二次验证也通过之后）。
func (s *Service) VerifyCredentials(ctx context.Context, in LoginInput) (User, error) {
	username := NormalizeUsername(in.Username)
	userKey := "user:" + username
	ipKey := "ip:" + in.RemoteAddr

	if retry, ok := s.limiter.Allow(userKey, ipKey); !ok {
		return User{}, &RateLimitedError{RetryAfter: retry}
	}

	creds, err := s.credentialsByUsername(ctx, username)
	if err != nil {
		if errors.Is(err, ErrUserNotFound) {
			// 与真实校验同量的计算，避免用响应时间区分“账号不存在”和“密码错误”。
			s.burnPasswordTime(in.Password)
			s.limiter.RecordFailure(userKey, ipKey)
			return User{}, ErrInvalidCredentials
		}
		return User{}, err
	}

	// 定时触发的保留账号不是一个人：它没有可用的口令，也不该在这里被"试着登录"。
	// 回答与"没有这个账号"完全一致（同样烧时间、同样计一次失败），
	// 所以它不会变成一个可以用来探测账号是否存在的信号。
	if isSystemAccount(creds.user.ID) {
		s.burnPasswordTime(in.Password)
		s.limiter.RecordFailure(userKey, ipKey)
		return User{}, ErrInvalidCredentials
	}

	ok, err := VerifyPassword(in.Password, creds.hash)
	if err != nil {
		// 哈希不可用属于数据损坏：不能当成“密码错误”，也不能静默放行。
		return User{}, fmt.Errorf("auth: 账号 %s 的密码哈希不可用: %w", username, err)
	}
	if !ok {
		s.limiter.RecordFailure(userKey, ipKey)
		return User{}, ErrInvalidCredentials
	}
	if creds.user.Disabled {
		s.limiter.RecordFailure(userKey, ipKey)
		return User{}, ErrUserDisabled
	}

	if NeedsRehash(creds.hash) {
		if newHash, err := HashPasswordWith(s.params, in.Password); err == nil {
			if err := s.updatePasswordHash(ctx, creds.user.ID, newHash); err != nil {
				// 升级失败不影响登录，但必须留下记录，不能静默吞掉。
				s.db.Logger().Warn("登录时升级密码哈希失败", "error", err, "user_id", creds.user.ID)
			}
		}
	}
	return creds.user, nil
}

// FinishLogin 在口令（以及需要时的二次验证）都通过后签发会话。
//
// 三件事：清掉失败计数、作废旧会话（会话轮换）、签发新会话。
func (s *Service) FinishLogin(ctx context.Context, user User, previousToken string) (Issued, error) {
	s.limiter.ResetSuccess("user:" + NormalizeUsername(user.Username))

	if previousToken != "" {
		if err := s.Revoke(ctx, previousToken); err != nil {
			return Issued{}, err
		}
	}
	return s.Issue(ctx, user)
}

// RecordLoginFailure 记一次登录失败并计入限速。
//
// 给"口令对、二次验证码错"用：那也是一次失败的登录尝试，不记就等于给了一条
// 不受限速约束的六位数猜法（口令已经知道的人可以按 Argon2 的速度一个个试）。
func (s *Service) RecordLoginFailure(username, remoteAddr string) {
	s.limiter.RecordFailure("user:"+NormalizeUsername(username), "ip:"+remoteAddr)
}

// ResetLoginSuccess 清掉某个账号的失败计数（口令与验证码都通过之后调用）。
func (s *Service) ResetLoginSuccess(username string) {
	s.limiter.ResetSuccess("user:" + NormalizeUsername(username))
}

// Issue 为账号签发新会话（登录、会话轮换使用）。
func (s *Service) Issue(ctx context.Context, user User) (Issued, error) {
	token, err := NewToken()
	if err != nil {
		return Issued{}, err
	}
	csrf, err := NewToken()
	if err != nil {
		return Issued{}, err
	}

	now := s.nowUTC()
	expires := now.Add(s.idle)
	_, err = s.db.SQL().ExecContext(ctx, `
		INSERT INTO sessions (id_hash, user_id, csrf_hash, created_at, last_seen_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		HashToken(token), user.ID, HashToken(csrf),
		formatTime(now), formatTime(now), formatTime(expires))
	if err != nil {
		return Issued{}, fmt.Errorf("auth: 创建会话失败: %w", err)
	}

	return Issued{Token: token, CSRF: csrf, ExpiresAt: expires, User: user}, nil
}

// Authenticate 校验会话令牌并返回会话。
//
// 已禁用的账号、超过空闲或绝对超时的会话一律视为无效，且顺手删除记录。
// 有效会话的 last_seen_at 与 expires_at 按 touchInterval 节流更新。
func (s *Service) Authenticate(ctx context.Context, token string) (Session, error) {
	if token == "" {
		return Session{}, ErrSessionInvalid
	}
	tokenHash := HashToken(token)
	var (
		sess          Session
		createdAt     string
		lastSeen      string
		expiresAt     string
		reauth        string
		disabled      int
		userCreatedAt string
		userUpdatedAt string
	)
	err := s.db.SQL().QueryRowContext(ctx, `
		SELECT s.id, s.csrf_hash, s.created_at, s.last_seen_at, s.expires_at, s.reauth_until,
		       u.id, u.username, u.display_name, u.role, u.disabled, u.version, u.created_at, u.updated_at
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.id_hash = ?`, tokenHash).
		Scan(&sess.ID, &sess.csrfHash, &createdAt, &lastSeen, &expiresAt, &reauth,
			&sess.User.ID, &sess.User.Username, &sess.User.DisplayName, &sess.User.Role,
			&disabled, &sess.User.Version, &userCreatedAt, &userUpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrSessionInvalid
	}
	if err != nil {
		return Session{}, fmt.Errorf("auth: 读取会话失败: %w", err)
	}
	sess.User.Disabled = disabled != 0
	sess.idHash = tokenHash

	if sess.CreatedAt, err = parseTime(createdAt); err != nil {
		return Session{}, err
	}
	if sess.LastSeenAt, err = parseTime(lastSeen); err != nil {
		return Session{}, err
	}
	if sess.ExpiresAt, err = parseTime(expiresAt); err != nil {
		return Session{}, err
	}
	if sess.ReauthUntil, err = parseTime(reauth); err != nil {
		return Session{}, err
	}
	if sess.User.CreatedAt, err = parseTime(userCreatedAt); err != nil {
		return Session{}, err
	}
	if sess.User.UpdatedAt, err = parseTime(userUpdatedAt); err != nil {
		return Session{}, err
	}

	now := s.nowUTC()
	absoluteDeadline := sess.CreatedAt.Add(s.absolute)
	if now.After(sess.ExpiresAt) || now.After(absoluteDeadline) {
		// 过期会话立即删除，不留在库里。
		_ = s.deleteByHash(ctx, HashToken(token))
		return Session{}, ErrSessionInvalid
	}
	if sess.User.Disabled {
		// 账号被禁用时，已有会话立即失效。
		_ = s.deleteByHash(ctx, HashToken(token))
		return Session{}, ErrUserDisabled
	}

	if now.Sub(sess.LastSeenAt) >= touchInterval {
		newExpiry := now.Add(s.idle)
		if absoluteDeadline.Before(newExpiry) {
			newExpiry = absoluteDeadline
		}
		if err := s.touch(ctx, sess.ID, now, newExpiry); err != nil {
			return Session{}, err
		}
		sess.LastSeenAt = now
		sess.ExpiresAt = newExpiry
		sess.extended = true
	}
	return sess, nil
}

// VerifyCSRF 校验请求头里的 CSRF 令牌是否属于该会话。
func (s *Service) VerifyCSRF(sess Session, token string) error {
	if token == "" || sess.csrfHash == "" {
		return ErrCSRF
	}
	got := HashToken(token)
	if subtle.ConstantTimeCompare([]byte(got), []byte(sess.csrfHash)) != 1 {
		return ErrCSRF
	}
	return nil
}

// RotateCSRF 为会话生成新的 CSRF 令牌，返回明文并只保存哈希。
//
// 触发时机：GET /auth/session（前端启动/刷新后重新取令牌）。
// 旧令牌随之失效，因此正在进行的请求可能是一次性 403——前端应在启动阶段
// 先取会话再发其他写请求。
func (s *Service) RotateCSRF(ctx context.Context, sessionID int64) (string, error) {
	token, err := NewToken()
	if err != nil {
		return "", err
	}
	res, err := s.db.SQL().ExecContext(ctx,
		`UPDATE sessions SET csrf_hash = ? WHERE id = ?`, HashToken(token), sessionID)
	if err != nil {
		return "", fmt.Errorf("auth: 轮换 CSRF 令牌失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return "", fmt.Errorf("auth: 轮换 CSRF 令牌失败: %w", err)
	}
	if n == 0 {
		return "", ErrSessionInvalid
	}
	return token, nil
}

// Revoke 作废一个会话；令牌不存在时视为成功（退出是幂等操作）。
func (s *Service) Revoke(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	return s.deleteByHash(ctx, HashToken(token))
}

// RevokeAllForUserExcept 作废某账号除 keepID 之外的全部会话（P14.6）。
//
// 用途：启用或关闭二次验证之后，其它设备上的会话必须重新登录。留着它们等于
// "改了鉴定强度，但旧鉴定继续有效"——一台已经被别人占着的浏览器不会因为你刚开了
// 二次验证就自己退出。发起这次改动的这一条会话保留：它刚刚当场证明过口令（和验证码）。
func (s *Service) RevokeAllForUserExcept(ctx context.Context, userID, keepID int64) (int64, error) {
	res, err := s.db.SQL().ExecContext(ctx,
		`DELETE FROM sessions WHERE user_id = ? AND id <> ?`, userID, keepID)
	if err != nil {
		return 0, fmt.Errorf("auth: 作废其它会话失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("auth: 读取作废结果失败: %w", err)
	}
	return n, nil
}

// RevokeAllForUser 作废某账号的全部会话（改密码、禁用、删除账号时使用）。
func (s *Service) RevokeAllForUser(ctx context.Context, userID int64) (int64, error) {
	res, err := s.db.SQL().ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID)
	if err != nil {
		return 0, fmt.Errorf("auth: 作废账号会话失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil
	}
	return n, nil
}

// PurgeExpired 删除已过期的会话，返回删除条数。启动时与定期清理调用。
//
// 绝对超时由 created_at 判定，因此这里同时按两个条件清理。
func (s *Service) PurgeExpired(ctx context.Context) (int64, error) {
	now := s.nowUTC()
	res, err := s.db.SQL().ExecContext(ctx, `
		DELETE FROM sessions WHERE expires_at <= ? OR created_at <= ?`,
		formatTime(now), formatTime(now.Add(-s.absolute)))
	if err != nil {
		return 0, fmt.Errorf("auth: 清理过期会话失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil
	}
	return n, nil
}

func (s *Service) deleteByHash(ctx context.Context, hash string) error {
	if _, err := s.db.SQL().ExecContext(ctx, `DELETE FROM sessions WHERE id_hash = ?`, hash); err != nil {
		return fmt.Errorf("auth: 删除会话失败: %w", err)
	}
	return nil
}

func (s *Service) touch(ctx context.Context, sessionID int64, seen time.Time, expires time.Time) error {
	_, err := s.db.SQL().ExecContext(ctx,
		`UPDATE sessions SET last_seen_at = ?, expires_at = ? WHERE id = ?`,
		formatTime(seen), formatTime(expires), sessionID)
	if err != nil {
		return fmt.Errorf("auth: 更新会话活跃时间失败: %w", err)
	}
	return nil
}

// burnPasswordTime 对不存在的账号做一次等价开销的哈希校验，削弱账号枚举的时间差。
func (s *Service) burnPasswordTime(password string) {
	hash, err := HashPasswordWith(s.params, "upanel-dummy-password-for-timing")
	if err != nil {
		return
	}
	_, _ = VerifyPassword(password, hash)
}
