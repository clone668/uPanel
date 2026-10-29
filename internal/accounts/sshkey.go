package accounts

// SSH 公钥：格式校验、指纹与 authorized_keys 里那块**受管理区域**（P14.5）。
//
// 这一版只做两件事：认出“这是一把什么钥匙”，以及在不动别人的行的前提下，把托管区
// 整块换成新内容。两件事都是纯函数——真正的读写在 engine 里，因为那要碰文件系统。
//
// 为什么要有托管区：authorized_keys 是人和面板共用的文件。全份重写会吃掉人手写的
// 钥匙（那是把人锁在门外的做法），只在末尾追加会永远删不掉旧钥匙。所以划一块用
// 行标记包起来的区域，面板只碰区域里面的字节，区域外面逐字节不动——PLAN.md 第 19
// 节要求的“原子更新面板管理的 authorized_keys 内容”就是这个意思。

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// 托管区的行标记。
//
// 用 `>>>` / `<<<` 这两个前缀是刻意的：它们不是 authorized_keys 的合法指令，也不像
// 任何一条正常钥匙的开头，所以“标记被当成钥匙读进去”这种事不可能发生。
const (
	blockBeginLine = "# >>> upanel 托管区（整块由面板重写，手改会被覆盖）>>>"
	blockEndLine   = "# <<< upanel 托管区 <<<"
)

// MaxKeysPerAccount 是一个账号能挂的公钥数量上限。
//
// 10 不是技术限制，是“一个人有几台机器”的量级。它存在是为了让一次误导入（比如把
// 整份 team_keys 贴进来）在预检就被拒，而不是变成一份 300 行的 authorized_keys。
const MaxKeysPerAccount = 10

// maxKeyLineLen 是一行公钥的长度上限。
//
// 4096 位 RSA 的一行约 740 字节；8 KiB 是给“带注释”留的余量，同时保证一行不会
// 突破 sshd 自己的行长度上限。
const maxKeyLineLen = 8192

// supportedKeyTypes 是接受的公钥类型（闭集）。
//
// 不收 sk-*（硬件密钥的 attestation 附件会让形状变复杂）、不收 ssh-dss（sshd 默认
// 已禁用）、不收证书（ssh-*-cert-v01@openssh.com）：这一版要做的是“给人自己的机器
// 开一把钥匙”，不是做 CA。
var supportedKeyTypes = map[string]bool{
	"ssh-ed25519":         true,
	"ssh-rsa":             true,
	"ecdsa-sha2-nistp256": true,
	"ecdsa-sha2-nistp384": true,
	"ecdsa-sha2-nistp521": true,
}

// PublicKey 是一把校验过的公钥。
type PublicKey struct {
	// Type 是算法名（闭集里的一个）。
	Type string
	// Blob 是 base64 解码后的 SSH 线格式主体。
	Blob []byte
	// Comment 是行尾注释（可空）。它只用于展示与区分“哪台机器”。
	Comment string
	// Fingerprint 是 `SHA256:…` 形式的指纹，用作去重与展示的稳定标识。
	Fingerprint string
}

// BlobText 返回 base64 的密钥体（落库时用的就是这一份文本）。
func (k PublicKey) BlobText() string { return base64.StdEncoding.EncodeToString(k.Blob) }

// KeyFromRecord 把登记表里的一行还原成可以写进文件的公钥。
//
// 解码失败不静默：一条读不懂的公钥记录会让 authorized_keys 里出现一行 sshd 会
// 忽略的内容，而页面照样显示“已配置”。
func KeyFromRecord(rec SSHKeyRecord) (PublicKey, error) {
	blob, err := base64.StdEncoding.DecodeString(rec.Blob)
	if err != nil {
		return PublicKey{}, fmt.Errorf("公钥记录 %s（%s）的主体不是合法 base64：%w", rec.ID, rec.Fingerprint, err)
	}
	return PublicKey{Type: rec.Type, Blob: blob, Comment: rec.Comment, Fingerprint: rec.Fingerprint}, nil
}

// Line 返回可以写进 authorized_keys 的那一行（不含换行）。
func (k PublicKey) Line() string {
	if k.Comment == "" {
		return k.Type + " " + base64.StdEncoding.EncodeToString(k.Blob)
	}
	return k.Type + " " + base64.StdEncoding.EncodeToString(k.Blob) + " " + k.Comment
}

