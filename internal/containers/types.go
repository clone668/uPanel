// Package containers 是 Docker 容器、镜像、网络、数据卷与 compose 项目的读取端，
// 以及“现有普通容器”的启停动作（P09.3，PLAN.md 第 12 节）。
//
// 三条边界：
//
//  1. **只通过固定命令读**：docker CLI 的绝对路径 + 常量参数列表，没有任何一处把
//     调用方给的字符串拼进命令。容器 ID 先过 64 位十六进制校验才进 argv，
//     名字、镜像、标签只出现在输出里，从不回到命令行。
//  2. **危险容器只显示、不接管**：privileged、宿主 PID/network/IPC、危险 capability、
//     挂载宿主根目录或 docker.sock 的容器，PLAN.md 第 12 节要求“显示风险信息”，
//     1.0 的面板写操作不接管这类容器。判决是一份**固定规则**，在预检、执行前复核与
//     执行器里各做一次——不是靠界面上把按钮画灰。
//  3. **不下发容器环境变量**：`docker inspect` 的 `Config.Env` 里有数据库口令之类的
//     东西，所以这里只挑固定字段（状态、资源、宿主配置里的命名空间与挂载），
//     绝不把整份 inspect 原样转出去。
package containers

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// 查询名。与 internal/agent 的登记表必须逐字一致。
const (
	QueryList     = "containers.list"
	QueryGet      = "containers.get"
	QueryLogs     = "containers.logs"
	QueryImages   = "containers.images"
	QueryNetworks = "containers.networks"
	QueryVolumes  = "containers.volumes"
	QueryProjects = "containers.projects"
)

// ActionControl 是启停动作的登记名（CONTRACTS.md 第 5 节 container.control）。
const ActionControl = "container.control"

// StreamPurposeLogs 是容器日志实时流的用途名。
//
// 它与日志模块的 `logs.stream` 是**两条**用途：一条读 journald、一条读 `docker logs`，
// 参数不同、限额也要各自成立。复用同一个用途名会让许可里的参数类型变成二选一。
const StreamPurposeLogs = "containers.logs.stream"

// 容器观测状态。与发现模块（internal/discovery）同一套拼写，前端因此只有一种枚举。
const (
	StateRunning = "running"
	StateStopped = "stopped"
	StateUnknown = "unknown"
)

// 风险级别闭集。
const (
	RiskNone = "none"
	RiskLow  = "low"
	RiskHigh = "high"
)

// 危险容器的原因码闭集。码是给程序判断用的，message 是给人看的。
const (
	RiskPrivileged    = "privileged"
	RiskHostNetwork   = "host_network"
	RiskHostPID       = "host_pid"
	RiskHostIPC       = "host_ipc"
	RiskDockerSock    = "docker_sock_mount"
	RiskHostRootMount = "host_root_mount"
	RiskDangerousCap  = "dangerous_capability"
)

// containerIDPattern 是完整容器 ID 的形状：64 位小写十六进制。
//
// 只接受完整 ID，不接受 12 位短 ID 与名字：短 ID 在容器多的机器上可能产生歧义，
// 而名字会随重建变化（P09.1 的 docker 绑定已经按这个理由只认 ID）。
var containerIDPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidContainerID 判断一个字符串是不是完整容器 ID。
func ValidContainerID(id string) bool { return containerIDPattern.MatchString(id) }

// 操作闭集（CONTRACTS.md 第 5 节 container.control 的 operation）。
const (
	OpStart   = "start"
	OpStop    = "stop"
	OpRestart = "restart"
)

// ValidOperation 判断操作是否在闭集内。
func ValidOperation(op string) bool {
	switch op {
	case OpStart, OpStop, OpRestart:
		return true
	}
	return false
}

// Port 是一条端口映射（宿主 → 容器）。
type Port struct {
	HostIP        string `json:"host_ip"`
	HostPort      int    `json:"host_port"`
	ContainerPort int    `json:"container_port"`
	Protocol      string `json:"protocol"`
}

// Mount 是容器看到的一条挂载（只保留类型、来源与去向；卷型挂载带卷名）。
type Mount struct {
	Type        string `json:"type"`
	Name        string `json:"name,omitempty"`
	Source      string `json:"source"`
	Destination string `json:"destination"`
}

