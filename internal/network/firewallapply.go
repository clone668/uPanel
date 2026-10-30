package network

// P14.3：真正动 UFW 的那一层。形状与 P14.2 的 applyengine.go 刻意保持一致——同一个
// 安全模型，换一个被改的对象：
//
//  1. **先武装、再改**。回滚 timer 在改规则之前就挂好；装不上就不改。
//  2. **回滚不依赖面板**。timer 是 systemd 的，执行的是代理二进制的 `fwrollback` 子命令，
//     读的是磁盘上的记录。面板停掉、浏览器关掉，都不影响它到期把规则集退回去。
//  3. **不信退出码，只信读回来的事实**。`ufw` 在"删一条不存在的规则"时打印
//     `Could not delete non-existent rule` 而**退出码是 0**（0.36.1 实测），所以每一次
//     改动之后都要重新读 `ufw show added` 与 `ufw status`，对不上就立刻自己回滚。
//  4. **失败不重试**。回滚任何一步失败，记录写 `failed` 与原因就停手。
//
// 与 P14.2 的一处不同：这里**不另做 netplan 那样的"候选文件"**。UFW 的规则集由它自己
// 序列化进 /etc/ufw/user.rules，面板不去直接编辑那份文件——绕过 ufw 命令去改它的数据文件，
// 等于跟它的内部格式签一份随时会变的合同。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"upanel/internal/syscmd"
)

// FirewallPaths 是这一层会写到的两个目录。
type FirewallPaths struct {
	SystemdDir string
	StateDir   string
}

func (p FirewallPaths) withDefaults() FirewallPaths {
	if p.SystemdDir == "" {
		p.SystemdDir = "/etc/systemd/system"
	}
	if p.StateDir == "" {
		p.StateDir = "/var/lib/upanel-agent/firewall/rollback"
	}
	return p
}

// FirewallOptions 是防火墙写路径的装配参数。
type FirewallOptions struct {
	// Read 是 P14.1 的只读采集依赖（观察现状与管理通路，必需）。
	Read Deps
	// Runner 是命令执行器（必需）。
	Runner CommandRunner
	// FS 是文件系统接缝；零值用真实 os 调用。
	FS WriteFileSystem
	// Executable 返回代理自己的二进制路径（回滚单元用它，必需）。
	Executable func() (string, error)
	// ConfigPath 写进回滚单元的命令行（只为现场可读，可为空）。
	ConfigPath string
	// Paths 是两个目录；零值取生产路径。
	Paths FirewallPaths
	// Now 提供时间（可注入）。
	Now func() time.Time
	// Stat 用于"文件在不在"；零值用 os.Stat。
	Stat func(name string) (os.FileInfo, error)
	// VerifyWindow 是"改完等它生效"的时间窗；零值用 firewallVerifyWindow。
	VerifyWindow time.Duration
}

// firewallTools 是这一层允许找到的程序。与 tools 分开：防火墙不碰 netplan，
// 网络也不碰 ufw——两条写路径各自只有两个程序，谁都不多一个。
type firewallTools struct {
	ufw       string
	systemctl string
}

// FirewallEngine 是防火墙写路径的入口。
type FirewallEngine struct {
	opts  FirewallOptions
	paths FirewallPaths
	fs    WriteFileSystem
	tools firewallTools
}

var ufwCandidates = []string{"/usr/sbin/ufw", "/sbin/ufw", "/usr/bin/ufw"}

// NewFirewallEngine 构造写路径。缺必需项直接报错。
func NewFirewallEngine(opts FirewallOptions) (*FirewallEngine, error) {
	if opts.Runner == nil {
		return nil, errors.New("network: 防火墙写路径缺少命令执行器")
	}
	if opts.Executable == nil {
		return nil, errors.New("network: 防火墙写路径缺少 代理二进制路径")
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
	if opts.VerifyWindow <= 0 {
		opts.VerifyWindow = firewallVerifyWindow
	}
	if opts.Read.FS == nil {
		opts.Read.FS = opts.FS
	}
	if opts.Read.Now == nil {
		opts.Read.Now = opts.Now
	}
	e := &FirewallEngine{opts: opts, paths: opts.Paths.withDefaults(), fs: opts.FS}
	if p := firstExisting(opts.FS, ufwCandidates); p != "" {
		e.tools.ufw = p
	}
	if p := firstExisting(opts.FS, systemctlCandidates); p != "" {
		e.tools.systemctl = p
	}
	return e, nil
}

func (e *FirewallEngine) now() time.Time { return e.opts.Now().UTC() }

// ---- 现场 ----

// FirewallPending 是"有一条还没确认的改动"这句话。
type FirewallPending struct {
	Token       string `json:"token"`
	Deadline    string `json:"deadline"`
	SecondsLeft int    `json:"seconds_left"`
	Previous    string `json:"previous"`
}

// FirewallConfigState 是 `GET /firewall/config` 的内容。
type FirewallConfigState struct {
	CollectedAt string           `json:"collected_at"`
	Manager     string           `json:"manager"`
	Reasons     []string         `json:"reasons"`
	Installed   bool             `json:"installed"`
	Enabled     bool             `json:"enabled"`
	Editable    bool             `json:"editable"`
	Rules       []ManagedRule    `json:"rules"`
	ConfigHash  string           `json:"config_hash,omitempty"`
	Pending     *FirewallPending `json:"pending"`
	Issues      []Issue          `json:"issues"`
}

// snapshot 是一次现场读取的结果：ufw 说了什么，以及我们在里面认出了什么。
type snapshot struct {
	state   FirewallState
	managed []ManagedRule
	// unknown 是带面板标记、但形状认不出来的规则（原话）。有它就不许改：
	// "我有一条自己的规则却读不懂"意味着这次改动的判断基础是错的。
	unknown []string
	// allAdded 是 `ufw show added` 里能解析的全部放行/拒绝规则（含管理员手写的）。
	// 管理通路的判断只能用这一份，不能用 `ufw status`：防火墙未启用时 status 连规则
	// 都不列，拿它当依据会把"规则都在、只是没启用"误判成"一条放行都没有"。
	allAdded []addedRule
	issues   []Issue
}

// Current 读一次现场，给 `GET /firewall/config` 用。
func (e *FirewallEngine) Current(ctx context.Context) FirewallConfigState {
	out := FirewallConfigState{
		CollectedAt: e.now().Format(time.RFC3339),
		Reasons:     []string{},
		Rules:       []ManagedRule{},
		Issues:      []Issue{},
	}
	snap := e.readSnapshot(ctx)
	out.Manager = snap.state.Manager
	out.Reasons = append(out.Reasons, snap.state.Reasons...)
	out.Installed = snap.state.Installed
	out.Enabled = snap.state.Enabled
	out.Editable = snap.state.Editable
	// append 而不是赋值：readSnapshot 在 ufw 读不到（未安装、或 `ufw status` 出错）时会提前
	// return，此时 snap.managed 是 nil。直接赋 nil 会让 `rules` 序列化成 `null`，而契约与
	// OpenAPI 都写着它是数组——前端 `.map` 拿到 null 就是一次崩溃。append 到上面那个空 slice
	// 上，空现场恒为 `[]`。
	out.Rules = append(out.Rules, snap.managed...)
	out.Issues = append(out.Issues, snap.state.Issues...)
	out.Issues = append(out.Issues, snap.issues...)
	// 指纹只在能改的时候给出：不能改的机器上给一个"看起来能提交"的值，页面就会
	// 画出提交按钮，而提交必然得到 FIREWALL_UNSUPPORTED。与 network.config 同一口径。
	if out.Editable {
		out.ConfigHash = firewallConfigHash(snap.state, snap.managed)
	}
	if rec, err := e.pendingRecord(); err == nil && rec != nil {
		left := int(rec.deadline().Sub(e.now()).Seconds())
		if left < 0 {
			left = 0
		}
		out.Pending = &FirewallPending{Token: rec.Token, Deadline: rec.deadline().Format(time.RFC3339),
			SecondsLeft: left, Previous: rec.describePrevious()}
	}
	return out
}

// readSnapshot 读 ufw 的两份输出并解析。
//
// 两份而不是一份：`ufw status` 只回答"现在生不生效"，未启用时它连规则都不列；
// `ufw show added` 回答"规则集里到底有什么"，与启用状态无关。缺任何一份，
// 都无法同时回答"改完是什么样"和"现在是什么样"。
func (e *FirewallEngine) readSnapshot(ctx context.Context) snapshot {
	var snap snapshot
	snap.state = Firewall(ctx, e.opts.Read)
	if !snap.state.Installed || e.tools.ufw == "" {
		return snap
	}
	status, code, err := e.runUFW(ctx, "status", "verbose", "numbered")
	if err != nil || code != 0 {
		snap.issues = append(snap.issues, Issue{Source: "ufw", Code: IssueFailed,
			Detail: "跑 ufw status 失败：" + errString(err) + describeExit(code)})
		return snap
	}
	parsed := FirewallState{}
	parseUFWOutput(status, &parsed)
	snap.state.Enabled = parsed.Enabled
	snap.state.Rules = parsed.Rules
	if parsed.DefaultIncoming != "" {
		snap.state.DefaultIncoming = parsed.DefaultIncoming
		snap.state.DefaultOutgoing = parsed.DefaultOutgoing
		snap.state.DefaultRouted = parsed.DefaultRouted
	}

	added, code, err := e.runUFW(ctx, "show", "added")
	if err != nil || code != 0 {
		snap.issues = append(snap.issues, Issue{Source: "ufw", Code: IssueFailed,
			Detail: "跑 ufw show added 失败：" + errString(err) + describeExit(code)})
		return snap
	}
	snap.managed, snap.unknown = parseShowAdded(added)
	snap.allAdded = addedRulePorts(added)
	return snap
}

func describeExit(code int) string {
	if code == 0 {
		return ""
	}
	return fmt.Sprintf("（退出码 %d）", code)
}

// showAddedLine 是 `ufw show added` 里的一行：一条规则的命令行原话。
type showAddedLine struct {
	Spec    string
	Comment string
	Raw     string
}

// parseShowAdded 把 `ufw show added` 的正文拆成规则行，并挑出面板自己那部分。
//
// 输出形状（0.36.1 实测）：
//
//	Added user rules (see 'ufw status' for running firewall):
//	ufw allow 22/tcp comment 'upanel-test'
//
// 注释由 UFW 原样打印，带单引号。没有注释的行是管理员手写的规则——**原样保留，不动**。
func parseShowAdded(text string) ([]ManagedRule, []string) {
	managed := []ManagedRule{}
	unknown := []string{}
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(strings.TrimRight(raw, "\r"))
		if line == "" || !strings.HasPrefix(line, "ufw ") {
			continue
		}
		parsed, ok := parseShowAddedLine(line)
		if !ok {
			continue
		}
		if !strings.HasPrefix(parsed.Comment, firewallCommentPrefix) {
			continue
		}
		id := strings.TrimPrefix(parsed.Comment, firewallCommentPrefix)
		rule, ok := parseManagedSpec(parsed.Spec, id)
		if !ok || !ValidRuleID(id) {
			unknown = append(unknown, parsed.Raw)
			continue
		}
		managed = append(managed, rule)
	}
	sort.Slice(managed, func(i, j int) bool { return managed[i].ID < managed[j].ID })
	return managed, unknown
}

