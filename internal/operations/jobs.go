package operations

// P03.2 Web 侧：任务的持久化、幂等与对账。
//
// 三条关系写在这里备查，因为这一轮最容易做错的就是它们：
//
//	Idempotency-Key ──► idempotency_keys(user_id, route, key, body_hash) ──► jobs.id
//	                                                                        │
//	                   preflights.consumed_job_id ◄─────────────────────────┤（一次性）
//	                                                                        │
//	                            jobs.operation_id ──► 代理 operations.operation_id
//	                                                  （代理侧 UNIQUE + request_hash）
//
// 顺序固定：**先落库，再提交**。job 与 operation_id 在调用代理之前就已写好，
// 所以“提交超时”只会留下一个待对账的任务，不会变成“再来一次、再格式化一次”。
//
// 两层幂等不是重复劳动：
//   - Web 层挡住重复点击与 HTTP 重试（相同键 + 相同请求体 → 原 job）；
//   - 代理层挡住 Web 崩溃窗口（同一个 operation_id + 同一个请求哈希 → 原操作）。
// 只做一层，另一层出问题时就可能出现两次执行。

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"upanel/internal/agentclient"
)

const (
	// jobIDPrefix 是公开任务标识的前缀。
	jobIDPrefix = "job_"
	// operationIDPrefix 是代理侧操作标识的前缀（形状由代理校验，Web 必须照做）。
	operationIDPrefix = "op_"
	// jobRouteSubmit 是幂等键的路由维度（CONTRACTS.md 4.2：相同用户 + 接口 + 键）。
	jobRouteSubmit = "POST /api/v1/jobs"
	// idempotencyTTL 是幂等记录的保留期（CONTRACTS.md 14.2：至少 24 小时）。
	//
	// 清理任务属 P03.4；这里只写下到期时间，不提前删。
	idempotencyTTL = 24 * time.Hour
	// MaxIdempotencyKeyLength 是幂等键的长度上限。
	MaxIdempotencyKeyLength = 200
	// reauthWindow 是提交任务时认可的再认证窗口（CONTRACTS.md 第 2 节：5 分钟）。
	reauthWindow = 5 * time.Minute
	// reauthClockSkew 是允许的时钟偏差：再认证时间可能比本机时间晚一点点。
	reauthClockSkew = time.Minute
)

// 任务相关错误。API 层据此映射状态码与契约错误码。
var (
	// ErrJobNotFound 表示任务不存在，或不属于当前用户。
	ErrJobNotFound = errors.New("operations: 任务不存在")
	// ErrPreflightNotFound 表示预检记录不存在（含“不属于当前用户”）。
	ErrPreflightNotFound = errors.New("operations: 预检不存在")
	// ErrPreflightExpired 表示预检已过期。
	ErrPreflightExpired = errors.New("operations: 预检已过期")
	// ErrPreflightUsed 表示预检已被消费（一次性）。
	ErrPreflightUsed = errors.New("operations: 预检已被使用")
	// ErrPreflightSessionChanged 表示预检绑定的会话已变化。
	ErrPreflightSessionChanged = errors.New("operations: 预检绑定的会话已变化")
	// ErrPreflightBlocked 表示预检结论是不允许执行。
	ErrPreflightBlocked = errors.New("operations: 预检不允许执行")
	// ErrReauthRequired 表示提交需要 5 分钟内的密码再认证。
	ErrReauthRequired = errors.New("operations: 需要重新再认证")
	// ErrIdempotencyConflict 表示同一个键被用于不同的请求。
	ErrIdempotencyConflict = errors.New("operations: 幂等键已用于另一个请求")
	// ErrInvalidIdempotencyKey 表示幂等键缺失或形状不合法。
	ErrInvalidIdempotencyKey = errors.New("operations: 幂等键不合法")
	// ErrJobNotCancelable 表示该动作不支持安全取消。
	ErrJobNotCancelable = errors.New("operations: 该任务不可取消")
	// ErrJobFinished 表示任务已结束，不能再取消。
	ErrJobFinished = errors.New("operations: 任务已结束")
)

// errConcurrentIdempotency 是事务内的回滚信号：并发的同一幂等键抢先落库。
var errConcurrentIdempotency = errors.New("operations: 幂等键并发冲突")

