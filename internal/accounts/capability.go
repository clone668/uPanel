package accounts

// 账号能力探针（P14.5）。
//
// **为什么账号要有自己的一份能力报告，而不是复用 shares.Capability。**
//
// 这一条是虚拟机验收打出来的：`GET /accounts` 原先直接返回 `share.accounts` 那份
// 能力，而那份能力的判据是 smbd / smbpasswd / pdbedit / setfacl / getfacl ——全部是
// Samba 的工具。于是一台**没装 Samba 的普通 Ubuntu 服务器**上，账号页显示
// “这台机器不支持账号管理”，新建账号按钮是灰的；而建一个普通账号需要的
// useradd / usermod / getent / chpasswd 一个不少。账号与 Samba 唯一的交集是
// `type=share` 的那一类，而那一类的写入口在共享页（DECISIONS Q475），不在账号页。
//
// P14.4 给 NFS 单独开 `NFSCapability` 用的就是同一条理由（见 shares/capability.go
// 那段注释）：把两个彼此独立的协议塞进一个 status，缺一个就把另一个也判死。
// 这里只是把同一条结论用到账号上。
//
// 探针只做只读的事：按固定绝对路径 `stat` 一遍。**不跑任何工具**——能力探测自己
// 不该是第一个改机器的东西，也不该因为一次 fork 失败就说机器不支持。路径是代码里
// 的常量（与 commands.go 的 Program* 同源），不来自配置、不来自 HTTP：可配置的
// “面板去执行哪个二进制”就是任意命令执行的入口（PLAN.md 第 4 节）。
//
// 判据的分层：
//   - **必需**：useradd / usermod / userdel / getent / passwd / chpasswd。缺任何一个
//     都既建不了也读不回，status 只能是 blocked。
//   - **按档可选**：`/bin/rbash` 与 `/usr/sbin/nologin`。缺哪一档就少哪一档
//     （ShellProfiles 只列真的能用的），两档都缺才 blocked——那台机器上没有能落的壳。
//   - **按动作可选**：visudo。没有它不影响建账号与写公钥，只让 `sudo_rule.*`
//     变成 unsupported（判决层本来也会用 SUDO_VISUDO_UNAVAILABLE 拒，这里提前说出来）。
//
// 与 shares 的探针一样缓存 30 秒：同一窗口里刷两次页面，看到的必须是同一份结论。

import (
	"context"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"upanel/internal/shares"
)

// 能力状态的取值。直接复用 shares 的两个词：available / blocked 在这里的意思
// 完全一样（“能做”/“不能做，原因见 reason”），再造一对同义词只会让页面判断两次。
const (
	CapabilityAvailable = shares.CapabilityAvailable
	CapabilityBlocked   = shares.CapabilityBlocked
)

// 壳档路径（闭集，与 commands.go 的 ShellDisabled / ShellRestricted 同源）。
const (
	shellPathRestricted = ShellRestricted
	shellPathDisabled   = ShellDisabled
)

// defaultCapabilityTTL 与 shares 的 30 秒一致。
const defaultCapabilityTTL = 30 * time.Second

// AccountTools 是探到的工具与壳的绝对路径。空字符串表示没找到。
//
// 壳也放在这里而不是另开一个结构：页面只需要知道“这一档能不能选”，而
// `shell_profiles` 这个派生列表就是从这几个字段算出来的。
type AccountTools struct {
	Useradd  string `json:"useradd,omitempty"`
	Usermod  string `json:"usermod,omitempty"`
	Userdel  string `json:"userdel,omitempty"`
	Getent   string `json:"getent,omitempty"`
	Passwd   string `json:"passwd,omitempty"`
	Chpasswd string `json:"chpasswd,omitempty"`
	// Visudo 为空表示这台机器不能写 sudo 规则（不是“不能管账号”）。
	Visudo string `json:"visudo,omitempty"`
	// RestrictedShell / DisabledShell 是两档壳的落点。
	RestrictedShell string `json:"restricted_shell,omitempty"`
	DisabledShell   string `json:"disabled_shell,omitempty"`
}

