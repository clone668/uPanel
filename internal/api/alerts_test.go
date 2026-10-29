package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"upanel/internal/alerts"
	"upanel/internal/audit"
	"upanel/internal/auth"
	"upanel/internal/certificates"
	"upanel/internal/config"
	"upanel/internal/platform"
	"upanel/internal/version"
)

// alertsFixture 装配一个带告警服务的真实数据库服务。
func newAlertsFixture(t *testing.T) *adminFixture {
	t.Helper()
	authSvc, aud, db := newTestServicesWithDB(t)
	alertSvc, err := alerts.New(db, alerts.Options{Logger: quietLogger()})
	if err != nil {
		t.Fatalf("构造告警服务失败: %v", err)
	}
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
		Alerts:            alertSvc,
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	seedTestUser(t, srv.opts.Auth, testUser, testUserPass)
	cookie, csrf := loginOK(t, srv)
	return &adminFixture{srv: srv, cookie: cookie, csrf: csrf, db: db}
}

// ruleBody 是一条 CPU 规则的合法请求体。
func ruleBody(threshold float64) map[string]any {
	return map[string]any{
		"name":               "CPU 持续高",
		"metric":             "cpu_usage_percent",
		"comparison":         "gte",
		"threshold":          threshold,
		"duration_seconds":   60,
		"recovery_threshold": 80,
		"cooldown_seconds":   120,
		"enabled":            true,
	}
}

func jsonBody(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("构造请求体失败: %v", err)
	}
	return string(raw)
}

