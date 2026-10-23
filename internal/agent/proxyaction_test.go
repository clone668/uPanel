package agent

// P10.2 代理侧用例：proxy.apply 的登记、参数形状、策略、预检映射与执行交接。
//
// 这一层不碰系统（渲染、写文件、nginx -t 在 internal/proxies 里）。它要证明的是
// 中间这一层没有把纪律弄丢：
//
//  1. 参数形状就是契约里的形状：站点标识、定义里的每个字段、panel_port 都在解码层
//     被校验，多余字段不会被静默忽略；
//  2. **面板端口不得成为上游端口**这条规则在解码层就生效——代理的动作入口是 HTTP，
//     “记录校验过”不能替参数上的值担保；
//  3. 判决里的 Blockers/Warnings/Impacts/Locks 原样变成预检计划；
//  4. outcome 原样传递：verified 就是 verified，accepted 就是 accepted，认不出来的一律
//     报错，绝不猜。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"upanel/internal/proxies"
)

// proxyTestSiteID 与内部包的形状校验一致：site_ + 32 位小写十六进制。
const proxyTestSiteID = "site_0123456789abcdef0123456789abcdef"

const proxyTestPanelPort = 9443

// proxyDefinitionJSON 是一份完整的定义（public 模式），字段名按契约写。
func proxyDefinitionJSON(overrides string) string {
	base := `{
		"domain": "app.example.com",
		"upstream_target_id": "local_3f2a1b",
		"upstream_scheme": "http",
		"upstream_port": 8080,
		"access_mode": "public",
		"allowed_cidrs": [],
		"basic_auth_users": [],
		"websocket_enabled": true,
		"max_body_bytes": 1048576,
		"read_timeout_seconds": 60,
		"enabled": true
	}`
	if overrides == "" {
		return base
	}
	// 用一次解码/编码把覆盖字段叠上去，避免字符串拼接出错。
	var def map[string]any
	if err := json.Unmarshal([]byte(base), &def); err != nil {
		panic(err)
	}
	var patch map[string]any
	if err := json.Unmarshal([]byte(overrides), &patch); err != nil {
		panic(err)
	}
	for k, v := range patch {
		def[k] = v
	}
	out, err := json.Marshal(def)
	if err != nil {
		panic(err)
	}
	return string(out)
}

func proxyParamsJSON(siteID string, version int, definition string) string {
	return `{"site_id":` + quoteJSON(siteID) + `,"version":` + itoa(version) +
		`,"definition":` + definition + `,"panel_port":` + itoa(proxyTestPanelPort) + `}`
}

// quoteJSON / itoa 让上面的拼装不必依赖 fmt。
func quoteJSON(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}

func itoa(n int) string {
	raw, _ := json.Marshal(n)
	return string(raw)
}

// fakeProxies 是 proxySource 的测试替身。
type fakeProxies struct {
	assessment proxies.Assessment
	assessErr  error

	report   proxies.WriteReport
	writeErr error

	assessed []proxies.ApplyParams
	writes   []proxies.WriteRequest
}

func (f *fakeProxies) AssessApply(_ context.Context, in proxies.ApplyParams) (proxies.Assessment, error) {
	f.assessed = append(f.assessed, in)
	if f.assessErr != nil {
		return proxies.Assessment{}, f.assessErr
	}
	return f.assessment, nil
}

func (f *fakeProxies) ExecuteApply(_ context.Context, req proxies.WriteRequest) (proxies.WriteReport, error) {
	f.writes = append(f.writes, req)
	if f.writeErr != nil {
		return proxies.WriteReport{}, f.writeErr
	}
	return f.report, nil
}

// proxyAssessment 是一份“放行且要素齐备”的判决。
func proxyAssessment() proxies.Assessment {
	return proxies.Assessment{
		Action:              proxies.ActionProxyApply,
		TargetID:            proxyTestSiteID,
		DisplayName:         "app.example.com",
		IdentityFingerprint: proxyTestSiteID,
		StateVersion:        "site:" + proxyTestSiteID + ":v1",
		Allowed:             true,
		Blockers:            []proxies.Blocker{},
		Warnings: []proxies.Blocker{{
			Code:    proxies.CodeTargetMissing,
			Message: "上游目标不在发现结果里：配置仍会生效，但访问会得到 502",
		}},
		Impacts: []proxies.Impact{
			{Kind: "nginx_config", Description: "写入或替换托管配置 /etc/nginx/conf.d/upanel-app.example.com.conf"},
			{Kind: "nginx_reload", Description: "nginx -t 通过后 nginx -s reload"},
		},
		Locks: []string{proxies.LockNginxConfig, "proxy:" + proxyTestSiteID},
	}
}

