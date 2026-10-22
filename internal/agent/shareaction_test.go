package agent

// P08.1 代理侧用例：五个共享/账号动作的登记、参数、策略、预检映射与执行交接。
//
// 这一层不碰系统（真正的 useradd / smbpasswd / setfacl 在 internal/shares 里，用假
// 执行器代替；真机上只在独立 Ubuntu 虚拟机验证）。它要证明中间这一层没有把纪律弄丢：
//
//  1. 参数形状就是契约里的形状：share_id / account_id / version / secret_ref 的形状
//     在解码层被拒，多余字段不会被静默忽略；
//  2. 策略对齐 CONTRACTS 第 5 节：apply/delete/account.* 要再认证，disable 不要；
//     五个动作都不需要确认短语、都不可取消；
//  3. 判决里的 Blockers/Warnings/Impacts/Locks 原样变成预检计划；
//  4. **outcome 原样传递**：share.apply 在 P08.1 是 accepted，代理层绝不能把它
//     写成 verified——那会让任务中心显示“已核验”，而事实是配置还没写进 Samba。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"upanel/internal/shares"
)

// fakeShares 是 sharesSource 的测试替身。
type fakeShares struct {
	assessment shares.Assessment
	assessErr  error

	report   shares.WriteReport
	writeErr error

	capability shares.Capability
	records    []shares.ShareRecord
	accounts   []shares.AccountRecord
	listErr    error

	// 连接状态（P08.2）：读 smbstatus，与登记表无关。
	connections shares.ConnectionsReport
	connErr     error
	connIDs     []string

	// 记录调用，验证代理层没有偷偷改入参或动作名。
	actions []string
	params  []any
	writes  []shares.WriteRequest
}

func (f *fakeShares) Assess(_ context.Context, action string, params any) (shares.Assessment, error) {
	f.actions = append(f.actions, action)
	f.params = append(f.params, params)
	if f.assessErr != nil {
		return shares.Assessment{}, f.assessErr
	}
	return f.assessment, nil
}

func (f *fakeShares) ExecuteWrite(_ context.Context, req shares.WriteRequest) (shares.WriteReport, error) {
	f.writes = append(f.writes, req)
	if f.writeErr != nil {
		return shares.WriteReport{}, f.writeErr
	}
	return f.report, nil
}

func (f *fakeShares) Capability(context.Context) shares.Capability { return f.capability }

func (f *fakeShares) Shares(context.Context) ([]shares.ShareRecord, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.records, nil
}

func (f *fakeShares) Accounts(context.Context) ([]shares.AccountRecord, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.accounts, nil
}

func (f *fakeShares) Connections(_ context.Context, shareID string) (shares.ConnectionsReport, error) {
	f.connIDs = append(f.connIDs, shareID)
	if f.connErr != nil {
		return shares.ConnectionsReport{}, f.connErr
	}
	return f.connections, nil
}

const (
	testShareID   = "shr_0123456789abcdef0123456789abcdef"
	testAccountID = "sac_fedcba9876543210fedcba9876543210"
	testSecretRef = "sec_0123456789abcdef0123456789abcdef"
)

// shareAssessment 是一份“放行且要素齐备”的共享判决。
func shareAssessment(action, displayName string) shares.Assessment {
	return shares.Assessment{
		Action:              action,
		DisplayName:         displayName,
		TargetID:            testShareID,
		IdentityFingerprint: "vol:vol_0123456789abcdef0123456789abcdef",
		StateVersion:        "7",
		Allowed:             true,
		Blockers:            []shares.Blocker{},
		Warnings: []shares.Blocker{{
			Code:    shares.NoticeShareConfigNotApplied,
			Message: "本阶段不写 Samba 配置，共享此刻不对外提供访问",
		}},
		Impacts: []shares.Impact{
			{Kind: shares.ImpactConfigChange, Description: "会准备目录并改写它的 POSIX ACL"},
			{Kind: shares.ImpactAccessChange, Description: "只有名单内的账号能访问"},
		},
		Locks: []string{"volume:vol_0123456789abcdef0123456789abcdef", "share:" + testShareID},
	}
}

