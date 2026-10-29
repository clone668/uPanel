package shares

// 共享定义：校验、渲染、配置版本与目录路径（P08.1）。
//
// 渲染结果是这一阶段唯一的“配置”。它的 sha256 就是 CONTRACTS.md 第 7 节说的
// 配置哈希（配置版本）：同一份定义永远渲染出同一串字节，不同定义则几乎必然不同。
// P08.2 会把这串字节写进托管 include 文件；P08.1 只算、只存、只展示，且在任何
// 返回结构里都把 config_applied 留成 false。

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/netip"
	"path"
	"sort"
	"strings"
)

// 共享名的形状。
//
// 比 Samba 宽松的接受范围收紧了几档，理由是每一档收紧都对应一个真实的坑：
//   - 只允许字母数字、空格、点、下划线、连字符：方括号会让 Samba 把名字解释成
//     段名的一部分，引号与反斜杠会被 ini 解析吃掉；
//   - 首字符必须是字母或数字：以空格/点/连字符开头的段名在界面上几乎看不见；
//   - 不许以空格或点结尾：Windows 客户端会把它裁掉，于是两台机器上的名字不一样；
//   - 不许出现 ".."：它在任何路径类语义里都不是一个“名字”。
//
// 另外单独拒绝 Samba 的保留段名（global/homes/printers…）：把共享命名为 global
// 不是“建了一个共享”，而是往 [global] 里追加键值，属于改全局配置。
const (
	maxShareNameLen = 64
	maxSharePathLen = 255
)

// reservedShareNames 是 Samba 自己占用的段名（小写比较）。
var reservedShareNames = map[string]bool{
	"global":   true,
	"homes":    true,
	"printers": true,
	"print$":   true,
	"ipc$":     true,
}

// DefinitionError 是一条定义校验失败。Code 来自阻塞码闭集，Message 给人读。
type DefinitionError struct {
	Code    string
	Message string
}

// Error 实现 error。
func (e *DefinitionError) Error() string { return e.Message }

func newDefError(code, format string, args ...any) *DefinitionError {
	return &DefinitionError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// ValidateName 校验共享名。
func ValidateName(name string) error {
	if name == "" {
		return newDefError(NoticeShareNameTaken, "共享名不能为空")
	}
	if len(name) > maxShareNameLen {
		return newDefError(NoticeShareNameTaken, "共享名过长（%d 字节，上限 %d）", len(name), maxShareNameLen)
	}
	if reservedShareNames[strings.ToLower(name)] {
		return newDefError(NoticeShareNameTaken, "共享名 %q 是 Samba 的保留段名，会改到全局配置", name)
	}
	if !isAlnum(name[0]) {
		return newDefError(NoticeShareNameTaken, "共享名必须以字母或数字开头，收到 %q", name)
	}
	if name[len(name)-1] == '-' {
		return newDefError(NoticeShareNameTaken, "共享名不能以连字符结尾，收到 %q", name)
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case isAlnum(c), c == '_', c == '-':
		default:
			return newDefError(NoticeShareNameTaken,
				"共享名只能包含字母、数字、下划线、连字符，位置 %d 是 %q", i, c)
		}
	}
	return nil
}

// ValidateRelativePath 校验共享目录相对卷根的路径。
//
// 空串是合法的：它表示“整个卷根”。这是单卷单共享时的常见用法，强行要求一个子目录
// 只会逼用户去建一个只用来当壳的目录。
func ValidateRelativePath(p string) error {
	if p == "" {
		return nil
	}
	if len(p) > maxSharePathLen {
		return newDefError(NoticeSharePathInvalid, "relative_path 过长（%d 字节，上限 %d）", len(p), maxSharePathLen)
	}
	if strings.HasPrefix(p, "/") {
		return newDefError(NoticeSharePathInvalid, "relative_path 必须是相对卷根的路径，不能以 / 开头")
	}
	if strings.ContainsAny(p, "\\") {
		return newDefError(NoticeSharePathInvalid, "relative_path 不能包含反斜杠")
	}
	for i := 0; i < len(p); i++ {
		if p[i] < 0x20 || p[i] == 0x7f {
			return newDefError(NoticeSharePathInvalid, "relative_path 不能包含控制字符")
		}
	}
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "":
			return newDefError(NoticeSharePathInvalid, "relative_path 不能有空段（多余的 /）")
		case ".", "..":
			return newDefError(NoticeSharePathInvalid, "relative_path 不能包含 %q 段", seg)
		}
	}
	return nil
}

