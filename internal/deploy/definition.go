// Package deploy 承载受约束的应用部署定义（DeploymentDefinition）与 Compose 配置渲染。
//
// 三条纪律，来自 CONTRACTS.md §8.2 与 STAGES.md §17：
//
//  1. 定义是**闭集**。请求体里多一个字段就是 400，而不是被忽略。`privileged`、`devices`、
//     `network_mode`、`cap_add`、`compose_yaml` 这些东西之所以危险，正是因为它们能被写进
//     配置文件——闭集让它们在**数据模型里没有位置**，于是也写不进渲染结果。
//  2. 宿主路径写不出来。挂载只能由“文件根 id + 根内相对路径”表达，绝对宿主路径、`..`、
//     `/proc`、`/sys`、`/dev`、`docker.sock` 一律拒绝；渲染时还要求解析出来的宿主路径
//     必须落在**部署数据根**之内（HostEnv.DataBase）。`/`、`/etc`、`/var/run` 因此
//     连一个能表达的形状都没有。
//  3. 渲染是确定性的，而且可核对。同一份定义渲染出**同一份字节**（环境变量按名字排序、
//     挂载按容器路径排序、没有时间戳），配置摘要就是这堆字节的 sha256。P15.3 在执行后
//     再记录实际镜像 digest，两者合起来才是“现在跑的就是我批过的那份”。
//
// 这个包只做“定义 → 校验 → 字节”这一段。落盘（root 独占 0600）、包装 systemd 单元、
// 执行 compose、记录镜像 digest 属于 P15.2/P15.3；本包不产生任何外部副作用。
//
// 渲染发生在**代理侧**，不在 Web 侧：环境变量的秘密只存在代理的密钥环里，Web 永远看不到
// 明文（CONTRACTS.md §14.2）。因此 Resolver 由代理实现，Web 只保存“哪个变量引用哪个
// sec_ 标识”。
package deploy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// 上限与范围。这些数值是契约的一部分（CONTRACTS.md §8.2），库层的 CHECK 只能相等或更松，
// 不能更紧：否则会出现“校验通过但写不进库”的死角。
const (
	// MaxNameLen 是部署名上限。63 与 DNS 标签同长，也是 Compose 项目名的实际上限。
	MaxNameLen     = 63
	MaxImageRefLen = 255

	// MaxEnvVars / MaxEnvNameLen / MaxEnvValueBytes：环境变量条目数、名字长度、单个值的字节数。
	// 值走 compose 的 environment 映射，整份配置会落盘，所以值必须有上限。
	MaxEnvVars       = 64
	MaxEnvNameLen    = 128
	MaxEnvValueBytes = 32 << 10

	// MaxMounts / MaxRelativePathLen / MaxContainerPathLen：挂载条目数与两个路径的长度。
	MaxMounts             = 16
	MaxRelativePathLen    = 512
	MaxContainerPathLen   = 512
	MaxVolumeDependencies = 32

	// 资源限制：初期两者**必填**（CONTRACTS.md §8.2）。下限不是摆设——16 MiB 以下的
	// 容器连镜像都解不开，写 1 只会得到“跑了但永远起不来”；上限防止一个部署吃掉整台机器。
	MinMemoryLimitBytes = 16 << 20
	MaxMemoryLimitBytes = 64 << 30
	MinCPULimit         = 0.05
	MaxCPULimit         = 8.0

	// 端口范围。0 表示“随机端口”，而随机端口会让面板记下的入口地址隔一次重启就失效，
	// 所以这里不接受 0。
	MinPort = 1
	MaxPort = 65535

	// 健康检查的时间窗。它与 apps 的健康检查刻意不同：那边的目标是外部 URL，
	// 这边探的是一个刚起来的容器，允许更密的间隔。
	MinHealthIntervalSeconds = 5
	MaxHealthIntervalSeconds = 300
	MinHealthTimeoutSeconds  = 1
	MaxHealthTimeoutSeconds  = 60
	MaxHealthRetries         = 10
	MaxHealthStartSeconds    = 600

	// 默认值：与 PLAN.md 第 11 节的探测默认值同源（30 秒一次、3 秒超时、3 次重试）。
	DefaultHealthIntervalSeconds = 30
	DefaultHealthTimeoutSeconds  = 3
	DefaultHealthRetries         = 3
)