// decodeRule 解出响应里的规则。
func decodeRule(t *testing.T, rec *httptest.ResponseRecorder) alertRuleDTO {
	t.Helper()
	var body struct {
		Data alertRuleDTO `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v（%s）", err, rec.Body.String())
	}
	return body.Data
}

// TestAlertsUnavailableWithoutService：没装配告警时明确回 503，而不是空列表。
//
// 空列表会被读成“这台机器上没配任何告警”，那会让一次真实故障看起来像是
// “没人配规则”而不是“功能没开”。
func TestAlertsUnavailableWithoutService(t *testing.T) {
	f := newAdminFixture(t) // 这个固定装置没有装 Alerts

	for _, path := range []string{"/api/v1/alerts/rules", "/api/v1/alerts/events", "/api/v1/alerts/silence"} {
		rec := f.do(t, http.MethodGet, path, "")
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s 状态码 = %d，期望 503", path, rec.Code)
		}
		if got := parseResult(t, rec); got.Error == nil || got.Error.Code != "NOT_IMPLEMENTED" {
			t.Fatalf("%s 错误码 = %+v", path, got.Error)
		}
	}
}

func TestAlertsRequireSession(t *testing.T) {
	f := newAlertsFixture(t)

	for _, req := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/alerts/rules"},
		{http.MethodGet, "/api/v1/alerts/events"},
		{http.MethodGet, "/api/v1/alerts/silence"},
		{http.MethodPost, "/api/v1/alerts/rules"},
		{http.MethodPut, "/api/v1/alerts/silence"},
	} {
		rec := f.doAs(t, nil, "", req.method, req.path, "")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s 未登录状态码 = %d，期望 401", req.method, req.path, rec.Code)
		}
	}
}

func TestAlertRuleReadableByViewerWritableOnlyByAdmin(t *testing.T) {
	f := newAlertsFixture(t)
	if _, err := f.srv.opts.Auth.CreateUser(context.Background(), auth.CreateUserInput{
		Username: "watcher", DisplayName: "watcher", Role: auth.RoleViewer, Password: "watcher-horse-9",
	}); err != nil {
		t.Fatalf("创建 viewer 失败: %v", err)
	}
	viewerCookie, viewerCSRF := loginAsUser(t, f.srv, "watcher", "watcher-horse-9")

	rec := f.doAs(t, viewerCookie, viewerCSRF, http.MethodGet, "/api/v1/alerts/rules", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("viewer 读规则状态码 = %d，期望 200（%s）", rec.Code, rec.Body.String())
	}
	rec = f.doAs(t, viewerCookie, viewerCSRF, http.MethodPost, "/api/v1/alerts/rules", jsonBody(t, ruleBody(90)))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer 写规则状态码 = %d，期望 403", rec.Code)
	}
	rec = f.doAs(t, viewerCookie, viewerCSRF, http.MethodPut, "/api/v1/alerts/silence", "{}")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer 设静默状态码 = %d，期望 403", rec.Code)
	}
}

func TestAlertRuleCreateDefaultsCooldown(t *testing.T) {
	f := newAlertsFixture(t)

	// 不给 cooldown_seconds：应当落成 300 秒的默认冷却，而不是 0。
	body := ruleBody(90)
	delete(body, "cooldown_seconds")
	rec := f.do(t, http.MethodPost, "/api/v1/alerts/rules", jsonBody(t, body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("创建状态码 = %d，期望 201（%s）", rec.Code, rec.Body.String())
	}
	if got := decodeRule(t, rec).CooldownSeconds; got != 300 {
		t.Fatalf("默认冷却 = %d，期望 300", got)
	}

	// 显式给 0 才是“不要冷却”——全指针请求体分得出这两件事。
	explicit := ruleBody(90)
	explicit["cooldown_seconds"] = 0
	rec = f.do(t, http.MethodPost, "/api/v1/alerts/rules", jsonBody(t, explicit))
	if rec.Code != http.StatusCreated {
		t.Fatalf("创建状态码 = %d，期望 201（%s）", rec.Code, rec.Body.String())
	}
	if got := decodeRule(t, rec).CooldownSeconds; got != 0 {
		t.Fatalf("显式 0 冷却 = %d，期望 0", got)
	}
}

// 容量序列 30 秒才落一个点：持续窗口装不下两个点，规则就没法说“持续”。
func TestAlertRuleRejectsTooShortDurationForCapacitySeries(t *testing.T) {
	f := newAlertsFixture(t)

	body := map[string]any{
		"name":               "空间使用率高",
		"metric":             "used_percent",
		"target_id":          "/",
		"comparison":         "gte",
		"threshold":          85,
		"recovery_threshold": 70,
		"duration_seconds":   30,
	}
	rec := f.do(t, http.MethodPost, "/api/v1/alerts/rules", jsonBody(t, body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("容量规则持续 30 秒状态码 = %d，期望 400（%s）", rec.Code, rec.Body.String())
	}
	if got := parseResult(t, rec); got.Error == nil || got.Error.Code != "VALIDATION_ERROR" {
		t.Fatalf("错误码 = %+v", got.Error)
	}

	body["duration_seconds"] = 60
	rec = f.do(t, http.MethodPost, "/api/v1/alerts/rules", jsonBody(t, body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("容量规则持续 60 秒状态码 = %d，期望 201（%s）", rec.Code, rec.Body.String())
	}
	if got := decodeRule(t, rec); got.Scope != "filesystem" || got.TargetID != "/" {
		t.Fatalf("scope/target 推导错了: %+v", got)
	}
}

func TestAlertRuleCRUD(t *testing.T) {
	f := newAlertsFixture(t)

	rec := f.do(t, http.MethodPost, "/api/v1/alerts/rules", jsonBody(t, ruleBody(90)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("创建状态码 = %d，期望 201（%s）", rec.Code, rec.Body.String())
	}
	created := decodeRule(t, rec)
	if created.Version != 1 || created.State != "ok" || created.Scope != "host" {
		t.Fatalf("新建规则 = %+v", created)
	}
	if created.Threshold != 90 || created.DurationSeconds != 60 || !created.Enabled {
		t.Fatalf("字段没有落库: %+v", created)
	}

	// 只改名字：PATCH 是部分更新，其余字段必须保持原值。
	rec = f.do(t, http.MethodPatch, "/api/v1/alerts/rules/1", jsonBody(t, map[string]any{
		"name": "CPU 持续高（改名）", "version": created.Version,
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH 状态码 = %d，期望 200（%s）", rec.Code, rec.Body.String())
	}
	updated := decodeRule(t, rec)
	if updated.Name != "CPU 持续高（改名）" || updated.Threshold != 90 ||
		updated.DurationSeconds != 60 || updated.Version != 2 {
		t.Fatalf("部分更新把别的字段改掉了: %+v", updated)
	}

	// 用旧版本再改一次：必须 409，不能悄悄覆盖。
	rec = f.do(t, http.MethodPatch, "/api/v1/alerts/rules/1", jsonBody(t, map[string]any{
		"threshold": 95, "version": 1,
	}))
	if rec.Code != http.StatusConflict {
		t.Fatalf("版本冲突状态码 = %d，期望 409", rec.Code)
	}
	if got := parseResult(t, rec); got.Error == nil || got.Error.Code != "VERSION_CONFLICT" {
		t.Fatalf("版本冲突错误码 = %+v", got.Error)
	}

	// 关掉规则：enabled=false 是零值，只有指针化的请求体才分得清它和“没提”。
	rec = f.do(t, http.MethodPatch, "/api/v1/alerts/rules/1", jsonBody(t, map[string]any{
		"enabled": false, "version": 2,
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("停用状态码 = %d，期望 200（%s）", rec.Code, rec.Body.String())
	}
	if disabled := decodeRule(t, rec); disabled.Enabled {
		t.Fatalf("enabled=false 没有生效: %+v", disabled)
	}

	rec = f.do(t, http.MethodDelete, "/api/v1/alerts/rules/1?version=3", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("删除状态码 = %d，期望 200（%s）", rec.Code, rec.Body.String())
	}
	rec = f.do(t, http.MethodGet, "/api/v1/alerts/rules", "")
	var list struct {
		Data []alertRuleDTO `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if len(list.Data) != 0 {
		t.Fatalf("删除后列表还有 %d 条", len(list.Data))
	}
}

func TestAlertRuleValidationErrors(t *testing.T) {
	f := newAlertsFixture(t)

	cases := []struct {
		name  string
		body  map[string]any
		field string
	}{
		{
			name: "恢复线在错误的一侧",
			body: func() map[string]any {
				b := ruleBody(90)
				b["recovery_threshold"] = 95
				return b
			}(),
		},
		{
			name: "缺少 recovery_threshold",
			body: func() map[string]any {
				b := ruleBody(90)
				delete(b, "recovery_threshold")
				return b
			}(),
		},
		{
			name: "未知指标",
			body: func() map[string]any {
				b := ruleBody(90)
				b["metric"] = "gpu_usage_percent"
				return b
			}(),
		},
		{
			name: "次级口径只给了一半",
			body: func() map[string]any {
				b := ruleBody(90)
				b["secondary_threshold"] = 10
				return b
			}(),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := f.do(t, http.MethodPost, "/api/v1/alerts/rules", jsonBody(t, tc.body))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("状态码 = %d，期望 400（%s）", rec.Code, rec.Body.String())
			}
			if got := parseResult(t, rec); got.Error == nil || got.Error.Code != "VALIDATION_ERROR" {
				t.Fatalf("错误码 = %+v", got.Error)
			}
		})
	}
}

