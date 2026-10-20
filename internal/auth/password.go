// Package auth 实现账号、密码哈希、会话与 CSRF（PLAN.md 第 5 节、CONTRACTS.md 第 2 节）。
//
// 边界：
//   - 密码用 Argon2id（golang.org/x/crypto/argon2），参数随哈希一起保存（PHC 字符串），
//     升级默认参数不需要迁移旧哈希；不自行实现密码学。
//   - 会话令牌与 CSRF 令牌只以 SHA-256 十六进制约值入库，明文只出现在 Cookie 与内存。
//   - 登录失败限速在进程内按账号与来源 IP 双维度计数，不做永久锁定（PLAN.md 第 5 节）。
//
// 本包不做 HTTP：DTO 校验、Cookie 读写、CSRF 头校验在 internal/api。
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

// 密码长度边界。
//
// 上限是硬限制：Argon2 的开销与输入长度无关，但超长输入没有任何收益，
// 而且会让请求体与日志处理更容易被滥用。下限是策略，见 ValidatePassword。
const (
	MinPasswordLength = 12
	MaxPasswordLength = 256
)

// PasswordParams 是 Argon2id 参数。
//
// 默认值取 128 MiB、3 次迭代、2 路并行：在开发宿主（M3 Air）上单次校验实测约 110 ms，
// 落在契约要求的 100—300 ms 区间内；换成更弱的机器只会更慢，不会更不安全。
// 实测记录见 docs/DECISIONS.md Q23。
// 参数随哈希保存在 PHC 字符串里，所以调整默认值不会让旧密码失效。
type PasswordParams struct {
	// Memory 为内存开销，单位 KiB。
	Memory uint32
	// Time 为迭代次数。
	Time uint32
	// Parallelism 为并行度。
	Parallelism uint8
	// SaltLength 为盐长度（字节）。
	SaltLength uint32
	// KeyLength 为派生密钥长度（字节）。
	KeyLength uint32
}

// DefaultPasswordParams 是新建哈希使用的参数。
var DefaultPasswordParams = PasswordParams{
	Memory:      128 * 1024,
	Time:        3,
	Parallelism: 2,
	SaltLength:  16,
	KeyLength:   32,
}

// 解析哈希时的防御性上限：数据库里的哈希被改坏时不应该让进程申请几 TB 内存。
const (
	maxParsedMemory uint32 = 1 << 20 // 1 GiB
	maxParsedTime   uint32 = 64
	maxParsedPar    uint8  = 64
	maxParsedKey    uint32 = 128
	maxParsedSalt   uint32 = 64
)

// ErrInvalidHash 表示哈希字符串不是本包能识别的 Argon2id PHC 格式。
var ErrInvalidHash = errors.New("auth: 密码哈希格式无效")

// ValidatePassword 校验新密码策略。message 面向用户，不含任何输入内容。
func ValidatePassword(username, password string) error {
	if len(password) < MinPasswordLength {
		return fmt.Errorf("密码至少需要 %d 个字符", MinPasswordLength)
	}
	if len(password) > MaxPasswordLength {
		return fmt.Errorf("密码不能超过 %d 字节", MaxPasswordLength)
	}
	if strings.EqualFold(password, username) {
		return errors.New("密码不能与用户名相同")
	}
	// 只包含单一重复字符的密码视为弱密码（例如 12 个 a）。
	if len(uniqueRunes(password)) == 1 {
		return errors.New("密码不能只由同一个字符组成")
	}
	return nil
}

// HashPassword 用默认参数生成 PHC 格式的 Argon2id 哈希。
func HashPassword(password string) (string, error) {
	return HashPasswordWith(DefaultPasswordParams, password)
}

