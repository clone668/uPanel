package proxies

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"upanel/internal/agentclient"
	"upanel/internal/nginx"
)

// recordingAgent 记录每次查询用的参数，并按需返回结果或错误。
type recordingAgent struct {
	params []nginxStateParams
	facts  nginx.Facts
	err    error
	raw    string
}

func (a *recordingAgent) Query(_ context.Context, name string, params any) (json.RawMessage, error) {
	if name != NginxStateQueryName {
		return nil, errors.New("unexpected query: " + name)
	}
	p, _ := params.(nginxStateParams)
	a.params = append(a.params, p)
	if a.err != nil {
		return nil, a.err
	}
	if a.raw != "" {
		return json.RawMessage(a.raw), nil
	}
	out, err := json.Marshal(a.facts)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// TestAgentFactsFreshSemantics 确认三种读取的缓存语义。
//
// 这是本包最容易悄悄坏掉的地方：写操作之后的那一次展示读取必须绕过代理缓存，
// 否则用户会看到“刚保存完，页面还是旧的”，而再下一次又该回到缓存。
func TestAgentFactsFreshSemantics(t *testing.T) {
	agent := &recordingAgent{facts: nginx.Facts{Available: true, Version: "1.24.0"}}
	facts, err := NewAgentFacts(agent, nil)
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	ctx := context.Background()

	// 第一次展示读取：走缓存（fresh=false）。
	if _, err := facts.Snapshot(ctx); err != nil {
		t.Fatalf("Snapshot 失败: %v", err)
	}
	if got := agent.params[len(agent.params)-1].Fresh; got {
		t.Fatal("普通展示读取不该绕过缓存")
	}

	// 写之后：下一次展示读取必须 fresh=true。
	facts.Invalidate()
	if _, err := facts.Snapshot(ctx); err != nil {
		t.Fatalf("Snapshot 失败: %v", err)
	}
	if got := agent.params[len(agent.params)-1].Fresh; !got {
		t.Fatal("Invalidate 之后的那次展示读取必须绕过缓存")
	}

	// 而且只绕过这一次。
	if _, err := facts.Snapshot(ctx); err != nil {
		t.Fatalf("Snapshot 失败: %v", err)
	}
	if got := agent.params[len(agent.params)-1].Fresh; got {
		t.Fatal("绕过缓存只该生效一次，之后要回到缓存")
	}

	// 判断用读取永远 fresh。
	if _, err := facts.Collect(ctx); err != nil {
		t.Fatalf("Collect 失败: %v", err)
	}
	if got := agent.params[len(agent.params)-1].Fresh; !got {
		t.Fatal("Collect 必须永远绕过缓存")
	}
}

// TestAgentFactsMapsUnavailable 确认代理不可用被映射成本包的 ErrFactsUnavailable。
//
// 这个映射决定了接口层回 503 还是 500：前者是“等一会儿重试”，后者会让人去查代码。
func TestAgentFactsMapsUnavailable(t *testing.T) {
	agent := &recordingAgent{err: &agentclient.UnavailableError{Err: errors.New("socket 不存在")}}
	facts, err := NewAgentFacts(agent, nil)
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if _, err := facts.Collect(context.Background()); !errors.Is(err, ErrFactsUnavailable) {
		t.Fatalf("错误 = %v，期望 ErrFactsUnavailable", err)
	}
}

// TestAgentFactsRejectsBadPayload 确认解析失败不会被当成“没有配置”。
func TestAgentFactsRejectsBadPayload(t *testing.T) {
	agent := &recordingAgent{raw: `{"available":"not a bool"}`}
	facts, err := NewAgentFacts(agent, nil)
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if _, err := facts.Snapshot(context.Background()); err == nil {
		t.Fatal("坏的响应必须报错，不能返回一份零值事实")
	}
}

// TestNewAgentFactsRequiresAgent 确认不拿一个“永远没有冲突”的假实现顶上。
func TestNewAgentFactsRequiresAgent(t *testing.T) {
	if _, err := NewAgentFacts(nil, nil); err == nil {
		t.Fatal("agent 为 nil 必须报错")
	}
}
