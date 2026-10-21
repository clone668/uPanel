//go:build unix

package processes

import (
	"errors"
	"syscall"
)

// osKiller 是生产实现：一次 syscall.Kill。
//
// 信号到 syscall 的映射集中在这张表里：动作参数只能是 "TERM" 或 "KILL" 这两个
// 字符串，不存在“把用户输入当信号编号”的路径（SIGKILL=9 之类的数字一律不接受）。
var osSignals = map[string]syscall.Signal{
	SignalTERM: syscall.SIGTERM,
	SignalKILL: syscall.SIGKILL,
}

type osKiller struct{}

// Signal 向 pid 发送 signal。
func (osKiller) Signal(pid int, signal string) error {
	sig, ok := osSignals[signal]
	if !ok {
		return ErrInvalidParams
	}
	return syscall.Kill(pid, sig)
}

// isNoSuchProcess 判断错误是不是“这个 PID 上已经没有进程（ ESRCH ）”。
func isNoSuchProcess(err error) bool {
	return errors.Is(err, syscall.ESRCH)
}
