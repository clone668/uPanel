// Package config 负责配置读取、默认值、覆盖优先级与校验。
//
// 配置文件为 JSON（严格解码：未知字段直接报错，不接受拼写错误的静默忽略）。
// 优先级：内置默认值 < 配置文件 < 命令行参数。
//
// 固定约定来自 PLAN.md 第 4、5 节：
//   - 面板默认绑定 127.0.0.1:9443；
//   - mock 模式只能绑定 127.0.0.1:18881，与真实代理完全隔离；
//   - 代理只监听 Unix socket，默认 /run/upanel/agent.sock。
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"upanel/internal/httpx"
	"upanel/internal/logging"
	"upanel/internal/platform"
)

// 固定端口与路径（PLAN.md 第 4、5 节，CONTRACTS.md 第 3 节）。
const (
	// ListenProduction 是 production 模式的默认监听地址。
	ListenProduction = "127.0.0.1:9443"
	// ListenMock 是 mock 模式的唯一允许监听地址。
	ListenMock = "127.0.0.1:18881"
	// AgentSocketPath 是特权代理的默认 Unix socket 路径。
	AgentSocketPath = "/run/upanel/agent.sock"
	// AgentWebUser 是普通代理接口唯一允许的对端账号（PLAN.md 第 4 节）。
	AgentWebUser = "upanel-web"

	// 数据库 busy_timeout 的允许范围（毫秒）。
	minBusyTimeoutMilliseconds     = 100
	maxBusyTimeoutMilliseconds     = 60000
	defaultBusyTimeoutMilliseconds = 5000

	minShutdownTimeoutSeconds = 1
	maxShutdownTimeoutSeconds = 120
	defaultShutdownSeconds    = 10

	// SMART 采集间隔的允许范围（PLAN.md 第 8 节：5—60 分钟，默认 15）。
	minSmartIntervalMinutes     = 5
	maxSmartIntervalMinutes     = 60
	defaultSmartIntervalMinutes = 15

	// 网络挂载容量查询时限（秒）。PLAN.md 第 8 节要求超时后标记 unavailable。
	minNetworkCapacityTimeoutSeconds     = 1
	maxNetworkCapacityTimeoutSeconds     = 60
	defaultNetworkCapacityTimeoutSeconds = 10
)

// Config 是配置文件根结构。Web 与代理共用同一份文件，各自只读取自己的段。
type Config struct {
	Web      Web      `json:"web"`
	Agent    Agent    `json:"agent"`
	Database Database `json:"database"`
	Log      Log      `json:"log"`
}

// Web 是 Web 服务配置。
type Web struct {
	// Mode 为 production 或 mock；默认 production。
	Mode string `json:"mode"`
	// ListenAddress 为 host:port；留空时按模式取默认值。
	ListenAddress string `json:"listen_address"`
	// AllowedOrigins 是允许发起状态变更请求的源（scheme://host[:port]）。
	// 留空时按最终监听地址推导（仅对回环地址成立）；非回环地址必须显式配置。
	AllowedOrigins []string `json:"allowed_origins"`
	// ShutdownTimeoutSeconds 为优雅停止等待时间。
	ShutdownTimeoutSeconds int `json:"shutdown_timeout_seconds"`
}

// Agent 是特权代理配置。代理没有 mock 模式。
type Agent struct {
	// SocketPath 必须是绝对路径。
	SocketPath string `json:"socket_path"`
	// ShutdownTimeoutSeconds 为优雅停止等待时间。
	ShutdownTimeoutSeconds int `json:"shutdown_timeout_seconds"`
	// WebUser 是普通接口唯一允许的对端账号（PLAN.md 第 4 节：upanel-web）。
	// 启动时按账号名解析成数字 UID，用 SO_PEERCRED 与内核报告的身份比对。
	WebUser string `json:"web_user"`
	// SocketGroup 是 socket 文件的属组；留空时与 WebUser 相同。
	SocketGroup string `json:"socket_group"`
	// DatabasePath 是代理数据库的绝对路径（PLAN.md 第 4 节：root 独占）。
	// Linux 默认 /var/lib/upanel-agent/agent.db；非 Linux 没有默认值。
	DatabasePath string `json:"database_path"`
	// SecretKeyPath 是本机秘密密钥文件的绝对路径（CONTRACTS.md 14.2，root:root 0600）。
	// Linux 默认 /etc/upanel/agent.key；非 Linux 没有默认值。
	SecretKeyPath string `json:"secret_key_path"`
	// Storage 是存储采集的节奏与工具路径（P04.3）。
	Storage Storage `json:"storage"`
	// Services 是服务模块的配置（P05.1）。
	Services Services `json:"services"`
	// Logs 是日志模块的配置（P05.3）。
	Logs Logs `json:"logs"`
	// Files 是文件模块的配置（P06.1）。
	Files Files `json:"files"`
}

