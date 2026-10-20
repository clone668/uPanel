// Package httpx 提供两个服务共用的 HTTP 响应约定：数据信封、错误信封、错误码、request_id。
//
// 契约来源：CONTRACTS.md 第 1 节。所有响应都是
//
//	{"data": ..., "request_id": "..."}                         成功
//	{"error": {"code","message","details"}, "request_id": "..."} 失败
//
// 并总是返回 X-Request-ID 响应头。request_id 在 P01.2 起由中间件生成并写入请求上下文，
// 响应写出时从上下文取，保证同一次请求内日志、响应头、响应体三处一致。
package httpx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
)

// HeaderRequestID 是请求/响应使用的 request id 头。
const HeaderRequestID = "X-Request-ID"

// Code 是契约里的错误码（CONTRACTS.md 第 1 节枚举，24 个）。
//
// 这是封闭枚举：不要用临时字符串代替。前端按 code 分支，新增 code 必须先改契约。
type Code string

const (
	CodeValidationError       Code = "VALIDATION_ERROR"
	CodeUnauthenticated       Code = "UNAUTHENTICATED"
	CodeForbidden             Code = "FORBIDDEN"
	CodeReauthRequired        Code = "REAUTH_REQUIRED"
	CodeCSRFFailed            Code = "CSRF_FAILED"
	CodeNotFound              Code = "NOT_FOUND"
	CodeVersionConflict       Code = "VERSION_CONFLICT"
	CodeResourceBusy          Code = "RESOURCE_BUSY"
	CodeDeviceInUse           Code = "DEVICE_IN_USE"
	CodeProtectedDevice       Code = "PROTECTED_DEVICE"
	CodeUnsupportedDevice     Code = "UNSUPPORTED_DEVICE"
	CodeDeviceIdentityChanged Code = "DEVICE_IDENTITY_CHANGED"
	CodePathOutsideRoot       Code = "PATH_OUTSIDE_ROOT"
	CodeDependencyBlocked     Code = "DEPENDENCY_BLOCKED"
	CodePreflightExpired      Code = "PREFLIGHT_EXPIRED"
	CodePreflightStale        Code = "PREFLIGHT_STALE"
	CodeConfirmationMismatch  Code = "CONFIRMATION_MISMATCH"
	CodeIdempotencyConflict   Code = "IDEMPOTENCY_CONFLICT"
	CodeAgentUnavailable      Code = "AGENT_UNAVAILABLE"
	CodeDependencyMissing     Code = "DEPENDENCY_MISSING"
	CodeNotImplemented        Code = "NOT_IMPLEMENTED"
	CodeCommandFailed         Code = "COMMAND_FAILED"
	CodeOperationInterrupted  Code = "OPERATION_INTERRUPTED"
	CodeRollbackFailed        Code = "ROLLBACK_FAILED"
	// CodeInternalError 不在契约枚举里，只用于“程序本身出错”（panic、序列化失败）。
	// 契约枚举描述的是可预期的业务拒绝；真正的 500 不应伪装成其中任何一个。
	CodeInternalError Code = "INTERNAL_ERROR"
)

// Codes 返回契约枚举（不含 CodeInternalError）。测试用它校验完整性。
func Codes() []Code {
	return []Code{
		CodeValidationError, CodeUnauthenticated, CodeForbidden, CodeReauthRequired,
		CodeCSRFFailed, CodeNotFound, CodeVersionConflict, CodeResourceBusy,
		CodeDeviceInUse, CodeProtectedDevice, CodeUnsupportedDevice, CodeDeviceIdentityChanged,
		CodePathOutsideRoot, CodeDependencyBlocked, CodePreflightExpired, CodePreflightStale,
		CodeConfirmationMismatch, CodeIdempotencyConflict, CodeAgentUnavailable,
		CodeDependencyMissing, CodeNotImplemented, CodeCommandFailed,
		CodeOperationInterrupted, CodeRollbackFailed,
	}
}

// StatusForCode 把错误码映射到默认 HTTP 状态码。
//
// 依据 CONTRACTS.md 第 1 节的状态码约定：400/401/403/404/409/413/422/429/501/503。
// 调用方仍可显式传状态码覆盖（例如请求体超限用 413 配 VALIDATION_ERROR）。
func StatusForCode(code Code) int {
	switch code {
	case CodeValidationError, CodeConfirmationMismatch:
		// 确认短语不符是“请求内容不对”，不是“状态冲突”：CONTRACTS §4.2 与
		// api/openapi.yaml 都写明 400 CONFIRMATION_MISMATCH，代理侧也直接回 400。
		// 早先把它归到 409 那组，会让公开 API 与契约差一个状态码。
		return http.StatusBadRequest
	case CodeUnauthenticated:
		return http.StatusUnauthorized
	case CodeForbidden, CodeReauthRequired, CodeCSRFFailed:
		return http.StatusForbidden
	case CodeNotFound:
		return http.StatusNotFound
	case CodeVersionConflict, CodeResourceBusy, CodeDeviceInUse,
		CodePreflightExpired, CodePreflightStale,
		CodeIdempotencyConflict, CodeDeviceIdentityChanged, CodeOperationInterrupted:
		return http.StatusConflict
	case CodeProtectedDevice, CodeUnsupportedDevice, CodePathOutsideRoot, CodeDependencyBlocked:
		return http.StatusUnprocessableEntity
	case CodeAgentUnavailable, CodeDependencyMissing:
		return http.StatusServiceUnavailable
	case CodeNotImplemented:
		return http.StatusNotImplemented
	case CodeCommandFailed, CodeRollbackFailed, CodeInternalError:
		return http.StatusInternalServerError
	default:
		// 未知 code 属于编程错误：不要猜一个 400 糊过去。
		return http.StatusInternalServerError
	}
}