// HashPasswordWith 用指定参数生成哈希。
func HashPasswordWith(params PasswordParams, password string) (string, error) {
	if len(password) == 0 {
		return "", errors.New("auth: 密码不能为空")
	}
	if len(password) > MaxPasswordLength {
		return "", fmt.Errorf("auth: 密码不能超过 %d 字节", MaxPasswordLength)
	}
	if params.SaltLength < 8 || params.SaltLength > maxParsedSalt {
		return "", fmt.Errorf("auth: 盐长度 %d 不在 8—%d 之间", params.SaltLength, maxParsedSalt)
	}
	if params.KeyLength < 16 || params.KeyLength > maxParsedKey {
		return "", fmt.Errorf("auth: 密钥长度 %d 不在 16—%d 之间", params.KeyLength, maxParsedKey)
	}
	if params.Memory < 8*1024 || params.Memory > maxParsedMemory {
		return "", fmt.Errorf("auth: 内存参数 %d KiB 超出允许范围", params.Memory)
	}
	if params.Time == 0 || params.Time > maxParsedTime {
		return "", fmt.Errorf("auth: 迭代次数 %d 超出允许范围", params.Time)
	}
	if params.Parallelism == 0 || params.Parallelism > maxParsedPar {
		return "", fmt.Errorf("auth: 并行度 %d 超出允许范围", params.Parallelism)
	}

	salt := make([]byte, params.SaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: 生成盐失败: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, params.Time, params.Memory, params.Parallelism, params.KeyLength)

	// PHC 字符串格式：$argon2id$v=19$m=65536,t=3,p=2$<salt>$<hash>
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, params.Memory, params.Time, params.Parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// VerifyPassword 校验密码是否匹配 PHC 哈希。
//
// 返回 (false, nil) 表示密码不匹配；(false, ErrInvalidHash) 表示哈希本身不可用，
// 调用方应把它当成服务端数据损坏，而不是“密码错误”。
func VerifyPassword(password, encoded string) (bool, error) {
	params, salt, key, err := decodeHash(encoded)
	if err != nil {
		return false, err
	}
	// 超长输入不参与派生，避免把请求体大小变成 CPU 开销。
	if len(password) > MaxPasswordLength {
		return false, nil
	}
	other := argon2.IDKey([]byte(password), salt, params.Time, params.Memory, params.Parallelism, uint32(len(key)))
	return subtle.ConstantTimeCompare(key, other) == 1, nil
}

// NeedsRehash 判断哈希是否使用了与当前默认值不同的参数（或版本）。
// 调用方在登录成功后按需重新哈希，不需要数据库迁移。
func NeedsRehash(encoded string) bool {
	params, _, _, err := decodeHash(encoded)
	if err != nil {
		return true
	}
	return params.Memory != DefaultPasswordParams.Memory ||
		params.Time != DefaultPasswordParams.Time ||
		params.Parallelism != DefaultPasswordParams.Parallelism ||
		params.SaltLength != DefaultPasswordParams.SaltLength ||
		params.KeyLength != DefaultPasswordParams.KeyLength
}

// ParamsOf 返回哈希里保存的参数，供 CLI 与日志（不含秘密）使用。
func ParamsOf(encoded string) (PasswordParams, error) {
	params, _, _, err := decodeHash(encoded)
	return params, err
}

func decodeHash(encoded string) (PasswordParams, []byte, []byte, error) {
	// 长度先兜底：正常哈希不到 100 字节，超长输入直接拒绝。
	if len(encoded) == 0 || len(encoded) > 512 {
		return PasswordParams{}, nil, nil, ErrInvalidHash
	}
	parts := strings.Split(encoded, "$")
	// "" "argon2id" "v=19" "m=..,t=..,p=.." "<salt>" "<hash>"
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return PasswordParams{}, nil, nil, ErrInvalidHash
	}

	versionName, versionValue, ok := strings.Cut(parts[2], "=")
	if !ok || versionName != "v" {
		return PasswordParams{}, nil, nil, ErrInvalidHash
	}
	version, err := parseUintField(versionValue, "v", 32)
	if err != nil || version != uint32(argon2.Version) {
		return PasswordParams{}, nil, nil, ErrInvalidHash
	}

	var params PasswordParams
	for _, field := range strings.Split(parts[3], ",") {
		name, value, ok := strings.Cut(field, "=")
		if !ok {
			return PasswordParams{}, nil, nil, ErrInvalidHash
		}
		switch name {
		case "m":
			v, err := parseUintField(value, "m", 32)
			if err != nil {
				return PasswordParams{}, nil, nil, ErrInvalidHash
			}
			params.Memory = v
		case "t":
			v, err := parseUintField(value, "t", 32)
			if err != nil {
				return PasswordParams{}, nil, nil, ErrInvalidHash
			}
			params.Time = v
		case "p":
			v, err := parseUintField(value, "p", 8)
			if err != nil {
				return PasswordParams{}, nil, nil, ErrInvalidHash
			}
			params.Parallelism = uint8(v)
		default:
			return PasswordParams{}, nil, nil, ErrInvalidHash
		}
	}
	if params.Memory == 0 || params.Memory > maxParsedMemory ||
		params.Time == 0 || params.Time > maxParsedTime ||
		params.Parallelism == 0 || params.Parallelism > maxParsedPar {
		return PasswordParams{}, nil, nil, ErrInvalidHash
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) < 8 || len(salt) > int(maxParsedSalt) {
		return PasswordParams{}, nil, nil, ErrInvalidHash
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(key) < 16 || len(key) > int(maxParsedKey) {
		return PasswordParams{}, nil, nil, ErrInvalidHash
	}
	params.SaltLength = uint32(len(salt))
	params.KeyLength = uint32(len(key))
	return params, salt, key, nil
}

func parseUintField(value, name string, bits int) (uint32, error) {
	if value == "" || strings.TrimLeft(value, "0123456789") != "" {
		return 0, fmt.Errorf("%s 不是无符号整数", name)
	}
	n, err := strconv.ParseUint(value, 10, bits)
	if err != nil {
		return 0, err
	}
	return uint32(n), nil
}

func uniqueRunes(s string) map[rune]struct{} {
	set := make(map[rune]struct{}, len(s))
	for _, r := range s {
		set[r] = struct{}{}
	}
	return set
}

// NewToken 生成 256 bit 随机令牌（base64url，无填充）。
func NewToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("auth: 生成随机令牌失败: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// HashToken 返回令牌的 SHA-256 十六进制约值。数据库只保存它。
//
// 会话令牌是 256 bit 的高熵随机值，不需要加盐或慢哈希；SHA-256 足以让泄露的
// 数据库记录无法直接当 Cookie 使用。
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
