package packages

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"upanel/internal/apt"
)

// fakeSource 是 Source 的假实现，只回一份固定事实。
type fakeSource struct {
	snap     apt.Snapshot
	state    apt.PowerState
	err      error
	stateErr error
	fresh    []bool
	power    int
}

func (f *fakeSource) Packages(_ context.Context, fresh bool) (apt.Snapshot, error) {
	f.fresh = append(f.fresh, fresh)
	if f.err != nil {
		return apt.Snapshot{}, f.err
	}
	return f.snap, nil
}

func (f *fakeSource) PowerState(_ context.Context) (apt.PowerState, error) {
	f.power++
	if f.stateErr != nil {
		return apt.PowerState{}, f.stateErr
	}
	return f.state, nil
}

func newService(t *testing.T, src Source) *Service {
	t.Helper()
	svc, err := New(src)
	if err != nil {
		t.Fatalf("构造服务失败: %v", err)
	}
	return svc
}

// 这些版本串都是 VM-A / VM-B 上的真值（见 internal/apt 的夹具）。
func factPackages() []apt.Package {
	return []apt.Package{
		// 候选同时来自 jammy-updates 与 jammy-security，且比已安装新 → 安全更新。
		{Name: "bash", Architecture: "amd64", Status: "ii ", InstalledVersion: "5.1-6ubuntu1",
			CandidateVersion: "5.1-6ubuntu1.1",
			CandidateSuites:  []string{"jammy-security", "jammy-updates"}},
		// apparmor：候选比已安装新，但候选只来自 jammy-updates；
		// jammy-security 里放的是**更旧**的 3.0.4-2ubuntu2.4。不是安全更新。
		{Name: "apparmor", Architecture: "amd64", Status: "ii ", InstalledVersion: "3.0.4-2ubuntu2",
			CandidateVersion: "3.0.4-2ubuntu2.5",
			CandidateSuites:  []string{"jammy-updates"}},
		// 已是最新：候选等于已安装（这里还带 epoch 与 `~`）。
		{Name: "libssl3", Architecture: "amd64", Status: "ii ", InstalledVersion: "2:3.0.2-0ubuntu1.30",
			CandidateVersion: "2:3.0.2-0ubuntu1.30",
			CandidateSuites:  []string{"jammy-updates"}},
		// 候选更旧（pin / 回退）：要如实说 older，且不算可升级。
		{Name: "pinned", Architecture: "amd64", Status: "ii ", InstalledVersion: "2.0-1",
			CandidateVersion: "1.9-1",
			CandidateSuites:  []string{"jammy"}},
		// 本地 dpkg -i 装的包：APT 说没有候选。
		{Name: "locally", Architecture: "amd64", Status: "ii ", InstalledVersion: "0.1.0",
			CandidateFromDpkgStatus: true},
		// 只剩配置文件：不是已安装。
		{Name: "gone", Architecture: "amd64", Status: "rc ", InstalledVersion: "1.0-1"},
		// 版本串解析不了：关系是 unknown，绝不能猜成 newer。
		{Name: "weird", Architecture: "amd64", Status: "ii ", InstalledVersion: "v1.0",
			CandidateVersion: "v1.1"},
	}
}

func factSnapshot() apt.Snapshot {
	at := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	idx := at.Add(-2 * time.Hour)
	return apt.Snapshot{
		CollectedAt:    at,
		IndexUpdatedAt: &idx,
		Packages:       factPackages(),
		Errors:         []apt.SourceError{},
	}
}

func TestJudgeRelations(t *testing.T) {
	snap := factSnapshot()
	byName := map[string]Entry{}
	for _, p := range snap.Packages {
		byName[p.Name] = judge(p)
	}

	if e := byName["bash"]; e.CandidateRelation != RelationNewer || !e.Upgradable || !e.Security {
		t.Errorf("bash = %+v", e)
	}
	if e := byName["apparmor"]; e.CandidateRelation != RelationNewer || !e.Upgradable || e.Security {
		t.Errorf("apparmor 不该算安全更新（候选取自 jammy-updates）: %+v", e)
	}
	if e := byName["libssl3"]; e.CandidateRelation != RelationSame || e.Upgradable {
		t.Errorf("libssl3 = %+v", e)
	}
	if e := byName["pinned"]; e.CandidateRelation != RelationOlder || e.Upgradable {
		t.Errorf("pinned = %+v", e)
	}
	if e := byName["locally"]; e.CandidateVersion != nil || e.CandidateRelation != RelationUnknown {
		t.Errorf("locally = %+v", e)
	}
	if e := byName["weird"]; e.CandidateRelation != RelationUnknown || e.Upgradable {
		t.Errorf("weird 的版本串解析不了，关系应为 unknown: %+v", e)
	}
	if e := byName["gone"]; e.Installed {
		t.Errorf("rc 状态不是已安装: %+v", e)
	}
	if e := byName["bash"]; e.Pockets == nil || len(e.Pockets) != 2 {
		t.Errorf("bash pockets = %v", e.Pockets)
	}
}

