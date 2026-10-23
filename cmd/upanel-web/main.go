// upanel-web 是 uPanel 的 Web 服务入口。
//
// 权限边界（PLAN.md 第 4 节）：本项目要求该进程以 upanel-web 身份运行，无 sudo、
// 不加入 docker 组、不提供登录 shell。P01.1 不实现认证与系统操作，因此本入口只做
// 配置解析、日志、数据库迁移、认证服务与 HTTP 服务。
package main

import (
	"context"
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

	"upanel/internal/agentclient"
	"upanel/internal/api"
	"upanel/internal/apphealth"
	"upanel/internal/appruntime"
	"upanel/internal/apps"
	"upanel/internal/audit"
	"upanel/internal/auth"
	"upanel/internal/config"
	"upanel/internal/containers"
	"upanel/internal/discovery"
	"upanel/internal/files"
	"upanel/internal/logging"
	"upanel/internal/logs"
	"upanel/internal/monitoring"
	"upanel/internal/operations"
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

	// PLAN.md 第 6 节要求面板自身监听 HTTPS 9443，TLS 在 P10 接入。在 TLS 就绪之前
	// production 只允许回环地址：宁可启动失败，也不让管理面板以明文暴露在网络上。
	if mode == platform.ModeProduction && !config.IsLoopbackAddress(listenAddr) {
		fmt.Fprintf(stderr, "配置错误: web.listen_address: P10 接入 TLS 之前 production 只能绑定回环地址（当前 %q）\n", listenAddr)
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

	// 允许源只来自本机配置；不由 Host 头或 X-Forwarded-* 推导（CONTRACTS.md 第 2 节）。
	allowedOrigins, err := config.ResolveAllowedOrigins(cfg.Web.AllowedOrigins, listenAddr)
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
	var preflightAgent operations.Agent
	// 系统基础资源读取端（P04.1）。演示模式下保持 nil：接口回 503，不造假数据。
	var monitor api.Monitor
	// 存储只读视图读取端（P04.2）。演示模式下同样保持 nil。
	var storageReader api.StorageReader
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
		preflightAgent = client
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
	proxiesService, err := proxies.New(db, proxies.Options{
		Facts:     nginxFactsSource,
		Apps:      appsService,
		Normalize: proxies.NormalizeOptions{PanelPort: panelPort},
	})
	if err != nil {
		logger.Error("初始化代理站点服务失败，拒绝启动", "error", err)
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
	opsService, err := operations.New(db, preflightAgent, logger, operations.Options{
		// 启动依赖门禁（P09.4）：应用与依赖卷的关系只存在 Web 库里，代理算不出来，
		// 所以这半边由 Web 补在预检之后。它只能收紧——代理拒绝过的预检不会因此放行。
		Gate: &appruntime.Gate{Apps: appsService, Volumes: storageReader},
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
		Auth:            authService,
		Audit:           auditWriter,
		Operations:      opsService,
		Monitor:         monitor,
		Storage:         storageReader,
		Services:        servicesReader,
		Processes:       processesReader,
		Logs:            logsReader,
		Files:           filesReader,
		Terminal:        terminalReader,
		Shares:          sharesReader,
		Apps:            appsService,
		Discovery:       discoveryReader,
		Containers:      containersReader,
		// 应用运行摘要（P09.4）：元数据来自库里，运行事实来自服务与容器读取端。
		// 演示模式下两个来源都是 nil，Resolver 会给每条应用 unknown + 理由。
		AppRuntime: &appruntime.Resolver{Services: servicesReader, Containers: containersReader},
		// 代理站点（P10.1）：定义来自库，Nginx 现状来自代理。演示模式下后者为 nil，
		// 读写的行为差别见 internal/api/proxies.go 的文件头说明。
		Proxies:        proxiesService,
		NginxFacts:     nginxFactsSource,
		PanelPort:      panelPort,
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
