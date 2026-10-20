package operations

// P03.3 服务层用例：代理事件镜像（水位线、幂等）、事件分页、重连用的 seq 区间、
// 日志游标与容量上限。
//
// 这些用例跑在真实 SQLite 上，不用内存假库：水位线的正确性依赖“同一个事务里
// 事件与水位一起落地”，那种保证只有真库能验。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"upanel/internal/agentclient"
)

// mirrorFixture 提交一个任务，然后把代理事件准备好。
func mirrorFixture(t *testing.T) (*jobFixture, Job) {
	t.Helper()
	f := newJobFixture(t, nil)
	pf := f.preflight(7)
	job := f.mustSubmit(f.submitInput(pf.ID, "key-mirror", "PING AAAA"))
	return f, job
}

// agentEvents 造两条代理事件：一条进度、一条结束。
func agentEvents() []agentclient.OperationEvent {
	return []agentclient.OperationEvent{
		{Seq: 1, Kind: "progress", Payload: json.RawMessage(`{"step":"ping_done","slices":3}`)},
		{Seq: 2, Kind: "finished", Payload: json.RawMessage(`{"outcome":"verified"}`)},
	}
}

func TestMirrorAgentEventsIsIdempotent(t *testing.T) {
	f, job := mirrorFixture(t)
	f.agent.events = agentEvents()

	ctx := context.Background()
	if err := f.svc.MirrorAgentEvents(ctx, job.ID, job.OperationID); err != nil {
		t.Fatalf("镜像事件失败: %v", err)
	}
	// 第二次调用必须一条都不抄：水位线已经把 seq 1、2 划掉了。
	if err := f.svc.MirrorAgentEvents(ctx, job.ID, job.OperationID); err != nil {
		t.Fatalf("第二次镜像事件失败: %v", err)
	}

	events, err := f.svc.ListJobEvents(ctx, 7, job.ID, 0, MaxJobEventPage)
	if err != nil {
		t.Fatalf("读取事件失败: %v", err)
	}
	// 提交路径自己写了两条 status（queued、代理回 accepted），镜像再补一条 progress
	// 与一条 status(finished)。关键是**第二次镜像一条都没加**。
	if len(events) != 4 {
		t.Fatalf("事件条数 = %d，期望 4（2 条 status + progress + finished）：%s",
			len(events), dumpKinds(events))
	}
	kinds := []string{events[0].Kind, events[1].Kind, events[2].Kind, events[3].Kind}
	want := []string{"status", "status", "progress", "status"}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("事件类型 = %v，期望 %v", kinds, want)
		}
	}

	var watermark int64
	if err := f.db.QueryRowContext(ctx,
		`SELECT agent_event_seq FROM jobs WHERE id = ?`, job.ID).Scan(&watermark); err != nil {
		t.Fatalf("读水位失败: %v", err)
	}
	if watermark != 2 {
		t.Fatalf("代理事件水位 = %d，期望 2", watermark)
	}
}

// 追加的事件必须带上 agent_seq 与原始 payload——不然日志页拿不到 message，
// 排查时只能看到一串“有一条进度事件”。
func TestMirroredEventKeepsAgentPayload(t *testing.T) {
	f, job := mirrorFixture(t)
	f.agent.events = agentEvents()
	if err := f.svc.MirrorAgentEvents(context.Background(), job.ID, job.OperationID); err != nil {
		t.Fatalf("镜像事件失败: %v", err)
	}
	events, err := f.svc.ListJobEvents(context.Background(), 7, job.ID, 0, 10)
	if err != nil {
		t.Fatalf("读取事件失败: %v", err)
	}
	var mirrored JobEvent
	for _, ev := range events {
		if ev.Kind == "progress" {
			mirrored = ev
			break
		}
	}
	if mirrored.Seq == 0 {
		t.Fatalf("没有读到镜像的 progress 事件：%s", dumpKinds(events))
	}
	payload := string(mirrored.Payload)
	for _, want := range []string{`"agent_seq":1`, `"agent_kind":"progress"`, `ping_done`} {
		if !strings.Contains(payload, want) {
			t.Fatalf("事件内容缺少 %s：%s", want, payload)
		}
	}
}

