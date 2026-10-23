package apt

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// 本文件只做一件事：把工具的**输出原文**变成结构体。没有判断，没有查表，
// 没有"顺手把 ~ubuntu0.x 当成安全更新"这类聪明事。
//
// 解析器的输入形状全部来自真机抓回来的原文（见 apt_test.go 里的夹具），
// 不是照文档猜的格式。

// ParseDpkgQuery 解析
//
//	dpkg-query -W -f='${binary:Package}\t${Version}\t${Architecture}\t${db:Status-Abbrev}\n'
//
// 的输出。
//
// 注意三点，都是真机上看得见的：
//
//  1. 包名可能带架构限定（`libssl3:amd64`）——multiarch 同名包时 dpkg 会这么给，
//     而 ${Architecture} 是另一列。两者都要留着，面板用 (name, architecture) 当键。
//  2. `dpkg-query -W` 对已删除但残留配置的包也会给行，状态是 `rc`；这些不是"已安装"，
//     所以 Status 原样传出去，由面板判。
//  3. 版本号里可以出现任何字符（含 `+`、`~`、`:`），所以不能按冒号切字段。
func ParseDpkgQuery(out string) ([]Package, error) {
	pkgs := make([]Package, 0, 256)
	for i, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 4 {
			return nil, fmt.Errorf("dpkg-query 输出第 %d 行不是 4 个制表符分隔字段（实际 %d 个）：%q",
				i+1, len(fields), truncateForError(line))
		}
		if fields[0] == "" {
			return nil, fmt.Errorf("dpkg-query 输出第 %d 行包名为空", i+1)
		}
		pkgs = append(pkgs, Package{
			Name:             fields[0],
			InstalledVersion: fields[1],
			Architecture:     fields[2],
			Status:           fields[3],
		})
	}
	return pkgs, nil
}

// policyEntry 是 `apt-cache policy` 里一个包的候选信息。
type policyEntry struct {
	// Version 是候选版本；空串表示 APT 明确说 `Candidate: (none)`。
	// 与"map 里没有这个键"（= 没读到）是两件事。
	Version string
	// Suites 是**候选版本那一块**的来源 suite，已去重升序。
	Suites []string
	// OnlyLocal 表示候选版本的来源只有 /var/lib/dpkg/status，任何仓库都不提供它
	// （本地 dpkg -i 装的包）。
	OnlyLocal bool
}

// 来源行的形状：`500 http://archive.ubuntu.com/ubuntu jammy-updates/main amd64 Packages`。
// 第一列是优先级，最后一列是文件名，中间是 [uri, suite, component...]。没有 uri 的行
// （`100 /var/lib/dpkg/status`）第一列后面只有路径。
var releaseFileSuffix = regexp.MustCompile(`\bPackages$`)

// phasedSuffix 是版本行末尾的滚动更新标记：`6.2-1ubuntu0.3 500 (phased 70%)`。
// P11.1 只把版本行认干净、不做判断（CONTRACTS 12.1-4）；
// P11.2 的模拟才用它解释"为什么这个包没升"。
var phasedSuffix = regexp.MustCompile(`^\(phased\s+\d+%\)$`)

