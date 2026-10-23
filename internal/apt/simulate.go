package apt

// `apt-get -s` 的解析（P11.2）。
//
// 预检要回答的是"这一手下去会发生什么"，而这个问题**只有 apt 自己能答**：依赖解析、
// 冲突、需要保留的包、会不会顺手删掉一堆东西，全都写在 `apt-get -s` 的输出里。
// 面板不去复刻依赖解析器，也不猜"删掉 A 大概会带走 B"——它就是读这份输出，
// 把它变成一份可以展示、可以核对、可以在执行后逐条验证的计划。
//
// 这份解析的取舍：**宁可不认，不要错认**。认不出来的行就当没看见（它们不影响
// 结论），而 `E:` 行与"unmet dependencies"块必须原文留下——那是失败时唯一能告诉
// 管理员"apt 为什么拒绝"的东西。

import (
	"sort"
	"strconv"
	"strings"
)

// 计划步骤的种类（Inst/Remv/Conf 行的判别）。
const (
	StepInstall   = "install"
	StepUpgrade   = "upgrade"
	StepDowngrade = "downgrade"
	StepRemove    = "remove"
)

// PlanStep 是 apt 计划里的一步，带版本。
//
// 带版本是关键：执行完之后的验证不是"看它有没有装"，而是"装上的版本是不是
// 预检时 apt 说要装的那个"。没有版本就只能比包名，那等于没验。
type PlanStep struct {
	Kind        string `json:"kind"`
	Package     string `json:"package"`
	FromVersion string `json:"from_version"`
	ToVersion   string `json:"to_version"`
}

// KeptBack 是一个被保留的包，连同 apt 给出的理由（括号里的那段原文）。
type KeptBack struct {
	Package string `json:"package"`
	Reason  string `json:"reason"`
}

// Counts 是 apt 汇总行的数字（`N upgraded, M newly installed, K to remove and J not upgraded.`）。
//
// 它与上面那些名字列表不是一回事：名字列表可能被 `-q` 的省略（`[...]`）截断，
// 而这一行永远是全量。两边对不上时以这行为准，并在告警里说明。
type Counts struct {
	Upgraded     int `json:"upgraded"`
	NewlyInstall int `json:"newly_installed"`
	ToRemove     int `json:"to_remove"`
	NotUpgraded  int `json:"not_upgraded"`
}

// Simulation 是一次 `apt-get -s` 的结论。
type Simulation struct {
	// Requested 是这次模拟请求的包名（原样，去重后保持顺序）。
	Requested []string `json:"requested"`
	// New / Additional 分别是"将被新安装"与"为了满足依赖而额外安装"。
	New        []string `json:"new"`
	Additional []string `json:"additional"`
	// Upgrade / Downgrade / Remove 是三种会改变已安装状态的动作。
	Upgrade   []string `json:"upgrade"`
	Downgrade []string `json:"downgrade"`
	Remove    []string `json:"remove"`
	// Held 是"被持有但这次仍会改动"的包。
	Held []string `json:"held"`
	// KeptBack 是升级时被保留的包（通常因为依赖不允许）。
	KeptBack []KeptBack `json:"kept_back"`
	// Autoremove 是"已自动安装且不再需要"的包——**注意它们不会被删**，
	// apt 只是提示你可以 `apt autoremove`。面板不把它当成会发生的动作。
	Autoremove []string `json:"autoremove"`
	// AlreadyNewest / Skipped / SetManual 是 apt 的三句闲话，但对人有用：
	// "已经是最新的""没装所以跳过""被设为手动安装"。
	AlreadyNewest []string `json:"already_newest"`
	Skipped       []string `json:"skipped"`
	SetManual     []string `json:"set_manual"`
	// Steps 是带版本的 Inst/Remv 行（执行后逐条核对用）。
	Steps []PlanStep `json:"steps"`
	// Unmet 是"依赖无法满足"那段原文（缩进那些行，含包名与依赖关系）。
	Unmet []string `json:"unmet"`
	// Errors 是 `E:` 行原文。
	Errors []string `json:"errors"`
	// Warnings 是 `WARNING:` / `W:` 行原文。
	Warnings []string `json:"warnings"`
	// Counts 是汇总行；没读到汇总行时四个字段都是 0。
	Counts Counts `json:"counts"`
	// HadSummary 表示是否读到了汇总行。为 false 时 Counts 不可信。
	HadSummary bool `json:"had_summary"`
}

