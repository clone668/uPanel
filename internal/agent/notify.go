package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/smtp"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"

	"upanel/internal/httpx"
)

// 通知投递（P13.3）：SMTP 与 HTTPS webhook。
//
// **为什么由代理发而不是由面板发。** SMTP 口令与 webhook 令牌存在代理的加密库里，
// 而协议刻意没有“取回明文”的接口（CONTRACTS §7：禁止通用明文 GET）。要让面板自己
// 发信，就得先给面板开一个读出明文的洞；把发送放在代理这一侧，明文从头到尾没有离开
// 过持有它的进程——面板只知道“发到哪、说什么”。
//
// 代价是代理多了“向外发一个请求”的能力，所以这里的限制是硬的、不看调用方的：
//   - 只允许 smtp 与 webhook 两种协议，没有第三种，也没有“自定义命令”；
//   - webhook 只接受 https，不跟随重定向，响应只读 4 KiB；
//   - SMTP 只接受 STARTTLS 或隐式 TLS，**没有明文模式**，且必须先在 TLS 上才可能 AUTH；
//   - 单次投递总时长上限 8 秒（小于面板侧 10 秒的调用上限，免得面板先把代理掐掉）；
//   - 秘密用途必须是 notification_password。否则面板可以拿任意一个 secret_ref（比如
//     备份仓库口令）让代理把它发到自己的服务器上——那是把“不能读明文”绕成“把明文
//     发给我”。这一条不靠调用方自觉，由代理自己核对。

const (
	// notificationPurpose 是投递时唯一允许的秘密用途。
	notificationPurpose = "notification_password"
	// maxNotifyRequestBytes 是投递请求体上限。
	//
	// 标题 200 + 正文 4000 字符，全为 4 字节 UTF-8 再被 JSON 转义成 \uXXXX 也远小于
	// 64 KiB。余量留给收件人列表与 URL。
	maxNotifyRequestBytes = 64 << 10
	// notifyTotalTimeout 是一次投递的总时长上限。
	notifyTotalTimeout = 8 * time.Second
	// notifyConnectTimeout 是 TCP 连接与 TLS 握手的上限。
	notifyConnectTimeout = 3 * time.Second
	// notifyMaxResponseBytes 是读取 webhook 响应的上限。
	notifyMaxResponseBytes = 4 << 10
	// notifyMaxDetailLen 是回给面板的“对端原话”长度上限。
	notifyMaxDetailLen = 300
)

// notifySecretReader 是投递需要的秘密能力：先核用途，再取明文。
//
// 单独成一个接口而不是加进 SecretStore：写入与读取是两种授权，混在一个接口里会让
// “能写秘密”的实现看上去也能读。Options.Secrets 是 SecretStore，这里用类型断言
// 取读取端——生产装配传的是 *agentstore.Secrets，两者都满足。
type notifySecretReader interface {
	Purpose(ctx context.Context, ref string) (string, bool, error)
	Resolve(ctx context.Context, ref string) ([]byte, error)
}

// notifyRequest 是 POST /internal/v1/notifications/deliver 的请求体。
//
// 与 internal/notifications.Settings 是同一份线格式的**两份实现**：代理不 import
// Web 侧的业务包（那会让特权进程依赖非特权进程的代码），两边各自校验各自的入参。
// 不一致会在这边被拒绝（未知字段严格报错），不会静默变成“某个设置被忽略了”。
type notifyRequest struct {
	Type      string         `json:"type"`
	Settings  notifySettings `json:"settings"`
	SecretRef string         `json:"secret_ref"`
	Title     string         `json:"title"`
	Body      string         `json:"body"`
}

// notifySettings 是受约束的投递参数。
type notifySettings struct {
	Host     string   `json:"host,omitempty"`
	Port     int      `json:"port,omitempty"`
	TLS      string   `json:"tls,omitempty"`
	From     string   `json:"from,omitempty"`
	To       []string `json:"to,omitempty"`
	Username string   `json:"username,omitempty"`
	URL      string   `json:"url,omitempty"`
}

