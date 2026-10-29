package api

// 计划任务与受管理脚本的只读接口（P12.4）。
//
// 断言的重点不是"能返回 200"，而是六件事：
//  1. 权限：整组 admin——viewer 连列表都拿不到，未登录 401；
//  2. 没有读取端时回 503，**不是**空列表——空列表会被读成"这台机器上没有定时任务"；
//  3. 意图与事实分开：enabled 是定义里的意图，installed/unit_file_state/active_state
//     是 systemd 的原话，两者不合成一句"已启用"；
//  4. id 形状不对是 400（参数不对），代理说没有才是 404（这条真的不在）；
//  5. blockers 永不为 null，[] 表示"现在没有阻塞项"；
//  6. 脚本列表不带正文、详情才带；正文不能出现在列表响应里。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"upanel/internal/agentclient"
	"upanel/internal/certificates"
	"upanel/internal/config"
	"upanel/internal/httpx"
	"upanel/internal/platform"
	"upanel/internal/schedules"
	"upanel/internal/scripts"
	"upanel/internal/version"
)

const (
	testScheduleID4 = "sch_0123456789abcdef0123456789abcdef"
	testScriptID4   = "scr_0123456789abcdef0123456789abcdef"
	testTriggerID4  = "trg_0123456789abcdef0123456789abcdef"
)

// fakeSchedulesReader 是 SchedulesReader 的测试替身：只回放摆好的结果。
type fakeSchedulesReader struct {
	list    []agentclient.Schedule
	listErr error

	detail    agentclient.ScheduleDetail
	detailErr error
	gotID     string
}

func (f *fakeSchedulesReader) ListSchedules(ctx context.Context) ([]agentclient.Schedule, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.list, nil
}

func (f *fakeSchedulesReader) GetSchedule(ctx context.Context, id string) (agentclient.ScheduleDetail, error) {
	f.gotID = id
	if f.detailErr != nil {
		return agentclient.ScheduleDetail{}, f.detailErr
	}
	return f.detail, nil
}

// fakeScriptsReader 是 ScriptsReader 的测试替身。
type fakeScriptsReader struct {
	list    []agentclient.Script
	listErr error

	one    agentclient.Script
	getErr error
	gotID  string
}

func (f *fakeScriptsReader) ListScripts(ctx context.Context) ([]agentclient.Script, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.list, nil
}

func (f *fakeScriptsReader) GetScript(ctx context.Context, id string) (agentclient.Script, error) {
	f.gotID = id
	if f.getErr != nil {
		return agentclient.Script{}, f.getErr
	}
	return f.one, nil
}

