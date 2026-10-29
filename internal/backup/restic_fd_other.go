//go:build !unix

package backup

import "os"

// /proc/self/fd 恢复只在 Unix 产品路径执行。非 Unix 的纯 argv 单测使用
// fakeRunner，给 ExtraFiles 一个独立、可安全关闭的句柄，绝不接管虚构的原 fd。
func dupRestoreFD(int) (*os.File, error) {
	return os.Open(os.DevNull)
}