// Files 是文件管理模块配置（P06.1）。
//
// Roots 是**唯一**允许访问的受管理文件根：CONTRACTS.md 第 11 节要求文件 API 的一切
// 路径都落在某个已登记的根之内，面板不提供“服务器任意目录”的入口。因此这份配置是
// 白名单而不是默认值——留空意味着“没有可管理的目录”，不是“整个文件系统”。
type Files struct {
	// Roots 是登记的文件根。允许为空；空表示本机没有开放文件管理。
	Roots []FileRoot `json:"roots"`
	// MaxUploadBytes 是单次上传的字节上限（P06.2）。0 表示取默认值 2 GiB
	// （PLAN.md 第 15 节）。它只能改“多大”，不能改“允许上传”这件事本身——
	// 那是每个根的 writable。
	MaxUploadBytes int64 `json:"max_upload_bytes"`
}

// FileRoot 是一个受管理的文件根。
//
// Path 必须是运维在配置文件里写下的绝对路径：面板不会按请求里的路径去“猜”根，
// 根只能来自 root:root 0600 的配置文件（PLAN.md 第 4 节）。
type FileRoot struct {
	// ID 是请求里使用的稳定标识（小写字母开头，允许数字、下划线与连字符）。
	ID string `json:"id"`
	// Name 是界面上显示的名字。
	Name string `json:"name"`
	// Path 是根的绝对路径。
	Path string `json:"path"`
	// Writable 表示该根允许写入（P06.2 起的上传/修改用它做门槛）。
	// P06.1 只有只读操作，因此这个字段当前不影响任何行为——但它必须如实登记，
	// 否则界面会给出“可写”的错误暗示。
	Writable bool `json:"writable"`
}

// Logs 是日志模块配置（P05.3）。
//
// 只暴露“用哪个 journalctl”。限额（单页 1000 行 / 1 MiB、时间范围 24 小时、
// 单条消息 8 KiB）是产品行为，不通过配置放宽——PLAN.md 第 14 节要求它们始终生效。
type Logs struct {
	// JournalctlPath 是 journalctl 的绝对路径；留空时在 /usr/bin、/bin 里查找。
	JournalctlPath string `json:"journalctl_path"`
}

// Services 是服务模块配置（P05.1）。
//
// 只暴露“用哪个 systemctl”，不暴露采集间隔与保护名单：它们属于产品行为，
// 后者更不能通过配置放宽（PLAN.md 第 14 节：禁止停止面板/代理/SSH/核心/网络服务）。
type Services struct {
	// SystemctlPath 是 systemctl 的绝对路径；留空时在 /usr/bin、/bin 里查找。
	SystemctlPath string `json:"systemctl_path"`
}

// Storage 是存储采集相关配置（PLAN.md 第 8 节：SMART 每 15 分钟，允许配置 5—60 分钟）。
type Storage struct {
	// SmartEnabled 控制是否读取 SMART/NVMe 健康。默认开启；
	// 关掉之后物理盘健康一律 unknown，不会被写成 healthy。
	SmartEnabled bool `json:"smart_enabled"`
	// SmartIntervalMinutes 是 SMART 采集间隔（分钟，5—60，默认 15）。
	SmartIntervalMinutes int `json:"smart_interval_minutes"`
	// SmartctlPath 是 smartctl 的绝对路径；留空时在 /usr/sbin、/usr/bin 等固定目录里查找。
	// 显式给出却找不到时不会退回自动查找——配置写错了要能看出来。
	SmartctlPath string `json:"smartctl_path"`
	// NetworkCapacityTimeoutSeconds 是网络挂载容量查询的时限（秒，1—60，默认 10）。
	// 网络挂载的 statfs 在隔离子进程里执行，超时后该挂载点会被隔离 5 分钟不再重试。
	NetworkCapacityTimeoutSeconds int `json:"network_capacity_timeout_seconds"`
}

