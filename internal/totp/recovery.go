package totp

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"strings"
)

// 恢复码是"手机丢了也进得去"的唯一一口。它的设计约束与口令不同：
//
//   - **一次性**：用掉就作废，所以库里存的是哈希而不是码本身，用过的哈希立刻删掉。
//   - **每个只够猜一次**：10 个码 × 10 个字符、字母表 32 个符号 ≈ 50 bit，
//     再加上登录限速，穷举不成立。
//   - **人能在没有键盘布局的情况下念出来**：字母表去掉 0/O/1/I 这类会念错的字符，
//     五五一组的显示形式，粘贴时连字符可有可无。
const (
	// RecoveryCount 是启用 TOTP 时一次发出的恢复码数量。
	RecoveryCount = 10
	// recoveryAlphabet 去掉了 0/1/O/I 这四个会念错的，剩下正好 32 个符号
	// （数字 2—9 加 24 个字母：A—Z 去掉 I 与 O）。为什么必须是 32 的整数倍：
	// 取一个字节再 `% 32`，256/32 整除，取模不产生偏差；31 或 33 个符号就会有偏。
	recoveryAlphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"
	// recoveryChars 是单个码的字符数（不含连字符）。
	recoveryChars = 10
	// recoveryPrefix 让哈希带上算法标识：以后换算法时，旧行还能认出来。
	recoveryPrefix = "sha256:"
)

// ErrRecoveryCode 表示提交的东西不像一个恢复码（长度或字符不对）。
var ErrRecoveryCode = errors.New("totp: 恢复码格式不对")

// GenerateRecoveryCodes 生成一批恢复码，形如 `3F7K9-QM2XP`。
//
// 每取一个字符都单独读一次随机源：所有码共用一次 rand.Read 也安全，但分开发出的码在
// 日志里更难被"一锅端"地联系到一起，且实现上没有批量缓冲的边界问题。
func GenerateRecoveryCodes() ([]string, error) {
	out := make([]string, 0, RecoveryCount)
	for i := 0; i < RecoveryCount; i++ {
		code, err := generateRecoveryCode()
		if err != nil {
			return nil, err
		}
		out = append(out, code)
	}
	return out, nil
}

func generateRecoveryCode() (string, error) {
	var out strings.Builder
	for i := 0; i < recoveryChars; i++ {
		if i == recoveryChars/2 {
			out.WriteByte('-')
		}
		var b [1]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", err
		}
		// 取模偏差：256 % 32 == 0，所以 `% len` 在 32 个符号上是均匀的。
		out.WriteByte(recoveryAlphabet[int(b[0])%len(recoveryAlphabet)])
	}
	return out.String(), nil
}

// CanonicalRecoveryCode 把用户输入收敛成用于哈希与比较的形式：大写、去掉所有非字母数字。
func CanonicalRecoveryCode(code string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(code) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		}
	}
	return b.String()
}

// HashRecoveryCode 返回 `sha256:<hex>` 形式的哈希。库里只存这个。
//
// 不用 Argon2id：恢复码是 50 bit 的随机串，不存在字典，慢哈希只会让登录多花几百毫秒
// 而买不到安全性；口令是另一回事，那边必须慢。
func HashRecoveryCode(code string) (string, error) {
	canonical := CanonicalRecoveryCode(code)
	if len(canonical) != recoveryChars {
		return "", ErrRecoveryCode
	}
	sum := sha256.Sum256([]byte(canonical))
	return recoveryPrefix + hex.EncodeToString(sum[:]), nil
}

// CompareRecoveryHash 比较一个码与库里的哈希。
//
// 形状不对（长度/字符）直接判否——这只会泄露"这不是一个码"，不泄露任何关于真码的信息。
// 形状对时就比较两个同长十六进制串，走常数时间，不让"前几位对了"从耗时上漏出去。
func CompareRecoveryHash(hash, code string) bool {
	want, err := HashRecoveryCode(code)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(want), []byte(hash)) == 1
}

// ValidRecoveryCode 只校验形状，不涉及哈希。判决层用它把"明显不是码"的输入早点挡掉，
// 免得为一个错格式的输入去查库。
func ValidRecoveryCode(code string) bool {
	return len(CanonicalRecoveryCode(code)) == recoveryChars
}
