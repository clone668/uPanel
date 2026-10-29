package api

// 备份管理（P12.2，CONTRACTS.md 第 12 节）。
//
// 这一组与站点、证书最大的不同：**备份定义不存在面板库里**。仓库与策略只有一份
// 真相，在代理的镜像里（见 internal/agentstore/backupstore.go 的注释）。因此这里的
// CRUD 不是在写自己的表，而是在编辑代理那份定义——推不过去就不算改成功。
//
// 直接后果，不粉饰：
//
//	代理不在时   整个 /backup/* 回 503 AGENT_UNAVAILABLE。不像 /proxies 那样可以
//	              "先登记、回头再 apply"——没有本地副本，"回头"是回不去的。
//	删除仓库      1.0 的契约里**没有** DELETE /backup/repositories/:id（只有 GET/POST/PATCH）。
//	              仓库一旦建成、快照就在里面，删掉定义会让"这些快照属于谁"无从回答。
//	              策略可以删（DELETE /backup/policies/:id）：删策略不动仓库也不动快照。
//
// 这一组**不执行备份**，也不删任何快照。执行是 backup.run 的预检 → 任务；清理是
// backup.prune 的预检 → 任务。契约明确要求"删除历史快照/清理仓库必须采用显式、
// 单独预检的策略执行，不在页面查询时触发"，所以这里连一个"清理"按钮的接口都没有。
//
// 权限（CONTRACTS 第 12 节）：整组 admin。仓库位置、SFTP 主机与用户名、备份了哪些
// 路径，合起来就是一张内网地图，viewer 不需要知道。登记与修改仓库还要再认证：
// 它们引用的是仓库口令与 SFTP 私钥，改一次定义就等于改一次"备份会往哪儿写"。

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"upanel/internal/agentclient"
	"upanel/internal/audit"
	"upanel/internal/auth"
	"upanel/internal/backup"
	"upanel/internal/httpx"
)

// BackupManager 是备份镜像与快照查询的调用端（P12.2）。
//
// 生产实现是 *agentclient.Client——它的方法签名本来就是这些，不需要再包一层适配器。
// 允许为 nil（演示模式不连代理）：那时 /backup/* 明确回 503，而不是给一份空列表——
// 空列表会被读成"这台机器上还没有配置备份"。
type BackupManager interface {
	ListBackupRepositories(ctx context.Context) ([]agentclient.BackupRepository, error)
	GetBackupRepository(ctx context.Context, id string) (agentclient.BackupRepository, error)
	CreateBackupRepository(ctx context.Context, def backup.Definition) (agentclient.BackupRepository, error)
	UpdateBackupRepository(ctx context.Context, id string, def backup.Definition, version int64) (agentclient.BackupRepository, error)

	ListBackupPolicies(ctx context.Context) ([]agentclient.BackupPolicy, error)
	GetBackupPolicy(ctx context.Context, id string) (agentclient.BackupPolicy, error)
	CreateBackupPolicy(ctx context.Context, pol backup.Policy) (agentclient.BackupPolicy, error)
	UpdateBackupPolicy(ctx context.Context, id string, pol backup.Policy, version int64) (agentclient.BackupPolicy, error)
	DeleteBackupPolicy(ctx context.Context, id string) error

	ListBackupSnapshots(ctx context.Context, repositoryID, policyID string) (agentclient.BackupSnapshotPage, error)
	GetBackupSnapshot(ctx context.Context, repositoryID, snapshotID string) (agentclient.BackupSnapshotDetail, error)
}

// backupRepositoryDTO 是一条仓库定义（CONTRACTS 第 12 节的字段，平铺）。
//
// password_secret_ref / credential_secret_ref 会**返回**：它们是引用而不是秘密，
// 而 PATCH 必须能把一份定义原样带回来（"读出来、改一处、存回去"是这里唯一的编辑
// 方式）。引用指向的秘密在被代理的加密存储里，面板从不持有明文。
type backupRepositoryDTO struct {
	ID                  string          `json:"id"`
	Name                string          `json:"name"`
	Backend             string          `json:"backend"`
	Location            backup.Location `json:"location"`
	PasswordSecretRef   string          `json:"password_secret_ref"`
	CredentialSecretRef string          `json:"credential_secret_ref,omitempty"`
	Version             int64           `json:"version"`
}

