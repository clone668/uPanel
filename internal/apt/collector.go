// Package apt 读取这台机器上 APT/dpkg 的**事实**：装了哪些包、装的是哪个版本、
// APT 认为的候选版本是哪个、候选来自哪些 suite、以及"需要重启"那个标志文件。
//
// 边界（CONTRACTS.md 第 12.1 节）：
//
//   - 只读。这里没有任何一条路径会跑 `apt-get install/remove/upgrade`，也不会刷新索引
//     （`package.refresh` 是 P11.2 的动作，要预检、要锁、要审计）。
//   - 只给事实，不给判断。"哪个算可升级""哪个算安全更新"由面板的 internal/packages
//     判——那些规则会变，而事实不该跟着规则一起变。
//   - 读不到就说读不到：`apt-cache` 挂了不影响已安装列表，但每个包的候选版本会明确
//     变成"读不到"，不会拿已安装版本冒充。
package apt

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"upanel/internal/syscmd"
)

// 默认路径。与 P04 起采集器用的 /proc 路径分开写：这里读的是包管理器自己的文件，
// 不是内核接口。
const (
	DefaultDpkgQuery     = "/usr/bin/dpkg-query"
	DefaultAptCache      = "/usr/bin/apt-cache"
	DefaultAptGet        = "/usr/bin/apt-get"
	DefaultListsDir      = "/var/lib/apt/lists"
	DefaultRebootFlag    = "/run/reboot-required"
	DefaultRebootPkgs    = "/run/reboot-required.pkgs"
	DefaultBootIDPath    = "/proc/sys/kernel/random/boot_id"
	DefaultCacheTTL      = 30 * time.Second
	rebootFlagPathLegacy = "/var/run/reboot-required"

	// APT/dpkg 的四个系统锁（P11.2 只探测、从不删除）。
	DefaultDpkgLockFrontend = "/var/lib/dpkg/lock-frontend"
	DefaultDpkgLock         = "/var/lib/dpkg/lock"
	DefaultAptListsLock     = "/var/lib/apt/lists/lock"
	DefaultAptArchivesLock  = "/var/cache/apt/archives/lock"
)

// 命令限额。apt-cache policy 对 676 个包实测 384ms，24.04 同量级；
// 60 秒是给"索引很大 + 磁盘很慢"的余量，不是常态。
const (
	dpkgQueryTimeout = 30 * time.Second
	dpkgQueryMaxOut  = 8 << 20
	aptCacheTimeout  = 60 * time.Second
	aptCacheMaxOut   = 16 << 20
	// aptCacheChunk 是一次 `apt-cache policy` 最多带多少个包名。
	//
	// 单次把一个长期运行的机器上的几千个包名全塞进 argv 也能跑，但参数越长越容易撞上
	// 系统对单个 argv 的限制，而且一次失败就整批没候选版本。分块后失败只影响那一块。
	aptCacheChunk = 2000
)

// Paths 是采集器读取的固定路径。
type Paths struct {
	DpkgQuery          string
	AptCache           string
	ListsDir           string
	RebootRequired     string
	RebootRequiredPkgs string
	BootID             string
	// 写事务用到的路径（P11.2）。读路径不碰它们。
	AptGet           string
	DpkgLockFrontend string
	DpkgLock         string
	AptListsLock     string
	AptArchivesLock  string
}

// DefaultPaths 返回 Linux 上的标准路径。
func DefaultPaths() Paths {
	return Paths{
		DpkgQuery:          DefaultDpkgQuery,
		AptCache:           DefaultAptCache,
		ListsDir:           DefaultListsDir,
		RebootRequired:     DefaultRebootFlag,
		RebootRequiredPkgs: DefaultRebootPkgs,
		BootID:             DefaultBootIDPath,
		AptGet:             DefaultAptGet,
		DpkgLockFrontend:   DefaultDpkgLockFrontend,
		DpkgLock:           DefaultDpkgLock,
		AptListsLock:       DefaultAptListsLock,
		AptArchivesLock:    DefaultAptArchivesLock,
	}
}

// SourceError 是一路来源的失败。
//
// 它出现在响应里而不是只进日志：页面必须能说"候选版本读不到，原因是这个"，
// 而不是显示一份看着完整、其实缺了一半的数据。
type SourceError struct {
	// Source 是来源标识：dpkg_query / apt_cache / reboot_pkgs / boot_id。
	Source string `json:"source"`
	// Message 是给人看的一句话（含退出码或解析失败的位置），不含命令输出全文。
	Message string `json:"message"`
}

