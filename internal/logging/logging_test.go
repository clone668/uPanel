package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestParseLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"":      slog.LevelInfo,
		"info":  slog.LevelInfo,
		"DEBUG": slog.LevelDebug,
		"warn":  slog.LevelWarn,
		"error": slog.LevelError,
	}
	for in, want := range cases {
		got, err := ParseLevel(in)
		if err != nil {
			t.Fatalf("ParseLevel(%q) 失败: %v", in, err)
		}
		if got != want {
			t.Fatalf("ParseLevel(%q) = %v，期望 %v", in, got, want)
		}
	}
	for _, in := range []string{"trace", "critical", "5"} {
		if _, err := ParseLevel(in); err == nil {
			t.Fatalf("ParseLevel(%q) 应当失败", in)
		}
	}
}

func TestParseFormat(t *testing.T) {
	for _, in := range []string{"", "json", "text", "TEXT"} {
		if _, err := ParseFormat(in); err != nil {
			t.Fatalf("ParseFormat(%q) 失败: %v", in, err)
		}
	}
	if _, err := ParseFormat("logfmt"); err == nil {
		t.Fatal("未知格式必须报错，不能静默降级")
	}
}

func TestJSONOutputHasProgramAndLevel(t *testing.T) {
	var buf bytes.Buffer
	logger, err := New(Options{Program: "upanel-web", Level: "debug", Format: "json", Output: &buf})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	logger.Debug("测试消息", "key", "value")

	var rec map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatalf("输出不是单行 JSON: %v (%q)", err, buf.String())
	}
	if rec["program"] != "upanel-web" {
		t.Fatalf("program 字段 = %v", rec["program"])
	}
	if rec["level"] != "DEBUG" {
		t.Fatalf("level = %v", rec["level"])
	}
	if rec["msg"] != "测试消息" {
		t.Fatalf("msg = %v", rec["msg"])
	}
	if rec["key"] != "value" {
		t.Fatalf("附加字段丢失: %v", rec["key"])
	}
}

func TestLevelFiltering(t *testing.T) {
	var buf bytes.Buffer
	logger, err := New(Options{Level: "warn", Format: "text", Output: &buf})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	logger.Info("不应出现")
	if buf.Len() != 0 {
		t.Fatalf("warn 级别下不应输出 info 日志，实际: %q", buf.String())
	}
	logger.Warn("应出现")
	if !strings.Contains(buf.String(), "应出现") {
		t.Fatalf("warn 日志未输出: %q", buf.String())
	}
}

func TestNewRejectsInvalidOptions(t *testing.T) {
	if _, err := New(Options{Level: "verbose"}); err == nil {
		t.Fatal("非法级别必须导致 New 失败")
	}
	if _, err := New(Options{Format: "xml"}); err == nil {
		t.Fatal("非法格式必须导致 New 失败")
	}
}
