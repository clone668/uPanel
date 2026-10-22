package storage

// 卷动作执行的测试（P07.4）。
//
// 这一层最值得钉的不是“成功路径写了什么”，而是**每一步失败之后世界长什么样**：
// fstab 必须回到原文，登记表不能留下半条记录，卷不能停在一个既没挂上也没人认领的
// 状态里。所以每个用例都同时断言三样：命令序列、fstab 内容、登记表状态。
//
// 所有路径都在临时目录里造：测试绝不碰宿主机的 /etc/fstab 与 /srv。

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"upanel/internal/httpx"
	"upanel/internal/syscmd"
)

// ---------- 假扫描器 ----------

// fakeVolumeScanner 是一份会跟着命令变的拓扑：mount 挂上、umount 卸掉。
type fakeVolumeScanner struct {
	mu      sync.Mutex
	nodes   []Device
	edges   []Edge
	mounted map[string]string // 设备 → 挂载点
	err     error
	scans   int
}

func newFakeVolumeScanner(nodes ...Device) *fakeVolumeScanner {
	s := &fakeVolumeScanner{mounted: map[string]string{}}
	s.setNodes(nodes...)
	return s
}

func (s *fakeVolumeScanner) setNodes(nodes ...Device) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nodes = nodes
	s.edges = nil
	for _, d := range nodes {
		if d.Kind != KindPartition {
			continue
		}
		for _, parent := range nodes {
			if parent.ID == d.ID {
				continue
			}
			if _, ok := partitionEntry(&parent, d.ID); ok {
				s.edges = append(s.edges, Edge{FromDeviceID: d.ID, ToDeviceID: parent.ID, Relation: RelationPartitionOf})
				break
			}
		}
	}
}

func (s *fakeVolumeScanner) mount(deviceID, mountPoint string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mounted[deviceID] = mountPoint
}

func (s *fakeVolumeScanner) umount(deviceID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.mounted, deviceID)
}

