package discovery

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"upanel/internal/processes"
)

// UnitInfo 是一个 systemd 单元在发现功能里需要的字段。
//
// 只取三个：单元名（身份）、描述（显示）、ActiveState（状态）。发现不读单元文件、
// 不做 enablement 判断——那是 P05.1 的服务页面的事。
type UnitInfo struct {
	Unit        string
	Description string
	ActiveState string
	// MainPID 是主进程 PID；不知道时为 nil。
	MainPID *int64
}

// UnitSource 提供 systemd 单元清单。*services.Collector 通过适配器满足它。
//
// ok=false 表示“这份清单不可信”（systemctl 读不出来）。这时监听照常发现，但不会有
// systemd 候选：把裸监听标成某个单元是编造。
type UnitSource interface {
	Units() (units []UnitInfo, ok bool, notice string)
}

// Deps 是发现收集器的依赖。全部可替换，测试不碰真机。
type Deps struct {
	// FS 是读取 /proc 的最小能力；为零值时用真实文件系统。
	FS processes.FileSystem
	// ProcRoot 是 procfs 挂载点，默认 /proc。
	ProcRoot string
	// Units 是 systemd 单元来源；为空表示本机没有可用的单元清单。
	Units UnitSource
	// Docker 是容器来源；为空表示不采集容器。
	Docker DockerSource
	// Logger 用于记录采集失败。为空时静默（测试）。
	Logger *slog.Logger
	// Now 是可替换的时钟。
	Now func() time.Time
	// TTL 是快照缓存时长。发现结果是给人看的候选列表，30 秒内的重复请求不该
	// 每次都去扫一遍 /proc/<pid>/fd。
	TTL time.Duration
}

// DefaultTTL 是发现快照的缓存时长。
const DefaultTTL = 15 * time.Second

// Collector 采集并缓存发现快照。
type Collector struct {
	deps Deps

	mu   sync.Mutex
	snap Snapshot
	has  bool
}

// NewCollector 构造收集器。
func NewCollector(deps Deps) *Collector {
	if deps.ProcRoot == "" {
		deps.ProcRoot = "/proc"
	}
	if deps.Now == nil {
		deps.Now = func() time.Time { return time.Now().UTC() }
	}
	if deps.TTL <= 0 {
		deps.TTL = DefaultTTL
	}
	return &Collector{deps: deps}
}

// Snapshot 返回缓存内的快照；没有缓存或已过期时现采一次。
func (c *Collector) Snapshot(ctx context.Context) Snapshot {
	c.mu.Lock()
	snap, has := c.snap, c.has
	c.mu.Unlock()
	if has && c.deps.Now().Sub(snap.CollectedAt) < c.deps.TTL {
		return snap
	}
	return c.Collect(ctx)
}

// Cached 只返回已经采过的快照（不触发采集）。
func (c *Collector) Cached() (Snapshot, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.snap, c.has
}

// Collect 现场采集一次并更新缓存。
func (c *Collector) Collect(ctx context.Context) Snapshot {
	now := c.deps.Now().UTC().Truncate(time.Second)
	snap := c.collect(ctx, now)
	c.mu.Lock()
	c.snap = snap
	c.has = true
	c.mu.Unlock()
	if snap.Status != StatusOK && c.deps.Logger != nil {
		c.deps.Logger.Warn("应用发现不完整", "status", snap.Status, "error", snap.Error)
	}
	return snap
}

// Target 按标识解析探测目标。没有这个目标时 ok=false——那是“面板不认识它”，
// 不是“它一定挂了”。
func (c *Collector) Target(ctx context.Context, id string) (Target, bool) {
	snap := c.Snapshot(ctx)
	t, ok := snap.Targets[id]
	return t, ok
}

