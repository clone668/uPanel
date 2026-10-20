package agentstore

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestSecrets 构造一个带真实数据库与真实密钥的秘密存储。
func newTestSecrets(t *testing.T, opts SecretsOptions) *Secrets {
	t.Helper()
	db := openTestDB(t)
	secrets, err := NewSecrets(db, newTestKeyring(t), testLogger(), opts)
	if err != nil {
		t.Fatalf("构造秘密存储失败: %v", err)
	}
	return secrets
}

// rowState 直接读库，返回状态、密文长度、nonce 长度与到期时间（毫秒）。
func rowState(t *testing.T, db *DB, ref string) (state string, ctLen, nonceLen int, expiresAt int64, reference string) {
	t.Helper()
	err := db.store().SQL().QueryRowContext(context.Background(),
		`SELECT state, length(ciphertext), length(nonce), expires_at, reference FROM secrets WHERE id = ?`,
		ref).Scan(&state, &ctLen, &nonceLen, &expiresAt, &reference)
	if err != nil {
		t.Fatalf("读取秘密行失败: %v", err)
	}
	return state, ctLen, nonceLen, expiresAt, reference
}

func TestPutTemporarySecretAndResolve(t *testing.T) {
	secrets := newTestSecrets(t, SecretsOptions{})
	ctx := context.Background()

	secret, err := secrets.Put(ctx, PutInput{Purpose: "smb_password", Value: []byte("Tr0ub4dor&3")})
	if err != nil {
		t.Fatalf("写入秘密失败: %v", err)
	}
	if !ValidSecretRef(secret.Ref) {
		t.Fatalf("返回的引用不合法: %q", secret.Ref)
	}
	if secret.State != StateTemporary {
		t.Fatalf("未带引用时状态应为 %s，实际 %s", StateTemporary, secret.State)
	}
	wantExpiry := secret.CreatedAt.Add(TemporarySecretTTL)
	if !secret.ExpiresAt.Equal(wantExpiry) {
		t.Fatalf("到期时间 = %s，期望 %s", secret.ExpiresAt, wantExpiry)
	}

	state, ctLen, nonceLen, expiresAt, reference := rowState(t, secrets.db, secret.Ref)
	if state != StateTemporary {
		t.Fatalf("库中状态 = %s", state)
	}
	if ctLen < len("Tr0ub4dor&3")+16 {
		t.Fatalf("密文长度 = %d，至少应为明文长度 + 16 字节标签", ctLen)
	}
	if nonceLen != nonceBytes {
		t.Fatalf("nonce 长度 = %d，期望 %d", nonceLen, nonceBytes)
	}
	if expiresAt == 0 {
		t.Fatal("临时秘密必须有到期时间")
	}
	if reference != "" {
		t.Fatalf("临时秘密不应有引用，实际 %q", reference)
	}

	plaintext, err := secrets.Resolve(ctx, secret.Ref)
	if err != nil {
		t.Fatalf("解密失败: %v", err)
	}
	if string(plaintext) != "Tr0ub4dor&3" {
		t.Fatalf("解密结果 = %q", string(plaintext))
	}
	wipe(plaintext)
}

// TestCiphertextNeverLeaksPlaintextToDisk 检查落盘内容里没有明文。
//
// 这不只是单元测试的取巧：验收脚本在虚拟机上对数据库文件做同样的检查，
// 用来证明“密文、不是明文”不是口号。
func TestCiphertextNeverLeaksPlaintextToDisk(t *testing.T) {
	secrets := newTestSecrets(t, SecretsOptions{})
	ctx := context.Background()
	const marker = "PLAINTEXT-MARKER-9f3a"

	if _, err := secrets.Put(ctx, PutInput{Purpose: "app_env", Value: []byte("API_TOKEN=" + marker)}); err != nil {
		t.Fatalf("写入秘密失败: %v", err)
	}

	dir := filepath.Dir(secrets.db.Path())
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("列目录失败: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", e.Name(), err)
		}
		if bytes.Contains(data, []byte(marker)) {
			t.Fatalf("数据库文件 %s 中出现了明文 %q", e.Name(), marker)
		}
	}
}

