// Package alerts 是告警规则与告警事件的唯一入口（P13.2）。
//
// 它只做三件事：规则怎么定义（本文件）、规则与事件怎么存（store.go）、以及
// “现在这一刻该不该报”（evaluator.go）。发通知（SMTP / webhook）不在这里，
// 那是 P13.3：本包产出的**事件**才是通知的输入，通知自己记自己那本账。
//
// 三条贯穿整个包的口径，先写在这里，后面每个函数都按它们办事：
//
//  1. **没有数据不是 0。** 窗口里没有样本、样本太旧、窗口早期还没攒够——这三种情况
//     都不是“现在没超限”，而是“现在不知道”。不知道的时候状态机原地不动：不触发、
//     也不恢复。采集停了十分钟就自动“恢复”一次的告警，比没有告警更坏。
//
//  2. **持续是一个区间的事实，不是一刻的事实。** duration_seconds 内的**每一个**
//     样本都要在线外才算超限；有一个样本落回线内就从 pending 掉回 ok。用区间平均值
//     判断会把“冲高 30 秒又落回来”抹平，那种尖峰是运维最需要看见的东西。
//
//  3. **恢复要跨过另一条线。** recovery_threshold 不是阈值的复述，它与阈值之间
//     有一条带子：带子里的样本既不算超限、也不算恢复。这条带子就是防刷屏的全部
//     机制——没有它，值在阈值上下抖两下就会产生一串“触发/恢复/触发/恢复”。
package alerts

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"upanel/internal/metrics"
	"upanel/internal/store"
)

// 规则状态的闭集，与 migrations/0012_alerts.sql 的 CHECK 一致。
const (
	// StateOK 表示当前没有超限，也没有正在攒的持续期。
	StateOK = "ok"
	// StatePending 表示已经超限，但还没持续够 duration_seconds。
	StatePending = "pending"
	// StateFiring 表示已触发，且有一个未恢复的事件行。
	StateFiring = "firing"
)

// 事件状态的闭集。
const (
	EventFiring   = "firing"
	EventResolved = "resolved"
)

// 组合方式（仅在规则带次级口径时有意义）。
const (
	// CombinationAny 是“任一满足就触发”：使用率超线**或**剩余空间低于线。
	CombinationAny = "any"
	// CombinationAll 是“全部满足才触发”，用于两个条件同时成立才算真的不够用。
	CombinationAll = "all"
)

// 取值边界。
const (
	// MinDurationSeconds 是持续时间下限。低于 30 秒就是在报瞬时抖动。
	MinDurationSeconds = 30
	// MaxDurationSeconds 是持续时间上限：等于 raw 粒度的保留窗口（24 小时）。
	//
	// 超过它就没有原始点可数了。与其让规则悄悄退化到分钟聚合值（那里平均值会把
	// 尖峰抹平，判据和用户以为的完全不是一回事），不如在写入时就拒绝。
	MaxDurationSeconds = 86400
	// MaxCooldownSeconds 是冷却上限。
	MaxCooldownSeconds = 86400
	// MaxRules 是规则总数上限。
	//
	// 评估循环每 15 秒对每条启用规则做一次（最多两次）窗口统计，200 条规则在单台
	// 机器上是稳妥的量级；再往上该考虑的是“是不是把该合并的规则写重复了”，
	// 而不是继续加。上限写在这里是为了让拒绝发生在写入时，而不是让评估循环慢慢
	// 拖垮面板。
	MaxRules = 200
	// MaxNameLen 是规则名上限，与迁移里的 CHECK 一致。
	MaxNameLen = 120
	// MaxTargetIDLen 是 target_id 上限，与 metric_samples.target_id 一致。
	MaxTargetIDLen = 512
	// MaxReasonLen 是 last_reason 上限，与迁移里的 CHECK 一致。
	MaxReasonLen = 200
)