// backupRepositoryListResponse 是 GET /backup/repositories 的响应体。
type backupRepositoryListResponse struct {
	Items []backupRepositoryDTO `json:"items"`
	// RepositoryStateQuery 说明"这个仓库初始化了没有"要去哪儿问。
	//
	// 仓库现状只由 backup.repository.state 回答，不是这个接口的字段：它要现采
	// （restic 读 config、SFTP 拨号加主机密钥核对），塞进列表等于给每一条都加一次
	// 拨号，SFTP 慢的时候整页卡住。列的是名字，界面据此去查那一条。
	RepositoryStateQuery string `json:"repository_state_query"`
}

// backupPolicyDTO 是一条策略定义。
type backupPolicyDTO struct {
	ID              string           `json:"id"`
	Name            string           `json:"name"`
	RepositoryID    string           `json:"repository_id"`
	Sources         []backup.Source  `json:"sources"`
	ConsistencyMode string           `json:"consistency_mode"`
	AppIDs          []string         `json:"app_ids"`
	Retention       backup.Retention `json:"retention"`
	Version         int64            `json:"version"`
}

// backupPolicyListResponse 是 GET /backup/policies 的响应体。
type backupPolicyListResponse struct {
	Items []backupPolicyDTO `json:"items"`
}

// backupRepositoryBody 是 POST / PATCH /repositories 的请求体。
//
// 字段名与 backup.Definition 一致，但不直接用那个类型解码：PATCH 多一个 version，
// 而 Definition 里没有它。转一手，好过给 Definition 加一个只在一处有意义的字段。
type backupRepositoryBody struct {
	Name                string          `json:"name"`
	Backend             string          `json:"backend"`
	Location            backup.Location `json:"location"`
	PasswordSecretRef   string          `json:"password_secret_ref"`
	CredentialSecretRef string          `json:"credential_secret_ref"`
	Version             int64           `json:"version"`
}

// backupPolicyBody 是 POST / PATCH /policies 的请求体。
type backupPolicyBody struct {
	Name            string           `json:"name"`
	RepositoryID    string           `json:"repository_id"`
	Sources         []backup.Source  `json:"sources"`
	ConsistencyMode string           `json:"consistency_mode"`
	AppIDs          []string         `json:"app_ids"`
	Retention       backup.Retention `json:"retention"`
	Version         int64            `json:"version"`
}

// backupSnapshotDTO 是一条快照。
//
// size_bytes / file_count 是**字符串指针**且 omitempty，两件事一起说：
//
//	没读    列表查询不返回大小（每条要单独跑一次 restic stats），那时字段直接不出现，
//	        而不是写 0——"没读"与"读了是 0 字节"是两件不同的事，界面上也得能分开。
//	字符串  字节数按公共契约是大计数器（CONTRACTS §1、openapi 的 ByteString），前端不得用
//	        Number 做精确运算。这里跟着 storage 那一批走，不另立一套。
type backupSnapshotDTO struct {
	SnapshotID        string   `json:"snapshot_id"`
	ShortID           string   `json:"short_id"`
	Time              string   `json:"time"`
	Paths             []string `json:"paths"`
	Tags              []string `json:"tags"`
	Hostname          string   `json:"hostname,omitempty"`
	PolicyID          string   `json:"policy_id,omitempty"`
	ConsistencyMode   string   `json:"consistency_mode,omitempty"`
	SizeBytes         *string  `json:"size_bytes,omitempty"`
	FileCount         *string  `json:"file_count,omitempty"`
	RestoreVerifiedAt *string  `json:"restore_verified_at"`
}

