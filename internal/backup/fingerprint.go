package backup

// SFTP 主机密钥指纹（P12.1；CONTRACTS.md 第 12 节：主机密钥必须显式登记，
// 禁止 StrictHostKeyChecking=no）。
//
// 指纹是管理员在登记仓库时**手工抄下来**的那一行（`ssh-keygen -lf` 的第三列）。
// 因此这一层只做两件事：把它解析成一个可比较的值，并拿它去比较“这次真正连到的
// 那把钥匙”。比较失败没有任何回退路径——不回退到 TOFU，不回退到 /root/.ssh/known_hosts，
// 也不回退到“先连上再说”。

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"
)

// Fingerprint 是一条已经解析过的主机密钥指纹。
type Fingerprint struct {
	// Algorithm 是 "SHA256" 或 "MD5"。
	Algorithm string
	// Value 是规范化后的摘要文本（SHA256 是无填充 base64，MD5 是冒号分隔的小写十六进制）。
	Value string
}

// ErrFingerprint 表示指纹文本无法解析。
var ErrFingerprint = errors.New("backup: 主机密钥指纹不合法")

// ParseFingerprint 解析 `SHA256:<base64>` 或 `MD5:aa:bb:…` 形式的指纹。
//
// 两种都收，但含义不同：SHA256 是 OpenSSH 7.2 起的默认输出，也是 RESTIC/ssh
// 实际协商时我们比对的那一份；MD5 只为兼容旧文档里抄下来的值。**不**接受裸摘要：
// 没有算法名就无法确定该用哪种长度比较，猜一个等于没比对。
func ParseFingerprint(s string) (Fingerprint, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Fingerprint{}, fmt.Errorf("%w：不能为空", ErrFingerprint)
	}
	algorithm, value, found := strings.Cut(s, ":")
	if !found {
		return Fingerprint{}, fmt.Errorf("%w：缺少算法前缀（应为 SHA256:… 或 MD5:…）", ErrFingerprint)
	}
	switch strings.ToUpper(algorithm) {
	case "SHA256":
		v := strings.TrimSuffix(value, "=")
		raw, err := base64.RawStdEncoding.DecodeString(v)
		if err != nil {
			return Fingerprint{}, fmt.Errorf("%w：SHA256 摘要不是合法 base64", ErrFingerprint)
		}
		if len(raw) != sha256.Size {
			return Fingerprint{}, fmt.Errorf("%w：SHA256 摘要是 %d 字节，应为 %d", ErrFingerprint, len(raw), sha256.Size)
		}
		return Fingerprint{Algorithm: "SHA256", Value: base64.RawStdEncoding.EncodeToString(raw)}, nil
	case "MD5":
		parts := strings.Split(strings.ToLower(value), ":")
		if len(parts) != 16 {
			return Fingerprint{}, fmt.Errorf("%w：MD5 摘要应为 16 组冒号分隔的十六进制（收到 %d 组）", ErrFingerprint, len(parts))
		}
		for _, p := range parts {
			if len(p) != 2 {
				return Fingerprint{}, fmt.Errorf("%w：MD5 的每一组应是两位十六进制", ErrFingerprint)
			}
			if _, err := hex.DecodeString(p); err != nil {
				return Fingerprint{}, fmt.Errorf("%w：MD5 含非十六进制字符", ErrFingerprint)
			}
		}
		return Fingerprint{Algorithm: "MD5", Value: strings.Join(parts, ":")}, nil
	default:
		return Fingerprint{}, fmt.Errorf("%w：不支持的算法 %q（只支持 SHA256 与 MD5）", ErrFingerprint, algorithm)
	}
}

// String 返回规范化后的指纹文本。
func (f Fingerprint) String() string { return f.Algorithm + ":" + f.Value }

// Matches 判断这把公钥是否就是登记的那一把。
//
// 比较用 hmac.Equal（等长常量时间）：指纹本身不是秘密，但“比较失败提前返回”
// 会让一台机器上的进程能测出前缀匹配到第几个字节——那是把一把钥匙的摘要变成
// 一次一位的预言机，没有任何理由留这个口子。
func (f Fingerprint) Matches(pub ssh.PublicKey) bool {
	if pub == nil {
		return false
	}
	switch f.Algorithm {
	case "SHA256":
		raw, err := base64.RawStdEncoding.DecodeString(f.Value)
		if err != nil {
			return false
		}
		sum := sha256.Sum256(pub.Marshal())
		return hmac.Equal(raw, sum[:])
	case "MD5":
		// ssh.FingerprintLegacyMD5 返回的是**不带算法前缀**的十六进制组（aa:bb:…），
		// 而 ParseFingerprint 存下的正是这一份，所以直接比这两串。早先这里比的是
		// f.String()（带 "MD5:" 前缀），于是一把正确的主机密钥永远比不中——
		// 表现为“所有 MD5 指纹都失配”，而不是一个安全的拒绝。
		sum := ssh.FingerprintLegacyMD5(pub)
		return hmac.Equal([]byte(sum), []byte(f.Value))
	}
	return false
}

// SHA256 返回这把公钥的 SHA256 指纹（OpenSSH 默认形式）。
func SHA256(pub ssh.PublicKey) string { return ssh.FingerprintSHA256(pub) }

// MD5 返回这把公钥的 MD5 指纹（兼容旧文档），**带 MD5: 前缀**，可直接喂给
// ParseFingerprint。ssh.FingerprintLegacyMD5 自己不带前缀，两种形状混用正是
// Matches 里那个比较缺陷的来源，所以对外只保留一种。
func MD5(pub ssh.PublicKey) string { return "MD5:" + ssh.FingerprintLegacyMD5(pub) }

// KnownHostsLine 生成一行 known_hosts 内容。
//
// 非 22 端口必须写成 `[host]:port`——这是 OpenSSH 的查找规则，写错的话 ssh 会
// 认为“这台主机不在 known_hosts 里”，而 StrictHostKeyChecking=yes 下那就等于
// 连不上：**失败关闭**，正是我们要的那一侧。
func KnownHostsLine(host string, port int, pub ssh.PublicKey) string {
	if pub == nil {
		return ""
	}
	h := host
	if port != 0 && port != DefaultSFTPPort {
		h = "[" + host + "]:" + fmt.Sprint(port)
	}
	// 行里只放“主机 + 类型 + 公钥”，不带注释：注释是远端给的自由文本，
	// 没有理由让它进我们的文件。
	return h + " " + pub.Type() + " " + base64.StdEncoding.EncodeToString(pub.Marshal())
}
