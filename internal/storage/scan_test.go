package storage

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"upanel/internal/testsupport"
)

// 这些用例跑的是**真实采集的夹具**（tests/fixtures/storage-2404、storage-2204），
// 不是手写的 sysfs 树。理由：手写的树会把“我以为内核长这样”固化成测试前提，
// 真机上不成立就变成假绿（DECISIONS Q85）。两份夹具分别来自 24.04 与 22.04 测试机。

// fixedNow 让所有采集时间可断言。
var fixedNow = time.Date(2026, 10, 20, 17, 40, 0, 0, time.UTC)

// fixtureDiskReader 从夹具目录的 disks/<name>.bin 读盘头部，替代真实的 /dev。
// 夹具里没有 <name>.bin 就当成“读失败”，与真实环境里盘不在的表现一致。
type fixtureDiskReader struct {
	dir string
	// reads 记录被读过的设备，用来断言“没有去唤醒不该读的盘”。
	reads []string
}

func (f *fixtureDiskReader) ReadDisk(_ context.Context, name string) ([]byte, error) {
	f.reads = append(f.reads, name)
	raw, err := os.ReadFile(filepath.Join(f.dir, name+".bin"))
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// fixtureDir 返回某个存储夹具的绝对路径。
func fixtureDir(name string) (string, error) {
	return testsupport.FixtureDir(name)
}

// fixtureRoot 组装一个指向夹具的 Root。
func fixtureRoot(t *testing.T, name string) (Root, *fixtureDiskReader, string) {
	t.Helper()
	dir, err := testsupport.FixtureDir(name)
	if err != nil {
		t.Fatalf("定位夹具失败: %v", err)
	}
	dr := &fixtureDiskReader{dir: filepath.Join(dir, "disks")}
	root := Root{
		Sys:         filepath.Join(dir, "sys"),
		Proc:        filepath.Join(dir, "proc"),
		Disks:       dr,
		Caps:        nil, // 夹具里没有真实文件系统，容量一律 null
		Now:         func() time.Time { return fixedNow },
		ReadTimeout: time.Second,
	}
	return root, dr, dir
}

func scanFixture(t *testing.T, name string) (*Topology, *fixtureDiskReader) {
	t.Helper()
	root, dr, _ := fixtureRoot(t, name)
	topo, err := Scan(context.Background(), root)
	if err != nil {
		t.Fatalf("Scan(%s) 失败: %v", name, err)
	}
	return topo, dr
}

func deviceByID(t *testing.T, topo *Topology, id string) Device {
	t.Helper()
	for _, d := range topo.Nodes {
		if d.ID == id {
			return d
		}
	}
	t.Fatalf("拓扑里没有设备 %q（现有：%v）", id, nodeIDs(topo))
	return Device{}
}

func nodeIDs(topo *Topology) []string {
	ids := make([]string, 0, len(topo.Nodes))
	for _, d := range topo.Nodes {
		ids = append(ids, d.ID)
	}
	return ids
}

func hasEdge(topo *Topology, from, to string) bool {
	for _, e := range topo.Edges {
		if e.FromDeviceID == from && e.ToDeviceID == to {
			return true
		}
	}
	return false
}

// TestScanRealCapture2404 用 24.04 测试机的真实采集跑一遍，逐项核对
// MANIFEST.txt 里 lsblk 记录的容量、类型、挂载。
func TestScanRealCapture2404(t *testing.T) {
	topo, dr := scanFixture(t, "storage-2404")

	if topo.Status != StatusOK {
		t.Fatalf("Status = %q，想要 %q", topo.Status, StatusOK)
	}
	if !topo.CollectedAt.Equal(fixedNow) {
		t.Errorf("CollectedAt = %v，想要 %v", topo.CollectedAt, fixedNow)
	}
	if topo.SnapshotVersion == "" || topo.SnapshotVersion[:7] != "sha256:" {
		t.Errorf("SnapshotVersion = %q，想要 sha256: 前缀", topo.SnapshotVersion)
	}

	// 四个数据/系统盘 + fd0，sda 上四个分区。loop 设备在 24.04 上有 8 个。
	for _, id := range []string{"sda", "sda1", "sda14", "sda15", "sda16", "sdb", "sdc", "sdd", "fd0"} {
		deviceByID(t, topo, id)
	}

	cases := []struct {
		id     string
		kind   string
		size   uint64
		mount  string
		fsType string
	}{
		{"sda", KindDisk, 42949672960, "", ""},
		{"sda1", KindPartition, 41874865664, "/", "ext4"},
		{"sda14", KindPartition, 4194304, "", ""},
		{"sda15", KindPartition, 111149056, "/boot/efi", "vfat"},
		{"sda16", KindPartition, 957350400, "/boot", "ext4"},
		{"sdb", KindDisk, 8589934592, "", ""},
		{"sdc", KindDisk, 4294967296, "", ""},
		{"sdd", KindDisk, 4294967296, "", ""},
	}
	for _, c := range cases {
		dev := deviceByID(t, topo, c.id)
		if dev.Kind != c.kind {
			t.Errorf("%s: Kind = %q，想要 %q", c.id, dev.Kind, c.kind)
		}
		if dev.SizeBytes != c.size {
			t.Errorf("%s: SizeBytes = %d，想要 %d", c.id, dev.SizeBytes, c.size)
		}
		if !dev.Online {
			t.Errorf("%s: Online = false，真机上这块盘是在线的", c.id)
		}
		if c.mount == "" {
			continue
		}
		if dev.Filesystem == nil {
			t.Errorf("%s: 有挂载点 %s，但 Filesystem 为 nil", c.id, c.mount)
			continue
		}
		found := false
		for _, m := range dev.Filesystem.Mounts {
			if m.Path == c.mount {
				found = true
				if m.FSType != c.fsType {
					t.Errorf("%s: 挂载点 %s 的 fstype = %q，想要 %q", c.id, c.mount, m.FSType, c.fsType)
				}
				// 夹具环境没有真实文件系统，容量必须是 null 而不是 0。
				if m.Capacity != nil {
					t.Errorf("%s: 挂载点 %s 的 capacity = %+v，夹具下应为 null", c.id, c.mount, m.Capacity)
				}
			}
		}
		if !found {
			t.Errorf("%s: 挂载点列表里没有 %s（实际：%+v）", c.id, c.mount, dev.Filesystem.Mounts)
		}
	}

	// fd0 是幽灵软驱：4 KiB、没有介质。不能当磁盘报成“正常可用”。
	fd := deviceByID(t, topo, "fd0")
	if fd.Kind != KindOther {
		t.Errorf("fd0: Kind = %q，想要 %q（软驱不是磁盘也不是光驱）", fd.Kind, KindOther)
	}

	// 整盘的分区表：sda 有分区，sdb/sdc/sdd 是空盘（type=none，没有分区）。
	if p := deviceByID(t, topo, "sdb").PartitionTable; p == nil {
		t.Errorf("sdb: PartitionTable 为 nil，空盘也要明确报 none")
	} else if p.Type != TableNone || len(p.Partitions) != 0 {
		t.Errorf("sdb: 空盘的分区表 = %+v，想要 type=none 且没有分区", p)
	}

	// 读过的盘必须恰好是有分区表的那几块 + 空盘（都要判断类型），
	// 但不能包含 fd0/loop：那会为了拿信息去打扰一个没有介质的设备。
	for _, name := range dr.reads {
		switch name {
		case "sda", "sdb", "sdc", "sdd":
		default:
			t.Errorf("不该读 /dev/%s 的头部（会唤醒/打扰设备），读了", name)
		}
	}
}

// TestProtectionSystemDisk2404 是系统盘保护的核心用例：
// 根在 sda1 上 → sda1 受保护 → 沿 partition_of 上溯 → sda 整盘受保护；
// sdb/sdc/sdd 是空白数据盘 → 不受保护。
func TestProtectionSystemDisk2404(t *testing.T) {
	topo, _ := scanFixture(t, "storage-2404")

	for _, id := range []string{"sda", "sda1", "sda15", "sda16"} {
		dev := deviceByID(t, topo, id)
		if !dev.Protection.SystemDisk {
			t.Errorf("%s: SystemDisk = false，它承载根/启动文件系统，必须受保护", id)
			continue
		}
		if len(dev.Protection.Reasons) == 0 {
			t.Errorf("%s: 受保护却没有理由", id)
		}
		if len(dev.Protection.Sources) == 0 {
			t.Errorf("%s: 受保护却没有来源设备", id)
		}
	}
	for _, id := range []string{"sdb", "sdc", "sdd", "fd0"} {
		dev := deviceByID(t, topo, id)
		if dev.Protection.SystemDisk {
			t.Errorf("%s: SystemDisk = true，但它是空白数据盘（理由 %v）", id, dev.Protection.Reasons)
		}
		if dev.Protection.Reasons == nil {
			t.Errorf("%s: Reasons 为 null，约定要给空数组", id)
		}
	}

	// sda14 是 BIOS boot 分区，没有挂载点，但它在一个受保护的整盘上：
	// 上溯规则只从子往父走，所以它自己不受保护——这是刻意的，
	// 真正拦住“删 sda14”的是整盘 sda 受保护这一条。
	// 这里把语义钉住，免得以后有人把它改成“整盘受保护则所有分区都算系统盘”。
	if deviceByID(t, topo, "sda14").Protection.SystemDisk {
		t.Errorf("sda14: 没有挂载点也没有交换区，不该被直接标成系统盘")
	}
}

// TestEdgesDirection 检查边的方向约定：from 是子/消费者，to 是父/提供者。
func TestEdgesDirection(t *testing.T) {
	topo, _ := scanFixture(t, "storage-2404")
	want := map[string]bool{
		"sda1->sda":  false,
		"sda15->sda": false,
		"sda16->sda": false,
	}
	for _, e := range topo.Edges {
		key := e.FromDeviceID + "->" + e.ToDeviceID
		if _, ok := want[key]; ok {
			want[key] = true
			if e.Relation != RelationPartitionOf {
				t.Errorf("%s: Relation = %q，想要 %q", key, e.Relation, RelationPartitionOf)
			}
		}
		if e.FromDeviceID == "" || e.ToDeviceID == "" {
			t.Errorf("边有空的端点: %+v", e)
		}
	}
	for key, ok := range want {
		if !ok {
			t.Errorf("缺少边 %s", key)
		}
	}
	// 边不能重名重复。
	seen := map[string]bool{}
	for _, e := range topo.Edges {
		key := e.FromDeviceID + "->" + e.ToDeviceID
		if seen[key] {
			t.Errorf("边 %s 重复", key)
		}
		seen[key] = true
	}
}

// TestPartitionTableFromDevice 用真实的分区表字节判断类型与空闲区。
func TestPartitionTableFromDevice(t *testing.T) {
	topo, _ := scanFixture(t, "storage-2404")

	sda := deviceByID(t, topo, "sda")
	if sda.PartitionTable == nil {
		t.Fatal("sda: PartitionTable 为 nil")
	}
	pt := sda.PartitionTable
	if pt.Type != TableGPT {
		t.Fatalf("sda: 分区表类型 = %q（source=%q warning=%q），想要 %q",
			pt.Type, pt.Source, pt.Warning, TableGPT)
	}
	if pt.Source != "device" {
		t.Errorf("sda: Source = %q，想要 device（盘上的 GPT 读到了）", pt.Source)
	}
	if len(pt.Partitions) != 4 {
		t.Errorf("sda: 内核分区数 = %d，想要 4", len(pt.Partitions))
	}
	if pt.Warning != "" {
		t.Errorf("sda: 盘上分区表与内核分区数一致，不该有 warning：%q", pt.Warning)
	}
	// 空闲区是“盘上还剩多少没分区”：sda 40 GiB，四个分区加起来不到 40 GiB，
	// 必须至少有一个空闲区间，且所有空闲区加起来与差值一致（按 1 MiB 对齐允许误差）。
	if len(pt.FreeRegions) == 0 {
		t.Fatalf("sda: FreeRegions 为空，这块盘并没有被分区占满")
	}
	var used uint64
	for _, p := range pt.Partitions {
		used += p.SizeBytes
	}
	var free uint64
	for _, f := range pt.FreeRegions {
		free += f.SizeBytes
		if f.StartBytes%512 != 0 {
			t.Errorf("空闲区起点 %d 不是扇区对齐", f.StartBytes)
		}
	}
	if used+free > sda.SizeBytes {
		t.Errorf("已用 %d + 空闲 %d 超过整盘 %d", used, free, sda.SizeBytes)
	}
	if sda.SizeBytes-(used+free) > 4*1024*1024 {
		t.Errorf("已用+空闲 = %d，比整盘 %d 少了超过 4 MiB：空闲区算漏了",
			used+free, sda.SizeBytes)
	}

	// 空白盘没有分区表：类型 none，不是 gpt 也不是 unknown。
	for _, id := range []string{"sdb", "sdc", "sdd"} {
		dev := deviceByID(t, topo, id)
		if dev.PartitionTable == nil {
			t.Errorf("%s: PartitionTable 为 nil（应该报 none）", id)
			continue
		}
		if dev.PartitionTable.Type != TableNone {
			t.Errorf("%s: 全零首扇区应判 none，实际 %q（warning=%q）",
				id, dev.PartitionTable.Type, dev.PartitionTable.Warning)
		}
	}
}

// TestIdentityHonestOnPhysicalMachine 是身份可信度的“不许吹牛”用例。
//
// 这两台 QEMU 测试机的盘：sysfs 的 serial/wwid 都读不到（属性存在但读报 EINVAL），
// 只有 /dev/disk/by-id 里的 scsi-S… 链接。所以可信度必须是 medium，
// 不能因为“看起来像有序列号”就报 high。
func TestIdentityHonestOnPhysicalMachine(t *testing.T) {
	topo, _ := scanFixture(t, "storage-2404")

	for _, id := range []string{"sda", "sdb", "sdc", "sdd"} {
		dev := deviceByID(t, topo, id)
		if dev.IdentityFingerprint == "" || len(dev.IdentityFingerprint) != len("sha256:")+64 {
			t.Errorf("%s: 指纹格式不对: %q", id, dev.IdentityFingerprint)
		}
		if dev.IdentityConfidence != ConfidenceMedium {
			t.Errorf("%s: IdentityConfidence = %q，这台机器上只有 by-id 的 scsi-S 链接，应为 %q",
				id, dev.IdentityConfidence, ConfidenceMedium)
		}
		if dev.WWN != nil {
			t.Errorf("%s: WWN = %q，sysfs 里读不到 wwid，不该有值", id, *dev.WWN)
		}
	}

	// 同一块盘的指纹必须稳定；不同盘的指纹必须不同。
	seen := map[string]string{}
	for _, id := range []string{"sda", "sdb", "sdc", "sdd"} {
		fp := deviceByID(t, topo, id).IdentityFingerprint
		if other, dup := seen[fp]; dup {
			t.Errorf("%s 与 %s 的指纹相同（%s）：不同物理盘不能共用一个身份", id, other, fp)
		}
		seen[fp] = id
	}

	// 两次采集（内容不变）指纹与快照版本都必须相同。
	topo2, _ := scanFixture(t, "storage-2404")
	for _, id := range []string{"sda", "sdb", "sdc", "sdd"} {
		if a, b := deviceByID(t, topo, id).IdentityFingerprint, deviceByID(t, topo2, id).IdentityFingerprint; a != b {
			t.Errorf("%s: 两次采集的指纹不同：%s vs %s", id, a, b)
		}
	}
	if topo.SnapshotVersion != topo2.SnapshotVersion {
		t.Errorf("内容不变，快照版本却变了：%s vs %s", topo.SnapshotVersion, topo2.SnapshotVersion)
	}
}

// TestHealthNeverClaimsHealthy 钉住健康语义（P04.2 起，P04.3 扩展）：
//
//   - 物理盘在任何采集发生之前只能是 unknown，不能是 healthy；
//   - 分区、loop、rom 这类设备没有独立的 SMART，只能是 unsupported 并写清原因；
//   - 温度没读到就是 null，不是 0。
func TestHealthNeverClaimsHealthy(t *testing.T) {
	topo, _ := scanFixture(t, "storage-2404")
	for _, d := range topo.Nodes {
		switch d.Kind {
		case KindDisk:
			if d.Health.Status != HealthUnknown {
				t.Errorf("%s 是物理盘：Health.Status = %q，扫描本身不读 SMART，只能是 %q",
					d.ID, d.Health.Status, HealthUnknown)
			}
		default:
			if d.Health.Status != HealthUnsupported {
				t.Errorf("%s（%s）: Health.Status = %q，没有独立 SMART 的设备只能是 %q",
					d.ID, d.Kind, d.Health.Status, HealthUnsupported)
			}
			if d.Health.Details == nil || *d.Health.Details == "" {
				t.Errorf("%s（%s）: unsupported 必须给出原因，否则界面只能显示一个空状态",
					d.ID, d.Kind)
			}
		}
		if d.Health.TemperatureCelsius != nil {
			t.Errorf("%s: TemperatureCelsius = %v，没采集就必须是 null 而不是 0",
				d.ID, *d.Health.TemperatureCelsius)
		}
		if len(d.Capabilities) != 0 {
			t.Errorf("%s: Capabilities = %+v，Scan 只采拓扑，能力由 Collector 按判决填（见 capabilities_test.go）",
				d.ID, d.Capabilities)
		}
	}
}

// TestScanRealCapture2204 在 22.04 上再跑一遍同一套断言，
// 覆盖 5.15 内核的 mountinfo 与 snap 的 loop 设备。
func TestScanRealCapture2204(t *testing.T) {
	topo, _ := scanFixture(t, "storage-2204")

	sda := deviceByID(t, topo, "sda")
	if !sda.Protection.SystemDisk {
		t.Errorf("sda: 22.04 上整盘也承载根文件系统，必须受保护")
	}
	if sda.Kind != KindDisk {
		t.Errorf("sda: Kind = %q", sda.Kind)
	}
	if sda.SizeBytes != 42949672960 {
		t.Errorf("sda: SizeBytes = %d，想要 42949672960", sda.SizeBytes)
	}
	if sda.PartitionTable == nil || sda.PartitionTable.Type != TableGPT {
		t.Errorf("sda: 22.04 也是 GPT，实际 %+v", sda.PartitionTable)
	}

	// 22.04 上 snap 用 squashfs 的 loop 设备挂到 /snap/*：
	// 它们必须被识别成 loop，且**不能**被算成系统盘（删掉它伤不到系统）。
	loops := 0
	for _, d := range topo.Nodes {
		if d.Kind != KindLoop {
			continue
		}
		loops++
		if d.Protection.SystemDisk {
			t.Errorf("%s: loop（snap）被标成系统盘，理由 %v", d.ID, d.Protection.Reasons)
		}
	}
	if loops == 0 {
		t.Errorf("22.04 夹具里应该有 snap 的 loop 设备，一个都没识别出来")
	}

	for _, id := range []string{"sdb", "sdc", "sdd"} {
		if deviceByID(t, topo, id).Protection.SystemDisk {
			t.Errorf("%s: 空白数据盘被标成系统盘", id)
		}
	}
}

// TestScanStableOrdering 保证节点顺序可预期（按类型再按名字），
// 界面每次刷新不会跳来跳去。
func TestScanStableOrdering(t *testing.T) {
	topo, _ := scanFixture(t, "storage-2404")
	ids := nodeIDs(topo)
	sorted := append([]string(nil), ids...)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := deviceByID(t, topo, sorted[i]), deviceByID(t, topo, sorted[j])
		if a.Kind != b.Kind {
			return kindRank(a.Kind) < kindRank(b.Kind)
		}
		return a.Name < b.Name
	})
	for i := range ids {
		if ids[i] != sorted[i] {
			t.Fatalf("节点顺序不稳定：第 %d 位是 %q，应为 %q\n实际：%v", i, ids[i], sorted[i], ids)
		}
	}
}

