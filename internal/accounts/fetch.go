package accounts

// Web 侧读账号公钥的客户端（P14.5）。
//
// 与 internal/shares/fetch.go 同一个形状：接口定义在消费方，生产实现是
// *agentclient.Client，代理不可用（503）与代理明确拒绝（4xx）分成两种错误。
//
// 这里只有一条查询：账号列表走 share.accounts（同一张 managed_accounts），
// 六个写动作一律走统一的预检 + 任务通道，Web 侧没有也不该有“直接建账号”的入口。
//
// 错误类型直接复用 shares 那边（ErrAgentUnavailable / RemoteError）：它们描述的
// 是**代理这条链路**的两种拒绝，不是共享模块的业务概念；再造一对同义类型只会让
// api 层写两遍一模一样的映射。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"upanel/internal/agentclient"
	"upanel/internal/httpx"
	"upanel/internal/shares"
)

// ErrAgentUnavailable 是 shares.ErrAgentUnavailable 的别名。
//
// 加这个别名是为了让调用方不必为了捕获一个错误去 import 共享模块——但错误值本身
// 是同一个，所以 api 层现有的那段 errors.Is 判断照样命中。
var ErrAgentUnavailable = shares.ErrAgentUnavailable

// RemoteError 是代理明确拒绝（4xx）时带到 Web 层的错误。
type RemoteError = shares.RemoteError

// QueryAgent 是 Web 侧需要的代理能力。
type QueryAgent interface {
	Query(ctx context.Context, name string, params any) (json.RawMessage, error)
}

// Fetcher 是 Web 侧读取 accounts.ssh_keys 的客户端。
type Fetcher struct {
	agent  QueryAgent
	logger *slog.Logger
}

// NewFetcher 构造 Fetcher。agent 为 nil 时返回错误：宁可启动时报错，
// 也不要运行时用一个永远返回“没有公钥”的假实现顶上——那会被读成
// “这个账号没有配钥匙”，而人可能正等着用那把钥匙登录。
func NewFetcher(agent QueryAgent, logger *slog.Logger) (*Fetcher, error) {
	if agent == nil {
		return nil, errors.New("accounts: Fetcher 需要代理客户端")
	}
	return &Fetcher{agent: agent, logger: logger}, nil
}

// SSHKeys 读取某个账号在面板里登记的公钥。
//
// 返回的是**登记表**里的行，不是此刻 authorized_keys 的内容：文件里有没有这些行
// 属于执行器的读回核验（execute.go 的 writeManagedKeys 会整份重读比对指纹集合），
// 而这条查询回答的是“面板认为配了哪几把”。两个问题分开问，因为它们的差距
// 恰恰是要显示给人看的东西。
func (f *Fetcher) SSHKeys(ctx context.Context, accountID string) ([]SSHKeyRecord, error) {
	raw, err := f.query(ctx, QueryAccountsSSHKeys, map[string]string{"account_id": accountID})
	if err != nil {
		return nil, err
	}
	var payload struct {
		AccountID string         `json:"account_id"`
		Keys      []SSHKeyRecord `json:"ssh_keys"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("accounts: 代理返回的公钥列表不是合法 JSON: %w", err)
	}
	// 代理说“这是 A 的公钥”，这里就必须是 A 的。不核对的话，一次错位的响应会
	// 让页面把别人的钥匙挂在 A 名下——那是最不该出现在界面上的错。
	if payload.AccountID != accountID {
		return nil, fmt.Errorf("accounts: 代理返回的公钥属于 %q，不是请求的 %q",
			payload.AccountID, accountID)
	}
	if payload.Keys == nil {
		payload.Keys = []SSHKeyRecord{}
	}
	return payload.Keys, nil
}

// Capability 读取账号模块的能力报告。
//
// 它读的是 accounts.capability，不是 share.accounts 带的那一份：后者判的是 Samba
// 的工具在不在（smbd / smbpasswd / pdbedit…），而账号要的是一整套别的工具
// （useradd / usermod / getent / chpasswd + 两档壳 + visudo）。两个工具集不相交，
// 用错一份能力会让一台没装 Samba 的机器连普通账号都建不了——这正是虚拟机验收
// 抓到的缺陷，所以这里宁可多一次查询，也不去猜。
func (f *Fetcher) Capability(ctx context.Context) (Capability, error) {
	raw, err := f.query(ctx, QueryAccountsCapability, struct{}{})
	if err != nil {
		return Capability{}, err
	}
	var payload struct {
		Capability Capability `json:"capability"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return Capability{}, fmt.Errorf("accounts: 代理返回的能力报告不是合法 JSON: %w", err)
	}
	// status 是闭集里的两个词之一。空串说明响应的形状不是这一版约定，
	// 这时候回一份“看起来像 blocked”的对象会让人以为机器缺工具——那不是真话。
	if payload.Capability.Status == "" {
		return Capability{}, fmt.Errorf("accounts: 代理返回的能力报告没有 status（%s）",
			raw)
	}
	return payload.Capability, nil
}

