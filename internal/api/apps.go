package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"upanel/internal/appruntime"
	"upanel/internal/apps"
	"upanel/internal/audit"
	"upanel/internal/auth"
	"upanel/internal/httpx"
)

// 应用与分组元数据（P09.1）。
//
// 这一层的职责只有三件事：
//  1. 把请求体严格解成 apps.Definition / apps.Patch（未知字段直接 400，不静默忽略）；
//  2. 把业务错误映射成契约错误码；
//  3. 把“元数据变更 + 审计”合成一笔事务。
//
// 元数据校验、判别模型与依赖卷确认全在 internal/apps 里，这里不复制一份——两处校验
// 迟早会分叉，分叉之后“接口允许、库里不允许”的记录就是这么来的。
//
// 权限：读取 V（脱敏 DTO 里没有任何秘密），写入 admin + CSRF（CONTRACTS.md 第 8 节）。
// 这里没有再认证：应用元数据不改变主机状态，也不授予任何主机权限。

// appBindingDTO 是应用的运行绑定。
//
// 三个字段最多只有一个出现，由 type 决定是哪一个——序列化时空值被省略，因此
// link 序列化成 {}，systemd 序列化成 {"unit":"…"}，依次类推。
type appBindingDTO struct {
	Unit         string `json:"unit,omitempty"`
	ContainerID  string `json:"container_id,omitempty"`
	DeploymentID string `json:"deployment_id,omitempty"`
}

// appHealthDTO 是健康检查定义（CONTRACTS.md 第 8 节）。
type appHealthDTO struct {
	Enabled             bool   `json:"enabled"`
	TargetID            string `json:"target_id"`
	Scheme              string `json:"scheme"`
	Port                int    `json:"port"`
	Path                string `json:"path"`
	ExpectedStatusCodes []int  `json:"expected_status_codes"`
	IntervalSeconds     int    `json:"interval_seconds"`
	TimeoutSeconds      int    `json:"timeout_seconds"`
}

// appCapabilitiesDTO 是“此刻真的能做到的事”，见 apps.Capabilities 的说明。
type appCapabilitiesDTO struct {
	Open    bool `json:"open"`
	Start   bool `json:"start"`
	Stop    bool `json:"stop"`
	Restart bool `json:"restart"`
	Logs    bool `json:"logs"`
}

// appDTO 是应用的公开形态（CONTRACTS.md 第 8 节 AppDefinition + AppSummary）。
//
// runtime_state / runtime_note / http_state / last_checked_at / dependency_protection
// 五个摘要字段都来自观测，没有一个由类型推出来：
//   - runtime_* 与 capabilities 来自 systemd 的单元状态或 docker 的容器状态（P09.4）；
//   - http_state 与 last_checked_at 来自健康探测结论（P09.2）；
//   - dependency_protection 说明依赖卷的启动顺序有没有被面板接管。
//
// 观测不到时给 unknown 并在 note 里写清原因，不猜一个 running 让首页好看。
type appDTO struct {
	ID          string        `json:"id"`
	Name        string        `json:"name"`
	Description string        `json:"description"`
	IconKey     string        `json:"icon_key"`
	GroupID     *string       `json:"group_id"`
	Type        string        `json:"type"`
	Binding     appBindingDTO `json:"binding"`
	PublicURL   *string       `json:"public_url"`
	HealthCheck appHealthDTO  `json:"health_check"`
	VolumeDeps  []string      `json:"volume_dependencies"`
	Favorite    bool          `json:"favorite"`
	Version     int           `json:"version"`
	CreatedAt   string        `json:"created_at"`
	UpdatedAt   string        `json:"updated_at"`

	RuntimeState             string             `json:"runtime_state"`
	RuntimeNote              *string            `json:"runtime_note"`
	HTTPState                string             `json:"http_state"`
	LastCheckedAt            *string            `json:"last_checked_at"`
	DependencyProtection     string             `json:"dependency_protection"`
	DependencyProtectionNote *string            `json:"dependency_protection_note"`
	Capabilities             appCapabilitiesDTO `json:"capabilities"`
}

// appListResponse 是 GET /apps 的响应体。
type appListResponse struct {
	Items      []appDTO `json:"items"`
	NextCursor *string  `json:"next_cursor"`
}

