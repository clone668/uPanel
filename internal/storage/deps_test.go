package storage

// P08.3：面板侧依赖（共享）对卷动作的阻断。
//
// 这一组用例盯的不是“有没有拦”，而是**口径**：
//   - 卸载只算启用中的共享（禁用中的没有客户端，不构成卸载的理由）；
//   - 格式化与退出管理算全部共享（格式化之后内容没了，禁用也一样）；
//   - 查询失败或没接线一律阻塞（“问不出”不等于“没有”）。
//
// 三条里任何一条写反，都会出现“页面允许的动作删掉了别人的数据”。

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// depRow 是一条依赖以及它此刻是否正在对外服务。
type depRow struct {
	dep     VolumeDependency
	enabled bool
}

// depProber 是假的依赖查询：一张按卷、按设备查表的表，外加一个可注入的错误。
//
// 它同时负责按 Purpose 过滤启用状态——真实适配层也必须这么做，因为只有它知道
// 共享的启用位；storage 层只负责把用途问清楚。
type depProber struct {
	byVolume map[string][]depRow
	byDevice map[string][]depRow
	err      error
	queries  []DependencyQuery
}

func (p *depProber) probe(_ context.Context, q DependencyQuery) ([]VolumeDependency, error) {
	p.queries = append(p.queries, q)
	if p.err != nil {
		return nil, p.err
	}
	rows := p.byVolume[q.VolumeID]
	if q.DeviceID != "" {
		rows = p.byDevice[q.DeviceID]
	}
	out := []VolumeDependency{}
	for _, r := range rows {
		if q.Purpose == PurposeUnmount && !r.enabled {
			continue
		}
		out = append(out, r.dep)
	}
	return out, nil
}

func shareDep(name string) VolumeDependency {
	return VolumeDependency{Kind: "share", ID: "share-" + name, Name: name}
}

// lastQuery 返回最后一次查询，用来断言问的是谁、问的什么用途。
func (p *depProber) lastQuery(t *testing.T) DependencyQuery {
	t.Helper()
	if len(p.queries) == 0 {
		t.Fatal("没有发出任何依赖查询")
	}
	return p.queries[len(p.queries)-1]
}

// ---------- 卸载 ----------

// TestAssessUnmountBlockedByEnabledShare 校验“有共享正在对外服务”时卸载被拒。
func TestAssessUnmountBlockedByEnabledShare(t *testing.T) {
	rec := testRecord(testVolID, "data", "sdb1", testUUID)
	a := newAssessFixture(t, fstabWith(t, rec.FstabEntry())).allowMountPoint("data").withRecord(rec)
	a.f.super.put("sdb1", testUUID, FilesystemExt4)
	prober := &depProber{byVolume: map[string][]depRow{
		testVolID: {{dep: shareDep("团队盘"), enabled: true}},
	}}
	a.env.Dependents = prober.probe

	topo := volumeTopo("sha256:topo1", testUUID)
	mountDeviceAt(findNode(&topo, "sdb1"), rec.MountPath())
	got := a.assess(t, ActionVolumeUnmount, VolumeParams{VolumeID: rec.ID, Version: "1"}, topo)

	mustBlock(t, got, NoticeVolumeInUseByShare)
	msg := blockerMessage(got, NoticeVolumeInUseByShare)
	if !strings.Contains(msg, "团队盘") {
		t.Errorf("说明里没有点出是哪个共享：%q", msg)
	}
	if q := prober.lastQuery(t); q.VolumeID != testVolID || q.Purpose != PurposeUnmount {
		t.Errorf("查询 = %+v，应当按卷问卸载口径", q)
	}
}

// TestAssessUnmountIgnoresDisabledShare 校验禁用中的共享不拦卸载。
//
// 这条是“口径”用例：如果实现改成“任何共享都拦”，它会红——这正是要防的过度阻断，
// 因为它会逼用户为了卸一块盘先删掉自己的配置。
func TestAssessUnmountIgnoresDisabledShare(t *testing.T) {
	rec := testRecord(testVolID, "data", "sdb1", testUUID)
	a := newAssessFixture(t, fstabWith(t, rec.FstabEntry())).allowMountPoint("data").withRecord(rec)
	a.f.super.put("sdb1", testUUID, FilesystemExt4)
	a.env.Dependents = (&depProber{byVolume: map[string][]depRow{
		testVolID: {{dep: shareDep("旧归档"), enabled: false}},
	}}).probe

	topo := volumeTopo("sha256:topo1", testUUID)
	mountDeviceAt(findNode(&topo, "sdb1"), rec.MountPath())
	mustAllow(t, a.assess(t, ActionVolumeUnmount, VolumeParams{VolumeID: rec.ID, Version: "1"}, topo))
}

