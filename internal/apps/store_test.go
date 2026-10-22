package apps

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"upanel/internal/storage"
	"upanel/internal/store"
	"upanel/migrations"
)

// quietLogger 让迁移日志不污染测试输出。
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// fixedNow 是测试用的固定时钟：时间字段可断言，又不依赖真实时间。
func fixedNow() time.Time { return time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC) }

// newTestService 构造跑在临时目录里的真实 SQLite 服务。
//
// 不用内存假实现：部分唯一索引、外键 RESTRICT/CASCADE、乐观锁的“零行即冲突”
// 都只有真实数据库才能验证。
func newTestService(t *testing.T, opts Options) (*Service, *store.DB) {
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
	if opts.Now == nil {
		opts.Now = fixedNow
	}
	svc, err := New(db, opts)
	if err != nil {
		t.Fatalf("构造服务失败: %v", err)
	}
	return svc, db
}

// seedUser 直接插入一行账号。
//
// 收藏表有指向 users 的外键，而这里不关心认证逻辑，所以不走 auth 服务（Argon2
// 计算对存储层用例没有意义）。密码哈希列不可为空，填一个固定占位串。
func seedUser(t *testing.T, db *store.DB, username string) int64 {
	t.Helper()
	ts := formatTime(fixedNow())
	res, err := db.ExecContext(context.Background(), `
		INSERT INTO users (username, display_name, password_hash, role, disabled, version, created_at, updated_at)
		VALUES (?, ?, 'placeholder', 'admin', 0, 1, ?, ?)`, username, username, ts, ts)
	if err != nil {
		t.Fatalf("插入测试账号失败: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("读取账号 id 失败: %v", err)
	}
	return id
}

// volumeLister 是固定卷列表。
type volumeLister struct {
	volumes []storage.Volume
	err     error
}

func (v volumeLister) Volumes(context.Context) ([]storage.Volume, error) {
	if v.err != nil {
		return nil, v.err
	}
	return v.volumes, nil
}

// TestCreateGetAndList 覆盖最基本的写读路径与默认值。
func TestCreateGetAndList(t *testing.T) {
	svc, _ := newTestService(t, Options{})
	ctx := context.Background()

	created, err := svc.Create(ctx, Definition{
		Name:        "面板",
		Description: "管理面板",
		IconKey:     "monitor",
		Type:        TypeLink,
		PublicURL:   "https://panel.example.com",
	})
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	if !validAppID(created.ID) {
		t.Fatalf("生成的标识不是 app_ + 32 位十六进制: %q", created.ID)
	}
	if created.Version != 1 {
		t.Fatalf("初始版本 = %d", created.Version)
	}
	if created.CreatedAt.IsZero() || !created.CreatedAt.Equal(fixedNow()) {
		t.Fatalf("创建时间 = %v，期望固定时钟", created.CreatedAt)
	}

	got, err := svc.Get(ctx, 0, created.ID)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if got.Name != "面板" || got.PublicURL != "https://panel.example.com" || got.IconKey != "monitor" {
		t.Fatalf("读回的记录 = %+v", got)
	}
	if got.GroupID != "" {
		t.Fatalf("未分组的分组标识应为空，实际 %q", got.GroupID)
	}
	if len(got.VolumeDependencies) != 0 {
		t.Fatalf("没有依赖卷时列表应为空，实际 %v", got.VolumeDependencies)
	}

	items, next, err := svc.List(ctx, ListOptions{})
	if err != nil {
		t.Fatalf("列表失败: %v", err)
	}
	if len(items) != 1 || items[0].ID != created.ID || next != "" {
		t.Fatalf("列表 = %+v，next = %q", items, next)
	}

	if _, err := svc.Get(ctx, 0, "app_00000000000000000000000000000000"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的应用应返回 ErrNotFound，实际 %v", err)
	}
	if _, err := svc.Get(ctx, 0, "not-an-id"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("形状不对的标识应返回 ErrNotFound，实际 %v", err)
	}
}

// TestBindingCanonicalJSONInDatabase 确认落库的是规范 JSON（唯一索引依赖这一点）。
func TestBindingCanonicalJSONInDatabase(t *testing.T) {
	svc, db := newTestService(t, Options{})
	ctx := context.Background()

	app, err := svc.Create(ctx, Definition{
		Name:    "网盘",
		Type:    TypeDocker,
		Binding: Binding{ContainerID: testContainerID},
	})
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	var binding, health, typ string
	if err := db.SQL().QueryRowContext(ctx,
		`SELECT binding_json, health_json, type FROM apps WHERE id = ?`, app.ID).
		Scan(&binding, &health, &typ); err != nil {
		t.Fatalf("读取原始列失败: %v", err)
	}
	want := `{"container_id":"` + testContainerID + `"}`
	if binding != want {
		t.Fatalf("binding_json = %s，期望 %s", binding, want)
	}
	if typ != "docker" {
		t.Fatalf("type = %q", typ)
	}
	// 健康检查即使关闭也必须是完整对象：字段集合固定，读回来才不需要猜缺了哪个键。
	if health == "" || health == "{}" {
		t.Fatalf("health_json = %q", health)
	}
}

// TestBindingCannotBeSharedByTwoApps 确认“一个容器只属于一个应用”。
func TestBindingCannotBeSharedByTwoApps(t *testing.T) {
	svc, _ := newTestService(t, Options{})
	ctx := context.Background()

	if _, err := svc.Create(ctx, Definition{
		Name: "A", Type: TypeDocker, Binding: Binding{ContainerID: testContainerID},
	}); err != nil {
		t.Fatalf("第一次创建失败: %v", err)
	}
	_, err := svc.Create(ctx, Definition{
		Name: "B", Type: TypeDocker, Binding: Binding{ContainerID: testContainerID},
	})
	if !errors.Is(err, ErrBindingTaken) {
		t.Fatalf("重复绑定应返回 ErrBindingTaken，实际 %v", err)
	}

	// 两个 link 应用没有绑定对象，必须都能建（唯一索引排除了 link）。
	for _, name := range []string{"链接一", "链接二"} {
		if _, err := svc.Create(ctx, Definition{
			Name: name, Type: TypeLink, PublicURL: "https://example.com",
		}); err != nil {
			t.Fatalf("创建 link 应用 %s 失败: %v", name, err)
		}
	}
}

// TestUpdateVersionConflictAndTypeChange 覆盖乐观锁与“改类型必须同时改绑定”。
func TestUpdateVersionConflictAndTypeChange(t *testing.T) {
	svc, _ := newTestService(t, Options{})
	ctx := context.Background()

	app, err := svc.Create(ctx, Definition{
		Name: "服务", Type: TypeSystemd, Binding: Binding{Unit: "nginx.service"},
	})
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}

	name := "反向代理"
	updated, err := svc.Update(ctx, UpdateInput{
		ID: app.ID, Version: 1, Patch: Patch{Name: &name},
	})
	if err != nil {
		t.Fatalf("更新失败: %v", err)
	}
	if updated.Version != 2 || updated.Name != "反向代理" {
		t.Fatalf("更新结果 = %+v", updated)
	}
	// 类型与绑定没动，必须原样保留。
	if updated.Type != TypeSystemd || updated.Binding.Unit != "nginx.service" {
		t.Fatalf("绑定被意外改动: %+v", updated.Binding)
	}

	if _, err := svc.Update(ctx, UpdateInput{
		ID: app.ID, Version: 1, Patch: Patch{Name: &name},
	}); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("旧版本应返回 ErrVersionConflict，实际 %v", err)
	}

	// 只改类型、不给绑定：必须拒绝，否则会留下“类型说 docker、绑定是单元名”的错位记录。
	newType := TypeDocker
	_, err = svc.Update(ctx, UpdateInput{ID: app.ID, Version: 2, Patch: Patch{Type: &newType}})
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("只改类型应返回 *ValidationError，实际 %v", err)
	}
	if ve.Field != "binding" {
		t.Fatalf("错误字段 = %q，期望 binding", ve.Field)
	}

	// 一起给：通过，且旧单元名被换成容器 ID。
	binding := Binding{ContainerID: testContainerID}
	updated, err = svc.Update(ctx, UpdateInput{
		ID: app.ID, Version: 2, Patch: Patch{Type: &newType, Binding: &binding},
	})
	if err != nil {
		t.Fatalf("改类型+绑定失败: %v", err)
	}
	if updated.Type != TypeDocker || updated.Binding.ContainerID != testContainerID || updated.Binding.Unit != "" {
		t.Fatalf("改类型后的记录 = %+v", updated)
	}
}

