//go:build linux

package agentstore

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// requireRootOnlyFile 校验 path 是普通文件、属主 root:root、且属组与其他用户没有任何权限。
//
// 这是“密钥不可由 Web 读取”的实际执行点：upanel-web 既不能读也不能写这个文件。
// 符号链接一律拒绝——它可能被换成指向别处的链接。
func requireRootOnlyFile(path string, want os.FileMode) error {
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
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("无法读取 %s 的属主信息", path)
	}
	if st.Uid != 0 || st.Gid != 0 {
		return fmt.Errorf("%s 的属主必须是 root:root，实际为 %d:%d", path, st.Uid, st.Gid)
	}
	perm := info.Mode().Perm()
	if perm&0o077 != 0 {
		return fmt.Errorf("%s 的权限必须为 %#o（属组与其他用户无任何权限），实际为 %#o", path, want, perm)
	}
	if perm&0o400 == 0 {
		return fmt.Errorf("%s 的属主没有读权限，实际为 %#o", path, perm)
	}
	return nil
}

// hasRootOwnershipSemantics 表示当前平台会真正检查属主与权限位。
func hasRootOwnershipSemantics() bool { return true }

// requireRootOnlyDir 校验 path 是 root 拥有、且属组与其他用户不可写的真实目录。
//
// 目录可写就能替换里面的文件（包括数据库与密钥），所以这里必须拦。
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
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("无法读取目录 %s 的属主信息", path)
	}
	if st.Uid != 0 {
		return fmt.Errorf("目录 %s 的属主必须是 root，实际为 %d", path, st.Uid)
	}
	if perm := info.Mode().Perm(); perm&0o022 != 0 {
		return fmt.Errorf("目录 %s 不得对属组或其他用户可写，实际权限 %#o", path, perm)
	}
	return nil
}