// ComposeRef 是容器身上的 compose 标签。
//
// 只取六个键：其余标签是第三方自己写的，可能包含口令或令牌，一律不转发出去。
type ComposeRef struct {
	Project     string `json:"project"`
	Service     string `json:"service"`
	WorkingDir  string `json:"working_dir,omitempty"`
	ConfigFiles string `json:"config_files,omitempty"`
	OneOff      bool   `json:"one_off"`
	Number      int    `json:"number"`
}

// Resources 是容器的资源限制（0 表示没有设置限制，不是“限制为 0”）。
type Resources struct {
	CPULimitMilli int64 `json:"cpu_limit_milli"`
	MemoryLimit   int64 `json:"memory_limit_bytes"`
}

// RiskReason 是一条风险/不接管原因。
type RiskReason struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Risk 是一个容器的风险判决。
type Risk struct {
	Level   string       `json:"level"`
	Reasons []RiskReason `json:"reasons"`
}

// Capabilities 是这个容器当前允许的写操作。
//
// Managed=false 时三项全部为 false，原因在 Risk.Reasons 里：界面据此显示“不接管”，
// 而不是显示三个永远点不动的按钮。
type Capabilities struct {
	Managed bool `json:"managed"`
	Start   bool `json:"start"`
	Stop    bool `json:"stop"`
	Restart bool `json:"restart"`
}

// Container 是一个容器的读取结果。
type Container struct {
	ID            string       `json:"id"`
	Name          string       `json:"name"`
	Image         string       `json:"image"`
	ImageID       string       `json:"image_id,omitempty"`
	State         string       `json:"state"`
	StateText     string       `json:"state_text"`
	CreatedAt     string       `json:"created_at,omitempty"`
	StartedAt     string       `json:"started_at,omitempty"`
	RestartCount  int          `json:"restart_count"`
	RestartPolicy string       `json:"restart_policy"`
	Ports         []Port       `json:"ports"`
	Mounts        []Mount      `json:"mounts"`
	Compose       *ComposeRef  `json:"compose"`
	Resources     Resources    `json:"resources"`
	Risk          Risk         `json:"risk"`
	Capabilities  Capabilities `json:"capabilities"`
}

// ListResponse 是 containers.list 的结果。
type ListResponse struct {
	Available   bool        `json:"available"`
	Notice      string      `json:"notice,omitempty"`
	Containers  []Container `json:"containers"`
	Truncated   bool        `json:"truncated"`
	CollectedAt string      `json:"collected_at"`
}

// Image 是一个镜像。
type Image struct {
	ID         string `json:"id"`
	Repository string `json:"repository"`
	Tag        string `json:"tag"`
	Digest     string `json:"digest,omitempty"`
	Size       string `json:"size"`
	CreatedAt  string `json:"created_at,omitempty"`
}

// ImagesResponse 是 containers.images 的结果。
type ImagesResponse struct {
	Available bool    `json:"available"`
	Notice    string  `json:"notice,omitempty"`
	Images    []Image `json:"images"`
	Truncated bool    `json:"truncated"`
}

// Network 是一个 Docker 网络。
type Network struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Driver   string   `json:"driver"`
	Scope    string   `json:"scope"`
	Internal bool     `json:"internal"`
	Subnets  []string `json:"subnets"`
}

// NetworksResponse 是 containers.networks 的结果。
type NetworksResponse struct {
	Available bool      `json:"available"`
	Notice    string    `json:"notice,omitempty"`
	Networks  []Network `json:"networks"`
	Truncated bool      `json:"truncated"`
}

// Volume 是一个数据卷。
type Volume struct {
	Name       string   `json:"name"`
	Driver     string   `json:"driver"`
	Scope      string   `json:"scope"`
	Mountpoint string   `json:"mountpoint,omitempty"`
	CreatedAt  string   `json:"created_at,omitempty"`
	InUseBy    []string `json:"in_use_by"`
}

// VolumesResponse 是 containers.volumes 的结果。
type VolumesResponse struct {
	Available bool     `json:"available"`
	Notice    string   `json:"notice,omitempty"`
	Volumes   []Volume `json:"volumes"`
	Truncated bool     `json:"truncated"`
}

