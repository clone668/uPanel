package api

import (
	"context"
	"path/filepath"
	"testing"

	"upanel/internal/apps"
	"upanel/internal/audit"
	"upanel/internal/auth"
	"upanel/internal/operations"
	"upanel/internal/storage"
	"upanel/internal/store"
	"upanel/migrations"
)

// testOrigins 是测试里使用的允许源（生产默认监听地址对应的回环源）。
func testOrigins() []string {
	return []string{"http://127.0.0.1:9443"}
}

// newTestServices 构造跑在临时目录里的真实认证服务与审计写入器（同一个数据库）。
//
// 测试不做内存假实现：认证逻辑的关键点（唯一约束、外键、事务）只有真实 SQLite
// 才能验证；用替身反而会漏掉“约束没建对”这类问题。
func newTestServices(t *testing.T) (*auth.Service, *audit.Writer) {
	t.Helper()
	svc, writer, _ := newTestServicesWithDB(t)
	return svc, writer
}

// newTestServicesWithDB 额外返回数据库句柄，供需要直接查表断言（例如审计）的用例使用。
func newTestServicesWithDB(t *testing.T) (*auth.Service, *audit.Writer, *store.DB) {
	t.Helper()
	svc, db := newTestAuthWithDB(t)
	writer, err := audit.New(db, nil)
	if err != nil {
		t.Fatalf("构造审计写入器失败: %v", err)
	}
	return svc, writer, db
}

// newTestAuth 只取认证服务（不需要审计的用例）。
func newTestAuth(t *testing.T) *auth.Service {
	t.Helper()
	svc, _ := newTestServices(t)
	return svc
}

// newTestOperations 构造预检服务。
//
// agent 传 nil 表示“代理不可用”（mock 模式或没有 socket）：预检接口必须据此返回
// 503 AGENT_UNAVAILABLE，而不是伪造一份计划。需要预检成功的用例传 stubs_test 里的假代理。
func newTestOperations(t *testing.T, db *store.DB, agent operations.Agent) *operations.Service {
	t.Helper()
	svc, err := operations.New(db, agent, quietLogger(), operations.Options{})
	if err != nil {
		t.Fatalf("构造预检服务失败: %v", err)
	}
	return svc
}

// newTestApps 构造应用元数据服务。
//
// 卷检查器默认是 nil：带依赖卷的写入会回 503（“无法确认依赖卷是否存在”）。
// 需要验证依赖卷行为的用例调用 newTestAppsWithVolumes 给一份明确的卷列表。
func newTestApps(t *testing.T, db *store.DB) *apps.Service {
	t.Helper()
	svc, err := apps.New(db, apps.Options{})
	if err != nil {
		t.Fatalf("构造应用元数据服务失败: %v", err)
	}
	return svc
}

// fakeVolumeLister 是一份固定的受管理卷列表，供依赖卷校验使用。
type fakeVolumeLister struct {
	volumes []storage.Volume
	err     error
}

func (f fakeVolumeLister) Volumes(context.Context) ([]storage.Volume, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.volumes, nil
}

// newTestAppsWithVolumes 构造带卷列表的应用元数据服务。
func newTestAppsWithVolumes(t *testing.T, db *store.DB, volumes ...string) *apps.Service {
	t.Helper()
	list := make([]storage.Volume, 0, len(volumes))
	for _, id := range volumes {
		list = append(list, storage.Volume{ID: id, DisplayName: id})
	}
	svc, err := apps.New(db, apps.Options{
		Volumes: apps.NewVolumeChecker(fakeVolumeLister{volumes: list}),
	})
	if err != nil {
		t.Fatalf("构造应用元数据服务失败: %v", err)
	}
	return svc
}

// newTestAuthWithDB 构造一个跑在临时目录里的真实认证服务。
//
// 测试不做内存假实现：认证逻辑的关键点（唯一约束、外键、事务）只有真实 SQLite
// 才能验证；用替身反而会漏掉“约束没建对”这类问题。
func newTestAuthWithDB(t *testing.T) (*auth.Service, *store.DB) {
	t.Helper()

	dir := t.TempDir()
	db, err := store.Open(context.Background(), store.Options{
		Path:   filepath.Join(dir, "upanel.db"),
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
	svc, err := auth.NewService(db, auth.Options{})
	if err != nil {
		t.Fatalf("构造认证服务失败: %v", err)
	}
	return svc, db
}

// seedTestUser 创建测试账号。密码哈希真实计算（Argon2id），不写死哈希串。
func seedTestUser(t *testing.T, svc *auth.Service, username, password string) auth.User {
	t.Helper()
	user, err := svc.CreateUser(context.Background(), auth.CreateUserInput{
		Username:    username,
		DisplayName: username,
		Role:        auth.RoleAdmin,
		Password:    password,
	})
	if err != nil {
		t.Fatalf("创建测试账号失败: %v", err)
	}
	return user
}
