package containers

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"upanel/internal/syscmd"
)

// dockerCandidates 是 docker 命令的常规安装位置。
//
// 与 storage/shares/discovery 同一套做法：固定候选路径，绝不从 PATH 里找，
// 也绝不把调用方给的字符串当程序名（PLAN.md 第 4 节）。
var dockerCandidates = []string{"/usr/bin/docker", "/snap/bin/docker"}

// dockerSocket 是守护进程的 Unix 套接字。Web 账号不接触它：这里只有代理（root）在读。
var dockerSocket = "/var/run/docker.sock"

// 输出分隔符。
//
// 用 ASCII 单元分隔符而不是竖线：inspect 的 JSON 字段里有路径与标签，路径理论上可以
// 含竖线，而 JSON 编码会把控制字符转义成 \u001f，因此分隔符不会出现在数据里。
const fs = "\x1f"

// psFormat 是容器列表的固定格式。
//
// 最后两列（端口、标签）原样保留，前面的列都由 docker 自己生成。标签只用来取
// compose 的六个键，其余标签（可能含令牌）在解析时丢弃。
const psFormat = "{{.ID}}" + fs + "{{.Names}}" + fs + "{{.Image}}" + fs + "{{.State}}" + fs +
	"{{.Status}}" + fs + "{{.CreatedAt}}" + fs + "{{.Ports}}" + fs + "{{.Labels}}"

// inspectFormat 是单个/多个容器的固定 inspect 格式。
//
// 只取固定字段：状态、资源、命名空间、挂载、capability、镜像 ID 与 compose 标签。
// **没有** Config.Env、没有完整 Labels 映射——那里有数据库口令之类的秘密。
const inspectFormat = "{{.Id}}" + fs + "{{.Created}}" + fs + "{{.State.Status}}" + fs +
	// 直接取 StartedAt 的值，不调用 .Format：docker 29 的 inspect --format 把模板
	// 数据当成 interface{}，调 time 的方法会报“can't evaluate field Format in type
	// interface {}”，整份容器清单因此读不出来（验收 VM 上第一遍就是这么挂的）。
	// docker 自己打出来的就是 RFC3339Nano，够当状态版本的一味。
	"{{.State.StartedAt}}" + fs +
	"{{.RestartCount}}" + fs + "{{.HostConfig.RestartPolicy.Name}}" + fs +
	"{{.HostConfig.Privileged}}" + fs + "{{.HostConfig.NetworkMode}}" + fs +
	"{{.HostConfig.PidMode}}" + fs + "{{.HostConfig.IpcMode}}" + fs +
	"{{.HostConfig.NanoCpus}}" + fs + "{{.HostConfig.Memory}}" + fs +
	"{{json .Mounts}}" + fs + "{{json .HostConfig.CapAdd}}" + fs + "{{.Image}}" + fs +
	`{{index .Config.Labels "com.docker.compose.project"}}` + fs +
	`{{index .Config.Labels "com.docker.compose.service"}}` + fs +
	`{{index .Config.Labels "com.docker.compose.project.working_dir"}}` + fs +
	`{{index .Config.Labels "com.docker.compose.project.config_files"}}` + fs +
	`{{index .Config.Labels "com.docker.compose.oneoff"}}` + fs +
	`{{index .Config.Labels "com.docker.compose.container-number"}}`

// 限额。
const (
	// MaxContainers 是单次列出的容器上限。超出时截断并置 truncated=true：
	// 一份“看起来很完整”的截断清单比明说截断更糟。
	MaxContainers = 500
	// MaxImages / MaxNetworks / MaxVolumes 同理。
	MaxImages   = 2000
	MaxNetworks = 500
	MaxVolumes  = 2000
	// maxInspectIDs 是一次 inspect 带上的 ID 数量上限，避免命令行过长。
	maxInspectIDs = 200
)

