//go:build !linux

package files

// 非 Linux 构建（P06.1）。
//
// CONTRACTS.md 第 11 节：“不支持该机制的平台拒绝真实文件操作”。这里就是那句话的实现：
// 所有读取方法都返回 ErrUnsupported，不做任何字符串前缀判断的替代方案——
// 那种替代方案在开发宿主上“看起来能用”，到了真机却是另一套语义，
// 而两边的差异恰好落在最危险的地方（谁能读哪个目录）。
//
// 开发宿主上真正验证文件行为的方式是交叉编译测试二进制并在 Ubuntu 虚拟机上跑
// （见 docs/PROGRESS.md 的测试流程），不是在这里加一条退化路径。

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"
)

// Root 是一个已登记的文件根（来自配置，不来自请求）。
type Root struct {
	ID       string
	Name     string
	Path     string
	Writable bool
}

// CapacityReader 读取根所在文件系统的容量（非 Linux 上不会被调用）。
type CapacityReader interface {
	Capacity(ctx context.Context, mountPath, fsType string) (*Capacity, error)
}

// Options 是 Manager 的构造参数。
type Options struct {
	Capacity      CapacityReader
	Logger        *slog.Logger
	Now           func() time.Time
	MountInfoPath string
}

// Manager 在非 Linux 平台上只是一个“拒绝执行”的壳。
type Manager struct {
	roots []Root
}

// New 记录登记表；任何读取都会以 ErrUnsupported 失败。
func New(roots []Root, _ Options) (*Manager, error) {
	return &Manager{roots: roots}, nil
}

// ErrPlatform 是这台机器上的拒绝原因。
var ErrPlatform = fmt.Errorf("%w: 当前平台不是 Linux，没有 openat2 与 RESOLVE_* 语义", ErrUnsupported)

// Roots 如实报告每个根都不可用：没有假装“空目录”。
func (m *Manager) Roots(_ context.Context) RootsResult {
	out := RootsResult{Roots: make([]RootInfo, 0, len(m.roots))}
	for _, root := range m.roots {
		out.Roots = append(out.Roots, RootInfo{
			RootID:            root.ID,
			Name:              root.Name,
			Path:              root.Path,
			Writable:          root.Writable,
			Available:         false,
			UnavailableReason: ErrPlatform.Error(),
		})
	}
	return out
}

// List 拒绝列举。
func (m *Manager) List(_ context.Context, params ListParams) (ListPage, error) {
	if err := params.Validate(); err != nil {
		return ListPage{}, err
	}
	return ListPage{}, ErrPlatform
}

// Text 拒绝读取。
func (m *Manager) Text(_ context.Context, params TextParams) (TextFile, error) {
	if err := params.Validate(); err != nil {
		return TextFile{}, err
	}
	return TextFile{}, ErrPlatform
}

// Download 是一个已打开、已核验的下载源（非 Linux 上永远不会被构造出来）。
type Download struct {
	File   *os.File
	Header DownloadHeader
}

// Close 关闭文件（非 Linux 上没有根句柄要还）。
func (d *Download) Close() error {
	if d == nil || d.File == nil {
		return nil
	}
	return d.File.Close()
}

// OpenDownload 拒绝下载。
func (m *Manager) OpenDownload(_ context.Context, params DownloadParams) (*Download, error) {
	if err := params.Validate(); err != nil {
		return nil, err
	}
	return nil, ErrPlatform
}
