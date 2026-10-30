package agentclient

// 计划任务与受管理脚本的读取端（P12.4）。
//
// 这里只验协议那一层：路径、信封、时间解码、以及"空 id 不发请求"。
// 真实配对（客户端 ↔ 代理处理器）在 internal/agent 的 e2e 测试里。

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

const (
	wireScheduleID = "sch_0123456789abcdef0123456789abcdef"
	wireScriptID   = "scr_0123456789abcdef0123456789abcdef"
)

func TestListSchedulesDecodesFactsAndIntent(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("方法 = %s", r.Method)
		}
		if want := "/internal/v1/schedules"; r.URL.Path != want {
			t.Errorf("路径 = %s，期望 %s", r.URL.Path, want)
		}
		io.WriteString(w, `{"data":{"schedules":[{
			"id":"`+wireScheduleID+`","name":"夜间根文件","kind":"backup",
			"target_id":"pol_0123456789abcdef0123456789abcdef",
			"definition":{"name":"夜间根文件","kind":"backup",
				"target_id":"pol_0123456789abcdef0123456789abcdef",
				"calendar_expression":"*-*-* 03:30:00","timezone":"Asia/Shanghai",
				"persistent":true,"max_runtime_seconds":3600,"enabled":true},
			"enabled":true,"version":4,"timer_ref":"upanel-sch_x.timer",
			"created_at":"2026-10-24T01:00:00Z","updated_at":"2026-10-24T02:00:00Z",
			"installed":true,"unit_file_state":"enabled","active_state":"active",
			"next_elapse":"2026-10-25T19:30:00Z",
			"blockers":[{"code":"SCHEDULE_TARGET_MISSING","message":"备份策略不在代理里"}]}]},
			"request_id":"rid"}`)
	}))

	list, err := c.ListSchedules(context.Background())
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("长度 = %d", len(list))
	}
	got := list[0]
	if got.ID != wireScheduleID || got.Kind != "backup" {
		t.Fatalf("内容不对：%+v", got)
	}
	// 事实与意图都要在：unit_file_state 是 systemd 的原话。
	if !got.Installed || got.UnitFileState != "enabled" || got.ActiveState != "active" {
		t.Fatalf("systemd 事实丢了：%+v", got)
	}
	if !got.Enabled || !got.Def.Enabled {
		t.Fatalf("意图丢了：%+v", got)
	}
	want := time.Date(2026, 10, 25, 19, 30, 0, 0, time.UTC)
	if !got.NextElapse.Equal(want) {
		t.Fatalf("下次触发时刻 = %s，期望 %s", got.NextElapse, want)
	}
	if len(got.Blockers) != 1 || got.Blockers[0].Code != "SCHEDULE_TARGET_MISSING" {
		t.Fatalf("阻塞项 = %+v", got.Blockers)
	}
}

func TestGetScheduleDecodesTriggers(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if want := "/internal/v1/schedules/" + wireScheduleID; r.URL.Path != want {
			t.Errorf("路径 = %s，期望 %s", r.URL.Path, want)
		}
		io.WriteString(w, `{"data":{"schedule":{"id":"`+wireScheduleID+`","name":"夜间",
			"kind":"user_script","target_id":"`+wireScriptID+`",
			"definition":{"name":"夜间","kind":"user_script","target_id":"`+wireScriptID+`",
				"calendar_expression":"*-*-* 04:00:00","timezone":"UTC",
				"persistent":false,"max_runtime_seconds":300,"enabled":false},
			"enabled":false,"version":1,"timer_ref":"","created_at":"2026-10-24T01:00:00Z",
			"updated_at":"2026-10-24T01:00:00Z","installed":false,
			"unit_file_state":"","active_state":"","blockers":[]},
			"triggers":[{"trigger_id":"trg_0123456789abcdef0123456789abcdef",
				"slot_at":"2026-10-24T20:00:00Z","action":"script.run",
				"operation_id":"op_0123456789abcdef0123456789abcdef",
				"outcome":"blocked","reason":"脚本内容哈希与已批准的不一致",
				"created_at":"2026-10-24T20:00:01Z"}]},"request_id":"rid"}`)
	}))

	detail, err := c.GetSchedule(context.Background(), wireScheduleID)
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if detail.Schedule.ID != wireScheduleID || detail.Schedule.Enabled {
		t.Fatalf("计划 = %+v", detail.Schedule)
	}
	if detail.Schedule.NextElapse.IsZero() == false {
		t.Fatalf("没有 next_elapse 时应当是零值，实际 %s", detail.Schedule.NextElapse)
	}
	if len(detail.Triggers) != 1 {
		t.Fatalf("触发记录长度 = %d", len(detail.Triggers))
	}
	tr := detail.Triggers[0]
	if tr.Outcome != "blocked" || tr.Reason == "" || tr.Action != "script.run" {
		t.Fatalf("触发记录 = %+v", tr)
	}
}

