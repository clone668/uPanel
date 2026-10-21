package api

// P06.1：文件管理 API（CONTRACTS.md 第 11 节）。
//
// 四条纪律：
//
//  1. **整组 admin**。文件内容就是服务器内容：配置里的凭据、别人的数据、应用日志。
//     PLAN.md 第 15 节把文件管理列为管理员能力，viewer 在这里得到 403，
//     而不是“去掉几行的只读视图”。
//
//  2. **根来自配置**。请求里只有 root_id 与根内相对路径；没有绝对路径字段。
//     未登记的 root_id 是 404，不会退回到“按路径找找看”。
//
//  3. **代理的判决原样转出**。越界是 PATH_OUTSIDE_ROOT，不是 AGENT_UNAVAILABLE；
//     本层只补一个兜底（代理真的连不上才是 503），不做二次归类。
//
//  4. **下载不经过 JSON**。文件字节从代理流直接进响应体，Content-Disposition
//     用 RFC 5987 编码，Content-Type 固定 octet-stream 且 nosniff：
//     浏览器永远不把这里的内容当成 HTML/脚本渲染。

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"upanel/internal/files"
	"upanel/internal/httpx"
)

// FilesReader 是 Web 的文件读取端（生产实现是 *files.Fetcher）。
//
// 用接口而不是直接依赖 *files.Fetcher：测试可以注入一个只回答“代理不可用”的实现，
// 用来证明 503 分支，而不必起一个真代理。
type FilesReader interface {
	Roots(ctx context.Context) (files.RootsResult, error)
	List(ctx context.Context, params files.ListParams) (files.ListPage, error)
	Text(ctx context.Context, params files.TextParams) (files.TextFile, error)
	OpenDownload(ctx context.Context, params files.DownloadParams) (*files.RemoteDownload, error)
}

// filesUnavailable 是“没有配置代理”时的统一回答。
const filesUnavailable = "文件管理不可用：未配置代理连接"

// handleFileRoots 处理 GET /api/v1/files/roots。
//
// 返回的是**已登记的根**，不是文件系统里的挂载点：界面据此渲染根选择器，
// 因此它同时也是“当前能管理什么”的唯一来源。没有登记根时返回空列表 + 说明，
// 而不是一个空的下拉框（那样看起来像加载失败）。
func (s *Server) handleFileRoots(c *gin.Context) {
	reader, ok := s.filesReader(c)
	if !ok {
		return
	}
	result, err := reader.Roots(c.Request.Context())
	if err != nil {
		s.writeFilesError(c, err, "列出文件根失败")
		return
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, result)
}

// handleFileList 处理 GET /api/v1/files/list?root_id=&path=&cursor=。
func (s *Server) handleFileList(c *gin.Context) {
	reader, ok := s.filesReader(c)
	if !ok {
		return
	}
	params := files.ListParams{
		RootID: strings.TrimSpace(c.Query("root_id")),
		Path:   c.Query("path"),
		Cursor: c.Query("cursor"),
	}
	if err := params.Validate(); err != nil {
		// 走与代理判决同一条映射：越界路径是 422 PATH_OUTSIDE_ROOT，
		// 形状问题才是 400 VALIDATION_ERROR。
		s.writeFilesError(c, err, "文件参数不合法")
		return
	}
	page, err := reader.List(c.Request.Context(), params)
	if err != nil {
		s.writeFilesError(c, err, "列出目录失败")
		return
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, page)
}

// handleFileText 处理 GET /api/v1/files/text?root_id=&path=&expected_version=。
func (s *Server) handleFileText(c *gin.Context) {
	reader, ok := s.filesReader(c)
	if !ok {
		return
	}
	params := files.TextParams{
		RootID:          strings.TrimSpace(c.Query("root_id")),
		Path:            c.Query("path"),
		ExpectedVersion: strings.TrimSpace(c.Query("expected_version")),
	}
	if err := params.Validate(); err != nil {
		// 走与代理判决同一条映射：越界路径是 422 PATH_OUTSIDE_ROOT，
		// 形状问题才是 400 VALIDATION_ERROR。
		s.writeFilesError(c, err, "文件参数不合法")
		return
	}
	text, err := reader.Text(c.Request.Context(), params)
	if err != nil {
		s.writeFilesError(c, err, "读取文件文本失败")
		return
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, text)
}

