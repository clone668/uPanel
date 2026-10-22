package storage

// P07.3：格式化——把一台分区做成 ext4 或 XFS，并**读回来**证明它真的变了。
//
// 与 P07.2 的三件套同一套纪律，但每一件事都换成了文件系统的说法：
//
//  1. **写之前重新看一眼**：重算判决、比对预检绑定的身份指纹与状态版本（这一步由
//     write.go 的 execute 统一做），然后在这里再确认三件与“这台分区”有关的事：
//     它还在内核视图里、它在盘上的起点与 GPT 条目一致、它的 /dev 节点此刻存在。
//     另外先读一次超级块：既拿到旧 UUID（写完之后必须变），也保证在动手之前
//     这台设备确实是读得通的。读不通就不写。
//  2. **写的时候只用固定参数集合**：mkfs.ext4 / mkfs.xfs 的 argv 由代码写死，
//     用户字符串只有卷标，而它在进来之前已经过 ValidateLabel（按文件系统的真实上限
//     限制字节数与字符集），且作为**独立 argv 项**传递，不经 shell。
//  3. **写之后读回来对**：mkfs 退出码 0 不算成功。等 udev 稳定后，用**两条独立的
//     读路径**核对同一台设备：超级块解析器（魔数 + UUID + 标签 + 几何）与 P07.1 的
//     签名分类器（头尾魔数）。两者都与请求一致、且 UUID 确实换过，才算 verified。
//
// 为什么 XFS 一定带 -f，而 ext4 一定不带 -F（实测，见
// docs/evidence/P07/2026-10-22-p07.3/measurements.txt）：
//
//   - mkfs.xfs 6.6.0 只要发现任何文件系统签名就以 rc=1 结束（"Use the -f option to
//     force overwrite"），不带 -f 就无法把一个用过的分区做成 XFS；
//   - mkfs.ext4 1.47.0 覆盖 ext4 / xfs / 随机内容时**都不需要** -F（rc=0），
//     所以 ext4 的固定参数集合里没有 -F；
//   - 两个力标志都**不解开“设备已挂载”检查**：实测 `mkfs.ext4 -F` 与 `mkfs.xfs -f`
//     在已挂载分区上同样以 rc=1 拒绝。也就是说 -f 不是“绕过占用检查”的开关：
//     PLAN.md 第 9.2 节禁止的是拿 force 跳过占用判断，而占用判断在这里全部由预检与
//     执行前的复核完成（挂载、swap、holders、打开者），与 mkfs 的力标志无关。

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"upanel/internal/syscmd"
)

const (
	// DefaultMkfsExt4Path / DefaultMkfsXFS 是格式化唯一的两个写入者。
	// 路径固定，不来自配置、不来自 HTTP（Ubuntu 24.04 上 mkfs.ext4 是指向 mke2fs
	// 的相对符号链接，按绝对路径调用时 argv[0] 仍是 mkfs.ext4，mke2fs 据此锁定 ext4）。
	DefaultMkfsExt4Path = "/usr/sbin/mkfs.ext4"
	// DefaultMkfsXFS 是 XFS 的写入者。
	DefaultMkfsXFS = "/usr/sbin/mkfs.xfs"

	// FormatCommandTimeout 是单条 mkfs 的时限。
	//
	// 比分区表命令的 30 秒长得多：mkfs.ext4 默认会给整台设备发 discard
	// （实测输出里的 "Discarding device blocks"），在大盘或慢盘上这一步是分钟级的。
	// 5 分钟仍在一个任务 10 分钟的总窗口之内，留得出 udev 与读回核验的时间。
	FormatCommandTimeout = 5 * time.Minute

	// 文件系统几何的下限：文件系统至少要覆盖分区的 9/10。
	//
	// mkfs 会按块大小向下取整，所以“完全相等”不是稳定判据；但一个只覆盖分区一小半的
	// 文件系统在正常路径上不可能出现（那意味着 mkfs 做错了设备）。上限是分区大小本身。
	formatCoverNumerator   = 9
	formatCoverDenominator = 10
)

// WriteStepMkfs 是格式化命令这一步的进度名。与分区表的 "sfdisk" 分开，
// 界面与日志据此知道这一次动的是文件系统还是分区表。
const WriteStepMkfs = "mkfs"

