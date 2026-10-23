package operations

// P10.3 服务层用例：任务终态回调。
//
// 这个钩子是证书账本的唯一写入点（internal/certificates/ledger.go），所以它的三条
// 约束在这里逐条证明：
//
//  1. **只在“刚刚进入终态”时触发一次**。对账是轮询的，同一个终态会被反复读到；
//     如果每次读都触发，同一张证书会被反复写一遍，而“上一次失败的原因”可能被
//     后来的成功覆盖成“看起来一直很顺利”。
//  2. **带上这次任务用的规范化参数**。账本要按域名落库，参数来自它消费的那份预检。
//  3. **钩子出错不能影响任务本身**。账本写失败是“记录没跟上”，不是“任务失败”。

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"upanel/internal/agentclient"
)

// hookRecorder 记录钩子被调用的次数与参数。
type hookRecorder struct {
	mu   sync.Mutex
	jobs []Job
	args []json.RawMessage
}

func (r *hookRecorder) record(_ context.Context, job Job, params json.RawMessage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.jobs = append(r.jobs, job)
	r.args = append(r.args, params)
}

func (r *hookRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.jobs)
}

// newHookFixture 造一套带终态钩子的装置：计划动作是证书导入，参数里带域名。
func newHookFixture(t *testing.T) (*jobFixture, *hookRecorder) {
	t.Helper()
	f := newJobFixture(t, func(p *agentclient.PreflightPlan) {
		p.Action = "certificate.import"
		p.Target.DisplayName = "hook.test"
		p.Target.IdentityFingerprint = "fp-cert-hook"
		p.Locks = []string{}
	})
	rec := &hookRecorder{}
	// 同包测试直接替换构造时注入的钩子：Options 里传一个记录器会要求再造一套夹具，
	// 而这里要证明的只是“钩子在什么时机被调用”。
	f.svc.onJobFinished = rec.record
	return f, rec
}

// TestJobFinishedHookFiresOnceOnTerminal 代理拒绝 → 一次终态 → 一次回调。
func TestJobFinishedHookFiresOnceOnTerminal(t *testing.T) {
	f, rec := newHookFixture(t)
	pf := f.preflight(7)
	f.agent.opErr = &agentclient.Error{Status: 409, Code: "PREFLIGHT_STALE", Message: "目标状态已变化"}

	if _, err := f.svc.SubmitJob(context.Background(), f.submitInput(pf.ID, "key-1", "PING AAAA")); err == nil {
		t.Fatalf("代理拒绝时提交应当返回错误")
	}
	if got := rec.count(); got != 1 {
		t.Fatalf("钩子被调用 %d 次，期望 1", got)
	}
	if rec.jobs[0].Action != "certificate.import" || rec.jobs[0].Status != "failed" {
		t.Fatalf("钩子收到的任务 = %+v，期望 certificate.import + failed", rec.jobs[0])
	}
	// 参数必须是这次任务实际用的那一份（预检的 normalized_params），不是请求体，
	// 也不是空值——账本要靠它知道写哪个域名。
	if string(rec.args[0]) != `{"label":"alpha"}` {
		t.Fatalf("钩子收到的参数 = %s", rec.args[0])
	}

	// 再读一次同一个终态任务：不许再触发。
	if _, err := f.svc.GetJob(context.Background(), 7, rec.jobs[0].ID); err != nil {
		t.Fatalf("读取任务失败: %v", err)
	}
	if got := rec.count(); got != 1 {
		t.Fatalf("重复读取终态任务后钩子被调用 %d 次，期望仍为 1", got)
	}
}

// TestJobFinishedHookPanicDoesNotBreakJob 钩子炸了也要把任务本身处理完。
func TestJobFinishedHookPanicDoesNotBreakJob(t *testing.T) {
	f, _ := newHookFixture(t)
	f.svc.onJobFinished = func(context.Context, Job, json.RawMessage) {
		panic("账本写入的 bug 不该让对账挂掉")
	}
	pf := f.preflight(7)
	f.agent.opErr = &agentclient.Error{Status: 409, Code: "PREFLIGHT_STALE", Message: "目标状态已变化"}

	job, err := f.svc.SubmitJob(context.Background(), f.submitInput(pf.ID, "key-1", "PING AAAA"))
	var rejected *Rejected
	if !errors.As(err, &rejected) {
		t.Fatalf("错误 = %v，期望 *Rejected", err)
	}
	if job.Status != "failed" {
		t.Fatalf("任务 = %+v，期望 failed", job)
	}
}

// TestJobFinishedHookSkippedWhenAlreadyTerminal 已经终态的任务不会因为再读一次而回调。
func TestJobFinishedHookSkippedWhenAlreadyTerminal(t *testing.T) {
	f, rec := newHookFixture(t)
	pf := f.preflight(7)
	f.agent.opErr = &agentclient.Error{Status: 409, Code: "PREFLIGHT_STALE", Message: "目标状态已变化"}
	job, err := f.svc.SubmitJob(context.Background(), f.submitInput(pf.ID, "key-1", "PING AAAA"))
	if err == nil {
		t.Fatalf("代理拒绝时提交应当返回错误")
	}
	if rec.count() != 1 {
		t.Fatalf("钩子被调用 %d 次，期望 1", rec.count())
	}

	// 代理此刻才回话（说 succeeded）也不该把终态改回去，更不该再触发一次。
	f.agent.getErr = nil
	f.agent.getOp = agentclient.Operation{
		OperationID: job.OperationID,
		Action:      job.Action,
		Stage:       "succeeded",
		Step:        "done",
		Result:      json.RawMessage(`{"outcome":"verified","observed_state":{}}`),
	}
	got, gerr := f.svc.GetJob(context.Background(), 7, job.ID)
	if gerr != nil {
		t.Fatalf("读取任务失败: %v", gerr)
	}
	if got.Status != "failed" {
		t.Fatalf("终态任务被改成了 %q", got.Status)
	}
	if rec.count() != 1 {
		t.Fatalf("钩子被调用 %d 次，期望仍为 1", rec.count())
	}
}
