package agent

// P08.3：把共享登记表接成存储判决的“面板侧依赖”来源，并驱动运行时脱盘监视。
//
// 两件事放在同一个文件里，是因为它们是同一个问题的两半：判决时要能回答
// “谁挂着这块卷”，运行时又要能在卷掉线之后把共享撤掉。两边用的是同一份登记表，
// 口径必须一致——否则会出现“预检说没依赖，监视却以为有”的分裂。

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"upanel/internal/shares"
	"upanel/internal/storage"
)

// ShareDependents 实现 storage.DependencyProber。
//
// 装配顺序上有一个环：storage 的判决需要依赖查询，而查询要读共享登记表与卷的现场
// 视图，卷的现场视图正是 storage.Collector 自己。所以这里允许“先交出去、后接线”：
// 建好两边之后再 Fill。没接线之前 Probe 返回错误 → 依赖未知 → 卷动作被拒。
// 这是刻意的：漏接线不该表现成“没有依赖”。
type ShareDependents struct {
	shares  shareLister
	volumes volumeLister
}

// shareLister 是本适配器用得到的那一半共享能力（登记表读取）。
type shareLister interface {
	Shares(ctx context.Context) ([]shares.ShareRecord, error)
}

// volumeLister 是卷的现场视图（受管理卷列表）；storage.Collector 满足它。
type volumeLister interface {
	Volumes(ctx context.Context) ([]storage.Volume, error)
}

// NewShareDependents 返回一个待接线的适配器（见类型注释里的环）。
func NewShareDependents() *ShareDependents { return &ShareDependents{} }

// Fill 接线。装配层在共享模块与卷采集器都建好之后调用一次。
func (d *ShareDependents) Fill(s shareLister, v volumeLister) {
	d.shares = s
	d.volumes = v
}

// Probe 回答“谁依赖这个卷/这台设备”。
//
// 三种用途的口径在 storage.DependencyPurpose 里定义，这里负责翻译成登记表的过滤：
// 卸载只算启用中的共享，其余（格式化/退出管理/展示）算全部。
//
// 一个刻意fail-closed 的地方：按设备查询时需要先知道“哪个卷挂在这台设备上”。
// 只要有一条共享绑在一个**查不到的卷**上，就无法排除它挂在目标设备上——
// 这时返回错误（VOLUME_DEPENDENCIES_UNKNOWN），而不是把它当成不相关。
// 悬空共享本身就是一处不一致，应该被看见、被告知，而不是被格式化的动作顺手抹掉。
func (d *ShareDependents) Probe(ctx context.Context, q storage.DependencyQuery) ([]storage.VolumeDependency, error) {
	if d == nil || d.shares == nil {
		return nil, errors.New("共享模块没有接线，无法确认面板侧依赖")
	}
	records, err := d.shares.Shares(ctx)
	if err != nil {
		return nil, fmt.Errorf("读取共享登记失败: %w", err)
	}
	var byID map[string]storage.Volume
	if q.DeviceID != "" {
		if d.volumes == nil {
			return nil, errors.New("卷列表不可用，无法把设备映射到受管理卷")
		}
		if byID, err = d.volumesByID(ctx); err != nil {
			return nil, err
		}
	}

	out := make([]storage.VolumeDependency, 0, len(records))
	seen := make(map[string]bool, len(records))
	for _, rec := range records {
		volID := rec.Definition.VolumeID
		switch {
		case q.VolumeID != "":
			if volID != q.VolumeID {
				continue
			}
		case q.DeviceID != "":
			vol, ok := byID[volID]
			if !ok {
				return nil, fmt.Errorf("共享 %s 绑定的卷 %s 不在受管理卷列表里，无法判断它是否依赖 %s",
					rec.Definition.Name, volID, q.DeviceID)
			}
			if vol.DeviceID != q.DeviceID {
				continue
			}
		default:
			// 既没给卷也没给设备：问的是“全部依赖”，照实返回。
		}
		if q.Purpose == storage.PurposeUnmount && !rec.Definition.Enabled {
			// 禁用中的共享此刻没有客户端，数据也还在盘上，不构成卸载的理由。
			continue
		}
		if seen[rec.ID] {
			continue
		}
		seen[rec.ID] = true
		out = append(out, storage.VolumeDependency{
			Kind: "share",
			ID:   rec.ID,
			Name: rec.Definition.Name,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// volumesByID 把受管理卷列成 id → 视图。
func (d *ShareDependents) volumesByID(ctx context.Context) (map[string]storage.Volume, error) {
	vols, err := d.volumes.Volumes(ctx)
	if err != nil {
		return nil, fmt.Errorf("读取受管理卷列表失败: %w", err)
	}
	out := make(map[string]storage.Volume, len(vols))
	for _, v := range vols {
		out[v.ID] = v
	}
	return out, nil
}

// shareWatchOperationID 是运行时监视用的锁持有者标识。
//
// 它不是一次真实操作，但资源锁需要一个持有者名字：用固定前缀加时间戳，
// 日志里就能分清“这一轮监视”和“某个人类操作”。
const shareWatchOperationIDPrefix = "share-watch"
