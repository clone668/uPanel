package api

// P09.3：容器、镜像、网络、数据卷与部署（compose 项目）的只读视图，加容器日志流
// （CONTRACTS.md 第 8 节）。
//
// 三条纪律：
//
//  1. **只有 admin**。容器清单等于这台机器上跑了什么、发布了哪些端口、挂了哪些目录，
//     那是侦察情报（与日志同理，PLAN.md 第 14 节）。viewer 在这里得到 403。
//
//  2. **Web 进程不碰 docker**。这里没有任何 docker 调用、没有命令行拼接、不读
//     /var/run/docker.sock：所有读取都在代理里用固定 argv 完成，本层只转发结构化查询。
//     启停是写操作，走预检 → 任务链路（action=container.control），这里**没有**
//     “直接启停”的入口。
//
//  3. **危险容器显示风险，不给按钮**。是否受管由代理判决（privileged、host 网络/PID、
//     危险 capability、挂 docker.sock 或宿主根）。这里原样转发 risk 与 capabilities，
//     不自己算一遍——两份判断迟早会不一致，而不一致的那一份会变成“按钮点了被拒”。

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"upanel/internal/containers"
	"upanel/internal/httpx"
)

// ContainersReader 是容器读取端（P09.3）。生产实现是 *containers.Fetcher。
type ContainersReader interface {
	List(ctx context.Context) (containers.ListResponse, error)
	Get(ctx context.Context, id string) (containers.Container, error)
	Logs(ctx context.Context, params containers.LogParams) (containers.LogPage, error)
	Images(ctx context.Context) (containers.ImagesResponse, error)
	Networks(ctx context.Context) (containers.NetworksResponse, error)
	Volumes(ctx context.Context) (containers.VolumesResponse, error)
	Projects(ctx context.Context) (containers.ProjectsResponse, error)
	StreamWithReason(ctx context.Context, params containers.LogParams,
		emit func(containers.LogEntry) error) (string, error)
}

// handleListContainers 处理 GET /api/v1/containers。
func (s *Server) handleListContainers(c *gin.Context) {
	reader := s.opts.Containers
	if reader == nil {
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"容器不可用：未配置代理连接")
		return
	}
	out, err := reader.List(c.Request.Context())
	if err != nil {
		s.writeContainersError(c, err, "读取容器列表失败")
		return
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, out)
}

// handleGetContainer 处理 GET /api/v1/containers/:id。
func (s *Server) handleGetContainer(c *gin.Context) {
	reader := s.opts.Containers
	if reader == nil {
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"容器不可用：未配置代理连接")
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	if !containers.ValidContainerID(id) {
		// 名字与短 ID 在这里就拒：代理侧也只认完整 64 位十六进制，
		// 先拒一次是为了给 400，而不是把“参数写错”变成一次无意义的往返。
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError,
			"container_id 必须是完整的 64 位容器 ID")
		return
	}
	out, err := reader.Get(c.Request.Context(), id)
	if err != nil {
		s.writeContainersError(c, err, "读取容器失败")
		return
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, out)
}

// handleContainerLogs 处理 GET /api/v1/containers/:id/logs。
func (s *Server) handleContainerLogs(c *gin.Context) {
	params, ok := parseContainerLogParams(c)
	if !ok {
		return
	}
	reader := s.opts.Containers
	if reader == nil {
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"容器不可用：未配置代理连接")
		return
	}
	page, err := reader.Logs(c.Request.Context(), params)
	if err != nil {
		s.writeContainersError(c, err, "读取容器日志失败")
		return
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, page)
}

