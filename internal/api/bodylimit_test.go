package api

// P06.2 的两个请求体上限，各有一条用例守着——因为它们在修复前都是**悄悄**错的：
//
//  1. 预检：`file.write_text` 的参数里带着最多 1 MiB 文本（CONTRACTS.md 第 567 行
//     要求这条路径的传输层放宽到 2 MiB）。全局 1 MiB 的兜底会让“保存一个大一点的
//     配置文件”在 1 MiB 处失败，错误还是 413——看起来像内容太大，其实是信封太紧。
//  2. 上传内容：`PUT /files/uploads/:id/content` 是原始字节流，体积由创建时声明的
//     size_bytes 决定。在入口套 MaxBytesReader 会让**每一个**超过 1 MiB 的上传
//     在半路被截断，而这条路由存在的全部意义就是搬大文件。
//
// 两条用例都断言“字节完整到达下一层”，而不是只看状态码：被截断的请求同样可以
// 返回 200（处理器拿到的是半截内容）。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"upanel/internal/files"
)

// TestBodyLimitFor picks 每个路由实际用的上限。
func TestBodyLimitFor(t *testing.T) {
	cases := []struct {
		method string
		path   string
		limit  int64
		wrap   bool
	}{
		{http.MethodPost, "/api/v1/operations/preflight", MaxPreflightBodyBytes, true},
		{http.MethodPut, "/api/v1/files/uploads/up_00112233445566778899aabbccddeeff/content", 0, false},
		// 只认内容入口：创建/查询/放弃都要照旧受限。
		{http.MethodPost, "/api/v1/files/uploads", MaxRequestBodyBytes, true},
		{http.MethodGet, "/api/v1/files/uploads/up_00112233445566778899aabbccddeeff", MaxRequestBodyBytes, true},
		// 后面再多一层就不是内容入口了。
		{http.MethodPut, "/api/v1/files/uploads/up_00112233445566778899aabbccddeeff/content/x", MaxRequestBodyBytes, true},
		{http.MethodPut, "/api/v1/files/uploads//content", MaxRequestBodyBytes, true},
		{http.MethodPost, "/api/v1/users", MaxRequestBodyBytes, true},
	}
	for _, item := range cases {
		limit, wrap := bodyLimitFor(item.method, item.path, MaxRequestBodyBytes)
		if limit != item.limit || wrap != item.wrap {
			t.Fatalf("%s %s → (%d,%v)，期望 (%d,%v)", item.method, item.path, limit, wrap, item.limit, item.wrap)
		}
	}
}

// TestPreflightAcceptsTextWriteBody 1 MiB 的 content 能穿过 Web 层到达代理：
// 请求体没有被 1 MiB 的兜底截断，代理收到的参数是完整的。
func TestPreflightAcceptsTextWriteBody(t *testing.T) {
	agent := &stubAgent{plan: stubPlan()}
	f := newPreflightFixture(t, agent)

	content := strings.Repeat("a", files.MaxTextBytes)
	rec := f.do(t, http.MethodPost, "/api/v1/operations/preflight",
		preflightBody("file.write_text", `{"root_id":"etc","relative_path":"app.conf","content":"`+content+`"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；响应 %.300s", rec.Code, rec.Body.String())
	}
	if len(agent.calls) != 1 {
		t.Fatalf("代理收到 %d 次预检", len(agent.calls))
	}
	// 完整＝参数里那一整段 content 一个字都没少。
	if got := len(agent.calls[0].Params); got < files.MaxTextBytes {
		t.Fatalf("代理收到的参数只有 %d 字节，内容被截断了", got)
	}
	if !strings.Contains(string(agent.calls[0].Params), content) {
		t.Fatal("代理收到的 content 与发出的不一致")
	}
}

// TestPreflightBodyLimitIsFinite 放宽不等于无界：超过 2 MiB 的信封仍然是 413。
func TestPreflightBodyLimitIsFinite(t *testing.T) {
	agent := &stubAgent{plan: stubPlan()}
	f := newPreflightFixture(t, agent)

	huge := strings.Repeat("a", int(MaxPreflightBodyBytes)+1)
	rec := f.do(t, http.MethodPost, "/api/v1/operations/preflight",
		preflightBody("file.write_text", `{"root_id":"etc","relative_path":"app.conf","content":"`+huge+`"}`))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("状态码 = %d，期望 413；响应 %.300s", rec.Code, rec.Body.String())
	}
	if len(agent.calls) != 0 {
		t.Fatalf("超限的请求仍然转给了代理 %d 次", len(agent.calls))
	}
}

// TestUploadContentIsNotCappedAtOneMiB 1.5 MiB 的上传内容完整送到代理。
// 修复前这条路径套着 1 MiB 的 MaxBytesReader，处理器只会拿到前 1 MiB。
func TestUploadContentIsNotCappedAtOneMiB(t *testing.T) {
	const size = 3 << 19 // 1.5 MiB
	stub := &stubUploads{stubFiles: &stubFiles{}, info: sampleUpload()}
	stub.info.SizeBytes = size
	srv, cookie, csrf := filesSession(t, stub)

	// Content-Length 由 httptest.NewRequest 从 strings.Reader 自动填好。
	body := strings.Repeat("x", size)
	rec := uploadRequest(t, srv, http.MethodPut, "/api/v1/files/uploads/"+sampleUploadID+"/content",
		cookie, csrf, strings.NewReader(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；响应 %.300s", rec.Code, rec.Body.String())
	}
	if len(stub.putBody) != size {
		t.Fatalf("代理收到 %d 字节，期望 %d：上传内容被入口截断了", len(stub.putBody), size)
	}
	if stub.putSize != size {
		t.Fatalf("传给代理的声明大小 = %d", stub.putSize)
	}
}

// TestUploadContentRejectsShortTransferWithoutCap 长度判定来自 Content-Length，
// 与入口有没有 MaxBytesReader 无关：短一字节就是 400，不转给代理。
func TestUploadContentRejectsShortTransferWithoutCap(t *testing.T) {
	stub := &stubUploads{stubFiles: &stubFiles{}, info: sampleUpload()}
	srv, cookie, csrf := filesSession(t, stub)

	req := httptest.NewRequest(http.MethodPut, "/api/v1/files/uploads/"+sampleUploadID+"/content",
		strings.NewReader("hell"))
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Origin", testOrigin)
	req.Header.Set("Cookie", cookie.String())
	req.Header.Set(CSRFTokenHeader, csrf)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400；响应 %s", rec.Code, rec.Body.String())
	}
	if stub.putCalls != 0 {
		t.Fatalf("长度不符的传输仍然转给了代理 %d 次", stub.putCalls)
	}
}
