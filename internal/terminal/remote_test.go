package terminal

// Web 侧调用端的用例：结果自洽性、错误翻译、许可与拨号的顺序。
//
// 这里用假的 Agent，因此可以在任何平台跑。真实代理（Unix socket、单次流许可、
// WebSocket 升级）由 internal/agent 的用例与 VM 验收覆盖。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/gorilla/websocket"

	"upanel/internal/agentclient"
	"upanel/internal/httpx"
)

// fakeAgent 是 Agent 的测试替身。
type fakeAgent struct {
	queryRaw json.RawMessage
	queryErr error
	queries  []string
	params   []any

	permit    agentclient.StreamPermit
	permitErr error
	purposes  []string

	dialConn   *websocket.Conn
	dialErr    error
	dialIDs    []string
	dialProtos [][]string

	closeErr   error
	closeCalls []string
	closeUsers []int64
	closeWhy   []string
}

func (f *fakeAgent) Query(_ context.Context, name string, params any) (json.RawMessage, error) {
	f.queries = append(f.queries, name)
	f.params = append(f.params, params)
	return f.queryRaw, f.queryErr
}

func (f *fakeAgent) CreateStream(_ context.Context, purpose string, _ any) (agentclient.StreamPermit, error) {
	f.purposes = append(f.purposes, purpose)
	return f.permit, f.permitErr
}

func (f *fakeAgent) DialTerminalStream(_ context.Context, streamID string, subprotocols []string) (*websocket.Conn, error) {
	f.dialIDs = append(f.dialIDs, streamID)
	f.dialProtos = append(f.dialProtos, subprotocols)
	return f.dialConn, f.dialErr
}

func (f *fakeAgent) CloseTerminalSession(_ context.Context, sessionID string, userID int64, reason string) error {
	f.closeCalls = append(f.closeCalls, sessionID)
	f.closeUsers = append(f.closeUsers, userID)
	f.closeWhy = append(f.closeWhy, reason)
	return f.closeErr
}

func newTestRemote(t *testing.T, agent Agent) *Remote {
	t.Helper()
	remote, err := NewRemote(agent, nil)
	if err != nil {
		t.Fatalf("NewRemote 失败: %v", err)
	}
	return remote
}

// TestSessionsNormalisesEmptyList 代理没给会话时是空数组，不是 nil。
func TestSessionsNormalisesEmptyList(t *testing.T) {
	agent := &fakeAgent{queryRaw: json.RawMessage(
		`{"account":"upanel-shell","account_uid":998,"idle_timeout_seconds":900,` +
			`"max_duration_seconds":3600,"disconnect_grace_seconds":60,"max_per_user":1,"max_sessions":2}`)}
	remote := newTestRemote(t, agent)
	result, err := remote.Sessions(context.Background(), 5, "")
	if err != nil {
		t.Fatalf("Sessions 失败: %v", err)
	}
	if result.Sessions == nil {
		t.Fatalf("sessions 是 nil，应当是空数组")
	}
	if result.Account != "upanel-shell" || result.AccountUID != 998 {
		t.Fatalf("边界字段 = %+v", result)
	}
	if len(agent.queries) != 1 || agent.queries[0] != QuerySessions {
		t.Fatalf("查询名 = %v", agent.queries)
	}
	params, ok := agent.params[0].(SessionsParams)
	if !ok || params.UserID != 5 || params.SessionID != "" {
		t.Fatalf("查询参数 = %#v", agent.params[0])
	}
}

// TestSessionsIncompleteResponseIsUnavailable 代理说“成功”却没有账号信息时，
// 不能把空账号当成 upanel-shell 显示出去。
func TestSessionsIncompleteResponseIsUnavailable(t *testing.T) {
	for _, raw := range []string{
		`{"account_uid":998}`,
		`{"account":"upanel-shell"}`,
		`not json`,
		`[]`,
	} {
		remote := newTestRemote(t, &fakeAgent{queryRaw: json.RawMessage(raw)})
		if _, err := remote.Sessions(context.Background(), 5, ""); !errors.Is(err, ErrAgentUnavailable) {
			t.Fatalf("%s 的错误 = %v，期望 ErrAgentUnavailable", raw, err)
		}
	}
}

