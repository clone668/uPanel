package api

// 证书动作的预检参数展开（P10.3）。
//
// 与 proxy.apply 同一条原则：**能落进系统的参数必须是面板算出来的**，不是调用方说
// 它是什么。三个动作的展开各有各的理由：
//
//	certificate.import  cert_pem 来自暂存区（上传时解析过的那一份），
//	                    certificate_id 复用该域名已有的记录（站点绑定指向它）
//	certificate.issue   同上；邮箱与 staging 是用户选的，但域名要归一化
//	certificate.renew   只接受一个域名：certificate_id、邮箱都从既有记录里取，
//	                    而且必须有记录——"续一张不存在的证书"没有意义
//
// 三个动作还共享一条**限流**：同一个域名一小时内只允许一次 ACME 尝试。这条规则查
// 的是任务表（失败也算一次），见 operations.Service.RecentAttempts。

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"upanel/internal/certificates"
	"upanel/internal/httpx"
	"upanel/internal/tlscert"
)

// certificateImportParamsRequest 是调用方交给 certificate.import 的字段。
//
// 没有 cert_pem：公钥证书走 POST /certificates/uploads，这里只引用它的 id。
// 没有 certificate_id：记录标识由服务端按域名决定（复用或新建），否则同一个域名
// 可以被两次导入写成两条记录，而站点只绑得上其中一条。
type certificateImportParamsRequest struct {
	Domain       string `json:"domain"`
	UploadID     string `json:"certificate_upload_id"`
	KeySecretRef string `json:"private_key_secret_ref"`
	// Staging 是用户说"这张证书来自测试环境"。它只进报告与告警：真实判断看证书自己
	// 的签发者（tlscert 会解析），因为人的说法可能是错的。
	Staging bool `json:"staging"`
}

// certificateIssueParamsRequest 是调用方交给 certificate.issue / certificate.renew 的字段。
//
// renew 只用到 domain：其余从既有记录里取（certificate_id、邮箱、staging）。这不是
// 省略，而是"续期不改身份"——换邮箱请走 issue，那会是一份新的计划。
type certificateIssueParamsRequest struct {
	Domain string `json:"domain"`
	Email  string `json:"email"`
	// Staging 默认必须显式为 true 或 false：省缺读成 false 会让人一句 JSON 写漏就把
	// 正式环境的速率限额花掉。所以这里用指针，缺字段直接拒绝。
	Staging *bool `json:"staging"`
}

// expandCertificateImport 把 {domain, upload_id, key_secret_ref} 展开成 ImportParams。
func (s *Server) expandCertificateImport(c *gin.Context, params json.RawMessage) (json.RawMessage, bool) {
	var req certificateImportParamsRequest
	if !decodeExpandParams(c, params, &req) {
		return nil, false
	}
	domain, ok := s.normalizeExpandDomain(c, req.Domain)
	if !ok {
		return nil, false
	}
	uploadID := strings.TrimSpace(req.UploadID)
	if uploadID == "" {
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError,
			"缺少 certificate_upload_id：公钥证书必须先经 POST /certificates/uploads 上传")
		return nil, false
	}
	keyRef := strings.TrimSpace(req.KeySecretRef)
	if keyRef == "" {
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError,
			"缺少 private_key_secret_ref：私钥必须先经 POST /secrets（用途 tls_private_key）上传")
		return nil, false
	}

	// 先看库里有没有这个域名的记录：有就复用它的标识（站点绑定指向它），
	// 而且来路不同的记录不能被手动导入悄悄顶掉。
	id, ok := s.certificateIDForDomain(c, domain, certificates.SourceManual)
	if !ok {
		return nil, false
	}

	// 最后才取暂存：Take 是销毁语义，前面任何一步失败都不该把用户刚传的证书吃掉。
	// 说明它的反面：域名不覆盖这条检查发生在 Take 之后（要判覆盖必须先解出证书里的
	// 域名），所以"传了一张别的域名的证书"会连上传一起消费掉。这是有意的——暂存
	// 只有 10 分钟、一次性，留一份"刚被判定不能用"的副本只会让后续请求拿到一个
	// 已经被人看过、且被判定失败的输入。错误信息里直说需要重传。
	sess := sessionFrom(c)
	staged, err := s.opts.CertificateStages.Take(uploadID, sess.User.ID, sess.BindingHash())
	if err != nil {
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError,
			"上传已过期或不存在（暂存 10 分钟）：请重新上传公钥证书")
		return nil, false
	}
	if !tlscert.CoversDomain(staged.Info, domain) {
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError,
			"这份证书覆盖的是 "+strings.Join(staged.Info.Domains, "、")+"，不包含 "+domain+
				"（上传在展开时已被消费，请重新上传一张覆盖该域名的证书）")
		return nil, false
	}

	expanded := certificates.ImportParams{
		CertificateID: id,
		Domain:        domain,
		CertPEM:       string(staged.PEM),
		KeySecretRef:  keyRef,
		Staging:       req.Staging,
	}
	return marshalExpanded(c, expanded)
}

