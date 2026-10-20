package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// createViewer 建一个 viewer 账号。
func createViewer(t *testing.T, svc *Service, username string) User {
	t.Helper()
	user, err := svc.CreateUser(context.Background(), CreateUserInput{
		Username:    username,
		DisplayName: username,
		Password:    testPassword,
		Role:        RoleViewer,
	})
	if err != nil {
		t.Fatalf("创建 viewer 失败: %v", err)
	}
	return user
}

func TestListUsersAndGet(t *testing.T) {
	svc := testService(t, Options{})
	ctx := context.Background()

	mustCreateAdmin(t, svc, "admin")
	viewer := createViewer(t, svc, "alice")

	users, next, err := svc.ListUsers(ctx, 0, "")
	if err != nil {
		t.Fatalf("ListUsers 失败: %v", err)
	}
	if len(users) != 2 {
		t.Fatalf("账号数 = %d，期望 2", len(users))
	}
	if users[0].Username != "admin" || users[1].Username != "alice" {
		t.Fatalf("按用户名排序失败: %s, %s", users[0].Username, users[1].Username)
	}
	if next != "" {
		t.Fatalf("只有两个账号时不应有下一页，next = %q", next)
	}

	got, err := svc.GetUser(ctx, viewer.ID)
	if err != nil {
		t.Fatalf("GetUser 失败: %v", err)
	}
	if got.Username != "alice" || got.Role != RoleViewer || got.Disabled {
		t.Fatalf("GetUser 结果不对: %+v", got)
	}
	if _, err := svc.GetUser(ctx, 9999); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("查询不存在账号应返回 ErrUserNotFound，实际 %v", err)
	}
}

func TestUpdateUserBumpsVersion(t *testing.T) {
	svc := testService(t, Options{})
	ctx := context.Background()
	admin := mustCreateAdmin(t, svc, "admin")
	viewer := createViewer(t, svc, "alice")

	name := "爱丽丝"
	updated, err := svc.UpdateUser(ctx, UpdateUserInput{
		ActorID:     admin.ID,
		UserID:      viewer.ID,
		Version:     viewer.Version,
		DisplayName: &name,
	})
	if err != nil {
		t.Fatalf("UpdateUser 失败: %v", err)
	}
	if updated.DisplayName != name {
		t.Fatalf("显示名 = %q，期望 %q", updated.DisplayName, name)
	}
	if updated.Version != viewer.Version+1 {
		t.Fatalf("version = %d，期望 %d", updated.Version, viewer.Version+1)
	}

	// 再读一遍，确认真的落库而不是只改了返回值。
	got, err := svc.GetUser(ctx, viewer.ID)
	if err != nil {
		t.Fatalf("GetUser 失败: %v", err)
	}
	if got.DisplayName != name || got.Version != updated.Version {
		t.Fatalf("库里的值与返回值不一致: %+v", got)
	}

	// 过期 version 必须冲突，且不产生部分修改。
	stale := "旧名字"
	if _, err := svc.UpdateUser(ctx, UpdateUserInput{
		ActorID:     admin.ID,
		UserID:      viewer.ID,
		Version:     viewer.Version,
		DisplayName: &stale,
	}); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("过期 version 应返回 ErrVersionConflict，实际 %v", err)
	}
	if got, _ = svc.GetUser(ctx, viewer.ID); got.DisplayName != name {
		t.Fatalf("冲突的写入不应生效，实际显示名 %q", got.DisplayName)
	}
}

func TestUpdateUserRejectsSelfRoleChange(t *testing.T) {
	svc := testService(t, Options{})
	ctx := context.Background()
	admin := mustCreateAdmin(t, svc, "admin")
	mustCreateAdmin(t, svc, "second")

	viewer := RoleViewer
	if _, err := svc.UpdateUser(ctx, UpdateUserInput{
		ActorID: admin.ID,
		UserID:  admin.ID,
		Version: admin.Version,
		Role:    &viewer,
	}); !errors.Is(err, ErrSelfModification) {
		t.Fatalf("自我降级应返回 ErrSelfModification，实际 %v", err)
	}

	disabled := true
	if _, err := svc.UpdateUser(ctx, UpdateUserInput{
		ActorID:  admin.ID,
		UserID:   admin.ID,
		Version:  admin.Version,
		Disabled: &disabled,
	}); !errors.Is(err, ErrSelfModification) {
		t.Fatalf("自我禁用应返回 ErrSelfModification，实际 %v", err)
	}

	// 改自己的显示名是允许的：这不是权限变更。
	name := "主机管理员"
	if _, err := svc.UpdateUser(ctx, UpdateUserInput{
		ActorID:     admin.ID,
		UserID:      admin.ID,
		Version:     admin.Version,
		DisplayName: &name,
	}); err != nil {
		t.Fatalf("改自己的显示名应成功，实际 %v", err)
	}
}

