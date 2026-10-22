package storage

// P07.2：破坏性写原语——初始化 GPT、在空闲区里建分区、删除分区。
//
// 这一层只做三件事，三件都必须成立才算成功：
//
//  1. **写之前重新看一眼**。哪怕代理已经复核过一次，真正动手前也要重新扫描拓扑并重新
//     判决：身份指纹与预检不一致是“换盘了”（DEVICE_IDENTITY_CHANGED），状态版本不一致
//     是“还是那块盘但状态变了”（PREFLIGHT_STALE），当前状态不允许就带着阻塞码拒绝。
//     一律不写。
//  2. **写的时候只用固定模板**。sfdisk 的脚本由代码里的模板加**已校验的数字**拼成，
//     用户字符串只有分区名，而它在拼进去之前要过字符集检查：`label: gpt`、
//     `start=<扇区>, size=<扇区>, type=linux`、`--delete <盘> <编号>`。不加 `--force`
//     （那会绕过占用检查），不加 `--lock`（实测 2.39.3 上会误报 already locked，
//     互斥由代理的资源锁与占用扫描负责）。
//  3. **写之后读回来对**。等 udev 稳定，再从**两个来源**读回：盘上的 GPT 条目表（字节）
//     与内核认可的分区列表（sysfs）。两边都与预期一致、且 /dev 节点状态正确，才算
//     verified。只对一边，就可能把“盘上写好了但内核没重读”说成成功。
//
// 为什么不用 sgdisk/parted：PLAN.md 第 9.2 节指定 sfdisk 的固定模板输入。实测 util-linux
// 2.39.3 的 sfdisk 只从标准输入读脚本（用法里没有脚本文件参数），因此 syscmd 开了一个
// 有上限的固定模板通道（Spec.Stdin），而不是退回去拼 shell。

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"upanel/internal/syscmd"
)

// 写原语用到的固定程序路径与时序参数。
const (
	// DefaultSfdiskPath 是分区表的唯一写入者。路径固定，不来自配置、不来自 HTTP。
	DefaultSfdiskPath = "/usr/sbin/sfdisk"
	// DefaultUdevadmPath 只用来 `udevadm settle`：等设备事件队列排空。
	DefaultUdevadmPath = "/usr/bin/udevadm"
	// DefaultDevDir 是设备节点目录。
	DefaultDevDir = "/dev"

	// WriteCommandTimeout 是单条写命令的时限。
	WriteCommandTimeout = 30 * time.Second
	// DefaultSettleTimeout 是 `udevadm settle` 的时限。
	DefaultSettleTimeout = 15 * time.Second
	// DefaultKernelWait 是等待内核/udev 反映新分区表的上限。
	DefaultKernelWait = 15 * time.Second
	// defaultKernelPoll 是轮询内核视图的间隔。
	defaultKernelPoll = 300 * time.Millisecond
	// outputExcerpt 是错误消息里保留的命令输出长度。
	outputExcerpt = 400
)

// 写动作的进度步骤（稳定枚举，进 job_events 的 payload.step）。
const (
	WriteStepRecheck = "recheck"
	WriteStepCommand = "sfdisk"
	WriteStepSettle  = "udev_settle"
	WriteStepVerify  = "verify"
)

// ErrUnsafeWrite 表示参数在写原语这一层被拒（对齐、越界、编号不合法）。
//
// 它本该只在“预检与执行之间出了矛盾”时出现——判决层已经检查过同样的规则。留着它是因为
// 写原语不能只依赖另一层：真正把字节交给内核的是这里。
var ErrUnsafeWrite = errors.New("storage: 写参数不安全")

// ErrNoWritePrimitive 表示这个动作还没有写原语。
//
// P07.3 起四个存储写动作都有原语了，所以它在写路径上不再出现（格式化由 format.go
// 执行）。留着是因为动作登记表会继续长大：一个还没实现写原语的动作必须明确说
// “没有”，而不是“什么也没做却返回成功”。
var ErrNoWritePrimitive = errors.New("storage: 动作还没有写原语")

// CommandRunner 执行一次外部命令。生产实现是 syscmd.Run（见 syscmd_adapter.go）。
type CommandRunner interface {
	Run(ctx context.Context, spec syscmd.Spec, limits syscmd.Limits) (syscmd.Result, error)
}

// CommandRunnerFunc 让函数直接满足 CommandRunner。
type CommandRunnerFunc func(ctx context.Context, spec syscmd.Spec, limits syscmd.Limits) (syscmd.Result, error)

// Run 实现 CommandRunner。
func (f CommandRunnerFunc) Run(ctx context.Context, spec syscmd.Spec, limits syscmd.Limits) (syscmd.Result, error) {
	return f(ctx, spec, limits)
}

// NodeChecker 判断设备节点是否存在。生产实现读真实 /dev。
//
// 单独抽出接口不是因为“方便测试”，而是因为节点存在性确实是 udev 的产物：分区在 sysfs
// 里出现不等于节点就绪，而 P07.3 的格式化要用节点。等待条件必须能问这两个问题。
type NodeChecker interface {
	Exists(name string) (bool, error)
}

// OSNodeChecker 读真实设备目录。
type OSNodeChecker struct {
	// Dev 是设备目录（真实值 /dev）。
	Dev string
}

