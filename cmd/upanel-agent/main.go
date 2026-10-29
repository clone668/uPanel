// upanel-agent 是 uPanel 的特权代理入口。
//
// 固定边界（PLAN.md 第 4 节、CONTRACTS.md 第 15 节）：
//   - 以 root 运行，独立 systemd 服务，只监听 Unix socket，不监听 TCP；
//   - 只提供登记过的动作，绝不提供 execute(command)、任意 argv 或任意文件写入；
//   - 没有 mock 模式：演示数据由 Web 进程内的 mock 实现提供。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"upanel/internal/accounts"
	"upanel/internal/agent"
	"upanel/internal/agentstore"
	"upanel/internal/apt"
	"upanel/internal/backup"
	"upanel/internal/certificates"
	"upanel/internal/config"
	"upanel/internal/containers"
	"upanel/internal/deploy"
	"upanel/internal/discovery"
	"upanel/internal/files"
	"upanel/internal/hostcaps"
	"upanel/internal/journal"
	"upanel/internal/logging"
	"upanel/internal/monitoring"
	"upanel/internal/network"
	"upanel/internal/nginx"
	"upanel/internal/platform"
	"upanel/internal/processes"
	"upanel/internal/proxies"
	"upanel/internal/schedules"
	"upanel/internal/scripts"
	"upanel/internal/services"
	"upanel/internal/shares"
	"upanel/internal/storage"
	"upanel/internal/terminal"
	"upanel/internal/tlscert"
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

	// 回滚子命令（P14.2）：`upanel-netrollback@<token>.timer` 到期时由 systemd 直接
	// 执行它。同样必须在解析配置之前处理——这条路径**故意**不读配置、不开数据库、
	// 不碰 socket：它要恢复什么，全在回滚记录文件里。
	if len(args) >= 1 && args[0] == "netrollback" {
		return runNetrollback(args[1:], stdout, stderr)
	}

	// 回滚子命令（P14.3）：`upanel-fwrollback@<token>.timer` 到期时由 systemd 直接执行。
	// 与 netrollback 一样，这条路径不读配置、不开数据库、不碰 socket。
	if len(args) >= 1 && args[0] == "fwrollback" {
		return runFwrollback(args[1:], stdout, stderr)
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
		netWrites   = fs.Bool("network-writes", false, "启用网络配置写动作（默认关闭：改网卡等于把机器从网络上摘掉。120 秒独立回滚已在真实 Ubuntu VM 验收通过，见 Q458）")
		fwWrites    = fs.Bool("firewall-writes", false, "启用防火墙写动作（默认关闭：改规则同样能把管理员关在门外，验收见 docs/evidence/P14）")
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
	// 受管理卷的权威登记表（P07.4，CONTRACTS.md 14.2 managed_resources）。
	// 只有代理写它：卷“归谁管、UUID 是什么、上次是谁改的”必须以代理侧的记录为准，
	// Web 侧的 volumes 表是按 operation_id 对账的镜像。
	resourceStore, err := agentstore.NewResourceStore(agentDB)
	if err != nil {
		logger.Error("初始化受管资源存储失败，拒绝启动", "error", err)
		return 1
	}
	// 共享定义与受管理账号的登记表（P08.1）。同样只有代理写它：共享目录落在哪、
	// ACL 是不是按定义准备好了、配置版本是多少，都以代理侧的记录为准。
	shareStore, err := agentstore.NewShareStore(agentDB)
	if err != nil {
		logger.Error("初始化共享存储失败，拒绝启动", "error", err)
		return 1
	}
	// 备份仓库定义的代理镜像（P12.1）。Web 登记或修改仓库时把定义同步进来，
	// 动作执行时按 id 从这里取——repository_id 因此不是一个可以随便编的字符串。
	backupStore, err := agentstore.NewBackupRepositoryStore(agentDB)
	if err != nil {
		logger.Error("初始化备份仓库存储失败，拒绝启动", "error", err)
		return 1
	}
	// 备份策略的代理镜像（P12.2）。与仓库定义同一个道理：policy_id 不是可以随便编
	// 的字符串，动作执行时按 id 从这个库里取定义，版本不符就 409。
	policyStore, err := agentstore.NewBackupPolicyStore(agentDB)
	if err != nil {
		logger.Error("初始化备份策略存储失败，拒绝启动", "error", err)
		return 1
	}
	verificationStore, err := agentstore.NewBackupVerificationStore(agentDB)
	if err != nil {
		logger.Error("初始化恢复验证存储失败，拒绝启动", "error", err)
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
	// P08.3：共享依赖查询。装配上有一个环——storage 的判决要问“谁挂着这块卷”，
	// 而回答要读共享登记表与卷的现场视图（后者正是下面的 storageCollector）。
	// 所以这里先建一个待接线的适配器交给 storage，等共享模块与采集器都齐了再 Fill。
	// 没接线之前它返回错误，卷的卸载/格式化/退出管理会被拒——漏接线不该表现成“没有依赖”。
	shareDeps := agent.NewShareDependents()
	// P11.3：电源动作要知道“现在有没有写操作在跑”（STAGES §13 的“关键任务运行时拒绝电源操作”）。
	// 与上面同一个环：动作登记表在本函数里、Server 建好之前就要挂上去，而“谁在跑”只有
	// Server 知道。所以先建持有者，等 srv 出来再 Bind。没 Bind 之前它返回错误——查不到
	// 不等于没有，一次重启把别人的写操作断在半路的代价太高。
	powerDeps := agent.NewPowerDependents()
	storageEnv := storage.SystemEnv()
	storageEnv.Dependents = shareDeps.Probe

	storageOpts := storage.Options{
		Logger:                 logger,
		HealthInterval:         cfg.Agent.Storage.SmartInterval(),
		NetworkCapacityTimeout: cfg.Agent.Storage.NetworkCapacityTimeout(),
		// P07.1：存储写动作预检需要的探针（设备签名、进程占用），全部只读。
		// 接上之后每台设备的 capabilities 才会被填成真实状态；没接就是空数组。
		Env: storageEnv,
		// P07.2：磁盘写原语。Runner 是唯一把命令交给内核的出口（syscmd：绝对路径 +
		// argv + 固定环境 + 固定模板 stdin + 有上限的输出）。不给 Runner 就是
		// DEPENDENCY_MISSING，而不是“假装写成功”。
		// P07.3：格式化接了 mkfs.ext4/mkfs.xfs，并且必须有超级块读取器——
		// 没有它就无法读回核验，格式化会以 503 拒绝而不是“跑完就算成功”。
		Write: storage.WriteOptions{
			Runner:      storage.SyscmdRunner(),
			Superblocks: storage.SystemSuperblockProber(),
			// P07.4：卷管理。登记表是代理数据库里的 managed_resources；
			// fstab 的写入、候选校验（findmnt --verify）、备份与原子替换都在
			// storage.FstabStore 里，路径是代码里的常量（不来自配置和 HTTP）。
			Store: resourceStore,
			Fstab: storage.FstabStore{
				Path:      storage.DefaultFstabPath,
				BackupDir: storage.DefaultFstabBackupDir,
			},
		},
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
	if err := storage.CheckWriteDependencies(); err != nil {
		// 缺 sfdisk/mkfs 不阻止启动：预检与拓扑仍然可用，写动作会以 503 明确拒绝。
		logger.Warn("磁盘写原语的依赖不完整，写动作将被拒绝", "reason", err)
	} else {
		logger.Info("磁盘写原语已接线",
			"sfdisk", storage.DefaultSfdiskPath,
			"udevadm", storage.DefaultUdevadmPath,
			"mkfs_ext4", storage.DefaultMkfsExt4Path,
			"mkfs_xfs", storage.DefaultMkfsXFS)
	}
	if err := storage.CheckVolumeDependencies(); err != nil {
		// 缺 mount/umount/findmnt/systemctl 同样不阻止启动：预检与拓扑仍然可用，
		// 卷动作会以 503 明确拒绝，而不是“看起来能做、点了才报错”。
		logger.Warn("卷管理的依赖不完整，卷动作将被拒绝", "reason", err)
	} else {
		logger.Info("受管理卷已接线",
			"fstab", storage.DefaultFstabPath,
			"backup_dir", storage.DefaultFstabBackupDir,
			"volume_root", storage.DefaultVolumeRoot,
			"mount", storage.DefaultMountPath,
			"umount", storage.DefaultUmountPath,
			"systemctl", storage.DefaultSystemctlPath)
	}
	storageCollector := storage.NewCollector(storageOpts)

	// 共享与受管理账号（P08.1）。能力探针每次调用读固定的工具路径与 smb.conf，
	// 结果缓存 30 秒；执行器是 storage.SyscmdRunner（同一套绝对路径 + argv + 固定环境）。
	// 秘密解析器就是上面那个 secrets：密码明文只经标准输入，用完立刻清零。
	sharesEngine, err := shares.New(shares.Options{
		Store:   shareStore,
		Volumes: storageCollector,
		Secrets: secrets,
		Runner:  storage.SyscmdRunner(),
		Prober:  shares.SystemProber(storage.SyscmdRunner()),
	})
	if err != nil {
		logger.Error("初始化共享模块失败", "error", err)
		return 1
	}
	// 环合上了：storage 的判决从现在起能问到“谁挂着这块卷”，
	// 运行时监视也能读到同一份登记表与同一份卷现场视图。
	shareDeps.Fill(sharesEngine, storageCollector)
	logger.Info("共享模块已接线",
		"include_dir", shares.DefaultIncludeDir,
		"config_path", shares.DefaultConfigPath,
		"managed_config", shares.ManagedConfigPath,
		// 从 P08.2 起这里会真的写配置，能不能写由能力探针现场判定（缺 testparm、
		// 缺 smbcontrol 或 smb.conf 读不到都会降到 unsupported）。日志里不写死一个值：
		// 写死的话，一台缺工具的机器上日志会说“能写”，而实际一按就失败。
		"config_write", "由能力探针判定（见 capability.config_write）")

	// 受管理 Linux 账号、SSH 公钥与受限 sudo（P14.5）。
	//
	// 登记表是同一个 shareStore：managed_accounts 只有一张（DECISIONS Q475），
	// 共享账号与普通账号的区别是 type 那一列，不是两张表。公钥与 sudo 规则落在
	// P14.5 的两张新表里（迁移 0014）。
	//
	// 路径用默认值（/etc/sudoers.d、/etc/sudoers、/home），文件读写走真实 os：
	// 这一层要 fsync 目录、要保住 0440 的权限、要在符号链接上拒绝动手，
	// 这些都不能经过一个“看起来一样”的假实现。
	accountsEngine, err := accounts.New(accounts.Options{
		Store:   shareStore,
		Secrets: secrets,
		Runner:  storage.SyscmdRunner(),
	})
	if err != nil {
		logger.Error("初始化账号模块失败", "error", err)
		return 1
	}
	logger.Info("受管理账号已接线",
		"sudoers_dir", accounts.DefaultPaths().SudoersDir,
		"home_base", accounts.DefaultPaths().HomeBase,
		"actions", "account.apply / account.delete / ssh_key.apply / ssh_key.delete / "+
			"sudo_rule.apply / sudo_rule.delete",
		// 能不能真的动手由判决层现场判定：缺 useradd/visudo/sudo 时会以 503 明确拒绝，
		// 而不是留一个点下去才报错的按钮。日志里不写死“可用”两个字。
		"capability", "由判决层现场判定")

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

	// 应用发现与健康探测（P09.2）。三个来源各自独立：
	//   - 监听端口：读 /proc/net/tcp{,6}，再按 socket inode 反查持有进程；
	//   - systemd：复用 P05.1 的单元快照（不另开一条 systemctl 通道）；
	//   - 容器：docker ps（命令与参数都是常量，缺命令/缺套接字如实降级）。
	// 采集结果缓存 15 秒：候选列表给人确认，不是每次刷新都换一批。
	// 探测只走 127.0.0.1，地址由代理自己从发现结果里解析，调用方给不了 host。
	discoveryCollector := discovery.NewCollector(discovery.Deps{
		Units:  discoveryUnits{collector: servicesCollector},
		Docker: &discovery.DockerClient{},
		Logger: logger,
	})
	logger.Info("已启用应用发现",
		"ttl", discovery.DefaultTTL.String(),
		"docker_candidates", "/usr/bin/docker, /snap/bin/docker",
		"probe", "只探测 127.0.0.1，TLS 校验保持开启")

	// 容器读取与启停（P09.3）。docker 命令走绝对路径探测，套接字固定为
	// /var/run/docker.sock：Web 账号永远接触不到 docker.sock，代理也只调用
	// 常量 argv。可用性在每次调用时重新判定——Docker 停掉之后界面必须显示
	// “不可用”，而不是继续展示上一次的容器列表。
	containersClient := containers.NewClient()
	// 这一份读数只用于启动日志与备份模块的装配说明：真正的可用性在每次调用时
	// 重新判定（Docker 可能在这一行之后才被启动）。
	dockerAvailable, dockerNotice := containersClient.Availability()
	if dockerAvailable {
		logger.Info("已启用容器管理",
			"docker", "绝对路径探测：/usr/bin/docker、/snap/bin/docker",
			"socket", "/var/run/docker.sock",
			"scope", "只读查询 + 普通容器 start/stop/restart；危险容器不接管")
	} else {
		logger.Warn("容器功能不可用，相关查询与动作不会登记", "reason", dockerNotice)
	}

	// Nginx 现状（P10.1）。只读：读 nginx.conf、conf.d/*.conf、sites-enabled/*，
	// 外加 `nginx -v` 与 nginx.service 的状态。写配置、nginx -t、reload 是 P10.2 的
	// 任务链路，这里一个字节都不写。
	//
	// 可执行文件按常见位置探测：找不到只影响版本号与服务状态的展示，配置照样能读，
	// 面板照常能判冲突——**不会**因为版本读不到就说“这台机器上没有 Nginx”。
	nginxPath := firstExistingFile("/usr/sbin/nginx", "/usr/bin/nginx",
		"/usr/local/sbin/nginx", "/usr/local/bin/nginx", "/snap/bin/nginx")
	nginxOpts := nginx.Options{}
	if nginxPath != "" {
		nginxOpts.NginxPath = nginxPath
		nginxOpts.Version = agent.NginxVersionProbe(nginxPath)
	}
	if servicesCollector != nil {
		nginxOpts.Unit = func(ctx context.Context) (string, bool, error) {
			// 单元状态只用于两件事：一句告警（“服务没在跑，这次不会 reload”）和
			// 现状展示。服务采集器在后台每 10 秒采一遍，快照通常就在手边。
			//
			// **为什么读快照，而不是现场问 systemctl。** 后台那一轮完整采集在这台
			// 验收机上要 20 秒（503 个单元，按 200 个一段问），而 systemd 的属性查询
			// 会被插在它后面的客户端等着。实测（.tmp/probe2）：后台采集跑着的时候，
			// 现场问一次 `nginx.service` 用了 **6.389 秒**，紧接着的几次只要
			// 104–124 毫秒——快的几次是掉在两段之间的空档里。同一个探针里读快照：
			// 五次 113–252 毫秒，平均 170 毫秒。预检整条链路的预算是 10 秒
			// （agentclient.DefaultTimeout），6.4 秒的单元探测就能把它顶成
			// 503 AGENT_UNAVAILABLE——“代理不可用”会让人去查 socket 和权限，
			// 而那里什么都没坏。
			//
			// 一份最多十几秒旧的单元状态，对这两条判断完全够用。真到执行的那一刻，
			// reload 决策另有一条 `systemctl is-active` 现场问（applyrun.go 的
			// unitState），不看这份旧账。
			//
			// 教训：别用一个没验证的实验去推翻一个解释。先前用 shell 起了一个“大查询”
			// 再测，得到 0.2 秒，于是写下“systemd 不排队”——那个后台查询其实根本没
			// 跑起来（事后 `pgrep systemctl` 是 0），结论是空的。复现要复现整条路径。
			snap := servicesCollector.Snapshot()
			if snap.Status == services.StatusOK {
				for _, u := range snap.Units {
					if u.Unit == nginx.DefaultUnitName {
						return u.ActiveState, true, nil
					}
				}
				if snap.Truncated {
					// 快照被单元上限截断过，说“没有这个服务”就是假话。
					return "", false, errors.New("服务快照被截断，里面没有 " + nginx.DefaultUnitName +
						"：这不是“没有这个服务”，是没拿到")
				}
				return "", false, nil
			}
			// 还没采到过（刚启动、第一轮采集还没跑完）：这时候才现场问一次。
			unit, found, err := servicesCollector.Unit(ctx, nginx.DefaultUnitName)
			if err != nil {
				return "", false, err
			}
			return unit.ActiveState, found, nil
		}
	}
	nginxCollector := nginx.New(nginxOpts)
	logger.Info("已启用 Nginx 现状读取",
		"config_root", nginx.DefaultConfigRoot,
		"binary", nginxPath,
		"unit", nginx.DefaultUnitName,
		"scope", "只读：nginx.conf / conf.d / sites-enabled；不跟随其它 include")

	// 反向代理写入路径（P10.2）。前提是找得到 nginx 可执行文件：没有它就没法
	// `nginx -t`、也没法 reload，而“写一份不校验的配置进 conf.d”是这条流水线里
	// 最不能接受的一步。找不到时**不登记** proxy.apply，能力页如实说没有。
	var proxyApplier *proxies.Applier
	// errDiscoveryCold 表示“还没采过应用发现”。它是告警文案的来源，不是失败：
	// 上游目标检查只是一句提醒，没有快照就说没有快照，不为此去采一遍。
	errDiscoveryCold := errors.New("还没有采集过应用发现，暂时无从确认")
	if nginxPath != "" {
		applier, err := proxies.NewApplier(proxies.ApplierOptions{
			Facts:   nginxCollector,
			Secrets: secrets,
			Runner:  storage.SyscmdRunner(),
			Paths: proxies.ConfigPaths{
				Root:      nginx.DefaultConfigRoot,
				NginxPath: nginxPath,
			},
			TargetExists: func(ctx context.Context, id string) (bool, error) {
				// 发现结果里的目标才是“本机上真实存在的东西”。读不到发现结果时
				// 返回错误而不是 false：那会让一次正常上线被一句“目标不存在”挡下。
				//
				// **只读已有快照，不触发新的采集。** 上游目标在不在只是一句告警
				// （“这样上线会得到 502”），为一句可能过时的告警去扫 /proc、翻
				// systemd 单元清单、再问一遍 docker，是拿确定的东西（预检能返回）
				// 换不确定的东西（告警更准一点）。没采过就说“不知道”——那正是
				// 告警该说的话，不是错误的事实。
				//
				// 注意：这条改动**不是**“预检慢”的原因所在，改前改后的预检耗时
				// 都在同一档里。真正拖慢预检的是 nginx 现状采集里那条现场单元
				// 探测（见下面 nginxOpts.Unit 的注释与实测数据）。这条改动本身
				// 成立的理由更简单：一句告警不该买一次全量采集。
				snap, ok := discoveryCollector.Cached()
				if !ok {
					return false, errDiscoveryCold
				}
				_, found := snap.Targets[id]
				return found, nil
			},
		})
		if err != nil {
			logger.Error("初始化反向代理写入路径失败", "error", err)
			return 1
		}
		proxyApplier = applier
		logger.Info("已启用反向代理写入",
			"managed_pattern", filepath.Join(nginx.DefaultConfigRoot, "conf.d", nginx.ManagedPrefix+"<domain>.conf"),
			"auth_dir", filepath.Join(nginx.DefaultConfigRoot, proxies.AuthDirName),
			"backup_dir", proxies.DefaultBackupDir,
			"gate", "nginx -t 通过才保留；失败回滚并复验旧配置",
			"reload", "nginx -s reload（不 restart）")
	} else {
		logger.Warn("反向代理写入未启用",
			"reason", "找不到 nginx 可执行文件：无法 nginx -t 与 reload",
			"hint", "安装 nginx 后重启代理；只读的现状展示不受影响")
	}

	// 证书动作（P10.3）。前提是秘密存储与命令执行器都在：手动导入的私钥只存在于
	// 秘密存储里，签发与续期只能通过 certbot。两个都在时登记 certificate.import /
	// issue / renew 与 certificate.state；任何一个不在就不登记——capabilities 因此
	// 不会报告一个点了才报错的入口。
	//
	// webroot 与 ACME 目录可以配置（agent.certificates）：默认值就是 Let's Encrypt
	// 与 /var/www/upanel-acme，覆盖它们的唯一实际用途是私有 CA 与验收。
	certOps, err := certificates.NewOps(certificates.OpsOptions{
		Secrets:                secrets,
		Runner:                 storage.SyscmdRunner(),
		Now:                    nil, // 走真实时钟
		CertbotPath:            cfg.Agent.Certificates.CertbotPath,
		Webroot:                cfg.Agent.Certificates.ACMEWebroot,
		StagingDirectoryURL:    cfg.Agent.Certificates.StagingDirectoryURL,
		ProductionDirectoryURL: cfg.Agent.Certificates.ProductionDirectoryURL,
	})
	if err != nil {
		logger.Error("初始化证书模块失败", "error", err)
		return 1
	}
	logger.Info("已接上证书模块",
		"certbot", firstNonEmpty(cfg.Agent.Certificates.CertbotPath, tlscert.CertbotPath),
		"webroot", firstNonEmpty(cfg.Agent.Certificates.ACMEWebroot, tlscert.ACMEWebroot),
		"cert_dir", tlscert.CertDir,
		"staging_dir", firstNonEmpty(cfg.Agent.Certificates.StagingDirectoryURL, certificates.ACMEDirectoryStaging),
		"production_dir", firstNonEmpty(cfg.Agent.Certificates.ProductionDirectoryURL, certificates.ACMEDirectoryProduction),
		"scope", "certificate.import / issue / renew / state；staging 默认，不碰 nginx 插件")

	// APT 事实读取（P11.1）。只读：跑 dpkg-query / apt-cache policy、读
	// /var/lib/apt/lists 的 mtime、读重启标志与 boot_id。这一层不装包、不卸包、
	// 不刷新索引——那些是 P11.2 的动作，要预检、要锁、要审计。
	//
	// Available() 是登记条件：主机上没有 dpkg-query（非 Debian 系）时就不登记这两条
	// 查询，capabilities 于是不会报告一个点开必然 503 的入口。
	aptFacts := apt.New(apt.Options{})
	// aptWriter 在下面按需赋值：非 Debian 系主机上保持 nil，登记表就不会多出四个
	// 点了才报错的入口。
	var aptWriter *apt.Writer
	if aptFacts.Available() {
		logger.Info("已启用 APT 事实读取",
			"snapshot_ttl", apt.DefaultCacheTTL.String(),
			"dpkg_query", apt.DefaultDpkgQuery,
			"apt_cache", apt.DefaultAptCache,
			"scope", "只读：apt.packages / system.power_state；不安装、不卸载、不刷新索引")
		// 这里**故意不做**开机预热采集。试过了，然后量出来的结论是它帮倒忙：
		// 开机那一分钟里服务/进程/磁盘几个采集器正在密集跑 systemctl，同一时间再压一份
		// 26 秒的 apt-cache，两边的 CPU 和 IO 互抢，预热实测从 26 秒拖到 62 秒；而
		// Packages 是单飞的——用户这时点开"更新"页，请求会排队等这份 62 秒的预热，
		// 越过面板 45 秒的查询预算，于是页面报"代理不可用"，而事实其实此刻正在被读。
		// 不预热的话，第一次请求自己采一遍（24.04 实测 21 秒，与后台采集器并行时的时长），
		// 落在预算内；之后就都是 0.3 秒的缓存命中。
		// 慢一次比"在最需要它的那一次谎报不可用"好。
		// 写事务（P11.2）。与只读采集共用同一套路径常量，但**单独一个 Writer**：
		// 它的状态是锁探测与 dpkg 快照，与只读采集的 30 秒缓存没有关系。
		//
		// Available() 要求 apt-get 与 dpkg-query 都在：只有 apt-get 时写事务跑得起来
		// 却核对不了结果——那正是本阶段拒绝的"命令成功就算成功"。
		aptWriter = apt.NewWriter(apt.WriterOptions{})
		if aptWriter.Available() {
			logger.Info("已启用 APT 写事务",
				"apt_get", apt.DefaultAptGet,
				"locks", apt.DefaultPaths().LockPaths(),
				"scope", "package.refresh / install / remove / upgrade；预检模拟、保护名单、apt-global 锁、执行后逐包核对")
		} else {
			logger.Warn("APT 写事务未启用",
				"reason", "找不到可执行的 apt-get 或 dpkg-query",
				"hint", "package.* 四个写动作不会出现在能力表里")
			aptWriter = nil
		}
	} else {
		logger.Warn("APT 事实读取未启用",
			"reason", "找不到可执行的 "+apt.DefaultDpkgQuery,
			"hint", "这台机器不是 Debian 系发行版；包管理相关入口不会出现在能力表里")
	}

	// 系统电源（P11.3）。登记条件只有一条：找得到 systemctl。boot_id 与 /run/systemd/system
	// 是运行时事实，留给预检判——装配时判一次就永远相信，正是本轮要避免的姿态。
	// 倒计时由代理持有（不是立刻下发再让页面倒数），所以“取消”是真的；见 internal/power。
	powerExec := newPowerExecutor(powerDeps)
	if powerAvailable(powerExec) {
		logger.Info("已启用系统电源动作",
			"systemctl", powerExec.Paths().Systemctl,
			"boot_id", powerExec.Paths().BootID,
			"scope", "system.power：reboot / poweroff，延迟 30—300 秒，倒计时在代理里、期间可取消；"+
				"执行结果只报 accepted，重启是否真的发生由下次开机后的 boot_id 比对回答")
	} else {
		logger.Warn("系统电源动作未启用",
			"reason", "找不到可执行的 systemctl",
			"hint", "system.power 不会出现在能力表里；这台机器上重启/关机请用命令行")
		powerExec = nil
	}

	// 备份仓库（P12.1）。装配条件只有一条：这台机器上有 restic。
	//
	// 为什么不把“restic 缺失”留成预检阻塞项而干脆不登记：能用 restic 之前的每一次点击
	// 都只能落在一个会被拒绝的预检上，而能力表会一直显示“我能初始化仓库”。装好 restic
	// 之后重启代理，这两个入口才会出现——这与 apt/docker 的门槛是同一条规矩。
	//
	// ssh 缺失不阻止登记：本地后端不需要它，SFTP 定义会在预检里得到
	// BACKUP_SSH_MISSING 这条明确阻塞项。
	backupPaths := backup.DefaultPaths()
	resticRunner := backup.NewRestic(backupPaths, nil)
	var backupModule *backup.Module
	if resticOK, resticWhy := resticRunner.Available(nil); resticOK {
		backupOps, err := backup.NewOps(backup.Options{
			Restic:        resticRunner,
			Roots:         backupRoots(fileRoots(cfg.Agent.Files.Roots)),
			Secrets:       secrets,
			Paths:         backupPaths,
			Restores:      backupRestoreFS{manager: filesManager},
			Verifications: verificationStore,
		})
		if err != nil {
			logger.Error("初始化备份模块失败，拒绝启动", "error", err)
			return 1
		}
		// 面板库副本目录：配置里给了就用它，否则放在代理 StateDir 下。
		panelCopyDir := cfg.Agent.Backup.PanelCopyDir
		if panelCopyDir == "" {
			panelCopyDir = filepath.Join(backupPaths.StateDir, "panel-copies")
		}
		// 停/起应用的能力（P12.2）：服务与容器两个来源各自可能不可用（没有
		// systemctl / 没有 docker）。这里始终接上——不可用的那一路由控制器在
		// Inspect 时如实报"读不到"，预检于是把 stop_managed_apps 的策略挡住，
		// 而不是备份到一半才发现停不下来。
		appController := &agent.BackupAppController{
			Services:   servicesCollector,
			Containers: containersClient,
		}
		backupModule = &backup.Module{
			Ops:               backupOps,
			Repos:             backupStore,
			Policies:          policyStore,
			Apps:              appController,
			PanelDatabasePath: cfg.Agent.Backup.PanelDatabasePath,
			PanelCopyDir:      panelCopyDir,
		}
		logger.Info("已启用备份仓库与备份运行",
			"restic", backupPaths.Restic,
			"ssh", backupPaths.SSH,
			"state_dir", backupPaths.StateDir,
			"cache_dir", backupPaths.CacheDir,
			"run_dir", backupPaths.RunDir,
			"roots", len(cfg.Agent.Files.Roots),
			"panel_database", cfg.Agent.Backup.PanelDatabasePath,
			"panel_copy_dir", panelCopyDir,
			"app_control_services", servicesCollector != nil,
			"app_control_docker", dockerAvailable,
			"scope", "backup.repository.initialize / backup.repository.state / backup.run / "+
				"backup.snapshots / backup.snapshot；本地与 SFTP，目录必须是已登记文件根；"+
				"仓库已存在时拒绝覆盖；建完用 restic cat config 核对；"+
				"快照建完用 restic stats 读回核对")
	} else {
		logger.Warn("备份仓库动作未启用", "reason", resticWhy,
			"hint", "apt install restic 后重启代理；备份相关入口不会出现在能力表里")
	}

	// 计划任务与受管理脚本（P12.4）。两者成对：脚本只有被计划指向才会被执行，
	// 而删脚本前要问"还有没有计划在引用它"。
	//
	// 单元落地端缺一样工具（systemctl / systemd-analyze）就整块不启用：定义能存下来、
	// 单元却装不上，界面只好显示"已保存、未生效"——那种半残状态比能力表里干脆没有
	// 这个入口更让人误解。缺席是诚实的。
	scheduleStore, err := agentstore.NewScheduleStore(agentDB)
	if err != nil {
		logger.Error("初始化计划任务存储失败，拒绝启动", "error", err)
		return 1
	}
	scriptStore, err := agentstore.NewScriptStore(agentDB, "")
	if err != nil {
		logger.Error("初始化脚本存储失败，拒绝启动", "error", err)
		return 1
	}
	var schedulesModule *schedules.Module
	var scriptsModule *scripts.Module
	if unitInstaller, installErr := schedules.NewInstaller(schedules.InstallerOptions{}); installErr != nil {
		logger.Warn("计划任务能力未启用", "reason", installErr,
			"hint", "计划任务依赖 systemctl 与 systemd-analyze；这两样齐了重启代理即可")
	} else {
		schedulesModule, err = schedules.NewModule(schedules.ModuleOptions{
			Store: scheduleStore,
			Units: unitInstaller,
			// 目标的版本与名字只从代理库现取：计划定义里不保存版本，
			// 否则一条老计划每次触发都会拿着老版本撞乐观并发。
			Lookup: scheduleTargetLookup(policyStore, scriptStore),
		})
		if err != nil {
			logger.Error("初始化计划任务模块失败，拒绝启动", "error", err)
			return 1
		}
		scriptsModule, err = scripts.NewModule(scripts.Options{
			Store: scriptStore,
			// 删除前的引用检查接的是计划模块：有脚本被引用时宁可拒绝删除，
			// 也不留下一条指向不存在脚本的计划。
			Refs: schedulesModule,
			// 目录穿越权限被修补时留痕：修了什么要能在日志里找到。
			Logger: logger,
		})
		if err != nil {
			logger.Error("初始化脚本模块失败，拒绝启动", "error", err)
			return 1
		}
		logger.Info("已启用计划任务与受管理脚本",
			"unit_dir", unitInstaller.Dir(),
			"script_dir", scriptsModule.Dir(),
			"scope", "schedule.apply / schedule.delete（写 systemd 单元、systemd-analyze 校验日历）/ "+
				"schedule.trigger（仅 root 经 upanelctl 触发，稳定触发 ID）/ "+
				"script.apply / script.delete / script.run（以 "+scripts.ShellUser+" 降权，解释器固定 /bin/sh，"+
				"运行前核对落盘哈希）")
	}

	// Compose 部署（P15.2）：一份定义 → 一个渲染好的 Compose 配置 + 一个包装单元，
	// 由 systemd 负责这套部署的整个生命周期（PLAN §11 规则 4：容器不自己重启，
	// 开机也由 systemd 拉起，因此不存在“卷还没挂上、docker 先把容器拉起来”的窗口）。
	//
	// 三样缺一不接，而且都在**启动时**判：
	//   systemctl —— 单元的落地与读回都靠它；找不到就是 DEPENDENCY_MISSING，
	//                那种动作在提交之后才报错，而那时单元文件已经落了一半；
	//   docker    —— 单元里的 ExecStart 用绝对路径调它（deploy.DockerBinary）；
	//   秘密存储  —— 配置里的 app_env 只能从密钥环解出来，没有它带秘密的定义一律拒绝。
	// 缺任何一样就不登记：capabilities 于是如实说这台机器不能部署，而不是留一个
	// 点下去才报错的入口。
	networkSource := agent.NewNetworkSource()
	var deployService *deploy.Service
	// deployInspector 是只读现场（P15.4），与 deployService（三个写动作）分开两个开关：
	// 读现场只要 systemctl 与配置目录，写动作还要 docker。一台 docker 暂时不在的机器
	// 仍然该说得出"这台机器上部署过什么、它们现在什么状态"——那是排障时最想知道的，
	// 而把它锁在写路径后面，人就只剩 SSH 里翻文件这一条路。
	var deployInspector agent.DeployInspector
	deployApplier, deployApplierErr := deploy.NewApplier(deploy.ApplierOptions{})
	switch {
	case deployApplierErr != nil:
		logger.Warn("部署未启用（读与写都不登记）", "reason", deployApplierErr,
			"hint", "单元落地与单元读回都需要 systemctl（/usr/bin/systemctl 或 /bin/systemctl），装好后重启代理")
	default:
		// 更新前备份（P15.3）只在备份模块装配成功时接上：没有 restic、没有仓库镜像的
		// 机器上，带 backup_policy_id 的更新会在预检里被挡住并说清原因，而不是被当成
		// “已经备份过了”照常换镜像。
		var deployBackup deploy.BackupCause
		if backupModule != nil {
			deployBackup = agent.NewDeployBackup(backupModule)
		}
		service, serviceErr := deploy.NewService(deploy.ServiceOptions{
			Applier: deployApplier,
			// 秘密解析器核对着用途才解密：一条部署定义不能把别人的 TLS 私钥
			// 当成环境变量塞进容器里再读出来。
			Resolver: agent.DeploySecretResolver{Secrets: secrets},
			// 宿主事实每次现取：预检与执行前复核是两个时刻，卷可能刚被卸下。
			Env: agent.DeployFacts{
				Roots:     filesManager,
				Volumes:   resourceStore,
				Topology:  storageCollector.Topology,
				Addresses: agent.InterfaceAddressesFunc(networkSource),
				DataBase:  storage.DefaultVolumeRoot,
			}.HostEnv,
			Backup: deployBackup,
		})
		if serviceErr != nil {
			logger.Error("初始化部署服务失败，部署的读写都不会登记", "error", serviceErr)
		} else {
			// 读现场无条件接上；写动作再按 docker 是否在决定。
			deployInspector = service
			if firstExistingFile(deploy.DockerBinary) == "" {
				logger.Warn("部署动作未启用（只读现场仍然可用）",
					"reason", "找不到 "+deploy.DockerBinary+"（单元里的 ExecStart 用绝对路径调用它）",
					"hint", "apt install docker.io 或 docker-ce；snap 版 docker 不在这个路径上")
				logger.Info("已启用 Compose 部署现场读取",
					"config_dir", deployApplier.ConfigDir(),
					"queries", "deployments.list / deployments.get（只读）",
					"scope", "读盘上三个文件与 systemd 的 enable/active 回答；"+
						"不拉镜像、不查 docker、不探测健康")
				break
			}
			deployService = service
			backupScope := "未接备份：带 backup_policy_id 的更新会被预检挡住"
			if deployBackup != nil {
				backupScope = "更新前可按策略做一次 files_only 备份并读回核对（未建成就不换镜像）"
			}
			logger.Info("已启用 Compose 部署",
				"unit_dir", deployApplier.Dir(),
				"config_dir", deployApplier.ConfigDir(),
				"data_base", storage.DefaultVolumeRoot,
				"version", "创建、更新与删除（deployment.apply / deployment.update / deployment.remove）",
				"scope", "定义 → 渲染 Compose 配置（0600）+ systemd 包装单元（0644）；"+
					"落地后用 systemctl is-enabled/is-active 读回，不以 start 退出码当成功；"+
					"删除保留数据卷与业务目录；更新换镜像后做面板侧健康探测，探不通回切配置与镜像"+
					"（数据不跟着回切）；"+backupScope)
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
		Shares:     sharesEngine,
		// 受管理账号（P14.5）。它复用 shares 的判决与报告形状，但需要的系统工具
		// 与 smb.conf 完全是另一套，所以是另一个开关：一台只装 Samba 的机器不该
		// 被登记成“能建账号”。
		Accounts:   accountsEngine,
		Discovery:  discoveryCollector,
		Containers: containersClient,
		Nginx:      nginxCollector,
		// 证书（P10.3）。上面已经构造好；把它接进来，三个写动作与一条查询才会登记。
		Certificates: certOps,
		// APT 事实读取（P11.1）。Available() 为假时登记表里不会出现这两条查询。
		Apt: aptFacts,
		// 网络与防火墙的只读采集（P14.1）。文件系统与 /proc、/sys 的读取是进程内的，
		// 只需接上 ip/ufw 的命令执行端；两个程序不在时仍能出网卡清单与监听端口，
		// 只是地址/防火墙那块带一条 missing。四个查询没有任何写入口。
		//
		// 与部署共用同一份采集器：面板上显示的地址与“能不能绑这个地址”必须是同一份数据。
		Network: networkSource,
	}
	if aptWriter != nil {
		deps.AptWrites = aptWriter
	}
	// 同上：只在真的有时才赋给接口，否则能力表会多出一个调用就 panic 的入口。
	if powerExec != nil {
		deps.Power = powerExec
	}
	// 备份仓库（P12.1）：restic 存在时才有实现端。
	if backupModule != nil {
		deps.Backup = backupModule
		// P12.2：运行端与快照查询。同一台机器上，能初始化仓库不等于能跑策略——
		// 但两者都要求 restic，所以登记条件一样，只是前者还额外依赖策略镜像。
		deps.BackupRun = backupModule
	}
	// 只在真的有时才赋给接口：nil 的 *proxies.Applier 塞进接口会变成一个非 nil
	// 的接口值，能力表于是多出一个调用就 panic 的入口。
	if proxyApplier != nil {
		deps.Proxies = proxyApplier
	}
	// 计划任务与脚本（P12.4）：同样按"有没有实现端"决定登记与否。
	if schedulesModule != nil {
		deps.Schedules = schedulesModule
	}
	if scriptsModule != nil {
		deps.Scripts = scriptsModule
	}
	// 部署动作（P15.2）：只在 systemctl、docker 与秘密存储都接上时才有实现端。
	if deployService != nil {
		deps.Deployments = deployService
	}
	// 部署现场（P15.4）：只要 systemctl 与配置目录在就有。它是查询，不是动作——
	// supported_queries 说有、点开却报错，比没有这条入口更糟。
	if deployInspector != nil {
		deps.DeployInspector = deployInspector
	}
	// 只在真的有时才赋给接口：把一个 nil 的 *terminal.Manager 塞进接口会得到
	// 一个非 nil 的接口值，登记表于是多出一个调用就 panic 的能力。
	var terminalAPI agent.TerminalManager
	if terminalManager != nil {
		deps.Terminal = terminalManager
		terminalAPI = terminalManager
	}
	// 网络配置写（P14.2）：默认不接，只认显式开关。
	//
	// STAGES §16 把这条路绑在一条硬证据上——"在独立测试 VM 里故意配置不可达地址、断开浏览器
	// 并停掉 Web 进程，120 秒后仍能自动恢复"。那条证据落进 docs/evidence/P14/ 之前，
	// 写路径必须保持关闭：改错一份 netplan 能把机器从网络上摘掉，而面板本身跑在网络上，
	// 救不回来就没有第二次机会。开关只为测试 VM 存在。
	var netEngine *network.ApplyEngine
	if *netWrites {
		engine, err := agent.NewNetworkApplyEngine(res.Source)
		if err != nil {
			logger.Error("初始化网络写动作失败", "error", err)
			return 1
		}
		netEngine = engine
		deps.NetworkWrites = engine
		logger.Warn("已启用网络配置写动作：仅用于测试虚拟机；回滚窗口 120 秒，未确认即恢复原配置",
			"action_scope", "network.apply / network.confirm / network.rollback")
	}
	// 防火墙写（P14.3）：与网络写分开的开关，默认同样不接。
	//
	// 为什么也要一个开关：改规则能把自己关在门外，而面板就在门里面。这条路自带三重
	// 保险（预检拦管理通路、改完读回来核验、120 秒独立 timer），但"要不要给它这个
	// 权力"仍然是装机时由人决定的事，不因为验收过了就默认打开。
	var fwEngine *network.FirewallEngine
	if *fwWrites {
		engine, err := agent.NewFirewallApplyEngine(res.Source)
		if err != nil {
			logger.Error("初始化防火墙写动作失败", "error", err)
			return 1
		}
		fwEngine = engine
		deps.FirewallWrites = engine
		logger.Warn("已启用防火墙写动作：回滚窗口 120 秒，未确认即恢复原规则集",
			"action_scope", "firewall.apply / firewall.confirm / firewall.rollback")
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

	// 外部程序能力探测（P16.1）。它回答的是“这台机器上到底装了什么、版本是多少”，
	// 与“某个动作能不能做”是两件事：后者仍由各子系统自己的预检判决。
	//
	// 缓存而不是每次现探：capabilities 是一条随时会被读的端点，让每次读都等
	// 十几条版本命令，等于把一个便宜的事实变成昂贵的依赖。探测在 ctx 起好之后
	// 由后台跑第一次，启动不等它。
	hostCaps := hostcaps.NewCache(hostcaps.Deps{}, hostcaps.DefaultInterval)

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
		// P09.3：容器日志流。与查询共用同一个客户端，可用性判决也只有一份。
		Containers: containersClient,
		// P08.3：运行时脱盘监视。间隔取默认值（30 秒），循环自己拿 samba:config 锁。
		ShareWatch: sharesEngine,
		// P12.1：仓库定义镜像。给的是存储层本身（不是 Module）：镜像的读写与
		// “能不能在这台机器上初始化”是两件事，后者由 deps.Backup 决定。
		BackupRepositories: backupStore,
		// P12.2：策略镜像。与仓库镜像同一个判断：写镜像不等于能执行。
		BackupPolicies: policyStore,
		// P12.4：计划与脚本的镜像读路由。给的是实现端本身：GET 要的
		// "systemd 说它现在是什么样"只有实现端能回答。
		Schedules: schedulesModule,
		Scripts:   scriptsModule,
		// P14.6：二次验证种子的生成、校验与销毁。给的是秘密存储本身——种子的明文
		// 只在这个进程里解开，Web 拿到的永远只是 sec_ 引用与一次性的 otpauth URI。
		Totp: secrets,
		// P16.1：外部程序能力的探测缓存。nil 时 capabilities 会如实报“没有读数”，
		// 而不是一个看起来像“这台机器什么都没有”的空数组。
		HostCaps: hostCaps,
	})
	if err != nil {
		logger.Error("初始化代理失败", "error", err)
		return 1
	}
	// 到这里执行器才存在：system.power 的“关键任务”判定从此有来源。
	// 之前它返回错误 → 预检拒绝 → 不会出现“没检查就重启”。
	bindPower(powerDeps, srv)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 开机对账：上一轮如果留下一条未确认的网络改动，先按记录里的 deadline 重判一次。
	// 必须做，因为 timer 是 OnActiveSec——机器重启后它从零开始计时，不重装就等于
	// 把回滚窗口延长了一整个开机时长；已经过期的则立刻回滚，不等新的 120 秒。
	if netEngine != nil {
		if err := netEngine.Reconcile(ctx); err != nil {
			// 对账失败不挡住代理启动：代理起来至少还能让页面显示出这条 pending 记录
			// 和它的到期时间，让运维知道机器还悬在回滚窗口里。
			logger.Error("网络回滚对账失败", "error", err)
		}
	}
	// 防火墙这边同理，且多一层理由：ufw 的规则是"开机就在"的，一次没确认的改动
	// 会在重启后依然是现场的规则——不在这里重判一次，那条 pending 记录就再也
	// 没有机会自己退回去了。
	if fwEngine != nil {
		if err := fwEngine.Reconcile(ctx); err != nil {
			logger.Error("防火墙回滚对账失败", "error", err)
		}
	}

	// 采集循环：立刻采一次，之后每 5 秒一次。它们只是读 /proc 与 /sys，不该挡住监听，
	// 所以放 goroutine；ctx 结束时一并退出。
	// 能力探测：立刻探一次，之后每 10 分钟重探一次。装完一个包不必重启代理，
	// 下一次刷新就能看到它出现；探不到就继续用上一份读数，不把界面清空。
	hostCaps.Start(ctx)
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

// firstNonEmpty 返回第一个非空值（用于日志里显示“配置值或默认值”）。
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
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

// backupRestoreFS 把文件模块的句柄级恢复能力适配给 backup，而不暴露任意绝对路径写接口。
type backupRestoreFS struct{ manager *files.Manager }

func (r backupRestoreFS) ProbeRestore(ctx context.Context, params backup.RestoreParams) (backup.RestoreProbe, error) {
	return r.manager.ProbeRestore(ctx, params)
}
func (r backupRestoreFS) BeginRestore(ctx context.Context, params backup.RestoreParams) (backup.RestoreSession, error) {
	return r.manager.BeginRestore(ctx, params)
}

// backupRoots 把已登记的文件根给备份模块当 RootSource（P12.1）。
//
// 用同一份配置（agent.files.roots）而不是另一张表：备份仓库只能落在面板已经
// 接管、并且文件模块也在用的根里。多一张表就等于多一条“文件页看不见、备份却往
// 里写”的隐藏路径。
type backupRoots []files.Root

// ResolveRoot 解析根标识。
func (r backupRoots) ResolveRoot(rootID string) (string, bool, bool) {
	for _, root := range r {
		if root.ID == rootID {
			return root.Path, root.Writable, false
		}
	}
	return "", false, true
}

// discoveryUnits 把 P05.1 的服务单元快照接到发现模块上。
//
// 一条独立的 systemctl 通道会得到与「服务」页面不同的单元集合与不同的时间点，
// 于是同一台机器在两个页面里说着两种话。这里只读同一份快照。
//
// 快照失败时返回 ok=false：此时监听照常被发现，但不会出现 systemd 候选——
// 把裸监听归到一个单元名下就是在编造。
type discoveryUnits struct {
	collector *services.Collector
}

func (d discoveryUnits) Units() ([]discovery.UnitInfo, bool, string) {
	if d.collector == nil {
		return nil, false, "本机没有可用的 systemctl，无法把监听关联到服务单元"
	}
	snap := d.collector.Snapshot()
	if snap.Status != services.StatusOK {
		return nil, false, snap.Error
	}
	out := make([]discovery.UnitInfo, 0, len(snap.Units))
	for _, u := range snap.Units {
		out = append(out, discovery.UnitInfo{
			Unit:        u.Unit,
			Description: u.Description,
			ActiveState: u.ActiveState,
			MainPID:     u.MainPID,
		})
	}
	return out, true, ""
}

// filesCapacity 把存储模块的容量读取器接到文件模块上。
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

// firstExistingFile 返回第一个存在的普通文件路径，全都不存在时返回空串。
//
// 用它来探测 nginx 可执行文件的位置：发行版把它放在 /usr/sbin，自编译的常在
// /usr/local，snap 装的在 /snap/bin。探不到不是错误——配置照样能读，
// 只是版本号与服务状态显示为“未知”。
func firstExistingFile(candidates ...string) string {
	for _, path := range candidates {
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			continue
		}
		return path
	}
	return ""
}