// MaxSilenceDuration 是单个维护静默窗口的上限（7 天）。
//
// 不是技术上做不到更长，而是“设了一周静默”几乎总是忘记关。7 天已经能盖住一次
// 长假，再长就该重新想想要不要直接停用规则（那会留痕在规则上，而不是藏在一个
// 时间窗里）。
const MaxSilenceDuration = 7 * 24 * time.Hour

// 错误。
var (
	// ErrNotFound 表示规则或事件不存在。
	ErrNotFound = errors.New("alerts: 对象不存在")
	// ErrVersionConflict 表示 version 不匹配（规则已被别人改过）。
	ErrVersionConflict = errors.New("alerts: 规则已被修改，请重新读取后再提交")
	// ErrLimitReached 表示规则数量达到上限。
	ErrLimitReached = errors.New("alerts: 规则数量已达上限")
)

// InvalidRuleError 是一条规则不合法。Field 用来在接口层指出是哪一项。
type InvalidRuleError struct {
	Field   string
	Message string
}

func (e *InvalidRuleError) Error() string {
	if e.Field == "" {
		return "alerts: " + e.Message
	}
	return "alerts: " + e.Field + " " + e.Message
}

// IsInvalidRule 判断错误是否属于规则非法。
func IsInvalidRule(err error) bool {
	var target *InvalidRuleError
	return errors.As(err, &target)
}

func invalidRule(field, format string, args ...any) error {
	return &InvalidRuleError{Field: field, Message: fmt.Sprintf(format, args...)}
}

// Secondary 是空间双阈值里的第二口径。
//
// 它必须有**自己的指标名**：PLAN.md 第 18 节要的是“使用率 85% 或剩余 10 GiB”，
// 第二个量的量纲是字节，不是百分比。原契约里只有一个 secondary_threshold，
// 拿它配 used_percent 的量纲去理解，就成了“剩余 10%”——那是另一条规则。
type Secondary struct {
	Metric     string
	Comparison string
	Threshold  float64
}

// RuleInput 是创建/修改规则时调用方给出的内容（不含 id、version 与状态）。
type RuleInput struct {
	Name              string
	TargetID          string
	Metric            string
	Comparison        string
	Threshold         float64
	Secondary         *Secondary
	Combination       string
	DurationSeconds   int
	RecoveryThreshold float64
	CooldownSeconds   int
	Enabled           bool
}

