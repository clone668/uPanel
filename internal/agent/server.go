// Package agent 是特权代理的服务端实现：只监听 Unix socket，只提供登记过的动作与查询。
//
// 边界来自 PLAN.md 第 4 节与 CONTRACTS.md 第 15 节：
//   - 以 root 运行，只监听 Unix socket，用 SO_PEERCRED 校验对端实际 UID；
//   - 普通接口只允许 Web 服务账号（默认 upanel-web），root 只能走单独限定的专用路由；
//   - 只提供登记过的动作与结构化只读查询，绝不提供 execute(command)、任意 argv、
//     任意路径读写或万能转发给代理 HTTP 接口的入口。
//
// P02.3 提供：对端 UID 校验、固定登记表（动作 + 查询）、POST /internal/v1/queries
// 的只读查询通道，以及命令执行器（package syscmd）的时限/输出上限机制。
// P02.4 增加 POST /internal/v1/secrets：用途闭集、AEAD 加密落库、返回引用且没有任何
// 读取明文的路由；临时秘密 10 分钟后由扫描器销毁。
// P03.1 增加 POST /internal/v1/preflights：动作的预检器生成规范化参数、目标身份指纹、
// 状态版本、影响/阻塞项与确认短语，计划落进代理库并在 120 秒后失效；预检本身不写系统。
// 生产登记表仍然为空：第一个真实动作属于 P04，验收用的动作在独立的验收登记表里。
// P05.3 增加 POST/GET /internal/v1/streams：一次性流许可 + 单次受限流（日志实时跟随）。
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"upanel/internal/httpx"
	"upanel/internal/platform"
	"upanel/internal/version"
)

// SocketMode 是代理 socket 的权限（PLAN.md 第 4 节：root:upanel-web，0660）。
const SocketMode os.FileMode = 0o660

// Options 是构造代理服务的参数。
type Options struct {
	// SocketPath 必须是绝对路径，且位于启动前已存在、root 拥有的目录下。
	SocketPath string
	// Version 为构建版本信息。
	Version version.Info
	// Logger 必须非空。
	Logger *slog.Logger
	// ShutdownTimeout 为优雅停止等待时间。
	ShutdownTimeout time.Duration
	// Peer 是对端 UID 规则；WebUID 不得为 0。
	Peer PeerPolicy
	// Registry 是登记表；为 nil 时使用生产登记表（当前为空）。
	Registry *Registry
	// Secrets 是加密秘密存储（P02.4）。必须非空：没有密钥就解不开已有秘密，
	// 带着“能启动但写不进秘密”的状态运行等于假装功能可用。
	Secrets SecretStore
	// Plans 是预检计划存储（P03.1）。必须非空：预检由代理生成并落库，
	// 少了它预检只能返回一份不会被任何执行路径认可的计划。
	Plans PlanStore
	// Operations 是执行记录存储（P03.2）。必须非空：接受一次操作必须先落库，
	// 否则“重启后仍能按 operation_id 查到”就是空话。
	Operations OperationStore
	// Logs 是日志读取端（P05.3）。为 nil 时不提供 logs.query 与日志流：
	// capabilities 与实际可用能力始终一致，不会报告一个调用就 501 的查询。
	Logs LogsSource
	// Files 是文件读取端（P06.1）。为 nil 时不提供 files.download 流；
	// files.roots/list/text 由登记表报告（同样的“给什么才登记什么”）。
	Files FilesSource
	// FileWrites 是文件写端（P06.2）。为 nil 时不提供 /internal/v1/uploads 路由，
	// 也不登记 file.* 写动作：一个只能读的面板与一个读不了却能写的面板，
	// 都不该由“某个字段恰好没配”来悄悄决定。
	FileWrites FileWriterSource
	// Terminal 是受限终端的会话层（P06.3）。为 nil 时不提供 terminal.io 流与
	// 会话关闭入口，也不登记 terminal.create / terminal.sessions。
	Terminal TerminalManager
	// Containers 是容器读取端（P09.3）。为 nil 时不提供容器日志流：查询与启停
	// 由登记表报告，流用途由这里决定，两者都不允许“登记了却走不通”。
	Containers ContainersSource
	// ShareWatch 是运行时卷监视（P08.3）。为 nil 时不起循环：没有共享模块就没有
	// “共享所在的卷脱盘”这件事要处理，而不是起一个什么都不做的循环去占锁。
	ShareWatch sharesReconciler
	// ShareWatchInterval 是监视间隔；<=0 时用 DefaultShareWatchInterval。
	ShareWatchInterval time.Duration
}

