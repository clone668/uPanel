package api

// P06.1 接口层用例：文件根、列举、文本与下载。
//
// 这一层要证明的是**四件事**，每件都对应一条验收条款：
//
//  1. 整组接口 admin 专属：未登录 401、viewer 403（包括下载——挡住列表却敞开下载等于没挡）。
//  2. 请求参数在 HTTP 层就被拒：没有 root_id、绝对路径、`..`、超长 cursor 一律 400，
//     而不是带着这些值继续往下走，靠底层"应该会拦住"。
//  3. 代理的判决原样转出：PATH_OUTSIDE_ROOT 是 422、VERSION_CONFLICT 是 409，
//     不能因为经过 Web 就变成 500 或 503（那会让"越界"看起来像"服务坏了"）。
//  4. 下载不走 JSON：octet-stream + nosniff + Content-Disposition（ASCII 退化 + RFC 5987），
//     Content-Length 与 X-UPanel-File-Version 描述的必须是**同一份**字节。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"upanel/internal/auth"
	"upanel/internal/config"
	"upanel/internal/files"
	"upanel/internal/httpx"
	"upanel/internal/platform"
	"upanel/internal/version"
)

// stubFiles 是 FilesReader 的测试替身。
//
// 它不冒充 openat2：路径约束与句柄核验由 internal/files 的 Linux 用例覆盖，
// 这里只验 HTTP 层的权限、参数校验、错误映射与下载响应头。
type stubFiles struct {
	roots    files.RootsResult
	rootsErr error

	page    files.ListPage
	listErr error

	text    files.TextFile
	textErr error

	download    *files.RemoteDownload
	downloadErr error

	listParams     files.ListParams
	textParams     files.TextParams
	downloadParams files.DownloadParams
}

func (s *stubFiles) Roots(context.Context) (files.RootsResult, error) {
	if s.rootsErr != nil {
		return files.RootsResult{}, s.rootsErr
	}
	return s.roots, nil
}

func (s *stubFiles) List(_ context.Context, params files.ListParams) (files.ListPage, error) {
	s.listParams = params
	if s.listErr != nil {
		return files.ListPage{}, s.listErr
	}
	return s.page, nil
}

func (s *stubFiles) Text(_ context.Context, params files.TextParams) (files.TextFile, error) {
	s.textParams = params
	if s.textErr != nil {
		return files.TextFile{}, s.textErr
	}
	return s.text, nil
}

func (s *stubFiles) OpenDownload(_ context.Context, params files.DownloadParams) (*files.RemoteDownload, error) {
	s.downloadParams = params
	if s.downloadErr != nil {
		return nil, s.downloadErr
	}
	return s.download, nil
}

// 中文与引号都在里面：Content-Disposition 的两种形式各要能对上。
const sampleFileName = `"配置 备份".conf`

func sampleDownload(body string) *files.RemoteDownload {
	return &files.RemoteDownload{
		Header: files.DownloadHeader{
			Name:      sampleFileName,
			SizeBytes: uint64(len(body)),
			Version:   "v1-a1b2-c3d4-5-6",
			MTime:     time.Date(2026, 10, 21, 2, 59, 0, 0, time.UTC),
		},
		Reader: strings.NewReader(body),
	}
}

func sampleRoots() files.RootsResult {
	return files.RootsResult{
		Roots: []files.RootInfo{
			{
				RootID:     "etc",
				Name:       "配置",
				Path:       "/etc",
				Writable:   false,
				Available:  true,
				MountPoint: "/",
				FSType:     "ext4",
			},
		},
	}
}

// ptrTo 给“本页还有下一页”造一个非 nil 的游标（null 表示尾页，见 files.ListPage）。
func ptrTo(s string) *string { return &s }

func sampleFilePage() files.ListPage {
	return files.ListPage{
		RootID: "etc",
		Path:   "nginx",
		Entries: []files.Entry{
			{
				Name:      "nginx.conf",
				Type:      "file",
				SizeBytes: 3222,
				Mode:      "0644",
				Owner:     files.Owner{UID: 0, GID: 0},
				MTime:     time.Date(2026, 10, 21, 2, 59, 0, 0, time.UTC),
				Version:   "v1-a1b2-c3d4-c96-17a0b1c2d3e4",
			},
		},
		NextCursor: ptrTo("nginx.conf"),
	}
}

