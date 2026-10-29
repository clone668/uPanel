package agent

// backup.run（P12.2）：按策略做一次备份。
//
// 与 backup.repository.initialize 同一个骨架：Decode 只认几个冻结字段，Plan 把
// 预检结论翻成计划，Execute 执行并把结果翻成 observed_state。三处与仓库动作不同：
//
//	参数   除 policy_id + version 之外还带**解析好的应用运行对象**。app_xxx 到底
//	       对应哪个 systemd 单元或容器 id，是 Web 侧数据库（应用绑定）的知识；
//	       这里沿用 apps.probe 的 target_id 那条路子：代理收到的是一份**断言**，
//	       它按自己的方式核实（形状、存在、保护规则），核实不过就不动。
//	       断言还必须落在策略的 app_ids 之内——参数不能扩大策略的授权范围。
//	结论   快照 id 建出来 ≠ 备份成立：internal/backup 会用 restic stats 读回来，
//	       没读回来就是 unknown，而不是"成功但没核对"。
//	影响   会停应用、会读面板库。这两件事都在 Plan 的 impacts 里说清楚，
//	       执行失败时停过的应用会被拉回来，拉不回来也算失败（见 internal/backup.Run）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"upanel/internal/backup"
	"upanel/internal/containers"
	"upanel/internal/services"
)

// backupRunPlanBudget 是备份预检的同步计算预算。
//
// 与仓库现状查询同量级：可能要读 restic 版本、拨一次 SFTP 探测目标位置，还会
// 逐个读应用现状（systemctl show / docker inspect）。SFTP 拨号上限 30 秒、
// 网络层失败还会再来一轮，最坏 60 秒；75 秒给 statvfs 留出余量，并仍小于 Web 的
// PreflightAgentTimeout（90 秒）。24.04 单线程 TCG 验收机上，Go 侧一次握手就经常
// 十几秒——原来的 45 秒会在第三条连接刚认证成功时耗尽，把可达仓库报成不可达。
const backupRunPlanBudget = 75 * time.Second

// backupSnapshotQueryBudget 是快照查询的预算。
//
// 比预检宽：`restic snapshots` 在大仓库上要读整条索引，不是一次 cat config。
const backupSnapshotQueryBudget = 90 * time.Second

// backupRunSource 是 backup.run 与快照查询需要的全部能力。
//
// 定义来源（策略、仓库）与执行端（预检、运行、查询）分开列：前者来自代理数据库，
// 后者来自 internal/backup 的 Ops。生产装配用 backup.Module 把两者合成一个值。
type backupRunSource interface {
	GetPolicy(ctx context.Context, id string) (backup.PolicyRecord, bool, error)
	GetRepository(ctx context.Context, id string) (backup.Repository, bool, error)
	AssessRun(ctx context.Context, req backup.RunAssessRequest) (backup.RunAssessment, error)
	Run(ctx context.Context, req backup.RunRequest) (backup.RunResult, error)
	ListSnapshots(ctx context.Context, req backup.SnapshotRequest) ([]backup.Snapshot, error)
	GetSnapshot(ctx context.Context, req backup.SnapshotRequest) (backup.Snapshot, error)
}

// backupAppParam 是一个解析好的应用运行对象（Web 侧的断言）。
type backupAppParam struct {
	AppID  string `json:"app_id"`
	Kind   string `json:"kind"`
	Object string `json:"object"`
}

// backupRunParams 是 backup.run 的参数。
//
// 字段名以 CONTRACTS 第 5 节的参数表为准：那里写的是 `backup_policy_id、version`。
// 这张表里所有以策略为目标的行都用这个名字（backup.prune 也是），不缩写成 policy_id——
// 预检计划里的 normalized_params 会被冻结、被人抄进工单，名字必须是契约里那个。
// `apps` 是 Web 展开出来的第 3 个字段（见 expandBackupRun），不在那张表里，
// 因为它是**服务端展开**的结果而不是调用方给的参数（proxy.apply 的 definition 同理）。
type backupRunParams struct {
	PolicyID string           `json:"backup_policy_id"`
	Version  int64            `json:"version"`
	Apps     []backupAppParam `json:"apps"`
}

