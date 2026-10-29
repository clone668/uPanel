package shares

// 判决与执行的单测（P08.1）。
//
// 这里用假的命令执行器、假的登记表、假的卷视图与假的秘密通道，把整条链路跑在
// 开发机上——不是图省事，而是因为**真实的系统写操作只在独立测试虚拟机里做**
// （PLAN.md 第 10 节）。开发机上没有 smbpasswd、没有 setfacl，唯一的真机验收在
// docs/evidence 里；这里保证的是：判决说什么、执行器发什么命令、读回不符时怎么回滚。
//
// 有两件事只有这一层能守：
//   1. 发给系统的命令是**什么**（尤其是密码永远不进 argv）；
//   2. 读回不符时**真的**回滚，而不是返回一个“部分成功”。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"upanel/internal/httpx"
	"upanel/internal/storage"
	"upanel/internal/syscmd"
)

// ---------- 假件 ----------

type fakeStore struct {
	mu       sync.Mutex
	shares   map[string]ShareRecord
	accounts map[string]AccountRecord
	saves    int
	deletes  int
	// saveErr / deleteErr 让落库这一步失败（P14.4 真机修正要用的场景）：
	// 22.04 验收 VM 上就是 SaveShare 被 CHECK 约束挡住，而导出文件已经写好并生效了。
	saveErr   error
	deleteErr error
}

func newFakeStore() *fakeStore {
	return &fakeStore{shares: map[string]ShareRecord{}, accounts: map[string]AccountRecord{}}
}

func (s *fakeStore) ListShares(context.Context) ([]ShareRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ShareRecord, 0, len(s.shares))
	for _, r := range s.shares {
		out = append(out, r)
	}
	return out, nil
}

func (s *fakeStore) GetShare(_ context.Context, id string) (ShareRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.shares[id]
	return r, ok, nil
}

func (s *fakeStore) GetShareByName(_ context.Context, name string) (ShareRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.shares {
		if r.Definition.Name == name {
			return r, true, nil
		}
	}
	return ShareRecord{}, false, nil
}

func (s *fakeStore) SaveShare(_ context.Context, rec ShareRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.saveErr != nil {
		return s.saveErr
	}
	s.shares[rec.ID] = rec
	s.saves++
	return nil
}

func (s *fakeStore) DeleteShare(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deleteErr != nil {
		return s.deleteErr
	}
	delete(s.shares, id)
	s.deletes++
	return nil
}

func (s *fakeStore) ListAccounts(context.Context) ([]AccountRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]AccountRecord, 0, len(s.accounts))
	for _, r := range s.accounts {
		out = append(out, r)
	}
	return out, nil
}

func (s *fakeStore) GetAccount(_ context.Context, id string) (AccountRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.accounts[id]
	return r, ok, nil
}

func (s *fakeStore) GetAccountByName(_ context.Context, name string) (AccountRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.accounts {
		if r.Name == name {
			return r, true, nil
		}
	}
	return AccountRecord{}, false, nil
}

func (s *fakeStore) SaveAccount(_ context.Context, rec AccountRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accounts[rec.ID] = rec
	s.saves++
	return nil
}

func (s *fakeStore) DeleteAccount(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.accounts, id)
	s.deletes++
	return nil
}

func (s *fakeStore) putShare(rec ShareRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.shares[rec.ID] = rec
}

func (s *fakeStore) putAccount(rec AccountRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accounts[rec.ID] = rec
}

type fakeVolumes struct {
	vols map[string]storage.Volume
}

func (v *fakeVolumes) Volume(_ context.Context, id string) (storage.Volume, error) {
	vol, ok := v.vols[id]
	if !ok {
		return storage.Volume{}, storage.ErrVolumeNotFound
	}
	return vol, nil
}

type fakeSecrets struct {
	values     map[string][]byte
	resolveErr error
	refErr     error
	references map[string]string
	resolved   []string
}

func newFakeSecrets() *fakeSecrets {
	return &fakeSecrets{values: map[string][]byte{}, references: map[string]string{}}
}

func (s *fakeSecrets) Resolve(_ context.Context, ref string) ([]byte, error) {
	s.resolved = append(s.resolved, ref)
	if s.resolveErr != nil {
		return nil, s.resolveErr
	}
	v, ok := s.values[ref]
	if !ok {
		return nil, errors.New("秘密不存在或已过期")
	}
	out := make([]byte, len(v))
	copy(out, v)
	return out, nil
}

func (s *fakeSecrets) Reference(_ context.Context, ref, reference string) error {
	if s.refErr != nil {
		return s.refErr
	}
	s.references[ref] = reference
	return nil
}

// call 是一次被记录下来的命令调用。
//
// Timeout 也记下来：同一个命令（getent）在预检与执行两个阶段的预算不同，
// 这是判决层与执行层最容易悄悄写成同一个数的东西，测试要能看见它。
type call struct {
	Program string
	Args    []string
	Stdin   string
	Timeout time.Duration
}

// fakeRunner 按 program + 首个参数决定返回什么，并记录每一次调用。
type fakeRunner struct {
	mu      sync.Mutex
	handled map[string]func(spec syscmd.Spec) (syscmd.Result, error)
	calls   []call
	// budgets 记录每个程序每次调用拿到的运行上限（按调用顺序）。
	// 预检与执行对同一个 getent 给的预算不同，只有这里看得出来。
	budgets map[string][]time.Duration
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{
		handled: map[string]func(syscmd.Spec) (syscmd.Result, error){},
		budgets: map[string][]time.Duration{},
	}
}

// budgetList 返回某个程序每次调用拿到的运行上限。
func (r *fakeRunner) budgetList(program string) []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Duration(nil), r.budgets[program]...)
}

// on 登记某个程序的处理函数。key 是程序绝对路径。
func (r *fakeRunner) on(program string, fn func(spec syscmd.Spec) (syscmd.Result, error)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handled[program] = fn
}

func resOK(stdout string) (syscmd.Result, error) {
	return syscmd.Result{ExitCode: 0, Stdout: stdout}, nil
}

func resFail(code int, stderr string) (syscmd.Result, error) {
	return syscmd.Result{ExitCode: code, Stderr: stderr}, &syscmd.ExitError{ExitCode: code}
}

func (r *fakeRunner) Run(_ context.Context, spec syscmd.Spec, limits syscmd.Limits) (syscmd.Result, error) {
	r.mu.Lock()
	r.calls = append(r.calls, call{
		Program: spec.Program,
		Args:    append([]string(nil), spec.Args...),
		Stdin:   string(spec.Stdin),
		Timeout: limits.Timeout,
	})
	r.budgets[spec.Program] = append(r.budgets[spec.Program], limits.Timeout)
	fn := r.handled[spec.Program]
	r.mu.Unlock()
	if fn == nil {
		return syscmd.Result{}, fmt.Errorf("fake runner: 没有为 %s 登记处理函数", spec.Program)
	}
	return fn(spec)
}

func (r *fakeRunner) find(program string, args ...string) (call, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.calls {
		if c.Program != program {
			continue
		}
		if len(args) > 0 && !hasArg(c.Args, args[0]) {
			continue
		}
		return c, true
	}
	return call{}, false
}

func (r *fakeRunner) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

// fakeFS 是 Lstat / ReadDir / Mkdir / Chmod 的假实现：只认识列出来的路径。
type fakeFS struct {
	files map[string]fakeFile
	dirs  map[string][]os.DirEntry
	made  []string
	// madePerm 记的是每个被创建目录**要求的**权限位。只记路径的话，
	// “上级 0711、目标 0700”这条区分在测试里就消失了。
	madePerm map[string]os.FileMode
	chmods   []string
}

type fakeFile struct {
	isDir   bool
	symlink bool
	// perm 为 0 表示用默认权限（目录 0755、文件 0644）。非 0 时按它来。
	perm os.FileMode
}

func (f *fakeFile) Name() string       { return "" }
func (f *fakeFile) Size() int64        { return 0 }
func (f *fakeFile) Mode() os.FileMode  { return f.mode() }
func (f *fakeFile) ModTime() time.Time { return time.Time{} }
func (f *fakeFile) IsDir() bool        { return f.isDir }
func (f *fakeFile) Sys() any           { return nil }

func (f *fakeFile) mode() os.FileMode {
	if f.symlink {
		return os.ModeSymlink | 0o777
	}
	if f.isDir {
		if f.perm != 0 {
			return os.ModeDir | f.perm
		}
		return os.ModeDir | 0o755
	}
	if f.perm != 0 {
		return f.perm
	}
	return 0o644
}

func newFakeFS() *fakeFS {
	return &fakeFS{
		files:    map[string]fakeFile{},
		dirs:     map[string][]os.DirEntry{},
		madePerm: map[string]os.FileMode{},
	}
}

func (f *fakeFS) addDir(path string) { f.files[path] = fakeFile{isDir: true} }

// addDirMode 造一个存在且带指定权限的目录（用来复现“上一版建出来的 0700 上级”）。
func (f *fakeFS) addDirMode(path string, perm os.FileMode) {
	f.files[path] = fakeFile{isDir: true, perm: perm}
}

func (f *fakeFS) lstat(name string) (os.FileInfo, error) {
	if fi, ok := f.files[name]; ok {
		return &fi, nil
	}
	return nil, &os.PathError{Op: "lstat", Path: name, Err: os.ErrNotExist}
}

func (f *fakeFS) readDir(name string) ([]os.DirEntry, error) {
	if f.files[name].isDir {
		return f.dirs[name], nil
	}
	return nil, &os.PathError{Op: "readdir", Path: name, Err: os.ErrNotExist}
}

// mkdir 与 os.MkdirAll 一样：已经存在的目录不报错、不改权限；缺的上级一起建出来，
// 而且都用同一个 perm。少了“一起建”这条，测试会让“上级被一起造出来”这件事看起来没发生。
func (f *fakeFS) mkdir(name string, perm os.FileMode) error {
	if _, ok := f.files[name]; ok {
		return nil
	}
	if parent := path.Dir(name); parent != name && parent != "." {
		if _, ok := f.files[parent]; !ok {
			if err := f.mkdir(parent, perm); err != nil {
				return err
			}
		}
	}
	f.made = append(f.made, name)
	f.madePerm[name] = perm.Perm()
	f.files[name] = fakeFile{isDir: true, perm: perm.Perm()}
	return nil
}

func (f *fakeFS) chmod(name string, mode os.FileMode) error {
	fi, ok := f.files[name]
	if !ok {
		return &os.PathError{Op: "chmod", Path: name, Err: os.ErrNotExist}
	}
	fi.perm = mode.Perm()
	f.files[name] = fi
	f.chmods = append(f.chmods, name+" "+mode.Perm().String())
	return nil
}

// ---------- 装配 ----------

// NL 是换行符。测试里的假命令输出都靠它拼——直接写转义序列在层层传递里容易被吃掉。
const NL = "\n"

const (
	testVolID  = "vol_0123456789abcdef0123456789abcdef"
	testUUID   = "5f1c9a3e-2b64-4c7d-8e10-9a3b5c7d1e2f"
	testAcctA  = "sac_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testAcctB  = "sac_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	testShareI = "shr_11111111111111111111111111111111"
	testMount  = "/srv/upanel/volumes/data"
	testDir    = testMount + "/projects/team"
	testSecret = "sec_22222222222222222222222222222222"
)