// collect 是采集主流程：监听表 → 持有进程 → 单元 → 容器 → 候选。
func (c *Collector) collect(ctx context.Context, now time.Time) Snapshot {
	fs := c.deps.FS
	if fs == nil {
		fs = osFS{}
	}

	var sources []SourceState
	table := readListenTable(fs, c.deps.ProcRoot)

	// 监听来源：读文件失败时状态是 error，但已经读到的行仍然进候选。
	listenState := SourceState{Source: SourceLocalListen, Status: StatusOK}
	if len(table.errors) > 0 {
		listenState.Status = StatusError
		listenState.Error = joinErrors(table.errors)
	}

	inodes := make(map[uint64]struct{}, len(table.sockets))
	for _, sock := range table.sockets {
		if sock.inode != 0 {
			inodes[sock.inode] = struct{}{}
		}
	}
	owners := socketOwners(fs, c.deps.ProcRoot, inodes)
	groups := dropShadowedSocketActivation(groupListeners(table, owners))

	// 单元来源。
	units := make(map[string]UnitInfo)
	unitState := SourceState{Source: SourceSystemd, Status: StatusOK}
	if c.deps.Units == nil {
		unitState.Status = "unavailable"
		unitState.Error = "本机没有可用的 systemd 清单，无法把监听关联到服务单元"
	} else if list, ok, notice := c.deps.Units.Units(); ok {
		for _, u := range list {
			units[u.Unit] = u
		}
	} else {
		unitState.Status = StatusError
		unitState.Error = firstNonEmpty(notice, "读取 systemd 单元失败")
	}

	// 容器来源。
	containers, dockerState := c.dockerContainers(ctx)

	candidates := make([]Candidate, 0, len(groups)+len(containers))
	used := make(map[string]int, len(groups)+len(containers))
	published := make(map[int]struct{})

	for _, ct := range containers {
		cand := containerCandidate(ct, used)
		for _, m := range ct.Ports {
			published[m.HostPort] = struct{}{}
		}
		candidates = append(candidates, cand)
	}

	for _, g := range groups {
		// 容器发布端口的宿主机侧监听先摘掉：docker-proxy 替发布端口听一遍，那不是
		// “另一个应用”。这一步必须在单元分支之前——docker-proxy 跑在 docker.service
		// 的 cgroup 里，先按单元分类的话，它会变成“docker.service 在监听 19487”，
		// 于是一个容器在候选里出现两次（容器 + docker 单元），而且两次都“有理有据”。
		g = stripDockerProxyListeners(g, published)
		if len(g.listeners) == 0 {
			continue
		}
		if g.unit != "" {
			if u, ok := units[g.unit]; ok {
				candidates = append(candidates, unitCandidate(u, g, used))
				continue
			}
		}
		candidates = append(candidates, localCandidate(g, used))
	}

	SortCandidates(candidates)
	truncated := false
	if len(candidates) > MaxCandidates {
		candidates = candidates[:MaxCandidates]
		truncated = true
	}

	listenState.Count = countBySource(candidates, SourceLocalListen)
	unitState.Count = countBySource(candidates, SourceSystemd)
	dockerState.Count = countBySource(candidates, SourceDocker)
	sources = append(sources, unitState, listenState, dockerState)

	snap := Snapshot{
		Status:      aggregateStatus(sources),
		CollectedAt: now,
		Sources:     sources,
		Candidates:  candidates,
		Truncated:   truncated,
		Targets:     TargetsOf(candidates),
	}
	snap.Error = aggregateError(sources)
	return snap
}

// dockerContainers 采集容器清单，返回容器与该来源的状态。
func (c *Collector) dockerContainers(ctx context.Context) ([]Container, SourceState) {
	state := SourceState{Source: SourceDocker, Status: StatusOK}
	if c.deps.Docker == nil {
		state.Status = "unavailable"
		state.Error = "本机没有可用的容器运行时接口，容器来源未采集"
		return nil, state
	}
	containers, available, notice, err := c.deps.Docker.Containers(ctx)
	switch {
	case !available:
		state.Status = "unavailable"
		state.Error = firstNonEmpty(notice, "容器来源不可用")
		return nil, state
	case err != nil:
		state.Status = StatusError
		state.Error = err.Error()
		return nil, state
	}
	return containers, state
}

