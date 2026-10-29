package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"upanel/internal/agentclient"
	"upanel/internal/audit"
	"upanel/internal/auth"
	"upanel/internal/httpx"
	"upanel/internal/totp"
)

// 二次验证（P14.6）：/auth/totp/setup、/auth/totp/confirm、DELETE /auth/totp。
//
// 分工是硬的：面板只管"这个账号开没开、上一步是哪一步、恢复码还剩几枚"（internal/auth
// 的 user_totp），种子与算码一律在代理里（/internal/v1/totp/*）。因此这一层从头到尾
// 看不到种子，只搬运三样东西：引用、六位数、时间步。
//
// 三条入口共同的失败原则：
//   - 代理不可用时**失败关闭**（503），绝不放行。一个"代理挂了所以先让他进去"的分支，
//     等于给攻击者一个"把代理弄挂"的绕过方式。
//   - 码不对是正常结果（401 + 清楚的话），不是 500，也不写出"这个码存在/不存在"的差别。
//   - 每一次通过都要落一个时间步（user_totp.last_step），同一个 30 秒窗口里的同一个码
//     用第二次会被拒——通过一次不等于通过一辈子。

// TotpAgent 是二次验证要问代理的那几件事。生产实现是 *agentclient.Client。
type TotpAgent interface {
	TotpEnroll(ctx context.Context, userID int64, username string) (agentclient.TotpEnrollment, error)
	TotpConfirm(ctx context.Context, userID int64, secretRef, code string) (agentclient.TotpCheck, error)
	TotpVerify(ctx context.Context, userID int64, secretRef, code string) (agentclient.TotpCheck, error)
	TotpDisable(ctx context.Context, userID int64, secretRef string) (bool, error)
}

// totpSetupResponse 是 POST /auth/totp/setup 的返回。
//
// enrollment_id 就是代理给的 secret_ref：它只是"那条种子存在哪"的指针，不是秘密材料。
// 但 otpauth_uri 里带着种子，前端只许把它画成二维码，不许留在本地存储里。
type totpSetupResponse struct {
	EnrollmentID string `json:"enrollment_id"`
	OTPAuthURI   string `json:"otpauth_uri"`
	Issuer       string `json:"issuer"`
	Account      string `json:"account"`
	Digits       int    `json:"digits"`
	Period       int    `json:"period"`
	// ExpiresAt 是这条**未确认**登记的到期时间（代理侧 10 分钟自毁）。
	ExpiresAt string `json:"expires_at,omitempty"`
}

// totpConfirmRequest 是 POST /auth/totp/confirm 的请求体。
type totpConfirmRequest struct {
	EnrollmentID string `json:"enrollment_id"`
	TOTP         string `json:"totp"`
}

// totpConfirmResponse 是启用成功的返回：十枚恢复码**只在这里出现一次**。
type totpConfirmResponse struct {
	Enabled bool `json:"enabled"`
	// RecoveryCodes 是明文一次性恢复码。库里只有哈希，页面上也只显示这一次。
	RecoveryCodes []string `json:"recovery_codes"`
}

// totpDisableRequest 是 DELETE /auth/totp 的请求体。
//
// 口令必填，再加**一种**第二因子：验证码或恢复码。两者都给时先看验证码
// （它更常用），恢复码只在没有验证码时用。
type totpDisableRequest struct {
	Password     string `json:"password"`
	TOTP         string `json:"totp"`
	RecoveryCode string `json:"recovery_code"`
}

// totpDisableResponse 是关闭成功后的返回。
type totpDisableResponse struct {
	Enabled bool `json:"enabled"`
	// RevokedSessions 是随之作废的**其它**会话数（本条会话保留）。
	RevokedSessions int64 `json:"revoked_sessions"`
}

