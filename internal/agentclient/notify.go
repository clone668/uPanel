package agentclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// 通知投递（P13.3）：把一次 SMTP/HTTPS 投递交给代理执行。
//
// 面板不自己发——SMTP 口令与 webhook 令牌只存在代理的加密库里，而这里刻意没有
// “取回明文”的接口。这个调用只说三件事：用什么方式、发到哪、说什么。

// DeliverInput 是一次投递请求（线格式见 internal/agent/notify.go 的 notifyRequest）。
type DeliverInput struct {
	// Type 是 smtp / webhook。
	Type     string
	Settings DeliverSettings
	// SecretRef 指向代理库里的秘密；为空表示不需要鉴权。
	SecretRef string
	Title     string
	Body      string
}

// DeliverSettings 是受约束的投递参数。
//
// 三个协议各用其中一部分：smtp 用 Host/Port/TLS/From/To/Username，webhook 用 URL
// 与 AllowPrivate。AllowPrivate 缺省为 false，显式开启才允许内网目标。
// 面板侧已经按类型清空过无关字段，这里不重复判断——代理自己也会再校验一次。
type DeliverSettings struct {
	Host         string   `json:"host,omitempty"`
	Port         int      `json:"port,omitempty"`
	TLS          string   `json:"tls,omitempty"`
	From         string   `json:"from,omitempty"`
	To           []string `json:"to,omitempty"`
	Username     string   `json:"username,omitempty"`
	URL          string   `json:"url,omitempty"`
	AllowPrivate bool     `json:"allow_private,omitempty"`
}

// DeliverResult 是投递的事实回执。
type DeliverResult struct {
	Accepted  bool   `json:"accepted"`
	Status    int    `json:"status"`
	Detail    string `json:"detail"`
	Retryable bool   `json:"retryable"`
}

// Deliver 请求代理执行一次投递。
//
// 返回 error 只表示“这次调用没拿到回执”（连不上、超时、代理拒绝），**不表示投递
// 失败**。投递失败是 DeliverResult.Accepted=false 加上 Detail 里对端的原话。
func (c *Client) Deliver(ctx context.Context, in DeliverInput) (DeliverResult, error) {
	body := struct {
		Type      string          `json:"type"`
		Settings  DeliverSettings `json:"settings"`
		SecretRef string          `json:"secret_ref,omitempty"`
		Title     string          `json:"title"`
		Body      string          `json:"body"`
	}{Type: in.Type, Settings: in.Settings, SecretRef: in.SecretRef, Title: in.Title, Body: in.Body}

	raw, err := c.do(ctx, http.MethodPost, "/internal/v1/notifications/deliver", body)
	if err != nil {
		return DeliverResult{}, err
	}
	var out DeliverResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return DeliverResult{}, fmt.Errorf("agentclient: 投递响应无法解析: %w", err)
	}
	// 代理只会在“按你的要求发了”时回 200，因此这里缺 accepted 字样就是协议走形了。
	if !out.Accepted && out.Detail == "" {
		return DeliverResult{}, &UnavailableError{Err: errors.New("代理解析投递结果失败：既未接受也没有原因")}
	}
	return out, nil
}