// createAppRequest 是 POST /apps 的请求体。
type createAppRequest struct {
	Name               string        `json:"name"`
	Description        string        `json:"description"`
	IconKey            string        `json:"icon_key"`
	GroupID            *string       `json:"group_id"`
	Type               string        `json:"type"`
	Binding            appBindingDTO `json:"binding"`
	PublicURL          string        `json:"public_url"`
	HealthCheck        *appHealthDTO `json:"health_check"`
	VolumeDependencies []string      `json:"volume_dependencies"`
}

// updateAppRequest 是 PATCH /apps/:id 的请求体：nil 表示“不改这一项”。
//
// group_id 用 apps.Optional：显式 null 表示移出分组，与“没提供这个字段”是两件事。
type updateAppRequest struct {
	Version            int                   `json:"version"`
	Name               *string               `json:"name"`
	Description        *string               `json:"description"`
	IconKey            *string               `json:"icon_key"`
	GroupID            apps.Optional[string] `json:"group_id"`
	Type               *string               `json:"type"`
	Binding            *appBindingDTO        `json:"binding"`
	PublicURL          *string               `json:"public_url"`
	HealthCheck        *appHealthDTO         `json:"health_check"`
	VolumeDependencies *[]string             `json:"volume_dependencies"`
}

// favoriteRequest 是 PUT /apps/:id/favorite 的请求体。
type favoriteRequest struct {
	Favorite bool `json:"favorite"`
}