// 生产环境里这些绝对路径由能力探针找到；测试里直接给定，避免依赖开发机装了什么。
var testTools = ToolPaths{
	Smbd:       "/usr/sbin/smbd",
	Smbpasswd:  "/usr/bin/smbpasswd",
	Pdbedit:    "/usr/bin/pdbedit",
	Testparm:   "/usr/bin/testparm",
	Smbcontrol: "/usr/bin/smbcontrol",
	Smbstatus:  "/usr/bin/smbstatus",
	Setfacl:    "/usr/bin/setfacl",
	Getfacl:    "/usr/bin/getfacl",
	// NFS 的两个工具（P14.4）。放在这里而不是只在 NFS 专用测试里给：
	// `share.capability` 是一份报告，两种协议的能力同时出现在里面——
	// 让 SMB 的测试跑在一台“没装 NFS”的假机器上，就没法验证这份报告会漏掉谁。
	Exportfs:  "/usr/sbin/exportfs",
	Showmount: "/usr/sbin/showmount",
}

type harness struct {
	engine *Engine
	store  *fakeStore
	vols   *fakeVolumes
	sec    *fakeSecrets
	runner *fakeRunner
	fs     *fakeFS
	sys    *fakeSystem
	now    time.Time
	// cfg 是这台“小机器”上的 Samba 配置路径。
	//
	// 它们是 t.TempDir() 下的真实文件，配的是真实的 os 实现：写入、fsync、rename、
	// 备份、回滚都是真的在做，只有 testparm/smbcontrol/systemctl 是假程序。
	// 用内存假文件系统会让“候选不合格时正式文件一个字节都没动”这条断言失去意义。
	cfg ConfigPaths
	// reloads 记录 smbcontrol 被调了几次（幂等重放不该重载，靠它断言）。
	reloads int
	// serviceState 是 systemctl is-active 的答案（active/inactive/unknown）。
	serviceState string
	// testparmBroken 让整份配置校验失败（模拟合并后不合格）。
	testparmBroken bool
	// rejectCandidate 让候选文件校验失败（模拟渲染出来的段本身不合法）。
	rejectCandidate bool
	// statusJSON 是 smbstatus --json 的答案。
	statusJSON string
	// fsyncs 记录目录 fsync 被调了几次。
	fsyncs int
	// exportfs 是假的 NFS 导出表（P14.4）。
	//
	// 它是**有状态**的：`exportfs -ra` 真的去读那个临时文件再重算表，`-v` 再把表
	// 打印出来。“写了什么”与“生效了什么”因此是两件可以被分开断言的事。
	exportfs *fakeExportfs
}

// fakeSystem 是 getent/passwd/useradd/userdel/smbpasswd/pdbedit 的一台“小系统”。
//
// 它按**调用顺序**改变状态：useradd 之前 getent 说没有这个账号，之后说有。
// 判决与执行都在同一个环境里跑，靠固定返回值是模拟不出来的——预检时账号必须
// 不存在（否则会被判成“接管别人的账号”），执行后必须存在（否则读回对不上）。
type fakeSystem struct {
	mu        sync.Mutex
	users     map[string]int          // 用户名 → uid
	samba     map[string]SambaAccount // passdb 里的账号
	noPass    bool                    // 设过密码也不显示 Password last set（模拟失败）
	useradd   int
	userdel   int
	smbDelete int
}

func newFakeSystem() *fakeSystem {
	return &fakeSystem{users: map[string]int{}, samba: map[string]SambaAccount{}}
}

// seed 造一个已经存在的账号（Linux + passdb）。
func (s *fakeSystem) seed(name string, uid int, disabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.users[name] = uid
	s.samba[name] = SambaAccount{
		Name: name, Disabled: disabled, PasswordSet: true,
		PasswordSetAt: time.Date(2026, 10, 22, 9, 23, 39, 0, time.UTC),
	}
}

func (s *fakeSystem) exists(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.users[name]
	return ok
}

func (s *fakeSystem) passwdLine(name string, uid int) string {
	return fmt.Sprintf("%s:x:%d:%d::%s:%s", name, uid, uid, DefaultHome, DefaultShell)
}

// install 把六个程序的假实现挂到 runner 上。
func (s *fakeSystem) install(r *fakeRunner) {
	r.on(ProgramGetent, func(spec syscmd.Spec) (syscmd.Result, error) {
		name := spec.Args[len(spec.Args)-1]
		s.mu.Lock()
		uid, ok := s.users[name]
		s.mu.Unlock()
		if !ok {
			// getent 找不到时退出码是 2：这不是失败，是“没有”。
			return resFail(2, "")
		}
		return resOK(s.passwdLine(name, uid))
	})
	r.on(ProgramUseradd, func(spec syscmd.Spec) (syscmd.Result, error) {
		name := spec.Args[len(spec.Args)-1]
		s.mu.Lock()
		s.useradd++
		if _, taken := s.users[name]; taken {
			s.mu.Unlock()
			return resFail(9, "useradd: user "+name+" already exists")
		}
		s.users[name] = 999
		s.mu.Unlock()
		return resOK("")
	})
	r.on(ProgramUserdel, func(spec syscmd.Spec) (syscmd.Result, error) {
		name := spec.Args[len(spec.Args)-1]
		s.mu.Lock()
		s.userdel++
		delete(s.users, name)
		s.mu.Unlock()
		return resOK("")
	})
	r.on(ProgramPasswd, func(spec syscmd.Spec) (syscmd.Result, error) {
		name := spec.Args[len(spec.Args)-1]
		if !s.exists(name) {
			return resFail(1, "passwd: user does not exist")
		}
		return resOK(name + " L 10/22/2026 -1 -1 -1 -1" + NL)
	})
	r.on("/usr/bin/smbpasswd", func(spec syscmd.Spec) (syscmd.Result, error) {
		if hasArg(spec.Args, "-x") {
			name := spec.Args[len(spec.Args)-1]
			s.mu.Lock()
			s.smbDelete++
			delete(s.samba, name)
			s.mu.Unlock()
			return resOK("Deleted user " + name + "." + NL)
		}
		name := spec.Args[len(spec.Args)-1]
		s.mu.Lock()
		acc, ok := s.samba[name]
		if !ok {
			acc = SambaAccount{Name: name}
		}
		if !s.noPass {
			// smbpasswd -s 会把两行密码读走；这里只记“设过了”。
			acc.PasswordSet = true
			acc.PasswordSetAt = time.Date(2026, 10, 22, 9, 23, 39, 0, time.UTC)
		}
		if hasArg(spec.Args, "-d") {
			acc.Disabled = true
			acc.Flags = "DU"
		}
		if hasArg(spec.Args, "-e") {
			acc.Disabled = false
			acc.Flags = "U"
		}
		s.samba[name] = acc
		s.mu.Unlock()
		return resOK("Added user " + name + "." + NL)
	})
	r.on("/usr/bin/pdbedit", func(spec syscmd.Spec) (syscmd.Result, error) {
		name := spec.Args[len(spec.Args)-1]
		s.mu.Lock()
		acc, ok := s.samba[name]
		s.mu.Unlock()
		if !ok {
			return resFail(1, "Username not found!")
		}
		flags := "[U          ]"
		if acc.Disabled {
			flags = "[DU         ]"
		}
		out := "Unix username:        " + acc.Name + NL +
			"Account Flags:        " + flags + NL
		if acc.PasswordSet && !s.noPass {
			out += "Password last set:    Thu, 22 Oct 2026 09:23:39 UTC" + NL
		} else {
			out += "Password last set:    0" + NL
		}
		return resOK(out)
	})
}

// newHarness 造一个“Samba 装好了、卷挂好了、目录还不存在”的环境。
func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		store:  newFakeStore(),
		vols:   &fakeVolumes{vols: map[string]storage.Volume{}},
		sec:    newFakeSecrets(),
		runner: newFakeRunner(),
		fs:     newFakeFS(),
		now:    time.Date(2026, 10, 22, 9, 30, 0, 0, time.UTC),
	}
	h.vols.vols[testVolID] = storage.Volume{
		ID:             testVolID,
		Version:        "3",
		DisplayName:    "数据卷",
		Slug:           "data",
		FilesystemUUID: testUUID,
		FilesystemType: storage.FilesystemExt4,
		MountPath:      testMount,
		DesiredState:   storage.DesiredMounted,
		ObservedState:  storage.VolumeStateMounted,
	}
	h.fs.addDir(testMount)
	h.fs.addDir(testMount + "/projects")
	h.sys = newFakeSystem()
	h.sys.install(h.runner)

	// 配置写入的真实落点：一个临时目录，结构和 /etc/samba 一样。
	dir := t.TempDir()
	h.cfg = ConfigPaths{
		Main:       filepath.Join(dir, "smb.conf"),
		Managed:    filepath.Join(dir, "upanel.d", "upanel.conf"),
		IncludeDir: filepath.Join(dir, "upanel.d"),
		BackupDir:  filepath.Join(dir, "backups"),
		// 开机依赖也落在这个临时目录里：/etc/systemd 是 root 的，而且
		// “内容没变就不重载”“路径非法就不写”这些判断必须能在测试里反复跑。
		StartupDep: filepath.Join(dir, "systemd", "smbd.service.d", "upanel-shares.conf"),
		// 导出这一半的四个路径（P14.4），同样落在临时目录里：
		// “候选写坏时正式文件不动”“读回不符就还原”这些判断必须能反复跑。
		Exports:           filepath.Join(dir, "exports.d", "upanel.exports"),
		ExportsDir:        filepath.Join(dir, "exports.d"),
		ExportsMain:       filepath.Join(dir, "exports"),
		ExportsStartupDep: filepath.Join(dir, "systemd", "nfs-server.service.d", "upanel-exports.conf"),
	}
	if err := os.WriteFile(h.cfg.Main, []byte("[global]\n   workgroup = WORKGROUP\n"), 0o644); err != nil {
		t.Fatalf("准备 smb.conf 失败: %v", err)
	}
	h.serviceState = ServiceStateActive
	h.statusJSON = `{"timestamp":"2026-10-22T12:55:52.192073+0000","version":"4.19.5-Ubuntu",` +
		`"sessions":{},"tcons":{},"open_files":{}}`
	h.installConfigPrograms()
	h.exportfs = &fakeExportfs{}
	h.exportfs.install(h.runner, h.cfg.Exports)

	prober := NewProber(ProberOptions{
		// 能力探针问的是固定常量路径，测试里把它们映射到临时目录：
		// 这样“include 接没接”“托管文件里有什么”都由真实文件回答，而不是常量。
		Stat: func(name string) (os.FileInfo, error) {
			if name == DefaultIncludeDir {
				return os.Stat(h.cfg.IncludeDir)
			}
			if name == DefaultExportsDir {
				return os.Stat(h.cfg.ExportsDir)
			}
			fi := fakeFile{isDir: true}
			return &fi, nil
		},
		ReadFile: func(name string) ([]byte, error) {
			switch name {
			case DefaultConfigPath:
				return os.ReadFile(h.cfg.Main)
			case ManagedConfigPath:
				return os.ReadFile(h.cfg.Managed)
			case DefaultExportsFile:
				return os.ReadFile(h.cfg.Exports)
			case DefaultExportsMain:
				return os.ReadFile(h.cfg.ExportsMain)
			}
			return nil, os.ErrNotExist
		},
		Runner: h.runner,
		Now:    func() time.Time { return h.now },
		Paths:  testTools,
	})
	h.runner.on("/usr/sbin/smbd", func(syscmd.Spec) (syscmd.Result, error) {
		return resOK("Version 4.19.5-Ubuntu\n")
	})

	e, err := New(Options{
		Store:   h.store,
		Volumes: h.vols,
		Secrets: h.sec,
		Runner:  h.runner,
		Prober:  prober,
		Now:     func() time.Time { return h.now },
		Lstat:   h.fs.lstat,
		ReadDir: h.fs.readDir,
		Mkdir:   h.fs.mkdir,
		Chmod:   h.fs.chmod,

		ConfigPaths: h.cfg,
		// 真实的写、改名、备份、回滚；只有目录 fsync 换成计数桩——Windows 上对
		// 目录句柄做 sync 会直接 Access denied，那是宿主平台的限制，不是被测代码的问题。
		// “有没有在正确的位置 fsync”由 h.fsyncs 断言，“fsync 真的落了盘”由虚拟机上
		// 的验收脚本负责（docs/evidence/P08/2026-10-22-p08.2/）。
		ConfigFS: func() ConfigFS {
			fs := SystemConfigFS()
			fs.FsyncDir = func(dir string) error {
				h.fsyncs++
				return nil
			}
			return fs
		}(),
	})
	if err != nil {
		t.Fatalf("构造 Engine 失败: %v", err)
	}
	h.engine = e
	return h
}