// 稳定机器码。界面按 field 高亮、按 code 归类；测试断言 code 而不是中文措辞
// （中文会为了读起来通顺而改，code 不该跟着动）。
const (
	CodeUnknownField  = "unknown_field"
	CodeMissing       = "missing"
	CodeMalformed     = "malformed"
	CodeOutOfRange    = "out_of_range"
	CodeDuplicate     = "duplicate"
	CodeNotAllowed    = "not_allowed"
	CodeUnknownRoot   = "unknown_root"
	CodeVolumeMissing = "volume_missing"
)

// ValidationError 表示定义里的某个字段不合法。接口层映射为 400 VALIDATION_ERROR。
//
// 只报第一个错误，与 internal/apps 同一个口径：一次说一条，改完再来。把所有错误攒起来
// 一起回，会让人对着十几条互相牵连的提示猜哪一条是真的。
type ValidationError struct {
	// Field 与 JSON 字段同名（`environment[2].name` 这种带下标的形态），前端可直接高亮。
	Field string
	// Code 是上面那组稳定机器码之一。
	Code string
	// Message 是中文可读说明，不含内部细节、不含秘密值。
	Message string
}

func (e *ValidationError) Error() string { return e.Message }

func invalid(code, field, format string, args ...any) error {
	return &ValidationError{Field: field, Code: code, Message: fmt.Sprintf(format, args...)}
}

// 形状。三条正则与它们各自的来源写在一起，改的时候能看出在改谁。
var (
	// namePattern：小写字母开头，随后是小写字母、数字、下划线、中划线。
	// 与 Compose 项目名同一套字符集；首字符限定为字母是为了让它同时能当服务名和容器名前缀，
	// 也不会被误读成数字或版本号。
	namePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)

	// rootIDPattern：与配置文件里的文件根 id 同形（config.fileRootIDPattern）。
	// 允许下划线开头——这里不用它当容器名，只是查表用的键。
	rootIDPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

	// envNamePattern：POSIX 标识符（CONTRACTS.md §8.2）。
	envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

	// cpuLimitPattern：十进制小数，允许整数部分一位到两位数字。真正的范围检查在 parseCPULimit。
	cpuLimitPattern = regexp.MustCompile(`^[0-9]{1,2}(\.[0-9]{1,3})?$`)
)

// Definition 是受约束的部署定义本体。
//
// 它对应 CONTRACTS.md §8.2 列的十个字段，一个不多一个不少。**没有** id、version、时间：
// 那些是记录层的属性（P15.2）。**也没有** compose_yaml：原始 Compose 是这套东西要替代的
// 输入，不是它的一个字段。
type Definition struct {
	// Name 同时是 Compose 项目名与容器名前缀。
	Name string `json:"name"`
	// ImageReference 允许 tag、digest 或两者同时给；给 digest 才是可复现的。
	ImageReference string `json:"image_reference"`
	// ContainerPort 是容器内监听的端口，也是唯一被发布出去的端口。
	ContainerPort int `json:"container_port"`
	// HostBindAddress 必须是**字面 IP**，不接受主机名：主机名的解析结果会随时间变化，
	// 而一份部署配置不该在三个月后指向另一台机器。
	HostBindAddress string `json:"host_bind_address"`
	// HostPort 是宿主端口。冲突检测与执行锁读同一份声明（Claims）。
	HostPort int `json:"host_port"`
	// Environment 是公开值与秘密引用混排的环境变量。
	Environment []Env `json:"environment,omitempty"`
	// Mounts 是数据目录挂载。
	Mounts []Mount `json:"mounts,omitempty"`
	// ResourceLimits 必填。
	ResourceLimits ResourceLimits `json:"resource_limits"`
	// HealthCheck 是**面板侧**的探测描述，不渲染进 Compose（见 Render 的注释）。
	HealthCheck HealthCheck `json:"health_check,omitempty"`
	// VolumeDependencies 是这份部署赖以存活的数据卷（受管理卷标识）。
	VolumeDependencies []string `json:"volume_dependencies,omitempty"`
}

// Env 是一条环境变量：公开值与秘密引用**二选一**。
//
// 用 *string 而不是 string，是为了把“没有给”和“给了一个空串”分开：
// `FOO=` 在某些镜像里是有意义的（表示显式置空），而“没给”必须是错误——
// 一个悄悄变成空串的数据库连接串会让容器带着半份配置起来。
type Env struct {
	Name string `json:"name"`
	// Value 是公开值，会原样出现在渲染出的配置里。
	Value *string `json:"value,omitempty"`
	// SecretRef 是秘密引用（sec_ + 32 位小写十六进制），明文只在渲染时读一次。
	SecretRef string `json:"secret_ref,omitempty"`
}

