package main

import (
	"bytes"
	"os"
	"runtime"
	"strings"
	"testing"
)

func TestVersionFlag(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"--version"}, &out, &errOut); code != 0 {
		t.Fatalf("退出码 = %d，stderr = %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "upanel-agent") {
		t.Fatalf("输出 = %q", out.String())
	}
}

func TestUnknownFlag(t *testing.T) {
	var out, errOut bytes.Buffer
	// 代理没有 --mode：演示模式由 Web 提供，传 --mode 必须被当作未知参数拒绝。
	if code := run([]string{"--mode", "mock"}, &out, &errOut); code != 2 {
		t.Fatalf("退出码 = %d，期望 2", code)
	}
}

func TestRefusesToStartWithoutRootOrLinux(t *testing.T) {
	// root 下会真正启动服务并阻塞，因此该用例只在非 root 时执行。
	if runtime.GOOS == "linux" && os.Geteuid() == 0 {
		t.Skip("以 root 运行时不执行启动拒绝用例")
	}
	var out, errOut bytes.Buffer
	code := run(nil, &out, &errOut)
	if code != 1 {
		t.Fatalf("退出码 = %d，期望 1（拒绝启动）", code)
	}
	msg := errOut.String()
	if !strings.Contains(msg, "拒绝启动") {
		t.Fatalf("stderr = %q", msg)
	}
	if strings.Contains(msg, "已开始监听") {
		t.Fatalf("不得在拒绝前启动服务: %q", msg)
	}
}
