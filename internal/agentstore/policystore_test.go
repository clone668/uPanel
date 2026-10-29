package agentstore

// 备份策略仓储的测试（P12.2，CONTRACTS.md 12 / 14.2）。
//
// 重点不在 CRUD 的形状，而在四条不变量：
//   1. 策略内容原样往返——它决定备份哪几个目录、要不要停应用；
//   2. 名字不撞车（唯一索引是最后一道，不能只靠应用层）；
//   3. 版本冲突与“没有这条记录”必须分得开（409 与 404 是两件事）；
//   4. 删策略不碰仓库表——仓库与快照留在原地是契约要求的。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"upanel/internal/backup"
)

const (
	testPolicyIDValue = "pol_0123456789abcdef0123456789abcdef"
	testPolicyRepo    = "rep_0123456789abcdef0123456789abcdef"
	testPolicyApp     = "app_0123456789abcdef0123456789abcdef"
)

func newTestPolicies(t *testing.T) (*BackupPolicyStore, *BackupRepositoryStore) {
	t.Helper()
	db := openTestDB(t)
	ps, err := NewBackupPolicyStore(db)
	if err != nil {
		t.Fatalf("构造策略仓储失败: %v", err)
	}
	rs, err := NewBackupRepositoryStore(db)
	if err != nil {
		t.Fatalf("构造仓库仓储失败: %v", err)
	}
	return ps, rs
}

// testPolicy 造一条形状合法的策略。
func testPolicy(name string) backup.Policy {
	return backup.Policy{
		Name:            name,
		RepositoryID:    testPolicyRepo,
		Sources:         []backup.Source{{Kind: backup.SourceKindRoot, RootID: "www", RelativePath: "site"}},
		ConsistencyMode: backup.ConsistencyFilesOnly,
		Retention:       backup.Retention{KeepLast: 7, KeepDaily: 14},
	}
}

func TestNewBackupPolicyStoreRejectsNilDB(t *testing.T) {
	if _, err := NewBackupPolicyStore(nil); err == nil {
		t.Fatal("没有数据库句柄却构造成功了")
	}
}

// TestPolicyStoreRoundTrip 确认来源、口径、保留与应用名单都原样回来。
func TestPolicyStoreRoundTrip(t *testing.T) {
	ps, _ := newTestPolicies(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 24, 6, 0, 0, 0, time.UTC)

	p := testPolicy("数据库")
	p.Sources = []backup.Source{
		{Kind: backup.SourceKindRoot, RootID: "data"},
		{Kind: backup.SourceKindPanelDatabase},
	}
	p.ConsistencyMode = backup.ConsistencyStopManagedApps
	p.AppIDs = []string{testPolicyApp}

	created, err := ps.Create(ctx, p, now)
	if err != nil {
		t.Fatalf("新建失败: %v", err)
	}
	if !ValidPolicyID(created.ID) {
		t.Fatalf("生成的 id 形态不对：%q", created.ID)
	}
	if created.Version != 1 {
		t.Fatalf("新记录版本应为 1，得到 %d", created.Version)
	}

	got, ok, err := ps.Get(ctx, created.ID)
	if err != nil || !ok {
		t.Fatalf("读回失败: ok=%v err=%v", ok, err)
	}
	if got.Policy.ConsistencyMode != backup.ConsistencyStopManagedApps {
		t.Fatalf("一致性口径丢了：%q", got.Policy.ConsistencyMode)
	}
	if len(got.Policy.Sources) != 2 || got.Policy.Sources[1].Kind != backup.SourceKindPanelDatabase {
		t.Fatalf("来源没有原样回来：%+v", got.Policy.Sources)
	}
	if len(got.Policy.AppIDs) != 1 || got.Policy.AppIDs[0] != testPolicyApp {
		t.Fatalf("应用名单没有原样回来：%+v", got.Policy.AppIDs)
	}
	if got.Policy.Retention.KeepDaily != 14 {
		t.Fatalf("保留策略没有原样回来：%+v", got.Policy.Retention)
	}
	if !got.CreatedAt.Equal(now) || !got.UpdatedAt.Equal(now) {
		t.Fatalf("时间戳不对：%v / %v", got.CreatedAt, got.UpdatedAt)
	}
}

