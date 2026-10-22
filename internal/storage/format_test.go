package storage

// 格式化的测试（P07.3）。
//
// 与 P07.2 的写测试同一套立场，但钉的东西换成了文件系统：
//
//  1. **写什么**：mkfs 的 argv 必须是代码写死的清单（ext4 只有 `-L`，xfs 固定带 `-f`），
//     标签是**一个** argv 项（空格不会被拆开），超限标签在执行前就被拒。
//  2. **写之前看什么**：身份指纹、状态版本、当前判决、目标分区在盘上与内核两侧的起点、
//     /dev 节点，任何一项对不上就**一条命令都不执行**。
//  3. **写之后怎么算成功**：超级块里的种类、UUID（必须换过）、标签（逐字节）、几何，
//     再加上签名分类器的第二源结论，全部对上才算 verified。**退出码 0 不算成功**——
//     “命令说成功、盘上没变”“标签被截断”这两类都必须判失败。
//
// 假 mkfs 会真的把超级块字节写进一个临时文件（就是“设备”），因此读回核验走的是
// 生产代码里那两条真实读取路径（DeviceSuperblockProber / DeviceSignatureProber），
// 而不是把同一个夹具读两遍自欺欺人。真实 Ubuntu 上的验证见
// docs/evidence/P07/2026-10-22-p07.3/ 与 tests/integration/p07-3-*.sh。

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"upanel/internal/syscmd"
)

// ---------- 夹具 ----------

const (
	// formatPartSize 是夹具分区的容量：1 GiB，够 ext4 与 xfs 都接受。
	formatPartSize = uint64(1) << 30
	// formatPartStart 是它的起点：1 MiB（与 P07.2 的对齐规则一致）。
	formatPartStart = uint64(1) << 20
	// fakeDeviceBytes 是假设备文件的大小：装得下超级块与签名窗口。
	fakeDeviceBytes = 64 << 10
)

// fakeMkfs 是一台会真的改“设备字节”的假 mkfs。
type fakeMkfs struct {
	devDir string

	mu       sync.Mutex
	commands []syscmd.Spec
	// failOn 是第几条命令返回失败（从 0 起；-1 表示不失败）。
	failOn int
	stderr string
	// noop 为 true 时“退出 0 但什么也没写”（验证核验不是看退出码）。
	noop bool
	// labelOverride 非 nil 时用它的值写盘（模拟截断/写错标签）。
	labelOverride *string
	// wrongKind 非空时写另一种文件系统（模拟 mkfs 做错）。
	wrongKind string
	// tiny 为 true 时只覆盖分区的 1/10（模拟几何不对）。
	tiny bool
	// reuseUUID 为 true 时写回原来的 UUID（模拟“根本没重建”）。
	reuseUUID bool
	// settleFail 为 true 时 udevadm settle 失败。
	settleFail bool

	seq int
}

func newFakeMkfs(devDir string) *fakeMkfs {
	return &fakeMkfs{devDir: devDir, failOn: -1}
}

func (r *fakeMkfs) Run(_ context.Context, spec syscmd.Spec, _ syscmd.Limits) (syscmd.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	idx := len(r.commands)
	r.commands = append(r.commands, spec)
	res := syscmd.Result{Program: spec.Program, ExitCode: 0}
	base := filepath.Base(spec.Program)
	if base == "udevadm" {
		if r.settleFail {
			res.ExitCode = 1
			res.Stderr = "udevadm settle: timed out"
			return res, &syscmd.ExitError{Program: spec.Program, ExitCode: 1}
		}
		return res, nil
	}
	if idx == r.failOn {
		res.ExitCode = 1
		res.Stderr = r.stderr
		return res, &syscmd.ExitError{Program: spec.Program, ExitCode: 1}
	}
	if r.noop {
		return res, nil
	}
	r.writeFilesystem(spec, base)
	return res, nil
}

