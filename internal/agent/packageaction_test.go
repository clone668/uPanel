package agent

// APT 写动作（P11.2）：参数校验、计划映射、策略、结果映射。
//
// 这一层要验证的是"代理有没有给 apt 层添油加醋"。apt 包自己的测试负责
// 锁、模拟解析、保护名单与结果核对；这里验证的是：参数进来的形状、四个动作的
// 准入门槛、以及 outcome 只有 verified / partial 两档。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"upanel/internal/apt"
	"upanel/internal/httpx"
)

// fakeWrites 是 agent 层的假写端。
type fakeWrites struct {
	assessment apt.Assessment
	assessErr  error
	assessed   []string

	execResult apt.ExecResult
	execErr    error
	executed   []string

	refreshResult apt.ExecResult
	refreshErr    error
	refreshed     int
}

func (f *fakeWrites) Assess(_ context.Context, op apt.Op, names []string) (apt.Assessment, error) {
	f.assessed = append(f.assessed, string(op)+"|"+strings.Join(names, ","))
	return f.assessment, f.assessErr
}

func (f *fakeWrites) Execute(_ context.Context, op apt.Op, names []string, progress func(string, map[string]any)) (apt.ExecResult, error) {
	f.executed = append(f.executed, string(op)+"|"+strings.Join(names, ","))
	if progress != nil {
		progress("simulate", map[string]any{"operation": string(op)})
		progress("execute", map[string]any{"operation": string(op)})
		progress("verify", map[string]any{"exit_code": 0})
	}
	return f.execResult, f.execErr
}

func (f *fakeWrites) Refresh(_ context.Context, _ func(string, map[string]any)) (apt.ExecResult, error) {
	f.refreshed++
	return f.refreshResult, f.refreshErr
}

// newFakeWrites 返回一份"预检放行、执行成功且核对通过"的默认值。
func newFakeWrites() *fakeWrites {
	indexAt := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	return &fakeWrites{
		assessment: apt.Assessment{
			Op:             apt.OpInstall,
			DisplayName:    "安装 cowsay",
			Identity:       "sha256:aaaa",
			StateVersion:   "sha256:bbbb",
			Allowed:        true,
			Locks:          []string{"apt-global"},
			IndexUpdatedAt: &indexAt,
			Impacts:        []apt.Impact{{Kind: apt.ImpactSoftware, Description: "将安装 1 个、升级 0 个软件包"}},
		},
		execResult: apt.ExecResult{
			Op:         apt.OpInstall,
			Names:      []string{"cowsay"},
			Verified:   true,
			VerifiedAt: indexAt,
			Observed: map[string]apt.ObservedPackage{
				"cowsay": {Status: "ii", Version: "3.03+dfsg2-8"},
			},
			Plan: apt.Simulation{
				Steps: []apt.PlanStep{{Kind: apt.StepInstall, Package: "cowsay", ToVersion: "3.03+dfsg2-8"}},
			},
		},
		refreshResult: apt.ExecResult{
			Op:         apt.OpRefresh,
			Verified:   true,
			VerifiedAt: indexAt,
		},
	}
}

func newPackageRegistry(t *testing.T, src PackageWrites) *Registry {
	t.Helper()
	reg := NewRegistry()
	if err := RegisterPackageActions(reg, src); err != nil {
		t.Fatalf("登记 package.* 失败: %v", err)
	}
	return reg
}

// TestRegisterPackageActionsNames：四个冻结的动作名必须全部登记，且可执行。
func TestRegisterPackageActionsNames(t *testing.T) {
	reg := newPackageRegistry(t, newFakeWrites())
	want := []string{
		PackageInstallAction, PackageRefreshAction,
		PackageRemoveAction, PackageUpgradeAction,
	}
	for _, name := range want {
		if _, ok := reg.Action(name); !ok {
			t.Fatalf("%s 没有登记", name)
		}
	}
	executable := reg.ExecutableActionNames()
	for _, name := range want {
		if !contains(executable, name) {
			t.Fatalf("%s 应当出现在可执行动作里: %v", name, executable)
		}
	}
}