func TestInstalledStatus(t *testing.T) {
	cases := map[string]bool{
		"ii ": true,
		"ii":  true,
		"rc ": false,
		"iU ": false, // 期望装、目前未配置——不是"已安装好用"
		"un ": false,
		"":    false,
		"i":   false,
	}
	for in, want := range cases {
		if got := isInstalledStatus(in); got != want {
			t.Errorf("isInstalledStatus(%q) = %v，期望 %v", in, got, want)
		}
	}
}

func TestPocketOfSuite(t *testing.T) {
	cases := map[string]string{
		"jammy":                 PocketRelease,
		"noble":                 PocketRelease,
		"jammy-updates":         PocketUpdates,
		"jammy-security":        PocketSecurity,
		"jammy-backports":       PocketBackports,
		"jammy-proposed":        PocketProposed,
		"bookworm-fasttrack":    PocketOther, // 认得形状但不是已知口袋
		"":                      PocketOther,
		"dl.example.com/stable": PocketOther,
	}
	for in, want := range cases {
		if got := pocketOfSuite(in); got != want {
			t.Errorf("pocketOfSuite(%q) = %q，期望 %q", in, got, want)
		}
	}
	// 输出顺序固定（与 OpenAPI enum 一致），不随输入顺序变。
	got := pocketsOf([]string{"jammy-security", "jammy", "jammy-updates"})
	want := []string{PocketRelease, PocketUpdates, PocketSecurity}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("pocketsOf = %v，期望 %v", got, want)
	}
}

func TestListFilters(t *testing.T) {
	src := &fakeSource{snap: factSnapshot()}
	svc := newService(t, src)

	// 只装入的包：`rc` 的 gone 不出现在默认视图里，但仍计入 errors/事实。
	res, err := svc.List(context.Background(), ListQuery{Installed: true})
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if res.Matched != 6 {
		t.Errorf("matched = %d，期望 6（7 个里排除 rc）", res.Matched)
	}
	if res.Counts.Installed != 6 {
		t.Errorf("counts.installed = %d", res.Counts.Installed)
	}
	if res.Counts.Upgradable != 2 || res.Counts.Security != 1 {
		t.Errorf("counts = %+v", res.Counts)
	}
	if !res.Filters.Installed {
		t.Error("filters 应回显本次生效的条件")
	}

	// installed=false：连 rc 也算进来。
	res, err = svc.List(context.Background(), ListQuery{})
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if res.Matched != 7 {
		t.Errorf("matched = %d，期望 7", res.Matched)
	}

	// 只看可升级。
	res, err = svc.List(context.Background(), ListQuery{Installed: true, Upgradable: true})
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if res.Matched != 2 {
		t.Errorf("可升级数 = %d，期望 2（bash、apparmor；pinned 的候选更旧不算）", res.Matched)
	}

	// 只看安全更新。
	res, err = svc.List(context.Background(), ListQuery{Installed: true, Security: true})
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if res.Matched != 1 || res.Packages[0].Name != "bash" {
		t.Errorf("安全更新 = %+v", res.Packages)
	}

	// 子串搜索，大小写不敏感。
	res, err = svc.List(context.Background(), ListQuery{Installed: true, Query: "SSL"})
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if res.Matched != 1 || res.Packages[0].Name != "libssl3" {
		t.Errorf("搜索 ssL = %+v", res.Packages)
	}

	// 展示用读取吃代理缓存（fresh=false）。
	for _, f := range src.fresh {
		if f {
			t.Error("展示读取不该请求 fresh=true")
		}
	}
}

