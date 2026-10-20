package auth

import (
	"strings"
	"testing"
	"time"
)

func TestValidatePassword(t *testing.T) {
	cases := []struct {
		name     string
		username string
		password string
		wantErr  bool
	}{
		{"正常密码", "alice", "correct-horse-9", false},
		{"太短", "alice", "short-1", true},
		{"空", "alice", "", true},
		{"与用户名相同", "alicealicealice", "alicealicealice", true},
		{"单一字符重复", "alice", strings.Repeat("a", 16), true},
		{"超长", "alice", strings.Repeat("x", MaxPasswordLength+1), true},
		{"刚好达到上限", "alice", strings.Repeat("x", MaxPasswordLength-2) + "A1", false},
	}
	for _, c := range cases {
		err := ValidatePassword(c.username, c.password)
		if c.wantErr && err == nil {
			t.Fatalf("%s: 期望报错，实际通过", c.name)
		}
		if !c.wantErr && err != nil {
			t.Fatalf("%s: 不应报错: %v", c.name, err)
		}
	}
}

func TestHashPasswordRoundTrip(t *testing.T) {
	const password = "correct-horse-9"

	hash, err := HashPassword(password)
	if err != nil {
		t.Fatalf("哈希失败: %v", err)
	}
	// PHC 格式：$argon2id$v=19$m=...,t=...,p=...$salt$hash
	if !strings.HasPrefix(hash, "$argon2id$v=19$m=131072,t=3,p=2$") {
		t.Fatalf("哈希格式不符合预期: %q", hash)
	}
	if strings.Contains(hash, password) {
		t.Fatal("哈希里不能出现明文密码")
	}

	ok, err := VerifyPassword(password, hash)
	if err != nil {
		t.Fatalf("校验失败: %v", err)
	}
	if !ok {
		t.Fatal("正确密码应校验通过")
	}

	ok, err = VerifyPassword("wrong-password-1", hash)
	if err != nil {
		t.Fatalf("校验错误密码时出错: %v", err)
	}
	if ok {
		t.Fatal("错误密码不应校验通过")
	}

	// 同一密码两次哈希必须不同（随机盐）。
	other, err := HashPassword(password)
	if err != nil {
		t.Fatalf("第二次哈希失败: %v", err)
	}
	if other == hash {
		t.Fatal("两次哈希结果相同，说明没有使用随机盐")
	}
}

func TestVerifyPasswordRejectsMalformedHash(t *testing.T) {
	cases := []string{
		"",
		"plaintext",
		"$argon2id$v=19$m=65536,t=3,p=2$onlyonepart",
		"$argon2i$v=19$m=65536,t=3,p=2$c2FsdA$aGFzaA",
		"$argon2id$v=19$m=notanumber,t=3,p=2$c2FsdA$aGFzaA",
		// 参数越界（内存 2 GiB）必须拒绝，避免恶意哈希串把进程拖死。
		"$argon2id$v=19$m=2097152,t=3,p=2$c2FsdHNhbHRzYWx0c2E$aGFzaGhhc2hoYXNoaGFzaA",
	}
	for _, hash := range cases {
		if _, err := VerifyPassword("whatever-password", hash); err == nil {
			t.Fatalf("畸形哈希必须报错: %q", hash)
		}
	}
}

func TestNeedsRehash(t *testing.T) {
	weak := PasswordParams{Memory: 8 * 1024, Time: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32}
	hash, err := HashPasswordWith(weak, "correct-horse-9")
	if err != nil {
		t.Fatalf("哈希失败: %v", err)
	}
	if !NeedsRehash(hash) {
		t.Fatal("低于默认参数的哈希应被判定为需要升级")
	}

	strong, err := HashPassword("correct-horse-9")
	if err != nil {
		t.Fatalf("哈希失败: %v", err)
	}
	if NeedsRehash(strong) {
		t.Fatal("默认参数的哈希不应被判定为需要升级")
	}
	// 畸形哈希也必须判为需要升级（调用方会重新计算），而不是静默接受。
	if !NeedsRehash("broken") {
		t.Fatal("无法解析的哈希应判为需要升级")
	}
}

func TestPasswordParamsParsing(t *testing.T) {
	params := PasswordParams{Memory: 32 * 1024, Time: 2, Parallelism: 3, SaltLength: 16, KeyLength: 24}
	hash, err := HashPasswordWith(params, "correct-horse-9")
	if err != nil {
		t.Fatalf("哈希失败: %v", err)
	}
	got, err := ParamsOf(hash)
	if err != nil {
		t.Fatalf("解析参数失败: %v", err)
	}
	if got.Memory != params.Memory || got.Time != params.Time || got.Parallelism != params.Parallelism ||
		got.SaltLength != params.SaltLength || got.KeyLength != params.KeyLength {
		t.Fatalf("解析出的参数 = %+v，期望 %+v", got, params)
	}
}

func TestTokenGeneration(t *testing.T) {
	seen := make(map[string]struct{}, 64)
	for i := 0; i < 64; i++ {
		token, err := NewToken()
		if err != nil {
			t.Fatalf("生成令牌失败: %v", err)
		}
		// 32 字节 base64url 无填充 = 43 字符，约 256 bit 熵。
		if len(token) < 43 {
			t.Fatalf("令牌过短: %q", token)
		}
		if _, dup := seen[token]; dup {
			t.Fatal("生成的令牌出现重复")
		}
		seen[token] = struct{}{}

		hash := HashToken(token)
		if len(hash) != 64 {
			t.Fatalf("令牌哈希应为 64 位十六进制: %q", hash)
		}
		if hash != HashToken(token) {
			t.Fatal("同一令牌的哈希必须稳定")
		}
		if hash == token {
			t.Fatal("库里不能保存明文令牌")
		}
	}
}

func TestDefaultPasswordParamsTiming(t *testing.T) {
	// 契约要求单次校验约 100—300 ms。这里只做数量级断言：
	// 太慢说明参数过重，太快说明参数过轻。
	hash, err := HashPassword("correct-horse-9")
	if err != nil {
		t.Fatalf("哈希失败: %v", err)
	}
	start := time.Now()
	if _, err := VerifyPassword("correct-horse-9", hash); err != nil {
		t.Fatalf("校验失败: %v", err)
	}
	elapsed := time.Since(start)
	t.Logf("单次 Argon2id 校验耗时 %s", elapsed)
	if elapsed < 10*time.Millisecond {
		t.Fatalf("校验耗时 %s 过短，参数可能过轻", elapsed)
	}
	if elapsed > time.Second {
		t.Fatalf("校验耗时 %s 过长，需重新基准", elapsed)
	}
}
