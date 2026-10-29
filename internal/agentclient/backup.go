package agentclient

// 备份定义镜像的客户端（P12.1 仓库、P12.2 策略与快照查询）。
//
// 备份定义（仓库与策略）**只有一份真相：代理那份镜像**。Web 侧不另存一份——预检
// 计划里的 normalized_params 是冻结的（只含 id 与 version），执行时若再问一遍 Web
// 要定义，就等于让”被冻结的那个目标”可以被替换。因此这里做的不是”同步”，而是
// “编辑代理里那一份”：每次改动都必须推过去，推失败就不能认为改动生效了。
//
// 代价要说清：代理不在时备份管理是**读不了也改不了**的（503），不像站点或证书
// 那样可以离线登记。这是上面那条取舍的直接后果，不该假装成”暂时读不到”。
//
// 定义类型直接用 internal/backup.Definition / Policy：同一份形状只该有一处定义，
// 在那之前先复制一份到客户端，等于给”哪一份算数”埋一个分歧点。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"upanel/internal/backup"
)

// BackupRepositoriesPath 是仓库镜像的内部路径前缀。
const BackupRepositoriesPath = "/internal/v1/backup/repositories"

// BackupRepository 是代理镜像里的一条仓库定义。
type BackupRepository struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Version    int64             `json:"version"`
	Backend    string            `json:"backend"`
	Location   string            `json:"location"`
	Definition backup.Definition `json:"definition"`
}

// backupRepositoryEnvelope 是代理的单条响应。
type backupRepositoryEnvelope struct {
	Repository BackupRepository `json:"repository"`
}

// backupRepositoryListEnvelope 是代理的列表响应。
type backupRepositoryListEnvelope struct {
	Repositories []BackupRepository `json:"repositories"`
}

// backupRepositoryPath 返回单条仓库的路径。
func backupRepositoryPath(id string) string {
	return BackupRepositoriesPath + "/" + url.PathEscape(id)
}

// ListBackupRepositories 列出代理里的全部仓库定义。
func (c *Client) ListBackupRepositories(ctx context.Context) ([]BackupRepository, error) {
	data, err := c.do(ctx, http.MethodGet, BackupRepositoriesPath, nil)
	if err != nil {
		return nil, err
	}
	var out backupRepositoryListEnvelope
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, &UnavailableError{Err: fmt.Errorf("代理返回的仓库列表不是合法 JSON: %w", err)}
	}
	return out.Repositories, nil
}

// GetBackupRepository 读取一条仓库定义。
func (c *Client) GetBackupRepository(ctx context.Context, id string) (BackupRepository, error) {
	if id == "" {
		return BackupRepository{}, errors.New("agentclient: repository_id 不能为空")
	}
	data, err := c.do(ctx, http.MethodGet, backupRepositoryPath(id), nil)
	if err != nil {
		return BackupRepository{}, err
	}
	return decodeRepository(data)
}

// CreateBackupRepository 在代理里登记一条仓库定义（id 由代理生成）。
func (c *Client) CreateBackupRepository(ctx context.Context, def backup.Definition) (BackupRepository, error) {
	data, err := c.do(ctx, http.MethodPost, BackupRepositoriesPath, map[string]any{"definition": def})
	if err != nil {
		return BackupRepository{}, err
	}
	return decodeRepository(data)
}

// UpdateBackupRepository 按期望版本更新一条定义（版本不符时代理回 VERSION_CONFLICT）。
func (c *Client) UpdateBackupRepository(ctx context.Context, id string, def backup.Definition, version int64) (BackupRepository, error) {
	if id == "" {
		return BackupRepository{}, errors.New("agentclient: repository_id 不能为空")
	}
	if version < 1 {
		return BackupRepository{}, errors.New("agentclient: version 必须为正整数")
	}
	data, err := c.do(ctx, http.MethodPatch, backupRepositoryPath(id), map[string]any{
		"definition": def,
		"version":    version,
	})
	if err != nil {
		return BackupRepository{}, err
	}
	return decodeRepository(data)
}

