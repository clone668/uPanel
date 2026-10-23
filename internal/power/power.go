package power

// 系统电源（P11.3）：重启与关机。CONTRACTS.md 第 5 节把它冻结成一个动作
// `system.power`，参数 `operation=reboot/poweroff` 与 `delay_seconds`（30—300）。
//
// 这一层只做四件事，每一件都对应一条契约要求：
//
//  1. **预检**：读得到 `boot_id` 吗？这台机器有 systemd 吗？有没有别的写操作正在跑？
//     三条里任何一条不成立就不许做——尤其是最后一条，正是 STAGES §13 的
//     “关键任务运行时拒绝电源操作”。
//  2. **倒计时**：延迟由**代理**持有，不是把 `systemctl reboot` 立刻发出去再让页面
//     倒数。这样“取消”是真的（倒计时期间调用 `systemctl stop` 什么也收不回来，
//     因为它还没发出去），任务状态也说得清：倒计时里任务还活着。
//  3. **下发**：`systemctl reboot` / `systemctl poweroff`，同步等它返回。
//  4. **如实收尾**：返回 `accepted`，不是 `verified`。**机器马上要消失的东西，
//     不能声称自己验证过**（PLAN.md 第 16 节：“不要把‘关机命令已受理’显示成
//     ‘服务器已成功关机’”）。真正确认发生在下次连上之后——用 `boot_id` 比。

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
)

// ActionPower 是冻结的动作名（CONTRACTS.md 第 5 节）。
const ActionPower = "system.power"

// Op 是电源操作的类型。
type Op string

const (
	OpReboot   Op = "reboot"
	OpPoweroff Op = "poweroff"
)

// Valid 判断操作是否已定义。
func (o Op) Valid() bool { return o == OpReboot || o == OpPoweroff }

// DisplayName 是给人看的名字。
func (o Op) DisplayName() string {
	switch o {
	case OpReboot:
		return "重启"
	case OpPoweroff:
		return "关机"
	}
	return string(o)
}

// SystemctlVerb 是这个操作交给 systemctl 的动词。
func (o Op) SystemctlVerb() string {
	switch o {
	case OpReboot:
		return "reboot"
	case OpPoweroff:
		return "poweroff"
	}
	return ""
}

const (
	// MinDelaySeconds / MaxDelaySeconds 是契约给的范围（CONTRACTS.md 第 5 节）。
	//
	// 下限 30 秒不是矫情：再短就没有“取消”的时间窗口，倒计时变成装饰。
	// 上限 300 秒是不让一次点击变成一个挂在那里半小时的悬案——真要约时间，
	// 那是 systemd 定时任务的事，不是这个按钮的事。
	MinDelaySeconds = 30
	MaxDelaySeconds = 300
)

// 阻塞/告警码。与 package.* 的码一样，是给界面分类用的稳定字符串。
const (
	// CodeJobsRunning 表示有别的写操作正在执行（STAGES §13：关键任务运行时拒绝）。
	CodeJobsRunning = "POWER_JOBS_RUNNING"
	// CodeUnsupported 表示这台机器不是 systemd 管起来的（容器、手工 chroot、非 systemd 发行版）。
	CodeUnsupported = "POWER_UNSUPPORTED"
	// CodeBootIDUnreadable 表示读不到 boot_id：没有它就没法确认重启真的发生过。
	CodeBootIDUnreadable = "POWER_BOOT_ID_UNREADABLE"
	// CodeJobsUnknown 表示查不到在跑的任务（依赖没接上）。**查不到不等于没有**，
	// 所以它也是阻塞项而不是告警。
	CodeJobsUnknown = "POWER_JOBS_UNKNOWN"
	// CodeIssueFailed 表示下发命令失败（systemctl 不在、权限被拒、systemd 没响应）。
	CodeIssueFailed = "POWER_ISSUE_FAILED"
	// CodeAbortedByJob 表示倒计时结束时发现有写任务开始跑，于是主动放弃。
	CodeAbortedByJob = "POWER_ABORTED_JOBS_STARTED"
	// CodeCanceled 表示倒计时期间被管理员取消。
	CodeCanceled = "POWER_CANCELED"
)

// 影响的类型（Impact.Kind 的取值，界面按它挑措辞）。
const (
	ImpactConnection = "connection_loss"
	ImpactPowerState = "power_state"
)

// 结果结论。只允许这两个：电源动作不许出现 verified（那要等到下次开机才可能）。
const (
	OutcomeAccepted = "accepted"
)

// DefaultPaths 是生产环境下的路径。
type Paths struct {
	Systemctl string
	BootID    string
	// SystemdRun 是 systemd 的运行时目录：它存在才说明这台机器真的由 systemd 管。
	// `/run/systemd/system` 在容器里通常不存在——那正是我们要分辨的情况。
	SystemdRun string
	// Uptime 是 /proc/uptime，只用来在界面上说“这次开机已经多久”。
	Uptime string
}