// Server 是代理服务。
type Server struct {
	opts      Options
	startedAt time.Time
	registry  *Registry
	runner    *runner
	// streams 是一次性流许可表（P05.3）。只存在内存里：许可寿命 30 秒，
	// 跨重启保留一个“别人可能已经拿到”的凭据只会让“一次性”变成空话。
	streams *streamStore
	http    *http.Server
}

// capabilities 是 GET /internal/v1/capabilities 的响应体。
//
// supported_actions / supported_queries 直接来自登记表：当前都是空数组，
// 如实反映“还没有开放任何动作与读取”。
type capabilities struct {
	Version          string    `json:"version"`
	Commit           string    `json:"commit"`
	BuildDate        string    `json:"build_date"`
	GoVersion        string    `json:"go_version"`
	Mode             string    `json:"mode"`
	OS               string    `json:"os"`
	Arch             string    `json:"arch"`
	StartedAt        string    `json:"started_at"`
	SupportedActions []string  `json:"supported_actions"`
	SupportedQueries []string  `json:"supported_queries"`
	Dependencies     []depInfo `json:"dependencies"`
}

// depInfo 描述代理依赖的外部程序。当前尚未探测，返回空数组。
type depInfo struct {
	ID      string `json:"id"`
	State   string `json:"state"`
	Version string `json:"version"`
	Path    string `json:"path"`
}