// normalizeApps 校验并规范化应用列表（去重，保持提交顺序）。
func normalizeApps(in []backupAppParam) ([]backupAppParam, error) {
	if len(in) > backup.MaxPolicyApps {
		return nil, fmt.Errorf("%w: 一次最多 %d 个应用，收到 %d",
			ErrParamsInvalid, backup.MaxPolicyApps, len(in))
	}
	seen := make(map[string]bool, len(in))
	out := make([]backupAppParam, 0, len(in))
	for _, a := range in {
		a.AppID = strings.TrimSpace(a.AppID)
		a.Kind = strings.ToLower(strings.TrimSpace(a.Kind))
		a.Object = strings.TrimSpace(a.Object)
		if !backup.ValidAppID(a.AppID) {
			return nil, fmt.Errorf("%w: app_id 形状不合法（%q）", ErrParamsInvalid, a.AppID)
		}
		switch a.Kind {
		case backup.AppKindSystemd:
			if err := services.ValidateUnitName(a.Object); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrParamsInvalid, err)
			}
		case backup.AppKindContainer:
			if !containers.ValidContainerID(a.Object) {
				return nil, fmt.Errorf("%w: 容器 id 必须是完整的 64 位十六进制（收到 %q）", ErrParamsInvalid, a.Object)
			}
		default:
			return nil, fmt.Errorf("%w: kind 只能是 systemd 或 container，收到 %q", ErrParamsInvalid, a.Kind)
		}
		key := a.Kind + "|" + a.Object
		if seen[key] {
			return nil, fmt.Errorf("%w: 同一个运行对象 %s 出现了两次", ErrParamsInvalid, a.Object)
		}
		seen[key] = true
		out = append(out, a)
	}
	return out, nil
}

// RegisterBackupRunAction 登记 backup.run。
func RegisterBackupRunAction(reg *Registry, src backupRunSource) error {
	if src == nil {
		return errors.New("agent: 备份运行没有实现端")
	}
	return reg.RegisterAction(Action{
		Name: backup.ActionRun,
		// 预检会拨 SFTP、读应用现状，给它一条自己的秒表；执行完全在后台跑。
		PlanBudget: backupRunPlanBudget,
		Decode: func(raw json.RawMessage) (any, error) {
			var p backupRunParams
			if err := DecodeParams(raw, &p); err != nil {
				return nil, err
			}
			p.PolicyID = strings.TrimSpace(p.PolicyID)
			if p.PolicyID == "" {
				return nil, fmt.Errorf("%w: backup_policy_id 不能为空", ErrParamsInvalid)
			}
			if p.Version < 1 {
				return nil, fmt.Errorf("%w: version 必须为正整数（策略记录的乐观并发令牌）", ErrParamsInvalid)
			}
			apps, err := normalizeApps(p.Apps)
			if err != nil {
				return nil, err
			}
			p.Apps = apps
			return p, nil
		},
		Plan:    newBackupRunPlan(src),
		Execute: newBackupRunExecute(src),
		// 策略：备份要读仓库口令、要可能停应用，属于"要密码再确认一次"的动作。
		// 不要短语：备份不是破坏性的（不删任何东西）。可取消：restic 支持在两次
		// 写入之间被打断，取消后已经写进去的块会被下一次备份复用，不会留下坏快照。
		Policy: ActionPolicy{RequiresReauth: true, Cancellable: true},
	})
}