// putManagedAccount 往登记表里放一个受管理账号。
func (h *harness) putManagedAccount(id, name string, disabled bool) {
	h.store.putAccount(AccountRecord{
		ID: id, Name: name, Type: AccountTypeShare, Managed: true,
		Disabled: disabled, HasPassword: true,
		UID: 999, GID: 999, Shell: DefaultShell, Home: DefaultHome,
		SecretRef: testSecret, Version: "1",
		CreatedAt: h.now, UpdatedAt: h.now,
	})
}

// applyParams 造一份合法的 share.apply 参数。
func (h *harness) applyParams() ShareApplyParams {
	def := baseDefinition()
	def.SMBUsers = []ShareUser{
		{AccountID: testAcctA, Access: AccessRead},
		{AccountID: testAcctB, Access: AccessWrite},
	}
	return ShareApplyParams{Definition: &def}
}

// setfaclOK / getfaclReturns 让 setfacl 成功、getfacl 返回指定输出。
func (h *harness) setfaclOK() {
	h.runner.on("/usr/bin/setfacl", func(syscmd.Spec) (syscmd.Result, error) { return resOK("") })
}

func (h *harness) getfaclReturns(out string) {
	h.runner.on("/usr/bin/getfacl", func(syscmd.Spec) (syscmd.Result, error) { return resOK(out) })
}

// wantedGetfacl 是“按期望写入后”读回来的真实形状。
const wantedGetfacl = `user::rwx
user:alice:r-x
user:bob:rwx
group::---
mask::rwx
other::---
`

// ---------- 判决 ----------

func TestAssessShareApplyAllowedWithFullFacts(t *testing.T) {
	h := newHarness(t)
	h.putManagedAccount(testAcctA, "alice", false)
	h.putManagedAccount(testAcctB, "bob", false)

	a, err := h.engine.Assess(context.Background(), ActionShareApply, h.applyParams())
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	if !a.Allowed {
		t.Fatalf("应该允许，阻塞项：%v", a.Blockers)
	}
	if a.IdentityFingerprint != testUUID {
		t.Errorf("目标身份是 %q，期望卷的文件系统 UUID %q", a.IdentityFingerprint, testUUID)
	}
	if a.DisplayName != "team-data" {
		t.Errorf("显示名是 %q", a.DisplayName)
	}
	// 锁必须覆盖共享名、卷与整份托管配置，且排序稳定（并发取锁时顺序不一致会死锁）。
	wantLocks := []string{ConfigLockName, "share:team-data", "volume:" + testVolID}
	if len(a.Locks) != len(wantLocks) {
		t.Fatalf("锁是 %v，期望 %v", a.Locks, wantLocks)
	}
	for i := range wantLocks {
		if a.Locks[i] != wantLocks[i] {
			t.Fatalf("锁是 %v，期望 %v", a.Locks, wantLocks)
		}
	}
	// P08.1 的固定告警：定义准备好了但共享还没被提供。
	if !hasBlockerCode(a.Warnings, NoticeShareConfigNotApplied) {
		t.Errorf("缺少 %s 告警：%v", NoticeShareConfigNotApplied, a.Warnings)
	}
}

func TestAssessShareApplyBlocksWhenVolumeUnmounted(t *testing.T) {
	h := newHarness(t)
	h.putManagedAccount(testAcctA, "alice", false)
	h.putManagedAccount(testAcctB, "bob", false)
	vol := h.vols.vols[testVolID]
	vol.ObservedState = storage.VolumeStateUnmounted
	vol.ObservedError = "挂载点下没有挂载任何文件系统"
	h.vols.vols[testVolID] = vol

	a, err := h.engine.Assess(context.Background(), ActionShareApply, h.applyParams())
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	if a.Allowed {
		t.Fatal("卷没挂载却允许了：共享目录会落在系统盘的空目录上")
	}
	b := findBlocker(a.Blockers, NoticeVolumeNotMounted)
	if b == nil {
		t.Fatalf("没有 %s 阻塞项：%v", NoticeVolumeNotMounted, a.Blockers)
	}
	if !strings.Contains(b.Message, "系统盘上的空目录") {
		t.Errorf("阻塞说明没有说清后果：%s", b.Message)
	}
}

func TestAssessShareApplyBlocksMissingAccountAndEmptyUsers(t *testing.T) {
	h := newHarness(t)
	h.putManagedAccount(testAcctA, "alice", false)

	a, err := h.engine.Assess(context.Background(), ActionShareApply, h.applyParams())
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	if a.Allowed {
		t.Fatal("引用了不存在的账号却允许了")
	}
	if findBlocker(a.Blockers, NoticeAccountMissing) == nil {
		t.Fatalf("没有 %s：%v", NoticeAccountMissing, a.Blockers)
	}

	p := h.applyParams()
	p.Definition.SMBUsers = nil
	a, err = h.engine.Assess(context.Background(), ActionShareApply, p)
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	if a.Allowed {
		t.Fatal("smb_users 为空却允许了：那会在 Samba 里退化成 guest")
	}
}

func TestAssessShareApplyBlocksPathEscapeAndSymlink(t *testing.T) {
	h := newHarness(t)
	h.putManagedAccount(testAcctA, "alice", false)
	h.putManagedAccount(testAcctB, "bob", false)

	p := h.applyParams()
	p.Definition.RelativePath = "../outside"
	a, err := h.engine.Assess(context.Background(), ActionShareApply, p)
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	if a.Allowed {
		t.Fatal("相对路径跑到卷外却允许了")
	}

	// 卷内路径上有一层是符号链接。
	h.fs.files[testMount+"/projects"] = fakeFile{symlink: true}
	p = h.applyParams()
	a, err = h.engine.Assess(context.Background(), ActionShareApply, p)
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	if a.Allowed {
		t.Fatal("路径上有符号链接却允许了")
	}
	if findBlocker(a.Blockers, NoticeSharePathSymlink) == nil {
		t.Fatalf("没有 %s：%v", NoticeSharePathSymlink, a.Blockers)
	}
}

func TestAssessShareApplyBlocksNameTakenAndStaleVersion(t *testing.T) {
	h := newHarness(t)
	h.putManagedAccount(testAcctA, "alice", false)
	h.putManagedAccount(testAcctB, "bob", false)
	def := baseDefinition()
	h.store.putShare(ShareRecord{
		ID: testShareI, Definition: def, ConfigHash: "h", Version: "2",
		CreatedAt: h.now, UpdatedAt: h.now,
	})

	a, err := h.engine.Assess(context.Background(), ActionShareApply, h.applyParams())
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	if a.Allowed || findBlocker(a.Blockers, NoticeShareNameTaken) == nil {
		t.Fatalf("同名共享已存在却没有 %s：%v", NoticeShareNameTaken, a.Blockers)
	}

	// 更新路径：版本过期要拒。
	stale := "1"
	p := h.applyParams()
	p.ShareID = strPtr(testShareI)
	p.Version = &stale
	a, err = h.engine.Assess(context.Background(), ActionShareApply, p)
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	if a.Allowed || findBlocker(a.Blockers, NoticeShareVersionMismatch) == nil {
		t.Fatalf("版本过期却没有 %s：%v", NoticeShareVersionMismatch, a.Blockers)
	}
	if a.StateVersion != "2" {
		t.Errorf("状态版本是 %q，期望 2", a.StateVersion)
	}
}

func TestAssessAccountApplyMarksNotManaged(t *testing.T) {
	h := newHarness(t)
	// 系统里已有同名账号（不是面板建的）：getent 返回一行。
	h.runner.on("/usr/bin/getent", func(syscmd.Spec) (syscmd.Result, error) {
		return resOK("legacy:x:1001:1001::/home/legacy:/bin/bash\n")
	})
	p := AccountApplyParams{Name: "legacy", SecretRef: testSecret}
	a, err := h.engine.Assess(context.Background(), ActionAccountApply, p)
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	if a.Allowed {
		t.Fatal("系统里已有非受管理账号却允许了：会静默接管别人的账号")
	}
	if findBlocker(a.Blockers, NoticeAccountNotManaged) == nil {
		t.Fatalf("没有 %s：%v", NoticeAccountNotManaged, a.Blockers)
	}
}

func TestAssessAccountDeleteRefusedWhileReferenced(t *testing.T) {
	h := newHarness(t)
	h.putManagedAccount(testAcctA, "alice", false)
	def := baseDefinition()
	def.SMBUsers = []ShareUser{{AccountID: testAcctA, Access: AccessRead}}
	h.store.putShare(ShareRecord{
		ID: testShareI, Definition: def, ConfigHash: "h", Version: "1",
		CreatedAt: h.now, UpdatedAt: h.now,
	})

	a, err := h.engine.Assess(context.Background(), ActionAccountDelete, AccountRefParams{AccountID: testAcctA})
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	if a.Allowed {
		t.Fatal("账号还被共享引用却允许删除了")
	}
	if findBlocker(a.Blockers, NoticeAccountHasShares) == nil {
		t.Fatalf("没有 %s：%v", NoticeAccountHasShares, a.Blockers)
	}
}

// TestAssessAccountDeleteBindsTarget — 删除的预检必须绑定一个可复核的目标。
//
// 真机 P08.1 验收 §9 撞到的就是它：账号存在、没有阻塞项，判决 allowed=true，
// 但目标身份指纹是空的，validatePlanResult 把这份判决判成“预检结果不合法”（500），
// 接口回“服务内部错误”，账号自然一个都没删掉。断言：允许、指纹是账号名、
// 状态版本非空，并且用判决自己的身份与版本能过校验器的三条规则。
func TestAssessAccountDeleteBindsTarget(t *testing.T) {
	h := newHarness(t)
	h.putManagedAccount(testAcctA, "alice", false)

	a, err := h.engine.Assess(context.Background(), ActionAccountDelete, AccountRefParams{AccountID: testAcctA})
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	if !a.Allowed {
		t.Fatalf("没有引用的账号却不允许删除: %v", a.Blockers)
	}
	if a.IdentityFingerprint != "alice" {
		t.Fatalf("身份指纹应是账号名 alice，实际 %q", a.IdentityFingerprint)
	}
	if a.StateVersion == "" {
		t.Fatal("允许执行的判决必须绑定状态版本")
	}
	if a.TargetID != testAcctA {
		t.Fatalf("目标标识应是 %s，实际 %q", testAcctA, a.TargetID)
	}
}