// writeFilesystem 把一次 mkfs 的“结果”写进假设备文件（调用方持锁）。
func (r *fakeMkfs) writeFilesystem(spec syscmd.Spec, base string) {
	kind := FilesystemExt4
	if strings.Contains(base, "xfs") {
		kind = FilesystemXFS
	}
	if r.wrongKind != "" {
		kind = r.wrongKind
	}
	label := argAfter(spec.Args, "-L")
	if r.labelOverride != nil {
		label = *r.labelOverride
	}
	if len(spec.Args) == 0 {
		return
	}
	devPath := spec.Args[len(spec.Args)-1]
	size := formatPartSize
	if r.tiny {
		size = formatPartSize / 10
	}

	var uuid [16]byte
	if r.reuseUUID {
		if old, err := os.ReadFile(devPath); err == nil {
			if id, err := parseFilesystemIdentity(old); err == nil && id.UUID != "" {
				uuid = uuidFromString(id.UUID)
			}
		}
	}
	if uuid == ([16]byte{}) {
		r.seq++
		for i := range uuid {
			uuid[i] = byte(r.seq*16 + i + 1)
		}
	}

	var head []byte
	if kind == FilesystemXFS {
		head = xfsHead(uuid, label, size)
	} else {
		head = ext4Head(uuid, label, size)
	}
	_ = os.WriteFile(devPath, head, 0o644)
}

// argAfter 返回 -flag 后面的那个参数（没有就返回空串）。
func argAfter(args []string, flag string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}

// ext4Head 造一个 64 KiB 的假设备内容：ext4 超级块 + 指定标签与几何。
func ext4Head(uuid [16]byte, label string, fsBytes uint64) []byte {
	head := make([]byte, fakeDeviceBytes)
	binary.LittleEndian.PutUint16(head[ext4MagicOffset:], ext4Magic)
	binary.LittleEndian.PutUint32(head[ext4LogBlockSz:], 2) // 4 KiB 块
	binary.LittleEndian.PutUint32(head[ext4BlocksLo:], uint32(fsBytes/4096))
	// mkfs.ext4 生成的一定同时带 extents 与 64bit：两个位都置上才像真实产物。
	binary.LittleEndian.PutUint32(head[ext4Incompat:],
		ext4FeatureIncompatExtents|ext4FeatureIncompat64Bit)
	copy(head[ext4UUIDOffset:], uuid[:])
	copy(head[ext4LabelOffset:ext4LabelOffset+ext4LabelLen], label)
	return head
}

// xfsHead 造一个 64 KiB 的假设备内容：XFS 超级块（全大端）。
func xfsHead(uuid [16]byte, label string, fsBytes uint64) []byte {
	head := make([]byte, fakeDeviceBytes)
	copy(head[xfsMagicOffset:], xfsMagicString)
	binary.BigEndian.PutUint32(head[xfsBlockSizeOffset:], 4096)
	binary.BigEndian.PutUint64(head[xfsDBlocksOffset:], fsBytes/4096)
	copy(head[xfsUUIDOffset:], uuid[:])
	copy(head[xfsLabelOffset:xfsLabelOffset+xfsLabelLen], label)
	return head
}

// uuidFromString 把 8-4-4-4-12 解析回 16 字节（只用于 reuseUUID 夹具）。
func uuidFromString(s string) [16]byte {
	var out [16]byte
	clean := strings.ReplaceAll(s, "-", "")
	if len(clean) != 32 {
		return out
	}
	for i := 0; i < 16; i++ {
		v, err := strconv.ParseUint(clean[i*2:i*2+2], 16, 8)
		if err != nil {
			return [16]byte{}
		}
		out[i] = byte(v)
	}
	return out
}

// formatNodes 用“假设备文件是否存在”回答节点存在性。
type formatNodes struct{ dir string }

func (f formatNodes) Exists(name string) (bool, error) {
	if name == "" || name != filepath.Base(name) {
		return false, errors.New("storage: 设备名不合法")
	}
	fi, err := os.Stat(filepath.Join(f.dir, name))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return !fi.IsDir(), nil
}

// formatHarness 是一次格式化动作的全部替身。
type formatHarness struct {
	scanner *fakeWriteScanner
	runner  *fakeMkfs
	env     Env
	w       *writer
	devDir  string
	disk    *Device
	part    *Device
	steps   []string
}

