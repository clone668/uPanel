package schedules

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func validBackup() Definition {
	return Definition{
		Name:               "每天备份 /srv",
		Kind:               KindBackup,
		TargetID:           "pol_0123456789abcdef0123456789abcdef",
		CalendarExpression: "*-*-* 03:30:00",
		Timezone:           "Asia/Shanghai",
		Persistent:         true,
		MaxRuntimeSeconds:  3600,
		Enabled:            true,
	}
}

func validScript() Definition {
	return Definition{
		Name:               "每小时清理临时目录",
		Kind:               KindUserScript,
		TargetID:           "scr_fedcba9876543210fedcba9876543210",
		CalendarExpression: "hourly",
		Timezone:           "UTC",
		Persistent:         false,
		MaxRuntimeSeconds:  300,
		Enabled:            true,
	}
}

// TestValidateAcceptsBothLiveKinds 覆盖 P12.4 真正放行的两种计划。
func TestValidateAcceptsBothLiveKinds(t *testing.T) {
	for _, d := range []Definition{validBackup(), validScript()} {
		if err := d.Validate(); err != nil {
			t.Fatalf("%s 的定义应当合法，得到 %v", d.Kind, err)
		}
	}
}

// TestValidateRejectsCommandTextAsTarget 是这一层最要紧的一条：
// 计划任务不能变成“定时跑一段 shell”。
func TestValidateRejectsCommandTextAsTarget(t *testing.T) {
	cases := []string{
		"rm -rf /srv",
		"/bin/sh -c 'tar czf /tmp/x.tgz /srv'",
		"scr_0123456789abcdef0123456789abcdef", // 备份计划指向脚本 id
		"pol_0123456789ABCDEF0123456789ABCDEF", // 大写十六进制不是我们生成的形态
		"pol_0123",
		"",
	}
	for _, target := range cases {
		d := validBackup()
		d.TargetID = target
		if err := d.Validate(); err == nil {
			t.Fatalf("target_id=%q 应当被拒绝", target)
		}
	}
}

// TestValidateRejectsCalendarInjection 覆盖“表达式会进入单元文件与 argv”这条边界。
func TestValidateRejectsCalendarInjection(t *testing.T) {
	cases := map[string]string{
		"换行注入另一段配置": "*-*-* 03:00:00\nExecStartPre=/bin/false",
		"回车":        "*-*-* 03:00:00\r",
		"制表符":       "*-*-* 03:00:00\t",
		"引号":        `*-*-* "03:00:00"`,
		"分号":        "*-*-* 03:00:00; rm -rf /",
		"空":         "",
		"超长":        strings.Repeat("1", MaxCalendarLen+1),
	}
	for name, expr := range cases {
		d := validBackup()
		d.CalendarExpression = expr
		if err := d.Validate(); err == nil {
			t.Fatalf("%s：表达式 %q 应当被拒绝", name, expr)
		}
	}
}

// TestValidateRejectsOptionLikeCalendar：句首的 `-` 会被 systemd-analyze 当成选项，
// 一条“计划”就变成了一次没有任何执行时间的校验。
func TestValidateRejectsOptionLikeCalendar(t *testing.T) {
	d := validBackup()
	d.CalendarExpression = "--now"
	err := d.Validate()
	if err == nil {
		t.Fatal("以 - 开头的表达式应当被拒绝")
	}
	if ve, ok := err.(*ValidationError); !ok || ve.Field != "calendar_expression" {
		t.Fatalf("期望 calendar_expression 上的校验失败，得到 %#v", err)
	}
}

// TestValidateRequiresExplicitTimezone：时区不能空、不能是 Local、必须是 IANA 名。
func TestValidateRequiresExplicitTimezone(t *testing.T) {
	for _, tz := range []string{"", "   ", "Local", "localtime", "Asia/Shanghai\n", "Europe/Nowhere", "-05:00"} {
		d := validBackup()
		d.Timezone = tz
		if err := d.Validate(); err == nil {
			t.Fatalf("时区 %q 应当被拒绝", tz)
		}
	}
}

