package shares

// 托管 Samba 配置（P08.2）：解析现有 smb.conf、渲染托管文件、接入 include、校验候选。
//
// 这一层全是**纯函数**：输入是文件字节，输出是字节与结论，中间不碰系统。理由有两个：
//
//  1. 这些判断（“主配置里有没有那一行 include”“托管文件里有哪些段”“这个候选能不能
//     交给 testparm”）是整条写路径里最容易被写错的部分，把它们从 I/O 里剥出来，
//     单元测试才能覆盖 “注释掉的 include 不算接入” 这类真机上很难构造的情形；
//  2. 配置内容决定配置版本。渲染必须确定性：同一份定义永远渲染出同一串字节，
//     否则 config_hash 每天都在变，而“配置有没有漂移”这个问题就永远只能回答“漂了”。
//
// 写文件、跑 testparm、重载这些动系统的事在 configwrite.go。

import (
	"fmt"
	"os"
	"path"
	"sort"
	"strings"

	"upanel/internal/syscmd"
)

const (
	// ManagedConfigName 是托管配置的文件名。整个目录归面板独占：面板只写这一个文件，
	// 也只认这一个文件里的段。
	ManagedConfigName = "upanel.conf"
	// ManagedConfigPath 是托管配置的绝对路径。
	ManagedConfigPath = DefaultIncludeDir + "/" + ManagedConfigName
	// DefaultConfigBackupDir 放 smb.conf 与托管文件的备份（目录 0700，只有 root 能进）。
	// 与 fstab 的备份分开：一次“共享配置坏了”的恢复不该去翻 fstab 的备份目录。
	DefaultConfigBackupDir = "/var/lib/upanel-agent/smb-backups"

	// managedDirMode / managedFileMode 是托管目录与文件的权限。
	// 目录 0755：smbd 以 root 读它，管理员也要能读；不设可写位——只有面板写它。
	managedDirMode  os.FileMode = 0o755
	managedFileMode os.FileMode = 0o644

	// includeComment 是追加到 smb.conf 里的那一行注释。
	includeComment = "# uPanel 托管共享配置：下面一行由面板维护，删掉它会让面板创建的共享全部失效"
	// includeLinePrefix 是 include 参数的键名（Samba 接受 "include = 路径"）。
	includeLinePrefix = "include = "
	// maxIncludeDepth 是跟随既有 include 的最大层数。
	//
	// 有上限不是为了性能，是为了在“A include B、B include A”这种配置上不会转圈：
	// 这种文件在真机上出现过，Samba 自己会报错，而我们的解析器不能因此卡死。
	maxIncludeDepth = 8
	// maxConfigFileBytes 是读一个配置文件的上限。
	//
	// 限制的真正目的不是省内存，是**不把块设备当配置文件读**：一个 include 指向
	// /dev/sda 时，os.ReadFile 会一直读到内存耗尽（P04.2 撞过同一个坑，那次是
	// 代理 OOM）。所以先 stat：必须是普通文件、大小在上限之内，再读。
	maxConfigFileBytes = 1 << 20
)

// managedGlobalSettings 是托管文件 [global] 段里的键值。
//
// 三条都直接对应 PLAN.md 第 10 节的一句话（“SMB 默认关闭 guest，最低 SMB2”）：
// 前两条是硬要求，第三条关掉 usershare 的匿名入口——否则“关闭 guest”只关了一半，
// 任何能登录主机的用户仍然可以用 net usershare 建一个公开共享。
//
// 顺序固定：渲染必须确定，配置版本才稳定。
var managedGlobalSettings = []struct {
	Key   string
	Value string
	Why   string
}{
	{"server min protocol", "SMB2", "PLAN.md 第 10 节：最低 SMB2"},
	{"map to guest", "never", "PLAN.md 第 10 节：默认关闭 guest"},
	{"usershare allow guests", "no", "同一句：usershare 的匿名入口也关掉"},
}

