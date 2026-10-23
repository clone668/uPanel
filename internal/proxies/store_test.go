package proxies

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"upanel/internal/nginx"
	"upanel/internal/store"
	"upanel/migrations"
)

// quietLogger 让迁移日志不污染测试输出。
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// fixedNow 是测试用的固定时钟。
func fixedNow() time.Time { return time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC) }

// newTestService 构造跑在临时目录里的真实 SQLite 服务。
//
// 不用内存假实现：域名唯一索引、指向 apps 的外键、乐观锁的“零行即冲突”
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

// fakeApps 是可控的应用存在性来源。
type fakeApps struct {
	existing map[string]bool
	err      error
}

func (f *fakeApps) AppExists(_ context.Context, id string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return f.existing[id], nil
}

// freeFacts 返回“读得到、且没有任何声明”的 Nginx 现状。
func freeFacts() *fakeFacts {
	return &fakeFacts{facts: nginx.Facts{Available: true, Version: "1.24.0", UnitState: "active"}}
}

// seedApp 直接插入一行应用（外键需要）。
func seedApp(t *testing.T, db *store.DB, id string) {
	t.Helper()
	now := fixedNow().Format(time.RFC3339)
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO apps (id, name, description, icon_key, type, binding_json, public_url,
		                  health_json, version, created_at, updated_at)
		VALUES (?, '示例应用', '', '', 'link', '{}', '', '{}', 1, ?, ?)`,
		id, now, now); err != nil {
		t.Fatalf("插入测试应用失败: %v", err)
	}
}

// TestCreateAndGetRoundTrip 确认一份完整定义存进去再读出来是同一个东西。
func TestCreateAndGetRoundTrip(t *testing.T) {
	ctx := context.Background()
	const appID = "app_0123456789abcdef0123456789abcdef"
	svc, db := newTestService(t, Options{
		Facts:     freeFacts(),
		Apps:      &fakeApps{existing: map[string]bool{appID: true}},
		Normalize: NormalizeOptions{PanelPort: 9443},
	})
	seedApp(t, db, appID)

	def := Definition{
		AppID:              appID,
		Domain:             "食狮.example.com",
		UpstreamTargetID:   "cntr_9b1c2d",
		UpstreamScheme:     SchemeHTTPS,
		UpstreamPort:       8443,
		AccessMode:         AccessCIDR,
		AllowedCIDRs:       []string{"192.168.1.0/24", "10.0.0.0/8"},
		WebSocketEnabled:   true,
		MaxBodyBytes:       5 << 20,
		ReadTimeoutSeconds: 120,
		Enabled:            true,
	}
	created, err := svc.Create(ctx, def)
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	if created.ID == "" || created.Version != 1 || created.CreatedAt.IsZero() {
		t.Fatalf("创建结果不完整: %+v", created)
	}
	if created.Domain != "xn--85x722f.example.com" {
		t.Fatalf("域名应归一成 A-label，实际 %q", created.Domain)
	}

	got, err := svc.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if got.AppID != appID || got.UpstreamTargetID != "cntr_9b1c2d" || got.UpstreamScheme != SchemeHTTPS ||
		got.UpstreamPort != 8443 || got.AccessMode != AccessCIDR || !got.WebSocketEnabled ||
		got.MaxBodyBytes != 5<<20 || got.ReadTimeoutSeconds != 120 || !got.Enabled {
		t.Fatalf("读回来的定义不对: %+v", got.Definition)
	}
	if len(got.AllowedCIDRs) != 2 || got.AllowedCIDRs[0] != "10.0.0.0/8" {
		t.Fatalf("白名单应原样读回，实际 %v", got.AllowedCIDRs)
	}
	if len(got.BasicAuthUsers) != 0 {
		t.Fatalf("cidr 模式不该有账号，实际 %v", got.BasicAuthUsers)
	}
}

// TestCreateDuplicateDomain 确认同一个域名不能有两条记录。
//
// 两条记录会生成两个 server_name 相同的 server 块，nginx 只用第一个——
// 第二个存在、但从不生效，比直接报错难查得多。
func TestCreateDuplicateDomain(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestService(t, Options{Facts: freeFacts()})

	if _, err := svc.Create(ctx, validDefinition()); err != nil {
		t.Fatalf("第一次创建应该成功: %v", err)
	}
	_, err := svc.Create(ctx, validDefinition())
	if err == nil {
		t.Fatal("重复域名应该被拒绝")
	}
	var cerr *ConflictError
	if !errors.As(err, &cerr) || cerr.Report.State != ConflictTaken {
		t.Fatalf("应是带报告的冲突错误，实际 %v", err)
	}
}

// TestCreateRequiresFacts 确认读不到 Nginx 时拒绝写入。
func TestCreateRequiresFacts(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestService(t, Options{})
	_, err := svc.Create(ctx, validDefinition())
	if !errors.Is(err, ErrFactsUnavailable) {
		t.Fatalf("没有代理时应回 ErrFactsUnavailable，实际 %v", err)
	}

	// 读失败也一样：读不到不等于没有。
	svc2, _ := newTestService(t, Options{Facts: &fakeFacts{collectErr: errors.New("socket 关了")}})
	if _, err := svc2.Create(ctx, validDefinition()); !errors.Is(err, ErrFactsUnavailable) {
		t.Fatalf("读失败时应回 ErrFactsUnavailable，实际 %v", err)
	}
}

// TestCreateChecksApp 确认 app_id 必须指向真实存在的应用。
func TestCreateChecksApp(t *testing.T) {
	ctx := context.Background()
	const appID = "app_0123456789abcdef0123456789abcdef"
	svc, _ := newTestService(t, Options{Facts: freeFacts(), Apps: &fakeApps{existing: map[string]bool{}}})

	def := validDefinition()
	def.AppID = appID
	_, err := svc.Create(ctx, def)
	var verr *ValidationError
	if !errors.As(err, &verr) || verr.Field != "app_id" {
		t.Fatalf("应是 app_id 校验错误，实际 %v", err)
	}

	// 没有检查端时不能“先存下再说”。
	svc2, _ := newTestService(t, Options{Facts: freeFacts()})
	if _, err := svc2.Create(ctx, def); !errors.Is(err, ErrFactsUnavailable) {
		t.Fatalf("没有应用检查端时应拒绝写入，实际 %v", err)
	}
}

// TestCreateRejectsExistingNginxDomain 确认和主机上已有配置撞名时拒绝，并指出是谁占的。
func TestCreateRejectsExistingNginxDomain(t *testing.T) {
	svc, _ := newTestService(t, Options{
		Facts: &fakeFacts{facts: factsWith(map[string][]string{
			"/etc/nginx/conf.d/other.conf": {"app.example.com"},
		})},
	})
	_, err := svc.Create(context.Background(), validDefinition())
	var cerr *ConflictError
	if !errors.As(err, &cerr) {
		t.Fatalf("应是冲突错误，实际 %v", err)
	}
	if len(cerr.Report.Matches) != 1 || cerr.Report.Matches[0].File != "/etc/nginx/conf.d/other.conf" {
		t.Fatalf("报告应指出占用的文件，实际 %+v", cerr.Report)
	}
}

// TestUpdate 走一遍修改：版本递增、只改一个字段不动别的、域名改成被占用的要拒绝。
func TestUpdate(t *testing.T) {
	ctx := context.Background()
	facts := freeFacts()
	svc, _ := newTestService(t, Options{Facts: facts})

	site, err := svc.Create(ctx, validDefinition())
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}

	// 另一个站点占住另一个域名。
	other := validDefinition()
	other.Domain = "other.example.com"
	if _, err := svc.Create(ctx, other); err != nil {
		t.Fatalf("创建第二个站点失败: %v", err)
	}

	timeout := 300
	ws := true
	updated, err := svc.Update(ctx, UpdateInput{
		ID:      site.ID,
		Version: site.Version,
		Patch:   Patch{ReadTimeoutSeconds: &timeout, WebSocketEnabled: &ws},
	})
	if err != nil {
		t.Fatalf("修改失败: %v", err)
	}
	if updated.Version != 2 || updated.ReadTimeoutSeconds != 300 || !updated.WebSocketEnabled {
		t.Fatalf("修改结果不对: %+v", updated)
	}
	if updated.Domain != site.Domain || updated.UpstreamPort != site.UpstreamPort {
		t.Fatalf("没改的字段不该变: %+v", updated)
	}

	// 用旧版本号再改一次：必须是明确的版本冲突。
	_, err = svc.Update(ctx, UpdateInput{ID: site.ID, Version: 1, Patch: Patch{ReadTimeoutSeconds: &timeout}})
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("应为版本冲突，实际 %v", err)
	}

	// 把域名改成别人已经用的：拒绝。
	taken := "other.example.com"
	_, err = svc.Update(ctx, UpdateInput{ID: site.ID, Version: 2, Patch: Patch{Domain: &taken}})
	var cerr *ConflictError
	if !errors.As(err, &cerr) || cerr.Report.State != ConflictTaken {
		t.Fatalf("改成已占用的域名应被拒绝，实际 %v", err)
	}

	// 改成同一个域名（只是重新提交）：不算冲突。
	same := site.Domain
	if _, err := svc.Update(ctx, UpdateInput{ID: site.ID, Version: 2, Patch: Patch{Domain: &same}}); err != nil {
		t.Fatalf("提交同一个域名不该失败: %v", err)
	}

	// 空 patch 是调用方的错误。
	if _, err := svc.Update(ctx, UpdateInput{ID: site.ID, Version: 3}); err == nil {
		t.Fatal("空 patch 应该被拒绝")
	}
}

// TestUpdateInvalidatesFacts 确认写完会把 Nginx 快照作废。
func TestUpdateInvalidatesFacts(t *testing.T) {
	ctx := context.Background()
	facts := freeFacts()
	svc, _ := newTestService(t, Options{Facts: facts})

	site, err := svc.Create(ctx, validDefinition())
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	if facts.invalidated == 0 {
		t.Fatal("创建之后应作废快照")
	}
	before := facts.invalidated
	if err := svc.Delete(ctx, site.ID, site.Version); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if facts.invalidated <= before {
		t.Fatal("删除之后应作废快照")
	}
}

// TestDelete 确认删除的版本语义。
func TestDelete(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestService(t, Options{Facts: freeFacts()})
	site, err := svc.Create(ctx, validDefinition())
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	if err := svc.Delete(ctx, site.ID, site.Version+1); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("版本不对应回版本冲突，实际 %v", err)
	}
	if err := svc.Delete(ctx, site.ID, site.Version); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if _, err := svc.Get(ctx, site.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("删掉之后应读不到，实际 %v", err)
	}
	if err := svc.Delete(ctx, site.ID, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("重复删除应回不存在，实际 %v", err)
	}
}

// TestListPagination 确认按 (domain, id) 稳定翻页，且不会漏也不会重。
func TestListPagination(t *testing.T) {
	ctx := context.Background()
	svc, _ := newTestService(t, Options{Facts: freeFacts()})

	domains := []string{"a.example.com", "b.example.com", "c.example.com", "d.example.com", "e.example.com"}
	for _, d := range domains {
		def := validDefinition()
		def.Domain = d
		if _, err := svc.Create(ctx, def); err != nil {
			t.Fatalf("创建 %s 失败: %v", d, err)
		}
	}

	var seen []string
	cursor := ""
	for pages := 0; pages < 10; pages++ {
		list, next, err := svc.List(ctx, ListOptions{Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatalf("列表失败: %v", err)
		}
		for _, s := range list {
			seen = append(seen, s.Domain)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if len(seen) != len(domains) {
		t.Fatalf("翻页应正好读到 %d 条，实际 %d 条：%v", len(domains), len(seen), seen)
	}
	for i := range domains {
		if seen[i] != domains[i] {
			t.Fatalf("顺序应为 %v，实际 %v", domains, seen)
		}
	}

	if _, _, err := svc.List(ctx, ListOptions{Limit: MaxListLimit + 1}); !errors.Is(err, ErrInvalidLimit) {
		t.Fatalf("超范围 limit 应被拒绝，实际 %v", err)
	}
	if _, _, err := svc.List(ctx, ListOptions{Cursor: "不是游标"}); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("非法游标应被拒绝，实际 %v", err)
	}
}

// TestGetUnknownID 确认形状不对的标识直接当作不存在，不去查库。
func TestGetUnknownID(t *testing.T) {
	svc, _ := newTestService(t, Options{Facts: freeFacts()})
	for _, id := range []string{"", "site_", "site_XYZ", "../etc/passwd"} {
		if _, err := svc.Get(context.Background(), id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%q 应回不存在，实际 %v", id, err)
		}
	}
}
