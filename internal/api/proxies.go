package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"upanel/internal/audit"
	"upanel/internal/auth"
	"upanel/internal/httpx"
	"upanel/internal/nginx"
	"upanel/internal/proxies"
)

// 反向代理站点（P10.1）。
//
// 这一阶段做的事只有三件：
//  1. 把站点定义登记进面板库（domain、upstream、访问策略、请求限制）；
//  2. 写入前判断域名冲突——主机上别的配置是不是已经声明了这个域名；
//  3. 把 Nginx 现状（配置文件、server_name、读不了的文件）如实读给界面看。
//
// **本阶段一个字节都不写进 /etc/nginx。** 生成配置、原子写入、nginx -t、reload 与
// 失败恢复是 P10.2 的 proxy.apply 任务；因此在 P10.1 的响应里**没有** applied_* 字段，
// 也不能有任何“已生效”的说法：`enabled` 是意图，不是事实。这一层连不上代理时，
// 读取（列表、详情）照常工作——站点定义在面板库里，不依赖代理——但**任何写入都会被拒绝**，
// 因为判不了域名冲突就不该落一条可能覆盖别人站点的记录。
//
// 权限（CONTRACTS.md 第 9 节）：整组 admin。站点定义里有访问策略与上游目标，
// 合起来就是“这台机器的哪个入口通向哪里”，与容器清单同理属于侦察材料，viewer 得到 403。
// 这里没有再认证：登记定义不改变主机状态；真正把配置写进 Nginx 的 proxy.apply
// 在 P10.2 落地时按第 5 节的动作表要求再认证。

// NginxFactsReader 是 Nginx 现状的读取端（生产实现是 *proxies.AgentFacts）。
//
// 允许为 nil（演示模式不连代理）：那时 /proxies 仍返回站点定义，只是 nginx 那一块
// 明确写“读不到”，而不是给一份“没有配置文件”的空表——空表会被读成“这台机器上没有 Nginx”。
type NginxFactsReader interface {
	Snapshot(ctx context.Context) (nginx.Facts, error)
}

// proxyBasicAuthUserDTO 是一条 basic auth 用户。
//
// **返回时只有 name，没有 secret_ref**（CONTRACTS.md 第 9 节）。因此修改
// basic_auth_users 时必须重新提供 secret_ref——读回来的东西填不回请求里，
// 这是有意的：引用本身也不该靠“读出来再存回去”来维持。
type proxyBasicAuthUserDTO struct {
	Name      string `json:"name"`
	SecretRef string `json:"secret_ref,omitempty"`
}

// proxySiteDTO 是一个代理站点的公开形态（CONTRACTS.md 第 9 节 ProxyDefinition，平铺）。
//
// 没有 applied_config_hash、没有 config_path：P10.1 不生成配置，这两个字段要等 P10.2
// 真的写了文件才有意义。现在填一个空值或假值，界面就会显示一个它并不掌握的结论。
type proxySiteDTO struct {
	ID                 string                  `json:"id"`
	AppID              *string                 `json:"app_id"`
	Domain             string                  `json:"domain"`
	UpstreamTargetID   string                  `json:"upstream_target_id"`
	UpstreamScheme     string                  `json:"upstream_scheme"`
	UpstreamPort       int                     `json:"upstream_port"`
	AccessMode         string                  `json:"access_mode"`
	AllowedCIDRs       []string                `json:"allowed_cidrs"`
	BasicAuthUsers     []proxyBasicAuthUserDTO `json:"basic_auth_users"`
	WebSocketEnabled   bool                    `json:"websocket_enabled"`
	MaxBodyBytes       int64                   `json:"max_body_bytes"`
	ReadTimeoutSeconds int                     `json:"read_timeout_seconds"`
	Enabled            bool                    `json:"enabled"`
	Version            int                     `json:"version"`
	CreatedAt          string                  `json:"created_at"`
	UpdatedAt          string                  `json:"updated_at"`
}