// newBackupRunPlan 生成预检。
func newBackupRunPlan(src backupRunSource) func(context.Context, PlanInput) (Plan, error) {
	return func(ctx context.Context, in PlanInput) (Plan, error) {
		params, ok := in.Params.(backupRunParams)
		if !ok {
			return Plan{}, fmt.Errorf("参数类型不匹配: %T", in.Params)
		}
		rec, found, err := src.GetPolicy(ctx, params.PolicyID)
		if err != nil {
			return Plan{}, err
		}
		if !found {
			// 找不到策略不是 500：这个 id 在这台机器上没有登记。
			return Plan{
				NormalizedParams: params,
				Target: Target{
					DisplayName:  params.PolicyID,
					StateVersion: fmt.Sprintf("backup-policy:%s", params.PolicyID),
				},
				Allowed: false,
				Blockers: []Notice{{
					Code: "BACKUP_POLICY_NOT_FOUND",
					Message: fmt.Sprintf("代理里没有策略 %s 的定义：先通过 /internal/v1/backup/policies 登记",
						params.PolicyID),
				}},
				Locks: []string{backup.PolicyLockName(params.PolicyID)},
			}, nil
		}
		if rec.Version != params.Version {
			return Plan{
				NormalizedParams: params,
				Target: Target{
					DisplayName:  rec.Name,
					StateVersion: fmt.Sprintf("backup-policy:%s:v%d", rec.ID, rec.Version),
				},
				Allowed: false,
				Blockers: []Notice{{
					Code: "BACKUP_POLICY_VERSION_STALE",
					Message: fmt.Sprintf("策略已经变成版本 %d（请求的是 %d）：重新预检一次",
						rec.Version, params.Version),
				}},
				Locks: []string{backup.PolicyLockName(params.PolicyID)},
			}, nil
		}
		repo, found, err := src.GetRepository(ctx, rec.Policy.RepositoryID)
		if err != nil {
			return Plan{}, err
		}
		if !found {
			return Plan{
				NormalizedParams: params,
				Target: Target{
					DisplayName:  rec.Name,
					StateVersion: fmt.Sprintf("backup-policy:%s:v%d", rec.ID, rec.Version),
				},
				Allowed: false,
				Blockers: []Notice{{
					Code: backup.CodeRepositoryMissing,
					Message: fmt.Sprintf("策略指向的仓库 %s 不在代理里（可能已删除）：先重新登记仓库，或把策略改到别的仓库",
						rec.Policy.RepositoryID),
				}},
				Locks: []string{backup.PolicyLockName(params.PolicyID)},
			}, nil
		}
		if blockers := appsVsPolicy(params.Apps, rec.Policy); len(blockers) > 0 {
			return Plan{
				NormalizedParams: params,
				Target: Target{
					DisplayName:  rec.Name,
					StateVersion: fmt.Sprintf("backup-policy:%s:v%d", rec.ID, rec.Version),
				},
				Allowed:  false,
				Blockers: blockers,
				Locks:    []string{backup.PolicyLockName(params.PolicyID)},
			}, nil
		}
		assessment, err := src.AssessRun(ctx, backup.RunAssessRequest{
			PolicyID:   rec.ID,
			Policy:     rec.Policy,
			Repository: repo,
			Apps:       appTargetsOf(params.Apps),
		})
		if err != nil {
			return Plan{}, err
		}
		return planFromRunAssessment(params, assessment), nil
	}
}

// appsVsPolicy 双向核对参数里的应用与策略的 app_ids。
//
// 两个方向都要查，因为它们会导致两种不同的坏事：
//
//	多给了   参数不能扩大授权范围：策略说"停 A 和 B"，请求不能顺手把 C 也停了。
//	少给了   stop_managed_apps 策略的含义是"备份前把这条策略管的**每一个**应用都停掉"。
//	         少解析出一个，快照照样能建成，但它会被打上 consistency=stop_managed_apps 的
//	         标签而实际有一份文件是在应用写着的中间态下读的——那是一条骗人的快照。
//	         Web 侧解析失败（应用被删、绑定改成了 compose 部署）必须**阻塞**，不能降级。
//
// files_only 策略没有 app_ids，也就没有"少解析了一个"这回事；两个循环都自然跳过。
func appsVsPolicy(apps []backupAppParam, pol backup.Policy) []Notice {
	if len(apps) == 0 && len(pol.AppIDs) == 0 {
		return nil
	}
	allowed := make(map[string]bool, len(pol.AppIDs))
	for _, id := range pol.AppIDs {
		allowed[id] = true
	}
	provided := make(map[string]bool, len(apps))
	var out []Notice
	for _, a := range apps {
		provided[a.AppID] = true
		if !allowed[a.AppID] {
			out = append(out, Notice{
				Code:    backup.CodePolicyAppsUnavailable,
				Message: fmt.Sprintf("应用 %s 不在这条策略的 app_ids 里：参数不能扩大策略授权范围", a.AppID),
			})
		}
	}
	for _, id := range pol.AppIDs {
		if provided[id] {
			continue
		}
		out = append(out, Notice{
			Code: backup.CodePolicyAppsUnavailable,
			Message: fmt.Sprintf("策略要求备份前停掉应用 %s，但请求里没有它的运行对象："+
				"这条快照做不到 %s，不能降级成停机不全的备份（先修好应用绑定或改策略）",
				id, backup.ConsistencyStopManagedApps),
		})
	}
	return out
}