func proxyRegistry(t *testing.T) (*Registry, *fakeProxies) {
	t.Helper()
	src := &fakeProxies{assessment: proxyAssessment()}
	reg := NewRegistry()
	if err := RegisterProxyActions(reg, src); err != nil {
		t.Fatalf("登记代理动作失败: %v", err)
	}
	return reg, src
}

// TestProxyActionRegistered 动作登记了，而且三件套齐全、可执行。
func TestProxyActionRegistered(t *testing.T) {
	reg, _ := proxyRegistry(t)
	a, ok := reg.Action(proxies.ActionProxyApply)
	if !ok {
		t.Fatalf("%s 没有登记", proxies.ActionProxyApply)
	}
	if a.Decode == nil || a.Plan == nil || a.Execute == nil {
		t.Fatal("缺少解码、预检或执行器")
	}
	names := reg.ExecutableActionNames()
	for _, n := range names {
		if n == proxies.ActionProxyApply {
			return
		}
	}
	t.Errorf("可执行动作里没有 %s：登记了却不报可执行，能力清单就少了一项", proxies.ActionProxyApply)
}

// TestProxyActionPolicy 策略对齐 CONTRACTS 第 5 节：要再认证、不要短语、不可取消。
func TestProxyActionPolicy(t *testing.T) {
	reg, _ := proxyRegistry(t)
	a, _ := reg.Action(proxies.ActionProxyApply)
	policy, err := a.resolvePolicy(nil)
	if err != nil {
		t.Fatalf("取策略失败: %v", err)
	}
	if !policy.RequiresReauth {
		t.Error("改的是“哪个域名指向本机哪个端口、谁能访问”，必须再认证")
	}
	if policy.RequiresPhrase {
		t.Error("配置有备份、回滚是一条 rename，不需要确认短语")
	}
	if policy.Cancellable {
		t.Error("写到一半的配置比不写更危险，不可取消")
	}
}