// 来源标识的闭集。
const (
	SourceDpkgQuery  = "dpkg_query"
	SourceAptCache   = "apt_cache"
	SourceRebootPkgs = "reboot_pkgs"
	SourceBootID     = "boot_id"
)

// Package 是一个包的**事实**（没有判断）。
type Package struct {
	// Name 是 dpkg 的二进制包名（多架构同名包带 `:arch` 限定，与 dpkg 一致）。
	Name string `json:"name"`
	// Architecture 是已安装包的架构（amd64、all 等）。
	Architecture string `json:"architecture"`
	// Status 是 `${db:Status-Abbrev}` 的原文（`ii`、`rc` 等）。
	Status string `json:"status"`
	// InstalledVersion 是 dpkg 数据库里的版本，epoch 前缀原样保留。
	InstalledVersion string `json:"installed_version"`
	// CandidateVersion 是 `apt-cache policy` 的 Candidate；读不到时为空串。
	CandidateVersion string `json:"candidate_version"`
	// CandidateSuites 是**候选版本那一条**的来源 suite 原文（去重、升序）。
	CandidateSuites []string `json:"candidate_suites"`
	// CandidateFromDpkgStatus 表示候选只来自 /var/lib/dpkg/status（本地装的包，
	// 任何仓库都没有）。它不是"错误"，但页面据此说"这个版本不是从仓库来的"。
	CandidateFromDpkgStatus bool `json:"candidate_from_dpkg_status"`
}

// Snapshot 是一次包事实采集。
type Snapshot struct {
	CollectedAt time.Time `json:"collected_at"`
	// IndexUpdatedAt 是 /var/lib/apt/lists 的 mtime（候选版本是哪一刻算出来的）；
	// 读不到时为 nil，页面显示"索引时间未知"，不假装刚刷新过。
	IndexUpdatedAt *time.Time `json:"index_updated_at"`
	// Packages 按包名升序。
	Packages []Package `json:"packages"`
	// Errors 是降级来源（apt_cache 等）。dpkg_query 失败会让整个采集返回错误。
	Errors []SourceError `json:"errors"`
}

// PowerState 是"需不需要重启"这件事的事实。
type PowerState struct {
	CollectedAt time.Time `json:"collected_at"`
	// RebootRequired 是 /run/reboot-required 是否存在。它是包自己（postinst）写的文件，
	// 面板不推断"装了内核所以要重启"。
	RebootRequired bool `json:"reboot_required"`
	// RebootRequiredSince 是主文件的 mtime；文件不存在时为 nil。
	RebootRequiredSince *time.Time `json:"reboot_required_since"`
	// RebootRequiredPackages 是 /run/reboot-required.pkgs 的每一行（去空行）。
	RebootRequiredPackages []string `json:"reboot_required_packages"`
	// RebootRequiredPackagesError 是读 .pkgs 失败的原因；空串表示读到了或文件不存在。
	RebootRequiredPackagesError string `json:"reboot_required_packages_error"`
	// BootID 是本次开机的随机标识；P11.3 用它确认重启真的发生过。
	BootID string `json:"boot_id"`
	// BootIDError 是读 boot_id 失败的原因；空串表示读到了。
	BootIDError string `json:"boot_id_error"`
}

// Runner 执行一条外部命令。生产实现是 syscmd.Run。
type Runner func(ctx context.Context, spec syscmd.Spec, limits syscmd.Limits) (syscmd.Result, error)

// 文件系统访问。拆出来是为了让单测能喂真实抓回来的输出（真机原文），
// 而不需要一台装着这些包的机器。
type (
	ReadFile func(path string) ([]byte, error)
	StatFile func(path string) (os.FileInfo, error)
)

// Options 是采集器参数。零值可用：真实命令、真实文件系统、30 秒缓存。
type Options struct {
	Paths Paths
	TTL   time.Duration
	Now   func() time.Time
	Run   Runner
	Read  ReadFile
	Stat  StatFile
}

