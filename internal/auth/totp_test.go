package auth

import (
	"context"
	"errors"
	"strings"
	"testing"

	"upanel/internal/totp"
)

// testConfirmStep 是"确认用的那个六位数匹配到的时间步"。启用时必须把它一起写进库里，
// 否则确认码在同一个 30 秒窗口里还能再用一次（Q480）。
const testConfirmStep int64 = 100

// TestEnableTotpIssuesRecoveryCodes 启用：十枚一次性码，库里只有哈希，状态行没有种子。
func TestEnableTotpIssuesRecoveryCodes(t *testing.T) {
	svc := testService(t, Options{})
	ctx := context.Background()
	user := mustCreateAdmin(t, svc, "admin")

	codes, err := svc.EnableTotp(ctx, user.ID, "totp-secret-1", testConfirmStep)
	if err != nil {
		t.Fatalf("启用二次验证失败: %v", err)
	}
	if len(codes) != totp.RecoveryCount {
		t.Fatalf("恢复码数量 = %d，期望 %d", len(codes), totp.RecoveryCount)
	}
	seen := map[string]bool{}
	for _, code := range codes {
		if len(code) != 11 || strings.Count(code, "-") != 1 {
			t.Fatalf("恢复码形状不对: %q", code)
		}
		if seen[code] {
			t.Fatalf("恢复码重复: %q", code)
		}
		seen[code] = true
	}

	state, exists, err := svc.TotpState(ctx, user.ID)
	if err != nil {
		t.Fatalf("读取状态失败: %v", err)
	}
	if !exists || !state.Enabled {
		t.Fatalf("启用后应有 enabled 状态行: %+v exists=%v", state, exists)
	}
	if state.SecretRef != "totp-secret-1" {
		t.Fatalf("状态行里的引用 = %q", state.SecretRef)
	}
	if state.LastStep != testConfirmStep {
		t.Fatalf("新启用的 last_step = %d，期望 %d（确认码那一步必须被记下来）",
			state.LastStep, testConfirmStep)
	}

	// 库里必须是哈希：任何一枚明文都不该以任何形式出现在恢复码表里。
	var total, hashed, plain int
	if err := svc.db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(*),
		        SUM(CASE WHEN substr(code_hash, 1, 7) = 'sha256:' THEN 1 ELSE 0 END)
		 FROM user_totp_recovery_codes WHERE user_id = ?`, user.ID).Scan(&total, &hashed); err != nil {
		t.Fatalf("统计恢复码失败: %v", err)
	}
	if total != totp.RecoveryCount || hashed != total {
		t.Fatalf("恢复码入库: 行数=%d 哈希行数=%d", total, hashed)
	}
	for _, code := range codes {
		if err := svc.db.SQL().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM user_totp_recovery_codes WHERE code_hash = ?`, code).
			Scan(&plain); err != nil {
			t.Fatalf("查询明文失败: %v", err)
		}
		if plain != 0 {
			t.Fatalf("恢复码 %q 以明文入库", code)
		}
	}

	// 重复启用被拒（不是"覆盖"）。
	if _, err := svc.EnableTotp(ctx, user.ID, "totp-secret-2", testConfirmStep); !errors.Is(err, ErrTotpAlreadyEnrolled) {
		t.Fatalf("重复启用应返回 ErrTotpAlreadyEnrolled，实际 %v", err)
	}
	if err := svc.db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM user_totp_recovery_codes WHERE user_id = ?`, user.ID).Scan(&total); err != nil {
		t.Fatalf("统计恢复码失败: %v", err)
	}
	if total != totp.RecoveryCount {
		t.Fatalf("重复启用改动了恢复码，实际 %d 行", total)
	}

	// 没有引用就不该启用：那会留下一条"启用了但不知道种子在哪"的状态。
	if _, err := svc.EnableTotp(ctx, mustCreateAdmin(t, svc, "other").ID, "", testConfirmStep); err == nil {
		t.Fatalf("空 secret_ref 必须被拒")
	}
}

// TestAcceptTotpStepRejectsReplay 时间步只许前进：同一个步、更早的步都被拒。
func TestAcceptTotpStepRejectsReplay(t *testing.T) {
	svc := testService(t, Options{})
	ctx := context.Background()
	user := mustCreateAdmin(t, svc, "admin")
	// 确认那一步（testConfirmStep-1）已经被用掉，所以这里从 testConfirmStep 起算。
	if _, err := svc.EnableTotp(ctx, user.ID, "totp-secret-1", testConfirmStep-1); err != nil {
		t.Fatalf("启用失败: %v", err)
	}

	if err := svc.AcceptTotpStep(ctx, user.ID, 100); err != nil {
		t.Fatalf("第一次接受时间步失败: %v", err)
	}
	if err := svc.AcceptTotpStep(ctx, user.ID, 100); !errors.Is(err, ErrTotpReplay) {
		t.Fatalf("同一步应被拒，实际 %v", err)
	}
	if err := svc.AcceptTotpStep(ctx, user.ID, 99); !errors.Is(err, ErrTotpReplay) {
		t.Fatalf("更早的步应被拒，实际 %v", err)
	}
	if err := svc.AcceptTotpStep(ctx, user.ID, 101); err != nil {
		t.Fatalf("下一步应被接受: %v", err)
	}

	state, _, err := svc.TotpState(ctx, user.ID)
	if err != nil {
		t.Fatalf("读取状态失败: %v", err)
	}
	if state.LastStep != 101 {
		t.Fatalf("last_step = %d，期望 101", state.LastStep)
	}

	// 没启用的账号：一条都不可能被接受（不会顺手建行）。
	other := mustCreateAdmin(t, svc, "other")
	if err := svc.AcceptTotpStep(ctx, other.ID, 100); !errors.Is(err, ErrTotpReplay) {
		t.Fatalf("未启用账号应被拒，实际 %v", err)
	}
	if _, exists, err := svc.TotpState(ctx, other.ID); err != nil || exists {
		t.Fatalf("未启用账号不该被建出状态行: exists=%v err=%v", exists, err)
	}
}

// TestConsumeRecoveryCodeIsOneShot 恢复码只能用一次，用掉之后数量递减。
func TestConsumeRecoveryCodeIsOneShot(t *testing.T) {
	svc := testService(t, Options{})
	ctx := context.Background()
	user := mustCreateAdmin(t, svc, "admin")
	codes, err := svc.EnableTotp(ctx, user.ID, "totp-secret-1", testConfirmStep)
	if err != nil {
		t.Fatalf("启用失败: %v", err)
	}

	remaining, err := svc.ConsumeRecoveryCode(ctx, user.ID, codes[0])
	if err != nil {
		t.Fatalf("使用恢复码失败: %v", err)
	}
	if remaining != totp.RecoveryCount-1 {
		t.Fatalf("剩余恢复码 = %d，期望 %d", remaining, totp.RecoveryCount-1)
	}
	if _, err := svc.ConsumeRecoveryCode(ctx, user.ID, codes[0]); !errors.Is(err, ErrRecoveryCodeInvalid) {
		t.Fatalf("同一枚恢复码第二次应无效，实际 %v", err)
	}
	if left, err := svc.RecoveryCodesRemaining(ctx, user.ID); err != nil || left != totp.RecoveryCount-1 {
		t.Fatalf("剩余枚数 = %d (err=%v)，期望 %d", left, err, totp.RecoveryCount-1)
	}

	// 大小写与连字符不影响：用户抄下来的样子不一定是屏幕上那个样子。
	sloppy := strings.ToLower(strings.ReplaceAll(codes[1], "-", " "))
	remaining, err = svc.ConsumeRecoveryCode(ctx, user.ID, sloppy)
	if err != nil {
		t.Fatalf("宽松形式的恢复码应被接受: %v", err)
	}
	if remaining != totp.RecoveryCount-2 {
		t.Fatalf("剩余恢复码 = %d，期望 %d", remaining, totp.RecoveryCount-2)
	}

	// 垃圾输入与别人的码都算无效，且不改变计数。
	for _, bad := range []string{"", "not-a-code", "0000000000", codes[2][:5]} {
		if _, err := svc.ConsumeRecoveryCode(ctx, user.ID, bad); !errors.Is(err, ErrRecoveryCodeInvalid) {
			t.Fatalf("无效恢复码 %q 应返回 ErrRecoveryCodeInvalid，实际 %v", bad, err)
		}
	}
	stranger := mustCreateAdmin(t, svc, "stranger")
	if _, err := svc.ConsumeRecoveryCode(ctx, stranger.ID, codes[2]); !errors.Is(err, ErrRecoveryCodeInvalid) {
		t.Fatalf("别人的恢复码应无效，实际 %v", err)
	}
	if left, err := svc.RecoveryCodesRemaining(ctx, user.ID); err != nil || left != totp.RecoveryCount-2 {
		t.Fatalf("无效尝试不该改变剩余枚数: %d (err=%v)", left, err)
	}
}

// TestDisableTotpClearsState 关闭要连状态行带恢复码一起清掉，且第二次关闭是 ErrTotpNotEnrolled。
func TestDisableTotpClearsState(t *testing.T) {
	svc := testService(t, Options{})
	ctx := context.Background()
	user := mustCreateAdmin(t, svc, "admin")
	if _, err := svc.EnableTotp(ctx, user.ID, "totp-secret-1", testConfirmStep); err != nil {
		t.Fatalf("启用失败: %v", err)
	}

	if err := svc.DisableTotp(ctx, user.ID); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}
	if _, exists, err := svc.TotpState(ctx, user.ID); err != nil || exists {
		t.Fatalf("关闭后不该留着状态行: exists=%v err=%v", exists, err)
	}
	var rows int
	if err := svc.db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM user_totp_recovery_codes WHERE user_id = ?`, user.ID).Scan(&rows); err != nil {
		t.Fatalf("统计恢复码失败: %v", err)
	}
	if rows != 0 {
		t.Fatalf("关闭后恢复码应级联删除，实际 %d 行", rows)
	}
	if err := svc.DisableTotp(ctx, user.ID); !errors.Is(err, ErrTotpNotEnrolled) {
		t.Fatalf("重复关闭应返回 ErrTotpNotEnrolled，实际 %v", err)
	}

	// 关闭之后可以重新登记，重新登记会得到新的一套码（旧的已随行删除）。
	codes, err := svc.EnableTotp(ctx, user.ID, "totp-secret-2", testConfirmStep)
	if err != nil {
		t.Fatalf("重新启用失败: %v", err)
	}
	if left, err := svc.RecoveryCodesRemaining(ctx, user.ID); err != nil || left != len(codes) {
		t.Fatalf("重新启用后剩余枚数 = %d (err=%v)，期望 %d", left, err, len(codes))
	}
}

