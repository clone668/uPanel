package storage

// 存储写动作判决的测试（P07.1）。
//
// 这一轮的核心不是“能不能写磁盘”，而是“该拒的都拒了、该放的都放了”。所以测试
// 用**手工构造的拓扑**（而不是 sysfs 夹具）：拓扑是判决的唯一输入，把它写成数据
// 才能一篇一篇地钉住每个阻塞项——真实夹具只用于验证扫描与保护判断本身（P04.x）。
//
// 两类断言同等重要：
//   - 拒绝类：系统盘、挂载、swap、holders、繁忙、身份不可靠、签名、对齐、越界……
//     必须给出**具体**的阻塞码，而且 permitted=false；
//   - 允许类：空白盘初始化、GPT 空闲区建分区、空白分区格式化必须能通过。
//     只写拒绝类测试的实现很容易变成“永远拒绝”，那同样是不合格。

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// ---------- 夹具 ----------

const mib = uint64(1 << 20)

// fakeSignatures 是一张“盘上有什么”的表：没列出来的设备都按空白处理。
type fakeSignatures struct {
	kinds map[string]string
	errs  map[string]error
	calls int
}

func (f *fakeSignatures) Probe(_ context.Context, devID string, _ uint64) (Signature, error) {
	f.calls++
	if f.errs != nil {
		if err, ok := f.errs[devID]; ok {
			return Signature{}, err
		}
	}
	if f.kinds != nil {
		if kind, ok := f.kinds[devID]; ok {
			return Signature{Kind: kind}, nil
		}
	}
	return Signature{Kind: SignatureBlank}, nil
}

// fakeBusy 是一张“谁打开着哪台设备 / 谁打开着哪个挂载点上的文件”的表。
type fakeBusy struct {
	procs      map[string][]BusyProcess
	errs       map[string]error
	mountProcs map[string][]BusyProcess
	mountErrs  map[string]error
	calls      int
	mountCalls int
}

func (f *fakeBusy) Scan(_ context.Context, devID string) ([]BusyProcess, error) {
	f.calls++
	if f.errs != nil {
		if err, ok := f.errs[devID]; ok {
			return nil, err
		}
	}
	return f.procs[devID], nil
}

func (f *fakeBusy) ScanMount(_ context.Context, mountPoint string) ([]BusyProcess, error) {
	f.mountCalls++
	if f.mountErrs != nil {
		if err, ok := f.mountErrs[mountPoint]; ok {
			return nil, err
		}
	}
	return f.mountProcs[mountPoint], nil
}

// testEnv 造一套“盘是空的、没人占用”的正常探针。
func testEnv() (Env, *fakeSignatures, *fakeBusy) {
	sigs := &fakeSignatures{}
	busy := &fakeBusy{}
	return Env{Signatures: sigs, Busy: busy, Dependents: noDependents}, sigs, busy
}

// blankDisk 造一块没有分区表、没有分区的整盘（初始化 GPT 的合法目标）。
func blankDisk(id string, size uint64) Device {
	return Device{
		ID: id, Name: id, Kind: KindDisk, SizeBytes: size,
		Online: true, LogicalSectorBytes: 512, PhysicalSectorBytes: 512,
		IdentityConfidence:  ConfidenceHigh,
		IdentityFingerprint: "sha256:disk-" + id,
		Serial:              strPtr("SN-" + id),
		Capabilities:        []Capability{},
		PartitionTable: &PartitionTable{
			Type:        TableNone,
			Partitions:  []Partition{},
			FreeRegions: []FreeRegion{},
			Source:      "device",
		},
		Filesystem: &Filesystem{Mounts: []Mount{}},
		Protection: Protection{Reasons: []string{}, Sources: []string{}},
	}
}

// gptDisk 造一块 GPT 盘，1 MiB 之前是分区表、末尾留 1 MiB 的备份区，
// 中间是一整块空闲区（与 parseGPT 的算法一致）。
func gptDisk(id string, size uint64) Device {
	d := blankDisk(id, size)
	d.PartitionTable = &PartitionTable{
		Type:       TableGPT,
		Partitions: []Partition{},
		FreeRegions: []FreeRegion{{
			StartBytes: mib,
			SizeBytes:  size - 2*mib,
		}},
		Source: "device",
	}
	return d
}

// addPartition 在盘上加一个分区，返回分区设备。
//
// 分区的身份可信度是 unknown、指纹是 name:sdb1 —— 这是真实扫描的形态
// （内核不给分区发序列号），判决必须能靠“整盘指纹 + 起始位置”把身份补出来。
func addPartition(disk *Device, id string, number int, start, size uint64) Device {
	disk.PartitionTable.Partitions = append(disk.PartitionTable.Partitions,
		Partition{DeviceID: id, Number: number, StartBytes: start, SizeBytes: size})
	return Device{
		ID: id, Name: id, Kind: KindPartition, SizeBytes: size,
		Online: true, LogicalSectorBytes: 512, PhysicalSectorBytes: 512,
		IdentityConfidence:  ConfidenceUnknown,
		IdentityFingerprint: "sha256:name-" + id,
		Capabilities:        []Capability{},
		Filesystem:          &Filesystem{Mounts: []Mount{}},
		Protection:          Protection{Reasons: []string{}, Sources: []string{}},
	}
}

