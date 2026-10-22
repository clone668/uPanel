package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"upanel/internal/audit"
	"upanel/internal/auth"
	"upanel/internal/httpx"
	"upanel/internal/terminal"
)

// adminUserDTO 是账号管理接口返回的账号信息（CONTRACTS.md 第 2 节“字段级脱敏 DTO”）。
//
// 脱敏边界：只返回管理与展示需要的字段。password_hash、会话、CSRF、再认证时间等
// 一律不出现在响应里；本结构体之外不要随手加字段。
type adminUserDTO struct {
	ID          int64  `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
	Disabled    bool   `json:"disabled"`
	Version     int    `json:"version"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

func adminUserOf(u auth.User) adminUserDTO {
	return adminUserDTO{
		ID:          u.ID,
		Username:    u.Username,
		DisplayName: u.DisplayName,
		Role:        string(u.Role),
		Disabled:    u.Disabled,
		Version:     u.Version,
		CreatedAt:   u.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:   u.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

// usersListResponse 是 GET /users 的返回体。游标为空时 next_cursor 为 null。
type usersListResponse struct {
	Items      []adminUserDTO `json:"items"`
	NextCursor *string        `json:"next_cursor"`
}

// createUserRequest 是 POST /users 的请求体。
type createUserRequest struct {
	Username        string `json:"username"`
	DisplayName     string `json:"display_name"`
	Role            string `json:"role"`
	InitialPassword string `json:"initial_password"`
}

// updateUserRequest 是 PATCH /users/:id 的请求体：nil 字段表示“不改”。
type updateUserRequest struct {
	Version     int     `json:"version"`
	DisplayName *string `json:"display_name"`
	Role        *string `json:"role"`
	Disabled    *bool   `json:"disabled"`
}

// setPasswordRequest 是 POST /users/:id/password 的请求体。
type setPasswordRequest struct {
	NewPassword string `json:"new_password"`
}

// deleteUserRequest 是 DELETE /users/:id 的可选请求体。
type deleteUserRequest struct {
	Version *int `json:"version"`
}

// reauthRequest 是 POST /auth/reauth 的请求体。
type reauthRequest struct {
	Password string  `json:"password"`
	TOTP     *string `json:"totp"`
}

// reauthResponse 是 POST /auth/reauth 的返回体。
type reauthResponse struct {
	ReauthExpiresAt string `json:"reauth_expires_at"`
}

// handleListUsers 处理 GET /api/v1/users（仅 admin）。
//
// 支持 limit/cursor：默认 50 条、最多 200 条（CONTRACTS.md 第 1 节）。响应始终带
// next_cursor 字段，没有下一页时为 null。
func (s *Server) handleListUsers(c *gin.Context) {
	limit, ok := listLimit(c)
	if !ok {
		return
	}

	users, next, err := s.opts.Auth.ListUsers(c.Request.Context(), limit, c.Query("cursor"))
	if err != nil {
		s.writeUserError(c, err)
		return
	}

	items := make([]adminUserDTO, 0, len(users))
	for _, u := range users {
		items = append(items, adminUserOf(u))
	}
	resp := usersListResponse{Items: items}
	if next != "" {
		resp.NextCursor = &next
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, resp)
}

// listLimit 解析 limit 查询参数；空值交给服务层取默认 50。
func listLimit(c *gin.Context) (int, bool) {
	return listLimitMax(c, auth.MaxUserListLimit)
}

// listLimitMax 是同名解析但上限由调用方提供。
//
// 各资源的上限在自己的包里定（apps.MaxListLimit 等），不共用一个常量：共用会让
// 账号列表的上限悄悄变成应用列表的上限，改一个地方坏两处。
func listLimitMax(c *gin.Context, max int) (int, bool) {
	raw := strings.TrimSpace(c.Query("limit"))
	if raw == "" {
		return 0, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > max {
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError,
			fmt.Sprintf("limit 必须是 1 到 %d 之间的整数", max))
		return 0, false
	}
	return n, true
}

// handleCreateUser 处理 POST /api/v1/users（admin）。
//
// 新建账号的初始密码只出现在请求体里：不回显、不入日志、不写审计。
func (s *Server) handleCreateUser(c *gin.Context) {
	sess := sessionFrom(c)
	const action = audit.ActionUserCreate

	var req createUserRequest
	if !decodeJSONBody(c, &req) {
		s.recordDenied(c, &sess, action, "user", "validation", httpx.CodeValidationError)
		return
	}

	var user auth.User
	err := s.opts.Auth.InTransaction(c.Request.Context(), func(ctx context.Context) error {
		u, err := s.opts.Auth.CreateUser(ctx, auth.CreateUserInput{
			Username:    req.Username,
			DisplayName: req.DisplayName,
			Password:    req.InitialPassword,
			Role:        auth.Role(req.Role),
		})
		if err != nil {
			return &errDenied{err}
		}
		if err := s.recordAuditInTx(ctx, c, sess, action, fmt.Sprintf("user:%d", u.ID), map[string]any{
			"username":     u.Username,
			"role":         string(u.Role),
			"display_name": u.DisplayName,
		}); err != nil {
			return err
		}
		user = u
		return nil
	})
	if err != nil {
		s.failUserAction(c, &sess, action, "user", err)
		return
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusCreated, adminUserOf(user))
}

// handleUpdateUser 处理 PATCH /api/v1/users/:id（admin + 再认证）。
func (s *Server) handleUpdateUser(c *gin.Context) {
	sess := sessionFrom(c)
	id, ok := pathID(c)
	if !ok {
		s.recordDenied(c, &sess, audit.ActionUserUpdate, "user", "validation", httpx.CodeNotFound)
		return
	}
	target := fmt.Sprintf("user:%d", id)

	var req updateUserRequest
	if !decodeJSONBody(c, &req) {
		s.recordDenied(c, &sess, audit.ActionUserUpdate, target, "validation", httpx.CodeValidationError)
		return
	}
	if req.DisplayName == nil && req.Role == nil && req.Disabled == nil {
		s.recordDenied(c, &sess, audit.ActionUserUpdate, target, "validation", httpx.CodeValidationError)
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError, "至少需要提供一个要修改的字段")
		return
	}

	in := auth.UpdateUserInput{
		ActorID:     sess.User.ID,
		UserID:      id,
		Version:     req.Version,
		DisplayName: req.DisplayName,
		Disabled:    req.Disabled,
	}
	changed := map[string]any{}
	if req.DisplayName != nil {
		changed["display_name"] = *req.DisplayName
	}
	if req.Role != nil {
		role := auth.Role(*req.Role)
		in.Role = &role
		changed["role"] = *req.Role
	}
	if req.Disabled != nil {
		changed["disabled"] = *req.Disabled
	}

	var user auth.User
	err := s.opts.Auth.InTransaction(c.Request.Context(), func(ctx context.Context) error {
		u, err := s.opts.Auth.UpdateUser(ctx, in)
		if err != nil {
			return &errDenied{err}
		}
		changed["version"] = u.Version
		if err := s.recordAuditInTx(ctx, c, sess, audit.ActionUserUpdate, target, changed); err != nil {
			return err
		}
		user = u
		return nil
	})
	if err != nil {
		s.failUserAction(c, &sess, audit.ActionUserUpdate, target, err)
		return
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, adminUserOf(user))
}

// handleSetUserPassword 处理 POST /api/v1/users/:id/password（admin + 再认证）。
//
// 成功后目标账号的全部会话立即失效；如果目标就是自己，当前会话也随之失效，
// 调用方必须重新登录（这是“改密码即踢下线”的一致行为）。
func (s *Server) handleSetUserPassword(c *gin.Context) {
	sess := sessionFrom(c)
	const action = audit.ActionUserPasswordReset
	id, ok := pathID(c)
	if !ok {
		s.recordDenied(c, &sess, action, "user", "validation", httpx.CodeNotFound)
		return
	}
	target := fmt.Sprintf("user:%d", id)

	var req setPasswordRequest
	if !decodeJSONBody(c, &req) {
		s.recordDenied(c, &sess, action, target, "validation", httpx.CodeValidationError)
		return
	}

	var user auth.User
	err := s.opts.Auth.InTransaction(c.Request.Context(), func(ctx context.Context) error {
		u, err := s.opts.Auth.SetPassword(ctx, id, req.NewPassword)
		if err != nil {
			return &errDenied{err}
		}
		// 口令本身不进审计：details 里只有账号标识与会话作废结果。
		if err := s.recordAuditInTx(ctx, c, sess, action, target, map[string]any{
			"username":          u.Username,
			"version":           u.Version,
			"sessions_revoked":  true,
			"target_is_actor":   u.ID == sess.User.ID,
			"actor_session_end": u.ID == sess.User.ID,
		}); err != nil {
			return err
		}
		user = u
		return nil
	})
	if err != nil {
		s.failUserAction(c, &sess, action, target, err)
		return
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, adminUserOf(user))
}

// handleDeleteUser 处理 DELETE /api/v1/users/:id（admin + 再认证）。
//
// version 优先从请求体取，其次从查询参数取（DELETE 带请求体不是所有客户端都支持）。
func (s *Server) handleDeleteUser(c *gin.Context) {
	sess := sessionFrom(c)
	const action = audit.ActionUserDelete
	id, ok := pathID(c)
	if !ok {
		s.recordDenied(c, &sess, action, "user", "validation", httpx.CodeNotFound)
		return
	}
	target := fmt.Sprintf("user:%d", id)

	version, ok := deleteVersion(c)
	if !ok {
		s.recordDenied(c, &sess, action, target, "validation", httpx.CodeValidationError)
		return
	}

	err := s.opts.Auth.InTransaction(c.Request.Context(), func(ctx context.Context) error {
		if err := s.opts.Auth.DeleteUser(ctx, auth.DeleteUserInput{
			ActorID: sess.User.ID,
			UserID:  id,
			Version: version,
		}); err != nil {
			return &errDenied{err}
		}
		return s.recordAuditInTx(ctx, c, sess, action, target, map[string]any{"version": version})
	})
	if err != nil {
		s.failUserAction(c, &sess, action, target, err)
		return
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, struct{}{})
}

// handleReauth 处理 POST /api/v1/auth/reauth（admin + CSRF）。
//
// 密码正确则把会话的再认证有效期推到 5 分钟后；失败与登录共用限速计数。
func (s *Server) handleReauth(c *gin.Context) {
	sess := sessionFrom(c)
	const action = audit.ActionAuthReauth
	target := fmt.Sprintf("session:%d", sess.ID)

	var req reauthRequest
	if !decodeJSONBody(c, &req) {
		s.recordDenied(c, &sess, action, target, "validation", httpx.CodeValidationError)
		return
	}
	if req.TOTP != nil {
		s.recordDenied(c, &sess, action, target, "validation", httpx.CodeValidationError)
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError, "二次验证尚未启用，不能提交 totp")
		return
	}
	if req.Password == "" {
		s.recordDenied(c, &sess, action, target, "validation", httpx.CodeValidationError)
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError, "密码不能为空")
		return
	}

	var until time.Time
	err := s.opts.Auth.InTransaction(c.Request.Context(), func(ctx context.Context) error {
		u, err := s.opts.Auth.Reauthenticate(ctx, sess, req.Password, remoteIP(c.Request))
		if err != nil {
			return &errDenied{err}
		}
		// 口令不进审计：只记窗口到期时间。
		if err := s.recordAuditInTx(ctx, c, sess, action, target, map[string]any{
			"reauth_expires_at": u.UTC().Format(time.RFC3339),
		}); err != nil {
			return err
		}
		until = u
		return nil
	})
	if err != nil {
		s.failReauth(c, &sess, action, target, err)
		return
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, reauthResponse{
		ReauthExpiresAt: until.UTC().Format(time.RFC3339),
	})
}

