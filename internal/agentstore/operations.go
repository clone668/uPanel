package agentstore

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// 执行操作相关的错误。
var (
	// ErrOperationInvalid 表示操作记录本身不合法（编程错误，不是用户输入错误）。
	ErrOperationInvalid = errors.New("agentstore: 操作记录不合法")
	// ErrOperationNotFound 表示没有这个 operation_id。
	ErrOperationNotFound = errors.New("agentstore: 操作不存在")
	// ErrOperationConflict 表示同一个 operation_id 被用于不同的请求。
	ErrOperationConflict = errors.New("agentstore: operation_id 已用于不同的请求")
	// ErrOperationFinished 表示操作已处于终态，不能再改变。
	ErrOperationFinished = errors.New("agentstore: 操作已结束")
	// ErrPreflightExpired 表示预检不存在、已过期或已被别的操作消费。
	ErrPreflightExpired = errors.New("agentstore: 预检不存在、已过期或已被使用")
)

// 操作阶段。
//
// 代理侧没有 queued：Web 的 queued 表示“job 已落库、代理还没接受”，
// 一旦写进 operations 表就是 accepted（持久接受），异步执行开始后才叫 running。
const (
	StageAccepted    = "accepted"
	StageRunning     = "running"
	StageSucceeded   = "succeeded"
	StageFailed      = "failed"
	StageCanceled    = "canceled"
	StageInterrupted = "interrupted"
)

// OperationIDPrefix 是操作标识的固定前缀。
const OperationIDPrefix = "op_"

// ValidOperationID 判断字符串是否是合法的操作标识（op_ + 32 位小写十六进制）。
//
// 代理不生成这个标识，所以必须严格校验：放行任意字符串等于让提交方决定表主键的形状。
func ValidOperationID(id string) bool {
	return hasHexSuffix(id, OperationIDPrefix)
}

// IsTerminalStage 判断阶段是否是终态。
func IsTerminalStage(stage string) bool {
	switch stage {
	case StageSucceeded, StageFailed, StageCanceled, StageInterrupted:
		return true
	}
	return false
}

// Operation 是一次系统操作的代理侧记录（CONTRACTS.md 14.2 operations）。
//
// JSON 列的读取一律给原始字节：执行按预检里那一份规范化参数进行，
// 任何重新序列化都可能让参数哈希对不上。
type Operation struct {
	// ID 是 Web 提交的稳定操作标识（op_<32 位十六进制>）。
	ID string
	// RequestHash 是“这一次请求”的指纹；同一 ID 换个请求再来必须被拒绝。
	RequestHash string
	Action      string
	// PreflightRef 是被消费掉的预检引用（库层面 UNIQUE：一份预检只能换一个操作）。
	PreflightRef        string
	Params              []byte
	ParamsHash          string
	IdentityFingerprint string
	StateVersion        string
	// Target 是目标的展示信息（display_name、identity_fingerprint）。
	// 预检行最终会被清理，执行历史必须能独立说明“改的是哪个对象”。
	Target      []byte
	Locks       []byte
	UserID      int64
	SessionHash string
	RequestID   string
	Stage       string
	Step        string
	Result      []byte
	ErrorJSON   []byte
	// CancellationRequested 是“已请求取消”，不是“已取消”（CONTRACTS.md 4.3）。
	CancellationRequested bool
	CreatedAt             time.Time
	StartedAt             time.Time
	FinishedAt            time.Time
	UpdatedAt             time.Time
}

// OperationEvent 是操作流水中的一条事件。
type OperationEvent struct {
	Seq       int64
	Kind      string
	Payload   []byte
	CreatedAt time.Time
}

// Operations 是执行操作的持久化仓储。
type Operations struct{ db *DB }

// NewOperations 构造仓储。db 为空时返回错误：执行记录是固定能力，不能“看起来在跑”。
func NewOperations(db *DB) (*Operations, error) {
	if db == nil {
		return nil, errors.New("agentstore: 操作仓储需要数据库句柄")
	}
	return &Operations{db: db}, nil
}

// errDuplicateReplay 是事务内的回滚信号：同一个 ID 的同一请求又来了。
var errDuplicateReplay = errors.New("agentstore: duplicate replay")

