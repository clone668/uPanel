//go:build !linux

package storage

// 非 Linux 平台上没有 syscall.Stat_t：返回 nil，fileOwner 会报“读不到属主”。
// 依赖属主断言的测试在非 Linux 上会 skip（见 volumeassess_test.go）。

func testOwnerSys(uid, gid int) any { return nil }