// handleContainerLogsStream 处理 GET /api/v1/containers/:id/logs/stream（SSE，仅 admin）。
//
// 与 /logs/stream 共用同一份“每个账号能同时开几条流”的额度：两者都是长连接，
// 分开计数只会让人以为可以开双份。
func (s *Server) handleContainerLogsStream(c *gin.Context) {
	params, ok := parseContainerLogParams(c)
	if !ok {
		return
	}
	reader := s.opts.Containers
	if reader == nil {
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"容器不可用：未配置代理连接")
		return
	}

	sess := sessionFrom(c)
	release, allowed := s.streams.acquire(sess.User.ID)
	if !allowed {
		writeError(c, http.StatusTooManyRequests, httpx.CodeValidationError,
			"同一个账号同时打开的流过多，请先关闭多余的标签页")
		return
	}
	defer release()

	// 开流之前先确认容器存在：SSE 一旦发出 200 与响应头，后面只能用一条 error
	// 事件说“容器不存在”，而客户端分不清那是“容器没了”还是“流断了”。
	// 代理侧的同一处也做了这件事；两边都做，是因为两条路径的 200 时机不同。
	if _, err := reader.Get(c.Request.Context(), params.ContainerID); err != nil {
		s.writeContainersError(c, err, "读取容器失败")
		return
	}

	h := c.Writer.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	c.Writer.WriteHeader(http.StatusOK)

	stream := &sseWriter{w: c.Writer, rc: http.NewResponseController(c.Writer)}
	if !stream.comment("connected") {
		return
	}

	ctx := c.Request.Context()
	entries := make(chan containers.LogEntry, 64)
	done := make(chan struct{})
	var (
		reason string
		err    error
	)
	go func() {
		defer close(done)
		reason, err = reader.StreamWithReason(ctx, params, func(entry containers.LogEntry) error {
			select {
			case entries <- entry:
				return nil
			case <-ctx.Done():
				// 写不动了：让上层收流，代理那边会跟着杀掉 docker logs。
				return ctx.Err()
			}
		})
	}()

	beat := time.NewTicker(sseHeartbeatInterval)
	defer beat.Stop()

	writeEntry := func(entry containers.LogEntry) bool {
		// id 用时间戳：EventSource 重连会把它放进 Last-Event-ID，
		// 前端据此知道“最后看到的是哪一条”，而不是靠消息序号猜。
		return stream.eventWithID(entry.Timestamp.UTC().Format("2006-01-02T15:04:05.000000000Z"),
			"entry", entry)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case entry := <-entries:
			if !writeEntry(entry) {
				return
			}
		case <-beat.C:
			if !stream.comment("keepalive") {
				return
			}
		case <-done:
			for {
				select {
				case entry := <-entries:
					if !writeEntry(entry) {
						return
					}
				default:
					if err != nil && !errors.Is(err, context.Canceled) {
						s.opts.Logger.Warn("容器日志流中断",
							"user_id", sess.User.ID,
							"container_id", params.ContainerID,
							"error", err)
						stream.eventWithID("", "error", map[string]any{
							"code":    "STREAM_FAILED",
							"message": err.Error(),
						})
						return
					}
					stream.eventWithID("", "end", map[string]any{"reason": reason})
					return
				}
			}
		}
	}
}

// handleListImages 处理 GET /api/v1/images。
func (s *Server) handleListImages(c *gin.Context) {
	reader := s.opts.Containers
	if reader == nil {
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"容器不可用：未配置代理连接")
		return
	}
	out, err := reader.Images(c.Request.Context())
	if err != nil {
		s.writeContainersError(c, err, "读取镜像列表失败")
		return
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, out)
}

// handleListContainerNetworks 处理 GET /api/v1/container-networks。
func (s *Server) handleListContainerNetworks(c *gin.Context) {
	reader := s.opts.Containers
	if reader == nil {
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"容器不可用：未配置代理连接")
		return
	}
	out, err := reader.Networks(c.Request.Context())
	if err != nil {
		s.writeContainersError(c, err, "读取容器网络失败")
		return
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, out)
}

// handleListContainerVolumes 处理 GET /api/v1/container-volumes。
func (s *Server) handleListContainerVolumes(c *gin.Context) {
	reader := s.opts.Containers
	if reader == nil {
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"容器不可用：未配置代理连接")
		return
	}
	out, err := reader.Volumes(c.Request.Context())
	if err != nil {
		s.writeContainersError(c, err, "读取容器数据卷失败")
		return
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, out)
}

// deploymentDetail 是 GET /api/v1/deployments/:id 的响应体。
//
// 它在项目清单之上补一份“这个项目现在有哪些容器、各自什么状态”：项目是容器的
// 归并视图，只给一个项目名等于让人再去容器列表里筛一遍。这里仍然只读——
// 0.1 不具备 Compose 整体更新能力（PLAN.md 第 12 节）。
type deploymentDetail struct {
	containers.Project
	Containers []containers.Container `json:"containers"`
}