// Mount 是一条 bind 挂载。宿主侧只能由“文件根 id + 根内相对路径”表达。
type Mount struct {
	// SourceRootID 是受管理文件根的 id（agent.files.roots[].id）。
	SourceRootID string `json:"source_root_id"`
	// RelativePath 是根内相对路径，空表示根自身。禁 `..`、禁绝对路径。
	RelativePath string `json:"relative_path,omitempty"`
	// ContainerPath 是容器内的绝对路径。禁 `/`、`/proc`、`/sys`、`/dev` 等目标。
	ContainerPath string `json:"container_path"`
	// ReadOnly 表示以只读方式挂进容器（内核强制，不是约定）。
	ReadOnly bool `json:"read_only"`
}

// ResourceLimits 是容器的资源上限。初期两项都必填。
type ResourceLimits struct {
	// MemoryLimitBytes 是内存上限（字节）。整数而不是 "512m"：字符串单位需要再解析一次，
	// 而解析规则（m 是 MiB 还是 MB）在别处是有分歧的。
	MemoryLimitBytes int64 `json:"memory_limit_bytes"`
	// CPULimit 是 CPU 核数上限，十进制字符串（"0.5"）。用字符串而不是 float64：
	// JSON 的浮点会经历一次二进制往返，0.1 这类值在摘要里就会变得不可比对。
	CPULimit string `json:"cpu_limit"`
}

// HealthCheck 是面板侧的探测描述。
//
// 它**不**渲染成 Compose 的 healthcheck 块，理由写在 Render 里：容器内的 healthcheck 需要
// 一条能在镜像里跑起来的命令，而面板不知道镜像里有什么。硬塞 `CMD-SHELL curl -f ...` 会在
// slim 镜像上永远 unhealthy，还会往容器里引入一条 shell 命令。
type HealthCheck struct {
	Enabled bool `json:"enabled"`
	// Kind 只允许 http / tcp。
	Kind string `json:"kind,omitempty"`
	// Port 为 0 表示用 ContainerPort。**只能**是容器端口：面板只发布这一个端口，
	// 探别的端口等于探一个从外面根本连不上的东西。
	Port int `json:"port,omitempty"`
	// Path 是 http 探测的路径，必须以 / 开头。
	Path string `json:"path,omitempty"`
	// IntervalSeconds / TimeoutSeconds / Retries / StartPeriodSeconds 是探测节奏。
	IntervalSeconds    int `json:"interval_seconds,omitempty"`
	TimeoutSeconds     int `json:"timeout_seconds,omitempty"`
	Retries            int `json:"retries,omitempty"`
	StartPeriodSeconds int `json:"start_period_seconds,omitempty"`
}

// Claim 是一条端口占用声明。
//
// 冲突检测与执行锁必须读同一份声明，否则“预检说没冲突、执行为什么失败”会变成常态。
// P15.2 的存储层用它建锁、查冲突，执行前再用同一份数据复核一遍。
type Claim struct {
	Address  string
	Port     int
	Protocol string
}

// 协议闭集。目前只发布 TCP——UDP 的“部署好了没有”没有与 HTTP/TCP 探测同形的判据。
const ProtocolTCP = "tcp"

// 健康检查的两种形态（与 normalizeHealthCheck 的闭集同一份字符串）。
//
// 写在这里是因为探测端（probe.go）要按它分支：两处各写一遍字面量，改一处忘另一处
// 的后果是探测永远报“未知类型”，而不是编译不过。
const (
	HealthCheckHTTP = "http"
	HealthCheckTCP  = "tcp"
)

// Claims 返回这份定义的端口声明。
//
// 只在规范化之后的定义上有意义（地址已是字面 IP）。未规范化的定义也能调用：
// 它只读两个端口字段与地址字段，不做判断。
func (d Definition) Claims() []Claim {
	if d.HostPort < MinPort || d.HostPort > MaxPort {
		return nil
	}
	return []Claim{{Address: d.HostBindAddress, Port: d.HostPort, Protocol: ProtocolTCP}}
}