// backupSnapshotListResponse 是 GET /backup/snapshots 的响应体。
type backupSnapshotListResponse struct {
	RepositoryID string              `json:"repository_id"`
	PolicyID     string              `json:"policy_id,omitempty"`
	Registered   bool                `json:"registered"`
	Count        int                 `json:"count"`
	Items        []backupSnapshotDTO `json:"items"`
	SizeIncluded bool                `json:"size_included"`
	CheckedAt    string              `json:"checked_at"`
}

// backupSnapshotDetailResponse 是 GET /backup/snapshots/:id 的响应体。
type backupSnapshotDetailResponse struct {
	RepositoryID string             `json:"repository_id"`
	Registered   bool               `json:"registered"`
	Found        bool               `json:"found"`
	Snapshot     *backupSnapshotDTO `json:"snapshot,omitempty"`
	CheckedAt    string             `json:"checked_at"`
}

// backupManager 取出调用端；没配置代理时写出 503 并返回 false。
func (s *Server) backupManager(c *gin.Context) (BackupManager, bool) {
	if s.opts.Backup == nil {
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"备份管理不可用：未配置代理连接（备份定义只存在代理里，面板没有本地副本）")
		return nil, false
	}
	return s.opts.Backup, true
}

// handleListBackupRepositories 处理 GET /api/v1/backup/repositories（admin）。
func (s *Server) handleListBackupRepositories(c *gin.Context) {
	mgr, ok := s.backupManager(c)
	if !ok {
		return
	}
	repos, err := mgr.ListBackupRepositories(c.Request.Context())
	if err != nil {
		s.writeBackupError(c, audit.ActionBackupRepositoryManage, "backup_repository", err)
		return
	}
	out := backupRepositoryListResponse{
		Items:                make([]backupRepositoryDTO, 0, len(repos)),
		RepositoryStateQuery: backup.QueryRepositoryState,
	}
	for _, repo := range repos {
		out.Items = append(out.Items, backupRepositoryDTOOf(repo))
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, out)
}

// handleGetBackupRepository 处理 GET /api/v1/backup/repositories/:id（admin）。
func (s *Server) handleGetBackupRepository(c *gin.Context) {
	mgr, ok := s.backupManager(c)
	if !ok {
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	if !backup.ValidRepositoryID(id) {
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError,
			"仓库 id 形态不合法（应为 rep_ 加 32 位小写十六进制）")
		return
	}
	repo, err := mgr.GetBackupRepository(c.Request.Context(), id)
	if err != nil {
		s.writeBackupError(c, audit.ActionBackupRepositoryManage, backupRepositoryTarget(id), err)
		return
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, backupRepositoryDTOOf(repo))
}

// handleCreateBackupRepository 处理 POST /api/v1/backup/repositories（admin + CSRF + 再认证）。
func (s *Server) handleCreateBackupRepository(c *gin.Context) {
	sess := sessionFrom(c)
	const action = audit.ActionBackupRepositoryCreate
	mgr, ok := s.backupManager(c)
	if !ok {
		return
	}
	def, _, ok := s.decodeBackupRepositoryBody(c, &sess, action)
	if !ok {
		return
	}
	repo, err := mgr.CreateBackupRepository(c.Request.Context(), def)
	if err != nil {
		s.writeBackupError(c, action, "backup_repository", err)
		return
	}
	s.recordBackupAudit(c, sess, action, backupRepositoryTarget(repo.ID), backupRepositoryAuditDetails(repo))
	httpx.WriteData(c.Writer, c.Request, http.StatusCreated, backupRepositoryDTOOf(repo))
}

