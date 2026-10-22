package storage

// 受管理卷读取视图的测试（P07.4）：列表、详情、占用。
//
// 这三条都是**只读**，但它们最容易写成谎话：把 desired_state 抄成 observed_state、
// 没查占用的卷说“没人用”、空数组写成 null。这里把这几条逐条钉住。
//
// 拓扑来自 tests/fixtures/storage-2404 的真夹具（复制到临时目录后按需改动 /proc/swaps
// 与 sys/block/*/holders），因此“挂载点在哪、谁是交换区、谁被 dm 消费”都是扫描器
// 真的从文件里读出来的，不是测试自己编的。

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"upanel/internal/httpx"
	"upanel/internal/syscmd"
)

const (
	// bootUUID 是夹具里 sda16（/boot，ext4）的 UUID。
	bootUUID = "b4f0deba-ce7d-45dc-9999-0427f2720320"
	volID1   = "vol_11111111111111111111111111111111"
	volID2   = "vol_22222222222222222222222222222222"
	volID3   = "vol_33333333333333333333333333333333"
)

// copyFixture 把夹具复制到临时目录，返回副本根目录。
//
// 复制是为了能在“真的扫描器”上造出占用：交换区由 /proc/swaps 决定，dm 消费者由
// sysfs 的 holders 目录决定，这两样都只能靠改文件来构造。
func copyFixture(t *testing.T, name string) string {
	t.Helper()
	src, err := fixtureDir(name)
	if err != nil {
		t.Fatalf("%v", err)
	}
	dst := filepath.Join(t.TempDir(), name)
	if err := os.CopyFS(dst, os.DirFS(src)); err != nil {
		t.Fatalf("复制夹具失败: %v", err)
	}
	return dst
}

// noopRunner 是卷读取用不到、但能力判定要用到的命令执行器。
func noopRunner() CommandRunner {
	return CommandRunnerFunc(func(context.Context, syscmd.Spec, syscmd.Limits) (syscmd.Result, error) {
		return syscmd.Result{ExitCode: 0}, nil
	})
}

// volumeCollector 在夹具根上组装一个带卷依赖的采集器。
//
// 命令执行器不在这里补：卷能力要求“三样都在”，缺了执行器就是缺了一项，
// 那正是 TestVolumeCapableNeedsAllThree 要验证的东西。
func volumeCollector(t *testing.T, dir string, wo WriteOptions) *Collector {
	t.Helper()
	// 依赖查询与占用检查一样，是“必须接线才能放行”的输入。夹具不接线时不是
	// 让它退化成空，而是显式写成“查过了，没有依赖”，免得把漏接线写进断言基准。
	if wo.Env.Dependents == nil {
		wo.Env.Dependents = noDependents
	}
	if wo.Fstab.Path == "" {
		wo.Fstab.Path = filepath.Join(t.TempDir(), "fstab")
	}
	if wo.Fstab.BackupDir == "" {
		wo.Fstab.BackupDir = t.TempDir()
	}
	if wo.Now == nil {
		wo.Now = func() time.Time { return time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC) }
	}
	return NewCollector(Options{
		Interval: time.Hour,
		Root: Root{
			Sys:   filepath.Join(dir, "sys"),
			Proc:  filepath.Join(dir, "proc"),
			Disks: &fixtureDiskReader{dir: filepath.Join(dir, "disks")},
		},
		Write: wo,
	})
}

// superProber 是一张“设备上是哪个文件系统”的表；没登记的设备按“读不出来”处理。
type superProber map[string]FilesystemIdentity

func (s superProber) Probe(_ context.Context, devID string, _ uint64) (FilesystemIdentity, error) {
	if id, ok := s[devID]; ok {
		return id, nil
	}
	return FilesystemIdentity{}, errors.New("没有已知的文件系统")
}

