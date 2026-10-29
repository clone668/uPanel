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

	// 历史监控（P13.1）。默认值就是 PLAN.md 第 18 节的 24h / 30d / 365d；
	// 范围允许压缩到分钟级——那是一台测试虚拟机验证“保留真的会删数据”的唯一办法，
	// 生产机上把它改成分钟级只会丢掉历史，不会带来别的好处。
	minMetricsIntervalSeconds        = 1
	maxMetricsIntervalSeconds        = 60
	defaultMetricsIntervalSeconds    = 2
	minMetricsRetentionSeconds       = 60
	maxMetricsRawRetentionSeconds    = 7 * 24 * 3600
	maxMetricsMinuteRetentionSeconds = 365 * 24 * 3600
	maxMetricsHourRetentionSeconds   = 10 * 365 * 24 * 3600
	defaultMetricsRawRetention       = 24 * 3600
	defaultMetricsMinuteRetention    = 30 * 24 * 3600
	defaultMetricsHourRetention      = 365 * 24 * 3600
	minMetricsMaxRows                = 10_000
	maxMetricsMaxRows                = 50_000_000
	defaultMetricsMaxRows            = 2_000_000

	// 终端账号与 home（PLAN.md 第 4 节：upanel-shell、/var/lib/upanel-shell）。
	defaultShellUser    = "upanel-shell"
	defaultTerminalHome = "/var/lib/upanel-shell"

	// 终端三个时限（PLAN.md 第 14 节：15 分钟无输入关闭、最长 1 小时、宽限 60 秒）。
	// 下限允许压到“能验收”的量级，上限就是计划值本身：只能收紧。
	minTerminalIdleSeconds            = 60
	defaultTerminalIdleSeconds        = 900
	minTerminalMaxDurationSeconds     = 60
	defaultTerminalMaxDurationSeconds = 3600
	minTerminalGraceSeconds           = 5
	defaultTerminalGraceSeconds       = 60
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
	// TLSCertPath 是面板 HTTPS 证书（PEM）的绝对路径；与 TLSKeyPath 必须成对出现。
	// 留空表示“经 SSH 隧道访问的明文部署”，只在回环监听时允许（见 CheckTransport）。
	TLSCertPath string `json:"tls_cert_path"`
	// TLSKeyPath 是面板 HTTPS 私钥（PEM）的绝对路径。
	TLSKeyPath string `json:"tls_key_path"`
	// ShutdownTimeoutSeconds 为优雅停止等待时间。
	ShutdownTimeoutSeconds int `json:"shutdown_timeout_seconds"`
	// Metrics 是历史监控的写入与保留参数（P13.1）。
	Metrics Metrics `json:"metrics"`
}

// Metrics 是历史监控（P13.1；PLAN.md 第 18 节）。
//
// 采集与聚合都在**面板进程内**（PLAN.md 第 17 节：监控采集使用程序内固定调度，
// 不暴露任意 root 监控脚本任务）。这里只暴露节奏与保留窗口，不暴露“采哪些指标”：
// 指标集合是产品行为，改它意味着告警与图表的含义一起变。
type Metrics struct {
	// Enabled 控制是否写历史。关掉之后 /system/metrics 回 enabled=false，
	// 首页的实时指标不受影响。
	Enabled bool `json:"enabled"`
	// IntervalSeconds 是轮询代理快照的间隔（秒，1—60，默认 2）。
	//
	// 默认值比代理的 5 秒采集快一档：5 秒轮询 5 秒采集会踩点，踩上就丢一整份快照。
	// 写入按快照的 collected_at 去重，所以更勤的轮询不会写重。
	IntervalSeconds int `json:"interval_seconds"`
	// RawRetentionSeconds 是原始样本的保留时长（秒，默认 86400 = 24 小时）。
	RawRetentionSeconds int `json:"raw_retention_seconds"`
	// MinuteRetentionSeconds 是 1 分钟聚合的保留时长（秒，默认 2592000 = 30 天）。
	MinuteRetentionSeconds int `json:"minute_retention_seconds"`
	// HourRetentionSeconds 是 1 小时聚合的保留时长（秒，默认 31536000 = 365 天）。
	HourRetentionSeconds int `json:"hour_retention_seconds"`
	// MaxRows 是 metric_samples 的行数上限。超过时从最老的原始样本开始丢整桶，
	// 保证面板的数据库不会被历史撑满（CONTRACTS.md 第 14.1 节）。
	MaxRows int `json:"max_rows"`
}

