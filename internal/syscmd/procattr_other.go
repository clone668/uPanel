//go:build !unix

package syscmd

import (
	"os"
	"os/exec"
)

// signalName 在非 Unix 平台拿不到 WaitStatus，只能如实返回空串。
// 代理只在 Linux 上运行；这里返回空是为了让符号在两个平台都在。
func signalName(ps *os.ProcessState) string { return "" }

// setProcAttr 在非 Unix 平台没有进程组语义（开发宿主只用它跑单元测试，
// 代理本身只在 Linux 上运行并拒绝在其他平台启动）。
func setProcAttr(cmd *exec.Cmd) {}

// killProcessGroup 在非 Unix 平台只能终止主进程。
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
