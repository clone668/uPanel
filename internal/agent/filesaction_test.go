package agent

// P06.2 代理侧用例：八个文件写动作的登记、参数、策略、预检与执行接线。
//
// 这一层不碰文件系统（那是 internal/files 的 Linux 用例与 VM 验收的事），它要证明的是
// **中间这一层没有把纪律弄丢**：
//
//  1. **八个动作一起登记**，一个不少：只登记其中几个会让 capabilities 报告一组
//     互相矛盾的入口（例如“能覆盖却不能再认证”）。
//  2. **参数名就是契约里的名字**：多一个字段（尤其是 `command`、`path` 这种别名）
//     在解码层就被拒，不会被静默忽略后当成默认值执行。
//  3. **策略对齐 CONTRACTS 第 5 节的 R/P 两列**：delete/chmod/chown 要再认证，
//     递归删除与覆盖上传还要确认短语，且短语首词是 DELETE / OVERWRITE。
//  4. **阻塞项不是错误**：目标不存在、根只读、目录非空、账号模块没落地……都是
//     Allowed=false + 一个固定的码，界面因此永远说得清按钮为什么是灰的。
//  5. **Plan → Execute 真的接上了**：预检允许之后执行一次，结果里是读回的状态；
//     执行被拒绝时那个判决（VERSION_CONFLICT 等）会原样出现在任务失败信息里，
//     而不是被压成一句“动作执行失败”。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"upanel/internal/agentstore"
	"upanel/internal/files"
)

// fakeWriter 是 FileWriterSource 的测试替身。
//
// 它按相对路径回应探针，并记录每个落地动作收到的参数：用例的重点一半在
// “返回了什么”，另一半在“**没有**调用什么”。
type fakeWriter struct {
	roots     files.RootsResult
	stats     map[string]files.PathStat
	trees     map[string]files.PathStat
	statErrs  map[string]error
	statErr   error
	treeErr   error
	accounts  bool
	upload    files.UploadInfo
	uploadErr error
	write     files.WriteResult
	writeErr  error

	calls []string

	mkdirParams  files.MkdirParams
	writeParams  files.WriteTextParams
	commitParams files.CommitUploadParams
	copyParams   files.CopyParams
	moveParams   files.CopyParams
	deleteParams files.DeleteParams
	chmodParams  files.ChmodParams
	chownParams  files.ChownParams
}

func uploadIDForTest() string { return "up_00112233445566778899aabbccddeeff" }

func (f *fakeWriter) Roots(context.Context) files.RootsResult { return f.roots }

func (f *fakeWriter) Stat(_ context.Context, rootID, rel string) (files.PathStat, error) {
	// 按路径注入的失败优先于“假成功”：真实的 files 层在父目录缺失时**报错**
	// （openat2 撞 ENOENT），而不是返回 Exists=false。专门留一个口子让用例能
	// 照真实行为走这条分支。
	if err, ok := f.statErrs[rel]; ok {
		return files.PathStat{}, err
	}
	if f.statErr != nil {
		return files.PathStat{}, f.statErr
	}
	if stat, ok := f.stats[rel]; ok {
		stat.RootID, stat.Path = rootID, rel
		return stat, nil
	}
	return files.PathStat{RootID: rootID, Path: rel}, nil
}

func (f *fakeWriter) StatTree(_ context.Context, rootID, rel string) (files.PathStat, error) {
	if f.treeErr != nil {
		return files.PathStat{}, f.treeErr
	}
	if stat, ok := f.trees[rel]; ok {
		stat.RootID, stat.Path = rootID, rel
		return stat, nil
	}
	if stat, ok := f.stats[rel]; ok {
		stat.RootID, stat.Path, stat.InTree = rootID, rel, true
		return stat, nil
	}
	return files.PathStat{RootID: rootID, Path: rel}, nil
}

func (f *fakeWriter) AccountsAvailable() bool { return f.accounts }

func (f *fakeWriter) Mkdir(_ context.Context, params files.MkdirParams) (files.WriteResult, error) {
	f.calls = append(f.calls, "mkdir")
	f.mkdirParams = params
	return f.write, f.writeErr
}

func (f *fakeWriter) WriteText(_ context.Context, params files.WriteTextParams) (files.WriteResult, error) {
	f.calls = append(f.calls, "write_text")
	f.writeParams = params
	return f.write, f.writeErr
}

func (f *fakeWriter) CommitUpload(_ context.Context, params files.CommitUploadParams) (files.WriteResult, error) {
	f.calls = append(f.calls, "commit_upload")
	f.commitParams = params
	return f.write, f.writeErr
}

func (f *fakeWriter) Copy(_ context.Context, params files.CopyParams) (files.WriteResult, error) {
	f.calls = append(f.calls, "copy")
	f.copyParams = params
	return f.write, f.writeErr
}

func (f *fakeWriter) Move(_ context.Context, params files.CopyParams) (files.WriteResult, error) {
	f.calls = append(f.calls, "move")
	f.moveParams = params
	return f.write, f.writeErr
}

