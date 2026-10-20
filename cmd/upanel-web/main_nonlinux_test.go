//go:build !linux

package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestProductionRefusedOnNonLinux 验证 STAGES.md P01 验收条件：
// 非 Linux 上的 production 启动必须明确拒绝，而不是自动退回 mock 或继续启动。
func TestProductionRefusedOnNonLinux(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run([]string{"--mode", "production"}, &out, &errOut)
	if code != 1 {
		t.Fatalf("退出码 = %d，期望 1（明确拒绝启动）", code)
	}
	msg := errOut.String()
	if !strings.Contains(msg, "production 模式仅支持 Linux") {
		t.Fatalf("拒绝原因未写明: %q", msg)
	}
	if !strings.Contains(msg, "--mode mock") {
		t.Fatalf("拒绝原因应给出 mock 提示: %q", msg)
	}
	if strings.Contains(msg, "已开始监听") {
		t.Fatalf("不得在拒绝前启动服务: %q", msg)
	}
}