func filesFixture(t *testing.T, reader FilesReader) (*Server, *http.Cookie) {
	t.Helper()
	authSvc, aud, db := newTestServicesWithDB(t)
	srv, err := New(Options{
		ListenAddress:   config.ListenProduction,
		Mode:            platform.ModeProduction,
		Version:         version.Info{Version: "test", GoVersion: "test"},
		Logger:          quietLogger(),
		ShutdownTimeout: time.Second,
		Auth:            authSvc,
		Audit:           aud,
		Operations:      newTestOperations(t, db, nil),
		AllowedOrigins:  testOrigins(),
		Files:           reader,
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	seedTestUser(t, srv.opts.Auth, testUser, testUserPass)
	cookie, _ := loginOK(t, srv)
	return srv, cookie
}

// filePaths 是所有文件接口，权限用例逐条覆盖：只挡其中一条等于没挡。
func filePaths() []string {
	return []string{
		"/api/v1/files/roots",
		"/api/v1/files/list?root_id=etc&path=",
		"/api/v1/files/text?root_id=etc&path=nginx.conf",
		"/api/v1/files/download?root_id=etc&path=nginx.conf",
	}
}

// TestFilesRequiresSession 未登录一律 401。
func TestFilesRequiresSession(t *testing.T) {
	srv, _ := filesFixture(t, &stubFiles{})
	for _, path := range filePaths() {
		rec := adminGet(t, srv, nil, path)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s 未登录状态码 = %d，期望 401", path, rec.Code)
		}
	}
}

// TestFilesForbiddenForViewer 文件内容是管理员能力（PLAN.md 第 15 节）。
func TestFilesForbiddenForViewer(t *testing.T) {
	srv, _ := filesFixture(t, &stubFiles{})
	if _, err := srv.opts.Auth.CreateUser(context.Background(), auth.CreateUserInput{
		Username: "watcher", DisplayName: "watcher", Role: auth.RoleViewer, Password: "watcher-horse-9",
	}); err != nil {
		t.Fatalf("创建 viewer 失败: %v", err)
	}
	cookie, _ := loginAsUser(t, srv, "watcher", "watcher-horse-9")
	for _, path := range filePaths() {
		rec := adminGet(t, srv, cookie, path)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s viewer 状态码 = %d，期望 403", path, rec.Code)
		}
		if got := parseResult(t, rec).Error; got == nil || got.Code != string(httpx.CodeForbidden) {
			t.Fatalf("%s 错误码 = %+v", path, got)
		}
	}
}

// TestFilesUnavailableWithoutAgent 没有配置代理时是 503，不是空列表：
// “没接上”和“一个根都没有”必须是两件事。
func TestFilesUnavailableWithoutAgent(t *testing.T) {
	srv, cookie := filesFixture(t, nil)
	for _, path := range filePaths() {
		rec := adminGet(t, srv, cookie, path)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s 状态码 = %d，期望 503", path, rec.Code)
		}
		got := parseResult(t, rec).Error
		if got == nil || got.Code != string(httpx.CodeAgentUnavailable) {
			t.Fatalf("%s 错误码 = %+v", path, got)
		}
	}
}

// TestFileRootsOK 根列表按契约返回（含不可用原因与容量为 null 的情况）。
func TestFileRootsOK(t *testing.T) {
	stub := &stubFiles{roots: sampleRoots()}
	srv, cookie := filesFixture(t, stub)

	rec := adminGet(t, srv, cookie, "/api/v1/files/roots")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；响应 %s", rec.Code, rec.Body.String())
	}
	result := parseResult(t, rec)
	if result.Error != nil {
		t.Fatalf("不应有错误: %+v", result.Error)
	}
	// 用 RawMessage 逐个字段看：要区分“capacity 字段不存在”和“capacity 显式为 null”，
	// 后者是契约要求的形状（前端据此显示“容量未读出”）。
	var data struct {
		Roots []map[string]json.RawMessage `json:"roots"`
	}
	if err := json.Unmarshal(result.Data, &data); err != nil {
		t.Fatalf("data 不是契约形状: %v（%s）", err, result.Data)
	}
	if len(data.Roots) != 1 {
		t.Fatalf("roots 数量 = %d", len(data.Roots))
	}
	root := data.Roots[0]
	if string(root["root_id"]) != `"etc"` || string(root["available"]) != "true" {
		t.Fatalf("root = %#v", root)
	}
	raw, exists := root["capacity"]
	if !exists {
		t.Fatalf("capacity 字段缺失：界面无法区分“没读出”和“0 字节可用”")
	}
	if string(raw) != "null" {
		t.Fatalf("capacity = %s，期望显式 null", raw)
	}
}