// expandCertificateIssue 把 {domain, email, staging} 展开成 IssueParams。
func (s *Server) expandCertificateIssue(c *gin.Context, action string, params json.RawMessage) (json.RawMessage, bool) {
	var req certificateIssueParamsRequest
	if !decodeExpandParams(c, params, &req) {
		return nil, false
	}
	domain, ok := s.normalizeExpandDomain(c, req.Domain)
	if !ok {
		return nil, false
	}
	if req.Staging == nil {
		// 缺字段就是拒绝，而不是"当它选正式环境"或"当它选测试环境"。
		// 前者会烧掉限额，后者会让人以为签好了而浏览器仍然报警告。
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError,
			"必须显式给出 staging（true 用测试环境，false 用正式环境）："+
				"正式签发有速率限额，这个选择不能靠默认值")
		return nil, false
	}

	existing, err := s.opts.Certificates.GetByDomain(c.Request.Context(), domain)
	found := err == nil
	if err != nil && !errors.Is(err, certificates.ErrNotFound) {
		s.internalError(c, "预检展开失败", err)
		return nil, false
	}

	var id, email string
	switch action {
	case certificates.ActionRenew:
		if !found {
			writeError(c, http.StatusNotFound, httpx.CodeNotFound,
				"这个域名还没有证书记录，无法续期：先用 certificate.issue 签发一次")
			return nil, false
		}
		if existing.Source != certificates.SourceCertbot {
			writeError(c, http.StatusConflict, httpx.CodeValidationError,
				"这条记录是手动导入的证书：certbot 续不了它，请重新导入或签发一张新的")
			return nil, false
		}
		id = existing.ID
		email = existing.ACMEEmail
	case certificates.ActionIssue:
		if found && existing.Source != certificates.SourceCertbot {
			writeError(c, http.StatusConflict, httpx.CodeValidationError,
				"这个域名已有一条手动导入的证书记录：签发会换掉它的来路。"+
					"先删除那条记录（站点会先解绑）再签发")
			return nil, false
		}
		if found {
			id = existing.ID
			email = existing.ACMEEmail
		} else {
			generated, gerr := certificates.NewID()
			if gerr != nil {
				s.internalError(c, "预检展开失败", gerr)
				return nil, false
			}
			id = generated
		}
	}
	if raw := strings.TrimSpace(req.Email); raw != "" {
		email = raw
	}
	if email == "" {
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError,
			"缺少 email：ACME 账号需要一个邮箱（Let's Encrypt 用它发到期提醒）")
		return nil, false
	}

	// 限流：一小时内同一个域名只允许一次 ACME 尝试。查任务表而不是证书表——
	// 失败的尝试同样消耗配额，而失败的首次签发在证书表里根本没有记录。
	recent, err := s.opts.Operations.RecentAttempts(c.Request.Context(),
		[]string{certificates.ActionIssue, certificates.ActionRenew},
		domain, time.Now().UTC().Add(-certificates.AttemptWindow))
	if err != nil {
		s.internalError(c, "预检展开失败", err)
		return nil, false
	}
	if recent > 0 || (found && certificates.TooSoon(existing.LastAttemptAt, time.Now().UTC())) {
		writeError(c, http.StatusTooManyRequests, httpx.CodeResourceBusy,
			"这个域名在一小时内已经尝试过签发/续期：ACME 对同一域名的签发有速率限额，"+
				"失败的尝试也算一次。请先核对失败原因（任务详情里有 certbot 的输出），"+
				"确认后再等一小时重试")
		return nil, false
	}

	expanded := certificates.IssueParams{
		CertificateID: id,
		Domain:        domain,
		Email:         email,
		Staging:       *req.Staging,
	}
	return marshalExpanded(c, expanded)
}

// certificateIDForDomain 决定这次写入用哪个记录标识。
//
// 规则两条：同域名已有记录就复用它（站点绑定、审计、告警才能跟着走）；来路不同则
// 拒绝——一条记录不能既是 certbot 签发又是手动导入，"当前这张是谁给的"必须只有一个答案。
func (s *Server) certificateIDForDomain(c *gin.Context, domain string, want certificates.Source) (string, bool) {
	existing, err := s.opts.Certificates.GetByDomain(c.Request.Context(), domain)
	if err == nil {
		if existing.Source != want {
			writeError(c, http.StatusConflict, httpx.CodeValidationError,
				"这个域名已有一条来路不同的证书记录（"+string(existing.Source)+"）："+
					"先删除那条记录再写入新的来路")
			return "", false
		}
		return existing.ID, true
	}
	if !errors.Is(err, certificates.ErrNotFound) {
		s.internalError(c, "预检展开失败", err)
		return "", false
	}
	id, gerr := certificates.NewID()
	if gerr != nil {
		s.internalError(c, "预检展开失败", gerr)
		return "", false
	}
	return id, true
}

// decodeExpandParams 解码一份只允许已知字段的参数。
func decodeExpandParams(c *gin.Context, params json.RawMessage, out any) bool {
	if len(strings.TrimSpace(string(params))) == 0 {
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError, "缺少动作参数")
		return false
	}
	dec := json.NewDecoder(strings.NewReader(string(params)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError,
			"动作参数无法解析："+err.Error())
		return false
	}
	return true
}

// normalizeExpandDomain 归一化展开用到的域名。
func (s *Server) normalizeExpandDomain(c *gin.Context, raw string) (string, bool) {
	domain, err := certificates.NormalizeDomainForQuery(raw)
	if err != nil {
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError, err.Error())
		return "", false
	}
	return domain, true
}

// marshalExpanded 把展开后的参数序列化。
func marshalExpanded(c *gin.Context, v any) (json.RawMessage, bool) {
	raw, err := json.Marshal(v)
	if err != nil {
		writeError(c, http.StatusInternalServerError, httpx.CodeInternalError, "预检展开失败")
		return nil, false
	}
	return raw, true
}
