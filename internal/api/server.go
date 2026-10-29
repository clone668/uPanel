// Package api 是 Web 服务的 HTTP 入口：路由组织、中间件与 DTO。
//
// 框架按 PLAN.md 第 3 节固定为 Gin。P01.2 建立路由分组、request_id、访问日志、panic
// 恢复、请求体上限与统一的 404/405 行为；认证、CSRF、DTO 校验与业务路由按阶段追加，
// 本包不预置它们的“空实现”。
package api

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"upanel/internal/alerts"
	"upanel/internal/appruntime"
	"upanel/internal/apps"
	"upanel/internal/audit"
	"upanel/internal/auth"
	"upanel/internal/certificates"
	"upanel/internal/config"
	"upanel/internal/httpx"
	"upanel/internal/notifications"
	"upanel/internal/operations"
	"upanel/internal/platform"
	"upanel/internal/proxies"
	"upanel/internal/version"
)

// MaxRequestBodyBytes 是所有 JSON 请求体的默认上限。
//
// CONTRACTS.md 第 1 节要求按动作分别限制；在具体动作落地前先用一个全局上限兜底，
// 避免任何路由接受无界请求体。
const MaxRequestBodyBytes int64 = 1 << 20 // 1 MiB

// MaxPreflightBodyBytes 是预检请求体的上限。
//
// CONTRACTS.md 第 567 行：请求体上限按动作独立设置，file.write_text 的参数携带
// 最多 1 MiB 内容，传输层因此放宽到 2 MiB（其余动作仍是 64 KiB，由代理的
// action.ParamsLimitBytes 执行）。这里比 2 MiB 多留 8 KiB 给 action、
// actor_context 与 JSON 转义——信封本身也要占地方。
const MaxPreflightBodyBytes int64 = 2<<20 + 8<<10

