//go:build linux

package platform

import "os"

// osFacts 描述 Linux 上的真实能力。此文件是 Linux 唯一的事实来源；
// 其他操作系统由 platform_other.go 提供，二者不互相 fallback。
var osFacts = osFactsType{
	supportsUnixSocket:    true,
	supportsPrivilegedOps: true,
	mockAvailable:         true,
	productionSupported:   true,
}

// IsRoot 报告当前进程的有效 UID 是否为 0。
func IsRoot() bool { return os.Geteuid() == 0 }