// handleTotpSetup 处理 POST /api/v1/auth/totp/setup（admin + 再认证 + CSRF）。
//
// 这里是"生成一条新种子"，**不是**启用：种子在代理里是临时秘密（10 分钟自毁），
// 只有 confirm 通过才转成持久秘密并在面板库里留下状态行。所以一个点了 setup 又放弃的
// 管理员，十分钟后机器上什么都不剩。
func (s *Server) handleTotpSetup(c *gin.Context) {
	sess := sessionFrom(c)
	const action = audit.ActionTotpSetup
	target := fmt.Sprintf("user:%d", sess.User.ID)

	if s.opts.Totp == nil {
		s.recordDenied(c, &sess, action, target, "agent_unavailable", httpx.CodeAgentUnavailable)
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable, "二次验证暂时不可用：代理未启用该能力")
		return
	}
	// 空对象或空体都接受：这个请求没有任何参数。
	if c.Request.ContentLength > 0 {
		var body struct{}
		if !decodeJSONBody(c, &body) {
			s.recordDenied(c, &sess, action, target, "validation", httpx.CodeValidationError)
			return
		}
	}

	state, exists, err := s.opts.Auth.TotpState(c.Request.Context(), sess.User.ID)
	if err != nil {
		s.recordDenied(c, &sess, action, target, "internal", httpx.CodeInternalError)
		s.internalError(c, "读取二次验证状态失败", err)
		return
	}
	if exists && state.Enabled {
		s.recordDenied(c, &sess, action, target, "already_enabled", httpx.CodeValidationError)
		writeError(c, http.StatusUnprocessableEntity, httpx.CodeValidationError,
			"这个账号已经启用了二次验证；要换设备，先关闭再重新登记")
		return
	}

	enrollment, err := s.opts.Totp.TotpEnroll(c.Request.Context(), sess.User.ID, sess.User.Username)
	if err != nil {
		if s.writeAgentError(c, err, "生成二次验证种子失败") {
			s.recordDenied(c, &sess, action, target, "agent_error", httpx.CodeAgentUnavailable)
			return
		}
		s.recordDenied(c, &sess, action, target, "internal", httpx.CodeInternalError)
		s.internalError(c, "生成二次验证种子失败", err)
		return
	}

	// 审计里不放引用也不放 URI：引用是"种子存在哪"的指针，URI 本身就是种子。
	// 要记的是"谁在什么时候发起过一次登记"。
	if err := s.recordAfterCommit(c, sess, action, target, map[string]any{
		"digits":     enrollment.Digits,
		"period":     enrollment.Period,
		"has_uri":    enrollment.OTPAuthURI != "",
		"expires_at": enrollment.ExpiresAt,
	}); err != nil {
		s.opts.Logger.Error("审计写入失败，登记未发起生效的记录缺失", "error", err,
			"request_id", c.GetString(ctxKeyRequestID))
	}

	httpx.WriteData(c.Writer, c.Request, http.StatusOK, totpSetupResponse{
		EnrollmentID: enrollment.SecretRef,
		OTPAuthURI:   enrollment.OTPAuthURI,
		Issuer:       enrollment.Issuer,
		Account:      enrollment.Account,
		Digits:       enrollment.Digits,
		Period:       enrollment.Period,
		ExpiresAt:    enrollment.ExpiresAt,
	})
}