// handleUpdateBackupRepository 处理 PATCH /api/v1/backup/repositories/:id（admin + CSRF + 再认证）。
func (s *Server) handleUpdateBackupRepository(c *gin.Context) {
	sess := sessionFrom(c)
	const action = audit.ActionBackupRepositoryUpdate
	mgr, ok := s.backupManager(c)
	if !ok {
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	if !backup.ValidRepositoryID(id) {
		s.recordDenied(c, &sess, action, "backup_repository", "validation", httpx.CodeValidationError)
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError,
			"仓库 id 形态不合法（应为 rep_ 加 32 位小写十六进制）")
		return
	}
	def, version, ok := s.decodeBackupRepositoryBody(c, &sess, action)
	if !ok {
		return
	}
	if version < 1 {
		// 与代理同一条判据：修改必须说明"我改的是哪一版"，否则两个人同时编辑时
		// 后写的那一份会悄悄覆盖前一份。
		s.recordDenied(c, &sess, action, backupRepositoryTarget(id), "version", httpx.CodeValidationError)
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError,
			"version 必须为正整数：修改仓库要带上当前版本")
		return
	}
	repo, err := mgr.UpdateBackupRepository(c.Request.Context(), id, def, version)
	if err != nil {
		s.writeBackupError(c, action, backupRepositoryTarget(id), err)
		return
	}
	s.recordBackupAudit(c, sess, action, backupRepositoryTarget(repo.ID), backupRepositoryAuditDetails(repo))
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, backupRepositoryDTOOf(repo))
}

// handleListBackupPolicies 处理 GET /api/v1/backup/policies（admin）。
func (s *Server) handleListBackupPolicies(c *gin.Context) {
	mgr, ok := s.backupManager(c)
	if !ok {
		return
	}
	policies, err := mgr.ListBackupPolicies(c.Request.Context())
	if err != nil {
		s.writeBackupError(c, audit.ActionBackupPolicyManage, "backup_policy", err)
		return
	}
	out := backupPolicyListResponse{Items: make([]backupPolicyDTO, 0, len(policies))}
	for _, pol := range policies {
		out.Items = append(out.Items, backupPolicyDTOOf(pol))
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, out)
}

// handleGetBackupPolicy 处理 GET /api/v1/backup/policies/:id（admin）。
func (s *Server) handleGetBackupPolicy(c *gin.Context) {
	mgr, ok := s.backupManager(c)
	if !ok {
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	if !backup.ValidPolicyID(id) {
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError,
			"策略 id 形态不合法（应为 pol_ 加 32 位小写十六进制）")
		return
	}
	pol, err := mgr.GetBackupPolicy(c.Request.Context(), id)
	if err != nil {
		s.writeBackupError(c, audit.ActionBackupPolicyManage, backupPolicyTarget(id), err)
		return
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, backupPolicyDTOOf(pol))
}

// handleCreateBackupPolicy 处理 POST /api/v1/backup/policies（admin + CSRF）。
//
// 策略登记**不**再认证：它不引用任何秘密，也不改变主机状态——真正会停应用、会读
// 仓库口令的是 backup.run，那条动作自己要再认证。契约第 12 节对政策组的权限列也只写
// 了 admin。
func (s *Server) handleCreateBackupPolicy(c *gin.Context) {
	sess := sessionFrom(c)
	const action = audit.ActionBackupPolicyCreate
	mgr, ok := s.backupManager(c)
	if !ok {
		return
	}
	pol, _, ok := s.decodeBackupPolicyBody(c, &sess, action)
	if !ok {
		return
	}
	created, err := mgr.CreateBackupPolicy(c.Request.Context(), pol)
	if err != nil {
		s.writeBackupError(c, action, "backup_policy", err)
		return
	}
	s.recordBackupAudit(c, sess, action, backupPolicyTarget(created.ID), backupPolicyAuditDetails(created))
	httpx.WriteData(c.Writer, c.Request, http.StatusCreated, backupPolicyDTOOf(created))
}

