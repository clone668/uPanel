package storage

// 写原语的测试（P07.2）。
//
// 这一层真正把字节交给内核，因此测试必须同时钉住三件事：
//
//  1. **写什么**：命令必须是固定模板（`--wipe never`、`--append --wipe-partitions never`、
//     `--delete <盘> <编号>` + 一行脚本），用户输入只能出现在分区名里，而且名字已经被
//     限制成“放进引号里没有歧义”的字符集。
//  2. **写之前看什么**：身份指纹、状态版本、当前判决，任何一项对不上就**一条命令都不执行**。
//     这一条是用“命令记录必须为空”来断言的——只断言错误码会漏掉“先写了再报错”。
//  3. **写之后怎么算成功**：盘上的 GPT 与内核视图都必须与预期逐条一致。盘上对了内核没跟上
//     不算成功，命令返回 0 但盘上没变更不算成功。
//
// 测试用一台**会真的改状态的假扫描器**：写命令执行后它更新盘上条目与内核分区列表，
// 于是核验读回的是“写完之后的盘”，而不是把同一个夹具读两遍自欺欺人。
// 真实 Ubuntu 上的验证见 docs/evidence/P07/ 与 tests/integration/p07-2-*.sh。

import (
	"context"
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

// fakeDiskState 是一台假的“整盘 + 内核视图”。
type fakeDiskState struct {
	diskID string
	// 盘上的 GPT 条目（写命令会改这里）。
	entries []GPTEntry
	// 内核认可的分区（正常跟随 entries；lag 时故意不跟随）。
	kernel []Partition
}

// fakeWriteScanner 是 TopologyScanner 的测试替身。
//
// 它保存一份拓扑，并在每次 ScanNow 时把它深拷贝出去（PartitionTable 是指针，
// 浅拷贝会让写路径改到夹具本身）。
type fakeWriteScanner struct {
	mu    sync.Mutex
	topo  Topology
	disks map[string]*fakeDiskState
	// lag 为 true 时模拟“盘上写好了但内核没重读”。
	lag bool
	// err 非空时扫描失败。
	err  error
	scan int
}

func newFakeWriteScanner(topo Topology, disks ...*fakeDiskState) *fakeWriteScanner {
	s := &fakeWriteScanner{topo: topo, disks: map[string]*fakeDiskState{}}
	for _, d := range disks {
		s.disks[d.diskID] = d
	}
	s.sync()
	return s
}

// setEntries / entriesOf：DiskEntries 是 *[]GPTEntry（空数组与“没读到”必须在 JSON 里
// 区分开），测试里用这两个小助手，免得每处都写一遍取址与拷贝。
func setEntries(pt *PartitionTable, entries []GPTEntry) {
	cp := append([]GPTEntry{}, entries...)
	pt.DiskEntries = &cp
}

func entriesOf(pt *PartitionTable) []GPTEntry {
	if pt.DiskEntries == nil {
		return nil
	}
	return append([]GPTEntry(nil), (*pt.DiskEntries)...)
}

// sync 把 fakeDiskState 推进拓扑（调用方持锁）。
func (s *fakeWriteScanner) sync() {
	for id, st := range s.disks {
		dev := findDevice(s.topo, id)
		if dev == nil || dev.PartitionTable == nil {
			continue
		}
		setEntries(dev.PartitionTable, st.entries)
		if !s.lag {
			dev.PartitionTable.Partitions = append([]Partition(nil), st.kernel...)
		}
	}
}

func (s *fakeWriteScanner) ScanNow(context.Context) (Topology, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scan++
	if s.err != nil {
		return Topology{}, s.err
	}
	s.sync()
	out := s.topo
	out.Nodes = make([]Device, len(s.topo.Nodes))
	for i := range s.topo.Nodes {
		n := s.topo.Nodes[i]
		if n.PartitionTable != nil {
			pt := *n.PartitionTable
			pt.Partitions = append([]Partition(nil), n.PartitionTable.Partitions...)
			setEntries(&pt, entriesOf(n.PartitionTable))
			pt.FreeRegions = append([]FreeRegion(nil), n.PartitionTable.FreeRegions...)
			n.PartitionTable = &pt
		}
		out.Nodes[i] = n
	}
	out.Edges = append([]Edge(nil), s.topo.Edges...)
	return out, nil
}

// fakeCommandRunner 是一台假的 sfdisk/udevadm：它按命令改 fakeDiskState，
// 并把收到的每一条命令原样记下来。
type fakeCommandRunner struct {
	scanner *fakeWriteScanner

	mu       sync.Mutex
	commands []syscmd.Spec
	// failOn 是第几条命令返回失败（从 0 起；-1 表示不失败）。
	failOn int
	stderr string
	// noop 为 true 时“命令成功但盘上什么也没变”（用来验证核验不是看退出码）。
	noop bool
}

func newFakeRunner(scanner *fakeWriteScanner) *fakeCommandRunner {
	return &fakeCommandRunner{scanner: scanner, failOn: -1}
}

func (r *fakeCommandRunner) Run(_ context.Context, spec syscmd.Spec, _ syscmd.Limits) (syscmd.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	idx := len(r.commands)
	r.commands = append(r.commands, spec)
	res := syscmd.Result{Program: spec.Program, ExitCode: 0}
	if idx == r.failOn {
		res.ExitCode = 1
		res.Stderr = r.stderr
		return res, &syscmd.ExitError{Program: spec.Program, ExitCode: 1}
	}
	if filepath.Base(spec.Program) == "udevadm" {
		return res, nil // udevadm settle 不改状态
	}
	if r.noop {
		return res, nil
	}
	r.scanner.mu.Lock()
	defer r.scanner.mu.Unlock()
	r.apply(spec)
	return res, nil
}

// apply 把一条 sfdisk 命令的效果落到假盘上（调用方持 scanner 锁）。
func (r *fakeCommandRunner) apply(spec syscmd.Spec) {
	diskID := diskIDFromArgs(spec.Args, r.scanner.disks)
	st := r.scanner.disks[diskID]
	if st == nil {
		return
	}
	dev := findDevice(r.scanner.topo, diskID)
	if dev == nil || dev.PartitionTable == nil {
		return
	}
	switch {
	case len(spec.Args) > 0 && spec.Args[0] == "--wipe":
		// 初始化 GPT：清空条目，内核也跟着清空。
		dev.PartitionTable.Type = TableGPT
		dev.PartitionTable.DiskGUID = "0123ABCD-0000-4000-8000-00000000BEEF"
		st.entries = nil
		st.kernel = nil
	case len(spec.Args) > 0 && spec.Args[0] == "--append":
		start, size, name := parseCreateScript(string(spec.Stdin))
		number := 1
		for _, e := range st.entries {
			if e.Number >= number {
				number = e.Number + 1
			}
		}
		st.entries = append(st.entries, GPTEntry{
			Number: number, StartBytes: start, SizeBytes: size,
			TypeGUID: gptTypeLinuxFilesystem, Name: name,
		})
		st.kernel = append(st.kernel, Partition{
			DeviceID: diskID + strconv.Itoa(number), Number: number,
			StartBytes: start, SizeBytes: size,
		})
	case len(spec.Args) > 0 && spec.Args[0] == "--delete":
		n, _ := strconv.Atoi(spec.Args[len(spec.Args)-1])
		kept := make([]GPTEntry, 0, len(st.entries))
		for _, e := range st.entries {
			if e.Number != n {
				kept = append(kept, e)
			}
		}
		st.entries = kept
		keptParts := make([]Partition, 0, len(st.kernel))
		for _, p := range st.kernel {
			if p.Number != n {
				keptParts = append(keptParts, p)
			}
		}
		st.kernel = keptParts
	}
}

// diskIDFromArgs 从命令参数里认出被操作的整盘（测试里 /dev 是一个普通目录，
// Windows 上分隔符还会变成反斜杠，所以先归一化再按后缀匹配）。
func diskIDFromArgs(args []string, disks map[string]*fakeDiskState) string {
	for _, a := range args {
		norm := strings.ReplaceAll(a, "\\", "/")
		for id := range disks {
			if strings.HasSuffix(norm, "/"+id) {
				return id
			}
		}
	}
	return ""
}

// parseCreateScript 按**固定格式**解析建分区脚本；格式不对就直接断言失败，
// 因为脚本形态本身就是产品的一部分（不能悄悄变成别的写法）。
func parseCreateScript(script string) (start, size uint64, name string) {
	body := strings.TrimSuffix(script, "\n")
	parts := strings.Split(body, ", ")
	if len(parts) < 3 {
		panic("建分区脚本格式不对：" + script)
	}
	for _, p := range parts {
		switch {
		case strings.HasPrefix(p, "start="):
			start, _ = strconv.ParseUint(strings.TrimPrefix(p, "start="), 10, 64)
		case strings.HasPrefix(p, "size="):
			size, _ = strconv.ParseUint(strings.TrimPrefix(p, "size="), 10, 64)
		case strings.HasPrefix(p, "name="):
			name = strings.Trim(strings.TrimPrefix(p, "name="), "\"")
		}
	}
	return start * sectorSize, size * sectorSize, name
}

// fakeNodes 判断 /dev 节点是否存在：跟随假盘的内核视图（内核认了才有节点），
// block 里的名字永远不出现，用来模拟 udev 落后。
type fakeNodes struct {
	scanner *fakeWriteScanner
	block   map[string]bool
}

func (f fakeNodes) Exists(name string) (bool, error) {
	if f.block[name] {
		return false, nil
	}
	f.scanner.mu.Lock()
	defer f.scanner.mu.Unlock()
	for _, st := range f.scanner.disks {
		for _, p := range st.kernel {
			if p.DeviceID == name {
				return true, nil
			}
		}
	}
	return false, nil
}

// writeHarness 是一次写动作的全部替身。
type writeHarness struct {
	scanner *fakeWriteScanner
	runner  *fakeCommandRunner
	nodes   fakeNodes
	env     Env
	w       *writer
	sfdisk  string
	udevadm string
	steps   []string
}

// newWriteHarness 造一套“盘是空的、命令记录在案”的写路径。
func newWriteHarness(t *testing.T, topo Topology, disks ...*fakeDiskState) *writeHarness {
	t.Helper()
	dir := t.TempDir()
	// 写路径会先检查固定程序是否存在（生产里是 /usr/sbin/sfdisk、/usr/bin/udevadm）。
	// 测试给两个占位文件：真正的执行被假执行器接管，不会被调用。
	sfdisk := filepath.Join(dir, "sfdisk")
	udevadm := filepath.Join(dir, "udevadm")
	for _, p := range []string{sfdisk, udevadm} {
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatalf("准备占位程序失败: %v", err)
		}
	}

	scanner := newFakeWriteScanner(topo, disks...)
	runner := newFakeRunner(scanner)
	env, _, _ := testEnv()

	h := &writeHarness{
		scanner: scanner, runner: runner, env: env,
		nodes:  fakeNodes{scanner: scanner},
		sfdisk: sfdisk, udevadm: udevadm,
	}
	h.w = newWriter(WriteOptions{
		Scanner: scanner, Env: env, Runner: runner, Nodes: h.nodes,
		Dev: "/dev", Sfdisk: sfdisk, Udevadm: udevadm,
		SettleTimeout: 50 * time.Millisecond, KernelWait: 60 * time.Millisecond,
	})
	// 轮询间隔压到 1ms：真实值是 300ms，测试不该等它。
	h.w.poll = time.Millisecond
	return h
}

