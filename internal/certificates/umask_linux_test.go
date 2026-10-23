//go:build linux

package certificates

import "syscall"

// setUmaskForTest 只在 POSIX 上有意义；见 probe_test.go 顶部说明。
func setUmaskForTest(mask int) int { return syscall.Umask(mask) }