// SmartInterval 返回 SMART 采集间隔。
func (s Storage) SmartInterval() time.Duration {
	return time.Duration(s.SmartIntervalMinutes) * time.Minute
}

// NetworkCapacityTimeout 返回网络挂载容量查询的时限。
func (s Storage) NetworkCapacityTimeout() time.Duration {
	return time.Duration(s.NetworkCapacityTimeoutSeconds) * time.Second
}

// Database 是 Web 侧 SQLite 配置（PLAN.md 第 3、4 节）。
type Database struct {
	// Path 必须是绝对路径；Linux 默认 /var/lib/upanel/upanel.db。
	// 非 Linux 平台没有默认值：开发宿主必须显式指定，避免误写到系统目录。
	Path string `json:"path"`
	// BusyTimeoutMilliseconds 是写锁等待上限，默认 5000。
	BusyTimeoutMilliseconds int `json:"busy_timeout_ms"`
}

// Log 是日志配置。
type Log struct {
	Level     string `json:"level"`
	Format    string `json:"format"`
	AddSource bool   `json:"add_source"`
}

// Defaults 返回内置默认配置。
func Defaults() Config {
	return Config{
		Web: Web{
			Mode:                   platform.ModeProduction.String(),
			ListenAddress:          ListenProduction,
			ShutdownTimeoutSeconds: defaultShutdownSeconds,
		},
		Agent: Agent{
			SocketPath:             AgentSocketPath,
			ShutdownTimeoutSeconds: defaultShutdownSeconds,
			WebUser:                AgentWebUser,
			DatabasePath:           defaultAgentDatabasePath(),
			SecretKeyPath:          defaultAgentSecretKeyPath(),
			Storage: Storage{
				SmartEnabled:                  true,
				SmartIntervalMinutes:          defaultSmartIntervalMinutes,
				NetworkCapacityTimeoutSeconds: defaultNetworkCapacityTimeoutSeconds,
			},
			Services: Services{},
		},
		Database: Database{
			Path:                    defaultDatabasePath(),
			BusyTimeoutMilliseconds: defaultBusyTimeoutMilliseconds,
		},
		Log: Log{Level: "info", Format: string(logging.FormatJSON)},
	}
}

// BusyTimeout 返回数据库 busy_timeout。
func (d Database) BusyTimeout() time.Duration {
	return time.Duration(d.BusyTimeoutMilliseconds) * time.Millisecond
}

// Result 是加载结果。
type Result struct {
	Config Config
	// Source 为实际读取的配置文件路径；使用内置默认值时为 ""。
	Source string
	// Explicit 表示路径由调用方显式指定。
	Explicit bool
	// ListenExplicit 表示 web.listen_address 由配置文件显式给出（而不是内建默认值）。
	// mock 模式的安全判断依赖它：只有真正显式写错地址时才报错。
	ListenExplicit bool
}

// Load 读取配置。
//
//   - 显式传入 path：文件必须存在且合法，否则返回错误（不允许悄悄改用默认值）。
//   - 未传入 path：尝试各平台默认路径；不存在时使用内置默认值，Source 为空。
//
// 返回的错误信息包含路径与具体字段问题，供 CLI 直接展示。
func Load(path string) (Result, error) {
	res := Result{Config: Defaults()}

	explicit := strings.TrimSpace(path) != ""
	if !explicit {
		path = defaultPath()
		if path == "" {
			return res, nil
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if !explicit && errors.Is(err, os.ErrNotExist) {
			return res, nil
		}
		return res, fmt.Errorf("读取配置文件失败 %s: %w", path, err)
	}

	cfg := Defaults()
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return res, fmt.Errorf("解析配置文件失败 %s: %w", path, err)
	}
	// 拒绝文件尾部的多余内容，避免“改了两个对象只生效一个”的静默错误。
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return res, fmt.Errorf("解析配置文件失败 %s: 文件在首个 JSON 对象之后仍有内容", path)
	}

	res.Config = cfg
	res.Source = path
	res.Explicit = explicit

	// 记录 web.listen_address 是否在文件中显式出现（严格解码已通过，这里的宽松解析只用于
	// 判断“键是否存在”，不参与取值）。
	var presence struct {
		Web map[string]json.RawMessage `json:"web"`
	}
	if err := json.Unmarshal(data, &presence); err == nil {
		_, res.ListenExplicit = presence.Web["listen_address"]
	}
	return res, nil
}

