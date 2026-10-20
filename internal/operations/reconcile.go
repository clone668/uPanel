package operations

// P03.4 Web 侧：重启对账与“执行结果未知”的处理。
//
// 两个数据库不是分布式事务（CONTRACTS.md §4.3）：代理写下了最终结果，Web 可能还没抄到。
// 这类任务在库里带 needs_sync=1 或是非终态。它们不能靠“用户下次打开页面”来解决——
// 没人打开就永远不同步。所以这里有一个扫描器：
//
//	启动时立刻扫一遍（进程刚起来，可能有上一轮留下的待对账任务）
//	之后每 ReconcileInterval 一次，直到进程停止
//
// 三条铁律：
//  1. **只查不跑**。对账只调 GET /internal/v1/operations/:id，绝不重新提交，绝不生成新的
//     operation_id。代理说没这条操作时，那就是一次“没有执行”，不是一个“再执行一次”的理由。
//  2. **提交途中不判定**。任务已经落库、CreateOperation 还在返回路上的那几秒里，代理确实
//     还没有这条操作。`inflight` 就是用来把这段窗口排除掉的：否则一个正在跑的动作会被
//     对账写成 failed，而磁盘其实已经动过了。
//  3. **结果未知要说未知**。代理曾经确认过这条操作（needs_sync 已经被清成 0），现在却查不到，
//     说明代理那份记录丢了——此时不能写 failed（那是在断言“没执行”），只能写 interrupted，
//     并在错误体里明确要求人工核对实际状态。CONTRACTS.md §4.3 的状态机里 interrupted 就是
//     为这种情况存在。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"upanel/internal/agentclient"
)

const (
	// ReconcileInterval 是后台对账的扫描间隔。
	//
	// 30 秒的取舍：待对账任务通常是零到几条，一次扫描就是几条 socket 调用，代价可以忽略；
	// 而用户刚关掉浏览器就重启了 Web 时，最多半分钟任务中心就会补齐。更长会让“页面看着
	// 停在运行中”的时间变长，更短只是白白查库。
	ReconcileInterval = 30 * time.Second

	// ReconcileBatch 是一次扫描最多处理的任务数。
	//
	// 有上限是因为这里没有分页游标：积压很多时，靠“下一轮从最旧的开始”自然推进。
	// 排序用 updated_at 升序，最久没人管的任务先被处理。
	ReconcileBatch = 100

	// IdempotencyPurgeInterval 是清理过期幂等记录的间隔。
	//
	// 幂等记录保留 24 小时（idempotencyTTL）。清得太勤没有意义：过期记录本身只是几十字节，
	// 而每小时一次足够让表不无限长。**清理必须跳过“任务还没结束”的记录**——
	// 删掉一个正在执行的任务的幂等键，用户重试就会得到第二次执行。
	IdempotencyPurgeInterval = time.Hour
)

// SweepStats 是一次对账扫描的结果。字段都是计数，便于日志与验收断言。
type SweepStats struct {
	// Scanned 是本次真正向代理查询过的任务数。
	Scanned int
	// Advanced 是状态或步骤被推进的任务数。
	Advanced int
	// Deferred 是代理暂时不可用、留待下次的任务数。
	Deferred int
	// Unconfirmed 是“代理从未见过这条操作”的任务数（提交没到达）。
	Unconfirmed int
	// Unknown 是“代理曾经确认过、现在查不到”的任务数（结果未知）。
	Unknown int
	// Failed 是本地写库失败的任务数。
	Failed int
	// SkippedInflight 是提交还在途中、本次刻意跳过的任务数。
	SkippedInflight int
}

// settled 表示这次扫描什么都没做（包括“跳过提交途中”的情形按需归入 Deferred）。
func (s SweepStats) settled() bool {
	return s.Advanced == 0 && s.Unconfirmed == 0 && s.Unknown == 0 && s.Failed == 0 && s.Deferred == 0
}

// reconcileOutcome 是一次单任务对账的结论，只用于计数与日志。
type reconcileOutcome int

const (
	outcomeSettled     reconcileOutcome = iota // 无需改动（终态或代理视图与本地一致）
	outcomeAdvanced                            // 状态/步骤前进了一步
	outcomeDeferred                            // 代理暂时不可用（连不上/超时）
	outcomeInflight                            // 提交仍在途中，本轮刻意不问
	outcomeUnconfirmed                         // 代理从未见过这条操作
	outcomeUnknown                             // 代理曾经见过、现在查不到：结果未知
	outcomeWriteFailed                         // 本地写库失败
)

