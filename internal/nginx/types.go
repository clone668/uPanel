package nginx

import (
	"sort"
	"strings"
	"time"
)

// Facts 是一次探测得到的**主机现状**。
//
// 它回答四个问题：装没装、在不在跑、有哪些配置文件、那些文件里声明了哪些域名。
// 它不回答“能不能用这个域名”——那是策略判断，属于 internal/proxies。事实和判断分开，
// 是因为事实可以被缓存、被展示、被复算，而判断会随时改主意。
type Facts struct {
	// Available 为 false 表示这台机器上读不到 Nginx 的配置。此时 Files、
	// ServerNames 一律为空，Reason 说明为什么——空列表绝不能被读成“没有冲突”。
	Available bool
	// ConfigMissing 表示配置目录根本不存在：这台机器上大概率没装 Nginx。
	//
	// 它和“目录存在但读不了”必须分开：前者是“确认没有配置”（因此确认没有域名冲突），
	// 后者是“不知道有什么”（因此什么都不能断言）。把两者合成一个 false，就会让人
	// 要么在没装 Nginx 的机器上白忙，要么在权限不足时放心地覆盖别人的站点。
	ConfigMissing bool
	Reason        string

	// Version 是 `nginx -v` 报的版本号（只有数字部分，例如 "1.24.0"）。
	// 拿不到就是空字符串，不编。
	Version string

	// UnitState 是 nginx.service 的 ActiveState（"active"/"inactive"/"failed"…）。
	// 空字符串表示没读到——不是“没在跑”。
	UnitState string

	// ConfigRoot 是配置根目录，通常是 /etc/nginx。
	ConfigRoot string

	// Files 是实际读到并解析过的配置文件，按路径排序。
	Files []ConfigFile

	// Includes 是配置里出现过、但本包**没有跟随**的 include 目标。
	// 只报事实，不当错误：主配置里 `include mime.types;` 天天见，那不是问题。
	// 但如果某个 include 引进了别的目录、里面又写了 server_name，这里就是漏网名单。
	Includes []Include

	// Warnings 是真正的问题：文件读不了、解析失败、文件大到没解析、目录不存在。
	// 每一条都指得出是哪个文件。
	Warnings []string

	// CollectedAt 是这些事实的采集时刻。
	CollectedAt time.Time
}

// ConfigFile 是一份解析过的配置文件。
type ConfigFile struct {
	Path string
	// Managed 为 true 表示文件名符合面板自己的命名规则（upanel-<domain>.conf）。
	// 面板只写这种文件；其余文件只读。
	Managed bool
	// SizeBytes 是文件字节数。
	SizeBytes int64
	// SHA256 是文件内容的 sha256（小写十六进制），读不到内容时为空。
	//
	// 它是**事实**，不是判断：面板拿它和自己渲染出来的字节比，就能回答“磁盘上这份
	// 文件还是我上次写的那一份吗”。没有它，唯一能说的就只有“文件存在”，而“存在”
	// 与“内容对”之间隔着一次手工编辑。
	SHA256 string
	// Error 非空表示这份文件没有解析成功，内容未知。此时 Servers 为空，
	// 且不能把“没有 server 块”当作“里面没有 server_name”。
	Error   string
	Servers []ServerBlock
}

// ServerBlock 是一个 `server { … }`，只保留判冲突用得上的两样东西。
type ServerBlock struct {
	// Line 是 `server` 这个字所在的行。
	Line int
	// NamesLine 是第一条 server_name 所在的行；没有 server_name 时为 0。
	//
	// 冲突报告指这一行而不是 `server` 那一行：用户要改的是 server_name，
	// 把他领到块的开头等于让他自己再找一遍。
	NamesLine int
	Names     []ServerName
	Listen    []ListenSpec
}

// Include 是一条没有跟随的 include。
type Include struct {
	File   string
	Line   int
	Target string
}

// NameKind 是 server_name 里一个名字的写法。
type NameKind string