// TestValidateRuntimeBounds 覆盖运行时长的两端。
func TestValidateRuntimeBounds(t *testing.T) {
	for _, secs := range []int{0, -1, MaxRuntimeSecondsLimit + 1} {
		d := validBackup()
		d.MaxRuntimeSeconds = secs
		if err := d.Validate(); err == nil {
			t.Fatalf("max_runtime_seconds=%d 应当被拒绝", secs)
		}
	}
	d := validBackup()
	d.MaxRuntimeSeconds = MaxRuntimeSecondsLimit
	if err := d.Validate(); err != nil {
		t.Fatalf("24 小时应当被接受，得到 %v", err)
	}
}

// TestValidateRejectsNameAbuse 覆盖计划名（它会进 Description）。
func TestValidateRejectsNameAbuse(t *testing.T) {
	for _, name := range []string{"", "   ", "备份\nExecStartPre=/bin/false", strings.Repeat("备", MaxNameLen+1)} {
		d := validBackup()
		d.Name = name
		if err := d.Validate(); err == nil {
			t.Fatalf("计划名 %q 应当被拒绝", name)
		}
	}
}

// TestNormalizeTrims 只去空白，不猜默认值。
func TestNormalizeTrims(t *testing.T) {
	d := Definition{Name: "  x  ", TargetID: " pol_x ", CalendarExpression: " hourly ", Timezone: " UTC "}
	got := d.Normalize()
	if got.Name != "x" || got.TargetID != "pol_x" || got.CalendarExpression != "hourly" || got.Timezone != "UTC" {
		t.Fatalf("Normalize 结果不对: %#v", got)
	}
}

// TestCalendarLineCarriesTimezone：时区必须进 OnCalendar 那一行。
func TestCalendarLineCarriesTimezone(t *testing.T) {
	d := validBackup()
	if got := CalendarLine(d); got != "*-*-* 03:30:00 Asia/Shanghai" {
		t.Fatalf("OnCalendar 值不对: %q", got)
	}
}

// TestRenderTimerPinsAllFourRules：时区、错过执行、最长运行、不重叠都要在单元里看得见。
func TestRenderTimerPinsAllFourRules(t *testing.T) {
	d := validBackup()
	raw, err := RenderTimer("sch_0123456789abcdef0123456789abcdef", d)
	if err != nil {
		t.Fatalf("渲染定时器失败: %v", err)
	}
	text := string(raw)
	for _, want := range []string{
		"OnCalendar=*-*-* 03:30:00 Asia/Shanghai",
		"Persistent=true",
		"Unit=upanel-sch_0123456789abcdef0123456789abcdef.service",
		"最长运行 3600 秒",
		"不重叠",
		"WantedBy=timers.target",
		"AccuracySec=1s",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("定时器单元缺少 %q：\n%s", want, text)
		}
	}

	d.Persistent = false
	raw, err = RenderTimer("sch_0123456789abcdef0123456789abcdef", d)
	if err != nil {
		t.Fatalf("渲染定时器失败: %v", err)
	}
	if !strings.Contains(string(raw), "Persistent=false") {
		t.Fatalf("Persistent=false 应当显式写出：\n%s", raw)
	}
}

// TestRenderServiceHasSingleTriggerPath：服务单元只能有一条触发命令，且只带计划 id。
func TestRenderServiceHasSingleTriggerPath(t *testing.T) {
	d := validScript()
	raw, err := RenderService("sch_0123456789abcdef0123456789abcdef", d)
	if err != nil {
		t.Fatalf("渲染服务失败: %v", err)
	}
	text := string(raw)
	if !strings.Contains(text, "ExecStart=/usr/local/bin/upanelctl schedule-trigger --schedule-id sch_0123456789abcdef0123456789abcdef --wait\n") {
		t.Fatalf("ExecStart 不是固定入口：\n%s", text)
	}
	if !strings.Contains(text, "Type=oneshot") {
		t.Fatalf("服务必须是 oneshot（不重叠规则依赖它）：\n%s", text)
	}
	if !strings.Contains(text, "TimeoutStartSec=360s") {
		t.Fatalf("最长运行时间没有写进 TimeoutStartSec：\n%s", text)
	}
	for _, forbidden := range []string{"backup.run", "script.run", "certificate.renew", "/bin/sh", "bash"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("服务单元里不该出现 %q：\n%s", forbidden, text)
		}
	}
}