// expect 用与生产相同的判决函数拿到“预检会记下什么”。
func (h *writeHarness) expect(t *testing.T, action string, params any) Assessment {
	t.Helper()
	topo, err := h.scanner.ScanNow(context.Background())
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	a, err := Assess(context.Background(), topo, action, params, h.env)
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	return a
}

// run 执行一次写动作，并把进度步骤记下来。
func (h *writeHarness) run(ctx context.Context, action string, params any, a Assessment) (WriteReport, error) {
	return h.w.execute(ctx, WriteRequest{
		Action:               action,
		Params:               params,
		ExpectedIdentity:     a.IdentityFingerprint,
		ExpectedStateVersion: a.StateVersion,
		Progress: func(step string, fields map[string]any) {
			h.steps = append(h.steps, step)
		},
	})
}

// sfdiskCalls 只返回 sfdisk 调用（udevadm settle 也在命令记录里）。
func (h *writeHarness) sfdiskCalls() []syscmd.Spec {
	out := make([]syscmd.Spec, 0, len(h.runner.commands))
	for _, c := range h.runner.commands {
		if filepath.Base(c.Program) == "sfdisk" {
			out = append(out, c)
		}
	}
	return out
}

// argsOf 把命令参数拼成一行，并把分隔符归一化成 "/"：
// 生产目标是 Linux（/dev/sdb），测试机可能是 Windows（\dev\sdb）。
func argsOf(spec syscmd.Spec) string {
	return strings.ReplaceAll(strings.Join(spec.Args, " "), "\\", "/")
}