// Interval 返回轮询间隔。
func (m Metrics) Interval() time.Duration {
	return time.Duration(m.IntervalSeconds) * time.Second
}

// Retention 返回三个粒度的保留窗口。
func (m Metrics) Retention() (raw, minute, hour time.Duration) {
	return time.Duration(m.RawRetentionSeconds) * time.Second,
		time.Duration(m.MinuteRetentionSeconds) * time.Second,
		time.Duration(m.HourRetentionSeconds) * time.Second
}

// Validate 校验历史监控段。
func (m Metrics) Validate() error {
	if m.IntervalSeconds < minMetricsIntervalSeconds || m.IntervalSeconds > maxMetricsIntervalSeconds {
		return fmt.Errorf("web.metrics.interval_seconds 必须在 %d—%d 之间，当前为 %d",
			minMetricsIntervalSeconds, maxMetricsIntervalSeconds, m.IntervalSeconds)
	}
	if m.RawRetentionSeconds < minMetricsRetentionSeconds || m.RawRetentionSeconds > maxMetricsRawRetentionSeconds {
		return fmt.Errorf("web.metrics.raw_retention_seconds 必须在 %d—%d 之间，当前为 %d",
			minMetricsRetentionSeconds, maxMetricsRawRetentionSeconds, m.RawRetentionSeconds)
	}
	if m.MinuteRetentionSeconds < minMetricsRetentionSeconds || m.MinuteRetentionSeconds > maxMetricsMinuteRetentionSeconds {
		return fmt.Errorf("web.metrics.minute_retention_seconds 必须在 %d—%d 之间，当前为 %d",
			minMetricsRetentionSeconds, maxMetricsMinuteRetentionSeconds, m.MinuteRetentionSeconds)
	}
	if m.HourRetentionSeconds < minMetricsRetentionSeconds || m.HourRetentionSeconds > maxMetricsHourRetentionSeconds {
		return fmt.Errorf("web.metrics.hour_retention_seconds 必须在 %d—%d 之间，当前为 %d",
			minMetricsRetentionSeconds, maxMetricsHourRetentionSeconds, m.HourRetentionSeconds)
	}
	// 粒度越粗留得越久：反过来的配置会让聚合数据比它的来源先消失，
	// 结果是“原始数据还在，但分钟曲线缺了一段”，这不像是配置，像是故障。
	if m.MinuteRetentionSeconds < m.RawRetentionSeconds {
		return fmt.Errorf("web.metrics.minute_retention_seconds（%d）不能小于 raw_retention_seconds（%d）",
			m.MinuteRetentionSeconds, m.RawRetentionSeconds)
	}
	if m.HourRetentionSeconds < m.MinuteRetentionSeconds {
		return fmt.Errorf("web.metrics.hour_retention_seconds（%d）不能小于 minute_retention_seconds（%d）",
			m.HourRetentionSeconds, m.MinuteRetentionSeconds)
	}
	if m.MaxRows < minMetricsMaxRows || m.MaxRows > maxMetricsMaxRows {
		return fmt.Errorf("web.metrics.max_rows 必须在 %d—%d 之间，当前为 %d",
			minMetricsMaxRows, maxMetricsMaxRows, m.MaxRows)
	}
	return nil
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
	// Terminal 是受限终端的配置（P06.3）。
	Terminal Terminal `json:"terminal"`
	// Certificates 是证书模块的配置（P10.3）。
	Certificates Certificates `json:"certificates"`
	// Backup 是备份模块的配置（P12.2）。
	Backup Backup `json:"backup"`
}

