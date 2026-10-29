package api

// P15.4 接口层用例：管理面的部署读取接口（/api/v1/deployments/managed*）。
//
// 这里验的是 HTTP 层的四件事，不冒充 docker 也不冒充 systemd：
//   - 权限：admin 才有。部署定义里有镜像、宿主端口、挂载路径与环境变量名，
//     属于这台机器的侦察情报（PLAN §14）。
//   - 路由：/deployments/managed 不能被 /deployments/:id 吃掉——那是观测面的
//     单条部署接口，两个接口回答的不是同一个问题。
//   - 参数：不合规的部署名在本地就 400，不往返一次。
//   - 错误映射：代理说"没有这份部署"就是 404，连不上是 503，读盘失败才是 500。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"upanel/internal/agentclient"
	"upanel/internal/auth"
	"upanel/internal/certificates"
	"upanel/internal/config"
	"upanel/internal/deploy"
	"upanel/internal/platform"
	"upanel/internal/version"
)

// stubDeployments 是 DeploymentsReader 的测试替身。
type stubDeployments struct {
	list    deploy.InspectionList
	listErr error

	item   deploy.Inspection
	getErr error
	asked  []string
}

func (s *stubDeployments) ListManagedDeployments(context.Context) (deploy.InspectionList, error) {
	if s.listErr != nil {
		return deploy.InspectionList{}, s.listErr
	}
	return s.list, nil
}

func (s *stubDeployments) GetManagedDeployment(_ context.Context, name string) (deploy.Inspection, error) {
	s.asked = append(s.asked, name)
	if s.getErr != nil {
		return deploy.Inspection{}, s.getErr
	}
	return s.item, nil
}

func sampleInspection() deploy.Inspection {
	at := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	digest := "sha256:" + strings.Repeat("a", 64)
	return deploy.Inspection{
		Name:          "webapp",
		DeploymentID:  deploy.DeploymentIDForName("webapp"),
		ProjectName:   "webapp",
		UnitName:      "upanel-deploy-webapp.service",
		UnitPath:      "/etc/systemd/system/upanel-deploy-webapp.service",
		ConfigPath:    "/etc/upanel/deploy/webapp.compose.yaml",
		ConfigPresent: true,
		ConfigDigest:  digest,
		Version:       digest,
		RecordPresent: true,
		Record: &deploy.DeploymentRecord{
			Schema:       deploy.RecordSchema,
			DeploymentID: deploy.DeploymentIDForName("webapp"),
			Name:         "webapp",
			Definition: deploy.Definition{
				Name:           "webapp",
				ImageReference: "nginx:1.27",
				ContainerPort:  8080,
				HostPort:       18080,
			},
			ConfigDigest: digest,
			Image: deploy.ImageRecord{Requested: "nginx:1.27",
				Digest: "sha256:" + strings.Repeat("b", 64), ConfigDigest: digest, ResolvedAt: at},
			UpdatedAt: at,
		},
		Unit: deploy.UnitState{Known: true, Enabled: true, Active: true},
	}
}

func newStubDeployments() *stubDeployments {
	item := sampleInspection()
	return &stubDeployments{
		list: deploy.InspectionList{Deployments: []deploy.Inspection{item}},
		item: item,
	}
}

func deploymentsFixture(t *testing.T, reader DeploymentsReader) (*Server, *http.Cookie) {
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
		Deployments:       reader,
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	seedTestUser(t, srv.opts.Auth, testUser, testUserPass)
	cookie, _ := loginOK(t, srv)
	return srv, cookie
}

// TestManagedDeploymentsRequiresAdmin 未登录 401、viewer 403。
func TestManagedDeploymentsRequiresAdmin(t *testing.T) {
	srv, _ := deploymentsFixture(t, newStubDeployments())
	paths := []string{"/api/v1/deployments/managed", "/api/v1/deployments/managed/webapp"}
	for _, path := range paths {
		rec := adminGet(t, srv, nil, path)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s 未登录状态码 = %d，期望 401（%s）", path, rec.Code, rec.Body.String())
		}
	}
	if _, err := srv.opts.Auth.CreateUser(context.Background(), auth.CreateUserInput{
		Username: "watcher", DisplayName: "watcher", Role: auth.RoleViewer, Password: "watcher-horse-9",
	}); err != nil {
		t.Fatalf("创建 viewer 失败: %v", err)
	}
	cookie, _ := loginAsUser(t, srv, "watcher", "watcher-horse-9")
	for _, path := range paths {
		rec := adminGet(t, srv, cookie, path)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s viewer 状态码 = %d，期望 403（%s）", path, rec.Code, rec.Body.String())
		}
	}
}