// executeFormat 是一次格式化的全过程：复核目标分区 → 读旧超级块 → mkfs → 等 udev → 读回核验。
//
// 进来的时候 write.go 的 execute 已经做完通用复核（重算判决、身份与状态版本比对、
// 表类型必须是 GPT），disk 是目标分区所属的整盘，before 是盘上现在的 GPT 形态。
func (w *writer) executeFormat(ctx context.Context, req WriteRequest, prep preparedWrite,
	topo Topology, disk *Device, diskID string, before tableState) (WriteReport, error) {

	// —— 1a. 目标分区必须还在，且起点与盘上的条目一致 ——
	//
	// “还在”来自 sysfs（内核此刻认定的分区）；“起点一致”是拿它跟盘上的 GPT 条目比。
	// 只有这样才敢说“/dev/sdb1 确实还是预检时那一段字节”，而不是一块被重建过、
	// 编号相同但位置不同的分区。
	target := findDevice(topo, prep.targetID)
	if target == nil || target.Kind != KindPartition {
		return WriteReport{}, preflightStale(prep.targetID + " 不在当前拓扑里")
	}
	part, ok := partitionEntry(disk, prep.targetID)
	if !ok {
		return WriteReport{}, preflightStale(prep.targetID + " 不在 " + diskID + " 的分区列表里")
	}
	entry, ok := findEntry(before.Entries, prep.targetID, disk.PartitionTable)
	if !ok {
		return WriteReport{}, preflightStale(prep.targetID + " 不在 " + diskID + " 盘上的 GPT 里")
	}
	if entry.StartBytes != part.StartBytes {
		return WriteReport{}, preflightStale(fmt.Sprintf(
			"%s 在内核里从 %d 字节开始，盘上的条目说 %d 字节——分区表与内核不一致",
			prep.targetID, part.StartBytes, entry.StartBytes))
	}
	if target.ReadOnly {
		return WriteReport{}, notAllowed([]Blocker{{
			Code:    NoticeReadOnly,
			Message: prep.targetID + " 被标记为只读，不能写入",
		}})
	}

	// —— 1b. /dev 节点必须存在：它是 mkfs 唯一的参数 ——
	if err := w.requireNode(prep.targetID); err != nil {
		return WriteReport{}, err
	}

	// —— 1c. 程序与探针 ——
	if err := w.requireFormatPrograms(prep.filesystem); err != nil {
		return WriteReport{}, err
	}
	if w.superblocks == nil {
		return WriteReport{}, dependencyMissing("没有配置超级块读取器，本机不能核验格式化结果")
	}

	// —— 1d. 写之前读一次超级块 ——
	//
	// 两个作用：拿到旧 UUID（写完之后必须不一样，否则说明“命令说成功、盘上没变”），
	// 以及在动手之前证明这台设备此刻读得通。
	prev, err := w.superblocks.Probe(ctx, prep.targetID, part.SizeBytes)
	if err != nil {
		return WriteReport{}, commandFailed(
			"格式化之前读不回 %s 的超级块（%v）。没有写入任何内容，核实设备之后重新预检",
			prep.targetID, err)
	}

	// —— 2. 写 ——
	spec, err := w.formatCommand(prep)
	if err != nil {
		return WriteReport{}, err
	}
	progress(req, WriteStepMkfs, map[string]any{
		"program":    spec.Program,
		"device_id":  prep.targetID,
		"filesystem": prep.filesystem,
	})
	res, runErr := w.runFormatCommand(ctx, spec)
	if runErr != nil {
		// 上下文在命令执行期间结束：写到哪一步不知道，既不能算成功也不能算“什么都没做”。
		// 返回可被 errors.Is(err, context.Canceled/DeadlineExceeded) 认出的错误，
		// runner 会记成 interrupted（OPERATION_INTERRUPTED，实际状态未知）。
		if ctxErr := ctx.Err(); ctxErr != nil {
			return WriteReport{}, interruptedWrite(ctxErr, prep.targetID, spec.Program)
		}
		return WriteReport{}, w.describeFormatFailure(ctx, prep, part, res, runErr)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		// mkfs 已返回 0，但后续核验做不了：字节很可能已经写下去了，只是没人证明。
		// 这一条按失败报并明说“实际状态未知”——它不是中断，因为命令确实跑完了。
		return WriteReport{}, commandFailed(
			"mkfs 已返回 0，但任务上下文在此之前结束（%v），没有完成读回核验，实际状态未知。"+
				"重新提交之前先在主机上确认 %s 的文件系统", ctxErr, prep.targetID)
	}

	// —— 3. 等 udev 稳定，等节点还原 ——
	progress(req, WriteStepSettle, nil)
	settled := w.settleUdev(ctx)
	nodeOK := w.waitForFormatNode(ctx, prep.targetID)

	// —— 4. 读回核验（两条独立读路径） ——
	progress(req, WriteStepVerify, nil)
	after, err := w.superblocks.Probe(ctx, prep.targetID, part.SizeBytes)
	if err != nil {
		return WriteReport{}, commandFailed(
			"mkfs 已返回 0，但读不回 %s 的超级块（%v）。不要直接重新提交：先确认这台分区的实际状态",
			prep.targetID, err)
	}
	if detail := compareFormatIdentity(prep, prev, after, part.SizeBytes); detail != "" {
		return WriteReport{}, commandFailed(
			"mkfs 已返回 0，但盘上的文件系统与请求不一致：%s。实际读到：%s。核实之前不要重新提交",
			detail, after.String())
	}
	if w.env.Signatures == nil {
		return WriteReport{}, dependencyMissing("没有配置签名探针，无法完成格式化后的第二源核验")
	}
	sig, err := w.env.Signatures.Probe(ctx, prep.targetID, part.SizeBytes)
	if err != nil {
		return WriteReport{}, commandFailed(
			"超级块已对上，但签名探针读不到 %s（%v）。核实之前不要重新提交", prep.targetID, err)
	}
	if want := signatureKindFor(prep.filesystem); sig.Kind != want {
		return WriteReport{}, commandFailed(
			"超级块显示已按请求格式化，但签名分类器看到的是 %s（期望 %s）。"+
				"两条读路径不一致，核实之前不要重新提交", sig, want)
	}
	if !nodeOK {
		return WriteReport{}, commandFailed(
			"%s 的文件系统已按请求写入，但 /dev 节点在核验窗口内没有到位。核实系统状态之后才能重新提交",
			prep.targetID)
	}

	return WriteReport{
		Observed: formatObservedState(prep, diskID, part, after, sig, settled, nodeOK),
		Data:     formatReportData(prep, part, prev, after, spec),
	}, nil
}

