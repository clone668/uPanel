package journal

import (
	"fmt"
	"os"
	"path/filepath"
)

// journalctl 的查找位置。与 systemctl 一样只在固定的系统目录里找：
// 不接受 PATH，也不接受相对路径（PLAN.md 第 4 节：程序路径只能来自登记的动作定义）。
var journalctlCandidates = []string{"/usr/bin/journalctl", "/bin/journalctl"}

// FindJournalctl 返回可用的 journalctl 绝对路径。
//
// 显式配置了路径却不可用时**不会**退回自动查找：配置写错要能看出来，
// 否则运维以为用的是自己指定的那个，实际跑的是另一个。
func FindJournalctl(explicit string) (string, bool) {
	if explicit != "" {
		if err := checkExecutable(explicit); err != nil {
			return "", false
		}
		return explicit, true
	}
	for _, candidate := range journalctlCandidates {
		if err := checkExecutable(candidate); err == nil {
			return candidate, true
		}
	}
	return "", false
}

// checkExecutable 校验路径是一个可执行文件。
func checkExecutable(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("journal: 路径必须是绝对路径: %q", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("journal: 无法访问 %s: %w", path, err)
	}
	if info.IsDir() {
		return fmt.Errorf("journal: %s 是目录", path)
	}
	if info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("journal: %s 不可执行", path)
	}
	return nil
}