// SweepPending 扫描所有非终态或未同步的任务，按 operation_id 向代理对账。
//
// 返回错误只表示“扫描本身跑不起来”（读库失败）；单个任务的失败只计数，不中止整轮——
// 一条坏记录不该让其余几十条任务永远得不到对账。
func (s *Service) SweepPending(ctx context.Context, limit int) (SweepStats, error) {
	var stats SweepStats
	if limit <= 0 || limit > ReconcileBatch {
		limit = ReconcileBatch
	}
	if s.agent == nil {
		return stats, nil
	}

	s.sweepMu.Lock()
	defer s.sweepMu.Unlock()

	// 先把 id 取干净再逐条处理：modernc sqlite 的连接池很小，一边遍历游标一边在同一连接池上
	// 开写事务会互相等。分两步也让“扫描范围”在整轮里稳定，不受本轮自己的写影响。
	rows, err := s.db.SQL().QueryContext(ctx, `
		SELECT id FROM jobs
		 WHERE needs_sync = 1 OR status NOT IN ('succeeded', 'failed', 'canceled', 'interrupted')
		 ORDER BY updated_at ASC, id ASC
		 LIMIT ?`, limit)
	if err != nil {
		return stats, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return stats, err
		}
		ids = append(ids, id)
	}
	scanErr := rows.Err()
	rows.Close()
	if scanErr != nil {
		return stats, scanErr
	}

	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		job, err := s.readJobUnscoped(ctx, id)
		if err != nil {
			// 任务在这一轮扫描期间被删掉了或读不出来：计入失败但继续。
			s.logger.Warn("对账读取任务失败", "job_id", id, "error", err)
			stats.Failed++
			continue
		}
		outcome := s.reconcileOne(ctx, job)
		if outcome == outcomeInflight {
			stats.SkippedInflight++
			continue
		}
		stats.Scanned++
		switch outcome {
		case outcomeAdvanced:
			stats.Advanced++
		case outcomeDeferred:
			stats.Deferred++
		case outcomeUnconfirmed:
			stats.Unconfirmed++
		case outcomeUnknown:
			stats.Unknown++
		case outcomeWriteFailed:
			stats.Failed++
		}
	}
	return stats, nil
}

// RunReconciler 启动时扫一遍，之后每 ReconcileInterval 扫一遍，直到 ctx 结束。
//
// 调用方用 goroutine 起它，并在收到停止信号时取消 ctx：扫描本身尊重 ctx，
// 进程退出不会被一条慢查询拖住。
func (s *Service) RunReconciler(ctx context.Context) {
	if s.agent == nil {
		// mock 模式与未配置 socket 时没有可对账的对象。说清楚，而不是让一条静默的循环跑着。
		s.logger.Info("未配置代理，跳过任务对账")
		return
	}
	s.logger.Info("开始任务对账", "interval", ReconcileInterval.String(), "batch", ReconcileBatch)
	s.sweepOnce(ctx, true)

	ticker := time.NewTicker(ReconcileInterval)
	defer ticker.Stop()
	purgeTicker := time.NewTicker(IdempotencyPurgeInterval)
	defer purgeTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sweepOnce(ctx, false)
		case <-purgeTicker.C:
			s.purgeOnce(ctx)
		}
	}
}

// purgeOnce 清理到期的幂等记录（CONTRACTS.md §14.2：至少保留 24 小时）。
//
// 只删“任务已经结束”的记录：幂等键存在的意义是“同一个意图只执行一次”，
// 而一个还没结束的任务随时可能被重试，删掉它的键等于把重试变成第二次执行。
func (s *Service) purgeOnce(ctx context.Context) {
	removed, err := s.PurgeExpiredIdempotencyKeys(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.logger.Warn("清理过期幂等记录失败", "error", err)
		}
		return
	}
	if removed > 0 {
		s.logger.Info("已清理过期幂等记录", "count", removed)
	}
}