// guardTopo 把设备装成一份快照，并按分区表推出 partition_of 边
// （真实扫描里这两样总是成对出现；分区表也可能挂在 dm 设备上，所以不限于 KindDisk），
// 额外的边用 extra 传入。
func guardTopo(version string, devs []Device, extra ...Edge) Topology {
	edges := make([]Edge, 0, len(devs)+len(extra))
	for _, d := range devs {
		if d.Kind != KindPartition {
			continue
		}
		for _, parent := range devs {
			if parent.ID == d.ID {
				continue
			}
			if _, ok := partitionEntry(&parent, d.ID); ok {
				edges = append(edges, Edge{
					FromDeviceID: d.ID, ToDeviceID: parent.ID, Relation: RelationPartitionOf,
				})
				break
			}
		}
	}
	edges = append(edges, extra...)
	return Topology{
		Status:          StatusOK,
		SnapshotVersion: version,
		IntervalMS:      5000,
		Nodes:           devs,
		Edges:           edges,
		NetworkMounts:   []NetworkMount{},
	}
}

// mustAllow 断言判决放行，并且没有阻塞项。
func mustAllow(t *testing.T, a Assessment) {
	t.Helper()
	if !a.Allowed || len(a.Blockers) != 0 {
		t.Fatalf("期望放行，实际 allowed=%v blockers=%+v", a.Allowed, a.Blockers)
	}
}

// mustBlock 断言判决拒绝，并且给出的阻塞码里有 want。
func mustBlock(t *testing.T, a Assessment, want string) {
	t.Helper()
	if a.Allowed {
		t.Fatalf("期望拒绝（%s），实际放行了", want)
	}
	if !hasBlocker(a.Blockers, want) {
		t.Fatalf("期望阻塞码 %s，实际 %+v", want, a.Blockers)
	}
}

func hasBlocker(list []Blocker, code string) bool {
	for _, b := range list {
		if b.Code == code {
			return true
		}
	}
	return false
}

func blockerMessage(a Assessment, code string) string {
	for _, b := range a.Blockers {
		if b.Code == code {
			return b.Message
		}
	}
	return ""
}

// ---------- 初始化 GPT ----------

// TestAssessInitializeBlankDisk 校验空白盘可以初始化，并且计划里带着
// 必须让人看见的影响（PLAN.md 第 9.2 节）与整盘锁。
func TestAssessInitializeBlankDisk(t *testing.T) {
	env, _, _ := testEnv()
	sdb := blankDisk("sdb", 8<<30)
	topo := guardTopo("sha256:v1", []Device{sdb})

	a, err := Assess(context.Background(), topo, ActionPartitionTableInitialize,
		PartitionTableInitializeParams{DeviceID: "sdb", TableType: TableTypeGPT}, env)
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	mustAllow(t, a)

	if got := strings.Join(a.Locks, ","); got != "disk:sdb" {
		t.Errorf("锁 = %q，整盘锁必须是 disk:sdb", got)
	}
	if a.StateVersion != "sha256:v1" {
		t.Errorf("StateVersion = %q，必须绑定快照版本", a.StateVersion)
	}
	if a.IdentityFingerprint == "" {
		t.Error("身份指纹为空：预检无法在执行前复核目标")
	}
	if !strings.Contains(a.DisplayName, "SN-sdb") {
		t.Errorf("DisplayName = %q，应带上序列号便于核对", a.DisplayName)
	}
	var hasLoss, hasWarningText bool
	for _, i := range a.Impacts {
		if i.Kind == ImpactDataLoss {
			hasLoss = true
			// 这句是 PLAN.md 第 9.2 节点名要求出现在初始化页面上的话。
			if strings.Contains(i.Description, "未检测到已知签名不代表盘上绝无原始数据") {
				hasWarningText = true
			}
		}
	}
	if !hasLoss {
		t.Error("初始化没有 data_loss 影响")
	}
	if !hasWarningText {
		t.Error("初始化影响里缺少“未检测到已知签名不代表盘上绝无原始数据”这句话")
	}
}

// TestAssessInitializeRefusesExistingTable 校验已有分区表的盘一律拒绝。
func TestAssessInitializeRefusesExistingTable(t *testing.T) {
	cases := []struct {
		name string
		dev  Device
		want string
	}{
		{"GPT", gptDisk("sdb", 8<<30), NoticeTablePresent},
		{"MBR", func() Device {
			d := blankDisk("sdb", 8<<30)
			d.PartitionTable.Type = TableMBR
			return d
		}(), NoticeTablePresent},
		{"判不出类型", func() Device {
			d := blankDisk("sdb", 8<<30)
			d.PartitionTable.Type = TableUnknown
			return d
		}(), NoticeTableUnknown},
		{"只拿到内核分区列表", func() Device {
			d := blankDisk("sdb", 8<<30)
			d.PartitionTable.Source = "kernel"
			return d
		}(), NoticeTableUnreadable},
		{"盘在休眠没读", func() Device {
			d := blankDisk("sdb", 8<<30)
			d.PartitionTable.Source = "skipped"
			return d
		}(), NoticeTableUnreadable},
		{"内核还认得分区", func() Device {
			d := blankDisk("sdb", 8<<30)
			d.PartitionTable.Partitions = []Partition{{DeviceID: "sdb1", Number: 1, StartBytes: mib, SizeBytes: mib}}
			return d
		}(), NoticeTablePresent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, _, _ := testEnv()
			topo := guardTopo("sha256:v1", []Device{tc.dev})
			a, err := Assess(context.Background(), topo, ActionPartitionTableInitialize,
				PartitionTableInitializeParams{DeviceID: "sdb", TableType: TableTypeGPT}, env)
			if err != nil {
				t.Fatalf("判决失败: %v", err)
			}
			mustBlock(t, a, tc.want)
		})
	}
}

