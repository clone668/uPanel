//go:build !unix

package syscmd

import "os/exec"

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
