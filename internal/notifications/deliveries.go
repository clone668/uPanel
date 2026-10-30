package notifications

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"upanel/internal/alerts"
)

// Delivery 是一次投递（发出去的东西 + 发成没发成）。
type Delivery struct {
	ID int64
	// ChannelID 为 0 表示通道后来被删掉了（外键 SET NULL），快照列仍然可读。
	ChannelID   int64
	ChannelName string
	ChannelType string
	Target      string
	// EventID 为 0 表示这是一次测试发送，或事件行已经被删掉了。
	EventID   int64
	EventRule string
	Kind      string
	Title     string
	Body      string
	State     string
	Attempts  int
	// MaxAttempts 是这一行的重试上限（入队时从通道上抄下来的快照）。
	MaxAttempts int
	// NextAttemptAt 为零值表示立刻可发。
	NextAttemptAt time.Time
	LastError     string
	LastStatus    int
	SentAt        time.Time
	CreatedAt     time.Time
}

// DeliveryQuery 是投递列表的查询条件。
type DeliveryQuery struct {
	// State 为空表示不过滤；否则是 DeliveryPending / DeliverySent / DeliveryFailed。
	State string
	// Kind 为空表示不过滤；否则是 KindFired / KindResolved / KindTest。
	Kind string
	// Limit 是返回条数上限（默认 50，最大 200）。
	Limit int
}

