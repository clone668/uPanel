package agent

// 六个账号动作的解码测试（P14.5）。
//
// **这个文件是为了一个真机上才炸出来的缺陷而写的。** `accounts.Definition` 当初
// 一个 json 标签都没写，而代理解参数用的是 `DisallowUnknownFields`。encoding/json
// 的“大小写不敏感”只对整词有效：`name` 能匹配到 `Name`，但 `display_name`、
// `password_secret_ref`、`shell_profile` 里带下划线，一个都匹配不上。
//
// 于是页面完全按 CONTRACTS §13.8 的形状提交，代理回
// `参数不合法: json: unknown field "display_name"`，预检 400，整条账号链路
// （建号、公钥、sudo）在黑盒验收里全线失败——而所有单元测试都是绿的，因为它们
// 只构造 Go 结构体，从不真的走一遍线上那份 JSON。
//
// 所以这里的用例一律从**线格式**出发：写 JSON 字符串、走真实的 Decode 函数、
// 再断言解出来的字段。谁改了结构体标签，这里就会红。

import (
	"errors"
	"testing"

	"upanel/internal/accounts"
)

const (
	// contractAccountID / contractSecretRef / contractKeyID 都是形状合法的内部标识。
	contractAccountID = "sac_00112233445566778899aabbccddeeff"
	contractSecretRef = "sec_00112233445566778899aabbccddeeff"
	contractKeyID     = "ssk_00112233445566778899aabbccddeeff"
)

// decodeAction 走真实的解码路径（登记层用哪个函数，这里就用哪个）。
func decodeAction(t *testing.T, action string, raw string) (any, error) {
	t.Helper()
	a := actionByName(t, action)
	return a.Decode([]byte(raw))
}

// actionByName 从登记表里取一个动作（六个动作与查询成对登记，见 accountquery_test.go）。
func actionByName(t *testing.T, name string) Action {
	t.Helper()
	reg, err := NewProductionRegistry(ProductionDeps{Accounts: &fakeAccounts{}})
	if err != nil {
		t.Fatalf("构造生产登记表失败: %v", err)
	}
	a, ok := reg.Action(name)
	if !ok {
		t.Fatalf("动作 %s 没登记", name)
	}
	return a
}

// TestAccountApplyDecodeAcceptsContractShape 按 CONTRACTS §13.8 的形状提交必须解得开。
func TestAccountApplyDecodeAcceptsContractShape(t *testing.T) {
	raw := `{
		"account_id": null,
		"version": null,
		"definition": {
			"name": "alice",
			"display_name": "Alice 的运维账号",
			"type": "regular",
			"disabled": false,
			"password_secret_ref": "` + contractSecretRef + `",
			"shell_profile": "restricted"
		}
	}`
	params, err := decodeAction(t, accounts.ActionAccountApply, raw)
	if err != nil {
		t.Fatalf("按约定形状提交却被拒: %v", err)
	}
	p, ok := params.(accounts.AccountApplyParams)
	if !ok {
		t.Fatalf("Decode 返回 %T", params)
	}
	if p.AccountID != nil {
		t.Errorf("account_id = %q，新建应当是 nil", *p.AccountID)
	}
	if p.Definition == nil {
		t.Fatal("definition 没了")
	}
	def := *p.Definition
	if def.Name != "alice" {
		t.Errorf("name = %q", def.Name)
	}
	// 这一条是缺陷的正身：带下划线的字段曾经一个都解不出来。
	if def.DisplayName != "Alice 的运维账号" {
		t.Errorf("display_name 没解出来：%q", def.DisplayName)
	}
	if def.PasswordSecretRef != contractSecretRef {
		t.Errorf("password_secret_ref 没解出来：%q", def.PasswordSecretRef)
	}
	if def.ShellProfile != accounts.ProfileRestricted {
		t.Errorf("shell_profile = %q", def.ShellProfile)
	}
	if def.Type != accounts.TypeRegular {
		t.Errorf("type = %q", def.Type)
	}
	if def.Disabled {
		t.Error("disabled 被解成了 true")
	}
}

