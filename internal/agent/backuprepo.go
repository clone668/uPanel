package agent

// 备份仓库定义的代理镜像（P12.1）。
//
// 为什么代理要自己存一份：backup.repository.initialize 的参数被契约冻结成
// `repository_id` + `version` 两项，里面**没有**路径、主机、秘密引用。而契约第 12 节
// 同时要求「代理验证路径/主机」——如果定义只存在 Web 库里，代理就只能相信 Web 送来的
// 任何字符串，那条要求就变成了空话。所以 Web 在登记或修改仓库时把定义同步进代理镜像，
// 代理执行时按 id 从**自己**的库里取，并核对 version。
//
// 这条路由只允许 Web 账号（对端 UID 校验在最外层 gate 里做，root 也不放行）：
// 没有它，Web 就永远开不出一个能初始化的仓库；有它，代理仍然每次都自己判定
// （restic 在不在、秘密用途对不对、主机密钥是否匹配、位置是否已是仓库、空间够不够）。

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"upanel/internal/agentstore"
	"upanel/internal/backup"
	"upanel/internal/httpx"
)

// maxBackupDefinitionBytes 是这条路由的请求体上限。
//
// 一条仓库定义只有名字、后端、路径/主机、端口、秘密引用与指纹，正常不到 1 KiB；
// 8 KiB 足够容纳最长的路径与注释，同时挡住把这条路由当成上传口的用法。
const maxBackupDefinitionBytes = 8 << 10

// BackupRepositoryMirror 是仓库定义镜像的存储能力。
//
// 生产实现是 agentstore.BackupRepositoryStore；测试用一个内存实现。
type BackupRepositoryMirror interface {
	ListRepositories(ctx context.Context) ([]backup.Repository, error)
	GetRepository(ctx context.Context, id string) (backup.Repository, bool, error)
	CreateRepository(ctx context.Context, def backup.Definition, now time.Time) (backup.Repository, error)
	UpdateRepository(ctx context.Context, id string, def backup.Definition, expectedVersion int64, now time.Time) (backup.Repository, error)
	DeleteRepository(ctx context.Context, id string) error
}

// backupRepositoryView 是仓库定义对外的形状（不含任何明文秘密）。
type backupRepositoryView struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Version    int64             `json:"version"`
	Backend    string            `json:"backend"`
	Location   string            `json:"location"`
	Definition backup.Definition `json:"definition"`
}

// backupRepositoryRequest 是 POST / PATCH 的请求体。
//
// 只有定义与（PATCH 时）期望版本：id 由代理生成，秘密只能以引用出现（Normalize 校验
// `sec_` 形状与用途分离，明文永远不走这条路由）。
type backupRepositoryRequest struct {
	Definition backup.Definition `json:"definition"`
	// Version 是 PATCH 的乐观并发令牌；POST 时忽略。
	Version int64 `json:"version"`
}

// backupRepositoryResponse 是单条响应。
type backupRepositoryResponse struct {
	Repository backupRepositoryView `json:"repository"`
}

// backupRepositoryListResponse 是列表响应。
type backupRepositoryListResponse struct {
	Repositories []backupRepositoryView `json:"repositories"`
}

// handleBackupRepositories 处理 /internal/v1/backup/repositories。
func (s *Server) handleBackupRepositories(w http.ResponseWriter, r *http.Request) {
	mirror := s.opts.BackupRepositories
	if mirror == nil {
		// 没有接线就没有这条能力：明确 404，而不是给一个能写进去却没人看的表。
		httpx.WriteError(w, r, http.StatusNotFound, httpx.CodeNotFound, "代理未启用备份仓库镜像")
		return
	}
	switch r.Method {
	case http.MethodGet:
		repos, err := mirror.ListRepositories(r.Context())
		if err != nil {
			s.writeBackupRepositoryError(w, r, err)
			return
		}
		out := backupRepositoryListResponse{Repositories: make([]backupRepositoryView, 0, len(repos))}
		for _, repo := range repos {
			out.Repositories = append(out.Repositories, viewOfRepository(repo))
		}
		httpx.WriteData(w, r, http.StatusOK, out)
	case http.MethodPost:
		_, def, ok := s.decodeBackupDefinition(w, r)
		if !ok {
			return
		}
		repo, err := mirror.CreateRepository(r.Context(), def, time.Now().UTC())
		if err != nil {
			s.writeBackupRepositoryError(w, r, err)
			return
		}
		httpx.WriteData(w, r, http.StatusCreated, backupRepositoryResponse{Repository: viewOfRepository(repo)})
	default:
		w.Header().Set("Allow", "GET, POST")
		httpx.WriteError(w, r, http.StatusMethodNotAllowed, httpx.CodeValidationError,
			"只支持 GET 与 POST")
	}
}