// TestSystemUserExistsRetriesOnceOnTimeout — getent 超时重试一次。
//
// 真机 P08.1 验收撞到过：同一个名字在预检里是十几毫秒，4 秒后的执行前复核把 3 秒
// 预算跑满（宿主同时在构建，虚拟机只有 2 个 vCPU），于是一条本来正确的建账号操作
// 以 500 收场。观察类命令是幂等的读，超时再问一次就够了。
func TestSystemUserExistsRetriesOnceOnTimeout(t *testing.T) {
	h := newHarness(t)
	var calls int
	h.runner.on(ProgramGetent, func(syscmd.Spec) (syscmd.Result, error) {
		calls++
		if calls == 1 {
			return syscmd.Result{ExitCode: -1, TimedOut: true},
				fmt.Errorf("%w（/usr/bin/getent）", syscmd.ErrTimeout)
		}
		return resOK("alice:x:997:987::/nonexistent:/usr/sbin/nologin\n")
	})

	exists, err := h.engine.systemUserExists(context.Background(), "alice", accountLookupTimeout)
	if err != nil {
		t.Fatalf("第二次拿到了答案却报错: %v", err)
	}
	if !exists {
		t.Fatal("应当判定账号存在")
	}
	if calls != 2 {
		t.Fatalf("应当恰好问两次，实际 %d 次", calls)
	}
}

// TestSystemUserExistsDoesNotRetryOnRealFailure — 只有超时才重试。
//
// 退出码 1 是这台机器的 NSS 配置问题，重试还是同一个答案，白等一次预算。
// 退出码 2 是“没有这个账号”，本来就不是失败。
func TestSystemUserExistsDoesNotRetryOnRealFailure(t *testing.T) {
	cases := []struct {
		name  string
		code  int
		want  bool
		isErr bool
	}{
		{name: "退出码 1", code: 1, want: false, isErr: true},
		{name: "退出码 2", code: 2, want: false, isErr: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			var calls int
			h.runner.on(ProgramGetent, func(syscmd.Spec) (syscmd.Result, error) {
				calls++
				return resFail(c.code, "getent: unsupported database")
			})
			exists, err := h.engine.systemUserExists(context.Background(), "alice", accountLookupTimeout)
			if c.isErr != (err != nil) {
				t.Fatalf("isErr=%v，实际 err=%v", c.isErr, err)
			}
			if exists != c.want {
				t.Fatalf("exists 应为 %v，实际 %v", c.want, exists)
			}
			if calls != 1 {
				t.Fatalf("不该重试，实际问了 %d 次", calls)
			}
		})
	}
}

// TestAssessAccountApplyInvalidNameSkipsSystemLookup — 名字已经不合法时不去问系统。
//
// 真机 P08.1 验收里 “Bad_Name” 这一条得到的是 500：判决层先去 getent，而这台
// 2 vCPU 的机器上 getent 被挤到超过两次 1.5 秒，错误直接冒成“服务内部错误”。
// 断言：名字不合法时一次 getent 都不发（runner 被设成永远超时，发了就必然报错），
// 判决照常给 ACCOUNT_NAME_INVALID。
func TestAssessAccountApplyInvalidNameSkipsSystemLookup(t *testing.T) {
	h := newHarness(t)
	var calls int
	h.runner.on(ProgramGetent, func(syscmd.Spec) (syscmd.Result, error) {
		calls++
		return syscmd.Result{ExitCode: -1, TimedOut: true}, fmt.Errorf("%w（/usr/bin/getent）", syscmd.ErrTimeout)
	})

	a, err := h.engine.Assess(context.Background(), ActionAccountApply, AccountApplyParams{Name: "Bad_Name"})
	if err != nil {
		t.Fatalf("名字不合法不该把预检变成错误: %v", err)
	}
	if a.Allowed {
		t.Fatal("不合法的账号名却被允许了")
	}
	if findBlocker(a.Blockers, NoticeAccountNameInvalid) == nil {
		t.Fatalf("没有 %s：%v", NoticeAccountNameInvalid, a.Blockers)
	}
	if calls != 0 {
		t.Fatalf("名字已经不合法，不该再问系统（实际问了 %d 次）", calls)
	}
}

// TestAssessAccountApplyLookupTimeoutIsWarning — 预检查询超时是告警，不是 500，也不是允许。
//
// 真机 P08.1 验收里，2 vCPU 的机器被服务采集器占住时 getent 实测卡到 4.7/7.3/7.5 秒，
// 预检那条 3 秒预算连两次 1.5 秒都跑满，于是一条正常的建账号预检以 500 收场。
// 判决层在这里的诚实答案是：我**没**核验出来，但我照样能判——因为“不接管既有账号”
// 这条不变量由执行阶段兜底（长预算复核 + useradd 对已存在的名字失败）。
// 断言：allowed 仍然是 true，带 ACCOUNT_LOOKUP_TIMEOUT 告警，并且确实问了两次。
func TestAssessAccountApplyLookupTimeoutIsWarning(t *testing.T) {
	h := newHarness(t)
	var calls int
	h.runner.on(ProgramGetent, func(syscmd.Spec) (syscmd.Result, error) {
		calls++
		return syscmd.Result{ExitCode: -1, TimedOut: true}, fmt.Errorf("%w（/usr/bin/getent）", syscmd.ErrTimeout)
	})

	a, err := h.engine.Assess(context.Background(), ActionAccountApply,
		AccountApplyParams{Name: "carol", SecretRef: testSecret})
	if err != nil {
		t.Fatalf("查询超时不该把预检变成错误: %v", err)
	}
	if !a.Allowed {
		t.Fatalf("查询超时不该阻塞一条本来可以做的操作，阻塞项：%v", a.Blockers)
	}
	if findBlocker(a.Warnings, NoticeAccountLookupTimeout) == nil {
		t.Fatalf("没有 %s 告警：%v", NoticeAccountLookupTimeout, a.Warnings)
	}
	if calls != accountLookupAttempts {
		t.Fatalf("超时后应当重试，实际问了 %d 次（期望 %d 次）", calls, accountLookupAttempts)
	}
	for _, budget := range h.runner.budgetList(ProgramGetent) {
		if want := lookupAttemptTimeout(accountLookupTimeout); budget != want {
			t.Fatalf("预检每次的上限是 %v，期望 %v", budget, want)
		}
	}
}

// TestAccountApplyExecutionLooksUpWithItsOwnBudget — 预检与执行用的不是同一个预算。
//
// 预检在 Web 等着的链路上（总 10 秒），执行在任务自己的预算里。同一个 getent
// 在两处拿到的运行上限必须不同：写成同一个数，要么预检吃掉整条链路，要么执行
// 在机器忙时误杀一次正确的操作。这里跑一遍完整的“预检 → 执行”，把两次的预算都记下来。
func TestAccountApplyExecutionLooksUpWithItsOwnBudget(t *testing.T) {
	h := newHarness(t)
	h.sec.values[testSecret] = []byte("cw-S3cret-Pass")

	p := AccountApplyParams{Name: "alice", SecretRef: testSecret}
	a, err := h.engine.Assess(context.Background(), ActionAccountApply, p)
	if err != nil {
		t.Fatalf("预检失败: %v", err)
	}
	if !a.Allowed {
		t.Fatalf("预检被拒：%v", a.Blockers)
	}
	if _, err := h.engine.ExecuteWrite(context.Background(), WriteRequest{
		Action:               ActionAccountApply,
		Params:               a.NormalizedParams,
		ExpectedIdentity:     a.IdentityFingerprint,
		ExpectedStateVersion: a.StateVersion,
		OperationID:          "op_budget",
	}); err != nil {
		t.Fatalf("执行失败: %v", err)
	}

	budgets := h.runner.budgetList(ProgramGetent)
	if len(budgets) < 2 {
		t.Fatalf("getent 只被问了 %d 次，预检与执行各至少一次", len(budgets))
	}
	if want := lookupAttemptTimeout(accountLookupTimeout); budgets[0] != want {
		t.Fatalf("预检的上限是 %v，期望 %v", budgets[0], want)
	}
	execWant := lookupAttemptTimeout(accountLookupExecTimeout)
	last := budgets[len(budgets)-1]
	if last != execWant {
		t.Fatalf("执行前复核的上限是 %v，期望 %v", last, execWant)
	}
	if execWant <= lookupAttemptTimeout(accountLookupTimeout) {
		t.Fatalf("执行预算（%v）没有比预检预算（%v）大：两处就该是两套数",
			execWant, lookupAttemptTimeout(accountLookupTimeout))
	}
}

// TestCreateAccountRefusesExistingUserOnExit9 — 名字被别人占了就停下，绝不接管。
//
// 这是预检那条 getent 超时之后的安全网：预检可能没问出结果（只发告警），
// 执行阶段的长预算复核也可能又撞上超时；这时 useradd 会以退出码 9 拒绝。
// 断言两件事：判决是 409 DEPENDENCY_BLOCKED 且点名 ACCOUNT_NOT_MANAGED，
// 并且**没有**继续去设 Samba 密码——那一步才是真正会把别人账号“接管”过来的动作。
func TestCreateAccountRefusesExistingUserOnExit9(t *testing.T) {
	h := newHarness(t)
	h.sec.values[testSecret] = []byte("cw-S3cret-Pass")
	// 系统查询说“没有”（NSS 缓存里的旧答案），useradd 才是现场真相。
	h.runner.on(ProgramUseradd, func(syscmd.Spec) (syscmd.Result, error) {
		return resFail(9, "useradd: user 'alice' already exists")
	})

	p := AccountApplyParams{Name: "alice", SecretRef: testSecret}
	a, err := h.engine.Assess(context.Background(), ActionAccountApply, p)
	if err != nil {
		t.Fatalf("预检失败: %v", err)
	}
	if !a.Allowed {
		t.Fatalf("预检被拒：%v", a.Blockers)
	}
	_, err = h.engine.ExecuteWrite(context.Background(), WriteRequest{
		Action:               ActionAccountApply,
		Params:               a.NormalizedParams,
		ExpectedIdentity:     a.IdentityFingerprint,
		ExpectedStateVersion: a.StateVersion,
		OperationID:          "op_exit9",
	})
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("执行返回的错误不是带判决的拒绝：%v", err)
	}
	if httpErr.Code != httpx.CodeDependencyBlocked || httpErr.Status != 409 {
		t.Fatalf("判决是 %d/%s，期望 409/%s", httpErr.Status, httpErr.Code, httpx.CodeDependencyBlocked)
	}
	if !strings.Contains(httpErr.Message, NoticeAccountNotManaged) {
		t.Fatalf("拒绝文案里没有点名 %s：%s", NoticeAccountNotManaged, httpErr.Message)
	}
	if _, found := h.runner.find(testTools.Smbpasswd); found {
		t.Fatal("useradd 失败之后还去设了 Samba 密码：这就是接管别人的账号")
	}
}

