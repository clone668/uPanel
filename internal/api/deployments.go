package api

// P15.4：面板管理的 Compose 部署——列表与详情（管理面读接口）。
//
// 与 containers.go 里的 /deployments 是**两张表，两个问题**：
//
//	GET /api/v1/deployments            这台机器上跑着什么 compose 项目（事实来自 docker）
//	GET /api/v1/deployments/managed    面板管着什么部署（事实来自配置目录与 systemd）
//
// 混成一张表就会有一边读错：拿项目清单当"我的部署"，别人手工起在宿主机上的项目
// 会变成一条点不动的部署；拿部署配置当"机器上跑着什么"，配置被删掉但容器还在跑的
// 项目会凭空消失。前端按下发的 deployment_id 把两张表连起来（与 compose 项目标识
// 同一个值），谁也不冒充谁。
//
// 三条纪律：
//  1. **只有 admin**：部署的定义里有环境变量名字、镜像、宿主端口与挂载路径，
//     那是这台机器的侦察情报（与容器清单同理，PLAN §14）。
//  2. **Web 不碰 docker、不碰 systemd**：这里没有任何命令拼接，读事实全在代理里
//     用固定 argv 完成。
//  3. **这是读接口**：创建、更新、删除一律走 /operations/preflight + /jobs
//     （deployment.apply / update / remove）。这里没有"直接部署"的按钮，
//     也没有任何接受原始 Compose YAML 的入口——定义是闭集（CONTRACTS §8.2），
//     privileged / devices / network_mode / cap_add 在数据模型里没有位置。

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"upanel/internal/agentclient"
	"upanel/internal/deploy"
	"upanel/internal/httpx"
)

// DeploymentsReader 是"面板管理的部署"读取端（P15.4）。
//
// 生产实现是 *agentclient.Client（方法在 internal/agentclient/deploy.go），
// 背后是代理的两条只读查询 deployments.list / deployments.get。演示模式下为 nil，
// 两个接口回 503 并说明没有代理连接——而不是给一份空列表（那是"这台机器上没有部署"，
// 一句完全不同的话）。
type DeploymentsReader interface {
	ListManagedDeployments(ctx context.Context) (deploy.InspectionList, error)
	GetManagedDeployment(ctx context.Context, name string) (deploy.Inspection, error)
}

// handleListManagedDeployments 处理 GET /api/v1/deployments/managed。
func (s *Server) handleListManagedDeployments(c *gin.Context) {
	reader := s.opts.Deployments
	if reader == nil {
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"部署不可用：未配置代理连接")
		return
	}
	out, err := reader.ListManagedDeployments(c.Request.Context())
	if err != nil {
		s.writeDeploymentsError(c, err, "读取部署列表失败")
		return
	}
	if out.Deployments == nil {
		// 空数组而不是 null：前端按数组处理，"null"会变成一次多余的判空。
		out.Deployments = []deploy.Inspection{}
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, out)
}

// handleGetManagedDeployment 处理 GET /api/v1/deployments/managed/:name。
func (s *Server) handleGetManagedDeployment(c *gin.Context) {
	reader := s.opts.Deployments
	if reader == nil {
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"部署不可用：未配置代理连接")
		return
	}
	name := strings.TrimSpace(c.Param("name"))
	if !deploy.ValidDeploymentName(name) {
		// 名字不合规在本地就拒：代理侧也只认这个形状，先拒一次是为了给 400，
		// 而不是把"参数写错"变成一次无意义的往返。
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError,
			"部署名必须是部署名规则允许的形状：小写字母开头，只含小写字母、数字、下划线、中划线，"+
				"最长 63 字符")
		return
	}
	out, err := reader.GetManagedDeployment(c.Request.Context(), name)
	if err != nil {
		s.writeDeploymentsError(c, err, "读取部署失败")
		return
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, out)
}

// writeDeploymentsError 把部署读取端的错误翻译成 HTTP 状态。
//
// 三类分开，与预检、容器的口径一致：
//   - 代理连不上：503 AGENT_UNAVAILABLE，等一会儿会好；
//   - 代理回了域内判决（404 没有这份部署、400 参数不对、503 配置目录读不了）：
//     如实带出它的状态码与错误码。把 404 压成 500 会让页面提示"稍后重试"，
//     而重试一万次也不会有这份部署；
//   - 其余：500。
func (s *Server) writeDeploymentsError(c *gin.Context, err error, what string) {
	if errors.Is(err, context.Canceled) {
		return
	}
	var unavailable *agentclient.UnavailableError
	if errors.As(err, &unavailable) {
		s.opts.Logger.Warn(what+"：代理不可用", "error", err,
			"request_id", c.GetString(ctxKeyRequestID))
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"代理不可用，暂时读不到部署")
		return
	}
	var agentErr *agentclient.Error
	if errors.As(err, &agentErr) {
		code := httpx.Code(agentErr.Code)
		if !knownCode(code) {
			// 代理给了枚举外的错误码：不转发给前端，按内部错误处理。
			s.opts.Logger.Error("代理返回未知错误码", "code", agentErr.Code,
				"request_id", c.GetString(ctxKeyRequestID))
			writeError(c, http.StatusInternalServerError, httpx.CodeInternalError, what)
			return
		}
		if code != httpx.CodeNotFound {
			// 404 是正常结论（这份部署不在了），不值得一条 WARN；
			// 其余状态码说明页面上有东西不正常，留一条。
			s.opts.Logger.Warn(what, "status", agentErr.Status, "code", agentErr.Code,
				"request_id", c.GetString(ctxKeyRequestID))
		}
		writeError(c, httpx.StatusForCode(code), code, agentErr.Message)
		return
	}
	s.opts.Logger.Error(what, "error", err, "request_id", c.GetString(ctxKeyRequestID))
	writeError(c, http.StatusInternalServerError, httpx.CodeInternalError, what)
}
