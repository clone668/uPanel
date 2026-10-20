package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"upanel/internal/agentclient"
)

// TestAgentclientTalksToRealAgent 用**真实客户端**打**真实代理处理器**。
//
// 单元测试里客户端一直对着 httptest 手写响应、代理一直对着手写请求，
// 两边的形状是否真的对得上没人验证过——曾经因此漏掉：代理对 POST /secrets 返回 201，
// 而客户端只认 200，真实配对时 PutSecret 必然失败。
// 这里把两端接起来（只替换连接方式：开发宿主不能建 Unix socket），
// 让请求体、状态码、信封解析都走各自的生产代码。
func TestAgentclientTalksToRealAgent(t *testing.T) {
	srv, _ := newPreflightServer(t, nil)

	// 把内核身份注入点固定成“Web 服务账号”，等价于 peerListener 读到 SO_PEERCRED。
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := withPeerInfo(r.Context(), peerInfo{UID: testWebUID, PID: 4242})
		srv.Handler().ServeHTTP(w, r.WithContext(ctx))
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()
	addr := strings.TrimPrefix(ts.URL, "http://")

	c, err := agentclient.New("/run/upanel/agent.sock", agentclient.WithDialer(
		func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", addr)
		}))
	if err != nil {
		t.Fatalf("构造客户端失败: %v", err)
	}

	// 1) 预检：客户端发请求体 → 代理 201 → 客户端解出计划。
	plan, err := c.CreatePreflight(context.Background(), agentclient.PreflightInput{
		Action: "acceptance.probe",
		Params: json.RawMessage(`{"label":"配对测试"}`),
		Actor: agentclient.ActorContext{
			UserID:        7,
			SessionIDHash: strings.Repeat("ab", 32),
			RequestID:     "rid-pairing",
		},
	})
	if err != nil {
		t.Fatalf("真实配对下预检失败: %v", err)
	}
	if plan.Ref == "" || plan.Action != "acceptance.probe" {
		t.Fatalf("计划 = %+v", plan)
	}
	if !plan.Allowed || !plan.RequiresReauth || !plan.RequiresPhrase {
		t.Fatalf("策略字段不对: %+v", plan)
	}
	if !strings.HasPrefix(plan.ConfirmationPhrase, "PROBE ") {
		t.Fatalf("确认短语 = %q", plan.ConfirmationPhrase)
	}
	if len(plan.Warnings) != 1 || plan.Warnings[0].Code != "acceptance_only" {
		t.Fatalf("告警 = %+v", plan.Warnings)
	}
	if len(plan.Locks) != 1 || plan.Locks[0] != "acceptance:probe" {
		t.Fatalf("资源锁 = %v", plan.Locks)
	}

	// 2) 业务拒绝：未登记动作必须是 *agentclient.Error，而不是被当成“代理不可用”。
	_, err = c.CreatePreflight(context.Background(), agentclient.PreflightInput{
		Action: "storage.volume.mount",
		Actor: agentclient.ActorContext{
			UserID:        7,
			SessionIDHash: strings.Repeat("ab", 32),
		},
	})
	var agentErr *agentclient.Error
	if !errors.As(err, &agentErr) {
		t.Fatalf("错误类型 = %T，期望 *agentclient.Error（%v）", err, err)
	}
	if agentErr.Code != "VALIDATION_ERROR" {
		t.Fatalf("错误码 = %q", agentErr.Code)
	}

	// 3) 秘密写入：代理返回 201，客户端必须认。
	secret, err := c.PutSecret(context.Background(), agentclient.PutSecretInput{
		Purpose: "smb_password",
		Value:   "配对测试用的明文",
	})
	if err != nil {
		t.Fatalf("真实配对下写入秘密失败: %v", err)
	}
	if secret.Ref == "" || secret.Purpose != "smb_password" {
		t.Fatalf("秘密响应 = %+v", secret)
	}
}
