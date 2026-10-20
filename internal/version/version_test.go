package version

import (
	"runtime"
	"strings"
	"testing"
)

func TestGetReportsRuntimeGoVersion(t *testing.T) {
	info := Get()
	if info.GoVersion != runtime.Version() {
		t.Fatalf("GoVersion = %q, 期望 %q", info.GoVersion, runtime.Version())
	}
	if info.Version == "" {
		t.Fatal("Version 不能为空：未注入时也必须有开发占位值")
	}
	if info.Commit == "" || info.BuildDate == "" {
		t.Fatalf("Commit/BuildDate 不能为空，当前为 %q / %q", info.Commit, info.BuildDate)
	}
}

func TestStringContainsAllFields(t *testing.T) {
	info := Info{Version: "0.1.0", Commit: "abc1234", BuildDate: "2026-10-20T00:00:00Z", GoVersion: runtime.Version()}
	got := info.String()
	for _, want := range []string{"0.1.0", "abc1234", "2026-10-20T00:00:00Z", runtime.Version()} {
		if !strings.Contains(got, want) {
			t.Fatalf("String() = %q，缺少 %q", got, want)
		}
	}
}

func TestDefaultIsDevPlaceholder(t *testing.T) {
	// 未通过 -ldflags 注入时，版本必须是明确的开发占位值，不能冒充正式发布。
	if Version != "0.0.0-dev" {
		t.Skipf("本测试运行在已注入版本的构建上：%s", Version)
	}
	if !strings.Contains(Get().String(), "0.0.0-dev") {
		t.Fatal("开发占位版本未出现在版本字符串中")
	}
}
