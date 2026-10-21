// Package processes 采集主机进程列表（P05.2），并执行受约束的信号动作。
//
// 边界（PLAN.md 第 14 节、CONTRACTS.md 第 5、10 节）：
//
//   - 进程身份是 PID + starttime，不是 PID。PID 会被内核重用，而 starttime
//     （/proc/<pid>/stat 第 22 字段，开机以来的时钟滴答数）不会：同一个 pid
//     换了 starttime 就是另一个进程，旧预检因此杀不到新进程。
//   - 先 TERM，后 KILL。SIGKILL 需要单独的预检与确认短语，面板**不会**在 TERM
//     超时后自动升级：那等于把一个普通动作偷偷变成强制动作。
//   - 禁止终止 PID 1、内核线程、面板与代理自身、受保护系统服务（及其进程树）。
//     判定依据是 cgroup 里的单元名与既有服务保护名单，不按命令名猜。
//   - 采集只读 /proc；唯一的写操作是 syscall.Kill，参数由本包构造。
package processes

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 查询名与动作名（CONTRACTS.md 第 5、15 节：名字固定，未登记一律拒绝）。
const (
	// QueryListName 返回最近一次采集的进程快照。
	QueryListName = "processes.list"
	// ActionSignal 是唯一的进程写动作。
	ActionSignal = "process.signal"
)

// DefaultInterval 是进程快照的采集间隔。
//
// 5 秒：与 /proc 资源计数器同一节奏（PLAN.md 第 8 节）。进程列表比服务列表变得快，
// 但把它压到 1 秒只会让代理每秒扫一遍整个 /proc，而页面上的数字并不会因此更可信。
// 预检与执行都现场读取 /proc，不受这个间隔影响。
const DefaultInterval = 5 * time.Second

// MaxProcesses 是单次快照收录的进程上限。
//
// 超过上限时按 PID 排序截断并置 Truncated=true：一台 4000 进程的机器不该被渲染成
// 4000 行，而“只显示前 2000 个”必须明说，不能让人以为机器上只有这些。
const MaxProcesses = 2000

// 快照状态闭集（与 monitoring / storage / services 同一套语义）。
const (
	StatusOK    = "ok"
	StatusError = "error"
)

// 信号闭集（CONTRACTS.md 第 5 节：signal=TERM/KILL）。
const (
	SignalTERM = "TERM"
	SignalKILL = "KILL"
)

// Signals 按固定顺序返回全部信号，供校验与界面使用。
func Signals() []string { return []string{SignalTERM, SignalKILL} }

// ValidSignal 判断信号是否在闭集内。
func ValidSignal(signal string) bool {
	switch signal {
	case SignalTERM, SignalKILL:
		return true
	}
	return false
}

// PhraseVerbKill 是 SIGKILL 的确认短语首词（面板生成 “KILL AB12CD K7”，只出现一次）。
const PhraseVerbKill = "KILL"

// 进程状态闭集（/proc/<pid>/stat 的状态字母归一化后的名字）。
//
// 保留名字而不是字母：界面与审计里“zombie”比“Z”更难误读，而原始字母仍然出现在
// /proc 里，需要核对时到主机上看得见。
const (
	StateRunning   = "running"    // R
	StateSleeping  = "sleeping"   // S
	StateDiskSleep = "disk_sleep" // D
	StateZombie    = "zombie"     // Z
	StateStopped   = "stopped"    // T
	StateTracing   = "tracing"    // t
	StateDead      = "dead"       // X
	StateIdle      = "idle"       // I
	StatePaging    = "paging"     // W
	StateUnknown   = "unknown"    // 其他（内核新增状态时原样不猜）
)

// 保护类别闭集。
//
// self / ssh / systemd_core / network / other_module 与服务模块共用同一套词：
// 进程与单元的保护理由本来就是同一件事，编两套只会让人对不上号。
const (
	// ProtectInit：PID 1。杀掉它等于让整台机器失去初始化进程。
	ProtectInit = "init"
	// ProtectKernel：内核线程。没有用户空间可终止，杀它只会让内核报警。
	ProtectKernel = "kernel_thread"
	// ProtectSelf：面板与代理自身（含它自己的进程树入口）。
	ProtectSelf = "self"
	// ProtectSSH：远程登录。
	ProtectSSH = "ssh"
	// ProtectCore：systemd 核心组件（日志、会话、设备管理、总线）。
	ProtectCore = "systemd_core"
	// ProtectNetwork：网络基础服务。
	ProtectNetwork = "network"
	// ProtectOtherModule：由反代、共享、容器模块负责的守护进程。
	ProtectOtherModule = "other_module"
)

