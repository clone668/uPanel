package totp

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

// RFC 6238 附录 B 的 SHA1 种子：ASCII 串 "12345678901234567890"。
// 上面的 base32 就是它，写成字面量是为了让"种子从哪来"一眼可见。
const rfcSecret = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"

// 这六组向量是这个包存在的全部理由：它们证明我们实现的是 RFC 6238，
// 而不是"某种能算出六位数的东西"。
func TestCodeMatchesRFC6238Vectors(t *testing.T) {
	cases := []struct {
		unix int64
		want string
	}{
		{59, "94287082"},
		{1111111109, "07081804"},
		{1111111111, "14050471"},
		{1234567890, "89005924"},
		{2000000000, "69279037"},
		{20000000000, "65353130"},
	}
	for _, c := range cases {
		got, err := codeWithDigits(rfcSecret, time.Unix(c.unix, 0), 8)
		if err != nil {
			t.Fatalf("t=%d: %v", c.unix, err)
		}
		if got != c.want {
			t.Errorf("t=%d: got %s, want %s", c.unix, got, c.want)
		}
	}
}

// 6 位码必须是同一个动态截断值取模，也就是 8 位结果的末六位。
// 这不是"再算一遍比对"，而是从向量推出来的第二组独立读数。
func TestSixDigitCodeIsTheTailOfTheRFCValue(t *testing.T) {
	pairs := []struct {
		unix int64
		rfc8 string
	}{
		{59, "94287082"},
		{1111111109, "07081804"},
		{1234567890, "89005924"},
	}
	for _, p := range pairs {
		got, err := Code(rfcSecret, time.Unix(p.unix, 0))
		if err != nil {
			t.Fatalf("t=%d: %v", p.unix, err)
		}
		if want := p.rfc8[len(p.rfc8)-Digits:]; got != want {
			t.Errorf("t=%d: got %s, want %s (末六位)", p.unix, got, want)
		}
		if len(got) != Digits {
			t.Errorf("t=%d: 码长 %d, want %d", p.unix, len(got), Digits)
		}
	}
}

func TestVerifyAcceptsWithinSkewOnly(t *testing.T) {
	now := time.Unix(1111111109, 0)
	prev, err := Code(rfcSecret, now.Add(-Period))
	if err != nil {
		t.Fatal(err)
	}
	current, err := Code(rfcSecret, now)
	if err != nil {
		t.Fatal(err)
	}
	next, err := Code(rfcSecret, now.Add(Period))
	if err != nil {
		t.Fatal(err)
	}
	for name, code := range map[string]string{"前一步": prev, "当前": current, "后一步": next} {
		if !Verify(rfcSecret, code, now) {
			t.Errorf("%s 的码应在窗口内被接受", name)
		}
	}

	tooOld, err := Code(rfcSecret, now.Add(-2*Period))
	if err != nil {
		t.Fatal(err)
	}
	tooNew, err := Code(rfcSecret, now.Add(2*Period))
	if err != nil {
		t.Fatal(err)
	}
	if Verify(rfcSecret, tooOld, now) {
		t.Error("两步之前（60 秒前）的码不该还在窗口里")
	}
	if Verify(rfcSecret, tooNew, now) {
		t.Error("两步之后（60 秒后）的码不该在窗口里")
	}
}

// MatchStep 返回的时间步是重放防护的依据：同一个步只能被用一次。
func TestMatchStepReportsTheStepItMatched(t *testing.T) {
	now := time.Unix(1111111109, 0)
	prev, err := Code(rfcSecret, now.Add(-Period))
	if err != nil {
		t.Fatal(err)
	}
	step, ok := MatchStep(rfcSecret, prev, now)
	if !ok {
		t.Fatal("上一步的码应当匹配")
	}
	nowStep := now.Unix() / int64(Period.Seconds())
	if step != nowStep-1 {
		t.Errorf("step=%d, want %d（上一步）", step, nowStep-1)
	}

	current, err := Code(rfcSecret, now)
	if err != nil {
		t.Fatal(err)
	}
	if s, ok := MatchStep(rfcSecret, current, now); !ok || s != nowStep {
		t.Errorf("当前码应当匹配到 %d，实际 %d/%v", nowStep, s, ok)
	}
}