// Backup 是备份模块的配置（P12.2）。
//
// 只装"这台机器上的位置"，不装策略：备份什么、多久备一次由 Web 侧的策略定义，
// 代理只负责按策略干活。面板库的位置是代理要读的东西，所以它必须由 root 的配置
// 决定，而不是由请求体决定——否则一次请求就能让 root 去读任意文件。
type Backup struct {
	// PanelDatabasePath 是面板数据库的路径。Linux 默认与 Web 的默认值相同
	// （/var/lib/upanel/upanel.db）：两者本来就在同一台机器上。
	// 留空表示"不知道面板库在哪"：列了 panel_database 来源的策略会在预检里被挡住。
	PanelDatabasePath string `json:"panel_database_path"`
	// PanelCopyDir 是面板库一致副本的落盘目录。留空时用代理 StateDir 下的
	// panel-copies（root:root 0700）。副本里有会话与口令哈希，只在一次运行期间存在。
	PanelCopyDir string `json:"panel_copy_dir"`
}

// Certificates 是证书模块的配置（P10.3）。
//
// 三个路径/地址都有默认值（证书目录、webroot、certbot 路径与 Let's Encrypt 的两个
// 入口都由代码常量决定），这里存在的理由只有一个：**验收与私有 ACME**。
//
//	staging_directory_url / production_directory_url  指向一个本地测试 ACME 服务器，
//	这样“签发流程”可以在没有公网域名的机器上跑完整条链路。
//
// 面板只能选「staging 还是正式」，**不能指定地址**：地址来自这份 root 独占的配置。
// 否则一个 HTTP 参数就能让 root 的 certbot 去连任意主机。
type Certificates struct {
	// CertbotPath 是 certbot 可执行文件的绝对路径；留空用 /usr/bin/certbot。
	CertbotPath string `json:"certbot_path"`
	// ACMEWebroot 是 HTTP-01 校验文件的根目录；留空用 /var/www/upanel-acme。
	ACMEWebroot string `json:"acme_webroot"`
	// StagingDirectoryURL 覆盖 ACME 测试环境目录地址。
	StagingDirectoryURL string `json:"staging_directory_url"`
	// ProductionDirectoryURL 覆盖 ACME 正式环境目录地址。
	//
	// **改这个值等于换掉信任根**：面板上勾「正式签发」时，certbot 会连这个地址。
	// 默认值是 Let's Encrypt；填别的只该发生在私有 CA 或验收环境里。
	ProductionDirectoryURL string `json:"production_directory_url"`
}

