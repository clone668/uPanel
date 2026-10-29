package agent

// 账号查询与动作的接线测试（P14.5）。
//
// 只钉四件事：
//   1. 没有账号实现端时不登记——capabilities 里不该出现一个点了才 503 的入口；
//   2. 有实现端时两条查询（公钥、能力）与六个动作成对出现；
//   3. accounts.ssh_keys 的参数形状在登记层就被挡住，返回空列表是 [] 不是 null；
//   4. accounts.capability 返回的是**账号**自己的那份能力，不是从 share.accounts
//      借来的 Samba 能力——这两者的判据不相交（虚拟机验收抓到的缺陷）。
//
// useradd/chpasswd/visudo/sudo 的真实行为不在这里验证：那是独立 Ubuntu 虚拟机
// 上的验收（docs/evidence/P14）。

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"upanel/internal/accounts"
	"upanel/internal/shares"
)

// fakeAccounts 是 accountsSource 的测试替身。
type fakeAccounts struct {
	keys    []accounts.SSHKeyRecord
	keysErr error
	caps    accounts.Capability
	capsErr error

	keyAccountIDs []string
	capCalls      int
}

// Capability 返回假能力。判据里的工具路径由用例自己给，这里只负责“问了几次、
// 返回哪一份”。
func (f *fakeAccounts) Capability(context.Context) accounts.Capability {
	f.capCalls++
	return f.caps
}

func (f *fakeAccounts) Assess(context.Context, string, any) (shares.Assessment, error) {
	return shares.Assessment{Allowed: true}, nil
}

func (f *fakeAccounts) ExecuteWrite(context.Context, shares.WriteRequest) (shares.WriteReport, error) {
	return shares.WriteReport{Outcome: shares.OutcomeVerified}, nil
}

func (f *fakeAccounts) Accounts(context.Context) ([]shares.AccountRecord, error) { return nil, nil }

func (f *fakeAccounts) Account(_ context.Context, id string) (shares.AccountRecord, bool, error) {
	return shares.AccountRecord{ID: id}, true, nil
}

func (f *fakeAccounts) SSHKeys(_ context.Context, accountID string) ([]accounts.SSHKeyRecord, error) {
	f.keyAccountIDs = append(f.keyAccountIDs, accountID)
	if f.keysErr != nil {
		return nil, f.keysErr
	}
	return f.keys, nil
}

// TestAccountActionsNotRegisteredWithoutSource 没有账号实现端时不登记查询与动作。
func TestAccountActionsNotRegisteredWithoutSource(t *testing.T) {
	reg, err := NewProductionRegistry(ProductionDeps{})
	if err != nil {
		t.Fatalf("构造生产登记表失败: %v", err)
	}
	for _, name := range []string{
		accounts.QueryAccountsSSHKeys,
		accounts.QueryAccountsCapability,
	} {
		if _, ok := reg.Query(name); ok {
			t.Errorf("%s 在没有账号实现端时被登记了", name)
		}
	}
	for _, name := range []string{
		accounts.ActionAccountApply, accounts.ActionAccountDelete,
		accounts.ActionSSHKeyApply, accounts.ActionSSHKeyDelete,
		accounts.ActionSudoRuleApply, accounts.ActionSudoRuleDelete,
	} {
		if _, ok := reg.Action(name); ok {
			t.Errorf("%s 在没有账号实现端时被登记了", name)
		}
	}
}

