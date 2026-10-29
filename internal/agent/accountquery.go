package agent

// 账号相关的只读查询（P14.5）：accounts.ssh_keys 与 accounts.capability。
//
// 账号列表不需要新查询：share.accounts 读的就是同一张 managed_accounts，P14.5 给
// 那张表补的两列（display_name / shell_profile）已经在 AccountRecord 里，所以
// GET /accounts 直接吃 share.accounts 的输出。
//
// 公钥列表必须是新查询：它读的是 managed_ssh_keys 这张 P14.5 才有的表，而且
// 它按账号过滤。**读的是登记表，不是文件**——authorized_keys 里此刻有没有这些行
// 属于“执行器写完读回”的事，页面上的公钥列表回答的是“面板认为配了哪几把”。
//
// 能力报告同样必须是新查询，而且**不能**用 share.accounts 带的那一份：
// 那里面是 Samba 的工具集（smbd / smbpasswd / pdbedit…），拿它当账号页的判据，
// 一台没装 Samba 的机器上连普通账号都建不了——虚拟机上就是这么发现的。
// 账号要的工具是 useradd / usermod / userdel / getent / passwd / chpasswd，
// 外加两档壳与 visudo，判据完全不同，所以各探各的。

import (
	"context"
	"encoding/json"
	"fmt"

	"upanel/internal/accounts"
	"upanel/internal/shares"
)

// accountsSSHKeysQueryName 是公钥列表查询的登记名。
const accountsSSHKeysQueryName = accounts.QueryAccountsSSHKeys

// accountsCapabilityQueryName 是账号能力查询的登记名。
const accountsCapabilityQueryName = accounts.QueryAccountsCapability

// sshKeysParams 是 accounts.ssh_keys 的参数。
type sshKeysParams struct {
	AccountID string `json:"account_id"`
}

// sshKeysView 是 accounts.ssh_keys 的返回体。
//
// Blob 不出这个边界（SSHKeyRecord 上标了 json:"-"）：页面只需要指纹与算法名，
// 而整行原文已经在代理的库里，没有理由在这个响应里多带一份。
type sshKeysView struct {
	AccountID string                  `json:"account_id"`
	Keys      []accounts.SSHKeyRecord `json:"ssh_keys"`
}

// newSSHKeysQuery 登记 accounts.ssh_keys。
func newSSHKeysQuery(src accountsSource) Query {
	return Query{
		Name: accountsSSHKeysQueryName,
		Decode: func(raw json.RawMessage) (any, error) {
			var p sshKeysParams
			if err := DecodeParams(raw, &p); err != nil {
				return nil, err
			}
			if !shares.ValidAccountID(p.AccountID) {
				return nil, fmt.Errorf("%w: account_id 必须是 sac_ 加 32 位小写十六进制，收到 %q",
					ErrParamsInvalid, p.AccountID)
			}
			return p, nil
		},
		Run: func(ctx context.Context, params any) (any, error) {
			p, ok := params.(sshKeysParams)
			if !ok {
				return nil, fmt.Errorf("%w: accounts.ssh_keys 的参数类型是 %T", ErrParamsInvalid, params)
			}
			keys, err := src.SSHKeys(ctx, p.AccountID)
			if err != nil {
				return nil, err
			}
			if keys == nil {
				// 一把公钥都没有是“没配”，不是“不知道”。null 会被前端当成后者。
				keys = []accounts.SSHKeyRecord{}
			}
			return sshKeysView{AccountID: p.AccountID, Keys: keys}, nil
		},
	}
}

// accountsCapabilityView 是 accounts.capability 的返回体。
//
// 只包一层，理由与 shares 那边一样：查询的返回体里留一个对象，以后要加
// “这一台机器上一次探测是什么时候”之类的字段时不必改形状。
type accountsCapabilityView struct {
	Capability accounts.Capability `json:"capability"`
}

// newAccountsCapabilityQuery 登记 accounts.capability。
//
// 参数是空对象：能力探测没有可调的输入。**不能让调用方选工具路径**——那等于
// 让 HTTP 决定面板去执行哪个二进制（PLAN.md 第 4 节）。
func newAccountsCapabilityQuery(src accountsSource) Query {
	return Query{
		Name: accountsCapabilityQueryName,
		Decode: func(raw json.RawMessage) (any, error) {
			var params struct{}
			if err := DecodeParams(raw, &params); err != nil {
				return nil, err
			}
			return params, nil
		},
		Run: func(ctx context.Context, _ any) (any, error) {
			return accountsCapabilityView{Capability: src.Capability(ctx)}, nil
		},
	}
}
