package notifications

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"upanel/internal/agentclient"
	"upanel/internal/alerts"
	"upanel/internal/store"
	"upanel/migrations"
)

// 测试用真实 SQLite，不用内存假实现：迁移里的 CHECK、in_app 唯一索引、
// “同一事件同一通道每种类型只一条”这个部分唯一索引，以及外键 SET NULL
// 都只有真库验得出来——而这几条正是这套设计最容易被写错的地方。

type clock struct{ t time.Time }

func (c *clock) Now() time.Time          { return c.t }
func (c *clock) Advance(d time.Duration) { c.t = c.t.Add(d) }

func baseTime() time.Time { return time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC) }

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func newTestService(t *testing.T) (*Service, *store.DB, *clock) {
	t.Helper()
	db, err := store.Open(context.Background(), store.Options{
		Path:   filepath.Join(t.TempDir(), "upanel.db"),
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
		t.Fatalf("构造通知服务失败: %v", err)
	}
	return svc, db, clk
}

// fakeSender 是可控的投递端，记录每次投递的入参。
type fakeSender struct {
	calls []DeliverInput
	// result 与 err 是每次投递的返回；两者都用时 err 优先。
	result DeliverResult
	err    error
}

func (f *fakeSender) Deliver(_ context.Context, in DeliverInput) (DeliverResult, error) {
	f.calls = append(f.calls, in)
	if f.err != nil {
		return DeliverResult{}, f.err
	}
	return f.result, nil
}

// testEvent 造一条“已触发”的事件。
func testEvent(silenced bool) alerts.Event {
	peak := 95.5
	return alerts.Event{
		ID:         7,
		RuleID:     3,
		RuleName:   "磁盘将满",
		Scope:      "filesystem",
		TargetID:   "/dev/sda1",
		Metric:     "filesystem.used_percent",
		Comparison: "gt",
		Threshold:  90,
		State:      alerts.EventFiring,
		StartedAt:  baseTime(),
		PeakValue:  &peak,
		Silenced:   silenced,
	}
}

// insertEventRow 造一条真实的事件行：投递表的 event_id 有外键，光有 Event 结构体
// 满足不了约束。
func insertEventRow(t *testing.T, db *store.DB, silenced bool) alerts.Event {
	t.Helper()
	ctx := context.Background()
	ts := formatTime(baseTime())
	res, err := db.SQL().ExecContext(ctx, `
		INSERT INTO alert_events
		    (rule_id, rule_name, scope, target_id, metric, comparison, threshold, state,
		     started_at, resolved_at, duration_seconds, peak_value, silenced,
		     acked_at, acked_by, created_at)
		VALUES (NULL, '磁盘将满', 'filesystem', '/dev/sda1', 'filesystem.used_percent', 'gt',
		        90, 'firing', ?, NULL, 0, 95.5, ?, NULL, '', ?)`,
		ts, boolInt(silenced), ts)
	if err != nil {
		t.Fatalf("写入测试事件失败: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("读取事件 id 失败: %v", err)
	}
	ev := testEvent(silenced)
	ev.ID = id
	return ev
}

func countRows(t *testing.T, db *store.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.SQL().QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("计数失败: %v", err)
	}
	return n
}

// −−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−
// 通道：校验边界
// −−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−

func TestChannelValidation(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()

	smtpBase := ChannelInput{
		Name: "运维邮件", Type: TypeSMTP, Enabled: true, MaxAttempts: DefaultMaxAttempts,
		Settings: Settings{Host: "smtp.example.com", Port: 587, TLS: TLSStartTLS,
			From: "panel@example.com", To: []string{"ops@example.com"}},
	}

	cases := []struct {
		name    string
		mutate  func(*ChannelInput)
		wantSub string
	}{
		{"没有 host", func(in *ChannelInput) { in.Settings.Host = "" }, "host"},
		{"端口越界", func(in *ChannelInput) { in.Settings.Port = 0 }, "port"},
		{"明文 smtp", func(in *ChannelInput) { in.Settings.TLS = "none" }, "tls"},
		{"收件人为空", func(in *ChannelInput) { in.Settings.To = nil }, "收件人"},
		{"地址不合法", func(in *ChannelInput) { in.Settings.To = []string{"不是邮箱"} }, "不是合法的邮件地址"},
		{"有用户名无秘密", func(in *ChannelInput) { in.Settings.Username = "u" }, "secret_ref"},
		{"有秘密无用户名", func(in *ChannelInput) {
			in.SecretRef = "sec_0123456789abcdef0123456789abcdef"
		}, "username"},
		{"重试上限越界", func(in *ChannelInput) { in.MaxAttempts = MaxAttempts + 1 }, "max_attempts"},
		{"名字为空", func(in *ChannelInput) { in.Name = "" }, "name"},
		{"类型不认识", func(in *ChannelInput) { in.Type = "telegram" }, "type"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := smtpBase
			// Settings 与 To 是切片/结构体，逐个人拷一份，免得一个子测试改到下一个。
			to := append([]string(nil), smtpBase.Settings.To...)
			in.Settings.To = to
			tc.mutate(&in)
			_, err := svc.CreateChannel(ctx, in)
			if err == nil {
				t.Fatalf("期望被拒绝，实际创建成功")
			}
			if !IsInvalidChannel(err) {
				t.Fatalf("期望 InvalidChannelError，实际 %T: %v", err, err)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("错误里应提到 %q，实际: %v", tc.wantSub, err)
			}
		})
	}
}