// Validate 校验整份配置。返回的错误面向运维人员，指出具体字段。
func (c Config) Validate() error {
	if err := c.Web.Validate(); err != nil {
		return err
	}
	if err := c.Agent.Validate(); err != nil {
		return err
	}
	if err := c.Database.Validate(); err != nil {
		return err
	}
	return c.Log.Validate()
}

// Validate 校验数据库段。
//
// 路径为空表示“本平台没有默认数据库位置”，此时必须由命令行显式给出；
// 校验本身不强制非空，是为了让 check-config 能如实报告“未配置”。
func (d Database) Validate() error {
	if d.Path != "" {
		if !path.IsAbs(d.Path) {
			return fmt.Errorf("database.path 必须是 POSIX 绝对路径，当前为 %q", d.Path)
		}
		if strings.ContainsAny(d.Path, "?#") {
			return fmt.Errorf("database.path 不能包含 '?' 或 '#'，当前为 %q", d.Path)
		}
	}
	if d.BusyTimeoutMilliseconds < minBusyTimeoutMilliseconds || d.BusyTimeoutMilliseconds > maxBusyTimeoutMilliseconds {
		return fmt.Errorf("database.busy_timeout_ms 必须在 %d—%d 之间，当前为 %d",
			minBusyTimeoutMilliseconds, maxBusyTimeoutMilliseconds, d.BusyTimeoutMilliseconds)
	}
	return nil
}

// Validate 校验 Web 段。
func (w Web) Validate() error {
	if _, err := platform.ParseMode(w.Mode); err != nil {
		return fmt.Errorf("web.mode: %w", err)
	}
	if _, err := validateListen(w.ListenAddress); err != nil {
		return fmt.Errorf("web.listen_address: %w", err)
	}
	if err := validateAllowedOrigins(w.AllowedOrigins); err != nil {
		return fmt.Errorf("web.allowed_origins: %w", err)
	}
	// “非回环地址必须显式配置允许源”这条跨字段规则放在 ResolveAllowedOrigins 里判断：
	// 它需要最终监听地址，而这里是配置段的单段校验。check-config 与启动路径都会调用它。
	if w.ShutdownTimeoutSeconds < minShutdownTimeoutSeconds || w.ShutdownTimeoutSeconds > maxShutdownTimeoutSeconds {
		return fmt.Errorf("web.shutdown_timeout_seconds 必须在 %d—%d 之间，当前为 %d",
			minShutdownTimeoutSeconds, maxShutdownTimeoutSeconds, w.ShutdownTimeoutSeconds)
	}
	return nil
}

// Validate 校验代理段。
func (a Agent) Validate() error {
	if a.SocketPath == "" {
		return errors.New("agent.socket_path 不能为空")
	}
	// 代理只在 Linux 上运行，socket 路径按 POSIX 语义判断（不受开发宿主路径分隔符影响）。
	if !path.IsAbs(a.SocketPath) {
		return fmt.Errorf("agent.socket_path 必须是 POSIX 绝对路径，当前为 %q", a.SocketPath)
	}
	if a.ShutdownTimeoutSeconds < minShutdownTimeoutSeconds || a.ShutdownTimeoutSeconds > maxShutdownTimeoutSeconds {
		return fmt.Errorf("agent.shutdown_timeout_seconds 必须在 %d—%d 之间，当前为 %d",
			minShutdownTimeoutSeconds, maxShutdownTimeoutSeconds, a.ShutdownTimeoutSeconds)
	}
	if err := validateAccountName("agent.web_user", a.WebUser); err != nil {
		return err
	}
	if a.SocketGroup != "" {
		if err := validateAccountName("agent.socket_group", a.SocketGroup); err != nil {
			return err
		}
	}
	// 两个代理数据路径都按 POSIX 语义校验。空值表示“本平台没有默认路径”，
	// 必须在配置文件或命令行显式给出——由启动路径判断，而不是在这里悄悄填一个。
	for _, item := range []struct{ field, value string }{
		{"agent.database_path", a.DatabasePath},
		{"agent.secret_key_path", a.SecretKeyPath},
	} {
		if item.value == "" {
			continue
		}
		if !path.IsAbs(item.value) {
			return fmt.Errorf("%s 必须是 POSIX 绝对路径，当前为 %q", item.field, item.value)
		}
		if strings.ContainsAny(item.value, "?#") {
			return fmt.Errorf("%s 不能包含 '?' 或 '#'，当前为 %q", item.field, item.value)
		}
	}
	if err := a.Storage.Validate(); err != nil {
		return err
	}
	if err := a.Logs.Validate(); err != nil {
		return err
	}
	if err := a.Files.Validate(); err != nil {
		return err
	}
	return a.Services.Validate()
}

