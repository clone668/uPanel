package agent

import (
	"context"
	"path/filepath"
	"testing"

	"upanel/internal/agentstore"
	"upanel/migrations/agentdb"
)

// newTestAgentDB 打开一个**真实**的临时代理数据库并跑完全部迁移。
//
// 不用内存库、不用桩：预检计划的 CHECK 约束、触发器和时间列都要经过真实 SQLite
// 实现才算验证过，所以这里只把数据库文件放进 t.TempDir()。
func newTestAgentDB(t *testing.T) *agentstore.DB {
	t.Helper()
	return newTestAgentDBAt(t, filepath.Join(t.TempDir(), "agent.db"))
}

func newTestAgentDBAt(t *testing.T, path string) *agentstore.DB {
	t.Helper()
	db, err := agentstore.Open(context.Background(), agentstore.Options{
		Path:   path,
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
	return db
}

// newTestPlanStore 在独立临时库上构造预检计划存储。
func newTestPlanStore(t *testing.T) *agentstore.PreflightPlans {
	t.Helper()
	return newTestPlanStoreOn(t, newTestAgentDB(t))
}

func newTestPlanStoreOn(t *testing.T, db *agentstore.DB) *agentstore.PreflightPlans {
	t.Helper()
	plans, err := agentstore.NewPreflightPlans(db)
	if err != nil {
		t.Fatalf("构造预检计划存储失败: %v", err)
	}
	return plans
}

// newTestOperationStore 在独立临时库上构造执行记录存储。
func newTestOperationStore(t *testing.T) *agentstore.Operations {
	t.Helper()
	return newTestOperationStoreOn(t, newTestAgentDB(t))
}

func newTestOperationStoreOn(t *testing.T, db *agentstore.DB) *agentstore.Operations {
	t.Helper()
	ops, err := agentstore.NewOperations(db)
	if err != nil {
		t.Fatalf("构造执行记录存储失败: %v", err)
	}
	return ops
}