// requireNode 确认设备节点此刻在设备目录里。
//
// 节点是 mkfs 唯一的参数，也是“用户点的那台设备”与“内核里的这台设备”之间唯一的凭据：
// sysfs 里有分区不等于节点就绪（udev 可能还没处理事件）。
func (w *writer) requireNode(devID string) error {
	ok, err := w.nodes.Exists(devID)
	if err != nil {
		return commandFailed("无法确认 %s 的设备节点：%v", devID, err)
	}
	if !ok {
		return preflightStale(devID + " 的设备节点不存在（" + w.dev + " 下没有它）")
	}
	return nil
}

// formatCommand 返回这次格式化要执行的固定命令。
//
// 参数集合是**代码写死的清单**：只可能有 `-L <已校验的标签>`、`-f`（仅 XFS，实测必需）
// 与设备路径。不拼接任何用户选项，也不从请求里取选项。
func (w *writer) formatCommand(prep preparedWrite) (syscmd.Spec, error) {
	if err := ValidateLabel(prep.filesystem, prep.label); err != nil {
		// 判决层已经校验过；这里是第二道：标签是唯一一个进入 argv 的用户字符串。
		return syscmd.Spec{}, fmt.Errorf("%w: %v", ErrUnsafeWrite, err)
	}
	devPath := joinPath(w.dev, prep.targetID)
	switch prep.filesystem {
	case FilesystemExt4:
		args := make([]string, 0, 3)
		if prep.label != "" {
			args = append(args, "-L", prep.label)
		}
		return syscmd.Spec{Program: w.mkfsExt4, Args: append(args, devPath)}, nil
	case FilesystemXFS:
		// -f 是固定参数集合的一部分：不带它，mkfs.xfs 在任何用过（有文件系统签名）的
		// 分区上都以 rc=1 结束。它不是占用检查的旁路——预检与执行前的复核已经把
		// 挂载/swap/holders/打开者全判过，而实测 -f 也解不开 mkfs 自己的“已挂载”检查。
		args := []string{"-f"}
		if prep.label != "" {
			args = append(args, "-L", prep.label)
		}
		return syscmd.Spec{Program: w.mkfsXFS, Args: append(args, devPath)}, nil
	default:
		return syscmd.Spec{}, fmt.Errorf("%w: 文件系统 %q 没有格式化原语", ErrUnsafeWrite, prep.filesystem)
	}
}

