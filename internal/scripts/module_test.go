package scripts

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"runtime"
	"strings"
	"testing"
	"time"

	"upanel/internal/syscmd"
)

// fakeStore 是脚本仓储的内存替身。它不做校验——校验在 Input.Validate 与 Module 里；
// 这里只负责版本号推进和 id 分配，让被测逻辑能拿到"另一层说成功"的返回值。
type fakeStore struct {
	recs  map[string]Record
	order []string
	seq   int
	err   error
	// dir 用来派生 Ref，和生产实现一样由 id 拼出来。
	dir string
	// badHash 让仓储回报一个与内容不符的指纹，用来触发"已保存但落盘核对不一致"。
	badHash bool
}

// ref 返回脚本应当落到的路径。
func (s *fakeStore) ref(id string) string {
	path, err := Path(s.dir, id)
	if err != nil {
		return "pending"
	}
	return path
}

// hash 返回仓储要记录的指纹。
func (s *fakeStore) hash(content string) string {
	if s.badHash {
		return Hash("tampered")
	}
	return Hash(content)
}

func newFakeStore() *fakeStore { return &fakeStore{recs: map[string]Record{}} }

func (s *fakeStore) List(context.Context) ([]Record, error) {
	out := make([]Record, 0, len(s.order))
	for _, id := range s.order {
		out = append(out, s.recs[id])
	}
	return out, nil
}

func (s *fakeStore) Get(_ context.Context, id string) (Record, bool, error) {
	if s.err != nil {
		return Record{}, false, s.err
	}
	rec, ok := s.recs[id]
	return rec, ok, nil
}

func (s *fakeStore) Create(_ context.Context, in Input, _ time.Time) (Record, error) {
	if s.err != nil {
		return Record{}, s.err
	}
	s.seq++
	id := fmt.Sprintf("scr_%032x", s.seq)
	rec := Record{ID: id, Name: in.Name, Content: in.Content, ContentHash: s.hash(in.Content),
		Ref: s.ref(id), Interpreter: Interpreter, Version: 1}
	s.recs[id] = rec
	s.order = append(s.order, id)
	return rec, nil
}

func (s *fakeStore) Update(_ context.Context, id string, in Input, expected int64, _ time.Time) (Record, error) {
	if s.err != nil {
		return Record{}, s.err
	}
	rec, ok := s.recs[id]
	if !ok {
		return Record{}, errors.New("脚本不存在")
	}
	if rec.Version != expected {
		return Record{}, errors.New("版本冲突")
	}
	rec.Name = in.Name
	rec.Content = in.Content
	rec.ContentHash = s.hash(in.Content)
	rec.Version++
	s.recs[id] = rec
	return rec, nil
}

func (s *fakeStore) Delete(_ context.Context, id string, expected int64) error {
	if s.err != nil {
		return s.err
	}
	rec, ok := s.recs[id]
	if !ok {
		return nil
	}
	if rec.Version != expected {
		return errors.New("版本冲突")
	}
	delete(s.recs, id)
	return nil
}

// fakeRefs 报告脚本是否被计划引用。
type fakeRefs struct {
	name string
	used bool
	err  error
}

func (r fakeRefs) ScriptScheduleName(context.Context, string) (string, bool, error) {
	return r.name, r.used, r.err
}

// fakeRunner 记录一次调用，返回预置结果。
type fakeRunner struct {
	res  syscmd.Result
	err  error
	spec syscmd.Spec
	lims syscmd.Limits
	call int
}

func (r *fakeRunner) run(_ context.Context, spec syscmd.Spec, lims syscmd.Limits) (syscmd.Result, error) {
	r.call++
	r.spec = spec
	r.lims = lims
	return r.res, r.err
}

func shellUser() *user.User {
	return &user.User{Username: ShellUser, Uid: "993", Gid: "993", HomeDir: "/var/lib/upanel-shell"}
}

type harness struct {
	mod   *Module
	store *fakeStore
	run   *fakeRunner
	dir   string
	owns  []int
}

