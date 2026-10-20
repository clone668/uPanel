package api

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"upanel/internal/config"
	"upanel/internal/platform"
	"upanel/internal/version"
)

// errorEnvelopeFor 解析错误信封，顺带断言是 JSON。
func errorEnvelopeFor(t *testing.T, body []byte) struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	RequestID string `json:"request_id"`
} {
	t.Helper()
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("响应不是合法 JSON: %v（body=%q）", err, string(body))
	}
	return env
}

func TestRequestIDGeneratedWhenAbsent(t *testing.T) {
	srv := newTestServer(t, platform.ModeMock, config.ListenMock)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/health/live", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", rec.Code)
	}
	rid := rec.Header().Get("X-Request-ID")
	if len(rid) != 32 {
		t.Fatalf("生成的 request id 应为 32 位十六进制，实际 %q", rid)
	}
	for _, c := range rid {
		if !strings.ContainsRune("0123456789abcdef", c) {
			t.Fatalf("request id 含非十六进制字符: %q", rid)
		}
	}

	var env struct {
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if env.RequestID != rid {
		t.Fatalf("响应体 request_id=%q 与响应头 %q 不一致", env.RequestID, rid)
	}
}

func TestRequestIDInheritedOnlyWhenValid(t *testing.T) {
	srv := newTestServer(t, platform.ModeMock, config.ListenMock)

	cases := []struct {
		name    string
		inbound string
		want    string // 期望的响应头值；空串表示“必须是新生成的值”
	}{
		{name: "合法值沿用", inbound: "abc-123_XYZ.9", want: "abc-123_XYZ.9"},
		{name: "空值重新生成", inbound: "", want: ""},
		{name: "含空格被丢弃", inbound: "bad id", want: ""},
		{name: "含控制字符被丢弃", inbound: "bad\tid", want: ""},
		{name: "超长被丢弃", inbound: strings.Repeat("a", 65), want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/v1/health/live", nil)
			if tc.inbound != "" {
				req.Header.Set("X-Request-ID", tc.inbound)
			}
			srv.Handler().ServeHTTP(rec, req)

			got := rec.Header().Get("X-Request-ID")
			if got == "" {
				t.Fatal("响应缺少 X-Request-ID")
			}
			if tc.want != "" && got != tc.want {
				t.Fatalf("X-Request-ID = %q，期望沿用 %q", got, tc.want)
			}
			if tc.want == "" && got == tc.inbound {
				t.Fatalf("非法 request id %q 不得被沿用", tc.inbound)
			}
		})
	}
}

func TestUnknownPathsReturnJSON404(t *testing.T) {
	srv := newTestServer(t, platform.ModeMock, config.ListenMock)

	// P01 验收：未知 API 绝不返回 index.html 或 200；末尾斜杠也不得因重定向而变成 301。
	for _, path := range []string{
		"/",
		"/index.html",
		"/api/v1",
		"/api/v1/",
		"/api/v1/unknown",
		"/api/v1/health",
		"/api/v1/health/live/",
		"/assets/app.js",
	} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

			if rec.Code != http.StatusNotFound {
				t.Fatalf("状态码 = %d，期望 404", rec.Code)
			}
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Fatalf("Content-Type = %q，期望 JSON", ct)
			}
			if strings.Contains(strings.ToLower(rec.Body.String()), "<html") {
				t.Fatalf("不得返回 HTML: %q", rec.Body.String())
			}
			env := errorEnvelopeFor(t, rec.Body.Bytes())
			if env.Error.Code != "NOT_FOUND" {
				t.Fatalf("error.code = %q，期望 NOT_FOUND", env.Error.Code)
			}
			if env.RequestID == "" {
				t.Fatal("错误信封缺少 request_id")
			}
		})
	}
}

func TestMethodNotAllowedIsJSONWithAllow(t *testing.T) {
	srv := newTestServer(t, platform.ModeMock, config.ListenMock)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/health/live", nil))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("状态码 = %d，期望 405", rec.Code)
	}
	allow := rec.Header().Get("Allow")
	if !strings.Contains(allow, http.MethodGet) || !strings.Contains(allow, http.MethodHead) {
		t.Fatalf("Allow = %q，应包含 GET 与 HEAD", allow)
	}
	env := errorEnvelopeFor(t, rec.Body.Bytes())
	if env.Error.Code != "VALIDATION_ERROR" {
		t.Fatalf("error.code = %q，期望 VALIDATION_ERROR", env.Error.Code)
	}
}

func TestHeadHealthLiveHasNoBody(t *testing.T) {
	// ResponseRecorder 不模拟 net/http 对 HEAD 的正文抑制，这里必须走真实连接。
	srv := newTestServer(t, platform.ModeMock, config.ListenMock)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Head(ts.URL + "/api/v1/health/live")
	if err != nil {
		t.Fatalf("HEAD 请求失败: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应失败: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d", resp.StatusCode)
	}
	if len(body) != 0 {
		t.Fatalf("HEAD 响应不应有正文，实际 %q", string(body))
	}
	if resp.Header.Get("X-Request-ID") == "" {
		t.Fatal("HEAD 响应也应带 X-Request-ID")
	}
}

func TestRecoveryTurnsPanicInto500WithoutLeakingStack(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelError}))

	authSvc, aud, db := newTestServicesWithDB(t)
	srv, err := New(Options{
		ListenAddress:   config.ListenMock,
		Mode:            platform.ModeMock,
		Version:         version.Info{Version: "test", Commit: "test", BuildDate: "test", GoVersion: "test"},
		Logger:          logger,
		ShutdownTimeout: time.Second,
		Auth:            authSvc,
		Audit:           aud,
		Operations:      newTestOperations(t, db, nil),
		AllowedOrigins:  testOrigins(),
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	srv.engine.GET("/api/v1/panic", func(*gin.Context) { panic("boom-secret-marker") })

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/panic", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d，期望 500", rec.Code)
	}
	env := errorEnvelopeFor(t, rec.Body.Bytes())
	if env.Error.Code != "INTERNAL_ERROR" {
		t.Fatalf("error.code = %q，期望 INTERNAL_ERROR", env.Error.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "boom-secret-marker") || strings.Contains(body, "goroutine") {
		t.Fatalf("响应体泄漏了 panic 内容或堆栈: %q", body)
	}
	// 堆栈必须留在日志里，便于排查。
	if !strings.Contains(logs.String(), "boom-secret-marker") || !strings.Contains(logs.String(), "stack=") {
		t.Fatalf("日志未记录 panic 详情: %q", logs.String())
	}
}

func TestBodyLimitMiddlewareRejectsOversizedBody(t *testing.T) {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(bodyLimitMiddleware(16))

	var seen error
	r.POST("/", func(c *gin.Context) {
		_, seen = io.ReadAll(c.Request.Body)
		if seen != nil {
			c.Status(http.StatusRequestEntityTooLarge)
			return
		}
		c.Status(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(strings.Repeat("x", 64)))
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("状态码 = %d，期望 413", rec.Code)
	}
	if !IsBodyTooLarge(seen) {
		t.Fatalf("IsBodyTooLarge 未识别超限错误: %v", seen)
	}
}

func TestIsBodyTooLargeFalseForOtherErrors(t *testing.T) {
	if IsBodyTooLarge(nil) || IsBodyTooLarge(io.EOF) {
		t.Fatal("非超限错误不得判定为超限")
	}
}