// Rejected 表示代理在提交或取消时明确拒绝了这次请求。
//
// 它带着已经落库的 Job：拒绝之后任务仍然存在（状态 failed + 错误码），
// 因为“谁在什么时候试图执行什么、为什么被拒”本身就是需要留痕的事实。
type Rejected struct {
	Code    string
	Message string
	Job     Job
}

func (e *Rejected) Error() string {
	return fmt.Sprintf("operations: 代理拒绝了请求（%s）: %s", e.Code, e.Message)
}

// Job 是任务的公开视图。
type Job struct {
	ID     string
	UserID int64
	Action string
	Status string
	Step   string
	// ProgressPercent 为 nil 表示“不知道”，不是 0（CONTRACTS.md 4.3）。
	ProgressPercent       *int
	Result                json.RawMessage
	Error                 json.RawMessage
	Cancellable           bool
	CancellationRequested bool
	OperationID           string
	PreflightID           string
	CreatedAt             time.Time
	UpdatedAt             time.Time
	StartedAt             time.Time
	FinishedAt            time.Time
	// NeedsSync 表示这条记录还需要与代理对账。
	NeedsSync bool
	// Duplicate 表示这次提交是幂等重放（返回的是已有的任务）。
	Duplicate bool
}

// SubmitInput 是提交一次任务的输入。
type SubmitInput struct {
	UserID      int64
	SessionHash string
	ReauthAt    time.Time
	RequestID   string
	// IdempotencyKey 是 Idempotency-Key 请求头的原值；只做哈希，不进日志。
	IdempotencyKey string
	PreflightID    string
	Confirmation   string
}