func newHarness(t *testing.T, refs ScheduleReference) *harness {
	t.Helper()
	h := &harness{store: newFakeStore(), run: &fakeRunner{}, dir: t.TempDir()}
	h.store.dir = h.dir
	mod, err := NewModule(Options{
		Store:  h.store,
		Refs:   refs,
		Dir:    h.dir,
		Runner: h.run.run,
		Lookup: func(name string) (*user.User, error) {
			if name != ShellUser {
				return nil, errors.New("no such user")
			}
			return shellUser(), nil
		},
		Chown: func(_ string, uid, gid int) error {
			h.owns = append(h.owns, uid, gid)
			return nil
		},
		Now: func() time.Time { return time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("NewModule: %v", err)
	}
	h.mod = mod
	return h
}

// seed 直接往仓储里放一条记录，并铺好磁盘文件。
func (h *harness) seed(t *testing.T, name, content string) Record {
	t.Helper()
	rec, err := h.store.Create(context.Background(), Input{Name: name, Content: content}, time.Now())
	if err != nil {
		t.Fatalf("seed create: %v", err)
	}
	path, err := Path(h.dir, rec.ID)
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), FileMode); err != nil {
		t.Fatalf("seed write: %v", err)
	}
	rec.Ref = path
	h.store.recs[rec.ID] = rec
	return rec
}

func TestApplyWritesFileAndRecordsIdentity(t *testing.T) {
	h := newHarness(t, fakeRefs{})
	rec, err := h.mod.Apply(context.Background(), ApplyRequest{
		Input: Input{Name: "  清理日志  ", Content: "#!/bin/sh\r\necho hi\r\n"},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if rec.Name != "清理日志" {
		t.Fatalf("name = %q, 期望去掉首尾空白", rec.Name)
	}
	raw, err := os.ReadFile(rec.Ref)
	if err != nil {
		t.Fatalf("读回脚本: %v", err)
	}
	// 落盘的必须是规范化之后的字节：否则同一份内容在库里和在盘上是两个哈希。
	if string(raw) != "#!/bin/sh\necho hi\n" {
		t.Fatalf("落盘内容 = %q", string(raw))
	}
	if got := Hash(string(raw)); got != rec.ContentHash {
		t.Fatalf("磁盘哈希 %s 与记录 %s 不一致", got, rec.ContentHash)
	}
	// 目录与文件都设成 root:upanel-shell（目录先，文件后）：目录的属组不对，
	// 文件权限写得再对也打不开（P12.4 的 24.04 现场）。
	if len(h.owns) != 4 || h.owns[0] != 0 || h.owns[1] != 993 || h.owns[2] != 0 || h.owns[3] != 993 {
		t.Fatalf("属主设置 = %v, 期望 [0 993 0 993]", h.owns)
	}
	// 脚本目录必须能被属组穿过：0750 且属组是 upanel-shell。
	// 只在 Linux 上断言权限位：宿主是 Windows 时 os.Chmod 只碰只读位，模式报的是 0777，
	// 在这里断言等于断言一个 Windows 上没有的语义。
	if runtime.GOOS == "linux" {
		dirInfo, err := os.Stat(h.dir)
		if err != nil {
			t.Fatalf("读脚本目录: %v", err)
		}
		if got := dirInfo.Mode().Perm(); got != DirMode {
			t.Fatalf("脚本目录权限 = %#o, 期望 %#o", got, DirMode)
		}
	}
	// 临时文件不许残留。
	entries, err := os.ReadDir(h.dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Fatalf("临时文件残留: %s", e.Name())
		}
	}
}

func TestApplyRejectsBadContentBeforeTouchingDisk(t *testing.T) {
	cases := []struct {
		name string
		in   Input
		want string
	}{
		{"空内容", Input{Name: "a", Content: ""}, "content: 不能为空"},
		{"空名字", Input{Name: "", Content: "echo\n"}, "name: 不能为空"},
		{"NUL 字节", Input{Name: "a", Content: "echo\x00\n"}, "NUL"},
		{"超长", Input{Name: "a", Content: strings.Repeat("x", MaxContentBytes+1)}, "最多"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, fakeRefs{})
			if _, err := h.mod.Apply(context.Background(), ApplyRequest{Input: c.in}); err == nil {
				t.Fatal("期望拒绝")
			} else if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("错误 = %v, 期望包含 %q", err, c.want)
			}
			entries, _ := os.ReadDir(h.dir)
			if len(entries) != 0 {
				t.Fatalf("被拒绝的保存留下了文件: %v", entries)
			}
		})
	}
}