// Accept 消费预检并写入操作记录，两件事必须在**同一个事务**里完成。
//
// 否则会出现“预检已经不能用，但没有任何人记得为什么”的空洞，或者反过来
// “操作记录存在，但它引用的预检还能被第二个操作消费”。库层的 UNIQUE(preflight_ref)
// 与这里的条件更新是同一件事的两道闸：条件更新决定谁赢，唯一索引保证谁也绕不过去。
//
// duplicate 为 true 表示这个 operation_id 已经用**相同** request_hash 提交过：
// 返回已有记录并回滚事务（预检不会被二次消费）。同一 ID 换了请求则返回
// ErrOperationConflict —— 那是两个不同的意图挤在同一个标识上，不能合并。
func (o *Operations) Accept(ctx context.Context, op Operation) (existing Operation, duplicate bool, err error) {
	if len(op.Locks) == 0 {
		op.Locks = []byte("[]")
	}
	if len(op.Target) == 0 {
		op.Target = []byte("{}")
	}
	if op.Stage == "" {
		op.Stage = StageAccepted
	}
	if err := validateOperation(op); err != nil {
		return Operation{}, false, err
	}
	var found Operation
	err = o.db.store().WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		// 先看这个 operation_id 是否已经存在：重放不该碰预检（它已经被第一次消费掉了），
		// 冲突也不该留下任何痕迹。把重放判断放在消费之前，是“一次预检只换一个操作”
		// 与“同一请求十次只执行一次”能同时成立的原因。
		if prior, e := o.getTx(ctx, tx, op.ID); e == nil {
			if prior.RequestHash == op.RequestHash {
				found = prior
				return errDuplicateReplay
			}
			return ErrOperationConflict
		} else if !errors.Is(e, ErrOperationNotFound) {
			return e
		}

		res, e := tx.ExecContext(ctx,
			`UPDATE preflight_plans SET used_at = ?, used_operation_id = ?
			  WHERE id = ? AND used_at = 0 AND expires_at > ?`,
			millis(op.CreatedAt), op.ID, op.PreflightRef, millis(op.CreatedAt))
		if e != nil {
			return fmt.Errorf("agentstore: 消费预检失败: %w", e)
		}
		n, e := res.RowsAffected()
		if e != nil {
			return fmt.Errorf("agentstore: 读取消费结果失败: %w", e)
		}
		if n == 0 {
			return ErrPreflightExpired
		}

		_, e = tx.ExecContext(ctx, `
			INSERT INTO operations
			  (operation_id, request_hash, action, preflight_ref, params_json, params_hash,
			   identity_fingerprint, state_version, target_json, locks_json, user_id, session_hash, request_id,
			   stage, step, result_json, error_json, cancellation_requested,
			   created_at, started_at, finished_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', '{}', '{}', 0, ?, 0, 0, ?)`,
			op.ID, op.RequestHash, op.Action, op.PreflightRef, string(op.Params), op.ParamsHash,
			op.IdentityFingerprint, op.StateVersion, string(op.Target), string(op.Locks), op.UserID, op.SessionHash,
			op.RequestID, op.Stage, millis(op.CreatedAt), millis(op.UpdatedAt))
		if e != nil {
			if !isUniqueViolation(e) {
				return fmt.Errorf("agentstore: 写入操作记录失败: %w", e)
			}
			// 两种唯一冲突：operation_id（重放或冲突）与 preflight_ref（并发抢同一份预检）。
			prior, getErr := o.getTx(ctx, tx, op.ID)
			if getErr == nil {
				if prior.RequestHash == op.RequestHash {
					found = prior
					return errDuplicateReplay
				}
				return ErrOperationConflict
			}
			if errors.Is(getErr, ErrOperationNotFound) {
				return ErrPreflightExpired
			}
			return getErr
		}
		return appendEventTx(ctx, tx, op.ID, "accepted", map[string]any{
			"action":        op.Action,
			"preflight_ref": op.PreflightRef,
			"stage":         op.Stage,
		}, op.UpdatedAt)
	})
	if errors.Is(err, errDuplicateReplay) {
		return found, true, nil
	}
	if err != nil {
		return Operation{}, false, err
	}
	return op, false, nil
}

// Get 读回一条操作记录。
func (o *Operations) Get(ctx context.Context, id string) (Operation, error) {
	if !ValidOperationID(id) {
		return Operation{}, ErrOperationNotFound
	}
	return scanOperation(o.db.store().SQL().QueryRowContext(ctx, operationSelect+` WHERE operation_id = ?`, id))
}