// Options 是构造 Web 服务的参数。
type Options struct {
	// ListenAddress 为最终监听地址；由 config.ResolveListen 计算。
	ListenAddress string
	// Mode 为 production 或 mock。
	Mode platform.Mode
	// Version 为构建版本信息。
	Version version.Info
	// Logger 必须非空：服务不允许在无日志的情况下运行。
	Logger *slog.Logger
	// ShutdownTimeout 为收到停止信号后等待在途请求的时间。
	ShutdownTimeout time.Duration
	// SchemaVersion 为启动时已迁移到的数据库 schema 版本，用于启动日志。
	// 0 表示尚未引入数据库（测试场景）。
	SchemaVersion int
	// Frontend 为 SPA 静态资源处理器（internal/webui）。为 nil 时非 API 路径也返回 JSON 404，
	// 与 P01.2 的行为一致（用于测试与不嵌入前端的场景）。
	Frontend http.Handler
	// Auth 是认证与会话服务。P02.1 起为必需依赖：没有它就无法校验登录、会话与 CSRF。
	Auth *auth.Service
	// AllowedOrigins 是允许发起状态变更请求的源（已由 config.ResolveAllowedOrigins 规范化）。
	// 不能为空：空集合等于拒绝所有浏览器请求。
	AllowedOrigins []string
	// Audit 是追加式审计写入器。P02.2 起为必需依赖：账号变更必须留下审计行。
	Audit *audit.Writer
	// Operations 是预检与任务服务（P03.1）。必需依赖：没有它就没有“先看影响再执行”的入口。
	// 代理不可用属于运行时状态（服务返回 503），不是启动条件。
	Operations *operations.Service
	// Capabilities 是代理侧外部程序探测的读取端（P16.1）。允许为 nil
	// （演示模式不连代理）：那时 GET /system/capabilities 明确回 503，
	// 而不是给一份空清单——空清单会被读成“这台机器什么都没装”。
	Capabilities CapabilitiesReader
	// Monitor 是系统基础资源读取端（P04.1）。允许为 nil（演示模式不连代理）：
	// 那时 GET /system/summary 明确回 503 AGENT_UNAVAILABLE，而不是给一份空摘要。
	Monitor Monitor
	// Storage 是存储只读视图读取端（P04.2）。允许为 nil（不连代理）：
	// 那时 /storage/* 明确回 503 AGENT_UNAVAILABLE，而不是给一份空拓扑。
	Storage StorageReader
	// Metrics 是历史监控读取端（P13.1）。允许为 nil：演示模式下这个进程不记历史，
	// 那时 /system/metrics 回 200 + enabled=false（说清“未开启”），而不是空曲线——
	// 空曲线会被读成“这段时间没有数据”。
	Metrics MetricsReader
	// Services 是服务只读视图读取端（P05.1）。允许为 nil（不连代理）：
	// 那时 /services* 明确回 503 AGENT_UNAVAILABLE，而不是给一份空服务列表。
	Services ServicesReader
	// Processes 是进程只读视图读取端（P05.2）。允许为 nil（不连代理）：
	// 那时 /processes 明确回 503 AGENT_UNAVAILABLE，而不是给一份空进程列表。
	Processes ProcessesReader
	// Logs 是日志读取端（P05.3）。允许为 nil（不连代理）：
	// 那时 /logs 与 /logs/stream 明确回 503 AGENT_UNAVAILABLE，
	// 而不是给一个“这段时间没有日志”的空结果。
	Logs LogsReader
	// Files 是文件读取端（P06.1）。允许为 nil（不连代理）：
	// 那时 /files/* 明确回 503 AGENT_UNAVAILABLE，而不是给一个空目录——
	// 空目录会被当成“这个目录是空的”。
	Files FilesReader
	// Terminal 是受限终端的调用端（P06.3）。允许为 nil（不连代理）：
	// 那时 /terminal/* 明确回 503 AGENT_UNAVAILABLE，而不是给一个空会话列表——
	// 空列表会被当成“现在没有终端在跑”。
	Terminal TerminalReader
	// Shares 是共享只读视图与秘密通道的调用端（P08.1）。允许为 nil（不连代理）：
	// 那时 /shares*、/share-accounts 与 /secrets 明确回 503 AGENT_UNAVAILABLE，
	// 而不是给一份空共享列表——空列表会被读成“这台机器上没有共享”。
	Shares SharesReader
	// Accounts 是受管理 Linux 账号的只读视图（P14.5）。允许为 nil（不连代理）：
	// 那时 /accounts 与 /accounts/:id/ssh-keys 明确回 503 AGENT_UNAVAILABLE，
	// 而不是给一份空账号列表——空列表会被读成“这台机器上一个账号都没有”。
	//
	// 与 Shares 分开：账号的判决与执行要 useradd/visudo/sudo 那一整套系统工具，
	// 只装了 Samba 的机器上它不存在，而共享页照样该能看。
	Accounts AccountsReader
	// Totp 是二次验证的代理调用端（P14.6）。允许为 nil（不连代理的演示模式）：
	// 那时三条 /auth/totp* 路由明确回 503 AGENT_UNAVAILABLE。**已经启用了二次验证的
	// 账号在 Totp 为 nil 时登录一律失败关闭**（见 verifySecondFactor）：放行等于
	// "把代理弄坏就能绕过二次验证"。
	Totp TotpAgent
	// Apps 是应用与分组元数据服务（P09.1）。必需依赖：它是纯数据库元数据，
	// 不连代理也能工作；缺了它 /apps 与 /app-groups 就没有落脚点，
	// 而“回 503 的空实现”会让人以为功能已经做好了。
	Apps *apps.Service
	// Discovery 是应用发现与健康探测的读取端（P09.2）。允许为 nil（演示模式不连代理）：
	// 那时 /apps/discovery 明确回 503 AGENT_UNAVAILABLE，而不是给一份空候选列表——
	// 空列表会被读成“这台机器上没有任何服务在监听”。
	Discovery DiscoveryReader
	// Containers 是容器、镜像、网络、卷与部署的读取端（P09.3）。允许为 nil
	// （演示模式不连代理）：那时 /containers*、/images、/container-networks、
	// /container-volumes、/deployments* 明确回 503 AGENT_UNAVAILABLE，
	// 而不是给一份空容器清单——空清单会被读成“这台机器上没有容器在跑”。
	Containers ContainersReader
	// Deployments 是**管理面**的部署读取端（P15.4）：面板管着哪些 Compose 部署、
	// 它们现在是什么状态（配置在不在、记录里钉的是哪个镜像、单元 enabled/active）。
	//
	// 与上面的 Containers 分开一个字段，因为它们回答的不是同一个问题：Containers
	// 那条读 docker（这台机器上跑着什么），这条读配置目录与 systemd（面板管着什么）。
	// 允许为 nil（演示模式不连代理）：那时 /deployments/managed* 回 503
	// AGENT_UNAVAILABLE，而不是给一份空列表——空列表会被读成"这台机器上还没有部署"。
	Deployments DeploymentsReader
	// AppRuntime 把应用元数据与主机运行事实拼成 AppSummary（P09.4）。允许为 nil
	// （演示模式不连代理）：那时每条应用的 runtime_state 都是 unknown、能力全为
	// false，并写明“未配置代理连接”。/apps 本身是纯数据库元数据，不会因为代理
	// 不在就变成 503——那会把“这台机器上有哪些入口”一起弄丢。
	AppRuntime *appruntime.Resolver
	// Proxies 是代理站点元数据服务（P10.1）。必需依赖：站点定义存在面板库里，
	// 读列表与详情不依赖代理。
	Proxies *proxies.Service // NginxFacts 读取主机 Nginx 现状（P10.1）。允许为 nil（演示模式不连代理）：
	// 那时 /proxies 仍返回站点定义，nginx 那一块写明 ReadError；/proxies/nginx
	// 与 /proxies/conflict 明确回 503，而不是给一份“没有配置文件”的空表。
	NginxFacts NginxFactsReader
	// PanelPort 是面板自己真实监听的端口（P10.2）。
	//
	// 它只用于一件事：展开 proxy.apply 的参数时带上 panel_port，让代理能在执行层
	// 再拒绝一次"把管理面板自己的端口写成上游"。值来自真实的监听地址，不是常量
	// 9443——面板端口可配，写死的那个数在某台机器上会正好是别的服务的端口。
	PanelPort int
	// Certificates 是证书元数据服务（P10.3）。必需依赖，理由与 Proxies 相同：
	// 证书记录是纯面板库里的元数据，读列表与详情不需要代理。
	//
	// 注意它与"签发/导入"的分工：本服务只回答**记录是什么**；证书能不能装上去、
	// 私钥怎么写进主机，全是代理侧的动作（certificate.import / certificate.issue），
	// 面板只负责提交任务。
	Certificates *certificates.Service
	// CertificateStages 是待导入公钥证书的暂存区（P10.3）。必需依赖：
	// POST /certificates/uploads 只把上传的 PEM 暂存在本进程内存里十分钟
	// （绑定用户 + 会话），导入任务执行时再展开进参数。
	//
	// 为什么不直接落库：公钥证书是公开材料，但也**不需要**长期保存——真正的落库
	// 时机是导入成功之后（那时证书已经从主机上读回来解析过一次）。暂存区让
	// "上传了但没点确认"不留任何持久痕迹。
	CertificateStages *certificates.StageStore
	// Packages 是软件包事实的判断层（P11.1）。允许为 nil（演示模式不连代理）：
	// 那时三个只读接口明确回 503 AGENT_UNAVAILABLE，而不是给一份空包列表——
	// 空列表会被读成"这台机器上一个包都没装"，那是假话。
	Packages PackagesReader
	// CertificateState 读取主机上证书的现场事实（P10.3）。允许为 nil（演示模式不连
	// 代理）：那时详情页的 observed 明确写 available=false 与原因，而不是给一份
	// "证书不存在"的空事实——后者会被读成"这张证书被删了"。
	CertificateState CertificateStateReader
	// Backup 是备份定义镜像与快照查询的调用端（P12.2）。允许为 nil（演示模式不连代理）：
	// 那时 /backup/* 明确回 503 AGENT_UNAVAILABLE，而不是给一份空列表——空列表会被
	// 读成"这台机器上还没有配置备份"。
	//
	// 与 Monitor/Storage 那一批不同，这里**没有**"回落到本地副本"这条路：备份定义
	// 只有代理那一份（见 internal/agentstore/backupstore.go），面板不存第二份。
	Backup BackupManager
	// Schedules / Scripts 是计划任务与受管理脚本镜像的读取端（P12.4）。允许为 nil
	// （演示模式不连代理）：那时 /schedules* 与 /scripts* 明确回 503
	// AGENT_UNAVAILABLE，而不是给一份空列表——空列表会被读成"这台机器上没有定时任务"。
	//
	// 分成两个字段而不是合成一个：代理可以只启用其中一个镜像（见 internal/agent
	// 的 Schedules/Scripts 选项），装配不谎报能力。生产里两个都指向同一个
	// *agentclient.Client。
	Schedules SchedulesReader
	Scripts   ScriptsReader
	// TLS 是面板 HTTPS 材料（P10.4）。为 nil 表示不开 TLS——只有回环监听允许这样
	// （config.CheckTransport 判定），本服务不再自己重复判断。
	//
	// 材料在 New 里就已经加载并校验过（证书能配私钥、没过期），因此 Serve 阶段的
	// 失败只可能是监听本身的问题，不会出现“服务起来了但证书是坏的”。
	TLS *config.PanelTLS
	// Alerts 是告警规则、事件与维护静默服务（P13.2）。允许为 nil（测试或未接数据库
	// 的进程）：那时 /alerts/* 明确回 503 NOT_IMPLEMENTED 并说明“这个进程没有装配
	// 告警”，而不是给一份空规则列表——空列表会被读成“这台机器上没配任何告警”，
	// 那会让一次真实故障看起来像是“没人配规则”而不是“功能没开”。
	//
	// 生产里它是必需依赖：告警的判定、事件与静默都在面板库里，没有代理也能工作。
	Alerts *alerts.Service
	// Notifications 是通知通道与投递账（P13.3）。同样允许为 nil，理由与 Alerts 一致：
	// 未装配时 /notification-channels 明确说“这个进程没有装配通知”，而不是给一份
	// 空通道列表——空列表会被读成“这台机器上一条通知都没配”。
	//
	// 注意它**不包含发送端**：真正发信的是代理（internal/agent/notify.go），
	// 面板这边只知道“发到哪、说什么”。演示模式下服务在，发送端不在，投递会停在
	// pending 而不是伪装成失败。
	Notifications *notifications.Service
	// Network 是网络与防火墙只读查询的读取端（P14.1）。允许为 nil（演示模式不连代理）：
	// 那时 /network/interfaces、/network/routes、/network/listeners 与 /firewall
	// 明确回 503 AGENT_UNAVAILABLE，而不是给一份空网卡列表或空规则表——空列表会被
	// 读成"这台机器没有网卡""防火墙没有任何规则"，两种假话都比 503 糟。
	//
	// 这里**没有**"本地降级"这条路：网卡、路由、监听端口与防火墙规则只存在于代理
	// 那一侧（/proc、/sys、ufw 的实时状态），面板不镜像它们。
	Network NetworkReader
}

