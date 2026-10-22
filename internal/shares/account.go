package shares

// 受管理共享账号：名字形状、系统读回的解析（P08.1）。
//
// 这一文件里全是**纯函数**：解析 getent passwd / passwd -S / pdbedit 的输出。
// 把解析单独抽出来，是因为它们是“读回核验”的全部依据——读错的解析会让执行器
// 把没做成的事报成做成了。纯函数可以用真实输出做单元测试，不需要那台机器上装着 Samba。

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// 账号名的形状。
//
// 只接受小写：Samba 的 SMB 用户名在多数配置下大小写不敏感，而 Linux 账号名大小写
// 敏感。允许大写会造出两个不同的 Linux 账号却映射到同一个 SMB 名——这种状态在
// 两边的工具里都“能用”，但没人说得清谁是谁。
const (
	maxAccountNameLen = 32
	// DefaultShell 是受管理共享账号的登录 shell。
	//
	// nologin 是这一阶段唯一的登录屏障之一（另一半是 passwd 锁）：共享账号只用于
	// SMB 认证，绝不能拿它登录系统。
	DefaultShell = "/usr/sbin/nologin"
	// DefaultHome 是受管理共享账号的 home。
	//
	// 指向 /nonexistent 是刻意的：不建 home 目录，也就没有“删账号会不会删数据”的
	// 问题（CONTRACTS.md 第 5 节 account.delete 要求不删 home 数据）。
	DefaultHome = "/nonexistent"
)

// ValidateAccountName 校验受管理账号名。
func ValidateAccountName(name string) error {
	if name == "" {
		return newDefError(NoticeAccountNameInvalid, "账号名不能为空")
	}
	if len(name) > maxAccountNameLen {
		return newDefError(NoticeAccountNameInvalid, "账号名过长（%d 字节，上限 %d）", len(name), maxAccountNameLen)
	}
	// 首字符只允许小写字母：允许下划线开头就会和 _apt、_chrony 这类系统账号
	// 混在同一个名字空间里，而“这个名字到底是谁的”在 SMB 侧看不出来。
	first := name[0]
	if first < 'a' || first > 'z' {
		return newDefError(NoticeAccountNameInvalid,
			"账号名必须以小写字母开头，收到 %q", name)
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
	// 保留名：这些账号要么是面板自己，要么是系统服务账号。允许创建同名账号
	// 意味着“面板以为自己在管一个共享账号”，而系统上那个账号属于别的服务。
	if reservedAccountNames[name] {
		return newDefError(NoticeAccountNameInvalid,
			"账号名 %q 是系统/面板的保留名，不能用作共享账号", name)
	}
	return nil
}

// reservedAccountNames 是不允许作为共享账号的名字（闭集，小写）。
var reservedAccountNames = map[string]bool{
	"root": true, "daemon": true, "bin": true, "sys": true, "sync": true,
	"games": true, "man": true, "lp": true, "mail": true, "news": true,
	"uucp": true, "proxy": true, "www-data": true, "backup": true, "list": true,
	"irc": true, "gnats": true, "nobody": true, "nogroup": true, "sudo": true,
	"admin": true, "sshd": true, "systemd-network": true, "systemd-resolve": true,
	"systemd-timesync": true, "messagebus": true, "upane": true, "upanel": true,
	"upanel-agent": true, "upanel-web": true,
}

// PasswdEntry 是从系统读回来的账号事实（不是我们传进去的期望值）。
type PasswdEntry struct {
	Name  string
	UID   int
	GID   int
	Home  string
	Shell string
}

// parsePasswdLine 解析 `getent passwd <name>` 的一行。
//
// 真实输出形如：
//
//	upanel-probe:x:997:987::/nonexistent:/usr/sbin/nologin
func parsePasswdLine(line string) (PasswdEntry, error) {
	fields := strings.Split(strings.TrimRight(line, "\r\n"), ":")
	if len(fields) != 7 {
		return PasswdEntry{}, fmt.Errorf("shares: passwd 行有 %d 个字段，期望 7 个：%q", len(fields), line)
	}
	uid, err := strconv.Atoi(fields[2])
	if err != nil {
		return PasswdEntry{}, fmt.Errorf("shares: passwd 行的 uid 不是整数：%q", fields[2])
	}
	gid, err := strconv.Atoi(fields[3])
	if err != nil {
		return PasswdEntry{}, fmt.Errorf("shares: passwd 行的 gid 不是整数：%q", fields[3])
	}
	return PasswdEntry{Name: fields[0], UID: uid, GID: gid, Home: fields[5], Shell: fields[6]}, nil
}

// LockState 是 `passwd -S` 报告的状态。
type LockState string

const (
	// LockLocked 表示账号已锁（*L*ocked）。这是受管理共享账号的期望状态。
	LockLocked LockState = "L"
	// LockUsable 表示账号有可用密码（*P*assword）。
	LockUsable LockState = "P"
	// LockNone 表示没有密码（*N*o password）。
	LockNone LockState = "NP"
	// LockUnknown 表示读不懂的输出：不猜，如实报告。
	LockUnknown LockState = "unknown"
)

// parseLockState 解析 `passwd -S <name>` 的第二列。
//
// 真实输出形如：`upanel-probe L 2026-10-20 -1 -1 -1 -1`
func parseLockState(out string) LockState {
	fields := strings.Fields(strings.TrimSpace(out))
	if len(fields) < 2 {
		return LockUnknown
	}
	switch fields[1] {
	case "L":
		return LockLocked
	case "P":
		return LockUsable
	case "NP":
		return LockNone
	default:
		return LockUnknown
	}
}

// SambaAccount 是从 passdb 读回来的账号事实。
type SambaAccount struct {
	Name string
	// Disabled 来自 `Account Flags: [DU         ]` 里的 D 位。
	Disabled bool
	// PasswordSet 报告 passdb 里是否已经设过密码。
	//
	// 判据是 `Password last set:` 后面那个时间戳能被解析成一个晚于 1970 的真实时刻。
	// 之所以用“能不能解析成一个合理时刻”而不是“有没有这行”：pdbedit 在没有密码时
	// 也会打印这一行（值是 0 或 1970 年的时间戳）。
	PasswordSet bool
	// PasswordSetAt 是 passdb 记录的设置时刻；PasswordSet 为 false 时是零值。
	PasswordSetAt time.Time
	// Flags 是原始的 Account Flags 内容（去掉方括号与空白），用于诊断。
	Flags string
}

// pdbedit 输出的时间格式。真实输出形如：
//
//	Password last set:    Thu, 22 Oct 2026 09:23:39 UTC
const pdbeditTimeLayout = "Mon, 02 Jan 2006 15:04:05 MST"

// parsePdbedit 解析 `pdbedit -L -v -u <name>` 的输出。
//
// 输出是一串 `键: 值` 行。这里只取三样：Unix username、Account Flags、
// Password last set。**不**解析任何哈希字段：`pdbedit -L -w` 才会导出哈希，
// 那一条命令在本代码里根本不存在。
func parsePdbedit(out string) (SambaAccount, error) {
	var acc SambaAccount
	seen := false
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		switch key {
		case "Unix username":
			if value != "" {
				acc.Name = value
				seen = true
			}
		case "Account Flags":
			flags := strings.TrimSuffix(strings.TrimPrefix(value, "["), "]")
			acc.Flags = strings.TrimSpace(flags)
			acc.Disabled = strings.Contains(acc.Flags, "D")
			seen = true
		case "Password last set":
			if ts, ok := parsePdbeditTime(value); ok {
				acc.PasswordSet = true
				acc.PasswordSetAt = ts
			}
		}
	}
	if !seen {
		return SambaAccount{}, fmt.Errorf("shares: pdbedit 输出里没有可识别的账号信息")
	}
	return acc, nil
}