// parseShowAddedLine 拆出 spec 与 comment。
func parseShowAddedLine(line string) (showAddedLine, bool) {
	body := strings.TrimPrefix(line, "ufw ")
	spec := body
	comment := ""
	if idx := strings.Index(body, " comment "); idx >= 0 {
		spec = body[:idx]
		comment = strings.TrimSpace(body[idx+len(" comment "):])
		comment = strings.Trim(comment, "'\"")
	}
	spec = strings.Join(strings.Fields(spec), " ")
	if spec == "" {
		return showAddedLine{}, false
	}
	return showAddedLine{Spec: spec, Comment: comment, Raw: line}, true
}

// parseManagedSpec 把一条规范写法解析回 ManagedRule。
//
// 认得两种形状（与 ManagedRule.spec 一一对应）：
//
//	allow 22/tcp
//	allow from 10.0.0.0/8 to any port 22 proto tcp
//
// 其余一律 ok=false：认不出来的规则不猜。把它当"没有"会让下一次 apply 再加一条同样的，
// 当成"有"会让回滚删掉一条形状不明的规则。两种都会在没有人的时候改掉别人的防火墙。
func parseManagedSpec(spec, id string) (ManagedRule, bool) {
	tokens := strings.Fields(spec)
	if len(tokens) < 2 {
		return ManagedRule{}, false
	}
	rule := ManagedRule{ID: id, Action: strings.ToLower(tokens[0])}
	switch rule.Action {
	case RuleAllow, RuleDeny, RuleReject, RuleLimit:
	default:
		return ManagedRule{}, false
	}
	if tokens[1] == "from" {
		if len(tokens) != 9 || tokens[3] != "to" || tokens[4] != "any" || tokens[5] != "port" ||
			tokens[7] != "proto" {
			return ManagedRule{}, false
		}
		prefix, err := parseFirewallSource(tokens[2])
		if err != nil {
			return ManagedRule{}, false
		}
		port, err := strconv.Atoi(tokens[6])
		if err != nil || port < 1 || port > 65535 {
			return ManagedRule{}, false
		}
		proto := strings.ToLower(tokens[8])
		if proto != ProtoTCP && proto != ProtoUDP {
			return ManagedRule{}, false
		}
		rule.From, rule.Port, rule.Proto = prefix, port, proto
		return rule, true
	}
	// `22/tcp`
	portPart := tokens[1]
	if len(tokens) != 2 {
		return ManagedRule{}, false
	}
	num, proto, ok := strings.Cut(portPart, "/")
	if !ok {
		return ManagedRule{}, false
	}
	port, err := strconv.Atoi(num)
	if err != nil || port < 1 || port > 65535 {
		return ManagedRule{}, false
	}
	proto = strings.ToLower(proto)
	if proto != ProtoTCP && proto != ProtoUDP {
		return ManagedRule{}, false
	}
	rule.Port, rule.Proto = port, proto
	return rule, true
}

// addedRule 是 `ufw show added` 里一条可解析的规则，连同它的注释。
type addedRule struct {
	Rule    ManagedRule
	Comment string
}

// addedRulePorts 从**全部**规则行里挑出能解析的条目（含管理员手写的）。
//
// 管理通路的判断必须看全部规则，不能只看面板自己的：一条手写的 `ufw allow 22/tcp`
// 同样是保住 SSH 的那条规则，把它排除掉会让预检在一台本来很安全的机器上拦住一切。
func addedRulePorts(text string) []addedRule {
	out := []addedRule{}
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(strings.TrimRight(raw, "\r"))
		if !strings.HasPrefix(line, "ufw ") {
			continue
		}
		parsed, ok := parseShowAddedLine(line)
		if !ok {
			continue
		}
		rule, ok := parseManagedSpec(parsed.Spec, "")
		if !ok {
			continue
		}
		out = append(out, addedRule{Rule: rule, Comment: parsed.Comment})
	}
	return out
}

