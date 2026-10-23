//go:build !unix

package backup

// 非 Unix 开发宿主上的桩（代理只在 Linux 上运行）。

import (
	"os"
	"os/exec"
)

func setProcessGroup(_ *exec.Cmd) {}

func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}

func processSignalName(_ *os.ProcessState) string { return "" }