func TestApplyUpdateKeepsVersionChain(t *testing.T) {
	h := newHarness(t, fakeRefs{})
	rec := h.seed(t, "旧", "#!/bin/sh\necho old\n")

	if _, err := h.mod.Apply(context.Background(), ApplyRequest{
		ScriptID: rec.ID, Version: 99, Input: Input{Name: "旧", Content: "#!/bin/sh\necho new\n"},
	}); err == nil {
		t.Fatal("版本不符时不容许保存")
	}

	got, err := h.mod.Apply(context.Background(), ApplyRequest{
		ScriptID: rec.ID, Version: 1, Input: Input{Name: "新", Content: "#!/bin/sh\necho new\n"},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got.Version != 2 {
		t.Fatalf("version = %d, 期望 2", got.Version)
	}
	raw, _ := os.ReadFile(got.Ref)
	if string(raw) != "#!/bin/sh\necho new\n" {
		t.Fatalf("磁盘内容 = %q", string(raw))
	}
}

func TestApplyFailsBeforeSavingWhenChownFails(t *testing.T) {
	h := newHarness(t, fakeRefs{})
	mod, err := NewModule(Options{
		Store: h.store, Dir: h.dir, Runner: h.run.run,
		Lookup: func(string) (*user.User, error) { return shellUser(), nil },
		Chown:  func(string, int, int) error { return errors.New("chown: operation not permitted") },
	})
	if err != nil {
		t.Fatalf("NewModule: %v", err)
	}
	if _, err := mod.Apply(context.Background(), ApplyRequest{Input: Input{Name: "a", Content: "echo\n"}}); err == nil {
		t.Fatal("期望报错")
	}
	// 落库之前就失败，所以库里不该有半条记录：这才是"保存失败"该有的样子。
	if list, _ := h.store.List(context.Background()); len(list) != 0 {
		t.Fatalf("落盘失败却留下了记录: %+v", list)
	}
}

func TestApplyReportsSaveWhenVerificationFails(t *testing.T) {
	h := newHarness(t, fakeRefs{})
	h.store.badHash = true
	rec, err := h.mod.Apply(context.Background(), ApplyRequest{Input: Input{Name: "a", Content: "echo\n"}})
	if err == nil {
		t.Fatal("期望报错")
	}
	// 库里已经有记录，错误必须把这个事实说出来，而不是让人以为什么都没发生。
	if !strings.Contains(err.Error(), "已保存") {
		t.Fatalf("错误 = %v, 期望说明记录已保存", err)
	}
	if _, found, _ := h.store.Get(context.Background(), rec.ID); !found {
		t.Fatal("记录应当已经落库")
	}
}

func TestAssessRunBlocksTamperedFile(t *testing.T) {
	h := newHarness(t, fakeRefs{})
	rec := h.seed(t, "a", "#!/bin/sh\necho hi\n")

	out, err := h.mod.AssessRun(context.Background(), RunRequest{ScriptID: rec.ID, Version: 1, MaxRuntimeSeconds: 60})
	if err != nil {
		t.Fatalf("AssessRun: %v", err)
	}
	if !out.Allowed {
		t.Fatalf("未被改动的脚本应当放行: %v", out.Blockers)
	}

	if err := os.WriteFile(rec.Ref, []byte("#!/bin/sh\nrm -rf /tmp/x\n"), 0o600); err != nil {
		t.Fatalf("改写: %v", err)
	}
	out, err = h.mod.AssessRun(context.Background(), RunRequest{ScriptID: rec.ID, Version: 1, MaxRuntimeSeconds: 60})
	if err != nil {
		t.Fatalf("AssessRun: %v", err)
	}
	if out.Allowed {
		t.Fatal("磁盘内容变了还放行")
	}
	if len(out.Blockers) == 0 || out.Blockers[0].Code != "SCRIPT_CONTENT_CHANGED" {
		t.Fatalf("阻塞项 = %v", out.Blockers)
	}
	// 预检拒绝就够了吗？不够——真正执行那一步也必须自己再核一次。
	if _, err := h.mod.Run(context.Background(), RunRequest{ScriptID: rec.ID, Version: 1, MaxRuntimeSeconds: 60}); err == nil {
		t.Fatal("Run 必须拒绝被改过的脚本")
	}
	if h.run.call != 0 {
		t.Fatalf("被拒绝的脚本仍然被执行了 %d 次", h.run.call)
	}
}

func TestRunRefusesInterpreterMismatch(t *testing.T) {
	h := newHarness(t, fakeRefs{})
	rec := h.seed(t, "a", "#!/bin/sh\ntrue\n")

	// 库里的解释器与批准的不一致时，Run 必须拒绝而不是照跑。
	// 情形不是假想的：旧迁移把这一列写成基名 `sh`，而契约对外说的是 `/bin/sh`。
	bad := h.store.recs[rec.ID]
	bad.Interpreter = "sh"
	h.store.recs[rec.ID] = bad

	if _, err := h.mod.Run(context.Background(), RunRequest{ScriptID: rec.ID, Version: 1, MaxRuntimeSeconds: 60}); err == nil {
		t.Fatal("解释器不一致时 Run 必须拒绝")
	}
	if h.run.call != 0 {
		t.Fatalf("被拒绝的脚本仍然被执行了 %d 次", h.run.call)
	}
	// 真正拿去 exec 的永远是常量，不是库里的值。
	if h.run.spec.Program != "" {
		t.Fatalf("被拒绝时不该构造执行规格: %q", h.run.spec.Program)
	}
}

func TestAssessRunBlocksMissingAccount(t *testing.T) {
	h := newHarness(t, fakeRefs{})
	rec := h.seed(t, "a", "#!/bin/sh\ntrue\n")
	mod, err := NewModule(Options{
		Store: h.store, Dir: h.dir, Runner: h.run.run,
		Lookup: func(string) (*user.User, error) { return nil, errors.New("no such user") },
		Chown:  func(string, int, int) error { return nil },
	})
	if err != nil {
		t.Fatalf("NewModule: %v", err)
	}
	out, err := mod.AssessRun(context.Background(), RunRequest{ScriptID: rec.ID, Version: 1, MaxRuntimeSeconds: 30})
	if err != nil {
		t.Fatalf("AssessRun: %v", err)
	}
	if out.Allowed {
		t.Fatal("没有 upanel-shell 时不该放行")
	}
	if out.Blockers[0].Code != "SCRIPT_IDENTITY_MISSING" {
		t.Fatalf("阻塞项 = %v", out.Blockers)
	}
	if _, err := mod.Run(context.Background(), RunRequest{ScriptID: rec.ID, Version: 1, MaxRuntimeSeconds: 30}); err == nil {
		t.Fatal("Run 必须有同样的结论")
	}
}

func TestRunDropsPrivilegeAndReportsExitCode(t *testing.T) {
	h := newHarness(t, fakeRefs{})
	rec := h.seed(t, "a", "#!/bin/sh\nexit 3\n")
	h.run.res = syscmd.Result{ExitCode: 3, Stdout: "out", Stderr: "err", Duration: 12 * time.Millisecond}
	h.run.err = &syscmd.ExitError{Program: Interpreter, ExitCode: 3}

	out, err := h.mod.Run(context.Background(), RunRequest{ScriptID: rec.ID, Version: 1, MaxRuntimeSeconds: 45})
	// 非零退出是脚本的结果，不是"执行失败"：这里必须是 nil，退出码原样带出去。
	if err != nil {
		t.Fatalf("非零退出不该报成执行失败: %v", err)
	}
	if out.ExitCode != 3 || out.Stderr != "err" {
		t.Fatalf("结果 = %+v", out)
	}
	if h.run.spec.Program != Interpreter {
		t.Fatalf("解释器 = %q, 期望 %q", h.run.spec.Program, Interpreter)
	}
	if len(h.run.spec.Args) != 1 || h.run.spec.Args[0] != rec.Ref {
		t.Fatalf("参数 = %v, 期望只有脚本路径", h.run.spec.Args)
	}
	if h.run.spec.Credential == nil {
		t.Fatal("没有降权身份")
	}
	if h.run.spec.Credential.UID != 993 || h.run.spec.Credential.GID != 993 {
		t.Fatalf("身份 = %+v", *h.run.spec.Credential)
	}
	if len(h.run.spec.Credential.Groups) != 1 || h.run.spec.Credential.Groups[0] != 993 {
		t.Fatalf("附加组 = %v", h.run.spec.Credential.Groups)
	}
	if h.run.spec.Dir != "/var/lib/upanel-shell" {
		t.Fatalf("工作目录 = %q", h.run.spec.Dir)
	}
	if h.run.lims.Timeout != 45*time.Second {
		t.Fatalf("超时 = %v, 期望 45s", h.run.lims.Timeout)
	}
}

func TestRunPropagatesTimeout(t *testing.T) {
	h := newHarness(t, fakeRefs{})
	rec := h.seed(t, "a", "#!/bin/sh\nsleep 999\n")
	h.run.res = syscmd.Result{ExitCode: -1, TimedOut: true, Signal: "SIGKILL"}
	h.run.err = syscmd.ErrTimeout

	if _, err := h.mod.Run(context.Background(), RunRequest{ScriptID: rec.ID, Version: 1, MaxRuntimeSeconds: 1}); err == nil {
		t.Fatal("超时必须报出来")
	}
}

func TestDeleteRefusesWhileReferenced(t *testing.T) {
	h := newHarness(t, fakeRefs{name: "每晚备份", used: true})
	rec := h.seed(t, "a", "#!/bin/sh\ntrue\n")

	out, err := h.mod.AssessDelete(context.Background(), rec.ID, 1)
	if err != nil {
		t.Fatalf("AssessDelete: %v", err)
	}
	if out.Allowed || out.Blockers[0].Code != "SCRIPT_IN_USE" {
		t.Fatalf("预检 = %+v", out)
	}
	if err := h.mod.Delete(context.Background(), rec.ID, 1); err == nil {
		t.Fatal("被引用时删除必须被拒绝")
	}
	if _, err := os.Stat(rec.Ref); err != nil {
		t.Fatalf("被拒绝的删除动了文件: %v", err)
	}
}

func TestDeleteWithoutReferenceCheckRefuses(t *testing.T) {
	h := newHarness(t, nil)
	rec := h.seed(t, "a", "#!/bin/sh\ntrue\n")
	if err := h.mod.Delete(context.Background(), rec.ID, 1); err == nil {
		t.Fatal("没有引用检查端时必须拒绝删除")
	}
}

func TestDeleteRemovesRecordAndFile(t *testing.T) {
	h := newHarness(t, fakeRefs{})
	rec := h.seed(t, "a", "#!/bin/sh\ntrue\n")

	if err := h.mod.Delete(context.Background(), rec.ID, 99); err == nil {
		t.Fatal("版本不符时不许删除")
	}
	if err := h.mod.Delete(context.Background(), rec.ID, 1); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, found, _ := h.store.Get(context.Background(), rec.ID); found {
		t.Fatal("记录还在")
	}
	if _, err := os.Stat(rec.Ref); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("文件还在: %v", err)
	}
}

func TestNewModuleRejectsRelativeDir(t *testing.T) {
	if _, err := NewModule(Options{Store: newFakeStore(), Dir: "var/lib/x"}); err == nil {
		t.Fatal("相对路径必须被拒绝")
	}
	if _, err := NewModule(Options{}); err == nil {
		t.Fatal("没有仓储必须被拒绝")
	}
}
