// Package logs 是 Web 侧的日志读取端（P05.3）。
//
// 它只做三件事：把参数交给代理的 logs.query、把结果校验成 journal.Page、
// 把一条实时流翻译成逐条 Entry。所有与 journalctl 打交道的事情都在代理里，
// Web 进程不执行任何系统命令。
package logs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"upanel/internal/agentclient"
	"upanel/internal/journal"
)

// ErrAgentUnavailable 表示读不到日志是因为代理不可用（连不上、超时、响应不合法）。
var ErrAgentUnavailable = errors.New("logs: 代理不可用")

// ErrStreamInterrupted 表示流在没有结束标记的情况下断了。
//
// 它与“正常结束”必须分开：正常结束时服务端一定发过 type=end，
// 没有 end 就说明中间断了，页面应当显示“可能缺一段”而不是“已结束”。
var ErrStreamInterrupted = errors.New("logs: 日志流在没有结束标记的情况下中断")

// Agent 是 Web 侧需要的代理能力。生产实现是 *agentclient.Client。
type Agent interface {
	Query(ctx context.Context, name string, params any) (json.RawMessage, error)
	CreateStream(ctx context.Context, purpose string, params any) (agentclient.StreamPermit, error)
	OpenStream(ctx context.Context, streamID string) (*agentclient.StreamReader, error)
}

// Fetcher 是按需查询日志的客户端。Web 自己不做缓存：日志是“此刻的事实”，
// 缓存只会让页面显示的时间范围与实际读到的条目对不上。
type Fetcher struct {
	agent  Agent
	logger *slog.Logger
}

// NewFetcher 构造 Fetcher。agent 为 nil 时返回错误：宁可启动时报错，
// 也不要运行时用一个永远返回空列表的假实现顶上。
func NewFetcher(agent Agent, logger *slog.Logger) (*Fetcher, error) {
	if agent == nil {
		return nil, errors.New("logs: Fetcher 需要代理客户端")
	}
	return &Fetcher{agent: agent, logger: logger}, nil
}

// Query 查一页日志。
func (f *Fetcher) Query(ctx context.Context, params journal.Params) (journal.Page, error) {
	if err := params.Validate(); err != nil {
		return journal.Page{}, err
	}
	raw, err := f.agent.Query(ctx, journal.QueryName, params)
	if err != nil {
		return journal.Page{}, f.mapError(err, "查询日志")
	}
	var page journal.Page
	if err := json.Unmarshal(raw, &page); err != nil {
		return journal.Page{}, fmt.Errorf("%w: 代理返回的日志页不是合法 JSON: %v", ErrAgentUnavailable, err)
	}
	if err := validatePage(params, page); err != nil {
		return journal.Page{}, err
	}
	return page, nil
}

// Stream 打开一条实时流，把每条 Entry 交给 emit，直到流结束。
//
// 返回 nil 表示收到了 type=end（原因见 endReason 参数）。
func (f *Fetcher) Stream(ctx context.Context, params journal.Params, emit func(journal.Entry) error) error {
	_, err := f.StreamWithReason(ctx, params, emit)
	return err
}

// StreamWithReason 与 Stream 相同，但额外返回服务端给出的结束原因。
func (f *Fetcher) StreamWithReason(ctx context.Context, params journal.Params, emit func(journal.Entry) error) (journal.StreamEnd, error) {
	if err := params.Validate(); err != nil {
		return "", err
	}
	permit, err := f.agent.CreateStream(ctx, agentclient.LogsStreamPurpose(), params)
	if err != nil {
		return "", f.mapError(err, "申请日志流许可")
	}
	reader, err := f.agent.OpenStream(ctx, permit.StreamID)
	if err != nil {
		return "", f.mapError(err, "打开日志流")
	}
	defer func() { _ = reader.Close() }()

	for {
		frame, err := reader.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return "", ErrStreamInterrupted
			}
			var unavailable *agentclient.UnavailableError
			if errors.As(err, &unavailable) {
				return "", fmt.Errorf("%w: %v", ErrAgentUnavailable, err)
			}
			return "", fmt.Errorf("logs: 读取日志流失败: %w", err)
		}
		switch frame.Type {
		case "entry":
			entry, err := decodeEntry(frame.Entry)
			if err != nil {
				return "", err
			}
			if err := emit(entry); err != nil {
				return "", err
			}
		case "end":
			if frame.Reason == "" {
				return "", fmt.Errorf("%w: 结束帧没有给出原因", ErrStreamInterrupted)
			}
			return journal.StreamEnd(frame.Reason), nil
		case "error":
			return "", fmt.Errorf("logs: 代理报告日志流失败: %s %s", frame.Code, frame.Message)
		default:
			// 未知帧类型不能静默忽略：那会让协议演进时旧客户端“看起来正常”。
			return "", fmt.Errorf("%w: 未知的流帧类型 %q", ErrStreamInterrupted, frame.Type)
		}
	}
}