// newFormatHarness 造一套“GPT 盘上有一个空白 1 GiB 分区”的格式化路径。
func newFormatHarness(t *testing.T) *formatHarness {
	t.Helper()
	devDir := t.TempDir()
	progs := t.TempDir()
	mkfsExt4 := filepath.Join(progs, "mkfs.ext4")
	mkfsXFS := filepath.Join(progs, "mkfs.xfs")
	sfdisk := filepath.Join(progs, "sfdisk")
	udevadm := filepath.Join(progs, "udevadm")
	// 真正的执行被假 runner 接管，这里只需要占位文件让 requireFiles 通过。
	for _, p := range []string{mkfsExt4, mkfsXFS, sfdisk, udevadm} {
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatalf("准备占位程序失败: %v", err)
		}
	}

	disk := gptDisk("sdb", 8<<30)
	part := addPartition(&disk, "sdb1", 1, formatPartStart, formatPartSize)
	setEntries(disk.PartitionTable, []GPTEntry{{
		Number: 1, StartBytes: formatPartStart, SizeBytes: formatPartSize,
		TypeGUID: gptTypeLinuxFilesystem, Name: "data",
	}})
	state := &fakeDiskState{
		diskID:  "sdb",
		entries: entriesOf(disk.PartitionTable),
		kernel:  append([]Partition(nil), disk.PartitionTable.Partitions...),
	}
	scanner := newFakeWriteScanner(guardTopo("v1", []Device{disk, part}), state)

	// 假设备：全零文件（= 空白分区），mkfs 会把超级块写进去。
	if err := os.WriteFile(filepath.Join(devDir, "sdb1"), make([]byte, fakeDeviceBytes), 0o644); err != nil {
		t.Fatalf("准备假设备失败: %v", err)
	}

	runner := newFakeMkfs(devDir)
	// 两条读取路径都指向假设备：签名分类器与超级块解析器读同一批真实字节，
	// 因此“两条路径一致/不一致”是代码行为，不是夹具自说自话。
	env := Env{
		Signatures: DeviceSignatureProber{Dev: devDir, Timeout: time.Second},
		Busy:       &fakeBusy{},
	}

	w := newWriter(WriteOptions{
		Scanner: scanner, Env: env, Runner: runner, Nodes: formatNodes{dir: devDir},
		Dev: devDir, Sfdisk: sfdisk, Udevadm: udevadm,
		MkfsExt4: mkfsExt4, MkfsXFS: mkfsXFS,
		Superblocks:   DeviceSuperblockProber{Dev: devDir, Timeout: time.Second},
		SettleTimeout: 50 * time.Millisecond, KernelWait: 60 * time.Millisecond,
	})
	w.poll = time.Millisecond

	return &formatHarness{
		scanner: scanner, runner: runner, env: env, w: w, devDir: devDir,
		disk: &disk, part: &part,
	}
}

// expect 用与生产相同的判决函数拿到“预检会记下什么”。
func (h *formatHarness) expect(t *testing.T, params FilesystemFormatParams) Assessment {
	t.Helper()
	topo, err := h.scanner.ScanNow(context.Background())
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	a, err := Assess(context.Background(), topo, ActionFilesystemFormat, params, h.env)
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	return a
}

// run 执行一次格式化。
func (h *formatHarness) run(ctx context.Context, params FilesystemFormatParams, a Assessment) (WriteReport, error) {
	h.steps = nil
	return h.w.execute(ctx, WriteRequest{
		Action:               ActionFilesystemFormat,
		Params:               params,
		ExpectedIdentity:     a.IdentityFingerprint,
		ExpectedStateVersion: a.StateVersion,
		Progress: func(step string, fields map[string]any) {
			h.steps = append(h.steps, step)
		},
	})
}

// mkfsCalls 只返回 mkfs 调用（udevadm settle 也在命令记录里）。
func (h *formatHarness) mkfsCalls() []syscmd.Spec {
	out := make([]syscmd.Spec, 0, len(h.runner.commands))
	for _, c := range h.runner.commands {
		if base := filepath.Base(c.Program); base != "udevadm" && base != "sfdisk" {
			out = append(out, c)
		}
	}
	return out
}

// oneFormat 跑一次“应当成功”的格式化：判决放行 → 执行 → 只允许一条 mkfs。
func oneFormat(t *testing.T, h *formatHarness, params FilesystemFormatParams) WriteReport {
	t.Helper()
	a := h.expect(t, params)
	if !a.Allowed {
		t.Fatalf("空白分区格式化应当放行：%+v", a.Blockers)
	}
	report, err := h.run(context.Background(), params, a)
	if err != nil {
		t.Fatalf("格式化失败: %v", err)
	}
	if calls := h.mkfsCalls(); len(calls) != 1 {
		t.Fatalf("执行了 %d 条 mkfs 命令：%+v", len(calls), calls)
	}
	return report
}

