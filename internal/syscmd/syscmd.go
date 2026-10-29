// Package syscmd 执行外部系统命令，并强制 PLAN.md 第 4 节规定的执行边界：
//
//   - 只按绝对路径调用可执行文件，逐项传参，绝不经过 sh -c / bash -c；
//   - 环境变量使用固定白名单，不继承调用者环境（LD_PRELOAD、LD_LIBRARY_PATH、
//     BASH_ENV、IFS 等用户可控的加载器/解释器变量一律不进入子进程）；
//   - 标准输入默认是空的（唯一例外是代码里的固定模板，见 Spec.Stdin），
//     输出有硬上限，超时后连同子进程组一起终止；
//   - 非零退出状态是**结果**而不是“命令失败”的推脱理由：调用方拿到退出码与输出后
//     自行判断，不得只看 Ready/HTTP 200 就宣称成功（PLAN.md 第 6 节）。
//
// 本包不提供“把字符串当命令执行”的入口，也不接受 HTTP 参数作为程序路径：
// 程序路径只来自代理登记的动作定义。
package syscmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// DefaultPath 是子进程看到的固定 PATH（不含用户 home 下的目录）。
const DefaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// 默认限额。CONTRACTS.md 第 4 节的预检/任务期限是 120 秒，这里取同一数量级作为兜底。
const (
	DefaultTimeout   = 120 * time.Second
	DefaultMaxStdout = 1 << 20   // 1 MiB
	DefaultMaxStderr = 256 << 10 // 256 KiB
	// killWaitDelay 是终止后等待管道关闭的上限，避免孙进程占着管道导致 Wait 永久阻塞。
	killWaitDelay = 2 * time.Second
	// MaxStdinBytes 是一次调用能拿到的标准输入上限。
	//
	// 固定模板只有几百字节（sfdisk 的一行脚本最长的也就一百多字节），64 KiB 是
	// 给"将来某个模板变长"留的余量，不是给内容传输留的口子：内容传输走文件的暂存通道，
	// 不走标准输入。
	MaxStdinBytes = 64 << 10
)

// 环境白名单。只给子进程三件事：固定的 PATH、固定的 UTF-8 区域设置，别无其他。
var fixedEnv = []string{
	"PATH=" + DefaultPath,
	"LANG=C.UTF-8",
	"LC_ALL=C.UTF-8",
}

// fixedEnvKeys 是 fixedEnv 里那些键的集合：调用方不得覆盖。
var fixedEnvKeys = func() map[string]bool {
	out := make(map[string]bool, len(fixedEnv))
	for _, kv := range fixedEnv {
		key, _, _ := strings.Cut(kv, "=")
		out[key] = true
	}
	return out
}()

// allowedEnvKeys 是调用方可以通过 Spec.Env **追加**的环境变量白名单（P11.2）。
//
// 为什么需要这个口子：APT 的非交互策略有一半只能在环境里说。`Dpkg::Options` 能挡
// conffile 提问，但 debconf 用哪个前端是 `DEBIAN_FRONTEND` 决定的——没有它，一次
// postinst 提问会让 apt 挂在一个不存在的终端上，直到超时被杀。这不是“执行参数”，
// 而是“请别问”，所以走环境。
//
// 为什么是白名单而不是 map：环境变量是提权路径的经典入口（`LD_PRELOAD`、
// `PATH`、`BASH_ENV`…）。这里只接受下面这几个键，值还必须在 allowedEnvValues 里。
// 任何其他键直接拒绝，不做“忽略掉继续跑”——静默丢掉一个调用方以为生效的策略，
// 比报错更危险。
//
// `TZ` 是 P12.4 加的，只有一个取值 `UTC`：`systemd-analyze calendar` 用**进程时区**
// 打印 `Next elapse`，而它在进程时区就是 UTC 时会**省掉** `(in UTC)` 那一行。
// 时区随宿主机而变，输出形状就跟着变——同一个表达式在 UTC 机器上读不出下次触发
// 时刻（P12.4 的 24.04 验收现场：`SCHEDULE_CALENDAR_INVALID`，而表达式完全合法）。
// 把时区钉死成 UTC，输出形状与机器无关，`Next elapse` 那一行本身就是 UTC。
// 它不是执行参数，不能加载任何东西，取值也不是调用方给的。
var allowedEnvKeys = map[string]bool{
	"DEBIAN_FRONTEND":          true,
	"APT_LISTCHANGES_FRONTEND": true,
	"NEEDRESTART_MODE":         true,
	"TZ":                       true,
}

