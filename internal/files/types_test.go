package files

// P06.1 参数与判决的纯逻辑用例（在任何平台都能跑）。
//
// 这里盯的是**形状校验**这一层：绝对路径、`..`、空段、NUL、超长、非法 UTF-8
// 必须在进入内核之前就被拒。它不是边界保证（边界是 openat2 的 RESOLVE_*，
// 由 manager_linux_test.go 在真实文件系统上验），但少了它，错误会变成
// 内核返回的一个 ELOOP/ENOENT，调用方与运维都要多绕一圈才能看懂。

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"upanel/internal/httpx"
)

func TestValidateRelPathAccepts(t *testing.T) {
	for _, path := range []string{
		"", // 根自身：列举允许
		"nginx",
		"nginx/sites-available",
		"a b/c d.txt",   // 空格不是特殊字符
		"配置/nginx.conf", // 非 ASCII 名字
		"..hidden",      // 以两个点开头但不是 ".." 段
		"a.b/c..d",
		strings.Repeat("a", 255), // 单个名字段到 NAME_MAX
	} {
		if err := ValidateRelPath(path); err != nil {
			t.Fatalf("ValidateRelPath(%q) = %v，期望通过", path, err)
		}
	}
}

func TestValidateRelPathRejects(t *testing.T) {
	cases := []struct {
		name string
		path string
		want error
	}{
		{"绝对路径", "/etc/passwd", ErrPathRejected},
		{"父目录", "..", ErrPathRejected},
		{"嵌套父目录", "../../etc/shadow", ErrPathRejected},
		{"中段父目录", "nginx/../../etc/shadow", ErrPathRejected},
		{"当前目录", ".", ErrInvalidParams},
		{"重复斜杠", "a//b", ErrInvalidParams},
		{"结尾斜杠", "a/", ErrInvalidParams},
		{"NUL", "a\x00b", ErrInvalidParams},
		{"单个名字段过长", strings.Repeat("a", 256), ErrInvalidParams},
		{"总长超过 PATH_MAX", strings.Repeat("a", 300) + "/" + strings.Repeat("b", 300) + "/" +
			strings.Repeat("c", 2500) + "/" + strings.Repeat("d", 1000), ErrInvalidParams},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			err := ValidateRelPath(item.path)
			if err == nil {
				t.Fatalf("ValidateRelPath(%q) 通过了，期望被拒", item.path)
			}
			if !errors.Is(err, item.want) {
				t.Fatalf("ValidateRelPath(%q) = %v，期望 %v", item.path, err, item.want)
			}
		})
	}
}

// TestValidateRelPathRejectsInvalidUTF8 JSON 会把非法 UTF-8 悄悄变成 U+FFFD，
// 那样我们就会去打开一个与请求里写的不同的名字。必须显式拒绝。
func TestValidateRelPathRejectsInvalidUTF8(t *testing.T) {
	path := string([]byte{'a', 0xff, 'b'})
	if err := ValidateRelPath(path); !errors.Is(err, ErrInvalidParams) {
		t.Fatalf("非法 UTF-8 未被拒绝: %v", err)
	}
}

func TestValidateRootID(t *testing.T) {
	for _, id := range []string{"etc", "www-html", "a", strings.Repeat("a", 32)} {
		if err := validateRootID(id); err != nil {
			t.Fatalf("root_id %q 应通过: %v", id, err)
		}
	}
	for _, id := range []string{"", "../etc", "etc/data", "Etc", "etc data", strings.Repeat("a", 33), "etc\n"} {
		if err := validateRootID(id); !errors.Is(err, ErrInvalidParams) {
			t.Fatalf("root_id %q 应被拒: %v", id, err)
		}
	}
}

func TestValidateExpectedVersion(t *testing.T) {
	for _, v := range []string{"", "v1-a1b2-c3d4-5-6", "0-0-0-0-0"} {
		if err := validateExpectedVersion(v); err != nil {
			t.Fatalf("version %q 应通过: %v", v, err)
		}
	}
	for _, v := range []string{"V1-ABC", "v1_a", strings.Repeat("a", 129), "v1 1"} {
		if err := validateExpectedVersion(v); !errors.Is(err, ErrInvalidParams) {
			t.Fatalf("version %q 应被拒: %v", v, err)
		}
	}
}

