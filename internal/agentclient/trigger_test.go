package agentclient

// 定时触发的客户端（P12.4）。
//
// 这里只验协议那一层：路径、请求体、信封、以及"没有 operation_id 就算失败"。
// 真实配对（客户端 ↔ 代理处理器）在 internal/agent 的 e2e 测试里。

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestTriggerScheduleSendsTriggerID(t *testing.T) {
	var gotBody string
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("方法 = %s", r.Method)
		}
		if want := "/internal/v1/schedules/sch_0123456789abcdef0123456789abcdef/trigger"; r.URL.Path != want {
			t.Errorf("路径 = %s，期望 %s", r.URL.Path, want)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q", ct)
		}
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		io.WriteString(w, `{"data":{"schedule_id":"sch_0123456789abcdef0123456789abcdef",
			"trigger_id":"trg_0123456789abcdef0123456789abcdef",
			"operation_id":"op_0123456789abcdef0123456789abcdef",
			"slot_at":"2026-10-25T03:30:00Z","action":"backup.run",
			"outcome":"triggered","duplicate":false},"request_id":"rid"}`)
	}))

	out, err := c.TriggerSchedule(context.Background(), TriggerInput{
		ScheduleID: "sch_0123456789abcdef0123456789abcdef",
		TriggerID:  "trg_0123456789abcdef0123456789abcdef",
	})
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	// 请求体只有触发 ID：调用方决定不了动作，也决定不了参数（CONTRACTS §15）。
	var body map[string]any
	if err := json.Unmarshal([]byte(gotBody), &body); err != nil {
		t.Fatalf("请求体不是 JSON: %v（%s）", err, gotBody)
	}
	if len(body) != 1 || body["trigger_id"] != "trg_0123456789abcdef0123456789abcdef" {
		t.Fatalf("请求体 = %s", gotBody)
	}
	if out.OperationID != "op_0123456789abcdef0123456789abcdef" || out.Action != "backup.run" {
		t.Fatalf("响应 = %+v", out)
	}
	if out.Duplicate {
		t.Fatal("首次触发不该是重放")
	}
}

func TestTriggerScheduleReplayFlag(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 重放是 200 + duplicate=true，不是错误码：它是"这个槽跑过了"，不是失败。
		io.WriteString(w, `{"data":{"schedule_id":"sch_0123456789abcdef0123456789abcdef",
			"trigger_id":"trg_0123456789abcdef0123456789abcdef",
			"operation_id":"op_0123456789abcdef0123456789abcdef",
			"slot_at":"2026-10-25T03:30:00Z","action":"backup.run",
			"outcome":"duplicate","duplicate":true},"request_id":"rid"}`)
	}))
	out, err := c.TriggerSchedule(context.Background(), TriggerInput{
		ScheduleID: "sch_0123456789abcdef0123456789abcdef",
		TriggerID:  "trg_0123456789abcdef0123456789abcdef",
	})
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if !out.Duplicate || out.Outcome != "duplicate" {
		t.Fatalf("响应 = %+v", out)
	}
}

func TestTriggerScheduleErrors(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch strings.TrimPrefix(r.URL.Path, "/internal/v1/schedules/") {
		case "missing/trigger":
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":{"code":"VALIDATION_ERROR","message":"触发 ID 与当前时间槽不符"},"request_id":"rid"}`)
		default:
			// 只有 schedule_id、没有 operation_id：不能被当成成功。
			io.WriteString(w, `{"data":{"schedule_id":"sch_0123456789abcdef0123456789abcdef"},"request_id":"rid"}`)
		}
	}))

	_, err := c.TriggerSchedule(context.Background(), TriggerInput{
		ScheduleID: "badshape",
		TriggerID:  "trg_0123456789abcdef0123456789abcdef",
	})
	var unavailable *UnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("缺 operation_id 应当是不可用错误（%T）", err)
	}

	// 业务拒绝是 *Error，不是"代理不可用"：调用方要能区分"重试"与"别重试"。
	_, err = c.TriggerSchedule(context.Background(), TriggerInput{
		ScheduleID: "missing",
		TriggerID:  "trg_0123456789abcdef0123456789abcdef",
	})
	var agentErr *Error
	if !errors.As(err, &agentErr) {
		t.Fatalf("业务拒绝应当是 *Error（%T）", err)
	}
	if agentErr.Code != "VALIDATION_ERROR" {
		t.Fatalf("错误码 = %q", agentErr.Code)
	}
}