// handleTotpConfirm 处理 POST /api/v1/auth/totp/confirm（admin + 再认证 + CSRF）。
//
// 顺序：代理验码 → 通过才在面板库里启用（并签发十枚恢复码）→ 作废其它会话。
// 任何一步失败都不留下"半启用"状态：代理那边种子仍是临时秘密，十分钟后自灭。
func (s *Server) handleTotpConfirm(c *gin.Context) {
	sess := sessionFrom(c)
	const action = audit.ActionTotpEnable
	target := fmt.Sprintf("user:%d", sess.User.ID)

	if s.opts.Totp == nil {
		s.recordDenied(c, &sess, action, target, "agent_unavailable", httpx.CodeAgentUnavailable)
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable, "二次验证暂时不可用：代理未启用该能力")
		return
	}
	var req totpConfirmRequest
	if !decodeJSONBody(c, &req) {
		s.recordDenied(c, &sess, action, target, "validation", httpx.CodeValidationError)
		return
	}
	if strings.TrimSpace(req.EnrollmentID) == "" || strings.TrimSpace(req.TOTP) == "" {
		s.recordDenied(c, &sess, action, target, "validation", httpx.CodeValidationError)
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError, "enrollment_id 与 totp 都不能为空")
		return
	}

	check, err := s.opts.Totp.TotpConfirm(c.Request.Context(), sess.User.ID, req.EnrollmentID, req.TOTP)
	if err != nil {
		if s.writeAgentError(c, err, "确认二次验证失败") {
			s.recordDenied(c, &sess, action, target, "agent_error", httpx.CodeAgentUnavailable)
			return
		}
		s.recordDenied(c, &sess, action, target, "internal", httpx.CodeInternalError)
		s.internalError(c, "确认二次验证失败", err)
		return
	}
	if !check.OK {
		s.opts.Auth.RecordLoginFailure(sess.User.Username, remoteIP(c.Request))
		s.recordDenied(c, &sess, action, target, "code_mismatch", httpx.CodeUnauthenticated)
		writeError(c, http.StatusUnauthorized, httpx.CodeUnauthenticated,
			"验证码不正确。请核对手机上的六位数，注意它每 30 秒变一次")
		return
	}

	// 确认用的那个码的时间步一并写进库里：代理已经验过它，面板这边必须把它记成"已用过"，
	// 否则同一个码在同一个 30 秒窗口里还能再登录一次（Q480）。
	codes, err := s.opts.Auth.EnableTotp(c.Request.Context(), sess.User.ID, req.EnrollmentID, check.Step)
	if err != nil {
		if s.writeTotpStateError(c, err) {
			s.recordDenied(c, &sess, action, target, "state", httpx.CodeValidationError)
			return
		}
		s.recordDenied(c, &sess, action, target, "internal", httpx.CodeInternalError)
		s.internalError(c, "启用二次验证失败", err)
		return
	}
	s.opts.Auth.ResetLoginSuccess(sess.User.Username)

	revoked, err := s.opts.Auth.RevokeAllForUserExcept(c.Request.Context(), sess.User.ID, sess.ID)
	if err != nil {
		// 启用已经生效，作废别的会话失败不能反过来"取消启用"（那会让种子与库里状态脱节），
		// 但必须留痕并告诉调用方：还有别的设备在用它旧的那套登录。
		s.opts.Logger.Error("启用二次验证后作废其它会话失败", "error", err,
			"user_id", sess.User.ID, "request_id", c.GetString(ctxKeyRequestID))
	}
	if err := s.recordAfterCommit(c, sess, action, target, map[string]any{
		"recovery_codes":   len(codes),
		"revoked_sessions": revoked,
	}); err != nil {
		s.opts.Logger.Error("审计写入失败（二次验证已启用）", "error", err,
			"request_id", c.GetString(ctxKeyRequestID))
	}

	httpx.WriteData(c.Writer, c.Request, http.StatusOK, totpConfirmResponse{
		Enabled:       true,
		RecoveryCodes: codes,
	})
}

