package backup

import (
	"context"
	"strings"
	"testing"
)

// 残留锁不是"预检过期"，也不是"设备被换"：它是上一次 restic 没交完锁就走了。
// 面板是这台机器上唯一碰得到仓库的东西，所以清锁必须由产品自己完成——
// 否则运维看到的就是一句"可以用 unlock 命令"，而那命令他跑不了。

func TestNeedsUnlockOnlyMatchesLockFailures(t *testing.T) {
	cases := []struct {
		name string
		res  ExecResult
		want bool
	}{
		{
			name: "stale lock",
			res:  ExecResult{ExitCode: 1, Stderr: "Fatal: unable to create lock in backend: repository is already locked exclusively by ...\nthe `unlock` command can be used to remove stale locks"},
			want: true,
		},
		{
			name: "already locked",
			res:  ExecResult{ExitCode: 1, Stderr: "Fatal: repository is already locked by PID 1234 on host (UID 0, GID 0)"},
			want: false, // 没有 unlock 提示：可能是另一路正在跑，不该动它
		},
		{
			name: "no space",
			res:  ExecResult{ExitCode: 1, Stderr: "error: node /a/b: no space left on device\nFatal: There were 10 errors"},
			want: false,
		},
		{
			name: "wrong password",
			res:  ExecResult{ExitCode: 1, Stderr: "Fatal: wrong password or no key found"},
			want: false,
		},
	}
	for _, c := range cases {
		if got := needsUnlock(c.res); got != c.want {
			t.Fatalf("%s：needsUnlock = %v，想要 %v", c.name, got, c.want)
		}
	}
}

func TestRestoreFailureKeepsTheRealCause(t *testing.T) {
	stderr := "error: node /srv/data/x.bin: write /proc/self/fd/3/x.bin: no space left on device\n" +
		"error: node /srv/data/y.bin: write /proc/self/fd/3/y.bin: no space left on device\n" +
		"Fatal: There were 10 errors"
	f := &resticFailure{op: "restore", res: ExecResult{ExitCode: 1, Stderr: stderr}, err: &ExitError{Program: "restic", ExitCode: 1}}
	reason := f.FailureReason()
	if !strings.Contains(reason, "There were 10 errors") {
		t.Fatalf("汇总行丢了：%s", reason)
	}
	if !strings.Contains(reason, "no space left on device") {
		t.Fatalf("失败消息必须带上真正的原因（写满磁盘），实际：%s", reason)
	}
	if strings.Contains(reason, "error: node /srv/data/y.bin") {
		t.Fatalf("只补第一条原因，不该把每条错误都堆进来：%s", reason)
	}
}

// 下面这段 stderr 是 2026-10-25 在 24.04（restic 0.16.4）上实地抓到的原文：
// 把快照恢复进 64 KiB / 8 inode 的 tmpfs，让它真写不下去。`restic restore` 的逐条
// 错误是 `ignoring error for <path>: <err>`，**不是** `error:`——上一版只认后者，
// 于是真机上恢复写满的消息只剩一句 `Fatal: There were 10 errors`，验收直接判红。
// 这条用例守的就是"产品自己写的格式化也认得真实形状"。
func TestRestoreFailureReadsIgnoringErrorLines(t *testing.T) {
	stderr := "ignoring error for /var/tmp/xprobe/src/d1/f2: write /var/tmp/xprobe/full/var/tmp/xprobe/src/d1/f2: no space left on device\n" +
		"ignoring error for /var/tmp/xprobe/src/d1/f1: open /var/tmp/xprobe/full/var/tmp/xprobe/src/d1/f1: no space left on device\n" +
		"ignoring error for /var/tmp/xprobe/src/d2/g3: UtimesNano: no such file or directory\n" +
		"Fatal: There were 11 errors\n"
	f := &resticFailure{op: "restore", res: ExecResult{ExitCode: 1, Stderr: stderr}, err: &ExitError{Program: "restic", ExitCode: 1}}
	reason := f.FailureReason()
	if !strings.Contains(reason, "There were 11 errors") {
		t.Fatalf("汇总行丢了：%s", reason)
	}
	if !strings.Contains(reason, "no space left on device") {
		t.Fatalf("失败消息必须带上真正的原因（写满磁盘），实际：%s", reason)
	}
	if strings.Contains(reason, "UtimesNano") {
		t.Fatalf("只补第一条原因，不该把后面的错误堆进来：%s", reason)
	}
}