// ---------- 成功路径 ----------

func TestFormatExt4Verified(t *testing.T) {
	h := newFormatHarness(t)
	params := FilesystemFormatParams{DeviceID: "sdb1", Filesystem: FilesystemExt4, Label: "data"}
	report := oneFormat(t, h, params)

	// 命令形态：ext4 的固定参数集合里**没有** -F（实测不需要）。
	spec := h.mkfsCalls()[0]
	if want := "-L data <DEV>/sdb1"; argvOf(spec) != replaceDev(want, h.devDir) {
		t.Errorf("argv = %q，期望 %q", argvOf(spec), replaceDev(want, h.devDir))
	}
	if spec.Program != h.w.mkfsExt4 {
		t.Errorf("程序 = %q，期望 %q", spec.Program, h.w.mkfsExt4)
	}
	if spec.Stdin != nil {
		t.Errorf("mkfs 不该收到标准输入：%q", spec.Stdin)
	}

	// 读回核验：种类、标签、UUID、几何都在结果里，且是**读回来的**值。
	if got := report.Observed["filesystem"]; got != FilesystemExt4 {
		t.Errorf("observed filesystem = %v", got)
	}
	if got := report.Observed["label"]; got != "data" {
		t.Errorf("observed label = %v", got)
	}
	if got := report.Observed["size_bytes"]; got != strconv.FormatUint(formatPartSize, 10) {
		t.Errorf("observed size_bytes = %v", got)
	}
	uuid, _ := report.Observed["uuid"].(string)
	if uuid == "" {
		t.Errorf("observed uuid 为空：%+v", report.Observed)
	}
	// 两源都对上才算 verified：签名分类器的结论也要进结果。
	if got := report.Observed["signature"]; got != SignatureExt {
		t.Errorf("observed signature = %v，期望 %s", got, SignatureExt)
	}
	if got := report.Observed["node_present"]; got != true {
		t.Errorf("observed node_present = %v", got)
	}
	// data 里要有实际 argv 与前一份身份：审计要能回答“跑了什么、原来是什么”。
	argv, ok := report.Data["command"].([]string)
	if !ok || len(argv) != 4 || argv[0] != h.w.mkfsExt4 {
		t.Fatalf("data.command = %+v", report.Data["command"])
	}
	if got := report.Data["previous_kind"]; got != "" {
		t.Errorf("previous_kind = %v，空白分区应当为空串", got)
	}
	if got := report.Data["checked_via"]; strings.Join(toStrings(got), ",") != "superblock,signature_probe" {
		t.Errorf("checked_via = %v", got)
	}
	// 进度步骤能回答“卡在哪一步”。
	want := []string{WriteStepRecheck, WriteStepMkfs, WriteStepSettle, WriteStepVerify}
	if strings.Join(h.steps, ",") != strings.Join(want, ",") {
		t.Errorf("进度步骤 = %v，期望 %v", h.steps, want)
	}
}

func TestFormatXFSArgsAlwaysCarryForce(t *testing.T) {
	cases := []struct {
		label string
		want  string
	}{
		{"data", "-f -L data <DEV>/sdb1"},
		{"", "-f <DEV>/sdb1"},
	}
	for _, tc := range cases {
		h := newFormatHarness(t)
		params := FilesystemFormatParams{DeviceID: "sdb1", Filesystem: FilesystemXFS, Label: tc.label}
		report := oneFormat(t, h, params)
		if got := argvOf(h.mkfsCalls()[0]); got != replaceDev(tc.want, h.devDir) {
			t.Errorf("标签 %q 的 argv = %q，期望 %q", tc.label, got, replaceDev(tc.want, h.devDir))
		}
		if got := report.Observed["filesystem"]; got != FilesystemXFS {
			t.Errorf("observed filesystem = %v", got)
		}
		if got := report.Observed["signature"]; got != SignatureXFS {
			t.Errorf("observed signature = %v", got)
		}
	}
}