// failReauth 处理再认证失败：记一条 failure 审计，再按原有映射写响应。
func (s *Server) failReauth(c *gin.Context, sess *auth.Session, action, target string, err error) {
	if errors.Is(err, errAuditWrite) {
		s.opts.Logger.Error("审计写入失败，再认证未生效", "error", err,
			"request_id", c.GetString(ctxKeyRequestID))
		writeError(c, http.StatusInternalServerError, httpx.CodeInternalError, "服务器内部错误")
		return
	}
	businessErr := err
	var denied *errDenied
	if errors.As(err, &denied) {
		businessErr = denied.err
	}
	switch {
	case errors.As(businessErr, new(*auth.RateLimitedError)):
		s.recordDenied(c, sess, action, target, "rate_limited", httpx.CodeValidationError)
	case errors.Is(businessErr, auth.ErrInvalidCredentials):
		s.recordDenied(c, sess, action, target, "wrong_password", httpx.CodeForbidden)
	case errors.Is(businessErr, auth.ErrUserDisabled):
		s.recordDenied(c, sess, action, target, "user_disabled", httpx.CodeForbidden)
	default:
		s.recordDenied(c, sess, action, target, "internal", httpx.CodeInternalError)
	}
	s.writeReauthError(c, businessErr)
}

// writeReauthError 映射再认证错误。
func (s *Server) writeReauthError(c *gin.Context, err error) {
	var limited *auth.RateLimitedError
	switch {
	case errors.As(err, &limited):
		httpx.WriteErrorDetails(c.Writer, c.Request, http.StatusTooManyRequests,
			httpx.CodeValidationError, "尝试过于频繁，请稍后再试",
			gin.H{"retry_after_seconds": int(limited.RetryAfter.Round(time.Second).Seconds())})
	case errors.Is(err, auth.ErrInvalidCredentials):
		// 会话本身仍然有效，只是这次没有拿到再认证授权，因此是 403 而不是 401。
		writeError(c, http.StatusForbidden, httpx.CodeForbidden, "密码不正确，再认证未通过")
	case errors.Is(err, auth.ErrUserDisabled):
		writeError(c, http.StatusForbidden, httpx.CodeForbidden, "账号已被禁用")
	default:
		s.internalError(c, "再认证失败", err)
	}
}

