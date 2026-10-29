package alerts

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"upanel/internal/store"
	"upanel/migrations"
)

// clock 是可推进的测试时钟。状态机的每个时间判断都要能在测试里精确落点，
// 用真实时间会让"持续 30 秒"变成"等 30 秒"。
type clock struct{ t time.Time }

func (c *clock) Now() time.Time          { return c.t }
func (c *clock) Advance(d time.Duration) { c.t = c.t.Add(d) }

// baseTime 是测试起点，对齐到 5 秒桶边界。
func baseTime() time.Time { return time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC) }

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// newTestService 构造跑在临时目录里的真实 SQLite 服务。
//
// 不用内存假实现：规则表的 CHECK、事件表的“一条规则最多一个未恢复事件”这个
// 部分唯一索引、以及外键 SET NULL 都只有真实数据库才验得出来。
func newTestService(t *testing.T) (*Service, *store.DB, *clock) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(context.Background(), store.Options{
		Path:   filepath.Join(dir, "upanel.db"),
		Logger: quietLogger(),
	})
	if err != nil {
		t.Fatalf("打开测试数据库失败: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("关闭测试数据库失败: %v", err)
		}
	})
	if _, err := db.Migrate(context.Background(), migrations.FS); err != nil {
		t.Fatalf("迁移测试数据库失败: %v", err)
	}
	clk := &clock{t: baseTime()}
	svc, err := New(db, Options{Now: clk.Now, Logger: quietLogger()})
	if err != nil {
		t.Fatalf("构造告警服务失败: %v", err)
	}
	return svc, db, clk
}

// seedPoints 按固定间隔写 n 个原始样本。
//
// 时间戳会被 InsertMetricSamples 向下对齐到 5 秒桶，所以起点与步长都取 5 秒的整数倍。
func seedPoints(t *testing.T, db *store.DB, scope, target, metric string, start time.Time, step time.Duration, n int, value float64) {
	t.Helper()
	samples := make([]store.MetricSample, 0, n)
	for i := 0; i < n; i++ {
		ts := start.Add(time.Duration(i) * step)
		samples = append(samples, store.MetricSample{
			Scope: scope, TargetID: target, Metric: metric,
			Resolution: store.MetricResolutionRaw,
			TS:         ts, Value: value, MinValue: value, MaxValue: value, SampleCount: 1,
		})
	}
	if _, err := db.InsertMetricSamples(context.Background(), samples); err != nil {
		t.Fatalf("写入测试样本失败: %v", err)
	}
}

// cpuRule 是一条 CPU 使用率规则：≥90 触发、<80 恢复、持续 30 秒、冷却 60 秒。
func cpuRule() RuleInput {
	return RuleInput{
		Name:              "CPU 持续高",
		Metric:            "cpu_usage_percent",
		Comparison:        store.CompareGTE,
		Threshold:         90,
		DurationSeconds:   30,
		RecoveryThreshold: 80,
		CooldownSeconds:   60,
		Enabled:           true,
	}
}

// −−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−
// 校验
// −−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−

func TestNormalizeRuleRejectsImpossibleRules(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*RuleInput)
		field  string
	}{
		{"未知指标", func(in *RuleInput) { in.Metric = "gpu_usage_percent" }, "metric"},
		{"主机指标带了目标", func(in *RuleInput) { in.TargetID = "eth0" }, "target_id"},
		{"网卡指标没给目标", func(in *RuleInput) {
			in.Metric = "rx_bytes_per_sec"
		}, "target_id"},
		{"比较方向不在闭集", func(in *RuleInput) { in.Comparison = "eq" }, "comparison"},
		{"持续太短", func(in *RuleInput) { in.DurationSeconds = 5 }, "duration_seconds"},
		{"持续超过一天", func(in *RuleInput) { in.DurationSeconds = 90000 }, "duration_seconds"},
		{"恢复线在错误的一侧", func(in *RuleInput) { in.RecoveryThreshold = 95 }, "recovery_threshold"},
		{"恢复线等于告警线（没有死区）", func(in *RuleInput) { in.RecoveryThreshold = 90 }, "recovery_threshold"},
		{"阈值为 NaN", func(in *RuleInput) { in.Threshold = math.NaN() }, "threshold"},
		{"冷却为负", func(in *RuleInput) { in.CooldownSeconds = -1 }, "cooldown_seconds"},
		{"次级指标跨 scope", func(in *RuleInput) {
			in.Secondary = &Secondary{Metric: "used_percent", Comparison: store.CompareGTE, Threshold: 85}
		}, "secondary_metric"},
		{"次级指标与主指标相同", func(in *RuleInput) {
			in.Secondary = &Secondary{Metric: "cpu_usage_percent", Comparison: store.CompareGTE, Threshold: 85}
		}, "secondary_metric"},
		{"组合方式不在闭集", func(in *RuleInput) { in.Combination = "xor" }, "combination"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := cpuRule()
			tc.mutate(&in)
			_, err := normalizeRule(in)
			if err == nil {
				t.Fatalf("期望被拒绝，实际通过了")
			}
			var invalid *InvalidRuleError
			if !errors.As(err, &invalid) {
				t.Fatalf("期望 InvalidRuleError，实际 %T: %v", err, err)
			}
			if invalid.Field != tc.field {
				t.Fatalf("字段 = %q，期望 %q（%s）", invalid.Field, tc.field, invalid.Message)
			}
		})
	}
}

