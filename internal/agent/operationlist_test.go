package agent

// 定时操作的对账读口（P12.4）。
//
// 这条路由只回答一个问题："定时触发这段时间跑了什么"。断言的重点：
//  1. actor 只接受 scheduler——别的值一律 400，这里不是一张可以随便翻的流水账；
//  2. 只返回定时触发那个保留操作者的记录，普通用户的操作不会混进来；
//  3. since / limit 的形状校验（坏时间、非法数、上限 clamp）；
//  4. 只读：POST 之外的方法在这里是 405（POST 走的是提交那条老路）。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"upanel/internal/agentstore"
	"upanel/internal/version"
)

// seedScheduledOperation 用真实的预检 + 接受路径写一条操作记录。
//
// 不走裸 INSERT：那样读口能过、但"这条记录本来能不能存在"就没被验过。
func seedScheduledOperation(t *testing.T, plans *agentstore.PreflightPlans, ops *agentstore.Operations, id string, userID int64, at time.Time) {
	t.Helper()
	ref := agentstore.PlanRefPrefix + hexDigest32(id)
	plan := agentstore.PreflightPlan{
		ID:                  ref,
		Action:              "backup.run",
		Params:              []byte(`{"backup_policy_id":"pol_0123456789abcdef0123456789abcdef","version":4}`),
		ParamsHash:          hexDigest64("params-" + id),
		Target:              []byte(`{"display_name":"夜间根文件","identity_fingerprint":"fp","state_version":"4"}`),
		IdentityFingerprint: "fp",
		StateVersion:        "4",
		Allowed:             true,
		Blockers:            []byte(`[]`),
		Warnings:            []byte(`[]`),
		Impacts:             []byte(`[]`),
		Locks:               []byte(`["backup:policy"]`),
		UserID:              userID,
		SessionHash:         hexDigest64("session-" + id),
		RequestID:           "rid-plan",
		CreatedAt:           at,
		ExpiresAt:           at.Add(120 * time.Second),
	}
	if err := plans.Insert(context.Background(), plan); err != nil {
		t.Fatalf("写入预检计划失败: %v", err)
	}
	_, _, err := ops.Accept(context.Background(), agentstore.Operation{
		ID:                  id,
		RequestHash:         hexDigest64("request-" + id),
		Action:              plan.Action,
		PreflightRef:        plan.ID,
		Params:              plan.Params,
		ParamsHash:          plan.ParamsHash,
		IdentityFingerprint: plan.IdentityFingerprint,
		StateVersion:        plan.StateVersion,
		Target:              plan.Target,
		Locks:               plan.Locks,
		UserID:              userID,
		SessionHash:         plan.SessionHash,
		RequestID:           "rid-op",
		CreatedAt:           at,
		UpdatedAt:           at,
	})
	if err != nil {
		t.Fatalf("接受操作失败: %v", err)
	}
	if err := ops.Finish(context.Background(), id, agentstore.StageSucceeded, "done", []byte(`{}`), []byte(`{}`), at.Add(time.Minute)); err != nil {
		t.Fatalf("结束操作失败: %v", err)
	}
}

// hexDigest32 / hexDigest64 返回形态合法的十六进制值（由 seed 派生，稳定可复现）。
func hexDigest32(seed string) string { return hexDigest64(seed)[:32] }

func hexDigest64(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])
}