// handleTotpDisable 处理 DELETE /api/v1/auth/totp（admin + 再认证 + CSRF）。
//
// 口令 + 一种第二因子。顺序上先验口令与因子，再去代理销毁种子，最后删面板库的状态行。
// 代理那一步失败就整体失败：种子还在代理里而面板说"已关闭"，是最糟的一半状态。
func (s *Server) handleTotpDisable(c *gin.Context) {
	sess := sessionFrom(c)
	const action = audit.ActionTotpDisable
	target := fmt.Sprintf("user:%d", sess.User.ID)

	if s.opts.Totp == nil {
		s.recordDenied(c, &sess, action, target, "agent_unavailable", httpx.CodeAgentUnavailable)
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable, "二次验证暂时不可用：代理未启用该能力")
		return
	}
	var req totpDisableRequest
	if !decodeJSONBody(c, &req) {
		s.recordDenied(c, &sess, action, target, "validation", httpx.CodeValidationError)
		return
	}
	if req.Password == "" {
		s.recordDenied(c, &sess, action, target, "validation", httpx.CodeValidationError)
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError, "密码不能为空")
		return
	}
	if strings.TrimSpace(req.TOTP) == "" && strings.TrimSpace(req.RecoveryCode) == "" {
		s.recordDenied(c, &sess, action, target, "validation", httpx.CodeValidationError)
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError, "需要验证码或恢复码")
		return
	}

	state, exists, err := s.opts.Auth.TotpState(c.Request.Context(), sess.User.ID)
	if err != nil {
		s.recordDenied(c, &sess, action, target, "internal", httpx.CodeInternalError)
		s.internalError(c, "读取二次验证状态失败", err)
		return
	}
	if !exists || !state.Enabled {
		s.recordDenied(c, &sess, action, target, "not_enrolled", httpx.CodeNotFound)
		writeError(c, http.StatusNotFound, httpx.CodeNotFound, "这个账号没有启用二次验证")
		return
	}

	// 口令先验：与再认证共用限速与等价耗时。
	if _, err := s.opts.Auth.VerifyReauthPassword(c.Request.Context(), sess, req.Password, remoteIP(c.Request)); err != nil {
		var limited *auth.RateLimitedError
		switch {
		case errors.As(err, &limited):
			s.recordDenied(c, &sess, action, target, "rate_limited", httpx.CodeValidationError)
			httpx.WriteErrorDetails(c.Writer, c.Request, http.StatusTooManyRequests,
				httpx.CodeValidationError, "尝试过于频繁，请稍后再试",
				gin.H{"retry_after_seconds": int(limited.RetryAfter.Round(time.Second).Seconds())})
		case errors.Is(err, auth.ErrInvalidCredentials):
			s.recordDenied(c, &sess, action, target, "wrong_password", httpx.CodeForbidden)
			writeError(c, http.StatusForbidden, httpx.CodeForbidden, "密码不正确，二次验证未关闭")
		case errors.Is(err, auth.ErrUserDisabled):
			s.recordDenied(c, &sess, action, target, "user_disabled", httpx.CodeForbidden)
			writeError(c, http.StatusForbidden, httpx.CodeForbidden, "账号已被禁用")
		default:
			s.recordDenied(c, &sess, action, target, "internal", httpx.CodeInternalError)
			s.internalError(c, "校验密码失败", err)
		}
		return
	}

	usedRecovery := false
	remaining := 0
	if strings.TrimSpace(req.TOTP) != "" {
		check, err := s.opts.Totp.TotpVerify(c.Request.Context(), sess.User.ID, state.SecretRef, req.TOTP)
		if err != nil {
			if s.writeAgentError(c, err, "校验二次验证码失败") {
				s.recordDenied(c, &sess, action, target, "agent_error", httpx.CodeAgentUnavailable)
				return
			}
			s.recordDenied(c, &sess, action, target, "internal", httpx.CodeInternalError)
			s.internalError(c, "校验二次验证码失败", err)
			return
		}
		if !check.OK {
			s.opts.Auth.RecordLoginFailure(sess.User.Username, remoteIP(c.Request))
			s.recordDenied(c, &sess, action, target, "code_mismatch", httpx.CodeUnauthenticated)
			writeError(c, http.StatusUnauthorized, httpx.CodeUnauthenticated, "验证码不正确，二次验证未关闭")
			return
		}
		if err := s.opts.Auth.AcceptTotpStep(c.Request.Context(), sess.User.ID, check.Step); err != nil {
			if errors.Is(err, auth.ErrTotpReplay) {
				s.recordDenied(c, &sess, action, target, "replay", httpx.CodeUnauthenticated)
				writeError(c, http.StatusUnauthorized, httpx.CodeUnauthenticated,
					"这个验证码刚刚用过。等手机上下一个六位数出现再试")
				return
			}
			s.recordDenied(c, &sess, action, target, "internal", httpx.CodeInternalError)
			s.internalError(c, "记录已用验证码失败", err)
			return
		}
	} else {
		remaining, err = s.opts.Auth.ConsumeRecoveryCode(c.Request.Context(), sess.User.ID, req.RecoveryCode)
		if err != nil {
			if errors.Is(err, auth.ErrRecoveryCodeInvalid) {
				s.opts.Auth.RecordLoginFailure(sess.User.Username, remoteIP(c.Request))
				s.recordDenied(c, &sess, action, target, "recovery_code_invalid", httpx.CodeUnauthenticated)
				writeError(c, http.StatusUnauthorized, httpx.CodeUnauthenticated, "恢复码无效（每枚只能用一次）")
				return
			}
			s.recordDenied(c, &sess, action, target, "internal", httpx.CodeInternalError)
			s.internalError(c, "使用恢复码失败", err)
			return
		}
		usedRecovery = true
	}

	// 代理侧销毁种子。失败就整体失败——见上面注释：不做一半。
	if _, err := s.opts.Totp.TotpDisable(c.Request.Context(), sess.User.ID, state.SecretRef); err != nil {
		if s.writeAgentError(c, err, "销毁二次验证种子失败") {
			s.recordDenied(c, &sess, action, target, "agent_error", httpx.CodeAgentUnavailable)
			return
		}
		s.recordDenied(c, &sess, action, target, "internal", httpx.CodeInternalError)
		s.internalError(c, "销毁二次验证种子失败", err)
		return
	}
	if err := s.opts.Auth.DisableTotp(c.Request.Context(), sess.User.ID); err != nil {
		if s.writeTotpStateError(c, err) {
			s.recordDenied(c, &sess, action, target, "state", httpx.CodeValidationError)
			return
		}
		s.recordDenied(c, &sess, action, target, "internal", httpx.CodeInternalError)
		s.internalError(c, "关闭二次验证失败", err)
		return
	}
	s.opts.Auth.ResetLoginSuccess(sess.User.Username)

	revoked, err := s.opts.Auth.RevokeAllForUserExcept(c.Request.Context(), sess.User.ID, sess.ID)
	if err != nil {
		s.opts.Logger.Error("关闭二次验证后作废其它会话失败", "error", err,
			"user_id", sess.User.ID, "request_id", c.GetString(ctxKeyRequestID))
	}
	if err := s.recordAfterCommit(c, sess, action, target, map[string]any{
		"used_recovery_code": usedRecovery,
		"recovery_remaining": remaining,
		"revoked_sessions":   revoked,
	}); err != nil {
		s.opts.Logger.Error("审计写入失败（二次验证已关闭）", "error", err,
			"request_id", c.GetString(ctxKeyRequestID))
	}

	httpx.WriteData(c.Writer, c.Request, http.StatusOK, totpDisableResponse{
		Enabled:         false,
		RevokedSessions: revoked,
	})
}

