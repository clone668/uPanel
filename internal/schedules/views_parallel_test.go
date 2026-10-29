package schedules

// 列表组装的并发性（P12.4）。
//
// 这一条用例存在的理由是一个真实现场：12 条计划的列表读了 10144 毫秒，压过 Web 侧
// 10 秒上限，健康的代理被报成 AGENT_UNAVAILABLE。串行组装下请求时长随计划条数线性
// 增长，因为每条计划都要 fork 两次 systemd 查询。用例把"问 systemd"换成可计并发的
// 替身，于是串行与并发在墙钟和并发峰值上都能分开——不是断言"跑得快"，是断言
// "同一时刻确实有多块在飞"。

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// loadUnits 是可计并发的单元端替身：每次"问 systemd"都睡 perCall，记下同时在飞的调用数。
//
// 串行实现里峰值只可能是 1；并发实现里峰值不会超过 ViewsConcurrency。
type loadUnits struct {
	perCall  time.Duration
	inFlight atomic.Int32
	peak     atomic.Int32
	calls    atomic.Int32
}

func (u *loadUnits) enter(ctx context.Context) {
	n := u.inFlight.Add(1)
	for {
		old := u.peak.Load()
		if n <= old || u.peak.CompareAndSwap(old, n) {
			break
		}
	}
	u.calls.Add(1)
	select {
	case <-time.After(u.perCall):
	case <-ctx.Done():
	}
	u.inFlight.Add(-1)
}

func (u *loadUnits) ValidateCalendar(ctx context.Context, d Definition) (CalendarCheck, error) {
	u.enter(ctx)
	return CalendarCheck{Normalized: CalendarLine(d),
		NextElapse: time.Date(2026, 10, 25, 3, 30, 0, 0, time.UTC)}, nil
}

func (u *loadUnits) Status(ctx context.Context, id string) (Status, error) {
	u.enter(ctx)
	return Status{TimerPath: "/etc/systemd/system/" + TimerName(id), Present: true,
		UnitFileState: "enabled", ActiveState: "active"}, nil
}

func (u *loadUnits) Ensure(context.Context, string, Definition) (Status, error) {
	return Status{}, nil
}

func (u *loadUnits) Remove(context.Context, string) error { return nil }

func TestViewsAsksSystemdInParallelAndKeepsOrder(t *testing.T) {
	const records = 8
	const perCall = 100 * time.Millisecond
	ctx := context.Background()
	store := newFakeStore()
	units := &loadUnits{perCall: perCall}
	now := time.Date(2026, 10, 24, 19, 30, 0, 0, time.UTC)
	mod, err := NewModule(ModuleOptions{Store: store, Units: units, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("NewModule: %v", err)
	}
	for i := 0; i < records; i++ {
		def := testDef()
		def.Name = fmt.Sprintf("计划 %02d", i)
		// 目标各不相同：库里有 UNIQUE(kind, target_id)，那条约束与并发的组装无关。
		def.TargetID = fmt.Sprintf("pol_%032d", i)
		if _, err := store.Create(ctx, def, now); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}

	start := time.Now()
	views, err := mod.Views(ctx)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Views: %v", err)
	}
	if len(views) != records {
		t.Fatalf("列表长度 = %d, 期望 %d", len(views), records)
	}
	// 并发只发生在"问 systemd"这一步：顺序必须与库里一致，否则界面上的顺序会随
	// 哪条计划的 systemd 先回答而抖动。
	for i, v := range views {
		if want := fmt.Sprintf("计划 %02d", i); v.Record.Name != want {
			t.Fatalf("第 %d 条 = %q, 期望 %q（顺序被打乱）", i, v.Record.Name, want)
		}
		if v.NextElapse == nil {
			t.Fatalf("第 %d 条没有下次触发时刻", i)
		}
	}
	// 每条计划两块查询（状态 + 日历），一块都不能少：并发不等于可以跳过。
	if want := int32(records * 2); units.calls.Load() != want {
		t.Fatalf("问了 %d 次 systemd, 期望 %d 次", units.calls.Load(), want)
	}
	// 串行的耗时是 records*2*perCall（1.6 秒）。留一倍余量：慢一半仍算并发，
	// 慢到串行量级就该红。
	serial := time.Duration(records*2) * perCall
	if elapsed >= serial/2 {
		t.Fatalf("列表组装用了 %s，串行量级是 %s：没有并发", elapsed, serial)
	}
	if peak := units.peak.Load(); peak < 2 {
		t.Fatalf("同时在飞的 systemd 查询峰值 = %d：组装是串行的", peak)
	} else if peak > int32(ViewsConcurrency) {
		t.Fatalf("并发峰值 = %d, 超过上限 %d", peak, ViewsConcurrency)
	}
}

func TestViewsSurfacesCancellationInsteadOfFakeUnitFailures(t *testing.T) {
	// 调用方走了就别把"我们不等了"记成"单元读不到"。describe 会把失败的 systemd
	// 查询落成 SCHEDULE_UNIT_STATUS_UNREADABLE，那种现场交上去就是假事实。
	store := newFakeStore()
	units := &loadUnits{perCall: 200 * time.Millisecond}
	now := time.Date(2026, 10, 24, 19, 30, 0, 0, time.UTC)
	mod, err := NewModule(ModuleOptions{Store: store, Units: units, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("NewModule: %v", err)
	}
	for i := 0; i < 8; i++ {
		def := testDef()
		def.Name = fmt.Sprintf("计划 %02d", i)
		def.TargetID = fmt.Sprintf("pol_%032d", i)
		if _, err := store.Create(context.Background(), def, now); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := mod.Views(ctx); err == nil {
		t.Fatal("调用方超时后 Views 必须把它报出来，不能交一份半成品")
	}
}