// notifyResponse 是投递结果。
//
// **请求本身成功、投递失败时也回 200**：从调用方的角度看，“代理按你的要求去发了，
// 结果被对端拒绝”与“代理没理你”是两件事。做成 5xx 会让面板只能猜该重试还是该放弃。
type notifyResponse struct {
	Accepted bool `json:"accepted"`
	// Status 是协议回执码：SMTP 应答码或 HTTP 状态码；0 表示没拿到。
	Status int `json:"status"`
	// Detail 是对端原话（截断到 300 字符），不含任何秘密。
	Detail string `json:"detail"`
	// Retryable 表示“再试一次可能成功”。超时、连接重置、SMTP 4xx、HTTP 429/5xx
	// 为真；证书错误、SMTP 5xx、HTTP 其它 4xx、地址写错为假。
	Retryable bool `json:"retryable"`
}

// handleNotificationDeliver 执行一次通知投递。
func (s *Server) handleNotificationDeliver(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		httpx.WriteError(w, r, http.StatusMethodNotAllowed, httpx.CodeValidationError, "只支持 POST")
		return
	}
	if ct := r.Header.Get("Content-Type"); ct != "" && !jsonContentType(ct) {
		httpx.WriteError(w, r, http.StatusUnsupportedMediaType, httpx.CodeValidationError,
			"请求体必须是 application/json")
		return
	}
	body, ok := readLimitedBody(w, r, maxNotifyRequestBytes)
	if !ok {
		return
	}
	var req notifyRequest
	if err := decodeStrictObject(body, &req); err != nil {
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationError, err.Error())
		return
	}
	if err := validateNotifyRequest(req); err != nil {
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationError, err.Error())
		return
	}

	password, ok := s.notifySecret(w, r, req.SecretRef)
	if !ok {
		return
	}
	defer wipeBytes(password)

	ctx, cancel := context.WithTimeout(r.Context(), notifyTotalTimeout)
	defer cancel()

	var result notifyResponse
	if req.Type == "smtp" {
		result = sendSMTP(ctx, req, string(password))
	} else {
		result = sendWebhook(ctx, req, string(password))
	}
	httpx.WriteData(w, r, http.StatusOK, result)
}

// notifySecret 取投递用的明文口令。第二个返回值为 false 表示响应已经写完。
//
// 用途核对在前、解密在后：先问“这是什么用途的”，再问“内容是什么”。反过来做也
// 能工作，但那意味着每个被拒绝的请求都已经把明文解开过一次。
func (s *Server) notifySecret(w http.ResponseWriter, r *http.Request, ref string) ([]byte, bool) {
	if ref == "" {
		return nil, true
	}
	reader, ok := s.opts.Secrets.(notifySecretReader)
	if !ok {
		// 装配问题：能写不能读的秘密存储发不出任何通知。
		s.opts.Logger.Error("秘密存储不支持读取，无法投递通知", "request_id", httpx.RequestID(r))
		httpx.WriteError(w, r, http.StatusServiceUnavailable, httpx.CodeInternalError,
			"代理的秘密存储不支持读取，无法投递")
		return nil, false
	}
	purpose, found, err := reader.Purpose(r.Context(), ref)
	if err != nil {
		s.opts.Logger.Error("读取秘密用途失败", "error", err, "request_id", httpx.RequestID(r))
		httpx.WriteError(w, r, http.StatusInternalServerError, httpx.CodeInternalError, "读取秘密失败")
		return nil, false
	}
	if !found || purpose != notificationPurpose {
		// 不区分“不存在”与“用途不对”：区分开就等于送出一个探测秘密用途的接口。
		s.opts.Logger.Warn("拒绝对非通知用途的秘密发起投递", "request_id", httpx.RequestID(r))
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationError,
			"secret_ref 只能指向用途为 notification_password 的秘密")
		return nil, false
	}
	plain, err := reader.Resolve(r.Context(), ref)
	if err != nil {
		// 秘密可能在入队之后被销毁或到期了：这是可解释的投递失败，不是畸形请求。
		httpx.WriteData(w, r, http.StatusOK, notifyResponse{
			Accepted: false,
			Detail:   "秘密不可用：" + err.Error(),
		})
		return nil, false
	}
	return plain, true
}

