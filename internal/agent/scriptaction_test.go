package agent

// script.apply / script.delete / script.run 的翻译层（P12.4）。
//
// 这一层的规矩只有一条：**永远不获得 root**。落盘与降权都在 internal/scripts 里，
// 这里验的是代理有没有守住三件事——正文规范化发生在绑定之前、非零退出算失败、
// 取消能真的传到执行器。

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"upanel/internal/scripts"
	"upanel/internal/syscmd"
)

const testScriptID = "scr_0123456789abcdef0123456789abcdef"

// fakeScripts 是 agent 层的假脚本端。
//
// mu 只保护"另一个 goroutine 也会写"的两个切片：测试会在动作协程里调 Run/AssessRun，
// 同时在自己的协程里轮询"执行器进去了没有"。不加锁的话 -race 会把这种轮询判成数据竞争
// （2026-10-28 的 P16 竞态读数里，TestScriptRunCancelReachesRunner 就是这么红的）。
type fakeScripts struct {
	mu sync.Mutex

	assessment scripts.Assessment
	assessErr  error
	applied    []scripts.ApplyRequest
	assessRuns []scripts.RunRequest

	record    scripts.Record
	applyExec error

	deleteAssessment scripts.Assessment
	deleted          []string
	deleteExec       error

	runOutcome scripts.RunOutcome
	runErr     error
	ran        []scripts.RunRequest
	// runBlocks 为 true 时 Run 一直等到 ctx 结束（模拟可取消的长脚本）。
	runBlocks bool
	// listed 非空时 List 返回它（用来构造多条记录）。
	listed []scripts.Record
}

func (f *fakeScripts) AssessApply(_ context.Context, req scripts.ApplyRequest) (scripts.Assessment, error) {
	f.applied = append(f.applied, req)
	return f.assessment, f.assessErr
}

func (f *fakeScripts) Apply(_ context.Context, req scripts.ApplyRequest) (scripts.Record, error) {
	if f.applyExec != nil {
		return scripts.Record{}, f.applyExec
	}
	rec := f.record
	rec.ID = req.ScriptID
	if rec.ID == "" {
		rec.ID = testScriptID
	}
	return rec, nil
}

func (f *fakeScripts) AssessDelete(_ context.Context, id string, _ int64) (scripts.Assessment, error) {
	f.deleted = append(f.deleted, id)
	return f.deleteAssessment, nil
}

func (f *fakeScripts) Delete(_ context.Context, id string, _ int64) error {
	f.deleted = append(f.deleted, id)
	return f.deleteExec
}

// List / Get 是镜像读路由要的两条（ScriptsService 把它们与动作面分开：
// 读的是元数据，写的是系统变更）。

func (f *fakeScripts) List(context.Context) ([]scripts.Record, error) {
	if len(f.listed) > 0 {
		return f.listed, nil
	}
	return []scripts.Record{f.record}, nil
}

func (f *fakeScripts) Get(_ context.Context, id string) (scripts.Record, bool, error) {
	if f.record.ID != id {
		return scripts.Record{}, false, nil
	}
	return f.record, true, nil
}

func (f *fakeScripts) AssessRun(_ context.Context, req scripts.RunRequest) (scripts.Assessment, error) {
	f.mu.Lock()
	f.assessRuns = append(f.assessRuns, req)
	f.mu.Unlock()
	return f.assessment, f.assessErr
}

func (f *fakeScripts) Run(ctx context.Context, req scripts.RunRequest) (scripts.RunOutcome, error) {
	f.mu.Lock()
	f.ran = append(f.ran, req)
	f.mu.Unlock()
	if f.runBlocks {
		<-ctx.Done()
		return scripts.RunOutcome{ScriptName: "巡检"}, ctx.Err()
	}
	return f.runOutcome, f.runErr
}

// ranRequests 返回执行器收到的请求快照。测试在自己的协程里读这个切片，
// 而 Run 在动作协程里写它——必须走一把锁，否则就是数据竞争。
func (f *fakeScripts) ranRequests() []scripts.RunRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]scripts.RunRequest, len(f.ran))
	copy(out, f.ran)
	return out
}

func newFakeScripts() *fakeScripts {
	return &fakeScripts{
		assessment: scripts.Assessment{
			Allowed:             true,
			DisplayName:         "巡检",
			IdentityFingerprint: "sha256:aaa",
			StateVersion:        "3",
			Impacts: []scripts.Impact{
				{Kind: "execute_as", Description: "脚本将以 upanel-shell 身份运行，永远不获得 root"},
			},
		},
		record: scripts.Record{
			ID:          testScriptID,
			Name:        "巡检",
			ContentHash: "sha256:bbb",
			Ref:         "/var/lib/upanel-shell/" + testScriptID,
			Interpreter: scripts.Interpreter,
			Version:     1,
		},
		deleteAssessment: scripts.Assessment{Allowed: true},
		runOutcome:       scripts.RunOutcome{ScriptName: "巡检", ExitCode: 0, Stdout: "ok\n"},
	}
}

