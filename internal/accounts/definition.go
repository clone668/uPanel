package accounts

// 受管理 Linux 账号的定义、校验与系统侧的取值（P14.5）。
//
// 这一文件全是**纯函数与常量**：没有 store、没有命令、没有文件。理由与共享那半边
// （internal/shares/definition.go）一样——定义校验是“接受的形状”的唯一出处，执行器
// 与预检都从这里读判据；它必须能在没有 root、没有那台机器的环境里被逐条断言。
//
// 与 share 账号的区别（PLAN.md 第 19 节）：这一版管的是**面板创建的普通账号**与
// **面板创建的共享账号**两种，已有系统账号只读，root 与系统 UID 一个字节都不碰。

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// 账号名的形状。与 internal/shares/account.go 的那一份**必须逐字一致**，但这里没有
// 直接复用：那一份是为“共享账号”写的，校验里混着 uid < 1000、nologin 这类共享专用
// 前提。两处各有各的 Tests 表，internal/accounts/name_test.go 里有一条交叉用例把两边
// 的名字形状判定拉到同一张表上比对，任何一边单独放宽都会被它拦下。
const (
	maxAccountNameLen   = 32
	maxDisplayNameRunes = 64
)

// ShellProfile 是“这个人能不能登录、登录之后是什么”。
type ShellProfile string

const (
	// ProfileDisabled 是禁用登录：shell 指到 nologin。共享账号只能是这一档。
	ProfileDisabled ShellProfile = "disabled"
	// ProfileRestricted 是受限登录：shell 指到 rbash。
	//
	// 为什么不是“随便填一个 shell”：PLAN.md 第 19 节禁止提交任意 shell 路径。
	// 一条能写 /bin/bash 的接口等于把“面板账号”变成“能登录的 root 候选”，而
	// 面板并不管密码策略、会话审计与 TOTP——这些它管不到的地方它就不能放行。
	ProfileRestricted ShellProfile = "restricted"
)

// Shell 返回这一档 profile 在系统上的 shell 路径。
func (p ShellProfile) Shell() string {
	if p == ProfileRestricted {
		return ShellRestricted
	}
	return ShellDisabled
}

const (
	// ShellDisabled 是禁用登录的 shell。
	ShellDisabled = "/usr/sbin/nologin"
	// ShellRestricted 是受限登录的 shell（rbash：不能改 PATH、不能 cd 出去、不能
	// 调带斜杠的命令）。
	ShellRestricted = "/bin/rbash"
)

// Type 是账号种类。
type Type string

const (
	// TypeRegular 是普通账号：uid 落在普通用户段，可以给受限 shell。
	TypeRegular Type = "regular"
	// TypeShare 是共享账号：系统段 uid、nologin、home 指向 /nonexistent。
	//
	// account.apply 接受这个类型只是为了**指向 share.account.apply**，不在这个包
	// 里执行：共享账号的凭据在 Samba 的 passdb 里，两条写路径各有各的读回核验，
	// 合成一条会让“谁写的”说不清（DECISIONS Q475）。
	TypeShare Type = "share"
)

// 普通账号 UID 的分配区间。
//
// 下限 1000 是 Debian/Ubuntu 的 FIRST_UID，上限 59999 是 adduser 的 LAST_UID 常规
// 区间上沿。区间之外一律拒绝——不是因为 60000 之后不能用，而是因为面板无法解释
// “为什么这个账号的 uid 和别的不一样”。共享账号走 --system，落在下限以下，
// 同一张表里两种类型靠这一条就能分开验证。
const (
	MinManagedUID = 1000
	MaxManagedUID = 59999
)

