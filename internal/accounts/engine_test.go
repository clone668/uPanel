package accounts

// 执行层的单元测试（P14.5）。
//
// 为什么这几条必须在单测里锁住，而不是只靠一次虚拟机验收：internal/accounts/types.go
// 的 Paths 注释把话说完了——「它存在只为单元测试：真机上 /etc/sudoers.d 是 root 的，
// 而'候选不合格时正式文件一个字节都没动'这条不变量必须在单元测试里能反复验证，
// 不能只靠一次虚拟机运行来担保。」VM 跑一轮是两台机器加几十分钟，而这里每一条
// 退化都是**静默**的：argv 里多一个口令、visudo 失败后旧规则没恢复、托管区外面的
// 行被吃掉、userdel 带上 -r —— 命令照样退出 0，页面照样绿灯。
//
// 假件全部刻意做成"看得见的笨"，而不是"被调用过就算通过"：
//
//	假 useradd   按真实工具的形状返回退出码（名字已被占用 = 9）
//	假 visudo    真的去解析候选内容，不是永远成功
//	假 sudo -l   真的从 sudoers.d 里的文件读出授权
//	假文件系统   内容、权限、属主都留档，rename 是原子的
//
// 于是断言的是行为（"文件里一个字节变了没有"），不是实现（"函数被调了几次"）。

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	"upanel/internal/shares"
	"upanel/internal/syscmd"
)

// ---------------------------------------------------------------------------
// 假系统：账号表 + 命令执行 + 文件系统，三者共享同一份现场
// ---------------------------------------------------------------------------

type fakeUser struct {
	uid   int
	gid   int
	home  string
	shell string
	lock  LockState
}

type fakeFile struct {
	data []byte
	mode os.FileMode
}

// recordedSpec 是交给"内核"的一份命令记录。
//
// stdin 存两份：copy 供断言内容，ref 供断言"用完清零"——引擎在 run 之后会把
// passwordStdin 返回的那个切片清零，ref 与它共享底层数组，所以执行完再读 ref
// 看到的应当是零。
type recordedSpec struct {
	program   string
	args      []string
	env       []string
	stdinCopy []byte
	stdinRef  []byte
	rollback  bool
}

type fakeSystem struct {
	users    map[string]fakeUser
	files    map[string]fakeFile
	dirs     map[string]os.FileMode
	chowns   map[string]string // path → "uid:gid"
	specs    []recordedSpec
	homeBase string

	// useraddExit 非零时模拟 useradd 失败（9 = 名字已被占用）。
	useraddExit int
	// failWrite 里的路径写入返回错误（验"写不进去时不动正式文件"）。
	failWrite map[string]bool
}

func newFakeSystem() *fakeSystem {
	sys := &fakeSystem{
		users:    map[string]fakeUser{},
		files:    map[string]fakeFile{},
		dirs:     map[string]os.FileMode{},
		chowns:   map[string]string{},
		homeBase: "/home",
	}
	// visudo 必须"存在"：判决层用 Stat(ProgramVisudo) 判断这台机器能不能写 sudoers。
	sys.files[ProgramVisudo] = fakeFile{mode: 0o755}
	return sys
}

// addUser 直接往系统里放一个账号（夹具用，不经过命令）。
func (f *fakeSystem) addUser(name string, uid int, shell string) fakeUser {
	u := fakeUser{uid: uid, gid: uid, home: path.Join(f.homeBase, name), shell: shell, lock: LockLocked}
	f.users[name] = u
	f.dirs[u.home] = 0o755
	return u
}

