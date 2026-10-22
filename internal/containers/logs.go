package containers

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"upanel/internal/logging"
	"upanel/internal/syscmd"
)

// 实时流的限额。与日志模块（journal）同口径：时长 10 分钟、空闲 60 秒、
// 5000 条、8 MiB。两处数字不一样会让人以为“容器日志更宽松”，而实际只是没对齐。
const (
	// DefaultStreamIdleTimeout 是多久没有新日志就收流。
	DefaultStreamIdleTimeout = 60 * time.Second
	// DefaultStreamMaxDuration 是一条流的时长上限。
	DefaultStreamMaxDuration = 10 * time.Minute
	// DefaultStreamMaxEntries 是一条流的条数上限。
	DefaultStreamMaxEntries = 5000
	// DefaultStreamMaxBytes 是一条流的字节上限。
	DefaultStreamMaxBytes = 8 << 20
)

// 往回翻页的窗口参数。
//
// 为什么不是 `--tail N --until 游标`：docker 先取“整个流的最后 N 行”，再按 `--until`
// 过滤，于是比游标早的行在过滤前就被 tail 丢光了——真机上第二页永远只有边界那一条
// （再被去重掉，变成空页）。往回翻只能给一个时间窗 `--since … --until 游标`，
// 窗口太小会漏（安静的容器），太大又会撞上单次输出上限（话多的容器）：
// 所以窗口既能加宽也能收窄，收窄时保留的是窗口靠后（更接近游标）的那段。
const (
	// logsBackPageInitialWindow 是往回翻页的初始时间窗。
	logsBackPageInitialWindow = 15 * time.Minute
	// logsBackPageWidenFactor 是窗口里行数不够时的放大倍数。
	logsBackPageWidenFactor = 8
	// logsBackPageShrinkFactor 是输出被上限截断时的缩小倍数。
	logsBackPageShrinkFactor = 4
	// logsBackPageAttempts 是单次翻页最多问 docker 几次。
	logsBackPageAttempts = 8
	// logsBackPageMaxWindow 是窗口上限（再往上没有意义：容器生命周期远短于此）。
	logsBackPageMaxWindow = 30 * 24 * time.Hour
)

// Logs 读取一页容器日志（从新到旧）。
//
// 分页是“时间键集”而不是游标：docker logs 没有 journal 那样的游标，只有
// `--since/--until` 与 `--tail`。因此 next_cursor 就是本页最老一条的时间戳，
// 带回来即“看比这个时间戳更早的”。代价是边界条目会重复一次，由读取端按
// “时间戳不早于游标”去重——按行号去重在这里不成立（同一个纳秒可能有多行）。
func (c *Client) Logs(ctx context.Context, params LogParams) (LogPage, error) {
	start := time.Now()
	page := LogPage{
		ContainerID:   params.ContainerID,
		Entries:       []LogEntry{},
		StreamsMerged: true,
	}
	if available, notice := c.Availability(); !available {
		return page, &UnavailableError{Notice: notice}
	}

	var (
		entries   []LogEntry
		lines     int
		truncated bool
		reason    string
		err       error
	)
	if params.Cursor == "" {
		entries, lines, truncated, reason, err = c.logsNewest(ctx, params)
	} else {
		entries, lines, truncated, reason, err = c.logsOlder(ctx, params)
	}
	if err != nil {
		return page, err
	}

	page.Entries = entries
	page.Lines = lines
	page.Truncated = truncated
	page.TruncatedReason = reason
	if len(entries) > 0 {
		oldest := entries[len(entries)-1].Timestamp
		cursor := oldest.UTC().Format(time.RFC3339Nano)
		page.NextCursor = &cursor
		// 本页最老一条与最新一条就是“实际看的是哪一段”。
		page.From = entries[len(entries)-1].Timestamp.UTC().Format(time.RFC3339Nano)
		page.To = entries[0].Timestamp.UTC().Format(time.RFC3339Nano)
	}
	page.DurationMS = time.Since(start).Milliseconds()
	return page, nil
}