// DeleteBackupRepository 删除代理里的一条定义。
//
// 它只删定义：仓库数据与快照留在原地（CONTRACTS 第 12 节），清理只能走显式的
// backup.prune。删除是幂等的，所以 404 也算成功——重试不该变成错误。
func (c *Client) DeleteBackupRepository(ctx context.Context, id string) error {
	if id == "" {
		return errors.New("agentclient: repository_id 不能为空")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, "http://agent"+backupRepositoryPath(id), nil)
	if err != nil {
		return fmt.Errorf("agentclient: 构造请求失败: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return &UnavailableError{Err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	// 204 是正常成功：这条路由没有响应体，decodeEnvelope 会把空体当成“缺少 data”。
	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if _, err := decodeEnvelope(resp); err != nil {
		return err
	}
	return nil
}

// decodeRepository 解出单条仓库响应。
func decodeRepository(data json.RawMessage) (BackupRepository, error) {
	var out backupRepositoryEnvelope
	if err := json.Unmarshal(data, &out); err != nil {
		return BackupRepository{}, &UnavailableError{Err: fmt.Errorf("代理返回的仓库不是合法 JSON: %w", err)}
	}
	if out.Repository.ID == "" {
		return BackupRepository{}, &UnavailableError{Err: errors.New("代理返回的仓库缺少 id")}
	}
	return out.Repository, nil
}

// BackupPoliciesPath 是策略镜像的内部路径前缀（P12.2）。
const BackupPoliciesPath = "/internal/v1/backup/policies"

// BackupPolicy 是代理镜像里的一条策略定义。
//
// 形状与代理的 backupPolicyView 一一对应：策略摊开在顶层而不是塞一个 policy_json，
// 因为 Web 侧也要用它做展示与预检参数（页面不该为了读一个字段先解一层字符串）。
type BackupPolicy struct {
	ID              string           `json:"id"`
	Name            string           `json:"name"`
	Version         int64            `json:"version"`
	RepositoryID    string           `json:"repository_id"`
	Sources         []backup.Source  `json:"sources"`
	ConsistencyMode string           `json:"consistency_mode"`
	AppIDs          []string         `json:"app_ids,omitempty"`
	Retention       backup.Retention `json:"retention"`
}

// Policy 把镜像里的扁平形状拼回 internal/backup 的策略类型。
//
// 用它可以拿镜像里的定义去做指纹、去算预检参数，而不用在 Web 侧再维护一份
// "策略长什么样"的结构——两份形状一有分叉，fingerprint 就对不上了。
func (p BackupPolicy) Policy() backup.Policy {
	return backup.Policy{
		Name:            p.Name,
		RepositoryID:    p.RepositoryID,
		Sources:         p.Sources,
		ConsistencyMode: backup.ConsistencyMode(p.ConsistencyMode),
		AppIDs:          p.AppIDs,
		Retention:       p.Retention,
	}
}

type backupPolicyEnvelope struct {
	Policy BackupPolicy `json:"policy"`
}

type backupPolicyListEnvelope struct {
	Policies []BackupPolicy `json:"policies"`
}

func backupPolicyPath(id string) string {
	return BackupPoliciesPath + "/" + url.PathEscape(id)
}

// ListBackupPolicies 列出代理里的全部策略定义。
func (c *Client) ListBackupPolicies(ctx context.Context) ([]BackupPolicy, error) {
	data, err := c.do(ctx, http.MethodGet, BackupPoliciesPath, nil)
	if err != nil {
		return nil, err
	}
	var out backupPolicyListEnvelope
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, &UnavailableError{Err: fmt.Errorf("代理返回的策略列表不是合法 JSON: %w", err)}
	}
	return out.Policies, nil
}

// GetBackupPolicy 读取一条策略定义。
func (c *Client) GetBackupPolicy(ctx context.Context, id string) (BackupPolicy, error) {
	if id == "" {
		return BackupPolicy{}, errors.New("agentclient: policy_id 不能为空")
	}
	data, err := c.do(ctx, http.MethodGet, backupPolicyPath(id), nil)
	if err != nil {
		return BackupPolicy{}, err
	}
	return decodePolicy(data)
}

// CreateBackupPolicy 在代理里登记一条策略（id 由代理生成）。
func (c *Client) CreateBackupPolicy(ctx context.Context, pol backup.Policy) (BackupPolicy, error) {
	data, err := c.do(ctx, http.MethodPost, BackupPoliciesPath, map[string]any{"policy": pol})
	if err != nil {
		return BackupPolicy{}, err
	}
	return decodePolicy(data)
}

// UpdateBackupPolicy 按期望版本更新一条策略（版本不符时代理回 409 VERSION_CONFLICT）。
func (c *Client) UpdateBackupPolicy(ctx context.Context, id string, pol backup.Policy, version int64) (BackupPolicy, error) {
	if id == "" {
		return BackupPolicy{}, errors.New("agentclient: policy_id 不能为空")
	}
	if version < 1 {
		return BackupPolicy{}, errors.New("agentclient: version 必须为正整数")
	}
	data, err := c.do(ctx, http.MethodPatch, backupPolicyPath(id), map[string]any{
		"policy":  pol,
		"version": version,
	})
	if err != nil {
		return BackupPolicy{}, err
	}
	return decodePolicy(data)
}

// DeleteBackupPolicy 删除代理里的一条策略。
//
// 幂等：404 也算成功，重试不该变成错误。删策略不动仓库，也不动已经建成的快照。
func (c *Client) DeleteBackupPolicy(ctx context.Context, id string) error {
	if id == "" {
		return errors.New("agentclient: policy_id 不能为空")
	}
	return c.doNoContent(ctx, http.MethodDelete, backupPolicyPath(id))
}

// decodePolicy 解出单条策略响应。
func decodePolicy(data json.RawMessage) (BackupPolicy, error) {
	var out backupPolicyEnvelope
	if err := json.Unmarshal(data, &out); err != nil {
		return BackupPolicy{}, &UnavailableError{Err: fmt.Errorf("代理返回的策略不是合法 JSON: %w", err)}
	}
	if out.Policy.ID == "" {
		return BackupPolicy{}, &UnavailableError{Err: errors.New("代理返回的策略缺少 id")}
	}
	return out.Policy, nil
}

// doNoContent 发一个不关心响应体的请求（204 与 404 都算成功）。
func (c *Client) doNoContent(ctx context.Context, method, path string) error {
	req, err := http.NewRequestWithContext(ctx, method, "http://agent"+path, nil)
	if err != nil {
		return fmt.Errorf("agentclient: 构造请求失败: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return &UnavailableError{Err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if _, err := decodeEnvelope(resp); err != nil {
		return err
	}
	return nil
}

// 快照查询（P12.2）。两条查询名与 internal/backup 的 QuerySnapshots/QuerySnapshot 逐字一致。
const (
	SnapshotsQueryName = "backup.snapshots"
	SnapshotQueryName  = "backup.snapshot"

	// BackupQueryTimeout 必须晚于代理侧 90 秒的快照查询预算到期。
	// SFTP 列表包含拨号、主机密钥核对与 restic 输出；10 秒通用上限会把一次正常慢查询
	// 掐成 AGENT_UNAVAILABLE，代理随后只能看到调用方留下的 context canceled。
	// 100 秒仍低于 Web 的 120 秒 WriteTimeout，给代理收尾与 HTTP 响应留出余量。
	BackupQueryTimeout = 100 * time.Second
)

// BackupSnapshot 是一条快照。
//
// size_bytes / file_count 是指针：列表查询**不返回**大小（每条要单独跑一次
// `restic stats`），nil 的意思是"这次没读"，不是"0 字节"。界面必须能分开这两件事。
type BackupSnapshot struct {
	SnapshotID        string     `json:"snapshot_id"`
	ShortID           string     `json:"short_id"`
	Time              time.Time  `json:"time"`
	Paths             []string   `json:"paths"`
	Tags              []string   `json:"tags"`
	Hostname          string     `json:"hostname"`
	PolicyID          string     `json:"policy_id"`
	ConsistencyMode   string     `json:"consistency_mode"`
	SizeBytes         *int64     `json:"size_bytes"`
	FileCount         *int64     `json:"file_count"`
	RestoreVerifiedAt *time.Time `json:"restore_verified_at"`
}

// BackupSnapshotPage 是 GET /backup/snapshots 背后那条查询的返回。
//
// Registered=false 表示**代理里没有这个仓库**（不是"查询不存在"，也不是"没有快照"）：
// 那时 snapshots 是空的，但空的原因是别的东西。
type BackupSnapshotPage struct {
	RepositoryID string           `json:"repository_id"`
	PolicyID     string           `json:"policy_id"`
	Registered   bool             `json:"registered"`
	Count        int              `json:"count"`
	Snapshots    []BackupSnapshot `json:"snapshots"`
	SizeIncluded bool             `json:"size_included"`
	CheckedAt    time.Time        `json:"checked_at"`
}

// BackupSnapshotDetail 是 GET /backup/snapshots/:id 背后那条查询的返回。
type BackupSnapshotDetail struct {
	BackupSnapshot
	Registered bool      `json:"registered"`
	Found      bool      `json:"found"`
	CheckedAt  time.Time `json:"checked_at"`
}

// backupSnapshotsParams 是 backup.snapshots 的参数。字段名与代理侧一致。
type backupSnapshotsParams struct {
	RepositoryID string `json:"repository_id"`
	PolicyID     string `json:"policy_id,omitempty"`
}

type backupSnapshotParams struct {
	RepositoryID string `json:"repository_id"`
	SnapshotID   string `json:"snapshot_id"`
}

// ListBackupSnapshots 读一个仓库里的快照列表（可按策略过滤）。
func (c *Client) ListBackupSnapshots(ctx context.Context, repositoryID, policyID string) (BackupSnapshotPage, error) {
	if repositoryID == "" {
		return BackupSnapshotPage{}, errors.New("agentclient: repository_id 不能为空")
	}
	raw, err := c.Query(ctx, SnapshotsQueryName, backupSnapshotsParams{
		RepositoryID: repositoryID,
		PolicyID:     policyID,
	})
	if err != nil {
		return BackupSnapshotPage{}, err
	}
	var out BackupSnapshotPage
	if err := json.Unmarshal(raw, &out); err != nil {
		return BackupSnapshotPage{}, &UnavailableError{Err: fmt.Errorf("代理返回的快照列表不是合法 JSON: %w", err)}
	}
	return out, nil
}

// GetBackupSnapshot 读一条快照的详情（含大小与文件数）。
func (c *Client) GetBackupSnapshot(ctx context.Context, repositoryID, snapshotID string) (BackupSnapshotDetail, error) {
	if repositoryID == "" {
		return BackupSnapshotDetail{}, errors.New("agentclient: repository_id 不能为空")
	}
	if snapshotID == "" {
		return BackupSnapshotDetail{}, errors.New("agentclient: snapshot_id 不能为空")
	}
	raw, err := c.Query(ctx, SnapshotQueryName, backupSnapshotParams{
		RepositoryID: repositoryID,
		SnapshotID:   snapshotID,
	})
	if err != nil {
		return BackupSnapshotDetail{}, err
	}
	var out BackupSnapshotDetail
	if err := json.Unmarshal(raw, &out); err != nil {
		return BackupSnapshotDetail{}, &UnavailableError{Err: fmt.Errorf("代理返回的快照详情不是合法 JSON: %w", err)}
	}
	return out, nil
}