// sharesRegistry 登记五个共享动作，返回登记表与假判决源。
func sharesRegistry(t *testing.T) (*Registry, *fakeShares) {
	t.Helper()
	src := &fakeShares{assessment: shareAssessment(shares.ActionShareApply, "docs")}
	reg := NewRegistry()
	if err := RegisterShareActions(reg, src); err != nil {
		t.Fatalf("登记共享动作失败: %v", err)
	}
	return reg, src
}

// TestShareActionsRegistered 五个动作都登记了，而且都有执行器。
func TestShareActionsRegistered(t *testing.T) {
	reg, _ := sharesRegistry(t)
	want := []string{
		shares.ActionAccountApply,
		shares.ActionAccountDelete,
		shares.ActionShareApply,
		shares.ActionShareDelete,
		shares.ActionShareDisable,
	}
	if got := reg.ActionNames(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("登记的动作 = %v，期望 %v", got, want)
	}
	for _, name := range want {
		a, _ := reg.Action(name)
		if a.Decode == nil || a.Plan == nil || a.Execute == nil {
			t.Errorf("%s 缺少解码、预检或执行器", name)
		}
	}
	// 能力列表必须与实际执行器一致：列进去就等于承诺能执行。
	if got := reg.ExecutableActionNames(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("ExecutableActionNames = %v，期望 %v", got, want)
	}
}

// TestShareActionPolicies 策略逐条对照 CONTRACTS.md 第 5 节。
func TestShareActionPolicies(t *testing.T) {
	reg, _ := sharesRegistry(t)
	cases := []struct {
		action string
		reauth bool
	}{
		{shares.ActionShareApply, true},
		{shares.ActionShareDisable, false},
		{shares.ActionShareDelete, true},
		{shares.ActionAccountApply, true},
		{shares.ActionAccountDelete, true},
	}
	for _, tc := range cases {
		t.Run(tc.action, func(t *testing.T) {
			a, ok := reg.Action(tc.action)
			if !ok {
				t.Fatalf("动作没登记")
			}
			policy, err := a.resolvePolicy(nil)
			if err != nil {
				t.Fatalf("取策略失败: %v", err)
			}
			if policy.RequiresReauth != tc.reauth {
				t.Errorf("RequiresReauth = %v，期望 %v", policy.RequiresReauth, tc.reauth)
			}
			// 确认短语只给初始化/格式化/删分区这类不可逆操作；共享动作都不可逆性不足。
			if policy.RequiresPhrase {
				t.Error("共享动作不需要确认短语")
			}
			// ACL 整份替换与 passdb 写入都是“一动手就要走到结果”的动作。
			if policy.Cancellable {
				t.Error("共享动作不可取消")
			}
		})
	}
}