// Exists 报告 name 是否是设备目录下的一个设备节点。
func (o OSNodeChecker) Exists(name string) (bool, error) {
	dir := o.Dev
	if dir == "" {
		dir = DefaultDevDir
	}
	// 只接受单个路径成分：名字来自内核设备名，不允许出现分隔符或 ".."。
	if name == "" || name != filepath.Base(name) || name == "." || name == ".." {
		return false, fmt.Errorf("storage: 设备名不合法 %q", name)
	}
	fi, err := os.Stat(filepath.Join(dir, name))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return fi.Mode()&os.ModeDevice != 0, nil
}

// TopologyScanner 现场扫描一次拓扑。写路径用它做执行前复核与写后读回。
type TopologyScanner interface {
	ScanNow(ctx context.Context) (Topology, error)
}

// collectorScanner 用采集器自己的扫描，顺带把缓存刷新成最新的一份。
type collectorScanner struct{ c *Collector }

func (s collectorScanner) ScanNow(ctx context.Context) (Topology, error) {
	if s.c == nil {
		return Topology{}, errors.New("storage: 采集器未接线")
	}
	return s.c.Refresh(ctx), nil
}

// WriteOptions 是写原语的外部依赖。零值不可执行：Runner 为 nil 时写动作一律拒绝。
type WriteOptions struct {
	// Scanner 现场扫描拓扑；nil 表示不可执行。
	Scanner TopologyScanner
	// Env 是判决需要的探针（签名、占用），必须与预检用的是同一组。
	Env Env
	// Runner 执行 sfdisk / udevadm；nil 表示本机没有命令执行器。
	Runner CommandRunner
	// Nodes 判断设备节点；nil 时用真实 /dev。
	Nodes NodeChecker
	// Dev 是设备目录（真实值 /dev）；空时用 DefaultDevDir。
	Dev string
	// Sfdisk / Udevadm / MkfsExt4 / MkfsXFS 是固定程序路径；空时用默认值。
	Sfdisk   string
	Udevadm  string
	MkfsExt4 string
	MkfsXFS  string
	// Superblocks 读取文件系统的超级块（格式化后的读回核验）。nil 表示本机不能格式化。
	Superblocks SuperblockProber
	// SettleTimeout / KernelWait 是 udev 稳定与内核视图等待的上限；0 时用默认值。
	SettleTimeout time.Duration
	KernelWait    time.Duration
	Logger        *slog.Logger
}

// writer 是写原语的实现体。
type writer struct {
	scanner     TopologyScanner
	env         Env
	runner      CommandRunner
	nodes       NodeChecker
	dev         string
	sfdisk      string
	udevadm     string
	mkfsExt4    string
	mkfsXFS     string
	superblocks SuperblockProber
	settle      time.Duration
	wait        time.Duration
	poll        time.Duration
	logger      *slog.Logger
}

func newWriter(opts WriteOptions) *writer {
	pick := func(v, def string) string {
		if strings.TrimSpace(v) == "" {
			return def
		}
		return v
	}
	positive := func(v, def time.Duration) time.Duration {
		if v <= 0 {
			return def
		}
		return v
	}
	dev := pick(opts.Dev, DefaultDevDir)
	nodes := opts.Nodes
	if nodes == nil {
		nodes = OSNodeChecker{Dev: dev}
	}
	return &writer{
		scanner:     opts.Scanner,
		env:         opts.Env,
		runner:      opts.Runner,
		nodes:       nodes,
		dev:         dev,
		sfdisk:      pick(opts.Sfdisk, DefaultSfdiskPath),
		udevadm:     pick(opts.Udevadm, DefaultUdevadmPath),
		mkfsExt4:    pick(opts.MkfsExt4, DefaultMkfsExt4Path),
		mkfsXFS:     pick(opts.MkfsXFS, DefaultMkfsXFS),
		superblocks: opts.Superblocks,
		settle:      positive(opts.SettleTimeout, DefaultSettleTimeout),
		wait:        positive(opts.KernelWait, DefaultKernelWait),
		poll:        defaultKernelPoll,
		logger:      opts.Logger,
	}
}

// WriteRequest 是一次写动作的执行请求（来自已经预检通过、已经复核过的代理操作）。
type WriteRequest struct {
	// Action 是三个写动作之一。
	Action string
	// Params 是预检落库的规范化参数。
	Params any
	// ExpectedIdentity / ExpectedStateVersion 是预检绑定的目标身份与状态版本。
	ExpectedIdentity     string
	ExpectedStateVersion string
	// Progress 回报一个步骤（可空）。
	Progress func(step string, fields map[string]any)
}

// WriteReport 是核验通过之后的结果。
type WriteReport struct {
	// Observed 是核验时读回的实际状态（进 result.observed_state）。
	Observed map[string]any
	// Data 是动作自有信息（进 result.data）。
	Data map[string]any
}

// preparedWrite 是动作参数解出来的内部形态，只含已校验的数字与已限制字符集的名字。
type preparedWrite struct {
	action   string
	targetID string

	tableType string

	startBytes uint64
	sizeBytes  uint64
	name       string

	// filesystem / label 只用于格式化：前者来自闭集（ext4/xfs），后者已过 ValidateLabel。
	filesystem string
	label      string

	// deleteNumber 是删除动作要用的槽位号（从盘上的 GPT 条目读出来，不来自参数）。
	deleteNumber int
}

