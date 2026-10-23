//go:build !linux

package certificates

// setUmaskForTest 在非 Linux 上是空操作：Windows 没有 umask 语义，
// 对应的断言在 probe_test.go 里按平台跳过。
func setUmaskForTest(int) int { return 0 }
