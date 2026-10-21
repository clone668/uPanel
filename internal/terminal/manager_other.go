//go:build !linux

package terminal

// 非 Linux 平台没有 PTY 会话层。
//
// 开发宿主（Windows / macOS）只用于前端与 mock；PLAN.md 第 3 节明确写了 Windows
// 不能作为 Linux 系统功能已验证的证据。这里不提供“能编译就假装能跑”的实现：
// 会话层的每个入口都返回 ErrUnsupported，代理启动时会据此**不登记**终端动作与查询，
// capabilities 于是如实报告“本机没有终端”。

import "fmt"

// ResolveAccount 在非 Linux 平台上不可用。
func ResolveAccount(name, homeOverride, shellOverride string) (Account, error) {
	return Account{}, fmt.Errorf("%w: 当前平台不支持 PTY 终端", ErrUnsupported)
}

// ptyProcess 只在 Linux 上有实现；这里保留接口声明供共享代码编译。
type ptyProcess interface {
	Read(p []byte) (int, error)
	Write(p []byte) (int, error)
	Resize(cols, rows int) error
	Close() error
	Pid() int
	Wait() ExitStatus
}

// spawnSpec 与 Linux 侧同名同形。
type spawnSpec struct {
	Account    Account
	Cols, Rows int
}

// startPTY 在非 Linux 平台上不可用。
func startPTY(spec spawnSpec) (ptyProcess, error) {
	return nil, fmt.Errorf("%w: 当前平台不支持 PTY 终端", ErrUnsupported)
}

// isPTYClosed 在非 Linux 平台上没有 errno 可判。
func isPTYClosed(err error) bool { return false }