// maxRequestIDLen 限制外部传入 request id 的长度，避免被用作日志注入载体。
const maxRequestIDLen = 64

type requestIDKey struct{}

// WithRequestID 把 request id 放进上下文。中间件在入口处调用一次。
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// Envelope 是成功响应信封。
type Envelope struct {
	Data      any    `json:"data"`
	RequestID string `json:"request_id"`
}

// ErrorBody 是错误内容。Details 为 nil 时省略。
type ErrorBody struct {
	Code    Code   `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details,omitempty"`
}

// ErrorEnvelope 是错误响应信封。
type ErrorEnvelope struct {
	Error     ErrorBody `json:"error"`
	RequestID string    `json:"request_id"`
}

// RequestID 返回本次请求的 request id：优先取上下文（中间件已生成），
// 其次用调用方传入的合法头值，最后现生成一个。
//
// 非法头值（空、超长、含非可视字符）一律丢弃并重新生成，不做截断后复用。
func RequestID(r *http.Request) string {
	if r == nil {
		return newRequestID()
	}
	if v, ok := r.Context().Value(requestIDKey{}).(string); ok && validRequestID(v) {
		return v
	}
	if v := r.Header.Get(HeaderRequestID); validRequestID(v) {
		return v
	}
	return newRequestID()
}

// InboundRequestID 返回调用方传入的合法 request id；没有或不合法时返回空串。
// 中间件用它决定“沿用还是新生成”，从而区分“客户端指定”与“服务端生成”。
func InboundRequestID(r *http.Request) string {
	if r == nil {
		return ""
	}
	v := r.Header.Get(HeaderRequestID)
	if !validRequestID(v) {
		return ""
	}
	return v
}

// NewRequestID 生成一个新的 request id（16 字节随机值的十六进制）。
func NewRequestID() string { return newRequestID() }

func validRequestID(v string) bool {
	if v == "" || len(v) > maxRequestIDLen {
		return false
	}
	for _, c := range v {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '_', c == '.':
		default:
			return false
		}
	}
	return true
}

func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 随机源不可用时不能返回空 id：用固定前缀保证仍然是合法可见值，
		// 同时由调用方日志暴露该异常。
		return "req-unavailable-random"
	}
	return hex.EncodeToString(b[:])
}

// WriteData 写出成功响应。
func WriteData(w http.ResponseWriter, r *http.Request, status int, data any) {
	rid := RequestID(r)
	write(w, status, rid, Envelope{Data: data, RequestID: rid})
}

// WriteError 写出错误响应，状态码由调用方给定。
// message 为中文可读文本，不得包含堆栈、路径或秘密。
func WriteError(w http.ResponseWriter, r *http.Request, status int, code Code, message string) {
	WriteErrorDetails(w, r, status, code, message, nil)
}

// WriteErrorDetails 写出带 details 的错误响应。
func WriteErrorDetails(w http.ResponseWriter, r *http.Request, status int, code Code, message string, details any) {
	rid := RequestID(r)
	write(w, status, rid, ErrorEnvelope{
		Error:     ErrorBody{Code: code, Message: message, Details: details},
		RequestID: rid,
	})
}

// WriteErrorCode 按错误码的默认状态码写出错误响应。
func WriteErrorCode(w http.ResponseWriter, r *http.Request, code Code, message string) {
	WriteError(w, r, StatusForCode(code), code, message)
}

func write(w http.ResponseWriter, status int, rid string, body any) {
	payload, err := json.Marshal(body)
	if err != nil {
		// 序列化失败属于编程错误：返回 500 且不输出半截内容。
		slog.Error("响应序列化失败", "error", err, "request_id", rid)
		writeRaw(w, http.StatusInternalServerError, rid, serializationFailure(rid))
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	// 使用 Header.Set：Go 会把键规范化为 X-Request-Id。HTTP 头名大小写不敏感，
	// 契约里的 X-Request-ID 指的就是这个头；这里保留规范化形式，避免 Go 侧
	// Header.Get 取不到值的隐患（非规范键在 map 中读取不到）。
	h.Set(HeaderRequestID, rid)
	w.WriteHeader(status)
	_, _ = w.Write(payload)
	_, _ = w.Write([]byte("\n"))
}

func writeRaw(w http.ResponseWriter, status int, rid string, payload []byte) {
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set(HeaderRequestID, rid)
	w.WriteHeader(status)
	_, _ = w.Write(payload)
	_, _ = w.Write([]byte("\n"))
}

func serializationFailure(rid string) []byte {
	body := ErrorEnvelope{
		Error:     ErrorBody{Code: CodeInternalError, Message: "响应序列化失败"},
		RequestID: rid,
	}
	b, err := json.Marshal(body)
	if err != nil {
		// 理论上不可达：结构体的字段都是 string。
		return []byte(`{"error":{"code":"INTERNAL_ERROR","message":"响应序列化失败"}}`)
	}
	return b
}
