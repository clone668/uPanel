// Package audit 写入追加式审计事件（CONTRACTS.md 第 14.1 节 audit_events）。
//
// P02.2 起记录账号管理动作，P02.4 起与业务变更共享同一事务，并记录被拒绝的高风险请求。
// 两条硬约束在这里落地：
//   - 只写不读改：本包没有 Update/Delete，数据库触发器也会拒绝 UPDATE/DELETE；
//   - 秘密不入库：Details 按键名过滤，口令、令牌、哈希类字段一律替换为 "[redacted]"。
package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"upanel/internal/store"
)

// 动作名。命名与接口路径对应，P02.4 扩展时沿用同一命名法（<域>.<对象>.<动作>）。
const (
	// ActionUserCreate 是管理员创建账号。
	ActionUserCreate = "user.create"
	// ActionUserUpdate 是修改显示名、角色或启用状态。
	ActionUserUpdate = "user.update"
	// ActionUserPasswordReset 是重置他人密码（会同时作废目标会话）。
	ActionUserPasswordReset = "user.password.reset"
	// ActionUserDelete 是删除账号。
	ActionUserDelete = "user.delete"
	// ActionUserManage 用于账号管理请求被中间件拒绝（admin/CSRF/再认证）时记录的事件：
	// 请求没有走到具体动作就结束了，用路由组的动作名比不记更诚实。
	ActionUserManage = "user.manage"
	// ActionAuthReauth 是密码再认证（成功与失败都记）。
	ActionAuthReauth = "auth.reauth"
	// ActionOperationPreflight 是生成一份预检计划（P03.1）。
	//
	// 预检不改变系统状态，但它是“准备执行一个危险动作”的明确意图：谁、为哪个动作、
	// 代理是否允许、要满足哪些条件，都值得留痕。
	ActionOperationPreflight = "operation.preflight"
	// ActionJobSubmit 是提交一次系统写任务（P03.2）。
	//
	// 这是真正产生副作用的入口：记下 job_id、动作名、预检引用与提交后的状态，
	// 事后才能回答“这次格式化是谁在什么时候提交的”。
	ActionJobSubmit = "job.submit"
	// ActionJobCancel 是请求取消任务。
	ActionJobCancel = "job.cancel"
	// ActionJobManage 用于任务请求被中间件拒绝（管理员角色/CSRF）时记录的事件。
	ActionJobManage = "job.manage"
)

// 结果枚举，与迁移里的 CHECK 约束一致。
const (
	OutcomeSuccess = "success"
	OutcomeFailure = "failure"
)

// redactedValue 是秘密字段的替代文本。
const redactedValue = "[redacted]"

// secretKeyFragments 命中即视为秘密字段。宁可按名字多拦，也不把秘密写进审计。
var secretKeyFragments = []string{
	"password", "passwd", "secret", "token", "hash", "cookie", "credential", "private_key",
}

// ErrInvalidEvent 表示事件本身不合法（编程错误，不是用户输入错误）。
var ErrInvalidEvent = errors.New("audit: 事件不合法")

// Event 是一条审计事件。
type Event struct {
	// ActorID 是操作者账号 id；0 表示没有账号主体（例如系统任务），写为 NULL。
	ActorID int64
	// ActorSnapshot 是操作者的文本快照（用户名与角色）：账号被删除后仍然可读。
	ActorSnapshot string
	// Action 是动作名，见上面的常量。
	Action string
	// Target 是作用对象，例如 "user:3"。
	Target string
	// JobID 是这次事件关联的任务 id（jobs.id）；不关联任务时为空，写库为 NULL。
	//
	// 单独一列而不是只放进 Details：迁移里就是这么建的（audit_events.job_id），
	// 事后要回答“这次失败的任务是谁提交的”，靠的是一列可索引的键，不是字符串匹配。
	JobID string
	// RequestID 是本次 HTTP 请求的 request_id，便于与日志对齐。
	RequestID string
	// Outcome 是 success 或 failure。
	Outcome string
	// Details 只放非秘密字段；写入时会再按字段名过滤一遍。
	Details map[string]any
}

// Writer 写入审计事件。
type Writer struct {
	db  *store.DB
	now func() time.Time
}

// New 构造 Writer。db 为空时返回错误：没有数据库就不能“假装记了审计”。
func New(db *store.DB, now func() time.Time) (*Writer, error) {
	if db == nil {
		return nil, errors.New("audit: 数据库句柄不能为空")
	}
	if now == nil {
		now = time.Now
	}
	return &Writer{db: db, now: now}, nil
}

// execer 是 *sql.DB 与 *sql.Tx 的公共写入子集。
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// Record 追加一条审计事件。
//
// **事务归属**：如果 ctx 由 store.WithTx 产生（业务事务），事件写在同一个事务里——
// 业务回滚则审计一起消失，业务提交则审计必然存在。没有事务时（例如记录被拒绝的请求）
// 直接用数据库连接写，独立提交。
func (w *Writer) Record(ctx context.Context, ev Event) error {
	if strings.TrimSpace(ev.Action) == "" {
		return fmt.Errorf("%w: 缺少 action", ErrInvalidEvent)
	}
	if ev.Outcome != OutcomeSuccess && ev.Outcome != OutcomeFailure {
		return fmt.Errorf("%w: 未知 outcome %q", ErrInvalidEvent, ev.Outcome)
	}
	actor := strings.TrimSpace(ev.ActorSnapshot)
	if actor == "" {
		actor = "system"
	}

	details, err := marshalDetails(ev.Details)
	if err != nil {
		return err
	}

	var actorID any
	if ev.ActorID > 0 {
		actorID = ev.ActorID
	}
	var jobID any
	if id := strings.TrimSpace(ev.JobID); id != "" {
		jobID = id
	}
	// 有业务事务就跟着走，否则独立写（被拒绝的请求没有业务事务可依附）。
	var target execer = w.db.SQL()
	if tx, ok := store.TxFromContext(ctx); ok {
		target = tx
	}

	_, err = target.ExecContext(ctx, `
		INSERT INTO audit_events (actor_id, actor_snapshot, action, target, request_id, job_id, outcome, redacted_details, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		actorID, actor, ev.Action, ev.Target, ev.RequestID, jobID, ev.Outcome, details,
		w.now().UTC().Truncate(time.Second).Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("audit: 写入审计事件失败: %w", err)
	}
	return nil
}

// JobIDFromTarget 从 "job:<id>" 这样的 target 里取出任务 id；不是任务对象时返回空串。
//
// 任务相关的事件都用这个 target 形态（job.submit、job.cancel、以及被拒绝的任务请求），
// 规则只有这一处，不必让每个调用点各传一遍。
func JobIDFromTarget(target string) string {
	const prefix = "job:"
	if !strings.HasPrefix(target, prefix) {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(target, prefix))
}

// marshalDetails 序列化明细，并把秘密字段替换掉。
func marshalDetails(details map[string]any) (string, error) {
	if len(details) == 0 {
		return "{}", nil
	}
	safe := make(map[string]any, len(details))
	for k, v := range details {
		if isSecretKey(k) {
			safe[k] = redactedValue
			continue
		}
		safe[k] = v
	}
	buf, err := json.Marshal(safe)
	if err != nil {
		return "", fmt.Errorf("audit: 审计明细无法序列化: %w", err)
	}
	return string(buf), nil
}

func isSecretKey(key string) bool {
	lower := strings.ToLower(key)
	for _, frag := range secretKeyFragments {
		if strings.Contains(lower, frag) {
			return true
		}
	}
	return false
}
