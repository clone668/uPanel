package backup

// restic 子进程的执行边界。
//
// 这里**没有**复用 internal/syscmd，原因只有一个：syscmd 的环境白名单是给 apt 用的
// 闭集（DEBIAN_FRONTEND 等），而 restic 必须拿到仓库口令。口令不能进 argv
// （`/proc/<pid>/cmdline` 对同机所有用户可读），只能走环境；给 syscmd 开一个
// “任意环境变量”的口子，等于把那道边界拆掉。
//
// 所以这里是第二道边界，而且比 syscmd 更窄：
//   - 环境**不是**继承来的，而是固定底（PATH/LANG/LC_ALL/HOME）+ 下面三个键；
//   - 键是硬编码的常量，不接受调用方传入键名；
//   - 值只来自秘密存储的解密结果或代码里的常量。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// 允许追加给 restic 的环境变量名。硬编码，不接受调用方提供键名。
const (
	// EnvResticPassword 是仓库口令（CONTRACTS.md 第 12 节：秘密不进 argv）。
	EnvResticPassword = "RESTIC_PASSWORD"
	// EnvResticCacheDir 把 restic 的缓存钉在代理自己的目录里，
	// 而不是 /root/.cache——那会让 root 的 home 变成一处没人清理的状态。
	EnvResticCacheDir = "RESTIC_CACHE_DIR"
	// EnvHome 是 restic 计算默认缓存/配置位置用的 HOME。
	EnvHome = "HOME"
)

// fixedBaseEnv 是子进程看到的固定底环境。
var fixedBaseEnv = []string{
	"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	"LANG=C.UTF-8",
	"LC_ALL=C.UTF-8",
}

// 默认限额。
const (
	// defaultExecTimeout 是一次 restic 调用的上限。init 在 SFTP 上要建目录、
	// 写几个小文件，本机上是几十毫秒；10 分钟是“慢到不该再等”的量级。
	defaultExecTimeout = 10 * time.Minute
	// defaultMaxStdout 是 restic 输出的上限。`--json` 的 cat config 只有几百字节，
	// 1 MiB 是给将来 snapshots --json 留的。
	defaultMaxStdout = 1 << 20
	// defaultMaxStderr 是 stderr 的上限（restic 的报错通常是一行）。
	defaultMaxStderr = 64 << 10
)

// ExecSpec 是一次 restic 调用。
type ExecSpec struct {
	// Program 必须是绝对路径，来自装配层（不是请求参数）。
	Program string
	// Args 逐项传参，不经任何 shell。
	Args []string
	// Password 是要放进 RESTIC_PASSWORD 的值；空串表示不设置。
	Password string
	// CacheDir 是 RESTIC_CACHE_DIR 的值；空串表示不设置。
	CacheDir string
	// Home 是 HOME 的值；空串表示用默认的 /root。
	Home string
	// Timeout 覆盖默认上限。
	Timeout time.Duration
	// MaxStdout / MaxStderr 覆盖默认上限。
	MaxStdout int64
	MaxStderr int64
	// StdoutWriter 非空时额外接收完整 stdout 流（例如 restic dump 的字节哈希）。
	// 有界 Stdout 仍然保留尾部用于诊断；文件内容不会因此在内存里累积。
	// 它只能由固定动作设置，不接受请求方提供 writer 或路径。
	StdoutWriter io.Writer
	// ExtraFiles 只给这一次子进程显式继承的已打开句柄。第一个在子进程中固定为 fd 3；
	// 用于安全恢复暂存目录，不能由请求方提供，也不清除父目录 fd 的 CLOEXEC。
	ExtraFiles []*os.File
	// OnStdoutLine 逐行回调 stdout（进度用）。它只拿到行，不拿到"要不要继续"的权力：
	// 说停的机制是 ctx，不是回调的返回值。
	//
	// 回调在 cmd.Run() 期间从读管道的那个 goroutine 上调用，实现方不要在里面阻塞太久，
	// 也不要在里面再起一个 restic（那会与本进程的子进程组语义打架）。
	OnStdoutLine func(line string)
}