// prepareWrite 把参数转成内部形态。类型不匹配是编程错误，不是业务拒绝。
func prepareWrite(action string, params any) (preparedWrite, error) {
	switch action {
	case ActionPartitionTableInitialize:
		p, ok := params.(PartitionTableInitializeParams)
		if !ok {
			return preparedWrite{}, fmt.Errorf("storage: %s 的参数类型不匹配: %T", action, params)
		}
		return preparedWrite{action: action, targetID: p.DeviceID, tableType: p.TableType}, nil
	case ActionPartitionCreate:
		p, ok := params.(PartitionCreateParams)
		if !ok {
			return preparedWrite{}, fmt.Errorf("storage: %s 的参数类型不匹配: %T", action, params)
		}
		return preparedWrite{
			action: action, targetID: p.DeviceID,
			startBytes: p.StartBytes, sizeBytes: p.SizeBytes, name: p.Name,
		}, nil
	case ActionPartitionDelete:
		p, ok := params.(PartitionDeleteParams)
		if !ok {
			return preparedWrite{}, fmt.Errorf("storage: %s 的参数类型不匹配: %T", action, params)
		}
		return preparedWrite{action: action, targetID: p.DeviceID}, nil
	case ActionFilesystemFormat:
		p, ok := params.(FilesystemFormatParams)
		if !ok {
			return preparedWrite{}, fmt.Errorf("storage: %s 的参数类型不匹配: %T", action, params)
		}
		return preparedWrite{
			action: action, targetID: p.DeviceID,
			filesystem: p.Filesystem, label: p.Label,
		}, nil
	default:
		return preparedWrite{}, fmt.Errorf("%w: %s", ErrNoWritePrimitive, action)
	}
}