// query 是查询的代理调用与错误归一。
func (f *Fetcher) query(ctx context.Context, name string, params any) (json.RawMessage, error) {
	raw, err := f.agent.Query(ctx, name, params)
	if err != nil {
		var unavailable *agentclient.UnavailableError
		if errors.As(err, &unavailable) {
			if f.logger != nil {
				f.logger.Warn("读取账号信息失败：代理不可用", "query", name, "error", err)
			}
			return nil, fmt.Errorf("%w: %v", ErrAgentUnavailable, err)
		}
		var rejected *agentclient.Error
		if errors.As(err, &rejected) {
			return nil, &RemoteError{
				Status:  rejected.Status,
				Code:    httpx.Code(rejected.Code),
				Message: rejected.Message,
			}
		}
		return nil, fmt.Errorf("accounts: 代理拒绝了 %s 查询: %w", name, err)
	}
	return raw, nil
}

// AccountLister 是账号列表的来源。生产实现是 *shares.Fetcher（share.accounts）。
//
// 单独声明成这样一个小接口，是因为 Reader 不关心列表是怎么读出来的，只关心
// “能读到记录”。它也顺手说明了这个模块与共享模块的关系：读同一张表，但写走
// 两条路，能力也各探各的（列表带回来的那份能力是 Samba 的，不用）。
type AccountLister interface {
	Accounts(ctx context.Context) (shares.Capability, []shares.AccountRecord, error)
}

// Reader 是 Web 侧账号读端的合成体：列表来自共享读取端，能力与公钥来自本包的
// Fetcher。
//
// 为什么合成而不是让本包自己再读一遍 share.accounts：同一张 managed_accounts 被
// 两条查询读出来，两条查询的字段就会各自漂移，而漂移的那一天页面会显示一个
// 和代理判决不一致的账号状态。列表只有一份实现（P08.1 已交付并验收过），
// 公钥与能力各只有一份实现（本包），合成体把它们拼在消费方看得见的地方。
type Reader struct {
	lists AccountLister
	keys  *Fetcher
}

// NewReader 构造 Reader。两半都必须有：缺列表就没有账号可看，缺公钥读端则
// “这个账号配了哪几把钥匙”永远答不上来，而那不是“没有钥匙”。
func NewReader(lists AccountLister, keys *Fetcher) (*Reader, error) {
	if lists == nil {
		return nil, errors.New("accounts: Reader 需要账号列表来源")
	}
	if keys == nil {
		return nil, errors.New("accounts: Reader 需要公钥读取端")
	}
	return &Reader{lists: lists, keys: keys}, nil
}

// Accounts 读到账号列表与同一时刻的能力报告。
//
// 列表带回来的 `shares.Capability` 被**丢掉**了，换成账号自己的能力：前者的
// status 判的是 Samba 齐不齐，而账号页要回答的是“这台机器能不能建账号”。
// 两个问题不同源，答案就不能互相借用。
func (r *Reader) Accounts(ctx context.Context) (Capability, []shares.AccountRecord, error) {
	_, records, err := r.lists.Accounts(ctx)
	if err != nil {
		return Capability{}, nil, err
	}
	caps, err := r.keys.Capability(ctx)
	if err != nil {
		return Capability{}, nil, err
	}
	return caps, records, nil
}

// SSHKeys 读到某个账号登记的公钥。
func (r *Reader) SSHKeys(ctx context.Context, accountID string) ([]SSHKeyRecord, error) {
	return r.keys.SSHKeys(ctx, accountID)
}
