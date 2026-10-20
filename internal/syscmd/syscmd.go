// Package syscmd 执行外部系统命令，并强制 PLAN.md 第 4 节规定的执行边界：
//
//   - 只按绝对路径调用可执行文件，逐项传参，绝不经过 sh -c / bash -c；
//   - 环境变量使用固定白名单，不继承调用者环境（LD_PRELOAD、LD_LIBRARY_PATH、
//     BASH_ENV、IFS 等用户可控的加载器/解释器变量一律不进入子进程）；
//   - 标准输入为空，输出有硬上限，超时后连同子进程组一起终止；
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
)

// 环境白名单。只给子进程三件事：固定的 PATH、固定的 UTF-8 区域设置，别无其他。
var fixedEnv = []string{
	"PATH=" + DefaultPath,
	"LANG=C.UTF-8",
	"LC_ALL=C.UTF-8",
}

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
)

// Spec 描述一次调用。Args 逐项传给可执行文件，不做任何 shell 解释。
type Spec struct {
	// Program 必须是绝对路径，且不来自 HTTP 参数（PLAN.md 第 4 节）。
	Program string
	// Args 是逐项参数，可以为空。
	Args []string
	// Dir 是可选的绝对工作目录；为空时使用进程当前目录。
	Dir string
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
	Program         string
	ExitCode        int
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
	limits = limits.withDefaults()

	runCtx, cancel := context.WithTimeout(ctx, limits.Timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, spec.Program, spec.Args...)
	cmd.Env = fixedEnv
	cmd.Dir = spec.Dir
	// 标准输入保持为空：不把父进程（可能是 Web 或交互终端）的输入交给子进程。
	cmd.Stdin = nil

	stdout := &cappedBuffer{limit: limits.MaxStdout}
	stderr := &cappedBuffer{limit: limits.MaxStderr}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	// 终止时杀掉整个进程组，避免子进程派生的孙进程继续运行。
	setProcAttr(cmd)
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