// 错误分类。它们对管理员意味着完全不同的下一步。
var (
	// ErrUnavailable 表示本机没有可用的 Docker（没装命令或守护进程没跑）。
	ErrUnavailable = errors.New("containers: Docker 不可用")
	// ErrNotFound 表示容器不在本机。
	ErrNotFound = errors.New("containers: 容器不存在")
	// ErrLogsUnsupported 表示该容器的日志驱动不支持读取。
	ErrLogsUnsupported = errors.New("containers: 日志驱动不支持读取")
	// ErrStateChanged 表示预检到执行之间容器换了（重建或状态变了）。
	ErrStateChanged = errors.New("containers: 容器状态已变化")
)

// UnavailableError 带上“为什么不可用”的说明。
type UnavailableError struct{ Notice string }

func (e *UnavailableError) Error() string        { return e.Notice }
func (e *UnavailableError) Is(target error) bool { return target == ErrUnavailable }

// NotFoundError 带上容器 ID。
type NotFoundError struct{ ContainerID string }

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("容器 %s 不在本机", shortID(e.ContainerID))
}
func (e *NotFoundError) Is(target error) bool { return target == ErrNotFound }

// Client 通过固定的 docker 命令行读取容器、镜像、网络、数据卷与日志，并执行启停。
//
// 命令与参数都是常量；容器 ID 先过 64 位十六进制校验，**任何情况下**都不会把
// 名字、镜像或标签拼回命令行。
type Client struct {
	// Program 是 docker 的绝对路径；为空时按 dockerCandidates 探测。
	Program string
	// Socket 是守护进程套接字路径（测试可替换）。
	Socket string
	// Stat 用来判断套接字是否存在（测试可替换）。
	Stat func(string) (os.FileInfo, error)
	// Now 可替换时钟（测试用）。
	Now func() time.Time
	// Exec 可替换 docker 执行（测试用）：翻页要按窗口多次试，只有能看见每次的
	// argv 与输出，才能证明“窗口在加宽/收窄”而不是碰巧拿到一页。
	Exec func(ctx context.Context, timeout time.Duration, args ...string) (syscmd.Result, error)

	resolved   string
	resolvedOK bool
}

// dockerRun 执行一条 docker 命令：默认走 run，测试可换成假 docker。
func (c *Client) dockerRun(ctx context.Context, timeout time.Duration, args ...string) (syscmd.Result, error) {
	if c.Exec != nil {
		return c.Exec(ctx, timeout, args...)
	}
	return c.run(ctx, timeout, args...)
}

// NewClient 返回一个按候选路径探测 docker 的客户端。
func NewClient() *Client { return &Client{} }

// Availability 报告本机有没有可用的 Docker。
//
// 两种情况分开说：没有命令 vs 有命令但守护进程没跑。前者是“没装”，后者是
// “装了但停了”——把它们合成一句“Docker 不可用”会让人去查错方向。
func (c *Client) Availability() (bool, string) {
	if c.program() == "" {
		return false, "本机没有 docker 命令，容器功能不可用"
	}
	socket := c.Socket
	if socket == "" {
		socket = dockerSocket
	}
	stat := c.Stat
	if stat == nil {
		stat = os.Stat
	}
	if _, err := stat(socket); err != nil {
		return false, "Docker 守护进程没有运行（套接字不存在），容器功能不可用"
	}
	return true, ""
}

