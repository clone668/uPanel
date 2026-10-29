package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"upanel/internal/alerts"
	"upanel/internal/audit"
	"upanel/internal/auth"
	"upanel/internal/httpx"
)

// 告警接口（P13.2）。这一层只做三件事：解请求、把领域错误翻成契约错误码、拼审计。
// 判定逻辑全在 internal/alerts，接口层不做任何"这个阈值合不合理"的判断——
// 那种判断一旦有两份，就有两个答案。

// defaultAlertCooldown 是新建规则时冷却期的默认值（秒）。
//
// 建规则的人多半想不到要设它，而"恢复之后马上又超限"是最常见的刷屏来源。
// 想要没有冷却就显式给 0：PATCH 与 POST 的全指针请求体分得出这两个意思。
const defaultAlertCooldown = 300

// alertRuleDTO 是一条规则的对外形状（CONTRACTS.md §13.2）。
//
// 状态机字段一起返回：界面上"这条规则现在是不是在报"和"它为什么一直不报"
// （last_reason）是同一屏要看的东西。
type alertRuleDTO struct {
	ID                 int64    `json:"id"`
	Name               string   `json:"name"`
	Scope              string   `json:"scope"`
	TargetID           string   `json:"target_id"`
	Metric             string   `json:"metric"`
	Comparison         string   `json:"comparison"`
	Threshold          float64  `json:"threshold"`
	SecondaryMetric    *string  `json:"secondary_metric"`
	SecondaryCompare   *string  `json:"secondary_comparison"`
	SecondaryThreshold *float64 `json:"secondary_threshold"`
	Combination        string   `json:"combination"`
	DurationSeconds    int      `json:"duration_seconds"`
	RecoveryThreshold  float64  `json:"recovery_threshold"`
	CooldownSeconds    int      `json:"cooldown_seconds"`
	Enabled            bool     `json:"enabled"`
	Version            int64    `json:"version"`

	State         string   `json:"state"`
	StateSince    string   `json:"state_since"`
	RecoverSince  string   `json:"recover_since"`
	CooldownUntil string   `json:"cooldown_until"`
	LastEvalAt    string   `json:"last_eval_at"`
	LastValue     *float64 `json:"last_value"`
	LastReason    string   `json:"last_reason"`

	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// alertEventDTO 是一条告警事件。
//
// 规则快照字段（rule_name / metric / comparison / threshold）来自事件行自己，
// 不是 join 回规则表取的：规则改过以后，这条事件仍然是"当时那条规则"触发的。
type alertEventDTO struct {
	ID              int64    `json:"id"`
	RuleID          int64    `json:"rule_id"`
	RuleName        string   `json:"rule_name"`
	Scope           string   `json:"scope"`
	TargetID        string   `json:"target_id"`
	Metric          string   `json:"metric"`
	Comparison      string   `json:"comparison"`
	Threshold       float64  `json:"threshold"`
	State           string   `json:"state"`
	StartedAt       string   `json:"started_at"`
	ResolvedAt      *string  `json:"resolved_at"`
	DurationSeconds int      `json:"duration_seconds"`
	PeakValue       *float64 `json:"peak_value"`
	Silenced        bool     `json:"silenced"`
	AckedAt         *string  `json:"acked_at"`
	AckedBy         string   `json:"acked_by"`
}

// alertSilenceDTO 是维护静默窗口。没有静默时起止字段为 null，active 为 false。
type alertSilenceDTO struct {
	Active    bool    `json:"active"`
	StartsAt  *string `json:"starts_at"`
	EndsAt    *string `json:"ends_at"`
	Reason    string  `json:"reason"`
	UpdatedAt string  `json:"updated_at"`
	UpdatedBy string  `json:"updated_by"`
}

// alertRuleRequest 是创建与修改共用的请求体。
//
// 全部用指针，是因为 PATCH 要能区分"没提这个字段"和"把它设成零值"：
// enabled=false、cooldown_seconds=0、threshold=0 都是合法取值，用非指针结构体
// 接，就永远分不清"关掉它"和"这次没提它"。
type alertRuleRequest struct {
	Name               *string  `json:"name"`
	TargetID           *string  `json:"target_id"`
	Metric             *string  `json:"metric"`
	Comparison         *string  `json:"comparison"`
	Threshold          *float64 `json:"threshold"`
	SecondaryMetric    *string  `json:"secondary_metric"`
	SecondaryCompare   *string  `json:"secondary_comparison"`
	SecondaryThreshold *float64 `json:"secondary_threshold"`
	Combination        *string  `json:"combination"`
	DurationSeconds    *int     `json:"duration_seconds"`
	RecoveryThreshold  *float64 `json:"recovery_threshold"`
	CooldownSeconds    *int     `json:"cooldown_seconds"`
	Enabled            *bool    `json:"enabled"`
	Version            *int64   `json:"version"`
}

// requireAlerts 取出告警服务。未装配时回 503 并说明原因，而不是回一份空规则列表——
// 空列表会被读成"这台机器上没有配任何告警"。
func (s *Server) requireAlerts(c *gin.Context) (*alerts.Service, bool) {
	if s.opts.Alerts == nil {
		writeError(c, http.StatusServiceUnavailable, httpx.CodeNotImplemented,
			"这个进程没有装配告警功能（未连接面板数据库）")
		return nil, false
	}
	return s.opts.Alerts, true
}

// handleListAlertRules 处理 GET /api/v1/alerts/rules（viewer 可读）。
func (s *Server) handleListAlertRules(c *gin.Context) {
	svc, ok := s.requireAlerts(c)
	if !ok {
		return
	}
	rules, err := svc.List(c.Request.Context())
	if err != nil {
		s.internalError(c, "读取告警规则失败", err)
		return
	}
	out := make([]alertRuleDTO, 0, len(rules))
	for _, r := range rules {
		out = append(out, ruleDTOOf(r))
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, out)
}

// handleCreateAlertRule 处理 POST /api/v1/alerts/rules（admin + CSRF）。
func (s *Server) handleCreateAlertRule(c *gin.Context) {
	sess := sessionFrom(c)
	const action = audit.ActionAlertRuleCreate

	var req alertRuleRequest
	if !decodeJSONBody(c, &req) {
		s.recordDenied(c, &sess, action, "alert_rule", "validation", httpx.CodeValidationError)
		return
	}
	// 新建的默认值只在这里给：冷却默认 300 秒，因为"恢复之后马上又超限"是最常见的
	// 刷屏来源，而第一次建规则的人多半想不到要设它。显式给 0 才是"不要冷却"。
	input, err := ruleInputFrom(alerts.RuleInput{Enabled: true, CooldownSeconds: defaultAlertCooldown}, req, true)
	if err != nil {
		s.recordDenied(c, &sess, action, "alert_rule", "validation", httpx.CodeValidationError)
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError, err.Error())
		return
	}

	var created alerts.Rule
	err = s.opts.Alerts.InTransaction(c.Request.Context(), func(ctx context.Context) error {
		rule, err := s.opts.Alerts.Create(ctx, input)
		if err != nil {
			return &errDenied{err}
		}
		if err := s.recordAuditInTx(ctx, c, sess, action, alertRuleTarget(rule.ID), ruleAuditDetails(rule)); err != nil {
			return err
		}
		created = rule
		return nil
	})
	if err != nil {
		s.failAlertAction(c, &sess, action, "alert_rule", err)
		return
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusCreated, ruleDTOOf(created))
}

