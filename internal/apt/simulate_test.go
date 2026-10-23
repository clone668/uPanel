package apt

// 用**真机抓回来的** `apt-get -s` 输出做解析测试（internal/apt/testdata）。
//
// 这些文件是在 24.04.5 上用 `apt-get -s` 原样重定向得到的，没有手工改过一行。
// 用真实输出而不是手写样例，是因为手写样例只会覆盖"我以为 apt 会打印什么"；
// P11.1 的 arch 限定名缺陷就是被自己手写的样例哄过去的。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readCapture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("读抓取文件 %s 失败: %v", name, err)
	}
	return string(data)
}

// TestParseSimulationRealCaptures 逐份核对真机输出里的关键事实。
func TestParseSimulationRealCaptures(t *testing.T) {
	tests := []struct {
		file    string
		request []string
		check   func(t *testing.T, sim Simulation)
	}{
		{
			file:    "2404-sim-install-new.txt",
			request: []string{"cowsay"},
			check: func(t *testing.T, sim Simulation) {
				if sim.Failed() {
					t.Fatalf("不该判失败: errors=%v unmet=%v", sim.Errors, sim.Unmet)
				}
				if !sim.Changed() {
					t.Fatal("装新包必须是变化")
				}
				if got := sim.New; len(got) != 1 || got[0] != "cowsay" {
					t.Fatalf("New 应为 [cowsay]，实际 %v", got)
				}
				if sim.Counts.NewlyInstall != 1 || sim.Counts.ToRemove != 0 || sim.Counts.Upgraded != 0 {
					t.Fatalf("汇总行读错: %+v", sim.Counts)
				}
				// `Conf` 行不是安装步骤；只有 Inst 进计划。
				if len(sim.Steps) != 1 {
					t.Fatalf("Steps 应只有 Inst 一行，实际 %d: %+v", len(sim.Steps), sim.Steps)
				}
				step := sim.Steps[0]
				if step.Kind != StepInstall || step.Package != "cowsay" || step.ToVersion != "3.03+dfsg2-8" {
					t.Fatalf("Inst 行解析错: %+v", step)
				}
			},
		},
		{
			file:    "2404-sim-install-upgrade.txt",
			request: []string{"zlib1g"},
			check: func(t *testing.T, sim Simulation) {
				if sim.Failed() || !sim.Changed() {
					t.Fatalf("应有变化且不失败: changed=%v failed=%v", sim.Changed(), sim.Failed())
				}
				if len(sim.Steps) != 1 {
					t.Fatalf("Steps 应为 1，实际 %+v", sim.Steps)
				}
				step := sim.Steps[0]
				if step.Kind != StepUpgrade {
					t.Fatalf("带旧版本的 Inst 行应判为 upgrade，实际 %q", step.Kind)
				}
				if step.FromVersion != "1:1.3.dfsg-3.1ubuntu2.2" {
					t.Fatalf("FromVersion 错: %q", step.FromVersion)
				}
				// 目标版本要从 `(版本 源 [arch])` 里取，且不能被源名/架构污染。
				if step.ToVersion != "1:1.3.dfsg-3.1ubuntu2.3" {
					t.Fatalf("ToVersion 错: %q", step.ToVersion)
				}
				if target, ok := sim.TargetVersion("zlib1g"); !ok || target != step.ToVersion {
					t.Fatalf("TargetVersion 错: %q %v", target, ok)
				}
			},
		},
		{
			file:    "2404-sim-upgrade.txt",
			request: []string{"bash", "zlib1g"},
			check: func(t *testing.T, sim Simulation) {
				// `bash is already the newest version (…)` 要认出来，否则界面
				// 没法解释"我点了两个包，为什么只有一个在计划里"。
				if len(sim.AlreadyNewest) != 1 || sim.AlreadyNewest[0] != "bash" {
					t.Fatalf("AlreadyNewest 错: %v", sim.AlreadyNewest)
				}
				if len(sim.Upgrade) != 1 || sim.Upgrade[0] != "zlib1g" {
					t.Fatalf("Upgrade 错: %v", sim.Upgrade)
				}
			},
		},
		{
			file:    "2404-sim-upgrade-notinstalled.txt",
			request: []string{"cowsay"},
			check: func(t *testing.T, sim Simulation) {
				if sim.Changed() {
					t.Fatalf("没装的包做 only-upgrade 不是变化，Counts=%+v", sim.Counts)
				}
				if len(sim.Skipped) != 1 || sim.Skipped[0] != "cowsay" {
					t.Fatalf("Skipped 错: %v", sim.Skipped)
				}
				if sim.HadSummary != true || sim.Counts.NotUpgraded != 9 {
					t.Fatalf("汇总行读错: had=%v %+v", sim.HadSummary, sim.Counts)
				}
			},
		},
		{
			file:    "2404-sim-already.txt",
			request: []string{"bash"},
			check: func(t *testing.T, sim Simulation) {
				if sim.Changed() {
					t.Fatal("已是最新不该判为变化")
				}
				if sim.Failed() {
					t.Fatal("已是最新不是失败")
				}
				if len(sim.AlreadyNewest) != 1 || sim.AlreadyNewest[0] != "bash" {
					t.Fatalf("AlreadyNewest 错: %v", sim.AlreadyNewest)
				}
			},
		},
		{
			file:    "2404-sim-install-multi.txt",
			request: []string{"netcat-openbsd", "curl"},
			check: func(t *testing.T, sim Simulation) {
				if sim.Changed() {
					t.Fatalf("两个都已是最新，不该是变化: %+v", sim.Counts)
				}
				if len(sim.AlreadyNewest) != 2 {
					t.Fatalf("AlreadyNewest 应有两个: %v", sim.AlreadyNewest)
				}
				// `xxx set to manually installed.` 必须读出来：它是 apt 顺手
				// 改了 auto/manual 标记，虽然不影响版本，但要告诉管理员。
				if len(sim.SetManual) != 2 {
					t.Fatalf("SetManual 应有两个: %v", sim.SetManual)
				}
			},
		},
		{
			file:    "2404-sim-missing.txt",
			request: []string{"nosuchpkg-xyz"},
			check: func(t *testing.T, sim Simulation) {
				if !sim.Failed() {
					t.Fatal("找不到包必须判失败")
				}
				if len(sim.Errors) != 1 || !strings.HasPrefix(sim.Errors[0], "E: Unable to locate package") {
					t.Fatalf("E: 行原文丢了: %v", sim.Errors)
				}
				if sim.Changed() {
					t.Fatal("失败不该有变化")
				}
			},
		},
		{
			file:    "2404-sim-remove.txt",
			request: []string{"bash"},
			check: func(t *testing.T, sim Simulation) {
				// 删 Essential 时 apt 先打标题和警告，再打汇总，最后 E: —— 它
				// **不会**打印 Remv 行。所以"要删什么"必须从标题段落读，
				// 不能只靠 Remv。
				if !sim.Failed() {
					t.Fatal("删 Essential 必须判失败（apt 用 E: 拒绝）")
				}
				if len(sim.Remove) != 1 || sim.Remove[0] != "bash" {
					t.Fatalf("Remove 段落应读到 bash: %v", sim.Remove)
				}
				if len(sim.Steps) != 0 {
					t.Fatalf("apt 拒绝时没有 Remv 行，Steps 应为空: %+v", sim.Steps)
				}
				if sim.Counts.ToRemove != 1 {
					t.Fatalf("汇总行 to remove 应为 1: %+v", sim.Counts)
				}
				foundEssentialWarning := false
				for _, w := range sim.Warnings {
					if strings.Contains(w, "essential packages will be removed") {
						foundEssentialWarning = true
					}
				}
				if !foundEssentialWarning {
					t.Fatalf("essential 警告原文丢了: %v", sim.Warnings)
				}
			},
		},
		{
			file:    "2404-sim-remove-ssh.txt",
			request: []string{"openssh-server"},
			check: func(t *testing.T, sim Simulation) {
				// 这一份是本阶段最危险的形态：apt **同意**删掉 SSH 服务端，
				// 退出码 0，只删它自己。保护名单是唯一的拦网。
				if sim.Failed() {
					t.Fatalf("apt 没有拒绝，不该判失败: %v", sim.Errors)
				}
				if len(sim.Remove) != 1 || sim.Remove[0] != "openssh-server" {
					t.Fatalf("Remove 错: %v", sim.Remove)
				}
				if len(sim.Steps) != 1 || sim.Steps[0].Package != "openssh-server" || sim.Steps[0].Kind != StepRemove {
					t.Fatalf("Remv 行解析错: %+v", sim.Steps)
				}
				if sim.Steps[0].FromVersion != "1:9.6p1-3ubuntu13.20" {
					t.Fatalf("Remv 的旧版本错: %q", sim.Steps[0].FromVersion)
				}
			},
		},
		{
			file:    "2404-sim-remove-cascade.txt",
			request: []string{"zlib1g"},
			check: func(t *testing.T, sim Simulation) {
				if !sim.Failed() {
					t.Fatal("依赖冲突必须判失败")
				}
				if len(sim.Unmet) < 3 {
					t.Fatalf("unmet 明细应有若干行，实际 %v", sim.Unmet)
				}
				// 明细行必须是**去缩进**的原文，断言两处真实的依赖关系。
				joined := strings.Join(sim.Unmet, "\n")
				if !strings.Contains(joined, "apt : Depends: gpgv") {
					t.Fatalf("unmet 明细丢了 apt 那条:\n%s", joined)
				}
				// E: 行归 Errors，不混进 Unmet 明细。
				if len(sim.Errors) != 1 || !strings.Contains(sim.Errors[0], "pkgProblemResolver::Resolve") {
					t.Fatalf("E: 行原文错: %v", sim.Errors)
				}
				if sim.Changed() {
					t.Fatal("被拒绝的模拟不该判为有变化")
				}
			},
		},
		{
			file:    "2404-sim-remove-ca.txt",
			request: []string{"ca-certificates"},
			check: func(t *testing.T, sim Simulation) {
				if sim.Failed() {
					t.Fatalf("这一份 apt 是同意删的（退出码 0），不该判失败: %v", sim.Errors)
				}
				// 26 个删除：标题段落与汇总行必须一致，否则说明解析只读到一半。
				if len(sim.Remove) != 26 {
					t.Fatalf("Remove 应为 26 个，实际 %d: %v", len(sim.Remove), sim.Remove)
				}
				if sim.Counts.ToRemove != 26 {
					t.Fatalf("汇总行 to remove 应为 26: %+v", sim.Counts)
				}
				// Remv 行里的 `[依赖列表]` 不能被当成版本号或包名。
				for _, step := range sim.Steps {
					if step.Kind != StepRemove {
						t.Fatalf("这份输出只有 Remv: %+v", step)
					}
					if strings.ContainsAny(step.Package, "[] ") {
						t.Fatalf("包名被污染: %q", step.Package)
					}
				}
				if len(sim.Steps) != 26 {
					t.Fatalf("Remv 行应有 26 条，实际 %d", len(sim.Steps))
				}
				if sim.Steps[0].Package != "ubuntu-server" || sim.Steps[0].FromVersion != "1.539.2" {
					t.Fatalf("第一条 Remv 解析错: %+v", sim.Steps[0])
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.file, func(t *testing.T) {
			sim := ParseSimulation(readCapture(t, tc.file), tc.request)
			tc.check(t, sim)
		})
	}
}

// TestParseSimulationIgnoresUnknownLines：认不出来的行不能变成计划。
func TestParseSimulationIgnoresUnknownLines(t *testing.T) {
	out := strings.Join([]string{
		"Reading package lists...",
		"Building dependency tree...",
		"Some future apt message we have never seen",
		"  indented noise under no header",
		"0 upgraded, 0 newly installed, 0 to remove and 0 not upgraded.",
	}, "\n")
	sim := ParseSimulation(out, nil)
	if len(sim.Steps) != 0 || len(sim.New) != 0 || len(sim.Remove) != 0 {
		t.Fatalf("未知内容不该造出计划: %+v", sim)
	}
	if !sim.HadSummary || sim.Counts.NotUpgraded != 0 {
		t.Fatalf("汇总行仍应读到: %+v", sim.Counts)
	}
}

// TestParseSimulationEmptyOutput：空输出不是"什么都没发生"，是没有结论。
func TestParseSimulationEmptyOutput(t *testing.T) {
	sim := ParseSimulation("", []string{"bash"})
	if sim.HadSummary {
		t.Fatal("空输出不该有汇总行")
	}
	if sim.Changed() {
		t.Fatal("空输出不该判为有变化")
	}
	if sim.Failed() {
		t.Fatal("空输出不该判为失败（那是调用方看退出码的事）")
	}
}
