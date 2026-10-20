//go:build !linux

package monitoring

// machineArch 在非 Linux 平台上返回空串：这些平台没有可采集的 /proc，
// 采集器本身也只在代理（Linux）里运行。这里存在的意义只有让代码在开发宿主上编译与测试。
func machineArch() string { return "" }
