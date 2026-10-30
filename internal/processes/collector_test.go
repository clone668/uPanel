package processes

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

func newTestCollector(fs *fakeFS, selfPID int64) *Collector {
	return NewCollector(Options{
		FS:       fs,
		Paths:    testPaths(),
		SelfPID:  selfPID,
		Interval: 5 * time.Second,
		Now:      func() time.Time { return time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC) },
	})
}

// makeStat 造一行 /proc/<pid>/stat。字段位置与内核格式一致（第 22 字段是 starttime），
// 正确性由 procfs_test.go 对真实样例的解析固定下来。
func makeStat(pid int64, comm string, state byte, ppid int64, utime, stime, start uint64) string {
	// ) 之后：state ppid | pgrp session tty tpgid flags minflt cminflt majflt cmajflt
	//        | utime stime | cutime cstime priority nice threads itreal
	//        | starttime
	return fmt.Sprintf("%d (%s) %c %d 0 0 0 -1 0 0 0 0 0 %d %d 0 0 20 0 1 0 %d 0 0",
		pid, comm, state, ppid, utime, stime, start)
}

// procStatFor 给某个 PID 写一份最小但完整的 /proc/<pid>/{stat,status,cmdline,cgroup}。
func procStatFor(fs *fakeFS, pid, ppid int64, comm, state string, utime, stime, start uint64, unit, cmdline string) {
	base := "/proc/" + strconv.FormatInt(pid, 10)
	fs.set(base+"/stat", makeStat(pid, comm, state[0], ppid, utime, stime, start))
	fs.set(base+"/status", "Name:\t"+comm+"\nUid:\t1000\t1000\t1000\t1000\nVmRSS:\t  2048 kB\n")
	fs.set(base+"/cgroup", "0::"+unit+"\n")
	fs.set(base+"/cmdline", cmdline)
	if _, ok := fs.dirs["/proc"]; ok {
		fs.mu.Lock()
		already := false
		for _, name := range fs.dirs["/proc"] {
			if name == strconv.FormatInt(pid, 10) {
				already = true
			}
		}
		if !already {
			fs.dirs["/proc"] = append(fs.dirs["/proc"], strconv.FormatInt(pid, 10))
		}
		fs.mu.Unlock()
	}
}

func findProcess(t *testing.T, snap Snapshot, pid int64) Process {
	t.Helper()
	for _, p := range snap.Processes {
		if p.PID == pid {
			return p
		}
	}
	t.Fatalf("快照里没有 PID %d（共 %d 个）", pid, len(snap.Processes))
	return Process{}
}

