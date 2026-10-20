package agentstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"upanel/migrations/agentdb"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

// openTestDB 打开一个真实的临时代理数据库并完成迁移。
func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(context.Background(), Options{
		Path:   filepath.Join(t.TempDir(), "agent.db"),
		Logger: testLogger(),
	})
	if err != nil {
		t.Fatalf("打开代理数据库失败: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("关闭代理数据库失败: %v", err)
		}
	})
	if _, err := db.Migrate(context.Background(), agentdb.FS); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	return db
}

// newTestKeyring 构造一把随机的真实密钥（不涉及文件权限，权限在 Linux 上单独验证）。
func newTestKeyring(t *testing.T) *Keyring {
	t.Helper()
	key := make([]byte, SecretKeyBytes)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("生成密钥失败: %v", err)
	}
	keyring, err := NewKeyring(key)
	if err != nil {
		t.Fatalf("构造 Keyring 失败: %v", err)
	}
	return keyring
}

func TestGenerateKeyFileThenLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys", "agent.key")
	fingerprint, err := GenerateKeyFile(path)
	if err != nil {
		t.Fatalf("生成密钥失败: %v", err)
	}
	if len(fingerprint) != 16 {
		t.Fatalf("指纹长度 = %d，期望 16（8 字节十六进制）", len(fingerprint))
	}

	keyring, err := LoadKeyring(path)
	if err != nil {
		t.Fatalf("加载密钥失败: %v", err)
	}
	if keyring.Fingerprint() != fingerprint {
		t.Fatalf("加载后的指纹 %q 与生成时 %q 不一致", keyring.Fingerprint(), fingerprint)
	}
	if keyring.Version() != KeyVersion {
		t.Fatalf("密钥版本 = %d，期望 %d", keyring.Version(), KeyVersion)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取密钥文件失败: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) != 2 || lines[0] != SecretKeyHeader {
		t.Fatalf("密钥文件格式不正确: %q", string(raw))
	}
	decoded, err := base64.StdEncoding.DecodeString(lines[1])
	if err != nil {
		t.Fatalf("密钥行不是合法 base64: %v", err)
	}
	if len(decoded) != SecretKeyBytes {
		t.Fatalf("密钥长度 = %d，期望 %d", len(decoded), SecretKeyBytes)
	}
}

func TestGenerateKeyFileNeverOverwrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.key")
	first, err := GenerateKeyFile(path)
	if err != nil {
		t.Fatalf("首次生成失败: %v", err)
	}
	if _, err := GenerateKeyFile(path); err == nil {
		t.Fatal("已存在的密钥必须拒绝覆盖（覆盖会让已存密文永久不可解）")
	} else if !errors.Is(err, ErrKeyFile) {
		t.Fatalf("错误应包装 ErrKeyFile，实际: %v", err)
	}
	keyring, err := LoadKeyring(path)
	if err != nil {
		t.Fatalf("加载密钥失败: %v", err)
	}
	if keyring.Fingerprint() != first {
		t.Fatal("拒绝覆盖后密钥指纹不应变化")
	}
}

func TestLoadKeyringRejectsMalformedFiles(t *testing.T) {
	validKey := make([]byte, SecretKeyBytes)
	if _, err := rand.Read(validKey); err != nil {
		t.Fatalf("生成随机密钥失败: %v", err)
	}
	goodLine := base64.StdEncoding.EncodeToString(validKey)

	cases := []struct {
		name    string
		content string
	}{
		{"标识行错误", "upanel/wrong-header/v1\n" + goodLine + "\n"},
		{"密钥不是 base64", SecretKeyHeader + "\n***\n"},
		{"密钥长度不对", SecretKeyHeader + "\n" + base64.StdEncoding.EncodeToString(validKey[:16]) + "\n"},
		{"多余的行", SecretKeyHeader + "\n" + goodLine + "\n" + goodLine + "\n"},
		{"只有标识行", SecretKeyHeader + "\n"},
		{"空文件", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "agent.key")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatalf("写文件失败: %v", err)
			}
			if _, err := LoadKeyring(path); err == nil {
				t.Fatalf("非法密钥文件必须被拒绝: %q", tc.content)
			} else if !errors.Is(err, ErrKeyFile) {
				t.Fatalf("错误应包装 ErrKeyFile，实际: %v", err)
			}
		})
	}
}

func TestLoadKeyringMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.key")
	_, err := LoadKeyring(path)
	if err == nil {
		t.Fatal("密钥文件不存在必须报错")
	}
	if !errors.Is(err, ErrKeyFile) {
		t.Fatalf("错误应包装 ErrKeyFile，实际: %v", err)
	}
	if !strings.Contains(err.Error(), "-init-key") {
		t.Fatalf("错误信息应指出初始化方式，实际: %v", err)
	}
}

func TestSealOpenRoundTripAndNonceUniqueness(t *testing.T) {
	keyring := newTestKeyring(t)
	plaintext := []byte("s3cr3t-value-口令")

	nonce1, ct1, err := keyring.Seal(plaintext, "sec_0123456789abcdef0123456789abcdef", "smb_password")
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	nonce2, ct2, err := keyring.Seal(plaintext, "sec_0123456789abcdef0123456789abcdef", "smb_password")
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	if bytes.Equal(nonce1, nonce2) {
		t.Fatal("两次加密必须使用不同的随机 nonce")
	}
	if bytes.Equal(ct1, ct2) {
		t.Fatal("相同明文配不同 nonce，密文不应相同")
	}
	if len(nonce1) != nonceBytes {
		t.Fatalf("nonce 长度 = %d，期望 %d", len(nonce1), nonceBytes)
	}
	if bytes.Contains(ct1, plaintext) {
		t.Fatal("密文中出现了明文")
	}

	got, err := keyring.Open(nonce1, ct1, "sec_0123456789abcdef0123456789abcdef", "smb_password")
	if err != nil {
		t.Fatalf("解密失败: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("解密结果 = %q，期望 %q", got, plaintext)
	}
}

func TestOpenRejectsWrongAADAndTampering(t *testing.T) {
	keyring := newTestKeyring(t)
	ref := "sec_0123456789abcdef0123456789abcdef"
	nonce, ct, err := keyring.Seal([]byte("value"), ref, "smb_password")
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}

	if _, err := keyring.Open(nonce, ct, ref, "sftp_private_key"); err == nil {
		t.Fatal("用途不同的 AAD 必须解密失败（防止把密文搬到别的用途上）")
	}
	if _, err := keyring.Open(nonce, ct, "sec_ffffffffffffffffffffffffffffffff", "smb_password"); err == nil {
		t.Fatal("引用不同的 AAD 必须解密失败")
	}

	tampered := append([]byte(nil), ct...)
	tampered[0] ^= 0xff
	if _, err := keyring.Open(nonce, tampered, ref, "smb_password"); err == nil {
		t.Fatal("被改动的密文必须解密失败")
	}
	if _, err := keyring.Open(nonce, ct[:len(ct)-1], ref, "smb_password"); err == nil {
		t.Fatal("被截断的密文必须解密失败")
	}
	if _, err := keyring.Open(nonce[:8], ct, ref, "smb_password"); err == nil {
		t.Fatal("nonce 长度不对必须解密失败")
	}

	other := newTestKeyring(t)
	if _, err := other.Open(nonce, ct, ref, "smb_password"); err == nil {
		t.Fatal("换一把密钥必须解密失败")
	}
}

func TestNewKeyringRejectsWrongKeySize(t *testing.T) {
	for _, size := range []int{0, 1, SecretKeyBytes - 1, SecretKeyBytes + 1} {
		if _, err := NewKeyring(make([]byte, size)); err == nil {
			t.Fatalf("%d 字节的密钥必须被拒绝", size)
		}
	}
}

func TestSecretRefFormat(t *testing.T) {
	ref, err := newSecretRef()
	if err != nil {
		t.Fatalf("生成引用失败: %v", err)
	}
	if !ValidSecretRef(ref) {
		t.Fatalf("生成的引用 %q 未通过自校验", ref)
	}
	if len(ref) != 36 {
		t.Fatalf("引用长度 = %d，期望 36", len(ref))
	}
	// 引用是随机生成的：连续两次不应相同。
	other, err := newSecretRef()
	if err != nil {
		t.Fatalf("生成引用失败: %v", err)
	}
	if ref == other {
		t.Fatal("两次生成的引用不应相同")
	}

	for _, bad := range []string{"", "sec_", "sec_0123", "sec_" + strings.Repeat("G", 32), "SEC_" + strings.Repeat("0", 32)} {
		if ValidSecretRef(bad) {
			t.Fatalf("非法引用 %q 不应通过校验", bad)
		}
	}
}
