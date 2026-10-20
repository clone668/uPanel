//go:build linux

package monitoring

import "syscall"

// machineArch 返回内核报告的机器架构（与 `uname -m` 一致）。
//
// 不用 runtime.GOARCH：那是二进制的编译目标，在“amd64 二进制跑在 arm64 上”这类
// 场景里会给出与机器不符的答案。
func machineArch() string {
	var u syscall.Utsname
	if err := syscall.Uname(&u); err != nil {
		return ""
	}
	buf := make([]byte, 0, len(u.Machine))
	for _, c := range u.Machine {
		if c == 0 {
			break
		}
		buf = append(buf, byte(c))
	}
	return string(buf)
}