// TestUpdateCanClearGroupWithExplicitNull 覆盖 PATCH 里“显式 null = 移出分组”。
func TestUpdateCanClearGroupWithExplicitNull(t *testing.T) {
	svc, _ := newTestService(t, Options{})
	ctx := context.Background()

	group, err := svc.CreateGroup(ctx, GroupInput{Name: "运维"})
	if err != nil {
		t.Fatalf("建分组失败: %v", err)
	}
	app, err := svc.Create(ctx, Definition{
		Name: "面板", Type: TypeLink, PublicURL: "https://panel.example.com", GroupID: group.ID,
	})
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	if app.GroupID != group.ID {
		t.Fatalf("分组 = %q", app.GroupID)
	}

	updated, err := svc.Update(ctx, UpdateInput{
		ID: app.ID, Version: 1,
		Patch: Patch{GroupID: Optional[string]{Set: true, Value: ""}},
	})
	if err != nil {
		t.Fatalf("移出分组失败: %v", err)
	}
	if updated.GroupID != "" {
		t.Fatalf("移出分组后 GroupID = %q", updated.GroupID)
	}
	var count int
	if err := svc.queryRow(ctx,
		`SELECT COUNT(*) FROM app_groups WHERE id = ?`, group.ID).Scan(&count); err != nil {
		t.Fatalf("查询分组失败: %v", err)
	}
	if count != 1 {
		t.Fatal("移出分组不应删除分组本身")
	}
}

