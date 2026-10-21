//go:build unix

package processes

import (
	"syscall"
	"testing"
)

// 检查与发送之间进程自己退出了：信号没发出去，但意图已经达成。
// 这种情况不该报失败——报失败会让人以为还得再试一次，而目标早就不在了。
func TestExecuteSignalESRCHMeansAlreadyGone(t *testing.T) {
	fs := newTestFS(t)
	killer := &fakeKiller{err: syscall.ESRCH}
	c := newSignalCollector(fs, killer)

	res, err := c.ExecuteSignal(t.Context(),
		SignalParams{PID: 39515, StartTimeTicks: 277319, Signal: "TERM"}, nil)
	if err != nil {
		t.Fatalf("ESRCH 不该被当成执行失败：%v", err)
	}
	if res.Delivered {
		t.Fatalf("信号并没有真正送达，Delivered 必须是 false")
	}
	if !res.Gone {
		t.Fatalf("进程已经不在，Gone 必须是 true")
	}
}

// 真实的 syscall.Kill 只接受 TERM/KILL 两个字符串，永不接受信号编号。
func TestOSKillerRejectsUnknownSignal(t *testing.T) {
	if err := (osKiller{}).Signal(1, "9"); err == nil {
		t.Fatalf("信号编号不该被接受")
	}
	if err := (osKiller{}).Signal(1, "SIGKILL"); err == nil {
		t.Fatalf("字面量里没有 SIG 前缀的形式不该被接受")
	}
	// 对一个必然不存在的 PID 发 0 号之外的信号：内核回答 ESRCH，不留下任何副作用。
	err := (osKiller{}).Signal(4194304, "TERM")
	if err == nil {
		t.Fatalf("向不存在的 PID 发信号应当报错")
	}
	if !isNoSuchProcess(err) {
		t.Fatalf("期望 ESRCH，实际 %v", err)
	}
}