// FindByPreflight 按预检引用查找操作（复核“这份预检被谁用了”）。
func (o *Operations) FindByPreflight(ctx context.Context, ref string) (Operation, error) {
	if !ValidPlanRef(ref) {
		return Operation{}, ErrOperationNotFound
	}
	return scanOperation(o.db.store().SQL().QueryRowContext(ctx, operationSelect+` WHERE preflight_ref = ?`, ref))
}

// MarkRunning 把操作推进到 running；已经在 running 时是幂等的（补一条 started 之外什么也不改）。
func (o *Operations) MarkRunning(ctx context.Context, id, step string, now time.Time) error {
	op, err := o.Get(ctx, id)
	if err != nil {
		return err
	}
	if op.Stage == StageRunning {
		return nil
	}
	if IsTerminalStage(op.Stage) {
		return ErrOperationFinished
	}
	if op.Stage != StageAccepted {
		return fmt.Errorf("%w: 阶段 %q 不能推进到 running", ErrOperationInvalid, op.Stage)
	}
	if _, err := o.db.store().ExecContext(ctx,
		`UPDATE operations SET stage = ?, step = ?, started_at = ?, updated_at = ?
		  WHERE operation_id = ? AND stage = ?`,
		StageRunning, step, millis(now), millis(now), id, StageAccepted); err != nil {
		return fmt.Errorf("agentstore: 推进操作状态失败: %w", err)
	}
	return o.AppendEvent(ctx, id, "started", map[string]any{"step": step}, now)
}

// Finish 写入终态与结果。
//
// 已是同一终态时返回 nil（重复完成是幂等的）；换成另一个终态则拒绝——
// 终态不可回退，succeeded 也不会变成 failed。
func (o *Operations) Finish(ctx context.Context, id, stage, step string, result, errJSON []byte, now time.Time) error {
	if !IsTerminalStage(stage) {
		return fmt.Errorf("%w: 阶段 %q 不是终态", ErrOperationInvalid, stage)
	}
	if len(result) == 0 {
		result = []byte("{}")
	}
	if len(errJSON) == 0 {
		errJSON = []byte("{}")
	}
	op, err := o.Get(ctx, id)
	if err != nil {
		return err
	}
	if op.Stage == stage {
		return nil
	}
	if IsTerminalStage(op.Stage) {
		return fmt.Errorf("%w: 当前是 %s，不能再改成 %s", ErrOperationFinished, op.Stage, stage)
	}
	if _, err := o.db.store().ExecContext(ctx,
		`UPDATE operations SET stage = ?, step = ?, result_json = ?, error_json = ?,
		        finished_at = ?, updated_at = ? WHERE operation_id = ?`,
		stage, step, string(result), string(errJSON), millis(now), millis(now), id); err != nil {
		return fmt.Errorf("agentstore: 写入终态失败: %w", err)
	}
	kind := "finished"
	switch stage {
	case StageCanceled:
		kind = "canceled"
	case StageFailed, StageInterrupted:
		kind = "error"
	}
	payload := map[string]any{"stage": stage, "step": step}
	var e struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(errJSON, &e) == nil && e.Code != "" {
		payload["error_code"] = e.Code
	}
	return o.AppendEvent(ctx, id, kind, payload, now)
}