// TestRenderEscapesPercentSpecifiers：计划名里的 % 必须转义，否则 systemd 会展开它。
func TestRenderEscapesPercentSpecifiers(t *testing.T) {
	d := validBackup()
	d.Name = "备份 %n 每天"
	raw, err := RenderService("sch_0123456789abcdef0123456789abcdef", d)
	if err != nil {
		t.Fatalf("渲染服务失败: %v", err)
	}
	if !strings.Contains(string(raw), "备份 %%n 每天") {
		t.Fatalf("计划名里的 %% 没有转义：\n%s", raw)
	}
}

// TestRenderRejectsBadID：单元名从 id 拼出来，形态必须先过。
func TestRenderRejectsBadID(t *testing.T) {
	d := validBackup()
	for _, id := range []string{"", "sch_x", "sch_0123456789abcdef0123456789abcdef/../evil", "../sch_0123456789abcdef0123456789abcdef"} {
		if _, err := RenderTimer(id, d); err == nil {
			t.Fatalf("id %q 应当被拒绝", id)
		}
		if _, err := RenderService(id, d); err == nil {
			t.Fatalf("id %q 应当被拒绝", id)
		}
	}
	if !ValidID("sch_0123456789abcdef0123456789abcdef") {
		t.Fatal("合法 id 未被接受")
	}
}

// TestTriggerIDIsStableWithinASlot：同一分钟内重试得到同一个 id，跨分钟才变。
func TestTriggerIDIsStableWithinASlot(t *testing.T) {
	id := "sch_0123456789abcdef0123456789abcdef"
	base := time.Date(2026, 10, 25, 3, 30, 0, 0, time.UTC)
	a := TriggerID(id, base)
	b := TriggerID(id, base.Add(59*time.Second))
	if a != b {
		t.Fatalf("同一分钟内触发 id 应当相同: %s != %s", a, b)
	}
	c := TriggerID(id, base.Add(time.Minute))
	if a == c {
		t.Fatal("跨分钟触发 id 应当不同")
	}
	if !strings.HasPrefix(a, "trg_") || len(a) != 4+32 {
		t.Fatalf("触发 id 形态不对: %q", a)
	}
	if other := TriggerID("sch_fedcba9876543210fedcba9876543210", base); other == a {
		t.Fatal("不同计划的触发 id 不该相同")
	}
	op := OperationID(a)
	if !strings.HasPrefix(op, "op_") || len(op) != 3+32 {
		t.Fatalf("操作 id 形态不对: %q", op)
	}
	if OperationID(a) != op {
		t.Fatal("操作 id 必须由触发 id 稳定派生")
	}
}

// TestActionForBackupUsesCurrentVersion：参数按代理镜像里的版本现算。
func TestActionForBackupUsesCurrentVersion(t *testing.T) {
	req, err := ActionFor(validBackup(), ResolvedTarget{Version: 7})
	if err != nil {
		t.Fatalf("翻译备份计划失败: %v", err)
	}
	if req.Action != "backup.run" {
		t.Fatalf("动作名不对: %q", req.Action)
	}
	var got struct {
		PolicyID string `json:"backup_policy_id"`
		Version  int64  `json:"version"`
	}
	if err := json.Unmarshal(req.Params, &got); err != nil {
		t.Fatalf("参数不是合法 JSON: %v", err)
	}
	if got.PolicyID != "pol_0123456789abcdef0123456789abcdef" || got.Version != 7 {
		t.Fatalf("备份参数不对: %#v", got)
	}
	if _, err := ActionFor(validBackup(), ResolvedTarget{}); err == nil {
		t.Fatal("没有版本时应当拒绝提交")
	}
}

// TestActionForScriptCarriesRuntimeCap：脚本计划把时长上限一起带下去。
func TestActionForScriptCarriesRuntimeCap(t *testing.T) {
	req, err := ActionFor(validScript(), ResolvedTarget{Version: 2})
	if err != nil {
		t.Fatalf("翻译脚本计划失败: %v", err)
	}
	if req.Action != "script.run" {
		t.Fatalf("动作名不对: %q", req.Action)
	}
	var got struct {
		ScriptID string `json:"script_id"`
		Version  int64  `json:"version"`
		Max      int    `json:"max_runtime_seconds"`
	}
	if err := json.Unmarshal(req.Params, &got); err != nil {
		t.Fatalf("参数不是合法 JSON: %v", err)
	}
	if got.ScriptID != "scr_fedcba9876543210fedcba9876543210" || got.Version != 2 || got.Max != 300 {
		t.Fatalf("脚本参数不对: %#v", got)
	}
	if _, err := ActionFor(validScript(), ResolvedTarget{}); err == nil {
		t.Fatal("没有版本时应当拒绝提交")
	}
}

