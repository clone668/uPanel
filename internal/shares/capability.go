package shares

// Samba 能力检测（P08.1）。
//
// 探针只做只读的事：看几个固定绝对路径上有没有可执行文件、读 smb.conf 的前一小段、
// 跑一次 `smbd --version` 拿版本号。它**不安装任何东西**（PLAN.md 第 10 节：禁止静默
// 安装）、不改配置、不启动服务。
//
// 路径是代码里的常量，不来自配置也不来自 HTTP：这是一个“面板决定去执行哪个二进制”
// 的地方，一旦可配置，配置就变成了任意命令执行的入口（PLAN.md 第 4 节）。

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"upanel/internal/syscmd"
)

// 固定路径候选。顺序即优先级：先常规位置，再 sbin（不同发行版的放置习惯不同）。
var (
	smbdCandidates       = []string{"/usr/sbin/smbd"}
	smbpasswdCandidates  = []string{"/usr/bin/smbpasswd", "/usr/sbin/smbpasswd"}
	pdbeditCandidates    = []string{"/usr/bin/pdbedit", "/usr/sbin/pdbedit"}
	testparmCandidates   = []string{"/usr/bin/testparm", "/usr/sbin/testparm"}
	smbcontrolCandidates = []string{"/usr/bin/smbcontrol", "/usr/sbin/smbcontrol"}
	smbstatusCandidates  = []string{"/usr/bin/smbstatus", "/usr/sbin/smbstatus"}
	setfaclCandidates    = []string{"/usr/bin/setfacl", "/usr/sbin/setfacl"}
	getfaclCandidates    = []string{"/usr/bin/getfacl", "/usr/sbin/getfacl"}

	// NFS 的工具候选（P14.4）。`exportfs` 一个就够：导出表的应用与读回都靠它，
	// `showmount` 是客户端视角的旁证，缺了不影响判决。
	exportfsCandidates  = []string{"/usr/sbin/exportfs", "/usr/bin/exportfs"}
	showmountCandidates = []string{"/usr/sbin/showmount", "/usr/bin/showmount"}
)

// 配置与托管目录的固定路径。
const (
	// DefaultConfigPath 是 Samba 的主配置文件。P08.1 只读它。
	DefaultConfigPath = "/etc/samba/smb.conf"
	// DefaultIncludeDir 是面板独占的 include 目录。P08.2 在这里写托管配置，
	// 并通过主配置里的一行 include 接进来。P08.1 只检查它存不存在。
	DefaultIncludeDir = "/etc/samba/upanel.d"
	// defaultCapabilityTTL 与存储模块的 30 秒一致：同一窗口内刷新页面
	// 拿到的是同一份能力，不会两次刷新看到互相矛盾的结论。
	defaultCapabilityTTL = 30 * time.Second
	// includeMarker 是在 smb.conf 里找的那一行 include 的目标片段。
	includeMarker = "/etc/samba/upanel.d/"
)

// ToolPaths 是一组探到的绝对路径。空字符串表示没找到。
type ToolPaths struct {
	Smbd       string `json:"smbd,omitempty"`
	Smbpasswd  string `json:"smbpasswd,omitempty"`
	Pdbedit    string `json:"pdbedit,omitempty"`
	Testparm   string `json:"testparm,omitempty"`
	Smbcontrol string `json:"smbcontrol,omitempty"`
	Smbstatus  string `json:"smbstatus,omitempty"`
	Setfacl    string `json:"setfacl,omitempty"`
	Getfacl    string `json:"getfacl,omitempty"`
	// Exportfs / Showmount 是 NFS 那一半的工具（P14.4）。
	Exportfs  string `json:"exportfs,omitempty"`
	Showmount string `json:"showmount,omitempty"`
}