// logsNewest 取最新的一页：这里 `--tail` 的语义正是“整个流的最后 N 行”，合适。
func (c *Client) logsNewest(ctx context.Context, params LogParams) ([]LogEntry, int, bool, string, error) {
	args := []string{"logs", "--timestamps", "--tail", fmt.Sprintf("%d", params.Tail)}
	if params.Since != "" {
		args = append(args, "--since", params.Since)
	}
	if params.Until != "" {
		args = append(args, "--until", params.Until)
	}
	args = append(args, params.ContainerID)

	res, err := c.dockerRun(ctx, 25*time.Second, args...)
	if err != nil {
		return nil, 0, false, "", err
	}
	if res.ExitCode != 0 {
		return nil, 0, false, "", classifyLogsFailure(params.ContainerID, res)
	}
	entries, lines, truncatedBytes := parseLogOutput(res.Stdout, params)
	truncated := truncatedBytes
	reason := ""
	if truncatedBytes {
		reason = "page_bytes"
	} else if len(entries) >= params.Tail {
		// --tail 给了这么多行，说明更早的还有；next_cursor 带回来就能继续翻。
		truncated = true
	}
	return entries, lines, truncated, reason, nil
}

// logsOlder 取“比游标更早”的一页：用 `--since … --until 游标` 的时间窗，
// 窗口按结果加宽/收窄，直到凑满一页或确定容器里确实没有更早的日志。
//
// 返回的 truncated 只在“满页”或“单页字节上限到了”时为 true；若窗口已经覆盖到
// 容器创建时间，更早的确实不存在，那就是 false（而不是含糊地说“可能还有”）。
func (c *Client) logsOlder(ctx context.Context, params LogParams) ([]LogEntry, int, bool, string, error) {
	until, err := time.Parse(time.RFC3339Nano, params.Cursor)
	if err != nil {
		return nil, 0, false, "", fmt.Errorf("containers: 游标不是时间戳：%v", err)
	}
	// 下限用容器创建时间：它是“更早的确实没有了”的可靠起点，不需要猜。
	floor, err := c.containerCreated(ctx, params.ContainerID)
	if err != nil {
		return nil, 0, false, "", err
	}
	if params.Since != "" {
		if since, err := time.Parse(time.RFC3339Nano, params.Since); err == nil && since.After(floor) {
			floor = since
		}
	}
	if !floor.Before(until) {
		return []LogEntry{}, 0, false, "", nil
	}

	window := logsBackPageInitialWindow
	for attempt := 0; attempt < logsBackPageAttempts; attempt++ {
		since := until.Add(-window)
		covers := false
		if !since.After(floor) {
			since = floor
			covers = true
		}
		args := []string{
			"logs", "--timestamps",
			"--since", since.UTC().Format(time.RFC3339Nano),
			"--until", until.UTC().Format(time.RFC3339Nano),
			params.ContainerID,
		}
		res, err := c.dockerRun(ctx, 25*time.Second, args...)
		if err != nil {
			return nil, 0, false, "", err
		}
		if res.ExitCode != 0 {
			return nil, 0, false, "", classifyLogsFailure(params.ContainerID, res)
		}
		if res.StdoutTruncated {
			// 输出撞上单次读取上限：我们丢掉的正是窗口末尾（最接近游标）的行，
			// 所以下一步是缩小窗口，而不是接受一份缺了要紧部分的页。
			if window <= time.Second && attempt > 0 {
				return nil, 0, false, "", fmt.Errorf("containers: 容器日志窗口无法收敛到可读大小")
			}
			window /= logsBackPageShrinkFactor
			if window < time.Millisecond {
				window = time.Millisecond
			}
			continue
		}
		entries, lines, truncatedBytes := parseLogOutput(res.Stdout, params)
		if len(entries) >= params.Tail {
			return entries, lines, true, "", nil
		}
		if truncatedBytes {
			return entries, lines, true, "page_bytes", nil
		}
		if covers {
			// 窗口已经铺到容器创建时间，更早的确实没有。
			return entries, lines, false, "", nil
		}
		window *= logsBackPageWidenFactor
		if window > logsBackPageMaxWindow {
			window = logsBackPageMaxWindow
		}
	}
	return nil, 0, false, "", fmt.Errorf("containers: 往回翻页在 %d 次尝试内没有收敛", logsBackPageAttempts)
}