// Changed 报告这次模拟会不会真的改变系统。
//
// "装一个已经是最新的包"是一次**没有变化**的模拟；把它当成成功执行会让任务结果
// 说谎（outcome=verified 却什么都没做）。AlreadyNewest/Skipped 也是同理。
func (s Simulation) Changed() bool {
	if s.HadSummary {
		return s.Counts.Upgraded > 0 || s.Counts.NewlyInstall > 0 || s.Counts.ToRemove > 0
	}
	return len(s.New) > 0 || len(s.Additional) > 0 || len(s.Upgrade) > 0 ||
		len(s.Downgrade) > 0 || len(s.Remove) > 0
}

// Failed 报告 apt 是否拒绝了这次模拟。
func (s Simulation) Failed() bool { return len(s.Errors) > 0 || len(s.Unmet) > 0 }

// RemovedSet 返回将被删除的包名集合（供保护名单与"顺手删了什么"使用）。
func (s Simulation) RemovedSet() map[string]bool {
	set := make(map[string]bool, len(s.Remove))
	for _, name := range s.Remove {
		set[name] = true
	}
	return set
}

// TargetVersion 返回某个包在这次计划里要达到的版本（没有计划则空串）。
//
// 只看"安装/升级"两类步骤：删除没有目标版本，降级有，但它不是 P11.2 的能力
// （面板只会请求 install / install --only-upgrade / remove）。
func (s Simulation) TargetVersion(pkg string) (string, bool) {
	for _, step := range s.Steps {
		if step.Package != pkg {
			continue
		}
		switch step.Kind {
		case StepInstall, StepUpgrade, StepDowngrade:
			return step.ToVersion, true
		}
	}
	return "", false
}

// listSection 是当前正在读的列表种类（`The following …:` 之后那一段）。
type listSection int

const (
	sectionNone listSection = iota
	sectionNew
	sectionAdditional
	sectionUpgrade
	sectionDowngrade
	sectionRemove
	sectionHeld
	sectionKeptBack
	sectionAutoremove
	sectionUnmet
)