// ParsePublicKey 校验一行公钥并算出指纹。
//
// 校验分三层，每一层都对应一种真实的坏输入：
//  1. 形态：不能有换行/回车/NUL（一行注入第二行，或者把 authorized_keys 写坏）；
//  2. 算法名：闭集（拼错的算法名会被 sshd 静默忽略，人以为配好了）；
//  3. 主体：base64 能解开，且**线格式里的算法名与行首那个名字一致**。第三条最要紧：
//     `ssh-ed25519 <一段 rsa 的 blob>` 这种写法人看不出来，sshd 也会照读不误，
//     但“页面上写着 ed25519、实际是 rsa”这种事没有任何一层会报错。
func ParsePublicKey(raw string) (PublicKey, error) {
	line := strings.TrimSpace(raw)
	if line == "" {
		return PublicKey{}, newDefError(NoticeSSHKeyInvalid, "公钥不能为空")
	}
	if len(line) > maxKeyLineLen {
		return PublicKey{}, newDefError(NoticeSSHKeyInvalid,
			"公钥过长（%d 字节，上限 %d）", len(line), maxKeyLineLen)
	}
	for _, r := range line {
		if r == utf8.RuneError || (unicode.IsControl(r) && r != '\t') {
			return PublicKey{}, newDefError(NoticeSSHKeyInvalid,
				"公钥里不能有控制字符（收到 %q）；一行一把，粘贴时不要带多余的换行", r)
		}
	}
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return PublicKey{}, newDefError(NoticeSSHKeyInvalid,
			"公钥至少要「算法名 + base64 主体」两段，收到 %d 段", len(fields))
	}
	// 行首必须是算法名。`command="…" ssh-ed25519 AAAA…` 这种带选项的写法在这里
	// 一律拒绝：选项能强制命令，等于一条通往任意命令执行的旁路。
	if !supportedKeyTypes[fields[0]] || fields[0] == "" {
		return PublicKey{}, newDefError(NoticeSSHKeyInvalid,
			"不支持的公钥类型 %q（支持 ssh-ed25519、ssh-rsa、ecdsa-sha2-nistp256/384/521）；"+
				"带前置选项（command=、from= 等）的行也不接受", fields[0])
	}
	blob, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil {
		return PublicKey{}, newDefError(NoticeSSHKeyInvalid,
			"公钥主体不是合法的 base64：%v", err)
	}
	if len(blob) < 4 {
		return PublicKey{}, newDefError(NoticeSSHKeyInvalid, "公钥主体太短，读不出算法名")
	}
	n := binary.BigEndian.Uint32(blob[:4])
	if n == 0 || int(n) > len(blob)-4 {
		return PublicKey{}, newDefError(NoticeSSHKeyInvalid,
			"公钥主体的算法名长度字段不对（%d，主体共 %d 字节）", n, len(blob))
	}
	inner := string(blob[4 : 4+n])
	if inner != fields[0] {
		return PublicKey{}, newDefError(NoticeSSHKeyInvalid,
			"行首写的是 %s，主体里的算法名却是 %s；这两个必须一致", fields[0], inner)
	}
	comment := strings.Join(fields[2:], " ")
	if utf8.RuneCountInString(comment) > 128 {
		return PublicKey{}, newDefError(NoticeSSHKeyInvalid,
			"公钥注释过长（%d 字符，上限 128）", utf8.RuneCountInString(comment))
	}
	sum := sha256.Sum256(blob)
	return PublicKey{
		Type:        fields[0],
		Blob:        blob,
		Comment:     comment,
		Fingerprint: "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:]),
	}, nil
}