// TestDeleteOnlyRemovesMetadata 覆盖删除语义与版本校验。
func TestDeleteOnlyRemovesMetadata(t *testing.T) {
	svc, db := newTestService(t, Options{})
	ctx := context.Background()

	app, err := svc.Create(ctx, Definition{
		Name: "面板", Type: TypeLink, PublicURL: "https://panel.example.com",
	})
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	if err := svc.Delete(ctx, app.ID, 2); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("版本不符应返回 ErrVersionConflict，实际 %v", err)
	}
	if err := svc.Delete(ctx, app.ID, 1); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if _, err := svc.Get(ctx, 0, app.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("删除后应 ErrNotFound，实际 %v", err)
	}
	if err := svc.Delete(ctx, app.ID, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("重复删除应 ErrNotFound，实际 %v", err)
	}
	var n int
	if err := db.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM apps`).Scan(&n); err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if n != 0 {
		t.Fatalf("apps 表应已空，实际 %d 行", n)
	}
}

// TestGroupLifecycle 覆盖分组的增查改删、重名与“分组非空不能删”。
func TestGroupLifecycle(t *testing.T) {
	svc, _ := newTestService(t, Options{})
	ctx := context.Background()

	first, err := svc.CreateGroup(ctx, GroupInput{Name: "运维"})
	if err != nil {
		t.Fatalf("建分组失败: %v", err)
	}
	if first.SortOrder != 0 || first.Version != 1 {
		t.Fatalf("首个分组 = %+v", first)
	}
	second, err := svc.CreateGroup(ctx, GroupInput{Name: "开发"})
	if err != nil {
		t.Fatalf("建分组失败: %v", err)
	}
	// 未指定排序值时应排到最后。
	if second.SortOrder != first.SortOrder+1 {
		t.Fatalf("第二个分组 sort_order = %d，期望 %d", second.SortOrder, first.SortOrder+1)
	}

	if _, err := svc.CreateGroup(ctx, GroupInput{Name: "运维"}); !errors.Is(err, ErrGroupNameTaken) {
		t.Fatalf("重名应返回 ErrGroupNameTaken，实际 %v", err)
	}
	if _, err := svc.CreateGroup(ctx, GroupInput{Name: "  "}); err == nil {
		t.Fatal("空分组名必须拒绝")
	}

	groups, next, err := svc.ListGroups(ctx, 0, "")
	if err != nil {
		t.Fatalf("分组列表失败: %v", err)
	}
	if len(groups) != 2 || groups[0].ID != first.ID || groups[1].ID != second.ID || next != "" {
		t.Fatalf("分组列表 = %+v，next = %q", groups, next)
	}

	newName := "基础设施"
	updated, err := svc.UpdateGroup(ctx, first.ID, 1, &newName, nil)
	if err != nil {
		t.Fatalf("改分组名失败: %v", err)
	}
	if updated.Name != "基础设施" || updated.Version != 2 {
		t.Fatalf("改后的分组 = %+v", updated)
	}
	if _, err := svc.UpdateGroup(ctx, first.ID, 1, &newName, nil); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("旧版本应 ErrVersionConflict，实际 %v", err)
	}
	if _, err := svc.UpdateGroup(ctx, second.ID, 1, &newName, nil); !errors.Is(err, ErrGroupNameTaken) {
		t.Fatalf("改成已有名字应 ErrGroupNameTaken，实际 %v", err)
	}

	// 分组里还有应用时删除必须被拒绝，且应用不会被顺手移出分组。
	app, err := svc.Create(ctx, Definition{
		Name: "面板", Type: TypeLink, PublicURL: "https://panel.example.com", GroupID: first.ID,
	})
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	if err := svc.DeleteGroup(ctx, first.ID, 2); !errors.Is(err, ErrGroupInUse) {
		t.Fatalf("非空分组删除应 ErrGroupInUse，实际 %v", err)
	}
	still, err := svc.Get(ctx, 0, app.ID)
	if err != nil {
		t.Fatalf("读取应用失败: %v", err)
	}
	if still.GroupID != first.ID {
		t.Fatalf("应用的分组被改动: %q", still.GroupID)
	}

	if err := svc.DeleteGroup(ctx, second.ID, 1); err != nil {
		t.Fatalf("删除空分组失败: %v", err)
	}
	if _, err := svc.GetGroup(ctx, second.ID); !errors.Is(err, ErrGroupNotFound) {
		t.Fatalf("删除后应 ErrGroupNotFound，实际 %v", err)
	}
}

// TestGroupMustExistOnCreate 确认引用不存在的分组被拒绝。
func TestGroupMustExistOnCreate(t *testing.T) {
	svc, _ := newTestService(t, Options{})
	ctx := context.Background()

	_, err := svc.Create(ctx, Definition{
		Name: "面板", Type: TypeLink, PublicURL: "https://panel.example.com",
		GroupID: "grp_00000000000000000000000000000000",
	})
	var missing *GroupMissingError
	if !errors.As(err, &missing) {
		t.Fatalf("应返回 *GroupMissingError，实际 %v", err)
	}
	if missing.GroupID != "grp_00000000000000000000000000000000" {
		t.Fatalf("GroupID = %q", missing.GroupID)
	}
}

// TestFavoritesArePerUser 确认收藏是“每个人自己的”，不是应用的属性。
func TestFavoritesArePerUser(t *testing.T) {
	svc, db := newTestService(t, Options{})
	ctx := context.Background()

	alice := seedUser(t, db, "alice")
	bob := seedUser(t, db, "bob")

	app, err := svc.Create(ctx, Definition{
		Name: "面板", Type: TypeLink, PublicURL: "https://panel.example.com",
	})
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	if err := svc.SetFavorite(ctx, alice, app.ID, true); err != nil {
		t.Fatalf("收藏失败: %v", err)
	}
	// 幂等：重复收藏不报错、不产生第二行。
	if err := svc.SetFavorite(ctx, alice, app.ID, true); err != nil {
		t.Fatalf("重复收藏失败: %v", err)
	}

	mine, err := svc.Get(ctx, alice, app.ID)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if !mine.Favorite {
		t.Fatal("alice 应看到已收藏")
	}
	other, err := svc.Get(ctx, bob, app.ID)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if other.Favorite {
		t.Fatal("bob 不应看到 alice 的收藏")
	}

	if err := svc.SetFavorite(ctx, alice, app.ID, false); err != nil {
		t.Fatalf("取消收藏失败: %v", err)
	}
	mine, err = svc.Get(ctx, alice, app.ID)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if mine.Favorite {
		t.Fatal("取消收藏后不应仍是收藏状态")
	}

	// 删除应用时收藏随之消失（从属对象 CASCADE），不留孤儿行。
	if err := svc.SetFavorite(ctx, bob, app.ID, true); err != nil {
		t.Fatalf("收藏失败: %v", err)
	}
	if err := svc.Delete(ctx, app.ID, 1); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	var n int
	if err := db.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM app_favorites`).Scan(&n); err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if n != 0 {
		t.Fatalf("收藏应随应用删除，实际还剩 %d 行", n)
	}
	if err := svc.SetFavorite(ctx, alice, "app_00000000000000000000000000000000", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("收藏不存在的应用应 ErrNotFound，实际 %v", err)
	}
}