func TestNormalizeRuleDerivesScopeFromMetric(t *testing.T) {
	in := cpuRule()
	rule, err := normalizeRule(in)
	if err != nil {
		t.Fatalf("合法规则被拒绝: %v", err)
	}
	if rule.Scope != store.MetricScopeHost {
		t.Fatalf("scope = %q，期望 host（由指标反查，不由调用方给）", rule.Scope)
	}
	if rule.Combination != CombinationAny {
		t.Fatalf("没有次级口径时组合方式应归一为 any，实际 %q", rule.Combination)
	}

	// 空间双阈值：使用率 85% 或剩余 10 GiB。
	fs := RuleInput{
		Name:              "根分区快满",
		Metric:            "used_percent",
		TargetID:          "/",
		Comparison:        store.CompareGTE,
		Threshold:         85,
		Secondary:         &Secondary{Metric: "available_bytes", Comparison: store.CompareLTE, Threshold: 10 << 30},
		Combination:       CombinationAny,
		DurationSeconds:   300,
		RecoveryThreshold: 80,
		CooldownSeconds:   600,
		Enabled:           true,
	}
	rule, err = normalizeRule(fs)
	if err != nil {
		t.Fatalf("双口径规则被拒绝: %v", err)
	}
	if rule.Scope != store.MetricScopeFilesystem || rule.Secondary == nil {
		t.Fatalf("双口径规则解析结果不对: %+v", rule)
	}
	if rule.Secondary.Metric != "available_bytes" || rule.Secondary.Comparison != store.CompareLTE {
		t.Fatalf("次级口径 = %+v", rule.Secondary)
	}
}

// −−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−
// 状态机（纯函数）
// −−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−

// full 是“窗口里每个样本都在线外”的统计。
func full(n int) store.MetricWindowStat {
	return store.MetricWindowStat{
		Samples: n, Over: n, Recovered: 0,
		Oldest: baseTime().Add(-30 * time.Second), Newest: baseTime(), Peak: 95,
	}
}

// recoveredStat 是“窗口里每个样本都回到恢复线内”的统计。
func recoveredStat(n int) store.MetricWindowStat {
	return store.MetricWindowStat{
		Samples: n, Over: 0, Recovered: n,
		Oldest: baseTime().Add(-30 * time.Second), Newest: baseTime(), Peak: 50,
	}
}

// bandStat 是“窗口里每个样本都落在两条线之间”的统计：既不超限也没恢复。
func bandStat(n int) store.MetricWindowStat {
	return store.MetricWindowStat{
		Samples: n, Over: 0, Recovered: 0,
		Oldest: baseTime().Add(-30 * time.Second), Newest: baseTime(), Peak: 85,
	}
}

func okRule() Rule {
	rule, err := normalizeRule(cpuRule())
	if err != nil {
		panic(err)
	}
	rule.ID = 1
	rule.State = StateOK
	rule.Version = 1
	return rule
}