// validateNotifyRequest 校验投递请求。**与面板侧的校验重复是故意的**：代理不能假定
// 调用方已经校验过——它就是最后一道。
func validateNotifyRequest(req notifyRequest) error {
	title := []rune(req.Title)
	if len(title) == 0 || len(title) > 200 {
		return errors.New("title 长度必须在 1—200 之间")
	}
	if strings.ContainsAny(req.Title, "\r\n") {
		return errors.New("title 不能包含换行")
	}
	if len([]rune(req.Body)) > 4000 {
		return errors.New("body 不能超过 4000 字符")
	}
	switch req.Type {
	case "smtp":
		st := req.Settings
		if st.Host == "" || len(st.Host) > 253 || strings.ContainsAny(st.Host, " \t\r\n/") {
			return errors.New("settings.host 不是合法的主机名")
		}
		if st.Port < 1 || st.Port > 65535 {
			return errors.New("settings.port 必须是 1—65535")
		}
		if st.TLS != "starttls" && st.TLS != "implicit" {
			return errors.New("settings.tls 必须是 starttls 或 implicit（不接受不加密的 SMTP）")
		}
		if !validMailAddress(st.From) {
			return errors.New("settings.from 不是合法的邮件地址")
		}
		if len(st.To) == 0 || len(st.To) > 16 {
			return errors.New("settings.to 必须有 1—16 个收件人")
		}
		for _, addr := range st.To {
			if !validMailAddress(addr) {
				return fmt.Errorf("settings.to 里的 %q 不是合法地址", addr)
			}
		}
		if st.Username == "" && req.SecretRef != "" {
			return errors.New("给了 secret_ref 就必须给 settings.username")
		}
		if st.Username != "" && req.SecretRef == "" {
			return errors.New("给了 settings.username 就必须给 secret_ref")
		}
	case "webhook":
		u, err := url.Parse(req.Settings.URL)
		if err != nil {
			return errors.New("settings.url 不是合法地址")
		}
		if u.Scheme != "https" || u.Host == "" {
			return errors.New("settings.url 必须是 https 地址")
		}
		if u.User != nil {
			return errors.New("settings.url 不接受用户名口令")
		}
		if u.Fragment != "" {
			return errors.New("settings.url 不接受 #片段")
		}
	default:
		return errors.New("type 必须是 smtp 或 webhook")
	}
	return nil
}

// validMailAddress 与面板侧同一条口径：一个 @、两侧非空、不含空白与尖括号。
func validMailAddress(addr string) bool {
	if addr == "" || len(addr) > 254 {
		return false
	}
	if strings.ContainsAny(addr, " \t\r\n<>\"") {
		return false
	}
	at := strings.IndexByte(addr, '@')
	return at > 0 && at == strings.LastIndexByte(addr, '@') && at != len(addr)-1
}

// tlsConfigFor 给一个主机名配上 TLS 参数。
//
// MinVersion 钉在 TLS 1.2：1.0/1.1 早已被主流邮件服务关掉，而 Go 默认仍允许它们。
// ServerName 必须显式给——它决定证书要对谁校验。
func tlsConfigFor(host string) *tls.Config {
	return &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
}