func newScriptRegistry(t *testing.T, src scriptSource) *Registry {
	t.Helper()
	reg := NewRegistry()
	if err := RegisterScriptActions(reg, src); err != nil {
		t.Fatalf("登记脚本动作失败: %v", err)
	}
	return reg
}

func TestRegisterScriptActionsRequiresSource(t *testing.T) {
	if err := RegisterScriptActions(NewRegistry(), nil); err == nil {
		t.Fatal("没有实现端就不该登记成功")
	}
	if err := RegisterScriptActionsIfAvailable(NewRegistry(), nil); err != nil {
		t.Fatalf("没有实现端时不该报错: %v", err)
	}
}

func TestScriptActionsRegistered(t *testing.T) {
	reg := newScriptRegistry(t, newFakeScripts())
	for _, name := range []string{scripts.ActionScriptApply, scripts.ActionScriptDelete, scripts.ActionScriptRun} {
		if _, ok := reg.Action(name); !ok {
			t.Fatalf("%s 没有登记", name)
		}
	}
}

func TestScriptPolicies(t *testing.T) {
	reg := newScriptRegistry(t, newFakeScripts())
	cases := map[string]bool{
		scripts.ActionScriptApply:  false,
		scripts.ActionScriptDelete: false,
		scripts.ActionScriptRun:    true,
	}
	for name, cancellable := range cases {
		action, _ := reg.Action(name)
		policy, err := action.ResolvePolicy(nil)
		if err != nil {
			t.Fatalf("%s 策略不合法: %v", name, err)
		}
		if !policy.RequiresReauth {
			t.Fatalf("%s 必须再认证", name)
		}
		if policy.RequiresPhrase {
			t.Fatalf("%s 不该要求短语", name)
		}
		if policy.Cancellable != cancellable {
			t.Fatalf("%s 可取消 = %v，期望 %v", name, policy.Cancellable, cancellable)
		}
	}
}

// TestScriptApplyDecodeNormalizes：CRLF 必须在**解码**时折成 LF。
// 否则预检绑定的是带 \r 的正文，落盘的是折过的，两者哈希不同。
func TestScriptApplyDecodeNormalizes(t *testing.T) {
	reg := newScriptRegistry(t, newFakeScripts())
	action, _ := reg.Action(scripts.ActionScriptApply)

	params, err := action.Decode(json.RawMessage(
		`{"script_id":null,"version":0,"name":"  巡检  ","content":"echo a\r\necho b\r\n"}`))
	if err != nil {
		t.Fatalf("正常参数被拒: %v", err)
	}
	p := params.(scripts.ApplyParams)
	if p.Name != "巡检" {
		t.Fatalf("名字没有去空白: %q", p.Name)
	}
	if strings.Contains(p.Content, "\r") {
		t.Fatalf("正文里还有回车: %q", p.Content)
	}
	if p.Content != "echo a\necho b\n" {
		t.Fatalf("正文 = %q", p.Content)
	}
}

func TestScriptApplyDecodeLimits(t *testing.T) {
	reg := newScriptRegistry(t, newFakeScripts())
	action, _ := reg.Action(scripts.ActionScriptApply)

	// 超过正文上限的请求在解码就被拒：它连预检都不该进，否则一份 10 MiB 的
	// 正文会先被写进预检表。
	huge := strings.Repeat("a", scripts.MaxContentBytes+1)
	if _, err := action.Decode(json.RawMessage(
		`{"script_id":null,"version":0,"name":"大","content":"` + huge + `"}`)); err == nil {
		t.Fatal("超长正文应当被拒")
	}
	// 参数上限必须留出名字与信封的余量，否则一份刚好 64 KiB 的合法脚本会被拒。
	if action.ParamsLimitBytes <= scripts.MaxContentBytes {
		t.Fatalf("参数上限 %d 太小", action.ParamsLimitBytes)
	}
	for _, raw := range []string{
		`{"script_id":null,"version":0,"name":"x","content":"echo hi","path":"/etc/passwd"}`,
		`{"script_id":null,"version":0,"name":"x","content":"echo hi","interpreter":"/bin/bash"}`,
		`{"script_id":null,"version":0,"name":"x","content":""}`,
	} {
		if _, err := action.Decode(json.RawMessage(raw)); !errors.Is(err, ErrParamsInvalid) {
			t.Fatalf("%s 应当被拒: %v", raw, err)
		}
	}
}