// ---- 判决 ----

// Assess 是 firewall.apply 的预检。
func (e *FirewallEngine) Assess(ctx context.Context, params FirewallApplyParams) (Assessment, error) {
	// 同 AssessConfirm / AssessRollback：拒绝分支也必须带 Non-nil 的 Normalized。
	// 定义不合法是最常撞到的一条拒绝，它绝不能变成 500。
	out := Assessment{Locks: []string{FirewallLockName},
		Normalized: FirewallApplyParams{ExpectedConfigHash: params.ExpectedConfigHash,
			Definition: params.Definition}}
	snap := e.readSnapshot(ctx)
	def, err := ValidateFirewallDefinition(params.Definition)
	if err != nil {
		out.Blockers = append(out.Blockers, Notice{Code: FirewallCodeDefinitionInvalid, Message: err.Error()})
		out.DisplayName = "防火墙规则（定义不合法）"
		return out, nil
	}
	out.Normalized = FirewallApplyParams{ExpectedConfigHash: params.ExpectedConfigHash, Definition: def}
	out.DisplayName = describeFirewallDefinition(*def)
	out.IdentityFingerprint = "firewall:ufw"
	current := firewallConfigHash(snap.state, snap.managed)
	out.StateVersion = current
	out.Impacts = e.futureImpacts(ctx, snap, *def)

	if !snap.state.Editable {
		msg := "这台机器的防火墙不由 UFW 独占管理"
		if len(snap.state.Reasons) > 0 {
			msg = strings.Join(snap.state.Reasons, "；")
		}
		out.Blockers = append(out.Blockers, Notice{Code: FirewallCodeUnsupported, Message: msg})
	}
	for _, issue := range snap.issues {
		out.Blockers = append(out.Blockers, Notice{Code: FirewallCodeUnsupported,
			Message: "读不到 ufw 的现状：" + issue.Detail})
	}
	for _, line := range snap.unknown {
		out.Blockers = append(out.Blockers, Notice{Code: FirewallCodeUnknownRule,
			Message: "现场有一条带面板标记、但面板认不出来的规则：" + line})
	}
	if params.ExpectedConfigHash != "" && params.ExpectedConfigHash != current {
		out.Blockers = append(out.Blockers, Notice{Code: FirewallCodeConfigChanged,
			Message: "预检之后现场变了：现在的指纹是 " + shortHash(current) +
				"，你看到的是 " + shortHash(params.ExpectedConfigHash) + "。刷新后重来"})
	}
	if rec, err := e.pendingRecord(); err == nil && rec != nil {
		out.Blockers = append(out.Blockers, Notice{Code: FirewallCodeRollbackPending,
			Message: "还有一条没确认的改动（到期 " + rec.deadline().Format(time.RFC3339) +
				"）：先确认它或把它退回去，再改下一次"})
	}
	out.Blockers = append(out.Blockers, e.managementBlockers(ctx, snap, *def)...)

	if len(out.Blockers) == 0 {
		out.Allowed = true
	}
	return out, nil
}

// futureImpacts 把"改完之后会怎样"写成影响说明。
func (e *FirewallEngine) futureImpacts(ctx context.Context, snap snapshot, def FirewallDefinition) []Impact {
	desired := map[string]ManagedRule{}
	for _, rule := range def.Rules {
		desired[rule.ID] = rule
	}
	present := map[string]ManagedRule{}
	for _, rule := range snap.managed {
		present[rule.ID] = rule
	}
	var add, change, drop []string
	for id, rule := range desired {
		old, ok := present[id]
		switch {
		case !ok:
			add = append(add, rule.describe())
		case !sameRule(old, rule):
			change = append(change, old.describe()+" → "+rule.describe())
		}
	}
	for id, rule := range present {
		if _, ok := desired[id]; !ok {
			drop = append(drop, rule.describe())
		}
	}
	sort.Strings(add)
	sort.Strings(change)
	sort.Strings(drop)

	out := []Impact{}
	if snap.state.Enabled != def.Enabled {
		state := "启用"
		if !def.Enabled {
			state = "停用"
		}
		out = append(out, Impact{Kind: "firewall-state",
			Description: "防火墙会" + state + "（现在是 " + enabledWord(snap.state.Enabled) + "）"})
	}
	if len(add) > 0 {
		out = append(out, Impact{Kind: "firewall-add",
			Description: "新增 " + strconv.Itoa(len(add)) + " 条：" + strings.Join(add, "；")})
	}
	if len(change) > 0 {
		out = append(out, Impact{Kind: "firewall-change",
			Description: "改写 " + strconv.Itoa(len(change)) + " 条：" + strings.Join(change, "；")})
	}
	if len(drop) > 0 {
		out = append(out, Impact{Kind: "firewall-remove",
			Description: "删除 " + strconv.Itoa(len(drop)) + " 条：" + strings.Join(drop, "；")})
	}
	out = append(out, Impact{Kind: "firewall-rollback",
		Description: fmt.Sprintf("改完之后 %d 秒内不确认，系统会自己把规则集退回改之前的样子（含启用状态）",
			int(FirewallRollbackWindow.Seconds()))})
	return out
}

func enabledWord(enabled bool) string {
	if enabled {
		return "启用"
	}
	return "未启用"
}

// managementBlockers 检查"改完之后面板与 SSH 还进得来吗"。
//
// 这是 P14.3 里唯一一条**不许放过**的检查：规则写错的后果就是把管理员关在门外，
// 而面板就在门里面。判据只有一条——**启用状态下**，每一个对外可达的管理端口都必须
// 有一条 `from any` 的放行（面板自己的规则或管理员手写的都算）。
//
// 只看"对外可达"的端口：绑在 127.0.0.1 上的面板不需要任何规则，ufw 自己的
// before.rules 已经放行 lo。要求它为回环端口也加一条规则，只会逼人加一条没用的规则，
// 然后在下一次改动里把它当成噪音删掉。
func (e *FirewallEngine) managementBlockers(ctx context.Context, snap snapshot, def FirewallDefinition) []Notice {
	if !def.Enabled {
		// 停用状态下没有任何规则会挡人：这条检查没有对象。
		return nil
	}
	list := Listeners(e.opts.Read)
	needed, identified := managementPorts(list)
	// 一条管理端口都认不出来 + 监听表本身没读全：这时"面板没在听"与"我没看见面板"
	// 分不开，而分不开的时候只能停下。宁可让用户先去看一眼 /network/listeners，
	// 也不要在一张读不准的监听表上赌"面板大概没暴露"。
	if identified == 0 && len(list.Issues) > 0 {
		return []Notice{{Code: FirewallCodeManagementUnknown,
			Message: "读不出面板与 SSH 的监听端口（监听表本身没读全）：无法确认改完之后还进得来。" +
				"先在 /network/listeners 里看到它们再改"}}
	}
	// 改完之后会生效的放行规则 = 管理员手写的（原样保留） + 面板这份定义里的。
	//
	// 带面板标记的规则一律交给定义算，不看现场：这次可能正把它从 allow 改成 deny、
	// 或者整个删掉，拿现场当依据会让预检看不到自己正要关掉的那个口。
	allowed := map[int]bool{}
	for _, added := range snap.allAdded {
		if added.Rule.Action != RuleAllow || added.Rule.From != "" || added.Rule.Proto != ProtoTCP {
			continue
		}
		if strings.HasPrefix(added.Comment, firewallCommentPrefix) {
			continue
		}
		allowed[added.Rule.Port] = true
	}
	for _, rule := range def.Rules {
		if rule.Action == RuleAllow && rule.From == "" && rule.Proto == ProtoTCP {
			allowed[rule.Port] = true
		}
	}
	out := []Notice{}
	for _, port := range needed {
		if allowed[port.Port] {
			continue
		}
		out = append(out, Notice{Code: FirewallCodeManagementPath,
			Message: fmt.Sprintf("%s：启用之后 %d/%s 没有任何放行规则，改完就再也进不来了。"+
				"先加一条 from any 的 allow 再改", port.Why, port.Port, ProtoTCP)})
	}
	return out
}