// 未知的代理事件类型不能丢：它按日志留存，而不是被枚举挡在门外。
func TestUnknownAgentEventKindBecomesLog(t *testing.T) {
	f, job := mirrorFixture(t)
	f.agent.events = []agentclient.OperationEvent{
		{Seq: 1, Kind: "something_new", Payload: json.RawMessage(`{"x":1}`)},
	}
	if err := f.svc.MirrorAgentEvents(context.Background(), job.ID, job.OperationID); err != nil {
		t.Fatalf("镜像事件失败: %v", err)
	}
	logs, _, err := f.svc.ListJobLogs(context.Background(), 7, job.ID, "", 10)
	if err != nil {
		t.Fatalf("读取日志失败: %v", err)
	}
	if len(logs) != 1 || logs[0].Kind != "log" {
		t.Fatalf("未知类型事件没有落成日志：%s", dumpKinds(logs))
	}
}

// 重连补发只回“客户端还没看过的”：after 之后的事件才是要推的。
func TestListJobEventsRespectsAfterSeq(t *testing.T) {
	f, job := mirrorFixture(t)
	f.agent.events = agentEvents()
	ctx := context.Background()
	if err := f.svc.MirrorAgentEvents(ctx, job.ID, job.OperationID); err != nil {
		t.Fatalf("镜像事件失败: %v", err)
	}

	all, err := f.svc.ListJobEvents(ctx, 7, job.ID, 0, 10)
	if err != nil || len(all) != 4 {
		t.Fatalf("全量事件 = %d 条（err=%v），期望 4", len(all), err)
	}
	rest, err := f.svc.ListJobEvents(ctx, 7, job.ID, all[1].Seq, 10)
	if err != nil {
		t.Fatalf("按 after 读取失败: %v", err)
	}
	if len(rest) != 2 || rest[0].Seq <= all[1].Seq {
		t.Fatalf("after=%d 的事件 = %s，期望只包含更大的 seq", all[1].Seq, dumpSeqs(rest))
	}
}

// 别人的任务读不到事件：归属校验在服务层，而不是靠调用方自觉。
func TestListJobEventsEnforcesOwnership(t *testing.T) {
	f, job := mirrorFixture(t)
	_, err := f.svc.ListJobEvents(context.Background(), 8, job.ID, 0, 10)
	if !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("用别的账号读取事件应报 TaskNotFound，实际 %v", err)
	}
}

// 区间查询是 gap 判定的依据：历史被清理后 min > 1。
//
// 用“新任务的事件从 seq=5 开始”来构造这个状态，而不是删行：状态类事件根本不允许删
// （这正是要保住的不变式），而“能被清理的只有日志”恰恰说明 gap 会出现在日志区间上。
func TestBoundsReflectPrunedHistory(t *testing.T) {
	f, job := mirrorFixture(t)
	ctx := context.Background()

	// 复制一个任务行，但它的第一条事件从 seq=5 开始 —— 相当于 seq 1..4 已被清理。
	const prunedJobID = "job_ffffffffffffffffffffffffffffffff"
	var preflightID string
	if err := f.db.QueryRowContext(ctx, `SELECT preflight_id FROM jobs WHERE id = ?`, job.ID).
		Scan(&preflightID); err != nil {
		t.Fatalf("读取预检引用失败: %v", err)
	}
	if _, err := f.db.ExecContext(ctx, `
		INSERT INTO jobs (id, operation_id, preflight_id, user_id, action, status,
		                  created_at, updated_at, needs_sync)
		VALUES (?, ?, ?, 7, 'acceptance.ping', 'running', ?, ?, 0)`,
		prunedJobID, "op_ffffffffffffffffffffffffffffffff", preflightID,
		"2026-10-20T09:00:00Z", "2026-10-20T09:00:00Z"); err != nil {
		t.Fatalf("准备任务行失败: %v", err)
	}
	for _, seq := range []int{5, 6} {
		if _, err := f.db.ExecContext(ctx,
			`INSERT INTO job_events (job_id, seq, kind, redacted_payload, created_at)
			 VALUES (?, ?, 'status', '{"status":"running"}', '2026-10-20T09:00:00Z')`,
			prunedJobID, seq); err != nil {
			t.Fatalf("插入事件失败: %v", err)
		}
	}

	bounds, err := f.svc.Bounds(ctx, 7, prunedJobID)
	if err != nil {
		t.Fatalf("读取区间失败: %v", err)
	}
	if bounds.MinSeq != 5 || bounds.MaxSeq != 6 {
		t.Fatalf("区间 = %+v，期望 min=5、max=6", bounds)
	}
	// 客户端带着 seq=1 来重连：1 < 5-1，说明它停在被清理的历史里，必须收到 gap。
	if !(1 < bounds.MinSeq-1) {
		t.Fatalf("gap 判定条件不成立：min=%d", bounds.MinSeq)
	}
}