func TestLastAdminCannotBeRemoved(t *testing.T) {
	svc := testService(t, Options{})
	ctx := context.Background()
	admin := mustCreateAdmin(t, svc, "admin")
	bob := createViewer(t, svc, "bob")
	viewerRole := RoleViewer
	disabled := true

	// 唯一管理员：降级、禁用、删除都必须拒绝。
	if _, err := svc.UpdateUser(ctx, UpdateUserInput{
		ActorID: admin.ID, UserID: admin.ID, Version: admin.Version, Role: &viewerRole,
	}); !errors.Is(err, ErrSelfModification) && !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("唯一管理员自我降级必须被拒绝，实际 %v", err)
	}
	if err := svc.DeleteUser(ctx, DeleteUserInput{
		ActorID: bob.ID, UserID: admin.ID, Version: admin.Version,
	}); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("删除唯一管理员应返回 ErrLastAdmin，实际 %v", err)
	}

	// 有第二个管理员时，禁用第一个是允许的。
	second := mustCreateAdmin(t, svc, "second")
	if _, err := svc.UpdateUser(ctx, UpdateUserInput{
		ActorID: second.ID, UserID: admin.ID, Version: admin.Version, Disabled: &disabled,
	}); err != nil {
		t.Fatalf("有备用管理员时禁用应成功，实际 %v", err)
	}

	// 现在 second 是唯一可用管理员：再禁用就超限。
	if _, err := svc.UpdateUser(ctx, UpdateUserInput{
		ActorID: admin.ID, UserID: second.ID, Version: second.Version, Disabled: &disabled,
	}); !errors.Is(err, ErrLastAdmin) && !errors.Is(err, ErrUserDisabled) {
		t.Fatalf("禁用最后一个可用管理员应返回 ErrLastAdmin，实际 %v", err)
	}
}

func TestUpdateUserDisablingRevokesSessions(t *testing.T) {
	svc := testService(t, Options{})
	ctx := context.Background()
	admin := mustCreateAdmin(t, svc, "admin")
	viewer := createViewer(t, svc, "alice")

	issued, err := svc.Login(ctx, LoginInput{
		Username: "alice", Password: testPassword, RemoteAddr: "192.0.2.20",
	})
	if err != nil {
		t.Fatalf("viewer 登录失败: %v", err)
	}
	if _, err := svc.Authenticate(ctx, issued.Token); err != nil {
		t.Fatalf("登录后的会话应有效: %v", err)
	}

	disabled := true
	updated, err := svc.UpdateUser(ctx, UpdateUserInput{
		ActorID: admin.ID, UserID: viewer.ID, Version: viewer.Version, Disabled: &disabled,
	})
	if err != nil {
		t.Fatalf("禁用账号失败: %v", err)
	}
	if !updated.Disabled {
		t.Fatal("返回的账号应为禁用状态")
	}
	if _, err := svc.Authenticate(ctx, issued.Token); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("禁用后旧会话必须失效，实际 %v", err)
	}
	if _, err := svc.Login(ctx, LoginInput{
		Username: "alice", Password: testPassword, RemoteAddr: "192.0.2.21",
	}); !errors.Is(err, ErrUserDisabled) {
		t.Fatalf("禁用账号登录应返回 ErrUserDisabled，实际 %v", err)
	}
}

func TestSetPasswordRevokesSessions(t *testing.T) {
	svc := testService(t, Options{})
	ctx := context.Background()
	viewer := createViewer(t, svc, "alice")

	issued, err := svc.Login(ctx, LoginInput{
		Username: "alice", Password: testPassword, RemoteAddr: "192.0.2.30",
	})
	if err != nil {
		t.Fatalf("登录失败: %v", err)
	}

	newPassword := "another-horse-7"
	updated, err := svc.SetPassword(ctx, viewer.ID, newPassword)
	if err != nil {
		t.Fatalf("重置密码失败: %v", err)
	}
	if updated.Version != viewer.Version+1 {
		t.Fatalf("重置密码后 version = %d，期望 %d", updated.Version, viewer.Version+1)
	}

	if _, err := svc.Authenticate(ctx, issued.Token); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("改密码后旧会话必须失效，实际 %v", err)
	}
	if _, err := svc.Login(ctx, LoginInput{
		Username: "alice", Password: testPassword, RemoteAddr: "192.0.2.31",
	}); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("旧密码登录应失败，实际 %v", err)
	}
	if _, err := svc.Login(ctx, LoginInput{
		Username: "alice", Password: newPassword, RemoteAddr: "192.0.2.32",
	}); err != nil {
		t.Fatalf("新密码登录应成功，实际 %v", err)
	}
}

