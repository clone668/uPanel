package proxies

// basic auth 的账号文件（P10.2）。
//
// 三件事必须同时成立，缺一条这批账号就是假的：
//
//  1. **明文不经过 argv、不落日志、不进库。** 密码从代理的秘密存储按 secret_ref 取出来，
//     在这台机器上直接算成哈希，只把哈希写进文件。syscmd 不提供“把字符串当命令”的
//     入口，这里也不需要任何外部命令——哈希用 Go 自己算。
//  2. **哈希必须被 nginx 认。** nginx 的 auth_basic_user_file 支持 apr1（`$apr1$…`）、
//     老式 crypt、`{SHA}` 与明文；这里只产生 apr1。它是 Apache 的 md5-crypt，
//     在 nginx 上按字面实现，不依赖系统里有没有 openssl/htpasswd。
//  3. **同样的输入必须产出同样的字节。** 盐由站点 id 与账号名派生，不用随机数：
//     否则每次应用都会写出一份内容不同、语义相同的文件，幂等判断立刻就失效了
//     （每次都要备份、每次都要 reload，而这次 reload 什么也没改变）。
//
// 文件权限由 nginx 的 worker 用户决定（见 applyrun.go 的 authFileOwner：
// 读得到 `user <name>;` 就写 0640 + 属组给 worker，读不到才退回 0644 并报告）。

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// apr1Alphabet 是 apr1 的 base64 变体（crypt 的经典字母表）。
const apr1Alphabet = "./0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// APR1 计算 Apache 的 apr1 md5-crypt 哈希，返回 `$apr1$<salt>$<hash>`。
//
// 算法照抄 Apache 的 apr_md5_crypt（也是 openssl passwd -apr1 的实现）。
// 不引第三方库的理由：这是一个四十行的纯函数，而一个“密码哈希”的依赖一旦出问题，
// 症状是“所有账号都登不上”或更糟。加密库的密码学强度不是这里的问题——apr1 本身
// 就只是“别把密码明文写在文件里”这个级别的保护，面板不假装它更强。
func APR1(password, salt string) string {
	salt = normalizeSalt(salt)
	pw := []byte(password)

	// 第一轮：password + magic + salt。
	h := md5.New()
	h.Write(pw)
	h.Write([]byte("$apr1$"))
	h.Write([]byte(salt))

	// 第二轮：password + salt + password，作为“填充块”循环塞进第一轮的上下文。
	alt := md5.New()
	alt.Write(pw)
	alt.Write([]byte(salt))
	alt.Write(pw)
	altSum := alt.Sum(nil)

	h = md5.New()
	h.Write(pw)
	h.Write([]byte("$apr1$"))
	h.Write([]byte(salt))
	for pl := len(pw); pl > 0; pl -= 16 {
		n := pl
		if n > 16 {
			n = 16
		}
		h.Write(altSum[:n])
	}
	// 密码长度的二进制位：从最低位往上，1 塞 NUL、0 塞密码首字节。这一步是
	// md5-crypt 的设计，不是可选的装饰——少了它，结果与 openssl 对不上。
	for i := len(pw); i > 0; i >>= 1 {
		if i&1 != 0 {
			h.Write([]byte{0})
		} else {
			h.Write(pw[:1])
		}
	}
	digest := h.Sum(nil)

	// 1000 轮拉伸。
	for i := 0; i < 1000; i++ {
		r := md5.New()
		if i&1 != 0 {
			r.Write(pw)
		} else {
			r.Write(digest)
		}
		if i%3 != 0 {
			r.Write([]byte(salt))
		}
		if i%7 != 0 {
			r.Write(pw)
		}
		if i&1 != 0 {
			r.Write(digest)
		} else {
			r.Write(pw)
		}
		digest = r.Sum(nil)
	}

	// 输出顺序是 md5-crypt 规定的置换，不是自然的 0..15。
	var b strings.Builder
	b.WriteString("$apr1$")
	b.WriteString(salt)
	b.WriteByte('$')
	order := [][3]int{
		{0, 6, 12}, {1, 7, 13}, {2, 8, 14}, {3, 9, 15}, {4, 10, 5},
	}
	for _, o := range order {
		v := uint(digest[o[0]])<<16 | uint(digest[o[1]])<<8 | uint(digest[o[2]])
		b.WriteString(to64(v, 4))
	}
	b.WriteString(to64(uint(digest[11]), 2))
	return b.String()
}

// normalizeSalt 把盐截到 8 个字符以内，并挡掉 apr1 的分隔符。
//
// 空盐在 Apache 里表示“随机生成”，这里不允许：随机会毁掉幂等。
func normalizeSalt(salt string) string {
	salt = strings.TrimPrefix(salt, "$apr1$")
	if i := strings.IndexByte(salt, '$'); i >= 0 {
		salt = salt[:i]
	}
	if len(salt) > 8 {
		salt = salt[:8]
	}
	if salt == "" {
		// 调用方不该走到这里（deriveSalt 永远非空）；留一个确定值而不是随机的。
		return "upanel00"
	}
	return salt
}

// to64 按 crypt 的顺序写出 n 的低 6 位一组。
func to64(v uint, n int) string {
	out := make([]byte, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, apr1Alphabet[v&0x3f])
		v >>= 6
	}
	return string(out)
}

// deriveSalt 由站点 id 与账号名派生一个稳定的 8 字符盐。
//
// 为什么不用随机：盐的作用是“两个相同密码的人拿到不同哈希”，而这里每个账号的盐
// 只依赖它自己的名字与站点，仍然满足这一点（同名不同站点、同站点不同名字，盐都不同）。
// 代价是盐可预测——对 apr1 来说没有实际损失，因为 apr1 的抗爆破能力本来就靠 1000 轮
// 拉伸，不靠盐的保密性。收益是整份文件逐字节可复现。
func deriveSalt(siteID, name string) string {
	sum := sha256.Sum256([]byte(siteID + "\x00" + name))
	return hex.EncodeToString(sum[:4]) // 8 个十六进制字符
}

// RenderAuthFile 把一组账号渲染成 htpasswd 内容。
//
// passwords 是 name → 明文密码。函数返回内容与文件应有的字节；**不返回哈希表**：
// 调用方只需要写文件，多一个可以拿去比对的哈希列表就多一条泄漏路径。
//
// 顺序按账号名排序：定义里的顺序来自用户输入，而用户改一下顺序不该触发一次重写。
func RenderAuthFile(siteID string, users []BasicAuthUser, passwords map[string]string) (string, error) {
	if len(users) == 0 {
		return "", fmt.Errorf("没有账号可写")
	}
	type entry struct{ name, hash string }
	entries := make([]entry, 0, len(users))
	seen := make(map[string]struct{}, len(users))
	for _, u := range users {
		if !basicAuthNamePattern.MatchString(u.Name) {
			return "", fmt.Errorf("账号名 %q 形状不合法", u.Name)
		}
		if _, dup := seen[u.Name]; dup {
			return "", fmt.Errorf("账号名 %q 重复", u.Name)
		}
		seen[u.Name] = struct{}{}
		pw := passwords[u.Name]
		if pw == "" {
			return "", fmt.Errorf("账号 %q 的密码为空", u.Name)
		}
		entries = append(entries, entry{name: u.Name, hash: APR1(pw, deriveSalt(siteID, u.Name))})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })

	var b strings.Builder
	for _, e := range entries {
		b.WriteString(e.name)
		b.WriteString(":")
		b.WriteString(e.hash)
		b.WriteString("\n")
	}
	return b.String(), nil
}
