//go:build unix

package backup

// 子进程的进程组与信号名（Unix）。

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// setProcessGroup 让 restic 进入独立进程组：它自己会再派 ssh，超时时必须一起杀掉。
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup 向整个进程组发 SIGKILL。
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		return cmd.Process.Kill()
	}
	return nil
}

// processSignalName 返回终止子进程的信号名；正常退出返回空串。
func processSignalName(ps *os.ProcessState) string {
	if ps == nil {
		return ""
	}
	ws, ok := ps.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() {
		return ""
	}
	switch ws.Signal() {
	case syscall.SIGKILL:
		return "SIGKILL"
	case syscall.SIGTERM:
		return "SIGTERM"
	case syscall.SIGINT:
		return "SIGINT"
	}
	return fmt.Sprintf("信号 %d", int(ws.Signal()))
}
