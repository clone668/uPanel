// Package apps 维护应用与分组的元数据（P09.1）。
//
// 这一层只回答三个问题：这个应用**是什么**（名称、说明、图标、分组）、**绑在哪个对象上**
// （link 无绑定 / systemd 单元 / 容器完整 ID / 面板创建的部署）、**从哪进去**
// （public_url）。它不回答“它现在跑没跑”“HTTP 通不通”——那些是运行时事实，
// 由 P09.2/P09.3 的探测产生，永远不写进元数据表（PLAN.md 第 11 节：运行状态和
// HTTP 状态分别显示；容器 running 不代表 HTTP 可访问）。
//
// 三条本包负责守住的判别规则：
//
//  1. type 与 binding 严格配对。link 没有绑定对象；systemd 只有 unit；docker 只有
//     完整 container_id；compose 只有 deployment_id。绑定对象的形状里不含主机、端口
//     或 URL，因此“换一台机器去执行”这种错误在数据模型上就表达不出来。
//  2. 同一个可绑定对象只能属于一个应用（库层部分唯一索引 + 写入时的明确错误）。
//  3. 公共入口（public_url，给浏览器）与内部目标（health_check.target_id，给探测与
//     反向代理）是**两个字段**，互不推导。面板不会把 public_url 的域名抓一遍，
//     也不会替用户生成一个 localhost 入口。
package apps

import (
	"errors"
	"fmt"
	"time"
)

// Type 是应用的运行类型（PLAN.md 第 11 节：固定为 link / systemd / docker / compose）。
type Type string

const (
	// TypeLink 只保存入口与说明，可打开，但没有启停。
	TypeLink Type = "link"
	// TypeSystemd 关联一个确切的服务单元。
	TypeSystemd Type = "systemd"
	// TypeDocker 关联一个已存在容器的完整 ID。
	TypeDocker Type = "docker"
	// TypeCompose 关联一个由面板创建并持有标记的部署。
	TypeCompose Type = "compose"
)

// Valid 判断类型是否在固定集合内。
func (t Type) Valid() bool {
	switch t {
	case TypeLink, TypeSystemd, TypeDocker, TypeCompose:
		return true
	}
	return false
}

// 元数据字段上限。数值取自 CONTRACTS.md 第 8 节与数据库 CHECK 约束，两处必须一致：
// 库层约束是最后一道，不是唯一一道。
const (
	MaxNameLen        = 80
	MaxDescriptionLen = 1000
	MaxGroupNameLen   = 40
	MaxPublicURLLen   = 2048
	MaxHealthPathLen  = 256
	MaxVolumeDeps     = 32
	MaxStatusCodes    = 8
)

// 健康检查的时间窗（CONTRACTS.md 第 8 节：interval 30—300、timeout 1—10）。
const (
	MinIntervalSeconds = 30
	MaxIntervalSeconds = 300
	MinTimeoutSeconds  = 1
	MaxTimeoutSeconds  = 10

	// DefaultIntervalSeconds / DefaultTimeoutSeconds 是 PLAN.md 第 11 节的默认值：
	// 每 30 秒一次、单次 3 秒超时。
	DefaultIntervalSeconds = 30
	DefaultTimeoutSeconds  = 3
)

// 列表分页（CONTRACTS.md 第 1 节：默认 50、最大 200）。
const (
	DefaultListLimit = 50
	MaxListLimit     = 200
)

// ID 前缀。与 job_ / pf_ / shr_ 同一套形状：前缀 + 32 位小写十六进制。
const (
	appIDPrefix   = "app_"
	groupIDPrefix = "grp_"
	// deploymentIDPrefix 是 compose 绑定引用的部署标识前缀（部署本身属 P09.3）。
	deploymentIDPrefix = "dep_"
	idHexLen           = 32
)

// 内置图标预设（PLAN.md 第 11 节：初期只选内置图标，避免远程 SVG/图片）。
//
// 库层没有 CHECK 约束：图标集合会随界面增长，一次增量就要改表不值得。
// 写入时在这里校验，空串表示“用默认图标”。
var BuiltinIconKeys = []string{
	"app", "globe", "database", "docker", "terminal",
	"files", "media", "monitor", "network", "shield", "tools",
}

