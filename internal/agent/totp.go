// /internal/v1/totp/* 的四个专用接口（P14.6）。
//
// 二次验证的种子**只存在于代理里**：Web 调 enroll 拿到 `sec_` 引用与一次性 otpauth URI，
// 从此再也见不到明文种子（CONTRACTS.md 第 13 节的 query 说明与第 14.1 节的 user_totp
// 表：`user_id`、代理 `secret_ref`、`enabled`、恢复码哈希，**无明文种子**）。
//
// 四个接口的分工正好对应秘密的生命周期：
//
//	enroll  代理生成种子 → 以内部用途 totp_seed 写入，**不带引用**，于是它是 10 分钟后
//	        自动销毁的临时秘密。放弃的登记流程会自己消失，不需要谁来清理。
//	confirm 校验一次码，通过后把临时秘密**转成持久秘密**并挂上引用 `totp:<user_id>`。
//	        "还没确认过的 TOTP 不算启用"因此有两处保证：Web 侧的 enabled 字段，以及
//	        代理侧这个种子在被确认之前根本没有归属。
//	verify  每次登录/再认证都要过这里：先核对归属（这条种子是不是这个用户的），再验码。
//	        返回匹配到的时间步，Web 用它做**重放**防护——同一个 30 秒窗口里同一个码不许用两次。
//	disable 销毁种子（墓碑行）。销毁之后同一条引用查到的是"不存在"，与从未登记同义。
//
// 归属核对（`totp:<user_id>`）不是多余的：任何一条 totp_seed 都解得开，光"能解密"证明不了
// "这条种子是你的"。把 3 号用户的 seed_ref 拿去验 5 号用户的码，必须在这一层被挡住。
package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"upanel/internal/agentstore"
	"upanel/internal/httpx"
	"upanel/internal/totp"
)

const (
	// totpIssuer 是 otpauth URI 里的发行方名称，出现在用户的 authenticator 应用里。
	totpIssuer = "uPanel"
	// totpPurpose 是种子在代理库里的用途（agentstore 里已登记为内部用途）。
	totpPurpose = "totp_seed"
	// totpRequestBytes 是这四个接口的请求体上限：几个短字段而已。
	totpRequestBytes = 4 << 10
	// maxTotpUsername 是标签里账号名的长度上限，与面板用户名上限一致。
	maxTotpUsername = 64
)

// TotpSecrets 是二次验证需要的秘密能力。
//
// 与 SecretStore 分开是有意的：SecretStore 刻意**没有**解密方法，而二次验证必须解密种子
// 才能算码。把解密能力挂在"整个代理都在用的接口"上，等于给每个调用点都开了一次解密机会；
// 单独一份接口，谁需要解密一眼可见。
type TotpSecrets interface {
	PutInternal(ctx context.Context, in agentstore.PutInput) (agentstore.Secret, error)
	Resolve(ctx context.Context, ref string) ([]byte, error)
	Binding(ctx context.Context, ref string) (purpose, reference string, ok bool, err error)
	Reference(ctx context.Context, ref, reference string) error
	Destroy(ctx context.Context, ref string) error
}

// totpBindingRef 是一条种子与用户的绑定形式。用 user_id 而不是用户名：
// 用户名可以改，id 不会。
func totpBindingRef(userID int64) string {
	return fmt.Sprintf("totp:%d", userID)
}

// totpEnrollRequest 是 POST /internal/v1/totp/enroll 的请求体。
type totpEnrollRequest struct {
	UserID int64 `json:"user_id"`
	// Username 只用来拼 otpauth 标签（用户在 authenticator 应用里看到的那一行）。
	Username string `json:"username"`
}

// totpEnrollResponse 是一次登记的全部产出。明文种子只在这一个响应里出现一次。
type totpEnrollResponse struct {
	SecretRef string `json:"secret_ref"`
	// OTPAuthURI 直接喂给二维码编码器；它里面带着种子，所以 Web 只许临时持有。
	OTPAuthURI string `json:"otpauth_uri"`
	Issuer     string `json:"issuer"`
	Account    string `json:"account"`
	Digits     int    `json:"digits"`
	Period     int    `json:"period"`
	ExpiresAt  string `json:"expires_at,omitempty"`
}

// totpCodeRequest 是 confirm / verify 共用的请求体。
type totpCodeRequest struct {
	UserID    int64  `json:"user_id"`
	SecretRef string `json:"secret_ref"`
	Code      string `json:"code"`
}