// Collector 采集并缓存 APT 事实。
type Collector struct {
	paths Paths
	ttl   time.Duration
	now   func() time.Time
	run   Runner
	read  ReadFile
	stat  StatFile

	mu       sync.Mutex
	cached   Snapshot
	hasCache bool
	// gen 每成功采集一次加一；等别人采的调用者靠它分辨"拿到的是他刚采的"还是
	// "拿到的是他失败后留下的旧快照"。后者对 fresh=true 是假话，必须自己重采。
	gen uint64
	// inflight 非 nil 表示已经有一次采集在跑；关掉它表示那次采集结束。
	inflight chan struct{}

	// 候选版本缓存。单独一把锁、单独一层理由（见 candidateVersions）。
	candMu sync.Mutex
	cand   candidateCache
}

// candidateCache 是"包名 → 候选版本"的跨快照缓存。
//
// 为什么需要它——这一条是量出来的，不是猜的：`apt-cache policy` 在 apt 2.8（24.04）
// 上把每个参数都对**全部** 87804 个包名做一次模式匹配，731 个已安装包名要跑 26 秒；
// 同样的命令在 apt 2.4（22.04）上 0.3 秒。候选版本只随两件事变化：索引更新，或者
// 这个包自己被换了版本。所以按 (索引 mtime, 已安装版本) 缓存它，30 秒重采一次变成
// 0.3 秒，索引一变（或某个包被装/卸/升级）只重算受影响的那几个名字。
type candidateCache struct {
	entries map[string]policyEntry
	// installed 记住算这条候选时这台机器上装的是哪个版本。
	installed map[string]string
	// indexKey 是 /var/lib/apt/lists 的 mtime；它一变，全部候选作废。
	// 空串表示"索引时间读不到"，与"读到了某个具体时间"是不同的键。
	indexKey string
	valid    bool
}

