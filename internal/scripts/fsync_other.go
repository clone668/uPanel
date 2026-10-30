//go:build !linux

package scripts

import "os"

// syncDirStrict 在非 Linux 宿主上是尽力而为：Windows 对目录句柄的 Sync 必然失败，
// 而那里的面板进程本来就不负责真实部署（脚本目录只在 Linux 代理上存在）。
// 真正的约束在 fsync_linux.go。
func syncDirStrict(d *os.File) error {
	_ = d.Sync()
	return nil
}