// handleUpdateAlertRule 处理 PATCH /api/v1/alerts/rules/:id（admin + CSRF）。
func (s *Server) handleUpdateAlertRule(c *gin.Context) {
	sess := sessionFrom(c)
	const action = audit.ActionAlertRuleUpdate

	id, ok := parseAlertID(c)
	if !ok {
		return
	}
	var req alertRuleRequest
	if !decodeJSONBody(c, &req) {
		s.recordDenied(c, &sess, action, alertRuleTargetRaw(c.Param("id")), "validation", httpx.CodeValidationError)
		return
	}
	if req.Version == nil || *req.Version <= 0 {
		s.recordDenied(c, &sess, action, alertRuleTarget(id), "validation", httpx.CodeValidationError)
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError,
			"version 必须给出：它是防止两次改规则互相覆盖的依据")
		return
	}

	var updated alerts.Rule
	var before alerts.Rule
	err := s.opts.Alerts.InTransaction(c.Request.Context(), func(ctx context.Context) error {
		current, err := s.opts.Alerts.Get(ctx, id)
		if err != nil {
			return &errDenied{err}
		}
		before = current
		input, err := ruleInputFrom(ruleInputOf(current), req, false)
		if err != nil {
			return &errDenied{err}
		}
		rule, err := s.opts.Alerts.Update(ctx, id, *req.Version, input)
		if err != nil {
			return &errDenied{err}
		}
		details := ruleAuditDetails(rule)
		// 改动前后的关键项一起记：审计要能回答"把线从多少挪到了多少"，
		// 只记改后值等于把一半事实丢了。
		details["before"] = map[string]any{
			"name":               before.Name,
			"threshold":          before.Threshold,
			"recovery_threshold": before.RecoveryThreshold,
			"duration_seconds":   before.DurationSeconds,
			"cooldown_seconds":   before.CooldownSeconds,
			"enabled":            before.Enabled,
			"version":            before.Version,
		}
		if err := s.recordAuditInTx(ctx, c, sess, action, alertRuleTarget(id), details); err != nil {
			return err
		}
		updated = rule
		return nil
	})
	if err != nil {
		s.failAlertAction(c, &sess, action, alertRuleTarget(id), err)
		return
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, ruleDTOOf(updated))
}

