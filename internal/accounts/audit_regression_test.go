package accounts

// 审计回归测试：每一处都对应一次"读代码读出来的、能复现的缺陷"。
//
// 与 engine_test.go 的分工：那边锁的是设计出来的不变量，这里锁的是修过的 bug——
// 任何一处回退，这里的某一条就会红。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// 修复一：createRegularAccount 在 Secrets 为 nil 时直接调 e.opts.Secrets.Resolve，
// 空指针崩在 useradd 之前。更新路径有这道门，新建路径漏了。
func TestCreateAccountWithoutSecretsChannelFailsCleanly(t *testing.T) {
	sys := newFakeSystem()
	store := newFakeStore()
	eng, err := New(Options{
		Store:  store,
		Runner: sys,
		// Secrets 故意不接。
		Now:   func() time.Time { return time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC) },
		Paths: Paths{SudoersDir: testSudoersDir, SudoersMain: "/etc/sudoers", HomeBase: "/home"},
		FS:    sys.fs(),
	})
	if err != nil {
		t.Fatalf("装配引擎失败：%v", err)
	}

	params := AccountApplyParams{Definition: &Definition{
		Name:              "alice",
		Type:              TypeRegular,
		PasswordSecretRef: testSecretRef,
	}}
	a := assess(t, eng, ActionAccountApply, params)
	_, err = execute(t, eng, ActionAccountApply, params, a)
	if err == nil {
		t.Fatal("秘密通道没接上时必须报错，而不是崩在 Resolve 上")
	}
	if !strings.Contains(err.Error(), "秘密通道不可用") {
		t.Fatalf("错误应当说明秘密通道不可用，收到：%v", err)
	}
	if specs := sys.specsFor(ProgramUseradd); len(specs) != 0 {
		t.Fatalf("没解析出秘密就不该调 useradd：%v", specs)
	}
	if _, exists := sys.users["alice"]; exists {
		t.Fatal("失败之后系统上不该留下账号")
	}
}

// 修复二：ssh_key.apply 的替换目标在**动笔之前**确认。旧代码先重写
// authorized_keys、再 GetSSHKey 发现目标不存在——文件已改、记录未改、没有回滚。
// 预检与执行之间记录被删掉时，这就是那条路径。
//
// hideKeyStore 让"GetSSHKey 找得到、ListSSHKeys 里没有"这种读己之写不一致
// 在假件里变得可表达：预检（走 GetSSHKey）放行，执行（走 ListSSHKeys）必须拒。
type hideKeyStore struct {
	*fakeStore
	ghost SSHKeyRecord
}

func (s *hideKeyStore) GetSSHKey(_ context.Context, accountID, keyID string) (SSHKeyRecord, bool, error) {
	if keyID == s.ghost.ID {
		return s.ghost, true, nil
	}
	return s.fakeStore.GetSSHKey(context.Background(), accountID, keyID)
}

func TestSSHKeyApplyReplacingGhostKeyLeavesFileUntouched(t *testing.T) {
	eng, sys, base, _ := newTestEngine(t)
	rec := seedAccount(t, sys, base, "alice", ProfileRestricted)
	keyPath := "/home/alice/.ssh/authorized_keys"

	ghostID := "ssk_0123456789abcdef0123456789abcdef"
	// 换一个走 GetSSHKey 永远说"在"的登记表。
	eng.opts.Store = &hideKeyStore{fakeStore: base, ghost: SSHKeyRecord{
		ID:        ghostID,
		AccountID: rec.ID,
		Type:      "ssh-ed25519",
		CreatedAt: time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC),
	}}

	params := SSHKeyApplyParams{AccountID: rec.ID, KeyID: &ghostID, PublicKey: ed25519KeyLine(7, "newbox")}
	a := assess(t, eng, ActionSSHKeyApply, params)
	_, err := execute(t, eng, ActionSSHKeyApply, params, a)
	if err == nil {
		t.Fatal("替换目标在登记列表里不存在时必须失败")
	}
	if !strings.Contains(err.Error(), "没有这把公钥") {
		t.Fatalf("错误应当是没找到公钥，收到：%v", err)
	}
	if _, exists := sys.files[keyPath]; exists {
		t.Fatalf("目标不存在时文件被改了：%q", sys.files[keyPath].data)
	}
	for _, name := range sys.fileNames() {
		if strings.HasSuffix(name, ".upanel.tmp") {
			t.Fatalf("残留了临时文件 %s", name)
		}
	}
}

