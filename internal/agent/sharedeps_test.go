package agent

// P08.3：共享依赖查询与运行时脱盘监视的代理侧测试。
//
// 这里盯的是两个“接口边界”：
//   - 依赖查询把共享登记表翻译成“谁挂着这块卷”，口径按用途区分；
//   - 监视循环必须**自己拿着 samba:config 锁**去对账——那是“托管配置只有一条写路径”
//     的保证，绕开它就等于没有锁。

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"upanel/internal/shares"
	"upanel/internal/storage"
)

// fakeVolumeLister 是最小的受管理卷列表。
type fakeVolumeLister struct {
	vols []storage.Volume
	err  error
}

func (f *fakeVolumeLister) Volumes(context.Context) ([]storage.Volume, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.vols, nil
}

// shareRecord 造一条共享（只填依赖查询用得到的字段）。
func shareRecord(id, name, volumeID string, enabled bool) shares.ShareRecord {
	return shares.ShareRecord{
		ID: id,
		Definition: shares.ShareDefinition{
			Name: name, VolumeID: volumeID, Enabled: enabled,
		},
	}
}

// TestShareDependentsByVolume 校验按卷查询，以及卸载口径只算启用中的共享。
func TestShareDependentsByVolume(t *testing.T) {
	d := NewShareDependents()
	d.Fill(&fakeShares{records: []shares.ShareRecord{
		shareRecord("shr_1", "team-data", "vol_a", true),
		shareRecord("shr_2", "archive", "vol_a", false),
		shareRecord("shr_3", "other", "vol_b", true),
	}}, &fakeVolumeLister{})

	got, err := d.Probe(context.Background(), storage.DependencyQuery{VolumeID: "vol_a", Purpose: storage.PurposeUnmount})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(got) != 1 || got[0].Name != "team-data" || got[0].Kind != "share" {
		t.Fatalf("依赖 = %+v，期望只有启用中的 team-data", got)
	}

	// 换成“退出管理”的口径：禁用中的也算。
	got, err = d.Probe(context.Background(), storage.DependencyQuery{VolumeID: "vol_a", Purpose: storage.PurposeForget})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("依赖 = %+v，退出口径应当把禁用中的也算上", got)
	}
	if got[0].Name != "archive" || got[1].Name != "team-data" {
		t.Errorf("依赖顺序 = %v，期望按名字排序", []string{got[0].Name, got[1].Name})
	}
}

// TestShareDependentsByDevice 校验按设备查询会先经过“卷挂在哪台设备上”。
func TestShareDependentsByDevice(t *testing.T) {
	d := NewShareDependents()
	d.Fill(&fakeShares{records: []shares.ShareRecord{
		shareRecord("shr_1", "team-data", "vol_a", false), // 格式化口径：禁用也算
		shareRecord("shr_2", "other", "vol_b", true),
	}}, &fakeVolumeLister{vols: []storage.Volume{
		{ID: "vol_a", DeviceID: "sdb1", ObservedState: storage.VolumeStateMounted},
		{ID: "vol_b", DeviceID: "sdc1", ObservedState: storage.VolumeStateMounted},
	}})

	got, err := d.Probe(context.Background(), storage.DependencyQuery{DeviceID: "sdb1", Purpose: storage.PurposeFormat})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(got) != 1 || got[0].Name != "team-data" {
		t.Fatalf("依赖 = %+v，期望 sdb1 上的 team-data", got)
	}

	// 没有任何共享的设备：空数组 + 没有错误，才是“查过了，没有”。
	got, err = d.Probe(context.Background(), storage.DependencyQuery{DeviceID: "sdd1", Purpose: storage.PurposeFormat})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("依赖 = %+v，期望空", got)
	}
}

// TestShareDependentsDanglingVolumeBlocks 校验悬空共享会让按设备查询失败。
//
// 宁可让一次格式化被拒（并被告知是哪条共享悬空），也不能因为“查不到它挂在哪”
// 就把它当成不相关——那是一次不可逆操作。
func TestShareDependentsDanglingVolumeBlocks(t *testing.T) {
	d := NewShareDependents()
	d.Fill(&fakeShares{records: []shares.ShareRecord{
		shareRecord("shr_1", "team-data", "vol_gone", true),
	}}, &fakeVolumeLister{vols: []storage.Volume{}})

	_, err := d.Probe(context.Background(), storage.DependencyQuery{DeviceID: "sdb1", Purpose: storage.PurposeFormat})
	if err == nil {
		t.Fatal("悬空共享却查询成功")
	}
	if !strings.Contains(err.Error(), "team-data") || !strings.Contains(err.Error(), "vol_gone") {
		t.Errorf("错误没有点名共享与卷：%v", err)
	}
}