// NFSCapability 是 NFS 导出的能力判定（P14.4）。
//
// 为什么与 Samba 的分开而不是合成一个 status：两个协议的工具、配置、服务彼此独立，
// 一台机器完全可能“Samba 齐全但没装 nfs-kernel-server”。合成一个 status 会让
// “缺 nfs-kernel-server”显示成“共享功能不可用”，连 SMB 那一半也一起被判死。
type NFSCapability struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
	// Exportfs 是探到的 `exportfs` 绝对路径（缺失时为空）。
	Exportfs string `json:"exportfs,omitempty"`
	// ExportsDir / ExportsFile 是面板独占的目录与文件。
	ExportsDir  string `json:"exports_dir"`
	ExportsFile string `json:"exports_file"`
	// ExportsDirReady 报告目录存不存在（不存在时应用会创建它）。
	ExportsDirReady bool `json:"exports_dir_ready"`
	// ExportsPresent / ExportsHash 是托管导出文件此刻的状态。
	ExportsPresent bool   `json:"exports_present"`
	ExportsHash    string `json:"exports_hash,omitempty"`
	// ExportedPaths 是托管文件里现在的导出路径（从文件里读，不从内核读）。
	ExportedPaths []string `json:"exported_paths"`
	// MainExportsReadable 报告系统主导出文件（/etc/exports）读不读得到。
	//
	// 读不到就没法确认“这条路径是不是已经被别人导出了”，冲突检测因此只能给告警。
	MainExportsReadable bool `json:"main_exports_readable"`
	// ServiceState 是 nfs-server 的运行状态（active / inactive / unknown）。
	ServiceState string `json:"service_state"`
	// ConfigWrite 是 available / unsupported（缺 exportfs 时不可写）。
	ConfigWrite string    `json:"config_write"`
	Notices     []Blocker `json:"notices"`
}

// Available 报告 NFS 导出能力是否可用。
func (c NFSCapability) Available() bool { return c.Status == CapabilityAvailable }