// TestFormatLabelWithSpaceIsOneArgv 校验标签是**一个** argv 项。
//
// 标签里有空格是合法的盘标（"data disk"），它必须作为一个参数交给 mkfs，
// 而不是被拆成两个（那会让 mkfs 把第二个词当设备路径）。不经 shell 是这条的前提。
func TestFormatLabelWithSpaceIsOneArgv(t *testing.T) {
	h := newFormatHarness(t)
	params := FilesystemFormatParams{DeviceID: "sdb1", Filesystem: FilesystemExt4, Label: "data disk"}
	report := oneFormat(t, h, params)
	spec := h.mkfsCalls()[0]
	if len(spec.Args) != 3 || spec.Args[0] != "-L" || spec.Args[1] != "data disk" {
		t.Fatalf("argv = %+v，期望 [-L \"data disk\" <设备>]", spec.Args)
	}
	if got := report.Observed["label"]; got != "data disk" {
		t.Errorf("observed label = %v", got)
	}
}

// ---------- 写之前的拒绝（一条命令都不执行） ----------

func TestFormatRejectsLabelOverLimit(t *testing.T) {
	// 17 字节：实测 mkfs.ext4 会打一行 Warning 就截断到 16 字节并 rc=0，
	// 因此必须在**进入命令之前**拒绝，否则“成功”的会是半截标签。
	h := newFormatHarness(t)
	params := FilesystemFormatParams{DeviceID: "sdb1", Filesystem: FilesystemExt4,
		Label: "0123456789abcdefg"}

	// 第一道：判决层就把它记成 LABEL_INVALID 并阻塞。
	a := h.expect(t, params)
	if a.Allowed {
		t.Fatalf("超限标签的判决应当阻塞：%+v", a.Blockers)
	}
	if len(a.Blockers) == 0 || a.Blockers[0].Code != NoticeLabelInvalid {
		t.Fatalf("阻塞项 = %+v，期望 %s", a.Blockers, NoticeLabelInvalid)
	}

	// 第二道：绕过判决直接执行，写原语自己也要拒，且一条命令都不执行。
	_, err := h.run(context.Background(), params, a)
	assertStatus(t, err, 409, "VALIDATION_ERROR")
	if !strings.Contains(err.Error(), NoticeLabelInvalid) {
		t.Errorf("拒绝理由不对：%v", err)
	}
	if len(h.runner.commands) != 0 {
		t.Fatalf("拒绝之后仍然执行了命令：%+v", h.runner.commands)
	}

	// 第三道：连命令本身都造不出来（标签是唯一进入 argv 的用户字符串）。
	_, err = h.w.formatCommand(preparedWrite{
		action: ActionFilesystemFormat, targetID: "sdb1",
		filesystem: FilesystemExt4, label: "0123456789abcdefg",
	})
	if !errors.Is(err, ErrUnsafeWrite) {
		t.Fatalf("formatCommand 错误 = %v，期望 ErrUnsafeWrite", err)
	}
}

func TestFormatRefusesMissingDeviceNode(t *testing.T) {
	h := newFormatHarness(t)
	// 签名探针换成固定结论：这样“节点不存在”才是唯一能拦住它的东西，
	// 而不是被更早的读盘失败抢先报成 SIGNATURE_UNREADABLE。
	h.w.env.Signatures = &fakeSignatures{kinds: map[string]string{"sdb1": ""}}
	h.env.Signatures = h.w.env.Signatures
	params := FilesystemFormatParams{DeviceID: "sdb1", Filesystem: FilesystemExt4}
	a := h.expect(t, params)
	if !a.Allowed {
		t.Fatalf("空白分区应当放行：%+v", a.Blockers)
	}

	if err := os.Remove(filepath.Join(h.devDir, "sdb1")); err != nil {
		t.Fatalf("移除假设备失败: %v", err)
	}
	_, err := h.run(context.Background(), params, a)
	assertStatus(t, err, 409, "PREFLIGHT_STALE")
	if !strings.Contains(err.Error(), "设备节点") {
		t.Errorf("拒绝理由没说清节点：%v", err)
	}
	if len(h.runner.commands) != 0 {
		t.Fatalf("拒绝之后仍然执行了命令：%+v", h.runner.commands)
	}
}

func TestFormatRefusesChangedIdentity(t *testing.T) {
	h := newFormatHarness(t)
	params := FilesystemFormatParams{DeviceID: "sdb1", Filesystem: FilesystemExt4}
	a := h.expect(t, params)
	a.IdentityFingerprint = "sha256:" + strings.Repeat("99", 32)

	_, err := h.run(context.Background(), params, a)
	assertStatus(t, err, 409, "DEVICE_IDENTITY_CHANGED")
	if len(h.runner.commands) != 0 {
		t.Fatalf("换盘之后仍然执行了命令：%+v", h.runner.commands)
	}
}

