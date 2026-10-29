package api

// P16.1：外部程序能力探测的只读出口（GET /api/v1/system/capabilities）。
//
// 这个接口存在的理由很具体：在这之前，面板对“这台机器上装了什么”一个字都说不出。
// 备份页要 restic、部署页要 docker、代理页要 nginx、防火墙页要 ufw，缺任何一个时
// 界面只能显示一条“读了但没读到”，而真正的原因（没装？装的是别的版本？跑在容器里
// 没有这个程序？）只有 SSH 进去才知道。管理员的第一句话永远是“我这台机器能做什么”。
//
// 三条纪律：
//
//  1. **只有 admin**。这份清单列的是这台机器装了哪些外部程序、什么版本、在哪，
//     合起来就是主机指纹。viewer 用不到它，泄露出去只对打算找漏洞的人有用
//     （与容器清单、部署定义同一个判断，PLAN §14）。
//
//  2. **`ready=false` 不是“什么都没有”**。代理还没探完时 `host_caps` 是 null，
//     前端必须画“还没探完”，不能画一张空表。这两个状态在视觉上必须不同——
//     空表等于告诉人“这台机器什么都没装”，那是一句假话。
//
//  3. **这里只是转述，不做判决**。`present` 只代表程序在固定目录里、版本读得到；
//     某个动作能不能真的做，仍然由那个动作自己的预检回答。把这份清单当准入条件
//     会造出第二套判决，两套判据迟早会打架。

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"upanel/internal/agentclient"
	"upanel/internal/hostcaps"
	"upanel/internal/httpx"
)

// CapabilitiesReader 是代理 capabilities 的读取端（P16.1）。
//
// 生产实现是 *agentclient.Client；演示模式下为 nil，接口回 503 并说明没有代理连接，
// 而不是给一份空清单——那会被读成“这台机器没有依赖”。
type CapabilitiesReader interface {
	Capabilities(ctx context.Context) (agentclient.Capabilities, error)
}

// systemCapabilitiesDTO 是 GET /api/v1/system/capabilities 的响应体。
type systemCapabilitiesDTO struct {
	// Ready 表示代理探过了没有。false 时 HostCaps 必定为 null。
	Ready bool `json:"ready"`
	// HostCaps 是探测报告。null 表示“还没探完”，不是“没有依赖”。
	HostCaps *hostcaps.Report `json:"host_caps"`
}

// handleSystemCapabilities 处理 GET /api/v1/system/capabilities。
//
// 权限 admin（见文件头第 1 条）。读的是代理的缓存快照，不在这个请求里探测：
// 这条端点会被页面反复刷新，让每次刷新都等十几条版本命令是不合适的。
func (s *Server) handleSystemCapabilities(c *gin.Context) {
	reader := s.opts.Capabilities
	if reader == nil {
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"外部程序能力不可用：未配置代理连接")
		return
	}
	caps, err := reader.Capabilities(c.Request.Context())
	if err != nil {
		s.writeCapabilitiesError(c, err)
		return
	}
	out := systemCapabilitiesDTO{Ready: caps.HostCaps != nil, HostCaps: caps.HostCaps}
	// 代理说 ready 但报告是空的条目表：那不是“没有依赖”，是代理表里一条都没有。
	// 这两种情况在这里都原样转述，不在 Web 侧补一个“大概是这样”。
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, out)
}

// writeCapabilitiesError 把代理侧的错误映射成 HTTP 状态。
func (s *Server) writeCapabilitiesError(c *gin.Context, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	var unavailable *agentclient.UnavailableError
	if errors.As(err, &unavailable) {
		s.opts.Logger.Warn("读取外部程序能力失败：代理不可用", "error", err,
			"request_id", c.GetString(ctxKeyRequestID))
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"代理不可用，暂时读不到外部程序能力")
		return
	}
	// 这条接口没有任何业务拒绝分支（没有参数、没有对象），剩下的都是需要人看日志的故障。
	s.opts.Logger.Error("读取外部程序能力失败", "error", err,
		"request_id", c.GetString(ctxKeyRequestID))
	writeError(c, http.StatusInternalServerError, httpx.CodeCommandFailed,
		"外部程序能力读取失败")
}
