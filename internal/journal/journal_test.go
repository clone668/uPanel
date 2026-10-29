package journal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"upanel/internal/syscmd"
	"upanel/internal/testsupport"
)

// 固定“现在”，让默认时间窗口可断言。
var testNow = time.Date(2026, 10, 21, 3, 5, 0, 0, time.UTC)

func fixedClock() time.Time { return testNow }

// fakeRunner 记录收到的 argv，并返回预置输出。
type fakeRunner struct {
	stdout string
	stderr string
	exit   int
	err    error
	spec   syscmd.Spec
	limits syscmd.Limits
	calls  int
}

func (f *fakeRunner) Run(_ context.Context, spec syscmd.Spec, limits syscmd.Limits) (syscmd.Result, error) {
	f.calls++
	f.spec = spec
	f.limits = limits
	res := syscmd.Result{Program: spec.Program, ExitCode: f.exit, Stdout: f.stdout, Stderr: f.stderr}
	if f.err != nil {
		return res, f.err
	}
	if f.exit != 0 {
		return res, &syscmd.ExitError{Program: spec.Program, ExitCode: f.exit}
	}
	return res, nil
}

func newTestQuerier(t *testing.T, runner Runner) *Querier {
	t.Helper()
	q, err := NewQuerier("/usr/bin/journalctl", runner, &fakeStreamer{}, fixedClock)
	if err != nil {
		t.Fatalf("NewQuerier: %v", err)
	}
	return q
}

// fakeStream 是可控的流式进程：把预置的行写进管道，然后阻塞直到被关闭。
type fakeStream struct {
	r        *io.PipeReader
	w        *io.PipeWriter
	closed   chan struct{}
	closeOne sync.Once
	exited   bool
}

func (f *fakeStream) Stdout() io.Reader { return f.r }
func (f *fakeStream) Stderr() string    { return "" }
func (f *fakeStream) Wait() error       { return nil }
func (f *fakeStream) Close() error {
	f.closeOne.Do(func() {
		f.exited = true
		_ = f.w.Close()
		_ = f.r.Close()
		close(f.closed)
	})
	return nil
}

type fakeStreamer struct {
	mu     sync.Mutex
	lines  []string
	spec   syscmd.Spec
	stream *fakeStream
}

func (f *fakeStreamer) Start(_ context.Context, spec syscmd.Spec, _ syscmd.Limits) (StreamProcess, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, w := io.Pipe()
	s := &fakeStream{r: r, w: w, closed: make(chan struct{})}
	f.spec = spec
	f.stream = s
	lines := append([]string(nil), f.lines...)
	go func() {
		for _, line := range lines {
			if _, err := w.Write([]byte(line + "\n")); err != nil {
				return
			}
		}
		// 之后保持沉默：真实 journalctl 安静时就是这样。
		<-s.closed
	}()
	return s, nil
}

// started 报告 Start 有没有被调用过。
//
// 测试里有一段"等子进程真的起来再取消"的轮询，跑在测试自己的协程里，而 Start 在
// Query.Stream 的协程里写 stream——直接读字段就是数据竞争（2026-10-28 的 P16 全量 -race
// 就是这么报的，TestStreamClientClosedStops）。轮询必须走这把锁。
func (f *fakeStreamer) started() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stream != nil
}

// readFixture 读 tests/fixtures/journal 下的真实采集样本。
//
// 用 testsupport 而不是 "testdata/xxx"：验收 VM 上测试二进制在 /tmp 下执行，
// 相对路径的 testdata/ 在那里不存在，三条“解析真实 journalctl 字节”的用例会以
// “文件不存在”失败——看起来像夹具丢了，其实是查找方式只适合在包目录里跑。
func readFixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := testsupport.Fixture("journal/" + name)
	if err != nil {
		t.Fatalf("读取夹具 %s 失败: %v", name, err)
	}
	return string(raw)
}

func TestParamsNormalizeDefaults(t *testing.T) {
	n, err := Params{}.Normalize(testNow)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if n.Source != SourceJournal {
		t.Fatalf("默认数据源 = %q，期望 %q", n.Source, SourceJournal)
	}
	if n.Limit != DefaultLimit {
		t.Fatalf("默认 limit = %d，期望 %d", n.Limit, DefaultLimit)
	}
	if !n.To.Equal(testNow) {
		t.Fatalf("默认 to = %s，期望 %s", n.To, testNow)
	}
	if !n.From.Equal(testNow.Add(-DefaultWindow)) {
		t.Fatalf("默认 from = %s，期望 %s", n.From, testNow.Add(-DefaultWindow))
	}
	if n.HasLevel {
		t.Fatal("没给 level 时不应标记为按级别过滤")
	}
}