// TestAssessInitializeRefusesSignature 校验“首扇区全零但盘上有文件系统”的盘被拒绝。
// 这是 init 最危险的一种误判：直接对整盘 mkfs 过的盘，LBA0 是全零的。
func TestAssessInitializeRefusesSignature(t *testing.T) {
	for _, kind := range []string{SignatureExt, SignatureXFS, SignatureBtrfs, SignatureSwap,
		SignatureLUKS, SignatureLVM2, SignatureMD, SignatureUnknown} {
		t.Run(kind, func(t *testing.T) {
			env, sigs, _ := testEnv()
			sigs.kinds = map[string]string{"sdb": kind}
			topo := guardTopo("sha256:v1", []Device{blankDisk("sdb", 8<<30)})
			a, err := Assess(context.Background(), topo, ActionPartitionTableInitialize,
				PartitionTableInitializeParams{DeviceID: "sdb", TableType: TableTypeGPT}, env)
			if err != nil {
				t.Fatalf("判决失败: %v", err)
			}
			mustBlock(t, a, NoticeSignaturePresent)
		})
	}
}

// TestAssessFailClosedWithoutProbes 校验探针缺失时是拒绝而不是放行：
// 读不到签名就证明不了盘是空的，看不见占用就说明不了没人用。
func TestAssessFailClosedWithoutProbes(t *testing.T) {
	topo := guardTopo("sha256:v1", []Device{blankDisk("sdb", 8<<30)})
	a, err := Assess(context.Background(), topo, ActionPartitionTableInitialize,
		PartitionTableInitializeParams{DeviceID: "sdb", TableType: TableTypeGPT}, Env{})
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	mustBlock(t, a, NoticeSignatureUnreadable)
	mustBlock(t, a, NoticeBusy)
}

// TestAssessSignatureReadError 校验读签名失败时拒绝（不是当成空白）。
func TestAssessSignatureReadError(t *testing.T) {
	env, sigs, _ := testEnv()
	sigs.errs = map[string]error{"sdb": errors.New("设备在休眠")}
	topo := guardTopo("sha256:v1", []Device{blankDisk("sdb", 8<<30)})
	a, err := Assess(context.Background(), topo, ActionPartitionTableInitialize,
		PartitionTableInitializeParams{DeviceID: "sdb", TableType: TableTypeGPT}, env)
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	mustBlock(t, a, NoticeSignatureUnreadable)
	if msg := blockerMessage(a, NoticeSignatureUnreadable); !strings.Contains(msg, "休眠") {
		t.Errorf("阻塞说明应带上原始原因：%q", msg)
	}
}

// TestAssessInitializeRejectsBadTableType 校验表类型不在闭集时拒绝。
func TestAssessInitializeRejectsBadTableType(t *testing.T) {
	env, _, _ := testEnv()
	topo := guardTopo("sha256:v1", []Device{blankDisk("sdb", 8<<30)})
	a, err := Assess(context.Background(), topo, ActionPartitionTableInitialize,
		PartitionTableInitializeParams{DeviceID: "sdb", TableType: "mbr"}, env)
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	mustBlock(t, a, NoticeTableUnknown)
}

// ---------- 系统盘保护 ----------

// TestAssessSystemDisk 校验受保护的盘以及它上面的分区都被拒绝。
func TestAssessSystemDisk(t *testing.T) {
	env, _, _ := testEnv()
	sda := gptDisk("sda", 40<<30)
	sda1 := addPartition(&sda, "sda1", 1, mib, 39<<30)
	sda.Protection = Protection{
		SystemDisk: true,
		Reasons:    []string{"承载根文件系统 /"},
		Sources:    []string{"sda1"},
	}
	topo := guardTopo("sha256:v1", []Device{sda, sda1})

	// 整盘：初始化与建分区都要拒。
	for _, tc := range []struct {
		action string
		params any
	}{
		{ActionPartitionTableInitialize, PartitionTableInitializeParams{DeviceID: "sda", TableType: TableTypeGPT}},
		{ActionPartitionCreate, PartitionCreateParams{DeviceID: "sda", StartBytes: 2 * mib, SizeBytes: 8 * mib}},
	} {
		a, err := Assess(context.Background(), topo, tc.action, tc.params, env)
		if err != nil {
			t.Fatalf("%s: 判决失败: %v", tc.action, err)
		}
		mustBlock(t, a, NoticeSystemDisk)
	}

	// 分区：格式化与删除也要拒，且锁仍是整盘。
	for _, tc := range []struct {
		action string
		params any
	}{
		{ActionPartitionDelete, PartitionDeleteParams{DeviceID: "sda1"}},
		{ActionFilesystemFormat, FilesystemFormatParams{DeviceID: "sda1", Filesystem: FilesystemExt4}},
	} {
		a, err := Assess(context.Background(), topo, tc.action, tc.params, env)
		if err != nil {
			t.Fatalf("%s: 判决失败: %v", tc.action, err)
		}
		mustBlock(t, a, NoticeSystemDisk)
		if got := strings.Join(a.Locks, ","); got != "disk:sda" {
			t.Errorf("%s: 锁 = %q，分区动作也必须锁整盘", tc.action, got)
		}
	}
}