// Capability 是 GET /shares 之前页面要读的那份能力报告。
//
// status 只有两态：available（工具齐全、配置可读）与 blocked（缺东西，reason 说明
// 缺什么）。config_write 从 P08.2 起不再是常量：它是“能不能真的把配置写下去并让它
// 生效”的结论，取决于 testparm、smbcontrol 与配置可读这三件事——把这一条放进能力
// 对象里，是因为“能不能写配置”与“共享能不能用”是同一个问题，不该让页面去猜。
type Capability struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`

	SambaVersion string `json:"samba_version"`
	// Tools 是探到的绝对路径；缺失的项不出现在这里。
	Tools ToolPaths `json:"tools"`

	ConfigPath     string `json:"config_path"`
	ConfigReadable bool   `json:"config_readable"`
	// IncludeDir 与 IncludeReady 描述 P08.2 要用的托管目录。
	IncludeDir   string `json:"include_dir"`
	IncludeReady bool   `json:"include_ready"`
	// IncludePresent 报告主配置里是否已经有一行**生效的** include 指到托管目录。
	IncludePresent bool `json:"include_present"`
	// ConfigWrite 是 unsupported 或 available（见上）。
	ConfigWrite string `json:"config_write"`

	// ServiceState 是 smbd 的运行状态（active / inactive / unknown）。
	//
	// 单列出来是因为“共享能不能用”不只取决于配置写对没有：smbd 没在跑的时候，
	// 一份完全正确的配置也提供不了任何访问，而面板必须能说出这一点。
	ServiceState string `json:"service_state"`
	// ServiceUnits 是这次问了哪几个 unit（smbd.service / smbd.socket）。
	ServiceUnits []string `json:"service_units"`

	// ManagedConfig 是面板独占的那份托管配置文件的绝对路径。
	ManagedConfig string `json:"managed_config"`
	// ManagedConfigPresent 报告它此刻存在不存在。
	ManagedConfigPresent bool `json:"managed_config_present"`
	// ManagedConfigHash 是它此刻的内容哈希（不存在时为空）。
	//
	// 与登记表里每条共享的 config_hash 不同：那是单条定义的哈希，这是整份文件的。
	// 整份哈希变了就说明有人在面板之外动过这个文件。
	ManagedConfigHash string `json:"managed_config_hash"`
	// ManagedShares 是托管文件里现有的段名（不含 global）。
	ManagedShares []string `json:"managed_shares"`

	// Notices 是人读的补充说明（缺 include 目录、缺 testparm……），不改变 status。
	Notices []Blocker `json:"notices"`
	// CollectedAt 是这份能力的采集时间。
	CollectedAt time.Time `json:"collected_at"`
	// NFS 是 NFS 导出的能力判定（P14.4）。它与上面这一整套 Samba 判定彼此独立：
	// 一台机器“Samba 齐全但没装 nfs-kernel-server”是常态，反之亦然。
	NFS NFSCapability `json:"nfs"`
}

// Available 报告共享能力是否可用。页面与预检都用它，不各自解释 status 字符串。
func (c Capability) Available() bool { return c.Status == CapabilityAvailable }

// ProberOptions 是探针的接缝：默认实现读真实文件系统并跑 smbd --version。
type ProberOptions struct {
	// Stat 读文件状态（生产实现是 os.Stat）。
	Stat func(name string) (os.FileInfo, error)
	// ReadFile 读一个小文件（smb.conf 的前几 KiB 就够判断 include）。
	ReadFile func(name string) ([]byte, error)
	// Runner 执行 `smbd --version`。为 nil 时不取版本（版本只是展示信息，不影响判决）。
	Runner CommandRunner
	// Now 提供时间（可注入）。
	Now func() time.Time
	// TTL 是缓存时长；0 表示用默认 30 秒。
	TTL time.Duration
	// Paths 允许覆盖候选路径。只给测试用：生产接线不传它，
	// 因此“面板执行哪个二进制”在运行时不可配置。
	Paths ToolPaths
}

// Prober 是带 30 秒缓存的能力探针。
type Prober struct {
	opts ProberOptions

	mu       sync.Mutex
	cached   Capability
	cachedAt time.Time
	primed   bool
}

// NewProber 构造探针。
func NewProber(opts ProberOptions) *Prober {
	if opts.Stat == nil {
		opts.Stat = os.Stat
	}
	if opts.ReadFile == nil {
		opts.ReadFile = os.ReadFile
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}
	if opts.TTL <= 0 {
		opts.TTL = defaultCapabilityTTL
	}
	return &Prober{opts: opts}
}

// SystemProber 返回读真实系统的探针。
func SystemProber(runner CommandRunner) *Prober {
	return NewProber(ProberOptions{Runner: runner})
}

// Capability 返回最近一次探测结果（超过 TTL 就重新探）。
func (p *Prober) Capability(ctx context.Context) Capability {
	now := p.opts.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.primed && now.Sub(p.cachedAt) < p.opts.TTL {
		return p.cached
	}
	capv := p.probe(ctx, now)
	p.cached = capv
	p.cachedAt = now
	p.primed = true
	return capv
}

// probe 做一次真实探测。
func (p *Prober) probe(ctx context.Context, now time.Time) Capability {
	out := Capability{
		ConfigPath:    DefaultConfigPath,
		IncludeDir:    DefaultIncludeDir,
		ManagedConfig: ManagedConfigPath,
		ServiceUnits:  append([]string(nil), serviceUnits...),
		ManagedShares: []string{},
		ConfigWrite:   ConfigWriteUnsupported,
		CollectedAt:   now.Truncate(time.Second),
	}

	missing := make([]string, 0, 6)
	// 注入的路径优先：ProberOptions.Paths 只给单元测试用（生产接线不传它），
	// 让测试能在没有装 Samba 的开发机上验证“装好了会怎样”。
	pick := func(name string, override string, candidates []string, required bool) string {
		if override != "" {
			return override
		}
		if required {
			return p.find(name, candidates, &missing)
		}
		return p.optional(candidates)
	}
	out.Tools.Smbd = pick("smbd", p.opts.Paths.Smbd, smbdCandidates, true)
	out.Tools.Smbpasswd = pick("smbpasswd", p.opts.Paths.Smbpasswd, smbpasswdCandidates, true)
	out.Tools.Pdbedit = pick("pdbedit", p.opts.Paths.Pdbedit, pdbeditCandidates, true)
	out.Tools.Setfacl = pick("setfacl", p.opts.Paths.Setfacl, setfaclCandidates, true)
	out.Tools.Getfacl = pick("getfacl", p.opts.Paths.Getfacl, getfaclCandidates, true)
	// testparm / smbcontrol / smbstatus 不是 P08.1 的必需项（配置校验、重载、
	// 连接状态在 P08.2），缺了只记提示：它们的缺失让配置写入不可用，但不该把
	// “共享列表都打不开”当成结论。
	out.Tools.Testparm = pick("testparm", p.opts.Paths.Testparm, testparmCandidates, false)
	out.Tools.Smbcontrol = pick("smbcontrol", p.opts.Paths.Smbcontrol, smbcontrolCandidates, false)
	out.Tools.Smbstatus = pick("smbstatus", p.opts.Paths.Smbstatus, smbstatusCandidates, false)

	// 配置可读性：读不懂主配置就不知道这台机器的 Samba 长什么样，
	// 能力必须是 blocked，而不是“工具都在所以没问题”。
	if data, err := p.opts.ReadFile(DefaultConfigPath); err != nil {
		missing = append(missing, "smb.conf")
		out.Notices = append(out.Notices, Blocker{
			Code:    NoticeConfigNotReadable,
			Message: "读不到 " + DefaultConfigPath + "：" + err.Error(),
		})
	} else {
		out.ConfigReadable = true
		// 这里要的是“有没有一行**生效的** include”，不是“文件里有没有出现这个路径”：
		// 被注释掉的 include 不算接入，把它算成已接入会让页面显示“已接入”而实际没接。
		out.IncludePresent = ParseSmbConf(data).IncludePresent
	}

	// 托管文件本身：存在就报它的段名与哈希（“面板以为写了什么”与“文件里是什么”
	// 是两件事，页面要能把它们并排给人看）。
	if data, err := p.opts.ReadFile(ManagedConfigPath); err == nil {
		out.ManagedConfigPresent = true
		out.ManagedConfigHash = ConfigHashOf(string(data))
		out.ManagedShares = SectionNames(data)
	}

	if fi, err := p.opts.Stat(DefaultIncludeDir); err == nil && fi.IsDir() {
		out.IncludeReady = true
	} else {
		out.Notices = append(out.Notices, Blocker{
			Code: NoticeIncludeDirMissing,
			Message: "托管 include 目录 " + DefaultIncludeDir +
				" 还不存在：应用共享时会创建它（目录本身不含任何配置）",
		})
	}
	if out.Tools.Testparm == "" {
		out.Notices = append(out.Notices, Blocker{
			Code:    NoticeTestparmMissing,
			Message: "没找到 testparm：配置校验无从做起，因此不允许写 Samba 配置",
		})
	}

	// smbd 的运行状态。Runner 为 nil 时只能说“不知道”。
	if p.opts.Runner != nil {
		out.ServiceState = probeServiceState(ctx, p.opts.Runner)
	} else {
		out.ServiceState = ServiceStateUnknown
	}

	if out.Tools.Smbd != "" {
		out.SambaVersion = p.version(ctx, out.Tools.Smbd)
	}

	// NFS 那一半独立探一次：它不参与上面 missing 的判定，也不改变 Samba 的 status。
	out.NFS = p.probeNFS(ctx, now)
	// exportfs 的落点从 NFS 判定里取，而不是在这里再 pick 一次。
	//
	// 两个理由：它**不是** Samba 的必需项（缺了不该把整个能力判成 blocked），
	// 而执行层拿工具路径的唯一入口是 Capability().Tools——不往这里放，
	// reloadExports/readExportTable 就会永远看到空字符串，然后在一台装好
	// nfs-kernel-server 的机器上报“缺少 exportfs”。探到了却传不出去，
	// 是比探不到更难查的一种缺陷。
	out.Tools.Exportfs = out.NFS.Exportfs

	if len(missing) > 0 {
		out.Status = CapabilityBlocked
		out.Reason = "缺少 " + strings.Join(missing, "、") + "；" +
			"共享与受管理账号因此不可用（本面板不自动安装软件包）"
		return out
	}
	out.Status = CapabilityAvailable

	// 配置写入的可用性：三件东西缺一不可。
	//   - testparm：没有它就没有“先校验再替换”这条纪律；
	//   - smbcontrol：没有它就重载不了，改了配置也不会生效，那等于什么都没做；
	//   - 主配置可读：读不到现状就判断不了“同名段/全局项会不会被覆盖”。
	// 权限不够写（不是 root、只读挂载）不在这里判：是否可写由执行期操作的真实结果
	// 给出（写失败会带 CONFIG_NOT_WRITABLE），预检时对目录做写探测就等于提前改动机器。
	var why []string
	if out.Tools.Testparm == "" {
		why = append(why, "testparm")
	}
	if out.Tools.Smbcontrol == "" {
		why = append(why, "smbcontrol")
	}
	if !out.ConfigReadable {
		why = append(why, "可读的 smb.conf")
	}
	if len(why) == 0 {
		out.ConfigWrite = ConfigWriteAvailable
	} else {
		out.Notices = append(out.Notices, Blocker{
			Code:    NoticeConfigNotWritable,
			Message: "缺少 " + strings.Join(why, "、") + "：共享能定义、能准备目录，但配置写不下去",
		})
	}
	return out
}

// probeNFS 探一次 NFS 导出的能力。
//
// 判据只有一条是硬性的：**有没有 exportfs**。没有它既写不了也读不回实际导出表，
// 那就不能声称能管 NFS。导出目录不存在不算阻塞（应用时会创建），/etc/exports 读不到
// 只影响冲突检测的确定性，记一条提示。
func (p *Prober) probeNFS(ctx context.Context, now time.Time) NFSCapability {
	out := NFSCapability{
		Status:        CapabilityAvailable,
		ExportsDir:    DefaultExportsDir,
		ExportsFile:   DefaultExportsFile,
		ExportedPaths: []string{},
		ConfigWrite:   ConfigWriteUnsupported,
	}
	out.Exportfs = p.opts.Paths.Exportfs
	if out.Exportfs == "" {
		out.Exportfs = p.optional(exportfsCandidates)
	}
	if out.Exportfs == "" {
		out.Status = CapabilityBlocked
		out.Reason = "没找到 exportfs（nfs-kernel-server 未安装）：NFS 导出既写不了也读不回"
		out.Notices = append(out.Notices, Blocker{
			Code:    NoticeNFSUnavailable,
			Message: "没找到 exportfs：NFS 导出不可用（本面板不自动安装软件包）",
		})
	} else {
		out.ConfigWrite = ConfigWriteAvailable
	}

	if fi, err := p.opts.Stat(DefaultExportsDir); err == nil && fi.IsDir() {
		out.ExportsDirReady = true
	}
	if data, err := p.opts.ReadFile(DefaultExportsFile); err == nil {
		out.ExportsPresent = true
		out.ExportsHash = ConfigHashOf(string(data))
		entries, unparsed := ParseExports(data)
		for _, e := range entries {
			out.ExportedPaths = append(out.ExportedPaths, e.Path)
		}
		sort.Strings(out.ExportedPaths)
		if len(unparsed) > 0 {
			out.Notices = append(out.Notices, Blocker{
				Code: NoticeExportsUnparsable,
				Message: fmt.Sprintf("托管导出文件里有 %d 行读不懂：整份文件会被重写，因此不影响结果，但说明有人手工改过它",
					len(unparsed)),
			})
		}
	}
	// 系统主导出文件：读得到才能说“这条路径没被别人导出过”。
	if _, err := p.opts.ReadFile(DefaultExportsMain); err == nil {
		out.MainExportsReadable = true
	} else if !os.IsNotExist(err) {
		out.Notices = append(out.Notices, Blocker{
			Code:    NoticeExportsUnparsable,
			Message: "读不到 " + DefaultExportsMain + "：" + err.Error() + "；与既有导出的冲突无从判断",
		})
	} else {
		// 文件不存在是干净的状态：没有别人的导出。
		out.MainExportsReadable = true
	}

	out.ServiceState = probeNFSServiceState(ctx, p.opts.Runner)
	if out.ServiceState != ServiceStateActive {
		out.Notices = append(out.Notices, Blocker{
			Code:    NoticeNFSServiceInactive,
			Message: "nfs-server 现在不是 active（" + out.ServiceState + "）：导出写下去也不会提供访问",
		})
	}
	return out
}

// Invalidate 丢掉缓存。//
// 写配置的流水线在改完 smb.conf 之后必须调用它：否则接下来 30 秒里所有读者（包括
// 紧随其后的这次请求自己）都会看到“include 还没接入”“托管文件不存在”这种已经过时
// 的结论，页面就会一边显示“已应用”一边显示“未接入”。
func (p *Prober) Invalidate() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.primed = false
}

// find 按候选顺序找第一个可执行文件；找不到就记进 missing。
func (p *Prober) find(name string, candidates []string, missing *[]string) string {
	for _, c := range candidates {
		if fi, err := p.opts.Stat(c); err == nil && fi.Mode().IsRegular() {
			return c
		}
	}
	*missing = append(*missing, name)
	return ""
}

// optional 按候选顺序找第一个可执行文件；找不到返回空串。
func (p *Prober) optional(candidates []string) string {
	for _, c := range candidates {
		if fi, err := p.opts.Stat(c); err == nil && fi.Mode().IsRegular() {
			return c
		}
	}
	return ""
}

// version 跑一次 `smbd --version` 取版本号。
//
// 它只读：`--version` 打印版本就退出，不启动守护进程、不碰配置。失败时返回空串——
// 版本号是展示信息，拿不到不该让能力判决变成 blocked。
func (p *Prober) version(ctx context.Context, smbd string) string {
	if p.opts.Runner == nil {
		return ""
	}
	res, err := p.opts.Runner.Run(ctx, syscmd.Spec{
		Program: smbd,
		Args:    []string{"--version"},
	}, syscmd.Limits{Timeout: 10 * time.Second})
	if err != nil {
		return ""
	}
	// 真实输出形如 "Version 4.19.5-Ubuntu"。
	line := strings.TrimSpace(res.Stdout)
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	line = strings.TrimPrefix(line, "Version ")
	if len(line) > 64 {
		line = line[:64]
	}
	return line
}