// SubmitJob 提交一次任务：先落库，再向代理提交。
func (s *Service) SubmitJob(ctx context.Context, in SubmitInput) (Job, error) {
	if in.UserID <= 0 {
		return Job{}, errors.New("operations: UserID 必须为正数")
	}
	if len(in.SessionHash) != 64 {
		return Job{}, errors.New("operations: SessionHash 必须是 64 位十六进制")
	}
	if err := validateIdempotencyKey(in.IdempotencyKey); err != nil {
		return Job{}, err
	}
	if strings.TrimSpace(in.PreflightID) == "" {
		return Job{}, fmt.Errorf("%w: preflight_id 不能为空", ErrPreflightNotFound)
	}
	if len(in.Confirmation) > 128 {
		return Job{}, fmt.Errorf("%w: confirmation_phrase 超过 128 字节", ErrPreflightNotFound)
	}
	// 没有代理就没有执行：不落库、不返回假 job，直接说代理不可用。
	if s.agent == nil {
		return Job{}, fmt.Errorf("%w: 未配置代理 socket", ErrAgentUnavailable)
	}

	now := s.now().UTC()
	bodyHash := hashHex([]byte(canonicalPair(in.PreflightID, in.Confirmation)))

	jobID, err := newPrefixedID(jobIDPrefix)
	if err != nil {
		return Job{}, err
	}
	operationID, err := newPrefixedID(operationIDPrefix)
	if err != nil {
		return Job{}, err
	}

	var (
		agentRef    string
		action      string
		cancellable bool
		replayJob   *Job
	)

	err = s.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		// 1) 幂等：这个键是不是已经在用？
		var existingJobID, existingHash string
		row := tx.QueryRowContext(ctx,
			`SELECT job_id, body_hash FROM idempotency_keys WHERE user_id = ? AND route = ? AND key = ?`,
			in.UserID, jobRouteSubmit, in.IdempotencyKey)
		switch scanErr := row.Scan(&existingJobID, &existingHash); {
		case scanErr == nil:
			if existingHash != bodyHash {
				return ErrIdempotencyConflict
			}
			job, err := getJobTx(ctx, tx, existingJobID)
			if err != nil {
				return err
			}
			job.Duplicate = true
			replayJob = &job
			return nil
		case errors.Is(scanErr, sql.ErrNoRows):
			// 首次提交，继续。
		default:
			return fmt.Errorf("operations: 读取幂等记录失败: %w", scanErr)
		}

		// 2) 预检：存在、属于本人、会话未变、未过期、未消费、允许执行。
		var (
			preflightUserID         int64
			sessionHash, expiresAt  string
			consumedBy              string
			allowed, requiresReauth int
			cancellableCol          int
		)
		row = tx.QueryRowContext(ctx, `
			SELECT user_id, session_hash, agent_ref, action, allowed, requires_reauth,
			       cancellable, expires_at, COALESCE(consumed_job_id, '')
			  FROM preflights WHERE id = ?`, in.PreflightID)
		if err := row.Scan(&preflightUserID, &sessionHash, &agentRef, &action, &allowed,
			&requiresReauth, &cancellableCol, &expiresAt, &consumedBy); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrPreflightNotFound
			}
			return fmt.Errorf("operations: 读取预检失败: %w", err)
		}
		// 别人的预检一律按“不存在”处理：不泄漏别人有哪些预检。
		if preflightUserID != in.UserID {
			return ErrPreflightNotFound
		}
		if sessionHash != in.SessionHash {
			return ErrPreflightSessionChanged
		}
		if consumedBy != "" {
			return ErrPreflightUsed
		}
		exp, err := parseStoredTime(expiresAt)
		if err != nil {
			return fmt.Errorf("operations: 预检到期时间不合法: %w", err)
		}
		if !now.Before(exp) {
			return ErrPreflightExpired
		}
		if allowed != 1 {
			return ErrPreflightBlocked
		}
		if requiresReauth == 1 && !withinReauthWindow(in.ReauthAt, now) {
			return ErrReauthRequired
		}
		// 可取消策略来自预检（代理下发），前端不能自己决定要不要显示取消按钮。
		cancellable = cancellableCol == 1

		// 3) 一次性：条件更新是“谁先谁赢”的判定点。
		res, err := tx.ExecContext(ctx,
			`UPDATE preflights SET consumed_job_id = ? WHERE id = ? AND consumed_job_id IS NULL`,
			jobID, in.PreflightID)
		if err != nil {
			return fmt.Errorf("operations: 消费预检失败: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("operations: 读取消费结果失败: %w", err)
		}
		if n == 0 {
			return ErrPreflightUsed
		}

		// 4) 任务本体。needs_sync=1：此刻代理还不知道这条操作，等待提交结果。
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO jobs
			  (id, operation_id, preflight_id, user_id, action, status, progress_percent, step,
			   result_json, error_json, cancellable, cancellation_requested, needs_sync,
			   created_at, updated_at, started_at, finished_at)
			VALUES (?, ?, ?, ?, ?, 'queued', NULL, '', '{}', '{}', ?, 0, 1, ?, ?, '', '')`,
			jobID, operationID, in.PreflightID, in.UserID, action, boolInt(cancellable),
			formatTime(now), formatTime(now)); err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("operations: 任务标识冲突，请重试: %w", err)
			}
			return fmt.Errorf("operations: 保存任务失败: %w", err)
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO idempotency_keys (user_id, route, key, body_hash, job_id, created_at, expires_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			in.UserID, jobRouteSubmit, in.IdempotencyKey, bodyHash, jobID,
			formatTime(now), formatTime(now.Add(idempotencyTTL))); err != nil {
			if isUniqueViolation(err) {
				// 并发的同一键请求抢先落库：回滚，让调用方按“重放”重读。
				return errConcurrentIdempotency
			}
			return fmt.Errorf("operations: 保存幂等记录失败: %w", err)
		}

		return appendJobEventTx(ctx, tx, jobID, "status",
			map[string]any{"status": "queued"}, now)
	})
	if errors.Is(err, errConcurrentIdempotency) {
		job, rerr := s.lookupByIdempotencyKey(ctx, in.UserID, in.IdempotencyKey)
		if rerr != nil {
			return Job{}, rerr
		}
		if job.ID != jobID {
			job.Duplicate = true
			return job, nil
		}
		return Job{}, errors.New("operations: 幂等键竞争后仍拿不到原任务")
	}
	if err != nil {
		return Job{}, err
	}
	if replayJob != nil {
		s.logger.Info("任务提交是幂等重放",
			"job_id", replayJob.ID, "status", replayJob.Status, "user_id", in.UserID)
		return *replayJob, nil
	}

	// 5) 落库之后才向代理提交：从这里往后，任何失败都只改变任务状态，不会产生第二次执行。
	actor := agentclient.ActorContext{
		UserID:        in.UserID,
		SessionIDHash: in.SessionHash,
		RequestID:     in.RequestID,
	}
	if !in.ReauthAt.IsZero() {
		at := in.ReauthAt.UTC().Format(time.RFC3339)
		actor.ReauthAt = &at
	}
	// 登记“正在提交”，再调代理（P03.4）。窗口内任何对账都不许下 NOT_FOUND 结论：
	// 代理还没收到这条操作，不代表它不会在执行；把在途动作记成失败是最坏的一种错。
	agentOp, err := func() (agentclient.Operation, error) {
		s.beginSubmit(jobID)
		defer s.endSubmit(jobID)
		return s.agent.CreateOperation(ctx, agentclient.OperationInput{
			OperationID:  operationID,
			PreflightRef: agentRef,
			Confirmation: in.Confirmation,
			Actor:        actor,
		})
	}()
	if err != nil {
		return s.handleSubmitFailure(ctx, jobID, operationID, in, err)
	}

	s.logger.Info("已提交操作",
		"job_id", jobID, "operation_id", operationID, "action", action,
		"stage", agentOp.Stage, "duplicate", agentOp.Duplicate, "user_id", in.UserID)

	synced, err := s.syncFromAgent(ctx, jobID, agentOp)
	if err != nil {
		// 代理已经接受了操作；这里失败只影响本地显示，留下 needs_sync 等下次对账。
		s.logger.Error("任务状态同步失败（代理已接受，稍后对账）",
			"job_id", jobID, "operation_id", operationID, "error", err)
		job, gerr := s.readJobUnscoped(ctx, jobID)
		if gerr != nil {
			return Job{}, gerr
		}
		return job, nil
	}
	return synced, nil
}

// handleSubmitFailure 处理代理提交失败：业务拒绝与不可用必须分开。
func (s *Service) handleSubmitFailure(ctx context.Context, jobID, operationID string, in SubmitInput, err error) (Job, error) {
	now := s.now().UTC()
	var agentErr *agentclient.Error
	var unavailable *agentclient.UnavailableError
	switch {
	case errors.As(err, &agentErr):
		// 代理明确拒绝（预检过期/失效、短语不符、幂等冲突、不可执行）：任务就此失败，
		// 不重试——重试同一个被拒绝的意图要么重复被拒，要么在原因修好前误执行。
		code := agentErr.Code
		if code == "" {
			code = "INTERNAL_ERROR"
		}
		errJSON, _ := json.Marshal(map[string]any{
			"code":    code,
			"message": agentErr.Message,
		})
		s.logger.Warn("代理拒绝提交",
			"job_id", jobID, "operation_id", operationID, "code", code,
			"user_id", in.UserID, "request_id", in.RequestID)
		job, serr := s.markJobFailed(ctx, jobID, "rejected", errJSON, now)
		if serr != nil {
			s.logger.Error("记录任务失败状态失败", "job_id", jobID, "error", serr)
			return Job{}, serr
		}
		return job, &Rejected{Code: code, Message: agentErr.Message, Job: job}
	case errors.As(err, &unavailable):
		// 连不上/超时：**不知道**代理是否已经接受。任务保持 queued + needs_sync，
		// 由 GET /jobs/:id 按 operation_id 去问——绝不生成新的 operation_id 重新提交。
		s.logger.Warn("提交代理失败，任务保持待对账",
			"job_id", jobID, "operation_id", operationID, "error", err)
		job, gerr := s.readJobUnscoped(ctx, jobID)
		if gerr != nil {
			return Job{}, gerr
		}
		return job, nil
	default:
		return Job{}, err
	}
}

// GetJob 读回任务；需要时向代理对账一次。
//
// “需要时”= 非终态或带待对账标记。列表接口不逐条对账（那会把一次翻页放大成 N 次 socket 调用）。
func (s *Service) GetJob(ctx context.Context, userID int64, jobID string) (Job, error) {
	job, err := s.getJobByID(ctx, jobID, userID)
	if err != nil {
		return Job{}, err
	}
	return s.reconcile(ctx, job), nil
}

// reconcile 用代理视图刷新任务；代理不可用时如实返回本地状态。
//
// 判定逻辑与后台扫描共用同一份（reconcileOne）：两条路径对“代理说 NOT_FOUND”的结论
// 必须一致，否则同一条任务会因为“被读”还是“被扫”得到不同的终态。
func (s *Service) reconcile(ctx context.Context, job Job) Job {
	outcome := s.reconcileOne(ctx, job)
	if outcome == outcomeSettled || outcome == outcomeInflight || outcome == outcomeDeferred {
		return job
	}
	refreshed, err := s.readJobUnscoped(ctx, job.ID)
	if err != nil {
		s.logger.Error("对账后重读任务失败", "job_id", job.ID, "error", err)
		return job
	}
	return refreshed
}

// ListJobs 返回该用户最近的任务摘要。
func (s *Service) ListJobs(ctx context.Context, userID int64, limit int) ([]Job, error) {
	if userID <= 0 {
		return nil, errors.New("operations: UserID 必须为正数")
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.SQL().QueryContext(ctx,
		`SELECT `+jobColumns+` FROM jobs WHERE user_id = ? ORDER BY created_at DESC, id DESC LIMIT ?`,
		userID, limit)
	if err != nil {
		return nil, fmt.Errorf("operations: 读取任务列表失败: %w", err)
	}
	defer rows.Close()
	out := make([]Job, 0, limit)
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, job)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("operations: 读取任务列表失败: %w", err)
	}
	return out, nil
}

// CancelJob 请求安全取消。
//
// 三道门在 Web 侧就挡住：终态、不可取消、无代理。只有过了这三道才去问代理，
// 由代理决定是“标记接受阶段取消”还是“通知执行器停下来”。
func (s *Service) CancelJob(ctx context.Context, userID int64, jobID string) (Job, error) {
	job, err := s.getJobByID(ctx, jobID, userID)
	if err != nil {
		return Job{}, err
	}
	if isTerminalStatus(job.Status) {
		return job, ErrJobFinished
	}
	if !job.Cancellable {
		return job, ErrJobNotCancelable
	}
	if s.agent == nil {
		return job, fmt.Errorf("%w: 未配置代理 socket", ErrAgentUnavailable)
	}
	op, err := s.agent.CancelOperation(ctx, job.OperationID)
	if err != nil {
		var agentErr *agentclient.Error
		if errors.As(err, &agentErr) {
			// 代理明确拒绝（已经结束、不可取消）：如实返回，不假装取消成功。
			return job, &Rejected{Code: agentErr.Code, Message: agentErr.Message, Job: job}
		}
		return job, err
	}
	synced, err := s.syncFromAgent(ctx, jobID, op)
	if err != nil {
		return job, err
	}
	s.logger.Info("已请求取消任务",
		"job_id", jobID, "operation_id", job.OperationID, "status", synced.Status, "user_id", userID)
	return synced, nil
}

// jobColumns 是 jobs 表的读取列，顺序与 scanJob 一致。
const jobColumns = `id, operation_id, preflight_id, user_id, action, status, progress_percent, step,
	       result_json, error_json, cancellable, cancellation_requested, needs_sync,
	       created_at, updated_at, started_at, finished_at`

// getJobByID 读一条任务；不属于该用户时按“不存在”处理。
func (s *Service) getJobByID(ctx context.Context, jobID string, userID int64) (Job, error) {
	job, err := scanJob(s.db.SQL().QueryRowContext(ctx,
		`SELECT `+jobColumns+` FROM jobs WHERE id = ?`, jobID))
	if err != nil {
		return Job{}, err
	}
	if job.UserID != userID {
		return Job{}, ErrJobNotFound
	}
	return job, nil
}

// readJobUnscoped 读一条任务，调用方必须已经确认归属。
func (s *Service) readJobUnscoped(ctx context.Context, jobID string) (Job, error) {
	return scanJob(s.db.SQL().QueryRowContext(ctx,
		`SELECT `+jobColumns+` FROM jobs WHERE id = ?`, jobID))
}

func getJobTx(ctx context.Context, tx *sql.Tx, jobID string) (Job, error) {
	return scanJob(tx.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE id = ?`, jobID))
}

