package processes

import (
	"strings"
	"testing"
)

func TestDirectProtectionPrecedence(t *testing.T) {
	// PID 1 优先于一切：即使它的单元名也不在保护名单里。
	if got := directProtection(procRow{pid: 1, comm: "systemd", state: StateSleeping, unit: "init.scope"}, 0); got.category != ProtectInit {
		t.Fatalf("PID 1 类别 = %q", got.category)
	}

	// 内核线程的判断只看“没有命令行且还活着”，与单元名无关。
	if got := directProtection(procRow{pid: 42, comm: "kworker/0:1", state: StateSleeping}, 0); got.category != ProtectKernel {
		t.Fatalf("内核线程类别 = %q", got.category)
	}
	// 僵尸同样没有命令行，但它不是内核线程：类别必须是空的。
	if got := directProtection(procRow{pid: 43, comm: "python3", state: StateZombie}, 0); got.category != "" {
		t.Fatalf("僵尸被当成了内核线程：%q", got.category)
	}

	// 代理自身：PID 相等即可，不需要单元名。
	self := directProtection(procRow{pid: 500, comm: "upanel-agent", state: StateSleeping, argv: []string{"upanel-agent"}}, 500)
	if self.category != ProtectSelf {
		t.Fatalf("代理自身类别 = %q", self.category)
	}

	// 没跑在 systemd 单元里的 upanel-* 可执行文件，靠 /proc/<pid>/exe 的基名认出来。
	outside := directProtection(procRow{pid: 600, comm: "upanel-web", state: StateSleeping, exeBase: "upanel-web", argv: []string{"upanel-web"}}, 0)
	if outside.category != ProtectSelf {
		t.Fatalf("upanel-web 类别 = %q", outside.category)
	}

	// 同名的第三方程序不算：判据是 exe 的基名，不是 comm。
	imposter := directProtection(procRow{pid: 601, comm: "upanel-agent", state: StateSleeping, exeBase: "bash", argv: []string{"bash"}}, 0)
	if imposter.category != "" {
		t.Fatalf("只有 comm 同名的进程不该被保护：%q", imposter.category)
	}

	// 普通进程：没有理由，也就没有类别。
	plain := directProtection(procRow{pid: 700, comm: "sleep", state: StateSleeping, unit: "session-1.scope", argv: []string{"sleep", "30"}}, 0)
	if plain.category != "" || len(plain.reasons) != 0 {
		t.Fatalf("普通进程被误判：%+v", plain)
	}
}

func TestProtectAncestorCycleTerminates(t *testing.T) {
	// 5 与 6 互为父进程（真实 /proc 里不会出现，但判定函数不能因为这种输入转不出来）。
	source := mapSource{
		5: {pid: 5, ppid: 6, comm: "a", state: StateSleeping, argv: []string{"a"}},
		6: {pid: 6, ppid: 5, comm: "b", state: StateSleeping, argv: []string{"b"}},
	}
	got := Protect(source[5], source, 0)
	if got.Protected {
		t.Fatalf("环里的普通进程不该受保护：%+v", got)
	}
}

func TestProtectAncestorChainStopsAtMissingParent(t *testing.T) {
	// 父进程读不到（已经退出）就停止向上追溯：不猜、也不当成受保护。
	source := mapSource{
		10: {pid: 10, ppid: 11, comm: "child", state: StateSleeping, argv: []string{"child"}},
	}
	got := Protect(source[10], source, 0)
	if got.Protected {
		t.Fatalf("读不到父进程不该判成受保护：%+v", got)
	}
	if got.Reasons == nil {
		t.Fatalf("Reasons 必须是空数组而不是 nil（JSON 里要序列化成 []）")
	}
}

func TestProtectReasonsAreDedupedAndSorted(t *testing.T) {
	// 一个进程既属于受保护单元、祖先又是代理自身：两个理由都要保留，且顺序固定。
	source := mapSource{
		1:   {pid: 1, ppid: 0, comm: "systemd", state: StateSleeping},
		100: {pid: 100, comm: "sshd", state: StateSleeping, argv: []string{"sshd"}},
		200: {pid: 200, ppid: 1, comm: "sshd", state: StateSleeping, unit: "ssh.service",
			exeBase: "sshd", argv: []string{"sshd"}},
	}
	got := Protect(source[200], source, 0)
	if !got.Protected {
		t.Fatalf("sshd 应受保护")
	}
	if got.Category != ProtectSSH {
		t.Fatalf("类别 = %q，期望 ssh", got.Category)
	}
	for i := 1; i < len(got.Reasons); i++ {
		if got.Reasons[i-1] > got.Reasons[i] {
			t.Fatalf("理由没有排序：%q", got.Reasons)
		}
	}
	seen := map[string]bool{}
	for _, reason := range got.Reasons {
		if seen[reason] {
			t.Fatalf("理由重复：%q", reason)
		}
		seen[reason] = true
		if !strings.Contains(reason, "ssh.service") {
			t.Fatalf("理由里没有单元名：%q", reason)
		}
	}
}

func TestSafeComm(t *testing.T) {
	if got := safeComm("a)b c"); got != "a)b c" {
		t.Fatalf("正常 comm 被改动：%q", got)
	}
	if got := safeComm("bad\x00name"); strings.Contains(got, "\x00") {
		t.Fatalf("comm 里的控制字符没有被清掉：%q", got)
	}
	if got := safeComm(""); got == "" {
		t.Fatalf("空 comm 必须有兜底文案")
	}
}
