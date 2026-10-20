package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"upanel/internal/agentclient"
	"upanel/internal/audit"
	"upanel/internal/auth"
	"upanel/internal/httpx"
	"upanel/internal/operations"
)

// preflightRequest 是 POST /api/v1/operations/preflight 的请求体（CONTRACTS.md 4.1）。
//
// 只有动作名与参数：没有 user_id（身份来自会话）、没有 allowed（结论来自代理）、
// 没有确认短语（短语由服务端在预检结果里生成，不能由前端决定）。
type preflightRequest struct {
	Action string          `json:"action"`
	Params json.RawMessage `json:"params"`
}

// preflightTargetDTO 是预检目标。state_version 属于内部绑定信息，不对前端暴露。
type preflightTargetDTO struct {
	DisplayName         string `json:"display_name"`
	IdentityFingerprint string `json:"identity_fingerprint"`
}

// preflightNoticeDTO 是阻塞项/告警项。
type preflightNoticeDTO struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// preflightImpactDTO 是操作影响。
type preflightImpactDTO struct {
	Kind        string `json:"kind"`
	Description string `json:"description"`
}

// preflightResponse 是预检接口的返回体（字段与 CONTRACTS.md 4.1 一致）。
//
// confirmation_phrase 在不要求短语时为 null：这比省略字段更明确——省略会让前端
// 分不清“不需要短语”和“服务端忘了给”。
type preflightResponse struct {
	PreflightID        string               `json:"preflight_id"`
	Action             string               `json:"action"`
	NormalizedParams   json.RawMessage      `json:"normalized_params"`
	Target             preflightTargetDTO   `json:"target"`
	ExpiresAt          string               `json:"expires_at"`
	Allowed            bool                 `json:"allowed"`
	Blockers           []preflightNoticeDTO `json:"blockers"`
	Warnings           []preflightNoticeDTO `json:"warnings"`
	Impacts            []preflightImpactDTO `json:"impacts"`
	RequiresReauth     bool                 `json:"requires_reauth"`
	ConfirmationPhrase *string              `json:"confirmation_phrase"`
	Cancellable        bool                 `json:"cancellable"`
	Locks              []string             `json:"locks"`
}

// handleCreatePreflight 处理 POST /api/v1/operations/preflight（仅 admin，需 CSRF）。
//
// 本接口**不改变系统状态**：它向代理要一份计划，把绑定信息记下来，然后把影响与
// 阻塞项交给前端展示。真正的执行在 POST /jobs（P03.2），那一步会重新校验再认证、
// 预检有效期、绑定关系与确认短语。
func (s *Server) handleCreatePreflight(c *gin.Context) {
	sess := sessionFrom(c)
	const action = audit.ActionOperationPreflight

	if !isJSONContentType(c.GetHeader("Content-Type")) {
		s.reject(c, "content_type", httpx.CodeValidationError)
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError, "预检请求必须使用 application/json")
		return
	}
	var req preflightRequest
	if !decodeJSONBody(c, &req) {
		return
	}

	pre, err := s.opts.Operations.CreatePreflight(c.Request.Context(), operations.CreateInput{
		UserID:      sess.User.ID,
		SessionHash: sess.BindingHash(),
		ReauthAt:    reauthAtFor(sess),
		RequestID:   c.GetString(ctxKeyRequestID),
		Action:      req.Action,
		Params:      req.Params,
	})
	if err != nil {
		s.writePreflightError(c, &sess, action, req.Action, err)
		return
	}

	// 预检本身不是写操作，但它是“准备执行一个危险动作”的明确意图，值得留痕。
	if s.opts.Audit != nil {
		ev := s.auditEvent(c, sess, action, "preflight:"+pre.Action, audit.OutcomeSuccess, map[string]any{
			"preflight_id":    pre.ID,
			"allowed":         pre.Allowed,
			"blockers":        len(pre.Blockers),
			"warnings":        len(pre.Warnings),
			"requires_reauth": pre.RequiresReauth,
			"requires_phrase": pre.RequiresPhrase,
			"expires_at":      pre.ExpiresAt.Format(time.RFC3339),
		})
		if err := s.opts.Audit.Record(c.Request.Context(), ev); err != nil {
			// 预检已经落库、管理员已经拿到计划；这里失败只影响可追溯性。
			// 记录 ERROR 但不改变响应——把已生成的预检说成失败会让前端重复提交。
			s.opts.Logger.Error("预检审计写入失败", "error", err,
				"preflight_id", pre.ID, "request_id", c.GetString(ctxKeyRequestID))
		}
	}

	resp := preflightResponse{
		PreflightID:      pre.ID,
		Action:           pre.Action,
		NormalizedParams: pre.NormalizedParams,
		Target: preflightTargetDTO{
			DisplayName:         pre.Target.DisplayName,
			IdentityFingerprint: pre.Target.IdentityFingerprint,
		},
		ExpiresAt:      pre.ExpiresAt.Format(time.RFC3339),
		Allowed:        pre.Allowed,
		Blockers:       noticeDTOs(pre.Blockers),
		Warnings:       noticeDTOs(pre.Warnings),
		Impacts:        impactDTOs(pre.Impacts),
		RequiresReauth: pre.RequiresReauth,
		Cancellable:    pre.Cancellable,
		Locks:          pre.Locks,
	}
	if pre.RequiresPhrase {
		phrase := pre.ConfirmationPhrase
		resp.ConfirmationPhrase = &phrase
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, resp)
}