// 能力闭集（当前状态与保护规则下真正能发的信号，固定顺序）。
const (
	CapTERM = "TERM"
	CapKILL = "KILL"
)

// Process 是一个进程的对外视图（GET /processes 与预检共用同一份 DTO）。
type Process struct {
	// PID 是进程号。**它单独不足以标识一个进程**：内核会重用 PID。
	PID int64 `json:"pid"`
	// StartTimeTicks 是 /proc/<pid>/stat 第 22 字段（开机以来的时钟滴答数）。
	// PID + starttime 才是身份；预检与执行都绑定这两个值。
	StartTimeTicks uint64 `json:"start_time_ticks"`
	// PPID 是父进程号（内核线程的父进程是 kthreadd，PID 2）。
	PPID int64 `json:"ppid"`
	// Owner 是有效用户名的名字（来自 /etc/passwd）；查不到时给 "uid:<数字>"。
	Owner string `json:"owner"`
	// State 是归一化后的进程状态（闭集见上）。
	State string `json:"state"`
	// CPUPercent 是相对**单核**的 CPU 占用百分比（多线程进程可以超过 100）。
	// 第一次采集没有可比较的基线，因此为 null——不编一个 0.0。
	CPUPercent *float64 `json:"cpu_percent"`
	// MemoryBytes 是 VmRSS（十进制字符串，超过 2^53 的字节数不能走 JSON number）；
	// 内核线程与僵尸没有 VmRSS，为 null。
	MemoryBytes *string `json:"memory_bytes"`
	// Command 是**脱敏后**的命令摘要：argv[0] 取基名，敏感参数的值替换成 ***，
	// 单参数与整体都截断。它不是完整命令行，也不能拿来当审计全文。
	Command string `json:"command"`
	// Unit 是这个进程所属的 systemd 单元（cgroup 里的 .service 或 .scope）；
	// 空串表示不在任何单元里（内核线程、宿主上直接启动的进程）。
	Unit string `json:"unit"`
	// Protection 说明为什么这个进程不允许被终止。
	Protection Protection `json:"protection"`
	// Capabilities 是当前状态与保护规则下允许发送的信号（固定顺序 TERM、KILL）。
	// 受保护、已退出（zombie/dead）的进程一律为空数组。
	Capabilities []string `json:"capabilities"`
}

// Protection 是进程的保护判定（形状与服务单元一致，界面可直接复用组件）。
type Protection struct {
	Protected bool `json:"protected"`
	// Category 是保护类别（闭集）；未保护时为空。
	Category string `json:"category,omitempty"`
	// Reasons 是人类可读理由，去重且排序固定。
	Reasons []string `json:"reasons"`
}

// Snapshot 是一次采集的完整结果，也是 processes.list 的返回值。
//
// 没有 snapshot_version：进程快照每秒都在变，用一份“版本”去绑预检只会让每次
// 提交都因为别处开了一个进程而失效。绑定身份的是 PID + starttime（见 Identity）。
type Snapshot struct {
	// Status 是最近一次采集尝试的结果；失败时 Processes 保留上一次成功的内容。
	Status      string    `json:"status"`
	Error       string    `json:"error,omitempty"`
	CollectedAt time.Time `json:"collected_at"`
	IntervalMS  int64     `json:"interval_ms"`
	// Total 是这台机器上实际看到的进程数；Processes 被截断时两者不相同。
	Total int `json:"total"`
	// Truncated 为 true 表示进程数超过上限，列表被截断而不是“只有这些”。
	Truncated bool      `json:"truncated"`
	Processes []Process `json:"processes"`
}

// SignalParams 是 process.signal 的参数（CONTRACTS.md 第 5 节）。
//
// 没有 pid 之外的“名字匹配”、没有正则、没有批量目标、没有 force/now 之类会被翻译成
// kill 选项的开关：一个进程，一个信号。
type SignalParams struct {
	PID            int64  `json:"pid"`
	StartTimeTicks uint64 `json:"start_time_ticks"`
	Signal         string `json:"signal"`
}