// proxyNginxStateDTO 是主机 Nginx 现状摘要。
//
// ReadError 非空表示**面板读不到代理**：那时 available/config_missing 都不可信，
// 界面必须显示“不知道”，而不是显示“主机上没有 Nginx”。
type proxyNginxStateDTO struct {
	Available        bool     `json:"available"`
	ConfigMissing    bool     `json:"config_missing"`
	Reason           string   `json:"reason"`
	Version          string   `json:"version"`
	UnitState        string   `json:"unit_state"`
	ConfigRoot       string   `json:"config_root"`
	CollectedAt      *string  `json:"collected_at"`
	ManagedFileCount int      `json:"managed_file_count"`
	BrokenFiles      []string `json:"broken_files"`
	Warnings         []string `json:"warnings"`
	ReadError        string   `json:"read_error"`
}

// proxyListResponse 是 GET /proxies 的响应体。
type proxyListResponse struct {
	Items      []proxySiteDTO     `json:"items"`
	NextCursor *string            `json:"next_cursor"`
	Nginx      proxyNginxStateDTO `json:"nginx"`
}

// proxyServerDTO 是一个 server 块。
type proxyServerDTO struct {
	Line      int      `json:"line"`
	NamesLine int      `json:"names_line"`
	Names     []string `json:"names"`
	Listen    []string `json:"listen"`
}

// proxyConfigFileDTO 是一份读到的配置文件。
type proxyConfigFileDTO struct {
	Path      string           `json:"path"`
	Managed   bool             `json:"managed"`
	SizeBytes int64            `json:"size_bytes"`
	Error     string           `json:"error"`
	Servers   []proxyServerDTO `json:"servers"`
}

// proxyNginxFactsDTO 是 GET /proxies/nginx 的响应体：Nginx 现状的完整视图。
type proxyNginxFactsDTO struct {
	State       proxyNginxStateDTO   `json:"state"`
	Files       []proxyConfigFileDTO `json:"files"`
	ServerNames []string             `json:"server_names"`
	Includes    []string             `json:"includes"`
}

// proxyConflictHitDTO 是一条“哪个文件的第几行声明了这个域名”的证据。
type proxyConflictHitDTO struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	Name    string `json:"name"`
	Managed bool   `json:"managed"`
}

// proxyConflictDTO 是 GET /proxies/conflict 的响应体。
//
// State 是闭集 clear|taken|unknown。**unknown 不是 clear**：读不到 Nginx、有文件解析
// 失败、有认不出的 server_name 写法时，结论是“判不了”，写入会被拒绝。
type proxyConflictDTO struct {
	Domain         string                `json:"domain"`
	State          string                `json:"state"`
	Reason         string                `json:"reason"`
	Matches        []proxyConflictHitDTO `json:"matches"`
	Undecidable    []proxyConflictHitDTO `json:"undecidable"`
	BrokenFiles    []string              `json:"broken_files"`
	ExistingSiteID *string               `json:"existing_site_id"`
	Nginx          proxyNginxStateDTO    `json:"nginx"`
}

// createProxyRequest 是 POST /proxies 的请求体。
//
// app_id 与 enabled 用指针：前者可空（不关联应用），后者省略时默认启用——
// “省略”和“显式 false”是两件事，把省略读成 false 会让人新建出来的站点一上来就是停用的。
type createProxyRequest struct {
	AppID              *string                 `json:"app_id"`
	Domain             string                  `json:"domain"`
	UpstreamTargetID   string                  `json:"upstream_target_id"`
	UpstreamScheme     string                  `json:"upstream_scheme"`
	UpstreamPort       int                     `json:"upstream_port"`
	AccessMode         string                  `json:"access_mode"`
	AllowedCIDRs       []string                `json:"allowed_cidrs"`
	BasicAuthUsers     []proxyBasicAuthUserDTO `json:"basic_auth_users"`
	WebSocketEnabled   bool                    `json:"websocket_enabled"`
	MaxBodyBytes       int64                   `json:"max_body_bytes"`
	ReadTimeoutSeconds int                     `json:"read_timeout_seconds"`
	Enabled            *bool                   `json:"enabled"`
}

