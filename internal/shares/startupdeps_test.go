package shares

// P08.3：开机依赖（drop-in）的渲染与写入。
//
// 这一层的价值全在“什么情况下不动手”：
//   - 路径非法就整份不写（RequiresMountsFor 按空白切分，一个带空格的路径会静默变成两条）；
//   - 内容没变就不重载（否则每次应用都要 daemon-reload 一遍）；
//   - 没有挂载点就删掉文件并重载（让 systemd 忘掉旧要求）；
//   - 写失败不回滚配置（配置本身是对的，缺的只是开机顺序这一层）。

import (
	"context"
	"os"
	"strings"
	"testing"

	"upanel/internal/syscmd"
)

// TestRenderStartupDropin 校验内容形状：确定性、去重、按 4 条一行切分。
func TestRenderStartupDropin(t *testing.T) {
	t.Run("空列表给出空的值保留段头", func(t *testing.T) {
		got, err := RenderStartupDropin(nil)
		if err != nil {
			t.Fatalf("渲染失败: %v", err)
		}
		if !strings.Contains(got, "[Unit]\n") {
			t.Errorf("缺少 [Unit] 段头：\n%s", got)
		}
		if !strings.Contains(got, "RequiresMountsFor=\n") {
			t.Errorf("空列表应当给出一个空的 RequiresMountsFor：\n%s", got)
		}
	})

	t.Run("排序去重并按四条一行切分", func(t *testing.T) {
		got, err := RenderStartupDropin([]string{
			"/srv/upanel/volumes/e", "/srv/upanel/volumes/a", "/srv/upanel/volumes/c",
			"/srv/upanel/volumes/a", "/srv/upanel/volumes/b", "/srv/upanel/volumes/d",
		})
		if err != nil {
			t.Fatalf("渲染失败: %v", err)
		}
		lines := requiresLines(got)
		if len(lines) != 2 {
			t.Fatalf("RequiresMountsFor 行数 = %d，期望 2：\n%s", len(lines), got)
		}
		if lines[0] != "/srv/upanel/volumes/a /srv/upanel/volumes/b /srv/upanel/volumes/c /srv/upanel/volumes/d" {
			t.Errorf("第一行 = %q", lines[0])
		}
		if lines[1] != "/srv/upanel/volumes/e" {
			t.Errorf("第二行 = %q", lines[1])
		}
	})

	t.Run("同样的输入渲染出同样的字节", func(t *testing.T) {
		// 这是“内容没变就不重载”的前提：渲染不确定就等于每次都变。
		a, err := RenderStartupDropin([]string{"/b", "/a"})
		if err != nil {
			t.Fatalf("渲染失败: %v", err)
		}
		b, err := RenderStartupDropin([]string{"/a", "/b", "/a"})
		if err != nil {
			t.Fatalf("渲染失败: %v", err)
		}
		if a != b {
			t.Errorf("同一组路径渲染出了不同内容：\n%q\n%q", a, b)
		}
	})

	t.Run("非法路径整份拒绝", func(t *testing.T) {
		cases := map[string]string{
			"相对路径":  "srv/upanel/volumes/a",
			"含空格":   "/srv/upanel volumes/a",
			"含制表符":  "/srv/upanel\tvolumes/a",
			"含引号":   `/srv/upanel/volumes/"a`,
			"非规范形式": "/srv/upanel/volumes/../volumes/a",
			"尾部斜杠":  "/srv/upanel/volumes/a/",
		}
		for name, path := range cases {
			t.Run(name, func(t *testing.T) {
				if _, err := RenderStartupDropin([]string{path}); err == nil {
					t.Fatalf("%q 应当被拒绝", path)
				}
			})
		}
	})
}

// requiresLines 取出一份 drop-in 里 RequiresMountsFor 的值（按行）。
func requiresLines(content string) []string {
	var out []string
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, "RequiresMountsFor=") {
			out = append(out, strings.TrimPrefix(line, "RequiresMountsFor="))
		}
	}
	return out
}

