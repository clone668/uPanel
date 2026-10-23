package certificates

// 证书动作的错误与判决（P10.3）。
//
// 与 internal/proxies/httperr.go 同一套约定：执行层不按错误文本猜状态码，只问
// 「这次拒绝对应哪个状态码」。区别只在阻塞码的归类——证书的阻塞几乎全是
// 「前置条件不满足」（certbot 没装、域名解析不到、80 端口不通、私钥没上传），
// 那些是 409；只有参数形状不合法是 400。

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"upanel/internal/httpx"
	"upanel/internal/proxies"
)

var (
	// ErrInvalidParams 表示参数形状不合法。
	ErrInvalidParams = errors.New("certificates: 参数不合法")
	// ErrMaterializeFailed 表示证书或私钥没能安全落盘。
	ErrMaterializeFailed = errors.New("certificates: 证书落盘失败")
	// ErrCertbotFailed 表示 certbot 没有成功。
	ErrCertbotFailed = errors.New("certificates: certbot 未成功")
)

// ParamError 是一个字段级的参数错误。
type ParamError struct {
	Field   string
	Message string
}

// Error 实现 error。
func (e *ParamError) Error() string { return e.Message }

// invalidParams 构造一个字段级参数错误。
func invalidParams(field, format string, args ...any) error {
	return &ParamError{Field: field, Message: fmt.Sprintf(format, args...)}
}

// httpError 是一次带判决的拒绝（复用代理侧的类型，让代理层只认一种形状）。
func httpError(status int, code httpx.Code, format string, args ...any) error {
	return &proxies.HTTPError{Status: status, Code: code, Message: fmt.Sprintf(format, args...)}
}

// dependencyBlocked 表示前置条件不满足。
func dependencyBlocked(blockCode, format string, args ...any) error {
	return httpError(http.StatusConflict, httpx.CodeDependencyBlocked,
		"%s（%s）", fmt.Sprintf(format, args...), blockCode)
}

// commandFailed 表示动作跑了但没有得到可核验的结果。
func commandFailed(format string, args ...any) error {
	return httpError(http.StatusInternalServerError, httpx.CodeCommandFailed, format, args...)
}

// blockedBy 把一组阻塞项变成一个依赖类错误（与 proxy.apply 的形状一致：
// 多条的文案用「；」连起来，码放在括号里，测试按码断言）。
func blockedBy(blockers []proxies.Blocker) error {
	if len(blockers) == 0 {
		return nil
	}
	messages := make([]string, 0, len(blockers))
	codes := make([]string, 0, len(blockers))
	dependency := false
	for _, b := range blockers {
		messages = append(messages, b.Message)
		codes = append(codes, b.Code)
		switch b.Code {
		case CodeCertParams:
			// 参数类：维持 400。
		default:
			dependency = true
		}
	}
	if dependency {
		return dependencyBlocked(strings.Join(codes, ","), "%s", strings.Join(messages, "；"))
	}
	return httpError(http.StatusBadRequest, httpx.CodeValidationError,
		"%s（%s）", strings.Join(messages, "；"), strings.Join(codes, ","))
}
