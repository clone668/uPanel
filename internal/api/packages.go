package api

// 软件包的只读接口（P11.1，CONTRACTS.md 第 12.1 节）。
//
// 三个接口都是 admin：包列表里有内网服务名、版本号与补丁状态，属于主机内部信息；
// 一个 viewer 账号不需要知道这台机器上装的 OpenSSH 是哪个版本。
//
// 这一组**不提供任何变更入口**。安装、卸载、升级、刷新索引是 P11.2 的动作，走
// 预检 → 任务链路（POST /operations/preflight + POST /jobs），并且有保护名单、
// 资源锁与不可取消阶段。在这里开一个"直接 apt-get"的路由，等于把那条链路上的
// 每一道检查都绕过去——所以这里只有 GET。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"upanel/internal/httpx"
	"upanel/internal/packages"
)

// slowQueryBudget 是这两个接口允许占用的写超时。
//
// Web 的 http.Server 写超时是 30 秒（给普通请求用的）。包列表在缓存过期后的第一次
// 请求要现采：`apt-cache policy` 在 apt 2.8 上要 21～27 秒，几乎贴着 30 秒，
// 真机上再碰上几个 systemctl 探针并发就会越过去。越过之后连接被切断，浏览器看到的是
// 空响应——和一个真的失败长得一模一样。所以这两个接口单独把自己的写截止时间放长，
// 由上游的 packages.AgentQueryTimeout（45 秒）先一步给出结论，再由这里如实写完。
const slowQueryBudget = 60 * time.Second

// extendWriteDeadline 把这一条请求的写截止时间往后推。
//
// 失败就算了：不是所有 ResponseWriter 都支持（测试里的 httptest 支持），
// 那也没关系，退回服务器自己的 30 秒。
func extendWriteDeadline(c *gin.Context, d time.Duration) {
	if rc := http.NewResponseController(c.Writer); rc != nil {
		_ = rc.SetWriteDeadline(time.Now().Add(d))
	}
}

// PackagesReader 是软件包判断层的读取端（P11.1）。生产实现是 *packages.Service。
//
// 接口定义在消费方：测试可以给假实现；生产里没有"固定成功"的替身——
// 没配置代理时 Packages 为 nil，三个接口回 503 AGENT_UNAVAILABLE。
type PackagesReader interface {
	List(ctx context.Context, q packages.ListQuery) (packages.List, error)
	Updates(ctx context.Context) (packages.Updates, error)
	PowerState(ctx context.Context) (packages.PowerStateView, error)
	// PowerJobCheck 给一条已完成的电源任务补一次 boot_id 核对（P11.3）。
	// 不适用时返回 nil；永不为它返回错误——它是任务的附带说明，不能拖垮任务查询。
	PowerJobCheck(ctx context.Context, action string, result json.RawMessage) *packages.PowerCheckView
}

// handleListPackages 处理 GET /api/v1/packages（admin）。
//
// 参数：query / installed / upgradable / security / limit / cursor。
// installed 默认 true：只列出真正装着的包；显式传 installed=false 表示把仅有配置
// 残留（`rc`）的包也算进来。
func (s *Server) handleListPackages(c *gin.Context) {
	reader := s.opts.Packages
	if reader == nil {
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"软件包列表不可用：未配置代理连接")
		return
	}
	limit, ok := listLimitMax(c, packages.MaxLimit)
	if !ok {
		return
	}
	installed, ok := boolQueryParam(c, "installed", true)
	if !ok {
		return
	}
	upgradable, ok := boolQueryParam(c, "upgradable", false)
	if !ok {
		return
	}
	security, ok := boolQueryParam(c, "security", false)
	if !ok {
		return
	}
	q := packages.ListQuery{
		Query:      strings.TrimSpace(c.Query("query")),
		Installed:  installed,
		Upgradable: upgradable,
		Security:   security,
		Limit:      limit,
		Cursor:     strings.TrimSpace(c.Query("cursor")),
	}
	extendWriteDeadline(c, slowQueryBudget)
	res, err := reader.List(c.Request.Context(), q)
	if err != nil {
		s.writePackagesError(c, err, "读取软件包列表失败", "软件包列表")
		return
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, res)
}

// handleListPackageUpdates 处理 GET /api/v1/packages/updates（admin）。
func (s *Server) handleListPackageUpdates(c *gin.Context) {
	reader := s.opts.Packages
	if reader == nil {
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"可升级清单不可用：未配置代理连接")
		return
	}
	extendWriteDeadline(c, slowQueryBudget)
	res, err := reader.Updates(c.Request.Context())
	if err != nil {
		s.writePackagesError(c, err, "读取可升级清单失败", "可升级清单")
		return
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, res)
}

// handlePowerState 处理 GET /api/v1/system/power-state（admin）。
func (s *Server) handlePowerState(c *gin.Context) {
	reader := s.opts.Packages
	if reader == nil {
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"重启标志不可用：未配置代理连接")
		return
	}
	res, err := reader.PowerState(c.Request.Context())
	if err != nil {
		s.writePackagesError(c, err, "读取重启标志失败", "重启标志")
		return
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, res)
}

// writePackagesError 把读取错误翻译成 HTTP 状态。
//
// 三种情况分开，因为调用方该做的事完全不同：
//   - 代理不可用 → 503，页面说"代理没连上"；
//   - 参数/游标不合法 → 400，页面说明白哪个参数错了；
//   - 其余 → 500，日志里留完整错误。
//
// 特别注意：**没有**"降级成空列表"的分支。apt-cache 读不到是代理侧用 errors[]
// 表达的降级（包列表照出、候选 unknown），那是数据本身的一部分，不是接口失败。
//
// what 是日志用的完整动作（"读取重启标志失败"），noun 是给它后面那个名词
// （"重启标志"）。两个都收，是因为 503 的文案要念给用户听：三个接口共用一个
// 函数，但它不能说成同一个东西——`/system/power-state` 报"无法读取软件包状态"
// 会让人去查错模块（P12.1 真机复现时撞到）。
func (s *Server) writePackagesError(c *gin.Context, err error, what, noun string) {
	if errors.Is(err, context.Canceled) {
		return
	}
	if errors.Is(err, packages.ErrInvalidQuery) || errors.Is(err, packages.ErrInvalidCursor) {
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError, err.Error())
		return
	}
	if errors.Is(err, packages.ErrFactsUnavailable) {
		s.opts.Logger.Warn(what+"：代理不可用", "error", err,
			"request_id", c.GetString(ctxKeyRequestID))
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"代理不可用，无法读取"+noun)
		return
	}
	s.opts.Logger.Error(what, "error", err, "request_id", c.GetString(ctxKeyRequestID))
	writeError(c, http.StatusInternalServerError, httpx.CodeInternalError, what)
}

// boolQueryParam 解析一个布尔查询参数，缺省时取 def。
//
// 只认 true/false/1/0：`installed=yes` 这种含混的写法一律 400，而不是悄悄当成
// false——那会让页面显示一份完全不同的列表而不报错。
func boolQueryParam(c *gin.Context, name string, def bool) (bool, bool) {
	raw := strings.TrimSpace(c.Query(name))
	if raw == "" {
		return def, true
	}
	switch raw {
	case "true", "1":
		return true, true
	case "false", "0":
		return false, true
	}
	if _, err := strconv.ParseBool(raw); err == nil {
		// 例如 "TRUE"、"False"：ParseBool 认得，接受。
		v, _ := strconv.ParseBool(raw)
		return v, true
	}
	writeError(c, http.StatusBadRequest, httpx.CodeValidationError,
		name+" 必须是 true 或 false")
	return false, false
}