// ValidateDefinition 校验整份共享定义（与卷、账号无关的那部分）。
//
// 与卷/账号有关的判决在 assess.go：那里看得到登记表与现场挂载状态。
func ValidateDefinition(d ShareDefinition) error {
	if err := ValidateName(d.Name); err != nil {
		return err
	}
	// 两个协议的**共有**部分先校验，再分头校验各自独有的部分：
	// 网段对两者都要合法（SMB 的 hosts allow 与 NFS 的客户端列表是同一套写法），
	// 但“必须有点名网段”只对 NFS 成立（SMB 不填就是不限，那是它自己的语义）。
	if d.VolumeID == "" {
		return newDefError(NoticeVolumeNotFound, "volume_id 不能为空：共享目录必须落在一个受管理卷里")
	}
	if err := ValidateRelativePath(d.RelativePath); err != nil {
		return err
	}
	for _, c := range d.AllowedCIDRs {
		if err := ValidateCIDR(c); err != nil {
			return err
		}
	}
	switch d.Protocol {
	case ProtocolSMB:
		if err := validateSMBDefinition(d); err != nil {
			return err
		}
	case ProtocolNFS:
		if err := ValidateNFSDefinition(d); err != nil {
			return err
		}
	default:
		return newDefError(NoticeProtocolUnsupported, "protocol 只能是 smb 或 nfs，收到 %q", d.Protocol)
	}
	return nil
}

// validateSMBDefinition 校验 SMB 独有的部分。
func validateSMBDefinition(d ShareDefinition) error {
	// nfs_options 出现在 smb 定义里是调用方搞错了协议。静默忽略会让页面显示
	// “已设置 root_squash”而实际什么都没做，所以拒绝。
	if d.NFSOptions != nil {
		return newDefError(NoticeNFSOptionsInvalid,
			"smb 共享不接受 nfs_options：那几个选项只对 NFS 导出有意义")
	}
	// smb_users 为空会让 Samba 退化成 guest 可访问（guests ok 由 guest ok 决定，
	// 但空 valid users 意味着没有任何人被授权——两种结果都不是我们要的）。
	// 因此这里直接拒绝：空名单不是“稍后再填”，是配置错误。
	if len(d.SMBUsers) == 0 {
		return newDefError(NoticeShareNoUsers, "smb_users 不能为空：没有授权账号的共享会退化成 guest 访问")
	}
	seen := make(map[string]bool, len(d.SMBUsers))
	for _, u := range d.SMBUsers {
		if u.AccountID == "" {
			return newDefError(NoticeAccountMissing, "smb_users 里有空的 account_id")
		}
		if seen[u.AccountID] {
			return newDefError(NoticeAccountMissing, "smb_users 里账号 %s 出现了两次", u.AccountID)
		}
		seen[u.AccountID] = true
		switch u.Access {
		case AccessRead, AccessWrite:
		default:
			return newDefError(NoticeAccountMissing, "access 只能是 read 或 write，收到 %q", u.Access)
		}
	}
	return nil
}

// ValidateCIDR 校验一条允许网段。
//
// 只接受 a.b.c.d/len 与 IPv6 的 : 写法，拒绝主机名与空串：Samba 的 hosts allow
// 会接受主机名，但那会引入一次 DNS 解析，而 DNS 结果不在这份配置的控制之内。
func ValidateCIDR(c string) error {
	s := strings.TrimSpace(c)
	if s == "" {
		return newDefError(NoticeSharePathInvalid, "allowed_cidrs 里不能有空串")
	}
	if s != c {
		return newDefError(NoticeSharePathInvalid, "allowed_cidrs 里不能有前后空格：%q", c)
	}
	if !strings.Contains(s, "/") {
		return newDefError(NoticeSharePathInvalid, "allowed_cidrs 里的 %q 缺少前缀长度", c)
	}
	// 用 netip 而不是自己数点号：前缀长度越界、IPv6 里塞 IPv4、带 zone 的地址
	// 这些情况只有解析器认得全。字符串检查放前面只是为了先把“明显不是 CIDR”的
	// 输入挡在解析器之外，报错更好懂。
	prefix, err := netip.ParsePrefix(s)
	if err != nil {
		return newDefError(NoticeSharePathInvalid, "allowed_cidrs 里的 %q 不是合法 CIDR：%v", c, err)
	}
	if prefix.Addr().Zone() != "" {
		return newDefError(NoticeSharePathInvalid, "allowed_cidrs 里的 %q 带了地址 zone（%q）", c, prefix.Addr().Zone())
	}
	// 主机位非零（10.0.0.5/24）在 iproute 里能用，在 hosts allow 里含义暧昧：
	// 它会被当成一整段还是单个地址取决于版本。要求写成网络地址，不留解释空间。
	if prefix.Addr() != prefix.Masked().Addr() {
		return newDefError(NoticeSharePathInvalid,
			"allowed_cidrs 里的 %q 主机位不为零，请写成网络地址 %s", c, prefix.Masked().String())
	}
	return nil
}

// cidrsCoverLoopback 报告这批允许网段里有没有一段**包含** 127.0.0.1。
//
// 用途只有一个：判断要不要提醒“Samba 对环回永远放行”。它不参与拦截，也不改变
// 渲染结果——名单写什么就渲染什么，是 Samba 自己在 hosts allow 里给 127.0.0.1
// 开了后门（见 man 5 smb.conf）。
func cidrsCoverLoopback(cidrs []string) bool {
	loop := netip.MustParseAddr("127.0.0.1")
	for _, c := range cidrs {
		p, err := netip.ParsePrefix(strings.TrimSpace(c))
		if err != nil {
			continue // 不合法的形状由 ValidateCIDR 报，这里不重复判
		}
		if p.Contains(loop) {
			return true
		}
	}
	return false
}

