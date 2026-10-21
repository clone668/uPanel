// Package services 采集 systemd 服务单元（P05.1）：单元清单、运行状态、开机启用状态、
// 保护判定，以及受约束的启停/重启/开机启动动作。
//
// 边界（PLAN.md 第 14 节、CONTRACTS.md 第 5、10 节）：
//
//   - 只使用**固定 argv** 调用 systemctl，参数由本包构造，单元名来自本包的枚举与校验；
//     不接受任意 systemctl 参数，也不经过 shell（internal/syscmd）。
//   - 每个单元只按自己的名字操作；不存在“按前缀/模式批量操作”。
//   - 面板、代理、SSH、systemd 核心与网络基础服务受保护；Nginx/Samba/Docker 等属于
//     各自模块，通用服务接口必须拒绝它们，而不是在这里顺手实现一份。
//   - 采集失败不清空列表：快照保留上一次成功内容并标明失败原因与采集时间。
package services

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 查询名（CONTRACTS.md 第 15 节：查询名按模块登记）。
const (
	// QueryListName 返回最近一次采集的完整快照。
	QueryListName = "services.list"
	// QueryUnitName 现场读取单个单元（详情页与预检都用它，不读缓存）。
	QueryUnitName = "services.unit"
)

// ActionControl 是唯一的服务写动作（CONTRACTS.md 第 5 节）。
const ActionControl = "service.control"

// DefaultInterval 是服务快照的采集间隔。
//
// PLAN.md 第 8 节规定的是资源与磁盘计数器（5 秒）、容量（30 秒）、SMART（15 分钟），
// 没有规定服务；10 秒是“页面打开就能看到变化、又不至于每秒拉起一次 systemctl”的折中。
// 详情页与预检不走缓存，直接现场读取，所以操作的即时性不受这个间隔影响。
const DefaultInterval = 10 * time.Second

// 快照状态闭集（与 monitoring / storage 同一套语义，前端可复用组件）。
const (
	StatusOK    = "ok"
	StatusError = "error"
)

// LoadState 闭集（systemd 的 LoadState，去掉与面板无关的取值后保留原词）。
const (
	LoadLoaded     = "loaded"
	LoadNotFound   = "not-found"
	LoadBadSetting = "bad-setting"
	LoadError      = "error"
	LoadMerged     = "merged"
	LoadMasked     = "masked"
)

// ActiveState 闭集（systemd 的 ActiveState）。
const (
	ActiveActive       = "active"
	ActiveReloading    = "reloading"
	ActiveInactive     = "inactive"
	ActiveFailed       = "failed"
	ActiveActivating   = "activating"
	ActiveDeactivating = "deactivating"
	ActiveMaintenance  = "maintenance"
)

// Enablement 是 UnitFileState 归一化后的开机启用状态（CONTRACTS.md 第 10 节）。
//
// systemctl 自己的取值有 enabled-runtime / linked-runtime 这类细分；界面只需要回答
// “开机会不会起来、能不能改”，所以归一化，同时把原始值放在 unit_file_state 里备查。
const (
	EnablementEnabled   = "enabled"
	EnablementDisabled  = "disabled"
	EnablementStatic    = "static"   // 没有 [Install] 段：不能 enable/disable
	EnablementIndirect  = "indirect" // 只能被别的单元拉起
	EnablementMasked    = "masked"   // 被 mask：start 也会失败
	EnablementGenerated = "generated"
	EnablementTransient = "transient"
	EnablementAlias     = "alias"
	EnablementLinked    = "linked"
	EnablementUnknown   = "unknown"
)

// 操作闭集（CONTRACTS.md 第 5 节 service.control 的 operation）。
const (
	OpStart   = "start"
	OpStop    = "stop"
	OpRestart = "restart"
	OpEnable  = "enable"
	OpDisable = "disable"
)

// Operations 按固定顺序返回全部操作，供校验与界面使用。
func Operations() []string { return []string{OpStart, OpStop, OpRestart, OpEnable, OpDisable} }

// ValidOperation 判断操作是否在闭集内。
func ValidOperation(op string) bool {
	switch op {
	case OpStart, OpStop, OpRestart, OpEnable, OpDisable:
		return true
	}
	return false
}

// 保护类别闭集。
const (
	// ProtectSelf：面板与代理自身。停掉之后就没有管理通道了。
	ProtectSelf = "self"
	// ProtectSSH：远程登录。停掉之后管理员可能直接失去机器。
	ProtectSSH = "ssh"
	// ProtectCore：systemd 核心组件（日志、会话、设备管理、总线）。
	ProtectCore = "systemd_core"
	// ProtectNetwork：网络基础服务。停掉之后面板自己也连不上。
	ProtectNetwork = "network"
	// ProtectOtherModule：由其他模块负责的守护进程（反代、共享、容器）。
	// 通用服务接口一律拒绝，避免绕过那些模块的依赖检查。
	ProtectOtherModule = "other_module"
)