// ParseSimulation 解析 `apt-get -s` 的输出。
//
// 它不返回 error：模拟的输出本来就可能残缺（超时被杀、apt 中途退出），
// 而"读到了什么"与"apt 退出了吗"是两个判断，后者由调用方看退出码决定。
func ParseSimulation(out string, requested []string) Simulation {
	sim := Simulation{Requested: dedupeKeepOrder(requested)}
	section := sectionNone

	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimRight(raw, " \t\r")
		if strings.TrimSpace(line) == "" {
			// 空行结束当前列表；unmet 块也有自己的空行。
			section = sectionNone
			continue
		}
		trimmed := strings.TrimSpace(line)
		indented := line != trimmed

		// 段落标题（不缩进行）。
		if !indented {
			switch {
			case strings.HasPrefix(trimmed, "E: "):
				sim.Errors = append(sim.Errors, trimmed)
			case strings.HasPrefix(trimmed, "W: "), strings.HasPrefix(trimmed, "WARNING: "):
				sim.Warnings = append(sim.Warnings, trimmed)
			}
			if header, ok := headerSection(trimmed); ok {
				section = header
				// "The following packages have unmet dependencies:" 之后是缩进细节。
				continue
			}
			// 非标题、不缩进的行：可能是"某包已是最新"这类闲话，也可能是汇总行。
			readChatter(&sim, trimmed)
			if strings.HasPrefix(trimmed, "Inst ") {
				if step, ok := parseInst(trimmed); ok {
					sim.Steps = append(sim.Steps, step)
				}
			}
			if strings.HasPrefix(trimmed, "Remv ") {
				if step, ok := parseRemv(trimmed); ok {
					sim.Steps = append(sim.Steps, step)
				}
			}
			continue
		}

		// 缩进行：要么是列表成员，要么是 unmet 细节。
		switch section {
		case sectionNew:
			sim.New = append(sim.New, strings.Fields(trimmed)...)
		case sectionAdditional:
			sim.Additional = append(sim.Additional, strings.Fields(trimmed)...)
		case sectionUpgrade:
			sim.Upgrade = append(sim.Upgrade, strings.Fields(trimmed)...)
		case sectionDowngrade:
			sim.Downgrade = append(sim.Downgrade, strings.Fields(trimmed)...)
		case sectionRemove:
			for _, name := range strings.Fields(trimmed) {
				sim.Remove = append(sim.Remove, strings.TrimSuffix(name, "*"))
			}
		case sectionHeld:
			sim.Held = append(sim.Held, strings.Fields(trimmed)...)
		case sectionKeptBack:
			if pkg, reason, ok := parseKeptBack(trimmed); ok {
				sim.KeptBack = append(sim.KeptBack, KeptBack{Package: pkg, Reason: reason})
			}
		case sectionAutoremove:
			sim.Autoremove = append(sim.Autoremove, strings.Fields(trimmed)...)
		case sectionUnmet:
			sim.Unmet = append(sim.Unmet, trimmed)
		}
	}

	sim.New = dedupeKeepOrder(sim.New)
	sim.Additional = dedupeKeepOrder(sim.Additional)
	sim.Upgrade = dedupeKeepOrder(sim.Upgrade)
	sim.Downgrade = dedupeKeepOrder(sim.Downgrade)
	sim.Remove = dedupeKeepOrder(sim.Remove)
	sim.Held = dedupeKeepOrder(sim.Held)
	sim.Autoremove = dedupeKeepOrder(sim.Autoremove)
	sort.Strings(sim.Autoremove)
	return sim
}

// headerSection 认段落标题。认不出来的标题一律 sectionNone（不猜）。
func headerSection(line string) (listSection, bool) {
	switch line {
	case "The following NEW packages will be installed:":
		return sectionNew, true
	case "The following additional packages will be installed:":
		return sectionAdditional, true
	case "The following packages will be upgraded:":
		return sectionUpgrade, true
	case "The following packages will be DOWNGRADED:":
		return sectionDowngrade, true
	case "The following packages will be REMOVED:":
		return sectionRemove, true
	case "The following held packages will be changed:":
		return sectionHeld, true
	case "The following packages have been kept back:":
		return sectionKeptBack, true
	case "The following packages were automatically installed and are no longer required:":
		return sectionAutoremove, true
	case "The following packages have unmet dependencies:":
		return sectionUnmet, true
	}
	return sectionNone, false
}

// readChatter 读那几句"什么都没发生"的闲话。
//
// 它们不影响计划，但界面上必须能说清楚"为什么这次没有变化"——否则用户看到
// 一份空的计划，会以为是面板没干活。
func readChatter(sim *Simulation, line string) {
	switch {
	case strings.HasSuffix(line, "is already the newest version.") ||
		strings.Contains(line, "is already the newest version ("):
		sim.AlreadyNewest = append(sim.AlreadyNewest, strings.Fields(line)[0])
	case strings.HasPrefix(line, "Skipping "):
		// `Skipping cowsay, it is not installed and only upgrades are requested.`
		rest := strings.TrimPrefix(line, "Skipping ")
		if pkg, _, ok := strings.Cut(rest, ","); ok {
			sim.Skipped = append(sim.Skipped, strings.TrimSpace(pkg))
		}
	case strings.HasSuffix(line, "set to manually installed."):
		sim.SetManual = append(sim.SetManual, strings.Fields(line)[0])
	}
	if counts, ok := parseSummary(line); ok {
		sim.Counts = counts
		sim.HadSummary = true
	}
}

