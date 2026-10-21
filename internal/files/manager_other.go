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
	"io"
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
	Capacity       CapacityReader
	Logger         *slog.Logger
	Now            func() time.Time
	MountInfoPath  string
	MaxUploadBytes int64
	Accounts       ManagedAccountResolver
}

// Manager 在非 Linux 平台上只是一个“拒绝执行”的壳。
type Manager struct {
	roots     []Root
	maxUpload int64
}

// New 记录登记表；任何读取都会以 ErrUnsupported 失败。
func New(roots []Root, opts Options) (*Manager, error) {
	m := &Manager{roots: roots, maxUpload: opts.MaxUploadBytes}
	if m.maxUpload <= 0 {
		m.maxUpload = DefaultMaxUploadBytes
	}
	return m, nil
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

// ─────────────── P06.2 写路径：非 Linux 上一律拒绝 ───────────────
//
// 这里没有“退化实现”。写操作比读操作更容易在退化实现里悄悄做错事
// （覆盖了不该覆盖的文件、把符号链接当普通文件改了权限），所以整条写路径
// 在非 Linux 上都只有一个答案：ErrPlatform。

// Stat 拒绝读取目标形状。
func (m *Manager) Stat(_ context.Context, _, _ string) (PathStat, error) {
	return PathStat{}, ErrPlatform
}

// StatTree 拒绝递归统计。
func (m *Manager) StatTree(_ context.Context, _, _ string) (PathStat, error) {
	return PathStat{}, ErrPlatform
}

// CreateUpload 拒绝创建暂存。
func (m *Manager) CreateUpload(_ context.Context, _ CreateUploadParams) (UploadInfo, error) {
	return UploadInfo{}, ErrPlatform
}

// WriteUploadContent 拒绝接收上传内容。
func (m *Manager) WriteUploadContent(_ context.Context, _ string, _ io.Reader) (UploadInfo, error) {
	return UploadInfo{}, ErrPlatform
}

// GetUpload 拒绝查询暂存。
func (m *Manager) GetUpload(_ context.Context, _ string) (UploadInfo, error) {
	return UploadInfo{}, ErrPlatform
}

// DeleteUpload 拒绝删除暂存。
func (m *Manager) DeleteUpload(_ context.Context, _ string) error { return ErrPlatform }

// SweepStaging 拒绝清理暂存。
func (m *Manager) SweepStaging(_ context.Context, _ time.Time) (int, error) { return 0, ErrPlatform }

// Mkdir 拒绝建目录。
func (m *Manager) Mkdir(_ context.Context, params MkdirParams) (WriteResult, error) {
	if err := params.Validate(); err != nil {
		return WriteResult{}, err
	}
	return WriteResult{}, ErrPlatform
}

// WriteText 拒绝改写文本。
func (m *Manager) WriteText(_ context.Context, params WriteTextParams) (WriteResult, error) {
	if err := params.Validate(); err != nil {
		return WriteResult{}, err
	}
	return WriteResult{}, ErrPlatform
}

// CommitUpload 拒绝提交上传。
func (m *Manager) CommitUpload(_ context.Context, params CommitUploadParams) (WriteResult, error) {
	if err := params.Validate(); err != nil {
		return WriteResult{}, err
	}
	return WriteResult{}, ErrPlatform
}

// Copy 拒绝复制。
func (m *Manager) Copy(_ context.Context, params CopyParams) (WriteResult, error) {
	if err := params.Validate(); err != nil {
		return WriteResult{}, err
	}
	return WriteResult{}, ErrPlatform
}

// Move 拒绝移动。
func (m *Manager) Move(_ context.Context, params CopyParams) (WriteResult, error) {
	if err := params.Validate(); err != nil {
		return WriteResult{}, err
	}
	return WriteResult{}, ErrPlatform
}

// Remove 拒绝删除。
func (m *Manager) Remove(_ context.Context, params DeleteParams) (WriteResult, error) {
	if err := params.Validate(); err != nil {
		return WriteResult{}, err
	}
	return WriteResult{}, ErrPlatform
}

// Chmod 拒绝改权限。
func (m *Manager) Chmod(_ context.Context, params ChmodParams) (WriteResult, error) {
	if err := params.Validate(); err != nil {
		return WriteResult{}, err
	}
	return WriteResult{}, ErrPlatform
}

// Chown 拒绝改所有者。
func (m *Manager) Chown(_ context.Context, params ChownParams) (WriteResult, error) {
	if err := params.Validate(); err != nil {
		return WriteResult{}, err
	}
	return WriteResult{}, ErrPlatform
}

// MaxUploadBytes 报告配置的单次上传上限（非 Linux 上仍然如实回报配置值：
// Web 侧要拿它做过大文件的早期拒绝，不需要真的能写）。
func (m *Manager) MaxUploadBytes() int64 {
	if m.maxUpload > 0 {
		return m.maxUpload
	}
	return DefaultMaxUploadBytes
}

// ReservedBytes 在非 Linux 上恒为 0：没有在途上传。
func (m *Manager) ReservedBytes(string) int64 { return 0 }

// AccountsAvailable 在非 Linux 上恒为 false：没有写路径，也就没有 chown。
func (m *Manager) AccountsAvailable() bool { return false }
