package notifications

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// DeliverInput 是一次投递请求。**没有明文秘密**：只有 secret_ref，真正去解它的是
// 代理（见 internal/agent/notify.go）。
type DeliverInput struct {
	// Type 是 smtp / webhook。
	Type     string
	Settings Settings
	// SecretRef 指向代理加密库里的秘密；为空表示这个通道不需要鉴权。
	SecretRef string
	Title     string
	Body      string
}

// DeliverResult 是一次投递的事实回执。
type DeliverResult struct {
	// Accepted 表示对端收下了。
	Accepted bool
	// Status 是协议回执码：SMTP 应答码（250/451/…）或 HTTP 状态码；0 表示没拿到。
	Status int
	// Detail 是对端原话（一句话），不含量密。
	Detail string
	// Retryable 表示这个失败值得再试一次。
	//
	// 它由代理判定而不是由这里猜：只有代理知道那是一次 4xx 临时拒绝、连接超时，
	// 还是一个 5xx 的永久错误。面板这边再猜一次，只会得到第二套与事实无关的规则。
	Retryable bool
}

// Sender 是投递的执行端（生产环境是代理客户端）。
//
// 面板**不自己发**：SMTP 口令与 webhook 令牌只存在代理的加密库里，而这里刻意
// 没有“取回明文”的接口（CONTRACTS §7：禁止通用 GET 明文接口）。所以发送也只能
// 由代理做——面板只知道“发到哪、说什么”，不知道“用什么凭据”。
type Sender interface {
	Deliver(ctx context.Context, in DeliverInput) (DeliverResult, error)
}

// DispatchReport 是一轮投递调度的结果。
type DispatchReport struct {
	// Due 是这一轮到期的投递数。
	Due int
	// Sent 是送达数，Retried 是排了下次重试的数量，Failed 是放弃数。
	Sent    int
	Retried int
	Failed  int
}

// Run 是投递调度循环。ctx 结束时返回。
func (s *Service) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = dispatchInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	// 启动先跑一轮：面板重启之后不该让上一轮剩下的 pending 等到下一个节拍。
	if _, err := s.DispatchOnce(ctx); err != nil && ctx.Err() == nil {
		s.logError(err)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := s.DispatchOnce(ctx); err != nil && ctx.Err() == nil {
				s.logError(err)
			}
		}
	}
}

// DispatchOnce 跑一轮投递调度。它不开定时器，由 Run 或测试调用。
func (s *Service) DispatchOnce(ctx context.Context) (DispatchReport, error) {
	var report DispatchReport
	now := s.nowUTC()
	due, err := s.dueDeliveries(ctx, now, dispatchBatch)
	if err != nil {
		return report, err
	}
	report.Due = len(due)
	if len(due) == 0 {
		return report, nil
	}
	sender := s.currentSender()
	if sender == nil {
		// 没有发送端（演示模式）：投递原地不动，**不伪装成失败**。
		// “没连代理”与“发不出去”是两件事，混在一起会让人去查 SMTP 配置。
		s.logError(ErrSenderUnavailable)
		return report, nil
	}
	for _, d := range due {
		ch, err := s.GetChannel(ctx, d.ChannelID)
		if err != nil {
			if !errors.Is(err, ErrNotFound) {
				// 读取故障不等于通道被删除。保留 pending，下一轮重读；
				// 否则一次数据库抖动就会把尚可投递的通知永久判死。
				return report, err
			}
			if markErr := s.abandonDelivery(ctx, d.ID, "通道已被删除，未发送"); markErr != nil {
				return report, markErr
			}
			report.Failed++
			continue
		}
		if !ch.Enabled {
			if markErr := s.abandonDelivery(ctx, d.ID, "通道已停用，未发送"); markErr != nil {
				return report, markErr
			}
			report.Failed++
			continue
		}
		// 抢占这一次尝试：只有把 attempts 从读到的值 +1 成功的那一次才发，
		// 免得两个进程（或一次重启前后的两份调度）把同一行发两遍。
		res, err := s.exec(ctx, `
			UPDATE notification_deliveries SET attempts = attempts + 1
			 WHERE id = ? AND state = 'pending' AND attempts = ?`, d.ID, d.Attempts)
		if err != nil {
			return report, fmt.Errorf("notifications: 抢占投递失败: %w", err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return report, fmt.Errorf("notifications: 抢占投递失败: %w", err)
		}
		if affected == 0 {
			continue
		}
		updated, err := s.attemptDelivery(ctx, sender, ch, d.ID, ch.Type, ch.Settings, ch.SecretRef,
			d.Title, d.Body, now, d.Attempts+1, d.MaxAttempts)
		if err != nil {
			return report, err
		}
		switch updated.State {
		case DeliverySent:
			report.Sent++
		case DeliveryPending:
			report.Retried++
		default:
			report.Failed++
		}
	}
	return report, nil
}

// dueDeliveries 取到期的待发投递。
func (s *Service) dueDeliveries(ctx context.Context, now time.Time, limit int) ([]Delivery, error) {
	rows, err := s.query(ctx, `
		SELECT id, COALESCE(channel_id, 0), channel_name, channel_type, target,
		       COALESCE(event_id, 0), event_rule, kind, title, body, state, attempts,
		       max_attempts, next_attempt_at, last_error, last_status, sent_at, created_at
		  FROM notification_deliveries
		 WHERE state = 'pending' AND next_attempt_at <= ?
		 ORDER BY next_attempt_at, id
		 LIMIT ?`, now.Unix(), limit)
	if err != nil {
		return nil, fmt.Errorf("notifications: 读取待发投递失败: %w", err)
	}
	defer rows.Close()
	out := make([]Delivery, 0, limit)
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, fmt.Errorf("notifications: 读取待发投递失败: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("notifications: 读取待发投递失败: %w", err)
	}
	return out, nil
}

// abandonDelivery 把一条投递判死并写清原因。
//
// 它**不计入 attempts**：一次都没发出去的东西不该看起来像试过一次。
func (s *Service) abandonDelivery(ctx context.Context, id int64, reason string) error {
	if _, err := s.exec(ctx, `
		UPDATE notification_deliveries
		   SET state = 'failed', last_error = ?, next_attempt_at = 0
		 WHERE id = ? AND state = 'pending'`, truncate(reason, MaxErrorLen), id); err != nil {
		return fmt.Errorf("notifications: 放弃投递失败: %w", err)
	}
	return nil
}

// logError 把失败记进日志，同一条一分钟内只记一次。
//
// 重复抑制在这里不是洁癖：一轮调度 5 秒一次，一台连不上 SMTP 的机器一分钟能刷
// 十二条一模一样的日志，把真正有用的那些挤走。
func (s *Service) logError(err error) {
	if s.logger == nil || err == nil {
		return
	}
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
	if errors.Is(err, ErrSenderUnavailable) {
		s.logger.Warn("通知没有发送端，投递停在 pending", "reason", err, "hint", "代理未连接（演示模式）")
		return
	}
	s.logger.Warn("通知投递失败", "error", err)
}