// writeUserError 把账号管理错误映射成契约错误码。
func (s *Server) writeUserError(c *gin.Context, err error) {
	status, code, msg, internal := mapUserError(err)
	if internal {
		s.internalError(c, "账号管理操作失败", err)
		return
	}
	writeError(c, status, code, msg)
}

// mapUserError 把服务层错误映射成 HTTP 状态码与契约错误码。
// internal 为 true 表示不是业务错误，调用方按 500 INTERNAL_ERROR 处理并只记日志。
func mapUserError(err error) (status int, code httpx.Code, message string, internal bool) {
	var input *auth.InputError
	switch {
	case errors.As(err, &input):
		return http.StatusBadRequest, httpx.CodeValidationError, input.Error(), false
	case errors.Is(err, auth.ErrInvalidLimit), errors.Is(err, auth.ErrInvalidCursor):
		return http.StatusBadRequest, httpx.CodeValidationError, err.Error(), false
	case errors.Is(err, auth.ErrUserNotFound):
		return http.StatusNotFound, httpx.CodeNotFound, "账号不存在", false
	case errors.Is(err, auth.ErrUsernameTaken):
		return http.StatusUnprocessableEntity, httpx.CodeValidationError, "用户名已被占用", false
	case errors.Is(err, auth.ErrVersionConflict):
		return http.StatusConflict, httpx.CodeVersionConflict, "账号已被其他人修改，请刷新后重试", false
	case errors.Is(err, auth.ErrLastAdmin):
		return http.StatusUnprocessableEntity, httpx.CodeValidationError, "不能移除最后一个可用管理员", false
	case errors.Is(err, auth.ErrSelfModification):
		return http.StatusForbidden, httpx.CodeForbidden, "不能修改自己的角色、启用状态或删除自己", false
	default:
		return http.StatusInternalServerError, httpx.CodeInternalError, "服务器内部错误", true
	}
}

