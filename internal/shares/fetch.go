package shares

// Web 侧读共享信息的客户端（P08.1）。
//
// 与 internal/storage/fetch.go 同一个形状：接口定义在消费方，生产实现是 *agentclient.Client，
// 代理不可用与“代理明确说不”分成两种错误（503 与 4xx），不压成一个。
//
// 这一层**不做判决**：能不能应用一条共享、账号存不存在，都是代理算的。Web 侧只在
// URL 参数的形状上先挡一道（标识会进日志与审计），其余一律转发。
//
// 密码明文只从这里经过一次：POST /api/v1/secrets 的 handler 把 body 里的 value
// 原样交给代理，不缓存、不写日志、不回显。Fetcher 的任何错误信息都不包含 value。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"upanel/internal/agentclient"
	"upanel/internal/httpx"
)

// ErrAgentUnavailable 表示读不到共享信息是因为代理不可用（连不上、超时）。
var ErrAgentUnavailable = errors.New("shares: 代理不可用")

// RemoteError 是代理明确拒绝（4xx）时带到 Web 层的错误。
//
// 共享的“不存在”是 404、“版本已过期”是 409、“能力不可用”是 409——页面要据此
// 显示不同的话，所以不能一律压成 500 或 503。
type RemoteError struct {
	Status  int
	Code    httpx.Code
	Message string
}

func (e *RemoteError) Error() string {
	return fmt.Sprintf("shares: 代理拒绝（%d %s）：%s", e.Status, e.Code, e.Message)
}

// QueryAgent 是 Web 侧需要的代理能力。生产实现是 *agentclient.Client。
//
// 除三条只读查询外只有“写一条秘密”：协议里没有读取明文的接口，所以这里也没有
// 对应的读方法——不是因为没写，而是因为不该有。
type QueryAgent interface {
	Query(ctx context.Context, name string, params any) (json.RawMessage, error)
	PutSecret(ctx context.Context, in agentclient.PutSecretInput) (agentclient.SecretRef, error)
}

// Fetcher 是 Web 侧读取 share.capability / share.list / share.accounts 的客户端。
type Fetcher struct {
	agent  QueryAgent
	logger *slog.Logger
}

// NewFetcher 构造 Fetcher。agent 为 nil 时返回错误：宁可启动时报错，
// 也不要在运行时用一个永远返回“没有共享”的假实现顶上。
func NewFetcher(agent QueryAgent, logger *slog.Logger) (*Fetcher, error) {
	if agent == nil {
		return nil, errors.New("shares: Fetcher 需要代理客户端")
	}
	return &Fetcher{agent: agent, logger: logger}, nil
}