func TestCollectRealFixtures(t *testing.T) {
	fs := newTestFS(t)
	c := newTestCollector(fs, 424242)
	snap := c.Collect(t.Context())

	if snap.Status != StatusOK {
		t.Fatalf("采集状态 = %q，原因 %q", snap.Status, snap.Error)
	}
	if snap.Total != 6 || len(snap.Processes) != 6 {
		t.Fatalf("进程数 = %d/%d，期望 6/6", snap.Total, len(snap.Processes))
	}
	if snap.Truncated {
		t.Fatalf("6 个进程不该被截断")
	}
	if snap.IntervalMS != 5000 {
		t.Errorf("interval_ms = %d，期望 5000", snap.IntervalMS)
	}
	// 排序稳定：按 PID 升序。
	for i := 1; i < len(snap.Processes); i++ {
		if snap.Processes[i-1].PID >= snap.Processes[i].PID {
			t.Fatalf("进程列表没有按 PID 升序：%d → %d",
				snap.Processes[i-1].PID, snap.Processes[i].PID)
		}
	}

	// PID 1：受保护（init），没有可发送的信号。
	pid1 := findProcess(t, snap, 1)
	if !pid1.Protection.Protected || pid1.Protection.Category != ProtectInit {
		t.Fatalf("PID 1 保护判定 = %+v", pid1.Protection)
	}
	if len(pid1.Capabilities) != 0 {
		t.Fatalf("PID 1 不该有可执行动作：%v", pid1.Capabilities)
	}
	if pid1.Owner != "root" {
		t.Errorf("PID 1 属主 = %q，期望 root", pid1.Owner)
	}
	if pid1.StartTimeTicks != 96 {
		t.Errorf("PID 1 starttime = %d，期望 96", pid1.StartTimeTicks)
	}
	if pid1.CPUPercent != nil {
		t.Errorf("第一次采集不该有 CPU 百分比，实际 %v", *pid1.CPUPercent)
	}
	if pid1.MemoryBytes == nil || *pid1.MemoryBytes != strconv.FormatInt(14040*1024, 10) {
		t.Errorf("PID 1 内存 = %v，期望 14376960（14040 kB）", pid1.MemoryBytes)
	}
	if pid1.Unit != "init.scope" {
		t.Errorf("PID 1 单元 = %q，期望 init.scope", pid1.Unit)
	}

	// 内核线程：受保护（kernel_thread），命令摘要是 [comm]，没有内存值。
	kthread := findProcess(t, snap, 3)
	if !kthread.Protection.Protected || kthread.Protection.Category != ProtectKernel {
		t.Fatalf("内核线程保护判定 = %+v", kthread.Protection)
	}
	if kthread.Command != "[pool_workqueue_release]" {
		t.Errorf("内核线程摘要 = %q", kthread.Command)
	}
	if kthread.MemoryBytes != nil {
		t.Errorf("内核线程不该有内存值，实际 %v", *kthread.MemoryBytes)
	}

	// sshd：单元 ssh.service 命中服务保护名单，类别是 ssh。
	sshd := findProcess(t, snap, 860)
	if !sshd.Protection.Protected || sshd.Protection.Category != ProtectSSH {
		t.Fatalf("sshd 保护判定 = %+v", sshd.Protection)
	}
	if sshd.Unit != "ssh.service" {
		t.Errorf("sshd 单元 = %q，期望 ssh.service", sshd.Unit)
	}

	// 普通进程：不受保护，能力是 TERM 与 KILL（固定顺序）。
	sleep := findProcess(t, snap, 39515)
	if sleep.Protection.Protected {
		t.Fatalf("普通进程被误判为受保护：%+v", sleep.Protection)
	}
	if len(sleep.Capabilities) != 2 || sleep.Capabilities[0] != CapTERM || sleep.Capabilities[1] != CapKILL {
		t.Fatalf("普通进程能力 = %v，期望 [TERM KILL]", sleep.Capabilities)
	}
	if sleep.Owner != "upanel" {
		t.Errorf("sleep 属主 = %q，期望 upanel", sleep.Owner)
	}
	if sleep.Command != "sleep 15" {
		t.Errorf("sleep 摘要 = %q，期望 \"sleep 15\"", sleep.Command)
	}
	if sleep.Unit != "session-705.scope" {
		t.Errorf("sleep 单元 = %q", sleep.Unit)
	}
	if sleep.StartTimeTicks != 277319 {
		t.Errorf("sleep starttime = %d", sleep.StartTimeTicks)
	}

	// 僵尸：已经结束，没有能力，但原因不是“受保护”。
	zombie := findProcess(t, snap, 39503)
	if zombie.Protection.Protected {
		t.Fatalf("僵尸不该被标成受保护：%+v", zombie.Protection)
	}
	if len(zombie.Capabilities) != 0 {
		t.Fatalf("僵尸不该有可执行动作：%v", zombie.Capabilities)
	}
	if zombie.State != StateZombie {
		t.Errorf("僵尸状态 = %q，期望 zombie", zombie.State)
	}

	// 带秘密的进程：摘要里没有明文。
	secret := findProcess(t, snap, 39491)
	if secret.Command != "perl -e sleep 15 -- --password=*** --token=***" {
		t.Errorf("秘密进程摘要 = %q", secret.Command)
	}
}