// Server 是 Web HTTP 服务。
type Server struct {
	opts           Options
	startedAt      time.Time
	engine         *gin.Engine
	http           *http.Server
	allowedOrigins *httpx.OriginSet
	// streams 限制单个用户同时打开的事件流数量（P03.3）。
	streams *streamLimiter
	// terminalGrants 是终端连接许可（P06.3）：一次性、30 秒、只存在于本进程内存。
	terminalGrants *terminalGrants
	// addrMu 保护 addr：Serve 在监听成功后写入，Addr 可能从测试或运维脚本里随时读。
	addrMu sync.RWMutex
	addr   string
}

// Addr 返回 Serve 实际监听的地址（监听地址写成 :0 时由内核分配，只有这里知道结果）。
// 尚未开始监听时返回空串。
func (s *Server) Addr() string {
	s.addrMu.RLock()
	defer s.addrMu.RUnlock()
	return s.addr
}

// healthLive 是 GET /api/v1/health/live 的响应体。
// CONTRACTS.md 第 3 节：最小存活状态，不返回内部路径或版本细节。
type healthLive struct {
	Status string `json:"status"`
}

// New 构造服务实例。参数不合法时返回错误，调用方必须拒绝启动。
func New(opts Options) (*Server, error) {
	if opts.Logger == nil {
		return nil, errors.New("api: Logger 不能为空")
	}
	if !opts.Mode.Valid() {
		return nil, fmt.Errorf("api: 未知运行模式 %q", string(opts.Mode))
	}
	if opts.ListenAddress == "" {
		return nil, errors.New("api: ListenAddress 不能为空")
	}
	if opts.Mode == platform.ModeMock && opts.ListenAddress != config.ListenMock {
		return nil, fmt.Errorf("api: mock 模式只能监听 %s，当前为 %s", config.ListenMock, opts.ListenAddress)
	}
	if opts.ShutdownTimeout <= 0 {
		return nil, errors.New("api: ShutdownTimeout 必须为正数")
	}
	if opts.Auth == nil {
		return nil, errors.New("api: Auth 不能为空")
	}
	if opts.Audit == nil {
		return nil, errors.New("api: Audit 不能为空")
	}
	if opts.Operations == nil {
		return nil, errors.New("api: Operations 不能为空")
	}
	if opts.Apps == nil {
		return nil, errors.New("api: Apps 不能为空")
	}
	if opts.Proxies == nil {
		return nil, errors.New("api: Proxies 不能为空")
	}
	if opts.Certificates == nil {
		return nil, errors.New("api: Certificates 不能为空")
	}
	if opts.CertificateStages == nil {
		return nil, errors.New("api: CertificateStages 不能为空")
	}
	origins, err := httpx.NewOriginSet(opts.AllowedOrigins)
	if err != nil {
		return nil, fmt.Errorf("api: AllowedOrigins 不合法: %w", err)
	}
	if origins.Empty() {
		return nil, errors.New("api: AllowedOrigins 不能为空")
	}

	s := &Server{
		opts:           opts,
		startedAt:      time.Now().UTC(),
		allowedOrigins: origins,
		streams:        newStreamLimiter(),
		terminalGrants: newTerminalGrants(),
	}
	s.engine = s.routes()
	s.http = &http.Server{
		Handler:           s.engine,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		// 写超时必须**大于一次认证校验的最坏耗时**。
		//
		// 登录要算 Argon2id（默认 128 MiB、3 轮、2 路），在开发宿主上约 110 ms，
		// 在单核慢机器上实测一次要 22—30 秒。原来的 30 秒写超时正好卡在这条线上：
		// handler 算完、状态码已经定了，但响应写不出去，net/http 直接把连接关掉，
		// 客户端只看到"连接被断开"（curl 拿到 000），连"密码错了"都读不到——
		// 一次慢机器上的正常登录被表现成连接故障。
		//
		// 2 分钟是按"最慢实测 30 秒"的 4 倍留的余量；长任务（备份、恢复）本来就走
		// 任务队列，不占着这个连接。
		WriteTimeout:   2 * time.Minute,
		IdleTimeout:    120 * time.Second,
		MaxHeaderBytes: 1 << 20,
		ErrorLog:       slog.NewLogLogger(opts.Logger.Handler(), slog.LevelWarn),
	}
	return s, nil
}

