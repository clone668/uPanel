package certificates

// 定时续期的参数形状与"不强制重签"（P12.5）。
//
// 定时触发发生在 systemd 里，那一刻没有面板可问：证书 id 与 ACME 邮箱都在面板库，
// 代理手上只有域名。三件事因此必须钉住——
//
//	参数里可以没有 id/邮箱，但填了就必须合法（可空不等于随便填）；
//	没邮箱时不传 --email，让 certbot 用机器上已有的 ACME 账号；
//	定时续期不强制重签，否则每晚一次 --force-renewal 一周就把限额烧光。

import (
	"os"
	"path/filepath"
	"upanel/internal/syscmd"

	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"upanel/internal/proxies"
)

// TestAssessIssueBindsTargetIdentityForScheduledRenewal：定时续期没有证书 id，
// 但判决**必须**给出一个目标身份与状态版本。
//
// 为什么钉这条：预检规则（internal/agent/preflight.go）只对 allowed=true 的计划
// 要求目标绑定——身份指纹与状态版本存在的意义是"执行前复核要动的还是预检时那个
// 东西"。定时续期手上只有域名，于是这里必须回退成域名，否则判决是
// `allowed=true` 却没有绑定，注册表把它当成实现错误回 500 INTERNAL_ERROR：
// systemd 只看见单元失败，管理员拿到一个数字，而这个数字背后的原因是
// "计划里本来就没有证书 id"——正是计划这种形态的正常结果。P12.5 第四轮真机
// 验收的四节（定时续期、快到期续期、失败留原因）全红在这一条上。
//
// 手工那条路不能跟着变弱：有 id 时绑定的仍然是 id，一个计划的结果不能去满足
// 另一条记录的复核。
func TestAssessIssueBindsTargetIdentityForScheduledRenewal(t *testing.T) {
	webroot := t.TempDir()
	ops, err := NewOps(OpsOptions{
		Secrets:          stubSecrets{},
		Runner:           exitRunner{result: syscmd.Result{ExitCode: 0}},
		FS:               SystemMaterialFS(),
		Now:              func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
		CertbotPath:      "/usr/bin/certbot",
		Webroot:          webroot,
		CertbotAvailable: func() (bool, string) { return true, "" },
		HTTPGet: func(_ context.Context, url string) (int, string, error) {
			name := url[strings.LastIndex(url, "/")+1:]
			data, readErr := os.ReadFile(filepath.Join(webroot, acmeChallengeDir, name))
			if readErr != nil {
				return 404, "", readErr
			}
			return 200, string(data), nil
		},
	})
	if err != nil {
		t.Fatalf("构造 Ops 失败：%v", err)
	}

	scheduled := IssueParams{Domain: "panel.example.test", Staging: true, Scheduled: true}
	a, err := ops.AssessIssue(context.Background(), scheduled, ActionRenew)
	if err != nil {
		t.Fatalf("AssessIssue: %v", err)
	}
	if !a.Allowed {
		t.Fatalf("夹具齐备时定时续期应当 allowed，阻塞项：%+v", a.Blockers)
	}
	if a.IdentityFingerprint == "" {
		t.Fatal("定时续期没有绑定目标身份：预检会把它判成实现错误（500），" +
			"而不是一条可执行的计划")
	}
	if a.StateVersion == "" {
		t.Fatal("定时续期没有绑定状态版本")
	}
	if a.IdentityFingerprint == "domain:"+scheduled.Domain {
		// 回退值必须与域名绑定：换一个域名就是另一个目标。
		other := scheduled
		other.Domain = "other.example.test"
		b, oerr := ops.AssessIssue(context.Background(), other, ActionRenew)
		if oerr != nil {
			t.Fatalf("AssessIssue(other): %v", oerr)
		}
		if b.IdentityFingerprint == a.IdentityFingerprint {
			t.Fatalf("两个不同的域名绑成了同一个目标身份：%q", a.IdentityFingerprint)
		}
	}

	const id = "crt_0123456789abcdef0123456789abcdef"
	manual, err := ops.AssessIssue(context.Background(),
		IssueParams{CertificateID: id, Domain: "panel.example.test", Email: "ops@example.test", Staging: true},
		ActionRenew)
	if err != nil {
		t.Fatalf("AssessIssue(manual): %v", err)
	}
	if manual.IdentityFingerprint != id {
		t.Fatalf("手工续期应当绑定证书 id，实际绑的是 %q", manual.IdentityFingerprint)
	}
}