// TestPackagePolicy：四个动作的准入门槛。
func TestPackagePolicy(t *testing.T) {
	reg := newPackageRegistry(t, newFakeWrites())
	tests := []struct {
		action     string
		reauth     bool
		phrase     bool
		phraseVerb string
	}{
		{PackageRefreshAction, false, false, ""},
		{PackageInstallAction, true, false, ""},
		{PackageUpgradeAction, true, false, ""},
		{PackageRemoveAction, true, true, "REMOVE"},
	}
	for _, tc := range tests {
		action, ok := reg.Action(tc.action)
		if !ok {
			t.Fatalf("%s 没有登记", tc.action)
		}
		policy, err := action.ResolvePolicy(nil)
		if err != nil {
			t.Fatalf("%s 策略不合法: %v", tc.action, err)
		}
		if policy.RequiresReauth != tc.reauth || policy.RequiresPhrase != tc.phrase {
			t.Fatalf("%s 门槛错: %+v", tc.action, policy)
		}
		if policy.PhraseVerb != tc.phraseVerb {
			t.Fatalf("%s 短语首词应为 %q，实际 %q", tc.action, tc.phraseVerb, policy.PhraseVerb)
		}
		// apt 拿到锁之后没有安全中断点，四个动作都不能取消。
		if policy.Cancellable {
			t.Fatalf("%s 不该可取消：apt 没有安全中断点", tc.action)
		}
	}
}

// TestValidatePackageName：包名的合法边界。
func TestValidatePackageName(t *testing.T) {
	good := []string{
		"cowsay", "zlib1g", "libc6", "netcat-openbsd", "python3.12",
		"bind9-libs:amd64", "bash:amd64", "nginx-core", "g++", "a2",
	}
	for _, name := range good {
		if err := ValidatePackageName(name); err != nil {
			t.Fatalf("%q 应当合法: %v", name, err)
		}
	}
	// `apt-get` 不在这个列表里：它是一个**语法合法**的包名（只是不存在）。
	// 名字合法但包不存在由 apt 自己回答（Unable to locate package），
	// 而参数校验只负责"这串字符能不能交给 apt"。
	bad := []string{
		"", "a", "-rf", "--force-yes", "bash -c", "bash\nrm", "../../etc/passwd",
		"Bash", "bash*", "bash=5.2", "bash~1", "bash:", ":amd64", "bash:amd64:all",
		" bash", "bash ", "cowsay;rm",
	}
	for _, name := range bad {
		if err := ValidatePackageName(name); err == nil {
			t.Fatalf("%q 应当被拒绝", name)
		}
	}
}

// TestNormalizePackageNames：去重保序，空数组与超限都拒绝。
func TestNormalizePackageNames(t *testing.T) {
	got, err := normalizePackageNames([]string{"curl", " wget ", "curl"})
	if err != nil {
		t.Fatalf("规范化失败: %v", err)
	}
	if strings.Join(got, ",") != "curl,wget" {
		t.Fatalf("去重保序错: %v", got)
	}
	if _, err := normalizePackageNames(nil); !errors.Is(err, ErrParamsInvalid) {
		t.Fatalf("空数组应被拒绝: %v", err)
	}
	many := make([]string, MaxPackageNames+1)
	for i := range many {
		many[i] = "pkg" + strings.Repeat("x", i%3)
	}
	if _, err := normalizePackageNames(many); !errors.Is(err, ErrParamsInvalid) {
		t.Fatalf("超过上限应被拒绝: %v", err)
	}
}

// TestPackageInstallDecodeStrict：未知字段直接拒绝，不做"忽略继续"。
func TestPackageInstallDecodeStrict(t *testing.T) {
	reg := newPackageRegistry(t, newFakeWrites())
	action, _ := reg.Action(PackageInstallAction)

	if _, err := action.Decode(json.RawMessage(`{"packages":["cowsay"],"allow_remove_essential":true}`)); !errors.Is(err, ErrParamsInvalid) {
		t.Fatalf("未知字段应被拒绝: %v", err)
	}
	if _, err := action.Decode(json.RawMessage(`{"packages":["cowsay"],"dist_upgrade":true}`)); !errors.Is(err, ErrParamsInvalid) {
		t.Fatalf("dist_upgrade 这类开关不该存在: %v", err)
	}
	params, err := action.Decode(json.RawMessage(`{"packages":["cowsay","curl"]}`))
	if err != nil {
		t.Fatalf("正常参数被拒: %v", err)
	}
	if got := params.(packageWriteParams); len(got.Packages) != 2 {
		t.Fatalf("参数解码错: %+v", got)
	}
}