// execute 是一次写动作的全过程：复核 → 写 → 等 udev → 读回核验。
//
// 出口只有一个：无论在哪一步失败，只要**任务上下文已经结束**，返回的错误就统一成
// 中断。理由是这一步写的是磁盘——上下文没了意味着“这次动作没有跑完”，而不是
// “这次动作没有生效”。runner 用 errors.Is(err, context.Canceled) 区分这两种收尾：
// 认不出就是 failed（可重试、可放心重来），认得出就是 interrupted（实际状态未知，
// 要人工核实）。把“复核阶段上下文就结束了”报成 SIGNATURE_UNREADABLE 之类的
// 业务拒绝，会让人以为盘上什么都没发生。
func (w *writer) execute(ctx context.Context, req WriteRequest) (WriteReport, error) {
	report, err := w.performWrite(ctx, req)
	if err == nil {
		return report, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(err, ctxErr) {
		return WriteReport{}, interruptedRun(ctxErr, targetDeviceID(req.Params))
	}
	return WriteReport{}, err
}

// performWrite 是 execute 的主体：它只负责“做”，中断的归类留给调用它的那一层。
func (w *writer) performWrite(ctx context.Context, req WriteRequest) (WriteReport, error) {
	if w == nil || w.runner == nil {
		return WriteReport{}, dependencyMissing("没有配置命令执行器，本机不能执行磁盘写动作")
	}
	if w.scanner == nil {
		return WriteReport{}, dependencyMissing("没有配置存储拓扑扫描，本机不能执行磁盘写动作")
	}
	prep, err := prepareWrite(req.Action, req.Params)
	if err != nil {
		return WriteReport{}, err
	}
	// 缺程序的判断按动作区分：格式化不需要 sfdisk，分区表动作不需要 mkfs。
	// 两者都不能在“命令找不到”时才报错——那会让管理员以为是盘的问题。
	if prep.action == ActionFilesystemFormat {
		if err := w.requireFormatPrograms(prep.filesystem); err != nil {
			return WriteReport{}, err
		}
	} else if err := w.requirePartitionPrograms(); err != nil {
		return WriteReport{}, err
	}

	// —— 1. 写之前重新看一眼 ——
	topo, err := w.scanner.ScanNow(ctx)
	if err != nil {
		return WriteReport{}, commandFailed("执行前重新扫描存储拓扑失败：%v", err)
	}
	if topo.Status == StatusError {
		return WriteReport{}, commandFailed("执行前重新扫描存储拓扑失败：%s", topo.Error)
	}
	assessment, err := Assess(ctx, topo, req.Action, req.Params, w.env)
	if err != nil {
		return WriteReport{}, err
	}
	if assessment.IdentityFingerprint != req.ExpectedIdentity {
		return WriteReport{}, identityChanged(assessment.DisplayName)
	}
	if assessment.StateVersion != req.ExpectedStateVersion {
		return WriteReport{}, preflightStale("拓扑版本是 " + assessment.StateVersion)
	}
	if !assessment.Allowed {
		return WriteReport{}, notAllowed(assessment.Blockers)
	}
	if len(assessment.DiskIDs) != 1 {
		// 预检已经在“多块物理盘”上拒绝过（RAID/dm/LVM）；这里再断一次，免得写错盘。
		return WriteReport{}, notAllowed([]Blocker{{
			Code:    NoticeStackedDevice,
			Message: "目标涉及多块物理盘，本阶段不支持",
		}})
	}
	diskID := assessment.DiskIDs[0]
	index := indexTopology(topo)
	disk := index.devices[diskID]
	if disk == nil {
		return WriteReport{}, preflightStale("整盘 " + diskID + " 不在当前拓扑里")
	}
	if err := checkWritableSectors(disk); err != nil {
		return WriteReport{}, err
	}
	before, err := tableStateOf(disk)
	if err != nil {
		return WriteReport{}, preflightStale(diskID + " 的分区表读不到：" + err.Error())
	}
	if prep.action == ActionPartitionTableInitialize {
		if before.Type != TableNone {
			return WriteReport{}, preflightStale(diskID + " 上现在有分区表（" + before.Type + "）")
		}
	} else if before.Type != TableGPT {
		return WriteReport{}, preflightStale(diskID + " 的分区表类型是 " + before.Type + "，不是 gpt")
	}
	// 复核已经通过（还是那块盘、还是那个状态、表还是 GPT、目标还在）：从这里开始，
	// 后面每一条路径都真的会去动设备，因此“复核完成”这条进度对两者都是同一个意思。
	progress(req, WriteStepRecheck, map[string]any{
		"device_id": diskID,
		"version":   assessment.StateVersion,
	})
	// 格式化与三个分区表动作共用上面的复核，但从这里分开：它不改变分区表，
	// 也不存在“写完之后表应该长什么样”的期望形态。
	if prep.action == ActionFilesystemFormat {
		return w.executeFormat(ctx, req, prep, topo, disk, diskID, before)
	}
	if prep.action == ActionPartitionDelete {
		entry, ok := findEntry(before.Entries, prep.targetID, disk.PartitionTable)
		if !ok {
			return WriteReport{}, preflightStale(prep.targetID + " 不在 " + diskID + " 盘上的 GPT 里")
		}
		prep.deleteNumber = entry.Number
	}
	// 这次动作期望得到的分区形态（写之前就从“现在的表 + 意图”算出来）。
	want, err := expectedState(prep, disk, before)
	if err != nil {
		return WriteReport{}, err
	}

	// —— 2. 写 ——
	spec, err := w.command(prep, diskID)
	if err != nil {
		return WriteReport{}, err
	}
	progress(req, WriteStepCommand, map[string]any{"program": spec.Program, "device_id": diskID})
	res, runErr := w.runCommand(ctx, spec)
	if runErr != nil {
		return WriteReport{}, w.describeCommandFailure(ctx, diskID, res, runErr)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		// 命令返回了，但上下文已经结束（超时/停机）：这时连“成功与否”都不能下结论。
		return WriteReport{}, commandFailed(
			"sfdisk 已返回，但任务上下文结束（%v），实际状态未知。重新提交之前先核实 %s 的实际状态",
			ctxErr, diskID)
	}

	// —— 3. 等 udev 稳定，等内核与节点反映出来 ——
	progress(req, WriteStepSettle, nil)
	settled := w.settleUdev(ctx)
	verifyTopo, kernel, kernelDetail := w.waitForKernel(ctx, diskID, want)

	// —— 4. 读回核验 ——
	progress(req, WriteStepVerify, nil)
	after, err := tableStateOf(findDevice(verifyTopo, diskID))
	if err != nil {
		return WriteReport{}, commandFailed(
			"写入后读不回 %s 的分区表（%v）。不要直接重新提交：先在主机上确认这块盘的实际状态", diskID, err)
	}
	if after.Type != TableGPT {
		// 命令返回 0 不等于写进去了：分区表类型是读回来的第一手证据。
		return WriteReport{}, commandFailed(
			"写入后 %s 上的分区表类型是 %s，期望 gpt。核实实际状态之前不要重新提交", diskID, after.Type)
	}
	if detail := compareEntries(want.entries, after.Entries); detail != "" {
		return WriteReport{}, commandFailed(
			"命令已结束，但盘上的 GPT 与预期不一致：%s。实际状态：%s。核实之前不要重新提交",
			detail, describeEntries(after.Entries))
	}
	if kernelDetail != "" {
		return WriteReport{}, commandFailed(
			"盘上的 GPT 已按预期写入，但内核/udev 的视图没有跟上：%s。核实系统状态之后才能重新提交",
			kernelDetail)
	}

	return WriteReport{
		Observed: observedState(diskID, after, kernel, settled),
		Data:     reportData(prep, diskID, after, verifyTopo),
	}, nil
}

// observedState 是核验时读回的实际状态。
func observedState(diskID string, after tableState, kernel []Partition, settled bool) map[string]any {
	ids := make([]string, 0, len(kernel))
	for _, p := range kernel {
		ids = append(ids, p.DeviceID)
	}
	return map[string]any{
		"device_id":         diskID,
		"table_type":        after.Type,
		"disk_guid":         after.DiskGUID,
		"disk_entries":      after.Entries,
		"kernel_partitions": ids,
		"udev_settled":      settled,
	}
}

// reportData 组装 result.data：这次动作建了/删了哪一条。
func reportData(prep preparedWrite, diskID string, after tableState, topo Topology) map[string]any {
	data := map[string]any{
		"device_id":   diskID,
		"table_type":  after.Type,
		"partitions":  len(after.Entries),
		"checked_via": []string{"disk_bytes", "kernel_sysfs"},
	}
	switch prep.action {
	case ActionPartitionCreate:
		entry, ok := entryByStart(after.Entries, prep.startBytes)
		if !ok {
			return data
		}
		created := map[string]any{
			"number":      entry.Number,
			"start_bytes": strconv.FormatUint(entry.StartBytes, 10),
			"size_bytes":  strconv.FormatUint(entry.SizeBytes, 10),
			"name":        entry.Name,
		}
		// 内核里的设备名（sdb1）是后续格式化要用的标识：只有 /dev 节点存在时它才有用，
		// 而 waitForKernel 已经确认过节点存在。
		if disk := findDevice(topo, diskID); disk != nil && disk.PartitionTable != nil {
			if p, ok := partitionAt(disk.PartitionTable.Partitions, prep.startBytes); ok {
				created["device_id"] = p.DeviceID
			}
		}
		data["created_partition"] = created
	case ActionPartitionDelete:
		data["deleted_device_id"] = prep.targetID
	}
	return data
}

// tableStateOf 取出一次扫描里盘上的 GPT 形态。读不到就是错误——核验不能“跳过”。
func tableStateOf(dev *Device) (tableState, error) {
	if dev == nil {
		return tableState{}, errors.New("设备不在拓扑里")
	}
	t := dev.PartitionTable
	if t == nil {
		return tableState{}, errors.New("没有分区表信息")
	}
	if t.Source != TableSourceDevice {
		reason := t.Warning
		if reason == "" {
			reason = "来源是 " + t.Source
		}
		return tableState{}, fmt.Errorf("盘上的分区表没有被读到（%s）", reason)
	}
	// 读到了 GPT 却拿不到条目表 = 读回来的东西不足以核验；不拿 nil 当“零条”。
	if t.Type == TableGPT && t.DiskEntries == nil {
		return tableState{}, fmt.Errorf("盘上的分区表是 %s，但条目表没有被读到", t.Type)
	}
	var entries []GPTEntry
	if t.DiskEntries != nil {
		entries = *t.DiskEntries
	}
	return tableState{Type: t.Type, DiskGUID: t.DiskGUID, Entries: entries}, nil
}

// checkWritableSectors 拒绝逻辑扇区不是 512 字节的盘。
//
// 理由是采集与核验两侧都按 512 字节扇区解释 GPT 的 LBA（table.go 的 sectorSize），
// 4Kn 盘的 LBA 是 4096 字节，按 512 解释会算出错误的起点与空闲区——那正是“看起来允许、
// 实际写错位置”的一类事故。不支持就说 UNSUPPORTED_DEVICE，不猜。
func checkWritableSectors(disk *Device) error {
	switch disk.LogicalSectorBytes {
	case sectorSize:
		return nil
	case 0:
		return unsupportedDevice("读不到 %s 的逻辑扇区大小，无法安全换算分区位置", disk.ID)
	default:
		return unsupportedDevice("%s 的逻辑扇区是 %d 字节（本阶段只支持 512 字节扇区）",
			disk.ID, disk.LogicalSectorBytes)
	}
}

// requirePartitionPrograms 确认分区表写动作需要的程序就在固定路径上。
//
// 缺了就说 DEPENDENCY_MISSING（503），而不是让命令执行器抛一个 "no such file or directory"
// 让管理员以为是盘的问题。
func (w *writer) requirePartitionPrograms() error {
	return requireFiles([]programNeed{
		{"sfdisk", w.sfdisk},
		{"udevadm", w.udevadm},
	})
}

// requireFormatPrograms 确认格式化这台设备所需的程序在固定路径上。
func (w *writer) requireFormatPrograms(filesystem string) error {
	need := []programNeed{{"udevadm", w.udevadm}}
	switch filesystem {
	case FilesystemExt4:
		need = append(need, programNeed{"mkfs.ext4", w.mkfsExt4})
	case FilesystemXFS:
		need = append(need, programNeed{"mkfs.xfs", w.mkfsXFS})
	default:
		return unsupportedDevice("文件系统 %q 没有格式化原语", filesystem)
	}
	return requireFiles(need)
}

// requireAllPrograms 检查本机是否具备全部写原语的固定依赖。
//
// 装配时调用一次（CheckWriteDependencies）：缺依赖不该阻止代理启动（拓扑与预检仍然
// 有用），但启动日志里必须说清楚哪几个缺，否则管理员只会在第一次点按钮时才发现。
func (w *writer) requireAllPrograms() error {
	need := []programNeed{
		{"sfdisk", w.sfdisk},
		{"udevadm", w.udevadm},
		{"mkfs.ext4", w.mkfsExt4},
		{"mkfs.xfs", w.mkfsXFS},
	}
	var missing []error
	for _, n := range need {
		if err := requireFiles([]programNeed{n}); err != nil {
			missing = append(missing, err)
		}
	}
	return errors.Join(missing...)
}

// programNeed 是一个“必须存在于固定路径”的程序。
type programNeed struct{ name, path string }

// requireFiles 逐个确认程序存在且是普通文件。
func requireFiles(need []programNeed) error {
	for _, n := range need {
		fi, err := os.Stat(n.path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return dependencyMissing("本机缺少 %s（%s），这个写动作不可执行", n.name, n.path)
			}
			return dependencyMissing("无法确认 %s（%s）是否可用：%v", n.name, n.path, err)
		}
		if fi.IsDir() {
			return dependencyMissing("%s（%s）是目录，这个写动作不可执行", n.name, n.path)
		}
		// 可执行位只在有意义的平台上检查：Windows 的 FileMode 不含用户可执行位
		// （Go 对 .exe 也报 0666），那里只做存在性检查。生产目标平台是 Linux，两者都查。
		if runtime.GOOS != "windows" && fi.Mode()&0o111 == 0 {
			return dependencyMissing("%s（%s）没有可执行位，这个写动作不可执行", n.name, n.path)
		}
	}
	return nil
}