// containerCandidate 把一个容器变成候选。
func containerCandidate(ct Container, used map[string]int) Candidate {
	state := containerState(ct.State)
	name := ct.Name
	if name == "" {
		// 名字读不到时用短 ID：容器身份是 ID，名字只是显示。
		name = shortID(ct.ID)
	}
	listeners := make([]Listener, 0, len(ct.Ports))
	probeable := false
	for _, m := range ct.Ports {
		if m.Protocol != "" && m.Protocol != "tcp" {
			continue
		}
		loop := loopbackReachable(m.HostIP)
		if len(listeners) < MaxListenersPerCandidate {
			listeners = append(listeners, Listener{
				Address:  m.HostIP,
				Port:     m.HostPort,
				Protocol: ListenerProtocol,
				Loopback: loop,
			})
		}
		if loop && state == ObservedRunning {
			probeable = true
		}
	}
	reason := fmt.Sprintf("Docker 里的容器 %s（镜像 %s）状态是 %s", name, ct.Image, ct.State)
	if len(listeners) > 0 {
		reason += fmt.Sprintf("，发布了 %d 个宿主机端口", len(listeners))
	} else {
		reason += "，没有把任何端口发布到宿主机"
	}
	note := ""
	switch {
	case state != ObservedRunning:
		note = fmt.Sprintf("容器当前状态是 %s，健康检查要等它跑起来才有结果", ct.State)
	case len(listeners) == 0:
		note = "容器没有发布宿主机端口，面板无法从回环上探测它"
	case !probeable:
		note = "容器只在非回环地址上发布端口，面板不从那些地址探测"
	}
	port := 0
	if len(listeners) > 0 {
		port = listeners[0].Port
	}
	return Candidate{
		TargetID:        assignTargetID(TargetContainerPrefix, name, used),
		Kind:            KindDocker,
		Source:          SourceDocker,
		Name:            name,
		Title:           name,
		Reason:          reason,
		ObservedState:   state,
		Listeners:       listeners,
		SuggestedScheme: SuggestedScheme(port),
		SuggestedPort:   port,
		ProbeAvailable:  probeable,
		ProbeNote:       note,
		ContainerID:     ct.ID,
	}
}

// unitCandidate 把一个监听中的单元变成候选。
func unitCandidate(u UnitInfo, g listenerGroup, used map[string]int) Candidate {
	title := trimServiceSuffix(u.Unit)
	if title == "" {
		title = u.Unit
	}
	state := unitState(u.ActiveState)
	probeable, port, note := probeStatus(g.listeners, state)
	reason := fmt.Sprintf("systemd 单元 %s 处于 %s", u.Unit, u.ActiveState)
	if u.MainPID != nil {
		reason += fmt.Sprintf("，主进程 %d 正在监听", *u.MainPID)
	} else {
		reason += "，它的进程正在监听"
	}
	if u.Description != "" {
		reason += "（" + u.Description + "）"
	}
	return Candidate{
		TargetID:        assignTargetID(TargetLocalPrefix, title, used),
		Kind:            KindSystemd,
		Source:          SourceSystemd,
		Name:            u.Unit,
		Title:           title,
		Reason:          reason,
		ObservedState:   state,
		Listeners:       g.listeners,
		SuggestedScheme: SuggestedScheme(port),
		SuggestedPort:   port,
		ProbeAvailable:  probeable,
		ProbeNote:       note,
		Unit:            u.Unit,
	}
}

