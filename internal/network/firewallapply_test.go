package network

// P14.3 写路径的单测。
//
// 与 P14.2 一样，这些测试盯的是几条"实现里最容易悄悄丢掉"的保证，而不是行覆盖率：
//
//  1. **先武装、再改规则**——顺序反了，规则就再也回不来了；
//  2. **删不存在的规则不是成功**——ufw 的退出码是 0，只在输出里说一句；
//  3. **管理通路被挡住必须拦住**——规则写错等于把自己关在门外；
//  4. **读回来对不上就自己退回去**——不信命令、只信现场。
//
// 真机验收在独立 Ubuntu VM 上做（见 docs/evidence/P14），这里跑的全是假的。

import (
	"context"
	"errors"
	"path"
	"strings"
	"testing"
	"time"

	"upanel/internal/syscmd"
)

// ---- ufw 的替身 ----

// simRule 是模拟器里的一条规则。Spec 是规范写法（与 ManagedRule.spec 同形），
// Comment 是 ufw 里那条 comment（管理员手写的规则这里是空）。
type simRule struct {
	Spec    string
	Comment string
}

// simUFW 是一台"会记住状态"的 ufw。
//
// 用一个有状态的替身，而不是逐条命令预设输出：这一层的正确性几乎全在"改完之后读回来
// 是什么样"，用一次性的预设回复去演它，等于把被测的逻辑抄一遍写进测试里。
type simUFW struct {
	enabled bool
	rules   []simRule
	// ip 是只读采集那一侧用的替身：状态一变就把 `ufw status` 的正文同步过去。
	ip *fakeRunner
	// calls 按顺序记下每一次命令（"程序名 参数..."）。
	calls []string
	// swallowAdds 里的子串一旦出现在要加的规则里，命令就报成功但不真的加：
	// 模拟"命令说改了，内核里没有"。用来测读回核验。
	swallowAdds []string
	// statusBroken 让 `ufw status` 与 `ufw show added` 都失败：模拟读不到现场。
	statusBroken bool
}

func newSimUFW(ip *fakeRunner) *simUFW {
	s := &simUFW{ip: ip}
	s.sync()
	return s
}

func (s *simUFW) Run(_ context.Context, spec syscmd.Spec, _ syscmd.Limits) (syscmd.Result, error) {
	program := path.Base(spec.Program)
	key := program + " " + strings.Join(spec.Args, " ")
	s.calls = append(s.calls, key)
	switch program {
	case "ufw":
		return s.runUFW(key, spec.Args), nil
	case "systemctl":
		return syscmd.Result{ExitCode: 0}, nil
	default:
		return syscmd.Result{ExitCode: 127}, nil
	}
}

func (s *simUFW) runUFW(key string, args []string) syscmd.Result {
	if s.statusBroken && (key == "ufw status verbose numbered" || key == "ufw show added") {
		return syscmd.Result{ExitCode: 1, Stderr: "ERROR: 读不到规则"}
	}
	switch {
	case key == "ufw status verbose numbered":
		return syscmd.Result{Stdout: s.statusText()}
	case key == "ufw show added":
		return syscmd.Result{Stdout: s.addedText()}
	case len(args) == 2 && args[0] == "--force" && args[1] == "enable":
		s.enabled = true
		s.sync()
		return syscmd.Result{Stdout: "Firewall is active and enabled on system startup"}
	case len(args) == 1 && args[0] == "disable":
		s.enabled = false
		s.sync()
		return syscmd.Result{Stdout: "Firewall stopped and disabled on system startup"}
	case len(args) >= 2 && args[0] == "delete":
		return s.delete(args[1:])
	case len(args) >= 2:
		return s.add(args)
	}
	return syscmd.Result{ExitCode: 1, Stderr: "看不懂的命令：" + key}
}

// splitComment 把参数尾部的 `comment <值>` 拆出来。
func splitComment(args []string) (spec string, comment string) {
	for i := 0; i < len(args); i++ {
		if args[i] == "comment" && i+1 < len(args) {
			return strings.Join(args[:i], " "), args[i+1]
		}
	}
	return strings.Join(args, " "), ""
}

func (s *simUFW) add(args []string) syscmd.Result {
	spec, comment := splitComment(args)
	for _, swallow := range s.swallowAdds {
		if strings.Contains(spec, swallow) {
			// 假装成功，但什么都没变。
			return syscmd.Result{Stdout: "Rules updated"}
		}
	}
	s.rules = append(s.rules, simRule{Spec: spec, Comment: comment})
	s.sync()
	return syscmd.Result{Stdout: "Rules updated"}
}

