package apps

import (
	"context"
	"errors"

	"upanel/internal/storage"
)

// VolumeLister 是能列出受管理卷的读取端（生产实现是 storage.Fetcher）。
type VolumeLister interface {
	Volumes(ctx context.Context) ([]storage.Volume, error)
}

// listerChecker 把卷列表读取端适配成 VolumeChecker。
type listerChecker struct{ lister VolumeLister }

// NewVolumeChecker 用卷列表读取端实现 VolumeChecker。
//
// lister 为 nil 表示“这个部署没有连代理”（演示模式）：那时带依赖卷的写入会得到
// ErrVolumeCheckUnavailable（503），而不是被误判成“卷不存在”（422）——
// 前者是“我没法确认”，后者是“它确实不在”，两者的处理完全不同。
func NewVolumeChecker(lister VolumeLister) VolumeChecker { return listerChecker{lister} }

func (c listerChecker) VolumeExists(ctx context.Context, id string) (bool, error) {
	if c.lister == nil {
		return false, errors.New("未配置代理连接")
	}
	volumes, err := c.lister.Volumes(ctx)
	if err != nil {
		return false, err
	}
	for _, v := range volumes {
		if v.ID == id {
			return true, nil
		}
	}
	return false, nil
}
