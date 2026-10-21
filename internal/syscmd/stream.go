package syscmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
)

// P05.3：需要**持续**读一个子进程的输出（journalctl --follow），而 Run 只能等它结束。
//
// 这里刻意只补一条“启动 + 读 + 收尸”的最小通道，边界与 Run 完全一致：
//   - 同样的程序路径校验、固定环境、空标准输入、独立进程组；
//   - ctx 结束时杀整个进程组（不是只杀主进程），WaitDelay 兜住占着管道的孙进程；
//   - stderr 仍然有上限，且只能通过 Stderr() 读取。
//
// 它**不**提供写 stdin、不提供任意 fd、不提供 shell：调用方拿到的只有一个 stdout 读口。
// 用法固定为：Start → 读 Stdout 直到 EOF 或被取消 → Close（或 Wait）收尸。
// 不调用 Close 就丢下 Stream 是资源泄漏，调用方必须 defer 它。

// Stream 是一个正在运行的子进程。
type Stream struct {
	cmd    *exec.Cmd
	stdout io.ReadCloser
	stderr *cappedBuffer

	waitOnce sync.Once
	waitErr  error

	closeOnce sync.Once
	closeErr  error
}

// Start 启动一个持续运行的子进程，返回它的输出通道。
//
// 与 Run 的区别只有“不等它结束”：所有执行边界（程序路径、环境、进程组、输出上限）
// 都由本包统一施加，调用方仍然拿不到 argv 拼接或 shell。
func Start(ctx context.Context, spec Spec, limits Limits) (*Stream, error) {
	if spec.Program == "" {
		return nil, ErrEmptyProgram
	}
	if err := validateProgram(spec.Program); err != nil {
		return nil, err
	}
	limits = limits.withDefaults()

	cmd := exec.CommandContext(ctx, spec.Program, spec.Args...)
	cmd.Env = fixedEnv
	cmd.Dir = spec.Dir
	cmd.Stdin = nil

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("syscmd: 无法接管 %s 的标准输出: %w", spec.Program, err)
	}
	stderr := &cappedBuffer{limit: limits.MaxStderr}
	cmd.Stderr = stderr

	// 与 Run 同一套终止语义：ctx 取消时杀进程组，WaitDelay 兜住残留管道。
	setProcAttr(cmd)
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	cmd.WaitDelay = killWaitDelay

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("syscmd: 启动 %s 失败: %w", spec.Program, err)
	}
	return &Stream{cmd: cmd, stdout: stdout, stderr: stderr}, nil
}

// Stdout 返回子进程的标准输出。读完（EOF）之后再调用 Wait/Close。
func (s *Stream) Stdout() io.Reader { return s.stdout }

// Stderr 返回至今为止收集到的标准错误（有上限，超出部分被丢弃）。
func (s *Stream) Stderr() string { return s.stderr.String() }

// StderrTruncated 报告标准错误是否被截断。
func (s *Stream) StderrTruncated() bool { return s.stderr.truncated }

// Pid 返回子进程的 PID（未启动时返回 0）。仅用于日志与诊断。
func (s *Stream) Pid() int {
	if s.cmd == nil || s.cmd.Process == nil {
		return 0
	}
	return s.cmd.Process.Pid
}

// Wait 等待子进程结束并收尸。可以被多次调用（只真正等一次）。
//
// 返回的错误语义与 Run 一致：*ExitError 表示以非零状态退出（对 --follow 来说是
// 被信号终止的正常路径），其他错误表示启动/执行本身出了问题。
func (s *Stream) Wait() error {
	s.waitOnce.Do(func() {
		err := s.cmd.Wait()
		// StdoutPipe 的说明要求先读完再 Wait；这里在 Wait 之后关闭读口，
		// 让还在阻塞的读调用立刻返回，而不是等到垃圾回收。
		_ = s.stdout.Close()
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			s.waitErr = &ExitError{Program: s.cmd.Path, ExitCode: exitErr.ExitCode()}
			return
		}
		s.waitErr = err
	})
	return s.waitErr
}

// Close 终止子进程组、读完残留输出并收尸。可以被多次调用。
//
// 顺序是“先杀进程组，再等”：只 Close stdout 不动进程会留下一个继续往管道写、
// 最终被 SIGPIPE 打死的 journalctl，那正是 P05.3 验收要排除的残留子进程。
func (s *Stream) Close() error {
	s.closeOnce.Do(func() {
		if s.cmd.Process != nil {
			_ = killProcessGroup(s.cmd)
		}
		// Wait 的错误在这里只是“进程被我们杀了”，不是调用方需要处理的失败；
		// 真正的失败（启动失败）已经在 Start 里返回过了。
		_ = s.Wait()
		s.closeErr = nil
	})
	return s.closeErr
}