func (f *fakeWriter) Remove(_ context.Context, params files.DeleteParams) (files.WriteResult, error) {
	f.calls = append(f.calls, "delete")
	f.deleteParams = params
	return f.write, f.writeErr
}

func (f *fakeWriter) Chmod(_ context.Context, params files.ChmodParams) (files.WriteResult, error) {
	f.calls = append(f.calls, "chmod")
	f.chmodParams = params
	return f.write, f.writeErr
}

func (f *fakeWriter) Chown(_ context.Context, params files.ChownParams) (files.WriteResult, error) {
	f.calls = append(f.calls, "chown")
	f.chownParams = params
	return f.write, f.writeErr
}

func (f *fakeWriter) CreateUpload(context.Context, files.CreateUploadParams) (files.UploadInfo, error) {
	f.calls = append(f.calls, "create_upload")
	return f.upload, f.uploadErr
}

func (f *fakeWriter) WriteUploadContent(_ context.Context, _ string, body io.Reader) (files.UploadInfo, error) {
	f.calls = append(f.calls, "write_upload_content")
	_, _ = io.Copy(io.Discard, body)
	return f.upload, f.uploadErr
}

func (f *fakeWriter) GetUpload(context.Context, string) (files.UploadInfo, error) {
	f.calls = append(f.calls, "get_upload")
	return f.upload, f.uploadErr
}

func (f *fakeWriter) DeleteUpload(context.Context, string) error {
	f.calls = append(f.calls, "delete_upload")
	return f.uploadErr
}

func (f *fakeWriter) SweepStaging(context.Context, time.Time) (int, error) { return 0, nil }

func (f *fakeWriter) ReservedBytes(string) int64 { return 0 }

func (f *fakeWriter) MaxUploadBytes() int64 { return files.DefaultMaxUploadBytes }

// landed 返回真正会改系统的调用。预检可以读（get_upload 是探针），
// 但不能落地——用例里绝大部分断言都是这一句。
func (f *fakeWriter) landed() []string {
	var out []string
	for _, call := range f.calls {
		switch call {
		case "mkdir", "write_text", "commit_upload", "copy", "move", "delete", "chmod", "chown":
			out = append(out, call)
		}
	}
	return out
}

// testFileRoots 是三个根：两个可写（跨根移动要用）、一个只读。
func testFileRoots() files.RootsResult {
	return files.RootsResult{Roots: []files.RootInfo{
		{RootID: "etc", Name: "配置", Path: "/etc", Writable: true, Available: true},
		{RootID: "data", Name: "数据", Path: "/srv/data", Writable: true, Available: true},
		{RootID: "sys", Name: "系统", Path: "/boot", Writable: false, Available: true},
	}}
}

// newFileActionServer 登记八个文件动作并起一台带真实库的代理。
func newFileActionServer(t *testing.T, src *fakeWriter) *Server {
	t.Helper()
	reg := NewRegistry()
	if err := RegisterFileActions(reg, src); err != nil {
		t.Fatalf("登记文件动作失败: %v", err)
	}
	srv, _, _ := newOperationServer(t, reg)
	return srv
}

// fileActionCases 是八个动作的合法参数，登记与解码用例共用同一张表。
func fileActionCases() []struct{ action, params string } {
	return []struct{ action, params string }{
		{ActionFileMkdir, `{"root_id":"etc","relative_path":"conf"}`},
		{ActionFileWriteText, `{"root_id":"etc","relative_path":"app.conf","content":"x=1"}`},
		{ActionFileCommitUpload, `{"upload_id":"` + uploadIDForTest() + `","overwrite":false}`},
		{ActionFileCopy, `{"source_root_id":"etc","source_path":"a.conf","target_root_id":"etc","target_path":"b.conf"}`},
		{ActionFileMove, `{"source_root_id":"etc","source_path":"a.conf","target_root_id":"data","target_path":"b.conf"}`},
		{ActionFileDelete, `{"root_id":"etc","relative_path":"old","recursive":false}`},
		{ActionFileChmod, `{"root_id":"etc","relative_path":"a.conf","mode":"0640"}`},
		{ActionFileChown, `{"root_id":"etc","relative_path":"a.conf","managed_account_id":3}`},
	}
}

// TestFileActionsRegisteredTogether 八个动作一个不少地登记了：
// 少一个就是 capabilities 里一组互相矛盾的入口。
func TestFileActionsRegisteredTogether(t *testing.T) {
	src := &fakeWriter{
		roots: testFileRoots(),
		stats: map[string]files.PathStat{"a.conf": {Exists: true, Type: files.TypeFile, Mode: "0644", Version: "v1"}},
		trees: map[string]files.PathStat{},
		upload: files.UploadInfo{UploadID: uploadIDForTest(), RootID: "etc", Path: "app.conf",
			SizeBytes: 5, State: files.UploadStateReceived},
	}
	srv := newFileActionServer(t, src)

	for _, item := range fileActionCases() {
		rec := postPreflight(t, srv, testWebUID, preflightBody(item.action, item.params))
		if rec.Code != 201 {
			t.Fatalf("%s 预检状态码 = %d，期望 201；响应 %s", item.action, rec.Code, rec.Body.String())
		}
	}
	if got := src.landed(); len(got) != 0 {
		t.Fatalf("预检不该执行任何写动作，却调用了 %v", got)
	}
}