func TestPutRejectsInvalidInput(t *testing.T) {
	secrets := newTestSecrets(t, SecretsOptions{})
	ctx := context.Background()

	cases := []struct {
		name string
		in   PutInput
		want error
	}{
		{"未登记用途", PutInput{Purpose: "unknown_purpose", Value: []byte("x")}, ErrPurpose},
		{"空用途", PutInput{Purpose: "", Value: []byte("x")}, ErrPurpose},
		{"内部用途", PutInput{Purpose: "totp_seed", Value: []byte("JBSWY3DPEHPK3PXP")}, ErrPurpose},
		{"空明文", PutInput{Purpose: "smb_password", Value: nil}, ErrValueSize},
		{"明文过长", PutInput{Purpose: "smb_password", Value: bytes.Repeat([]byte("a"), 5<<10)}, ErrValueSize},
		{"引用含控制字符", PutInput{Purpose: "smb_password", Value: []byte("x"), Reference: "bad\nref"}, ErrReference},
		{"引用过长", PutInput{Purpose: "smb_password", Value: []byte("x"), Reference: strings.Repeat("r", MaxReferenceLength+1)}, ErrReference},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := secrets.Put(ctx, tc.in); !errors.Is(err, tc.want) {
				t.Fatalf("错误 = %v，期望包装 %v", err, tc.want)
			}
			// 被拒绝的写入不能在库里留下任何行。
			var count int
			if err := secrets.db.store().SQL().QueryRowContext(ctx, `SELECT COUNT(*) FROM secrets`).Scan(&count); err != nil {
				t.Fatalf("统计行数失败: %v", err)
			}
			if count != 0 {
				t.Fatalf("被拒绝的写入留下了 %d 行", count)
			}
		})
	}
}

func TestPutWithReferenceIsPersistent(t *testing.T) {
	secrets := newTestSecrets(t, SecretsOptions{})
	ctx := context.Background()

	secret, err := secrets.Put(ctx, PutInput{
		Purpose:   "backup_repository_password",
		Value:     []byte("repo-pass"),
		Reference: "backup.repository.1",
	})
	if err != nil {
		t.Fatalf("写入秘密失败: %v", err)
	}
	if secret.State != StatePersistent {
		t.Fatalf("带引用时状态应为 %s，实际 %s", StatePersistent, secret.State)
	}
	if !secret.ExpiresAt.IsZero() {
		t.Fatalf("持久秘密不应有到期时间，实际 %s", secret.ExpiresAt)
	}

	state, _, _, expiresAt, reference := rowState(t, secrets.db, secret.Ref)
	if state != StatePersistent || expiresAt != 0 || reference != "backup.repository.1" {
		t.Fatalf("库中记录不符合持久秘密: state=%s expires=%d reference=%q", state, expiresAt, reference)
	}

	// 到期扫描不能碰持久秘密，哪怕时间推得再远。
	removed, err := secrets.DestroyExpired(ctx, time.Now().Add(365*24*time.Hour))
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if removed != 0 {
		t.Fatalf("持久秘密被销毁了 %d 条", removed)
	}
	if _, err := secrets.Resolve(ctx, secret.Ref); err != nil {
		t.Fatalf("持久秘密应仍可解密: %v", err)
	}
}