// parseSummary 解析 `1 upgraded, 0 newly installed, 0 to remove and 8 not upgraded.`
//
// 用短语匹配而不是"下一个词"：`newly installed,` 与 `to remove` 都是两个词，
// 按位置猜出来的数字会在格式稍微一变时静默错位——而这种数字恰恰是用来做交叉核对的。
func parseSummary(line string) (Counts, bool) {
	if !strings.Contains(line, "newly installed") || !strings.HasSuffix(line, "not upgraded.") {
		return Counts{}, false
	}
	fields := strings.Fields(line)
	var counts Counts
	found := 0
	for i := 0; i+1 < len(fields); i++ {
		n, err := strconv.Atoi(fields[i])
		if err != nil {
			continue
		}
		next := fields[i+1]
		switch {
		case next == "upgraded,":
			counts.Upgraded = n
			found++
		case next == "newly" && i+2 < len(fields) && fields[i+2] == "installed,":
			counts.NewlyInstall = n
			found++
		case next == "to" && i+2 < len(fields) && fields[i+2] == "remove":
			counts.ToRemove = n
			found++
		case next == "not" && i+2 < len(fields) && fields[i+2] == "upgraded.":
			counts.NotUpgraded = n
			found++
		}
	}
	return counts, found > 0
}

// parseInst 解析 `Inst name [old] (new repo [arch])` 或 `Inst name (new repo [arch])`。
func parseInst(line string) (PlanStep, bool) {
	rest := strings.TrimPrefix(line, "Inst ")
	name, rest, ok := cutField(rest)
	if !ok {
		return PlanStep{}, false
	}
	step := PlanStep{Kind: StepInstall, Package: name}
	if strings.HasPrefix(rest, "[") {
		if old, after, ok := cutBracket(rest); ok {
			step.FromVersion = old
			step.Kind = StepUpgrade
			rest = after
		}
	}
	if strings.HasPrefix(rest, "(") {
		if inner, _, ok := cutParen(rest); ok {
			if version, _, ok := cutField(inner); ok {
				step.ToVersion = version
			}
		}
	}
	return step, true
}

// parseRemv 解析 `Remv name [version]`。
func parseRemv(line string) (PlanStep, bool) {
	rest := strings.TrimPrefix(line, "Remv ")
	name, rest, ok := cutField(rest)
	if !ok {
		return PlanStep{}, false
	}
	step := PlanStep{Kind: StepRemove, Package: name}
	if old, _, ok := cutBracket(rest); ok {
		step.FromVersion = old
	}
	return step, true
}

// parseKeptBack 解析 `  pkg (依赖原因)`。
func parseKeptBack(line string) (string, string, bool) {
	pkg, rest, ok := cutField(line)
	if !ok {
		return "", "", false
	}
	if inner, _, ok := cutParen(rest); ok {
		return pkg, strings.TrimSpace(inner), true
	}
	return pkg, "", true
}

// cutField 切出第一个空白分隔的字段。
func cutField(s string) (string, string, bool) {
	s = strings.TrimLeft(s, " \t")
	if s == "" {
		return "", "", false
	}
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' || s[i] == '\t' {
			return s[:i], strings.TrimLeft(s[i:], " \t"), true
		}
	}
	return s, "", true
}

// cutBracket 切出 `[内容]`。
func cutBracket(s string) (string, string, bool) {
	if !strings.HasPrefix(s, "[") {
		return "", s, false
	}
	end := strings.IndexByte(s, ']')
	if end < 0 {
		return "", s, false
	}
	return s[1:end], strings.TrimLeft(s[end+1:], " \t"), true
}

// cutParen 切出 `(内容)`；内容里允许嵌套一层括号（apt 会打印 `[amd64]`，不是括号，
// 但依赖说明里会出现 `(>= 1.0)`，所以按配对计数而不是找第一个 `)`）。
func cutParen(s string) (string, string, bool) {
	if !strings.HasPrefix(s, "(") {
		return "", s, false
	}
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return s[1:i], strings.TrimLeft(s[i+1:], " \t"), true
			}
		}
	}
	return "", s, false
}

// dedupeKeepOrder 去重并保持首次出现的顺序。
func dedupeKeepOrder(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, item := range in {
		if item == "" || seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
	}
	return out
}
