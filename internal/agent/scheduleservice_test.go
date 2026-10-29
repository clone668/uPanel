package agent

// 计划与脚本的镜像读路由（P12.4）：GET /internal/v1/schedules[/:id]、/internal/v1/scripts[/:id]。
//
// 这几条路由存在的理由是"只有一份真相"。Web 不再自己存计划与脚本，它连"systemd 有没有
// 真的 enable 了这个 timer"都要问代理。所以测试盯两件事：**意图与事实并排给出**，
// 以及列表与详情的区别（脚本列表不带正文）。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"upanel/internal/schedules"
	"upanel/internal/scripts"
	"upanel/internal/version"
)

// newMirrorServer 挂上计划与脚本的镜像读路由。
func newMirrorServer(t *testing.T, sched SchedulesService, scr ScriptsService) *Server {
	t.Helper()
	db := newTestAgentDB(t)
	srv, err := New(Options{
		SocketPath:      socketPathForTest(t),
		Version:         version.Info{Version: "test", Commit: "test", BuildDate: "test", GoVersion: "test"},
		Logger:          quietLogger(),
		ShutdownTimeout: time.Second,
		Peer:            testPeer(),
		Registry:        NewRegistry(),
		Secrets:         newTestSecretStore(t),
		Plans:           &recordingPlanStore{inner: newTestPlanStoreOn(t, db)},
		Operations:      newTestOperationStoreOn(t, db),
		Schedules:       sched,
		Scripts:         scr,
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	return srv
}

func getPath(t *testing.T, srv *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("X-Request-ID", "rid-mirror")
	return serveAs(srv, req, testWebUID)
}

// fakeView 造一条"意图已启用、systemd 也认"的计划现场。
func fakeView() schedules.View {
	created := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	return schedules.View{
		Record: schedules.Record{
			ID:       testScheduleID,
			Name:     "夜间备份",
			Kind:     schedules.KindBackup,
			TargetID: "pol_0123456789abcdef0123456789abcdef",
			Definition: schedules.Definition{
				Name: "夜间备份", Kind: schedules.KindBackup,
				TargetID:           "pol_0123456789abcdef0123456789abcdef",
				CalendarExpression: "*-*-* 03:30:00",
				Timezone:           "Asia/Shanghai",
				Persistent:         true,
				MaxRuntimeSeconds:  7200,
				Enabled:            true,
			},
			TimerRef:  "upanel-sch_x.timer",
			Enabled:   true,
			Version:   3,
			CreatedAt: created,
			UpdatedAt: created.Add(time.Hour),
		},
		Units: schedules.Status{
			Present:       true,
			UnitFileState: "enabled",
			ActiveState:   "active",
		},
		NextElapse: timePtr(time.Date(2026, 10, 25, 3, 30, 0, 0, time.UTC)),
	}
}

func TestSchedulesListRoute(t *testing.T) {
	svc := newFakeScheduleService()
	svc.views = []schedules.View{fakeView()}
	srv := newMirrorServer(t, svc, nil)

	rec := getPath(t, srv, "/internal/v1/schedules")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	var out struct {
		Schedules []scheduleViewResponse `json:"schedules"`
	}
	decodeData(t, rec.Body.Bytes(), &out)
	if len(out.Schedules) != 1 {
		t.Fatalf("条数 = %d", len(out.Schedules))
	}
	got := out.Schedules[0]
	// 意图
	if !got.Enabled || got.Version != 3 || got.TimerRef != "upanel-sch_x.timer" {
		t.Fatalf("意图字段 = %+v", got)
	}
	if got.Definition.Timezone != "Asia/Shanghai" || got.Definition.CalendarExpression != "*-*-* 03:30:00" {
		t.Fatalf("定义 = %+v", got.Definition)
	}
	// 事实
	if !got.Installed || got.UnitFileState != "enabled" || got.ActiveState != "active" {
		t.Fatalf("单元事实 = %+v", got)
	}
	if got.NextElapse != "2026-10-25T03:30:00Z" {
		t.Fatalf("下次触发 = %q", got.NextElapse)
	}
	// 空的阻塞项要序列化成 []，不能是 null：界面直接遍历它。
	if got.Blockers == nil {
		t.Fatal("blockers 不能是 null")
	}
}

func TestScheduleDetailRoute(t *testing.T) {
	svc := newFakeScheduleService()
	svc.view = fakeView()
	svc.viewOK = true
	svc.triggers = []schedules.TriggerRecord{{
		TriggerID:   testTriggerID,
		ScheduleID:  testScheduleID,
		SlotAt:      time.Date(2026, 10, 24, 19, 30, 0, 0, time.UTC),
		Action:      "backup.run",
		OperationID: schedules.OperationID(testTriggerID),
		Outcome:     schedules.TriggerOutcomeTriggered,
		CreatedAt:   time.Date(2026, 10, 24, 19, 30, 0, 0, time.UTC),
	}}
	srv := newMirrorServer(t, svc, nil)

	rec := getPath(t, srv, "/internal/v1/schedules/"+testScheduleID)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	var out scheduleDetailResponse
	decodeData(t, rec.Body.Bytes(), &out)
	if out.Schedule.ID != testScheduleID || out.Schedule.NextElapse == "" {
		t.Fatalf("计划 = %+v", out.Schedule)
	}
	if len(out.Triggers) != 1 || out.Triggers[0].TriggerID != testTriggerID {
		t.Fatalf("触发记录 = %+v", out.Triggers)
	}
	if out.Triggers[0].SlotAt != "2026-10-24T19:30:00Z" {
		t.Fatalf("触发时刻 = %q", out.Triggers[0].SlotAt)
	}
}

func TestScheduleDetailNotFound(t *testing.T) {
	svc := newFakeScheduleService()
	svc.viewOK = false
	srv := newMirrorServer(t, svc, nil)
	if rec := getPath(t, srv, "/internal/v1/schedules/"+testScheduleID); rec.Code != http.StatusNotFound {
		t.Fatalf("未知计划状态码 = %d", rec.Code)
	}
	// 形状不对的 id 也是 404，而不是 400：对外没有"这条路由存在但参数错"的信息。
	if rec := getPath(t, srv, "/internal/v1/schedules/nope"); rec.Code != http.StatusNotFound {
		t.Fatalf("坏 id 状态码 = %d", rec.Code)
	}
	// 不认识的子路径同样是 404，不能落到详情上。
	if rec := getPath(t, srv, "/internal/v1/schedules/"+testScheduleID+"/runs"); rec.Code != http.StatusNotFound {
		t.Fatalf("未知子路径状态码 = %d", rec.Code)
	}
}

func TestSchedulesListRejectsWrite(t *testing.T) {
	srv := newMirrorServer(t, newFakeScheduleService(), nil)
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/schedules", strings.NewReader(`{}`))
	rec := serveAs(srv, req, testWebUID)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("状态码 = %d", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != "GET" {
		t.Fatalf("Allow = %q", got)
	}
}

// TestScriptsListHidesContent：列表只有元数据。
func TestScriptsListHidesContent(t *testing.T) {
	scr := newFakeScripts()
	scr.record.ID = testScriptID
	scr.record.Content = "echo 秘密\n"
	srv := newMirrorServer(t, nil, scr)

	rec := getPath(t, srv, "/internal/v1/scripts")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "秘密") {
		t.Fatalf("列表里带了正文: %s", rec.Body.String())
	}
	var out struct {
		Scripts []scriptViewResponse `json:"scripts"`
	}
	decodeData(t, rec.Body.Bytes(), &out)
	if len(out.Scripts) != 1 || out.Scripts[0].ID != testScriptID {
		t.Fatalf("列表 = %+v", out.Scripts)
	}
	if out.Scripts[0].ContentHash != "sha256:bbb" || out.Scripts[0].Version != 1 {
		t.Fatalf("元数据 = %+v", out.Scripts[0])
	}
}

// TestScriptDetailHasContent：详情才给正文（管理员要能看见自己批准的是什么）。
func TestScriptDetailHasContent(t *testing.T) {
	scr := newFakeScripts()
	scr.record.ID = testScriptID
	scr.record.Content = "echo 巡检\n"
	srv := newMirrorServer(t, nil, scr)

	rec := getPath(t, srv, "/internal/v1/scripts/"+testScriptID)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	var out scriptDetailResponse
	decodeData(t, rec.Body.Bytes(), &out)
	if out.Script.Content != "echo 巡检\n" {
		t.Fatalf("正文 = %q", out.Script.Content)
	}
	if out.Script.Interpreter != scripts.Interpreter {
		t.Fatalf("解释器 = %q", out.Script.Interpreter)
	}
}

func TestScriptsNotFound(t *testing.T) {
	scr := newFakeScripts()
	srv := newMirrorServer(t, nil, scr)
	other := "scr_" + strings.Repeat("0", 32)
	if rec := getPath(t, srv, "/internal/v1/scripts/"+other); rec.Code != http.StatusNotFound {
		t.Fatalf("未知脚本状态码 = %d", rec.Code)
	}
	if rec := getPath(t, srv, "/internal/v1/scripts/nope"); rec.Code != http.StatusNotFound {
		t.Fatalf("坏 id 状态码 = %d", rec.Code)
	}
	req := httptest.NewRequest(http.MethodDelete, "/internal/v1/scripts/"+testScriptID, nil)
	if rec := serveAs(srv, req, testWebUID); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE 状态码 = %d", rec.Code)
	}
}

// TestMirrorRoutesHiddenWithoutWiring：没接线就是 404，不是空列表。
// 空列表会让界面说"你还没有计划"，而事实是这个代理根本没启用这一块。
func TestMirrorRoutesHiddenWithoutWiring(t *testing.T) {
	srv := newMirrorServer(t, nil, nil)
	for _, path := range []string{"/internal/v1/schedules", "/internal/v1/scripts"} {
		if rec := getPath(t, srv, path); rec.Code != http.StatusNotFound {
			t.Fatalf("%s 状态码 = %d，期望 404", path, rec.Code)
		}
	}
}
