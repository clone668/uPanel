package logging

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// newCapturingLogger 构造一个写进 bytes.Buffer 的 logger，用来检查真正写出的字节。
func newCapturingLogger(t *testing.T, format string) (*slog.Logger, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	logger, err := New(Options{Program: "test", Level: "debug", Format: format, Output: &buf})
	if err != nil {
		t.Fatalf("构造 logger 失败: %v", err)
	}
	return logger, &buf
}

func TestRedactsSecretKeys(t *testing.T) {
	logger, buf := newCapturingLogger(t, "json")
	logger.Info("创建账号",
		"username", "alice",
		"password", "alice-horse-9",
		"csrf_token", "csrf-abc",
		"session_hash", "hash-abc",
		"agent_key", "key-abc",
		"api_key", "api-abc",
		"cookie", "__Host-upanel_session=abc",
	)
	out := buf.String()
	for _, secret := range []string{"alice-horse-9", "csrf-abc", "hash-abc", "key-abc", "api-abc", "__Host-upanel_session"} {
		if strings.Contains(out, secret) {
			t.Fatalf("日志里出现秘密 %q：%s", secret, out)
		}
	}
	if !strings.Contains(out, "alice") {
		t.Fatalf("非秘密字段不应被抹掉：%s", out)
	}
	if strings.Count(out, Redacted) < 6 {
		t.Fatalf("期望至少 6 处 [redacted]：%s", out)
	}
}

func TestRedactsSecretValuesInsideText(t *testing.T) {
	logger, buf := newCapturingLogger(t, "json")
	logger.Error("登录失败",
		"error", errors.New("bad request: password=hunter2&user=alice"),
		"raw", "$argon2id$v=19$m=131072,t=3,p=2$c2FsdA$aGFzaA",
	)
	logger.Info("请求带 Authorization 头", "header", "Bearer abc.def.ghi")
	out := buf.String()
	for _, secret := range []string{"hunter2", "c2FsdA", "aGFzaA", "abc.def.ghi"} {
		if strings.Contains(out, secret) {
			t.Fatalf("文本里的秘密未被抹掉 %q：%s", secret, out)
		}
	}
	if !strings.Contains(out, "alice") {
		t.Fatalf("同一段文本里的非秘密部分不应被整段抹掉：%s", out)
	}
}

func TestRedactsNestedGroupsAndWithAttrs(t *testing.T) {
	logger, buf := newCapturingLogger(t, "json")
	logger.With("session", "sess-1", "token", "tok-1").
		WithGroup("request").
		Info("嵌套", slog.Group("auth", "secret", "s3cret", "user", "bob"))
	out := buf.String()
	for _, secret := range []string{"sess-1", "tok-1", "s3cret"} {
		if strings.Contains(out, secret) {
			t.Fatalf("嵌套属性里的秘密未被抹掉 %q：%s", secret, out)
		}
	}
	if !strings.Contains(out, "bob") {
		t.Fatalf("嵌套里的非秘密字段丢失：%s", out)
	}
}

func TestSecretMarkerForcesRedaction(t *testing.T) {
	logger, buf := newCapturingLogger(t, "json")
	logger.Info("提交", "value", Secret("looks-innocent-but-secret"))
	out := buf.String()
	if strings.Contains(out, "looks-innocent-but-secret") {
		t.Fatalf("Secret() 标记的值仍被写出：%s", out)
	}
	if !strings.Contains(out, Redacted) {
		t.Fatalf("缺少 [redacted]：%s", out)
	}
}

func TestTextFormatAlsoRedacted(t *testing.T) {
	logger, buf := newCapturingLogger(t, "text")
	logger.Warn("危险操作", "password", "top-secret", "user", "carol")
	out := buf.String()
	if strings.Contains(out, "top-secret") {
		t.Fatalf("text 格式出现秘密：%s", out)
	}
	if !strings.Contains(out, "carol") {
		t.Fatalf("text 格式丢了非秘密字段：%s", out)
	}
}