// errAuditWrite 表示审计写入失败：业务变更必须随之回滚（P02.4）。
var errAuditWrite = errors.New("审计写入失败")

// errDenied 表示事务内被业务拒绝：业务没有变更，但要按“被拒绝”记审计。
type errDenied struct{ err error }

func (e *errDenied) Error() string { return e.err.Error() }
func (e *errDenied) Unwrap() error { return e.err }

// auditEvent 构造一条与当前请求关联的审计事件。
func (s *Server) auditEvent(c *gin.Context, sess auth.Session, action, target, outcome string,
	details map[string]any) audit.Event {
	return audit.Event{
		ActorID:       sess.User.ID,
		ActorSnapshot: fmt.Sprintf("%s(%s)#%d", sess.User.Username, sess.User.Role, sess.User.ID),
		Action:        action,
		Target:        target,
		JobID:         audit.JobIDFromTarget(target),
		RequestID:     c.GetString(ctxKeyRequestID),
		Outcome:       outcome,
		Details:       details,
	}
}

// recordAuditInTx 在**当前事务**里写一条成功审计。
//
// 返回错误即表示审计没写成：调用方必须让事务回滚（业务不生效、审计也不存在），
// 不能出现“改了库但没有审计”的状态。
func (s *Server) recordAuditInTx(ctx context.Context, c *gin.Context, sess auth.Session,
	action, target string, details map[string]any) error {
	if s.opts.Audit == nil {
		// 审计未接入属于部署错误：拒绝执行写操作，而不是悄悄不留痕。
		return fmt.Errorf("%w: 审计组件未初始化", errAuditWrite)
	}
	if err := s.opts.Audit.Record(ctx, s.auditEvent(c, sess, action, target, audit.OutcomeSuccess, details)); err != nil {
		return fmt.Errorf("%w: %v", errAuditWrite, err)
	}
	return nil
}