// Shares 读取共享列表与同一时刻的能力报告。
//
// 两者一起返回是刻意的：definition.enabled 说的是“定义启用”，config_applied 说的
// 是“配置真的写进 Samba 了”。分成两次请求，中间就会出现一小段时间界面显示
// “已启用、已生效”而 Samba 那边还没有这一段。
func (f *Fetcher) Shares(ctx context.Context) (Capability, []ShareRecord, error) {
	raw, err := f.query(ctx, QueryShares, nil)
	if err != nil {
		return Capability{}, nil, err
	}
	var payload struct {
		Capability Capability    `json:"capability"`
		Shares     []ShareRecord `json:"shares"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return Capability{}, nil, fmt.Errorf("shares: 代理返回的共享列表不是合法 JSON: %w", err)
	}
	if payload.Shares == nil {
		payload.Shares = []ShareRecord{}
	}
	if payload.Capability.Notices == nil {
		payload.Capability.Notices = []Blocker{}
	}
	return payload.Capability, payload.Shares, nil
}

// Share 读取单条共享。
//
// 走的是列表查询再挑一条：CONTRACTS.md 第 15 节只登记了 share.list 与 share.accounts，
// 为详情页再加一条“按 id 查”会让代理多一个只差一个过滤条件的入口。共享数量是几十
// 的量级，全量返回的代价远小于两个入口各自演化的风险。
func (f *Fetcher) Share(ctx context.Context, id string) (Capability, ShareRecord, bool, error) {
	cap, recs, err := f.Shares(ctx)
	if err != nil {
		return Capability{}, ShareRecord{}, false, err
	}
	for _, rec := range recs {
		if rec.ID == id {
			return cap, rec, true, nil
		}
	}
	return cap, ShareRecord{}, false, nil
}

// Connections 读取某一份共享此刻的连接状态。
//
// 走 share.connections 查询：它读的是 smbstatus，是**这台机器此刻的事实**，
// 而不是登记表里的配置。返回体里带着 service 状态，因为空列表有两种含义
// （没有人连着 / 服务没在跑），只有把服务状态一起看才分得清。
func (f *Fetcher) Connections(ctx context.Context, id string) (ConnectionsReport, error) {
	raw, err := f.query(ctx, QueryConnections, map[string]any{"share_id": id})
	if err != nil {
		return ConnectionsReport{}, err
	}
	var report ConnectionsReport
	if err := json.Unmarshal(raw, &report); err != nil {
		return ConnectionsReport{}, fmt.Errorf("shares: 代理返回的连接状态不是合法 JSON: %w", err)
	}
	if report.Connections == nil {
		report.Connections = []Connection{}
	}
	if report.Notices == nil {
		report.Notices = []Blocker{}
	}
	return report, nil
}

// Accounts 读取受管理账号列表与能力报告。
//
// 返回的 AccountRecord 里没有密码、没有哈希，也没有 secret_ref（那个字段是 json:"-"）。
func (f *Fetcher) Accounts(ctx context.Context) (Capability, []AccountRecord, error) {
	raw, err := f.query(ctx, QueryAccounts, nil)
	if err != nil {
		return Capability{}, nil, err
	}
	var payload struct {
		Capability Capability      `json:"capability"`
		Accounts   []AccountRecord `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return Capability{}, nil, fmt.Errorf("shares: 代理返回的账号列表不是合法 JSON: %w", err)
	}
	if payload.Accounts == nil {
		payload.Accounts = []AccountRecord{}
	}
	if payload.Capability.Notices == nil {
		payload.Capability.Notices = []Blocker{}
	}
	return payload.Capability, payload.Accounts, nil
}

// PutSecret 把一次秘密写入转给代理，返回引用。
//
// 明文只在 in.Value 里出现一次：这里不缓存它、不拼进日志、也不放进任何错误信息。
// 返回的 SecretRef 只有引用与生命周期，Web 侧拿它去填 share.account.apply 的 secret_ref。
func (f *Fetcher) PutSecret(ctx context.Context, in agentclient.PutSecretInput) (agentclient.SecretRef, error) {
	ref, err := f.agent.PutSecret(ctx, in)
	if err != nil {
		var unavailable *agentclient.UnavailableError
		if errors.As(err, &unavailable) {
			if f.logger != nil {
				// 只记用途，不记内容。
				f.logger.Warn("写入秘密失败：代理不可用", "purpose", in.Purpose, "error", err)
			}
			return agentclient.SecretRef{}, fmt.Errorf("%w: %v", ErrAgentUnavailable, err)
		}
		var rejected *agentclient.Error
		if errors.As(err, &rejected) {
			if f.logger != nil {
				f.logger.Warn("代理拒绝了秘密写入",
					"purpose", in.Purpose, "status", rejected.Status, "code", rejected.Code)
			}
			return agentclient.SecretRef{}, &RemoteError{
				Status:  rejected.Status,
				Code:    httpx.Code(rejected.Code),
				Message: rejected.Message,
			}
		}
		return agentclient.SecretRef{}, fmt.Errorf("shares: 代理拒绝了秘密写入: %w", err)
	}
	return ref, nil
}

// query 是三条读取共用的代理调用与错误归一。
func (f *Fetcher) query(ctx context.Context, name string, params any) (json.RawMessage, error) {
	raw, err := f.agent.Query(ctx, name, params)
	if err != nil {
		var unavailable *agentclient.UnavailableError
		if errors.As(err, &unavailable) {
			if f.logger != nil {
				f.logger.Warn("读取共享信息失败：代理不可用", "query", name, "error", err)
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
		return nil, fmt.Errorf("shares: 代理拒绝了 %s 查询: %w", name, err)
	}
	return raw, nil
}
