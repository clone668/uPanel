package auth

// 定时触发的保留账号（P12.4）。
//
// 那一行在 users 表里存在，只因为 jobs.user_id 是 NOT NULL 外键——代理自己产生的
// 定时操作要有一个"操作者"，而它不可能是任何真实管理员。这里断言的是它的边界：
// 对账号管理**不存在**、登录等于"没有这个账号"、也不会被算进"系统里有没有人"。

import (
	"context"
	"errors"
	"testing"

	"upanel/internal/schedules"
)

// insertSystemUser 插入一条与导入路径一致的保留账号。
func insertSystemUser(t *testing.T, svc *Service) {
	t.Helper()
	if _, err := svc.db.SQL().ExecContext(context.Background(), `
		INSERT INTO users (id, username, display_name, password_hash, role, disabled, version, created_at, updated_at)
		VALUES (?, 'scheduler', '定时任务', '!system-account-no-password', 'viewer', 1, 1,
		        '2026-10-23T19:30:00Z', '2026-10-23T19:30:00Z')`,
		schedules.SystemUserID); err != nil {
		t.Fatalf("插入保留账号失败: %v", err)
	}
}

// TestSystemAccountHiddenFromManagement：列表里没有、按 id 查是"没有这个账号"、改和删一样。
func TestSystemAccountHiddenFromManagement(t *testing.T) {
	svc := testService(t, Options{})
	ctx := context.Background()
	admin := mustCreateAdmin(t, svc, "admin")
	insertSystemUser(t, svc)

	users, _, err := svc.ListUsers(ctx, 0, "")
	if err != nil {
		t.Fatalf("ListUsers 失败: %v", err)
	}
	for _, u := range users {
		if u.ID == schedules.SystemUserID {
			t.Fatalf("保留账号不该出现在账号列表里：%+v", u)
		}
	}
	if len(users) != 1 {
		t.Fatalf("账号数 = %d，期望 1（只有 admin）", len(users))
	}

	if _, err := svc.GetUser(ctx, schedules.SystemUserID); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("按 id 查保留账号应当返回 ErrUserNotFound，实际 %v", err)
	}

	name := "改个名"
	if _, err := svc.UpdateUser(ctx, UpdateUserInput{
		ActorID: admin.ID, UserID: schedules.SystemUserID, Version: 1, DisplayName: &name,
	}); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("修改保留账号应当返回 ErrUserNotFound，实际 %v", err)
	}

	if err := svc.DeleteUser(ctx, DeleteUserInput{
		ActorID: admin.ID, UserID: schedules.SystemUserID, Version: 1,
	}); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("删除保留账号应当返回 ErrUserNotFound，实际 %v", err)
	}

	// 它还在：上面两次写操作在"看起来成功"和"真的删了"之间，只有后者是不可逆的。
	var n int
	if err := svc.db.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE id = ?`,
		schedules.SystemUserID).Scan(&n); err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("保留账号被删掉了（行数 = %d）", n)
	}
}

// TestSystemAccountLoginLooksLikeMissingAccount：登录等于"没有这个账号"。
func TestSystemAccountLoginLooksLikeMissingAccount(t *testing.T) {
	svc := testService(t, Options{})
	insertSystemUser(t, svc)

	_, err := svc.Login(context.Background(), LoginInput{
		Username:   "scheduler",
		Password:   "whatever-9",
		RemoteAddr: "192.0.2.10:5000",
	})
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("保留账号登录应当返回 ErrInvalidCredentials，实际 %v", err)
	}
}

// TestSystemAccountNotCountedAsUser：它不算"系统里有人"，首个管理员照样能建。
func TestSystemAccountNotCountedAsUser(t *testing.T) {
	svc := testService(t, Options{})
	ctx := context.Background()
	insertSystemUser(t, svc)

	n, err := svc.CountUsers(ctx)
	if err != nil {
		t.Fatalf("CountUsers 失败: %v", err)
	}
	if n != 0 {
		t.Fatalf("账号数 = %d，期望 0（保留账号不算人）", n)
	}

	if _, err := svc.CreateFirstAdmin(ctx, "admin", "管理员", testPassword); err != nil {
		t.Fatalf("首个管理员应当能创建: %v", err)
	}
	users, _, err := svc.ListUsers(ctx, 0, "")
	if err != nil {
		t.Fatalf("ListUsers 失败: %v", err)
	}
	if len(users) != 1 || users[0].Username != "admin" {
		t.Fatalf("账号列表不对：%+v", users)
	}
}