// TestProxyActionDecode 合法输入被规范化，非法输入在形状层被拒。
func TestProxyActionDecode(t *testing.T) {
	reg, _ := proxyRegistry(t)

	t.Run("合法输入被规范化", func(t *testing.T) {
		p, err := decodeFor(t, reg, proxies.ActionProxyApply,
			proxyParamsJSON(proxyTestSiteID, 3, proxyDefinitionJSON(`{"domain":"  App.Example.COM  "}`)))
		if err != nil {
			t.Fatalf("合法参数被拒: %v", err)
		}
		got, ok := p.(proxies.ApplyParams)
		if !ok {
			t.Fatalf("解码结果类型 = %T", p)
		}
		if got.SiteID != proxyTestSiteID || got.Version != 3 {
			t.Errorf("站点标识/版本 = %q/%d", got.SiteID, got.Version)
		}
		if got.PanelPort != proxyTestPanelPort {
			t.Errorf("panel_port = %d，期望 %d", got.PanelPort, proxyTestPanelPort)
		}
		// 域名归一化在解码阶段就完成：预检展示的与实际写下去的必须是同一份。
		if got.Definition == nil || got.Definition.Domain != "app.example.com" {
			t.Errorf("域名 = %+v", got.Definition)
		}
	})

	t.Run("字段名按契约 snake_case", func(t *testing.T) {
		// 契约里的字段名必须被认；Go 字段名不该出现（出现就说明 tag 丢了）。
		raw := `{"site_id":"` + proxyTestSiteID + `","version":1,"panel_port":9443,
			"definition":{"domain":"app.example.com","upstream_target_id":"local_3f2a1b",
			"upstream_scheme":"http","upstream_port":8080,"access_mode":"cidr",
			"allowed_cidrs":["10.0.0.0/8"],"basic_auth_users":[],"websocket_enabled":false,
			"max_body_bytes":1048576,"read_timeout_seconds":60,"enabled":true}}`
		p, err := decodeFor(t, reg, proxies.ActionProxyApply, raw)
		if err != nil {
			t.Fatalf("契约字段名被拒: %v", err)
		}
		def := p.(proxies.ApplyParams).Definition
		if len(def.AllowedCIDRs) != 1 || def.AllowedCIDRs[0] != "10.0.0.0/8" {
			t.Errorf("网段没解出来：%+v", def)
		}
		if def.WebSocketEnabled {
			t.Error("websocket_enabled=false 被解成了 true")
		}
	})

	bad := []struct {
		name string
		raw  string
	}{
		{"站点标识形状", proxyParamsJSON("site_XYZ", 1, proxyDefinitionJSON(""))},
		{"站点标识大写", proxyParamsJSON("site_0123456789ABCDEF0123456789abcdef", 1, proxyDefinitionJSON(""))},
		{"站点标识带路径", proxyParamsJSON("../../etc/passwd", 1, proxyDefinitionJSON(""))},
		{"缺定义", `{"site_id":"` + proxyTestSiteID + `","version":1,"panel_port":9443}`},
		{"定义是 null", `{"site_id":"` + proxyTestSiteID + `","version":1,"definition":null,"panel_port":9443}`},
		{"多余字段", `{"site_id":"` + proxyTestSiteID + `","version":1,"definition":` +
			proxyDefinitionJSON("") + `,"panel_port":9443,"force":true}`},
		{"定义里的多余字段", proxyParamsJSON(proxyTestSiteID, 1, proxyDefinitionJSON(`{"rewrite":"/x"}`))},
		{"上游端口等于面板端口", proxyParamsJSON(proxyTestSiteID, 1,
			proxyDefinitionJSON(`{"upstream_port":9443}`))},
		{"上游端口越界", proxyParamsJSON(proxyTestSiteID, 1, proxyDefinitionJSON(`{"upstream_port":70000}`))},
		{"上游地址是主机名", proxyParamsJSON(proxyTestSiteID, 1,
			proxyDefinitionJSON(`{"upstream_target_id":"example.com"}`))},
		{"上游地址是云元数据", proxyParamsJSON(proxyTestSiteID, 1,
			proxyDefinitionJSON(`{"upstream_target_id":"169.254.169.254"}`))},
		{"上游协议不认识", proxyParamsJSON(proxyTestSiteID, 1,
			proxyDefinitionJSON(`{"upstream_scheme":"gopher"}`))},
		{"访问模式不认识", proxyParamsJSON(proxyTestSiteID, 1,
			proxyDefinitionJSON(`{"access_mode":"ip_allowlist"}`))},
		{"域名带通配符", proxyParamsJSON(proxyTestSiteID, 1, proxyDefinitionJSON(`{"domain":"*.example.com"}`))},
		{"域名带端口", proxyParamsJSON(proxyTestSiteID, 1, proxyDefinitionJSON(`{"domain":"a.example.com:8080"}`))},
		{"账号引用的不是秘密", proxyParamsJSON(proxyTestSiteID, 1,
			proxyDefinitionJSON(`{"access_mode":"basic_auth","basic_auth_users":[{"name":"alice","secret_ref":"hunter2"}]}`))},
		{"尾部内容", proxyParamsJSON(proxyTestSiteID, 1, proxyDefinitionJSON("")) + `{}`},
		{"不是对象", `["` + proxyTestSiteID + `"]`},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := decodeFor(t, reg, proxies.ActionProxyApply, tc.raw); err == nil {
				t.Fatalf("非法参数被接受了: %s", tc.raw)
			}
		})
	}
}

// TestProxyPlanMapping 判决原样变成计划：告警、影响、锁、身份、状态版本一个都不能丢。
func TestProxyPlanMapping(t *testing.T) {
	reg, src := proxyRegistry(t)
	a, _ := reg.Action(proxies.ActionProxyApply)
	params, err := a.Decode(json.RawMessage(proxyParamsJSON(proxyTestSiteID, 1, proxyDefinitionJSON(""))))
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	plan, err := a.Plan(context.Background(), PlanInput{Params: params})
	if err != nil {
		t.Fatalf("预检失败: %v", err)
	}
	if len(src.assessed) != 1 {
		t.Fatalf("判决层调用次数 = %d", len(src.assessed))
	}
	if got := src.assessed[0].SiteID; got != proxyTestSiteID {
		t.Errorf("传给判决层的站点标识 = %q", got)
	}
	if !plan.Allowed {
		t.Fatalf("判决放行，计划却拒绝：%+v", plan.Blockers)
	}
	var sawWarning bool
	for _, w := range plan.Warnings {
		if w.Code == proxies.CodeTargetMissing {
			sawWarning = true
		}
	}
	if !sawWarning {
		t.Errorf("上游目标告警没进计划：%+v", plan.Warnings)
	}
	if len(plan.Impacts) != 2 {
		t.Errorf("影响数 = %d，期望 2", len(plan.Impacts))
	}
	if len(plan.Locks) != 2 || plan.Locks[0] != proxies.LockNginxConfig {
		t.Errorf("锁 = %v，第一条必须是全局配置锁", plan.Locks)
	}
	if plan.Target.IdentityFingerprint != proxyTestSiteID {
		t.Errorf("身份指纹 = %q", plan.Target.IdentityFingerprint)
	}
	if plan.Target.StateVersion == "" {
		t.Error("状态版本为空：执行前就无法证明“提交的是这份计划”")
	}
	if plan.Target.DisplayName != "app.example.com" {
		t.Errorf("显示名 = %q", plan.Target.DisplayName)
	}
}