// Run 是 CommandRunner 的假实现。
func (f *fakeSystem) Run(ctx context.Context, spec syscmd.Spec, limits syscmd.Limits) (syscmd.Result, error) {
	rec := recordedSpec{
		program: spec.Program,
		args:    append([]string(nil), spec.Args...),
		env:     append([]string(nil), spec.Env...),
	}
	if spec.Stdin != nil {
		rec.stdinCopy = append([]byte(nil), spec.Stdin...)
		rec.stdinRef = spec.Stdin
	}
	f.specs = append(f.specs, rec)

	res := syscmd.Result{Program: spec.Program}
	exit := func(code int, stderr string) (syscmd.Result, error) {
		res.ExitCode = code
		res.Stderr = stderr
		return res, &syscmd.ExitError{Program: spec.Program, ExitCode: code}
	}

	switch spec.Program {
	case ProgramGetent:
		if len(spec.Args) != 2 || spec.Args[0] != "passwd" {
			return exit(1, "假 getent 只认 passwd <name>")
		}
		u, ok := f.users[spec.Args[1]]
		if !ok {
			// 真 getent 用 2 表示"这个键不在这个库里"。
			return exit(2, "")
		}
		res.Stdout = spec.Args[1] + ":x:" + itoa(u.uid) + ":" + itoa(u.gid) + ":,,,:" +
			u.home + ":" + u.shell + "\n"
		return res, nil

	case ProgramPasswd:
		if len(spec.Args) != 2 || spec.Args[0] != "-S" {
			return exit(1, "假 passwd 只认 -S <name>")
		}
		u, ok := f.users[spec.Args[1]]
		if !ok {
			return exit(1, "passwd: 用户不存在\n")
		}
		res.Stdout = spec.Args[1] + " " + string(u.lock) + " 2026-10-27 0 99999 7 -1\n"
		return res, nil

	case ProgramUseradd:
		if f.useraddExit != 0 {
			return exit(f.useraddExit, "useradd: 名字已被占用\n")
		}
		name, shell := "", ""
		for i := 0; i < len(spec.Args); i++ {
			if spec.Args[i] == "--shell" && i+1 < len(spec.Args) {
				shell = spec.Args[i+1]
				i++
				continue
			}
			if !strings.HasPrefix(spec.Args[i], "--") {
				name = spec.Args[i]
			}
		}
		if name == "" || shell == "" {
			return exit(1, "假 useradd 没能从 argv 里认出名字与壳")
		}
		if _, exists := f.users[name]; exists {
			return exit(9, "useradd: 名字已被占用\n")
		}
		uid := 1000 + len(f.users) + 1
		home := path.Join(f.homeBase, name)
		// 真的 useradd 不给 -p 时写进去的是 "!"，passwd -S 报 L。
		f.users[name] = fakeUser{uid: uid, gid: uid, home: home, shell: shell, lock: LockLocked}
		f.dirs[home] = 0o755
		return res, nil

	case ProgramUsermod:
		if len(spec.Args) != 3 || spec.Args[0] != "--shell" {
			return exit(1, "假 usermod 只认 --shell <壳> <名字>")
		}
		u, ok := f.users[spec.Args[2]]
		if !ok {
			return exit(6, "usermod: 用户不存在\n")
		}
		u.shell = spec.Args[1]
		f.users[spec.Args[2]] = u
		return res, nil

	case ProgramUserdel:
		if len(spec.Args) != 1 {
			return exit(1, "假 userdel 只认一个参数（加 -r 就是 bug）")
		}
		if _, ok := f.users[spec.Args[0]]; !ok {
			return exit(6, "userdel: 用户不存在\n")
		}
		// 故意**不删 home**：与真 userdel（不带 -r）一致，数据不归面板删。
		delete(f.users, spec.Args[0])
		return res, nil

	case ProgramChpasswd:
		if len(spec.Args) != 0 {
			return exit(1, "chpasswd 不该带参数")
		}
		for _, line := range strings.Split(strings.TrimRight(string(spec.Stdin), "\n"), "\n") {
			name, pass, ok := strings.Cut(line, ":")
			if !ok || name == "" || pass == "" {
				return exit(1, "chpasswd: 读不懂的标准输入\n")
			}
			u, exists := f.users[name]
			if !exists {
				return exit(1, "chpasswd: 用户不存在\n")
			}
			u.lock = LockUsable
			f.users[name] = u
		}
		return res, nil

	case ProgramVisudo:
		// visudo -c -f <候选>：只看候选自己。
		if len(spec.Args) == 3 && spec.Args[0] == "-c" && spec.Args[1] == "-f" {
			content, ok := f.files[spec.Args[2]]
			if !ok {
				return exit(1, "visudo: 打不开候选文件\n")
			}
			if !sudoersSane(content.data) {
				return exit(1, "/tmp/候选: 语法错误\n")
			}
			return res, nil
		}
		// visudo -c：整份配置，含 sudoers.d 里的每一个文件。
		if len(spec.Args) == 1 && spec.Args[0] == "-c" {
			for name, file := range f.files {
				if path.Dir(name) != "/etc/sudoers.d" || strings.HasPrefix(path.Base(name), ".") {
					continue // sudoers 忽略带点的文件名，与真工具一致
				}
				if !sudoersSane(file.data) {
					return exit(1, name+": 语法错误\n")
				}
			}
			return res, nil
		}
		return exit(1, "假 visudo 不认这组参数")

	case ProgramSudo:
		if len(spec.Args) != 3 || spec.Args[0] != "-l" || spec.Args[1] != "-U" {
			return exit(1, "假 sudo 只认 -l -U <名字>")
		}
		name := spec.Args[2]
		res.Stdout = "Matching Defaults entries for " + name + " on upanel-test:\n" +
			"    env_reset, mail_badpass\n\n" +
			"User " + name + " may run the following commands on upanel-test:\n"
		for _, file := range f.files {
			for _, line := range strings.Split(string(file.data), "\n") {
				prefix := name + " ALL = (root) NOPASSWD: "
				if strings.HasPrefix(line, prefix) {
					res.Stdout += "    (root) NOPASSWD: " + strings.TrimPrefix(line, prefix) + "\n"
				}
			}
		}
		return res, nil
	}
	return exit(1, "假执行器不认识 "+spec.Program)
}

// sudoersSane 只做语法层面的判断：非注释行必须是"账号 ALL = (root) NOPASSWD: /绝对路径"。
func sudoersSane(content []byte) bool {
	for _, raw := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.Index(line, " ALL = (root) NOPASSWD: /")
		if idx <= 0 {
			return false
		}
	}
	return true
}

