//go:build !linux

package storage

import "os"

// fileOwner 在非 Linux 上不可用。
//
// 第二个返回值是 false 而不是 (0,0,true)：调用方据此跳过属主检查（fstab 与挂载点
// 检查只在 Linux 上有意义），而不是误以为“属主是 root”。这与 rdev_other.go 同一条
// 原则——平台上做不到的事要明确说出来，不能返回一个看起来正确的零值。
func fileOwner(fi os.FileInfo) (uid, gid int, ok bool) {
	return 0, 0, false
}
