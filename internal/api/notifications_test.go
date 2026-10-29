package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"upanel/internal/auth"
	"upanel/internal/certificates"
	"upanel/internal/config"
	"upanel/internal/notifications"
	"upanel/internal/platform"
	"upanel/internal/version"
)

// 通知通道接口的测试：形态（谁能不能调）、校验边界、版本冲突、测试发送的拒绝理由，
// 以及“审计里绝不出现口令”这条硬要求。

func newNotificationsFixture(t *testing.T) *adminFixture {
	t.Helper()
	authSvc, aud, db := newTestServicesWithDB(t)
	notifSvc, err := notifications.New(db, notifications.Options{Logger: quietLogger()})
	if err != nil {
		t.Fatalf("构造通知服务失败: %v", err)
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
		Notifications:     notifSvc,
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	seedTestUser(t, srv.opts.Auth, testUser, testUserPass)
	cookie, csrf := loginOK(t, srv)
	return &adminFixture{srv: srv, cookie: cookie, csrf: csrf, db: db}
}

func smtpChannelBody() map[string]any {
	return map[string]any{
		"name": "运维邮件",
		"type": "smtp",
		"settings": map[string]any{
			"host": "smtp.example.com",
			"port": 587,
			"tls":  "starttls",
			"from": "panel@example.com",
			"to":   []string{"ops@example.com"},
		},
		"enabled":      true,
		"max_attempts": 3,
	}
}

func decodeChannel(t *testing.T, rec *httptest.ResponseRecorder) notificationChannelDTO {
	t.Helper()
	var body struct {
		Data notificationChannelDTO `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v（%s）", err, rec.Body.String())
	}
	return body.Data
}

// TestNotificationChannelsUnavailableWithoutService：没装配时明确回 503。
func TestNotificationChannelsUnavailableWithoutService(t *testing.T) {
	f := newAdminFixture(t) // 没有装 Notifications
	rec := f.do(t, http.MethodGet, "/api/v1/notification-channels", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d，期望 503", rec.Code)
	}
	if got := parseResult(t, rec); got.Error == nil || got.Error.Code != "NOT_IMPLEMENTED" {
		t.Fatalf("错误码 = %+v", got.Error)
	}
}

// TestNotificationChannelsRequireSession：所有端点都要登录。
func TestNotificationChannelsRequireSession(t *testing.T) {
	f := newNotificationsFixture(t)
	for _, req := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/notification-channels"},
		{http.MethodPost, "/api/v1/notification-channels"},
		{http.MethodPatch, "/api/v1/notification-channels/1"},
		{http.MethodDelete, "/api/v1/notification-channels/1"},
		{http.MethodPost, "/api/v1/notification-channels/1/test"},
	} {
		rec := f.doAs(t, nil, "", req.method, req.path, "")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s 未登录状态码 = %d，期望 401", req.method, req.path, rec.Code)
		}
	}
}

// TestNotificationChannelsAdminOnly：读也必须是 admin——通道里带着“告警发到哪”。
func TestNotificationChannelsAdminOnly(t *testing.T) {
	f := newNotificationsFixture(t)
	if _, err := f.srv.opts.Auth.CreateUser(t.Context(), auth.CreateUserInput{
		Username: "watcher", DisplayName: "watcher", Role: auth.RoleViewer, Password: "watcher-horse-9",
	}); err != nil {
		t.Fatalf("创建 viewer 失败: %v", err)
	}
	viewerCookie, viewerCSRF := loginAsUser(t, f.srv, "watcher", "watcher-horse-9")

	rec := f.doAs(t, viewerCookie, viewerCSRF, http.MethodGet, "/api/v1/notification-channels", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer 读通道状态码 = %d，期望 403", rec.Code)
	}
	rec = f.doAs(t, viewerCookie, viewerCSRF, http.MethodPost, "/api/v1/notification-channels",
		jsonBody(t, smtpChannelBody()))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer 建通道状态码 = %d，期望 403", rec.Code)
	}
}

// TestNotificationChannelCreateListAndTarget：建一条 smtp 通道，列表里能读到，
// target 是收件人列表的人读形式，secret_ref 为 null（没配鉴权）。
func TestNotificationChannelCreateListAndTarget(t *testing.T) {
	f := newNotificationsFixture(t)

	rec := f.do(t, http.MethodPost, "/api/v1/notification-channels", jsonBody(t, smtpChannelBody()))
	if rec.Code != http.StatusCreated {
		t.Fatalf("建通道状态码 = %d，期望 201（%s）", rec.Code, rec.Body.String())
	}
	created := decodeChannel(t, rec)
	if created.Target != "ops@example.com" {
		t.Fatalf("target = %q，期望收件人列表", created.Target)
	}
	if created.SecretRef != nil {
		t.Fatalf("没配鉴权时 secret_ref 应为 null，实际 %q", *created.SecretRef)
	}
	if created.MaxAttempts != 3 || !created.Enabled || created.Version != 1 {
		t.Fatalf("新建通道的默认值不对: %+v", created)
	}

	rec = f.do(t, http.MethodGet, "/api/v1/notification-channels", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("列表状态码 = %d，期望 200", rec.Code)
	}
	var listed struct {
		Data []notificationChannelDTO `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("列表不是合法 JSON: %v", err)
	}
	// 迁移种的 in_app + 刚建的 smtp。
	if len(listed.Data) != 2 {
		t.Fatalf("应有两條通道（in_app + smtp），实际 %d 条", len(listed.Data))
	}
}

// TestNotificationChannelValidation：非法配置在接口层就被拒，并留一条被拒审计。
func TestNotificationChannelValidation(t *testing.T) {
	f := newNotificationsFixture(t)

	bad := smtpChannelBody()
	bad["settings"] = map[string]any{"url": "http://hooks.example.com/x"}
	rec := f.do(t, http.MethodPost, "/api/v1/notification-channels", jsonBody(t, bad))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("http 回调状态码 = %d，期望 400（%s）", rec.Code, rec.Body.String())
	}
	if got := parseResult(t, rec); got.Error == nil || got.Error.Code != "VALIDATION_ERROR" {
		t.Fatalf("错误码 = %+v", got.Error)
	}

	// 缺 type。
	noType := smtpChannelBody()
	delete(noType, "type")
	rec = f.do(t, http.MethodPost, "/api/v1/notification-channels", jsonBody(t, noType))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("缺 type 状态码 = %d，期望 400", rec.Code)
	}

	// 第二条 in_app。
	rec = f.do(t, http.MethodPost, "/api/v1/notification-channels",
		jsonBody(t, map[string]any{"name": "第二个收件箱", "type": "in_app"}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("第二条 in_app 状态码 = %d，期望 400（%s）", rec.Code, rec.Body.String())
	}
}

