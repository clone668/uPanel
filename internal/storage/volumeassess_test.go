package storage

// 卷动作判决的测试（P07.4）。
//
// 与 P07.1/P07.2 的判决测试同一个思路：手工构造“世界”，然后逐条钉住
// 拒绝码与放行条件。这里多了一层：判决依赖登记表与 fstab，所以每个用例都要
// 指明“库里有什么卷”“fstab 里有什么行”“盘上现在挂着没有”。
//
// 放行类用例与拒绝类用例数量相当，是刻意的：只写拒绝的测试很容易固化成一个
// “永远拒绝”的实现，而永远拒绝同样不合格。

import (
	"context"
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

// ---------- 假文件系统 ----------

// fakeFileInfo 是最小的 os.FileInfo：只需要 Mode / IsDir / Sys（属主）。
type fakeFileInfo struct {
	name  string
	mode  os.FileMode
	uid   int
	gid   int
	owner bool // 是否提供属主信息（false 时 Sys() 返回 nil）
}

func (f fakeFileInfo) Name() string       { return f.name }
func (f fakeFileInfo) Size() int64        { return 0 }
func (f fakeFileInfo) Mode() os.FileMode  { return f.mode }
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return f.mode.IsDir() }
func (f fakeFileInfo) Sys() any {
	if !f.owner {
		return nil
	}
	return testOwnerSys(f.uid, f.gid)
}

// fakeTree 是一棵“存在的目录树”：路径 → 文件信息 + 目录内容。
type fakeTree struct {
	info    map[string]fakeFileInfo
	entries map[string][]os.DirEntry
}

func newFakeTree() *fakeTree {
	return &fakeTree{info: map[string]fakeFileInfo{}, entries: map[string][]os.DirEntry{}}
}

// dir 登记一个 root 所有、0755 的目录（从 / 到 path 的每一段都补上）。
func (t *fakeTree) dir(path string, perm os.FileMode) *fakeTree {
	for _, seg := range pathSegments(path) {
		if _, ok := t.info[seg]; ok {
			continue
		}
		t.info[seg] = fakeFileInfo{name: seg, mode: os.ModeDir | perm, uid: 0, gid: 0, owner: true}
	}
	return t
}

// setMode 改一个已登记目录的权限（dir 只补不覆盖）。
func (t *fakeTree) setMode(path string, perm os.FileMode) *fakeTree {
	fi := t.info[path]
	fi.mode = os.ModeDir | perm
	t.info[path] = fi
	return t
}

// symlink 把一个路径登记成符号链接。
func (t *fakeTree) symlink(path string) *fakeTree {
	t.dir(parentOf(path), 0o755)
	t.info[path] = fakeFileInfo{name: lastSeg(path), mode: os.ModeSymlink | 0o777}
	return t
}

// file 把一个路径登记成普通文件（不是目录）。
func (t *fakeTree) file(path string) *fakeTree {
	t.dir(parentOf(path), 0o755)
	t.info[path] = fakeFileInfo{name: lastSeg(path), mode: 0o644, owner: true}
	return t
}

// contents 指定某个目录里的条目名。
func (t *fakeTree) contents(path string, names ...string) *fakeTree {
	ents := make([]os.DirEntry, 0, len(names))
	for _, n := range names {
		ents = append(ents, fakeDirEntry{name: n})
	}
	t.entries[path] = ents
	return t
}

func (t *fakeTree) lstat(path string) (os.FileInfo, error) {
	if fi, ok := t.info[path]; ok {
		return fi, nil
	}
	return nil, os.ErrNotExist
}

func (t *fakeTree) readDir(path string) ([]os.DirEntry, error) {
	return t.entries[path], nil
}

// mkdirAll 是 writer 的建目录接缝：只登记目录，不碰真实文件系统。
func (t *fakeTree) mkdirAll(path string, perm os.FileMode) error {
	t.dir(path, perm)
	return nil
}

// chmod 是 writer 的改权限接缝。
func (t *fakeTree) chmod(path string, mode os.FileMode) error {
	t.setMode(path, mode.Perm())
	return nil
}

type fakeDirEntry struct{ name string }

func (e fakeDirEntry) Name() string               { return e.name }
func (e fakeDirEntry) IsDir() bool                { return false }
func (e fakeDirEntry) Type() os.FileMode          { return 0 }
func (e fakeDirEntry) Info() (os.FileInfo, error) { return fakeFileInfo{name: e.name}, nil }

func parentOf(p string) string {
	if i := strings.LastIndex(p, "/"); i > 0 {
		return p[:i]
	}
	return "/"
}