// ManagedGlobalKeys 返回托管文件会设置的全局键（只读用途：覆盖检测与展示）。
func ManagedGlobalKeys() []string {
	out := make([]string, 0, len(managedGlobalSettings))
	for _, kv := range managedGlobalSettings {
		out = append(out, kv.Key)
	}
	return out
}

// SmbConf 是主配置（以及它 include 进来的文件）的解析结果。
//
// 只记录三件事：出现了哪些段、有哪些能展开的 include、有哪些展开不了的 include。
// 键值一概不解析——这一层不需要知道任何参数的值（值的判断交给 testparm 读回）。
type SmbConf struct {
	// Sections 是段名（原样，按出现顺序去重）。
	Sections []string
	// Includes 是能展开的 include 目标（绝对路径，按出现顺序去重）。
	Includes []string
	// Unresolved 是展开不了的 include 取值（含 % 变量或通配符）。
	Unresolved []string
	// IncludePresent 报告有没有一行**生效的** include 指向托管文件。
	//
	// 注释行不算：一个被 # 掉的 include 在 Samba 眼里不存在，把它当成“已接入”
	// 会让面板以为配置生效了，而共享其实一份都没提供。
	IncludePresent bool
	// TrailingNewline 报告文件是否以换行结尾（追加内容前要补一个）。
	TrailingNewline bool
}

// ParseSmbConf 解析一份 smb.conf 文本。
func ParseSmbConf(data []byte) SmbConf {
	out := SmbConf{TrailingNewline: len(data) > 0 && data[len(data)-1] == '\n'}
	seenSection := make(map[string]bool, 8)
	seenInclude := make(map[string]bool, 4)

	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		// 注释：# 与 ; 都是 smb.conf 的注释符（两种都真的在用）。
		if trimmed[0] == '#' || trimmed[0] == ';' {
			continue
		}
		if trimmed[0] == '[' {
			end := strings.IndexByte(trimmed, ']')
			if end < 0 {
				continue // 段名没闭合：testparm 会拒，这里不猜
			}
			name := strings.TrimSpace(trimmed[1:end])
			if name == "" || seenSection[strings.ToLower(name)] {
				continue
			}
			seenSection[strings.ToLower(name)] = true
			out.Sections = append(out.Sections, name)
			continue
		}
		key, value, ok := splitConfigLine(trimmed)
		if !ok || !strings.EqualFold(key, "include") {
			continue
		}
		if target, resolvable := resolveIncludePath(value); resolvable {
			if path.Clean(target) == ManagedConfigPath {
				out.IncludePresent = true
			}
			if !seenInclude[target] {
				seenInclude[target] = true
				out.Includes = append(out.Includes, target)
			}
			continue
		}
		out.Unresolved = append(out.Unresolved, value)
	}
	return out
}

// splitConfigLine 把 "key = value" 拆开。Samba 也接受 "key=value" 与前后空白。
func splitConfigLine(line string) (string, string, bool) {
	eq := strings.IndexByte(line, '=')
	if eq <= 0 {
		return "", "", false
	}
	return strings.TrimSpace(line[:eq]), strings.TrimSpace(line[eq+1:]), true
}

// resolveIncludePath 判断一个 include 取值能不能静态展开。
//
// 含 % 变量的（%m、%U、%H）与含通配符的都不算：它们取决于以后谁来连接、或者取决于
// shell 风格的通配（Samba 的 include **不**做通配展开，写成 *.conf 是一个常见的坑）。
// 这类 include 我们读不出来，于是“有没有同名段”也就答不全——调用方会据此给一条告警，
// 而不是假装自己看全了。
func resolveIncludePath(value string) (string, bool) {
	v := strings.TrimSpace(value)
	if v == "" || !strings.HasPrefix(v, "/") {
		return "", false
	}
	if strings.ContainsAny(v, "%*?[]") {
		return "", false
	}
	return path.Clean(v), true
}

// ConfigFileReader 读一个配置文件（生产实现先 stat 再读，见 readConfigFile）。
type ConfigFileReader func(name string) ([]byte, error)