func TestCollectProtectsAgentOwnTree(t *testing.T) {
	fs := newTestFS(t)
	// 代理自己的进程树：500 是 upanel-agent（代理进程本身），501 是它拉起的子进程。
	procStatFor(fs, 500, 1, "upanel-agent", "S", 10, 10, 900,
		"/system.slice/upanel-agent.service", "/usr/local/bin/upanel-agent\x00")
	procStatFor(fs, 501, 500, "systemctl", "S", 5, 5, 950,
		"/system.slice/upanel-agent.service", "/usr/bin/systemctl\x00stop\x00cron.service\x00")

	c := newTestCollector(fs, 500)
	snap := c.Collect(t.Context())

	agent := findProcess(t, snap, 500)
	if !agent.Protection.Protected || agent.Protection.Category != ProtectSelf {
		t.Fatalf("代理自身保护判定 = %+v", agent.Protection)
	}
	child := findProcess(t, snap, 501)
	if !child.Protection.Protected || child.Protection.Category != ProtectSelf {
		t.Fatalf("代理子进程保护判定 = %+v（祖先链应传播 self）", child.Protection)
	}
	joined := strings.Join(child.Protection.Reasons, "\n")
	if !strings.Contains(joined, "它由 PID 500") {
		t.Fatalf("子进程的保护理由没有说明祖先来源：%q", joined)
	}
}

func TestCollectDoesNotInheritSSHFromAncestors(t *testing.T) {
	fs := newTestFS(t)
	// 模拟“从 SSH 会话里启动的进程”：800 是 sshd 派生的登录 shell（session scope），
	// 801 是它在跑的命令。801 的祖先链上确实有 sshd，但 sshd 的受保护类别是 ssh——
	// 祖先只传播 self，所以 801 必须仍然可以终止，否则管理员从 SSH 里启动的
	// 每一个进程都会变成不可终止。
	procStatFor(fs, 800, 860, "sshd", "S", 1, 1, 2100,
		"/user.slice/user-1000.slice/session-2.scope", "sshd: upanel@pts/0\x00")
	procStatFor(fs, 801, 800, "sleep", "S", 1, 1, 2200,
		"/user.slice/user-1000.slice/session-2.scope", "sleep\x00300\x00")

	c := newTestCollector(fs, 424242)
	snap := c.Collect(t.Context())
	sessionShell := findProcess(t, snap, 800)
	if sessionShell.Protection.Protected {
		t.Fatalf("登录会话进程不该被当成 SSH 服务本体：%+v", sessionShell.Protection)
	}
	command := findProcess(t, snap, 801)
	if command.Protection.Protected {
		t.Fatalf("从 SSH 会话启动的普通进程不该被保护：%+v", command.Protection)
	}
	if len(command.Capabilities) != 2 {
		t.Fatalf("普通进程应有两个能力，实际 %v", command.Capabilities)
	}
}

func TestCollectCPUPercentFromDeltas(t *testing.T) {
	fs := newTestFS(t)
	// 基线：sleep 累计 5 个滴答，整机汇总 1000 个滴答、2 核。
	fs.set("/proc/39515/stat", makeStat(39515, "sleep", 'S', 39443, 5, 0, 277319))
	fs.setStatFile("cpu  1000 0 0 0 0 0 0 0 0 0\ncpu0  500 0 0 0 0 0 0 0 0 0\ncpu1  500 0 0 0 0 0 0 0 0 0\n")

	c := newTestCollector(fs, 424242)
	first := c.Collect(t.Context())
	if findProcess(t, first, 39515).CPUPercent != nil {
		t.Fatalf("第一次采集没有基线，cpu_percent 必须是 null")
	}

	// 第二次：sleep 多用 10 个滴答（5→15），整机多用 100 个滴答，2 核。
	// guest/guest_nice 的增长已经含在 user/nice 中，不得再进入分母。
	// 相对单核的百分比 = 100 × 10 × 2 ÷ 100 = 20.0。
	fs.set("/proc/39515/stat", makeStat(39515, "sleep", 'S', 39443, 15, 0, 277319))
	fs.setStatFile("cpu  1100 0 0 0 0 0 0 0 1000 2000\ncpu0  550 0 0 0 0 0 0 0 0 0\ncpu1  550 0 0 0 0 0 0 0 0 0\n")

	second := c.Collect(t.Context())
	sleep := findProcess(t, second, 39515)
	if sleep.CPUPercent == nil {
		t.Fatalf("第二次采集应该有 CPU 百分比")
	}
	if *sleep.CPUPercent != 20 {
		t.Fatalf("CPU 百分比 = %v，期望 20.0", *sleep.CPUPercent)
	}

	// PID 被重用（starttime 变了）：旧的滴答基线绝不套到新进程上。
	fs.set("/proc/39515/stat", makeStat(39515, "sleep", 'S', 39443, 999, 0, 277999))
	fs.setStatFile("cpu  1200 0 0 0 0 0 0 0 0 0\ncpu0  600 0 0 0 0 0 0 0 0 0\ncpu1  600 0 0 0 0 0 0 0 0 0\n")
	third := c.Collect(t.Context())
	if reused := findProcess(t, third, 39515); reused.CPUPercent != nil {
		t.Fatalf("starttime 变了就不得给 CPU 百分比，实际 %v", *reused.CPUPercent)
	}
	if reused := findProcess(t, third, 39515); reused.StartTimeTicks != 277999 {
		t.Fatalf("重用的 PID 应报告新的 starttime，实际 %d", reused.StartTimeTicks)
	}
}

