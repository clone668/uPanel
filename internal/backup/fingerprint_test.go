package backup

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// testHostKey 生成一把测试用主机密钥。
func testHostKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestParseFingerprint(t *testing.T) {
	cases := []struct {
		name string
		in   string
		ok   bool
	}{
		{"sha256 无填充", "SHA256:47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU", true},
		{"sha256 带填充", "SHA256:47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU=", true},
		{"小写算法名", "sha256:47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU", true},
		{"md5", "MD5:2a:3b:4c:5d:6e:7f:80:91:a2:b3:c4:d5:e6:f7:08:19", true},
		{"空", "", false},
		{"无算法前缀", "47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU", false},
		{"未知算法", "SHA1:abcdef", false},
		{"sha256 长度不对", "SHA256:YWJj", false},
		{"md5 组数不对", "MD5:2a:3b", false},
		{"md5 非十六进制", "MD5:zz:3b:4c:5d:6e:7f:80:91:a2:b3:c4:d5:e6:f7:08:19", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fp, err := ParseFingerprint(c.in)
			if c.ok {
				if err != nil {
					t.Fatalf("应被接受，却报错 %v", err)
				}
				if fp.String() == "" {
					t.Fatal("解析结果不该是空指纹")
				}
				return
			}
			if err == nil {
				t.Fatalf("应被拒绝，却接受了：%q", fp.String())
			}
			if !errors.Is(err, ErrFingerprint) {
				t.Fatalf("错误应包 ErrFingerprint，得到 %v", err)
			}
		})
	}
}

func TestFingerprintMatchesOnlyTheRegisteredKey(t *testing.T) {
	key := testHostKey(t)
	other := testHostKey(t)

	fp, err := ParseFingerprint(SHA256(key))
	if err != nil {
		t.Fatal(err)
	}
	if !fp.Matches(key) {
		t.Fatal("登记的那把钥匙应该匹配")
	}
	if fp.Matches(other) {
		t.Fatal("另一把钥匙绝不能匹配")
	}
	if fp.Matches(nil) {
		t.Fatal("nil 公钥必须不匹配")
	}

	legacy, err := ParseFingerprint(MD5(key))
	if err != nil {
		t.Fatal(err)
	}
	if !legacy.Matches(key) {
		t.Fatal("MD5 指纹应该匹配同一把钥匙")
	}
	if legacy.Matches(other) {
		t.Fatal("MD5 指纹不该匹配另一把钥匙")
	}
}

func TestKnownHostsLine(t *testing.T) {
	key := testHostKey(t)
	line := KnownHostsLine("backup.example.com", DefaultSFTPPort, key)
	if !strings.HasPrefix(line, "backup.example.com "+key.Type()+" ") {
		t.Fatalf("22 端口应写裸主机名，得到 %q", line)
	}
	// 非默认端口必须用 [host]:port，否则 ssh 查不到这条记录。
	line = KnownHostsLine("backup.example.com", 2222, key)
	if !strings.HasPrefix(line, "[backup.example.com]:2222 "+key.Type()+" ") {
		t.Fatalf("非默认端口应写成 [host]:port，得到 %q", line)
	}
	// 端口 0（未给出）按 22 处理。
	line0 := KnownHostsLine("backup.example.com", 0, key)
	if !strings.HasPrefix(line0, "backup.example.com ") {
		t.Fatalf("端口 0 应按 22 处理，得到 %q", line0)
	}
	// 行里不许出现注释以外的多余字段：三段，不多不少。
	if got := len(strings.Fields(line)); got != 3 {
		t.Fatalf("known_hosts 行应为三段，得到 %d 段：%q", got, line)
	}
	if KnownHostsLine("h", 22, nil) != "" {
		t.Fatal("nil 公钥应得到空行（失败关闭）")
	}
}