// TestPolicyStoreNameUnique 确认同名写第二次被拒，而且报的是能给人看的话。
func TestPolicyStoreNameUnique(t *testing.T) {
	ps, _ := newTestPolicies(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if _, err := ps.Create(ctx, testPolicy("站点文件"), now); err != nil {
		t.Fatalf("第一条应当成功: %v", err)
	}
	_, err := ps.Create(ctx, testPolicy("站点文件"), now)
	if !errors.Is(err, ErrPolicyName) {
		t.Fatalf("同名应当报 ErrPolicyName，得到 %v", err)
	}
	if !strings.Contains(err.Error(), "站点文件") {
		t.Fatalf("错误里应带名字：%v", err)
	}
}

// TestPolicyStoreVersionConflict 区分 404 与 409。
func TestPolicyStoreVersionConflict(t *testing.T) {
	ps, _ := newTestPolicies(t)
	ctx := context.Background()
	now := time.Now().UTC()
	created, err := ps.Create(ctx, testPolicy("站点文件"), now)
	if err != nil {
		t.Fatalf("新建失败: %v", err)
	}

	updated := testPolicy("站点文件与图片")
	updated.Sources = append(updated.Sources, backup.Source{Kind: backup.SourceKindRoot, RootID: "www", RelativePath: "img"})
	rec, err := ps.Update(ctx, created.ID, updated, created.Version, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("按当前版本更新应当成功: %v", err)
	}
	if rec.Version != created.Version+1 {
		t.Fatalf("更新后版本应自增，得到 %d", rec.Version)
	}
	if len(rec.Policy.Sources) != 2 {
		t.Fatalf("来源没有更新：%+v", rec.Policy.Sources)
	}

	// 拿着旧版本再改一次：必须 409，而不是悄悄覆盖。
	if _, err := ps.Update(ctx, created.ID, updated, created.Version, now); !errors.Is(err, ErrPolicyVersion) {
		t.Fatalf("旧版本应当报 ErrPolicyVersion，得到 %v", err)
	}
	// 不存在的 id：必须 404，而不是版本冲突。
	if _, err := ps.Update(ctx, testPolicyIDValue, updated, 1, now); !errors.Is(err, ErrPolicyNotFound) {
		t.Fatalf("不存在的 id 应当报 ErrPolicyNotFound，得到 %v", err)
	}
}

// TestPolicyStoreDeleteKeepsRepository 确认删策略不碰仓库表。
func TestPolicyStoreDeleteKeepsRepository(t *testing.T) {
	ps, rs := newTestPolicies(t)
	ctx := context.Background()
	now := time.Now().UTC()

	def := backup.Definition{
		Name:              "本地仓库",
		Backend:           backup.BackendLocal,
		Location:          backup.Location{RootID: "www", RelativePath: "repo"},
		PasswordSecretRef: "sec_0123456789abcdef0123456789abcdef",
	}
	normalized, err := def.Normalize()
	if err != nil {
		t.Fatalf("定义不合法: %v", err)
	}
	repo, err := rs.Create(ctx, normalized, now)
	if err != nil {
		t.Fatalf("建仓库失败: %v", err)
	}
	p := testPolicy("站点文件")
	p.RepositoryID = repo.ID
	created, err := ps.Create(ctx, p, now)
	if err != nil {
		t.Fatalf("建策略失败: %v", err)
	}

	if err := ps.Delete(ctx, created.ID); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if _, ok, err := ps.Get(ctx, created.ID); err != nil || ok {
		t.Fatalf("删除后不该还能读到: ok=%v err=%v", ok, err)
	}
	if _, ok, err := rs.Get(ctx, repo.ID); err != nil || !ok {
		t.Fatalf("删策略把仓库也删了: ok=%v err=%v", ok, err)
	}
	// 幂等：再删一次不算错（重试不该变成错误）。
	if err := ps.Delete(ctx, created.ID); err != nil {
		t.Fatalf("重复删除应当幂等: %v", err)
	}
}

// TestPoliciesForRepository 确认“谁在用这个仓库”能问出来。
func TestPoliciesForRepository(t *testing.T) {
	ps, _ := newTestPolicies(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for _, name := range []string{"站点文件", "图片"} {
		if _, err := ps.Create(ctx, testPolicy(name), now); err != nil {
			t.Fatalf("建策略失败: %v", err)
		}
	}
	other := testPolicy("别的仓库")
	other.RepositoryID = "rep_fedcba9876543210fedcba9876543210"
	if _, err := ps.Create(ctx, other, now); err != nil {
		t.Fatalf("建策略失败: %v", err)
	}
	got, err := ps.PoliciesForRepository(ctx, testPolicyRepo)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("应只有 2 条引用该仓库，得到 %d", len(got))
	}
}