// TestAssessRecursiveProtectionThroughStack 校验保护会沿依赖递归：
// 根在 LVM 上时，LVM 里的分区、LVM 卷、它的 PV 分区、物理盘**全部**受保护。
// 只看目标自己的 Protection 字段是不够的——分区的 Protection 通常是空的。
func TestAssessRecursiveProtectionThroughStack(t *testing.T) {
	env, _, _ := testEnv()
	sda := gptDisk("sda", 40<<30)
	// 根分区 sda3 被 dm 层消费 → 受保护，然后沿边向上把 sda 也带上。
	sda3 := addPartition(&sda, "sda3", 3, 20<<30, 10<<30)
	sda3.Protection = Protection{SystemDisk: true, Reasons: []string{"承载根文件系统 /"}, Sources: []string{"sda3"}}
	sda.Protection = Protection{SystemDisk: true, Reasons: []string{"承载根文件系统 /"}, Sources: []string{"sda3"}}
	sda.Holders = []string{"dm-0"}

	dm := Device{
		ID: "dm-0", Name: "dm-0", Kind: KindLVM, SizeBytes: 10 << 30,
		Online: true, IdentityConfidence: ConfidenceMedium,
		IdentityFingerprint: "sha256:dm0",
		PartitionTable: &PartitionTable{
			Type: TableGPT, Partitions: []Partition{}, FreeRegions: []FreeRegion{}, Source: "device",
		},
		Capabilities: []Capability{},
		Protection:   Protection{Reasons: []string{}, Sources: []string{}},
	}
	// dm-0 上面还有一个分区：这才是最容易“看着像个普通分区”的目标。
	dmP1 := addPartition(&dm, "dm-0p1", 1, mib, 4<<30)
	topo := guardTopo("sha256:v1", []Device{sda, sda3, dm, dmP1},
		Edge{FromDeviceID: "dm-0", ToDeviceID: "sda3", Relation: RelationDependsOn})

	a, err := Assess(context.Background(), topo, ActionFilesystemFormat,
		FilesystemFormatParams{DeviceID: "dm-0p1", Filesystem: FilesystemExt4}, env)
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	mustBlock(t, a, NoticeSystemDisk)
	// 目标自己在 LVM 卷之上，本阶段不支持在它上面写。
	mustBlock(t, a, NoticeStackedDevice)
	if got := strings.Join(a.DiskIDs, ","); got != "sda" {
		t.Errorf("DiskIDs = %q，应沿依赖追到物理盘 sda", got)
	}
	if got := strings.Join(a.Locks, ","); got != "disk:sda" {
		t.Errorf("锁 = %q，应沿依赖锁到物理盘 sda 而不是 dm-0", got)
	}
}

// ---------- 占用：挂载 / swap / holders / 繁忙 ----------

// TestAssessRefusesOccupiedDevice 校验挂载、交换区、holders、繁忙四类占用。
func TestAssessRefusesOccupiedDevice(t *testing.T) {
	t.Run("已挂载", func(t *testing.T) {
		env, _, _ := testEnv()
		sdb := gptDisk("sdb", 8<<30)
		sdb1 := addPartition(&sdb, "sdb1", 1, mib, 4<<30)
		sdb1.Filesystem = &Filesystem{Type: strPtr("ext4"), Mounts: []Mount{{Path: "/data", Source: "/dev/sdb1", FSType: "ext4", Options: []string{"rw"}}}}
		topo := guardTopo("sha256:v1", []Device{sdb, sdb1})

		a, err := Assess(context.Background(), topo, ActionFilesystemFormat,
			FilesystemFormatParams{DeviceID: "sdb1", Filesystem: FilesystemExt4}, env)
		if err != nil {
			t.Fatalf("判决失败: %v", err)
		}
		mustBlock(t, a, NoticeDeviceMounted)
		if msg := blockerMessage(a, NoticeDeviceMounted); !strings.Contains(msg, "/data") {
			t.Errorf("阻塞说明应带上挂载点：%q", msg)
		}
	})

	t.Run("活动交换区", func(t *testing.T) {
		env, _, _ := testEnv()
		sdb := gptDisk("sdb", 8<<30)
		sdb1 := addPartition(&sdb, "sdb1", 1, mib, 2<<30)
		sdb1.Swap = &SwapUsage{Type: "partition", SizeBytes: 2 << 30, UsedBytes: 1 << 30}
		topo := guardTopo("sha256:v1", []Device{sdb, sdb1})

		a, err := Assess(context.Background(), topo, ActionPartitionDelete,
			PartitionDeleteParams{DeviceID: "sdb1"}, env)
		if err != nil {
			t.Fatalf("判决失败: %v", err)
		}
		mustBlock(t, a, NoticeSwapActive)
	})

	t.Run("有 holders", func(t *testing.T) {
		env, _, _ := testEnv()
		sdb := gptDisk("sdb", 8<<30)
		sdb.Holders = []string{"md0"}
		topo := guardTopo("sha256:v1", []Device{sdb})

		a, err := Assess(context.Background(), topo, ActionPartitionCreate,
			PartitionCreateParams{DeviceID: "sdb", StartBytes: mib, SizeBytes: mib}, env)
		if err != nil {
			t.Fatalf("判决失败: %v", err)
		}
		mustBlock(t, a, NoticeHasHolders)
	})

	t.Run("有进程打开", func(t *testing.T) {
		env, _, busy := testEnv()
		busy.procs = map[string][]BusyProcess{"sdb": {{PID: "4321", Comm: "dd"}}}
		topo := guardTopo("sha256:v1", []Device{blankDisk("sdb", 8<<30)})

		a, err := Assess(context.Background(), topo, ActionPartitionTableInitialize,
			PartitionTableInitializeParams{DeviceID: "sdb", TableType: TableTypeGPT}, env)
		if err != nil {
			t.Fatalf("判决失败: %v", err)
		}
		mustBlock(t, a, NoticeBusy)
		if msg := blockerMessage(a, NoticeBusy); !strings.Contains(msg, "4321/dd") {
			t.Errorf("阻塞说明应带上进程：%q", msg)
		}
	})

	t.Run("占用检查失败", func(t *testing.T) {
		env, _, busy := testEnv()
		busy.errs = map[string]error{"sdb": errors.New("权限不足")}
		topo := guardTopo("sha256:v1", []Device{blankDisk("sdb", 8<<30)})

		a, err := Assess(context.Background(), topo, ActionPartitionTableInitialize,
			PartitionTableInitializeParams{DeviceID: "sdb", TableType: TableTypeGPT}, env)
		if err != nil {
			t.Fatalf("判决失败: %v", err)
		}
		mustBlock(t, a, NoticeBusy)
	})

	// 分区动作也要查整盘：分区表的写入落在整盘上，整盘被人打开着同样不能动。
	t.Run("整盘被打开时分区动作也拒", func(t *testing.T) {
		env, _, busy := testEnv()
		busy.procs = map[string][]BusyProcess{"sdb": {{PID: "77", Comm: "smartctl"}}}
		sdb := gptDisk("sdb", 8<<30)
		sdb1 := addPartition(&sdb, "sdb1", 1, mib, 2<<30)
		topo := guardTopo("sha256:v1", []Device{sdb, sdb1})

		a, err := Assess(context.Background(), topo, ActionFilesystemFormat,
			FilesystemFormatParams{DeviceID: "sdb1", Filesystem: FilesystemExt4}, env)
		if err != nil {
			t.Fatalf("判决失败: %v", err)
		}
		mustBlock(t, a, NoticeBusy)
	})
}