func TestWebhookRejectsNonHTTPS(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	for _, u := range []string{
		"http://hooks.example.com/x",
		"https://user:pass@hooks.example.com/x",
		"https://hooks.example.com/x#frag",
		"ftp://hooks.example.com/x",
	} {
		_, err := svc.CreateChannel(ctx, ChannelInput{
			Name: "回调", Type: TypeWebhook, Enabled: true, MaxAttempts: DefaultMaxAttempts,
			Settings: Settings{URL: u},
		})
		if err == nil {
			t.Fatalf("%s 应该被拒绝", u)
		}
		if !IsInvalidChannel(err) {
			t.Fatalf("%s 期望 InvalidChannelError，实际 %T", u, err)
		}
	}
	if _, err := svc.CreateChannel(ctx, ChannelInput{
		Name: "回调", Type: TypeWebhook, Enabled: true, MaxAttempts: DefaultMaxAttempts,
		Settings: Settings{URL: "https://hooks.example.com/x"},
	}); err != nil {
		t.Fatalf("合法 https 被拒绝: %v", err)
	}
}

// TestInAppChannelIsSingleton：迁移里种了唯一一条 in_app，再建一条要被唯一索引挡住。
func TestInAppChannelIsSingleton(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()

	channels, err := svc.ListChannels(ctx)
	if err != nil {
		t.Fatalf("列通道失败: %v", err)
	}
	if len(channels) != 1 || channels[0].Type != TypeInApp {
		t.Fatalf("迁移应种下恰好一条 in_app 通道，实际 %d 条", len(channels))
	}
	if !channels[0].Enabled {
		t.Fatal("in_app 通道迁移后应默认启用")
	}

	_, err = svc.CreateChannel(ctx, ChannelInput{
		Name: "第二个收件箱", Type: TypeInApp, Enabled: true, MaxAttempts: DefaultMaxAttempts,
	})
	if !IsInvalidChannel(err) {
		t.Fatalf("第二条 in_app 应被拒绝，实际 %v", err)
	}
}