// lookupByIdempotencyKey 按幂等键读回任务（并发重放路径）。
func (s *Service) lookupByIdempotencyKey(ctx context.Context, userID int64, key string) (Job, error) {
	var jobID string
	err := s.db.SQL().QueryRowContext(ctx,
		`SELECT job_id FROM idempotency_keys WHERE user_id = ? AND route = ? AND key = ?`,
		userID, jobRouteSubmit, key).Scan(&jobID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Job{}, ErrJobNotFound
		}
		return Job{}, fmt.Errorf("operations: 读取幂等记录失败: %w", err)
	}
	return s.getJobByID(ctx, jobID, userID)
}

// syncFromAgent 用代理视图更新任务并写一条状态事件。
func (s *Service) syncFromAgent(ctx context.Context, jobID string, op agentclient.Operation) (Job, error) {
	now := s.now().UTC()
	status := statusForStage(op.Stage)
	result := nonEmptyObject(op.Result)
	errJSON := nonEmptyObject(op.Error)
	startedAt := op.StartedAt
	finishedAt := op.FinishedAt

	// 先把代理侧事件抄进 job_events（P03.3），再更新状态。
	//
	// 顺序的理由：事件是**已经发生过的事**，状态是它的结论。先抄事件，客户端在任何时刻
	// 读到“状态已变”时，导致它变化的那几条事件一定已经在库里了；反过来先改状态，
	// 中间那一瞬的 SSE 客户端会看到 succeeded 却还缺 finished 事件。
	//
	// 镜像失败不阻断状态同步：事件是旁支，状态是主干。但必须留下日志——
	// 悄悄丢事件会让任务中心少一段历史，而页面本身不会报错。
	if err := s.MirrorAgentEvents(ctx, jobID, op.OperationID); err != nil {
		s.logger.Warn("镜像代理事件失败", "job_id", jobID, "operation_id", op.OperationID, "error", err)
	}

	// becameTerminal 由事务内的状态迁移置位：本次同步是否把任务从非终态推到了终态。
	var becameTerminal bool
	err := s.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		// 先读旧状态：事件记的是**变化**，不是每次对账的回声。
		var oldStatus, oldStep string
		if err := tx.QueryRowContext(ctx, `SELECT status, step FROM jobs WHERE id = ?`, jobID).
			Scan(&oldStatus, &oldStep); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrJobNotFound
			}
			return fmt.Errorf("operations: 读取任务状态失败: %w", err)
		}
		// 终态不可回退由 jobs_terminal_is_final 触发器兜底；这里的条件更新把意图写明。
		res, err := tx.ExecContext(ctx, `
			UPDATE jobs SET status = ?, step = ?, progress_percent = ?, result_json = ?, error_json = ?,
			       cancellable = ?, cancellation_requested = ?, needs_sync = 0,
			       started_at = CASE WHEN ? <> '' THEN ? ELSE started_at END,
			       finished_at = CASE WHEN ? <> '' THEN ? ELSE finished_at END,
			       updated_at = ?
			 WHERE id = ? AND status NOT IN ('succeeded', 'failed', 'canceled', 'interrupted')`,
			status, op.Step, nullablePercent(op.ProgressPercent), string(result), string(errJSON),
			boolInt(op.Cancellable), boolInt(op.CancellationRequested),
			startedAt, startedAt, finishedAt, finishedAt, formatTime(now), jobID)
		if err != nil {
			return fmt.Errorf("operations: 更新任务状态失败: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("operations: 读取更新结果失败: %w", err)
		}
		if n == 0 || (oldStatus == status && oldStep == op.Step) {
			return nil
		}
		// 只有“从非终态 → 终态”这一次算完成；之后无论对账再看多少遍都不算。
		becameTerminal = isTerminalStatus(status) && !isTerminalStatus(oldStatus)
		return appendJobEventTx(ctx, tx, jobID, "status", map[string]any{
			"status": status,
			"step":   op.Step,
			"from":   oldStatus,
		}, now)
	})
	if err != nil {
		return Job{}, err
	}
	job, err := s.readJobUnscoped(ctx, jobID)
	if err != nil {
		return Job{}, err
	}
	if becameTerminal {
		s.notifyFinished(ctx, job)
	}
	return job, nil
}