// containerCreated 读出容器的创建时间，作为往回翻页的下限。
func (c *Client) containerCreated(ctx context.Context, id string) (time.Time, error) {
	res, err := c.dockerRun(ctx, 10*time.Second,
		"inspect", "--type", "container", "--format", "{{.Created}}", id)
	if err != nil {
		return time.Time{}, err
	}
	if res.ExitCode != 0 {
		return time.Time{}, classifyLogsFailure(id, res)
	}
	ts, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(res.Stdout))
	if err != nil {
		return time.Time{}, fmt.Errorf("containers: 容器创建时间读不出来：%v", err)
	}
	return ts, nil
}

// classifyLogsFailure 把 docker logs 的失败分成“容器不在”“日志驱动不支持读取”与真失败。
func classifyLogsFailure(id string, res syscmd.Result) error {
	stderr := strings.ToLower(res.Stderr)
	switch {
	case strings.Contains(stderr, "no such container"):
		return &NotFoundError{ContainerID: id}
	case strings.Contains(stderr, "does not support reading"),
		strings.Contains(stderr, "configured logging driver does not support"):
		return &LogsUnsupported{ContainerID: id}
	}
	return dockerFailure("docker logs", res)
}

// parseLogOutput 解析 `docker logs --timestamps` 的输出。
//
// 返回的条目按时间从新到旧。多行消息在 --timestamps 下每行都有时间戳，
// 因此没有时间戳的行只可能是被截断的残行：把它接在上一条后面并标记截断，
// 而不是丢掉——丢掉会让人以为那句日志没写过。
func parseLogOutput(out string, params LogParams) ([]LogEntry, int, bool) {
	type entry struct {
		ts   time.Time
		text string
		tr   bool
		red  bool
	}
	collected := make([]entry, 0, params.Tail)
	total := 0
	for _, raw := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		raw = strings.TrimRight(raw, "\r")
		if raw == "" {
			continue
		}
		total++
		ts, msg, ok := splitTimestamp(raw)
		if !ok {
			if len(collected) > 0 {
				last := &collected[len(collected)-1]
				last.text += "\n" + raw
				last.tr = true
			}
			continue
		}
		// 同一时间戳（游标边界）会重复出现，按“不早于游标”去重。
		if params.Cursor != "" {
			if cursor, err := time.Parse(time.RFC3339Nano, params.Cursor); err == nil && !ts.Before(cursor) {
				continue
			}
		}
		if params.Since != "" {
			if since, err := time.Parse(time.RFC3339Nano, params.Since); err == nil && ts.Before(since) {
				continue
			}
		}
		text, redacted := logging.Redact(msg)
		truncated := false
		if len(text) > MaxLogLineBytes {
			text = text[:MaxLogLineBytes] + "…（已截断）"
			truncated = true
		}
		collected = append(collected, entry{ts: ts, text: text, tr: truncated, red: redacted})
	}

	// 从新到旧，并执行单页条数与字节上限。
	entries := make([]LogEntry, 0, len(collected))
	var bytes int
	for i := len(collected) - 1; i >= 0; i-- {
		e := collected[i]
		if len(entries) >= params.Tail {
			return entries, total, true
		}
		bytes += len(e.text)
		if bytes > MaxLogPageBytes && len(entries) > 0 {
			return entries, total, true
		}
		entries = append(entries, LogEntry{Timestamp: e.ts, Message: e.text, Truncated: e.tr, Redacted: e.red})
	}
	return entries, total, false
}

// splitTimestamp 把 "2026-10-22T10:00:00.123456789Z 正文" 切成时间与正文。
func splitTimestamp(line string) (time.Time, string, bool) {
	space := strings.IndexByte(line, ' ')
	if space <= 0 {
		return time.Time{}, "", false
	}
	ts, err := time.Parse(time.RFC3339Nano, line[:space])
	if err != nil {
		return time.Time{}, "", false
	}
	return ts, line[space+1:], true
}

// StreamOptions 是一条实时流的限额。
type StreamOptions struct {
	IdleTimeout time.Duration
	MaxDuration time.Duration
	MaxEntries  int
	MaxBytes    int64
}

