package api

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"upanel/internal/auth"
	"upanel/internal/httpx"
)

// 认证相关的固定名称（CONTRACTS.md 第 2 节）。
const (
	// SessionCookieName 是会话 Cookie 名。__Host- 前缀要求 Secure、Path=/、不带 Domain。
	SessionCookieName = "__Host-upanel_session"
	// CSRFTokenHeader 是非 GET/HEAD 请求携带 CSRF 令牌的头名。
	CSRFTokenHeader = "X-CSRF-Token"
	// RequestMarkerHeader 用来区分“前端 JSON 请求”与“跨站表单/图片请求”。
	RequestMarkerHeader = "X-UPanel-Request"
	// RequestMarkerValue 是该头的唯一合法值。
	RequestMarkerValue = "1"
)

// ctxKeySession 存放本次请求的已认证会话。
const ctxKeySession = "upanel.session"

// userDTO 是返回给前端的账号信息。不包含密码哈希、内部版本号等字段。
type userDTO struct {
	ID          int64  `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
}

func userOf(u auth.User) userDTO {
	return userDTO{ID: u.ID, Username: u.Username, DisplayName: u.DisplayName, Role: string(u.Role)}
}

// loginRequest 是 POST /auth/login 的请求体。
type loginRequest struct {
	Username string  `json:"username"`
	Password string  `json:"password"`
	TOTP     *string `json:"totp"`
}

// loginResponse 是成功登录的返回体。
type loginResponse struct {
	User      userDTO `json:"user"`
	CSRFToken string  `json:"csrf_token"`
	ExpiresAt string  `json:"expires_at"`
}

// sessionResponse 是 GET /auth/session 的返回体。
type sessionResponse struct {
	User            userDTO `json:"user"`
	CSRFToken       string  `json:"csrf_token"`
	ExpiresAt       string  `json:"expires_at"`
	ReauthExpiresAt *string `json:"reauth_expires_at"`
}

// handleLogin 处理 POST /api/v1/auth/login。
//
// 依次执行四道入口检查，任何一道不过都不进入密码校验：
// JSON Content-Type → X-UPanel-Request: 1 → Origin 精确匹配 → 严格解析请求体。
func (s *Server) handleLogin(c *gin.Context) {
	if !isJSONContentType(c.GetHeader("Content-Type")) {
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError, "登录请求必须使用 application/json")
		return
	}
	if c.GetHeader(RequestMarkerHeader) != RequestMarkerValue {
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError, "登录请求必须携带 "+RequestMarkerHeader+": "+RequestMarkerValue)
		return
	}
	origin := c.GetHeader("Origin")
	if origin == "" {
		writeError(c, http.StatusForbidden, httpx.CodeCSRFFailed, "登录请求必须携带 Origin")
		return
	}
	if !s.allowedOrigins.Contains(origin) {
		writeError(c, http.StatusForbidden, httpx.CodeCSRFFailed, "Origin 不在允许列表内")
		return
	}

	var req loginRequest
	dec := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, MaxRequestBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		if IsBodyTooLarge(err) {
			writeError(c, http.StatusRequestEntityTooLarge, httpx.CodeValidationError, "请求体超过大小上限")
			return
		}
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError, "请求体不是合法的登录 JSON")
		return
	}
	// 拒绝尾部多余内容，避免“只解析了第一个对象”的歧义。
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError, "请求体在首个 JSON 对象之后仍有内容")
		return
	}
	if req.TOTP != nil {
		// 二次验证在 P14 落地；在此之前不接受该字段，避免前端误以为已生效。
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError, "二次验证尚未启用，不能提交 totp")
		return
	}
	if strings.TrimSpace(req.Username) == "" || req.Password == "" {
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError, "用户名和密码不能为空")
		return
	}
	if len(req.Password) > auth.MaxPasswordLength {
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError, "密码长度超出上限")
		return
	}

	issued, err := s.opts.Auth.Login(c.Request.Context(), auth.LoginInput{
		Username:      req.Username,
		Password:      req.Password,
		RemoteAddr:    remoteIP(c.Request),
		PreviousToken: sessionToken(c.Request),
	})
	if err != nil {
		s.writeLoginError(c, err)
		return
	}

	setSessionCookie(c, issued.Token, issued.ExpiresAt)
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, loginResponse{
		User:      userOf(issued.User),
		CSRFToken: issued.CSRF,
		ExpiresAt: issued.ExpiresAt.UTC().Format(time.RFC3339),
	})
}

// writeLoginError 把认证错误映射成契约里的错误码。
func (s *Server) writeLoginError(c *gin.Context, err error) {
	var limited *auth.RateLimitedError
	switch {
	case errors.As(err, &limited):
		// 契约枚举里没有“限速”专用码（与 Q18 的 405 处理一致）：用 429 表达状态，
		// 配 VALIDATION_ERROR 承载可预期拒绝，并在 details 里给出重试秒数。
		httpx.WriteErrorDetails(c.Writer, c.Request, http.StatusTooManyRequests,
			httpx.CodeValidationError, "登录尝试过于频繁，请稍后再试",
			gin.H{"retry_after_seconds": int(limited.RetryAfter.Round(time.Second).Seconds())})
	case errors.Is(err, auth.ErrInvalidCredentials):
		writeError(c, http.StatusUnauthorized, httpx.CodeUnauthenticated, "用户名或密码不正确")
	case errors.Is(err, auth.ErrUserDisabled):
		writeError(c, http.StatusForbidden, httpx.CodeForbidden, "账号已被禁用")
	case errors.Is(err, auth.ErrNoUsers):
		writeError(c, http.StatusUnauthorized, httpx.CodeUnauthenticated, "尚未创建管理员账号")
	default:
		s.internalError(c, "登录处理失败", err)
	}
}

// handleSession 处理 GET /api/v1/auth/session（admin 与 viewer 都可读）。
//
// 每次调用都会轮换 CSRF 令牌并把明文返回给前端：服务端只保存哈希，因此页面刷新后
// 唯一能再次拿到令牌的途径就是这里（见 docs/DECISIONS.md Q25）。
func (s *Server) handleSession(c *gin.Context) {
	sess := sessionFrom(c)
	csrf, err := s.opts.Auth.RotateCSRF(c.Request.Context(), sess.ID)
	if err != nil {
		s.internalError(c, "轮换 CSRF 令牌失败", err)
		return
	}
	resp := sessionResponse{
		User:      userOf(sess.User),
		CSRFToken: csrf,
		ExpiresAt: sess.ExpiresAt.UTC().Format(time.RFC3339),
	}
	if !sess.ReauthUntil.IsZero() {
		v := sess.ReauthUntil.UTC().Format(time.RFC3339)
		resp.ReauthExpiresAt = &v
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, resp)
}

// handleLogout 处理 POST /api/v1/auth/logout：服务端删除会话并清除 Cookie。
func (s *Server) handleLogout(c *gin.Context) {
	if err := s.opts.Auth.Revoke(c.Request.Context(), sessionToken(c.Request)); err != nil {
		s.internalError(c, "退出登录失败", err)
		return
	}
	clearSessionCookie(c)
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, struct{}{})
}

// requireSession 校验会话 Cookie。通过后把会话放进上下文供 handler 使用。
func (s *Server) requireSession() gin.HandlerFunc {
	return func(c *gin.Context) {
		sess, err := s.opts.Auth.Authenticate(c.Request.Context(), sessionToken(c.Request))
		switch {
		case err == nil:
			c.Set(ctxKeySession, sess)
			c.Next()
		case errors.Is(err, auth.ErrSessionInvalid):
			writeError(c, http.StatusUnauthorized, httpx.CodeUnauthenticated, "未登录或会话已过期")
			c.Abort()
		case errors.Is(err, auth.ErrUserDisabled):
			writeError(c, http.StatusForbidden, httpx.CodeForbidden, "账号已被禁用")
			c.Abort()
		default:
			s.internalError(c, "会话校验失败", err)
			c.Abort()
		}
	}
}

// requireCSRF 校验非 GET/HEAD 请求的 CSRF 令牌是否与会话绑定。
//
// 必须在 requireSession 之后使用（依赖上下文里的会话）。
func (s *Server) requireCSRF() gin.HandlerFunc {
	return func(c *gin.Context) {
		sess := sessionFrom(c)
		if err := s.opts.Auth.VerifyCSRF(sess, c.GetHeader(CSRFTokenHeader)); err != nil {
			s.reject(c, "csrf", httpx.CodeCSRFFailed)
			writeError(c, http.StatusForbidden, httpx.CodeCSRFFailed, "CSRF 令牌缺失或不匹配")
			c.Abort()
			return
		}
		c.Next()
	}
}

// originGuard 对非 GET/HEAD 请求做 Origin 白名单检查。
//
// Cookie 上的 SameSite=Strict 与 CSRF 令牌已是主防线；这里做纵深防御：
// 带 Origin 的请求必须命中白名单。没有 Origin 的请求（本机脚本、CLI）不放行也不拒绝，
// 由后续 CSRF 校验决定——它们拿不到绑定会话的令牌。
func (s *Server) originGuard() gin.HandlerFunc {
	return func(c *gin.Context) {
		switch c.Request.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			c.Next()
			return
		}
		if origin := c.GetHeader("Origin"); origin != "" && !s.allowedOrigins.Contains(origin) {
			writeError(c, http.StatusForbidden, httpx.CodeCSRFFailed, "Origin 不在允许列表内")
			c.Abort()
			return
		}
		c.Next()
	}
}

// remoteIP 返回连接来源的 IP（去掉端口），用于按来源限速。
//
// 不解析 X-Forwarded-For：生产默认不信任代理头（CONTRACTS.md 第 2 节）。
func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func sessionToken(r *http.Request) string {
	cookie, err := r.Cookie(SessionCookieName)
	if err != nil {
		return ""
	}
	return cookie.Value
}

func setSessionCookie(c *gin.Context, token string, expires time.Time) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     SessionCookieName,
		Value:    token,
		Path:     "/",
		Expires:  expires.UTC(),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	})
}

func clearSessionCookie(c *gin.Context) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		Expires:  time.Unix(0, 0).UTC(),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	})
}

// sessionFrom 取出 requireSession 放进上下文的会话。
func sessionFrom(c *gin.Context) auth.Session {
	v, ok := c.Get(ctxKeySession)
	if !ok {
		// 只可能是中间件顺序写错：属于编程错误，不是用户输入问题。
		panic("api: 缺少会话上下文，requireSession 必须在 handler 之前")
	}
	sess, ok := v.(auth.Session)
	if !ok {
		panic("api: 会话上下文类型错误")
	}
	return sess
}

// internalError 记录真实错误并返回不泄漏细节的 500。
func (s *Server) internalError(c *gin.Context, msg string, err error) {
	s.opts.Logger.Error(msg, "error", err,
		"request_id", c.GetString(ctxKeyRequestID), "path", c.Request.URL.Path)
	writeError(c, http.StatusInternalServerError, httpx.CodeInternalError, "服务内部错误")
}

// isJSONContentType 判断 Content-Type 是否为 application/json（允许带 charset 参数）。
func isJSONContentType(v string) bool {
	if v == "" {
		return false
	}
	mediaType := strings.TrimSpace(strings.SplitN(v, ";", 2)[0])
	return strings.EqualFold(mediaType, "application/json")
}
