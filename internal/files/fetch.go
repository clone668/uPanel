package files

// Web 侧的文件读取端（P06.1）。
//
// Web 进程不做任何文件系统访问：它只把 root_id 与相对路径交给代理的 files.* 查询，
// 把下载交给 files.download 流。所有边界判定都在代理里（openat2 + 根句柄），
// 这一层只做三件事：参数先校验一遍（好给出 400 而不是让代理回 422）、
// 结果校验自洽（不把不合契约的响应当成正常数据）、把代理的拒绝翻成 Web 的错误。
//
// 为什么要“再校验一遍参数”：Web 是暴露给浏览器的那个进程。让一次 `..` 直接
// 打到代理，等于把这个决定权交给网络往返——本地判定能给出一致的 400，
// 也不必为了一个语法错误去开一次 IPC。

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"upanel/internal/agentclient"
	"upanel/internal/httpx"
)

// ErrAgentUnavailable 表示读不到文件是因为代理不可用（连不上、超时、响应不合法）。
var ErrAgentUnavailable = errors.New("files: 代理不可用")

// Agent 是 Web 侧需要的代理能力。生产实现是 *agentclient.Client。
type Agent interface {
	Query(ctx context.Context, name string, params any) (json.RawMessage, error)
	CreateStream(ctx context.Context, purpose string, params any) (agentclient.StreamPermit, error)
	OpenStream(ctx context.Context, streamID string) (*agentclient.StreamReader, error)
}

// RemoteError 是代理明确拒绝了一次文件操作。
//
// 状态码与错误码来自代理（也就是 files.HTTPStatusOf 的判决），Web 原样转给浏览器：
// 一次“路径越界”不能因为经过了 Web 就变成“代理不可用”。
type RemoteError struct {
	Status  int
	Code    httpx.Code
	Message string
}

// Error 实现 error。
func (e *RemoteError) Error() string { return e.Message }

// Fetcher 是 Web 侧的文件读取端。
type Fetcher struct {
	agent  Agent
	logger *slog.Logger
}

// NewFetcher 构造 Fetcher。agent 为 nil 时返回错误：宁可启动时报错，
// 也不要运行时用一个永远返回空列表的假实现顶上。
func NewFetcher(agent Agent, logger *slog.Logger) (*Fetcher, error) {
	if agent == nil {
		return nil, errors.New("files: Fetcher 需要代理客户端")
	}
	return &Fetcher{agent: agent, logger: logger}, nil
}

// Roots 列出已登记的根。
func (f *Fetcher) Roots(ctx context.Context) (RootsResult, error) {
	raw, err := f.agent.Query(ctx, QueryRoots, RootsParams{})
	if err != nil {
		return RootsResult{}, f.mapError(err, "列出文件根")
	}
	var result RootsResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return RootsResult{}, fmt.Errorf("%w: 代理返回的文件根列表不是合法 JSON: %v", ErrAgentUnavailable, err)
	}
	if result.Roots == nil {
		// nil 与空列表在语义上不同：前者是“代理没给这个字段”，后者是“没有登记根”。
		// 页面要靠这个区别决定显示“响应不完整”还是“未登记文件根”。
		return RootsResult{}, fmt.Errorf("%w: 代理返回的文件根列表缺少 roots 字段", ErrAgentUnavailable)
	}
	for i, root := range result.Roots {
		if root.RootID == "" || root.Name == "" {
			return RootsResult{}, fmt.Errorf("%w: 第 %d 个文件根缺少 root_id 或 name", ErrAgentUnavailable, i+1)
		}
	}
	return result, nil
}

// List 列目录。
func (f *Fetcher) List(ctx context.Context, params ListParams) (ListPage, error) {
	if err := params.Validate(); err != nil {
		return ListPage{}, err
	}
	raw, err := f.agent.Query(ctx, QueryList, params)
	if err != nil {
		return ListPage{}, f.mapError(err, "列出目录")
	}
	var page ListPage
	if err := json.Unmarshal(raw, &page); err != nil {
		return ListPage{}, fmt.Errorf("%w: 代理返回的目录页不是合法 JSON: %v", ErrAgentUnavailable, err)
	}
	if page.RootID != params.RootID || page.Path != params.Path {
		// 代理回的必须是**这次问的那个目录**。回错了会让界面拼出错误的路径。
		return ListPage{}, fmt.Errorf("%w: 代理返回的目录页是 %s:%q，与请求的 %s:%q 不一致",
			ErrAgentUnavailable, page.RootID, page.Path, params.RootID, params.Path)
	}
	for i, entry := range page.Entries {
		if entry.Name == "" || entry.Version == "" {
			return ListPage{}, fmt.Errorf("%w: 第 %d 个目录项缺少 name 或 version", ErrAgentUnavailable, i+1)
		}
		switch entry.Type {
		case TypeFile, TypeDir, TypeSymlink, TypeOther:
		default:
			return ListPage{}, fmt.Errorf("%w: 第 %d 个目录项的类型 %q 不在闭集内",
				ErrAgentUnavailable, i+1, entry.Type)
		}
	}
	return page, nil
}

