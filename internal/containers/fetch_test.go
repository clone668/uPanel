package containers

// Web 侧读取端的错误翻译用例（P09.3）。
//
// 这里只钉一件事：代理的两种 5xx 不能混为一谈。
//   - 代理自己不可用（503 AGENT_UNAVAILABLE、500 INTERNAL_ERROR、连不上）→ ErrAgentUnavailable；
//   - 代理回了域内判决（503 DOCKER_UNAVAILABLE、409 PREFLIGHT_STALE …）→ 原样带出，
//     否则页面会说“等代理一会儿”，而用户该做的是把 dockerd 起起来。
//
// 真机上这条差别是 §11.2 抓出来的：dockerd 停掉后读详情，状态码 503 对了，
// 码却是 AGENT_UNAVAILABLE。原因就在这里——旧逻辑只看状态码 >= 500。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"upanel/internal/agentclient"
)

// errAgent 是 Agent 的测试替身：每次查询都返回同一个错误。
type errAgent struct{ err error }

func (a errAgent) Query(context.Context, string, any) (json.RawMessage, error) {
	return nil, a.err
}

func (a errAgent) CreateStream(context.Context, string, any) (agentclient.StreamPermit, error) {
	return agentclient.StreamPermit{}, a.err
}

func (a errAgent) OpenStream(context.Context, string) (*agentclient.StreamReader, error) {
	return nil, a.err
}

// fetchWith 用给定错误构造 Fetcher 并读一次容器详情。
func fetchWith(t *testing.T, err error) error {
	t.Helper()
	f := &Fetcher{agent: errAgent{err: err}}
	_, got := f.Get(context.Background(), strings.Repeat("a", 64))
	return got
}

// TestFetchKeepsDockerUnavailable 代理的 503 DOCKER_UNAVAILABLE 原样带出。
func TestFetchKeepsDockerUnavailable(t *testing.T) {
	got := fetchWith(t, &agentclient.Error{
		Status:  http.StatusServiceUnavailable,
		Code:    "DOCKER_UNAVAILABLE",
		Message: "Docker 守护进程没有应答（套接字在，但连不上）",
	})
	var rejected *RejectedError
	if !errors.As(got, &rejected) {
		t.Fatalf("错误 = %v，期望 *RejectedError", got)
	}
	if rejected.Status != http.StatusServiceUnavailable || rejected.Code != "DOCKER_UNAVAILABLE" {
		t.Fatalf("判决 = %d %s，期望 503 DOCKER_UNAVAILABLE", rejected.Status, rejected.Code)
	}
	if errors.Is(got, ErrAgentUnavailable) {
		t.Fatal("“Docker 不可用”不能翻成“代理不可用”：两者的处置完全不同")
	}
}

// TestFetchKeepsPreflightStale 域内的 409 也照样带出（码不被改写）。
func TestFetchKeepsPreflightStale(t *testing.T) {
	got := fetchWith(t, &agentclient.Error{
		Status: http.StatusConflict, Code: "PREFLIGHT_STALE", Message: "容器状态变了",
	})
	var rejected *RejectedError
	if !errors.As(got, &rejected) || rejected.Code != "PREFLIGHT_STALE" {
		t.Fatalf("错误 = %v，期望 409 PREFLIGHT_STALE", got)
	}
}

// TestFetchMapsAgentGeneric5xxToUnavailable 代理自己的 5xx 仍然是“代理不可用”。
func TestFetchMapsAgentGeneric5xxToUnavailable(t *testing.T) {
	for _, code := range []string{"AGENT_UNAVAILABLE", "INTERNAL_ERROR", ""} {
		got := fetchWith(t, &agentclient.Error{
			Status: http.StatusServiceUnavailable, Code: code, Message: "代理自己出问题",
		})
		if !errors.Is(got, ErrAgentUnavailable) {
			t.Fatalf("码 %q：错误 = %v，期望 ErrAgentUnavailable", code, got)
		}
	}
	got := fetchWith(t, &agentclient.UnavailableError{Err: errors.New("连不上代理")})
	if !errors.Is(got, ErrAgentUnavailable) {
		t.Fatalf("连不上代理：错误 = %v，期望 ErrAgentUnavailable", got)
	}
}
