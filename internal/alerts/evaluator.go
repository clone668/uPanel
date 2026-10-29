package alerts

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"time"

	"upanel/internal/metrics"
	"upanel/internal/store"
)

// 评估循环的时间常量。
const (
	// DefaultInterval 是评估周期。
	//
	// 采集侧每 5 秒落一个原始样本（internal/metrics 的 DefaultInterval=2s，代理 5s 采集），
	// 15 秒一轮意味着窗口里至少能多出三个点——判断“持续”用的是区间内的全部点，
	// 评估频率只要比采样慢一点就够了；快了只是让同一次超限被数很多遍。
	DefaultInterval = 15 * time.Second
	// rawBucket 是原始粒度的桶宽。
	rawBucket = 5 * time.Second
	// freshnessFloor 是”最新样本过期”的下限。
	//
	// 采集 5 秒一桶、web 每 2 秒轮询一次，正常情况下最新样本不会比 now 旧 10 秒。
	// 20 秒还没更新，就说明采集真的停了——这时候既不能说”还超限”，也不能说
	// “已恢复”，只能说”不知道”。
	freshnessFloor = 20 * time.Second
	// evalErrorLogInterval 是同一条规则的评估错误最多多久记一次日志。
	evalErrorLogInterval = 5 * time.Minute
)

// coverageSlack 返回窗口起点允许的缺口。
//
// 窗口是从 now 往回推 duration 秒，落桶又是向下对齐的，所以窗口里最早那个样本
// 必然比起点晚 0—1 个写入节奏。留两个节奏的余量，超过它才叫”窗口前段没数据”。
//
// 这里的节奏必须按**序列自己的**写入节奏算，不能按 raw 桶宽一刀切：容量序列
// 30 秒才落一行（见 metrics.CapacityCadence），拿 5 秒的余量去量它，60 秒的窗口
// 会被判成”前段没数据”，于是一条空间规则配好之后永远不响。
func coverageSlack(metric string) time.Duration {
	if cadence := metrics.WriteCadence(metric); cadence > rawBucket {
		return 2 * cadence
	}
	return 2 * rawBucket
}

// staleAfter 返回”最新样本过期”的界线，同样按序列自己的写入节奏算。
//
// 容量序列 30 秒一个点，20 秒不去更新是常态而不是故障；拿 20 秒去判它，
// “数据不足”会变成常态，规则永远停在原地。
func staleAfter(metric string) time.Duration {
	if cadence := metrics.WriteCadence(metric); 3*cadence > freshnessFloor {
		return 3 * cadence
	}
	return freshnessFloor
}

// Observation 是一条规则在某一刻能看到的全部事实。
type Observation struct {
	// Now 是评估时刻。
	Now time.Time
	// Primary 是主口径在评估窗口里的分布。
	Primary store.MetricWindowStat
	// Secondary 是次级口径的分布；HasSecondary 为 false 时无意义。
	Secondary    store.MetricWindowStat
	HasSecondary bool
}

// outcome 是一次判定得出的结论。
//
// 它是一个**纯函数**的返回值（decide 不碰库、不取时钟），这样状态机的每一条边
// 都能被直接测到：把 Rule 和 Observation 摆出来，断言 outcome。用数据库去测状态机，
// 会测出一堆“这轮跑完之后那列是几”，而不是“在这种输入下它应该做什么”。
type outcome struct {
	State         string
	StateSince    time.Time
	RecoverSince  time.Time
	CooldownUntil time.Time
	Reason        string
	// Peak 是这一轮朝告警方向最极端的值；没有数据时为 nil。
	Peak *float64
	// Fire 表示这一轮要新开一个事件；Resolve 表示要把打开的事件收口。
	Fire    bool
	Resolve bool
}

