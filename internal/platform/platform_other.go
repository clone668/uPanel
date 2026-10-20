//go:build !linux

package platform

// osFacts 描述非 Linux 平台的能力：只能做开发与 mock 演示，
// 不提供真实系统读取或特权动作。production 模式由 CheckMode 明确拒绝。
var osFacts = osFactsType{
	supportsUnixSocket:    false,
	supportsPrivilegedOps: false,
	mockAvailable:         true,
	productionSupported:   false,
}

// IsRoot 在非 Linux 平台始终为 false：没有可用的 root 语义。
func IsRoot() bool { return false }