// writePreflightError 把预检失败映射成契约错误码。
func (s *Server) writePreflightError(c *gin.Context, sess *auth.Session, action, requested string, err error) {
	var unavailable *agentclient.UnavailableError
	var agentErr *agentclient.Error
	switch {
	case errors.As(err, &unavailable):
		// 代理连不上：这不是动作不合法，前端应提示“稍后重试”，不能显示成参数错误。
		s.recordDenied(c, sess, action, "preflight:"+requested, "agent", httpx.CodeAgentUnavailable)
		s.opts.Logger.Warn("预检无法连接代理", "error", err,
			"action", requested, "request_id", c.GetString(ctxKeyRequestID))
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"特权代理不可用，暂时无法生成预检")
	case errors.As(err, &agentErr):
		code := httpx.Code(agentErr.Code)
		if !knownCode(code) {
			// 代理给了枚举外的错误码：按内部错误处理，不把它转发给前端。
			s.opts.Logger.Error("代理返回未知错误码", "code", agentErr.Code,
				"action", requested, "request_id", c.GetString(ctxKeyRequestID))
			s.recordDenied(c, sess, action, "preflight:"+requested, "agent_code", httpx.CodeInternalError)
			s.internalError(c, "代理返回未知错误码", errors.New(agentErr.Error()))
			return
		}
		s.recordDenied(c, sess, action, "preflight:"+requested, "agent", code)
		writeError(c, httpx.StatusForCode(code), code, agentErr.Message)
	case errors.Is(err, operations.ErrInvalidAction),
		errors.Is(err, operations.ErrParamsTooLarge),
		errors.Is(err, operations.ErrParamsNotObject):
		s.recordDenied(c, sess, action, "preflight:"+requested, "validation", httpx.CodeValidationError)
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError, err.Error())
	case errors.Is(err, operations.ErrAgentUnavailable):
		s.recordDenied(c, sess, action, "preflight:"+requested, "agent", httpx.CodeAgentUnavailable)
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable, err.Error())
	default:
		// 包含 ErrInvalidPlan：自己和自己说话却对不上契约，属于实现错误。
		s.recordDenied(c, sess, action, "preflight:"+requested, "internal", httpx.CodeInternalError)
		s.internalError(c, "生成预检失败", err)
	}
}

// reauthAtFor 返回会话最近一次再认证时间；不在窗口内时返回零值。
func reauthAtFor(sess auth.Session) time.Time {
	if sess.ReauthUntil.IsZero() || !time.Now().Before(sess.ReauthUntil) {
		return time.Time{}
	}
	return sess.ReauthUntil.UTC().Add(-auth.ReauthWindow)
}

// knownCode 判断错误码是否在契约枚举内。
func knownCode(code httpx.Code) bool {
	for _, c := range httpx.Codes() {
		if c == code {
			return true
		}
	}
	return false
}

func noticeDTOs(in []operations.Notice) []preflightNoticeDTO {
	out := make([]preflightNoticeDTO, 0, len(in))
	for _, n := range in {
		out = append(out, preflightNoticeDTO{Code: n.Code, Message: n.Message})
	}
	return out
}

func impactDTOs(in []operations.Impact) []preflightImpactDTO {
	out := make([]preflightImpactDTO, 0, len(in))
	for _, i := range in {
		out = append(out, preflightImpactDTO{Kind: i.Kind, Description: i.Description})
	}
	return out
}
