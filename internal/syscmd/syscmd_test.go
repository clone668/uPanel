package syscmd

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// 测试用的“外部程序”就是测试二进制自身：用 -test.run 只跑 TestSyscmdHelper，
// 模式通过 -- 之后的参数传入（syscmd 会把环境变量清成白名单，没法用环境变量传参）。
const helperFlag = "-test.run=TestSyscmdHelper"

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}

// TestSyscmdHelper 是辅助进程入口；正常跑测试时立即跳过。
func TestSyscmdHelper(t *testing.T) {
	mode := helperMode()
	if mode == "" {
		t.Skip("不是辅助进程")
	}
	runHelper(mode)
}

func helperMode() string {
	args := os.Args
	for i, a := range args {
		if a == "--" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func runHelper(mode string) {
	switch mode {
	case "exit3":
		os.Exit(3)
	case "exit7-after-output":
		os.Stdout.WriteString("partial stdout\n")
		os.Stderr.WriteString("partial stderr\n")
		os.Exit(7)
	case "echo-args":
		for _, a := range os.Args[helperIndex()+2:] {
			os.Stdout.WriteString("ARG|" + a + "\n")
		}
		os.Exit(0)
	case "echo-env":
		for _, kv := range os.Environ() {
			os.Stdout.WriteString("ENV|" + kv + "\n")
		}
		os.Exit(0)
	case "big-output":
		chunk := []byte(strings.Repeat("x", 4096))
		for i := 0; i < 64; i++ {
			os.Stdout.Write(chunk)
		}
		os.Exit(0)
	case "sleep":
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
	os.Stderr.WriteString("unknown helper mode: " + mode + "\n")
	os.Exit(2)
}

func helperIndex() int {
	for i, a := range os.Args {
		if a == "--" {
			return i
		}
	}
	return -1
}

// helper 返回 (程序路径, 参数)：程序路径是测试二进制自身的绝对路径。
func helper(t *testing.T, mode string, extra ...string) (string, []string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("取测试二进制路径失败: %v", err)
	}
	args := []string{helperFlag, "--", mode}
	args = append(args, extra...)
	return exe, args
}

func TestRunRejectsRelativeAndEmptyProgram(t *testing.T) {
	if _, err := Run(context.Background(), Spec{Program: ""}, Limits{}); !errors.Is(err, ErrEmptyProgram) {
		t.Fatalf("空程序路径应返回 ErrEmptyProgram，实际 %v", err)
	}
	for _, prog := range []string{"ls", "./ls", "bin/uptime"} {
		if _, err := Run(context.Background(), Spec{Program: prog}, Limits{}); !errors.Is(err, ErrProgramNotAbsolute) {
			t.Fatalf("%q 应返回 ErrProgramNotAbsolute，实际 %v", prog, err)
		}
	}
}

// TestRunRejectsCommandStrings 验证“绝对路径形状的一整段命令”不会被当成程序名去执行。
// 这个用例在 Linux 上尤其重要：/bin/sh; rm -rf /tmp/x 在 POSIX 路径规则下是“绝对路径”。
func TestRunRejectsCommandStrings(t *testing.T) {
	for _, prog := range []string{
		"/bin/sh; rm -rf /tmp/x",
		"/bin/sh -c id",
		"/usr/bin/printf\nid",
		"/bin/echo$(id)",
		"/bin/echo`id`",
		"/bin/echo|id",
		"/bin/echo&&id",
		"/bin/echo'id'",
	} {
		if _, err := Run(context.Background(), Spec{Program: prog}, Limits{}); !errors.Is(err, ErrProgramInvalid) {
			t.Fatalf("%q 应返回 ErrProgramInvalid，实际 %v", prog, err)
		}
	}
}

func TestRunCapturesStdoutAndExitCode(t *testing.T) {
	exe, args := helper(t, "exit7-after-output")
	res, err := Run(context.Background(), Spec{Program: exe, Args: args}, Limits{Timeout: 20 * time.Second})
	var exitErr *ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("应返回 *ExitError，实际 %v", err)
	}
	if exitErr.ExitCode != 7 || res.ExitCode != 7 {
		t.Fatalf("退出码 = %d / %d，期望 7", exitErr.ExitCode, res.ExitCode)
	}
	if res.Stdout != "partial stdout\n" {
		t.Fatalf("stdout = %q", res.Stdout)
	}
	if res.Stderr != "partial stderr\n" {
		t.Fatalf("stderr = %q", res.Stderr)
	}
	if res.TimedOut {
		t.Fatal("不应标记为超时")
	}
}

func TestRunPassesArgsVerbatimWithoutShell(t *testing.T) {
	// 这些字符串在 shell 里会被解释；逐项传参时必须原样到达子进程。
	want := []string{"a b", "$(touch /tmp/pwned)", "; rm -rf /", "`id`", "&& echo hi"}
	exe, args := helper(t, "echo-args", want...)
	res, err := Run(context.Background(), Spec{Program: exe, Args: args}, Limits{Timeout: 20 * time.Second})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	var got []string
	for _, line := range strings.Split(strings.TrimRight(res.Stdout, "\n"), "\n") {
		if strings.HasPrefix(line, "ARG|") {
			got = append(got, strings.TrimPrefix(line, "ARG|"))
		}
	}
	if len(got) != len(want) {
		t.Fatalf("参数个数 = %d，期望 %d（实际输出 %q）", len(got), len(want), res.Stdout)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 个参数 = %q，期望 %q", i+1, got[i], want[i])
		}
	}
}