// managementPort 是一个必须保住的管理端口。
type managementPort struct {
	Port int
	Why  string
}

// managementPorts 列出"启用防火墙之后必须仍然可达"的端口，并报告其中有几个是
// **按进程名认出来的**（不是猜出来的）。
//
// 两道来源：
//
//  1. 面板自己绑在非回环地址上的监听端口（绑在回环上的不需要规则）；
//  2. SSH。优先按进程名认（`sshd`），一个都认不出来时退回到 22/tcp——**宁可多要一条
//     放行，也不能少要一条**：少要的那条会让人再也连不上机器，多要的那条只是浪费一行。
func managementPorts(list ListenerList) ([]managementPort, int) {
	seen := map[int]bool{}
	out := []managementPort{}
	identified := 0
	add := func(port int, why string) {
		if port <= 0 || seen[port] {
			return
		}
		seen[port] = true
		out = append(out, managementPort{Port: port, Why: why})
	}
	sshFound := false
	for _, l := range list.Listeners {
		if l.Protocol != "tcp" || l.Loopback {
			continue
		}
		name := strings.ToLower(l.Process)
		switch {
		case strings.Contains(name, "upanel"):
			add(l.Port, "面板自己的监听端口")
			identified++
		case strings.HasPrefix(name, "sshd"):
			add(l.Port, "SSH")
			identified++
			sshFound = true
		}
	}
	if !sshFound {
		add(22, "SSH（没认出来 sshd 的监听，按默认端口要求）")
	}
	return out, identified
}

// AssessConfirm 是 firewall.confirm 的预检。
func (e *FirewallEngine) AssessConfirm(ctx context.Context, params FirewallConfirmParams) (Assessment, error) {
	// Normalized 必须在**每一个**返回路径上都是非 nil。validatePlanResult 把
	// `NormalizedParams == nil` 判成"预检结果不合法"，于是"没有待确认记录"这种最普通
	// 的拒绝会变成 500——用户看到的是内部错误，而不是"你确认得太早/太晚"。真机 P14.3
	// 验收撞到的就是这一条。网络那条路（applyengine.go 的 AssessConfirm）是把
	// `normalized := params` 放在函数第一行解决的，这里照同一个做法。
	normalized := FirewallConfirmParams{Token: strings.TrimSpace(params.Token)}
	out := Assessment{Locks: []string{FirewallLockName}, DisplayName: "确认防火墙改动",
		Normalized: normalized, IdentityFingerprint: "firewall:ufw"}
	rec, err := e.pendingRecord()
	if err != nil {
		out.Blockers = append(out.Blockers, Notice{Code: FirewallCodeUnsupported, Message: err.Error()})
		return out, nil
	}
	if rec == nil {
		out.Blockers = append(out.Blockers, Notice{Code: FirewallCodeNoPending,
			Message: "没有待确认的防火墙改动（已经确认过、已经退回去、或者根本没改过）"})
		return out, nil
	}
	if params.Token != "" && params.Token != rec.Token {
		out.Blockers = append(out.Blockers, Notice{Code: FirewallCodeTokenMismatch,
			Message: "这个 token 不是当前待确认的那一条（当前是 " + rec.Token + "）"})
		return out, nil
	}
	out.StateVersion = rec.Token
	out.Normalized = FirewallConfirmParams{Token: rec.Token}
	if !e.now().Before(rec.deadline()) {
		out.Blockers = append(out.Blockers, Notice{Code: FirewallCodeConfirmExpired,
			Message: "确认来得太晚了：窗口在 " + rec.deadline().Format(time.RFC3339) + " 就到期了，系统应当已经退回原样"})
		return out, nil
	}
	out.Allowed = true
	out.Impacts = []Impact{{Kind: "firewall-confirm",
		Description: "保留这次改动，取消到期自动退回"}}
	return out, nil
}

// AssessRollback 是 firewall.rollback 的预检。
func (e *FirewallEngine) AssessRollback(ctx context.Context, params FirewallRollbackParams) (Assessment, error) {
	// 同 AssessConfirm：Normalized 不能只在"有记录"那条路上赋值，否则拒绝分支会变成 500。
	normalized := FirewallRollbackParams{Token: strings.TrimSpace(params.Token)}
	out := Assessment{Locks: []string{FirewallLockName}, DisplayName: "立刻退回防火墙改动",
		Normalized: normalized, IdentityFingerprint: "firewall:ufw"}
	rec, err := e.pendingRecord()
	if err != nil {
		out.Blockers = append(out.Blockers, Notice{Code: FirewallCodeUnsupported, Message: err.Error()})
		return out, nil
	}
	if rec == nil {
		out.Blockers = append(out.Blockers, Notice{Code: FirewallCodeNoPending,
			Message: "没有待确认的防火墙改动，没有东西可以退回去"})
		return out, nil
	}
	if params.Token != "" && params.Token != rec.Token {
		out.Blockers = append(out.Blockers, Notice{Code: FirewallCodeTokenMismatch,
			Message: "这个 token 不是当前待确认的那一条（当前是 " + rec.Token + "）"})
		return out, nil
	}
	out.StateVersion = rec.Token
	out.Normalized = FirewallRollbackParams{Token: rec.Token}
	out.Allowed = true
	out.Impacts = []Impact{{Kind: "firewall-rollback",
		Description: "立刻退回：" + rec.describePrevious()}}
	return out, nil
}

// ---- 执行 ----

// FirewallWriteRequest 是执行入口的参数。
type FirewallWriteRequest struct {
	Action               string
	Params               any
	ExpectedIdentity     string
	ExpectedStateVersion string
	OperationID          string
	Progress             func(step string, fields map[string]any)
}

// FirewallWriteReport 是执行报告。
type FirewallWriteReport struct {
	Outcome      string
	Observed     map[string]any
	Data         map[string]any
	RecoveryHint string
}

// ExecuteWrite 分发三个动作。
func (e *FirewallEngine) ExecuteWrite(ctx context.Context, req FirewallWriteRequest) (FirewallWriteReport, error) {
	switch req.Action {
	case FirewallActionApply:
		params, ok := req.Params.(FirewallApplyParams)
		if !ok {
			return FirewallWriteReport{}, fmt.Errorf("%w: firewall.apply 参数类型是 %T", ErrPreflightChanged, req.Params)
		}
		return e.apply(ctx, params, req)
	case FirewallActionConfirm:
		params, ok := req.Params.(FirewallConfirmParams)
		if !ok {
			return FirewallWriteReport{}, fmt.Errorf("%w: firewall.confirm 参数类型是 %T", ErrPreflightChanged, req.Params)
		}
		return e.confirm(ctx, params, req)
	case FirewallActionRollback:
		params, ok := req.Params.(FirewallRollbackParams)
		if !ok {
			return FirewallWriteReport{}, fmt.Errorf("%w: firewall.rollback 参数类型是 %T", ErrPreflightChanged, req.Params)
		}
		return e.rollbackNow(ctx, params, req)
	default:
		return FirewallWriteReport{}, fmt.Errorf("%w: 未知防火墙动作 %q", ErrPreflightChanged, req.Action)
	}
}

