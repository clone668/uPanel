package network

// P14.2：真正动手的那一层。改一张物理网卡的 IPv4，写一份面板自己的 netplan 文件，
// 并且在改完之后**先留下一条能自己退回去的路**。
//
// 这份实现的重点不在"怎么把地址写进去"（那只有十几行），而在下面四件事：
//
//  1. **先武装、再 apply**。`netplan apply` 有可能把管理员关在门外，也可能卡住不动。
//     回滚 timer 在 apply 之前就已经挂好，所以"改坏了没人管"这个状态不存在。
//  2. **回滚不依赖面板**。timer 是 systemd 的，执行的是代理自己的二进制，读的是磁盘上
//     的记录文件。面板停掉、浏览器关掉、数据库坏了，都不影响它到期把配置退回去。
//  3. **不信退出码，只信读回来的内核事实**。`netplan apply` 返回 0 而地址没上去是常见
//     的（渲染错、其他文件覆盖、networkd 没重载）。所以每次改完都要读回地址与路由，
//     对不上就立刻自己回滚，不等 timer。
//  4. **失败不重试**。回滚任何一步失败，记录写 `failed` 与原因就停手：自动重试会把
//     "一个说不清的状态"变成"两个"。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"upanel/internal/syscmd"
)

// 动作结果（与 internal/agent 的 Result.Outcome 闭集对齐）。
//
// 这里刻意**没有** `failed`：代理那边的闭集是 verified/accepted/rolled_back/partial/unknown，
// 而"失败"本身不是一个状态——它总是落在这三种之一：改动生效并核验过（verified）、
// 出了差错但已经退回改之前（rolled_back）、出了差错且退不回去（unknown，要人去现场看）。
// 把失败写成 "failed" 会让前端多一个不知道该显示什么的分支，也会让"机器现在是什么样"
// 这个问题失去答案。
const (
	OutcomeVerified   = "verified"
	OutcomeRolledBack = "rolled_back"
	OutcomeUnknown    = "unknown"
)

// rollbackTokenPrefix 是回滚记录标识的前缀。
const rollbackTokenPrefix = "nrb_"

// 这一层拼路径一律用 path.Join，不用 filepath.Join。
//
// 这里拼的全是 Linux 上的绝对路径（/etc/netplan、/var/lib/upanel-agent/…），
// 与"在哪个操作系统上编译"无关。用 filepath.Join 会让同一个函数在开发机（Windows）
// 上拼出反斜杠路径——生产不会出错，但单测里的假文件系统对不上，而这种"只在 CI 里
// 假的失败"最容易被改成"放宽断言"而不是"修对"。
//
// 常用工具与服务端命令的路径候选。
var (
	netplanCandidates   = []string{"/usr/sbin/netplan", "/sbin/netplan", "/usr/bin/netplan"}
	systemctlCandidates = []string{"/usr/bin/systemctl", "/bin/systemctl"}
)

// CommandRunner 是把命令交给内核的唯一出口（生产实现转给 syscmd.Run）。
//
// 与本仓库其它领域包（storage、shares、proxies）保持同一个形状：换一个包不需要
// 换一套接缝，测试里的假执行器也能在几个包之间直接复用。
type CommandRunner interface {
	Run(ctx context.Context, spec syscmd.Spec, limits syscmd.Limits) (syscmd.Result, error)
}

// CommandRunnerFunc 让一个函数满足 CommandRunner（单测用）。
type CommandRunnerFunc func(ctx context.Context, spec syscmd.Spec, limits syscmd.Limits) (syscmd.Result, error)

// Run 实现 CommandRunner。
func (f CommandRunnerFunc) Run(ctx context.Context, spec syscmd.Spec, limits syscmd.Limits) (syscmd.Result, error) {
	return f(ctx, spec, limits)
}

// WriteFileSystem 是这一包写盘用到的全部操作。
//
// 与 P14.1 的只读 FileSystem 分开声明：只读采集不该顺手拿到写能力，反过来也一样——
// 写路径要能原子替换、要能建目录、要能删文件，这些在只读那一侧一个都不该出现。
type WriteFileSystem interface {
	ReadFile(name string) ([]byte, error)
	ListDir(dir string) ([]string, error)
	WriteFile(name string, data []byte, perm os.FileMode) error
	Rename(oldpath, newpath string) error
	// SyncDir 把目录项刷到盘上。rename 只是让新文件在**运行中的**文件系统里可见，
	// 目录项本身要 fsync 过才在掉电后还成立。生产实现里是"打开目录 + Sync"。
	SyncDir(dir string) error
	Remove(name string) error
	MkdirAll(dir string, perm os.FileMode) error
	Readlink(name string) (string, error)
	Stat(name string) (os.FileInfo, error)
}

// ApplyPaths 是这一层会写到的三个目录。
//
// 零值即生产路径。可注入不是为了"让生产可配置"，而是为了在单测里把"候选文件不合格时
// 正式文件一个字节都没动""回滚只删自己那份文件"这类不变量反复验证。
type ApplyPaths struct {
	NetplanDir string
	SystemdDir string
	StateDir   string
}

func (p ApplyPaths) withDefaults() ApplyPaths {
	if p.NetplanDir == "" {
		p.NetplanDir = "/etc/netplan"
	}
	if p.SystemdDir == "" {
		p.SystemdDir = "/etc/systemd/system"
	}
	if p.StateDir == "" {
		p.StateDir = "/var/lib/upanel-agent/network/rollback"
	}
	return p
}

// ApplyOptions 是写路径的装配参数。
type ApplyOptions struct {
	// Read 是 P14.1 的只读采集依赖（观察现场用，必需）。
	Read Deps
	// Runner 是命令执行器（必需）。
	Runner CommandRunner
	// FS 是文件系统接缝；零值用真实 os 调用。
	FS WriteFileSystem
	// Executable 返回代理自己的二进制路径（回滚单元用它，必需）。
	Executable func() (string, error)
	// ConfigPath 是代理配置文件路径，写进回滚单元的命令行。
	//
	// 可以为空：那条子命令不读配置（它要恢复的东西全在回滚记录里），这一项只是让
	// 现场的单元文件能回答"这条回滚是从哪个配置启动的代理"。
	ConfigPath string
	// Paths 是三个目录；零值取生产路径。
	Paths ApplyPaths
	// Now 提供时间（可注入）。
	Now func() time.Time
	// Stat 用于"目录/文件在不在"；零值用 os.Stat。
	Stat func(name string) (os.FileInfo, error)
}

// ApplyEngine 是网络写路径的入口。
type ApplyEngine struct {
	opts  ApplyOptions
	paths ApplyPaths
	fs    WriteFileSystem
	tools tools

	// dhcpWait 是切到 DHCP 之后等租约出现的时间窗。
	//
	// 单独拎出来当字段，是为了让单测能把它压成 0：这个等待**只能**靠真实的定时器
	// 走完（轮询间隔是 time.After），压不住的话"等不到地址"这条分支在测试里要跑
	// 满 30 秒，谁都不愿意写、更不愿意跑。
	dhcpWait time.Duration
}

// dhcpVerifyWindow 比静态地址那 15 秒宽。
//
// 静态地址是 `netplan apply` 之后 networkd 照文件配上去的，几秒内就在；DHCP 还得
// 等服务器回应（真机上可能跨一台中继），15 秒在慢网络里会把"正常但慢"读成"没生效"，
// 然后触发一次完全没必要的回滚——把用户刚做的改动撤掉，还留下一条 rolled_back 记录。
const dhcpVerifyWindow = 30 * time.Second

type tools struct {
	netplan   string
	systemctl string
}

// NewApplyEngine 构造写路径。缺必需项时返回错误：宁可启动时报错，也不要等到用户
// 点了"应用"才给一个说不清原因的 503。
func NewApplyEngine(opts ApplyOptions) (*ApplyEngine, error) {
	if opts.Runner == nil {
		return nil, errors.New("network: 写路径缺少命令执行器")
	}
	if opts.Executable == nil {
		return nil, errors.New("network: 写路径缺少 代理二进制路径")
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}
	if opts.FS == nil {
		opts.FS = osWriteFS{}
	}
	if opts.Stat == nil {
		opts.Stat = os.Stat
	}
	if opts.Read.FS == nil {
		opts.Read.FS = opts.FS
	}
	if opts.Read.Now == nil {
		opts.Read.Now = opts.Now
	}
	e := &ApplyEngine{opts: opts, paths: opts.Paths.withDefaults(), fs: opts.FS,
		dhcpWait: dhcpVerifyWindow}
	if p := firstExisting(opts.FS, netplanCandidates); p != "" {
		e.tools.netplan = p
	}
	if p := firstExisting(opts.FS, systemctlCandidates); p != "" {
		e.tools.systemctl = p
	}
	return e, nil
}

func firstExisting(fsys WriteFileSystem, candidates []string) string {
	for _, name := range candidates {
		if _, err := fsys.Stat(name); err == nil {
			return name
		}
	}
	return ""
}