// TestEnableTotpRecordsConfirmStep 确认用的那个码不能立刻再用来登录。
//
// 真机第一轮在这里读到 `last_step = -1`：代理明明验过确认码，面板这边却把"上一步"留成
// "什么都没用过"，于是同一个六位数在同一个 30 秒窗口里还能再登一次（Q480）。这条用例
// 钉的就是那个洞——它同时断言"确认那一步算已用过"和"下一个时间步仍然放行"，免得把
// 重放防护修成"连下一步都不认"。
func TestEnableTotpRecordsConfirmStep(t *testing.T) {
	svc := testService(t, Options{})
	ctx := context.Background()
	user := mustCreateAdmin(t, svc, "admin")

	const confirmStep = int64(58_000_000)
	if _, err := svc.EnableTotp(ctx, user.ID, "totp-secret-1", confirmStep); err != nil {
		t.Fatalf("启用失败: %v", err)
	}
	state, exists, err := svc.TotpState(ctx, user.ID)
	if err != nil || !exists {
		t.Fatalf("启用后应有状态行: exists=%v err=%v", exists, err)
	}
	if state.LastStep != confirmStep {
		t.Fatalf("last_step = %d，期望 %d", state.LastStep, confirmStep)
	}
	if err := svc.AcceptTotpStep(ctx, user.ID, confirmStep); !errors.Is(err, ErrTotpReplay) {
		t.Fatalf("确认用的那一步应算已用过，实际 %v", err)
	}
	if err := svc.AcceptTotpStep(ctx, user.ID, confirmStep+1); err != nil {
		t.Fatalf("下一个时间步应被接受: %v", err)
	}

	// 没有时间步就不启用：宁可让用户重扫一次，也不留一行"第一个码能反复用"的状态。
	other := mustCreateAdmin(t, svc, "other")
	if _, err := svc.EnableTotp(ctx, other.ID, "totp-secret-9", -1); err == nil {
		t.Fatalf("缺时间步的启用必须被拒")
	}
	if _, exists, err := svc.TotpState(ctx, other.ID); err != nil || exists {
		t.Fatalf("被拒的启用不该留下状态行: exists=%v err=%v", exists, err)
	}
}