// TestShareActionDecode 参数解码：合法输入被规范化，非法输入在形状层被拒。
func TestShareActionDecode(t *testing.T) {
	reg, _ := sharesRegistry(t)

	t.Run("合法输入被规范化", func(t *testing.T) {
		p, err := decodeFor(t, reg, shares.ActionShareApply, `{
			"share_id": null,
			"version": null,
			"definition": {
				"name": "  docs  ",
				"protocol": "smb",
				"volume_id": " vol_0123456789abcdef0123456789abcdef ",
				"relative_path": " docs ",
				"enabled": true,
				"read_only": false,
				"allowed_cidrs": ["10.0.0.0/24"],
				"smb_users": [{"account_id":"`+testAccountID+`","access":"write"}]
			}
		}`)
		if err != nil {
			t.Fatalf("合法参数被拒: %v", err)
		}
		got := p.(shares.ShareApplyParams)
		if got.ShareID != nil || got.Version != nil {
			t.Errorf("新建时 share_id/version 必须是 null：%+v", got)
		}
		if got.Definition.Name != "docs" || got.Definition.RelativePath != "docs" {
			t.Errorf("首尾空白没有去掉：%+v", got.Definition)
		}
		if got.Definition.VolumeID != "vol_0123456789abcdef0123456789abcdef" {
			t.Errorf("volume_id = %q", got.Definition.VolumeID)
		}

		p, err = decodeFor(t, reg, shares.ActionAccountApply,
			`{"account_id":null,"name":" upanel-share-docs ","secret_ref":" `+testSecretRef+` ","disabled":false}`)
		if err != nil {
			t.Fatalf("合法参数被拒: %v", err)
		}
		acct := p.(shares.AccountApplyParams)
		if acct.Name != "upanel-share-docs" || acct.SecretRef != testSecretRef {
			t.Errorf("规范化结果 = %+v", acct)
		}
	})

	bad := []struct {
		name   string
		action string
		raw    string
	}{
		{"缺定义", shares.ActionShareApply, `{"share_id":null,"version":null}`},
		{"定义是 null", shares.ActionShareApply, `{"share_id":null,"version":null,"definition":null}`},
		{"共享标识形状", shares.ActionShareApply,
			`{"share_id":"shr_X","version":"1","definition":{"name":"docs"}}`},
		{"版本是空串", shares.ActionShareApply,
			`{"share_id":"` + testShareID + `","version":"","definition":{"name":"docs"}}`},
		{"多余字段", shares.ActionShareApply,
			`{"share_id":null,"version":null,"definition":{"name":"docs"},"force":true}`},
		{"disable 缺版本", shares.ActionShareDisable, `{"share_id":"` + testShareID + `"}`},
		{"disable 标识形状", shares.ActionShareDisable, `{"share_id":"../etc/passwd","version":"1"}`},
		{"disable 大写标识", shares.ActionShareDisable,
			`{"share_id":"shr_0123456789ABCDEF0123456789abcdef","version":"1"}`},
		{"delete 多余字段", shares.ActionShareDelete,
			`{"share_id":"` + testShareID + `","version":"1","purge_data":true}`},
		{"账号标识形状", shares.ActionAccountApply,
			`{"account_id":"sac_X","name":"alice","secret_ref":"","disabled":false}`},
		{"账号名为空", shares.ActionAccountApply,
			`{"account_id":null,"name":"   ","secret_ref":"","disabled":false}`},
		// 密码永远不从这条路径进来：secret_ref 必须是秘密通道返回的引用。
		{"明文当引用", shares.ActionAccountApply,
			`{"account_id":null,"name":"alice","secret_ref":"hunter2","disabled":false}`},
		{"账号删除缺标识", shares.ActionAccountDelete, `{}`},
		{"账号删除标识形状", shares.ActionAccountDelete, `{"account_id":"sac_1"}`},
		{"尾部内容", shares.ActionShareDisable,
			`{"share_id":"` + testShareID + `","version":"1"}{}`},
		{"不是对象", shares.ActionShareDisable, `["` + testShareID + `"]`},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := decodeFor(t, reg, tc.action, tc.raw); err == nil {
				t.Fatalf("非法参数被接受了: %s", tc.raw)
			}
		})
	}
}

// TestSharePlanMapping 判决原样变成计划：告警、影响、锁一个都不能丢。
func TestSharePlanMapping(t *testing.T) {
	reg, src := sharesRegistry(t)
	a, _ := reg.Action(shares.ActionShareApply)
	params, err := a.Decode(json.RawMessage(`{"share_id":null,"version":null,
		"definition":{"name":"docs","protocol":"smb","volume_id":"vol_0123456789abcdef0123456789abcdef",
		"relative_path":"docs","enabled":true,"read_only":false,
		"allowed_cidrs":["10.0.0.0/24"],
		"smb_users":[{"account_id":"` + testAccountID + `","access":"write"}]}}`))
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	plan, err := a.Plan(t.Context(), PlanInput{Params: params})
	if err != nil {
		t.Fatalf("预检失败: %v", err)
	}
	if len(src.actions) != 1 || src.actions[0] != shares.ActionShareApply {
		t.Fatalf("传给判决层的动作 = %v", src.actions)
	}
	if !plan.Allowed {
		t.Fatalf("判决放行，计划却是拒绝：%+v", plan.Blockers)
	}
	// “配置还没写”必须是告警而不是静默：用户在点之前就该知道共享不对外提供访问。
	var sawConfigNotice bool
	for _, w := range plan.Warnings {
		if w.Code == shares.NoticeShareConfigNotApplied {
			sawConfigNotice = true
		}
	}
	if !sawConfigNotice {
		t.Errorf("计划里缺少 SHARE_CONFIG_NOT_APPLIED 告警：%+v", plan.Warnings)
	}
	if len(plan.Impacts) != 2 {
		t.Errorf("影响数 = %d，期望 2", len(plan.Impacts))
	}
	if len(plan.Locks) != 2 || plan.Target.StateVersion != "7" {
		t.Errorf("锁/状态版本没有透出：%+v", plan)
	}
	if plan.Target.IdentityFingerprint == "" {
		t.Error("身份指纹为空：执行前就无法复核“还是那个卷”")
	}
}