// handleUpdateBackupPolicy 处理 PATCH /api/v1/backup/policies/:id（admin + CSRF）。
func (s *Server) handleUpdateBackupPolicy(c *gin.Context) {
	sess := sessionFrom(c)
	const action = audit.ActionBackupPolicyUpdate
	mgr, ok := s.backupManager(c)
	if !ok {
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	if !backup.ValidPolicyID(id) {
		s.recordDenied(c, &sess, action, "backup_policy", "validation", httpx.CodeValidationError)
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError,
			"策略 id 形态不合法（应为 pol_ 加 32 位小写十六进制）")
		return
	}
	pol, version, ok := s.decodeBackupPolicyBody(c, &sess, action)
	if !ok {
		return
	}
	if version < 1 {
		s.recordDenied(c, &sess, action, backupPolicyTarget(id), "version", httpx.CodeValidationError)
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError,
			"version 必须为正整数：修改策略要带上当前版本")
		return
	}
	updated, err := mgr.UpdateBackupPolicy(c.Request.Context(), id, pol, version)
	if err != nil {
		s.writeBackupError(c, action, backupPolicyTarget(id), err)
		return
	}
	s.recordBackupAudit(c, sess, action, backupPolicyTarget(updated.ID), backupPolicyAuditDetails(updated))
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, backupPolicyDTOOf(updated))
}

// handleDeleteBackupPolicy 处理 DELETE /api/v1/backup/policies/:id（admin + CSRF）。
//
// 删除是幂等的：删一条不存在的策略也回 204。它只删策略——仓库、快照、以及"哪些
// 应用要停"的授权就地失联，但仓库里的数据一个块都不动（契约第 12 节）。
func (s *Server) handleDeleteBackupPolicy(c *gin.Context) {
	sess := sessionFrom(c)
	const action = audit.ActionBackupPolicyDelete
	mgr, ok := s.backupManager(c)
	if !ok {
		return
	}
	id := strings.TrimSpace(c.Param("id"))
	if !backup.ValidPolicyID(id) {
		s.recordDenied(c, &sess, action, "backup_policy", "validation", httpx.CodeValidationError)
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError,
			"策略 id 形态不合法（应为 pol_ 加 32 位小写十六进制）")
		return
	}
	if err := mgr.DeleteBackupPolicy(c.Request.Context(), id); err != nil {
		s.writeBackupError(c, action, backupPolicyTarget(id), err)
		return
	}
	s.recordBackupAudit(c, sess, action, backupPolicyTarget(id), map[string]any{
		"cascaded": false,
		// 说清删的是什么：策略、它的停应用授权，以及什么没被删。
		"deleted": "policy",
		"kept":    []string{"repository", "snapshots"},
		"note":    "删除策略不删除仓库或快照；清理必须显式发起 backup.prune",
	})
	// 与这组里其他写操作一样回 200 + 空对象，不用 204：统一信封是这个 API 的公共
	// 契约（每个响应都带 request_id），一个 204 会让前端那条通用解析路径在这里断掉。
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, struct{}{})
}

// handleListBackupSnapshots 处理 GET /api/v1/backup/snapshots?repository_id=&policy_id=（admin）。
//
// repository_id 必填：快照 id 是仓库内部的键，不跨仓库唯一；而且每个仓库要单独
// 解锁（口令不同、本地与远端不同），"全部仓库的快照"这张表在代理里根本不存在。
func (s *Server) handleListBackupSnapshots(c *gin.Context) {
	mgr, ok := s.backupManager(c)
	if !ok {
		return
	}
	repoID := strings.TrimSpace(c.Query("repository_id"))
	if repoID == "" {
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError,
			"缺少 repository_id：快照按仓库读取，没有跨仓库的快照列表")
		return
	}
	if !backup.ValidRepositoryID(repoID) {
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError,
			"repository_id 形态不合法（应为 rep_ 加 32 位小写十六进制）")
		return
	}
	policyID := strings.TrimSpace(c.Query("policy_id"))
	if policyID != "" && !backup.ValidPolicyID(policyID) {
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError,
			"policy_id 形态不合法（应为 pol_ 加 32 位小写十六进制）")
		return
	}
	// 快照列表可能要在仓库索引上走一趟（大仓库不是一次 cat config），给它宽一点的
	// 写截止时间，由代理侧的查询预算先一步给结论（见 internal/agent/backuprun.go）。
	extendWriteDeadline(c, slowQueryBudget)
	page, err := mgr.ListBackupSnapshots(c.Request.Context(), repoID, policyID)
	if err != nil {
		s.writeBackupError(c, audit.ActionBackupPolicyManage, backupRepositoryTarget(repoID), err)
		return
	}
	out := backupSnapshotListResponse{
		RepositoryID: page.RepositoryID,
		PolicyID:     page.PolicyID,
		Registered:   page.Registered,
		Count:        len(page.Snapshots),
		Items:        make([]backupSnapshotDTO, 0, len(page.Snapshots)),
		SizeIncluded: page.SizeIncluded,
		CheckedAt:    backupTime(page.CheckedAt),
	}
	for _, snap := range page.Snapshots {
		out.Items = append(out.Items, backupSnapshotDTOOf(snap))
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, out)
}

