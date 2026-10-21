package api

// P06.2 接口层用例：暂存上传的四条路由（CONTRACTS.md 第 11 节）。
//
// 这一层要证明的**不是**“文件上传得动”——那是 internal/files 的 Linux 用例和
// VM 验收的事。这里要证明的是三件在 HTTP 边界上会发生、而且一旦出错就没人拦得住的事：
//
//  1. **上传不等于落地**。PUT 收完字节之后目标路径一个字节都没变：本层从创建到
//     收完只调用代理的 CreateUpload / PutUploadContent，从不调用任何“写目标文件”
//     的东西——因为路由表里根本没有那种路由（最后一条用例守着这件事）。
//  2. **长度必须自证**。Content-Length 缺失（分块）或与创建时声明的 size_bytes
//     不一致，一律 400，且**不会**把请求转给代理：一次只传了一半的传输不能变成
//     “一半的文件”，而判定的地方越靠前越好。
//  3. **代理的判决原样转出**。上传状态不对（409 VERSION_CONFLICT）、暂存不存在
//     （404）、盘满或超限（413，复用 VALIDATION_ERROR）、根只读（403）——都在
//     既有的错误码闭集里，不为了“上传”新造一个码。前端只写一套处理。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"upanel/internal/auth"
	"upanel/internal/files"
	"upanel/internal/httpx"
)

// sampleUploadID 是形状合法的暂存 id（up_ + 32 位小写十六进制）。
const sampleUploadID = "up_00112233445566778899aabbccddeeff"

func sampleUpload() files.UploadInfo {
	return files.UploadInfo{
		UploadID:      sampleUploadID,
		RootID:        "etc",
		Path:          "app/app.conf",
		SizeBytes:     5,
		ReceivedBytes: 0,
		State:         files.UploadStateOpen,
		CreatedAt:     time.Date(2026, 10, 21, 3, 0, 0, 0, time.UTC),
		ExpiresAt:     time.Date(2026, 10, 22, 3, 0, 0, 0, time.UTC),
	}
}

// stubUploads 同时是 FilesReader 与 FilesUploader：只读侧嵌 stubFiles（本文件不测读取）。
//
// 它记录收到的东西，因为用例要断言的重点是“**没有**传下去什么”：
// 被拒的请求不得到达代理，收完的 PUT 不得顺带调用创建或放弃。
type stubUploads struct {
	*stubFiles

	info      files.UploadInfo
	createErr error
	getErr    error
	putErr    error
	deleteErr error

	createCalls int
	getCalls    int
	putCalls    int
	deleteCalls int

	created   files.CreateUploadParams
	putID     string
	putSize   int64
	putBody   []byte
	putRead   io.Reader
	gotID     string
	deletedID string
}

func (s *stubUploads) CreateUpload(_ context.Context, params files.CreateUploadParams) (files.UploadInfo, error) {
	s.createCalls++
	s.created = params
	if s.createErr != nil {
		return files.UploadInfo{}, s.createErr
	}
	return s.info, nil
}

func (s *stubUploads) PutUploadContent(_ context.Context, uploadID string, _ files.UploadInfo, body io.Reader, size int64) (files.UploadInfo, error) {
	s.putCalls++
	s.putID = uploadID
	s.putSize = size
	s.putRead = body
	content, _ := io.ReadAll(body)
	s.putBody = content
	if s.putErr != nil {
		return files.UploadInfo{}, s.putErr
	}
	done := s.info
	done.State = files.UploadStateReceived
	done.ReceivedBytes = uint64(size)
	return done, nil
}

func (s *stubUploads) GetUpload(_ context.Context, uploadID string) (files.UploadInfo, error) {
	s.getCalls++
	s.gotID = uploadID
	if s.getErr != nil {
		return files.UploadInfo{}, s.getErr
	}
	return s.info, nil
}

func (s *stubUploads) DeleteUpload(_ context.Context, uploadID string) error {
	s.deleteCalls++
	s.deletedID = uploadID
	return s.deleteErr
}

// uploadRecorder 是支持截止时间的响应记录器。
//
// handleUploadContent 会用 http.ResponseController 延长读/写截止时间；gin 会把
// ResponseWriter 包一层但实现了 Unwrap，所以只要这里补上两个方法，PUT 用例走的就是
// **真实代码路径**，而不是被“无法为长上传延长超时”的 500 挡在门外。
type uploadRecorder struct {
	*httptest.ResponseRecorder
	readDeadline  time.Time
	writeDeadline time.Time
}