// handleDeleteAlertRule 处理 DELETE /api/v1/alerts/rules/:id（admin + CSRF）。
//
// version 走查询参数（可选）：给出就必须匹配，不给出就按当前版本删。
// 事件行不会被删掉——它们的外键是 SET NULL，历史留下来。
func (s *Server) handleDeleteAlertRule(c *gin.Context) {
	sess := sessionFrom(c)
	const action = audit.ActionAlertRuleDelete

	id, ok := parseAlertID(c)
	if !ok {
		return
	}
	version := int64(0)
	if raw := c.Query("version"); raw != "" {
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || v <= 0 {
			s.recordDenied(c, &sess, action, alertRuleTarget(id), "validation", httpx.CodeValidationError)
			writeError(c, http.StatusBadRequest, httpx.CodeValidationError, "version 必须是正整数")
			return
		}
		version = v
	}

	var deleted alerts.Rule
	err := s.opts.Alerts.InTransaction(c.Request.Context(), func(ctx context.Context) error {
		rule, err := s.opts.Alerts.Get(ctx, id)
		if err != nil {
			return &errDenied{err}
		}
		deleted = rule
		if err := s.opts.Alerts.Delete(ctx, id, version); err != nil {
			return &errDenied{err}
		}
		return s.recordAuditInTx(ctx, c, sess, action, alertRuleTarget(id), ruleAuditDetails(rule))
	})
	if err != nil {
		s.failAlertAction(c, &sess, action, alertRuleTarget(id), err)
		return
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, map[string]any{"id": deleted.ID})
}

// handleListAlertEvents 处理 GET /api/v1/alerts/events（viewer 可读）。
func (s *Server) handleListAlertEvents(c *gin.Context) {
	svc, ok := s.requireAlerts(c)
	if !ok {
		return
	}
	q := alerts.EventsQuery{State: c.Query("state")}
	if raw := c.Query("rule_id"); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			writeError(c, http.StatusBadRequest, httpx.CodeValidationError, "rule_id 必须是正整数")
			return
		}
		q.RuleID = id
	}
	if raw := c.Query("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			writeError(c, http.StatusBadRequest, httpx.CodeValidationError, "limit 必须是正整数")
			return
		}
		q.Limit = n
	}
	events, err := svc.ListEvents(c.Request.Context(), q)
	if err != nil {
		if alerts.IsInvalidRule(err) {
			writeError(c, http.StatusBadRequest, httpx.CodeValidationError, err.Error())
			return
		}
		s.internalError(c, "读取告警事件失败", err)
		return
	}
	out := make([]alertEventDTO, 0, len(events))
	for _, e := range events {
		out = append(out, eventDTOOf(e))
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, out)
}

