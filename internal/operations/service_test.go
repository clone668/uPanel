package operations

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"upanel/internal/agentclient"
	"upanel/internal/store"
	"upanel/migrations"
)

// quietLogger 丢弃全部日志：测试只关心返回值与库里的行。
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

// testSessionHash 是一个形态合法的会话令牌摘要。
var testSessionHash = strings.Repeat("cd", 32)

// fakeAgent 是可控的假代理：记录收到的请求，返回预设的计划或错误。
type fakeAgent struct {
	plan  agentclient.PreflightPlan
	err   error
	calls []agentclient.PreflightInput

	// P03.2：操作提交、查询与取消。零值表示“代理返回了一份全零操作”，
	// 只在没配置行为的用例里出现；涉及操作的用例必须显式设 op / opErr。
	op        agentclient.Operation
	opErr     error
	getOp     agentclient.Operation
	getErr    error
	cancelOp  agentclient.Operation
	cancelErr error

	// P03.3：代理侧事件流水。events 按调用顺序整体返回（不分页，代理接口本来就不分页）。
	events    []agentclient.OperationEvent
	eventsErr error

	// P03.4：opHook 在 CreateOperation 里被调用，用来把“提交卡在代理调用中”这个窗口
	// 变成可复现的测试时序（提交与对账并发那条用例需要它）。nil 表示不卡。
	opHook func()

	opCalls     []agentclient.OperationInput
	getCalls    []string
	cancelCalls []string
	eventCalls  []string
}

func (f *fakeAgent) CreatePreflight(_ context.Context, in agentclient.PreflightInput) (agentclient.PreflightPlan, error) {
	f.calls = append(f.calls, in)
	if f.err != nil {
		return agentclient.PreflightPlan{}, f.err
	}
	return f.plan, nil
}

func (f *fakeAgent) CreateOperation(_ context.Context, in agentclient.OperationInput) (agentclient.Operation, error) {
	f.opCalls = append(f.opCalls, in)
	if f.opHook != nil {
		f.opHook()
	}
	if f.opErr != nil {
		return agentclient.Operation{}, f.opErr
	}
	return f.op, nil
}

func (f *fakeAgent) GetOperation(_ context.Context, operationID string) (agentclient.Operation, error) {
	f.getCalls = append(f.getCalls, operationID)
	if f.getErr != nil {
		return agentclient.Operation{}, f.getErr
	}
	return f.getOp, nil
}

func (f *fakeAgent) CancelOperation(_ context.Context, operationID string) (agentclient.Operation, error) {
	f.cancelCalls = append(f.cancelCalls, operationID)
	if f.cancelErr != nil {
		return agentclient.Operation{}, f.cancelErr
	}
	return f.cancelOp, nil
}

func (f *fakeAgent) OperationEvents(_ context.Context, operationID string) ([]agentclient.OperationEvent, error) {
	f.eventCalls = append(f.eventCalls, operationID)
	if f.eventsErr != nil {
		return nil, f.eventsErr
	}
	return f.events, nil
}

