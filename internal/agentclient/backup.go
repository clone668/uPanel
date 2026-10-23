package agentclient

// 备份仓库定义镜像的客户端（P12.1）。
//
// Web 侧有一份自己的 backup_repositories 表；这里同步的是**代理**那一份。两边用同一个
// id 与同一个 version，代理执行时只认自己那份——所以 Web 每次改定义都必须推过去，
// 推失败就不能认为改动生效了。
//
// 定义类型直接用 internal/backup.Definition：同一份形状只该有一处定义，
// 在那之前先复制一份到客户端，等于给“哪一份算数”埋一个分歧点。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"

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