// appTargetsOf 把参数翻成运行对象。
func appTargetsOf(apps []backupAppParam) []backup.AppTarget {
	out := make([]backup.AppTarget, 0, len(apps))
	for _, a := range apps {
		out = append(out, backup.AppTarget{
			AppID:  a.AppID,
			Kind:   a.Kind,
			Object: a.Object,
			Label:  a.AppID,
		})
	}
	return out
}

// planFromRunAssessment 把 internal/backup 的结论逐字段翻成 Plan。
func planFromRunAssessment(params backupRunParams, a backup.RunAssessment) Plan {
	blockers := make([]Notice, 0, len(a.Blockers))
	for _, b := range a.Blockers {
		blockers = append(blockers, Notice{Code: b.Code, Message: b.Message})
	}
	warnings := make([]Notice, 0, len(a.Warnings))
	for _, w := range a.Warnings {
		warnings = append(warnings, Notice{Code: w.Code, Message: w.Message})
	}
	impacts := make([]Impact, 0, len(a.Impacts))
	for _, i := range a.Impacts {
		impacts = append(impacts, Impact{Kind: i.Kind, Description: i.Description})
	}
	locks := a.Locks
	if len(locks) == 0 {
		locks = []string{backup.PolicyLockName(params.PolicyID)}
	}
	return Plan{
		NormalizedParams: params,
		Target: Target{
			DisplayName:         a.Target.DisplayName,
			IdentityFingerprint: a.Target.IdentityFingerprint,
			StateVersion:        a.Target.StateVersion,
		},
		Allowed:  a.Allowed,
		Blockers: blockers,
		Warnings: warnings,
		Impacts:  impacts,
		Locks:    locks,
	}
}