// Text 读文本。
func (f *Fetcher) Text(ctx context.Context, params TextParams) (TextFile, error) {
	if err := params.Validate(); err != nil {
		return TextFile{}, err
	}
	raw, err := f.agent.Query(ctx, QueryText, params)
	if err != nil {
		return TextFile{}, f.mapError(err, "读取文件文本")
	}
	var text TextFile
	if err := json.Unmarshal(raw, &text); err != nil {
		return TextFile{}, fmt.Errorf("%w: 代理返回的文本不是合法 JSON: %v", ErrAgentUnavailable, err)
	}
	if text.Version == "" || text.RootID != params.RootID || text.Path != params.Path {
		return TextFile{}, fmt.Errorf("%w: 代理返回的文本缺少 version 或路径不一致", ErrAgentUnavailable)
	}
	if text.Encoding != "utf-8" {
		return TextFile{}, fmt.Errorf("%w: 代理返回的编码 %q 不是 utf-8", ErrAgentUnavailable, text.Encoding)
	}
	if uint64(len(text.Content)) > MaxTextBytes {
		return TextFile{}, fmt.Errorf("%w: 代理返回的文本超过 %d 字节", ErrAgentUnavailable, MaxTextBytes)
	}
	return text, nil
}

// RemoteDownload 是一条已打开的下载流：首行元数据已解析，Reader 里是文件字节。
type RemoteDownload struct {
	Header DownloadHeader
	Reader io.Reader
	close  func() error
}

// Close 关闭流。关闭后代理会看到连接断开并释放句柄。
func (d *RemoteDownload) Close() error {
	if d == nil || d.close == nil {
		return nil
	}
	return d.close()
}

// OpenDownload 申请一条下载流并读出元数据首行。
func (f *Fetcher) OpenDownload(ctx context.Context, params DownloadParams) (*RemoteDownload, error) {
	if err := params.Validate(); err != nil {
		return nil, err
	}
	permit, err := f.agent.CreateStream(ctx, StreamPurposeDownload, params)
	if err != nil {
		return nil, f.mapError(err, "申请下载流许可")
	}
	reader, err := f.agent.OpenStream(ctx, permit.StreamID)
	if err != nil {
		return nil, f.mapError(err, "打开下载流")
	}
	// 首行是元数据（JSON + '\n'），其后是原始字节。边界只有这一处，
	// 因此这里用同一个 bufio.Reader 读首行、再把同一个 reader 交给调用方。
	buffered := reader.Raw()
	line, err := readHeaderLine(buffered)
	if err != nil {
		_ = reader.Close()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%w: 下载流在给出元数据之前就结束了", ErrAgentUnavailable)
		}
		return nil, err
	}
	var header DownloadHeader
	if err := json.Unmarshal(line, &header); err != nil {
		_ = reader.Close()
		return nil, fmt.Errorf("%w: 下载流的元数据无法解析: %v", ErrAgentUnavailable, err)
	}
	if header.Name == "" || header.Version == "" {
		_ = reader.Close()
		return nil, fmt.Errorf("%w: 下载流的元数据缺少 name 或 version", ErrAgentUnavailable)
	}
	return &RemoteDownload{Header: header, Reader: buffered, close: reader.Close}, nil
}

// readHeaderLine 读下载流的首行，并对长度设上限。
//
// 首行是元数据（名字、大小、版本、mtime），几百字节量级；给它 64 KiB 的余量，
// 超了就说明这条流不是我们认识的协议，宁可报错也不去读一个无界的“一行”。
func readHeaderLine(r io.Reader) ([]byte, error) {
	buffered, ok := r.(*bufio.Reader)
	if !ok {
		buffered = bufio.NewReaderSize(r, 64<<10)
	}
	var line []byte
	for {
		chunk, err := buffered.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > 64<<10 {
			return nil, fmt.Errorf("%w: 下载流首行超过 64 KiB", ErrAgentUnavailable)
		}
		if err == nil {
			return line[:len(line)-1], nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return nil, err
	}
}

// mapError 把代理错误翻译成 files 包的错误。
//
// 三类分开处理，因为接口层对它们的状态码不同：
//   - 连不上（拨号失败、超时、响应不合契约）与代理自己 5xx：文件子系统此刻不可用 → 503；
//   - 代理的明确拒绝（越界、不存在、非文本、版本冲突）：原样带出状态码与码；
//   - 其余：如实包装，接口层按 500 处理。
func (f *Fetcher) mapError(err error, what string) error {
	var unavailable *agentclient.UnavailableError
	if errors.As(err, &unavailable) {
		return f.unavailable(what, err)
	}
	var rejected *agentclient.Error
	if errors.As(err, &rejected) {
		switch {
		case rejected.Status >= http.StatusInternalServerError:
			return f.unavailable(what, err)
		default:
			return &RemoteError{Status: rejected.Status, Code: httpx.Code(rejected.Code), Message: rejected.Message}
		}
	}
	return fmt.Errorf("files: %s 失败: %w", what, err)
}

// unavailable 记录并包装“代理不可用”。
func (f *Fetcher) unavailable(what string, err error) error {
	if f.logger != nil {
		f.logger.Warn(what+"：代理不可用", "error", err)
	}
	return fmt.Errorf("%w: %v", ErrAgentUnavailable, err)
}