func TestGetScheduleRejectsEmptyID(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("空 id 不该发请求")
	}))
	if _, err := c.GetSchedule(context.Background(), ""); err == nil {
		t.Fatal("空 id 应当报错")
	}
}

func TestListScriptsDecodesMetadataOnly(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if want := "/internal/v1/scripts"; r.URL.Path != want {
			t.Errorf("路径 = %s，期望 %s", r.URL.Path, want)
		}
		io.WriteString(w, `{"data":{"scripts":[{"id":"`+wireScriptID+`","name":"清理",
			"content_hash":"sha256:abc","interpreter":"/bin/sh","version":2,
			"created_at":"2026-10-24T01:00:00Z","updated_at":"2026-10-24T02:00:00Z"}]},
			"request_id":"rid"}`)
	}))
	list, err := c.ListScripts(context.Background())
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if len(list) != 1 || list[0].ID != wireScriptID {
		t.Fatalf("列表 = %+v", list)
	}
	if list[0].Content != "" {
		t.Fatalf("列表不该带正文，实际 %q", list[0].Content)
	}
	if list[0].Interpreter != "/bin/sh" {
		t.Fatalf("解释器 = %q", list[0].Interpreter)
	}
}

func TestGetScriptHasContent(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if want := "/internal/v1/scripts/" + wireScriptID; r.URL.Path != want {
			t.Errorf("路径 = %s，期望 %s", r.URL.Path, want)
		}
		io.WriteString(w, `{"data":{"script":{"id":"`+wireScriptID+`","name":"清理",
			"content_hash":"sha256:abc","interpreter":"/bin/sh","version":2,
			"created_at":"2026-10-24T01:00:00Z","updated_at":"2026-10-24T02:00:00Z",
			"content":"#!/bin/sh\necho hi\n"}},"request_id":"rid"}`)
	}))
	got, err := c.GetScript(context.Background(), wireScriptID)
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if got.Content != "#!/bin/sh\necho hi\n" {
		t.Fatalf("正文 = %q", got.Content)
	}
}

func TestGetScriptRejectsEmptyID(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("空 id 不该发请求")
	}))
	if _, err := c.GetScript(context.Background(), ""); err == nil {
		t.Fatal("空 id 应当报错")
	}
}

// TestScheduleReadsUnavailableOnTransportError：连不上就是 UnavailableError，
// 调用方才好把它翻成 503，而不是当成"没有计划"。
func TestScheduleReadsUnavailableOnTransportError(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("需要一个可劫持的连接")
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Fatalf("劫持失败: %v", err)
		}
		conn.Close()
	}))
	if _, err := c.ListSchedules(context.Background()); err == nil {
		t.Fatal("连接断开应当报错")
	} else {
		var unavailable *UnavailableError
		if !errors.As(err, &unavailable) {
			t.Fatalf("错误类型 = %T，期望 *UnavailableError（%v）", err, err)
		}
	}
}