// TestAssessUnmountBlocksWhenDependencyQueryFails 校验查询失败时阻塞。
func TestAssessUnmountBlocksWhenDependencyQueryFails(t *testing.T) {
	rec := testRecord(testVolID, "data", "sdb1", testUUID)
	a := newAssessFixture(t, fstabWith(t, rec.FstabEntry())).allowMountPoint("data").withRecord(rec)
	a.f.super.put("sdb1", testUUID, FilesystemExt4)
	a.env.Dependents = (&depProber{err: errors.New("共享登记表读不出来")}).probe

	topo := volumeTopo("sha256:topo1", testUUID)
	mountDeviceAt(findNode(&topo, "sdb1"), rec.MountPath())
	got := a.assess(t, ActionVolumeUnmount, VolumeParams{VolumeID: rec.ID, Version: "1"}, topo)

	mustBlock(t, got, NoticeVolumeDependenciesUnknown)
	if msg := blockerMessage(got, NoticeVolumeDependenciesUnknown); !strings.Contains(msg, "共享登记表") {
		t.Errorf("说明里没有带上失败原因：%q", msg)
	}
}

// TestAssessUnmountBlocksWithoutDependencyProber 校验没接线时同样阻塞。
//
// “没接线”和“查空了”在类型上必须能够区分，否则装配漏接一个探针就会悄悄放行。
func TestAssessUnmountBlocksWithoutDependencyProber(t *testing.T) {
	rec := testRecord(testVolID, "data", "sdb1", testUUID)
	a := newAssessFixture(t, fstabWith(t, rec.FstabEntry())).allowMountPoint("data").withRecord(rec)
	a.f.super.put("sdb1", testUUID, FilesystemExt4)
	a.env.Dependents = nil

	topo := volumeTopo("sha256:topo1", testUUID)
	mountDeviceAt(findNode(&topo, "sdb1"), rec.MountPath())
	mustBlock(t, a.assess(t, ActionVolumeUnmount, VolumeParams{VolumeID: rec.ID, Version: "1"}, topo),
		NoticeVolumeDependenciesUnknown)
}

// ---------- 退出管理 ----------

// TestAssessForgetBlockedByAnyShare 校验退出管理连禁用的共享也拦。
//
// 退出管理会删掉绑定关系：盘还挂着配置却没了，下次开机就没人把它挂回来。
// 禁用中的共享同样要算，因为它记着“这个卷该挂在哪儿”。
func TestAssessForgetBlockedByAnyShare(t *testing.T) {
	rec := testRecord(testVolID, "data", "sdb1", testUUID)
	a := newAssessFixture(t, fstabWith(t, rec.FstabEntry())).allowMountPoint("data").withRecord(rec)
	a.f.super.put("sdb1", testUUID, FilesystemExt4)
	prober := &depProber{byVolume: map[string][]depRow{
		testVolID: {{dep: shareDep("旧归档"), enabled: false}},
	}}
	a.env.Dependents = prober.probe

	got := a.assess(t, ActionVolumeForget, VolumeParams{VolumeID: rec.ID, Version: "1"},
		volumeTopo("sha256:topo1", testUUID))

	mustBlock(t, got, NoticeVolumeHasDependencies)
	if q := prober.lastQuery(t); q.Purpose != PurposeForget {
		t.Errorf("查询用途 = %q，应当是 forget", q.Purpose)
	}
}

// TestAssessForgetBlocksWhenDependencyQueryFails 校验退出管理在查询失败时阻塞。
func TestAssessForgetBlocksWhenDependencyQueryFails(t *testing.T) {
	rec := testRecord(testVolID, "data", "sdb1", testUUID)
	a := newAssessFixture(t, fstabWith(t, rec.FstabEntry())).allowMountPoint("data").withRecord(rec)
	a.f.super.put("sdb1", testUUID, FilesystemExt4)
	a.env.Dependents = (&depProber{err: errors.New("登记表打不开")}).probe

	mustBlock(t, a.assess(t, ActionVolumeForget, VolumeParams{VolumeID: rec.ID, Version: "1"},
		volumeTopo("sha256:topo1", testUUID)), NoticeVolumeDependenciesUnknown)
}

// ---------- 格式化 ----------

// formatTopo 造一块带一个空分区的 GPT 盘（格式化判决的合法目标）。
func formatTopo(t *testing.T) Topology {
	t.Helper()
	sdb := gptDisk("sdb", 8<<30)
	sdb1 := addPartition(&sdb, "sdb1", 1, mib, 4<<30)
	return guardTopo("sha256:v1", []Device{sdb, sdb1})
}

