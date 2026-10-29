package agentclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// writeQueryResult 回一份 /internal/v1/queries 的成功信封。
func writeQueryResult(w http.ResponseWriter, data string) {
	io.WriteString(w, `{"data":`+data+`,"request_id":"rid"}`)
}

// TestListManagedDeploymentsParses 是主线：列表原样解出来，形状核对通过。
func TestListManagedDeploymentsParses(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/internal/v1/queries" {
			t.Errorf("意外请求 %s %s", r.Method, r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(raw), `"query":"deployments.list"`) {
			t.Errorf("查询名不对：%s", raw)
		}
		writeQueryResult(w, `{"deployments":[
			{"name":"webapp","deployment_id":"dep_abc","project":"webapp",
			 "unit":"upanel-deploy-webapp.service","unit_path":"/etc/systemd/system/upanel-deploy-webapp.service",
			 "config_path":"/etc/upanel/deploy/webapp.compose.yaml","config_present":true,
			 "config_digest":"sha256:aa","version":"sha256:aa","previous_config_present":false,
			 "record_present":true,"config_drift":false,
			 "unit_state":{"known":true,"enabled":true,"active":true}}],
			"skipped":[{"name":"Bad_Name","reason":"名字不合规"}],"truncated":false}`)
	}))

	out, err := c.ListManagedDeployments(context.Background())
	if err != nil {
		t.Fatalf("调用失败：%v", err)
	}
	if len(out.Deployments) != 1 {
		t.Fatalf("条数 = %d", len(out.Deployments))
	}
	item := out.Deployments[0]
	if item.Name != "webapp" || item.Version != "sha256:aa" || !item.Unit.Known || !item.Unit.Active {
		t.Fatalf("现场 = %+v", item)
	}
	if len(out.Skipped) != 1 || out.Skipped[0].Name != "Bad_Name" {
		t.Fatalf("skipped = %+v", out.Skipped)
	}
}

// TestListManagedDeploymentsRequiresField 测"代理没答"与"这台机器上没有部署"的区别：
// 缺 deployments 键是 UnavailableError，空数组是合法结果。
func TestListManagedDeploymentsRequiresField(t *testing.T) {
	empty := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeQueryResult(w, `{"deployments":[],"truncated":false}`)
	}))
	out, err := empty.ListManagedDeployments(context.Background())
	if err != nil {
		t.Fatalf("空列表不该报错：%v", err)
	}
	if out.Deployments == nil || len(out.Deployments) != 0 {
		t.Fatalf("空列表 = %+v", out.Deployments)
	}

	missing := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeQueryResult(w, `{"truncated":false}`)
	}))
	var unavailable *UnavailableError
	if _, err := missing.ListManagedDeployments(context.Background()); !errors.As(err, &unavailable) {
		t.Fatalf("缺 deployments 键期望 UnavailableError，实际 %v", err)
	}
}

// TestListManagedDeploymentsChecksShape 测一条缺 version 的现场：更新与删除都带不回令牌。
func TestListManagedDeploymentsChecksShape(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeQueryResult(w, `{"deployments":[{"name":"webapp","deployment_id":"dep_abc"}],"truncated":false}`)
	}))
	var unavailable *UnavailableError
	if _, err := c.ListManagedDeployments(context.Background()); !errors.As(err, &unavailable) {
		t.Fatalf("缺 version 期望 UnavailableError，实际 %v", err)
	}
}

// TestGetManagedDeploymentParses 与 404 的原样带出。
func TestGetManagedDeployment(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body := string(raw)
		if strings.Contains(body, `"name":"missing"`) {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"error":{"code":"NOT_FOUND","message":"没有这个部署：missing"},"request_id":"rid"}`)
			return
		}
		if !strings.Contains(body, `"query":"deployments.get"`) || !strings.Contains(body, `"name":"webapp"`) {
			t.Errorf("请求不对：%s", body)
		}
		writeQueryResult(w, `{"name":"webapp","deployment_id":"dep_abc","project":"webapp",
			"version":"sha256:aa","config_present":true,"config_digest":"sha256:aa",
			"record_present":true,"unit_state":{"known":true,"enabled":true,"active":false}}`)
	}))

	out, err := c.GetManagedDeployment(context.Background(), "webapp")
	if err != nil {
		t.Fatalf("调用失败：%v", err)
	}
	if out.Name != "webapp" || out.Unit.Enabled != true {
		t.Fatalf("现场 = %+v", out)
	}

	var rejected *Error
	if _, err := c.GetManagedDeployment(context.Background(), "missing"); !errors.As(err, &rejected) {
		t.Fatalf("404 期望 *Error，实际 %v", err)
	} else if rejected.Status != http.StatusNotFound || rejected.Code != "NOT_FOUND" {
		t.Fatalf("拒绝 = %+v", rejected)
	}

	// 不合规的名字在本地就拒，不往返一次。
	if _, err := c.GetManagedDeployment(context.Background(), "Bad Name"); err == nil {
		t.Fatal("非法名字必须在本地拒")
	}
}
