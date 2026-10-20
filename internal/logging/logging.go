// Package logging 建立结构化日志。日志统一写入 stderr，由 systemd journal 收集
// （PLAN.md 第 4 节：日志走 systemd journal，应用中只展示经权限筛选的日志）。
//
// 秘密脱敏是本包的固有行为（P02.4）：所有经 New 构造的 logger 都套了 redactingHandler，
// 按键名（password/token/csrf/cookie/secret…）与值形态（argon2id 哈希、Bearer 令牌、
// `password=…` 形式）替换为 [redacted]，见 redact.go。调用方仍不应主动传秘密；
// 这层是兜底，不是许可。
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

// Format 是日志输出格式。
type Format string

const (
	// FormatJSON 为每行一个 JSON 对象，生产默认。
	FormatJSON Format = "json"
	// FormatText 为 logfmt 风格，便于人工阅读。
	FormatText Format = "text"
)

// Options 是构造 logger 的参数。
type Options struct {
	// Program 会作为固定字段写入每条日志，例如 upanel-web。
	Program string
	// Level 取 debug、info、warn、error；空值按 info。
	Level string
	// Format 取 json、text；空值按 json。
	Format string
	// AddSource 为 true 时附加源码位置，仅调试使用。
	AddSource bool
	// Output 为空时写 os.Stderr。
	Output io.Writer
}

// ParseLevel 解析日志级别，拒绝未知值（不做静默降级）。
func ParseLevel(v string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("未知日志级别 %q：只支持 debug、info、warn、error", v)
	}
}

// ParseFormat 解析日志格式，拒绝未知值。
func ParseFormat(v string) (Format, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", string(FormatJSON):
		return FormatJSON, nil
	case string(FormatText):
		return FormatText, nil
	default:
		return "", fmt.Errorf("未知日志格式 %q：只支持 json 或 text", v)
	}
}

// New 按选项构造 logger。返回的错误必须导致进程拒绝启动，不能退化成无日志运行。
func New(opts Options) (*slog.Logger, error) {
	level, err := ParseLevel(opts.Level)
	if err != nil {
		return nil, err
	}
	format, err := ParseFormat(opts.Format)
	if err != nil {
		return nil, err
	}

	out := opts.Output
	if out == nil {
		out = os.Stderr
	}
	handlerOpts := &slog.HandlerOptions{Level: level, AddSource: opts.AddSource}

	var handler slog.Handler
	switch format {
	case FormatText:
		handler = slog.NewTextHandler(out, handlerOpts)
	default:
		handler = slog.NewJSONHandler(out, handlerOpts)
	}

	logger := slog.New(withRedaction(handler))
	if opts.Program != "" {
		logger = logger.With(slog.String("program", opts.Program))
	}
	return logger, nil
}
