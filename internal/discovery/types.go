// Package discovery 发现本机正在对外提供服务的对象：监听中的端口、背后的 systemd
// 单元、以及 Docker 里的容器（P09.2）。
//
// 这一层只做**发现**：它读到什么就说什么，不判断“这个该不该被面板接管”，也不写任何
// 东西。所有写动作都是人在 Web 上确认候选之后走 P09.1 的元数据接口完成的——发现结果
// 本身不是应用记录，候选列表里没有出现“已创建”的状态。
//
// 三条本包负责守住的事：
//
//  1. **探测目标的地址永远由代理自己算出来**。候选的 target_id 形如 local_nginx 或
//     cntr_blog，它指向的是“本机上我看到的这个东西”，不是任何调用方给的 host。探测
//     时只有回环地址：单元和裸监听走 127.0.0.1，容器走它发布的宿主机端口再落到
//     127.0.0.1。于是 169.254.169.254（云元数据）、内网其它主机、任意外部域名在这条
//     路径上根本没有出现的位置——不是“被过滤”，是结构上不存在（SSRF 的正面约束）。
//  2. **发现失败要说清楚是哪一类失败**。“没装 docker”“docker 没在跑”“systemctl 读
//     不出来”是三件不同的事，前端要能分别告诉管理员。全部失败时快照状态是 error，
//     部分失败是 partial，并且每个来源各自带 status/error。
//  3. **不猜端口对应哪个应用**。一个裸监听端口背后可能是一个跑在 screen 里的临时
//     脚本，面板不替它编名字：进程名读不到就叫 local_port-8080，说明里写“读不到持有
//     进程”。名称是给人看的提示，不是身份。
package discovery

import (
	"regexp"
	"sort"
	"strings"
	"time"
)

// 快照状态（与 storage/services 的同一套语义）。
const (
	// StatusOK 表示所有来源都成功采集。
	StatusOK = "ok"
	// StatusPartial 表示至少一个来源失败，其余来源的结果仍然可用。
	StatusPartial = "partial"
	// StatusError 表示没有任何一个来源成功——这份快照不能当作“本机什么都没有”。
	StatusError = "error"
)

// 来源标识。它们是“这个候选怎么被发现的”的唯一依据，前端原样显示。
const (
	// SourceSystemd 来自 systemd 单元：单元在 active，且主进程持有监听套接字。
	SourceSystemd = "systemd"
	// SourceLocalListen 来自裸监听：/proc/net/tcp* 里有监听，但背后没有一个面板能
	// 认出来的单元（可能是用户自己的脚本、screen/tmux 里的进程、或者 cgroup 里读不
	// 到单元名）。
	SourceLocalListen = "local_listen"
	// SourceDocker 来自 Docker：docker ps 里存在的容器。
	SourceDocker = "docker"
)

// 候选类型。与 apps.Type 不同：这里的 kind 说的是“发现到的对象是什么”，
// 而应用类型说的是“用户把它登记成什么”。
const (
	// KindSystemd 是 systemd 单元候选。
	KindSystemd = "systemd"
	// KindDocker 是容器候选。
	KindDocker = "docker"
	// KindLocal 是裸监听候选。
	KindLocal = "local"
)

// 观测到的状态闭集（与 apps 的 runtime 枚举同词，含义一致）。
const (
	ObservedRunning = "running"
	ObservedStopped = "stopped"
	ObservedFailed  = "failed"
	ObservedUnknown = "unknown"
)

// 协议与监听地址的上限。
const (
	// MaxCandidates 是单次快照收录的候选上限。超过时截断并置 Truncated=true：
	// 宁可明说“只列了前 N 个”，也不要把一台机器渲染成几千行。
	MaxCandidates = 500
	// MaxListenersPerCandidate 是单个候选最多列出的监听项（一个进程可能同时监听
	// IPv4/IPv6/多端口）。
	MaxListenersPerCandidate = 16
	// maxSocketScan 是 inode→pid 反查时最多扫描的进程数，防止在进程风暴时把代理拖住。
	maxSocketScan = 8192
)

