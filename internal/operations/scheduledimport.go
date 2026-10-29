package operations

// 定时操作的对账导入（P12.4）。
//
// 定时触发那条路径面板没有参与：systemd 调 upanelctl，root 的 CLI 直接打代理的
// 专用 trigger 路由，代理自己算 operation_id、自己预检、自己执行。CONTRACTS §15 因此
// 要求"代理产生的定时 operation 会被 Web 对账导入任务表，不能只存在于 journal"。
// 这就是那条路的面板端。
//
// 为什么要"导入"而不是"实时记一笔"：面板没有见到那次触发，它只能反过来问代理。
// 反过来说，**面板不是真相**——导入的每一条都在 needs_sync=1 上，下一轮对账会按
// operation_id 回代理核对，状态往前推。导入只负责"让它出现在任务中心"，不负责
// 断言它现在是什么阶段。
//
// 两条表约束决定了这里的形状，不是设计偏好：
//   - jobs.preflight_id NOT NULL REFERENCES preflights：所以每条导入都要落一条本地
//     预检记录。这条预检是**如实抄写**代理那份（引用、参数、哈希、目标、会话哈希、
//     到期时间都来自代理），不是凭空造一张单据；id 由 operation 的预检引用派生，
//     重复导入得到同一个 id。
//   - jobs.user_id NOT NULL REFERENCES users：定时触发的操作者是一个保留值
//     （schedules.SystemUserID），所以 users 里必须有一行。那一行永远不会被登录：
//     disabled=1、password_hash 是不可能匹配的标记，并且账号管理把它当不存在
//     （见 auth 的账号列表与写操作的守卫）。
//
// 幂等：operation_id 在 jobs 上 UNIQUE，job id 由 operation_id 派生。同一批导入
// 跑十次只会有一条记录，重试不需要额外状态。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"upanel/internal/agentclient"
	"upanel/internal/schedules"
)

const (
	// scheduledImportBatch 是一次导入向代理要的条数。
	//
	// 与代理侧的封顶（agentstore.MaxActorsBatch）一致：定时任务的积压通常是"Web 停了
	// 几天"级别的个位数，100 足够；再多也只是把 socket 当成批量搬运通道。
	scheduledImportBatch = 100

	// systemUsername 是保留账号的登录名。它不会被任何人用到——账号列表里看不到它，
	// 登录时 disabled 直接拒绝。
	systemUsername = "scheduler"

	// systemPasswordMarker 是不可校验的密码标记。
	//
	// 存一个不可能通过 Argon2id 校验的值，而不是空串：空串在某些实现里会被当成
	// "未设置密码"的合法输入，一个标记则明确是"这里没有密码"。
	systemPasswordMarker = "!system-account-no-password"

	// systemUserDisplay 是保留账号在任务详情里的显示名。
	systemUserDisplay = "定时任务"
)

// ImportStats 是一次导入的结果，字段都是计数，便于日志与验收断言。
type ImportStats struct {
	// Asked 是从代理读到的操作条数。
	Asked int
	// Imported 是本次新落库的任务数。
	Imported int
	// Skipped 是已经导入过的条数（同一 operation_id）。
	Skipped int
	// Deferred 表示本轮没做成（代理不可用），留待下一轮。
	Deferred bool
}