// TestFileActionsAbsentWithoutWriter 没接写端时八个动作都不存在：
// “未登记”是 400，而不是“预检通过、执行时才发现没有实现”。
func TestFileActionsAbsentWithoutWriter(t *testing.T) {
	srv, _, _ := newOperationServer(t, NewAcceptanceRegistry())
	for _, item := range fileActionCases() {
		rec := postPreflight(t, srv, testWebUID, preflightBody(item.action, item.params))
		if rec.Code != 400 {
			t.Fatalf("%s 状态码 = %d，期望 400；响应 %s", item.action, rec.Code, rec.Body.String())
		}
		if code := errorCode(t, rec); code != "VALIDATION_ERROR" {
			t.Fatalf("%s 错误码 = %q", item.action, code)
		}
	}
}

// TestFileActionsRejectUnknownFields 参数名就是契约里的名字：
// 多一个字段就是一次误操作，必须在解码层被拒。
func TestFileActionsRejectUnknownFields(t *testing.T) {
	src := &fakeWriter{roots: testFileRoots(), stats: map[string]files.PathStat{}, trees: map[string]files.PathStat{}}
	srv := newFileActionServer(t, src)

	for _, item := range fileActionCases() {
		// 在最后一个 } 之前塞一个契约里没有的字段。`command` 与 `path` 是最危险的两种
		// 误用：一个像“万能命令”，一个是 `relative_path` 的别名。
		for _, extra := range []string{`"command":"rm -rf /"`, `"path":"/etc/shadow"`} {
			body := strings.TrimSuffix(item.params, "}") + `,` + extra + `}`
			rec := postPreflight(t, srv, testWebUID, preflightBody(item.action, body))
			if rec.Code != 400 {
				t.Fatalf("%s + %s 状态码 = %d，期望 400；响应 %s", item.action, extra, rec.Code, rec.Body.String())
			}
			if code := errorCode(t, rec); code != "VALIDATION_ERROR" {
				t.Fatalf("%s + %s 错误码 = %q", item.action, extra, code)
			}
		}
	}
	if got := src.landed(); len(got) != 0 {
		t.Fatalf("被拒的参数不该执行任何动作，却调用了 %v", got)
	}
}

// TestFileWriteTextParamsLimit write_text 的参数上限是“文本上限 + 余量”，不是通用的 64 KiB：
// 一个 1 MiB 以内的文件必须能保存，超过文本上限的必须拒绝。
func TestFileWriteTextParamsLimit(t *testing.T) {
	src := &fakeWriter{roots: testFileRoots(), stats: map[string]files.PathStat{}, trees: map[string]files.PathStat{}}
	srv := newFileActionServer(t, src)

	// 恰好 1 MiB 的内容：预检允许。这一条同时守着“信封上限”那件事——
	// 信封按 64 KiB 解码时，这段内容连预检都到不了（曾经就是这样）。
	body := `{"root_id":"etc","relative_path":"app.conf","content":"` + strings.Repeat("a", files.MaxTextBytes) + `"}`
	if rec := postPreflight(t, srv, testWebUID, preflightBody(ActionFileWriteText, body)); rec.Code != 201 {
		t.Fatalf("1 MiB 内容的状态码 = %d，期望 201；响应 %.200s", rec.Code, rec.Body.String())
	}

	// 比文本上限多 1 字节：形状没错，是内容超限——400（不是 413，也没被收下）。
	overText := `{"root_id":"etc","relative_path":"app.conf","content":"` + strings.Repeat("a", files.MaxTextBytes+1) + `"}`
	rec := postPreflight(t, srv, testWebUID, preflightBody(ActionFileWriteText, overText))
	if rec.Code != 400 {
		t.Fatalf("超文本上限的状态码 = %d，期望 400；响应 %.200s", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec); code != "VALIDATION_ERROR" {
		t.Fatalf("超文本上限错误码 = %q", code)
	}

	// 超过逐动作参数上限（1 MiB + 4 KiB）：解码前就是 413。
	overParams := `{"root_id":"etc","relative_path":"app.conf","content":"` + strings.Repeat("a", files.MaxTextBytes+8<<10) + `"}`
	rec = postPreflight(t, srv, testWebUID, preflightBody(ActionFileWriteText, overParams))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超参数上限的状态码 = %d，期望 413；响应 %.200s", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec); code != "VALIDATION_ERROR" {
		t.Fatalf("超参数上限错误码 = %q", code)
	}
}