// TestFileListRejectsBadParams 参数错误**不会**把请求转给代理，且状态码/错误码
// 与本地的形状校验、代理的判决共用同一张映射表：
//
//   - 路径越界（绝对路径、`..` 段）是 422 PATH_OUTSIDE_ROOT——它在 Web 层被拦下
//     和在代理层被拦下必须是同一个答案，前端只写一套处理；
//   - 形状问题（缺 root_id、空/重复段、超长、游标不合法）才是 400 VALIDATION_ERROR。
func TestFileListRejectsBadParams(t *testing.T) {
	stub := &stubFiles{page: sampleFilePage()}
	srv, cookie := filesFixture(t, stub)

	cases := []struct {
		path   string
		status int
		code   httpx.Code
	}{
		{"/api/v1/files/list?path=", http.StatusBadRequest, httpx.CodeValidationError},
		{"/api/v1/files/list?root_id=etc&path=/etc/passwd", http.StatusUnprocessableEntity, httpx.CodePathOutsideRoot},
		{"/api/v1/files/list?root_id=etc&path=..", http.StatusUnprocessableEntity, httpx.CodePathOutsideRoot},
		{"/api/v1/files/list?root_id=etc&path=a/../b", http.StatusUnprocessableEntity, httpx.CodePathOutsideRoot},
		{"/api/v1/files/list?root_id=etc&path=" + strings.Repeat("a", 4097), http.StatusBadRequest, httpx.CodeValidationError},
		{"/api/v1/files/list?root_id=etc&path=&cursor=" + strings.Repeat("c", 4097), http.StatusBadRequest, httpx.CodeValidationError},
	}
	for _, item := range cases {
		rec := adminGet(t, srv, cookie, item.path)
		if rec.Code != item.status {
			t.Fatalf("%s → %d，期望 %d；响应 %s", item.path, rec.Code, item.status, rec.Body.String())
		}
		if got := parseResult(t, rec).Error; got == nil || got.Code != string(item.code) {
			t.Fatalf("%s 错误码 = %+v，期望 %s", item.path, got, item.code)
		}
	}
	if stub.listParams.RootID != "" {
		t.Fatalf("被拒的请求不应该到达代理，却收到了 %+v", stub.listParams)
	}
}

// TestFileListOK 正常列举把 root_id / path / cursor 原样交给下层。
func TestFileListOK(t *testing.T) {
	stub := &stubFiles{page: sampleFilePage()}
	srv, cookie := filesFixture(t, stub)

	rec := adminGet(t, srv, cookie, "/api/v1/files/list?root_id=etc&path=nginx&cursor=nginx.conf")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d；响应 %s", rec.Code, rec.Body.String())
	}
	if stub.listParams.RootID != "etc" || stub.listParams.Path != "nginx" || stub.listParams.Cursor != "nginx.conf" {
		t.Fatalf("参数传递 = %+v", stub.listParams)
	}
	body := rec.Body.String()
	// size_bytes 是按字符串输出的：JS 的安全整数装不下大文件。
	if !strings.Contains(body, `"size_bytes":"3222"`) {
		t.Fatalf("size_bytes 未按字符串输出：%s", body)
	}
}