// DefaultPaths 返回 Ubuntu 上的固定位置。
func DefaultPaths() Paths {
	return Paths{
		Systemctl:  "/usr/bin/systemctl",
		BootID:     "/proc/sys/kernel/random/boot_id",
		SystemdRun: "/run/systemd/system",
		Uptime:     "/proc/uptime",
	}
}

// Params 是这个动作的参数（CONTRACTS.md 第 5 节冻结的字段名）。
type Params struct {
	Operation    Op  `json:"operation"`
	DelaySeconds int `json:"delay_seconds"`
}

// Delay 返回倒计时长度。
func (p Params) Delay() time.Duration { return time.Duration(p.DelaySeconds) * time.Second }

// AssessRequest 是一次预检的输入。
type AssessRequest struct {
	Params Params
	// ExcludeOperationID 是“把自己从在跑任务里排掉”的那个 id。
	//
	// 只有执行前复核会填它：那时候电源操作自己已经在执行器里跑着了，不排掉就会
	// 永远撞见“有写操作正在执行”，然后自己把自己拒掉——这曾是 P11.3 的真实故障：
	// 每个 system.power 任务都停在 revalidate，错误是 POWER_JOBS_RUNNING，
	// 而在它之前刚跑的预检却是 allowed。
	//
	// 预检阶段为空串：那时还没有操作 id，谁在跑都要算进来。
	ExcludeOperationID string
}

// Validate 校验参数。
//
// 刻意不接受“省略 delay_seconds 用默认值”：默认值会把“我以为它立刻重启”和
// “它其实 30 秒后才重启”混成同一个请求。要说清楚多少秒。
func (p Params) Validate() error {
	if !p.Operation.Valid() {
		return fmt.Errorf("operation 只能是 reboot 或 poweroff（收到 %q）", string(p.Operation))
	}
	if p.DelaySeconds < MinDelaySeconds || p.DelaySeconds > MaxDelaySeconds {
		return fmt.Errorf("delay_seconds 必须在 %d—%d 之间（收到 %d）秒",
			MinDelaySeconds, MaxDelaySeconds, p.DelaySeconds)
	}
	return nil
}

// InFlight 是“正在跑的一个写操作”。动作名进消息，账号进审计，两者都不猜。
type InFlight struct {
	OperationID string
	Action      string
}

// Facts 是预检要读的系统事实。
type Facts struct {
	// BootID 是本次启动的标识。空串表示读不到，原因在 BootIDError。
	BootID      string
	BootIDError string
	// Uptime 是本次开机至今；读不到时为零值，原因在 UptimeError。
	Uptime      time.Duration
	UptimeError string
	// Systemd 表示 `/run/systemd/system` 存在。
	Systemd bool
	// SystemctlPresent 表示 systemctl 可执行文件存在。
	SystemctlPresent bool
}

// InFlightSource 提供“现在有哪些写操作在跑”。
//
// 它由代理实现：判定“关键任务”的权力属于持有执行器的那一方，不属于这个包。
//
// 返回错误是有意义的：**查不到不等于没有**。装配漏了线、库里读失败，都必须让
// 预检拒绝，而不是让一次重启把这些“大概没在跑”的操作断在半路。
type InFlightSource interface {
	InFlight(excludeOperationID string) ([]InFlight, error)
}

// Assessment 是一次预检的结论。
type Assessment struct {
	Op        Op
	Params    Params
	Allowed   bool
	Blockers  []Reason
	Warnings  []Reason
	Impacts   []Impact
	Locks     []string
	Facts     Facts
	InFlight  []InFlight
	Target    Target
	CheckedAt time.Time
}

// Reason 是一条阻塞或告警。
type Reason struct {
	Code    string
	Message string
}

// Impact 是这次操作的后果。
type Impact struct {
	Kind        string
	Description string
}

// Target 与代理的 Plan.Target 同形：执行前用它再确认“还是这台机器”。
type Target struct {
	DisplayName         string
	IdentityFingerprint string
	StateVersion        string
}

// Executor 是电源动作的实现端。
type Executor struct {
	paths Paths
	// run 执行一条外部命令。默认是 syscmd.Run；测试换成假的。
	run func(ctx context.Context, program string, args []string, timeout time.Duration) (CommandResult, error)
	// now 与 sleep 是为了让倒计时在测试里不必真的等 30 秒。
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error
	// readFile 读 /proc 下的小文件；测试可以换。
	readFile func(path string) ([]byte, error)
	// stat 判断路径存在。
	stat func(path string) bool
	// inFlight 提供在跑的操作。为 nil 表示“查不到”——预检会因此拒绝。
	inFlight InFlightSource
}