func (u *uploadRecorder) SetReadDeadline(t time.Time) error  { u.readDeadline = t; return nil }
func (u *uploadRecorder) SetWriteDeadline(t time.Time) error { u.writeDeadline = t; return nil }

// uploadRequest 发一次上传相关的请求，头里带齐同源与 CSRF（缺失时传空串，用例自会看见 403）。
func uploadRequest(t *testing.T, srv *Server, method, path string, cookie *http.Cookie, csrf string, body io.Reader) *uploadRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, body)
	if method == http.MethodPut {
		req.Header.Set("Content-Type", "application/octet-stream")
	} else {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Origin", testOrigin)
	if cookie != nil {
		req.Header.Set("Cookie", cookie.String())
	}
	if csrf != "" {
		req.Header.Set(CSRFTokenHeader, csrf)
	}
	rec := &uploadRecorder{ResponseRecorder: httptest.NewRecorder()}
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// uploadBody 构造一份合法的创建声明。size_bytes 是**十进制字符串**（见 DTO 的 json tag）。
func uploadBody(size string) string {
	return `{"root_id":"etc","path":"app/app.conf","size_bytes":"` + size + `"}`
}

// uploadRoutes 是四条路由的固定形状，权限用例逐条覆盖。
func uploadRoutes() []struct{ method, path string } {
	return []struct{ method, path string }{
		{http.MethodPost, "/api/v1/files/uploads"},
		{http.MethodPut, "/api/v1/files/uploads/" + sampleUploadID + "/content"},
		{http.MethodGet, "/api/v1/files/uploads/" + sampleUploadID},
		{http.MethodDelete, "/api/v1/files/uploads/" + sampleUploadID},
	}
}

// TestFilesUploadRequiresSession 未登录一律 401，包括 PUT 与 DELETE：
// 挡住创建却敞开放弃（或反过来）等于没挡。
func TestFilesUploadRequiresSession(t *testing.T) {
	srv, _, _ := filesSession(t, &stubUploads{stubFiles: &stubFiles{}})
	for _, route := range uploadRoutes() {
		rec := uploadRequest(t, srv, route.method, route.path, nil, "", strings.NewReader(uploadBody("5")))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s 未登录状态码 = %d，期望 401", route.method, route.path, rec.Code)
		}
	}
}

// TestFilesUploadForbiddenForViewer 上传是管理员能力：文件内容整组 admin（PLAN.md 第 15 节）。
func TestFilesUploadForbiddenForViewer(t *testing.T) {
	srv, _, _ := filesSession(t, &stubUploads{stubFiles: &stubFiles{}})
	if _, err := srv.opts.Auth.CreateUser(context.Background(), auth.CreateUserInput{
		Username: "watcher", DisplayName: "watcher", Role: auth.RoleViewer, Password: "watcher-horse-9",
	}); err != nil {
		t.Fatalf("创建 viewer 失败: %v", err)
	}
	cookie, csrf := loginAsUser(t, srv, "watcher", "watcher-horse-9")
	for _, route := range uploadRoutes() {
		rec := uploadRequest(t, srv, route.method, route.path, cookie, csrf, strings.NewReader(uploadBody("5")))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s viewer 状态码 = %d，期望 403", route.method, route.path, rec.Code)
		}
		if got := parseResult(t, rec.ResponseRecorder).Error; got == nil || got.Code != string(httpx.CodeForbidden) {
			t.Fatalf("%s %s 错误码 = %+v", route.method, route.path, got)
		}
	}
}

// TestFilesUploadRequiresCSRF 三条改状态的路由都要 CSRF 令牌；GET 不要（它不改状态）。
func TestFilesUploadRequiresCSRF(t *testing.T) {
	stub := &stubUploads{stubFiles: &stubFiles{}, info: sampleUpload()}
	srv, cookie, _ := filesSession(t, stub)

	for _, route := range uploadRoutes() {
		if route.method == http.MethodGet {
			continue
		}
		rec := uploadRequest(t, srv, route.method, route.path, cookie, "", strings.NewReader(uploadBody("5")))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s 缺 CSRF 状态码 = %d，期望 403", route.method, route.path, rec.Code)
		}
		if got := parseResult(t, rec.ResponseRecorder).Error; got == nil || got.Code != string(httpx.CodeCSRFFailed) {
			t.Fatalf("%s %s 错误码 = %+v，期望 CSRF_FAILED", route.method, route.path, got)
		}
	}
	if stub.createCalls+stub.putCalls+stub.deleteCalls != 0 {
		t.Fatalf("CSRF 被拒的请求仍然到达代理：create=%d put=%d delete=%d",
			stub.createCalls, stub.putCalls, stub.deleteCalls)
	}
}