// localCandidate 把一个不属于任何可识别单元的监听变成候选。
// dropShadowedSocketActivation 去掉 pid 1 上“别人也在听”的那些监听。
//
// systemd 的 .socket 单元里，监听套接字由 pid 1 持有；服务被激活后守护进程自己也会
// 持有同一个端口（`ssh.socket` 与 sshd 的 22、rpcbind 的 111 都是这样）。不处理的话
// 候选列表里会出现两条一模一样的“端口 22”：一条是 sshd，一条是 pid 1。给人看的列表
// 里多一条分不清的东西不是“多给了信息”，是噪音——而且它会让“这个端口是哪个服务”这个
// 本来就有答案的问题变成一个选择题。
//
// 规则保守：pid 1 **独占**的监听照旧算候选（那才是“套接字开着、服务还没被激活”的真实
// 状态），别人也在听的端口归那个有人认领的进程。
func dropShadowedSocketActivation(groups []listenerGroup) []listenerGroup {
	claimed := make(map[string]struct{}, len(groups))
	for _, g := range groups {
		if g.pid == 1 {
			continue
		}
		for _, l := range g.listeners {
			claimed[listenerKey(l)] = struct{}{}
		}
	}
	if len(claimed) == 0 {
		return groups
	}

	out := make([]listenerGroup, 0, len(groups))
	for _, g := range groups {
		if g.pid == 1 {
			kept := make([]Listener, 0, len(g.listeners))
			for _, l := range g.listeners {
				if _, shadowed := claimed[listenerKey(l)]; shadowed {
					continue
				}
				kept = append(kept, l)
			}
			if len(kept) == 0 {
				continue
			}
			g.listeners = kept
		}
		out = append(out, g)
	}
	return out
}

// listenerKey 是一个监听项的身份：地址 + 端口 + 协议（同一个套接字表里这三样唯一）。
func listenerKey(l Listener) string {
	return l.Address + "/" + itoa(l.Port) + "/" + l.Protocol
}

func localCandidate(g listenerGroup, used map[string]int) Candidate {
	comm := g.comm
	label := comm
	if label == "" {
		label = "端口 " + itoa(firstPort(g.listeners))
	}
	title := label
	if comm != "" {
		title = comm + " (" + itoa(firstPort(g.listeners)) + ")"
	}
	probeable, port, note := probeStatus(g.listeners, ObservedRunning)
	var reason string
	switch {
	case g.pid == 1 && comm != "":
		// pid 1 的监听器不是“某个应用的进程”，是 systemd 自己代持的套接字（.socket
		// 单元）。别人也在听的端口已经在 dropShadowedSocketActivation 里归给了那个
		// 进程，所以走到这里的是它独占的——通常是服务还没被第一次连接激活。
		reason = fmt.Sprintf("systemd（pid 1）正在这个端口上等待连接，背后是一个还没被激活的 socket 单元")
	case g.pid > 0 && comm != "":
		reason = fmt.Sprintf("进程 %d（%s）正在监听，它不属于任何能被面板识别的 systemd 单元", g.pid, comm)
	case g.pid > 0:
		reason = fmt.Sprintf("进程 %d 正在监听，读不到它的命令名", g.pid)
	default:
		reason = "这个端口处于监听状态，但读不到持有它的进程（查找需要 root）"
	}
	return Candidate{
		TargetID:        assignTargetID(TargetLocalPrefix, title, used),
		Kind:            KindLocal,
		Source:          SourceLocalListen,
		Name:            comm,
		Title:           title,
		Reason:          reason,
		ObservedState:   ObservedRunning,
		Listeners:       g.listeners,
		SuggestedScheme: SuggestedScheme(port),
		SuggestedPort:   port,
		ProbeAvailable:  probeable,
		ProbeNote:       note,
	}
}

// probeStatus 决定一个候选此刻能不能探测，并给出建议端口。
//
// 建议端口取第一个能从回环连到的监听端口；一个都连不到时为 0，并说明为什么。
func probeStatus(listeners []Listener, state string) (bool, int, string) {
	for _, l := range listeners {
		if l.Loopback {
			if state != ObservedRunning {
				return false, l.Port, fmt.Sprintf("状态是 %s，健康检查要等它跑起来才有结果", state)
			}
			return true, l.Port, ""
		}
	}
	if len(listeners) == 0 {
		return false, 0, "没有观测到监听端口"
	}
	addrs := make([]string, 0, len(listeners))
	for _, l := range listeners {
		addrs = append(addrs, fmt.Sprintf("%s:%d", l.Address, l.Port))
	}
	return false, 0, fmt.Sprintf("只在 %s 上监听，127.0.0.1 上没有它；面板只从回环探测", joinList(addrs))
}

