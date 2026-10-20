//go:build unix

package syscmd

import (
	"os/exec"
	"syscall"
)

// setProcAttr 让子进程进入独立进程组，终止时可以连同它派生的进程一起杀掉。
func setProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup 向整个进程组发送 SIGKILL。
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	// 负 PID 表示进程组；如果进程组已经消失，退回到只杀主进程。
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		return cmd.Process.Kill()
	}
	return nil
}