// notifyFinished 在任务**刚刚**进入终态后通知 OnJobFinished。
//
// 三条约束写在这里，因为它们都不会在编译期报错：
//   - 用 context.WithoutCancel：写账本的那一笔不该因为浏览器提前断开而消失；
//   - 吞掉 panic：账本写坏了是账本的问题，不能让一个已经成功的任务在对账循环里炸掉；
//   - 只记日志、不改状态：任务已经结束，回调失败不等于任务失败。
func (s *Service) notifyFinished(ctx context.Context, job Job) {
	if s.onJobFinished == nil || !isTerminalStatus(job.Status) {
		return
	}
	detached := context.WithoutCancel(ctx)
	// 参数取不到不算失败：多数动作的消费者根本不需要它，需要它的那一方（证书）
	// 会把"没有参数"当成"这一笔只能记状态，记不了事实"。
	params, err := s.jobParams(detached, job.ID)
	if err != nil {
		s.logger.Warn("读取任务参数失败，回调将拿不到参数",
			"job_id", job.ID, "action", job.Action, "error", err)
		params = nil
	}
	defer func() {
		if r := recover(); r != nil {
			s.logger.Error("任务结束回调 panic", "job_id", job.ID, "action", job.Action, "panic", r)
		}
	}()
	s.onJobFinished(detached, job, params)
}