// Definition 是一份受管理账号的定义。
//
// 没有 uid、没有 gid、没有 groups、没有 home：这四项都由系统在创建时分配，面板
// 只读回、只展示、只据此对账。能提交它们就等于能提交 uid=0（CONTRACTS.md 第 13 节）。
//
// **每个字段都显式写 json 标签，一个都不能省。** 代理侧解参数用的是
// `DisallowUnknownFields`（CONTRACTS.md 第 5 节：未知字段直接拒绝），而
// encoding/json 的“大小写不敏感”只对 Name→name 这种整词有效：`display_name`、
// `password_secret_ref`、`shell_profile` 里带下划线，没有标签就会一个都匹配不上，
// 于是页面按 CONTRACTS §13.8 的形状提交、代理一个字段都不认。这条缺陷在单元测试
// 里看不见，因为它只在“真按线上形状发一份 JSON”时才出现——所以
// internal/agent/accountaction_test.go 里那几条解码用例是这条注释的执行版本。
type Definition struct {
	Name string `json:"name"`
	// DisplayName 只是给人看的标签，不参与任何系统调用。
	DisplayName string `json:"display_name"`
	Type        Type   `json:"type"`
	Disabled    bool   `json:"disabled"`
	// PasswordSecretRef 指向代理秘密存储里的密文；空串表示不设密码。
	PasswordSecretRef string       `json:"password_secret_ref"`
	ShellProfile      ShellProfile `json:"shell_profile"`
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

// Normalize 补上默认值并校验，返回可以落库的定义。
//
// 归一化只做“没有明确说就算什么”，不做“明显不对就改对”：类型、profile 这类闭集
// 直接拒绝，不猜。
func (d Definition) Normalize() (Definition, error) {
	out := d
	out.Name = strings.TrimSpace(out.Name)
	out.DisplayName = strings.TrimSpace(out.DisplayName)

	if err := ValidateName(out.Name); err != nil {
		return Definition{}, err
	}
	switch out.Type {
	case TypeRegular, TypeShare:
	case "":
		return Definition{}, newDefError(NoticeAccountTypeInvalid, "type 不能为空（regular 或 share）")
	default:
		return Definition{}, newDefError(NoticeAccountTypeInvalid,
			"type 只能是 regular 或 share，收到 %q", string(out.Type))
	}
	if out.DisplayName == "" {
		out.DisplayName = out.Name
	}
	if err := ValidateDisplayName(out.DisplayName); err != nil {
		return Definition{}, err
	}
	if out.ShellProfile == "" {
		out.ShellProfile = ProfileDisabled
	}
	switch out.ShellProfile {
	case ProfileDisabled:
	case ProfileRestricted:
		// 共享账号不接受“能登录”这一档。共享账号存在的意义是给 SMB 认证，
		// 一旦它能登录，面板给它的 nologin 与 /nonexistent home 就都白设了。
		if out.Type == TypeShare {
			return Definition{}, newDefError(NoticeAccountShellProfileInvalid,
				"共享账号（type=share）不能使用 restricted 登录档；共享账号只用于 SMB 认证，不提供登录")
		}
	default:
		return Definition{}, newDefError(NoticeAccountShellProfileInvalid,
			"shell_profile 只能是 disabled 或 restricted，收到 %q", string(out.ShellProfile))
	}
	if out.PasswordSecretRef != "" && !validSecretRefShape(out.PasswordSecretRef) {
		return Definition{}, newDefError(NoticeAccountSecretInvalid,
			"password_secret_ref 形状不对（期望 sec_ + 32 位小写十六进制），收到 %q；"+
				"明文密码永远不会出现在这个字段里", out.PasswordSecretRef)
	}
	return out, nil
}

// reservedAccountNames 是不允许作为受管理账号的名字（闭集，小写）。
//
// 与 internal/shares/account.go 的那一份同形，另外多两条面板身份：
// PLAN.md 第 19 节明确“1.0 不提供通过 Web 给 upanel-web / upanel-shell 授予 sudo
// 的功能”，所以面板身份既不能建、也不能被当成受管理账号去改。
var reservedAccountNames = map[string]bool{
	"root": true, "daemon": true, "bin": true, "sys": true, "sync": true,
	"games": true, "man": true, "lp": true, "mail": true, "news": true,
	"uucp": true, "proxy": true, "www-data": true, "backup": true, "list": true,
	"irc": true, "gnats": true, "nobody": true, "nogroup": true, "sudo": true,
	"admin": true, "sshd": true, "systemd-network": true, "systemd-resolve": true,
	"systemd-timesync": true, "messagebus": true, "upane": true, "upanel": true,
	"upanel-agent": true, "upanel-web": true, "upanel-shell": true,
	"nfsnobody": true, "landscape": true, "pollinate": true, "syslog": true,
	"tss": true, "usbmux": true, "uuidd": true, "sshd-rsa": true,
}

// ValidateName 校验受管理账号名。
//
// 只接受小写：Linux 账号名大小写敏感，而 SMB 名在多数配置下大小写不敏感。允许大写
// 会造出两个不同的 Linux 账号却映射到同一个 SMB 名。
func ValidateName(name string) error {
	if name == "" {
		return newDefError(NoticeAccountNameInvalid, "账号名不能为空")
	}
	if len(name) > maxAccountNameLen {
		return newDefError(NoticeAccountNameInvalid,
			"账号名过长（%d 字节，上限 %d）", len(name), maxAccountNameLen)
	}
	first := name[0]
	if first < 'a' || first > 'z' {
		return newDefError(NoticeAccountNameInvalid,
			"账号名必须以小写字母开头，收到 %q；下划线开头会和 _apt、_chrony 这类系统账号混在同一个名字空间里", name)
	}
	for i := 1; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '_', c == '-':
		default:
			return newDefError(NoticeAccountNameInvalid,
				"账号名只能包含小写字母、数字、下划线、连字符（位置 %d 是 %q）；"+
					"大写会让 SMB 名与 Linux 名不再一一对应", i, c)
		}
	}
	if strings.HasSuffix(name, "-") {
		return newDefError(NoticeAccountNameInvalid, "账号名不能以连字符结尾")
	}
	// useradd 自己的规则里，名字也不能以数字开头（否则和 uid 的写法分不开）——
	// 上面对首字符的限制已经覆盖了它，这里不重复。
	if reservedAccountNames[name] {
		return newDefError(NoticeAccountReserved,
			"账号名 %q 是系统/面板的保留名，不能作为受管理账号", name)
	}
	return nil
}

