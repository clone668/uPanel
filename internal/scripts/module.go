package scripts

// 受管理脚本的落地与执行（P12.4；CONTRACTS.md 第 5、12 节）。
//
// 三件事，每一件都有一个"为什么不能更简单"：
//
//	落盘      文件是 root 所有、属组 upanel-shell、0750。内容永远由库里的正文派生，
//	          写完立刻回读核对哈希——"写完了"和"写对了"是两件事。
//	运行前    再核一次磁盘上的字节。对不上就**不跑**：那个文件可能被任何一次手工编辑
//	          改过，而它对应的计划是管理员批准过的。跳过"哈希不对就重铺一份"的自动修复——
//	          自动覆盖会把"有人在改这个文件"这件事从现场抹掉。
//	运行      以 upanel-shell 身份、固定解释器 /bin/sh、工作目录是它的 home、
//	          时长上限来自计划定义。永远不获得 root（CONTRACTS §5）。
//
// 降权用内核的 setuid 语义（syscmd 的 Credential），不经 sudo / setpriv：
// 那把"以谁的身份运行"交给了另一个程序的参数解析。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"upanel/internal/syscmd"
)

// Runner 执行一条外部命令（生产实现是 syscmd.Run）。
type Runner func(ctx context.Context, spec syscmd.Spec, limits syscmd.Limits) (syscmd.Result, error)