// ConfiguredSections 收集“现有配置”里的段名（小写）及其来源文件。
//
// 托管文件本身**不算**现有配置：它就是我们要写的那份东西。把它算进去的话，
// 第二次应用同一条共享时，“同名段已存在”的判决会被自己的上次写入触发。
//
// 展开不了的 include 放在第二个返回值里。它们意味着这份清单不完整：Samba 在运行时
// 可能还有别的段，而我们看不到。调用方要把这件事说出来，不能默默当成“没有冲突”。
func ConfiguredSections(main []byte, read ConfigFileReader) (map[string]string, []string) {
	sections := make(map[string]string, 8)
	var unresolved []string

	root := ParseSmbConf(main)
	queue := append([]string(nil), root.Includes...)
	unresolved = append(unresolved, root.Unresolved...)
	for _, name := range root.Sections {
		if _, ok := sections[strings.ToLower(name)]; !ok {
			sections[strings.ToLower(name)] = DefaultConfigPath
		}
	}
	visited := map[string]bool{DefaultConfigPath: true}
	for depth := 0; depth < maxIncludeDepth && len(queue) > 0; depth++ {
		next := make([]string, 0, len(queue))
		for _, name := range queue {
			if visited[name] || name == ManagedConfigPath {
				continue
			}
			visited[name] = true
			data, err := read(name)
			if err != nil {
				// 读不到就当它不存在，但要说出来：一个 include 指向的文件读不到，
				// 意味着我们看不到其中的段。
				unresolved = append(unresolved, name)
				continue
			}
			parsed := ParseSmbConf(data)
			for _, sec := range parsed.Sections {
				lower := strings.ToLower(sec)
				if _, ok := sections[lower]; !ok {
					sections[lower] = name
				}
			}
			unresolved = append(unresolved, parsed.Unresolved...)
			next = append(next, parsed.Includes...)
		}
		queue = next
	}
	if len(queue) > 0 {
		unresolved = append(unresolved, queue...)
	}
	return sections, dedupeStrings(unresolved)
}

// RenderManagedConfig 渲染整份托管配置：一个 [global] 段加每个共享一段。
//
// 段按共享名（小写比较）排序，这样同一组定义永远渲染出同一串字节。段与段之间空一行，
// 人读起来分得清，diff 也干净。
func RenderManagedConfig(shares []ManagedShare) string {
	sorted := append([]ManagedShare(nil), shares...)
	sort.SliceStable(sorted, func(i, j int) bool {
		li, lj := strings.ToLower(sorted[i].Name), strings.ToLower(sorted[j].Name)
		if li == lj {
			return sorted[i].Name < sorted[j].Name
		}
		return li < lj
	})

	var b strings.Builder
	b.WriteString("# 由 uPanel 生成，每次应用共享时整份重写：手工修改会在下次应用时被覆盖。\n")
	b.WriteString("# 这个文件归面板独占，不要在它里面放别的东西；删掉它会让面板创建的共享全部消失。\n")
	b.WriteString("\n[global]\n")
	for _, kv := range managedGlobalSettings {
		fmt.Fprintf(&b, "   # %s\n", kv.Why)
		fmt.Fprintf(&b, "   %s = %s\n", kv.Key, kv.Value)
	}
	for _, s := range sorted {
		b.WriteString("\n")
		b.WriteString(strings.TrimRight(s.Block, "\n"))
		b.WriteString("\n")
	}
	return b.String()
}