// daemonReloads 数一数 systemctl daemon-reload 被调了几次。
func (h *harness) daemonReloads() int {
	n := 0
	h.runner.mu.Lock()
	defer h.runner.mu.Unlock()
	for _, c := range h.runner.calls {
		if c.Program == "/usr/bin/systemctl" && hasArg(c.Args, "daemon-reload") {
			n++
		}
	}
	return n
}

// TestApplyStartupDeps 校验写入、幂等、删除三条路径。
func TestApplyStartupDeps(t *testing.T) {
	t.Run("写入并重载", func(t *testing.T) {
		h := newHarness(t)
		res := h.engine.applyStartupDeps(context.Background(), []string{testMount})
		if res.Status != StartupDepsWritten {
			t.Fatalf("状态 = %q，期望 %q（notes=%+v）", res.Status, StartupDepsWritten, res.Notes)
		}
		if !res.Changed {
			t.Error("写了文件却没报告 Changed")
		}
		data, err := os.ReadFile(h.cfg.StartupDep)
		if err != nil {
			t.Fatalf("drop-in 没写出来: %v", err)
		}
		if strings.Contains(string(data), "[]") {
			t.Errorf("drop-in 内容不对：\n%s", data)
		}
		if got := requiresLines(string(data)); len(got) != 1 || got[0] != testMount {
			t.Errorf("RequiresMountsFor = %v，期望 [%s]", got, testMount)
		}
		if n := h.daemonReloads(); n != 1 {
			t.Errorf("daemon-reload 调用了 %d 次，期望 1", n)
		}
	})

	t.Run("内容没变就不重载", func(t *testing.T) {
		h := newHarness(t)
		if res := h.engine.applyStartupDeps(context.Background(), []string{testMount}); res.Status != StartupDepsWritten {
			t.Fatalf("第一次写入失败：%q %+v", res.Status, res.Notes)
		}
		res := h.engine.applyStartupDeps(context.Background(), []string{testMount})
		if res.Status != StartupDepsUnchanged {
			t.Fatalf("第二次状态 = %q，期望 %q", res.Status, StartupDepsUnchanged)
		}
		if res.Changed {
			t.Error("内容没变却报告 Changed")
		}
		if n := h.daemonReloads(); n != 1 {
			t.Errorf("daemon-reload 调用了 %d 次，期望仍是 1", n)
		}
	})

	t.Run("没有挂载点时删除并重载", func(t *testing.T) {
		h := newHarness(t)
		if res := h.engine.applyStartupDeps(context.Background(), []string{testMount}); res.Status != StartupDepsWritten {
			t.Fatalf("准备写入失败：%q", res.Status)
		}
		res := h.engine.applyStartupDeps(context.Background(), nil)
		if res.Status != StartupDepsRemoved {
			t.Fatalf("状态 = %q，期望 %q", res.Status, StartupDepsRemoved)
		}
		if _, err := os.Stat(h.cfg.StartupDep); !os.IsNotExist(err) {
			t.Errorf("drop-in 应当被删掉，stat 返回 %v", err)
		}
		if n := h.daemonReloads(); n != 2 {
			t.Errorf("daemon-reload 调用了 %d 次，期望 2（写入一次、删除一次）", n)
		}
	})

	t.Run("本来就没有文件时跳过", func(t *testing.T) {
		h := newHarness(t)
		res := h.engine.applyStartupDeps(context.Background(), nil)
		if res.Status != StartupDepsSkipped {
			t.Fatalf("状态 = %q，期望 %q", res.Status, StartupDepsSkipped)
		}
		if n := h.daemonReloads(); n != 0 {
			t.Errorf("什么都没写却重载了 %d 次", n)
		}
	})
}