// TestActionForCertificateRenewalCarriesOnlyWhatATimerCanKnow：定时续期的参数里
// 只有域名、ACME 环境，以及"这是定时触发"这个事实。
//
// 三样缺一不可：域名定了续哪张证书；staging 决定 --server，续错环境等于把一个
// 本不该上生产的对象换掉；scheduled 让执行侧不强制重签（每晚 --force-renewal
// 一周就能把 Let's Encrypt 的速率限额烧光）。
func TestActionForCertificateRenewalCarriesOnlyWhatATimerCanKnow(t *testing.T) {
	d := validBackup()
	d.Kind = KindCertificateRenewal
	d.TargetID = "crt_0123456789abcdef0123456789abcdef"
	d.Certificate = &CertificateParams{Domain: "Panel.Example.com", Staging: true}
	d = d.Normalize()
	if err := d.Validate(); err != nil {
		t.Fatalf("证书计划的定义应当合法，得到 %v", err)
	}
	req, err := ActionFor(d, ResolvedTarget{Version: 1})
	if err != nil {
		t.Fatalf("翻译证书续期计划失败: %v", err)
	}
	if req.Action != "certificate.renew" {
		t.Fatalf("动作名不对: %q", req.Action)
	}
	var got map[string]any
	if err := json.Unmarshal(req.Params, &got); err != nil {
		t.Fatalf("参数不是合法 JSON: %v", err)
	}
	if got["domain"] != "panel.example.com" {
		t.Fatalf("domain = %v（应当已经归一化成小写）", got["domain"])
	}
	if got["staging"] != true {
		t.Fatalf("staging = %v", got["staging"])
	}
	if got["scheduled"] != true {
		t.Fatalf("scheduled = %v", got["scheduled"])
	}
	if _, ok := got["certificate_id"]; ok {
		t.Fatal("定时续期不该带证书 id：触发那一刻没有面板库可问")
	}
	if _, ok := got["email"]; ok {
		t.Fatal("定时续期不该带邮箱：ACME 账号在机器上，certbot 自己找")
	}

	// 没有种类参数的定义翻不出动作：那会让 certbot 拿到空域名。
	noParams := d
	noParams.Certificate = nil
	if _, err := ActionFor(noParams, ResolvedTarget{Version: 1}); err == nil {
		t.Fatal("缺证书参数时应当拒绝提交")
	}
}

// TestKeyChangesWithEveryRenderedField：定义指纹覆盖所有会进单元文件的字段。
func TestKeyChangesWithEveryRenderedField(t *testing.T) {
	base := validBackup()
	baseKey, err := base.Key()
	if err != nil {
		t.Fatalf("计算指纹失败: %v", err)
	}
	mutations := map[string]func(*Definition){
		"name":        func(d *Definition) { d.Name = "改名" },
		"calendar":    func(d *Definition) { d.CalendarExpression = "*-*-* 04:30:00" },
		"timezone":    func(d *Definition) { d.Timezone = "UTC" },
		"persistent":  func(d *Definition) { d.Persistent = false },
		"max_runtime": func(d *Definition) { d.MaxRuntimeSeconds = 600 },
		"enabled":     func(d *Definition) { d.Enabled = false },
	}
	for field, mutate := range mutations {
		d := base
		mutate(&d)
		key, err := d.Key()
		if err != nil {
			t.Fatalf("%s：计算指纹失败: %v", field, err)
		}
		if key == baseKey {
			t.Fatalf("%s 改变后指纹没变", field)
		}
	}
}

func validCertificate() Definition {
	return Definition{
		Name:               "每周续期面板证书",
		Kind:               KindCertificateRenewal,
		TargetID:           "crt_0123456789abcdef0123456789abcdef",
		Certificate:        &CertificateParams{Domain: "panel.example.com", Staging: false},
		CalendarExpression: "Mon *-*-* 04:00:00",
		Timezone:           "UTC",
		Persistent:         false,
		MaxRuntimeSeconds:  1800,
		Enabled:            true,
	}
}