// newScheduleFixture 组装一个带两个读取端的服务。
func newScheduleFixture(t *testing.T, sch SchedulesReader, scr ScriptsReader) *adminFixture {
	t.Helper()
	authSvc, aud, db := newTestServicesWithDB(t)
	srv, err := New(Options{
		ListenAddress:     config.ListenProduction,
		Mode:              platform.ModeProduction,
		Version:           version.Info{Version: "test", GoVersion: "test"},
		Logger:            quietLogger(),
		ShutdownTimeout:   time.Second,
		Auth:              authSvc,
		Audit:             aud,
		Operations:        newTestOperations(t, db, nil),
		Apps:              newTestApps(t, db),
		Proxies:           newTestProxies(t, db),
		Certificates:      newTestCertificates(t, db),
		CertificateStages: certificates.NewStageStore(nil),
		AllowedOrigins:    testOrigins(),
		Schedules:         sch,
		Scripts:           scr,
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	seedTestUser(t, srv.opts.Auth, testUser, testUserPass)
	cookie, csrf := loginOK(t, srv)
	return &adminFixture{srv: srv, cookie: cookie, csrf: csrf, db: db}
}

// sampleSchedule 是一条启用了意图、但单元没装的计划。
//
// "意图启用、单元未安装"是这里最该覆盖的状态：它正好是面板最容易被写错的地方
// （把 enabled 直接翻译成"在跑"）。
func sampleSchedule() agentclient.Schedule {
	return agentclient.Schedule{
		ID:       testScheduleID4,
		Name:     "夜间根文件",
		Kind:     string(schedules.KindBackup),
		TargetID: testPolicyID,
		Def: schedules.Definition{
			Name:               "夜间根文件",
			Kind:               schedules.KindBackup,
			TargetID:           testPolicyID,
			CalendarExpression: "*-*-* 03:30:00",
			Timezone:           "Asia/Shanghai",
			Persistent:         true,
			MaxRuntimeSeconds:  3600,
			Enabled:            true,
		},
		Enabled:       true,
		Version:       4,
		TimerRef:      "upanel-sch_" + strings.Repeat("0", 32) + ".timer",
		CreatedAt:     time.Date(2026, 10, 24, 1, 0, 0, 0, time.UTC),
		UpdatedAt:     time.Date(2026, 10, 24, 2, 0, 0, 0, time.UTC),
		Installed:     false,
		UnitFileState: "",
		ActiveState:   "",
		Blockers: []agentclient.Notice{{
			Code:    "SCHEDULE_UNIT_MISSING",
			Message: "systemd 上没有这套单元：保存计划后要执行一次 schedule.apply",
		}},
	}
}

// TestSchedulesRequireAdmin：整组 admin，未登录 401、viewer 403。
func TestSchedulesRequireAdmin(t *testing.T) {
	f := newScheduleFixture(t, &fakeSchedulesReader{list: []agentclient.Schedule{sampleSchedule()}},
		&fakeScriptsReader{list: []agentclient.Script{}})

	for _, path := range []string{
		"/api/v1/schedules",
		"/api/v1/schedules/" + testScheduleID4,
		"/api/v1/scripts",
		"/api/v1/scripts/" + testScriptID4,
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		f.srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("未登录 %s 状态码 = %d，期望 401", path, rec.Code)
		}
	}

	f.createUser(t, "alice", "viewer", "alice-horse-9")
	viewerCookie, viewerCSRF := loginAsUser(t, f.srv, "alice", "alice-horse-9")
	for _, path := range []string{
		"/api/v1/schedules",
		"/api/v1/schedules/" + testScheduleID4,
		"/api/v1/scripts",
		"/api/v1/scripts/" + testScriptID4,
	} {
		rec := f.doAs(t, viewerCookie, viewerCSRF, http.MethodGet, path, "")
		if rec.Code != http.StatusForbidden {
			t.Fatalf("viewer %s 状态码 = %d，期望 403", path, rec.Code)
		}
	}
}

// TestSchedulesUnavailableWithoutReader：没接代理时 503，不是空列表。
func TestSchedulesUnavailableWithoutReader(t *testing.T) {
	f := newScheduleFixture(t, nil, nil)
	for _, path := range []string{
		"/api/v1/schedules",
		"/api/v1/schedules/" + testScheduleID4,
		"/api/v1/scripts",
		"/api/v1/scripts/" + testScriptID4,
	} {
		rec := f.do(t, http.MethodGet, path, "")
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s 状态码 = %d，期望 503", path, rec.Code)
		}
		if code := errorCode(t, rec); code != string(httpx.CodeAgentUnavailable) {
			t.Fatalf("%s 错误码 = %q，期望 AGENT_UNAVAILABLE", path, code)
		}
		// 空列表会被人读成"这台机器上没有定时任务"，那是不一样的话。
		if strings.Contains(rec.Body.String(), "items") {
			t.Fatalf("%s 的 503 不该带一份列表: %s", path, rec.Body.String())
		}
	}
}

