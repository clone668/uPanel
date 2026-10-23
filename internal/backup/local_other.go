//go:build !linux

package backup

// 非 Linux 开发主机上的桩：如实报告“没有本地容量读数”，而不是回一个 0。
//
// 代理只在 Linux 上跑（PLAN 第 3 节）；这个文件存在只是为了让 `go build ./...`
// 在开发宿主上也能过，和我们对待 storage/files 的方式一致。

import (
	"errors"
	"os"
	"path/filepath"
)

// ErrNoLocalCapacity 表示这个平台上没有本地容量实现。
var ErrNoLocalCapacity = errors.New("backup: 这个平台没有本地容量读取实现")

// LocalProbe 见 local_linux.go。
type LocalProbe struct {
	Exists     bool
	IsDir      bool
	RepoConfig bool
	Capacity   Capacity
}

// ProbeLocal 在非 Linux 上只 stat，不给容量。
func ProbeLocal(path string) (LocalProbe, error) {
	var probe LocalProbe
	info, err := os.Stat(path)
	if err == nil {
		probe.Exists = true
		probe.IsDir = info.IsDir()
		if probe.IsDir {
			if _, err := os.Stat(filepath.Join(path, "config")); err == nil {
				probe.RepoConfig = true
			}
		}
	}
	probe.Capacity = Capacity{Supported: false, Target: path, Error: ErrNoLocalCapacity.Error(), CheckedAt: nowUTC()}
	return probe, nil
}

// EnsureDir 见 local_linux.go。
func EnsureDir(path string) error { return os.MkdirAll(path, 0o700) }