// ---------- 设备可用性与类型 ----------

// TestAssessDeviceState 校验离线与只读设备被拒绝。
func TestAssessDeviceState(t *testing.T) {
	for name, mutate := range map[string]func(*Device){
		"离线": func(d *Device) { d.Online = false },
		"只读": func(d *Device) { d.ReadOnly = true },
	} {
		t.Run(name, func(t *testing.T) {
			env, _, _ := testEnv()
			dev := blankDisk("sdb", 8<<30)
			mutate(&dev)
			topo := guardTopo("sha256:v1", []Device{dev})
			a, err := Assess(context.Background(), topo, ActionPartitionTableInitialize,
				PartitionTableInitializeParams{DeviceID: "sdb", TableType: TableTypeGPT}, env)
			if err != nil {
				t.Fatalf("判决失败: %v", err)
			}
			if name == "离线" {
				mustBlock(t, a, NoticeOffline)
			} else {
				mustBlock(t, a, NoticeReadOnly)
			}
		})
	}
}

// TestAssessUnsupportedKind 校验动作与设备类型不匹配时给出 UNSUPPORTED。
func TestAssessUnsupportedKind(t *testing.T) {
	env, _, _ := testEnv()
	sdb := gptDisk("sdb", 8<<30)
	sdb1 := addPartition(&sdb, "sdb1", 1, mib, 2<<30)
	topo := guardTopo("sha256:v1", []Device{sdb, sdb1})

	// 对整盘格式化：不做（PLAN.md 第 9.1 节禁止整盘 mkfs）。
	a, err := Assess(context.Background(), topo, ActionFilesystemFormat,
		FilesystemFormatParams{DeviceID: "sdb", Filesystem: FilesystemExt4}, env)
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	mustBlock(t, a, NoticeUnsupportedKind)

	// 对分区初始化分区表：不做。
	a, err = Assess(context.Background(), topo, ActionPartitionTableInitialize,
		PartitionTableInitializeParams{DeviceID: "sdb1", TableType: TableTypeGPT}, env)
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	mustBlock(t, a, NoticeUnsupportedKind)
}

// TestAssessDeviceNotFound 校验目标不在拓扑里时拒绝（拔盘、改名、id 写错）。
func TestAssessDeviceNotFound(t *testing.T) {
	env, _, _ := testEnv()
	topo := guardTopo("sha256:v1", []Device{blankDisk("sdb", 8<<30)})
	a, err := Assess(context.Background(), topo, ActionPartitionTableInitialize,
		PartitionTableInitializeParams{DeviceID: "sdz", TableType: TableTypeGPT}, env)
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	mustBlock(t, a, NoticeDeviceMissing)
	if a.StateVersion == "" {
		t.Error("即使找不到设备，也必须给出状态版本（预检需要绑定它）")
	}
	// 设备不在了就没有身份可绑：指纹是空的，这是设计而不是漏填。预检层只对
	// allowed=true 要求绑定（Q190：无条件要求会把这条正确的拒绝变成 500）。
	if a.IdentityFingerprint != "" {
		t.Errorf("找不到设备时不该编造身份指纹，实际 %q", a.IdentityFingerprint)
	}
	if a.Allowed {
		t.Error("找不到设备必须判不允许")
	}
	if a.DisplayName != "sdz" {
		t.Errorf("拒绝时也要说明问的是哪个设备，实际 %q", a.DisplayName)
	}
}

// TestAssessTopologyUnavailable 校验采集失败的快照不能用来放行。
func TestAssessTopologyUnavailable(t *testing.T) {
	env, _, _ := testEnv()
	topo := Topology{
		Status:          StatusError,
		Error:           "读 /sys/block 失败",
		SnapshotVersion: "sha256:old",
		Nodes:           []Device{blankDisk("sdb", 8<<30)},
		Edges:           []Edge{},
	}
	a, err := Assess(context.Background(), topo, ActionPartitionTableInitialize,
		PartitionTableInitializeParams{DeviceID: "sdb", TableType: TableTypeGPT}, env)
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	mustBlock(t, a, NoticeTopologyUnavailable)
	if a.StateVersion != "sha256:old" {
		t.Errorf("StateVersion = %q，应保留快照版本以便对比", a.StateVersion)
	}
}

// ---------- 身份 ----------

// TestAssessIdentityUnknown 校验身份不可靠的设备不能动。
func TestAssessIdentityUnknown(t *testing.T) {
	env, _, _ := testEnv()
	sdb := gptDisk("sdb", 8<<30)
	sdb.IdentityConfidence = ConfidenceUnknown
	sdb.IdentityFingerprint = ""
	sdb1 := addPartition(&sdb, "sdb1", 1, mib, 2<<30)
	topo := guardTopo("sha256:v1", []Device{sdb, sdb1})

	a, err := Assess(context.Background(), topo, ActionFilesystemFormat,
		FilesystemFormatParams{DeviceID: "sdb1", Filesystem: FilesystemExt4}, env)
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	mustBlock(t, a, NoticeIdentityUnknown)
	if a.IdentityFingerprint == "" {
		t.Error("即使身份不可靠，指纹也不能为空：预检要求绑定一个可复核的值")
	}
}