// TestListSchedulesSplitsIntentAndFacts：意图是意图，事实是事实。
func TestListSchedulesSplitsIntentAndFacts(t *testing.T) {
	f := newScheduleFixture(t, &fakeSchedulesReader{list: []agentclient.Schedule{sampleSchedule()}},
		&fakeScriptsReader{})
	rec := f.do(t, http.MethodGet, "/api/v1/schedules", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，响应 = %s", rec.Code, rec.Body.String())
	}
	var out scheduleListResponse
	if err := json.Unmarshal(parseResult(t, rec).Data, &out); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(out.Items) != 1 {
		t.Fatalf("列表长度 = %d，期望 1", len(out.Items))
	}
	got := out.Items[0]
	if got.ID != testScheduleID4 || got.Kind != string(schedules.KindBackup) {
		t.Fatalf("列表内容不对：%+v", got)
	}
	// 意图：定义说启用。
	if !got.Enabled || !got.Definition.Enabled {
		t.Fatalf("意图应当是启用：enabled=%v definition.enabled=%v", got.Enabled, got.Definition.Enabled)
	}
	// 事实：单元没装，systemd 没话说。
	if got.Installed {
		t.Fatal("installed 应当如实为 false：这台机器上没有这套单元")
	}
	if got.UnitFileState != "" || got.ActiveState != "" {
		t.Fatalf("systemd 没给原话时不该自己编：%q / %q", got.UnitFileState, got.ActiveState)
	}
	if got.Definition.CalendarExpression != "*-*-* 03:30:00" || got.Definition.Timezone != "Asia/Shanghai" {
		t.Fatalf("定义字段没带全：%+v", got.Definition)
	}
	if !got.Definition.Persistent || got.Definition.MaxRuntimeSeconds != 3600 {
		t.Fatalf("错过执行策略与时长上限要带出来：%+v", got.Definition)
	}
	// 阻塞项要说清"为什么现在跑不起来"。
	if len(got.Blockers) != 1 || got.Blockers[0].Code != "SCHEDULE_UNIT_MISSING" {
		t.Fatalf("阻塞项不对：%+v", got.Blockers)
	}
	if got.NextElapse != "" {
		t.Fatalf("读不到下次触发时刻时不该出现该字段，实际 %q", got.NextElapse)
	}
	if got.CreatedAt != "2026-10-24T01:00:00Z" {
		t.Fatalf("时间要按 RFC3339 输出，实际 %q", got.CreatedAt)
	}
}

// TestListSchedulesBlockersNeverNull：[] 表示"没有阻塞项"，null 会被读成"没检查"。
func TestListSchedulesBlockersNeverNull(t *testing.T) {
	clean := sampleSchedule()
	clean.Blockers = nil
	clean.NextElapse = time.Date(2026, 10, 25, 19, 30, 0, 0, time.UTC)
	f := newScheduleFixture(t, &fakeSchedulesReader{list: []agentclient.Schedule{clean}}, &fakeScriptsReader{})
	rec := f.do(t, http.MethodGet, "/api/v1/schedules", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, `"blockers":null`) {
		t.Fatalf("blockers 不能是 null: %s", body)
	}
	if !strings.Contains(body, `"blockers":[]`) {
		t.Fatalf("没有阻塞项时应当是空数组: %s", body)
	}
	var out scheduleListResponse
	if err := json.Unmarshal(parseResult(t, rec).Data, &out); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if out.Items[0].NextElapse != "2026-10-25T19:30:00Z" {
		t.Fatalf("下次触发时刻 = %q", out.Items[0].NextElapse)
	}
}