// runFormatCommand 执行一次 mkfs。
func (w *writer) runFormatCommand(ctx context.Context, spec syscmd.Spec) (syscmd.Result, error) {
	return w.runner.Run(ctx, spec, syscmd.Limits{Timeout: FormatCommandTimeout})
}

// describeFormatFailure 把一次 mkfs 失败说清楚：退出码、输出摘要与**盘上现在是什么**。
//
// “盘上现在是什么”是这里最有用的一句话：mkfs.xfs 在太小的设备上失败时不会碰盘，
// 而别的失败可能已经写了一半。只说“命令失败”会把管理员留在未知状态前。
func (w *writer) describeFormatFailure(ctx context.Context, prep preparedWrite,
	part Partition, res syscmd.Result, runErr error) error {

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
	observed := "读回超级块也失败了"
	if w.superblocks != nil {
		if id, err := w.superblocks.Probe(context.WithoutCancel(ctx), prep.targetID, part.SizeBytes); err == nil {
			observed = "盘上现在是 " + id.String()
		}
	}
	program := "mkfs." + prep.filesystem
	if output == "" {
		return commandFailed("%s 格式化 %s：%s（%v）。%s。核实实际状态之前不要重新提交",
			program, prep.targetID, detail, runErr, observed)
	}
	return commandFailed("%s 格式化 %s：%s：%s。%s。核实实际状态之前不要重新提交",
		program, prep.targetID, detail, output, observed)
}

// waitForFormatNode 等 /dev 节点还原。
//
// 格式化不改变分区表，节点本来就在；这一步防的是“命令期间 udev 把节点重排了一遍”。
// 等不到不算立刻的失败——真正的判断在随后的读回核验里（nodeOK 进结果，界面能说清楚）。
func (w *writer) waitForFormatNode(ctx context.Context, devID string) bool {
	deadline := time.Now().Add(w.wait)
	for {
		if ok, err := w.nodes.Exists(devID); err == nil && ok {
			return true
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(w.poll):
		}
	}
}

// compareFormatIdentity 把读回来的文件系统身份与请求逐项对照。
//
// 返回空串表示全部对上；否则返回一句话说明第一处不一致。
//
// 四件事：种类、UUID 换过、标签、几何。
//   - **UUID 必须换过**：这是唯一能证明“mkfs 真的重建了文件系统”的读回证据。
//     只比种类与标签的话，一次“退出码 0 但什么都没写”的失败会被判成成功。
//   - 标签必须逐字节相等：实测 mkfs.ext4 对 17 字节标签只打一行 Warning 就截断到
//     16 字节并 **rc=0**，所以只信退出码就会把截断当成成功。
func compareFormatIdentity(want preparedWrite, prev, got FilesystemIdentity, partSize uint64) string {
	if got.Kind != want.filesystem {
		actual := got.Kind
		if actual == "" {
			actual = "认不出来的文件系统"
		}
		return "请求的是 " + want.filesystem + "，读回来的是 " + actual +
			noteDetail(got.Detail)
	}
	if got.UUID == "" {
		return "读回来的超级块里没有 UUID"
	}
	if prev.UUID != "" && got.UUID == prev.UUID {
		return "UUID 没有变化（仍是 " + got.UUID + "），文件系统没有被重建"
	}
	if got.Label != want.label {
		return "标签是 " + strconv.Quote(got.Label) + "，请求的是 " + strconv.Quote(want.label)
	}
	if got.Bytes == 0 {
		return "读回来的文件系统几何是 0 字节"
	}
	if got.Bytes > partSize {
		return fmt.Sprintf("文件系统声称 %d 字节，比分区的 %d 字节还大", got.Bytes, partSize)
	}
	if min := partSize / formatCoverDenominator * formatCoverNumerator; got.Bytes < min {
		return fmt.Sprintf("文件系统只覆盖 %d 字节，不到分区 %d 字节的 %d/%d",
			got.Bytes, partSize, formatCoverNumerator, formatCoverDenominator)
	}
	return ""
}

