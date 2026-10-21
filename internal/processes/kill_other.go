//go:build !unix

package processes

// osKiller 在非 unix 平台上是明确的“做不到”。
//
// 代理只在 Linux 上运行；这个分支存在的意义是：如果有人在本机（例如 Windows
// 开发机）编译整个仓库，得到的是一个编译得过、但一调用就报“不支持”的实现，
// 而不是一个假装发了信号的空函数。
type osKiller struct{}

// Signal 在非 unix 平台上永远返回错误。
func (osKiller) Signal(pid int, signal string) error {
	_ = pid
	_ = signal
	return ErrSignalUnsupported
}

// isNoSuchProcess 在非 unix 平台上没有 ESRCH 的概念。
func isNoSuchProcess(err error) bool { return false }
