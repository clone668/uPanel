//go:build !linux

package agent

// isNoListenerErr 在非 Linux 平台上无法可靠区分"没人监听"和"说不清"。
//
// 代理本身只在 Linux 上运行（platform.CheckAgent），这个实现只是为了让开发宿主
// 能编译并跑与 socket 无关的单元测试：连得上仍然算"有人"（探测本身不看这个函数），
// 连不上就按陈旧处理，好让清理分支在开发宿主上也能被测到。
func isNoListenerErr(err error) bool {
	_ = err
	return true
}