func (e *FirewallEngine) progress(req FirewallWriteRequest, step string, fields map[string]any) {
	if req.Progress != nil {
		req.Progress(step, fields)
	}
}

// apply 是核心：写记录 → 武装 timer → 改规则 → 核验，失败立刻退回。
func (e *FirewallEngine) apply(ctx context.Context, params FirewallApplyParams, req FirewallWriteRequest) (FirewallWriteReport, error) {
	if e.tools.ufw == "" {
		return FirewallWriteReport{}, errors.New("找不到 ufw：无法改防火墙")
	}
	// 执行前必须重跑完整预检，而不只是比较摘要。管理端口、独占管理器与未确认记录
	// 都不是配置摘要的一部分；少查任何一项，预检到执行之间的变化就会被直接写进 UFW。
	assessment, err := e.Assess(ctx, params)
	if err != nil {
		return FirewallWriteReport{}, err
	}
	if !assessment.Allowed {
		return FirewallWriteReport{}, fmt.Errorf("%w: %v", ErrPreflightChanged, assessment.Blockers)
	}
	if req.ExpectedIdentity != "" && req.ExpectedIdentity != assessment.IdentityFingerprint {
		return FirewallWriteReport{}, fmt.Errorf("%w: 防火墙身份已变", ErrPreflightChanged)
	}
	if req.ExpectedStateVersion != "" && req.ExpectedStateVersion != assessment.StateVersion {
		return FirewallWriteReport{}, ErrPreflightChanged
	}
	normalized, ok := assessment.Normalized.(FirewallApplyParams)
	if !ok {
		return FirewallWriteReport{}, errors.New("防火墙预检参数类型不一致")
	}
	def := normalized.Definition
	snap := e.readSnapshot(ctx)

	rec, err := e.newRecord(snap, *def, req.OperationID)
	if err != nil {
		return FirewallWriteReport{}, err
	}
	// 先武装：timer 装不上就不许改。这一步之后，任何失败都有一条退路。
	if err := e.armRollback(ctx, rec, FirewallRollbackWindow); err != nil {
		_ = e.markRecord(rec, StateFailed, "武装回滚 timer 失败："+err.Error())
		return FirewallWriteReport{}, fmt.Errorf("武装回滚 timer 失败（没有改动任何规则）：%w", err)
	}
	e.note(rec, "回滚 timer 已武装：%s", rec.deadline().Format(time.RFC3339))
	e.progress(req, "回滚已就绪", map[string]any{"deadline": rec.deadline().Format(time.RFC3339)})

	if err := e.rewrite(ctx, snap, *def, rec); err != nil {
		e.note(rec, "改动失败：%s", err.Error())
		return e.failAndRestore(ctx, rec, "改动没有留下：已经退回改之前的样子。原因："+err.Error())
	}
	e.progress(req, "规则已写入", nil)

	if err := e.verifyApplied(ctx, *def); err != nil {
		e.note(rec, "核验不符：%s", err.Error())
		return e.failAndRestore(ctx, rec, "读回来与要改的不一样，已经退回改之前的样子："+err.Error())
	}

	return FirewallWriteReport{Outcome: OutcomeVerified, Data: e.reportData(rec),
		Observed: e.observed(ctx),
		RecoveryHint: fmt.Sprintf("请在 %s 之前确认这次改动；不确认的话系统会自动退回",
			rec.deadline().Format(time.RFC3339))}, nil
}

// failAndRestore 是"改动没成"的统一收尾：退回、把记录写成终态、给一句人话。
//
// 记录必须落成终态（rolled_back），不能留着 pending：留着 pending 的后果是
// 下一次改动被自己的旧记录挡住，而那条 timer 还在盘上——已经退回去的东西，
// 不该再有一次"到期再退一遍"的机会。
func (e *FirewallEngine) failAndRestore(ctx context.Context, rec *FirewallRollbackRecord, hint string) (FirewallWriteReport, error) {
	if err := e.runRollback(ctx, rec); err != nil {
		return FirewallWriteReport{Outcome: OutcomeUnknown, RecoveryHint: hintFor(rec),
			Observed: e.observed(ctx)}, nil
	}
	_ = e.disarmRollback(ctx, rec, StateRolledBack, "改动失败，已立刻退回")
	return FirewallWriteReport{Outcome: OutcomeRolledBack, Data: e.reportData(rec),
		Observed: e.observed(ctx), RecoveryHint: hint}, nil
}

// rewrite 把现场改成目标状态：先补放行、再收多余、最后动启用状态。
//
// 顺序不是随意的。补在前：ufw 是"先匹配先赢"，把想要的放行尽早插进去，才能在
// 同一批改动里既收掉一条旧的 deny 又不留一个"两个都不生效"的空档。启用放在最后：
// 启用是唯一会把规则真的套到包上的动作，前面都失败了就还来得及。
func (e *FirewallEngine) rewrite(ctx context.Context, snap snapshot, def FirewallDefinition, rec *FirewallRollbackRecord) error {
	present := map[string]ManagedRule{}
	for _, rule := range snap.managed {
		present[rule.ID] = rule
	}
	desired := map[string]ManagedRule{}
	for _, rule := range def.Rules {
		desired[rule.ID] = rule
	}

	// 1. 新增（含"同 ID 但形状变了"的那条：先删后加以保证读回来只有一条）
	for _, rule := range def.Rules {
		if old, ok := present[rule.ID]; ok && sameRule(old, rule) {
			continue
		}
		if old, ok := present[rule.ID]; ok {
			if err := e.ufwDelete(ctx, old); err != nil {
				return err
			}
			e.note(rec, "删除旧规则：%s", old.describe())
		}
		if err := e.ufwAdd(ctx, rule); err != nil {
			return err
		}
		e.note(rec, "新增规则：%s", rule.describe())
	}

	// 2. 删除不在定义里的
	drop := []ManagedRule{}
	for id, rule := range present {
		if _, ok := desired[id]; !ok {
			drop = append(drop, rule)
		}
	}
	sort.Slice(drop, func(i, j int) bool { return drop[i].ID < drop[j].ID })
	for _, rule := range drop {
		if err := e.ufwDelete(ctx, rule); err != nil {
			return err
		}
		e.note(rec, "删除规则：%s", rule.describe())
	}

	// 3. 启用/停用
	if snap.state.Enabled != def.Enabled {
		args := []string{"disable"}
		if def.Enabled {
			args = []string{"--force", "enable"}
		}
		res, err := e.runUFWResult(ctx, args...)
		if err != nil {
			return fmt.Errorf("ufw %s 失败：%w", strings.Join(args, " "), err)
		}
		if res.Code != 0 {
			return fmt.Errorf("ufw %s 退出码 %d：%s", strings.Join(args, " "), res.Code, res.Output)
		}
		e.note(rec, "防火墙状态：%s", enabledWord(def.Enabled))
	}
	return nil
}