func TestFormatRefusesStaleStateVersion(t *testing.T) {
	h := newFormatHarness(t)
	params := FilesystemFormatParams{DeviceID: "sdb1", Filesystem: FilesystemExt4}
	a := h.expect(t, params)
	a.StateVersion = "sha256:" + strings.Repeat("88", 32)

	_, err := h.run(context.Background(), params, a)
	assertStatus(t, err, 409, "PREFLIGHT_STALE")
	if len(h.runner.commands) != 0 {
		t.Fatalf("状态变了之后仍然执行了命令：%+v", h.runner.commands)
	}
}

// TestFormatRefusesPartitionThatMoved 校验“编号相同但位置变了”也会被拒。
//
// 内核里 sdb1 从 1 MiB 开始，盘上的条目说它从 2 MiB 开始。这一条**专门**需要自己的
// 复核：拓扑版本号只覆盖节点/挂载这类快照信息，不覆盖 GPT 条目里的起始扇区，
// 所以版本号对得上也说明不了“sdb1 还是那段字节”。拿旧信息去格式化，格式化的
// 就是另一段字节。
func TestFormatRefusesPartitionThatMoved(t *testing.T) {
	h := newFormatHarness(t)
	params := FilesystemFormatParams{DeviceID: "sdb1", Filesystem: FilesystemExt4}
	a := h.expect(t, params)

	// 只改盘上的条目起点，内核视图不动：两个来源不一致。
	h.scanner.mu.Lock()
	state := h.scanner.disks["sdb"]
	state.entries[0].StartBytes = 2 * formatPartStart
	h.scanner.mu.Unlock()

	_, err := h.run(context.Background(), params, a)
	if err == nil {
		t.Fatal("分区位置变了却执行了格式化")
	}
	assertStatus(t, err, 409, "PREFLIGHT_STALE")
	if !strings.Contains(err.Error(), "不做任何写入") {
		t.Errorf("拒绝理由不清楚：%v", err)
	}
	if len(h.runner.commands) != 0 {
		t.Fatalf("拒绝之后仍然执行了命令：%+v", h.runner.commands)
	}
}

func TestFormatWithoutSuperblockProberIsDependencyMissing(t *testing.T) {
	h := newFormatHarness(t)
	h.w.superblocks = nil
	params := FilesystemFormatParams{DeviceID: "sdb1", Filesystem: FilesystemExt4}
	a := h.expect(t, params)
	_, err := h.run(context.Background(), params, a)
	assertStatus(t, err, 503, "DEPENDENCY_MISSING")
	if len(h.runner.commands) != 0 {
		t.Fatalf("没有读回能力却执行了命令：%+v", h.runner.commands)
	}
}

func TestFormatWithoutMkfsIsDependencyMissing(t *testing.T) {
	h := newFormatHarness(t)
	_ = os.Remove(h.w.mkfsXFS)
	params := FilesystemFormatParams{DeviceID: "sdb1", Filesystem: FilesystemXFS}
	a := h.expect(t, params)
	_, err := h.run(context.Background(), params, a)
	assertStatus(t, err, 503, "DEPENDENCY_MISSING")
	if len(h.runner.commands) != 0 {
		t.Fatalf("缺少 mkfs 却执行了命令：%+v", h.runner.commands)
	}
}

// ---------- 写之后的判负（退出码 0 也不算成功） ----------

func TestFormatFailsOnNonZeroExit(t *testing.T) {
	h := newFormatHarness(t)
	h.runner.failOn = 0
	// 实测原文：mkfs.xfs 在太小的设备上以 rc=1 拒绝，且不碰盘。
	h.runner.stderr = "mkfs.xfs: device is too small (1024 blocks)"
	params := FilesystemFormatParams{DeviceID: "sdb1", Filesystem: FilesystemXFS}
	a := h.expect(t, params)
	_, err := h.run(context.Background(), params, a)
	assertStatus(t, err, 500, "COMMAND_FAILED")
	if !strings.Contains(err.Error(), "退出码 1") {
		t.Errorf("失败消息没有退出码：%v", err)
	}
	if !strings.Contains(err.Error(), "too small") {
		t.Errorf("失败消息没有命令输出：%v", err)
	}
	// 关键：失败消息必须带上**盘上现在是什么**，否则管理员不知道要不要重来。
	if !strings.Contains(err.Error(), "盘上现在是") {
		t.Errorf("失败消息没有读回实际状态：%v", err)
	}
}