// ListDeliveries 返回投递列表，最新在前。
func (s *Service) ListDeliveries(ctx context.Context, q DeliveryQuery) ([]Delivery, error) {
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
		switch q.State {
		case DeliveryPending, DeliverySent, DeliveryFailed:
			where += " AND state = ?"
			args = append(args, q.State)
		default:
			return nil, invalid("state", "必须是 %s/%s/%s 之一", DeliveryPending, DeliverySent, DeliveryFailed)
		}
	}
	if q.Kind != "" {
		switch q.Kind {
		case KindFired, KindResolved, KindTest:
			where += " AND kind = ?"
			args = append(args, q.Kind)
		default:
			return nil, invalid("kind", "必须是 %s/%s/%s 之一", KindFired, KindResolved, KindTest)
		}
	}
	args = append(args, limit)
	rows, err := s.query(ctx, `
		SELECT id, COALESCE(channel_id, 0), channel_name, channel_type, target,
		       COALESCE(event_id, 0), event_rule, kind, title, body, state, attempts,
		       max_attempts, next_attempt_at, last_error, last_status, sent_at, created_at
		  FROM notification_deliveries
		 WHERE 1 = 1`+where+`
		 ORDER BY created_at DESC, id DESC
		 LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("notifications: 读取投递列表失败: %w", err)
	}
	defer rows.Close()
	out := make([]Delivery, 0, 16)
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, fmt.Errorf("notifications: 读取投递列表失败: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("notifications: 读取投递列表失败: %w", err)
	}
	return out, nil
}

func scanDelivery(sc scanner) (Delivery, error) {
	var (
		d                    Delivery
		nextAttempt          int64
		sentAt               sql.NullString
		createdAt            string
		attempts, maxAttempt int64
		lastStatus           int64
	)
	if err := sc.Scan(&d.ID, &d.ChannelID, &d.ChannelName, &d.ChannelType, &d.Target,
		&d.EventID, &d.EventRule, &d.Kind, &d.Title, &d.Body, &d.State, &attempts,
		&maxAttempt, &nextAttempt, &d.LastError, &lastStatus, &sentAt, &createdAt); err != nil {
		return Delivery{}, err
	}
	d.Attempts = int(attempts)
	d.MaxAttempts = int(maxAttempt)
	d.LastStatus = int(lastStatus)
	if nextAttempt > 0 {
		d.NextAttemptAt = time.Unix(nextAttempt, 0).UTC()
	}
	var err error
	if sentAt.Valid {
		if d.SentAt, err = parseTime(sentAt.String); err != nil {
			return Delivery{}, err
		}
	}
	if d.CreatedAt, err = parseTime(createdAt); err != nil {
		return Delivery{}, err
	}
	return d, nil
}

// −−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−
// 入队：告警事件 → 每个启用通道一条投递
// −−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−

// AlertFired 是告警服务在“新开一个事件”那一刻的回调。
//
// 它在评估事务**内部**被调用（见 alerts.applyOutcome）：事件行与投递行要么一起进去，
// 要么一起没有。分开写的话，中间崩一次就会留下“事件在库里、谁也不知道它发生过”。
func (s *Service) AlertFired(ctx context.Context, ev alerts.Event) error {
	return s.enqueue(ctx, ev, KindFired)
}

// AlertResolved 是告警服务在“收口一个事件”那一刻的回调。
func (s *Service) AlertResolved(ctx context.Context, ev alerts.Event) error {
	return s.enqueue(ctx, ev, KindResolved)
}

// enqueue 给每个启用通道写一条投递。
//
// **维护静默的事件一条都不发。** 这是 P13.2 那句“静默只压通知”的落点：事件照记
// （alerts 那边的事），但这里一条投递也不入队。不这么做的话，silenced 就只是一个
// 没人看的字段——静默期间照发，静默之后又说“这条事件是静默的”，两句话都在骗人。
//
// 恢复也一并静默：一条你从未被告知开始的告警，不该用一条“它结束了”来收尾。
func (s *Service) enqueue(ctx context.Context, ev alerts.Event, kind string) error {
	if ev.Silenced {
		return nil
	}
	channels, err := s.enabledChannels(ctx)
	if err != nil {
		return err
	}
	if len(channels) == 0 {
		return nil
	}
	title, body := renderMessage(ev, kind)
	now := s.nowUTC()
	nowText := formatTime(now)
	for _, ch := range channels {
		state := DeliveryPending
		var sentAt any
		if ch.Type == TypeInApp {
			// 面板内通知出现即送达：它不需要发到任何地方，读投递账就是读收件箱。
			state = DeliverySent
			sentAt = nowText
		}
		maxAttempts := ch.MaxAttempts
		if ch.Type == TypeInApp {
			maxAttempts = 1
		}
		// INSERT OR IGNORE 靠 idx_notification_deliveries_once 兜底：同一事件在同一通道上
		// 每种类型只一条。重试是改这一行，不是再插一行。
		if _, err := s.exec(ctx, `
			INSERT OR IGNORE INTO notification_deliveries
			    (channel_id, channel_name, channel_type, target, event_id, event_rule, kind,
			     title, body, state, attempts, max_attempts, next_attempt_at, last_error,
			     last_status, sent_at, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, 0, '', 0, ?, ?)`,
			ch.ID, ch.Name, ch.Type, truncate(ch.Target(), 1024), ev.ID, truncate(ev.RuleName, 120),
			kind, title, body, state, maxAttempts, sentAt, nowText); err != nil {
			return fmt.Errorf("notifications: 写入投递失败: %w", err)
		}
	}
	return nil
}

// SendTest 对一条通道做一次**显式**测试发送（POST /notification-channels/:id/test）。
//
// 它与正常投递的区别有三处，每一处都是刻意的：
//   - 不重试。测试发送要的是“此刻能不能发出去”这个事实，不是“最终能不能成功”；
//     带重试的测试会让人盯着页面等三分钟才看到“还是不行”。
//   - 走不进 outbox 的调度，立即尝试一次，结果原样回给调用方（含协议回执码）。
//   - 仍然记一行投递账（kind=test）：谁在什么时候按了测试键、结果如何，是运维最常
//     回头查的一件事。它不参与事件的去重（event_id 为 NULL）。
func (s *Service) SendTest(ctx context.Context, channelID int64) (Delivery, error) {
	ch, err := s.GetChannel(ctx, channelID)
	if err != nil {
		return Delivery{}, err
	}
	if ch.Type == TypeInApp {
		return Delivery{}, ErrNotSendable
	}
	sender := s.currentSender()
	if sender == nil {
		return Delivery{}, ErrSenderUnavailable
	}
	now := s.nowUTC()
	title, body := renderTestMessage(ch)
	maxAttempts := 1
	res, err := s.exec(ctx, `
		INSERT INTO notification_deliveries
		    (channel_id, channel_name, channel_type, target, event_id, event_rule, kind,
		     title, body, state, attempts, max_attempts, next_attempt_at, last_error,
		     last_status, sent_at, created_at)
		VALUES (?, ?, ?, ?, NULL, '', ?, ?, ?, ?, 0, ?, 0, '', 0, NULL, ?)`,
		ch.ID, ch.Name, ch.Type, truncate(ch.Target(), 1024), KindTest, title, body,
		DeliveryPending, maxAttempts, formatTime(now))
	if err != nil {
		return Delivery{}, fmt.Errorf("notifications: 写入测试投递失败: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Delivery{}, fmt.Errorf("notifications: 读取测试投递 id 失败: %w", err)
	}
	d, err := s.attemptDelivery(ctx, sender, ch, id, ch.Type, ch.Settings, ch.SecretRef,
		title, body, now, 1, maxAttempts)
	if err != nil {
		return Delivery{}, err
	}
	return d, nil
}

// attemptDelivery 发一次并把结果写回那一行。返回写回之后的投递。
//
// maxAttempts 决定“失败之后是 pending 还是 failed”；测试发送传 1，也就是一次就定生死。
func (s *Service) attemptDelivery(ctx context.Context, sender Sender, ch Channel, id int64,
	channelType string, settings Settings, secretRef, title, body string,
	now time.Time, attempt, maxAttempts int) (Delivery, error) {

	result, sendErr := sender.Deliver(ctx, DeliverInput{
		Type:      channelType,
		Settings:  settings,
		SecretRef: secretRef,
		Title:     title,
		Body:      body,
	})
	// 对端一旦已经给出结果，就不能随调用方断开而丢掉本地回执。
	// 否则 accepted 的通知仍停在 pending，下一轮会把同一消息再发一次。
	writeCtx := context.WithoutCancel(ctx)
	if sendErr != nil {
		result = DeliverResult{
			Accepted:  false,
			Detail:    sendErr.Error(),
			Retryable: true,
		}
	}
	if result.Accepted {
		if _, err := s.exec(writeCtx, `
			UPDATE notification_deliveries
			   SET state = 'sent', attempts = ?, sent_at = ?, last_status = ?, last_error = '',
			       next_attempt_at = 0
			 WHERE id = ?`,
			attempt, formatTime(now), result.Status, id); err != nil {
			return Delivery{}, fmt.Errorf("notifications: 写回投递结果失败: %w", err)
		}
		return s.getDelivery(writeCtx, id)
	}

	detail := truncate(strings.TrimSpace(result.Detail), MaxErrorLen)
	retry := result.Retryable && attempt < maxAttempts
	state := DeliveryFailed
	var next int64
	if retry {
		state = DeliveryPending
		next = now.Add(retryDelay(attempt)).Unix()
	}
	if _, err := s.exec(writeCtx, `
		UPDATE notification_deliveries
		   SET state = ?, attempts = ?, last_status = ?, last_error = ?, next_attempt_at = ?
		 WHERE id = ?`,
		state, attempt, result.Status, detail, next, id); err != nil {
		return Delivery{}, fmt.Errorf("notifications: 写回投递结果失败: %w", err)
	}
	return s.getDelivery(writeCtx, id)
}

// retryDelay 返回第 attempt 次失败之后要等多久。
//
// 指数退避 30s / 60s / 120s …，上限 10 分钟。上限不是怕算得太大，而是怕一次
// “重试上限 10”的通道把最后一次尝试排到几小时之后——那时人早就以为它在正常工作了。
func retryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := retryBaseDelay
	for i := 1; i < attempt; i++ {
		delay *= 2
		if delay >= maxRetryDelay {
			return maxRetryDelay
		}
	}
	return delay
}

func (s *Service) getDelivery(ctx context.Context, id int64) (Delivery, error) {
	row := s.queryRow(ctx, `
		SELECT id, COALESCE(channel_id, 0), channel_name, channel_type, target,
		       COALESCE(event_id, 0), event_rule, kind, title, body, state, attempts,
		       max_attempts, next_attempt_at, last_error, last_status, sent_at, created_at
		  FROM notification_deliveries WHERE id = ?`, id)
	d, err := scanDelivery(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Delivery{}, ErrNotFound
	}
	if err != nil {
		return Delivery{}, fmt.Errorf("notifications: 读取投递失败: %w", err)
	}
	return d, nil
}

// −−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−
// 消息渲染
// −−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−

// comparisonSymbol 把比较符翻成符号。
func comparisonSymbol(op string) string {
	switch op {
	case "gt":
		return ">"
	case "gte":
		return "≥"
	case "lt":
		return "<"
	case "lte":
		return "≤"
	default:
		return op
	}
}

// formatMetricValue 给指标值配上单位。
//
// 单位由指标名决定（percent 加 %，bytes 加 B），不额外存一列——两处写同一件事
// 迟早会不一致（与 migrations/0012 的 threshold 注释同一条理由）。
func formatMetricValue(metric string, v *float64) string {
	if v == nil {
		return "（无）"
	}
	num := strconv.FormatFloat(*v, 'f', -1, 64)
	switch {
	case strings.HasSuffix(metric, "_percent"):
		return num + "%"
	case strings.HasSuffix(metric, "_bytes"):
		return num + " B"
	case strings.HasSuffix(metric, "_bytes_per_sec"):
		return num + " B/s"
	default:
		return num
	}
}

// renderMessage 把一条事件渲染成标题与正文。
//
// 正文里只有事实：规则、目标、条件、时间、值。没有“请立即处理”这类话——通知的
// 读者是这个面板的管理员，他需要的是判据，不是催促。
func renderMessage(ev alerts.Event, kind string) (string, string) {
	verb := "告警"
	if kind == KindResolved {
		verb = "恢复"
	}
	title := cleanHeaderValue(fmt.Sprintf("[%s] %s：%s %s %s",
		verb, ev.RuleName, ev.Metric, comparisonSymbol(ev.Comparison),
		formatMetricValue(ev.Metric, floatPtr(ev.Threshold))))
	title = truncate(title, MaxTitleLen)

	var b strings.Builder
	fmt.Fprintf(&b, "规则：%s\n", ev.RuleName)
	target := ev.Scope
	if ev.TargetID != "" {
		target = ev.Scope + " / " + ev.TargetID
	}
	fmt.Fprintf(&b, "目标：%s\n", target)
	fmt.Fprintf(&b, "条件：%s %s %s\n", ev.Metric, comparisonSymbol(ev.Comparison),
		formatMetricValue(ev.Metric, floatPtr(ev.Threshold)))
	if kind == KindResolved {
		fmt.Fprintf(&b, "开始时间：%s\n", formatTime(ev.StartedAt))
		fmt.Fprintf(&b, "恢复时间：%s\n", formatTime(ev.ResolvedAt))
		fmt.Fprintf(&b, "持续：%d 秒\n", ev.DurationSeconds)
		fmt.Fprintf(&b, "期间峰值：%s\n", formatMetricValue(ev.Metric, ev.PeakValue))
	} else {
		fmt.Fprintf(&b, "开始时间：%s\n", formatTime(ev.StartedAt))
		fmt.Fprintf(&b, "触发时取值：%s\n", formatMetricValue(ev.Metric, ev.PeakValue))
	}
	fmt.Fprintf(&b, "事件编号：%d\n", ev.ID)
	return title, truncate(b.String(), MaxBodyLen)
}

func renderTestMessage(ch Channel) (string, string) {
	title := cleanHeaderValue(fmt.Sprintf("[测试] %s", ch.Name))
	body := fmt.Sprintf("这是一条来自 uPanel 的测试通知。\n通道：%s（%s）\n目的地：%s\n"+
		"收到这条消息说明通道参数可用；它不代表任何主机发生了异常。",
		ch.Name, ch.Type, ch.Target())
	return truncate(title, MaxTitleLen), truncate(body, MaxBodyLen)
}

func floatPtr(v float64) *float64 { return &v }