// delete 复刻 ufw 0.36.1 的真实行为：删一条不存在的规则**退出码仍是 0**，
// 只在输出里说 `Could not delete non-existent rule`。
func (s *simUFW) delete(args []string) syscmd.Result {
	spec, comment := splitComment(args)
	for i, rule := range s.rules {
		if rule.Spec != spec || rule.Comment != comment {
			continue
		}
		s.rules = append(s.rules[:i], s.rules[i+1:]...)
		s.sync()
		return syscmd.Result{Stdout: "Rules updated"}
	}
	return syscmd.Result{Stdout: "Could not delete non-existent rule"}
}

func (s *simUFW) statusText() string {
	if !s.enabled {
		return "Status: inactive\n"
	}
	var b strings.Builder
	b.WriteString("Status: active\n")
	b.WriteString("Logging: on (low)\n")
	b.WriteString("Default: deny (incoming), allow (outgoing), disabled (routed)\n\n")
	b.WriteString("To                         Action      From\n")
	b.WriteString("--                         ------      ----\n")
	for _, rule := range s.rules {
		spec := rule.Spec
		action := strings.ToUpper(strings.Fields(spec)[0])
		to := strings.Fields(spec)[1]
		b.WriteString(pad(to, 27) + pad(action, 12) + "Anywhere")
		if rule.Comment != "" {
			b.WriteString("                   # " + rule.Comment)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func (s *simUFW) addedText() string {
	var b strings.Builder
	b.WriteString("Added user rules (see 'ufw status' for running firewall):\n")
	if len(s.rules) == 0 {
		b.WriteString("(None)\n")
		return b.String()
	}
	for _, rule := range s.rules {
		b.WriteString("ufw " + rule.Spec)
		if rule.Comment != "" {
			b.WriteString(" comment '" + rule.Comment + "'")
		}
		b.WriteString("\n")
	}
	return b.String()
}

// sync 把当前状态同步给只读采集那一侧。
func (s *simUFW) sync() {
	s.ip.reply("ufw status verbose numbered", s.statusText())
}

// indexOf 返回某条命令第一次出现的下标（前缀匹配：token 是随机生成的）。
func (s *simUFW) indexOf(prefix string) int {
	for i, call := range s.calls {
		if strings.HasPrefix(call, prefix) {
			return i
		}
	}
	return -1
}

func pad(s string, n int) string {
	for len(s) < n {
		s += " "
	}
	return s
}

// ---- 装配 ----

const (
	testFwSystemdDir = "/etc/systemd/system"
	testFwStateDir   = "/var/lib/upanel-agent/firewall/rollback"
)

type fwFixture struct {
	fs    *fakeFS
	ip    *fakeRunner
	ufw   *simUFW
	clock *fakeClock
	eng   *FirewallEngine
}

func newFwFixture(t *testing.T) *fwFixture {
	t.Helper()
	fs, ip := simpleHost()
	// ufw 与 systemctl 都在固定目录里（写路径不认 PATH）。
	fs.dir("/usr/sbin", "ufw")
	fs.file("/usr/sbin/ufw", "")
	fs.dir("/usr/bin", "systemctl")
	fs.file("/usr/bin/systemctl", "")
	// 监听表：读得通、里面什么都没有（面板与 SSH 都没在听——最小的一台机器）。
	for _, name := range []string{"net/tcp", "net/tcp6", "net/udp", "net/udp6"} {
		fs.file("/proc/"+name, "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n")
	}

	clock := newFakeClock()
	ufw := newSimUFW(ip)
	deps := testDeps(fs, ip)
	deps.Now = clock.Now
	eng, err := NewFirewallEngine(FirewallOptions{
		Read:       deps,
		Runner:     ufw,
		FS:         fs,
		Executable: func() (string, error) { return "/usr/local/bin/upanel-agent", nil },
		ConfigPath: "/etc/upanel/agent.json",
		Paths:      FirewallPaths{SystemdDir: testFwSystemdDir, StateDir: testFwStateDir},
		Now:        clock.Now,
		// 核验窗口在单测里没有意义（假 ufw 立刻就是终态），缩到 1ms 免得跑成 15 秒。
		VerifyWindow: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("组装防火墙写路径失败: %v", err)
	}
	return &fwFixture{fs: fs, ip: ip, ufw: ufw, clock: clock, eng: eng}
}

// seed 往模拟器里塞一条规则（可以是管理员手写的：comment 传空）。
func (f *fwFixture) seed(spec, comment string) {
	f.ufw.rules = append(f.ufw.rules, simRule{Spec: spec, Comment: comment})
	f.ufw.sync()
}

// configHash 取当前指纹。
func (f *fwFixture) configHash(t *testing.T) string {
	t.Helper()
	return f.eng.Current(context.Background()).ConfigHash
}

// assessApply 走一遍预检。
func (f *fwFixture) assessApply(t *testing.T, def *FirewallDefinition, hash string) Assessment {
	t.Helper()
	assess, err := f.eng.Assess(context.Background(),
		FirewallApplyParams{ExpectedConfigHash: hash, Definition: def})
	if err != nil {
		t.Fatalf("预检失败: %v", err)
	}
	return assess
}

// apply 走预检 + 执行。
func (f *fwFixture) apply(t *testing.T, def *FirewallDefinition) FirewallWriteReport {
	t.Helper()
	hash := f.configHash(t)
	assess := f.assessApply(t, def, hash)
	if !assess.Allowed {
		t.Fatalf("预检应通过，被拒: %+v", assess.Blockers)
	}
	report, err := f.eng.ExecuteWrite(context.Background(), FirewallWriteRequest{
		Action:               FirewallActionApply,
		Params:               assess.Normalized,
		ExpectedIdentity:     assess.IdentityFingerprint,
		ExpectedStateVersion: assess.StateVersion,
		OperationID:          "op-1",
	})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	return report
}

// timerArmed 报告某条 timer 单元在不在。
func (f *fwFixture) timerArmed() (string, bool) {
	names, err := f.fs.ListDir(testFwSystemdDir)
	if err != nil {
		return "", false
	}
	for _, name := range names {
		if strings.HasPrefix(name, "upanel-fwrollback@") && strings.HasSuffix(name, ".timer") {
			return name, true
		}
	}
	return "", false
}

// ---- 解析 ----

func TestParseShowAddedSeparatesManagedAndForeign(t *testing.T) {
	text := `Added user rules (see 'ufw status' for running firewall):
ufw allow 22/tcp comment 'upanel:ssh'
ufw allow from 10.0.0.0/8 to any port 443 proto tcp comment 'upanel:web'
ufw allow 8080/tcp
ufw limit 2222/tcp comment 'upanel:broken id'
`
	managed, unknown := parseShowAdded(text)
	if len(managed) != 2 {
		t.Fatalf("应认出 2 条面板规则，实际 %d: %+v", len(managed), managed)
	}
	if managed[0].ID != "ssh" || managed[0].Port != 22 || managed[0].Proto != ProtoTCP {
		t.Fatalf("第一条解析错: %+v", managed[0])
	}
	if managed[1].ID != "web" || managed[1].From != "10.0.0.0/8" || managed[1].Port != 443 {
		t.Fatalf("第二条解析错: %+v", managed[1])
	}
	// 没有面板标记的那条是管理员手写的：既不算我们的，也不该被当成"认不出来"。
	if len(unknown) != 1 || !strings.Contains(unknown[0], "broken id") {
		t.Fatalf("只应有一条认不出来的面板规则，实际: %+v", unknown)
	}
}

func TestParseManagedSpecRoundTrips(t *testing.T) {
	cases := []ManagedRule{
		{ID: "a", Action: RuleAllow, Port: 22, Proto: ProtoTCP},
		{ID: "b", Action: RuleDeny, Port: 53, Proto: ProtoUDP, From: "192.168.1.0/24"},
		{ID: "c", Action: RuleLimit, Port: 2222, Proto: ProtoTCP},
	}
	for _, want := range cases {
		got, ok := parseManagedSpec(want.spec(), want.ID)
		if !ok {
			t.Fatalf("%s 认不出来", want.spec())
		}
		if !sameRule(got, want) || got.ID != want.ID {
			t.Fatalf("%s 解析回来是 %+v", want.spec(), got)
		}
	}
	// 认不出来的形状一律不猜。
	for _, spec := range []string{
		"allow 22",         // 没有协议
		"allow out 22/tcp", // 出方向
		"allow from 10.0.0.0/8 to any port 22 proto tcp extra",
		"allow any proto tcp",
		"allow 22/sctp comment 'x'",
	} {
		if _, ok := parseManagedSpec(spec, "x"); ok {
			t.Fatalf("%q 不该被认出来", spec)
		}
	}
}

// ---- 顺序：先武装 ----

func TestFirewallApplyArmsTimerBeforeTouchingRules(t *testing.T) {
	f := newFwFixture(t)
	f.seed("allow 22/tcp", "upanel:ssh")
	def := &FirewallDefinition{Enabled: false, Rules: []ManagedRule{
		{ID: "ssh", Action: RuleAllow, Port: 22, Proto: ProtoTCP},
		{ID: "web", Action: RuleAllow, Port: 443, Proto: ProtoTCP},
	}}
	report := f.apply(t, def)
	if report.Outcome != OutcomeVerified {
		t.Fatalf("应 verified，实际 %s：%+v", report.Outcome, report)
	}
	armIdx := f.ufw.indexOf("systemctl start upanel-fwrollback@")
	addIdx := f.ufw.indexOf("ufw allow 443/tcp comment")
	if armIdx < 0 {
		t.Fatalf("没有武装回滚 timer：%v", f.ufw.calls)
	}
	if addIdx < 0 {
		t.Fatalf("没有加规则的命令：%v", f.ufw.calls)
	}
	if armIdx > addIdx {
		t.Fatalf("先改规则后武装 timer：顺序反了就等于没有回滚\n%v", f.ufw.calls)
	}
	if _, ok := f.timerArmed(); !ok {
		t.Fatal("timer 单元没留在盘上")
	}
}

// ---- 预检 ----

func TestFirewallExecuteRejectsPendingRollbackAfterPreflight(t *testing.T) {
	f := newFwFixture(t)
	def := &FirewallDefinition{Enabled: false, Rules: []ManagedRule{
		{ID: "ssh", Action: RuleAllow, Port: 22, Proto: ProtoTCP},
	}}
	f.apply(t, def)
	before := len(f.ufw.calls)

	// 上一笔仍在回滚窗口内。执行必须自己挡住，不能依赖调用方刚好又做了一次预检。
	params := FirewallApplyParams{Definition: def}
	_, err := f.eng.ExecuteWrite(context.Background(), FirewallWriteRequest{
		Action: FirewallActionApply, Params: params, OperationID: "op-2",
	})
	if !errors.Is(err, ErrPreflightChanged) {
		t.Fatalf("未确认记录必须挡住执行: %v", err)
	}
	for _, call := range f.ufw.calls[before:] {
		if !strings.HasPrefix(call, "ufw status") && !strings.HasPrefix(call, "ufw show added") {
			t.Fatalf("被拒绝后仍执行了写命令: %v", f.ufw.calls[before:])
		}
	}
}

func TestFirewallApplyRejectsStaleConfigHash(t *testing.T) {
	f := newFwFixture(t)
	stale := f.configHash(t)
	// 预检之后现场变了：别人（或管理员）加了一条面板规则的形状。
	f.seed("allow 22/tcp", "upanel:ssh")
	def := &FirewallDefinition{Enabled: false, Rules: []ManagedRule{
		{ID: "web", Action: RuleAllow, Port: 443, Proto: ProtoTCP}}}
	assess := f.assessApply(t, def, stale)
	if assess.Allowed {
		t.Fatal("指纹变了还放行")
	}
	found := false
	for _, blocker := range assess.Blockers {
		if blocker.Code == FirewallCodeConfigChanged {
			found = true
		}
	}
	if !found {
		t.Fatalf("应报 FIREWALL_CONFIG_CHANGED，实际 %+v", assess.Blockers)
	}
}

func TestFirewallApplyBlocksWhenManagementPathDisappears(t *testing.T) {
	f := newFwFixture(t)
	// 一条会把自己关在门外的改动：启用防火墙，却没有任何放行 SSH 的规则。
	def := &FirewallDefinition{Enabled: true, Rules: []ManagedRule{
		{ID: "web", Action: RuleAllow, Port: 443, Proto: ProtoTCP}}}
	assess := f.assessApply(t, def, f.configHash(t))
	if assess.Allowed {
		t.Fatal("启用防火墙却不放行 SSH，居然放行了")
	}
	if code := firstBlockerCode(assess.Blockers); code != FirewallCodeManagementPath {
		t.Fatalf("应报 FIREWALL_MANAGEMENT_PATH，实际 %s：%+v", code, assess.Blockers)
	}

	// 补一条放行 SSH 的规则之后就该放行。
	def.Rules = append(def.Rules, ManagedRule{ID: "ssh", Action: RuleAllow, Port: 22, Proto: ProtoTCP})
	assess = f.assessApply(t, def, f.configHash(t))
	if !assess.Allowed {
		t.Fatalf("补上 SSH 放行后仍被拒: %+v", assess.Blockers)
	}
}

func TestFirewallManagementUsesForeignRulesAsWell(t *testing.T) {
	snap := snapshot{allAdded: []addedRule{
		// 管理员手写的一条放行，同样保住 SSH。
		{Rule: ManagedRule{Action: RuleAllow, Port: 22, Proto: ProtoTCP}},
	}}
	f := newFwFixture(t)
	blockers := f.eng.managementBlockers(context.Background(), snap,
		FirewallDefinition{Enabled: true, Rules: []ManagedRule{
			{ID: "web", Action: RuleAllow, Port: 443, Proto: ProtoTCP}}})
	if len(blockers) != 0 {
		t.Fatalf("有一条手写的 allow 22/tcp 就不该拦，实际: %+v", blockers)
	}
}

func TestFirewallManagementIgnoresRulesBeingRemoved(t *testing.T) {
	// 现场有一条面板的 allow 22，但这次的定义里没有它：改完它就没了，
	// 所以"现在有"不能当成"改完还有"。
	snap := snapshot{
		allAdded: []addedRule{{Rule: ManagedRule{Action: RuleAllow, Port: 22, Proto: ProtoTCP},
			Comment: "upanel:ssh"}},
	}
	f := newFwFixture(t)
	blockers := f.eng.managementBlockers(context.Background(), snap,
		FirewallDefinition{Enabled: true, Rules: []ManagedRule{
			{ID: "web", Action: RuleAllow, Port: 443, Proto: ProtoTCP}}})
	if len(blockers) == 0 {
		t.Fatal("把唯一的 SSH 放行删掉却不拦")
	}
}

func firstBlockerCode(blockers []Notice) string {
	if len(blockers) == 0 {
		return ""
	}
	return blockers[0].Code
}

// ---- 加缺删多 ----

func TestFirewallApplyAddsMissingAndRemovesExtra(t *testing.T) {
	f := newFwFixture(t)
	f.seed("allow 22/tcp", "upanel:ssh")   // 定义里还在
	f.seed("allow 8080/tcp", "upanel:old") // 定义里没了：要删
	f.seed("allow 9000/tcp", "")           // 管理员手写的：一条都不许动
	def := &FirewallDefinition{Enabled: false, Rules: []ManagedRule{
		{ID: "ssh", Action: RuleAllow, Port: 22, Proto: ProtoTCP},
		{ID: "web", Action: RuleAllow, Port: 443, Proto: ProtoTCP}}}
	report := f.apply(t, def)
	if report.Outcome != OutcomeVerified {
		t.Fatalf("应 verified，实际 %s：%+v", report.Outcome, report)
	}
	if f.ufw.indexOf("ufw delete allow 8080/tcp comment upanel:old") < 0 {
		t.Fatalf("没有删掉定义里没有的规则：%v", f.ufw.calls)
	}
	if f.ufw.indexOf("ufw allow 443/tcp comment upanel:web") < 0 {
		t.Fatalf("没有加上缺的规则：%v", f.ufw.calls)
	}
	for _, call := range f.ufw.calls {
		if strings.Contains(call, "9000") {
			t.Fatalf("碰了管理员手写的规则：%s", call)
		}
	}
	specs := map[string]bool{}
	for _, rule := range f.ufw.rules {
		specs[rule.Spec+" # "+rule.Comment] = true
	}
	if !specs["allow 22/tcp # upanel:ssh"] || !specs["allow 443/tcp # upanel:web"] ||
		!specs["allow 9000/tcp # "] {
		t.Fatalf("最终规则集不对: %+v", f.ufw.rules)
	}
	if specs["allow 8080/tcp # upanel:old"] {
		t.Fatal("该删的规则还在")
	}
}

func TestUfwDeleteMissingRuleIsFailure(t *testing.T) {
	f := newFwFixture(t)
	// 现场里没有这条，但记录说改之前有：必须报错，不能因为退出码是 0 就当成功。
	err := f.eng.ufwDelete(context.Background(), ManagedRule{
		ID: "ghost", Action: RuleAllow, Port: 1234, Proto: ProtoTCP})
	if err == nil {
		t.Fatal("删一条不存在的规则被当成了成功")
	}
	if !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("错误信息里应说清是「不存在」：%v", err)
	}
}

// ---- 核验与回滚 ----

func TestFirewallApplyRollsBackWhenVerifyMismatch(t *testing.T) {
	f := newFwFixture(t)
	f.seed("allow 22/tcp", "upanel:ssh")
	f.ufw.swallowAdds = []string{"443/tcp"} // 命令说成功，规则没生效
	def := &FirewallDefinition{Enabled: false, Rules: []ManagedRule{
		{ID: "ssh", Action: RuleAllow, Port: 22, Proto: ProtoTCP},
		{ID: "web", Action: RuleAllow, Port: 443, Proto: ProtoTCP}}}
	report := f.apply(t, def)
	if report.Outcome != OutcomeRolledBack {
		t.Fatalf("应 rolled_back，实际 %s：%+v", report.Outcome, report)
	}
	if report.RecoveryHint == "" {
		t.Fatal("rolled_back 必须给人一句人话")
	}
	for _, rule := range f.ufw.rules {
		if strings.Contains(rule.Spec, "443") {
			t.Fatal("没加进去的规则不该留在现场")
		}
	}
}

func TestFirewallRollbackRestoresPreviousSetAndState(t *testing.T) {
	f := newFwFixture(t)
	f.seed("allow 22/tcp", "upanel:ssh")
	def := &FirewallDefinition{Enabled: false, Rules: []ManagedRule{
		{ID: "ssh", Action: RuleAllow, Port: 22, Proto: ProtoTCP},
		{ID: "web", Action: RuleAllow, Port: 443, Proto: ProtoTCP}}}
	if report := f.apply(t, def); report.Outcome != OutcomeVerified {
		t.Fatalf("第一轮应 verified，实际 %s", report.Outcome)
	}
	rec, err := f.eng.pendingRecord()
	if err != nil || rec == nil {
		t.Fatalf("应有待确认记录: rec=%v err=%v", rec, err)
	}
	if err := f.eng.runRollback(context.Background(), rec); err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
	if err := f.eng.disarmRollback(context.Background(), rec, StateRolledBack, "测试里手动退回"); err != nil {
		t.Fatalf("收尾失败: %v", err)
	}
	if len(f.ufw.rules) != 1 || f.ufw.rules[0].Spec != "allow 22/tcp" {
		t.Fatalf("没退回改之前的规则集: %+v", f.ufw.rules)
	}
	if _, ok := f.timerArmed(); ok {
		t.Fatal("回滚之后 timer 还挂着")
	}
	if rec, _ := f.eng.pendingRecord(); rec != nil {
		t.Fatal("回滚之后还留一条 pending 记录")
	}
}

func TestFirewallRollbackRestoresEnabledState(t *testing.T) {
	f := newFwFixture(t)
	f.ufw.enabled = true // 改之前是启用着的
	f.ufw.sync()
	f.seed("allow 22/tcp", "upanel:ssh")
	def := &FirewallDefinition{Enabled: false, Rules: []ManagedRule{}} // 这次要停用并清空
	if report := f.apply(t, def); report.Outcome != OutcomeVerified {
		t.Fatalf("应 verified，实际 %s：%+v", report.Outcome, report)
	}
	if f.ufw.enabled {
		t.Fatal("没停用")
	}
	rec, err := f.eng.pendingRecord()
	if err != nil || rec == nil {
		t.Fatalf("应有待确认记录: %v", err)
	}
	if err := f.eng.runRollback(context.Background(), rec); err != nil {
		t.Fatalf("回滚失败: %v", err)
	}
	if !f.ufw.enabled {
		t.Fatal("回滚没把启用状态恢复回去")
	}
	if len(f.ufw.rules) != 1 || f.ufw.rules[0].Comment != "upanel:ssh" {
		t.Fatalf("回滚没把规则加回来: %+v", f.ufw.rules)
	}
}

func TestConfirmDisarmsTimer(t *testing.T) {
	f := newFwFixture(t)
	def := &FirewallDefinition{Enabled: false, Rules: []ManagedRule{
		{ID: "web", Action: RuleAllow, Port: 443, Proto: ProtoTCP}}}
	if report := f.apply(t, def); report.Outcome != OutcomeVerified {
		t.Fatalf("应 verified，实际 %s", report.Outcome)
	}
	rec, err := f.eng.pendingRecord()
	if err != nil || rec == nil {
		t.Fatalf("应有待确认记录: %v", err)
	}
	assess, err := f.eng.AssessConfirm(context.Background(), FirewallConfirmParams{Token: rec.Token})
	if err != nil || !assess.Allowed {
		t.Fatalf("确认预检应通过: %v %+v", err, assess.Blockers)
	}
	report, err := f.eng.ExecuteWrite(context.Background(), FirewallWriteRequest{
		Action: FirewallActionConfirm, Params: assess.Normalized,
		ExpectedStateVersion: assess.StateVersion,
	})
	if err != nil || report.Outcome != OutcomeVerified {
		t.Fatalf("确认失败: %v %+v", err, report)
	}
	if _, ok := f.timerArmed(); ok {
		t.Fatal("确认之后 timer 还挂着")
	}
	if rec, _ := f.eng.pendingRecord(); rec != nil {
		t.Fatal("确认之后还留一条 pending 记录")
	}
	// 确认之后规则还在（确认的意思就是保留）。
	if len(f.ufw.rules) != 1 {
		t.Fatalf("确认把规则改没了: %+v", f.ufw.rules)
	}
}

func TestConfirmAfterDeadlineIsRefused(t *testing.T) {
	f := newFwFixture(t)
	def := &FirewallDefinition{Enabled: false, Rules: []ManagedRule{
		{ID: "web", Action: RuleAllow, Port: 443, Proto: ProtoTCP}}}
	f.apply(t, def)
	rec, err := f.eng.pendingRecord()
	if err != nil || rec == nil {
		t.Fatalf("应有待确认记录: %v", err)
	}
	f.clock.advance(FirewallRollbackWindow + time.Second)
	assess, err := f.eng.AssessConfirm(context.Background(), FirewallConfirmParams{Token: rec.Token})
	if err != nil {
		t.Fatalf("预检失败: %v", err)
	}
	if assess.Allowed {
		t.Fatal("窗口过去了还允许确认")
	}
	if code := firstBlockerCode(assess.Blockers); code != FirewallCodeConfirmExpired {
		t.Fatalf("应报 FIREWALL_ROLLBACK_EXPIRED，实际 %s", code)
	}
}

// ---- 开机对账 ----

func TestReconcileRollsBackExpiredRecord(t *testing.T) {
	f := newFwFixture(t)
	def := &FirewallDefinition{Enabled: false, Rules: []ManagedRule{
		{ID: "web", Action: RuleAllow, Port: 443, Proto: ProtoTCP}}}
	f.apply(t, def)
	f.clock.advance(FirewallRollbackWindow + time.Second)
	if err := f.eng.Reconcile(context.Background()); err != nil {
		t.Fatalf("对账失败: %v", err)
	}
	if len(f.ufw.rules) != 0 {
		t.Fatalf("过期记录没被退回: %+v", f.ufw.rules)
	}
	if rec, _ := f.eng.pendingRecord(); rec != nil {
		t.Fatal("退回之后还留一条 pending 记录")
	}
}

func TestReconcileRearmsUnexpiredRecord(t *testing.T) {
	f := newFwFixture(t)
	def := &FirewallDefinition{Enabled: false, Rules: []ManagedRule{
		{ID: "web", Action: RuleAllow, Port: 443, Proto: ProtoTCP}}}
	f.apply(t, def)
	before, _ := f.timerArmed()
	f.clock.advance(30 * time.Second)
	if err := f.eng.Reconcile(context.Background()); err != nil {
		t.Fatalf("对账失败: %v", err)
	}
	after, ok := f.timerArmed()
	if !ok {
		t.Fatal("没过期的记录应对着剩余时间重新武装")
	}
	if before != after {
		t.Fatalf("重新武装应沿用同一个 token：%s → %s", before, after)
	}
	if len(f.ufw.rules) != 1 {
		t.Fatalf("没过期的记录不该被退回: %+v", f.ufw.rules)
	}
}

// ---- 读不到现场 ----

func TestFirewallApplyRefusesWhenStateUnreadable(t *testing.T) {
	f := newFwFixture(t)
	f.ufw.statusBroken = true
	def := &FirewallDefinition{Enabled: false, Rules: []ManagedRule{
		{ID: "web", Action: RuleAllow, Port: 443, Proto: ProtoTCP}}}
	assess := f.assessApply(t, def, "")
	if assess.Allowed {
		t.Fatal("读不到现场还放行")
	}
	if code := firstBlockerCode(assess.Blockers); code != FirewallCodeUnsupported {
		t.Fatalf("应报 FIREWALL_UNSUPPORTED，实际 %s：%+v", code, assess.Blockers)
	}
}

// ---- 拒绝分支也必须带规范化参数 ----

// 三个 Assess 的**每一条**返回路径都要带非 nil 的 Normalized。
//
// 缺陷现场（真机 P14.3 验收第一轮，8 条断言因此变红）：AssessConfirm / AssessRollback
// 只在"有 pending 记录"那条路上赋值 Normalized，于是"没有待确认记录"这种最普通的拒绝
// 带着 nil 的 NormalizedParams 上桥；代理那边的 validatePlanResult 见到 nil 就判
// "预检结果不合法" → 500 INTERNAL_ERROR。用户看到的不是"你确认得太早/太晚"，而是
// "服务内部错误"——一条本该说人话的拒绝变成了一次故障。
//
// 这三条测试钉的是"每个拒绝分支都非 nil"，不是某一个分支。
func TestFirewallAssessRefusalsKeepNormalizedParams(t *testing.T) {
	f := newFwFixture(t)
	ctx := context.Background()

	// 1) confirm：没有待确认记录。
	assess, err := f.eng.AssessConfirm(ctx, FirewallConfirmParams{Token: "frb_0123456789abcdef0123456789abcdef"})
	if err != nil {
		t.Fatalf("confirm 预检不该返回错误: %v", err)
	}
	if assess.Allowed {
		t.Fatal("没有待确认记录却放行确认")
	}
	if code := firstBlockerCode(assess.Blockers); code != FirewallCodeNoPending {
		t.Fatalf("应报 FIREWALL_NO_PENDING_ROLLBACK，实际 %s", code)
	}
	if assess.Normalized == nil {
		t.Fatal("confirm 的拒绝分支没带 Normalized：代理会判“预检结果不合法”并回 500")
	}

	// 2) rollback：同样没有待确认记录。
	assess, err = f.eng.AssessRollback(ctx, FirewallRollbackParams{Token: "frb_0123456789abcdef0123456789abcdef"})
	if err != nil {
		t.Fatalf("rollback 预检不该返回错误: %v", err)
	}
	if assess.Allowed {
		t.Fatal("没有待确认记录却放行回滚")
	}
	if code := firstBlockerCode(assess.Blockers); code != FirewallCodeNoPending {
		t.Fatalf("应报 FIREWALL_NO_PENDING_ROLLBACK，实际 %s", code)
	}
	if assess.Normalized == nil {
		t.Fatal("rollback 的拒绝分支没带 Normalized：代理会判“预检结果不合法”并回 500")
	}

	// 3) apply：定义不合法（Decode 在真机上会先拦，但这条纵深防御分支同样不能说 nil）。
	assess = f.assessApply(t, &FirewallDefinition{Enabled: true, Rules: []ManagedRule{
		{ID: "web", Action: RuleAllow, Port: 0, Proto: ProtoTCP}}}, "")
	if assess.Allowed {
		t.Fatal("端口越界却放行")
	}
	if code := firstBlockerCode(assess.Blockers); code != FirewallCodeDefinitionInvalid {
		t.Fatalf("应报 FIREWALL_DEFINITION_INVALID，实际 %s", code)
	}
	if assess.Normalized == nil {
		t.Fatal("apply 的定义不合法分支没带 Normalized：代理会判“预检结果不合法”并回 500")
	}
}

// 第二条：有 pending 但 token 不符，同样是拒绝分支，同样不能是 nil。
func TestFirewallTokenMismatchKeepsNormalizedParams(t *testing.T) {
	f := newFwFixture(t)
	ctx := context.Background()
	def := &FirewallDefinition{Enabled: false, Rules: []ManagedRule{
		{ID: "web", Action: RuleAllow, Port: 443, Proto: ProtoTCP}}}
	if report := f.apply(t, def); report.Outcome != OutcomeVerified {
		t.Fatalf("应 verified，实际 %s", report.Outcome)
	}
	assess, err := f.eng.AssessConfirm(ctx, FirewallConfirmParams{Token: "frb_ffffffffffffffffffffffffffffffff"})
	if err != nil {
		t.Fatalf("预检不该返回错误: %v", err)
	}
	if assess.Allowed {
		t.Fatal("token 不符却放行确认")
	}
	if code := firstBlockerCode(assess.Blockers); code != FirewallCodeTokenMismatch {
		t.Fatalf("应报 FIREWALL_TOKEN_MISMATCH，实际 %s", code)
	}
	if assess.Normalized == nil {
		t.Fatal("token 不符的拒绝分支没带 Normalized")
	}
}