func TestDestroyExpiredTemporary(t *testing.T) {
	base := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	clock := base
	secrets := newTestSecrets(t, SecretsOptions{
		TTL: 10 * time.Second,
		Now: func() time.Time { return clock },
	})
	ctx := context.Background()

	secret, err := secrets.Put(ctx, PutInput{Purpose: "notification_password", Value: []byte("mail-pass")})
	if err != nil {
		t.Fatalf("写入秘密失败: %v", err)
	}

	// 到期前一秒：不应销毁。
	removed, err := secrets.DestroyExpired(ctx, base.Add(9*time.Second))
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if removed != 0 {
		t.Fatalf("未到期就销毁了 %d 条", removed)
	}

	// 到期时刻：销毁。
	removed, err = secrets.DestroyExpired(ctx, base.Add(10*time.Second))
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if removed != 1 {
		t.Fatalf("销毁条数 = %d，期望 1", removed)
	}

	state, ctLen, nonceLen, _, _ := rowState(t, secrets.db, secret.Ref)
	if state != StateDestroyed {
		t.Fatalf("状态 = %s，期望 %s", state, StateDestroyed)
	}
	if ctLen != 0 || nonceLen != 0 {
		t.Fatalf("销毁后密文与 nonce 必须清空，实际 ciphertext=%d nonce=%d 字节", ctLen, nonceLen)
	}
	if _, err := secrets.Resolve(ctx, secret.Ref); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("销毁后解密应返回 ErrSecretNotFound，实际 %v", err)
	}

	// 再扫一次不应重复计数。
	removed, err = secrets.DestroyExpired(ctx, base.Add(time.Hour))
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if removed != 0 {
		t.Fatalf("重复扫描销毁了 %d 条", removed)
	}
}

func TestResolveExpiredTemporaryBeforeSweep(t *testing.T) {
	base := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	clock := base
	secrets := newTestSecrets(t, SecretsOptions{
		TTL: time.Minute,
		Now: func() time.Time { return clock },
	})
	ctx := context.Background()

	secret, err := secrets.Put(ctx, PutInput{Purpose: "smb_password", Value: []byte("value")})
	if err != nil {
		t.Fatalf("写入秘密失败: %v", err)
	}
	// 还没有被扫描器销毁，但已过期：解密必须拒绝，不能“反正还在就给你”。
	clock = base.Add(2 * time.Minute)
	if _, err := secrets.Resolve(ctx, secret.Ref); !errors.Is(err, ErrSecretExpired) {
		t.Fatalf("过期后解密应返回 ErrSecretExpired，实际 %v", err)
	}
}

func TestReferencePromotesTemporarySecret(t *testing.T) {
	base := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	clock := base
	secrets := newTestSecrets(t, SecretsOptions{
		TTL: time.Minute,
		Now: func() time.Time { return clock },
	})
	ctx := context.Background()

	secret, err := secrets.Put(ctx, PutInput{Purpose: "sftp_private_key", Value: []byte("-----BEGIN KEY-----")})
	if err != nil {
		t.Fatalf("写入秘密失败: %v", err)
	}
	if err := secrets.Reference(ctx, secret.Ref, "backup.repository.2"); err != nil {
		t.Fatalf("转为持久秘密失败: %v", err)
	}

	state, _, _, expiresAt, reference := rowState(t, secrets.db, secret.Ref)
	if state != StatePersistent || expiresAt != 0 || reference != "backup.repository.2" {
		t.Fatalf("转持久后记录不符合预期: state=%s expires=%d reference=%q", state, expiresAt, reference)
	}

	// 即使超过原定存活时间，持久秘密也不能被销毁。
	removed, err := secrets.DestroyExpired(ctx, base.Add(time.Hour))
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if removed != 0 {
		t.Fatalf("持久秘密被销毁了 %d 条", removed)
	}
	plaintext, err := secrets.Resolve(ctx, secret.Ref)
	if err != nil {
		t.Fatalf("转持久后应可解密: %v", err)
	}
	if string(plaintext) != "-----BEGIN KEY-----" {
		t.Fatalf("解密结果 = %q", string(plaintext))
	}
	wipe(plaintext)

	// 同一引用重复绑定是幂等的；改挂到别的引用必须失败。
	if err := secrets.Reference(ctx, secret.Ref, "backup.repository.2"); err != nil {
		t.Fatalf("重复绑定同一引用应当幂等: %v", err)
	}
	if err := secrets.Reference(ctx, secret.Ref, "backup.repository.3"); !errors.Is(err, ErrReference) {
		t.Fatalf("改挂引用应失败，实际 %v", err)
	}
}

