package api

// 证书的只读视图与公钥证书上传（P10.3）。
//
// 这一层只做三件事，边界写清楚是因为"证书"这个词很容易把别的东西一起带进来：
//
//  1. 读面板库里的证书记录（列表、详情）；
//  2. 需要现场事实时，向代理问一句 certificate.state（到期、签发者、盘上是不是
//     还是那一张），读不到就如实说读不到；
//  3. 接收一份**公钥**证书 PEM，暂存在内存里十分钟，交给随后的 certificate.import
//     任务展开（CONTRACTS.md 第 9 节）。
//
// **私钥从不经过这里。** 它走 POST /secrets（用途 tls_private_key），进代理的加密
// 秘密存储；导入任务里只有 secret_ref。因此本文件的任何 DTO 都没有 key_path、
// key_secret_ref 或内容字段——不是"忘了加"，是不该有。

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"upanel/internal/audit"
	"upanel/internal/certificates"
	"upanel/internal/httpx"
	"upanel/internal/tlscert"
)

const (
	// uploadFieldName 是上传表单里的文件字段名。
	uploadFieldName = "certificate"
	// maxUploadFormBytes 是 multipart 请求体的上限：1 MiB 的证书 + 表单边界的余量。
	maxUploadFormBytes int64 = tlscert.MaxCertBytes + 64<<10
	// maxUploadMemory 是解析 multipart 时留在内存里的上限（超出的部分 gin 会写临时
	// 文件，而 1 MiB 以内的证书永远碰不到这条线）。
	maxUploadMemory int64 = tlscert.MaxCertBytes + 64<<10
)

// CertificateStateReader 读取主机上证书的现场事实（P10.3）。
//
// 生产实现是 *certificates.AgentInspector。允许为 nil（演示模式不连代理）：
// 那时详情页的 observed 明确写 available=false 与原因，而不是给一份"证书不存在"的
// 空事实——后者会被读成"这张证书被删了"。
type CertificateStateReader interface {
	Inspect(ctx context.Context, domain, source string) (certificates.Inspected, error)
}

