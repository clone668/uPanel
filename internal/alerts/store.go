package alerts

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"upanel/internal/store"
)

// Options 是 Service 的构造参数。
type Options struct {
	// Now 注入时钟（测试用）；nil 时用 time.Now。
	Now func() time.Time
	// Logger 可选。只用于评估循环里的错误：一条规则读不出数据不该让整个循环安静地
	// 死掉，但也不该每 15 秒刷一条同样的日志（见 logError 的去重）。
	Logger *slog.Logger
	// Notifier 是新开/收口事件时的回调（P13.3 的通知入队）。
	//
	// 它是**接口而不是具体实现**：通知要往 Web 库里写投递账，而告警不应该反过来依赖
	// 通知包（那会绕成环）。nil 表示“这一轮不发通知”，事件照记——演示模式就是这个形态。
	Notifier EventNotifier
}

// EventNotifier 接收告警事件的生命周期通知。
//
// 两个方法都在评估事务**内部**被调用：投递行与事件行要么一起进去，要么一起没有。
// 实现方必须只用传进来的 ctx 写库（上下文里带着那笔事务，见 store.TxFromContext），
// 不要自己开新连接——那会在同一个 SQLite 上等自己持有的写锁。
type EventNotifier interface {
	AlertFired(ctx context.Context, ev Event) error
	AlertResolved(ctx context.Context, ev Event) error
}

// Service 是告警规则、事件与维护静默的唯一入口。接口层不得直接写这三张表。
type Service struct {
	db       *store.DB
	now      func() time.Time
	logger   *slog.Logger
	notifier EventNotifier

	logOnceMu  sync.Mutex
	lastLogErr string
	lastLogAt  time.Time
}

// New 构造 Service。db 为必需依赖。
func New(db *store.DB, opts Options) (*Service, error) {
	if db == nil {
		return nil, errors.New("alerts: 数据库句柄不能为空")
	}
	s := &Service{db: db, now: opts.Now, logger: opts.Logger, notifier: opts.Notifier}
	if s.now == nil {
		s.now = time.Now
	}
	return s, nil
}

// InTransaction 在一个事务里执行 fn，供接口层把“规则变更 + 审计”合成一笔事务。
func (s *Service) InTransaction(ctx context.Context, fn func(ctx context.Context) error) error {
	if fn == nil {
		return errors.New("alerts: InTransaction 需要非空函数")
	}
	return s.db.WithTx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		return fn(ctx)
	})
}

func (s *Service) nowUTC() time.Time { return s.now().UTC().Truncate(time.Second) }

// Now 返回服务的当前时刻（UTC）。接口层拼维护静默的 active 判定时要用同一个时钟，
// 不能各自取一次 time.Now——测试里那会让"刚设好的窗口"看起来还没生效。
func (s *Service) Now() time.Time { return s.nowUTC() }

// −−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−
// 规则
// −−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−

// ruleColumns 是规则表读取列的固定顺序，与 scanRule 一一对应。
const ruleColumns = `id, name, scope, target_id, metric, comparison, threshold,
	secondary_metric, secondary_comparison, secondary_threshold, combination,
	duration_seconds, recovery_threshold, cooldown_seconds, enabled, version,
	state, state_since, recover_since, cooldown_until, last_eval_at, last_value, last_reason,
	created_at, updated_at`