// resolvedAccess 是一条已解析名字的访问授权。
type resolvedAccess struct {
	Name   string
	Access string
}

// RenderBlock 把定义渲染成一段 Samba 配置。
//
// 只产出这一段共享自己的键值，**不产出 [global]**：全局加固（最低协议 SMB2、
// 禁用 guest、关 wide links）属于托管 include 文件里的全局段，由 P08.2 在接入时
// 一次性写好。把它塞进每个共享段会让人以为“删掉这个共享就恢复了默认全局设置”。
//
// 输出必须是确定性的：账号按名字排序，键的顺序固定。配置版本（ConfigHash）建立
// 在这个性质上。
func (d ShareDefinition) RenderBlock(mountPath string, userNames map[string]string) (string, error) {
	if err := ValidateDefinition(d); err != nil {
		return "", err
	}
	if !path.IsAbs(mountPath) {
		return "", newDefError(NoticeSharePathInvalid, "卷挂载点必须是绝对路径，收到 %q", mountPath)
	}
	dir, err := ShareDir(mountPath, d.RelativePath)
	if err != nil {
		return "", err
	}

	access := make([]resolvedAccess, 0, len(d.SMBUsers))
	for _, u := range d.SMBUsers {
		name := userNames[u.AccountID]
		if name == "" {
			return "", newDefError(NoticeAccountMissing,
				"账号 %s 没有可用的系统用户名，无法渲染共享配置", u.AccountID)
		}
		access = append(access, resolvedAccess{Name: name, Access: u.Access})
	}
	sort.Slice(access, func(i, j int) bool { return access[i].Name < access[j].Name })

	valid := make([]string, 0, len(access))
	writers := make([]string, 0, len(access))
	for _, a := range access {
		valid = append(valid, a.Name)
		if a.Access == AccessWrite && !d.ReadOnly {
			writers = append(writers, a.Name)
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "[%s]\n", d.Name)
	fmt.Fprintf(&b, "   path = %s\n", dir)
	fmt.Fprintf(&b, "   browseable = yes\n")
	fmt.Fprintf(&b, "   guest ok = no\n")
	if d.ReadOnly {
		fmt.Fprintf(&b, "   read only = yes\n")
	} else {
		fmt.Fprintf(&b, "   read only = no\n")
	}
	fmt.Fprintf(&b, "   follow symlinks = no\n")
	fmt.Fprintf(&b, "   wide links = no\n")
	fmt.Fprintf(&b, "   valid users = %s\n", strings.Join(valid, ","))
	if len(writers) > 0 {
		fmt.Fprintf(&b, "   write list = %s\n", strings.Join(writers, ","))
	}
	if len(d.AllowedCIDRs) > 0 {
		cidrs := append([]string(nil), d.AllowedCIDRs...)
		sort.Strings(cidrs)
		fmt.Fprintf(&b, "   hosts allow = %s\n", strings.Join(cidrs, " "))
	}
	return b.String(), nil
}

// ConfigHashOf 返回渲染结果的 sha256（十六进制）。
//
// 这就是 CONTRACTS.md 第 7 节说的“配置哈希/配置版本”。它只对**配置内容**做哈希，
// 不含时间戳与版本计数器：同一份定义无论应用多少次，哈希都一样——否则“配置有没有
// 漂移”这个问题就永远只能回答“漂了”。
func ConfigHashOf(rendered string) string {
	sum := sha256.Sum256([]byte(rendered))
	return hex.EncodeToString(sum[:])
}

// ShareDir 把卷挂载点与相对路径拼成共享目录的绝对路径。
//
// 这里是**词法**拼接，不碰文件系统；符号链接与越界检查在 assess.go 里靠 Lstat
// 逐段做（见 CheckPathInsideVolume）。分成两层是因为预检必须能在目录还不存在时
// 就说出“将要创建在哪里”，而那时没有任何东西可以 Lstat。
func ShareDir(mountPath, relativePath string) (string, error) {
	if !path.IsAbs(mountPath) {
		return "", newDefError(NoticeSharePathInvalid, "卷挂载点必须是绝对路径，收到 %q", mountPath)
	}
	if err := ValidateRelativePath(relativePath); err != nil {
		return "", err
	}
	cleanMount := path.Clean(mountPath)
	if relativePath == "" {
		return cleanMount, nil
	}
	joined := path.Join(cleanMount, relativePath)
	// path.Join 已经消化掉 // 与 . ；这里再核一次前缀，防止将来某次改动把
	// 校验顺序调换之后悄悄放出一条越界路径。
	if joined != cleanMount && !strings.HasPrefix(joined, cleanMount+"/") {
		return "", newDefError(NoticeSharePathEscapes,
			"共享目录 %q 不在卷挂载点 %q 之内", joined, cleanMount)
	}
	return joined, nil
}

// isAlnum 判断 ASCII 字母或数字。
func isAlnum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}
