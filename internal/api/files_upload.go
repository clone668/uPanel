package api

// P06.2：流式暂存上传的 Web 入口（CONTRACTS.md 第 11 节）。
//
// 四个路由，一条纪律：**上传不等于落地**。
//
//	POST   /files/uploads             声明根、目标路径、字节数，拿到 upload_id
//	PUT    /files/uploads/:id/content 把字节流送去代理的暂存区
//	GET    /files/uploads/:id         查进度与状态
//	DELETE /files/uploads/:id         放弃这次上传（目标文件一个字节都没变）
//
// 内容收完之后目标路径仍然没变，要变成真文件必须走 file.commit_upload 的预检与任务。
// 这条分界不是形式主义：它让“往服务器放文件”在审计里是一条**操作**，
// 而不是一条“POST 之后什么都可能已经发生”的记录。
//
// 另外三条本层要守的事：
//
//  1. **字节不进内存**。PUT 的请求体直接交给代理客户端边读边转发，
//     这里没有 ioutil.ReadAll，也没有把内容 base64 进 JSON。
//  2. **长度必须自证**。Content-Length 必须等于创建时声明的 size_bytes：
//     一次只发一半的传输不能变成“一半的文件”。分块传输（无 Content-Length）
//     直接拒绝——契约写的是“不支持断点续传”，一个不知道长度的上传连
//     “传完了没有”都无法判定。
//  3. **超时由这两端自己管**。Web 的 http.Server 有 30 秒 ReadTimeout/WriteTimeout，
//     它对整个请求生效；一个几 GiB 的上传会在第 30 秒被掐断。所以这里像
//     日志流那样用 http.ResponseController 自己往后推截止时间。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"upanel/internal/files"
	"upanel/internal/httpx"
)

const (
	// uploadIdleTimeout 是一次上传请求允许“安静”多久。真正的上限是暂存 TTL
	// 与容量预留，不由套接字超时决定。
	uploadIdleTimeout = 60 * time.Second

	// uploadDeadlineRefresh 是推截止时间的节流间隔：每读 64 KiB 就调一次
	// SetReadDeadline 是没必要的系统调用。
	uploadDeadlineRefresh = 5 * time.Second

	// uploadResponseTimeout 是收完之后写响应的时间：这次请求可能已经跑了几分钟，
	// 请求开始时设置的那个写截止时间早就过了。
	uploadResponseTimeout = 30 * time.Second
)

// FilesUploader 是 Web 的暂存上传端（生产实现是 *files.Fetcher）。
//
// 单独一个接口而不是塞进 FilesReader：上传是反方向的一次性写入，
// 把它挂在读取接口上会让每个只想读文件的测试替身都必须实现四个写方法。
// 分成两个接口之后，“这个部署能不能上传”是可判定的——不能时给 501，
// 而不是一个看起来成功的空响应。
type FilesUploader interface {
	CreateUpload(ctx context.Context, params files.CreateUploadParams) (files.UploadInfo, error)
	PutUploadContent(ctx context.Context, uploadID string, expected files.UploadInfo, body io.Reader, size int64) (files.UploadInfo, error)
	GetUpload(ctx context.Context, uploadID string) (files.UploadInfo, error)
	DeleteUpload(ctx context.Context, uploadID string) error
}

// *files.Fetcher 是生产的接线方式（cmd/upanel-web/main.go: filesReader = filesFetcher）。
var _ FilesUploader = (*files.Fetcher)(nil)

// filesUploader 取上传端；未配置返回 503，实现不支持上传返回 501。
func (s *Server) filesUploader(c *gin.Context) (FilesUploader, bool) {
	if s.opts.Files == nil {
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable, filesUnavailable)
		return nil, false
	}
	uploader, ok := s.opts.Files.(FilesUploader)
	if !ok {
		writeError(c, http.StatusNotImplemented, httpx.CodeNotImplemented,
			"当前部署不支持上传：文件端没有提供暂存上传能力")
		return nil, false
	}
	return uploader, true
}

// handleCreateUpload 处理 POST /api/v1/files/uploads。
//
// 请求体就是 files.CreateUploadParams 本身——两个进程、前端与本层共用一套 DTO，
// 不在这里重新定义一份“接口层的上传参数”。
func (s *Server) handleCreateUpload(c *gin.Context) {
	uploader, ok := s.filesUploader(c)
	if !ok {
		return
	}
	var params files.CreateUploadParams
	dec := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, MaxRequestBodyBytes))
	// 未知字段一律拒绝：一个把 size_bytes 写成数字（而不是十进制字符串）
	// 或者把 overwrite 提前塞进来的请求，应当在入口就报错，而不是被静默忽略。
	dec.DisallowUnknownFields()
	if err := dec.Decode(&params); err != nil {
		if IsBodyTooLarge(err) {
			writeError(c, http.StatusRequestEntityTooLarge, httpx.CodeValidationError, "请求体超过大小上限")
			return
		}
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError,
			"请求体不是合法的上传声明 JSON（size_bytes 是十进制字符串，如 \"1048576\"）")
		return
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError, "请求体在首个 JSON 对象之后仍有内容")
		return
	}
	if err := params.Validate(); err != nil {
		s.writeFilesError(c, err, "上传声明不合法")
		return
	}
	info, err := uploader.CreateUpload(c.Request.Context(), params)
	if err != nil {
		s.writeFilesError(c, err, "创建上传失败")
		return
	}
	s.opts.Logger.Info("创建上传暂存",
		"upload_id", info.UploadID, "root_id", info.RootID, "path", info.Path,
		"size_bytes", info.SizeBytes, "expires_at", info.ExpiresAt,
		"actor", sessionFrom(c).User.Username, "request_id", c.GetString(ctxKeyRequestID))
	httpx.WriteData(c.Writer, c.Request, http.StatusCreated, info)
}

