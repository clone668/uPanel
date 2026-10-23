package api

// 预检参数的**服务端展开**（P10.2）。
//
// 绝大多数动作的参数由调用方给出、由代理校验，Web 只做形状与体积检查。proxy.apply
// 是例外：它的参数必须等于"面板库里的这条记录此刻的样子"，而不是调用方说它是什么。
// 于是 Web 在转给代理之前把 {site_id} 展开成完整的 ApplyParams：
//
//   - 定义来自库（P10.1 已经通过全部校验的那一份），不是请求体；
//   - panel_port 来自面板真实的监听地址，不是请求体；
//   - version 来自记录，用于把"预检的是哪一版"钉死。
//
// 为什么不做成"调用方传定义、代理校验"：那条路径上，调用方可以传一份**没经过记录
// 校验**的定义（比如上游端口正是面板端口，或者 access_mode 与账号列表自相矛盾），
// 而 HTTP 入口对任何 admin 都开着。展开之后，能落进 nginx 的东西必然是"库里已经
// 存在的那条记录"，这与 P10.1 的"登记与判断"是同一份真相。
//
// 展开失败时按契约给出 400/404/503，而不是把一份缺字段的参数交给代理去拒绝——
// 那会让用户看到"参数不合法"，而真正的问题是"这条记录已经不在了"。

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"upanel/internal/httpx"
	"upanel/internal/proxies"
)

// proxyApplyParamsRequest 是调用方交给 proxy.apply 的唯一字段。
type proxyApplyParamsRequest struct {
	SiteID string `json:"site_id"`
}

// expandPreflightParams 在把参数交给预检服务之前做服务端展开。
//
// 返回 false 表示已经写出错误响应，调用方应当直接 return。
func (s *Server) expandPreflightParams(c *gin.Context, action string, params json.RawMessage) (json.RawMessage, bool) {
	if action != proxies.ActionProxyApply {
		return params, true
	}
	if s.opts.Proxies == nil {
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"代理站点服务不可用，无法生成 apply 参数")
		return nil, false
	}
	if s.opts.PanelPort <= 0 {
		// 拿不到面板端口就拒绝：展开出来的参数会缺 panel_port，而代理对"上游端口
		// 不得等于面板端口"这条规则只能据此执行。宁可不做，也不给一份判不了的请求。
		s.internalError(c, "预检展开失败", errors.New("api: PanelPort 未配置，无法执行 panel_port 保护"))
		return nil, false
	}

	var req proxyApplyParamsRequest
	if len(params) > 0 {
		dec := json.NewDecoder(strings.NewReader(string(params)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeError(c, http.StatusBadRequest, httpx.CodeValidationError,
				"proxy.apply 的参数只有 site_id 一个字段："+err.Error())
			return nil, false
		}
	}
	siteID := strings.TrimSpace(req.SiteID)
	if !proxies.ValidSiteID(siteID) {
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError,
			"site_id 必须是 site_ 加 32 位小写十六进制")
		return nil, false
	}

	site, err := s.opts.Proxies.Get(c.Request.Context(), siteID)
	if err != nil {
		if errors.Is(err, proxies.ErrNotFound) {
			writeError(c, http.StatusNotFound, httpx.CodeNotFound, "代理站点不存在："+siteID)
			return nil, false
		}
		writeError(c, http.StatusInternalServerError, httpx.CodeInternalError, "读取代理站点失败")
		return nil, false
	}

	def := site.Definition
	expanded := proxies.ApplyParams{
		SiteID:     site.ID,
		Version:    site.Version,
		Definition: &def,
		PanelPort:  s.opts.PanelPort,
	}
	raw, err := json.Marshal(expanded)
	if err != nil {
		s.internalError(c, "预检展开失败", err)
		return nil, false
	}
	return raw, true
}
