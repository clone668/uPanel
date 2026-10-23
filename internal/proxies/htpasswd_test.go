package proxies

// P10.2 basic auth 账号文件用例。
//
// 三条不能让步的纪律，逐条对应下面的用例：
//
//  1. 哈希必须被 nginx 认——用 openssl 这个外部实现当判据，不用“我自己算的和我自己
//     算的一样”这种空转对照；
//  2. 明文不进文件、不进日志——渲染结果里出现一次明文就算失败；
//  3. 同样输入产出同样字节——否则每次应用都会重写账号文件、触发一次不必要的 reload。

import (
	"strings"
	"testing"
)

// TestAPR1MatchesOpenSSL 与 openssl 的输出逐字对齐。
//
// 向量来自开发主机上的实跑（不是从文档抄的）：
//
//	$ openssl passwd -apr1 -salt 01234567 password
//	$apr1$01234567$IXBaQywhAhc0d75ZbaSDp/
//
// 这个对照是整个 apr1 实现的唯一硬证据：算法里任何一处置换、轮数、位顺序写错，
// 都会得到一个格式正确但**nginx 永远认不出的哈希**——症状是所有账号都 401，
// 而且看起来像“密码填错了”。
func TestAPR1MatchesOpenSSL(t *testing.T) {
	cases := []struct{ password, salt, want string }{
		{"password", "01234567", "$apr1$01234567$IXBaQywhAhc0d75ZbaSDp/"},
		// 同一密码换盐必须换结果（盐确实参与了运算，不是摆设）。
		{"password", "abcdefgh", APR1("password", "abcdefgh")},
	}
	if got := APR1(cases[0].password, cases[0].salt); got != cases[0].want {
		t.Fatalf("APR1(%q, %q) = %q，期望 %q", cases[0].password, cases[0].salt, got, cases[0].want)
	}
	if cases[0].want == cases[1].want {
		t.Fatal("换了盐却得到同样的哈希：盐没有参与运算")
	}
	if !strings.HasPrefix(cases[1].want, "$apr1$abcdefgh$") {
		t.Errorf("哈希前缀不对：%q", cases[1].want)
	}
	// apr1 的输出长度是固定的：$apr1$ + 8 位盐 + $ + 22 位。
	if n := len(cases[0].want); n != 6+8+1+22 {
		t.Errorf("apr1 长度 = %d，期望 %d", n, 6+8+1+22)
	}
}

// TestAPR1SaltNormalization 盐按 apr1 的规则截断与清洗。
func TestAPR1SaltNormalization(t *testing.T) {
	ref := APR1("secret", "01234567")
	// 带前缀、带后缀、超长都要归到同一个 8 字符盐上。
	for _, in := range []string{
		"01234567",
		"$apr1$01234567",
		"$apr1$01234567$ignored",
		"0123456789abcdef",
	} {
		if got := APR1("secret", in); got != ref {
			t.Errorf("盐 %q 归化后的结果 = %q，期望 %q", in, got, ref)
		}
	}
	// 空盐不能变成随机盐：那会让每次应用都写出不同文件。
	if a, b := APR1("secret", ""), APR1("secret", ""); a != b {
		t.Errorf("空盐产生了不确定的结果：%q vs %q", a, b)
	}
}

// TestDeriveSaltIsStableAndDistinct 盐由站点与账号名派生，稳定且两两不同。
func TestDeriveSaltIsStableAndDistinct(t *testing.T) {
	a := deriveSalt(renderTestSiteID, "alice")
	if a != deriveSalt(renderTestSiteID, "alice") {
		t.Fatal("同样的站点与账号名派生出不同盐：幂等会失效")
	}
	if len(a) != 8 {
		t.Errorf("盐长 = %d，期望 8", len(a))
	}
	others := []string{
		deriveSalt(renderTestSiteID, "bob"),
		deriveSalt("site_fedcba9876543210fedcba9876543210", "alice"),
	}
	for _, o := range others {
		if o == a {
			t.Errorf("不同（站点，账号名）得到同一个盐 %q", o)
		}
	}
}

