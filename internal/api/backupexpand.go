package api

// backup.run 参数的服务端展开（P12.2）。
//
// 与 proxy.apply 的展开同一条理由（见 preflightexpand.go 的开头）：调用方给出的
// 只是"哪条策略、哪一版"，剩下的由面板填。这里填的是 `apps`——策略说"备份前停
// app_A 与 app_B"，可代理拿到的不该是 app_A 这个**名字**，而是它此刻对应的运行
// 对象（哪个 systemd 单元、哪个容器 id）。
//
// 为什么由面板解析：
//
//	app_A 到底是什么   这是面板数据库（应用绑定）的知识。代理那边只有运行事实，
//	                   没有"这个应用绑在哪个单元上"这张表——而它也不该有：那会让
//	                   同一件事有两个登记处。
//	参数不能扩大授权   代理会核对每个 apps 条目都落在策略的 app_ids 之内（见
//	                   appsVsPolicy）。展开之后参数里出现的应用必然是策略列过的，
//	                   而且**一个都不少**：少一个就是停机不全的备份，代理会阻塞。
//
// 失败时给的是 404/409/422，不是把一份缺字段的参数交给代理去拒绝：
//
//	策略不在        404 NOT_FOUND——调用方拿的是一条已经不存在的策略。
//	应用解析不出    422 DEPENDENCY_BLOCKED——策略还在，但它依赖的应用绑定没了
//	                （删了，或者改成了 compose 部署）。这不是"参数不合法"，是
//	                "现在跑不了"。
//
// 版本不符**不在这里判**：代理的预检已经有一条 BACKUP_POLICY_VERSION_STALE 阻塞码，
// 而它的判决用的是自己那份定义（Web 拿到的镜像可能刚好是旧的）。在这里先拒一次，
// 等于给同一件事两个判决处，而且会让"预检给我一份带阻塞的计划"这条路径断掉。

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"

	"upanel/internal/agentclient"
	"upanel/internal/apps"
	"upanel/internal/backup"
	"upanel/internal/httpx"
)

// backupRunParamsRequest 是调用方交给 backup.run 的全部字段（CONTRACTS 第 5 节：
// `backup_policy_id、version`）。
//
// `apps` 不在其中：它是展开结果，不是参数。调用方自己给 apps 会被拒绝——那正是
// "参数不能扩大策略授权范围"这条规则要挡的东西。
type backupRunParamsRequest struct {
	PolicyID string `json:"backup_policy_id"`
	Version  int64  `json:"version"`
}

// backupRunAppParam 是展开出来的一个应用运行对象（代理的 backupAppParam 同形）。
type backupRunAppParam struct {
	AppID  string `json:"app_id"`
	Kind   string `json:"kind"`
	Object string `json:"object"`
}

// backupRunExpandedParams 是交给代理的完整参数。
type backupRunExpandedParams struct {
	PolicyID string              `json:"backup_policy_id"`
	Version  int64               `json:"version"`
	Apps     []backupRunAppParam `json:"apps"`
}

