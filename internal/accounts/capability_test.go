package accounts

// 能力探针的单元测试（P14.5）。
//
// 这一条是虚拟机验收打出来的缺陷的守卫：`GET /accounts` 曾经把 share.accounts
// 带回来的 Samba 能力当成账号能力返回，于是一台**没装 Samba 的 Ubuntu 服务器**
// 上账号页显示“这台机器不支持账号管理”、新建按钮是灰的——而建一个普通账号需要的
// useradd / usermod / userdel / getent / passwd / chpasswd 一个不少，Samba 一个
// 都不用。能力探针把判据换成了账号自己的工具集，这里锁住四件事：
//
//  1. 工具齐全 → available，壳档两档都在，sudo 可写；
//  2. 缺必需工具 → blocked，而且 reason 说得出缺的是哪个；
//  3. 缺 visudo → 只关掉 sudo 那一半（账号仍然可用）；
//  4. 缺一档壳 → 只少一档；两档都缺 → blocked。
//
// 文件系统是假的：这里的断言全是“给定这些路径在不在，结论是什么”，真机上那条
// 结论由 tests/integration/p14-5-acceptance.sh 第 2 节独立核对。

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// fakeFS 是一个只有“这些路径存在”的文件系统替身。
type fakeFS map[string]bool

func (f fakeFS) stat(name string) (os.FileInfo, error) {
	if f[name] {
		return fakeFileInfo{name: name, mode: 0o755}, nil
	}
	return nil, os.ErrNotExist
}

// okTools 是“一台工具齐全的 Ubuntu”的路径集合。
func okTools() fakeFS {
	return fakeFS{
		ProgramUseradd:  true,
		ProgramUsermod:  true,
		ProgramUserdel:  true,
		ProgramGetent:   true,
		ProgramPasswd:   true,
		ProgramChpasswd: true,
		ProgramVisudo:   true,
		ShellRestricted: true,
		ShellDisabled:   true,
	}
}

// probeWith 用给定的路径集合探一次，不做缓存（TTL 给 0 时仍是 30 秒，
// 所以这里每次构造新探针）。
func probeWith(fs fakeFS) Capability {
	p := NewProber(ProberOptions{Stat: fs.stat})
	return p.Capability(context.Background())
}

// TestCapabilityAvailableWithAccountTools 工具齐全时可用，两档壳都在。
func TestCapabilityAvailableWithAccountTools(t *testing.T) {
	c := probeWith(okTools())
	if c.Status != CapabilityAvailable {
		t.Fatalf("status = %q（reason=%q）", c.Status, c.Reason)
	}
	if c.Reason != "" {
		t.Errorf("可用时不该有 reason：%q", c.Reason)
	}
	if !c.Available() {
		t.Error("Available() 与 status 不一致")
	}
	if c.Tools.Useradd != ProgramUseradd || c.Tools.Chpasswd != ProgramChpasswd {
		t.Errorf("工具路径读错了：%+v", c.Tools)
	}
	if c.SudoRuleWrite != "available" {
		t.Errorf("sudo_rule_write = %q", c.SudoRuleWrite)
	}
	if len(c.ShellProfiles) != 2 {
		t.Errorf("shell_profiles = %v，期望两档", c.ShellProfiles)
	}
	if len(c.Notices) != 0 {
		t.Errorf("工具齐全时不该有告警：%+v", c.Notices)
	}
	if c.CollectedAt.IsZero() {
		t.Error("collected_at 没填")
	}
}

// TestCapabilityBlockedWithoutUseradd 缺 useradd 就是 blocked，而且说得出缺什么。
func TestCapabilityBlockedWithoutUseradd(t *testing.T) {
	fs := okTools()
	delete(fs, ProgramUseradd)
	c := probeWith(fs)
	if c.Status != CapabilityBlocked {
		t.Fatalf("status = %q，期望 blocked", c.Status)
	}
	if !strings.Contains(c.Reason, "useradd") {
		t.Errorf("reason 没说缺什么：%q", c.Reason)
	}
	if c.Available() {
		t.Error("Available() 与 status 不一致")
	}
}

// TestCapabilityIgnoresSambaTools 账号能力不认识 Samba 的工具。
//
// 这条是缺陷的正身：只要判据里出现过 smbd，就说明又接回 share.accounts 那份能力了。
// 反过来也一样——有 smbd 而没有 useradd 的机器，账号能力必须是 blocked。
func TestCapabilityIgnoresSambaTools(t *testing.T) {
	fs := okTools()
	delete(fs, ProgramUseradd)
	fs["/usr/sbin/smbd"] = true
	fs["/usr/bin/smbpasswd"] = true
	fs["/usr/bin/pdbedit"] = true
	c := probeWith(fs)
	if c.Status != CapabilityBlocked {
		t.Fatalf("有 Samba 没 useradd 时 status = %q，期望 blocked", c.Status)
	}
	// 工具集里只该有账号的那几个：多出 smbd 就是接错了来源。
	for _, path := range []string{c.Tools.Useradd, c.Tools.Getent, c.Tools.Visudo} {
		if strings.Contains(path, "smb") {
			t.Errorf("工具集里混进了 Samba 的路径：%q", path)
		}
	}
}