func TestAssessAccountApplyRejectsRename(t *testing.T) {
	h := newHarness(t)
	h.putManagedAccount(testAcctA, "alice", false)
	p := AccountApplyParams{AccountID: strPtr(testAcctA), Name: "alice2"}
	a, err := h.engine.Assess(context.Background(), ActionAccountApply, p)
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	if a.Allowed {
		t.Fatal("改名却没有被拒")
	}
	if findBlocker(a.Blockers, NoticeAccountNameInvalid) == nil {
		t.Fatalf("没有 %s：%v", NoticeAccountNameInvalid, a.Blockers)
	}
}

// ---------- 执行 ----------

func TestExecuteShareApplyWritesConfigAndServes(t *testing.T) {
	h := newHarness(t)
	h.putManagedAccount(testAcctA, "alice", false)
	h.putManagedAccount(testAcctB, "bob", false)
	h.setfaclOK()
	h.getfaclReturns(wantedGetfacl)

	p := h.applyParams()
	a, err := h.engine.Assess(context.Background(), ActionShareApply, p)
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	report, err := h.engine.ExecuteWrite(context.Background(), WriteRequest{
		Action:               ActionShareApply,
		Params:               a.NormalizedParams,
		ExpectedIdentity:     a.IdentityFingerprint,
		ExpectedStateVersion: a.StateVersion,
		OperationID:          "op_test_apply",
	})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	// P08.2：配置写下去了，testparm 两次都过，smbd 在跑，重载被接受——所以是 verified。
	if report.Outcome != OutcomeVerified {
		t.Fatalf("结果状态是 %q，期望 %q（错误=%v）", report.Outcome, OutcomeVerified, report.Observed)
	}
	if report.Observed["served"] != true {
		t.Errorf("served = %v，期望 true", report.Observed["served"])
	}
	if report.Observed["samba_config"] != ConfigFileWritten {
		t.Errorf("samba_config = %v，期望 %q", report.Observed["samba_config"], ConfigFileWritten)
	}
	if report.Observed["include"] != IncludeInstalled {
		t.Errorf("include = %v，期望 %q", report.Observed["include"], IncludeInstalled)
	}
	if report.Observed["acl_verified"] != true {
		t.Errorf("acl_verified = %v", report.Observed["acl_verified"])
	}
	if h.reloads != 1 {
		t.Errorf("smbcontrol 被调了 %d 次，期望 1 次", h.reloads)
	}
	if h.fsyncs == 0 {
		t.Error("没有对目录做 fsync：rename 掉电后可能回退")
	}

	// 目录必须真的建了，而且两种权限分开：目标 0700（先紧后松）。
	//
	// 这条断言的第一版只要求“建了一个目录、权限 0700”，于是它把缺陷一起钉住了：
	// MkdirAll 用 0700 把缺的上级一起造出来，共享账号连目标目录都进不去（Q233）。
	// 这里的上级 projects 是夹具里本来就有的 0755，够穿越，不该被动。
	if len(h.fs.made) != 1 || h.fs.made[0] != testDir {
		t.Fatalf("创建的目录是 %v，期望 [%s]", h.fs.made, testDir)
	}
	if perm := h.fs.madePerm[testDir]; perm != 0o700 {
		t.Errorf("共享目录 %s 的权限是 %04o，期望 0700", testDir, perm)
	}
	if len(h.fs.chmods) != 0 {
		t.Errorf("不该改已有上级目录的权限，实际改了：%v", h.fs.chmods)
	}
	// setfacl 的参数必须是一份完整 ACL，且只含定义里的两个账号。
	setc, found := h.runner.find("/usr/bin/setfacl")
	if !found {
		t.Fatal("没有调用 setfacl")
	}
	spec := setc.Args[len(setc.Args)-2]
	want := "user::rwx,group::---,other::---,user:alice:r-x,user:bob:rwx"
	if spec != want {
		t.Fatalf("setfacl 的 ACL 是\n%s\n期望\n%s", spec, want)
	}

	// 托管文件里必须有这一段（而且是按定义渲染出来的那一段）。
	managed := h.readManaged()
	if !strings.Contains(managed, "[team-data]") {
		t.Fatalf("托管配置里没有 [team-data]：\n%s", managed)
	}
	if !strings.Contains(managed, "path = "+testDir) {
		t.Errorf("托管配置里的 path 不是 %s：\n%s", testDir, managed)
	}
	// 主配置里只加了两行：注释 + include。别的字节一个都没动。
	main := h.readMain()
	if !strings.HasPrefix(main, "[global]\n   workgroup = WORKGROUP\n") {
		t.Errorf("主配置原有内容被改动了：\n%s", main)
	}
	if strings.Count(main, "include = ") != 1 {
		t.Errorf("include 行数不是 1：\n%s", main)
	}

	// 落库的记录：目录准备好了、配置已应用、配置哈希与渲染结果一致。
	recs, err := h.store.ListShares(context.Background())
	if err != nil || len(recs) != 1 {
		t.Fatalf("登记表里有 %d 条共享（err=%v）", len(recs), err)
	}
	rec := recs[0]
	if !rec.DirectoryPrepared {
		t.Error("directory_prepared 是 false")
	}
	if !rec.ConfigApplied {
		t.Error("config_applied 是 false：配置已经写进托管文件了")
	}
	if rec.ConfigHash == "" {
		t.Error("配置哈希是空的")
	}
	if rec.Version != "1" {
		t.Errorf("新共享的版本是 %q，期望 1", rec.Version)
	}
	if rec.LastOperationID != "op_test_apply" {
		t.Errorf("last_operation_id 是 %q", rec.LastOperationID)
	}
}

// TestExecuteShareApplyClearsRuntimeRevocation 钉住“重新应用真的是出路”。
//
// 真机上抓到的缺陷（P08.4 第 9 节）：卷掉线 → 对账把共享段撤掉、记下 runtime_revoked；
// 人点“重新应用” → 配置写回去、config_applied 回到真、客户端真的又能连了，**可是
// runtime_revoked 一直在**。于是页面永远显示“运行时已撤销”，并且 clientHintsUsable 恒假：
// CONTRACTS §7.4(2) 要求的同时给出现状与出路，变成了一条只通告坏消息的死路。
//
// 这里的断言是：apply 成功后记录里没有撤销现场；撤销仍然只由对账产生（卷再掉线会重新记）。
func TestExecuteShareApplyClearsRuntimeRevocation(t *testing.T) {
	h := newHarness(t)
	h.putManagedAccount(testAcctA, "alice", false)
	h.putManagedAccount(testAcctB, "bob", false)
	h.setfaclOK()
	h.getfaclReturns(wantedGetfacl)

	// 一条已经落在册、被卷掉线撤销过的共享：config_applied 已经是假，配置里没有它。
	id := "shr_0123456789abcdef0123456789abcdef"
	def := *h.applyParams().Definition
	h.store.putShare(ShareRecord{
		ID:                id,
		Definition:        def,
		ConfigHash:        strings.Repeat("0", 64),
		Version:           "3",
		DirectoryPrepared: true,
		ConfigApplied:     false,
		RuntimeRevoked: &RuntimeRevocation{
			At:          h.now,
			VolumeID:    def.VolumeID,
			VolumeName:  "数据卷",
			VolumeState: "unmounted",
			Reason:      "卷掉线，配置段已撤销",
		},
		CreatedAt: h.now,
		UpdatedAt: h.now,
	})

	p := h.applyParams()
	p.ShareID = &id
	version := "3"
	p.Version = &version

	a, err := h.engine.Assess(context.Background(), ActionShareApply, p)
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	for _, b := range a.Blockers {
		t.Fatalf("重新应用被挡住：%s %s", b.Code, b.Message)
	}
	if _, err := h.engine.ExecuteWrite(context.Background(), WriteRequest{
		Action:               ActionShareApply,
		Params:               a.NormalizedParams,
		ExpectedIdentity:     a.IdentityFingerprint,
		ExpectedStateVersion: a.StateVersion,
		OperationID:          "op_test_reapply",
	}); err != nil {
		t.Fatalf("重新应用执行失败: %v", err)
	}

	rec, found, err := h.store.GetShare(context.Background(), id)
	if err != nil || !found {
		t.Fatalf("重新应用之后记录不见了（err=%v）", err)
	}
	if rec.RuntimeRevoked != nil {
		t.Fatalf("重新应用成功了，撤销现场却还留着：%+v（页面会永远显示“已撤销”）", rec.RuntimeRevoked)
	}
	if !rec.ConfigApplied {
		t.Error("config_applied 没回到真")
	}
	if rec.Version != "4" {
		t.Errorf("版本是 %q，期望 4（重新应用也算一次改动）", rec.Version)
	}
	if !strings.Contains(h.readManaged(), "[team-data]") {
		t.Error("重新应用之后托管配置里没有这一段")
	}
}

// TestExecuteShareApplyIdempotentReload 验证幂等重放：内容没变就不重载。
func TestExecuteShareApplyIdempotentReload(t *testing.T) {
	h := newHarness(t)
	h.putManagedAccount(testAcctA, "alice", false)
	h.putManagedAccount(testAcctB, "bob", false)
	h.setfaclOK()
	h.getfaclReturns(wantedGetfacl)

	apply := func(version *string, id *string) WriteReport {
		p := h.applyParams()
		p.ShareID = id
		p.Version = version
		a, err := h.engine.Assess(context.Background(), ActionShareApply, p)
		if err != nil {
			t.Fatalf("判决失败: %v", err)
		}
		report, err := h.engine.ExecuteWrite(context.Background(), WriteRequest{
			Action:               ActionShareApply,
			Params:               a.NormalizedParams,
			ExpectedIdentity:     a.IdentityFingerprint,
			ExpectedStateVersion: a.StateVersion,
		})
		if err != nil {
			t.Fatalf("执行失败: %v", err)
		}
		return report
	}

	first := apply(nil, nil)
	if first.Observed["samba_config"] != ConfigFileWritten {
		t.Fatalf("第一次 samba_config = %v", first.Observed["samba_config"])
	}
	if h.reloads != 1 {
		t.Fatalf("第一次重载了 %d 次，期望 1 次", h.reloads)
	}

	// 同一个共享再应用一次：内容一模一样，文件不写、也不重载。
	recs, _ := h.store.ListShares(context.Background())
	rec := recs[0]
	version := rec.Version
	id := rec.ID
	second := apply(&version, &id)
	if second.Observed["samba_config"] != ConfigFileUnchanged {
		t.Errorf("第二次 samba_config = %v，期望 %q", second.Observed["samba_config"], ConfigFileUnchanged)
	}
	if second.Observed["reload"] != ReloadSkippedUnchanged {
		t.Errorf("第二次 reload = %v，期望 %q", second.Observed["reload"], ReloadSkippedUnchanged)
	}
	if h.reloads != 1 {
		t.Errorf("幂等重放又重载了（共 %d 次）：内容没变就不该重载", h.reloads)
	}
	if second.Observed["served"] != true {
		t.Errorf("第二次 served = %v，期望 true（配置仍然在提供服务）", second.Observed["served"])
	}
}

