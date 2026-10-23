package syscmd

// 追加环境变量的白名单（P11.2）。
//
// 这个口子存在的唯一理由是 APT 的非交互策略：`DEBIAN_FRONTEND=noninteractive`
// 必须在环境里说。它不是"开放环境变量"——白名单之外的键、以及固定环境里的
// PATH/LANG/LC_ALL，一律拒绝。

import (
	"context"
	"errors"
	"runtime"
	"testing"
)

// absProgram 是一个在本平台绝对路径合法的程序名。
//
// 只需要通过 validateProgram（不检查存在性）：环境校验发生在启动之前，
// 这两条测试断的正是"命令根本没被跑起来"。
func absProgram() string {
	if runtime.GOOS == "windows" {
		return `C:\Windows\System32\where.exe`
	}
	return "/usr/bin/true"
}

// TestValidateEnvRejectsOutsideWhitelist：白名单外的键不接受。
func TestValidateEnvRejectsOutsideWhitelist(t *testing.T) {
	for _, env := range [][]string{
		{"LD_PRELOAD=/tmp/evil.so"},
		{"PATH=/tmp"},
		{"BASH_ENV=/tmp/evil.sh"},
		{"IFS=,"},
		{"debian_frontend=noninteractive"}, // 大小写不对就不是同一个变量
		{"DEBIAN_FRONTEND"},                // 不是 KEY=VALUE
		{"=noninteractive"},                // 空键
		{"", "DEBIAN_FRONTEND=noninteractive"},
	} {
		if err := validateEnv(env); !errors.Is(err, ErrEnvNotAllowed) {
			t.Fatalf("%v 应被拒绝，实际 err=%v", env, err)
		}
	}
}

// TestValidateEnvRejectsFixedOverride：固定环境不许覆盖。
func TestValidateEnvRejectsFixedOverride(t *testing.T) {
	for _, env := range [][]string{
		{"PATH=/usr/bin"},
		{"LANG=en_US.UTF-8"},
		{"LC_ALL=C"},
		{"DEBIAN_FRONTEND=noninteractive", "PATH=/tmp"},
	} {
		if err := validateEnv(env); !errors.Is(err, ErrEnvNotAllowed) {
			t.Fatalf("%v 应被拒绝，实际 err=%v", env, err)
		}
	}
}

// TestValidateEnvRejectsBadValue：键对了、值不在允许集合里，也要拒绝。
func TestValidateEnvRejectsBadValue(t *testing.T) {
	for _, env := range [][]string{
		{"DEBIAN_FRONTEND=text"},
		{"APT_LISTCHANGES_FRONTEND=full"},
		{"NEEDRESTART_MODE=always"},
	} {
		if err := validateEnv(env); !errors.Is(err, ErrEnvNotAllowed) {
			t.Fatalf("%v 应被拒绝，实际 err=%v", env, err)
		}
	}
}

// TestValidateEnvAcceptsAPTNonInteractive：面板真正要用的那三个必须过。
func TestValidateEnvAcceptsAPTNonInteractive(t *testing.T) {
	env := []string{
		"DEBIAN_FRONTEND=noninteractive",
		"APT_LISTCHANGES_FRONTEND=none",
		"NEEDRESTART_MODE=l",
	}
	if err := validateEnv(env); err != nil {
		t.Fatalf("非交互策略被拒: %v", err)
	}
	if err := validateEnv(nil); err != nil {
		t.Fatalf("空环境不该报错: %v", err)
	}
}

// TestRunRejectsBadEnvBeforeStarting：校验发生在启动之前，命令不会被跑起来。
func TestRunRejectsBadEnvBeforeStarting(t *testing.T) {
	res, err := Run(context.Background(), Spec{
		Program: absProgram(),
		Env:     []string{"LD_PRELOAD=/tmp/evil.so"},
	}, Limits{})
	if !errors.Is(err, ErrEnvNotAllowed) {
		t.Fatalf("应报 ErrEnvNotAllowed，实际 %v", err)
	}
	if res.ExitCode != -1 {
		t.Fatalf("命令不该被启动（退出码应为 -1）: %+v", res)
	}
}

// TestStreamStartRejectsBadEnv：流式入口同一套校验。
func TestStreamStartRejectsBadEnv(t *testing.T) {
	_, err := Start(context.Background(), Spec{
		Program: absProgram(),
		Env:     []string{"PATH=/tmp"},
	}, Limits{})
	if !errors.Is(err, ErrEnvNotAllowed) {
		t.Fatalf("应报 ErrEnvNotAllowed，实际 %v", err)
	}
}