// expandBackupRun 把 {backup_policy_id, version} 展开成代理要的参数。
func (s *Server) expandBackupRun(c *gin.Context, params json.RawMessage) (json.RawMessage, bool) {
	if s.opts.Backup == nil {
		writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
			"备份定义不可用：未配置代理连接，无法按策略展开要停的应用")
		return nil, false
	}

	var req backupRunParamsRequest
	if len(params) > 0 {
		dec := json.NewDecoder(strings.NewReader(string(params)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeError(c, http.StatusBadRequest, httpx.CodeValidationError,
				"backup.run 的参数只有 backup_policy_id 与 version 两个："+err.Error())
			return nil, false
		}
	}
	policyID := strings.TrimSpace(req.PolicyID)
	if !backup.ValidPolicyID(policyID) {
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError,
			"backup_policy_id 必须是 pol_ 加 32 位小写十六进制")
		return nil, false
	}
	if req.Version < 1 {
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError,
			"version 必须为正整数：它就是这条策略的乐观并发令牌")
		return nil, false
	}

	pol, err := s.opts.Backup.GetBackupPolicy(c.Request.Context(), policyID)
	if err != nil {
		var unavailable *agentclient.UnavailableError
		var agentErr *agentclient.Error
		switch {
		case errors.As(err, &unavailable):
			writeError(c, http.StatusServiceUnavailable, httpx.CodeAgentUnavailable,
				"特权代理不可用，读不到策略定义，无法展开备份参数")
		case errors.As(err, &agentErr) && agentErr.Code == string(httpx.CodeNotFound):
			writeError(c, http.StatusNotFound, httpx.CodeNotFound,
				"策略不存在："+policyID+"（可能已被删除，刷新策略列表再试）")
		default:
			s.internalError(c, "读取备份策略失败", err)
		}
		return nil, false
	}

	expanded, ok := s.resolveBackupApps(c, pol, req.Version)
	if !ok {
		return nil, false
	}
	raw, err := json.Marshal(expanded)
	if err != nil {
		s.internalError(c, "预检展开失败", err)
		return nil, false
	}
	return raw, true
}

// resolveBackupApps 把策略的 app_ids 解析成运行对象。
//
// files_only 策略返回空数组（不是缺字段）：它没有要停的应用，而"没有"要说出来。
//
// 版本保留**调用方提交的那一版**：应用对象按当前镜像展开，但不能因此把旧预检
// 升级成新版本。调用方拿了旧版时，代理的预检会以 BACKUP_POLICY_VERSION_STALE
// 挡住——那一处判决用的是代理自己那份定义，比这里的镜像更权威。
func (s *Server) resolveBackupApps(c *gin.Context, pol agentclient.BackupPolicy, requestedVersion int64) (backupRunExpandedParams, bool) {
	out := backupRunExpandedParams{
		PolicyID: pol.ID,
		Version:  requestedVersion,
		Apps:     []backupRunAppParam{},
	}
	if len(pol.AppIDs) == 0 {
		return out, true
	}

	index, err := s.opts.Apps.BindingIndex(c.Request.Context())
	if err != nil {
		s.internalError(c, "读取应用绑定失败", err)
		return out, false
	}
	byID := make(map[string]apps.BindingRef, len(index))
	for _, ref := range index {
		byID[ref.AppID] = ref
	}

	var unresolved []string
	for _, appID := range pol.AppIDs {
		ref, found := byID[appID]
		if !found {
			unresolved = append(unresolved, appID+"（应用已不存在）")
			continue
		}
		kind, object, ok := backupRunObjectOf(ref)
		if !ok {
			unresolved = append(unresolved, appID+"（"+unresolvableReason(ref)+"）")
			continue
		}
		out.Apps = append(out.Apps, backupRunAppParam{AppID: appID, Kind: kind, Object: object})
	}
	if len(unresolved) > 0 {
		// 不降级：少停一个应用的备份会被打上 stop_managed_apps 的标签，而实际有一份
		// 文件是在应用写着的中间态下读的——那比失败更糟，因为它看起来是成功的。
		writeError(c, http.StatusUnprocessableEntity, httpx.CodeDependencyBlocked,
			"策略 "+pol.Name+" 要求备份前停掉的应用里有 "+strings.Join(unresolved, "、")+
				"：解析不出运行对象，这次备份不能按 stop_managed_apps 跑。"+
				"请先修好应用绑定，或把这条应用从策略里去掉")
		return out, false
	}
	return out, true
}

