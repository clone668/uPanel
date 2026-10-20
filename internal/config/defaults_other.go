//go:build !linux

package config

// defaultPath 在非 Linux 平台没有默认配置文件：开发宿主不应该自动读取
// 系统目录下的配置。调用方必须显式 --config。
func defaultPath() string { return "" }

// defaultDatabasePath 在非 Linux 平台没有默认数据库路径：必须显式 --db，
// 避免开发宿主在无提示的情况下读写系统位置。
func defaultDatabasePath() string { return "" }

// defaultAgentDatabasePath 在非 Linux 平台没有默认值：代理只在 Linux 上运行，
// 开发宿主必须显式给出路径（不会悄悄写到系统目录）。
func defaultAgentDatabasePath() string { return "" }

// defaultAgentSecretKeyPath 同上。
func defaultAgentSecretKeyPath() string { return "" }