func TestAlertSilenceRoundTrip(t *testing.T) {
	f := newAlertsFixture(t)

	rec := f.do(t, http.MethodGet, "/api/v1/alerts/silence", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("读取静默状态码 = %d，期望 200", rec.Code)
	}
	var empty struct {
		Data alertSilenceDTO `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &empty); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if empty.Data.Active || empty.Data.StartsAt != nil || empty.Data.EndsAt != nil {
		t.Fatalf("初始状态不该有静默窗口: %+v", empty.Data)
	}
	// 契约 §13.2 说"没有窗口时起止为 null"。这里查的是**原始 JSON 里的 null**，不是
	// Go 侧的 nil 指针——`""` 解成指针也会是 nil，那种写法证明不了线上看到的是什么。
	// 空串在客户端是另一件事：`new Date("")` 给出非法时间、`x == null` 判成"有值"。
	for _, k := range []string{"starts_at", "ends_at"} {
		if v := dataField(t, rec.Body.Bytes(), k); v != nil {
			t.Fatalf("没有窗口时 %s 应为 JSON null，实际 %#v", k, v)
		}
	}

	start := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	end := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	rec = f.do(t, http.MethodPut, "/api/v1/alerts/silence", jsonBody(t, map[string]any{
		"starts_at": start, "ends_at": end, "reason": "内核升级",
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("设置静默状态码 = %d，期望 200（%s）", rec.Code, rec.Body.String())
	}
	var set struct {
		Data alertSilenceDTO `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &set); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if !set.Data.Active || set.Data.Reason != "内核升级" {
		t.Fatalf("静默窗口 = %+v", set.Data)
	}

	// 起止都省略 = 清除。
	rec = f.do(t, http.MethodPut, "/api/v1/alerts/silence", "{}")
	if rec.Code != http.StatusOK {
		t.Fatalf("清除静默状态码 = %d，期望 200（%s）", rec.Code, rec.Body.String())
	}
	var cleared struct {
		Data alertSilenceDTO `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &cleared); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if cleared.Data.Active || cleared.Data.StartsAt != nil || cleared.Data.EndsAt != nil {
		t.Fatalf("清除后还有窗口: %+v", cleared.Data)
	}
}

func TestAlertEventsListable(t *testing.T) {
	f := newAlertsFixture(t)

	rec := f.do(t, http.MethodGet, "/api/v1/alerts/events?state=firing&limit=10", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("事件列表状态码 = %d，期望 200（%s）", rec.Code, rec.Body.String())
	}
	var body struct {
		Data []alertEventDTO `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if len(body.Data) != 0 {
		t.Fatalf("还没有触发过，事件列表应为空，实际 %d 条", len(body.Data))
	}

	rec = f.do(t, http.MethodGet, "/api/v1/alerts/events?state=bogus", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法 state 状态码 = %d，期望 400", rec.Code)
	}
}

// dataField 从 {"data": {...}} 里取一个字段的**原始 JSON 值**。
//
// 存在的理由：Go 侧的 *string 为 nil 有两种来源——JSON null 和 JSON "" 都会解成 nil
// （后者解成指向空串的指针，前者才是 nil，但写错了也看不出来）。所以"契约说是 null"
// 这件事必须在原始 JSON 上判，不能在解好的结构体上判。
func dataField(t *testing.T, body []byte, key string) any {
	t.Helper()
	var raw struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("响应不是合法 JSON: %v（原文 %s）", err, string(body))
	}
	v, ok := raw.Data[key]
	if !ok {
		t.Fatalf("响应里没有字段 %s（原文 %s）", key, string(body))
	}
	return v
}