// handleAckAlertEvent 处理 POST /api/v1/alerts/events/:id/ack（admin + CSRF）。
//
// 确认是**人**的动作，不是机器结论：它只写 acked_at / acked_by 两个字段，
// 不动 state、不动 resolved_at、不动规则。所以它对同一条事件天然幂等——
// 点两次、重试一次，库里"谁先看见的"都是同一个人、同一个时刻，
// 而不是把名字换成最后点击的那个。
//
// 为什么是 admin：确认的意思是"这件事有人管了"，它和改阈值、设静默同属
// "谁对这台机器负责"的一部分；而事件列表本身是 V（viewer 也能看），
// 看得到与认领是两件事。
func (s *Server) handleAckAlertEvent(c *gin.Context) {
	sess := sessionFrom(c)
	const action = audit.ActionAlertEventAck

	id, ok := parseEventID(c)
	if !ok {
		s.recordDenied(c, &sess, action, alertEventTargetRaw(c.Param("id")), "validation",
			httpx.CodeValidationError)
		return
	}

	var acked alerts.Event
	err := s.opts.Alerts.InTransaction(c.Request.Context(), func(ctx context.Context) error {
		ev, err := s.opts.Alerts.AckEvent(ctx, id, sess.User.Username)
		if err != nil {
			return &errDenied{err}
		}
		details := map[string]any{
			"rule_id":  ev.RuleID,
			"state":    ev.State,
			"acked_at": formatAlertTime(ev.AckedAt),
			"acked_by": ev.AckedBy,
		}
		if err := s.recordAuditInTx(ctx, c, sess, action, alertEventTarget(id), details); err != nil {
			return err
		}
		acked = ev
		return nil
	})
	if err != nil {
		// mapAlertError 那句 "告警规则不存在" 是给规则用的；事件不存在要按事件说。
		if errors.Is(err, alerts.ErrNotFound) {
			s.recordDenied(c, &sess, action, alertEventTarget(id), "business", httpx.CodeNotFound)
			writeError(c, http.StatusNotFound, httpx.CodeNotFound, "告警事件不存在")
			return
		}
		s.failAlertAction(c, &sess, action, alertEventTarget(id), err)
		return
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, eventDTOOf(acked))
}

// handleGetAlertSilence 处理 GET /api/v1/alerts/silence（viewer 可读）。
func (s *Server) handleGetAlertSilence(c *gin.Context) {
	svc, ok := s.requireAlerts(c)
	if !ok {
		return
	}
	silence, err := svc.GetSilence(c.Request.Context())
	if err != nil {
		s.internalError(c, "读取维护静默失败", err)
		return
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, silenceDTOOf(silence, svc.Now()))
}

// alertSilenceRequest 是 PUT /api/v1/alerts/silence 的请求体。
//
// 起止都省略表示**清除**静默。这是刻意的：清除是一次显式动作，不是"把窗口设成
// 过去的时间"那种能被时钟漂移意外造成的状态。
type alertSilenceRequest struct {
	StartsAt *string `json:"starts_at"`
	EndsAt   *string `json:"ends_at"`
	Reason   *string `json:"reason"`
}

// handlePutAlertSilence 处理 PUT /api/v1/alerts/silence（admin + CSRF）。
func (s *Server) handlePutAlertSilence(c *gin.Context) {
	sess := sessionFrom(c)
	const action = audit.ActionAlertSilenceUpdate

	var req alertSilenceRequest
	if !decodeJSONBody(c, &req) {
		s.recordDenied(c, &sess, action, "alert_silence", "validation", httpx.CodeValidationError)
		return
	}
	in := alerts.Silence{}
	if req.Reason != nil {
		in.Reason = *req.Reason
	}
	var err error
	if req.StartsAt != nil {
		if in.StartsAt, err = parseAlertTime(*req.StartsAt, "starts_at"); err != nil {
			writeError(c, http.StatusBadRequest, httpx.CodeValidationError, err.Error())
			return
		}
	}
	if req.EndsAt != nil {
		if in.EndsAt, err = parseAlertTime(*req.EndsAt, "ends_at"); err != nil {
			writeError(c, http.StatusBadRequest, httpx.CodeValidationError, err.Error())
			return
		}
	}

	var out alerts.Silence
	err = s.opts.Alerts.InTransaction(c.Request.Context(), func(ctx context.Context) error {
		silence, err := s.opts.Alerts.SetSilence(ctx, in, sess.User.Username)
		if err != nil {
			return &errDenied{err}
		}
		details := map[string]any{"reason": silence.Reason, "active": silence.ActiveAt(s.opts.Alerts.Now())}
		if !silence.StartsAt.IsZero() {
			details["starts_at"] = silence.StartsAt.Format(time.RFC3339)
			details["ends_at"] = silence.EndsAt.Format(time.RFC3339)
		}
		if err := s.recordAuditInTx(ctx, c, sess, action, "alert_silence", details); err != nil {
			return err
		}
		out = silence
		return nil
	})
	if err != nil {
		s.failAlertAction(c, &sess, action, "alert_silence", err)
		return
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, silenceDTOOf(out, s.opts.Alerts.Now()))
}