// TestListPagination 确认游标翻页不重复、不跳行（排序键是 name + id）。
func TestListPagination(t *testing.T) {
	svc, _ := newTestService(t, Options{})
	ctx := context.Background()

	// 两个同名应用 + 一个不同名：同名的也必须能翻到，且顺序稳定。
	for _, name := range []string{"b-app", "a-app", "a-app"} {
		if _, err := svc.Create(ctx, Definition{
			Name: name, Type: TypeLink, PublicURL: "https://example.com",
		}); err != nil {
			t.Fatalf("创建 %s 失败: %v", name, err)
		}
	}

	seen := map[string]int{}
	cursor := ""
	pages := 0
	for {
		items, next, err := svc.List(ctx, ListOptions{Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatalf("列表失败: %v", err)
		}
		for _, it := range items {
			seen[it.ID]++
		}
		pages++
		if next == "" {
			break
		}
		cursor = next
		if pages > 5 {
			t.Fatal("翻页没有终止")
		}
	}
	if len(seen) != 3 {
		t.Fatalf("翻到 %d 条，期望 3 条", len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("应用 %s 出现了 %d 次", id, n)
		}
	}

	if _, _, err := svc.List(ctx, ListOptions{Cursor: "bm90LWEtY3Vyc29y"}); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("非法游标应 ErrInvalidCursor，实际 %v", err)
	}
	if _, _, err := svc.List(ctx, ListOptions{Limit: MaxListLimit + 1}); !errors.Is(err, ErrInvalidLimit) {
		t.Fatalf("超限应 ErrInvalidLimit，实际 %v", err)
	}
}

