package agentclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// 二次验证（P14.6）：种子的生成、确认、校验与销毁都在代理里。
//
// 面板这一侧从头到尾看不到种子：登记时拿到一条 otpauth URI（给二维码用）与一个
// `sec_` 引用，此后每次校验只把"这个用户 + 这条引用 + 这个六位数"交给代理，
// 代理自己解密、自己算码、自己回报匹配到的时间步。

// TotpEnrollment 是一次登记的结果（线格式见 internal/agent/totp.go 的 totpEnrollResponse）。
type TotpEnrollment struct {
	// SecretRef 是代理侧那条种子的引用。它**不是**秘密材料，但它指向的种子是：
	// 面板只把它存进 user_totp.secret_ref，不写日志、不回给前端。
	SecretRef string `json:"secret_ref"`
	// OTPAuthURI 里带着种子，只许临时持有（拼成二维码给用户扫）。
	OTPAuthURI string `json:"otpauth_uri"`
	Issuer     string `json:"issuer"`
	Account    string `json:"account"`
	Digits     int    `json:"digits"`
	Period     int    `json:"period"`
	ExpiresAt  string `json:"expires_at"`
}

// TotpCheck 是一次校验的结果。
//
// OK=false 是**正常结果**（码不对），不是错误。Reason 是闭集，目前只有 code_mismatch。
type TotpCheck struct {
	OK bool `json:"ok"`
	// Step 是匹配到的时间步（30 秒一格）。面板拿它做重放防护：必须严格大于
	// user_totp.last_step，否则同一个码被用第二次。
	Step   int64  `json:"step"`
	Reason string `json:"reason"`
}

// TotpEnroll 让代理生成一条新种子，返回引用与 otpauth URI。
//
// username 只用来拼 URI 里的标签（用户在 authenticator 应用里看到的那一行）。
func (c *Client) TotpEnroll(ctx context.Context, userID int64, username string) (TotpEnrollment, error) {
	body := struct {
		UserID   int64  `json:"user_id"`
		Username string `json:"username"`
	}{UserID: userID, Username: username}
	raw, err := c.do(ctx, http.MethodPost, "/internal/v1/totp/enroll", body)
	if err != nil {
		return TotpEnrollment{}, err
	}
	var out TotpEnrollment
	if err := json.Unmarshal(raw, &out); err != nil {
		return TotpEnrollment{}, fmt.Errorf("agentclient: 二次验证登记响应无法解析: %w", err)
	}
	if out.SecretRef == "" || out.OTPAuthURI == "" {
		return TotpEnrollment{}, &UnavailableError{Err: errors.New("代理响应缺少 secret_ref 或 otpauth_uri")}
	}
	return out, nil
}

// TotpConfirm 校验一次码；通过后代理把临时种子转为持久秘密。
func (c *Client) TotpConfirm(ctx context.Context, userID int64, secretRef, code string) (TotpCheck, error) {
	return c.totpCheck(ctx, "/internal/v1/totp/confirm", userID, secretRef, code)
}

// TotpVerify 校验一次码（登录与再认证用）。种子必须已经确认到这个用户名下。
func (c *Client) TotpVerify(ctx context.Context, userID int64, secretRef, code string) (TotpCheck, error) {
	return c.totpCheck(ctx, "/internal/v1/totp/verify", userID, secretRef, code)
}

func (c *Client) totpCheck(ctx context.Context, path string, userID int64, secretRef, code string) (TotpCheck, error) {
	body := struct {
		UserID    int64  `json:"user_id"`
		SecretRef string `json:"secret_ref"`
		Code      string `json:"code"`
	}{UserID: userID, SecretRef: secretRef, Code: code}
	raw, err := c.do(ctx, http.MethodPost, path, body)
	if err != nil {
		return TotpCheck{}, err
	}
	var out TotpCheck
	if err := json.Unmarshal(raw, &out); err != nil {
		return TotpCheck{}, fmt.Errorf("agentclient: 二次验证校验响应无法解析: %w", err)
	}
	// 通过时必须带时间步：没有步就没法做重放防护，那是协议走形，不是"通过了但没有信息"。
	if out.OK && out.Step <= 0 {
		return TotpCheck{}, &UnavailableError{Err: errors.New("代理报告校验通过但没有给出时间步")}
	}
	return out, nil
}

// TotpDisable 销毁代理侧的种子。已有的不在了也算成功（destroyed=false）。
func (c *Client) TotpDisable(ctx context.Context, userID int64, secretRef string) (bool, error) {
	body := struct {
		UserID    int64  `json:"user_id"`
		SecretRef string `json:"secret_ref"`
	}{UserID: userID, SecretRef: secretRef}
	raw, err := c.do(ctx, http.MethodPost, "/internal/v1/totp/disable", body)
	if err != nil {
		return false, err
	}
	var out struct {
		Destroyed bool `json:"destroyed"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return false, fmt.Errorf("agentclient: 销毁种子响应无法解析: %w", err)
	}
	return out.Destroyed, nil
}