// updateProxyRequest 是 PATCH /proxies/:id 的请求体：nil 表示“不改这一项”。
//
// allowed_cidrs 与 basic_auth_users 用指向切片的指针：`[]` 表示“清空”这个列表，
// 而字段缺席表示“不动它”。两者在 JSON 里长得不一样，在语义上也不能混。
type updateProxyRequest struct {
	Version            int                      `json:"version"`
	AppID              *string                  `json:"app_id"`
	Domain             *string                  `json:"domain"`
	UpstreamTargetID   *string                  `json:"upstream_target_id"`
	UpstreamScheme     *string                  `json:"upstream_scheme"`
	UpstreamPort       *int                     `json:"upstream_port"`
	AccessMode         *string                  `json:"access_mode"`
	AllowedCIDRs       *[]string                `json:"allowed_cidrs"`
	BasicAuthUsers     *[]proxyBasicAuthUserDTO `json:"basic_auth_users"`
	WebSocketEnabled   *bool                    `json:"websocket_enabled"`
	MaxBodyBytes       *int64                   `json:"max_body_bytes"`
	ReadTimeoutSeconds *int                     `json:"read_timeout_seconds"`
	Enabled            *bool                    `json:"enabled"`
}

// handleListProxies 处理 GET /api/v1/proxies（admin）。
func (s *Server) handleListProxies(c *gin.Context) {
	limit, ok := listLimitMax(c, proxies.MaxListLimit)
	if !ok {
		return
	}
	opts := proxies.ListOptions{Limit: limit, Cursor: c.Query("cursor"), AppID: c.Query("app_id")}
	// 域名过滤按归一化后的值匹配：库里存的是归一化域名，而用户可能输入大写或末尾带点。
	// 归一化失败是 400，不是“没找到”——静默返回空列表会让人以为这个站点不存在。
	if raw := strings.TrimSpace(c.Query("domain")); raw != "" {
		domain, err := proxies.NormalizeDomain(raw)
		if err != nil {
			writeError(c, http.StatusBadRequest, httpx.CodeValidationError, err.Error())
			return
		}
		opts.Domain = domain
	}

	items, next, err := s.opts.Proxies.List(c.Request.Context(), opts)
	if err != nil {
		s.writeProxyError(c, "读取代理站点列表失败", err)
		return
	}
	resp := proxyListResponse{Items: make([]proxySiteDTO, 0, len(items))}
	for _, item := range items {
		resp.Items = append(resp.Items, proxySiteDTOOf(item))
	}
	if next != "" {
		resp.NextCursor = &next
	}
	resp.Nginx = s.proxyNginxState(c.Request.Context())
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, resp)
}

// handleGetProxySite 处理 GET /api/v1/proxies/:id（admin）。
func (s *Server) handleGetProxySite(c *gin.Context) {
	site, err := s.opts.Proxies.Get(c.Request.Context(), c.Param("id"))
	if err != nil {
		s.writeProxyError(c, "读取代理站点失败", err)
		return
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, proxySiteDTOOf(site))
}

// handleProxyNginxFacts 处理 GET /api/v1/proxies/nginx（admin）：Nginx 现状的完整视图。
//
// 这是一条独立的读接口，而不是塞进 GET /proxies：配置文件的清单、每份文件里的 server 块、
// 读不了的文件，都比站点列表更细也更容易变。它自己的失败就是 503——用户点进来就是为了
// 看这一步读到了什么。
func (s *Server) handleProxyNginxFacts(c *gin.Context) {
	if s.opts.NginxFacts == nil {
		httpx.WriteError(c.Writer, c.Request, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"读取 Nginx 现状需要连接代理：当前部署没有可用的代理连接")
		return
	}
	facts, err := s.opts.NginxFacts.Snapshot(c.Request.Context())
	if err != nil {
		if errors.Is(err, proxies.ErrFactsUnavailable) {
			httpx.WriteError(c.Writer, c.Request, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
				"读取 Nginx 现状失败：代理不可用")
			return
		}
		s.internalError(c, "读取 Nginx 现状失败", err)
		return
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, proxyNginxFactsDTOOf(facts, ""))
}