// newTestService 构造跑在临时目录真实数据库上的预检服务。
func newTestService(t *testing.T, agent Agent, now func() time.Time) (*Service, *store.DB) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(context.Background(), store.Options{
		Path:   filepath.Join(dir, "upanel.db"),
		Logger: quietLogger(),
	})
	if err != nil {
		t.Fatalf("打开测试数据库失败: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("关闭测试数据库失败: %v", err)
		}
	})
	if _, err := db.Migrate(context.Background(), migrations.FS); err != nil {
		t.Fatalf("数据库迁移失败: %v", err)
	}
	// preflights.user_id 外键指向 users：先建一个账号。
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO users (id, username, password_hash, role, created_at, updated_at)
		 VALUES (7, 'admin', 'x', 'admin', '2026-10-20T08:00:00Z', '2026-10-20T08:00:00Z')`); err != nil {
		t.Fatalf("准备账号失败: %v", err)
	}
	svc, err := New(db, agent, quietLogger(), Options{Now: now})
	if err != nil {
		t.Fatalf("构造服务失败: %v", err)
	}
	return svc, db
}

// fixedNow 返回一个固定时钟与它指向的时间。
func fixedNow() (func() time.Time, time.Time) {
	at := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	return func() time.Time { return at }, at
}

// samplePlan 返回一份代理侧的预检计划。
func samplePlan(now time.Time) agentclient.PreflightPlan {
	return agentclient.PreflightPlan{
		Ref:              "pf_0123456789abcdef0123456789abcdef",
		Action:           "storage.filesystem.format",
		NormalizedParams: json.RawMessage(`{"device":"/dev/sdb","label":"data"}`),
		Target: agentclient.PreflightTarget{
			DisplayName:         "/dev/sdb（ACHI-1234）",
			IdentityFingerprint: "fp-dev-sdb",
			StateVersion:        "7",
		},
		ExpiresAt:      now.Add(120 * time.Second).Format(time.RFC3339),
		Allowed:        true,
		Blockers:       []agentclient.PreflightNotice{},
		Warnings:       []agentclient.PreflightNotice{{Code: "data_loss", Message: "目标上的数据会全部丢失"}},
		Impacts:        []agentclient.PreflightImpact{{Kind: "data_loss", Description: "分区表被重写"}},
		RequiresReauth: true,
		RequiresPhrase: true,
		Cancellable:    false,
		Locks:          []string{"disk:dev_sdb"},
	}
}

func TestCreatePreflightStoresBindings(t *testing.T) {
	nowFn, now := fixedNow()
	agent := &fakeAgent{}
	plan := samplePlan(now)
	plan.ConfirmationPhrase = "FORMAT K7M2QX"
	agent.plan = plan
	svc, db := newTestService(t, agent, nowFn)

	got, err := svc.CreatePreflight(context.Background(), CreateInput{
		UserID:      7,
		SessionHash: testSessionHash,
		ReauthAt:    now.Add(-time.Minute),
		RequestID:   "rid-op",
		Action:      "storage.filesystem.format",
		Params:      json.RawMessage(`{ "label" : "data", "device" : "/dev/sdb" }`),
	})
	if err != nil {
		t.Fatalf("创建预检失败: %v", err)
	}
	if !strings.HasPrefix(got.ID, preflightIDPrefix) || len(got.ID) != len(preflightIDPrefix)+32 {
		t.Fatalf("预检 id 形态不合法: %q", got.ID)
	}
	if got.Action != "storage.filesystem.format" || !got.Allowed {
		t.Fatalf("预检 = %+v", got)
	}
	if got.ConfirmationPhrase != "FORMAT K7M2QX" {
		t.Fatalf("确认短语 = %q", got.ConfirmationPhrase)
	}
	if got.Target.StateVersion != "7" || got.Target.IdentityFingerprint != "fp-dev-sdb" {
		t.Fatalf("目标 = %+v", got.Target)
	}
	if len(got.Warnings) != 1 || len(got.Impacts) != 1 || len(got.Locks) != 1 {
		t.Fatalf("列表字段 = %+v", got)
	}

	// 转发给代理的必须是原样的参数与完整的身份上下文。
	if len(agent.calls) != 1 {
		t.Fatalf("代理被调用 %d 次", len(agent.calls))
	}
	call := agent.calls[0]
	if call.Actor.UserID != 7 || call.Actor.SessionIDHash != testSessionHash || call.Actor.RequestID != "rid-op" {
		t.Fatalf("操作者上下文 = %+v", call.Actor)
	}
	if call.Actor.ReauthAt == nil || *call.Actor.ReauthAt != now.Add(-time.Minute).Format(time.RFC3339) {
		t.Fatalf("再认证时间 = %v", call.Actor.ReauthAt)
	}
	if string(call.Params) != `{ "label" : "data", "device" : "/dev/sdb" }` {
		t.Fatalf("参数被改写: %s", call.Params)
	}

	// 库里必须存下完整绑定：用户、会话、动作、参数哈希、状态版本、代理引用、期限。
	var (
		userID                          int64
		sessionHash, action             string
		paramsHash, stateVersion        string
		agentRef, normalizedParams      string
		targetJSON                      string
		allowed, requiresReauth, phrase int
		createdAt, expiresAt            string
		consumedJob                     *string
	)
	err = db.SQL().QueryRowContext(context.Background(), `
		SELECT user_id, session_hash, action, params_hash, state_version, agent_ref,
		       normalized_params, target_json, allowed, requires_reauth, requires_phrase,
		       created_at, expires_at, consumed_job_id
		  FROM preflights WHERE id = ?`, got.ID).
		Scan(&userID, &sessionHash, &action, &paramsHash, &stateVersion, &agentRef,
			&normalizedParams, &targetJSON, &allowed, &requiresReauth, &phrase,
			&createdAt, &expiresAt, &consumedJob)
	if err != nil {
		t.Fatalf("读取预检行失败: %v", err)
	}
	if userID != 7 || sessionHash != testSessionHash || agentRef != plan.Ref {
		t.Fatalf("绑定列不对: user=%d session=%s ref=%s", userID, sessionHash, agentRef)
	}
	if action != "storage.filesystem.format" || stateVersion != "7" {
		t.Fatalf("动作或状态版本不对: %s / %s", action, stateVersion)
	}
	if allowed != 1 || requiresReauth != 1 || phrase != 1 {
		t.Fatalf("布尔列不对: allowed=%d reauth=%d phrase=%d", allowed, requiresReauth, phrase)
	}
	if consumedJob != nil {
		t.Fatal("新预检不应有消费任务")
	}
	// 参数入库前要规范化：哈希按规范化字节算，前端送来的空白差异不该产生两条绑定。
	if normalizedParams != `{"device":"/dev/sdb","label":"data"}` {
		t.Fatalf("规范化参数 = %s", normalizedParams)
	}
	wantHash := hashHex([]byte(normalizedParams))
	if paramsHash != wantHash {
		t.Fatalf("参数哈希 = %s，期望 %s", paramsHash, wantHash)
	}
	var target map[string]string
	if err := json.Unmarshal([]byte(targetJSON), &target); err != nil {
		t.Fatalf("目标 JSON 不合法: %v", err)
	}
	if target["state_version"] != "7" || target["identity_fingerprint"] != "fp-dev-sdb" {
		t.Fatalf("目标 JSON = %v", target)
	}
	if createdAt != now.Format(time.RFC3339) {
		t.Fatalf("创建时间 = %s", createdAt)
	}
	if expiresAt != plan.ExpiresAt {
		t.Fatalf("到期时间 = %s，期望 %s（代理给的时间戳是唯一真值）", expiresAt, plan.ExpiresAt)
	}
}

func TestCreatePreflightRejectsBadInput(t *testing.T) {
	nowFn, now := fixedNow()
	agent := &fakeAgent{plan: samplePlan(now)}
	svc, db := newTestService(t, agent, nowFn)

	base := CreateInput{
		UserID:      7,
		SessionHash: testSessionHash,
		Action:      "storage.filesystem.format",
		Params:      json.RawMessage(`{}`),
	}

	cases := []struct {
		name   string
		mutate func(*CreateInput)
		want   error
	}{
		{"空动作名", func(in *CreateInput) { in.Action = "" }, ErrInvalidAction},
		{"动作名太长大写", func(in *CreateInput) { in.Action = strings.Repeat("A", 100) }, ErrInvalidAction},
		{"动作名没有点", func(in *CreateInput) { in.Action = "format" }, ErrInvalidAction},
		{"动作名含连字符", func(in *CreateInput) { in.Action = "storage.file-system.format" }, ErrInvalidAction},
		{"用户为 0", func(in *CreateInput) { in.UserID = 0 }, nil},
		{"会话哈希长度不对", func(in *CreateInput) { in.SessionHash = "abc" }, nil},
		{"参数不是对象", func(in *CreateInput) { in.Params = json.RawMessage(`[]`) }, ErrParamsNotObject},
		{"参数不是 JSON", func(in *CreateInput) { in.Params = json.RawMessage(`{`) }, ErrParamsNotObject},
		{"参数过大", func(in *CreateInput) {
			in.Params = json.RawMessage(`{"x":"` + strings.Repeat("a", MaxParamsBytes) + `"}`)
		}, ErrParamsTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := base
			tc.mutate(&in)
			_, err := svc.CreatePreflight(context.Background(), in)
			if err == nil {
				t.Fatal("必须报错")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("错误 = %v，期望 %v", err, tc.want)
			}
		})
	}

	// 参数校验失败的请求不该打到代理，也不该落库。
	for _, call := range agent.calls {
		if string(call.Params) == `[]` {
			t.Fatal("非法参数不应转发给代理")
		}
	}
	var count int
	if err := db.SQL().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM preflights`).Scan(&count); err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if count != 0 {
		t.Fatalf("被拒绝的请求不应落库，实际 %d 条", count)
	}
}