// TestManagedDeploymentsList 走通列表，并钉住路由：静态段优先于 :id。
func TestManagedDeploymentsList(t *testing.T) {
	srv, cookie := deploymentsFixture(t, newStubDeployments())
	rec := adminGet(t, srv, cookie, "/api/v1/deployments/managed")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（%s）", rec.Code, rec.Body.String())
	}
	var body struct {
		Data deploy.InspectionList `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if len(body.Data.Deployments) != 1 || body.Data.Deployments[0].Name != "webapp" {
		t.Fatalf("列表 = %+v", body.Data.Deployments)
	}
	if body.Data.Deployments[0].Record == nil || body.Data.Deployments[0].Record.Definition.ImageReference != "nginx:1.27" {
		t.Fatalf("定义没透出：%+v", body.Data.Deployments[0].Record)
	}
}

// TestManagedDeploymentsListEmptyIsArray 空列表必须是 []，不能是 null。
func TestManagedDeploymentsListEmptyIsArray(t *testing.T) {
	src := &stubDeployments{list: deploy.InspectionList{}}
	srv, cookie := deploymentsFixture(t, src)
	rec := adminGet(t, srv, cookie, "/api/v1/deployments/managed")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"deployments":[]`)) {
		t.Fatalf("空列表不是 []：%s", rec.Body.String())
	}
}

// TestManagedDeploymentDetail 走通单条，并验不合规名字在本地被拒。
func TestManagedDeploymentDetail(t *testing.T) {
	src := newStubDeployments()
	srv, cookie := deploymentsFixture(t, src)
	rec := adminGet(t, srv, cookie, "/api/v1/deployments/managed/webapp")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
	var body struct {
		Data deploy.Inspection `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if body.Data.Name != "webapp" || body.Data.Version == "" || !body.Data.Unit.Enabled {
		t.Fatalf("现场 = %+v", body.Data)
	}

	for _, name := range []string{"WebApp", "a.b", "-x", "webapp%20x"} {
		rec := adminGet(t, srv, cookie, "/api/v1/deployments/managed/"+name)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s 状态码 = %d，期望 400（%s）", name, rec.Code, rec.Body.String())
		}
	}
	// 编码过的斜杠到不了 handler：路径在匹配之前就被规范化掉，落到 404。
	// 这里断言"不是 200 且没问过读取端"——防的是路径穿越，不是某一个状态码。
	rec = adminGet(t, srv, cookie, "/api/v1/deployments/managed/..%2Fetc")
	if rec.Code == http.StatusOK {
		t.Fatalf("编码斜杠被当成了部署名：%s", rec.Body.String())
	}
	// 非法名字不该走到读取端。
	if len(src.asked) != 1 || src.asked[0] != "webapp" {
		t.Fatalf("读取端收到的名字 = %v", src.asked)
	}
}

// TestManagedDeploymentErrorMapping 三种错误三种状态码。
func TestManagedDeploymentErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
		code string
	}{
		{"没有这份部署", &agentclient.Error{Status: http.StatusNotFound,
			Code: "NOT_FOUND", Message: "没有这个部署：webapp"}, http.StatusNotFound, "NOT_FOUND"},
		{"代理连不上", &agentclient.UnavailableError{Err: errors.New("dial unix: no such file")},
			http.StatusServiceUnavailable, "AGENT_UNAVAILABLE"},
		{"别的错误", errors.New("解析失败"), http.StatusInternalServerError, "INTERNAL_ERROR"},
	}
	for _, tc := range cases {
		src := newStubDeployments()
		src.getErr = tc.err
		srv, cookie := deploymentsFixture(t, src)
		rec := adminGet(t, srv, cookie, "/api/v1/deployments/managed/webapp")
		if rec.Code != tc.want {
			t.Fatalf("%s 状态码 = %d，期望 %d（%s）", tc.name, rec.Code, tc.want, rec.Body.String())
		}
		var body struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s 错误响应不是合法 JSON: %v", tc.name, err)
		}
		if body.Error.Code != tc.code {
			t.Fatalf("%s 错误码 = %q，期望 %q", tc.name, body.Error.Code, tc.code)
		}
	}
}

// TestManagedDeploymentsUnavailableWithoutReader 没有代理连接时 503，不是空列表。
func TestManagedDeploymentsUnavailableWithoutReader(t *testing.T) {
	srv, cookie := deploymentsFixture(t, nil)
	for _, path := range []string{"/api/v1/deployments/managed", "/api/v1/deployments/managed/webapp"} {
		rec := adminGet(t, srv, cookie, path)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s 状态码 = %d，期望 503（%s）", path, rec.Code, rec.Body.String())
		}
	}
}