// New 构造代理服务。参数不合法时返回错误。
func New(opts Options) (*Server, error) {
	if opts.Logger == nil {
		return nil, errors.New("agent: Logger 不能为空")
	}
	if opts.SocketPath == "" {
		return nil, errors.New("agent: SocketPath 不能为空")
	}
	if !filepath.IsAbs(opts.SocketPath) {
		return nil, fmt.Errorf("agent: SocketPath 必须是绝对路径，当前为 %q", opts.SocketPath)
	}
	if opts.ShutdownTimeout <= 0 {
		return nil, errors.New("agent: ShutdownTimeout 必须为正数")
	}
	if err := opts.Peer.validate(); err != nil {
		return nil, err
	}
	if opts.Secrets == nil {
		return nil, errors.New("agent: Secrets 不能为空（秘密存储是固定能力，不允许缺省）")
	}
	if opts.Plans == nil {
		return nil, errors.New("agent: Plans 不能为空（预检计划是固定能力，不允许缺省）")
	}
	if opts.Operations == nil {
		return nil, errors.New("agent: Operations 不能为空（执行记录是固定能力，不允许缺省）")
	}
	registry := opts.Registry
	if registry == nil {
		// 没有显式登记表时用生产表。缺省情况下 deps 为空，于是生产表里只有
		// “不依赖外部实现”的能力；依赖采集器的查询由调用方显式传入。
		production, err := NewProductionRegistry(ProductionDeps{})
		if err != nil {
			return nil, err
		}
		registry = production
	}

	s := &Server{opts: opts, startedAt: time.Now().UTC(), registry: registry, streams: newStreamStore()}
	s.runner = newRunner(s)

	mux := http.NewServeMux()
	mux.HandleFunc("/internal/v1/capabilities", s.handleCapabilities)
	mux.HandleFunc("/internal/v1/queries", s.handleQueries)
	mux.HandleFunc("/internal/v1/preflights", s.handlePreflights)
	mux.HandleFunc("/internal/v1/operations", s.handleOperations)
	mux.HandleFunc("/internal/v1/operations/", s.handleOperationByID)
	mux.HandleFunc("/internal/v1/secrets", s.handleSecrets)
	// 受限流（P05.3）：先建许可，再用许可开一条一次性的流。
	mux.HandleFunc("/internal/v1/streams", s.handleStreams)
	mux.HandleFunc("/internal/v1/streams/", s.handleStreamByID)
	// 暂存上传（P06.2）：字节先进根内的暂存区，落地另有预检与任务。
	mux.HandleFunc("/internal/v1/uploads", s.handleUploads)
	mux.HandleFunc("/internal/v1/uploads/", s.handleUploadByIDOrContent)
	// 终端会话的关闭入口（P06.3）。创建走 terminal.create 任务，字节走
	// terminal.io 流；只有“关掉它”需要一条代理内部路由——Web 核对完归属之后
	// 让代理执行，代理再核对一次。
	mux.HandleFunc("/internal/v1/terminal/sessions/", s.handleTerminalSessionByID)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteError(w, r, http.StatusNotFound, httpx.CodeNotFound, "代理接口不存在")
	})

	s.http = &http.Server{
		// gate 位于最外层：任何路径（包括未知路径）都先校验对端 UID。
		Handler:           s.gate(mux),
		ConnContext:       connContext,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		// WriteTimeout 对长连接是非法的默认值：日志流（P05.3）在每次写之前用
		// http.ResponseController 把截止时间往后推，这里保留一个短值兜住普通请求。
		WriteTimeout:   15 * time.Second,
		IdleTimeout:    60 * time.Second,
		MaxHeaderBytes: 64 << 10,
		ErrorLog:       slog.NewLogLogger(opts.Logger.Handler(), slog.LevelWarn),
	}
	return s, nil
}

// Handler 返回路由，供测试使用。
func (s *Server) Handler() http.Handler { return s.http.Handler }

// nowUTC 返回代理当前时间（UTC）。预检的创建时间与到期时间都以它为准。
func (s *Server) nowUTC() time.Time { return time.Now().UTC() }

func (s *Server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		httpx.WriteError(w, r, http.StatusMethodNotAllowed, httpx.CodeValidationError, "只支持 GET")
		return
	}
	httpx.WriteData(w, r, http.StatusOK, capabilities{
		Version:   s.opts.Version.Version,
		Commit:    s.opts.Version.Commit,
		BuildDate: s.opts.Version.BuildDate,
		GoVersion: s.opts.Version.GoVersion,
		Mode:      platform.ModeProduction.String(),
		OS:        platform.Current().OS,
		Arch:      platform.Current().Arch,
		StartedAt: s.startedAt.Format(time.RFC3339),
		// 只报有执行器的动作：能预检但还不能执行的动作（P07.1 的存储动作）会给出
		// 真实的拒绝原因，但提交一定 501，列进这里就是能力撒谎。见 ExecutableActionNames。
		SupportedActions: s.registry.ExecutableActionNames(),
		SupportedQueries: s.registry.QueryNames(),
		Dependencies:     []depInfo{},
	})
}

