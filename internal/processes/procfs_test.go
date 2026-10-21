package processes

import (
	"testing"
)

// 用真实 /proc 字节核对解析器：这些文件全部抓自 upanel-2404。

func TestParseStatRealFixtures(t *testing.T) {
	cases := []struct {
		sample string
		comm   string
		state  string
		ppid   int64
		start  uint64
		utime  uint64
		stime  uint64
	}{
		// PID 1：systemd，父进程 0，启动于开机后 96 个滴答。
		{sample: "pid1", comm: "systemd", state: StateSleeping, ppid: 0, start: 96, utime: 1431, stime: 1354},
		// 内核线程：comm 里有下划线，父进程是 kthreadd（PID 2）。
		{sample: "kthread", comm: "pool_workqueue_release", state: StateSleeping, ppid: 2, start: 96, utime: 0, stime: 0},
		// 僵尸：状态 Z，命令行是空文件。
		{sample: "zombie", comm: "python3", state: StateZombie, ppid: 39501, start: 277269, utime: 0, stime: 0},
		// comm 里有空格与右括号（用符号链接改的可执行文件名）：只能取最后一个 ')'。
		{sample: "trickcomm", comm: "a)b c", state: StateSleeping, ppid: 39443, start: 277206, utime: 0, stime: 0},
	}
	for _, tc := range cases {
		t.Run(tc.sample, func(t *testing.T) {
			info, err := parseStat([]byte(fixture(t, tc.sample+".stat")))
			if err != nil {
				t.Fatalf("解析 %s.stat 失败: %v", tc.sample, err)
			}
			if info.comm != tc.comm {
				t.Errorf("comm = %q，期望 %q", info.comm, tc.comm)
			}
			if info.state != tc.state {
				t.Errorf("state = %q，期望 %q", info.state, tc.state)
			}
			if info.ppid != tc.ppid {
				t.Errorf("ppid = %d，期望 %d", info.ppid, tc.ppid)
			}
			if info.startTicks != tc.start {
				t.Errorf("starttime = %d，期望 %d", info.startTicks, tc.start)
			}
			if info.utime != tc.utime || info.stime != tc.stime {
				t.Errorf("utime/stime = %d/%d，期望 %d/%d", info.utime, info.stime, tc.utime, tc.stime)
			}
		})
	}
}

func TestParseStatRejectsGarbage(t *testing.T) {
	cases := map[string]string{
		"没有 comm":       "1 systemd S 0 1",
		"字段不足":          "1 (systemd) S 0",
		"状态字段异常":        "1 (systemd) SS 0 1 1 0 -1 1 1 1 1 1 1 1 1 1 20 0 1 0 96",
		"starttime 非数字": "1 (systemd) S 0 1 1 0 -1 1 1 1 1 1 1 1 1 1 20 0 1 0 abc",
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseStat([]byte(data)); err == nil {
				t.Fatalf("期望解析失败，实际成功：%q", data)
			}
		})
	}
}

func TestParseStatusRealFixtures(t *testing.T) {
	// 普通进程：Uid 1000（upanel），VmRSS 1968 kB。
	sleep := parseStatus([]byte(fixture(t, "sleep.status")))
	if sleep.uid != 1000 {
		t.Errorf("sleep uid = %d，期望 1000", sleep.uid)
	}
	if sleep.rssBytes == nil || *sleep.rssBytes != 1968*1024 {
		t.Fatalf("sleep VmRSS = %v，期望 %d", sleep.rssBytes, 1968*1024)
	}

	// 内核线程没有 VmRSS 行：缺就是缺，不能给 0。
	kthread := parseStatus([]byte(fixture(t, "kthread.status")))
	if kthread.rssBytes != nil {
		t.Errorf("内核线程不该有 VmRSS，实际 %d", *kthread.rssBytes)
	}

	// 僵尸的内存已经释放，同样没有 VmRSS。
	zombie := parseStatus([]byte(fixture(t, "zombie.status")))
	if zombie.rssBytes != nil {
		t.Errorf("僵尸不该有 VmRSS，实际 %d", *zombie.rssBytes)
	}

	// PID 1 是 root。
	pid1 := parseStatus([]byte(fixture(t, "pid1.status")))
	if pid1.uid != 0 {
		t.Errorf("pid1 uid = %d，期望 0", pid1.uid)
	}
}

func TestParseCmdlineRealFixtures(t *testing.T) {
	args := parseCmdline([]byte(fixture(t, "secretcmd.cmdline")))
	want := []string{"perl", "-e", "sleep 15", "--", "--password=hunter2", "--token=abc123def456"}
	if len(args) != len(want) {
		t.Fatalf("参数个数 = %d，期望 %d（%q）", len(args), len(want), args)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Errorf("参数 %d = %q，期望 %q", i, args[i], want[i])
		}
	}

	// 内核线程与僵尸的 cmdline 是空文件。
	if got := parseCmdline([]byte(fixture(t, "kthread.cmdline"))); len(got) != 0 {
		t.Errorf("内核线程不该有命令行，实际 %q", got)
	}
	if got := parseCmdline([]byte(fixture(t, "zombie.cmdline"))); len(got) != 0 {
		t.Errorf("僵尸不该有命令行，实际 %q", got)
	}
}

func TestCgroupUnitRealFixtures(t *testing.T) {
	cases := map[string]string{
		// system.slice 下的服务：单元就是 ssh.service。
		"sshd": "ssh.service",
		// 用户会话在 session-*.scope 里：它本身不是服务单元。
		"secretcmd": "session-705.scope",
		// 内核线程在根 cgroup，没有单元。
		"kthread": "",
		// PID 1 在 init.scope 里（Ubuntu 24.04 的实测值）。
		"pid1": "init.scope",
	}
	for sample, want := range cases {
		if got := cgroupUnit([]byte(fixture(t, sample+".cgroup"))); got != want {
			t.Errorf("%s.cgroup → %q，期望 %q", sample, got, want)
		}
	}
}

func TestCgroupUnitIgnoresV1Hierarchies(t *testing.T) {
	// cgroup v1 的多行输出：没有统一的单元语义，不猜。
	v1 := "12:memory:/user.slice\n11:cpu:/user.slice\n1:name=systemd:/user.slice/user-1000.slice/session-3.scope\n"
	if got := cgroupUnit([]byte(v1)); got != "" {
		t.Fatalf("cgroup v1 不该给出单元名，实际 %q", got)
	}
	// v2 的空控制器行同样可以解析。
	if got := cgroupUnit([]byte("0::/system.slice/cron.service\n")); got != "cron.service" {
		t.Fatalf("v2 解析失败：%q", got)
	}
}

func TestParsePasswd(t *testing.T) {
	users := parsePasswd([]byte(fixture(t, "passwd.txt")))
	if users[0] != "root" {
		t.Errorf("uid 0 = %q，期望 root", users[0])
	}
	if users[1000] != "upanel" {
		t.Errorf("uid 1000 = %q，期望 upanel", users[1000])
	}
	// 未知 uid 不编名字。
	if got := ownerName(users, 4242); got != "uid:4242" {
		t.Errorf("未知 uid 显示成 %q，期望 uid:4242", got)
	}
}

func TestParsePID(t *testing.T) {
	for _, name := range []string{"1", "39515"} {
		if _, ok := ParsePID(name); !ok {
			t.Errorf("%q 应该是合法 PID", name)
		}
	}
	for _, name := range []string{"", "stat", "1a", "-1", "0", "self", "fx"} {
		if _, ok := ParsePID(name); ok {
			t.Errorf("%q 不该被当成 PID", name)
		}
	}
}