// ExecResult 是一次 restic 调用的结果。
type ExecResult struct {
	Program         string
	ExitCode        int
	Signal          string
	Stdout          string
	Stderr          string
	StdoutTruncated bool
	StderrTruncated bool
	TimedOut        bool
	Duration        time.Duration
	// ProgressDropped 是被丢掉的进度行数（队列满时丢最老的一条）。
	//
	// 进度是"最新状态"而不是流水账，丢几行不影响结论——真正要留的结论在 Stdout 里，
	// 那一路不经过进度队列。留这个计数是为了出问题时有据可查，不是为了报错。
	ProgressDropped int64
}

// ErrExecTimeout 表示 restic 超出运行时限被终止。
var ErrExecTimeout = errors.New("backup: restic 超过运行时限，已终止")

// ErrOutputIncomplete 表示子进程已经按 0 退出，但它的输出没能在等待窗口内读完。
//
// 这不是"restic 失败了"，而是"我们没拿到它的结论"。24.04 真机上撞到的正是这件事：
// 进程退出码 0、快照也建成了，但 stdout 没读完，面板上写的是
// "restic backup 失败（退出状态 0）"——一句话把成功说成了失败。
var ErrOutputIncomplete = errors.New("backup: restic 的输出没有读完（进程已按 0 退出，结论不完整，不等于它失败了）")

// ErrBudgetExhausted 表示**上层**的时间预算在 restic 给出结论之前用尽。
//
// 它和 ErrExecTimeout 是两件事：那条是"restic 自己跑太久"，这条是"调用方没等到它
// 说完"。真机上撞到过：只读查询的 30 秒预算用尽，restic 被一起杀掉，而面板上写的
// 是"restic 以退出状态 -1 结束"——读起来像 restic 崩了，其实是我们自己收的工。
// 没有结论就是没有结论，不能说成仓库或口令有问题。
var ErrBudgetExhausted = errors.New("backup: 请求的时间预算用尽，restic 被中止（这次没有结论，不等于仓库有问题）")

// RunRestic 执行一次 restic 命令。
//
// 返回 nil error 表示正常退出；非零退出码返回 *ExitError（restic 自己的报错在
// Stderr 里，调用方据此给出人话）；restic 自己跑太久返回 ErrExecTimeout；
// 上层 ctx 先到期返回 ErrBudgetExhausted（两种超时不是一回事，见 runOutcome）。
func RunRestic(ctx context.Context, spec ExecSpec) (ExecResult, error) {
	res := ExecResult{Program: spec.Program, ExitCode: -1}
	if spec.Program == "" {
		return res, errors.New("backup: 没有 restic 程序路径")
	}
	if err := checkProgramPath(spec.Program); err != nil {
		return res, err
	}

	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = defaultExecTimeout
	}
	maxOut := spec.MaxStdout
	if maxOut <= 0 {
		maxOut = defaultMaxStdout
	}
	maxErr := spec.MaxStderr
	if maxErr <= 0 {
		maxErr = defaultMaxStderr
	}

	env := append([]string{}, fixedBaseEnv...)
	home := spec.Home
	if home == "" {
		home = "/root"
	}
	env = append(env, EnvHome+"="+home)
	if spec.CacheDir != "" {
		env = append(env, EnvResticCacheDir+"="+spec.CacheDir)
	}
	if spec.Password != "" {
		env = append(env, EnvResticPassword+"="+spec.Password)
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, spec.Program, spec.Args...)
	cmd.Env = env
	cmd.ExtraFiles = spec.ExtraFiles
	cmd.Stdin = nil
	stdout := &cappedBuffer{limit: maxOut}
	if spec.OnStdoutLine != nil {
		stdout.emit = newLineEmitter(spec.OnStdoutLine)
	}
	stderr := &cappedBuffer{limit: maxErr}
	cmd.Stdout = stdout
	if spec.StdoutWriter != nil {
		if spec.OnStdoutLine != nil {
			return res, errors.New("backup: 同一次调用不能同时流式读取 stdout 与解析进度行")
		}
		cmd.Stdout = io.MultiWriter(stdout, spec.StdoutWriter)
	}
	cmd.Stderr = stderr
	setProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	cmd.WaitDelay = 2 * time.Second

	start := time.Now()
	err := cmd.Run()
	res.Duration = time.Since(start)
	// 收工前把队列里的进度行处理完：cmd.Run 只保证管道排空了，消费协程可能还在
	// 处理最后几行。这一步只等"最多几行的写库时间"，与命令本身成没成功无关。
	if stdout.emit != nil {
		stdout.emit.close()
		res.ProgressDropped = stdout.emit.dropped
	}
	res.Stdout = stdout.String()
	res.Stderr = stderr.String()
	res.StdoutTruncated = stdout.truncated
	res.StderrTruncated = stderr.truncated
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
		res.Signal = processSignalName(cmd.ProcessState)
	}
	return runOutcome(ctx, runCtx, timeout, res, err)
}

