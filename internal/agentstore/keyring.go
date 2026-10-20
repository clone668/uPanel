package agentstore

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// 密钥文件格式与 AEAD 参数。
const (
	// SecretKeyHeader 是本机密钥文件的首行标识。带标识是为了让“备份错文件”“截断文件”
	// 这类事故在加载时就报错，而不是被当成密钥用下去。
	SecretKeyHeader = "upanel/agent-secret-key/v1"
	// SecretKeyBytes 是 AES-256 的密钥长度。
	SecretKeyBytes = 32
	// KeyVersion 是当前密钥版本，随密文一起存库，供将来轮换时区分。
	KeyVersion = 1
	// nonceBytes 是 AES-GCM 的 standard nonce 长度：每条秘密独立随机生成，绝不复用。
	nonceBytes = 12
	// secretKeyMode 是密钥文件权限：root:root 0600（CONTRACTS.md 14.2）。
	secretKeyMode os.FileMode = 0o600
	// secretKeyDirMode 是密钥目录在“由代理创建”时的权限（不给属组/其他用户写）。
	secretKeyDirMode os.FileMode = 0o755
)

// ErrKeyFile 表示本机密钥文件缺失、格式错误或权限不合规。
var ErrKeyFile = errors.New("agentstore: 本机秘密密钥不可用")

// Keyring 是秘密加密用的 AEAD。零值不可用，必须用 LoadKeyring / NewKeyring 构造。
type Keyring struct {
	aead cipher.AEAD
	// fingerprint 是密钥的 SHA-256 前 8 字节，用来在不泄露密钥的前提下确认“同一把钥匙”。
	fingerprint string
}

// NewKeyring 用 32 字节原始密钥构造 Keyring。密钥本身不会保存，只保存 AEAD 实例。
func NewKeyring(key []byte) (*Keyring, error) {
	if len(key) != SecretKeyBytes {
		return nil, fmt.Errorf("%w: 密钥长度必须是 %d 字节，实际 %d 字节", ErrKeyFile, SecretKeyBytes, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("%w: 初始化 AES 失败: %v", ErrKeyFile, err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("%w: 初始化 GCM 失败: %v", ErrKeyFile, err)
	}
	sum := sha256.Sum256(key)
	return &Keyring{aead: aead, fingerprint: hex.EncodeToString(sum[:8])}, nil
}

// Version 返回密钥版本。
func (k *Keyring) Version() int { return KeyVersion }

// Fingerprint 返回密钥指纹（SHA-256 前 8 字节的十六进制），可安全打印与入库。
func (k *Keyring) Fingerprint() string { return k.fingerprint }

// LoadKeyring 读取并校验本机密钥文件。
//
// 拒绝启动的情形（全部返回错误，不降级为“没有密钥也能跑”）：
//   - 文件不存在或不是普通文件（符号链接一律拒绝，避免指向别处）；
//   - 属主不是 root，或属组/其他用户有任何权限（Linux 上强制，见 perms_linux.go）；
//   - 首行标识不是 SecretKeyHeader、内容不是恰好 32 字节的 base64。
func LoadKeyring(path string) (*Keyring, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("%w: 密钥路径必须是绝对路径，当前为 %q", ErrKeyFile, path)
	}
	if _, err := os.Lstat(path); err != nil && errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s 不存在；首次部署请以 root 运行 `upanel-agent -init-key` 生成",
			ErrKeyFile, path)
	}
	if err := requireRootOnlyFile(path, secretKeyMode); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrKeyFile, err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: 读取 %s 失败: %v", ErrKeyFile, path, err)
	}
	key, err := parseKeyFile(raw)
	wipe(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	defer wipe(key)
	return NewKeyring(key)
}

// parseKeyFile 解析密钥文件内容：首行标识 + 次行 base64（允许末尾换行，不接受多余内容）。
func parseKeyFile(raw []byte) ([]byte, error) {
	trimmed := bytes.TrimRight(raw, "\r\n")
	lines := bytes.Split(trimmed, []byte("\n"))
	if len(lines) != 2 {
		return nil, fmt.Errorf("%w: 密钥文件必须是两行（版本标识 + base64 密钥），实际 %d 行", ErrKeyFile, len(lines))
	}
	if string(bytes.TrimRight(lines[0], "\r")) != SecretKeyHeader {
		return nil, fmt.Errorf("%w: 首行必须是 %q", ErrKeyFile, SecretKeyHeader)
	}
	encoded := string(bytes.TrimRight(lines[1], "\r"))
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("%w: 密钥不是合法 base64: %v", ErrKeyFile, err)
	}
	if len(key) != SecretKeyBytes {
		wipe(key)
		return nil, fmt.Errorf("%w: 密钥必须是 %d 字节，实际 %d 字节", ErrKeyFile, SecretKeyBytes, len(key))
	}
	return key, nil
}

