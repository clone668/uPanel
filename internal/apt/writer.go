package apt

// APT 写事务（P11.2）：刷新索引、安装、卸载、升级。
//
// 这是整个 P11 里唯一会改变系统状态的地方，所以它的形状与只读采集器完全不同：
//
//  1. **先模拟，再执行**。预检阶段跑的是 `apt-get -s`——apt 自己算一遍，把要装什么、
//     要删什么、哪些被保留、哪些依赖不满足原样交出来。面板不去复刻依赖解析器。
//  2. **模拟的结论就是验收标准**。`-s` 的 `Inst foo [1.0] (2.0 ...)` 里写明了目标版本，
//     执行完之后的验证是"装上的版本是不是那一版"，不是"命令退出码是不是 0"。
//  3. **锁只探测、从不删除**。外部 apt 正在跑时，预检直接拒绝（APT_BUSY），
//     执行前再复核一次；两个 apt 同时写 dpkg 数据库会把机器弄成需要人工救援的状态。
//  4. **非交互**。所有写命令都带 `DEBIAN_FRONTEND=noninteractive` 与
//     `Dpkg::Options=--force-confdef/--force-confold`：没有终端可问，配置文件的处置
//     必须是事先说好的（保留本机版本、只处理没改过的）。
//  5. **失败就说失败**。apt 的 `E:` 行与 stderr 尾部原样进任务错误，不翻译成
//     "操作失败"，也不假装重试过。APT 中断之后不自动 `dpkg --configure -a`：
//     自动修复会把"apt 拒绝的原因"抹掉，下一次更难查。

import (
	"errors"
	"os"
	"strings"
	"sync"
	"time"

	"upanel/internal/syscmd"
)

// Op 是 APT 写事务的四种操作（CONTRACTS.md 第 5 节的四个动作）。
type Op string

const (
	OpRefresh Op = "refresh"
	OpInstall Op = "install"
	OpUpgrade Op = "upgrade"
	OpRemove  Op = "remove"
)

// ValidOp 报告操作名是否合法。
func ValidOp(op Op) bool {
	switch op {
	case OpRefresh, OpInstall, OpUpgrade, OpRemove:
		return true
	}
	return false
}

// 命名的动作名（与 CONTRACTS.md 第 5 节一致）。
const (
	ActionRefresh = "package.refresh"
	ActionInstall = "package.install"
	ActionRemove  = "package.remove"
	ActionUpgrade = "package.upgrade"
)

// OpForAction 把动作名映射成操作；未登记的动作返回空串。
func OpForAction(action string) Op {
	switch action {
	case ActionRefresh:
		return OpRefresh
	case ActionInstall:
		return OpInstall
	case ActionRemove:
		return OpRemove
	case ActionUpgrade:
		return OpUpgrade
	}
	return ""
}

// 阻塞项代码。
const (
	// CodeAptBusy：有别的 apt/dpkg 正在运行（锁被持有）。
	CodeAptBusy = "APT_BUSY"
	// CodeProtected：模拟结果里会出现受保护包的删除。
	CodeProtected = "PACKAGE_PROTECTED"
	// CodeUnmet：apt 自己说这次操作不成立（依赖冲突、找不到包、需要删除 Essential）。
	CodeUnmet = "PACKAGE_UNMET"
	// CodeNotInstalled：要升级/卸载的包没装（升级一个没装的包是空操作，卸载一个
	// 没装的包是打错字——两者都不该静默成功）。
	CodeNotInstalled = "PACKAGE_NOT_INSTALLED"
	// CodeLockUnreadable：锁文件存在但探测不了（权限/文件系统不支持 flock）。
	// 它不是 busy，也不能当成"没人占用"——两条都不成立时，说出来。
	CodeLockUnreadable = "APT_LOCK_UNREADABLE"
)

