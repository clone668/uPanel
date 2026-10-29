package main

// totp-reset 是本机管理员在"丢了 authenticator、也丢了恢复码"时唯一的出路。
// 这些用例要证明五件事：状态真的清了、恢复码一起清了、既有会话全部作废、
// 代理侧结果如实上报（不假装清干净）、审计留痕且 actor 不是某个账号。

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"upanel/internal/audit"
	"upanel/internal/auth"
	"upanel/internal/store"
)

func TestTotpResetClearsStateAndRevokesSessions(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "upanel.db")
	const password = "correct-horse-9"

	withStdin(t, password+"\n"+password+"\n")
	if code, _, errOut := runCtl("init-admin", "--db", dbPath, "--username", "admin"); code != 0 {
		t.Fatalf("init-admin 退出码 = %d，stderr = %q", code, errOut)
	}

	ctx := context.Background()
	db, err := store.Open(ctx, store.Options{Path: dbPath, Logger: quietTestLogger()})
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	defer db.Close()
	svc, err := auth.NewService(db, auth.Options{})
	if err != nil {
		t.Fatalf("初始化认证服务失败: %v", err)
	}
	user, err := svc.FindUserByUsername(ctx, "admin")
	if err != nil {
		t.Fatalf("查询账号失败: %v", err)
	}
	codes, err := svc.EnableTotp(ctx, user.ID, "sec_totp_admin", 100)
	if err != nil {
		t.Fatalf("启用二次验证失败: %v", err)
	}
	issued, err := svc.Login(ctx, auth.LoginInput{Username: "admin", Password: password})
	if err != nil {
		t.Fatalf("登录失败: %v", err)
	}

	code, out, errOut := runCtl("totp-reset", "--db", dbPath, "--username", "admin", "--no-agent", "--json")
	if code != 0 {
		t.Fatalf("totp-reset 退出码 = %d，stderr = %q", code, errOut)
	}
	var report totpResetReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("输出不是合法 JSON: %v (%q)", err, out)
	}
	if !report.StateCleared || report.Username != "admin" || report.Role != string(auth.RoleAdmin) {
		t.Fatalf("报告 = %+v", report)
	}
	if report.RecoveryCodes != len(codes) {
		t.Fatalf("报告的恢复码枚数 = %d，期望 %d", report.RecoveryCodes, len(codes))
	}
	if report.AgentStatus != "skipped" {
		t.Fatalf("--no-agent 时 agent_status = %q，期望 skipped", report.AgentStatus)
	}
	if report.RevokedSessions < 1 {
		t.Fatalf("作废会话数 = %d，期望至少 1", report.RevokedSessions)
	}
	// 输出里不许出现任何秘密材料：恢复码、口令、种子都不行。
	for _, secret := range append([]string{password}, codes...) {
		if strings.Contains(out, secret) || strings.Contains(errOut, secret) {
			t.Fatalf("输出里出现了秘密材料")
		}
	}

	// 状态行与恢复码都没了，且会话已作废。
	if _, exists, err := svc.TotpState(ctx, user.ID); err != nil || exists {
		t.Fatalf("清除后不该留着状态行: exists=%v err=%v", exists, err)
	}
	var rows int
	if err := db.SQL().QueryRow(
		`SELECT COUNT(*) FROM user_totp_recovery_codes WHERE user_id = ?`, user.ID).Scan(&rows); err != nil {
		t.Fatalf("统计恢复码失败: %v", err)
	}
	if rows != 0 {
		t.Fatalf("清除后恢复码应级联删除，实际 %d 行", rows)
	}
	if _, err := svc.Authenticate(ctx, issued.Token); err == nil {
		t.Fatal("清除二次验证后，旧会话仍然有效")
	}

	// 审计：一条 auth.totp.reset，actor 是本地 CLI（actor_id 为 NULL）。
	var (
		action  string
		actorID *int64
	)
	if err := db.SQL().QueryRow(
		`SELECT action, actor_id FROM audit_events WHERE action = ? ORDER BY id DESC LIMIT 1`,
		audit.ActionTotpReset).Scan(&action, &actorID); err != nil {
		t.Fatalf("读取审计失败: %v", err)
	}
	if actorID != nil {
		t.Fatalf("本地 CLI 的审计 actor_id 应为 NULL，实际 %d", *actorID)
	}
}