func TestDecideTransitions(t *testing.T) {
	now := baseTime()
	cases := []struct {
		name        string
		rule        Rule
		obs         Observation
		wantState   string
		wantFire    bool
		wantResolve bool
	}{
		{
			name:      "ok 遇到全部超限 → pending",
			rule:      okRule(),
			obs:       Observation{Now: now, Primary: full(7)},
			wantState: StatePending,
		},
		{
			name: "pending 还没持续够 → 保持 pending",
			rule: func() Rule {
				r := okRule()
				r.State = StatePending
				r.StateSince = now.Add(-10 * time.Second)
				return r
			}(),
			obs:       Observation{Now: now, Primary: full(7)},
			wantState: StatePending,
		},
		{
			name: "pending 持续够 → firing 并开事件",
			rule: func() Rule {
				r := okRule()
				r.State = StatePending
				r.StateSince = now.Add(-31 * time.Second)
				return r
			}(),
			obs:       Observation{Now: now, Primary: full(7)},
			wantState: StateFiring,
			wantFire:  true,
		},
		{
			name: "pending 被打断 → 回到 ok",
			rule: func() Rule {
				r := okRule()
				r.State = StatePending
				r.StateSince = now.Add(-25 * time.Second)
				return r
			}(),
			obs:       Observation{Now: now, Primary: recoveredStat(7)},
			wantState: StateOK,
		},
		{
			name: "firing 刚回到恢复线内 → 仍在 firing，开始计恢复",
			rule: func() Rule {
				r := okRule()
				r.State = StateFiring
				r.StateSince = now.Add(-10 * time.Minute)
				return r
			}(),
			obs:       Observation{Now: now, Primary: recoveredStat(7)},
			wantState: StateFiring,
		},
		{
			name: "firing 恢复持续够 → 回到 ok 并收口事件",
			rule: func() Rule {
				r := okRule()
				r.State = StateFiring
				r.StateSince = now.Add(-10 * time.Minute)
				r.RecoverSince = now.Add(-31 * time.Second)
				return r
			}(),
			obs:         Observation{Now: now, Primary: recoveredStat(7)},
			wantState:   StateOK,
			wantResolve: true,
		},
		{
			name: "firing 落进死区 → 清掉恢复计时，事件继续开着",
			rule: func() Rule {
				r := okRule()
				r.State = StateFiring
				r.StateSince = now.Add(-10 * time.Minute)
				r.RecoverSince = now.Add(-20 * time.Second)
				return r
			}(),
			obs:       Observation{Now: now, Primary: bandStat(7)},
			wantState: StateFiring,
		},
		{
			name: "冷却期内重新超限 → 不重新起算",
			rule: func() Rule {
				r := okRule()
				r.CooldownUntil = now.Add(30 * time.Second)
				return r
			}(),
			obs:       Observation{Now: now, Primary: full(7)},
			wantState: StateOK,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := decide(tc.rule, tc.obs)
			if out.State != tc.wantState {
				t.Fatalf("state = %q，期望 %q（reason=%s）", out.State, tc.wantState, out.Reason)
			}
			if out.Fire != tc.wantFire {
				t.Fatalf("Fire = %v，期望 %v", out.Fire, tc.wantFire)
			}
			if out.Resolve != tc.wantResolve {
				t.Fatalf("Resolve = %v，期望 %v", out.Resolve, tc.wantResolve)
			}
			if tc.wantResolve && out.CooldownUntil.IsZero() {
				t.Fatalf("恢复之后必须设置冷却到期时间")
			}
		})
	}
}

func TestDecideKeepsReasonWhileCounting(t *testing.T) {
	now := baseTime()

	// pending 的第二轮：状态和历史都不变，但 last_reason 不能被清成空串。
	// 空着的那一栏读起来像“这条规则什么都没做”，而它其实正在攒持续。
	pending := okRule()
	pending.State = StatePending
	pending.StateSince = now.Add(-10 * time.Second)
	out := decide(pending, Observation{Now: now, Primary: full(7)})
	if out.State != StatePending {
		t.Fatalf("state = %q，期望 pending", out.State)
	}
	if out.Reason == "" {
		t.Fatalf("pending 的第二轮把 last_reason 清空了：界面会显示成“什么都没发生”")
	}
	if !strings.Contains(out.Reason, "还需") {
		t.Fatalf("pending 的 last_reason 应该说清还差多久，实际 %q", out.Reason)
	}

	// firing 里正在往回走的那几轮同理。
	firing := okRule()
	firing.State = StateFiring
	firing.StateSince = now.Add(-10 * time.Minute)
	firing.RecoverSince = now.Add(-10 * time.Second)
	out = decide(firing, Observation{Now: now, Primary: recoveredStat(7)})
	if out.State != StateFiring || out.Resolve {
		t.Fatalf("恢复还没持续够就不该收口（state=%s resolve=%v）", out.State, out.Resolve)
	}
	if !strings.Contains(out.Reason, "恢复持续") {
		t.Fatalf("恢复期的 last_reason 应该说清在等恢复持续，实际 %q", out.Reason)
	}
}