func TestCreatePreflightWithoutAgentIsUnavailable(t *testing.T) {
	nowFn, _ := fixedNow()
	svc, _ := newTestService(t, nil, nowFn)
	_, err := svc.CreatePreflight(context.Background(), CreateInput{
		UserID:      7,
		SessionHash: testSessionHash,
		Action:      "storage.filesystem.format",
	})
	if !errors.Is(err, ErrAgentUnavailable) {
		t.Fatalf("错误 = %v，期望 ErrAgentUnavailable", err)
	}
}

func TestCreatePreflightPassesAgentErrors(t *testing.T) {
	nowFn, _ := fixedNow()
	business := &fakeAgent{err: &agentclient.Error{Status: 400, Code: "VALIDATION_ERROR", Message: "未登记的动作"}}
	svc, db := newTestService(t, business, nowFn)

	_, err := svc.CreatePreflight(context.Background(), CreateInput{
		UserID:      7,
		SessionHash: testSessionHash,
		Action:      "storage.filesystem.format",
	})
	var agentErr *agentclient.Error
	if !errors.As(err, &agentErr) {
		t.Fatalf("错误类型 = %T，期望 *agentclient.Error", err)
	}
	if errors.Is(err, ErrAgentUnavailable) {
		t.Fatal("业务拒绝不能被翻译成代理不可用")
	}

	unavailable := &fakeAgent{err: &agentclient.UnavailableError{Err: errors.New("连接被拒绝")}}
	svc, _ = newTestService(t, unavailable, nowFn)
	_, err = svc.CreatePreflight(context.Background(), CreateInput{
		UserID:      7,
		SessionHash: testSessionHash,
		Action:      "storage.filesystem.format",
	})
	var un *agentclient.UnavailableError
	if !errors.As(err, &un) {
		t.Fatalf("错误类型 = %T，期望 *agentclient.UnavailableError", err)
	}

	var count int
	if err := db.SQL().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM preflights`).Scan(&count); err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if count != 0 {
		t.Fatalf("代理失败时不能留下预检记录，实际 %d 条", count)
	}
}

func TestCreatePreflightRejectsSuspiciousExpiry(t *testing.T) {
	nowFn, now := fixedNow()

	cases := []struct {
		name      string
		expiresAt string
	}{
		{"超过 120 秒", now.Add(10 * time.Minute).Format(time.RFC3339)},
		{"已经过期", now.Add(-time.Second).Format(time.RFC3339)},
		{"不是 RFC3339", "十分钟后"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := samplePlan(now)
			plan.ExpiresAt = tc.expiresAt
			svc, db := newTestService(t, &fakeAgent{plan: plan}, nowFn)
			_, err := svc.CreatePreflight(context.Background(), CreateInput{
				UserID:      7,
				SessionHash: testSessionHash,
				Action:      "storage.filesystem.format",
			})
			if !errors.Is(err, ErrInvalidPlan) {
				t.Fatalf("错误 = %v，期望 ErrInvalidPlan", err)
			}
			var count int
			if err := db.SQL().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM preflights`).Scan(&count); err != nil {
				t.Fatalf("统计失败: %v", err)
			}
			if count != 0 {
				t.Fatal("不合规的期限不能落库")
			}
		})
	}
}