// TestFileTextForwardsAgentVerdict 代理的拒绝原样转出，不二次归类。
func TestFileTextForwardsAgentVerdict(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		code   httpx.Code
	}{
		{"越界", &files.RemoteError{Status: 422, Code: httpx.CodePathOutsideRoot, Message: "路径越界"}, 422, httpx.CodePathOutsideRoot},
		{"版本冲突", &files.RemoteError{Status: 409, Code: httpx.CodeVersionConflict, Message: "文件已改变"}, 409, httpx.CodeVersionConflict},
		{"根不可用", &files.RemoteError{Status: 409, Code: httpx.CodeRootUnavailable, Message: "根已被替换"}, 409, httpx.CodeRootUnavailable},
		{"不是文本", &files.RemoteError{Status: 422, Code: httpx.CodeFileNotText, Message: "不是文本文件"}, 422, httpx.CodeFileNotText},
		{"不存在", &files.RemoteError{Status: 404, Code: httpx.CodeNotFound, Message: "没有这个文件"}, 404, httpx.CodeNotFound},
		{"代理不可用", files.ErrAgentUnavailable, 503, httpx.CodeAgentUnavailable},
		{"未知故障", errors.New("boom"), 500, httpx.CodeInternalError},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			stub := &stubFiles{textErr: item.err}
			srv, cookie := filesFixture(t, stub)
			rec := adminGet(t, srv, cookie, "/api/v1/files/text?root_id=etc&path=nginx.conf")
			if rec.Code != item.status {
				t.Fatalf("状态码 = %d，期望 %d；响应 %s", rec.Code, item.status, rec.Body.String())
			}
			got := parseResult(t, rec).Error
			if got == nil || got.Code != string(item.code) {
				t.Fatalf("错误码 = %+v，期望 %s", got, item.code)
			}
		})
	}
}

// TestFileTextParamsCarryExpectedVersion expected_version 必须到达代理：
// 少了它，“文件已改变”就永远发现不了。
func TestFileTextParamsCarryExpectedVersion(t *testing.T) {
	stub := &stubFiles{text: files.TextFile{
		RootID: "etc", Path: "hostname", Version: "v1-1-2-3-4", SizeBytes: 12,
		Encoding: "utf-8", Content: "upanel\n",
	}}
	srv, cookie := filesFixture(t, stub)

	rec := adminGet(t, srv, cookie, "/api/v1/files/text?root_id=etc&path=hostname&expected_version=v1-1-2-3-4")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d；响应 %s", rec.Code, rec.Body.String())
	}
	if stub.textParams.ExpectedVersion != "v1-1-2-3-4" {
		t.Fatalf("expected_version 丢失：%+v", stub.textParams)
	}
}

// TestFileDownloadStreamsBytes 下载的响应头与字节必须是同一份文件。
func TestFileDownloadStreamsBytes(t *testing.T) {
	body := "Port 22\nPermitRootLogin no\n"
	stub := &stubFiles{download: sampleDownload(body)}
	srv, cookie := filesFixture(t, stub)

	rec := adminGet(t, srv, cookie, "/api/v1/files/download?root_id=etc&path=nginx.conf")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d；响应 %s", rec.Code, rec.Body.String())
	}
	h := rec.Header()
	if got := h.Get("Content-Type"); got != "application/octet-stream" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := h.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q（浏览器不得把内容当 HTML 渲染）", got)
	}
	if got := h.Get("Content-Length"); got != "27" {
		t.Fatalf("Content-Length = %q，期望 27", got)
	}
	if got := h.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
	if got := h.Get("X-UPanel-File-Version"); got != "v1-a1b2-c3d4-5-6" {
		t.Fatalf("X-UPanel-File-Version = %q", got)
	}
	// 文件名里既有中文又有引号：ASCII 退化形式要把引号换成 '_'，RFC 5987 形式给原文。
	cd := h.Get("Content-Disposition")
	if !strings.HasPrefix(cd, `attachment; filename="`) {
		t.Fatalf("Content-Disposition 缺少 ASCII 形式：%q", cd)
	}
	if strings.Contains(cd, "\n") || strings.Contains(cd, "\r") {
		t.Fatalf("Content-Disposition 含换行（响应拆分）：%q", cd)
	}
	if !strings.Contains(cd, "filename*=UTF-8''") {
		t.Fatalf("Content-Disposition 缺少 RFC 5987 形式：%q", cd)
	}
	if rec.Body.String() != body {
		t.Fatalf("响应体 = %q，期望 %q", rec.Body.String(), body)
	}
}

