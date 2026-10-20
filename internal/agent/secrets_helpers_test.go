package agent

import (
	"context"
	"crypto/rand"
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"upanel/internal/agentstore"
	"upanel/migrations/agentdb"
)

// testLogger 返回一个丢弃全部输出的 Logger（错误级别以上也不打印，避免污染测试输出）。
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

// newTestSecretStore 打开一个**真实**的临时代理数据库并返回秘密存储。
//
// 这里不用桩：加密、SQLite、迁移、CHECK 约束都走生产实现，只是数据库放在 t.TempDir()
// 里。密钥同样是真的 32 字节随机密钥（不落盘，密钥文件的权限校验在 Linux 上另行验证）。
func newTestSecretStore(t *testing.T) *agentstore.Secrets {
	t.Helper()
	return newTestSecretStoreWith(t, agentstore.SecretsOptions{})
}

func newTestSecretStoreWith(t *testing.T, opts agentstore.SecretsOptions) *agentstore.Secrets {
	t.Helper()
	dir := t.TempDir()
	db, err := agentstore.Open(context.Background(), agentstore.Options{
		Path:   filepath.Join(dir, "agent.db"),
		Logger: testLogger(),
	})
	if err != nil {
		t.Fatalf("打开代理数据库失败: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("关闭代理数据库失败: %v", err)
		}
	})
	if _, err := db.Migrate(context.Background(), agentdb.FS); err != nil {
		t.Fatalf("代理数据库迁移失败: %v", err)
	}

	key := make([]byte, agentstore.SecretKeyBytes)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("生成测试密钥失败: %v", err)
	}
	keyring, err := agentstore.NewKeyring(key)
	if err != nil {
		t.Fatalf("构造 Keyring 失败: %v", err)
	}
	secrets, err := agentstore.NewSecrets(db, keyring, testLogger(), opts)
	if err != nil {
		t.Fatalf("构造秘密存储失败: %v", err)
	}
	return secrets
}

// reqContext 返回一个随测试结束自动取消的上下文。
func reqContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx
}