// Create 新建一条规则。
func (s *Service) Create(ctx context.Context, in RuleInput) (Rule, error) {
	norm, err := normalizeRule(in)
	if err != nil {
		return Rule{}, err
	}
	var count int
	if err := s.queryRow(ctx, `SELECT COUNT(*) FROM alert_rules`).Scan(&count); err != nil {
		return Rule{}, fmt.Errorf("alerts: 统计规则数量失败: %w", err)
	}
	if count >= MaxRules {
		return Rule{}, ErrLimitReached
	}
	now := s.nowUTC()
	nowText := formatTime(now)
	res, err := s.exec(ctx, `
		INSERT INTO alert_rules
		    (name, scope, target_id, metric, comparison, threshold,
		     secondary_metric, secondary_comparison, secondary_threshold, combination,
		     duration_seconds, recovery_threshold, cooldown_seconds, enabled, version,
		     state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, 'ok', ?, ?)`,
		norm.Name, norm.Scope, norm.TargetID, norm.Metric, norm.Comparison, norm.Threshold,
		nullString(norm.Secondary), nullComparison(norm.Secondary), nullFloat(norm.Secondary),
		norm.Combination, norm.DurationSeconds, norm.RecoveryThreshold, norm.CooldownSeconds,
		boolInt(norm.Enabled), nowText, nowText)
	if err != nil {
		return Rule{}, fmt.Errorf("alerts: 写入规则失败: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Rule{}, fmt.Errorf("alerts: 读取规则 id 失败: %w", err)
	}
	return s.Get(ctx, id)
}

// Get 读取一条规则。
func (s *Service) Get(ctx context.Context, id int64) (Rule, error) {
	if id <= 0 {
		return Rule{}, ErrNotFound
	}
	row := s.queryRow(ctx, `SELECT `+ruleColumns+` FROM alert_rules WHERE id = ?`, id)
	rule, err := scanRule(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Rule{}, ErrNotFound
	}
	if err != nil {
		return Rule{}, fmt.Errorf("alerts: 读取规则失败: %w", err)
	}
	return rule, nil
}

// List 返回全部规则，按 id 升序。
func (s *Service) List(ctx context.Context) ([]Rule, error) {
	rows, err := s.query(ctx, `SELECT `+ruleColumns+` FROM alert_rules ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("alerts: 读取规则列表失败: %w", err)
	}
	defer rows.Close()
	out := make([]Rule, 0, 16)
	for rows.Next() {
		rule, err := scanRule(rows)
		if err != nil {
			return nil, fmt.Errorf("alerts: 读取规则列表失败: %w", err)
		}
		out = append(out, rule)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("alerts: 读取规则列表失败: %w", err)
	}
	return out, nil
}

// Update 用一份完整的输入替换规则内容（version 必须匹配）。
//
// **不重置状态机**：改阈值不等于“现在没超限”。一条正在 firing 的规则把阈值调高
// 之后，如果新条件仍然成立，那个事件就该继续开着；如果不再成立，下一个评估周期
// 自然会把它恢复。在这里顺手把状态改成 ok，等于用一次配置修改伪造了一次恢复。
//
// 唯一例外是**停用**：停用之后没有任何东西会再评估这条规则，所以它会先把打开的
// 事件收口（见 closeOpenEvent），不留一条永远 firing 的孤儿行。
func (s *Service) Update(ctx context.Context, id int64, version int64, in RuleInput) (Rule, error) {
	if id <= 0 {
		return Rule{}, ErrNotFound
	}
	norm, err := normalizeRule(in)
	if err != nil {
		return Rule{}, err
	}
	now := s.nowUTC()
	res, err := s.exec(ctx, `
		UPDATE alert_rules
		   SET name = ?, target_id = ?, metric = ?, scope = ?, comparison = ?, threshold = ?,
		       secondary_metric = ?, secondary_comparison = ?, secondary_threshold = ?,
		       combination = ?, duration_seconds = ?, recovery_threshold = ?,
		       cooldown_seconds = ?, enabled = ?, version = version + 1, updated_at = ?
		 WHERE id = ? AND version = ?`,
		norm.Name, norm.TargetID, norm.Metric, norm.Scope, norm.Comparison, norm.Threshold,
		nullString(norm.Secondary), nullComparison(norm.Secondary), nullFloat(norm.Secondary),
		norm.Combination, norm.DurationSeconds, norm.RecoveryThreshold, norm.CooldownSeconds,
		boolInt(norm.Enabled), formatTime(now), id, version)
	if err != nil {
		return Rule{}, fmt.Errorf("alerts: 更新规则失败: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return Rule{}, fmt.Errorf("alerts: 更新规则失败: %w", err)
	}
	if affected == 0 {
		return Rule{}, s.conflictOrMissing(ctx, id)
	}
	if !norm.Enabled {
		if err := s.stopRule(ctx, id, now); err != nil {
			return Rule{}, err
		}
	}
	return s.Get(ctx, id)
}

// Delete 删除一条规则。version 必须匹配。
//
// 打开的事件先收口再删规则：事件行的 rule_id 是 ON DELETE SET NULL，不收口的话
// 那行会变成“没有规则的、永远 firing 的事件”，既不会恢复也没人去恢复它。
func (s *Service) Delete(ctx context.Context, id int64, version int64) error {
	if id <= 0 {
		return ErrNotFound
	}
	rule, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if version > 0 && rule.Version != version {
		return ErrVersionConflict
	}
	// 收口打开的事件**不发通知**：这不是“机器好了”，是“这条规则不再被评估了”。
	if _, _, err := s.closeOpenEvent(ctx, rule, s.nowUTC()); err != nil {
		return err
	}
	res, err := s.exec(ctx, `DELETE FROM alert_rules WHERE id = ? AND version = ?`, id, rule.Version)
	if err != nil {
		return fmt.Errorf("alerts: 删除规则失败: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("alerts: 删除规则失败: %w", err)
	}
	if affected == 0 {
		return ErrVersionConflict
	}
	return nil
}

// SetEnabled 只启停一条规则（PATCH 里常见的最小改动）。version 必须匹配。
func (s *Service) SetEnabled(ctx context.Context, id int64, version int64, enabled bool) (Rule, error) {
	rule, err := s.Get(ctx, id)
	if err != nil {
		return Rule{}, err
	}
	if version > 0 && rule.Version != version {
		return Rule{}, ErrVersionConflict
	}
	now := s.nowUTC()
	res, err := s.exec(ctx,
		`UPDATE alert_rules SET enabled = ?, version = version + 1, updated_at = ? WHERE id = ? AND version = ?`,
		boolInt(enabled), formatTime(now), id, rule.Version)
	if err != nil {
		return Rule{}, fmt.Errorf("alerts: 启停规则失败: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return Rule{}, fmt.Errorf("alerts: 启停规则失败: %w", err)
	}
	if affected == 0 {
		return Rule{}, ErrVersionConflict
	}
	if !enabled {
		if err := s.stopRule(ctx, id, now); err != nil {
			return Rule{}, err
		}
	}
	return s.Get(ctx, id)
}

// stopRule 把一条不再评估的规则收回到 ok，并把打开的事件收口。
func (s *Service) stopRule(ctx context.Context, id int64, now time.Time) error {
	rule, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if _, _, err := s.closeOpenEvent(ctx, rule, now); err != nil {
		return err
	}
	if _, err := s.exec(ctx, `
		UPDATE alert_rules
		   SET state = 'ok', state_since = ?, recover_since = 0, cooldown_until = 0, last_reason = ?
		 WHERE id = ? AND state <> 'ok'`,
		now.Unix(), "规则已停用", id); err != nil {
		return fmt.Errorf("alerts: 重置规则状态失败: %w", err)
	}
	return nil
}

func (s *Service) conflictOrMissing(ctx context.Context, id int64) error {
	var exists int
	err := s.queryRow(ctx, `SELECT COUNT(*) FROM alert_rules WHERE id = ?`, id).Scan(&exists)
	if err != nil {
		return fmt.Errorf("alerts: 读取规则失败: %w", err)
	}
	if exists == 0 {
		return ErrNotFound
	}
	return ErrVersionConflict
}

func scanRule(sc scanner) (Rule, error) {
	var (
		r                           Rule
		secMetric, secComparison    sql.NullString
		secThreshold, lastValue     sql.NullFloat64
		duration, cooldown, enabled int64
		stateSince, recoverSince    int64
		cooldownUntil, lastEvalAt   int64
		createdAt, updatedAt        string
		lastReason                  string
	)
	err := sc.Scan(&r.ID, &r.Name, &r.Scope, &r.TargetID, &r.Metric, &r.Comparison, &r.Threshold,
		&secMetric, &secComparison, &secThreshold, &r.Combination,
		&duration, &r.RecoveryThreshold, &cooldown, &enabled, &r.Version,
		&r.State, &stateSince, &recoverSince, &cooldownUntil, &lastEvalAt, &lastValue, &lastReason,
		&createdAt, &updatedAt)
	if err != nil {
		return Rule{}, err
	}
	if secMetric.Valid && secComparison.Valid && secThreshold.Valid {
		r.Secondary = &Secondary{Metric: secMetric.String, Comparison: secComparison.String, Threshold: secThreshold.Float64}
	}
	r.DurationSeconds = int(duration)
	r.CooldownSeconds = int(cooldown)
	r.Enabled = enabled != 0
	if lastValue.Valid {
		v := lastValue.Float64
		r.LastValue = &v
	}
	r.LastReason = lastReason
	r.StateSince = fromUnix(stateSince)
	r.RecoverSince = fromUnix(recoverSince)
	r.CooldownUntil = fromUnix(cooldownUntil)
	r.LastEvalAt = fromUnix(lastEvalAt)
	if r.CreatedAt, err = parseTime(createdAt); err != nil {
		return Rule{}, err
	}
	if r.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return Rule{}, err
	}
	return r, nil
}

// −−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−
// 事件
// −−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−

// Event 是一条告警事件（一个事件周期一行）。
type Event struct {
	ID int64
	// RuleID 为 0 表示规则后来被删掉了（外键 SET NULL），快照字段仍然可读。
	RuleID     int64
	RuleName   string
	Scope      string
	TargetID   string
	Metric     string
	Comparison string
	Threshold  float64
	State      string
	StartedAt  time.Time
	// ResolvedAt 为零值表示还在持续。
	ResolvedAt time.Time
	// DurationSeconds 是实际持续时长（恢复时算好写死）。
	DurationSeconds int
	PeakValue       *float64
	Silenced        bool
	AckedAt         time.Time
	AckedBy         string
	CreatedAt       time.Time
}

// EventsQuery 是事件列表的查询条件。
type EventsQuery struct {
	// State 为空表示不过滤；否则是 EventFiring / EventResolved。
	State string
	// RuleID 大于 0 时只返回该规则的事件。
	RuleID int64
	// Limit 是返回条数上限（默认 50，最大 200）。
	Limit int
}

// ListEvents 返回事件列表，最新在前。
func (s *Service) ListEvents(ctx context.Context, q EventsQuery) ([]Event, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	var where string
	args := []any{}
	if q.State != "" {
		if q.State != EventFiring && q.State != EventResolved {
			return nil, invalidRule("state", "必须是 firing/resolved 之一")
		}
		where += " AND state = ?"
		args = append(args, q.State)
	}
	if q.RuleID > 0 {
		where += " AND rule_id = ?"
		args = append(args, q.RuleID)
	}
	args = append(args, limit)
	rows, err := s.query(ctx, eventColumns+` WHERE 1 = 1`+where+`
		 ORDER BY started_at DESC, id DESC
		 LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("alerts: 读取事件失败: %w", err)
	}
	defer rows.Close()
	out := make([]Event, 0, 16)
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("alerts: 读取事件失败: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("alerts: 读取事件失败: %w", err)
	}
	return out, nil
}

// eventColumns 是事件行的选取列表，列表与单条读取共用一份。
//
// 共用不是为了少打字：两处各写一份时，加一列的代价是"改一处、忘一处"，而忘掉的那处
// 不会报错，只会少一个字段。COALESCE 只用在**可空且上层有零值语义**的列上：
// rule_id 被 SET NULL 后是"规则不在了"，对外是 0；acked_by 是空串而不是 NULL。
const eventColumns = `
		SELECT id, COALESCE(rule_id, 0), rule_name, scope, target_id, metric, comparison, threshold,
		       state, started_at, resolved_at, duration_seconds, peak_value, silenced,
		       acked_at, COALESCE(acked_by, ''), created_at
		  FROM alert_events`

// scanEvent 从一行读出事件。列顺序与 eventColumns 一致。
func scanEvent(sc scanner) (Event, error) {
	var (
		e                    Event
		resolvedAt, ackedAt  sql.NullString
		duration             sql.NullInt64
		peak                 sql.NullFloat64
		silenced             int64
		startedAt, createdAt string
	)
	if err := sc.Scan(&e.ID, &e.RuleID, &e.RuleName, &e.Scope, &e.TargetID, &e.Metric,
		&e.Comparison, &e.Threshold, &e.State, &startedAt, &resolvedAt, &duration,
		&peak, &silenced, &ackedAt, &e.AckedBy, &createdAt); err != nil {
		return Event{}, err
	}
	e.Silenced = silenced != 0
	var err error
	if e.StartedAt, err = parseTime(startedAt); err != nil {
		return Event{}, err
	}
	if e.CreatedAt, err = parseTime(createdAt); err != nil {
		return Event{}, err
	}
	if resolvedAt.Valid {
		if e.ResolvedAt, err = parseTime(resolvedAt.String); err != nil {
			return Event{}, err
		}
	}
	if ackedAt.Valid {
		if e.AckedAt, err = parseTime(ackedAt.String); err != nil {
			return Event{}, err
		}
	}
	if duration.Valid {
		e.DurationSeconds = int(duration.Int64)
	}
	if peak.Valid {
		v := peak.Float64
		e.PeakValue = &v
	}
	return e, nil
}

// GetEvent 读取一条事件；不存在时返回 ErrNotFound。
func (s *Service) GetEvent(ctx context.Context, id int64) (Event, error) {
	if id <= 0 {
		return Event{}, ErrNotFound
	}
	e, err := scanEvent(s.queryRow(ctx, eventColumns+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Event{}, ErrNotFound
	}
	if err != nil {
		return Event{}, fmt.Errorf("alerts: 读取事件失败: %w", err)
	}
	return e, nil
}

// AckEvent 把一条事件标记为"人已经看见了"（P13.4 的确认动作）。
//
// 三件事**刻意不做**：
//
//  1. 不改事件状态。确认是"我知道了"，不是"它好了"——机器还在超限就是还在超限，
//     让一次点击把 firing 改成 resolved，等于用人手伪造一个机器事实。
//  2. 不动规则的判定。确认与冷却、静默无关：它不让告警消失，只让"有没有人看过"
//     这件事有答案。
//  3. 不覆盖第一次确认。第二次点确认原样返回第一次的时间与人：留下"谁先看见的"
//     比更新成最后一个人更有用，而且这让接口天然幂等——重试一次不会改任何事实。
//
// 调用方负责把它放进事务里（api 层用 InTransaction），确认与审计要么一起落地、
// 要么一起没有。
func (s *Service) AckEvent(ctx context.Context, id int64, by string) (Event, error) {
	if id <= 0 {
		return Event{}, ErrNotFound
	}
	by = strings.TrimSpace(by)
	if by == "" {
		return Event{}, invalidRule("acked_by", "不能为空")
	}
	current, err := s.GetEvent(ctx, id)
	if err != nil {
		return Event{}, err
	}
	if !current.AckedAt.IsZero() {
		return current, nil
	}
	// acked_at IS NULL 这个条件是并发下的解法：两个人同一刻点确认，只有第一个写入，
	// 第二个的 UPDATE 匹配不到行（而不是把第一个人的名字改成自己的）。
	if _, err := s.exec(ctx, `
		UPDATE alert_events
		   SET acked_at = ?, acked_by = ?
		 WHERE id = ? AND acked_at IS NULL`,
		formatTime(s.nowUTC()), by, id); err != nil {
		return Event{}, fmt.Errorf("alerts: 确认事件失败: %w", err)
	}
	return s.GetEvent(ctx, id)
}

// −−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−
// 维护静默
// −−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−

// Silence 是全局维护静默窗口。零值窗口表示没有静默。
type Silence struct {
	StartsAt  time.Time
	EndsAt    time.Time
	Reason    string
	UpdatedAt time.Time
	UpdatedBy string
}

// ActiveAt 判断 now 是否落在静默窗口内（两端都含）。
func (s Silence) ActiveAt(now time.Time) bool {
	if s.StartsAt.IsZero() || s.EndsAt.IsZero() {
		return false
	}
	return !now.Before(s.StartsAt) && !now.After(s.EndsAt)
}

// GetSilence 读取当前的静默窗口。
func (s *Service) GetSilence(ctx context.Context) (Silence, error) {
	var (
		out              Silence
		startsAt, endsAt sql.NullString
		updatedAt        string
	)
	err := s.queryRow(ctx, `
		SELECT starts_at, ends_at, reason, updated_at, updated_by FROM alert_silence WHERE id = 1`).
		Scan(&startsAt, &endsAt, &out.Reason, &updatedAt, &out.UpdatedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return Silence{}, nil
	}
	if err != nil {
		return Silence{}, fmt.Errorf("alerts: 读取维护静默失败: %w", err)
	}
	if startsAt.Valid && endsAt.Valid {
		if out.StartsAt, err = parseTime(startsAt.String); err != nil {
			return Silence{}, err
		}
		if out.EndsAt, err = parseTime(endsAt.String); err != nil {
			return Silence{}, err
		}
	}
	if out.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return Silence{}, err
	}
	return out, nil
}

// SetSilence 设置或清除静默窗口。起止都为零值表示清除。
func (s *Service) SetSilence(ctx context.Context, in Silence, actor string) (Silence, error) {
	now := s.nowUTC()
	clear := in.StartsAt.IsZero() && in.EndsAt.IsZero()
	if !clear {
		if in.StartsAt.IsZero() || in.EndsAt.IsZero() {
			return Silence{}, invalidRule("silence", "起止时间必须同时给出或同时留空")
		}
		if !in.EndsAt.After(in.StartsAt) {
			return Silence{}, invalidRule("ends_at", "必须晚于 starts_at")
		}
		if in.EndsAt.Sub(in.StartsAt) > MaxSilenceDuration {
			return Silence{}, invalidRule("ends_at", "静默窗口不能超过 %d 天", int(MaxSilenceDuration.Hours()/24))
		}
		if len([]rune(in.Reason)) > MaxReasonLen {
			return Silence{}, invalidRule("reason", "长度不能超过 %d 个字符", MaxReasonLen)
		}
	}
	var startsAt, endsAt any
	reason := ""
	if !clear {
		startsAt = formatTime(in.StartsAt)
		endsAt = formatTime(in.EndsAt)
		reason = in.Reason
	}
	if _, err := s.exec(ctx, `
		UPDATE alert_silence SET starts_at = ?, ends_at = ?, reason = ?, updated_at = ?, updated_by = ?
		 WHERE id = 1`, startsAt, endsAt, reason, formatTime(now), actor); err != nil {
		return Silence{}, fmt.Errorf("alerts: 写入维护静默失败: %w", err)
	}
	return s.GetSilence(ctx)
}

// −−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−
// 小工具
// −−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−

// scanner 是 *sql.Row 与 *sql.Rows 的共同读法。
type scanner interface {
	Scan(dest ...any) error
}

func (s *Service) queryRow(ctx context.Context, query string, args ...any) *sql.Row {
	if tx, ok := store.TxFromContext(ctx); ok {
		return tx.QueryRowContext(ctx, query, args...)
	}
	return s.db.SQL().QueryRowContext(ctx, query, args...)
}

func (s *Service) query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if tx, ok := store.TxFromContext(ctx); ok {
		return tx.QueryContext(ctx, query, args...)
	}
	return s.db.SQL().QueryContext(ctx, query, args...)
}

func (s *Service) exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if tx, ok := store.TxFromContext(ctx); ok {
		return tx.ExecContext(ctx, query, args...)
	}
	return s.db.SQL().ExecContext(ctx, query, args...)
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func parseTime(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("alerts: 时间格式无效: %w", err)
	}
	return t.UTC(), nil
}

func fromUnix(v int64) time.Time {
	if v <= 0 {
		return time.Time{}
	}
	return time.Unix(v, 0).UTC()
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func nullString(sec *Secondary) any {
	if sec == nil {
		return nil
	}
	return sec.Metric
}

func nullComparison(sec *Secondary) any {
	if sec == nil {
		return nil
	}
	return sec.Comparison
}

func nullFloat(sec *Secondary) any {
	if sec == nil {
		return nil
	}
	return sec.Threshold
}