// Options 是构造参数。
type Options struct {
	Paths    Paths
	Run      func(ctx context.Context, program string, args []string, timeout time.Duration) (CommandResult, error)
	Now      func() time.Time
	Sleep    func(ctx context.Context, d time.Duration) error
	ReadFile func(path string) ([]byte, error)
	Stat     func(path string) bool
	InFlight InFlightSource
}

// New 构造执行器。缺省值只在生产路径上使用；测试要什么就传什么。
func New(opts Options) *Executor {
	e := &Executor{
		paths:    opts.Paths,
		run:      opts.Run,
		now:      opts.Now,
		sleep:    opts.Sleep,
		readFile: opts.ReadFile,
		stat:     opts.Stat,
		inFlight: opts.InFlight,
	}
	if e.paths.Systemctl == "" || e.paths.BootID == "" || e.paths.SystemdRun == "" {
		def := DefaultPaths()
		if e.paths.Systemctl == "" {
			e.paths.Systemctl = def.Systemctl
		}
		if e.paths.BootID == "" {
			e.paths.BootID = def.BootID
		}
		if e.paths.SystemdRun == "" {
			e.paths.SystemdRun = def.SystemdRun
		}
		if e.paths.Uptime == "" {
			e.paths.Uptime = def.Uptime
		}
	}
	if e.now == nil {
		e.now = time.Now
	}
	if e.sleep == nil {
		// 默认的等待必须是**可打断**的：ctx 一结束（取消、代理停机、操作超时）
		// 就立刻返回，不能把一个 300 秒的 t.C 留在原地。
		e.sleep = func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		}
	}
	if e.readFile == nil {
		e.readFile = os.ReadFile
	}
	if e.stat == nil {
		e.stat = func(path string) bool {
			_, err := os.Stat(path)
			return err == nil
		}
	}
	return e
}

// SetInFlight 在代理构造之后补上“在跑的操作”来源。
//
// 为什么是事后绑定：动作登记表在 `agent.New` **之前**就建好了（查询与动作都挂在上头），
// 而“谁在跑”只有 Server 知道。与其让登记表反过来依赖 Server（循环），不如让这一格
// 在 Server 起来之后填。填之前它是 nil，预检会以 POWER_JOBS_UNKNOWN 拒绝——
// “查不到”绝不等于“没有任务在跑”。
func (e *Executor) SetInFlight(src InFlightSource) { e.inFlight = src }

// Paths 返回实际使用的路径（诊断用）。
func (e *Executor) Paths() Paths { return e.paths }

// bootIDShape 与 P11.1 的 apt.ParseBootID 是同一条规则，这里再写一遍是**故意**的：
// 那一份是为了把 boot_id 展示给页面，这一份是为了比较两次启动。
// 截断的值如果被原样收下，重启前后的比对会在两个同样截断的值上判等——
// 那等于没比对，而且看起来像比对过了。
var bootIDShape = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ReadBootID 读并校验 boot_id。
func (e *Executor) ReadBootID() (string, error) {
	data, err := e.readFile(e.paths.BootID)
	if err != nil {
		return "", fmt.Errorf("读不到 %s：%w", e.paths.BootID, err)
	}
	id := strings.ToLower(strings.TrimSpace(string(data)))
	if id == "" {
		return "", fmt.Errorf("%s 是空的", e.paths.BootID)
	}
	if !bootIDShape.MatchString(id) {
		return "", fmt.Errorf("%s 内容不像 boot_id（%d 字节）", e.paths.BootID, len(id))
	}
	return id, nil
}

// Facts 读一遍系统事实。读取失败不是错误，而是事实的一部分：
// “读不到”要原样告诉管理员，而不是折成零值。
func (e *Executor) Facts() Facts {
	var f Facts
	if id, err := e.ReadBootID(); err != nil {
		f.BootIDError = err.Error()
	} else {
		f.BootID = id
	}
	if data, err := e.readFile(e.paths.Uptime); err != nil {
		f.UptimeError = fmt.Sprintf("读不到 %s：%v", e.paths.Uptime, err)
	} else if d, err := parseUptime(data); err != nil {
		f.UptimeError = err.Error()
	} else {
		f.Uptime = d
	}
	f.Systemd = e.stat(e.paths.SystemdRun)
	f.SystemctlPresent = e.stat(e.paths.Systemctl)
	return f
}

// parseUptime 解析 /proc/uptime 的第一个字段（秒，带两位小数）。
func parseUptime(data []byte) (time.Duration, error) {
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0, fmt.Errorf("%s 是空的", "/proc/uptime")
	}
	var secs float64
	if _, err := fmt.Sscanf(fields[0], "%f", &secs); err != nil {
		return 0, fmt.Errorf("%s 的第一个字段不是数字：%q", "/proc/uptime", fields[0])
	}
	if secs < 0 {
		return 0, fmt.Errorf("%s 是负数：%q", "/proc/uptime", fields[0])
	}
	return time.Duration(secs * float64(time.Second)), nil
}
