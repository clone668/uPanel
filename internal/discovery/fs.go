package discovery

import (
	"os"
	"path"
)

// osFS 是本包读取 /proc 的真实实现。它满足 processes.FileSystem——用一个接口而不是
// 直接调 os，是为了让测试用真机抓下来的 /proc 字节做输入（与 P06 的进程采集一致）。
type osFS struct{}

func (osFS) ReadFile(name string) ([]byte, error) { return os.ReadFile(name) }

func (osFS) ListDir(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, path.Base(entry.Name()))
	}
	return names, nil
}

func (osFS) Readlink(name string) (string, error) { return os.Readlink(name) }