// TestUpdateAndDelete：版本号冲突、in_app 不可删、删除后投递账保留。
func TestUpdateAndDelete(t *testing.T) {
	svc, db, _ := newTestService(t)
	ctx := context.Background()

	created, err := svc.CreateChannel(ctx, ChannelInput{
		Name: "运维邮件", Type: TypeSMTP, Enabled: true, MaxAttempts: DefaultMaxAttempts,
		Settings: Settings{Host: "smtp.example.com", Port: 587, TLS: TLSStartTLS,
			From: "panel@example.com", To: []string{"ops@example.com"}},
	})
	if err != nil {
		t.Fatalf("建通道失败: %v", err)
	}
	if created.Version != 1 {
		t.Fatalf("新建通道 version 应为 1，实际 %d", created.Version)
	}

	if _, err := svc.UpdateChannel(ctx, created.ID, 99, ChannelInput{
		Name: "x", Type: TypeSMTP, Enabled: false, MaxAttempts: DefaultMaxAttempts,
		Settings: created.Settings,
	}); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("旧版本号应冲突，实际 %v", err)
	}

	updated, err := svc.UpdateChannel(ctx, created.ID, 1, ChannelInput{
		Name: "运维邮件（停用）", Type: TypeSMTP, Enabled: false, MaxAttempts: DefaultMaxAttempts,
		Settings: created.Settings,
	})
	if err != nil {
		t.Fatalf("改通道失败: %v", err)
	}
	if updated.Enabled || updated.Version != 2 {
		t.Fatalf("改后应为停用且 version=2，实际 enabled=%v version=%d", updated.Enabled, updated.Version)
	}

	// 事件 + 投递行，然后删通道：投递账要留着，channel_id 变 NULL。
	ev := insertEventRow(t, db, false)
	if _, err := svc.CreateChannel(ctx, ChannelInput{
		Name: "临时", Type: TypeWebhook, Enabled: true, MaxAttempts: DefaultMaxAttempts,
		Settings: Settings{URL: "https://hooks.example.com/x"},
	}); err != nil {
		t.Fatalf("建 webhook 通道失败: %v", err)
	}
	if err := svc.AlertFired(ctx, ev); err != nil {
		t.Fatalf("入队失败: %v", err)
	}
	before := countRows(t, db, `SELECT COUNT(*) FROM notification_deliveries WHERE event_id = ?`, ev.ID)

	var hookID int64
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT id FROM notification_channels WHERE type = 'webhook'`).Scan(&hookID); err != nil {
		t.Fatalf("读 webhook id 失败: %v", err)
	}
	if err := svc.DeleteChannel(ctx, hookID, 0); err != nil {
		t.Fatalf("删通道失败: %v", err)
	}
	after := countRows(t, db, `SELECT COUNT(*) FROM notification_deliveries WHERE event_id = ?`, ev.ID)
	if before != after {
		t.Fatalf("删通道不该删投递账：删前 %d 条，删后 %d 条", before, after)
	}
	orphan := countRows(t, db, `SELECT COUNT(*) FROM notification_deliveries
		WHERE event_id = ? AND channel_id IS NULL`, ev.ID)
	if orphan != 1 {
		t.Fatalf("被删通道的投递应留下 channel_id=NULL 的一行，实际 %d 行", orphan)
	}

	// in_app 删不掉。
	var inAppID int64
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT id FROM notification_channels WHERE type = 'in_app'`).Scan(&inAppID); err != nil {
		t.Fatalf("读 in_app id 失败: %v", err)
	}
	if err := svc.DeleteChannel(ctx, inAppID, 0); !IsInvalidChannel(err) {
		t.Fatalf("in_app 通道应拒删，实际 %v", err)
	}
}

// −−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−
// 入队：静默不发、in_app 即送达、同事件同通道去重
// −−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−

