//go:build unix

package backup

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// dupRestoreFD 给子进程一个独立目录描述符。不能把 files 层拥有的原 fd
// 包进 os.File：包装对象回收时关闭它，会让后续发布后复核得到 EBADF。
// 用 F_DUPFD_CLOEXEC 一次完成复制与 close-on-exec，避免 Dup 后再
// CloseOnExec 之间被并发的另一次 exec 继承。
func dupRestoreFD(fd int) (*os.File, error) {
	dup, err := unix.FcntlInt(uintptr(fd), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("backup: 复制恢复目录句柄失败: %w", err)
	}
	file := os.NewFile(uintptr(dup), "upanel-restore-stage")
	if file == nil {
		_ = unix.Close(dup)
		return nil, fmt.Errorf("backup: 包装恢复目录句柄失败: fd %d", fd)
	}
	return file, nil
}