// TestSharePlanBlockersAreNotErrors 判决拒绝不算错误：Blockers 进计划，Allowed=false。
func TestSharePlanBlockersAreNotErrors(t *testing.T) {
	reg, src := sharesRegistry(t)
	src.assessment = shares.Assessment{
		Allowed:  false,
		Blockers: []shares.Blocker{{Code: shares.NoticeVolumeNotMounted, Message: "卷没挂载"}},
	}
	a, _ := reg.Action(shares.ActionShareDisable)
	params, err := a.Decode(json.RawMessage(
		`{"share_id":"` + testShareID + `","version":"1"}`))
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	plan, err := a.Plan(t.Context(), PlanInput{Params: params})
	if err != nil {
		t.Fatalf("判决拒绝不该变成错误: %v", err)
	}
	if plan.Allowed {
		t.Fatal("判决 Blockers 非空却放行")
	}
	if len(plan.Blockers) != 1 || plan.Blockers[0].Code != shares.NoticeVolumeNotMounted {
		t.Fatalf("阻塞项 = %+v", plan.Blockers)
	}
}

// TestSharePlanPropagatesAssessError 判决层自己出错才是错误（且带 ErrPlanFailed）。
func TestSharePlanPropagatesAssessError(t *testing.T) {
	reg, src := sharesRegistry(t)
	src.assessErr = errors.New("登记表打不开")
	a, _ := reg.Action(shares.ActionShareDisable)
	params, _ := a.Decode(json.RawMessage(
		`{"share_id":"` + testShareID + `","version":"1"}`))
	if _, err := a.Plan(t.Context(), PlanInput{Params: params}); !errors.Is(err, ErrPlanFailed) {
		t.Fatalf("错误 = %v，期望 ErrPlanFailed", err)
	}
}

// TestShareExecutorPassesAcceptedThrough 是本阶段最要紧的一条：
// share.apply 的结果是 accepted，代理层**不能**把它写成 verified。
func TestShareExecutorPassesAcceptedThrough(t *testing.T) {
	reg, src := sharesRegistry(t)
	src.report = shares.WriteReport{
		Outcome: shares.OutcomeAccepted,
		Observed: map[string]any{
			"samba_config": "not_written",
			"served":       false,
		},
		Data:         map[string]any{"share_id": testShareID, "version": "2"},
		RecoveryHint: "P08.2 才会写入配置并重载",
	}
	a, _ := reg.Action(shares.ActionShareApply)
	params, _ := a.Decode(json.RawMessage(`{"share_id":null,"version":null,
		"definition":{"name":"docs","protocol":"smb","volume_id":"vol_0123456789abcdef0123456789abcdef",
		"relative_path":"docs","enabled":true,"read_only":false,
		"allowed_cidrs":["10.0.0.0/24"],
		"smb_users":[{"account_id":"` + testAccountID + `","access":"write"}]}}`))

	out, err := a.Execute(t.Context(), ExecuteInput{
		Params:      params,
		OperationID: "op_abc",
		Target: Target{
			IdentityFingerprint: "vol:vol_0123456789abcdef0123456789abcdef",
			StateVersion:        "7",
		},
	})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if out.Outcome != OutcomeAccepted {
		t.Fatalf("outcome = %q，share.apply 在 P08.1 只能是 accepted", out.Outcome)
	}
	// accepted 不能带核验时间：时间戳本身就是“已核验”的那半个承诺。
	if !out.VerifiedAt.IsZero() {
		t.Fatalf("accepted 却带了 verified_at = %v", out.VerifiedAt)
	}
	if out.ObservedState["served"] != false || out.ObservedState["samba_config"] != "not_written" {
		t.Errorf("observed_state = %+v", out.ObservedState)
	}
	if out.RecoveryHint == "" {
		t.Error("accepted 必须说明“还差什么才算完成”")
	}

	// 交接给执行器的请求必须带着预检绑定的身份与版本。
	if len(src.writes) != 1 {
		t.Fatalf("执行器调用次数 = %d", len(src.writes))
	}
	w := src.writes[0]
	if w.ExpectedIdentity == "" || w.ExpectedStateVersion != "7" || w.OperationID != "op_abc" {
		t.Errorf("执行请求丢了预检绑定：%+v", w)
	}
	if w.Action != shares.ActionShareApply {
		t.Errorf("动作名 = %q", w.Action)
	}
}