// TestAlertEventNullableTimesAreJSONNull 钉住"可空时间字段是 JSON null，不是空串"。
//
// 库层已经有 CHECK 约束保证 firing 的行 resolved_at 为 NULL（反之 resolved 必须有值），
// 接口层原先把它格式化成空串：于是"这条事件到底恢复了没有"要靠 state 判，resolved_at
// 上什么都看不出来。契约 §13.2 写的是 null，这里按**原始 JSON** 判，并且两种状态各造
// 一条——只造一条的话，"全都返回 null" 也能过。
func TestAlertEventNullableTimesAreJSONNull(t *testing.T) {
	f := newAlertsFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	insert := `INSERT INTO alert_events
	    (rule_id, rule_name, scope, target_id, metric, comparison, threshold,
	     state, started_at, resolved_at, duration_seconds, peak_value, silenced, created_at)
	    VALUES (NULL, ?, 'host', '', 'load1', 'gte', 1000, ?, ?, ?, ?, ?, 0, ?)`
	at := now.Format(time.RFC3339)
	if _, err := f.db.SQL().Exec(insert, "还在报的", "firing", at, nil, nil, nil, at); err != nil {
		t.Fatalf("写入 firing 事件失败: %v", err)
	}
	if _, err := f.db.SQL().Exec(insert, "已经恢复的", "resolved", at, at, 90, 1234.5, at); err != nil {
		t.Fatalf("写入 resolved 事件失败: %v", err)
	}

	rec := f.do(t, http.MethodGet, "/api/v1/alerts/events?limit=10", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("事件列表状态码 = %d，期望 200（%s）", rec.Code, rec.Body.String())
	}
	var body struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if len(body.Data) != 2 {
		t.Fatalf("事件数 = %d，期望 2", len(body.Data))
	}
	seen := map[string]map[string]any{}
	for _, e := range body.Data {
		seen[e["rule_name"].(string)] = e
	}
	open, ok := seen["还在报的"]
	if !ok {
		t.Fatalf("列表里没有未恢复的那条: %+v", body.Data)
	}
	if v := open["resolved_at"]; v != nil {
		t.Fatalf("未恢复事件的 resolved_at 应为 JSON null，实际 %#v", v)
	}
	if v := open["acked_at"]; v != nil {
		t.Fatalf("没有确认过的事件 acked_at 应为 JSON null，实际 %#v", v)
	}
	done, ok := seen["已经恢复的"]
	if !ok {
		t.Fatalf("列表里没有已恢复的那条: %+v", body.Data)
	}
	if v, isStr := done["resolved_at"].(string); !isStr || v == "" {
		t.Fatalf("已恢复事件的 resolved_at 应是非空字符串，实际 %#v", done["resolved_at"])
	}
}