// backupRunObjectOf 把一条绑定翻成代理认识的运行对象。
//
// 只认 systemd 与 docker 两种：代理的停机通道就是这两条（服务单元、单个容器）。
func backupRunObjectOf(ref apps.BindingRef) (kind, object string, ok bool) {
	switch ref.Type {
	case apps.TypeSystemd:
		if strings.TrimSpace(ref.Unit) == "" {
			return "", "", false
		}
		return backup.AppKindSystemd, ref.Unit, true
	case apps.TypeDocker:
		if strings.TrimSpace(ref.ContainerID) == "" {
			return "", "", false
		}
		return backup.AppKindContainer, ref.ContainerID, true
	default:
		return "", "", false
	}
}

// unresolvableReason 说明一条绑定为什么不能当停机对象。
func unresolvableReason(ref apps.BindingRef) string {
	switch ref.Type {
	case apps.TypeCompose:
		// 一个 compose 部署是 N 个容器加一套编排语义（依赖顺序、项目标签）。按单个
		// 容器停会留下半个项目在跑，拉起时也不再是 compose 的那一套。宁可在这里
		// 说清楚，也不假装停了。
		return "compose 部署暂不支持作为停机对象，只能绑定单个服务或容器"
	case apps.TypeLink:
		return "只登记了入口、没有绑定可启停的对象"
	case apps.TypeSystemd:
		return "绑定里没有服务单元名"
	case apps.TypeDocker:
		return "绑定里没有完整的容器 id"
	default:
		return "绑定类型未知"
	}
}

// backup.restore 的调用方只给契约里的五个字段。覆盖恢复时，现有绑定模型不能证明
// 哪个应用“不依赖”目标目录，因此保守地展开全部可停止的受管理应用；有无法安全停止
// 的 compose/link 绑定时阻塞，绝不漏停后继续覆盖。
type backupRestoreParamsRequest struct {
	RepositoryID string `json:"repository_id"`
	SnapshotID   string `json:"snapshot_id"`
	TargetRootID string `json:"target_root_id"`
	RelativePath string `json:"relative_path"`
	Overwrite    bool   `json:"overwrite"`
}

type backupRestoreExpandedParams struct {
	backupRestoreParamsRequest
	Apps []backupRunAppParam `json:"apps"`
}

func (s *Server) expandBackupRestore(c *gin.Context, raw json.RawMessage) (json.RawMessage, bool) {
	var req backupRestoreParamsRequest
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(c, http.StatusBadRequest, httpx.CodeValidationError, "backup.restore 参数只接受契约字段；apps 由服务端展开："+err.Error())
		return nil, false
	}
	out := backupRestoreExpandedParams{backupRestoreParamsRequest: req, Apps: []backupRunAppParam{}}
	if req.Overwrite {
		index, err := s.opts.Apps.BindingIndex(c.Request.Context())
		if err != nil {
			s.internalError(c, "读取覆盖恢复依赖应用失败", err)
			return nil, false
		}
		sort.Slice(index, func(i, j int) bool { return index[i].AppID < index[j].AppID })
		var unresolved []string
		for _, ref := range index {
			kind, object, ok := backupRunObjectOf(ref)
			if !ok {
				unresolved = append(unresolved, ref.AppID+"（"+unresolvableReason(ref)+"）")
				continue
			}
			out.Apps = append(out.Apps, backupRunAppParam{AppID: ref.AppID, Kind: kind, Object: object})
		}
		if len(unresolved) > 0 {
			writeError(c, http.StatusUnprocessableEntity, httpx.CodeDependencyBlocked,
				"覆盖恢复必须停止依赖应用，但以下绑定无法证明已安全停止："+strings.Join(unresolved, "、"))
			return nil, false
		}
		if len(out.Apps) > backup.MaxPolicyApps {
			writeError(c, http.StatusUnprocessableEntity, httpx.CodeDependencyBlocked,
				"覆盖恢复需要停止的受管理应用超过 32 个，当前版本拒绝扩大单次停机范围")
			return nil, false
		}
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		s.internalError(c, "展开覆盖恢复参数失败", err)
		return nil, false
	}
	return encoded, true
}