// handleGetBackupSnapshot 处理 GET /api/v1/backup/snapshots/:id?repository_id=（admin）。
//
// repository_id 是必填的查询参数：只知道快照 id 时，代理要挨个仓库去问"它在不在"，
// 而每个仓库都要口令与拨号。让调用方给出它在哪个仓库里，是一次如实的分工——页面
// 本来就是从某个仓库的列表点进来的。
func (s *Server) handleGetBackupSnapshot(c *gin.Context) {
	mgr, ok := s.backupManager(c)
	if !ok {
		return
	}
	repoID := strings.TrimSpace(c.Query("repository_id"))
	if repoID == "" {
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError,
			"缺少 repository_id：同一个快照 id 在不同仓库里没有意义，必须说明它在哪个仓库")
		return
	}
	if !backup.ValidRepositoryID(repoID) {
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError,
			"repository_id 形态不合法（应为 rep_ 加 32 位小写十六进制）")
		return
	}
	snapshotID := strings.TrimSpace(c.Param("id"))
	if !validSnapshotID(snapshotID) {
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError,
			"快照 id 必须是完整的 64 位十六进制（restic 的短 id 不接受：它不唯一）")
		return
	}
	extendWriteDeadline(c, slowQueryBudget)
	detail, err := mgr.GetBackupSnapshot(c.Request.Context(), repoID, snapshotID)
	if err != nil {
		s.writeBackupError(c, audit.ActionBackupPolicyManage, backupRepositoryTarget(repoID), err)
		return
	}
	out := backupSnapshotDetailResponse{
		RepositoryID: repoID,
		Registered:   detail.Registered,
		Found:        detail.Found,
		CheckedAt:    backupTime(detail.CheckedAt),
	}
	if detail.Found {
		snap := backupSnapshotDTOOf(detail.BackupSnapshot)
		out.Snapshot = &snap
	}
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, out)
}

// decodeBackupRepositoryBody 读仓库请求体并规范化定义。
func (s *Server) decodeBackupRepositoryBody(c *gin.Context, sess *auth.Session, action string) (backup.Definition, int64, bool) {
	var body backupRepositoryBody
	if !decodeJSONBody(c, &body) {
		s.recordDenied(c, sess, action, "backup_repository", "validation", httpx.CodeValidationError)
		return backup.Definition{}, 0, false
	}
	def, err := backup.Definition{
		Name:                body.Name,
		Backend:             backup.Backend(body.Backend),
		Location:            body.Location,
		PasswordSecretRef:   body.PasswordSecretRef,
		CredentialSecretRef: body.CredentialSecretRef,
	}.Normalize()
	if err != nil {
		s.recordDenied(c, sess, action, "backup_repository", "validation", httpx.CodeValidationError)
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError, err.Error())
		return backup.Definition{}, 0, false
	}
	return def, body.Version, true
}