const (
	// NameExact 是普通主机名：example.com、www.example.com、127.0.0.1。
	NameExact NameKind = "exact"
	// NameWildcard 是 `*.example.com`，只匹配子域，不匹配 example.com 本身。
	NameWildcard NameKind = "wildcard"
	// NameWildcardApex 是 `.example.com`，匹配 example.com 和它的子域。
	NameWildcardApex NameKind = "wildcard_apex"
	// NameWildcardTail 是 `www.example.*`，匹配这个前缀后接任意一级域名。
	NameWildcardTail NameKind = "wildcard_tail"
	// NameRegex 是 `~^www\d+\.example\.com$` 这类正则。
	NameRegex NameKind = "regex"
	// NamePlaceholder 是 `_`：占位名，不声明任何真实域名。
	NamePlaceholder NameKind = "placeholder"
	// NameOther 是本包认不出的写法（例如 `www.example.*`）。认不出就不装作认得。
	NameOther NameKind = "other"
)

// ServerName 是 server_name 里的一个条目。
type ServerName struct {
	// Raw 是文件里原本写的样子。
	Raw  string
	Kind NameKind
	// Host 是归一化后的主机名：小写、去尾点、去通配前缀。
	// 只有 NameExact、NameWildcard、NameWildcardApex、NameWildcardTail 有值。
	Host string
}

// Verdict 是“这个 server_name 会不会占住 host”的判定结果。
type Verdict int

const (
	// NotClaimed 表示它不声明这个主机名。
	NotClaimed Verdict = iota
	// Claimed 表示它声明了这个主机名——再写一个同名的 server 块就是冲突。
	Claimed
	// Undecidable 表示它的写法本包判不了（正则、认不出的写法）。
	// 判不了就说判不了，不能当“不冲突”放过去。
	Undecidable
)

// Classify 把一个 server_name 条目归一化并定类。
func Classify(raw string) ServerName {
	name := ServerName{Raw: raw}
	v := strings.TrimSpace(raw)
	if v == "" {
		name.Kind = NameOther
		return name
	}
	if v == "_" {
		name.Kind = NamePlaceholder
		return name
	}
	if strings.HasPrefix(v, "~") {
		name.Kind = NameRegex
		return name
	}
	switch {
	case strings.HasPrefix(v, "*."):
		host := normalizeHost(v[2:])
		if host == "" {
			name.Kind = NameOther
			return name
		}
		name.Kind = NameWildcard
		name.Host = host
		return name
	case strings.HasPrefix(v, "."):
		host := normalizeHost(v[1:])
		if host == "" {
			name.Kind = NameOther
			return name
		}
		name.Kind = NameWildcardApex
		name.Host = host
		return name
	case strings.HasSuffix(v, ".*"):
		// `www.example.*`：前缀后面接任意一级域名。前缀照样归一化，
		// 因为它仍然是一条能判的规则——不认它就会把一堆无关域名一起挡住。
		host := normalizeHost(v[:len(v)-1])
		if host == "" {
			name.Kind = NameOther
			return name
		}
		name.Kind = NameWildcardTail
		name.Host = host
		return name
	}
	host := normalizeHost(v)
	if host == "" {
		name.Kind = NameOther
		return name
	}
	name.Kind = NameExact
	name.Host = host
	return name
}

// normalizeHost 把主机名归一化到可比较的形式：小写、去尾点。
// 形状不像主机名时返回空字符串。
func normalizeHost(v string) string {
	h := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(v), "."))
	if h == "" || len(h) > 253 {
		return ""
	}
	labelLen := 0
	for i := 0; i < len(h); i++ {
		c := h[i]
		if c == '.' {
			if labelLen == 0 {
				return ""
			}
			labelLen = 0
			continue
		}
		if !(c >= 'a' && c <= 'z') && !(c >= '0' && c <= '9') && c != '-' {
			return ""
		}
		labelLen++
		if labelLen > 63 {
			return ""
		}
	}
	if labelLen == 0 {
		return ""
	}
	return h
}

// Verdict 判断这个 server_name 会不会占住 host（host 必须已归一化）。
func (n ServerName) Verdict(host string) Verdict {
	switch n.Kind {
	case NameExact:
		if n.Host == host {
			return Claimed
		}
		return NotClaimed
	case NameWildcard:
		if strings.HasSuffix(host, "."+n.Host) {
			return Claimed
		}
		return NotClaimed
	case NameWildcardApex:
		if n.Host == host || strings.HasSuffix(host, "."+n.Host) {
			return Claimed
		}
		return NotClaimed
	case NameWildcardTail:
		if strings.HasPrefix(host, n.Host+".") {
			return Claimed
		}
		return NotClaimed
	case NamePlaceholder:
		// `_` 不声明任何具体域名：它只是“没有名字的默认 server”。
		return NotClaimed
	default:
		return Undecidable
	}
}

