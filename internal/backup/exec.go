package backup

// restic 子进程的执行边界。
//
// 这里**没有**复用 internal/syscmd，原因只有一个：syscmd 的环境白名单是给 apt 用的
// 闭集（DEBIAN_FRONTEND 等），而 restic 必须拿到仓库口令。口令不能进 argv
// （`/proc/<pid>/cmdline` 对同机所有用户可读），只能走环境；给 syscmd 开一个
// “任意环境变量”的口子，等于把那道边界拆掉。
//
// 所以这里是第二道边界，而且比 syscmd 更窄：
//   - 环境**不是**继承来的，而是固定底（PATH/LANG/LC_ALL/HOME）+ 下面三个键；
//   - 键是硬编码的常量，不接受调用方传入键名；
//   - 值只来自秘密存储的解密结果或代码里的常量。

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

// 允许追加给 restic 的环境变量名。硬编码，不接受调用方提供键名。
const (
	// EnvResticPassword 是仓库口令（CONTRACTS.md 第 12 节：秘密不进 argv）。
	EnvResticPassword = "RESTIC_PASSWORD"
	// EnvResticCacheDir 把 restic 的缓存钉在代理自己的目录里，
	// 而不是 /root/.cache——那会让 root 的 home 变成一处没人清理的状态。
	EnvResticCacheDir = "RESTIC_CACHE_DIR"
	// EnvHome 是 restic 计算默认缓存/配置位置用的 HOME。
	EnvHome = "HOME"
)

// fixedBaseEnv 是子进程看到的固定底环境。
var fixedBaseEnv = []string{
	"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	"LANG=C.UTF-8",
	"LC_ALL=C.UTF-8",
}

// 默认限额。
const (
	// defaultExecTimeout 是一次 restic 调用的上限。init 在 SFTP 上要建目录、
	// 写几个小文件，本机上是几十毫秒；10 分钟是“慢到不该再等”的量级。
	defaultExecTimeout = 10 * time.Minute
	// defaultMaxStdout 是 restic 输出的上限。`--json` 的 cat config 只有几百字节，
	// 1 MiB 是给将来 snapshots --json 留的。
	defaultMaxStdout = 1 << 20
	// defaultMaxStderr 是 stderr 的上限（restic 的报错通常是一行）。
	defaultMaxStderr = 64 << 10
)

// ExecSpec 是一次 restic 调用。
type ExecSpec struct {
	// Program 必须是绝对路径，来自装配层（不是请求参数）。
	Program string
	// Args 逐项传参，不经任何 shell。
	Args []string
	// Password 是要放进 RESTIC_PASSWORD 的值；空串表示不设置。
	Password string
	// CacheDir 是 RESTIC_CACHE_DIR 的值；空串表示不设置。
	CacheDir string
	// Home 是 HOME 的值；空串表示用默认的 /root。
	Home string
	// Timeout 覆盖默认上限。
	Timeout time.Duration
	// MaxStdout / MaxStderr 覆盖默认上限。
	MaxStdout int64
	MaxStderr int64
}

// ExecResult 是一次 restic 调用的结果。
type ExecResult struct {
	Program         string
	ExitCode        int
	Signal          string
	Stdout          string
	Stderr          string
	StdoutTruncated bool
	StderrTruncated bool
	TimedOut        bool
	Duration        time.Duration
}

// ErrExecTimeout 表示 restic 超出时限被终止。
var ErrExecTimeout = errors.New("backup: restic 超过运行时限，已终止")

// RunRestic 执行一次 restic 命令。
//
// 返回 nil error 表示正常退出；非零退出码返回 *ExitError（restic 自己的报错在
// Stderr 里，调用方据此给出人话），超时返回 ErrExecTimeout。
func RunRestic(ctx context.Context, spec ExecSpec) (ExecResult, error) {
	res := ExecResult{Program: spec.Program, ExitCode: -1}
	if spec.Program == "" {
		return res, errors.New("backup: 没有 restic 程序路径")
	}
	if err := checkProgramPath(spec.Program); err != nil {
		return res, err
	}

	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = defaultExecTimeout
	}
	maxOut := spec.MaxStdout
	if maxOut <= 0 {
		maxOut = defaultMaxStdout
	}
	maxErr := spec.MaxStderr
	if maxErr <= 0 {
		maxErr = defaultMaxStderr
	}

	env := append([]string{}, fixedBaseEnv...)
	home := spec.Home
	if home == "" {
		home = "/root"
	}
	env = append(env, EnvHome+"="+home)
	if spec.CacheDir != "" {
		env = append(env, EnvResticCacheDir+"="+spec.CacheDir)
	}
	if spec.Password != "" {
		env = append(env, EnvResticPassword+"="+spec.Password)
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, spec.Program, spec.Args...)
	cmd.Env = env
	cmd.Stdin = nil
	stdout := &cappedBuffer{limit: maxOut}
	stderr := &cappedBuffer{limit: maxErr}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	setProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	cmd.WaitDelay = 2 * time.Second

	start := time.Now()
	err := cmd.Run()
	res.Duration = time.Since(start)
	res.Stdout = stdout.String()
	res.Stderr = stderr.String()
	res.StdoutTruncated = stdout.truncated
	res.StderrTruncated = stderr.truncated
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
		res.Signal = processSignalName(cmd.ProcessState)
	}

	switch {
	case errors.Is(runCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil:
		res.TimedOut = true
		res.ExitCode = -1
		return res, fmt.Errorf("%w（上限 %s）", ErrExecTimeout, timeout)
	case errors.Is(runCtx.Err(), context.Canceled):
		return res, fmt.Errorf("backup: restic 调用被取消: %w", runCtx.Err())
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return res, &ExitError{Program: spec.Program, ExitCode: res.ExitCode}
	}
	if err != nil {
		return res, fmt.Errorf("backup: 启动 %s 失败: %w", spec.Program, err)
	}
	return res, nil
}

// ExitError 表示 restic 以非零状态退出。
type ExitError struct {
	Program  string
	ExitCode int
}

func (e *ExitError) Error() string {
	return fmt.Sprintf("backup: %s 以退出状态 %d 结束", e.Program, e.ExitCode)
}

// checkProgramPath 校验程序路径：绝对路径，且不含 shell 元字符。
func checkProgramPath(program string) error {
	for _, r := range program {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '/', r == '.', r == '_', r == '-', r == '+':
		default:
			return fmt.Errorf("backup: 程序路径含非法字符 %q", r)
		}
	}
	if !filepath.IsAbs(program) {
		return fmt.Errorf("backup: 程序路径必须是绝对路径（%q）", program)
	}
	return nil
}

// cappedBuffer 保留前 limit 字节，其余读完丢弃（不阻塞子进程）。
type cappedBuffer struct {
	buf       bytes.Buffer
	limit     int64
	truncated bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	written := len(p)
	if remaining := c.limit - int64(c.buf.Len()); remaining > 0 {
		if int64(len(p)) > remaining {
			c.buf.Write(p[:remaining])
			c.truncated = true
		} else {
			c.buf.Write(p)
		}
	} else if len(p) > 0 {
		c.truncated = true
	}
	return written, nil
}

func (c *cappedBuffer) String() string { return c.buf.String() }

// lastLine 取输出里最后一条非空行，用于把 restic 的报错压成一句话。
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return line
		}
	}
	return ""
}