// TestAccountApplyDecodeDefaultsShellProfile 不给壳档时落 disabled 档，不猜。
func TestAccountApplyDecodeDefaultsShellProfile(t *testing.T) {
	raw := `{"definition":{"name":"bob","type":"regular"}}`
	params, err := decodeAction(t, accounts.ActionAccountApply, raw)
	if err != nil {
		t.Fatalf("Decode 失败: %v", err)
	}
	p := params.(accounts.AccountApplyParams)
	if p.Definition.ShellProfile != accounts.ProfileDisabled {
		t.Errorf("shell_profile = %q，期望默认 %q", p.Definition.ShellProfile, accounts.ProfileDisabled)
	}
}

// TestAccountApplyDecodeRejectsPrivilegedFields 系统身份字段一律在解码层被拒。
//
// 这四条不在结构体里，`DisallowUnknownFields` 会把它们当未知字段拒掉——“提交不了”
// 比“提交了被忽略”可靠得多：后者会让人以为设置生效了。
func TestAccountApplyDecodeRejectsPrivilegedFields(t *testing.T) {
	for _, field := range []struct{ name, value string }{
		{"uid", "0"},
		{"gid", "0"},
		{"groups", `["sudo"]`},
		{"home", "/root"},
		{"shell", "/bin/bash"},
	} {
		raw := `{"definition":{"name":"alice","type":"regular","` + field.name + `":` + field.value + `}}`
		if _, err := decodeAction(t, accounts.ActionAccountApply, raw); !errors.Is(err, ErrParamsInvalid) {
			t.Errorf("definition.%s 被接受了（err=%v）", field.name, err)
		}
	}
}

// TestAccountActionsDecodeContractShapes 六个动作按约定形状提交都解得开。
//
// 一行一条，覆盖每一种参数组合：新建/更新、带/不带 key_id、必填 version。
func TestAccountActionsDecodeContractShapes(t *testing.T) {
	cases := []struct {
		name   string
		action string
		raw    string
	}{
		{"建账号", accounts.ActionAccountApply,
			`{"account_id":null,"version":null,"definition":{"name":"alice","type":"regular","shell_profile":"restricted"}}`},
		{"改账号", accounts.ActionAccountApply,
			`{"account_id":"` + contractAccountID + `","version":"3","definition":{"name":"alice","display_name":"A","type":"regular","disabled":true,"shell_profile":"restricted"}}`},
		{"删账号", accounts.ActionAccountDelete,
			`{"account_id":"` + contractAccountID + `","version":"3"}`},
		{"加公钥", accounts.ActionSSHKeyApply,
			`{"account_id":"` + contractAccountID + `","version":"3","key_id":null,"public_key":"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExample alice@host"}`},
		{"换公钥", accounts.ActionSSHKeyApply,
			`{"account_id":"` + contractAccountID + `","version":"3","key_id":"` + contractKeyID + `","public_key":"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExample alice@host"}`},
		{"删公钥", accounts.ActionSSHKeyDelete,
			`{"account_id":"` + contractAccountID + `","version":"3","key_id":"` + contractKeyID + `"}`},
		{"下发 sudo", accounts.ActionSudoRuleApply,
			`{"account_id":"` + contractAccountID + `","version":"3","template_id":"system_status"}`},
		{"撤 sudo", accounts.ActionSudoRuleDelete,
			`{"account_id":"` + contractAccountID + `","version":"3"}`},
	}
	for _, c := range cases {
		if _, err := decodeAction(t, c.action, c.raw); err != nil {
			t.Errorf("%s（%s）被拒: %v", c.name, c.action, err)
		}
	}
}

// TestSudoRuleApplyDecodeRejectsUnknownTemplate 模板标识必须是闭集里的那两个。
func TestSudoRuleApplyDecodeRejectsUnknownTemplate(t *testing.T) {
	raw := `{"account_id":"` + contractAccountID + `","version":"1","template_id":"all"}`
	if _, err := decodeAction(t, accounts.ActionSudoRuleApply, raw); !errors.Is(err, ErrParamsInvalid) {
		t.Errorf("template_id=all 被接受了（err=%v）", err)
	}
}