// sendSMTP 投递一封邮件。
func sendSMTP(ctx context.Context, req notifyRequest, password string) notifyResponse {
	st := req.Settings
	addr := net.JoinHostPort(st.Host, strconv.Itoa(st.Port))
	dialer := &net.Dialer{Timeout: notifyConnectTimeout}

	var (
		conn net.Conn
		err  error
	)
	if st.TLS == "implicit" {
		// DialWithDialer 的时限同样管住 TLS 握手。
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, tlsConfigFor(st.Host))
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return classifySMTPError(err, "连接失败")
	}
	// ctx 到点时把连接掐掉：net/smtp 自己不认 context，不掐它就会一直等到对端超时。
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stop:
		}
	}()

	c, err := smtp.NewClient(conn, st.Host)
	if err != nil {
		_ = conn.Close()
		return classifySMTPError(err, "建立 SMTP 会话失败")
	}
	defer func() { _ = c.Close() }()

	if st.TLS == "starttls" {
		if err := c.StartTLS(tlsConfigFor(st.Host)); err != nil {
			return classifySMTPError(err, "STARTTLS 失败")
		}
	}

	if st.Username != "" {
		// PlainAuth 自己会拒绝在非 TLS 连接上发送口令，那正是要的行为（上面已经强制
		// 加密，这条是双保险）。只支持 PLAIN：net/smtp 没有 LOGIN，只有 PLAIN 之外的
		// 机制的对端会在下面报“服务器不支持 AUTH”，而不是悄悄发一封匿名邮件。
		if err := c.Auth(smtp.PlainAuth("", st.Username, password, st.Host)); err != nil {
			return classifySMTPError(err, "认证失败")
		}
	}
	if err := c.Mail(st.From); err != nil {
		return classifySMTPError(err, "MAIL FROM 被拒绝")
	}
	for _, to := range st.To {
		if err := c.Rcpt(to); err != nil {
			return classifySMTPError(err, "RCPT TO 被拒绝")
		}
	}
	data, err := c.Data()
	if err != nil {
		return classifySMTPError(err, "DATA 被拒绝")
	}
	if _, err := data.Write(buildMail(req, st)); err != nil {
		return classifySMTPError(err, "写入正文失败")
	}
	if err := data.Close(); err != nil {
		return classifySMTPError(err, "结束正文失败")
	}
	if err := c.Quit(); err != nil {
		// 正文已经被收下，QUIT 时对端抽风：算投递成功，但把异常记进 detail。
		return notifyResponse{Accepted: true, Status: 250,
			Detail: truncateDetail("已投递（QUIT 时对端异常：" + err.Error() + "）")}
	}
	return notifyResponse{Accepted: true, Status: 250, Detail: "已投递"}
}

// buildMail 组装一封 RFC 5322 文本邮件。
//
// 正文用 base64：UTF-8 中文在 8bit 编码下会被一些老 MTA 弄坏，而 base64 不用管行长
// 与点转义（net/smtp 自己会做点转义，但那是为 8bit 服务的）。标题是纯事实（规则 +
// 指标），不含中文之外的怪字符，直接放。
func buildMail(req notifyRequest, st notifySettings) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "From: %s\r\n", st.From)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(st.To, ", "))
	fmt.Fprintf(&b, "Subject: %s\r\n", req.Title)
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().UTC().Format(time.RFC1123Z))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("Content-Transfer-Encoding: base64\r\n")
	b.WriteString("X-Mailer: uPanel\r\n\r\n")
	encoded := base64.StdEncoding.EncodeToString([]byte(req.Body))
	for len(encoded) > 76 {
		b.WriteString(encoded[:76])
		b.WriteString("\r\n")
		encoded = encoded[76:]
	}
	if encoded != "" {
		b.WriteString(encoded)
		b.WriteString("\r\n")
	}
	return b.Bytes()
}

// classifySMTPError 把错误翻成“值不值得再试”。
//
// 4xx 是临时拒绝（邮箱忙、灰名单、限速），5xx 是永久拒绝（地址不存在、认证失败、
// 内容被拒）。这个区分不是我们发明的，是 RFC 5321 的规定——照抄它比猜可靠。
func classifySMTPError(err error, prefix string) notifyResponse {
	var protoErr *textproto.Error
	if errors.As(err, &protoErr) {
		return notifyResponse{
			Status:    protoErr.Code,
			Detail:    truncateDetail(prefix + "：" + protoErr.Error()),
			Retryable: protoErr.Code >= 400 && protoErr.Code < 500,
		}
	}
	// 证书错误不是“再试一次会好”的东西：配置错了就是错了。
	if isTLSError(err) {
		return notifyResponse{Detail: truncateDetail(prefix + "：" + err.Error())}
	}
	return notifyResponse{Detail: truncateDetail(prefix + "：" + err.Error()), Retryable: true}
}

