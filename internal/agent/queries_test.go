package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// echoParams 是测试查询自己的参数 DTO：用于验证严格解码与执行链路。
type echoParams struct {
	Message string `json:"message"`
	Count   int    `json:"count"`
}

// testRegistry 返回带一个**仅测试用**查询的登记表。
//
// 生产登记表里没有任何查询（见 NewProductionRegistry）；这里的实现不进入生产代码，
// 与 STAGES.md P03.4 对“测试用无害动作只放在独立测试登记表”的要求一致。
func testRegistry(t *testing.T) *Registry {
	t.Helper()
	reg := NewRegistry()
	err := reg.RegisterQuery(Query{
		Name: "test.echo",
		Decode: func(raw json.RawMessage) (any, error) {
			var p echoParams
			if err := DecodeParams(raw, &p); err != nil {
				return nil, err
			}
			return p, nil
		},
		Run: func(ctx context.Context, params any) (any, error) {
			p, ok := params.(echoParams)
			if !ok {
				return nil, errors.New("参数类型错误")
			}
			if p.Message == "fail" {
				return nil, errors.New("内部失败")
			}
			return map[string]any{"message": p.Message, "count": p.Count}, nil
		},
	})
	if err != nil {
		t.Fatalf("登记测试查询失败: %v", err)
	}
	return reg
}

func postQuery(t *testing.T, srv *Server, body string, uid uint32) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/queries", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return serveAs(srv, req, uid)
}

func TestQueriesRejectsUnknownQuery(t *testing.T) {
	srv := newTestServer(t) // 生产登记表：没有任何查询
	rec := postQuery(t, srv, `{"query":"system.summary"}`, testWebUID)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400（%s）", rec.Code, rec.Body.String())
	}
	if code := decodeErrorCode(t, rec.Body.Bytes()); code != "VALIDATION_ERROR" {
		t.Fatalf("错误码 = %q", code)
	}
	if !strings.Contains(rec.Body.String(), "未登记") {
		t.Fatalf("错误信息应说明未登记: %s", rec.Body.String())
	}
}

func TestQueriesRejectsCommandStyleInput(t *testing.T) {
	// 任何“命令字符串”字段都不在协议里：要么作为未知字段被拒绝，要么因未登记被拒绝。
	bodies := []string{
		`{"query":"command","params":{"command":"id"}}`,
		`{"query":"test.echo","command":"id -u"}`,
		`{"query":"test.echo","params":{"argv":["id"]}}`,
	}
	for _, body := range bodies {
		srv := newTestServerWithRegistry(t, testRegistry(t))
		rec := postQuery(t, srv, body, testWebUID)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: 状态码 = %d，期望 400（%s）", body, rec.Code, rec.Body.String())
		}
	}
}

func TestQueriesRejectsBadEnvelope(t *testing.T) {
	srv := newTestServerWithRegistry(t, testRegistry(t))

	for _, tc := range []struct{ name, body string }{
		{"缺少 query", `{"params":{}}`},
		{"空请求体", ``},
		{"不是 JSON", `not json`},
		{"尾部多余内容", `{"query":"test.echo"}{"query":"test.echo"}`},
		{"未知字段", `{"query":"test.echo","command":"id"}`},
		{"类型不对", `{"query":123}`},
	} {
		rec := postQuery(t, srv, tc.body, testWebUID)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: 状态码 = %d，期望 400（%s）", tc.name, rec.Code, rec.Body.String())
		}
	}
}

func TestQueriesRejectsUnknownParams(t *testing.T) {
	srv := newTestServerWithRegistry(t, testRegistry(t))
	rec := postQuery(t, srv, `{"query":"test.echo","params":{"message":"hi","bogus":1}}`, testWebUID)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("未知参数必须被拒绝，状态码 = %d（%s）", rec.Code, rec.Body.String())
	}
}

func TestQueriesRejectsOversizeBody(t *testing.T) {
	srv := newTestServerWithRegistry(t, testRegistry(t))
	huge := strings.Repeat("x", maxQueryRequestBytes+1024)
	rec := postQuery(t, srv, `{"query":"test.echo","params":{"message":"`+huge+`"}}`, testWebUID)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("状态码 = %d，期望 413", rec.Code)
	}
}

func TestQueriesRejectsNonJSONContentType(t *testing.T) {
	srv := newTestServerWithRegistry(t, testRegistry(t))
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/queries", strings.NewReader(`{"query":"test.echo"}`))
	req.Header.Set("Content-Type", "text/plain")
	rec := serveAs(srv, req, testWebUID)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("状态码 = %d，期望 415", rec.Code)
	}
}

func TestQueriesRequiresPost(t *testing.T) {
	srv := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/internal/v1/queries", nil)
	rec := serveAs(srv, req, testWebUID)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("状态码 = %d，期望 405", rec.Code)
	}
	if rec.Header().Get("Allow") != "POST" {
		t.Fatalf("Allow = %q", rec.Header().Get("Allow"))
	}
}

func TestQueriesRunsRegisteredQuery(t *testing.T) {
	srv := newTestServerWithRegistry(t, testRegistry(t))
	rec := postQuery(t, srv, `{"query":"test.echo","params":{"message":"你好","count":2}}`, testWebUID)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（%s）", rec.Code, rec.Body.String())
	}
	var body struct {
		Data struct {
			Message string `json:"message"`
			Count   int    `json:"count"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if body.Data.Message != "你好" || body.Data.Count != 2 {
		t.Fatalf("data = %+v", body.Data)
	}
}

func TestQueriesReportsRunFailure(t *testing.T) {
	srv := newTestServerWithRegistry(t, testRegistry(t))
	rec := postQuery(t, srv, `{"query":"test.echo","params":{"message":"fail"}}`, testWebUID)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d，期望 500", rec.Code)
	}
	if code := decodeErrorCode(t, rec.Body.Bytes()); code != "INTERNAL_ERROR" {
		t.Fatalf("错误码 = %q", code)
	}
	// 内部错误信息不能带回响应体。
	if strings.Contains(rec.Body.String(), "内部失败") {
		t.Fatalf("错误响应泄漏了内部细节: %s", rec.Body.String())
	}
}

func TestCapabilitiesListsRegisteredQueriesStably(t *testing.T) {
	reg := testRegistry(t)
	if err := reg.RegisterQuery(Query{
		Name:   "test.alpha",
		Decode: func(raw json.RawMessage) (any, error) { return struct{}{}, nil },
		Run:    func(ctx context.Context, params any) (any, error) { return struct{}{}, nil },
	}); err != nil {
		t.Fatalf("登记失败: %v", err)
	}
	srv := newTestServerWithRegistry(t, reg)
	req := httptest.NewRequest(http.MethodGet, "/internal/v1/capabilities", nil)
	rec := serveAs(srv, req, testWebUID)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", rec.Code)
	}
	var body struct {
		Data struct {
			SupportedQueries []string `json:"supported_queries"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	got := strings.Join(body.Data.SupportedQueries, ",")
	if got != "test.alpha,test.echo" {
		t.Fatalf("supported_queries = %q，期望按字典序", got)
	}
}
