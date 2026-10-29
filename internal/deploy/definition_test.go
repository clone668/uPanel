package deploy

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// TestDecodeDefinitionRejectsUnknownFields 是“危险选项无法表达”的第一道闸：
// 结构体里没有的字段必须是明确的 400，不能被静默丢弃。
func TestDecodeDefinitionRejectsUnknownFields(t *testing.T) {
	cases := []string{
		`{"name":"webapp","privileged":true}`,
		`{"name":"webapp","devices":["/dev/sda:/dev/sda"]}`,
		`{"name":"webapp","network_mode":"host"}`,
		`{"name":"webapp","cap_add":["SYS_ADMIN"]}`,
		`{"name":"webapp","pid":"host"}`,
		`{"name":"webapp","compose_yaml":"services: {}"}`,
		`{"name":"webapp","restart":"always"}`,
	}
	for _, body := range cases {
		_, err := DecodeDefinition(strings.NewReader(body))
		if err == nil {
			t.Fatalf("含有未知字段的请求体应当被拒：%s", body)
		}
		var verr *ValidationError
		if !errors.As(err, &verr) {
			t.Fatalf("期望 ValidationError，实际 %T：%v", err, err)
		}
		if verr.Code != CodeUnknownField {
			t.Fatalf("%s 的拒因 = %s，期望 %s", body, verr.Code, CodeUnknownField)
		}
		if verr.Field == "" {
			t.Fatalf("%s 的拒因必须点名是哪个字段", body)
		}
		if !strings.Contains(verr.Message, "闭集") {
			t.Fatalf("拒因要说清为什么不允许加字段：%s", verr.Message)
		}
	}
}

// TestDecodeDefinitionRoundTrip 确认合法请求体原样解出来，值指针的“有无”也保住了。
func TestDecodeDefinitionRoundTrip(t *testing.T) {
	body := `{
		"name": "webapp",
		"image_reference": "nginx:1.27",
		"container_port": 8080,
		"host_bind_address": "127.0.0.1",
		"host_port": 18080,
		"environment": [
			{"name": "APP_MODE", "value": "production"},
			{"name": "EMPTY", "value": ""},
			{"name": "DB_PASSWORD", "secret_ref": "` + testSecretRef + `"}
		],
		"mounts": [
			{"source_root_id": "data", "relative_path": "webapp", "container_path": "/data", "read_only": true}
		],
		"resource_limits": {"memory_limit_bytes": 536870912, "cpu_limit": "0.5"},
		"health_check": {"enabled": true, "kind": "http", "path": "/healthz"},
		"volume_dependencies": ["` + testVolumeID + `"]
	}`
	got, err := DecodeDefinition(strings.NewReader(body))
	if err != nil {
		t.Fatalf("合法请求体被拒：%v", err)
	}
	if got.Name != "webapp" || got.ContainerPort != 8080 || got.HostPort != 18080 {
		t.Fatalf("解出来的定义不对：%+v", got)
	}
	if len(got.Environment) != 3 {
		t.Fatalf("环境变量条数 = %d", len(got.Environment))
	}
	// 显式给空串与“没给”在 JSON 里是两件事，必须能分开。
	if got.Environment[1].Value == nil || *got.Environment[1].Value != "" {
		t.Fatalf("显式的空串被当成了“没给”：%+v", got.Environment[1])
	}
	if got.Environment[2].Value != nil {
		t.Fatalf("没有 value 的条目不该有值指针：%+v", got.Environment[2])
	}
	if _, err := Normalize(got); err != nil {
		t.Fatalf("解出来的定义应当能通过校验：%v", err)
	}
}

// TestDecodeDefinitionRejectsTrailingContent 确认“两个对象拼在一起”这种请求不会被当成
// 其中某一个生效。
func TestDecodeDefinitionRejectsTrailingContent(t *testing.T) {
	_, err := DecodeDefinition(strings.NewReader(`{"name":"a"}{"name":"b"}`))
	expectInvalid(t, err, CodeMalformed, "")
}

// TestDecodeDefinitionRejectsBrokenJSON 确认语法错误是被拒而不是被当成零值定义。
func TestDecodeDefinitionRejectsBrokenJSON(t *testing.T) {
	_, err := DecodeDefinition(strings.NewReader(`{"name":`))
	expectInvalid(t, err, CodeMalformed, "")
}

// TestDefinitionJSONRoundTrip 确认结构体标签与契约字段名一致（前端按这些名字发请求）。
func TestDefinitionJSONRoundTrip(t *testing.T) {
	d := validDefinition()
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	for _, field := range []string{
		`"name"`, `"image_reference"`, `"container_port"`, `"host_bind_address"`, `"host_port"`,
		`"environment"`, `"mounts"`, `"resource_limits"`, `"health_check"`, `"volume_dependencies"`,
	} {
		if !strings.Contains(string(raw), field) {
			t.Fatalf("序列化结果缺少契约字段 %s：%s", field, raw)
		}
	}
	for _, forbidden := range []string{"compose_yaml", "privileged", "devices", "network_mode"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("序列化结果里不该出现 %q：%s", forbidden, raw)
		}
	}
}
