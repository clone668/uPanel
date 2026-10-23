package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"upanel/internal/agentclient"
	"upanel/internal/audit"
	"upanel/internal/auth"
	"upanel/internal/httpx"
	"upanel/internal/operations"
)

// 本文件实现 CONTRACTS.md 4.2 与 4.3 里的任务接口。
//
// 一条纪律贯穿这里：**响应只说实话**。
//   - 进度不知道就是 null，不编一个从 0 涨到 100 的假数字；
//   - 提交超时但代理可能已接受时，状态仍是 queued，不谎报成功也不谎报失败；
//   - 取消是“请求”，所以 cancellation_requested 与 status 是两个字段；
//   - 不可取消的动作不给取消入口，取消请求返回 409 而不是假装排队。

// jobSubmitRequest 是 POST /api/v1/jobs 的请求体（CONTRACTS.md 4.2）。
//
// 没有 action、没有 params：提交阶段不允许替换目标，参数一律按预检恢复。
type jobSubmitRequest struct {
	PreflightID        string `json:"preflight_id"`
	ConfirmationPhrase string `json:"confirmation_phrase"`
}

// jobSubmitResponse 是创建任务的返回体。
type jobSubmitResponse struct {
	JobID  string `json:"job_id"`
	Status string `json:"status"`
	// EventsURL 指向 P03.3 的 SSE 端点。P03.2 只给出地址，不实现流。
	EventsURL string `json:"events_url"`
}

// jobResponse 是单个任务的视图（CONTRACTS.md 4.3）。
//
// 不包含 operation_id、预检引用与参数：前者是内部稳定标识，后两者是绑定材料。
// viewer 与 admin 看到的是同一份字段——因为这里本来就没有原始参数或日志可裁剪。
type jobResponse struct {
	JobID                 string          `json:"job_id"`
	Action                string          `json:"action"`
	Status                string          `json:"status"`
	Step                  string          `json:"step"`
	ProgressPercent       *int            `json:"progress_percent"`
	Result                json.RawMessage `json:"result"`
	Error                 json.RawMessage `json:"error"`
	Cancellable           bool            `json:"cancellable"`
	CancellationRequested bool            `json:"cancellation_requested"`
	CreatedAt             string          `json:"created_at"`
	UpdatedAt             string          `json:"updated_at"`
	StartedAt             *string         `json:"started_at"`
	FinishedAt            *string         `json:"finished_at"`
}

// handleCreateJob 处理 POST /api/v1/jobs（admin，需 CSRF，必须带 Idempotency-Key）。
func (s *Server) handleCreateJob(c *gin.Context) {
	// 提交这一步不总是"落一条记录就返回"：代理在**接受之前**要复核一次计划，
	// 软件包动作的这次复核是一条 `apt-get -s`（24.04 上实测 38.7 秒）。Web 的写超时
	// 是 30 秒，不推就会在第 30 秒切断连接：代理其实已经接受了，浏览器却看到失败，
	// 任务停在"待对账"——这正是 P11.2 验收第二轮在 24.04 上的原样失败。
	extendWriteDeadline(c, slowOperationWriteBudget)
	sess := sessionFrom(c)
	const action = audit.ActionJobSubmit

	if !isJSONContentType(c.GetHeader("Content-Type")) {
		s.reject(c, "content_type", httpx.CodeValidationError)
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError, "提交任务必须使用 application/json")
		return
	}
	key := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if key == "" {
		// 幂等键是这张接口的**必需**材料：缺了它，“重试一次”就等于“再执行一次”。
		s.reject(c, "idempotency_key", httpx.CodeValidationError)
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError,
			"缺少 Idempotency-Key 请求头：一次意图重试时必须复用同一个键")
		return
	}
	var req jobSubmitRequest
	if !decodeJSONBody(c, &req) {
		return
	}

	job, err := s.opts.Operations.SubmitJob(c.Request.Context(), operations.SubmitInput{
		UserID:         sess.User.ID,
		SessionHash:    sess.BindingHash(),
		ReauthAt:       reauthAtFor(sess),
		RequestID:      c.GetString(ctxKeyRequestID),
		IdempotencyKey: key,
		PreflightID:    req.PreflightID,
		Confirmation:   req.ConfirmationPhrase,
	})
	if err != nil {
		s.writeJobError(c, &sess, action, "job", err)
		return
	}

	// 重放（同一键同一请求）不再记一条“又提交了一次”的审计：那不是新的意图。
	if !job.Duplicate && s.opts.Audit != nil {
		ev := s.auditEvent(c, sess, action, "job:"+job.ID, audit.OutcomeSuccess, map[string]any{
			"job_id":     job.ID,
			"action":     job.Action,
			"status":     job.Status,
			"preflight":  job.PreflightID,
			"needs_sync": job.NeedsSync,
		})
		if err := s.opts.Audit.Record(c.Request.Context(), ev); err != nil {
			// 任务已经落库；审计失败只影响可追溯性，不能让前端以为提交失败而去重试。
			s.opts.Logger.Error("任务审计写入失败", "error", err,
				"job_id", job.ID, "request_id", c.GetString(ctxKeyRequestID))
		}
	}

	httpx.WriteData(c.Writer, c.Request, http.StatusAccepted, jobSubmitResponse{
		JobID:     job.ID,
		Status:    job.Status,
		EventsURL: "/api/v1/jobs/" + job.ID + "/events",
	})
}

