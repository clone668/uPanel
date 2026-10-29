package shares

// NFS 导出（P14.4）。
//
// 这一半和 SMB 那一半共用“定义 → 预检 → 执行 → 读回”的框架，但配置面完全不同：
//
//   - 只维护 `/etc/exports.d/upanel.exports` 一个文件（PLAN.md 第 10 节），
//     **不碰** /etc/exports 本身，也不整体重写别人的导出；
//   - 每一行的行尾带 `# upanel:<共享 id>` 标记，这是面板认领这一行的唯一依据
//     （`exportfs` 会忽略行尾注释，VM 上实测过）；
//   - 应用之后不看退出码，而是用 `exportfs -v` 读回**实际生效的导出表**再比对。
//
// 为什么标记必须写在行尾而不是单独一行：单行标记与它管的那一行之间没有任何强制绑定，
// 一次手工编辑就能让标记和导出错位（标记说这是 A 的，下一行却是 B 的目录）。行尾标记
// 不会错位。VM 实测：`/srv/t2 10.0.2.0/24(ro,...) # trailing` 被 `exportfs -ra`
// 正常接受，导出选项按括号里的来。
//
// 这个文件是纯函数层：不碰文件系统、不跑命令。所有“真的动手”的部分在 exports.go。

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"
)

// NFS 的固定路径。与 Samba 那几个常量一样，是代码里的常量，不来自配置也不来自 HTTP。
const (
	// DefaultExportsFile 是面板独占的导出文件。文件名固定，因为“哪一份是面板的”
	// 必须是一个常量，而不是从内容里猜出来的。
	DefaultExportsFile = "/etc/exports.d/upanel.exports"
	// DefaultExportsDir 是 exports.d 目录（可能还不存在，应用时创建）。
	DefaultExportsDir = "/etc/exports.d"
	// DefaultExportsMain 是系统主导出文件。面板**只读**它，用于冲突检测。
	DefaultExportsMain = "/etc/exports"
	// exportMarkerPrefix 是行尾标记的名字部分。
	exportMarkerPrefix = "upanel:"
	// exportsFileSuffix 是 exportfs 会自动收进来的后缀。
	//
	// 这是实现细节里最要紧的一条：`/etc/exports.d/` 里的文件只有以 `.exports`
	// 结尾才会被读到（VM 实测：改名成 .conf 之后导出就消失）。
	exportsFileSuffix = ".exports"
)

// 导出的选项闭集。
//
// 没有 `no_root_squash`、没有 `async`、没有 `insecure`、没有 `*`——这些不是
// “默认关掉的可选项”，而是**根本不存在**的取值（PLAN.md 第 10 节）。
// 要放开它们需要改代码，而不是改一次请求的参数。
const (
	ExportOptRW         = "rw"
	ExportOptRO         = "ro"
	ExportOptSync       = "sync"
	ExportOptRootSquash = "root_squash"
	// ExportOptNoSubtree 是唯一一个“不是安全开关”的选项，写出来是为了让 exportfs
	// 的默认值显式化：不写的话不同版本行为不同，而子树检查在共享子目录上会导致
	// 客户端访问到不该访问的路径。
	ExportOptNoSubtree = "no_subtree_check"
)

// ExportEntry 是导出表里的一行（从任意 exports 文件里解析出来的）。
type ExportEntry struct {
	// Path 是导出目录（绝对路径）。
	Path string
	// Specs 是这一行剩下的部分，形如 `10.0.0.0/24(rw,sync,root_squash)`。
	Specs []string
	// ManagedID 是行尾标记认领的共享 id；不是面板写的行就是空串。
	ManagedID string
}

// Managed 报告这一行是不是面板自己写的。
func (e ExportEntry) Managed() bool { return e.ManagedID != "" }