// Project 是一个 compose 项目（由容器标签归并而来）。
//
// 0.1 不创建、不更新 compose 项目，也不执行任何外部 YAML：这里只有“这台机器上
// 现在按项目标签看有哪些容器”。
type Project struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	WorkingDir   string   `json:"working_dir,omitempty"`
	ConfigFiles  string   `json:"config_files,omitempty"`
	Services     []string `json:"services"`
	ContainerIDs []string `json:"container_ids"`
	Running      int      `json:"running"`
	Total        int      `json:"total"`
	// Capabilities 固定为 false：0.1 不具备 Compose 整体更新能力（PLAN.md 第 12 节）。
	Capabilities ProjectCapabilities `json:"capabilities"`
}

// ProjectCapabilities 是项目级操作能力。0.1 全部为 false，且这不是占位——
// 面板确实不会去执行外部 compose 文件。
type ProjectCapabilities struct {
	Apply  bool `json:"apply"`
	Update bool `json:"update"`
	Remove bool `json:"remove"`
}

// ProjectsResponse 是 containers.projects 的结果。
type ProjectsResponse struct {
	Available bool      `json:"available"`
	Notice    string    `json:"notice,omitempty"`
	Projects  []Project `json:"projects"`
	Truncated bool      `json:"truncated"`
	// ComposePlugin 报告本机是否装了 docker compose 插件。0.1 不用它做任何管理，
	// 只用来解释“为什么项目清单是从标签归并出来的”。
	ComposePlugin bool `json:"compose_plugin"`
}

// 日志读取的上限（CONTRACTS.md 第 10.3 节的限额在容器日志上同样成立）。
const (
	// MaxLogLines 是单页最多返回的行数。
	MaxLogLines = 1000
	// DefaultLogLines 是缺省行数。
	DefaultLogLines = 100
	// MaxLogLineBytes 是单行保留的字节数，超出截断。
	MaxLogLineBytes = 8 << 10
	// MaxLogPageBytes 是单页消息累计上限。
	MaxLogPageBytes = 1 << 20
)

// LogParams 是 containers.logs 的参数。
//
// 没有 level：docker 没有日志级别字段，做一次“猜测级别”的正则只会让
// “这页只有 3 条 error”变成一句假话。
type LogParams struct {
	ContainerID string `json:"container_id"`
	// Since/Until 是 RFC3339 时间窗口（含端点）。缺省时回看最近 DefaultLogLines 行。
	Since string `json:"since,omitempty"`
	Until string `json:"until,omitempty"`
	// Tail 是本页最多返回的行数（1—1000）。
	Tail int `json:"tail,omitempty"`
	// Cursor 是上一页的 next_cursor（不透明）：语义是“看比这条更早的”。
	Cursor string `json:"cursor,omitempty"`
}

// Validate 校验参数形状，并补齐缺省值。
func (p *LogParams) Validate(now time.Time) error {
	p.ContainerID = strings.TrimSpace(p.ContainerID)
	if !ValidContainerID(p.ContainerID) {
		return fmt.Errorf("container_id 必须是完整的 64 位容器 ID")
	}
	if p.Tail == 0 {
		p.Tail = DefaultLogLines
	}
	if p.Tail < 1 || p.Tail > MaxLogLines {
		return fmt.Errorf("tail 必须在 1—%d 之间，收到 %d", MaxLogLines, p.Tail)
	}
	if p.Since != "" {
		ts, err := parseRFC3339(p.Since)
		if err != nil {
			return fmt.Errorf("since 不是合法的 RFC 3339 时间")
		}
		p.Since = ts.UTC().Format(time.RFC3339Nano)
	}
	if p.Until != "" {
		ts, err := parseRFC3339(p.Until)
		if err != nil {
			return fmt.Errorf("until 不是合法的 RFC 3339 时间")
		}
		if ts.After(now.Add(2 * time.Minute)) {
			// 未来窗口永远为空。与其让人以为“这段时间没有日志”，不如直说参数写错了。
			return fmt.Errorf("until 不能晚于当前时间 2 分钟以上")
		}
		p.Until = ts.UTC().Format(time.RFC3339Nano)
	}
	if p.Since != "" && p.Until != "" && p.Since > p.Until {
		return fmt.Errorf("since 必须早于 until")
	}
	if p.Cursor != "" {
		ts, err := parseRFC3339(p.Cursor)
		if err != nil {
			// 游标是上一页的 next_cursor，只能原样带回。解析不了就是调用方拼的，
			// 重试同一个值不会变好，因此是 400 而不是“当成没给”。
			return fmt.Errorf("cursor 不是本服务发出的游标")
		}
		p.Cursor = ts.UTC().Format(time.RFC3339Nano)
	}
	return nil
}