// program 返回 docker 的绝对路径（探测一次后记住）。
func (c *Client) program() string {
	if c.Program != "" {
		return c.Program
	}
	if c.resolvedOK {
		return c.resolved
	}
	for _, candidate := range dockerCandidates {
		if _, err := os.Stat(candidate); err == nil {
			c.resolved = candidate
			c.resolvedOK = true
			return candidate
		}
	}
	return ""
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

// run 执行一条固定的 docker 命令。
//
// 非零退出状态不当成“执行本身出错”：docker 对“容器不存在”“日志驱动不支持读取”
// 都是用退出码与 stderr 表达的，调用方按语义解释。超时与启动失败才是 error。
func (c *Client) run(ctx context.Context, timeout time.Duration, args ...string) (syscmd.Result, error) {
	program := c.program()
	if program == "" {
		return syscmd.Result{}, &UnavailableError{Notice: "本机没有 docker 命令，容器功能不可用"}
	}
	res, err := syscmd.Run(ctx, syscmd.Spec{Program: program, Args: args}, syscmd.Limits{
		Timeout:   timeout,
		MaxStdout: 8 << 20,
		MaxStderr: 64 << 10,
	})
	if err != nil {
		var exitErr *syscmd.ExitError
		if errors.As(err, &exitErr) {
			return res, nil
		}
		return res, fmt.Errorf("containers: 调用 docker 失败: %w", err)
	}
	return res, nil
}

// daemonDownPatterns 是 docker CLI 在“命令在、守护进程没应答”时给出的固定说法。
//
// 套接字存在不等于守护进程活着：dockerd 被停掉、被杀掉、或者 socket 被别的进程
// 占着时，socket 文件还在，而每条 docker 命令都会以这几句话失败。把这种情况
// 认成“命令用错了”会让人去查面板，而不是去把 Docker 起来。
var daemonDownPatterns = []string{
	"cannot connect to the docker daemon",
	"is the docker daemon running",
	"docker.sock: connect: no such file or directory",
	"docker.sock: connect: connection refused",
}

// daemonUnavailable 判断一次失败是不是“守护进程没应答”。
func daemonUnavailable(res syscmd.Result) *UnavailableError {
	lower := strings.ToLower(res.Stderr)
	for _, pattern := range daemonDownPatterns {
		if strings.Contains(lower, pattern) {
			return &UnavailableError{Notice: "Docker 守护进程没有应答（套接字在，但连不上），容器功能不可用"}
		}
	}
	return nil
}

// dockerFailure 把 docker 的非零退出翻成带 stderr 第一行的错误。
//
// “守护进程没应答”单独成一类（UnavailableError）：它是 503 DOCKER_UNAVAILABLE，
// 与“这条命令失败了”不是一回事。
func dockerFailure(what string, res syscmd.Result) error {
	if unavailable := daemonUnavailable(res); unavailable != nil {
		return unavailable
	}
	detail := strings.TrimSpace(res.Stderr)
	if detail == "" {
		detail = strings.TrimSpace(res.Stdout)
	}
	if detail == "" {
		detail = fmt.Sprintf("退出码 %d", res.ExitCode)
	}
	if idx := strings.Index(detail, "\n"); idx >= 0 {
		detail = detail[:idx]
	}
	return fmt.Errorf("containers: %s 失败：%s", what, detail)
}

// degradeUnavailable 把“守护进程没应答”变成清单类接口的降级结果。
//
// 清单类接口的约定是 200 + available=false + notice（CONTRACTS §8.3(5)）：
// 守护进程半路死掉不该变成 503，否则同一种状况在轮询里会时好时坏。
// 返回 true 表示已经降级，调用方不该再报错。
func degradeUnavailable(err error, set func(notice string)) bool {
	var unavailable *UnavailableError
	if errors.As(err, &unavailable) {
		set(unavailable.Notice)
		return true
	}
	return false
}

// List 返回本机全部容器（含已停止的）。
func (c *Client) List(ctx context.Context) (ListResponse, error) {
	resp := ListResponse{Containers: []Container{}, CollectedAt: c.now().Format(time.RFC3339)}
	available, notice := c.Availability()
	if !available {
		resp.Notice = notice
		return resp, nil
	}
	resp.Available = true

	res, err := c.dockerRun(ctx, 15*time.Second, "ps", "-a", "--no-trunc", "--format", psFormat)
	if err != nil {
		return resp, err
	}
	if res.ExitCode != 0 {
		err := dockerFailure("docker ps", res)
		if degradeUnavailable(err, func(notice string) {
			resp.Available = false
			resp.Notice = notice
			resp.Containers = []Container{}
		}) {
			return resp, nil
		}
		return resp, err
	}
	rows, err := parsePS(res.Stdout)
	if err != nil {
		return resp, err
	}
	if len(rows) > MaxContainers {
		rows = rows[:MaxContainers]
		resp.Truncated = true
	}
	if len(rows) == 0 {
		return resp, nil
	}

	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	facts, err := c.inspect(ctx, ids)
	if err != nil {
		if degradeUnavailable(err, func(notice string) {
			resp.Available = false
			resp.Notice = notice
			resp.Containers = []Container{}
		}) {
			return resp, nil
		}
		return resp, err
	}
	out := make([]Container, 0, len(rows))
	for _, row := range rows {
		container := Container{
			ID:        row.ID,
			Name:      row.Name,
			Image:     row.Image,
			StateText: row.StateText,
			CreatedAt: row.CreatedAt,
			Ports:     row.Ports,
			Mounts:    []Mount{},
			Compose:   row.Compose,
		}
		if f, ok := facts[row.ID]; ok {
			applyFacts(&container, f)
		} else {
			// inspect 没给事实时（理论上不会发生）仍然把这一行返回：一条“状态读不出来”
			// 的容器要好过它从清单里凭空消失。
			container.State = MapState(row.State)
		}
		container.Capabilities = container.ControlCapabilities()
		if container.Risk.Level == "" {
			container.Risk = Risk{Level: RiskNone, Reasons: []RiskReason{}}
		}
		if container.Ports == nil {
			container.Ports = []Port{}
		}
		out = append(out, container)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].State != out[j].State {
			// running 在前：这是“现在有哪些服务在跑”的清单。
			return out[i].State == StateRunning
		}
		return out[i].Name < out[j].Name
	})
	resp.Containers = out
	return resp, nil
}