func TestParamsRejects(t *testing.T) {
	cases := []struct {
		name   string
		params Params
		want   string
	}{
		{"数据源不在白名单", Params{Source: "dmesg"}, "source"},
		{"内核日志不接受 unit", Params{Source: SourceKernel, Unit: "ssh.service"}, "unit"},
		{"unit 不接受通配符", Params{Source: SourceJournal, Unit: "ssh*"}, "unit"},
		{"unit 不接受路径", Params{Source: SourceJournal, Unit: "../../etc/passwd"}, "unit"},
		{"unit 缺少后缀", Params{Source: SourceJournal, Unit: "sshd"}, "unit"},
		{"级别不在闭集", Params{Level: "verbose"}, "level"},
		{"级别不接受数字", Params{Level: "3"}, "level"},
		{"游标含空格", Params{Cursor: "s=abc;i=1 ;rm -rf /"}, "cursor"},
		{"游标含换行", Params{Cursor: "s=abc\nMESSAGE=x"}, "cursor"},
		{"limit 过大", Params{Limit: MaxLimit + 1}, "limit"},
		{"limit 为负", Params{Limit: -1}, "limit"},
		{"from 格式错误", Params{From: "2026-10-21 03:00:00"}, "RFC3339"},
		{"to 格式错误", Params{To: "tomorrow"}, "RFC3339"},
		{"from 不早于 to", Params{From: "2026-10-21T03:00:00Z", To: "2026-10-21T03:00:00Z"}, "早于"},
		{"跨度超过 24 小时", Params{
			From: "2026-10-19T03:00:00Z", To: "2026-10-21T03:04:00Z"}, "超过上限"},
		{"to 在未来", Params{To: "2026-10-22T03:00:00Z"}, "超过当前时间"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.params.Normalize(testNow)
			if err == nil {
				t.Fatalf("应当拒绝，实际通过了")
			}
			if !errors.Is(err, ErrInvalidParams) {
				t.Fatalf("应返回 ErrInvalidParams，实际 %v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误信息 %q 里应包含 %q", err.Error(), tc.want)
			}
		})
	}
}

// TestValidateIsClockFree 锁定 Validate 的边界：凡是与“现在”无关的错误都必须在
// Validate 就被拒绝（代理的解码阶段只调用它，见 internal/agent 的 logs.query），
// 而与 now 相关的那一半必须**留给 Normalize**——Validate 里没有 now，不能替它猜。
func TestValidateIsClockFree(t *testing.T) {
	mustReject := []Params{
		{Source: "dmesg"},
		{Source: SourceKernel, Unit: "ssh.service"},
		{Unit: "ssh*"},
		{Level: "verbose"},
		{Cursor: "s=abc;i=1 ;rm -rf /"},
		{Limit: MaxLimit + 1},
		// 时间格式：手写日期必须在进入读取端之前就死掉，
		// 否则它会被当成“没给时间”而安静地换成默认窗口。
		{From: "2026-10-21"},
		{To: "tomorrow"},
		// 两端都给时，先后与跨度不依赖 now。
		{From: "2026-10-21T03:00:00Z", To: "2026-10-21T03:00:00Z"},
		{From: "2026-10-19T03:00:00Z", To: "2026-10-21T03:04:00Z"},
	}
	for _, p := range mustReject {
		if err := p.Validate(); !errors.Is(err, ErrInvalidParams) {
			t.Fatalf("%+v 应被 Validate 拒绝，实际 %v", p, err)
		}
	}
	mustPass := []Params{
		{},
		{Source: SourceKernel},
		{Unit: "ssh.service", Level: "warning", Limit: 10},
		// to 在未来：只有拿到 now 才知道“未来”是多久，Validate 不判。
		{To: "2026-10-22T03:00:00Z"},
		// 只给 from：窗口的另一端由 now 决定，Validate 不判先后。
		{From: "2026-10-21T03:00:00Z"},
	}
	for _, p := range mustPass {
		if err := p.Validate(); err != nil {
			t.Fatalf("%+v 不该被 Validate 拒绝，实际 %v", p, err)
		}
	}
	// 同一个“to 在未来”的参数，在 Normalize 里必须被拒绝——两层的分工要能对得上。
	if _, err := (Params{To: "2026-10-22T03:00:00Z"}).Normalize(testNow); !errors.Is(err, ErrInvalidParams) {
		t.Fatalf("Normalize 应拒绝未来的 to，实际 %v", err)
	}
}