// certificateDTO 是证书记录的公开形态（CONTRACTS.md 第 9 节 CertificateSummary）。
//
// 没有 key_path、没有 key_secret_ref、没有 cert_pem。key_storage 只说明"私钥由谁
// 保管"，用来回答界面上那个很自然的问题——"续期时我还要再传一次私钥吗"。
type certificateDTO struct {
	ID          string   `json:"id"`
	Domain      string   `json:"domain"`
	Domains     []string `json:"domains"`
	Source      string   `json:"source"`
	Staging     bool     `json:"staging"`
	Issuer      string   `json:"issuer"`
	Subject     string   `json:"subject"`
	NotBefore   string   `json:"not_before"`
	NotAfter    string   `json:"not_after"`
	Fingerprint string   `json:"fingerprint"`
	// RenewalState 是派生值：valid / expiring / expired / unknown。
	RenewalState string `json:"renewal_state"`
	// KeyStorage 是私钥的保管方：certbot（Certbot 自己管）或 secret_store
	// （经 POST /secrets 存进代理加密库）。
	KeyStorage    string `json:"key_storage"`
	ACMEEmail     string `json:"acme_email,omitempty"`
	LastAttemptAt string `json:"last_attempt_at,omitempty"`
	LastError     string `json:"last_error,omitempty"`
	Version       int    `json:"version"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
}

// certificateObservedDTO 是"盘上现在是什么样"。
//
// available=false 时只有 reason 有意义：其余字段是零值，界面必须据此显示"不知道"，
// 而不是显示"证书不存在、有效期为零"。
type certificateObservedDTO struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
	// Inspected 平铺展开（它自己没有任何路径字段）。
	certificates.Inspected
}

// certificateDetailResponse 是 GET /certificates/:id 的响应体。
type certificateDetailResponse struct {
	Certificate certificateDTO          `json:"certificate"`
	Observed    *certificateObservedDTO `json:"observed,omitempty"`
}

// certificateListResponse 是 GET /certificates 的响应体。
type certificateListResponse struct {
	Items      []certificateDTO `json:"items"`
	NextCursor *string          `json:"next_cursor"`
}

// certificateUploadResponse 是 POST /certificates/uploads 的响应体。
//
// 只有标识与这份上传的解析结果，没有 PEM 内容：上传方刚给过它，不需要再读一遍。
type certificateUploadResponse struct {
	CertificateUploadID string   `json:"certificate_upload_id"`
	Domains             []string `json:"domains"`
	Issuer              string   `json:"issuer"`
	Subject             string   `json:"subject"`
	NotBefore           string   `json:"not_before"`
	NotAfter            string   `json:"not_after"`
	Fingerprint         string   `json:"fingerprint"`
	Staging             bool     `json:"staging"`
	SelfSigned          bool     `json:"self_signed"`
	ExpiresAt           string   `json:"expires_at"`
}

// handleListCertificates 处理 GET /api/v1/certificates（admin）。
func (s *Server) handleListCertificates(c *gin.Context) {
	limit, ok := listLimitMax(c, certificates.MaxListLimit)
	if !ok {
		return
	}
	opts := certificates.ListOptions{Limit: limit, Cursor: c.Query("cursor")}
	if raw := strings.TrimSpace(c.Query("domain")); raw != "" {
		// 库里的 domain 是归一化后的 A-label。归一化失败是 400，不是"没找到"。
		domain, err := normalizeCertificateDomain(raw)
		if err != nil {
			writeError(c, http.StatusBadRequest, httpx.CodeValidationError, err.Error())
			return
		}
		opts.Domain = domain
	}
	items, next, err := s.opts.Certificates.List(c.Request.Context(), opts)
	if err != nil {
		s.writeCertificateError(c, "读取证书列表失败", err)
		return
	}
	resp := certificateListResponse{Items: make([]certificateDTO, 0, len(items))}
	now := time.Now().UTC()
	for _, item := range items {
		resp.Items = append(resp.Items, certificateDTOOf(item, now))
	}
	if next != "" {
		resp.NextCursor = &next
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, resp)
}

// handleGetCertificate 处理 GET /api/v1/certificates/:id（admin）。
//
// observed 是附加信息：代理读不到时它变成 available=false，**但详情本身仍然是 200**。
// 这是有意的——库里那条记录还在，为了"读不到现场"而整页 503，会把用户已经掌握的
// 事实（这张证书覆盖什么、绑定在哪个域）一起丢掉。
func (s *Server) handleGetCertificate(c *gin.Context) {
	cert, err := s.opts.Certificates.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		s.writeCertificateError(c, "读取证书失败", err)
		return
	}
	resp := certificateDetailResponse{Certificate: certificateDTOOf(cert, time.Now().UTC())}
	resp.Observed = s.certificateObserved(c.Request.Context(), cert, c.GetString(ctxKeyRequestID))
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, resp)
}

// handleUploadCertificate 处理 POST /api/v1/certificates/uploads（admin + CSRF + 再认证）。
//
// 只暂存。导入生效必须执行 certificate.import 任务——所以这里既不写库，也不碰主机。
func (s *Server) handleUploadCertificate(c *gin.Context) {
	const action = audit.ActionCertificateUpload
	sess := sessionFrom(c)

	// 先按 Content-Length 挡一次（解析之前就能拒绝的东西不该先读进内存）。
	if c.Request.ContentLength > maxUploadFormBytes {
		s.recordDenied(c, &sess, action, "certificate:upload", "validation", httpx.CodeValidationError)
		writeError(c, http.StatusRequestEntityTooLarge, httpx.CodeValidationError,
			"上传内容超过 1 MiB：这里只接收公钥证书（PEM）")
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxUploadFormBytes)
	if err := c.Request.ParseMultipartForm(maxUploadMemory); err != nil {
		s.recordDenied(c, &sess, action, "certificate:upload", "validation", httpx.CodeValidationError)
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError,
			"请求不是合法的 multipart 表单："+err.Error())
		return
	}
	defer func() {
		// 临时文件（如果有）必须清掉：这是唯一会在磁盘上留下痕迹的中间态。
		if c.Request.MultipartForm != nil {
			_ = c.Request.MultipartForm.RemoveAll()
		}
	}()

	file, header, err := c.Request.FormFile(uploadFieldName)
	if err != nil {
		s.recordDenied(c, &sess, action, "certificate:upload", "validation", httpx.CodeValidationError)
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError,
			"缺少文件字段 "+uploadFieldName+"（单个 PEM 公钥证书）")
		return
	}
	defer func() { _ = file.Close() }()
	if header != nil && header.Size > tlscert.MaxCertBytes {
		s.recordDenied(c, &sess, action, "certificate:upload", "validation", httpx.CodeValidationError)
		writeError(c, http.StatusRequestEntityTooLarge, httpx.CodeValidationError,
			"证书文件超过 1 MiB")
		return
	}
	// 多读一个字节用来判断"是不是超了"，比事后猜长度可靠。
	body, err := io.ReadAll(io.LimitReader(file, tlscert.MaxCertBytes+1))
	if err != nil {
		s.internalError(c, "读取上传内容失败", err)
		return
	}
	if int64(len(body)) > tlscert.MaxCertBytes {
		s.recordDenied(c, &sess, action, "certificate:upload", "validation", httpx.CodeValidationError)
		writeError(c, http.StatusRequestEntityTooLarge, httpx.CodeValidationError,
			"证书文件超过 1 MiB")
		return
	}

	// 夹带私钥是最常见的误操作（fullchain + privkey 一起粘进上传框）。
	// 明确拒绝，不"忽略多余的块"：那样私钥就已经进了这个进程的内存与可能的日志。
	if tlscert.ContainsPrivateKey(body) {
		s.recordDenied(c, &sess, action, "certificate:upload", "validation", httpx.CodeValidationError)
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError,
			"上传内容里有私钥：这里只接收公钥证书链。私钥请经 POST /secrets（用途 tls_private_key）上传，"+
				"导入时用 secret_ref 引用它")
		return
	}

	staged, err := s.opts.CertificateStages.Put(sess.User.ID, sess.BindingHash(), body)
	if err != nil {
		switch {
		case errors.Is(err, certificates.ErrStageTooLarge):
			writeError(c, http.StatusRequestEntityTooLarge, httpx.CodeValidationError, err.Error())
		case errors.Is(err, certificates.ErrStageFull):
			writeError(c, http.StatusTooManyRequests, httpx.CodeResourceBusy, err.Error())
		default:
			// 解析失败属于用户输入问题（不是 PEM、没有证书块、是 CA 证书……）。
			s.recordDenied(c, &sess, action, "certificate:upload", "validation", httpx.CodeValidationError)
			writeError(c, http.StatusBadRequest, httpx.CodeValidationError, "证书无法解析："+err.Error())
		}
		return
	}

	// 审计：记域名、指纹与有效期，不记内容。
	if err := s.recordAuditInTx(c.Request.Context(), c, sess, action,
		"certificate_upload:"+staged.ID, map[string]any{
			"upload_id":   staged.ID,
			"domains":     staged.Info.Domains,
			"issuer":      staged.Info.Issuer,
			"fingerprint": staged.Info.Fingerprint,
			"not_after":   staged.Info.NotAfter.UTC().Format(time.RFC3339),
			"staging":     staged.Info.Staging,
			"expires_at":  staged.ExpiresAt.UTC().Format(time.RFC3339),
		}); err != nil {
		s.opts.Logger.Error("审计写入失败（证书已暂存）", "error", err,
			"request_id", c.GetString(ctxKeyRequestID))
		writeError(c, http.StatusInternalServerError, httpx.CodeInternalError,
			"证书已暂存，但审计未写入：请联系管理员检查审计存储")
		return
	}

	httpx.WriteData(c.Writer, c.Request, http.StatusCreated, certificateUploadResponse{
		CertificateUploadID: staged.ID,
		Domains:             staged.Info.Domains,
		Issuer:              staged.Info.Issuer,
		Subject:             staged.Info.Subject,
		NotBefore:           staged.Info.NotBefore.UTC().Format(time.RFC3339),
		NotAfter:            staged.Info.NotAfter.UTC().Format(time.RFC3339),
		Fingerprint:         staged.Info.Fingerprint,
		Staging:             staged.Info.Staging,
		SelfSigned:          staged.Info.SelfSigned,
		ExpiresAt:           staged.ExpiresAt.UTC().Format(time.RFC3339),
	})
}

// certificateObserved 读取一份现场事实。任何失败都变成"读不到 + 原因"。
func (s *Server) certificateObserved(ctx context.Context, cert certificates.Certificate, requestID string) *certificateObservedDTO {
	if s.opts.CertificateState == nil {
		return &certificateObservedDTO{
			Available: false,
			Reason:    "未配置代理连接：面板读不到这台机器上的证书文件",
		}
	}
	observed, err := s.opts.CertificateState.Inspect(ctx, cert.Domain, string(cert.Source))
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil
		}
		s.opts.Logger.Warn("读取证书现场失败", "certificate_id", cert.ID, "error", err,
			"request_id", requestID)
		return &certificateObservedDTO{
			Available: false,
			Reason:    "代理不可用，读不到证书文件：" + err.Error(),
		}
	}
	return &certificateObservedDTO{Available: true, Inspected: observed}
}

// certificateDTOOf 把一条记录转成 DTO。
func certificateDTOOf(cert certificates.Certificate, now time.Time) certificateDTO {
	domains := cert.Domains
	if domains == nil {
		domains = []string{}
	}
	dto := certificateDTO{
		ID:           cert.ID,
		Domain:       cert.Domain,
		Domains:      domains,
		Source:       string(cert.Source),
		Staging:      cert.Staging,
		Issuer:       cert.Issuer,
		Subject:      cert.Subject,
		NotBefore:    cert.NotBefore.UTC().Format(time.RFC3339),
		NotAfter:     cert.NotAfter.UTC().Format(time.RFC3339),
		Fingerprint:  cert.Fingerprint,
		RenewalState: string(cert.State(now)),
		ACMEEmail:    cert.ACMEEmail,
		LastError:    cert.LastError,
		Version:      cert.Version,
		CreatedAt:    cert.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:    cert.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if cert.Source == certificates.SourceCertbot {
		dto.KeyStorage = "certbot"
	} else {
		dto.KeyStorage = "secret_store"
	}
	if !cert.LastAttemptAt.IsZero() {
		dto.LastAttemptAt = cert.LastAttemptAt.UTC().Format(time.RFC3339)
	}
	return dto
}

// writeCertificateError 把证书服务的错误映射成 HTTP 判决。
//
// 与 writeProxyError 同一套规则：能在库里判定的都是 4xx（带得上字段或结论），
// 判定不了的才是 500。压成 500 会把"这张证书不覆盖那个域名"显示成"面板坏了"。
func (s *Server) writeCertificateError(c *gin.Context, what string, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	var invalid *certificates.ValidationError
	switch {
	case errors.Is(err, certificates.ErrNotFound):
		writeError(c, http.StatusNotFound, httpx.CodeNotFound, "证书不存在")
		return
	case errors.Is(err, certificates.ErrInvalidLimit), errors.Is(err, certificates.ErrInvalidCursor):
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError, err.Error())
		return
	case errors.As(err, &invalid):
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError, invalid.Message)
		return
	case errors.Is(err, certificates.ErrDomainTaken):
		// 409 带 VALIDATION_ERROR：与 proxies 的域名冲突同一套（冲突不是请求写错了，
		// 而是当前状态不允许）。
		writeError(c, http.StatusConflict, httpx.CodeValidationError, err.Error())
		return
	case errors.Is(err, certificates.ErrInUse):
		writeError(c, http.StatusConflict, httpx.CodeResourceBusy, err.Error())
		return
	case errors.Is(err, certificates.ErrVersionConflict):
		writeError(c, http.StatusConflict, httpx.CodeVersionConflict,
			"已被其他人修改，请刷新后重试")
		return
	}
	s.internalError(c, what, err)
}

// normalizeCertificateDomain 归一化一个查询用的域名。
//
// 直接复用证书包的规则（它自己就是代理侧 normalizeDomain 的包装）：列表过滤与
// 写入用的是同一套归一化，否则"按域名过滤"会在大写或末尾点上找不到记录。
func normalizeCertificateDomain(raw string) (string, error) {
	return certificates.NormalizeDomainForQuery(raw)
}