// runOutcome 把一次 cmd.Run() 的结果翻译成我们自己的错误分类。
//
// 单独拆成一个纯函数是为了让这张判定表能被钉死：真机上撞到的正是"上层预算用尽"
// 被翻译成"restic 以退出状态 -1 结束"。它不碰进程，只读三个 ctx 状态、res 与 err。
func runOutcome(ctx, runCtx context.Context, timeout time.Duration, res ExecResult, err error) (ExecResult, error) {
	switch {
	case errors.Is(runCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil:
		res.TimedOut = true
		res.ExitCode = -1
		return res, fmt.Errorf("%w（上限 %s）", ErrExecTimeout, timeout)
	case ctx.Err() != nil:
		// 上层先到期或先取消：这不是 restic 的退出，也不是它的错。
		res.ExitCode = -1
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			res.TimedOut = true
			return res, ErrBudgetExhausted
		}
		return res, fmt.Errorf("backup: restic 调用被取消: %w", ctx.Err())
	case errors.Is(runCtx.Err(), context.Canceled):
		return res, fmt.Errorf("backup: restic 调用被取消: %w", runCtx.Err())
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return res, &ExitError{Program: res.Program, ExitCode: res.ExitCode, Signal: res.Signal}
	}
	// 进程按 0 退出，但管道在 WaitDelay 之内没读到 EOF：输出不完整，不是它失败。
	// os/exec 只在"命令本身没出错"时才把 ErrWaitDelay 交出来，所以这里读到它，
	// 就已经确定退出状态是 0（信号名同样为空）。结论拿不到就说结论拿不到，
	// 不能说成 restic 失败——那句话会让人去查仓库、查口令、查磁盘，全都查不到。
	if errors.Is(err, exec.ErrWaitDelay) {
		return res, ErrOutputIncomplete
	}
	if err != nil {
		return res, fmt.Errorf("backup: 启动 %s 失败: %w", res.Program, err)
	}
	return res, nil
}

// ExitError 表示 restic 以非零状态退出。
type ExitError struct {
	Program  string
	ExitCode int
	// Signal 是被终止时的信号名（正常退出为空）。
	//
	// 单独留一个字段：`ExitCode` 为 -1 时那句"以退出状态 -1 结束"什么都没说——
	// 谁停的、是崩溃还是被杀，读的人只能猜。P11.2 在 syscmd 里修过同一类问题。
	Signal string
}

func (e *ExitError) Error() string {
	if e.Signal != "" {
		return fmt.Sprintf("backup: %s 被 %s 终止（不是它自己退出的，可能是崩溃或被系统杀掉）", e.Program, e.Signal)
	}
	return fmt.Sprintf("backup: %s 以退出状态 %d 结束", e.Program, e.ExitCode)
}

// checkProgramPath 校验程序路径：绝对路径，且不含 shell 元字符。
func checkProgramPath(program string) error {
	for _, r := range program {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '/', r == '.', r == '_', r == '-', r == '+':
		default:
			return fmt.Errorf("backup: 程序路径含非法字符 %q", r)
		}
	}
	if !filepath.IsAbs(program) {
		return fmt.Errorf("backup: 程序路径必须是绝对路径（%q）", program)
	}
	return nil
}