// Serve 在 Unix socket 上提供服务，直到 ctx 结束。
//
// 启动前依次校验（任一失败都拒绝启动，不降级运行）：
//  1. 平台是 Linux 且以 root 运行；
//  2. socket 所在目录存在；
//  3. 若目标路径已存在，必须是 socket 才能被安全替换，其他类型一律拒绝删除；
//  4. socket 建立后设置为 root:<SocketGID> 0660，并回读确认（不接受“命令没报错”当作成功）。
func (s *Server) Serve(ctx context.Context) error {
	if err := platform.CheckAgent(); err != nil {
		return err
	}
	if err := s.prepareSocketPath(); err != nil {
		return err
	}
	// 目录权限不对会让 Web 账号根本连不到 socket，必须在启动时就说清楚，
	// 而不是等到第一次调用时神秘地“连不上”。
	if err := verifySocketDir(filepath.Dir(s.opts.SocketPath), s.opts.Peer.SocketGID); err != nil {
		return err
	}

	raw, err := net.Listen("unix", s.opts.SocketPath)
	if err != nil {
		return fmt.Errorf("监听 Unix socket 失败 %s: %w", s.opts.SocketPath, err)
	}
	ln := peerListener{Listener: raw}

	if err := s.secureSocket(); err != nil {
		_ = raw.Close()
		_ = os.Remove(s.opts.SocketPath)
		return err
	}

	s.opts.Logger.Info("代理已开始监听",
		"socket", s.opts.SocketPath,
		"socket_mode", fmt.Sprintf("%#o", SocketMode),
		"socket_gid", s.opts.Peer.SocketGID,
		"allowed_web_user", s.opts.Peer.WebUser,
		"allowed_web_uid", s.opts.Peer.WebUID,
		"uid", os.Geteuid(),
		"version", s.opts.Version.Version,
		"started_at", s.startedAt,
		"registered_actions", len(s.registry.ActionNames()),
		"registered_queries", len(s.registry.QueryNames()),
		"preflight_ttl_seconds", int(PreflightTTL.Seconds()),
		"lock_queue_seconds", int(LockQueueTimeout.Seconds()),
		"operation_timeout_seconds", int(OperationTimeout.Seconds()))

	// 停机期间到期的临时秘密必须在这里先清掉，否则它们会在库里多留一整个运行周期。
	if _, err := s.sweepExpiredSecrets(ctx, time.Now()); err != nil {
		s.opts.Logger.Warn("启动时清理到期临时秘密失败（下次扫描会重试）", "error", err)
	}
	go s.runSecretSweeper(ctx)

	// 过期预检计划同样没有保留价值：先清一次，之后周期清理。
	if _, err := s.sweepExpiredPlans(ctx, time.Now()); err != nil {
		s.opts.Logger.Warn("启动时清理过期预检计划失败（下次扫描会重试）", "error", err)
	}
	go s.runPlanSweeper(ctx)

	// 过期暂存（未提交的上传）24 小时后清掉：它们占着真实磁盘空间。
	go s.runStagingSweeper(ctx)

	// 共享卷监视（P08.3）：运行中掉盘的卷要让它的共享随之下线。
	// 循环自己拿 shares.ConfigLockName，拿不到就跳过本轮（见 sharewatch.go）。
	go s.runShareWatch(ctx)

	// 上次运行遗留的非终态操作：标成 interrupted，**不自动重新执行**（CONTRACTS.md 4.3）。
	// 顺序在监听之后、接活之前：先让“过去的未完成”变成可查的事实，再接受新操作。
	s.reconcileInterrupted(ctx)

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		s.opts.Logger.Info("收到停止信号，开始优雅停止")
		// 先取消在跑的操作并等它们写终态，再关 HTTP：
		// 反过来会让“停机”变成“库里留一堆 running”。
		s.runner.stopAll()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), s.opts.ShutdownTimeout)
		defer cancel()
		if err := s.http.Shutdown(shutdownCtx); err != nil {
			s.opts.Logger.Error("优雅停止未能在超时内完成", "error", err)
			_ = s.http.Close()
		}
	}()

	err = s.http.Serve(ln)
	_ = raw.Close()
	_ = os.Remove(s.opts.SocketPath)
	if errors.Is(err, http.ErrServerClosed) {
		<-shutdownDone
		s.opts.Logger.Info("代理已停止")
		return nil
	}
	if err != nil {
		return fmt.Errorf("代理服务异常退出: %w", err)
	}
	return nil
}