// TestExecuteShareApplyServiceInactiveIsAccepted 验证“配置写好了但服务没跑”时
// 只能是 accepted —— 这时共享不对外提供访问，说 verified 就是造假。
func TestExecuteShareApplyServiceInactiveIsAccepted(t *testing.T) {
	h := newHarness(t)
	h.serviceState = ServiceStateInactive
	h.putManagedAccount(testAcctA, "alice", false)
	h.putManagedAccount(testAcctB, "bob", false)
	h.setfaclOK()
	h.getfaclReturns(wantedGetfacl)

	p := h.applyParams()
	a, err := h.engine.Assess(context.Background(), ActionShareApply, p)
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	report, err := h.engine.ExecuteWrite(context.Background(), WriteRequest{
		Action:               ActionShareApply,
		Params:               a.NormalizedParams,
		ExpectedIdentity:     a.IdentityFingerprint,
		ExpectedStateVersion: a.StateVersion,
	})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if report.Outcome != OutcomeAccepted {
		t.Fatalf("结果状态是 %q，期望 %q", report.Outcome, OutcomeAccepted)
	}
	if report.Observed["served"] != false {
		t.Errorf("served = %v，smbd 没在跑时必须为 false", report.Observed["served"])
	}
	if report.Observed["reload"] != ReloadSkippedInactive {
		t.Errorf("reload = %v，期望 %q", report.Observed["reload"], ReloadSkippedInactive)
	}
	if h.reloads != 0 {
		t.Errorf("smbd 没在跑却调用了 %d 次 smbcontrol", h.reloads)
	}
	// 配置本身仍然要写下去：服务没跑是运维状态，不是拒绝写配置的理由。
	if !strings.Contains(h.readManaged(), "[team-data]") {
		t.Error("配置没有写进托管文件")
	}
}

func TestExecuteShareApplyFailsOnACLReadbackMismatch(t *testing.T) {
	h := newHarness(t)
	h.putManagedAccount(testAcctA, "alice", false)
	h.putManagedAccount(testAcctB, "bob", false)
	h.setfaclOK()
	// 读回来 alice 是 rwx（写多了），bob 干脆不在。
	h.getfaclReturns("user::rwx\nuser:alice:rwx\ngroup::---\nmask::rwx\nother::---\n")

	p := h.applyParams()
	a, err := h.engine.Assess(context.Background(), ActionShareApply, p)
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	_, err = h.engine.ExecuteWrite(context.Background(), WriteRequest{
		Action: ActionShareApply, Params: a.NormalizedParams,
		ExpectedIdentity:     a.IdentityFingerprint,
		ExpectedStateVersion: a.StateVersion, OperationID: "op_x",
	})
	if err == nil {
		t.Fatal("ACL 读回不符却执行成功了：目录上的权限会被当成“已经按定义准备好”")
	}
	if !strings.Contains(err.Error(), "ACL") {
		t.Fatalf("错误没有提到 ACL：%v", err)
	}
	if h.store.saves != 0 {
		t.Fatal("核验失败却把记录写进了登记表")
	}
}

func TestExecuteShareApplyFailsOnMaskTooNarrow(t *testing.T) {
	h := newHarness(t)
	h.putManagedAccount(testAcctA, "alice", false)
	h.putManagedAccount(testAcctB, "bob", false)
	h.setfaclOK()
	// 条目都在、权限也对，但 mask 被压成 r-x：bob 的写权限实际不生效。
	h.getfaclReturns("user::rwx\nuser:alice:r-x\nuser:bob:rwx\ngroup::---\nmask::r-x\nother::---\n")

	p := h.applyParams()
	a, err := h.engine.Assess(context.Background(), ActionShareApply, p)
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	_, err = h.engine.ExecuteWrite(context.Background(), WriteRequest{
		Action: ActionShareApply, Params: a.NormalizedParams,
		ExpectedIdentity:     a.IdentityFingerprint,
		ExpectedStateVersion: a.StateVersion, OperationID: "op_x",
	})
	if err == nil {
		t.Fatal("mask 覆盖不了写权限却执行成功了")
	}
	if !strings.Contains(err.Error(), "mask") {
		t.Fatalf("错误没有提到 mask：%v", err)
	}
}

func TestExecuteRejectsStaleIdentityAndVersion(t *testing.T) {
	h := newHarness(t)
	h.putManagedAccount(testAcctA, "alice", false)
	h.putManagedAccount(testAcctB, "bob", false)
	h.setfaclOK()
	h.getfaclReturns(wantedGetfacl)

	p := h.applyParams()
	a, err := h.engine.Assess(context.Background(), ActionShareApply, p)
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	// 换了盘：UUID 变了。
	vol := h.vols.vols[testVolID]
	vol.FilesystemUUID = "0a9e6b71-3c8d-4f52-b1a4-7d6e2c8f9b03"
	h.vols.vols[testVolID] = vol
	_, err = h.engine.ExecuteWrite(context.Background(), WriteRequest{
		Action: ActionShareApply, Params: a.NormalizedParams,
		ExpectedIdentity: testUUID, OperationID: "op_x",
	})
	if err == nil {
		t.Fatal("卷换了文件系统却执行成功了")
	}
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.Code != httpx.CodeDeviceIdentityChanged {
		t.Fatalf("错误是 %v，期望 409 DEVICE_IDENTITY_CHANGED", err)
	}
	if h.runner.count() == 0 {
		t.Fatal("应该先读能力再判决")
	}
	if h.store.saves != 0 {
		t.Fatal("身份不符却写了登记表")
	}
}

func TestExecuteShareDisableRevokesACL(t *testing.T) {
	h := newHarness(t)
	h.putManagedAccount(testAcctA, "alice", false)
	h.putManagedAccount(testAcctB, "bob", false)
	h.setfaclOK()
	h.getfaclReturns("user::rwx\ngroup::---\nother::---\n")
	def := baseDefinition()
	def.SMBUsers = []ShareUser{{AccountID: testAcctA, Access: AccessRead}}
	h.store.putShare(ShareRecord{
		ID: testShareI, Definition: def, ConfigHash: "h", Version: "4",
		DirectoryPrepared: true, CreatedAt: h.now, UpdatedAt: h.now,
	})
	// 目录已经存在（之前 apply 过）。
	h.fs.addDir(testDir)

	a, err := h.engine.Assess(context.Background(), ActionShareDisable, ShareRefParams{
		ShareID: testShareI, Version: "4",
	})
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	if !a.Allowed {
		t.Fatalf("应该允许，阻塞项：%v", a.Blockers)
	}
	report, err := h.engine.ExecuteWrite(context.Background(), WriteRequest{
		Action: ActionShareDisable, Params: a.NormalizedParams,
		ExpectedIdentity: a.IdentityFingerprint, ExpectedStateVersion: a.StateVersion,
		OperationID: "op_disable",
	})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if report.Observed["acl_revoked"] != true {
		t.Errorf("acl_revoked = %v", report.Observed["acl_revoked"])
	}
	setc, _ := h.runner.find("/usr/bin/setfacl")
	if spec := setc.Args[len(setc.Args)-2]; spec != "user::rwx,group::---,other::---" {
		t.Fatalf("撤销时发的 ACL 是 %q", spec)
	}
	rec, _, _ := h.store.GetShare(context.Background(), testShareI)
	if rec.Definition.Enabled {
		t.Error("禁用后 enabled 仍是 true")
	}
	if rec.DirectoryPrepared {
		t.Error("禁用后 directory_prepared 仍是 true：ACL 已经撤销了")
	}
	if rec.Version != "5" {
		t.Errorf("版本是 %q，期望 5", rec.Version)
	}
}

func TestExecuteShareDeleteKeepsDirectory(t *testing.T) {
	h := newHarness(t)
	h.setfaclOK()
	h.getfaclReturns("user::rwx\ngroup::---\nother::---\n")
	def := baseDefinition()
	def.SMBUsers = []ShareUser{{AccountID: testAcctA, Access: AccessRead}}
	h.store.putShare(ShareRecord{
		ID: testShareI, Definition: def, ConfigHash: "h", Version: "1",
		DirectoryPrepared: true, CreatedAt: h.now, UpdatedAt: h.now,
	})
	h.fs.addDir(testDir)

	a, err := h.engine.Assess(context.Background(), ActionShareDelete, ShareRefParams{
		ShareID: testShareI, Version: "1",
	})
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	report, err := h.engine.ExecuteWrite(context.Background(), WriteRequest{
		Action: ActionShareDelete, Params: a.NormalizedParams,
		ExpectedIdentity: a.IdentityFingerprint, ExpectedStateVersion: a.StateVersion,
		OperationID: "op_delete",
	})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if report.Observed["directory_kept"] != true {
		t.Errorf("directory_kept = %v", report.Observed["directory_kept"])
	}
	if _, found, _ := h.store.GetShare(context.Background(), testShareI); found {
		t.Fatal("共享记录没有被删除")
	}
	// 目录还在（共享的删除只移除导出配置）。
	if _, err := h.fs.lstat(testDir); err != nil {
		t.Fatal("共享目录被删掉了：删除共享不能删数据")
	}
}

// TestAssessCreateBindsStateVersion 新建共享与新建账号都必须绑定一个状态版本。
//
// 真机 P08.1 验收 §5 撞到的就是它：新建账号的判决 allowed=true，却没有状态版本；
// agent 的预检校验（validatePlanResult）要求 allowed 的计划绑定状态快照，于是判
// “预检结果不合法”，用户拿到 500 内部错误——“新建账号”这条路整条走不通。
// 新建共享是同一个漏洞，只是当时还没走到。
//
// 这里同时验证执行端能接住这份版本：把判决给出的版本原样交回去，guard 必须放行。
// 只断言“非空”是不够的——版本还得是执行端重算一遍能对上的那一串。
func TestAssessCreateBindsStateVersion(t *testing.T) {
	h := newHarness(t)
	h.putManagedAccount(testAcctA, "alice", false)
	h.putManagedAccount(testAcctB, "bob", false)
	h.setfaclOK()
	h.getfaclReturns(wantedGetfacl)
	h.sec.values[testSecret] = []byte("cw-S3cret-Pass")

	// 新建共享。
	shareApply, err := h.engine.Assess(context.Background(), ActionShareApply, h.applyParams())
	if err != nil {
		t.Fatalf("共享判决失败: %v", err)
	}
	if !shareApply.Allowed {
		t.Fatalf("新建共享应该允许，阻塞项：%v", shareApply.Blockers)
	}
	if shareApply.StateVersion != absentStateVersion(baseDefinition().Name) {
		t.Fatalf("新建共享的状态版本是 %q，期望 %q",
			shareApply.StateVersion, absentStateVersion(baseDefinition().Name))
	}
	if _, err := h.engine.ExecuteWrite(context.Background(), WriteRequest{
		Action: ActionShareApply, Params: shareApply.NormalizedParams,
		ExpectedIdentity:     shareApply.IdentityFingerprint,
		ExpectedStateVersion: shareApply.StateVersion, OperationID: "op_share_create",
	}); err != nil {
		t.Fatalf("拿着判决自己的状态版本执行却被拒（版本不可复现？）: %v", err)
	}

	// 新建账号。
	acctApply, err := h.engine.Assess(context.Background(), ActionAccountApply,
		AccountApplyParams{Name: "carol", SecretRef: testSecret})
	if err != nil {
		t.Fatalf("账号判决失败: %v", err)
	}
	if !acctApply.Allowed {
		t.Fatalf("新建账号应该允许，阻塞项：%v", acctApply.Blockers)
	}
	if acctApply.StateVersion != absentStateVersion("carol") {
		t.Fatalf("新建账号的状态版本是 %q，期望 %q",
			acctApply.StateVersion, absentStateVersion("carol"))
	}
	if _, err := h.engine.ExecuteWrite(context.Background(), WriteRequest{
		Action: ActionAccountApply, Params: acctApply.NormalizedParams,
		ExpectedIdentity:     acctApply.IdentityFingerprint,
		ExpectedStateVersion: acctApply.StateVersion, OperationID: "op_acct_create",
	}); err != nil {
		t.Fatalf("拿着判决自己的状态版本执行却被拒（版本不可复现？）: %v", err)
	}
}