// 修复三：account.delete 里 ReadFile(sudoers 文件) 的非"不存在"错误曾被当成
// "没有文件"吞掉——记录照删、文件照留，账号删掉之后 sudoers.d 里多一条指向
// 不存在账号的悬空规则。现在必须整个动作失败，且一个字都不动。
func TestAccountDeleteUnreadableSudoersKeepsEverything(t *testing.T) {
	eng, sys, store, _ := newTestEngine(t)
	rec := seedAccount(t, sys, store, "alice", ProfileRestricted)

	sudoParams := SudoRuleApplyParams{AccountID: rec.ID, TemplateID: "system_status"}
	sa := assess(t, eng, ActionSudoRuleApply, sudoParams)
	if _, err := execute(t, eng, ActionSudoRuleApply, sudoParams, sa); err != nil {
		t.Fatalf("夹具应用 sudo 规则失败：%v", err)
	}
	sudoersPath := SudoersPath(testSudoersDir, "alice")
	good, _ := sys.fileContent(t, sudoersPath)

	// 预检在健康现场上做；执行前磁盘开始读不出来（比如 EIO）。
	del := AccountRefParams{AccountID: rec.ID}
	a := assess(t, eng, ActionAccountDelete, del)

	fs := sys.fs()
	orig := fs.ReadFile
	fs.ReadFile = func(name string) ([]byte, error) {
		if name == sudoersPath {
			return nil, errors.New("假文件系统：这块盘读不出来")
		}
		return orig(name)
	}
	eng.opts.FS = fs

	_, err := execute(t, eng, ActionAccountDelete, del, a)
	if err == nil {
		t.Fatal("规则文件读不出来时删除必须失败，不能当成没有文件继续")
	}
	if specs := sys.specsFor(ProgramUserdel); len(specs) != 0 {
		t.Fatalf("规则没处理完就调了 userdel：%v", specs)
	}
	if _, found, _ := store.GetAccount(context.Background(), rec.ID); !found {
		t.Fatal("失败之后账号记录不该被删")
	}
	if _, hasRule, _ := store.GetSudoRule(context.Background(), rec.ID); !hasRule {
		t.Fatal("失败之后 sudo 规则记录不该被删")
	}
	after, _ := sys.fileContent(t, sudoersPath)
	if after != good {
		t.Fatalf("失败之后规则文件变了：\n旧：%q\n新：%q", good, after)
	}
	if _, exists := sys.users["alice"]; !exists {
		t.Fatal("失败之后系统账号不该被删")
	}
}

// 修复四：更新路径上 Secrets.Reference 的失败曾被 `_ =` 吞掉——口令已经生效、
// 临时秘密 10 分钟后过期，结果里却只字不提。新建路径报 secret_persisted，
// 更新路径也必须报。
type failRefSecrets struct {
	*fakeSecrets
}

func (s *failRefSecrets) Reference(context.Context, string, string) error {
	return errors.New("假秘密通道：引用写不进去")
}

func TestUpdateAccountReportsSecretReferenceFailure(t *testing.T) {
	eng, sys, store, secrets := newTestEngine(t)
	rec := seedAccount(t, sys, store, "alice", ProfileRestricted)
	secrets.values[testSecretRef] = []byte("kestrel-ocean-41")
	eng.opts.Secrets = &failRefSecrets{fakeSecrets: secrets}

	params := AccountApplyParams{
		AccountID: &rec.ID,
		Version:   &rec.Version,
		Definition: &Definition{
			Name:              "alice",
			Type:              TypeRegular,
			ShellProfile:      ProfileRestricted,
			PasswordSecretRef: testSecretRef,
		},
	}
	a := assess(t, eng, ActionAccountApply, params)
	report, err := execute(t, eng, ActionAccountApply, params, a)
	if err != nil {
		t.Fatalf("引用失败不该让动作失败（口令已生效）：%v", err)
	}
	if got, _ := report.Observed["password_changed"].(bool); !got {
		t.Fatal("结果里应当报告改了口令")
	}
	if got, ok := report.Observed["secret_persisted"].(bool); !ok || got {
		t.Fatalf("结果里必须报告秘密没挂上引用（secret_persisted=false），收到 %v", report.Observed["secret_persisted"])
	}
	if _, ok := report.Observed["secret_note"].(string); !ok {
		t.Fatal("结果里必须带 secret_note 说明引用失败的原因")
	}
}

// 修复五：restoreKeys 回滚时 Chown 失败曾被 `_ =` 吞掉——内容回了，属主是 root，
// sshd 的 StrictModes 会拒用这份 authorized_keys，回滚等于没回。现在必须把
// "内容已回滚、属主未恢复"写进错误，而不是让界面显示一次干净的回滚。
type failSaveKeyStore struct {
	*fakeStore
}

func (s *failSaveKeyStore) SaveSSHKey(context.Context, SSHKeyRecord) error {
	return errors.New("假登记表：写不进去")
}

func TestSSHKeyRollbackReportsChownFailure(t *testing.T) {
	eng, sys, store, _ := newTestEngine(t)
	rec := seedAccount(t, sys, store, "alice", ProfileRestricted)

	eng.opts.Store = &failSaveKeyStore{fakeStore: store}
	fs := sys.fs()
	fs.Chown = func(name string, uid, gid int) error {
		return errors.New("假文件系统：chown 被拒")
	}
	eng.opts.FS = fs

	params := SSHKeyApplyParams{AccountID: rec.ID, PublicKey: ed25519KeyLine(7, "newbox")}
	a := assess(t, eng, ActionSSHKeyApply, params)
	_, err := execute(t, eng, ActionSSHKeyApply, params, a)
	if err == nil {
		t.Fatal("落库失败必须报错")
	}
	if !strings.Contains(err.Error(), "属主") {
		t.Fatalf("回滚属主失败必须写进错误，收到：%v", err)
	}
	// 文件内容必须回滚到"不存在"（新建路径回滚就是删掉），只是属主没法保证。
	if _, exists := sys.files["/home/alice/.ssh/authorized_keys"]; exists {
		t.Fatal("落库失败后 authorized_keys 不该留下新内容")
	}
}
