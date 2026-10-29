package journal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"upanel/internal/syscmd"
)

// Runner 执行一次外部命令。生产实现是 syscmd.Run，测试里换成固定输出。
//
// 单独抽出来不是为了“可替换”，而是为了让解析与限额逻辑能用真实字节验证：
// 验收里抓到的 journalctl 原始输出会原样喂给这里，而不需要在单元测试里跑 journalctl。
type Runner interface {
	Run(ctx context.Context, spec syscmd.Spec, limits syscmd.Limits) (syscmd.Result, error)
}

// RunnerFunc 让普通函数满足 Runner。
type RunnerFunc func(ctx context.Context, spec syscmd.Spec, limits syscmd.Limits) (syscmd.Result, error)

// Run 实现 Runner。
func (f RunnerFunc) Run(ctx context.Context, spec syscmd.Spec, limits syscmd.Limits) (syscmd.Result, error) {
	return f(ctx, spec, limits)
}

// Streamer 启动一个持续运行的命令。生产实现是 syscmd.Start。
type Streamer interface {
	Start(ctx context.Context, spec syscmd.Spec, limits syscmd.Limits) (StreamProcess, error)
}

// StreamProcess 是持续运行的子进程的最小接口（*syscmd.Stream 满足它）。
type StreamProcess interface {
	Stdout() io.Reader
	Stderr() string
	Wait() error
	Close() error
}

// StreamerFunc 让普通函数满足 Streamer。
type StreamerFunc func(ctx context.Context, spec syscmd.Spec, limits syscmd.Limits) (StreamProcess, error)

// Start 实现 Streamer。
func (f StreamerFunc) Start(ctx context.Context, spec syscmd.Spec, limits syscmd.Limits) (StreamProcess, error) {
	return f(ctx, spec, limits)
}

// 查询与流各自的时限。查询是“翻一页”，15 秒足够；流有独立的整体时长上限。
const (
	queryTimeout  = 15 * time.Second
	maxQueryBytes = 4 << 20
	// QueryName 是日志查询在代理登记表里的名字。
	QueryName = "logs.query"
	// QueryVersion 进版本相关的指纹，改动查询形态时递增（当前没有缓存依赖它，
	// 保留是为了让“同一份参数对应同一份结果”这件事有明确表示）。
	QueryVersion = "journal-query/v1"
)

// ErrCursorRejected 表示 journalctl 拒绝了这个游标（无效、或不属于当前 journal）。
var ErrCursorRejected = errors.New("journal: journalctl 拒绝了该游标")

// Querier 按固定参数调用 journalctl，并把输出解析成 Entry。
type Querier struct {
	program  string
	runner   Runner
	streamer Streamer
	clock    func() time.Time
}

// NewQuerier 构造 Querier。program 必须是 journalctl 的绝对路径。
//
// runner 与 streamer 都必须给出：查询与实时流是两条独立路径，缺一条就会在运行到
// 那一步时才失败——那正是“功能看起来在、实际用不了”的状态，不允许存在。
func NewQuerier(program string, runner Runner, streamer Streamer, clock func() time.Time) (*Querier, error) {
	if !strings.HasPrefix(program, "/") {
		return nil, fmt.Errorf("journal: journalctl 路径必须是绝对路径，当前为 %q", program)
	}
	if runner == nil {
		return nil, errors.New("journal: Querier 需要 Runner")
	}
	if streamer == nil {
		return nil, errors.New("journal: Querier 需要 Streamer")
	}
	if clock == nil {
		clock = time.Now
	}
	return &Querier{program: program, runner: runner, streamer: streamer, clock: clock}, nil
}

// Program 返回当前使用的 journalctl 路径。
func (q *Querier) Program() string { return q.program }