// assertStatus 断言错误是带状态码的拒绝。
func assertStatus(t *testing.T, err error, want int, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("期望被拒绝（%d %s），实际成功了", want, code)
	}
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("错误不是 HTTPError: %v", err)
	}
	if httpErr.Status != want || string(httpErr.Code) != code {
		t.Fatalf("拒绝 = %d %s（%s），期望 %d %s", httpErr.Status, httpErr.Code, httpErr.Message, want, code)
	}
}

// ---------- 初始化 GPT ----------

func TestWriteInitializeGPTOnBlankDisk(t *testing.T) {
	disk := blankDisk("sdb", 8<<30)
	h := newWriteHarness(t, guardTopo("v1", []Device{disk}), &fakeDiskState{diskID: "sdb"})
	params := PartitionTableInitializeParams{DeviceID: "sdb", TableType: TableGPT}
	a := h.expect(t, ActionPartitionTableInitialize, params)
	if !a.Allowed {
		t.Fatalf("空白盘初始化应当放行：%+v", a.Blockers)
	}

	report, err := h.run(context.Background(), ActionPartitionTableInitialize, params, a)
	if err != nil {
		t.Fatalf("写动作失败: %v", err)
	}

	// 命令形态：唯一一次 sfdisk 调用，固定模板，标准输入只有一行 label。
	if calls := h.sfdiskCalls(); len(calls) != 1 {
		t.Fatalf("执行了 %d 条 sfdisk 命令：%+v", len(calls), calls)
	}
	spec := h.sfdiskCalls()[0]
	if spec.Program != h.sfdisk {
		t.Errorf("程序 = %q", spec.Program)
	}
	if got := argsOf(spec); got != "--wipe never /dev/sdb" {
		t.Errorf("参数 = %q，期望 --wipe never /dev/sdb", got)
	}
	if string(spec.Stdin) != "label: gpt\n" {
		t.Errorf("脚本 = %q", spec.Stdin)
	}

	// 读回核验：报告里是核验过的状态。
	if got := report.Observed["table_type"]; got != TableGPT {
		t.Errorf("observed table_type = %v", got)
	}
	if got := report.Data["partitions"]; got != 0 {
		t.Errorf("分区数 = %v，期望 0", got)
	}
	// 进度步骤必须能解释“卡在哪一步”。
	want := []string{WriteStepRecheck, WriteStepCommand, WriteStepSettle, WriteStepVerify}
	if strings.Join(h.steps, ",") != strings.Join(want, ",") {
		t.Errorf("进度步骤 = %v，期望 %v", h.steps, want)
	}
}