func TestSilencedEventEnqueuesNothing(t *testing.T) {
	svc, db, _ := newTestService(t)
	ctx := context.Background()
	if _, err := svc.CreateChannel(ctx, ChannelInput{
		Name: "运维邮件", Type: TypeSMTP, Enabled: true, MaxAttempts: DefaultMaxAttempts,
		Settings: Settings{Host: "smtp.example.com", Port: 587, TLS: TLSStartTLS,
			From: "panel@example.com", To: []string{"ops@example.com"}},
	}); err != nil {
		t.Fatalf("建通道失败: %v", err)
	}

	ev := insertEventRow(t, db, true)
	if err := svc.AlertFired(ctx, ev); err != nil {
		t.Fatalf("入队失败: %v", err)
	}
	if err := svc.AlertResolved(ctx, ev); err != nil {
		t.Fatalf("入队失败: %v", err)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM notification_deliveries`); n != 0 {
		t.Fatalf("静默事件不该产生投递，实际 %d 条", n)
	}
}

func TestInAppDeliveryIsSentImmediately(t *testing.T) {
	svc, db, _ := newTestService(t)
	ctx := context.Background()

	ev := insertEventRow(t, db, false)
	if err := svc.AlertFired(ctx, ev); err != nil {
		t.Fatalf("入队失败: %v", err)
	}

	rows, err := svc.ListDeliveries(ctx, DeliveryQuery{})
	if err != nil {
		t.Fatalf("列投递失败: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("应只有 in_app 一条投递，实际 %d 条", len(rows))
	}
	d := rows[0]
	if d.ChannelType != TypeInApp || d.State != DeliverySent || d.SentAt.IsZero() {
		t.Fatalf("in_app 投递应即时 sent 且有 sent_at，实际 state=%s sent_at=%v", d.State, d.SentAt)
	}
	if d.Kind != KindFired {
		t.Fatalf("kind 应为 fired，实际 %s", d.Kind)
	}
	if !strings.Contains(d.Title, "磁盘将满") || !strings.Contains(d.Title, "告警") {
		t.Fatalf("标题应含规则名与“告警”，实际 %q", d.Title)
	}
}

func TestEnqueueDeduplicatesPerEventChannelKind(t *testing.T) {
	svc, db, _ := newTestService(t)
	ctx := context.Background()

	ev := insertEventRow(t, db, false)
	for i := 0; i < 3; i++ {
		if err := svc.AlertFired(ctx, ev); err != nil {
			t.Fatalf("第 %d 次入队失败: %v", i+1, err)
		}
	}
	if n := countRows(t, db,
		`SELECT COUNT(*) FROM notification_deliveries WHERE event_id = ? AND kind = 'fired'`,
		ev.ID); n != 1 {
		t.Fatalf("同一事件同一通道同一类型只应一条，实际 %d 条", n)
	}
}

// −−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−
// 投递：退避、放弃、测试发送
// −−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−

func TestRetryBackoffAndGiveUp(t *testing.T) {
	svc, db, clk := newTestService(t)
	ctx := context.Background()

	ch, err := svc.CreateChannel(ctx, ChannelInput{
		Name: "运维邮件", Type: TypeSMTP, Enabled: true, MaxAttempts: 2,
		Settings: Settings{Host: "smtp.example.com", Port: 587, TLS: TLSStartTLS,
			From: "panel@example.com", To: []string{"ops@example.com"}},
	})
	if err != nil {
		t.Fatalf("建通道失败: %v", err)
	}
	ev := insertEventRow(t, db, false)
	if err := svc.AlertFired(ctx, ev); err != nil {
		t.Fatalf("入队失败: %v", err)
	}
	var deliveryID int64
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT id FROM notification_deliveries WHERE channel_id = ?`, ch.ID).Scan(&deliveryID); err != nil {
		t.Fatalf("读投递 id 失败: %v", err)
	}

	// 第一次：可重试的失败（SMTP 451）→ pending，下次排在 30 秒之后。
	svc.SetSender(&fakeSender{result: DeliverResult{Status: 451, Detail: "4.7.1 稍后再试", Retryable: true}})
	report, err := svc.DispatchOnce(ctx)
	if err != nil {
		t.Fatalf("调度失败: %v", err)
	}
	if report.Retried != 1 {
		t.Fatalf("第一次失败应排重试，报告: %+v", report)
	}
	d, err := svc.getDelivery(ctx, deliveryID)
	if err != nil {
		t.Fatalf("读投递失败: %v", err)
	}
	if d.State != DeliveryPending || d.Attempts != 1 {
		t.Fatalf("第一次失败后应为 pending/attempts=1，实际 %s/%d", d.State, d.Attempts)
	}
	if wantMin := clk.Now().Add(retryBaseDelay); d.NextAttemptAt.Before(wantMin.Add(-time.Second)) {
		t.Fatalf("下次尝试应不早于 %v，实际 %v", wantMin, d.NextAttemptAt)
	}
	// 还没到点，第二轮不该碰它。
	if report, err = svc.DispatchOnce(ctx); err != nil || report.Due != 0 {
		t.Fatalf("未到点的投递不该被取出，报告: %+v err=%v", report, err)
	}

	// 到点后第二次：仍然失败，而 max_attempts=2 已经用完 → failed。
	// 时钟直接跳到第一次算出来的 next_attempt_at 之后——退避是产品行为，
	// 在测试里等 30 秒没有意义。
	clk.t = d.NextAttemptAt.Add(time.Second)
	report, err = svc.DispatchOnce(ctx)
	if err != nil {
		t.Fatalf("调度失败: %v", err)
	}
	if report.Failed != 1 {
		t.Fatalf("第二次失败应判死，报告: %+v", report)
	}
	d, err = svc.getDelivery(ctx, deliveryID)
	if err != nil {
		t.Fatalf("读投递失败: %v", err)
	}
	if d.State != DeliveryFailed || d.Attempts != 2 {
		t.Fatalf("用尽重试后应为 failed/attempts=2，实际 %s/%d", d.State, d.Attempts)
	}
	if d.LastStatus != 451 || d.LastError == "" {
		t.Fatalf("失败行应带对端回执码与原话，实际 status=%d err=%q", d.LastStatus, d.LastError)
	}
}

