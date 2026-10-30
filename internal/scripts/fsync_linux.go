//go:build linux

package scripts

import "os"

// syncDirStrict 让目录项落盘。Linux 上目录可以 fsync，失败必须上抛：
// rename 已经发生了，"文件在、目录项没持久化"在掉电后就是"脚本丢了"，
// 静默返回会让一次 Apply 报成功而现场未必如此。
func syncDirStrict(d *os.File) error { return d.Sync() }