// TestAssessAbsentVersionChangesOnceObjectExists 新建的版本在对象出现之后就过期。
//
// “absent:<名字>” 绑的是“此刻还没有它”。同名记录一旦出现，重新判决会给出阻塞项，
// 版本也变成别的东西——这正是执行端要拦的漂移。
func TestAssessAbsentVersionChangesOnceObjectExists(t *testing.T) {
	h := newHarness(t)
	h.putManagedAccount(testAcctA, "alice", false)
	h.putManagedAccount(testAcctB, "bob", false)
	h.sec.values[testSecret] = []byte("cw-S3cret-Pass")

	before, err := h.engine.Assess(context.Background(), ActionAccountApply,
		AccountApplyParams{Name: "dave", SecretRef: testSecret})
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	if !before.Allowed {
		t.Fatalf("dave 还不存在，应该允许，阻塞项：%v", before.Blockers)
	}

	// 有人抢在前面建了同名受管理账号。
	h.putManagedAccount("sac_dddddddddddddddddddddddddddddddd", "dave", false)

	after, err := h.engine.Assess(context.Background(), ActionAccountApply,
		AccountApplyParams{Name: "dave", SecretRef: testSecret})
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	if after.Allowed || findBlocker(after.Blockers, NoticeAccountNameTaken) == nil {
		t.Fatalf("同名受管理账号已存在却没有 %s：%v", NoticeAccountNameTaken, after.Blockers)
	}
	if after.StateVersion == before.StateVersion {
		t.Fatalf("对象已经出现，状态版本还是 %q：预检与执行之间没有可核对的差异",
			after.StateVersion)
	}
}

func TestExecuteAccountCreateVerifiesAndPersists(t *testing.T) {
	// 假系统里还没有 alice：预检读到的是“没有”，执行器建完之后读到的才是“有”。
	h := newHarness(t)
	h.sec.values[testSecret] = []byte("cw-S3cret-Pass")

	p := AccountApplyParams{Name: "alice", SecretRef: testSecret}
	a, err := h.engine.Assess(context.Background(), ActionAccountApply, p)
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	if !a.Allowed {
		t.Fatalf("应该允许，阻塞项：%v", a.Blockers)
	}
	report, err := h.engine.ExecuteWrite(context.Background(), WriteRequest{
		Action: ActionAccountApply, Params: a.NormalizedParams,
		ExpectedIdentity:     a.IdentityFingerprint,
		ExpectedStateVersion: a.StateVersion, OperationID: "op_acct",
	})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if report.Outcome != OutcomeVerified {
		t.Fatalf("结果状态是 %q，期望 verified（每一项都读回核验过）", report.Outcome)
	}
	// 密码只经标准输入，永远不进命令行。
	for _, c := range h.runner.calls {
		for _, arg := range c.Args {
			if strings.Contains(arg, "cw-S3cret-Pass") {
				t.Fatalf("密码出现在 %s 的命令行里：%q", c.Program, arg)
			}
		}
	}
	smb, found := h.runner.find("/usr/bin/smbpasswd")
	if !found || smb.Stdin != "cw-S3cret-Pass\ncw-S3cret-Pass\n" {
		t.Fatalf("smbpasswd 的标准输入是 %q", smb.Stdin)
	}
	// useradd 必须建的是系统账号：不给 home、不给 shell。
	ua, _ := h.runner.find("/usr/sbin/useradd")
	for _, want := range []string{"--system", "--no-create-home", DefaultHome, DefaultShell} {
		if !hasArg(ua.Args, want) {
			t.Errorf("useradd 参数里没有 %q：%v", want, ua.Args)
		}
	}
	// 登记表：受管理、有密码、uid 来自读回。
	recs, _ := h.store.ListAccounts(context.Background())
	if len(recs) != 1 {
		t.Fatalf("登记表里有 %d 条账号", len(recs))
	}
	rec := recs[0]
	if !rec.Managed || !rec.HasPassword || rec.Disabled {
		t.Fatalf("账号记录不对：%+v", rec)
	}
	if rec.UID != 999 || rec.Shell != DefaultShell {
		t.Fatalf("读回的 uid/shell 没存进记录：%+v", rec)
	}
	// 秘密必须被引用成持久秘密，否则 10 分钟后它就没了。
	wantRef := "share_account:" + rec.ID
	if h.sec.references[testSecret] != wantRef {
		t.Fatalf("秘密引用是 %q，期望 %q", h.sec.references[testSecret], wantRef)
	}
}

func TestExecuteAccountCreateRollsBackWhenPasswordMissing(t *testing.T) {
	h := newHarness(t)
	h.sec.values[testSecret] = []byte("cw-S3cret-Pass")
	// passdb 里的 Password last set 一直是空的：执行器必须回滚，
	// 而不是留下一个“Linux 账号建好了、Samba 里却设不上密码”的半成品。
	h.sys.noPass = true

	p := AccountApplyParams{Name: "alice", SecretRef: testSecret}
	a, err := h.engine.Assess(context.Background(), ActionAccountApply, p)
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	_, err = h.engine.ExecuteWrite(context.Background(), WriteRequest{
		Action: ActionAccountApply, Params: a.NormalizedParams,
		ExpectedIdentity:     a.IdentityFingerprint,
		ExpectedStateVersion: a.StateVersion, OperationID: "op_acct",
	})
	if err == nil {
		t.Fatal("passdb 里没有密码却执行成功了")
	}
	if !strings.Contains(err.Error(), "回滚") {
		t.Fatalf("错误里没说清已经回滚：%v", err)
	}
	if _, found := h.runner.find("/usr/sbin/userdel"); !found {
		t.Fatal("没有回滚：刚创建的系统账号还在")
	}
	if h.store.saves != 0 {
		t.Fatal("失败却写了登记表")
	}
}

func TestExecuteAccountDeleteRemovesBothSides(t *testing.T) {
	h := newHarness(t)
	h.putManagedAccount(testAcctA, "alice", false)
	// 系统与 passdb 两处都有这个账号；删完之后两处都读不到。
	h.sys.seed("alice", 999, false)

	a, err := h.engine.Assess(context.Background(), ActionAccountDelete, AccountRefParams{AccountID: testAcctA})
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	if !a.Allowed {
		t.Fatalf("应该允许，阻塞项：%v", a.Blockers)
	}
	report, err := h.engine.ExecuteWrite(context.Background(), WriteRequest{
		Action: ActionAccountDelete, Params: a.NormalizedParams,
		ExpectedIdentity: a.IdentityFingerprint, ExpectedStateVersion: a.StateVersion,
		OperationID: "op_del",
	})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if report.Outcome != OutcomeVerified {
		t.Fatalf("结果状态是 %q", report.Outcome)
	}
	if _, found, _ := h.store.GetAccount(context.Background(), testAcctA); found {
		t.Fatal("账号记录没有删除")
	}
	// 顺序：先删 Samba 账号，再删系统账号。
	smbIdx, userIdx := -1, -1
	for i, c := range h.runner.calls {
		if c.Program == "/usr/bin/smbpasswd" {
			smbIdx = i
		}
		if c.Program == "/usr/sbin/userdel" {
			userIdx = i
		}
	}
	if smbIdx < 0 || userIdx < 0 || smbIdx > userIdx {
		t.Fatalf("删除顺序不对：smbpasswd=%d userdel=%d", smbIdx, userIdx)
	}
}

// ---------- 小工具 ----------

func strPtr(s string) *string { return &s }

func hasBlockerCode(bs []Blocker, code string) bool { return findBlocker(bs, code) != nil }

func findBlocker(bs []Blocker, code string) *Blocker {
	for i := range bs {
		if bs[i].Code == code {
			return &bs[i]
		}
	}
	return nil
}

// ---------- 配置写入的失败路径（P08.2） ----------

// applyOnce 走一遍完整链路：预检 → 执行。失败时返回错误。
func (h *harness) applyOnce(t *testing.T) (WriteReport, error) {
	t.Helper()
	p := h.applyParams()
	a, err := h.engine.Assess(context.Background(), ActionShareApply, p)
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	if !a.Allowed {
		return WriteReport{}, blockerError(a)
	}
	return h.engine.ExecuteWrite(context.Background(), WriteRequest{
		Action:               ActionShareApply,
		Params:               a.NormalizedParams,
		ExpectedIdentity:     a.IdentityFingerprint,
		ExpectedStateVersion: a.StateVersion,
	})
}

// ---------- 上级目录的穿越权限（Q233，真机抓出来的缺陷） ----------

// 缺的上级目录要按 0711 建：能穿过去，看不到里面有什么。
func TestExecuteShareApplyCreatesAncestorsTraversable(t *testing.T) {
	h := newHarness(t)
	h.putManagedAccount(testAcctA, "alice", false)
	h.putManagedAccount(testAcctB, "bob", false)
	h.setfaclOK()
	h.getfaclReturns(wantedGetfacl)

	// 三级相对路径：projects 已有，a 与 a/b 都得现建。
	def := baseDefinition()
	def.RelativePath = "projects/a/b/team"
	def.SMBUsers = []ShareUser{
		{AccountID: testAcctA, Access: AccessRead},
		{AccountID: testAcctB, Access: AccessWrite},
	}
	dir := testMount + "/projects/a/b/team"
	p := ShareApplyParams{Definition: &def}
	a, err := h.engine.Assess(context.Background(), ActionShareApply, p)
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	if !a.Allowed {
		t.Fatalf("预检不允许：%+v", a.Blockers)
	}
	if _, err := h.engine.ExecuteWrite(context.Background(), WriteRequest{
		Action:               ActionShareApply,
		Params:               a.NormalizedParams,
		ExpectedIdentity:     a.IdentityFingerprint,
		ExpectedStateVersion: a.StateVersion,
	}); err != nil {
		t.Fatalf("执行失败: %v", err)
	}

	for _, want := range []struct {
		path string
		perm os.FileMode
	}{
		{testMount + "/projects/a", 0o711},
		{testMount + "/projects/a/b", 0o711},
		{dir, 0o700},
	} {
		got, ok := h.fs.madePerm[want.path]
		if !ok {
			t.Fatalf("%s 没有被创建，创建了 %v", want.path, h.fs.made)
		}
		if got != want.perm {
			t.Errorf("%s 的权限是 %04o，期望 %04o", want.path, got, want.perm)
		}
	}
	if len(h.fs.chmods) != 0 {
		t.Errorf("新目录不该走到 chmod，实际改了：%v", h.fs.chmods)
	}
}

