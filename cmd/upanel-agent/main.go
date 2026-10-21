// upanel-agent 是 uPanel 的特权代理入口。
//
// 固定边界（PLAN.md 第 4 节、CONTRACTS.md 第 15 节）：
//   - 以 root 运行，独立 systemd 服务，只监听 Unix socket，不监听 TCP；
//   - 只提供登记过的动作，绝不提供 execute(command)、任意 argv 或任意文件写入；
//   - 没有 mock 模式：演示数据由 Web 进程内的 mock 实现提供。
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"upanel/internal/agent"
	"upanel/internal/agentstore"
	"upanel/internal/config"
	"upanel/internal/files"
	"upanel/internal/journal"
	"upanel/internal/logging"
	"upanel/internal/monitoring"
	"upanel/internal/platform"
	"upanel/internal/processes"
	"upanel/internal/services"
	"upanel/internal/storage"
	"upanel/internal/terminal"
	"upanel/internal/version"
	"upanel/migrations/agentdb"
)

const program = "upanel-agent"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	// 容量 Helper 模式（P04.3）：重新执行自己，在一个可以被父进程放弃的子进程里
	// 对网络挂载做一次 statfs。必须在解析配置、打开数据库之前处理：
	// helper 不碰任何状态，只做一次系统调用就把结果打到 stdout。
	//
	// 它只有一条固定路径：一个参数（挂载点的绝对路径），不读文件内容，不返回目录项，
	// 因此不是“万能命令执行入口”（PLAN.md 第 4 节）。
	if len(args) >= 1 && args[0] == storage.StatfsHelperFlag {
		return storage.RunStatfsHelper(args[1:])
	}

	fs := flag.NewFlagSet(program, flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		configPath  = fs.String("config", "", "配置文件路径（JSON）；缺省读取平台默认路径")
		socketFlag  = fs.String("socket", "", "Unix socket 绝对路径（覆盖配置文件）")
		dbFlag      = fs.String("db", "", "代理数据库绝对路径（覆盖配置文件）")
		keyFlag     = fs.String("secret-key", "", "本机秘密密钥绝对路径（覆盖配置文件）")
		initKey     = fs.Bool("init-key", false, "生成新的本机秘密密钥后退出（已存在则失败，绝不覆盖）")
		acceptance  = fs.Bool("acceptance-actions", false, "启用验收专用动作表（仅用于测试 VM，生产环境不得使用）")
		logLevel    = fs.String("log-level", "", "日志级别：debug、info、warn、error")
		logFormat   = fs.String("log-format", "", "日志格式：json 或 text")
		showVersion = fs.Bool("version", false, "输出版本信息并退出")
	)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "%s — uPanel 特权代理（仅 Linux，必须以 root 运行，只监听 Unix socket）\n\n"+
			"用法: %s [参数]\n\n参数:\n", program, program)
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

	if *socketFlag != "" {
		cfg.Agent.SocketPath = *socketFlag
	}
	if *dbFlag != "" {
		cfg.Agent.DatabasePath = *dbFlag
	}
	if *keyFlag != "" {
		cfg.Agent.SecretKeyPath = *keyFlag
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

	// 代理的启动前提：Linux + root。非 Linux 或非 root 一律拒绝，不降级为演示模式。
	if err := platform.CheckAgent(); err != nil {
		fmt.Fprintf(stderr, "拒绝启动: %v\n", err)
		return 1
	}

	// 生成密钥不需要账号解析与代理服务，先处理并退出。
	if *initKey {
		return initSecretKey(cfg.Agent.SecretKeyPath, stdout, stderr)
	}

	// 对端 UID 规则按账号名解析；账号不存在时拒绝启动，而不是默认放行任何人。
	peer, err := agent.ResolvePeerPolicy(cfg.Agent.WebUser, cfg.Agent.SocketGroup)
	if err != nil {
		fmt.Fprintf(stderr, "拒绝启动: %v\n", err)
		return 1
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

	// 代理数据库与秘密密钥是硬依赖：打不开、迁移失败、密钥不合规都必须拒绝启动。
	// 带着“能起来但存不了秘密”的状态运行，等于把失败推迟到管理员最需要它的时刻。
	if cfg.Agent.DatabasePath == "" {
		logger.Error("未配置代理数据库路径，拒绝启动",
			"field", "agent.database_path",
			"hint", "本平台没有默认值，请用 --db 或配置文件显式指定")
		return 2
	}
	if cfg.Agent.SecretKeyPath == "" {
		logger.Error("未配置秘密密钥路径，拒绝启动",
			"field", "agent.secret_key_path",
			"hint", "本平台没有默认值，请用 --secret-key 或配置文件显式指定")
		return 2
	}

	agentDB, err := agentstore.Open(context.Background(), agentstore.Options{
		Path:        cfg.Agent.DatabasePath,
		BusyTimeout: cfg.Database.BusyTimeout(),
		Logger:      logger,
	})
	if err != nil {
		logger.Error("代理数据库打开失败，拒绝启动", "error", err, "path", cfg.Agent.DatabasePath)
		return 1
	}
	defer func() {
		if err := agentDB.Close(); err != nil {
			logger.Error("关闭代理数据库失败", "error", err)
		}
	}()

	migrateResult, err := agentDB.Migrate(context.Background(), agentdb.FS)
	if err != nil {
		logger.Error("代理数据库迁移失败，拒绝启动", "error", err, "path", cfg.Agent.DatabasePath)
		return 1
	}

	keyring, err := agentstore.LoadKeyring(cfg.Agent.SecretKeyPath)
	if err != nil {
		logger.Error("本机秘密密钥不可用，拒绝启动",
			"error", err,
			"path", cfg.Agent.SecretKeyPath,
			"hint", "首次部署请以 root 运行 `upanel-agent -init-key`，并把密钥文件单独备份")
		return 1
	}
	secrets, err := agentstore.NewSecrets(agentDB, keyring, logger, agentstore.SecretsOptions{})
	if err != nil {
		logger.Error("初始化秘密存储失败，拒绝启动", "error", err)
		return 1
	}
	plans, err := agentstore.NewPreflightPlans(agentDB)
	if err != nil {
		logger.Error("初始化预检计划存储失败，拒绝启动", "error", err)
		return 1
	}
	operations, err := agentstore.NewOperations(agentDB)
	if err != nil {
		logger.Error("初始化执行记录存储失败，拒绝启动", "error", err)
		return 1
	}

	// 系统基础资源采集（P04.1）：5 秒轮询一次 /proc、/sys，结果留在内存里的快照中，
	// system.summary 查询只读这份快照。采集失败不阻止代理启动——它只会让对应段
	// 在响应里标成 error/unavailable，capabilities 仍然是真实能力。
	collector := monitoring.NewCollector(monitoring.Options{Logger: logger})

	// 存储拓扑采集（P04.2）：5 秒扫描一次 /sys、/proc 与 /dev/disk，容量按 30 秒缓存，
	// storage.topology 查询只读最近一次快照。全部只读。
	// P04.3 起额外：每 5 秒读 /proc/diskstats 算速率，按 15 分钟（可配 5—60）读 SMART，
	// 网络挂载容量走隔离子进程——不执行任何改变系统状态的命令。
	storageOpts := storage.Options{
		Logger:                 logger,
		HealthInterval:         cfg.Agent.Storage.SmartInterval(),
		NetworkCapacityTimeout: cfg.Agent.Storage.NetworkCapacityTimeout(),
		// P07.1：存储写动作预检需要的探针（设备签名、进程占用），全部只读。
		// 接上之后每台设备的 capabilities 才会被填成真实状态；没接就是空数组。
		Env: storage.SystemEnv(),
	}
	if cfg.Agent.Storage.SmartEnabled {
		if path, ok := storage.FindSmartctl(cfg.Agent.Storage.SmartctlPath); ok {
			storageOpts.Prober = &storage.SmartProber{Path: path}
			logger.Info("已启用 SMART 采集", "smartctl", path,
				"interval", cfg.Agent.Storage.SmartInterval().String())
		} else {
			logger.Warn("未找到 smartctl，物理盘健康将保持 unknown",
				"hint", "安装 smartmontools，或在 agent.storage.smartctl_path 指定绝对路径",
				"configured", cfg.Agent.Storage.SmartctlPath)
		}
	} else {
		logger.Info("SMART 采集已在配置中关闭")
	}
	storageCollector := storage.NewCollector(storageOpts)

	// 服务单元采集（P05.1）：每 10 秒枚举一次 systemctl 状态，services.list 读这份快照；
	// services.unit 与预检/执行现场读取。找不到 systemctl 时采集如实报依赖缺失，
	// 不阻止代理启动（其余模块与任务链路仍然可用）。
	var servicesCollector *services.Collector
	if path, ok := services.FindSystemctl(cfg.Agent.Services.SystemctlPath); ok {
		ctl, err := services.NewSystemctl(path)
		if err != nil {
			logger.Error("初始化 systemctl 调用器失败", "error", err, "path", path)
			return 1
		}
		servicesCollector = services.NewCollector(services.Options{Systemctl: ctl, Logger: logger})
		logger.Info("已启用服务状态采集", "systemctl", path, "interval", services.DefaultInterval.String())
	} else {
		logger.Warn("未找到 systemctl，服务状态将报依赖缺失",
			"hint", "本机需要 systemd；或在 agent.services.systemctl_path 指定绝对路径",
			"configured", cfg.Agent.Services.SystemctlPath)
	}

	// 进程采集（P05.2）：每 5 秒扫一次 /proc，processes.list 读这份快照；
	// 预检与执行现场读取 /proc。它只读 /proc，唯一的写操作是 syscall.Kill，
	// 参数由 internal/processes 构造，信号只能是 TERM 或 KILL。
	processesCollector := processes.NewCollector(processes.Options{Logger: logger})
	logger.Info("已启用进程状态采集",
		"interval", processes.DefaultInterval.String(),
		"max_processes", processes.MaxProcesses,
		"self_pid", os.Getpid())

	// 日志读取（P05.3）：journal 查询与实时跟随都只调用 journalctl，参数由
	// internal/journal 构造（unit 形状、级别闭集、RFC3339 时间、游标字符集都校验过）。
	// 找不到 journalctl 时不登记 logs.query、也不接受日志流许可——capabilities
	// 因此不会报告一个调用就失败的查询。
	var logsQuerier agent.LogsSource
	if path, ok := journal.FindJournalctl(cfg.Agent.Logs.JournalctlPath); ok {
		q, err := journal.NewQuerier(path, journal.SyscmdRunner(), journal.SyscmdStreamer(), nil)
		if err != nil {
			logger.Error("初始化 journal 读取器失败", "error", err, "path", path)
			return 1
		}
		logsQuerier = q
		logger.Info("已启用日志读取",
			"journalctl", path,
			"max_limit", journal.MaxLimit,
			"max_span", journal.MaxSpan.String(),
			"max_page_bytes", journal.MaxPageBytes,
			"max_message_bytes", journal.MaxMessageBytes,
			"stream_duration", journal.DefaultStreamDuration.String(),
			"stream_idle", journal.DefaultStreamIdle.String())
	} else {
		logger.Warn("未找到 journalctl，日志查询与日志流将不可用",
			"hint", "本机需要 systemd；或在 agent.logs.journalctl_path 指定绝对路径",
			"configured", cfg.Agent.Logs.JournalctlPath)
	}

	// 文件模块（P06.1）：根只能来自配置（agent.files.roots），请求里只有 root_id
	// 与根内相对路径。没有登记任何根时仍然构造 Manager：files.roots 返回空列表，
	// 界面显示“未登记文件根”——而不是让文件页看起来能浏览整个服务器。
	//
	// 根的身份在**登记时**核对一次（打开根目录、记下 dev/inode 与挂载点），
	// 之后每次请求都重新打开并比对；路径解析靠 openat2 的 RESOLVE_*，
	// 不做字符串前缀判断。容量读取接存储模块的同一条通道（网络挂载走隔离子进程）。
	filesManager, err := files.New(fileRoots(cfg.Agent.Files.Roots), files.Options{
		Capacity: filesCapacity{reader: storage.SystemRoot().Caps},
		Logger:   logger,
		// 单次上传上限（P06.2）。0 表示用默认 2 GiB；配置层已经校验过它不会是负值。
		MaxUploadBytes: cfg.Agent.Files.MaxUploadBytes,
	})
	if err != nil {
		logger.Error("初始化文件模块失败", "error", err)
		return 1
	}
	for _, root := range cfg.Agent.Files.Roots {
		logger.Info("已登记文件根",
			"root_id", root.ID, "name", root.Name, "path", root.Path, "writable", root.Writable)
	}
	if len(cfg.Agent.Files.Roots) == 0 {
		logger.Info("没有登记任何文件根：文件页只显示“未登记文件根”",
			"hint", "在 /etc/upanel/upanel.json 的 agent.files.roots 里登记要管理的目录")
	}

	// 终端会话层（P06.3）。三件事必须一起成立才会启用：
	//   1) 账号 upanel-shell 存在、不是 root、shell 不是 nologin、home 属于它且别人写不了；
	//   2) 不是特权组的成员（sudo/docker/disk…）——加入任何一个，降权就只是名义上的；
	//   3) 本平台有 PTY（只有 Linux 有）。
	// 任何一项不满足都**不登记**终端能力并说明怎么修：带着一个注定起不来的 shell
	// 对外宣称“有终端”，只会把问题推给用户去猜。
	//
	// shell 进程的降权发生在 fork/exec 之间（terminal.startPTY），并且启动后还会
	// 读回 /proc/<pid>/status 核对内核报告的 uid/gid——不是“我传了参数所以应该生效”。
	var terminalManager *terminal.Manager
	terminalAccount, terminalErr := terminal.ResolveAccount(
		cfg.Agent.Terminal.EffectiveShellUser(),
		cfg.Agent.Terminal.EffectiveHome(),
		cfg.Agent.Terminal.ShellPath,
	)
	if terminalErr != nil {
		logger.Warn("终端能力未启用",
			"reason", terminalErr,
			"hint", "按安装步骤创建专用账号（useradd -r -M -d /var/lib/upanel-shell -s /bin/bash upanel-shell）、"+
				"把 home 权限收到 0750、并确保它不在 sudo/docker/disk 等特权组里")
	} else {
		mgr, err := terminal.NewManager(terminal.Options{
			Account: terminalAccount,
			Limits: terminal.Limits{
				IdleTimeout:     cfg.Agent.Terminal.IdleTimeout(),
				MaxDuration:     cfg.Agent.Terminal.MaxDuration(),
				DisconnectGrace: cfg.Agent.Terminal.DisconnectGrace(),
				MaxPerUser:      terminal.ContractMaxPerUser,
				MaxSessions:     terminal.ContractMaxSessions,
			},
			Logger: logger,
		})
		if err != nil {
			logger.Warn("终端能力未启用", "reason", err)
		} else {
			terminalManager = mgr
			logger.Info("已启用受限终端",
				"account", terminalAccount.User,
				"account_uid", terminalAccount.UID,
				"account_gid", terminalAccount.GID,
				"home", terminalAccount.Home,
				"shell", terminalAccount.Shell,
				"idle_timeout", cfg.Agent.Terminal.IdleTimeout().String(),
				"max_duration", cfg.Agent.Terminal.MaxDuration().String(),
				"disconnect_grace", cfg.Agent.Terminal.DisconnectGrace().String(),
				"max_per_user", terminal.ContractMaxPerUser,
				"max_sessions", terminal.ContractMaxSessions)
		}
	}

	// 动作登记表：生产表自 P05.1 起有 service.control，P05.2 起有 process.signal（受保护名单与读回验证见 internal/services）。
	// 验收表只能靠显式参数启用，且动作全部以 acceptance. 开头、不读写任何系统资源。
	registry := agent.NewRegistry()
	if *acceptance {
		registry = agent.NewAcceptanceRegistry()
		logger.Warn("已启用验收专用动作表：仅用于测试虚拟机，生产环境不得使用",
			"actions", registry.ActionNames())
	}
	// 查询表在两种模式下都是生产实现：验收要验证的就是真实采集链路。
	deps := agent.ProductionDeps{
		Collector: collector,
		Storage:   storageCollector,
		Services:  servicesCollector,
		Processes: processesCollector,
		Logs:      logsQuerier,
		Files:     filesManager,
		// 写端与读端是同一个 Manager：根登记表、句柄级边界、错误判决都只有一份。
		FileWrites: filesManager,
	}
	// 只在真的有时才赋给接口：把一个 nil 的 *terminal.Manager 塞进接口会得到
	// 一个非 nil 的接口值，登记表于是多出一个调用就 panic 的能力。
	var terminalAPI agent.TerminalManager
	if terminalManager != nil {
		deps.Terminal = terminalManager
		terminalAPI = terminalManager
	}
	if err := agent.RegisterProductionQueries(registry, deps); err != nil {
		logger.Error("登记生产查询失败", "error", err)
		return 1
	}
	// 真实动作只在生产模式登记：验收模式的动作表必须保持“只有 acceptance.*”，
	// 否则验收链路会顺手拿到一个能改真实服务状态的动作。
	if !*acceptance {
		if err := agent.RegisterProductionActions(registry, deps); err != nil {
			logger.Error("登记生产动作失败", "error", err)
			return 1
		}
	}

	logger.Info("启动",
		"version", version.Get().Version,
		"commit", version.Get().Commit,
		"go_version", version.Get().GoVersion,
		"os", plat.OS,
		"arch", plat.Arch,
		"config_source", source,
		"socket", cfg.Agent.SocketPath,
		"allowed_web_user", peer.WebUser,
		"allowed_web_uid", peer.WebUID,
		"socket_group", peer.SocketGroup,
		"agent_database", cfg.Agent.DatabasePath,
		"agent_schema_version", migrateResult.To,
		// 字段名刻意不含 secret/key 之外的敏感词：日志脱敏按字段名匹配，
		// 命中就整值打码（失败关闭）。这些是路径与指纹，本身不是秘密，需要能看见。
		"key_path", cfg.Agent.SecretKeyPath,
		"key_version", keyring.Version(),
		// 指纹只能用来核对“是不是同一把钥匙”，不能用来恢复密钥。
		"key_fingerprint", keyring.Fingerprint(),
	)

	srv, err := agent.New(agent.Options{
		SocketPath:      cfg.Agent.SocketPath,
		Version:         version.Get(),
		Logger:          logger,
		ShutdownTimeout: cfg.Agent.ShutdownTimeout(),
		Peer:            peer,
		Registry:        registry,
		Secrets:         secrets,
		Plans:           plans,
		Operations:      operations,
		Logs:            logsQuerier,
		Files:           filesManager,
		FileWrites:      filesManager,
		Terminal:        terminalAPI,
	})
	if err != nil {
		logger.Error("初始化代理失败", "error", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 采集循环：立刻采一次，之后每 5 秒一次。它们只是读 /proc 与 /sys，不该挡住监听，
	// 所以放 goroutine；ctx 结束时一并退出。
	go collector.Run(ctx)
	go storageCollector.Run(ctx)
	if servicesCollector != nil {
		go servicesCollector.Run(ctx)
	}
	go processesCollector.Run(ctx)
	if terminalManager != nil {
		// 会话超时回收（空闲/最长寿命/断流宽限）由这一个循环负责；
		// 它每秒看一次表，代价是几个 map 迭代，而省掉的是“定时器按会话创建/销毁”的簿记错误。
		go terminalManager.Run(ctx)
	}

	if err := srv.Serve(ctx); err != nil {
		logger.Error("代理退出", "error", err)
		if terminalManager != nil {
			// 代理要走就把 PTY 一起带走：留一个没有父进程的 shell，
			// 就是留一个谁都关不掉、也不再受限额约束的会话。
			terminalManager.Shutdown()
		}
		return 1
	}
	if terminalManager != nil {
		terminalManager.Shutdown()
	}
	return 0
}

// initSecretKey 生成新的本机秘密密钥。
//
// 只在密钥不存在时成功：已存在则失败退出，绝不覆盖（覆盖等于让库里的密文永久不可解）。
// 输出只有路径、权限与指纹——密钥本身不回显到终端，也不进日志。
func initSecretKey(path string, stdout, stderr io.Writer) int {
	if path == "" {
		fmt.Fprintf(stderr, "错误: 未配置秘密密钥路径（agent.secret_key_path 或 --secret-key）\n")
		return 2
	}
	fingerprint, err := agentstore.GenerateKeyFile(path)
	if err != nil {
		fmt.Fprintf(stderr, "生成密钥失败: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "已生成密钥文件: %s（属主 root:root，权限 0600）\n", path)
	fmt.Fprintf(stdout, "密钥指纹: %s\n", fingerprint)
	fmt.Fprintln(stdout, "该文件必须单独备份：丢失或损坏后，已保存的秘密将无法解密。")
	fmt.Fprintln(stdout, "不要把它放进 uPanel 自己的备份仓库，也不要让 Web 进程或 upanel-web 账号读到。")
	return 0
}

// fileRoots 把配置里的文件根转成文件模块的登记项。
//
// 单独一个函数是为了让“配置 → 登记”这一层显式：配置校验（绝对路径、非 "/"、
// id 形状）在 internal/config 里做完，文件模块只接受已校验过的形状。
func fileRoots(configured []config.FileRoot) []files.Root {
	out := make([]files.Root, 0, len(configured))
	for _, root := range configured {
		out = append(out, files.Root{
			ID:       root.ID,
			Name:     root.Name,
			Path:     root.Path,
			Writable: root.Writable,
		})
	}
	return out
}

// filesCapacity 把存储模块的容量读取器接到文件模块上。
//
// 两边的容量字段相同，这里只是类型转换：不重新 statfs、不另起通道，
// 因此网络挂载的“隔离子进程 + 超时后隔离 5 分钟”策略与存储页完全一致。
type filesCapacity struct {
	reader storage.CapacityReader
}

func (c filesCapacity) Capacity(ctx context.Context, mountPath, fsType string) (*files.Capacity, error) {
	if c.reader == nil {
		return nil, nil
	}
	capv, err := c.reader.Capacity(ctx, mountPath, fsType)
	if err != nil || capv == nil {
		return nil, err
	}
	return &files.Capacity{
		TotalBytes:     capv.TotalBytes,
		AvailableBytes: capv.AvailableBytes,
		UsedPercent:    capv.UsedPercent,
		CollectedAt:    capv.CollectedAt,
	}, nil
}
