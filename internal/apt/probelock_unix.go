//go:build unix

package apt

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// probeLock 试着对锁文件加一次非阻塞排他锁。
//
// 拿不到（EWOULDBLOCK）就说明有别的进程正持有它——真正的 apt/dpkg 就是这么加锁的。
// 拿到之后立刻释放：这次探测只持续到 close 为止，不会让下一个进程等我们。
//
// 文件不存在（ENOENT）不算错也不算占用：很多系统上 `/var/lib/apt/lists/lock` 只在
// 需要时才出现。权限不足则如实报错——那是"读不到"，不是"有人在跑"。
func probeLock(path string) (held bool, err error) {
	file, err := os.OpenFile(path, os.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	defer file.Close()

	fd := int(file.Fd())
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			return true, nil
		}
		return false, err
	}
	// 释放失败没有意义（close 也会释放），忽略。
	_ = unix.Flock(fd, unix.LOCK_UN)
	return false, nil
}