// Handler 返回路由，供测试与嵌入式使用。
func (s *Server) Handler() http.Handler { return s.engine }

// Serve 监听端口并阻塞服务，直到 ctx 结束或监听失败。
//
// 监听在进入 Serve 之前显式完成，端口被占用等问题会作为启动错误返回，
// 不会被当成“服务已就绪”。
//
// 开了 TLS 时先监听、再套 TLS：这样“端口被占用”仍然以监听错误的形式
// 在同一个地方出现，而不是被埋进第一次握手里。
func (s *Server) Serve(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.opts.ListenAddress)
	if err != nil {
		return fmt.Errorf("监听 %s 失败: %w", s.opts.ListenAddress, err)
	}

	scheme := "http"
	if s.opts.TLS != nil {
		ln = tls.NewListener(ln, s.opts.TLS.Config)
		scheme = "https"
		for _, warning := range s.opts.TLS.Warnings {
			s.opts.Logger.Warn("面板 HTTPS 材料注意", "detail", warning)
		}
		s.opts.Logger.Info("面板 HTTPS 已就绪",
			"cert", s.opts.TLS.Info.Subject,
			"fingerprint_sha256", s.opts.TLS.Info.Fingerprint,
			"not_after", s.opts.TLS.Info.NotAfter.Format(time.RFC3339),
			"min_version", "TLS 1.2")
	}

	s.addrMu.Lock()
	s.addr = ln.Addr().String()
	s.addrMu.Unlock()

	loopback := config.IsLoopbackAddress(ln.Addr().String())
	if !loopback {
		s.opts.Logger.Warn("监听地址不是回环地址，面板将对该网络可达",
			"address", ln.Addr().String(),
			"hint", "默认应为 127.0.0.1，并配合 SSH 隧道访问")
	}

	s.opts.Logger.Info("Web 服务已开始监听",
		"address", ln.Addr().String(),
		"network", ln.Addr().Network(),
		"scheme", scheme,
		"mode", s.opts.Mode.String(),
		"loopback_only", loopback,
		"version", s.opts.Version.Version,
		"schema_version", s.opts.SchemaVersion,
		"pid", os.Getpid(),
		"started_at", s.startedAt)

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		s.opts.Logger.Info("收到停止信号，开始优雅停止", "timeout", s.opts.ShutdownTimeout.String())
		shutdownCtx, cancel := context.WithTimeout(context.Background(), s.opts.ShutdownTimeout)
		defer cancel()
		if err := s.http.Shutdown(shutdownCtx); err != nil {
			s.opts.Logger.Error("优雅停止未能在超时内完成", "error", err)
			_ = s.http.Close()
		}
	}()

	err = s.http.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		<-shutdownDone
		s.opts.Logger.Info("Web 服务已停止")
		return nil
	}
	return fmt.Errorf("HTTP 服务异常退出: %w", err)
}

// writeError 是包内统一的错误写出入口，保证状态码与错误码一致。
func writeError(c *gin.Context, status int, code httpx.Code, message string) {
	httpx.WriteError(c.Writer, c.Request, status, code, message)
}