// Get 读取单个容器。找不到时返回 NotFoundError。
func (c *Client) Get(ctx context.Context, id string) (Container, error) {
	if !ValidContainerID(id) {
		return Container{}, &ValidationError{Message: "container_id 必须是完整的 64 位容器 ID"}
	}
	if available, notice := c.Availability(); !available {
		return Container{}, &UnavailableError{Notice: notice}
	}
	facts, err := c.inspect(ctx, []string{id})
	if err != nil {
		return Container{}, err
	}
	f, ok := facts[id]
	if !ok {
		return Container{}, &NotFoundError{ContainerID: id}
	}
	container := Container{ID: id, Mounts: []Mount{}, Ports: []Port{}}
	applyFacts(&container, f)
	container.Capabilities = container.ControlCapabilities()
	if container.Risk.Level == "" {
		container.Risk = Risk{Level: RiskNone, Reasons: []RiskReason{}}
	}
	// 名字与端口不在 inspect 的固定字段里：单独读一次 ps 取那一行。
	// 读不到就保持空名字/空端口——id 已确认存在，缺行只可能是这一瞬间被删了，
	// 此时留空比分不清更诚实。
	if row, err := c.psFor(ctx, id); err == nil && row != nil {
		container.Name = row.Name
		container.StateText = row.StateText
		container.Ports = row.Ports
		if container.Image == "" {
			container.Image = row.Image
		}
		if container.Compose == nil {
			container.Compose = row.Compose
		}
	}
	return container, nil
}

// psFor 读取单个容器在 ps 里的那一行。
func (c *Client) psFor(ctx context.Context, id string) (*psRow, error) {
	res, err := c.dockerRun(ctx, 15*time.Second, "ps", "-a", "--no-trunc", "--format", psFormat)
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, dockerFailure("docker ps", res)
	}
	rows, err := parsePS(res.Stdout)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if rows[i].ID == id {
			return &rows[i], nil
		}
	}
	return nil, nil
}

// image 与 network 读取。

// Images 返回本机镜像。
func (c *Client) Images(ctx context.Context) (ImagesResponse, error) {
	resp := ImagesResponse{Images: []Image{}}
	if available, notice := c.Availability(); !available {
		resp.Notice = notice
		return resp, nil
	}
	resp.Available = true
	res, err := c.dockerRun(ctx, 20*time.Second, "images", "--no-trunc", "--digests", "--format",
		"{{.ID}}"+fs+"{{.Repository}}"+fs+"{{.Tag}}"+fs+"{{.Digest}}"+fs+"{{.Size}}"+fs+"{{.CreatedAt}}")
	if err != nil {
		return resp, err
	}
	if res.ExitCode != 0 {
		err := dockerFailure("docker images", res)
		if degradeUnavailable(err, func(notice string) {
			resp.Available = false
			resp.Notice = notice
			resp.Images = []Image{}
		}) {
			return resp, nil
		}
		return resp, err
	}
	rows, err := parseImages(res.Stdout)
	if err != nil {
		return resp, err
	}
	if len(rows) > MaxImages {
		rows = rows[:MaxImages]
		resp.Truncated = true
	}
	resp.Images = rows
	return resp, nil
}

