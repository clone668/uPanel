package storage

// P07.4 的测试替身：命令、超级块、卷登记表、时钟。
//
// 卷管理横跨四样东西：fstab 文件、mount/umount/systemctl/findmnt 四条命令、
// 超级块里的 UUID、以及登记表本身。验收脚本会把这四样都拿到真机上跑一遍，
// 但单元测试必须能在没有 root、没有设备、没有 systemd 的机器上把
// **每一条拒绝路径与每一条回滚路径**都走一遍——那才是这一层的价值所在。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"upanel/internal/syscmd"
)

// fakeVolumeRunner 是按程序名分派的假执行器。
//
// handlers 的键是 filepath.Base(spec.Program)（"findmnt"、"mount"……），
// 值是这条命令的反应。没有登记的命默认成功——大多数测试关心的只是
// “某一步失败了会怎样”，而不是每一步都要写一行桩。
type fakeVolumeRunner struct {
	mu       sync.Mutex
	commands []syscmd.Spec
	handlers map[string]func(args []string) (syscmd.Result, error)
}

func newFakeVolumeRunner() *fakeVolumeRunner {
	return &fakeVolumeRunner{handlers: map[string]func([]string) (syscmd.Result, error){}}
}

// on 登记某条命令的反应。
func (r *fakeVolumeRunner) on(program string, h func(args []string) (syscmd.Result, error)) *fakeVolumeRunner {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handlers[program] = h
	return r
}

// always 让某条命令固定返回给定结果。
func (r *fakeVolumeRunner) always(program string, res syscmd.Result) *fakeVolumeRunner {
	return r.on(program, func([]string) (syscmd.Result, error) {
		return res, nil
	})
}

// fail 让某条命令固定失败。
func (r *fakeVolumeRunner) fail(program string, code int, stderr string) *fakeVolumeRunner {
	return r.on(program, func([]string) (syscmd.Result, error) {
		res := syscmd.Result{Program: program, ExitCode: code, Stderr: stderr}
		return res, &syscmd.ExitError{Program: program, ExitCode: code}
	})
}

func (r *fakeVolumeRunner) Run(_ context.Context, spec syscmd.Spec, _ syscmd.Limits) (syscmd.Result, error) {
	name := filepath.Base(spec.Program)
	r.mu.Lock()
	r.commands = append(r.commands, spec)
	h := r.handlers[name]
	r.mu.Unlock()
	if h == nil {
		return syscmd.Result{Program: spec.Program, ExitCode: 0}, nil
	}
	res, err := h(spec.Args)
	if res.Program == "" {
		res.Program = spec.Program
	}
	return res, err
}

// ran 报告某条命令被执行过几次（按基名匹配）。
func (r *fakeVolumeRunner) ran(program string) []syscmd.Spec {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []syscmd.Spec
	for _, c := range r.commands {
		if filepath.Base(c.Program) == program {
			out = append(out, c)
		}
	}
	return out
}

// programs 返回执行过的命令序列（基名），用于断言顺序。
func (r *fakeVolumeRunner) programs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.commands))
	for _, c := range r.commands {
		out = append(out, filepath.Base(c.Program))
	}
	return out
}

// ---------- 超级块 ----------

// fakeSuperblocks 是一张“哪台设备上是什么文件系统”的表。
type fakeSuperblocks struct {
	byDevice map[string]FilesystemIdentity
	errs     map[string]error
	calls    []string
}

func newFakeSuperblocks() *fakeSuperblocks {
	return &fakeSuperblocks{byDevice: map[string]FilesystemIdentity{}}
}

// put 登记一台设备的超级块。
func (f *fakeSuperblocks) put(deviceID, uuid, fstype string) *fakeSuperblocks {
	f.byDevice[deviceID] = FilesystemIdentity{Kind: fstype, UUID: uuid, Label: "UPANEL-" + deviceID}
	return f
}

func (f *fakeSuperblocks) Probe(_ context.Context, deviceID string, _ uint64) (FilesystemIdentity, error) {
	f.calls = append(f.calls, deviceID)
	if f.errs != nil {
		if err, ok := f.errs[deviceID]; ok {
			return FilesystemIdentity{}, err
		}
	}
	if id, ok := f.byDevice[deviceID]; ok {
		return id, nil
	}
	return FilesystemIdentity{}, errors.New("没有已知的文件系统")
}

// ---------- 卷登记表 ----------

// fakeVolumeStore 是内存登记表。
//
// 它刻意保留每条记录的副本（而不是指针），这样测试里“执行前的记录”不会被
// 被测代码就地改掉——那种耦合会让“回滚到旧版本”这类断言失去意义。
type fakeVolumeStore struct {
	recs    map[string]VolumeRecord
	err     error
	saveErr error
	delErr  error
	lists   int
}

func newFakeVolumeStore() *fakeVolumeStore {
	return &fakeVolumeStore{recs: map[string]VolumeRecord{}}
}

func (s *fakeVolumeStore) ListVolumes(_ context.Context) ([]VolumeRecord, error) {
	s.lists++
	if s.err != nil {
		return nil, s.err
	}
	out := make([]VolumeRecord, 0, len(s.recs))
	for _, r := range s.recs {
		out = append(out, r)
	}
	sortRecords(out)
	return out, nil
}

func (s *fakeVolumeStore) GetVolume(_ context.Context, id string) (VolumeRecord, bool, error) {
	if s.err != nil {
		return VolumeRecord{}, false, s.err
	}
	r, ok := s.recs[id]
	if !ok {
		return VolumeRecord{}, false, nil
	}
	return r, true, nil
}