// TestFileDownloadRejectedBeforeHeaders 打开失败时回的是错误信封，不是空文件。
func TestFileDownloadRejectedBeforeHeaders(t *testing.T) {
	stub := &stubFiles{downloadErr: &files.RemoteError{
		Status: 422, Code: httpx.CodePathOutsideRoot, Message: "路径越界",
	}}
	srv, cookie := filesFixture(t, stub)

	// 路径形状合法（`..` 在 HTTP 层就会被拒），这里要验的是代理在打开句柄之后
	// 才发现的越界（例如硬链接指向根外）：判决必须原样到这里。
	rec := adminGet(t, srv, cookie, "/api/v1/files/download?root_id=etc&path=nginx.conf")
	if rec.Code != 422 {
		t.Fatalf("状态码 = %d，期望 422；响应 %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("Content-Type = %q，期望 JSON 错误信封", ct)
	}
	if got := parseResult(t, rec).Error; got == nil || got.Code != string(httpx.CodePathOutsideRoot) {
		t.Fatalf("错误码 = %+v", got)
	}
}

// failingReader 在读到一半时失败，用来验"头已经发出去了"之后的诚实行为。
type failingReader struct {
	data []byte
	done bool
}

func (r *failingReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, errors.New("代理流中断")
	}
	r.done = true
	n := copy(p, r.data)
	return n, nil
}

// TestFileDownloadTruncatedIsHonest 中途失败时状态码已经是 200，能做的只有
// 不把错误 JSON 续进响应体——浏览器看到的是"文件没下完"。
func TestFileDownloadTruncatedIsHonest(t *testing.T) {
	stub := &stubFiles{download: &files.RemoteDownload{
		Header: files.DownloadHeader{Name: "big.bin", SizeBytes: 4096, Version: "v1-1-2-3-4"},
		Reader: &failingReader{data: []byte("partial")},
	}}
	srv, cookie := filesFixture(t, stub)

	rec := adminGet(t, srv, cookie, "/api/v1/files/download?root_id=etc&path=big.bin")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（头已发出）", rec.Code)
	}
	if got := rec.Body.String(); got != "partial" {
		t.Fatalf("响应体 = %q，期望只包含已写出的字节", got)
	}
}

// TestContentDisposition 纯函数用例：文件名会进响应头，这里逐类字符过一遍。
func TestContentDisposition(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"普通名", "sshd_config", `attachment; filename="sshd_config"; filename*=UTF-8''sshd_config`},
		{"中文", "配置.conf", `attachment; filename="__.conf"; filename*=UTF-8''%E9%85%8D%E7%BD%AE.conf`},
		{"引号与反斜杠被替换", `a"b\c`, `attachment; filename="a_b_c"; filename*=UTF-8''a%22b%5Cc`},
		{"空格百分号编码", "my file.txt", `attachment; filename="my file.txt"; filename*=UTF-8''my%20file.txt`},
		{"换行被丢掉（响应拆分）", "a\r\nb", `attachment; filename="ab"; filename*=UTF-8''a%0D%0Ab`},
		{"空名兜底", "", `attachment; filename="download"; filename*=UTF-8''`},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			if got := contentDisposition(item.input); got != item.want {
				t.Fatalf("contentDisposition(%q) = %q，期望 %q", item.input, got, item.want)
			}
		})
	}
}

// TestPercentEncodeRFC5987 attr-char 之外的字节一律编码。
func TestPercentEncodeRFC5987(t *testing.T) {
	cases := map[string]string{
		"abc-123.txt":   "abc-123.txt",
		"!$&+-.^_`|~":   "!$&+-.^_`|~",
		"a b":           "a%20b",
		"a;b":           "a%3Bb",
		"(x)":           "%28x%29",
		"配置":            "%E9%85%8D%E7%BD%AE",
		"semi;colon,ok": "semi%3Bcolon%2Cok",
	}
	for input, want := range cases {
		if got := percentEncodeRFC5987(input); got != want {
			t.Fatalf("percentEncodeRFC5987(%q) = %q，期望 %q", input, got, want)
		}
	}
}
