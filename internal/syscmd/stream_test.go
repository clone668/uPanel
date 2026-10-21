package syscmd

import (
	"bufio"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestStreamReadsIncrementallyAndCloses 验证 P05.3 需要的两件事：
//  1. 输出是**边到边读**的（Start 返回后就能拿到前几行，不必等进程结束）；
//  2. Close 之后子进程确实被收掉了（不留下一个永远挂着的 journalctl）。
//
// 第二条是 STAGES.md 第 7 节验收里“结束日志订阅不会残留无限 journalctl 子进程”的
// 底层保障：如果这里漏了，上层再怎么写都会漏进程。
func TestStreamReadsIncrementallyAndCloses(t *testing.T) {
	exe, args := helper(t, "stream-lines")
	stream, err := Start(context.Background(), Spec{Program: exe, Args: args}, Limits{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if stream.Pid() == 0 {
		t.Fatal("Pid 应为正数")
	}

	reader := bufio.NewReader(stream.Stdout())
	var lines []string
	deadline := time.After(10 * time.Second)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for len(lines) < 3 {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			lines = append(lines, strings.TrimRight(line, "\r\n"))
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = stream.Close()
		t.Fatalf("10 秒内没读到三行（说明 Start 并没有让输出边到边读）")
	}
	<-deadline
	for i, want := range []string{"line 0", "line 1", "line 2"} {
		if i >= len(lines) || lines[i] != want {
			t.Fatalf("第 %d 行 = %q，期望 %q", i+1, lines[i], want)
		}
	}

	// 进程此刻还在挂着（模拟 journalctl -f 的安静期）。Close 必须让它迅速结束。
	start := time.Now()
	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Close 用了 %s，说明没有及时终止子进程", elapsed)
	}
	// 重复 Close 必须安全（调用方会 defer，又可能在正常路径上主动关一次）。
	if err := stream.Close(); err != nil {
		t.Fatalf("重复 Close: %v", err)
	}
}

// TestStreamContextCancelKills 验证 ctx 取消等价于 Close：请求被取消时
// （Web 断开、代理停机）子进程不能继续跑。
func TestStreamContextCancelKills(t *testing.T) {
	exe, args := helper(t, "stream-lines")
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := Start(ctx, Spec{Program: exe, Args: args}, Limits{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = stream.Close() }()

	// 等到确实有输出（说明进程已经起来），再取消。
	reader := bufio.NewReader(stream.Stdout())
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatalf("读第一行失败: %v", err)
	}
	cancel()

	waitDone := make(chan error, 1)
	go func() { waitDone <- stream.Wait() }()
	select {
	case err := <-waitDone:
		var exitErr *ExitError
		if err != nil && !errors.As(err, &exitErr) {
			t.Fatalf("取消后 Wait 应返回 *ExitError（被信号终止）或 nil，实际 %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ctx 取消后子进程没有结束")
	}
}

// TestStreamRejectsBadProgram 验证流式入口与 Run 共用同一套边界校验。
func TestStreamRejectsBadProgram(t *testing.T) {
	if _, err := Start(context.Background(), Spec{Program: ""}, Limits{}); !errors.Is(err, ErrEmptyProgram) {
		t.Fatalf("空程序路径应返回 ErrEmptyProgram，实际 %v", err)
	}
	for _, prog := range []string{"journalctl", "/bin/sh -c id", "/bin/echo|id"} {
		if _, err := Start(context.Background(), Spec{Program: prog}, Limits{}); err == nil {
			t.Fatalf("%q 应被拒绝", prog)
		}
	}
}

// TestStreamCapsStderr 验证 stderr 仍然有上限。
func TestStreamCapsStderr(t *testing.T) {
	exe, args := helper(t, "die-with-stderr")
	stream, err := Start(context.Background(), Spec{Program: exe, Args: args}, Limits{MaxStderr: 64})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := stream.Wait(); err == nil {
		t.Fatal("应以非零状态结束")
	}
	_ = stream.Close()
	if len(stream.Stderr()) > 64 {
		t.Fatalf("stderr 长度 = %d，超过上限 64", len(stream.Stderr()))
	}
	if !stream.StderrTruncated() {
		t.Fatal("超限时应标记 StderrTruncated")
	}
}