// PurgeExpiredIdempotencyKeys 删除已过期且任务已结束的幂等记录，返回删除条数。
func (s *Service) PurgeExpiredIdempotencyKeys(ctx context.Context) (int, error) {
	now := s.now().UTC()
	res, err := s.db.SQL().ExecContext(ctx, `
		DELETE FROM idempotency_keys
		 WHERE expires_at <= ?
		   AND job_id IN (SELECT id FROM jobs
		                   WHERE status IN ('succeeded', 'failed', 'canceled', 'interrupted'))`,
		formatTime(now))
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

// sweepOnce 跑一轮扫描并按结果写日志。
//
// force 用于启动那一轮：即使一条待对账任务都没有也留一行记录，否则“启动时检查了”
// 这件事在日志里看不见。
func (s *Service) sweepOnce(ctx context.Context, force bool) {
	stats, err := s.SweepPending(ctx, ReconcileBatch)
	if err != nil {
		if ctx.Err() == nil {
			s.logger.Warn("任务对账扫描失败", "error", err)
		}
		return
	}
	if !force && stats.settled() {
		return
	}
	s.logger.Info("任务对账扫描完成",
		"scanned", stats.Scanned,
		"advanced", stats.Advanced,
		"deferred", stats.Deferred,
		"unconfirmed", stats.Unconfirmed,
		"unknown", stats.Unknown,
		"write_failed", stats.Failed,
		"skipped_inflight", stats.SkippedInflight)
}

// reconcileOne 用代理视图刷新一条任务，返回结论供计数。
func (s *Service) reconcileOne(ctx context.Context, job Job) reconcileOutcome {
	if isTerminalStatus(job.Status) && !job.NeedsSync {
		return outcomeSettled
	}
	if s.agent == nil {
		return outcomeDeferred
	}
	// 提交还在途中：代理此刻说 NOT_FOUND 不代表操作没落地，等下一轮。
	if s.submitInFlight(job.ID) {
		return outcomeInflight
	}

	op, err := s.agent.GetOperation(ctx, job.OperationID)
	if err != nil {
		var agentErr *agentclient.Error
		if errors.As(err, &agentErr) && agentErr.Code == "NOT_FOUND" {
			return s.handleAgentForgets(ctx, job)
		}
		// 连不上/超时/协议违约：如实保留当前状态与待对账标记，下一轮再来。
		return outcomeDeferred
	}

	synced, err := s.syncFromAgent(ctx, job.ID, op)
	if err != nil {
		s.logger.Error("任务对账失败", "job_id", job.ID, "operation_id", job.OperationID, "error", err)
		return outcomeWriteFailed
	}
	if synced.Status == job.Status && synced.Step == job.Step && synced.NeedsSync == job.NeedsSync {
		return outcomeSettled
	}
	return outcomeAdvanced
}

// handleAgentForgets 处理“代理查不到这条操作”。
//
// 两种情况必须分开，因为对用户的含义完全相反：
//   - needs_sync=1：Web 从未成功把这条操作的状态抄回来过。提交要么没到达，要么响应丢了，
//     而代理的记录是持久化的——查不到就是没落地。这是“没有执行”，写 failed + unconfirmed。
//   - needs_sync=0：Web 之前成功同步过一次，说明代理当时确实有这条操作。它后来消失，
//     只可能是代理侧数据被清理或回滚。此时执行与否不知道，**不能**写 failed，
//     只能写 interrupted 并要求人工核对（CONTRACTS.md §4.3）。
//
// 两条路都**不重试提交**：这里没有 CreateOperation 的调用点，唯一的出路是用户重新预检。
func (s *Service) handleAgentForgets(ctx context.Context, job Job) reconcileOutcome {
	now := s.now().UTC()
	if job.NeedsSync {
		errJSON, _ := json.Marshal(map[string]any{
			"code":    "AGENT_UNAVAILABLE",
			"message": "代理没有这条操作记录（提交可能未到达），请重新预检后再提交",
		})
		if _, err := s.markJobFailed(ctx, job.ID, "unconfirmed", errJSON, now); err != nil {
			s.logger.Error("标记任务未确认失败", "job_id", job.ID, "error", err)
			return outcomeWriteFailed
		}
		s.logger.Warn("任务提交未确认", "job_id", job.ID, "operation_id", job.OperationID)
		return outcomeUnconfirmed
	}

	errJSON, _ := json.Marshal(map[string]any{
		"code":    "OPERATION_INTERRUPTED",
		"message": "代理已经查不到这条操作，执行结果未知：请先核对系统实际状态，再决定是否重新预检",
	})
	if _, err := s.markJobInterrupted(ctx, job.ID, "unknown_result", errJSON, now); err != nil {
		s.logger.Error("标记任务结果未知失败", "job_id", job.ID, "error", err)
		return outcomeWriteFailed
	}
	s.logger.Warn("任务执行结果未知（代理丢失操作记录）",
		"job_id", job.ID, "operation_id", job.OperationID)
	return outcomeUnknown
}

// markJobInterrupted 把任务标成 interrupted（执行结果未知）并记录原因。
func (s *Service) markJobInterrupted(ctx context.Context, jobID, step string, errJSON []byte, now time.Time) (Job, error) {
	err := s.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE jobs SET status = 'interrupted', step = ?, error_json = ?, needs_sync = 0,
			       cancellable = 0, finished_at = ?, updated_at = ?
			 WHERE id = ? AND status NOT IN ('succeeded', 'failed', 'canceled', 'interrupted')`,
			step, string(errJSON), formatTime(now), formatTime(now), jobID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			// 已经被别处写成终态了：不覆盖，也不再追加事件。
			return nil
		}
		return appendJobEventTx(ctx, tx, jobID, "status",
			map[string]any{"status": "interrupted", "step": step, "reason": "unknown_result"}, now)
	})
	if err != nil {
		return Job{}, err
	}
	return s.readJobUnscoped(ctx, jobID)
}

// beginSubmit 登记“这条任务正在提交途中”。
func (s *Service) beginSubmit(jobID string) {
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	if s.inflight == nil {
		s.inflight = make(map[string]int)
	}
	s.inflight[jobID]++
}

// endSubmit 解除登记。
func (s *Service) endSubmit(jobID string) {
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	if n := s.inflight[jobID]; n > 1 {
		s.inflight[jobID] = n - 1
		return
	}
	delete(s.inflight, jobID)
}

// submitInFlight 报告这条任务是否正在提交途中。
func (s *Service) submitInFlight(jobID string) bool {
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	return s.inflight[jobID] > 0
}
