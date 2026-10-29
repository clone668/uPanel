// upanel-web 是 uPanel 的 Web 服务入口。
//
// 权限边界（PLAN.md 第 4 节）：本项目要求该进程以 upanel-web 身份运行，无 sudo、
// 不加入 docker 组、不提供登录 shell。P01.1 不实现认证与系统操作，因此本入口只做
// 配置解析、日志、数据库迁移、认证服务与 HTTP 服务。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"upanel/internal/accounts"
	"upanel/internal/agentclient"
	"upanel/internal/alerts"
	"upanel/internal/api"
	"upanel/internal/apphealth"
	"upanel/internal/appruntime"
	"upanel/internal/apps"
	"upanel/internal/audit"
	"upanel/internal/auth"
	"upanel/internal/certificates"
	"upanel/internal/config"
	"upanel/internal/containers"
	"upanel/internal/discovery"
	"upanel/internal/files"
	"upanel/internal/logging"
	"upanel/internal/logs"
	"upanel/internal/metrics"
	"upanel/internal/monitoring"
	"upanel/internal/notifications"
	"upanel/internal/operations"
	"upanel/internal/packages"
	"upanel/internal/platform"
	"upanel/internal/processes"
	"upanel/internal/proxies"
	"upanel/internal/services"
	"upanel/internal/shares"
	"upanel/internal/storage"
	"upanel/internal/store"
	"upanel/internal/terminal"
	"upanel/internal/version"
	"upanel/internal/webui"
	"upanel/migrations"
)