// decide 是告警状态机的全部。
//
// 三个状态、两个事件，任何一条输入都有确定的落点：
//
//	ok      --超限-->        pending（开始计持续）
//	pending --超限够久-->    firing（开事件）
//	pending --回到线内-->    ok
//	firing  --回到恢复线内够久--> ok（收口事件，进冷却）
//
// 数据不足时**原地不动**：不推进、不触发、不恢复。
func decide(r Rule, obs Observation) outcome {
	out := outcome{
		State:         r.State,
		StateSince:    r.StateSince,
		RecoverSince:  r.RecoverSince,
		CooldownUntil: r.CooldownUntil,
	}
	if out.State == "" {
		out.State = StateOK
	}

	if reason := coverageProblem(r, obs); reason != "" {
		out.Reason = truncateReason(reason)
		return out
	}

	over, recovered := classify(r, obs)
	out.Peak = observedPeak(r, obs)

	switch r.State {
	case StateOK, "":
		// 冷却期内不再重新起算：恢复之后马上又超限（备份刚跑完、阈值贴着常态）
		// 是最常见的刷屏来源，cooldown_seconds 就是为它准备的。
		if !r.CooldownUntil.IsZero() && obs.Now.Before(r.CooldownUntil) {
			out.Reason = truncateReason("恢复后的冷却期内（直到 " + formatTime(r.CooldownUntil) + "）")
			return out
		}
		if over {
			out.State = StatePending
			out.StateSince = obs.Now
			out.Reason = truncateReason(fmt.Sprintf("已超限，开始计持续（需 %d 秒）", r.DurationSeconds))
		}
	case StatePending:
		if !over {
			out.State = StateOK
			out.StateSince = obs.Now
			out.Reason = "窗口内不再全部超限，持续被打断"
			return out
		}
		since := r.StateSince
		if since.IsZero() {
			// 插件的状态列缺失（老库、手写数据）时不要把 0 当成“从很久以前就在超限”：
			// 那会立刻触发，等于绕过 duration_seconds。从这一刻重新计。
			since = obs.Now
			out.StateSince = since
		}
		elapsed := obs.Now.Sub(since)
		if elapsed >= time.Duration(r.DurationSeconds)*time.Second {
			out.State = StateFiring
			out.StateSince = obs.Now
			out.RecoverSince = time.Time{}
			out.Fire = true
			out.Reason = ""
			return out
		}
		// 还在攒：把“还差多久”写回 last_reason。不写的话第二轮评估就会把第一次那句
		// “开始计持续”覆盖成空串——一条正处于 pending 的规则看起来像什么都没发生，
		// 而那正是最需要解释的时候（“它是不是根本没在看？”）。
		out.Reason = truncateReason(fmt.Sprintf("已超限，正在计持续（还需 %s）",
			durationText(time.Duration(r.DurationSeconds)*time.Second-elapsed)))
		return out
	case StateFiring:
		if !recovered {
			// 回到两条线之间的死区、或者重新超限，都算“又开始不好了”：
			// 清掉恢复计时，事件继续开着。
			if !r.RecoverSince.IsZero() {
				out.RecoverSince = time.Time{}
			}
			out.Reason = ""
			return out
		}
		if r.RecoverSince.IsZero() {
			out.RecoverSince = obs.Now
			out.Reason = truncateReason(fmt.Sprintf("已回到恢复线内，开始计恢复持续（需 %d 秒）", r.DurationSeconds))
			return out
		}
		elapsed := obs.Now.Sub(r.RecoverSince)
		if elapsed >= time.Duration(r.DurationSeconds)*time.Second {
			out.State = StateOK
			out.StateSince = obs.Now
			out.RecoverSince = time.Time{}
			out.CooldownUntil = obs.Now.Add(time.Duration(r.CooldownSeconds) * time.Second)
			out.Resolve = true
			out.Reason = ""
			return out
		}
		// 与 pending 同理：恢复期的第二轮也不该把理由清成空串——那时规则还在 firing，
		// 只是已经在往回走，last_reason 空着等于把这段过程藏起来。
		out.Reason = truncateReason(fmt.Sprintf("已回到恢复线内，正在计恢复持续（还需 %s）",
			durationText(time.Duration(r.DurationSeconds)*time.Second-elapsed)))
	default:
		// 状态列里出现了闭集外的值（只可能是手改库），当作 ok 重新起算。
		out.State = StateOK
		out.StateSince = obs.Now
		out.Reason = "未知的规则状态，已重置为 ok"
	}
	return out
}

