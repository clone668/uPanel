package backup

import (
	"strings"
	"testing"
)

// testPolicyRepoID / testAppID 是测试用的合法标识（ops_test.go 里的 testRepoID
// 是仓库**id**本身——64 位十六进制，不是 rep_ 形式，两者不是一回事）。
const (
	testPolicyRepoID = "rep_0123456789abcdef0123456789abcdef"
	testAppID        = "app_0123456789abcdef0123456789abcdef"
	testAppID2       = "app_fedcba9876543210fedcba9876543210"
)

// TestPolicyNormalizeAcceptsFilesOnly 是最小合法策略：一个根、只备份文件。
func TestPolicyNormalizeAcceptsFilesOnly(t *testing.T) {
	p := Policy{
		Name:         "  站点文件  ",
		RepositoryID: testPolicyRepoID,
		Sources:      []Source{{RootID: "www"}},
	}
	out, err := p.Normalize()
	if err != nil {
		t.Fatalf("最小策略不该被拒：%v", err)
	}
	if out.Name != "站点文件" {
		t.Fatalf("名字应被去掉首尾空白，得到 %q", out.Name)
	}
	// 一致性口径缺省是 files_only：绝大多数策略只备份文件，逼人每次显式写一遍
	// 只会让人复制粘贴。
	if out.ConsistencyMode != ConsistencyFilesOnly {
		t.Fatalf("缺省一致性应为 files_only，得到 %q", out.ConsistencyMode)
	}
	// kind 缺省按 root 处理，但规范之后要写实：JSON 里不该再留一个空 kind。
	if got := out.Sources[0].Kind; got != SourceKindRoot {
		t.Fatalf("来源 kind 应规范成 root，得到 %q", got)
	}
	if out.Sources[0].RelativePath != "" {
		t.Fatalf("空相对路径表示根本身，得到 %q", out.Sources[0].RelativePath)
	}
}

// TestPolicyNormalizeRejects 是拒绝表。每一条都对应一个真实后果。
func TestPolicyNormalizeRejects(t *testing.T) {
	cases := []struct {
		name string
		in   Policy
		want string
	}{
		{
			name: "名字为空",
			in:   Policy{RepositoryID: testPolicyRepoID, Sources: []Source{{RootID: "www"}}},
			want: "policy.name",
		},
		{
			name: "名字含换行",
			in:   Policy{Name: "站点\n文件", RepositoryID: testPolicyRepoID, Sources: []Source{{RootID: "www"}}},
			want: "policy.name",
		},
		{
			name: "仓库 id 形状不对",
			in:   Policy{Name: "a", RepositoryID: "rep_XYZ", Sources: []Source{{RootID: "www"}}},
			want: "policy.repository_id",
		},
		{
			name: "没有来源",
			in:   Policy{Name: "a", RepositoryID: testPolicyRepoID},
			want: "policy.sources",
		},
		{
			name: "根 id 为空",
			in:   Policy{Name: "a", RepositoryID: testPolicyRepoID, Sources: []Source{{}}},
			want: "policy.sources[0].root_id",
		},
		{
			name: "相对路径带 ..",
			in: Policy{Name: "a", RepositoryID: testPolicyRepoID,
				Sources: []Source{{RootID: "www", RelativePath: "../etc"}}},
			want: "policy.sources[0].relative_path",
		},
		{
			name: "绝对路径",
			in: Policy{Name: "a", RepositoryID: testPolicyRepoID,
				Sources: []Source{{RootID: "www", RelativePath: "/etc"}}},
			want: "policy.sources[0].relative_path",
		},
		{
			name: "重复来源",
			in: Policy{Name: "a", RepositoryID: testPolicyRepoID,
				Sources: []Source{{RootID: "www"}, {RootID: "www"}}},
			want: "重复",
		},
		{
			name: "面板库来源带路径",
			in: Policy{Name: "a", RepositoryID: testPolicyRepoID,
				Sources: []Source{{Kind: SourceKindPanelDatabase, RootID: "www"}}},
			want: "不接受 root_id",
		},
		{
			name: "面板库列两次",
			in: Policy{Name: "a", RepositoryID: testPolicyRepoID,
				Sources: []Source{{Kind: SourceKindPanelDatabase}, {Kind: SourceKindPanelDatabase}}},
			want: "与前一条重复", // 重复判定先于“只能列一次”
		},
		{
			name: "未知来源种类",
			in: Policy{Name: "a", RepositoryID: testPolicyRepoID,
				Sources: []Source{{Kind: "tarball", RootID: "www"}}},
			want: "policy.sources[0].kind",
		},
		{
			name: "未知一致性口径",
			in: Policy{Name: "a", RepositoryID: testPolicyRepoID,
				Sources: []Source{{RootID: "www"}}, ConsistencyMode: "crash_consistent"},
			want: "policy.consistency_mode",
		},
		{
			name: "停应用但没列应用",
			in: Policy{Name: "a", RepositoryID: testPolicyRepoID,
				Sources: []Source{{RootID: "www"}}, ConsistencyMode: ConsistencyStopManagedApps},
			want: "policy.app_ids",
		},
		{
			name: "只备份文件却列了应用",
			in: Policy{Name: "a", RepositoryID: testPolicyRepoID,
				Sources: []Source{{RootID: "www"}}, AppIDs: []string{testAppID}},
			want: "policy.app_ids",
		},
		{
			name: "应用 id 形状不对",
			in: Policy{Name: "a", RepositoryID: testPolicyRepoID,
				Sources:         []Source{{RootID: "www"}},
				ConsistencyMode: ConsistencyStopManagedApps,
				AppIDs:          []string{"nginx"}},
			want: "policy.app_ids[0]",
		},
		{
			name: "保留条数为负",
			in: Policy{Name: "a", RepositoryID: testPolicyRepoID,
				Sources: []Source{{RootID: "www"}}, Retention: Retention{KeepLast: -1}},
			want: "policy.retention.keep_last",
		},
		{
			name: "保留条数太大",
			in: Policy{Name: "a", RepositoryID: testPolicyRepoID,
				Sources: []Source{{RootID: "www"}}, Retention: Retention{KeepDaily: 100000}},
			want: "policy.retention.keep_daily",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.in.Normalize()
			if err == nil {
				t.Fatalf("应当被拒：%+v", tc.in)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误里应含 %q，得到 %q", tc.want, err.Error())
			}
			if !strings.Contains(err.Error(), ErrInvalid.Error()) {
				t.Fatalf("错误应包裹 ErrInvalid（否则接口层分不出 400 与 500）：%q", err.Error())
			}
		})
	}
}