func TestTotpResetTextOutputWarnsWhenAgentNotDestroyed(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "upanel.db")
	const password = "correct-horse-9"
	withStdin(t, password+"\n"+password+"\n")
	if code, _, errOut := runCtl("init-admin", "--db", dbPath, "--username", "admin"); code != 0 {
		t.Fatalf("init-admin 退出码 = %d，stderr = %q", code, errOut)
	}

	ctx := context.Background()
	db, err := store.Open(ctx, store.Options{Path: dbPath, Logger: quietTestLogger()})
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	defer db.Close()
	svc, err := auth.NewService(db, auth.Options{})
	if err != nil {
		t.Fatalf("初始化认证服务失败: %v", err)
	}
	user, err := svc.FindUserByUsername(ctx, "admin")
	if err != nil {
		t.Fatalf("查询账号失败: %v", err)
	}
	if _, err := svc.EnableTotp(ctx, user.ID, "sec_totp_admin", 100); err != nil {
		t.Fatalf("启用二次验证失败: %v", err)
	}

	// 不传 --no-agent：宿主上 /run/upanel/agent.sock 不存在（或对端身份不被接受），
	// 两种情况下恢复都必须成功，但输出必须如实说明代理侧没销毁。
	code, out, _ := runCtl("totp-reset", "--db", dbPath, "--username", "admin", "--json")
	if code != 0 {
		t.Fatalf("代理不可达时 totp-reset 也必须成功（恢复不能被代理拖住），退出码 = %d，输出 %q", code, out)
	}
	var report totpResetReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("输出不是合法 JSON: %v (%q)", err, out)
	}
	if !report.StateCleared {
		t.Fatalf("报告 = %+v", report)
	}
	switch report.AgentStatus {
	case "destroyed", "already_gone":
		// 这台宿主机上真有一个可用的代理：那也算正常结果。
	case "unreachable", "forbidden", "error":
		if report.AgentDetail == "" {
			t.Fatalf("代理没销毁时必须给出原因: %+v", report)
		}
	default:
		t.Fatalf("agent_status = %q 不在闭集里", report.AgentStatus)
	}
}

func TestTotpResetRejectsNothingToReset(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "upanel.db")
	const password = "correct-horse-9"
	withStdin(t, password+"\n"+password+"\n")
	if code, _, errOut := runCtl("init-admin", "--db", dbPath, "--username", "admin"); code != 0 {
		t.Fatalf("init-admin 退出码 = %d，stderr = %q", code, errOut)
	}

	// 没启用二次验证：明确说"没有可恢复的东西"，不假装成功。
	code, _, errOut := runCtl("totp-reset", "--db", dbPath, "--username", "admin", "--no-agent")
	if code == 0 {
		t.Fatal("没有启用二次验证时 totp-reset 必须失败")
	}
	if !strings.Contains(errOut, "没有启用二次验证") {
		t.Fatalf("stderr = %q", errOut)
	}

	// 不存在的账号：不能凭空造。
	code, _, errOut = runCtl("totp-reset", "--db", dbPath, "--username", "ghost", "--no-agent")
	if code == 0 {
		t.Fatal("对不存在的账号 totp-reset 必须失败")
	}
	if !strings.Contains(errOut, "没有账号") {
		t.Fatalf("stderr = %q", errOut)
	}

	// 非法用户名：用法错误。
	if code, _, _ := runCtl("totp-reset", "--db", dbPath, "--username", "bad name!"); code != exitUsage {
		t.Fatalf("非法用户名退出码 = %d，期望 %d", code, exitUsage)
	}
}
