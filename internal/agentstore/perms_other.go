//go:build !linux

package agentstore

import (
	"errors"
	"fmt"
	"os"
)

// hasRootOwnershipSemantics 表示当前平台不会检查属主与权限位（见下方说明）。
func hasRootOwnershipSemantics() bool { return false }

// requireRootOnlyFile 在非 Linux 开发宿主上只做类型检查。
//
// 属主与权限位在这里没有可移植语义（Windows 与 macOS 各不相同），真正的强制检查在
// Linux 上执行（见 perms_linux.go），并由 Linux 虚拟机里的验收脚本给出证据。
// 代理本身拒绝在非 Linux 上启动（platform.CheckAgent），所以这里不会成为生产路径。
func requireRootOnlyFile(path string, _ os.FileMode) error {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%s 不存在", path)
		}
		return fmt.Errorf("检查 %s 失败: %v", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s 是符号链接，必须是真实文件", path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s 不是普通文件（类型 %s）", path, info.Mode().Type())
	}
	return nil
}

// requireRootOnlyDir 在非 Linux 开发宿主上只做类型检查（理由同上）。
func requireRootOnlyDir(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("目录 %s 不可用: %v", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("目录 %s 是符号链接，必须是真实目录", path)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s 不是目录", path)
	}
	return nil
}