func TestWriteInitializeRefusesDiskThatAlreadyHasTable(t *testing.T) {
	disk := gptDisk("sdb", 8<<30)
	h := newWriteHarness(t, guardTopo("v1", []Device{disk}), &fakeDiskState{diskID: "sdb"})
	params := PartitionTableInitializeParams{DeviceID: "sdb", TableType: TableGPT}
	// 判决层拒绝它（PARTITION_TABLE_PRESENT）；这里把判决改成“放行”，验证写原语自己也会拒。
	a := h.expect(t, ActionPartitionTableInitialize, params)
	a.Allowed = true
	a.Blockers = nil

	_, err := h.run(context.Background(), ActionPartitionTableInitialize, params, a)
	// 身份与版本都没变，但**重新判决**发现当前状态不允许：拒绝要带上具体阻塞码。
	assertStatus(t, err, 409, "VALIDATION_ERROR")
	if !strings.Contains(err.Error(), NoticeTablePresent) {
		t.Errorf("拒绝理由里没有 %s：%v", NoticeTablePresent, err)
	}
	if len(h.sfdiskCalls()) != 0 {
		t.Fatalf("拒绝之后仍然执行了命令：%+v", h.sfdiskCalls())
	}
}

// ---------- 建分区 ----------

func TestWriteCreatePartition(t *testing.T) {
	disk := gptDisk("sdb", 8<<30)
	h := newWriteHarness(t, guardTopo("v1", []Device{disk}), &fakeDiskState{diskID: "sdb"})
	params := PartitionCreateParams{DeviceID: "sdb", StartBytes: mib, SizeBytes: 4 * mib, Name: "data"}
	a := h.expect(t, ActionPartitionCreate, params)
	if !a.Allowed {
		t.Fatalf("空闲区建分区应当放行：%+v", a.Blockers)
	}

	report, err := h.run(context.Background(), ActionPartitionCreate, params, a)
	if err != nil {
		t.Fatalf("写动作失败: %v", err)
	}
	if calls := h.sfdiskCalls(); len(calls) != 1 {
		t.Fatalf("执行了 %d 条 sfdisk 命令", len(calls))
	}
	spec := h.sfdiskCalls()[0]
	if got := argsOf(spec); got != "--append --wipe-partitions never /dev/sdb" {
		t.Errorf("参数 = %q，期望 --append --wipe-partitions never /dev/sdb", got)
	}
	// 起点 1 MiB = 2048 扇区，大小 4 MiB = 8192 扇区：模板里的数字是**扇区**，不是字节。
	if want := "start=2048, size=8192, type=linux, name=\"data\"\n"; string(spec.Stdin) != want {
		t.Errorf("脚本 = %q，期望 %q", spec.Stdin, want)
	}
	created, ok := report.Data["created_partition"].(map[string]any)
	if !ok {
		t.Fatalf("报告里没有 created_partition：%+v", report.Data)
	}
	if created["device_id"] != "sdb1" || created["number"] != 1 {
		t.Errorf("新建分区 = %+v", created)
	}
	if created["start_bytes"] != strconv.FormatUint(mib, 10) {
		t.Errorf("start_bytes = %v", created["start_bytes"])
	}
}

