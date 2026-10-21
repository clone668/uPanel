package processes

import (
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
)

// 本包的样例全部是 2026-10-21 从测试虚拟机 upanel-2404 上真实抓下来的 /proc 文件
// （Ubuntu 24.04 / kernel 6.8.0-147-generic / systemd 255），见 testdata/。
// 解析器必须能吃掉这些真实字节，而不是吃掉我猜的格式。

func fixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("读取样例 %s 失败: %v", name, err)
	}
	return string(data)
}

// fakeFS 是把 /proc 换成一张表的文件系统。
//
// 路径用 path.Join 拼（与生产一致），所以同一份样例在 Linux 与开发机上读到的键
// 完全相同；缺失的路径返回 ENOENT，与真实 /proc 上进程消失时一致。
type fakeFS struct {
	mu    sync.Mutex
	files map[string]string
	links map[string]string
	dirs  map[string][]string
	// listErr 让“读不到 /proc 目录”这条失败路径可以被测到。
	listErr error
}

func newFakeFS() *fakeFS {
	return &fakeFS{
		files: map[string]string{},
		links: map[string]string{},
		dirs:  map[string][]string{"/proc": {}},
	}
}

func (f *fakeFS) ReadFile(name string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	content, ok := f.files[name]
	if !ok {
		return nil, &os.PathError{Op: "open", Path: name, Err: os.ErrNotExist}
	}
	return []byte(content), nil
}

func (f *fakeFS) ListDir(dir string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	names, ok := f.dirs[dir]
	if !ok {
		return nil, &os.PathError{Op: "open", Path: dir, Err: os.ErrNotExist}
	}
	return append([]string{}, names...), nil
}

func (f *fakeFS) Readlink(name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	target, ok := f.links[name]
	if !ok {
		return "", &os.PathError{Op: "readlink", Path: name, Err: os.ErrNotExist}
	}
	return target, nil
}

func (f *fakeFS) set(name, content string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[name] = content
}

func (f *fakeFS) remove(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.files, name)
}

// addProc 把一个真实样例挂到指定 PID 上（文件按 <样例名>.<字段> 命名）。
func (f *fakeFS) addProc(t *testing.T, pid int64, sample string) {
	t.Helper()
	base := "/proc/" + strconv.FormatInt(pid, 10)
	for _, field := range []string{"stat", "status", "cmdline", "cgroup"} {
		name := "testdata/" + sample + "." + field
		data, err := os.ReadFile(filepath.FromSlash(name))
		if err != nil {
			continue
		}
		f.set(base+"/"+field, string(data))
	}
	f.mu.Lock()
	f.dirs["/proc"] = append(f.dirs["/proc"], strconv.FormatInt(pid, 10))
	f.mu.Unlock()
}

// setStatFile 设置 /proc/stat。
func (f *fakeFS) setStatFile(content string) { f.set("/proc/stat", content) }

// testPaths 是测试用的固定路径。
func testPaths() Paths { return Paths{ProcRoot: "/proc", Passwd: "/etc/passwd"} }

// newTestFS 构造一份“一台小机器”的 /proc：PID 1、内核线程、sshd、一个普通进程、
// 一个僵尸、一个命令行里带秘密的进程。
func newTestFS(t *testing.T) *fakeFS {
	t.Helper()
	fs := newFakeFS()
	fs.setStatFile(fixture(t, "stat.txt"))
	fs.set("/etc/passwd", fixture(t, "passwd.txt"))
	fs.addProc(t, 1, "pid1")
	fs.addProc(t, 3, "kthread")
	fs.addProc(t, 860, "sshd")
	fs.addProc(t, 39491, "secretcmd")
	fs.addProc(t, 39503, "zombie")
	fs.addProc(t, 39515, "sleep")
	return fs
}