// now 是这一层唯一的时钟来源。
//
// 到期判断、窗口剩余、记录时间戳全部走它：单测里把时钟拔到 119 秒、121 秒，就能
// 验证"窗口之内还能确认""窗口之外只能回滚"，而不必真的等两分钟。
func (e *ApplyEngine) now() time.Time { return e.opts.Now().UTC() }

// ---- 现场 ----

// ConfigState 是 `GET /network/config` 的内容：可改的那张网卡现在是什么样、
// 指纹是多少、有没有一条还没确认的回滚。
type ConfigState struct {
	CollectedAt  string     `json:"collected_at"`
	Capability   Capability `json:"capability"`
	Interface    string     `json:"interface,omitempty"`
	PermanentMAC string     `json:"permanent_mac,omitempty"`
	// ConfigHash 只在 Editable 为 true 时给出：算不出来就别给一个"看起来能提交"的值。
	ConfigHash string `json:"config_hash,omitempty"`
	// Mode 是**内核渲染产物**里的模式（dhcp/static/unknown），不是我们文件里写了什么。
	Mode string `json:"mode"`
	// IPv4 是现在这网卡上的地址（多个时只给第一个 global 的：面板只改一个地址）。
	IPv4 *IPv4Setting `json:"ipv4,omitempty"`
	// PanelFile 是面板自己那份文件；不存在时 null——"我们没有管它"是个明确状态。
	PanelFile    *panelFileRef `json:"panel_file"`
	NetplanFiles []netplanFile `json:"netplan_files"`
	Unsupported  []string      `json:"unsupported"`
	Pending      *RollbackView `json:"pending"`
	Issues       []Issue       `json:"issues"`
}

// RollbackView 是对外暴露的回滚记录视图（不含文件内容与命令行）。
type RollbackView struct {
	Token            string `json:"token"`
	Interface        string `json:"interface"`
	Mode             string `json:"mode"`
	CandidateAddress string `json:"candidate_address,omitempty"`
	AppliedAt        string `json:"applied_at"`
	Deadline         string `json:"deadline"`
	SecondsLeft      int    `json:"seconds_left"`
	State            string `json:"state"`
	Result           string `json:"result,omitempty"`
}

// Current 汇总一次现场：能不能改、指纹、现况、面板文件、待确认的回滚。
func (e *ApplyEngine) Current(ctx context.Context) ConfigState {
	list := Interfaces(ctx, e.opts.Read)
	out := ConfigState{
		CollectedAt:  e.now().Format(time.RFC3339),
		Capability:   list.Capability,
		NetplanFiles: []netplanFile{},
		Unsupported:  []string{},
		Issues:       append([]Issue{}, list.Issues...),
		Mode:         "unknown",
	}
	if iface := findInterface(list.Interfaces, list.Capability.Target); iface != nil {
		out.Interface = iface.Name
		out.PermanentMAC = strings.ToLower(iface.MAC)
		out.IPv4 = firstGlobalIPv4(iface)
	}
	// 面板文件与指纹：能不能改都读一遍。不能改的机器上"我们曾经管过它"这个事实
	// 仍然要看得见，否则一台被改成 NetworkManager 的机器会静默丢掉历史。
	if out.Interface != "" {
		view, err := inspectNetplan(e.fs, e.paths.NetplanDir, out.Interface, out.PermanentMAC)
		if err != nil {
			out.Issues = append(out.Issues, Issue{Source: "netplan", Code: issueCode(err),
				Detail: err.Error()})
		} else {
			out.NetplanFiles = view.Files
			out.Unsupported = view.Unsupported
			if ref := panelRefOf(view); ref != nil {
				out.PanelFile = ref
			}
			if hash, _, herr := view.configHash(); herr == nil {
				out.ConfigHash = hash
			}
			out.Mode = e.observeMode(ctx, out.Interface)
		}
	}
	if pending, perr := e.pendingView(); perr != nil {
		// 读不出回滚记录是个要摆到页面上的事实：它可能意味着"有一条改动正在等确认，
		// 但面板已经看不清它了"，那比没有记录更值得让人知道。
		out.Issues = append(out.Issues, Issue{Source: "rollback", Code: issueCode(perr),
			Detail: "读回滚记录失败：" + perr.Error()})
	} else {
		out.Pending = pending
	}
	return out
}

func panelRefOf(view netplanView) *panelFileRef {
	for _, f := range view.Files {
		if f.Name == view.PanelName {
			ref := panelFileRef{Name: f.Name, Sha256: f.Sha256}
			return &ref
		}
	}
	return nil
}

func findInterface(list []Interface, name string) *Interface {
	if name == "" {
		return nil
	}
	for i := range list {
		if list[i].Name == name {
			return &list[i]
		}
	}
	return nil
}

// firstGlobalIPv4 取这张网卡上第一个全局 IPv4 地址。
//
// 面板只改一个地址，所以"有多个"这件事在这里只体现为"报了第一个"——页面上的
// 编辑框只填得下一个。多地址的配置在 netplan 巡检里已经是 `unsupported`，
// 走不到这里。
func firstGlobalIPv4(iface *Interface) *IPv4Setting {
	for _, a := range iface.Addresses {
		if a.Family != FamilyIPv4 || a.Scope != "global" {
			continue
		}
		return &IPv4Setting{Address: a.Address, PrefixLength: a.PrefixLen}
	}
	return nil
}

// observeMode 从 netplan 渲染出来的 networkd 产物里读"这张网卡现在怎么拿地址"。
//
// 不读我们自己的 YAML：那份文件说的是"我们要它怎样"，而渲染产物才是"它现在怎样"。
// 读不到就写 unknown，不猜 dhcp 也不猜 static。
func (e *ApplyEngine) observeMode(ctx context.Context, iface string) string {
	name := path.Join(e.opts.Read.runDir(), "systemd/network", "10-netplan-"+iface+".network")
	data, err := e.fs.ReadFile(name)
	if err != nil {
		return "unknown"
	}
	text := string(data)
	dhcp := ""
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "DHCP=") {
			dhcp = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(line, "DHCP=")))
		}
	}
	switch {
	case dhcp == "yes" || dhcp == "ipv4" || dhcp == "true":
		return ModeDHCP
	case strings.Contains(text, "\nAddress=") || strings.HasPrefix(text, "Address="):
		return ModeStatic
	case dhcp == "no" || dhcp == "false":
		return ModeStatic
	default:
		return "unknown"
	}
}

// ---- 预检 ----

// Assess 计算 network.apply 的预检。
func (e *ApplyEngine) Assess(ctx context.Context, params ApplyParams) (Assessment, error) {
	def, err := ValidateDefinition(params.Definition)
	if err != nil {
		return refused(definitionBlockers(err)), nil
	}
	params.InterfaceID = strings.TrimSpace(params.InterfaceID)
	if !ValidInterfaceName(params.InterfaceID) {
		return refused([]Notice{{Code: CodeDefinitionInvalid,
			Message: "interface_id 必须是普通网卡名（字母开头，字母数字与 _.- 组成），收到 " +
				strconv.Quote(params.InterfaceID)}}), nil
	}
	normalized := ApplyParams{InterfaceID: params.InterfaceID,
		ExpectedConfigHash: strings.TrimSpace(params.ExpectedConfigHash), Definition: def}

	ready, blockers, warnings, impacts, ident, stateVersion, err := e.inspectTarget(ctx, params.InterfaceID, normalized.ExpectedConfigHash)
	if err != nil {
		return Assessment{}, err
	}
	out := Assessment{
		Normalized:          normalized,
		DisplayName:         params.InterfaceID,
		IdentityFingerprint: ident,
		StateVersion:        stateVersion,
		Allowed:             len(blockers) == 0,
		Blockers:            blockers,
		Warnings:            warnings,
		Impacts:             impacts,
		Locks:               []string{NetworkLockName},
	}
	if !ready {
		return out, nil
	}
	if pending, err := e.pendingRecord(); err != nil {
		return Assessment{}, err
	} else if pending != nil {
		out.Allowed = false
		out.Blockers = append(out.Blockers, Notice{Code: CodeRollbackPending, Message: fmt.Sprintf(
			"上一次改动（%s，到期 %s）还没有确认或回滚：先处理它，不许叠加",
			pending.Mode, pending.Deadline)})
	}
	out.Impacts = append(out.Impacts, Impact{Kind: "network", Description: describeDefinition(params.InterfaceID, *def)})
	out.Impacts = append(out.Impacts, Impact{Kind: "rollback", Description: fmt.Sprintf(
		"改完 %d 秒内不确认，系统会自动把 %s 的配置退回改之前的样子（不依赖面板是否在运行）",
		int(RollbackWindow.Seconds()), params.InterfaceID)})
	return out, nil
}