func TestRunUsesFixedEnvironment(t *testing.T) {
	// 宿主上设置一个危险的加载器变量，确认它不会进入子进程。
	t.Setenv("LD_PRELOAD", "/tmp/evil.so")
	t.Setenv("UPANEL_SHOULD_NOT_LEAK", "1")

	exe, args := helper(t, "echo-env")
	res, err := Run(context.Background(), Spec{Program: exe, Args: args}, Limits{Timeout: 20 * time.Second})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if res.Stdout == "" {
		t.Fatal("子进程没有输出环境变量")
	}
	for _, line := range strings.Split(strings.TrimSpace(res.Stdout), "\n") {
		kv := strings.TrimPrefix(line, "ENV|")
		if strings.HasPrefix(kv, "LD_PRELOAD=") || strings.HasPrefix(kv, "UPANEL_SHOULD_NOT_LEAK=") {
			t.Fatalf("父进程环境变量泄漏进子进程: %q", kv)
		}
	}
	for _, want := range []string{"PATH=" + DefaultPath, "LANG=C.UTF-8", "LC_ALL=C.UTF-8"} {
		if !strings.Contains(res.Stdout, "ENV|"+want+"\n") {
			t.Fatalf("缺少白名单变量 %q（实际 %q）", want, res.Stdout)
		}
	}
}

func TestRunTruncatesOutputAtLimit(t *testing.T) {
	exe, args := helper(t, "big-output")
	res, err := Run(context.Background(), Spec{Program: exe, Args: args}, Limits{
		Timeout:   20 * time.Second,
		MaxStdout: 1024,
	})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if !res.StdoutTruncated || !res.Truncated() {
		t.Fatal("应标记输出被截断")
	}
	if len(res.Stdout) != 1024 {
		t.Fatalf("保留长度 = %d，期望 1024", len(res.Stdout))
	}
}

func TestRunTimesOutAndKillsProcess(t *testing.T) {
	exe, args := helper(t, "sleep")
	start := time.Now()
	res, err := Run(context.Background(), Spec{Program: exe, Args: args}, Limits{Timeout: 300 * time.Millisecond})
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("应返回 ErrTimeout，实际 %v", err)
	}
	if !res.TimedOut {
		t.Fatal("Result.TimedOut 应为 true")
	}
	// 必须是“被终止”，而不是等到 30 秒的 sleep 自然结束。
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("超时终止耗时 %s，看起来没有真正杀掉进程", elapsed)
	}
}

func TestRunHonorsParentCancellation(t *testing.T) {
	exe, args := helper(t, "sleep")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Run(ctx, Spec{Program: exe, Args: args}, Limits{Timeout: 20 * time.Second}); err == nil {
		t.Fatal("父 ctx 取消后应返回错误")
	} else if errors.Is(err, ErrTimeout) {
		t.Fatalf("父 ctx 取消不应报成超时: %v", err)
	}
}

func TestLimitsDefaults(t *testing.T) {
	l := Limits{}.withDefaults()
	if l.Timeout != DefaultTimeout || l.MaxStdout != DefaultMaxStdout || l.MaxStderr != DefaultMaxStderr {
		t.Fatalf("默认限额 = %+v", l)
	}
}