// allowedEnvValues 是上面那些键的固定取值。
var allowedEnvValues = map[string]map[string]bool{
	"TZ":                       {"UTC": true},
	"DEBIAN_FRONTEND":          {"noninteractive": true},
	"APT_LISTCHANGES_FRONTEND": {"none": true},
	"NEEDRESTART_MODE":         {"a": true, "l": true, "n": true},
}

// ErrEnvNotAllowed 表示 Spec.Env 里出现了白名单之外的键或取值。
var ErrEnvNotAllowed = errors.New("syscmd: 环境变量不在允许列表内")

var (
	// ErrEmptyProgram 表示没有给出程序路径。
	ErrEmptyProgram = errors.New("syscmd: 程序路径不能为空")
	// ErrProgramNotAbsolute 表示程序路径不是绝对路径。
	ErrProgramNotAbsolute = errors.New("syscmd: 程序路径必须是绝对路径")
	// ErrProgramInvalid 表示程序路径含有不可能出现在可执行文件名里的字符
	// （空格、引号、分号、管道等 shell 元字符），一律拒绝，避免“看起来像绝对路径
	// 其实是一段命令”的输入被送进 exec。
	ErrProgramInvalid = errors.New("syscmd: 程序路径含有非法字符")
	// ErrTimeout 表示命令超出运行时限并被终止。
	ErrTimeout = errors.New("syscmd: 命令超出运行时限")
	// ErrOutputLimit 表示输出超过上限（输出被截断，命令本身可能成功）。
	ErrOutputLimit = errors.New("syscmd: 命令输出超过上限，已截断")
	// ErrStdinTooLarge 表示标准输入超过 MaxStdinBytes。
	ErrStdinTooLarge = errors.New("syscmd: 标准输入超过上限")
)

// Spec 描述一次调用。Args 逐项传给可执行文件，不做任何 shell 解释。
type Spec struct {
	// Program 必须是绝对路径，且不来自 HTTP 参数（PLAN.md 第 4 节）。
	Program string
	// Args 是逐项参数，可以为空。
	Args []string
	// Dir 是可选的绝对工作目录；为空时使用进程当前目录。
	Dir string
	// Stdin 是可选的标准输入字节（**固定脚本**，不是命令）。
	//
	// 它只为一件事存在：P07.2 的 sfdisk 分区表模板。sfdisk 的脚本只能从 stdin
	// 读（util-linux 2.39.3 的用法里没有"脚本文件"参数），而本包不允许 sh -c
	// "echo … | sfdisk"——那会把整个执行边界交给一个 shell。于是这里开一个有上限的
	// 字节通道，而不是开一个命令通道。
	//
	// 约束（与 Program 同级）：只能由代码里的固定模板填充，**不得来自 HTTP 参数**；
	// 超过 MaxStdinBytes 直接拒绝；为空时子进程的 stdin 仍是空的（不是父进程的 stdin）。
	Stdin []byte
	// Env 是要追加给子进程的环境变量，形如 `KEY=VALUE`。
	//
	// 只接受 allowedEnvKeys 里的键与 allowedEnvValues 里的值（P11.2 的 APT 非交互策略）。
	// 它**不覆盖** fixedEnv 里的 PATH/LANG/LC_ALL：同名键会被拒绝，而不是悄悄替换——
	// 能改 PATH 就等于能让程序以另一个身份被找到。
	Env []string
	// Credential 让子进程以另一个身份运行（P12.4 的 script.run）。
	//
	// 它是**降权**通道，不是提权通道：UID 为 0 会被直接拒绝。受管理脚本以
	// upanel-shell 运行，永远拿不到 root（CONTRACTS §5 script.run 那一行）。
	//
	// 不用 `sudo -u` 或 `setpriv` 完成这件事：那会把"以谁的身份运行"交给另一个
	// 程序的参数解析，而这里只需要内核的 setuid 语义。降权发生在 fork 之后、
	// exec 之前，子进程从第一条指令起就已经是目标身份。
	Credential *Credential
}

