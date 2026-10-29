package agent

// 备份策略的代理镜像（P12.2）。
//
// 与仓库镜像同一条理由（见 backuprepo.go）：`backup.run` 的参数被契约冻结成
// `policy_id` + `version`，里面**没有**来源列表、没有一致性口径、没有应用名单。
// 策略只存在 Web 库里的话，代理就只能相信 Web 送来的任何字符串，而契约第 12 节
// 要的是「执行前展示停机影响」——影响是从策略里算出来的，策略必须由代理自己存。
//
// 这条路由只允许 Web 账号（对端 UID 校验在最外层 gate 里做，root 也不放行）。
// 有它，代理仍然每次都自己判定：根在不在、仓库存不存在、应用还登记着没有、restic 在不在。

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

// maxBackupPolicyBytes 是这条路由的请求体上限。
//
// 一条策略最多 32 个来源 + 32 个应用 id，正常不到 4 KiB；16 KiB 留足余量，
// 同时挡住把这条路由当成上传口的用法。
const maxBackupPolicyBytes = 16 << 10

// BackupPolicyMirror 是策略镜像的存储能力。
//
// 生产实现是 agentstore.BackupPolicyStore；测试用内存实现。
type BackupPolicyMirror interface {
	ListPolicies(ctx context.Context) ([]backup.PolicyRecord, error)
	GetPolicy(ctx context.Context, id string) (backup.PolicyRecord, bool, error)
	CreatePolicy(ctx context.Context, p backup.Policy, now time.Time) (backup.PolicyRecord, error)
	UpdatePolicy(ctx context.Context, id string, p backup.Policy, expectedVersion int64, now time.Time) (backup.PolicyRecord, error)
	DeletePolicy(ctx context.Context, id string) error
}

// backupPolicyView 是策略对外的形状。
//
// 它把规范化之后的策略整体摊开：来源、口径、应用名单、保留都在顶层，
// 界面不需要再拆一层 policy_json。
type backupPolicyView struct {
	ID              string           `json:"id"`
	Name            string           `json:"name"`
	Version         int64            `json:"version"`
	RepositoryID    string           `json:"repository_id"`
	Sources         []backup.Source  `json:"sources"`
	ConsistencyMode string           `json:"consistency_mode"`
	AppIDs          []string         `json:"app_ids,omitempty"`
	Retention       backup.Retention `json:"retention"`
}

// backupPolicyRequest 是 POST / PATCH 的请求体。
type backupPolicyRequest struct {
	Policy backup.Policy `json:"policy"`
	// Version 是 PATCH 的乐观并发令牌；POST 时忽略。
	Version int64 `json:"version"`
}

// backupPolicyResponse / backupPolicyListResponse 是响应体。
type backupPolicyResponse struct {
	Policy backupPolicyView `json:"policy"`
}

type backupPolicyListResponse struct {
	Policies []backupPolicyView `json:"policies"`
}

// handleBackupPolicies 处理 /internal/v1/backup/policies。
func (s *Server) handleBackupPolicies(w http.ResponseWriter, r *http.Request) {
	mirror := s.opts.BackupPolicies
	if mirror == nil {
		httpx.WriteError(w, r, http.StatusNotFound, httpx.CodeNotFound, "代理未启用备份策略镜像")
		return
	}
	switch r.Method {
	case http.MethodGet:
		records, err := mirror.ListPolicies(r.Context())
		if err != nil {
			s.writeBackupPolicyError(w, r, err)
			return
		}
		out := backupPolicyListResponse{Policies: make([]backupPolicyView, 0, len(records))}
		for _, rec := range records {
			out.Policies = append(out.Policies, viewOfPolicy(rec))
		}
		httpx.WriteData(w, r, http.StatusOK, out)
	case http.MethodPost:
		_, p, ok := s.decodeBackupPolicy(w, r)
		if !ok {
			return
		}
		rec, err := mirror.CreatePolicy(r.Context(), p, time.Now().UTC())
		if err != nil {
			s.writeBackupPolicyError(w, r, err)
			return
		}
		httpx.WriteData(w, r, http.StatusCreated, backupPolicyResponse{Policy: viewOfPolicy(rec)})
	default:
		w.Header().Set("Allow", "GET, POST")
		httpx.WriteError(w, r, http.StatusMethodNotAllowed, httpx.CodeValidationError, "只支持 GET 与 POST")
	}
}