// TestErrorMapping 代理不可用 → 503；代理的明确拒绝 → 原样带出；其余 → 未分类。
func TestErrorMapping(t *testing.T) {
	rejected := &agentclient.Error{Status: 409, Code: string(httpx.CodeResourceBusy), Message: "会话已被占用"}
	remote := newTestRemote(t, &fakeAgent{queryErr: rejected})
	_, err := remote.Sessions(context.Background(), 5, "")
	var remoteErr *RemoteError
	if !errors.As(err, &remoteErr) {
		t.Fatalf("错误 = %v，期望 RemoteError", err)
	}
	if remoteErr.Status != 409 || remoteErr.Code != httpx.CodeResourceBusy {
		t.Fatalf("判决 = %+v", remoteErr)
	}

	unavailable := &agentclient.UnavailableError{Err: errors.New("连不上")}
	remote = newTestRemote(t, &fakeAgent{queryErr: unavailable})
	if _, err := remote.Sessions(context.Background(), 5, ""); !errors.Is(err, ErrAgentUnavailable) {
		t.Fatalf("不可用的错误 = %v", err)
	}

	// 代理回 5xx 是“代理这一侧坏了”，不是业务拒绝：不能让前端按业务码处理。
	remote = newTestRemote(t, &fakeAgent{queryErr: &agentclient.Error{Status: 500, Code: "INTERNAL_ERROR"}})
	if _, err := remote.Sessions(context.Background(), 5, ""); !errors.Is(err, ErrAgentUnavailable) {
		t.Fatalf("5xx 的错误 = %v", err)
	}
}

// TestSessionMissing 会话不在列表里就是 ErrNotFound（别人的 id 也走这条路）。
func TestSessionMissing(t *testing.T) {
	remote := newTestRemote(t, &fakeAgent{queryRaw: json.RawMessage(
		`{"account":"upanel-shell","account_uid":998,"sessions":[]}`)})
	if _, _, err := remote.Session(context.Background(), 5, "0123456789abcdef0123456789abcdef"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("错误 = %v，期望 ErrNotFound", err)
	}
}

// TestCloseValidatesBeforeCallingAgent 形态不对的 id 不占一次 IPC，也不进代理日志。
func TestCloseValidatesBeforeCallingAgent(t *testing.T) {
	agent := &fakeAgent{}
	remote := newTestRemote(t, agent)
	if err := remote.Close(context.Background(), 5, "bogus", ""); !errors.Is(err, ErrInvalidParams) {
		t.Fatalf("错误 = %v", err)
	}
	if len(agent.closeCalls) != 0 {
		t.Fatalf("代理被调用了 %d 次", len(agent.closeCalls))
	}

	// reason 为空时补成 user（只有请求关闭才会传空）。
	if err := remote.Close(context.Background(), 5, "0123456789abcdef0123456789abcdef", ""); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}
	if len(agent.closeWhy) != 1 || agent.closeWhy[0] != ReasonUser {
		t.Fatalf("reason = %v", agent.closeWhy)
	}
	if agent.closeUsers[0] != 5 {
		t.Fatalf("user_id = %d", agent.closeUsers[0])
	}
}

// TestConnectOrder 先申请许可，再拨号；许可用途与参数必须对上。
func TestConnectOrder(t *testing.T) {
	agent := &fakeAgent{permit: agentclient.StreamPermit{StreamID: "stream-1"}}
	remote := newTestRemote(t, agent)
	// 拨号会失败（没有真的连接），但顺序与参数已经可以断言。
	_, err := remote.Connect(context.Background(), 9, "0123456789abcdef0123456789abcdef")
	if err == nil {
		t.Fatalf("没有连接的拨号应当失败")
	}
	if len(agent.purposes) != 1 || agent.purposes[0] != PurposeIO {
		t.Fatalf("流用途 = %v", agent.purposes)
	}
	if len(agent.dialIDs) != 1 || agent.dialIDs[0] != "stream-1" {
		t.Fatalf("拨号用的流 id = %v", agent.dialIDs)
	}
	if len(agent.dialProtos[0]) != 1 || agent.dialProtos[0][0] != ProtocolName {
		t.Fatalf("子协议 = %v", agent.dialProtos[0])
	}
}

// TestConnectValidatesParams 参数不合法时不申请许可。
func TestConnectValidatesParams(t *testing.T) {
	agent := &fakeAgent{}
	remote := newTestRemote(t, agent)
	if _, err := remote.Connect(context.Background(), 0, "0123456789abcdef0123456789abcdef"); !errors.Is(err, ErrInvalidParams) {
		t.Fatalf("错误 = %v", err)
	}
	if _, err := remote.Connect(context.Background(), 9, "short"); !errors.Is(err, ErrInvalidParams) {
		t.Fatalf("错误 = %v", err)
	}
	if len(agent.purposes) != 0 {
		t.Fatalf("参数不合法却申请了许可")
	}
}