// fileRootIDPattern 是文件根 id 的允许形态（与代理登记表的名字规则独立：
// 根 id 出现在 URL 查询参数里，不需要点分，但同样只接受小写与数字）。
var fileRootIDPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// Validate 校验文件模块配置。
//
// 这里拒绝三类“看起来能跑但会毁掉边界”的配置：
//   - 重复 id（同一个 id 指向两个目录时，请求根本无法表达要哪一个）；
//   - 路径不是绝对路径、或含 ".." 之类的段（配置写成相对路径时，最终解析取决于进程 cwd，
//     重启后可能指向别处）；
//   - 根路径为 "/"：那等于把整个文件系统登记成一个根，正是“可管理整个服务器所有目录”。
//     要开放某个目录，就明确写它。
func (f Files) Validate() error {
	if f.MaxUploadBytes < 0 {
		return fmt.Errorf("agent.files.max_upload_bytes 不能为负，当前为 %d", f.MaxUploadBytes)
	}
	// 上限本身不设“太小也行”：一个明显不是字节数的值（比如误写成 2048）在这里拦下来，
	// 免得运维以为放宽到了一定程度、实际传不了几张图。
	if f.MaxUploadBytes > 0 && f.MaxUploadBytes < 1<<20 {
		return fmt.Errorf("agent.files.max_upload_bytes 必须至少 1 MiB（当前 %d）；"+
			"0 表示使用默认 2 GiB", f.MaxUploadBytes)
	}
	seen := make(map[string]bool, len(f.Roots))
	for i, root := range f.Roots {
		field := fmt.Sprintf("agent.files.roots[%d]", i)
		if !fileRootIDPattern.MatchString(root.ID) {
			return fmt.Errorf("%s.id 不合法（应为小写字母开头、只含小写字母/数字/下划线/连字符，最多 32 字符），当前为 %q",
				field, root.ID)
		}
		if seen[root.ID] {
			return fmt.Errorf("%s.id 重复：%q 已经被另一个文件根使用", field, root.ID)
		}
		seen[root.ID] = true
		if err := root.Validate(field); err != nil {
			return err
		}
	}
	return nil
}

// Validate 校验单个文件根。
func (r FileRoot) Validate(field string) error {
	name := strings.TrimSpace(r.Name)
	if name == "" {
		return fmt.Errorf("%s.name 不能为空", field)
	}
	if utf8.RuneCountInString(name) > 64 {
		return fmt.Errorf("%s.name 最多 64 个字符", field)
	}
	for _, runeValue := range name {
		if runeValue < 0x20 || runeValue == 0x7f {
			return fmt.Errorf("%s.name 含有控制字符", field)
		}
	}
	if r.Path == "" {
		return fmt.Errorf("%s.path 不能为空", field)
	}
	if !path.IsAbs(r.Path) {
		return fmt.Errorf("%s.path 必须是 POSIX 绝对路径，当前为 %q", field, r.Path)
	}
	if strings.ContainsAny(r.Path, "?#\x00") {
		return fmt.Errorf("%s.path 不能包含 '?'、'#' 或 NUL，当前为 %q", field, r.Path)
	}
	if path.Clean(r.Path) != r.Path {
		return fmt.Errorf("%s.path 必须是规范化路径（不含 '.'、'..'、重复或结尾的 '/'），当前为 %q",
			field, r.Path)
	}
	if r.Path == "/" {
		return fmt.Errorf("%s.path 不能是根目录 '/': 那等于把整个服务器文件系统登记成一个文件根；"+
			"请登记具体目录", field)
	}
	return nil
}