// inspectTarget 是 apply 与 confirm/rollback 共用的"目标还在不在、能不能改"的检查。
func (e *ApplyEngine) inspectTarget(ctx context.Context, iface, expectedHash string) (
	ready bool, blockers, warnings []Notice, impacts []Impact, ident, stateVersion string, err error) {
	list := runInterfaces(ctx, e.opts.Read)
	capb := list.Capability
	if !capb.Editable {
		msg := "这台机器的网络配置不在面板可改范围内"
		if capb.Backend != "" {
			msg += "（后端 " + capb.Backend + "）"
		}
		if len(capb.Reasons) > 0 {
			msg += "：" + strings.Join(capb.Reasons, "；")
		}
		return false, []Notice{{Code: CodeUnsupported, Message: msg}}, nil, nil, "", "", nil
	}
	if capb.Target != "" && capb.Target != iface {
		return false, []Notice{{Code: CodeUnsupported, Message: fmt.Sprintf(
			"这台机器只能改 %s（唯一一张物理网卡），不是 %s", capb.Target, iface)}}, nil, nil, "", "", nil
	}
	target := findInterface(list.Interfaces, iface)
	if target == nil {
		return false, []Notice{{Code: CodeTargetChanged,
			Message: "读不到网卡 " + iface + "：它可能被改名或拔掉了"}}, nil, nil, "", "", nil
	}
	mac := strings.ToLower(target.MAC)
	ident = "net:" + iface + ":" + mac
	view, verr := inspectNetplan(e.fs, e.paths.NetplanDir, iface, mac)
	if verr != nil {
		return false, nil, nil, nil, ident, "", &TargetIOError{Err: verr}
	}
	hash, _, herr := view.configHash()
	if herr != nil {
		return false, nil, nil, nil, ident, "", herr
	}
	stateVersion = hash
	if len(view.Unsupported) > 0 {
		return false, []Notice{{Code: CodeUnsupported,
				Message: "这台机器的 netplan 里有面板不能安全覆盖的配置：" + strings.Join(view.Unsupported, "；")}},
			nil, nil, ident, stateVersion, nil
	}
	if expectedHash != "" && expectedHash != hash {
		// 哈希对不上有两种原因，界面上是两条完全不同的路：配置被改过（刷新重来），
		// 或者这张网卡的永久 MAC 变了（这台机器的网卡换过了，做任何事之前都得先看现场）。
		// 用"把 MAC 抹掉再算一次"把两者分开：指纹的原像是确定性的，抹掉 MAC 之后仍然
		// 对得上，说明差别只出在 MAC 这一项。
		if alt, _, aerr := view.configHashWithMAC(""); aerr == nil && alt == expectedHash {
			return false, []Notice{{Code: CodeTargetChanged, Message: fmt.Sprintf(
					"%s 的永久 MAC 现在是 %s，与预检时那份配置不是同一张网卡", iface, mac)}},
				nil, nil, ident, stateVersion, nil
		}
		return false, []Notice{{Code: CodeConfigChanged, Message: fmt.Sprintf(
				"配置在预检之后变了：期望 %s，现在是 %s。请刷新后重新预检", shortHash(expectedHash), shortHash(hash))}},
			nil, nil, ident, stateVersion, nil
	}
	blockers, warnings = e.targetNotices(ctx, target, iface)
	return true, blockers, warnings, impacts, ident, stateVersion, nil
}

// targetNotices 是"目标本身没毛病，但改之前应该知道"的那些话。
//
// 两条告警都在回答同一个问题：**改完之后你还能不能进得来**。
// 回答它有两种证据，各自能单独成立：
//
//   - 面板自己的监听在通配地址上，或者绑在目标网卡的网段里：那这次改动就可能是
//     "把梯子踢掉"；
//   - 这台机器的全部默认路由都在目标网卡上：同样，改完之后外面就找不到它了。
//
// 读不到监听表时不硬猜（不报这条），因为"我看不到"不等于"没有"。
func (e *ApplyEngine) targetNotices(ctx context.Context, target *Interface, iface string) (
	blockers, warnings []Notice) {
	if target.Carrier != nil && !*target.Carrier {
		warnings = append(warnings, Notice{Code: CodeNoCarrier,
			Message: "这张网卡现在没有载波（链路没连上）：改完也不会通"})
	}
	if subnets := targetSubnets(target); len(subnets) > 0 {
		listeners := Listeners(e.opts.Read)
		for _, l := range listeners.Listeners {
			if !strings.Contains(strings.ToLower(l.Process+" "+l.Unit), "upanel-web") {
				continue
			}
			if l.Wildcard || inAnySubnet(l.Address, subnets) {
				warnings = append(warnings, Notice{Code: CodeManagementPath, Message: fmt.Sprintf(
					"面板自己就监听着 %s:%d，而它落在 %s 上：改完之后浏览器可能立刻断开，"+
						"%d 秒内不确认会自动退回", l.Address, l.Port, iface, int(RollbackWindow.Seconds()))})
				return nil, warnings
			}
		}
	}
	// 默认路由是不是只有这一张网卡在扛——是的话，改它等于改这台机器唯一的出口。
	routes := Routes(ctx, e.opts.Read)
	defaults := 0
	onTarget := 0
	for _, r := range routes.Routes {
		if !r.Default {
			continue
		}
		defaults++
		if r.Device == iface {
			onTarget++
		}
	}
	if defaults > 0 && defaults == onTarget {
		warnings = append(warnings, Notice{Code: CodeManagementPath, Message: fmt.Sprintf(
			"这台机器全部的默认路由都在 %s 上：改完之后浏览器与面板可能立刻断开，"+
				"%d 秒内不确认会自动退回", iface, int(RollbackWindow.Seconds()))})
	}
	return nil, warnings
}

// targetSubnets 返回这张网卡上现有的 IPv4 网段（用于判断"监听是不是落在它上面"）。
func targetSubnets(iface *Interface) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(iface.Addresses))
	for _, a := range iface.Addresses {
		if a.Family != FamilyIPv4 {
			continue
		}
		addr, err := netip.ParseAddr(a.Address)
		if err != nil {
			continue
		}
		out = append(out, netip.PrefixFrom(addr, a.PrefixLen).Masked())
	}
	return out
}

