package agent

// P06.2：流式暂存上传的代理侧入口（CONTRACTS.md 第 11 节）。
//
// 三个接口，一条纪律：
//
//	POST   /internal/v1/uploads            创建暂存：声明根、目标路径、字节数
//	PUT    /internal/v1/uploads/:id/content 把字节流送进暂存区
//	GET    /internal/v1/uploads/:id         查进度与状态
//	DELETE /internal/v1/uploads/:id         放弃这次上传
//
// 纪律是：**上传统统不等于落地**。内容收完之后目标路径一个字节都没变，
// 要变成真文件必须走 file.commit_upload 的预检与任务。把这两步分开，
// 是为了让“往服务器放一个文件”和“改服务器上的一个文件”在审计里是两条记录，
// 而不是一条“POST 之后什么都可能已经发生”的记录。
//
// 字节不进内存：PUT 的请求体直接交给 files.Manager 边读边写边算摘要。
// 一个 2 GiB 的上传在代理里的常驻内存是 64 KiB 的复制缓冲。

import (
	"context"
	"io"
	"net/http"
	"strings"
	"time"

	"upanel/internal/files"
	"upanel/internal/httpx"
)

const (
	// uploadIdleTimeout 是一次上传请求允许“安静”多久。
	//
	// 代理的 http.Server 有 15 秒 ReadTimeout，它对整个请求体生效——一个正在
	// 正常传输的 2 GiB 上传会在第 15 秒被掐断。所以这里像流那样自己管截止时间：
	// 每读到一批数据就把“还能安静多久”往后推。真正的上限由 files.UploadTTL
	// 与容量预留管，不由一个套接字超时管。
	uploadIdleTimeout = 60 * time.Second

	// stagingSweepInterval 是清理过期暂存的间隔。
	stagingSweepInterval = 15 * time.Minute
)

// handleUploads 处理 POST /internal/v1/uploads。
func (s *Server) handleUploads(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		httpx.WriteError(w, r, http.StatusMethodNotAllowed, httpx.CodeValidationError, "只支持 POST")
		return
	}
	if s.opts.FileWrites == nil {
		httpx.WriteError(w, r, http.StatusNotImplemented, httpx.CodeNotImplemented, "文件写入能力未启用")
		return
	}
	if ct := r.Header.Get("Content-Type"); ct != "" && !jsonContentType(ct) {
		httpx.WriteError(w, r, http.StatusUnsupportedMediaType, httpx.CodeValidationError,
			"请求体必须是 application/json")
		return
	}
	body, ok := readLimitedBody(w, r, maxQueryRequestBytes)
	if !ok {
		return
	}
	var params files.CreateUploadParams
	if err := decodeStrictObject(body, &params); err != nil {
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationError, err.Error())
		return
	}
	// 形状先在这里拒：一个缺 root_id 或 size_bytes 为 0 的声明不该走到
	// “预留磁盘空间”那一步去失败。Web 层也校验同一份 Params.Validate，
	// 但代理不能假设调用方是 Web。
	if err := params.Validate(); err != nil {
		s.writeUploadError(w, r, err, "创建暂存上传")
		return
	}
	info, err := s.opts.FileWrites.CreateUpload(r.Context(), params)
	if err != nil {
		s.writeUploadError(w, r, err, "创建暂存上传")
		return
	}
	s.opts.Logger.Info("创建暂存上传",
		"upload_id", info.UploadID, "root_id", info.RootID, "path", info.Path,
		"size_bytes", info.SizeBytes, "expires_at", info.ExpiresAt,
		"request_id", httpx.RequestID(r))
	httpx.WriteData(w, r, http.StatusCreated, info)
}

// handleUploadByIDOrContent 把 /internal/v1/uploads/<id>[/content] 分给两个处理器。
//
// 用前缀路由而不是路径参数：代理的 mux 是标准库的 ServeMux，它按最长前缀匹配，
// 这里只需要再判断一次“后缀是不是 content”。
func (s *Server) handleUploadByIDOrContent(w http.ResponseWriter, r *http.Request) {
	if _, rest := splitUploadPath(r.URL.Path); rest != "" {
		s.handleUploadContent(w, r)
		return
	}
	s.handleUploadByID(w, r)
}

// handleUploadByID 处理 GET/DELETE /internal/v1/uploads/:id。
func (s *Server) handleUploadByID(w http.ResponseWriter, r *http.Request) {
	if s.opts.FileWrites == nil {
		httpx.WriteError(w, r, http.StatusNotImplemented, httpx.CodeNotImplemented, "文件写入能力未启用")
		return
	}
	id, rest := splitUploadPath(r.URL.Path)
	if id == "" || rest != "" {
		httpx.WriteError(w, r, http.StatusNotFound, httpx.CodeNotFound, "上传不存在")
		return
	}
	switch r.Method {
	case http.MethodGet:
		info, err := s.opts.FileWrites.GetUpload(r.Context(), id)
		if err != nil {
			s.writeUploadError(w, r, err, "查询暂存上传")
			return
		}
		httpx.WriteData(w, r, http.StatusOK, info)
	case http.MethodDelete:
		if err := s.opts.FileWrites.DeleteUpload(r.Context(), id); err != nil {
			s.writeUploadError(w, r, err, "放弃暂存上传")
			return
		}
		s.opts.Logger.Info("放弃暂存上传", "upload_id", id, "request_id", httpx.RequestID(r))
		httpx.WriteData(w, r, http.StatusOK, map[string]string{"upload_id": id, "state": "deleted"})
	default:
		w.Header().Set("Allow", "GET, DELETE")
		httpx.WriteError(w, r, http.StatusMethodNotAllowed, httpx.CodeValidationError, "只支持 GET 与 DELETE")
	}
}