// ImportScheduled 把代理侧"定时触发产生的操作"导入任务表。
//
// 调用点是对账扫描（SweepPending）与本进程启动时的那一次扫描：定时操作不会因为
// 管理员打开页面才存在，所以不能等页面来触发导入。
//
// 返回错误只表示"扫描本身跑不起来"（读库失败）。代理不可用不算错误——那是
// Deferred，下一轮再说；把一次网络抖动报成错误会让后台循环的日志变成噪声。
func (s *Service) ImportScheduled(ctx context.Context, limit int) (ImportStats, error) {
	var stats ImportStats
	if s.agent == nil {
		return stats, nil
	}
	if limit <= 0 || limit > scheduledImportBatch {
		limit = scheduledImportBatch
	}

	since, err := s.scheduledWatermark(ctx)
	if err != nil {
		return stats, err
	}
	ops, err := s.agent.ListScheduledOperations(ctx, since, limit)
	if err != nil {
		// 代理不可用是常态（重启、升级、磁盘忙），不在这里报错。
		stats.Deferred = true
		s.logger.Warn("导入定时操作：代理暂时不可用", "error", err)
		return stats, nil
	}
	stats.Asked = len(ops)

	for _, op := range ops {
		imported, err := s.importOneScheduled(ctx, op)
		if err != nil {
			// 单条失败不中止整轮：一条坏记录不该让其余几十条永远进不了任务中心。
			// 但要说清是哪一条——这正是"不能只存在于 journal"要防的那种沉默。
			s.logger.Error("导入定时操作失败", "operation_id", op.OperationID, "error", err)
			continue
		}
		if imported {
			stats.Imported++
			// 导入的那一刻这条操作可能**早就跑完了**（触发两秒结束，对账三十秒后才
			// 把它导进任务表），于是它一进库就是终态，永远不会有"从非终态推进到
			// 终态"的那一下去触发 OnJobFinished。结果是：定时续期真的换了证书，
			// 面板账本却还停在续期之前——指纹与到期时间都是旧的。
			//
			// 所以终态的导入要自己补这一次回调。只补一次：导入对同一条 operation_id
			// 是幂等的，下一轮会走 Skipped 分支。
			s.notifyImportedTerminal(ctx, op)
		} else {
			stats.Skipped++
		}
	}
	if stats.Imported > 0 {
		s.logger.Info("已导入定时操作", "imported", stats.Imported,
			"skipped", stats.Skipped, "asked", stats.Asked)
	}
	return stats, nil
}

// notifyImportedTerminal 给"导入时就已经结束"的任务补一次结束回调。
//
// 没结束的任务不用管：它之后由对账推进到终态时，syncFromAgent 会照常触发回调。
// 这里只处理那一种被时间差漏掉的形状，并且只对终态做。
func (s *Service) notifyImportedTerminal(ctx context.Context, op agentclient.ScheduledOperation) {
	status, ok := jobStatusOfStage(op.Stage)
	if !ok || !isTerminalStatus(status) {
		return
	}
	if s.onJobFinished == nil {
		return
	}
	job, err := s.readJobUnscoped(ctx, derivedScheduledJobID(op.OperationID))
	if err != nil {
		// 刚落库的行读不出来：不正常，但不能让它吞掉整轮导入。
		s.logger.Error("导入的定时操作读不出任务记录，结束回调未触发",
			"operation_id", op.OperationID, "stage", op.Stage, "error", err)
		return
	}
	s.logger.Info("导入的定时操作导入时已结束，补一次结束回调",
		"job_id", job.ID, "operation_id", op.OperationID,
		"action", job.Action, "status", job.Status)
	s.notifyFinished(ctx, job)
}

// scheduledWatermark 返回"已经导入到哪儿"。
//
// 水位从任务表本身推出来（定时操作的任务 created_at 就是代理那条操作的创建时间），
// 不另立一张状态表：多一份状态就多一处会与事实不符的地方。
//
// 用 >= 而不是 >：同一毫秒可能有多条，靠 operation_id 的 UNIQUE 去重，
// 重复读一条是便宜的，漏掉一条是永久的。
func (s *Service) scheduledWatermark(ctx context.Context) (time.Time, error) {
	var raw string
	err := s.db.SQL().QueryRowContext(ctx,
		`SELECT COALESCE(MAX(created_at), '') FROM jobs WHERE user_id = ?`,
		schedules.SystemUserID).Scan(&raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("operations: 读取定时任务水位失败: %w", err)
	}
	if raw == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("operations: 定时任务水位的格式不对（%q）: %w", raw, err)
	}
	return t, nil
}