func (s *fakeVolumeStore) SaveVolume(_ context.Context, rec VolumeRecord) error {
	if s.saveErr != nil {
		return s.saveErr
	}
	s.recs[rec.ID] = rec
	return nil
}

func (s *fakeVolumeStore) DeleteVolume(_ context.Context, id string) error {
	if s.delErr != nil {
		return s.delErr
	}
	delete(s.recs, id)
	return nil
}

// scannerOnly 是最小的拓扑扫描替身：卷动作的依赖检查只关心“有没有”扫描器。
type scannerOnly struct{}

func (scannerOnly) ScanNow(context.Context) (Topology, error) {
	return Topology{Status: StatusOK, Nodes: []Device{}, Edges: []Edge{}, NetworkMounts: []NetworkMount{}}, nil
}

// ---------- 共用夹具 ----------

// volumeFixture 是一套“临时目录里的 fstab + 假命令 + 假登记表”。
type volumeFixture struct {
	dir      string
	fstab    FstabStore
	runner   *fakeVolumeRunner
	store    *fakeVolumeStore
	super    *fakeSuperblocks
	now      time.Time
	existing string // 初始 fstab 内容
	// fsyncedDirs 记录目录 fsync 的调用（顺序证据）。
	fsyncedDirs []string
}

const testUUID = "5f1c9a3e-2b64-4c7d-8e10-9a3b5c7d1e2f"
const testUUID2 = "0a9e6b71-3c8d-4f52-b1a4-7d6e2c8f9b03"

func newVolumeFixture(t *testing.T, content string) *volumeFixture {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fstab")
	if err := writeTestFile(path, content, 0o644); err != nil {
		t.Fatalf("写测试 fstab 失败: %v", err)
	}
	runner := newFakeVolumeRunner()
	f := &volumeFixture{
		dir:      dir,
		runner:   runner,
		store:    newFakeVolumeStore(),
		super:    newFakeSuperblocks(),
		now:      time.Date(2026, 10, 22, 9, 30, 0, 0, time.UTC),
		existing: content,
	}
	f.fstab = FstabStore{
		Path:      path,
		BackupDir: filepath.Join(dir, "backups"),
		Runner:    runner,
		Findmnt:   "/usr/bin/findmnt",
		Now:       func() time.Time { return f.now },
		// 目录 fsync 是平台相关的：Linux 上对目录 open+fsync 是合法且必要的一步，
		// Windows 上会直接返回 Access is denied。生产环境只有 Linux，但单元测试
		// 也可能在开发宿主上跑，所以这里换成记录器——它同时让“rename 之后确实
		// fsync 了目录”这件事可断言（见 TestFstabWriteFsyncsDirectory）。
		fsyncDir: func(d string) error {
			f.fsyncedDirs = append(f.fsyncedDirs, d)
			return nil
		},
	}
	return f
}

// noDependents 是“问过了，确实没有面板对象依赖”的假查询。
//
// 生产里 nil 探针是阻塞的（证明不了没人用）；测试若想走“放行”这条路，
// 就必须显式写出“查过了，空”这个事实，而不是靠漏接线蒙过去。
func noDependents(context.Context, DependencyQuery) ([]VolumeDependency, error) {
	return nil, nil
}

// volumeEnv 组一份判决环境（未提供的探针留空，对应“依赖缺失”的路径）。
func (f *volumeFixture) volumeEnv() VolumeEnv {
	return VolumeEnv{
		Store:       f.store,
		Fstab:       f.fstab,
		Superblocks: f.super,
		Busy:        &fakeBusy{},
		Dependents:  noDependents,
		Root:        DefaultVolumeRoot,
		Now:         func() time.Time { return f.now },
	}
}

// record 造一条已登记的卷记录。
func testRecord(id, slug, deviceID, uuid string) VolumeRecord {
	return VolumeRecord{
		ID:                  id,
		DisplayName:         slug,
		Slug:                slug,
		DeviceID:            deviceID,
		FilesystemUUID:      uuid,
		FilesystemType:      FilesystemExt4,
		IdentityFingerprint: "sha256:disk-" + deviceID,
		Config:              DefaultVolumeConfig(),
		DesiredState:        DesiredUnmounted,
		Version:             "1",
	}
}

// volumeTopo 造一份含一块数据盘的拓扑：sdb 是整盘，sdb1 是它上面的分区，
// 分区带一个 ext4 文件系统与给定的 UUID。
func volumeTopo(version, uuid string) Topology {
	disk := blankDisk("sdb", 8<<30)
	part := addPartition(&disk, "sdb1", 1, 2*mib, 7<<30)
	part.Filesystem = &Filesystem{
		Type:   strPtr(FilesystemExt4),
		UUID:   strPtr(uuid),
		Mounts: []Mount{},
	}
	return guardTopo(version, []Device{disk, part})
}

// mountDeviceAt 把一台设备标成已挂载在某处。
func mountDeviceAt(dev *Device, mountPoint string) {
	if dev.Filesystem == nil {
		dev.Filesystem = &Filesystem{Mounts: []Mount{}}
	}
	dev.Filesystem.Mounts = append(dev.Filesystem.Mounts, Mount{
		Path: mountPoint, Source: "/dev/" + dev.ID, FSType: FilesystemExt4,
		Options: []string{"rw"},
	})
}

// writeTestFile 写一个测试文件并设权限。
func writeTestFile(path, content string, mode uint32) error {
	if err := os.WriteFile(path, []byte(content), os.FileMode(mode)); err != nil {
		return fmt.Errorf("写 %s 失败: %w", path, err)
	}
	return nil
}

// mustContain 断言字符串包含子串。
func mustContain(t *testing.T, got, want, what string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Fatalf("%s 里没有 %q：\n%s", what, want, got)
	}
}