// TestValidateAcceptsCertificateRenewalWithParams 是 P12.5 的第一条：证书续期计划的
// 形态本身是对的——但它到底放不放行，取决于动作与账本是否接好（module 那边的 blocker）。
func TestValidateAcceptsCertificateRenewalWithParams(t *testing.T) {
	if err := validCertificate().Validate(); err != nil {
		t.Fatalf("带 domain 与 staging 的证书计划应当合法，得到 %v", err)
	}
}

// TestValidateCertificateRequiresParams：缺参数在保存前就拒，而不是等触发那一夜。
func TestValidateCertificateRequiresParams(t *testing.T) {
	d := validCertificate()
	d.Certificate = nil
	err := d.Validate()
	if err == nil {
		t.Fatal("没有 domain/staging 的证书计划应当被拒绝")
	}
	if ve, ok := err.(*ValidationError); !ok || ve.Field != "certificate" {
		t.Fatalf("错误应指向 certificate 字段，得到 %v", err)
	}
}

// TestValidateCertificateDomainShapes：域名的形态闭集。
func TestValidateCertificateDomainShapes(t *testing.T) {
	rejected := []string{
		"",
		"*.example.com",   // 通配符要 DNS-01
		"example",         // 单标签，ACME 不签
		"exa mple.com",    // 空白
		"-example.com",    // 段首连字符
		"example-.com",    // 段尾连字符
		"example..com",    // 空段
		".example.com",    // 首点
		"example.com.",    // 尾点
		"a_b.example.com", // 下划线不是域名
	}
	for _, domain := range rejected {
		d := validCertificate()
		d.Certificate = &CertificateParams{Domain: domain}
		if err := d.Validate(); err == nil {
			t.Fatalf("域名 %q 应当被拒绝", domain)
		}
	}
	// 一个 63 字节的段是合法的，64 不是。
	ok := strings.Repeat("a", 63) + ".example.com"
	d := validCertificate()
	d.Certificate = &CertificateParams{Domain: ok}
	if err := d.Validate(); err != nil {
		t.Fatalf("63 字节的段应当合法，得到 %v", err)
	}
	long := strings.Repeat("a", 64) + ".example.com"
	d.Certificate = &CertificateParams{Domain: long}
	if err := d.Validate(); err == nil {
		t.Fatal("64 字节的段应当被拒绝")
	}
}

// TestNormalizeLowercasesCertificateDomain：ACME 的域名从来是小写，
// 存大小写两种会让“同一个域名两条计划”这种怪事通过唯一键。
func TestNormalizeLowercasesCertificateDomain(t *testing.T) {
	d := validCertificate()
	d.Certificate = &CertificateParams{Domain: "  Panel.Example.COM ", Staging: true}
	got := d.Normalize()
	if got.Certificate == nil || got.Certificate.Domain != "panel.example.com" {
		t.Fatalf("域名应归一成小写，得到 %+v", got.Certificate)
	}
	if !got.Certificate.Staging {
		t.Fatal("staging 不该在归一化里丢掉")
	}
}

// TestValidateRejectsCertificateParamsOnOtherKinds：给了就用，不然就拒——
// 静默忽略一个用户写下的字段，会让“保存成功、跑的是别人”这种事故查不出来。
func TestValidateRejectsCertificateParamsOnOtherKinds(t *testing.T) {
	for _, d := range []Definition{validBackup(), validScript()} {
		d.Certificate = &CertificateParams{Domain: "panel.example.com"}
		err := d.Validate()
		if err == nil {
			t.Fatalf("%s 带证书参数应当被拒绝", d.Kind)
		}
		if ve, ok := err.(*ValidationError); !ok || ve.Field != "certificate" {
			t.Fatalf("错误应指向 certificate 字段，得到 %v", err)
		}
	}
}

// TestKeyCoversCertificateParams：staging 翻了就是一次真实变更，单元必须重写。
func TestKeyCoversCertificateParams(t *testing.T) {
	a := validCertificate()
	b := validCertificate()
	b.Certificate = &CertificateParams{Domain: "panel.example.com", Staging: true}
	ka, err := a.Key()
	if err != nil {
		t.Fatal(err)
	}
	kb, err := b.Key()
	if err != nil {
		t.Fatal(err)
	}
	if ka == kb {
		t.Fatal("staging 不同，定义指纹不该相同")
	}
}