// handleBackupPolicyByID 处理 /internal/v1/backup/policies/:id。
func (s *Server) handleBackupPolicyByID(w http.ResponseWriter, r *http.Request) {
	mirror := s.opts.BackupPolicies
	if mirror == nil {
		httpx.WriteError(w, r, http.StatusNotFound, httpx.CodeNotFound, "代理未启用备份策略镜像")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/internal/v1/backup/policies/")
	if id == "" {
		httpx.WriteError(w, r, http.StatusNotFound, httpx.CodeNotFound, "缺少策略 id")
		return
	}
	if !agentstore.ValidPolicyID(id) {
		// 形态不对的 id 不查库：这一层不猜“你说的是哪一条”。
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationError,
			"策略 id 形态不合法（应为 pol_ 加 32 位小写十六进制）")
		return
	}
	switch r.Method {
	case http.MethodGet:
		rec, found, err := mirror.GetPolicy(r.Context(), id)
		if err != nil {
			s.writeBackupPolicyError(w, r, err)
			return
		}
		if !found {
			httpx.WriteError(w, r, http.StatusNotFound, httpx.CodeNotFound,
				fmt.Sprintf("代理里没有策略 %s 的定义", id))
			return
		}
		httpx.WriteData(w, r, http.StatusOK, backupPolicyResponse{Policy: viewOfPolicy(rec)})
	case http.MethodPatch:
		req, p, ok := s.decodeBackupPolicy(w, r)
		if !ok {
			return
		}
		if req.Version < 1 {
			httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationError,
				"version 必须为正整数：修改策略要带上当前版本")
			return
		}
		rec, err := mirror.UpdatePolicy(r.Context(), id, p, req.Version, time.Now().UTC())
		if err != nil {
			s.writeBackupPolicyError(w, r, err)
			return
		}
		httpx.WriteData(w, r, http.StatusOK, backupPolicyResponse{Policy: viewOfPolicy(rec)})
	case http.MethodDelete:
		// 幂等：删一条不存在的策略也回 204。它只删策略，仓库与快照留在原地
		// （契约第 12 节：删除策略不删除仓库或快照）。
		if err := mirror.DeletePolicy(r.Context(), id); err != nil {
			s.writeBackupPolicyError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "GET, PATCH, DELETE")
		httpx.WriteError(w, r, http.StatusMethodNotAllowed, httpx.CodeValidationError,
			"只支持 GET、PATCH 与 DELETE")
	}
}

// decodeBackupPolicy 读请求体并规范化策略。
func (s *Server) decodeBackupPolicy(w http.ResponseWriter, r *http.Request) (backupPolicyRequest, backup.Policy, bool) {
	if ct := r.Header.Get("Content-Type"); ct != "" && !jsonContentType(ct) {
		httpx.WriteError(w, r, http.StatusUnsupportedMediaType, httpx.CodeValidationError,
			"请求体必须是 application/json")
		return backupPolicyRequest{}, backup.Policy{}, false
	}
	body, ok := readLimitedBody(w, r, maxBackupPolicyBytes)
	if !ok {
		return backupPolicyRequest{}, backup.Policy{}, false
	}
	var req backupPolicyRequest
	if err := decodeStrictObject(body, &req); err != nil {
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationError, err.Error())
		return backupPolicyRequest{}, backup.Policy{}, false
	}
	p, err := req.Policy.Normalize()
	if err != nil {
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationError, err.Error())
		return backupPolicyRequest{}, backup.Policy{}, false
	}
	return req, p, true
}

// writeBackupPolicyError 把存储层错误映射成契约错误码。
func (s *Server) writeBackupPolicyError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, agentstore.ErrPolicyNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, httpx.CodeNotFound, "代理里没有这条策略")
	case errors.Is(err, agentstore.ErrPolicyVersion):
		httpx.WriteError(w, r, http.StatusConflict, httpx.CodeVersionConflict, err.Error())
	case errors.Is(err, agentstore.ErrPolicyName):
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationError, err.Error())
	default:
		s.opts.Logger.Error("备份策略镜像操作失败", "err", err, "request_id", httpx.RequestID(r))
		httpx.WriteError(w, r, http.StatusInternalServerError, httpx.CodeInternalError, "代理内部错误")
	}
}

// viewOfPolicy 输出一条策略的对外形状。
func viewOfPolicy(rec backup.PolicyRecord) backupPolicyView {
	p := rec.Policy
	return backupPolicyView{
		ID:              rec.ID,
		Name:            rec.Name,
		Version:         rec.Version,
		RepositoryID:    p.RepositoryID,
		Sources:         p.Sources,
		ConsistencyMode: string(p.ConsistencyMode),
		AppIDs:          p.AppIDs,
		Retention:       p.Retention,
	}
}