// Validate 校验日志模块配置。与 Services 同理：只校验路径，
// 不给“放宽限额”这类配置留入口。
func (l Logs) Validate() error {
	if l.JournalctlPath == "" {
		return nil
	}
	if !path.IsAbs(l.JournalctlPath) {
		return fmt.Errorf("agent.logs.journalctl_path 必须是 POSIX 绝对路径，当前为 %q", l.JournalctlPath)
	}
	if strings.ContainsAny(l.JournalctlPath, "?#") {
		return fmt.Errorf("agent.logs.journalctl_path 不能包含 '?' 或 '#'，当前为 %q", l.JournalctlPath)
	}
	return nil
}

// Validate 校验服务模块配置。
//
// 只校验路径：不给“放宽保护名单”这类配置留入口（PLAN.md 第 14 节）。
func (s Services) Validate() error {
	if s.SystemctlPath == "" {
		return nil
	}
	if !path.IsAbs(s.SystemctlPath) {
		return fmt.Errorf("agent.services.systemctl_path 必须是 POSIX 绝对路径，当前为 %q", s.SystemctlPath)
	}
	if strings.ContainsAny(s.SystemctlPath, "?#") {
		return fmt.Errorf("agent.services.systemctl_path 不能包含 '?' 或 '#'，当前为 %q", s.SystemctlPath)
	}
	return nil
}

// Validate 校验存储采集段。
//
// 范围来自 PLAN.md 第 8 节：SMART 允许配置为 5—60 分钟。超出范围直接报错，
// 而不是悄悄钳制——运维以为设成了 1 分钟，界面却每 15 分钟才更新，那更难查。
func (s Storage) Validate() error {
	if s.SmartIntervalMinutes < minSmartIntervalMinutes || s.SmartIntervalMinutes > maxSmartIntervalMinutes {
		return fmt.Errorf("agent.storage.smart_interval_minutes 必须在 %d—%d 之间，当前为 %d",
			minSmartIntervalMinutes, maxSmartIntervalMinutes, s.SmartIntervalMinutes)
	}
	if s.NetworkCapacityTimeoutSeconds < minNetworkCapacityTimeoutSeconds ||
		s.NetworkCapacityTimeoutSeconds > maxNetworkCapacityTimeoutSeconds {
		return fmt.Errorf("agent.storage.network_capacity_timeout_seconds 必须在 %d—%d 之间，当前为 %d",
			minNetworkCapacityTimeoutSeconds, maxNetworkCapacityTimeoutSeconds,
			s.NetworkCapacityTimeoutSeconds)
	}
	if s.SmartctlPath != "" {
		if !path.IsAbs(s.SmartctlPath) {
			return fmt.Errorf("agent.storage.smartctl_path 必须是 POSIX 绝对路径，当前为 %q", s.SmartctlPath)
		}
		if strings.ContainsAny(s.SmartctlPath, "?#") {
			return fmt.Errorf("agent.storage.smartctl_path 不能包含 '?' 或 '#'，当前为 %q", s.SmartctlPath)
		}
	}
	return nil
}

// validateAccountName 校验账号/组名：只允许常规名字，不接受数字 UID、路径或空值。
// 使用数字 UID 会绕过“专用账号”的要求，因此这里直接拒绝纯数字。
func validateAccountName(field, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("%s 不能为空（普通接口只允许专用 Web 账号访问）", field)
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return fmt.Errorf("%s 含有非法字符 %q，只接受账号名而不是数字 UID 或路径", field, r)
		}
	}
	if _, err := strconv.Atoi(name); err == nil {
		return fmt.Errorf("%s 不能是纯数字的 UID %q：必须使用专用账号名", field, name)
	}
	return nil
}

// ShutdownTimeout 返回代理优雅停止等待时间。
func (a Agent) ShutdownTimeout() time.Duration {
	return time.Duration(a.ShutdownTimeoutSeconds) * time.Second
}

// Validate 校验日志段。
func (l Log) Validate() error {
	if _, err := logging.ParseLevel(l.Level); err != nil {
		return fmt.Errorf("log.level: %w", err)
	}
	if _, err := logging.ParseFormat(l.Format); err != nil {
		return fmt.Errorf("log.format: %w", err)
	}
	return nil
}

// ModeValue 返回解析后的运行模式（Mode 字段名留给配置字段本身）。
func (w Web) ModeValue() (platform.Mode, error) {
	return platform.ParseMode(w.Mode)
}