// 日志分页：limit 生效、next_cursor 能翻下一页、翻完为 null。
func TestListJobLogsPaginates(t *testing.T) {
	f, job := mirrorFixture(t)
	ctx := context.Background()
	f.agent.events = []agentclient.OperationEvent{
		{Seq: 1, Kind: "log", Payload: json.RawMessage(`{"message":"第一行"}`)},
		{Seq: 2, Kind: "log", Payload: json.RawMessage(`{"message":"第二行"}`)},
		{Seq: 3, Kind: "log", Payload: json.RawMessage(`{"message":"第三行"}`)},
	}
	if err := f.svc.MirrorAgentEvents(ctx, job.ID, job.OperationID); err != nil {
		t.Fatalf("镜像事件失败: %v", err)
	}

	first, next, err := f.svc.ListJobLogs(ctx, 7, job.ID, "", 2)
	if err != nil {
		t.Fatalf("第一页失败: %v", err)
	}
	if len(first) != 2 || next == "" {
		t.Fatalf("第一页 = %d 条，next=%q，期望 2 条且有下一页", len(first), next)
	}
	second, next2, err := f.svc.ListJobLogs(ctx, 7, job.ID, next, 2)
	if err != nil {
		t.Fatalf("第二页失败: %v", err)
	}
	if len(second) != 1 || next2 != "" {
		t.Fatalf("第二页 = %d 条，next=%q，期望 1 条且没有下一页", len(second), next2)
	}
	if second[0].Seq <= first[len(first)-1].Seq {
		t.Fatalf("第二页 seq = %d，应大于第一页最后一条 %d", second[0].Seq, first[len(first)-1].Seq)
	}

	if _, _, err := f.svc.ListJobLogs(ctx, 7, job.ID, "不是游标", 2); !errors.Is(err, ErrInvalidLogCursor) {
		t.Fatalf("非法游标应报 ErrInvalidLogCursor，实际 %v", err)
	}
}

// 日志容量上限：超出后删最旧的日志，状态事件一条都不能少。
func TestPruneKeepsStatusEventsAndNewestLogs(t *testing.T) {
	f, job := mirrorFixture(t)
	ctx := context.Background()

	// 直接灌 MaxJobLogEvents+5 条日志（走 appendJobEventTx，保证 seq 单调）。
	err := f.svc.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		for i := 0; i < MaxJobLogEvents+5; i++ {
			payload := map[string]any{"agent_seq": i + 1, "agent_kind": "log",
				"payload": json.RawMessage(`{"message":"x"}`)}
			if err := appendJobEventTx(ctx, tx, job.ID, "log", payload, f.clock.Now()); err != nil {
				return err
			}
		}
		return pruneJobLogsTx(ctx, tx, job.ID)
	})
	if err != nil {
		t.Fatalf("灌日志失败: %v", err)
	}

	logs, _, err := f.svc.ListJobLogs(ctx, 7, job.ID, "", MaxJobEventPage)
	if err != nil {
		t.Fatalf("读取日志失败: %v", err)
	}
	if len(logs) != MaxJobEventPage {
		t.Fatalf("第一页日志 = %d 条，期望 %d", len(logs), MaxJobEventPage)
	}

	var logCount, statusCount int
	if err := f.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM job_events WHERE job_id = ? AND kind = 'log'`, job.ID).Scan(&logCount); err != nil {
		t.Fatalf("统计日志失败: %v", err)
	}
	if err := f.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM job_events WHERE job_id = ? AND kind = 'status'`, job.ID).Scan(&statusCount); err != nil {
		t.Fatalf("统计状态事件失败: %v", err)
	}
	if logCount != MaxJobLogEvents {
		t.Fatalf("清理后日志 = %d 条，期望 %d", logCount, MaxJobLogEvents)
	}
	if statusCount == 0 {
		t.Fatal("清理日志把状态事件也删了：状态事件必须永不删除")
	}
}

func dumpKinds(events []JobEvent) string {
	parts := make([]string, 0, len(events))
	for _, ev := range events {
		parts = append(parts, ev.Kind)
	}
	return strings.Join(parts, ",")
}

func dumpSeqs(events []JobEvent) string {
	parts := make([]string, 0, len(events))
	for _, ev := range events {
		parts = append(parts, itoa(ev.Seq))
	}
	return strings.Join(parts, ",")
}

func itoa(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}