// jobParams 读取任务消费的那份预检记录里的规范化参数。
//
// 为什么不在提交时把参数抄一份到 jobs：多一份副本就多一个会与预检不一致的来源，
// 而"代理执行的是哪一份参数"这个问题必须只有一个答案。预检记录不随任务结束被删，
// 所以这里随时读得到。
func (s *Service) jobParams(ctx context.Context, jobID string) (json.RawMessage, error) {
	var raw string
	err := s.db.SQL().QueryRowContext(ctx, `
		SELECT p.normalized_params
		  FROM jobs j JOIN preflights p ON p.id = j.preflight_id
		 WHERE j.id = ?`, jobID).Scan(&raw)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("operations: 查询任务参数失败: %w", err)
	}
	if !json.Valid([]byte(raw)) {
		return nil, nil
	}
	return json.RawMessage(raw), nil
}

// markJobFailed 把任务标成失败并记录错误码。
func (s *Service) markJobFailed(ctx context.Context, jobID, step string, errJSON []byte, now time.Time) (Job, error) {
	var changed bool
	err := s.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE jobs SET status = 'failed', step = ?, error_json = ?, needs_sync = 0,
			       finished_at = ?, updated_at = ?
			 WHERE id = ? AND status NOT IN ('succeeded', 'failed', 'canceled', 'interrupted')`,
			step, string(errJSON), formatTime(now), formatTime(now), jobID)
		if err != nil {
			return fmt.Errorf("operations: 标记任务失败失败: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("operations: 读取更新结果失败: %w", err)
		}
		if n == 0 {
			return nil
		}
		changed = true
		return appendJobEventTx(ctx, tx, jobID, "status",
			map[string]any{"status": "failed", "step": step}, now)
	})
	if err != nil {
		return Job{}, err
	}
	job, err := s.readJobUnscoped(ctx, jobID)
	if err != nil {
		return Job{}, err
	}
	if changed {
		s.notifyFinished(ctx, job)
	}
	return job, nil
}

// appendJobEventTx 追加一条任务事件（seq 在任务内单调递增）。
func appendJobEventTx(ctx context.Context, tx *sql.Tx, jobID, kind string, payload map[string]any, now time.Time) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("operations: 事件内容无法序列化: %w", err)
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO job_events (job_id, seq, kind, redacted_payload, created_at)
		SELECT ?, COALESCE(MAX(seq), 0) + 1, ?, ?, ? FROM job_events WHERE job_id = ?`,
		jobID, kind, string(body), formatTime(now), jobID)
	if err != nil {
		return fmt.Errorf("operations: 追加任务事件失败: %w", err)
	}
	return nil
}