// TestCapabilityWithoutVisudoStillManagesAccounts 缺 visudo 只关掉 sudo 那一半。
func TestCapabilityWithoutVisudoStillManagesAccounts(t *testing.T) {
	fs := okTools()
	delete(fs, ProgramVisudo)
	c := probeWith(fs)
	if c.Status != CapabilityAvailable {
		t.Fatalf("status = %q，期望 available（账号不依赖 visudo）", c.Status)
	}
	if c.SudoRuleWrite != "unsupported" {
		t.Errorf("sudo_rule_write = %q，期望 unsupported", c.SudoRuleWrite)
	}
	if len(c.Notices) != 1 || c.Notices[0].Code != NoticeSudoVisudoUnavailable {
		t.Fatalf("告警 = %+v，期望一条 %s", c.Notices, NoticeSudoVisudoUnavailable)
	}
	if c.Notices[0].Message == "" {
		t.Error("告警没有 message：页面上那句话就没了")
	}
}

// TestCapabilityShellProfilesFollowTheDisk 缺一档壳只少一档。
func TestCapabilityShellProfilesFollowTheDisk(t *testing.T) {
	fs := okTools()
	delete(fs, ShellRestricted)
	c := probeWith(fs)
	if c.Status != CapabilityAvailable {
		t.Fatalf("status = %q，期望 available", c.Status)
	}
	if len(c.ShellProfiles) != 1 || c.ShellProfiles[0] != string(ProfileDisabled) {
		t.Errorf("shell_profiles = %v，期望只剩 disabled", c.ShellProfiles)
	}
	if len(c.Notices) != 1 || c.Notices[0].Code != NoticeAccountShellMissing {
		t.Fatalf("告警 = %+v，期望一条 %s", c.Notices, NoticeAccountShellMissing)
	}
}

// TestCapabilityBlockedWithoutAnyShell 两档壳都没有时不能建账号。
func TestCapabilityBlockedWithoutAnyShell(t *testing.T) {
	fs := okTools()
	delete(fs, ShellRestricted)
	delete(fs, ShellDisabled)
	c := probeWith(fs)
	if c.Status != CapabilityBlocked {
		t.Fatalf("status = %q，期望 blocked", c.Status)
	}
	if !strings.Contains(c.Reason, ShellRestricted) || !strings.Contains(c.Reason, ShellDisabled) {
		t.Errorf("reason 没把两档壳都点名：%q", c.Reason)
	}
}

// TestCapabilityIsCached 30 秒内只探一次。
//
// 同一窗口里刷两次页面必须看到同一份结论：两次探测之间机器上多了一个工具，
// 第一屏说能用、第二屏说不能用，人只会以为面板在抽风。
func TestCapabilityIsCached(t *testing.T) {
	fs := okTools()
	stats := 0
	now := time.Date(2026, 10, 27, 4, 0, 0, 0, time.UTC)
	p := NewProber(ProberOptions{
		Stat: func(name string) (os.FileInfo, error) {
			stats++
			return fs.stat(name)
		},
		Now: func() time.Time { return now },
	})
	first := p.Capability(context.Background())
	afterFirst := stats
	if first.Status != CapabilityAvailable {
		t.Fatalf("status = %q", first.Status)
	}
	now = now.Add(10 * time.Second)
	p.Capability(context.Background())
	if stats != afterFirst {
		t.Errorf("TTL 之内又探了一次：%d → %d", afterFirst, stats)
	}
	// 过了 TTL 必须重新探：否则“装好工具之后页面还是不认”会持续到进程重启。
	now = now.Add(31 * time.Second)
	p.Capability(context.Background())
	if stats == afterFirst {
		t.Error("过了 TTL 没有重新探测")
	}
	// Invalidate 之后立刻重探。
	before := stats
	p.Invalidate()
	p.Capability(context.Background())
	if stats == before {
		t.Error("Invalidate 之后没有重新探测")
	}
}

// TestEngineCapabilityUsesProber 引擎把能力委托给探针，而不是自己现编一个。
func TestEngineCapabilityUsesProber(t *testing.T) {
	fs := okTools()
	delete(fs, ProgramUsermod)
	engine := &Engine{opts: Options{Prober: NewProber(ProberOptions{Stat: fs.stat})}}
	c := engine.Capability(context.Background())
	if c.Status != CapabilityBlocked {
		t.Fatalf("status = %q，期望 blocked", c.Status)
	}
	if !strings.Contains(c.Reason, "usermod") {
		t.Errorf("reason = %q", c.Reason)
	}
}