// fs 返回接在同一个现场上的文件系统接缝。
func (f *fakeSystem) fs() FileFS {
	return FileFS{
		ReadFile: func(name string) ([]byte, error) {
			file, ok := f.files[name]
			if !ok {
				return nil, os.ErrNotExist
			}
			return append([]byte(nil), file.data...), nil
		},
		WriteFileSync: func(name string, data []byte, perm os.FileMode) error {
			if f.failWrite[name] {
				return errors.New("假文件系统：这块盘写不进去")
			}
			f.files[name] = fakeFile{data: append([]byte(nil), data...), mode: perm}
			return nil
		},
		Rename: func(oldpath, newpath string) error {
			file, ok := f.files[oldpath]
			if !ok {
				return os.ErrNotExist
			}
			delete(f.files, oldpath)
			f.files[newpath] = file
			return nil
		},
		FsyncDir: func(dir string) error { return nil },
		MkdirAll: func(dir string, perm os.FileMode) error {
			f.dirs[dir] = perm
			return nil
		},
		Remove: func(name string) error {
			if _, ok := f.files[name]; !ok {
				return os.ErrNotExist
			}
			delete(f.files, name)
			return nil
		},
		Stat: func(name string) (os.FileInfo, error) {
			if file, ok := f.files[name]; ok {
				return fakeFileInfo{name: path.Base(name), mode: file.mode, size: int64(len(file.data))}, nil
			}
			if mode, ok := f.dirs[name]; ok {
				return fakeFileInfo{name: path.Base(name), mode: mode | os.ModeDir}, nil
			}
			return nil, os.ErrNotExist
		},
		Chown: func(name string, uid, gid int) error {
			f.chowns[name] = itoa(uid) + ":" + itoa(gid)
			return nil
		},
	}
}

type fakeFileInfo struct {
	name string
	mode os.FileMode
	size int64
}

func (i fakeFileInfo) Name() string       { return i.name }
func (i fakeFileInfo) Size() int64        { return i.size }
func (i fakeFileInfo) Mode() os.FileMode  { return i.mode }
func (i fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (i fakeFileInfo) IsDir() bool        { return i.mode.IsDir() }
func (i fakeFileInfo) Sys() any           { return nil }

// specsFor 返回某个程序的所有执行记录。
func (f *fakeSystem) specsFor(program string) []recordedSpec {
	var out []recordedSpec
	for _, spec := range f.specs {
		if spec.program == program {
			out = append(out, spec)
		}
	}
	return out
}

// onlySpec 要求某个程序恰好被执行过一次，并返回那一次。
func (f *fakeSystem) onlySpec(t *testing.T, program string) recordedSpec {
	t.Helper()
	got := f.specsFor(program)
	if len(got) != 1 {
		t.Fatalf("%s 应当恰好执行一次，实际 %d 次（全部记录：%v）", program, len(got), f.allPrograms())
	}
	return got[0]
}

func (f *fakeSystem) allPrograms() []string {
	out := make([]string, 0, len(f.specs))
	for _, spec := range f.specs {
		out = append(out, spec.program+" "+strings.Join(spec.args, " "))
	}
	return out
}

// fileContent 返回某个文件的内容与权限。
func (f *fakeSystem) fileContent(t *testing.T, name string) (string, os.FileMode) {
	t.Helper()
	file, ok := f.files[name]
	if !ok {
		t.Fatalf("%s 不存在（现有文件：%v）", name, f.fileNames())
	}
	return string(file.data), file.mode
}

func (f *fakeSystem) fileNames() []string {
	out := make([]string, 0, len(f.files))
	for name := range f.files {
		out = append(out, name)
	}
	return out
}

// ---------------------------------------------------------------------------
// 假登记表与假秘密通道
// ---------------------------------------------------------------------------

type fakeStore struct {
	accounts map[string]shares.AccountRecord
	keys     map[string][]SSHKeyRecord
	rules    map[string]SudoRuleRecord
	shares   []shares.ShareRecord
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		accounts: map[string]shares.AccountRecord{},
		keys:     map[string][]SSHKeyRecord{},
		rules:    map[string]SudoRuleRecord{},
	}
}

func (s *fakeStore) ListAccounts(context.Context) ([]shares.AccountRecord, error) {
	out := make([]shares.AccountRecord, 0, len(s.accounts))
	for _, rec := range s.accounts {
		out = append(out, rec)
	}
	return out, nil
}

func (s *fakeStore) GetAccount(_ context.Context, id string) (shares.AccountRecord, bool, error) {
	rec, found := s.accounts[id]
	return rec, found, nil
}

func (s *fakeStore) GetAccountByName(_ context.Context, name string) (shares.AccountRecord, bool, error) {
	for _, rec := range s.accounts {
		if rec.Name == name {
			return rec, true, nil
		}
	}
	return shares.AccountRecord{}, false, nil
}

func (s *fakeStore) SaveAccount(_ context.Context, rec shares.AccountRecord) error {
	s.accounts[rec.ID] = rec
	return nil
}

func (s *fakeStore) DeleteAccount(_ context.Context, id string) error {
	delete(s.accounts, id)
	delete(s.keys, id)
	delete(s.rules, id)
	return nil
}

func (s *fakeStore) ListShares(context.Context) ([]shares.ShareRecord, error) {
	return s.shares, nil
}

func (s *fakeStore) ListSSHKeys(_ context.Context, accountID string) ([]SSHKeyRecord, error) {
	return append([]SSHKeyRecord(nil), s.keys[accountID]...), nil
}

func (s *fakeStore) GetSSHKey(_ context.Context, accountID, keyID string) (SSHKeyRecord, bool, error) {
	for _, key := range s.keys[accountID] {
		if key.ID == keyID {
			return key, true, nil
		}
	}
	return SSHKeyRecord{}, false, nil
}

