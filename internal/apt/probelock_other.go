//go:build !unix

package apt

import "errors"

// ErrLockProbeUnsupported 表示这个平台上没有 flock 语义。
//
// 开发宿主（Windows）只跑单元测试；代理只在 Linux 上运行并拒绝在其他平台启动，
// 所以这里返回"读不到"，而不是假装"没人占用"——后者会让一次真实的锁冲突在
// 宿主测试里被静默吞掉。
var ErrLockProbeUnsupported = errors.New("apt: 这个平台不支持探测 apt 锁")

func probeLock(string) (bool, error) { return false, ErrLockProbeUnsupported }