// stripDockerProxyListeners 摘掉“容器发布端口的宿主机侧监听”。
//
// 一个容器发布 `0.0.0.0:19487->80/tcp` 之后，宿主机上会有一个 docker-proxy（老版本是
// dockerd 自己）在 19487 上监听，把连接转进容器网络。这条监听在发现结果里没有信息量：
// 同一个服务已经以 `cntr_…` 候选出现过了，再算一条 `local_…` 只是让“本机有几个服务”
// 这个问题多一个假答案。
//
// 判据只有两个：端口在容器发布的宿主机端口集合里，且持有者读起来就是 docker 的代理
// （`docker-proxy` / `dockerd`）或读不到（面板没 root 时就是这样）。一个人自己的程序
// 监听在容器已占用的端口上是不成立的，所以这里不需要更细的身份判断；反过来，读不到
// 持有者时也照摘——宁可少一条噪音候选，不要把同一个服务数两遍。
func stripDockerProxyListeners(g listenerGroup, published map[int]struct{}) listenerGroup {
	if len(published) == 0 {
		return g
	}
	if !(g.comm == "" || g.comm == "docker-proxy" || g.comm == "dockerd") {
		return g
	}
	kept := make([]Listener, 0, len(g.listeners))
	for _, l := range g.listeners {
		if _, ok := published[l.Port]; ok {
			continue
		}
		kept = append(kept, l)
	}
	g.listeners = kept
	return g
}

// unitState 把 systemd 的 ActiveState 映射到观测枚举。
//
// reloading 是“在跑且在重载配置”，算 running；activating/deactivating 是过渡态，
// 既不是稳定运行也不是稳定停止，算 unknown。
func unitState(active string) string {
	switch active {
	case "active", "reloading":
		return ObservedRunning
	case "failed":
		return ObservedFailed
	case "inactive":
		return ObservedStopped
	}
	return ObservedUnknown
}

func trimServiceSuffix(unit string) string {
	const suffix = ".service"
	if len(unit) > len(suffix) && unit[len(unit)-len(suffix):] == suffix {
		return unit[:len(unit)-len(suffix)]
	}
	return unit
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	if id == "" {
		return "unnamed"
	}
	return id
}

func firstPort(listeners []Listener) int {
	for _, l := range listeners {
		if l.Loopback {
			return l.Port
		}
	}
	if len(listeners) > 0 {
		return listeners[0].Port
	}
	return 0
}

func countBySource(cs []Candidate, source string) int {
	n := 0
	for _, c := range cs {
		if c.Source == source {
			n++
		}
	}
	return n
}

// aggregateStatus 汇总来源状态：全失败是 error，部分失败是 partial。
func aggregateStatus(sources []SourceState) string {
	if len(sources) == 0 {
		return StatusError
	}
	okCount := 0
	for _, s := range sources {
		if s.Status == StatusOK {
			okCount++
		}
	}
	switch {
	case okCount == len(sources):
		return StatusOK
	case okCount == 0:
		return StatusError
	}
	return StatusPartial
}

// aggregateError 把失败来源的原因拼成一句可直接显示的话。
func aggregateError(sources []SourceState) string {
	var msgs []string
	for _, s := range sources {
		if s.Status != StatusOK && s.Error != "" {
			msgs = append(msgs, s.Source+"："+s.Error)
		}
	}
	return joinList(msgs)
}

func joinErrors(errs []string) string { return joinList(errs) }

func joinList(items []string) string {
	out := ""
	for i, item := range items {
		if i > 0 {
			out += "；"
		}
		out += item
	}
	return out
}