func TestSnapshotKeepsLastSuccessOnFailure(t *testing.T) {
	fs := newTestFS(t)
	c := newTestCollector(fs, 424242)
	first := c.Collect(t.Context())
	if first.Status != StatusOK {
		t.Fatalf("第一次采集失败：%s", first.Error)
	}

	fs.mu.Lock()
	fs.listErr = fmt.Errorf("boom")
	fs.mu.Unlock()

	second := c.Collect(t.Context())
	if second.Status != StatusError {
		t.Fatalf("读不到 /proc 时状态应为 error，实际 %q", second.Status)
	}
	if len(second.Processes) != len(first.Processes) {
		t.Fatalf("失败时丢了上一次成功的列表：%d → %d", len(first.Processes), len(second.Processes))
	}
	if !second.CollectedAt.Equal(first.CollectedAt) {
		t.Fatalf("失败不该推进 collected_at：%v → %v", first.CollectedAt, second.CollectedAt)
	}
	if !strings.Contains(second.Error, "boom") {
		t.Fatalf("失败快照必须带原因，实际 %q", second.Error)
	}

	// 快照读的是缓存，不应再触发一次采集。
	cached := c.Snapshot()
	if cached.Status != StatusError || len(cached.Processes) != len(first.Processes) {
		t.Fatalf("Snapshot() 没有返回缓存内容：%+v", cached.Status)
	}
}

func TestSnapshotBeforeFirstCollect(t *testing.T) {
	c := newTestCollector(newTestFS(t), 424242)
	snap := c.Snapshot()
	if snap.Status != StatusError || snap.Error == "" {
		t.Fatalf("还没采集过时必须明确报失败，实际 %+v", snap)
	}
	if len(snap.Processes) != 0 {
		t.Fatalf("还没采集过时不该有进程")
	}
	if !snap.CollectedAt.IsZero() {
		t.Fatalf("还没采集过时 collected_at 必须是零值，实际 %v", snap.CollectedAt)
	}
	if snap.IntervalMS != 5000 {
		t.Fatalf("interval_ms 必须为正：%d", snap.IntervalMS)
	}
}

func TestLiveReadsWithoutCache(t *testing.T) {
	fs := newTestFS(t)
	c := newTestCollector(fs, 424242)
	c.Collect(t.Context())

	process, found, err := c.Live(39515)
	if err != nil || !found {
		t.Fatalf("Live(39515) = %v, %v", found, err)
	}
	if process.StartTimeTicks != 277319 {
		t.Fatalf("Live starttime = %d", process.StartTimeTicks)
	}
	if process.CPUPercent != nil {
		t.Fatalf("现场读取不该猜 CPU 百分比")
	}
	if !process.Protection.Protected && len(process.Capabilities) != 2 {
		t.Fatalf("现场读取的保护判定与能力不一致：%+v %v", process.Protection, process.Capabilities)
	}

	// 进程消失后现场读取立刻反映出来（缓存里可能还留着它）。
	if _, found, _ := c.Live(3939393); found {
		t.Fatalf("不存在的 PID 不该被现场读取找到")
	}
}