func TestDecideMissingDataNeverChangesState(t *testing.T) {
	now := baseTime()
	firing := okRule()
	firing.State = StateFiring
	firing.StateSince = now.Add(-10 * time.Minute)

	stale := full(7)
	stale.Newest = now.Add(-90 * time.Second) // 采集停了

	young := full(3)
	young.Oldest = now.Add(-10 * time.Second) // 窗口前段还没攒够

	cases := []struct {
		name string
		stat store.MetricWindowStat
	}{
		{"窗口里没有任何样本", store.MetricWindowStat{}},
		{"最新样本已经过期", stale},
		{"窗口前段没有样本", young},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, state := range []string{StateOK, StatePending, StateFiring} {
				rule := firing
				rule.State = state
				out := decide(rule, Observation{Now: now, Primary: tc.stat})
				if out.State != state {
					t.Fatalf("状态从 %s 变成了 %s：数据不足时不该下任何结论", state, out.State)
				}
				if out.Fire || out.Resolve {
					t.Fatalf("数据不足时不该触发也不该恢复（Fire=%v Resolve=%v）", out.Fire, out.Resolve)
				}
				if out.Reason == "" {
					t.Fatalf("数据不足时必须写出原因，否则“规则为什么不响”就没有答案")
				}
			}
		})
	}
}

func TestDecideDoubleThresholdCombination(t *testing.T) {
	now := baseTime()
	primaryOK := recoveredStat(7) // 使用率不高
	secondaryOver := store.MetricWindowStat{
		Samples: 7, Over: 7, Oldest: now.Add(-30 * time.Second), Newest: now, Peak: 3 << 30,
	}
	bothFull := full(7)

	doubleRule := func(combination string) Rule {
		r := okRule()
		r.Metric = "used_percent"
		r.Scope = store.MetricScopeFilesystem
		r.TargetID = "/"
		r.Secondary = &Secondary{Metric: "available_bytes", Comparison: store.CompareLTE, Threshold: 10 << 30}
		r.Combination = combination
		return r
	}

	t.Run("any：只有次级超限也触发", func(t *testing.T) {
		out := decide(doubleRule(CombinationAny), Observation{
			Now: now, Primary: primaryOK, Secondary: secondaryOver, HasSecondary: true,
		})
		if out.State != StatePending {
			t.Fatalf("state = %q，期望 pending（任一满足就触发）", out.State)
		}
		if out.Peak == nil || *out.Peak != float64(3<<30) {
			t.Fatalf("显示值应取真正超限的那个口径（剩余字节），实际 %v", out.Peak)
		}
	})

	t.Run("all：只有一个条件成立不算", func(t *testing.T) {
		out := decide(doubleRule(CombinationAll), Observation{
			Now: now, Primary: primaryOK, Secondary: secondaryOver, HasSecondary: true,
		})
		if out.State != StateOK {
			t.Fatalf("state = %q，期望 ok（全部满足才触发）", out.State)
		}
	})

	t.Run("all：两个条件都成立才触发", func(t *testing.T) {
		out := decide(doubleRule(CombinationAll), Observation{
			Now: now, Primary: bothFull, Secondary: secondaryOver, HasSecondary: true,
		})
		if out.State != StatePending {
			t.Fatalf("state = %q，期望 pending", out.State)
		}
	})

	t.Run("any：主口径恢复了但次级还在限外，不算恢复", func(t *testing.T) {
		rule := doubleRule(CombinationAny)
		rule.State = StateFiring
		rule.StateSince = now.Add(-10 * time.Minute)
		out := decide(rule, Observation{
			Now: now, Primary: primaryOK, Secondary: secondaryOver, HasSecondary: true,
		})
		if out.State != StateFiring || out.Resolve {
			t.Fatalf("还有一个口径在限外时不该收口事件（state=%q resolve=%v）", out.State, out.Resolve)
		}
	})
}