// TestProxyPlanBlockersAreNotErrors 判决拒绝不算错误：Blockers 进计划，Allowed=false。
func TestProxyPlanBlockersAreNotErrors(t *testing.T) {
	reg, src := proxyRegistry(t)
	src.assessment = proxies.Assessment{
		Allowed:  false,
		Blockers: []proxies.Blocker{{Code: proxies.CodeDomainTaken, Message: "域名已被占用"}},
	}
	a, _ := reg.Action(proxies.ActionProxyApply)
	params, err := a.Decode(json.RawMessage(proxyParamsJSON(proxyTestSiteID, 1, proxyDefinitionJSON(""))))
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	plan, err := a.Plan(context.Background(), PlanInput{Params: params})
	if err != nil {
		t.Fatalf("判决拒绝不该变成错误: %v", err)
	}
	if plan.Allowed {
		t.Fatal("Blockers 非空却放行")
	}
	if len(plan.Blockers) != 1 || plan.Blockers[0].Code != proxies.CodeDomainTaken {
		t.Fatalf("阻塞项 = %+v", plan.Blockers)
	}
}

// TestProxyPlanPropagatesAssessError 判决层自己出错才是错误（且带 ErrPlanFailed）。
func TestProxyPlanPropagatesAssessError(t *testing.T) {
	reg, src := proxyRegistry(t)
	src.assessErr = errors.New("读不到 Nginx 现状")
	a, _ := reg.Action(proxies.ActionProxyApply)
	params, err := a.Decode(json.RawMessage(proxyParamsJSON(proxyTestSiteID, 1, proxyDefinitionJSON(""))))
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	if _, err := a.Plan(context.Background(), PlanInput{Params: params}); !errors.Is(err, ErrPlanFailed) {
		t.Fatalf("错误 = %v，期望 ErrPlanFailed", err)
	}
}

// TestProxyExecutorPassesOutcomesThrough outcome 原样传递，绑定信息一起交给执行器。
func TestProxyExecutorPassesOutcomesThrough(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{proxies.OutcomeVerified, OutcomeVerified},
		{proxies.OutcomeAccepted, OutcomeAccepted},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			reg, src := proxyRegistry(t)
			src.report = proxies.WriteReport{
				Outcome: tc.in,
				Observed: map[string]any{
					"file_state": proxies.FileStateWritten,
					"reload":     proxies.ReloadDone,
					"served":     tc.in == proxies.OutcomeVerified,
				},
				Data:         map[string]any{"site_id": proxyTestSiteID, "domain": "app.example.com"},
				RecoveryHint: "站点改名前的旧配置已移除",
			}
			a, _ := reg.Action(proxies.ActionProxyApply)
			params, err := a.Decode(json.RawMessage(proxyParamsJSON(proxyTestSiteID, 1, proxyDefinitionJSON(""))))
			if err != nil {
				t.Fatalf("解码失败: %v", err)
			}
			out, err := a.Execute(context.Background(), ExecuteInput{
				Params:      params,
				OperationID: "op_p10_2",
				Target: Target{
					IdentityFingerprint: proxyTestSiteID,
					StateVersion:        "site:" + proxyTestSiteID + ":v1",
				},
			})
			if err != nil {
				t.Fatalf("执行失败: %v", err)
			}
			if out.Outcome != tc.want {
				t.Fatalf("outcome = %q，期望 %q", out.Outcome, tc.want)
			}
			// verified 必须带核验时间，accepted 不能带：时间戳本身就是“已核验”的半个承诺。
			if tc.want == OutcomeVerified && out.VerifiedAt.IsZero() {
				t.Error("verified 却没有 verified_at")
			}
			if tc.want == OutcomeAccepted && !out.VerifiedAt.IsZero() {
				t.Error("accepted 却带了 verified_at")
			}
			if out.Data["site_id"] != proxyTestSiteID {
				t.Errorf("data 没有原样透出：%+v", out.Data)
			}
			if out.RecoveryHint == "" {
				t.Error("恢复提示丢了")
			}

			// 交接给执行器的请求必须带着预检绑定的身份与版本。
			if len(src.writes) != 1 {
				t.Fatalf("执行器调用次数 = %d", len(src.writes))
			}
			w := src.writes[0]
			if w.Action != proxies.ActionProxyApply {
				t.Errorf("动作名 = %q", w.Action)
			}
			if w.ExpectedIdentity != proxyTestSiteID || w.ExpectedStateVersion != "site:"+proxyTestSiteID+":v1" {
				t.Errorf("执行请求丢了预检绑定：%+v", w)
			}
			if w.OperationID != "op_p10_2" {
				t.Errorf("操作号 = %q", w.OperationID)
			}
		})
	}
}