// 包级错误。业务错误与系统错误必须分开：前者是 4xx，后者是 500 且只进日志。
var (
	// ErrNotFound 表示应用不存在（或已被删除）。
	ErrNotFound = errors.New("apps: 应用不存在")
	// ErrGroupNotFound 表示分组不存在。
	ErrGroupNotFound = errors.New("apps: 分组不存在")
	// ErrVersionConflict 表示写入的版本号与库中不一致。
	ErrVersionConflict = errors.New("apps: 版本不一致，已被其他人修改")
	// ErrBindingTaken 表示这个单元/容器/部署已经被别的应用登记。
	ErrBindingTaken = errors.New("apps: 该运行绑定已被其他应用占用")
	// ErrGroupNameTaken 表示分组名已存在。
	ErrGroupNameTaken = errors.New("apps: 分组名已存在")
	// ErrGroupInUse 表示分组里还有应用，不能删除。
	ErrGroupInUse = errors.New("apps: 分组里还有应用")
	// ErrInvalidLimit 表示 limit 超出范围。
	ErrInvalidLimit = errors.New("apps: 列表条数必须在 1 到 200 之间")
	// ErrInvalidCursor 表示游标不是本服务签发的。
	ErrInvalidCursor = errors.New("apps: 翻页游标无效")
)

// ValidationError 表示调用方传入的字段不合法，接口层映射为 400 VALIDATION_ERROR。
type ValidationError struct {
	// Field 是出错的字段名（与 JSON 字段同名），前端可直接高亮。
	Field string
	// Message 是中文可读说明，不含内部细节。
	Message string
}

func (e *ValidationError) Error() string { return e.Message }

func invalid(field, format string, args ...any) error {
	return &ValidationError{Field: field, Message: fmt.Sprintf(format, args...)}
}

// Binding 是应用的运行绑定。三个字段最多只有一个非空，由 Type 决定是哪一个。
//
// 它对应库里的 binding_json，也是接口里 binding 对象的解码目标：
// 用同一个结构体接三种绑定，好处是“给 systemd 传 container_id”能给出明确的中文错误，
// 而不是一句“未知字段”。
type Binding struct {
	// Unit 是 systemd 单元名，必须带 .service 后缀。
	Unit string
	// ContainerID 是容器的**完整** ID（64 位小写十六进制）。名称会随重建变化，不能当依据。
	ContainerID string
	// DeploymentID 是面板创建的部署标识（dep_ + 32 位十六进制）。
	DeploymentID string
}

// HealthCheck 是健康检查定义（CONTRACTS.md 第 8 节）。
//
// 目标用 target_id 表示，而不是主机名或 URL：target_id 来自代理发现的“本机/容器目标”，
// 形状上只允许 local_ / cntr_ 前缀，因此云元数据地址、任意外网主机、未经检查的 DNS
// 重绑定在数据模型里都写不出来（PLAN.md 第 11 节：健康检查与反向代理目标只允许本机
// 回环地址或明确登记的目标）。
type HealthCheck struct {
	// Enabled 表示是否真的去探测。false 时不产生任何请求。
	Enabled bool
	// TargetID 是目标标识，形如 local_3f2a… 或 cntr_9b1c…。
	TargetID string
	// Scheme 只允许 http / https。
	Scheme string
	// Port 是目标端口，1—65535。
	Port int
	// Path 必须以 / 开头，只能是路径，不能是完整 URL。
	Path string
	// ExpectedStatusCodes 是明确的状态码集合；空表示默认 200—399。
	ExpectedStatusCodes []int
	// IntervalSeconds 是探测间隔，30—300。
	IntervalSeconds int
	// TimeoutSeconds 是单次超时，1—10，且必须小于间隔。
	TimeoutSeconds int
}

// Definition 是应用的元数据本体（不含 id、version、时间与收藏）。
//
// 创建与修改都以它为单位校验：`Definition` 过一次 Normalize 之后才是可入库的形态，
// 默认值、去重、规范化都在那一步完成，存储层不再二次解释。
type Definition struct {
	Name               string
	Description        string
	IconKey            string
	GroupID            string
	Type               Type
	Binding            Binding
	PublicURL          string
	Health             HealthCheck
	VolumeDependencies []string
}

// App 是一条应用元数据记录。
type App struct {
	ID string
	Definition
	Version   int
	CreatedAt time.Time
	UpdatedAt time.Time
	// Favorite 是**当前请求者**是否收藏了这个应用。它不是应用本身的属性，
	// 读取时按用户 join 出来；没有请求者上下文时一律 false。
	Favorite bool
	// HealthResult 是最近一次健康探测的结论（P09.2）。为 nil 表示**从没探过**，
	// 与“探过、结论是 unknown”不是同一件事：前者是“还没看”，后者是“看了但没结论”。
	// 它不是元数据，删应用时随库级外键一起删掉。
	HealthResult *HealthResult
}