// isTLSError 判定错误是否属于“证书/协议栈配错”，这类不重试。
func isTLSError(err error) bool {
	var (
		certErr   *tls.CertificateVerificationError
		recordErr tls.RecordHeaderError
		alertErr  tls.AlertError
	)
	if errors.As(err, &certErr) || errors.As(err, &recordErr) || errors.As(err, &alertErr) {
		return true
	}
	// 证书错误在多层包装之后可能只剩字符串（对端提前断开时 TLS 记录层只能给出
	// 纯文本）。宁可少重试一次：每次重试都要再连一次对端、再等一轮超时。
	msg := err.Error()
	return strings.Contains(msg, "x509") || strings.Contains(msg, "certificate")
}

// sendWebhook 投递一个 HTTPS 回调。
func sendWebhook(ctx context.Context, req notifyRequest, token string) notifyResponse {
	payload, err := json.Marshal(map[string]string{
		"title":  req.Title,
		"text":   req.Body,
		"source": "upanel",
	})
	if err != nil {
		return notifyResponse{Detail: "序列化正文失败"}
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, req.Settings.URL,
		bytes.NewReader(payload))
	if err != nil {
		return notifyResponse{Detail: truncateDetail("构造请求失败：" + err.Error())}
	}
	httpReq.Header.Set("Content-Type", "application/json; charset=utf-8")
	httpReq.Header.Set("User-Agent", "uPanel")
	if token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{
		Timeout: notifyTotalTimeout,
		Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: notifyConnectTimeout}).DialContext,
			TLSHandshakeTimeout:   notifyConnectTimeout,
			ResponseHeaderTimeout: notifyTotalTimeout,
			// 不读环境里的代理变量：从面板发出去的告警应该走服务器自己的出口，
			// 而不是某个 shell 里恰好设着的 http_proxy。也不复用连接——投递是低频事件，
			// 留一条空闲连接不如让每次投递都干净。
			Proxy:             nil,
			DisableKeepAlives: true,
		},
		// **不跟随重定向。** 跟随意味着对端可以把我们的告警正文引到一个我们没配过的
		// 地址上（包括内网其它服务）；而“地址配错了”本来就该在测试发送时发现。
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return classifyWebhookError(err)
	}
	defer func() { _ = resp.Body.Close() }()
	// 响应只读到 4 KiB：对端回一个 100 MB 的错误页不该把代理的内存吃掉。
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, notifyMaxResponseBytes))
	return classifyWebhookResponse(resp.StatusCode, resp.Header.Get("Location"), raw)
}

// classifyWebhookResponse 把一次 HTTP 回执翻成投递结果。
//
// 200—299 是收下了；3xx 是“我们没跟随重定向，所以这条没送到”；429 与 5xx 是
// “对端现在不行”，值得再试；其余 4xx 是我们请求本身不合对端的规矩，重试只会
// 重复同一句拒绝。
func classifyWebhookResponse(status int, location string, body []byte) notifyResponse {
	summary := truncateDetail(string(body))
	switch {
	case status >= 200 && status < 300:
		return notifyResponse{Accepted: true, Status: status, Detail: summary}
	case status >= 300 && status < 400:
		return notifyResponse{Status: status, Retryable: false,
			Detail: truncateDetail("对端要求重定向（不接受）：" + location)}
	default:
		return notifyResponse{
			Status:    status,
			Detail:    summary,
			Retryable: status == http.StatusTooManyRequests || status >= 500,
		}
	}
}

// classifyWebhookError 判定一次 HTTP 失败值不值得重试。
func classifyWebhookError(err error) notifyResponse {
	if isTLSError(err) || strings.Contains(err.Error(), "unsupported protocol scheme") {
		return notifyResponse{Detail: truncateDetail("请求失败：" + err.Error())}
	}
	return notifyResponse{Detail: truncateDetail("请求失败：" + err.Error()), Retryable: true}
}

// truncateDetail 把“对端原话”压成一行、截到 300 字符。
func truncateDetail(s string) string {
	s = strings.TrimSpace(strings.NewReplacer("\r", " ", "\n", " ").Replace(s))
	runes := []rune(s)
	if len(runes) <= notifyMaxDetailLen {
		return string(runes)
	}
	return string(runes[:notifyMaxDetailLen-1]) + "…"
}