// Rule 是一条已落库的规则，含它的当前状态机。
type Rule struct {
	ID       int64
	Name     string
	Scope    string
	TargetID string
	Metric   string
	// Comparison 是告警方向（gt/gte/lt/lte），与 store.IsKnownComparison 一致。
	Comparison string
	Threshold  float64
	// Secondary 为 nil 表示单阈值规则。
	Secondary   *Secondary
	Combination string
	// DurationSeconds 是“持续多久才算数”。
	DurationSeconds int
	// RecoveryThreshold 是主口径的恢复线，必须落在阈值的安全侧。
	RecoveryThreshold float64
	CooldownSeconds   int
	Enabled           bool
	// Version 是乐观并发版本，只在**用户改动**时递增；状态机的迁移不动它。
	Version int64
	// 状态机。
	State         string
	StateSince    time.Time
	RecoverSince  time.Time
	CooldownUntil time.Time
	LastEvalAt    time.Time
	LastValue     *float64
	LastReason    string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Target 返回规则指向的 (scope, target_id)，也就是统计窗口要查的那一行。
func (r Rule) Target() (string, string) { return r.Scope, r.TargetID }

// Window 返回在 now 这一刻要统计的时间区间。
//
// 右端**含** now 对应的那一秒（store.MetricWindowStats 是 (From, To]）：
// 最新那个桶正是“此刻还超不超”的唯一依据，不能因为是半开区间就被切掉。
func (r Rule) Window(now time.Time) (time.Time, time.Time) {
	return now.Add(-time.Duration(r.DurationSeconds) * time.Second), now
}

// RecoveryComparison 返回主口径的恢复方向，是告警方向的镜像。
//
// gte 的恢复是 lt、gt 的恢复是 lte，以此类推。恢复线本身由 RecoveryThreshold 给出，
// 所以两条线之间的带子是 [recovery_threshold, threshold)。
func RecoveryComparison(comparison string) string {
	switch comparison {
	case store.CompareGT:
		return store.CompareLTE
	case store.CompareGTE:
		return store.CompareLT
	case store.CompareLT:
		return store.CompareGTE
	case store.CompareLTE:
		return store.CompareGT
	default:
		return ""
	}
}

// normalizeRule 校验并规范化一份输入，返回可以直接落库的 Rule（不含 id/状态）。
//
// 所有“能不能这么配”的判断都在这里，因为它是唯一一处既能被接口层调用、
// 又能被测试直接调用的地方。散在 handler 里校验，等第二个调用方出现时就会漏。
func normalizeRule(in RuleInput) (Rule, error) {
	var out Rule

	name := strings.TrimSpace(in.Name)
	if name == "" {
		return out, invalidRule("name", "不能为空")
	}
	if len([]rune(name)) > MaxNameLen {
		return out, invalidRule("name", "长度不能超过 %d 个字符", MaxNameLen)
	}
	out.Name = name

	metric := strings.TrimSpace(in.Metric)
	scope, ok := scopeOfMetric(metric)
	if !ok {
		return out, invalidRule("metric",
			"%q 不是已登记的指标（可选值见 GET /api/v1/system/metrics/status 的 series 字段）", metric)
	}
	out.Metric = metric
	out.Scope = scope

	target := strings.TrimSpace(in.TargetID)
	switch scope {
	case store.MetricScopeHost:
		if target != "" {
			return out, invalidRule("target_id", "主机级指标不接受目标（应为空）")
		}
	case store.MetricScopeInterface, store.MetricScopeFilesystem:
		if target == "" {
			return out, invalidRule("target_id", "必须指定（%s 级指标需要具体目标）", scope)
		}
	}
	if len(target) > MaxTargetIDLen {
		return out, invalidRule("target_id", "长度不能超过 %d 个字节", MaxTargetIDLen)
	}
	out.TargetID = target

	if !store.IsKnownComparison(in.Comparison) {
		return out, invalidRule("comparison", "必须是 gt/gte/lt/lte 之一")
	}
	out.Comparison = in.Comparison
	if err := checkFinite("threshold", in.Threshold); err != nil {
		return out, err
	}
	out.Threshold = in.Threshold

	if in.Secondary != nil {
		sec := Secondary{
			Metric:     strings.TrimSpace(in.Secondary.Metric),
			Comparison: strings.TrimSpace(in.Secondary.Comparison),
		}
		secScope, ok := scopeOfMetric(sec.Metric)
		if !ok {
			return out, invalidRule("secondary_metric", "%q 不是已登记的指标", sec.Metric)
		}
		if secScope != scope {
			return out, invalidRule("secondary_metric",
				"必须与主指标在同一个 scope 下（主指标是 %s，它是 %s）：两个 scope 的数据不在同一个窗口里", scope, secScope)
		}
		if sec.Metric == metric {
			return out, invalidRule("secondary_metric", "不能与主指标相同（那是同一个量，不构成双口径）")
		}
		if !store.IsKnownComparison(sec.Comparison) {
			return out, invalidRule("secondary_comparison", "必须是 gt/gte/lt/lte 之一")
		}
		if err := checkFinite("secondary_threshold", in.Secondary.Threshold); err != nil {
			return out, err
		}
		sec.Threshold = in.Secondary.Threshold
		out.Secondary = &sec
	}

	switch in.Combination {
	case "":
		out.Combination = CombinationAny
	case CombinationAny, CombinationAll:
		out.Combination = in.Combination
	default:
		return out, invalidRule("combination", "必须是 any/all 之一")
	}
	// 没有次级口径时组合方式没有意义，统一存成 any：留着一个 all 会让人以为
	// “两个条件都要满足”，而实际上只有一个条件。
	if out.Secondary == nil {
		out.Combination = CombinationAny
	}

	if in.DurationSeconds < MinDurationSeconds || in.DurationSeconds > MaxDurationSeconds {
		return out, invalidRule("duration_seconds", "必须在 %d 到 %d 秒之间", MinDurationSeconds, MaxDurationSeconds)
	}
	// 持续窗口至少要装得下两个采集周期。容量序列 30 秒才落一个点，窗口比 60 秒短
	// 就只能装 0—1 个点，"窗口内每一个样本都超限"这句话就没有内容了——而那种规则
	// 的症状是"配好了永远不响"，比一条报错难查得多。所以这里宁可拒绝它。
	if need, slow := minDurationFor(metric, out.Secondary); in.DurationSeconds < need {
		return out, invalidRule("duration_seconds",
			"不能低于 %d 秒：%s 每 %d 秒才写一个点，窗口装不下两个点就说不上“持续”",
			need, slow, int(metrics.WriteCadence(slow).Seconds()))
	}
	out.DurationSeconds = in.DurationSeconds

	if err := checkFinite("recovery_threshold", in.RecoveryThreshold); err != nil {
		return out, err
	}
	// 恢复线必须落在安全侧，而且不能等于阈值：相等等于没有死区，
	// 值在阈值上抖动就会一路刷屏，与 recovery_threshold 存在的理由相反。
	switch in.Comparison {
	case store.CompareGT, store.CompareGTE:
		if !(in.RecoveryThreshold < in.Threshold) {
			return out, invalidRule("recovery_threshold",
				"必须小于 threshold %g（告警是“高于”，恢复线要在它下面）", in.Threshold)
		}
	case store.CompareLT, store.CompareLTE:
		if !(in.RecoveryThreshold > in.Threshold) {
			return out, invalidRule("recovery_threshold",
				"必须大于 threshold %g（告警是“低于”，恢复线要在它上面）", in.Threshold)
		}
	}
	out.RecoveryThreshold = in.RecoveryThreshold

	if in.CooldownSeconds < 0 || in.CooldownSeconds > MaxCooldownSeconds {
		return out, invalidRule("cooldown_seconds", "必须在 0 到 %d 秒之间", MaxCooldownSeconds)
	}
	out.CooldownSeconds = in.CooldownSeconds
	out.Enabled = in.Enabled
	return out, nil
}

func checkFinite(field string, v float64) error {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return invalidRule(field, "必须是有限数值")
	}
	return nil
}