// RequestCancel 请求取消一个操作，返回更新后的记录。
//
// accepted 直接变成 canceled：还没开始，停下是安全的。
// running 只把 cancellation_requested 置 1，由执行器在下一个检查点收尾——
// “请求取消”与“已经取消”是两件事，CONTRACTS.md 4.3 要求它们分别可见。
func (o *Operations) RequestCancel(ctx context.Context, id string, now time.Time) (Operation, error) {
	op, err := o.Get(ctx, id)
	if err != nil {
		return Operation{}, err
	}
	if IsTerminalStage(op.Stage) {
		return op, ErrOperationFinished
	}
	if op.Stage == StageAccepted {
		res, err := o.db.store().ExecContext(ctx,
			`UPDATE operations SET stage = ?, step = 'canceled', cancellation_requested = 1,
			        finished_at = ?, updated_at = ? WHERE operation_id = ? AND stage = ?`,
			StageCanceled, millis(now), millis(now), id, StageAccepted)
		if err != nil {
			return Operation{}, fmt.Errorf("agentstore: 取消失败: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return Operation{}, fmt.Errorf("agentstore: 读取取消结果失败: %w", err)
		}
		if n == 0 {
			// 竞态：另一条路径刚刚把它推进起来了。重新读，按 running 处理。
			return o.RequestCancel(ctx, id, now)
		}
		if err := o.AppendEvent(ctx, id, "canceled", map[string]any{"stage": StageCanceled}, now); err != nil {
			return Operation{}, err
		}
		return o.Get(ctx, id)
	}
	if _, err := o.db.store().ExecContext(ctx,
		`UPDATE operations SET cancellation_requested = 1, updated_at = ? WHERE operation_id = ?`,
		millis(now), id); err != nil {
		return Operation{}, fmt.Errorf("agentstore: 记录取消请求失败: %w", err)
	}
	if !op.CancellationRequested {
		if err := o.AppendEvent(ctx, id, "progress",
			map[string]any{"cancellation_requested": true}, now); err != nil {
			return Operation{}, err
		}
	}
	return o.Get(ctx, id)
}

// AppendEvent 追加一条事件（seq 在操作内单调递增）。
func (o *Operations) AppendEvent(ctx context.Context, id, kind string, payload map[string]any, now time.Time) error {
	return appendEventTx(ctx, o.db.store(), id, kind, payload, now)
}

// Events 读回一个操作的全部事件，按 seq 升序。
func (o *Operations) Events(ctx context.Context, id string) ([]OperationEvent, error) {
	rows, err := o.db.store().SQL().QueryContext(ctx,
		`SELECT seq, kind, payload_json, created_at FROM operation_events
		  WHERE operation_id = ? ORDER BY seq ASC`, id)
	if err != nil {
		return nil, fmt.Errorf("agentstore: 读取操作事件失败: %w", err)
	}
	defer rows.Close()
	var out []OperationEvent
	for rows.Next() {
		var (
			ev        OperationEvent
			payload   string
			createdAt int64
		)
		if err := rows.Scan(&ev.Seq, &ev.Kind, &payload, &createdAt); err != nil {
			return nil, fmt.Errorf("agentstore: 读取操作事件失败: %w", err)
		}
		ev.Payload = []byte(payload)
		ev.CreatedAt = fromMillis(createdAt)
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("agentstore: 读取操作事件失败: %w", err)
	}
	return out, nil
}

// ListUnfinished 返回所有非终态的操作，供代理启动时对账（P03.2 只读出来报告，不改写状态）。
func (o *Operations) ListUnfinished(ctx context.Context) ([]Operation, error) {
	rows, err := o.db.store().SQL().QueryContext(ctx,
		operationSelect+` WHERE stage IN (?, ?) ORDER BY created_at ASC`, StageAccepted, StageRunning)
	if err != nil {
		return nil, fmt.Errorf("agentstore: 读取未完成操作失败: %w", err)
	}
	defer rows.Close()
	var out []Operation
	for rows.Next() {
		op, err := scanOperation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, op)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("agentstore: 读取未完成操作失败: %w", err)
	}
	return out, nil
}

// operationSelect 是读操作的固定列序，避免 Get/FindByPreflight/List 各写一套。
const operationSelect = `
	SELECT operation_id, request_hash, action, preflight_ref, params_json, params_hash,
	       identity_fingerprint, state_version, target_json, locks_json, user_id, session_hash, request_id,
	       stage, step, result_json, error_json, cancellation_requested,
	       created_at, started_at, finished_at, updated_at
	  FROM operations`

func (o *Operations) getTx(ctx context.Context, tx *sql.Tx, id string) (Operation, error) {
	return scanOperation(tx.QueryRowContext(ctx, operationSelect+` WHERE operation_id = ?`, id))
}

// rowScanner 让 *sql.Row 与 *sql.Rows 共用一套扫描代码。
type rowScanner interface {
	Scan(dest ...any) error
}