// TargetIDPattern 是探测目标标识的形状，与 CONTRACTS.md 第 8 节完全一致。
//
// 这个正则同时是 Web 侧写入 health_check.target_id 的校验：主机名、IP、端口、
// 完整 URL 都表达不出来，因此“让面板去请求任意地址”不是被拦下，而是没有地方可写。
var TargetIDPattern = regexp.MustCompile(`^(local|cntr)_[a-z0-9][a-z0-9_-]{0,47}$`)

// 目标前缀。
const (
	// TargetLocalPrefix 指向本机回环上的一个端口。
	TargetLocalPrefix = "local_"
	// TargetContainerPrefix 指向一个容器发布的宿主机端口。
	TargetContainerPrefix = "cntr_"
)

// ValidTargetID 校验目标标识的形状。
func ValidTargetID(id string) bool { return TargetIDPattern.MatchString(id) }

// Listener 是一个监听套接字。地址是内核里的地址原样（例如 127.0.0.1 / 0.0.0.0 / ::1）。
type Listener struct {
	Address  string `json:"address"`
	Port     int    `json:"port"`
	Protocol string `json:"protocol"`
	// Loopback 表示这个监听能不能从 127.0.0.1 上连到。0.0.0.0 与 :: 算能（它们包含
	// 回环），只绑在某个具体外网地址上的不算。
	Loopback bool `json:"loopback"`
}

// Candidate 是一个“看起来是个能访问的服务”的对象。
//
// 它带着足够的上下文让人判断要不要登记：来源、名称、状态、端口、以及**为什么**它出现
// 在这里。没有 bound_app_id 之类的字段——候选列表不知道面板里已经有什么，那是 Web 侧
// 合并的活。
type Candidate struct {
	// TargetID 是探测目标标识（local_… / cntr_…），形状见 TargetIDPattern。
	TargetID string `json:"target_id"`
	// Kind 是发现到的对象类型：systemd / docker / local。
	Kind string `json:"kind"`
	// Source 是发现来源：systemd / docker / local_listen。
	Source string `json:"source"`
	// Name 是对象名：单元名、容器名、或进程名（读不到时为空）。
	Name string `json:"name"`
	// Title 是给人看的建议名称（不含 .service 后缀、不含容器 ID 前缀）。
	Title string `json:"title"`
	// Reason 是发现来源的说明，一句话，写清“凭什么把它列出来”。
	Reason string `json:"reason"`
	// ObservedState 是运行状态；systemd 取 ActiveState，容器取 docker 状态，
	// 裸监听恒为 running（有监听套接字就是有进程在听）。
	ObservedState string `json:"observed_state"`
	// Listeners 是关联的监听项，按端口排序。
	Listeners []Listener `json:"listeners"`
	// SuggestedScheme / SuggestedPort 是新建应用时的建议值（443/8443 → https）。
	SuggestedScheme string `json:"suggested_scheme"`
	SuggestedPort   int    `json:"suggested_port"`
	// ProbeAvailable 表示面板能不能从回环上探测这个候选。为 false 时必须给人看
	// ProbeNote：候选照样列出来（发现是真的），但“点一下就能配健康检查”是假的。
	ProbeAvailable bool   `json:"probe_available"`
	ProbeNote      string `json:"probe_note"`
	// Unit 是 systemd 单元名（kind=systemd 时非空）。
	Unit string `json:"unit,omitempty"`
	// ContainerID 是完整容器 ID（kind=docker 时非空）。
	ContainerID string `json:"container_id,omitempty"`
}

