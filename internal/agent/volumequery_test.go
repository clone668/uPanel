package agent

// 卷查询的接线测试（P07.4）：storage.volumes 与 storage.volume.blockers。
//
// 这一层只负责三件事：把参数按形状拒绝、把采集器的视图原样返回、以及**只在真的有
// 卷能力时登记**。判决与执行在 storage 与 volumeaction 两层各自有测试，这里不重复。

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"upanel/internal/storage"
	"upanel/internal/syscmd"
	"upanel/internal/testsupport"
)

// fakeAgentVolumes 是 agent 侧最小的卷登记表：只读，不做任何判断。
type fakeAgentVolumes struct{ recs []storage.VolumeRecord }

func (f *fakeAgentVolumes) ListVolumes(context.Context) ([]storage.VolumeRecord, error) {
	return append([]storage.VolumeRecord(nil), f.recs...), nil
}

func (f *fakeAgentVolumes) GetVolume(_ context.Context, id string) (storage.VolumeRecord, bool, error) {
	for _, r := range f.recs {
		if r.ID == id {
			return r, true, nil
		}
	}
	return storage.VolumeRecord{}, false, nil
}

func (f *fakeAgentVolumes) SaveVolume(context.Context, storage.VolumeRecord) error { return nil }

func (f *fakeAgentVolumes) DeleteVolume(context.Context, string) error { return nil }

// fakeAgentSuperblocks 让卷视图能读出 UUID（真机上是读盘上的字节）。
type fakeAgentSuperblocks map[string]storage.FilesystemIdentity

func (f fakeAgentSuperblocks) Probe(_ context.Context, devID string, _ uint64) (storage.FilesystemIdentity, error) {
	if id, ok := f[devID]; ok {
		return id, nil
	}
	return storage.FilesystemIdentity{}, os.ErrNotExist
}

// testVolumeCollector 是在夹具采集器上补齐卷依赖的采集器。
//
// 只有 Store / Fstab.Runner / Superblocks 三样都在，VolumeCapable() 才为真——
// 这三样正是生产装配里决定“这台主机能不能做卷管理”的东西。
func testVolumeCollector(t *testing.T, recs []storage.VolumeRecord) *storage.Collector {
	t.Helper()
	dir, err := testsupport.FixtureDir("storage-2404")
	if err != nil {
		t.Fatalf("%v", err)
	}
	runner := storage.CommandRunnerFunc(func(context.Context, syscmd.Spec, syscmd.Limits) (syscmd.Result, error) {
		return syscmd.Result{ExitCode: 0}, nil
	})
	return storage.NewCollector(storage.Options{
		Root: storage.Root{
			Sys:   filepath.Join(dir, "sys"),
			Proc:  filepath.Join(dir, "proc"),
			Disks: fixtureDiskReader{dir: filepath.Join(dir, "disks")},
		},
		Write: storage.WriteOptions{
			Store: &fakeAgentVolumes{recs: recs},
			// Fstab.Runner 只需要“有”：查询不写 fstab，但能力判定看它。
			Fstab: storage.FstabStore{
				Path:      filepath.Join(t.TempDir(), "fstab"),
				BackupDir: t.TempDir(),
				Runner:    runner,
				Findmnt:   "/usr/bin/findmnt",
			},
			Superblocks: fakeAgentSuperblocks{
				// 夹具里 sda16 是 /boot 上的 ext4。
				"sda16": {Kind: storage.FilesystemExt4, UUID: "b4f0deba-ce7d-45dc-9999-0427f2720320"},
			},
			Runner: runner,
		},
	})
}

// fixtureVolume 造一条指向夹具里那块 ext4 分区的登记记录。
func fixtureVolume() storage.VolumeRecord {
	return storage.VolumeRecord{
		ID:             "vol_0123456789abcdef0123456789abcdef",
		DisplayName:    "启动卷",
		Slug:           "boot-data",
		DeviceID:       "sda16",
		FilesystemUUID: "b4f0deba-ce7d-45dc-9999-0427f2720320",
		FilesystemType: storage.FilesystemExt4,
		Config:         storage.DefaultVolumeConfig(),
		DesiredState:   storage.DesiredUnmounted,
		Version:        "3",
		UpdatedAt:      time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC),
	}
}