// TestPackageRefreshDecodeRequiresEmptyObject：刷新不接受任何参数。
func TestPackageRefreshDecodeRequiresEmptyObject(t *testing.T) {
	reg := newPackageRegistry(t, newFakeWrites())
	action, _ := reg.Action(PackageRefreshAction)
	if _, err := action.Decode(json.RawMessage(`{}`)); err != nil {
		t.Fatalf("空参数应通过: %v", err)
	}
	if _, err := action.Decode(nil); err != nil {
		t.Fatalf("缺省参数应通过: %v", err)
	}
	if _, err := action.Decode(json.RawMessage(`{"packages":["bash"]}`)); !errors.Is(err, ErrParamsInvalid) {
		t.Fatalf("刷新不该接受包名: %v", err)
	}
}

// TestPackageInstallPlanMapping：预检结论逐字段直译，不加减。
func TestPackageInstallPlanMapping(t *testing.T) {
	src := newFakeWrites()
	src.assessment.Blockers = []apt.Reason{{Code: apt.CodeAptBusy, Message: "有别的 apt 正在运行"}}
	src.assessment.Warnings = []apt.Reason{{Code: apt.WarningNoChange, Message: "已经是最新版本：cowsay"}}
	src.assessment.Allowed = false
	reg := newPackageRegistry(t, src)
	action, _ := reg.Action(PackageInstallAction)

	plan, err := action.Plan(context.Background(), PlanInput{
		Params: packageWriteParams{Packages: []string{"cowsay"}},
	})
	if err != nil {
		t.Fatalf("预检报错: %v", err)
	}
	if plan.Allowed {
		t.Fatal("预检结论是拒绝，计划不能放行")
	}
	if len(plan.Blockers) != 1 || plan.Blockers[0].Code != apt.CodeAptBusy {
		t.Fatalf("阻塞项没照抄: %+v", plan.Blockers)
	}
	if len(plan.Warnings) != 1 || plan.Warnings[0].Code != apt.WarningNoChange {
		t.Fatalf("告警没照抄: %+v", plan.Warnings)
	}
	if plan.Target.DisplayName != "安装 cowsay" ||
		plan.Target.IdentityFingerprint != "sha256:aaaa" ||
		plan.Target.StateVersion != "sha256:bbbb" {
		t.Fatalf("目标绑定错: %+v", plan.Target)
	}
	if len(plan.Locks) != 1 || plan.Locks[0] != "apt-global" {
		t.Fatalf("资源锁错: %+v", plan.Locks)
	}
	if plan.NormalizedParams.(packageWriteParams).Packages[0] != "cowsay" {
		t.Fatalf("规范化参数错: %+v", plan.NormalizedParams)
	}
	if len(src.assessed) != 1 || src.assessed[0] != "install|cowsay" {
		t.Fatalf("预检没有把原始参数交下去: %v", src.assessed)
	}
}

// TestPackagePlanDefaultsLock：apt 侧没给锁时兜底成 apt-global。
func TestPackagePlanDefaultsLock(t *testing.T) {
	src := newFakeWrites()
	src.assessment.Locks = nil
	reg := newPackageRegistry(t, src)
	action, _ := reg.Action(PackageUpgradeAction)
	plan, err := action.Plan(context.Background(), PlanInput{Params: packageWriteParams{Packages: []string{"zlib1g"}}})
	if err != nil {
		t.Fatalf("预检报错: %v", err)
	}
	if len(plan.Locks) != 1 || plan.Locks[0] != "apt-global" {
		t.Fatalf("兜底锁错: %+v", plan.Locks)
	}
}