// Group 是应用分组。
type Group struct {
	ID        string
	Name      string
	SortOrder int
	Version   int
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Capabilities 描述“此刻真的能做到的事”（CONTRACTS.md 第 8 节 AppSummary）。
//
// 语义是能力，不是类型特性：P09.1 只落地了元数据，所以 systemd/docker/compose 的
// 启停与日志此刻都是 false——按钮要等 P09.3/P09.4 把动作接上之后才允许亮起来。
// 用“类型允许什么”填这一组字段会直接产出点不动的按钮，那是假功能。
type Capabilities struct {
	// Open 表示可以一键打开：有 public_url，且协议是 http/https（写入时已校验）。
	Open bool
	// Start / Stop / Restart 是生命周期控制。
	Start   bool
	Stop    bool
	Restart bool
	// Logs 表示能看关联对象的日志。
	Logs bool
}

// CapabilitiesFor 返回当前已实现的运行时能力。
func CapabilitiesFor(def Definition) Capabilities {
	return Capabilities{Open: def.PublicURL != ""}
}

// 运行时状态枚举（CONTRACTS.md 第 8 节 AppSummary）。
const (
	// RuntimeRunning 是观测到的“在跑”。
	RuntimeRunning = "running"
	// RuntimeStopped 是观测到的“没在跑”。
	RuntimeStopped = "stopped"
	// RuntimeFailed 是观测到的“启动失败/异常退出”。
	RuntimeFailed = "failed"
	// RuntimeUnknown 表示还没有可用的观测结果——不知道就说不知道。
	RuntimeUnknown = "unknown"
	// RuntimeNotApplicable 表示这个类型没有运行时（link）。
	RuntimeNotApplicable = "not_applicable"
)

// HTTP 状态枚举。
const (
	// HTTPOK 表示最近一次探测命中期望状态码。
	HTTPOK = "ok"
	// HTTPError 表示最近一次探测失败或状态码不在期望集合内。
	HTTPError = "error"
	// HTTPUnknown 表示配置了健康检查但还没有探测结果。
	HTTPUnknown = "unknown"
	// HTTPNotApplicable 表示没有配置健康检查。
	HTTPNotApplicable = "not_applicable"
)

// 依赖保护状态枚举（PLAN.md 第 11 节关键依赖规则 5）。
const (
	// ProtectionManaged 表示依赖保护由面板接管（P09.4 起才可能出现）。
	ProtectionManaged = "managed"
	// ProtectionUnmanaged 表示面板不能保证这个应用的启动依赖——必须如实标出来。
	ProtectionUnmanaged = "unmanaged"
	// ProtectionNotApplicable 表示没有依赖卷，或不涉及生命周期（link）。
	ProtectionNotApplicable = "not_applicable"
)

// RuntimeStateOf 返回此刻能给出的运行时状态。
//
// P09.1 没有任何探测，所以只有两种诚实答案：link 没有运行时；其余类型是“不知道”。
// 这里不返回 stopped——“不知道”和“停了”是两件事，混用会让页面把人骗去点启动。
func RuntimeStateOf(t Type) string {
	if t == TypeLink {
		return RuntimeNotApplicable
	}
	return RuntimeUnknown
}

// HTTPStateOf 返回此刻能给出的 HTTP 状态。
func HTTPStateOf(h HealthCheck) string {
	if !h.Enabled {
		return HTTPNotApplicable
	}
	return HTTPUnknown
}

// ObservedHTTPState 返回应用此刻应当显示的 HTTP 状态（P09.2）。
//
// 三种“不知道”要分开：
//   - 没启用健康检查 → not_applicable（这件事不适用，不是缺数据）；
//   - 启用了但还没探过 → unknown + last_checked_at 为 null（“还没看”）；
//   - 探过了 → 上一次的结论（ok / error / unknown）。
//
// 结论可能是 error 而不是“现在一定坏了”——它说的是“上一次探测时不通”。
// 这个字段从不假装比它知道的更多。
func ObservedHTTPState(h HealthCheck, r *HealthResult) string {
	if !h.Enabled {
		return HTTPNotApplicable
	}
	if r == nil {
		return HTTPUnknown
	}
	return r.HTTPState
}

// LastCheckedAt 返回最近一次探测时间；没探过时为 nil。
func LastCheckedAt(h HealthCheck, r *HealthResult) *time.Time {
	if !h.Enabled || r == nil || r.CheckedAt.IsZero() {
		return nil
	}
	checked := r.CheckedAt
	return &checked
}

// ProtectionOf 返回依赖保护状态。
//
// P09.1 还没有任何接管动作（RequiresMountsFor / BindsTo / 包装单元都在 P09.4），
// 因此有依赖卷的非 link 应用一律是 unmanaged：不是“已保护”的另一种说法。
func ProtectionOf(def Definition) string {
	if def.Type == TypeLink || len(def.VolumeDependencies) == 0 {
		return ProtectionNotApplicable
	}
	return ProtectionUnmanaged
}