func TestWriteCreatePartitionWithoutName(t *testing.T) {
	disk := gptDisk("sdb", 8<<30)
	h := newWriteHarness(t, guardTopo("v1", []Device{disk}), &fakeDiskState{diskID: "sdb"})
	params := PartitionCreateParams{DeviceID: "sdb", StartBytes: mib, SizeBytes: 4 * mib}
	a := h.expect(t, ActionPartitionCreate, params)
	if _, err := h.run(context.Background(), ActionPartitionCreate, params, a); err != nil {
		t.Fatalf("写动作失败: %v", err)
	}
	// 没有名字时不写 name= 字段：空名字不该变成 name=""。
	if want := "start=2048, size=8192, type=linux\n"; string(h.runner.commands[0].Stdin) != want {
		t.Errorf("脚本 = %q，期望 %q", h.runner.commands[0].Stdin, want)
	}
}

// ---------- 删分区 ----------

func TestWriteDeletePartition(t *testing.T) {
	disk := gptDisk("sdb", 8<<30)
	part := addPartition(&disk, "sdb1", 1, mib, 4*mib)
	setEntries(disk.PartitionTable, []GPTEntry{{
		Number: 1, StartBytes: mib, SizeBytes: 4 * mib,
		TypeGUID: gptTypeLinuxFilesystem, Name: "data",
	}})
	state := &fakeDiskState{
		diskID:  "sdb",
		entries: entriesOf(disk.PartitionTable),
		kernel:  disk.PartitionTable.Partitions,
	}
	h := newWriteHarness(t, guardTopo("v1", []Device{disk, part}, Edge{
		FromDeviceID: "sdb1", ToDeviceID: "sdb", Relation: RelationPartitionOf,
	}), state)

	params := PartitionDeleteParams{DeviceID: "sdb1"}
	a := h.expect(t, ActionPartitionDelete, params)
	if !a.Allowed {
		t.Fatalf("删空闲分区应当放行：%+v", a.Blockers)
	}

	if _, err := h.run(context.Background(), ActionPartitionDelete, params, a); err != nil {
		t.Fatalf("写动作失败: %v", err)
	}
	if calls := h.sfdiskCalls(); len(calls) != 1 {
		t.Fatalf("执行了 %d 条 sfdisk 命令", len(calls))
	}
	// 编号来自盘上的 GPT 条目（不来自参数），且按槽位号删——不会重排其余分区。
	if want := "--delete /dev/sdb 1"; argsOf(h.sfdiskCalls()[0]) != want {
		t.Errorf("参数 = %q，期望 %q", argsOf(h.sfdiskCalls()[0]), want)
	}
	if len(state.entries) != 0 {
		t.Errorf("盘上还剩 %d 条条目", len(state.entries))
	}
}

