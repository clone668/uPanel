package operations

// P10.3 服务层用例：按目标统计最近的尝试次数。
//
// 这条查询是 ACME 限流的**唯一**依据，所以它要证明的恰好是“成功的不算特殊”：
// 被代理拒绝的任务、卡在提交里的任务、已经失败的任务，都算一次尝试。限流放在
// 任务表上就是这个原因——证书表里只有成功签出来的那几张。

import (
	"context"
	"errors"
	"testing"
	"time"

	"upanel/internal/agentclient"
)

// attemptFixture 造一套以某个域名签发证书为计划的装置。
func attemptFixture(t *testing.T, domain string) *jobFixture {
	t.Helper()
	return newJobFixture(t, func(p *agentclient.PreflightPlan) {
		p.Action = "certificate.issue"
		p.Target.DisplayName = domain
		p.Target.IdentityFingerprint = "fp-cert-" + domain
		// 限流用例不关心资源锁，留空以免第二条任务被锁挡住。
		p.Locks = []string{}
	})
}

// TestRecentAttemptsCountsEveryTry 一次尝试就算一次，不管它成没成。
func TestRecentAttemptsCountsEveryTry(t *testing.T) {
	const domain = "example.test"
	actions := []string{"certificate.issue", "certificate.renew"}

	// 第一次任务：代理接受，任务停在 queued（常态）。
	f := attemptFixture(t, domain)
	ctx := context.Background()
	since := f.clock.Now().Add(-time.Hour)

	before, err := f.svc.RecentAttempts(ctx, actions, domain, since)
	if err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if before != 0 {
		t.Fatalf("还没有任务时统计 = %d，期望 0", before)
	}

	pf := f.preflight(7)
	f.mustSubmit(f.submitInput(pf.ID, "key-1", "PING AAAA"))

	after, err := f.svc.RecentAttempts(ctx, actions, domain, since)
	if err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if after != 1 {
		t.Fatalf("一次提交后统计 = %d，期望 1", after)
	}

	// 被代理明确拒绝的那一次同样消耗 ACME 配额，必须算进去。
	g := attemptFixture(t, domain)
	g.agent.opErr = &agentclient.Error{Status: 409, Code: "PREFLIGHT_STALE", Message: "目标状态已变化"}
	pf2 := g.preflight(7)
	if _, err := g.svc.SubmitJob(context.Background(), g.submitInput(pf2.ID, "key-1", "PING AAAA")); err == nil {
		t.Fatalf("代理拒绝时提交应当返回错误")
	} else {
		var rejected *Rejected
		if !errors.As(err, &rejected) {
			t.Fatalf("错误 = %v，期望 *Rejected", err)
		}
	}
	failed, err := g.svc.RecentAttempts(context.Background(), actions, domain,
		g.clock.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if failed != 1 {
		t.Fatalf("失败任务的统计 = %d，期望 1（失败的尝试同样消耗配额）", failed)
	}
}

// TestRecentAttemptsSeparatesTargetsAndWindows 目标与时间窗都要生效。
func TestRecentAttemptsSeparatesTargetsAndWindows(t *testing.T) {
	const domain = "scope.test"
	actions := []string{"certificate.issue", "certificate.renew"}

	f := attemptFixture(t, domain)
	pf := f.preflight(7)
	f.mustSubmit(f.submitInput(pf.ID, "key-1", "PING AAAA"))

	ctx := context.Background()
	since := f.clock.Now().Add(-time.Hour)

	// 别的域名不受影响。
	if n, err := f.svc.RecentAttempts(ctx, actions, "other.test", since); err != nil || n != 0 {
		t.Fatalf("另一个域名的统计 = %d（%v），期望 0", n, err)
	}
	// 窗口之后开始的观察看不到这次尝试（时间旅行等于“已经过了很久”）。
	if n, err := f.svc.RecentAttempts(ctx, actions, domain, f.clock.Now().Add(time.Minute)); err != nil || n != 0 {
		t.Fatalf("窗口外的统计 = %d（%v），期望 0", n, err)
	}
	// 动作不在清单里就不算——限流只对 ACME 那两个动作生效。
	if n, err := f.svc.RecentAttempts(ctx, []string{"proxy.apply"}, domain, since); err != nil || n != 0 {
		t.Fatalf("其它动作的统计 = %d（%v），期望 0", n, err)
	}
	// 空参数直接返回 0，不查库。
	if n, err := f.svc.RecentAttempts(ctx, nil, domain, since); err != nil || n != 0 {
		t.Fatalf("空动作的统计 = %d（%v），期望 0", n, err)
	}
	if n, err := f.svc.RecentAttempts(ctx, actions, "  ", since); err != nil || n != 0 {
		t.Fatalf("空目标的统计 = %d（%v），期望 0", n, err)
	}
}