// ErrInvalidParams 表示 process.signal 的参数不合法。
var ErrInvalidParams = fmt.Errorf("processes: 参数不合法")

// Validate 校验信号参数。非法输入不进代理、更不会进 syscall.Kill。
func ValidateParams(p SignalParams) error {
	if p.PID <= 0 || p.PID > 4194304 {
		return fmt.Errorf("%w: pid 必须是 1—4194304 之间的正整数，收到 %d", ErrInvalidParams, p.PID)
	}
	if p.StartTimeTicks == 0 {
		return fmt.Errorf("%w: start_time_ticks 必须大于 0（它是进程身份的一半，不能省略）", ErrInvalidParams)
	}
	if !ValidSignal(strings.ToUpper(strings.TrimSpace(p.Signal))) {
		return fmt.Errorf("%w: signal 只能是 TERM 或 KILL，收到 %q", ErrInvalidParams, p.Signal)
	}
	return nil
}

// Normalize 归一化参数（signal 大写、去空白）。
func Normalize(p SignalParams) SignalParams {
	return SignalParams{
		PID:            p.PID,
		StartTimeTicks: p.StartTimeTicks,
		Signal:         strings.ToUpper(strings.TrimSpace(p.Signal)),
	}
}

// SignalLock 是 process.signal 的资源锁：同一进程的并发操作不会叠加。
func SignalLock(pid int64, startTimeTicks uint64) string {
	return "process:" + strconv.FormatInt(pid, 10) + ":" + strconv.FormatUint(startTimeTicks, 10)
}

// Identity 返回进程身份指纹（PID + starttime）。
//
// 与 storage/services 同一套 sha256 前缀格式（64 位十六进制）。预检保存它，执行前
// 重算一遍：不一致说明原来那个进程已经不在了，绝不能拿旧指纹去杀新进程。
func Identity(pid int64, startTimeTicks uint64) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("process/v1|%d|%d", pid, startTimeTicks)))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// AbsentIdentity 是“这个 PID 上没有进程”的身份指纹。
//
// 它与任何现存进程的 Identity(pid, start) 都不相等：进程退出后重算预检得到的
// 就是它，于是旧预检在执行前复核时被明确判成“目标身份已变化”。
func AbsentIdentity(pid int64) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("process/absent/v1|%d", pid)))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// StateVersion 是单个进程的状态版本。
//
// 刻意**不含** CPU、内存与运行状态：这些每秒都在变，放进来会让每一份预检在几秒内
// 变成 PREFLIGHT_STALE。进程的不可变部分（PID + starttime）才是版本。
func StateVersion(pid int64, startTimeTicks uint64) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("process-state/v1|%d|%d", pid, startTimeTicks)))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ParsePID 解析一个 /proc 下的目录名；不是纯数字目录返回 ok=false。
func ParsePID(name string) (int64, bool) {
	if name == "" {
		return 0, false
	}
	for i := 0; i < len(name); i++ {
		if name[i] < '0' || name[i] > '9' {
			return 0, false
		}
	}
	pid, err := strconv.ParseInt(name, 10, 64)
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

// stateFromLetter 把 /proc/<pid>/stat 的状态字母转成闭集里的名字。
func stateFromLetter(letter byte) string {
	switch letter {
	case 'R':
		return StateRunning
	case 'S':
		return StateSleeping
	case 'D':
		return StateDiskSleep
	case 'Z':
		return StateZombie
	case 'T':
		return StateStopped
	case 't':
		return StateTracing
	case 'X', 'x':
		return StateDead
	case 'I':
		return StateIdle
	case 'W':
		return StatePaging
	}
	return StateUnknown
}

// exited 判断进程是否已经结束（僵尸或已死）：这两种状态没有可发送的信号，
// 杀一个僵尸也不会让它更快被回收——回收是它父进程的事。
func exited(state string) bool {
	return state == StateZombie || state == StateDead
}

// capabilitiesFor 计算当前状态与保护规则下允许发送的信号。
//
// 规则刻意保守：受保护与已退出的进程一律没有能力（界面据此不摆按钮，
// 而不是摆一个点了就报错的按钮）。
func capabilitiesFor(state string, protected bool) []string {
	if protected || exited(state) {
		return []string{}
	}
	return []string{CapTERM, CapKILL}
}

// dedupeSorted 去重并排序（理由是给人看的文本，顺序稳定才便于测试与阅读）。
func dedupeSorted(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