func TestListPaging(t *testing.T) {
	snap := factSnapshot()
	// 造 250 个包，验证分页不重不漏。
	snap.Packages = nil
	for i := 0; i < 250; i++ {
		snap.Packages = append(snap.Packages, apt.Package{
			Name:             fmt.Sprintf("pkg%03d", i),
			Architecture:     "amd64",
			Status:           "ii ",
			InstalledVersion: "1.0",
			CandidateVersion: "1.0",
		})
	}
	svc := newService(t, &fakeSource{snap: snap})

	seen := map[string]bool{}
	limit := 50
	cursor := ""
	pages := 0
	for {
		res, err := svc.List(context.Background(), ListQuery{Limit: limit, Cursor: cursor})
		if err != nil {
			t.Fatalf("第 %d 页失败: %v", pages+1, err)
		}
		if res.Limit != limit {
			t.Errorf("limit = %d", res.Limit)
		}
		if res.Matched != 250 {
			t.Errorf("matched = %d", res.Matched)
		}
		for _, p := range res.Packages {
			if seen[p.Name] {
				t.Fatalf("%s 在两页里都出现了", p.Name)
			}
			seen[p.Name] = true
		}
		pages++
		if res.NextCursor == nil {
			break
		}
		cursor = *res.NextCursor
		if pages > 10 {
			t.Fatal("翻页没有终止")
		}
	}
	if pages != 5 || len(seen) != 250 {
		t.Fatalf("页数 = %d，包数 = %d（期望 5 / 250）", pages, len(seen))
	}

	// 最后一页的 next_cursor 必须是 null，不是空串。
	res, err := svc.List(context.Background(), ListQuery{Limit: 200, Cursor: encodeCursor("pkg249", "amd64")})
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(res.Packages) != 0 || res.NextCursor != nil {
		t.Errorf("越过末行后应为空页且 next_cursor 为 null，实际 %d 条 / %v",
			len(res.Packages), res.NextCursor)
	}
	// 上限本身允许 200，且一页装得下的更多内容不会被塞进来。
	small := &fakeSource{snap: apt.Snapshot{CollectedAt: time.Now().UTC(),
		Packages: snap.Packages[:120]}}
	res, err = newService(t, small).List(context.Background(), ListQuery{Limit: 200})
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(res.Packages) != 120 || res.NextCursor != nil {
		t.Fatalf("一页装下 120 条时不该有下一页：%d 条 / %v", len(res.Packages), res.NextCursor)
	}
}

func TestListCursorSurvivesMultiarchNames(t *testing.T) {
	// 包名里带 `:`（multiarch）——游标不能因为分隔符被拆坏。
	snap := apt.Snapshot{
		CollectedAt: time.Now().UTC(),
		Packages: []apt.Package{
			{Name: "libssl3:amd64", Architecture: "amd64", Status: "ii ", InstalledVersion: "1"},
			{Name: "libssl3:i386", Architecture: "i386", Status: "ii ", InstalledVersion: "1"},
			{Name: "zlib1g", Architecture: "amd64", Status: "ii ", InstalledVersion: "1"},
		},
	}
	svc := newService(t, &fakeSource{snap: snap})
	first, err := svc.List(context.Background(), ListQuery{Limit: 1})
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if first.NextCursor == nil {
		t.Fatal("应有下一页")
	}
	second, err := svc.List(context.Background(), ListQuery{Limit: 1, Cursor: *first.NextCursor})
	if err != nil {
		t.Fatalf("第二页失败: %v", err)
	}
	if len(second.Packages) != 1 || second.Packages[0].Name != "libssl3:i386" {
		t.Fatalf("第二页 = %+v", second.Packages)
	}
}

func TestListValidation(t *testing.T) {
	svc := newService(t, &fakeSource{snap: factSnapshot()})

	// limit 省略 → 默认 50。
	res, err := svc.List(context.Background(), ListQuery{})
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if res.Limit != DefaultLimit {
		t.Errorf("默认 limit = %d，期望 %d", res.Limit, DefaultLimit)
	}

	// 超过上限 → 400 类错误，不静默截断成 200（那样调用方以为拿到了要的东西）。
	if _, err := svc.List(context.Background(), ListQuery{Limit: MaxLimit + 1}); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("limit 超限错误 = %v", err)
	}
	if _, err := svc.List(context.Background(), ListQuery{Query: "vim\napt"}); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("控制字符应被拒: %v", err)
	}
	if _, err := svc.List(context.Background(), ListQuery{Query: string(make([]byte, MaxQueryLen+1))}); !errors.Is(err, ErrInvalidQuery) {
		t.Errorf("超长 query 应被拒: %v", err)
	}
	if _, err := svc.List(context.Background(), ListQuery{Cursor: "not-base64!!"}); !errors.Is(err, ErrInvalidCursor) {
		t.Errorf("坏游标错误 = %v", err)
	}
	// 合法 base64 但不是我们的形状。
	if _, err := svc.List(context.Background(), ListQuery{Cursor: "YWJj"}); !errors.Is(err, ErrInvalidCursor) {
		t.Errorf("形状不对的游标应被拒: %v", err)
	}
}