// TestCreatePreflightAcceptsClockSkewWithinAllowance 验证两端时钟差在允许范围内不会被误判。
func TestCreatePreflightAcceptsClockSkewWithinAllowance(t *testing.T) {
	nowFn, now := fixedNow()
	plan := samplePlan(now)
	plan.ExpiresAt = now.Add(PreflightTTL + 3*time.Second).Format(time.RFC3339)
	svc, _ := newTestService(t, &fakeAgent{plan: plan}, nowFn)
	if _, err := svc.CreatePreflight(context.Background(), CreateInput{
		UserID:      7,
		SessionHash: testSessionHash,
		Action:      "storage.filesystem.format",
	}); err != nil {
		t.Fatalf("允许的时钟偏差不应拒绝: %v", err)
	}
}

func TestCreatePreflightEmptyParamsBecomeObject(t *testing.T) {
	nowFn, now := fixedNow()
	agent := &fakeAgent{plan: samplePlan(now)}
	svc, _ := newTestService(t, agent, nowFn)
	if _, err := svc.CreatePreflight(context.Background(), CreateInput{
		UserID:      7,
		SessionHash: testSessionHash,
		Action:      "storage.filesystem.format",
	}); err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	if string(agent.calls[0].Params) != "{}" {
		t.Fatalf("缺省参数应为 {}，实际 %q", agent.calls[0].Params)
	}
}