// ListenSpec 是一个 `listen` 指令里判冲突用得上的部分。
type ListenSpec struct {
	// Address 是归一化后的地址：只写端口时为 "*"（表示所有地址）。
	Address string
	Port    int
	// DefaultServer 表示带 default_server 参数。
	DefaultServer bool
	// SSL 表示带 ssl 参数。
	SSL bool
	// Raw 是原文，展示用。
	Raw string
}

// ParseListen 解析一条 listen 指令的参数（不加参数时 nginx 默认 80）。
func ParseListen(args []string) ListenSpec {
	spec := ListenSpec{Address: "*", Port: 80}
	if len(args) == 0 {
		spec.Raw = ""
		return spec
	}
	spec.Raw = strings.Join(args, " ")
	addr := args[0]
	switch {
	case strings.HasPrefix(addr, "unix:"):
		// unix 域名套接字：没有端口，也不参与端口冲突判断。
		spec.Address = addr
		spec.Port = 0
	case strings.HasPrefix(addr, "["):
		// IPv6：[::]:443
		if end := strings.Index(addr, "]"); end > 0 {
			rest := addr[end+1:]
			spec.Address = addr[1:end]
			if strings.HasPrefix(rest, ":") {
				spec.Port = atoiOr(rest[1:], 0)
			}
		} else {
			spec.Address = addr
		}
	case strings.Count(addr, ":") == 1:
		i := strings.Index(addr, ":")
		spec.Address = addr[:i]
		spec.Port = atoiOr(addr[i+1:], 0)
	case strings.Contains(addr, ":"):
		// 没有方括号的 IPv6，端口无从判断。
		spec.Address = addr
		spec.Port = 0
	default:
		if p := atoiOr(addr, -1); p >= 0 {
			spec.Port = p
		} else {
			spec.Address = addr
		}
	}
	if spec.Address == "" {
		spec.Address = "*"
	}
	for _, a := range args[1:] {
		switch a {
		case "default_server", "default":
			spec.DefaultServer = true
		case "ssl":
			spec.SSL = true
		}
	}
	return spec
}

func atoiOr(s string, def int) int {
	if s == "" {
		return def
	}
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return def
		}
		n = n*10 + int(s[i]-'0')
		if n > 1<<20 {
			return def
		}
	}
	return n
}

// NameClaim 是一条“某个文件里某一行声明了这个域名”的证据。
type NameClaim struct {
	File string
	Line int
	Name ServerName
	// Managed 表示这条声明来自面板自己的文件。
	Managed bool
}

// DomainClaims 是 Lookup 的结果：占住了的、和判不了的，分开列。
//
// 分开列而不是合成一个 bool，是因为两者的后续动作不同：占住了要拒绝并指路，
// 判不了要么拒绝要么让用户确认——但绝不能静默当成“没冲突”。
type DomainClaims struct {
	Matches     []NameClaim
	Undecidable []NameClaim
}

// Lookup 在已采集的事实里查 host 被谁声明过（host 必须已归一化）。
func (f Facts) Lookup(host string) DomainClaims {
	var out DomainClaims
	for _, file := range f.Files {
		for _, srv := range file.Servers {
			line := srv.NamesLine
			if line == 0 {
				line = srv.Line
			}
			for _, name := range srv.Names {
				claim := NameClaim{File: file.Path, Line: line, Name: name, Managed: file.Managed}
				switch name.Verdict(host) {
				case Claimed:
					out.Matches = append(out.Matches, claim)
				case Undecidable:
					out.Undecidable = append(out.Undecidable, claim)
				}
			}
		}
	}
	return out
}

// ServerNames 汇总所有已声明的主机名（去重、排序），供展示与自检用。
func (f Facts) ServerNames() []string {
	seen := make(map[string]struct{}, len(f.Files)*4)
	for _, file := range f.Files {
		for _, srv := range file.Servers {
			for _, name := range srv.Names {
				if name.Host != "" {
					seen[name.Host] = struct{}{}
				}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for h := range seen {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

// BrokenFiles 返回解析失败的文件路径。冲突判定在这些文件面前是不完整的，
// 调用方必须把这个事实讲出来。
func (f Facts) BrokenFiles() []string {
	var out []string
	for _, file := range f.Files {
		if file.Error != "" {
			out = append(out, file.Path)
		}
	}
	return out
}