// Capability 是账号页要读的那份能力报告。
//
// 字段比 shares.Capability 少得多，因为账号这件事依赖的东西少得多：没有配置
// 文件、没有守护进程、没有 include 目录。页面上“账号管理可用/不可用”这一句
// 的唯一来源就是这里的 Status。
type Capability struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`

	Tools AccountTools `json:"tools"`

	// SudoRuleWrite 是 available / unsupported：判据只有 visudo 在不在。
	//
	// 与 shares 的 config_write 同一个词表，页面不必学第二套。
	SudoRuleWrite string `json:"sudo_rule_write"`
	// ShellProfiles 是这台机器上真的能落的壳档（disabled / restricted 的子集）。
	//
	// 列出来而不是让人去猜：`restricted` 要 /bin/rbash，缺了它就只能选 disabled，
	// 而这个结论在页面上必须是一句话，不是一次失败之后才知道。
	ShellProfiles []string `json:"shell_profiles"`

	// Notices 是人读的补充说明（缺 visudo、缺某一档壳），不改变 status。
	Notices []shares.Blocker `json:"notices"`
	// CollectedAt 是这份能力的采集时间。
	CollectedAt time.Time `json:"collected_at"`
}

// Available 报告账号能力是否可用。页面与测试都用它，不各自解释 status 字符串。
func (c Capability) Available() bool { return c.Status == CapabilityAvailable }

// ProberOptions 是探针的接缝：默认实现读真实文件系统。
type ProberOptions struct {
	// Stat 读文件状态（生产实现是 os.Stat）。
	Stat func(name string) (os.FileInfo, error)
	// Now 提供时间（可注入）。
	Now func() time.Time
	// TTL 是缓存时长；0 表示用默认 30 秒。
	TTL time.Duration
	// Paths 允许覆盖工具路径。**只给测试用**：生产接线不传它，因此“面板执行哪个
	// 二进制”在运行时不可配置。
	Paths AccountTools
}

// Prober 是带 30 秒缓存的能力探针。
type Prober struct {
	opts ProberOptions

	mu       sync.Mutex
	cached   Capability
	cachedAt time.Time
	primed   bool
}

// NewProber 构造探针。
func NewProber(opts ProberOptions) *Prober {
	if opts.Stat == nil {
		opts.Stat = os.Stat
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}
	if opts.TTL <= 0 {
		opts.TTL = defaultCapabilityTTL
	}
	return &Prober{opts: opts}
}

// SystemProber 返回读真实文件系统的探针。
func SystemProber() *Prober { return NewProber(ProberOptions{}) }

// Capability 返回最近一次探测结果（超过 TTL 就重新探）。
//
// ctx 现在没用上：探测只 stat 本地路径。留着这个参数是为了以后真要跑一次
// `useradd --version` 时不必改签名——那时候它就会被用上。
func (p *Prober) Capability(ctx context.Context) Capability {
	_ = ctx
	now := p.opts.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.primed && now.Sub(p.cachedAt) < p.opts.TTL {
		return p.cached
	}
	probed := p.probe(now)
	p.cached = probed
	p.cachedAt = now
	p.primed = true
	return probed
}

// Invalidate 丢掉缓存。改动工具集合的操作（现在没有，将来可能有包管理动作）之后
// 必须调它，否则接下来 30 秒里页面看到的还是旧结论。
func (p *Prober) Invalidate() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.primed = false
}

// probe 做一次真实探测。
func (p *Prober) probe(now time.Time) Capability {
	out := Capability{
		Tools:         p.opts.Paths,
		ShellProfiles: []string{},
		Notices:       []shares.Blocker{},
		CollectedAt:   now.Truncate(time.Second),
	}
	pick := func(name string, override string, candidates []string, missing *[]string) string {
		if override != "" {
			return override
		}
		for _, c := range candidates {
			if fi, err := p.opts.Stat(c); err == nil && fi.Mode().IsRegular() {
				return c
			}
		}
		*missing = append(*missing, name)
		return ""
	}

	missing := make([]string, 0, 6)
	out.Tools.Useradd = pick("useradd", out.Tools.Useradd, []string{ProgramUseradd}, &missing)
	out.Tools.Usermod = pick("usermod", out.Tools.Usermod, []string{ProgramUsermod}, &missing)
	out.Tools.Userdel = pick("userdel", out.Tools.Userdel, []string{ProgramUserdel}, &missing)
	out.Tools.Getent = pick("getent", out.Tools.Getent, []string{ProgramGetent}, &missing)
	out.Tools.Passwd = pick("passwd", out.Tools.Passwd, []string{ProgramPasswd}, &missing)
	out.Tools.Chpasswd = pick("chpasswd", out.Tools.Chpasswd, []string{ProgramChpasswd}, &missing)

	// 壳：按档可选。缺一档只少一档。
	shellMissing := make([]string, 0, 2)
	out.Tools.RestrictedShell = pick("rbash", out.Tools.RestrictedShell,
		[]string{shellPathRestricted}, &shellMissing)
	out.Tools.DisabledShell = pick("nologin", out.Tools.DisabledShell,
		[]string{shellPathDisabled}, &shellMissing)
	if out.Tools.RestrictedShell != "" {
		out.ShellProfiles = append(out.ShellProfiles, string(ProfileRestricted))
	}
	if out.Tools.DisabledShell != "" {
		out.ShellProfiles = append(out.ShellProfiles, string(ProfileDisabled))
	}
	sort.Strings(out.ShellProfiles)
	if len(shellMissing) > 0 {
		out.Notices = append(out.Notices, shares.Blocker{
			Code: NoticeAccountShellMissing,
			Message: "没找到 " + strings.Join(shellMissing, "、") +
				"：对应的壳档不能用来建账号（另一档仍然可用）",
		})
	}

	// visudo：只影响 sudo 规则，不影响账号本身。
	visudoMissing := make([]string, 0, 1)
	out.Tools.Visudo = pick("visudo", out.Tools.Visudo, []string{ProgramVisudo}, &visudoMissing)
	if out.Tools.Visudo == "" {
		out.SudoRuleWrite = shares.ConfigWriteUnsupported
		out.Notices = append(out.Notices, shares.Blocker{
			Code:    NoticeSudoVisudoUnavailable,
			Message: "没找到 visudo：" + "sudo 规则不能下发，账号本身仍然可以管理",
		})
	} else {
		out.SudoRuleWrite = shares.ConfigWriteAvailable
	}

	if len(missing) > 0 {
		out.Status = CapabilityBlocked
		out.Reason = "缺少 " + strings.Join(missing, "、") + "；" +
			"受管理账号因此不可用（本面板不自动安装软件包）"
		return out
	}
	if len(out.ShellProfiles) == 0 {
		out.Status = CapabilityBlocked
		out.Reason = "这台机器上两档壳（" + shellPathRestricted + "、" + shellPathDisabled +
			"）都不可用：新建账号落不到任何一个合法的登录壳上"
		return out
	}
	out.Status = CapabilityAvailable
	return out
}