// TestAccountQueriesAndActionsComeInPairs 有源时一条查询与六个动作成对出现，
// 而且账号动作与共享动作是两套（互不覆盖）。
func TestAccountQueriesAndActionsComeInPairs(t *testing.T) {
	src := &fakeAccounts{}
	reg, err := NewProductionRegistry(ProductionDeps{Accounts: src})
	if err != nil {
		t.Fatalf("构造生产登记表失败: %v", err)
	}
	for _, name := range []string{
		accounts.QueryAccountsSSHKeys,
		accounts.QueryAccountsCapability,
	} {
		if _, ok := reg.Query(name); !ok {
			t.Errorf("查询 %s 没登记", name)
		}
	}
	for _, name := range []string{
		accounts.ActionAccountApply, accounts.ActionAccountDelete,
		accounts.ActionSSHKeyApply, accounts.ActionSSHKeyDelete,
		accounts.ActionSudoRuleApply, accounts.ActionSudoRuleDelete,
	} {
		a, ok := reg.Action(name)
		if !ok {
			t.Errorf("动作 %s 没登记", name)
			continue
		}
		// 六个都是再认证动作（CONTRACTS.md 第 5 节 R=是）。
		if !a.Policy.RequiresReauth {
			t.Errorf("%s 没要求再认证", name)
		}
		if a.Policy.RequiresPhrase {
			t.Errorf("%s 不该要求确认短语：它没有不可逆到那个程度", name)
		}
		if a.Policy.Cancellable {
			t.Errorf("%s 不该可取消：账号写入要走到一个说得清的结果", name)
		}
	}
	// 共享动作在没有共享源时仍然不存在：两个开关不是同一个。
	if _, ok := reg.Action(shares.ActionAccountApply); ok {
		t.Error("share.account.apply 在没有共享源时被登记了")
	}
}

