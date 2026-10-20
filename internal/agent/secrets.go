package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"upanel/internal/agentstore"
	"upanel/internal/httpx"
)

// maxSecretRequestBytes 是 POST /internal/v1/secrets 的请求体上限。
//
// 组成部分：明文上限（agentstore.MaxSecretValueBytes）+ 引用与用途字段 + JSON 转义余量
// （非 ASCII 字符会被转义成 6 字节，留 3 倍余量足够）。
const maxSecretRequestBytes = 3*agentstore.MaxSecretValueBytes + 4<<10

// SecretStore 是代理需要的秘密存储能力。
//
// 只有写入与到期销毁：**没有**读取明文的接口，因为协议里也不存在读取明文的 HTTP 路由
// （CONTRACTS.md 第 15 节）。解密路径只在代理内部流程里使用（agentstore.Secrets.Resolve）。
type SecretStore interface {
	Put(ctx context.Context, in agentstore.PutInput) (agentstore.Secret, error)
	DestroyExpired(ctx context.Context, now time.Time) (int, error)
}

// secretRequest 是 POST /internal/v1/secrets 的请求体。
//
// 只有用途、明文与可选引用：没有用途之外的类型标签，也没有任意键值对。
type secretRequest struct {
	Purpose string `json:"purpose"`
	// Value 是明文。这里用 string 而不是 []byte：encoding/json 会把 []byte 当成 base64，
	// 那样传 "password" 会被当成非法 base64 拒绝，语义也不直观。
	Value string `json:"value"`
	// Reference 是引用这份秘密的配置标识；为空时是 10 分钟后销毁的临时秘密。
	Reference string `json:"reference"`
}

// secretResponse 是成功写入后的响应：只有引用与生命周期信息，**没有明文**。
type secretResponse struct {
	SecretRef string `json:"secret_ref"`
	Purpose   string `json:"purpose"`
	State     string `json:"state"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

// handleSecrets 加密保存一条秘密并返回引用。
func (s *Server) handleSecrets(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		// 明确拒绝 GET：协议里没有读取明文的入口，这里也不能给出“只读看看”的错觉。
		w.Header().Set("Allow", "POST")
		httpx.WriteError(w, r, http.StatusMethodNotAllowed, httpx.CodeValidationError,
			"只支持 POST（不提供读取秘密的接口）")
		return
	}
	if ct := r.Header.Get("Content-Type"); ct != "" && !jsonContentType(ct) {
		httpx.WriteError(w, r, http.StatusUnsupportedMediaType, httpx.CodeValidationError,
			"请求体必须是 application/json")
		return
	}

	body, ok := readLimitedBody(w, r, maxSecretRequestBytes)
	if !ok {
		return
	}
	// 请求体里有明文：解析完立刻清零副本，避免明文在内存里多留一段时间。
	defer wipeBytes(body)

	var req secretRequest
	if err := decodeStrictObject(body, &req); err != nil {
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationError, err.Error())
		return
	}

	purpose, found := agentstore.LookupPurpose(req.Purpose)
	if !found {
		s.opts.Logger.Warn("拒绝未登记的秘密用途",
			"purpose", req.Purpose, "request_id", httpx.RequestID(r))
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationError,
			fmt.Sprintf("未登记的用途 %q", req.Purpose))
		return
	}
	if !purpose.Uploadable {
		s.opts.Logger.Warn("拒绝通过通用接口上传内部用途的秘密",
			"purpose", req.Purpose, "request_id", httpx.RequestID(r))
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationError,
			fmt.Sprintf("用途 %q 只能由代理内部流程写入", req.Purpose))
		return
	}

	value := []byte(req.Value)
	defer wipeBytes(value)

	secret, err := s.opts.Secrets.Put(r.Context(), agentstore.PutInput{
		Purpose:   purpose.Name,
		Value:     value,
		Reference: req.Reference,
	})
	if err != nil {
		s.writeSecretError(w, r, err)
		return
	}

	resp := secretResponse{
		SecretRef: secret.Ref,
		Purpose:   secret.Purpose,
		State:     secret.State,
	}
	if !secret.ExpiresAt.IsZero() {
		resp.ExpiresAt = secret.ExpiresAt.UTC().Format(time.RFC3339)
	}
	httpx.WriteData(w, r, http.StatusCreated, resp)
}

// writeSecretError 把存储层错误映射成契约错误码。
//
// 校验类错误回 400 并带上明确原因（这些原因都不含明文）；其余一律 500，只记日志。
func (s *Server) writeSecretError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, agentstore.ErrPurpose),
		errors.Is(err, agentstore.ErrValueSize),
		errors.Is(err, agentstore.ErrReference):
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationError, err.Error())
	default:
		s.opts.Logger.Error("保存加密秘密失败",
			"request_id", httpx.RequestID(r), "error", err)
		httpx.WriteError(w, r, http.StatusInternalServerError, httpx.CodeInternalError, "保存秘密失败")
	}
}

// wipeBytes 清零字节切片。
func wipeBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// sweepInterval 是到期临时秘密的扫描间隔。
//
// 秘密的销毁时间精确到毫秒，扫描间隔只影响“最多晚多久真正销毁”，30 秒远小于 10 分钟
// 的存活期，也不会给数据库带来可见负载。
const sweepInterval = 30 * time.Second

// sweepExpiredSecrets 执行一次到期销毁，并把结果写进日志。
func (s *Server) sweepExpiredSecrets(ctx context.Context, now time.Time) (int, error) {
	if s.opts.Secrets == nil {
		return 0, errors.New("agent: 没有配置秘密存储")
	}
	removed, err := s.opts.Secrets.DestroyExpired(ctx, now)
	if err != nil {
		s.opts.Logger.Error("销毁到期临时秘密失败", "error", err)
		return removed, err
	}
	if removed > 0 {
		s.opts.Logger.Info("已销毁到期临时秘密", "count", removed, "at", now.UTC().Format(time.RFC3339))
	}
	return removed, nil
}

// SweepExpiredSecrets 立即扫描一次（供运维与测试调用）。
func (s *Server) SweepExpiredSecrets(ctx context.Context) (int, error) {
	return s.sweepExpiredSecrets(ctx, time.Now())
}

// runSecretSweeper 周期性销毁到期临时秘密，直到 ctx 结束。
func (s *Server) runSecretSweeper(ctx context.Context) {
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			// 单次失败不退出：下一轮还会再试，失败本身已经记过日志。
			_, _ = s.sweepExpiredSecrets(ctx, now)
		}
	}
}
