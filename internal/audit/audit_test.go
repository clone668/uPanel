package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"upanel/internal/store"
	"upanel/migrations"
)

func testStore(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), store.Options{
		Path:   filepath.Join(t.TempDir(), "upanel.db"),
		Logger: slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})),
	})
	if err != nil {
		t.Fatalf("打开测试数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Migrate(context.Background(), migrations.FS); err != nil {
		t.Fatalf("迁移测试数据库失败: %v", err)
	}
	return db
}

func TestRecordStoresRedactedDetails(t *testing.T) {
	db := testStore(t)
	now := func() time.Time { return time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC) }
	w, err := New(db, now)
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}

	err = w.Record(context.Background(), Event{
		ActorSnapshot: "admin(admin)#1",
		Action:        ActionUserCreate,
		Target:        "user:2",
		RequestID:     "rid-1",
		Outcome:       OutcomeSuccess,
		Details: map[string]any{
			"username":         "alice",
			"role":             "viewer",
			"initial_password": "hunter2-should-not-leak",
			"password_hash":    "$argon2id$v=19$...",
			"csrf_token":       "token-should-not-leak",
		},
	})
	if err != nil {
		t.Fatalf("Record 失败: %v", err)
	}

	var (
		snapshot, action, target, requestID, outcome, details, createdAt string
		actorID                                                          *int64
	)
	err = db.SQL().QueryRowContext(context.Background(), `
		SELECT actor_id, actor_snapshot, action, target, request_id, outcome, redacted_details, created_at
		FROM audit_events ORDER BY id DESC LIMIT 1`).
		Scan(&actorID, &snapshot, &action, &target, &requestID, &outcome, &details, &createdAt)
	if err != nil {
		t.Fatalf("读取审计失败: %v", err)
	}
	if actorID != nil {
		t.Fatalf("没有账号主体时 actor_id 必须是 NULL，实际 %v", *actorID)
	}
	if snapshot != "admin(admin)#1" || action != ActionUserCreate || target != "user:2" ||
		requestID != "rid-1" || outcome != OutcomeSuccess {
		t.Fatalf("审计字段不对: %+v", []string{snapshot, action, target, requestID, outcome})
	}
	if createdAt != "2026-10-20T12:00:00Z" {
		t.Fatalf("created_at = %q，期望 RFC3339 UTC", createdAt)
	}

	// 秘密字段必须被替换，非秘密字段原样保留。
	var parsed map[string]any
	if err := json.Unmarshal([]byte(details), &parsed); err != nil {
		t.Fatalf("明细不是合法 JSON: %v（%s）", err, details)
	}
	if parsed["username"] != "alice" || parsed["role"] != "viewer" {
		t.Fatalf("非秘密字段不应被改动: %v", parsed)
	}
	for _, key := range []string{"initial_password", "password_hash", "csrf_token"} {
		if parsed[key] != redactedValue {
			t.Fatalf("%s 应被替换为 %s，实际 %v", key, redactedValue, parsed[key])
		}
	}
	if strings.Contains(details, "hunter2") || strings.Contains(details, "argon2id") {
		t.Fatalf("审计明细里出现了秘密原文: %s", details)
	}
}

func TestAuditIsAppendOnly(t *testing.T) {
	db := testStore(t)
	w, err := New(db, nil)
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	if err := w.Record(context.Background(), Event{
		Action: ActionUserDelete, Outcome: OutcomeSuccess,
	}); err != nil {
		t.Fatalf("Record 失败: %v", err)
	}

	if _, err := db.SQL().ExecContext(context.Background(),
		`UPDATE audit_events SET outcome = 'failure'`); err == nil {
		t.Fatal("审计表必须拒绝 UPDATE")
	} else if !strings.Contains(err.Error(), "只追加") {
		t.Fatalf("UPDATE 的报错应说明只追加，实际 %v", err)
	}

	if _, err := db.SQL().ExecContext(context.Background(), `DELETE FROM audit_events`); err == nil {
		t.Fatal("审计表必须拒绝 DELETE")
	} else if !strings.Contains(err.Error(), "只追加") {
		t.Fatalf("DELETE 的报错应说明只追加，实际 %v", err)
	}
}