// TestFilesUploadUnavailableWithoutAgent 没有配置代理时是 503 AGENT_UNAVAILABLE。
func TestFilesUploadUnavailableWithoutAgent(t *testing.T) {
	srv, cookie, csrf := filesSession(t, nil)
	for _, route := range uploadRoutes() {
		rec := uploadRequest(t, srv, route.method, route.path, cookie, csrf, strings.NewReader(uploadBody("5")))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s %s 状态码 = %d，期望 503", route.method, route.path, rec.Code)
		}
		if got := parseResult(t, rec.ResponseRecorder).Error; got == nil || got.Code != string(httpx.CodeAgentUnavailable) {
			t.Fatalf("%s %s 错误码 = %+v", route.method, route.path, got)
		}
	}
}

// TestFilesUploadNotImplemented 有读取端但没有暂存上传能力时是 501，不是看起来成功的空响应：
// “这个部署不能上传”必须可判定。
func TestFilesUploadNotImplemented(t *testing.T) {
	srv, cookie, csrf := filesSession(t, &stubFiles{})
	for _, route := range uploadRoutes() {
		rec := uploadRequest(t, srv, route.method, route.path, cookie, csrf, strings.NewReader(uploadBody("5")))
		if rec.Code != http.StatusNotImplemented {
			t.Fatalf("%s %s 状态码 = %d，期望 501", route.method, route.path, rec.Code)
		}
		if got := parseResult(t, rec.ResponseRecorder).Error; got == nil || got.Code != string(httpx.CodeNotImplemented) {
			t.Fatalf("%s %s 错误码 = %+v", route.method, route.path, got)
		}
	}
}

// TestCreateUploadRejectsBadBody 创建声明的形状问题在 HTTP 层就拒，且不转给代理。
//
// 两条不同判决刻意放在一起：`..` 与绝对路径是 422 PATH_OUTSIDE_ROOT（与只读路由
// 同一张表），其余是 400 VALIDATION_ERROR。前端只写一套处理。
func TestCreateUploadRejectsBadBody(t *testing.T) {
	stub := &stubUploads{stubFiles: &stubFiles{}, info: sampleUpload()}
	srv, cookie, csrf := filesSession(t, stub)

	cases := []struct {
		name   string
		body   string
		status int
		code   httpx.Code
	}{
		{"不是 JSON", `size_bytes=5`, http.StatusBadRequest, httpx.CodeValidationError},
		{"size_bytes 写成数字", `{"root_id":"etc","path":"app.conf","size_bytes":5}`, http.StatusBadRequest, httpx.CodeValidationError},
		{"size_bytes 为空串", `{"root_id":"etc","path":"app.conf","size_bytes":""}`, http.StatusBadRequest, httpx.CodeValidationError},
		{"未知字段 overwrite", `{"root_id":"etc","path":"app.conf","size_bytes":"5","overwrite":true}`, http.StatusBadRequest, httpx.CodeValidationError},
		{"首个对象之后还有内容", uploadBody("5") + `{}`, http.StatusBadRequest, httpx.CodeValidationError},
		{"缺 root_id", `{"path":"app.conf","size_bytes":"5"}`, http.StatusBadRequest, httpx.CodeValidationError},
		{"size_bytes 为 0", `{"root_id":"etc","path":"app.conf","size_bytes":"0"}`, http.StatusBadRequest, httpx.CodeValidationError},
		{"空路径", `{"root_id":"etc","path":"","size_bytes":"5"}`, http.StatusBadRequest, httpx.CodeValidationError},
		{"绝对路径", `{"root_id":"etc","path":"/etc/passwd","size_bytes":"5"}`, http.StatusUnprocessableEntity, httpx.CodePathOutsideRoot},
		{"上跳路径", `{"root_id":"etc","path":"../escape","size_bytes":"5"}`, http.StatusUnprocessableEntity, httpx.CodePathOutsideRoot},
		{"暂存目录本身", `{"root_id":"etc","path":".upanel/staging/x","size_bytes":"5"}`, http.StatusUnprocessableEntity, httpx.CodePathOutsideRoot},
		{"expected_version 非法", `{"root_id":"etc","path":"app.conf","size_bytes":"5","expected_version":"v1/a"}`, http.StatusBadRequest, httpx.CodeValidationError},
	}
	for _, item := range cases {
		rec := uploadRequest(t, srv, http.MethodPost, "/api/v1/files/uploads", cookie, csrf, strings.NewReader(item.body))
		if rec.Code != item.status {
			t.Fatalf("%s → %d，期望 %d；响应 %s", item.name, rec.Code, item.status, rec.Body.String())
		}
		if got := parseResult(t, rec.ResponseRecorder).Error; got == nil || got.Code != string(item.code) {
			t.Fatalf("%s 错误码 = %+v，期望 %s", item.name, got, item.code)
		}
	}
	if stub.createCalls != 0 {
		t.Fatalf("被拒的创建声明仍然转给了代理 %d 次", stub.createCalls)
	}
}

