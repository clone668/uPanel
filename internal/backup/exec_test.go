package backup

// restic 执行结果的翻译（P12.1）。
//
// 这一张判定表是真机上撞出来的：24.04 那台虚机上，只读查询的预算用尽、restic 被
// 一起杀掉，而面板上给出的原因原文是
//
//     restic cat config 失败：backup: /usr/bin/restic 以退出状态 -1 结束
//
// 读起来像 restic 崩了。实际上是我们自己收的工——"这次没等到结论"被写成了
// "restic 出了问题"，人会照着这条去查 restic、查仓库、查口令。
//
// 这里不起子进程（那要 Linux 才能测信号），只把 runOutcome 这张表钉住：谁到期、
// 谁取消、谁退出、谁被信号终止，四种情况必须给出四种不同的话。

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// exitErrorStub 造一个 *exec.ExitError：runOutcome 只按类型判断，不读它的内容。
func exitErrorStub() error {
	return &exec.ExitError{}
}

// liveCtx 返回一个不会到期的 ctx。
func liveCtx() context.Context { return context.Background() }

// expiredCtx 返回一个已经到期的 ctx（DeadlineExceeded）。
func expiredCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	return ctx
}

// canceledCtx 返回一个已经被取消的 ctx（Canceled）。
func canceledCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func TestRunOutcomeClassifiesWhoEndedTheCall(t *testing.T) {
	const timeout = 30 * time.Second

	t.Run("restic 自己超时", func(t *testing.T) {
		res, err := runOutcome(liveCtx(), expiredCtx(t), timeout, ExecResult{Program: "/usr/bin/restic"}, exitErrorStub())
		if !errors.Is(err, ErrExecTimeout) {
			t.Fatalf("应为 ErrExecTimeout，实际: %v", err)
		}
		if errors.Is(err, ErrBudgetExhausted) {
			t.Fatalf("restic 自己的超时不该说成上层预算用尽: %v", err)
		}
		if !res.TimedOut {
			t.Fatalf("TimedOut 应为 true")
		}
		if !strings.Contains(err.Error(), timeout.String()) {
			t.Fatalf("错误里要说清上限是多少: %v", err)
		}
	})

	t.Run("上层预算用尽", func(t *testing.T) {
		ctx := expiredCtx(t)
		res, err := runOutcome(ctx, ctx, timeout, ExecResult{Program: "/usr/bin/restic"}, exitErrorStub())
		if !errors.Is(err, ErrBudgetExhausted) {
			t.Fatalf("应为 ErrBudgetExhausted，实际: %v", err)
		}
		if errors.Is(err, ErrExecTimeout) {
			t.Fatalf("上层预算用尽不该说成 restic 跑太久: %v", err)
		}
		if !res.TimedOut {
			t.Fatalf("TimedOut 应为 true")
		}
		// 这句话会原样进 repo_config_error，必须说清"没有结论"，不能指向仓库或口令。
		if !strings.Contains(err.Error(), "没有结论") {
			t.Fatalf("要说清这次没有结论: %v", err)
		}
	})

	t.Run("上层取消", func(t *testing.T) {
		ctx := canceledCtx(t)
		_, err := runOutcome(ctx, ctx, timeout, ExecResult{Program: "/usr/bin/restic"}, exitErrorStub())
		if err == nil {
			t.Fatalf("取消了却当成成功")
		}
		if errors.Is(err, ErrBudgetExhausted) || errors.Is(err, ErrExecTimeout) {
			t.Fatalf("取消不是超时: %v", err)
		}
		if !strings.Contains(err.Error(), "取消") {
			t.Fatalf("要说清是被取消: %v", err)
		}
	})

	t.Run("restic 非零退出", func(t *testing.T) {
		res, err := runOutcome(liveCtx(), liveCtx(), timeout, ExecResult{Program: "/usr/bin/restic", ExitCode: 1}, exitErrorStub())
		var exitErr *ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("应为 *ExitError，实际: %v", err)
		}
		if exitErr.ExitCode != 1 {
			t.Fatalf("退出码应为 1，实际 %d", exitErr.ExitCode)
		}
		if strings.Contains(err.Error(), "信号") {
			t.Fatalf("正常退出不该提信号: %v", err)
		}
		if res.TimedOut {
			t.Fatalf("退出码非零不是超时")
		}
	})

	t.Run("restic 被信号终止", func(t *testing.T) {
		res, err := runOutcome(liveCtx(), liveCtx(), timeout,
			ExecResult{Program: "/usr/bin/restic", ExitCode: -1, Signal: "SIGSEGV"}, exitErrorStub())
		var exitErr *ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("应为 *ExitError，实际: %v", err)
		}
		if exitErr.Signal != "SIGSEGV" {
			t.Fatalf("信号名丢了: %q", exitErr.Signal)
		}
		// 关键：不能说"以退出状态 -1 结束"——那是句什么都没说的话。
		if strings.Contains(err.Error(), "-1") {
			t.Fatalf("不许把 -1 当退出码说出来: %v", err)
		}
		if !strings.Contains(err.Error(), "SIGSEGV") {
			t.Fatalf("要说清是谁停的: %v", err)
		}
		if res.ExitCode != -1 {
			t.Fatalf("ExitCode 应保持 -1")
		}
	})

	t.Run("起不来", func(t *testing.T) {
		_, err := runOutcome(liveCtx(), liveCtx(), timeout, ExecResult{Program: "/usr/bin/restic"},
			&exec.Error{Name: "/usr/bin/restic", Err: errors.New("no such file or directory")})
		if err == nil || !strings.Contains(err.Error(), "启动") {
			t.Fatalf("启动失败要说启动失败: %v", err)
		}
	})

	t.Run("正常退出", func(t *testing.T) {
		res, err := runOutcome(liveCtx(), liveCtx(), timeout, ExecResult{Program: "/usr/bin/restic", Stdout: "{}"}, nil)
		if err != nil {
			t.Fatalf("正常退出却报错: %v", err)
		}
		if res.TimedOut {
			t.Fatalf("正常退出不该标超时")
		}
	})
}

// TestResticFailureSaysTimeInsteadOfWrongPassword：预算用尽的原因要原样呈现，
// 不许套上"restic cat config 失败："——那句话说出去就是在指向仓库和口令。
func TestResticFailureSaysTimeInsteadOfWrongPassword(t *testing.T) {
	f := &resticFailure{op: "cat config", res: ExecResult{Program: "/usr/bin/restic", ExitCode: -1}, err: ErrBudgetExhausted}
	reason := f.FailureReason()
	if reason != ErrBudgetExhausted.Error() {
		t.Fatalf("原因应原样为 %q，实际 %q", ErrBudgetExhausted.Error(), reason)
	}
	if strings.Contains(reason, "口令") || strings.Contains(reason, "cat config 失败") {
		t.Fatalf("不许把这句时间上的话读成口令/仓库问题: %q", reason)
	}
}