// 告警项代码。
const (
	// WarningCollateral：这次操作会删掉调用方没点名的包。
	WarningCollateral = "collateral_removal"
	// WarningNoChange：模拟显示这次操作不会改变任何东西。
	WarningNoChange = "no_change"
	// WarningKeptBack：升级时有些包被 apt 保留。
	WarningKeptBack = "kept_back"
	// WarningConfigKept：本机的配置文件会被保留（conffile 策略）。
	WarningConfigKept = "conffile_kept"
	// WarningReboot：这次操作之后可能需要重启（由 /run/reboot-required 决定，不猜）。
	WarningReboot = "reboot_flagged"
	// WarningIndexAge：索引超过一天没刷新。
	WarningIndexAge = "index_stale"
)

// 影响类别（与 CONTRACTS.md 4.1 的 impacts.kind 对齐）。
const (
	ImpactSoftware = "software_change"
	ImpactRemoval  = "software_removal"
	ImpactNone     = "none"
)

// Reason 是阻塞项/告警项（agent 层转换用，与 services.Reason 同形）。
type Reason struct {
	Code    string
	Message string
}

// Impact 是一次操作的影响。
type Impact struct {
	Kind        string
	Description string
}

// 命令限额。
//
// 检索与模拟都是秒级；真正的写事务可能几分钟（下载 + 解包 + postinst）。
// 20 分钟是给"慢镜像 + 慢磁盘"的余量，不是常态；超时会被杀进程组。
const (
	simulateTimeout = 5 * time.Minute
	writeTimeout    = 20 * time.Minute
	writeMaxStdout  = 2 << 20
	writeMaxStderr  = 512 << 10
	dpkgStateFresh  = 5 * time.Second
)

// aptGetNonInteractive 是所有写命令共用的非交互参数。
//
//   - `-y`：不问；`--force-confdef/--force-confold`：配置文件能不动就不动，新版本
//     只落在 `.dpkg-dist` 里；`Dpkg::Use-Pty=0`：不给 dpkg 分配伪终端，否则它的
//     进度条会往 stdout 里灌一堆控制字符。
//
// 这些参数是**代码里的固定值**：不接受来自请求的参数，也不读配置文件。
func aptGetNonInteractive() []string {
	return []string{
		"-y", "-q",
		"-o", "Dpkg::Options::=--force-confdef",
		"-o", "Dpkg::Options::=--force-confold",
		"-o", "Dpkg::Use-Pty=0",
	}
}

// aptNonInteractiveEnv 是配套的环境变量（debconf 前端）。
//
// NEEDRESTART_MODE=l：Ubuntu 的 needrestart 在 apt 事务里会问"哪些服务要重启"。
// 选 `l`（list）而不是 `a`（auto）：面板不想在一次装包之后**悄悄重启**一堆服务，
// 需要重启这件事由 `/run/reboot-required` 与后续的电源模块如实报告。
func aptNonInteractiveEnv() []string {
	return []string{
		"DEBIAN_FRONTEND=noninteractive",
		"APT_LISTCHANGES_FRONTEND=none",
		"NEEDRESTART_MODE=l",
	}
}

// WriterOptions 是 Writer 的构造参数。零值可用。
//
// 与只读采集器的 Options 分开：采集器还有 TTL 与 Read，写事务没有那些概念，
// 硬塞进同一个结构会让"这个字段对这条路径有意义吗"变成一个要查代码的问题。
type WriterOptions struct {
	Paths Paths
	Run   Runner
	Now   func() time.Time
	// Stat 读索引目录的 mtime（refresh 的验证用）。默认走 os.Stat。
	Stat StatFile
	// Locks 探测锁文件。默认走 ProbeLocks（真 flock）。
	//
	// 可替换只为单测："真的有另一个 apt 在跑"这个条件在测试机上造不出来，
	// 而它在 Windows 开发宿主上又是唯一不支持的一项。换掉探测函数不影响
	// 生产路径的任何判断——预检与执行都从这份结论往下走。
	Locks func(paths []string) []LockState
}

// Writer 执行 APT 写事务。
type Writer struct {
	paths Paths
	run   Runner
	now   func() time.Time
	stat  StatFile
	locks func([]string) []LockState

	mu    sync.Mutex
	state *dpkgState
}

// dpkgState 是一次 dpkg 数据库的读快照（状态 + 版本 + Essential 位）。
type dpkgState struct {
	at        time.Time
	status    map[string]string
	version   map[string]string
	essential map[string]bool
}