// TestScheduledRenewalParamsMarshalWithoutEmptyIdentityFields：定时续期的参数
// 编码出来不该带着 `"certificate_id":""` 与 `"email":""`。
//
// 面板把代理交上来的定时任务导入任务表时会把参数重新编码一遍，没有 omitempty 的话，
// 计划里本来没有的两个字段会以空值出现在审计行里——真机验收 §5 的
// schedule-a-job-params-no-certificate-id 就是这么红的。字段存在而值为空，和字段不
// 存在，在"这条计划依赖什么"这个问题上不是一个答案。
func TestScheduledRenewalParamsMarshalWithoutEmptyIdentityFields(t *testing.T) {
	raw, err := json.Marshal(IssueParams{Domain: "panel.example.test", Staging: true, Scheduled: true})
	if err != nil {
		t.Fatalf("编码参数失败：%v", err)
	}
	got := string(raw)
	if strings.Contains(got, "certificate_id") {
		t.Fatalf("定时续期的参数里出现了 certificate_id：%s", got)
	}
	if strings.Contains(got, "email") {
		t.Fatalf("定时续期的参数里出现了 email：%s", got)
	}
	for _, want := range []string{`"domain":"panel.example.test"`, `"scheduled":true`, `"staging":true`} {
		if !strings.Contains(got, want) {
			t.Fatalf("参数里少了 %s：%s", want, got)
		}
	}

	// 有 id 与邮箱时照常出现——omitempty 只是"空的不写"，不是"永远不写"。
	raw, err = json.Marshal(IssueParams{
		CertificateID: "crt_0123456789abcdef0123456789abcdef",
		Domain:        "panel.example.test",
		Email:         "ops@example.test",
		Staging:       true,
	})
	if err != nil {
		t.Fatalf("编码参数失败：%v", err)
	}
	got = string(raw)
	for _, want := range []string{`"certificate_id":"crt_0123456789abcdef0123456789abcdef"`, `"email":"ops@example.test"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("手工签发的参数里少了 %s：%s", want, got)
		}
	}
}

func TestNormalizeScheduledRenewalWithoutIDOrEmail(t *testing.T) {
	p, err := IssueParams{Domain: "Panel.Example.com", Staging: true, Scheduled: true}.Normalize()
	if err != nil {
		t.Fatalf("定时续期没有 id 与邮箱也应当通过归一化：%v", err)
	}
	if p.Domain != "panel.example.com" {
		t.Fatalf("域名没有被归一化：%q", p.Domain)
	}
	if p.CertificateID != "" || p.Email != "" {
		t.Fatalf("空字段不应当被猜出值：%+v", p)
	}
}

func TestNormalizeScheduledStillRejectsBadValues(t *testing.T) {
	// "可空"不能变成"随便填"：填了就照常校验，否则这两个字段就成了绕过形态检查的后门。
	cases := []struct {
		name  string
		in    IssueParams
		field string
	}{
		{"id 形态不对", IssueParams{Domain: "panel.example.com", CertificateID: "crt_bad", Scheduled: true}, "certificate_id"},
		{"邮箱形态不对", IssueParams{Domain: "panel.example.com", Email: "not-an-email", Scheduled: true}, "email"},
		{"域名缺失", IssueParams{Scheduled: true}, "domain"},
		{"单标签域名", IssueParams{Domain: "localhost", Scheduled: true}, "domain"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := c.in.Normalize()
			if err == nil {
				t.Fatalf("应当拒绝：%+v", c.in)
			}
			if got := fieldOf(err); got != c.field {
				t.Fatalf("字段 = %q，期望 %q（%T: %s）", got, c.field, err, err.Error())
			}
		})
	}
}

func TestNormalizeManualIssueStillRequiresIDAndEmail(t *testing.T) {
	// 面板上点的签发/续期照旧要 id 与邮箱：那条路上两个值都有，缺了就是参数错了。
	base := IssueParams{Domain: "panel.example.com"}
	if _, err := base.Normalize(); err == nil {
		t.Fatal("没有证书 id 时应当拒绝（非定时路径）")
	}
	withID := base
	withID.CertificateID = "crt_0123456789abcdef0123456789abcdef"
	if _, err := withID.Normalize(); err == nil {
		t.Fatal("没有邮箱时应当拒绝（非定时路径）")
	}
	withID.Email = "ops@example.com"
	if _, err := withID.Normalize(); err != nil {
		t.Fatalf("四件套齐全时应当通过：%v", err)
	}
}

func TestCertbotArgsOmitsEmailWhenUnknown(t *testing.T) {
	_, args := CertbotArgs("/usr/bin/certbot", "/var/www/acme", "panel.example.com", "",
		"https://acme-staging-v02.api.letsencrypt.org/directory", false)
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "--email") {
		t.Fatalf("没有邮箱时不应当出现 --email（传空值 certbot 会直接拒绝）：%v", args)
	}
	if !strings.Contains(joined, "--cert-name panel.example.com") {
		t.Fatalf("--cert-name 必须还在：续的是哪张证书由它决定：%v", args)
	}
	if strings.Contains(joined, "--force-renewal") {
		t.Fatalf("定时续期不该强制重签：%v", args)
	}

	_, withEmail := CertbotArgs("/usr/bin/certbot", "/var/www/acme", "panel.example.com",
		"ops@example.com", "https://acme-v02.api.letsencrypt.org/directory", false)
	if !strings.Contains(strings.Join(withEmail, " "), "--email ops@example.com") {
		t.Fatalf("有邮箱时必须传：%v", withEmail)
	}
}

func TestForceRenewalOnlyOnManualRenew(t *testing.T) {
	cases := []struct {
		name   string
		action string
		params IssueParams
		want   bool
	}{
		{"面板上点续期", ActionRenew, IssueParams{}, true},
		{"定时续期", ActionRenew, IssueParams{Scheduled: true}, false},
		{"签发", ActionIssue, IssueParams{}, false},
		{"定时触发但不是续期（不该出现）", ActionIssue, IssueParams{Scheduled: true}, false},
	}
	for _, c := range cases {
		if got := forceRenewal(c.action, c.params); got != c.want {
			t.Fatalf("%s: forceRenewal = %v，期望 %v", c.name, got, c.want)
		}
	}
}

// fieldOf 取参数错误的字段名。两种来源都要认：证书自己的 ParamError，以及
// 域名归一化借用的 proxies.ValidationError（同一个 Field 语义，前端直接高亮）。
func fieldOf(err error) string {
	var pe *ParamError
	if errors.As(err, &pe) {
		return pe.Field
	}
	var ve *proxies.ValidationError
	if errors.As(err, &ve) {
		return ve.Field
	}
	return ""
}

// TestLedgerAppliesScheduledRenewalByDomain：定时续期没有证书 id，账本按域名对上
// 记录并只更新事实——记录 id 不变（站点绑定与审计跟着走），邮箱保留（定时那次
// 参数里没有它）。
func TestLedgerAppliesScheduledRenewalByDomain(t *testing.T) {
	svc, _ := newTestService(t, Options{})
	ctx := context.Background()
	applier, err := NewJobApplier(svc, quietLogger())
	if err != nil {
		t.Fatalf("构造账本失败: %v", err)
	}

	// 先有一条 certbot 签发的记录（续期之前的那一张）。
	old := newCertFixture(t, "sched.example.test",
		fixedNow().Add(-80*24*time.Hour), fixedNow().Add(10*24*time.Hour), "Old CA")
	rec, err := svc.Create(ctx, CreateInput{
		ID:        "crt_0123456789abcdef0123456789abcdef",
		Domain:    "sched.example.test",
		Info:      old.info,
		Source:    SourceCertbot,
		ACMEEmail: "ops@example.test",
	})
	if err != nil {
		t.Fatalf("建行失败: %v", err)
	}

	// 盘上换了一张新证书；定时续期只报域名与 ACME 环境。
	fresh := newCertFixture(t, "sched.example.test",
		fixedNow().Add(-time.Hour), fixedNow().Add(89*24*time.Hour), "New CA")
	params, _ := json.Marshal(IssueParams{
		Domain:    "sched.example.test",
		Staging:   false,
		Scheduled: true,
	})
	result, _ := json.Marshal(map[string]any{
		"outcome": OutcomeVerified,
		"observed_state": map[string]any{
			"domain":       "sched.example.test",
			"source":       "certbot",
			"issuer":       fresh.info.Issuer,
			"subject":      fresh.info.Subject,
			"domains":      fresh.info.Domains,
			"not_before":   fresh.info.NotBefore.UTC().Format(time.RFC3339),
			"not_after":    fresh.info.NotAfter.UTC().Format(time.RFC3339),
			"fingerprint":  fresh.info.Fingerprint,
			"chain_length": float64(fresh.info.ChainLength),
			"attempted_at": fixedNow().UTC().Format(time.RFC3339),
		},
	})

	if err := applier.Apply(ctx, JobFact{
		ID: "job_sched_1", Action: ActionRenew, Status: "succeeded", Result: result,
	}, params); err != nil {
		t.Fatalf("账本写入失败: %v", err)
	}

	got, err := svc.Get(ctx, rec.ID)
	if err != nil {
		t.Fatalf("按原 id 读记录失败（记录 id 不该变）: %v", err)
	}
	if got.Fingerprint != fresh.info.Fingerprint {
		t.Fatalf("指纹没跟上盘上的新证书：%s", got.Fingerprint)
	}
	if !got.NotAfter.After(old.info.NotAfter) {
		t.Fatalf("到期时间没更新：%v", got.NotAfter)
	}
	if got.ACMEEmail != "ops@example.test" {
		t.Fatalf("邮箱被清空了：%q（定时续期没有邮箱参数，应当保留原来的）", got.ACMEEmail)
	}
	if got.LastError != "" {
		t.Fatalf("last_error 应当清空：%q", got.LastError)
	}

	// 域名在账本里找不到时不建新行：定时续期只按域名对账，凭空建一条没有绑定
	// 关系的记录只会让"这张证书是谁签的"更糊。
	orphanParams, _ := json.Marshal(IssueParams{Domain: "ghost.example.test", Scheduled: true})
	err = applier.Apply(ctx, JobFact{
		ID: "job_sched_2", Action: ActionRenew, Status: "succeeded", Result: result,
	}, orphanParams)
	if err == nil {
		t.Fatal("域名不在账本里时应当报错")
	}
	if !strings.Contains(err.Error(), "ghost.example.test") {
		t.Fatalf("错误里应当带上域名：%s", err.Error())
	}
	if _, gerr := svc.GetByDomain(ctx, "ghost.example.test"); gerr == nil {
		t.Fatal("不该为不存在的域名建记录")
	}
}

// TestExecuteIssueFailureCarriesReasonToJobMessage：定时续期失败时，原因必须走到
// 任务的 error.message。
//
// 为什么钉这条：失败任务的 result 按契约是空对象（internal/agent/runner.go），
// 报告里的 stderr_tail 到不了管理员眼前；而面板账本上的 last_error 抄的正是
// error.message。没有 FailureReason()，一次每晚都在失败的续期在界面上只剩下
// "动作执行失败"——看不出是 80 端口不通、账号失效还是速率限额。P12.5 的真机验收
// 撞到的就是这个：账本想记原因，结果只记下一句空话。
func TestExecuteIssueFailureCarriesReasonToJobMessage(t *testing.T) {
	webroot := t.TempDir()
	const detail = "urn:ietf:params:acme:error:connection :: Fetching " +
		"http://panel.example.test/.well-known/acme-challenge/x: Timeout during connect"
	ops, err := NewOps(OpsOptions{
		Secrets: stubSecrets{},
		Runner: exitRunner{result: syscmd.Result{
			ExitCode: 1,
			Stderr:   "Saving debug log to /var/log/letsencrypt/letsencrypt.log\n" + detail + "\n",
		}},
		FS:               SystemMaterialFS(),
		Now:              func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
		CertbotPath:      "/usr/bin/certbot",
		Webroot:          webroot,
		CertbotAvailable: func() (bool, string) { return true, "" },
		// 探测端读的正是刚写下的那个文件，不做网络请求（与 probe_test 同一手法）。
		HTTPGet: func(_ context.Context, url string) (int, string, error) {
			name := url[strings.LastIndex(url, "/")+1:]
			data, readErr := os.ReadFile(filepath.Join(webroot, acmeChallengeDir, name))
			if readErr != nil {
				return 404, "", readErr
			}
			return 200, string(data), nil
		},
	})
	if err != nil {
		t.Fatalf("构造 Ops 失败：%v", err)
	}

	_, err = ops.ExecuteIssue(context.Background(), proxies.WriteRequest{
		Params: IssueParams{Domain: "panel.example.test", Staging: true, Scheduled: true},
	}, ActionRenew)
	if err == nil {
		t.Fatal("certbot 以非零状态退出时 ExecuteIssue 应当返回错误")
	}
	var reasoned interface{ FailureReason() string }
	if !errors.As(err, &reasoned) {
		t.Fatalf("执行失败要带一句人话（FailureReason）：%v", err)
	}
	reason := reasoned.FailureReason()
	if !strings.Contains(reason, "connection") {
		t.Fatalf("原因里没有 certbot 自己的 detail：%q", reason)
	}
	if strings.Contains(reason, "\n") {
		t.Fatalf("error.message 是单行字段，原因里不该有换行：%q", reason)
	}
	if !errors.Is(err, ErrCertbotFailed) {
		t.Fatalf("错误链上应当仍然认得出 ErrCertbotFailed：%v", err)
	}
}