// SectionNames 返回托管文件里的段名（不含 global）。用于展示与漂移判断。
func SectionNames(data []byte) []string {
	parsed := ParseSmbConf(data)
	out := make([]string, 0, len(parsed.Sections))
	for _, s := range parsed.Sections {
		if strings.EqualFold(s, "global") {
			continue
		}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// IncludeBlock 是要追加到 smb.conf 末尾的那两行（注释 + include）。
//
// 为什么是“追加到末尾”而不是插在 [global] 后面：Samba 对同一个参数在全局段里出现
// 多次时，**后面的生效**（真机验证过：主配置写 map to guest = bad user，托管文件
// 后面写 never，读回来是 Never）。追加到末尾因此同时满足两件事：最小改动（一行
// include + 一行注释），以及“托管配置的全局项确实能盖住默认发放版的取值”。
//
// 前导换行是刻意的：主配置的最后一行如果没有换行，直接拼上去会把两行粘成一行。
func IncludeBlock() string {
	return "\n" + includeComment + "\n" + includeLinePrefix + ManagedConfigPath + "\n"
}

// PlanInclude 计算接入后的主配置内容。
//
// 返回 (候选内容, 是否改动)。已经接入时原样返回，一个字节都不动——这既是幂等，
// 也是“已有配置保留”最直接的证明：没必要的重写就是没必要的风险。
func PlanInclude(current []byte, conf SmbConf) ([]byte, bool) {
	if conf.IncludePresent {
		return current, false
	}
	body := current
	if len(body) > 0 && !conf.TrailingNewline {
		body = append(append([]byte(nil), body...), '\n')
	}
	candidate := make([]byte, 0, len(body)+len(IncludeBlock()))
	candidate = append(candidate, body...)
	candidate = append(candidate, IncludeBlock()...)
	return candidate, true
}

// testparmFatalMarkers 是 testparm 输出里“这份配置不能用”的标记。
//
// 为什么不能只看退出码：真机验证过，`testparm -s` 对**未知参数**只打印
// “Unknown parameter encountered” + “Ignoring unknown parameter”，退出码仍然是 0
// （段头写坏才是 1）。只看退出码就会把一个被静默忽略的配置当成校验通过——而
// “被静默忽略”正是最坏的一种失败：共享起来了，但少了那条限制。
var testparmFatalMarkers = []string{
	"Unknown parameter",
	"Ignoring unknown parameter",
	"Error loading services",
	"Error:",
	"can't parse",
	"Parameter not found",
	"Load failed",
}

// testparmSuccessMarker 是 testparm 成功时的固定输出。
var testparmSuccessMarker = "Loaded services file OK."

// TestparmVerdict 判定一次 testparm 运行是否真的通过。
//
// 三条一起才算通过：退出码 0、出现成功标记、没有致命标记。
func TestparmVerdict(res syscmd.Result, err error) error {
	combined := res.Stdout + "\n" + res.Stderr
	for _, marker := range testparmFatalMarkers {
		if strings.Contains(combined, marker) {
			return fmt.Errorf("%s：%s", FailConfigValidation, firstLineContaining(combined, marker))
		}
	}
	if err != nil {
		return fmt.Errorf("%s：testparm 以错误结束：%v", FailConfigValidation, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("%s：testparm 退出码 %d", FailConfigValidation, res.ExitCode)
	}
	if !strings.Contains(combined, testparmSuccessMarker) {
		return fmt.Errorf("%s：testparm 没有报 “%s”，输出为 %q",
			FailConfigValidation, testparmSuccessMarker, truncateForMessage(combined))
	}
	return nil
}

// firstLineContaining 取出含某个标记的那一行，用于错误消息。
func firstLineContaining(text, marker string) string {
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, marker) {
			return strings.TrimSpace(line)
		}
	}
	return marker
}

// truncateForMessage 把一段输出截到一条消息该有的长度。
func truncateForMessage(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " / "))
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}

// dedupeStrings 去重并保持顺序。
func dedupeStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// readConfigFile 读一个配置文件，拒绝非普通文件与超大文件。
//
// 这是 ConfiguredSections 的生产读取口：include 的路径来自 /etc/samba 下的配置文件
// （root 拥有），不是 HTTP 参数，但“配置里写了一个路径”与“那个路径是一个文件”
// 是两件事——`include = /dev/sda` 是一个把代理读成 OOM 的合法配置文件。
func readConfigFile(name string) ([]byte, error) {
	fi, err := os.Stat(name)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("shares: %s 不是普通文件（%s），拒绝读取", name, fi.Mode().Type())
	}
	if fi.Size() > maxConfigFileBytes {
		return nil, fmt.Errorf("shares: %s 大小 %d 字节，超过配置文件上限 %d", name, fi.Size(), maxConfigFileBytes)
	}
	return os.ReadFile(name)
}