// TestCreateUploadOK 合法声明返回 201 与暂存视图；size_bytes / received_bytes 是字符串。
func TestCreateUploadOK(t *testing.T) {
	stub := &stubUploads{stubFiles: &stubFiles{}, info: sampleUpload()}
	srv, cookie, csrf := filesSession(t, stub)

	rec := uploadRequest(t, srv, http.MethodPost, "/api/v1/files/uploads", cookie, csrf,
		strings.NewReader(`{"root_id":"etc","path":"app/app.conf","size_bytes":"5","expected_version":"v1-a1b2-c3d4-5-6"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("状态码 = %d，期望 201；响应 %s", rec.Code, rec.Body.String())
	}
	if stub.created.RootID != "etc" || stub.created.Path != "app/app.conf" ||
		stub.created.SizeBytes != 5 || stub.created.ExpectedVersion != "v1-a1b2-c3d4-5-6" {
		t.Fatalf("转给代理的参数 = %+v", stub.created)
	}
	// 用 RawMessage 看字面量：数字与字符串在 JSON 里是两种形状，前端靠它决定要不要 parseInt。
	var data map[string]json.RawMessage
	if err := json.Unmarshal(parseResult(t, rec.ResponseRecorder).Data, &data); err != nil {
		t.Fatalf("data 不是对象: %v", err)
	}
	if string(data["size_bytes"]) != `"5"` || string(data["received_bytes"]) != `"0"` {
		t.Fatalf("字节数不是十进制字符串: size=%s received=%s", data["size_bytes"], data["received_bytes"])
	}
	if string(data["state"]) != `"open"` {
		t.Fatalf("state = %s，期望 open", data["state"])
	}
	if string(data["upload_id"]) != `"`+sampleUploadID+`"` {
		t.Fatalf("upload_id = %s", data["upload_id"])
	}
}

// TestUploadContentRejectsBadRequests 内容接收前的四道门，任何一道不过都不碰代理的写入：
//
//   - id 形状不合法（400）——id 会被拼进内部 URL，含 `/` 或 `..` 就是一次路径注入；
//   - 没有 Content-Length（400）——分块传输连“传完了没有”都无法判定；
//   - Content-Length 与声明的 size_bytes 不符（400）——一次只传一半的传输不能变成半个文件；
//   - 暂存状态不是 open（409）——已经收完或已失败的暂存不能重传。
func TestUploadContentRejectsBadRequests(t *testing.T) {
	received := sampleUpload()
	received.State = files.UploadStateReceived

	cases := []struct {
		name   string
		id     string
		state  files.UploadInfo
		body   io.Reader
		status int
		code   httpx.Code
		put    bool
	}{
		{"id 形状不合法", "up_0011", sampleUpload(), strings.NewReader("hello"), http.StatusBadRequest, httpx.CodeValidationError, false},
		{"id 含大写", "up_00112233445566778899AABBCCDDEEFF", sampleUpload(), strings.NewReader("hello"), http.StatusBadRequest, httpx.CodeValidationError, false},
		{"没有 Content-Length", sampleUploadID, sampleUpload(), struct{ io.Reader }{strings.NewReader("hello")}, http.StatusBadRequest, httpx.CodeValidationError, false},
		{"长度不符", sampleUploadID, sampleUpload(), strings.NewReader("hello!"), http.StatusBadRequest, httpx.CodeValidationError, false},
		{"状态已接收", sampleUploadID, received, strings.NewReader("hello"), http.StatusConflict, httpx.CodeVersionConflict, false},
	}
	for _, item := range cases {
		stub := &stubUploads{stubFiles: &stubFiles{}, info: item.state}
		srv, cookie, csrf := filesSession(t, stub)
		rec := uploadRequest(t, srv, http.MethodPut, "/api/v1/files/uploads/"+item.id+"/content", cookie, csrf, item.body)
		if rec.Code != item.status {
			t.Fatalf("%s → %d，期望 %d；响应 %s", item.name, rec.Code, item.status, rec.Body.String())
		}
		if got := parseResult(t, rec.ResponseRecorder).Error; got == nil || got.Code != string(item.code) {
			t.Fatalf("%s 错误码 = %+v，期望 %s", item.name, got, item.code)
		}
		if item.put && stub.putCalls == 0 {
			t.Fatalf("%s 应当转给代理", item.name)
		}
		if !item.put && stub.putCalls != 0 {
			t.Fatalf("%s 不该转给代理，却转了 %d 次", item.name, stub.putCalls)
		}
	}
}

// TestUploadContentForwardsStream 收完的 PUT：字节原样交给代理、大小取自 Content-Length、
// 响应里 state 变成 received——而**目标路径仍然没变**（本层从头到尾没调用任何“落地”）。
func TestUploadContentForwardsStream(t *testing.T) {
	stub := &stubUploads{stubFiles: &stubFiles{}, info: sampleUpload()}
	srv, cookie, csrf := filesSession(t, stub)

	rec := uploadRequest(t, srv, http.MethodPut, "/api/v1/files/uploads/"+sampleUploadID+"/content",
		cookie, csrf, strings.NewReader("hello"))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；响应 %s", rec.Code, rec.Body.String())
	}
	if stub.putID != sampleUploadID || stub.putSize != 5 || string(stub.putBody) != "hello" {
		t.Fatalf("转给代理的内容 = id:%q size:%d body:%q", stub.putID, stub.putSize, stub.putBody)
	}
	// 交给代理的是包了 keepalive 的请求体本身：这里没有把几 GiB 读进内存再转手的余地。
	if _, ok := stub.putRead.(*uploadKeepalive); !ok {
		t.Fatalf("请求体应当是 *uploadKeepalive，实际 %T", stub.putRead)
	}
	if rec.readDeadline.IsZero() {
		t.Fatalf("长上传必须推动读截止时间，否则第 30 秒会被 Web 超时掐断")
	}
	// 落地是 file.commit_upload 的事：这一层既不创建也不放弃，更不写目标文件。
	if stub.createCalls != 0 || stub.deleteCalls != 0 {
		t.Fatalf("PUT 不该顺带创建或放弃暂存：create=%d delete=%d", stub.createCalls, stub.deleteCalls)
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(parseResult(t, rec.ResponseRecorder).Data, &data); err != nil {
		t.Fatalf("data 不是对象: %v", err)
	}
	if string(data["state"]) != `"received"` {
		t.Fatalf("state = %s，期望 received", data["state"])
	}
	if string(data["received_bytes"]) != `"5"` {
		t.Fatalf("received_bytes = %s，期望 \"5\"", data["received_bytes"])
	}
}

// TestUploadContentReportsAgentErrors 代理的判决原样转出：盘满/超限是 413 但复用
// VALIDATION_ERROR（没有为上传新增错误码），暂存不存在是 404。
func TestUploadContentReportsAgentErrors(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		code   httpx.Code
	}{
		{"暂存不存在", files.ErrUploadNotFound, http.StatusNotFound, httpx.CodeNotFound},
		{"盘满", files.ErrNoSpace, http.StatusRequestEntityTooLarge, httpx.CodeValidationError},
		{"超过上限", files.ErrTooLarge, http.StatusRequestEntityTooLarge, httpx.CodeValidationError},
		{"根只读", files.ErrNotWritable, http.StatusForbidden, httpx.CodeForbidden},
		{"根不可用", files.ErrRootUnavailable, http.StatusConflict, httpx.CodeRootUnavailable},
	}
	for _, item := range cases {
		stub := &stubUploads{stubFiles: &stubFiles{}, info: sampleUpload()}
		stub.getErr = item.err
		stub.putErr = item.err
		srv, cookie, csrf := filesSession(t, stub)
		rec := uploadRequest(t, srv, http.MethodPut, "/api/v1/files/uploads/"+sampleUploadID+"/content",
			cookie, csrf, strings.NewReader("hello"))
		if rec.Code != item.status {
			t.Fatalf("%s → %d，期望 %d；响应 %s", item.name, rec.Code, item.status, rec.Body.String())
		}
		if got := parseResult(t, rec.ResponseRecorder).Error; got == nil || got.Code != string(item.code) {
			t.Fatalf("%s 错误码 = %+v，期望 %s", item.name, got, item.code)
		}
	}
}

// TestGetUploadOK 查进度把 id 原样转给代理，响应是暂存视图本身。
func TestGetUploadOK(t *testing.T) {
	stub := &stubUploads{stubFiles: &stubFiles{}, info: sampleUpload()}
	srv, cookie, _ := filesSession(t, stub)

	rec := adminGet(t, srv, cookie, "/api/v1/files/uploads/"+sampleUploadID)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；响应 %s", rec.Code, rec.Body.String())
	}
	if stub.gotID != sampleUploadID {
		t.Fatalf("转给代理的 id = %q", stub.gotID)
	}
	if stub.createCalls+stub.putCalls+stub.deleteCalls != 0 {
		t.Fatalf("查询不该有副作用：create=%d put=%d delete=%d", stub.createCalls, stub.putCalls, stub.deleteCalls)
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(parseResult(t, rec).Data, &data); err != nil {
		t.Fatalf("data 不是对象: %v", err)
	}
	if string(data["state"]) != `"open"` || string(data["path"]) != `"app/app.conf"` {
		t.Fatalf("data = %s", parseResult(t, rec).Data)
	}
}

// TestDeleteUploadOK 放弃上传只删暂存：响应明说 state=deleted，目标文件没被碰过。
func TestDeleteUploadOK(t *testing.T) {
	stub := &stubUploads{stubFiles: &stubFiles{}, info: sampleUpload()}
	srv, cookie, csrf := filesSession(t, stub)

	rec := uploadRequest(t, srv, http.MethodDelete, "/api/v1/files/uploads/"+sampleUploadID, cookie, csrf, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；响应 %s", rec.Code, rec.Body.String())
	}
	if stub.deletedID != sampleUploadID || stub.deleteCalls != 1 {
		t.Fatalf("放弃调用 = id:%q calls:%d", stub.deletedID, stub.deleteCalls)
	}
	if stub.createCalls != 0 || stub.putCalls != 0 {
		t.Fatalf("放弃不该伴随创建或上传：create=%d put=%d", stub.createCalls, stub.putCalls)
	}
	var data map[string]string
	if err := json.Unmarshal(parseResult(t, rec.ResponseRecorder).Data, &data); err != nil {
		t.Fatalf("data 不是对象: %v", err)
	}
	if data["upload_id"] != sampleUploadID || data["state"] != "deleted" {
		t.Fatalf("data = %v", data)
	}
}

// TestDeleteUploadReportsMissing 放弃一个不存在的暂存是 404，不是 200：
// “我删掉了”和“本来就没有”必须能分开。
func TestDeleteUploadReportsMissing(t *testing.T) {
	stub := &stubUploads{stubFiles: &stubFiles{}, info: sampleUpload(), deleteErr: files.ErrUploadNotFound}
	srv, cookie, csrf := filesSession(t, stub)

	rec := uploadRequest(t, srv, http.MethodDelete, "/api/v1/files/uploads/"+sampleUploadID, cookie, csrf, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d，期望 404；响应 %s", rec.Code, rec.Body.String())
	}
	if got := parseResult(t, rec.ResponseRecorder).Error; got == nil || got.Code != string(httpx.CodeNotFound) {
		t.Fatalf("错误码 = %+v", got)
	}
}

// TestFilesHasNoDirectWriteRoute 路由表里没有“直接把内容写到目标文件”的入口。
//
// 这条用例守的是整个 P06.2 的设计：上传只把字节放进暂存区，落地必须走
// file.commit_upload 的预检 → 任务。任何人以后想“顺手加一个写文件的 POST”，
// 都会先在这里被问一句为什么。
func TestFilesHasNoDirectWriteRoute(t *testing.T) {
	srv, cookie, csrf := filesSession(t, &stubUploads{stubFiles: &stubFiles{}, info: sampleUpload()})
	for _, route := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/files/write"},
		{http.MethodPut, "/api/v1/files/text"},
		{http.MethodPost, "/api/v1/files/upload"},
		{http.MethodPut, "/api/v1/files/download"},
	} {
		rec := uploadRequest(t, srv, route.method, route.path, cookie, csrf, strings.NewReader(uploadBody("5")))
		// 404 是“没有这条路由”，405 是“这条路径只有别的身份方法”（例如 /files/text
		// 只认 GET）。两者都说明同一件事：这里没有直写入口。
		if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s %s 状态码 = %d，期望 404/405（本层不提供直写路由）", route.method, route.path, rec.Code)
		}
	}
}
