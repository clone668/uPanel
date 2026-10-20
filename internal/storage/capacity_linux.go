//go:build linux

package storage

import (
	"context"
	"fmt"
	"syscall"
	"time"
)

// Capacity 读取挂载点容量与 inode 使用情况。
func (OSCapacityReader) Capacity(ctx context.Context, mountPath string) (*Capacity, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cap, err := statfsCapacity(mountPath)
	if err != nil {
		return nil, err
	}
	cap.CollectedAt = time.Now().UTC()
	return cap, nil
}

// statfsCapacity 用 statfs 读出总容量、可用容量与 inode 使用情况。
//
// 取的是**非特权**可用容量（f_bavail，即普通用户能写的空间），不是 f_bfree：
// 界面上说“还能用多少”应当与 df 的口径一致，否则用户会以为写 5% 预留空间也不算满。
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