// TestNotificationChannelUpdateConflict 与删除：
func TestNotificationChannelUpdateAndDelete(t *testing.T) {
	f := newNotificationsFixture(t)

	created := decodeChannel(t, f.do(t, http.MethodPost, "/api/v1/notification-channels",
		jsonBody(t, smtpChannelBody())))

	// 版本对不上 → 409。
	body := map[string]any{"name": "改名", "version": 99}
	rec := f.do(t, http.MethodPatch, "/api/v1/notification-channels/"+itoaInt(created.ID), jsonBody(t, body))
	if rec.Code != http.StatusConflict {
		t.Fatalf("旧版本号状态码 = %d，期望 409（%s）", rec.Code, rec.Body.String())
	}

	// 版本对得上 → 200，version 递增。
	body = map[string]any{"name": "运维邮件（停用）", "enabled": false, "version": created.Version}
	rec = f.do(t, http.MethodPatch, "/api/v1/notification-channels/"+itoaInt(created.ID), jsonBody(t, body))
	if rec.Code != http.StatusOK {
		t.Fatalf("改通道状态码 = %d，期望 200（%s）", rec.Code, rec.Body.String())
	}
	updated := decodeChannel(t, rec)
	if updated.Enabled || updated.Name != "运维邮件（停用）" || updated.Version != 2 {
		t.Fatalf("改后不对: %+v", updated)
	}
	// 没提的字段要留着（setting 整体替换只发生在显式给出时）。
	if updated.Settings.Host != "smtp.example.com" {
		t.Fatalf("没提 settings 却被清掉了: %+v", updated.Settings)
	}

	// 删除 → 200，再读列表只剩 in_app。
	rec = f.do(t, http.MethodDelete, "/api/v1/notification-channels/"+itoaInt(created.ID), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("删除状态码 = %d，期望 200（%s）", rec.Code, rec.Body.String())
	}
	rec = f.do(t, http.MethodDelete, "/api/v1/notification-channels/"+itoaInt(created.ID), "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("再删一次状态码 = %d，期望 404", rec.Code)
	}
}