// scheduledListServer 是一个带真实仓储的代理服务。
func scheduledListServer(t *testing.T) (*Server, *agentstore.PreflightPlans, *agentstore.Operations) {
	t.Helper()
	db := newTestAgentDB(t)
	plans := newTestPlanStoreOn(t, db)
	ops := newTestOperationStoreOn(t, db)
	srv, err := New(Options{
		SocketPath:      socketPathForTest(t),
		Version:         version.Info{Version: "test", Commit: "test", BuildDate: "test", GoVersion: "test"},
		Logger:          quietLogger(),
		ShutdownTimeout: time.Second,
		Peer:            testPeer(),
		Secrets:         newTestSecretStore(t),
		Plans:           plans,
		Operations:      ops,
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	return srv, plans, ops
}

// TestListScheduledOperationsReturnsOnlyScheduler 只返回定时触发那个操作者的记录。
func TestListScheduledOperationsReturnsOnlyScheduler(t *testing.T) {
	srv, plans, ops := scheduledListServer(t)
	base := time.Date(2026, 10, 23, 19, 30, 0, 0, time.UTC)
	seedScheduledOperation(t, plans, ops, "op_"+strings.Repeat("1", 32), SchedulerUserID, base)
	seedScheduledOperation(t, plans, ops, "op_"+strings.Repeat("2", 32), 7, base.Add(time.Minute))

	req := httptest.NewRequest(http.MethodGet, "/internal/v1/operations?actor=scheduler", nil)
	rec := serveAs(srv, req, testWebUID)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，响应 = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data scheduledOperationListResponse `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if len(body.Data.Operations) != 1 {
		t.Fatalf("返回了 %d 条，期望 1（只有定时触发那条）", len(body.Data.Operations))
	}
	got := body.Data.Operations[0]
	if got.OperationID != "op_"+strings.Repeat("1", 32) {
		t.Fatalf("operation_id = %s", got.OperationID)
	}
	// Web 要凭这些字段如实落一条预检与一条任务。
	if got.PreflightRef == "" || got.ParamsHash == "" || got.SessionHash == "" || got.StateVersion != "4" {
		t.Fatalf("字段不全：%+v", got)
	}
	if got.CreatedAt != "2026-10-23T19:30:00Z" {
		t.Fatalf("created_at = %s", got.CreatedAt)
	}
	if string(got.Params) != `{}` && !strings.Contains(string(got.Params), "backup_policy_id") {
		t.Fatalf("params = %s", got.Params)
	}
}

// TestListScheduledOperationsActorGate：actor 只接受 scheduler。
func TestListScheduledOperationsActorGate(t *testing.T) {
	srv, plans, ops := scheduledListServer(t)
	seedScheduledOperation(t, plans, ops, "op_"+strings.Repeat("1", 32), SchedulerUserID, time.Now().UTC())

	for _, query := range []string{"", "?actor=", "?actor=all", "?actor=7"} {
		req := httptest.NewRequest(http.MethodGet, "/internal/v1/operations"+query, nil)
		rec := serveAs(srv, req, testWebUID)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("query=%q 状态码 = %d，期望 400", query, rec.Code)
		}
	}
}

// TestListScheduledOperationsParamShape：since 与 limit 的形状。
func TestListScheduledOperationsParamShape(t *testing.T) {
	srv, _, _ := scheduledListServer(t)

	for _, query := range []string{
		"?actor=scheduler&since=昨天",
		"?actor=scheduler&limit=0",
		"?actor=scheduler&limit=-3",
		"?actor=scheduler&limit=many",
	} {
		req := httptest.NewRequest(http.MethodGet, "/internal/v1/operations"+query, nil)
		rec := serveAs(srv, req, testWebUID)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("query=%q 状态码 = %d，期望 400（%s）", query, rec.Code, rec.Body.String())
		}
	}

	// limit 超过上限是 clamp，不是错误：调用方要的是"这一批"。
	req := httptest.NewRequest(http.MethodGet, "/internal/v1/operations?actor=scheduler&limit=9999", nil)
	rec := serveAs(srv, req, testWebUID)
	if rec.Code != http.StatusOK {
		t.Fatalf("超上限的 limit 状态码 = %d，期望 200", rec.Code)
	}
}

// TestListScheduledOperationsSinceFilters：since 是"从这一刻起（含）"。
func TestListScheduledOperationsSinceFilters(t *testing.T) {
	srv, plans, ops := scheduledListServer(t)
	base := time.Date(2026, 10, 20, 3, 0, 0, 0, time.UTC)
	seedScheduledOperation(t, plans, ops, "op_"+strings.Repeat("1", 32), SchedulerUserID, base)
	seedScheduledOperation(t, plans, ops, "op_"+strings.Repeat("2", 32), SchedulerUserID, base.Add(24*time.Hour))

	req := httptest.NewRequest(http.MethodGet,
		"/internal/v1/operations?actor=scheduler&since="+base.Add(24*time.Hour).Format(time.RFC3339), nil)
	rec := serveAs(srv, req, testWebUID)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，响应 = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data scheduledOperationListResponse `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if len(body.Data.Operations) != 1 || body.Data.Operations[0].OperationID != "op_"+strings.Repeat("2", 32) {
		t.Fatalf("按 since 过滤失败：%+v", body.Data.Operations)
	}
}

// TestListScheduledOperationsRejectsOtherMethods：这条集合路由只多了一个 GET。
func TestListScheduledOperationsRejectsOtherMethods(t *testing.T) {
	srv, _, _ := scheduledListServer(t)
	req := httptest.NewRequest(http.MethodDelete, "/internal/v1/operations?actor=scheduler", nil)
	rec := serveAs(srv, req, testWebUID)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("状态码 = %d，期望 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); !strings.Contains(allow, "POST") || !strings.Contains(allow, "GET") {
		t.Fatalf("Allow = %q，期望同时列出 GET 与 POST", allow)
	}
}

// TestListScheduledOperationsRejectsRoot：普通路由只接受 Web 账号，root 也进不来。
func TestListScheduledOperationsRejectsRoot(t *testing.T) {
	srv, _, _ := scheduledListServer(t)
	req := httptest.NewRequest(http.MethodGet, "/internal/v1/operations?actor=scheduler", nil)
	rec := serveAs(srv, req, 0)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("root 状态码 = %d，期望 403", rec.Code)
	}
}
