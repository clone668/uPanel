package journal

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"upanel/internal/syscmd"
)

// 流的默认限额。PLAN.md 第 14 节要求“流断开释放子进程和订阅资源”，
// STAGES.md 第 7 节的验收条目是“结束日志订阅不会残留无限 journalctl 子进程”。
//
// 这里的每个上限都对应一种“没人看着它了”的情形：客户端断开、客户端还连着但
// 一直不读、看太久忘了关、输出太猛。任何一种都必须让子进程结束。
const (
	// DefaultStreamDuration 是一条流最长活多久。10 分钟够看完一次事故的即时输出，
	// 之后 SSE 客户端会重新连一条（续传靠游标，不丢条目）。
	DefaultStreamDuration = 10 * time.Minute
	// DefaultStreamIdle 是“多久没有任何新日志就收掉”。安静系统上 journalctl -f
	// 可以几小时不吐一行，留着它等于留一个永远不动的子进程。
	DefaultStreamIdle = 60 * time.Second
	// DefaultStreamEntries 是单条流最多推送的条数。
	DefaultStreamEntries = 5000
	// DefaultStreamBytes 是单条流最多推送的消息字节数。
	DefaultStreamBytes = 8 << 20
	// DefaultStreamReplay 是续传时先补发的历史条数上限。
	DefaultStreamReplay = 100
	// streamStderrBytes 是流式 journalctl 的 stderr 上限。
	streamStderrBytes = 64 << 10
)

// StreamEnd 是一条流结束的原因。闭集：调用方据此决定“要不要重连”。
type StreamEnd string

// 结束原因的闭集。
const (
	// StreamEndClientClosed 表示调用方不再接收（SSE 断开、上层取消）。
	StreamEndClientClosed StreamEnd = "client_closed"
	// StreamEndDuration 表示到达单条流的最长时长。
	StreamEndDuration StreamEnd = "duration_limit"
	// StreamEndIdle 表示长时间没有新日志。
	StreamEndIdle StreamEnd = "idle_timeout"
	// StreamEndEntries 表示达到条数上限。
	StreamEndEntries StreamEnd = "entry_limit"
	// StreamEndBytes 表示达到字节上限。
	StreamEndBytes StreamEnd = "byte_limit"
	// StreamEndJournalExit 表示 journalctl 自己退出了（不是我们收掉的）。
	StreamEndJournalExit StreamEnd = "journal_exit"
)

// EmitFunc 接收一条日志。返回错误表示调用方已无法继续接收，流必须结束。
type EmitFunc func(Entry) error

// StreamOptions 是一条实时流的限额；零值字段取默认值。
type StreamOptions struct {
	MaxDuration time.Duration
	IdleTimeout time.Duration
	MaxEntries  int
	MaxBytes    int64
	// Replay 是带游标续传时先补发的历史条数。
	Replay int
}

func (o StreamOptions) withDefaults() StreamOptions {
	if o.MaxDuration <= 0 {
		o.MaxDuration = DefaultStreamDuration
	}
	if o.IdleTimeout <= 0 {
		o.IdleTimeout = DefaultStreamIdle
	}
	if o.MaxEntries <= 0 {
		o.MaxEntries = DefaultStreamEntries
	}
	if o.MaxBytes <= 0 {
		o.MaxBytes = DefaultStreamBytes
	}
	if o.Replay <= 0 {
		o.Replay = DefaultStreamReplay
	}
	return o
}