// authUsers 造一组账号。
func authUsers() []BasicAuthUser {
	return []BasicAuthUser{
		{Name: "alice", SecretRef: "sec_0123456789abcdef0123456789abcdef"},
		{Name: "bob", SecretRef: "sec_fedcba9876543210fedcba9876543210"},
	}
}

// TestRenderAuthFileSortedAndStable 按账号名排序、逐字节可复现、不含明文。
func TestRenderAuthFileSortedAndStable(t *testing.T) {
	passwords := map[string]string{"alice": "hunter2-alice", "bob": "hunter2-bob"}
	out, err := RenderAuthFile(renderTestSiteID, authUsers(), passwords)
	if err != nil {
		t.Fatalf("渲染账号文件失败: %v", err)
	}
	again, err := RenderAuthFile(renderTestSiteID, authUsers(), passwords)
	if err != nil {
		t.Fatalf("渲染账号文件失败: %v", err)
	}
	if out != again {
		t.Fatal("同样输入渲染出不同字节：每次应用都会重写账号文件")
	}

	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("行数 = %d，期望 2：%q", len(lines), out)
	}
	// 定义里的顺序是 alice、bob，这里必须按名字排——用户改一下顺序不该触发一次重写。
	if !strings.HasPrefix(lines[0], "alice:$apr1$") || !strings.HasPrefix(lines[1], "bob:$apr1$") {
		t.Errorf("账号没有按名字排序：%q", lines)
	}

	// 明文一个字节都不能出现。
	for name, pw := range passwords {
		if strings.Contains(out, pw) {
			t.Errorf("账号 %s 的明文出现在了账号文件里", name)
		}
	}
	// 秘密引用也不该出现：文件里只有"名字 + 哈希"。
	for _, u := range authUsers() {
		if strings.Contains(out, u.SecretRef) {
			t.Errorf("账号 %s 的秘密引用出现在了账号文件里", u.Name)
		}
	}
	// 哈希必须能被 nginx 认（apr1 前缀）。
	for _, line := range lines {
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 || !strings.HasPrefix(parts[1], "$apr1$") {
			t.Errorf("这一行不是 apr1 哈希：%q", line)
		}
	}
}

// TestRenderAuthFileOrderIndependent 定义顺序不同但账号相同，产物必须一致。
func TestRenderAuthFileOrderIndependent(t *testing.T) {
	passwords := map[string]string{"alice": "a", "bob": "b"}
	forward, err := RenderAuthFile(renderTestSiteID, authUsers(), passwords)
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	users := authUsers()
	users[0], users[1] = users[1], users[0]
	backward, err := RenderAuthFile(renderTestSiteID, users, passwords)
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	if forward != backward {
		t.Errorf("账号顺序影响了产物：\n%q\n%q", forward, backward)
	}
}

// TestRenderAuthFileRejects 缺账号、重名、名字形状不对、密码为空都必须拒绝。
func TestRenderAuthFileRejects(t *testing.T) {
	ok := map[string]string{"alice": "a", "bob": "b"}
	cases := []struct {
		name  string
		users []BasicAuthUser
		pw    map[string]string
	}{
		{"没有账号", nil, ok},
		{"账号名为空", []BasicAuthUser{{Name: "", SecretRef: authUsers()[0].SecretRef}}, ok},
		{"账号名带大写", []BasicAuthUser{{Name: "Alice", SecretRef: authUsers()[0].SecretRef}}, ok},
		{"账号名带冒号", []BasicAuthUser{{Name: "a:b", SecretRef: authUsers()[0].SecretRef}}, ok},
		{"账号名带换行", []BasicAuthUser{{Name: "a\nb", SecretRef: authUsers()[0].SecretRef}}, ok},
		{"重名", []BasicAuthUser{authUsers()[0], authUsers()[0]}, ok},
		{"密码为空", []BasicAuthUser{authUsers()[0]}, map[string]string{"alice": ""}},
		{"密码缺失", []BasicAuthUser{authUsers()[0]}, map[string]string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := RenderAuthFile(renderTestSiteID, tc.users, tc.pw)
			if err == nil {
				t.Fatalf("非法输入却渲染出了账号文件：%q", out)
			}
			if out != "" {
				t.Errorf("拒绝时必须返回空内容，实际 %q", out)
			}
		})
	}
}
