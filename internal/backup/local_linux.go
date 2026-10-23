//go:build linux

package backup

// 本地仓库的容量与路径探测（Linux）。

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// LocalProbe 是本地路径的一次探测结果。
type LocalProbe struct {
	// Exists 表示路径本身存在。
	Exists bool
	// IsDir 表示它是一个目录。
	IsDir bool
	// RepoConfig 表示它下面已经有 restic 的 config 文件。
	RepoConfig bool
	// Capacity 是最近一个存在祖先的容量。
	Capacity Capacity
}

// ProbeLocal 探测本地路径（只读）。
func ProbeLocal(path string) (LocalProbe, error) {
	var probe LocalProbe
	info, err := os.Stat(path)
	switch {
	case err == nil:
		probe.Exists = true
		probe.IsDir = info.IsDir()
	case errors.Is(err, errNotExistSentinel):
		probe.Exists = false
	default:
		return probe, fmt.Errorf("backup: 读取本地路径 %s 失败: %w", path, err)
	}
	if probe.Exists && probe.IsDir {
		if _, err := os.Stat(filepath.Join(path, "config")); err == nil {
			probe.RepoConfig = true
		} else if !errors.Is(err, errNotExistSentinel) {
			return probe, fmt.Errorf("backup: 读取本地 %s/config 失败: %w", path, err)
		}
	}
	capacity, err := localCapacity(path)
	if err != nil {
		probe.Capacity = Capacity{Supported: false, Target: path, Error: err.Error(), CheckedAt: nowUTC()}
		return probe, nil
	}
	probe.Capacity = capacity
	return probe, nil
}

// localCapacity 读目标（或它最近的存活祖先）所在文件系统的容量。
func localCapacity(path string) (Capacity, error) {
	probePath := path
	for {
		var st unix.Statfs_t
		err := unix.Statfs(probePath, &st)
		if err == nil {
			// Bsize 是首选 I/O 块大小，statvfs 的语义是 f_bsize × f_blocks。
			bsize := int64(st.Bsize)
			return Capacity{
				Supported:      true,
				Target:         probePath,
				TotalBytes:     bsize * int64(st.Blocks),
				AvailableBytes: bsize * int64(st.Bavail),
				CheckedAt:      nowUTC(),
			}, nil
		}
		if !errors.Is(err, unix.ENOENT) {
			return Capacity{}, fmt.Errorf("读取 %s 的容量失败: %w", probePath, err)
		}
		parent := filepath.Dir(probePath)
		if parent == probePath {
			return Capacity{}, fmt.Errorf("%s 及其所有祖先都不存在", path)
		}
		probePath = parent
	}
}

// EnsureDir 创建一个本地目录（restic 自己也会建，但我们要先知道权限对不对）。
//
// 只有执行阶段调用；预检不写盘。权限 0700：仓库里是加密后的数据与索引，
// 除了 root 没有第二个角色需要读它。
func EnsureDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("backup: 创建本地仓库目录 %s 失败: %w", path, err)
	}
	return nil
}