// TestGetScheduleHasTriggers：详情带最近的触发记录。
func TestGetScheduleHasTriggers(t *testing.T) {
	reader := &fakeSchedulesReader{detail: agentclient.ScheduleDetail{
		Schedule: sampleSchedule(),
		Triggers: []agentclient.ScheduleTrigger{{
			TriggerID:   testTriggerID4,
			SlotAt:      time.Date(2026, 10, 24, 19, 30, 0, 0, time.UTC),
			Action:      "backup.run",
			OperationID: "op_" + strings.Repeat("a", 32),
			Outcome:     "triggered",
			CreatedAt:   time.Date(2026, 10, 24, 19, 30, 1, 0, time.UTC),
		}},
	}}
	f := newScheduleFixture(t, reader, &fakeScriptsReader{})
	rec := f.do(t, http.MethodGet, "/api/v1/schedules/"+testScheduleID4, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，响应 = %s", rec.Code, rec.Body.String())
	}
	if reader.gotID != testScheduleID4 {
		t.Fatalf("传给读取端的 id = %q", reader.gotID)
	}
	var out scheduleDetailResponse
	if err := json.Unmarshal(parseResult(t, rec).Data, &out); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if out.Schedule.ID != testScheduleID4 {
		t.Fatalf("计划 id = %q", out.Schedule.ID)
	}
	if len(out.Triggers) != 1 {
		t.Fatalf("触发记录长度 = %d，期望 1", len(out.Triggers))
	}
	tr := out.Triggers[0]
	if tr.TriggerID != testTriggerID4 || tr.Outcome != "triggered" || tr.Action != "backup.run" {
		t.Fatalf("触发记录不对：%+v", tr)
	}
	if tr.SlotAt != "2026-10-24T19:30:00Z" {
		t.Fatalf("时间槽 = %q", tr.SlotAt)
	}
}

// TestScheduleBadIDShapeIsBadRequest：形状不对是 400，不是代理的 404。
func TestScheduleBadIDShapeIsBadRequest(t *testing.T) {
	reader := &fakeSchedulesReader{}
	f := newScheduleFixture(t, reader, &fakeScriptsReader{})

	for _, id := range []string{"sch_xyz", "sch_" + strings.Repeat("0", 31), "SCH_" + strings.Repeat("0", 32)} {
		rec := f.do(t, http.MethodGet, "/api/v1/schedules/"+id, "")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("id=%q 状态码 = %d，期望 400", id, rec.Code)
		}
		if code := errorCode(t, rec); code != string(httpx.CodeValidationError) {
			t.Fatalf("id=%q 错误码 = %q，期望 VALIDATION_ERROR", id, code)
		}
	}
	// 带路径分隔符的 id 走不到 handler：gin 的路由树在匹配阶段就把它当成了
	// 另一条路径（清理后是 /api/v1/etc/passwd）。这里断言的是"没有被当成
	// 一条计划 id 去问代理"，而不是某个具体状态码——路由改版也不该让这条变绿。
	rec := f.do(t, http.MethodGet, "/api/v1/schedules/../../etc/passwd", "")
	if rec.Code == http.StatusOK {
		t.Fatalf("带 .. 的 id 不该成功: %s", rec.Body.String())
	}
	if reader.gotID != "" {
		t.Fatalf("形状不对的 id 不该问代理，实际问了 %q", reader.gotID)
	}
}

// TestScheduleAgentNotFoundIs404：代理说没有才是 404。
func TestScheduleAgentNotFoundIs404(t *testing.T) {
	reader := &fakeSchedulesReader{detailErr: &agentclient.Error{
		Code: string(httpx.CodeNotFound), Message: "计划任务不存在",
	}}
	f := newScheduleFixture(t, reader, &fakeScriptsReader{})
	rec := f.do(t, http.MethodGet, "/api/v1/schedules/"+testScheduleID4, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d，期望 404", rec.Code)
	}
	if code := errorCode(t, rec); code != string(httpx.CodeNotFound) {
		t.Fatalf("错误码 = %q，期望 NOT_FOUND", code)
	}
}