// TestAssessIdentityLowConfidenceIsWarning 校验弱身份只是告警：
// 它不阻止执行，但必须出现在 warnings 里。
func TestAssessIdentityLowConfidenceIsWarning(t *testing.T) {
	env, _, _ := testEnv()
	sdb := blankDisk("sdb", 8<<30)
	sdb.IdentityConfidence = ConfidenceLow
	topo := guardTopo("sha256:v1", []Device{sdb})

	a, err := Assess(context.Background(), topo, ActionPartitionTableInitialize,
		PartitionTableInitializeParams{DeviceID: "sdb", TableType: TableTypeGPT}, env)
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	mustAllow(t, a)
	found := false
	for _, w := range a.Warnings {
		if w.Code == NoticeIdentityLowConfidence {
			found = true
		}
	}
	if !found {
		t.Fatalf("弱身份没有产生告警：%+v", a.Warnings)
	}
}

// TestAssessIdentityConflict 校验同一个指纹出现在两台设备上时拒绝。
func TestAssessIdentityConflict(t *testing.T) {
	env, _, _ := testEnv()
	sdb := blankDisk("sdb", 8<<30)
	sdc := blankDisk("sdc", 8<<30)
	sdc.IdentityFingerprint = sdb.IdentityFingerprint // 克隆盘 / 伪造 WWN
	topo := guardTopo("sha256:v1", []Device{sdb, sdc})

	a, err := Assess(context.Background(), topo, ActionPartitionTableInitialize,
		PartitionTableInitializeParams{DeviceID: "sdb", TableType: TableTypeGPT}, env)
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	mustBlock(t, a, NoticeIdentityConflict)
}

// TestAssessPartitionIdentityBindsDisk 校验分区的指纹由“整盘身份 + 起始位置”合成：
// 换盘、或者 1 号分区换了位置，指纹都必须变——这是“sdb1 还是 sdb1，但已经是别人的
// 分区了”这种情况的唯一防线。
func TestAssessPartitionIdentityBindsDisk(t *testing.T) {
	env, _, _ := testEnv()

	fingerprintFor := func(diskFP string, start uint64) string {
		sdb := gptDisk("sdb", 8<<30)
		sdb.IdentityFingerprint = diskFP
		sdb1 := addPartition(&sdb, "sdb1", 1, start, 2<<30)
		topo := guardTopo("sha256:v1", []Device{sdb, sdb1})
		a, err := Assess(context.Background(), topo, ActionPartitionDelete,
			PartitionDeleteParams{DeviceID: "sdb1"}, env)
		if err != nil {
			t.Fatalf("判决失败: %v", err)
		}
		return a.IdentityFingerprint
	}

	base := fingerprintFor("sha256:disk-A", mib)
	if base == "" {
		t.Fatal("分区指纹为空")
	}
	if same := fingerprintFor("sha256:disk-A", mib); same != base {
		t.Errorf("同一块盘同一位置的分区指纹不稳定：%q vs %q", base, same)
	}
	if other := fingerprintFor("sha256:disk-B", mib); other == base {
		t.Error("换了盘，分区指纹却没变")
	}
	if moved := fingerprintFor("sha256:disk-A", 4*mib); moved == base {
		t.Error("分区起点变了，指纹却没变")
	}
}

// ---------- 建分区 ----------

// TestAssessCreateRegion 校验区间校验：对齐、最小尺寸、不越界、不重叠。
func TestAssessCreateRegion(t *testing.T) {
	cases := []struct {
		name  string
		start uint64
		size  uint64
		want  string
	}{
		{"合法", 2 * mib, 4 * mib, ""},
		{"起点没对齐", mib + 512, mib, NoticeRegionUnaligned},
		{"大小没对齐", 2 * mib, mib + 512, NoticeRegionUnaligned},
		{"太小", 2 * mib, 512, NoticeRegionTooSmall},
		{"零大小", 2 * mib, 0, NoticeRegionTooSmall},
		{"超出空闲区", 2 * mib, 8 << 30, NoticeRegionNotFree},
		{"落在分区表区", 0, mib, NoticeRegionNotFree},
		{"整数溢出", ^uint64(0) - 512, 4096, NoticeRegionNotFree},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, _, _ := testEnv()
			sdb := gptDisk("sdb", 8<<30)
			topo := guardTopo("sha256:v1", []Device{sdb})
			a, err := Assess(context.Background(), topo, ActionPartitionCreate,
				PartitionCreateParams{DeviceID: "sdb", StartBytes: tc.start, SizeBytes: tc.size, Name: "data"}, env)
			if err != nil {
				t.Fatalf("判决失败: %v", err)
			}
			if tc.want == "" {
				mustAllow(t, a)
				return
			}
			mustBlock(t, a, tc.want)
		})
	}
}

// TestAssessCreateRefusesNonGPT 校验 MBR 盘上不建分区（初期不修改 MBR 表）。
func TestAssessCreateRefusesNonGPT(t *testing.T) {
	env, _, _ := testEnv()
	d := blankDisk("sdb", 8<<30)
	d.PartitionTable.Type = TableMBR
	topo := guardTopo("sha256:v1", []Device{d})
	a, err := Assess(context.Background(), topo, ActionPartitionCreate,
		PartitionCreateParams{DeviceID: "sdb", StartBytes: mib, SizeBytes: mib}, env)
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	mustBlock(t, a, NoticeTableNotGPT)
}

