package agent

// P06.1 代理侧用例：文件查询登记、状态码直通与下载流。
//
// 这一层不碰 openat2（那是 internal/files 的 Linux 用例的事），它要证明的是
// **中间这一层没有把判决弄丢**：
//
//  1. 没登记文件读取端时，三条查询不存在（未登记的查询是 400，不是“返回空列表”）；
//  2. 参数形状是固定的：多出来的字段（尤其是 {"path":"/etc/shadow"} 之外的 command）
//     在解码层就被拒；
//  3. files 的判决经 statusCarrier 直通：越界是 422 PATH_OUTSIDE_ROOT、
//     版本冲突是 409 VERSION_CONFLICT，而不是被压成 500（那会让"越界"看起来像"代理坏了"）；
//  4. 下载走一次性许可的流：首行是 JSON 元数据，其后是原始字节，顺序不能反。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"upanel/internal/files"
	"upanel/internal/version"
)

// fakeFiles 是 FilesSource 的假实现。
type fakeFiles struct {
	rootsResult files.RootsResult
	page        files.ListPage
	listErr     error
	text        files.TextFile
	textErr     error
	download    *files.Download
	downloadErr error

	listParams     files.ListParams
	textParams     files.TextParams
	downloadParams files.DownloadParams
}

func (f *fakeFiles) Roots(context.Context) files.RootsResult { return f.rootsResult }

func (f *fakeFiles) List(_ context.Context, params files.ListParams) (files.ListPage, error) {
	f.listParams = params
	if f.listErr != nil {
		return files.ListPage{}, f.listErr
	}
	return f.page, nil
}

func (f *fakeFiles) Text(_ context.Context, params files.TextParams) (files.TextFile, error) {
	f.textParams = params
	if f.textErr != nil {
		return files.TextFile{}, f.textErr
	}
	return f.text, nil
}

func (f *fakeFiles) OpenDownload(_ context.Context, params files.DownloadParams) (*files.Download, error) {
	f.downloadParams = params
	if f.downloadErr != nil {
		return nil, f.downloadErr
	}
	return f.download, nil
}