// New 构造采集器。参数缺省按默认值补齐，不返回错误：
// "这台机器上没有 apt" 是运行期判定（见 Available），不是构造失败。
func New(opts Options) *Collector {
	paths := opts.Paths
	def := DefaultPaths()
	if paths.DpkgQuery == "" {
		paths.DpkgQuery = def.DpkgQuery
	}
	if paths.AptCache == "" {
		paths.AptCache = def.AptCache
	}
	if paths.ListsDir == "" {
		paths.ListsDir = def.ListsDir
	}
	if paths.RebootRequired == "" {
		paths.RebootRequired = def.RebootRequired
	}
	if paths.RebootRequiredPkgs == "" {
		paths.RebootRequiredPkgs = def.RebootRequiredPkgs
	}
	if paths.BootID == "" {
		paths.BootID = def.BootID
	}
	ttl := opts.TTL
	if ttl <= 0 {
		ttl = DefaultCacheTTL
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	run := opts.Run
	if run == nil {
		run = syscmd.Run
	}
	read := opts.Read
	if read == nil {
		read = os.ReadFile
	}
	st := opts.Stat
	if st == nil {
		st = os.Stat
	}
	return &Collector{paths: paths, ttl: ttl, now: now, run: run, read: read, stat: st}
}

// Available 报告这台机器上有没有 dpkg-query。
//
// 代理启动时用它决定**要不要登记** apt.packages 与 system.power_state：
// 一台不是 Debian 系的主机上登记这两条查询，等于让 capabilities 说"我能读包列表"，
// 而点开必然 503。
func (c *Collector) Available() bool {
	info, err := c.stat(c.paths.DpkgQuery)
	if err != nil {
		return false
	}
	return !info.IsDir() && info.Mode()&0o111 != 0
}

// Packages 返回包事实。
//
// fresh=true 绕过快照缓存：写操作前的判断必须看见刚刚在命令行里 apt 过的东西，
// 而缓存窗口里的"没有变化"正是最危险的那句谎话。候选版本缓存不受 fresh 影响——
// 候选只随索引和已安装版本变化，与"什么时候问的"无关（见 candidateCache）。
//
// 同一时刻只跑一次采集：开机预热与第一个页面请求会撞在一起，而在 24.04 上一次
// 冷采要二十多秒，两次并发冷采不是"快一倍"，是互相抢 CPU 把两边一起拖过超时。
func (c *Collector) Packages(ctx context.Context, fresh bool) (Snapshot, error) {
	for attempt := 0; attempt < 3; attempt++ {
		if !fresh && attempt == 0 {
			if snap, ok := c.freshCache(); ok {
				return snap, nil
			}
		}
		c.mu.Lock()
		wait, seen := c.inflight, c.gen
		if wait == nil {
			done := make(chan struct{})
			c.inflight = done
			c.mu.Unlock()

			snap, err := c.collect(ctx)

			c.mu.Lock()
			if err == nil {
				c.cached = snap
				c.hasCache = true
				c.gen++
			}
			c.inflight = nil
			close(done)
			c.mu.Unlock()
			return snap, err
		}
		c.mu.Unlock()

		select {
		case <-wait:
		case <-ctx.Done():
			return Snapshot{}, ctx.Err()
		}
		// 那次采集成功了：gen 变了，缓存里的就是刚刚这一刻的机器状态。他开始时我们还没到，
		// 但他结束得比我们到得晚，而两次采集之间不会有写操作插进来（写操作自己排队），
		// 所以这份结果对 fresh=true 也够新——比再跑一遍二十秒的冷采好得多。
		if snap, ok := c.cacheSince(seen); ok {
			return snap, nil
		}
		// 那次采集失败了：缓存是旧的，fresh=true 时它正是那句最危险的谎话。自己重来。
	}
	return Snapshot{}, errors.New("apt: 连续三次采集都被并发采集打断，放弃本次读取")
}

// freshCache 返回仍在缓存窗口内的快照。
func (c *Collector) freshCache() (Snapshot, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.hasCache && c.now().Sub(c.cached.CollectedAt) < c.ttl {
		return c.cached, true
	}
	return Snapshot{}, false
}

// cacheSince 返回"在 gen 变成 seen 之后采集到"的那份快照。gen 没动就是没有新快照。
func (c *Collector) cacheSince(seen uint64) (Snapshot, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.hasCache || c.gen == seen {
		return Snapshot{}, false
	}
	return c.cached, true
}

// collect 真正跑命令。不持锁：一起采集两次只会多跑两条命令，不会互相阻塞。
func (c *Collector) collect(ctx context.Context) (Snapshot, error) {
	snap := Snapshot{CollectedAt: c.now(), Packages: []Package{}, Errors: []SourceError{}}
	if ts, err := c.indexUpdatedAt(); err != nil {
		snap.Errors = append(snap.Errors, SourceError{Source: SourceAptCache,
			Message: fmt.Sprintf("读 APT 索引时间失败：%v", err)})
	} else if ts != nil {
		snap.IndexUpdatedAt = ts
	}

	installed, err := c.installedPackages(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	// 候选缓存以索引 mtime 为作废键，所以必须先拿到索引时间再问候选。
	candidates, candErr := c.candidateVersions(ctx, installed, snap.IndexUpdatedAt)
	if candErr != nil {
		snap.Errors = append(snap.Errors, SourceError{Source: SourceAptCache,
			Message: candErr.Error()})
	}
	for i := range installed {
		if info, ok := candidates[installed[i].Name]; ok {
			installed[i].CandidateVersion = info.Version
			installed[i].CandidateSuites = info.Suites
			installed[i].CandidateFromDpkgStatus = info.OnlyLocal
		}
	}
	sort.SliceStable(installed, func(i, j int) bool {
		if installed[i].Name != installed[j].Name {
			return installed[i].Name < installed[j].Name
		}
		return installed[i].Architecture < installed[j].Architecture
	})
	snap.Packages = installed
	return snap, nil
}

// installedPackages 跑 dpkg-query 拿已注册的包。
//
// 失败就是失败：这一页的全部内容是"装了什么"，读不到它没有可降级的东西。
func (c *Collector) installedPackages(ctx context.Context) ([]Package, error) {
	res, err := c.run(ctx,
		syscmd.Spec{Program: c.paths.DpkgQuery,
			Args: []string{"-W", "-f", "${binary:Package}\t${Version}\t${Architecture}\t${db:Status-Abbrev}\n"}},
		syscmd.Limits{Timeout: dpkgQueryTimeout, MaxStdout: dpkgQueryMaxOut, MaxStderr: 64 << 10})
	if err != nil {
		return nil, fmt.Errorf("执行 dpkg-query 失败: %w", err)
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("dpkg-query 退出码 %d：%s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	if res.StdoutTruncated {
		// 截断的列表比"读不到"更危险：它看起来完整，但少了一部分包。
		return nil, errors.New("dpkg-query 输出被截断（超过上限），拒绝把不完整的包列表当成完整结果")
	}
	pkgs, perr := ParseDpkgQuery(res.Stdout)
	if perr != nil {
		return nil, perr
	}
	return pkgs, nil
}

// candidateVersions 跑 apt-cache policy 拿候选版本与它的来源 suite。
//
// 只问**需要重算的名字**：新出现的包、已安装版本变了的包，以及索引换过之后的全部。
// 这是这一层唯一的性能杠杆——见 candidateCache 上的注释（24.04 上一个名字 36 毫秒）。
// 返回的 map 可能少几个名字（apt-cache 失败的那些），调用方把它们留空并带上错误。
func (c *Collector) candidateVersions(ctx context.Context, pkgs []Package,
	indexAt *time.Time) (map[string]policyEntry, error) {
	indexKey := ""
	if indexAt != nil {
		indexKey = indexAt.UTC().Format(time.RFC3339Nano)
	}

	c.candMu.Lock()
	defer c.candMu.Unlock()
	if c.cand.valid && c.cand.indexKey != indexKey {
		// 索引换了：候选全部作废。installed 也要一起清——它记的是"这条候选是基于
		// 哪个已安装版本算的"，条目没了它就没意义；留着会让下面的差集算出"一个
		// 都不用重问"，于是候选全部消失。
		c.cand.entries = map[string]policyEntry{}
		c.cand.installed = nil
	}
	if c.cand.entries == nil {
		c.cand.entries = map[string]policyEntry{}
	}

	names := make([]string, 0, len(pkgs))
	for _, p := range pkgs {
		if v, ok := c.cand.installed[p.Name]; ok && v == p.InstalledVersion {
			continue
		}
		names = append(names, p.Name)
	}

	// 同名不同架构的基名（真的装着 libc6:amd64 与 libc6:i386）。
	// 见 lookupPolicy：基名那块只说得出**一个**架构的候选，不能拿去顶另一个。
	bases := make(map[string]string, len(pkgs))
	ambiguous := make(map[string]struct{}, 8)
	for _, p := range pkgs {
		base := p.Name
		if i := strings.IndexByte(base, ':'); i >= 0 {
			base = base[:i]
		}
		if prev, ok := bases[base]; ok && prev != p.Name {
			ambiguous[base] = struct{}{}
		}
		bases[base] = p.Name
	}

	var firstErr error
	for start := 0; start < len(names); start += aptCacheChunk {
		end := start + aptCacheChunk
		if end > len(names) {
			end = len(names)
		}
		chunk := names[start:end]
		res, err := c.run(ctx,
			syscmd.Spec{Program: c.paths.AptCache, Args: append([]string{"policy"}, chunk...)},
			syscmd.Limits{Timeout: aptCacheTimeout, MaxStdout: aptCacheMaxOut, MaxStderr: 64 << 10})
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("执行 apt-cache policy 失败: %w", err)
			}
			continue
		}
		if res.ExitCode != 0 {
			if firstErr == nil {
				firstErr = fmt.Errorf("apt-cache policy 退出码 %d：%s",
					res.ExitCode, strings.TrimSpace(res.Stderr))
			}
			continue
		}
		if res.StdoutTruncated {
			if firstErr == nil {
				firstErr = errors.New("apt-cache policy 输出被截断（超过上限）")
			}
			continue
		}
		entries, perr := ParseAptCachePolicy(res.Stdout)
		if perr != nil {
			if firstErr == nil {
				firstErr = perr
			}
			continue
		}
		for _, name := range chunk {
			// 采到的名字也存，**没采到的也存**（零值＝没有候选）。apt-cache policy
			// 对每个问到的名字都会打一块；它没打，就是"这个名字没有任何候选来源"
			// （本地 dpkg -i 装的包、或已经不在任何仓库里的包）。不存的话这些名字
			// 每 30 秒都要重新问一次——那正是这一层缓存要避免的开销。
			entry, _ := lookupPolicy(entries, name, ambiguous)
			c.cand.entries[name] = entry
		}
	}

	// 记下这批候选是基于哪些已安装版本算出来的；只记真正采到的包，
	// 失败的那些下一轮还会重问。
	if c.cand.installed == nil {
		c.cand.installed = make(map[string]string, len(pkgs))
	}
	next := make(map[string]string, len(pkgs))
	out := make(map[string]policyEntry, len(pkgs))
	for _, p := range pkgs {
		e, ok := c.cand.entries[p.Name]
		if !ok {
			continue
		}
		out[p.Name] = e
		next[p.Name] = p.InstalledVersion
	}
	c.cand.installed = next
	c.cand.indexKey = indexKey
	c.cand.valid = true
	// 已经不在 dpkg 里的包名不要留在缓存里：entries 只在采到新候选时长大，
	// 不裁剪的话一台长期运行的机器会把装过又卸掉的包名永远攒着。
	if len(c.cand.entries) > 2*len(pkgs)+128 {
		keep := make(map[string]policyEntry, len(out))
		for name, e := range out {
			keep[name] = e
		}
		c.cand.entries = keep
	}
	return out, firstErr
}

// lookupPolicy 在一个 chunk 的解析结果里找一个包名的候选条目。
//
// 为什么需要退回基名：`apt-cache policy` 打印的块头**去掉了架构限定**。真机原文
// （24.04，这次验收抓出来的）——
//
//	$ apt-cache policy bind9-libs:amd64
//	bind9-libs:
//	  Installed: 1:9.18.39-0ubuntu0.24.04.8
//	  Candidate: 1:9.18.39-0ubuntu0.24.04.8
//
// 我们按 dpkg 的 `${binary:Package}` 去问（`bind9-libs:amd64`），它回的是 `bind9-libs:`，
// 于是键对不上、候选被判成"读不到"。731 个包里恰好只有这一个多架构包，页面就会在一个
// 完全正常、且与已安装版本相同的包上写"候选读不到"——**它不是读不到，是我们没认出来**。
//
// 退回基名有条件：只有当这台机器上**没有**同名不同架构的另一个包时才退。真的装着
// libc6:amd64 与 libc6:i386 时，基名那块只说得出一个架构的候选，拿它去顶另一个架构
// 就是编数据；那时宁可口径保守（两个都记"读不到"），也不能把 amd64 的版本写到 i386 上。
func lookupPolicy(entries map[string]policyEntry, name string,
	ambiguous map[string]struct{}) (policyEntry, bool) {
	if e, ok := entries[name]; ok {
		return e, true
	}
	i := strings.IndexByte(name, ':')
	if i < 0 {
		return policyEntry{}, false
	}
	base := name[:i]
	if _, bad := ambiguous[base]; bad {
		return policyEntry{}, false
	}
	e, ok := entries[base]
	return e, ok
}

// indexUpdatedAt 返回 APT 索引目录的 mtime。
func (c *Collector) indexUpdatedAt() (*time.Time, error) {
	info, err := c.stat(c.paths.ListsDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// 索引目录不存在是"从来没刷新过"，不是读失败。
			return nil, nil
		}
		return nil, err
	}
	ts := info.ModTime().UTC()
	return &ts, nil
}