// SplitManagedBlock 把 authorized_keys 的内容切成“托管区之前 / 托管区里的公钥 /
// 托管区之后”三段，并报告托管区是否存在。
//
// 标记只认**整行**，且必须恰好一对。零对 = 还没有托管区（合法，第一次写入）；
// 一对我认；两对以上、只有一半、结束在开始之前——一律报错，一个字都不改。
// 这里不修、不猜：托管区坏了意味着有人手改过，面板要让人来看，而不是替人决定。
func SplitManagedBlock(content []byte) (before, after []byte, keys []PublicKey, found bool, err error) {
	text := string(content)
	lines := strings.SplitAfter(text, "\n")
	beginLine, endLine := -1, -1    // 行号（从 0 起，仅用于报错）
	beginStart, bodyStart := -1, -1 // 字节偏移：开始标记那一行的行首 / 托管区正文起点
	endStart, endStop := -1, -1
	offset := 0
	for i, line := range lines {
		trimmed := strings.TrimRight(line, "\r\n")
		switch trimmed {
		case blockBeginLine:
			if beginLine >= 0 {
				return nil, nil, nil, false, newDefError(NoticeSSHKeyBlockDamaged,
					"authorized_keys 里出现了不止一对托管区标记（第 %d 行又见到开始标记）", i+1)
			}
			if endLine >= 0 {
				return nil, nil, nil, false, newDefError(NoticeSSHKeyBlockDamaged,
					"authorized_keys 里的结束标记（第 %d 行）在开始标记之前", endLine+1)
			}
			beginLine = i
			beginStart = offset
			bodyStart = offset + len(line)
			endStart, endStop = -1, -1
		case blockEndLine:
			if beginLine < 0 {
				return nil, nil, nil, false, newDefError(NoticeSSHKeyBlockDamaged,
					"authorized_keys 第 %d 行是结束标记，但前面没有开始标记", i+1)
			}
			if endLine >= 0 {
				return nil, nil, nil, false, newDefError(NoticeSSHKeyBlockDamaged,
					"authorized_keys 里出现了不止一对托管区标记")
			}
			endLine = i
			endStart = offset
			endStop = offset + len(line)
		}
		offset += len(line)
	}
	if beginLine < 0 && endLine < 0 {
		return content, nil, nil, false, nil
	}
	if endLine < 0 {
		return nil, nil, nil, false, newDefError(NoticeSSHKeyBlockDamaged,
			"authorized_keys 里的托管区从第 %d 行开始，但没有结束标记", beginLine+1)
	}
	inner := text[bodyStart:endStart]
	for i, raw := range strings.Split(inner, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, kerr := ParsePublicKey(line)
		if kerr != nil {
			return nil, nil, nil, false, newDefError(NoticeSSHKeyBlockDamaged,
				"托管区里第 %d 行不是一把合法的公钥（%v）；托管区坏了，面板不会整块覆盖它",
				i+1, kerr)
		}
		keys = append(keys, key)
	}
	return []byte(text[:beginStart]), []byte(text[endStop:]), keys, true, nil
}

// ApplyKeys 返回“托管区里恰好是这些公钥、其余字节一个不变”的新内容。
//
// 已有托管区就整块替换（连同标记一起重写）；没有就追加到文件末尾。追加时保证托管区
// 之前有一个换行——authorized_keys 最后一行没有换行时直接接上去会把两行粘成一行，
// 那把钥匙和标记就都废了。
func ApplyKeys(content []byte, keys []PublicKey) ([]byte, error) {
	before, after, _, found, err := SplitManagedBlock(content)
	if err != nil {
		return nil, err
	}
	base := string(content)
	if found {
		// 已有托管区：把两头接起来就等于“把托管区连标记一起摘掉”。
		base = string(before) + string(after)
	}
	if len(keys) == 0 {
		// 一把钥匙都不剩就整块拆掉，不留一对空标记：空标记会让下一次读取
		// 以为“面板正在管这个文件”，而它其实什么都没管。
		return []byte(base), nil
	}
	if base != "" && !strings.HasSuffix(base, "\n") {
		base += "\n"
	}
	var out strings.Builder
	out.WriteString(base)
	out.WriteString(blockBeginLine)
	out.WriteString("\n")
	for _, k := range keys {
		out.WriteString(k.Line())
		out.WriteString("\n")
	}
	out.WriteString(blockEndLine)
	out.WriteString("\n")
	return []byte(out.String()), nil
}

// SameKey 判断两把公钥是不是同一把（按主体字节，不按注释）。
func SameKey(a, b PublicKey) bool {
	if a.Type != b.Type || len(a.Blob) != len(b.Blob) {
		return false
	}
	for i := range a.Blob {
		if a.Blob[i] != b.Blob[i] {
			return false
		}
	}
	return true
}

// DescribeKey 是给审计摘要用的一句话（指纹 + 注释，不含整份公钥）。
func DescribeKey(k PublicKey) string {
	if k.Comment == "" {
		return fmt.Sprintf("%s %s", k.Type, k.Fingerprint)
	}
	return fmt.Sprintf("%s %s（%s）", k.Type, k.Fingerprint, k.Comment)
}