// scanJob 把一行结果读成 Job。
func scanJob(row interface{ Scan(...any) error }) (Job, error) {
	var (
		job         Job
		progress    sql.NullInt64
		result      string
		errJSON     string
		cancellable int
		cancelReq   int
		needsSync   int
		createdAt   string
		updatedAt   string
		startedAt   string
		finishedAt  string
	)
	if err := row.Scan(&job.ID, &job.OperationID, &job.PreflightID, &job.UserID, &job.Action,
		&job.Status, &progress, &job.Step, &result, &errJSON, &cancellable, &cancelReq,
		&needsSync, &createdAt, &updatedAt, &startedAt, &finishedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Job{}, ErrJobNotFound
		}
		return Job{}, fmt.Errorf("operations: 读取任务失败: %w", err)
	}
	if progress.Valid {
		v := int(progress.Int64)
		job.ProgressPercent = &v
	}
	job.Result = json.RawMessage(result)
	job.Error = json.RawMessage(errJSON)
	job.Cancellable = cancellable == 1
	job.CancellationRequested = cancelReq == 1
	job.NeedsSync = needsSync == 1
	var err error
	if job.CreatedAt, err = parseStoredTime(createdAt); err != nil {
		return Job{}, err
	}
	if job.UpdatedAt, err = parseStoredTime(updatedAt); err != nil {
		return Job{}, err
	}
	if startedAt != "" {
		if job.StartedAt, err = parseStoredTime(startedAt); err != nil {
			return Job{}, err
		}
	}
	if finishedAt != "" {
		if job.FinishedAt, err = parseStoredTime(finishedAt); err != nil {
			return Job{}, err
		}
	}
	if job.Status == "" {
		return Job{}, errors.New("operations: 任务状态为空")
	}
	return job, nil
}