// handleProxyDomainCheck 处理 GET /api/v1/proxies/conflict?domain=&exclude_id=（admin）。
//
// 它回答“这个域名现在能不能用”，并且把依据一起返回。exclude_id 是修改场景：
// 把自己那条记录排除掉，否则改一个站点的其它字段时会被自己的域名挡下来。
func (s *Server) handleProxyDomainCheck(c *gin.Context) {
	domain := strings.TrimSpace(c.Query("domain"))
	if domain == "" {
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError, "缺少 domain 参数")
		return
	}
	report, err := s.opts.Proxies.CheckDomain(c.Request.Context(), domain, strings.TrimSpace(c.Query("exclude_id")))
	if err != nil {
		s.writeProxyError(c, "判断域名可用性失败", err)
		return
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, proxyConflictDTOOf(report))
}

// handleCreateProxySite 处理 POST /api/v1/proxies（admin + CSRF）。
func (s *Server) handleCreateProxySite(c *gin.Context) {
	sess := sessionFrom(c)
	const action = audit.ActionProxyCreate

	var req createProxyRequest
	if !decodeJSONBody(c, &req) {
		s.recordDenied(c, &sess, action, "proxy", "validation", httpx.CodeValidationError)
		return
	}
	def := proxies.Definition{
		Domain:             req.Domain,
		UpstreamTargetID:   req.UpstreamTargetID,
		UpstreamScheme:     req.UpstreamScheme,
		UpstreamPort:       req.UpstreamPort,
		AccessMode:         proxies.AccessMode(req.AccessMode),
		AllowedCIDRs:       req.AllowedCIDRs,
		BasicAuthUsers:     basicAuthUsersFromDTO(req.BasicAuthUsers),
		WebSocketEnabled:   req.WebSocketEnabled,
		MaxBodyBytes:       req.MaxBodyBytes,
		ReadTimeoutSeconds: req.ReadTimeoutSeconds,
		Enabled:            req.Enabled == nil || *req.Enabled,
	}
	if req.AppID != nil {
		def.AppID = *req.AppID
	}

	var created proxies.Site
	err := s.opts.Proxies.InTransaction(c.Request.Context(), func(ctx context.Context) error {
		site, err := s.opts.Proxies.Create(ctx, def)
		if err != nil {
			return &errDenied{err}
		}
		if err := s.recordAuditInTx(ctx, c, sess, action, proxyTarget(site.ID), map[string]any{
			"domain":                site.Domain,
			"app_id":                site.AppID,
			"upstream_target_id":    site.UpstreamTargetID,
			"upstream_scheme":       site.UpstreamScheme,
			"upstream_port":         site.UpstreamPort,
			"access_mode":           string(site.AccessMode),
			"allowed_cidrs":         site.AllowedCIDRs,
			"basic_auth_user_count": len(site.BasicAuthUsers),
			"websocket_enabled":     site.WebSocketEnabled,
			"max_body_bytes":        site.MaxBodyBytes,
			"read_timeout_seconds":  site.ReadTimeoutSeconds,
			"enabled":               site.Enabled,
			"version":               site.Version,
			// 审计里写明这一版只登记定义：将来有人查“这条记录写入时 Nginx 改了吗”，
			// 答案是没改，P10.1 没有写配置的能力。
			"applied": false,
		}); err != nil {
			return err
		}
		created = site
		return nil
	})
	if err != nil {
		s.failProxyAction(c, &sess, action, "proxy", err)
		return
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusCreated, proxySiteDTOOf(created))
}