func TestScriptApplyPlanUsesNormalizedContent(t *testing.T) {
	src := newFakeScripts()
	reg := newScriptRegistry(t, src)
	action, _ := reg.Action(scripts.ActionScriptApply)

	params, err := action.Decode(json.RawMessage(
		`{"script_id":null,"version":0,"name":"巡检","content":"echo a\r\necho b\n"}`))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := action.Plan(context.Background(), PlanInput{Params: params})
	if err != nil {
		t.Fatalf("预检失败: %v", err)
	}
	if len(src.applied) != 1 {
		t.Fatalf("模块收到的请求数 = %d", len(src.applied))
	}
	if src.applied[0].Input.Content != "echo a\necho b\n" {
		t.Fatalf("送给模块的正文 = %q", src.applied[0].Input.Content)
	}
	if len(plan.Impacts) != 1 || !strings.Contains(plan.Impacts[0].Description, "upanel-shell") {
		t.Fatalf("影响没说清运行身份: %+v", plan.Impacts)
	}
	if len(plan.Locks) != 0 {
		t.Fatalf("新建脚本不该加锁: %v", plan.Locks)
	}
}

func TestScriptApplyExecuteVerified(t *testing.T) {
	reg := newScriptRegistry(t, newFakeScripts())
	action, _ := reg.Action(scripts.ActionScriptApply)
	params, err := action.Decode(json.RawMessage(
		`{"script_id":null,"version":0,"name":"巡检","content":"echo hi\n"}`))
	if err != nil {
		t.Fatal(err)
	}
	res, err := action.Execute(context.Background(), ExecuteInput{
		Params: params, Now: time.Date(2026, 10, 24, 20, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if res.Outcome != OutcomeVerified {
		t.Fatalf("结果 = %q，期望 verified（落盘后回读过哈希）", res.Outcome)
	}
	if res.ObservedState["content_hash"] != "sha256:bbb" {
		t.Fatalf("观测状态 = %+v", res.ObservedState)
	}
	if !strings.Contains(res.RecoveryHint, "upanel-shell") {
		t.Fatalf("提示没说清运行身份: %q", res.RecoveryHint)
	}
}

func TestScriptDeleteExecuteVerified(t *testing.T) {
	src := newFakeScripts()
	reg := newScriptRegistry(t, src)
	action, _ := reg.Action(scripts.ActionScriptDelete)
	params, err := action.Decode(json.RawMessage(`{"script_id":"` + testScriptID + `","version":2}`))
	if err != nil {
		t.Fatal(err)
	}
	// 被计划引用时模块会拒绝；代理只负责把原因带出来。
	src.deleteExec = errors.New("脚本仍被计划「夜间备份」引用")
	_, err = action.Execute(context.Background(), ExecuteInput{Params: params})
	var reasoner FailureReasoner
	if !errors.As(err, &reasoner) || !strings.Contains(reasoner.FailureReason(), "引用") {
		t.Fatalf("原因 = %v", err)
	}
}

func scriptRunParams(t *testing.T, runtime int) any {
	t.Helper()
	reg := newScriptRegistry(t, newFakeScripts())
	action, _ := reg.Action(scripts.ActionScriptRun)
	params, err := action.Decode(json.RawMessage(
		`{"script_id":"` + testScriptID + `","version":3,"max_runtime_seconds":` +
			strconv.Itoa(runtime) + `}`))
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	return params
}

func TestScriptRunDecodeStrict(t *testing.T) {
	reg := newScriptRegistry(t, newFakeScripts())
	action, _ := reg.Action(scripts.ActionScriptRun)
	for _, raw := range []string{
		`{"script_id":"` + testScriptID + `","version":3,"max_runtime_seconds":0}`,
		`{"script_id":"` + testScriptID + `","version":3,"max_runtime_seconds":86401}`,
		`{"script_id":"` + testScriptID + `","version":0,"max_runtime_seconds":60}`,
		`{"script_id":"scr_zz","version":1,"max_runtime_seconds":60}`,
		`{"script_id":"` + testScriptID + `","version":1,"max_runtime_seconds":60,"content":"id"}`,
	} {
		if _, err := action.Decode(json.RawMessage(raw)); !errors.Is(err, ErrParamsInvalid) {
			t.Fatalf("%s 应当被拒: %v", raw, err)
		}
	}
}

func TestScriptRunExitZeroVerified(t *testing.T) {
	src := newFakeScripts()
	src.runOutcome = scripts.RunOutcome{
		ScriptName: "巡检", ExitCode: 0, Duration: 1500 * time.Millisecond, Stdout: "ok\n",
	}
	reg := newScriptRegistry(t, src)
	action, _ := reg.Action(scripts.ActionScriptRun)
	res, err := action.Execute(context.Background(), ExecuteInput{
		Params: scriptRunParams(t, 60), Now: time.Date(2026, 10, 24, 20, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if res.Outcome != OutcomeVerified || res.ObservedState["exit_code"] != 0 {
		t.Fatalf("结果 = %+v", res)
	}
	ran := src.ranRequests()
	if len(ran) != 1 || ran[0].MaxRuntimeSeconds != 60 {
		t.Fatalf("运行请求 = %+v", ran)
	}
}

// TestScriptRunNonZeroIsFailure：非零退出不是“跑完了”，是失败。
func TestScriptRunNonZeroIsFailure(t *testing.T) {
	src := newFakeScripts()
	src.runOutcome = scripts.RunOutcome{
		ScriptName: "巡检", ExitCode: 2, Stderr: "错误: 没有权限\n其余的行\n",
	}
	reg := newScriptRegistry(t, src)
	action, _ := reg.Action(scripts.ActionScriptRun)
	_, err := action.Execute(context.Background(), ExecuteInput{Params: scriptRunParams(t, 60)})
	var reasoner FailureReasoner
	if !errors.As(err, &reasoner) {
		t.Fatalf("非零退出必须失败（%T）", err)
	}
	reason := reasoner.FailureReason()
	if !strings.Contains(reason, "退出状态 2") || !strings.Contains(reason, "没有权限") {
		t.Fatalf("原因 = %q", reason)
	}
	// 只带第一行：整段输出可能含路径或密钥片段，不能塞进任务记录。
	if strings.Contains(reason, "其余的行") {
		t.Fatalf("原因里带了多余输出: %q", reason)
	}
}

func TestScriptRunTimeoutIsFailure(t *testing.T) {
	src := newFakeScripts()
	src.runErr = syscmd.ErrTimeout
	src.runOutcome = scripts.RunOutcome{ScriptName: "巡检"}
	reg := newScriptRegistry(t, src)
	action, _ := reg.Action(scripts.ActionScriptRun)
	_, err := action.Execute(context.Background(), ExecuteInput{Params: scriptRunParams(t, 90)})
	var reasoner FailureReasoner
	if !errors.As(err, &reasoner) || !strings.Contains(reasoner.FailureReason(), "超过 90 秒") {
		t.Fatalf("原因 = %v", err)
	}
}

// TestScriptRunCancelReachesRunner：取消必须真的传到执行器（否则进程继续跑）。
func TestScriptRunCancelReachesRunner(t *testing.T) {
	src := newFakeScripts()
	src.runBlocks = true
	reg := newScriptRegistry(t, src)
	action, _ := reg.Action(scripts.ActionScriptRun)

	cancel := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := action.Execute(context.Background(), ExecuteInput{
			Params: scriptRunParams(t, 600), Cancel: cancel,
		})
		done <- err
	}()
	// 等到执行器真的进去了再取消：否则测的是“取消先到”，不是“取消穿透”。
	deadline := time.Now().Add(2 * time.Second)
	for len(src.ranRequests()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("执行器没有被调用")
		}
		time.Sleep(time.Millisecond)
	}
	close(cancel)
	select {
	case err := <-done:
		if !errors.Is(err, ErrCanceled) {
			t.Fatalf("错误 = %v，期望 ErrCanceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("取消没有传到执行器")
	}
}

func TestOutputTail(t *testing.T) {
	if got := outputTail(""); got != "（没有输出）" {
		t.Fatalf("空输出 = %q", got)
	}
	if got := outputTail("\n\n  \n"); got != "（没有输出）" {
		t.Fatalf("只有空行时 = %q", got)
	}
	if got := outputTail("第一行\n第二行"); got != "第一行" {
		t.Fatalf("多行时 = %q", got)
	}
	// 控制字符被删掉而不是原样进任务记录（终端转义序列在日志里会变成噪音）。
	if got := outputTail("\x1b[31m坏了\x1b[0m\n细节"); got != "[31m坏了[0m" {
		t.Fatalf("含控制字符时 = %q", got)
	}
	long := strings.Repeat("x", 300)
	got := outputTail(long)
	if !strings.HasSuffix(got, "…") || len(got) != 160+len("…") {
		t.Fatalf("截断后长度 = %d，期望 %d", len(got), 160+len("…"))
	}
}
