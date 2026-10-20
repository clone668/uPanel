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
	"upanel/internal/logging"
	"upanel/internal/monitoring"
	"upanel/internal/platform"
	"upanel/internal/storage"
	"upanel/internal/version"
	"upanel/migrations/agentdb"
)

const program = "upanel-agent"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
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
	// storage.topology 查询只读最近一次快照。全部只读，不执行任何外部命令。
	storageCollector := storage.NewCollector(storage.Options{Logger: logger})

	// 动作登记表：生产表当前为空（真实动作随 P07 起逐项登记）。
	// 验收表只能靠显式参数启用，且动作全部以 acceptance. 开头、不读写任何系统资源。
	registry := agent.NewRegistry()
	if *acceptance {
		registry = agent.NewAcceptanceRegistry()
		logger.Warn("已启用验收专用动作表：仅用于测试虚拟机，生产环境不得使用",
			"actions", registry.ActionNames())
	}
	// 查询表在两种模式下都是生产实现：验收要验证的就是真实采集链路。
	if err := agent.RegisterProductionQueries(registry, agent.ProductionDeps{Collector: collector, Storage: storageCollector}); err != nil {
		logger.Error("登记生产查询失败", "error", err)
		return 1
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

	if err := srv.Serve(ctx); err != nil {
		logger.Error("代理退出", "error", err)
		return 1
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