// 上一版建出来的 0700 上级要能被修好，并且修了什么要说出来。
func TestExecuteShareApplyRepairsTightAncestor(t *testing.T) {
	h := newHarness(t)
	h.putManagedAccount(testAcctA, "alice", false)
	h.putManagedAccount(testAcctB, "bob", false)
	h.setfaclOK()
	h.getfaclReturns(wantedGetfacl)
	// 真机上的实际形状：share 路径是 mounted_vol/team/docs，team 由上一版建成了 0700。
	parentDir := testMount + "/projects"
	h.fs.addDirMode(parentDir, 0o700)

	report, err := h.applyOnce(t)
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if len(h.fs.chmods) != 1 || !strings.HasPrefix(h.fs.chmods[0], parentDir+" ") {
		t.Fatalf("chmod 调用是 %v，期望只修 %s", h.fs.chmods, parentDir)
	}
	if got := h.fs.files[parentDir].perm; got != 0o711 {
		t.Errorf("上级目录修完之后是 %04o，期望 0711", got)
	}
	// 只加 x，其余位原样：0700 → 0711 而不是 0755。
	if got := h.fs.chmods[0]; !strings.Contains(got, "-rwx--x--x") {
		t.Errorf("chmod 用的是 %q，期望 -rwx--x--x（只补 other 的 x）", got)
	}
	repaired, ok := report.Observed["path_traversal_repaired"].([]string)
	if !ok || len(repaired) != 1 || repaired[0] != parentDir {
		t.Errorf("observed.path_traversal_repaired = %v，期望 [%s]",
			report.Observed["path_traversal_repaired"], parentDir)
	}
}

// 预检要把“这一级挡人”说出来：不阻塞（面板自己会修），但用户得知道。
func TestAssessShareApplyWarnsNotTraversableAncestor(t *testing.T) {
	h := newHarness(t)
	h.putManagedAccount(testAcctA, "alice", false)
	h.putManagedAccount(testAcctB, "bob", false)
	parentDir := testMount + "/projects"
	h.fs.addDirMode(parentDir, 0o700)

	a, err := h.engine.Assess(context.Background(), ActionShareApply, h.applyParams())
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	if !a.Allowed {
		t.Fatalf("上级不可穿越不该阻塞，实际阻塞项 %+v", a.Blockers)
	}
	b := findBlocker(a.Warnings, NoticeSharePathNotTraversable)
	if b == nil {
		t.Fatalf("没有 %s 告警：%+v", NoticeSharePathNotTraversable, a.Warnings)
	}
	if !strings.Contains(b.Message, parentDir) {
		t.Errorf("告警没有点名哪一级：%q", b.Message)
	}
}

// 允许网段挡不住本机：Samba 的 hosts allow 永远放行 127.0.0.1（man 5 smb.conf）。
// 这条是告警，不阻塞——但必须说出来，否则界面会让人以为“只允许 10.0.0.0/24”连本机也挡住了。
func TestAssessShareApplyWarnsHostsAllowLoopback(t *testing.T) {
	h := newHarness(t)
	h.putManagedAccount(testAcctA, "alice", false)
	h.putManagedAccount(testAcctB, "bob", false)

	assess := func(cidrs []string) Assessment {
		t.Helper()
		def := baseDefinition()
		def.AllowedCIDRs = cidrs
		a, err := h.engine.Assess(context.Background(), ActionShareApply, ShareApplyParams{Definition: &def})
		if err != nil {
			t.Fatalf("判决失败: %v", err)
		}
		return a
	}

	a := assess([]string{"10.0.0.0/24"})
	if !a.Allowed {
		t.Fatalf("名单不覆盖环回不该阻塞：%+v", a.Blockers)
	}
	b := findBlocker(a.Warnings, NoticeHostsAllowLoopback)
	if b == nil {
		t.Fatalf("没有 %s 告警：%+v", NoticeHostsAllowLoopback, a.Warnings)
	}
	if !strings.Contains(b.Message, "127.0.0.1") {
		t.Errorf("告警没说清是哪个地址被放行：%q", b.Message)
	}

	// 名单覆盖环回（或干脆不限）时不该有这条告警：说多了等于没说。
	for _, cidrs := range [][]string{{"127.0.0.0/8"}, {"0.0.0.0/0"}, nil} {
		if got := findBlocker(assess(cidrs).Warnings, NoticeHostsAllowLoopback); got != nil {
			t.Errorf("allowed_cidrs=%v 时不该有 %s：%q", cidrs, NoticeHostsAllowLoopback, got.Message)
		}
	}
}

// 候选配置不合格时：正式文件一个字节都不能动。
func TestExecuteShareApplyRejectsBadCandidate(t *testing.T) {
	h := newHarness(t)
	h.putManagedAccount(testAcctA, "alice", false)
	h.putManagedAccount(testAcctB, "bob", false)
	h.setfaclOK()
	h.getfaclReturns(wantedGetfacl)
	h.rejectCandidate = true

	before := h.readMain()
	if _, err := h.applyOnce(t); err == nil {
		t.Fatal("候选不合格却执行成功了")
	} else if !strings.Contains(err.Error(), FailConfigValidation) {
		t.Fatalf("错误没有提到校验失败：%v", err)
	}
	if h.readMain() != before {
		t.Error("主配置被改动了：校验失败时不应该碰它")
	}
	if h.readManaged() != "" {
		t.Error("托管文件被写下了：候选不合格时不该落盘")
	}
	if h.reloads != 0 {
		t.Errorf("调用了 %d 次 smbcontrol：校验没过就不该重载", h.reloads)
	}
	if recs, _ := h.store.ListShares(context.Background()); len(recs) != 0 {
		t.Errorf("登记表里有 %d 条记录：失败的动作不该落库", len(recs))
	}
}

// 合并后不合格时回滚：两份文件都回到写之前的样子。
func TestExecuteShareApplyRollsBackOnMergedFailure(t *testing.T) {
	h := newHarness(t)
	h.putManagedAccount(testAcctA, "alice", false)
	h.putManagedAccount(testAcctB, "bob", false)
	h.setfaclOK()
	h.getfaclReturns(wantedGetfacl)
	mainBefore := h.readMain()
	h.testparmBroken = true

	_, err := h.applyOnce(t)
	if err == nil {
		t.Fatal("合并后不合格却执行成功了")
	}
	if !strings.Contains(err.Error(), "已回滚") {
		t.Fatalf("错误里没有回滚说明：%v", err)
	}
	if h.readMain() != mainBefore {
		t.Errorf("主配置没有回到写之前：\n%s", h.readMain())
	}
	// 托管文件原本不存在：回滚之后也不该存在。
	if h.readManaged() != "" {
		t.Errorf("托管文件没有被撤回：\n%s", h.readManaged())
	}
	if h.reloads != 0 {
		t.Errorf("调用了 %d 次 smbcontrol", h.reloads)
	}
}

// 重写已有托管文件时先备份，备份内容是上一份。
func TestExecuteShareApplyBacksUpBeforeRewrite(t *testing.T) {
	h := newHarness(t)
	h.putManagedAccount(testAcctA, "alice", false)
	h.putManagedAccount(testAcctB, "bob", false)
	h.setfaclOK()
	h.getfaclReturns(wantedGetfacl)
	if _, err := h.applyOnce(t); err != nil {
		t.Fatalf("第一次执行失败: %v", err)
	}
	first := h.readManaged()

	// 改一份定义（加一条来源网段：配置文本变了，ACL 不变），再应用一次
	// → 文件内容变了 → 必须先备份旧内容。
	recs, _ := h.store.ListShares(context.Background())
	rec := recs[0]
	version := rec.Version
	id := rec.ID
	def := baseDefinition()
	def.SMBUsers = []ShareUser{
		{AccountID: testAcctA, Access: AccessRead},
		{AccountID: testAcctB, Access: AccessWrite},
	}
	def.AllowedCIDRs = []string{"10.0.0.0/24"}
	p := ShareApplyParams{Definition: &def, ShareID: &id, Version: &version}
	a, err := h.engine.Assess(context.Background(), ActionShareApply, p)
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	report, err := h.engine.ExecuteWrite(context.Background(), WriteRequest{
		Action:               ActionShareApply,
		Params:               a.NormalizedParams,
		ExpectedIdentity:     a.IdentityFingerprint,
		ExpectedStateVersion: a.StateVersion,
	})
	if err != nil {
		t.Fatalf("第二次执行失败: %v", err)
	}
	backups, ok := report.Observed["config_backups"].([]string)
	if !ok || len(backups) == 0 {
		t.Fatalf("没有记录备份：%v", report.Observed["config_backups"])
	}
	entries, err := os.ReadDir(h.cfg.BackupDir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("备份目录里没有文件（err=%v）", err)
	}
	// 备份里必须真的能读到上一份托管配置。
	found := false
	for _, en := range entries {
		body, err := os.ReadFile(h.cfg.BackupDir + string(os.PathSeparator) + en.Name())
		if err != nil {
			continue
		}
		if string(body) == first {
			found = true
		}
	}
	if !found {
		t.Error("备份里没有上一份托管配置的内容：这样的备份在出问题时救不了人")
	}
	if h.readManaged() == first {
		t.Error("托管配置没有被更新成新内容")
	}
	if h.reloads != 2 {
		t.Errorf("重载了 %d 次，期望 2 次（内容变化了）", h.reloads)
	}
}

// 现有配置里已有同名段时，预检就要挡住：不许靠重名覆盖别人的共享。
func TestAssessRejectsNameTakenBySystemConfig(t *testing.T) {
	h := newHarness(t)
	h.putManagedAccount(testAcctA, "alice", false)
	h.putManagedAccount(testAcctB, "bob", false)
	// 主配置里已经有 [team-data]（不在面板登记表里）。
	if err := os.WriteFile(h.cfg.Main,
		[]byte("[global]\n   workgroup = WORKGROUP\n\n[team-data]\n   path = /srv/other\n"), 0o644); err != nil {
		t.Fatalf("写 smb.conf 失败: %v", err)
	}
	a, err := h.engine.Assess(context.Background(), ActionShareApply, h.applyParams())
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	if a.Allowed {
		t.Fatal("重名被放行了")
	}
	if !hasBlockerCode(a.Blockers, NoticeShareNameTakenBySystem) {
		t.Errorf("阻塞项里没有 %s：%v", NoticeShareNameTakenBySystem, a.Blockers)
	}
}

// 全局项会被改写时必须给出“现在 → 之后”的告警。
func TestAssessWarnsGlobalOverrides(t *testing.T) {
	h := newHarness(t)
	h.putManagedAccount(testAcctA, "alice", false)
	h.putManagedAccount("sac_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "bob", false)
	a, err := h.engine.Assess(context.Background(), ActionShareApply, h.applyParams())
	if err != nil {
		t.Fatalf("判决失败: %v", err)
	}
	var found string
	for _, w := range a.Warnings {
		if w.Code == NoticeConfigGlobalOverride {
			found = w.Message
		}
	}
	if found == "" {
		t.Fatalf("没有 %s 告警：%v", NoticeConfigGlobalOverride, a.Warnings)
	}
	// 默认值是 Bad User，托管文件会把它改成 never：这条差别必须出现在消息里。
	if !strings.Contains(found, "Bad User") || !strings.Contains(found, "never") {
		t.Errorf("告警没有说清现在是什么、会变成什么：%s", found)
	}
}