// TestQueryArgsAreFixed 锁定命令行形状：unit/时间/游标都只能以“一个参数项”的形式出现，
// 且参数集合与顺序固定。这条测试是“不接受任意 journalctl 参数”的实际保障。
func TestQueryArgsAreFixed(t *testing.T) {
	n, err := Params{
		Source: SourceJournal,
		Unit:   "ssh.service",
		Level:  "error",
		From:   "2026-10-21T02:00:00Z",
		To:     "2026-10-21T03:00:00Z",
		Cursor: "s=abc;i=1;b=def;m=2;t=3;x=4",
		Limit:  10,
	}.Normalize(testNow)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	runner := &fakeRunner{}
	q := newTestQuerier(t, runner)
	if _, err := q.Query(context.Background(), Params{
		Source: SourceJournal,
		Unit:   "ssh.service",
		Level:  "error",
		From:   "2026-10-21T02:00:00Z",
		To:     "2026-10-21T03:00:00Z",
		Cursor: "s=abc;i=1;b=def;m=2;t=3;x=4",
		Limit:  10,
	}); err != nil {
		t.Fatalf("Query: %v", err)
	}
	// 带游标时**不能**再给 --since/--until：journalctl 实测拒绝两者同时出现
	// （exit 1，“Please specify only one of --since=, --cursor=, and --after-cursor=.”），
	// 窗口改由 Query 解析后过滤。
	want := []string{
		"--output=json", "--no-pager", "--quiet", "--reverse", "--lines=12",
		"--unit=ssh.service", "--priority=err",
		"--cursor=s=abc;i=1;b=def;m=2;t=3;x=4",
	}
	if got := runner.spec.Args; strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("argv 不一致:\n得到 %q\n期望 %q", got, want)
	}
	if runner.spec.Program != "/usr/bin/journalctl" {
		t.Fatalf("程序路径 = %q", runner.spec.Program)
	}
	if !n.HasLevel || n.Level.Priority() != "err" {
		t.Fatalf("level 归一化 = %v/%q", n.HasLevel, n.Level.Priority())
	}
}

// TestQueryKernelArgs 验证内核日志用的开关。
func TestQueryKernelArgs(t *testing.T) {
	runner := &fakeRunner{}
	q := newTestQuerier(t, runner)
	if _, err := q.Query(context.Background(), Params{Source: SourceKernel}); err != nil {
		t.Fatalf("Query: %v", err)
	}
	joined := strings.Join(runner.spec.Args, " ")
	// journalctl 没有 --kernel（实测 systemd 255 会报 unrecognized option），只有 -k。
	if !strings.Contains(joined, " -k") {
		t.Fatalf("argv 缺少 -k: %q", runner.spec.Args)
	}
	if strings.Contains(joined, "--kernel") {
		t.Fatalf("argv 里不该有 --kernel: %q", runner.spec.Args)
	}
	if strings.Contains(joined, "--unit=") {
		t.Fatalf("内核日志不应带 --unit: %q", runner.spec.Args)
	}
}

// TestQueryParsesRealJournalOutput 用 VM 上抓到的真实字节验证解析。
func TestQueryParsesRealJournalOutput(t *testing.T) {
	runner := &fakeRunner{stdout: readFixture(t, "real-forward.jsonl")}
	q := newTestQuerier(t, runner)
	page, err := q.Query(context.Background(), Params{Limit: MaxLimit})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Entries) != 6 {
		t.Fatalf("条目数 = %d，期望 6", len(page.Entries))
	}
	if page.Scanned != 6 {
		t.Fatalf("scanned = %d，期望 6", page.Scanned)
	}
	// 真实夹具的 PRIORITY 依次是 6、3、7、0、4、6。
	wantLevels := []Level{LevelInfo, LevelError, LevelDebug, LevelEmergency, LevelWarning, LevelInfo}
	for i, want := range wantLevels {
		if page.Entries[i].Level != want {
			t.Fatalf("第 %d 条 level = %q，期望 %q", i+1, page.Entries[i].Level, want)
		}
		if page.Entries[i].Cursor == "" || page.Entries[i].Timestamp.IsZero() {
			t.Fatalf("第 %d 条缺少游标或时间", i+1)
		}
		if page.Entries[i].Identifier != "upanel-p053-fixture" {
			t.Fatalf("第 %d 条 identifier = %q", i+1, page.Entries[i].Identifier)
		}
	}
	// 第二条带口令与 Bearer 令牌：必须被抹掉，并且如实标记 redacted。
	secret := page.Entries[1]
	if !secret.Redacted {
		t.Fatal("带口令的条目应标记 redacted=true")
	}
	for _, leak := range []string{"hunter2", "abc123def456", "eyJhbGciOiJIUzI1NiJ9"} {
		if strings.Contains(secret.Message, leak) {
			t.Fatalf("消息里仍然含有秘密 %q: %q", leak, secret.Message)
		}
	}
	if !strings.Contains(secret.Message, "password=[redacted]") {
		t.Fatalf("口令应被替换成 password=[redacted]，实际 %q", secret.Message)
	}
	// 9000 字节那条：journald 给出的 MESSAGE 是 null，必须标成 binary 而不是编一段占位文本。
	big := page.Entries[5]
	if !big.Binary {
		t.Fatal("MESSAGE 为 null 的条目应标记 binary=true")
	}
	if big.Message != "" {
		t.Fatalf("非文本消息不应有文本内容，实际 %d 字节", len(big.Message))
	}
}

