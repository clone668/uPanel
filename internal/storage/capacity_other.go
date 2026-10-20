//go:build !linux

package storage

import (
	"fmt"
	"runtime"
)

// statfsCapacity 在非 Linux 平台上不可用。
//
// 本项目的采集器只在 Linux 上运行（代理是 root 的 systemd 服务）；这个文件存在的意义
// 是让 `go build ./...` 在开发宿主（Windows）上也能过，并且明确报“不可用”，
// 而不是偷偷给出一份假容量。
func statfsCapacity(mountPath string) (*Capacity, error) {
	return nil, fmt.Errorf("%w: %s 上没有 statfs（%s）", ErrCapacityUnavailable, mountPath, runtime.GOOS)
}