func (s *fakeStore) SaveSSHKey(_ context.Context, rec SSHKeyRecord) error {
	list := s.keys[rec.AccountID]
	for i, key := range list {
		if key.ID == rec.ID {
			list[i] = rec
			s.keys[rec.AccountID] = list
			return nil
		}
	}
	s.keys[rec.AccountID] = append(list, rec)
	return nil
}

func (s *fakeStore) DeleteSSHKey(_ context.Context, accountID, keyID string) error {
	out := make([]SSHKeyRecord, 0, len(s.keys[accountID]))
	for _, key := range s.keys[accountID] {
		if key.ID == keyID {
			continue
		}
		out = append(out, key)
	}
	s.keys[accountID] = out
	return nil
}

func (s *fakeStore) GetSudoRule(_ context.Context, accountID string) (SudoRuleRecord, bool, error) {
	rec, found := s.rules[accountID]
	return rec, found, nil
}

func (s *fakeStore) SaveSudoRule(_ context.Context, rec SudoRuleRecord) error {
	s.rules[rec.AccountID] = rec
	return nil
}

func (s *fakeStore) DeleteSudoRule(_ context.Context, accountID string) error {
	delete(s.rules, accountID)
	return nil
}

type fakeSecrets struct {
	values map[string][]byte
	refs   map[string]string
}

func newFakeSecrets() *fakeSecrets {
	return &fakeSecrets{values: map[string][]byte{}, refs: map[string]string{}}
}

func (s *fakeSecrets) Resolve(_ context.Context, ref string) ([]byte, error) {
	value, ok := s.values[ref]
	if !ok {
		return nil, errors.New("秘密不存在或已过期")
	}
	return append([]byte(nil), value...), nil
}

func (s *fakeSecrets) Reference(_ context.Context, ref, reference string) error {
	s.refs[ref] = reference
	return nil
}

// ---------------------------------------------------------------------------
// 装配与夹具
// ---------------------------------------------------------------------------

const (
	testSudoersDir = "/etc/sudoers.d"
	// testSecretRef 是形状合法的秘密引用（sec_ + 32 位小写十六进制）。
	testSecretRef = "sec_0123456789abcdef0123456789abcdef"
	// testAccountID 是形状合法的账号标识（sac_ + 32 位小写十六进制）。
	testAccountID = "sac_0123456789abcdef0123456789abcdef"
)

func newTestEngine(t *testing.T) (*Engine, *fakeSystem, *fakeStore, *fakeSecrets) {
	t.Helper()
	sys := newFakeSystem()
	store := newFakeStore()
	secrets := newFakeSecrets()
	eng, err := New(Options{
		Store:   store,
		Secrets: secrets,
		Runner:  sys,
		Now:     func() time.Time { return time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC) },
		Paths:   Paths{SudoersDir: testSudoersDir, SudoersMain: "/etc/sudoers", HomeBase: "/home"},
		FS:      sys.fs(),
	})
	if err != nil {
		t.Fatalf("装配账号引擎失败：%v", err)
	}
	return eng, sys, store, secrets
}

// seedAccount 往登记表里放一条 regular 账号，并让系统上真的有它。
func seedAccount(t *testing.T, sys *fakeSystem, store *fakeStore, name string, profile ShellProfile) shares.AccountRecord {
	t.Helper()
	u := sys.addUser(name, 1001, profile.Shell())
	rec := shares.AccountRecord{
		ID:           testAccountID,
		Name:         name,
		DisplayName:  name,
		Type:         string(TypeRegular),
		Managed:      true,
		HasPassword:  true,
		UID:          u.uid,
		GID:          u.gid,
		Shell:        u.shell,
		Home:         u.home,
		ShellProfile: string(profile),
		Version:      "1",
		CreatedAt:    time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC),
		UpdatedAt:    time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC),
	}
	if err := store.SaveAccount(context.Background(), rec); err != nil {
		t.Fatalf("夹具写入账号失败：%v", err)
	}
	return rec
}

// assess 要求预检通过并返回判决（预检不通过时测试直接失败：这一轮测的是执行层）。
func assess(t *testing.T, eng *Engine, action string, params any) shares.Assessment {
	t.Helper()
	a, err := eng.Assess(context.Background(), action, params)
	if err != nil {
		t.Fatalf("%s 预检返回错误：%v", action, err)
	}
	if !a.Allowed {
		t.Fatalf("%s 预检应当允许，阻塞项：%+v", action, a.Blockers)
	}
	return a
}

// execute 带着预检拿到的身份与状态版本执行一次。
func execute(t *testing.T, eng *Engine, action string, params any, a shares.Assessment) (shares.WriteReport, error) {
	t.Helper()
	return eng.ExecuteWrite(context.Background(), shares.WriteRequest{
		Action:               action,
		Params:               params,
		ExpectedIdentity:     a.IdentityFingerprint,
		ExpectedStateVersion: a.StateVersion,
		OperationID:          "op-p145-test",
	})
}

// httpErr 把错误拆成状态码与消息（不是 *shares.HTTPError 时测试失败）。
func httpErr(t *testing.T, err error) *shares.HTTPError {
	t.Helper()
	var he *shares.HTTPError
	if !errors.As(err, &he) {
		t.Fatalf("期望一个带 HTTP 判决的错误，收到 %T：%v", err, err)
	}
	return he
}