// classify 判断“窗口是否全部超限”与“窗口是否全部恢复到恢复线内”。
//
// 主口径用两条线（告警线 + 恢复线），中间的带子既不算超限也不算恢复；
// 次级口径没有恢复线字段，它的恢复就是“不超限”。
//
// 组合方式是对偶的：
//   - any（任一满足就触发）：恢复要求**两个都**回到安全侧——只要还有一个在限外，
//     这件事就还没完；
//   - all（全部满足才触发）：恢复只要**有一个**回到安全侧——条件已经不再全部成立。
func classify(r Rule, obs Observation) (over, recovered bool) {
	p := obs.Primary
	pOver := p.Samples > 0 && p.Over == p.Samples
	pRecovered := p.Samples > 0 && p.Recovered == p.Samples
	if r.Secondary == nil || !obs.HasSecondary {
		return pOver, pRecovered
	}
	s := obs.Secondary
	sOver := s.Samples > 0 && s.Over == s.Samples
	sRecovered := s.Samples > 0 && s.Over == 0
	if r.Combination == CombinationAll {
		return pOver && sOver, pRecovered || sRecovered
	}
	return pOver || sOver, pRecovered && sRecovered
}

// observedPeak 选出这一轮要显示/记录的极值。
//
// 双口径时取“真正在限外的那一个”：使用率 12% 而剩余空间只有 3 GiB，把 12 记成
// “当前值”会让人以为一切正常。两个都在限外时取主口径——它是规则的名字里那个量。
func observedPeak(r Rule, obs Observation) *float64 {
	if obs.Primary.Empty() {
		return nil
	}
	if r.Secondary != nil && obs.HasSecondary && !obs.Secondary.Empty() {
		primaryOver := obs.Primary.Over == obs.Primary.Samples
		secondaryOver := obs.Secondary.Over == obs.Secondary.Samples
		if !primaryOver && secondaryOver {
			v := obs.Secondary.Peak
			return &v
		}
	}
	v := obs.Primary.Peak
	return &v
}

// coverageProblem 判断这一轮的数据够不够下结论。
//
// 返回空串表示“够”。非空串是给人看的原因，会写进 last_reason：一条规则一直不响时，
// 第一件要回答的事就是“它是不是根本没数据”。
func coverageProblem(r Rule, obs Observation) string {
	now := obs.Now
	start := now.Add(-time.Duration(r.DurationSeconds) * time.Second)
	if reason := singleCoverage("主指标", r.Metric, obs.Primary, start, now); reason != "" {
		return reason
	}
	if r.Secondary != nil {
		if !obs.HasSecondary {
			return "次级指标的窗口没有取到"
		}
		if reason := singleCoverage("次级指标", r.Secondary.Metric, obs.Secondary, start, now); reason != "" {
			return reason
		}
	}
	return ""
}

func singleCoverage(label, metric string, st store.MetricWindowStat, start, now time.Time) string {
	if st.Empty() {
		return fmt.Sprintf("%s %s 在窗口内没有任何样本（采集未开启，或这条序列从未写入）", label, metric)
	}
	if slack := coverageSlack(metric); st.Oldest.After(start.Add(slack)) {
		return fmt.Sprintf("%s %s 的窗口前段还没有样本（最早 %s，窗口起点 %s，这条序列每 %s一个点）：持续时长还不成立",
			label, metric, formatTime(st.Oldest), formatTime(start), durationText(metrics.WriteCadence(metric)))
	}
	if stale := staleAfter(metric); now.Sub(st.Newest) > stale {
		return fmt.Sprintf("%s %s 的最新样本已经过期（%s，现在 %s，容许 %s 的间隔）：这一轮不知道",
			label, metric, formatTime(st.Newest), formatTime(now), durationText(stale))
	}
	return ""
}

// durationText 把一段时长写成给人看的样子。
func durationText(d time.Duration) string {
	seconds := int(d.Seconds())
	if seconds <= 0 {
		return "若干秒"
	}
	return fmt.Sprintf("%d 秒", seconds)
}

func truncateReason(s string) string {
	if len([]rune(s)) <= MaxReasonLen {
		return s
	}
	return string([]rune(s)[:MaxReasonLen-1]) + "…"
}