// handleUploadContent 处理 PUT /internal/v1/uploads/:id/content。
func (s *Server) handleUploadContent(w http.ResponseWriter, r *http.Request) {
	if s.opts.FileWrites == nil {
		httpx.WriteError(w, r, http.StatusNotImplemented, httpx.CodeNotImplemented, "文件写入能力未启用")
		return
	}
	if r.Method != http.MethodPut {
		w.Header().Set("Allow", "PUT")
		httpx.WriteError(w, r, http.StatusMethodNotAllowed, httpx.CodeValidationError, "只支持 PUT")
		return
	}
	id, rest := splitUploadPath(r.URL.Path)
	if id == "" || rest != "content" {
		httpx.WriteError(w, r, http.StatusNotFound, httpx.CodeNotFound, "上传不存在")
		return
	}

	rc := http.NewResponseController(w)
	if err := rc.SetReadDeadline(time.Now().Add(uploadIdleTimeout)); err != nil {
		// 拿不到 ResponseController 说明这个 ResponseWriter 不支持延长截止时间
		// （被中间件包过），那就不该在这里冒险启动一个可能被 15 秒掐断的上传。
		s.opts.Logger.Error("无法设置上传读截止时间", "error", err, "request_id", httpx.RequestID(r))
		httpx.WriteError(w, r, http.StatusInternalServerError, httpx.CodeInternalError,
			"服务端无法为长上传延长超时")
		return
	}
	body := &deadlineReader{body: r.Body, rc: rc}

	info, err := s.opts.FileWrites.WriteUploadContent(r.Context(), id, body)
	// 响应也要延长写截止时间：这个请求可能已经跑了几分钟，原来的 15 秒早就过了。
	_ = rc.SetWriteDeadline(time.Now().Add(streamWriteTimeout))
	if err != nil {
		s.writeUploadError(w, r, err, "接收上传内容")
		return
	}
	s.opts.Logger.Info("暂存上传收完",
		"upload_id", info.UploadID, "root_id", info.RootID, "path", info.Path,
		"size_bytes", info.SizeBytes, "received_bytes", info.ReceivedBytes,
		"request_id", httpx.RequestID(r))
	httpx.WriteData(w, r, http.StatusOK, info)
}

// splitUploadPath 把 /internal/v1/uploads/<id>[/<rest>] 拆成两段。
func splitUploadPath(p string) (string, string) {
	tail := strings.TrimPrefix(p, "/internal/v1/uploads/")
	if tail == "" || tail == p {
		return "", ""
	}
	if i := strings.IndexByte(tail, '/'); i >= 0 {
		return tail[:i], tail[i+1:]
	}
	return tail, ""
}

// writeUploadError 把 files 的错误翻成响应。
//
// 判决来自 files.HTTPStatusOf（代理与 Web 共用同一张表）；没被判定的错误回 500
// 并记日志——一次没被分类的失败应该看起来像失败，而不是被兜成一个 200。
func (s *Server) writeUploadError(w http.ResponseWriter, r *http.Request, err error, what string) {
	if carrier, ok := statusCarrierOf(err); ok {
		status, code := carrier.HTTPStatus()
		httpx.WriteError(w, r, status, code, err.Error())
		return
	}
	// files 的哨兵错误（ErrUnknownRoot、ErrNotWritable、ErrNoSpace、ErrUploadState……）
	// 本身不是 *HTTPError，用同一张映射表判决：把“这个根登记为只读”报成 500 会让人
	// 去查代理日志，而正确的动作是换一个根。Web 侧的 writeFilesError 走的是同一张表。
	if mapped, ok := files.HTTPStatusOf(err); ok {
		httpx.WriteError(w, r, mapped.Status, mapped.Code, mapped.Message)
		return
	}
	s.opts.Logger.Error(what+"失败", "error", err, "request_id", httpx.RequestID(r))
	httpx.WriteError(w, r, http.StatusInternalServerError, httpx.CodeInternalError, what+"失败")
}

// deadlineReader 每读到一批数据就把读截止时间往后推。
//
// 只用一次 SetReadDeadline 是不够的：60 秒的“安静上限”对一个几 GiB 的慢速上传
// 来说仍然会在中途到期。这里按“上次推迟已经过去 5 秒”来节流，避免每读 64 KiB
// 就做一次系统调用。
type deadlineReader struct {
	body   io.Reader
	rc     *http.ResponseController
	lastOK time.Time
}

// Read 实现 io.Reader。
func (d *deadlineReader) Read(p []byte) (int, error) {
	if d.lastOK.IsZero() || time.Since(d.lastOK) > 5*time.Second {
		if err := d.rc.SetReadDeadline(time.Now().Add(uploadIdleTimeout)); err != nil {
			return 0, err
		}
		d.lastOK = time.Now()
	}
	return d.body.Read(p)
}

// runStagingSweeper 周期清理过期暂存。
//
// 清理失败不退出：一次扫描赶上有目录在被并发删除是正常的，
// 下一个周期会重来。但每次失败都要留痕，否则“暂存越攒越多”会变成无声的。
func (s *Server) runStagingSweeper(ctx context.Context) {
	ticker := time.NewTicker(stagingSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if s.opts.FileWrites == nil {
				continue
			}
			removed, err := s.opts.FileWrites.SweepStaging(ctx, now.UTC())
			if err != nil {
				s.opts.Logger.Warn("清理过期暂存失败（下个周期重试）", "error", err)
				continue
			}
			if removed > 0 {
				s.opts.Logger.Info("清理了过期的暂存上传", "count", removed)
			}
		}
	}
}