func TestRecordRejectsInvalidEvents(t *testing.T) {
	db := testStore(t)
	w, err := New(db, nil)
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}

	if err := w.Record(context.Background(), Event{Outcome: OutcomeSuccess}); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("缺少 action 应返回 ErrInvalidEvent，实际 %v", err)
	}
	if err := w.Record(context.Background(), Event{Action: ActionUserCreate, Outcome: "maybe"}); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("未知 outcome 应返回 ErrInvalidEvent，实际 %v", err)
	}

	if _, err := New(nil, nil); err == nil {
		t.Fatal("没有数据库时构造 Writer 必须失败")
	}
}

func TestAuditSurvivesActorDeletion(t *testing.T) {
	db := testStore(t)
	w, err := New(db, nil)
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	ctx := context.Background()

	// 建一个账号，然后删掉它：审计行必须仍然可读（actor_id 没有外键，按设计保留旧值）。
	res, err := db.SQL().ExecContext(ctx, `
		INSERT INTO users (username, display_name, password_hash, role, disabled, version, created_at, updated_at)
		VALUES ('gone', '', 'x', 'admin', 0, 1, '2026-10-20T00:00:00Z', '2026-10-20T00:00:00Z')`)
	if err != nil {
		t.Fatalf("插入账号失败: %v", err)
	}
	id, _ := res.LastInsertId()

	if err := w.Record(ctx, Event{
		ActorID:       id,
		ActorSnapshot: "gone(admin)#" + strconv.FormatInt(id, 10),
		Action:        ActionUserCreate,
		Outcome:       OutcomeSuccess,
	}); err != nil {
		t.Fatalf("Record 失败: %v", err)
	}

	if _, err := db.SQL().ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id); err != nil {
		t.Fatalf("删除账号失败: %v", err)
	}

	var snapshot string
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT actor_snapshot FROM audit_events WHERE actor_id = ?`, id).Scan(&snapshot); err != nil {
		t.Fatalf("账号删除后审计行应仍可读: %v", err)
	}
	if snapshot == "" {
		t.Fatal("actor_snapshot 必须保留账号快照")
	}
}

// TestRecordJoinsBusinessTransaction 证明审计与业务同生共死（P02.4）：
// 业务事务回滚时审计行一起消失，提交时一起出现。
func TestRecordJoinsBusinessTransaction(t *testing.T) {
	db := testStore(t)
	w, err := New(db, nil)
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	ev := Event{
		ActorSnapshot: "admin(admin)#1",
		Action:        ActionUserUpdate,
		Target:        "user:2",
		Outcome:       OutcomeSuccess,
	}
	countRows := func() int {
		t.Helper()
		var n int
		if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM audit_events`).Scan(&n); err != nil {
			t.Fatalf("统计审计行失败: %v", err)
		}
		return n
	}

	// 1) 业务失败 -> 整笔回滚：审计行不能留下。
	boom := errors.New("业务失败")
	err = db.WithTx(context.Background(), func(ctx context.Context, _ *sql.Tx) error {
		if err := w.Record(ctx, ev); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("期望业务错误上抛，实际 %v", err)
	}
	if n := countRows(); n != 0 {
		t.Fatalf("业务回滚后审计行应为 0，实际 %d", n)
	}

	// 2) 业务成功 -> 审计与业务一起提交。
	err = db.WithTx(context.Background(), func(ctx context.Context, _ *sql.Tx) error {
		return w.Record(ctx, ev)
	})
	if err != nil {
		t.Fatalf("提交事务失败: %v", err)
	}
	if n := countRows(); n != 1 {
		t.Fatalf("提交后审计行应为 1，实际 %d", n)
	}

	// 3) 没有事务时独立提交（用于记录被拒绝的请求）。
	if err := w.Record(context.Background(), ev); err != nil {
		t.Fatalf("无事务写入失败: %v", err)
	}
	if n := countRows(); n != 2 {
		t.Fatalf("独立写入后审计行应为 2，实际 %d", n)
	}
}
