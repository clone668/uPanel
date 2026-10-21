package processes

import (
	"path"
	"regexp"
	"strings"
)

// 命令行脱敏（CONTRACTS.md 第 10 节：“脱敏命令摘要”）。
//
// 进程列表是给人看“这台机器在跑什么”的，不是给人看密码的。argv 里塞密码、
// token、密钥是很常见的做法（`mysql -p***`、`--token=...`、`API_KEY=...`），
// 把它们原样显示在面板上，等于给每一个能打开页面的人发了一份凭据。
//
// 规则（刻意保守且可测）：
//  1. argv[0] 是绝对路径时只取基名；含空格的整串（sshd 这类）原样保留。
//  2. `--flag=value`、`KEY=value`：flag/键名命中敏感词 → 值替换成 ***。
//  3. `--flag value` 两个参数形式：flag 命中敏感词 → 下一个参数替换成 ***。
//  4. 控制字符（含换行、NUL）替换成空格；单参数截断到 80 字节，整体 240 字节。
//  5. 没有命令行（内核线程、僵尸）→ `[comm]`，明确表示“没有用户空间命令行”。

const (
	// maxArgRunes 是单个参数保留的最大字符数。
	maxArgRunes = 80
	// maxCommandRunes 是整条摘要保留的最大字符数。
	maxCommandRunes = 240
	// redacted 是替换后的占位符。
	redacted = "***"
)

// sensitiveName 匹配“这个参数名后面跟的是秘密”。区分大小写不敏感：
// 真实命令行里 --Password、PASSWORD、Api_Key 都出现过。
var sensitiveName = regexp.MustCompile(`(?i)(pass(word|wd|phrase)?|pwd|secret|token|api[-_]?key|apikey|access[-_]?key|private[-_]?key|credential|bearer|auth|session[-_]?key|signature)$`)

// flagName 提取 `--flag=value` 或 `KEY=value` 里的名字部分；没有 '=' 返回空串。
func flagName(arg string) (name string, ok bool) {
	index := strings.IndexByte(arg, '=')
	if index <= 0 {
		return "", false
	}
	name = arg[:index]
	// 只处理像选项或环境变量的名字：以 - 开头，或全大写字母/数字/下划线。
	if strings.HasPrefix(name, "-") {
		return strings.TrimLeft(name, "-"), true
	}
	if isEnvName(name) {
		return name, true
	}
	return "", false
}

// isEnvName 判断是否形如 API_KEY / TOKEN 这样的环境变量名。
func isEnvName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '_':
		default:
			return false
		}
	}
	return true
}

// sanitizeArg 处理一个参数：若它是 `name=value` 且 name 敏感，值替换掉。
func sanitizeArg(arg string) string {
	name, ok := flagName(arg)
	if !ok {
		return arg
	}
	if !sensitiveName.MatchString(name) {
		return arg
	}
	index := strings.IndexByte(arg, '=')
	return arg[:index+1] + redacted
}

// isSensitiveFlag 判断 `--flag value` 形式里的 flag 是否敏感（此时下一个参数是值）。
func isSensitiveFlag(arg string) bool {
	if !strings.HasPrefix(arg, "-") || strings.Contains(arg, "=") {
		return false
	}
	return sensitiveName.MatchString(strings.TrimLeft(arg, "-"))
}

// sanitizePart 截断并清掉控制字符。
func sanitizePart(part string) string {
	part = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, part)
	part = strings.TrimSpace(part)
	runes := []rune(part)
	if len(runes) > maxArgRunes {
		return string(runes[:maxArgRunes]) + "…"
	}
	return part
}

// SanitizeCommand 生成脱敏的命令摘要。
//
// argv 为空时用 comm 拼成 `[comm]`：内核线程没有用户空间命令行，这个方括号写法
// 与 ps 一致，读的人一眼就知道“这里没有 argv 可看”。
func SanitizeCommand(argv []string, comm string) string {
	if len(argv) == 0 {
		name := sanitizePart(comm)
		if name == "" {
			return "[未知进程]"
		}
		return "[" + name + "]"
	}
	parts := make([]string, 0, len(argv))
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		if i == 0 {
			// 绝对路径只留基名；含空格的整串（sshd 的 "sshd: /usr/sbin/sshd -D ..."）
			// 保持原样，取基名会把整串切成一段毫无意义的尾巴。
			if strings.HasPrefix(arg, "/") && !strings.ContainsAny(arg, " \t") {
				arg = path.Base(arg)
			}
			parts = append(parts, sanitizePart(arg))
			continue
		}
		if isSensitiveFlag(arg) && i+1 < len(argv) {
			parts = append(parts, sanitizePart(arg), redacted)
			i++
			continue
		}
		parts = append(parts, sanitizePart(sanitizeArg(arg)))
	}
	out := strings.TrimSpace(strings.Join(parts, " "))
	runes := []rune(out)
	if len(runes) > maxCommandRunes {
		return string(runes[:maxCommandRunes]) + "…"
	}
	return out
}