// TestSSHKeysQueryReturnsArray 一把公钥都没有时返回 []，不是 null。
func TestSSHKeysQueryReturnsArray(t *testing.T) {
	src := &fakeAccounts{}
	q := newSSHKeysQuery(src)
	params, err := q.Decode(json.RawMessage(`{"account_id":"` + testAccountID + `"}`))
	if err != nil {
		t.Fatalf("Decode 失败: %v", err)
	}
	out, err := q.Run(t.Context(), params)
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	var payload struct {
		AccountID string            `json:"account_id"`
		Keys      []json.RawMessage `json:"ssh_keys"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	if payload.AccountID != testAccountID {
		t.Errorf("account_id = %q", payload.AccountID)
	}
	if payload.Keys == nil {
		t.Errorf("ssh_keys = null，期望 []")
	}
	if len(src.keyAccountIDs) != 1 || src.keyAccountIDs[0] != testAccountID {
		t.Errorf("读取端收到的账号 = %v", src.keyAccountIDs)
	}
}

// TestSSHKeysQueryRejectsBadAccountID 形状不对的标识在登记层就被拒绝。
func TestSSHKeysQueryRejectsBadAccountID(t *testing.T) {
	q := newSSHKeysQuery(&fakeAccounts{})
	for _, raw := range []string{
		`{"account_id":""}`,
		`{"account_id":"alice"}`,
		`{"account_id":"SAC_FFEEDDCCBBAA99887766554433221100"}`,
		`{"account_id":"sac_short"}`,
		`{}`,
		`{"account_id":"` + testAccountID + `","command":"rm -rf /"}`,
	} {
		if _, err := q.Decode(json.RawMessage(raw)); !errors.Is(err, ErrParamsInvalid) {
			t.Errorf("Decode(%s) 错误 = %v，期望 ErrParamsInvalid", raw, err)
		}
	}
}

// TestAccountActionDecodeRejectsBadParams 六个动作的参数在登记层就被挡住。
func TestAccountActionDecodeRejectsBadParams(t *testing.T) {
	src := &fakeAccounts{}
	cases := []struct {
		action string
		raw    string
	}{
		// definition 缺失 / 没有 type。
		{accounts.ActionAccountApply, `{"version":"1"}`},
		{accounts.ActionAccountApply, `{"definition":{}}`},
		// 账号标识形状不对。
		{accounts.ActionAccountApply, `{"account_id":"alice","definition":{"type":"regular"}}`},
		{accounts.ActionAccountDelete, `{"account_id":"alice","version":"1"}`},
		{accounts.ActionAccountDelete, `{"account_id":"` + testAccountID + `"}`}, // 缺 version
		// 公钥：缺 public_key。
		{accounts.ActionSSHKeyApply, `{"account_id":"` + testAccountID + `","version":"1"}`},
		// 公钥标识形状不对。
		{accounts.ActionSSHKeyDelete, `{"account_id":"` + testAccountID + `","version":"1","key_id":"nope"}`},
		// sudo 模板必须在闭集里。
		{accounts.ActionSudoRuleApply, `{"account_id":"` + testAccountID + `","version":"1","template_id":"root_all"}`},
		{accounts.ActionSudoRuleDelete, `{"account_id":"` + testAccountID + `"}`}, // 缺 version
		{accounts.ActionSudoRuleDelete, `{"account_id":"` + testAccountID + `","version":"1.5"}`},
	}
	for _, tc := range cases {
		a, ok := registrationFor(src, tc.action)
		if !ok {
			t.Errorf("%s 没登记", tc.action)
			continue
		}
		if _, err := a.Decode(json.RawMessage(tc.raw)); !errors.Is(err, ErrParamsInvalid) {
			t.Errorf("%s.Decode(%s) 错误 = %v，期望 ErrParamsInvalid", tc.action, tc.raw, err)
		}
	}
}

// registrationFor 从一张只接了账号源的生产登记表里取出一个动作。
func registrationFor(src accountsSource, name string) (Action, bool) {
	reg, err := NewProductionRegistry(ProductionDeps{Accounts: src})
	if err != nil {
		return Action{}, false
	}
	return reg.Action(name)
}

// TestAccountsCapabilityQueryReturnsAccountTools accounts.capability 返回的是账号
// 自己那份能力，而且原样带出工具与壳档。
//
// 这一条锁的是**来源**：GET /accounts 曾经把 share.accounts 带回来的 Samba 能力
// 当成账号能力返回，于是一台没装 Samba 的机器上账号页显示“不支持账号管理”，
// 新建按钮是灰的——而建普通账号一个 Samba 工具都不需要。用例里给的能力报告带
// 的是 useradd/getent 这类账号工具，返回体里就必须是它们，不是 smbd。
func TestAccountsCapabilityQueryReturnsAccountTools(t *testing.T) {
	src := &fakeAccounts{caps: accounts.Capability{
		Status: accounts.CapabilityAvailable,
		Tools: accounts.AccountTools{
			Useradd:         "/usr/sbin/useradd",
			Getent:          "/usr/bin/getent",
			Chpasswd:        "/usr/sbin/chpasswd",
			RestrictedShell: "/bin/rbash",
		},
		SudoRuleWrite: "available",
		ShellProfiles: []string{"disabled", "restricted"},
	}}
	q := newAccountsCapabilityQuery(src)
	params, err := q.Decode(json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Decode 失败: %v", err)
	}
	out, err := q.Run(t.Context(), params)
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	var payload struct {
		Capability accounts.Capability `json:"capability"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	if payload.Capability.Status != accounts.CapabilityAvailable {
		t.Errorf("status = %q", payload.Capability.Status)
	}
	if payload.Capability.Tools.Useradd != "/usr/sbin/useradd" {
		t.Errorf("tools.useradd = %q", payload.Capability.Tools.Useradd)
	}
	if len(payload.Capability.ShellProfiles) != 2 {
		t.Errorf("shell_profiles = %v", payload.Capability.ShellProfiles)
	}
	// 能力这条查询不接受任何参数：给了未知字段就该在登记层被拒。
	for _, bad := range []string{`{"paths":{"useradd":"/tmp/evil"}}`, `{"account_id":"` + testAccountID + `"}`} {
		if _, err := q.Decode(json.RawMessage(bad)); !errors.Is(err, ErrParamsInvalid) {
			t.Errorf("Decode(%s) 错误 = %v，期望 ErrParamsInvalid", bad, err)
		}
	}
	if src.capCalls != 1 {
		t.Errorf("能力探针被问了 %d 次，期望 1", src.capCalls)
	}
}