// Record 是一条受管理脚本。
//
// 形状定义在这里、代理库返回别名：正文与它的指纹在同一次事务里写进来，
// 落盘文件的字节永远由这里的正文派生。
type Record struct {
	ID          string
	Name        string
	Content     string
	ContentHash string
	Ref         string
	Interpreter string
	Version     int64
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Store 是本模块需要的脚本仓储能力（由 agentstore.ScriptStore 实现）。
type Store interface {
	List(ctx context.Context) ([]Record, error)
	Get(ctx context.Context, id string) (Record, bool, error)
	Create(ctx context.Context, in Input, now time.Time) (Record, error)
	Update(ctx context.Context, id string, in Input, expectedVersion int64, now time.Time) (Record, error)
	Delete(ctx context.Context, id string, expectedVersion int64) error
}

// ScheduleReference 回答"这个脚本还被计划引用吗"。
//
// 它是接口而不是直接依赖计划仓储：脚本模块不认识计划，计划模块才认识脚本。
// 删除脚本时"存在计划引用则拒绝"（CONTRACTS §5 script.delete）靠它落地。
type ScheduleReference interface {
	ScriptScheduleName(ctx context.Context, scriptID string) (string, bool, error)
}

// Module 是脚本动作的实现端。
type Module struct {
	store     Store
	refs      ScheduleReference
	dir       string
	run       Runner
	now       func() time.Time
	lookup    func(string) (*user.User, error)
	chownFile func(string, int, int) error
	logger    *slog.Logger
}

// Options 是构造参数。
type Options struct {
	// Store 必需。
	Store Store
	// Refs 用于删除前的计划引用检查；为 nil 时删除会拒绝（宁可拒绝，也不留下悬空计划）。
	Refs ScheduleReference
	// Dir 是脚本目录，默认 DefaultDir。
	Dir string
	// Runner 默认 syscmd.Run。
	Runner Runner
	// Lookup 解析系统账号，默认 user.Lookup。
	Lookup func(string) (*user.User, error)
	// Chown 设置文件属主/属组，默认 os.Chown。
	Chown func(path string, uid, gid int) error
	// Now 默认 time.Now。
	Now func() time.Time
	// Logger 可选：目录穿越权限被修补时用来留痕（为 nil 时不留）。
	Logger *slog.Logger
}

// NewModule 构造实现端。
func NewModule(opts Options) (*Module, error) {
	if opts.Store == nil {
		return nil, errors.New("scripts: 脚本模块需要仓储")
	}
	dir := strings.TrimSpace(opts.Dir)
	if dir == "" {
		dir = DefaultDir
	}
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("scripts: 脚本目录必须是绝对路径，当前为 %q", dir)
	}
	run := opts.Runner
	if run == nil {
		run = syscmd.Run
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	lookup := opts.Lookup
	if lookup == nil {
		lookup = user.Lookup
	}
	chown := opts.Chown
	if chown == nil {
		chown = os.Chown
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Module{store: opts.Store, refs: opts.Refs, dir: dir, run: run, now: now,
		lookup: lookup, chownFile: chown, logger: logger}, nil
}

// Dir 返回脚本目录。
func (m *Module) Dir() string { return m.dir }

// Assessment 是 apply/delete 的预检结论。
type Assessment struct {
	Allowed             bool
	Blockers            []Notice
	Warnings            []Notice
	Impacts             []Impact
	DisplayName         string
	IdentityFingerprint string
	StateVersion        string
}

// Notice 是一条阻塞项或告警项。
type Notice struct {
	Code    string
	Message string
}

// Impact 是一条影响说明。
type Impact struct {
	Kind        string
	Description string
}

// ApplyRequest 是 script.apply 的参数。
type ApplyRequest struct {
	// ScriptID 为空表示新建。
	ScriptID string
	Version  int64
	Input    Input
}

// List 返回全部脚本。
func (m *Module) List(ctx context.Context) ([]Record, error) { return m.store.List(ctx) }

// Get 返回一条脚本。
func (m *Module) Get(ctx context.Context, id string) (Record, bool, error) {
	return m.store.Get(ctx, id)
}

// AssessApply 生成 script.apply 的预检结论。
func (m *Module) AssessApply(ctx context.Context, req ApplyRequest) (Assessment, error) {
	in := req.Input.Normalize()
	if err := in.Validate(); err != nil {
		return Assessment{Allowed: false, Blockers: []Notice{{Code: "SCRIPT_INVALID", Message: err.Error()}},
			DisplayName: in.Name}, nil
	}
	out := Assessment{Allowed: true, DisplayName: in.Name}
	if req.ScriptID == "" {
		out.IdentityFingerprint = "new-script"
		out.StateVersion = "none"
		out.Impacts = []Impact{{Kind: "execute_as", Description: "脚本将以 " + ShellUser + " 身份运行，永远不获得 root"}}
		return out, nil
	}
	rec, found, err := m.store.Get(ctx, req.ScriptID)
	if err != nil {
		return Assessment{}, err
	}
	if !found {
		out.Allowed = false
		out.Blockers = []Notice{{Code: "SCRIPT_NOT_FOUND", Message: "脚本不存在"}}
		out.IdentityFingerprint = req.ScriptID
		out.StateVersion = "none"
		return out, nil
	}
	out.DisplayName = rec.Name
	out.IdentityFingerprint = rec.ID
	out.StateVersion = strconv.FormatInt(rec.Version, 10)
	if req.Version != rec.Version {
		out.Allowed = false
		out.Blockers = append(out.Blockers, Notice{
			Code:    "VERSION_CONFLICT",
			Message: fmt.Sprintf("脚本已被改动过（当前版本 %d，预检基于 %d），请重新读取后再保存", rec.Version, req.Version),
		})
	}
	out.Impacts = []Impact{{Kind: "execute_as", Description: "脚本将以 " + ShellUser + " 身份运行，永远不获得 root"}}
	return out, nil
}

// Apply 执行 script.apply：先落库，再把字节铺到固定目录，最后回读核对。
func (m *Module) Apply(ctx context.Context, req ApplyRequest) (Record, error) {
	in := req.Input.Normalize()
	if err := in.Validate(); err != nil {
		return Record{}, err
	}
	now := m.now().UTC()

	// 先写临时文件：它同时验证了"目录可写、空间够"，而这两件事比任何一句
	// "已保存"都更能说明这次保存是否真的落地。
	tmp, err := m.writeTemp(in.Content)
	if err != nil {
		return Record{}, err
	}
	defer func() { _ = os.Remove(tmp) }()

	var rec Record
	if req.ScriptID == "" {
		rec, err = m.store.Create(ctx, in, now)
	} else {
		rec, err = m.store.Update(ctx, req.ScriptID, in, req.Version, now)
	}
	if err != nil {
		return Record{}, err
	}

	final, err := Path(m.dir, rec.ID)
	if err != nil {
		return rec, err
	}
	if err := m.renameInto(tmp, final); err != nil {
		return rec, fmt.Errorf("脚本记录已保存（版本 %d），但落盘失败：%w；重新保存会重试", rec.Version, err)
	}
	if err := m.verifyFile(final, rec.ContentHash); err != nil {
		return rec, fmt.Errorf("脚本记录已保存（版本 %d），但落盘后核对不一致：%w；重新保存会重试", rec.Version, err)
	}
	return rec, nil
}

// AssessDelete 生成 script.delete 的预检结论。
func (m *Module) AssessDelete(ctx context.Context, id string, version int64) (Assessment, error) {
	rec, found, err := m.store.Get(ctx, id)
	if err != nil {
		return Assessment{}, err
	}
	if !found {
		return Assessment{Allowed: false, Blockers: []Notice{{Code: "SCRIPT_NOT_FOUND", Message: "脚本不存在"}},
			IdentityFingerprint: id, StateVersion: "none"}, nil
	}
	out := Assessment{
		Allowed:             true,
		DisplayName:         rec.Name,
		IdentityFingerprint: rec.ID,
		StateVersion:        strconv.FormatInt(rec.Version, 10),
		Impacts:             []Impact{{Kind: "delete_file", Description: "删除受管理脚本文件 " + rec.Ref}},
	}
	if version != rec.Version {
		out.Allowed = false
		out.Blockers = append(out.Blockers, Notice{
			Code:    "VERSION_CONFLICT",
			Message: fmt.Sprintf("脚本已被改动过（当前版本 %d，预检基于 %d）", rec.Version, version),
		})
	}
	if name, referenced, err := m.referencedBy(ctx, id); err != nil {
		return Assessment{}, err
	} else if referenced {
		out.Allowed = false
		out.Blockers = append(out.Blockers, Notice{
			Code:    "SCRIPT_IN_USE",
			Message: fmt.Sprintf("计划任务「%s」还在引用这条脚本；先删除或改掉那条计划", name),
		})
	}
	return out, nil
}

// Delete 执行 script.delete：先删记录，再删文件。
//
// 顺序是刻意的：万一文件删不掉，库里已经没有这条脚本了，于是"它还会不会被运行"
// 的答案是明确的"不会"——留一个删不掉的记录才是危险的那种残留。
func (m *Module) Delete(ctx context.Context, id string, version int64) error {
	rec, found, err := m.store.Get(ctx, id)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	if name, referenced, err := m.referencedBy(ctx, id); err != nil {
		return err
	} else if referenced {
		return fmt.Errorf("计划任务「%s」还在引用这条脚本", name)
	}
	if err := m.store.Delete(ctx, id, version); err != nil {
		return err
	}
	if err := os.Remove(rec.Ref); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("脚本记录已删除，但文件 %s 没删掉：%w", rec.Ref, err)
	}
	return nil
}

// RunRequest 是 script.run 的参数。
type RunRequest struct {
	ScriptID          string
	Version           int64
	MaxRuntimeSeconds int
}

// RunOutcome 是一次脚本运行的结果。
type RunOutcome struct {
	ScriptName string
	ExitCode   int
	Duration   time.Duration
	TimedOut   bool
	Signal     string
	Stdout     string
	Stderr     string
	Truncated  bool
}

// AssessRun 生成 script.run 的预检结论。
//
// 它在预检时就核对磁盘上的字节：不够的脚本不该等到点开任务才失败。
func (m *Module) AssessRun(ctx context.Context, req RunRequest) (Assessment, error) {
	out := Assessment{IdentityFingerprint: req.ScriptID, StateVersion: strconv.FormatInt(req.Version, 10)}
	if req.MaxRuntimeSeconds <= 0 || req.MaxRuntimeSeconds > MaxRuntimeSecondsLimit {
		out.Allowed = false
		out.Blockers = append(out.Blockers, Notice{Code: "SCRIPT_RUNTIME_INVALID",
			Message: "max_runtime_seconds 必须在 1—86400 之间"})
	}
	rec, found, err := m.store.Get(ctx, req.ScriptID)
	if err != nil {
		return Assessment{}, err
	}
	if !found {
		out.Allowed = false
		out.Blockers = append(out.Blockers, Notice{Code: "SCRIPT_NOT_FOUND", Message: "脚本不存在"})
		return out, nil
	}
	out.DisplayName = rec.Name
	out.StateVersion = strconv.FormatInt(rec.Version, 10)
	if req.Version != rec.Version {
		out.Allowed = false
		out.Blockers = append(out.Blockers, Notice{
			Code:    "VERSION_CONFLICT",
			Message: fmt.Sprintf("脚本已被改动过（当前版本 %d，预检基于 %d）", rec.Version, req.Version),
		})
	}
	if err := m.verifyFile(rec.Ref, rec.ContentHash); err != nil {
		out.Allowed = false
		out.Blockers = append(out.Blockers, Notice{
			Code:    "SCRIPT_CONTENT_CHANGED",
			Message: fmt.Sprintf("磁盘上的脚本与批准的版本不一致（%v）；重新保存脚本可恢复批准内容", err),
		})
	}
	ident, err := m.shellIdentity()
	if err != nil {
		out.Allowed = false
		out.Blockers = append(out.Blockers, Notice{Code: "SCRIPT_IDENTITY_MISSING", Message: err.Error()})
	}
	out.Impacts = []Impact{{Kind: "execute_as", Description: "以 " + ident.describe() + " 身份运行"}}
	if out.Blockers == nil {
		out.Allowed = true
	}
	return out, nil
}

// Run 执行 script.run。
func (m *Module) Run(ctx context.Context, req RunRequest) (RunOutcome, error) {
	rec, found, err := m.store.Get(ctx, req.ScriptID)
	if err != nil {
		return RunOutcome{}, err
	}
	if !found {
		return RunOutcome{}, errors.New("脚本不存在")
	}
	if req.Version != rec.Version {
		return RunOutcome{}, fmt.Errorf("脚本已被改动过（当前版本 %d，提交基于 %d），拒绝运行", rec.Version, req.Version)
	}
	// 运行前最后核一次字节：预检之后到真正执行之间，那个文件仍可能被改过。
	if err := m.verifyFile(rec.Ref, rec.ContentHash); err != nil {
		return RunOutcome{}, fmt.Errorf("磁盘上的脚本与批准的版本不一致，拒绝运行：%w", err)
	}
	// 解释器不信库里的值，但也不许它跟这里的不一致。
	//
	// 真正拿去 exec 的永远是常量 Interpreter（一个能被库值左右的 Program 就是
	// 一个能换二进制的入口）。这一行只保证"对外说的"与"实际跑的"是同一个：
	// 万一库里的值被谁改过、或迁移没跑到，这里当场拒绝，而不是让它继续对外
	// 声称一个自己没在用的解释器。
	if rec.Interpreter != Interpreter {
		return RunOutcome{}, fmt.Errorf("脚本记录里的解释器是 %q，与批准的 %q 不一致，拒绝运行", rec.Interpreter, Interpreter)
	}
	ident, err := m.shellIdentity()
	if err != nil {
		return RunOutcome{}, err
	}
	// 跑之前再确认一次目录能穿过去：保存之后目录权限仍可能被谁改回去
	// （比如有人按旧文档重新 `install -d -m 0700 /var/lib/upanel-agent`）。
	// 这一步只补 x 位与属组，不碰脚本内容；修过哪几级会写进日志。
	if err := m.prepareDir(ident); err != nil {
		return RunOutcome{}, fmt.Errorf("脚本目录不可用，拒绝运行：%w", err)
	}
	timeout := time.Duration(req.MaxRuntimeSeconds) * time.Second
	if timeout <= 0 {
		timeout = time.Minute
	}

	res, runErr := m.run(ctx, syscmd.Spec{
		Program:    Interpreter,
		Args:       []string{rec.Ref},
		Dir:        ident.home,
		Credential: &syscmd.Credential{UID: ident.uid, GID: ident.gid, Groups: []uint32{ident.gid}},
	}, syscmd.Limits{
		Timeout:   timeout,
		MaxStdout: 256 << 10,
		MaxStderr: 64 << 10,
	})

	out := RunOutcome{
		ScriptName: rec.Name,
		ExitCode:   res.ExitCode,
		Duration:   res.Duration,
		TimedOut:   res.TimedOut,
		Signal:     res.Signal,
		Stdout:     res.Stdout,
		Stderr:     res.Stderr,
		Truncated:  res.Truncated(),
	}
	if runErr != nil {
		var exitErr *syscmd.ExitError
		if errors.As(runErr, &exitErr) {
			// 非零退出是**结果**，不是执行失败：脚本自己说的话要原样带出去。
			return out, nil
		}
		return out, runErr
	}
	return out, nil
}

// shellIdentity 解析 upanel-shell 的 uid/gid 与 home。
func (m *Module) shellIdentity() (identity, error) {
	u, err := m.lookup(ShellUser)
	if err != nil {
		return identity{}, fmt.Errorf("账号 %s 不存在：受管理脚本没有可用的运行身份", ShellUser)
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil || uid == 0 {
		return identity{}, fmt.Errorf("账号 %s 的 UID %q 不能用于降权运行", ShellUser, u.Uid)
	}
	gid, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil {
		return identity{}, fmt.Errorf("账号 %s 的主组 %q 不是数字", ShellUser, u.Gid)
	}
	home := strings.TrimSpace(u.HomeDir)
	// 用 "/" 判绝对路径，不用 filepath.IsAbs：这是**目标机器**上的账号事实，
	// 不该因为开发/测试所在的宿主是 Windows 而改变结论。
	if !strings.HasPrefix(home, "/") {
		return identity{}, fmt.Errorf("账号 %s 的 home %q 不是绝对路径", ShellUser, u.HomeDir)
	}
	return identity{uid: uint32(uid), gid: uint32(gid), home: home}, nil
}

type identity struct {
	uid  uint32
	gid  uint32
	home string
}

func (i identity) describe() string {
	if i.uid == 0 && i.home == "" {
		return ShellUser
	}
	return fmt.Sprintf("%s（uid %d）", ShellUser, i.uid)
}

// referencedBy 查脚本是否被计划引用。
func (m *Module) referencedBy(ctx context.Context, id string) (string, bool, error) {
	if m.refs == nil {
		// 没有引用检查端时宁可拒绝删除：删掉一条被计划引用的脚本，那条计划就会在
		// 下一次触发时失败，而失败原因离真正的操作差了一整条链路。
		return "", true, nil
	}
	return m.refs.ScriptScheduleName(ctx, id)
}

// prepareDir 让脚本目录真的能被运行账号用起来，并把修过哪几级说出来。
//
// 这一步不是"加固"，是**修产品自己造的坑**（与 Q233 同一条纪律）：目录是面板建的，
// 而它的上级 /var/lib/upanel-agent 是 0700 root:root（代理数据目录，本来只给 root）。
// 于是 upanel-shell 连穿都穿不过去，脚本文件权限写得再对也打不开——
// P12.4 的 24.04 真机现场就是这样：`/bin/sh: 0: cannot open …/scr_….sh: Permission denied`，
// 而预检说"允许"。
//
// 修法与 Q233 一样分两种权限：
//
//	脚本目录自身  root:upanel-shell  0750（属组要能穿过并读到 <id>.sh）
//	每一级上级    只补 other 的 x（0700 → 0711）：能穿过去、列不出来，
//	              代理数据目录里的文件名与数据库内容仍然只属于 root。
//
// 为什么不干脆让脚本目录搬出 /var/lib/upanel-agent：PLAN 第 4 节把那里定为
// "代理数据，root 独占"，把脚本放进同级目录会多出一个"谁的目录"的问题；
// 而补一级 x 位把边界留在原地——只是让面板自己的账号进得去自己造的文件。
func (m *Module) prepareDir(ident identity) error {
	if err := os.MkdirAll(m.dir, DirMode); err != nil {
		return fmt.Errorf("创建脚本目录 %s 失败: %w", m.dir, err)
	}
	if err := m.chownFile(m.dir, 0, int(ident.gid)); err != nil {
		return fmt.Errorf("设置脚本目录 %s 的属组失败: %w", m.dir, err)
	}
	if err := os.Chmod(m.dir, DirMode); err != nil {
		return fmt.Errorf("设置脚本目录 %s 的权限失败: %w", m.dir, err)
	}
	repaired, err := m.ensureAncestorsTraversable()
	if err != nil {
		return err
	}
	if len(repaired) > 0 {
		m.logger.Info("已给脚本目录的上级补上穿越权限",
			"dir", m.dir, "repaired", repaired,
			"meaning", "只加 other 的 x 位：能穿过去、列不出来")
	}
	return nil
}

// ensureAncestorsTraversable 让脚本目录的每一级上级都对其他用户有 x 位。
//
// 只加 x，其余位原样保留；已经是目录、且不是符号链接才动它。修过哪几级按从下到上返回。
func (m *Module) ensureAncestorsTraversable() ([]string, error) {
	var repaired []string
	for cur := filepath.Dir(m.dir); ; cur = filepath.Dir(cur) {
		info, err := os.Lstat(cur)
		if err != nil {
			return repaired, fmt.Errorf("读取脚本目录的上级 %s 失败: %w", cur, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return repaired, fmt.Errorf("脚本目录的上级 %s 不是真实目录", cur)
		}
		perm := info.Mode().Perm()
		if perm&0o001 == 0 {
			// group 的 x 一起补，是为了让"修好的目录"与"新建的目录"长得一样（0711）。
			if err := os.Chmod(cur, perm|0o011); err != nil {
				return repaired, fmt.Errorf("给 %s 补上穿越权限失败: %w", cur, err)
			}
			repaired = append(repaired, cur)
		}
		if cur == "/" || filepath.Dir(cur) == cur {
			break
		}
	}
	return repaired, nil
}

// writeTemp 在脚本目录里写一份临时文件（同目录，保证 rename 是原子的）。
func (m *Module) writeTemp(content string) (string, error) {
	ident, err := m.shellIdentity()
	if err != nil {
		return "", err
	}
	// 目录先准备好：文件权限再对，穿不过去的目录一样打不开。
	if err := m.prepareDir(ident); err != nil {
		return "", err
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("生成临时文件名失败: %w", err)
	}
	tmp := filepath.Join(m.dir, ".tmp-"+hex.EncodeToString(b[:]))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, FileMode)
	if err != nil {
		return "", fmt.Errorf("写入脚本临时文件失败: %w", err)
	}
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return "", fmt.Errorf("写入脚本临时文件失败: %w", err)
	}
	// 先把字节刷到盘上再改名：rename 只保证"名字"原子，不保证内容已经落盘。
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return "", fmt.Errorf("同步脚本临时文件失败: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("关闭脚本临时文件失败: %w", err)
	}
	// 属主仍然是 root，只把属组交给 upanel-shell。
	//
	// 脚本正文能不能改，是"管理员批准过的那一份"说了算；如果文件归 upanel-shell
	// 所有，被运行的脚本就能改写自己下一次要运行的内容，于是"批准的内容指纹"
	// 会变成一句空话。0750 + root:upanel-shell 只给了读与执行。
	if err := m.chownFile(tmp, 0, int(ident.gid)); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("设置脚本文件属组失败: %w", err)
	}
	return tmp, nil
}

// renameInto 把临时文件改名成最终文件。
func (m *Module) renameInto(tmp, final string) error {
	if err := os.Chmod(tmp, FileMode); err != nil {
		return fmt.Errorf("设置脚本权限失败: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		return fmt.Errorf("发布脚本文件失败: %w", err)
	}
	// 目录项也要落盘：机器在改名后立刻掉电时，"文件在、目录里没有"同样是事故。
	if d, err := os.Open(m.dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// verifyFile 核对磁盘上的字节与批准的内容指纹。
func (m *Module) verifyFile(path, wantHash string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("脚本文件 %s 不存在", path)
		}
		return fmt.Errorf("读取脚本文件 %s 失败: %w", path, err)
	}
	got := Hash(string(raw))
	if got != wantHash {
		return fmt.Errorf("脚本文件 %s 的内容指纹是 %s，批准的是 %s", path, got, wantHash)
	}
	return nil
}
