package agent

// P08.3：运行时脱盘监视循环。
//
// 它只做一件事：每 DefaultShareWatchInterval 看一次“启用中的共享还挂不挂得上”，
// 发现卷离线就把那几段从托管配置里摘掉。为什么这条循环住在代理里：
//
//  1. 它要拿 shares.ConfigLockName 这把锁。托管配置是整份重写的，绕过代理自己写
//     就等于把“同一时刻只有一条写入路径”这条保证撤掉——锁在代理进程里，写手也得在。
//  2. 它写的是系统配置文件。这件事必须与人工操作共用同一套执行路径（同一个 Engine、
//     同一条 testparm/备份/原子替换/重载流水线），否则撤销就成了第二条野生写路径。
//  3. 它的结果要能进日志并被人看见：撤销是单向的，卷回来不会自动恢复。
//
// 循环本身不判断“该不该撤”——那是 shares.ReconcileVolumes 的事。这里只负责节奏、
// 锁与日志。判决逻辑放在单元测试里反复跑，节奏与锁放在这里，是刻意的分工。

import (
	"context"
	"errors"
	"fmt"
	"time"

	"upanel/internal/shares"
)

// DefaultShareWatchInterval 是运行时卷监视的间隔。
//
// 30 秒是在两种错误之间取的折中：太长会让“盘掉了但共享还在”暴露得久，太短则每轮都要
// 读一遍受管理卷的超级块（每个卷几毫秒）并和人工操作抢 samba:config 锁。
// 循环拿不到锁时只是跳过这一轮，不会阻塞人工操作，因此偏短的一侧没有危险性。
const DefaultShareWatchInterval = 30 * time.Second

// sharesReconciler 是监视循环需要的唯一能力（shares.Engine 满足它）。
type sharesReconciler interface {
	ReconcileVolumes(ctx context.Context) (shares.ReconcileReport, error)
}

// runShareWatch 是监视循环本体。
//
// 第一轮不等一个间隔：开机时盘就没挂上的情况（RequiresMountsFor 没能把它们挂回来）
// 应该立刻被处理，而不是让共享先对外挂 30 秒。
func (s *Server) runShareWatch(ctx context.Context) {
	if s.opts.ShareWatch == nil {
		return
	}
	interval := s.opts.ShareWatchInterval
	if interval <= 0 {
		interval = DefaultShareWatchInterval
	}
	s.opts.Logger.Info("已启用共享卷监视",
		"interval", interval.String(),
		"lock", shares.ConfigLockName,
		"behavior", "卷离线时撤销共享配置；不自动恢复")
	s.reconcileSharesOnce(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.reconcileSharesOnce(ctx)
		}
	}
}

// reconcileSharesOnce 跑一轮对账，自己负责拿锁与写日志。
func (s *Server) reconcileSharesOnce(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	opID := fmt.Sprintf("%s-%d", shareWatchOperationIDPrefix, time.Now().UnixNano())
	release, err := s.runner.locks.acquire(ctx, opID, []string{shares.ConfigLockName}, LockQueueTimeout)
	if err != nil {
		// 拿不到锁说明有人正在应用共享。跳过这一轮是对的：那次应用自己会重新渲染
		// 全量配置（离线的段本来就会被跳过），没有必要排队等 30 秒再补一刀。
		if errors.Is(err, ErrResourceBusy) {
			s.opts.Logger.Debug("跳过本轮共享卷监视：托管配置正被其他操作写入")
			return
		}
		if ctx.Err() != nil {
			return
		}
		s.opts.Logger.Warn("共享卷监视没能拿到配置锁", "error", err)
		return
	}
	defer release()

	rep, err := s.opts.ShareWatch.ReconcileVolumes(ctx)
	if err != nil {
		s.opts.Logger.Error("共享卷监视失败", "error", err)
		return
	}
	if len(rep.Revoked) == 0 {
		// 什么都没发生时不写日志：每 30 秒一条“一切正常”会把真正的事件埋掉。
		return
	}
	names := make([]string, 0, len(rep.Revoked))
	for _, r := range rep.Revoked {
		names = append(names, r.Name)
	}
	states := make([]string, 0, len(rep.Revoked))
	for _, r := range rep.Revoked {
		states = append(states, r.Revocation.VolumeState)
	}
	s.opts.Logger.Warn("卷离线，相关共享已从托管配置撤销",
		"shares", names,
		"volume_states", states,
		"config_written", rep.ConfigWrites,
		"reload", rep.Reload,
		"service", rep.Service,
		"served", rep.Served,
		"exports_written", rep.ExportsWritten,
		"exports_reload", rep.ExportsReload,
		"exports_served", rep.ExportsServed,
		"note", "卷恢复后需要重新应用共享——不会自动写回")
}