// TestParamsValidate 三种参数各自的必填约束。
func TestParamsValidate(t *testing.T) {
	ok := []struct {
		name string
		err  error
	}{
		{"list 根", ListParams{RootID: "etc"}.Validate()},
		{"list 带游标", ListParams{RootID: "etc", Path: "nginx", Cursor: "nginx.conf"}.Validate()},
		{"text", TextParams{RootID: "etc", Path: "hostname"}.Validate()},
		{"text 带版本", TextParams{RootID: "etc", Path: "hostname", ExpectedVersion: "v1-1-2-3-4"}.Validate()},
		{"download", DownloadParams{RootID: "etc", Path: "hostname"}.Validate()},
	}
	for _, item := range ok {
		if item.err != nil {
			t.Fatalf("%s 应通过: %v", item.name, item.err)
		}
	}

	bad := []struct {
		name string
		err  error
		want error
	}{
		{"list 缺 root_id", ListParams{}.Validate(), ErrInvalidParams},
		{"list 绝对路径", ListParams{RootID: "etc", Path: "/etc"}.Validate(), ErrPathRejected},
		{"list 父目录", ListParams{RootID: "etc", Path: "../.."}.Validate(), ErrPathRejected},
		{"list 游标过长", ListParams{RootID: "etc", Cursor: strings.Repeat("c", 4097)}.Validate(), ErrInvalidParams},
		{"list 游标含斜杠", ListParams{RootID: "etc", Cursor: "a/b"}.Validate(), ErrInvalidParams},
		{"text 空路径", TextParams{RootID: "etc"}.Validate(), ErrInvalidParams},
		{"text 只有空格", TextParams{RootID: "etc", Path: "   "}.Validate(), ErrInvalidParams},
		{"text 绝对路径", TextParams{RootID: "etc", Path: "/etc/shadow"}.Validate(), ErrPathRejected},
		{"download 父目录", DownloadParams{RootID: "etc", Path: "../../etc/shadow"}.Validate(), ErrPathRejected},
		{"download 版本非法", DownloadParams{RootID: "etc", Path: "a", ExpectedVersion: "V1"}.Validate(), ErrInvalidParams},
	}
	for _, item := range bad {
		if item.err == nil {
			t.Fatalf("%s 应被拒", item.name)
		}
		if !errors.Is(item.err, item.want) {
			t.Fatalf("%s = %v，期望 %v", item.name, item.err, item.want)
		}
	}
}

// TestHTTPStatusOfMapsEverySentinel 每个分类错误都有确定的状态码与 code。
//
// 这张表就是“拒绝长什么样”的唯一约定：代理查询、代理流、Web 公开接口都取同一处。
func TestHTTPStatusOfMapsEverySentinel(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   httpx.Code
	}{
		{ErrInvalidParams, http.StatusBadRequest, httpx.CodeValidationError},
		{ErrNotDirectory, http.StatusBadRequest, httpx.CodeValidationError},
		{ErrNotRegular, http.StatusBadRequest, httpx.CodeValidationError},
		{ErrTooLarge, http.StatusRequestEntityTooLarge, httpx.CodeValidationError},
		{ErrPathRejected, http.StatusUnprocessableEntity, httpx.CodePathOutsideRoot},
		{ErrHardLinked, http.StatusUnprocessableEntity, httpx.CodePathOutsideRoot},
		{ErrUnknownRoot, http.StatusNotFound, httpx.CodeNotFound},
		{ErrNotFound, http.StatusNotFound, httpx.CodeNotFound},
		{ErrRootUnavailable, http.StatusConflict, httpx.CodeRootUnavailable},
		{ErrNotText, http.StatusUnprocessableEntity, httpx.CodeFileNotText},
		{ErrVersionConflict, http.StatusConflict, httpx.CodeVersionConflict},
		{ErrUnsupported, http.StatusNotImplemented, httpx.CodeNotImplemented},
	}
	for _, item := range cases {
		t.Run(string(item.code), func(t *testing.T) {
			// 用 %w 包一层：真实调用链里错误总是带着上下文过来的。
			wrapped := fmt.Errorf("列目录: %w", item.err)
			got, ok := HTTPStatusOf(wrapped)
			if !ok {
				t.Fatalf("%v 没有被分类", wrapped)
			}
			if got.Status != item.status || got.Code != item.code {
				t.Fatalf("判决 = %d/%s，期望 %d/%s", got.Status, got.Code, item.status, item.code)
			}
			if got.Message == "" {
				t.Fatalf("判决缺少给人看的说明")
			}
			// statusCarrier 是代理层的接口；这里顺带证明它被实现了。
			status, code := got.HTTPStatus()
			if status != item.status || code != item.code {
				t.Fatalf("HTTPStatus() = %d/%s", status, code)
			}
		})
	}
}

