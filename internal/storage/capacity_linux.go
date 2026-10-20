//go:build linux

package storage

import (
	"fmt"
	"syscall"
)

// statfsCapacity 用 statfs 读出总容量、可用容量与 inode 使用情况。
//
// 取的是**非特权**可用容量（f_bavail，即普通用户能写的空间），不是 f_bfree：
// 界面上说“还能用多少”应当与 df 的口径一致，否则用户会以为写 5% 预留空间也不算满。
//
// 这个函数**没有超时**：它是阻塞的系统调用。本地文件系统上这是微秒级操作，
// 但网络挂载上它可能卡在不可中断的内核 I/O 里——那正是它只能被本地挂载直接调用、
// 网络挂载必须走 capacity.go 里隔离子进程的原因。
func statfsCapacity(mountPath string) (*Capacity, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(mountPath, &st); err != nil {
		return nil, fmt.Errorf("%w: statfs %s 失败: %v", ErrCapacityUnavailable, mountPath, err)
	}
	bsize := uint64(st.Bsize)
	total := st.Blocks * bsize
	available := st.Bavail * bsize

	cap := &Capacity{
		TotalBytes:     total,
		AvailableBytes: available,
		InodeTotal:     st.Files,
		InodeAvailable: st.Ffree,
	}
	if total > 0 {
		used := uint64(0)
		if available < total {
			used = total - available
		}
		cap.UsedPercent = round1(float64(used) / float64(total) * 100)
	}
	// Files=0 表示这个文件系统没有 inode 概念（极少数伪文件系统）：
	// 那时百分比保持 0，而不是把“没有这个概念”说成“用了 0%”。
	if st.Files > 0 {
		used := uint64(0)
		if st.Ffree < st.Files {
			used = st.Files - st.Ffree
		}
		cap.InodeUsedPercent = round1(float64(used) / float64(st.Files) * 100)
	}
	return cap, nil
}