// Credential 是子进程的运行身份。
type Credential struct {
	UID uint32
	GID uint32
	// Groups 是要设置的附加组；为空时只设主组（不从父进程继承）。
	Groups []uint32
}

// ErrRootCredential 表示试图用这个通道以 root 身份运行。
var ErrRootCredential = errors.New("syscmd: 降权通道不接受 UID 0")

// validateEnv 校验调用方追加的环境变量。
func validateEnv(env []string) error {
	for _, kv := range env {
		key, value, ok := strings.Cut(kv, "=")
		if !ok || key == "" {
			return fmt.Errorf("%w（%q 不是 KEY=VALUE）", ErrEnvNotAllowed, kv)
		}
		if fixedEnvKeys[key] {
			// 覆盖 PATH 就等于让程序以另一个身份被找到；覆盖 LANG/LC_ALL 会让
			// dpkg 的输出变成另一种编码，解析器读到的就不是它以为读到的东西。
			return fmt.Errorf("%w（%q 属于固定环境，不许覆盖）", ErrEnvNotAllowed, key)
		}
		if !allowedEnvKeys[key] {
			return fmt.Errorf("%w（键 %q 不在白名单内）", ErrEnvNotAllowed, key)
		}
		values, ok := allowedEnvValues[key]
		if !ok {
			return fmt.Errorf("%w（键 %q 没有允许的取值集合）", ErrEnvNotAllowed, key)
		}
		if !values[value] {
			return fmt.Errorf("%w（%s=%q 不在允许取值内）", ErrEnvNotAllowed, key, value)
		}
	}
	return nil
}

// Limits 是本次调用的限额；零值字段取默认值。
type Limits struct {
	Timeout   time.Duration
	MaxStdout int64
	MaxStderr int64
}

func (l Limits) withDefaults() Limits {
	if l.Timeout <= 0 {
		l.Timeout = DefaultTimeout
	}
	if l.MaxStdout <= 0 {
		l.MaxStdout = DefaultMaxStdout
	}
	if l.MaxStderr <= 0 {
		l.MaxStderr = DefaultMaxStderr
	}
	return l
}

// Result 是一次调用的完整结果。即使返回错误，Result 也会尽量填好。
type Result struct {
	Program  string
	ExitCode int
	// Signal 是终止子进程的信号名（如 "SIGKILL"）；正常退出时为空。
	//
	// 为什么退出码不够：被信号杀死的进程 ExitCode 是 -1，"退出码 -1" 写给用户
	// 等于什么都没说——它既不是 apt 自己的判断，也没说谁停的。信号名是运行时
	// 就摆在 ProcessState 里的事实，传出去比让上层猜要诚实。
	//
	// 注意：本包自己超时/取消时也走 SIGKILL 终止进程组，所以那两种情况 Signal
	// 同样是 "SIGKILL"。区分"谁停的"看 TimedOut 与返回的错误，不看这里。
	Signal          string
	Stdout          string
	Stderr          string
	StdoutTruncated bool
	StderrTruncated bool
	TimedOut        bool
	Duration        time.Duration
}

// Truncated 报告是否有任一输出流被截断。
func (r Result) Truncated() bool { return r.StdoutTruncated || r.StderrTruncated }

// ExitError 表示命令已正常启动但以非零状态退出。
// 调用方必须根据动作语义解释退出码，不能把它当成“执行本身出错”。
type ExitError struct {
	Program  string
	ExitCode int
}

func (e *ExitError) Error() string {
	return fmt.Sprintf("syscmd: %s 以退出状态 %d 结束", e.Program, e.ExitCode)
}

