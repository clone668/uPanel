package notifications

import (
	"context"

	"upanel/internal/agentclient"
)

// AgentSender 把投递交给代理执行（Sender 的生产实现）。
//
// 这一层薄到几乎不像一层，但它把两件事固定下来：
//   - 面板只知道“发到哪、说什么”。Settings 原样转过去，secret_ref 原样转过去，
//     明文从头到尾不经过这个进程——**这正是把发送放到代理侧的全部理由**；
//   - 调用失败（连不上代理、超时）在这里就是一次“值得重试的投递失败”，
//     attemptDelivery 拿到 error 时已经这么处理，所以这里只需把 error 交上去。
type AgentSender struct {
	client AgentClient
}

// AgentClient 是 AgentSender 需要的代理客户端能力（生产实现是 *agentclient.Client）。
// 抽成接口是为了在单测里注入一个“总是失败”或“总是接受”的替身，而不是为了多一层抽象。
type AgentClient interface {
	Deliver(ctx context.Context, in agentclient.DeliverInput) (agentclient.DeliverResult, error)
}

// NewAgentSender 构造一个走代理的投递端。
func NewAgentSender(client AgentClient) AgentSender {
	return AgentSender{client: client}
}

// Deliver 实现 Sender。
func (a AgentSender) Deliver(ctx context.Context, in DeliverInput) (DeliverResult, error) {
	res, err := a.client.Deliver(ctx, agentclient.DeliverInput{
		Type: in.Type,
		Settings: agentclient.DeliverSettings{
			Host:     in.Settings.Host,
			Port:     in.Settings.Port,
			TLS:      in.Settings.TLS,
			From:     in.Settings.From,
			To:       in.Settings.To,
			Username: in.Settings.Username,
			URL:      in.Settings.URL,
		},
		SecretRef: in.SecretRef,
		Title:     in.Title,
		Body:      in.Body,
	})
	if err != nil {
		// 代理不可用：值不值得重试由 attemptDelivery 与通道的 max_attempts 决定，
		// 这里不替它猜。
		return DeliverResult{}, err
	}
	return DeliverResult{
		Accepted:  res.Accepted,
		Status:    res.Status,
		Detail:    res.Detail,
		Retryable: res.Retryable,
	}, nil
}

// 编译期断言：生产装配的形状必须一直成立。
var _ func(AgentClient) AgentSender = NewAgentSender