// runCommand 执行一次写命令。
func (w *writer) runCommand(ctx context.Context, spec syscmd.Spec) (syscmd.Result, error) {
	return w.runner.Run(ctx, spec, syscmd.Limits{Timeout: WriteCommandTimeout})
}

// describeCommandFailure 把一次命令失败说清楚：退出码、输出摘要与**读回的实际状态**。
func (w *writer) describeCommandFailure(ctx context.Context, diskID string, res syscmd.Result, runErr error) error {
	// 上下文在命令执行期间结束（停机、任务超时）：命令是被 syscmd 连同进程组一起杀掉
	// 的，写了多少不知道。这时既不能算成功，也不能当成一次普通的“命令失败”——后者会让
	// 人以为盘没动过。返回带上下文的错误，runner 据此记成 interrupted（实际状态未知）。
	if ctxErr := ctx.Err(); ctxErr != nil {
		return interruptedWrite(ctxErr, diskID, "sfdisk")
	}
	var detail string
	switch {
	case res.TimedOut:
		detail = "命令超时被终止"
	case res.ExitCode >= 0 && res.Program != "":
		detail = "退出码 " + strconv.Itoa(res.ExitCode)
	default:
		detail = "命令没有正常结束"
	}
	output := excerpt(res.Stderr)
	if output == "" {
		output = excerpt(res.Stdout)
	}
	observed := "读回分区表也失败了"
	if topo, err := w.scanner.ScanNow(context.WithoutCancel(ctx)); err == nil {
		if state, stateErr := tableStateOf(findDevice(topo, diskID)); stateErr == nil {
			observed = "盘上现在是 " + describeEntries(state.Entries)
		}
	}
	if output == "" {
		return commandFailed("sfdisk %s（%v）。%s。核实实际状态之前不要重新提交", detail, runErr, observed)
	}
	return commandFailed("sfdisk %s：%s。%s。核实实际状态之前不要重新提交", detail, output, observed)
}