// handleUpdateProxySite 处理 PATCH /api/v1/proxies/:id（admin + CSRF，version 必填）。
func (s *Server) handleUpdateProxySite(c *gin.Context) {
	sess := sessionFrom(c)
	const action = audit.ActionProxyUpdate

	id := c.Param("id")
	target := proxyTarget(id)

	var req updateProxyRequest
	if !decodeJSONBody(c, &req) {
		s.recordDenied(c, &sess, action, target, "validation", httpx.CodeValidationError)
		return
	}
	if req.Version <= 0 {
		s.recordDenied(c, &sess, action, target, "validation", httpx.CodeValidationError)
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError, "version 必须是正整数")
		return
	}
	patch := proxies.Patch{
		AppID:              req.AppID,
		Domain:             req.Domain,
		UpstreamTargetID:   req.UpstreamTargetID,
		UpstreamScheme:     req.UpstreamScheme,
		UpstreamPort:       req.UpstreamPort,
		AllowedCIDRs:       req.AllowedCIDRs,
		WebSocketEnabled:   req.WebSocketEnabled,
		MaxBodyBytes:       req.MaxBodyBytes,
		ReadTimeoutSeconds: req.ReadTimeoutSeconds,
		Enabled:            req.Enabled,
	}
	if req.AccessMode != nil {
		mode := proxies.AccessMode(*req.AccessMode)
		patch.AccessMode = &mode
	}
	if req.BasicAuthUsers != nil {
		users := basicAuthUsersFromDTO(*req.BasicAuthUsers)
		patch.BasicAuthUsers = &users
	}
	if patch.IsEmpty() {
		s.recordDenied(c, &sess, action, target, "validation", httpx.CodeValidationError)
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError, "至少需要提供一个要修改的字段")
		return
	}

	changed := map[string]any{"version": req.Version, "applied": false}
	if patch.Domain != nil {
		changed["domain"] = *patch.Domain
	}
	if patch.AppID != nil {
		changed["app_id"] = *patch.AppID
	}
	if patch.AccessMode != nil {
		changed["access_mode"] = string(*patch.AccessMode)
	}
	if patch.AllowedCIDRs != nil {
		changed["allowed_cidrs"] = *patch.AllowedCIDRs
	}
	if patch.BasicAuthUsers != nil {
		changed["basic_auth_user_count"] = len(*patch.BasicAuthUsers)
	}
	if patch.Enabled != nil {
		changed["enabled"] = *patch.Enabled
	}
	if patch.WebSocketEnabled != nil {
		changed["websocket_enabled"] = *patch.WebSocketEnabled
	}
	if patch.UpstreamPort != nil {
		changed["upstream_port"] = *patch.UpstreamPort
	}

	var updated proxies.Site
	err := s.opts.Proxies.InTransaction(c.Request.Context(), func(ctx context.Context) error {
		site, err := s.opts.Proxies.Update(ctx, proxies.UpdateInput{ID: id, Version: req.Version, Patch: patch})
		if err != nil {
			return &errDenied{err}
		}
		changed["version"] = site.Version
		if err := s.recordAuditInTx(ctx, c, sess, action, target, changed); err != nil {
			return err
		}
		updated = site
		return nil
	})
	if err != nil {
		s.failProxyAction(c, &sess, action, target, err)
		return
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, proxySiteDTOOf(updated))
}

// handleDeleteProxySite 处理 DELETE /api/v1/proxies/:id（admin + CSRF，version 必填）。
//
// 删除的只是面板里这条记录：不碰应用、不碰容器、不删 /etc/nginx 里的任何文件。
// P10.2 之后，删除一条已经生效的站点会变成 proxy.delete 任务（带 nginx+domain 锁），
// 那时这个接口的行为会跟着变——现在它只作用在自己库里。
func (s *Server) handleDeleteProxySite(c *gin.Context) {
	sess := sessionFrom(c)
	const action = audit.ActionProxyDelete

	id := c.Param("id")
	target := proxyTarget(id)
	version, ok := deleteVersion(c)
	if !ok {
		s.recordDenied(c, &sess, action, target, "validation", httpx.CodeValidationError)
		return
	}

	err := s.opts.Proxies.InTransaction(c.Request.Context(), func(ctx context.Context) error {
		if err := s.opts.Proxies.Delete(ctx, id, version); err != nil {
			return &errDenied{err}
		}
		return s.recordAuditInTx(ctx, c, sess, action, target, map[string]any{
			"version": version,
			"applied": false,
		})
	})
	if err != nil {
		s.failProxyAction(c, &sess, action, target, err)
		return
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, struct{}{})
}