// TestShareDependentsFailsClosed 校验没接线、登记表读不出来、卷列表读不出来都是错误。
func TestShareDependentsFailsClosed(t *testing.T) {
	t.Run("没接线", func(t *testing.T) {
		d := NewShareDependents()
		if _, err := d.Probe(context.Background(), storage.DependencyQuery{VolumeID: "vol_a"}); err == nil {
			t.Fatal("没接线却查询成功")
		}
	})

	t.Run("共享登记表读不出来", func(t *testing.T) {
		d := NewShareDependents()
		d.Fill(&fakeShares{listErr: errors.New("库被锁")}, &fakeVolumeLister{})
		if _, err := d.Probe(context.Background(), storage.DependencyQuery{VolumeID: "vol_a"}); err == nil {
			t.Fatal("读登记失败却查询成功")
		}
	})

	t.Run("卷列表读不出来", func(t *testing.T) {
		d := NewShareDependents()
		d.Fill(&fakeShares{}, &fakeVolumeLister{err: errors.New("扫描失败")})
		if _, err := d.Probe(context.Background(), storage.DependencyQuery{DeviceID: "sdb1"}); err == nil {
			t.Fatal("读卷列表失败却查询成功")
		}
	})

	t.Run("缺卷列表时按卷查询仍然可用", func(t *testing.T) {
		// 按卷问不需要设备映射：卷动作不该因为卷列表读不出来就全部被拒。
		d := NewShareDependents()
		d.Fill(&fakeShares{records: []shares.ShareRecord{shareRecord("shr_1", "team-data", "vol_a", true)}}, nil)
		got, err := d.Probe(context.Background(), storage.DependencyQuery{VolumeID: "vol_a", Purpose: storage.PurposeUnmount})
		if err != nil {
			t.Fatalf("查询失败: %v", err)
		}
		if len(got) != 1 {
			t.Errorf("依赖 = %+v", got)
		}
	})
}

// fakeReconciler 是一次对账的替身：记录被调了几次，并顺手验证“锁此刻在我手里”。
type fakeReconciler struct {
	mu       sync.Mutex
	calls    int
	lockHeld bool
	report   shares.ReconcileReport
	err      error
}

func (f *fakeReconciler) ReconcileVolumes(context.Context) (shares.ReconcileReport, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	return f.report, f.err
}

// watchServer 造一台只有监视循环需要的那几个部件的 Server。
func watchServer(t *testing.T, r sharesReconciler) *Server {
	t.Helper()
	s := &Server{
		opts: Options{
			Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
			ShareWatch: r,
		},
	}
	s.runner = &runner{srv: s, locks: newLockManager(), cancels: map[string]context.CancelFunc{}}
	return s
}

// TestReconcileSharesOnceHoldsConfigLock 校验对账期间拿着 samba:config 锁，结束后释放。
func TestReconcileSharesOnceHoldsConfigLock(t *testing.T) {
	s := watchServer(t, nil)
	var busyDuringCall bool
	rec := reconcilerFunc(func(ctx context.Context) (shares.ReconcileReport, error) {
		// 同一把锁此刻应当已经在这轮对账手里：再拿一次必然排队失败。
		_, err := s.runner.locks.acquire(ctx, "someone-else",
			[]string{shares.ConfigLockName}, time.Millisecond)
		busyDuringCall = errors.Is(err, ErrResourceBusy)
		return shares.ReconcileReport{}, nil
	})
	s.opts.ShareWatch = rec

	s.reconcileSharesOnce(context.Background())
	if !busyDuringCall {
		t.Error("对账期间没有持有 samba:config 锁：托管配置的写路径就不止一条了")
	}
	// 结束后必须释放，否则人工应用会被永远挡住。
	release, err := s.runner.locks.acquire(context.Background(), "after", []string{shares.ConfigLockName}, time.Millisecond)
	if err != nil {
		t.Fatalf("对账结束后锁没有释放: %v", err)
	}
	release()
}

// TestReconcileSharesOnceReportsError 校验对账失败不会 panic，也不会留下锁。
func TestReconcileSharesOnceReportsError(t *testing.T) {
	s := watchServer(t, nil)
	s.opts.ShareWatch = reconcilerFunc(func(context.Context) (shares.ReconcileReport, error) {
		return shares.ReconcileReport{}, errors.New("卷列表读不出来")
	})
	s.reconcileSharesOnce(context.Background())

	release, err := s.runner.locks.acquire(context.Background(), "after", []string{shares.ConfigLockName}, time.Millisecond)
	if err != nil {
		t.Fatalf("失败之后锁没有释放: %v", err)
	}
	release()
}

// TestReconcileSharesOnceSkipsWhenCancelled 校验停机时不再动手。
func TestReconcileSharesOnceSkipsWhenCancelled(t *testing.T) {
	fake := &fakeReconciler{}
	s := watchServer(t, fake)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.reconcileSharesOnce(ctx)
	if fake.calls != 0 {
		t.Errorf("上下文已取消，却跑了 %d 次对账", fake.calls)
	}
}

// reconcilerFunc 让一个函数满足 sharesReconciler。
type reconcilerFunc func(context.Context) (shares.ReconcileReport, error)

func (f reconcilerFunc) ReconcileVolumes(ctx context.Context) (shares.ReconcileReport, error) {
	return f(ctx)
}