// handleListDeployments 处理 GET /api/v1/deployments。
func (s *Server) handleListDeployments(c *gin.Context) {
	reader := s.opts.Containers
	if reader == nil {
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"容器不可用：未配置代理连接")
		return
	}
	out, err := reader.Projects(c.Request.Context())
	if err != nil {
		s.writeContainersError(c, err, "读取部署失败")
		return
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, out)
}

// handleGetDeployment 处理 GET /api/v1/deployments/:id。
func (s *Server) handleGetDeployment(c *gin.Context) {
	reader := s.opts.Containers
	if reader == nil {
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"容器不可用：未配置代理连接")
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	ctx := c.Request.Context()

	projects, err := reader.Projects(ctx)
	if err != nil {
		s.writeContainersError(c, err, "读取部署失败")
		return
	}
	var project *containers.Project
	for i := range projects.Projects {
		if projects.Projects[i].ID == id {
			project = &projects.Projects[i]
			break
		}
	}
	if project == nil {
		// 部署标识由项目名派生（dep_ + sha256 前缀），拿它反查不到就是“没有这个项目”。
		// 不按名字模糊匹配：那会把“名字改过”说成“找到了”。
		writeError(c, http.StatusNotFound, httpx.CodeNotFound, "没有这个部署")
		return
	}

	list, err := reader.List(ctx)
	if err != nil {
		s.writeContainersError(c, err, "读取部署下的容器失败")
		return
	}
	members := make([]containers.Container, 0, len(project.ContainerIDs))
	want := make(map[string]struct{}, len(project.ContainerIDs))
	for _, cid := range project.ContainerIDs {
		want[cid] = struct{}{}
	}
	for _, item := range list.Containers {
		if _, ok := want[item.ID]; ok {
			members = append(members, item)
		}
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, deploymentDetail{
		Project:    *project,
		Containers: members,
	})
}

// parseContainerLogParams 把路径与查询串读成 containers.LogParams。
//
// 容器 ID 来自路径（:id），时间窗口与条数来自查询串。容器 ID 必须是完整 64 位
// 十六进制：代理侧也只认这个形状，这里先拒一次是为了给 400 而不是把问题
// 变成一次无意义的往返。
func parseContainerLogParams(c *gin.Context) (containers.LogParams, bool) {
	id := strings.TrimSpace(c.Param("id"))
	if !containers.ValidContainerID(id) {
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError,
			"container_id 必须是完整的 64 位容器 ID")
		return containers.LogParams{}, false
	}
	params := containers.LogParams{
		ContainerID: id,
		Since:       strings.TrimSpace(c.Query("since")),
		Until:       strings.TrimSpace(c.Query("until")),
		Cursor:      strings.TrimSpace(c.Query("cursor")),
	}
	if raw := strings.TrimSpace(c.Query("tail")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > containers.MaxLogLines {
			writeError(c, http.StatusBadRequest, httpx.CodeValidationError,
				"tail 必须是 1—"+strconv.Itoa(containers.MaxLogLines)+" 的整数")
			return containers.LogParams{}, false
		}
		params.Tail = n
	}
	return params, true
}

// writeContainersError 把容器模块的错误翻译成 HTTP 状态。
//
// 三类分开：
//   - 代理明确拒绝（RejectedError）：带着它自己的状态码与码回去——容器不存在是 404、
//     日志驱动不支持是 422，把它们压成 500 会让“换一个容器”变成“等一会儿再试”；
//   - 代理不可用：503 AGENT_UNAVAILABLE，等一会儿会好；
//   - 其余：500。
func (s *Server) writeContainersError(c *gin.Context, err error, what string) {
	if errors.Is(err, context.Canceled) {
		return
	}
	var rejected *containers.RejectedError
	if errors.As(err, &rejected) {
		writeError(c, rejected.Status, httpx.Code(rejected.Code), rejected.Message)
		return
	}
	if errors.Is(err, containers.ErrAgentUnavailable) {
		s.opts.Logger.Warn(what+"：代理不可用", "error", err,
			"request_id", c.GetString(ctxKeyRequestID))
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"代理不可用，无法读取容器信息")
		return
	}
	s.opts.Logger.Error(what, "error", err, "request_id", c.GetString(ctxKeyRequestID))
	writeError(c, http.StatusInternalServerError, httpx.CodeInternalError, what)
}