// Unit 是一个 systemd 服务单元的状态（列表与详情共用同一个 DTO）。
type Unit struct {
	// Unit 是完整单元名（例如 cron.service）。所有接口都用它作为唯一标识。
	Unit string `json:"unit"`
	// Description 是单元文件里的描述，可能为空。
	Description string `json:"description"`
	// LoadState / ActiveState / SubState 保留 systemd 原词（闭集见上）。
	LoadState   string `json:"load_state"`
	ActiveState string `json:"active_state"`
	SubState    string `json:"sub_state"`
	// UnitFileState 是 systemctl 的原值（例如 enabled-runtime），便于核对。
	UnitFileState string `json:"unit_file_state"`
	// Enablement 是归一化后的开机启用状态。
	Enablement string `json:"enablement"`
	// Template 为 true 表示这是模板单元（foo@.service），必须带实例名才能操作。
	Template bool `json:"template"`
	// MainPID 是主进程 PID；没有主进程时为 null（不是 0）。
	MainPID *int64 `json:"main_pid"`
	// StartedAt 是 ActiveEnterTimestamp：本次进入 active 的时间；从未启动过为 null。
	StartedAt *time.Time `json:"started_at"`
	// RestartCount 来自 NRestarts；读不到为 null。
	RestartCount *int64 `json:"restart_count"`
	// MemoryBytes 是 cgroup 当前内存（十进制字符串）；读不到为 null。
	MemoryBytes *string `json:"memory_bytes"`
	// TasksCurrent 是 cgroup 当前任务数；读不到为 null。
	TasksCurrent *int64 `json:"tasks_current"`
	// FragmentPath 是单元文件路径（可能为空）。
	FragmentPath string `json:"fragment_path"`
	// Protection 说明为什么这个单元不允许通过面板操作。
	Protection Protection `json:"protection"`
	// Capabilities 是**当前状态与保护规则下**允许的操作（闭集子集，固定顺序）。
	// 受保护单元一律为空数组：界面据此不摆按钮，而不是摆一个点了会报错的按钮。
	Capabilities []string `json:"capabilities"`
	// CheckedAt 是读取这个单元的时间（UTC）。
	CheckedAt time.Time `json:"checked_at"`
}

// Protection 是服务单元的保护判定。
type Protection struct {
	Protected bool `json:"protected"`
	// Category 是保护类别（闭集）；未保护时为空。
	Category string `json:"category,omitempty"`
	// Reasons 是人类可读理由，去重且排序固定。
	Reasons []string `json:"reasons"`
}

// Snapshot 是一次采集的完整结果，也是 services.list 的返回值。
type Snapshot struct {
	// Status 是最近一次采集尝试的结果；失败时 Units 保留上一次成功的内容。
	Status      string    `json:"status"`
	Error       string    `json:"error,omitempty"`
	CollectedAt time.Time `json:"collected_at"`
	IntervalMS  int64     `json:"interval_ms"`
	// SnapshotVersion 只由单元状态内容决定（不含时间与 PID），
	// 预检用它判断“从预检到执行之间状态有没有变”。
	SnapshotVersion string `json:"snapshot_version"`
	Units           []Unit `json:"units"`
	// Truncated 在本机服务数量超过上限时为 true（列表被截断，不是“只有这些”）。
	Truncated bool `json:"truncated"`
}

// MaxUnits 是单次快照收录的单元上限。
//
// 超过上限时按名字排序后截断并置 Truncated=true：宁可明说“只显示前 N 个”，
// 也不要把一台 600 个单元的机器渲染成几千行。
const MaxUnits = 1000