// handleFileDownload 处理 GET /api/v1/files/download?root_id=&path=。
//
// 响应体是文件原始字节。元数据（名字、大小、version）来自代理流的首行，
// 因此 Content-Length 与 Content-Disposition 描述的就是**这一份**字节。
func (s *Server) handleFileDownload(c *gin.Context) {
	reader, ok := s.filesReader(c)
	if !ok {
		return
	}
	params := files.DownloadParams{
		RootID:          strings.TrimSpace(c.Query("root_id")),
		Path:            c.Query("path"),
		ExpectedVersion: strings.TrimSpace(c.Query("expected_version")),
	}
	if err := params.Validate(); err != nil {
		// 走与代理判决同一条映射：越界路径是 422 PATH_OUTSIDE_ROOT，
		// 形状问题才是 400 VALIDATION_ERROR。
		s.writeFilesError(c, err, "文件参数不合法")
		return
	}
	download, err := reader.OpenDownload(c.Request.Context(), params)
	if err != nil {
		s.writeFilesError(c, err, "打开下载失败")
		return
	}
	defer func() { _ = download.Close() }()

	h := c.Writer.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Disposition", contentDisposition(download.Header.Name))
	h.Set("Content-Length", strconv.FormatUint(download.Header.SizeBytes, 10))
	h.Set("Cache-Control", "no-store")
	// 版本信息也发给前端：下载完再回到列表时，界面能判断“下载的是不是当前这一份”。
	h.Set("X-UPanel-File-Version", download.Header.Version)

	c.Writer.WriteHeader(http.StatusOK)
	written, copyErr := io.Copy(c.Writer, download.Reader)
	if copyErr != nil {
		// 客户端断开或代理中途失败。这时响应头已经发出去了，能做的只有如实记日志：
		// 浏览器看到的是“文件没下完”，而不是一段被当成文件内容的错误 JSON。
		s.opts.Logger.Warn("下载中断",
			"root_id", params.RootID, "path", params.Path,
			"written_bytes", written, "size_bytes", download.Header.SizeBytes,
			"error", copyErr, "request_id", c.GetString(ctxKeyRequestID))
	}
}

// filesReader 取文件读取端；未配置时直接写 503 并返回 false。
func (s *Server) filesReader(c *gin.Context) (FilesReader, bool) {
	if s.opts.Files == nil {
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable, filesUnavailable)
		return nil, false
	}
	// *files.Fetcher 满足 FilesReader；显式转换让“接口满足关系”在编译期就被检查。
	var reader FilesReader = s.opts.Files
	return reader, true
}

// writeFilesError 把文件读取的错误翻成 HTTP 响应。
func (s *Server) writeFilesError(c *gin.Context, err error, what string) {
	if errors.Is(err, context.Canceled) {
		return
	}
	var remote *files.RemoteError
	if errors.As(err, &remote) {
		// 代理的明确拒绝：状态码与码原样转出。越界不能因为经过 Web 就变成“服务坏了”。
		writeError(c, remote.Status, remote.Code, remote.Message)
		return
	}
	// 本地的形状校验与代理的判决走**同一张**映射表：同一个 `..` 在 Web 层被拦下
	// 和在代理层被拦下必须是同一个状态码、同一个错误码。否则前端要为同一件事写
	// 两套处理，而其中一套永远收不到（P06.1 验收就是在 VM 上抓到这一点的：
	// 越界路径被 Web 提前拦成 400 VALIDATION_ERROR，前端于是永远不会显示
	// “路径在文件根之外”）。
	if mapped, ok := files.HTTPStatusOf(err); ok {
		writeError(c, mapped.Status, mapped.Code, mapped.Message)
		return
	}
	switch {
	case errors.Is(err, files.ErrAgentUnavailable):
		s.opts.Logger.Warn(what+"：代理不可用", "error", err,
			"request_id", c.GetString(ctxKeyRequestID))
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"代理不可用，无法完成文件操作")
	default:
		s.opts.Logger.Error(what, "error", err, "request_id", c.GetString(ctxKeyRequestID))
		writeError(c, http.StatusInternalServerError, httpx.CodeInternalError, what)
	}
}

// contentDisposition 生成安全的 Content-Disposition 值。
//
// 两个都要给：filename 是 ASCII 退化形式（老浏览器与某些客户端只认它），
// filename* 是 RFC 5987 的 UTF-8 百分号编码（中文名靠它才能正确落盘）。
// 控制字符与引号一律替换掉——文件名来自服务器文件系统，但它会进入响应头，
// 而响应头里的换行意味着响应拆分。
func contentDisposition(name string) string {
	// 按 rune 而不是按字节遍历：一个中文字符是一个字符，退化形式里也只该占一个 '_'，
	// 而不是三个（"______.conf" 这种名字既难读又像乱码）。
	ascii := make([]byte, 0, len(name))
	for _, r := range name {
		switch {
		case r < 0x20 || r == 0x7f:
			// 控制字符：直接丢掉（换行/回车是对响应头的攻击面，其它也无可显示意义）。
		case r == '"' || r == '\\':
			ascii = append(ascii, '_')
		case r < 0x80:
			ascii = append(ascii, byte(r))
		default:
			ascii = append(ascii, '_')
		}
	}
	if len(ascii) == 0 {
		ascii = []byte("download")
	}
	return `attachment; filename="` + string(ascii) + `"; filename*=UTF-8''` + percentEncodeRFC5987(name)
}

// percentEncodeRFC5987 按 RFC 5987 attr-char 规则编码。
//
// 只保留 attr-char（字母数字与 !#$&+-.^_`|~），其余一律百分号编码：
// 这样括号、引号、分号、逗号、空格都不会破坏 Content-Disposition 的语法。
func percentEncodeRFC5987(value string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(value) * 3)
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			b.WriteByte(c)
		case strings.IndexByte("!#$&+-.^_`|~", c) >= 0:
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&0x0F])
		}
	}
	return b.String()
}