// newBackupRunExecute 生成执行器。
func newBackupRunExecute(src backupRunSource) func(context.Context, ExecuteInput) (Result, error) {
	return func(ctx context.Context, in ExecuteInput) (Result, error) {
		params, ok := in.Params.(backupRunParams)
		if !ok {
			return Result{}, fmt.Errorf("参数类型不匹配: %T", in.Params)
		}
		// 执行前复核：策略版本、仓库还在不在、应用是否仍在策略范围内。
		rec, found, err := src.GetPolicy(ctx, params.PolicyID)
		if err != nil {
			return Result{}, err
		}
		if !found {
			return Result{}, fmt.Errorf("BACKUP_POLICY_NOT_FOUND：策略 %s 在执行前不见了", params.PolicyID)
		}
		if rec.Version != params.Version {
			return Result{}, fmt.Errorf("BACKUP_POLICY_VERSION_STALE：策略在预检之后被改过（版本 %d → %d）",
				params.Version, rec.Version)
		}
		repo, found, err := src.GetRepository(ctx, rec.Policy.RepositoryID)
		if err != nil {
			return Result{}, err
		}
		if !found {
			return Result{}, fmt.Errorf("%s：策略指向的仓库 %s 在执行前不见了",
				backup.CodeRepositoryMissing, rec.Policy.RepositoryID)
		}
		if notices := appsVsPolicy(params.Apps, rec.Policy); len(notices) > 0 {
			msgs := make([]string, 0, len(notices))
			for _, n := range notices {
				msgs = append(msgs, n.Message)
			}
			return Result{}, fmt.Errorf("%s：%s", backup.CodePolicyAppsUnavailable, strings.Join(msgs, "；"))
		}

		out, err := src.Run(ctx, backup.RunRequest{
			PolicyID:    rec.ID,
			Policy:      rec.Policy,
			Repository:  repo,
			OperationID: in.OperationID,
			Apps:        appTargetsOf(params.Apps),
			Progress:    in.Progress,
			Cancel:      in.Cancel,
		})
		// 快照可能建成了，但读回来核对失败：这是 unknown，不是失败——报失败会让人
		// 以为可以放心重跑，而重跑会在仓库里留下第二份内容相同的快照。
		if err != nil && out.SnapshotID != "" {
			return Result{
				Outcome:       OutcomeUnknown,
				ObservedState: runObserved(rec, out, params),
				RecoveryHint: "restic 已经写出快照，但读回核对失败：" + out.VerifyError +
					"；先用 backup.snapshots 确认这条快照在不在，再决定是否重跑（重跑不会覆盖它，只会再写一份）",
				Data: map[string]any{
					"policy_id":   rec.ID,
					"snapshot_id": out.SnapshotID,
					"verified":    false,
				},
			}, nil
		}
		if err != nil {
			if errors.Is(err, ErrCanceled) || errors.Is(err, context.Canceled) ||
				errors.Is(err, backup.ErrCanceled) {
				return Result{}, ErrCanceled
			}
			return Result{}, backupFailure{err: err}
		}
		return Result{
			Outcome:       OutcomeVerified,
			VerifiedAt:    out.FinishedAt,
			ObservedState: runObserved(rec, out, params),
			RecoveryHint:  "快照已建好并读回核对。备份成功不等于恢复已验证：恢复验证要单独做一次（P12.3 的 backup.verify）",
			Data: map[string]any{
				"policy_id":     rec.ID,
				"repository_id": repo.ID,
				"snapshot_id":   out.SnapshotID,
				"verified":      out.Verified,
			},
		}, nil
	}
}

// runObserved 把一次运行的结果翻成 observed_state。
//
// 停过哪些应用、拉起成没成，都在这份状态里：一份"备份成功"的报告如果对机器状态
// 只字不提，管理员就得自己去 systemctl 里逐个确认。
func runObserved(rec backup.PolicyRecord, out backup.RunResult, params backupRunParams) map[string]any {
	observed := map[string]any{
		"policy_id":        rec.ID,
		"policy_name":      rec.Name,
		"repository_id":    rec.Policy.RepositoryID,
		"consistency_mode": string(rec.Policy.ConsistencyMode),
		"started_at":       out.StartedAt.Format(time.RFC3339),
		"duration_ms":      out.Duration.Milliseconds(),
		"apps_restored":    out.AppsRestored,
	}
	if out.SnapshotID != "" {
		observed["snapshot_id"] = out.SnapshotID
	}
	if out.Verified {
		observed["verified_by"] = "restic stats"
	}
	if out.VerifyError != "" {
		observed["verify_error"] = out.VerifyError
	}
	if len(out.Warnings) > 0 {
		observed["warnings"] = out.Warnings
	}
	if summary := out.Summary; summary.SnapshotID != "" {
		observed["files_new"] = summary.FilesNew
		observed["files_changed"] = summary.FilesChanged
		observed["files_unmodified"] = summary.FilesUnmodified
		observed["data_added"] = summary.DataAdded
		observed["total_files_processed"] = summary.TotalFiles
		observed["restic_duration_ms"] = summary.Duration.Milliseconds()
	}
	sources := make([]map[string]any, 0, len(out.Sources))
	for _, sp := range out.Sources {
		entry := map[string]any{"source": sp.Source.String(), "kind": string(sp.Source.NormalizedKind())}
		if sp.AbsolutePath != "" {
			entry["path"] = sp.AbsolutePath
		}
		entry["exists"] = sp.Exists
		sources = append(sources, entry)
	}
	observed["sources"] = sources
	if out.PanelCopy != nil {
		observed["panel_database"] = map[string]any{
			"size_bytes":     out.PanelCopy.SizeBytes,
			"schema_version": out.PanelCopy.SourceSchemaVersion,
			"integrity":      out.PanelCopy.Integrity,
			"copied_at":      out.PanelCopy.CopiedAt.Format(time.RFC3339),
		}
	}
	if len(out.Apps) > 0 {
		actions := make([]map[string]any, 0, len(out.Apps))
		for _, act := range out.Apps {
			entry := map[string]any{
				"app_id":    act.Target.AppID,
				"kind":      act.Target.Kind,
				"object":    act.Target.Object,
				"operation": act.Operation,
				"done":      act.Done,
				"verified":  act.Verified,
			}
			if act.Note != "" {
				entry["note"] = act.Note
			}
			actions = append(actions, entry)
		}
		observed["apps"] = actions
	}
	return observed
}