// TestFileActionPolicies 策略对齐 CONTRACTS 第 5 节：
// delete/chmod/chown 要再认证；递归删除与覆盖上传还要确认短语，且短语首词固定。
func TestFileActionPolicies(t *testing.T) {
	src := &fakeWriter{
		roots: testFileRoots(),
		stats: map[string]files.PathStat{"a.conf": {Exists: true, Type: files.TypeFile, Mode: "0644", Version: "v1"}},
		trees: map[string]files.PathStat{},
		upload: files.UploadInfo{UploadID: uploadIDForTest(), RootID: "etc", Path: "a.conf",
			SizeBytes: 5, State: files.UploadStateReceived},
	}
	srv := newFileActionServer(t, src)

	cases := []struct {
		name     string
		action   string
		params   string
		reauth   bool
		phrase   bool
		verbVerb string
	}{
		{"建目录不设门槛", ActionFileMkdir, `{"root_id":"etc","relative_path":"conf"}`, false, false, ""},
		{"写文本不设门槛", ActionFileWriteText, `{"root_id":"etc","relative_path":"a.conf","expected_version":"v1","content":"x"}`, false, false, ""},
		{"复制不设门槛", ActionFileCopy, `{"source_root_id":"etc","source_path":"a.conf","target_root_id":"etc","target_path":"b.conf"}`, false, false, ""},
		{"移动不设门槛", ActionFileMove, `{"source_root_id":"etc","source_path":"a.conf","target_root_id":"data","target_path":"b.conf"}`, false, false, ""},
		{"删除文件只需再认证", ActionFileDelete, `{"root_id":"etc","relative_path":"a.conf","recursive":false}`, true, false, ""},
		{"递归删除要短语", ActionFileDelete, `{"root_id":"etc","relative_path":"a.conf","recursive":true}`, true, true, PhraseVerbDelete},
		{"改权限只需再认证", ActionFileChmod, `{"root_id":"etc","relative_path":"a.conf","mode":"0600"}`, true, false, ""},
		{"改所有者只需再认证", ActionFileChown, `{"root_id":"etc","relative_path":"a.conf","managed_account_id":3}`, true, false, ""},
		{"新建上传落位不设门槛", ActionFileCommitUpload, `{"upload_id":"` + uploadIDForTest() + `","overwrite":false}`, false, false, ""},
		{"覆盖上传要再认证与短语", ActionFileCommitUpload, `{"upload_id":"` + uploadIDForTest() + `","overwrite":true,"expected_version":"v1"}`, true, true, PhraseVerbOverwrite},
	}
	for _, item := range cases {
		plan := mustPreflight(t, srv, item.action, item.params)
		if plan.RequiresReauth != item.reauth || plan.RequiresPhrase != item.phrase {
			t.Fatalf("%s：reauth=%v phrase=%v，期望 reauth=%v phrase=%v",
				item.name, plan.RequiresReauth, plan.RequiresPhrase, item.reauth, item.phrase)
		}
		if item.phrase {
			if !strings.HasPrefix(plan.ConfirmationPhrase, item.verbVerb+" ") {
				t.Fatalf("%s 的确认短语 = %q，期望以 %q 开头", item.name, plan.ConfirmationPhrase, item.verbVerb)
			}
		} else if plan.ConfirmationPhrase != "" {
			t.Fatalf("%s 不要求短语却下发了 %q", item.name, plan.ConfirmationPhrase)
		}
		if plan.Cancellable {
			t.Fatalf("%s 不该标记为可取消", item.name)
		}
	}
}

