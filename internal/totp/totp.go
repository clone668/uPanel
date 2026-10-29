// Package totp 实现 RFC 6238（TOTP）与一次性恢复码。
//
// 为什么是手写而不是引第三方库：RFC 4226/6238 的全部算法就是「HMAC-SHA1 一个计数器 →
// 动态截断 → 取模」，标准库已经把 HMAC 与 SHA1 给全了，剩下不到六十行。真正的风险不在
// 算法，在**窗口与比较**——接受几个时间步、怎么比较。所以这个包把两件事都钉死：窗口是
// ±1 步（前后各 30 秒，容忍时钟漂移，又不至于把有效期放到 90 秒以上），比较一律走
// `subtle.ConstantTimeCompare`。
//
// 证据不是"我们算的对"，是 RFC 6238 附录 B 的那六组测试向量（见 totp_test.go）：
// 种子 `12345678901234567890`（ASCII，base32 为 GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ），
// 时间点 59 / 1111111109 / 1111111111 / 1234567890 / 2000000000 / 20000000000，
// SHA1 的 8 位结果逐个对得上，才敢说自己实现的是 RFC 6238 而不是"某种能算出六位数的东西"。
package totp

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const (
	// Digits 是面板要求用户输入的位数。RFC 6238 允许 6—8 位，6 位是通行做法。
	Digits = 6
	// Period 是一个时间步的长度。
	Period = 30 * time.Second
	// SkewSteps 是允许前后各偏移几步。±1 步意味着最坏情况下一个码能用 90 秒。
	SkewSteps = 1
	// SecretBytes 是种子长度：RFC 4226 建议至少 160 bit，与 HMAC-SHA1 的输出等长。
	SecretBytes = 20
)

// ErrSecret 表示种子不是合法的 base32，或解出来是空的。
var ErrSecret = errors.New("totp: 种子不是合法的 base32")

// ErrCode 表示提交的码不是 6 位数字。（不是"码不对"，是"这根本不是个码"。）
var ErrCode = errors.New("totp: 码必须是 6 位数字")

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// GenerateSecret 生成一个新的种子，返回不带填充的 base32。
func GenerateSecret() (string, error) {
	buf := make([]byte, SecretBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return b32.EncodeToString(buf), nil
}

// Normalize 把用户可能粘进来的东西（小写、空格、填充符）收敛成标准 base32。
// authenticator 应用显示的种子常带空格与小写，粘贴时也常带 `=`，一律接受。
func Normalize(secret string) string {
	s := strings.ToUpper(strings.TrimSpace(secret))
	s = strings.ReplaceAll(s, " ", "")
	s = strings.ReplaceAll(s, "-", "")
	s = strings.TrimRight(s, "=")
	return s
}

func decodeSecret(secret string) ([]byte, error) {
	key, err := b32.DecodeString(Normalize(secret))
	if err != nil || len(key) == 0 {
		return nil, ErrSecret
	}
	return key, nil
}

// Code 返回某时刻的 6 位码，调用方用它做展示（测试与排障用）。
func Code(secret string, at time.Time) (string, error) {
	return codeWithDigits(secret, at, Digits)
}

func codeWithDigits(secret string, at time.Time, digits int) (string, error) {
	key, err := decodeSecret(secret)
	if err != nil {
		return "", err
	}
	return hotp(key, uint64(at.Unix()/int64(Period.Seconds())), digits), nil
}

// hotp 是 RFC 4226 的 HOTP：计数器 → 8 字节大端 → HMAC-SHA1 → 动态截断 → 取模。
func hotp(key []byte, counter uint64, digits int) string {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], counter)

	mac := hmac.New(sha1.New, key)
	mac.Write(buf[:])
	sum := mac.Sum(nil)

	offset := sum[len(sum)-1] & 0x0f
	value := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff

	mod := uint32(1)
	for i := 0; i < digits; i++ {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", digits, value%mod)
}

// Verify 判断某个码是否在允许的窗口内。返回 true 表示通过。
//
// 比较用常数时间；窗口内每个候选都算一遍再比，不在第一个字符不同时就短路——
// 码只有六位，逐位比较的时序差异足以把"前缀对了"泄露出去。
func Verify(secret, code string, at time.Time) bool {
	_, ok := MatchStep(secret, code, at)
	return ok
}

// MatchStep 与 Verify 相同，但额外返回匹配到的时间步。调用方拿它做**重放**防护：
// 同一个时间步的码只许用一次，第二次提交必须拒绝，否则一个被偷看到的码在 60 秒内可以
// 反复使用。返回的 step 在窗口为 ±1 时可能比当前步小一。
func MatchStep(secret, code string, at time.Time) (int64, bool) {
	if !validCode(code) {
		return 0, false
	}
	key, err := decodeSecret(secret)
	if err != nil {
		return 0, false
	}
	now := int64(at.Unix() / int64(Period.Seconds()))
	matched := int64(0)
	found := false
	for delta := -int64(SkewSteps); delta <= int64(SkewSteps); delta++ {
		step := now + delta
		if step < 0 {
			continue
		}
		candidate := hotp(key, uint64(step), Digits)
		if subtle.ConstantTimeCompare([]byte(candidate), []byte(code)) == 1 && !found {
			matched = step
			found = true
		}
	}
	if !found {
		return 0, false
	}
	return matched, true
}

func validCode(code string) bool {
	if len(code) != Digits {
		return false
	}
	for i := 0; i < len(code); i++ {
		if code[i] < '0' || code[i] > '9' {
			return false
		}
	}
	return true
}

// URI 拼出 otpauth:// 地址，直接喂给二维码编码器。
//
// 标签里 issuer 与账号用冒号分隔，两段都要转义——账号里出现 `:` 或 `@` 时，
// 不转义会让 authenticator 应用把标签切错，用户看到的是别人的名字。
//
// 这里不能用 `url.PathEscape`：它按 RFC 3986 的 path segment 规则转义，而 `:` 与 `@`
// 在 path segment 里是**合法字符**，于是原样留下——`admin@example.com` 会被 authenticator
// 当成 "issuer=admin, account=example.com"。测试先红了这一条（TestURIEscapesAwkwardAccountNames）。
func URI(issuer, account, secret string) string {
	label := escapeLabel(issuer) + ":" + escapeLabel(account)
	q := url.Values{}
	q.Set("secret", Normalize(secret))
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", fmt.Sprint(Digits))
	q.Set("period", fmt.Sprint(int(Period.Seconds())))
	return "otpauth://totp/" + label + "?" + q.Encode()
}

// escapeLabel 只放行 unreserved 字符（ALPHA / DIGIT / `-` / `.` / `_` / `~`），
// 其余一律百分号编码。标签是要被 authenticator 应用按冒号切分的，所以这里宁可过度转义。
func escapeLabel(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}