// command 返回这次写动作要执行的固定命令。
func (w *writer) command(prep preparedWrite, diskID string) (syscmd.Spec, error) {
	devPath := filepath.Join(w.dev, diskID)
	switch prep.action {
	case ActionPartitionTableInitialize:
		// `label: gpt`：只写一张新的空 GPT（sfdisk 会同时写保护性 MBR）。
		// `--wipe never`：**不**清盘上已有的文件系统签名——未知签名不自动清，这是
		// PLAN.md 第 9.1 节的硬要求；判决层已经在签名上拒绝过，这里是第二道。
		return syscmd.Spec{
			Program: w.sfdisk,
			Args:    []string{"--wipe", "never", devPath},
			Stdin:   initializeScript(),
		}, nil
	case ActionPartitionCreate:
		if err := checkAligned(prep); err != nil {
			return syscmd.Spec{}, err
		}
		if prep.name != "" {
			// 名字是唯一一个用户字符串，因此拼模板之前再按形状规则检查一次。
			if err := ValidatePartitionName(prep.name); err != nil {
				return syscmd.Spec{}, fmt.Errorf("%w: %v", ErrUnsafeWrite, err)
			}
		}
		return syscmd.Spec{
			Program: w.sfdisk,
			Args:    []string{"--append", "--wipe-partitions", "never", devPath},
			Stdin:   createScript(prep.startBytes/sectorSize, prep.sizeBytes/sectorSize, prep.name),
		}, nil
	case ActionPartitionDelete:
		if prep.deleteNumber < 1 {
			return syscmd.Spec{}, fmt.Errorf("%w: 分区编号 %d 不合法", ErrUnsafeWrite, prep.deleteNumber)
		}
		// 按槽位号删：实测 2.39.3 删掉中间一条**不会**重排后面的编号，因此删除只影响
		// 目标那一条，其余分区的设备名保持不变。
		return syscmd.Spec{
			Program: w.sfdisk,
			Args:    []string{"--delete", devPath, strconv.Itoa(prep.deleteNumber)},
		}, nil
	default:
		return syscmd.Spec{}, fmt.Errorf("%w: %s", ErrNoWritePrimitive, prep.action)
	}
}

// checkAligned 在写之前再断言一次对齐与最小尺寸。
func checkAligned(prep preparedWrite) error {
	if prep.startBytes%AlignBytes != 0 || prep.sizeBytes%AlignBytes != 0 {
		return fmt.Errorf("%w: start=%d size=%d 没有按 %d 字节对齐",
			ErrUnsafeWrite, prep.startBytes, prep.sizeBytes, AlignBytes)
	}
	if prep.sizeBytes < MinPartitionBytes {
		return fmt.Errorf("%w: size=%d 小于最小分区 %d", ErrUnsafeWrite, prep.sizeBytes, MinPartitionBytes)
	}
	return nil
}

// initializeScript 是初始化 GPT 的固定脚本（标准输入）。
//
// 只有一行 `label: gpt`：没有分区、没有 label-id、没有 --force。sfdisk 自己算
// first-lba（实测 2.39.3 给 2048，正好是 1 MiB 对齐的起点）。
func initializeScript() []byte { return []byte("label: gpt\n") }

// createScript 生成一条新分区的固定脚本。
//
// 字段全是**已校验的值**：起点与大小是 1 MiB 对齐后换算出的扇区数，类型固定 Linux
// filesystem（不接受用户选择），名字只可能是可打印 ASCII 且不含引号与反斜杠
// （见 ValidatePartitionName：sfdisk 的脚本语法用双引号括起名字，实测双引号无法转义）。
func createScript(startSector, sizeSector uint64, name string) []byte {
	var b strings.Builder
	b.WriteString("start=")
	b.WriteString(strconv.FormatUint(startSector, 10))
	b.WriteString(", size=")
	b.WriteString(strconv.FormatUint(sizeSector, 10))
	b.WriteString(", type=linux")
	if name != "" {
		b.WriteString(", name=\"")
		b.WriteString(name)
		b.WriteString("\"")
	}
	b.WriteString("\n")
	return []byte(b.String())
}