// TestNonRetryableFailureGivesUpImmediately：5xx 是永久拒绝，一次就判死。
func TestNonRetryableFailureGivesUpImmediately(t *testing.T) {
	svc, db, _ := newTestService(t)
	ctx := context.Background()

	ch, err := svc.CreateChannel(ctx, ChannelInput{
		Name: "运维邮件", Type: TypeSMTP, Enabled: true, MaxAttempts: 5,
		Settings: Settings{Host: "smtp.example.com", Port: 587, TLS: TLSStartTLS,
			From: "panel@example.com", To: []string{"ops@example.com"}},
	})
	if err != nil {
		t.Fatalf("建通道失败: %v", err)
	}
	ev := insertEventRow(t, db, false)
	if err := svc.AlertFired(ctx, ev); err != nil {
		t.Fatalf("入队失败: %v", err)
	}

	svc.SetSender(&fakeSender{result: DeliverResult{Status: 550, Detail: "5.1.1 用户不存在"}})
	report, err := svc.DispatchOnce(ctx)
	if err != nil {
		t.Fatalf("调度失败: %v", err)
	}
	if report.Failed != 1 || report.Retried != 0 {
		t.Fatalf("永久拒绝应直接放弃，报告: %+v", report)
	}
	var state string
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT state FROM notification_deliveries WHERE channel_id = ?`, ch.ID).Scan(&state); err != nil {
		t.Fatalf("读状态失败: %v", err)
	}
	if state != DeliveryFailed {
		t.Fatalf("状态应为 failed，实际 %s", state)
	}
}

// TestDispatchWithoutSenderLeavesPending：没有发送端时投递原地不动，不伪装成失败。
func TestDispatchWithoutSenderLeavesPending(t *testing.T) {
	svc, db, _ := newTestService(t)
	ctx := context.Background()

	ch, err := svc.CreateChannel(ctx, ChannelInput{
		Name: "运维邮件", Type: TypeSMTP, Enabled: true, MaxAttempts: DefaultMaxAttempts,
		Settings: Settings{Host: "smtp.example.com", Port: 587, TLS: TLSStartTLS,
			From: "panel@example.com", To: []string{"ops@example.com"}},
	})
	if err != nil {
		t.Fatalf("建通道失败: %v", err)
	}
	ev := insertEventRow(t, db, false)
	if err := svc.AlertFired(ctx, ev); err != nil {
		t.Fatalf("入队失败: %v", err)
	}
	report, err := svc.DispatchOnce(ctx)
	if err != nil {
		t.Fatalf("没有发送端时调度不该报错: %v", err)
	}
	if report.Sent != 0 || report.Failed != 0 || report.Retried != 0 {
		t.Fatalf("没有发送端时不该改变任何投递，报告: %+v", report)
	}
	var state string
	var attempts int
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT state, attempts FROM notification_deliveries WHERE channel_id = ?`,
		ch.ID).Scan(&state, &attempts); err != nil {
		t.Fatalf("读投递失败: %v", err)
	}
	if state != DeliveryPending || attempts != 0 {
		t.Fatalf("应停在 pending 且没有消耗尝试次数，实际 %s/attempts=%d", state, attempts)
	}
}

func TestAttemptDeliveryPersistsAcceptedResultAfterCallerCancellation(t *testing.T) {
	svc, db, _ := newTestService(t)
	ctx := context.Background()
	ch, err := svc.CreateChannel(ctx, ChannelInput{
		Name: "运维 webhook", Type: TypeWebhook, Enabled: true,
		Settings: Settings{URL: "https://hooks.example.com/alert"},
	})
	if err != nil {
		t.Fatalf("建通道失败: %v", err)
	}
	ev := insertEventRow(t, db, false)
	if err := svc.AlertFired(ctx, ev); err != nil {
		t.Fatalf("入队失败: %v", err)
	}
	var id int64
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT id FROM notification_deliveries WHERE channel_id = ?`, ch.ID).Scan(&id); err != nil {
		t.Fatalf("读投递 id 失败: %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	d, err := svc.attemptDelivery(canceled, &fakeSender{result: DeliverResult{Accepted: true, Status: 204}},
		ch, id, ch.Type, ch.Settings, ch.SecretRef, "标题", "正文", svc.nowUTC(), 1, 3)
	if err != nil {
		t.Fatalf("对端已接受后，调用方取消不应丢掉本地回执: %v", err)
	}
	if d.State != DeliverySent || d.Attempts != 1 {
		t.Fatalf("应持久化 sent/attempts=1，实际 %s/%d", d.State, d.Attempts)
	}
}

func TestDispatchChannelReadFailureLeavesDeliveryPending(t *testing.T) {
	svc, db, _ := newTestService(t)
	ctx := context.Background()
	ch, err := svc.CreateChannel(ctx, ChannelInput{
		Name: "运维 webhook", Type: TypeWebhook, Enabled: true,
		Settings: Settings{URL: "https://hooks.example.com/alert"},
	})
	if err != nil {
		t.Fatalf("建通道失败: %v", err)
	}
	ev := insertEventRow(t, db, false)
	if err := svc.AlertFired(ctx, ev); err != nil {
		t.Fatalf("入队失败: %v", err)
	}
	if _, err := db.SQL().ExecContext(ctx,
		`UPDATE notification_channels SET settings = '{"unknown":true}' WHERE id = ?`, ch.ID); err != nil {
		t.Fatalf("制造通道读取故障失败: %v", err)
	}
	svc.SetSender(&fakeSender{result: DeliverResult{Accepted: true, Status: 204}})
	if _, err := svc.DispatchOnce(ctx); err == nil {
		t.Fatal("通道读取故障应返回错误，不能把投递当成通道已删除")
	}
	var state string
	var attempts int
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT state, attempts FROM notification_deliveries WHERE channel_id = ?`, ch.ID).
		Scan(&state, &attempts); err != nil {
		t.Fatalf("读投递失败: %v", err)
	}
	if state != DeliveryPending || attempts != 0 {
		t.Fatalf("读取故障后应保留 pending/attempts=0，实际 %s/%d", state, attempts)
	}
}