// −−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−
// 落库与评估
// −−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−

func TestRuleCRUDVersionConflict(t *testing.T) {
	svc, _, clk := newTestService(t)
	ctx := context.Background()

	created, err := svc.Create(ctx, cpuRule())
	if err != nil {
		t.Fatalf("创建规则失败: %v", err)
	}
	if created.Version != 1 || created.State != StateOK || created.Scope != store.MetricScopeHost {
		t.Fatalf("新建规则 = %+v", created)
	}

	update := cpuRule()
	update.Name = "CPU 更严格的线"
	update.Threshold = 95
	update.RecoveryThreshold = 85
	if _, err := svc.Update(ctx, created.ID, 99, update); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("版本不匹配时应回 ErrVersionConflict，实际 %v", err)
	}

	clk.Advance(time.Minute)
	updated, err := svc.Update(ctx, created.ID, created.Version, update)
	if err != nil {
		t.Fatalf("更新规则失败: %v", err)
	}
	if updated.Version != 2 || updated.Name != "CPU 更严格的线" || updated.Threshold != 95 {
		t.Fatalf("更新结果 = %+v", updated)
	}

	list, err := svc.List(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("规则列表 = %v, %v", list, err)
	}

	if err := svc.Delete(ctx, created.ID, 1); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("用旧版本删除应回 ErrVersionConflict，实际 %v", err)
	}
	if err := svc.Delete(ctx, created.ID, 2); err != nil {
		t.Fatalf("删除规则失败: %v", err)
	}
	if _, err := svc.Get(ctx, created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("删除后应 ErrNotFound，实际 %v", err)
	}
}