// secureSocket 把 socket 设为 root:<SocketGID> 0660 并回读校验。
func (s *Server) secureSocket() error {
	path := s.opts.SocketPath
	if err := os.Chown(path, 0, int(s.opts.Peer.SocketGID)); err != nil {
		return fmt.Errorf("设置 socket 属主失败 %s: %w", path, err)
	}
	if err := os.Chmod(path, SocketMode); err != nil {
		return fmt.Errorf("设置 socket 权限失败 %s: %w", path, err)
	}
	if err := verifySocketOwnership(path, s.opts.Peer.SocketGID); err != nil {
		return err
	}
	return nil
}

// prepareSocketPath 校验并清理 socket 文件。
func (s *Server) prepareSocketPath() error {
	dir := filepath.Dir(s.opts.SocketPath)
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("socket 目录不可用 %s: %w（应由安装脚本以 root 创建）", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("socket 目录不是目录: %s", dir)
	}

	st, err := os.Lstat(s.opts.SocketPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("检查 socket 路径失败 %s: %w", s.opts.SocketPath, err)
	}
	// 只允许替换 socket 本身：普通文件、目录、符号链接一律拒绝，避免误删。
	if st.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%s 已存在且不是 socket（类型 %s），拒绝覆盖", s.opts.SocketPath, st.Mode().Type())
	}
	// 是 socket 还要再问一句：**上面有没有人在服务**。
	//
	// 这里的删除是给"上次异常退出留下的陈旧 socket"用的，不是给"抢别人正在用的
	// socket"用的。两者长得一模一样，唯一的分辨办法是连一下：连得上就是有人，
	// 连不上才是陈旧。
	//
	// 不分这一下会出真事：P11.3 验收那台 22.04 上，上一轮留下的 systemd 单元在
	// 3 秒一次的重启循环里抢同一个 socket 路径。脚本手启的代理明明活着，Web 却报
	// `dial unix /run/upanel/agent.sock: no such file or directory`——抢到路径的那个
	// 实例随后失败退出，退出时把这个路径顺手删了；还活着的那个从此不可达，
	// 而它自己完全不知道。
	live, err := probeAgentSocket(s.opts.SocketPath)
	if err != nil {
		return err
	}
	if live {
		return fmt.Errorf("%s 上已经有一个代理在服务，拒绝启动第二个"+
			"（同一个 socket 只能有一个代理；不删别人的 socket）", s.opts.SocketPath)
	}
	if err := os.Remove(s.opts.SocketPath); err != nil {
		return fmt.Errorf("清理残留 socket 失败 %s: %w", s.opts.SocketPath, err)
	}
	s.opts.Logger.Warn("清理了残留的 socket 文件", "socket", s.opts.SocketPath)
	return nil
}

// probeAgentSocket 判断这个 socket 路径上还有没有活着的监听者。
//
// 三态，不能压成两态：
//   - 连得上 → 有人，调用方必须拒绝启动；
//   - 内核明说没人监听（连接被拒、路径已不存在）→ 陈旧，可以清；
//   - 说不清（超时之类）→ 当成"有人"，**不许清**。
//
// 第三态宁可让运维看见一条"拒绝启动"，也不要让一个可能活着的代理失去自己的
// socket——那种故障在面板上只表现为"代理不可用"，没人查得到真正的原因。
func probeAgentSocket(path string) (bool, error) {
	conn, err := net.DialTimeout("unix", path, socketProbeTimeout)
	if err == nil {
		_ = conn.Close()
		return true, nil
	}
	if os.IsTimeout(err) {
		return true, nil
	}
	if isNoListenerErr(err) {
		return false, nil
	}
	return false, fmt.Errorf("探测 socket 是否有人服务失败 %s: %w", path, err)
}

// socketProbeTimeout 是一次探测的上限。
//
// 连一个 Unix socket 是本地内存操作，正常是微秒级；给 1 秒是为了在机器刚重启、
// 负载很高时不要误判成"没人"，也不要让启动卡在这里。
const socketProbeTimeout = time.Second