// verifySecondFactor 是登录与再认证共用的那道门。
//
// 返回 true 表示"可以继续"。返回 false 时已经写好了响应（401/403/503），调用方直接返回。
// code 为 nil/空表示这次请求没带验证码——对启用了二次验证的账号，那不是"跳过"，而是失败。
func (s *Server) verifySecondFactor(c *gin.Context, user auth.User, code *string) bool {
	state, exists, err := s.opts.Auth.TotpState(c.Request.Context(), user.ID)
	if err != nil {
		s.internalError(c, "读取二次验证状态失败", err)
		return false
	}
	if !exists || !state.Enabled {
		return true
	}
	if s.opts.Totp == nil {
		// 账号启用了二次验证，而这台机器的代理没有这项能力：**失败关闭**。
		// 从这里放行等于"把代理弄坏就能绕过二次验证"。
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"这个账号启用了二次验证，但代理侧暂时不可用，无法完成校验")
		return false
	}

	submitted := ""
	if code != nil {
		submitted = strings.TrimSpace(*code)
	}
	if submitted == "" {
		httpx.WriteErrorDetails(c.Writer, c.Request, http.StatusUnauthorized,
			httpx.CodeUnauthenticated, "需要二次验证码",
			gin.H{"totp_required": true})
		return false
	}

	check, err := s.opts.Totp.TotpVerify(c.Request.Context(), user.ID, state.SecretRef, submitted)
	if err != nil {
		if s.writeAgentError(c, err, "校验二次验证码失败") {
			return false
		}
		s.internalError(c, "校验二次验证码失败", err)
		return false
	}
	if !check.OK {
		s.opts.Auth.RecordLoginFailure(user.Username, remoteIP(c.Request))
		httpx.WriteErrorDetails(c.Writer, c.Request, http.StatusUnauthorized,
			httpx.CodeUnauthenticated, "二次验证码不正确",
			gin.H{"totp_required": true})
		return false
	}
	if err := s.opts.Auth.AcceptTotpStep(c.Request.Context(), user.ID, check.Step); err != nil {
		if errors.Is(err, auth.ErrTotpReplay) {
			httpx.WriteErrorDetails(c.Writer, c.Request, http.StatusUnauthorized,
				httpx.CodeUnauthenticated, "这个验证码刚刚用过，等手机上下一个六位数出现再试",
				gin.H{"totp_required": true})
			return false
		}
		s.internalError(c, "记录已用验证码失败", err)
		return false
	}
	return true
}