// PowerState 读重启标志与 boot_id。
//
// 不缓存：三个小文件，几十微秒，而这几个值必须"就是此刻的"——
// P11.3 的重启确认会前后各读一次 boot_id。
func (c *Collector) PowerState(ctx context.Context) (PowerState, error) {
	if err := ctx.Err(); err != nil {
		return PowerState{}, err
	}
	st := PowerState{CollectedAt: c.now(), RebootRequiredPackages: []string{}}

	info, err := c.stat(c.paths.RebootRequired)
	switch {
	case err == nil:
		st.RebootRequired = true
		ts := info.ModTime().UTC()
		st.RebootRequiredSince = &ts
	case errors.Is(err, os.ErrNotExist):
		// 不存在就是不需要重启。另有旧路径 /var/run（多数发行版是 /run 的符号链接），
		// 只在 /run 不存在时才去看它，避免同一个文件被算两次。
		if legacy, lerr := c.stat(rebootFlagPathLegacy); lerr == nil {
			st.RebootRequired = true
			ts := legacy.ModTime().UTC()
			st.RebootRequiredSince = &ts
		}
	default:
		// 读不到（权限、I/O 错误）不能当成"不需要重启"：那会在最需要提示的时刻
		// 给出相反的结论。这里以错误返回，由接口层如实报 503。
		return PowerState{}, fmt.Errorf("读 %s 失败: %w", c.paths.RebootRequired, err)
	}

	if data, err := c.read(c.paths.RebootRequiredPkgs); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			st.RebootRequiredPackagesError = err.Error()
		}
	} else {
		st.RebootRequiredPackages = ParseRebootPkgs(data)
	}

	if data, err := c.read(c.paths.BootID); err != nil {
		st.BootIDError = err.Error()
	} else {
		id, perr := ParseBootID(data)
		if perr != nil {
			st.BootIDError = perr.Error()
		} else {
			st.BootID = id
		}
	}
	return st, nil
}