func TestEvaluateFiresOnceResolvesOnceAndCoolsDown(t *testing.T) {
	svc, db, clk := newTestService(t)
	ctx := context.Background()
	start := baseTime()

	rule, err := svc.Create(ctx, cpuRule())
	if err != nil {
		t.Fatalf("创建规则失败: %v", err)
	}

	// 窗口 [start-30, start] 里全是 95%：超限，但还只是第一轮。
	seedPoints(t, db, store.MetricScopeHost, "", "cpu_usage_percent", start.Add(-30*time.Second), 5*time.Second, 7, 95)
	report, err := svc.EvaluateOnce(ctx)
	if err != nil {
		t.Fatalf("第一轮评估失败: %v", err)
	}
	if report.Fired != 0 {
		t.Fatalf("第一轮就触发等于绕过了 duration_seconds")
	}
	got, _ := svc.Get(ctx, rule.ID)
	if got.State != StatePending {
		t.Fatalf("第一轮之后状态 = %q，期望 pending", got.State)
	}

	// 再过 30 秒，窗口里依然全是 95%：持续够了，触发。
	clk.Advance(30 * time.Second)
	now := clk.Now()
	seedPoints(t, db, store.MetricScopeHost, "", "cpu_usage_percent", now.Add(-30*time.Second), 5*time.Second, 7, 95)
	report, err = svc.EvaluateOnce(ctx)
	if err != nil {
		t.Fatalf("第二轮评估失败: %v", err)
	}
	if report.Fired != 1 {
		t.Fatalf("持续够之后应触发一次，实际 %d", report.Fired)
	}
	events, err := svc.ListEvents(ctx, EventsQuery{})
	if err != nil || len(events) != 1 {
		t.Fatalf("事件列表 = %v, %v", events, err)
	}
	if events[0].State != EventFiring || events[0].RuleID != rule.ID {
		t.Fatalf("事件 = %+v", events[0])
	}

	// 再评估一轮，条件不变：不该再产生第二个事件（去重）。
	clk.Advance(15 * time.Second)
	now = clk.Now()
	seedPoints(t, db, store.MetricScopeHost, "", "cpu_usage_percent", now.Add(-15*time.Second), 5*time.Second, 4, 95)
	if _, err := svc.EvaluateOnce(ctx); err != nil {
		t.Fatalf("第三轮评估失败: %v", err)
	}
	if events, _ := svc.ListEvents(ctx, EventsQuery{}); len(events) != 1 {
		t.Fatalf("条件没变化时不该再开事件，实际 %d 条", len(events))
	}

	// 掉回 50%：进入恢复计时，这一轮还不算恢复。
	clk.Advance(30 * time.Second)
	now = clk.Now()
	seedPoints(t, db, store.MetricScopeHost, "", "cpu_usage_percent", now.Add(-30*time.Second), 5*time.Second, 7, 50)
	if _, err := svc.EvaluateOnce(ctx); err != nil {
		t.Fatalf("恢复第一轮评估失败: %v", err)
	}
	if got, _ := svc.Get(ctx, rule.ID); got.State != StateFiring {
		t.Fatalf("恢复刚开始时状态 = %q，期望仍是 firing", got.State)
	}

	// 恢复也持续够 30 秒：收口事件，进入冷却。
	clk.Advance(30 * time.Second)
	now = clk.Now()
	seedPoints(t, db, store.MetricScopeHost, "", "cpu_usage_percent", now.Add(-30*time.Second), 5*time.Second, 7, 50)
	report, err = svc.EvaluateOnce(ctx)
	if err != nil {
		t.Fatalf("恢复第二轮评估失败: %v", err)
	}
	if report.Resolved != 1 {
		t.Fatalf("恢复持续够之后应收口一次，实际 %d", report.Resolved)
	}
	events, _ = svc.ListEvents(ctx, EventsQuery{})
	if len(events) != 1 || events[0].State != EventResolved || events[0].ResolvedAt.IsZero() {
		t.Fatalf("恢复后事件行 = %+v", events)
	}
	if events[0].DurationSeconds < 30 {
		t.Fatalf("事件持续时长 = %d，至少要覆盖触发的那个持续窗口", events[0].DurationSeconds)
	}

	// 冷却期内又超限：不重新起算（否则阈值贴着常态时会刷屏）。
	clk.Advance(15 * time.Second)
	now = clk.Now()
	seedPoints(t, db, store.MetricScopeHost, "", "cpu_usage_percent", now.Add(-15*time.Second), 5*time.Second, 4, 99)
	report, err = svc.EvaluateOnce(ctx)
	if err != nil {
		t.Fatalf("冷却期评估失败: %v", err)
	}
	if report.Fired != 0 {
		t.Fatalf("冷却期内不该再触发")
	}
	if got, _ := svc.Get(ctx, rule.ID); got.State != StateOK {
		t.Fatalf("冷却期内状态 = %q，期望 ok", got.State)
	}
}

func TestSilenceFlagsEventsButDoesNotStopDetection(t *testing.T) {
	svc, db, clk := newTestService(t)
	ctx := context.Background()
	start := baseTime()

	if _, err := svc.Create(ctx, cpuRule()); err != nil {
		t.Fatalf("创建规则失败: %v", err)
	}
	if _, err := svc.SetSilence(ctx, Silence{
		StartsAt: start.Add(-time.Minute),
		EndsAt:   start.Add(time.Hour),
		Reason:   "内核升级",
	}, "alice"); err != nil {
		t.Fatalf("设置维护静默失败: %v", err)
	}
	if s, _ := svc.GetSilence(ctx); !s.ActiveAt(start) {
		t.Fatalf("静默窗口应当对当前时刻生效: %+v", s)
	}

	seedPoints(t, db, store.MetricScopeHost, "", "cpu_usage_percent", start.Add(-30*time.Second), 5*time.Second, 7, 95)
	if _, err := svc.EvaluateOnce(ctx); err != nil {
		t.Fatalf("第一轮评估失败: %v", err)
	}
	clk.Advance(30 * time.Second)
	now := clk.Now()
	seedPoints(t, db, store.MetricScopeHost, "", "cpu_usage_percent", now.Add(-30*time.Second), 5*time.Second, 7, 95)
	if _, err := svc.EvaluateOnce(ctx); err != nil {
		t.Fatalf("第二轮评估失败: %v", err)
	}
	events, _ := svc.ListEvents(ctx, EventsQuery{})
	if len(events) != 1 {
		t.Fatalf("静默期间事件仍然要记录，实际 %d 条", len(events))
	}
	if !events[0].Silenced {
		t.Fatalf("静默期间产生的事件应带 silenced 标记")
	}
}