// decodeEntry 解析一帧里的 Entry 并校验必需字段。
func decodeEntry(raw json.RawMessage) (journal.Entry, error) {
	if len(raw) == 0 {
		return journal.Entry{}, fmt.Errorf("%w: entry 帧没有内容", ErrStreamInterrupted)
	}
	var entry journal.Entry
	if err := json.Unmarshal(raw, &entry); err != nil {
		return journal.Entry{}, fmt.Errorf("%w: entry 帧无法解析: %v", ErrStreamInterrupted, err)
	}
	if entry.Cursor == "" || entry.Timestamp.IsZero() {
		return journal.Entry{}, fmt.Errorf("%w: entry 帧缺少 cursor 或 timestamp", ErrStreamInterrupted)
	}
	return entry, nil
}

// validatePage 校验代理返回的日志页自洽。
//
// 与 services/storage 同一套规则：缺了“时间窗口”这类必需字段，宁可报错，
// 也不给一份看起来正常的空列表——页面会把它当成“这段时间没有日志”。
func validatePage(params journal.Params, page journal.Page) error {
	if page.From.IsZero() || page.To.IsZero() || page.From.After(page.To) {
		return fmt.Errorf("%w: 代理返回的日志页缺少有效的时间窗口", ErrAgentUnavailable)
	}
	switch page.Source {
	case journal.SourceJournal, journal.SourceKernel:
	default:
		return fmt.Errorf("%w: 代理返回的日志页数据源 %q 不在白名单内", ErrAgentUnavailable, page.Source)
	}
	if page.Unit != "" && page.Unit != params.Unit {
		return fmt.Errorf("%w: 代理返回的 unit %q 与请求的 %q 不一致", ErrAgentUnavailable, page.Unit, params.Unit)
	}
	for i, entry := range page.Entries {
		if entry.Cursor == "" || entry.Timestamp.IsZero() {
			return fmt.Errorf("%w: 第 %d 条日志缺少 cursor 或 timestamp", ErrAgentUnavailable, i+1)
		}
	}
	if page.NextCursor != nil && *page.NextCursor == "" {
		return fmt.Errorf("%w: 代理返回了空的 next_cursor", ErrAgentUnavailable)
	}
	return nil
}

// paramRejected 是“代理说这次参数不对”的载体：消息原样透传，判决仍然是参数错误。
//
// 为什么不是 `fmt.Errorf("%w: %s", journal.ErrInvalidParams, msg)`：那会把
// “journal: 查询参数不合法: ” 再打一遍，而代理的消息里已经有了同样的说明。
// 读日志的人应该看到一句参数错误，不是看到我们内部有几层。
type paramRejected struct{ msg string }

func (e paramRejected) Error() string { return e.msg }

func (e paramRejected) Is(target error) bool { return target == journal.ErrInvalidParams }

// mapError 把代理错误翻译成 logs 包的错误。
//
// 三类分开处理，因为接口层对它们的状态码不同：
//   - 连不上（拨号失败、超时、响应不合契约）与代理自己 5xx：日志子系统此刻不可用 → 503；
//   - 代理回 400：**参数**问题（典型例子是 to 在未来，只有拿到“现在”的代理判得出）
//     → 接口层必须回 400，否则会让人以为是服务端坏了并反复重试；
//   - 其余 4xx：代理拒绝了这次调用，但不是“不可用”，如实带出原因。
func (f *Fetcher) mapError(err error, what string) error {
	var unavailable *agentclient.UnavailableError
	if errors.As(err, &unavailable) {
		return f.unavailable(what, err)
	}
	var rejected *agentclient.Error
	if errors.As(err, &rejected) {
		switch {
		case rejected.Status == http.StatusBadRequest:
			// 代理已经说明了哪一项参数不对（典型例子是 to 在未来，只有拿到“现在”的
			// 代理判得出）。这里换成 paramRejected 保住 errors.Is 的判决，
			// 但**不再**往消息前面加一层 “journal: 查询参数不合法: ”：那样最终文本
			// 会变成三层前缀套娃（真机载荷 err-future.json 记的就是这个）。
			return paramRejected{msg: rejected.Message}
		case rejected.Status >= http.StatusInternalServerError:
			return f.unavailable(what, err)
		}
	}
	return fmt.Errorf("logs: %s 被代理拒绝: %w", what, err)
}

// unavailable 记录并包装“代理不可用”。
func (f *Fetcher) unavailable(what string, err error) error {
	if f.logger != nil {
		f.logger.Warn(what+"：代理不可用", "error", err)
	}
	return fmt.Errorf("%w: %v", ErrAgentUnavailable, err)
}