// settleUdev 等设备事件队列排空。失败不中止：真正的判断在随后的内核视图等待与读回。
func (w *writer) settleUdev(ctx context.Context) bool {
	spec := syscmd.Spec{
		Program: w.udevadm,
		Args:    []string{"settle", "--timeout=" + strconv.Itoa(int(w.settle.Seconds()))},
	}
	res, err := w.runner.Run(ctx, spec, syscmd.Limits{Timeout: w.settle + 5*time.Second})
	if err != nil {
		if w.logger != nil {
			w.logger.Warn("udevadm settle 未成功（继续读回核验）",
				"exit_code", res.ExitCode, "error", err, "stderr", excerpt(res.Stderr))
		}
		return false
	}
	return true
}

// waitForKernel 轮询到内核视图与 /dev 节点都符合预期，或到达等待上限。
//
// 返回最后一次扫描到的拓扑、内核分区列表，以及不一致的说明（一致时为空串）。
func (w *writer) waitForKernel(ctx context.Context, diskID string, want expectation) (Topology, []Partition, string) {
	deadline := time.Now().Add(w.wait)
	var lastTopo Topology
	var lastParts []Partition
	for {
		topo, parts, detail := w.readKernelView(ctx, diskID, want)
		if len(topo.Nodes) > 0 {
			lastTopo = topo
		}
		lastParts = parts
		if detail == "" {
			return lastTopo, lastParts, ""
		}
		if time.Now().After(deadline) {
			return lastTopo, lastParts, detail
		}
		select {
		case <-ctx.Done():
			return lastTopo, lastParts, "等待内核视图时任务上下文结束：" + ctx.Err().Error()
		case <-time.After(w.poll):
		}
	}
}

// readKernelView 读一次内核视图与节点状态，并与期望比对。不一致时返回说明。
func (w *writer) readKernelView(ctx context.Context, diskID string, want expectation) (Topology, []Partition, string) {
	topo, err := w.scanner.ScanNow(ctx)
	if err != nil {
		return Topology{}, nil, "重新扫描拓扑失败：" + err.Error()
	}
	node := findDevice(topo, diskID)
	if node == nil {
		return topo, nil, "整盘 " + diskID + " 不在内核视图里"
	}
	if node.PartitionTable == nil {
		return topo, nil, "读不到 " + diskID + " 的内核分区列表"
	}
	parts := node.PartitionTable.Partitions
	if msg := comparePartitions(want, parts); msg != "" {
		return topo, parts, msg
	}
	// 节点就绪：新建的分区必须已有节点，删掉的分区必须已经没有节点。
	if want.addedStart != 0 {
		part, ok := partitionAt(parts, want.addedStart)
		if !ok {
			return topo, parts, "内核里没有起点为 " + strconv.FormatUint(want.addedStart, 10) + " 的分区"
		}
		exists, nodeErr := w.nodes.Exists(part.DeviceID)
		if nodeErr != nil {
			return topo, parts, "检查 /dev/" + part.DeviceID + " 失败：" + nodeErr.Error()
		}
		if !exists {
			return topo, parts, "/dev/" + part.DeviceID + " 还没有出现"
		}
	}
	if want.removedDeviceID != "" {
		exists, nodeErr := w.nodes.Exists(want.removedDeviceID)
		if nodeErr != nil {
			return topo, parts, "检查 /dev/" + want.removedDeviceID + " 失败：" + nodeErr.Error()
		}
		if exists {
			return topo, parts, "/dev/" + want.removedDeviceID + " 仍然存在"
		}
	}
	return topo, parts, ""
}

// expectation 是这次动作期望得到的结果。
type expectation struct {
	// entries 是期望的完整 GPT 条目集合。
	entries []GPTEntry
	// addedStart 是这次新建的分区起点（0 表示没有新建）。
	addedStart uint64
	// removedDeviceID 是这次删掉的分区（空串表示没有删除）。
	removedDeviceID string
}

// expectedState 从“现在的表 + 意图”算出期望结果。
func expectedState(prep preparedWrite, disk *Device, before tableState) (expectation, error) {
	switch prep.action {
	case ActionPartitionTableInitialize:
		// 初始化之后是一张空 GPT。
		return expectation{entries: []GPTEntry{}}, nil
	case ActionPartitionCreate:
		if err := checkAligned(prep); err != nil {
			return expectation{}, err
		}
		if prep.startBytes+prep.sizeBytes > disk.SizeBytes {
			return expectation{}, fmt.Errorf("%w: 请求区间越过盘尾（%d + %d > %d）",
				ErrUnsafeWrite, prep.startBytes, prep.sizeBytes, disk.SizeBytes)
		}
		want := make([]GPTEntry, 0, len(before.Entries)+1)
		want = append(want, before.Entries...)
		want = append(want, GPTEntry{
			// Number 留 0：编号由 sfdisk 选最小的空槽，产品不预测它，
			// 核验按起点与内容比对（见 compareEntries）。
			Number:     0,
			StartBytes: prep.startBytes,
			SizeBytes:  prep.sizeBytes,
			TypeGUID:   gptTypeLinuxFilesystem,
			Name:       prep.name,
		})
		return expectation{entries: want, addedStart: prep.startBytes}, nil
	case ActionPartitionDelete:
		want := make([]GPTEntry, 0, len(before.Entries))
		for _, e := range before.Entries {
			if e.Number == prep.deleteNumber {
				continue
			}
			want = append(want, e)
		}
		return expectation{entries: want, removedDeviceID: prep.targetID}, nil
	default:
		return expectation{}, fmt.Errorf("%w: %s", ErrNoWritePrimitive, prep.action)
	}
}