// TestVolumesQueryReturnsRegisteredVolumes 校验 storage.volumes 的形状与内容。
//
// 形状是 {"volumes": [...]} 而不是裸数组：以后加 snapshot_version 之类附注时
// 不必改响应外形（裸数组只能靠再包一层，那是破坏性变更）。
func TestVolumesQueryReturnsRegisteredVolumes(t *testing.T) {
	col := testVolumeCollector(t, []storage.VolumeRecord{fixtureVolume()})
	if !col.VolumeCapable() {
		t.Fatal("三样依赖都接上了，VolumeCapable() 却是假")
	}
	if topo := col.Refresh(context.Background()); topo.Status != storage.StatusOK {
		t.Fatalf("扫描失败: %+v", topo.Error)
	}
	reg, err := NewProductionRegistry(ProductionDeps{Storage: col})
	if err != nil {
		t.Fatalf("构造生产登记表失败: %v", err)
	}
	srv := newTestServerWithRegistry(t, reg)

	rec := postQuery(t, srv, `{"query":"`+storage.VolumesQueryName+`"}`, testWebUID)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（%s）", rec.Code, rec.Body.String())
	}
	var body struct {
		Data struct {
			Volumes []storage.Volume `json:"volumes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if len(body.Data.Volumes) != 1 {
		t.Fatalf("卷数 = %d，期望 1（%s）", len(body.Data.Volumes), rec.Body.String())
	}
	got := body.Data.Volumes[0]
	if got.ID != "vol_0123456789abcdef0123456789abcdef" || got.Slug != "boot-data" {
		t.Errorf("卷标识字段不对：%+v", got)
	}
	if got.MountPath != storage.MountPathOf("boot-data") {
		t.Errorf("挂载点 = %q", got.MountPath)
	}
	// observed_state 必须是一个具体值：它是页面上一句话的依据，不能是空串。
	if got.ObservedState == "" {
		t.Errorf("observed_state 是空的：%+v", got)
	}
	// 默认配置要如实序列化：前端按同一套 DTO 读，缺字段等于配置不明。
	if !got.Nodev || !got.Nosuid || got.Automount || got.ReadOnly {
		t.Errorf("配置字段不对：%+v", got)
	}
}

// TestVolumeBlockersQueryRejectsBadID 校验参数形状：卷标识会进 URL、日志与审计。
func TestVolumeBlockersQueryRejectsBadID(t *testing.T) {
	col := testVolumeCollector(t, nil)
	reg, err := NewProductionRegistry(ProductionDeps{Storage: col})
	if err != nil {
		t.Fatalf("构造生产登记表失败: %v", err)
	}
	srv := newTestServerWithRegistry(t, reg)

	for _, raw := range []string{
		`{"query":"` + storage.VolumeBlockersQueryName + `"}`,
		`{"query":"` + storage.VolumeBlockersQueryName + `","params":{}}`,
		`{"query":"` + storage.VolumeBlockersQueryName + `","params":{"volume_id":""}}`,
		`{"query":"` + storage.VolumeBlockersQueryName + `","params":{"volume_id":"vol_x"}}`,
		`{"query":"` + storage.VolumeBlockersQueryName + `","params":{"volume_id":"../etc/passwd"}}`,
		`{"query":"` + storage.VolumeBlockersQueryName + `","params":{"volume_id":"vol_0123456789ABCDEF0123456789abcdef"}}`,
		`{"query":"` + storage.VolumeBlockersQueryName + `","params":{"volume_id":"vol_0123456789abcdef0123456789abcdef","mount":true}}`,
	} {
		rec := postQuery(t, srv, raw, testWebUID)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: 状态码 = %d，期望 400（%s）", raw, rec.Code, rec.Body.String())
		}
	}
}

// TestVolumeBlockersQueryReturnsEmptyLists 校验“没有占用”是空数组而不是 null。
//
// 页面据此决定显示什么；null 会让前端多一段“这个字段可能是 null”的分支，
// 而那段分支永远没法被真实数据验证。
func TestVolumeBlockersQueryReturnsEmptyLists(t *testing.T) {
	col := testVolumeCollector(t, []storage.VolumeRecord{fixtureVolume()})
	if topo := col.Refresh(context.Background()); topo.Status != storage.StatusOK {
		t.Fatalf("扫描失败: %+v", topo.Error)
	}
	reg, err := NewProductionRegistry(ProductionDeps{Storage: col})
	if err != nil {
		t.Fatalf("构造生产登记表失败: %v", err)
	}
	srv := newTestServerWithRegistry(t, reg)

	rec := postQuery(t, srv, `{"query":"`+storage.VolumeBlockersQueryName+
		`","params":{"volume_id":"`+fixtureVolume().ID+`"}}`, testWebUID)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（%s）", rec.Code, rec.Body.String())
	}
	var body struct {
		Data struct {
			VolumeID  string                     `json:"volume_id"`
			Mounted   bool                       `json:"mounted"`
			Mounts    []string                   `json:"mounts"`
			Processes []storage.BusyProcess      `json:"processes"`
			Holders   []string                   `json:"holders"`
			Apps      []storage.VolumeDependency `json:"apps"`
			Shares    []storage.VolumeDependency `json:"shares"`
			Backups   []storage.VolumeDependency `json:"backups"`
			CheckedAt time.Time                  `json:"checked_at"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if body.Data.VolumeID != fixtureVolume().ID {
		t.Errorf("volume_id = %q", body.Data.VolumeID)
	}
	if body.Data.Mounts == nil || body.Data.Processes == nil || body.Data.Holders == nil ||
		body.Data.Apps == nil || body.Data.Shares == nil || body.Data.Backups == nil {
		t.Errorf("占用列表不该是 null：%s", rec.Body.String())
	}
	if body.Data.CheckedAt.IsZero() {
		t.Error("checked_at 是零值：占用是某一瞬间的快照，必须带时间")
	}
}

// TestVolumeQueriesNotRegisteredWithoutVolumeDeps 钉住能力门：
// 采集器接上了、但卷依赖没接时，两条卷查询与五个卷动作都不登记。
//
// 登记了就等于在 capabilities 里承诺“能做”，而实际会在第一次点击时以
// DEPENDENCY_MISSING 失败——那是能力撒谎。
func TestVolumeQueriesNotRegisteredWithoutVolumeDeps(t *testing.T) {
	col := testStorageCollector(t) // 只有拓扑，没有 Store/Fstab/Superblocks
	if col.VolumeCapable() {
		t.Fatal("只接了拓扑却报 VolumeCapable")
	}
	reg, err := NewProductionRegistry(ProductionDeps{Storage: col})
	if err != nil {
		t.Fatalf("构造生产登记表失败: %v", err)
	}
	for _, name := range []string{storage.VolumesQueryName, storage.VolumeBlockersQueryName} {
		if _, ok := reg.Query(name); ok {
			t.Errorf("没有卷能力却登记了查询 %s", name)
		}
	}
	for _, name := range []string{
		storage.ActionVolumeImport, storage.ActionVolumeMount, storage.ActionVolumeUnmount,
		storage.ActionVolumeConfigure, storage.ActionVolumeForget,
	} {
		if _, ok := reg.Action(name); ok {
			t.Errorf("没有卷能力却登记了动作 %s", name)
		}
	}
}

// TestVolumeQueriesRegisteredWhenCapable 反向确认：依赖齐了就该出现，且带执行器。
func TestVolumeQueriesRegisteredWhenCapable(t *testing.T) {
	col := testVolumeCollector(t, nil)
	reg, err := NewProductionRegistry(ProductionDeps{Storage: col})
	if err != nil {
		t.Fatalf("构造生产登记表失败: %v", err)
	}
	for _, name := range []string{storage.VolumesQueryName, storage.VolumeBlockersQueryName} {
		if _, ok := reg.Query(name); !ok {
			t.Errorf("卷能力齐备却没有登记查询 %s", name)
		}
	}
	for _, name := range []string{
		storage.ActionVolumeImport, storage.ActionVolumeMount, storage.ActionVolumeUnmount,
		storage.ActionVolumeConfigure, storage.ActionVolumeForget,
	} {
		a, ok := reg.Action(name)
		if !ok {
			t.Errorf("卷能力齐备却没有登记动作 %s", name)
			continue
		}
		if a.Execute == nil {
			t.Errorf("%s 没有执行器", name)
		}
	}
}