func inAnySubnet(raw string, subnets []netip.Prefix) bool {
	addr, err := netip.ParseAddr(strings.Trim(strings.TrimSpace(raw), "[]"))
	if err != nil {
		return false
	}
	for _, p := range subnets {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

func definitionBlockers(err error) []Notice {
	return []Notice{{Code: CodeDefinitionInvalid, Message: strings.TrimPrefix(err.Error(), "network: ")}}
}

func refused(blockers []Notice) Assessment {
	return Assessment{Allowed: false, Blockers: blockers, Locks: []string{NetworkLockName}}
}

// AssessConfirm 计算 network.confirm 的预检。
func (e *ApplyEngine) AssessConfirm(ctx context.Context, params ConfirmParams) (Assessment, error) {
	params.InterfaceID = strings.TrimSpace(params.InterfaceID)
	params.Token = strings.TrimSpace(params.Token)
	params.ArrivedOn = normalizeArrivedOn(params.ArrivedOn)
	normalized := params
	out := Assessment{
		Normalized:          normalized,
		DisplayName:         params.InterfaceID,
		IdentityFingerprint: "net:" + params.InterfaceID,
		Allowed:             true,
		Locks:               []string{NetworkLockName},
	}
	rec, blockers := e.confirmChecks(ctx, params)
	if len(blockers) > 0 {
		out.Allowed = false
		out.Blockers = blockers
		return out, nil
	}
	out.StateVersion = rec.Deadline
	out.Impacts = append(out.Impacts, Impact{Kind: "network", Description: fmt.Sprintf(
		"保留 %s 的新配置（%s），停掉回滚 timer", params.InterfaceID, describeRecord(rec))})
	return out, nil
}

// confirmChecks 是确认的三条硬条件，预检与执行都会跑一遍（执行时再跑一次，
// 因为预检之后可能已经到期、地址可能已经掉了）。
func (e *ApplyEngine) confirmChecks(ctx context.Context, params ConfirmParams) (*RollbackRecord, []Notice) {
	rec, err := e.pendingRecord()
	if err != nil {
		return nil, []Notice{{Code: CodeNoPendingRollback, Message: "读回滚记录失败：" + err.Error()}}
	}
	if rec == nil {
		return nil, []Notice{{Code: CodeNoPendingRollback,
			Message: "没有待确认的改动：可能已经确认过、已经回滚，或者根本没改过"}}
	}
	if params.Token != "" && rec.Token != params.Token {
		return nil, []Notice{{Code: CodeNoPendingRollback,
			Message: "回滚标识与待确认的记录不一致（" + rec.Token + "）"}}
	}
	if rec.Interface != params.InterfaceID {
		return nil, []Notice{{Code: CodeTargetChanged,
			Message: "待确认的改动属于 " + rec.Interface + "，不是 " + params.InterfaceID}}
	}
	if !e.now().Before(rec.deadline()) {
		return nil, []Notice{{Code: CodeRollbackExpired, Message: fmt.Sprintf(
			"确认来晚了：%s 的记录已在 %s 到期，接下来（或已经）由回滚 timer 处理",
			rec.Token, rec.Deadline)}}
	}
	// 只有静态配置有"候选地址"这个可比的凭据；DHCP 拿到的地址是随机的，
	// 用它做门槛等于让确认永远失败。
	if rec.CandidateAddress != "" {
		if params.ArrivedOn == "" {
			return nil, []Notice{{Code: CodeConfirmWrongAddress,
				Message: "确认请求没有带来到达地址：无法证明你是在新地址上操作的"}}
		}
		if params.ArrivedOn != rec.CandidateAddress {
			return nil, []Notice{{Code: CodeConfirmWrongAddress, Message: fmt.Sprintf(
				"确认必须从新地址 %s 发起，这次是从 %s 来的：在旧地址上点确认等于没验过新地址",
				rec.CandidateAddress, params.ArrivedOn)}}
		}
	}
	live, addrErr := e.observedIPv4(ctx, rec.Interface)
	if addrErr != nil {
		return nil, []Notice{{Code: CodeTargetChanged, Message: "读回地址失败：" + addrErr.Error()}}
	}
	if rec.CandidateAddress != "" && live != rec.CandidateAddress {
		return nil, []Notice{{Code: CodeTargetChanged, Message: fmt.Sprintf(
			"%s 上现在不是候选地址（读回 %q）：改动没有真正生效，不能确认", rec.Interface, live)}}
	}
	if live == "" {
		return nil, []Notice{{Code: CodeTargetChanged,
			Message: rec.Interface + " 上现在没有全局 IPv4 地址：改动没有真正生效，不能确认"}}
	}
	return rec, nil
}

// AssessRollback 计算 network.rollback 的预检。
func (e *ApplyEngine) AssessRollback(ctx context.Context, params RollbackParams) (Assessment, error) {
	params.InterfaceID = strings.TrimSpace(params.InterfaceID)
	params.Token = strings.TrimSpace(params.Token)
	out := Assessment{
		Normalized:          params,
		DisplayName:         params.InterfaceID,
		IdentityFingerprint: "net:" + params.InterfaceID,
		Allowed:             true,
		Locks:               []string{NetworkLockName},
	}
	rec, err := e.pendingRecord()
	if err != nil {
		out.Allowed = false
		out.Blockers = []Notice{{Code: CodeNoPendingRollback, Message: "读回滚记录失败：" + err.Error()}}
		return out, nil
	}
	if rec == nil {
		out.Allowed = false
		out.Blockers = []Notice{{Code: CodeNoPendingRollback,
			Message: "没有待回滚的改动：可能已经确认或已经回滚过"}}
		return out, nil
	}
	if params.Token != "" && rec.Token != params.Token {
		out.Allowed = false
		out.Blockers = []Notice{{Code: CodeNoPendingRollback,
			Message: "回滚标识与待确认的记录不一致（" + rec.Token + "）"}}
		return out, nil
	}
	if rec.Interface != params.InterfaceID {
		out.Allowed = false
		out.Blockers = []Notice{{Code: CodeTargetChanged,
			Message: "待回滚的改动属于 " + rec.Interface + "，不是 " + params.InterfaceID}}
		return out, nil
	}
	out.StateVersion = rec.Deadline
	out.Impacts = append(out.Impacts, Impact{Kind: "rollback", Description: fmt.Sprintf(
		"立刻把 %s 退回改之前的样子（%s）", rec.Interface, describePrevious(rec))})
	return out, nil
}

// ---- 执行 ----

// WriteRequest 是执行一次网络写动作的入参（与其它动作执行器同形）。
type WriteRequest struct {
	Action               string
	Params               any
	ExpectedIdentity     string
	ExpectedStateVersion string
	OperationID          string
	Progress             func(step string, fields map[string]any)
}

// WriteReport 是执行结果。
type WriteReport struct {
	Outcome      string
	Observed     map[string]any
	Data         map[string]any
	RecoveryHint string
}

// ExecuteWrite 执行 apply / confirm / rollback。
func (e *ApplyEngine) ExecuteWrite(ctx context.Context, req WriteRequest) (WriteReport, error) {
	switch req.Action {
	case ActionApply:
		params, ok := req.Params.(ApplyParams)
		if !ok {
			return WriteReport{}, fmt.Errorf("%w: 参数类型不对", ErrDefinitionInvalid)
		}
		return e.apply(ctx, params, req)
	case ActionConfirm:
		params, ok := req.Params.(ConfirmParams)
		if !ok {
			return WriteReport{}, fmt.Errorf("%w: 参数类型不对", ErrDefinitionInvalid)
		}
		return e.confirm(ctx, params, req)
	case ActionRollback:
		params, ok := req.Params.(RollbackParams)
		if !ok {
			return WriteReport{}, fmt.Errorf("%w: 参数类型不对", ErrDefinitionInvalid)
		}
		return e.rollbackNow(ctx, params, req)
	default:
		return WriteReport{}, fmt.Errorf("network: 未知动作 %q", req.Action)
	}
}

func (e *ApplyEngine) progress(req WriteRequest, step string, fields map[string]any) {
	if req.Progress != nil {
		req.Progress(step, fields)
	}
}

// apply 是 network.apply 的执行。
func (e *ApplyEngine) apply(ctx context.Context, params ApplyParams, req WriteRequest) (WriteReport, error) {
	def, err := ValidateDefinition(params.Definition)
	if err != nil {
		return WriteReport{}, err
	}
	params.Definition = def
	iface := strings.TrimSpace(params.InterfaceID)

	// 执行前重跑一遍预检里那套判断：预检到提交之间，现场可能已经变了。
	_, blockers, _, _, ident, stateVersion, err := e.inspectTarget(ctx, iface, params.ExpectedConfigHash)
	if err != nil {
		return WriteReport{}, err
	}
	if len(blockers) > 0 {
		return WriteReport{}, fmt.Errorf("%w: %s", ErrPreflightChanged, blockers[0].Message)
	}
	if req.ExpectedIdentity != "" && ident != req.ExpectedIdentity {
		// 网卡还是那个名字，但永久 MAC 换了：这台机器上的 ens4 已经不是你预检时看的 ens4。
		return WriteReport{}, fmt.Errorf("%w: 目标身份已变（预检 %s，现在 %s）",
			ErrPreflightChanged, req.ExpectedIdentity, ident)
	}
	if req.ExpectedStateVersion != "" && stateVersion != req.ExpectedStateVersion {
		return WriteReport{}, fmt.Errorf("%w: 状态版本已变（预检 %s，现在 %s）",
			ErrPreflightChanged, shortHash(req.ExpectedStateVersion), shortHash(stateVersion))
	}
	if pending, perr := e.pendingRecord(); perr != nil {
		return WriteReport{}, perr
	} else if pending != nil {
		return WriteReport{}, fmt.Errorf("%w: 已有一条未确认的改动（%s）", ErrPreflightChanged, pending.Token)
	}

	view, err := inspectNetplan(e.fs, e.paths.NetplanDir, iface, e.permanentMAC(ctx, iface))
	if err != nil {
		return WriteReport{}, err
	}
	previous, err := e.capturePrevious(ctx, view, iface)
	if err != nil {
		return WriteReport{}, err
	}

	// 1) 先写候选文件（原子替换），再 `netplan generate` 只渲染不生效。
	if err := e.writeNetplanFile(view.PanelName, renderPanelNetplan(iface, *def)); err != nil {
		return WriteReport{}, err
	}
	e.progress(req, "render", map[string]any{"interface": iface})
	gen, genErr := e.runNetplan(ctx, "generate")
	if genErr != nil || gen.exit != 0 {
		// 渲染都过不了：把文件退回去，**不做 apply**，现场一个字节没动。
		restoreErr := e.restorePanelFile(view, previous)
		return WriteReport{
			Outcome: outcomeOfRestore(restoreErr),
			Observed: map[string]any{"netplan_generate": gen.describe(),
				"restore_error": errString(restoreErr)},
			RecoveryHint: "配置没有通过 netplan 的语法检查，已把面板文件恢复原状；现场未改动",
		}, nil
	}

	// 2) 先武装回滚 timer，再 apply。
	rec, err := e.newRecord(ctx, iface, *def, previous, req.OperationID)
	if err != nil {
		_ = e.restorePanelFile(view, previous)
		return WriteReport{}, err
	}
	if err := e.writeRecord(rec); err != nil {
		_ = e.restorePanelFile(view, previous)
		return WriteReport{}, err
	}
	if err := e.armRollback(ctx, rec, RollbackWindow); err != nil {
		// 回滚装不上就不许改：这是整条路径的安全前提。
		restoreErr := e.restorePanelFile(view, previous)
		_ = e.markRecord(rec, StateFailed, "回滚 timer 没能装上："+err.Error())
		return WriteReport{
			Outcome: outcomeOfRestore(restoreErr),
			Observed: map[string]any{"arm_error": err.Error(),
				"restore_error": errString(restoreErr)},
			RecoveryHint: "回滚 timer 没装上，因此没有执行改动；面板文件已恢复",
		}, nil
	}

	e.progress(req, "apply", map[string]any{"interface": iface, "deadline": rec.Deadline})
	e.note(rec, "渲染通过：%s", gen.describe())
	e.note(rec, "timer 已武装：%s 到期 %s", e.timerUnitName(rec.Token), rec.Deadline)
	applied, applyErr := e.runNetplan(ctx, "apply")
	e.note(rec, "%s", applied.describe())
	if applyErr != nil || applied.exit != 0 {
		// apply 失败：立刻自己回滚（timer 到期也会做同一件事，但没必要让它等）。
		rbErr := e.runRollback(ctx, rec)
		return WriteReport{
			Outcome: rollbackOutcome(rec),
			Observed: map[string]any{
				"netplan_apply": applied.describe(), "rollback_error": errString(rbErr),
				"rollback_state": rec.State,
			},
			RecoveryHint: "netplan apply 失败，已按记录回滚",
		}, nil
	}

	// 3) 不信退出码，读回内核事实。
	verifyErr := e.verifyApplied(ctx, iface, *def, previous)
	if verifyErr != nil {
		rbErr := e.runRollback(ctx, rec)
		return WriteReport{
			Outcome: rollbackOutcome(rec),
			Observed: map[string]any{
				"verify_error": verifyErr.Error(), "rollback_error": errString(rbErr),
				"rollback_state": rec.State,
			},
			RecoveryHint: "改动没有生效（读回的地址与要求不一致），已按记录回滚",
		}, nil
	}

	return WriteReport{
		Outcome: OutcomeVerified,
		Observed: map[string]any{
			"interface": iface, "address": def.candidateAddress(),
			"mode": def.Mode, "deadline": rec.Deadline, "rollback_token": rec.Token,
		},
		Data: map[string]any{
			"rollback_token":  rec.Token,
			"deadline":        rec.Deadline,
			"window_seconds":  int(RollbackWindow.Seconds()),
			"previous":        describePrevious(rec),
			"config_hash_now": stateVersionOf(ctx, e, iface),
		},
		RecoveryHint: fmt.Sprintf(
			"请在 %s 之前从新地址确认；不确认的话系统会自动退回", rec.Deadline),
	}, nil
}

// confirm 执行 network.confirm：停 timer、删单元、记录写 confirmed。
func (e *ApplyEngine) confirm(ctx context.Context, params ConfirmParams, req WriteRequest) (WriteReport, error) {
	rec, blockers := e.confirmChecks(ctx, params)
	if len(blockers) > 0 {
		return WriteReport{}, fmt.Errorf("%w: %s", ErrPreflightChanged, blockers[0].Message)
	}
	if err := e.disarmRollback(ctx, rec, StateConfirmed, "已确认保留（来自 "+params.ArrivedOn+"）"); err != nil {
		return WriteReport{}, err
	}
	e.progress(req, "confirmed", map[string]any{"token": rec.Token})
	return WriteReport{
		Outcome:  OutcomeVerified,
		Observed: map[string]any{"interface": rec.Interface, "address": rec.CandidateAddress, "state": StateConfirmed},
		Data:     map[string]any{"rollback_token": rec.Token, "state": StateConfirmed},
	}, nil
}

// rollbackNow 执行 network.rollback：不等 timer，立刻退回去。
func (e *ApplyEngine) rollbackNow(ctx context.Context, params RollbackParams, req WriteRequest) (WriteReport, error) {
	rec, err := e.pendingRecord()
	if err != nil {
		return WriteReport{}, err
	}
	if rec == nil || (params.Token != "" && rec.Token != params.Token) {
		return WriteReport{}, fmt.Errorf("%w: 没有可回滚的记录", ErrPreflightChanged)
	}
	e.progress(req, "rollback", map[string]any{"token": rec.Token})
	if err := e.runRollback(ctx, rec); err != nil {
		return WriteReport{
			Outcome:      rollbackOutcome(rec),
			Observed:     map[string]any{"rollback_state": rec.State, "error": err.Error()},
			RecoveryHint: "回滚没走完，记录已标 failed：按 journal 与记录文件里的原因现场处理，不会重试",
		}, nil
	}
	return WriteReport{
		Outcome:  OutcomeVerified,
		Observed: map[string]any{"interface": rec.Interface, "state": rec.State, "previous": describePrevious(rec)},
		Data:     map[string]any{"rollback_token": rec.Token, "state": rec.State},
	}, nil
}

// ErrPreflightChanged 表示执行前复核发现现场已经不是预检时的那一份。
var ErrPreflightChanged = errors.New("network: 预检之后现场变了")

// outcomeOfRestore：改动还没生效就退回去了，按"恢复本身成不成功"给结论。
func outcomeOfRestore(restoreErr error) string {
	if restoreErr == nil {
		return OutcomeRolledBack
	}
	return OutcomeUnknown
}

// rollbackOutcome：变更是动过的，按回滚记录的终态给结论。
//
// 只看 `rec.State`，不看回滚函数返回的错误：`runRollback` 会把每一种失败都写进记录，
// 而记的终态才是"机器现在在哪"的答案。`failed` 在这里翻译成 `unknown`——记录说失败
// 等于我们不知道网卡现在是什么配置，这必须让人来看，而不是让前端显示一个"已回滚"。
func rollbackOutcome(rec *RollbackRecord) string {
	if rec.State == StateRolledBack {
		return OutcomeRolledBack
	}
	return OutcomeUnknown
}

// TargetIOError 是"读得到但读不动"（权限、IO 错误）的包装：它不该被翻译成
// `NETWORK_TARGET_CHANGED`，那会让管理员去查网卡，而真正的问题在文件系统。
type TargetIOError struct{ Err error }

func (e *TargetIOError) Error() string { return "读 netplan 配置失败：" + e.Err.Error() }
func (e *TargetIOError) Unwrap() error { return e.Err }

// ---- 记录与单元 ----

// RollbackRecord 是磁盘上的回滚记录（`/var/lib/upanel-agent/network/rollback/<token>.json`）。
//
// 它必须在**没有面板**的情况下也能被读懂并执行，所以字段全是自足的：要恢复哪个文件、
// 恢复成什么、退回哪个地址、什么时候到期。
type RollbackRecord struct {
	Token            string         `json:"token"`
	Interface        string         `json:"interface"`
	PermanentMAC     string         `json:"permanent_mac"`
	Mode             string         `json:"mode"`
	CandidateAddress string         `json:"candidate_address,omitempty"`
	CandidateGateway string         `json:"candidate_gateway,omitempty"`
	Previous         previousConfig `json:"previous"`
	AppliedAt        string         `json:"applied_at"`
	Deadline         string         `json:"deadline"`
	State            string         `json:"state"`
	Result           string         `json:"result,omitempty"`
	// Actions 是这个 token 走过的步骤与命令原话（最多保留最近四十条）。
	//
	// 它在故障现场比 journal 好用：journal 会把两次操作、别的单元混在一起，
	// 而这里只有这一次改动自己的脚印。写失败不中断流程——它是复盘材料，不是控制流。
	Actions     []string `json:"actions,omitempty"`
	Executable  string   `json:"executable"`
	ConfigPath  string   `json:"config_path"`
	OperationID string   `json:"operation_id,omitempty"`
	UpdatedAt   string   `json:"updated_at"`
}

// note 记一条脚印。写盘失败只丢一条记录，不阻断正在做的事。
func (e *ApplyEngine) note(rec *RollbackRecord, format string, args ...any) {
	rec.Actions = append(rec.Actions, fmt.Sprintf(format, args...))
	if len(rec.Actions) > 40 {
		rec.Actions = rec.Actions[len(rec.Actions)-40:]
	}
	_ = e.writeRecord(rec)
}

// previousConfig 是"改之前的样子"，回滚按它逐项还原。
type previousConfig struct {
	PanelFilePresent bool   `json:"panel_file_present"`
	PanelFileName    string `json:"panel_file_name"`
	PanelFileSHA256  string `json:"panel_file_sha256,omitempty"`
	// PanelFileContent 是原文件的字节。回滚要"按字节还原"，不是"重新渲染一份差不多的"。
	PanelFileContent string `json:"panel_file_content,omitempty"`
	Mode             string `json:"mode"`
	Address          string `json:"address,omitempty"`
	PrefixLength     int    `json:"prefix_length,omitempty"`
	Gateway          string `json:"gateway,omitempty"`
}

func (r *RollbackRecord) deadline() time.Time {
	t, err := time.Parse(time.RFC3339, r.Deadline)
	if err != nil {
		return time.Time{}
	}
	return t
}

// capturePrevious 记下改之前的现场（面板文件的字节 + 网卡上的地址 + 默认网关）。
func (e *ApplyEngine) capturePrevious(ctx context.Context, view netplanView, iface string) (previousConfig, error) {
	out := previousConfig{PanelFileName: view.PanelName}
	for _, f := range view.Files {
		if f.Name != view.PanelName {
			continue
		}
		data, err := e.fs.ReadFile(path.Join(e.paths.NetplanDir, f.Name))
		if err != nil {
			return previousConfig{}, fmt.Errorf("读 %s 失败：%w", f.Name, err)
		}
		out.PanelFilePresent = true
		out.PanelFileSHA256 = f.Sha256
		out.PanelFileContent = string(data)
	}
	out.Mode = e.observeMode(ctx, iface)
	if addr := e.addressOf(ctx, iface); addr != nil {
		out.Address = addr.Address
		out.PrefixLength = addr.PrefixLen
	}
	out.Gateway = e.defaultGateway(ctx, iface)
	return out, nil
}

// writeRecord 原子写回滚记录（同目录临时文件 + rename），权限 0600。
func (e *ApplyEngine) writeRecord(rec *RollbackRecord) error {
	rec.UpdatedAt = e.now().Format(time.RFC3339)
	raw, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	if err := e.fs.MkdirAll(e.paths.StateDir, 0o700); err != nil {
		return fmt.Errorf("建状态目录失败：%w", err)
	}
	return e.writeFileAtomic(path.Join(e.paths.StateDir, rec.Token+".json"), raw, 0o600)
}

func (e *ApplyEngine) readRecord(token string) (*RollbackRecord, error) {
	data, err := e.fs.ReadFile(path.Join(e.paths.StateDir, token+".json"))
	if err != nil {
		return nil, err
	}
	var rec RollbackRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("记录 %s 解不开：%w", token, err)
	}
	return &rec, nil
}

// pendingRecord 找当前唯一一条 pending 记录。
//
// "唯一"是设计前提：一次只许有一条未确认的改动（预检会用 NETWORK_ROLLBACK_PENDING 拦住
// 第二条）。真出现两条（人为放进去的），按到期时间最早的返回——最急的那条先处理。
func (e *ApplyEngine) pendingRecord() (*RollbackRecord, error) {
	names, err := e.fs.ListDir(e.paths.StateDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	sort.Strings(names)
	var found []*RollbackRecord
	for _, name := range names {
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		rec, rerr := e.readRecord(strings.TrimSuffix(name, ".json"))
		if rerr != nil {
			continue
		}
		if rec.State == StatePending {
			found = append(found, rec)
		}
	}
	if len(found) == 0 {
		return nil, nil
	}
	sort.Slice(found, func(i, j int) bool { return found[i].Deadline < found[j].Deadline })
	return found[0], nil
}

func (e *ApplyEngine) pendingView() (*RollbackView, error) {
	rec, err := e.pendingRecord()
	if err != nil || rec == nil {
		return nil, err
	}
	left := int(rec.deadline().Sub(e.now()).Seconds())
	if left < 0 {
		left = 0
	}
	return &RollbackView{
		Token: rec.Token, Interface: rec.Interface, Mode: rec.Mode,
		CandidateAddress: rec.CandidateAddress, AppliedAt: rec.AppliedAt,
		Deadline: rec.Deadline, SecondsLeft: left, State: rec.State, Result: rec.Result,
	}, nil
}

func (e *ApplyEngine) markRecord(rec *RollbackRecord, state, result string) error {
	rec.State = state
	rec.Result = result
	return e.writeRecord(rec)
}

// ---- netplan 文件 ----

// writeNetplanFile 原子替换面板那份文件：同目录临时文件 + rename。
//
// 不直接覆盖：写到一半掉电会留下一个半个文件，而 netplan 会把它当配置读。
func (e *ApplyEngine) writeNetplanFile(name string, content []byte) error {
	full := path.Join(e.paths.NetplanDir, name)
	if err := e.fs.MkdirAll(e.paths.NetplanDir, 0o755); err != nil {
		return fmt.Errorf("建 %s 失败：%w", e.paths.NetplanDir, err)
	}
	return e.writeFileAtomic(full, content, panelNetplanMode)
}

func (e *ApplyEngine) writeFileAtomic(full string, content []byte, perm os.FileMode) error {
	tmp := full + ".upanel-tmp"
	if err := e.fs.WriteFile(tmp, content, perm); err != nil {
		return err
	}
	if err := e.fs.Rename(tmp, full); err != nil {
		_ = e.fs.Remove(tmp)
		return err
	}
	if err := e.fs.SyncDir(path.Dir(full)); err != nil {
		// 内容已经换上了（运行中的 netplan apply 读得到），只是目录项还没落盘。
		// 这里仍然报错：调用方据此在 apply 之前停手，比"以为写稳了"诚实。
		return fmt.Errorf("刷目录 %s 失败：%w", path.Dir(full), err)
	}
	return nil
}

// restorePanelFile 按记录把面板文件还原成改之前的样子。
func (e *ApplyEngine) restorePanelFile(view netplanView, prev previousConfig) error {
	full := path.Join(e.paths.NetplanDir, view.PanelName)
	if prev.PanelFilePresent {
		return e.writeFileAtomic(full, []byte(prev.PanelFileContent), panelNetplanMode)
	}
	if _, err := e.fs.Stat(full); err != nil {
		return nil
	}
	return e.fs.Remove(full)
}

// ---- 回滚单元 ----

// rollbackTemplateName 是模板单元名；每个 token 的 timer 由模板实例化。
const rollbackTemplateName = "upanel-netrollback@.service"

func (e *ApplyEngine) timerUnitName(token string) string {
	return "upanel-netrollback@" + token + ".timer"
}

func (e *ApplyEngine) timerPath(token string) string {
	return path.Join(e.paths.SystemdDir, e.timerUnitName(token))
}

// armRollback 写模板单元 + 这台 token 的 timer，然后 start 它。
//
// `OnActiveSec=` 从 timer 被 start 的那一刻算起，所以"武装"与"apply"之间的几十毫秒
// 不计入窗口——这正是我们要的：窗口是"改完之后给人反应的时间"。
func (e *ApplyEngine) armRollback(ctx context.Context, rec *RollbackRecord, window time.Duration) error {
	if e.tools.systemctl == "" {
		return errors.New("找不到 systemctl：无法武装回滚 timer")
	}
	exe, err := e.opts.Executable()
	if err != nil {
		return fmt.Errorf("取代理二进制路径失败：%w", err)
	}
	rec.Executable = exe
	rec.ConfigPath = e.opts.ConfigPath
	if err := e.writeRecord(rec); err != nil {
		return err
	}
	// 模板里用 %i 而不是把 token 拼进来：模板是**共享**的，一次只该存在一份。
	// 把具体 token 写死进模板，会让第二个 token 的武装偷偷改掉第一个单元的行为。
	//
	// 路径加引号：ExecStart 不经 shell，但 systemd 自己会按空白切分，配置路径里
	// 带一个空格就会变成两个参数。配置路径为空就整段不写——那条命令不读配置，
	// 少一个空参数比多一个 '""' 诚实。
	configArg := ""
	if e.opts.ConfigPath != "" {
		configArg = ` --config "` + e.opts.ConfigPath + `"`
	}
	template := fmt.Sprintf(`# 由 uPanel 生成（P14.2）：到期未确认则把网络配置退回改之前的样子。
# 它跑的是代理自己的二进制，不依赖 upanel-web 或面板数据库是否还在。
[Unit]
Description=uPanel network rollback (%%i)

[Service]
Type=oneshot
RemainAfterExit=no
ExecStart="%s" netrollback --token %%i%s
TimeoutStartSec=180
`, exe, configArg)
	if err := e.writeFileAtomic(path.Join(e.paths.SystemdDir, rollbackTemplateName),
		[]byte(template), 0o644); err != nil {
		return err
	}
	if err := e.writeTimer(rec.Token, window); err != nil {
		return err
	}
	if err := e.systemctl(ctx, "daemon-reload"); err != nil {
		return err
	}
	if err := e.systemctl(ctx, "start", e.timerUnitName(rec.Token)); err != nil {
		return err
	}
	return nil
}

// writeTimer 写这台 token 的 timer。
//
// 不给 `[Install]`：我们**故意**不 enable 它。开机后由代理的 Reconcile 按剩余时间重装
// ——timer 的 `OnActiveSec` 是单调时钟，重启后就没了；如果 enable 了，开机时那一份
// 会带着旧的计时语义醒过来，而"还剩 40 秒"这件事只有代理知道。
func (e *ApplyEngine) writeTimer(token string, window time.Duration) error {
	seconds := int(window.Seconds())
	if seconds <= 0 {
		seconds = 1
	}
	content := fmt.Sprintf(`# 由 uPanel 生成（P14.2）：%d 秒后触发回滚。
[Unit]
Description=uPanel network rollback timer (%s)

[Timer]
OnActiveSec=%ds
AccuracySec=1s
Unit=upanel-netrollback@%s.service
`, seconds, token, seconds, token)
	return e.writeFileAtomic(e.timerPath(token), []byte(content), 0o644)
}

// disarmRollback 停 timer、删单元、记录写终态。
func (e *ApplyEngine) disarmRollback(ctx context.Context, rec *RollbackRecord, state, result string) error {
	if e.tools.systemctl != "" {
		// stop 失败不阻断：timer 可能已经触发（那它自己会写终态），也可能单元已经不在了。
		_ = e.systemctl(ctx, "stop", e.timerUnitName(rec.Token))
	}
	_ = e.fs.Remove(e.timerPath(rec.Token))
	if err := e.markRecord(rec, state, result); err != nil {
		return err
	}
	if e.tools.systemctl != "" {
		_ = e.systemctl(ctx, "daemon-reload")
	}
	return nil
}

// runRollback 是回滚的唯一实现：timer 触发的子命令、显式 rollback 动作、
// 以及"apply 核验失败时立刻退回"都走这里。
func (e *ApplyEngine) runRollback(ctx context.Context, rec *RollbackRecord) error {
	if e.tools.netplan == "" {
		return e.markRecord(rec, StateFailed, "找不到 netplan：无法回滚")
	}
	view := netplanView{PanelName: rec.Previous.PanelFileName}
	if view.PanelName == "" {
		view.PanelName = PanelNetplanPrefix + rec.Interface + ".yaml"
	}
	if err := e.restorePanelFile(view, rec.Previous); err != nil {
		return e.markRecord(rec, StateFailed, "恢复面板文件失败："+err.Error())
	}
	gen, err := e.runNetplan(ctx, "generate")
	e.note(rec, "回滚：%s", gen.describe())
	if err != nil || gen.exit != 0 {
		return e.markRecord(rec, StateFailed, "回滚时 netplan generate 失败："+gen.describe()+errString(err))
	}
	applied, err := e.runNetplan(ctx, "apply")
	e.note(rec, "回滚：%s", applied.describe())
	if err != nil || applied.exit != 0 {
		return e.markRecord(rec, StateFailed, "回滚时 netplan apply 失败："+applied.describe()+errString(err))
	}
	// 读回旧地址：回滚"成功"的唯一判据是网卡上真的回到了旧配置（或者旧配置本来就是
	// DHCP，那就看有没有地址）。
	if err := e.verifyRestored(ctx, rec); err != nil {
		return e.markRecord(rec, StateFailed, "回滚后核验不一致："+err.Error())
	}
	if err := e.disarmRollback(ctx, rec, StateRolledBack, "已回滚到改之前的配置"); err != nil {
		return err
	}
	return nil
}

// Reconcile 在代理启动时处理遗留的 pending 记录。
//
// 这条路径只在"回滚期间机器重启或代理被停掉"时才走到，但它是整条救生索的最后一段：
// 没有它，重启之后那条 pending 记录会永远躺着，而配置已经坏了。
func (e *ApplyEngine) Reconcile(ctx context.Context) error {
	rec, err := e.pendingRecord()
	if err != nil {
		return err
	}
	if rec == nil {
		return nil
	}
	now := e.now()
	if !now.Before(rec.deadline()) {
		return e.runRollback(ctx, rec)
	}
	return e.armRollback(ctx, rec, rec.deadline().Sub(now))
}

// ExecutePendingRollback 是 `upanel-agent netrollback --token …` 的执行体：
// 由 systemd 直接调用，此时面板可能根本没在运行。
func (e *ApplyEngine) ExecutePendingRollback(ctx context.Context, token string) error {
	rec, err := e.readRecord(token)
	if err != nil {
		return err
	}
	if rec.State != StatePending {
		// 已经被确认或已经回滚过：timer 迟到了，什么都不做。
		return nil
	}
	return e.runRollback(ctx, rec)
}

// ---- 核验 ----

// verifyApplied 读回内核事实，确认这次改动真的生效了。
//
// 两件事都要成立才算数：
//
//  1. **新配置在**：地址（静态时还要求前缀与网关）确实出现在这张网卡上；
//  2. **旧地址不在**。这一条常被略过，但它是判"我们那份文件真的接管了"的唯一办法：
//     networkd 会把所有 netplan 文件里声明的地址都配上，如果别的文件里也写了旧地址，
//     我们的文件只是**多加**了一个地址，而页面会说"改好了"。
func (e *ApplyEngine) verifyApplied(ctx context.Context, iface string, def ApplyDefinition, prev previousConfig) error {
	if def.Mode == ModeStatic && def.IPv4 != nil {
		if err := e.waitForAddress(ctx, iface, def.IPv4.Address, def.IPv4.PrefixLength); err != nil {
			return err
		}
		if def.IPv4.Gateway != "" {
			if gw := e.defaultGateway(ctx, iface); gw != def.IPv4.Gateway {
				return fmt.Errorf("默认网关应为 %s，读回 %q", def.IPv4.Gateway, gw)
			}
		}
		if prev.Address != "" && prev.Address != def.IPv4.Address {
			if e.hasGlobalIPv4(ctx, iface, prev.Address) {
				return fmt.Errorf("旧地址 %s 还在网卡上：说明不是我们这份文件在管它", prev.Address)
			}
		}
		return nil
	}
	// DHCP：地址由服务器给，不能要求具体值。
	//
	// 这里的判据**不能**是"旧地址必须消失"，原因是会踩到：DHCP 服务器完全可以把同一个
	// 地址再发给你。10-26 在 22.04 上实测到过——ens4 上写死 10.0.2.15/24，再切回 DHCP，
	// slirp 恒发 10.0.2.15，于是"旧地址还在"永远成立，任务 outcome=rolled_back，回滚
	// 记录里写着"切到 DHCP 之后旧地址 10.0.2.15 还在网卡上"。可那一刻地址还在，恰恰
	// 就是成功的样子。同一段代码还有个更早的问题：原来那句 `waitForAddress(…, exact=false)`
	// 只要求"网卡上有全局 IPv4"，而旧静态地址一直都在，所以那句话是**空转**的，一秒都没等。
	//
	// 改判"这张网卡上恰好剩一个全局地址"：
	//   · 我们这份文件只声明 `dhcp4: true`，正常情况下只会有一个租来的地址；
	//   · 交接那一瞬旧地址还在、新的没来（或者两个都在），所以等它收敛；
	//   · 如果**别的** netplan 文件也在给这张网卡配地址，那个地址不会被摘掉，就会一直
	//     停在两个上——这才是原来那条"旧地址还在"真正想抓的东西。
	//
	// 已知抓不到的：别的文件把旧地址原地声明成同名静态地址时，地址层面与"DHCP 又发了
	// 同一个地址"无法区分。这一条写进 DECISIONS，不假装它被覆盖了。
	if err := e.waitForSingleAddress(ctx, iface, e.dhcpWait); err != nil {
		return err
	}
	return nil
}

// verifyRestored 确认回滚之后旧配置回来了。
func (e *ApplyEngine) verifyRestored(ctx context.Context, rec *RollbackRecord) error {
	if rec.Previous.Mode == ModeStatic && rec.Previous.Address != "" {
		if err := e.waitForAddress(ctx, rec.Interface, rec.Previous.Address,
			rec.Previous.PrefixLength); err != nil {
			return err
		}
		// 回滚之后候选地址必须不在了，否则"退回去"只是叠了一层。
		if rec.CandidateAddress != "" && rec.CandidateAddress != rec.Previous.Address {
			if e.hasGlobalIPv4(ctx, rec.Interface, rec.CandidateAddress) {
				return fmt.Errorf("候选地址 %s 还在网卡上", rec.CandidateAddress)
			}
		}
		return nil
	}
	// 退回 DHCP：判据与切过去时一致（恰好一个全局地址），理由见 verifyApplied。
	// 这里不能再用"地址出现就算数"：返回 DHCP 之前网卡上往往还挂着那个静态候选地址，
	// 那句等待会在地址交接之前就成立，等于什么都没验。
	return e.waitForSingleAddress(ctx, rec.Interface, e.dhcpWait)
}

// waitForSingleAddress 等到这张网卡上恰好剩一个全局 IPv4 地址。
//
// 为什么是"恰好一个"而不是"至少一个"，见 verifyApplied 里 DHCP 那一段注释。
func (e *ApplyEngine) waitForSingleAddress(ctx context.Context, iface string, window time.Duration) error {
	deadline := e.now().Add(window)
	for {
		addrs := e.globalIPv4s(ctx, iface)
		if len(addrs) == 1 {
			return nil
		}
		if !e.now().Before(deadline) {
			if len(addrs) == 0 {
				return fmt.Errorf("%s 上没有拿到全局 IPv4 地址（读回 无）：DHCP 没有发下租约", iface)
			}
			return fmt.Errorf("%s 上有 %d 个全局 IPv4 地址（读回 %s）：多个说明别的配置文件也在给这张网卡配地址",
				iface, len(addrs), e.describeIPv4s(addrs))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// waitForAddress 轮询到某个静态地址（含前缀）出现在网卡上，或超时（15 秒）。
//
// 为什么要轮询：`netplan apply` 返回之后 networkd 还在异步配置接口。立刻读一次就判
// 失败会把"正常的几百毫秒"当成"改动没生效"，进而触发一次完全没必要的回滚。
//
// 只服务静态地址。DHCP 走 waitForSingleAddress：那边判的是"这张网卡上还剩几个地址"，
// 不是"某个地址在不在"——两者不能合成一个函数，合成的那天就会有人把 DHCP 也传进来，
// 然后"旧静态地址还在"被当成"新地址到了"。
func (e *ApplyEngine) waitForAddress(ctx context.Context, iface, want string, prefix int) error {
	deadline := e.now().Add(15 * time.Second)
	for {
		if e.hasGlobalIPv4Prefix(ctx, iface, want, prefix) {
			return nil
		}
		if !e.now().Before(deadline) {
			return fmt.Errorf("%s 上没有出现 %s/%d（读回 %s）",
				iface, want, prefix, e.describeIPv4s(e.globalIPv4s(ctx, iface)))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (e *ApplyEngine) describeIPv4s(addrs []Address) string {
	if len(addrs) == 0 {
		return "无"
	}
	parts := make([]string, 0, len(addrs))
	for _, a := range addrs {
		parts = append(parts, fmt.Sprintf("%s/%d", a.Address, a.PrefixLen))
	}
	return strings.Join(parts, "、")
}

// globalIPv4s 返回这张网卡上全部全局 IPv4 地址。
//
// 不用"第一个"：`ip -j` 的地址顺序不保证是配置顺序，而改完一张网卡之后它上面
// 一时可能有新旧两个地址。判断"在不在"必须看全体，判断"退了没有"也是。
func (e *ApplyEngine) globalIPv4s(ctx context.Context, iface string) []Address {
	list := runInterfaces(ctx, e.opts.Read)
	target := findInterface(list.Interfaces, iface)
	if target == nil {
		return nil
	}
	out := make([]Address, 0, 2)
	for _, a := range target.Addresses {
		if a.Family == FamilyIPv4 && a.Scope == "global" {
			out = append(out, a)
		}
	}
	return out
}

// hasGlobalIPv4 判断某个地址（不管前缀）在不在这张网卡上。
func (e *ApplyEngine) hasGlobalIPv4(ctx context.Context, iface, addr string) bool {
	for _, a := range e.globalIPv4s(ctx, iface) {
		if a.Address == addr {
			return true
		}
	}
	return false
}

func (e *ApplyEngine) hasGlobalIPv4Prefix(ctx context.Context, iface, addr string, prefix int) bool {
	for _, a := range e.globalIPv4s(ctx, iface) {
		if a.Address == addr && a.PrefixLen == prefix {
			return true
		}
	}
	return false
}

// addressOf 是"给错误信息用的那个地址"：有多个时取第一个，只为了让人看懂现状。
// 判定一律走 hasGlobalIPv4 / hasGlobalIPv4Prefix。
func (e *ApplyEngine) addressOf(ctx context.Context, iface string) *Address {
	addrs := e.globalIPv4s(ctx, iface)
	if len(addrs) == 0 {
		return nil
	}
	return &addrs[0]
}

func (e *ApplyEngine) observedIPv4(ctx context.Context, iface string) (string, error) {
	addr := e.addressOf(ctx, iface)
	if addr == nil {
		return "", nil
	}
	return addr.Address, nil
}

func (e *ApplyEngine) defaultGateway(ctx context.Context, iface string) string {
	routes := Routes(ctx, e.opts.Read)
	for _, r := range routes.Routes {
		if r.Destination == "0.0.0.0/0" && r.Device == iface {
			return r.Gateway
		}
	}
	return ""
}

func (e *ApplyEngine) permanentMAC(ctx context.Context, iface string) string {
	list := runInterfaces(ctx, e.opts.Read)
	if target := findInterface(list.Interfaces, iface); target != nil {
		return strings.ToLower(target.MAC)
	}
	return ""
}

// ---- 小工具 ----

type netplanResult struct {
	command  string
	exit     int
	stdout   string
	stderr   string
	timedOut bool
	err      error
}

func (r netplanResult) describe() string {
	out := fmt.Sprintf("%s exit=%d", r.command, r.exit)
	if r.timedOut {
		out += " 超时"
	}
	if s := strings.TrimSpace(r.stdout + r.stderr); s != "" {
		out += " " + truncate(s, 400)
	}
	if r.err != nil {
		out += " err=" + r.err.Error()
	}
	return out
}

func (e *ApplyEngine) runNetplan(ctx context.Context, args ...string) (netplanResult, error) {
	if e.tools.netplan == "" {
		return netplanResult{command: "netplan"}, errors.New("找不到 netplan")
	}
	full := append([]string{"netplan"}, args...)
	res, err := e.opts.Runner.Run(ctx, syscmd.Spec{Program: e.tools.netplan, Args: args},
		syscmd.Limits{Timeout: 60 * time.Second, MaxStdout: 64 << 10, MaxStderr: 64 << 10})
	out := netplanResult{command: strings.Join(full, " "), exit: res.ExitCode,
		stdout: res.Stdout, stderr: res.Stderr, timedOut: res.TimedOut}
	if err != nil {
		var exitErr *syscmd.ExitError
		if errors.As(err, &exitErr) {
			out.exit = exitErr.ExitCode
			out.err = nil
			return out, nil
		}
		out.err = err
		return out, err
	}
	return out, nil
}

func (e *ApplyEngine) systemctl(ctx context.Context, args ...string) error {
	if e.tools.systemctl == "" {
		return errors.New("找不到 systemctl")
	}
	_, err := e.opts.Runner.Run(ctx, syscmd.Spec{Program: e.tools.systemctl, Args: args},
		syscmd.Limits{Timeout: 30 * time.Second, MaxStdout: 32 << 10, MaxStderr: 32 << 10})
	if err != nil {
		var exitErr *syscmd.ExitError
		if errors.As(err, &exitErr) {
			return fmt.Errorf("systemctl %s 退出码 %d", strings.Join(args, " "), exitErr.ExitCode)
		}
		return fmt.Errorf("systemctl %s 失败：%w", strings.Join(args, " "), err)
	}
	return nil
}

func (e *ApplyEngine) newRecord(ctx context.Context, iface string, def ApplyDefinition, prev previousConfig, opID string) (*RollbackRecord, error) {
	token, err := newRollbackToken()
	if err != nil {
		return nil, err
	}
	now := e.now()
	return &RollbackRecord{
		Token: token, Interface: iface, PermanentMAC: e.permanentMAC(ctx, iface),
		Mode: def.Mode, CandidateAddress: def.candidateAddress(),
		CandidateGateway: gatewayOf(def),
		Previous:         prev,
		AppliedAt:        now.Format(time.RFC3339),
		Deadline:         now.Add(RollbackWindow).Format(time.RFC3339),
		State:            StatePending,
		ConfigPath:       e.opts.ConfigPath,
		OperationID:      opID,
	}, nil
}

func gatewayOf(def ApplyDefinition) string {
	if def.IPv4 == nil {
		return ""
	}
	return def.IPv4.Gateway
}

func newRollbackToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return rollbackTokenPrefix + hex.EncodeToString(buf), nil
}

// ValidRollbackToken 校验标识形状：它会被拼进单元名与文件名，所以形状必须是死的。
func ValidRollbackToken(token string) bool {
	if !strings.HasPrefix(token, rollbackTokenPrefix) {
		return false
	}
	rest := strings.TrimPrefix(token, rollbackTokenPrefix)
	if len(rest) != 32 {
		return false
	}
	for _, r := range rest {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func describeRecord(rec *RollbackRecord) string {
	if rec.CandidateAddress != "" {
		return "地址改为 " + rec.CandidateAddress
	}
	return "改为 DHCP"
}

func describePrevious(rec *RollbackRecord) string {
	if rec.Previous.Mode == ModeStatic && rec.Previous.Address != "" {
		s := fmt.Sprintf("静态 %s/%d", rec.Previous.Address, rec.Previous.PrefixLength)
		if rec.Previous.Gateway != "" {
			s += "，网关 " + rec.Previous.Gateway
		}
		return s
	}
	if rec.Previous.Mode == ModeDHCP {
		return "DHCP"
	}
	return "未确认"
}

func stateVersionOf(ctx context.Context, e *ApplyEngine, iface string) string {
	view, err := inspectNetplan(e.fs, e.paths.NetplanDir, iface, e.permanentMAC(ctx, iface))
	if err != nil {
		return ""
	}
	hash, _, err := view.configHash()
	if err != nil {
		return ""
	}
	return hash
}

// runInterfaces 是 Interfaces 的薄封装：只为让上面几处读现场时少写一行 ctx。
func runInterfaces(ctx context.Context, deps Deps) InterfaceList { return Interfaces(ctx, deps) }

func shortHash(h string) string {
	if len(h) <= 15 {
		return h
	}
	return h[:15] + "…"
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// ---- 生产文件系统 ----

type osWriteFS struct{}

func (osWriteFS) ReadFile(name string) ([]byte, error) { return os.ReadFile(name) }
func (osWriteFS) ListDir(dir string) ([]string, error) { return listDirNames(dir) }
func (osWriteFS) WriteFile(n string, d []byte, p os.FileMode) error {
	f, err := os.OpenFile(n, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, p)
	if err != nil {
		return err
	}
	if _, err := f.Write(d); err != nil {
		f.Close()
		return err
	}
	// rename 之前必须先把内容刷下去。ext4 的 data=ordered 下，rename 进了日志而内容
	// 还在页缓存时掉电，重启后看到的是一个**长度对、内容为零**的文件——对 netplan
	// 来说就是一份解析不了的配置，下一次开机会没有网络。
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
func (osWriteFS) Rename(o, n string) error { return os.Rename(o, n) }
func (osWriteFS) SyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		d.Close()
		return err
	}
	return d.Close()
}
func (osWriteFS) Remove(n string) error                  { return os.Remove(n) }
func (osWriteFS) MkdirAll(d string, p os.FileMode) error { return os.MkdirAll(d, p) }
func (osWriteFS) Readlink(n string) (string, error)      { return os.Readlink(n) }
func (osWriteFS) Stat(n string) (os.FileInfo, error)     { return os.Stat(n) }

// listDirNames 与只读那一侧的实现同形：只返回名字。
func listDirNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names, nil
}