// 没有可识别的错误行时，退还 restic 写下的第一条非汇总行——仍然是它自己的话，
// 比只剩一个错误计数有用；空 stderr 才保持原样。
func TestFirstErrorLineFallsBackToFirstRealLine(t *testing.T) {
	if got := firstErrorLine("\n  reading index files  \nFatal: unable to load indexes\n"); got != "reading index files" {
		t.Fatalf("退回第一条非汇总行失败：%q", got)
	}
	if got := firstErrorLine("Fatal: only a summary\n\n"); got != "" {
		t.Fatalf("只有汇总行时应返回空，实际：%q", got)
	}
}

func TestCheckRetriesOnceAfterStaleLock(t *testing.T) {
	var ops []string
	calls := 0
	r := NewRestic(Paths{Restic: "/usr/bin/restic"}, func(_ context.Context, spec ExecSpec) (ExecResult, error) {
		sub := spec.Args[len(spec.Args)-1]
		ops = append(ops, sub)
		switch sub {
		case "unlock":
			return ExecResult{Program: spec.Program}, nil
		case "check":
			calls++
			if calls == 1 {
				return ExecResult{Program: spec.Program, ExitCode: 1,
						Stderr: "Fatal: unable to create lock in backend: repository is already locked exclusively\n" +
							"the `unlock` command can be used to remove stale locks"},
					&ExitError{Program: spec.Program, ExitCode: 1}
			}
			return ExecResult{Program: spec.Program}, nil
		}
		t.Fatalf("意外的子命令 %q", sub)
		return ExecResult{}, nil
	})
	if err := r.Check(context.Background(), Invocation{Repository: "/srv/repo", Password: "secret"}); err != nil {
		t.Fatalf("清掉残留锁后 check 应该成功：%v", err)
	}
	if got := strings.Join(ops, ","); got != "check,unlock,check" {
		t.Fatalf("调用顺序 = %s，想要 check,unlock,check", got)
	}
}

func TestCheckReportsFailureWithoutRetryWhenUnlockCannotHelp(t *testing.T) {
	var ops []string
	r := NewRestic(Paths{Restic: "/usr/bin/restic"}, func(_ context.Context, spec ExecSpec) (ExecResult, error) {
		ops = append(ops, spec.Args[len(spec.Args)-1])
		return ExecResult{Program: spec.Program, ExitCode: 1, Stderr: "Fatal: wrong password or no key found"},
			&ExitError{Program: spec.Program, ExitCode: 1}
	})
	err := r.Check(context.Background(), Invocation{Repository: "/srv/repo", Password: "secret"})
	if err == nil {
		t.Fatal("口令错误必须原样报出来")
	}
	if !strings.Contains(err.Error(), "口令不正确") {
		t.Fatalf("口令错误必须被翻成人话，实际：%v", err)
	}
	if got := strings.Join(ops, ","); got != "check" {
		t.Fatalf("不该重试，实际调用：%s", got)
	}
}

func TestForgetPruneRetriesOnceAfterStaleLock(t *testing.T) {
	const id = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	var ops []string
	calls := 0
	r := NewRestic(Paths{Restic: "/usr/bin/restic"}, func(_ context.Context, spec ExecSpec) (ExecResult, error) {
		forget := false
		for _, a := range spec.Args {
			if a == "forget" {
				forget = true
			}
		}
		if !forget {
			ops = append(ops, "unlock")
			return ExecResult{Program: spec.Program}, nil
		}
		calls++
		ops = append(ops, "forget")
		if calls == 1 {
			return ExecResult{Program: spec.Program, ExitCode: 1,
					Stderr: "Fatal: unable to create lock in backend: repository is already locked exclusively\n" +
						"the `unlock` command can be used to remove stale locks"},
				&ExitError{Program: spec.Program, ExitCode: 1}
		}
		return ExecResult{Program: spec.Program}, nil
	})
	if err := r.ForgetPrune(context.Background(), Invocation{Repository: "/srv/repo", Password: "secret"}, []string{id}); err != nil {
		t.Fatalf("清掉残留锁后 forget --prune 应该成功：%v", err)
	}
	if got := strings.Join(ops, ","); got != "forget,unlock,forget" {
		t.Fatalf("调用顺序 = %s，想要 forget,unlock,forget", got)
	}
}