// importOneScheduled 把一条定时操作落成"预检 + 任务"，返回是否新导入。
func (s *Service) importOneScheduled(ctx context.Context, op agentclient.ScheduledOperation) (bool, error) {
	if op.OperationID == "" || op.Action == "" || op.PreflightRef == "" {
		return false, errors.New("代理返回的定时操作缺少 operation_id / action / preflight_ref")
	}
	status, ok := jobStatusOfStage(op.Stage)
	if !ok {
		return false, fmt.Errorf("代理返回了未定义的阶段 %q", op.Stage)
	}

	jobID := derivedScheduledJobID(op.OperationID)
	preflightID := derivedScheduledPreflightID(op.PreflightRef)

	imported := false
	err := s.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var exists int
		err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE operation_id = ?`, op.OperationID).Scan(&exists)
		if err != nil {
			return fmt.Errorf("operations: 检查定时操作是否已导入失败: %w", err)
		}
		if exists > 0 {
			return nil
		}

		now := s.now().UTC()
		if err := ensureSystemUserTx(ctx, tx, now); err != nil {
			return err
		}
		if err := insertScheduledPreflightTx(ctx, tx, op, preflightID, jobID, s.now().UTC()); err != nil {
			return err
		}
		if err := insertScheduledJobTx(ctx, tx, op, preflightID, jobID, status, now); err != nil {
			return err
		}
		imported = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return imported, nil
}

// ensureSystemUserTx 保证保留账号存在。
//
// 不写在迁移里，是因为它会改变 `CreateFirstAdmin` 看到的账号数（那种"数据库里明明
// 一个账号都没有，却不让创建第一个管理员"的坑）。按需创建没有这个问题：能走到这里
// 说明系统已经在跑定时任务了。
func ensureSystemUserTx(ctx context.Context, tx *sql.Tx, now time.Time) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO users (id, username, display_name, password_hash, role, disabled, version, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'viewer', 1, 1, ?, ?)
		ON CONFLICT DO NOTHING`,
		schedules.SystemUserID, systemUsername, systemUserDisplay, systemPasswordMarker,
		formatTime(now), formatTime(now))
	if err != nil {
		return fmt.Errorf("operations: 创建定时任务保留账号失败: %w", err)
	}
	// ON CONFLICT DO NOTHING 会让"登录名已被某个真实账号占了"静默通过，而下一步的
	// 外键就会以一个看不懂的方式失败。这里把话说清楚：保留账号必须真的存在。
	var id int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM users WHERE username = ?`, systemUsername).Scan(&id); err != nil {
		return fmt.Errorf("operations: 读取定时任务保留账号失败: %w", err)
	}
	if id != schedules.SystemUserID {
		return fmt.Errorf("operations: 登录名 %q 已被账号 %d 占用，定时任务的保留账号无法建立", systemUsername, id)
	}
	return nil
}

// insertScheduledPreflightTx 落一条本地预检记录（如实抄写代理那份）。
func insertScheduledPreflightTx(ctx context.Context, tx *sql.Tx, op agentclient.ScheduledOperation, preflightID, jobID string, now time.Time) error {
	params := op.Params
	if len(params) == 0 {
		params = []byte("{}")
	}
	target := op.Target
	if len(target) == 0 {
		target = []byte("{}")
	}
	created := op.CreatedAt.UTC()
	if created.IsZero() {
		created = now
	}
	// 到期时间沿用契约里的 120 秒：这条预检早就被代理消费掉了（used_at 已置），
	// 本地这行的 expires_at 只是为了让记录自洽，不参与任何判定。
	expires := created.Add(PreflightTTL)
	_, err := tx.ExecContext(ctx, `
		INSERT INTO preflights
		  (id, user_id, session_hash, action, params_hash, normalized_params, target_json,
		   state_version, agent_ref, allowed, requires_reauth, requires_phrase,
		   created_at, expires_at, consumed_job_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1, 0, 0, ?, ?, ?)`,
		preflightID, schedules.SystemUserID, op.SessionHash, op.Action, op.ParamsHash,
		string(params), string(target), op.StateVersion, op.PreflightRef,
		formatTime(created), formatTime(expires), jobID)
	if err != nil {
		if isUniqueViolation(err) {
			// 派生 id 撞车（同一份预检引用被两条操作引用，代理库不允许）：
			// 说明代理侧的数据不自洽，如实报出来，别把它吞成"已导入"。
			return fmt.Errorf("operations: 定时预检记录已存在（agent_ref=%s）: %w", op.PreflightRef, err)
		}
		return fmt.Errorf("operations: 保存定时预检记录失败: %w", err)
	}
	return nil
}

// insertScheduledJobTx 落一条任务记录。
//
// needs_sync=1：导入只负责"让它出现在任务中心"，阶段是不是真的推进过由下一轮对账
// 回代理核对。cancellable=0：定时操作没有一个人在键盘前做过再认证，面板不提供
// 取消入口（要停就停计划本身，或用 root 在本机处理）。
func insertScheduledJobTx(ctx context.Context, tx *sql.Tx, op agentclient.ScheduledOperation, preflightID, jobID, status string, now time.Time) error {
	result := op.Result
	if len(result) == 0 {
		result = []byte("{}")
	}
	errJSON := op.Error
	if len(errJSON) == 0 {
		errJSON = []byte("{}")
	}
	created := op.CreatedAt.UTC()
	if created.IsZero() {
		created = now
	}
	updated := op.UpdatedAt.UTC()
	if updated.IsZero() {
		updated = created
	}
	started, finished := "", ""
	if !op.StartedAt.IsZero() {
		started = formatTime(op.StartedAt.UTC())
	}
	if !op.FinishedAt.IsZero() {
		finished = formatTime(op.FinishedAt.UTC())
	}

	_, err := tx.ExecContext(ctx, `
		INSERT INTO jobs
		  (id, operation_id, preflight_id, user_id, action, status, progress_percent, step,
		   result_json, error_json, cancellable, cancellation_requested, needs_sync,
		   created_at, updated_at, started_at, finished_at)
		VALUES (?, ?, ?, ?, ?, ?, NULL, ?, ?, ?, 0, 0, 1, ?, ?, ?, ?)`,
		jobID, op.OperationID, preflightID, schedules.SystemUserID, op.Action, status, op.Step,
		string(result), string(errJSON), formatTime(created), formatTime(updated), started, finished)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("operations: 定时任务已存在（operation_id=%s）: %w", op.OperationID, err)
		}
		return fmt.Errorf("operations: 保存定时任务失败: %w", err)
	}
	return appendJobEventTx(ctx, tx, jobID, "status", map[string]any{
		"status": status,
		"step":   op.Step,
		// 这条事件的来源要写清：不是面板提交的，是从代理对账导入的。
		"source":      "scheduled_import",
		"preflight":   op.PreflightRef,
		"imported_at": formatTime(now),
	}, now)
}

// jobStatusOfStage 把代理的阶段翻成任务状态。
//
// accepted → queued：代理接受了但还没开始跑，和面板提交后的那一刻是同一个状态。
// 其余一一对应（两边的闭集是同一套词）。
func jobStatusOfStage(stage string) (string, bool) {
	switch stage {
	case "accepted":
		return "queued", true
	case "running":
		return "running", true
	case "succeeded":
		return "succeeded", true
	case "failed":
		return "failed", true
	case "canceled":
		return "canceled", true
	case "interrupted":
		return "interrupted", true
	}
	return "", false
}

// derivedScheduledJobID 由 operation_id 派生任务 id。
//
// 派生而不是随机：同一条操作无论导入几次都是同一个任务 id，UNIQUE 之外又多一道
// 天然的一致性——不会出现"两条任务指向同一个 operation_id"的候选。
func derivedScheduledJobID(operationID string) string {
	return jobIDPrefix + hashHex([]byte("scheduled-job\n" + operationID))[:32]
}

// derivedScheduledPreflightID 由代理的预检引用派生本地预检 id。
func derivedScheduledPreflightID(agentRef string) string {
	return preflightIDPrefix + hashHex([]byte("scheduled-preflight\n" + agentRef))[:32]
}