// validateIdempotencyKey 校验幂等键的形状。
//
// CONTRACTS.md 4.2 要求客户端生成 UUID；这里不强制 UUID，但拒绝空、过长与含控制字符的键：
// 键会进数据库主键并出现在日志字段里，形状必须可判定。
func validateIdempotencyKey(key string) error {
	if key == "" {
		return fmt.Errorf("%w: 缺少 Idempotency-Key 请求头", ErrInvalidIdempotencyKey)
	}
	if len(key) > MaxIdempotencyKeyLength {
		return fmt.Errorf("%w: 超过 %d 字节", ErrInvalidIdempotencyKey, MaxIdempotencyKeyLength)
	}
	for _, r := range key {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: 含控制字符", ErrInvalidIdempotencyKey)
		}
	}
	return nil
}

// canonicalPair 把两个字段拼成稳定的哈希输入（\x00 不会出现在两者内部）。
func canonicalPair(a, b string) string {
	return a + "\x00" + b
}

// withinReauthWindow 判断再认证是否在允许的窗口内。
func withinReauthWindow(at, now time.Time) bool {
	if at.IsZero() {
		return false
	}
	at = at.UTC()
	if at.After(now.Add(reauthClockSkew)) {
		return false
	}
	return now.Sub(at) <= reauthWindow
}

// statusForStage 把代理阶段映射成公开状态。
//
// accepted（持久接受、尚未执行）对用户就是 queued：任务中心展示“排队中”，
// 而 accepted 是代理内部的持久化事实。
func statusForStage(stage string) string {
	switch stage {
	case "accepted":
		return "queued"
	case "running":
		return "running"
	case "succeeded", "failed", "canceled", "interrupted":
		return stage
	default:
		return "queued"
	}
}

// isTerminalStatus 判断公开状态是否终态。
func isTerminalStatus(status string) bool {
	switch status {
	case "succeeded", "failed", "canceled", "interrupted":
		return true
	}
	return false
}

// nonEmptyObject 保证 JSON 列永远是合法对象。
func nonEmptyObject(raw json.RawMessage) json.RawMessage {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || !json.Valid([]byte(trimmed)) {
		return json.RawMessage("{}")
	}
	return raw
}

// nullablePercent 把 *int 转成可写库的值（nil → NULL，表示“不知道”）。
func nullablePercent(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}

// parseStoredTime 解析数据库里的 RFC3339 文本。
func parseStoredTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("operations: 时间列不是 RFC3339: %q", s)
	}
	return t.UTC(), nil
}

// newPrefixedID 生成 prefix + 16 字节随机值的十六进制标识。
func newPrefixedID(prefix string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("operations: 生成标识失败: %w", err)
	}
	return prefix + hex.EncodeToString(b[:]), nil
}

// isUniqueViolation 判断错误是否来自 UNIQUE 约束。
//
// 文本判断是 modernc.org/sqlite 的已知取舍（internal/auth/service.go 同样处理）：
// 驱动没有导出可判定的错误类型。
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}