// groupDTO 是分组的公开形态。
type groupDTO struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	SortOrder int    `json:"sort_order"`
	Version   int    `json:"version"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// groupListResponse 是 GET /app-groups 的响应体。
type groupListResponse struct {
	Items      []groupDTO `json:"items"`
	NextCursor *string    `json:"next_cursor"`
}

// createGroupRequest 是 POST /app-groups 的请求体。
type createGroupRequest struct {
	Name      string `json:"name"`
	SortOrder *int   `json:"sort_order"`
}

// updateGroupRequest 是 PATCH /app-groups/:id 的请求体。
type updateGroupRequest struct {
	Version   int     `json:"version"`
	Name      *string `json:"name"`
	SortOrder *int    `json:"sort_order"`
}

// handleListApps 处理 GET /api/v1/apps（权限 V）。
func (s *Server) handleListApps(c *gin.Context) {
	limit, ok := listLimitMax(c, apps.MaxListLimit)
	if !ok {
		return
	}
	items, next, err := s.opts.Apps.List(c.Request.Context(), apps.ListOptions{
		UserID:  sessionFrom(c).User.ID,
		Limit:   limit,
		Cursor:  c.Query("cursor"),
		GroupID: c.Query("group_id"),
	})
	if err != nil {
		s.writeAppError(c, "读取应用列表失败", err)
		return
	}
	resp := appListResponse{Items: make([]appDTO, 0, len(items))}
	// 一轮运行事实给整页应用共用：逐个去问 systemctl / docker 既慢，
	// 又会让同一页里两条应用因为读取时刻不同而互相矛盾。
	facts := s.appFacts(c.Request.Context())
	for _, a := range items {
		resp.Items = append(resp.Items, appDTOOf(a, facts.App(a)))
	}
	if next != "" {
		resp.NextCursor = &next
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, resp)
}

// handleGetApp 处理 GET /api/v1/apps/:id（权限 V）。
func (s *Server) handleGetApp(c *gin.Context) {
	app, err := s.opts.Apps.Get(c.Request.Context(), sessionFrom(c).User.ID, c.Param("id"))
	if err != nil {
		s.writeAppError(c, "读取应用失败", err)
		return
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, s.appDTOFor(c.Request.Context(), app))
}

// handleCreateApp 处理 POST /api/v1/apps（admin + CSRF）。
func (s *Server) handleCreateApp(c *gin.Context) {
	sess := sessionFrom(c)
	const action = audit.ActionAppCreate

	var req createAppRequest
	if !decodeJSONBody(c, &req) {
		s.recordDenied(c, &sess, action, "app", "validation", httpx.CodeValidationError)
		return
	}
	def := apps.Definition{
		Name:               req.Name,
		Description:        req.Description,
		IconKey:            req.IconKey,
		Type:               apps.Type(req.Type),
		Binding:            bindingFromDTO(req.Binding),
		PublicURL:          req.PublicURL,
		VolumeDependencies: req.VolumeDependencies,
	}
	if req.GroupID != nil {
		def.GroupID = *req.GroupID
	}
	if req.HealthCheck != nil {
		def.Health = healthFromDTO(*req.HealthCheck)
	}

	var created apps.App
	err := s.opts.Apps.InTransaction(c.Request.Context(), func(ctx context.Context) error {
		app, err := s.opts.Apps.Create(ctx, def)
		if err != nil {
			return &errDenied{err}
		}
		if err := s.recordAuditInTx(ctx, c, sess, action, appTarget(app.ID), map[string]any{
			"name":                app.Name,
			"type":                string(app.Type),
			"binding":             bindingSummary(app.Definition),
			"group_id":            app.GroupID,
			"public_url":          app.PublicURL,
			"volume_dependencies": app.VolumeDependencies,
			"version":             app.Version,
		}); err != nil {
			return err
		}
		created = app
		return nil
	})
	if err != nil {
		s.failAppAction(c, &sess, action, "app", err)
		return
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusCreated, s.appDTOFor(c.Request.Context(), created))
}

// handleUpdateApp 处理 PATCH /api/v1/apps/:id（admin + CSRF，version 必填）。
func (s *Server) handleUpdateApp(c *gin.Context) {
	sess := sessionFrom(c)
	const action = audit.ActionAppUpdate

	id := c.Param("id")
	target := appTarget(id)

	var req updateAppRequest
	if !decodeJSONBody(c, &req) {
		s.recordDenied(c, &sess, action, target, "validation", httpx.CodeValidationError)
		return
	}
	if req.Version <= 0 {
		s.recordDenied(c, &sess, action, target, "validation", httpx.CodeValidationError)
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError, "version 必须是正整数")
		return
	}
	patch := apps.Patch{
		Name:               req.Name,
		Description:        req.Description,
		IconKey:            req.IconKey,
		GroupID:            req.GroupID,
		PublicURL:          req.PublicURL,
		VolumeDependencies: req.VolumeDependencies,
	}
	if req.Type != nil {
		t := apps.Type(*req.Type)
		patch.Type = &t
	}
	if req.Binding != nil {
		b := bindingFromDTO(*req.Binding)
		patch.Binding = &b
	}
	if req.HealthCheck != nil {
		h := healthFromDTO(*req.HealthCheck)
		patch.Health = &h
	}
	if patch.Name == nil && patch.Description == nil && patch.IconKey == nil &&
		!patch.GroupID.Set && patch.Type == nil && patch.Binding == nil &&
		patch.PublicURL == nil && patch.Health == nil && patch.VolumeDependencies == nil {
		s.recordDenied(c, &sess, action, target, "validation", httpx.CodeValidationError)
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError, "至少需要提供一个要修改的字段")
		return
	}

	changed := map[string]any{"version": req.Version}
	if patch.Name != nil {
		changed["name"] = *patch.Name
	}
	if patch.Description != nil {
		changed["description_set"] = true
	}
	if patch.IconKey != nil {
		changed["icon_key"] = *patch.IconKey
	}
	if patch.GroupID.Set {
		changed["group_id"] = patch.GroupID.Value
	}
	if patch.Type != nil {
		changed["type"] = string(*patch.Type)
	}
	if patch.Binding != nil {
		changed["binding_set"] = true
	}
	if patch.PublicURL != nil {
		changed["public_url"] = *patch.PublicURL
	}
	if patch.Health != nil {
		changed["health_enabled"] = patch.Health.Enabled
	}
	if patch.VolumeDependencies != nil {
		changed["volume_dependencies"] = *patch.VolumeDependencies
	}

	var updated apps.App
	err := s.opts.Apps.InTransaction(c.Request.Context(), func(ctx context.Context) error {
		app, err := s.opts.Apps.Update(ctx, apps.UpdateInput{ID: id, Version: req.Version, Patch: patch})
		if err != nil {
			return &errDenied{err}
		}
		changed["version"] = app.Version
		if patch.Binding != nil {
			changed["binding"] = bindingSummary(app.Definition)
		}
		if err := s.recordAuditInTx(ctx, c, sess, action, target, changed); err != nil {
			return err
		}
		updated = app
		return nil
	})
	if err != nil {
		s.failAppAction(c, &sess, action, target, err)
		return
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, s.appDTOFor(c.Request.Context(), updated))
}

// handleDeleteApp 处理 DELETE /api/v1/apps/:id（admin + CSRF，version 必填）。
//
// 删除的只是入口与绑定记录：不卸载项目、不停止服务、不删容器、不碰依赖卷里的数据。
func (s *Server) handleDeleteApp(c *gin.Context) {
	sess := sessionFrom(c)
	const action = audit.ActionAppDelete

	id := c.Param("id")
	target := appTarget(id)
	version, ok := deleteVersion(c)
	if !ok {
		s.recordDenied(c, &sess, action, target, "validation", httpx.CodeValidationError)
		return
	}

	err := s.opts.Apps.InTransaction(c.Request.Context(), func(ctx context.Context) error {
		if err := s.opts.Apps.Delete(ctx, id, version); err != nil {
			return &errDenied{err}
		}
		return s.recordAuditInTx(ctx, c, sess, action, target, map[string]any{"version": version})
	})
	if err != nil {
		s.failAppAction(c, &sess, action, target, err)
		return
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, struct{}{})
}

// handleSetAppFavorite 处理 PUT /api/v1/apps/:id/favorite（V + CSRF）。
//
// 收藏是当前用户的偏好，不是管理动作：幂等、不需要 version、不记审计
// （否则每点一次星标就产生一条审计，真正的管理事件会被淹掉）。
func (s *Server) handleSetAppFavorite(c *gin.Context) {
	var req favoriteRequest
	if !decodeJSONBody(c, &req) {
		return
	}
	if err := s.opts.Apps.SetFavorite(c.Request.Context(), sessionFrom(c).User.ID,
		c.Param("id"), req.Favorite); err != nil {
		s.writeAppError(c, "更新收藏失败", err)
		return
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, gin.H{"favorite": req.Favorite})
}

// handleListAppGroups 处理 GET /api/v1/app-groups（权限 V）。
func (s *Server) handleListAppGroups(c *gin.Context) {
	limit, ok := listLimitMax(c, apps.MaxListLimit)
	if !ok {
		return
	}
	items, next, err := s.opts.Apps.ListGroups(c.Request.Context(), limit, c.Query("cursor"))
	if err != nil {
		s.writeAppError(c, "读取分组列表失败", err)
		return
	}
	resp := groupListResponse{Items: make([]groupDTO, 0, len(items))}
	for _, g := range items {
		resp.Items = append(resp.Items, groupDTOOf(g))
	}
	if next != "" {
		resp.NextCursor = &next
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, resp)
}

// handleCreateAppGroup 处理 POST /api/v1/app-groups（admin + CSRF）。
func (s *Server) handleCreateAppGroup(c *gin.Context) {
	sess := sessionFrom(c)
	const action = audit.ActionAppGroupCreate

	var req createGroupRequest
	if !decodeJSONBody(c, &req) {
		s.recordDenied(c, &sess, action, "app_group", "validation", httpx.CodeValidationError)
		return
	}

	var created apps.Group
	err := s.opts.Apps.InTransaction(c.Request.Context(), func(ctx context.Context) error {
		g, err := s.opts.Apps.CreateGroup(ctx, apps.GroupInput{Name: req.Name, SortOrder: req.SortOrder})
		if err != nil {
			return &errDenied{err}
		}
		if err := s.recordAuditInTx(ctx, c, sess, action, appGroupTarget(g.ID), map[string]any{
			"name":       g.Name,
			"sort_order": g.SortOrder,
		}); err != nil {
			return err
		}
		created = g
		return nil
	})
	if err != nil {
		s.failAppAction(c, &sess, action, "app_group", err)
		return
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusCreated, groupDTOOf(created))
}

// handleUpdateAppGroup 处理 PATCH /api/v1/app-groups/:id（admin + CSRF）。
func (s *Server) handleUpdateAppGroup(c *gin.Context) {
	sess := sessionFrom(c)
	const action = audit.ActionAppGroupUpdate

	id := c.Param("id")
	target := appGroupTarget(id)

	var req updateGroupRequest
	if !decodeJSONBody(c, &req) {
		s.recordDenied(c, &sess, action, target, "validation", httpx.CodeValidationError)
		return
	}
	if req.Version <= 0 {
		s.recordDenied(c, &sess, action, target, "validation", httpx.CodeValidationError)
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError, "version 必须是正整数")
		return
	}
	if req.Name == nil && req.SortOrder == nil {
		s.recordDenied(c, &sess, action, target, "validation", httpx.CodeValidationError)
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError, "至少需要提供一个要修改的字段")
		return
	}

	changed := map[string]any{"version": req.Version}
	if req.Name != nil {
		changed["name"] = *req.Name
	}
	if req.SortOrder != nil {
		changed["sort_order"] = *req.SortOrder
	}

	var updated apps.Group
	err := s.opts.Apps.InTransaction(c.Request.Context(), func(ctx context.Context) error {
		g, err := s.opts.Apps.UpdateGroup(ctx, id, req.Version, req.Name, req.SortOrder)
		if err != nil {
			return &errDenied{err}
		}
		changed["version"] = g.Version
		if err := s.recordAuditInTx(ctx, c, sess, action, target, changed); err != nil {
			return err
		}
		updated = g
		return nil
	})
	if err != nil {
		s.failAppAction(c, &sess, action, target, err)
		return
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, groupDTOOf(updated))
}

// handleDeleteAppGroup 处理 DELETE /api/v1/app-groups/:id（admin + CSRF）。
//
// 分组里还有应用时返回 422 DEPENDENCY_BLOCKED：删分组不会顺手把应用删掉或改成分组外，
// 那两种“顺手”都会让人在别处找不到自己的应用。
func (s *Server) handleDeleteAppGroup(c *gin.Context) {
	sess := sessionFrom(c)
	const action = audit.ActionAppGroupDelete

	id := c.Param("id")
	target := appGroupTarget(id)
	version, ok := deleteVersion(c)
	if !ok {
		s.recordDenied(c, &sess, action, target, "validation", httpx.CodeValidationError)
		return
	}

	err := s.opts.Apps.InTransaction(c.Request.Context(), func(ctx context.Context) error {
		if err := s.opts.Apps.DeleteGroup(ctx, id, version); err != nil {
			return &errDenied{err}
		}
		return s.recordAuditInTx(ctx, c, sess, action, target, map[string]any{"version": version})
	})
	if err != nil {
		s.failAppAction(c, &sess, action, target, err)
		return
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, struct{}{})
}

// appDTOFor 读一轮运行事实，把一条应用转成公开 DTO。
func (s *Server) appDTOFor(ctx context.Context, a apps.App) appDTO {
	return appDTOOf(a, s.appFacts(ctx).App(a))
}

// appFacts 读本轮的应用运行事实。
//
// AppRuntime 为 nil 表示这个部署没有接运行事实来源（演示模式）：那时每一条应用都
// 得到 unknown + 一句“未配置代理连接”。这不是降级成假数据，而是把“不知道”如实说出来。
func (s *Server) appFacts(ctx context.Context) *appruntime.Facts {
	if s.opts.AppRuntime == nil {
		return appruntime.NoFacts("未配置代理连接")
	}
	return s.opts.AppRuntime.Snapshot(ctx)
}

// appDTOOf 把业务对象与运行摘要转成公开 DTO。
//
// info 来自 appruntime：它是这条应用**此刻**的运行事实（或诚实的“不知道”）。
// 元数据层（internal/apps）不产生这些字段，也就不可能把它们写进库里。
func appDTOOf(a apps.App, info appruntime.Info) appDTO {
	dto := appDTO{
		ID:                   a.ID,
		Name:                 a.Name,
		Description:          a.Description,
		IconKey:              a.IconKey,
		Type:                 string(a.Type),
		Binding:              bindingDTOOf(a.Type, a.Binding),
		HealthCheck:          healthDTOOf(a.Health),
		VolumeDeps:           a.VolumeDependencies,
		Favorite:             a.Favorite,
		Version:              a.Version,
		CreatedAt:            a.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:            a.UpdatedAt.UTC().Format(time.RFC3339),
		RuntimeState:         info.RuntimeState,
		HTTPState:            apps.ObservedHTTPState(a.Health, a.HealthResult),
		DependencyProtection: info.Protection,
	}
	if info.RuntimeNote != "" {
		note := info.RuntimeNote
		dto.RuntimeNote = &note
	}
	if info.ProtectionNote != "" {
		note := info.ProtectionNote
		dto.DependencyProtectionNote = &note
	}
	if checked := apps.LastCheckedAt(a.Health, a.HealthResult); checked != nil {
		ts := checked.UTC().Format(time.RFC3339)
		dto.LastCheckedAt = &ts
	}
	if a.GroupID != "" {
		groupID := a.GroupID
		dto.GroupID = &groupID
	}
	if a.PublicURL != "" {
		publicURL := a.PublicURL
		dto.PublicURL = &publicURL
	}
	if dto.VolumeDeps == nil {
		// 空列表必须是 []，不能是 null：null 在 JSON 里是“不知道”，
		// 而“没有依赖卷”是一个确定的事实（CONTRACTS.md 第 1 节）。
		dto.VolumeDeps = []string{}
	}
	dto.Capabilities = appCapabilitiesDTO{
		Open:    info.Capabilities.Open,
		Start:   info.Capabilities.Start,
		Stop:    info.Capabilities.Stop,
		Restart: info.Capabilities.Restart,
		Logs:    info.Capabilities.Logs,
	}
	return dto
}

// bindingDTOOf 只输出与类型对应的那一个绑定字段。
func bindingDTOOf(t apps.Type, b apps.Binding) appBindingDTO {
	switch t {
	case apps.TypeSystemd:
		return appBindingDTO{Unit: b.Unit}
	case apps.TypeDocker:
		return appBindingDTO{ContainerID: b.ContainerID}
	case apps.TypeCompose:
		return appBindingDTO{DeploymentID: b.DeploymentID}
	}
	return appBindingDTO{}
}

func bindingFromDTO(b appBindingDTO) apps.Binding {
	return apps.Binding{
		Unit:         b.Unit,
		ContainerID:  b.ContainerID,
		DeploymentID: b.DeploymentID,
	}
}

// bindingSummary 是审计里用的绑定摘要（只记判别结果，不记整段 JSON）。
func bindingSummary(def apps.Definition) string {
	switch def.Type {
	case apps.TypeSystemd:
		return "unit=" + def.Binding.Unit
	case apps.TypeDocker:
		return "container_id=" + def.Binding.ContainerID
	case apps.TypeCompose:
		return "deployment_id=" + def.Binding.DeploymentID
	}
	return "none"
}

func healthDTOOf(h apps.HealthCheck) appHealthDTO {
	codes := h.ExpectedStatusCodes
	if codes == nil {
		codes = []int{}
	}
	return appHealthDTO{
		Enabled:             h.Enabled,
		TargetID:            h.TargetID,
		Scheme:              h.Scheme,
		Port:                h.Port,
		Path:                h.Path,
		ExpectedStatusCodes: codes,
		IntervalSeconds:     h.IntervalSeconds,
		TimeoutSeconds:      h.TimeoutSeconds,
	}
}

func healthFromDTO(h appHealthDTO) apps.HealthCheck {
	return apps.HealthCheck{
		Enabled:             h.Enabled,
		TargetID:            h.TargetID,
		Scheme:              h.Scheme,
		Port:                h.Port,
		Path:                h.Path,
		ExpectedStatusCodes: h.ExpectedStatusCodes,
		IntervalSeconds:     h.IntervalSeconds,
		TimeoutSeconds:      h.TimeoutSeconds,
	}
}

func groupDTOOf(g apps.Group) groupDTO {
	return groupDTO{
		ID:        g.ID,
		Name:      g.Name,
		SortOrder: g.SortOrder,
		Version:   g.Version,
		CreatedAt: g.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt: g.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

// appTarget 是审计里应用对象的写法。
func appTarget(id string) string { return "app:" + id }

// appGroupTarget 是审计里分组对象的写法。
func appGroupTarget(id string) string { return "app_group:" + id }

// writeAppError 把业务错误写成响应，内部错误只记日志。
func (s *Server) writeAppError(c *gin.Context, what string, err error) {
	status, code, msg, details, internal := mapAppError(err)
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

// failAppAction 处理“事务内失败”的响应与审计。
func (s *Server) failAppAction(c *gin.Context, sess *auth.Session, action, target string, err error) {
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
	status, code, msg, details, internal := mapAppError(businessErr)
	if internal {
		s.recordDenied(c, sess, action, target, "internal", code)
		s.internalError(c, "应用元数据操作失败", businessErr)
		return
	}
	s.recordDenied(c, sess, action, target, "business", code)
	if details != nil {
		httpx.WriteErrorDetails(c.Writer, c.Request, status, code, msg, details)
		return
	}
	writeError(c, status, code, msg)
}

// mapAppError 把业务错误映射成 HTTP 状态码、契约错误码与可选 details。
func mapAppError(err error) (status int, code httpx.Code, message string, details gin.H, internal bool) {
	var (
		validation *apps.ValidationError
		volumeMiss *apps.VolumeMissingError
		groupMiss  *apps.GroupMissingError
		targetMiss *apps.HealthTargetMissingError
	)
	switch {
	case errors.As(err, &validation):
		return http.StatusBadRequest, httpx.CodeValidationError, validation.Message, nil, false
	case errors.As(err, &targetMiss):
		return http.StatusUnprocessableEntity, httpx.CodeValidationError,
			"健康检查目标不在代理当前的发现结果里：" + targetMiss.TargetID,
			gin.H{"target_id": targetMiss.TargetID}, false
	case errors.As(err, &volumeMiss):
		return http.StatusUnprocessableEntity, httpx.CodeDependencyMissing,
			"依赖卷不存在：" + volumeMiss.VolumeID,
			gin.H{"volume_id": volumeMiss.VolumeID}, false
	case errors.As(err, &groupMiss):
		return http.StatusUnprocessableEntity, httpx.CodeValidationError,
			"分组不存在：" + groupMiss.GroupID,
			gin.H{"group_id": groupMiss.GroupID}, false
	case errors.Is(err, apps.ErrVolumeCheckUnavailable):
		return http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"无法确认依赖卷是否存在：代理不可用，本次没有保存", nil, false
	case errors.Is(err, apps.ErrHealthTargetCheckUnavailable):
		return http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"无法确认健康检查目标是否存在：代理不可用，本次没有保存", nil, false
	case errors.Is(err, apps.ErrInvalidLimit), errors.Is(err, apps.ErrInvalidCursor):
		return http.StatusBadRequest, httpx.CodeValidationError, err.Error(), nil, false
	case errors.Is(err, apps.ErrNotFound):
		return http.StatusNotFound, httpx.CodeNotFound, "应用不存在", nil, false
	case errors.Is(err, apps.ErrGroupNotFound):
		return http.StatusNotFound, httpx.CodeNotFound, "分组不存在", nil, false
	case errors.Is(err, apps.ErrVersionConflict):
		return http.StatusConflict, httpx.CodeVersionConflict, "已被其他人修改，请刷新后重试", nil, false
	case errors.Is(err, apps.ErrBindingTaken):
		return http.StatusUnprocessableEntity, httpx.CodeValidationError,
			"这个运行绑定已经被别的应用登记了；一个容器/单元/部署只能属于一个应用", nil, false
	case errors.Is(err, apps.ErrGroupNameTaken):
		return http.StatusUnprocessableEntity, httpx.CodeValidationError, "分组名已存在", nil, false
	case errors.Is(err, apps.ErrGroupInUse):
		return http.StatusUnprocessableEntity, httpx.CodeDependencyBlocked,
			"分组里还有应用，先把它们移出分组再删除", nil, false
	default:
		return http.StatusInternalServerError, httpx.CodeInternalError, "服务器内部错误", nil, true
	}
}