func TestReferenceRejectsBadInput(t *testing.T) {
	secrets := newTestSecrets(t, SecretsOptions{})
	ctx := context.Background()

	secret, err := secrets.Put(ctx, PutInput{Purpose: "smb_password", Value: []byte("value")})
	if err != nil {
		t.Fatalf("写入秘密失败: %v", err)
	}
	if err := secrets.Reference(ctx, "sec_ffffffffffffffffffffffffffffffff", "ref"); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("不存在的引用应返回 ErrSecretNotFound，实际 %v", err)
	}
	if err := secrets.Reference(ctx, secret.Ref, ""); !errors.Is(err, ErrReference) {
		t.Fatalf("空引用应返回 ErrReference，实际 %v", err)
	}
	if err := secrets.Reference(ctx, secret.Ref, "bad\nref"); !errors.Is(err, ErrReference) {
		t.Fatalf("非法引用应返回 ErrReference，实际 %v", err)
	}
	if _, err := secrets.Resolve(ctx, "not-a-ref"); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("非法引用解密应返回 ErrSecretNotFound，实际 %v", err)
	}
}

func TestTempSecretsHaveIndependentNonces(t *testing.T) {
	secrets := newTestSecrets(t, SecretsOptions{})
	ctx := context.Background()

	seen := map[string]bool{}
	for i := 0; i < 4; i++ {
		secret, err := secrets.Put(ctx, PutInput{Purpose: "smb_password", Value: []byte("same-value")})
		if err != nil {
			t.Fatalf("写入秘密失败: %v", err)
		}
		var nonce, ciphertext []byte
		if err := secrets.db.store().SQL().QueryRowContext(ctx,
			`SELECT nonce, ciphertext FROM secrets WHERE id = ?`, secret.Ref).Scan(&nonce, &ciphertext); err != nil {
			t.Fatalf("读取 nonce 失败: %v", err)
		}
		if seen[string(nonce)] {
			t.Fatal("出现了重复的 nonce")
		}
		seen[string(nonce)] = true
		if bytes.Contains(ciphertext, []byte("same-value")) {
			t.Fatal("密文中出现了明文")
		}
	}
}

func TestNewSecretsValidatesArguments(t *testing.T) {
	db := openTestDB(t)
	keyring := newTestKeyring(t)
	if _, err := NewSecrets(nil, keyring, testLogger(), SecretsOptions{}); err == nil {
		t.Fatal("缺少数据库必须报错")
	}
	if _, err := NewSecrets(db, nil, testLogger(), SecretsOptions{}); err == nil {
		t.Fatal("缺少密钥必须报错")
	}
	if _, err := NewSecrets(db, keyring, nil, SecretsOptions{}); err == nil {
		t.Fatal("缺少 Logger 必须报错")
	}
	if _, err := NewSecrets(db, keyring, testLogger(), SecretsOptions{TTL: -time.Second}); err == nil {
		t.Fatal("负的存活时间必须报错")
	}
}

func TestOpenRejectsNonRootOwnedDir(t *testing.T) {
	// 只在 Linux 上检查属主：开发宿主没有可移植的语义（见 perms_other.go）。
	if !hasRootOwnershipSemantics() {
		t.Skip("当前平台不检查属主与权限位")
	}
	dir := t.TempDir()
	// 目录对属组/其他用户可写时必须拒绝打开。
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatalf("改权限失败: %v", err)
	}
	if _, err := Open(context.Background(), Options{
		Path:   filepath.Join(dir, "agent.db"),
		Logger: testLogger(),
	}); err == nil {
		t.Fatal("属组/其他用户可写的目录必须被拒绝")
	}
}