// Stream 实时跟随 journal，把每条新日志交给 emit。
//
// 返回的 StreamEnd 说明这条流是**怎么结束的**：调用方据此决定重连还是停止。
// 无论哪种结束方式，返回之前都已经杀掉子进程组并收尸——不存在“函数回来了，
// journalctl 还在跑”的中间状态。
func (q *Querier) Stream(ctx context.Context, params Params, opts StreamOptions, emit EmitFunc) (StreamEnd, error) {
	n, err := params.Normalize(q.clock())
	if err != nil {
		return "", err
	}
	opts = opts.withDefaults()

	runCtx, cancel := context.WithTimeout(ctx, opts.MaxDuration)
	defer cancel()

	proc, err := q.start(runCtx, streamArgs(n, opts.Replay))
	if err != nil {
		return "", err
	}
	// 唯一的收尾路径：先杀进程组再收尸。放在 defer 里是为了让上面每一个 return
	// 都走同一条路——包括“emit 失败”和“panic 展开”这两种最容易被漏掉的情形。
	defer func() { _ = proc.Close() }()

	lines := make(chan string, 16)
	readErr := make(chan error, 1)
	go pumpLines(runCtx, proc.Stdout(), lines, readErr)

	idle := time.NewTimer(opts.IdleTimeout)
	defer idle.Stop()

	var (
		count int
		bytes int64
	)
	resetIdle := func() {
		if !idle.Stop() {
			select {
			case <-idle.C:
			default:
			}
		}
		idle.Reset(opts.IdleTimeout)
	}

	for {
		select {
		case <-ctx.Done():
			return StreamEndClientClosed, nil
		case <-runCtx.Done():
			// 父 ctx 还活着而 runCtx 结束 → 是我们自己的时长上限。
			if ctx.Err() != nil {
				return StreamEndClientClosed, nil
			}
			return StreamEndDuration, nil
		case <-idle.C:
			return StreamEndIdle, nil
		case err := <-readErr:
			if errors.Is(err, io.EOF) {
				return StreamEndJournalExit, nil
			}
			// 被我们自己取消时读到的是管道关闭错误，不是故障。
			if ctx.Err() != nil || runCtx.Err() != nil {
				return StreamEndClientClosed, nil
			}
			return StreamEndJournalExit, fmt.Errorf("journal: 读取 journalctl 输出失败: %w", err)
		case line := <-lines:
			entry, ok := parseStreamLine(line)
			if !ok {
				continue
			}
			count++
			bytes += int64(len(entry.Message))
			if err := emit(entry); err != nil {
				return StreamEndClientClosed, nil
			}
			if count >= opts.MaxEntries {
				return StreamEndEntries, nil
			}
			if bytes >= opts.MaxBytes {
				return StreamEndBytes, nil
			}
			resetIdle()
		}
	}
}

// start 启动 journalctl 跟随进程。
func (q *Querier) start(ctx context.Context, args []string) (StreamProcess, error) {
	if q.streamer == nil {
		return nil, errors.New("journal: 未配置流式执行器")
	}
	proc, err := q.streamer.Start(ctx, syscmd.Spec{Program: q.program, Args: args}, syscmd.Limits{
		MaxStderr: streamStderrBytes,
	})
	if err != nil {
		return nil, fmt.Errorf("journal: 启动 journalctl 跟随失败: %w", err)
	}
	return proc, nil
}

// streamArgs 构造跟随模式的参数。
//
// 与查询不同，跟随模式**不使用时间窗口**：--until 会让 journalctl 到点就退出，
// --since 只是把起点往后挪；实时流看的是“现在之后发生了什么”，起点由游标决定。
func streamArgs(n Normalized, replay int) []string {
	args := []string{
		"--output=json",
		"--no-pager",
		"--quiet",
		"--follow",
	}
	if n.Cursor == "" {
		// 没有游标就是“从现在开始”，不要先补发 10 条历史（那会让页面刚打开就跳）。
		args = append(args, "--lines=0")
	} else {
		args = append(args, fmt.Sprintf("--after-cursor=%s", n.Cursor))
		if replay > 0 {
			// --follow 下 --lines 只限制**首批**补发，不影响后续跟随。
			args = append(args, fmt.Sprintf("--lines=%d", replay))
		}
	}
	if n.Source == SourceKernel {
		// 同 Query：journalctl 只认 -k，没有 --kernel。
		args = append(args, "-k")
	}
	if n.Unit != "" {
		args = append(args, "--unit="+n.Unit)
	}
	if n.HasLevel {
		args = append(args, "--priority="+n.Level.Priority())
	}
	return args
}

// pumpLines 把子进程输出按行推给主循环。ctx 结束时立刻退出，不再阻塞在 send 上。
func pumpLines(ctx context.Context, r io.Reader, lines chan<- string, readErr chan<- error) {
	reader := bufio.NewReaderSize(r, 64<<10)
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			select {
			case lines <- strings.TrimRight(line, "\r\n"):
			case <-ctx.Done():
				readErr <- ctx.Err()
				return
			}
		}
		if err != nil {
			readErr <- err
			return
		}
	}
}

// parseStreamLine 解析跟随模式的一行输出；非条目的行返回 ok=false。
func parseStreamLine(line string) (Entry, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "-- ") || !strings.HasPrefix(trimmed, "{") {
		return Entry{}, false
	}
	entry, err := parseEntry([]byte(trimmed))
	if err != nil {
		return Entry{}, false
	}
	return entry, true
}
