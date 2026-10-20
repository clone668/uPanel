//go:build !linux

package agent

import (
	"errors"
	"net"
)

// peerCred 在非 Linux 平台上不可用：代理本身只在 Linux 上运行（platform.CheckAgent），
// 这个实现只是为了让开发宿主能编译并跑与 socket 无关的单元测试。
func peerCred(conn *net.UnixConn) (peerInfo, error) {
	return peerInfo{}, errors.New("当前平台不支持 SO_PEERCRED，代理只能在 Linux 上运行")
}

// verifySocketOwnership 在非 Linux 平台上无法读取 Unix 属主信息。
func verifySocketOwnership(path string, wantGID uint32) error {
	return errors.New("当前平台无法校验 Unix socket 属主，代理只能在 Linux 上运行")
}

// verifySocketDir 在非 Linux 平台上无法读取目录属主信息。
func verifySocketDir(dir string, wantGID uint32) error {
	return errors.New("当前平台无法校验 Unix socket 目录，代理只能在 Linux 上运行")
}