// TestFileActionPreflightBlockers 预检的阻塞项：每条都对应一种“现在不能做”的真实原因。
//
// 这些是 Allowed=false + 一个固定的码，**不是** 500：界面据此说清楚按钮为什么是灰的。
func TestFileActionPreflightBlockers(t *testing.T) {
	cases := []struct {
		name    string
		action  string
		params  string
		setup   func(*fakeWriter)
		allowed bool
		code    string
		impact  string
		warning string
	}{
		{
			name: "建目录", action: ActionFileMkdir,
			params: `{"root_id":"etc","relative_path":"conf"}`, allowed: true, impact: "create",
		},
		{
			name: "建同名目录", action: ActionFileMkdir,
			params: `{"root_id":"etc","relative_path":"conf"}`,
			setup: func(f *fakeWriter) {
				f.stats["conf"] = files.PathStat{Exists: true, Type: files.TypeDir, Mode: "0755"}
			},
			code: noticeTargetExists,
		},
		{
			name: "根只读", action: ActionFileMkdir,
			params: `{"root_id":"sys","relative_path":"conf"}`, code: noticeRootReadOnly,
		},
		{
			name: "根未登记", action: ActionFileMkdir,
			params: `{"root_id":"nope","relative_path":"conf"}`, code: noticeRootUnknown,
		},
		{
			// 父目录不存在时预检必须说 PARENT_MISSING：给一份 allowed=true 的计划，
			// 任务会在落地那一刻撞 ENOENT，那时界面上已经没人看得懂发生了什么。
			name: "父目录不存在", action: ActionFileMkdir,
			params: `{"root_id":"etc","relative_path":"ghost/child"}`, code: noticeParentMissing,
		},
		{
			// 上一条走的是“父目录读得到、目标 Exists=false”的形状；真实的 files 层在
			// 父目录缺失时是**报错**（打开父目录撞 ENOENT → ErrNotFound），根本不返回
			// 一份 Exists=false 的路径状态。这条照真实行为走错误分支。
			name: "父目录不存在（files 层报 ErrNotFound）", action: ActionFileMkdir,
			params: `{"root_id":"etc","relative_path":"ghost/child"}`,
			setup: func(f *fakeWriter) {
				f.statErrs["ghost/child"] = fmt.Errorf("%w: ghost", files.ErrNotFound)
				f.statErrs["ghost"] = fmt.Errorf("%w: ghost", files.ErrNotFound)
			},
			code: noticeParentMissing,
		},
		{
			// 目标读不到、父目录读得到：这才是真正的“目标不存在”。预检不能因为
			// 遇到了错误分支就一律往 PARENT_MISSING 上推。
			name: "目标读不到而父目录正常", action: ActionFileDelete,
			params: `{"root_id":"etc","relative_path":"app.conf"}`,
			setup: func(f *fakeWriter) {
				f.statErrs["app.conf"] = fmt.Errorf("%w: app.conf", files.ErrNotFound)
				f.stats[""] = files.PathStat{Exists: true, Type: files.TypeDir, Mode: "0755"}
			},
			code: noticeTargetMissing,
		},
		{
			// 两级父目录都缺：parentNotices 自己也会撞 ErrNotFound，仍然要说 PARENT_MISSING。
			name: "两级父目录都不存在", action: ActionFileMkdir,
			params: `{"root_id":"etc","relative_path":"deep/ghost/child"}`,
			setup: func(f *fakeWriter) {
				f.statErrs["deep/ghost/child"] = fmt.Errorf("%w: deep", files.ErrNotFound)
				f.statErrs["deep/ghost"] = fmt.Errorf("%w: deep", files.ErrNotFound)
			},
			code: noticeParentMissing,
		},
		{
			name: "父目录是文件", action: ActionFileMkdir,
			params: `{"root_id":"etc","relative_path":"app.conf/child"}`,
			setup: func(f *fakeWriter) {
				f.stats["app.conf"] = files.PathStat{Exists: true, Type: files.TypeFile, Version: "v1"}
			},
			code: noticeParentMissing,
		},
		{
			name: "写文本到不存在的目录里", action: ActionFileWriteText,
			params: `{"root_id":"etc","relative_path":"ghost/new.conf","content":"x=1"}`,
			code:   noticeParentMissing,
		},
		{
			name: "复制到不存在的目录里", action: ActionFileCopy,
			params: `{"source_root_id":"etc","source_path":"a.conf","target_root_id":"etc","target_path":"ghost/b.conf"}`,
			setup: func(f *fakeWriter) {
				f.stats["a.conf"] = files.PathStat{Exists: true, Type: files.TypeFile, Version: "v1"}
			},
			code: noticeParentMissing,
		},
		{
			name: "写新文本", action: ActionFileWriteText,
			params: `{"root_id":"etc","relative_path":"new.conf","content":"x=1"}`, allowed: true, impact: "write",
		},
		{
			name: "覆盖但没给版本", action: ActionFileWriteText,
			params: `{"root_id":"etc","relative_path":"app.conf","content":"x=1"}`,
			setup: func(f *fakeWriter) {
				f.stats["app.conf"] = files.PathStat{Exists: true, Type: files.TypeFile, SizeBytes: 8, Version: "v1"}
			},
			code: noticeVersionConflict,
		},
		{
			name: "版本对不上", action: ActionFileWriteText,
			params: `{"root_id":"etc","relative_path":"app.conf","expected_version":"v0","content":"x=1"}`,
			setup: func(f *fakeWriter) {
				f.stats["app.conf"] = files.PathStat{Exists: true, Type: files.TypeFile, SizeBytes: 8, Version: "v1"}
			},
			code: noticeVersionConflict,
		},
		{
			name: "版本对上", action: ActionFileWriteText,
			params: `{"root_id":"etc","relative_path":"app.conf","expected_version":"v1","content":"x=1"}`,
			setup: func(f *fakeWriter) {
				f.stats["app.conf"] = files.PathStat{Exists: true, Type: files.TypeFile, SizeBytes: 8, Version: "v1"}
			},
			allowed: true, impact: "write",
		},
		{
			name: "写到目录上", action: ActionFileWriteText,
			params: `{"root_id":"etc","relative_path":"conf","expected_version":"v1","content":"x"}`,
			setup: func(f *fakeWriter) {
				f.stats["conf"] = files.PathStat{Exists: true, Type: files.TypeDir, Version: "v1"}
			},
			code: noticeTargetNotRegular,
		},
		{
			name: "删不存在的", action: ActionFileDelete,
			params: `{"root_id":"etc","relative_path":"ghost"}`, code: noticeTargetMissing,
		},
		{
			name: "非空目录不递归", action: ActionFileDelete,
			params: `{"root_id":"etc","relative_path":"old"}`,
			setup: func(f *fakeWriter) {
				f.stats["old"] = files.PathStat{Exists: true, Type: files.TypeDir}
				f.trees["old"] = files.PathStat{Exists: true, Type: files.TypeDir, InTree: true, TreeEntries: 3}
			},
			code: noticeDirNotEmpty,
		},
		{
			name: "递归删除有告警", action: ActionFileDelete,
			params: `{"root_id":"etc","relative_path":"old","recursive":true}`,
			setup: func(f *fakeWriter) {
				f.stats["old"] = files.PathStat{Exists: true, Type: files.TypeDir}
				f.trees["old"] = files.PathStat{Exists: true, Type: files.TypeDir, InTree: true, TreeEntries: 3, TreeBytes: 120}
			},
			allowed: true, impact: "delete", warning: "RECURSIVE_DELETE",
		},
		{
			name: "树太大不递归", action: ActionFileDelete,
			params: `{"root_id":"etc","relative_path":"old"}`,
			setup: func(f *fakeWriter) {
				f.stats["old"] = files.PathStat{Exists: true, Type: files.TypeDir}
				f.trees["old"] = files.PathStat{Exists: true, Type: files.TypeDir, TreeTruncated: true}
			},
			code: noticeTreeTooLarge,
		},
		{
			name: "复制目标已存在", action: ActionFileCopy,
			params: `{"source_root_id":"etc","source_path":"a.conf","target_root_id":"etc","target_path":"b.conf"}`,
			setup: func(f *fakeWriter) {
				f.stats["a.conf"] = files.PathStat{Exists: true, Type: files.TypeFile, Version: "v1"}
				f.stats["b.conf"] = files.PathStat{Exists: true, Type: files.TypeFile, Version: "v2"}
			},
			code: noticeTargetExists,
		},
		{
			name: "复制到空位", action: ActionFileCopy,
			params: `{"source_root_id":"etc","source_path":"a.conf","target_root_id":"etc","target_path":"b.conf"}`,
			setup: func(f *fakeWriter) {
				f.stats["a.conf"] = files.PathStat{Exists: true, Type: files.TypeFile, Version: "v1"}
			},
			allowed: true,
		},
		{
			name: "跨根移动不原子", action: ActionFileMove,
			params: `{"source_root_id":"etc","source_path":"a.conf","target_root_id":"data","target_path":"b.conf"}`,
			setup: func(f *fakeWriter) {
				f.stats["a.conf"] = files.PathStat{Exists: true, Type: files.TypeFile, Version: "v1"}
			},
			allowed: true, impact: "not_atomic",
		},
		{
			name: "同根移动是原子改名", action: ActionFileMove,
			params: `{"source_root_id":"etc","source_path":"a.conf","target_root_id":"etc","target_path":"b.conf"}`,
			setup: func(f *fakeWriter) {
				f.stats["a.conf"] = files.PathStat{Exists: true, Type: files.TypeFile, Version: "v1"}
			},
			allowed: true, impact: "rename",
		},
		{
			name: "上传还没收完", action: ActionFileCommitUpload,
			params: `{"upload_id":"` + uploadIDForTest() + `","overwrite":false}`,
			setup: func(f *fakeWriter) {
				f.upload.State = files.UploadStateOpen
			},
			code: noticeUploadNotReady,
		},
		{
			name: "上传不存在", action: ActionFileCommitUpload,
			params: `{"upload_id":"` + uploadIDForTest() + `","overwrite":false}`,
			setup: func(f *fakeWriter) {
				f.uploadErr = files.ErrUploadNotFound
			},
			code: noticeUploadNotFound,
		},
		{
			name: "落位但目标已存在", action: ActionFileCommitUpload,
			params: `{"upload_id":"` + uploadIDForTest() + `","overwrite":false}`,
			setup: func(f *fakeWriter) {
				f.stats["app.conf"] = files.PathStat{Exists: true, Type: files.TypeFile, Version: "v1"}
			},
			code: noticeTargetExists,
		},
		{
			name: "覆盖但版本不对", action: ActionFileCommitUpload,
			params: `{"upload_id":"` + uploadIDForTest() + `","overwrite":true,"expected_version":"v0"}`,
			setup: func(f *fakeWriter) {
				f.stats["app.conf"] = files.PathStat{Exists: true, Type: files.TypeFile, Version: "v1"}
			},
			code: noticeVersionConflict,
		},
		{
			name: "落位到空位", action: ActionFileCommitUpload,
			params:  `{"upload_id":"` + uploadIDForTest() + `","overwrite":false}`,
			allowed: true, impact: "create",
		},
		{
			name: "改权限", action: ActionFileChmod,
			params: `{"root_id":"etc","relative_path":"a.conf","mode":"0640"}`,
			setup: func(f *fakeWriter) {
				f.stats["a.conf"] = files.PathStat{Exists: true, Type: files.TypeFile, Mode: "0644", Version: "v1"}
			},
			allowed: true, impact: "mode",
		},
		{
			name: "权限没变化有告警", action: ActionFileChmod,
			params: `{"root_id":"etc","relative_path":"a.conf","mode":"0644"}`,
			setup: func(f *fakeWriter) {
				f.stats["a.conf"] = files.PathStat{Exists: true, Type: files.TypeFile, Mode: "0644", Version: "v1"}
			},
			allowed: true, impact: "mode", warning: "NO_CHANGE",
		},
		{
			name: "改所有者但账号模块没落地", action: ActionFileChown,
			params: `{"root_id":"etc","relative_path":"a.conf","managed_account_id":3}`,
			setup: func(f *fakeWriter) {
				f.stats["a.conf"] = files.PathStat{Exists: true, Type: files.TypeFile, Version: "v1"}
			},
			code: noticeAccountsMissing,
		},
		{
			name: "改所有者", action: ActionFileChown,
			params: `{"root_id":"etc","relative_path":"a.conf","managed_account_id":3,"managed_group_id":4}`,
			setup: func(f *fakeWriter) {
				f.accounts = true
				f.stats["a.conf"] = files.PathStat{Exists: true, Type: files.TypeFile, Version: "v1"}
			},
			allowed: true, impact: "owner",
		},
	}

	for _, item := range cases {
		src := &fakeWriter{
			roots:    testFileRoots(),
			stats:    map[string]files.PathStat{},
			trees:    map[string]files.PathStat{},
			statErrs: map[string]error{},
			upload: files.UploadInfo{UploadID: uploadIDForTest(), RootID: "etc", Path: "app.conf",
				SizeBytes: 5, State: files.UploadStateReceived},
		}
		if item.setup != nil {
			item.setup(src)
		}
		srv := newFileActionServer(t, src)
		plan := mustPreflight(t, srv, item.action, item.params)

		if plan.Allowed != item.allowed {
			t.Fatalf("%s：allowed = %v，期望 %v；blockers=%+v", item.name, plan.Allowed, item.allowed, plan.Blockers)
		}
		if item.code != "" {
			if len(plan.Blockers) == 0 || plan.Blockers[0].Code != item.code {
				t.Fatalf("%s：blockers = %+v，期望首个是 %s", item.name, plan.Blockers, item.code)
			}
		}
		if item.allowed && len(plan.Blockers) != 0 {
			t.Fatalf("%s：允许的计划不该带阻塞项 %+v", item.name, plan.Blockers)
		}
		if item.impact != "" {
			found := false
			for _, impact := range plan.Impacts {
				if impact.Kind == item.impact {
					found = true
				}
			}
			if !found {
				t.Fatalf("%s：impacts = %+v，期望含 %s", item.name, plan.Impacts, item.impact)
			}
		}
		if item.warning != "" {
			found := false
			for _, warning := range plan.Warnings {
				if warning.Code == item.warning {
					found = true
				}
			}
			if !found {
				t.Fatalf("%s：warnings = %+v，期望含 %s", item.name, plan.Warnings, item.warning)
			}
		}
		if got := src.landed(); len(got) != 0 {
			t.Fatalf("%s：预检不该执行写动作，却调用了 %v", item.name, got)
		}
	}
}