// ManagedExport 是一条准备写进托管导出文件的导出。
type ManagedExport struct {
	// ShareID 是登记表里的共享 id，写在行尾标记里。
	ShareID string
	// Name 是共享名（只用于消息与排序）。
	Name string
	// Dir 是导出目录的绝对路径。
	Dir string
	// MountPath 是这条共享所在卷的挂载点（启动依赖用）。
	MountPath string
	// CIDRs 是允许的客户端网段（已验证过是网络地址形式）。
	CIDRs []string
	// ReadOnly 决定 rw / ro。
	ReadOnly bool
	// Line 是渲染出来的一行（含行尾标记）。
	Line string
}

// ExportOptions 返回这条导出的选项（固定顺序，渲染是确定的）。
func (e ManagedExport) ExportOptions() []string {
	out := make([]string, 0, 4)
	if e.ReadOnly {
		out = append(out, ExportOptRO)
	} else {
		out = append(out, ExportOptRW)
	}
	out = append(out, ExportOptSync, ExportOptRootSquash, ExportOptNoSubtree)
	return out
}

// ExportedDirError 描述“这个目录没法写进导出文件”的原因。
//
// 单独一个错误类型是因为它有两种处置：路径形状问题（校验阶段就该拦），
// 以及“目录里出现了 exports 语法表达不了的字节”（我们自己的路径校验漏了）。
type ExportedDirError struct {
	Reason string
}

func (e *ExportedDirError) Error() string { return e.Reason }

// ValidateExportDir 校验一个目录能不能无歧义地写进 exports。
//
// exports 是空白分隔的，注释以 `#` 开始，所以这三样东西在路径里会改变整行的含义：
// 空白（被拆成两个字段，第二个会被当成客户端）、`#`（后面全成注释）、引号
// （不同版本对引号的处理不一致）。面板生成的目录来自卷挂载点与相对路径，
// 三者都不该出现；出现就说明上游校验漏了，这里兜住而不是硬写。
func ValidateExportDir(dir string) error {
	if !strings.HasPrefix(dir, "/") {
		return &ExportedDirError{Reason: "导出目录必须是绝对路径：" + dir}
	}
	if dir == "/" {
		return &ExportedDirError{Reason: "不允许导出根目录 /"}
	}
	if strings.ContainsAny(dir, " \t\r\n") {
		return &ExportedDirError{Reason: "导出目录里有空白字符，exports 会把它拆成两个字段：" + dir}
	}
	if strings.ContainsRune(dir, '#') {
		return &ExportedDirError{Reason: "导出目录里有 #，exports 会把它后面全部当注释：" + dir}
	}
	if strings.ContainsAny(dir, `"'`) {
		return &ExportedDirError{Reason: "导出目录里有引号，不同版本的 exports 解析不一致：" + dir}
	}
	if strings.ContainsRune(dir, '\x00') {
		return &ExportedDirError{Reason: "导出目录里有 NUL 字节"}
	}
	return nil
}

// RenderExportLine 渲染一条导出行。
//
// 网段先去重再排序：同一份定义无论在什么顺序下提交，渲染出来的字节必须一样——
// 配置哈希是这份定义的版本标识，顺序抖动会让“没改”变成“改了”。
func RenderExportLine(e ManagedExport) (string, error) {
	if !ValidShareID(e.ShareID) {
		return "", fmt.Errorf("共享标识 %q 不合法，无法写标记", e.ShareID)
	}
	if err := ValidateExportDir(e.Dir); err != nil {
		return "", err
	}
	cidrs, err := NormalizeCIDRs(e.CIDRs)
	if err != nil {
		return "", err
	}
	if len(cidrs) == 0 {
		return "", &ExportedDirError{Reason: "导出没有允许网段：NFS 不接受空网段（那等于向全世界导出）"}
	}
	spec := "(" + strings.Join(e.ExportOptions(), ",") + ")"
	parts := make([]string, 0, len(cidrs)+1)
	parts = append(parts, e.Dir)
	for _, c := range cidrs {
		parts = append(parts, c+spec)
	}
	return strings.Join(parts, " ") + " # " + exportMarkerPrefix + e.ShareID, nil
}