// findEntry 在盘上的 GPT 条目里找内核设备名对应的那一条（按起点与 sysfs 的分区对齐）。
func findEntry(entries []GPTEntry, deviceID string, table *PartitionTable) (GPTEntry, bool) {
	if table == nil {
		return GPTEntry{}, false
	}
	var start uint64
	found := false
	for _, p := range table.Partitions {
		if p.DeviceID == deviceID {
			start = p.StartBytes
			found = true
			break
		}
	}
	if !found {
		return GPTEntry{}, false
	}
	return entryByStart(entries, start)
}

// tableState 是读回的分区表形态。
type tableState struct {
	Type     string
	DiskGUID string
	Entries  []GPTEntry
}

// compareEntries 对比期望与实际，返回不一致的说明（一致时为空串）。
//
// 规则：
//   - 已经存在的条目必须原样保留（编号、起点、大小、名字、类型），这样“删错一条”
//     或“编号被重排”都会被抓到；
//   - 新建的那一条按起点定位，比较大小、名字与类型；编号不比较（由 sfdisk 选空槽）；
//   - 条目数量必须一致（多了少了都是不一致）。
func compareEntries(want, got []GPTEntry) string {
	if len(want) != len(got) {
		return fmt.Sprintf("期望 %d 个分区，实际 %d 个", len(want), len(got))
	}
	var fresh *GPTEntry
	for i := range want {
		if want[i].Number == 0 {
			fresh = &want[i]
			continue
		}
		actual, ok := entryByNumber(got, want[i].Number)
		if !ok {
			return fmt.Sprintf("分区 %d 不见了", want[i].Number)
		}
		if actual.StartBytes != want[i].StartBytes || actual.SizeBytes != want[i].SizeBytes {
			return fmt.Sprintf("分区 %d 的区间被改动了", want[i].Number)
		}
		if actual.Name != want[i].Name {
			return fmt.Sprintf("分区 %d 的名字是 %q，期望 %q", want[i].Number, actual.Name, want[i].Name)
		}
		if actual.TypeGUID != want[i].TypeGUID {
			return fmt.Sprintf("分区 %d 的类型是 %s，期望 %s", want[i].Number, actual.TypeGUID, want[i].TypeGUID)
		}
	}
	if fresh == nil {
		return ""
	}
	actual, ok := entryByStart(got, fresh.StartBytes)
	if !ok {
		return "新建的分区不在盘上（起点 " + strconv.FormatUint(fresh.StartBytes, 10) + "）"
	}
	if actual.SizeBytes != fresh.SizeBytes {
		return fmt.Sprintf("新建分区的大小是 %d，期望 %d", actual.SizeBytes, fresh.SizeBytes)
	}
	if actual.Name != fresh.Name {
		return fmt.Sprintf("新建分区的名字是 %q，期望 %q", actual.Name, fresh.Name)
	}
	if actual.TypeGUID != fresh.TypeGUID {
		return fmt.Sprintf("新建分区的类型是 %s，期望 %s", actual.TypeGUID, fresh.TypeGUID)
	}
	return ""
}

// comparePartitions 对比期望与内核分区列表，返回不一致的说明。
func comparePartitions(want expectation, got []Partition) string {
	if len(want.entries) != len(got) {
		return fmt.Sprintf("内核认出 %d 个分区，期望 %d 个", len(got), len(want.entries))
	}
	for _, e := range want.entries {
		if _, ok := partitionAt(got, e.StartBytes); !ok {
			return "内核里没有起点为 " + strconv.FormatUint(e.StartBytes, 10) + " 的分区"
		}
	}
	return ""
}

// partitionAt 找起点匹配的内核分区。
func partitionAt(parts []Partition, start uint64) (Partition, bool) {
	for _, p := range parts {
		if p.StartBytes == start {
			return p, true
		}
	}
	return Partition{}, false
}

// entryByNumber 按槽位号找条目。
func entryByNumber(entries []GPTEntry, n int) (GPTEntry, bool) {
	for _, e := range entries {
		if e.Number == n {
			return e, true
		}
	}
	return GPTEntry{}, false
}

// entryByStart 按起点找条目。
func entryByStart(entries []GPTEntry, start uint64) (GPTEntry, bool) {
	for _, e := range entries {
		if e.StartBytes == start {
			return e, true
		}
	}
	return GPTEntry{}, false
}

// describeEntries 给出一句人读的分区列表（错误消息用）。
func describeEntries(entries []GPTEntry) string {
	if len(entries) == 0 {
		return "没有分区"
	}
	parts := make([]string, 0, len(entries))
	for _, e := range entries {
		name := ""
		if e.Name != "" {
			name = " name=" + e.Name
		}
		parts = append(parts, fmt.Sprintf("#%d start=%d size=%d%s", e.Number, e.StartBytes, e.SizeBytes, name))
	}
	return strings.Join(parts, "；")
}

// excerpt 截断命令输出，避免把整段 sfdisk 报告塞进日志与消息。
func excerpt(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > outputExcerpt {
		return s[:outputExcerpt] + "…"
	}
	return s
}

// findDevice 在拓扑里按 id 找设备。
func findDevice(topo Topology, id string) *Device {
	for i := range topo.Nodes {
		if topo.Nodes[i].ID == id {
			return &topo.Nodes[i]
		}
	}
	return nil
}

// progress 安全地回报一个步骤（req.Progress 可空）。
func progress(req WriteRequest, step string, fields map[string]any) {
	if req.Progress == nil {
		return
	}
	req.Progress(step, fields)
}