// TestFileMkdirRunsAndVerifies 预检允许之后确实执行了一次，结果里是读回的状态。
//
// 这条用例证明 Plan 与 Execute 是同一条线：预检给出的目标、锁与执行时用的参数
// 是同一份东西，而不是“预检看了一遍、执行时又自己拼了一套”。
func TestFileMkdirRunsAndVerifies(t *testing.T) {
	src := &fakeWriter{
		roots: testFileRoots(),
		stats: map[string]files.PathStat{},
		trees: map[string]files.PathStat{},
		write: files.WriteResult{RootID: "etc", Path: "conf", Version: "v9", Type: files.TypeDir, Mode: "0755"},
	}
	srv := newFileActionServer(t, src)

	plan := mustPreflight(t, srv, ActionFileMkdir, `{"root_id":"etc","relative_path":"conf"}`)
	if !plan.Allowed {
		t.Fatalf("预检不允许: %+v", plan.Blockers)
	}
	if len(plan.Locks) != 1 || plan.Locks[0] != "file:etc:/conf" {
		t.Fatalf("锁 = %v，期望 file:etc:/conf", plan.Locks)
	}

	id := opID("file-mkdir")
	if rec := submitOperation(t, srv, operationSubmitBody(id, plan.PreflightRef, "", testSessionHash, "")); rec.Code != 201 {
		t.Fatalf("提交状态码 = %d；响应 %s", rec.Code, rec.Body.String())
	}
	done := waitForStage(t, srv, id, agentstore.StageSucceeded)

	var result struct {
		Outcome       string         `json:"outcome"`
		ObservedState map[string]any `json:"observed_state"`
		Data          map[string]any `json:"data"`
	}
	if err := json.Unmarshal(done.Result, &result); err != nil {
		t.Fatalf("结果不是合法 JSON: %v（%s）", err, done.Result)
	}
	if result.Outcome != OutcomeVerified {
		t.Fatalf("outcome = %q，期望 verified", result.Outcome)
	}
	if result.ObservedState["exists"] != true || result.ObservedState["type"] != files.TypeDir {
		t.Fatalf("observed_state = %+v，应当说明目录已经存在", result.ObservedState)
	}
	if result.Data["path"] != "conf" {
		t.Fatalf("data = %+v", result.Data)
	}

	if len(src.calls) != 1 || src.calls[0] != "mkdir" {
		t.Fatalf("执行调用 = %v，期望恰好一次 mkdir", src.calls)
	}
	if src.mkdirParams.RootID != "etc" || src.mkdirParams.Path != "conf" {
		t.Fatalf("执行参数 = %+v", src.mkdirParams)
	}
}