// Networks 返回本机网络。
func (c *Client) Networks(ctx context.Context) (NetworksResponse, error) {
	resp := NetworksResponse{Networks: []Network{}}
	if available, notice := c.Availability(); !available {
		resp.Notice = notice
		return resp, nil
	}
	resp.Available = true
	res, err := c.dockerRun(ctx, 15*time.Second, "network", "ls", "--no-trunc", "--format",
		"{{.ID}}"+fs+"{{.Name}}"+fs+"{{.Driver}}"+fs+"{{.Scope}}")
	if err != nil {
		return resp, err
	}
	if res.ExitCode != 0 {
		err := dockerFailure("docker network ls", res)
		if degradeUnavailable(err, func(notice string) {
			resp.Available = false
			resp.Notice = notice
			resp.Networks = []Network{}
		}) {
			return resp, nil
		}
		return resp, err
	}
	rows, err := parseNetworkList(res.Stdout)
	if err != nil {
		return resp, err
	}
	if len(rows) > MaxNetworks {
		rows = rows[:MaxNetworks]
		resp.Truncated = true
	}
	// 子网要靠 inspect 才有。一条命令带上全部网络 ID（已校验为十六进制），
	// 失败时保留 ls 的结果——子网读不出来不该让整份网络清单消失。
	subnets, _ := c.networkSubnets(ctx, rows)
	for i := range rows {
		if s, ok := subnets[rows[i].ID]; ok {
			rows[i].Subnets = s.Subnets
			rows[i].Internal = s.Internal
		}
	}
	resp.Networks = rows
	return resp, nil
}

// Volumes 返回本机数据卷，并标出“现在被哪些容器挂着”。
//
// 不做 `docker volume inspect`：卷名是用户字符串，而本包的原则是不把用户字符串
// 拼进命令行；挂载点又是宿主路径，对“有哪些卷”这个问题没有帮助。使用关系来自
// 容器 inspect 的 Mounts（type=volume 的 Name）。
func (c *Client) Volumes(ctx context.Context) (VolumesResponse, error) {
	resp := VolumesResponse{Volumes: []Volume{}}
	if available, notice := c.Availability(); !available {
		resp.Notice = notice
		return resp, nil
	}
	resp.Available = true
	res, err := c.dockerRun(ctx, 15*time.Second, "volume", "ls", "--format",
		"{{.Name}}"+fs+"{{.Driver}}"+fs+"{{.Scope}}")
	if err != nil {
		return resp, err
	}
	if res.ExitCode != 0 {
		err := dockerFailure("docker volume ls", res)
		if degradeUnavailable(err, func(notice string) {
			resp.Available = false
			resp.Notice = notice
			resp.Volumes = []Volume{}
		}) {
			return resp, nil
		}
		return resp, err
	}
	volumes, err := parseVolumes(res.Stdout)
	if err != nil {
		return resp, err
	}
	usage, err := c.volumeUsage(ctx)
	if err != nil {
		return resp, err
	}
	for i := range volumes {
		if names, ok := usage[volumes[i].Name]; ok {
			volumes[i].InUseBy = names
		}
	}
	if len(volumes) > MaxVolumes {
		volumes = volumes[:MaxVolumes]
		resp.Truncated = true
	}
	resp.Volumes = volumes
	return resp, nil
}

// volumeUsage 返回“卷名 → 挂着它的容器名”。
func (c *Client) volumeUsage(ctx context.Context) (map[string][]string, error) {
	list, err := c.List(ctx)
	if err != nil {
		return nil, err
	}
	usage := map[string][]string{}
	for _, container := range list.Containers {
		for _, m := range container.Mounts {
			if !strings.EqualFold(m.Type, "volume") || m.Name == "" {
				continue
			}
			usage[m.Name] = append(usage[m.Name], container.Name)
		}
	}
	return usage, nil
}