func TestSetPasswordEnforcesPolicy(t *testing.T) {
	svc := testService(t, Options{})
	ctx := context.Background()
	viewer := createViewer(t, svc, "alice")

	err := func() error {
		_, err := svc.SetPassword(ctx, viewer.ID, "short")
		return err
	}()
	var input *InputError
	if !errors.As(err, &input) {
		t.Fatalf("弱密码应返回 *InputError，实际 %v", err)
	}

	if _, err := svc.SetPassword(ctx, 9999, "another-horse-7"); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("不存在的账号应返回 ErrUserNotFound，实际 %v", err)
	}
}

func TestDeleteUser(t *testing.T) {
	svc := testService(t, Options{})
	ctx := context.Background()
	admin := mustCreateAdmin(t, svc, "admin")
	viewer := createViewer(t, svc, "alice")

	issued, err := svc.Login(ctx, LoginInput{
		Username: "alice", Password: testPassword, RemoteAddr: "192.0.2.40",
	})
	if err != nil {
		t.Fatalf("登录失败: %v", err)
	}

	if err := svc.DeleteUser(ctx, DeleteUserInput{
		ActorID: admin.ID, UserID: viewer.ID, Version: viewer.Version + 5,
	}); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("版本不符应返回 ErrVersionConflict，实际 %v", err)
	}

	if err := svc.DeleteUser(ctx, DeleteUserInput{
		ActorID: admin.ID, UserID: viewer.ID, Version: viewer.Version,
	}); err != nil {
		t.Fatalf("删除账号失败: %v", err)
	}

	if _, err := svc.GetUser(ctx, viewer.ID); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("删除后查询应返回 ErrUserNotFound，实际 %v", err)
	}
	// 外键 ON DELETE CASCADE：会话随账号一起消失。
	if _, err := svc.Authenticate(ctx, issued.Token); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("删除账号后会话必须失效，实际 %v", err)
	}

	// 不能删除自己。
	if err := svc.DeleteUser(ctx, DeleteUserInput{
		ActorID: admin.ID, UserID: admin.ID, Version: admin.Version,
	}); !errors.Is(err, ErrSelfModification) {
		t.Fatalf("删除自己应返回 ErrSelfModification，实际 %v", err)
	}
}

func TestListUsersPagination(t *testing.T) {
	svc := testService(t, Options{})
	ctx := context.Background()

	mustCreateAdmin(t, svc, "admin")
	for _, name := range []string{"alice", "bob", "carol", "dave"} {
		createViewer(t, svc, name)
	}

	// 第一页：两个，游标非空。
	page1, cursor, err := svc.ListUsers(ctx, 2, "")
	if err != nil {
		t.Fatalf("第一页失败: %v", err)
	}
	if len(page1) != 2 || page1[0].Username != "admin" || page1[1].Username != "alice" {
		t.Fatalf("第一页 = %+v", page1)
	}
	if cursor == "" {
		t.Fatal("还有下一页时必须返回游标")
	}
	// 游标不透明：不含明文用户名，客户端不得解析。
	if strings.Contains(cursor, "alice") {
		t.Fatalf("游标泄露了明文用户名: %q", cursor)
	}

	// 第二页继续往下，不重复上一页。
	page2, cursor2, err := svc.ListUsers(ctx, 2, cursor)
	if err != nil {
		t.Fatalf("第二页失败: %v", err)
	}
	if len(page2) != 2 || page2[0].Username != "bob" || page2[1].Username != "carol" {
		t.Fatalf("第二页 = %+v", page2)
	}
	if cursor2 == "" {
		t.Fatal("第二页之后仍有数据，游标不能为空")
	}

	// 最后一页：next 为空。
	page3, cursor3, err := svc.ListUsers(ctx, 2, cursor2)
	if err != nil {
		t.Fatalf("第三页失败: %v", err)
	}
	if len(page3) != 1 || page3[0].Username != "dave" {
		t.Fatalf("第三页 = %+v", page3)
	}
	if cursor3 != "" {
		t.Fatalf("最后一页不应有游标，实际 %q", cursor3)
	}

	// 非法入参。
	if _, _, err := svc.ListUsers(ctx, -1, ""); !errors.Is(err, ErrInvalidLimit) {
		t.Fatalf("负 limit 应返回 ErrInvalidLimit，实际 %v", err)
	}
	if _, _, err := svc.ListUsers(ctx, MaxUserListLimit+1, ""); !errors.Is(err, ErrInvalidLimit) {
		t.Fatalf("超限 limit 应返回 ErrInvalidLimit，实际 %v", err)
	}
	if _, _, err := svc.ListUsers(ctx, 0, "not-a-cursor!"); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("坏游标应返回 ErrInvalidCursor，实际 %v", err)
	}
	if _, _, err := svc.ListUsers(ctx, 0, "AAAA"); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("缺少前缀的游标应返回 ErrInvalidCursor，实际 %v", err)
	}
}