// TestFileWriteTextRejectionKeepsVerdict 执行阶段被拒绝时，判决原样进任务失败信息：
// 覆盖冲突必须显示 VERSION_CONFLICT，而不是一句“动作执行失败”。
func TestFileWriteTextRejectionKeepsVerdict(t *testing.T) {
	src := &fakeWriter{
		roots:    testFileRoots(),
		stats:    map[string]files.PathStat{},
		trees:    map[string]files.PathStat{},
		writeErr: files.ErrVersionConflict,
	}
	srv := newFileActionServer(t, src)

	plan := mustPreflight(t, srv, ActionFileWriteText,
		`{"root_id":"etc","relative_path":"app.conf","content":"x=1"}`)
	id := opID("file-write-conflict")
	if rec := submitOperation(t, srv, operationSubmitBody(id, plan.PreflightRef, "", testSessionHash, "")); rec.Code != 201 {
		t.Fatalf("提交状态码 = %d；响应 %s", rec.Code, rec.Body.String())
	}
	failed := waitForStage(t, srv, id, agentstore.StageFailed)
	var errBody struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(failed.Error, &errBody); err != nil {
		t.Fatalf("错误体不是合法 JSON: %v（%s）", err, failed.Error)
	}
	if errBody.Code != "VERSION_CONFLICT" {
		t.Fatalf("错误码 = %q，期望 VERSION_CONFLICT", errBody.Code)
	}
	if errBody.Message == "" {
		t.Fatal("失败信息不能是空的")
	}
}