// cappedBuffer 保留输出的尾部，其余读完丢弃（不阻塞子进程）。
//
// emit 非空时把读到的字节按行喂给进度队列。两条路互不影响：落盘的那一份受 limit
// 约束，回调拿到的是流上的行，不会因为"输出太大被截断"而丢掉进度。
//
// 保**尾部**而不是开头，是因为结论在最后一行：restic 的 summary / error 都写在
// 收尾处。1 MiB 的进度在密集输出下大约六七分钟就写满，保开头等于把"快照建成了"
// 这句话丢掉，于是成功会变成"restic 没有报告快照 id"。
type cappedBuffer struct {
	buf       []byte
	limit     int64
	truncated bool
	emit      *lineEmitter
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	c.buf = append(c.buf, p...)
	// 超过两倍上限才砍，砍到只剩最后 limit 字节：这样均摊下来每字节只搬一次，
	// 不会变成"每来一小段就把 1 MiB 挪一遍"。
	if int64(len(c.buf)) > 2*c.limit {
		c.buf = append(c.buf[:0], c.buf[int64(len(c.buf))-c.limit:]...)
		c.truncated = true
	}
	if c.emit != nil {
		c.emit.write(p)
	}
	return n, nil
}

func (c *cappedBuffer) String() string { return string(c.buf) }

// maxLineBytes 是单行的上限（进度行只有几百字节；整行超限说明这不是我们认得的输出，
// 丢掉即可，不能让它把内存撑开）。
const maxLineBytes = 64 << 10

// maxQueuedLines 是进度行的排队上限。队列满时丢最老的一条。
const maxQueuedLines = 256

// lineEmitter 把 stdout 切成行交给进度回调。
//
// 回调**不在**管道的读协程上跑，这是这一层唯一真正要紧的规矩。它是从真机上学来的：
// restic 的 --json 进度密到每几十毫秒一条（24.04 上 4.6 秒 60 条），而回调那条路上
// 要同步写一次 SQLite（同一台虚机上单次 50—150ms）。读协程一旦被回调拖住，管道就
// 排不空；进程退出后 os/exec 的 WaitDelay 到点返回 ErrWaitDelay——restic 退出码 0、
// 快照也建成了，面板上却是"restic backup 失败（退出状态 0）"。
//
// 所以这里只做一件事：把行塞进队列，塞不进去就丢最老的一条。丢的只会是进度行；
// 结论（summary / error）留在缓冲区那一份里，那条路不经过队列。
type lineEmitter struct {
	onLine func(string)
	lines  chan string
	done   chan struct{}
	// 下面四个只在读协程里动，读协程退出后才由调用方读 dropped（close 同步过）。
	pending  []byte
	overlong bool
	dropped  int64
}

func newLineEmitter(onLine func(string)) *lineEmitter {
	e := &lineEmitter{
		onLine: onLine,
		lines:  make(chan string, maxQueuedLines),
		done:   make(chan struct{}),
	}
	go e.consume()
	return e
}

// consume 是唯一的消费者：一行一行交给回调，不掺管道的事。
func (e *lineEmitter) consume() {
	defer close(e.done)
	for line := range e.lines {
		e.onLine(line)
	}
}

// close 等队列处理完。返回后 dropped 才可以读。
func (e *lineEmitter) close() {
	close(e.lines)
	<-e.done
}

// write 把一段字节切成行入队。
func (e *lineEmitter) write(p []byte) {
	for len(p) > 0 {
		idx := bytes.IndexByte(p, byte(10))
		if idx < 0 {
			e.append(p)
			return
		}
		e.append(p[:idx])
		e.push()
		p = p[idx+1:]
	}
}

// append 追加一段未成行的字节；超限就整行丢弃直到下一个换行。
func (e *lineEmitter) append(b []byte) {
	if e.overlong {
		return
	}
	if len(e.pending)+len(b) > maxLineBytes {
		e.overlong = true
		e.pending = e.pending[:0]
		return
	}
	e.pending = append(e.pending, b...)
}

// push 把攒够的一行入队。绝不阻塞：队列满就丢最老的一条。
func (e *lineEmitter) push() {
	e.overlong = false
	line := string(e.pending)
	e.pending = e.pending[:0]
	select {
	case e.lines <- line:
		return
	default:
	}
	// 消费得比产生慢：丢掉最老的那条给这一条腾位置。进度是"最新状态"，
	// 中间态没有留档价值；而最后一条（100%）必须留下。
	select {
	case <-e.lines:
		e.dropped++
	default:
	}
	select {
	case e.lines <- line:
	default:
		e.dropped++
	}
}

// lastLine 取输出里最后一条非空行，用于把 restic 的报错压成一句话。
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return line
		}
	}
	return ""
}