// ParseAptCachePolicy 解析 `apt-cache policy <names...>` 的输出。
//
// 真机原文（22.04，这次实抓，nano 那一段）：
//
//	nano:
//	  Installed: 6.2-1ubuntu0.2
//	  Candidate: 6.2-1ubuntu0.3
//	  Version table:
//	     6.2-1ubuntu0.3 500 (phased 70%)
//	        500 http://archive.ubuntu.com/ubuntu jammy-updates/main amd64 Packages
//	 *** 6.2-1ubuntu0.2 500
//	        500 http://security.ubuntu.com/ubuntu jammy-security/main amd64 Packages
//	        100 /var/lib/dpkg/status
//	     6.2-1 500
//	        500 http://archive.ubuntu.com/ubuntu jammy/main amd64 Packages
//
// 两个反直觉的地方，都是真机看出来的：
//
//  1. `***` 标的是**当前已安装的那个版本**，不是候选版本。nano 这一份里 `***` 指着
//     6.2-1ubuntu0.2，候选却是 6.2-1ubuntu0.3。若拿 `***` 那一块的 suite 当候选来源，
//     nano 会被算成"安全更新"——真实情况正相反：候选来自 jammy-updates，
//     jammy-security 里放的是它现在装着的、更旧的那个版本。
//  2. 所以候选的 suite 必须从**版本号等于 `Candidate:` 的那一块**里取，
//     那一块可能根本没有 `***` 前缀。
//
// 返回 map 以包名为键。
func ParseAptCachePolicy(out string) (map[string]policyEntry, error) {
	res := make(map[string]policyEntry, 256)

	// 一个版本块的来源。同一个版本号在输出里理论上只出现一次；
	// 真机上 `file:` 与 http 混用时可能重复，所以累加而不是覆盖。
	type block struct {
		suites     []string
		hasRelease bool
		hasLocal   bool
	}

	var (
		name      string
		candidate string
		blocks    map[string]*block

		curVersion    string
		curSuites     []string
		curHasRelease bool
		curLocal      bool
	)

	flushBlock := func() {
		if name == "" || curVersion == "" {
			return
		}
		b := blocks[curVersion]
		if b == nil {
			b = &block{}
			blocks[curVersion] = b
		}
		for _, s := range curSuites {
			if !containsString(b.suites, s) {
				b.suites = append(b.suites, s)
			}
		}
		b.hasRelease = b.hasRelease || curHasRelease
		b.hasLocal = b.hasLocal || curLocal
	}

	flushPackage := func() {
		flushBlock()
		if name == "" {
			return
		}
		entry := policyEntry{Version: candidate}
		if candidate != "" {
			if b := blocks[candidate]; b != nil {
				entry.Suites = normalizeSuites(b.suites)
				entry.OnlyLocal = b.hasLocal && !b.hasRelease
			}
		}
		res[name] = entry
		name, candidate, blocks = "", "", nil
		curVersion, curSuites, curHasRelease, curLocal = "", nil, false, false
	}

	for i, raw := range strings.Split(out, "\n") {
		line := strings.TrimRight(raw, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		// 包名行：顶格、以 `:` 结尾。缩进行一律属于当前包。
		if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			if !strings.HasSuffix(line, ":") {
				return nil, fmt.Errorf("apt-cache policy 输出第 %d 行既不是顶格的包名行也不是缩进行：%q",
					i+1, truncateForError(line))
			}
			flushPackage()
			name = strings.TrimSuffix(line, ":")
			blocks = make(map[string]*block, 4)
			continue
		}
		if name == "" {
			// 第一个包名之前的缩进行（`WARNING: ...` 之类）。
			continue
		}
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "Installed:"):
			// 已安装版本由 dpkg-query 给（权威且带状态位），这里不重复取。
		case strings.HasPrefix(trimmed, "Candidate:"):
			candidate = strings.TrimSpace(strings.TrimPrefix(trimmed, "Candidate:"))
			if candidate == "(none)" {
				candidate = ""
			}
		case strings.HasPrefix(trimmed, "Version table:"):
			// 表头。
		case strings.HasPrefix(trimmed, "WARNING"):
			// `WARNING: apt does not have a stable CLI interface` 之类的提示。
			// 不是数据，跳过而不是让整页失败。
		default:
			fields := strings.Fields(trimmed)
			if len(fields) > 0 && fields[0] == "***" {
				fields = fields[1:]
			}
			// 版本行：版本号 + 优先级数字 [+ 可选的 `(phased N%)`]。
			// 优先级不是纯数字时不是版本行（`100 /var/lib/dpkg/status` 那种）。
			if len(fields) >= 2 && isAllDigits(fields[1]) &&
				(len(fields) == 2 || phasedSuffix.MatchString(strings.Join(fields[2:], " "))) {
				flushBlock()
				curVersion = fields[0]
				curSuites, curHasRelease, curLocal = nil, false, false
				continue
			}
			if curVersion == "" {
				continue
			}
			suite, local, isRelease := parseReleaseLine(fields)
			if !isRelease {
				continue
			}
			if local {
				curLocal = true
				continue
			}
			curHasRelease = true
			if suite != "" && !containsString(curSuites, suite) {
				curSuites = append(curSuites, suite)
			}
		}
	}
	flushPackage()
	return res, nil
}