// TestShareExecutorVerifiedNeedsTimestamp verified 必须带核验时间（账号动作才是 verified）。
func TestShareExecutorVerifiedNeedsTimestamp(t *testing.T) {
	reg, src := sharesRegistry(t)
	src.report = shares.WriteReport{Outcome: shares.OutcomeVerified}
	a, _ := reg.Action(shares.ActionAccountApply)
	params, _ := a.Decode(json.RawMessage(
		`{"account_id":null,"name":"upanel-share-docs","secret_ref":"` + testSecretRef + `","disabled":false}`))
	out, err := a.Execute(t.Context(), ExecuteInput{Params: params})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if out.Outcome != OutcomeVerified || out.VerifiedAt.IsZero() {
		t.Fatalf("verified 必须带 verified_at：%+v", out)
	}
	if err := validateResult(out); err != nil {
		t.Fatalf("结果不合法: %v", err)
	}
}

// TestShareExecutorRejectsUnknownOutcome 未知 outcome 是编程错误，不能猜成 verified。
func TestShareExecutorRejectsUnknownOutcome(t *testing.T) {
	reg, src := sharesRegistry(t)
	src.report = shares.WriteReport{Outcome: "rolled_back"}
	a, _ := reg.Action(shares.ActionShareApply)
	params, _ := a.Decode(json.RawMessage(`{"share_id":null,"version":null,
		"definition":{"name":"docs","protocol":"smb","volume_id":"vol_0123456789abcdef0123456789abcdef",
		"relative_path":"docs","enabled":true,"read_only":false,
		"allowed_cidrs":["10.0.0.0/24"],
		"smb_users":[{"account_id":"` + testAccountID + `","access":"write"}]}}`))
	if _, err := a.Execute(t.Context(), ExecuteInput{Params: params}); !errors.Is(err, ErrInvalidResult) {
		t.Fatalf("错误 = %v，期望 ErrInvalidResult", err)
	}
}

// TestShareActionsNotRegisteredWithoutSource 没有共享能力时不登记动作也不登记查询。
//
// 这是“能力清单不撒谎”的另一半：一台没接共享模块的代理不应该在 capabilities 里
// 出现 share.apply，也不应该对 share.list 回一个空列表。
func TestShareActionsNotRegisteredWithoutSource(t *testing.T) {
	reg, err := NewProductionRegistry(ProductionDeps{})
	if err != nil {
		t.Fatalf("构造生产登记表失败: %v", err)
	}
	for _, name := range []string{
		shares.ActionShareApply, shares.ActionShareDisable, shares.ActionShareDelete,
		shares.ActionAccountApply, shares.ActionAccountDelete,
	} {
		if _, ok := reg.Action(name); ok {
			t.Errorf("%s 在缺少共享源时被登记了", name)
		}
	}
	for _, name := range []string{shares.QueryCapability, shares.QueryShares, shares.QueryAccounts} {
		if _, ok := reg.Query(name); ok {
			t.Errorf("%s 在缺少共享源时被登记了", name)
		}
	}
}

// TestShareQueriesAndActionsComeInPairs 有源时五个动作与三条查询成对出现。
func TestShareQueriesAndActionsComeInPairs(t *testing.T) {
	src := &fakeShares{capability: shares.Capability{Status: shares.CapabilityAvailable}}
	reg, err := NewProductionRegistry(ProductionDeps{Shares: src})
	if err != nil {
		t.Fatalf("构造生产登记表失败: %v", err)
	}
	for _, name := range []string{shares.QueryCapability, shares.QueryShares, shares.QueryAccounts} {
		if _, ok := reg.Query(name); !ok {
			t.Errorf("查询 %s 没登记", name)
		}
	}
	for _, name := range []string{
		shares.ActionShareApply, shares.ActionShareDisable, shares.ActionShareDelete,
		shares.ActionAccountApply, shares.ActionAccountDelete,
	} {
		if _, ok := reg.Action(name); !ok {
			t.Errorf("动作 %s 没登记", name)
		}
	}
}