// ResolveListen 计算最终监听地址。
//
// listenExplicit 表示地址由命令行或配置文件显式给出：
//   - mock 模式且未显式指定 → 固定 127.0.0.1:18881；
//   - mock 模式且显式指定为其他地址 → 报错，不允许 mock 暴露到别处；
//   - production 模式 → 使用给定地址。
func ResolveListen(mode platform.Mode, address string, listenExplicit bool) (string, error) {
	address = strings.TrimSpace(address)
	if mode == platform.ModeMock {
		if !listenExplicit {
			return ListenMock, nil
		}
		if address != ListenMock {
			return "", fmt.Errorf("mock 模式只能绑定 %s，当前配置为 %q（演示数据不得暴露到其他地址）", ListenMock, address)
		}
		return ListenMock, nil
	}
	if address == "" {
		return ListenProduction, nil
	}
	if _, err := validateListen(address); err != nil {
		return "", err
	}
	return address, nil
}

// ShutdownTimeout 返回优雅停止等待时间。
func (w Web) ShutdownTimeout() time.Duration {
	return time.Duration(w.ShutdownTimeoutSeconds) * time.Second
}

// validateListen 校验 host:port 形式。
//
// 要求显式主机：":9443" 这类“所有接口”的写法必须写成 "0.0.0.0:9443" 或 "::"，
// 避免误配置把面板暴露到公网。
func validateListen(address string) (string, error) {
	address = strings.TrimSpace(address)
	if address == "" {
		return ListenProduction, nil
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", fmt.Errorf("不是合法的 host:port（%v）；例如 127.0.0.1:9443", err)
	}
	if strings.TrimSpace(host) == "" {
		return "", errors.New("必须显式给出监听主机，例如 127.0.0.1 或 0.0.0.0，不能用 :port 省略")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("端口必须是 1—65535 之间的整数，当前为 %q", port)
	}
	return address, nil
}

// validateAllowedOrigins 校验 web.allowed_origins 的每一项都能规范化。
//
// 允许为空（回环部署时推导即可）；但不允许写入无法识别的值——那只会让登录
// 在运行时诡异失败，而不是在启动时明确报错。
func validateAllowedOrigins(origins []string) error {
	for i, origin := range origins {
		if _, err := httpx.NormalizeOrigin(origin); err != nil {
			return fmt.Errorf("第 %d 项不合法: %w", i+1, err)
		}
	}
	return nil
}

// ResolveAllowedOrigins 计算最终允许源列表。
//
//   - 配置里显式给出了源 → 规范化、去重后使用（无论监听地址是什么）。
//   - 配置为空且监听地址是回环 → 用 http://127.0.0.1:<port>、http://localhost:<port>
//     以及监听主机自身（当它既不是 127.0.0.1 也不是 localhost 时，例如 ::1）。
//   - 配置为空且监听地址非回环 → 返回错误。
//
// 期望值只来自本地配置，**不**从 Host 头或 X-Forwarded-* 推导（CONTRACTS.md 第 2 节）。
func ResolveAllowedOrigins(origins []string, listenAddress string) ([]string, error) {
	if len(origins) > 0 {
		set, err := httpx.NewOriginSet(origins)
		if err != nil {
			return nil, fmt.Errorf("web.allowed_origins: %w", err)
		}
		return set.List(), nil
	}

	host, port, err := net.SplitHostPort(strings.TrimSpace(listenAddress))
	if err != nil {
		return nil, fmt.Errorf("web.allowed_origins: 无法从监听地址 %q 推导允许源: %w", listenAddress, err)
	}
	if !IsLoopbackAddress(listenAddress) {
		return nil, fmt.Errorf("web.allowed_origins 不能为空：监听地址 %s 不是回环地址", listenAddress)
	}

	hosts := []string{"127.0.0.1", "localhost"}
	if h := strings.Trim(host, "[]"); h != "" && h != "127.0.0.1" && !strings.EqualFold(h, "localhost") {
		hosts = append(hosts, h)
	}
	raw := make([]string, 0, len(hosts))
	for _, h := range hosts {
		raw = append(raw, "http://"+net.JoinHostPort(h, port))
	}
	set, err := httpx.NewOriginSet(raw)
	if err != nil {
		return nil, fmt.Errorf("web.allowed_origins: 推导出的源不合法: %w", err)
	}
	return set.List(), nil
}

// IsLoopbackAddress 判断监听地址是否只绑定回环接口。
func IsLoopbackAddress(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
