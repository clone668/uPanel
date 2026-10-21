package agent

// P06.2 代理侧用例：/internal/v1/uploads 的四条路由。
//
// 这里要证明的是**判决没被吞掉**：
//
//  1. 声明形状在解码层就拒（unknown field、size_bytes 写成数字、首个对象后还有内容）；
//  2. files 的错误经 writeUploadError 直通：根未登记是 404、根只读是 403、
//     盘满是 413，而一个没被分类的错误是 500 并记日志——不会被兜成 200；
//  3. 路径拆分只认 `<id>` 与 `<id>/content` 两种形状，别的一律 404；
//     方法不对是 405 且带 Allow；
//  4. 收完的 PUT 把请求体**边读边交**给写端，并把请求体原样送达（不 base64、不进 JSON）。
//
// 文件系统行为（预留、TTL、原子 rename）由 internal/files 的 Linux 用例与 VM 验收覆盖。

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"upanel/internal/files"
	"upanel/internal/version"
)

// newUploadServer 起一台带文件写端的代理；src 为 nil 时代表“这个部署不能写”。
func newUploadServer(t *testing.T, src *fakeWriter) *Server {
	t.Helper()
	var writes FileWriterSource
	if src != nil {
		writes = src
	}
	srv, err := New(Options{
		SocketPath:      socketPathForTest(t),
		Version:         version.Info{Version: "test", Commit: "test", BuildDate: "test", GoVersion: "test"},
		Logger:          quietLogger(),
		ShutdownTimeout: time.Second,
		Peer:            testPeer(),
		Registry:        NewRegistry(),
		Secrets:         newTestSecretStore(t),
		Plans:           newTestPlanStore(t),
		Operations:      newTestOperationStore(t),
		FileWrites:      writes,
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	return srv
}

// uploadServe 与 serveAs 相同，但允许换一个 ResponseWriter：
// PUT 会通过 http.ResponseController 延长截止时间，httptest.ResponseRecorder
// 不支持它，所以流式用例需要一个补上这两个方法的记录器。
func uploadServe(srv *Server, req *http.Request, w http.ResponseWriter) {
	ctx := withPeerInfo(req.Context(), peerInfo{UID: testWebUID, PID: 31337})
	srv.Handler().ServeHTTP(w, req.WithContext(ctx))
}

// uploadRecorder 记录截止时间，用来证明长上传的路径真的执行了。
type uploadRecorder struct {
	*httptest.ResponseRecorder
	readDeadline  time.Time
	writeDeadline time.Time
}

func (u *uploadRecorder) SetReadDeadline(t time.Time) error  { u.readDeadline = t; return nil }
func (u *uploadRecorder) SetWriteDeadline(t time.Time) error { u.writeDeadline = t; return nil }

// internalUpload 发一次代理内部的上传请求。
func internalUpload(t *testing.T, srv *Server, method, path string, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-ID", "rid-upload")
	return serveAs(srv, req, testWebUID)
}

// TestUploadEndpointsAbsentWithoutWriter 没有写端时是 501，不是空响应：
// “这个部署不能上传”必须可判定。
func TestUploadEndpointsAbsentWithoutWriter(t *testing.T) {
	srv := newUploadServer(t, nil)
	cases := []struct{ method, path string }{
		{http.MethodPost, "/internal/v1/uploads"},
		{http.MethodGet, "/internal/v1/uploads/" + uploadIDForTest()},
		{http.MethodDelete, "/internal/v1/uploads/" + uploadIDForTest()},
		{http.MethodPut, "/internal/v1/uploads/" + uploadIDForTest() + "/content"},
	}
	for _, item := range cases {
		rec := internalUpload(t, srv, item.method, item.path, `{"root_id":"etc","path":"app.conf","size_bytes":"5"}`)
		if rec.Code != http.StatusNotImplemented {
			t.Fatalf("%s %s 状态码 = %d，期望 501；响应 %s", item.method, item.path, rec.Code, rec.Body.String())
		}
		if code := errorCode(t, rec); code != "NOT_IMPLEMENTED" {
			t.Fatalf("%s %s 错误码 = %q", item.method, item.path, code)
		}
	}
}

// TestCreateUploadDecodesStrictly 声明 JSON 严格解码：多一个字段就是一次误操作。
func TestCreateUploadDecodesStrictly(t *testing.T) {
	src := &fakeWriter{upload: files.UploadInfo{
		UploadID: uploadIDForTest(), RootID: "etc", Path: "app.conf", SizeBytes: 5, State: files.UploadStateOpen,
	}}
	srv := newUploadServer(t, src)

	cases := []struct{ name, body string }{
		{"不是 JSON", `size_bytes=5`},
		{"size_bytes 写成数字", `{"root_id":"etc","path":"app.conf","size_bytes":5}`},
		{"未知字段 overwrite", `{"root_id":"etc","path":"app.conf","size_bytes":"5","overwrite":true}`},
		{"首个对象之后还有内容", `{"root_id":"etc","path":"app.conf","size_bytes":"5"}{}`},
		{"缺少 root_id", `{"path":"app.conf","size_bytes":"5"}`},
	}
	for _, item := range cases {
		rec := internalUpload(t, srv, http.MethodPost, "/internal/v1/uploads", item.body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s 状态码 = %d，期望 400；响应 %s", item.name, rec.Code, rec.Body.String())
		}
		if code := errorCode(t, rec); code != "VALIDATION_ERROR" {
			t.Fatalf("%s 错误码 = %q", item.name, code)
		}
	}
	if len(src.calls) != 0 {
		t.Fatalf("被拒的声明不该到达写端，却调用了 %v", src.calls)
	}
}

// TestCreateUploadOK 合法声明转给写端并返回 201；字节数是十进制字符串。
func TestCreateUploadOK(t *testing.T) {
	src := &fakeWriter{upload: files.UploadInfo{
		UploadID: uploadIDForTest(), RootID: "etc", Path: "app.conf", SizeBytes: 5, State: files.UploadStateOpen,
	}}
	srv := newUploadServer(t, src)

	rec := internalUpload(t, srv, http.MethodPost, "/internal/v1/uploads",
		`{"root_id":"etc","path":"app.conf","size_bytes":"5"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("状态码 = %d，期望 201；响应 %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"size_bytes":5`) {
		t.Fatalf("size_bytes 必须序列化成字符串（超出 2^53 的字节数用 JSON 数字会失真）: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"size_bytes":"5"`) {
		t.Fatalf("size_bytes 不是十进制字符串: %s", rec.Body.String())
	}
	if len(src.calls) != 1 || src.calls[0] != "create_upload" {
		t.Fatalf("调用 = %v", src.calls)
	}
}

// TestCreateUploadMapsVerdicts files 的判决经 HTTPStatusOf 直通：
// “根没登记”和“盘满了”对运维是完全不同的两件事，不能都变成 500。
func TestCreateUploadMapsVerdicts(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"根未登记", files.ErrUnknownRoot, http.StatusNotFound, "NOT_FOUND"},
		{"根只读", files.ErrNotWritable, http.StatusForbidden, "FORBIDDEN"},
		{"盘满", files.ErrNoSpace, http.StatusRequestEntityTooLarge, "VALIDATION_ERROR"},
		{"超过上限", files.ErrTooLarge, http.StatusRequestEntityTooLarge, "VALIDATION_ERROR"},
		{"路径越界", files.ErrPathRejected, http.StatusUnprocessableEntity, "PATH_OUTSIDE_ROOT"},
		{"根不可用", files.ErrRootUnavailable, http.StatusConflict, "ROOT_UNAVAILABLE"},
		{"未分类失败", errors.New("底层读不到库"), http.StatusInternalServerError, "INTERNAL_ERROR"},
	}
	for _, item := range cases {
		src := &fakeWriter{uploadErr: item.err}
		srv := newUploadServer(t, src)
		rec := internalUpload(t, srv, http.MethodPost, "/internal/v1/uploads",
			`{"root_id":"etc","path":"app.conf","size_bytes":"5"}`)
		if rec.Code != item.status {
			t.Fatalf("%s 状态码 = %d，期望 %d；响应 %s", item.name, rec.Code, item.status, rec.Body.String())
		}
		if code := errorCode(t, rec); code != item.code {
			t.Fatalf("%s 错误码 = %q，期望 %s", item.name, code, item.code)
		}
	}
}

// TestUploadByIDRoutes 只有 `<id>` 与 `<id>/content` 两种形状：
// 前缀路由必须自己判断后缀，别的一律 404；方法不对是 405 且带 Allow。
func TestUploadByIDRoutes(t *testing.T) {
	src := &fakeWriter{upload: files.UploadInfo{
		UploadID: uploadIDForTest(), RootID: "etc", Path: "app.conf", SizeBytes: 5, State: files.UploadStateReceived,
	}}
	srv := newUploadServer(t, src)

	get := internalUpload(t, srv, http.MethodGet, "/internal/v1/uploads/"+uploadIDForTest(), "")
	if get.Code != http.StatusOK {
		t.Fatalf("GET 状态码 = %d；响应 %s", get.Code, get.Body.String())
	}
	del := internalUpload(t, srv, http.MethodDelete, "/internal/v1/uploads/"+uploadIDForTest(), "")
	if del.Code != http.StatusOK || !strings.Contains(del.Body.String(), `"state":"deleted"`) {
		t.Fatalf("DELETE 状态码 = %d；响应 %s", del.Code, del.Body.String())
	}

	// 多一层后缀不是 content：既不是 id 也不是内容入口。
	if rec := internalUpload(t, srv, http.MethodPut, "/internal/v1/uploads/"+uploadIDForTest()+"/other", "x"); rec.Code != http.StatusNotFound {
		t.Fatalf("未知后缀状态码 = %d，期望 404；响应 %s", rec.Code, rec.Body.String())
	}
	// 空 id。
	if rec := internalUpload(t, srv, http.MethodGet, "/internal/v1/uploads/", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("空 id 状态码 = %d，期望 404", rec.Code)
	}
	// 方法不对：PUT 一个没有 /content 的路径是 405，Allow 里没有 PUT。
	rec := internalUpload(t, srv, http.MethodPut, "/internal/v1/uploads/"+uploadIDForTest(), "x")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT 到 id 状态码 = %d，期望 405；响应 %s", rec.Code, rec.Body.String())
	}
	if allow := rec.Header().Get("Allow"); !strings.Contains(allow, "GET") || !strings.Contains(allow, "DELETE") {
		t.Fatalf("Allow = %q", allow)
	}
	// 内容入口只收 PUT。
	rec = internalUpload(t, srv, http.MethodGet, "/internal/v1/uploads/"+uploadIDForTest()+"/content", "")
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "PUT" {
		t.Fatalf("GET content 状态码 = %d，Allow = %q", rec.Code, rec.Header().Get("Allow"))
	}
}