func TestFormatFailsWhenFileSystemWasNotRebuilt(t *testing.T) {
	h := newFormatHarness(t)
	// 先放一个“已经是 ext4、标签 data”的分区：请求一模一样的格式化，
	// 于是只有 UUID 变化能证明它真的被重建了。
	params := FilesystemFormatParams{DeviceID: "sdb1", Filesystem: FilesystemExt4, Label: "data"}
	oneFormat(t, h, params)

	// 再提交一次，但让假 mkfs “退出 0 却写回同一个 UUID”。
	h.runner.reuseUUID = true
	a := h.expect(t, params)
	_, err := h.run(context.Background(), params, a)
	assertStatus(t, err, 500, "COMMAND_FAILED")
	if !strings.Contains(err.Error(), "UUID 没有变化") {
		t.Errorf("失败理由不对：%v", err)
	}
}

func TestFormatFailsOnTruncatedLabel(t *testing.T) {
	h := newFormatHarness(t)
	// 请求 16 字节标签，而 mkfs 只写进去 15 字节（模拟截断/写错）。
	truncated := "0123456789abcde"
	h.runner.labelOverride = &truncated
	params := FilesystemFormatParams{DeviceID: "sdb1", Filesystem: FilesystemExt4,
		Label: "0123456789abcdef"}
	a := h.expect(t, params)
	_, err := h.run(context.Background(), params, a)
	assertStatus(t, err, 500, "COMMAND_FAILED")
	if !strings.Contains(err.Error(), "标签") {
		t.Errorf("失败理由没说标签：%v", err)
	}
}

func TestFormatFailsWhenWrongFileSystemLands(t *testing.T) {
	h := newFormatHarness(t)
	h.runner.wrongKind = FilesystemExt4 // 请求 xfs，结果盘上是 ext4
	params := FilesystemFormatParams{DeviceID: "sdb1", Filesystem: FilesystemXFS}
	a := h.expect(t, params)
	_, err := h.run(context.Background(), params, a)
	assertStatus(t, err, 500, "COMMAND_FAILED")
	if !strings.Contains(err.Error(), "请求的是 xfs") {
		t.Errorf("失败理由不对：%v", err)
	}
}

func TestFormatFailsOnTooSmallFilesystem(t *testing.T) {
	h := newFormatHarness(t)
	h.runner.tiny = true
	params := FilesystemFormatParams{DeviceID: "sdb1", Filesystem: FilesystemExt4}
	a := h.expect(t, params)
	_, err := h.run(context.Background(), params, a)
	assertStatus(t, err, 500, "COMMAND_FAILED")
	if !strings.Contains(err.Error(), "只覆盖") {
		t.Errorf("失败理由不对：%v", err)
	}
}

// TestFormatFailsWhenSignatureDisagrees 校验两条读路径不一致时判负。
//
// 超级块说 ext4、签名分类器说 btrfs（这里用固定结论的探针模拟）：两者不可能同时
// 正确，所以既不能报成功，也不能自己选一个相信。
func TestFormatFailsWhenSignatureDisagrees(t *testing.T) {
	h := newFormatHarness(t)
	params := FilesystemFormatParams{DeviceID: "sdb1", Filesystem: FilesystemExt4}
	a := h.expect(t, params)

	// 只替换签名探针：判决与写前读都还走真实文件。
	h.w.env.Signatures = &fakeSignatures{kinds: map[string]string{"sdb1": SignatureBtrfs}}
	h.env.Signatures = h.w.env.Signatures

	_, err := h.run(context.Background(), params, a)
	assertStatus(t, err, 500, "COMMAND_FAILED")
	if !strings.Contains(err.Error(), "两条读路径不一致") {
		t.Errorf("失败理由不对：%v", err)
	}
}