// handleBackupRepositoryByID 处理 /internal/v1/backup/repositories/:id。
func (s *Server) handleBackupRepositoryByID(w http.ResponseWriter, r *http.Request) {
	mirror := s.opts.BackupRepositories
	if mirror == nil {
		httpx.WriteError(w, r, http.StatusNotFound, httpx.CodeNotFound, "代理未启用备份仓库镜像")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/internal/v1/backup/repositories/")
	if id == "" {
		httpx.WriteError(w, r, http.StatusNotFound, httpx.CodeNotFound, "缺少仓库 id")
		return
	}
	if !agentstore.ValidRepositoryID(id) {
		// 形态不对的 id 不查库：这一层不猜“你说的是哪一条”。
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationError,
			"仓库 id 形态不合法（应为 rep_ 加 32 位小写十六进制）")
		return
	}
	switch r.Method {
	case http.MethodGet:
		repo, found, err := mirror.GetRepository(r.Context(), id)
		if err != nil {
			s.writeBackupRepositoryError(w, r, err)
			return
		}
		if !found {
			httpx.WriteError(w, r, http.StatusNotFound, httpx.CodeNotFound,
				fmt.Sprintf("代理里没有仓库 %s 的定义", id))
			return
		}
		httpx.WriteData(w, r, http.StatusOK, backupRepositoryResponse{Repository: viewOfRepository(repo)})
	case http.MethodPatch:
		req, def, ok := s.decodeBackupDefinition(w, r)
		if !ok {
			return
		}
		if req.Version < 1 {
			httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationError,
				"version 必须为正整数：修改仓库定义要带上当前版本")
			return
		}
		repo, err := mirror.UpdateRepository(r.Context(), id, def, req.Version, time.Now().UTC())
		if err != nil {
			s.writeBackupRepositoryError(w, r, err)
			return
		}
		httpx.WriteData(w, r, http.StatusOK, backupRepositoryResponse{Repository: viewOfRepository(repo)})
	case http.MethodDelete:
		// 幂等：删一条不存在的定义也回 204。语义上它只删定义，仓库数据与快照
		// 留在原地（契约第 12 节：删除策略不删除仓库）。
		if err := mirror.DeleteRepository(r.Context(), id); err != nil {
			s.writeBackupRepositoryError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "GET, PATCH, DELETE")
		httpx.WriteError(w, r, http.StatusMethodNotAllowed, httpx.CodeValidationError,
			"只支持 GET、PATCH 与 DELETE")
	}
}

// decodeBackupDefinition 读请求体并把定义规范化。
//
// 规范化是唯一一处形状校验：后端字段互斥、路径分量、秘密引用形态、指纹格式都在
// backup.Definition.Normalize 里，这里不重复实现也不放宽。
func (s *Server) decodeBackupDefinition(w http.ResponseWriter, r *http.Request) (backupRepositoryRequest, backup.Definition, bool) {
	if ct := r.Header.Get("Content-Type"); ct != "" && !jsonContentType(ct) {
		httpx.WriteError(w, r, http.StatusUnsupportedMediaType, httpx.CodeValidationError,
			"请求体必须是 application/json")
		return backupRepositoryRequest{}, backup.Definition{}, false
	}
	body, ok := readLimitedBody(w, r, maxBackupDefinitionBytes)
	if !ok {
		return backupRepositoryRequest{}, backup.Definition{}, false
	}
	var req backupRepositoryRequest
	if err := decodeStrictObject(body, &req); err != nil {
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationError, err.Error())
		return backupRepositoryRequest{}, backup.Definition{}, false
	}
	def, err := req.Definition.Normalize()
	if err != nil {
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationError, err.Error())
		return backupRepositoryRequest{}, backup.Definition{}, false
	}
	return req, def, true
}

// writeBackupRepositoryError 把存储层错误映射成契约错误码。
func (s *Server) writeBackupRepositoryError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, agentstore.ErrRepositoryNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, httpx.CodeNotFound, "代理里没有这条仓库定义")
	case errors.Is(err, agentstore.ErrRepositoryVersion):
		// 版本冲突是 409：调用方拿着旧版本改，得先读回来。
		httpx.WriteError(w, r, http.StatusConflict, httpx.CodeVersionConflict, err.Error())
	case errors.Is(err, agentstore.ErrRepositoryName):
		// 名字重复不是并发冲突，是这次请求本身不合法：回 400 让人改名字。
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationError, err.Error())
	default:
		s.opts.Logger.Error("备份仓库镜像操作失败", "err", err, "request_id", httpx.RequestID(r))
		httpx.WriteError(w, r, http.StatusInternalServerError, httpx.CodeInternalError, "代理内部错误")
	}
}

// viewOfRepository 输出一条定义的对外形状。
func viewOfRepository(repo backup.Repository) backupRepositoryView {
	return backupRepositoryView{
		ID:         repo.ID,
		Name:       repo.Name,
		Version:    repo.Version,
		Backend:    string(repo.Definition.Backend),
		Location:   repo.Definition.Location.String(repo.Definition.Backend),
		Definition: repo.Definition,
	}
}