// −−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−
// 转换与错误映射
// −−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−

// ruleInputFrom 把请求体叠到一份基础输入上。
//
// create 为 true 时要求必填项齐全（新建没有"原来是什么"可以叠）；
// 为 false 时是 PATCH 语义：没提的字段沿用 base。
func ruleInputFrom(base alerts.RuleInput, req alertRuleRequest, create bool) (alerts.RuleInput, error) {
	in := base
	if req.Name != nil {
		in.Name = *req.Name
	}
	if req.TargetID != nil {
		in.TargetID = *req.TargetID
	}
	if req.Metric != nil {
		in.Metric = *req.Metric
	}
	if req.Comparison != nil {
		in.Comparison = *req.Comparison
	}
	if req.Threshold != nil {
		in.Threshold = *req.Threshold
	}
	if req.Combination != nil {
		in.Combination = *req.Combination
	}
	if req.DurationSeconds != nil {
		in.DurationSeconds = *req.DurationSeconds
	}
	if req.RecoveryThreshold != nil {
		in.RecoveryThreshold = *req.RecoveryThreshold
	}
	if req.CooldownSeconds != nil {
		in.CooldownSeconds = *req.CooldownSeconds
	}
	if req.Enabled != nil {
		in.Enabled = *req.Enabled
	}

	// 次级口径：三种情况要分清楚——不动它、整体设置、整体清除。
	switch {
	case req.SecondaryMetric == nil && req.SecondaryCompare == nil && req.SecondaryThreshold == nil:
		// 不动。
	case req.SecondaryMetric != nil && *req.SecondaryMetric == "":
		// 显式清除。另外两项必须一起省略：半套清除是想干什么说不清。
		if req.SecondaryCompare != nil || req.SecondaryThreshold != nil {
			return in, errors.New("清除次级口径时不要同时给出 secondary_comparison / secondary_threshold")
		}
		in.Secondary = nil
	default:
		if req.SecondaryMetric == nil || req.SecondaryCompare == nil || req.SecondaryThreshold == nil {
			return in, errors.New("次级口径的三个字段必须一起给出：secondary_metric / secondary_comparison / secondary_threshold")
		}
		in.Secondary = &alerts.Secondary{
			Metric:     *req.SecondaryMetric,
			Comparison: *req.SecondaryCompare,
			Threshold:  *req.SecondaryThreshold,
		}
	}

	if create {
		// 新建时必须显式给出这几个：零值不能当成"就这样"。
		// 一个 threshold=0 的 CPU 规则会在第一轮就永远 firing，那不该是默认值。
		switch {
		case req.Metric == nil:
			return in, errors.New("metric 必须给出")
		case req.Comparison == nil:
			return in, errors.New("comparison 必须给出")
		case req.Threshold == nil:
			return in, errors.New("threshold 必须给出")
		case req.DurationSeconds == nil:
			return in, errors.New("duration_seconds 必须给出")
		case req.RecoveryThreshold == nil:
			return in, errors.New("recovery_threshold 必须给出")
		}
	}
	return in, nil
}