// TestDisabledChannelIsAbandoned：通道停用之后到点的投递判死，并说清原因。
func TestDisabledChannelIsAbandoned(t *testing.T) {
	svc, db, _ := newTestService(t)
	ctx := context.Background()

	ch, err := svc.CreateChannel(ctx, ChannelInput{
		Name: "运维邮件", Type: TypeSMTP, Enabled: true, MaxAttempts: DefaultMaxAttempts,
		Settings: Settings{Host: "smtp.example.com", Port: 587, TLS: TLSStartTLS,
			From: "panel@example.com", To: []string{"ops@example.com"}},
	})
	if err != nil {
		t.Fatalf("建通道失败: %v", err)
	}
	ev := insertEventRow(t, db, false)
	if err := svc.AlertFired(ctx, ev); err != nil {
		t.Fatalf("入队失败: %v", err)
	}
	if _, err := svc.UpdateChannel(ctx, ch.ID, ch.Version, ChannelInput{
		Name: ch.Name, Type: ch.Type, Enabled: false, MaxAttempts: ch.MaxAttempts,
		Settings: ch.Settings,
	}); err != nil {
		t.Fatalf("停用通道失败: %v", err)
	}

	sender := &fakeSender{result: DeliverResult{Accepted: true, Status: 250}}
	svc.SetSender(sender)
	report, err := svc.DispatchOnce(ctx)
	if err != nil {
		t.Fatalf("调度失败: %v", err)
	}
	if report.Failed != 1 || len(sender.calls) != 0 {
		t.Fatalf("停用通道的投递应被判死且不发，报告: %+v 调用 %d 次", report, len(sender.calls))
	}
	var lastError string
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT last_error FROM notification_deliveries WHERE channel_id = ?`, ch.ID).Scan(&lastError); err != nil {
		t.Fatalf("读投递失败: %v", err)
	}
	if !strings.Contains(lastError, "停用") {
		t.Fatalf("原因里应说明通道已停用，实际 %q", lastError)
	}
}

func TestSendTest(t *testing.T) {
	svc, db, _ := newTestService(t)
	ctx := context.Background()

	// in_app 不能测试发送：它的“投递”就是收件箱里的一行，没有外部动作可做。
	var inAppID int64
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT id FROM notification_channels WHERE type = 'in_app'`).Scan(&inAppID); err != nil {
		t.Fatalf("读 in_app id 失败: %v", err)
	}
	if _, err := svc.SendTest(ctx, inAppID); !errors.Is(err, ErrNotSendable) {
		t.Fatalf("in_app 测试发送应回 ErrNotSendable，实际 %v", err)
	}

	ch, err := svc.CreateChannel(ctx, ChannelInput{
		Name: "回调", Type: TypeWebhook, Enabled: true, MaxAttempts: DefaultMaxAttempts,
		Settings: Settings{URL: "https://hooks.example.com/x"},
	})
	if err != nil {
		t.Fatalf("建通道失败: %v", err)
	}
	// 没有发送端（演示模式）：明确说“代理不可用”，而不是记一次投递失败。
	if _, err := svc.SendTest(ctx, ch.ID); !errors.Is(err, ErrSenderUnavailable) {
		t.Fatalf("没有发送端时应回 ErrSenderUnavailable，实际 %v", err)
	}
	if n := countRows(t, db,
		`SELECT COUNT(*) FROM notification_deliveries WHERE channel_id = ?`, ch.ID); n != 0 {
		t.Fatalf("没有发送端时不该留下投递行，实际 %d 条", n)
	}

	// 有发送端、但对端拒绝：仍然记一行 kind=test，且 attempts=1 就定生死。
	sender := &fakeSender{result: DeliverResult{Status: 404, Detail: "not found"}}
	svc.SetSender(sender)
	d, err := svc.SendTest(ctx, ch.ID)
	if err != nil {
		t.Fatalf("测试发送失败: %v", err)
	}
	if d.Kind != KindTest || d.State != DeliveryFailed || d.Attempts != 1 {
		t.Fatalf("测试发送应记 kind=test/failed/attempts=1，实际 %s/%s/%d", d.Kind, d.State, d.Attempts)
	}
	if len(sender.calls) != 1 {
		t.Fatalf("测试发送应只尝试一次，实际 %d 次", len(sender.calls))
	}
	if sender.calls[0].SecretRef != "" || sender.calls[0].Settings.URL != "https://hooks.example.com/x" {
		t.Fatalf("投递入参不对: %+v", sender.calls[0])
	}
	// 测试投递不参与事件去重（event_id 为 NULL），所以它能有多行。
	if n := countRows(t, db,
		`SELECT COUNT(*) FROM notification_deliveries WHERE channel_id = ? AND event_id IS NULL`,
		ch.ID); n != 1 {
		t.Fatalf("应有一行 event_id 为 NULL 的测试投递，实际 %d 条", n)
	}

	// 成功一次：state=sent 且有 sent_at。
	sender.result = DeliverResult{Accepted: true, Status: 200, Detail: "ok"}
	d, err = svc.SendTest(ctx, ch.ID)
	if err != nil {
		t.Fatalf("测试发送失败: %v", err)
	}
	if d.State != DeliverySent || d.SentAt.IsZero() {
		t.Fatalf("成功时应为 sent 且有 sent_at，实际 %s/%v", d.State, d.SentAt)
	}
}