// parsePdbeditTime 解析 pdbedit 的时间值。
//
// 认不出来的值一律当成“没有设置密码”（返回 ok=false）：把一个读不懂的时间当成
// “已设置密码”会让执行器报出一个假的成功。
func parsePdbeditTime(value string) (time.Time, bool) {
	v := strings.TrimSpace(value)
	if v == "" || v == "0" {
		return time.Time{}, false
	}
	ts, err := time.Parse(pdbeditTimeLayout, v)
	if err != nil {
		return time.Time{}, false
	}
	// 1970 附近的时刻是“没有设置”的另一种写法。
	if ts.Year() < 2001 {
		return time.Time{}, false
	}
	return ts.UTC(), true
}

// sambaPasswordStdin 构造 smbpasswd -s 需要的标准输入。
//
// `smbpasswd -a -s <name>` 从标准输入读两遍密码。这是密码离开秘密存储之后唯一
// 到过的地方：不进 argv（argv 会出现在 /proc/<pid>/cmdline 与 ps 里）、不进日志、
// 不进数据库。调用方必须在用完之后把返回的切片清掉（本函数返回的切片就是它自己
// 分配的那个，没有别的副本）。
func sambaPasswordStdin(secret []byte) []byte {
	buf := make([]byte, 0, len(secret)*2+2)
	buf = append(buf, secret...)
	buf = append(buf, '\n')
	buf = append(buf, secret...)
	buf = append(buf, '\n')
	return buf
}

// wipe 把一段字节清零。用循环而不是 clear 语义的赋值，是为了让它在任何 Go 版本上
// 都真的写到内存里（编译器不会把一次“写零后再也没读”的循环优化掉，因为它有逃逸的切片头）。
func wipe(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