// TestFormatVerifiedWhenSettleFails 校验 settle 失败不单独判负。
//
// 判据是“设备字节对不对”，不是“udev 队列排空没有”：settle 失败照样读得回超级块，
// 于是结果里 verified 但 udev_settled=false，让界面能说清楚这一点。
func TestFormatVerifiedWhenSettleFails(t *testing.T) {
	h := newFormatHarness(t)
	h.runner.settleFail = true
	params := FilesystemFormatParams{DeviceID: "sdb1", Filesystem: FilesystemExt4}
	report := oneFormat(t, h, params)
	if got := report.Observed["udev_settled"]; got != false {
		t.Errorf("observed udev_settled = %v，期望 false", got)
	}
	if got := report.Observed["filesystem"]; got != FilesystemExt4 {
		t.Errorf("observed filesystem = %v", got)
	}
}

// TestFormatCancellationIsInterrupted 校验上下文结束时的错误能被
// errors.Is(err, context.Canceled) 认出——runner 据此记成 interrupted（实际状态未知），
// 而不是 failed（“什么都没发生”）或 succeeded（编造成绩）。
//
// 这里取消发生在**复核阶段**（还没进入 mkfs）：照样算中断，只不过措辞要说清
// “可能已经改了设备，也可能还没动手”。把这一种报成 SIGNATURE_UNREADABLE 之类的
// 业务拒绝，会让人以为盘上一定没动过。
func TestFormatCancellationIsInterrupted(t *testing.T) {
	h := newFormatHarness(t)
	params := FilesystemFormatParams{DeviceID: "sdb1", Filesystem: FilesystemExt4}
	a := h.expect(t, params)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := h.run(ctx, params, a)
	if err == nil {
		t.Fatal("上下文已结束却报成功")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("错误 = %v，应当能被 errors.Is(err, context.Canceled) 认出", err)
	}
	if !strings.Contains(err.Error(), "实际状态未知") {
		t.Errorf("中断消息没说清状态未知：%v", err)
	}
	if len(h.runner.commands) != 0 {
		t.Fatalf("上下文已结束却执行了命令：%+v", h.runner.commands)
	}
}

// TestFormatDeadlineStaysDeadline 校验超时透传成 DeadlineExceeded。
//
// runner 的顺序是“先 canceled 后 deadline”：这里要是被归一成 canceled，
// 任务会被记成 interrupted（要人工核实）而不是 timeout（超时，可重试）。
func TestFormatDeadlineStaysDeadline(t *testing.T) {
	h := newFormatHarness(t)
	params := FilesystemFormatParams{DeviceID: "sdb1", Filesystem: FilesystemExt4}
	a := h.expect(t, params)

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	_, err := h.run(ctx, params, a)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("错误 = %v，应当能被 errors.Is(err, context.DeadlineExceeded) 认出", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Errorf("超时不该被说成 canceled：%v", err)
	}
}

// TestFormatCommandFailureWithLiveContextIsFailed 校验普通的命令失败不被当成中断。
//
// 上下文活着、mkfs 以非 0 结束：这是“这条命令没跑完”，结果要按失败报并带上
// 读回的实际状态——不是“实际状态未知”。
func TestFormatCommandFailureWithLiveContextIsFailed(t *testing.T) {
	h := newFormatHarness(t)
	h.runner.failOn = 0
	h.runner.stderr = ""
	params := FilesystemFormatParams{DeviceID: "sdb1", Filesystem: FilesystemExt4}
	a := h.expect(t, params)
	_, err := h.run(context.Background(), params, a)
	if err == nil || errors.Is(err, context.Canceled) {
		t.Fatalf("错误 = %v，应当是一次普通的命令失败", err)
	}
	assertStatus(t, err, 500, "COMMAND_FAILED")
}

// ---------- 小工具 ----------

// argvOf 把一条命令的参数拼成一行（程序单独断言），并把分隔符归一化成 "/"。
func argvOf(spec syscmd.Spec) string {
	return strings.ReplaceAll(strings.Join(spec.Args, " "), "\\", "/")
}

// replaceDev 把期望里的 <DEV> 换成实际设备目录（Windows 上分隔符与盘符都不同）。
func replaceDev(want, devDir string) string {
	return strings.ReplaceAll(want, "<DEV>", strings.ReplaceAll(devDir, "\\", "/"))
}

// toStrings 把 any 收成字符串数组（只用于断言 checked_via）。
func toStrings(v any) []string {
	switch list := v.(type) {
	case []string:
		return list
	case []any:
		out := make([]string, 0, len(list))
		for _, item := range list {
			s, _ := item.(string)
			out = append(out, s)
		}
		return out
	}
	return nil
}
