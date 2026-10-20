// Package platform 隔离操作系统相关的实现与显式 mock 实现。
//
// 设计约束（PLAN.md 第 4 节、STAGES.md P01）：
//   - Linux 特定能力只在带 build tag 的实现中出现；
//   - 其他操作系统不能自动退回“伪成功”，production 模式必须明确拒绝启动；
//   - mock 是显式模式（--mode mock），只用于本地演示，且只绑定 127.0.0.1:18881。
package platform

import (
	"errors"
	"fmt"
	"runtime"
)

// Mode 是进程运行模式。只允许两个值，不存在“自动降级”。
type Mode string

const (
	// ModeProduction 使用真实的 Linux 实现。
	ModeProduction Mode = "production"
	// ModeMock 使用进程内演示实现，不触碰真实系统。
	ModeMock Mode = "mock"
)

// String 返回模式字符串。
func (m Mode) String() string { return string(m) }

// Valid 判断模式是否为已定义值。
func (m Mode) Valid() bool {
	return m == ModeProduction || m == ModeMock
}

// ParseMode 解析模式字符串，拒绝未知值（不做大小写或别名宽容）。
func ParseMode(v string) (Mode, error) {
	m := Mode(v)
	if !m.Valid() {
		return "", fmt.Errorf("未知运行模式 %q：只支持 %q 或 %q", v, ModeProduction, ModeMock)
	}
	return m, nil
}

// ErrProductionUnsupported 表示当前操作系统不支持 production 模式。
var ErrProductionUnsupported = errors.New("production 模式仅支持 Linux")

// Info 描述当前二进制与操作系统的静态事实。
type Info struct {
	OS   string `json:"os"`
	Arch string `json:"arch"`
	// SupportsUnixSocket 表示能否创建 Unix domain socket（代理通道必需）。
	SupportsUnixSocket bool `json:"supports_unix_socket"`
	// SupportsPrivilegedOps 表示能否执行真实系统读取与特权动作。
	SupportsPrivilegedOps bool `json:"supports_privileged_ops"`
	// MockAvailable 表示当前平台是否允许显式 mock 模式。
	MockAvailable bool `json:"mock_available"`
}

// osFactsType 由各操作系统的实现文件赋值，是本包唯一的能力事实来源。
type osFactsType struct {
	supportsUnixSocket    bool
	supportsPrivilegedOps bool
	mockAvailable         bool
	productionSupported   bool
}

// Current 返回当前平台事实。
func Current() Info {
	return Info{
		OS:                    runtime.GOOS,
		Arch:                  runtime.GOARCH,
		SupportsUnixSocket:    osFacts.supportsUnixSocket,
		SupportsPrivilegedOps: osFacts.supportsPrivilegedOps,
		MockAvailable:         osFacts.mockAvailable,
	}
}

// CheckMode 校验给定模式能否在当前平台运行。
//
// production 在非 Linux 上返回 ErrProductionUnsupported 的包装错误，调用方必须据此
// 以非零退出码拒绝启动，不得改为自动使用 mock。
func CheckMode(m Mode) error {
	if !m.Valid() {
		return fmt.Errorf("未知运行模式 %q", string(m))
	}
	if m == ModeMock {
		if !osFacts.mockAvailable {
			return fmt.Errorf("当前平台 %s/%s 不支持 mock 模式", runtime.GOOS, runtime.GOARCH)
		}
		return nil
	}
	if !osFacts.productionSupported {
		return fmt.Errorf("%w：当前平台为 %s/%s。本地演示请显式使用 --mode mock（绑定 127.0.0.1:18881）",
			ErrProductionUnsupported, runtime.GOOS, runtime.GOARCH)
	}
	return nil
}

// CheckAgent 校验特权代理的运行前提：必须是 Linux 且以 root 运行。
//
// 代理没有 mock 模式：演示数据由 Web 进程内的 mock 实现提供，代理只做真实系统操作。
func CheckAgent() error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("upanel-agent 只能在 Linux 上运行：当前平台为 %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	if !IsRoot() {
		return errors.New("upanel-agent 必须以 root 运行")
	}
	return nil
}
