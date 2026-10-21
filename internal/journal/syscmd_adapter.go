package journal

import (
	"context"

	"upanel/internal/syscmd"
)

// 本文件把 syscmd 的两个入口适配成本包的接口。
//
// 存在的理由只有一个：syscmd.Run 与 syscmd.Start 是函数，返回类型比 Runner/Streamer
// 更具体，Go 不会自动做这种协变转换。适配放在这里而不是让调用方自己写一遍，
// 是为了让“生产路径到底跑的是哪两个函数”只有一处答案。

type syscmdRunner struct{}

func (syscmdRunner) Run(ctx context.Context, spec syscmd.Spec, limits syscmd.Limits) (syscmd.Result, error) {
	return syscmd.Run(ctx, spec, limits)
}

// SyscmdRunner 返回生产用的 Runner（syscmd.Run）。
func SyscmdRunner() Runner { return syscmdRunner{} }

type syscmdStreamer struct{}

func (syscmdStreamer) Start(ctx context.Context, spec syscmd.Spec, limits syscmd.Limits) (StreamProcess, error) {
	return syscmd.Start(ctx, spec, limits)
}

// SyscmdStreamer 返回生产用的 Streamer（syscmd.Start）。
func SyscmdStreamer() Streamer { return syscmdStreamer{} }