// TestPackageExecuteVerified：核对通过 → verified。
func TestPackageExecuteVerified(t *testing.T) {
	src := newFakeWrites()
	reg := newPackageRegistry(t, src)
	action, _ := reg.Action(PackageInstallAction)

	var steps []string
	res, err := action.Execute(context.Background(), ExecuteInput{
		Params: packageWriteParams{Packages: []string{"cowsay"}},
		Progress: func(step string, _ map[string]any) {
			steps = append(steps, step)
		},
	})
	if err != nil {
		t.Fatalf("执行报错: %v", err)
	}
	if err := validateResult(res); err != nil {
		t.Fatalf("结果不自洽: %v", err)
	}
	if res.Outcome != OutcomeVerified {
		t.Fatalf("核对通过应为 verified，实际 %s", res.Outcome)
	}
	if res.VerifiedAt.IsZero() {
		t.Fatal("verified 必须有核验时间")
	}
	if res.ObservedState["pkg:cowsay"] != "ii 3.03+dfsg2-8" {
		t.Fatalf("observed_state 要记下真实版本: %+v", res.ObservedState)
	}
	if res.ObservedState["verified"] != true {
		t.Fatalf("observed_state 要记下核对结论: %+v", res.ObservedState)
	}
	if len(steps) != 3 {
		t.Fatalf("进度阶段应透传: %v", steps)
	}
	if len(src.executed) != 1 || src.executed[0] != "install|cowsay" {
		t.Fatalf("执行参数被改过: %v", src.executed)
	}
}

// TestPackageExecuteMismatchIsPartial：命令成功但事实不对 → partial，不是 verified。
func TestPackageExecuteMismatchIsPartial(t *testing.T) {
	src := newFakeWrites()
	src.execResult.Verified = false
	src.execResult.Mismatches = []string{"zlib1g 装上的是 1.2，apt 说要装 1.3"}
	src.execResult.Observed = map[string]apt.ObservedPackage{
		"zlib1g": {Status: "ii", Version: "1.2"},
	}
	reg := newPackageRegistry(t, src)
	action, _ := reg.Action(PackageUpgradeAction)

	res, err := action.Execute(context.Background(), ExecuteInput{
		Params: packageWriteParams{Packages: []string{"zlib1g"}},
	})
	if err != nil {
		t.Fatalf("执行报错: %v", err)
	}
	if res.Outcome != OutcomePartial {
		t.Fatalf("事实对不上应为 partial，实际 %s", res.Outcome)
	}
	if res.ObservedState["mismatch_count"] != 1 {
		t.Fatalf("不匹配数量要报出来: %+v", res.ObservedState)
	}
	if !strings.Contains(res.RecoveryHint, "不一致") {
		t.Fatalf("partial 要给恢复提示: %q", res.RecoveryHint)
	}
	if res.Data["mismatches"].([]string)[0] == "" {
		t.Fatal("不匹配的细节要留在 data 里")
	}
}

// TestPackageExecuteFailureCarriesEvidence：失败时把读回的事实一并交出去。
func TestPackageExecuteFailureCarriesEvidence(t *testing.T) {
	src := newFakeWrites()
	src.execErr = errors.New("apt-get remove 失败（退出码 100）：E: 子进程 已安装 post-removal 脚本 返回了错误号 1")
	src.execResult.Op = apt.OpRemove
	src.execResult.Names = []string{"openssh-server"}
	src.execResult.ExitCode = 100
	src.execResult.Verified = false
	src.execResult.Observed = map[string]apt.ObservedPackage{
		"openssh-server": {Status: "ii", Version: "1:9.6p1-3ubuntu13.20"},
	}
	reg := newPackageRegistry(t, src)
	action, _ := reg.Action(PackageRemoveAction)

	res, err := action.Execute(context.Background(), ExecuteInput{
		Params: packageWriteParams{Packages: []string{"openssh-server"}},
	})
	if err == nil {
		t.Fatal("apt 失败必须返回 error")
	}
	if res.Outcome != OutcomePartial {
		t.Fatalf("失败应为 partial，实际 %s", res.Outcome)
	}
	if res.ObservedState["pkg:openssh-server"] != "ii 1:9.6p1-3ubuntu13.20" {
		t.Fatalf("失败时的现场必须留下: %+v", res.ObservedState)
	}
	if res.ObservedState["verified"] != false {
		t.Fatalf("失败不能留 verified=true: %+v", res.ObservedState)
	}
	if res.Data["exit_code"] != 100 {
		t.Fatalf("退出码要留下: %+v", res.Data)
	}
}

