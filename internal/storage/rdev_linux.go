//go:build linux

package storage

// 设备号的平台实现（Linux）。
//
// /proc/<pid>/fd/N 的 stat 里 st_rdev 就是那块设备的设备号。次设备号/主设备号的
// 位布局是内核的 new_encode_dev：主设备号是低 8 位之后的 12 位，再拼上高 32 位里
// 剩下的部分；次设备号是低 8 位与 12..31 位。不要自己发明除法和取模，会在大设备号上出错。

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// osMajorMinor 返回一个已打开 fd 指向的块设备的 "major:minor"。
//
//   - 普通文件、socket、管道返回空串（它们没有块设备号，也因此不构成占用）；
//   - 进程在扫描期间退出会让 os.Stat 返回 ENOENT，由调用方按“已消失”跳过。
func osMajorMinor(fdPath string) (string, error) {
	fi, err := os.Stat(fdPath)
	if err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return "", fmt.Errorf("%w: %v", errESRCH, err)
		}
		return "", err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("storage: 取不到 %s 的设备号（Stat_t 类型不符）", fdPath)
	}
	rdev := uint64(st.Rdev)
	if rdev == 0 {
		return "", nil
	}
	return fmt.Sprintf("%d:%d", devMajor(rdev), devMinor(rdev)), nil
}

// devMajor / devMinor 按内核的 new_encode_dev 解码 dev_t。
func devMajor(rdev uint64) uint32 {
	return uint32((rdev>>8)&0xfff) | uint32((rdev>>32)&^uint64(0xfff))
}

func devMinor(rdev uint64) uint32 {
	return uint32(rdev&0xff) | uint32((rdev>>12)&^uint64(0xff))
}
