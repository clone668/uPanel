package accounts

// 读回解析（P14.5）：getent passwd / passwd -S / sudo -l 的输出。
//
// 全是**纯函数**，理由与 internal/shares/account.go 相同：读回核验的全部依据就是
// 这些解析，读错一行会让执行器把没做成的事报成做成了。纯函数可以用真机抄下来的
// 输出做单元测试，不需要那台机器上装着一整套账号工具。

import (
	"fmt"
	"strconv"
	"strings"
)

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
//	alice:x:1001:1001:Alice,,,:/home/alice:/bin/rbash
//
// 字段数是 7：名字、口令占位、uid、gid、gecos、home、shell。gecos 里可以有冒号
// 吗？标准上不允许，`getent` 会原样打印 passwd 里的那一行——所以按 7 段切不会错。
func parsePasswdLine(line string) (PasswdEntry, error) {
	fields := strings.Split(strings.TrimRight(line, "\r\n"), ":")
	if len(fields) != 7 {
		return PasswdEntry{}, fmt.Errorf("accounts: passwd 行有 %d 个字段，期望 7 个：%q", len(fields), line)
	}
	uid, err := strconv.Atoi(fields[2])
	if err != nil {
		return PasswdEntry{}, fmt.Errorf("accounts: passwd 行的 uid 不是整数：%q", fields[2])
	}
	gid, err := strconv.Atoi(fields[3])
	if err != nil {
		return PasswdEntry{}, fmt.Errorf("accounts: passwd 行的 gid 不是整数：%q", fields[3])
	}
	return PasswdEntry{Name: fields[0], UID: uid, GID: gid, Home: fields[5], Shell: fields[6]}, nil
}

// LockState 是 `passwd -S` 报告的状态。
type LockState string

const (
	// LockLocked 表示账号的口令是锁着的（*L*ocked）。
	LockLocked LockState = "L"
	// LockUsable 表示账号有可用口令（*P*assword）。
	LockUsable LockState = "P"
	// LockNone 表示没有口令（*N*o *P*assword）。
	LockNone LockState = "NP"
	// LockUnknown 表示读不懂的输出：不猜，如实报告。
	LockUnknown LockState = "unknown"
)

// parseLockState 解析 `passwd -S <name>` 的第二列。
//
// 真实输出形如：`alice P 2026-10-27 0 99999 7 -1`
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

// SudoRuleLine 是 `sudo -l -U` 读回来的一条授权。
type SudoRuleLine struct {
	// RunAs 是括号里那一段（`(root)` 的 root；`(ALL : ALL)` 也有）。
	RunAs string
	// NoPasswd 报告这条是不是 NOPASSWD（面板渲染出来的全是 NOPASSWD）。
	NoPasswd bool
	// Command 是授权的那条命令（含参数，原样）。
	Command string
}

// parseSudoList 解析 `sudo -l -U <name>` 输出里的授权行。
//
// 真实输出形如：
//
//	Matching Defaults entries for alice on upanel-test:
//	    env_reset, mail_badpass
//
//	User alice may run the following commands on upanel-test:
//	    (root) NOPASSWD: /usr/bin/systemctl status --no-pager
//
// 只认以 `(` 开头的行。`(ALL : ALL) ALL` 这种“什么都能干”的授权**照样返回**，
// 由调用方判成致命不符——解析层不做判断，它只把事实读出来。
func parseSudoList(out string) []SudoRuleLine {
	var rules []SudoRuleLine
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimSpace(raw)
		if !strings.HasPrefix(line, "(") {
			continue
		}
		closing := strings.Index(line, ")")
		if closing < 0 {
			continue
		}
		rule := SudoRuleLine{RunAs: strings.TrimSpace(line[1:closing])}
		rest := strings.TrimSpace(line[closing+1:])
		rest = strings.TrimSpace(strings.TrimPrefix(rest, ":"))
		if strings.HasPrefix(rest, "NOPASSWD:") {
			rule.NoPasswd = true
			rest = strings.TrimSpace(strings.TrimPrefix(rest, "NOPASSWD:"))
		}
		// `PASSWD:` 是默认值，显式写出来也只是个前缀，不是命令的一部分。
		if strings.HasPrefix(rest, "PASSWD:") {
			rest = strings.TrimSpace(strings.TrimPrefix(rest, "PASSWD:"))
		}
		if rest == "" {
			continue
		}
		rule.Command = rest
		rules = append(rules, rule)
	}
	return rules
}

// sudoRuleMatches 判断读回来的一条授权是不是模板期望的那一条。
//
// 精确比较命令（含参数）：templates 里没有通配符，所以“期望 systemctl status
// --no-pager”与“实际 systemctl *”必须是两件事。这一条比较就是 Q476 的落点。
func sudoRuleMatches(rule SudoRuleLine, template SudoTemplate) bool {
	if !rule.NoPasswd || rule.RunAs != "root" {
		return false
	}
	for _, cmd := range template.Commands {
		if rule.Command == cmd {
			return true
		}
	}
	return false
}

// sudoRulesMatchTemplate 判断读回来的整组授权与模板是否**完全一致**。
//
// “完全”是双向的：模板里的每条都在，且没有多出来的。少了不算成功，多了更不能算——
// 多出来的那条是别人给的权限，面板不能把它当成自己的成果报成 verified。
func sudoRulesMatchTemplate(rules []SudoRuleLine, template SudoTemplate) bool {
	if len(rules) != len(template.Commands) {
		return false
	}
	seen := make([]bool, len(template.Commands))
	for _, rule := range rules {
		matched := false
		for i, cmd := range template.Commands {
			if seen[i] {
				continue
			}
			if sudoRuleMatches(rule, SudoTemplate{Commands: []string{cmd}}) {
				seen[i] = true
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	for _, ok := range seen {
		if !ok {
			return false
		}
	}
	return true
}