// TestKeepsNonSecretMetadataVisible 固定一条工程约定：脱敏按字段名匹配且失败关闭，
// 所以**不含密���语义的字段必须用不撞词的键名**（例如 key_fingerprint 而不是
// secret_key_fingerprint），否则运维在日志里看不到本该可查的路径与指纹。
func TestKeepsNonSecretMetadataVisible(t *testing.T) {
	logger, buf := newCapturingLogger(t, "json")
	logger.Info("启动",
		"key_path", "/etc/upanel/agent.key",
		"key_version", 1,
		"key_fingerprint", "fa82afd431fd1291",
		"ref", "sec_0123456789abcdef0123456789abcdef",
		"purpose", "smb_password",
		"state", "temporary",
	)
	out := buf.String()
	for _, want := range []string{"/etc/upanel/agent.key", "fa82afd431fd1291", "sec_0123456789abcdef0123456789abcdef", "smb_password"} {
		if !strings.Contains(out, want) {
			t.Fatalf("非秘密元数据 %q 被误打码：%s", want, out)
		}
	}
	if strings.Contains(out, Redacted) {
		t.Fatalf("这一行不应出现 [redacted]：%s", out)
	}
}

// TestRedactsSecretRefKey 记录另一面：含 secret 的键名会被整值打码（失败关闭），
// 这正是上面那条约定存在的原因。
func TestRedactsSecretRefKey(t *testing.T) {
	logger, buf := newCapturingLogger(t, "json")
	logger.Info("保存", "secret_ref", "sec_0123456789abcdef0123456789abcdef")
	if !strings.Contains(buf.String(), Redacted) {
		t.Fatalf("secret_ref 键名应被整值打码：%s", buf.String())
	}
}

func TestOutputStaysValidJSON(t *testing.T) {
	logger, buf := newCapturingLogger(t, "json")
	logger.Info("带引号的秘密", "note", `password="a b"&x=1`)
	var payload map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &payload); err != nil {
		t.Fatalf("脱敏后不是合法 JSON: %v（原文 %s）", err, buf.String())
	}
	if strings.Contains(buf.String(), "a b") {
		t.Fatalf("带空格的口令泄漏：%s", buf.String())
	}
}

// TestRedactReportsWhetherItMasked 锁住 Redact 的第二个返回值（P05.3）。
//
// journal 的 DTO 用它在条目上标 redacted。返回值必须诚实：
// 没抹到东西时不能报 true（那会让每条日志都挂着“已过滤”的标记，
// 用户就再也看不出哪几条真的被过滤过），抹到了也不能报 false。
func TestRedactReportsWhetherItMasked(t *testing.T) {
	cases := []struct {
		name       string
		in         string
		wantMasked bool
		wantGone   []string
		wantKept   []string
	}{
		{"空串", "", false, nil, nil},
		{"普通文本", "sshd: 接受来自 10.0.0.5 的连接", false, nil, []string{"10.0.0.5", "sshd"}},
		{"查询串里的口令", "login?user=alice&password=hunter2", true,
			[]string{"hunter2"}, []string{"alice", "login"}},
		{"Authorization 头", "请求带 Authorization: Bearer abc.def.ghi", true,
			[]string{"abc.def.ghi"}, []string{"Authorization"}},
		{"argon2 哈希", "$argon2id$v=19$m=131072,t=3,p=2$c2FsdA$aGFzaA", true,
			[]string{"c2FsdA", "aGFzaA"}, nil},
		{"token 键值形态", "token=eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.abcd", true,
			[]string{"eyJhbGciOiJIUzI1NiJ9"}, []string{"token="}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, masked := Redact(tc.in)
			if masked != tc.wantMasked {
				t.Fatalf("masked = %v，期望 %v（输出 %q）", masked, tc.wantMasked, out)
			}
			for _, secret := range tc.wantGone {
				if strings.Contains(out, secret) {
					t.Fatalf("应抹掉 %q，实际输出 %q", secret, out)
				}
			}
			for _, kept := range tc.wantKept {
				if !strings.Contains(out, kept) {
					t.Fatalf("应保留 %q，实际输出 %q", kept, out)
				}
			}
		})
	}
}