// parseReleaseLine 从一条来源行里取出 suite。
//
// 形状：`<pin> <uri> <suite> <component...> <arch> <file>`，
// 例：`500 http://archive.ubuntu.com/ubuntu jammy-updates/main amd64 Packages`
// → suite=jammy-updates（component 的 `/main` 切掉，只留 suite）。
//
// flat 布局的仓库没有 component（`500 https://dl.example.com/stable ./ Packages`），
// 这时返回空串：flat 仓库没有"口袋"概念，猜一个出来只会误导。
//
// local=true 表示这是 `/var/lib/dpkg/status`（本地已安装状态文件）。
func parseReleaseLine(fields []string) (suite string, local bool, isRelease bool) {
	if len(fields) == 0 {
		return "", false, false
	}
	// 去掉末尾的 Release/Packages 文件名。
	for len(fields) > 0 && releaseFileSuffix.MatchString(fields[len(fields)-1]) {
		fields = fields[:len(fields)-1]
	}
	if len(fields) == 0 {
		return "", false, false
	}
	// 去掉架构列（`amd64`、`i386`、`all`），它在 Packages 文件名前面。
	if len(fields) >= 2 && isArchToken(fields[len(fields)-1]) {
		fields = fields[:len(fields)-1]
	}
	// 现在剩下 `<pin> <uri 或路径> <suite...>`。
	body := fields
	if isAllDigits(body[0]) {
		body = body[1:]
	}
	if len(body) == 0 {
		return "", false, true
	}
	if body[0] == "/var/lib/dpkg/status" || strings.HasSuffix(body[0], "dpkg/status") {
		return "", true, true
	}
	if len(body) < 2 {
		// 只有路径、没有 suite，判断不出口袋。
		return "", false, true
	}
	suite = body[1]
	if i := strings.Index(suite, "/"); i >= 0 {
		if strings.HasPrefix(suite, "./") || strings.HasPrefix(suite, "/") {
			return "", false, true
		}
		suite = suite[:i]
	}
	return suite, false, true
}

// isArchToken 判断是不是架构列。真机上就这些名字，够用且不会误判 suite。
func isArchToken(s string) bool {
	switch s {
	case "amd64", "i386", "arm64", "armhf", "armel", "ppc64el", "s390x", "riscv64", "all", "source":
		return true
	}
	return false
}

func normalizeSuites(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s != "" && !containsString(out, s) {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func containsString(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}

// ParseRebootPkgs 解析 /run/reboot-required.pkgs：一行一个包名，可重复
// （同一个包可能被几条 hook 各写一次），这里保序去重。
func ParseRebootPkgs(data []byte) []string {
	out := make([]string, 0, 8)
	for _, line := range strings.Split(string(data), "\n") {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		if !containsString(out, name) {
			out = append(out, name)
		}
	}
	return out
}

// bootIDShape 是内核给的格式：`xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx`。
// 校验形状而不是直接透传：一个被截断的 boot_id 会让 P11.3 的"重启前后核对"
// 在两次同样截断的值上判等，等于没核对。
var bootIDShape = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ParseBootID 解析 /proc/sys/kernel/random/boot_id。
func ParseBootID(data []byte) (string, error) {
	id := strings.TrimSpace(string(data))
	if id == "" {
		return "", fmt.Errorf("boot_id 为空")
	}
	if !bootIDShape.MatchString(id) {
		return "", fmt.Errorf("boot_id 形状不对（%d 字节）：%q", len(id), truncateForError(id))
	}
	return strings.ToLower(id), nil
}

// truncateForError 把一段原文缩到 200 字节以内再进错误信息：
// 错误会被写进响应，不能因为一行异常就把整份输出带出去。
func truncateForError(s string) string {
	const max = 200
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