// TestPackageExecuteFailureReasonReachesMessage：apt 的原因必须能走到 error.message。
//
// 失败任务的 result 按契约是空对象，"为什么失败"只剩 error.message 一个出口。
// 这里只验动作层把原因说明了；runner 把它写进终态由
// TestOperationFailureReasonReachesErrorMessage 覆盖。
func TestPackageExecuteFailureReasonReachesMessage(t *testing.T) {
	src := newFakeWrites()
	src.execErr = errors.New("apt-get remove 失败（退出码 100）：E: 子进程 已安装 post-removal 脚本 返回了错误号 1")
	src.execResult.Op = apt.OpRemove
	src.execResult.Names = []string{"openssh-server"}
	src.execResult.ExitCode = 100
	reg := newPackageRegistry(t, src)
	action, _ := reg.Action(PackageRemoveAction)

	_, err := action.Execute(context.Background(), ExecuteInput{
		Params: packageWriteParams{Packages: []string{"openssh-server"}},
	})
	if err == nil {
		t.Fatal("apt 失败必须返回 error")
	}
	reason, ok := failureReasonOf(err)
	if !ok {
		t.Fatalf("软件包动作的失败要实现 FailureReasoner: %v", err)
	}
	if !strings.Contains(reason, "E: 子进程 已安装 post-removal 脚本 返回了错误号 1") {
		t.Fatalf("原因里要带上 apt 那句话: %q", reason)
	}
	if strings.Contains(reason, "\n") {
		t.Fatalf("原因必须压成一行: %q", reason)
	}
}

// TestPackageExecuteRejectsEmptyNames：预检落库的参数被换空时明确拒绝。
func TestPackageExecuteRejectsEmptyNames(t *testing.T) {
	src := newFakeWrites()
	reg := newPackageRegistry(t, src)
	action, _ := reg.Action(PackageInstallAction)
	_, err := action.Execute(context.Background(), ExecuteInput{Params: packageWriteParams{}})
	if !errors.Is(err, ErrParamsInvalid) {
		t.Fatalf("空包名应被拒绝: %v", err)
	}
	if len(src.executed) != 0 {
		t.Fatalf("不该调用写端: %v", src.executed)
	}
}

// TestPackageRefreshExecute：刷新走 Refresh 而不是 Execute。
func TestPackageRefreshExecute(t *testing.T) {
	src := newFakeWrites()
	src.refreshResult.RefreshWarnings = []string{"W: legacy trusted.gpg keyring"}
	indexAt := time.Date(2026, 10, 23, 12, 30, 0, 0, time.UTC)
	src.refreshResult.IndexUpdatedAt = &indexAt
	reg := newPackageRegistry(t, src)
	action, _ := reg.Action(PackageRefreshAction)

	res, err := action.Execute(context.Background(), ExecuteInput{Params: packageRefreshParams{}})
	if err != nil {
		t.Fatalf("执行报错: %v", err)
	}
	if src.refreshed != 1 || len(src.executed) != 0 {
		t.Fatalf("刷新不该走写事务: refreshed=%d executed=%v", src.refreshed, src.executed)
	}
	if res.Outcome != OutcomeVerified {
		t.Fatalf("刷新成功应为 verified: %s", res.Outcome)
	}
	if res.ObservedState["errors"] != 0 {
		t.Fatalf("错误数应记账: %+v", res.ObservedState)
	}
	if res.Data["index_after"] == "" {
		t.Fatalf("索引时间要留下: %+v", res.Data)
	}
}