// writeAgentError 把代理调用的失败写进响应，返回 true 表示已经处理过。
//
// 三种情况分开：代理不可用（503，可能稍后再试）、代理明确拒绝（4xx，业务原因）、
// 其余（内部错误，交给调用方按 500 处理）。
func (s *Server) writeAgentError(c *gin.Context, err error, what string) bool {
	var unavailable *agentclient.UnavailableError
	if errors.As(err, &unavailable) {
		s.opts.Logger.Error(what+"（代理不可用）", "error", err, "request_id", c.GetString(ctxKeyRequestID))
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"代理暂时不可用，二次验证无法完成")
		return true
	}
	var rejected *agentclient.Error
	if errors.As(err, &rejected) {
		switch rejected.Status {
		case http.StatusNotFound:
			// 种子不在（虚拟机上最常见的原因是十分钟的登记窗口过了，或者已经关闭过）。
			writeError(c, http.StatusConflict, httpx.CodeValidationError,
				"这条登记已经失效，请重新开始登记")
		case http.StatusForbidden:
			writeError(c, http.StatusForbidden, httpx.CodeForbidden, rejected.Message)
		case http.StatusBadRequest:
			writeError(c, http.StatusBadRequest, httpx.CodeValidationError, rejected.Message)
		default:
			s.opts.Logger.Error(what+"（代理拒绝）", "error", err, "request_id", c.GetString(ctxKeyRequestID))
			writeError(c, http.StatusBadGateway, httpx.CodeAgentUnavailable, what)
		}
		return true
	}
	return false
}

// writeTotpStateError 映射面板库那一半的状态错误。返回 true 表示已经处理过。
func (s *Server) writeTotpStateError(c *gin.Context, err error) bool {
	switch {
	case errors.Is(err, auth.ErrTotpAlreadyEnrolled):
		writeError(c, http.StatusUnprocessableEntity, httpx.CodeValidationError,
			"这个账号已经启用了二次验证；要换设备，先关闭再重新登记")
		return true
	case errors.Is(err, auth.ErrTotpNotEnrolled):
		writeError(c, http.StatusNotFound, httpx.CodeNotFound, "这个账号没有启用二次验证")
		return true
	case errors.Is(err, auth.ErrTotpReplay):
		writeError(c, http.StatusUnauthorized, httpx.CodeUnauthenticated,
			"这个验证码刚刚用过，等手机上下一个六位数出现再试")
		return true
	case errors.Is(err, auth.ErrRecoveryCodeInvalid):
		writeError(c, http.StatusUnauthorized, httpx.CodeUnauthenticated, "恢复码无效（每枚只能用一次）")
		return true
	default:
		return false
	}
}

// recordAfterCommit 写一条"业务已经生效"的审计。
//
// 与 recordAuditInTx 的区别：这条路线的业务改不动一个事务里（代理调用不能包在
// 数据库事务里等网络），所以审计写在业务之后，失败也只能记日志——但它不会被静默吞掉。
func (s *Server) recordAfterCommit(c *gin.Context, sess auth.Session,
	action, target string, details map[string]any) error {
	if s.opts.Audit == nil {
		return fmt.Errorf("%w: 审计组件未初始化", errAuditWrite)
	}
	return s.opts.Audit.Record(c.Request.Context(), s.auditEvent(c, sess, action, target, audit.OutcomeSuccess, details))
}

// totpCodeShapeOK 判断一串输入像不像一个六位数验证码。
//
// 只在面板侧做"明显是垃圾"的快速判断（空、超长、非数字），真正对不对由代理算。
// 不在这里决定"必须是六位"：位数是代理的契约，两边各写一份只会有一天不一致。
func totpCodeShapeOK(code string) bool {
	code = strings.TrimSpace(code)
	if code == "" || len(code) > int(totp.Digits)+2 {
		return false
	}
	for _, r := range code {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