// verifyApplied 读回现场，逐项核对。
func (e *FirewallEngine) verifyApplied(ctx context.Context, def FirewallDefinition) error {
	want := append([]ManagedRule(nil), def.Rules...)
	sort.Slice(want, func(i, j int) bool { return want[i].ID < want[j].ID })
	// 计时用真实时钟，不用注入的 Now：这个窗口是"真的等一会儿"，跟记录里的时间戳
	// 不是一回事。用注入的时钟，单测里它会停住，于是这个循环永远等不到到期。
	start := time.Now()
	for {
		snap := e.readSnapshot(ctx)
		if err := compareFirewallState(snap, def.Enabled, want); err == nil {
			return nil
		} else if time.Since(start) >= e.opts.VerifyWindow {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// compareFirewallState 比对"读回来的"与"要的"。错误信息要能直接读出来差在哪。
func compareFirewallState(snap snapshot, enabled bool, want []ManagedRule) error {
	if len(snap.unknown) > 0 {
		return fmt.Errorf("现场还有认不出来的面板规则：%s", strings.Join(snap.unknown, "；"))
	}
	if snap.state.Enabled != enabled {
		return fmt.Errorf("启用状态对不上：要的是 %s，读回来是 %s", enabledWord(enabled),
			enabledWord(snap.state.Enabled))
	}
	got := map[string]ManagedRule{}
	for _, rule := range snap.managed {
		got[rule.ID] = rule
	}
	for _, rule := range want {
		actual, ok := got[rule.ID]
		if !ok {
			return fmt.Errorf("规则 %s 没有出现在读回来的规则集里（要的是 %s）", rule.ID, rule.spec())
		}
		if !sameRule(actual, rule) {
			return fmt.Errorf("规则 %s 读回来是 %s，要的是 %s", rule.ID, actual.spec(), rule.spec())
		}
		delete(got, rule.ID)
	}
	if len(got) > 0 {
		extra := make([]string, 0, len(got))
		for id, rule := range got {
			extra = append(extra, id+"="+rule.spec())
		}
		sort.Strings(extra)
		return fmt.Errorf("还多出来 %d 条面板规则：%s", len(extra), strings.Join(extra, "；"))
	}
	return nil
}

// firewallVerifyWindow 是"改完等它生效"的时间窗。
//
// ufw 是同步生效的，正常不需要等；留 15 秒是因为 `ufw enable` 之后内核表的装载
// 在某些负载下会落后于命令返回，而"刚启用就判失败并立刻回滚"是最坏的一种误判：
// 用户的规则其实已经生效了，却被我们自己撤掉。
const firewallVerifyWindow = 15 * time.Second

func (e *FirewallEngine) observed(ctx context.Context) map[string]any {
	snap := e.readSnapshot(ctx)
	rules := make([]string, 0, len(snap.managed))
	for _, rule := range snap.managed {
		rules = append(rules, rule.spec()+" # "+rule.comment())
	}
	sort.Strings(rules)
	return map[string]any{
		"enabled":       snap.state.Enabled,
		"manager":       snap.state.Manager,
		"managed_rules": rules,
		"config_hash":   firewallConfigHash(snap.state, snap.managed),
	}
}

// ---- 确认与回滚 ----

func (e *FirewallEngine) confirm(ctx context.Context, params FirewallConfirmParams, req FirewallWriteRequest) (FirewallWriteReport, error) {
	rec, err := e.pendingRecord()
	if err != nil {
		return FirewallWriteReport{}, err
	}
	if rec == nil {
		return FirewallWriteReport{}, fmt.Errorf("%w: 没有待确认的防火墙改动", ErrPreflightChanged)
	}
	if params.Token != "" && params.Token != rec.Token {
		return FirewallWriteReport{}, ErrPreflightChanged
	}
	if !e.now().Before(rec.deadline()) {
		return FirewallWriteReport{}, fmt.Errorf("%w: 窗口已经过去了", ErrPreflightChanged)
	}
	if err := e.disarmRollback(ctx, rec, StateConfirmed, "已确认：保留这次改动"); err != nil {
		return FirewallWriteReport{}, err
	}
	e.progress(req, "改动已确认", nil)
	return FirewallWriteReport{Outcome: OutcomeVerified, Data: e.reportData(rec),
		Observed: e.observed(ctx)}, nil
}

func (e *FirewallEngine) rollbackNow(ctx context.Context, params FirewallRollbackParams, req FirewallWriteRequest) (FirewallWriteReport, error) {
	rec, err := e.pendingRecord()
	if err != nil {
		return FirewallWriteReport{}, err
	}
	if rec == nil {
		return FirewallWriteReport{}, fmt.Errorf("%w: 没有待确认的防火墙改动", ErrPreflightChanged)
	}
	if params.Token != "" && params.Token != rec.Token {
		return FirewallWriteReport{}, ErrPreflightChanged
	}
	if err := e.runRollback(ctx, rec); err != nil {
		return FirewallWriteReport{Outcome: OutcomeUnknown, RecoveryHint: hintFor(rec),
			Observed: e.observed(ctx)}, nil
	}
	_ = e.disarmRollback(ctx, rec, StateRolledBack, "已按请求退回")
	return FirewallWriteReport{
		Outcome:      OutcomeRolledBack,
		Data:         e.reportData(rec),
		Observed:     e.observed(ctx),
		RecoveryHint: "已经退回改之前的样子",
	}, nil
}

func hintFor(rec *FirewallRollbackRecord) string {
	return "改动与退回都没能确认成功：以 root 在本机执行 `ufw status numbered` 看现在是什么样。" +
		"记录在 /var/lib/upanel-agent/firewall/rollback/" + rec.Token + ".json"
}

func (e *FirewallEngine) reportData(rec *FirewallRollbackRecord) map[string]any {
	out := map[string]any{
		"rollback_token": rec.Token,
		"deadline":       rec.deadline().Format(time.RFC3339),
		"window_seconds": int(FirewallRollbackWindow.Seconds()),
		"previous":       rec.describePrevious(),
	}
	if rec.Target != nil {
		out["target"] = rec.Target
	}
	return out
}

// ---- 回滚记录 ----

// FirewallRollbackRecord 是磁盘上那条记录的形状。
//
// 与 P14.2 的记录同一个目录树（`/var/lib/upanel-agent/<领域>/rollback/`），
// 理由也一样：开机对账发生在数据库就绪之前，而读它的是 **systemd 那一侧**。
type FirewallRollbackRecord struct {
	Token  string `json:"token"`
	State  string `json:"state"`
	Reason string `json:"result,omitempty"`

	// PreviousEnabled / PreviousRules 是"退回去要恢复成什么样"。
	PreviousEnabled bool          `json:"previous_enabled"`
	PreviousRules   []ManagedRule `json:"previous_rules"`
	// DesiredRules 是这次要改成什么（诊断用：回滚之后能一眼看出当初想干什么）。
	DesiredRules []ManagedRule `json:"desired_rules"`

	AppliedAt string   `json:"applied_at"`
	Deadline  string   `json:"deadline"`
	Actions   []string `json:"actions"`
	UpdatedAt string   `json:"updated_at"`

	Executable  string         `json:"executable"`
	ConfigPath  string         `json:"config_path,omitempty"`
	OperationID string         `json:"operation_id,omitempty"`
	Target      map[string]any `json:"target,omitempty"`
}

func (r *FirewallRollbackRecord) deadline() time.Time {
	t, err := time.Parse(time.RFC3339, r.Deadline)
	if err != nil {
		return time.Time{}
	}
	return t
}

func (e *FirewallEngine) note(rec *FirewallRollbackRecord, format string, args ...any) {
	rec.Actions = append(rec.Actions, fmt.Sprintf(format, args...))
	rec.UpdatedAt = e.now().Format(time.RFC3339)
	_ = e.writeRecord(rec)
}

func (r *FirewallRollbackRecord) describePrevious() string {
	if len(r.PreviousRules) == 0 {
		if r.PreviousEnabled {
			return "退回到：防火墙启用，面板管理的规则为空"
		}
		return "退回到：防火墙未启用，面板管理的规则为空"
	}
	specs := make([]string, 0, len(r.PreviousRules))
	for _, rule := range r.PreviousRules {
		specs = append(specs, rule.describe())
	}
	sort.Strings(specs)
	return "退回到：" + enabledWord(r.PreviousEnabled) + "，" + strings.Join(specs, "；")
}

func (e *FirewallEngine) recordPath(token string) string {
	return path.Join(e.paths.StateDir, token+".json")
}

func (e *FirewallEngine) newRecord(snap snapshot, def FirewallDefinition, opID string) (*FirewallRollbackRecord, error) {
	token, err := newFirewallToken()
	if err != nil {
		return nil, err
	}
	now := e.now()
	rec := &FirewallRollbackRecord{
		Token:           token,
		State:           StatePending,
		PreviousEnabled: snap.state.Enabled,
		PreviousRules:   snap.managed,
		DesiredRules:    def.Rules,
		AppliedAt:       now.Format(time.RFC3339),
		Deadline:        now.Add(FirewallRollbackWindow).Format(time.RFC3339),
		Actions:         []string{},
		UpdatedAt:       now.Format(time.RFC3339),
		OperationID:     opID,
	}
	if rec.PreviousRules == nil {
		rec.PreviousRules = []ManagedRule{}
	}
	if rec.DesiredRules == nil {
		rec.DesiredRules = []ManagedRule{}
	}
	return rec, nil
}

func newFirewallToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成回滚标识失败：%w", err)
	}
	return firewallTokenPrefix + hex.EncodeToString(buf), nil
}

func (e *FirewallEngine) writeRecord(rec *FirewallRollbackRecord) error {
	if !ValidFirewallToken(rec.Token) {
		return errors.New("回滚标识形状不合法")
	}
	if err := e.fs.MkdirAll(e.paths.StateDir, 0o700); err != nil {
		return fmt.Errorf("建回滚记录目录失败：%w", err)
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return e.writeFileAtomic(e.recordPath(rec.Token), append(data, '\n'), 0o600)
}

func (e *FirewallEngine) readRecord(token string) (*FirewallRollbackRecord, error) {
	if !ValidFirewallToken(token) {
		return nil, errors.New("回滚标识形状不合法")
	}
	data, err := e.fs.ReadFile(e.recordPath(token))
	if err != nil {
		return nil, err
	}
	var rec FirewallRollbackRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("回滚记录 %s 读不动：%w", token, err)
	}
	return &rec, nil
}

// pendingRecord 找出唯一那条 pending 记录。
//
// 目录里同时存在多条 pending 是"上一次回滚没走完"的迹象，不是可以随便挑一条的状态：
// 这时返回错误，让调用方拒绝继续，而不是替人做选择。
func (e *FirewallEngine) pendingRecord() (*FirewallRollbackRecord, error) {
	names, err := e.fs.ListDir(e.paths.StateDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("读回滚记录目录失败：%w", err)
	}
	var found *FirewallRollbackRecord
	for _, name := range names {
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		token := strings.TrimSuffix(name, ".json")
		rec, err := e.readRecord(token)
		if err != nil {
			continue
		}
		if rec.State != StatePending {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("同时存在多条未确认的记录（%s 与 %s）：先弄清上一次发生了什么",
				found.Token, rec.Token)
		}
		found = rec
	}
	return found, nil
}

func (e *FirewallEngine) markRecord(rec *FirewallRollbackRecord, state, result string) error {
	rec.State = state
	rec.Reason = result
	rec.UpdatedAt = e.now().Format(time.RFC3339)
	rec.Actions = append(rec.Actions, result)
	return e.writeRecord(rec)
}

// ---- systemd timer ----

const firewallTemplateName = "upanel-fwrollback@.service"

func firewallTimerUnit(token string) string { return "upanel-fwrollback@" + token + ".timer" }

func (e *FirewallEngine) timerPath(token string) string {
	return path.Join(e.paths.SystemdDir, firewallTimerUnit(token))
}

// armRollback 写模板 + 这台 token 的 timer，然后 start。
//
// 与 P14.2 的 armRollback 形状相同、模板名不同，**故意不做成同一个函数**：
// 两边的 ExecStart 子命令不同（netrollback / fwrollback），硬合并会把"哪个子命令
// 对应哪种回滚"这个唯一区分点变成一个参数，而参数传错时的现象是"防火墙回滚跑了
// netplan 的回滚"——那是最难查的一类。
//
// `OnActiveSec=` 的已知限制（并发 daemon-reload 会把它重新起算）见 Q457，
// 防火墙这条 timer 同样受影响，方向一样（只会晚，不会提前）。
func (e *FirewallEngine) armRollback(ctx context.Context, rec *FirewallRollbackRecord, window time.Duration) error {
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
	configArg := ""
	if e.opts.ConfigPath != "" {
		configArg = ` --config "` + e.opts.ConfigPath + `"`
	}
	template := fmt.Sprintf(`# 由 uPanel 生成（P14.3）：到期未确认则把 UFW 规则集退回改之前的样子。
# 它跑的是代理自己的二进制，不依赖 upanel-web 或面板数据库是否还在。
[Unit]
Description=uPanel firewall rollback (%%i)

[Service]
Type=oneshot
RemainAfterExit=no
ExecStart="%s" fwrollback --token %%i%s
TimeoutStartSec=180
`, exe, configArg)
	if err := e.writeFileAtomic(path.Join(e.paths.SystemdDir, firewallTemplateName),
		[]byte(template), 0o644); err != nil {
		return err
	}
	seconds := int(window.Seconds())
	if seconds <= 0 {
		seconds = 1
	}
	timer := fmt.Sprintf(`# 由 uPanel 生成（P14.3）：%d 秒后触发回滚。
[Unit]
Description=uPanel firewall rollback timer (%s)

[Timer]
OnActiveSec=%ds
AccuracySec=1s
Unit=upanel-fwrollback@%s.service
`, seconds, rec.Token, seconds, rec.Token)
	if err := e.writeFileAtomic(e.timerPath(rec.Token), []byte(timer), 0o644); err != nil {
		return err
	}
	if err := e.systemctl(ctx, "daemon-reload"); err != nil {
		return err
	}
	return e.systemctl(ctx, "start", firewallTimerUnit(rec.Token))
}

// disarmRollback 停 timer、删单元、记录写终态。
func (e *FirewallEngine) disarmRollback(ctx context.Context, rec *FirewallRollbackRecord, state, result string) error {
	if e.tools.systemctl != "" {
		_ = e.systemctl(ctx, "stop", firewallTimerUnit(rec.Token))
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

// runRollback 是回滚的唯一实现：timer 触发的子命令、显式 rollback、以及 apply 失败时
// 立刻退回，三条路都走这里。
func (e *FirewallEngine) runRollback(ctx context.Context, rec *FirewallRollbackRecord) error {
	if e.tools.ufw == "" {
		return e.markRecord(rec, StateFailed, "找不到 ufw：无法回滚")
	}
	// 现在的现场：我们要把它改成 PreviousRules + PreviousEnabled。
	snap := e.readSnapshot(ctx)
	if len(snap.unknown) > 0 {
		return e.markRecord(rec, StateFailed,
			"回滚时现场有认不出来的面板规则："+strings.Join(snap.unknown, "；"))
	}
	present := map[string]ManagedRule{}
	for _, rule := range snap.managed {
		present[rule.ID] = rule
	}
	want := map[string]ManagedRule{}
	for _, rule := range rec.PreviousRules {
		want[rule.ID] = rule
	}
	// 先补回原来的，再删掉多出来的：顺序与 apply 一致，理由也一样。
	for _, rule := range rec.PreviousRules {
		if old, ok := present[rule.ID]; ok && sameRule(old, rule) {
			continue
		}
		if old, ok := present[rule.ID]; ok {
			if err := e.ufwDelete(ctx, old); err != nil {
				return e.markRecord(rec, StateFailed, "回滚时删规则失败："+err.Error())
			}
		}
		if err := e.ufwAdd(ctx, rule); err != nil {
			return e.markRecord(rec, StateFailed, "回滚时加规则失败："+err.Error())
		}
	}
	extra := []ManagedRule{}
	for id, rule := range present {
		if _, ok := want[id]; !ok {
			extra = append(extra, rule)
		}
	}
	sort.Slice(extra, func(i, j int) bool { return extra[i].ID < extra[j].ID })
	for _, rule := range extra {
		if err := e.ufwDelete(ctx, rule); err != nil {
			return e.markRecord(rec, StateFailed, "回滚时删规则失败："+err.Error())
		}
	}
	if snap.state.Enabled != rec.PreviousEnabled {
		args := []string{"disable"}
		if rec.PreviousEnabled {
			args = []string{"--force", "enable"}
		}
		res, err := e.runUFWResult(ctx, args...)
		if err != nil || res.Code != 0 {
			return e.markRecord(rec, StateFailed, "回滚时切换启用状态失败："+errString(err)+res.Output)
		}
	}
	// 读回来确认真的退回去了；对不上就记 failed，不再重试。
	final := e.readSnapshot(ctx)
	wanted := append([]ManagedRule(nil), rec.PreviousRules...)
	sort.Slice(wanted, func(i, j int) bool { return wanted[i].ID < wanted[j].ID })
	if err := compareFirewallState(final, rec.PreviousEnabled, wanted); err != nil {
		return e.markRecord(rec, StateFailed, "退回之后读回来仍对不上："+err.Error())
	}
	e.note(rec, "回滚完成：%s", rec.describePrevious())
	return nil
}

// Reconcile 在代理启动时跑一次：上次有没有一条没确认的改动。
//
// 与 P14.2 的 Reconcile 同一条口径：过期的立刻退回去，没过期的按剩余时间重新武装。
func (e *FirewallEngine) Reconcile(ctx context.Context) error {
	rec, err := e.pendingRecord()
	if err != nil {
		return err
	}
	if rec == nil {
		return nil
	}
	if !e.now().Before(rec.deadline()) {
		if err := e.runRollback(ctx, rec); err != nil {
			return err
		}
		return e.disarmRollback(ctx, rec, StateRolledBack, "开机对账：窗口已过，已退回")
	}
	left := rec.deadline().Sub(e.now())
	return e.armRollback(ctx, rec, left)
}

// ExecutePendingRollback 给 `fwrollback` 子命令用：按 token 找到记录并退回去。
func (e *FirewallEngine) ExecutePendingRollback(ctx context.Context, token string) error {
	rec, err := e.readRecord(token)
	if err != nil {
		return err
	}
	if rec.State != StatePending {
		// 已经确认过或已经退回去过：timer 晚到一步，什么都不做。
		return nil
	}
	if err := e.runRollback(ctx, rec); err != nil {
		return err
	}
	return e.disarmRollback(ctx, rec, StateRolledBack, "到期自动退回")
}

// ---- 跑命令 ----

// ufwResult 是一次 ufw 调用的结果。
type ufwResult struct {
	Code   int
	Output string
}

// runUFW 跑一条 ufw 命令并返回合并输出。它是 ufw 这条路上**唯一**的出口。
func (e *FirewallEngine) runUFW(ctx context.Context, args ...string) (string, int, error) {
	res, err := e.runUFWResult(ctx, args...)
	return res.Output, res.Code, err
}

func (e *FirewallEngine) runUFWResult(ctx context.Context, args ...string) (ufwResult, error) {
	if e.tools.ufw == "" {
		return ufwResult{Code: -1}, errors.New("找不到 ufw")
	}
	out, err := e.opts.Runner.Run(ctx, syscmd.Spec{
		Program: e.tools.ufw,
		Args:    args,
	}, syscmd.Limits{Timeout: ufwTimeout})
	res := ufwResult{Code: out.ExitCode, Output: strings.TrimSpace(out.Stdout + out.Stderr)}
	return res, err
}

// ufwTimeout 单条 ufw 命令的预算。`ufw enable` 要重写整张内核表，慢的时候几秒；
// 30 秒是"它卡住了"与"它还在干活"之间的分界。
const ufwTimeout = 30 * time.Second

// ufwAdd 加一条规则。
func (e *FirewallEngine) ufwAdd(ctx context.Context, rule ManagedRule) error {
	args := append(strings.Fields(rule.spec()), "comment", rule.comment())
	res, err := e.runUFWResult(ctx, args...)
	if err != nil {
		return fmt.Errorf("ufw %s 失败：%w", strings.Join(args, " "), err)
	}
	if res.Code != 0 {
		return fmt.Errorf("ufw %s 退出码 %d：%s", strings.Join(args, " "), res.Code, res.Output)
	}
	return nil
}

// ufwDelete 删一条规则。
//
// 这里有一件必须防的事：**删一条不存在的规则时 ufw 退出码是 0**，只在输出里说
// `Could not delete non-existent rule`（0.36.1 实测）。所以判据是输出，不是退出码——
// 拿退出码当判据，会让"我以为删掉了"变成静默的假成功。
func (e *FirewallEngine) ufwDelete(ctx context.Context, rule ManagedRule) error {
	args := append([]string{"delete"}, strings.Fields(rule.spec())...)
	args = append(args, "comment", rule.comment())
	res, err := e.runUFWResult(ctx, args...)
	if err != nil {
		return fmt.Errorf("ufw %s 失败：%w", strings.Join(args, " "), err)
	}
	if res.Code != 0 {
		return fmt.Errorf("ufw %s 退出码 %d：%s", strings.Join(args, " "), res.Code, res.Output)
	}
	if strings.Contains(res.Output, "Could not delete non-existent rule") {
		return fmt.Errorf("要删的规则 %s 在 ufw 里不存在（现场与记录不一致）", rule.spec())
	}
	return nil
}

func (e *FirewallEngine) systemctl(ctx context.Context, args ...string) error {
	if e.tools.systemctl == "" {
		return errors.New("找不到 systemctl")
	}
	out, err := e.opts.Runner.Run(ctx, syscmd.Spec{Program: e.tools.systemctl, Args: args},
		syscmd.Limits{Timeout: 30 * time.Second})
	if err != nil {
		return err
	}
	if out.ExitCode != 0 {
		return fmt.Errorf("systemctl %s 退出码 %d：%s", strings.Join(args, " "), out.ExitCode,
			strings.TrimSpace(out.Stdout+out.Stderr))
	}
	return nil
}

// writeFileAtomic 原子替换一份文件：同目录临时文件 + rename。
//
// 与 applyengine.go 里同名方法是**同一套做法、各自一份**。不抽成包级函数：那边的
// 方法吃的是 ApplyEngine 的 fs 字段，抽出共享会让两条写路径的失败处理互相牵连；
// 这里要的是"写完一份文件就完事"，重命名这份风险比共享一份抽象小。
func (e *FirewallEngine) writeFileAtomic(full string, content []byte, perm os.FileMode) error {
	tmp := full + ".upanel-tmp"
	if err := e.fs.WriteFile(tmp, content, perm); err != nil {
		return err
	}
	if err := e.fs.Rename(tmp, full); err != nil {
		_ = e.fs.Remove(tmp)
		return err
	}
	if err := e.fs.SyncDir(path.Dir(full)); err != nil {
		return fmt.Errorf("刷目录 %s 失败：%w", path.Dir(full), err)
	}
	return nil
}