// ---------- 执行前的复核 ----------

func TestWriteRefusesWhenIdentityChanged(t *testing.T) {
	disk := blankDisk("sdb", 8<<30)
	h := newWriteHarness(t, guardTopo("v1", []Device{disk}), &fakeDiskState{diskID: "sdb"})
	params := PartitionTableInitializeParams{DeviceID: "sdb", TableType: TableGPT}
	a := h.expect(t, ActionPartitionTableInitialize, params)

	_, err := h.w.execute(context.Background(), WriteRequest{
		Action:               ActionPartitionTableInitialize,
		Params:               params,
		ExpectedIdentity:     "sha256:另一块盘",
		ExpectedStateVersion: a.StateVersion,
	})
	assertStatus(t, err, 409, "DEVICE_IDENTITY_CHANGED")
	if len(h.runner.commands) != 0 {
		t.Fatalf("身份不符却执行了命令：%+v", h.runner.commands)
	}
}

func TestWriteRefusesWhenStateVersionChanged(t *testing.T) {
	disk := blankDisk("sdb", 8<<30)
	h := newWriteHarness(t, guardTopo("v2", []Device{disk}), &fakeDiskState{diskID: "sdb"})
	params := PartitionTableInitializeParams{DeviceID: "sdb", TableType: TableGPT}
	a := h.expect(t, ActionPartitionTableInitialize, params)

	_, err := h.w.execute(context.Background(), WriteRequest{
		Action:               ActionPartitionTableInitialize,
		Params:               params,
		ExpectedIdentity:     a.IdentityFingerprint,
		ExpectedStateVersion: "sha256:旧版本",
	})
	assertStatus(t, err, 409, "PREFLIGHT_STALE")
	if len(h.runner.commands) != 0 {
		t.Fatalf("版本不符却执行了命令：%+v", h.runner.commands)
	}
}

func TestWriteRefusesWhenStateNoLongerAllows(t *testing.T) {
	// 盘在预检之后被按了只读开关：判决必须带着具体阻塞码拒绝，且不写。
	disk := blankDisk("sdb", 8<<30)
	h := newWriteHarness(t, guardTopo("v1", []Device{disk}), &fakeDiskState{diskID: "sdb"})
	params := PartitionTableInitializeParams{DeviceID: "sdb", TableType: TableGPT}
	a := h.expect(t, ActionPartitionTableInitialize, params)

	h.scanner.mu.Lock()
	dev := findDevice(h.scanner.topo, "sdb")
	dev.ReadOnly = true
	h.scanner.mu.Unlock()

	_, err := h.run(context.Background(), ActionPartitionTableInitialize, params, a)
	assertStatus(t, err, 409, "VALIDATION_ERROR")
	if !strings.Contains(err.Error(), NoticeReadOnly) {
		t.Errorf("拒绝理由里没有 %s：%v", NoticeReadOnly, err)
	}
	if len(h.runner.commands) != 0 {
		t.Fatalf("被拒绝却执行了命令：%+v", h.runner.commands)
	}
}