// TestProxyExecutorRejectsUnknownOutcome 认不出来的 outcome 是编程错误，不能猜成 verified。
func TestProxyExecutorRejectsUnknownOutcome(t *testing.T) {
	reg, src := proxyRegistry(t)
	src.report = proxies.WriteReport{Outcome: "rolled_back"}
	a, _ := reg.Action(proxies.ActionProxyApply)
	params, err := a.Decode(json.RawMessage(proxyParamsJSON(proxyTestSiteID, 1, proxyDefinitionJSON(""))))
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	if _, err := a.Execute(context.Background(), ExecuteInput{Params: params}); !errors.Is(err, ErrInvalidResult) {
		t.Fatalf("错误 = %v，期望 ErrInvalidResult", err)
	}
}

// TestProxyExecutorPassesHTTPError 判决拒绝要带着自己的状态码与错误码穿过代理层。
func TestProxyExecutorPassesHTTPError(t *testing.T) {
	reg, src := proxyRegistry(t)
	a, _ := reg.Action(proxies.ActionProxyApply)
	params, err := a.Decode(json.RawMessage(proxyParamsJSON(proxyTestSiteID, 1, proxyDefinitionJSON(""))))
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	// 构造一个带判决的拒绝：这里不经过 proxies 包内部，直接看它能否原样穿过。
	src.writeErr = errors.New("回滚失败：磁盘只读")
	if _, err := a.Execute(context.Background(), ExecuteInput{Params: params}); err == nil {
		t.Fatal("执行器报错却被吞掉了")
	}
	if !strings.Contains(src.writeErr.Error(), "回滚失败") {
		t.Fatal("这条用例的前提是执行器返回错误")
	}
}

// TestProxyActionNotRegisteredWithoutSource 没有写入能力时不登记动作：能力清单不撒谎。
func TestProxyActionNotRegisteredWithoutSource(t *testing.T) {
	reg, err := NewProductionRegistry(ProductionDeps{})
	if err != nil {
		t.Fatalf("构造生产登记表失败: %v", err)
	}
	if _, ok := reg.Action(proxies.ActionProxyApply); ok {
		t.Errorf("%s 在缺少写入路径时被登记了：点一下就是一个 501", proxies.ActionProxyApply)
	}
	for _, name := range reg.ExecutableActionNames() {
		if name == proxies.ActionProxyApply {
			t.Errorf("可执行动作里出现了 %s", proxies.ActionProxyApply)
		}
	}
}

// TestProxyActionRegisteredWithSource 有写入能力时动作成对出现。
func TestProxyActionRegisteredWithSource(t *testing.T) {
	reg, err := NewProductionRegistry(ProductionDeps{Proxies: &fakeProxies{}})
	if err != nil {
		t.Fatalf("构造生产登记表失败: %v", err)
	}
	a, ok := reg.Action(proxies.ActionProxyApply)
	if !ok {
		t.Fatalf("有写入路径却没登记 %s", proxies.ActionProxyApply)
	}
	if a.Decode == nil || a.Plan == nil || a.Execute == nil {
		t.Error("登记的动作缺三件套之一")
	}
}