// TestNotificationChannelInAppNotDeletable：in_app 是面板自己的收件箱，删不掉。
func TestNotificationChannelInAppNotDeletable(t *testing.T) {
	f := newNotificationsFixture(t)

	rec := f.do(t, http.MethodGet, "/api/v1/notification-channels", "")
	var listed struct {
		Data []notificationChannelDTO `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("列表不是合法 JSON: %v", err)
	}
	var inAppID int64
	for _, ch := range listed.Data {
		if ch.Type == "in_app" {
			inAppID = ch.ID
		}
	}
	if inAppID == 0 {
		t.Fatal("迁移应种下一条 in_app 通道")
	}
	rec = f.do(t, http.MethodDelete, "/api/v1/notification-channels/"+itoaInt(inAppID), "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("删 in_app 状态码 = %d，期望 400（%s）", rec.Code, rec.Body.String())
	}
}

// TestNotificationTestSendRefusals：测试发送的三种拒绝理由各自不同。
//
//   - in_app：没有外部动作可做 → 400；
//   - 没有发送端（演示模式）：代理不可用 → 503 AGENT_UNAVAILABLE；
//   - 通道不存在 → 404。
func TestNotificationTestSendRefusals(t *testing.T) {
	f := newNotificationsFixture(t)

	rec := f.do(t, http.MethodGet, "/api/v1/notification-channels", "")
	var listed struct {
		Data []notificationChannelDTO `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("列表不是合法 JSON: %v", err)
	}
	var inAppID int64
	for _, ch := range listed.Data {
		if ch.Type == "in_app" {
			inAppID = ch.ID
		}
	}

	rec = f.do(t, http.MethodPost, "/api/v1/notification-channels/"+itoaInt(inAppID)+"/test", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("in_app 测试发送状态码 = %d，期望 400（%s）", rec.Code, rec.Body.String())
	}

	created := decodeChannel(t, f.do(t, http.MethodPost, "/api/v1/notification-channels",
		jsonBody(t, smtpChannelBody())))
	// 这个固定装置没有 SetSender：投递会明确说“代理不可用”，而不是记一次失败。
	rec = f.do(t, http.MethodPost, "/api/v1/notification-channels/"+itoaInt(created.ID)+"/test", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("没有发送端时状态码 = %d，期望 503（%s）", rec.Code, rec.Body.String())
	}
	if got := parseResult(t, rec); got.Error == nil || got.Error.Code != "AGENT_UNAVAILABLE" {
		t.Fatalf("错误码 = %+v", got.Error)
	}

	rec = f.do(t, http.MethodPost, "/api/v1/notification-channels/999/tests", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("不存在的路径状态码 = %d，期望 404", rec.Code)
	}
	rec = f.do(t, http.MethodPost, "/api/v1/notification-channels/999/test", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("不存在的通道状态码 = %d，期望 404（%s）", rec.Code, rec.Body.String())
	}
}