// Projects 把容器按 compose 标签归并成项目。
//
// 这是**只读**视图：0.1 不创建、不更新、不删除 compose 项目，也不执行任何 YAML。
// 三个能力字段因此恒为 false——不是“还没接上”，是这个版本明确不做。
func (c *Client) Projects(ctx context.Context) (ProjectsResponse, error) {
	resp := ProjectsResponse{Projects: []Project{}}
	list, err := c.List(ctx)
	if err != nil {
		return resp, err
	}
	resp.Available = list.Available
	resp.Notice = list.Notice
	if !list.Available {
		return resp, nil
	}
	byProject := map[string]*Project{}
	order := make([]string, 0, 8)
	for _, container := range list.Containers {
		if container.Compose == nil || container.Compose.Project == "" {
			continue
		}
		name := container.Compose.Project
		project, ok := byProject[name]
		if !ok {
			project = &Project{
				ID:           ProjectID(name),
				Name:         name,
				WorkingDir:   container.Compose.WorkingDir,
				ConfigFiles:  container.Compose.ConfigFiles,
				Services:     []string{},
				ContainerIDs: []string{},
				Capabilities: ProjectCapabilities{},
			}
			byProject[name] = project
			order = append(order, name)
		}
		project.Total++
		if container.State == StateRunning {
			project.Running++
		}
		project.ContainerIDs = append(project.ContainerIDs, container.ID)
		if container.Compose.Service != "" {
			project.Services = append(project.Services, container.Compose.Service)
		}
	}
	sort.Strings(order)
	for _, name := range order {
		project := byProject[name]
		sort.Strings(project.Services)
		project.Services = dedupe(project.Services)
		sort.Strings(project.ContainerIDs)
		resp.Projects = append(resp.Projects, *project)
	}
	if len(resp.Projects) > MaxNetworks {
		resp.Projects = resp.Projects[:MaxNetworks]
		resp.Truncated = true
	}
	resp.ComposePlugin = c.composePlugin()
	return resp, nil
}

// composePlugin 报告本机是否装了 docker compose 插件（只用于说明清单来源）。
func (c *Client) composePlugin() bool {
	program := c.program()
	if program == "" {
		return false
	}
	_, err := os.Stat("/usr/libexec/docker/cli-plugins/docker-compose")
	if err == nil {
		return true
	}
	_, err = os.Stat("/usr/lib/docker/cli-plugins/docker-compose")
	return err == nil
}

// dedupe 去掉相邻重复（调用前已排序）。
func dedupe(in []string) []string {
	out := in[:0]
	for i, v := range in {
		if i == 0 || v != in[i-1] {
			out = append(out, v)
		}
	}
	return out
}

// shortID 把容器 ID 缩成前 12 位（只用于日志与错误消息）。
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// inspect 批量读取固定字段。
func (c *Client) inspect(ctx context.Context, ids []string) (map[string]inspectFacts, error) {
	out := make(map[string]inspectFacts, len(ids))
	for start := 0; start < len(ids); start += maxInspectIDs {
		end := start + maxInspectIDs
		if end > len(ids) {
			end = len(ids)
		}
		batch := ids[start:end]
		args := make([]string, 0, len(batch)+4)
		args = append(args, "inspect", "--type", "container", "--format", inspectFormat)
		for _, id := range batch {
			// 只有通过校验的 ID 才会进 argv。名字永远不进。
			if !ValidContainerID(id) {
				return nil, fmt.Errorf("containers: 拒绝把非容器 ID 放进命令行：%q", id)
			}
			args = append(args, id)
		}
		res, err := c.dockerRun(ctx, 20*time.Second, args...)
		if err != nil {
			return nil, err
		}
		if res.ExitCode != 0 {
			// inspect 对“其中一个不存在”会整体失败。这种情况按“不存在”处理，
			// 让调用方决定是 404 还是把这一条跳过。
			if strings.Contains(res.Stderr, "No such") {
				continue
			}
			return nil, dockerFailure("docker inspect", res)
		}
		parsed, err := parseInspect(res.Stdout)
		if err != nil {
			return nil, err
		}
		for id, f := range parsed {
			out[id] = f
		}
	}
	return out, nil
}