// ruleInputOf 把一条已落库的规则还原成输入，作为 PATCH 的叠加基础。
func ruleInputOf(r alerts.Rule) alerts.RuleInput {
	in := alerts.RuleInput{
		Name:              r.Name,
		TargetID:          r.TargetID,
		Metric:            r.Metric,
		Comparison:        r.Comparison,
		Threshold:         r.Threshold,
		Combination:       r.Combination,
		DurationSeconds:   r.DurationSeconds,
		RecoveryThreshold: r.RecoveryThreshold,
		CooldownSeconds:   r.CooldownSeconds,
		Enabled:           r.Enabled,
	}
	if r.Secondary != nil {
		sec := *r.Secondary
		in.Secondary = &sec
	}
	return in
}

func ruleDTOOf(r alerts.Rule) alertRuleDTO {
	dto := alertRuleDTO{
		ID:                r.ID,
		Name:              r.Name,
		Scope:             r.Scope,
		TargetID:          r.TargetID,
		Metric:            r.Metric,
		Comparison:        r.Comparison,
		Threshold:         r.Threshold,
		Combination:       r.Combination,
		DurationSeconds:   r.DurationSeconds,
		RecoveryThreshold: r.RecoveryThreshold,
		CooldownSeconds:   r.CooldownSeconds,
		Enabled:           r.Enabled,
		Version:           r.Version,
		State:             r.State,
		StateSince:        formatAlertTime(r.StateSince),
		RecoverSince:      formatAlertTime(r.RecoverSince),
		CooldownUntil:     formatAlertTime(r.CooldownUntil),
		LastEvalAt:        formatAlertTime(r.LastEvalAt),
		LastValue:         r.LastValue,
		LastReason:        r.LastReason,
		CreatedAt:         formatAlertTime(r.CreatedAt),
		UpdatedAt:         formatAlertTime(r.UpdatedAt),
	}
	if r.Secondary != nil {
		metric := r.Secondary.Metric
		comparison := r.Secondary.Comparison
		threshold := r.Secondary.Threshold
		dto.SecondaryMetric = &metric
		dto.SecondaryCompare = &comparison
		dto.SecondaryThreshold = &threshold
	}
	return dto
}

func eventDTOOf(e alerts.Event) alertEventDTO {
	return alertEventDTO{
		ID:              e.ID,
		RuleID:          e.RuleID,
		RuleName:        e.RuleName,
		Scope:           e.Scope,
		TargetID:        e.TargetID,
		Metric:          e.Metric,
		Comparison:      e.Comparison,
		Threshold:       e.Threshold,
		State:           e.State,
		StartedAt:       formatAlertTime(e.StartedAt),
		ResolvedAt:      formatAlertTimeOrNil(e.ResolvedAt),
		DurationSeconds: e.DurationSeconds,
		PeakValue:       e.PeakValue,
		Silenced:        e.Silenced,
		AckedAt:         formatAlertTimeOrNil(e.AckedAt),
		AckedBy:         e.AckedBy,
	}
}

func silenceDTOOf(s alerts.Silence, now time.Time) alertSilenceDTO {
	return alertSilenceDTO{
		Active:    s.ActiveAt(now),
		StartsAt:  formatAlertTimeOrNil(s.StartsAt),
		EndsAt:    formatAlertTimeOrNil(s.EndsAt),
		Reason:    s.Reason,
		UpdatedAt: formatAlertTime(s.UpdatedAt),
		UpdatedBy: s.UpdatedBy,
	}
}

// ruleAuditDetails 是规则进审计时记的字段。
//
// 阈值、恢复线、持续、冷却、启停、双口径全都在：这些是"告警会不会响"的全部输入。
// 少记一个，事后就无法解释"为什么那天没报"。
func ruleAuditDetails(r alerts.Rule) map[string]any {
	details := map[string]any{
		"rule_id":            r.ID,
		"name":               r.Name,
		"scope":              r.Scope,
		"target_id":          r.TargetID,
		"metric":             r.Metric,
		"comparison":         r.Comparison,
		"threshold":          r.Threshold,
		"recovery_threshold": r.RecoveryThreshold,
		"duration_seconds":   r.DurationSeconds,
		"cooldown_seconds":   r.CooldownSeconds,
		"combination":        r.Combination,
		"enabled":            r.Enabled,
		"version":            r.Version,
	}
	if r.Secondary != nil {
		details["secondary_metric"] = r.Secondary.Metric
		details["secondary_comparison"] = r.Secondary.Comparison
		details["secondary_threshold"] = r.Secondary.Threshold
	}
	return details
}