// −−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−
// 设置解码
// −−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−

// TestDecodeSettingsRejectsUnknownKeys：库里存着的 settings 若带上了不认识的键，
// 宁可读失败也不要静默忽略——静默忽略意味着“配了但没生效”，而没人会去查。
func TestDecodeSettingsRejectsUnknownKeys(t *testing.T) {
	if _, err := decodeSettings(`{"host":"h","port":25,"tls":"starttls","password":"oops"}`); err == nil {
		t.Fatal("带未知键（password）的 settings 应被拒绝")
	}
	st, err := decodeSettings(`{"host":"h","port":25,"tls":"starttls"}`)
	if err != nil {
		t.Fatalf("合法 settings 应被接受: %v", err)
	}
	if st.Host != "h" || st.Port != 25 || st.TLS != TLSStartTLS {
		t.Fatalf("解出的 settings 不对: %+v", st)
	}
}

// TestEncodeSettingsRoundTrip：编码解码往返不丢字段。
func TestEncodeSettingsRoundTrip(t *testing.T) {
	in := Settings{Host: "h", Port: 587, TLS: TLSStartTLS, From: "a@b.c",
		To: []string{"d@e.f"}, Username: "u", URL: "https://hooks.example.com/x", AllowPrivate: true}
	raw, err := encodeSettings(in)
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}
	back, err := decodeSettings(raw)
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	if back.Host != in.Host || back.Port != in.Port || back.TLS != in.TLS ||
		back.From != in.From || back.Username != in.Username || len(back.To) != 1 ||
		back.URL != in.URL || back.AllowPrivate != in.AllowPrivate {
		t.Fatalf("往返丢了字段: %+v", back)
	}
}