// DecodeDefinition 解一份部署定义，**拒绝未知字段**。
//
// 这是“危险选项无法表达”的第一道闸：`{"privileged":true}` 在结构体里没有落脚点，
// 而 DisallowUnknownFields 让它变成一条明确的 400，而不是被静默丢弃——静默丢弃会让人
// 以为自己提交成功了。
func DecodeDefinition(r io.Reader) (Definition, error) {
	var d Definition
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return Definition{}, decodeError(err)
	}
	// 后面还有东西（第二个对象、垃圾字节）同样是坏输入：那是一份说不清哪一份生效的请求。
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return Definition{}, invalid(CodeMalformed, "", "请求体里只能有一个 JSON 对象")
	}
	return d, nil
}

// decodeError 把 encoding/json 的错误翻成一句能照做的中文。
func decodeError(err error) error {
	var syn *json.SyntaxError
	if errors.As(err, &syn) {
		return invalid(CodeMalformed, "", "请求体不是合法的 JSON（第 %d 字节附近）", syn.Offset)
	}
	msg := err.Error()
	const unknown = "json: unknown field "
	if strings.HasPrefix(msg, unknown) {
		name := strings.Trim(strings.TrimPrefix(msg, unknown), `"`)
		return &ValidationError{
			Field: name,
			Code:  CodeUnknownField,
			Message: fmt.Sprintf("部署定义里没有 %q 这个字段。定义是闭集（CONTRACTS.md §8.2）："+
				"privileged、devices、network_mode、cap_add、compose_yaml 这类选项在数据模型里"+
				"写不出来，也不能通过请求体加进来", name),
		}
	}
	if strings.HasPrefix(msg, "json: cannot unmarshal ") {
		return invalid(CodeMalformed, "", "字段类型不对：%s", strings.TrimPrefix(msg, "json: cannot unmarshal "))
	}
	return invalid(CodeMalformed, "", "请求体解析失败")
}

// validSecretRefShape 判断秘密引用的形状：sec_ + 32 位小写十六进制。
//
// 与 agentstore.ValidSecretRef、accounts/definition.go 是同一条形状；这里单独写一份而不是
// 引入 store 包，是因为本包要能在代理侧（不拿数据库）跑校验。
func validSecretRefShape(ref string) bool {
	const prefix = "sec_"
	if len(ref) != len(prefix)+32 || !strings.HasPrefix(ref, prefix) {
		return false
	}
	for i := len(prefix); i < len(ref); i++ {
		c := ref[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// isIPLiteral 判断字符串是不是字面 IP（v4 或 v6，不接受方括号、端口、主机名）。
func isIPLiteral(s string) bool {
	return net.ParseIP(s) != nil
}

// formatFloatShorthand 把规范化后的 CPU 上限写成 Compose 能读的十进制字面量。
func formatFloatShorthand(v float64) string {
	s := strconv.FormatFloat(v, 'f', -1, 64)
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return s
}

// sortedVolumeIDs 去重并保持输入顺序（依赖卷的顺序对判定没有意义，但保持它让人读日志时不迷路）。
func sortedVolumeIDs(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, id := range in {
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// sortedEnv 按名字排序（渲染的确定性来自这里）。
func sortedEnv(in []Env) []Env {
	out := make([]Env, len(in))
	copy(out, in)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// sortedMounts 按容器路径排序（同上）。
func sortedMounts(in []Mount) []Mount {
	out := make([]Mount, len(in))
	copy(out, in)
	sort.SliceStable(out, func(i, j int) bool { return out[i].ContainerPath < out[j].ContainerPath })
	return out
}

// validVolumeID 判断卷标识的形状：vol_ + 32 位小写十六进制。
//
// 与 storage.ValidVolumeID 是同一条形状，这里单独写一份而不是引 storage，理由和
// validSecretRefShape 一样：deploy 必须能在代理侧独立跑校验。另外还有一条硬约束——
// internal/storage 依赖 internal/agentclient（读容器/存储事实要先问代理），而
// internal/agentclient 要解部署现场（P15.4），从 deploy 引 storage 会成环。
// 改形状时两处一起改；两边各有一条断言钉着长度与前缀。
func validVolumeID(id string) bool {
	if len(id) != 4+32 || !strings.HasPrefix(id, "vol_") {
		return false
	}
	for i := 4; i < len(id); i++ {
		c := id[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
