package auth

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"upanel/internal/store"
	"upanel/migrations"
)

// testService 构造一个指向临时 SQLite 文件的真实 Service。
//
// 不用内存替身：本包的正确性大量依赖数据库约束（UNIQUE、外键、事务），
// 替身会让“约束没建对”这类问题在测试里消失。
func testService(t *testing.T, opts Options) *Service {
	t.Helper()

	db, err := store.Open(context.Background(), store.Options{
		Path:   filepath.Join(t.TempDir(), "upanel.db"),
		Logger: testLogger(),
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

	svc, err := NewService(db, opts)
	if err != nil {
		t.Fatalf("构造 Service 失败: %v", err)
	}
	return svc
}

// fakeClock 是可推进的测试时钟。
type fakeClock struct{ now time.Time }

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

const testPassword = "correct-horse-9"

// mustCreateAdmin 建一个 admin 账号，测试里复用。
func mustCreateAdmin(t *testing.T, svc *Service, username string) User {
	t.Helper()
	user, err := svc.CreateUser(context.Background(), CreateUserInput{
		Username:    username,
		DisplayName: username,
		Password:    testPassword,
		Role:        RoleAdmin,
	})
	if err != nil {
		t.Fatalf("创建账号失败: %v", err)
	}
	return user
}

// testLogger 返回丢弃输出的日志器：测试不关心日志内容，但不能传 nil。
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestCreateFirstAdmin(t *testing.T) {
	svc := testService(t, Options{})
	ctx := context.Background()

	if n, err := svc.CountUsers(ctx); err != nil || n != 0 {
		t.Fatalf("新库账号数 = %d，错误 = %v", n, err)
	}

	user, err := svc.CreateFirstAdmin(ctx, "Admin", "管理员", testPassword)
	if err != nil {
		t.Fatalf("创建首个管理员失败: %v", err)
	}
	if user.Username != "admin" {
		t.Fatalf("用户名应归一化为小写，实际 %q", user.Username)
	}
	if user.Role != RoleAdmin {
		t.Fatalf("角色 = %q，期望 admin", user.Role)
	}

	// 已存在账号时必须拒绝第二个“首个管理员”，否则任何能执行 CLI 的人都能重置管理员。
	if _, err := svc.CreateFirstAdmin(ctx, "second", "第二", testPassword); !errors.Is(err, ErrUsersExist) {
		t.Fatalf("已有账号时再建首个管理员应返回 ErrUsersExist，实际 %v", err)
	}
	if n, err := svc.CountUsers(ctx); err != nil || n != 1 {
		t.Fatalf("账号数 = %d，错误 = %v", n, err)
	}
}

func TestCreateUserRejectsDuplicatesAndBadInput(t *testing.T) {
	svc := testService(t, Options{})
	ctx := context.Background()
	mustCreateAdmin(t, svc, "admin")

	cases := []struct {
		name  string
		input CreateUserInput
	}{
		{"重复用户名", CreateUserInput{Username: "admin", Password: testPassword, Role: RoleViewer}},
		{"重复用户名（大小写不同）", CreateUserInput{Username: "ADMIN", Password: testPassword, Role: RoleViewer}},
		{"用户名过短", CreateUserInput{Username: "ab", Password: testPassword, Role: RoleViewer}},
		{"用户名以数字开头", CreateUserInput{Username: "1admin", Password: testPassword, Role: RoleViewer}},
		{"用户名含非法字符", CreateUserInput{Username: "ad min", Password: testPassword, Role: RoleViewer}},
		{"密码与用户名相同", CreateUserInput{Username: "sameuser", Password: "sameuser", Role: RoleViewer}},
		{"密码太短", CreateUserInput{Username: "shorty", Password: "abc123", Role: RoleViewer}},
		{"角色非法", CreateUserInput{Username: "badrole", Password: testPassword, Role: Role("root")}},
		{"显示名过长", CreateUserInput{Username: "longname", DisplayName: strings.Repeat("名", MaxDisplayNameLen+1), Password: testPassword, Role: RoleViewer}},
	}
	for _, c := range cases {
		if _, err := svc.CreateUser(ctx, c.input); err == nil {
			t.Fatalf("%s: 期望报错，实际创建成功", c.name)
		}
	}

	if n, err := svc.CountUsers(ctx); err != nil || n != 1 {
		t.Fatalf("失败创建不应留下记录，账号数 = %d（错误 %v）", n, err)
	}
}

func TestCreateUserRejectsDuplicateAtDatabaseLevel(t *testing.T) {
	svc := testService(t, Options{})
	ctx := context.Background()
	mustCreateAdmin(t, svc, "admin")

	// 直接绕过 ValidateUsername 写库，验证唯一约束真的存在（而不是只靠 Go 侧检查）。
	_, err := svc.db.SQL().ExecContext(ctx, `
		INSERT INTO users (username, password_hash, role, disabled, version, created_at, updated_at)
		VALUES ('ADMIN', 'x', 'viewer', 0, 1, '2026-10-20T00:00:00Z', '2026-10-20T00:00:00Z')`)
	if err == nil {
		t.Fatal("users.username 必须有大小写不敏感的唯一约束")
	}
}

func TestUsernameValidation(t *testing.T) {
	valid := []string{"admin", "a_b", "user-1", "user.name", "abc", "a1234567890123456789012345678901"}
	for _, name := range valid {
		if err := ValidateUsername(name); err != nil {
			t.Fatalf("%q 应为合法用户名: %v", name, err)
		}
	}
	invalid := []string{"", "ab", "Admin", " admin", "admin ", "adm in", "1abc", "_abc", "-abc",
		"a12345678901234567890123456789012", "名字", "admin\x00", "admin;drop"}
	for _, name := range invalid {
		if err := ValidateUsername(name); err == nil {
			t.Fatalf("%q 应为非法用户名", name)
		}
	}
}

func TestNormalizeUsername(t *testing.T) {
	cases := map[string]string{
		"  Admin ": "admin",
		"ADMIN":    "admin",
		"admin":    "admin",
	}
	for in, want := range cases {
		if got := NormalizeUsername(in); got != want {
			t.Fatalf("NormalizeUsername(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func TestCredentialsStoreHashNotPassword(t *testing.T) {
	svc := testService(t, Options{})
	ctx := context.Background()
	mustCreateAdmin(t, svc, "admin")

	creds, err := svc.credentialsByUsername(ctx, "admin")
	if err != nil {
		t.Fatalf("读取凭据失败: %v", err)
	}
	if creds.hash == testPassword {
		t.Fatal("库里不能保存明文密码")
	}
	if !hasArgon2idPrefix(creds.hash) {
		t.Fatalf("保存的应是 Argon2id PHC 哈希，实际 %q", creds.hash)
	}

	// SQL 注入形态的用户名按普通值处理：查不到账号，而不是命中整表。
	if _, err := svc.credentialsByUsername(ctx, "' OR 1=1 --"); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("注入形态用户名应返回 ErrUserNotFound，实际 %v", err)
	}
}

func hasArgon2idPrefix(s string) bool {
	return len(s) > 10 && s[:10] == "$argon2id$"
}