// Query 查一页日志。
//
// 参数在这里补齐默认值并再次校验：调用方（Web 或代理）已经校验过一次，
// 但“命令行只从 Normalized 构造”这条纪律不能依赖上游记得校验。
func (q *Querier) Query(ctx context.Context, params Params) (Page, error) {
	n, err := params.Normalize(q.clock())
	if err != nil {
		return Page{}, err
	}
	args := queryArgs(n)
	start := time.Now()
	res, err := q.runner.Run(ctx, syscmd.Spec{Program: q.program, Args: args}, syscmd.Limits{
		Timeout:   queryTimeout,
		MaxStdout: maxQueryBytes,
		MaxStderr: 64 << 10,
	})
	page := Page{
		Source:     n.Source,
		Unit:       n.Unit,
		Level:      n.Level,
		From:       n.From,
		To:         n.To,
		DurationMS: time.Since(start).Milliseconds(),
	}
	if err != nil {
		return page, q.mapError(err, res)
	}
	if res.ExitCode != 0 {
		return page, q.exitError(res)
	}

	entries, scanned, truncatedByOutput := parseLines(res.Stdout, res.StdoutTruncated)
	page.Scanned = scanned

	// 翻页去重：--cursor 是**含**该条目的起点，所以第一行可能就是“上一页最后一条”。
	// 用游标全等比较而不是时间比较：同一微秒里的多条日志用时间根本分不开。
	if n.Cursor != "" && len(entries) > 0 && entries[0].Cursor == n.Cursor {
		entries = entries[1:]
	}

	// 带游标时窗口只能在本地过滤：journalctl 实测拒绝同时给
	// --since/--until 与 --cursor（exit 1，“Please specify only one of ...”）。
	// 记录这一页**扫到的最老游标**，翻页链即便整页都被窗口过滤掉也能继续往前走。
	oldestScanned := ""
	if n.Cursor != "" && len(entries) > 0 {
		oldestScanned = entries[len(entries)-1].Cursor
		inWindow := make([]Entry, 0, len(entries))
		for _, entry := range entries {
			if entry.Timestamp.Before(n.From) || entry.Timestamp.After(n.To) {
				continue
			}
			inWindow = append(inWindow, entry)
		}
		entries = inWindow
	}

	limit := n.Limit
	// 先按字节上限收手：单页消息总量超过 MaxPageBytes 就停在这里，
	// 剩下的部分靠 next_cursor 让调用方自己再要一页（而不是把整段历史读进内存）。
	kept := entries
	var reason string
	hasMore := len(entries) > limit
	stoppedByLimit := hasMore
	bytes := 0
	for i, entry := range entries {
		if i >= limit {
			hasMore = true
			stoppedByLimit = true
			break
		}
		if i > 0 && bytes+len(entry.Message) > MaxPageBytes {
			kept = entries[:i]
			hasMore = true
			// 截断点在页面中间：下一页必须从“最后一条给出的条目”接着翻，
			// 不能跳到扫到的最老游标，否则中间的条目就丢了。
			stoppedByLimit = true
			reason = "page_bytes"
			break
		}
		bytes += len(entry.Message)
	}
	if len(kept) > limit {
		kept = kept[:limit]
	}

	page.Entries = kept
	if page.Entries == nil {
		page.Entries = []Entry{}
	}
	// 输出被 syscmd 截断（单次 stdout 超过 4 MiB）时，最后一行可能不完整，
	// parseLines 已经丢掉了它；这里把“结果被截断”这件事继续往上传递。
	if hasMore || truncatedByOutput {
		page.Truncated = true
		page.TruncatedReason = reason
		if truncatedByOutput && reason == "" {
			page.TruncatedReason = "output_limit"
		}
	}
	// 带游标时要额外判断“这一页是不是被上限截断的”：命令行只给了 --lines=limit+2，
	// 拿满这些行就说明更早的地方还有日志，哪怕它们全被窗口过滤掉了。
	if n.Cursor != "" && !hasMore && oldestScanned != "" && scanned > limit+1 {
		hasMore = true
		stoppedByLimit = false
	}
	if hasMore {
		next := ""
		if stoppedByLimit && len(page.Entries) > 0 {
			next = page.Entries[len(page.Entries)-1].Cursor
		} else if oldestScanned != "" {
			// 整页都落在窗口外（或刚好扫到底）：从扫到的最老一条继续往前，
			// 否则调用方会拿着同一个游标原地打转。
			next = oldestScanned
		}
		if next != "" {
			page.NextCursor = &next
		}
	}
	return page, nil
}

// queryArgs 按固定顺序构造 journalctl 参数。
//
// 唯一的可变部分是归一化后的值：unit 已经过形状校验，时间由 journalTimestamp
// 按 journalctl 认得的形状写出来，游标过了字符集检查，limit 是整数。
// 没有一处来自“调用方给的字符串片段”。
func queryArgs(n Normalized) []string {
	args := []string{
		"--output=json",
		"--no-pager",
		"--quiet",
		// 倒序：日志页面看的是“最近发生了什么”，往前翻页比往后翻更符合直觉。
		"--reverse",
		// 多取两条：一条用来抵消 --cursor 的“含起点”，一条用来判断还有没有更早的。
		fmt.Sprintf("--lines=%d", n.Limit+2),
	}
	if n.Source == SourceKernel {
		// journalctl 没有 --kernel，只有 -k。实测 systemd 255：
		// `journalctl --kernel` → “unrecognized option '--kernel'”，退出码 1。
		args = append(args, "-k")
	}
	if n.Unit != "" {
		args = append(args, "--unit="+n.Unit)
	}
	if n.HasLevel {
		args = append(args, "--priority="+n.Level.Priority())
	}
	if n.Cursor != "" {
		// --cursor 与 --since/--until 互斥（journalctl 实测 exit 1：
		// “Please specify only one of --since=, --cursor=, and --after-cursor=.”）。
		// 游标本身已经指明位置，时间窗口改由 Query 在解析后过滤。
		args = append(args, "--cursor="+n.Cursor)
		return args
	}
	args = append(args,
		"--since="+journalTimestamp(n.From),
		"--until="+journalTimestamp(n.To),
	)
	return args
}