func alertRuleTarget(id int64) string { return "alert_rule:" + strconv.FormatInt(id, 10) }

func alertRuleTargetRaw(raw string) string { return "alert_rule:" + raw }

// parseEventID 解析事件 id。错误措辞与规则分开：把 "规则 id 必须是正整数"
// 回给一个确认事件的调用方，只会让人去查规则列表。
func parseEventID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError, "事件 id 必须是正整数")
		return 0, false
	}
	return id, true
}

func alertEventTarget(id int64) string { return "alert_event:" + strconv.FormatInt(id, 10) }

func alertEventTargetRaw(raw string) string { return "alert_event:" + raw }

func parseAlertID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError, "规则 id 必须是正整数")
		return 0, false
	}
	return id, true
}

func parseAlertTime(value, field string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, errors.New(field + " 必须是 RFC3339 时间")
	}
	return t.UTC(), nil
}

// formatAlertTime 把零值时间格式化成空串。
//
// 只给"永远是时间"的字段用（started_at、updated_at 这类）。可空时间不走这里——
// 空串与 null 在 JSON 里是两件事，契约说过"可空"就必须是 null，见 formatAlertTimeOrNil。
func formatAlertTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// formatAlertTimeOrNil 给契约里标了"可空"的时间字段用：没有值就是 **null**，不是空串。
//
// 这一处曾经走的是 formatAlertTime，于是"没有维护窗口"的响应里 starts_at 是 ""，
// 而 CONTRACTS §13.2 写的是 null。二者在客户端是两件事：`""` 会被 `new Date(..)` 解成
// 零值、被 `x == null` 判成"有值"，于是"到底有没有设过窗口"要靠猜。openapi-lint 把
// 这类漂移挡在了宿主检查里（`nullable` 在 OpenAPI 3.1 里不存在，写它本身就是一处漂移）。
func formatAlertTimeOrNil(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	v := t.UTC().Format(time.RFC3339)
	return &v
}

// failAlertAction 把领域错误映射成契约错误码并记一条被拒绝的审计。
func (s *Server) failAlertAction(c *gin.Context, sess *auth.Session, action, target string, err error) {
	if errors.Is(err, errAuditWrite) {
		s.opts.Logger.Error("审计写入失败，业务变更已回滚", "error", err, "action", action,
			"request_id", c.GetString(ctxKeyRequestID))
		writeError(c, http.StatusInternalServerError, httpx.CodeInternalError, "服务器内部错误")
		return
	}
	businessErr := err
	var denied *errDenied
	if errors.As(err, &denied) {
		businessErr = denied.err
	}
	status, code, msg, internal := mapAlertError(businessErr)
	if internal {
		s.recordDenied(c, sess, action, target, "internal", code)
		s.internalError(c, "告警操作失败", businessErr)
		return
	}
	s.recordDenied(c, sess, action, target, "business", code)
	writeError(c, status, code, msg)
}

// mapAlertError 把告警领域的错误翻成状态码与契约错误码。
func mapAlertError(err error) (status int, code httpx.Code, message string, internal bool) {
	var invalid *alerts.InvalidRuleError
	switch {
	case errors.As(err, &invalid):
		return http.StatusBadRequest, httpx.CodeValidationError, invalid.Error(), false
	case errors.Is(err, alerts.ErrNotFound):
		return http.StatusNotFound, httpx.CodeNotFound, "告警规则不存在", false
	case errors.Is(err, alerts.ErrVersionConflict):
		return http.StatusConflict, httpx.CodeVersionConflict, "规则已被其他人修改，请刷新后重试", false
	case errors.Is(err, alerts.ErrLimitReached):
		return http.StatusConflict, httpx.CodeDependencyBlocked,
			"告警规则数量已达上限（" + strconv.Itoa(alerts.MaxRules) + "）；先删掉不再需要的规则", false
	default:
		return http.StatusInternalServerError, httpx.CodeInternalError, "服务器内部错误", true
	}
}