// ed25519KeyLine 造一行形状合法的 ssh-ed25519 公钥（线格式：算法名 + 32 字节主体）。
func ed25519KeyLine(seed byte, comment string) string {
	blob := make([]byte, 0, 4+11+4+32)
	blob = appendUint32(blob, 11)
	blob = append(blob, "ssh-ed25519"...)
	blob = appendUint32(blob, 32)
	for i := 0; i < 32; i++ {
		blob = append(blob, seed+byte(i))
	}
	line := "ssh-ed25519 " + base64.StdEncoding.EncodeToString(blob)
	if comment != "" {
		line += " " + comment
	}
	return line
}

func appendUint32(buf []byte, n uint32) []byte {
	var tmp [4]byte
	binary.BigEndian.PutUint32(tmp[:], n)
	return append(buf, tmp[:]...)
}

func hasArg(spec recordedSpec, want string) bool {
	for _, arg := range spec.args {
		if arg == want {
			return true
		}
	}
	return false
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// 一、新建账号：口令只走标准输入，uid 由系统分配
// ---------------------------------------------------------------------------

func TestCreateRegularAccountKeepsPasswordOnStdin(t *testing.T) {
	eng, sys, store, secrets := newTestEngine(t)
	const password = "kestrel-ocean-41"
	secrets.values[testSecretRef] = []byte(password)

	params := AccountApplyParams{Definition: &Definition{
		Name:              "alice",
		DisplayName:       "Alice",
		Type:              TypeRegular,
		ShellProfile:      ProfileRestricted,
		PasswordSecretRef: testSecretRef,
	}}
	a := assess(t, eng, ActionAccountApply, params)
	if a.StateVersion != absentStateVersion("alice") {
		t.Fatalf("新建的绑定版本应当是 %q，收到 %q", absentStateVersion("alice"), a.StateVersion)
	}

	report, err := execute(t, eng, ActionAccountApply, params, a)
	if err != nil {
		t.Fatalf("执行失败：%v", err)
	}
	if report.Outcome != shares.OutcomeVerified {
		t.Fatalf("结果应当是 verified，收到 %q", report.Outcome)
	}

	// useradd 的 argv 逐项对照：形状变了就说明“能提交 uid”那条防线被动了。
	add := sys.onlySpec(t, ProgramUseradd)
	want := []string{"--create-home", "--shell", ShellRestricted, "--user-group", "alice"}
	if strings.Join(add.args, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("useradd 的 argv 是 %q，期望 %q", add.args, want)
	}
	if hasArg(add, "--uid") || hasArg(add, "-u") || hasArg(add, "--gid") || hasArg(add, "--groups") {
		t.Fatalf("useradd 的 argv 不该带 uid/gid/groups：%q", add.args)
	}

	// 口令只出现在 chpasswd 的标准输入里。
	pass := sys.onlySpec(t, ProgramChpasswd)
	if len(pass.args) != 0 {
		t.Fatalf("chpasswd 不该带参数（口令会进 /proc/<pid>/cmdline）：%q", pass.args)
	}
	if got, want := string(pass.stdinCopy), "alice:"+password+"\n"; got != want {
		t.Fatalf("chpasswd 的标准输入是 %q，期望 %q", got, want)
	}
	if !allZero(pass.stdinRef) {
		t.Fatalf("chpasswd 的标准输入用完之后没有清零：%q", pass.stdinRef)
	}

	// 全量扫描：口令不能出现在任何一次执行的 argv 或环境里。
	for _, spec := range sys.specs {
		for _, field := range append(append([]string{spec.program}, spec.args...), spec.env...) {
			if strings.Contains(field, password) {
				t.Fatalf("口令出现在了 argv/env 里：%s %q", spec.program, field)
			}
		}
	}

	// 读回核对之后落库的那一条。
	id, _ := report.Data["account_id"].(string)
	rec, found, err := store.GetAccount(context.Background(), id)
	if err != nil || !found {
		t.Fatalf("账号记录没落库（found=%v err=%v）", found, err)
	}
	if rec.UID != 1001 || rec.GID != 1001 {
		t.Fatalf("落库的 uid/gid 是 %d/%d，期望系统分配的 1001/1001", rec.UID, rec.GID)
	}
	if rec.Shell != ShellRestricted || rec.ShellProfile != string(ProfileRestricted) {
		t.Fatalf("落库的 shell 是 %q（档 %q），期望 %q", rec.Shell, rec.ShellProfile, ShellRestricted)
	}
	if rec.Home != "/home/alice" {
		t.Fatalf("落库的 home 是 %q", rec.Home)
	}
	if !rec.HasPassword || rec.SecretRef != testSecretRef {
		t.Fatalf("落库的 has_password=%v secret_ref=%q", rec.HasPassword, rec.SecretRef)
	}
	if rec.Type != string(TypeRegular) || !rec.Managed || rec.Disabled {
		t.Fatalf("落库的种类/受管/禁用是 %q/%v/%v", rec.Type, rec.Managed, rec.Disabled)
	}
	if rec.Version != "1" {
		t.Fatalf("新建的版本应当是 1，收到 %q", rec.Version)
	}
	if rec.LastOperationID != "op-p145-test" {
		t.Fatalf("落库的 last_operation_id 是 %q", rec.LastOperationID)
	}
	if got := report.Observed["password_state"]; got != string(LockUsable) {
		t.Fatalf("读回的口令状态是 %v，期望 P", got)
	}
	if got, _ := report.Observed["secret_persisted"].(bool); !got {
		t.Fatal("临时秘密应当转成持久的")
	}
	if got := secrets.refs[testSecretRef]; got != "account:"+id {
		t.Fatalf("秘密挂的引用是 %q，期望 %q", got, "account:"+id)
	}
	if sys.users["alice"].shell != ShellRestricted {
		t.Fatalf("系统上 alice 的壳是 %q", sys.users["alice"].shell)
	}
}

func TestCreateRegularAccountRefusesOccupiedName(t *testing.T) {
	eng, sys, store, _ := newTestEngine(t)
	// 预检问不出名字被占用（真机上那是"预检那次 getent 超时"的情形），
	// useradd 才是最后一道门：退出码 9 = 名字已被占用。
	sys.useraddExit = 9

	params := AccountApplyParams{Definition: &Definition{Name: "alice", Type: TypeRegular}}
	a := assess(t, eng, ActionAccountApply, params)

	_, err := execute(t, eng, ActionAccountApply, params, a)
	if err == nil {
		t.Fatal("名字已被占用时执行必须失败，不能静默接管")
	}
	he := httpErr(t, err)
	if he.Status != 409 {
		t.Fatalf("状态码是 %d，期望 409（前置条件不满足）", he.Status)
	}
	if !strings.Contains(he.Message, NoticeAccountNotManaged) {
		t.Fatalf("消息里应当带 %s：%s", NoticeAccountNotManaged, he.Message)
	}
	if specs := sys.specsFor(ProgramUserdel); len(specs) != 0 {
		t.Fatalf("没有建成账号就不该调 userdel：%v", specs)
	}
	if _, found, _ := store.GetAccountByName(context.Background(), "alice"); found {
		t.Fatal("失败之后登记表里不该留下记录")
	}
}

// ---------------------------------------------------------------------------
// 二、sudoers 门禁二失败：旧文件逐字节恢复
// ---------------------------------------------------------------------------

func TestSudoRuleApplyRestoresOldFileWhenWholeConfigFails(t *testing.T) {
	eng, sys, store, _ := newTestEngine(t)
	rec := seedAccount(t, sys, store, "alice", ProfileRestricted)
	sudoersPath := SudoersPath(testSudoersDir, "alice")

	// 第一步：正常写一条 system_status 规则。
	first := SudoRuleApplyParams{AccountID: rec.ID, TemplateID: "system_status"}
	a := assess(t, eng, ActionSudoRuleApply, first)
	if _, err := execute(t, eng, ActionSudoRuleApply, first, a); err != nil {
		t.Fatalf("第一次应用 sudo 规则失败：%v", err)
	}
	good, mode := sys.fileContent(t, sudoersPath)
	if mode != 0o440 {
		t.Fatalf("sudoers 文件权限是 %v，期望 0440", mode)
	}
	if !strings.Contains(good, claimMarker) {
		t.Fatalf("写出去的规则里没有认领标记：%q", good)
	}

	// 第二步：sudoers.d 里放一个别的东西写坏的文件。候选自己合格，整份配置不合格。
	sys.files[path.Join(testSudoersDir, "zz-broken")] = fakeFile{
		data: []byte("这不是 sudoers 语法\n"), mode: 0o440,
	}

	// 第三步：换模板。门禁一通过、rename 已经发生，门禁二必须让它退回去。
	second := SudoRuleApplyParams{AccountID: rec.ID, TemplateID: "journal_recent"}
	a2 := assess(t, eng, ActionSudoRuleApply, second)
	_, err := execute(t, eng, ActionSudoRuleApply, second, a2)
	if err == nil {
		t.Fatal("整份配置坏掉时写 sudoers 必须失败")
	}
	he := httpErr(t, err)
	if !strings.Contains(he.Message, NoticeSudoVisudoFailed) {
		t.Fatalf("消息里应当带 %s：%s", NoticeSudoVisudoFailed, he.Message)
	}

	// 旧规则逐字节相同——不是"内容看起来差不多"。
	after, _ := sys.fileContent(t, sudoersPath)
	if after != good {
		t.Fatalf("门禁二失败之后正式文件被改了：\n旧：%q\n新：%q", good, after)
	}
	// 临时文件不许残留（残留的 .tmp 会被下一次 visudo -c 读到）。
	for _, name := range sys.fileNames() {
		if path.Dir(name) == testSudoersDir && strings.HasPrefix(path.Base(name), ".") {
			t.Fatalf("sudoers.d 里残留了临时文件 %s", name)
		}
	}
	// 登记表里还是第一条规则：不能出现"文件是旧的、记录是新的"。
	rule, hasRule, err := store.GetSudoRule(context.Background(), rec.ID)
	if err != nil || !hasRule {
		t.Fatalf("sudo 规则记录丢了（hasRule=%v err=%v）", hasRule, err)
	}
	if rule.Template != "system_status" {
		t.Fatalf("登记表里的模板是 %q，期望仍然是 system_status", rule.Template)
	}
}

// ---------------------------------------------------------------------------
// 三、authorized_keys：托管区外面的行一个字节都不动
// ---------------------------------------------------------------------------

func TestSSHKeyApplyKeepsForeignLines(t *testing.T) {
	eng, sys, store, _ := newTestEngine(t)
	rec := seedAccount(t, sys, store, "alice", ProfileRestricted)
	keyPath := "/home/alice/.ssh/authorized_keys"

	// 人自己写的内容：一行注释、一把普通钥匙、一行带选项的钥匙。
	// 带选项的那行 ParsePublicKey 会拒绝，但面板不解析托管区之外的行——
	// 它正好证明"外面的字节原样保留"。
	foreign := "# 这行是我自己写的\n" +
		ed25519KeyLine(1, "laptop") + "\n" +
		"from=\"10.0.0.5\" " + ed25519KeyLine(2, "office") + "\n"
	sys.files[keyPath] = fakeFile{data: []byte(foreign), mode: 0o600}

	params := SSHKeyApplyParams{AccountID: rec.ID, PublicKey: ed25519KeyLine(7, "newbox")}
	a := assess(t, eng, ActionSSHKeyApply, params)
	report, err := execute(t, eng, ActionSSHKeyApply, params, a)
	if err != nil {
		t.Fatalf("写公钥失败：%v", err)
	}
	if report.Outcome != shares.OutcomeVerified {
		t.Fatalf("结果应当是 verified，收到 %q", report.Outcome)
	}

	after, mode := sys.fileContent(t, keyPath)
	if !strings.HasPrefix(after, foreign) {
		t.Fatalf("托管区之外的内容变了：\n之前：%q\n之后：%q", foreign, after)
	}
	if !strings.Contains(after, blockBeginLine) || !strings.Contains(after, blockEndLine) {
		t.Fatalf("没写出托管区标记：%q", after)
	}
	if !strings.Contains(after, ed25519KeyLine(7, "newbox")) {
		t.Fatalf("新钥匙没写进文件：%q", after)
	}
	if mode != 0o600 {
		t.Fatalf("authorized_keys 权限是 %v，期望 0600", mode)
	}
	// .ssh 与文件都必须是账号本人的（root 拥有的 .ssh 会被 StrictModes 拒绝）。
	owner := itoa(sys.users["alice"].uid) + ":" + itoa(sys.users["alice"].gid)
	for _, name := range []string{"/home/alice/.ssh", keyPath + ".upanel.tmp"} {
		got, touched := sys.chowns[name]
		if !touched {
			t.Fatalf("%s 的属主没有被设过（root 拥有的 .ssh 会让 sshd 拒绝使用里面的钥匙）", name)
		}
		if got != owner {
			t.Fatalf("%s 的属主被设成了 %s，期望 %s", name, got, owner)
		}
	}

	keyID, _ := report.Data["key_id"].(string)
	left := SSHKeyRefParams{AccountID: rec.ID, KeyID: keyID}
	a2 := assess(t, eng, ActionSSHKeyDelete, left)
	if _, err := execute(t, eng, ActionSSHKeyDelete, left, a2); err != nil {
		t.Fatalf("删公钥失败：%v", err)
	}
	// 一把钥匙都不剩：托管区连标记一起拆掉，人写的三行回到原样。
	finalContent, _ := sys.fileContent(t, keyPath)
	if finalContent != foreign {
		t.Fatalf("删光之后托管区之外的内容没有回到原样：\n期望：%q\n实际：%q", foreign, finalContent)
	}
}

// ---------------------------------------------------------------------------
// 四、删账号：不带 -r，连带撤掉 sudoers 里的规则
// ---------------------------------------------------------------------------

func TestAccountDeleteKeepsHomeAndDropsSudoers(t *testing.T) {
	eng, sys, store, _ := newTestEngine(t)
	rec := seedAccount(t, sys, store, "alice", ProfileRestricted)
	// home 里放一份数据，删账号之后它必须还在。
	sys.files["/home/alice/notes.txt"] = fakeFile{data: []byte("别删我\n"), mode: 0o600}

	// 先给一条 sudo 规则（走真实路径，这样文件与登记表都是自洽的）。
	sudoParams := SudoRuleApplyParams{AccountID: rec.ID, TemplateID: "system_status"}
	sa := assess(t, eng, ActionSudoRuleApply, sudoParams)
	if _, err := execute(t, eng, ActionSudoRuleApply, sudoParams, sa); err != nil {
		t.Fatalf("夹具应用 sudo 规则失败：%v", err)
	}
	sudoersPath := SudoersPath(testSudoersDir, "alice")

	del := AccountRefParams{AccountID: rec.ID}
	a := assess(t, eng, ActionAccountDelete, del)
	if a.StateVersion != "1" {
		t.Fatalf("删除要绑定账号的当前版本，收到 %q", a.StateVersion)
	}
	report, err := execute(t, eng, ActionAccountDelete, del, a)
	if err != nil {
		t.Fatalf("删账号失败：%v", err)
	}
	if report.Outcome != shares.OutcomeVerified {
		t.Fatalf("结果应当是 verified，收到 %q", report.Outcome)
	}

	del2 := sys.onlySpec(t, ProgramUserdel)
	if len(del2.args) != 1 || del2.args[0] != "alice" {
		t.Fatalf("userdel 的 argv 是 %q，期望只有账号名（加 -r 会删掉 home 里的数据）", del2.args)
	}
	if hasArg(del2, "-r") || hasArg(del2, "--remove") {
		t.Fatalf("userdel 带了 -r：%q", del2.args)
	}
	if _, exists := sys.files[sudoersPath]; exists {
		t.Fatalf("删账号之后 %s 还在", sudoersPath)
	}
	if _, hasRule, _ := store.GetSudoRule(context.Background(), rec.ID); hasRule {
		t.Fatal("删账号之后 sudo 规则记录还在")
	}
	if _, exists := sys.files["/home/alice/notes.txt"]; !exists {
		t.Fatal("home 里的数据被删了：面板不删数据")
	}
	if _, exists := sys.dirs["/home/alice"]; !exists {
		t.Fatal("home 目录被删了：面板不删数据")
	}
	if _, found, _ := store.GetAccount(context.Background(), rec.ID); found {
		t.Fatal("删账号之后登记表里还有记录")
	}
	if got, _ := report.Observed["home_data_kept"].(bool); !got {
		t.Fatal("结果里必须说清数据还在")
	}
	if got, _ := report.Observed["linux_removed"].(bool); !got {
		t.Fatal("系统账号应当被删掉了")
	}
	if got, _ := report.Observed["sudo_rule_removed"].(bool); !got {
		t.Fatal("sudo 规则文件应当被删掉了")
	}
}

// ---------------------------------------------------------------------------
// 五、禁用是换壳，不是 usermod -L
// ---------------------------------------------------------------------------

func TestDisableSwitchesShellInsteadOfLockingPassword(t *testing.T) {
	eng, sys, store, _ := newTestEngine(t)
	rec := seedAccount(t, sys, store, "alice", ProfileRestricted)

	params := AccountApplyParams{
		AccountID: &rec.ID,
		Version:   &rec.Version,
		Definition: &Definition{
			Name:         "alice",
			Type:         TypeRegular,
			ShellProfile: ProfileRestricted,
			Disabled:     true,
		},
	}
	a := assess(t, eng, ActionAccountApply, params)
	report, err := execute(t, eng, ActionAccountApply, params, a)
	if err != nil {
		t.Fatalf("禁用失败：%v", err)
	}

	um := sys.onlySpec(t, ProgramUsermod)
	want := []string{"--shell", ShellDisabled, "alice"}
	if strings.Join(um.args, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("usermod 的 argv 是 %q，期望 %q", um.args, want)
	}
	// -L 会锁口令，-U 会给从未设过口令的账号写一个空口令：两个都不许出现。
	if hasArg(um, "-L") || hasArg(um, "--lock") || hasArg(um, "-U") || hasArg(um, "--unlock") {
		t.Fatalf("禁用不该碰口令锁：%q", um.args)
	}
	if lock := sys.users["alice"].lock; lock != LockLocked {
		// 夹具里账号本来就是"锁着"的（useradd 不给 -p 的样子），这里只要确认它没被改。
		t.Fatalf("禁用之后系统上的口令状态变成了 %q", lock)
	}
	if sys.users["alice"].shell != ShellDisabled {
		t.Fatalf("系统上 alice 的壳是 %q，期望 %q", sys.users["alice"].shell, ShellDisabled)
	}
	got, found, err := store.GetAccount(context.Background(), rec.ID)
	if err != nil || !found {
		t.Fatalf("读不回账号记录（found=%v err=%v）", found, err)
	}
	if !got.Disabled || got.Shell != ShellDisabled {
		t.Fatalf("落库的 disabled/shell 是 %v/%q", got.Disabled, got.Shell)
	}
	// 期望档必须留着：否则"启用"不知道回哪一档。
	if got.ShellProfile != string(ProfileRestricted) {
		t.Fatalf("禁用之后 shell_profile 变成了 %q，启用就无从下手了", got.ShellProfile)
	}
	if got.Version != "2" {
		t.Fatalf("更新之后版本应当是 2，收到 %q", got.Version)
	}
	if v, _ := report.Observed["shell_changed"].(bool); !v {
		t.Fatal("结果里应当报告换了壳")
	}
	if v, _ := report.Observed["password_changed"].(bool); v {
		t.Fatal("禁用不该动口令")
	}
}

// ---------------------------------------------------------------------------
// 六、执行前复核：版本对不上就不动手
// ---------------------------------------------------------------------------

func TestExecuteRejectsStaleStateVersion(t *testing.T) {
	eng, sys, store, _ := newTestEngine(t)
	rec := seedAccount(t, sys, store, "alice", ProfileRestricted)

	params := AccountRefParams{AccountID: rec.ID}
	a := assess(t, eng, ActionAccountDelete, params)

	// 预检之后、执行之前，别处改了一次（版本推到了 2）。
	changed, _, _ := store.GetAccount(context.Background(), rec.ID)
	changed.Version = "2"
	if err := store.SaveAccount(context.Background(), changed); err != nil {
		t.Fatalf("夹具改版本失败：%v", err)
	}

	_, err := execute(t, eng, ActionAccountDelete, params, a)
	if err == nil {
		t.Fatal("状态版本过期时执行必须失败")
	}
	he := httpErr(t, err)
	if he.Status != 409 {
		t.Fatalf("状态码是 %d，期望 409", he.Status)
	}
	if specs := sys.specsFor(ProgramUserdel); len(specs) != 0 {
		t.Fatalf("复核没通过就调了 userdel：%v", specs)
	}
	if _, found, _ := store.GetAccount(context.Background(), rec.ID); !found {
		t.Fatal("复核没通过却把记录删了")
	}
}
