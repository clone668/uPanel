package agent

// 计划任务与证书动作之间的接缝（P12.5）。
//
// 两件事只能在这里钉：
//
//	名字    internal/schedules 不能 import internal/certificates（会成环：
//	         certificates → proxies → agentclient → schedules），所以
//	         schedules.ActionCertificateRenew 是字面量。改一边不改另一边，
//	         后果是每晚安静地"计划指向的动作当前不可用"。
//	参数    计划翻译出来的那三样（domain / staging / scheduled）必须能被登记表里
//	         那个动作的 Decode 吃下去。参数形状一旦对不上，失败出现在凌晨的触发里，
//	         而不是在这里。

import (
	"encoding/json"
	"testing"

	"upanel/internal/certificates"
	"upanel/internal/schedules"
)

func TestScheduleCertificateActionNameMatchesRegistry(t *testing.T) {
	if schedules.ActionCertificateRenew != certificates.ActionRenew {
		t.Fatalf("计划侧动作名 %q 与证书登记的动作名 %q 不一致",
			schedules.ActionCertificateRenew, certificates.ActionRenew)
	}
}

func TestRegisteredRenewActionAcceptsScheduledParams(t *testing.T) {
	// 用 ScheduleDefinition 真翻一次，再把结果交给动作的 Decode：这是触发那一刻
	// 实际走的两步，中间没有别的翻译层。
	def := schedules.Definition{
		Name:               "证书续期",
		Kind:               schedules.KindCertificateRenewal,
		TargetID:           "crt_0123456789abcdef0123456789abcdef",
		Certificate:        &schedules.CertificateParams{Domain: "Panel.Example.com", Staging: true},
		CalendarExpression: "*-*-* 03:17:00",
		Timezone:           "UTC",
		Persistent:         true,
		MaxRuntimeSeconds:  600,
		Enabled:            true,
	}
	def = def.Normalize()
	req, err := schedules.ActionFor(def, schedules.ResolvedTarget{Version: 1})
	if err != nil {
		t.Fatalf("ActionFor: %v", err)
	}
	act := newCertificateIssueAction(nil, certificates.ActionRenew)
	params, err := act.Decode(req.Params)
	if err != nil {
		t.Fatalf("登记表拒绝了解析这些参数（%s）：%v", string(req.Params), err)
	}
	p, ok := params.(certificates.IssueParams)
	if !ok {
		t.Fatalf("Decode 交回来的类型是 %T", params)
	}
	if p.Domain != "panel.example.com" || !p.Staging || !p.Scheduled {
		t.Fatalf("参数丢了东西：%+v", p)
	}
	if p.CertificateID != "" || p.Email != "" {
		t.Fatalf("定时续期不该凭空多出 id 或邮箱：%+v", p)
	}

	// 面板上点的那条路还是老形状：有 id 有邮箱、没有 scheduled。
	manual, err := act.Decode(json.RawMessage(`{"certificate_id":"crt_0123456789abcdef0123456789abcdef",
		"domain":"panel.example.com","email":"ops@example.com","staging":false}`))
	if err != nil {
		t.Fatalf("手工签发的参数应当仍然通过：%v", err)
	}
	if mp, ok := manual.(certificates.IssueParams); !ok || mp.Scheduled {
		t.Fatalf("手工参数被当成了定时触发：%+v", manual)
	}

	// 少了 domain 的定时参数必须被拒：certbot 拿到空域名只会失败一次。
	if _, err := act.Decode(json.RawMessage(`{"staging":true,"scheduled":true}`)); err == nil {
		t.Fatal("没有域名的定时续期参数应当被拒绝")
	}
}