// TestUploadContentStreamsBody 收完的 PUT：请求体边读边交给写端，响应是暂存视图；
// 目标路径一个字节都没变（写端只收到“收内容”这一个调用）。
func TestUploadContentStreamsBody(t *testing.T) {
	src := &fakeWriter{upload: files.UploadInfo{
		UploadID: uploadIDForTest(), RootID: "etc", Path: "app.conf", SizeBytes: 5, State: files.UploadStateReceived,
	}}
	srv := newUploadServer(t, src)

	req := httptest.NewRequest(http.MethodPut, "/internal/v1/uploads/"+uploadIDForTest()+"/content",
		strings.NewReader("hello"))
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Request-ID", "rid-upload")
	rec := &uploadRecorder{ResponseRecorder: httptest.NewRecorder()}
	uploadServe(srv, req, rec)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；响应 %s", rec.Code, rec.Body.String())
	}
	if rec.readDeadline.IsZero() {
		t.Fatal("长上传必须推动读截止时间，否则第 15 秒会被代理的 ReadTimeout 掐断")
	}
	if len(src.calls) != 1 || src.calls[0] != "write_upload_content" {
		t.Fatalf("调用 = %v，期望只有一次收内容", src.calls)
	}
	if !strings.Contains(rec.Body.String(), `"state":"received"`) {
		t.Fatalf("响应 = %s", rec.Body.String())
	}
}

// TestSplitUploadPath 前缀路由的拆分规则本身也值得一条用例：
// 它决定了“哪一个 id 会被拼进写端的调用”，拆错就等于把路径交给别人。
func TestSplitUploadPath(t *testing.T) {
	cases := []struct{ path, id, rest string }{
		{"/internal/v1/uploads", "", ""},
		{"/internal/v1/uploads/", "", ""},
		{"/internal/v1/uploads/up_1", "up_1", ""},
		{"/internal/v1/uploads/up_1/content", "up_1", "content"},
		{"/internal/v1/uploads/up_1/a/b", "up_1", "a/b"},
		{"/internal/v1/other", "", ""},
	}
	for _, item := range cases {
		id, rest := splitUploadPath(item.path)
		if id != item.id || rest != item.rest {
			t.Fatalf("%s → (%q,%q)，期望 (%q,%q)", item.path, id, rest, item.id, item.rest)
		}
	}
}