// minDurationFor 返回这条规则允许的最短持续时长，以及决定这个下限的是哪个指标。
//
// 下限是"两个写入节奏"：主口径与次级口径里最慢的那个说了算。没有次级口径时就是
// 主指标的节奏。raw 桶宽 5 秒的序列算出来是 10 秒，被 MinDurationSeconds（30 秒）
// 盖住；容量序列算出来是 60 秒，这才是它真正需要的地板。
func minDurationFor(primary string, secondary *Secondary) (int, string) {
	slow, slowest := primary, metrics.WriteCadence(primary)
	if secondary != nil {
		if cadence := metrics.WriteCadence(secondary.Metric); cadence > slowest {
			slow, slowest = secondary.Metric, cadence
		}
	}
	need := int(2 * slowest.Seconds())
	if need < MinDurationSeconds {
		need = MinDurationSeconds
	}
	return need, slow
}

// scopeOfMetric 由指标名反查它登记的 scope。
//
// 规则里不存 scope 输入，存的是**查出来的** scope：让调用方同时给 scope 和 metric，
// 就多了一种“两者不匹配”的非法输入要解释，而它没有正确的语义可供解释。
func scopeOfMetric(metric string) (string, bool) {
	if metric == "" {
		return "", false
	}
	for _, scope := range metrics.Scopes() {
		if metrics.IsKnownMetric(scope, metric) {
			return scope, true
		}
	}
	return "", false
}