// TestNormalizeClearsIrrelevantSettings：smtp 通道不该留着 url，webhook 通道不该
// 留着 host/port/收件人——那是“改了 type 之后旧字段怎么办”的答案：不留。
func TestNormalizeClearsIrrelevantSettings(t *testing.T) {
	svc, db, _ := newTestService(t)
	ctx := context.Background()

	ch, err := svc.CreateChannel(ctx, ChannelInput{
		Name: "运维邮件", Type: TypeSMTP, Enabled: true, MaxAttempts: DefaultMaxAttempts,
		Settings: Settings{Host: "smtp.example.com", Port: 587, TLS: TLSStartTLS,
			From: "panel@example.com", To: []string{"ops@example.com"},
			URL: "https://leftover.example.com"},
	})
	if err != nil {
		t.Fatalf("建通道失败: %v", err)
	}
	if ch.Settings.URL != "" {
		t.Fatalf("smtp 通道不该留下 url，实际 %q", ch.Settings.URL)
	}
	var stored string
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT settings FROM notification_channels WHERE id = ?`, ch.ID).Scan(&stored); err != nil {
		t.Fatalf("读 settings 失败: %v", err)
	}
	if strings.Contains(stored, "leftover") {
		t.Fatalf("落库的 settings 里仍有 url: %s", stored)
	}

	hook, err := svc.CreateChannel(ctx, ChannelInput{
		Name: "回调", Type: TypeWebhook, Enabled: true, MaxAttempts: DefaultMaxAttempts,
		Settings: Settings{URL: "https://hooks.example.com/x", AllowPrivate: true,
			Host: "should-go-away", Port: 25, To: []string{"x@y.z"}},
	})
	if err != nil {
		t.Fatalf("建通道失败: %v", err)
	}
	if hook.Settings.Host != "" || hook.Settings.Port != 0 || len(hook.Settings.To) != 0 {
		t.Fatalf("webhook 通道不该留下 smtp 字段: %+v", hook.Settings)
	}
	if hook.Settings.URL != "https://hooks.example.com/x" || !hook.Settings.AllowPrivate {
		t.Fatalf("webhook 的 url 或内网授权丢了: %+v", hook.Settings)
	}
}

// −−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−
// 发送端适配
// −−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−

// capturingClient 记录 AgentSender 转给代理客户端的入参。
type capturingClient struct {
	in        agentclient.DeliverInput
	res       agentclient.DeliverResult
	err       error
	callCount int
}

func (c *capturingClient) Deliver(_ context.Context, in agentclient.DeliverInput) (agentclient.DeliverResult, error) {
	c.in = in
	c.callCount++
	return c.res, c.err
}

func TestAgentSenderMapsSettings(t *testing.T) {
	client := &capturingClient{res: agentclient.DeliverResult{Accepted: true, Status: 250, Detail: "已投递"}}
	sender := NewAgentSender(client)

	res, err := sender.Deliver(context.Background(), DeliverInput{
		Type: TypeSMTP,
		Settings: Settings{Host: "smtp.example.com", Port: 587, TLS: TLSStartTLS,
			From: "panel@example.com", To: []string{"ops@example.com"}, Username: "panel"},
		SecretRef: "sec_0123456789abcdef0123456789abcdef",
		Title:     "[告警] 磁盘将满",
		Body:      "正文",
	})
	if err != nil {
		t.Fatalf("投递失败: %v", err)
	}
	if !res.Accepted || res.Status != 250 {
		t.Fatalf("回执应原样透传，实际 %+v", res)
	}
	if client.in.Settings.Host != "smtp.example.com" || client.in.Settings.Port != 587 ||
		client.in.Settings.TLS != TLSStartTLS || client.in.Settings.Username != "panel" ||
		len(client.in.Settings.To) != 1 {
		t.Fatalf("设置没有原样转过去: %+v", client.in.Settings)
	}
	if client.in.SecretRef != "sec_0123456789abcdef0123456789abcdef" {
		t.Fatalf("secret_ref 应原样转过去，实际 %q", client.in.SecretRef)
	}

	if _, err := sender.Deliver(context.Background(), DeliverInput{
		Type:     TypeWebhook,
		Settings: Settings{URL: "https://hooks.internal/x", AllowPrivate: true},
		Title:    "t",
		Body:     "b",
	}); err != nil {
		t.Fatalf("webhook 投递失败: %v", err)
	}
	if client.in.Settings.URL != "https://hooks.internal/x" || !client.in.Settings.AllowPrivate {
		t.Fatalf("webhook 设置没有原样转给代理: %+v", client.in.Settings)
	}
}

// TestAgentSenderPropagatesError：代理不可用时把错误交上去，由 attemptDelivery
// 统一当成一次可重试的失败。
func TestAgentSenderPropagatesError(t *testing.T) {
	sender := NewAgentSender(&capturingClient{err: errors.New("连不上代理")})
	if _, err := sender.Deliver(context.Background(), DeliverInput{Type: TypeWebhook}); err == nil {
		t.Fatal("代理不可用时应返回错误")
	}
}
