package api

// P16.1 接口层用例：GET /api/v1/system/capabilities。
//
// 验四件事：
//   - 权限：admin 才有（这份清单是主机指纹）；未登录 401、viewer 403。
//   - 三种“没有”要分开：没接代理 → 503；代理连不上 → 503 且说“代理不可用”；
//     代理还没探完 → 200 + ready=false + host_caps=null。最后一种**不是**空清单。
//   - 有读数时原样转述：条目、版本、partial、issues 一个都不吞。
//   - 代理给了坏响应 → 500（这是需要人看日志的故障，不是“没有能力”）。

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
	"upanel/internal/auth"
	"upanel/internal/certificates"
	"upanel/internal/config"
	"upanel/internal/hostcaps"
	"upanel/internal/platform"
	"upanel/internal/version"
)

// stubCapabilities 是 CapabilitiesReader 的测试替身。
type stubCapabilities struct {
	caps agentclient.Capabilities
	err  error
}

func (s *stubCapabilities) Capabilities(context.Context) (agentclient.Capabilities, error) {
	if s.err != nil {
		return agentclient.Capabilities{}, s.err
	}
	return s.caps, nil
}

func capabilitiesFixture(t *testing.T, reader CapabilitiesReader) (*Server, *http.Cookie) {
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
		Capabilities:      reader,
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	seedTestUser(t, srv.opts.Auth, testUser, testUserPass)
	cookie, _ := loginOK(t, srv)
	return srv, cookie
}

func getCapabilities(t *testing.T, srv *Server, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	return adminGet(t, srv, cookie, "/api/v1/system/capabilities")
}

func sampleHostCaps() *hostcaps.Report {
	return &hostcaps.Report{
		GeneratedAt: "2026-10-30T12:00:00Z",
		Partial:     true,
		Issues:      []string{"/usr/sbin 读不出来：permission denied"},
		Items: []hostcaps.Item{
			{
				ID: "restic", Kind: hostcaps.KindProgram, Feature: "备份与恢复",
				State: hostcaps.StatePresent, Path: "/usr/bin/restic", Version: "0.12.1",
			},
			{
				ID: "docker", Kind: hostcaps.KindProgram, Feature: "容器清单、容器启停、Compose 部署",
				State: hostcaps.StateUnknown, Hint: "",
				Reason: "找不到 docker，但有固定目录读不出来：说不清是真的没有，还是这一轮没看见",
			},
		},
	}
}

func TestSystemCapabilitiesRequiresAdmin(t *testing.T) {
	srv, _ := capabilitiesFixture(t, &stubCapabilities{caps: agentclient.Capabilities{HostCaps: sampleHostCaps()}})

	if rec := getCapabilities(t, srv, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("未登录状态码 = %d，期望 401（%s）", rec.Code, rec.Body.String())
	}

	if _, err := srv.opts.Auth.CreateUser(context.Background(), auth.CreateUserInput{
		Username: "watcher", DisplayName: "watcher", Role: auth.RoleViewer, Password: "watcher-horse-9",
	}); err != nil {
		t.Fatalf("创建 viewer 失败: %v", err)
	}
	viewerCookie, _ := loginAsUser(t, srv, "watcher", "watcher-horse-9")
	if rec := getCapabilities(t, srv, viewerCookie); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer 状态码 = %d，期望 403（%s）", rec.Code, rec.Body.String())
	}
}

func TestSystemCapabilitiesPassesReportThrough(t *testing.T) {
	report := sampleHostCaps()
	srv, cookie := capabilitiesFixture(t, &stubCapabilities{caps: agentclient.Capabilities{HostCaps: report}})
	rec := getCapabilities(t, srv, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d（%s）", rec.Code, rec.Body.String())
	}

	var body struct {
		Data struct {
			Ready    bool             `json:"ready"`
			HostCaps *hostcaps.Report `json:"host_caps"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if !body.Data.Ready {
		t.Fatal("代理探过了，ready 必须是 true")
	}
	if body.Data.HostCaps == nil {
		t.Fatal("探过了就必须把报告原样带出来，不能吞成 null")
	}
	if body.Data.HostCaps.GeneratedAt != "2026-10-30T12:00:00Z" {
		t.Fatalf("generated_at = %q", body.Data.HostCaps.GeneratedAt)
	}
	// partial 与 issues 必须一起转述：只说“没有 docker”而隐瞒“有目录读不出来”，
	// 就是把这台机器说成另一台。
	if !body.Data.HostCaps.Partial {
		t.Fatal("partial 被吞掉了")
	}
	if len(body.Data.HostCaps.Issues) != 1 || !strings.Contains(body.Data.HostCaps.Issues[0], "/usr/sbin") {
		t.Fatalf("issues 被吞掉了：%v", body.Data.HostCaps.Issues)
	}
	if len(body.Data.HostCaps.Items) != 2 {
		t.Fatalf("条目数 = %d", len(body.Data.HostCaps.Items))
	}
	if body.Data.HostCaps.Items[0].Version != "0.12.1" || body.Data.HostCaps.Items[0].Path != "/usr/bin/restic" {
		t.Fatalf("restic 那一条被改了：%+v", body.Data.HostCaps.Items[0])
	}
}

func TestSystemCapabilitiesNotReadyIsNotAnEmptyList(t *testing.T) {
	// 代理起来了但还没探完：HostCaps 是 nil。
	srv, cookie := capabilitiesFixture(t, &stubCapabilities{caps: agentclient.Capabilities{}})
	rec := getCapabilities(t, srv, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("还没探完不是错误，状态码 = %d（%s）", rec.Code, rec.Body.String())
	}

	var body struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if ready, _ := body.Data["ready"].(bool); ready {
		t.Fatal("代理没探完时 ready 必须是 false")
	}
	if value, ok := body.Data["host_caps"]; !ok || value != nil {
		t.Fatalf("没探完时 host_caps 必须是 null，不能是空对象或空数组：%v", value)
	}
}

func TestSystemCapabilitiesWithoutAgentIsNotAnEmptyList(t *testing.T) {
	srv, cookie := capabilitiesFixture(t, nil)
	rec := getCapabilities(t, srv, cookie)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("没接代理时状态码 = %d，期望 503（%s）", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "AGENT_UNAVAILABLE") {
		t.Fatalf("要说明是代理不可用而不是没有能力：%s", body)
	}
}

func TestSystemCapabilitiesMapsAgentFailure(t *testing.T) {
	srv, cookie := capabilitiesFixture(t, &stubCapabilities{
		err: &agentclient.UnavailableError{Err: errors.New("dial unix: no such file")},
	})
	rec := getCapabilities(t, srv, cookie)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("代理连不上时状态码 = %d，期望 503（%s）", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "代理不可用") {
		t.Fatalf("要说清是代理不可用：%s", body)
	}

	srv2, cookie2 := capabilitiesFixture(t, &stubCapabilities{err: errors.New("响应无法解析")})
	rec2 := getCapabilities(t, srv2, cookie2)
	if rec2.Code != http.StatusInternalServerError {
		t.Fatalf("坏响应状态码 = %d，期望 500（%s）", rec2.Code, rec2.Body.String())
	}
}