func (o StreamOptions) withDefaults() StreamOptions {
	if o.IdleTimeout <= 0 {
		o.IdleTimeout = DefaultStreamIdleTimeout
	}
	if o.MaxDuration <= 0 {
		o.MaxDuration = DefaultStreamMaxDuration
	}
	if o.MaxEntries <= 0 {
		o.MaxEntries = DefaultStreamMaxEntries
	}
	if o.MaxBytes <= 0 {
		o.MaxBytes = DefaultStreamMaxBytes
	}
	return o
}

// Stream 跟随容器日志，逐条交给 emit。
//
// 返回的字符串是流的结束原因（闭集）。emit 返回错误表示调用方写不动了：
// 此时立即收流，进程组由 syscmd 的 ctx 取消一起带走。
func (c *Client) Stream(ctx context.Context, params LogParams, opts StreamOptions, emit func(LogEntry) error) (string, error) {
	opts = opts.withDefaults()
	if available, notice := c.Availability(); !available {
		return "", &UnavailableError{Notice: notice}
	}
	program := c.program()
	since := params.Since
	if since == "" {
		// 默认从“现在”开始：跟随流不该先把历史重放一遍。
		since = c.now().Format(time.RFC3339Nano)
	}
	args := []string{"logs", "--follow", "--tail", "0", "--timestamps", "--since", since, params.ContainerID}

	streamCtx, cancel := context.WithTimeout(ctx, opts.MaxDuration)
	defer cancel()

	stream, err := syscmd.Start(streamCtx, syscmd.Spec{Program: program, Args: args}, syscmd.Limits{MaxStderr: 64 << 10})
	if err != nil {
		return "", fmt.Errorf("containers: 启动 docker logs 失败: %w", err)
	}
	defer func() { _ = stream.Close() }()

	lines := make(chan string, 64)
	readErr := make(chan error, 1)
	go func() {
		reader := bufio.NewReaderSize(stream.Stdout(), 64<<10)
		for {
			line, err := reader.ReadString('\n')
			if len(line) > 0 {
				select {
				case lines <- strings.TrimRight(line, "\r\n"):
				case <-streamCtx.Done():
					return
				}
			}
			if err != nil {
				readErr <- err
				return
			}
		}
	}()

	idle := time.NewTimer(opts.IdleTimeout)
	defer idle.Stop()

	var (
		emitted int
		bytes   int64
	)
	for {
		select {
		case <-ctx.Done():
			return StreamEndClientClosed, nil
		case <-streamCtx.Done():
			// 时长上限到了；与客户端断开区分开。
			if ctx.Err() != nil {
				return StreamEndClientClosed, nil
			}
			return StreamEndDurationLimit, nil
		case <-idle.C:
			return StreamEndIdleTimeout, nil
		case err := <-readErr:
			// docker logs 退出（容器停了，或 --follow 被中止）。
			stderr := strings.TrimSpace(stream.Stderr())
			if stderr != "" && strings.Contains(strings.ToLower(stderr), "does not support reading") {
				return "", &LogsUnsupported{ContainerID: params.ContainerID}
			}
			if err != nil && !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "file already closed") {
				if stderr != "" {
					return StreamEndContainerExit, nil
				}
			}
			return StreamEndContainerExit, nil
		case line := <-lines:
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(opts.IdleTimeout)
			ts, msg, ok := splitTimestamp(line)
			if !ok {
				continue
			}
			text, redacted := logging.Redact(msg)
			truncated := false
			if len(text) > MaxLogLineBytes {
				text = text[:MaxLogLineBytes] + "…（已截断）"
				truncated = true
			}
			entry := LogEntry{Timestamp: ts, Message: text, Truncated: truncated, Redacted: redacted}
			if err := emit(entry); err != nil {
				return StreamEndClientClosed, nil
			}
			emitted++
			bytes += int64(len(text))
			if emitted >= opts.MaxEntries {
				return StreamEndEntryLimit, nil
			}
			if bytes >= opts.MaxBytes {
				return StreamEndByteLimit, nil
			}
		}
	}
}