// handleUploadContent 处理 PUT /api/v1/files/uploads/:id/content。
//
// 请求体是原始字节（application/octet-stream），不是 JSON、不是 multipart：
// 多一层编码就多一次把 2 GiB 读进内存的机会。
func (s *Server) handleUploadContent(c *gin.Context) {
	uploader, ok := s.filesUploader(c)
	if !ok {
		return
	}
	uploadID := c.Param("id")
	if !files.ValidUploadID(uploadID) {
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError,
			"upload_id 形状不合法（应为 up_ 加 32 位小写十六进制）")
		return
	}
	// 上面这条校验不是多余的：upload_id 会被拼进代理的内部 URL，
	// 一个含 '/' 或 '..' 的 id 是一次路径注入，不能等到拼 URL 时才发现。

	ctx := c.Request.Context()
	info, err := uploader.GetUpload(ctx, uploadID)
	if err != nil {
		s.writeFilesError(c, err, "查询上传暂存失败")
		return
	}
	if info.State != files.UploadStateOpen {
		s.writeFilesError(c, fmt.Errorf("%w: 上传状态是 %q，不能再接收内容", files.ErrUploadState, info.State),
			"上传状态不允许接收内容")
		return
	}

	declared := c.Request.ContentLength
	if declared < 0 {
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError,
			"上传必须声明 Content-Length（不支持分块传输，也不支持断点续传）")
		return
	}
	if uint64(declared) != info.SizeBytes {
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError,
			fmt.Sprintf("本次传输声明 %d 字节，与创建上传时声明的 %d 字节不一致", declared, info.SizeBytes))
		return
	}

	rc := http.NewResponseController(c.Writer)
	if err := rc.SetReadDeadline(time.Now().Add(uploadIdleTimeout)); err != nil {
		// 拿不到 ResponseController 就说明这个 ResponseWriter 不支持延长截止时间
		// （被不透明的中间件包过）。那就不要启动一个注定在第 30 秒被掐断的上传：
		// 让浏览器在传之前看到失败，而不是在传到一半时。
		s.opts.Logger.Error("无法设置上传读截止时间", "error", err,
			"request_id", c.GetString(ctxKeyRequestID))
		writeError(c, http.StatusInternalServerError, httpx.CodeInternalError,
			"服务端无法为长上传延长超时")
		return
	}
	body := &uploadKeepalive{body: c.Request.Body, rc: rc, last: time.Now()}

	started := time.Now()
	updated, err := uploader.PutUploadContent(ctx, uploadID, info, body, declared)
	// 响应也要延长写截止时间：这次请求可能已经跑了几分钟。
	_ = rc.SetWriteDeadline(time.Now().Add(uploadResponseTimeout))
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			s.opts.Logger.Warn("上传内容失败",
				"upload_id", uploadID, "root_id", info.RootID, "path", info.Path,
				"size_bytes", info.SizeBytes, "elapsed", time.Since(started).String(),
				"error", err, "request_id", c.GetString(ctxKeyRequestID))
		}
		s.writeFilesError(c, err, "上传内容失败")
		return
	}
	s.opts.Logger.Info("上传内容收完",
		"upload_id", updated.UploadID, "root_id", updated.RootID, "path", updated.Path,
		"size_bytes", updated.SizeBytes, "elapsed", time.Since(started).String(),
		"actor", sessionFrom(c).User.Username, "request_id", c.GetString(ctxKeyRequestID))
	// 收完只是“暂存里有了完整字节”，目标路径仍然没变：落地要 file.commit_upload。
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, updated)
}

// handleGetUpload 处理 GET /api/v1/files/uploads/:id。
func (s *Server) handleGetUpload(c *gin.Context) {
	uploader, ok := s.filesUploader(c)
	if !ok {
		return
	}
	info, err := uploader.GetUpload(c.Request.Context(), c.Param("id"))
	if err != nil {
		s.writeFilesError(c, err, "查询上传暂存失败")
		return
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, info)
}

// handleDeleteUpload 处理 DELETE /api/v1/files/uploads/:id。
//
// 只删暂存，不碰目标文件：这个接口存在的意义是“我放弃这次上传了，把空间还回来”。
func (s *Server) handleDeleteUpload(c *gin.Context) {
	uploader, ok := s.filesUploader(c)
	if !ok {
		return
	}
	uploadID := c.Param("id")
	if err := uploader.DeleteUpload(c.Request.Context(), uploadID); err != nil {
		s.writeFilesError(c, err, "放弃上传暂存失败")
		return
	}
	s.opts.Logger.Info("放弃上传暂存",
		"upload_id", uploadID, "actor", sessionFrom(c).User.Username,
		"request_id", c.GetString(ctxKeyRequestID))
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, gin.H{"upload_id": uploadID, "state": "deleted"})
}

// uploadKeepalive 在每次读到数据之后把读截止时间往后推。
//
// 只设一次是不够的：60 秒的“安静上限”对一个几 GiB 的慢速上传仍然会在中途到期，
// 而契约要的是“传得慢可以，停住不行”。
type uploadKeepalive struct {
	body io.Reader
	rc   *http.ResponseController
	last time.Time
}

// Read 实现 io.Reader。
func (u *uploadKeepalive) Read(p []byte) (int, error) {
	n, err := u.body.Read(p)
	if n > 0 {
		if now := time.Now(); now.Sub(u.last) >= uploadDeadlineRefresh {
			if deadlineErr := u.rc.SetReadDeadline(now.Add(uploadIdleTimeout)); deadlineErr != nil && err == nil {
				// 推不动截止时间就如实失败：继续读下去只会在超时时刻留下半截上传。
				return n, deadlineErr
			}
			u.last = now
		}
	}
	return n, err
}
