package api

// 网络与防火墙的只读接口（P14.1，CONTRACTS.md 第 14.1 节）。
//
// 四个接口都是 admin：网卡、路由、监听端口与防火墙规则合起来是"这台机器上开了
// 什么入口、通向哪里"，属于侦察材料，viewer 得到 403。
//
// 这一组**不提供任何变更入口**。改地址是 P14.2 的 network.apply，改规则是 P14.3 的
// firewall.apply，两者都要走预检 → 任务（确认、独立回滚 timer、审计）。在这里开一个
// "直接 netplan apply"或"直接 ufw allow"的路由，等于把那条链路上的每一道检查都绕过去
// ——所以这里只有 GET。
//
// 响应直接复用 internal/network 的 DTO：代理侧已经是同一份结构，多一层拷贝只会
// 让两边的字段慢慢漂开。判断（能不能改）也在代理侧算好了，因为它需要 /run 下的
// 渲染产物与 sysfs 事实，Web 拿不到。

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"

	"upanel/internal/httpx"
	"upanel/internal/network"
)

// NetworkReader 是四条只读查询的读取端（生产实现是 *agentclient.Client）。
//
// 接口定义在消费方：测试给假实现；生产里没有"固定成功"的替身——没接代理时
// Network 为 nil，四个接口回 503 AGENT_UNAVAILABLE，而不是给一份空网卡列表
// ——空列表会被读成"这台机器没有网卡"。
type NetworkReader interface {
	NetworkInterfaces(ctx context.Context) (network.InterfaceList, error)
	NetworkRoutes(ctx context.Context) (network.RouteList, error)
	NetworkListeners(ctx context.Context) (network.ListenerList, error)
	FirewallState(ctx context.Context) (network.FirewallState, error)
	// NetworkConfig 是 P14.2 加的第五条：可改网卡的生效配置 + config_hash + 待确认的回滚。
	// 它与四个只读查询同一组、同一个权限（admin），因为它同样不改任何字节；
	// 但它读的是**写路径的入口状态**——没有它，页面给不出一个能提交的表单。
	NetworkConfig(ctx context.Context) (network.ConfigState, error)
	// FirewallConfig 是 P14.3 加的第六条：面板管理的防火墙规则 + config_hash +
	// 待确认的回滚。与 NetworkConfig 同一性质——只读、不改任何字节，但它读的是
	// **写路径的入口状态**，没有它页面给不出一个能提交的表单。
	FirewallConfig(ctx context.Context) (network.FirewallConfigState, error)
}

// handleListNetworkInterfaces 处理 GET /api/v1/network/interfaces（admin）。
func (s *Server) handleListNetworkInterfaces(c *gin.Context) {
	reader := s.opts.Network
	if reader == nil {
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"网卡列表不可用：未配置代理连接")
		return
	}
	res, err := reader.NetworkInterfaces(c.Request.Context())
	if err != nil {
		s.writeNetworkError(c, err, "读取网卡列表失败", "网卡列表")
		return
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, res)
}

// handleListNetworkRoutes 处理 GET /api/v1/network/routes（admin）。
func (s *Server) handleListNetworkRoutes(c *gin.Context) {
	reader := s.opts.Network
	if reader == nil {
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"路由表不可用：未配置代理连接")
		return
	}
	res, err := reader.NetworkRoutes(c.Request.Context())
	if err != nil {
		s.writeNetworkError(c, err, "读取路由表失败", "路由表")
		return
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, res)
}

// handleListNetworkListeners 处理 GET /api/v1/network/listeners（admin）。
func (s *Server) handleListNetworkListeners(c *gin.Context) {
	reader := s.opts.Network
	if reader == nil {
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"监听端口不可用：未配置代理连接")
		return
	}
	res, err := reader.NetworkListeners(c.Request.Context())
	if err != nil {
		s.writeNetworkError(c, err, "读取监听端口失败", "监听端口")
		return
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, res)
}

// handleGetFirewall 处理 GET /api/v1/firewall（admin）。
func (s *Server) handleGetFirewall(c *gin.Context) {
	reader := s.opts.Network
	if reader == nil {
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"防火墙状态不可用：未配置代理连接")
		return
	}
	res, err := reader.FirewallState(c.Request.Context())
	if err != nil {
		s.writeNetworkError(c, err, "读取防火墙状态失败", "防火墙状态")
		return
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, res)
}

// handleGetNetworkConfig 处理 GET /api/v1/network/config（admin，P14.2）。
//
// 它回的是"写路径的入口状态"：能不能改（capability）、改的时候要带的 config_hash、
// 现在这张网卡上的地址，以及有没有一条还没确认的回滚（pending，带剩余秒数）。
//
// 三条响应语义：
//   - 未接线 → 503：不给一份看起来能用的空配置，那会让页面画出"可以改"的表单；
//   - 代理不可用 → 503（等一会儿会好），不是 500；
//   - 能不能改由代理判（要读 /run 下的渲染产物与 sysfs），Web 不改写、不补充。
func (s *Server) handleGetNetworkConfig(c *gin.Context) {
	reader := s.opts.Network
	if reader == nil {
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"网络配置不可用：未配置代理连接")
		return
	}
	res, err := reader.NetworkConfig(c.Request.Context())
	if err != nil {
		s.writeNetworkError(c, err, "读取网络配置失败", "网络配置")
		return
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, res)
}

// handleGetFirewallConfig 处理 GET /api/v1/firewall/config（admin，P14.3）。
//
// 与 /network/config 同一形状、同一理由：回的是写路径的入口状态——能不能改
// （editable）、改的时候要带的 config_hash、面板管理的规则集，以及有没有一条
// 还没确认的回滚（pending，带剩余秒数）。
//
// 一个刻意的差别：**它只列面板管理的规则**（带 `upanel:` 标记的那些）。管理员手写的
// 规则在这里不出现，因为面板不会去动它们——把它们混进"可以编辑的列表"，下一步就
// 会有人以为删掉一行只是删掉一个显示项。手写规则仍然在 GET /firewall 里完整可见。
//
// 三条响应语义同 /network/config：未接线 → 503（不给一份看起来能改的空表）、
// 代理不可用 → 503、能不能改由代理判（要跑 ufw、要看监听表），Web 不改写。
func (s *Server) handleGetFirewallConfig(c *gin.Context) {
	reader := s.opts.Network
	if reader == nil {
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"防火墙配置不可用：未配置代理连接")
		return
	}
	res, err := reader.FirewallConfig(c.Request.Context())
	if err != nil {
		s.writeNetworkError(c, err, "读取防火墙配置失败", "防火墙配置")
		return
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, res)
}

// writeNetworkError 把读取错误翻成 HTTP 状态。//
// 四种读取同一张翻译表：代理不可用 → 503（等一会儿会好）；代理明确拒绝 → 按它给的码；
// 其余 → 500，日志里留完整错误。
//
// **没有**"降级成空列表"的分支：读不到和"这台机器没有网卡"在界面上必须能分开。
// network 包的 issues[] 是数据的一部分（某一条来源读不到时照出其余部分），
// 那是"部分降级"，不是接口失败——两者不能混成一个 200 空表。
func (s *Server) writeNetworkError(c *gin.Context, err error, what, noun string) {
	s.writeMirrorError(c, what, noun, err)
}
