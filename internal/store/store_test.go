package store

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func openTemp(t *testing.T) (*DB, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "upanel.db")
	db, err := Open(context.Background(), Options{Path: path, Logger: testLogger()})
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, path
}

func TestOpenAppliesPragmas(t *testing.T) {
	db, _ := openTemp(t)
	ctx := context.Background()

	var mode string
	if err := db.SQL().QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatalf("读取 journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q，期望 wal", mode)
	}
	var fk int
	if err := db.SQL().QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk); err != nil {
		t.Fatalf("读取 foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Fatalf("foreign_keys = %d，期望 1", fk)
	}
	var busy int
	if err := db.SQL().QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&busy); err != nil {
		t.Fatalf("读取 busy_timeout: %v", err)
	}
	if busy != int(DefaultBusyTimeout.Milliseconds()) {
		t.Fatalf("busy_timeout = %d，期望 %d", busy, DefaultBusyTimeout.Milliseconds())
	}
}

func TestOpenRejectsBadPaths(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	cases := []struct {
		name string
		opts Options
	}{
		{"空路径", Options{Path: "", Logger: testLogger()}},
		{"相对路径", Options{Path: "upanel.db", Logger: testLogger()}},
		{"目录不存在", Options{Path: filepath.Join(dir, "missing", "upanel.db"), Logger: testLogger()}},
		{"路径含问号", Options{Path: filepath.Join(dir, "a?b.db"), Logger: testLogger()}},
		{"没有日志器", Options{Path: filepath.Join(dir, "upanel.db")}},
		{"busy_timeout 过小", Options{Path: filepath.Join(dir, "upanel.db"), Logger: testLogger(), BusyTimeout: 50 * time.Millisecond}},
	}
	for _, c := range cases {
		if db, err := Open(ctx, c.opts); err == nil {
			_ = db.Close()
			t.Fatalf("%s: 期望打开失败，实际成功", c.name)
		}
	}
}

func TestWithTxCommitsAndRollsBack(t *testing.T) {
	db, _ := openTemp(t)
	ctx := context.Background()

	if _, err := db.SQL().ExecContext(ctx, `CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT NOT NULL)`); err != nil {
		t.Fatalf("建表失败: %v", err)
	}

	if err := db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO t (id, name) VALUES (1, 'committed')`)
		return err
	}); err != nil {
		t.Fatalf("提交事务失败: %v", err)
	}

	wantErr := errors.New("业务拒绝")
	if err := db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO t (id, name) VALUES (2, 'rolled back')`); err != nil {
			return err
		}
		return wantErr
	}); !errors.Is(err, wantErr) {
		t.Fatalf("回滚事务返回 %v，期望 %v", err, wantErr)
	}

	var count int
	if err := db.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM t`).Scan(&count); err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if count != 1 {
		t.Fatalf("行数 = %d，期望 1（回滚的行不能留下）", count)
	}

	// panic 也必须回滚，并且把 panic 继续抛给调用方。
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatalf("期望 panic 继续向上传播")
			}
		}()
		_ = db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, `INSERT INTO t (id, name) VALUES (3, 'panic')`); err != nil {
				return err
			}
			panic("boom")
		})
	}()

	if err := db.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM t`).Scan(&count); err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if count != 1 {
		t.Fatalf("panic 后行数 = %d，期望 1", count)
	}
}

func TestDataSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "upanel.db")
	ctx := context.Background()

	db, err := Open(ctx, Options{Path: path, Logger: testLogger()})
	if err != nil {
		t.Fatalf("首次打开失败: %v", err)
	}
	if _, err := db.SQL().ExecContext(ctx, `CREATE TABLE kv (k TEXT PRIMARY KEY, v TEXT NOT NULL)`); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	if err := db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO kv (k, v) VALUES ('persisted', 'yes')`)
		return err
	}); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}

	// 重新打开：模拟进程重启。
	db2, err := Open(ctx, Options{Path: path, Logger: testLogger()})
	if err != nil {
		t.Fatalf("重新打开失败: %v", err)
	}
	defer func() { _ = db2.Close() }()

	var v string
	if err := db2.SQL().QueryRowContext(ctx, `SELECT v FROM kv WHERE k = 'persisted'`).Scan(&v); err != nil {
		t.Fatalf("重启后读取失败: %v", err)
	}
	if v != "yes" {
		t.Fatalf("重启后值 = %q，期望 yes", v)
	}

	// WAL 模式下，数据文件之外还有 -wal/-shm；关机/关闭后数据必须在主文件里可读。
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("数据库文件不存在: %v", err)
	}
}

// TestOpenTightensDatabaseFileMode 断言库文件不会被同机其他用户读到。
// 只在 POSIX 上有意义：Windows 的 os.Chmod 只控制只读位，权限位不反映真实 ACL。
func TestOpenTightensDatabaseFileMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上 chmod 不控制权限位，本用例在 Linux 验收 VM 上执行")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "upanel.db")
	ctx := context.Background()

	db, err := Open(ctx, Options{Path: path, Logger: testLogger()})
	if err != nil {
		t.Fatalf("首次打开失败: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}

	// 模拟“历史遗留的宽松权限”：SQLite 按 umask 建文件时常见的结果。
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("设置初始权限失败: %v", err)
	}

	db2, err := Open(ctx, Options{Path: path, Logger: testLogger()})
	if err != nil {
		t.Fatalf("重新打开失败: %v", err)
	}
	defer func() { _ = db2.Close() }()

	for _, name := range []string{path, path + "-wal", path + "-shm"} {
		info, err := os.Stat(name)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			t.Fatalf("stat %s 失败: %v", name, err)
		}
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			t.Fatalf("%s 权限为 %04o，期望不对 group/other 开放", name, perm)
		}
	}
}

// TestWithTxQueuesBehindAnotherWriter 是 P03.2 VM 验收撞出来的真实缺陷的回归用例。
//
// 症状：代理刚重启（启动时那笔“把未结束操作标成 interrupted”的写事务）之后的第一次
// 任务提交返回 500，代理日志是 `database is locked (SQLITE_BUSY)`，落在
// `UPDATE preflight_plans` 上——离上一笔写事务提交只差 350 ms。
//
// 原因不是 busy_timeout 太小，而是默认的 DEFERRED 事务在 WAL 下先取读快照、再升级成写，
// **升级失败时 busy handler 不会被调用**，直接把 SQLITE_BUSY 返回给调用方。
// 修法是连接串加 `_txlock=immediate`：BEGIN 时就取写锁，冲突交给 busy_timeout 排队。
//
// 用例把两个“先读后写”的事务重叠起来：A 读到并写之后持锁 30 ms，B 在同样的位置写。
// DEFERRED 下 B 必然 SQLITE_BUSY；IMMEDIATE 下 B 在 BEGIN 处排队，等 A 提交后继续。
func TestWithTxQueuesBehindAnotherWriter(t *testing.T) {
	db, _ := openTemp(t)
	ctx := context.Background()
	if _, err := db.SQL().ExecContext(ctx, `CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT NOT NULL)`); err != nil {
		t.Fatalf("建表失败: %v", err)
	}

	holding := make(chan struct{})
	release := make(chan struct{})
	aErr := make(chan error, 1)
	go func() {
		aErr <- db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM t`).Scan(&n); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO t (name) VALUES ('a')`); err != nil {
				return err
			}
			close(holding)
			<-release // 持有写锁，等 B 撞上来
			return nil
		})
	}()
	<-holding

	bErr := make(chan error, 1)
	go func() {
		bErr <- db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM t`).Scan(&n); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO t (name) VALUES ('b')`)
			return err
		})
	}()

	time.Sleep(30 * time.Millisecond) // 给 B 足够时间走到 BEGIN / 升级点
	close(release)

	if err := <-aErr; err != nil {
		t.Fatalf("第一个写事务失败: %v", err)
	}
	if err := <-bErr; err != nil {
		t.Fatalf("并发写事务应排队等待而不是返回 SQLITE_BUSY，实际: %v", err)
	}

	var count int
	if err := db.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM t`).Scan(&count); err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if count != 2 {
		t.Fatalf("行数 = %d，期望 2（两个事务都必须提交）", count)
	}
}