// recordDenied 记录被拒绝或失败的请求（outcome=failure）。
//
// 这类事件**没有**业务事务可依附（事务已回滚或从未开始），因此独立提交；
// 写失败只记 ERROR 日志，不改变已经发给客户端的响应——但也不会静默吞掉。
func (s *Server) recordDenied(c *gin.Context, sess *auth.Session, action, target, stage string, code httpx.Code) {
	if s.opts.Audit == nil {
		return
	}
	ev := audit.Event{
		Action:    action,
		Target:    target,
		JobID:     audit.JobIDFromTarget(target),
		RequestID: c.GetString(ctxKeyRequestID),
		Outcome:   audit.OutcomeFailure,
		Details: map[string]any{
			"stage":       stage,
			"error_code":  string(code),
			"http_method": c.Request.Method,
			"http_path":   c.Request.URL.Path,
		},
	}
	if sess != nil {
		ev.ActorID = sess.User.ID
		ev.ActorSnapshot = fmt.Sprintf("%s(%s)#%d", sess.User.Username, sess.User.Role, sess.User.ID)
	}
	if err := s.opts.Audit.Record(c.Request.Context(), ev); err != nil {
		s.opts.Logger.Error("审计写入失败（拒绝记录缺失）", "error", err,
			"action", action, "stage", stage, "request_id", c.GetString(ctxKeyRequestID))
	}
}

// failUserAction 处理“事务内失败”的响应与审计。
//
// 三种情形：审计没写成（业务已回滚 -> 500，且必须打 ERROR）；业务拒绝（记一条 failure 审计
// 再返回对应错误码）；内部错误（记 failure 审计后走 500，响应不含内部信息）。
func (s *Server) failUserAction(c *gin.Context, sess *auth.Session, action, target string, err error) {
	if errors.Is(err, errAuditWrite) {
		s.opts.Logger.Error("审计写入失败，业务变更已回滚", "error", err, "action", action,
			"request_id", c.GetString(ctxKeyRequestID))
		writeError(c, http.StatusInternalServerError, httpx.CodeInternalError, "服务器内部错误")
		return
	}
	businessErr := err
	var denied *errDenied
	if errors.As(err, &denied) {
		businessErr = denied.err
	}
	status, code, message, internal := mapUserError(businessErr)
	if internal {
		s.recordDenied(c, sess, action, target, "internal", code)
		s.internalError(c, "账号管理操作失败", businessErr)
		return
	}
	s.recordDenied(c, sess, action, target, "business", code)
	writeError(c, status, code, message)
}

// auditSession 取会话，取不到时返回 nil（拒绝可能发生在任何中间件之前）。
func auditSession(c *gin.Context) *auth.Session {
	v, ok := c.Get(ctxKeySession)
	if !ok {
		return nil
	}
	sess, ok := v.(auth.Session)
	if !ok {
		return nil
	}
	return &sess
}

// auditRoute 给一组路由打上审计动作名，供中间件拒绝时记录事件。
func (s *Server) auditRoute(action string) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set(ctxKeyAuditAction, action)
		c.Next()
	}
}

// auditActionFrom 取本路由组的审计动作名；没有登记则返回 ""（不记录）。
func auditActionFrom(c *gin.Context) string {
	v, ok := c.Get(ctxKeyAuditAction)
	if !ok {
		return ""
	}
	action, _ := v.(string)
	return action
}

// reject 记录一条被中间件拒绝的审计（仅当该路由登记了审计动作）。
func (s *Server) reject(c *gin.Context, stage string, code httpx.Code) {
	action := auditActionFrom(c)
	if action == "" {
		return
	}
	s.recordDenied(c, auditSession(c), action, denyTarget(c, action), stage, code)
}

