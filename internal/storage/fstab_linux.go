//go:build linux

package storage

import (
	"os"
	"syscall"
)

// fileOwner 取文件的数字属主。
//
// Linux 上它是 syscall.Stat_t 里的 Uid/Gid。用它而不是 user.Lookup：
// 这里要的是“这个目录是不是 root 的”，而名字解析在 NSS/LDAP 慢或挂掉的机器上
// 会阻塞——挂载点检查不该因为解析用户名而卡住。
func fileOwner(fi os.FileInfo) (uid, gid int, ok bool) {
	st, isStat := fi.Sys().(*syscall.Stat_t)
	if !isStat || st == nil {
		return 0, 0, false
	}
	return int(st.Uid), int(st.Gid), true
}