// networkSubnets 批量读取网络的子网与 internal 标志。
func (c *Client) networkSubnets(ctx context.Context, rows []Network) (map[string]struct {
	Subnets  []string
	Internal bool
}, error) {
	out := make(map[string]struct {
		Subnets  []string
		Internal bool
	}, len(rows))
	if len(rows) == 0 {
		return out, nil
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		// 网络 ID 也是 64 位十六进制；形状不对的就跳过（不猜）。
		if ValidContainerID(row.ID) {
			ids = append(ids, row.ID)
		}
	}
	if len(ids) == 0 {
		return out, nil
	}
	for start := 0; start < len(ids); start += maxInspectIDs {
		end := start + maxInspectIDs
		if end > len(ids) {
			end = len(ids)
		}
		args := append([]string{"network", "inspect", "--format",
			"{{.ID}}" + fs + "{{.Internal}}" + fs + "{{range .IPAM.Config}}{{.Subnet}} {{end}}"}, ids[start:end]...)
		res, err := c.dockerRun(ctx, 15*time.Second, args...)
		if err != nil {
			return out, err
		}
		if res.ExitCode != 0 {
			return out, dockerFailure("docker network inspect", res)
		}
		for _, line := range strings.Split(strings.TrimRight(res.Stdout, "\n"), "\n") {
			fields := strings.Split(line, fs)
			if len(fields) != 3 {
				continue
			}
			subnets := strings.Fields(fields[2])
			if subnets == nil {
				subnets = []string{}
			}
			out[strings.TrimSpace(fields[0])] = struct {
				Subnets  []string
				Internal bool
			}{Subnets: subnets, Internal: strings.TrimSpace(fields[1]) == "true"}
		}
	}
	return out, nil
}

// jsonMount 是 inspect 里 Mounts 的 JSON 形状（只取四个字段）。
type jsonMount struct {
	Type        string `json:"Type"`
	Name        string `json:"Name"`
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
}

// inspectFacts 是一个容器的 inspect 结论。
type inspectFacts struct {
	ID            string
	Created       string
	State         string
	StartedAt     string
	RestartCount  int
	RestartPolicy string
	Privileged    bool
	NetworkMode   string
	PidMode       string
	IpcMode       string
	NanoCPUs      int64
	Memory        int64
	Mounts        []Mount
	CapAdd        []string
	ImageID       string
	Compose       *ComposeRef
}

// applyFacts 把 inspect 结论写进 Container。
func applyFacts(c *Container, f inspectFacts) {
	c.ID = f.ID
	c.CreatedAt = normalizeDockerTime(f.Created)
	if f.StartedAt != "" && !strings.HasPrefix(f.StartedAt, "0001-01-01") {
		c.StartedAt = f.StartedAt
	}
	c.RestartCount = f.RestartCount
	c.RestartPolicy = f.RestartPolicy
	c.ImageID = f.ImageID
	c.Resources = Resources{CPULimitMilli: f.NanoCPUs / 1e6, MemoryLimit: f.Memory}
	c.Mounts = f.Mounts
	c.Compose = f.Compose
	c.Risk = AssessRisk(InspectFacts{
		Privileged:  f.Privileged,
		NetworkMode: f.NetworkMode,
		PidMode:     f.PidMode,
		IpcMode:     f.IpcMode,
		CapAdd:      f.CapAdd,
		Mounts:      f.Mounts,
	})
	c.State = MapState(f.State)
}

// normalizeDockerTime 把 inspect 的 RFC3339Nano 时间压成秒级 RFC3339（保留时区）。
func normalizeDockerTime(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || strings.HasPrefix(s, "0001-01-01") {
		return ""
	}
	ts, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return s
	}
	return ts.UTC().Format(time.RFC3339)
}