func TestUpdatesOrderingAndCap(t *testing.T) {
	snap := factSnapshot()
	snap.Packages = nil
	// 1200 个可升级包，其中每 100 个里有一个安全更新。名字故意让"安全更新"排在
	// 字典序后面，验证排序真的把 security 提到前面。
	for i := 0; i < 1200; i++ {
		name := fmt.Sprintf("pkg%04d", i)
		suites := []string{"jammy-updates"}
		if i%100 == 0 {
			suites = []string{"jammy-security"}
		}
		snap.Packages = append(snap.Packages, apt.Package{
			Name: name, Architecture: "amd64", Status: "ii ",
			InstalledVersion: "1.0", CandidateVersion: "2.0", CandidateSuites: suites,
		})
	}
	svc := newService(t, &fakeSource{snap: snap})
	res, err := svc.Updates(context.Background())
	if err != nil {
		t.Fatalf("Updates 失败: %v", err)
	}
	if res.Total != 1200 || res.Security != 12 {
		t.Fatalf("total/security = %d/%d", res.Total, res.Security)
	}
	if !res.Truncated || len(res.Updates) != MaxUpdates {
		t.Fatalf("truncated/条数 = %v/%d", res.Truncated, len(res.Updates))
	}
	// 前 12 条必须是安全更新。
	for i := 0; i < 12; i++ {
		if !res.Updates[i].Security {
			t.Fatalf("第 %d 条不是安全更新: %+v", i, res.Updates[i])
		}
	}
	if res.Updates[12].Security {
		t.Fatalf("第 12 条不该是安全更新（安全更新只有 12 个）: %+v", res.Updates[12])
	}
	// 其余按包名升序。
	for i := 13; i < len(res.Updates); i++ {
		if res.Updates[i-1].Name > res.Updates[i].Name {
			t.Fatalf("排序错：%s 在 %s 之后", res.Updates[i-1].Name, res.Updates[i].Name)
		}
	}
}

func TestUpdatesEmpty(t *testing.T) {
	snap := factSnapshot()
	snap.Packages = nil
	svc := newService(t, &fakeSource{snap: snap})
	res, err := svc.Updates(context.Background())
	if err != nil {
		t.Fatalf("Updates 失败: %v", err)
	}
	if res.Total != 0 || res.Updates == nil || res.Errors == nil {
		t.Fatalf("空结果要给出空数组而不是 null: %+v", res)
	}
}

func TestDegradedErrorsPassThrough(t *testing.T) {
	// apt-cache 失败：包照出，候选全 unknown，errors[] 带出来源。
	src := &fakeSource{snap: apt.Snapshot{
		CollectedAt: time.Now().UTC(),
		Packages: []apt.Package{
			{Name: "bash", Architecture: "amd64", Status: "ii ", InstalledVersion: "5.1-6ubuntu1"},
		},
		Errors: []apt.SourceError{{Source: "apt_cache", Message: "apt-cache policy 退出码 100"}},
	}}
	svc := newService(t, src)
	res, err := svc.List(context.Background(), ListQuery{Installed: true})
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(res.Errors) != 1 || res.Errors[0].Source != "apt_cache" {
		t.Fatalf("errors = %+v", res.Errors)
	}
	if res.Packages[0].CandidateRelation != RelationUnknown || res.Packages[0].CandidateVersion != nil {
		t.Fatalf("降级时不能拿已安装版本冒充候选: %+v", res.Packages[0])
	}
}

func TestPowerState(t *testing.T) {
	at := time.Date(2026, 10, 23, 9, 30, 0, 0, time.UTC)
	src := &fakeSource{state: apt.PowerState{
		CollectedAt:            at,
		RebootRequired:         true,
		RebootRequiredSince:    &at,
		RebootRequiredPackages: []string{"linux-image-5.15.0-198-generic"},
		BootID:                 "ab2eb5e8-c1b4-4a00-a3d0-33ced7870abc",
	}}
	view, err := newService(t, src).PowerState(context.Background())
	if err != nil {
		t.Fatalf("PowerState 失败: %v", err)
	}
	if !view.RebootRequired || view.BootID == "" || len(view.RebootRequiredPackages) != 1 {
		t.Fatalf("view = %+v", view)
	}
	if src.power != 1 {
		t.Errorf("PowerState 应每次现场读取，实际 %d 次", src.power)
	}

	// 空清单也要是空数组。
	src2 := &fakeSource{state: apt.PowerState{CollectedAt: at}}
	view, err = newService(t, src2).PowerState(context.Background())
	if err != nil {
		t.Fatalf("PowerState 失败: %v", err)
	}
	if view.RebootRequiredPackages == nil {
		t.Error("reboot_required_packages 应为空数组")
	}
}

func TestSourceErrorPropagates(t *testing.T) {
	svc := newService(t, &fakeSource{err: fmt.Errorf("%w: socket 不见了", ErrFactsUnavailable)})
	if _, err := svc.List(context.Background(), ListQuery{}); !errors.Is(err, ErrFactsUnavailable) {
		t.Errorf("List 错误 = %v", err)
	}
	if _, err := svc.Updates(context.Background()); !errors.Is(err, ErrFactsUnavailable) {
		t.Errorf("Updates 错误 = %v", err)
	}
}

func TestNewRequiresSource(t *testing.T) {
	if _, err := New(nil); err == nil {
		t.Fatal("没有事实来源时应报错，而不是给一个永远空的列表")
	}
}