// NewWriter 构造 Writer。
func NewWriter(opts WriterOptions) *Writer {
	paths := opts.Paths
	if strings.TrimSpace(paths.DpkgQuery) == "" {
		paths = DefaultPaths()
	}
	run := opts.Run
	if run == nil {
		run = syscmd.Run
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	stat := opts.Stat
	if stat == nil {
		stat = os.Stat
	}
	locks := opts.Locks
	if locks == nil {
		locks = ProbeLocks
	}
	return &Writer{paths: paths, run: run, now: now, stat: stat, locks: locks}
}

// Paths 返回 Writer 使用的路径（测试与诊断）。
func (w *Writer) Paths() Paths { return w.paths }

// Available 报告这台机器上能不能做 APT 写事务：apt-get 与 dpkg-query 都是可执行文件。
//
// 两个都要：只有 apt-get 而没有 dpkg-query 时，写事务跑得起来却**核对不了结果**，
// 那正是本阶段明确拒绝的"命令成功就算成功"。
func (w *Writer) Available() bool {
	for _, path := range []string{w.paths.AptGet, w.paths.DpkgQuery} {
		if strings.TrimSpace(path) == "" {
			return false
		}
		info, err := w.stat(path)
		if err != nil || info.IsDir() {
			return false
		}
	}
	return true
}

// Assessment 是一次写事务的预检结论。
type Assessment struct {
	Op Op
	// DisplayName 是给管理员看的目标描述。
	DisplayName string
	// Identity 是这次操作的身份指纹（操作 + 包名集合）。
	Identity string
	// StateVersion 是目标当前状态的版本（索引时间 + 这些包当前版本）。
	StateVersion string
	Allowed      bool
	Blockers     []Reason
	Warnings     []Reason
	Impacts      []Impact
	Locks        []string
	// Simulation 是 `apt-get -s` 的结论（refresh 时为零值）。
	Simulation Simulation
	// Protected 是模拟里将被删除的受保护包（非空即阻止）。
	Protected []string
	// Collateral 是模拟里将被删除、但调用方没点名的包。
	Collateral []string
	// BusyLocks 是探测到的被占用的锁。
	BusyLocks []LockState
	// IndexUpdatedAt 是当前索引时间（读不到为 nil）。
	IndexUpdatedAt *time.Time
}

// ExecResult 是一次写事务的执行结果。
type ExecResult struct {
	Op       Op
	Names    []string
	Stdout   string
	Stderr   string
	ExitCode int
	Duration time.Duration
	// Plan 是执行前那一份模拟（用于核对）。
	Plan Simulation
	// Observed 是执行后从 dpkg 数据库读到的真实状态（只含相关包）。
	Observed map[string]ObservedPackage
	// Mismatches 是"计划说要变、实际没变"或"变错版本"的包。
	Mismatches []string
	// Verified 表示每一条计划步骤都在读回的事实里对上了。
	Verified bool
	// VerifiedAt 是完成核对的时间。
	VerifiedAt time.Time
	// IndexUpdatedAt 是执行后的索引时间。
	IndexUpdatedAt *time.Time
	// IndexBefore 是执行前的索引时间。
	IndexBefore *time.Time
	// RefreshErrors 是 `apt-get update` 输出里的错误行。
	RefreshErrors []string
	// RefreshWarnings 是同一份输出里的告警行。
	RefreshWarnings []string
}

// ObservedPackage 是一个包在执行后的真实状态。
type ObservedPackage struct {
	// Status 是 dpkg 的状态缩写（`ii`、`rc`、空串表示没装）。
	Status string `json:"status"`
	// Version 是已安装版本。
	Version string `json:"version"`
}

// Installed 表示这个包现在是装着的（`ii`，不是只剩配置的 `rc`）。
func (o ObservedPackage) Installed() bool { return strings.HasPrefix(o.Status, "ii") }

// ErrBusy 表示执行时发现锁被别的进程持有。
var ErrBusy = errors.New("apt: 有别的 apt/dpkg 正在运行")

// ErrOpUnsupported 表示操作名不合法。
var ErrOpUnsupported = errors.New("apt: 不支持的写操作")