// TestFileTargetsDescribeWhatChanged 预检目标的两个字段各自回答一个问题：
// 身份指纹说“是哪个对象”，状态版本说“它此刻是什么”。
//
// 这里直接测两个构造函数，因为 execute 前的复核就是拿它们重算再逐字比对：
// 任何一边漏掉一个变化，都会让一份过期的预检看起来还有效。
func TestFileTargetsDescribeWhatChanged(t *testing.T) {
	absent := fileTarget("etc", "app.conf", files.PathStat{})
	present := fileTarget("etc", "app.conf", files.PathStat{Exists: true, Version: "v1"})
	other := fileTarget("etc", "other.conf", files.PathStat{Exists: true, Version: "v1"})

	if absent.IdentityFingerprint != present.IdentityFingerprint {
		t.Fatalf("同一个路径的指纹不该变: %q vs %q", absent.IdentityFingerprint, present.IdentityFingerprint)
	}
	if present.IdentityFingerprint == other.IdentityFingerprint {
		t.Fatal("不同路径的指纹必须不同")
	}
	if absent.StateVersion != "absent" || present.StateVersion != "v1" {
		t.Fatalf("状态版本 = %q / %q", absent.StateVersion, present.StateVersion)
	}

	open := uploadTarget(files.UploadInfo{UploadID: uploadIDForTest(), RootID: "etc", Path: "app.conf",
		SizeBytes: 5, State: files.UploadStateOpen})
	done := uploadTarget(files.UploadInfo{UploadID: uploadIDForTest(), RootID: "etc", Path: "app.conf",
		SizeBytes: 5, ReceivedBytes: 5, State: files.UploadStateReceived})
	if open.IdentityFingerprint != done.IdentityFingerprint {
		t.Fatal("同一份暂存的指纹不该随状态变化")
	}
	if open.StateVersion == done.StateVersion {
		t.Fatal("暂存状态变了，状态版本必须跟着变，否则过期预检会看起来还有效")
	}
}