// handleListJobs 处理 GET /api/v1/jobs（任意已登录角色，只返回本人的任务）。
func (s *Server) handleListJobs(c *gin.Context) {
	sess := sessionFrom(c)
	limit := 0
	if raw := c.Query("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 || n > 200 {
			writeError(c, http.StatusBadRequest, httpx.CodeValidationError, "limit 必须是 1—200 的整数")
			return
		}
		limit = n
	}
	jobs, err := s.opts.Operations.ListJobs(c.Request.Context(), sess.User.ID, limit)
	if err != nil {
		s.internalError(c, "读取任务列表失败", err)
		return
	}
	out := make([]jobResponse, 0, len(jobs))
	for _, job := range jobs {
		out = append(out, jobDTO(job))
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, gin.H{"jobs": out})
}

// handleGetJob 处理 GET /api/v1/jobs/:id。
//
// 这是“浏览器关闭后仍能找到原 operation”的入口：非终态任务会按 operation_id
// 向代理对账一次，然后把代理看到的真实阶段写回。
func (s *Server) handleGetJob(c *gin.Context) {
	sess := sessionFrom(c)
	job, err := s.opts.Operations.GetJob(c.Request.Context(), sess.User.ID, c.Param("id"))
	if err != nil {
		s.writeJobError(c, &sess, audit.ActionJobSubmit, "job:"+c.Param("id"), err)
		return
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, jobDTO(job))
}

// handleCancelJob 处理 POST /api/v1/jobs/:id/cancel（admin，需 CSRF）。
func (s *Server) handleCancelJob(c *gin.Context) {
	sess := sessionFrom(c)
	const action = audit.ActionJobCancel

	job, err := s.opts.Operations.CancelJob(c.Request.Context(), sess.User.ID, c.Param("id"))
	if err != nil {
		s.writeJobError(c, &sess, action, "job:"+c.Param("id"), err)
		return
	}
	if s.opts.Audit != nil {
		ev := s.auditEvent(c, sess, action, "job:"+job.ID, audit.OutcomeSuccess, map[string]any{
			"job_id":                 job.ID,
			"action":                 job.Action,
			"status":                 job.Status,
			"cancellation_requested": job.CancellationRequested,
		})
		if err := s.opts.Audit.Record(c.Request.Context(), ev); err != nil {
			s.opts.Logger.Error("取消任务审计写入失败", "error", err,
				"job_id", job.ID, "request_id", c.GetString(ctxKeyRequestID))
		}
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusAccepted, jobDTO(job))
}