// ValidateDisplayName 校验展示名。
//
// 展示名不进任何命令行、不进任何配置文件，只进页面与审计摘要。唯一的硬要求是
// 不能带控制字符（换行/回车会让审计摘要变成两行，日志就再也对齐不了）与长度上限。
func ValidateDisplayName(name string) error {
	if !utf8.ValidString(name) {
		return newDefError(NoticeAccountDisplayNameInvalid, "展示名不是合法的 UTF-8")
	}
	if utf8.RuneCountInString(name) > maxDisplayNameRunes {
		return newDefError(NoticeAccountDisplayNameInvalid,
			"展示名过长（%d 字符，上限 %d）", utf8.RuneCountInString(name), maxDisplayNameRunes)
	}
	for _, r := range name {
		if r == utf8.RuneError || unicode.IsControl(r) {
			return newDefError(NoticeAccountDisplayNameInvalid,
				"展示名里不能有控制字符（收到 %q）", r)
		}
	}
	if strings.TrimSpace(name) == "" {
		return newDefError(NoticeAccountDisplayNameInvalid, "展示名不能全是空白")
	}
	return nil
}

// UIDInRange 判断一个读回来的 uid 是否落在普通账号的区间里。
func UIDInRange(uid int) bool { return uid >= MinManagedUID && uid <= MaxManagedUID }

// secretRefPattern 与 agentstore.ValidSecretRef 是同一条形状（sec_ + 32 位小写十六进制）。
//
// 这里不 import agentstore：agentstore 反过来要存这个包的类型，import 会成环。
var secretRefPattern = regexp.MustCompile(`^sec_[0-9a-f]{32}$`)

func validSecretRefShape(ref string) bool { return secretRefPattern.MatchString(ref) }