// noteDetail 把解析器的补充说明接在错误消息后面（没有就什么都不加）。
func noteDetail(detail string) string {
	if detail == "" {
		return ""
	}
	return "（" + detail + "）"
}

// signatureKindFor 把文件系统名映射到签名分类器的种类名。
func signatureKindFor(filesystem string) string {
	if filesystem == FilesystemXFS {
		return SignatureXFS
	}
	return SignatureExt
}

// formatObservedState 是核验通过时读回的实际状态（进 result.observed_state）。
func formatObservedState(prep preparedWrite, diskID string, part Partition,
	after FilesystemIdentity, sig Signature, settled, nodeOK bool) map[string]any {

	return map[string]any{
		"device_id":            prep.targetID,
		"disk_id":              diskID,
		"filesystem":           after.Kind,
		"uuid":                 after.UUID,
		"label":                after.Label,
		"size_bytes":           strconv.FormatUint(after.Bytes, 10),
		"partition_size_bytes": strconv.FormatUint(part.SizeBytes, 10),
		"block_size_bytes":     strconv.FormatUint(after.BlockSize, 10),
		"signature":            sig.Kind,
		"udev_settled":         settled,
		"node_present":         nodeOK,
	}
}

// formatReportData 组装 result.data：这次把哪台分区做成了什么。
//
// command 是**实际执行的 argv**（含程序路径）：审计要能回答“到底跑了哪条命令、
// 带了哪些参数”，而这些参数全部来自代码里的固定集合与已校验的标签。
func formatReportData(prep preparedWrite, part Partition, prev, after FilesystemIdentity,
	spec syscmd.Spec) map[string]any {

	argv := make([]string, 0, len(spec.Args)+1)
	argv = append(argv, spec.Program)
	argv = append(argv, spec.Args...)
	return map[string]any{
		"device_id":     prep.targetID,
		"filesystem":    after.Kind,
		"uuid":          after.UUID,
		"label":         after.Label,
		"size_bytes":    strconv.FormatUint(after.Bytes, 10),
		"checked_via":   []string{"superblock", "signature_probe"},
		"command":       argv,
		"previous_kind": prev.Kind,
		"previous_uuid": prev.UUID,
	}
}

// interruptedWrite 把“命令执行期间上下文结束”说成一次中断。
//
// 包的是上下文的错误（context.Canceled / context.DeadlineExceeded），因此 errors.Is
// 能认出它，runner 会把它记成 interrupted（实际状态未知），而不是 failed（那会让人
// 以为什么都没发生）或 succeeded（那是编造成绩）。
func interruptedWrite(ctxErr error, devID, program string) error {
	detail := "任务上下文已结束"
	if ctxErr != nil {
		detail = ctxErr.Error()
	}
	return fmt.Errorf("storage: %s 的 %s 执行到一半被中断（%s），实际状态未知；"+
		"先确认磁盘的实际状态，不要直接重新提交: %w", devID, program, detail, ctxErr)
}

// interruptedRun 把“上下文在动作过程中的任意一步结束”说成一次中断。
//
// 与 interruptedWrite 的差别只在措辞：那条确定命令已经跑起来了，这条不知道——
// 可能刚在读超级块，也可能正卡在 mkfs 里。两种情况都必须说成“实际状态未知”，
// 因为写磁盘的动作没有“安全取消”，也没法从进程里判断内核收到了多少字节。
func interruptedRun(ctxErr error, devID string) error {
	detail := "任务上下文已结束"
	if ctxErr != nil {
		detail = ctxErr.Error()
	}
	who := "本次写动作"
	if devID != "" {
		who = devID + " 的写动作"
	}
	return fmt.Errorf("storage: %s因任务上下文结束而中止（%s）：可能已经改写了设备，也可能还没动手，"+
		"实际状态未知；先确认设备的实际状态，不要直接重新提交: %w", who, detail, ctxErr)
}

// targetDeviceID 从参数里取出这次动作的目标设备（只用于错误消息）。
func targetDeviceID(params any) string {
	switch p := params.(type) {
	case FilesystemFormatParams:
		return p.DeviceID
	case PartitionCreateParams:
		return p.DeviceID
	case PartitionDeleteParams:
		return p.DeviceID
	case PartitionTableInitializeParams:
		return p.DeviceID
	}
	return ""
}