// TestAssessFormatBlockedByAnyShare 校验格式化被任何共享挡住（含禁用中的）。
func TestAssessFormatBlockedByAnyShare(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		name := "启用中的共享"
		if !enabled {
			name = "禁用中的共享"
		}
		t.Run(name, func(t *testing.T) {
			env, _, _ := testEnv()
			prober := &depProber{byDevice: map[string][]depRow{
				"sdb1": {{dep: shareDep("团队盘"), enabled: enabled}},
			}}
			env.Dependents = prober.probe

			a, err := Assess(context.Background(), formatTopo(t), ActionFilesystemFormat,
				FilesystemFormatParams{DeviceID: "sdb1", Filesystem: FilesystemExt4}, env)
			if err != nil {
				t.Fatalf("判决失败: %v", err)
			}
			mustBlock(t, a, NoticeVolumeInUseByShare)
			if q := prober.lastQuery(t); q.DeviceID != "sdb1" || q.Purpose != PurposeFormat {
				t.Errorf("查询 = %+v，应当按设备问格式化口径", q)
			}
		})
	}
}

// TestAssessFormatAllowsWithoutShares 校验没有依赖时格式化放行。
//
// 反向用例：如果实现变成“不查也拦”或“永远拦”，这条会红。
func TestAssessFormatAllowsWithoutShares(t *testing.T) {
	env, _, _ := testEnv()
	a, err := Assess(context.Background(), formatTopo(t), ActionFilesystemFormat,
		FilesystemFormatParams{DeviceID: "sdb1", Filesystem: FilesystemExt4}, env)
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	mustAllow(t, a)
}

// TestAssessFormatBlocksWithoutDependencyProber 校验没接线时格式化被拒。
func TestAssessFormatBlocksWithoutDependencyProber(t *testing.T) {
	env, _, _ := testEnv()
	env.Dependents = nil
	a, err := Assess(context.Background(), formatTopo(t), ActionFilesystemFormat,
		FilesystemFormatParams{DeviceID: "sdb1", Filesystem: FilesystemExt4}, env)
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	mustBlock(t, a, NoticeVolumeDependenciesUnknown)
}

// TestAssessFormatBlocksWhenDependencyQueryFails 校验查询失败时格式化被拒。
func TestAssessFormatBlocksWhenDependencyQueryFails(t *testing.T) {
	env, _, _ := testEnv()
	env.Dependents = (&depProber{err: errors.New("登记表被锁住")}).probe
	a, err := Assess(context.Background(), formatTopo(t), ActionFilesystemFormat,
		FilesystemFormatParams{DeviceID: "sdb1", Filesystem: FilesystemExt4}, env)
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	mustBlock(t, a, NoticeVolumeDependenciesUnknown)
}

// ---------- 占用列表 ----------

// TestVolumeBlockersMarkBusyWhenDependencyQueryFails 校验查询失败时占用列表报 busy。
//
// 页面上的 busy 直接对应“卸载按钮能不能点”。查不出依赖却报 busy=false，
// 就会把人引到一次注定被拒（或更糟：删掉数据）的操作上。
func TestVolumeBlockersMarkBusyWhenDependencyQueryFails(t *testing.T) {
	dir := copyFixture(t, "storage-2404")
	store := newFakeVolumeStore()
	store.recs[volID1] = testRecord(volID1, "boot-elsewhere", "sda16", bootUUID)
	c := volumeCollector(t, dir, WriteOptions{
		Store:       store,
		Superblocks: superProber{"sda16": {Kind: FilesystemExt4, UUID: bootUUID}},
		Env:         Env{Busy: &fakeBusy{}, Dependents: (&depProber{err: errors.New("打不开")}).probe},
	})

	got, err := c.VolumeBlockers(context.Background(), volID1)
	if err != nil {
		t.Fatalf("读取占用失败: %v", err)
	}
	if !got.Busy {
		t.Fatal("依赖查询失败却报告 busy=false")
	}
	if len(got.Shares) != 0 {
		t.Errorf("没查成却列了共享：%+v", got.Shares)
	}
}

// TestVolumeBlockersListsShares 校验依赖被归到 shares 一栏并且算进 busy。
func TestVolumeBlockersListsShares(t *testing.T) {
	dir := copyFixture(t, "storage-2404")
	store := newFakeVolumeStore()
	store.recs[volID1] = testRecord(volID1, "boot-elsewhere", "sda16", bootUUID)
	c := volumeCollector(t, dir, WriteOptions{
		Store:       store,
		Superblocks: superProber{"sda16": {Kind: FilesystemExt4, UUID: bootUUID}},
		Env: Env{Busy: &fakeBusy{}, Dependents: (&depProber{byVolume: map[string][]depRow{
			volID1: {{dep: shareDep("团队盘"), enabled: true}},
		}}).probe},
	})

	got, err := c.VolumeBlockers(context.Background(), volID1)
	if err != nil {
		t.Fatalf("读取占用失败: %v", err)
	}
	if len(got.Shares) != 1 || got.Shares[0].Name != "团队盘" {
		t.Errorf("shares = %+v", got.Shares)
	}
	if !got.Busy {
		t.Error("有共享绑着却报告 busy=false")
	}
}
