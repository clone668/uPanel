package main

// host-caps 子命令的测试（P16.1）。
//
// 这些用例只回答“命令本身还能不能跑”：能不能解析参数、能不能吐 JSON、
// 用法错了会不会乱返 0。**它们不证明任何一台 Ubuntu 上装了什么**——探到的
// 内容取决于跑测试的这台机器，绝大多数开发机上四条固定目录都读不全，
// 于是整份报告是 unknown + partial，这恰恰是设计要的结果。

import (
	"encoding/json"
	"strings"
	"testing"

	"upanel/internal/hostcaps"
)

func TestHostCapsRuns(t *testing.T) {
	code, out, errOut := runCtl("host-caps")
	if code != 0 {
		t.Fatalf("退出码 = %d，stderr = %q", code, errOut)
	}
	if !strings.Contains(out, "状态") || !strings.Contains(out, "标识") {
		t.Fatalf("表头不在输出里: %q", out)
	}
	// 缺席不是错误：每条规格都必须在表里有一行，哪怕是 unknown。
	for _, id := range hostcaps.ItemIDs() {
		if !strings.Contains(out, id) {
			t.Fatalf("输出里没有 %q 这一行:\n%s", id, out)
		}
	}
}

func TestHostCapsJSON(t *testing.T) {
	code, out, errOut := runCtl("host-caps", "--json")
	if code != 0 {
		t.Fatalf("退出码 = %d，stderr = %q", code, errOut)
	}
	var report hostcaps.Report
	dec := json.NewDecoder(strings.NewReader(out))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&report); err != nil {
		t.Fatalf("输出不是合法的报告 JSON: %v (%q)", err, out)
	}
	if report.GeneratedAt == "" {
		t.Fatalf("generated_at 为空: %+v", report)
	}
	if len(report.Items) == 0 {
		t.Fatalf("items 是空的——哪怕全 unknown 也该有 %d 条", len(hostcaps.ItemIDs()))
	}
	if len(report.Items) != len(hostcaps.ItemIDs()) {
		t.Fatalf("items = %d，规格里有 %d 条", len(report.Items), len(hostcaps.ItemIDs()))
	}
	for _, item := range report.Items {
		switch item.State {
		case hostcaps.StatePresent, hostcaps.StateAbsent:
		case hostcaps.StateUnknown:
			if item.Reason == "" {
				t.Fatalf("%s 是 unknown 却没给理由: %+v", item.ID, item)
			}
		default:
			t.Fatalf("%s 的状态不在三态里: %q", item.ID, item.State)
		}
	}
}

func TestHostCapsLine(t *testing.T) {
	code, out, errOut := runCtl("host-caps", "--line")
	if code != 0 {
		t.Fatalf("退出码 = %d，stderr = %q", code, errOut)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != len(hostcaps.ItemIDs()) {
		t.Fatalf("行数 = %d，规格里有 %d 条", len(lines), len(hostcaps.ItemIDs()))
	}
	seen := map[string]bool{}
	for _, line := range lines {
		fields := strings.Split(line, "\t")
		// 六列是契约：改列数就是破坏安装脚本，测试在这里拦住。
		if len(fields) != 6 {
			t.Fatalf("列数 = %d，期望 6：%q", len(fields), line)
		}
		id, state := fields[0], fields[1]
		if !seen[id] {
			seen[id] = true
		} else {
			t.Fatalf("标识 %q 重复出现", id)
		}
		switch state {
		case "present", "absent", "unknown":
		default:
			t.Fatalf("%s 的状态不在三态里: %q", id, state)
		}
		// 版本与路径可以为空，但不许是占位符——空字段的含义是“没有”，
		// 一个 "-" 或 "n/a" 会让脚本把它当成版本号。
		if fields[2] == "-" || fields[2] == "n/a" || fields[3] == "-" {
			t.Fatalf("%s 用了占位符而不是空字段: %q", id, line)
		}
	}
	for _, id := range hostcaps.ItemIDs() {
		if !seen[id] {
			t.Fatalf("输出里没有 %q", id)
		}
	}
	// 没有表头：第一行第一列必须是标识，而不是一个列名。
	if first := strings.Split(lines[0], "\t")[0]; first != hostcaps.ItemIDs()[0] {
		t.Fatalf("第一行第一列是 %q，期望标识 %q（--line 不该有表头）", first, hostcaps.ItemIDs()[0])
	}
}

func TestHostCapsRejectsBothFormats(t *testing.T) {
	code, _, errOut := runCtl("host-caps", "--json", "--line")
	if code != exitUsage {
		t.Fatalf("退出码 = %d，期望 %d", code, exitUsage)
	}
	if !strings.Contains(errOut, "--json") {
		t.Fatalf("说明里没点名两个参数: %q", errOut)
	}
}

func TestHostCapsRejectsUsage(t *testing.T) {
	if code, _, _ := runCtl("host-caps", "多余的位置参数"); code != exitUsage {
		t.Fatalf("退出码 = %d，期望 %d", code, exitUsage)
	}
}

func TestHostCapsRejectsZeroTimeout(t *testing.T) {
	// 0 和负数会让探测一开始就超时，那不是“跑完了”，是用法错误。
	for _, value := range []string{"0", "-1s"} {
		code, _, errOut := runCtl("host-caps", "--timeout", value)
		if code != exitUsage {
			t.Fatalf("--timeout %s 的退出码 = %d，期望 %d", value, code, exitUsage)
		}
		if !strings.Contains(errOut, "timeout") {
			t.Fatalf("--timeout %s 的说明里没提 timeout: %q", value, errOut)
		}
	}
}