func scanOperation(row rowScanner) (Operation, error) {
	var (
		op                            Operation
		params, target, locks, result string
		errIn                         string
		cancelRequested               int
		createdAt, startedAt          int64
		finishedAt, updatedAt         int64
	)
	err := row.Scan(&op.ID, &op.RequestHash, &op.Action, &op.PreflightRef, &params, &op.ParamsHash,
		&op.IdentityFingerprint, &op.StateVersion, &target, &locks, &op.UserID, &op.SessionHash, &op.RequestID,
		&op.Stage, &op.Step, &result, &errIn, &cancelRequested,
		&createdAt, &startedAt, &finishedAt, &updatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Operation{}, ErrOperationNotFound
		}
		return Operation{}, fmt.Errorf("agentstore: 读取操作记录失败: %w", err)
	}
	op.Params = []byte(params)
	op.Target = []byte(target)
	op.Locks = []byte(locks)
	op.Result = []byte(result)
	op.ErrorJSON = []byte(errIn)
	op.CancellationRequested = cancelRequested == 1
	op.CreatedAt = fromMillis(createdAt)
	op.StartedAt = fromMillis(startedAt)
	op.FinishedAt = fromMillis(finishedAt)
	op.UpdatedAt = fromMillis(updatedAt)
	return op, nil
}

// execer 是 *store.DB 与 *sql.Tx 的公共写入面。
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func appendEventTx(ctx context.Context, ex execer, id, kind string, payload map[string]any, now time.Time) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("agentstore: 事件内容无法序列化: %w", err)
	}
	_, err = ex.ExecContext(ctx, `
		INSERT INTO operation_events (operation_id, seq, kind, payload_json, created_at)
		SELECT ?, COALESCE(MAX(seq), 0) + 1, ?, ?, ? FROM operation_events WHERE operation_id = ?`,
		id, kind, string(body), millis(now), id)
	if err != nil {
		return fmt.Errorf("agentstore: 追加操作事件失败: %w", err)
	}
	return nil
}

// validateOperation 校验操作记录的自洽性（防编程错误，不是防用户输入）。
func validateOperation(op Operation) error {
	if !ValidOperationID(op.ID) {
		return fmt.Errorf("%w: operation_id %q 不合法", ErrOperationInvalid, op.ID)
	}
	if !isHexDigest(op.RequestHash) {
		return fmt.Errorf("%w: request_hash 必须是 64 位十六进制", ErrOperationInvalid)
	}
	if strings.TrimSpace(op.Action) == "" {
		return fmt.Errorf("%w: 缺少 action", ErrOperationInvalid)
	}
	if !ValidPlanRef(op.PreflightRef) {
		return fmt.Errorf("%w: preflight_ref %q 不合法", ErrOperationInvalid, op.PreflightRef)
	}
	if len(op.Params) == 0 {
		return fmt.Errorf("%w: 缺少规范化参数", ErrOperationInvalid)
	}
	if !isHexDigest(op.ParamsHash) {
		return fmt.Errorf("%w: params_hash 必须是 64 位十六进制", ErrOperationInvalid)
	}
	if op.UserID <= 0 {
		return fmt.Errorf("%w: user_id 必须为正数", ErrOperationInvalid)
	}
	if !isHexDigest(op.SessionHash) {
		return fmt.Errorf("%w: session_hash 必须是 64 位十六进制", ErrOperationInvalid)
	}
	if len(op.Locks) > 0 && !json.Valid(op.Locks) {
		return fmt.Errorf("%w: locks 必须是合法 JSON", ErrOperationInvalid)
	}
	if len(op.Target) > 0 && !json.Valid(op.Target) {
		return fmt.Errorf("%w: target 必须是合法 JSON", ErrOperationInvalid)
	}
	if op.Stage != StageAccepted && op.Stage != StageRunning && !IsTerminalStage(op.Stage) {
		return fmt.Errorf("%w: 阶段 %q 不合法", ErrOperationInvalid, op.Stage)
	}
	if op.CreatedAt.IsZero() || op.UpdatedAt.IsZero() {
		return fmt.Errorf("%w: 操作必须有创建时间", ErrOperationInvalid)
	}
	if op.UpdatedAt.Before(op.CreatedAt) {
		return fmt.Errorf("%w: 更新时间早于创建时间", ErrOperationInvalid)
	}
	return nil
}

// hasHexSuffix 判断 s 是否为 prefix + 32 位小写十六进制。
func hasHexSuffix(s, prefix string) bool {
	if !strings.HasPrefix(s, prefix) || len(s) != len(prefix)+32 {
		return false
	}
	body := s[len(prefix):]
	if strings.ToLower(body) != body {
		return false
	}
	decoded, err := hex.DecodeString(body)
	return err == nil && len(decoded) == 16
}