// TestAssessCreateNoFreeRegion 校验没有空闲区时拒绝。
func TestAssessCreateNoFreeRegion(t *testing.T) {
	env, _, _ := testEnv()
	d := gptDisk("sdb", 8<<30)
	d.PartitionTable.FreeRegions = []FreeRegion{}
	d.PartitionTable.Partitions = []Partition{{DeviceID: "sdb1", Number: 1, StartBytes: mib, SizeBytes: 8<<30 - 2*mib}}
	topo := guardTopo("sha256:v1", []Device{d})
	a, err := Assess(context.Background(), topo, ActionPartitionCreate,
		PartitionCreateParams{DeviceID: "sdb", StartBytes: mib, SizeBytes: mib}, env)
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	mustBlock(t, a, NoticeNoFreeRegion)
	mustBlock(t, a, NoticeRegionNotFree)
}

// TestAssessCreateNameInvalid 校验分区名太长被拒。
func TestAssessCreateNameInvalid(t *testing.T) {
	env, _, _ := testEnv()
	topo := guardTopo("sha256:v1", []Device{gptDisk("sdb", 8<<30)})
	a, err := Assess(context.Background(), topo, ActionPartitionCreate,
		PartitionCreateParams{DeviceID: "sdb", StartBytes: mib, SizeBytes: mib,
			Name: strings.Repeat("x", 37)}, env)
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	mustBlock(t, a, NoticeLabelInvalid)
}

// ---------- 删除与格式化 ----------

// TestAssessDeletePartition 校验空白分区可以删除，有数据时告警，属于别的层时拒绝。
func TestAssessDeletePartition(t *testing.T) {
	cases := []struct {
		name      string
		signature string
		want      string
	}{
		{"空白分区", SignatureBlank, ""},
		{"已有 ext4", SignatureExt, ""},
		{"LUKS 加密头", SignatureLUKS, NoticeSignaturePresent},
		{"LVM2 物理卷", SignatureLVM2, NoticeSignaturePresent},
		{"md 成员", SignatureMD, NoticeSignaturePresent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, sigs, _ := testEnv()
			sigs.kinds = map[string]string{"sdb1": tc.signature}
			sdb := gptDisk("sdb", 8<<30)
			sdb1 := addPartition(&sdb, "sdb1", 1, mib, 2<<30)
			topo := guardTopo("sha256:v1", []Device{sdb, sdb1})

			a, err := Assess(context.Background(), topo, ActionPartitionDelete,
				PartitionDeleteParams{DeviceID: "sdb1"}, env)
			if err != nil {
				t.Fatalf("判决失败: %v", err)
			}
			if tc.want == "" {
				mustAllow(t, a)
				return
			}
			mustBlock(t, a, tc.want)
		})
	}
}

// TestAssessDeleteRefusesMBRParent 校验 MBR 盘上的分区不能删。
func TestAssessDeleteRefusesMBRParent(t *testing.T) {
	env, _, _ := testEnv()
	d := blankDisk("sdb", 8<<30)
	d.PartitionTable.Type = TableMBR
	d.PartitionTable.Partitions = []Partition{{DeviceID: "sdb1", Number: 1, StartBytes: mib, SizeBytes: 2 << 30}}
	sdb1 := addPartition(&d, "sdb1", 1, mib, 2<<30)
	// addPartition 会再追加一条同名分区，去掉重复的那条（保持夹具与真实一致）。
	d.PartitionTable.Partitions = d.PartitionTable.Partitions[:1]
	topo := guardTopo("sha256:v1", []Device{d, sdb1})

	a, err := Assess(context.Background(), topo, ActionPartitionDelete,
		PartitionDeleteParams{DeviceID: "sdb1"}, env)
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	mustBlock(t, a, NoticeTableNotGPT)
}

// TestAssessDeleteMissingPartition 校验分区表里没有这个分区时拒绝。
func TestAssessDeleteMissingPartition(t *testing.T) {
	env, _, _ := testEnv()
	sdb := gptDisk("sdb", 8<<30)
	// 拓扑里有 sdb1 这个节点、也知道它属于 sdb，但整盘的分区列表里没有它
	// （分区表刚被别的东西改过）。这种情况下必须拒绝，不能按旧信息删。
	sdb1 := Device{
		ID: "sdb1", Name: "sdb1", Kind: KindPartition, SizeBytes: 2 << 30,
		Online: true, IdentityConfidence: ConfidenceUnknown,
		IdentityFingerprint: "sha256:name-sdb1",
		Capabilities:        []Capability{},
		Protection:          Protection{Reasons: []string{}, Sources: []string{}},
	}
	topo := guardTopo("sha256:v1", []Device{sdb, sdb1},
		Edge{FromDeviceID: "sdb1", ToDeviceID: "sdb", Relation: RelationPartitionOf})

	a, err := Assess(context.Background(), topo, ActionPartitionDelete,
		PartitionDeleteParams{DeviceID: "sdb1"}, env)
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	mustBlock(t, a, NoticePartitionMissing)
}

// TestAssessParentUnknown 校验父盘查不出来时拒绝（锁不住整盘就不能动分区）。
func TestAssessParentUnknown(t *testing.T) {
	env, _, _ := testEnv()
	sdb1 := Device{
		ID: "sdb1", Name: "sdb1", Kind: KindPartition, SizeBytes: 2 << 30,
		Online: true, IdentityConfidence: ConfidenceUnknown,
		IdentityFingerprint: "sha256:name-sdb1",
		Capabilities:        []Capability{},
		Protection:          Protection{Reasons: []string{}, Sources: []string{}},
	}
	topo := guardTopo("sha256:v1", []Device{sdb1})

	a, err := Assess(context.Background(), topo, ActionPartitionDelete,
		PartitionDeleteParams{DeviceID: "sdb1"}, env)
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	mustBlock(t, a, NoticeParentUnknown)
	if len(a.Locks) != 0 {
		t.Errorf("锁 = %v，父盘不明时不应给出任何锁", a.Locks)
	}
}