// GenerateKeyFile 生成一把新的本机密钥并写入 path。
//
// 只在密钥**不存在**时成功：用 O_EXCL 创建，已存在则报错，绝不覆盖（覆盖等于让
// 已存的密文永久不可解）。目录不存在时按 root 独占的 0755 创建（末级目录）。
// 返回密钥指纹，供运维在不泄露密钥的前提下核对文件。
func GenerateKeyFile(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%w: 密钥路径必须是绝对路径，当前为 %q", ErrKeyFile, path)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, secretKeyDirMode); err != nil {
		return "", fmt.Errorf("%w: 创建密钥目录 %s 失败: %v", ErrKeyFile, dir, err)
	}
	if err := requireRootOnlyDir(dir); err != nil {
		return "", fmt.Errorf("%w: %v", ErrKeyFile, err)
	}

	key := make([]byte, SecretKeyBytes)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return "", fmt.Errorf("%w: 生成随机密钥失败: %v", ErrKeyFile, err)
	}
	defer wipe(key)

	content := SecretKeyHeader + "\n" + base64.StdEncoding.EncodeToString(key) + "\n"
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, secretKeyMode)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("%w: %s 已存在；拒绝覆盖已有密钥（覆盖会让已存的密文永久不可解）", ErrKeyFile, path)
		}
		return "", fmt.Errorf("%w: 创建 %s 失败: %v", ErrKeyFile, path, err)
	}
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("%w: 写入 %s 失败: %v", ErrKeyFile, path, err)
	}
	// 先落盘再回读校验：只改内存里的权限位、不等 fsync，掉电后可能留下 0644 的文件。
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("%w: 同步 %s 失败: %v", ErrKeyFile, path, err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("%w: 关闭 %s 失败: %v", ErrKeyFile, path, err)
	}
	// 用 umask 之后创建的文件权限可能被收窄或放宽，这里显式设置并回读。
	if err := os.Chmod(path, secretKeyMode); err != nil {
		return "", fmt.Errorf("%w: 设置 %s 权限失败: %v", ErrKeyFile, path, err)
	}
	keyring, err := LoadKeyring(path)
	if err != nil {
		return "", err
	}
	return keyring.Fingerprint(), nil
}

// Seal 加密明文：每条秘密生成独立随机 nonce，AAD 绑定秘密引用与用途。
//
// AAD 的作用是防止把 A 秘密的密文搬到 B 秘密的行上（库里被换行即解密失败）。
func (k *Keyring) Seal(plaintext []byte, ref, purpose string) (nonce, ciphertext []byte, err error) {
	nonce = make([]byte, nonceBytes)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, fmt.Errorf("agentstore: 生成 nonce 失败: %w", err)
	}
	ciphertext = k.aead.Seal(nil, nonce, plaintext, aadFor(ref, purpose))
	return nonce, ciphertext, nil
}

// Open 解密密文。nonce 或密文被改动、AAD 不匹配都会失败（GCM 认证失败）。
func (k *Keyring) Open(nonce, ciphertext []byte, ref, purpose string) ([]byte, error) {
	if len(nonce) != nonceBytes {
		return nil, fmt.Errorf("agentstore: nonce 长度必须是 %d 字节，实际 %d 字节", nonceBytes, len(nonce))
	}
	if len(ciphertext) < k.aead.Overhead() {
		return nil, errors.New("agentstore: 密文长度不足，数据可能被截断")
	}
	plaintext, err := k.aead.Open(nil, nonce, ciphertext, aadFor(ref, purpose))
	if err != nil {
		return nil, errors.New("agentstore: 解密失败（密钥不匹配或密文被改动）")
	}
	return plaintext, nil
}

// aadFor 构造 AAD。格式固定、带版本前缀，避免不同版本的 AAD 撞在一起。
func aadFor(ref, purpose string) []byte {
	return []byte("upanel/agent/secret/v1|" + ref + "|" + purpose)
}

// refPrefix 是 secret_ref 的固定前缀。
const refPrefix = "sec_"

// newSecretRef 生成 secret_ref：sec_ + 16 字节随机（32 位小写十六进制），共 36 字符。
func newSecretRef() (string, error) {
	buf := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		return "", fmt.Errorf("agentstore: 生成秘密引用失败: %w", err)
	}
	defer wipe(buf)
	return refPrefix + hex.EncodeToString(buf), nil
}

// ValidSecretRef 校验 secret_ref 的形态。
func ValidSecretRef(ref string) bool {
	if len(ref) != 36 || !strings.HasPrefix(ref, refPrefix) {
		return false
	}
	for _, r := range ref[len(refPrefix):] {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f':
		default:
			return false
		}
	}
	return true
}
