//go:build unix

package syscmd

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// signalName 返回终止子进程的信号名；正常退出的进程返回空串。
//
// WaitStatus.Signaled 为假时不编造名字——退出码 0 或非零都能说明问题，只有
// 被信号打死这一种情形的退出码是 -1，那才是这个函数存在的理由。
func signalName(ps *os.ProcessState) string {
	if ps == nil {
		return ""
	}
	ws, ok := ps.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() {
		return ""
	}
	sig := ws.Signal()
	switch sig {
	case syscall.SIGKILL:
		return "SIGKILL"
	case syscall.SIGTERM:
		return "SIGTERM"
	case syscall.SIGINT:
		return "SIGINT"
	case syscall.SIGSEGV:
		return "SIGSEGV"
	case syscall.SIGABRT:
		return "SIGABRT"
	case syscall.SIGPIPE:
		return "SIGPIPE"
	case syscall.SIGHUP:
		return "SIGHUP"
	}
	return fmt.Sprintf("信号 %d", int(sig))
}

// setProcAttr 让子进程进入独立进程组，终止时可以连同它派生的进程一起杀掉；
// 给了 Credential 时同时把子进程降到那个身份（fork 之后、exec 之前生效）。
func setProcAttr(cmd *exec.Cmd, cred *Credential) error {
	attr := &syscall.SysProcAttr{Setpgid: true}
	if cred != nil {
		attr.Credential = &syscall.Credential{
			Uid:    cred.UID,
			Gid:    cred.GID,
			Groups: cred.Groups,
		}
	}
	cmd.SysProcAttr = attr
	return nil
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