// TestReadDiskLinksUsesSymlinkTarget 盯住一个真实发生过的事故：
//
// 真实系统上 /dev/disk/by-id/<name> 是指向**块设备**的符号链接。曾经的实现先调
// os.ReadFile 再退回 EvalSymlinks——ReadFile 会顺着链接打开 /dev/sda 并把整块盘读进内存，
// 4 GiB 的验收机上代理 3 秒内被 OOM 杀掉（P04.2 在 24.04 上实测）。
//
// 夹具把链接目标写成普通文件内容，所以夹具用例全绿，真机才炸。这个用例用**真的相对符号链接**
// 把两条路径区分开：只有先 Lstat 再 Readlink 才可能通过。
func TestReadDiskLinksUsesSymlinkTarget(t *testing.T) {
	devDir := t.TempDir()
	byID := filepath.Join(devDir, "disk", "by-id")
	if err := os.MkdirAll(byID, 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	// udev 写的是相对链接：by-id/x → ../../sda1。
	if err := os.Symlink("../../sda1", filepath.Join(byID, "scsi-SFAKE_DISK")); err != nil {
		t.Skipf("文件系统不支持符号链接: %v", err)
	}
	// 夹具形态：普通文件里写着目标路径。
	if err := os.WriteFile(filepath.Join(byID, "wwn-0x5000c500"), []byte("/dev/sdb\n"), 0o644); err != nil {
		t.Fatalf("写夹具文件失败: %v", err)
	}
	// 不是设备路径的普通文件必须被忽略，不能被当成链接目标。
	if err := os.WriteFile(filepath.Join(byID, "not-a-link"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("写无关文件失败: %v", err)
	}
	// 指向一个大文件的符号链接：旧实现会把它的内容读进来，新实现只看链接本身。
	big := filepath.Join(devDir, "big.bin")
	if err := os.WriteFile(big, make([]byte, 1<<20), 0o644); err != nil {
		t.Fatalf("写大文件失败: %v", err)
	}
	if err := os.Symlink(big, filepath.Join(byID, "loop-like")); err != nil {
		t.Skipf("文件系统不支持符号链接: %v", err)
	}

	got := readDiskLinks(devDir, map[string]bool{"sda1": true, "sdb": true}, "by-id")
	if want := "scsi-SFAKE_DISK"; got["sda1"] != want {
		t.Fatalf("相对符号链接没有被解析成设备名：sda1 → %q，期望 %q（实际 %v）", got["sda1"], want, got)
	}
	if want := "wwn-0x5000c500"; got["sdb"] != want {
		t.Fatalf("夹具形态（普通文件里写路径）失效：sdb → %q，期望 %q（实际 %v）", got["sdb"], want, got)
	}
	if _, ok := got[""]; ok {
		t.Fatalf("空设备名不该出现在结果里：%v", got)
	}
	for dev, link := range got {
		if dev == "sda1" || dev == "sdb" {
			continue
		}
		t.Fatalf("意外解析出设备 %q → %q（大文件与无关文件都不该变成设备）", dev, link)
	}
}

// TestLinkNamesReadsDirectoryEntries 盯住 holders/slaves 的读取来源。
//
// 2026-10-21 在验收机上实测（6.8.0-147）：`/sys/block/sdc/sdc1/holders/dm-0` 存在，
// 而 `/sys/block/sdc/sdc1/holders/list` 返回 No such file or directory。只读 list 的实现
// 因此看不见“分区被 dm 占用”，判决层也就不会给出 DEVICE_HAS_HOLDERS——一块正被别的东西
// 用着的分区会被判成“可以格式化”。目录项才是事实来源，list 只作退路。
func TestLinkNamesReadsDirectoryEntries(t *testing.T) {
	dir := t.TempDir()

	// (1) 只有目录项、没有 list：真机分区的情况。
	// 真机里这些条目是符号链接，但被读的只有名字；这里用普通文件，测试不依赖
	// 宿主是否允许创建符号链接（Windows 默认不允许），真机形态由验收脚本覆盖。
	only := filepath.Join(dir, "only-entries")
	if err := os.MkdirAll(only, 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(only, "dm-0"), nil, 0o644); err != nil {
		t.Fatalf("写目录项失败: %v", err)
	}
	if got := linkNames(only); len(got) != 1 || got[0] != "dm-0" {
		t.Fatalf("目录项没有被读出来：%v", got)
	}

	// (2) 只有 list 文件：夹具与老内核的形态。
	listed := filepath.Join(dir, "only-list")
	if err := os.MkdirAll(listed, 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(listed, "list"), []byte("md0\n"), 0o644); err != nil {
		t.Fatalf("写 list 失败: %v", err)
	}
	if got := linkNames(listed); len(got) != 1 || got[0] != "md0" {
		t.Fatalf("list 退路没生效：%v", got)
	}

	// (3) 两者都在：合并去重靠 set 语义由调用方处理，这里不该把 list 自己当成依赖者，
	// 也不该漏掉目录里的名字。
	both := filepath.Join(dir, "both")
	if err := os.MkdirAll(both, 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(both, "dm-1"), nil, 0o644); err != nil {
		t.Fatalf("写目录项失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(both, "list"), []byte("dm-1\n"), 0o644); err != nil {
		t.Fatalf("写 list 失败: %v", err)
	}
	got := linkNames(both)
	for _, name := range got {
		if name == "list" {
			t.Fatalf("list 文件本身被当成依赖者：%v", got)
		}
	}
	if len(got) != 1 || got[0] != "dm-1" {
		t.Fatalf("目录项与 list 同时存在时结果不对：%v", got)
	}

	// (4) 目录不存在（设备没有 holders 目录）：空结果，不 panic。
	if got := linkNames(filepath.Join(dir, "missing")); len(got) != 0 {
		t.Fatalf("不存在的目录返回了 %v", got)
	}
}

// TestSnapshotVersionIgnoresTimeAndCapacity 盯住指纹的语义：只有内容变，版本才变。
//
// 每个节点都带 collected_at（每 5 秒一次扫描就会变），容量是 Collector 后填的 statfs 结果。
// 这两样都进了指纹的话，“内容没变版本就不变”就不成立，预检要么误报要么看不出真变化。
func TestSnapshotVersionIgnoresTimeAndCapacity(t *testing.T) {
	root, _, _ := fixtureRoot(t, "storage-2404")

	later := Root{
		Sys:         root.Sys,
		Proc:        root.Proc,
		Disks:       root.Disks,
		Caps:        root.Caps,
		Now:         func() time.Time { return fixedNow.Add(37 * time.Second) },
		ReadTimeout: root.ReadTimeout,
	}
	first, err := Scan(context.Background(), root)
	if err != nil {
		t.Fatalf("第一次 Scan 失败: %v", err)
	}
	second, err := Scan(context.Background(), later)
	if err != nil {
		t.Fatalf("第二次 Scan 失败: %v", err)
	}
	if first.SnapshotVersion != second.SnapshotVersion {
		t.Fatalf("采集时间不同不该改变指纹：%s != %s", first.SnapshotVersion, second.SnapshotVersion)
	}

	// 给挂载点补上容量（Collector 的做法），指纹仍不应变化。
	for i := range second.Nodes {
		fs := second.Nodes[i].Filesystem
		if fs == nil || len(fs.Mounts) == 0 {
			continue
		}
		fs.Mounts[0].Capacity = &Capacity{TotalBytes: 123456789, CollectedAt: fixedNow}
	}
	again, err := snapshotVersion(second.Nodes, second.Edges, second.NetworkMounts)
	if err != nil {
		t.Fatalf("计算指纹失败: %v", err)
	}
	if again != first.SnapshotVersion {
		t.Fatalf("容量不该改变指纹：%s != %s", again, first.SnapshotVersion)
	}

	// 内容真的变了，指纹必须变。
	moved := append([]Device(nil), second.Nodes...)
	for i := range moved {
		if moved[i].Filesystem == nil || len(moved[i].Filesystem.Mounts) == 0 {
			continue
		}
		fs := *moved[i].Filesystem
		fs.Mounts = append([]Mount(nil), fs.Mounts...)
		fs.Mounts[0].Path = "/moved"
		moved[i].Filesystem = &fs
		break
	}
	changed, err := snapshotVersion(moved, second.Edges, second.NetworkMounts)
	if err != nil {
		t.Fatalf("计算指纹失败: %v", err)
	}
	if changed == first.SnapshotVersion {
		t.Fatal("挂载点变了，指纹却没变")
	}
}

// TestDeviceWithTwoMountPointsKeepsBoth 覆盖“同一块盘挂两处”。
//
// 追加的那一行是 VM-E 上 `mount --bind /srv/upanel/mm/a /srv/upanel/mm/b` 之后
// /proc/self/mountinfo 里的原话（major:minor 8:1，与 `/` 是同一个 /dev/sda1），
// 其余夹具是 24.04 真机采集。两处挂载必须是两个 Mount 条目、各自查容量、按 path 稳定排序：
// 合并成一条会让“哪个目录快满了”在页面上直接消失。
func TestDeviceWithTwoMountPointsKeepsBoth(t *testing.T) {
	src, err := fixtureDir("storage-2404")
	if err != nil {
		t.Fatalf("定位夹具失败: %v", err)
	}
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS(src)); err != nil {
		t.Fatalf("复制夹具树失败: %v", err)
	}

	// VM-E 真实记录（内核给出的原话，字段顺序不变）。
	const bindLine = "85 28 8:1 /srv/upanel/mm/a /srv/upanel/mm/b rw,relatime shared:1 - ext4 /dev/sda1 rw,discard,errors=remount-ro,commit=30\n"
	mountsPath := filepath.Join(dir, "proc", "self", "mountinfo")
	f, err := os.OpenFile(mountsPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("打开夹具 mountinfo 失败: %v", err)
	}
	if _, err := f.WriteString(bindLine); err != nil {
		t.Fatalf("写入 bind 挂载记录失败: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("关闭夹具 mountinfo 失败: %v", err)
	}

	// 容量由 Collector 在 Scan 之后填（30 秒缓存那一层），这里只验拓扑结构。
	topo, err := Scan(context.Background(), Root{
		Sys:         filepath.Join(dir, "sys"),
		Proc:        filepath.Join(dir, "proc"),
		Disks:       &fixtureDiskReader{dir: filepath.Join(dir, "disks")},
		Now:         func() time.Time { return fixedNow },
		ReadTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("Scan 失败: %v", err)
	}

	dev := deviceByID(t, topo, "sda1")
	if dev.Filesystem == nil {
		t.Fatalf("sda1 有挂载点，但 Filesystem 为 nil")
	}
	if len(dev.Filesystem.Mounts) != 2 {
		t.Fatalf("sda1 的挂载点 = %+v，想要 2 条（/ 与 /srv/upanel/mm/b）", dev.Filesystem.Mounts)
	}
	want := []string{"/", "/srv/upanel/mm/b"}
	for i, m := range dev.Filesystem.Mounts {
		if m.Path != want[i] {
			t.Errorf("挂载点[%d] = %q，想要 %q（挂载点必须按 path 排序，页面才不会每次都换顺序）",
				i, m.Path, want[i])
		}
		if m.FSType != "ext4" {
			t.Errorf("挂载点 %s 的 fstype = %q，想要 ext4（同一个文件系统）", m.Path, m.FSType)
		}
	}

	// 多一个挂载点不能让系统盘保护失效：根还在，整盘 sda 仍受保护。
	disk := deviceByID(t, topo, "sda")
	if !disk.Protection.SystemDisk {
		t.Errorf("sda: 多挂一个目录之后 SystemDisk = false，系统盘保护不能因此失效")
	}
}