// parseRFC3339 解析时间，并拒绝“只有日期”的写法。
func parseRFC3339(s string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, strings.TrimSpace(s))
}

// LogEntry 是一条容器日志。
type LogEntry struct {
	Timestamp time.Time `json:"timestamp"`
	Message   string    `json:"message"`
	Truncated bool      `json:"truncated"`
	Redacted  bool      `json:"redacted"`
}

// LogPage 是 containers.logs 的结果。
//
// Entries 按时间**从新到旧**，与 journal 日志同一约定；next_cursor 是本页最老一条的
// 时间戳，带回来即“看比它更早的”。
type LogPage struct {
	ContainerID string     `json:"container_id"`
	From        string     `json:"from,omitempty"`
	To          string     `json:"to,omitempty"`
	Entries     []LogEntry `json:"entries"`
	NextCursor  *string    `json:"next_cursor"`
	Truncated   bool       `json:"truncated"`
	// TruncatedReason 为空表示撞到调用方给的 tail；max_tail 表示 docker 只给了这么多行；
	// page_bytes 表示单页字节上限先到。
	TruncatedReason string `json:"truncated_reason,omitempty"`
	Lines           int    `json:"lines"`
	DurationMS      int64  `json:"duration_ms"`
	// StreamsMerged 恒为 true：docker logs 把 stdout 与 stderr 合在一起给，
	// 不标来源。面板如实说明这一点，而不是给每条编一个来源。
	StreamsMerged bool `json:"streams_merged"`
}

// StreamEnd 是流结束的原因（与日志模块的闭集同名同义）。
const (
	StreamEndDurationLimit = "duration_limit"
	StreamEndIdleTimeout   = "idle_timeout"
	StreamEndEntryLimit    = "entry_limit"
	StreamEndByteLimit     = "byte_limit"
	StreamEndContainerExit = "container_exit"
	StreamEndClientClosed  = "client_closed"
)

// ControlParams 是 container.control 的参数。
type ControlParams struct {
	ContainerID string `json:"container_id"`
	Operation   string `json:"operation"`
}

// Normalize 规范化参数（trim + 小写），并校验闭集。
func (p *ControlParams) Normalize() error {
	p.ContainerID = strings.TrimSpace(p.ContainerID)
	p.Operation = strings.ToLower(strings.TrimSpace(p.Operation))
	if !ValidContainerID(p.ContainerID) {
		return fmt.Errorf("container_id 必须是完整的 64 位容器 ID")
	}
	if !ValidOperation(p.Operation) {
		return fmt.Errorf("operation 只能是 start/stop/restart，收到 %q", p.Operation)
	}
	return nil
}

// 危险 capability 的固定名单。
//
// 这几个都能把容器变成“宿主机上的 root”：SYS_ADMIN 可以挂载与 ptrace、
// SYS_MODULE 能装内核模块、SYS_PTRACE 能读别的进程内存、SYS_RAWIO 能直接读设备、
// NET_ADMIN 能改宿主网络（host 网络下）、DAC_READ_SEARCH 能绕过文件权限。
var dangerousCaps = map[string]string{
	"CAP_SYS_ADMIN":       "可以直接挂载文件系统、操作内核接口，等同于宿主 root",
	"CAP_SYS_MODULE":      "可以加载内核模块，等同于接管内核",
	"CAP_SYS_PTRACE":      "可以读写其他进程的内存",
	"CAP_SYS_RAWIO":       "可以直接访问块设备与裸 IO",
	"CAP_NET_ADMIN":       "可以修改网络接口、路由与防火墙",
	"CAP_DAC_READ_SEARCH": "可以绕过文件读取权限检查",
}

// parseCapName 把 docker 给出的 capability 名规范化（可能是 SYS_ADMIN 或 CAP_SYS_ADMIN）。
func parseCapName(name string) string {
	name = strings.ToUpper(strings.TrimSpace(name))
	if name == "" {
		return ""
	}
	if !strings.HasPrefix(name, "CAP_") {
		name = "CAP_" + name
	}
	return name
}