// TestScheduleAgentUnavailableIs503：调用失败是 503，不是 500 也不是空结果。
func TestScheduleAgentUnavailableIs503(t *testing.T) {
	reader := &fakeSchedulesReader{listErr: &agentclient.UnavailableError{Err: errors.New("dial unix: connection refused")}}
	f := newScheduleFixture(t, reader, &fakeScriptsReader{})
	rec := f.do(t, http.MethodGet, "/api/v1/schedules", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d，期望 503", rec.Code)
	}
	if code := errorCode(t, rec); code != string(httpx.CodeAgentUnavailable) {
		t.Fatalf("错误码 = %q，期望 AGENT_UNAVAILABLE", code)
	}
}

// TestScriptListHasNoContent：列表只返回元数据，正文只在详情里。
func TestScriptListHasNoContent(t *testing.T) {
	rec0 := agentclient.Script{
		ID:          testScriptID4,
		Name:        "清理临时文件",
		ContentHash: "sha256:" + strings.Repeat("b", 64),
		Interpreter: "/bin/sh",
		Version:     2,
		CreatedAt:   time.Date(2026, 10, 24, 1, 0, 0, 0, time.UTC),
		UpdatedAt:   time.Date(2026, 10, 24, 3, 0, 0, 0, time.UTC),
		Content:     "#!/bin/sh\nrm -rf /var/tmp/upanel-scratch/*\n",
	}
	f := newScheduleFixture(t, &fakeSchedulesReader{}, &fakeScriptsReader{
		list: []agentclient.Script{rec0},
		one:  rec0,
	})

	rec := f.do(t, http.MethodGet, "/api/v1/scripts", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，响应 = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "rm -rf") || strings.Contains(body, `"content"`) {
		t.Fatalf("列表不该带正文: %s", body)
	}
	var out scriptListResponse
	if err := json.Unmarshal(parseResult(t, rec).Data, &out); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(out.Items) != 1 || out.Items[0].ContentHash != rec0.ContentHash {
		t.Fatalf("列表内容不对：%+v", out.Items)
	}
	if out.Items[0].Interpreter != "/bin/sh" {
		t.Fatalf("解释器 = %q（它由代理决定，调用方不能指定）", out.Items[0].Interpreter)
	}

	rec2 := f.do(t, http.MethodGet, "/api/v1/scripts/"+testScriptID4, "")
	if rec2.Code != http.StatusOK {
		t.Fatalf("详情状态码 = %d，响应 = %s", rec2.Code, rec2.Body.String())
	}
	var detail scriptDetailResponse
	if err := json.Unmarshal(parseResult(t, rec2).Data, &detail); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if detail.Script.Content != rec0.Content {
		t.Fatalf("详情要带正文，实际 %q", detail.Script.Content)
	}
}

// TestScriptBadIDShapeIsBadRequest：脚本 id 形状不对也是 400。
func TestScriptBadIDShapeIsBadRequest(t *testing.T) {
	reader := &fakeScriptsReader{}
	f := newScheduleFixture(t, &fakeSchedulesReader{}, reader)
	for _, id := range []string{"scr_xyz", "scr_" + strings.Repeat("0", 40)} {
		rec := f.do(t, http.MethodGet, "/api/v1/scripts/"+id, "")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("id=%q 状态码 = %d，期望 400", id, rec.Code)
		}
	}
	if reader.gotID != "" {
		t.Fatalf("形状不对的 id 不该问代理，实际问了 %q", reader.gotID)
	}
}

// TestScriptsValidIDsAccepted 确保测试里用的 id 真的过得了形状校验。
//
// 这条看起来多余，但它挡的是一整类假通过：如果 ValidID 的规则变了而常量没变，
// 上面几条"400"的用例会继续绿，而"能读到"的那条会变成 400——那才是真信号。
func TestScriptsValidIDsAccepted(t *testing.T) {
	if !schedules.ValidID(testScheduleID4) {
		t.Fatalf("测试用的计划 id 过不了 schedules.ValidID: %s", testScheduleID4)
	}
	if !scripts.ValidID(testScriptID4) {
		t.Fatalf("测试用的脚本 id 过不了 scripts.ValidID: %s", testScriptID4)
	}
}