// TestNotificationChannelAuditHasNoSecrets：审计里只记 secret_ref，从不记口令。
//
// 这一条用真实审计表验：建一条带 secret_ref 的 smtp 通道，然后翻审计详情，
// 里面必须出现这个引用、且不含任何“口令”字样的键。
func TestNotificationChannelAuditHasNoSecrets(t *testing.T) {
	f := newNotificationsFixture(t)

	body := smtpChannelBody()
	body["secret_ref"] = "sec_0123456789abcdef0123456789abcdef"
	body["settings"].(map[string]any)["username"] = "panel"
	rec := f.do(t, http.MethodPost, "/api/v1/notification-channels", jsonBody(t, body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("建通道状态码 = %d，期望 201（%s）", rec.Code, rec.Body.String())
	}

	var details string
	if err := f.db.SQL().QueryRowContext(t.Context(),
		`SELECT redacted_details FROM audit_events WHERE action = 'notification.channel.create'
		  ORDER BY id DESC LIMIT 1`).Scan(&details); err != nil {
		t.Fatalf("读审计失败: %v", err)
	}
	// secret_ref 这个**键**要留下（“这条通道用的是哪份凭据”是审计要回答的），
	// 但值是红字：审计写入器把秘密键名一律替换成 [redacted]，引用本身也只是引用。
	if !strings.Contains(details, `"secret_ref":"[redacted]"`) {
		t.Fatalf("审计里应留下 secret_ref 这个键并做红字处理: %s", details)
	}
	if strings.Contains(details, "sec_0123456789abcdef0123456789abcdef") {
		t.Fatalf("审计详情里出现了秘密引用本身: %s", details)
	}
	for _, banned := range []string{"password", "口令", "secret_value"} {
		if strings.Contains(details, banned) {
			t.Fatalf("审计详情里出现了 %q: %s", banned, details)
		}
	}
}

// insertTestDelivery 往投递账里塞一行，返回它的 id。
func insertTestDelivery(t *testing.T, f *adminFixture, kind, state string, attempts int, nextAt int64,
	lastError string, sentAt any) int64 {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second).Format(time.RFC3339)
	res, err := f.db.SQL().Exec(`INSERT INTO notification_deliveries
	    (channel_id, channel_name, channel_type, target, event_id, event_rule, kind, title, body,
	     state, attempts, max_attempts, next_attempt_at, last_error, last_status, sent_at, created_at)
	    VALUES (NULL, '运维邮件', 'smtp', 'ops@example.com', NULL, '磁盘快满了', ?, ?, '磁盘 /srv 用了 95%',
	            ?, ?, 3, ?, ?, 0, ?, ?)`,
		kind, "磁盘快满了", state, attempts, nextAt, lastError, sentAt, now)
	if err != nil {
		t.Fatalf("写入投递失败: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("取投递 id 失败: %v", err)
	}
	return id
}

// TestNotificationDeliveriesListAndFilter 覆盖投递账的读路径（P13.4 的面板收件箱）：
// 三种状态都读得出来、next_attempt_at 在"不再排期"时是 null 而不是 0、
// 通道被删之后快照字段照旧可读、state/kind 的非法值回 400 而不是静默忽略。
func TestNotificationDeliveriesListAndFilter(t *testing.T) {
	f := newNotificationsFixture(t)

	// 三行：已发出（next_attempt_at=0 表示不再排期）、等重试（还没到点）、已判死。
	sentID := insertTestDelivery(t, f, "fired", "sent", 1, 0, "", time.Now().UTC().Format(time.RFC3339))
	insertTestDelivery(t, f, "resolved", "pending", 1, time.Now().Add(time.Minute).Unix(), "timeout", nil)
	failedID := insertTestDelivery(t, f, "fired", "failed", 3, 0, "550 5.1.1 user unknown", nil)

	rec := f.do(t, http.MethodGet, "/api/v1/notification-deliveries?limit=10", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("投递账状态码 = %d，期望 200（%s）", rec.Code, rec.Body.String())
	}
	var body struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if len(body.Data) != 3 {
		t.Fatalf("投递行数 = %d，期望 3（%s）", len(body.Data), rec.Body.String())
	}
	byID := map[int64]map[string]any{}
	for _, d := range body.Data {
		byID[int64(d["id"].(float64))] = d
	}

	// 已发出的那行：next_attempt_at 必须是 null —— 0 会被界面读成"马上要重试"。
	sent, ok := byID[sentID]
	if !ok {
		t.Fatalf("列表里没有已发出的那行: %s", rec.Body.String())
	}
	if v := sent["next_attempt_at"]; v != nil {
		t.Fatalf("已发出的投递 next_attempt_at 应为 JSON null，实际 %#v", v)
	}
	if v, isStr := sent["sent_at"].(string); !isStr || v == "" {
		t.Fatalf("已发出的投递 sent_at 应是非空字符串，实际 %#v", sent["sent_at"])
	}
	// 通道已被删（channel_id 为 NULL）：快照必须还在，否则历史读不出来。
	if got := sent["channel_name"]; got != "运维邮件" {
		t.Fatalf("channel_name = %#v，期望快照值 %q", got, "运维邮件")
	}
	if got := sent["channel_id"]; got != float64(0) {
		t.Fatalf("通道已删时 channel_id = %#v，期望 0", got)
	}

	// 对端原话要原样出来：那是运维唯一能据以动手的信息。
	failed, ok := byID[failedID]
	if !ok {
		t.Fatalf("列表里没有判死的那行: %s", rec.Body.String())
	}
	if got := failed["last_error"]; got != "550 5.1.1 user unknown" {
		t.Fatalf("last_error = %#v，期望保留对端原话", got)
	}
	if got := failed["attempts"]; got != float64(3) {
		t.Fatalf("attempts = %#v，期望 3", got)
	}

	// 过滤：state=failed 只剩一行；kind 是闭集，非法值必须 400。
	rec = f.do(t, http.MethodGet, "/api/v1/notification-deliveries?state=failed", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("按 state 过滤状态码 = %d，期望 200", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if len(body.Data) != 1 || body.Data[0]["state"] != "failed" {
		t.Fatalf("state=failed 过滤结果不对: %s", rec.Body.String())
	}
	rec = f.do(t, http.MethodGet, "/api/v1/notification-deliveries?kind=bogus", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法 kind 状态码 = %d，期望 400（%s）", rec.Code, rec.Body.String())
	}
	rec = f.do(t, http.MethodGet, "/api/v1/notification-deliveries?limit=0", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("limit=0 状态码 = %d，期望 400", rec.Code)
	}
}

// TestNotificationDeliveriesAdminOnly：投递账里带着收件邮箱与 webhook 地址，
// 与通道列表同级——viewer 读不到，未登录更是。
func TestNotificationDeliveriesAdminOnly(t *testing.T) {
	f := newNotificationsFixture(t)

	rec := f.doAs(t, nil, "", http.MethodGet, "/api/v1/notification-deliveries", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("未登录状态码 = %d，期望 401", rec.Code)
	}
	if _, err := f.srv.opts.Auth.CreateUser(t.Context(), auth.CreateUserInput{
		Username: "watcher", DisplayName: "watcher", Role: auth.RoleViewer, Password: "watcher-horse-9",
	}); err != nil {
		t.Fatalf("创建 viewer 失败: %v", err)
	}
	viewerCookie, viewerCSRF := loginAsUser(t, f.srv, "watcher", "watcher-horse-9")
	rec = f.doAs(t, viewerCookie, viewerCSRF, http.MethodGet, "/api/v1/notification-deliveries", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer 读投递账状态码 = %d，期望 403", rec.Code)
	}
}

// TestNotificationDeliveriesUnavailableWithoutService：没装配通知时回 503，
// 不是一份看起来"从没发过通知"的空列表。
func TestNotificationDeliveriesUnavailableWithoutService(t *testing.T) {
	f := newAdminFixture(t)
	rec := f.do(t, http.MethodGet, "/api/v1/notification-deliveries", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d，期望 503（%s）", rec.Code, rec.Body.String())
	}
}