// proxySiteDTOOf 把业务对象转成公开 DTO。
func proxySiteDTOOf(site proxies.Site) proxySiteDTO {
	dto := proxySiteDTO{
		ID:                 site.ID,
		Domain:             site.Domain,
		UpstreamTargetID:   site.UpstreamTargetID,
		UpstreamScheme:     site.UpstreamScheme,
		UpstreamPort:       site.UpstreamPort,
		AccessMode:         string(site.AccessMode),
		AllowedCIDRs:       site.AllowedCIDRs,
		BasicAuthUsers:     basicAuthUserDTOs(site.BasicAuthUsers),
		WebSocketEnabled:   site.WebSocketEnabled,
		MaxBodyBytes:       site.MaxBodyBytes,
		ReadTimeoutSeconds: site.ReadTimeoutSeconds,
		Enabled:            site.Enabled,
		Version:            site.Version,
		CreatedAt:          site.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:          site.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if site.AppID != "" {
		appID := site.AppID
		dto.AppID = &appID
	}
	if dto.AllowedCIDRs == nil {
		// 空列表必须是 []，不能是 null：null 在 JSON 里读作“不知道”，
		// 而“没有限制网段”是一个确定的事实。
		dto.AllowedCIDRs = []string{}
	}
	if dto.BasicAuthUsers == nil {
		dto.BasicAuthUsers = []proxyBasicAuthUserDTO{}
	}
	return dto
}

// basicAuthUserDTOs 把用户列表转成 DTO。**secret_ref 不出现在响应里**。
func basicAuthUserDTOs(users []proxies.BasicAuthUser) []proxyBasicAuthUserDTO {
	out := make([]proxyBasicAuthUserDTO, 0, len(users))
	for _, u := range users {
		out = append(out, proxyBasicAuthUserDTO{Name: u.Name})
	}
	return out
}

// basicAuthUsersFromDTO 把请求里的用户列表转成业务对象（含 secret_ref）。
func basicAuthUsersFromDTO(users []proxyBasicAuthUserDTO) []proxies.BasicAuthUser {
	if users == nil {
		return nil
	}
	out := make([]proxies.BasicAuthUser, 0, len(users))
	for _, u := range users {
		out = append(out, proxies.BasicAuthUser{Name: u.Name, SecretRef: u.SecretRef})
	}
	return out
}

// proxyNginxState 读取 Nginx 现状摘要（展示用，走代理侧缓存）。
//
// 读不到时返回的是一份“不知道”：ReadError 写明原因，Available 保持 false，
// 绝不用“没有配置文件”来表达“读不到配置”。这两句话的方向相反——
// 一句让人去别处找冲突，一句让人去查代理。
func (s *Server) proxyNginxState(ctx context.Context) proxyNginxStateDTO {
	if s.opts.NginxFacts == nil {
		return proxyNginxStateDTO{
			Reason:    "未配置代理连接：面板读不到这台机器上的 Nginx 现状",
			ReadError: "no_agent_connection",
		}
	}
	facts, err := s.opts.NginxFacts.Snapshot(ctx)
	if err != nil {
		return proxyNginxStateDTO{
			Reason:    "读取 Nginx 现状失败：" + err.Error(),
			ReadError: "agent_unavailable",
		}
	}
	return proxyNginxStateDTOOf(facts)
}

// proxyNginxStateDTOOf 把事实摘要转成 DTO。
func proxyNginxStateDTOOf(facts nginx.Facts) proxyNginxStateDTO {
	dto := proxyNginxStateDTO{
		Available:     facts.Available,
		ConfigMissing: facts.ConfigMissing,
		Reason:        facts.Reason,
		Version:       facts.Version,
		UnitState:     facts.UnitState,
		ConfigRoot:    facts.ConfigRoot,
		BrokenFiles:   facts.BrokenFiles(),
		Warnings:      facts.Warnings,
	}
	for _, f := range facts.Files {
		if f.Managed {
			dto.ManagedFileCount++
		}
	}
	if !facts.CollectedAt.IsZero() {
		ts := facts.CollectedAt.UTC().Format(time.RFC3339)
		dto.CollectedAt = &ts
	}
	if dto.BrokenFiles == nil {
		dto.BrokenFiles = []string{}
	}
	if dto.Warnings == nil {
		dto.Warnings = []string{}
	}
	return dto
}

// proxyNginxFactsDTOOf 把完整事实转成 DTO。readError 由调用方决定（查询失败时非空）。
func proxyNginxFactsDTOOf(facts nginx.Facts, readError string) proxyNginxFactsDTO {
	state := proxyNginxStateDTOOf(facts)
	state.ReadError = readError
	dto := proxyNginxFactsDTO{
		State:       state,
		Files:       make([]proxyConfigFileDTO, 0, len(facts.Files)),
		ServerNames: facts.ServerNames(),
		Includes:    make([]string, 0, len(facts.Includes)),
	}
	for _, file := range facts.Files {
		item := proxyConfigFileDTO{
			Path:      file.Path,
			Managed:   file.Managed,
			SizeBytes: file.SizeBytes,
			Error:     file.Error,
			Servers:   make([]proxyServerDTO, 0, len(file.Servers)),
		}
		for _, srv := range file.Servers {
			server := proxyServerDTO{Line: srv.Line, NamesLine: srv.NamesLine,
				Names: make([]string, 0, len(srv.Names)), Listen: make([]string, 0, len(srv.Listen))}
			for _, name := range srv.Names {
				server.Names = append(server.Names, name.Raw)
			}
			for _, listen := range srv.Listen {
				server.Listen = append(server.Listen, listen.Raw)
			}
			item.Servers = append(item.Servers, server)
		}
		dto.Files = append(dto.Files, item)
	}
	for _, inc := range facts.Includes {
		dto.Includes = append(dto.Includes, inc.Target)
	}
	if dto.ServerNames == nil {
		dto.ServerNames = []string{}
	}
	return dto
}

// proxyConflictDTOOf 把冲突报告转成 DTO。
func proxyConflictDTOOf(report proxies.ConflictReport) proxyConflictDTO {
	dto := proxyConflictDTO{
		Domain:         report.Domain,
		State:          string(report.State),
		Reason:         report.Reason,
		Matches:        conflictHitsDTOOf(report.Matches),
		Undecidable:    conflictHitsDTOOf(report.Undecidable),
		BrokenFiles:    report.BrokenFiles,
		ExistingSiteID: nil,
		Nginx: proxyNginxStateDTO{
			Available:        report.NginxAvailable,
			ConfigMissing:    report.NginxConfigMissing,
			Reason:           report.NginxReason,
			Version:          report.NginxVersion,
			UnitState:        report.NginxUnitState,
			ManagedFileCount: report.ManagedFileCount,
			BrokenFiles:      report.BrokenFiles,
			Warnings:         []string{},
		},
	}
	if report.ExistingSiteID != "" {
		id := report.ExistingSiteID
		dto.ExistingSiteID = &id
	}
	if dto.BrokenFiles == nil {
		dto.BrokenFiles = []string{}
	}
	return dto
}

func conflictHitsDTOOf(hits []proxies.ConflictHit) []proxyConflictHitDTO {
	out := make([]proxyConflictHitDTO, 0, len(hits))
	for _, h := range hits {
		out = append(out, proxyConflictHitDTO{File: h.File, Line: h.Line, Name: h.Name, Managed: h.Managed})
	}
	return out
}

// proxyTarget 是审计里站点对象的写法。
func proxyTarget(id string) string { return "proxy:" + id }

// writeProxyError 把业务错误写成响应，内部错误只记日志。
func (s *Server) writeProxyError(c *gin.Context, what string, err error) {
	status, code, msg, details, internal := mapProxyError(err)
	if internal {
		s.internalError(c, what, err)
		return
	}
	if details != nil {
		httpx.WriteErrorDetails(c.Writer, c.Request, status, code, msg, details)
		return
	}
	writeError(c, status, code, msg)
}

// failProxyAction 处理“事务内失败”的响应与审计。
func (s *Server) failProxyAction(c *gin.Context, sess *auth.Session, action, target string, err error) {
	if errors.Is(err, errAuditWrite) {
		s.opts.Logger.Error("审计写入失败，业务变更已回滚", "error", err, "action", action,
			"request_id", c.GetString(ctxKeyRequestID))
		writeError(c, http.StatusInternalServerError, httpx.CodeInternalError, "服务器内部错误")
		return
	}
	businessErr := err
	var denied *errDenied
	if errors.As(err, &denied) {
		businessErr = denied.err
	}
	status, code, msg, details, internal := mapProxyError(businessErr)
	if internal {
		s.recordDenied(c, sess, action, target, "internal", code)
		s.internalError(c, "代理站点操作失败", businessErr)
		return
	}
	s.recordDenied(c, sess, action, target, "business", code)
	if details != nil {
		httpx.WriteErrorDetails(c.Writer, c.Request, status, code, msg, details)
		return
	}
	writeError(c, status, code, msg)
}

// mapProxyError 是站点错误的唯一映射处。
//
// 冲突（域名被别人占了）是 409：它不是请求写错了，而是当前状态不允许。
// 判不了（读不到 Nginx）是 503：等代理回来就能继续，不是用户该改的东西。
func mapProxyError(err error) (status int, code httpx.Code, message string, details gin.H, internal bool) {
	var (
		validation *proxies.ValidationError
		conflict   *proxies.ConflictError
	)
	switch {
	case errors.As(err, &validation):
		return http.StatusBadRequest, httpx.CodeValidationError, validation.Message, nil, false
	case errors.As(err, &conflict):
		return http.StatusConflict, httpx.CodeValidationError, conflict.Error(), gin.H{
			"domain":           conflict.Report.Domain,
			"state":            string(conflict.Report.State),
			"matches":          conflictHitsDTOOf(conflict.Report.Matches),
			"undecidable":      conflictHitsDTOOf(conflict.Report.Undecidable),
			"broken_files":     conflict.Report.BrokenFiles,
			"existing_site_id": nullableStringPtr(conflict.Report.ExistingSiteID),
		}, false
	case errors.Is(err, proxies.ErrDomainTaken):
		return http.StatusConflict, httpx.CodeValidationError,
			"这个域名已经被面板里另一条站点记录占用", nil, false
	case errors.Is(err, proxies.ErrFactsUnavailable):
		return http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"无法判断域名是否已被占用：代理不可用，本次没有保存", nil, false
	case errors.Is(err, proxies.ErrInvalidLimit), errors.Is(err, proxies.ErrInvalidCursor):
		return http.StatusBadRequest, httpx.CodeValidationError, err.Error(), nil, false
	case errors.Is(err, proxies.ErrNotFound):
		return http.StatusNotFound, httpx.CodeNotFound, "代理站点不存在", nil, false
	case errors.Is(err, proxies.ErrVersionConflict):
		return http.StatusConflict, httpx.CodeVersionConflict, "已被其他人修改，请刷新后重试", nil, false
	default:
		return http.StatusInternalServerError, httpx.CodeInternalError, "服务器内部错误", nil, true
	}
}

// nullableStringPtr 把空串转成 nil，供 gin.H 里的可空字段使用。
func nullableStringPtr(v string) *string {
	if v == "" {
		return nil
	}
	out := v
	return &out
}
