// 秘密脱敏（P02.4）：日志是最容易泄漏秘密的地方——一次 `logger.Error("登录失败", "error", err)`
// 就可能把口令带进 journal。这里不依赖调用方记性，而是把脱敏做成 logger 的**固有行为**：
// 所有经本包构造的 logger 在写出之前都会过一遍 redactingHandler。
//
// 两层防线：
//  1. 按键名（password、token、csrf、cookie、secret、key…）整值替换；
//  2. 按值形态（argon2id 哈希、Bearer 令牌、`password=…` 形式的查询串/错误文本）替换片段。
//
// 明确知道是秘密的值可以用 `logging.Secret` 包装，无论键名如何都会被替换。
package logging

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
)

// Redacted 是脱敏后的替代文本。故意保持短小，便于在日志里检索“这里被抹掉了什么”。
const Redacted = "[redacted]"

// secretKeyFragments 命中即视为秘密字段。宁可多抹，也不把秘密写进 journal。
//
// 注意不收录过短的通用词（例如单独的 "key"），否则 "socket_key_file" 之类正常字段
// 也会被抹掉，反而降低日志可用性；真正敏感的键名都会被下面的片段命中。
var secretKeyFragments = []string{
	"password", "passwd", "secret", "token", "csrf", "cookie", "credential",
	"private_key", "api_key", "apikey", "authorization", "session", "password_hash",
	"agent_key", "passphrase",
}

// secretValuePatterns 是“值形态”规则：即使键名无关，也要在文本里抹掉这些形态。
var secretValuePatterns = []*regexp.Regexp{
	// Argon2id / bcrypt 哈希。
	regexp.MustCompile(`\$argon2[a-z]*\$[^\s"']+`),
	regexp.MustCompile(`\$2[aby]\$[^\s"']+`),
	// 形如 password=abc、password="a b"、token: xyz 的内嵌秘密（常见于 URL 查询串与错误文本）。
	regexp.MustCompile(`(?i)\b(password|passwd|secret|token|csrf|api_key|apikey|authorization)\s*=\s*("[^"]*"|'[^']*'|[^\s&"']+)`),
	regexp.MustCompile(`(?i)\b(password|passwd|secret|token|csrf)\s*:\s*([^\s,}"']+)`),
	// Bearer 令牌。
	regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._\-]+`),
}

// secretMarker 是 Secret 返回值的内部类型：渲染时固定替换。
type secretMarker struct{}

// Secret 把一个已知秘密包起来。日志渲染时它永远显示为 [redacted]，
// 即使键名看起来无关（例如把口令放在 "value" 字段里）。
func Secret(s string) slog.Value {
	return slog.AnyValue(secretMarker{})
}

// redactingHandler 在任何 slog.Handler 外面套一层脱敏。
type redactingHandler struct {
	inner slog.Handler
	// groups 记录当前 WithGroup 产生的组名（仅用于日志与测试断言，不参与判断）。
	groups []string
}

// withRedaction 用脱敏包装 handler。
func withRedaction(inner slog.Handler) slog.Handler {
	return &redactingHandler{inner: inner}
}

func (h *redactingHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h *redactingHandler) Handle(ctx context.Context, r slog.Record) error {
	out := slog.NewRecord(r.Time, r.Level, redactText(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(redactAttr(a))
		return true
	})
	return h.inner.Handle(ctx, out)
}

func (h *redactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	safe := make([]slog.Attr, 0, len(attrs))
	for _, a := range attrs {
		safe = append(safe, redactAttr(a))
	}
	return &redactingHandler{inner: h.inner.WithAttrs(safe), groups: h.groups}
}

func (h *redactingHandler) WithGroup(name string) slog.Handler {
	groups := append(append([]string(nil), h.groups...), name)
	return &redactingHandler{inner: h.inner.WithGroup(name), groups: groups}
}

// redactAttr 按键名与值形态处理一个属性，组属性递归处理。
func redactAttr(a slog.Attr) slog.Attr {
	a.Key = redactText(a.Key)
	v := a.Value.Resolve()

	if _, ok := v.Any().(secretMarker); ok {
		return slog.String(a.Key, Redacted)
	}
	if isSecretKey(a.Key) {
		return slog.String(a.Key, Redacted)
	}
	switch v.Kind() {
	case slog.KindGroup:
		group := v.Group()
		safe := make([]slog.Attr, 0, len(group))
		for _, ga := range group {
			safe = append(safe, redactAttr(ga))
		}
		a.Value = slog.GroupValue(safe...)
		return a
	case slog.KindString:
		a.Value = slog.StringValue(redactText(v.String()))
		return a
	case slog.KindAny:
		// error 与其他任意值统一按文本处理（error 的 Error() 文本里可能带查询串）。
		if err, ok := v.Any().(error); ok && err != nil {
			a.Value = slog.StringValue(redactText(err.Error()))
			return a
		}
		a.Value = slog.StringValue(redactText(toString(v.Any())))
		return a
	default:
		return a
	}
}

// isSecretKey 判断键名是否命秘密片段（大小写不敏感）。
func isSecretKey(key string) bool {
	lower := strings.ToLower(key)
	for _, frag := range secretKeyFragments {
		if strings.Contains(lower, frag) {
			return true
		}
	}
	return false
}

// Redact 抹掉任意文本里的秘密形态，并报告是否真的抹掉过东西。
//
// 给日志包以外的读取路径用（P05.3 的 journal 消息）：它们读到的是别人的文本，
// 不经过 logger，但同样可能带着口令与令牌。第二个返回值让调用方能在 DTO 里标
// “这条被过滤过”——过滤是尽力措施，把是否过滤过说出来才是诚实的做法
// （PLAN.md 第 14 节：过滤秘密不能作为向 viewer 开放日志的依据）。
func Redact(s string) (string, bool) {
	if s == "" {
		return s, false
	}
	out := redactText(s)
	return out, out != s
}

// redactText 抹掉文本里的秘密形态。
func redactText(s string) string {
	if s == "" {
		return s
	}
	out := s
	for _, re := range secretValuePatterns {
		out = re.ReplaceAllStringFunc(out, func(match string) string {
			// 保留 "key=" 前缀，只抹掉值，便于看出是哪一类秘密。
			if idx := strings.IndexAny(match, "=:"); idx > 0 {
				return match[:idx+1] + Redacted
			}
			return Redacted
		})
	}
	return out
}

// toString 把任意值转成文本；nil 返回空串。
func toString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	if sv, ok := v.(interface{ String() string }); ok {
		return sv.String()
	}
	return strings.TrimSpace(fmt.Sprint(v))
}
