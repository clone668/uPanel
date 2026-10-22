package agent

// 共享相关的只读查询（P08.1）：share.capability、share.list、share.accounts。
//
// 三条查询都不碰系统状态：capability 读固定的工具路径与 smb.conf，list/accounts 读
// 代理自己的登记表。写操作一律走预检 + 任务，不存在“边查边写”的路径。
//
// share.list 把能力一起返回，是因为页面必须同时看到两件事：这条共享的定义是什么，
// 以及它**现在有没有被提供**。definition 里的 enabled 只是“定义启用”，
// config_applied 才是“配置写进 Samba 了”。把两者分开发两次请求，中间就会出现
// 一段时间里界面显示“已启用、已生效”，而 Samba 那边还没有这一段。

import (
	"context"
	"encoding/json"

	"upanel/internal/shares"
)

// shareCapabilityQueryName 是能力查询的登记名。
const shareCapabilityQueryName = shares.QueryCapability

// shareListQueryName / shareAccountsQueryName 是两条读登记表的查询名。
const (
	shareListQueryName     = shares.QueryShares
	shareAccountsQueryName = shares.QueryAccounts
)

// newShareCapabilityQuery 登记 share.capability。
func newShareCapabilityQuery(src sharesSource) Query {
	return Query{
		Name: shareCapabilityQueryName,
		Decode: func(raw json.RawMessage) (any, error) {
			var params struct{}
			if err := DecodeParams(raw, &params); err != nil {
				return nil, err
			}
			return params, nil
		},
		Run: func(ctx context.Context, _ any) (any, error) {
			return src.Capability(ctx), nil
		},
	}
}

// shareListView 是 share.list 的返回体。
type shareListView struct {
	Capability shares.Capability    `json:"capability"`
	Shares     []shares.ShareRecord `json:"shares"`
}

// newShareListQuery 登记 share.list。
func newShareListQuery(src sharesSource) Query {
	return Query{
		Name: shareListQueryName,
		Decode: func(raw json.RawMessage) (any, error) {
			var params struct{}
			if err := DecodeParams(raw, &params); err != nil {
				return nil, err
			}
			return params, nil
		},
		Run: func(ctx context.Context, _ any) (any, error) {
			recs, err := src.Shares(ctx)
			if err != nil {
				return nil, err
			}
			if recs == nil {
				// 空登记表是“没有共享”，不是“不知道”。null 会被前端当成后者。
				recs = []shares.ShareRecord{}
			}
			return shareListView{Capability: src.Capability(ctx), Shares: recs}, nil
		},
	}
}

// shareAccountView 是 share.accounts 的返回体。
//
// 里面**没有**密码、没有哈希、也没有 secret_ref：AccountRecord 的 SecretRef 字段
// 标了 json:"-"（它只是一个引用，但泄漏它等于告诉别人“去解哪个秘密”）。
type shareAccountView struct {
	Capability shares.Capability      `json:"capability"`
	Accounts   []shares.AccountRecord `json:"accounts"`
}

// newShareAccountsQuery 登记 share.accounts。
func newShareAccountsQuery(src sharesSource) Query {
	return Query{
		Name: shareAccountsQueryName,
		Decode: func(raw json.RawMessage) (any, error) {
			var params struct{}
			if err := DecodeParams(raw, &params); err != nil {
				return nil, err
			}
			return params, nil
		},
		Run: func(ctx context.Context, _ any) (any, error) {
			recs, err := src.Accounts(ctx)
			if err != nil {
				return nil, err
			}
			if recs == nil {
				// 同上：空账号表是“还没有受管理账号”，不是“读不到”。
				recs = []shares.AccountRecord{}
			}
			return shareAccountView{Capability: src.Capability(ctx), Accounts: recs}, nil
		},
	}
}

// RegisterShareQueries 登记三条共享查询。
func RegisterShareQueries(reg *Registry, src sharesSource) error {
	for _, q := range []Query{
		newShareCapabilityQuery(src),
		newShareListQuery(src),
		newShareAccountsQuery(src),
	} {
		if err := reg.RegisterQuery(q); err != nil {
			return err
		}
	}
	return nil
}