const program = "upanel-web"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet(program, flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		configPath  = fs.String("config", "", "配置文件路径（JSON）；缺省读取平台默认路径")
		modeFlag    = fs.String("mode", "", "运行模式：production 或 mock（覆盖配置文件）")
		listenFlag  = fs.String("listen", "", "监听地址 host:port（覆盖配置文件）")
		dbFlag      = fs.String("db", "", "Web 数据库文件绝对路径（覆盖 database.path）")
		logLevel    = fs.String("log-level", "", "日志级别：debug、info、warn、error")
		logFormat   = fs.String("log-format", "", "日志格式：json 或 text")
		showVersion = fs.Bool("version", false, "输出版本信息并退出")
	)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "%s — uPanel Web 服务\n\n用法: %s [参数]\n\n参数:\n", program, program)
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "错误: 不接受位置参数: %s\n", strings.Join(fs.Args(), " "))
		return 2
	}
	if *showVersion {
		fmt.Fprintf(stdout, "%s %s\n", program, version.Get().String())
		return 0
	}

	res, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "配置错误: %v\n", err)
		return 2
	}
	cfg := res.Config

	if *modeFlag != "" {
		cfg.Web.Mode = *modeFlag
	}
	listenExplicit := res.ListenExplicit
	if *listenFlag != "" {
		cfg.Web.ListenAddress = *listenFlag
		listenExplicit = true
	}
	if *logLevel != "" {
		cfg.Log.Level = *logLevel
	}
	if *logFormat != "" {
		cfg.Log.Format = *logFormat
	}

	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(stderr, "配置错误: %v\n", err)
		return 2
	}

	mode, err := cfg.Web.ModeValue()
	if err != nil {
		fmt.Fprintf(stderr, "配置错误: web.mode: %v\n", err)
		return 2
	}
	listenAddr, err := config.ResolveListen(mode, cfg.Web.ListenAddress, listenExplicit)
	if err != nil {
		fmt.Fprintf(stderr, "配置错误: web.listen_address: %v\n", err)
		return 2
	}

	// 面板传输层（P10.4）：监听地址与 TLS 配置这个组合能不能接受，由
	// config.CheckTransport 一处判定 —— CLI 的 check-config 用的是同一个函数，
	// 因此“CLI 说行、服务不启动”这种分裂不会出现。
	//
	// 曾经这里有一条“TLS 接入之前 production 只能绑回环”的临时保护：TLS 已经接进来了，
	// 那条判断不但多余，还会把一份**合法**的非回环 + TLS 配置挡在一个过时的理由后面。
	// 现在提前到这里，是为了在打开数据库之前就把配置拒绝掉（配置错误不该有副作用）。
	var panelTLS *config.PanelTLS
	if cfg.Web.TLSConfigured() {
		panelTLS, err = cfg.Web.LoadTLS()
		if err != nil {
			fmt.Fprintf(stderr, "配置错误: %v\n", err)
			return 2
		}
	}
	if err := config.CheckTransport(listenAddr, cfg.Web.TLSConfigured()); err != nil {
		fmt.Fprintf(stderr, "配置错误: %v\n", err)
		return 2
	}

	// 平台准入：production 在非 Linux 上必须明确拒绝，不能自动退回 mock。
	if err := platform.CheckMode(mode); err != nil {
		fmt.Fprintf(stderr, "拒绝启动: %v\n", err)
		return 1
	}

	dbPath := cfg.Database.Path
	if *dbFlag != "" {
		dbPath = *dbFlag
		if !filepath.IsAbs(dbPath) {
			fmt.Fprintf(stderr, "配置错误: --db 必须是绝对路径，当前为 %q\n", dbPath)
			return 2
		}
	}
	if dbPath == "" {
		fmt.Fprintf(stderr, "配置错误: 未配置数据库路径：本平台没有默认位置，请用 --db 或 database.path 指定\n")
		return 2
	}

	logger, err := logging.New(logging.Options{
		Program:   program,
		Level:     cfg.Log.Level,
		Format:    cfg.Log.Format,
		AddSource: cfg.Log.AddSource,
		Output:    stderr,
	})
	if err != nil {
		fmt.Fprintf(stderr, "日志初始化失败: %v\n", err)
		return 2
	}

	source := res.Source
	if source == "" {
		source = "内置默认值"
	}
	plat := platform.Current()
	logger.Info("启动",
		"version", version.Get().Version,
		"commit", version.Get().Commit,
		"go_version", version.Get().GoVersion,
		"mode", mode.String(),
		"os", plat.OS,
		"arch", plat.Arch,
		"config_source", source,
	)
	if mode == platform.ModeMock {
		logger.Warn("演示模式：使用进程内演示数据，不读取真实系统，也不写入服务器",
			"address", config.ListenMock)
	}

	// 数据库是硬依赖：打不开或迁移失败都必须拒绝启动，不能带着半个 schema 提供服务。
	db, err := store.Open(context.Background(), store.Options{
		Path:        dbPath,
		BusyTimeout: cfg.Database.BusyTimeout(),
		Logger:      logger,
	})
	if err != nil {
		logger.Error("数据库打开失败，拒绝启动", "error", err, "path", dbPath)
		return 1
	}
	defer func() {
		if err := db.Close(); err != nil {
			logger.Error("关闭数据库失败", "error", err)
		}
	}()

	migrateResult, err := db.Migrate(context.Background(), migrations.FS)
	if err != nil {
		logger.Error("数据库迁移失败，拒绝启动", "error", err, "path", dbPath)
		return 1
	}
	logger.Info("数据库 schema 已就绪",
		"path", dbPath,
		"schema_version", migrateResult.To,
		"applied_versions", migrateResult.AppliedVersions(),
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	authService, err := auth.NewService(db, auth.Options{})
	if err != nil {
		logger.Error("初始化认证服务失败，拒绝启动", "error", err)
		return 1
	}
	auditWriter, err := audit.New(db, nil)
	if err != nil {
		logger.Error("初始化审计写入器失败，拒绝启动", "error", err)
		return 1
	}
	// 通知（P13.3）：通道定义、投递账与调度都在面板库里；**发送由代理执行**，
	// 因为 SMTP 口令与 webhook 令牌只存在代理的加密库里，而这里刻意没有取回明文的
	// 接口。所以这个服务必须先于告警服务创建——告警在触发/恢复的同一笔事务里
	// 入队投递，靠的就是它的 Notifier 身份。
	//
	// 发送端在生产模式下稍后接上（见下面的 SetSender）：演示模式没有代理，
	// 投递会停在 pending 并把原因说清楚，而不是“假装发过了”。
	notifService, err := notifications.New(db, notifications.Options{Logger: logger})
	if err != nil {
		logger.Error("初始化通知服务失败，拒绝启动", "error", err)
		return 1
	}
	// 告警（P13.2）：规则、事件与维护静默全在面板库里，判定只读历史样本表，
	// 不经过代理——代理挂了正是最需要告警的时候。
	//
	// 评估循环与采集循环共用同一个 ctx：进程收到 SIGTERM 时它跟着停，不会在
	// 优雅停止之后继续往库里写状态。
	alertsService, err := alerts.New(db, alerts.Options{Logger: logger, Notifier: notifService})
	if err != nil {
		logger.Error("初始化告警服务失败，拒绝启动", "error", err)
		return 1
	}
	go alertsService.Run(ctx, alerts.DefaultInterval)
	// 启动时清掉过期会话；之后每 15 分钟一次。清理失败不阻止启动（会话本身仍会按超时判定），
	// 但要留下记录。
	if removed, err := authService.PurgeExpired(ctx); err != nil {
		logger.Warn("启动时清理过期会话失败", "error", err)
	} else if removed > 0 {
		logger.Info("已清理过期会话", "count", removed)
	}
	go func() {
		ticker := time.NewTicker(15 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := authService.PurgeExpired(ctx); err != nil && ctx.Err() == nil {
					logger.Warn("定期清理过期会话失败", "error", err)
				}
			}
		}
	}()

	// 没有账号时明确提示：系统不提供默认密码，第一个管理员只能用本机 CLI 创建。
	if n, err := authService.CountUsers(ctx); err != nil {
		logger.Error("读取账号数量失败", "error", err)
	} else if n == 0 {
		logger.Warn("尚未创建任何账号，登录不可用",
			"hint", "请在服务器本机运行 upanelctl init-admin 创建第一个管理员")
	}

	// 面板 HTTPS（P10.4）：证书已经在上面（打开数据库之前）加载过了，这里只留
	// “没启用 TLS”的那句告警——它是回环 + SSH 隧道部署下唯一允许的形态。
	if panelTLS == nil {
		logger.Warn("面板未启用 HTTPS",
			"listen", listenAddr,
			"hint", "只允许回环监听 + SSH 隧道这一种部署方式；要绑管理网地址必须配置 web.tls_cert_path 与 web.tls_key_path")
	}

	// 允许源只来自本机配置；不由 Host 头或 X-Forwarded-* 推导（CONTRACTS.md 第 2 节）。
	// 开了 TLS 时推导出来的源就是 https://…，否则浏览器每次改状态都会因 Origin 不符被拒。
	allowedOrigins, err := config.ResolveAllowedOrigins(cfg.Web.AllowedOrigins, listenAddr, panelTLS != nil)
	if err != nil {
		fmt.Fprintf(stderr, "配置错误: %v\n", err)
		return 2
	}
	logger.Info("已确定允许的请求源", "allowed_origins", allowedOrigins)

	// 前端资源随二进制发布（PLAN.md 第 3 节：生产机不装 Node.js）。
	// 没有产物时明确告警并按请求返回 503，不返回看起来正常的空页面。
	assets := webui.Embedded()
	if assets.Available() {
		logger.Info("前端资源已就绪", "source", "嵌入二进制")
	} else {
		logger.Warn("二进制未嵌入前端资源，页面请求将返回 503", "reason", assets.Reason())
	}
	frontend, err := webui.NewHandler(assets, webui.Options{Logger: logger, Mode: mode.String()})
	if err != nil {
		logger.Error("初始化前端静态资源失败，拒绝启动", "error", err)
		return 1
	}

	// 特权代理客户端（P03.1 起预检必须由代理生成）。
	// mock 模式不连接代理：演示数据在进程内产生，与真实代理完全隔离。
	// opsAgent 是"慢操作"客户端：预检与提交都要跑一次动作的 Plan，软件包动作的
	// Plan 是一条 `apt-get -s`（24.04 上实测最长 38.7 秒）。名字刻意不叫
	// preflightAgent——它同时承担 POST /jobs 的提交，只按预检命名会让人以为提交
	// 走的是另一条（10 秒上限的）链路。
	var opsAgent operations.Agent
	// 系统基础资源读取端（P04.1）。演示模式下保持 nil：接口回 503，不造假数据。
	var monitor api.Monitor
	// 存储只读视图读取端（P04.2）。演示模式下同样保持 nil。
	var storageReader api.StorageReader
	// 历史监控读取端（P13.1）。演示模式下保持 nil：那时 /system/metrics 回
	// enabled=false 并说明"演示模式不采集历史"，而不是一条看起来正常的空曲线。
	var metricsReader api.MetricsReader
	// 服务只读视图读取端（P05.1）。演示模式下同样保持 nil。
	var servicesReader api.ServicesReader
	// 进程只读视图读取端（P05.2）。演示模式下同样保持 nil。
	var processesReader api.ProcessesReader
	// 日志读取端（P05.3）。演示模式下同样保持 nil：那时 /logs 明确回 503。
	var logsReader api.LogsReader
	// 文件读取端（P06.1）。演示模式下同样保持 nil：那时 /files/* 明确回 503，
	// 而不是给一个空目录列表——空目录会被当成“这个目录是空的”。
	var filesReader api.FilesReader
	// 终端调用端（P06.3）。演示模式下同样保持 nil：那时 /terminal/* 明确回 503，
	// 而不是给一个空会话列表——空列表会被当成“现在没有终端在跑”。
	var terminalReader api.TerminalReader
	// 共享只读视图与秘密通道（P08.1）。演示模式下同样保持 nil：那时 /shares*、
	// /share-accounts 与 /secrets 明确回 503，而不是给一份空共享列表——
	// 空列表会被读成“这台机器上没有共享”。
	var sharesReader api.SharesReader
	// 受管理 Linux 账号（P14.5）。演示模式下保持 nil：那时 /accounts 与
	// /accounts/:id/ssh-keys 明确回 503，而不是给一份空账号列表——
	// 空列表会被读成“这台机器上一个账号都没有”。
	var accountsReader api.AccountsReader
	// 二次验证的代理调用端（P14.6）。演示模式下保持 nil：那时 /auth/totp/* 明确回
	// 503，而不是给一张"看起来能用"的二维码——那张 QR 后面的种子根本不在任何代理里。
	var totpAgent api.TotpAgent
	// 应用发现与健康探测读取端（P09.2）。演示模式下保持 nil：那时 /apps/discovery
	// 明确回 503，而不是给一份空候选列表——空列表会被读成“没有一个服务在监听”。
	var discoveryReader api.DiscoveryReader
	// 容器、镜像、网络、卷与部署读取端（P09.3）。演示模式下保持 nil：那时
	// /containers*、/images、/container-networks、/container-volumes、/deployments*
	// 明确回 503，而不是给一份空容器清单——空清单会被读成“没有容器在跑”。
	var containersReader api.ContainersReader
	// Nginx 现状读取端（P10.1）。演示模式下保持 nil：那时 /proxies 仍返回站点定义，
	// nginx 那一块写明读不到；/proxies/nginx 与 /proxies/conflict 明确回 503，
	// 而不是给一份“主机上没有配置文件”的空表。
	var nginxFacts *proxies.AgentFacts
	// 软件包事实的判断层（P11.1）。演示模式下保持 nil：那时 /packages、
	// /packages/updates 与 /system/power-state 明确回 503，而不是给一份空包列表
	// ——空列表会被读成"这台机器上一个包都没装"。
	var packagesReader api.PackagesReader
	// 备份管理（P12.2）。演示模式下保持 nil：那时 /backup/* 明确回 503，
	// 而不是给一份空列表——空列表会被读成"这台机器上还没有配置备份"。
	//
	// 这里没有"本地副本"可用：备份定义只有代理那一份（见 internal/agentstore/
	// backupstore.go），所以代理不在时备份管理不是"只读可用"，而是读不了也改不了。
	var backupManager api.BackupManager
	// 计划任务与受管理脚本（P12.4）：与备份同一条规矩，只有代理那一份。
	// 演示模式下保持 nil，四个只读接口明确回 503，而不是给一份空列表。
	//
	// 用通用客户端（10 秒上限）就够：这两个接口读的是代理自己的 SQLite 与几条
	// systemctl 事实，不碰 restic/SFTP，没有"冷采一次几十秒"的问题。
	var schedulesReader api.SchedulesReader
	var scriptsReader api.ScriptsReader
	// 网络与防火墙的只读读取端（P14.1）。演示模式下保持 nil：那时
	// /network/* 与 /firewall 明确回 503，而不是给一份空网卡列表或空规则表
	// ——空表会被读成"这台机器没有网卡""防火墙没有任何规则"。
	var networkReader api.NetworkReader
	// 部署管理面的读取端（P15.4）。演示模式下保持 nil：/api/v1/deployments/managed*
	// 明确回 503，而不是给一份空列表——空列表会被读成"这台机器上还没有部署"，
	// 而真相是"面板根本没有连代理，什么都不知道"。
	//
	// 与 Containers（containersReader）是**两个读取端**：那条读 docker 回答"这台
	// 机器上跑着什么"，这条读 /etc/upanel/deploy 与 systemd 回答"面板管着什么"。
	var deploymentsReader api.DeploymentsReader

	// 外部程序能力（P16.1）。演示模式下保持 nil：/api/v1/system/capabilities
	// 明确回 503，而不是一份空清单——空清单会说“这台机器什么都没装”。
	//
	// 它读的是代理的缓存（代理启动后后台探一次、之后每十分钟重探），这条接口
	// 不会在请求里跑任何命令。
	var capabilitiesReader api.CapabilitiesReader
	// 证书现场事实（只读，P10.3）：只有 root 读得到 /etc/letsencrypt 与
	// /etc/ssl/upanel。演示模式下保持 nil，详情页的 observed 会写"读不到"，
	// 而不是给出"这张证书不存在"。
	var certificateState api.CertificateStateReader
	// 健康检查目标的存在性检查（P09.2 的写入期约束）与探测执行端。
	// 这两个都只在生产模式接上：演示模式没有代理，写入带健康检查的应用会明确回 503，
	// 而不是存下一个面板根本探不到的目标。
	var healthTargetChecker apps.HealthTargetChecker
	var healthProber apphealth.Prober
	if mode == platform.ModeProduction {
		client, err := agentclient.New(cfg.Agent.SocketPath)
		if err != nil {
			logger.Error("初始化代理客户端失败，拒绝启动",
				"error", err, "socket", cfg.Agent.SocketPath)
			return 1
		}
		// 通知发送端（P13.3）：投递交给代理执行。用通用客户端就够——代理自己把
		// 一次投递限在 8 秒内（见 internal/agent/notify.go），比这里的 10 秒短，
		// 所以永远轮不到面板先把代理掐掉。
		//
		// 调度循环与投递使用同一个 ctx：SIGTERM 之后不再开新的一轮。
		notifService.SetSender(notifications.NewAgentSender(client))
		go notifService.Run(ctx, 0)
		// 软件包事实单独一个客户端，只多一件事：更长的调用上限。
		// `apt-cache policy` 在 24.04 上冷采一次 26 秒（量见 packages.AgentQueryTimeout），
		// 10 秒的通用上限会把一次正常的冷采变成"代理不可用"。
		pkgClient, err := agentclient.New(cfg.Agent.SocketPath,
			agentclient.WithTimeout(packages.AgentQueryTimeout))
		if err != nil {
			logger.Error("初始化软件包代理客户端失败，拒绝启动",
				"error", err, "socket", cfg.Agent.SocketPath)
			return 1
		}
		// 预检与提交共用一个客户端，只多一件事：更长的调用上限。
		// 两条路都要跑动作的 Plan（软件包动作= `apt-get -s`，24.04 上实测 15.6—38.7 秒，
		// 量见 operations.PreflightAgentTimeout）。10 秒的通用上限会把一次正常的预检
		// 或提交变成"代理不可用"——P11.2 验收第一、二轮的原样失败。
		preflightClient, err := agentclient.New(cfg.Agent.SocketPath,
			agentclient.WithTimeout(operations.PreflightAgentTimeout))
		if err != nil {
			logger.Error("初始化预检代理客户端失败，拒绝启动",
				"error", err, "socket", cfg.Agent.SocketPath)
			return 1
		}
		opsAgent = preflightClient
		// 快照列表与详情也单独一个客户端：代理给 restic/SFTP 查询 90 秒，
		// 通用 10 秒上限会先把正常慢查询掐成“代理不可用”。
		backupClient, err := agentclient.New(cfg.Agent.SocketPath,
			agentclient.WithTimeout(agentclient.BackupQueryTimeout))
		if err != nil {
			logger.Error("初始化备份代理客户端失败，拒绝启动",
				"error", err, "socket", cfg.Agent.SocketPath)
			return 1
		}
		fetcher, err := monitoring.NewFetcher(client, logger)
		if err != nil {
			logger.Error("初始化系统信息读取端失败，拒绝启动", "error", err)
			return 1
		}
		monitor = fetcher
		storageFetcher, err := storage.NewFetcher(client, logger)
		if err != nil {
			logger.Error("初始化存储读取端失败，拒绝启动", "error", err)
			return 1
		}
		storageReader = storageFetcher

		// 历史监控（P13.1）：采集仍在代理侧，这里只是把 Web 已经拿到的两份快照变成
		// 时序样本写进面板库（PLAN.md 第 18 节）。存储拓扑既做只读视图也做容量曲线，
		// 用同一个 Fetcher，不新建第二个客户端——多一个客户端就多一处"读到的是不同
		// 时刻的拓扑"的机会。
		//
		// 采集被配置关掉时仍然构造 Reader：旧数据照样查得到，Status 会说明"不再写新的"。
		var recorder *metrics.Recorder
		if cfg.Web.Metrics.Enabled {
			rawRetention, minuteRetention, hourRetention := cfg.Web.Metrics.Retention()
			recorder, err = metrics.NewRecorder(metrics.Options{
				Store:    db,
				Summary:  fetcher,
				Topology: storageFetcher,
				Logger:   logger,
				Interval: cfg.Web.Metrics.Interval(),
				Retention: metrics.Retention{
					Raw:    rawRetention,
					Minute: minuteRetention,
					Hour:   hourRetention,
				},
				MaxRows: int64(cfg.Web.Metrics.MaxRows),
			})
			if err != nil {
				logger.Error("初始化历史监控失败，拒绝启动", "error", err)
				return 1
			}
		}
		reader, err := metrics.NewReader(metrics.ReaderOptions{
			Store:    db,
			Recorder: recorder,
			NoRecorderReason: "配置里关闭了历史采集（web.metrics.enabled=false）：" +
				"旧数据可查，不再写新数据",
		})
		if err != nil {
			logger.Error("初始化历史监控查询端失败，拒绝启动", "error", err)
			return 1
		}
		metricsReader = reader
		if recorder != nil {
			// 采集循环与请求共用同一个 ctx：进程收到 SIGTERM 时它跟着停，
			// 而不是在优雅停止之后继续往库里写。
			go recorder.Run(ctx)
		}

		servicesFetcher, err := services.NewFetcher(client, logger)
		if err != nil {
			logger.Error("初始化服务读取端失败，拒绝启动", "error", err)
			return 1
		}
		servicesReader = servicesFetcher
		processesFetcher, err := processes.NewFetcher(client, logger)
		if err != nil {
			logger.Error("初始化进程读取端失败，拒绝启动", "error", err)
			return 1
		}
		processesReader = processesFetcher
		logsFetcher, err := logs.NewFetcher(client, logger)
		if err != nil {
			logger.Error("初始化日志读取端失败，拒绝启动", "error", err)
			return 1
		}
		logsReader = logsFetcher
		filesFetcher, err := files.NewFetcher(client, logger)
		if err != nil {
			logger.Error("初始化文件读取端失败，拒绝启动", "error", err)
			return 1
		}
		filesReader = filesFetcher
		terminalRemote, err := terminal.NewRemote(client, logger)
		if err != nil {
			logger.Error("初始化终端调用端失败，拒绝启动", "error", err)
			return 1
		}
		terminalReader = terminalRemote
		sharesFetcher, err := shares.NewFetcher(client, logger)
		if err != nil {
			logger.Error("初始化共享读取端失败，拒绝启动", "error", err)
			return 1
		}
		sharesReader = sharesFetcher
		// 账号读端（P14.5）：列表复用 share.accounts 的那一份（同一张
		// managed_accounts），公钥走 accounts.ssh_keys。合成在 accounts.NewReader 里，
		// 这里只把两半接上——列表没有第二份实现，公钥也没有。
		accountsFetcher, err := accounts.NewFetcher(client, logger)
		if err != nil {
			logger.Error("初始化账号读取端失败，拒绝启动", "error", err)
			return 1
		}
		accountsReader, err = accounts.NewReader(sharesFetcher, accountsFetcher)
		if err != nil {
			logger.Error("初始化账号读端失败，拒绝启动", "error", err)
			return 1
		}
		// 二次验证（P14.6）：种子的生成、校验与销毁都在代理里，面板只搬运引用与六位数。
		// 用通用客户端（10 秒上限）：代理那四条路由只做一次 HMAC，比这个数短得多。
		totpAgent = client
		discoveryFetcher, err := discovery.NewFetcher(client, logger)
		if err != nil {
			logger.Error("初始化应用发现读取端失败，拒绝启动", "error", err)
			return 1
		}
		containersFetcher, err := containers.NewFetcher(client, logger)
		if err != nil {
			logger.Error("初始化容器读取端失败，拒绝启动", "error", err)
			return 1
		}
		containersReader = containersFetcher
		nginxFactsClient, err := proxies.NewAgentFacts(client, logger)
		if err != nil {
			logger.Error("初始化 Nginx 现状读取端失败，拒绝启动", "error", err)
			return 1
		}
		nginxFacts = nginxFactsClient
		pkgFacts, err := packages.NewAgentFacts(pkgClient, logger)
		if err != nil {
			logger.Error("初始化软件包事实读取端失败，拒绝启动", "error", err)
			return 1
		}
		pkgService, err := packages.New(pkgFacts)
		if err != nil {
			logger.Error("初始化软件包服务失败，拒绝启动", "error", err)
			return 1
		}
		packagesReader = pkgService
		certInspector, err := certificates.NewAgentInspector(client)
		if err != nil {
			logger.Error("初始化证书状态读取端失败，拒绝启动", "error", err)
			return 1
		}
		certificateState = certInspector
		// 备份定义镜像与快照查询：直接用专用客户端，它的方法签名就是 api.BackupManager。
		backupManager = backupClient
		// 计划与脚本镜像：同样是那个通用客户端的方法签名就是 api.SchedulesReader /
		// api.ScriptsReader，但**不能**复用通用客户端——列表的代价随计划条数增长
		// （每条启用计划 fork 两次 systemd 查询），10 秒通用上限会把一次正常的列表
		// 变成"代理不可用"（量见 agentclient.ScheduleQueryTimeout）。
		scheduleClient, err := agentclient.New(cfg.Agent.SocketPath,
			agentclient.WithTimeout(agentclient.ScheduleQueryTimeout))
		if err != nil {
			logger.Error("初始化计划任务代理客户端失败，拒绝启动",
				"error", err, "socket", cfg.Agent.SocketPath)
			return 1
		}
		schedulesReader = scheduleClient
		scriptsReader = scheduleClient
		// 网络与防火墙只读（P14.1）：通用客户端就够。一次采集由代理侧的 8 秒总预算
		// 与 6 秒单命令上限兜住（见 internal/agent/networkquery.go），比这里的 10 秒短，
		// 所以永远轮不到面板先把代理掐断。
		networkReader = client
		// 部署现场（P15.4）：通用客户端就够。一次列表最多读 200 份现场，每份两次
		// systemctl（enabled/active），代理侧按 ctx 与单命令上限兜住，比这里的
		// 10 秒长不了。
		deploymentsReader = client
		// 外部程序能力（P16.1）：读代理的探测缓存，不在请求里探。
		capabilitiesReader = client
		discoveryReader = discoveryFetcher
		healthTargetChecker = discoveryFetcher
		healthProber = discoveryFetcher
		logger.Info("代理客户端已配置", "socket", cfg.Agent.SocketPath)
	} else {
		// 明确说明原因：演示模式下预检与系统信息会返回 503，而不是给一份假的数据。
		logger.Warn("演示模式不连接特权代理，预检与系统信息接口将不可用",
			"socket", cfg.Agent.SocketPath)
	}
	// 应用与分组元数据（P09.1）：纯数据库，不依赖代理。
	// 依赖卷的存在性只在写入带依赖卷的应用时才需要问代理；storageReader 为 nil
	//（演示模式）时那类写入会明确回 503，而不是存下一条未经验证的引用。
	//
	// 回环入口地址只在演示模式放开：那时浏览器和面板在同一台机器上，127.0.0.1 真的能打开。
	appsService, err := apps.New(db, apps.Options{
		Volumes: apps.NewVolumeChecker(storageReader),
		// 健康检查的目标必须真的是代理发现出来的东西：形状校验（local_/cntr_）挡的是
		// 任意 host，存在性校验挡的是“一个看起来合法、其实探不到的标识”。
		Targets: healthTargetChecker,
		Normalize: apps.NormalizeOptions{
			AllowLoopbackPublicURL: mode == platform.ModeMock,
		},
	})
	if err != nil {
		logger.Error("初始化应用元数据服务失败，拒绝启动", "error", err)
		return 1
	}

	// 代理站点元数据（P10.1）：站点定义存在面板库里，读列表与详情不依赖代理；
	// 但**任何写入都要先判域名冲突**，判不了（没连代理、有配置解析失败）就拒绝写入。
	// facts 为 nil 时（演示模式）写入明确回 503，而不是落一条可能覆盖别人站点的记录。
	//
	// PanelPort 用真实监听端口，不写死 9443：面板端口可配，而上游端口永远不能等于它——
	// 配一个反代把管理面板暴露到公网，是这套系统里最容易犯也最不该犯的错。
	panelPort := 0
	if _, portStr, err := net.SplitHostPort(listenAddr); err == nil {
		if p, convErr := strconv.Atoi(portStr); convErr == nil {
			panelPort = p
		}
	}
	var nginxFactsSource proxies.FactsSource
	if nginxFacts != nil {
		nginxFactsSource = nginxFacts
	}
	// 证书（P10.3）与代理站点互相需要对方的一小块能力：站点写入前要问"这张证书
	// 能不能给这个域名用"，删证书前要问"还有哪些站点绑着它"。两个服务都不持有
	// 对方的指针——接口在这里互相注入，构造顺序靠一层极小的间接层解开。
	certUsage := &certificateUsage{}
	certService, err := certificates.New(db, certificates.Options{Usage: certUsage})
	if err != nil {
		logger.Error("初始化证书服务失败，拒绝启动", "error", err)
		return 1
	}
	proxiesService, err := proxies.New(db, proxies.Options{
		Facts:     nginxFactsSource,
		Apps:      appsService,
		Certs:     certService,
		Normalize: proxies.NormalizeOptions{PanelPort: panelPort},
	})
	if err != nil {
		logger.Error("初始化代理站点服务失败，拒绝启动", "error", err)
		return 1
	}
	// 这一步必须在开始监听之前完成：它只补上"证书占用查询"那半边，
	// 补齐之前不会有任何请求进来。
	certUsage.proxies = proxiesService

	// 证书任务的结果 → 证书记录（P10.3 的账本）。
	certLedger, err := certificates.NewJobApplier(certService, logger)
	if err != nil {
		logger.Error("初始化证书账本失败，拒绝启动", "error", err)
		return 1
	}

	// 健康探测调度（P09.2）：按每个应用自己的 interval_seconds 让代理探一次，
	// 结论落库（app_health_results）。探测只在生产模式启动——演示模式没有代理，
	// 起一个每 5 秒空转并在库里写 unknown 的循环，只会造出一堆假结论。
	if healthProber != nil {
		scheduler, err := apphealth.New(appsService, healthProber, logger)
		if err != nil {
			logger.Error("初始化健康探测调度失败，拒绝启动", "error", err)
			return 1
		}
		go scheduler.Run(ctx)
		logger.Info("健康探测调度已启动", "tick", apphealth.DefaultTick.String())
	}
	opsService, err := operations.New(db, opsAgent, logger, operations.Options{
		// 启动依赖门禁（P09.4）：应用与依赖卷的关系只存在 Web 库里，代理算不出来，
		// 所以这半边由 Web 补在预检之后。它只能收紧——代理拒绝过的预检不会因此放行。
		Gate: &appruntime.Gate{Apps: appsService, Volumes: storageReader},
		// 证书账本（P10.3）：任务进入终态时把代理报告里的证书事实抄进 certificates 表。
		// 放在这里而不是"等用户刷新页面时补写"，是因为任务中心关掉标签页的那一刻，
		// 那条新证书（或那次失败）就会从账本里消失。
		OnJobFinished: func(ctx context.Context, job operations.Job, params json.RawMessage) {
			if err := certLedger.Apply(ctx, certificates.JobFact{
				ID:     job.ID,
				Action: job.Action,
				Status: job.Status,
				Result: job.Result,
				Error:  job.Error,
			}, params); err != nil {
				// 账本没写成不改任务状态（任务已经结束了），但必须留下 ERROR：
				// 界面上的证书列表与主机上的现实脱节的唯一线索就在这里。
				logger.Error("证书账本写入失败", "error", err,
					"job_id", job.ID, "action", job.Action)
			}
		},
	})
	if err != nil {
		logger.Error("初始化预检服务失败，拒绝启动", "error", err)
		return 1
	}
	// 任务对账（P03.4，CONTRACTS.md §4.3）：启动时立刻扫一遍非终态/待对账任务，之后每
	// operations.ReconcileInterval 一次。只查不跑——对账永远不会重新提交操作。
	// 用 goroutine 是因为它不该挡住监听；扫描自己尊重 ctx，进程停止时一并退出。
	go opsService.RunReconciler(ctx)

	srv, err := api.New(api.Options{
		ListenAddress:   listenAddr,
		Mode:            mode,
		Version:         version.Get(),
		Logger:          logger,
		ShutdownTimeout: cfg.Web.ShutdownTimeout(),
		SchemaVersion:   migrateResult.To,
		Frontend:        frontend,
		TLS:             panelTLS,
		Auth:            authService,
		Audit:           auditWriter,
		Operations:      opsService,
		Monitor:         monitor,
		// 历史监控（P13.1）：查询读面板自己的样本库，不经过代理；采集循环
		// 在上面已经按配置启动。
		Metrics: metricsReader,
		Alerts:  alertsService,
		// 通知通道（P13.3）：定义与投递账在面板库，发送在代理。
		Notifications: notifService,
		Storage:       storageReader,
		Services:      servicesReader,
		Processes:     processesReader,
		Logs:          logsReader,
		Files:         filesReader,
		Terminal:      terminalReader,
		Shares:        sharesReader,
		// 受管理账号（P14.5）：只读。写走预检与任务。
		Accounts: accountsReader,
		// 二次验证（P14.6）：登录与再认证的第二步；启用了 2FA 的账号在 nil 时失败关闭。
		Totp:       totpAgent,
		Apps:       appsService,
		Discovery:  discoveryReader,
		Containers: containersReader,
		// 应用运行摘要（P09.4）：元数据来自库里，运行事实来自服务与容器读取端。
		// 演示模式下两个来源都是 nil，Resolver 会给每条应用 unknown + 理由。
		AppRuntime: &appruntime.Resolver{Services: servicesReader, Containers: containersReader},
		// 代理站点（P10.1）：定义来自库，Nginx 现状来自代理。演示模式下后者为 nil，
		// 读写的行为差别见 internal/api/proxies.go 的文件头说明。
		Proxies:    proxiesService,
		NginxFacts: nginxFactsSource,
		PanelPort:  panelPort,
		// 软件包（P11.1）：只读。判断在面板侧（internal/packages），
		// 事实来自代理的 apt.packages / system.power_state。
		Packages: packagesReader,
		// 证书（P10.3）：记录读库；现场事实问代理；上传暂存在本进程内存里。
		Certificates:      certService,
		CertificateStages: certificates.NewStageStore(nil),
		CertificateState:  certificateState,
		// 备份（P12.2）：定义与查询都在代理那边，面板不存第二份。
		Backup: backupManager,
		// 计划任务与受管理脚本（P12.4）：只读镜像，写路径走预检与任务。
		Schedules: schedulesReader,
		Scripts:   scriptsReader,
		// 网络与防火墙（P14.1）：只读，事实全部来自代理的 /proc、/sys 与 ufw。
		// 演示模式下为 nil，四个接口回 503 并说明未配置代理连接。
		Network: networkReader,
		// 部署管理面（P15.4）：只读现场。创建/更新/删除走预检与任务
		// （deployment.apply / update / remove），这里没有"直接部署"的入口。
		Deployments:    deploymentsReader,
		Capabilities:   capabilitiesReader,
		AllowedOrigins: allowedOrigins,
	})
	if err != nil {
		logger.Error("初始化 Web 服务失败", "error", err)
		return 1
	}

	if err := srv.Serve(ctx); err != nil {
		logger.Error("Web 服务退出", "error", err)
		return 1
	}
	return 0
}

// certificateUsage 打破证书服务与代理站点服务之间的构造循环。
//
// 两个服务各自只需要对方的一小块能力：certificates 要问"这张证书还被哪些站点绑着"
// （删记录前必须知道），proxies 要问"这张证书能不能给这个域名用"（写入前必须知道）。
// 它们互相注入接口而不是指针，剩下的只是先构造哪一个。
//
// proxies 字段在开始监听之前赋值，赋值之后不再改变：没有锁，也不需要锁——
// 那时还没有任何请求能到达。
type certificateUsage struct {
	proxies interface {
		SitesUsingCertificate(ctx context.Context, certID string) ([]string, error)
	}
}

// SitesUsingCertificate 转给代理站点服务。
func (u *certificateUsage) SitesUsingCertificate(ctx context.Context, certID string) ([]string, error) {
	if u == nil || u.proxies == nil {
		// 没接上就返回空而不是报错：删除证书时的占用检查会因此放行，库层的外键
		// 仍然会挡住真正有站点绑着的删除。这条路径只会在启动顺序写错时出现。
		return nil, nil
	}
	return u.proxies.SitesUsingCertificate(ctx, certID)
}