func (s *fakeVolumeScanner) ScanNow(ctx context.Context) (Topology, error) {
	if err := ctx.Err(); err != nil {
		return Topology{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scans++
	if s.err != nil {
		return Topology{}, s.err
	}
	nodes := make([]Device, len(s.nodes))
	copy(nodes, s.nodes)
	for i := range nodes {
		fs := Filesystem{Mounts: []Mount{}}
		if nodes[i].Filesystem != nil {
			fs.Type = nodes[i].Filesystem.Type
			fs.UUID = nodes[i].Filesystem.UUID
			fs.Label = nodes[i].Filesystem.Label
		}
		if mp, ok := s.mounted[nodes[i].ID]; ok {
			fs.Mounts = []Mount{{Path: mp, Source: "/dev/" + nodes[i].ID, FSType: FilesystemExt4,
				Options: []string{"rw"}}}
		}
		nodes[i].Filesystem = &fs
	}
	return Topology{
		Status:          StatusOK,
		SnapshotVersion: "sha256:topo1",
		IntervalMS:      5000,
		Nodes:           nodes,
		Edges:           append([]Edge(nil), s.edges...),
		NetworkMounts:   []NetworkMount{},
	}, nil
}

// ---------- 夹具 ----------

// volumeWriter 是“一整套可执行的卷管理”：临时目录里的 fstab、会变的拓扑、
// 登记表、超级块、以及按程序名分派的假命令。
type volumeWriter struct {
	dir     string
	fstab   string
	tree    *fakeTree
	w       *writer
	runner  *fakeVolumeRunner
	store   *fakeVolumeStore
	super   *fakeSuperblocks
	scanner *fakeVolumeScanner
	now     time.Time
	// fsyncedDirs 记录目录 fsync（顺序证据；Windows 上真实 fsync 目录会失败）。
	fsyncedDirs []string
	// 每种命令的“行为开关”（由用例设置）。
	mountFails   bool
	mountNoop    bool // mount 返回 0 但没挂上
	umountFails  bool
	umountBusy   bool
	reloadFails  bool
	findmntFails bool
	// findmntSaysAbsent 让 findmnt 报“没挂载”（rc=1），但拓扑扫描仍然显示挂着：
	// 这是专门用来单测第二来源（重新扫描）的开关。
	findmntSaysAbsent bool
	unitState         string
	uuidOnMountPt     string // findmnt 读回的 UUID（空表示用登记的那个）
}

const writeVolID = "vol_0123456789abcdef0123456789abcdef"

// newVolumeWriter 造一套默认世界：sdb1 上是 ext4 + testUUID，什么都没挂。
func newVolumeWriter(t *testing.T) *volumeWriter {
	t.Helper()
	dir := strings.ReplaceAll(t.TempDir(), "\\", "/")
	vw := &volumeWriter{
		dir:       dir,
		fstab:     dir + "/fstab",
		tree:      newFakeTree().dir(DefaultVolumeRoot, 0o755),
		runner:    newFakeVolumeRunner(),
		store:     newFakeVolumeStore(),
		super:     newFakeSuperblocks(),
		now:       time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC),
		unitState: "loaded",
	}
	disk := blankDisk("sdb", 8<<30)
	part := addPartition(&disk, "sdb1", 1, 2*mib, 7<<30)
	part.Filesystem = &Filesystem{Type: strPtr(FilesystemExt4), UUID: strPtr(testUUID), Mounts: []Mount{}}
	vw.scanner = newFakeVolumeScanner(disk, part)
	vw.super.put("sdb1", testUUID, FilesystemExt4)

	if err := writeTestFile(vw.fstab, existingFstab, 0o644); err != nil {
		t.Fatalf("%v", err)
	}
	vw.wire(t)
	return vw
}

// wire 按当前的开关重装命令反应与 writer（用例改完开关后再调一次即可）。
func (vw *volumeWriter) wire(t *testing.T) {
	t.Helper()
	runner := vw.runner
	runner.handlers = map[string]func([]string) (syscmd.Result, error){}

	// findmnt：--verify 校验候选文件；否则读挂载点的事实。
	runner.on("findmnt", func(args []string) (syscmd.Result, error) {
		if vw.findmntFails {
			return syscmd.Result{ExitCode: 1, Stderr: "unknown option"}, &syscmd.ExitError{Program: "findmnt", ExitCode: 1}
		}
		if len(args) > 0 && args[0] == "--verify" {
			return syscmd.Result{ExitCode: 0}, nil
		}
		if vw.findmntSaysAbsent {
			return syscmd.Result{ExitCode: 1}, &syscmd.ExitError{Program: "findmnt", ExitCode: 1}
		}
		target := args[len(args)-1]
		vw.scanner.mu.Lock()
		deviceID := ""
		for dev, mp := range vw.scanner.mounted {
			if mp == target {
				deviceID = dev
			}
		}
		vw.scanner.mu.Unlock()
		if deviceID == "" {
			return syscmd.Result{ExitCode: 1}, &syscmd.ExitError{Program: "findmnt", ExitCode: 1}
		}
		uuid := testUUID
		if vw.uuidOnMountPt != "" {
			uuid = vw.uuidOnMountPt
		}
		return syscmd.Result{ExitCode: 0, Stdout: uuid + " /dev/" + deviceID + " ext4 rw,nodev\n"}, nil
	})

	// mount：成功时把挂载点记到拓扑里（除非 noop）。
	runner.on("mount", func(args []string) (syscmd.Result, error) {
		if vw.mountFails {
			return syscmd.Result{ExitCode: 32, Stderr: "mount: wrong fs type"},
				&syscmd.ExitError{Program: "mount", ExitCode: 32}
		}
		if !vw.mountNoop {
			vw.scanner.mount("sdb1", args[len(args)-1])
		}
		return syscmd.Result{ExitCode: 0}, nil
	})

	// umount：busy 时返回 32；成功时从拓扑里删掉。
	runner.on("umount", func(args []string) (syscmd.Result, error) {
		if vw.umountFails || vw.umountBusy {
			msg := "umount: /x: not mounted"
			if vw.umountBusy {
				msg = "umount: /x: target is busy"
			}
			return syscmd.Result{ExitCode: 32, Stderr: msg}, &syscmd.ExitError{Program: "umount", ExitCode: 32}
		}
		vw.scanner.mu.Lock()
		for dev, mp := range vw.scanner.mounted {
			if mp == args[len(args)-1] {
				delete(vw.scanner.mounted, dev)
			}
		}
		vw.scanner.mu.Unlock()
		return syscmd.Result{ExitCode: 0}, nil
	})

	// systemctl：daemon-reload 与 show -p LoadState。
	runner.on("systemctl", func(args []string) (syscmd.Result, error) {
		switch {
		case len(args) > 0 && args[0] == "daemon-reload":
			if vw.reloadFails {
				return syscmd.Result{ExitCode: 1, Stderr: "Failed to reload"},
					&syscmd.ExitError{Program: "systemctl", ExitCode: 1}
			}
			return syscmd.Result{ExitCode: 0}, nil
		case len(args) > 0 && args[0] == "show":
			return syscmd.Result{ExitCode: 0, Stdout: vw.unitState + "\n"}, nil
		}
		return syscmd.Result{ExitCode: 0}, nil
	})

	vw.w = newWriter(WriteOptions{
		Scanner:     vw.scanner,
		Runner:      runner,
		Superblocks: vw.super,
		Store:       vw.store,
		Fstab: FstabStore{
			Path:      vw.fstab,
			BackupDir: vw.dir + "/backups",
			Runner:    runner,
			Findmnt:   "/usr/bin/findmnt",
			Now:       func() time.Time { return vw.now },
			// 目录 fsync 在 Windows 上是 Access is denied；生产只有 Linux，
			// 所以这里换成记录器，顺带让“rename 之后 fsync 了目录”可断言。
			fsyncDir: func(d string) error {
				vw.fsyncedDirs = append(vw.fsyncedDirs, d)
				return nil
			},
		},
		// 挂载点走到假文件系统上：路径还是生产的 /srv/upanel/volumes，
		// 但那里有没有目录、权限多少由 tree 说了算。
		Lstat:     vw.tree.lstat,
		ReadDir:   vw.tree.readDir,
		MkdirAll:  vw.tree.mkdirAll,
		Chmod:     vw.tree.chmod,
		Mount:     "/usr/bin/mount",
		Umount:    "/usr/bin/umount",
		Systemctl: "/usr/bin/systemctl",
		Findmnt:   "/usr/bin/findmnt",
		Env:       Env{Busy: &fakeBusy{}},
		Now:       func() time.Time { return vw.now },
	})
}

// preflight 按真实链路走一次预检，把身份与版本绑进请求里。
func (vw *volumeWriter) preflight(t *testing.T, action string, params any) WriteRequest {
	t.Helper()
	topo, err := vw.scanner.ScanNow(context.Background())
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	a, err := AssessVolume(context.Background(), topo, action, params, vw.w.volumeEnv())
	if err != nil {
		t.Fatalf("预检失败: %v", err)
	}
	if !a.Allowed {
		t.Fatalf("预检没放行：%+v", a.Blockers)
	}
	return WriteRequest{
		Action:               action,
		Params:               params,
		ExpectedIdentity:     a.IdentityFingerprint,
		ExpectedStateVersion: a.StateVersion,
		OperationID:          "op_test_1",
	}
}

// execute 跑一次执行。
func (vw *volumeWriter) execute(req WriteRequest) (WriteReport, error) {
	return vw.w.execute(context.Background(), req)
}

// fstabContent 读回当前 fstab 原文。
func (vw *volumeWriter) fstabContent(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(vw.fstab)
	if err != nil {
		t.Fatalf("读 fstab 失败: %v", err)
	}
	return string(data)
}

// mustFstabUnchanged 断言 fstab 还是原文。
func (vw *volumeWriter) mustFstabUnchanged(t *testing.T) {
	t.Helper()
	if got := vw.fstabContent(t); got != existingFstab {
		t.Fatalf("fstab 没有回到原文：\n%s", got)
	}
}

// register 把一条卷记录放进登记表，并（可选）在 fstab 里写好它的条目。
func (vw *volumeWriter) register(t *testing.T, rec VolumeRecord, withFstab bool) {
	t.Helper()
	vw.store.recs[rec.ID] = rec
	if !withFstab {
		return
	}
	if err := writeTestFile(vw.fstab, fstabWith(t, rec.FstabEntry()), 0o644); err != nil {
		t.Fatalf("%v", err)
	}
}

// ---------- 导入 ----------

func TestVolumeImportWritesFstabAndRecord(t *testing.T) {
	vw := newVolumeWriter(t)
	req := vw.preflight(t, ActionVolumeImport, VolumeImportParams{DeviceID: "sdb1", Slug: "data"})

	report, err := vw.execute(req)
	if err != nil {
		t.Fatalf("导入失败: %v", err)
	}

	// fstab：受管块里有一条按 UUID 的条目，块外原文逐字节保留。
	content := vw.fstabContent(t)
	wantLine := "UUID=" + testUUID + " " + MountPathOf("data") + " ext4 "
	mustContain(t, content, wantLine, "fstab")
	mustContain(t, content, existingFstab, "fstab（块外）")

	// 登记表：一条记录，version=1，未挂载，带操作号。
	if len(vw.store.recs) != 1 {
		t.Fatalf("登记记录数 = %d，想要 1", len(vw.store.recs))
	}
	var rec VolumeRecord
	for _, r := range vw.store.recs {
		rec = r
	}
	if rec.Version != "1" || rec.DesiredState != DesiredUnmounted {
		t.Errorf("记录 = %+v", rec)
	}
	if rec.LastOperationID != "op_test_1" {
		t.Errorf("last_operation_id = %q", rec.LastOperationID)
	}
	if rec.FilesystemUUID != testUUID || rec.FilesystemType != FilesystemExt4 {
		t.Errorf("记录里的文件系统信息不对：%+v", rec)
	}
	if !strings.HasSuffix(rec.MountPath(), "/data") {
		t.Errorf("挂载点 = %q", rec.MountPath())
	}

	// 核验来源：fstab 读回 + systemctl show。
	if got := observedString(t, report, "checked_via"); !strings.Contains(got, "fstab_reread") ||
		!strings.Contains(got, "systemctl_show") {
		t.Errorf("checked_via = %q", got)
	}

	// 命令序列：findmnt --verify（候选文件）→ systemctl daemon-reload → systemctl show。
	programs := vw.runner.programs()
	want := []string{"findmnt", "systemctl", "systemctl"}
	if strings.Join(programs, ",") != strings.Join(want, ",") {
		t.Errorf("导入的命令序列 = %v，想要 %v", programs, want)
	}
	if len(vw.runner.ran("mount"))+len(vw.runner.ran("umount")) != 0 {
		t.Error("导入执行了挂载/卸载")
	}
	// 备份路径必须交回来（审计与人工恢复都靠它）。
	if backup, _ := report.Data["fstab_backup"].(string); backup == "" {
		t.Error("结果里没有 fstab 备份路径")
	} else if _, statErr := os.Stat(backup); statErr != nil {
		t.Errorf("备份文件不存在：%v", statErr)
	}
	// 挂载点目录被建出来了（路径是生产的 /srv/upanel/volumes/…，但落到假文件系统上）。
	if fi, statErr := vw.tree.lstat(rec.MountPath()); statErr != nil || !fi.IsDir() {
		t.Errorf("挂载点目录没有建出来：%v", statErr)
	}
}

func observedString(t *testing.T, report WriteReport, key string) string {
	t.Helper()
	v, ok := report.Observed[key]
	if !ok {
		t.Fatalf("observed 里没有 %q：%+v", key, report.Observed)
	}
	parts, _ := v.([]string)
	return strings.Join(parts, ",")
}

func TestVolumeImportKeepsOtherVolumes(t *testing.T) {
	vw := newVolumeWriter(t)
	other := testRecord(writeVolID, "zeta", "sdc1", testUUID2)
	vw.store.recs[other.ID] = other
	if err := writeTestFile(vw.fstab, fstabWith(t, other.FstabEntry()), 0o644); err != nil {
		t.Fatalf("%v", err)
	}

	req := vw.preflight(t, ActionVolumeImport, VolumeImportParams{DeviceID: "sdb1", Slug: "data"})
	if _, err := vw.execute(req); err != nil {
		t.Fatalf("导入失败: %v", err)
	}
	content := vw.fstabContent(t)
	mustContain(t, content, testUUID2, "fstab（另一个卷的条目）")
	mustContain(t, content, testUUID, "fstab（新卷的条目）")
	if _, ok := vw.store.recs[writeVolID]; !ok {
		t.Error("导入把别的卷的登记记录弄丢了")
	}
}

func TestVolumeImportRollsBackWhenSaveFails(t *testing.T) {
	vw := newVolumeWriter(t)
	vw.store.saveErr = errors.New("数据库只读")
	req := vw.preflight(t, ActionVolumeImport, VolumeImportParams{DeviceID: "sdb1", Slug: "data"})

	_, err := vw.execute(req)
	if err == nil {
		t.Fatal("登记失败却返回成功")
	}
	mustContain(t, err.Error(), "恢复", "错误消息")
	// fstab 必须回到原文，登记表里不能有记录。
	vw.mustFstabUnchanged(t)
	if len(vw.store.recs) != 0 {
		t.Errorf("回滚之后还留着 %d 条记录", len(vw.store.recs))
	}
	// 回滚之后 systemd 也要回到旧视图。
	if n := countArgs(vw.runner.ran("systemctl"), "daemon-reload"); n != 2 {
		t.Errorf("daemon-reload 次数 = %d，想要 2（写入一次 + 回滚一次）", n)
	}
}

func TestVolumeImportRollsBackWhenReloadFails(t *testing.T) {
	vw := newVolumeWriter(t)
	vw.reloadFails = true
	vw.wire(t)
	req := vw.preflight(t, ActionVolumeImport, VolumeImportParams{DeviceID: "sdb1", Slug: "data"})

	_, err := vw.execute(req)
	if err == nil {
		t.Fatal("daemon-reload 失败却返回成功")
	}
	mustContain(t, err.Error(), "daemon-reload", "错误消息")
	vw.mustFstabUnchanged(t)
	if len(vw.store.recs) != 0 {
		t.Error("回滚之后登记表里还有记录")
	}
}

func TestVolumeImportRejectsWhenVerifyFails(t *testing.T) {
	vw := newVolumeWriter(t)
	vw.findmntFails = true
	vw.wire(t)
	req := vw.preflight(t, ActionVolumeImport, VolumeImportParams{DeviceID: "sdb1", Slug: "data"})

	_, err := vw.execute(req)
	if err == nil {
		t.Fatal("候选文件校验失败却返回成功")
	}
	vw.mustFstabUnchanged(t)
	if len(vw.store.recs) != 0 {
		t.Error("校验失败之后写了登记记录")
	}
	// 校验失败时不该 daemon-reload（文件根本没换）。
	if n := countArgs(vw.runner.ran("systemctl"), "daemon-reload"); n != 0 {
		t.Errorf("校验失败却 daemon-reload 了 %d 次", n)
	}
}

func TestVolumeImportRollsBackWhenSystemdDoesNotKnowUnit(t *testing.T) {
	vw := newVolumeWriter(t)
	vw.unitState = ""
	vw.wire(t)
	req := vw.preflight(t, ActionVolumeImport, VolumeImportParams{DeviceID: "sdb1", Slug: "data"})

	_, err := vw.execute(req)
	if err == nil {
		t.Fatal("systemd 不认识单元却返回成功")
	}
	mustContain(t, err.Error(), "LoadState", "错误消息")
	vw.mustFstabUnchanged(t)
}

// TestVolumeImportRejectsWhenSuperblockChanged 钉住“写之前再读一次盘上的字节”：
// 预检之后有人把盘换成了别的文件系统，UUID 变了就不许写 fstab。
func TestVolumeImportRejectsWhenSuperblockChanged(t *testing.T) {
	vw := newVolumeWriter(t)
	req := vw.preflight(t, ActionVolumeImport, VolumeImportParams{DeviceID: "sdb1", Slug: "data"})
	vw.super.put("sdb1", testUUID2, FilesystemExt4)

	_, err := vw.execute(req)
	if err == nil {
		t.Fatal("UUID 变了却仍然写了 fstab")
	}
	// 文件系统 UUID 是算进导入的身份指纹里的：盘上被重新格式化就是“目标身份变了”，
	// 不是普通命令失败——用户批准的是那个 UUID 的分区，换了一个就不该写。
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.Code != httpx.CodeDeviceIdentityChanged {
		t.Errorf("错误码不对（想要 DEVICE_IDENTITY_CHANGED）：%v", err)
	}
	vw.mustFstabUnchanged(t)
}

// ---------- 挂载 ----------

func TestVolumeMount(t *testing.T) {
	vw := newVolumeWriter(t)
	rec := testRecord(writeVolID, "data", "sdb1", testUUID)
	vw.store.recs[rec.ID] = rec
	if err := writeTestFile(vw.fstab, fstabWith(t, rec.FstabEntry()), 0o644); err != nil {
		t.Fatalf("%v", err)
	}

	req := vw.preflight(t, ActionVolumeMount, VolumeParams{VolumeID: rec.ID, Version: "1"})
	report, err := vw.execute(req)
	if err != nil {
		t.Fatalf("挂载失败: %v", err)
	}

	// mount 只收到挂载点：设备由 fstab 里的 UUID 决定，不走设备路径。
	mounts := vw.runner.ran("mount")
	if len(mounts) != 1 {
		t.Fatalf("mount 调用次数 = %d", len(mounts))
	}
	if got := mounts[0].Args; len(got) != 1 || !strings.HasSuffix(got[0], "/data") {
		t.Errorf("mount 参数 = %v，应当只有挂载点", got)
	}
	// 两个来源都记在 checked_via 里。
	if got := observedString(t, report, "checked_via"); !strings.Contains(got, "findmnt") ||
		!strings.Contains(got, "topology_scan") {
		t.Errorf("checked_via = %q", got)
	}
	// 登记：desired_state=mounted，version 自增。
	got := vw.store.recs[rec.ID]
	if got.DesiredState != DesiredMounted {
		t.Errorf("desired_state = %q", got.DesiredState)
	}
	if got.Version != "2" {
		t.Errorf("version = %q，想要 2", got.Version)
	}
	if got := vw.scanner.mounted["sdb1"]; got != rec.MountPath() {
		t.Errorf("拓扑里的挂载点 = %q，想要 %q", got, rec.MountPath())
	}
}

func TestVolumeMountCompensatesWhenFindmntSaysNotMounted(t *testing.T) {
	vw := newVolumeWriter(t)
	rec := registeredVolume(t, vw, testRecord(writeVolID, "data", "sdb1", testUUID))
	vw.mountNoop = true
	vw.wire(t)

	req := vw.preflight(t, ActionVolumeMount, VolumeParams{VolumeID: rec.ID, Version: "1"})
	_, err := vw.execute(req)
	if err == nil {
		t.Fatal("mount 返回 0 但没挂上，却当成功了")
	}
	mustContain(t, err.Error(), "卸回未挂载状态", "错误消息")
	if n := len(vw.runner.ran("umount")); n != 1 {
		t.Errorf("补偿卸载次数 = %d，想要 1", n)
	}
	// 登记没有被改成 mounted。
	if vw.store.recs[rec.ID].DesiredState != DesiredUnmounted {
		t.Error("挂载失败却把 desired_state 改成了 mounted")
	}
}

func TestVolumeMountCompensatesOnUUIDMismatch(t *testing.T) {
	vw := newVolumeWriter(t)
	rec := registeredVolume(t, vw, testRecord(writeVolID, "data", "sdb1", testUUID))
	vw.uuidOnMountPt = testUUID2
	vw.wire(t)

	req := vw.preflight(t, ActionVolumeMount, VolumeParams{VolumeID: rec.ID, Version: "1"})
	_, err := vw.execute(req)
	if err == nil {
		t.Fatal("挂上的是别的 UUID，却当成功了")
	}
	mustContain(t, err.Error(), "UUID", "错误消息")
	if n := len(vw.runner.ran("umount")); n != 1 {
		t.Errorf("补偿卸载次数 = %d", n)
	}
}

func TestVolumeMountCompensatesWhenSaveFails(t *testing.T) {
	vw := newVolumeWriter(t)
	rec := registeredVolume(t, vw, testRecord(writeVolID, "data", "sdb1", testUUID))
	vw.store.saveErr = errors.New("数据库只读")

	req := vw.preflight(t, ActionVolumeMount, VolumeParams{VolumeID: rec.ID, Version: "1"})
	_, err := vw.execute(req)
	if err == nil {
		t.Fatal("登记失败却返回成功")
	}
	// 卷已经挂上了但登记没写进去 → 必须卸回去，避免“挂着一个没人认领的卷”。
	if n := len(vw.runner.ran("umount")); n != 1 {
		t.Fatalf("补偿卸载次数 = %d，想要 1：%v", n, err)
	}
	if vw.store.recs[rec.ID].DesiredState != DesiredUnmounted {
		t.Error("登记记录被改动却报失败")
	}
}

func TestVolumeMountRejectsWhenUUIDChanged(t *testing.T) {
	vw := newVolumeWriter(t)
	rec := registeredVolume(t, vw, testRecord(writeVolID, "data", "sdb1", testUUID))
	req := vw.preflight(t, ActionVolumeMount, VolumeParams{VolumeID: rec.ID, Version: "1"})
	vw.super.put("sdb1", testUUID2, FilesystemExt4)

	_, err := vw.execute(req)
	if err == nil {
		t.Fatal("盘上的 UUID 变了却仍然挂载")
	}
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.Code != httpx.CodePreflightStale {
		t.Errorf("错误码不对（应当是 PREFLIGHT_STALE）：%v", err)
	}
	if len(vw.runner.ran("mount")) != 0 {
		t.Error("UUID 不符却执行了 mount")
	}
}

func TestVolumeMountRejectsOnStaleStateVersion(t *testing.T) {
	vw := newVolumeWriter(t)
	rec := registeredVolume(t, vw, testRecord(writeVolID, "data", "sdb1", testUUID))
	req := vw.preflight(t, ActionVolumeMount, VolumeParams{VolumeID: rec.ID, Version: "1"})
	req.ExpectedStateVersion = "topo:sha256:other|vol:x@1|fstab:-|obs:unmounted"

	_, err := vw.execute(req)
	if err == nil {
		t.Fatal("状态版本不符却执行了")
	}
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.Code != httpx.CodePreflightStale {
		t.Errorf("错误码不对：%v", err)
	}
	if len(vw.runner.ran("mount")) != 0 {
		t.Error("预检过期却执行了 mount")
	}
}

func TestVolumeMountRejectsOnIdentityChange(t *testing.T) {
	vw := newVolumeWriter(t)
	rec := registeredVolume(t, vw, testRecord(writeVolID, "data", "sdb1", testUUID))
	req := vw.preflight(t, ActionVolumeMount, VolumeParams{VolumeID: rec.ID, Version: "1"})
	req.ExpectedIdentity = "sha256:disk-别的盘"

	_, err := vw.execute(req)
	if err == nil {
		t.Fatal("身份不符却执行了")
	}
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.Code != httpx.CodeDeviceIdentityChanged {
		t.Errorf("错误码不对：%v", err)
	}
}

// ---------- 卸载 ----------

func TestVolumeUnmount(t *testing.T) {
	vw := newVolumeWriter(t)
	rec := registeredVolume(t, vw, testRecord(writeVolID, "data", "sdb1", testUUID))
	vw.scanner.mount("sdb1", rec.MountPath())

	req := vw.preflight(t, ActionVolumeUnmount, VolumeParams{VolumeID: rec.ID, Version: "1"})
	report, err := vw.execute(req)
	if err != nil {
		t.Fatalf("卸载失败: %v", err)
	}
	if n := len(vw.runner.ran("umount")); n != 1 {
		t.Errorf("umount 次数 = %d", n)
	}
	if got := vw.store.recs[rec.ID]; got.DesiredState != DesiredUnmounted || got.Version != "2" {
		t.Errorf("登记 = %+v", got)
	}
	if moved, _ := report.Observed["mounted"].(bool); moved {
		t.Error("结果里 mounted 仍然是真")
	}
	// 卸载不该改 fstab。
	mustContain(t, vw.fstabContent(t), testUUID, "fstab（条目应保留）")
}

func TestVolumeUnmountBusyIsReportedWithMountPoint(t *testing.T) {
	vw := newVolumeWriter(t)
	rec := registeredVolume(t, vw, testRecord(writeVolID, "data", "sdb1", testUUID))
	vw.scanner.mount("sdb1", rec.MountPath())
	vw.umountBusy = true
	vw.wire(t)

	req := vw.preflight(t, ActionVolumeUnmount, VolumeParams{VolumeID: rec.ID, Version: "1"})
	_, err := vw.execute(req)
	if err == nil {
		t.Fatal("umount 返回 32 却当成功了")
	}
	mustContain(t, err.Error(), "target is busy", "错误消息")
	mustContain(t, err.Error(), "挂载点仍然存在", "错误消息（占用说明）")
	if got := vw.store.recs[rec.ID]; got.DesiredState != DesiredUnmounted || got.Version != "1" {
		t.Errorf("卸载失败却改了登记：%+v", got)
	}
}

func TestVolumeUnmountFailsWhenStillMountedAfterSuccess(t *testing.T) {
	vw := newVolumeWriter(t)
	rec := registeredVolume(t, vw, testRecord(writeVolID, "data", "sdb1", testUUID))
	vw.scanner.mount("sdb1", rec.MountPath())
	req := vw.preflight(t, ActionVolumeUnmount, VolumeParams{VolumeID: rec.ID, Version: "1"})
	// umount 假装成功、findmnt 也说没挂，但拓扑里它还在。
	// 这条用例因此只钉第二来源：重新扫描才是最后一道。
	vw.runner.on("umount", func([]string) (syscmd.Result, error) {
		return syscmd.Result{ExitCode: 0}, nil
	})
	vw.findmntSaysAbsent = true

	_, err := vw.execute(req)
	if err == nil {
		t.Fatal("umount 返回 0 但挂载点还在，却当成功了")
	}
	mustContain(t, err.Error(), "重新扫描", "错误消息")
	if vw.store.recs[rec.ID].Version != "1" {
		t.Error("核验失败却改了登记")
	}
}

// TestVolumeUnmountDoesNotRemountWhenSaveFails 钉住一处刻意的决定：
// 卸载已生效、登记写失败时**不自动挂回去**，而是在消息里说清“不要再执行卸载”。
func TestVolumeUnmountDoesNotRemountWhenSaveFails(t *testing.T) {
	vw := newVolumeWriter(t)
	rec := registeredVolume(t, vw, testRecord(writeVolID, "data", "sdb1", testUUID))
	vw.scanner.mount("sdb1", rec.MountPath())
	vw.store.saveErr = errors.New("数据库只读")

	req := vw.preflight(t, ActionVolumeUnmount, VolumeParams{VolumeID: rec.ID, Version: "1"})
	_, err := vw.execute(req)
	if err == nil {
		t.Fatal("登记失败却返回成功")
	}
	mustContain(t, err.Error(), "已卸载", "错误消息")
	mustContain(t, err.Error(), "不需要重新执行卸载", "错误消息")
	if n := len(vw.runner.ran("mount")); n != 0 {
		t.Errorf("卸载之后又执行了 %d 次 mount（不该自动挂回去）", n)
	}
}

// ---------- 配置 ----------

func TestVolumeConfigureRewritesEntry(t *testing.T) {
	vw := newVolumeWriter(t)
	rec := registeredVolume(t, vw, testRecord(writeVolID, "data", "sdb1", testUUID))
	yes := true
	req := vw.preflight(t, ActionVolumeConfigure, VolumeConfigureParams{
		VolumeID: rec.ID, Version: "1", Automount: &yes,
	})
	if _, err := vw.execute(req); err != nil {
		t.Fatalf("改配置失败: %v", err)
	}
	content := vw.fstabContent(t)
	if strings.Contains(content, "noauto") {
		t.Errorf("打开自动挂载之后条目里还有 noauto：\n%s", content)
	}
	if got := vw.store.recs[rec.ID]; !got.Config.Automount || got.Version != "2" {
		t.Errorf("登记 = %+v", got)
	}
}

func TestVolumeConfigureRollsBackWhenSaveFails(t *testing.T) {
	vw := newVolumeWriter(t)
	before := fstabWith(t, testRecord(writeVolID, "data", "sdb1", testUUID).FstabEntry())
	vw.store.recs[writeVolID] = testRecord(writeVolID, "data", "sdb1", testUUID)
	if err := writeTestFile(vw.fstab, before, 0o644); err != nil {
		t.Fatalf("%v", err)
	}
	vw.store.saveErr = errors.New("数据库只读")

	yes := true
	req := vw.preflight(t, ActionVolumeConfigure, VolumeConfigureParams{
		VolumeID: writeVolID, Version: "1", ReadOnly: &yes,
	})
	if _, err := vw.execute(req); err == nil {
		t.Fatal("登记失败却返回成功")
	}
	if got := vw.fstabContent(t); got != before {
		t.Errorf("回滚之后 fstab 不是原样：\n%s", got)
	}
}

// ---------- 退出管理 ----------

func TestVolumeForget(t *testing.T) {
	vw := newVolumeWriter(t)
	rec := registeredVolume(t, vw, testRecord(writeVolID, "data", "sdb1", testUUID))

	req := vw.preflight(t, ActionVolumeForget, VolumeParams{VolumeID: rec.ID, Version: "1"})
	report, err := vw.execute(req)
	if err != nil {
		t.Fatalf("退出管理失败: %v", err)
	}
	if _, ok := vw.store.recs[rec.ID]; ok {
		t.Error("登记记录没有删掉")
	}
	if strings.Contains(vw.fstabContent(t), testUUID) {
		t.Errorf("fstab 里还有这个 UUID 的条目：\n%s", vw.fstabContent(t))
	}
	if got := observedString(t, report, "checked_via"); !strings.Contains(got, "fstab_reread") {
		t.Errorf("checked_via = %q", got)
	}
	// 退出管理不动盘上数据，也不 mount/umount。
	if n := len(vw.runner.ran("mount")) + len(vw.runner.ran("umount")); n != 0 {
		t.Errorf("退出管理执行了 %d 次挂载操作", n)
	}
}

func TestVolumeForgetRollsBackWhenDeleteFails(t *testing.T) {
	vw := newVolumeWriter(t)
	rec := registeredVolume(t, vw, testRecord(writeVolID, "data", "sdb1", testUUID))
	before := vw.fstabContent(t)
	vw.store.delErr = errors.New("数据库只读")

	req := vw.preflight(t, ActionVolumeForget, VolumeParams{VolumeID: rec.ID, Version: "1"})
	if _, err := vw.execute(req); err == nil {
		t.Fatal("删除登记失败却返回成功")
	}
	if got := vw.fstabContent(t); got != before {
		t.Errorf("回滚之后 fstab 不是原样：\n%s", got)
	}
	if _, ok := vw.store.recs[rec.ID]; !ok {
		t.Error("回滚之后登记记录却不见了")
	}
}

// ---------- 中断与依赖 ----------

// TestVolumeExecuteInterrupted 钉住“上下文结束”的归类：
// 扫描因为取消而失败时，报的是“实际状态未知”，而不是普通命令失败。
func TestVolumeExecuteInterrupted(t *testing.T) {
	vw := newVolumeWriter(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := vw.w.executeVolume(ctx, WriteRequest{
		Action: ActionVolumeImport,
		Params: VolumeImportParams{DeviceID: "sdb1", Slug: "data"},
	})
	if err == nil {
		t.Fatal("上下文已结束却返回成功")
	}
	mustContain(t, err.Error(), "实际状态未知", "错误消息")
	mustContain(t, err.Error(), "/etc/fstab", "错误消息（要提示核实哪个文件）")
}

func TestVolumeExecuteRequiresDeps(t *testing.T) {
	vw := newVolumeWriter(t)
	vw.w.store = nil
	_, err := vw.w.executeVolume(context.Background(), WriteRequest{
		Action: ActionVolumeImport,
		Params: VolumeImportParams{DeviceID: "sdb1", Slug: "data"},
	})
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.Code != httpx.CodeDependencyMissing {
		t.Fatalf("错误码不对：%v", err)
	}
}

// ---------- systemd 单元名 ----------

func TestSystemdUnitFor(t *testing.T) {
	cases := map[string]string{
		"/srv/upanel/volumes/data":     "srv-upanel-volumes-data.mount",
		"/srv/upanel/volumes/backup-1": "srv-upanel-volumes-backup-1.mount",
		"/srv/upanel/volumes/a/b":      "srv-upanel-volumes-a-b.mount",
		"/srv/upanel/volumes/data/":    "srv-upanel-volumes-data.mount",
	}
	for in, want := range cases {
		if got := systemdUnitFor(in); got != want {
			t.Errorf("systemdUnitFor(%q) = %q，想要 %q", in, got, want)
		}
	}
}

// ---------- 辅助 ----------

// registeredVolume 把记录放进登记表并在 fstab 里写好它的条目（挂载点用临时根）。
func registeredVolume(t *testing.T, vw *volumeWriter, rec VolumeRecord) VolumeRecord {
	t.Helper()
	vw.store.recs[rec.ID] = rec
	if err := writeTestFile(vw.fstab, fstabWith(t, rec.FstabEntry()), 0o644); err != nil {
		t.Fatalf("%v", err)
	}
	return rec
}

// countArgs 数一条命令被调用时带了某个参数几次。
func countArgs(specs []syscmd.Spec, arg string) int {
	n := 0
	for _, s := range specs {
		for _, a := range s.Args {
			if a == arg {
				n++
				break
			}
		}
	}
	return n
}