func TestTriggerScheduleRequiresIDs(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("参数不全时不该发请求")
	}))
	if _, err := c.TriggerSchedule(context.Background(), TriggerInput{TriggerID: "trg_x"}); err == nil {
		t.Fatal("缺 schedule_id 应当报错")
	}
	if _, err := c.TriggerSchedule(context.Background(), TriggerInput{ScheduleID: "sch_x"}); err == nil {
		t.Fatal("缺 trigger_id 应当报错")
	}
}

// TestTriggerScheduleCarriesMaxRuntime：等待上限随触发答复一起来。
//
// 这条字段不是装饰：root 读不到计划（那是 Web 账号的路由），--wait 要知道"等多久"
// 只能由答复带。缺了它就只能退回兜底上限，单元的超时与这里的等待就对不上了。
func TestTriggerScheduleCarriesMaxRuntime(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":{"schedule_id":"sch_0123456789abcdef0123456789abcdef",
			"trigger_id":"trg_0123456789abcdef0123456789abcdef",
			"operation_id":"op_0123456789abcdef0123456789abcdef",
			"slot_at":"2026-10-25T03:30:00Z","action":"script.run",
			"outcome":"triggered","max_runtime_seconds":120,"duplicate":false},"request_id":"rid"}`)
	}))
	out, err := c.TriggerSchedule(context.Background(), TriggerInput{
		ScheduleID: "sch_0123456789abcdef0123456789abcdef",
		TriggerID:  "trg_0123456789abcdef0123456789abcdef",
	})
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if out.MaxRuntimeSeconds != 120 {
		t.Fatalf("max_runtime_seconds = %d，期望 120", out.MaxRuntimeSeconds)
	}
}

// TestGetTriggerRunUsesScheduleScopedPath：状态读走 root-only 的那条形状。
//
// 不能是 /internal/v1/operations/<id>：那是 Web 账号的路由，root 的定时单元调它
// 会被 gate 拒成 403（P12.4 在 24.04 上的真缺陷）。
func TestGetTriggerRunUsesScheduleScopedPath(t *testing.T) {
	const schID = "sch_0123456789abcdef0123456789abcdef"
	const opID = "op_0123456789abcdef0123456789abcdef"
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("方法 = %s", r.Method)
		}
		want := "/internal/v1/schedules/" + schID + "/trigger-runs/" + opID
		if r.URL.Path != want {
			t.Errorf("路径 = %s，期望 %s", r.URL.Path, want)
		}
		io.WriteString(w, `{"data":{"operation_id":"`+opID+`","action":"script.run",
			"stage":"running","step":"执行中","cancellable":false,
			"created_at":"2026-10-25T03:30:00Z","updated_at":"2026-10-25T03:30:10Z"},
			"request_id":"rid"}`)
	}))
	op, err := c.GetTriggerRun(context.Background(), schID, opID)
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if op.Stage != "running" {
		t.Fatalf("stage = %q", op.Stage)
	}
}

func TestGetTriggerRunRequiresIDs(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("参数不全时不该发请求")
	}))
	if _, err := c.GetTriggerRun(context.Background(), "", "op_0123456789abcdef0123456789abcdef"); err == nil {
		t.Fatal("缺 schedule_id 应当报错")
	}
	if _, err := c.GetTriggerRun(context.Background(), "sch_0123456789abcdef0123456789abcdef", ""); err == nil {
		t.Fatal("缺 operation_id 应当报错")
	}
}