// 快照查询的参数。
type backupSnapshotsParams struct {
	RepositoryID string `json:"repository_id"`
	PolicyID     string `json:"policy_id"`
}

type backupSnapshotParams struct {
	RepositoryID string `json:"repository_id"`
	SnapshotID   string `json:"snapshot_id"`
}

// RegisterBackupSnapshotQueries 登记快照的两条只读查询。
//
// 一条列表、一条详情：列表不含大小（每条都要单独跑一次 restic stats，那是分钟级的
// 代价），详情才跑 stats。这个不对称必须在文档里写明，否则"列表里没有大小"会被
// 当成"读不到大小"。
func RegisterBackupSnapshotQueries(reg *Registry, src backupRunSource) error {
	if src == nil {
		return nil
	}
	if err := reg.RegisterQuery(Query{
		Name: backup.QuerySnapshots,
		Decode: func(raw json.RawMessage) (any, error) {
			var p backupSnapshotsParams
			if err := DecodeParams(raw, &p); err != nil {
				return nil, err
			}
			p.RepositoryID = strings.TrimSpace(p.RepositoryID)
			p.PolicyID = strings.TrimSpace(p.PolicyID)
			if p.RepositoryID == "" {
				return nil, fmt.Errorf("%w: repository_id 不能为空", ErrParamsInvalid)
			}
			if p.PolicyID != "" && !strings.HasPrefix(p.PolicyID, "pol_") {
				return nil, fmt.Errorf("%w: policy_id 形状不合法（%q）", ErrParamsInvalid, p.PolicyID)
			}
			return p, nil
		},
		Run: func(ctx context.Context, params any) (any, error) {
			p, ok := params.(backupSnapshotsParams)
			if !ok {
				return nil, fmt.Errorf("%w: 参数类型不匹配: %T", ErrParamsInvalid, params)
			}
			ctx, cancel := context.WithTimeout(ctx, backupSnapshotQueryBudget)
			defer cancel()
			repo, found, err := src.GetRepository(ctx, p.RepositoryID)
			if err != nil {
				return nil, err
			}
			if !found {
				// 没登记就是没登记：查询回一个明确的状态，不回 404——404 会被读成
				// "这条查询不存在"，而实际情况是"这个仓库不在这台机器上"。
				return map[string]any{
					"repository_id": p.RepositoryID,
					"registered":    false,
					"count":         0,
					"snapshots":     []map[string]any{},
					"checked_at":    time.Now().UTC().Format(time.RFC3339),
				}, nil
			}
			snaps, err := src.ListSnapshots(ctx, backup.SnapshotRequest{
				RepositoryID: repo.ID,
				Definition:   repo.Definition,
				PolicyID:     p.PolicyID,
			})
			if err != nil {
				return nil, err
			}
			items := make([]map[string]any, 0, len(snaps))
			for _, s := range snaps {
				items = append(items, snapshotView(s, false))
			}
			return map[string]any{
				"repository_id": repo.ID,
				"registered":    true,
				"policy_id":     p.PolicyID,
				"count":         len(items),
				"snapshots":     items,
				"checked_at":    time.Now().UTC().Format(time.RFC3339),
				// 列表不含 size：每条都要单独跑一次 restic stats。
				"size_included": false,
			}, nil
		},
	}); err != nil {
		return fmt.Errorf("代理登记 %s 失败: %w", backup.QuerySnapshots, err)
	}

	return reg.RegisterQuery(Query{
		Name: backup.QuerySnapshot,
		Decode: func(raw json.RawMessage) (any, error) {
			var p backupSnapshotParams
			if err := DecodeParams(raw, &p); err != nil {
				return nil, err
			}
			p.RepositoryID = strings.TrimSpace(p.RepositoryID)
			p.SnapshotID = strings.TrimSpace(p.SnapshotID)
			if p.RepositoryID == "" {
				return nil, fmt.Errorf("%w: repository_id 不能为空", ErrParamsInvalid)
			}
			if len(p.SnapshotID) != 64 {
				return nil, fmt.Errorf("%w: snapshot_id 必须是完整的 64 位十六进制", ErrParamsInvalid)
			}
			return p, nil
		},
		Run: func(ctx context.Context, params any) (any, error) {
			p, ok := params.(backupSnapshotParams)
			if !ok {
				return nil, fmt.Errorf("%w: 参数类型不匹配: %T", ErrParamsInvalid, params)
			}
			ctx, cancel := context.WithTimeout(ctx, backupSnapshotQueryBudget)
			defer cancel()
			repo, found, err := src.GetRepository(ctx, p.RepositoryID)
			if err != nil {
				return nil, err
			}
			if !found {
				return map[string]any{
					"repository_id": p.RepositoryID,
					"snapshot_id":   p.SnapshotID,
					"registered":    false,
					"found":         false,
					"checked_at":    time.Now().UTC().Format(time.RFC3339),
				}, nil
			}
			snap, err := src.GetSnapshot(ctx, backup.SnapshotRequest{
				RepositoryID: repo.ID,
				Definition:   repo.Definition,
				SnapshotID:   p.SnapshotID,
			})
			if err != nil {
				if errors.Is(err, backup.ErrSnapshotNotFound) {
					return map[string]any{
						"repository_id": repo.ID,
						"snapshot_id":   p.SnapshotID,
						"registered":    true,
						"found":         false,
						"checked_at":    time.Now().UTC().Format(time.RFC3339),
					}, nil
				}
				return nil, err
			}
			view := snapshotView(snap, true)
			view["registered"] = true
			view["found"] = true
			view["checked_at"] = time.Now().UTC().Format(time.RFC3339)
			return view, nil
		},
	})
}

// snapshotView 把一条快照翻成查询结果。
//
// size/file_count 只在 withSize 为 true 时出现：false 时**不写 0**，因为没有读数
// 与读数是 0 是两件不同的事。
func snapshotView(s backup.Snapshot, withSize bool) map[string]any {
	out := map[string]any{
		"snapshot_id": s.ID,
		"short_id":    s.ShortID,
		"time":        s.Time.Format(time.RFC3339),
		"paths":       s.Paths,
		"tags":        s.Tags,
	}
	if s.Hostname != "" {
		out["hostname"] = s.Hostname
	}
	if s.PolicyID != "" {
		out["policy_id"] = s.PolicyID
	}
	if s.ConsistencyMode != "" {
		out["consistency_mode"] = s.ConsistencyMode
	}
	if withSize {
		out["size_bytes"] = s.SizeBytes
		out["file_count"] = s.FileCount
	}
	// 恢复验证记录：P12.3 之前恒为 null。
	out["restore_verified_at"] = nil
	if s.RestoreVerifiedAt != nil {
		out["restore_verified_at"] = s.RestoreVerifiedAt.Format(time.RFC3339)
	}
	return out
}