// 单元名允许的字符集：字母数字开头，允许 : _ . @ -，必须以 .service 结尾。
//
// 这个正则同时挡住了 /（路径）、空格（多参数）、; & | $ `（shell 元字符）
// 和以 - 开头的名字（会被 systemctl 当成选项）。
var unitNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:_.@-]*\.service$`)

// ErrInvalidUnitName 表示单元名不在允许的字符集内。
var ErrInvalidUnitName = fmt.Errorf("services: 单元名不合法")

// ValidateUnitName 校验一个单元名。
func ValidateUnitName(name string) error {
	if len(name) == 0 || len(name) > 256 {
		return fmt.Errorf("%w: 长度必须在 1—256 字节之间", ErrInvalidUnitName)
	}
	if strings.Contains(name, "..") {
		return fmt.Errorf("%w: 不允许出现 ..", ErrInvalidUnitName)
	}
	if !unitNamePattern.MatchString(name) {
		return fmt.Errorf("%w: %q（只允许字母数字与 : _ . @ -，且必须以 .service 结尾）", ErrInvalidUnitName, name)
	}
	return nil
}

// baseName 去掉实例部分（foo@bar.service → foo），用于保护名单匹配。
func baseName(unit string) string {
	if i := strings.IndexByte(unit, '@'); i >= 0 {
		return unit[:i]
	}
	return strings.TrimSuffix(unit, ".service")
}

// IsTemplate 判断是否是模板单元（foo@.service）。
func IsTemplate(unit string) bool { return strings.Contains(unit, "@.") }

// normalizeEnablement 把 UnitFileState 归一化。
func normalizeEnablement(state string) string {
	switch state {
	case "enabled", "enabled-runtime":
		return EnablementEnabled
	case "disabled":
		return EnablementDisabled
	case "static":
		return EnablementStatic
	case "indirect":
		return EnablementIndirect
	case "masked", "masked-runtime":
		return EnablementMasked
	case "generated":
		return EnablementGenerated
	case "transient":
		return EnablementTransient
	case "alias":
		return EnablementAlias
	case "linked", "linked-runtime":
		return EnablementLinked
	}
	return EnablementUnknown
}

// capabilityFor 计算给定状态下允许的操作。
//
// 规则刻意保守：状态已经满足的操作不出现（active 不出现 start），
// 不能改的单元文件状态不出现 enable/disable（static/indirect 没有 [Install] 段）。
func capabilityFor(u Unit) []string {
	if u.Protection.Protected || u.LoadState == LoadNotFound || u.LoadState == LoadError ||
		u.LoadState == LoadBadSetting || u.Template {
		return []string{}
	}
	out := make([]string, 0, 5)
	switch u.ActiveState {
	case ActiveInactive:
		out = append(out, OpStart)
	case ActiveFailed:
		out = append(out, OpStart, OpStop)
	case ActiveActive, ActiveReloading:
		out = append(out, OpStop, OpRestart)
	}
	switch u.Enablement {
	case EnablementDisabled:
		out = append(out, OpEnable)
	case EnablementEnabled:
		out = append(out, OpDisable)
	}
	return out
}

// ComputeSnapshotVersion 计算快照版本：只由单元状态内容决定。
//
// 刻意排除 collected_at、PID、内存与重启次数——否则一次普通重启或内存波动就会让
// 预检失效，而那些变化并不影响“能不能 start/stop”。名字排序保证结果稳定。
func ComputeSnapshotVersion(units []Unit) string {
	names := make([]string, 0, len(units))
	byName := make(map[string]Unit, len(units))
	for _, u := range units {
		names = append(names, u.Unit)
		byName[u.Unit] = u
	}
	sort.Strings(names)
	h := sha256.New()
	fmt.Fprintf(h, "services/v1\n")
	for _, name := range names {
		u := byName[name]
		fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00%s\x00%t\n",
			u.Unit, u.LoadState, u.ActiveState, u.SubState, u.Enablement, u.Template)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// StateVersion 是单个单元的状态版本，预检与执行前复核用它判断状态是否已变。
func StateVersion(u Unit) string {
	h := sha256.New()
	fmt.Fprintf(h, "service-unit/v1\n%s\x00%s\x00%s\x00%s\x00%s\n",
		u.Unit, u.LoadState, u.ActiveState, u.SubState, u.Enablement)
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// parseNumPtr 解析 systemd 的数值属性；"[not set]"、"infinity" 与空串都当“没有值”。
func parseNumPtr(v string) *int64 {
	v = strings.TrimSpace(v)
	if v == "" || v == "[not set]" || v == "infinity" {
		return nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return nil
	}
	return &n
}

// parsePIDPtr 解析 MainPID：systemd 用 0 表示“没有主进程”。
//
// 0 在这里不是一个可展示的 PID，把它原样传出去会让界面写“PID 0”——那是一个
// 不存在的进程号。契约要求这种情况下给 null，所以 0 与非正数一律转成 null。
func parsePIDPtr(v string) *int64 {
	n := parseNumPtr(v)
	if n == nil || *n <= 0 {
		return nil
	}
	return n
}

// parseBytesPtr 解析可能超过 int64 的字节数（MemoryCurrent），保留十进制字符串。
func parseBytesPtr(v string) *string {
	v = strings.TrimSpace(v)
	if v == "" || v == "[not set]" || v == "infinity" {
		return nil
	}
	if _, err := strconv.ParseUint(v, 10, 64); err != nil {
		return nil
	}
	return &v
}

// timestampLayout 是 systemd 在 LC_ALL=C.UTF-8 下输出时间戳的格式。
const timestampLayout = "Mon 2006-01-02 15:04:05 MST"

// parseTimestamp 解析 ActiveEnterTimestamp；"n/a" 与空串表示没有。
func parseTimestamp(v string) *time.Time {
	v = strings.TrimSpace(v)
	if v == "" || v == "n/a" {
		return nil
	}
	t, err := time.ParseInLocation(timestampLayout, v, time.UTC)
	if err != nil {
		return nil
	}
	t = t.UTC()
	return &t
}