// AssessRisk 按 PLAN.md 第 12 节的固定规则给出风险判决。
//
// 规则是**闭集**：只有这里列出的情形算危险容器，不认识的字段不猜。原因是这条判断
// 决定“面板会不会去动它”，宁可少接管一个奇怪的容器，也不能把一个 privileged 容器
// 当成普通容器重启。
func AssessRisk(in InspectFacts) Risk {
	reasons := make([]RiskReason, 0, 4)
	add := func(code, msg string) {
		reasons = append(reasons, RiskReason{Code: code, Message: msg})
	}
	if in.Privileged {
		add(RiskPrivileged, "容器以 privileged 运行，容器内等同于宿主 root")
	}
	switch strings.ToLower(in.NetworkMode) {
	case "host":
		add(RiskHostNetwork, "容器使用宿主网络命名空间，重启会影响宿主网络")
	}
	switch strings.ToLower(in.PidMode) {
	case "host":
		add(RiskHostPID, "容器使用宿主 PID 命名空间，可以看到并影响宿主进程")
	}
	switch strings.ToLower(in.IpcMode) {
	case "host":
		add(RiskHostIPC, "容器使用宿主 IPC 命名空间")
	}
	for _, cap := range in.CapAdd {
		name := parseCapName(cap)
		if desc, ok := dangerousCaps[name]; ok {
			add(RiskDangerousCap, fmt.Sprintf("容器被授予 %s：%s", name, desc))
		}
	}
	for _, m := range in.Mounts {
		if !strings.EqualFold(m.Type, "bind") {
			continue
		}
		src := strings.TrimRight(m.Source, "/")
		if strings.HasSuffix(m.Source, "docker.sock") || strings.HasSuffix(m.Destination, "docker.sock") {
			add(RiskDockerSock, fmt.Sprintf("容器挂载了 Docker 套接字（%s → %s），等同于能让容器控制宿主 Docker",
				m.Source, m.Destination))
			continue
		}
		// "/" 去掉尾部斜杠之后是空串：宿主根目录被 bind 进容器，容器可以改宿主的任何文件。
		if src == "" {
			add(RiskHostRootMount, fmt.Sprintf("容器把宿主根目录挂到 %s，可以修改宿主上的任何文件", m.Destination))
		}
	}
	level := RiskNone
	if len(reasons) > 0 {
		level = RiskHigh
	}
	return Risk{Level: level, Reasons: reasons}
}

// InspectFacts 是风险判决需要的事实（来自 docker inspect 的固定字段）。
type InspectFacts struct {
	Privileged  bool
	NetworkMode string
	PidMode     string
	IpcMode     string
	CapAdd      []string
	Mounts      []Mount
}

// Capabilities 按状态与风险给出允许的写操作。
//
// 不接管（Managed=false）时三项一律 false：界面据此画“不接管”，而不是把三个按钮
// 画成灰的再让人猜为什么。状态与操作的对应关系来自 docker 自己的语义——
// start 只能用在 created/exited 上，对 paused 容器 start 会报错；stop 只对 running 有意义。
func (c Container) ControlCapabilities() Capabilities {
	caps := Capabilities{}
	if c.Risk.Level == RiskHigh {
		return caps
	}
	caps.Managed = true
	switch c.State {
	case StateRunning:
		caps.Stop = true
		caps.Restart = true
	case StateStopped:
		caps.Start = true
		caps.Restart = true
	}
	return caps
}

// OperationAllowed 判断在给定状态下能不能做这个操作。
func OperationAllowed(state, op string) bool {
	switch op {
	case OpStart:
		return state == StateStopped
	case OpStop:
		return state == StateRunning
	case OpRestart:
		return state == StateRunning || state == StateStopped
	}
	return false
}

// MapState 把 docker 的 State 映射成面板的观测枚举。
//
// paused/restarting 归为 unknown：restarting 是“在重启循环里”，把它写成 running
// 会让一个坏掉的容器显示成正常；paused 既不是运行也不是停止，且 start 对它不成立。
func MapState(dockerState string) string {
	switch strings.ToLower(strings.TrimSpace(dockerState)) {
	case "running":
		return StateRunning
	case "exited", "created", "dead", "removing":
		return StateStopped
	}
	return StateUnknown
}

// ObservedStateText 把面板的观测枚举翻回 docker 的原始词，用于结果里的 observed_state。
func ObservedStateText(state string) string {
	switch state {
	case StateRunning:
		return "running"
	case StateStopped:
		return "stopped"
	}
	return "unknown"
}

// ParseInt 解析 docker 输出里的整数；空串按 0 处理（inspect 对未设置的限额给 0）。
func ParseInt(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return v
}