// TestNewRemoteRequiresAgent 没有代理客户端时启动即失败，而不是运行时假装成功。
func TestNewRemoteRequiresAgent(t *testing.T) {
	if _, err := NewRemote(nil, nil); err == nil {
		t.Fatalf("nil 代理被接受了")
	}
}

// probeAgent 在 fakeAgent 之上多一个能力探测，用来验“代理没登记终端”时的软禁用。
type probeAgent struct {
	fakeAgent
	caps    agentclient.Capabilities
	capsErr error
	probes  int
}

func (p *probeAgent) Capabilities(_ context.Context) (agentclient.Capabilities, error) {
	p.probes++
	return p.caps, p.capsErr
}

// TestRemoteDisabledWhenAgentHasNoTerminal 证明“本机没有终端”不是 400 参数错误。
//
// 代理在终端账号不可用时不登记 terminal.create / terminal.sessions；不探测的话，
// Web 会把它翻成代理那句“未登记的查询”，读起来像是调用方把参数写错了。
func TestRemoteDisabledWhenAgentHasNoTerminal(t *testing.T) {
	agent := &probeAgent{caps: agentclient.Capabilities{SupportedQueries: []string{"system.summary"}}}
	remote := newTestRemote(t, agent)

	_, err := remote.Sessions(context.Background(), 5, "")
	if !errors.Is(err, ErrDisabled) {
		t.Fatalf("Sessions 错误 = %v，应为 ErrDisabled", err)
	}
	mapped, ok := HTTPStatusOf(err)
	if !ok || mapped.Status != http.StatusNotImplemented || mapped.Code != httpx.CodeNotImplemented {
		t.Fatalf("判决 = %+v ok=%v，应为 501 NOT_IMPLEMENTED", mapped, ok)
	}
	if len(agent.queries) != 0 {
		t.Fatalf("已判定没有终端能力却仍然调用了代理: %v", agent.queries)
	}

	if _, err := remote.Connect(context.Background(), 5, "0123456789abcdef0123456789abcdef"); !errors.Is(err, ErrDisabled) {
		t.Fatalf("Connect 错误 = %v，应为 ErrDisabled", err)
	}
	if err := remote.Close(context.Background(), 5, "0123456789abcdef0123456789abcdef", ReasonUser); !errors.Is(err, ErrDisabled) {
		t.Fatalf("Close 错误 = %v，应为 ErrDisabled", err)
	}
	if len(agent.purposes) != 0 || len(agent.closeCalls) != 0 {
		t.Fatalf("已判定没有终端能力却仍然申请了许可/关闭")
	}
	if agent.probes != 1 {
		t.Fatalf("能力探测次数 = %d，应为 1（启动时一次）", agent.probes)
	}
}

// TestRemoteEnabledWhenAgentHasTerminal：代理登记了两个名字，终端可用。
func TestRemoteEnabledWhenAgentHasTerminal(t *testing.T) {
	agent := &probeAgent{
		fakeAgent: fakeAgent{queryRaw: json.RawMessage(
			`{"account":"upanel-shell","account_uid":998,"idle_timeout_seconds":900,` +
				`"max_duration_seconds":3600,"disconnect_grace_seconds":60,"max_per_user":1,"max_sessions":2}`)},
		caps: agentclient.Capabilities{
			SupportedQueries: []string{QuerySessions},
			SupportedActions: []string{ActionCreate},
		},
	}
	remote := newTestRemote(t, agent)
	if _, err := remote.Sessions(context.Background(), 5, ""); err != nil {
		t.Fatalf("Sessions 失败: %v", err)
	}
	if len(agent.queries) != 1 {
		t.Fatalf("代理调用次数 = %d", len(agent.queries))
	}
}

// TestRemoteProbeFailureKeepsEnabled 探测失败（代理还没起来）不改变行为：
// 把一次连接失败说成“本机没有终端”会让人去查账号，而问题其实在 socket 上。
func TestRemoteProbeFailureKeepsEnabled(t *testing.T) {
	agent := &probeAgent{
		fakeAgent: fakeAgent{queryRaw: json.RawMessage(
			`{"account":"upanel-shell","account_uid":998,"idle_timeout_seconds":900,` +
				`"max_duration_seconds":3600,"disconnect_grace_seconds":60,"max_per_user":1,"max_sessions":2}`)},
		capsErr: errors.New("socket 不通"),
	}
	remote := newTestRemote(t, agent)
	if _, err := remote.Sessions(context.Background(), 5, ""); err != nil {
		t.Fatalf("Sessions 失败: %v", err)
	}
}
