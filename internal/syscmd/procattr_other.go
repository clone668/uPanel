//go:build !unix

package syscmd

import (
	"errors"
	"os"
	"os/exec"
)

// signalName 在非 Unix 平台拿不到 WaitStatus，只能如实返回空串。
// 代理只在 Linux 上运行；这里返回空是为了让符号在两个平台都在。
func signalName(ps *os.ProcessState) string { return "" }

// setProcAttr 在非 Unix 平台没有进程组语义（开发宿主只用它跑单元测试，
// 代理本身只在 Linux 上运行并拒绝在其他平台启动）。降权同样不可用，
// 需要时明确报错，而不是"看起来降权了、其实还是同一个身份"。
func setProcAttr(cmd *exec.Cmd, cred *Credential) error {
	if cred != nil {
		return errors.New("syscmd: 当前平台不支持降权运行")
	}
	return nil
}

// killProcessGroup 在非 Unix 平台只能终止主进程。
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
