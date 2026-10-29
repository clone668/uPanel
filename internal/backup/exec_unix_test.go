//go:build unix

package backup

// 真起一个子进程，把 24.04 上撞到的那条钉死。
//
// 现场是：restic 的 --json 进度每几十毫秒一条，进度回调那条路上要同步写一次
// SQLite（那台虚机上单次 50—150ms）。回调跑在管道的读协程上，读协程被拖住，
// 管道就排不空；进程已经按 0 退出，os/exec 的 WaitDelay（2 秒）到点，Run 返回
// ErrWaitDelay——restic 明明把快照写成了，任务结果是 failed，面板上写着
// "restic backup 失败（退出状态 0）"。
//
// 这里用 sh 造同样的形状：一堆行然后一行结论，回调故意慢到超过 WaitDelay。
// 断言只有三条：命令必须报成功、结论必须在、后面那行输出不能丢。

import (
	"context"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSlowProgressCallbackDoesNotFailTheCommand(t *testing.T) {
	const lines = 10
	var calls int64
	res, err := RunRestic(context.Background(), ExecSpec{
		Program: "/bin/sh",
		Args: []string{"-c",
			"i=0; while [ $i -lt " + strconv.Itoa(lines) + " ]; do echo progress-$i; i=$((i+1)); done; echo SUMMARY-LINE"},
		Timeout: 60 * time.Second,
		OnStdoutLine: func(line string) {
			// 每条比 WaitDelay 慢一个量级：旧实现下读协程被这批回调堵住，
			// 进程退出两秒后 Run 就返回 ErrWaitDelay。
			time.Sleep(250 * time.Millisecond)
			atomic.AddInt64(&calls, 1)
		},
	})
	if err != nil {
		t.Fatalf("进程按 0 退出却报了错: %v", err)
	}
	if res.ExitCode != 0 || res.Signal != "" {
		t.Fatalf("退出状态应为 0，实际 code=%d signal=%q", res.ExitCode, res.Signal)
	}
	if !strings.Contains(res.Stdout, "SUMMARY-LINE") {
		t.Fatalf("结论行丢了，stdout=%q", res.Stdout)
	}
	if got := atomic.LoadInt64(&calls); got != lines+1 {
		t.Fatalf("进度回调应收到 %d 行，实际 %d 行", lines+1, got)
	}
	if res.ProgressDropped != 0 {
		t.Fatalf("队列没满却丢了 %d 条进度", res.ProgressDropped)
	}
}
