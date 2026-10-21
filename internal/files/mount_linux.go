//go:build linux

package files

// 挂载表解析（P06.1）。
//
// 这里只回答两个问题：这个绝对路径落在哪个挂载点上、那个挂载是什么文件系统。
// 用途有两处：给 files.roots 报告 mount_point/fs_type，以及让容量查询知道
// “这是不是网络挂载”（网络挂载的 statfs 必须走隔离子进程，见 internal/storage）。
//
// 为什么没有复用存储模块的解析：那边的解析与块设备采集绑在一起（它要 major:minor
// 去对 sysfs），而这里只要 (mountpoint, fstype) 两项。把那边整体导出只会让文件模块
// 连带依赖采集器；这两个函数的规模不值得那样做。差异由 mount_linux_test.go 里的
// 真实样本钉住。

import (
	"fmt"
	"os"
	"strings"
)

// mountEntry 是 /proc/self/mountinfo 里的一行（只保留用得到的字段）。
type mountEntry struct {
	mountPoint string
	fsType     string
}

// mountOf 返回 absPath 所在的挂载点与文件系统类型。
//
// 取**最长**的、且按目录边界匹配的挂载点：/boot/efi 不能被 /boot 覆盖，
// /bootxyz 也不能被算成在 /boot 里。
func (m *Manager) mountOf(absPath string) (string, string, error) {
	entries, err := readMountEntries(m.mounts)
	if err != nil {
		return "", "", err
	}
	best := mountEntry{}
	found := false
	for _, entry := range entries {
		if !pathHasPrefix(absPath, entry.mountPoint) {
			continue
		}
		if !found || len(entry.mountPoint) > len(best.mountPoint) {
			best = entry
			found = true
		}
	}
	if !found {
		return "", "", fmt.Errorf("files: 挂载表里找不到 %q 所在的挂载点", absPath)
	}
	return best.mountPoint, best.fsType, nil
}

// readMountEntries 解析挂载表。
func readMountEntries(path string) ([]mountEntry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("files: 读不到挂载表 %s: %w", path, err)
	}
	out := make([]mountEntry, 0, 32)
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// 分隔符 " - " 之后是 fstype、挂载源、super options；
		// 之前是 id、parent、major:minor、root、mountpoint、options、可选字段。
		before, after, ok := splitMountLine(line)
		if !ok {
			continue
		}
		pre := strings.Fields(before)
		post := strings.Fields(after)
		if len(pre) < 5 || len(post) < 1 {
			continue
		}
		out = append(out, mountEntry{
			mountPoint: unescapeMountField(pre[4]),
			fsType:     post[0],
		})
	}
	return out, nil
}

// splitMountLine 在第一个独立出现的 " - " 分隔符处切开。
//
// 不能直接 strings.Split(line, " - ")：挂载点里真的可能有空格（会被转义成 \040，
// 但 super options 里也可能出现 " - "）。按字段扫描更稳。
func splitMountLine(line string) (string, string, bool) {
	fields := strings.Split(line, " ")
	for i, field := range fields {
		if field != "-" {
			continue
		}
		if i == 0 || i+1 >= len(fields) {
			return "", "", false
		}
		return strings.Join(fields[:i], " "), strings.Join(fields[i+1:], " "), true
	}
	return "", "", false
}

// unescapeMountField 还原 mountinfo 里的八进制转义（空格、制表、换行、反斜杠）。
func unescapeMountField(s string) string {
	if !strings.ContainsRune(s, '\\') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			code := s[i+1 : i+4]
			if v, ok := parseOctal(code); ok {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// parseOctal 解析三位八进制。
func parseOctal(s string) (int, bool) {
	if len(s) != 3 {
		return 0, false
	}
	value := 0
	for i := 0; i < 3; i++ {
		c := s[i]
		if c < '0' || c > '7' {
			return 0, false
		}
		value = value*8 + int(c-'0')
	}
	return value, true
}

// pathHasPrefix 按目录边界判断前缀。
func pathHasPrefix(path, prefix string) bool {
	if prefix == "" {
		return false
	}
	if prefix == "/" {
		return strings.HasPrefix(path, "/")
	}
	if path == prefix {
		return true
	}
	return strings.HasPrefix(path, prefix+"/")
}
