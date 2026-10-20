//go:build linux

package agent

import (
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
)

// peerCred 用 SO_PEERCRED 读取已连接 Unix socket 的对端身份。
//
// 这是内核报告的凭据，对端无法在请求里伪造。读取失败时必须拒绝服务，
// 不能退化成“没有凭据就放行”。
func peerCred(conn *net.UnixConn) (peerInfo, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return peerInfo{}, fmt.Errorf("获取原始连接失败: %w", err)
	}
	var (
		cred  *syscall.Ucred
		opErr error
	)
	if ctlErr := raw.Control(func(fd uintptr) {
		cred, opErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); ctlErr != nil {
		return peerInfo{}, fmt.Errorf("读取 SO_PEERCRED 失败: %w", ctlErr)
	}
	if opErr != nil {
		return peerInfo{}, fmt.Errorf("读取 SO_PEERCRED 失败: %w", opErr)
	}
	if cred == nil {
		return peerInfo{}, errors.New("读取 SO_PEERCRED 返回空凭据")
	}
	return peerInfo{UID: cred.Uid, GID: cred.Gid, PID: cred.Pid}, nil
}

// verifySocketDir 校验 socket 所在目录的属主与权限。
//
// 要求（PLAN.md 第 4 节）：
//   - 属主是 root：目录由安装流程/systemd 以 root 创建，避免被替换；
//   - 组和其他用户不可写：否则未授权用户可以把 socket 换掉；
//   - 至少对 socket 属组或其他人可进入：否则 Web 账号根本连不到 socket，
//     这种失败必须在启动时明确报出来，而不是等到第一次调用才神秘地连不上。
func verifySocketDir(dir string, wantGID uint32) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("socket 目录不可用 %s: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("socket 目录不是目录: %s", dir)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("无法读取 socket 目录的属主信息 %s", dir)
	}
	if st.Uid != 0 {
		return fmt.Errorf("socket 目录 %s 的属主 UID 为 %d，期望 0（root）", dir, st.Uid)
	}
	perm := info.Mode().Perm()
	if perm&0o022 != 0 {
		return fmt.Errorf("socket 目录 %s 的权限为 %#o：不能对组或其他用户可写", dir, perm)
	}
	groupTraversable := perm&0o010 != 0 && st.Gid == wantGID
	otherTraversable := perm&0o001 != 0
	if !groupTraversable && !otherTraversable {
		return fmt.Errorf("socket 目录 %s 的权限为 %#o（root:%d）：Web 账号无法进入该目录，"+
			"请设置为 0755（root:root）或 0750 且属组为 socket 属组", dir, perm, st.Gid)
	}
	return nil
}

// verifySocketOwnership 回读 socket 的属主/属组/权限，确认设置真的生效。
// 只检查“chmod 没返回错误”是不够的：文件系统可能是挂载进来的、权限位可能被屏蔽。
func verifySocketOwnership(path string, wantGID uint32) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("回读 socket 状态失败 %s: %w", path, err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("无法读取 socket 属主信息 %s", path)
	}
	if st.Uid != 0 {
		return fmt.Errorf("socket 属主 UID 为 %d，期望 0（root）", st.Uid)
	}
	if st.Gid != wantGID {
		return fmt.Errorf("socket 属组 GID 为 %d，期望 %d", st.Gid, wantGID)
	}
	if perm := info.Mode().Perm(); perm != SocketMode {
		return fmt.Errorf("socket 权限为 %#o，期望 %#o", perm, SocketMode)
	}
	return nil
}
