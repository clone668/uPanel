package api

// 证书续期计划的参数展开（P12.5）。
//
// 这里钉的是"指向哪一张证书"只有一个来源：定义里的 target_id 与种类参数里的域名
// 一旦对不上，界面说的是 A、凌晨续的是 B——而 systemd 那边没有人能拦。代理判不了
// （证书账本在面板库），所以必须在面板这一层按记录算出来。
//
// 五条：certbot 记录 → 参数被填好；手动导入的证书 → 409；不存在的证书 → 404；
// 调用方给的域名与环境不一致 → 409（不静默覆盖）；非证书计划 → 参数原样不动。

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"upanel/internal/certificates"
	"upanel/internal/schedules"
)

// applyParamsOf 取出代理收到的 preflight 参数并解析成 schedule.apply 的参数。
func applyParamsOf(t *testing.T, agent *stubAgent) schedules.ApplyParams {
	t.Helper()
	if len(agent.calls) != 1 {
		t.Fatalf("代理收到 %d 次预检，期望 1", len(agent.calls))
	}
	var out schedules.ApplyParams
	dec := json.NewDecoder(strings.NewReader(string(agent.calls[0].Params)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		t.Fatalf("展开后的计划参数无法解析: %v（%s）", err, agent.calls[0].Params)
	}
	return out
}

// certRenewalBody 造一份证书续期计划定义（不含种类参数）。
func certRenewalBody(targetID string) string {
	def := map[string]any{
		"name":                "证书续期",
		"kind":                "certificate_renewal",
		"target_id":           targetID,
		"calendar_expression": "*-*-* 03:17:00",
		"timezone":            "UTC",
		"persistent":          true,
		"max_runtime_seconds": 600,
		"enabled":             true,
	}
	raw, _ := json.Marshal(map[string]any{"schedule_id": nil, "version": 0, "definition": def})
	return string(raw)
}

func TestExpandScheduleApplyFillsCertificateFromLedger(t *testing.T) {
	f, agent := newBindFixture(t)
	cert := seedCertificate(t, f, "renew.test", certificates.SourceCertbot,
		time.Now().UTC().Add(60*24*time.Hour), "ops@example.test")

	rec, code := preflightPost(t, f, schedules.ActionApply, certRenewalBody(cert.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（%s）；响应 %s", rec.Code, code, rec.Body.String())
	}
	got := applyParamsOf(t, agent)
	if got.Definition.Certificate == nil {
		t.Fatal("展开后必须带上证书种类参数：触发那一刻没有面板可问")
	}
	if got.Definition.Certificate.Domain != cert.Domain {
		t.Fatalf("域名 = %q，期望 %q", got.Definition.Certificate.Domain, cert.Domain)
	}
	if got.Definition.Certificate.Staging {
		t.Fatal("这条记录不是 staging 签的，展开的 ACME 环境也不该是")
	}
	if got.Definition.TargetID != cert.ID {
		t.Fatalf("target_id 被改动了：%q", got.Definition.TargetID)
	}
}

func TestExpandScheduleApplyRejectsManualCertificate(t *testing.T) {
	f, agent := newBindFixture(t)
	cert := seedCertificate(t, f, "manual.test", certificates.SourceManual,
		time.Now().UTC().Add(60*24*time.Hour), "")

	rec, code := preflightPost(t, f, schedules.ActionApply, certRenewalBody(cert.ID))
	if rec.Code != http.StatusConflict {
		t.Fatalf("状态码 = %d，期望 409（%s）；响应 %s", rec.Code, code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "手动导入") {
		t.Fatalf("响应里应当说清是手动导入的证书：%s", rec.Body.String())
	}
	if len(agent.calls) != 0 {
		t.Fatalf("展开失败时代理收到 %d 次预检，期望 0", len(agent.calls))
	}
}

func TestExpandScheduleApplyRejectsUnknownCertificate(t *testing.T) {
	f, agent := newBindFixture(t)
	rec, code := preflightPost(t, f, schedules.ActionApply,
		certRenewalBody("crt_0123456789abcdef0123456789abcdef"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d，期望 404（%s）；响应 %s", rec.Code, code, rec.Body.String())
	}
	if len(agent.calls) != 0 {
		t.Fatalf("展开失败时代理收到 %d 次预检，期望 0", len(agent.calls))
	}
}

func TestExpandScheduleApplyRejectsMismatchedDomain(t *testing.T) {
	f, agent := newBindFixture(t)
	cert := seedCertificate(t, f, "real.test", certificates.SourceCertbot,
		time.Now().UTC().Add(60*24*time.Hour), "ops@example.test")

	var body map[string]any
	if err := json.Unmarshal([]byte(certRenewalBody(cert.ID)), &body); err != nil {
		t.Fatalf("解析自造参数失败: %v", err)
	}
	body["definition"].(map[string]any)["certificate"] = map[string]any{
		"domain": "other.test", "staging": false,
	}
	raw, _ := json.Marshal(body)

	rec, code := preflightPost(t, f, schedules.ActionApply, string(raw))
	if rec.Code != http.StatusConflict {
		t.Fatalf("状态码 = %d，期望 409（%s）；响应 %s", rec.Code, code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "other.test") {
		t.Fatalf("响应里应当点出对不上的域名：%s", rec.Body.String())
	}
	if len(agent.calls) != 0 {
		t.Fatalf("展开失败时代理收到 %d 次预检，期望 0", len(agent.calls))
	}
}

func TestExpandScheduleApplyLeavesOtherKindsAlone(t *testing.T) {
	f, agent := newBindFixture(t)
	def := map[string]any{
		"name":                "每晚备份",
		"kind":                "backup",
		"target_id":           "pol_0123456789abcdef0123456789abcdef",
		"calendar_expression": "*-*-* 03:17:00",
		"timezone":            "UTC",
		"persistent":          true,
		"max_runtime_seconds": 3600,
		"enabled":             true,
	}
	raw, _ := json.Marshal(map[string]any{"schedule_id": nil, "version": 0, "definition": def})
	rec, code := preflightPost(t, f, schedules.ActionApply, string(raw))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（%s）；响应 %s", rec.Code, code, rec.Body.String())
	}
	got := applyParamsOf(t, agent)
	if got.Definition.Certificate != nil {
		t.Fatalf("备份计划不该被塞进证书参数：%+v", got.Definition.Certificate)
	}
	if got.Definition.Kind != schedules.KindBackup {
		t.Fatalf("种类被改动了：%q", got.Definition.Kind)
	}
}