// TestPolicyStopAppsSortsAppIDs 让同一组应用换个顺序也得到同一份指纹。
func TestPolicyStopAppsSortsAppIDs(t *testing.T) {
	base := Policy{
		Name:            "数据库",
		RepositoryID:    testPolicyRepoID,
		Sources:         []Source{{RootID: "data"}},
		ConsistencyMode: ConsistencyStopManagedApps,
	}
	a := base
	a.AppIDs = []string{testAppID, testAppID2}
	b := base
	b.AppIDs = []string{testAppID2, testAppID}

	na, err := a.Normalize()
	if err != nil {
		t.Fatalf("规范化失败：%v", err)
	}
	nb, err := b.Normalize()
	if err != nil {
		t.Fatalf("规范化失败：%v", err)
	}
	fa, err := na.IdentityFingerprint()
	if err != nil {
		t.Fatalf("指纹失败：%v", err)
	}
	fb, err := nb.IdentityFingerprint()
	if err != nil {
		t.Fatalf("指纹失败：%v", err)
	}
	if fa != fb {
		t.Fatalf("顺序不该影响身份指纹：%s vs %s", fa, fb)
	}
}

// TestPolicyFingerprintChangesWithContent 确认指纹真的绑住了内容。
func TestPolicyFingerprintChangesWithContent(t *testing.T) {
	a := Policy{Name: "a", RepositoryID: testPolicyRepoID, Sources: []Source{{RootID: "www"}}}
	b := a
	b.Sources = []Source{{RootID: "www", RelativePath: "site"}}
	na, _ := a.Normalize()
	nb, _ := b.Normalize()
	fa, _ := na.IdentityFingerprint()
	fb, _ := nb.IdentityFingerprint()
	if fa == fb {
		t.Fatal("改了来源之后指纹必须变：否则预检绑定的策略可以被悄悄替换")
	}
}

// TestPolicyPanelDatabaseFlag 确认面板库来源能被认出来。
func TestPolicyPanelDatabaseFlag(t *testing.T) {
	p := Policy{
		Name:         "全量",
		RepositoryID: testPolicyRepoID,
		Sources: []Source{
			{RootID: "www", RelativePath: "site"},
			{Kind: SourceKindPanelDatabase},
		},
	}
	out, err := p.Normalize()
	if err != nil {
		t.Fatalf("规范化失败：%v", err)
	}
	if !out.HasPanelDatabase() {
		t.Fatal("列了面板数据库来源却没认出来")
	}
	if got := len(out.RootSources()); got != 1 {
		t.Fatalf("根来源应只有 1 条，得到 %d", got)
	}
}