// decodeBackupPolicyBody 读策略请求体并规范化策略。
func (s *Server) decodeBackupPolicyBody(c *gin.Context, sess *auth.Session, action string) (backup.Policy, int64, bool) {
	var body backupPolicyBody
	if !decodeJSONBody(c, &body) {
		s.recordDenied(c, sess, action, "backup_policy", "validation", httpx.CodeValidationError)
		return backup.Policy{}, 0, false
	}
	pol, err := backup.Policy{
		Name:            body.Name,
		RepositoryID:    body.RepositoryID,
		Sources:         body.Sources,
		ConsistencyMode: backup.ConsistencyMode(body.ConsistencyMode),
		AppIDs:          body.AppIDs,
		Retention:       body.Retention,
	}.Normalize()
	if err != nil {
		s.recordDenied(c, sess, action, "backup_policy", "validation", httpx.CodeValidationError)
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError, err.Error())
		return backup.Policy{}, 0, false
	}
	return pol, body.Version, true
}

// writeBackupError 把镜像调用失败映射成契约错误码。
//
// 代理明确拒绝时按它给的 code 返回（NOT_FOUND → 404、VERSION_CONFLICT → 409）：
// 这两个是编辑备份定义时最常撞上的两件事——"这条记录已经不在了"和"别人先改了一步"，
// 它们必须是两个不同的状态码，否则界面只能笼统地说"失败了"。
func (s *Server) writeBackupError(c *gin.Context, action, target string, err error) {
	var unavailable *agentclient.UnavailableError
	var agentErr *agentclient.Error
	switch {
	case errors.As(err, &unavailable):
		s.opts.Logger.Warn("备份镜像调用失败：代理不可用", "error", err,
			"request_id", c.GetString(ctxKeyRequestID))
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"特权代理不可用，暂时读不到也改不了备份定义")
	case errors.As(err, &agentErr):
		code := httpx.Code(agentErr.Code)
		if !knownCode(code) {
			s.opts.Logger.Error("代理返回未知错误码", "code", agentErr.Code,
				"request_id", c.GetString(ctxKeyRequestID))
			writeError(c, http.StatusInternalServerError, httpx.CodeInternalError,
				"代理返回未知错误码")
			return
		}
		writeError(c, httpx.StatusForCode(code), code, agentErr.Message)
	default:
		s.internalError(c, "备份镜像操作失败", err)
	}
}

// recordBackupAudit 写一条成功的审计事件。
//
// 与站点/证书不同，这里**无法**把审计与写入放进同一个事务：定义写在代理的库里，
// 面板没有事务可用。所以审计写失败时只记日志、不改响应——那次改动已经真的发生了，
// 回 500 既撤不回来，还会让调用方重试（重试一次 PATCH 会撞 VERSION_CONFLICT，
// 重试一次 POST 会在镜像里多一条定义）。这是取舍，不是疏忽。
func (s *Server) recordBackupAudit(c *gin.Context, sess auth.Session, action, target string, details map[string]any) {
	if err := s.recordAuditInTx(c.Request.Context(), c, sess, action, target, details); err != nil {
		s.opts.Logger.Error("备份审计写入失败（改动已生效）", "error", err,
			"action", action, "target", target, "request_id", c.GetString(ctxKeyRequestID))
	}
}

// backupRepositoryDTOOf 把镜像里的仓库翻成对外形状。
func backupRepositoryDTOOf(repo agentclient.BackupRepository) backupRepositoryDTO {
	return backupRepositoryDTO{
		ID:                  repo.ID,
		Name:                repo.Name,
		Backend:             repo.Backend,
		Location:            repo.Definition.Location,
		PasswordSecretRef:   repo.Definition.PasswordSecretRef,
		CredentialSecretRef: repo.Definition.CredentialSecretRef,
		Version:             repo.Version,
	}
}