func lastSeg(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// ---------- 夹具 ----------

// assessFixture 把“判决需要的那一整套”装在一起。
type assessFixture struct {
	f    *volumeFixture
	tree *fakeTree
	env  VolumeEnv
}

// newAssessFixture 造一套默认世界：登记表空、fstab 是干净的原文、挂载点目录齐全。
//
// 挂载点目录默认是 root:root 0755（那是真实 Ubuntu 上 /srv 的样子），
// 需要制造问题的用例再单独改 tree。
func newAssessFixture(t *testing.T, fstabContent string) *assessFixture {
	t.Helper()
	f := newVolumeFixture(t, fstabContent)
	tree := newFakeTree().dir(DefaultVolumeRoot, 0o755)
	env := f.volumeEnv()
	env.Lstat = tree.lstat
	env.ReadDir = tree.readDir
	return &assessFixture{f: f, tree: tree, env: env}
}

// allowMountPoint 让挂载点目录存在（判决会检查它能不能建出来）。
func (a *assessFixture) allowMountPoint(slug string) *assessFixture {
	a.tree.dir(a.env.mountPathOf(slug), 0o755)
	return a
}

// withRecord 往登记表里放一条记录。
func (a *assessFixture) withRecord(rec VolumeRecord) *assessFixture {
	a.f.store.recs[rec.ID] = rec
	return a
}

// swapFstab 换一份 fstab 内容（重写文件并同步到判决环境里那份副本）。
//
// VolumeEnv 里存的是 FstabStore 的**值**，所以改 fixture 上的那个不会影响判决用
// 的那个——这一点曾经让两条“块外已有条目”的用例静默变成“放行”。
func (a *assessFixture) swapFstab(t *testing.T, content string) {
	t.Helper()
	path := writeFstab(t, content)
	a.f.fstab.Path = path
	a.env.Fstab.Path = path
}

// assess 跑一次判决。
func (a *assessFixture) assess(t *testing.T, action string, params any, topo Topology) Assessment {
	t.Helper()
	got, err := AssessVolume(context.Background(), topo, action, params, a.env)
	if err != nil {
		t.Fatalf("判决返回错误（应当是阻塞项而不是 error）：%v", err)
	}
	return got
}

// fstabWith 生成“原文 + 托管块（给定条目）”的 fstab 内容。
func fstabWith(t *testing.T, entries ...FstabEntry) string {
	t.Helper()
	block, err := ParseFstab(existingFstab)
	if err != nil {
		t.Fatalf("解析原文失败: %v", err)
	}
	return block.Render(entries)
}

const testVolID = "vol_0123456789abcdef0123456789abcdef"

// ---------- 导入 ----------

func TestAssessImportAllowsFreshPartition(t *testing.T) {
	a := newAssessFixture(t, existingFstab).allowMountPoint("data")
	a.f.super.put("sdb1", testUUID, FilesystemExt4)

	got := a.assess(t, ActionVolumeImport,
		VolumeImportParams{DeviceID: "sdb1", Slug: "data"}, volumeTopo("sha256:topo1", testUUID))

	mustAllow(t, got)
	if !strings.Contains(strings.Join(got.Locks, ","), "disk:sdb") {
		t.Errorf("锁 = %v，必须包含整盘锁 disk:sdb", got.Locks)
	}
	if !strings.Contains(strings.Join(got.Locks, ","), "fstab") {
		t.Errorf("锁 = %v，必须包含 fstab", got.Locks)
	}
	if got.IdentityFingerprint == "" {
		t.Error("身份指纹为空：执行前无法复核目标")
	}
	if got.TargetID != "sdb1" {
		t.Errorf("TargetID = %q", got.TargetID)
	}
	if got.StateVersion == "" {
		t.Error("StateVersion 为空")
	}
	// 影响里必须写清楚“会写 fstab”，因为这是唯一会改开机行为的动作。
	if len(got.Impacts) == 0 {
		t.Fatal("导入没有给出任何影响说明：管理员看不到它会改开机配置")
	}
	impact := ""
	for _, imp := range got.Impacts {
		impact += imp.Description + "；"
	}
	mustContain(t, impact, testUUID, "影响说明")
	mustContain(t, impact, "受管块", "影响说明")
	mustContain(t, impact, "不会格式化", "影响说明")
}

func TestAssessImportRejections(t *testing.T) {
	type setup func(*assessFixture, *Topology)
	cases := []struct {
		name  string
		code  string
		setup setup
	}{
		{
			name: "设备不在拓扑里", code: NoticeDeviceMissing,
			setup: func(*assessFixture, *Topology) {},
		},
		{
			name: "目标是整盘而不是分区", code: NoticeUnsupportedKind,
			setup: func(a *assessFixture, _ *Topology) {
				a.f.super.put("sdb1", testUUID, FilesystemExt4)
			},
		},
		{
			name: "分区已经挂着", code: NoticeVolumeMounted,
			setup: func(a *assessFixture, _ *Topology) {
				a.f.super.put("sdb1", testUUID, FilesystemExt4)
			},
		},
		{
			name: "分区挂在想要的挂载点上", code: NoticeVolumeMountedElsewhere,
			setup: func(a *assessFixture, _ *Topology) {
				a.f.super.put("sdb1", testUUID, FilesystemExt4)
			},
		},
		{
			name: "设备不在线", code: NoticeOffline,
			setup: func(a *assessFixture, _ *Topology) {
				a.f.super.put("sdb1", testUUID, FilesystemExt4)
			},
		},
		{
			name: "设备标记只读", code: NoticeReadOnly,
			setup: func(a *assessFixture, _ *Topology) {
				a.f.super.put("sdb1", testUUID, FilesystemExt4)
			},
		},
		{
			name: "超级块读不出来", code: NoticeFilesystemUnreadable,
			setup: func(a *assessFixture, _ *Topology) {
				a.f.super.errs = map[string]error{"sdb1": errors.New("输入输出错误")}
			},
		},
		{
			name: "没有 UUID", code: NoticeFilesystemUnreadable,
			setup: func(a *assessFixture, _ *Topology) {
				a.f.super.byDevice["sdb1"] = FilesystemIdentity{Kind: FilesystemExt4}
			},
		},
		{
			name: "文件系统不在支持范围", code: NoticeFilesystemUnsupported,
			setup: func(a *assessFixture, _ *Topology) {
				a.f.super.byDevice["sdb1"] = FilesystemIdentity{Kind: "vfat", UUID: testUUID}
			},
		},
		{
			name: "卷名已被占用", code: NoticeVolumeSlugTaken,
			setup: func(a *assessFixture, _ *Topology) {
				a.f.super.put("sdb1", testUUID, FilesystemExt4)
				a.withRecord(testRecord(testVolID, "data", "sdc1", testUUID2))
			},
		},
		{
			name: "UUID 已被占用", code: NoticeVolumeUUIDTaken,
			setup: func(a *assessFixture, _ *Topology) {
				a.f.super.put("sdb1", testUUID, FilesystemExt4)
				a.withRecord(testRecord(testVolID, "other", "sdc1", testUUID))
			},
		},
		{
			name: "fstab 块外已有同 UUID 的行", code: NoticeFstabExternalEntry,
			setup: func(a *assessFixture, _ *Topology) {
				a.f.super.put("sdb1", testUUID, FilesystemExt4)
				a.swapFstab(t, existingFstab+"UUID="+testUUID+" /mnt/manual ext4 defaults 0 2\n")
			},
		},
		{
			name: "fstab 块外已有同挂载点的行", code: NoticeFstabExternalEntry,
			setup: func(a *assessFixture, _ *Topology) {
				a.f.super.put("sdb1", testUUID, FilesystemExt4)
				a.swapFstab(t, existingFstab+"/dev/sdc1 /srv/upanel/volumes/data ext4 defaults 0 2\n")
			},
		},
		{
			name: "挂载点路径上有符号链接", code: NoticeMountPointUnsafe,
			setup: func(a *assessFixture, _ *Topology) {
				a.f.super.put("sdb1", testUUID, FilesystemExt4)
				a.tree.symlink("/srv/upanel")
			},
		},
		{
			name: "挂载点父目录允许任何用户写入", code: NoticeMountPointUnsafe,
			setup: func(a *assessFixture, _ *Topology) {
				a.f.super.put("sdb1", testUUID, FilesystemExt4)
				a.tree.setMode(DefaultVolumeRoot, 0o777)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newAssessFixture(t, existingFstab)
			topo := volumeTopo("sha256:topo1", testUUID)
			deviceID := "sdb1"
			switch tc.name {
			case "设备不在拓扑里":
				deviceID = "sdz9"
			case "目标是整盘而不是分区":
				deviceID = "sdb"
			case "分区已经挂着":
				mountDeviceAt(findNode(&topo, "sdb1"), "/mnt/manual")
			case "分区挂在想要的挂载点上":
				mountDeviceAt(findNode(&topo, "sdb1"), "/srv/upanel/volumes/data")
			case "设备不在线":
				findNode(&topo, "sdb1").Online = false
			case "设备标记只读":
				findNode(&topo, "sdb1").ReadOnly = true
			}
			tc.setup(a, &topo)

			got := a.assess(t, ActionVolumeImport,
				VolumeImportParams{DeviceID: deviceID, Slug: "data"}, topo)
			mustBlock(t, got, tc.code)
			if got.StateVersion != "" && tc.code != NoticeFstabUnavailable {
				// 被拒绝时也可以有 StateVersion（判决本身仍然是“关于某一刻”的），
				// 但不该是空的假值以外的怪东西——这里只确认它不以空格开头。
				if strings.HasPrefix(got.StateVersion, " ") {
					t.Errorf("StateVersion 形状可疑：%q", got.StateVersion)
				}
			}
		})
	}
}

// TestAssessImportRejectsSystemDisk 单独钉住系统盘保护：数据盘可以导入，
// 系统盘上的分区永远不行。
func TestAssessImportRejectsSystemDisk(t *testing.T) {
	a := newAssessFixture(t, existingFstab).allowMountPoint("data")
	a.f.super.put("sda1", testUUID, FilesystemExt4)

	disk := blankDisk("sda", 40<<30)
	disk.Protection = Protection{SystemDisk: true, Reasons: []string{"承载根文件系统 /"}, Sources: []string{"/"}}
	part := addPartition(&disk, "sda1", 1, 2*mib, 39<<30)
	part.Filesystem = &Filesystem{Type: strPtr(FilesystemExt4), UUID: strPtr(testUUID), Mounts: []Mount{}}
	topo := guardTopo("sha256:topo1", []Device{disk, part})

	got := a.assess(t, ActionVolumeImport, VolumeImportParams{DeviceID: "sda1", Slug: "data"}, topo)
	mustBlock(t, got, NoticeSystemDisk)
}

// TestAssessImportWarnsOnNonEmptyMountPoint 校验“挂载点里已经有文件”只是警告：
// 文件不会丢，只是挂上之后看不到——这属于要告知，不属于要拒绝。
func TestAssessImportWarnsOnNonEmptyMountPoint(t *testing.T) {
	a := newAssessFixture(t, existingFstab).allowMountPoint("data")
	a.tree.contents(a.env.mountPathOf("data"), "lost+found", "notes.txt")
	a.f.super.put("sdb1", testUUID, FilesystemExt4)

	got := a.assess(t, ActionVolumeImport, VolumeImportParams{DeviceID: "sdb1", Slug: "data"},
		volumeTopo("sha256:topo1", testUUID))
	mustAllow(t, got)
	if !hasNotice(got.Warnings, NoticeMountPointNotEmpty) {
		t.Errorf("没有给出挂载点非空的警告：%+v", got.Warnings)
	}
}

func TestAssessVolumeWithoutStoreOrTopology(t *testing.T) {
	f := newVolumeFixture(t, existingFstab)
	env := f.volumeEnv()
	env.Store = nil
	got, err := AssessVolume(context.Background(), volumeTopo("sha256:t", testUUID),
		ActionVolumeImport, VolumeImportParams{DeviceID: "sdb1", Slug: "data"}, env)
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	mustBlock(t, got, NoticeTopologyUnavailable)

	env = f.volumeEnv()
	broken := Topology{Status: StatusError, Error: "sysfs 读不出来"}
	got, err = AssessVolume(context.Background(), broken, ActionVolumeImport,
		VolumeImportParams{DeviceID: "sdb1", Slug: "data"}, env)
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	mustBlock(t, got, NoticeTopologyUnavailable)
}

// ---------- 挂载 ----------

func TestAssessMountAllows(t *testing.T) {
	rec := testRecord(testVolID, "data", "sdb1", testUUID)
	content := fstabWith(t, rec.FstabEntry())
	a := newAssessFixture(t, content).allowMountPoint("data").withRecord(rec)
	a.f.super.put("sdb1", testUUID, FilesystemExt4)

	got := a.assess(t, ActionVolumeMount, VolumeParams{VolumeID: rec.ID, Version: "1"},
		volumeTopo("sha256:topo1", testUUID))
	mustAllow(t, got)
	locks := strings.Join(got.Locks, ",")
	if !strings.Contains(locks, "volume:"+rec.ID) || !strings.Contains(locks, "disk:sdb") {
		t.Errorf("锁 = %v，必须有卷锁与整盘锁", got.Locks)
	}
	if got.IdentityFingerprint == "" {
		t.Error("身份指纹为空")
	}
}

func TestAssessMountRejections(t *testing.T) {
	rec := testRecord(testVolID, "data", "sdb1", testUUID)
	cases := []struct {
		name    string
		code    string
		records []VolumeRecord
		fstab   string
		mutate  func(*Topology)
		params  VolumeParams
	}{
		{
			name: "卷不存在", code: NoticeVolumeNotFound,
			fstab: existingFstab, params: VolumeParams{VolumeID: testVolID, Version: "1"},
		},
		{
			name: "版本过期", code: NoticeVolumeVersionMismatch,
			records: []VolumeRecord{rec}, fstab: fstabWith(t, rec.FstabEntry()),
			params: VolumeParams{VolumeID: testVolID, Version: "7"},
		},
		{
			name: "已经挂着", code: NoticeVolumeMounted,
			records: []VolumeRecord{rec}, fstab: fstabWith(t, rec.FstabEntry()),
			mutate: func(topo *Topology) { mountDeviceAt(findNode(topo, "sdb1"), rec.MountPath()) },
			params: VolumeParams{VolumeID: testVolID, Version: "1"},
		},
		{
			name: "挂在别处", code: NoticeVolumeMountedElsewhere,
			records: []VolumeRecord{rec}, fstab: fstabWith(t, rec.FstabEntry()),
			mutate: func(topo *Topology) { mountDeviceAt(findNode(topo, "sdb1"), "/mnt/old") },
			params: VolumeParams{VolumeID: testVolID, Version: "1"},
		},
		{
			name: "设备不在", code: NoticeDeviceMissing,
			records: []VolumeRecord{rec}, fstab: fstabWith(t, rec.FstabEntry()),
			mutate: func(topo *Topology) { deleteNode(topo, "sdb1") },
			params: VolumeParams{VolumeID: testVolID, Version: "1"},
		},
		{
			name: "UUID 不符", code: NoticeFilesystemMismatch,
			records: []VolumeRecord{rec}, fstab: fstabWith(t, rec.FstabEntry()),
			params: VolumeParams{VolumeID: testVolID, Version: "1"},
		},
		{
			name: "fstab 里没有条目", code: NoticeFstabDrift,
			records: []VolumeRecord{rec}, fstab: existingFstab,
			params: VolumeParams{VolumeID: testVolID, Version: "1"},
		},
		{
			name: "fstab 条目与登记不一致", code: NoticeFstabDrift,
			records: []VolumeRecord{rec},
			fstab: fstabWith(t, FstabEntry{UUID: testUUID, MountPoint: rec.MountPath(),
				FSType: FilesystemExt4, Options: []string{"ro"}, Pass: 2}),
			params: VolumeParams{VolumeID: testVolID, Version: "1"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newAssessFixture(t, tc.fstab).allowMountPoint("data")
			for _, r := range tc.records {
				a.withRecord(r)
			}
			uuid := testUUID
			if tc.name == "UUID 不符" {
				uuid = testUUID2
			}
			a.f.super.put("sdb1", uuid, FilesystemExt4)
			topo := volumeTopo("sha256:topo1", uuid)
			if tc.mutate != nil {
				tc.mutate(&topo)
			}
			got := a.assess(t, ActionVolumeMount, tc.params, topo)
			mustBlock(t, got, tc.code)
		})
	}
}

// ---------- 卸载 ----------

func TestAssessUnmountAllowsWhenNotBusy(t *testing.T) {
	rec := testRecord(testVolID, "data", "sdb1", testUUID)
	a := newAssessFixture(t, fstabWith(t, rec.FstabEntry())).allowMountPoint("data").withRecord(rec)
	a.f.super.put("sdb1", testUUID, FilesystemExt4)
	topo := volumeTopo("sha256:topo1", testUUID)
	mountDeviceAt(findNode(&topo, "sdb1"), rec.MountPath())

	got := a.assess(t, ActionVolumeUnmount, VolumeParams{VolumeID: rec.ID, Version: "1"}, topo)
	mustAllow(t, got)
}

// TestAssessUnmountListsBusyOwners 钉住“繁忙占用列表”：有进程开着设备就不许卸，
// 而且要说清是哪个 pid。
func TestAssessUnmountListsBusyOwners(t *testing.T) {
	rec := testRecord(testVolID, "data", "sdb1", testUUID)
	a := newAssessFixture(t, fstabWith(t, rec.FstabEntry())).allowMountPoint("data").withRecord(rec)
	a.f.super.put("sdb1", testUUID, FilesystemExt4)
	a.env.Busy = &fakeBusy{procs: map[string][]BusyProcess{
		"sdb1": {{PID: "4242", Comm: "postgres"}},
	}}
	topo := volumeTopo("sha256:topo1", testUUID)
	mountDeviceAt(findNode(&topo, "sdb1"), rec.MountPath())

	got := a.assess(t, ActionVolumeUnmount, VolumeParams{VolumeID: rec.ID, Version: "1"}, topo)
	mustBlock(t, got, NoticeVolumeBusy)
	msg := blockerMessage(got, NoticeVolumeBusy)
	mustContain(t, msg, "4242", "阻塞消息")
	mustContain(t, msg, "postgres", "阻塞消息")
}

// TestAssessUnmountListsOpenFileOwners 钉住最常见的那种占用：进程打开着挂载点上的文件。
//
// umount 拦的是“这个文件系统上还有打开的文件”，不是“这块块设备被打开着”。
// 只查设备号的实现会让这里放行（设备没人开着），然后 umount 半途失败 target is busy——
// 用户拿到的是一句英文错误，而不是“nginx 正开着这个卷”。
func TestAssessUnmountListsOpenFileOwners(t *testing.T) {
	rec := testRecord(testVolID, "data", "sdb1", testUUID)
	a := newAssessFixture(t, fstabWith(t, rec.FstabEntry())).allowMountPoint("data").withRecord(rec)
	a.f.super.put("sdb1", testUUID, FilesystemExt4)
	a.env.Busy = &fakeBusy{mountProcs: map[string][]BusyProcess{
		rec.MountPath(): {{PID: "777", Comm: "nginx"}},
	}}
	topo := volumeTopo("sha256:topo1", testUUID)
	mountDeviceAt(findNode(&topo, "sdb1"), rec.MountPath())

	got := a.assess(t, ActionVolumeUnmount, VolumeParams{VolumeID: rec.ID, Version: "1"}, topo)
	mustBlock(t, got, NoticeVolumeBusy)
	msg := blockerMessage(got, NoticeVolumeBusy)
	mustContain(t, msg, "777", "阻塞消息")
	mustContain(t, msg, "nginx", "阻塞消息")
	mustContain(t, msg, rec.MountPath(), "阻塞消息")
}

// TestAssessUnmountMountScanErrorBlocks 挂载点上的占用查不成时必须拒绝：
// “看不见”不能当成“没人用”，否则卸载会在执行阶段半途失败。
func TestAssessUnmountMountScanErrorBlocks(t *testing.T) {
	rec := testRecord(testVolID, "data", "sdb1", testUUID)
	a := newAssessFixture(t, fstabWith(t, rec.FstabEntry())).allowMountPoint("data").withRecord(rec)
	a.f.super.put("sdb1", testUUID, FilesystemExt4)
	a.env.Busy = &fakeBusy{mountErrs: map[string]error{
		rec.MountPath(): errors.New("/proc 读不到"),
	}}
	topo := volumeTopo("sha256:topo1", testUUID)
	mountDeviceAt(findNode(&topo, "sdb1"), rec.MountPath())

	got := a.assess(t, ActionVolumeUnmount, VolumeParams{VolumeID: rec.ID, Version: "1"}, topo)
	mustBlock(t, got, NoticeBusy)
	mustContain(t, blockerMessage(got, NoticeBusy), "/proc 读不到", "阻塞消息")
}

func TestAssessUnmountRejectsWhenNotMounted(t *testing.T) {
	rec := testRecord(testVolID, "data", "sdb1", testUUID)
	a := newAssessFixture(t, fstabWith(t, rec.FstabEntry())).allowMountPoint("data").withRecord(rec)
	a.f.super.put("sdb1", testUUID, FilesystemExt4)

	got := a.assess(t, ActionVolumeUnmount, VolumeParams{VolumeID: rec.ID, Version: "1"},
		volumeTopo("sha256:topo1", testUUID))
	mustBlock(t, got, NoticeVolumeNotMounted)
}

func TestAssessUnmountWithoutBusyScanner(t *testing.T) {
	rec := testRecord(testVolID, "data", "sdb1", testUUID)
	a := newAssessFixture(t, fstabWith(t, rec.FstabEntry())).allowMountPoint("data").withRecord(rec)
	a.env.Busy = nil
	a.f.super.put("sdb1", testUUID, FilesystemExt4)
	topo := volumeTopo("sha256:topo1", testUUID)
	mountDeviceAt(findNode(&topo, "sdb1"), rec.MountPath())

	got := a.assess(t, ActionVolumeUnmount, VolumeParams{VolumeID: rec.ID, Version: "1"}, topo)
	mustBlock(t, got, NoticeBusy)
}

// TestAssessUnmountIgnoresFstabHash 钉住一处刻意的设计：卸载的 StateVersion
// 不含 fstab 哈希。别人改了别的卷的条目，不该让一次卸载的预检失效。
func TestAssessUnmountIgnoresFstabHash(t *testing.T) {
	rec := testRecord(testVolID, "data", "sdb1", testUUID)
	entry := rec.FstabEntry()

	run := func(content string) string {
		a := newAssessFixture(t, content).allowMountPoint("data").withRecord(rec)
		a.f.super.put("sdb1", testUUID, FilesystemExt4)
		topo := volumeTopo("sha256:topo1", testUUID)
		mountDeviceAt(findNode(&topo, "sdb1"), rec.MountPath())
		got := a.assess(t, ActionVolumeUnmount, VolumeParams{VolumeID: rec.ID, Version: "1"}, topo)
		mustAllow(t, got)
		return got.StateVersion
	}

	base := run(fstabWith(t, entry))
	other := run(fstabWith(t, entry, FstabEntry{
		UUID: testUUID2, MountPoint: "/srv/upanel/volumes/zeta", FSType: FilesystemExt4,
		Options: []string{"rw"}, Pass: 2,
	}))
	if base != other {
		t.Errorf("fstab 多了别的卷的条目，卸载的 StateVersion 变了：\n%s\n%s", base, other)
	}
}

// TestAssessMountStateVersionBindsFstab 与上一条形成对照：挂载不重写 fstab，
// 但它依赖“fstab 里那一行还在”，所以必须绑哈希。
func TestAssessMountStateVersionBindsFstab(t *testing.T) {
	rec := testRecord(testVolID, "data", "sdb1", testUUID)
	entry := rec.FstabEntry()
	a := newAssessFixture(t, fstabWith(t, entry)).allowMountPoint("data").withRecord(rec)
	a.f.super.put("sdb1", testUUID, FilesystemExt4)
	first := a.assess(t, ActionVolumeMount, VolumeParams{VolumeID: rec.ID, Version: "1"},
		volumeTopo("sha256:topo1", testUUID))
	mustAllow(t, first)

	b := newAssessFixture(t, fstabWith(t, entry, FstabEntry{
		UUID: testUUID2, MountPoint: "/srv/upanel/volumes/zeta", FSType: FilesystemExt4,
		Options: []string{"rw"}, Pass: 2,
	})).allowMountPoint("data").withRecord(rec)
	b.f.super.put("sdb1", testUUID, FilesystemExt4)
	second := b.assess(t, ActionVolumeMount, VolumeParams{VolumeID: rec.ID, Version: "1"},
		volumeTopo("sha256:topo1", testUUID))
	mustAllow(t, second)

	if first.StateVersion == second.StateVersion {
		t.Error("fstab 变了，挂载的 StateVersion 却没变")
	}
}

// ---------- 配置 ----------

func TestAssessConfigure(t *testing.T) {
	rec := testRecord(testVolID, "data", "sdb1", testUUID)
	cases := []struct {
		name    string
		mounted bool
		params  VolumeConfigureParams
		code    string // 空表示应当放行
	}{
		{
			name:   "未挂载时改读写选项",
			params: VolumeConfigureParams{VolumeID: testVolID, Version: "1", ReadOnly: boolPtr(true)},
		},
		{
			name: "未挂载时打开自动挂载",
			params: VolumeConfigureParams{VolumeID: testVolID, Version: "1",
				Automount: boolPtr(true)},
		},
		{
			name:    "挂载时打开自动挂载（允许：只改条目，不影响当前挂载）",
			mounted: true,
			params:  VolumeConfigureParams{VolumeID: testVolID, Version: "1", Automount: boolPtr(true)},
		},
		{
			name:    "挂载时改只读（拒绝：要重新挂载才生效）",
			mounted: true,
			code:    NoticeVolumeOptionsNeedUnmount,
			params:  VolumeConfigureParams{VolumeID: testVolID, Version: "1", ReadOnly: boolPtr(true)},
		},
		{
			name:    "挂载时关掉 nodev（拒绝）",
			mounted: true,
			code:    NoticeVolumeOptionsNeedUnmount,
			params:  VolumeConfigureParams{VolumeID: testVolID, Version: "1", Nodev: boolPtr(false)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newAssessFixture(t, fstabWith(t, rec.FstabEntry())).allowMountPoint("data").withRecord(rec)
			a.f.super.put("sdb1", testUUID, FilesystemExt4)
			topo := volumeTopo("sha256:topo1", testUUID)
			if tc.mounted {
				mountDeviceAt(findNode(&topo, "sdb1"), rec.MountPath())
			}
			got := a.assess(t, ActionVolumeConfigure, tc.params, topo)
			if tc.code == "" {
				mustAllow(t, got)
				locks := strings.Join(got.Locks, ",")
				if !strings.Contains(locks, "fstab") || !strings.Contains(locks, "volume:"+testVolID) {
					t.Errorf("锁 = %v，configure 必须同时锁 fstab 与卷", got.Locks)
				}
				return
			}
			mustBlock(t, got, tc.code)
		})
	}
}

// TestAssessConfigureRequiresDeviceForAutomount 钉住“打开自动挂载要求设备在位”：
// 设备读不到时写进 fstab 的 automount 条目会让开机挂载失败。
func TestAssessConfigureRequiresDeviceForAutomount(t *testing.T) {
	rec := testRecord(testVolID, "data", "sdb1", testUUID)
	a := newAssessFixture(t, fstabWith(t, rec.FstabEntry())).allowMountPoint("data").withRecord(rec)
	topo := volumeTopo("sha256:topo1", testUUID)
	deleteNode(&topo, "sdb1")

	got := a.assess(t, ActionVolumeConfigure, VolumeConfigureParams{VolumeID: testVolID,
		Version: "1", Automount: boolPtr(true)}, topo)
	mustBlock(t, got, NoticeDeviceMissing)
}

// TestAssessConfigureAppliesOnlyGivenFields 钉住“缺省不等于 false”：
// 只传 automount 时，nodev/nosuid 必须保持记录里的值。
func TestAssessConfigureAppliesOnlyGivenFields(t *testing.T) {
	current := DefaultVolumeConfig()
	got, err := VolumeConfigureParams{VolumeID: testVolID, Automount: boolPtr(true)}.Apply(current)
	if err != nil {
		t.Fatalf("Apply 失败: %v", err)
	}
	if !got.Automount || !got.Nodev || !got.Nosuid || got.ReadOnly {
		t.Errorf("叠加结果不对：%+v", got)
	}
	// 显式 false 必须生效。
	got, err = VolumeConfigureParams{Nodev: boolPtr(false), Nosuid: boolPtr(false)}.Apply(current)
	if err != nil {
		t.Fatalf("Apply 失败: %v", err)
	}
	if got.Nodev || got.Nosuid {
		t.Errorf("显式 false 没有生效：%+v", got)
	}
	// 什么都没给时保持原样（它仍然是合法的修复手段）。
	got, err = VolumeConfigureParams{}.Apply(current)
	if err != nil {
		t.Fatalf("Apply 失败: %v", err)
	}
	if got != current {
		t.Errorf("空参数改变了配置：%+v", got)
	}
}

// TestAssessVolumeActionsBindTargetIdentity 钉住一条跨动作的不变量：只要设备在位，
// **每个**卷动作的预检都必须绑定目标身份指纹。
//
// P07.4 的真机验收在 configure 上第一次撞到它：判决放行了、阻塞项也没有，但计划里
// 没有指纹，代理的 validatePlanResult 判“预检必须绑定一个可复核的目标”，面板侧收到
// 500。当时四个单动作用例都在，漏的是“每个动作都要有”这一条——所以这里按动作表遍历，
// 以后新增卷动作时这个循环会自动把它带上。
func TestAssessVolumeActionsBindTargetIdentity(t *testing.T) {
	rec := testRecord(testVolID, "data", "sdb1", testUUID)
	cases := []struct {
		action        string
		notRegistered bool // 导入时卷还没登记，fstab 里也没有它的条目
		mounted       bool
		params        func() any
	}{
		{action: ActionVolumeImport, notRegistered: true,
			params: func() any { return VolumeImportParams{DeviceID: "sdb1", Slug: "data"} }},
		{action: ActionVolumeMount,
			params: func() any { return VolumeParams{VolumeID: rec.ID, Version: "1"} }},
		{action: ActionVolumeUnmount, mounted: true,
			params: func() any { return VolumeParams{VolumeID: rec.ID, Version: "1"} }},
		{action: ActionVolumeConfigure,
			params: func() any {
				return VolumeConfigureParams{VolumeID: rec.ID, Version: "1", Automount: boolPtr(true)}
			}},
		{action: ActionVolumeForget,
			params: func() any { return VolumeParams{VolumeID: rec.ID, Version: "1"} }},
	}
	for _, tc := range cases {
		t.Run(tc.action, func(t *testing.T) {
			a := newAssessFixture(t, fstabWith(t, rec.FstabEntry())).allowMountPoint("data")
			if tc.notRegistered {
				a.f.store.recs = map[string]VolumeRecord{}
				a.swapFstab(t, existingFstab)
			} else {
				a.withRecord(rec)
			}
			a.f.super.put("sdb1", testUUID, FilesystemExt4)
			topo := volumeTopo("sha256:topo1", testUUID)
			if tc.mounted {
				mountDeviceAt(findNode(&topo, "sdb1"), rec.MountPath())
			}
			got := a.assess(t, tc.action, tc.params(), topo)
			mustAllow(t, got)
			if got.IdentityFingerprint == "" {
				t.Errorf("%s 放行了却没有绑定目标身份指纹：代理会判“预检结果不合法”，面板侧 500",
					tc.action)
			}
			if got.StateVersion == "" {
				t.Errorf("%s 放行了却没有状态版本：提交时无法判断预检是否过期", tc.action)
			}
		})
	}
}

// ---------- 退出管理 ----------

func TestAssessForget(t *testing.T) {
	rec := testRecord(testVolID, "data", "sdb1", testUUID)

	t.Run("挂着时拒绝", func(t *testing.T) {
		a := newAssessFixture(t, fstabWith(t, rec.FstabEntry())).allowMountPoint("data").withRecord(rec)
		a.f.super.put("sdb1", testUUID, FilesystemExt4)
		topo := volumeTopo("sha256:topo1", testUUID)
		mountDeviceAt(findNode(&topo, "sdb1"), rec.MountPath())
		got := a.assess(t, ActionVolumeForget, VolumeParams{VolumeID: rec.ID, Version: "1"}, topo)
		mustBlock(t, got, NoticeVolumeMounted)
	})

	t.Run("没挂载时放行", func(t *testing.T) {
		a := newAssessFixture(t, fstabWith(t, rec.FstabEntry())).allowMountPoint("data").withRecord(rec)
		a.f.super.put("sdb1", testUUID, FilesystemExt4)
		got := a.assess(t, ActionVolumeForget, VolumeParams{VolumeID: rec.ID, Version: "1"},
			volumeTopo("sha256:topo1", testUUID))
		mustAllow(t, got)
		locks := strings.Join(got.Locks, ",")
		if !strings.Contains(locks, "fstab") || !strings.Contains(locks, "volume:"+testVolID) {
			t.Errorf("锁 = %v，forget 必须同时锁 fstab 与卷", got.Locks)
		}
	})

	t.Run("盘拔掉了也能退出管理", func(t *testing.T) {
		a := newAssessFixture(t, fstabWith(t, rec.FstabEntry())).allowMountPoint("data").withRecord(rec)
		topo := volumeTopo("sha256:topo1", testUUID)
		deleteNode(&topo, "sdb1")
		got := a.assess(t, ActionVolumeForget, VolumeParams{VolumeID: rec.ID, Version: "1"}, topo)
		mustAllow(t, got)
	})
}

// ---------- 观察 ----------

func TestObserveVolumeStates(t *testing.T) {
	rec := testRecord(testVolID, "data", "sdb1", testUUID)

	run := func(mutate func(*Topology, *fakeSuperblocks), super func(*fakeSuperblocks)) VolumeObservation {
		a := newAssessFixture(t, fstabWith(t, rec.FstabEntry())).allowMountPoint("data").withRecord(rec)
		a.f.super.put("sdb1", testUUID, FilesystemExt4)
		topo := volumeTopo("sha256:topo1", testUUID)
		if mutate != nil {
			mutate(&topo, a.f.super)
		}
		if super != nil {
			super(a.f.super)
		}
		return observeVolume(context.Background(), topo, indexTopology(topo), rec, a.env)
	}

	if got := run(nil, nil); got.State != VolumeStateUnmounted {
		t.Errorf("未挂载时状态 = %q", got.State)
	}
	if got := run(func(topo *Topology, _ *fakeSuperblocks) {
		mountDeviceAt(findNode(topo, "sdb1"), rec.MountPath())
	}, nil); got.State != VolumeStateMounted || got.MountedElsewhere {
		t.Errorf("挂着时状态 = %q elsewhere=%v", got.State, got.MountedElsewhere)
	}
	if got := run(func(topo *Topology, _ *fakeSuperblocks) {
		mountDeviceAt(findNode(topo, "sdb1"), "/mnt/old")
	}, nil); got.State != VolumeStateMounted || !got.MountedElsewhere {
		t.Errorf("挂在别处时状态 = %q elsewhere=%v", got.State, got.MountedElsewhere)
	}
	if got := run(func(topo *Topology, _ *fakeSuperblocks) { deleteNode(topo, "sdb1") }, nil); got.State != VolumeStateMissing {
		t.Errorf("设备不在时状态 = %q", got.State)
	}
	if got := run(nil, func(s *fakeSuperblocks) {
		s.byDevice["sdb1"] = FilesystemIdentity{Kind: FilesystemExt4, UUID: testUUID2}
	}); got.State != VolumeStateMismatch {
		t.Errorf("UUID 不符时状态 = %q", got.State)
	}
	if got := run(nil, func(s *fakeSuperblocks) {
		s.errs = map[string]error{"sdb1": errors.New("读失败")}
	}); got.State != VolumeStateError {
		t.Errorf("读超级块失败时状态 = %q", got.State)
	}
	// 没有超级块读取器时是 unknown，而不是当作“没有文件系统”。
	a := newAssessFixture(t, fstabWith(t, rec.FstabEntry())).allowMountPoint("data").withRecord(rec)
	a.env.Superblocks = nil
	topo := volumeTopo("sha256:topo1", testUUID)
	if got := observeVolume(context.Background(), topo, indexTopology(topo), rec, a.env); got.State != VolumeStateUnknown {
		t.Errorf("没有读取器时状态 = %q，应当是 unknown", got.State)
	}
}

// ---------- fstab 外部条目 ----------

func TestExternalFstabRefs(t *testing.T) {
	content := existingFstab + fstabWith(t, FstabEntry{
		UUID: testUUID, MountPoint: "/srv/upanel/volumes/data", FSType: FilesystemExt4,
		Options: []string{"rw"}, Pass: 2,
	})[len(existingFstab):]

	// 托管块里的这一条不算外部条目。
	if refs := ExternalFstabRefs(content, testUUID, "/srv/upanel/volumes/data"); len(refs) != 0 {
		t.Fatalf("托管块里的条目被当成了外部条目：%+v", refs)
	}

	withManual := content + "UUID=" + testUUID + " /mnt/manual ext4 defaults 0 2\n"
	refs := ExternalFstabRefs(withManual, testUUID, "")
	if len(refs) != 1 {
		t.Fatalf("外部条目数 = %d，想要 1：%+v", len(refs), refs)
	}
	if refs[0].Line != len(splitLines(withManual)) {
		t.Errorf("行号 = %d，想要 %d", refs[0].Line, len(splitLines(withManual)))
	}

	// 注释行与空行不算。
	commented := content + "# UUID=" + testUUID + " /mnt/manual ext4 defaults 0 2\n\n"
	if refs := ExternalFstabRefs(commented, testUUID, ""); len(refs) != 0 {
		t.Errorf("注释行被当成了外部条目：%+v", refs)
	}

	// 挂在同一个挂载点上的外部条目也算冲突。
	byMount := content + "/dev/sdc1 /srv/upanel/volumes/data ext4 defaults 0 2\n"
	if refs := ExternalFstabRefs(byMount, testUUID, "/srv/upanel/volumes/data"); len(refs) != 1 {
		t.Errorf("按挂载点没有找到外部条目：%+v", refs)
	}
}

// ---------- 挂载点路径 ----------

func TestCheckMountPointPath(t *testing.T) {
	env := func(tree *fakeTree) VolumeEnv {
		return VolumeEnv{Lstat: tree.lstat, ReadDir: tree.readDir}
	}

	t.Run("干静的路径没有意见", func(t *testing.T) {
		tree := newFakeTree().dir(DefaultVolumeRoot, 0o755)
		if msgs := checkMountPointPath(env(tree), DefaultVolumeRoot+"/data"); len(msgs) != 0 {
			t.Errorf("不该有意见：%v", msgs)
		}
	})

	t.Run("不在受管目录下", func(t *testing.T) {
		tree := newFakeTree().dir("/srv", 0o755)
		msgs := checkMountPointPath(env(tree), "/srv/data")
		if len(msgs) == 0 {
			t.Fatal("受管目录之外的挂载点被接受了")
		}
	})

	t.Run("不是规范路径", func(t *testing.T) {
		tree := newFakeTree().dir(DefaultVolumeRoot, 0o755)
		msgs := checkMountPointPath(env(tree), DefaultVolumeRoot+"/data/../data")
		if len(msgs) == 0 {
			t.Fatal("非规范路径被接受了")
		}
	})

	t.Run("路径上有符号链接", func(t *testing.T) {
		tree := newFakeTree().symlink("/srv/upanel")
		msgs := checkMountPointPath(env(tree), DefaultVolumeRoot+"/data")
		if len(msgs) == 0 {
			t.Fatal("符号链接被接受了")
		}
		mustContain(t, strings.Join(msgs, "；"), "符号链接", "理由")
	})

	t.Run("上级不是目录", func(t *testing.T) {
		tree := newFakeTree().file("/srv/upanel")
		msgs := checkMountPointPath(env(tree), DefaultVolumeRoot+"/data")
		if len(msgs) == 0 {
			t.Fatal("上级是普通文件却被接受了")
		}
	})

	t.Run("父目录允许任何用户写入", func(t *testing.T) {
		tree := newFakeTree().dir(DefaultVolumeRoot, 0o777)
		msgs := checkMountPointPath(env(tree), DefaultVolumeRoot+"/data")
		if len(msgs) == 0 {
			t.Fatal("世界可写的父目录被接受了")
		}
		mustContain(t, strings.Join(msgs, "；"), "任何用户写入", "理由")
	})

	t.Run("父目录属主不是 root", func(t *testing.T) {
		if runtime.GOOS != "linux" {
			t.Skip("属主检查需要 syscall.Stat_t 的 Uid/Gid，只在 Linux 上可断言")
		}
		tree := newFakeTree()
		tree.dir(DefaultVolumeRoot, 0o755)
		fi := tree.info[DefaultVolumeRoot]
		fi.uid = 1000
		tree.info[DefaultVolumeRoot] = fi
		msgs := checkMountPointPath(env(tree), DefaultVolumeRoot+"/data")
		if len(msgs) == 0 {
			t.Fatal("属主不是 root 的父目录被接受了")
		}
		mustContain(t, strings.Join(msgs, "；"), "uid 1000", "理由")
	})
}

// ---------- 夹具辅助 ----------

// findNode 取拓扑里的某台设备（测试内部用）。
func findNode(topo *Topology, id string) *Device {
	for i := range topo.Nodes {
		if topo.Nodes[i].ID == id {
			return &topo.Nodes[i]
		}
	}
	panic("拓扑里没有设备 " + id)
}

// deleteNode 把设备从拓扑里去掉（模拟拔盘），并清掉指向它的边。
func deleteNode(topo *Topology, id string) {
	nodes := topo.Nodes[:0]
	for _, n := range topo.Nodes {
		if n.ID != id {
			nodes = append(nodes, n)
		}
	}
	topo.Nodes = nodes
	edges := topo.Edges[:0]
	for _, e := range topo.Edges {
		if e.FromDeviceID != id && e.ToDeviceID != id {
			edges = append(edges, e)
		}
	}
	topo.Edges = edges
}

// writeFstab 把给定内容写进临时文件并返回路径（用于“fstab 里已经有别人的行”的用例）。
func writeFstab(t *testing.T, content string) string {
	t.Helper()
	path := t.TempDir() + "/fstab"
	if err := writeTestFile(path, content, 0o644); err != nil {
		t.Fatalf("%v", err)
	}
	return path
}

func boolPtr(b bool) *bool { return &b }

// hasNotice 报告警告列表里有没有某个码。
func hasNotice(list []Blocker, code string) bool {
	for _, b := range list {
		if b.Code == code {
			return true
		}
	}
	return false
}