// TestScheduleReadsBadJSONIsUnavailable：代理返回不是 JSON 时也算不可用。
//
// 这一条挡的是"把一次协议破裂读成空列表"：空列表会让界面显示"没有定时任务"，
// 而实际情况是这一轮读失败了。
func TestScheduleReadsBadJSONIsUnavailable(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":"这不是计划列表","request_id":"rid"}`)
	}))
	if _, err := c.ListSchedules(context.Background()); err == nil {
		t.Fatal("非法信封应当报错")
	} else {
		var unavailable *UnavailableError
		if !errors.As(err, &unavailable) {
			t.Fatalf("错误类型 = %T，期望 *UnavailableError（%v）", err, err)
		}
	}
}

// TestListScheduledOperationsQuery：actor 固定 scheduler，since/limit 照传。
func TestListScheduledOperationsQuery(t *testing.T) {
	var gotQuery string
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/v1/operations" {
			t.Errorf("路径 = %s", r.URL.Path)
		}
		gotQuery = r.URL.RawQuery
		io.WriteString(w, `{"data":{"operations":[{
			"operation_id":"op_0123456789abcdef0123456789abcdef","action":"backup.run",
			"preflight_ref":"pf_0123456789abcdef0123456789abcdef",
			"params":{"backup_policy_id":"pol_0123456789abcdef0123456789abcdef","version":4},
			"params_hash":"`+strings.Repeat("c", 64)+`","state_version":"4",
			"target":{"display_name":"夜间根文件"},
			"session_hash":"`+strings.Repeat("d", 64)+`",
			"stage":"succeeded","step":"done","result":{"outcome":"verified"},"error":{},
			"created_at":"2026-10-23T19:30:00Z","started_at":"2026-10-23T19:30:01Z",
			"finished_at":"2026-10-23T19:33:00Z","updated_at":"2026-10-23T19:33:00Z"}]},
			"request_id":"rid"}`)
	}))

	since := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	list, err := c.ListScheduledOperations(context.Background(), since, "op_0123456789abcdef0123456789abcdef", 25)
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if !strings.Contains(gotQuery, "actor=scheduler") {
		t.Fatalf("查询串必须有 actor=scheduler：%s", gotQuery)
	}
	if !strings.Contains(gotQuery, "since=2026-10-22T00%3A00%3A00Z") {
		t.Fatalf("since 没传对：%s", gotQuery)
	}
	if !strings.Contains(gotQuery, "since_id=op_0123456789abcdef0123456789abcdef") {
		t.Fatalf("复合游标 ID 没传对：%s", gotQuery)
	}
	if !strings.Contains(gotQuery, "limit=25") {
		t.Fatalf("limit 没传对：%s", gotQuery)
	}
	if len(list) != 1 {
		t.Fatalf("长度 = %d", len(list))
	}
	got := list[0]
	if got.OperationID == "" || got.PreflightRef == "" || got.SessionHash == "" {
		t.Fatalf("字段不全：%+v", got)
	}
	if got.Stage != "succeeded" || !got.FinishedAt.Equal(time.Date(2026, 10, 23, 19, 33, 0, 0, time.UTC)) {
		t.Fatalf("阶段/时间不对：%+v", got)
	}
}

// TestListScheduledOperationsOmitsZeroSince：水位为空时不发 since（代理从最早一条开始）。
func TestListScheduledOperationsOmitsZeroSince(t *testing.T) {
	var gotQuery string
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		io.WriteString(w, `{"data":{"operations":[]},"request_id":"rid"}`)
	}))
	if _, err := c.ListScheduledOperations(context.Background(), time.Time{}, "", 0); err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if strings.Contains(gotQuery, "since=") || strings.Contains(gotQuery, "limit=") {
		t.Fatalf("零值不该出现在查询串里：%s", gotQuery)
	}
}