// TestVolumesViewObservesLiveState 列表里的 observed_state 必须来自现场，
// 且按挂载点排序（稳定输出是验收脚本与页面 diff 的前提）。
func TestVolumesViewObservesLiveState(t *testing.T) {
	dir := copyFixture(t, "storage-2404")
	store := newFakeVolumeStore()
	// 登记的 device_id 故意反着写：sda16 那条的挂载路径更大。
	store.recs[volID2] = testRecord(volID2, "boot-elsewhere", "sda16", bootUUID)
	store.recs[volID1] = testRecord(volID1, "alpha", "sdb", testUUID)
	c := volumeCollector(t, dir, WriteOptions{
		Store: store,
		Superblocks: superProber{
			"sda16": {Kind: FilesystemExt4, UUID: bootUUID},
			"sdb":   {Kind: FilesystemExt4, UUID: testUUID},
		},
	})

	got, err := c.Volumes(context.Background())
	if err != nil {
		t.Fatalf("读取卷列表失败: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("卷数 = %d", len(got))
	}
	if got[0].Slug != "alpha" || got[1].Slug != "boot-elsewhere" {
		t.Fatalf("排序不对：%q, %q", got[0].Slug, got[1].Slug)
	}

	// sdb：拓扑里有、没挂载、超级块 UUID 与登记一致 → unmounted。
	if got[0].ObservedState != VolumeStateUnmounted {
		t.Errorf("alpha 的 observed_state = %q，期望 %q（%s）",
			got[0].ObservedState, VolumeStateUnmounted, got[0].ObservedError)
	}
	if got[0].MountPath != MountPathOf("alpha") {
		t.Errorf("挂载点 = %q", got[0].MountPath)
	}
	if got[0].Capacity != nil {
		t.Errorf("没挂载却给了容量：%+v", got[0].Capacity)
	}

	// sda16：文件系统挂在 /boot，不是面板的挂载点 → 事实仍然是 mounted
	// （“挂在别处”在契约里不是一个独立状态，见 TestObserveVolumeFlagsMountedElsewhere）。
	if got[1].ObservedState != VolumeStateMounted {
		t.Errorf("boot-elsewhere 的 observed_state = %q，期望 %q（%s）",
			got[1].ObservedState, VolumeStateMounted, got[1].ObservedError)
	}
	for _, v := range got {
		if v.Dependencies == nil {
			t.Errorf("%s 的 dependencies 是 nil，应为空数组", v.Slug)
		}
		// desired_state 是意图，observed_state 是事实；两者都不能是空串。
		if v.DesiredState != DesiredUnmounted {
			t.Errorf("%s 的 desired_state = %q", v.Slug, v.DesiredState)
		}
		if v.ObservedState == "" {
			t.Errorf("%s 的 observed_state 是空串", v.Slug)
		}
	}
}

// TestVolumeViewDefaults 记录里缺省的值要有确定的落点，不能漏成空串。
func TestVolumeViewDefaults(t *testing.T) {
	dir := copyFixture(t, "storage-2404")
	rec := testRecord(volID1, "alpha", "sdb", testUUID)
	rec.DesiredState = "" // 老记录没写期望状态
	store := newFakeVolumeStore()
	store.recs[rec.ID] = rec
	c := volumeCollector(t, dir, WriteOptions{
		Store:       store,
		Superblocks: superProber{"sdb": {Kind: FilesystemExt4, UUID: testUUID}},
	})

	v, err := c.Volume(context.Background(), rec.ID)
	if err != nil {
		t.Fatalf("读取卷失败: %v", err)
	}
	// 缺省按“期望不挂载”解释：这是最保守的读法。
	if v.DesiredState != DesiredUnmounted {
		t.Errorf("desired_state = %q，期望 %q", v.DesiredState, DesiredUnmounted)
	}
	if v.ObservedState == "" {
		t.Error("observed_state 是空串")
	}
	if !v.Nodev || !v.Nosuid {
		t.Errorf("nodev/nosuid 丢了：%+v", v)
	}
}

// TestVolumeViewUnknownCarriesReason 读不出 UUID 时是 unknown，并且必须带原因。
func TestVolumeViewUnknownCarriesReason(t *testing.T) {
	dir := copyFixture(t, "storage-2404")
	store := newFakeVolumeStore()
	store.recs[volID1] = testRecord(volID1, "alpha", "sdb", testUUID)
	// 没有超级块读取器：本机无从核对 UUID。
	c := volumeCollector(t, dir, WriteOptions{Store: store})

	v, err := c.Volume(context.Background(), volID1)
	if err != nil {
		t.Fatalf("读取卷失败: %v", err)
	}
	if v.ObservedState != VolumeStateUnknown {
		t.Fatalf("observed_state = %q，期望 %q", v.ObservedState, VolumeStateUnknown)
	}
	if v.ObservedError == "" {
		t.Error("unknown 却没给原因：页面只能显示“未知”三个字")
	}
}

// TestVolumeDetailErrors 不存在的卷是 ErrVolumeNotFound（调用方翻成 404），
// 登记表读不出来则是另一类错误（调用方翻成 500）。
func TestVolumeDetailErrors(t *testing.T) {
	dir := copyFixture(t, "storage-2404")
	store := newFakeVolumeStore()
	store.recs[volID1] = testRecord(volID1, "alpha", "sdb", testUUID)
	c := volumeCollector(t, dir, WriteOptions{
		Store:       store,
		Superblocks: superProber{"sdb": {Kind: FilesystemExt4, UUID: testUUID}},
	})

	if _, err := c.Volume(context.Background(), volID2); !errors.Is(err, ErrVolumeNotFound) {
		t.Fatalf("未知卷的错误 = %v，期望 ErrVolumeNotFound", err)
	}
	if _, err := c.VolumeBlockers(context.Background(), volID2); !errors.Is(err, ErrVolumeNotFound) {
		t.Fatalf("未知卷占用信息的错误 = %v，期望 ErrVolumeNotFound", err)
	}

	store.err = errors.New("登记表读不出来")
	if _, err := c.Volume(context.Background(), volID1); errors.Is(err, ErrVolumeNotFound) {
		t.Fatal("登记表故障被当成了“卷不存在”：页面会去查错地方")
	} else if err == nil {
		t.Fatal("登记表故障没有报错")
	}
}

// TestVolumeViewsWithoutStore 没有登记表时三条都是 DEPENDENCY_MISSING（503），
// 不是空列表——空列表会让页面说“这台机器上干干净净，没有卷”。
func TestVolumeViewsWithoutStore(t *testing.T) {
	dir := copyFixture(t, "storage-2404")
	c := volumeCollector(t, dir, WriteOptions{})

	checks := []struct {
		name string
		run  func() error
	}{
		{"列表", func() error { _, err := c.Volumes(context.Background()); return err }},
		{"详情", func() error { _, err := c.Volume(context.Background(), volID1); return err }},
		{"占用", func() error { _, err := c.VolumeBlockers(context.Background(), volID1); return err }},
	}
	for _, chk := range checks {
		t.Run(chk.name, func(t *testing.T) {
			var httpErr *HTTPError
			if !errors.As(chk.run(), &httpErr) {
				t.Fatalf("期待 *HTTPError（DEPENDENCY_MISSING）")
			}
			if httpErr.Status != http.StatusServiceUnavailable || httpErr.Code != httpx.CodeDependencyMissing {
				t.Errorf("判决 = %d %s", httpErr.Status, httpErr.Code)
			}
		})
	}
}

// TestVolumeBlockersReportsOccupancy 占用列表要能指着说“是这些进程在用”。
func TestVolumeBlockersReportsOccupancy(t *testing.T) {
	dir := copyFixture(t, "storage-2404")
	store := newFakeVolumeStore()
	store.recs[volID1] = testRecord(volID1, "boot-elsewhere", "sda16", bootUUID)
	busy := &fakeBusy{procs: map[string][]BusyProcess{
		"sda16": {{PID: "4242", Comm: "postgres"}},
	}}
	c := volumeCollector(t, dir, WriteOptions{
		Store:       store,
		Superblocks: superProber{"sda16": {Kind: FilesystemExt4, UUID: bootUUID}},
		Env:         Env{Busy: busy},
	})

	got, err := c.VolumeBlockers(context.Background(), volID1)
	if err != nil {
		t.Fatalf("读取占用失败: %v", err)
	}
	if !got.Mounted {
		t.Error("sda16 的文件系统挂在 /boot，却报告没挂载")
	}
	if len(got.Mounts) != 1 || got.Mounts[0] != "/boot" {
		t.Errorf("挂载点 = %v", got.Mounts)
	}
	if len(got.Processes) != 1 || got.Processes[0].PID != "4242" {
		t.Errorf("进程 = %+v", got.Processes)
	}
	if !got.Busy {
		t.Error("有进程占用却报告 busy=false")
	}
	if busy.calls == 0 {
		t.Error("没有去查占用")
	}
	if got.CheckedAt.IsZero() {
		t.Error("checked_at 是零值：占用是某一瞬间的快照")
	}
	for name, n := range map[string]int{
		"holders": len(got.Holders), "apps": len(got.Apps),
		"shares": len(got.Shares), "backups": len(got.Backups),
	} {
		if n != 0 {
			t.Errorf("%s = %d，这套夹具里没有依赖", name, n)
		}
	}
	if got.Holders == nil || got.Apps == nil || got.Shares == nil || got.Backups == nil {
		t.Error("空列表序列化成了 null")
	}
}

// TestVolumeBlockersMountedIdleIsNotBusy 挂着但没人用 = 不忙。
//
// 这两件事必须分开报：挂载状态有 mounted/mounts 两个字段说，busy 只说“现在卸载会被
// 谁挡住”。把它们混成一个，页面就会在“预检允许卸载”的地方显示“设备忙”，
// 用户点下去又成功了——比不给按钮更让人不敢相信这个面板。
func TestVolumeBlockersMountedIdleIsNotBusy(t *testing.T) {
	dir := copyFixture(t, "storage-2404")
	store := newFakeVolumeStore()
	store.recs[volID1] = testRecord(volID1, "boot-elsewhere", "sda16", bootUUID)
	busy := &fakeBusy{}
	c := volumeCollector(t, dir, WriteOptions{
		Store:       store,
		Superblocks: superProber{"sda16": {Kind: FilesystemExt4, UUID: bootUUID}},
		Env:         Env{Busy: busy},
	})

	got, err := c.VolumeBlockers(context.Background(), volID1)
	if err != nil {
		t.Fatalf("读取占用失败: %v", err)
	}
	if !got.Mounted || len(got.Mounts) != 1 {
		t.Fatalf("没报告挂载状态：%+v", got)
	}
	if got.Busy {
		t.Error("挂着但没有任何使用者，却报告 busy=true")
	}
	if len(got.Processes) != 0 || len(got.Holders) != 0 || got.SwapActive {
		t.Errorf("这套夹具里没有占用者：%+v", got)
	}
	if busy.calls == 0 {
		t.Error("挂着的卷没有去查占用进程")
	}
}

// TestVolumeBlockersListsOpenFileOwners 占用列表要能报出“打开着挂载点上文件”的进程。
//
// 这是 umount 撞 EBUSY 的最常见原因，而它不体现在设备号上：只看设备占用的实现
// 会给出一个空列表，页面只能说“设备忙”，说不出是谁。
func TestVolumeBlockersListsOpenFileOwners(t *testing.T) {
	dir := copyFixture(t, "storage-2404")
	store := newFakeVolumeStore()
	store.recs[volID1] = testRecord(volID1, "boot-elsewhere", "sda16", bootUUID)
	busy := &fakeBusy{mountProcs: map[string][]BusyProcess{
		"/boot": {{PID: "777", Comm: "nginx"}},
	}}
	c := volumeCollector(t, dir, WriteOptions{
		Store:       store,
		Superblocks: superProber{"sda16": {Kind: FilesystemExt4, UUID: bootUUID}},
		Env:         Env{Busy: busy},
	})

	got, err := c.VolumeBlockers(context.Background(), volID1)
	if err != nil {
		t.Fatalf("读取占用失败: %v", err)
	}
	if len(got.Processes) != 1 || got.Processes[0].PID != "777" {
		t.Fatalf("进程 = %+v，期望只有 777", got.Processes)
	}
	if !got.Busy {
		t.Error("有进程打开着挂载点上的文件，却报告 busy=false")
	}
	if busy.mountCalls == 0 {
		t.Error("没有去查挂载点上的打开文件")
	}
}

// TestVolumeBlockersMountScanErrorIsBusy 挂载点扫描失败也必须算“不能证明没人用”。
func TestVolumeBlockersMountScanErrorIsBusy(t *testing.T) {
	dir := copyFixture(t, "storage-2404")
	store := newFakeVolumeStore()
	store.recs[volID1] = testRecord(volID1, "boot-elsewhere", "sda16", bootUUID)
	c := volumeCollector(t, dir, WriteOptions{
		Store:       store,
		Superblocks: superProber{"sda16": {Kind: FilesystemExt4, UUID: bootUUID}},
		Env: Env{Busy: &fakeBusy{mountErrs: map[string]error{
			"/boot": errors.New("/proc 读不到"),
		}}},
	})

	got, err := c.VolumeBlockers(context.Background(), volID1)
	if err != nil {
		t.Fatalf("读取占用失败: %v", err)
	}
	if !got.Busy {
		t.Error("挂载点扫描失败，却报告 busy=false")
	}
}

// TestVolumeBlockersUnmountedSkipsBusyScan 没挂载的卷不去查进程：
// 没人能打开没挂载的文件系统，扫一遍只是白白读整个 /proc。
func TestVolumeBlockersUnmountedSkipsBusyScan(t *testing.T) {
	dir := copyFixture(t, "storage-2404")
	store := newFakeVolumeStore()
	store.recs[volID1] = testRecord(volID1, "alpha", "sdb", testUUID)
	busy := &fakeBusy{}
	c := volumeCollector(t, dir, WriteOptions{
		Store:       store,
		Superblocks: superProber{"sdb": {Kind: FilesystemExt4, UUID: testUUID}},
		Env:         Env{Busy: busy},
	})

	got, err := c.VolumeBlockers(context.Background(), volID1)
	if err != nil {
		t.Fatalf("读取占用失败: %v", err)
	}
	if got.Mounted || got.Busy {
		t.Fatalf("没挂载的卷报告占用：%+v", got)
	}
	if busy.calls != 0 {
		t.Errorf("对没挂载的卷也查了占用（%d 次）", busy.calls)
	}
	if got.Processes == nil || got.Mounts == nil {
		t.Error("空列表序列化成了 null")
	}
}

// TestVolumeBlockersUncertainIsBusy “没查成”必须算“不能证明没人用”。
func TestVolumeBlockersUncertainIsBusy(t *testing.T) {
	dir := copyFixture(t, "storage-2404")
	store := newFakeVolumeStore()
	store.recs[volID1] = testRecord(volID1, "boot-elsewhere", "sda16", bootUUID)

	cases := []struct {
		name string
		busy BusyScanner
	}{
		{"没有占用检查器", nil},
		{"占用检查器报错", &fakeBusy{errs: map[string]error{"sda16": errors.New("/proc 读不到")}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := volumeCollector(t, dir, WriteOptions{
				Store:       store,
				Superblocks: superProber{"sda16": {Kind: FilesystemExt4, UUID: bootUUID}},
				Env:         Env{Busy: tc.busy},
			})
			got, err := c.VolumeBlockers(context.Background(), volID1)
			if err != nil {
				t.Fatalf("读取占用失败: %v", err)
			}
			if !got.Busy {
				t.Fatal("查不到占用却报告 busy=false：页面会让人以为可以安全卸载")
			}
			if len(got.Processes) != 0 {
				t.Errorf("没查成却列了进程：%+v", got.Processes)
			}
		})
	}
}

// TestVolumeBlockersIncludesSwapAndHolders 交换区与 dm 消费者也算占用。
//
// 这两项不是“进程”，但让 umount 一样失败；页面必须能把它们说出来。
func TestVolumeBlockersIncludesSwapAndHolders(t *testing.T) {
	dir := copyFixture(t, "storage-2404")
	// sdb 当活动交换区（/proc/swaps 里只会出现在用的交换区）。
	swaps := "Filename\t\t\t\tType\t\tSize\t\tUsed\t\tPriority\n" +
		"/dev/sdb                               partition\t8388604\t\t0\t\t-2\n"
	if err := os.WriteFile(filepath.Join(dir, "proc", "swaps"), []byte(swaps), 0o644); err != nil {
		t.Fatalf("改夹具 swaps 失败: %v", err)
	}
	// sda16 被 dm-0 消费。
	holders := filepath.Join(dir, "sys", "block", "sda", "sda16", "holders", "dm-0")
	if err := os.MkdirAll(holders, 0o755); err != nil {
		t.Fatalf("建夹具 holders 失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(holders, "dev"), []byte("253:0\n"), 0o644); err != nil {
		t.Fatalf("写夹具 holders/dev 失败: %v", err)
	}

	store := newFakeVolumeStore()
	store.recs[volID1] = testRecord(volID1, "swap-disk", "sdb", testUUID)
	store.recs[volID3] = testRecord(volID3, "boot-elsewhere", "sda16", bootUUID)
	c := volumeCollector(t, dir, WriteOptions{
		Store: store,
		Superblocks: superProber{
			"sdb":   {Kind: FilesystemExt4, UUID: testUUID},
			"sda16": {Kind: FilesystemExt4, UUID: bootUUID},
		},
		Env: Env{Busy: &fakeBusy{}},
	})

	swap, err := c.VolumeBlockers(context.Background(), volID1)
	if err != nil {
		t.Fatalf("读取交换区卷占用失败: %v", err)
	}
	if !swap.SwapActive {
		t.Error("sdb 是活动交换区，却没有标出来")
	}
	if !swap.Busy {
		t.Error("活动交换区却报告 busy=false")
	}

	held, err := c.VolumeBlockers(context.Background(), volID3)
	if err != nil {
		t.Fatalf("读取持有者卷占用失败: %v", err)
	}
	if len(held.Holders) != 1 || held.Holders[0] != "dm-0" {
		t.Errorf("holders = %v", held.Holders)
	}
	if !held.Busy {
		t.Error("有 dm 消费者却报告 busy=false")
	}
}

// TestObserveVolumeFlagsMountedElsewhere “挂在别处”不是独立状态，但它必须被标出来。
//
// 契约里 observed_state 是闭集（mounted/unmounted/missing/mismatch/error/unknown），
// 而一块挂在 /mnt 的 ext4 在事实上仍然是 mounted。把区别放在布尔上是刻意的：
// 页面靠它说“这块盘在 /mnt，不在面板的挂载点”，判决靠它拒绝误操作。
func TestObserveVolumeFlagsMountedElsewhere(t *testing.T) {
	disk := blankDisk("sdb", 8<<30)
	part := addPartition(&disk, "sdb1", 1, 2*mib, 7<<30)
	mountDeviceAt(&part, "/mnt/otherwise")
	topo := guardTopo("sha256:v1", []Device{disk, part})
	index := indexTopology(topo)
	rec := testRecord(volID1, "alpha", "sdb1", testUUID)
	env := VolumeEnv{
		Superblocks: superProber{"sdb1": {Kind: FilesystemExt4, UUID: testUUID}},
		Now:         func() time.Time { return time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC) },
	}

	obs := observeVolume(context.Background(), topo, index, rec, env)
	if obs.State != VolumeStateMounted || !obs.MountedElsewhere {
		t.Fatalf("obs = state %q elsewhere %v", obs.State, obs.MountedElsewhere)
	}
	if len(obs.Mounts) != 1 || obs.Mounts[0] != "/mnt/otherwise" {
		t.Errorf("mounts = %v", obs.Mounts)
	}
	mustContain(t, obs.Detail, "不是面板的挂载点", "说明")

	// 同一块盘，超级块 UUID 换成别的：路径被另一个文件系统复用 → mismatch。
	env.Superblocks = superProber{"sdb1": {Kind: FilesystemExt4, UUID: testUUID2}}
	obs = observeVolume(context.Background(), topo, index, rec, env)
	if obs.State != VolumeStateMismatch {
		t.Fatalf("state = %q，期望 %q", obs.State, VolumeStateMismatch)
	}

	// 设备不在拓扑里 → missing（拔了、改名了）。
	obs = observeVolume(context.Background(), topo, index, testRecord(volID2, "gone", "sdz9", testUUID), env)
	if obs.State != VolumeStateMissing {
		t.Fatalf("state = %q，期望 %q", obs.State, VolumeStateMissing)
	}
}

// TestVolumeCapableNeedsAllThree 卷能力判据是“三样都在”，缺一样就不该登记查询与动作。
func TestVolumeCapableNeedsAllThree(t *testing.T) {
	dir := copyFixture(t, "storage-2404")
	store := newFakeVolumeStore()
	super := superProber{}

	if !volumeCollector(t, dir, WriteOptions{Store: store, Superblocks: super, Runner: noopRunner()}).VolumeCapable() {
		t.Error("依赖齐全却没有卷能力")
	}
	if volumeCollector(t, dir, WriteOptions{Superblocks: super, Runner: noopRunner()}).VolumeCapable() {
		t.Error("没有登记表也报告有卷能力")
	}
	if volumeCollector(t, dir, WriteOptions{Store: store, Runner: noopRunner()}).VolumeCapable() {
		t.Error("没有超级块读取器也报告有卷能力")
	}
	// 没有命令执行器：能读，但校验不了候选 fstab（findmnt），因此不能写。
	if volumeCollector(t, dir, WriteOptions{Store: store, Superblocks: super}).VolumeCapable() {
		t.Error("没有 fstab 执行器也报告有卷能力")
	}
}