// TestAssessFormat 校验格式化的三种结果：空白分区允许、ext4 允许但告警、
// 标签超长拒绝。
func TestAssessFormat(t *testing.T) {
	t.Run("空白分区允许", func(t *testing.T) {
		env, _, _ := testEnv()
		sdb := gptDisk("sdb", 8<<30)
		sdb1 := addPartition(&sdb, "sdb1", 1, mib, 2<<30)
		topo := guardTopo("sha256:v1", []Device{sdb, sdb1})

		a, err := Assess(context.Background(), topo, ActionFilesystemFormat,
			FilesystemFormatParams{DeviceID: "sdb1", Filesystem: FilesystemExt4, Label: "data"}, env)
		if err != nil {
			t.Fatalf("判决失败: %v", err)
		}
		mustAllow(t, a)
		if len(a.Impacts) == 0 {
			t.Error("格式化没有给出任何影响")
		}
	})

	t.Run("已有数据要告警", func(t *testing.T) {
		env, sigs, _ := testEnv()
		sigs.kinds = map[string]string{"sdb1": SignatureXFS}
		sdb := gptDisk("sdb", 8<<30)
		sdb1 := addPartition(&sdb, "sdb1", 1, mib, 2<<30)
		topo := guardTopo("sha256:v1", []Device{sdb, sdb1})

		a, err := Assess(context.Background(), topo, ActionFilesystemFormat,
			FilesystemFormatParams{DeviceID: "sdb1", Filesystem: FilesystemExt4}, env)
		if err != nil {
			t.Fatalf("判决失败: %v", err)
		}
		mustAllow(t, a)
		found := false
		for _, w := range a.Warnings {
			if w.Code == NoticeExistingData {
				found = true
			}
		}
		if !found {
			t.Fatalf("已有数据没有告警：%+v", a.Warnings)
		}
	})

	t.Run("标签超长", func(t *testing.T) {
		env, _, _ := testEnv()
		sdb := gptDisk("sdb", 8<<30)
		sdb1 := addPartition(&sdb, "sdb1", 1, mib, 2<<30)
		topo := guardTopo("sha256:v1", []Device{sdb, sdb1})

		a, err := Assess(context.Background(), topo, ActionFilesystemFormat,
			FilesystemFormatParams{DeviceID: "sdb1", Filesystem: FilesystemXFS,
				Label: strings.Repeat("y", 13)}, env)
		if err != nil {
			t.Fatalf("判决失败: %v", err)
		}
		mustBlock(t, a, NoticeLabelInvalid)
	})

	t.Run("不支持的文件系统", func(t *testing.T) {
		env, _, _ := testEnv()
		sdb := gptDisk("sdb", 8<<30)
		sdb1 := addPartition(&sdb, "sdb1", 1, mib, 2<<30)
		topo := guardTopo("sha256:v1", []Device{sdb, sdb1})

		a, err := Assess(context.Background(), topo, ActionFilesystemFormat,
			FilesystemFormatParams{DeviceID: "sdb1", Filesystem: "btrfs"}, env)
		if err != nil {
			t.Fatalf("判决失败: %v", err)
		}
		mustBlock(t, a, NoticeUnsupportedKind)
	})
}

// ---------- 参数与工具函数 ----------

// TestAssessRejectsUnknownAction 校验动作名不认识时报错（编程错误，不是业务拒绝）。
func TestAssessRejectsUnknownAction(t *testing.T) {
	env, _, _ := testEnv()
	topo := guardTopo("sha256:v1", []Device{blankDisk("sdb", 8<<30)})
	if _, err := Assess(context.Background(), topo, "storage.nuke", nil, env); err == nil {
		t.Fatal("未知动作没有报错")
	}
	if _, err := Assess(context.Background(), topo, ActionPartitionCreate,
		PartitionTableInitializeParams{DeviceID: "sdb"}, env); err == nil {
		t.Fatal("参数类型不匹配没有报错")
	}
}

// TestRegionInsideFree 校验区间包含判断（含整数溢出的边界）。
func TestRegionInsideFree(t *testing.T) {
	free := []FreeRegion{{StartBytes: mib, SizeBytes: 4 * mib}}
	cases := []struct {
		start, size uint64
		want        bool
	}{
		{mib, 4 * mib, true},
		{mib, mib, true},
		{4 * mib, mib, true},
		{mib - 512, mib, false},
		{2 * mib, 4 * mib, false},
		{^uint64(0) - 1024, 4096, false},
		{mib, 0, false},
	}
	for _, tc := range cases {
		if got := regionInsideFree(free, tc.start, tc.size); got != tc.want {
			t.Errorf("regionInsideFree(start=%d, size=%d) = %v，期望 %v",
				tc.start, tc.size, got, tc.want)
		}
	}
}

// TestValidateLabel 校验标签规则按文件系统分开：ext4 16 字节、xfs 12 字节。
func TestValidateLabel(t *testing.T) {
	if err := ValidateLabel(FilesystemExt4, strings.Repeat("a", 16)); err != nil {
		t.Errorf("ext4 16 字节标签被拒: %v", err)
	}
	if err := ValidateLabel(FilesystemExt4, strings.Repeat("a", 17)); err == nil {
		t.Error("ext4 17 字节标签被接受")
	}
	if err := ValidateLabel(FilesystemXFS, strings.Repeat("a", 13)); err == nil {
		t.Error("xfs 13 字节标签被接受")
	}
	if err := ValidateLabel(FilesystemExt4, "has/slash"); err == nil {
		t.Error("标签里的 / 被接受")
	}
	if err := ValidateLabel(FilesystemExt4, "with space"); err != nil {
		t.Errorf("空格是合法盘标，被拒了: %v", err)
	}
	if err := ValidateLabel(FilesystemExt4, ""); err != nil {
		t.Errorf("空标签应允许: %v", err)
	}
}
