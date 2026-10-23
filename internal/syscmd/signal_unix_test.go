//go:build unix

package syscmd

// 被信号终止时 Result.Signal 必须有值（P11.2）。
//
// 这条是补上去的，理由写在它自己身上：硬杀 apt 的验收一开始只能看到
// 「退出码 -1」，因为 syscmd 把 WaitStatus 里的信号名丢掉了。上层要"谁停的"
// 这个事实，就只能自己猜——而猜出来的原因不叫原因。

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRunRecordsSignalWhenKilled(t *testing.T) {
	// 子进程给自己发 SIGKILL：这是硬杀时最真实的形状（父进程毫无机会清理）。
	res, err := Run(context.Background(),
		Spec{Program: "/bin/sh", Args: []string{"-c", "kill -9 $$"}},
		Limits{Timeout: 20 * time.Second})
	if err == nil {
		t.Fatal("被 SIGKILL 终止应当返回错误")
	}
	var exitErr *ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("应返回 *ExitError，实际 %v", err)
	}
	if res.ExitCode != -1 {
		t.Fatalf("退出码 = %d，期望 -1（被信号终止）", res.ExitCode)
	}
	if res.Signal != "SIGKILL" {
		t.Fatalf("Signal = %q，期望 SIGKILL", res.Signal)
	}
	if res.TimedOut {
		t.Fatal("这不是超时，是我们自己发起的终止")
	}
}

func TestRunLeavesSignalEmptyOnNormalExit(t *testing.T) {
	res, err := Run(context.Background(),
		Spec{Program: "/bin/sh", Args: []string{"-c", "exit 3"}},
		Limits{Timeout: 20 * time.Second})
	var exitErr *ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("应返回 *ExitError，实际 %v", err)
	}
	if res.ExitCode != 3 {
		t.Fatalf("退出码 = %d，期望 3", res.ExitCode)
	}
	// 正常退出没有信号，不能编一个出来。
	if res.Signal != "" {
		t.Fatalf("正常退出的 Signal 应为空，实际 %q", res.Signal)
	}
}