// backupPolicyDTOOf 把镜像里的策略翻成对外形状。
//
// app_ids 为空时输出 `[]` 而不是 null：files_only 策略"没有要停的应用"是一个明确的
// 事实，null 会被读成"这一项没给"。
func backupPolicyDTOOf(pol agentclient.BackupPolicy) backupPolicyDTO {
	out := backupPolicyDTO{
		ID:              pol.ID,
		Name:            pol.Name,
		RepositoryID:    pol.RepositoryID,
		Sources:         pol.Sources,
		ConsistencyMode: pol.ConsistencyMode,
		AppIDs:          pol.AppIDs,
		Retention:       pol.Retention,
		Version:         pol.Version,
	}
	if out.Sources == nil {
		out.Sources = []backup.Source{}
	}
	if out.AppIDs == nil {
		out.AppIDs = []string{}
	}
	return out
}

// backupSnapshotDTOOf 把一条快照翻成对外形状。
func backupSnapshotDTOOf(snap agentclient.BackupSnapshot) backupSnapshotDTO {
	out := backupSnapshotDTO{
		SnapshotID:      snap.SnapshotID,
		ShortID:         snap.ShortID,
		Time:            backupTime(snap.Time),
		Paths:           snap.Paths,
		Tags:            snap.Tags,
		Hostname:        snap.Hostname,
		PolicyID:        snap.PolicyID,
		ConsistencyMode: snap.ConsistencyMode,
		SizeBytes:       byteCount(snap.SizeBytes),
		FileCount:       byteCount(snap.FileCount),
	}
	if out.Paths == nil {
		out.Paths = []string{}
	}
	if out.Tags == nil {
		out.Tags = []string{}
	}
	if snap.RestoreVerifiedAt != nil {
		at := backupTime(*snap.RestoreVerifiedAt)
		out.RestoreVerifiedAt = &at
	}
	return out
}

// byteCount 把可能的大计数器转成公共契约要求的十进制字符串；nil 保持 nil（没读）。
func byteCount(v *int64) *string {
	if v == nil {
		return nil
	}
	s := strconv.FormatInt(*v, 10)
	return &s
}

// backupTime 统一时间格式；零值写空串，不写 1970。
func backupTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// validSnapshotID 判断快照 id 形状（64 位小写十六进制，与 restic 一致）。
func validSnapshotID(id string) bool {
	if len(id) != 64 {
		return false
	}
	for _, r := range id {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// backupRepositoryTarget / backupPolicyTarget 是审计里的作用对象。
func backupRepositoryTarget(id string) string { return "backup_repository:" + id }
func backupPolicyTarget(id string) string     { return "backup_policy:" + id }

// backupRepositoryAuditDetails 是一条仓库成功的审计细节。
//
// **不写两份秘密引用**：审计组件会按字段名过滤含 secret/password 的键，但这里干脆
// 只记"有没有配"。要回答的问题是"谁把备份改到了哪台机器"，不是"引用是哪个字符串"。
func backupRepositoryAuditDetails(repo agentclient.BackupRepository) map[string]any {
	loc := repo.Definition.Location
	details := map[string]any{
		"name":                repo.Name,
		"backend":             repo.Backend,
		"version":             repo.Version,
		"credential_required": repo.Definition.CredentialSecretRef != "",
	}
	switch backup.Backend(repo.Backend) {
	case backup.BackendLocal:
		details["root_id"] = loc.RootID
		details["relative_path"] = loc.RelativePath
	case backup.BackendSFTP:
		details["host"] = loc.Host
		details["port"] = loc.Port
		details["username"] = loc.Username
		details["remote_path"] = loc.RemotePath
		details["host_key_fingerprint"] = loc.HostKeyFingerprint
	}
	return details
}

// backupPolicyAuditDetails 是一条策略成功的审计细节。
func backupPolicyAuditDetails(pol agentclient.BackupPolicy) map[string]any {
	return map[string]any{
		"name":                pol.Name,
		"version":             pol.Version,
		"repository_id":       pol.RepositoryID,
		"consistency_mode":    pol.ConsistencyMode,
		"app_count":           len(pol.AppIDs),
		"source_count":        len(pol.Sources),
		"retention_keep_last": pol.Retention.KeepLast,
	}
}