// denyTarget 给出“中间件拒绝”这条审计指向的对象。
//
// 终端路由的 :id 就是会话 id，指向 session 才说得清“谁想连哪个会话”；写成固定的
// "user" 会让一条 terminal.attach 的越权尝试看起来像在操作某个用户，事后无法追到
// 具体会话。秘密上传没有 :id（用途在请求体里，中间件拒绝时还没解析），指向 "secret"
// 比指向一个凭空的 "user" 准确。应用与分组同理：路径里的 :id 才是被操作的对象。
// 其余路由保持原有的 "user"（它们的 :id 是用户 id）。
func denyTarget(c *gin.Context, action string) string {
	switch action {
	case audit.ActionTerminalAttach, audit.ActionTerminalClose:
		if id := c.Param("id"); terminal.IsValidSessionID(id) {
			return "terminal:" + id
		}
	case audit.ActionSecretCreate:
		return "secret"
	case audit.ActionAppManage, audit.ActionAppCreate, audit.ActionAppUpdate, audit.ActionAppDelete:
		if id := c.Param("id"); id != "" {
			return "app:" + id
		}
		return "app"
	case audit.ActionAppGroupManage, audit.ActionAppGroupCreate,
		audit.ActionAppGroupUpdate, audit.ActionAppGroupDelete:
		if id := c.Param("id"); id != "" {
			return "app_group:" + id
		}
		return "app_group"
	}
	return "user"
}

// requireAdmin 只允许 admin 通过；viewer 得到 403 FORBIDDEN。
func (s *Server) requireAdmin() gin.HandlerFunc {
	return func(c *gin.Context) {
		if sessionFrom(c).User.Role != auth.RoleAdmin {
			s.reject(c, "admin", httpx.CodeForbidden)
			writeError(c, http.StatusForbidden, httpx.CodeForbidden, "需要管理员权限")
			c.Abort()
			return
		}
		c.Next()
	}
}

// requireReauth 要求会话在 5 分钟窗口内完成过密码再认证。
func (s *Server) requireReauth() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !s.opts.Auth.ReauthValid(sessionFrom(c)) {
			s.reject(c, "reauth", httpx.CodeReauthRequired)
			writeError(c, http.StatusForbidden, httpx.CodeReauthRequired, "该操作需要先完成密码再认证")
			c.Abort()
			return
		}
		c.Next()
	}
}

// pathID 解析 :id 路径参数。非正整数一律按“资源不存在”处理，不泄漏内部细节。
func pathID(c *gin.Context) (int64, bool) {
	raw := c.Param("id")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		writeError(c, http.StatusNotFound, httpx.CodeNotFound, "资源不存在")
		return 0, false
	}
	return id, true
}

// deleteVersion 读取 DELETE 请求的 version：请求体优先，其次查询参数。
func deleteVersion(c *gin.Context) (int, bool) {
	if c.Request.ContentLength > 0 {
		var req deleteUserRequest
		if !decodeJSONBody(c, &req) {
			return 0, false
		}
		if req.Version != nil {
			if *req.Version <= 0 {
				writeError(c, http.StatusBadRequest, httpx.CodeValidationError, "version 必须是正整数")
				return 0, false
			}
			return *req.Version, true
		}
	}
	if raw := c.Query("version"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v <= 0 {
			writeError(c, http.StatusBadRequest, httpx.CodeValidationError, "version 必须是正整数")
			return 0, false
		}
		return v, true
	}
	writeError(c, http.StatusBadRequest, httpx.CodeValidationError, "缺少 version")
	return 0, false
}

// decodeJSONBody 严格解析 JSON 请求体：拒绝未知字段与尾部多余内容。
// 失败时已经写回错误响应，调用方直接返回即可。
func decodeJSONBody(c *gin.Context, dst any) bool {
	return decodeJSONBodyLimit(c, dst, MaxRequestBodyBytes)
}

// decodeJSONBodyLimit 是带自定义上限的 decodeJSONBody。
//
// 只有预检用它，而且必须是放宽：file.write_text 的参数里带着最多 1 MiB 文本
// （CONTRACTS.md 第 567 行）。其余路由继续用 1 MiB，不在首页面上悄悄变大。
func decodeJSONBodyLimit(c *gin.Context, dst any, limit int64) bool {
	dec := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, limit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		if IsBodyTooLarge(err) {
			writeError(c, http.StatusRequestEntityTooLarge, httpx.CodeValidationError, "请求体超过大小上限")
			return false
		}
		msg := "请求体不是合法的 JSON"
		if errors.Is(err, io.EOF) {
			msg = "请求体不能为空"
		} else if strings.Contains(err.Error(), "unknown field") {
			msg = "请求体包含未知字段"
		}
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError, msg)
		return false
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError, "请求体在首个 JSON 对象之后仍有内容")
		return false
	}
	return true
}