// EvalReport 是一轮评估的结果汇总（给日志与自检用）。
type EvalReport struct {
	// Rules 是本轮被评估的启用规则数。
	Rules int
	// Evaluated 是真正下了结论的规则数（数据不足的不算）。
	Evaluated int
	// Fired 是新开的事件数，Resolved 是收口的事件数。
	Fired    int
	Resolved int
}

// EvaluateOnce 跑一轮评估。它不开定时器，由 Run 或测试调用。
func (s *Service) EvaluateOnce(ctx context.Context) (EvalReport, error) {
	var report EvalReport
	rules, err := s.enabledRules(ctx)
	if err != nil {
		return report, err
	}
	if len(rules) == 0 {
		return report, nil
	}
	silence, err := s.GetSilence(ctx)
	if err != nil {
		return report, err
	}
	now := s.nowUTC()
	for _, rule := range rules {
		report.Rules++
		fired, resolved, err := s.evaluateRule(ctx, rule, now, silence)
		if err != nil {
			return report, fmt.Errorf("alerts: 评估规则 %d 失败: %w", rule.ID, err)
		}
		if fired {
			report.Fired++
		}
		if resolved {
			report.Resolved++
		}
		if fired || resolved || rule.LastValue != nil {
			report.Evaluated++
		}
	}
	return report, nil
}

