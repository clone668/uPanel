package shares

// 卷与共享之间的那道闸（P08.3）。
//
// 共享目录是“卷挂载点下面的一个子目录”。卷挂着的时候，那是一个真实文件系统的内容；
// 卷没挂的时候，同一个路径**依然是存在的**——它是系统盘上的一个空目录，而 smbd 不区分
// 这两件事。所以“卷没挂”不能只当成一次失败的观测，它必须变成一次**暴露面收缩**：
// 这条共享这一次不进托管配置。
//
// 这个文件只回答一个问题：某条共享此刻该不该出现在配置里。写文件与重载在
// configwrite.go，运行时撤销在 reconcile.go。

import (
	"context"

	"upanel/internal/storage"
)

// mountGate 是一条共享此刻的挂载闸门状态。
type mountGate struct {
	// Mounted 只有观测到“确实挂着”才是 true。
	//
	// 只有一个是不能当真的：**不知道**（observed_state=unknown / 空 / error）。
	// “问不出来”与“没挂”在这里走同一条路——都不写进配置。理由是不对称的代价：
	// 少一份共享是可见、可修、可重放的；多一份共享是指向系统盘空目录的静默误导，
	// 客户端看到的是一个空文件夹，而面板上写着“已生效”。
	Mounted bool
	// State 是观测到的原始状态（unmounted / unknown / error / missing…），
	// 原样保留，撤销记录里也要用它。
	State string
	// VolumeName 给消息用。
	VolumeName string
	// MountPath 是卷的挂载点（卷找不到时按 slug 推出来的规范路径）。
	MountPath string
	// Notices 是这次判定要带出来的告警（离线 / 观测不明）。
	Notices []Blocker
}

// mountGateFor 判定一条共享的卷此刻挂没挂。
//
// 卷已经不在登记表里时返回 found=false：那属于另一类问题（VOLUME_NOT_FOUND），
// 由调用方按各自的语境处理——渲染阶段是报错停下，撤销阶段是照样把它摘掉。
func (e *Engine) mountGateFor(ctx context.Context, volumeID string) (mountGate, bool, error) {
	vol, found, err := e.volumeFor(ctx, volumeID)
	if err != nil {
		return mountGate{}, false, err
	}
	if !found {
		return mountGate{}, false, nil
	}
	gate := mountGate{
		State:      vol.ObservedState,
		VolumeName: vol.DisplayName,
		MountPath:  vol.MountPath,
	}
	if gate.MountPath == "" {
		gate.MountPath = storage.MountPathOf(vol.Slug)
	}
	switch vol.ObservedState {
	case storage.VolumeStateMounted:
		gate.Mounted = true
	case "":
		gate.State = "unknown"
		gate.Notices = append(gate.Notices, Blocker{
			Code: NoticeVolumeStateUnknown,
			Message: "卷 " + gate.VolumeName + " 的挂载状态没有观测值：这一轮不能断言它挂着，" +
				"因此在观测成功之前不把这条共享写进配置",
		})
	default:
		msg := "卷 " + gate.VolumeName + " 当前没有挂载（observed_state=" + vol.ObservedState + "）"
		if vol.ObservedError != "" {
			msg += "：" + vol.ObservedError
		}
		msg += "；此时它的挂载点只是系统盘上的一个空目录，把共享留在配置里等于共享那个空目录"
		gate.Notices = append(gate.Notices, Blocker{Code: NoticeVolumeOffline, Message: msg})
	}
	return gate, true, nil
}