// totpCodeResponse 是一次校验的结果。
//
// 码不对是**正常结果**而不是错误：HTTP 200 + `ok=false` + 闭集 reason。
// 只有"请求本身不成立"（参数畸形、用途不对、种子不属于这个用户、种子不存在）才用 4xx。
type totpCodeResponse struct {
	OK bool `json:"ok"`
	// Step 是匹配到的时间步。Web 存下它，下一次必须严格更大，否则是重放。
	Step   int64  `json:"step,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// 校验结果的原因闭集。
const (
	totpReasonCodeMismatch = "code_mismatch"
)

// totpDisableRequest 是 POST /internal/v1/totp/disable 的请求体。
type totpDisableRequest struct {
	UserID    int64  `json:"user_id"`
	SecretRef string `json:"secret_ref"`
}

// totpDisableResponse 是关闭二次验证的结果。
type totpDisableResponse struct {
	Destroyed bool `json:"destroyed"`
}

// handleTotpEnroll 处理 POST /internal/v1/totp/enroll。
func (s *Server) handleTotpEnroll(w http.ResponseWriter, r *http.Request) {
	if !s.totpReady(w, r, http.MethodPost) {
		return
	}
	var req totpEnrollRequest
	if !s.decodeTotpBody(w, r, &req) {
		return
	}
	if req.UserID <= 0 {
		s.writeTotpValidation(w, r, "user_id 必须是正整数")
		return
	}
	username := strings.TrimSpace(req.Username)
	if username == "" || len(username) > maxTotpUsername {
		s.writeTotpValidation(w, r, "username 不能为空且不超过 64 字节")
		return
	}
	if strings.ContainsAny(username, "\r\n\x00") {
		s.writeTotpValidation(w, r, "username 里不能有控制字符")
		return
	}

	seed, err := totp.GenerateSecret()
	if err != nil {
		s.writeTotpInternal(w, r, "生成种子失败", err)
		return
	}
	// 用 []byte 传进去，写完立刻清零自己这份：入参在库内部只做一次加密，不会留副本。
	value := []byte(seed)
	defer wipeBytes(value)

	secret, err := s.opts.Totp.PutInternal(r.Context(), agentstore.PutInput{
		Purpose: totpPurpose,
		Value:   value,
		// 不带引用：确认之前它是临时秘密，10 分钟后自己消失。
	})
	if err != nil {
		s.writeTotpInternal(w, r, "保存种子失败", err)
		return
	}

	body := totpEnrollResponse{
		SecretRef:  secret.Ref,
		OTPAuthURI: totp.URI(totpIssuer, username, seed),
		Issuer:     totpIssuer,
		Account:    username,
		Digits:     totp.Digits,
		Period:     int(totp.Period.Seconds()),
	}
	if !secret.ExpiresAt.IsZero() {
		body.ExpiresAt = secret.ExpiresAt.UTC().Format(time.RFC3339)
	}
	s.opts.Logger.Info("已生成二次验证登记",
		"ref", secret.Ref, "user_id", req.UserID, "request_id", httpx.RequestID(r))
	httpx.WriteData(w, r, http.StatusCreated, body)
}

// handleTotpConfirm 处理 POST /internal/v1/totp/confirm。
func (s *Server) handleTotpConfirm(w http.ResponseWriter, r *http.Request) {
	if !s.totpReady(w, r, http.MethodPost) {
		return
	}
	var req totpCodeRequest
	if !s.decodeTotpBody(w, r, &req) {
		return
	}
	if !s.validateTotpCodeInput(w, r, &req) {
		return
	}
	_, reference, ok := s.totpBinding(w, r, &req)
	if !ok {
		return
	}
	// 确认接受"还没挂归属"的临时种子，也接受"已经挂到同一个用户名下"的重复确认
	// （Reference 对同引用是幂等的）。挂到别人名下的必须挡住。
	if reference != "" && reference != totpBindingRef(req.UserID) {
		s.writeTotpForbidden(w, r, "这条种子已经绑定到别的用户")
		return
	}

	step, matched, done := s.checkTotpCode(w, r, &req)
	if done {
		return
	}
	if !matched {
		httpx.WriteData(w, r, http.StatusOK, totpCodeResponse{OK: false, Reason: totpReasonCodeMismatch})
		return
	}
	if err := s.opts.Totp.Reference(r.Context(), req.SecretRef, totpBindingRef(req.UserID)); err != nil {
		if errors.Is(err, agentstore.ErrSecretExpired) {
			s.writeTotpValidation(w, r, "这条登记已经过期，请重新开始")
			return
		}
		s.writeTotpInternal(w, r, "把种子转为持久秘密失败", err)
		return
	}
	s.opts.Logger.Info("二次验证登记已确认",
		"ref", req.SecretRef, "user_id", req.UserID, "request_id", httpx.RequestID(r))
	httpx.WriteData(w, r, http.StatusOK, totpCodeResponse{OK: true, Step: step})
}

// handleTotpVerify 处理 POST /internal/v1/totp/verify。
func (s *Server) handleTotpVerify(w http.ResponseWriter, r *http.Request) {
	if !s.totpReady(w, r, http.MethodPost) {
		return
	}
	var req totpCodeRequest
	if !s.decodeTotpBody(w, r, &req) {
		return
	}
	if !s.validateTotpCodeInput(w, r, &req) {
		return
	}
	_, reference, ok := s.totpBinding(w, r, &req)
	if !ok {
		return
	}
	// 验证要求这条种子**已经**归属这个用户：未确认的临时登记不该能用来登录。
	if reference != totpBindingRef(req.UserID) {
		s.writeTotpForbidden(w, r, "这条种子还没有确认到这个用户名下")
		return
	}
	step, matched, done := s.checkTotpCode(w, r, &req)
	if done {
		return
	}
	if !matched {
		httpx.WriteData(w, r, http.StatusOK, totpCodeResponse{OK: false, Reason: totpReasonCodeMismatch})
		return
	}
	httpx.WriteData(w, r, http.StatusOK, totpCodeResponse{OK: true, Step: step})
}

// handleTotpDisable 处理 POST /internal/v1/totp/disable。
//
// 关闭是**幂等**的：种子已经不在了（第一次关闭、或 10 分钟到期自毁）同样回
// `destroyed:true`。理由两条——Web 的"重试"不该因为第一次其实成功而报错；
// 一条"引用不存在"和一条"引用存在但已销毁"给出不同响应，本身就是个存在性探针。
func (s *Server) handleTotpDisable(w http.ResponseWriter, r *http.Request) {
	if !s.totpReady(w, r, http.MethodPost) {
		return
	}
	var req totpDisableRequest
	if !s.decodeTotpBody(w, r, &req) {
		return
	}
	if req.UserID <= 0 {
		s.writeTotpValidation(w, r, "user_id 必须是正整数")
		return
	}
	if strings.TrimSpace(req.SecretRef) == "" {
		s.writeTotpValidation(w, r, "secret_ref 不能为空")
		return
	}
	purpose, reference, found, err := s.opts.Totp.Binding(r.Context(), req.SecretRef)
	if err != nil {
		s.writeTotpInternal(w, r, "核对种子归属失败", err)
		return
	}
	if !found {
		// 没有可销毁的东西。目标状态（"这个引用下没有种子"）已经成立。
		httpx.WriteData(w, r, http.StatusOK, totpDisableResponse{Destroyed: false})
		return
	}
	if purpose != totpPurpose {
		s.writeTotpValidation(w, r, fmt.Sprintf("引用 %s 不是二次验证种子（用途 %s）", req.SecretRef, purpose))
		return
	}
	if reference != "" && reference != totpBindingRef(req.UserID) {
		s.writeTotpForbidden(w, r, "这条种子已经绑定到别的用户")
		return
	}
	if err := s.opts.Totp.Destroy(r.Context(), req.SecretRef); err != nil {
		s.writeTotpInternal(w, r, "销毁种子失败", err)
		return
	}
	s.opts.Logger.Info("二次验证已关闭，种子已销毁",
		"ref", req.SecretRef, "user_id", req.UserID, "request_id", httpx.RequestID(r))
	httpx.WriteData(w, r, http.StatusOK, totpDisableResponse{Destroyed: true})
}

// validateTotpCodeInput 检查 confirm / verify 的输入形状。纯本地检查，不碰存储：
// 参数畸形时不该先花一次数据库查询，也不该让"码为空"落到"归属不对"的解释上。
func (s *Server) validateTotpCodeInput(w http.ResponseWriter, r *http.Request, req *totpCodeRequest) bool {
	if req.UserID <= 0 {
		s.writeTotpValidation(w, r, "user_id 必须是正整数")
		return false
	}
	if strings.TrimSpace(req.SecretRef) == "" {
		s.writeTotpValidation(w, r, "secret_ref 不能为空")
		return false
	}
	if strings.TrimSpace(req.Code) == "" {
		s.writeTotpValidation(w, r, "code 不能为空")
		return false
	}
	return true
}

// checkTotpCode 解密种子并验码。done=true 表示已经写过响应，调用方直接返回。
// 调用前必须已经过 validateTotpCodeInput（code 非空由那里保证）。
func (s *Server) checkTotpCode(w http.ResponseWriter, r *http.Request, req *totpCodeRequest) (step int64, matched, done bool) {
	plaintext, err := s.opts.Totp.Resolve(r.Context(), req.SecretRef)
	if err != nil {
		switch {
		case errors.Is(err, agentstore.ErrSecretNotFound):
			httpx.WriteError(w, r, http.StatusNotFound, httpx.CodeNotFound, "这条二次验证登记不存在或已销毁")
		case errors.Is(err, agentstore.ErrSecretExpired):
			s.writeTotpValidation(w, r, "这条二次验证登记已经过期，请重新开始")
		default:
			s.writeTotpInternal(w, r, "读取种子失败", err)
		}
		return 0, false, true
	}
	defer wipeBytes(plaintext)

	step, matched = totp.MatchStep(string(plaintext), strings.TrimSpace(req.Code), time.Now().UTC())
	return step, matched, false
}

// totpBinding 核对一条种子的用途与归属。不通过时已写好响应并返回 ok=false。
func (s *Server) totpBinding(w http.ResponseWriter, r *http.Request, req *totpCodeRequest) (purpose, reference string, ok bool) {
	if req.UserID <= 0 {
		s.writeTotpValidation(w, r, "user_id 必须是正整数")
		return "", "", false
	}
	if strings.TrimSpace(req.SecretRef) == "" {
		s.writeTotpValidation(w, r, "secret_ref 不能为空")
		return "", "", false
	}
	purpose, reference, found, err := s.opts.Totp.Binding(r.Context(), req.SecretRef)
	if err != nil {
		s.writeTotpInternal(w, r, "核对种子归属失败", err)
		return "", "", false
	}
	if !found {
		httpx.WriteError(w, r, http.StatusNotFound, httpx.CodeNotFound, "这条二次验证登记不存在或已销毁")
		return "", "", false
	}
	if purpose != totpPurpose {
		// 拿一张 TLS 私钥的引用当 TOTP 种子用，必须在这里被挡住，而不是在"解出来不像码"时。
		s.writeTotpValidation(w, r, fmt.Sprintf("引用 %s 不是二次验证种子（用途 %s）", req.SecretRef, purpose))
		return "", "", false
	}
	return purpose, reference, true
}

// totpReady 做通用前置：接口是否接线、方法是否允许。返回 false 表示已写好响应。
func (s *Server) totpReady(w http.ResponseWriter, r *http.Request, method string) bool {
	if s.opts.Totp == nil {
		// 没接线就 404：一个"登记了却走不通"的接口比不存在的接口更难查。
		httpx.WriteError(w, r, http.StatusNotFound, httpx.CodeNotFound, "代理未启用二次验证")
		return false
	}
	if r.Method != method {
		w.Header().Set("Allow", method)
		httpx.WriteError(w, r, http.StatusMethodNotAllowed, httpx.CodeValidationError,
			"只支持 "+method)
		return false
	}
	if ct := r.Header.Get("Content-Type"); ct != "" && !jsonContentType(ct) {
		httpx.WriteError(w, r, http.StatusUnsupportedMediaType, httpx.CodeValidationError,
			"请求体必须是 application/json")
		return false
	}
	return true
}

// decodeTotpBody 读并严格解码请求体。失败时已写好响应。
func (s *Server) decodeTotpBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	body, ok := readLimitedBody(w, r, totpRequestBytes)
	if !ok {
		return false
	}
	defer wipeBytes(body)
	if err := decodeStrictObject(body, dst); err != nil {
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationError, err.Error())
		return false
	}
	return true
}

func (s *Server) writeTotpValidation(w http.ResponseWriter, r *http.Request, message string) {
	httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationError, message)
}

func (s *Server) writeTotpForbidden(w http.ResponseWriter, r *http.Request, message string) {
	httpx.WriteError(w, r, http.StatusForbidden, httpx.CodeForbidden, message)
}

func (s *Server) writeTotpInternal(w http.ResponseWriter, r *http.Request, what string, err error) {
	s.opts.Logger.Error(what, "error", err, "request_id", httpx.RequestID(r))
	httpx.WriteError(w, r, http.StatusInternalServerError, httpx.CodeInternalError, what)
}