// TestListFiltersByGroup 确认 group_id 过滤。
func TestListFiltersByGroup(t *testing.T) {
	svc, _ := newTestService(t, Options{})
	ctx := context.Background()

	group, err := svc.CreateGroup(ctx, GroupInput{Name: "运维"})
	if err != nil {
		t.Fatalf("建分组失败: %v", err)
	}
	if _, err := svc.Create(ctx, Definition{
		Name: "组内", Type: TypeLink, PublicURL: "https://a.example.com", GroupID: group.ID,
	}); err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	if _, err := svc.Create(ctx, Definition{
		Name: "组外", Type: TypeLink, PublicURL: "https://b.example.com",
	}); err != nil {
		t.Fatalf("创建失败: %v", err)
	}

	items, _, err := svc.List(ctx, ListOptions{GroupID: group.ID})
	if err != nil {
		t.Fatalf("列表失败: %v", err)
	}
	if len(items) != 1 || items[0].Name != "组内" {
		t.Fatalf("按分组过滤结果 = %+v", items)
	}
}

// TestVolumeDependenciesNeedChecker 确认依赖卷的三种结局：无法确认、不存在、通过。
func TestVolumeDependenciesNeedChecker(t *testing.T) {
	ctx := context.Background()
	def := Definition{
		Name: "数据应用", Type: TypeSystemd, Binding: Binding{Unit: "app.service"},
		VolumeDependencies: []string{testVolumeID},
	}

	// 没有卷检查器（演示模式 / 未连代理）：明确报“无法确认”，不写库。
	svc, db := newTestService(t, Options{})
	if _, err := svc.Create(ctx, def); !errors.Is(err, ErrVolumeCheckUnavailable) {
		t.Fatalf("没有检查器应 ErrVolumeCheckUnavailable，实际 %v", err)
	}
	var n int
	if err := db.SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM apps`).Scan(&n); err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if n != 0 {
		t.Fatalf("确认失败时不应写库，实际 %d 行", n)
	}

	// 检查器报错（代理挂了）：同样是“无法确认”，而不是“卷不存在”。
	broken, _ := newTestService(t, Options{
		Volumes: NewVolumeChecker(volumeLister{err: errors.New("socket 不可用")}),
	})
	if _, err := broken.Create(ctx, def); !errors.Is(err, ErrVolumeCheckUnavailable) {
		t.Fatalf("检查器报错应 ErrVolumeCheckUnavailable，实际 %v", err)
	}

	// 卷不在代理列表里：这是“确实不存在”，是业务拒绝。
	wrong, _ := newTestService(t, Options{
		Volumes: NewVolumeChecker(volumeLister{volumes: []storage.Volume{{ID: "vol_ffffffffffffffffffffffffffffffff"}}}),
	})
	_, err := wrong.Create(ctx, def)
	var miss *VolumeMissingError
	if !errors.As(err, &miss) {
		t.Fatalf("应返回 *VolumeMissingError，实际 %v", err)
	}
	if miss.VolumeID != testVolumeID {
		t.Fatalf("VolumeID = %q", miss.VolumeID)
	}

	// 卷存在：通过，并且依赖列表按输入顺序读回来。
	ok, _ := newTestService(t, Options{
		Volumes: NewVolumeChecker(volumeLister{volumes: []storage.Volume{{ID: testVolumeID}}}),
	})
	created, err := ok.Create(ctx, def)
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	if len(created.VolumeDependencies) != 1 || created.VolumeDependencies[0] != testVolumeID {
		t.Fatalf("依赖卷 = %v", created.VolumeDependencies)
	}
	// 不提供 VolumeDependencies 的 PATCH 不应清空已登记的依赖。
	name := "改名"
	updated, err := ok.Update(ctx, UpdateInput{ID: created.ID, Version: 1, Patch: Patch{Name: &name}})
	if err != nil {
		t.Fatalf("更新失败: %v", err)
	}
	if len(updated.VolumeDependencies) != 1 {
		t.Fatalf("未提供依赖卷的更新不应改动依赖，实际 %v", updated.VolumeDependencies)
	}
	// 显式给空数组才是清空。
	empty := []string{}
	cleared, err := ok.Update(ctx, UpdateInput{ID: created.ID, Version: 2, Patch: Patch{VolumeDependencies: &empty}})
	if err != nil {
		t.Fatalf("清空依赖失败: %v", err)
	}
	if len(cleared.VolumeDependencies) != 0 {
		t.Fatalf("清空后仍剩 %v", cleared.VolumeDependencies)
	}
}

// TestHTTPStateEnums 确认 HTTP 摘要字段的诚实取值。
//
// 运行时状态、依赖保护与能力三项在 P09.4 之后由 internal/appruntime 产出
// （那里有真实的观测来源），本包只定义枚举形状，因此不再有它们的纯函数。
func TestHTTPStateEnums(t *testing.T) {
	if got := HTTPStateOf(HealthCheck{}); got != HTTPNotApplicable {
		t.Fatalf("未启用健康检查 HTTP 状态 = %q", got)
	}
	if got := HTTPStateOf(HealthCheck{Enabled: true}); got != HTTPUnknown {
		t.Fatalf("启用但未探测 HTTP 状态 = %q", got)
	}
}
