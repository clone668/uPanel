//go:build !linux

package storage

// 设备号的平台实现（非 Linux）。
//
// 这里不做任何推断：占用检查依赖 /proc 与 Linux 的 dev_t 编码，别的平台上一律报
// “无法判断”。返回错误而不是空结果，是为了让判决层把 DEVICE_BUSY 记成阻塞项——
// 在不能判断的地方，“看起来没人用”是最危险的答案。代理本身只在 Linux 上运行，
// 这个文件存在的意义只是让整个包在开发机上能编译、能跑单元测试。

import "errors"

var errPlatformUnsupported = errors.New("storage: 占用检查只在 Linux 上实现")

func osMajorMinor(fdPath string) (string, error) { return "", errPlatformUnsupported }

func osDeviceID(path string) (uint64, error) { return 0, errPlatformUnsupported }