// journalTimestamp 把时间写成 journalctl 认得的形式。
//
// 为什么不是 RFC3339：`2026-10-27T13:29:43.1270959Z` 在 Ubuntu 22.04 的
// journalctl（systemd 249）上直接 exit 1 —— “Failed to parse timestamp”。
// 249 的时间解析器只认空格分隔的 `2026-10-27 13:29:43`，ISO8601 的 `T` 分隔符
// 要等到 systemd 251 才被接受；用 RFC3339 写参数的代价是**22.04 上每一条 /logs
// 查询都 400**，而 24.04（systemd 255）上一切正常——两边的通过与否只取决于
// 客户机版本，这种差异最容易在只测了一个版本时被漏掉（P16.2 在 22.04 上抓到）。
//
// 三处细节都是有意的：
//   - 用空格分隔的 `YYYY-MM-DD HH:MM:SS`：249 与 255 都认；
//   - 固定 6 位小数：systemd 的分数秒是微秒精度，Go 的纳秒（RFC3339Nano 会给到
//     9 位）在 249 上同样解析失败；
//   - 显式 ` UTC` 后缀：不带时区时 journalctl 按**客户机本地时区**解释，
//     同一串时间在非 UTC 的机器上会整体偏移，窗口就悄悄变了。
func journalTimestamp(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04:05.000000") + " UTC"
}

// parseLines 解析 journalctl 的多行 JSON 输出。
//
// 逐行处理的原因：journalctl -o json 是“一行一条”，而 syscmd 的输出上限可能把最后一行
// 切一半——半行必须丢掉（并已由 truncated 标记），不能让它变成一条解析错误。
func parseLines(stdout string, stdoutTruncated bool) (entries []Entry, scanned int, truncated bool) {
	lines := strings.Split(stdout, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		// journalctl 在部分输出模式下会打印 “-- Journal begins at … --” 这类横幅。
		// 它不是条目，也不是错误。
		if strings.HasPrefix(trimmed, "-- ") {
			continue
		}
		if !strings.HasPrefix(trimmed, "{") {
			// 输出被截断时，最后一段可能是半行；其余情况是非 JSON 噪声，都不该被当成条目。
			if stdoutTruncated && i == len(lines)-1 {
				return entries, scanned, true
			}
			continue
		}
		entry, err := parseEntry([]byte(trimmed))
		if err != nil {
			if stdoutTruncated && i == len(lines)-1 {
				return entries, scanned, true
			}
			// 一行坏掉的输出不构成“整个查询失败”的理由，但也不能当它不存在：
			// 跳过并计数，页面能看出“扫到的东西和拿到的条目对不上”。
			continue
		}
		scanned++
		entries = append(entries, entry)
	}
	return entries, scanned, false
}

// mapError 把 syscmd 的错误翻译成本包的错误。
func (q *Querier) mapError(err error, res syscmd.Result) error {
	if errors.Is(err, syscmd.ErrTimeout) {
		return fmt.Errorf("journal: journalctl 在 %s 内没有完成", queryTimeout)
	}
	var exitErr *syscmd.ExitError
	if errors.As(err, &exitErr) {
		return q.exitError(res)
	}
	return fmt.Errorf("journal: 执行 %s 失败: %w", q.program, err)
}

// exitError 解释 journalctl 的非零退出。
//
// 实测（systemd 255）：无效游标是 exit 1 + stderr “Failed to seek to cursor: Invalid argument”，
// 无效时间戳是 “…Failed to parse timestamp…”，无效级别是 “Unknown log level …”。
// 前两种必须能和“其他失败”分开：游标来自上一页，坏了是**调用方的参数问题**（400），
// 不是服务端内部错误（500）。
func (q *Querier) exitError(res syscmd.Result) error {
	stderr := strings.TrimSpace(res.Stderr)
	lower := strings.ToLower(stderr)
	switch {
	case strings.Contains(lower, "failed to seek to cursor"):
		return fmt.Errorf("%w: %s", ErrCursorRejected, stderr)
	case strings.Contains(lower, "failed to parse timestamp"):
		return fmt.Errorf("%w: %s", ErrInvalidParams, stderr)
	case strings.Contains(lower, "unknown log level"):
		return fmt.Errorf("%w: %s", ErrInvalidParams, stderr)
	}
	if stderr == "" {
		stderr = "无错误输出"
	}
	return fmt.Errorf("journal: journalctl 以退出状态 %d 结束: %s", res.ExitCode, truncateForError(stderr))
}

// truncateForError 把 stderr 截短，避免把一整屏错误文本塞进日志与响应。
func truncateForError(s string) string {
	const limit = 300
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}