// TestPackagePolicyUnchangedByParams：策略只看动作，不看参数。
//
// 这一条是结构性的：agent 的预检先算策略、再跑 Plan，所以策略**不可能**知道
// 这次操作会不会连带删掉 26 个包。卸载一律要短语，正是这个约束下的正确选择。
func TestPackagePolicyUnchangedByParams(t *testing.T) {
	reg := newPackageRegistry(t, newFakeWrites())
	action, _ := reg.Action(PackageRemoveAction)
	for _, params := range []any{
		packageWriteParams{Packages: []string{"cowsay"}},
		packageWriteParams{Packages: make([]string, 50)},
		nil,
	} {
		policy, err := action.ResolvePolicy(params)
		if err != nil {
			t.Fatalf("策略不合法: %v", err)
		}
		if !policy.RequiresPhrase || policy.PhraseVerb != "REMOVE" {
			t.Fatalf("卸载的短语要求不该随参数变化: %+v", policy)
		}
	}
}

func contains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

// TestPackagePlanBudgetDeclared 钉住四个软件包动作的预检预算。
//
// 真机上 `apt-get -s --only-upgrade install zlib1g` 要 15.6 秒（24.04，apt 2.8.3），
// 冷索引更久。Web 的等待上限比这里宽（operations.PreflightAgentTimeout，90 秒），
// 所以模拟真的卡住时，必须是**代理**先返回一句能看懂的话，而不是让 Web 等到超时后
// 猜一句"代理不可用"——那是假话。
//
// 只给 install 加预算是很容易漏的：refresh 和 remove 的 Plan 是另一段代码，而且
// refresh 慢起来是卡在网络上，不是卡在 CPU 上。
func TestPackagePlanBudgetDeclared(t *testing.T) {
	reg := newPackageRegistry(t, newFakeWrites())
	for _, name := range []string{
		PackageRefreshAction, PackageInstallAction,
		PackageRemoveAction, PackageUpgradeAction,
	} {
		action, ok := reg.Action(name)
		if !ok {
			t.Fatalf("%s 没有登记", name)
		}
		if action.PlanBudget != packagePlanBudget {
			t.Fatalf("%s 的预检预算应当声明 %s，实际 %s（0 表示不限：连接会先被掐断）",
				name, packagePlanBudget, action.PlanBudget)
		}
	}
	if packagePlanBudget <= 16*time.Second {
		t.Fatalf("预算 %s 太小：24.04 上升级模拟实测 15.6 秒", packagePlanBudget)
	}
}

// TestPreflightBudgetTurnsIntoResourceBusy 验证预检预算真的被用上、并且说人话。
//
// 走真实的 handlePreflights：登记一个 Plan 会等到 ctx 结束的动作，预算设成 30 毫秒，
// 断言回的是 409 RESOURCE_BUSY 加一句"预检超过 … 未完成"，而不是把连接掐断、
// 也不是伪装成 INTERNAL_ERROR。
func TestPreflightBudgetTurnsIntoResourceBusy(t *testing.T) {
	reg := NewRegistry()
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	err := reg.RegisterAction(Action{
		Name:       "test.slow",
		PlanBudget: 30 * time.Millisecond,
		Decode:     func(raw json.RawMessage) (any, error) { return struct{}{}, nil },
		Plan: func(ctx context.Context, in PlanInput) (Plan, error) {
			select {
			case <-block:
			case <-ctx.Done():
				return Plan{}, ctx.Err()
			}
			return Plan{}, errors.New("不该走到这里")
		},
	})
	if err != nil {
		t.Fatalf("登记动作: %v", err)
	}
	srv := newTestServerWithRegistry(t, reg)
	w := postPreflight(t, srv, testWebUID, preflightBody("test.slow", `{}`))
	if w.Code != http.StatusConflict {
		t.Fatalf("期望 409（RESOURCE_BUSY），实际 %d: %s", w.Code, w.Body.String())
	}
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("响应不是错误信封: %v", err)
	}
	if env.Error.Code != string(httpx.CodeResourceBusy) {
		t.Fatalf("期望错误码 RESOURCE_BUSY，实际 %q", env.Error.Code)
	}
	if !strings.Contains(env.Error.Message, "预检超过") || !strings.Contains(env.Error.Message, "30ms") {
		t.Fatalf("消息要说清是哪一步慢、预算是多少: %q", env.Error.Message)
	}
}
