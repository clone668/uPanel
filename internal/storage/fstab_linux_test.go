//go:build linux

package storage

// 测试侧的属主接缝：只有 Linux 的 syscall.Stat_t 带 Uid/Gid。
//
// 生产代码里 fileOwner 也是这么分平台的（fstab_linux.go / fstab_other.go），
// 所以测试要用同一个形状造 FileInfo，否则“属主不是 root”这条规则在开发宿主上
// 根本走不到。

import "syscall"

func testOwnerSys(uid, gid int) any {
	return &syscall.Stat_t{Uid: uint32(uid), Gid: uint32(gid)}
}