func (s *Service) enabledRules(ctx context.Context) ([]Rule, error) {
	rows, err := s.query(ctx, `SELECT `+ruleColumns+` FROM alert_rules WHERE enabled = 1 ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("alerts: 读取启用规则失败: %w", err)
	}
	defer rows.Close()
	out := make([]Rule, 0, 16)
	for rows.Next() {
		rule, err := scanRule(rows)
		if err != nil {
			return nil, fmt.Errorf("alerts: 读取启用规则失败: %w", err)
		}
		out = append(out, rule)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("alerts: 读取启用规则失败: %w", err)
	}
	return out, nil
}

// evaluateRule 评估一条规则并落库。返回 (是否新开事件, 是否收口事件, 错误)。
func (s *Service) evaluateRule(ctx context.Context, rule Rule, now time.Time, silence Silence) (bool, bool, error) {
	from, to := rule.Window(now)
	scope, target := rule.Target()

	primary, err := s.db.MetricWindowStats(ctx, store.MetricWindowQuery{
		Scope: scope, TargetID: target, Metric: rule.Metric,
		From: from, To: to,
		Comparison: rule.Comparison, Threshold: rule.Threshold,
		RecoveryComparison: RecoveryComparison(rule.Comparison),
		RecoveryThreshold:  rule.RecoveryThreshold,
	})
	if err != nil {
		return false, false, err
	}

	obs := Observation{Now: now, Primary: primary}
	if rule.Secondary != nil {
		secondary, err := s.db.MetricWindowStats(ctx, store.MetricWindowQuery{
			Scope: scope, TargetID: target, Metric: rule.Secondary.Metric,
			From: from, To: to,
			Comparison: rule.Secondary.Comparison, Threshold: rule.Secondary.Threshold,
		})
		if err != nil {
			return false, false, err
		}
		obs.Secondary = secondary
		obs.HasSecondary = true
	}

	out := decide(rule, obs)
	return s.applyOutcome(ctx, rule, out, now, silence)
}

// applyOutcome 把一次判定写回库。状态迁移与新开事件在同一笔事务里。
func (s *Service) applyOutcome(ctx context.Context, rule Rule, out outcome, now time.Time, silence Silence) (bool, bool, error) {
	var fired, resolved bool
	err := s.db.WithTx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		// 状态迁移按 (版本, 状态) 做比较交换：读规则的时候它是 pending，
		// 这一轮算完要写 firing——中间如果有人改了规则，这次迁移就不该生效，
		// 因为它是按旧条件算出来的结论。
		res, err := s.exec(ctx, `
			UPDATE alert_rules
			   SET state = ?, state_since = ?, recover_since = ?, cooldown_until = ?,
			       last_eval_at = ?, last_value = ?, last_reason = ?
			 WHERE id = ? AND version = ? AND state = ?`,
			out.State, unixOrZero(out.StateSince), unixOrZero(out.RecoverSince),
			unixOrZero(out.CooldownUntil), now.Unix(), floatOrNil(out.Peak),
			out.Reason, rule.ID, rule.Version, rule.State)
		if err != nil {
			return fmt.Errorf("alerts: 写回规则状态失败: %w", err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("alerts: 写回规则状态失败: %w", err)
		}
		if affected == 0 {
			// 规则在这一轮过程中被改过（或状态已被别的评估改过）。
			// 放弃这次结论是**正确**的：结论是基于旧条件算的。
			return nil
		}
		if out.Fire {
			ev, created, err := s.insertEvent(ctx, rule, now, silence, out.Peak)
			if err != nil {
				return err
			}
			// 通知入队与事件行在同一笔事务里：要么一起进去，要么一起没有。
			// created=false 表示去重索引挡住了这次插入（同一规则已有一条打开的事件）——
			// 那时没有新事实，也就不该再发一次通知。
			if created && s.notifier != nil {
				if err := s.notifier.AlertFired(ctx, ev); err != nil {
					return err
				}
			}
			fired = true
		}
		if out.Resolve {
			ev, closed, err := s.resolveEvent(ctx, rule, now)
			if err != nil {
				return err
			}
			if closed && s.notifier != nil {
				if err := s.notifier.AlertResolved(ctx, ev); err != nil {
					return err
				}
			}
			resolved = true
		}
		return nil
	})
	if err != nil {
		return false, false, err
	}
	return fired, resolved, nil
}

// insertEvent 新开一个事件行，并把它作为一条 Event 返回（通知要用同一份事实）。
//
// peak 不在这里定：事件期间的极值要覆盖整个持续区间，触发这一刻的窗口只是它的尾巴。
// 恢复时按 [started_at, resolved_at] 重算一次再写死。
//
// **第二条返回值表示“这一次真的插进去了”。** `INSERT OR IGNORE` 在唯一索引
// idx_alert_events_open 上被挡住时是 0 行——那种情况下库里已经有同一个事件周期的
// 打开行，再通知一次就是重复刷屏（PLAN.md 第 18 节明确禁止）。
func (s *Service) insertEvent(ctx context.Context, rule Rule, now time.Time, silence Silence, peak *float64) (Event, bool, error) {
	nowText := formatTime(now)
	silenced := silence.ActiveAt(now)
	res, err := s.exec(ctx, `
		INSERT OR IGNORE INTO alert_events
		    (rule_id, rule_name, scope, target_id, metric, comparison, threshold,
		     state, started_at, peak_value, silenced, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 'firing', ?, ?, ?, ?)`,
		rule.ID, rule.Name, rule.Scope, rule.TargetID, rule.Metric, rule.Comparison, rule.Threshold,
		nowText, floatOrNil(peak), boolInt(silenced), nowText)
	if err != nil {
		return Event{}, false, fmt.Errorf("alerts: 写入事件失败: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return Event{}, false, fmt.Errorf("alerts: 写入事件失败: %w", err)
	}
	if affected == 0 {
		return Event{}, false, nil
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Event{}, false, fmt.Errorf("alerts: 读取事件 id 失败: %w", err)
	}
	return Event{
		ID:         id,
		RuleID:     rule.ID,
		RuleName:   rule.Name,
		Scope:      rule.Scope,
		TargetID:   rule.TargetID,
		Metric:     rule.Metric,
		Comparison: rule.Comparison,
		Threshold:  rule.Threshold,
		State:      EventFiring,
		StartedAt:  now,
		PeakValue:  peak,
		Silenced:   silenced,
		CreatedAt:  now,
	}, true, nil
}

// resolveEvent 收口这条规则打开的事件。
//
// peak 与 duration 按整个事件区间重算：事件开了六小时，只在触发那一刻记下的值
// 会让事后看到“峰值 91%”，而实际早就顶到 100% 了。
func (s *Service) resolveEvent(ctx context.Context, rule Rule, now time.Time) (Event, bool, error) {
	return s.closeOpenEvent(ctx, rule, now)
}

// closeOpenEvent 把规则当前打开的事件收口（没有就算了）。
//
// 它同时服务两种收口：恢复（状态机判定“持续回到恢复线内”），以及规则被停用/删除
// （此时没有任何东西会再评估它，开着的事件会变成一条永远 firing 的孤儿行）。
// 两种情况下 resolved_at 的事实含义都是“这条事件在此时结束了”，区别写在审计里
// （谁改了规则），不在事件行里——事件表只记事实，不记动机。
//
// 返回值里的第二条表示“真的收口了一行”。**停用与删除造成的收口不发通知**：
// 那不是“机器好了”，是“没人再看它了”，发一条“已恢复”等于把两件事说成一件事。
func (s *Service) closeOpenEvent(ctx context.Context, rule Rule, now time.Time) (Event, bool, error) {
	var (
		id          int64
		startedText string
		silenced    int64
	)
	err := s.queryRow(ctx,
		`SELECT id, started_at, silenced FROM alert_events WHERE rule_id = ? AND state = 'firing'`,
		rule.ID).Scan(&id, &startedText, &silenced)
	if err == sql.ErrNoRows {
		return Event{}, false, nil
	}
	if err != nil {
		return Event{}, false, fmt.Errorf("alerts: 读取打开的事件失败: %w", err)
	}
	started, err := parseTime(startedText)
	if err != nil {
		return Event{}, false, err
	}
	peak := s.spanPeak(ctx, rule, started, now)
	duration := int(now.Sub(started).Seconds())
	if duration < 0 {
		duration = 0
	}
	_, err = s.exec(ctx, `
		UPDATE alert_events
		   SET state = 'resolved', resolved_at = ?, duration_seconds = ?, peak_value = ?
		 WHERE id = ? AND state = 'firing'`,
		formatTime(now), duration, floatOrNil(peak), id)
	if err != nil {
		return Event{}, false, fmt.Errorf("alerts: 收口事件失败: %w", err)
	}
	return Event{
		ID:              id,
		RuleID:          rule.ID,
		RuleName:        rule.Name,
		Scope:           rule.Scope,
		TargetID:        rule.TargetID,
		Metric:          rule.Metric,
		Comparison:      rule.Comparison,
		Threshold:       rule.Threshold,
		State:           EventResolved,
		StartedAt:       started,
		ResolvedAt:      now,
		DurationSeconds: duration,
		PeakValue:       peak,
		Silenced:        silenced != 0,
	}, true, nil
}

// spanPeak 统计 [started, now] 区间里朝告警方向最极端的值。
func (s *Service) spanPeak(ctx context.Context, rule Rule, started, now time.Time) *float64 {
	scope, target := rule.Target()
	st, err := s.db.MetricWindowStats(ctx, store.MetricWindowQuery{
		Scope: scope, TargetID: target, Metric: rule.Metric,
		From: started, To: now,
		Comparison: rule.Comparison, Threshold: rule.Threshold,
	})
	if err != nil || st.Empty() {
		return nil
	}
	v := st.Peak
	return &v
}

// Run 是评估循环。ctx 结束时返回（与采集循环共用同一个 ctx）。
func (s *Service) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	// 启动先跑一轮：面板重启之后不该等一个周期才开始判断。
	if _, err := s.EvaluateOnce(ctx); err != nil && ctx.Err() == nil {
		s.logError(err)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := s.EvaluateOnce(ctx); err != nil && ctx.Err() == nil {
				s.logError(err)
			}
		}
	}
}

func (s *Service) logError(err error) {
	if s.logger == nil {
		return
	}
	// 同一条错误一分钟内只记一次：评估循环 15 秒一轮，库被锁住时一分钟能刷四条
	// 一模一样的日志，把真正有用的那几行挤走。
	now := s.nowUTC()
	s.logOnceMu.Lock()
	repeat := s.lastLogErr == err.Error() && !s.lastLogAt.IsZero() &&
		now.Truncate(time.Minute).Equal(s.lastLogAt.Truncate(time.Minute))
	s.lastLogErr = err.Error()
	s.lastLogAt = now
	s.logOnceMu.Unlock()
	if repeat {
		return
	}
	s.logger.Warn("告警评估失败", "error", err)
}

func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func floatOrNil(v *float64) any {
	if v == nil || math.IsNaN(*v) {
		return nil
	}
	return *v
}
