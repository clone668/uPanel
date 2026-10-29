package accounts

import (
	"encoding/base64"
	"encoding/binary"
	"strings"
	"testing"
)

// makeKey 造一把形状正确的公钥（线格式：算法名 + 32 字节主体）。
func makeKey(t *testing.T, algorithm string, seed byte, comment string) string {
	t.Helper()
	var blob []byte
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(algorithm)))
	blob = append(blob, n[:]...)
	blob = append(blob, algorithm...)
	binary.BigEndian.PutUint32(n[:], 32)
	blob = append(blob, n[:]...)
	body := make([]byte, 32)
	for i := range body {
		body[i] = seed + byte(i)
	}
	blob = append(blob, body...)
	line := algorithm + " " + base64.StdEncoding.EncodeToString(blob)
	if comment != "" {
		line += " " + comment
	}
	return line
}

func TestParsePublicKeyAcceptsSupportedTypes(t *testing.T) {
	for _, algorithm := range []string{
		"ssh-ed25519", "ssh-rsa", "ecdsa-sha2-nistp256",
		"ecdsa-sha2-nistp384", "ecdsa-sha2-nistp521",
	} {
		line := makeKey(t, algorithm, 1, "laptop")
		key, err := ParsePublicKey(line)
		if err != nil {
			t.Fatalf("%s: %v", algorithm, err)
		}
		if key.Type != algorithm {
			t.Fatalf("%s: 读回来的类型是 %q", algorithm, key.Type)
		}
		if key.Comment != "laptop" {
			t.Fatalf("%s: 注释是 %q", algorithm, key.Comment)
		}
		if !strings.HasPrefix(key.Fingerprint, "SHA256:") || len(key.Fingerprint) < 16 {
			t.Fatalf("%s: 指纹形状不对 %q", algorithm, key.Fingerprint)
		}
		if key.Line() == "" {
			t.Fatalf("%s: 还原出来的行是空的", algorithm)
		}
	}
}

func TestParsePublicKeyRefusesBadShapes(t *testing.T) {
	good := makeKey(t, "ssh-ed25519", 7, "")
	fields := strings.Fields(good)
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"空", "   ", "不能为空"},
		{"只有一段", "ssh-ed25519", "两段"},
		{"带命令选项", `command="rm -rf /" ` + good, "不支持的公钥类型"},
		{"不认识的非对称算法", "ssh-dss " + fields[1], "不支持的公钥类型"},
		{"base64 坏掉", "ssh-ed25519 !!!not-base64!!!", "base64"},
		{"行首类型与主体不符", "ssh-rsa " + fields[1], "必须一致"},
		{"换行注入", good + "\n" + good, "控制字符"},
		{"带 NUL", good + "\x00", "控制字符"},
		{"主体太短", "ssh-ed25519 " + base64.StdEncoding.EncodeToString([]byte{0, 0}), "太短"},
		{"长度字段越界", "ssh-ed25519 " + base64.StdEncoding.EncodeToString([]byte{0, 0, 0, 99, 'a'}), "长度字段"},
	}
	for _, c := range cases {
		_, err := ParsePublicKey(c.raw)
		if err == nil {
			t.Fatalf("%s: 期望拒绝，实际通过", c.name)
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: 报错里没提到 %q，实际是 %v", c.name, c.want, err)
		}
	}
}

func TestParsePublicKeyIsStableOnFingerprint(t *testing.T) {
	line := makeKey(t, "ssh-ed25519", 3, "a")
	other := makeKey(t, "ssh-ed25519", 3, "b")
	a, err := ParsePublicKey(line)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ParsePublicKey(other)
	if err != nil {
		t.Fatal(err)
	}
	if a.Fingerprint != b.Fingerprint {
		t.Fatalf("同一把钥匙换了注释就换了指纹：%q vs %q", a.Fingerprint, b.Fingerprint)
	}
	if !SameKey(a, b) {
		t.Fatal("SameKey 判成不是同一把")
	}
	c, err := ParsePublicKey(makeKey(t, "ssh-ed25519", 4, "a"))
	if err != nil {
		t.Fatal(err)
	}
	if SameKey(a, c) {
		t.Fatal("不同的钥匙被判成同一把")
	}
}