func TestDeleteRuleKeepsEventHistory(t *testing.T) {
	svc, db, clk := newTestService(t)
	ctx := context.Background()
	start := baseTime()

	rule, err := svc.Create(ctx, cpuRule())
	if err != nil {
		t.Fatalf("创建规则失败: %v", err)
	}
	seedPoints(t, db, store.MetricScopeHost, "", "cpu_usage_percent", start.Add(-30*time.Second), 5*time.Second, 7, 95)
	if _, err := svc.EvaluateOnce(ctx); err != nil {
		t.Fatalf("第一轮评估失败: %v", err)
	}
	clk.Advance(30 * time.Second)
	now := clk.Now()
	seedPoints(t, db, store.MetricScopeHost, "", "cpu_usage_percent", now.Add(-30*time.Second), 5*time.Second, 7, 95)
	if _, err := svc.EvaluateOnce(ctx); err != nil {
		t.Fatalf("第二轮评估失败: %v", err)
	}

	if err := svc.Delete(ctx, rule.ID, rule.Version); err != nil {
		t.Fatalf("删除规则失败: %v", err)
	}
	events, err := svc.ListEvents(ctx, EventsQuery{})
	if err != nil {
		t.Fatalf("读取事件失败: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("删规则不该删掉历史事件，实际 %d 条", len(events))
	}
	if events[0].RuleID != 0 {
		t.Fatalf("规则删除后事件的 rule_id 应为 0（外键置空），实际 %d", events[0].RuleID)
	}
	if events[0].State != EventResolved {
		t.Fatalf("删规则时打开的事件要收口，实际 %q", events[0].State)
	}
	if events[0].RuleName != "CPU 持续高" {
		t.Fatalf("规则快照丢了: %+v", events[0])
	}
}

func TestSilenceValidation(t *testing.T) {
	svc, _, clk := newTestService(t)
	ctx := context.Background()
	now := clk.Now()

	if _, err := svc.SetSilence(ctx, Silence{StartsAt: now}, "alice"); err == nil {
		t.Fatalf("只给起点应当被拒绝")
	}
	if _, err := svc.SetSilence(ctx, Silence{
		StartsAt: now, EndsAt: now.Add(-time.Minute),
	}, "alice"); err == nil {
		t.Fatalf("终点早于起点应当被拒绝")
	}
	if _, err := svc.SetSilence(ctx, Silence{
		StartsAt: now, EndsAt: now.Add(MaxSilenceDuration + time.Hour),
	}, "alice"); err == nil {
		t.Fatalf("超过 7 天的静默窗口应当被拒绝")
	}

	// 空窗口 = 清除。
	silence, err := svc.SetSilence(ctx, Silence{}, "alice")
	if err != nil {
		t.Fatalf("清除静默失败: %v", err)
	}
	if !silence.StartsAt.IsZero() || silence.ActiveAt(now) {
		t.Fatalf("清除后不应还有窗口: %+v", silence)
	}
}

// −−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−
// 采集节奏（P13.2 验收在真机上撞出来的）
// −−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−
//
// 容量序列（used_percent / inode_used_percent / available_bytes）走 storage 的
// 30 秒缓存，30 秒才落一行；主机与网卡序列 5 秒一行。判定侧把两者当成一条节奏
// 之后，一条空间规则的表象是“配好了永远不响”，而 last_reason 写的是
// “窗口内没有任何样本”——数据明明写着，人被这句话带去查采集。

func TestMinDurationCountsTwoCadences(t *testing.T) {
	// 5 秒节奏的序列算出来 10 秒，地板仍是 MinDurationSeconds。
	if need, slow := minDurationFor("cpu_usage_percent", nil); need != MinDurationSeconds || slow != "cpu_usage_percent" {
		t.Fatalf("主机指标的地板应当是 %d 秒（由 cpu_usage_percent 决定），实际 %d/%s", MinDurationSeconds, need, slow)
	}
	// 容量序列 30 秒一个点，两个节奏 = 60 秒。
	if need, slow := minDurationFor("used_percent", nil); need != 60 || slow != "used_percent" {
		t.Fatalf("容量序列的地板应当是 60 秒，实际 %d/%s", need, slow)
	}
	// 主次口径混用时，最慢的那个说了算。
	need, slow := minDurationFor("used_percent", &Secondary{Metric: "available_bytes"})
	if need != 60 || slow != "used_percent" {
		t.Fatalf("主次口径都是容量序列时应取 60 秒，实际 %d/%s", need, slow)
	}
}

func TestNormalizeRuleRejectsDurationShorterThanOneCadence(t *testing.T) {
	in := RuleInput{
		Name:              "空间使用率高",
		Metric:            "used_percent",
		TargetID:          "/",
		Comparison:        store.CompareGTE,
		Threshold:         85,
		DurationSeconds:   30,
		RecoveryThreshold: 70,
		Enabled:           true,
	}
	rule, err := normalizeRule(in)
	if err == nil {
		t.Fatalf("容量规则持续 30 秒应当被拒：一个 30 秒的窗口装不下两个 30 秒节奏的样本")
	}
	if !IsInvalidRule(err) {
		t.Fatalf("应当是规则非法错误，实际 %v", err)
	}
	var invalid *InvalidRuleError
	if !errors.As(err, &invalid) || invalid.Field != "duration_seconds" {
		t.Fatalf("错误应当点在 duration_seconds 上，实际 %v", err)
	}

	in.DurationSeconds = 60
	rule, err = normalizeRule(in)
	if err != nil {
		t.Fatalf("容量规则持续 60 秒应当成立，实际 %v", err)
	}
	if rule.Scope != store.MetricScopeFilesystem || rule.TargetID != "/" {
		t.Fatalf("scope/target 推导错了: %+v", rule)
	}
}

func TestCoverageFollowsTheSeriesCadence(t *testing.T) {
	now := baseTime()
	fsRule := Rule{
		Metric: "used_percent", Scope: store.MetricScopeFilesystem, TargetID: "/",
		Comparison: store.CompareGTE, Threshold: 85, RecoveryThreshold: 70,
		DurationSeconds: 60, State: StateOK,
	}
	// 窗口 (now-60, now]，只有两个点：now-40 与 now-10。
	// 对 5 秒节奏的序列，窗口前 20 秒没有点叫“没攒够”；对 30 秒节奏的容量序列，
	// 前 40 秒没有点是**常态**，必须照样下结论。
	twoPoints := store.MetricWindowStat{
		Samples: 2, Over: 2, Recovered: 0,
		Oldest: now.Add(-40 * time.Second), Newest: now.Add(-10 * time.Second), Peak: 91,
	}
	if got := decide(fsRule, Observation{Now: now, Primary: twoPoints}); got.State != StatePending {
		t.Fatalf("容量序列两个点都在限外时应当开始计持续，实际 state=%s reason=%s", got.State, got.Reason)
	}

	hostRule := fsRule
	hostRule.Metric, hostRule.Scope, hostRule.TargetID = "cpu_usage_percent", store.MetricScopeHost, ""
	got := decide(hostRule, Observation{Now: now, Primary: twoPoints})
	if got.State != StateOK || !strings.Contains(got.Reason, "窗口前段") {
		t.Fatalf("同样的形状换到 5 秒节奏的序列上应当判成“前段没攒够”，实际 state=%s reason=%s", got.State, got.Reason)
	}

	// 过期界线同样按节奏走：容量序列 90 秒内都算新鲜，主机序列 20 秒就过期。
	staleFS := twoPoints
	staleFS.Newest = now.Add(-100 * time.Second)
	if got := decide(fsRule, Observation{Now: now, Primary: staleFS}); !strings.Contains(got.Reason, "过期") {
		t.Fatalf("容量序列 100 秒没更新应当判成过期，实际 reason=%s", got.Reason)
	}
	freshHost := twoPoints
	// 最早那个点贴着窗口起点（前段够），只有“最新”太旧——这样过的就不是前段那道闸。
	freshHost.Oldest = now.Add(-55 * time.Second)
	freshHost.Newest = now.Add(-25 * time.Second)
	if got := decide(hostRule, Observation{Now: now, Primary: freshHost}); !strings.Contains(got.Reason, "过期") {
		t.Fatalf("主机序列 25 秒没更新应当判成过期，实际 reason=%s", got.Reason)
	}
}