// Validate 校验证书模块的配置。
func (c Certificates) Validate() error {
	for _, item := range []struct{ field, value string }{
		{"agent.certificates.certbot_path", c.CertbotPath},
		{"agent.certificates.acme_webroot", c.ACMEWebroot},
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
	for _, item := range []struct{ field, value string }{
		{"agent.certificates.staging_directory_url", c.StagingDirectoryURL},
		{"agent.certificates.production_directory_url", c.ProductionDirectoryURL},
	} {
		if item.value == "" {
			continue
		}
		// 只允许 https：ACME 的账号与订单都走 HTTPS，明文目录地址会让 certbot
		// 去连一个 http 端点（它自己会拒），错误信息出现在很远的后面。
		if !strings.HasPrefix(item.value, "https://") {
			return fmt.Errorf("%s 必须是 https:// 开头的地址，当前为 %q", item.field, item.value)
		}
		if strings.ContainsAny(item.value, " 	") {
			return fmt.Errorf("%s 不能包含空白，当前为 %q", item.field, item.value)
		}
	}
	return nil
}

// Terminal 是受限 Web 终端的配置（P06.3）。
//
// 三个时限只能**收紧**不能放宽：PLAN.md 第 14 节写明 15 分钟无输入关闭、最长 1 小时、
// 断流宽限 60 秒。把它们做成可调的理由不是“让运维自己决定安全边界”，
// 而是“让验收能真的等到它发生”——把 15 分钟压到 90 秒是收紧，是允许的；
// 调成 2 小时会把那条要求变成一句空话，所以 Validate 直接拒绝。
//
// shell_user 与 home 属于安装事实（PLAN.md 第 4 节：upanel-shell、/var/lib/upanel-shell），
// 留空就按计划值来。代理启动时会核对该账号存在、不是 root、shell 不是 nologin、
// home 不属于别人也不能被别人写；任何一项不满足都**不登记**终端能力，
// 而不是带着一个注定起不来的 shell 对外宣称“有终端”。
type Terminal struct {
	// ShellUser 是运行 shell 的系统账号名。
	ShellUser string `json:"shell_user"`
	// HomePath 是 shell 的工作目录；留空时取计划值或账号自身属性。
	HomePath string `json:"home_path"`
	// ShellPath 是 shell 可执行文件；留空时取账号自身的登录 shell。
	ShellPath string `json:"shell_path"`
	// IdleTimeoutSeconds 是无输入自动关闭的秒数（60—900）。
	IdleTimeoutSeconds int `json:"idle_timeout_seconds"`
	// MaxDurationSeconds 是会话绝对寿命的秒数（60—3600）。
	MaxDurationSeconds int `json:"max_duration_seconds"`
	// DisconnectGraceSeconds 是断流后保留会话的秒数（5—60）。
	DisconnectGraceSeconds int `json:"disconnect_grace_seconds"`
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
			Metrics: Metrics{
				Enabled:                true,
				IntervalSeconds:        defaultMetricsIntervalSeconds,
				RawRetentionSeconds:    defaultMetricsRawRetention,
				MinuteRetentionSeconds: defaultMetricsMinuteRetention,
				HourRetentionSeconds:   defaultMetricsHourRetention,
				MaxRows:                defaultMetricsMaxRows,
			},
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
			Backup:   Backup{PanelDatabasePath: defaultDatabasePath()},
			Terminal: Terminal{
				ShellUser:              defaultShellUser,
				HomePath:               defaultTerminalHome,
				IdleTimeoutSeconds:     defaultTerminalIdleSeconds,
				MaxDurationSeconds:     defaultTerminalMaxDurationSeconds,
				DisconnectGraceSeconds: defaultTerminalGraceSeconds,
			},
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
	if err := w.validateTLSPaths(); err != nil {
		return err
	}
	// “非回环地址必须显式配置允许源”这条跨字段规则放在 ResolveAllowedOrigins 里判断：
	// 它需要最终监听地址，而这里是配置段的单段校验。check-config 与启动路径都会调用它。
	if w.ShutdownTimeoutSeconds < minShutdownTimeoutSeconds || w.ShutdownTimeoutSeconds > maxShutdownTimeoutSeconds {
		return fmt.Errorf("web.shutdown_timeout_seconds 必须在 %d—%d 之间，当前为 %d",
			minShutdownTimeoutSeconds, maxShutdownTimeoutSeconds, w.ShutdownTimeoutSeconds)
	}
	if err := w.Metrics.Validate(); err != nil {
		return err
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
	// 备份的两条路径：面板库是"要读的文件"，副本目录是"要写的目录"。
	// 两者都按 POSIX 语义校验；空值表示未配置（面板库空 = 不知道在哪）。
	for _, item := range []struct{ field, value string }{
		{"agent.backup.panel_database_path", a.Backup.PanelDatabasePath},
		{"agent.backup.panel_copy_dir", a.Backup.PanelCopyDir},
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
	if err := a.Terminal.Validate(); err != nil {
		return err
	}
	if err := a.Certificates.Validate(); err != nil {
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

// Validate 校验终端模块配置。
//
// 它只做“能不能描述清楚”的校验（路径形态、时限范围），**不**去问账号是否存在：
// 那件事在代理启动时做（terminal.ResolveAccount），因为 config 包要在两个平台上
// 都能跑，而账号解析只有 Linux 有。分开之后，配置文件写错会在 check-config 阶段
// 报出来，账号没建好会在代理启动日志里报出来——两类问题的答案不在同一个地方。
func (t Terminal) Validate() error {
	if err := validateAccountName("agent.terminal.shell_user", t.ShellUser); err != nil {
		return err
	}
	for _, item := range []struct{ field, value string }{
		{"agent.terminal.home_path", t.HomePath},
		{"agent.terminal.shell_path", t.ShellPath},
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
	for _, item := range []struct {
		field string
		value int
		def   int
		min   int
		max   int
		unit  string
		hint  string
	}{
		{"agent.terminal.idle_timeout_seconds", t.IdleTimeoutSeconds, defaultTerminalIdleSeconds,
			minTerminalIdleSeconds, defaultTerminalIdleSeconds, "秒", "15 分钟无输入关闭"},
		{"agent.terminal.max_duration_seconds", t.MaxDurationSeconds, defaultTerminalMaxDurationSeconds,
			minTerminalMaxDurationSeconds, defaultTerminalMaxDurationSeconds, "秒", "最长会话 1 小时"},
		{"agent.terminal.disconnect_grace_seconds", t.DisconnectGraceSeconds, defaultTerminalGraceSeconds,
			minTerminalGraceSeconds, defaultTerminalGraceSeconds, "秒", "断流宽限 60 秒"},
	} {
		value := item.value
		if value == 0 {
			// 0 表示“用计划值”，不是“关闭这个限制”。
			continue
		}
		if value < item.min || value > item.max {
			return fmt.Errorf("%s 必须在 %d—%d %s 之间（%s；只能比计划值更严，不能更宽），当前为 %d",
				item.field, item.min, item.max, item.unit, item.hint, value)
		}
	}
	return nil
}

// IdleTimeout 返回生效的空闲时限（配置为 0 时取计划值）。
func (t Terminal) IdleTimeout() time.Duration {
	return durationOrDefault(t.IdleTimeoutSeconds, defaultTerminalIdleSeconds)
}

// MaxDuration 返回生效的会话最长时限。
func (t Terminal) MaxDuration() time.Duration {
	return durationOrDefault(t.MaxDurationSeconds, defaultTerminalMaxDurationSeconds)
}

// DisconnectGrace 返回生效的断流宽限。
func (t Terminal) DisconnectGrace() time.Duration {
	return durationOrDefault(t.DisconnectGraceSeconds, defaultTerminalGraceSeconds)
}

// EffectiveShellUser 返回生效的终端账号名。
func (t Terminal) EffectiveShellUser() string {
	if t.ShellUser == "" {
		return defaultShellUser
	}
	return t.ShellUser
}

// EffectiveHome 返回生效的终端 home（配置为空时给计划值）。
func (t Terminal) EffectiveHome() string {
	if t.HomePath == "" {
		return defaultTerminalHome
	}
	return t.HomePath
}

// durationOrDefault 把秒数转成 time.Duration。
func durationOrDefault(seconds, def int) time.Duration {
	if seconds <= 0 {
		seconds = def
	}
	return time.Duration(seconds) * time.Second
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
//   - 配置为空且监听地址是回环 → 用 <scheme>://127.0.0.1:<port>、<scheme>://localhost:<port>
//     以及监听主机自身（当它既不是 127.0.0.1 也不是 localhost 时，例如 ::1）。
//   - 配置为空且监听地址非回环 → 返回错误。
//
// https 参数决定推导出来的 scheme：开了面板 TLS，浏览器看到的源就是 https，
// 推导成 http 会让每一次状态变更请求都因为 Origin 不匹配被拒（P10.4）。
//
// 期望值只来自本地配置，**不**从 Host 头或 X-Forwarded-* 推导（CONTRACTS.md 第 2 节）。
func ResolveAllowedOrigins(origins []string, listenAddress string, https bool) ([]string, error) {
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

	scheme := "http"
	if https {
		scheme = "https"
	}
	hosts := []string{"127.0.0.1", "localhost"}
	if h := strings.Trim(host, "[]"); h != "" && h != "127.0.0.1" && !strings.EqualFold(h, "localhost") {
		hosts = append(hosts, h)
	}
	raw := make([]string, 0, len(hosts))
	for _, h := range hosts {
		raw = append(raw, scheme+"://"+net.JoinHostPort(h, port))
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