func TestApplyKeysKeepsForeignLinesByteForByte(t *testing.T) {
	foreign := "ssh-rsa AAAAfake human@host\n# 我自己的注释\nfrom=\"10.0.0.1\" ssh-ed25519 AAAAhalf\n"
	key, err := ParsePublicKey(makeKey(t, "ssh-ed25519", 9, "upanel-key"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := ApplyKeys([]byte(foreign), []PublicKey{key})
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if !strings.HasPrefix(got, foreign) {
		t.Fatalf("别人的行被改动了：\n%q\n不再是前缀", got)
	}
	if !strings.Contains(got, blockBeginLine) || !strings.Contains(got, blockEndLine) {
		t.Fatalf("托管区标记没写上：\n%s", got)
	}
	if !strings.Contains(got, key.Line()) {
		t.Fatalf("公钥没写进托管区：\n%s", got)
	}

	// 再写一次，换一把钥匙：托管区被替换，别人的行还是逐字节不动。
	key2, err := ParsePublicKey(makeKey(t, "ssh-ed25519", 11, "second"))
	if err != nil {
		t.Fatal(err)
	}
	out2, err := ApplyKeys(out, []PublicKey{key2})
	if err != nil {
		t.Fatal(err)
	}
	got2 := string(out2)
	if !strings.HasPrefix(got2, foreign) {
		t.Fatalf("第二次写入改动了别人的行：\n%q", got2)
	}
	if strings.Contains(got2, key.Line()) {
		t.Fatalf("旧钥匙没被换掉：\n%s", got2)
	}
	if !strings.Contains(got2, key2.Line()) {
		t.Fatalf("新钥匙没写上：\n%s", got2)
	}
	if strings.Count(got2, blockBeginLine) != 1 {
		t.Fatalf("托管区出现了 %d 次：\n%s", strings.Count(got2, blockBeginLine), got2)
	}

	// 清空：托管区连同标记一起消失，别人的行原样留着。
	out3, err := ApplyKeys(out2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(out3) != foreign {
		t.Fatalf("清空之后应该逐字节回到只有别人的行：\n%q\nvs\n%q", string(out3), foreign)
	}
}

func TestApplyKeysAddsNewlineBeforeBlock(t *testing.T) {
	// 最后一行没有换行时直接追加会把钥匙行和标记粘成一行。
	key, err := ParsePublicKey(makeKey(t, "ssh-ed25519", 21, "x"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := ApplyKeys([]byte("ssh-rsa AAAf x"), []PublicKey{key})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(out), "\n")
	if lines[0] != "ssh-rsa AAAf x" {
		t.Fatalf("第一行被粘住了：%q", lines[0])
	}
	if lines[1] != blockBeginLine {
		t.Fatalf("第二行不是开始标记：%q", lines[1])
	}
}

func TestSplitManagedBlockRefusesDamagedMarkers(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"只有开始", blockBeginLine + "\n", "没有结束标记"},
		{"只有结束", blockEndLine + "\n", "前面没有开始标记"},
		{
			"两对",
			blockBeginLine + "\n" + blockEndLine + "\n" + blockBeginLine + "\n" + blockEndLine + "\n",
			"不止一对",
		},
		{"结束在前", blockEndLine + "\n" + blockBeginLine + "\n", "前面没有开始标记"},
		{
			"托管区里塞了不是钥匙的行",
			blockBeginLine + "\nrm -rf /\n" + blockEndLine + "\n",
			"不是一把合法的公钥",
		},
	}
	for _, c := range cases {
		_, _, _, _, err := SplitManagedBlock([]byte(c.content))
		if err == nil {
			t.Fatalf("%s: 期望拒绝，实际通过", c.name)
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: 报错里没提到 %q，实际是 %v", c.name, c.want, err)
		}
		if code := codeOf(err); code != NoticeSSHKeyBlockDamaged {
			t.Fatalf("%s: 码是 %q，期望 %q", c.name, code, NoticeSSHKeyBlockDamaged)
		}
	}
}

func TestSplitManagedBlockRoundTrips(t *testing.T) {
	key, err := ParsePublicKey(makeKey(t, "ssh-ed25519", 5, "round"))
	if err != nil {
		t.Fatal(err)
	}
	content, err := ApplyKeys(nil, []PublicKey{key})
	if err != nil {
		t.Fatal(err)
	}
	_, _, keys, found, err := SplitManagedBlock(content)
	if err != nil {
		t.Fatal(err)
	}
	if !found || len(keys) != 1 {
		t.Fatalf("读回来 found=%v keys=%d", found, len(keys))
	}
	if keys[0].Fingerprint != key.Fingerprint {
		t.Fatalf("指纹对不上：%q vs %q", keys[0].Fingerprint, key.Fingerprint)
	}
}

func codeOf(err error) string {
	var de *DefinitionError
	if e, ok := err.(*DefinitionError); ok {
		de = e
	}
	if de == nil {
		return ""
	}
	return de.Code
}