// TestHTTPStatusOfLeavesUnknownAlone 未分类的错误不给兜底判决：
// “没分类的失败”必须看起来像失败，而不是像一条参数错误。
func TestHTTPStatusOfLeavesUnknownAlone(t *testing.T) {
	if got, ok := HTTPStatusOf(errors.New("openat2: 磁盘满了")); ok || got != nil {
		t.Fatalf("未分类错误被映射成 %+v", got)
	}
	if got, ok := HTTPStatusOf(nil); ok || got != nil {
		t.Fatalf("nil 被映射成 %+v", got)
	}
}

// TestAsHTTPErrorIsAlways500ForUnknown 代理用它包装查询结果：
// 未分类失败必须是 500，而不是 200 也不是 400。
func TestAsHTTPErrorIsAlways500ForUnknown(t *testing.T) {
	err := AsHTTPError(errors.New("openat2: 磁盘满了"))
	if err.Status != http.StatusInternalServerError || err.Code != httpx.CodeInternalError {
		t.Fatalf("判决 = %d/%s", err.Status, err.Code)
	}
	if !strings.Contains(err.Error(), "磁盘满了") {
		t.Fatalf("判决丢了原始细节: %q", err.Error())
	}

	mapped := AsHTTPError(fmt.Errorf("%w: ..", ErrPathRejected))
	if mapped.Status != http.StatusUnprocessableEntity || mapped.Code != httpx.CodePathOutsideRoot {
		t.Fatalf("已分类错误被重判: %d/%s", mapped.Status, mapped.Code)
	}

	// 已经带判决的错误原样返回，不重复包装（否则消息里会出现两遍前缀）。
	original := &HTTPError{Status: http.StatusConflict, Code: httpx.CodeVersionConflict, Message: "文件已改变"}
	if got := AsHTTPError(original); got != original {
		t.Fatalf("已有判决被重新构造: %+v", got)
	}
	if got := AsHTTPError(nil); got != nil {
		t.Fatalf("nil → %+v", got)
	}
}

// TestJoinPath 只做拼接，不做归一化。
func TestJoinPath(t *testing.T) {
	if got := JoinPath("", "a"); got != "a" {
		t.Fatalf("JoinPath(\"\", a) = %q", got)
	}
	if got := JoinPath("a", "b"); got != "a/b" {
		t.Fatalf("JoinPath(a, b) = %q", got)
	}
	if got := JoinPath("a", ".."); got != "a/.." {
		t.Fatalf("JoinPath(a, ..) = %q，期望原样拼接", got)
	}
}

// TestLimitsAreContractual 限额是契约值，不是可调参数。
func TestLimitsAreContractual(t *testing.T) {
	if MaxTextBytes != 1<<20 {
		t.Fatalf("MaxTextBytes = %d，契约是 1 MiB", MaxTextBytes)
	}
	if ListPageSize != 200 {
		t.Fatalf("ListPageSize = %d，契约是一页 200 项", ListPageSize)
	}
	if MaxPathBytes != 4096 {
		t.Fatalf("MaxPathBytes = %d，契约是 PATH_MAX=4096", MaxPathBytes)
	}
}