// writeJobError 把任务失败映射成契约错误码。
func (s *Server) writeJobError(c *gin.Context, sess *auth.Session, action, target string, err error) {
	var unavailable *agentclient.UnavailableError
	var agentErr *agentclient.Error
	var rejected *operations.Rejected
	switch {
	case errors.As(err, &rejected):
		// 代理明确拒绝：把它的错误码按枚举校验后原样返回给前端。
		code := httpx.Code(rejected.Code)
		if !knownCode(code) {
			s.opts.Logger.Error("代理返回未知错误码", "code", rejected.Code,
				"request_id", c.GetString(ctxKeyRequestID))
			s.recordDenied(c, sess, action, target, "agent_code", httpx.CodeInternalError)
			s.internalError(c, "代理返回未知错误码", errors.New(rejected.Message))
			return
		}
		s.recordDenied(c, sess, action, target, "agent", code)
		s.logJobRejection(c, rejected)
		writeError(c, httpx.StatusForCode(code), code, rejected.Message)
	case errors.As(err, &unavailable):
		s.recordDenied(c, sess, action, target, "agent", httpx.CodeAgentUnavailable)
		s.opts.Logger.Warn("任务接口无法连接代理", "error", err,
			"request_id", c.GetString(ctxKeyRequestID))
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"特权代理不可用，暂时无法提交或查询任务")
	case errors.As(err, &agentErr):
		code := httpx.Code(agentErr.Code)
		if !knownCode(code) {
			s.recordDenied(c, sess, action, target, "agent_code", httpx.CodeInternalError)
			s.internalError(c, "代理返回未知错误码", errors.New(agentErr.Message))
			return
		}
		s.recordDenied(c, sess, action, target, "agent", code)
		writeError(c, httpx.StatusForCode(code), code, agentErr.Message)
	case errors.Is(err, operations.ErrInvalidIdempotencyKey):
		s.recordDenied(c, sess, action, target, "idempotency_key", httpx.CodeValidationError)
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError, err.Error())
	case errors.Is(err, operations.ErrIdempotencyConflict):
		s.recordDenied(c, sess, action, target, "idempotency_key", httpx.CodeIdempotencyConflict)
		writeError(c, http.StatusConflict, httpx.CodeIdempotencyConflict,
			"同一个 Idempotency-Key 已经用于另一个请求；重试时请复用原请求体，或换一个新键")
	case errors.Is(err, operations.ErrPreflightNotFound):
		s.recordDenied(c, sess, action, target, "preflight", httpx.CodeNotFound)
		writeError(c, http.StatusNotFound, httpx.CodeNotFound, "预检不存在或已不属于当前账号")
	case errors.Is(err, operations.ErrPreflightExpired):
		s.recordDenied(c, sess, action, target, "preflight", httpx.CodePreflightExpired)
		writeError(c, http.StatusConflict, httpx.CodePreflightExpired,
			"预检已过期，请重新预检后再提交")
	case errors.Is(err, operations.ErrPreflightUsed):
		s.recordDenied(c, sess, action, target, "preflight", httpx.CodePreflightStale)
		writeError(c, http.StatusConflict, httpx.CodePreflightStale,
			"这份预检已经被使用过：一次预检只能提交一次任务")
	case errors.Is(err, operations.ErrPreflightSessionChanged):
		s.recordDenied(c, sess, action, target, "preflight", httpx.CodePreflightStale)
		writeError(c, http.StatusConflict, httpx.CodePreflightStale,
			"预检绑定的会话已变化（重新登录或会话轮换），请重新预检")
	case errors.Is(err, operations.ErrPreflightBlocked):
		s.recordDenied(c, sess, action, target, "preflight", httpx.CodeDependencyBlocked)
		writeError(c, http.StatusConflict, httpx.CodeDependencyBlocked,
			"预检结论是不允许执行：先处理阻塞项，再重新预检")
	case errors.Is(err, operations.ErrReauthRequired):
		s.recordDenied(c, sess, action, target, "reauth", httpx.CodeReauthRequired)
		writeError(c, http.StatusForbidden, httpx.CodeReauthRequired,
			"该动作要求 5 分钟内的密码再认证，请先重新验证密码")
	case errors.Is(err, operations.ErrJobNotFound):
		s.recordDenied(c, sess, action, target, "job", httpx.CodeNotFound)
		writeError(c, http.StatusNotFound, httpx.CodeNotFound, "任务不存在")
	case errors.Is(err, operations.ErrJobNotCancelable):
		// 409 是契约要求（CONTRACTS.md 4.3）；错误码仍走枚举内的 VALIDATION_ERROR，
		// 消息说清“这是策略不允许，不是服务端故障”。
		s.recordDenied(c, sess, action, target, "cancel", httpx.CodeValidationError)
		writeError(c, http.StatusConflict, httpx.CodeValidationError,
			"该动作开始执行后不可取消（格式化、分区写入这类操作一旦开始就必须走完）")
	case errors.Is(err, operations.ErrJobFinished):
		s.recordDenied(c, sess, action, target, "cancel", httpx.CodeVersionConflict)
		writeError(c, http.StatusConflict, httpx.CodeVersionConflict,
			"任务已经结束，终态不可变更；重新操作请创建新的预检与任务")
	case errors.Is(err, operations.ErrAgentUnavailable):
		s.recordDenied(c, sess, action, target, "agent", httpx.CodeAgentUnavailable)
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable, err.Error())
	default:
		s.recordDenied(c, sess, action, target, "internal", httpx.CodeInternalError)
		s.internalError(c, "任务操作失败", err)
	}
}

// logJobRejection 在服务端留下“任务被拒”的完整痕迹（前端只看到错误码与消息）。
func (s *Server) logJobRejection(c *gin.Context, rejected *operations.Rejected) {
	s.opts.Logger.Warn("代理拒绝了任务请求",
		"code", rejected.Code,
		"job_id", rejected.Job.ID,
		"status", rejected.Job.Status,
		"request_id", c.GetString(ctxKeyRequestID))
}

// jobDTO 把内部任务整理成公开视图。
func jobDTO(job operations.Job) jobResponse {
	out := jobResponse{
		JobID:                 job.ID,
		Action:                job.Action,
		Status:                job.Status,
		Step:                  job.Step,
		ProgressPercent:       job.ProgressPercent,
		Result:                job.Result,
		Error:                 job.Error,
		Cancellable:           job.Cancellable,
		CancellationRequested: job.CancellationRequested,
		CreatedAt:             formatRFC3339(job.CreatedAt),
		UpdatedAt:             formatRFC3339(job.UpdatedAt),
	}
	if !job.StartedAt.IsZero() {
		at := formatRFC3339(job.StartedAt)
		out.StartedAt = &at
	}
	if !job.FinishedAt.IsZero() {
		at := formatRFC3339(job.FinishedAt)
		out.FinishedAt = &at
	}
	return out
}

// formatRFC3339 把时间统一成 UTC 的 RFC3339 文本。
func formatRFC3339(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}