// Run 执行一次命令。
//
// 返回的 error 可能是 ErrTimeout（命令被终止）、*ExitError（非零退出）、
// 或者是启动失败的错误。无论哪种情况，Result 都已经填好可用的字段。
func Run(ctx context.Context, spec Spec, limits Limits) (Result, error) {
	res := Result{Program: spec.Program, ExitCode: -1}

	if spec.Program == "" {
		return res, ErrEmptyProgram
	}
	if err := validateProgram(spec.Program); err != nil {
		return res, err
	}
	if len(spec.Stdin) > MaxStdinBytes {
		return res, fmt.Errorf("%w：%d 字节，上限 %d", ErrStdinTooLarge, len(spec.Stdin), MaxStdinBytes)
	}
	if err := validateEnv(spec.Env); err != nil {
		return res, err
	}
	if spec.Credential != nil && spec.Credential.UID == 0 {
		return res, ErrRootCredential
	}
	limits = limits.withDefaults()

	runCtx, cancel := context.WithTimeout(ctx, limits.Timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, spec.Program, spec.Args...)
	cmd.Env = fixedEnv
	if len(spec.Env) > 0 {
		cmd.Env = append(append([]string{}, fixedEnv...), spec.Env...)
	}
	cmd.Dir = spec.Dir
	// 标准输入默认保持为空：不把父进程（可能是 Web 或交互终端）的输入交给子进程。
	// 给了固定模板才接一个只读的内存流——它不是"把 stdin 透传下去"。
	cmd.Stdin = nil
	if len(spec.Stdin) > 0 {
		cmd.Stdin = bytes.NewReader(spec.Stdin)
	}

	stdout := &cappedBuffer{limit: limits.MaxStdout}
	stderr := &cappedBuffer{limit: limits.MaxStderr}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	// 终止时杀掉整个进程组，避免子进程派生的孙进程继续运行。
	// 降权（P12.4）与进程组在同一处设置：两者都只能写一次 SysProcAttr。
	if err := setProcAttr(cmd, spec.Credential); err != nil {
		return res, err
	}
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	cmd.WaitDelay = killWaitDelay

	start := time.Now()
	err := cmd.Run()
	res.Duration = time.Since(start)
	res.Stdout = stdout.String()
	res.Stderr = stderr.String()
	res.StdoutTruncated = stdout.truncated
	res.StderrTruncated = stderr.truncated

	var exitErr *exec.ExitError
	exited := errors.As(err, &exitErr)
	if exited {
		res.ExitCode = exitErr.ExitCode()
	}
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
		res.Signal = signalName(cmd.ProcessState)
	}

	switch {
	case errors.Is(runCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil:
		res.TimedOut = true
		res.ExitCode = -1
		return res, fmt.Errorf("%w（%s，上限 %s）", ErrTimeout, spec.Program, limits.Timeout)
	case errors.Is(runCtx.Err(), context.Canceled):
		return res, fmt.Errorf("syscmd: 命令被取消（%s）: %w", spec.Program, runCtx.Err())
	case exited:
		// 负数表示子进程被信号终止；调用方从 Result 里能看到这个事实。
		return res, &ExitError{Program: spec.Program, ExitCode: res.ExitCode}
	case err != nil:
		return res, fmt.Errorf("syscmd: 启动或执行 %s 失败: %w", spec.Program, err)
	}
	return res, nil
}

// validateProgram 校验程序路径：必须是绝对路径，且只含可执行文件名里可能出现的字符。
//
// 允许的字符集刻意很小（字母、数字、路径分隔符与少数安全符号）。空格、引号、分号、
// 管道、`$`、反引号、换行等一律拒绝——这些字符只可能出现在“一段命令”里，
// 而本包只接受“一个程序 + 有序参数列表”。
func validateProgram(program string) error {
	for _, r := range program {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '/', r == '.', r == '_', r == '-', r == '+', r == ':', r == '@', r == '~', r == '\\':
		default:
			return fmt.Errorf("%w %q（程序路径只能是一个文件名，不能是命令字符串）", ErrProgramInvalid, r)
		}
	}
	if !filepath.IsAbs(program) {
		return fmt.Errorf("%w，当前为 %q", ErrProgramNotAbsolute, program)
	}
	return nil
}

// cappedBuffer 保留前 limit 字节并继续把剩余数据读完（不阻塞子进程），// 同时记录是否发生过截断。多余的字节被丢弃而不是缓冲在内存里。
type cappedBuffer struct {
	buf       bytes.Buffer
	limit     int64
	truncated bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	room := c.limit - int64(c.buf.Len())
	if room <= 0 {
		c.truncated = true
		return len(p), nil
	}
	if int64(len(p)) > room {
		c.buf.Write(p[:room])
		c.truncated = true
		return len(p), nil
	}
	c.buf.Write(p)
	return len(p), nil
}

func (c *cappedBuffer) String() string { return c.buf.String() }
