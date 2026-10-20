package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"upanel/internal/store"
	"upanel/migrations"
)

func TestVersionFlag(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"--version"}, &out, &errOut); code != 0 {
		t.Fatalf("退出码 = %d，stderr = %q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "upanel-web") {
		t.Fatalf("版本输出缺少程序名: %q", out.String())
	}
	if !strings.Contains(out.String(), "0.0.0-dev") {
		t.Fatalf("未注入版本时应显示开发占位值: %q", out.String())
	}
}

func TestUnknownFlag(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"--nope"}, &out, &errOut); code != 2 {
		t.Fatalf("退出码 = %d，期望 2", code)
	}
}

func TestPositionalArgumentsRejected(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"serve"}, &out, &errOut); code != 2 {
		t.Fatalf("退出码 = %d，期望 2", code)
	}
	if !strings.Contains(errOut.String(), "位置参数") {
		t.Fatalf("stderr = %q", errOut.String())
	}
}

func TestMissingExplicitConfigExitsWithConfigError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.json")
	var out, errOut bytes.Buffer
	if code := run([]string{"--config", missing}, &out, &errOut); code != 2 {
		t.Fatalf("退出码 = %d，期望 2", code)
	}
	if !strings.Contains(errOut.String(), "配置错误") {
		t.Fatalf("stderr = %q", errOut.String())
	}
}

func TestInvalidModeRejected(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"--mode", "demo"}, &out, &errOut); code != 2 {
		t.Fatalf("退出码 = %d，期望 2", code)
	}
	if !strings.Contains(errOut.String(), "web.mode") {
		t.Fatalf("stderr = %q", errOut.String())
	}
}

func TestBadConfigFileRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(path, []byte(`{"web":{"mode":"mock","listen_address":"0.0.0.0:18881"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"--config", path, "--mode", "mock"}, &out, &errOut); code != 2 {
		t.Fatalf("退出码 = %d，期望 2（mock 不得绑定非回环地址）", code)
	}
	if !strings.Contains(errOut.String(), "18881") {
		t.Fatalf("stderr = %q", errOut.String())
	}
}

func TestProductionRefusesNonLoopbackBeforeTLS(t *testing.T) {
	// PLAN.md 第 6 节：面板自身要监听 HTTPS 9443；TLS 在 P10 接入。
	// 在此之前 production 绑定非回环地址必须直接失败，而不是把明文管理界面暴露出去。
	for _, addr := range []string{"0.0.0.0:9443", "192.0.2.10:9443", "[::]:9443"} {
		var out, errOut bytes.Buffer
		code := run([]string{"--mode", "production", "--listen", addr}, &out, &errOut)
		if code != 2 {
			t.Fatalf("%s: 退出码 = %d，期望 2（stderr=%q）", addr, code, errOut.String())
		}
		if !strings.Contains(errOut.String(), "只能绑定回环地址") {
			t.Fatalf("%s: stderr = %q", addr, errOut.String())
		}
	}
}

func TestDBFlagMustBeAbsolute(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run([]string{"--mode", "mock", "--db", "data/upanel.db"}, &out, &errOut)
	if code != 2 {
		t.Fatalf("退出码 = %d，期望 2（stderr=%q）", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "必须是绝对路径") {
		t.Fatalf("stderr = %q", errOut.String())
	}
}

func TestMissingDatabasePathRefused(t *testing.T) {
	// 显式把 database.path 置空且不传 --db：必须拒绝启动，不能猜一个路径。
	dir := t.TempDir()
	cfg := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(cfg, []byte(`{"web":{"mode":"mock"},"database":{"path":""}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code := run([]string{"--config", cfg}, &out, &errOut)
	if code != 2 {
		t.Fatalf("退出码 = %d，期望 2（stderr=%q）", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "未配置数据库路径") {
		t.Fatalf("stderr = %q", errOut.String())
	}
}

func TestDatabaseOpenFailureRefusesStart(t *testing.T) {
	// 目录不存在：store.Open 不负责创建数据目录，必须拒绝启动而不是继续跑。
	missing := filepath.Join(t.TempDir(), "absent", "upanel.db")
	var out, errOut bytes.Buffer
	code := run([]string{"--mode", "mock", "--db", missing}, &out, &errOut)
	if code != 1 {
		t.Fatalf("退出码 = %d，期望 1（stderr=%q）", code, errOut.String())
	}
	msg := errOut.String()
	if !strings.Contains(msg, "数据库打开失败") {
		t.Fatalf("stderr = %q", msg)
	}
	if strings.Contains(msg, "已开始监听") {
		t.Fatalf("数据库失败时不得开始监听: %q", msg)
	}
}

func TestMigrationFailureRefusesStart(t *testing.T) {
	// 数据库里存在高于程序内置版本的迁移：迁移必须失败，服务不得启动。
	dbPath := filepath.Join(t.TempDir(), "upanel.db")
	db, err := store.Open(context.Background(), store.Options{
		Path:   dbPath,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	if err := func() error {
		if _, err := db.Migrate(context.Background(), migrations.FS); err != nil {
			return err
		}
		_, err := db.SQL().ExecContext(context.Background(),
			`INSERT INTO schema_migrations(version, name, checksum, applied_at) VALUES (9999, 'future', 'x', '2030-01-01T00:00:00Z')`)
		return err
	}(); err != nil {
		t.Fatalf("准备数据库失败: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("关闭数据库失败: %v", err)
	}

	var out, errOut bytes.Buffer
	code := run([]string{"--mode", "mock", "--db", dbPath}, &out, &errOut)
	if code != 1 {
		t.Fatalf("退出码 = %d，期望 1（stderr=%q）", code, errOut.String())
	}
	msg := errOut.String()
	if !strings.Contains(msg, "数据库迁移失败") {
		t.Fatalf("stderr = %q", msg)
	}
	if strings.Contains(msg, "已开始监听") {
		t.Fatalf("迁移失败时不得开始监听: %q", msg)
	}
}