func TestWriteRefusesNonWritableSectorSize(t *testing.T) {
	disk := blankDisk("sdb", 8<<30)
	disk.LogicalSectorBytes = 4096
	h := newWriteHarness(t, guardTopo("v1", []Device{disk}), &fakeDiskState{diskID: "sdb"})
	params := PartitionTableInitializeParams{DeviceID: "sdb", TableType: TableGPT}
	a := h.expect(t, ActionPartitionTableInitialize, params)

	_, err := h.run(context.Background(), ActionPartitionTableInitialize, params, a)
	assertStatus(t, err, 422, "UNSUPPORTED_DEVICE")
	if len(h.runner.commands) != 0 {
		t.Fatalf("4Kn 盘却执行了命令：%+v", h.runner.commands)
	}
}

func TestWriteWithoutRunnerIsDependencyMissing(t *testing.T) {
	w := newWriter(WriteOptions{})
	_, err := w.execute(context.Background(), WriteRequest{Action: ActionPartitionTableInitialize})
	assertStatus(t, err, 503, "DEPENDENCY_MISSING")
}

// ---------- 写后核验 ----------

func TestWriteFailsWhenCommandReportsError(t *testing.T) {
	disk := blankDisk("sdb", 8<<30)
	h := newWriteHarness(t, guardTopo("v1", []Device{disk}), &fakeDiskState{diskID: "sdb"})
	h.runner.failOn = 0
	h.runner.stderr = "sfdisk: /dev/sdb: cannot open"
	params := PartitionTableInitializeParams{DeviceID: "sdb", TableType: TableGPT}
	a := h.expect(t, ActionPartitionTableInitialize, params)

	_, err := h.run(context.Background(), ActionPartitionTableInitialize, params, a)
	assertStatus(t, err, 500, "COMMAND_FAILED")
	msg := err.Error()
	if !strings.Contains(msg, "cannot open") {
		t.Errorf("错误里没有 sfdisk 的输出：%v", msg)
	}
	// 失败消息必须带上读回的实际状态，管理员才知道现在盘上是什么样。
	if !strings.Contains(msg, "盘上现在") {
		t.Errorf("错误里没有读回的实际状态：%v", msg)
	}
}

func TestWriteFailsWhenDiskDidNotChange(t *testing.T) {
	// 命令返回 0，但盘上没有变化：不能报成功。
	disk := blankDisk("sdb", 8<<30)
	h := newWriteHarness(t, guardTopo("v1", []Device{disk}), &fakeDiskState{diskID: "sdb"})
	h.runner.noop = true
	params := PartitionTableInitializeParams{DeviceID: "sdb", TableType: TableGPT}
	a := h.expect(t, ActionPartitionTableInitialize, params)

	_, err := h.run(context.Background(), ActionPartitionTableInitialize, params, a)
	assertStatus(t, err, 500, "COMMAND_FAILED")
	// 命令返回 0 但盘上没变：核验读回的是“还是没有分区表”，所以报的是类型不符。
	if !strings.Contains(err.Error(), "期望 gpt") {
		t.Errorf("错误没有说明盘上的表不符合预期：%v", err)
	}
}

func TestWriteFailsWhenKernelDoesNotFollow(t *testing.T) {
	// 盘上写对了，但内核没重读：分区在 /dev 里就不会出现，这时报成功是骗人。
	disk := gptDisk("sdb", 8<<30)
	h := newWriteHarness(t, guardTopo("v1", []Device{disk}), &fakeDiskState{diskID: "sdb"})
	h.scanner.lag = true
	params := PartitionCreateParams{DeviceID: "sdb", StartBytes: mib, SizeBytes: 4 * mib, Name: "data"}
	a := h.expect(t, ActionPartitionCreate, params)

	_, err := h.run(context.Background(), ActionPartitionCreate, params, a)
	assertStatus(t, err, 500, "COMMAND_FAILED")
	if !strings.Contains(err.Error(), "内核") {
		t.Errorf("错误没有指出内核视图没跟上：%v", err)
	}
	// 盘上的条目确实写进去了（这一步只报“内核没跟上”，不是“没写”）。
	if got := len(h.scanner.disks["sdb"].entries); got != 1 {
		t.Errorf("盘上有 %d 条条目，期望 1", got)
	}
}