func TestVerifyRejectsMalformedCodes(t *testing.T) {
	now := time.Unix(1111111109, 0)
	for _, code := range []string{"", "12345", "1234567", "abcdef", "12 456", "１２３４５６"} {
		if Verify(rfcSecret, code, now) {
			t.Errorf("%q 不该被当成合法的码", code)
		}
	}
	if _, ok := MatchStep(rfcSecret, "12345", now); ok {
		t.Error("长度不对时 MatchStep 也不该说匹配")
	}
}

func TestVerifyRejectsWrongCodeButValidShape(t *testing.T) {
	now := time.Unix(1111111109, 0)
	right, err := Code(rfcSecret, now)
	if err != nil {
		t.Fatal(err)
	}
	wrong := right[:Digits-1] + string('0'+(right[Digits-1]-'0'+1)%10)
	if Verify(rfcSecret, wrong, now) {
		t.Errorf("错一位的码 %s（正确是 %s）不该通过", wrong, right)
	}
}

func TestSecretIsForgivingAboutFormatting(t *testing.T) {
	now := time.Unix(1111111109, 0)
	variants := []string{
		strings.ToLower(rfcSecret),
		"gezd gnbv gy3t qojq gezd gnbv gy3t qojq",
		rfcSecret + "========",
		"GEZD-GNBV-GY3T-QOJQ-GEZD-GNBV-GY3T-QOJQ",
	}
	right, err := Code(rfcSecret, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range variants {
		got, err := Code(v, now)
		if err != nil {
			t.Errorf("%q: %v", v, err)
			continue
		}
		if got != right {
			t.Errorf("%q 算出的码是 %s，应当与 %s 相同", v, got, right)
		}
		if !Verify(v, right, now) {
			t.Errorf("%q 应当能验证 %s", v, right)
		}
	}
	if _, err := Code("不是 base32 的种子", now); err == nil {
		t.Error("非 base32 的种子应当报错")
	}
	if _, err := Code("", now); err == nil {
		t.Error("空种子应当报错")
	}
}

func TestGenerateSecretIsRandomAndUnpadded(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 16; i++ {
		s, err := GenerateSecret()
		if err != nil {
			t.Fatal(err)
		}
		if strings.ContainsAny(s, "=") {
			t.Fatalf("%q 带填充符", s)
		}
		if want := base32Len(SecretBytes); len(s) != want {
			t.Fatalf("长度 %d, want %d", len(s), want)
		}
		if _, err := Code(s, time.Now()); err != nil {
			t.Fatalf("%q 算不出码: %v", s, err)
		}
		if seen[s] {
			t.Fatalf("生成了重复的种子 %q", s)
		}
		seen[s] = true
	}
}

func base32Len(n int) int {
	if n%5 == 0 {
		return n * 8 / 5
	}
	return n*8/5 + 1
}

func TestURIIsWhatAuthenticatorAppsExpect(t *testing.T) {
	uri := URI("uPanel", "admin", rfcSecret)
	u, err := url.Parse(uri)
	if err != nil {
		t.Fatalf("拼出来的不是合法 URI: %v", err)
	}
	if u.Scheme != "otpauth" || u.Host != "totp" {
		t.Errorf("scheme/host = %s/%s, want otpauth/totp", u.Scheme, u.Host)
	}
	if got := u.Path; got != "/uPanel:admin" {
		t.Errorf("path = %q, want /uPanel:admin", got)
	}
	q := u.Query()
	for key, want := range map[string]string{
		"secret":    rfcSecret,
		"issuer":    "uPanel",
		"algorithm": "SHA1",
		"digits":    "6",
		"period":    "30",
	} {
		if got := q.Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

// 账号里带冒号或 @ 时，标签必须转义，否则 authenticator 应用会把名字切错。
func TestURIEscapesAwkwardAccountNames(t *testing.T) {
	uri := URI("uPanel", "a:b@c", rfcSecret)
	if !strings.Contains(uri, "a%3Ab%40c") {
		t.Errorf("转义不对：%s", uri)
	}
	u, err := url.Parse(uri)
	if err != nil {
		t.Fatalf("拼出来的不是合法 URI: %v", err)
	}
	// 用 EscapedPath：url.Parse 会把 %3A 解码回冒号，看 Path 是看不出转义的。
	if strings.Count(strings.TrimPrefix(u.EscapedPath(), "/"), ":") != 1 {
		t.Errorf("标签里只应有分隔 issuer 与账号的那一个冒号：%q", u.EscapedPath())
	}
}

func TestRecoveryCodesShapeAndUniqueness(t *testing.T) {
	codes, err := GenerateRecoveryCodes()
	if err != nil {
		t.Fatal(err)
	}
	if len(codes) != RecoveryCount {
		t.Fatalf("数量 %d, want %d", len(codes), RecoveryCount)
	}
	seen := map[string]bool{}
	for _, c := range codes {
		if len(c) != recoveryChars+1 || c[recoveryChars/2] != '-' {
			t.Fatalf("%q 的形状不对（应为 5 位 + 连字符 + 5 位）", c)
		}
		for _, r := range strings.ReplaceAll(c, "-", "") {
			if !strings.ContainsRune(recoveryAlphabet, r) {
				t.Fatalf("%q 里出现了字母表外的字符 %q", c, r)
			}
		}
		if seen[c] {
			t.Fatalf("同一批里出现重复的恢复码 %q", c)
		}
		seen[c] = true
	}
}

// 字母表必须是 256 的因数，否则取模有偏；也不能包含念不清的字符。
func TestRecoveryAlphabetIsUniformAndUnambiguous(t *testing.T) {
	if len(recoveryAlphabet) != 32 {
		t.Errorf("字母表有 %d 个符号，want 32", len(recoveryAlphabet))
	}
	if 256%len(recoveryAlphabet) != 0 {
		t.Errorf("字母表长度 %d 不能整除 256，取模会有偏差", len(recoveryAlphabet))
	}
	for _, bad := range []string{"0", "1", "O", "I"} {
		if strings.Contains(recoveryAlphabet, bad) {
			t.Errorf("字母表不该包含容易念错的 %q", bad)
		}
	}
}

func TestRecoveryHashRoundTrip(t *testing.T) {
	codes, err := GenerateRecoveryCodes()
	if err != nil {
		t.Fatal(err)
	}
	code := codes[0]
	hash, err := HashRecoveryCode(code)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, recoveryPrefix) {
		t.Errorf("哈希应当带算法前缀 %q：%s", recoveryPrefix, hash)
	}
	if strings.Contains(hash, CanonicalRecoveryCode(code)) {
		t.Error("哈希里不该出现明文码")
	}
	if !CompareRecoveryHash(hash, code) {
		t.Error("同一个码应当匹配自己的哈希")
	}
	// 用户会照着屏幕念、也可能随手粘贴：大小写与连字符都不该影响识别。
	for _, variant := range []string{
		strings.ToLower(code),
		strings.ReplaceAll(code, "-", ""),
		" " + code + " ",
	} {
		if !CompareRecoveryHash(hash, variant) {
			t.Errorf("变体 %q 应当也能匹配", variant)
		}
	}
	other, err := GenerateRecoveryCodes()
	if err != nil {
		t.Fatal(err)
	}
	if CompareRecoveryHash(hash, other[0]) {
		t.Error("另一个码不该匹配这个哈希")
	}
}

func TestRecoveryRejectsShortAndLongInput(t *testing.T) {
	for _, bad := range []string{"", "ABC", "ABCDE-FGH", "ABCDE-FGHJKL", "ABCDE-FGHJK-2"} {
		if _, err := HashRecoveryCode(bad); err == nil {
			t.Errorf("%q 不该被当成合法恢复码", bad)
		}
		if ValidRecoveryCode(bad) {
			t.Errorf("%q 不该被判为形状合法", bad)
		}
		if CompareRecoveryHash(recoveryPrefix+"deadbeef", bad) {
			t.Errorf("%q 不该匹配任何哈希", bad)
		}
	}
	if !ValidRecoveryCode("ABCDE-FGHJK") {
		t.Error("正确形状的恢复码应当被判为合法")
	}
}