// TestQueryRespectsJournalOrder 验证“返回什么顺序就原样保留”。
// 命令行固定带 --reverse，所以解析层不能自作主张再排一次序。
func TestQueryRespectsJournalOrder(t *testing.T) {
	runner := &fakeRunner{stdout: readFixture(t, "real-reverse3.jsonl")}
	q := newTestQuerier(t, runner)
	page, err := q.Query(context.Background(), Params{Limit: 3})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Entries) != 3 {
		t.Fatalf("条目数 = %d，期望 3", len(page.Entries))
	}
	for i := 1; i < len(page.Entries); i++ {
		if page.Entries[i].Timestamp.After(page.Entries[i-1].Timestamp) {
			t.Fatalf("倒序被破坏：第 %d 条比第 %d 条更晚", i+1, i)
		}
	}
}

// TestQueryLevelFilteredFixture 用 --priority=warning 的真实输出确认解析不受过滤影响。
func TestQueryLevelFilteredFixture(t *testing.T) {
	runner := &fakeRunner{stdout: readFixture(t, "real-warning.jsonl")}
	q := newTestQuerier(t, runner)
	page, err := q.Query(context.Background(), Params{Level: "warning"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Entries) == 0 {
		t.Fatal("夹具里应有条目")
	}
	for _, entry := range page.Entries {
		switch entry.Level {
		case LevelEmergency, LevelAlert, LevelCritical, LevelError, LevelWarning:
		default:
			t.Fatalf("--priority=warning 的结果里出现了 %q", entry.Level)
		}
	}
}

// TestQueryPagingDropsCursorRow 验证翻页去重：--cursor 是**含**起点的，
// 所以第一行可能就是上一页的最后一条，必须按游标全等丢掉。
func TestQueryPagingDropsCursorRow(t *testing.T) {
	all := readFixture(t, "real-reverse3.jsonl")
	lines := strings.Split(strings.TrimSpace(all), "\n")
	if len(lines) < 2 {
		t.Fatalf("夹具行数不足: %d", len(lines))
	}
	var first struct {
		Cursor string `json:"__CURSOR"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("解析夹具首行: %v", err)
	}
	runner := &fakeRunner{stdout: all}
	// 夹具抓取于 testNow 之后 15 分钟；带游标时窗口是本地过滤的，
	// 所以这里用一个“当时”的时钟，否则整页都会被默认的 1 小时窗口挡掉。
	q, err := NewQuerier("/usr/bin/journalctl", runner, &fakeStreamer{}, func() time.Time {
		return testNow.Add(20 * time.Minute)
	})
	if err != nil {
		t.Fatalf("NewQuerier: %v", err)
	}
	page, err := q.Query(context.Background(), Params{Cursor: first.Cursor, Limit: 5})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Entries) != len(lines)-1 {
		t.Fatalf("条目数 = %d，期望 %d（应丢掉与 cursor 相同的第一条）", len(page.Entries), len(lines)-1)
	}
	for _, entry := range page.Entries {
		if entry.Cursor == first.Cursor {
			t.Fatal("结果里不应包含与 cursor 相同的条目")
		}
	}
	if page.NextCursor != nil {
		t.Fatalf("不足一页时 next_cursor 应为 null，实际 %q", *page.NextCursor)
	}
}

// buildEntry 造一条 journalctl JSON 行。
func buildEntry(cursor, ts, priority, message string) string {
	raw := map[string]string{
		"__CURSOR":             cursor,
		"__REALTIME_TIMESTAMP": ts,
		"PRIORITY":             priority,
		"MESSAGE":              message,
		"SYSLOG_IDENTIFIER":    "test",
	}
	b, _ := json.Marshal(raw)
	return string(b)
}

// TestQueryHasMoreAndNextCursor 验证“还有更多”时给出 next_cursor。
func TestQueryHasMoreAndNextCursor(t *testing.T) {
	var lines []string
	for i := 0; i < 5; i++ {
		lines = append(lines, buildEntry(
			fmt.Sprintf("s=a;i=%d;b=b;m=1;t=1;x=1", i),
			fmt.Sprintf("%d", 1792551729406955-int64(i)*1000),
			"6", fmt.Sprintf("line %d", i)))
	}
	runner := &fakeRunner{stdout: strings.Join(lines, "\n")}
	q := newTestQuerier(t, runner)
	page, err := q.Query(context.Background(), Params{Limit: 2})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Entries) != 2 {
		t.Fatalf("条目数 = %d，期望 2", len(page.Entries))
	}
	if !page.Truncated {
		t.Fatal("被条数截断时应标记 truncated")
	}
	if page.NextCursor == nil {
		t.Fatal("还有更多时 next_cursor 不应为空")
	}
	if *page.NextCursor != page.Entries[1].Cursor {
		t.Fatalf("next_cursor = %q，期望最后一条的游标 %q", *page.NextCursor, page.Entries[1].Cursor)
	}
}

// TestQueryPageByteLimit 验证字节上限：单页消息总量超过 1 MiB 时提前收手并标记原因。
//
// 单条消息先被 8 KiB 上限截断，所以要让总量超过 1 MiB 需要上百条——这正好说明
// 两个限额是叠加生效的：先保“单条不漏进内存”，再保“整页不漏进内存”。
func TestQueryPageByteLimit(t *testing.T) {
	big := strings.Repeat("y", MaxMessageBytes)
	var lines []string
	for i := 0; i < 200; i++ {
		lines = append(lines, buildEntry(
			fmt.Sprintf("s=a;i=%d;b=b;m=1;t=1;x=1", i),
			fmt.Sprintf("%d", 1792551729406955-int64(i)*1000),
			"6", big))
	}
	runner := &fakeRunner{stdout: strings.Join(lines, "\n")}
	q := newTestQuerier(t, runner)
	page, err := q.Query(context.Background(), Params{Limit: 200})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Entries) >= 200 {
		t.Fatalf("应被字节上限截住，实际返回 %d 条", len(page.Entries))
	}
	if len(page.Entries) == 0 {
		t.Fatal("至少应返回一条")
	}
	if page.TruncatedReason != "page_bytes" {
		t.Fatalf("truncated_reason = %q，期望 page_bytes", page.TruncatedReason)
	}
	if page.NextCursor == nil {
		t.Fatal("被字节上限截断时仍应给出 next_cursor")
	}
	total := 0
	for _, entry := range page.Entries {
		total += len(entry.Message)
	}
	if total > MaxPageBytes {
		t.Fatalf("这一页的消息总量 %d 超过上限 %d", total, MaxPageBytes)
	}
}

// TestQueryTruncatesLongMessage 验证单条消息截断与 rune 边界。
func TestQueryTruncatesLongMessage(t *testing.T) {
	// 用多字节字符制造“切点落在字符中间”的情形。
	message := strings.Repeat("日", MaxMessageBytes) // 每个 3 字节，远超上限
	line := buildEntry("s=a;i=1;b=b;m=1;t=1;x=1", "1792551729406955", "6", message)
	runner := &fakeRunner{stdout: line}
	q := newTestQuerier(t, runner)
	page, err := q.Query(context.Background(), Params{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Entries) != 1 {
		t.Fatalf("条目数 = %d", len(page.Entries))
	}
	entry := page.Entries[0]
	if !entry.Truncated {
		t.Fatal("超长消息应标记 truncated")
	}
	if len(entry.Message) > MaxMessageBytes {
		t.Fatalf("截断后长度 = %d，超过上限 %d", len(entry.Message), MaxMessageBytes)
	}
	if !utf8Valid(entry.Message) {
		t.Fatal("截断后应是合法 UTF-8（不能切在字符中间）")
	}
}

func utf8Valid(s string) bool {
	for i := 0; i < len(s); {
		if s[i] < 0x80 {
			i++
			continue
		}
		var size int
		switch {
		case s[i]&0xE0 == 0xC0:
			size = 2
		case s[i]&0xF0 == 0xE0:
			size = 3
		case s[i]&0xF8 == 0xF0:
			size = 4
		default:
			return false
		}
		if i+size > len(s) {
			return false
		}
		i += size
	}
	return true
}

// TestQueryCursorRejected 验证无效游标被识别成“参数问题”，而不是内部错误。
func TestQueryCursorRejected(t *testing.T) {
	runner := &fakeRunner{exit: 1, stderr: "Failed to seek to cursor: Invalid argument\n"}
	q := newTestQuerier(t, runner)
	_, err := q.Query(context.Background(), Params{Cursor: "s=deadbeef;i=1", Limit: 5})
	if !errors.Is(err, ErrCursorRejected) {
		t.Fatalf("应返回 ErrCursorRejected，实际 %v", err)
	}
}

// TestQueryMapsTimeout 验证超时被明确说出来。
func TestQueryMapsTimeout(t *testing.T) {
	runner := &fakeRunner{err: fmt.Errorf("%w（/usr/bin/journalctl，上限 15s）", syscmd.ErrTimeout)}
	q := newTestQuerier(t, runner)
	_, err := q.Query(context.Background(), Params{})
	if err == nil || !strings.Contains(err.Error(), "没有完成") {
		t.Fatalf("超时应给出明确错误，实际 %v", err)
	}
}

// TestLevelUnknownIsNotInfo 验证读不出的级别不会被冒充成 info。
func TestLevelUnknownIsNotInfo(t *testing.T) {
	if got := LevelFromPriority(""); got != LevelUnknown {
		t.Fatalf("空 PRIORITY 应映射成 unknown，实际 %q", got)
	}
	if got := LevelFromPriority("9"); got != LevelUnknown {
		t.Fatalf("越界 PRIORITY 应映射成 unknown，实际 %q", got)
	}
	line := buildEntry("s=a;i=1;b=b;m=1;t=1;x=1", "1792551729406955", "", "no priority")
	runner := &fakeRunner{stdout: line}
	q := newTestQuerier(t, runner)
	page, err := q.Query(context.Background(), Params{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if page.Entries[0].Level != LevelUnknown {
		t.Fatalf("level = %q，期望 unknown", page.Entries[0].Level)
	}
}

// TestQueryToleratesTruncatedOutput 验证被截断的 stdout 不会产生半条脏数据。
func TestQueryToleratesTruncatedOutput(t *testing.T) {
	full := buildEntry("s=a;i=1;b=b;m=1;t=1;x=1", "1792551729406955", "6", "complete")
	half := `{"__CURSOR":"s=a;i=2;b=b;m=1;t=1;x=`
	runner := &fakeRunner{stdout: full + "\n" + half}
	q := newTestQuerier(t, runner)
	page, err := q.Query(context.Background(), Params{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Entries) != 1 || page.Entries[0].Message != "complete" {
		t.Fatalf("应只保留完整那一行，实际 %+v", page.Entries)
	}
}

// ---- 流 ----

func TestStreamEmitsEntriesAndCloses(t *testing.T) {
	streamer := &fakeStreamer{lines: []string{
		buildEntry("s=a;i=1;b=b;m=1;t=1;x=1", "1792551729406955", "6", "first"),
		buildEntry("s=a;i=2;b=b;m=1;t=1;x=2", "1792551729406956", "3", "second password=hunter2"),
	}}
	q, err := NewQuerier("/usr/bin/journalctl", &fakeRunner{}, streamer, fixedClock)
	if err != nil {
		t.Fatalf("NewQuerier: %v", err)
	}
	var got []Entry
	end, err := q.Stream(context.Background(), Params{Unit: "ssh.service"},
		StreamOptions{MaxEntries: 2, IdleTimeout: 5 * time.Second},
		func(entry Entry) error {
			got = append(got, entry)
			return nil
		})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if end != StreamEndEntries {
		t.Fatalf("结束原因 = %q，期望 %q", end, StreamEndEntries)
	}
	if len(got) != 2 {
		t.Fatalf("收到 %d 条，期望 2", len(got))
	}
	if !got[1].Redacted {
		t.Fatal("带口令的流条目也应标记 redacted")
	}
	// --follow 的参数：有 unit 就要有 --unit，没有游标就从现在开始。
	args := strings.Join(streamer.spec.Args, " ")
	if !strings.Contains(args, "--follow") || !strings.Contains(args, "--unit=ssh.service") {
		t.Fatalf("argv = %q", streamer.spec.Args)
	}
	if !strings.Contains(args, "--lines=0") {
		t.Fatalf("无游标时应从“现在”开始: %q", streamer.spec.Args)
	}
	// 关键：返回之前必须已经关掉子进程。
	if streamer.stream == nil || !streamer.stream.exited {
		t.Fatal("Stream 返回时子进程应已被关闭")
	}
}

func TestStreamResumeUsesCursor(t *testing.T) {
	streamer := &fakeStreamer{lines: []string{
		buildEntry("s=a;i=9;b=b;m=1;t=1;x=9", "1792551729406955", "6", "resumed"),
	}}
	q, _ := NewQuerier("/usr/bin/journalctl", &fakeRunner{}, streamer, fixedClock)
	if _, err := q.Stream(context.Background(),
		Params{Cursor: "s=a;i=8;b=b;m=1;t=1;x=8"},
		StreamOptions{MaxEntries: 1, IdleTimeout: 5 * time.Second},
		func(Entry) error { return nil }); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	args := strings.Join(streamer.spec.Args, " ")
	if !strings.Contains(args, "--after-cursor=s=a;i=8;b=b;m=1;t=1;x=8") {
		t.Fatalf("续传应使用 --after-cursor: %q", streamer.spec.Args)
	}
	if !strings.Contains(args, "--lines=100") {
		t.Fatalf("续传应限制首批补发条数: %q", streamer.spec.Args)
	}
}

func TestStreamClientClosedStops(t *testing.T) {
	streamer := &fakeStreamer{}
	q, _ := NewQuerier("/usr/bin/journalctl", &fakeRunner{}, streamer, fixedClock)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan StreamEnd, 1)
	go func() {
		end, err := q.Stream(ctx, Params{}, StreamOptions{IdleTimeout: 30 * time.Second},
			func(Entry) error { return nil })
		if err != nil {
			t.Errorf("Stream: %v", err)
		}
		done <- end
	}()
	// 等子进程真的起来了再取消，避免测到“还没启动就取消”的空路径。
	deadline := time.Now().Add(2 * time.Second)
	for !streamer.started() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case end := <-done:
		if end != StreamEndClientClosed {
			t.Fatalf("结束原因 = %q，期望 client_closed", end)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("取消后 Stream 没有返回（子进程可能没被收掉）")
	}
	if !streamer.stream.exited {
		t.Fatal("取消后子进程应已被关闭")
	}
}

func TestStreamIdleTimeout(t *testing.T) {
	streamer := &fakeStreamer{} // 一行都没有：模拟安静的系统
	q, _ := NewQuerier("/usr/bin/journalctl", &fakeRunner{}, streamer, fixedClock)
	end, err := q.Stream(context.Background(), Params{},
		StreamOptions{IdleTimeout: 50 * time.Millisecond}, func(Entry) error { return nil })
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if end != StreamEndIdle {
		t.Fatalf("结束原因 = %q，期望 idle_timeout", end)
	}
	if !streamer.stream.exited {
		t.Fatal("空闲超时后子进程应已被关闭")
	}
}

func TestStreamEmitFailureCloses(t *testing.T) {
	streamer := &fakeStreamer{lines: []string{
		buildEntry("s=a;i=1;b=b;m=1;t=1;x=1", "1792551729406955", "6", "first"),
	}}
	q, _ := NewQuerier("/usr/bin/journalctl", &fakeRunner{}, streamer, fixedClock)
	end, err := q.Stream(context.Background(), Params{},
		StreamOptions{IdleTimeout: 5 * time.Second},
		func(Entry) error { return errors.New("对端写不动了") })
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if end != StreamEndClientClosed {
		t.Fatalf("emit 失败应视为客户端断开，实际 %q", end)
	}
	if !streamer.stream.exited {
		t.Fatal("emit 失败后子进程应已被关闭")
	}
}

func TestStreamRejectsBadParams(t *testing.T) {
	q, _ := NewQuerier("/usr/bin/journalctl", &fakeRunner{}, &fakeStreamer{}, fixedClock)
	_, err := q.Stream(context.Background(), Params{Source: "syslog"}, StreamOptions{}, func(Entry) error { return nil })
	if !errors.Is(err, ErrInvalidParams) {
		t.Fatalf("应返回 ErrInvalidParams，实际 %v", err)
	}
}

// TestQueryCursorAppliesWindowLocally 验证带游标时窗口在本地过滤。
//
// journalctl 实测拒绝 --since/--until 与 --cursor 同时出现
// （“Please specify only one of --since=, --cursor=, and --after-cursor=.”），
// 所以窗口必须在解析之后自己过滤，否则“翻页 + 时间窗口”这个组合根本跑不通。
func TestQueryCursorAppliesWindowLocally(t *testing.T) {
	cursorRow := buildEntry("s=a;i=9;b=b;m=1;t=1;x=9", fmt.Sprintf("%d", testNow.Add(-1*time.Minute).UnixMicro()), "6", "边界条目")
	inWindow := buildEntry("s=a;i=8;b=b;m=1;t=1;x=8", fmt.Sprintf("%d", testNow.Add(-2*time.Minute).UnixMicro()), "6", "窗口内")
	outWindow := buildEntry("s=a;i=7;b=b;m=1;t=1;x=7", fmt.Sprintf("%d", testNow.Add(-30*time.Minute).UnixMicro()), "6", "窗口外")
	runner := &fakeRunner{stdout: strings.Join([]string{cursorRow, inWindow, outWindow}, "\n") + "\n"}
	q := newTestQuerier(t, runner)
	page, err := q.Query(context.Background(), Params{
		From:   testNow.Add(-10 * time.Minute).Format(time.RFC3339Nano),
		To:     testNow.Format(time.RFC3339Nano),
		Cursor: "s=a;i=9;b=b;m=1;t=1;x=9",
		Limit:  10,
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Entries) != 1 || page.Entries[0].Message != "窗口内" {
		t.Fatalf("条目 = %+v，期望只剩窗口内那一条", page.Entries)
	}
	if page.Scanned != 3 {
		t.Fatalf("scanned = %d，期望 3（扫到三行，返回一条）", page.Scanned)
	}
	for _, arg := range runner.spec.Args {
		if strings.HasPrefix(arg, "--since=") || strings.HasPrefix(arg, "--until=") {
			t.Fatalf("带游标时不该给时间参数: %q", runner.spec.Args)
		}
	}
}

// TestQueryCursorAdvancesWhenPageFallsOutsideWindow 验证“整页都被窗口过滤掉”时
// next_cursor 仍然往前走：否则调用方会拿着同一个游标原地打转，
// 表现就是每一页都重复上一页的边界条目（真实 VM 验收里出现过）。
func TestQueryCursorAdvancesWhenPageFallsOutsideWindow(t *testing.T) {
	rows := []string{buildEntry("s=a;i=5;b=b;m=1;t=1;x=5", fmt.Sprintf("%d", testNow.Add(-40*time.Minute).UnixMicro()), "6", "旧1")}
	rows = append(rows, buildEntry("s=a;i=4;b=b;m=1;t=1;x=4", fmt.Sprintf("%d", testNow.Add(-41*time.Minute).UnixMicro()), "6", "旧2"))
	rows = append(rows, buildEntry("s=a;i=3;b=b;m=1;t=1;x=3", fmt.Sprintf("%d", testNow.Add(-42*time.Minute).UnixMicro()), "6", "旧3"))
	runner := &fakeRunner{stdout: strings.Join(rows, "\n") + "\n"}
	q := newTestQuerier(t, runner)
	page, err := q.Query(context.Background(), Params{
		From:   testNow.Add(-10 * time.Minute).Format(time.RFC3339Nano),
		To:     testNow.Format(time.RFC3339Nano),
		Cursor: "s=a;i=6;b=b;m=1;t=1;x=6",
		Limit:  1,
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Entries) != 0 {
		t.Fatalf("窗口外的条目不该返回: %+v", page.Entries)
	}
	if page.NextCursor == nil {
		t.Fatal("整页都在窗口外时 next_cursor 仍然要往前走，不能是 null")
	}
	if *page.NextCursor != "s=a;i=3;b=b;m=1;t=1;x=3" {
		t.Fatalf("next_cursor = %q，期望扫到的最老一条", *page.NextCursor)
	}
	if page.NextCursor != nil && *page.NextCursor == "s=a;i=6;b=b;m=1;t=1;x=6" {
		t.Fatal("next_cursor 不能还是请求时那个游标（会原地打转）")
	}
}

// TestQueryArgsTimeIsJournalctlCompatible 把传给 journalctl 的时间形状钉死。
//
// 为什么需要这条：参数是"我们自己格式化出来的"，所以它看起来不值一测——
// 但 RFC3339Nano（`2026-10-27T13:29:43.1270959Z`）在 Ubuntu 22.04 的
// journalctl（systemd 249）上 exit 1："Failed to parse timestamp"，
// 于是那台机器上**每一条** /logs 查询都是 400；24.04（systemd 255）却全对。
// 只在一个客户机版本上验收时，这种差异会被读成"日志功能没问题"。
//
// 这里同时钉三件事：空格分隔（不是 T）、固定微秒精度、显式 UTC 时区。
func TestQueryArgsTimeIsJournalctlCompatible(t *testing.T) {
	from := time.Date(2026, 10, 27, 13, 29, 43, 127095900, time.UTC)
	to := from.Add(time.Hour)
	args := queryArgs(Normalized{Source: SourceJournal, From: from, To: to, Limit: 10})

	find := func(prefix string) string {
		for _, arg := range args {
			if strings.HasPrefix(arg, prefix) {
				return strings.TrimPrefix(arg, prefix)
			}
		}
		t.Fatalf("参数里没有 %s：%q", prefix, args)
		return ""
	}

	gotFrom := find("--since=")
	gotTo := find("--until=")
	if gotFrom != "2026-10-27 13:29:43.127095 UTC" {
		t.Fatalf("--since = %q，期望 \"2026-10-27 13:29:43.127095 UTC\"", gotFrom)
	}
	if gotTo != "2026-10-27 14:29:43.127095 UTC" {
		t.Fatalf("--until = %q，期望 \"2026-10-27 14:29:43.127095 UTC\"", gotTo)
	}
	// 形状本身再查一遍：ISO 的 T 分隔符是 22.04 上的那个坑。
	// 只看第 10 个字符（日期与时间之间），不要搜 "T"——末尾的 "UTC" 里就有一个。
	for _, value := range []string{gotFrom, gotTo} {
		if len(value) < 11 || value[10] != ' ' {
			t.Fatalf("时间 %q 的第 11 个字符不是空格：ISO 的 T 分隔符 systemd 249 不认", value)
		}
		body := strings.TrimSuffix(value, " UTC")
		if _, err := time.Parse("2006-01-02 15:04:05.000000", body); err != nil {
			t.Fatalf("时间 %q 不符合 journalctl 认得的微秒精度形状：%v", value, err)
		}
	}
	// 非 UTC 的输入也必须写成 UTC：不带时区时 journalctl 按客户机本地时区解释，
	// 窗口会整体偏移，页面上看到的时间段就不是用户以为的那一段。
	shanghai := time.FixedZone("CST", 8*3600)
	zoneArgs := queryArgs(Normalized{Source: SourceJournal, From: from.In(shanghai), To: to.In(shanghai), Limit: 10})
	for _, arg := range zoneArgs {
		if strings.HasPrefix(arg, "--since=") && arg != "--since=2026-10-27 13:29:43.127095 UTC" {
			t.Fatalf("非 UTC 输入被写成 %q，期望同一时刻的 UTC 形式", arg)
		}
	}
}