// SourceState 是一个来源的采集结果。
type SourceState struct {
	Source string `json:"source"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
	Count  int    `json:"count"`
}

// Snapshot 是一次发现的结果。
type Snapshot struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
	// CollectedAt 是这次采集的时间。失败时仍然是被请求的时间，因为“什么时候看过”
	// 与“看到了什么”是两件事。
	CollectedAt time.Time     `json:"collected_at"`
	Sources     []SourceState `json:"sources"`
	Candidates  []Candidate   `json:"candidates"`
	// Truncated 表示候选被截断（超过 MaxCandidates）。
	Truncated bool `json:"truncated"`
	// Targets 是 target_id → 探测目标的解析表。它只在代理内部使用，不发给 Web：
	// Web 需要的是“能不能探测”，而不是“探测到哪个地址”。
	Targets map[string]Target `json:"-"`
}

// Target 是探测目标的解析结果：地址由代理自己算，端口来自应用元数据。
type Target struct {
	// ID 是目标标识。
	ID string
	// Kind 是 candidate kind（systemd / docker / local）。
	Kind string
	// Name 是对象名（单元名 / 容器名 / 进程名）。
	Name string
	// Address 是可以用来发起 HTTP 请求的地址。当前实现只会给出 127.0.0.1：
	// 端口映射与监听都在回环上可达，因此不需要也不允许探测其它地址。
	Address string
	// PublishedPorts 是容器发布的宿主机端口（kind=docker）。探测时应用的 port
	// 必须落在这个集合里，否则说明端口配错了，而不是“容器没起”。
	PublishedPorts []int
	// State 是发现时刻的状态，用于给探测失败补一句人话。
	State string
}

// TargetsOf 把候选整理成 target_id → 探测目标。
func TargetsOf(candidates []Candidate) map[string]Target {
	out := make(map[string]Target, len(candidates))
	for _, c := range candidates {
		ports := make([]int, 0, len(c.Listeners))
		for _, l := range c.Listeners {
			// 只有能从回环连到的监听才算“可探测端口”。绑在某个具体外网地址上
			// 的监听，127.0.0.1 上并没有它——那不是配置错误，是这台机器的现状。
			if l.Loopback {
				ports = append(ports, l.Port)
			}
		}
		out[c.TargetID] = Target{
			ID:             c.TargetID,
			Kind:           c.Kind,
			Name:           c.Name,
			Address:        "127.0.0.1",
			PublishedPorts: dedupeInts(ports),
			State:          c.ObservedState,
		}
	}
	return out
}

// slugify 把任意对象名变成一个符合 target_id 后缀字符集的片段。
//
// 规则：小写；[a-z0-9] 保留；其余字符折成 - ；连续 - 合并；去掉首尾 -；长度截到 46
// （给去重后缀 -2 留位置）。空结果用 p 开头兜底，因为 target_id 要求首字符是
// [a-z0-9]。
func slugify(name string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		case r == '_':
			b.WriteRune('_')
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	s := strings.Trim(b.String(), "-_")
	if s == "" {
		return "target"
	}
	if len(s) > 46 {
		s = strings.TrimRight(s[:46], "-_")
	}
	if s == "" {
		return "target"
	}
	if c := s[0]; !(c >= 'a' && c <= 'z') && !(c >= '0' && c <= '9') {
		s = "p" + s
	}
	return s
}

// assignTargetID 生成一个在本次快照里唯一的 target_id。
func assignTargetID(prefix, name string, used map[string]int) string {
	base := prefix + slugify(name)
	id := base
	for i := 2; ; i++ {
		if _, ok := used[id]; !ok {
			used[id] = 1
			return id
		}
		id = base + "-" + itoa(i)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func dedupeInts(in []int) []int {
	if len(in) == 0 {
		return []int{}
	}
	seen := make(map[int]struct{}, len(in))
	out := make([]int, 0, len(in))
	for _, v := range in {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	sort.Ints(out)
	return out
}

// SuggestedScheme 按端口给出协议建议：443/8443 视作 TLS，其余按明文 HTTP。
//
// 这只是**建议**：真正的 scheme 由人确认，面板不会替人决定一个跑了自签证书的服务。
func SuggestedScheme(port int) string {
	if port == 443 || port == 8443 {
		return "https"
	}
	return "http"
}

// SortCandidates 让候选列表稳定：先按来源，再按名称，最后按 target_id。
func SortCandidates(cs []Candidate) {
	sort.Slice(cs, func(i, j int) bool {
		if cs[i].Kind != cs[j].Kind {
			return cs[i].Kind < cs[j].Kind
		}
		if cs[i].Name != cs[j].Name {
			return cs[i].Name < cs[j].Name
		}
		return cs[i].TargetID < cs[j].TargetID
	})
}
