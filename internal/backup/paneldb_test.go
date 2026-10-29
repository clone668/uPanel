package backup

// 面板库一致副本的测试（P12.2）。
//
// 这些用例要钉住的是**一致性**，不是"文件被复制了一份"：
//   1. 副本里有 WAL 里已提交但尚未 checkpoint 的行；
//   2. 副本是一个自洽的单文件（没有 -wal/-shm 跟着它）；
//   3. 源库一个字节都没被改动（只读打开，写回去会报 readonly）；
//   4. 源库不存在、目标是源库本身、副本不完整时都要**明说**，不能给一份"看起来正常"的文件。

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// openWALDB 建一个 WAL 模式的库并写入一行，返回保持打开的句柄（模拟面板在跑）。
func openWALDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE settings(key TEXT PRIMARY KEY, value TEXT)`); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO settings VALUES ('first', 'v1')`); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 7`); err != nil {
		t.Fatalf("设置 user_version 失败: %v", err)
	}
	return db
}

func TestCopyPanelDatabaseCapturesWAL(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "upanel.db")
	db := openWALDB(t, src)
	defer func() { _ = db.Close() }()

	// 第二行只在 WAL 里（写连接还开着，没有 checkpoint）。
	if _, err := db.Exec(`INSERT INTO settings VALUES ('second', 'v2')`); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if _, err := os.Stat(src + "-wal"); err != nil {
		t.Fatalf("前置条件不成立：应当存在 -wal 文件（%v）", err)
	}

	dst := filepath.Join(dir, "copy", "panel.db")
	copy, err := CopyPanelDatabase(context.Background(), src, dst)
	if err != nil {
		t.Fatalf("副本失败: %v", err)
	}
	if copy.Integrity != "ok" {
		t.Fatalf("完整性检查没通过：%q", copy.Integrity)
	}
	if copy.SourceSchemaVersion != 7 || copy.SchemaVersion != 7 {
		t.Fatalf("schema 版本应为 7，得到 源=%d 副本=%d", copy.SourceSchemaVersion, copy.SchemaVersion)
	}
	if copy.SizeBytes == 0 || copy.PageCount == 0 {
		t.Fatalf("副本读数不对：size=%d pages=%d", copy.SizeBytes, copy.PageCount)
	}

	// 副本必须是自洽的单文件：-wal/-shm 不该跟着它。
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(dst + suffix); err == nil {
			t.Fatalf("副本不该带 %s", suffix)
		}
	}

	// 关键断言：WAL 里那一行在副本里。
	cdb, err := sql.Open("sqlite", "file:"+dst+"?mode=ro")
	if err != nil {
		t.Fatalf("打开副本失败: %v", err)
	}
	defer func() { _ = cdb.Close() }()
	var got int
	if err := cdb.QueryRow(`SELECT count(*) FROM settings`).Scan(&got); err != nil {
		t.Fatalf("读副本失败: %v", err)
	}
	if got != 2 {
		t.Fatalf("副本应有 2 行（含 WAL 里那一行），得到 %d", got)
	}
	// 权限只在类 Unix 上有意义：Windows 上 os.Chmod 只映射只读位，
	// 断言 0600 会得到一个与产品行为无关的 0666。真机验收里查的是 Linux。
	if info, err := os.Stat(dst); err != nil {
		t.Fatalf("副本文件不存在: %v", err)
	} else if runtime.GOOS != "windows" && info.Mode().Perm() != panelCopyFileMode {
		t.Fatalf("副本权限应为 %o，得到 %o", panelCopyFileMode, info.Mode().Perm())
	}
}

func TestCopyPanelDatabaseLeavesSourceUntouched(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "upanel.db")
	db := openWALDB(t, src)
	defer func() { _ = db.Close() }()
	before, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("读源库失败: %v", err)
	}
	if _, err := CopyPanelDatabase(context.Background(), src, filepath.Join(dir, "copy.db")); err != nil {
		t.Fatalf("副本失败: %v", err)
	}
	after, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("读源库失败: %v", err)
	}
	if len(before) != len(after) {
		t.Fatalf("源库大小变了：%d → %d", len(before), len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("源库内容被改动（第 %d 字节）", i)
		}
	}
}

func TestCopyPanelDatabaseRejects(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "upanel.db")
	db := openWALDB(t, src)
	defer func() { _ = db.Close() }()

	cases := []struct {
		name string
		src  string
		dst  string
		want string
	}{
		{name: "源不存在", src: filepath.Join(dir, "nope.db"), dst: filepath.Join(dir, "c1.db"), want: "不存在"},
		{name: "目标是源本身", src: src, dst: src, want: "源文件本身"},
		{name: "相对路径", src: "upanel.db", dst: filepath.Join(dir, "c2.db"), want: "绝对路径"},
		{name: "目标为空", src: src, dst: "", want: "不能为空"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := CopyPanelDatabase(context.Background(), tc.src, tc.dst)
			if err == nil {
				t.Fatal("应当被拒")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误里应含 %q，得到 %q", tc.want, err.Error())
			}
		})
	}
}

// TestCopyPanelDatabaseOverwritesStaleCopy 确认上一次的副本不会被当成本次的。
func TestCopyPanelDatabaseOverwritesStaleCopy(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "upanel.db")
	db := openWALDB(t, src)
	defer func() { _ = db.Close() }()
	dst := filepath.Join(dir, "copy.db")
	if err := os.WriteFile(dst, []byte("stale"), 0o600); err != nil {
		t.Fatalf("造旧副本失败: %v", err)
	}
	copy, err := CopyPanelDatabase(context.Background(), src, dst)
	if err != nil {
		t.Fatalf("副本失败: %v", err)
	}
	if copy.SizeBytes <= int64(len("stale")) {
		t.Fatalf("旧副本没有被覆盖：size=%d", copy.SizeBytes)
	}
	if copy.CopiedAt.Before(time.Now().Add(-time.Minute)) {
		t.Fatalf("时间戳不对：%v", copy.CopiedAt)
	}
}
