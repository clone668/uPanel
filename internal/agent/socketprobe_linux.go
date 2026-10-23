//go:build linux

package agent

import (
	"errors"
	"syscall"
)

// isNoListenerErr 判断一次连接失败是不是"内核明说这个 socket 上没有监听者"。
//
// 只有这两种情况才允许把 socket 文件当成陈旧残留清掉：
//   - ECONNREFUSED：文件还在，但没人 listen——典型的上次异常退出；
//   - ENOENT：探测的这一刻文件已经被删了——本来就没有人。
//
// 别的错误（超时、EMFILE、EACCES 等）都归到"说不清"，调用方按有人处理，
// 不许清。分不清就删，删掉的可能是一个还活着的代理。
func isNoListenerErr(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOENT)
}