// TestAlertRuleLastValueIsJSONNull 钉住同一类漂移的第三条：这一轮没读到数据时，
// last_value 是 null 而不是 0。"没有数据"与"值是 0"在告警界面是两件事。
func TestAlertRuleLastValueIsJSONNull(t *testing.T) {
	f := newAlertsFixture(t)
	rec := f.do(t, http.MethodPost, "/api/v1/alerts/rules", jsonBody(t, ruleBody(90)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("建规则状态码 = %d，期望 201（%s）", rec.Code, rec.Body.String())
	}
	if v := dataField(t, rec.Body.Bytes(), "last_value"); v != nil {
		t.Fatalf("刚建好、还没评估过的规则 last_value 应为 JSON null，实际 %#v", v)
	}
	// 三个次级口径字段同样要么是值、要么是 null，不能是空字符串。
	for _, k := range []string{"secondary_metric", "secondary_comparison", "secondary_threshold"} {
		if v := dataField(t, rec.Body.Bytes(), k); v != nil {
			t.Fatalf("没有次级口径时 %s 应为 JSON null，实际 %#v", k, v)
		}
	}
}

// insertTestEvent 往库里塞一条事件，返回它的 id。
//
// 走 SQL 而不是"跑一轮评估让事件自己冒出来"：确认动作与"事件怎么产生的"无关，
// 用真实评估去造前提只会让这个测试依赖指标样本、时钟和规则状态机三样东西。
func insertTestEvent(t *testing.T, f *adminFixture, state string) int64 {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second).Format(time.RFC3339)
	var resolvedAt any
	var duration any
	if state == alerts.EventResolved {
		resolvedAt = now
		duration = 90
	}
	res, err := f.db.SQL().Exec(`INSERT INTO alert_events
	    (rule_id, rule_name, scope, target_id, metric, comparison, threshold,
	     state, started_at, resolved_at, duration_seconds, peak_value, silenced, created_at)
	    VALUES (NULL, '磁盘快满了', 'filesystem', '/srv', 'used_percent', 'gte', 90,
	            ?, ?, ?, ?, 95.5, 0, ?)`,
		state, now, resolvedAt, duration, now)
	if err != nil {
		t.Fatalf("写入事件失败: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("取事件 id 失败: %v", err)
	}
	return id
}

// TestAlertEventAckIsIdempotentAndKeepsFirstConfirmer 钉住确认动作的三条语义：
// 第一次点记下是谁、什么时候；第二次点什么都不改；确认不动 state。
//
// 第三条尤其重要：确认是"我知道了"，不是"它好了"。如果 ack 顺手把 firing 改成
// resolved，那么面板上"还在报"与"已经恢复"就不再是机器的结论，而是某次点击的
// 副作用——那比没有确认按钮更坏。
func TestAlertEventAckIsIdempotentAndKeepsFirstConfirmer(t *testing.T) {
	f := newAlertsFixture(t)
	id := insertTestEvent(t, f, alerts.EventFiring)

	// 第二个管理员：用来证明第二次确认不会把名字换成后来的人。
	if _, err := f.srv.opts.Auth.CreateUser(context.Background(), auth.CreateUserInput{
		Username: "second-admin", DisplayName: "second-admin", Role: auth.RoleAdmin,
		Password: "second-admin-horse-9",
	}); err != nil {
		t.Fatalf("创建第二个管理员失败: %v", err)
	}

	rec := f.do(t, http.MethodPost, "/api/v1/alerts/events/"+strconv.FormatInt(id, 10)+"/ack", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("确认状态码 = %d，期望 200（%s）", rec.Code, rec.Body.String())
	}
	first, ok := dataField(t, rec.Body.Bytes(), "acked_at").(string)
	if !ok || first == "" {
		t.Fatalf("确认后 acked_at 应是非空字符串，实际 %#v", dataField(t, rec.Body.Bytes(), "acked_at"))
	}
	if got := dataField(t, rec.Body.Bytes(), "acked_by"); got != testUser {
		t.Fatalf("确认人 = %#v，期望 %q", got, testUser)
	}
	if got := dataField(t, rec.Body.Bytes(), "state"); got != alerts.EventFiring {
		t.Fatalf("确认后事件状态 = %#v，期望仍是 firing（确认不改判定）", got)
	}

	// 第二个管理员再确认一次：时间与人都保持第一次的。
	otherCookie, otherCSRF := loginAsUser(t, f.srv, "second-admin", "second-admin-horse-9")
	rec = f.doAs(t, otherCookie, otherCSRF, http.MethodPost,
		"/api/v1/alerts/events/"+strconv.FormatInt(id, 10)+"/ack", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("第二次确认状态码 = %d，期望 200（%s）", rec.Code, rec.Body.String())
	}
	if got := dataField(t, rec.Body.Bytes(), "acked_at"); got != first {
		t.Fatalf("第二次确认后 acked_at 变了：%#v → %#v", first, got)
	}
	if got := dataField(t, rec.Body.Bytes(), "acked_by"); got != testUser {
		t.Fatalf("第二次确认把确认人改成了 %#v，期望保持第一个确认人 %q", got, testUser)
	}

	// 审计里有这一条，且记的是往哪条事件上认领。
	var action, target string
	if err := f.db.SQL().QueryRowContext(t.Context(),
		`SELECT action, target FROM audit_events WHERE action = ? ORDER BY id DESC LIMIT 1`,
		audit.ActionAlertEventAck).Scan(&action, &target); err != nil {
		t.Fatalf("审计里没有确认记录: %v", err)
	}
	if want := "alert_event:" + strconv.FormatInt(id, 10); target != want {
		t.Fatalf("审计 target = %q，期望 %q", target, want)
	}
}

// TestAlertEventAckErrors 覆盖确认的四种拒绝：越权、缺 CSRF、不存在、id 非法。
//
// 不存在的措辞必须是"告警事件不存在"：从 mapAlertError 借那句"告警规则不存在"
// 会把一个点错链接的人送去翻规则列表。
func TestAlertEventAckErrors(t *testing.T) {
	f := newAlertsFixture(t)
	id := strconv.FormatInt(insertTestEvent(t, f, alerts.EventResolved), 10)

	rec := f.do(t, http.MethodPost, "/api/v1/alerts/events/999999/ack", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("不存在的事件状态码 = %d，期望 404（%s）", rec.Code, rec.Body.String())
	}
	if got := parseResult(t, rec); got.Error == nil || got.Error.Message != "告警事件不存在" {
		t.Fatalf("错误消息 = %+v，期望 %q", got.Error, "告警事件不存在")
	}

	rec = f.do(t, http.MethodPost, "/api/v1/alerts/events/abc/ack", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法 id 状态码 = %d，期望 400", rec.Code)
	}

	// 没有 CSRF 令牌：写操作必须被拦在 handler 之前。
	rec = f.doAs(t, f.cookie, "", http.MethodPost, "/api/v1/alerts/events/"+id+"/ack", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("缺 CSRF 状态码 = %d，期望 403", rec.Code)
	}

	// viewer 能看事件列表，但不能认领。
	if _, err := f.srv.opts.Auth.CreateUser(context.Background(), auth.CreateUserInput{
		Username: "watcher", DisplayName: "watcher", Role: auth.RoleViewer, Password: "watcher-horse-9",
	}); err != nil {
		t.Fatalf("创建 viewer 失败: %v", err)
	}
	viewerCookie, viewerCSRF := loginAsUser(t, f.srv, "watcher", "watcher-horse-9")
	rec = f.doAs(t, viewerCookie, viewerCSRF, http.MethodPost, "/api/v1/alerts/events/"+id+"/ack", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer 确认状态码 = %d，期望 403", rec.Code)
	}
	if rec = f.doAs(t, viewerCookie, viewerCSRF, http.MethodGet, "/api/v1/alerts/events", ""); rec.Code != http.StatusOK {
		t.Fatalf("viewer 读事件列表状态码 = %d，期望 200", rec.Code)
	}
}