func TestNewRequiresDatabaseAndLogger(t *testing.T) {
	if _, err := New(nil, nil, quietLogger(), Options{}); err == nil {
		t.Fatal("缺少数据库必须报错")
	}
}

// --- Web 侧门禁（P09.4）---

// fakeGate 是可控的门禁：记录收到的动作与规范化参数，返回预设的阻塞项。
type fakeGate struct {
	blockers []Notice
	err      error

	gotAction string
	gotParams string
	calls     int
}

func (g *fakeGate) Blockers(_ context.Context, action string, params json.RawMessage) ([]Notice, error) {
	g.calls++
	g.gotAction = action
	g.gotParams = string(params)
	return g.blockers, g.err
}

// TestGateTightensAllowedPreflight 确认门禁能把“代理说可以”变成“不允许执行”。
func TestGateTightensAllowedPreflight(t *testing.T) {
	nowFn, now := fixedNow()
	agent := &fakeAgent{plan: samplePlan(now)}
	svc, db := newTestService(t, agent, nowFn)
	gate := &fakeGate{blockers: []Notice{{Code: "VOLUME_OFFLINE", Message: "依赖卷当前未挂载"}}}
	svc.gate = gate

	got, err := svc.CreatePreflight(context.Background(), CreateInput{
		UserID:      7,
		SessionHash: testSessionHash,
		Action:      "service.control",
		Params:      json.RawMessage(`{ "operation" : "start", "unit" : "nginx.service" }`),
	})
	if err != nil {
		t.Fatalf("创建预检失败: %v", err)
	}
	if got.Allowed {
		t.Fatal("门禁命中后 allowed 必须为假")
	}
	if len(got.Blockers) != 1 || got.Blockers[0].Code != "VOLUME_OFFLINE" {
		t.Fatalf("阻塞项 = %+v", got.Blockers)
	}
	// 门禁必须看到**规范化**后的参数与动作名：它判断的东西要和代理执行的东西是同一份。
	if gate.gotAction != got.Action {
		t.Fatalf("门禁收到的动作 = %q，预检动作 = %q", gate.gotAction, got.Action)
	}
	if gate.gotParams != string(got.NormalizedParams) {
		t.Fatalf("门禁收到的参数 = %s，规范化参数 = %s", gate.gotParams, got.NormalizedParams)
	}

	// 落库的必须是收紧后的结论：POST /jobs 读的是这一列。
	var allowed int
	if err := db.SQL().QueryRowContext(context.Background(),
		`SELECT allowed FROM preflights WHERE id = ?`, got.ID).Scan(&allowed); err != nil {
		t.Fatalf("读预检记录失败: %v", err)
	}
	if allowed != 0 {
		t.Fatalf("库里的 allowed = %d，门禁命中后必须是 0", allowed)
	}

	// 提交这一步必须真的被拦住，而不是只把 allowed 写在响应里。
	if _, err := svc.SubmitJob(context.Background(), SubmitInput{
		UserID:         7,
		SessionHash:    testSessionHash,
		PreflightID:    got.ID,
		IdempotencyKey: "idem-gate-1",
		ReauthAt:       now.Add(-time.Minute),
	}); !errors.Is(err, ErrPreflightBlocked) {
		t.Fatalf("提交被门禁拦下的预检应返回 ErrPreflightBlocked，实际 %v", err)
	}
}