func TestWriteFailsWhenNewPartitionHasNoNode(t *testing.T) {
	// 内核认了分区但 /dev 节点还没出现：P07.3 的格式化会因此失败，所以这里不算成功。
	disk := gptDisk("sdb", 8<<30)
	h := newWriteHarness(t, guardTopo("v1", []Device{disk}), &fakeDiskState{diskID: "sdb"})
	h.w.nodes = fakeNodes{scanner: h.scanner, block: map[string]bool{"sdb1": true}}
	params := PartitionCreateParams{DeviceID: "sdb", StartBytes: mib, SizeBytes: 4 * mib}
	a := h.expect(t, ActionPartitionCreate, params)

	_, err := h.run(context.Background(), ActionPartitionCreate, params, a)
	assertStatus(t, err, 500, "COMMAND_FAILED")
	if !strings.Contains(err.Error(), "还没有出现") {
		t.Errorf("错误没有指出节点缺失：%v", err)
	}
}

// ---------- 模板与参数形状 ----------

func TestPartitionNameRejectsQuoteAndBackslash(t *testing.T) {
	// 实测 sfdisk 2.39.3 无法在脚本里表达带双引号的名字（line 1: unsupported command），
	// 反斜杠会走它自己的转义。两个都在判决层拒掉，而不是赌转义语义。
	for _, name := range []string{`a"b`, `a\b`, `a\"b`} {
		if err := ValidatePartitionName(name); err == nil {
			t.Errorf("分区名 %q 应被拒绝", name)
		}
	}
	// 实测这些字符在 sfdisk 的引号里原样往返，因此必须仍然被接受（只有引号与反斜杠不行）。
	for _, name := range []string{"", "data", "data-1", "a b", "a,b", "a#b", "a=b", "$(id)", "`id`", "a;b", "%*!@^", ": / . _ -"} {
		if err := ValidatePartitionName(name); err != nil {
			t.Errorf("分区名 %q 被误拒: %v", name, err)
		}
	}
}

func TestWritePrimitiveRefusesUnsafeParams(t *testing.T) {
	h := newWriteHarness(t, Topology{})
	disk := blankDisk("sdb", 8<<30)

	// 未对齐：写原语自己也要断言，不能只靠判决层。
	if _, err := h.w.command(preparedWrite{
		action: ActionPartitionCreate, startBytes: mib + 512, sizeBytes: 4 * mib,
	}, "sdb"); !errors.Is(err, ErrUnsafeWrite) {
		t.Errorf("未对齐的区间被接受: %v", err)
	}
	// 编号 0 不合法（删除必须按真实槽位号）。
	if _, err := h.w.command(preparedWrite{action: ActionPartitionDelete, deleteNumber: 0}, "sdb"); !errors.Is(err, ErrUnsafeWrite) {
		t.Errorf("编号 0 被接受: %v", err)
	}
	// 格式化还没有写原语：必须明确说不支持，而不是返回一条空命令。
	if _, err := h.w.command(preparedWrite{action: ActionFilesystemFormat}, "sdb1"); !errors.Is(err, ErrNoWritePrimitive) {
		t.Errorf("格式化应当报 ErrNoWritePrimitive: %v", err)
	}
	// 越界：期望状态的构建也会拒。
	if _, err := expectedState(preparedWrite{
		action: ActionPartitionCreate, startBytes: 8 << 30, sizeBytes: 4 * mib,
	}, &disk, tableState{Type: TableGPT}); !errors.Is(err, ErrUnsafeWrite) {
		t.Errorf("越过盘尾的区间被接受: %v", err)
	}
}