// NormalizeCIDRs 去重、排序并校验一组网段。
func NormalizeCIDRs(in []string) ([]string, error) {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, c := range in {
		s := strings.TrimSpace(c)
		if s == "" {
			return nil, &ExportedDirError{Reason: "允许网段里不能有空串"}
		}
		if err := ValidateCIDR(s); err != nil {
			return nil, err
		}
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out, nil
}

// exportsHeader 是托管导出文件的固定开头。
//
// 它必须自己也是注释行：exports 的解析器会把非注释、非空的第一行当成一条导出。
var exportsHeader = []string{
	"# 由 uPanel 维护：整份文件由面板重写，手工改动会在下一次应用时被覆盖。",
	"# 行尾的 `# upanel:<共享 id>` 是面板认领这一行的唯一依据；没有标记的行面板不碰。",
	"# 要手工添加导出，请另建一个 .exports 文件——同一条路径不要两边都写（exportfs 会报重复）。",
}

// RenderExportsFile 渲染整份托管导出文件（确定性：同一组导出永远同样的字节）。
func RenderExportsFile(exports []ManagedExport) string {
	lines := make([]string, 0, len(exports)+len(exportsHeader)+1)
	lines = append(lines, exportsHeader...)
	for _, e := range exports {
		if e.Line != "" {
			lines = append(lines, e.Line)
		}
	}
	return strings.Join(lines, "\n") + "\n"
}

// ParseExports 解析一份 exports 文件的内容。
//
// 返回解析出来的条目与被跳过的行（不是注释、但没有以 `/` 开头）。被跳过的行不静默
// 丢掉：冲突检测要靠它，而“有一行我读不懂，所以不知道有没有冲突”与“没有冲突”
// 是两件事，前者必须以告警的形式说出来。
func ParseExports(data []byte) ([]ExportEntry, []string) {
	var (
		entries  []ExportEntry
		unparsed []string
	)
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimRight(raw, " \t\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		var managed string
		if i := commentStart(line); i >= 0 {
			if id, ok := markerID(line[i+1:]); ok {
				managed = id
			}
			line = line[:i]
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if !strings.HasPrefix(fields[0], "/") {
			unparsed = append(unparsed, trimmed)
			continue
		}
		entries = append(entries, ExportEntry{
			Path:      fields[0],
			Specs:     fields[1:],
			ManagedID: managed,
		})
	}
	return entries, unparsed
}

// commentStart 返回行尾注释的 `#` 下标；没有注释返回 -1。
//
// 只认“前面是空白或行首”的 `#`：目录名里带 `#` 的情形由 ValidateExportDir 提前拒绝，
// 但别人的文件可能不那么干净，所以这里按 exports 自己的规则（`#` 必须在空白后）判断。
func commentStart(line string) int {
	for i := 0; i < len(line); i++ {
		if line[i] != '#' {
			continue
		}
		if i == 0 || line[i-1] == ' ' || line[i-1] == '\t' {
			return i
		}
	}
	return -1
}

// markerID 从注释文本里认出面板标记。
func markerID(comment string) (string, bool) {
	s := strings.TrimSpace(comment)
	if !strings.HasPrefix(s, exportMarkerPrefix) {
		return "", false
	}
	id := strings.TrimSpace(strings.TrimPrefix(s, exportMarkerPrefix))
	if !ValidShareID(id) {
		return "", false
	}
	return id, true
}

// ExportConflicts 找出“别的导出文件里已经导出、与 desired 同路径”的那些。
//
// 为什么要拦：exportfs 对同一条路径被导出两次报 `duplicated export entries` 并以 1
// 退出（VM 实测），整份导出表的刷新因此失败——**别人原本好用的导出也一起停止刷新**。
// 这种失败在预检里能看见，就绝不该留到执行时。
func ExportConflicts(desired []ManagedExport, others []ExportEntry) map[string][]string {
	want := make(map[string][]string, len(desired))
	for _, e := range desired {
		want[e.Dir] = append(want[e.Dir], e.Name)
	}
	out := make(map[string][]string)
	for _, o := range others {
		if o.Managed() {
			// 面板自己文件里的行由“整份重写”负责，不参与冲突判断。
			continue
		}
		if names, ok := want[o.Path]; ok {
			out[o.Path] = names
		}
	}
	return out
}

// SameDirShares 找出 desired 里被不止一条共享占着的目录。
//
// 与 ExportConflicts 是两种不同的重复：那个是“面板撞上了别人的导出文件”，这个是
// “面板自己的两条共享指向同一个目录”。后果一模一样——exportfs 报 `duplicated
// export entries` 并以 1 退出，整份导出表的刷新失败，连别人的导出一起停摆。
//
// 之所以不能在渲染时靠去重“和稀泥”：两条共享的网段、只读标记可以不同，合并成
// 一行等于替用户猜他想留哪一条。这里只做判定，让调用方在预检就把话说明白。
func SameDirShares(exports []ManagedExport) map[string][]string {
	byDir := make(map[string][]string, len(exports))
	seen := make(map[string]bool, len(exports))
	for _, e := range exports {
		// 同一条共享渲染多次（多网段各一行之类）不算冲突，按名字去一次重。
		key := e.Dir + "\x00" + e.Name
		if seen[key] {
			continue
		}
		seen[key] = true
		byDir[e.Dir] = append(byDir[e.Dir], e.Name)
	}
	out := make(map[string][]string)
	for dir, names := range byDir {
		if len(names) > 1 {
			out[dir] = names
		}
	}
	return out
}

// ExportClient 是导出表里一条“客户端 → 选项”的记录（来自 `exportfs -v`）。
type ExportClient struct {
	// Client 是客户端写法（网段或主机名）。
	Client string
	// Options 是括号里的选项原文。
	Options string
}

// ExportTable 是 `exportfs -v` 解析出来的实际导出表：路径 → 客户端列表。
type ExportTable map[string][]ExportClient

// ExportTablePaths 返回导出表里的路径（排序）。
func (t ExportTable) ExportTablePaths() []string {
	out := make([]string, 0, len(t))
	for p := range t {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// ParseExportfsV 解析 `exportfs -v` 的输出。
//
// 输出形状（VM 实测）是“路径一行，缩进的客户端行跟在后面”：
//
//	/srv/t1       	10.0.2.0/24(sync,wdelay,hide,no_subtree_check,sec=sys,rw,secure,root_squash,no_all_squash)
//	/srv/upanel-nfs
//			<world>(async,...,ro,secure,root_squash,no_all_squash)
//
// 所以解析要认“行首是不是空白”而不是按行数分组。
func ParseExportfsV(out string) (ExportTable, error) {
	table := ExportTable{}
	var current string
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimRight(raw, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			// 新的一段：第一个字段是路径。
			fields := strings.Fields(line)
			current = fields[0]
			if _, ok := table[current]; !ok {
				table[current] = nil
			}
			line = strings.TrimSpace(strings.TrimPrefix(line, current))
			if line == "" {
				continue
			}
		}
		if current == "" {
			return nil, fmt.Errorf("exportfs -v 的第一行不是路径：%q", raw)
		}
		for _, spec := range strings.Fields(line) {
			client, opts, ok := splitClientSpec(spec)
			if !ok {
				return nil, fmt.Errorf("exportfs -v 里读不懂的条目：%q", spec)
			}
			table[current] = append(table[current], ExportClient{Client: client, Options: opts})
		}
	}
	return table, nil
}

// splitClientSpec 把 `10.0.0.0/24(rw,sync)` 拆成客户端与选项。
func splitClientSpec(spec string) (string, string, bool) {
	open := strings.IndexByte(spec, '(')
	if open <= 0 || !strings.HasSuffix(spec, ")") {
		return "", "", false
	}
	return spec[:open], spec[open+1 : len(spec)-1], true
}

// VerifyExport 核对导出表里这条导出的实际选项。
//
// 返回空串表示通过，否则是一句人读的不符说明。判据是“真的生效了什么”，
// 不是“文件里写了什么”：root_squash 被 no_root_squash 抵掉就是没 squash，
// sync 被 async 抵掉就是不保证落盘——这两条都不是风格问题。
func VerifyExport(table ExportTable, e ManagedExport) string {
	clients, ok := table[e.Dir]
	if !ok {
		return "导出表里没有 " + e.Dir
	}
	cidrs, err := NormalizeCIDRs(e.CIDRs)
	if err != nil {
		return "允许网段不合法：" + err.Error()
	}
	for _, want := range cidrs {
		var found *ExportClient
		for i := range clients {
			if clients[i].Client == want {
				found = &clients[i]
				break
			}
		}
		if found == nil {
			return fmt.Sprintf("%s 没有对 %s 的导出", e.Dir, want)
		}
		if why := verifyExportOptions(found.Options, e.ReadOnly); why != "" {
			return fmt.Sprintf("%s 对 %s 的实际选项不对：%s", e.Dir, want, why)
		}
	}
	return ""
}

// verifyExportOptions 核对一条客户端条目的选项集合。
func verifyExportOptions(options string, readOnly bool) string {
	has := make(map[string]bool, 12)
	for _, tok := range strings.Split(options, ",") {
		tok = strings.TrimSpace(tok)
		if tok != "" {
			has[tok] = true
		}
	}
	if has["no_root_squash"] {
		return "实际是 no_root_squash（root 不会被 squash）"
	}
	if !has[ExportOptRootSquash] {
		return "实际没有 root_squash"
	}
	if has["async"] {
		return "实际是 async（不保证落盘）"
	}
	if !has[ExportOptSync] {
		return "实际没有 sync"
	}
	switch {
	case has[ExportOptRO] && has[ExportOptRW]:
		return "实际同时有 ro 与 rw"
	case readOnly && !has[ExportOptRO]:
		return "定义是只读，实际不是 ro"
	case !readOnly && !has[ExportOptRW]:
		return "定义是可写，实际不是 rw"
	}
	return ""
}

// ValidateNFSOptions 校验 NFS 的选项块。
//
// 这一版只接受 root_squash=true 与 sync=true（CONTRACTS §7）。两个字段都是 bool，
// 所以“no_root_squash”在结构上根本表达不出来；这里的校验挡住的是 false 与缺省 nil，
// 它们的含义是“不做 squash”与“不保证落盘”，都不是这个面板会做的事。
func ValidateNFSOptions(o *NFSOptions) error {
	if o == nil {
		return newDefError(NoticeNFSOptionsInvalid, "nfs 共享必须带 nfs_options（root_squash=true、sync=true）")
	}
	if !o.RootSquash {
		return newDefError(NoticeNFSOptionsInvalid,
			"只允许 root_squash=true：no_root_squash 会让客户端 root 拥有导出目录的全部权力")
	}
	if !o.Sync {
		return newDefError(NoticeNFSOptionsInvalid,
			"只允许 sync=true：async 会让客户端以为写成功了，而数据还在服务端内存里")
	}
	return nil
}

// ValidateNFSDefinition 校验 NFS 共享的定义部分。
func ValidateNFSDefinition(d ShareDefinition) error {
	if err := ValidateNFSOptions(d.NFSOptions); err != nil {
		return err
	}
	if len(d.SMBUsers) > 0 {
		return newDefError(NoticeNFSUsersNotAllowed,
			"nfs 共享不接受 smb_users：NFS 的访问控制是网段 + root_squash，没有 Samba 账号这一层")
	}
	if len(d.AllowedCIDRs) == 0 {
		return newDefError(NoticeShareNoCIDRs,
			"nfs 共享必须点名允许的网段：不填等于向全世界导出")
	}
	for _, c := range d.AllowedCIDRs {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(c))
		if err != nil {
			// ValidateCIDR 会给出更具体的说法，这里只为拿到前缀长度。
			continue
		}
		if prefix.Bits() == 0 {
			return newDefError(NoticeNFSOpenToWorld,
				"允许网段 %s 覆盖全部地址：NFS 不接受向全世界导出，请写具体网段", c)
		}
	}
	return nil
}