// TestApplyStartupDepsReloadFailureIsReported 校验 daemon-reload 失败时说清楚，
// 并且**不**回滚已经写好的配置。
func TestApplyStartupDepsReloadFailureIsReported(t *testing.T) {
	h := newHarness(t)
	h.runner.on("/usr/bin/systemctl", func(spec syscmd.Spec) (syscmd.Result, error) {
		if hasArg(spec.Args, "daemon-reload") {
			return resFail(1, "Failed to reload daemon")
		}
		return resOK("active\nactive\n")
	})

	res := h.engine.applyStartupDeps(context.Background(), []string{testMount})
	if res.Status != StartupDepsFailed {
		t.Fatalf("状态 = %q，期望 %q", res.Status, StartupDepsFailed)
	}
	if len(res.Notes) == 0 || res.Notes[0].Code != NoticeStartupDepFailed {
		t.Fatalf("没有给出失败说明：%+v", res.Notes)
	}
	// 文件在那里，等下一次重载才生效——这比“假装没写成”更接近事实。
	if _, err := os.Stat(h.cfg.StartupDep); err != nil {
		t.Errorf("drop-in 应当已经写出来了：%v", err)
	}
}

// TestApplyStartupDepsRejectsUnsafePath 校验渲染失败时一个字节都不写。
func TestApplyStartupDepsRejectsUnsafePath(t *testing.T) {
	h := newHarness(t)
	res := h.engine.applyStartupDeps(context.Background(), []string{"/srv/upanel volumes/a"})
	if res.Status != StartupDepsFailed {
		t.Fatalf("状态 = %q，期望 %q", res.Status, StartupDepsFailed)
	}
	if _, err := os.Stat(h.cfg.StartupDep); !os.IsNotExist(err) {
		t.Errorf("路径非法却写出了文件（stat=%v）", err)
	}
	if n := h.daemonReloads(); n != 0 {
		t.Errorf("没写文件却重载了 %d 次", n)
	}
}

// TestRenderManagedSharesSkipsOfflineVolumes 校验卷离线的共享不会被渲染成配置段。
//
// 这是“served 的四条件”之外的另一道闸：判决会拦住第一次应用（卷没挂不放行），
// 但如果卷是在预检之后离线的，渲染这一步必须自己再查一遍。
func TestRenderManagedSharesSkipsOfflineVolumes(t *testing.T) {
	h := newHarness(t)
	h.putManagedAccount(testAcctA, "alice", false)
	h.putManagedAccount(testAcctB, "bob", false)
	rec := ShareRecord{
		ID:         testShareI,
		Definition: baseDefinition(),
		Version:    "1",
		CreatedAt:  h.now,
		UpdatedAt:  h.now,
	}
	rec.Definition.Enabled = true
	h.store.putShare(rec)

	// 卷没挂：这一段不该出现。
	vol := h.vols.vols[testVolID]
	vol.ObservedState = "unmounted"
	h.vols.vols[testVolID] = vol

	desired, skipped, err := h.engine.renderManagedShares(context.Background(), []ShareRecord{rec})
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	if len(desired) != 0 {
		t.Errorf("卷没挂却渲染了 %d 段：%+v", len(desired), desired)
	}
	if len(skipped) != 1 || skipped[0] != rec.Definition.Name {
		t.Errorf("skipped = %v，期望点名 %q", skipped, rec.Definition.Name)
	}

	// 挂回来之后要被渲染，并且带上挂载点（开机依赖要用它）。
	vol.ObservedState = "mounted"
	h.vols.vols[testVolID] = vol
	desired, skipped, err = h.engine.renderManagedShares(context.Background(), []ShareRecord{rec})
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	if len(skipped) != 0 {
		t.Errorf("卷挂着却跳过了：%v", skipped)
	}
	if len(desired) != 1 {
		t.Fatalf("desired = %d 段，期望 1", len(desired))
	}
	if desired[0].MountPath != testMount {
		t.Errorf("MountPath = %q，期望 %q", desired[0].MountPath, testMount)
	}
}