// TestGateCannotRelaxBlockedPreflight 确认门禁永远不能把拒绝翻成放行。
func TestGateCannotRelaxBlockedPreflight(t *testing.T) {
	nowFn, now := fixedNow()
	plan := samplePlan(now)
	plan.Allowed = false
	plan.Blockers = []agentclient.PreflightNotice{{Code: "SERVICE_PROTECTED", Message: "单元受保护"}}
	svc, _ := newTestService(t, &fakeAgent{plan: plan}, nowFn)
	gate := &fakeGate{} // 门禁无话可说
	svc.gate = gate

	got, err := svc.CreatePreflight(context.Background(), CreateInput{
		UserID:      7,
		SessionHash: testSessionHash,
		Action:      "service.control",
	})
	if err != nil {
		t.Fatalf("创建预检失败: %v", err)
	}
	if got.Allowed {
		t.Fatal("代理拒绝过的预检不能因为门禁没意见就变成可执行")
	}
	if len(got.Blockers) != 1 || got.Blockers[0].Code != "SERVICE_PROTECTED" {
		t.Fatalf("阻塞项 = %+v", got.Blockers)
	}
}

// TestGateErrorFailsPreflight 确认门禁自身故障时不做“当它没拦”。
func TestGateErrorFailsPreflight(t *testing.T) {
	nowFn, now := fixedNow()
	svc, db := newTestService(t, &fakeAgent{plan: samplePlan(now)}, nowFn)
	svc.gate = &fakeGate{err: errors.New("数据库读不出依赖关系")}

	if _, err := svc.CreatePreflight(context.Background(), CreateInput{
		UserID:      7,
		SessionHash: testSessionHash,
		Action:      "service.control",
	}); err == nil {
		t.Fatal("门禁故障时预检必须失败")
	}
	var n int
	if err := db.SQL().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM preflights`).Scan(&n); err != nil {
		t.Fatalf("读预检记录失败: %v", err)
	}
	if n != 0 {
		t.Fatalf("门禁故障时不该落库任何预检，实际 %d 行", n)
	}
}

// TestPreflightAgentTimeoutWiderThanDefault 钉住预检的调用上限。
//
// 这条测试看着像同义反复，钉的是一件真事：预检里有唯一一条慢的系统调用
// （软件包动作的 `apt-get -s`，24.04 上升级模拟实测 15.6 秒）。谁把上限改回
// agentclient.DefaultTimeout，升级预检就会稳定变成 AGENT_UNAVAILABLE——
// P11.2 验收第一轮的原样失败（duration_ms 10162）。
//
// 上限还必须比代理自己的模拟预算（internal/agent 的 assessBudget，60 秒）宽：
// 先说话的是代理，不是 Web 的猜测。
func TestPreflightAgentTimeoutWiderThanDefault(t *testing.T) {
	if PreflightAgentTimeout <= agentclient.DefaultTimeout {
		t.Fatalf("预检上限 %s 不比通用上限 %s 宽：升级预检会被切成代理不可用",
			PreflightAgentTimeout, agentclient.DefaultTimeout)
	}
	if PreflightAgentTimeout <= 60*time.Second {
		t.Fatalf("预检上限 %s 不比代理侧的模拟预算（60 秒）宽：真超时会先说成代理不可用",
			PreflightAgentTimeout)
	}
	if PreflightAgentTimeout >= PreflightTTL {
		t.Fatalf("预检上限 %s 不该到有效期（%s）：等到了计划也过期了",
			PreflightAgentTimeout, PreflightTTL)
	}
}