// newServerWithFiles 构造一个带文件读取端的代理：三条查询登记进登记表，
// 读取端同时进 Options.Files（下载流用），与生产里 RegisterProductionQueries 的做法一致。
func newServerWithFiles(t *testing.T, src FilesSource) *Server {
	t.Helper()
	reg := NewRegistry()
	if src != nil {
		for _, query := range []Query{
			newFilesRootsQuery(src),
			newFilesListQuery(src),
			newFilesTextQuery(src),
		} {
			if err := reg.RegisterQuery(query); err != nil {
				t.Fatalf("登记 %s 失败: %v", query.Name, err)
			}
		}
	}
	srv, err := New(Options{
		SocketPath:      socketPathForTest(t),
		Version:         version.Info{Version: "test", Commit: "test", BuildDate: "test", GoVersion: "test"},
		Logger:          quietLogger(),
		ShutdownTimeout: time.Second,
		Peer:            testPeer(),
		Registry:        reg,
		Secrets:         newTestSecretStore(t),
		Plans:           newTestPlanStore(t),
		Operations:      newTestOperationStore(t),
		Files:           src,
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	return srv
}

// TestFilesQueriesAbsentWithoutSource 没接文件读取端时查询不存在。
//
// 这条很关键：如果这里改成"返回空列表"，界面会显示"未登记任何文件根"，
// 而真相是"这台机器上文件管理没配置"。两者对运维是不同的结论。
func TestFilesQueriesAbsentWithoutSource(t *testing.T) {
	srv := newServerWithFiles(t, nil)
	for _, name := range []string{FilesRootsQueryName, FilesListQueryName, FilesTextQueryName} {
		rec := postJSON(t, srv, "/internal/v1/queries", fmt.Sprintf(`{"query":%q,"params":{}}`, name))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s 状态码 = %d，期望 400；body=%s", name, rec.Code, rec.Body.String())
		}
		if code := errorCode(t, rec); code != "VALIDATION_ERROR" {
			t.Fatalf("%s 错误码 = %q", name, code)
		}
	}
}

// TestFilesRootsQueryOK 列根返回契约形状。
func TestFilesRootsQueryOK(t *testing.T) {
	src := &fakeFiles{rootsResult: files.RootsResult{Roots: []files.RootInfo{{
		RootID: "etc", Name: "配置", Path: "/etc", Available: true,
	}}}}
	srv := newServerWithFiles(t, src)

	rec := postJSON(t, srv, "/internal/v1/queries", `{"query":"files.roots","params":{}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d；body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"root_id":"etc"`) {
		t.Fatalf("响应缺少根：%s", rec.Body.String())
	}
}

// TestFilesListDecodesParams 参数原样到达读取端（含游标）。
func TestFilesListDecodesParams(t *testing.T) {
	src := &fakeFiles{page: files.ListPage{RootID: "etc", Path: "nginx"}}
	srv := newServerWithFiles(t, src)

	rec := postJSON(t, srv, "/internal/v1/queries",
		`{"query":"files.list","params":{"root_id":"etc","path":"nginx","cursor":"nginx.conf"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d；body=%s", rec.Code, rec.Body.String())
	}
	if src.listParams.RootID != "etc" || src.listParams.Path != "nginx" || src.listParams.Cursor != "nginx.conf" {
		t.Fatalf("参数 = %+v", src.listParams)
	}
}

// TestFilesQueriesRejectUnknownFields 参数形状固定：多余字段一律拒绝。
func TestFilesQueriesRejectUnknownFields(t *testing.T) {
	srv := newServerWithFiles(t, &fakeFiles{})
	for _, body := range []string{
		`{"query":"files.roots","params":{"path":"/etc"}}`,
		`{"query":"files.list","params":{"root_id":"etc","path":"","command":"ls"}}`,
		`{"query":"files.text","params":{"root_id":"etc","path":"x","follow":true}}`,
	} {
		rec := postJSON(t, srv, "/internal/v1/queries", body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s → 状态码 %d，期望 400；body=%s", body, rec.Code, rec.Body.String())
		}
	}
}

// TestFilesQueriesRejectBadPaths 形状不合法的路径在解码层就被拒（400）。
func TestFilesQueriesRejectBadPaths(t *testing.T) {
	srv := newServerWithFiles(t, &fakeFiles{})
	for _, body := range []string{
		`{"query":"files.list","params":{"root_id":"etc","path":"../../etc"}}`,
		`{"query":"files.list","params":{"root_id":"etc","path":"/etc"}}`,
		`{"query":"files.list","params":{"root_id":"../etc","path":""}}`,
		`{"query":"files.text","params":{"root_id":"etc","path":""}}`,
	} {
		rec := postJSON(t, srv, "/internal/v1/queries", body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s → 状态码 %d，期望 400；body=%s", body, rec.Code, rec.Body.String())
		}
	}
}

// TestFilesVerdictsPassThrough 读取端的判决原样变成 HTTP 状态码与错误码。
//
// 这里断言的是 P06.1 最关键的一条链路：拒绝不是"代理出错"，而是"这个请求不被允许"。
func TestFilesVerdictsPassThrough(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"越界", fmt.Errorf("%w: path 含 '..' 段", files.ErrPathRejected), 422, "PATH_OUTSIDE_ROOT"},
		{"硬链接", fmt.Errorf("%w: 文件有 2 个硬链接", files.ErrHardLinked), 422, "PATH_OUTSIDE_ROOT"},
		{"不存在", fmt.Errorf("%w: 没有这个文件", files.ErrNotFound), 404, "NOT_FOUND"},
		{"根不可用", fmt.Errorf("%w: 根的 inode 变了", files.ErrRootUnavailable), 409, "ROOT_UNAVAILABLE"},
		{"版本冲突", fmt.Errorf("%w: 文件已改变", files.ErrVersionConflict), 409, "VERSION_CONFLICT"},
		{"不是文本", fmt.Errorf("%w: 不是 UTF-8", files.ErrNotText), 422, "FILE_NOT_TEXT"},
		{"太大", fmt.Errorf("%w: 超过 1 MiB", files.ErrTooLarge), 413, "VALIDATION_ERROR"},
		{"平台不支持", files.ErrUnsupported, 501, "NOT_IMPLEMENTED"},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			src := &fakeFiles{listErr: item.err}
			srv := newServerWithFiles(t, src)
			rec := postJSON(t, srv, "/internal/v1/queries",
				`{"query":"files.list","params":{"root_id":"etc","path":""}}`)
			if rec.Code != item.status {
				t.Fatalf("状态码 = %d，期望 %d；body=%s", rec.Code, item.status, rec.Body.String())
			}
			if code := errorCode(t, rec); code != item.code {
				t.Fatalf("错误码 = %q，期望 %q", code, item.code)
			}
		})
	}
}

// TestFilesRealFailuresStay500 真正的执行故障没有被顺手报成 4xx。
func TestFilesRealFailuresStay500(t *testing.T) {
	srv := newServerWithFiles(t, &fakeFiles{listErr: errors.New("openat2: 磁盘满了")})
	rec := postJSON(t, srv, "/internal/v1/queries",
		`{"query":"files.list","params":{"root_id":"etc","path":""}}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d，期望 500；body=%s", rec.Code, rec.Body.String())
	}
}

// createFilePermit 申请一条下载流许可。与 createPermit 的区别只有一点：
// 它不假定用途是 logs.stream——这里验的正是 files.download 这条通道。
func createFilePermit(t *testing.T, srv *Server, body string) string {
	t.Helper()
	rec := postJSON(t, srv, "/internal/v1/streams", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("创建许可状态码 = %d；body=%s", rec.Code, rec.Body.String())
	}
	var env struct {
		Data streamResponse `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("解析许可响应: %v", err)
	}
	if env.Data.StreamID == "" || env.Data.Purpose != StreamPurposeDownload || env.Data.ExpiresAt.IsZero() {
		t.Fatalf("许可响应缺少必需字段: %+v", env.Data)
	}
	return env.Data.StreamID
}

// openTempFile 把内容写进临时文件并打开，用来构造一个真实的 *files.Download。
func openTempFile(t *testing.T, name, content string) *os.File {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写临时文件失败: %v", err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("打开临时文件失败: %v", err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return file
}

// TestFilesDownloadStream 下载流的帧格式：首行 JSON 元数据 + 原始字节。
func TestFilesDownloadStream(t *testing.T) {
	content := "Port 22\nPermitRootLogin no\n"
	file := openTempFile(t, "sshd_config", content)
	src := &fakeFiles{download: &files.Download{
		File: file,
		Header: files.DownloadHeader{
			Name:      "sshd_config",
			SizeBytes: uint64(len(content)),
			Version:   "v1-a1b2-c3d4-1b-17a0b1c2d3e4",
			MTime:     time.Date(2026, 10, 21, 2, 59, 0, 0, time.UTC),
		},
	}}
	srv := newServerWithFiles(t, src)

	id := createFilePermit(t, srv, `{"purpose":"files.download","params":{"root_id":"etc","path":"ssh/sshd_config"}}`)
	rec := serveAs(srv, httptest.NewRequest(http.MethodGet, "/internal/v1/streams/"+id, nil), testWebUID)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d；body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q", cc)
	}
	if src.downloadParams.Path != "ssh/sshd_config" {
		t.Fatalf("下载参数 = %+v", src.downloadParams)
	}

	body := rec.Body.Bytes()
	index := strings.IndexByte(string(body), '\n')
	if index < 0 {
		t.Fatalf("响应里没有首行分隔：%q", string(body))
	}
	var header files.DownloadHeader
	if err := json.Unmarshal(body[:index], &header); err != nil {
		t.Fatalf("首行不是 DownloadHeader：%v（%q）", err, string(body[:index]))
	}
	if header.Name != "sshd_config" || header.SizeBytes != uint64(len(content)) ||
		header.Version != "v1-a1b2-c3d4-1b-17a0b1c2d3e4" {
		t.Fatalf("首行内容 = %+v", header)
	}
	// 首行之后必须**原样**是文件字节：多一个换行、少一个字节都是错的文件。
	if got := string(body[index+1:]); got != content {
		t.Fatalf("文件字节 = %q，期望 %q", got, content)
	}
}

// TestFilesDownloadStreamRejected 打开失败时是错误信封，不是 octet-stream：
// 否则浏览器会把一段 JSON 保存成文件。
func TestFilesDownloadStreamRejected(t *testing.T) {
	src := &fakeFiles{downloadErr: fmt.Errorf("%w: path 含 '..' 段", files.ErrPathRejected)}
	srv := newServerWithFiles(t, src)

	id := createFilePermit(t, srv, `{"purpose":"files.download","params":{"root_id":"etc","path":"nginx.conf"}}`)
	rec := serveAs(srv, httptest.NewRequest(http.MethodGet, "/internal/v1/streams/"+id, nil), testWebUID)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("状态码 = %d，期望 422；body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); strings.Contains(ct, "octet-stream") {
		t.Fatalf("Content-Type = %q，失败响应不能声明成二进制流", ct)
	}
	if code := errorCode(t, rec); code != "PATH_OUTSIDE_ROOT" {
		t.Fatalf("错误码 = %q", code)
	}
}

// TestFilesDownloadPermitSingleUse 许可一次性：第二次 GET 是 404。
func TestFilesDownloadPermitSingleUse(t *testing.T) {
	content := "ok\n"
	file := openTempFile(t, "a.txt", content)
	src := &fakeFiles{download: &files.Download{
		File:   file,
		Header: files.DownloadHeader{Name: "a.txt", SizeBytes: uint64(len(content)), Version: "v1-1-2-3-4"},
	}}
	srv := newServerWithFiles(t, src)

	id := createFilePermit(t, srv, `{"purpose":"files.download","params":{"root_id":"etc","path":"a.txt"}}`)
	first := serveAs(srv, httptest.NewRequest(http.MethodGet, "/internal/v1/streams/"+id, nil), testWebUID)
	if first.Code != http.StatusOK {
		t.Fatalf("第一次 GET 状态码 = %d", first.Code)
	}
	second := serveAs(srv, httptest.NewRequest(http.MethodGet, "/internal/v1/streams/"+id, nil), testWebUID)
	if second.Code != http.StatusNotFound {
		t.Fatalf("第二次 GET 状态码 = %d，期望 404", second.Code)
	}
}

// TestFilesDownloadParamsRejectedAtCreate 形状不合法的路径在创建许可时就被拒，
// 不会先发一个许可再在 GET 时失败。
func TestFilesDownloadParamsRejectedAtCreate(t *testing.T) {
	srv := newServerWithFiles(t, &fakeFiles{})
	rec := postJSON(t, srv, "/internal/v1/streams",
		`{"purpose":"files.download","params":{"root_id":"etc","path":"../../etc/shadow"}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400；body=%s", rec.Code, rec.Body.String())
	}
	if srv.streams.count() != 0 {
		t.Fatalf("被拒的请求不应留下许可，剩余 %d", srv.streams.count())
	}
}
