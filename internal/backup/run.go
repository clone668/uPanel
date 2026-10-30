package backup

// 备份的执行与快照查询（P12.2；CONTRACTS.md 第 12 节）。
//
// 这个文件回答两个问题：
//
//	现在能不能备份？    AssessRun：仓库是否已初始化、来源还在不在、应用能不能停、
//	                   面板库能不能做一致副本。全是只读探测。
//	刚才备份成了什么？  Run：停应用（按口径）→ 面板库在线副本 → restic backup →
//	                   读回快照核对 → 按原状态拉起应用 → 清掉临时副本。
//
// 三处刻意的不对称：
//
//	退出码不是证据   restic 退出码 0 只说明命令跑完了；`--json` 里的 snapshot id
//	                 才是"快照真的存在"。Run 结尾会用 `restic stats <id>` 再读一次。
//	停止必须可回滚   停应用是"我们改变了主机状态"里最重的一种。执行失败、被取消、
//	                 乃至备份成功但拉起失败，结果里都必须逐条写清楚谁被停了、谁没起来。
//	临时副本必须清   面板库副本里有口令哈希与会话；它只在这次运行期间存在。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// 动作/查询默认的时间上限。
const (
	// RunTimeout 是一次 restic backup 的上限。
	//
	// 6 小时不是拍的：本地仓库上几十 GB 的首次全量在慢盘上就是这个量级。
	// 更长的上限没有意义——那说明源选错了，而不是 restic 需要更多时间。
	RunTimeout = 6 * time.Hour
	// SnapshotQueryTimeout 是一次快照列表/统计的上限（与只读查询同量级）。
	SnapshotQueryTimeout = 2 * time.Minute
	// SnapshotListLimit 是一次最多列多少条快照。
	SnapshotListLimit = 500
)

// 阻塞/告警码（P12.2 新增）。稳定字符串，界面按它分类。
const (
	// CodeSourceMissing 表示策略里的某个来源在这台机器上不存在。
	CodeSourceMissing = "BACKUP_SOURCE_MISSING"
	// CodeSourceBlocked 表示来源落在不能被备份的位置（不存在/不是目录/无法解析）。
	CodeSourceBlocked = "BACKUP_SOURCE_BLOCKED"
	// CodeRepositoryMissing 表示策略引用的仓库定义不在代理镜像里。
	CodeRepositoryMissing = "BACKUP_REPOSITORY_MISSING"
	// CodeRepositoryNotReady 表示目标位置还不是一个可用的仓库。
	CodeRepositoryNotReady = "BACKUP_REPOSITORY_NOT_READY"
	// CodePolicyAppsUnavailable 表示策略要停的应用里有停不了的。
	CodePolicyAppsUnavailable = "BACKUP_APP_UNAVAILABLE"
	// CodePanelDatabaseMissing 表示策略要备份面板库，但面板库文件读不到。
	CodePanelDatabaseMissing = "BACKUP_PANEL_DATABASE_MISSING"
	// CodeRepositoryBusy 表示同一个仓库上已经有一次备份在跑。
	CodeRepositoryBusy = "BACKUP_REPOSITORY_BUSY"
)

// 快照标签。它们是我们自己打的，读回来时用来认"这份备份是谁的、按什么口径做的"。
const (
	// tagUPanel 是所有快照都带的标签。
	tagUPanel = "upanel"
	// tagPolicyPrefix 是策略标签前缀（完整形式 upanel:policy:<pol_id>）。
	tagPolicyPrefix = "upanel:policy:"
	// tagConsistencyPrefix 是一致性口径标签前缀（完整形式 upanel:consistency:<mode>）。
	tagConsistencyPrefix = "upanel:consistency:"
)

// Snapshot 是一条对外展示的快照。
type Snapshot struct {
	ID              string    `json:"id"`
	ShortID         string    `json:"short_id"`
	Time            time.Time `json:"time"`
	Hostname        string    `json:"hostname,omitempty"`
	Paths           []string  `json:"paths"`
	Tags            []string  `json:"tags,omitempty"`
	PolicyID        string    `json:"policy_id,omitempty"`
	ConsistencyMode string    `json:"consistency_mode,omitempty"`
	// SizeBytes / FileCount 只有详情里有：restic 的 stats 要单独跑一次，
	// 列表里为每条快照跑一次会把"看一眼列表"变成几分钟的活。
	SizeBytes int64 `json:"size_bytes,omitempty"`
	FileCount int64 `json:"file_count,omitempty"`
	// RestoreVerifiedAt 是"这份快照被恢复验证过"的时间。
	//
	// P12.3 之前恒为 null：备份成功不等于恢复已验证（PLAN 第 17 节要求分别展示
	// 最近备份与最近恢复验证时间），所以这里不填一个看起来像验证过的值。
	RestoreVerifiedAt *time.Time `json:"restore_verified_at"`
}

// SnapshotRequest 是一次快照查询的输入。
type SnapshotRequest struct {
	RepositoryID string
	Definition   Definition
	// PolicyID 非空时只列这条策略产生的快照（按标签过滤）。
	PolicyID string
	// SnapshotID 非空时只读这一条。
	SnapshotID string
}

// ListSnapshots 列出仓库里的快照（按时间倒序）。
func (o *Ops) ListSnapshots(ctx context.Context, req SnapshotRequest) ([]Snapshot, error) {
	return o.listSnapshots(ctx, req, SnapshotListLimit)
}

// listSnapshots 的 limit 为 0 时返回完整集合；保留策略不能使用展示页截断的数据。
func (o *Ops) listSnapshots(ctx context.Context, req SnapshotRequest, limit int) ([]Snapshot, error) {
	def, err := req.Definition.Normalize()
	if err != nil {
		return nil, err
	}
	inv, cleanup, err := o.readInvocation(ctx, def)
	if err != nil {
		return nil, err
	}
	if cleanup != nil {
		defer cleanup()
	}
	inv.Timeout = SnapshotQueryTimeout
	var tags []string
	if req.PolicyID != "" {
		tags = []string{tagPolicyPrefix + req.PolicyID}
	}
	raw, err := o.opts.Restic.Snapshots(ctx, inv, tags)
	if err != nil {
		return nil, err
	}
	out := make([]Snapshot, 0, len(raw))
	for _, s := range raw {
		snap, err := snapshotOf(s)
		if err != nil {
			// 一条坏记录不该让整页变成错误：跳过它，但也不假装它不存在。
			// 这里选择跳过，是因为列表的用途是"看有哪些备份"，一条形状异常的记录
			// 已经由 id 校验挡在详情之外。
			continue
		}
		out = append(out, snap)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Time.Equal(out[j].Time) {
			return out[i].ID > out[j].ID
		}
		return out[i].Time.After(out[j].Time)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	for i := range out {
		if err := o.mergeRestoreVerification(ctx, req.RepositoryID, &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// GetSnapshot 读一条快照的详情（含 restic stats 的大小与文件数）。
func (o *Ops) GetSnapshot(ctx context.Context, req SnapshotRequest) (Snapshot, error) {
	if !resticIDPattern.MatchString(req.SnapshotID) {
		return Snapshot{}, fmt.Errorf("backup: 快照 id 形状不合法（%q）", req.SnapshotID)
	}
	def, err := req.Definition.Normalize()
	if err != nil {
		return Snapshot{}, err
	}
	inv, cleanup, err := o.readInvocation(ctx, def)
	if err != nil {
		return Snapshot{}, err
	}
	if cleanup != nil {
		defer cleanup()
	}
	inv.Timeout = SnapshotQueryTimeout

	raw, err := o.opts.Restic.Snapshots(ctx, inv, nil)
	if err != nil {
		return Snapshot{}, err
	}
	var found *ResticSnapshot
	for i := range raw {
		if raw[i].ID == req.SnapshotID || raw[i].ShortID == req.SnapshotID {
			found = &raw[i]
			break
		}
	}
	if found == nil {
		return Snapshot{}, ErrSnapshotNotFound
	}
	snap, err := snapshotOf(*found)
	if err != nil {
		return Snapshot{}, err
	}
	stats, err := o.opts.Restic.SnapshotStat(ctx, inv, snap.ID)
	if err != nil {
		return Snapshot{}, err
	}
	snap.SizeBytes = stats.TotalSize
	snap.FileCount = stats.TotalFileCount
	if err := o.mergeRestoreVerification(ctx, req.RepositoryID, &snap); err != nil {
		return Snapshot{}, err
	}
	return snap, nil
}

func (o *Ops) mergeRestoreVerification(ctx context.Context, repositoryID string, snap *Snapshot) error {
	if o.opts.Verifications == nil {
		return nil
	}
	record, found, err := o.opts.Verifications.RestoreVerification(ctx, repositoryID, snap.ID)
	if err != nil {
		return err
	}
	if found {
		at := record.VerifiedAt.UTC()
		snap.RestoreVerifiedAt = &at
	}
	return nil
}

// ErrSnapshotNotFound 表示这条快照不在仓库里。
var ErrSnapshotNotFound = errors.New("backup: 仓库里没有这条快照")

// snapshotOf 把 restic 的形状翻成我们的形状。
func snapshotOf(s ResticSnapshot) (Snapshot, error) {
	if !resticIDPattern.MatchString(s.ID) {
		return Snapshot{}, fmt.Errorf("backup: 快照 id 形状不合法（%q）", s.ID)
	}
	t, err := time.Parse(time.RFC3339Nano, s.Time)
	if err != nil {
		// restic 的时间是 RFC3339；解析不了就说明这条记录不是我们认得的形状。
		return Snapshot{}, fmt.Errorf("backup: 快照 %s 的时间无法解析（%q）", s.ShortID, s.Time)
	}
	out := Snapshot{
		ID:       s.ID,
		ShortID:  s.ShortID,
		Time:     t.UTC(),
		Hostname: s.Hostname,
		Paths:    s.Paths,
		Tags:     s.Tags,
	}
	for _, tag := range s.Tags {
		switch {
		case strings.HasPrefix(tag, tagPolicyPrefix):
			out.PolicyID = strings.TrimPrefix(tag, tagPolicyPrefix)
		case strings.HasPrefix(tag, tagConsistencyPrefix):
			out.ConsistencyMode = strings.TrimPrefix(tag, tagConsistencyPrefix)
		}
	}
	return out, nil
}

// AppTarget 是要在备份期间临时停掉的一个受管理应用运行对象。
//
// 它是**具体的运行对象**而不是应用 id：真实停谁由 Web 在提交时解析成 systemd 单元名
// 或 64 位容器 id（与 app.logs 同一条路子），代理不猜"app_xxx 现在对应哪个容器"。
type AppTarget struct {
	// AppID 是应用标识（审计与展示用）。
	AppID string
	// Kind 是 systemd 或 container。
	Kind string
	// Object 是单元名或完整容器 id。
	Object string
	// Label 是给人看的名字。
	Label string
}

// 应用运行对象的种类。
const (
	AppKindSystemd   = "systemd"
	AppKindContainer = "container"
)

// AppRuntime 是一个应用的运行事实（预检读一次，执行前再读一次）。
type AppRuntime struct {
	Target AppTarget
	// Found 表示这个运行对象现在真的存在。
	Found bool
	// Running 表示它现在在跑——只有它在跑，才需要停、也才需要拉起。
	Running bool
	// StateText 是读回的状态原文（展示用）。
	StateText string
	// Blockers 是不能停的原因（受保护单元、危险容器、状态说不清）。
	Blockers []Reason
}

// AppAction 是一次停止/拉起的结果。
type AppAction struct {
	Target AppTarget
	// Operation 是 stop 或 start。
	Operation string
	Done      bool
	Verified  bool
	Note      string
}

// AppController 是停/起受管理应用的能力。
//
// 生产实现在代理层（复用 service.control 与 container.control 的同一套判决），
// 这里只声明"备份需要它做什么"。
type AppController interface {
	// Inspect 读这些运行对象的现状（不改任何状态）。
	Inspect(ctx context.Context, targets []AppTarget) ([]AppRuntime, error)
	// Stop 停掉**正在运行且没有阻塞项**的那些，返回逐条结果。
	Stop(ctx context.Context, runtimes []AppRuntime) ([]AppAction, error)
	// Start 按原状态把停过的拉回来。
	Start(ctx context.Context, runtimes []AppRuntime) ([]AppAction, error)
}

// SourcePath 是解析后的来源。
type SourcePath struct {
	Source Source
	// AbsolutePath 是解析出的绝对路径（面板数据库来源为空）。
	AbsolutePath string
	// Exists 表示这个路径现在存在。
	Exists bool
	// IsDir 表示它是目录。
	IsDir bool
}

// RunAssessRequest 是一次备份预检的输入。
type RunAssessRequest struct {
	PolicyID   string
	Policy     Policy
	Repository Repository
	// PanelDatabasePath 是面板库的源路径（配置项）。为空表示这台机器不知道面板库在哪。
	PanelDatabasePath string
	// Controller 非空时才会去读应用的现状（stop_managed_apps）。
	Controller AppController
	// Apps 是解析后的应用运行对象。
	Apps []AppTarget
	// InFlightRepository 表示同一仓库上已经有备份在跑。
	InFlightRepository bool
}

// RunAssessment 是一次备份预检的结论。
type RunAssessment struct {
	PolicyID      string
	RepositoryID  string
	Allowed       bool
	Blockers      []Reason
	Warnings      []Reason
	Impacts       []Impact
	Locks         []string
	Target        Target
	Sources       []SourcePath
	PanelCopy     bool
	Apps          []AppRuntime
	ResticVersion string
	CheckedAt     time.Time
}

// AssessRun 判断这条策略现在能不能跑。只读：不建仓库、不停应用、不写任何文件。
func (o *Ops) AssessRun(ctx context.Context, req RunAssessRequest) (RunAssessment, error) {
	pol, err := req.Policy.Normalize()
	if err != nil {
		return RunAssessment{}, err
	}
	def, err := req.Repository.Definition.Normalize()
	if err != nil {
		return RunAssessment{}, err
	}
	now := o.opts.Now().UTC()
	a := RunAssessment{
		PolicyID:     req.PolicyID,
		RepositoryID: req.Repository.ID,
		Allowed:      true,
		Locks:        []string{LockName(req.Repository.ID), PolicyLockName(req.PolicyID)},
		CheckedAt:    now,
	}
	fingerprint, err := pol.IdentityFingerprint()
	if err != nil {
		return RunAssessment{}, err
	}
	repoFingerprint, err := def.IdentityFingerprint()
	if err != nil {
		return RunAssessment{}, err
	}
	a.Target = Target{
		DisplayName:         pol.Name + " → " + def.DisplayName(),
		IdentityFingerprint: fingerprint + "|" + repoFingerprint,
		StateVersion:        fmt.Sprintf("backup-policy:%s:v%d|repository:%s:v%d", req.PolicyID, 1, req.Repository.ID, req.Repository.Version),
	}

	if ok, why := o.opts.Restic.Available(o.opts.Stat); !ok {
		a.block(CodeResticMissing, "这台机器上没有可用的 restic："+why)
	} else if version, err := o.opts.Restic.Version(ctx); err == nil {
		a.ResticVersion = version
	}

	if req.InFlightRepository {
		a.block(CodeRepositoryBusy,
			"同一个仓库上已经有一次备份在执行；restic 的写操作不能并发，等它跑完再来")
	}

	// 来源：根必须已登记，路径必须真的存在。备份一个不存在的目录，restic 会照做并
	// 产出一个空快照——那看起来像"备份成功"。
	a.Sources = o.resolveSources(pol, &a)

	// 面板库：只有策略里列了它才需要。
	if pol.HasPanelDatabase() {
		a.PanelCopy = true
		switch {
		case req.PanelDatabasePath == "":
			a.block(CodePanelDatabaseMissing,
				"策略要备份面板数据库，但这台代理没有配置面板库路径（agent.backup.panel_database_path）")
		default:
			if st, err := os.Stat(req.PanelDatabasePath); err != nil {
				if errors.Is(err, os.ErrNotExist) {
					a.block(CodePanelDatabaseMissing, "面板数据库 "+req.PanelDatabasePath+" 不存在")
				} else {
					a.block(CodePanelDatabaseMissing, "读不到面板数据库 "+req.PanelDatabasePath+"："+err.Error())
				}
			} else if st.IsDir() {
				a.block(CodePanelDatabaseMissing, req.PanelDatabasePath+" 是目录，不是面板数据库文件")
			}
		}
	}

	// 仓库：目标位置必须已经初始化。这是"能不能备份"与"能不能初始化"的分界。
	o.assessRepositoryReady(ctx, def, &a)

	// 应用：只有 stop_managed_apps 才有东西要停。
	if pol.StopsApps() {
		if req.Controller == nil {
			a.block(CodePolicyAppsUnavailable,
				"这条策略要求先停应用，但这台代理没有应用控制能力（找不到 systemctl / docker）")
		} else if len(req.Apps) == 0 {
			a.block(CodePolicyAppsUnavailable,
				"这条策略要求先停应用，但没有解析出任何可停的运行对象（应用可能已被删除或改绑）")
		} else {
			runtimes, err := req.Controller.Inspect(ctx, req.Apps)
			if err != nil {
				a.block(CodePolicyAppsUnavailable, "读不到应用现状："+err.Error())
			} else {
				a.Apps = runtimes
				for _, rt := range runtimes {
					if !rt.Found {
						a.block(CodePolicyAppsUnavailable,
							fmt.Sprintf("应用 %s 的运行对象 %s 不存在（可能已被删除或改绑）", rt.Target.Label, rt.Target.Object))
						continue
					}
					for _, b := range rt.Blockers {
						a.block(b.Code, b.Message)
					}
				}
			}
		}
	}

	// 影响：说清楚"会发生什么"。停应用尤其要说，因为那是可见的停机。
	if pol.StopsApps() {
		running := 0
		for _, rt := range a.Apps {
			if rt.Found && rt.Running {
				running++
			}
		}
		a.Impacts = append(a.Impacts, Impact{
			Kind: ImpactAvailability,
			Description: fmt.Sprintf("备份期间会停掉 %d 个正在运行的应用（共列出 %d 个），备份结束后按原状态拉起",
				running, len(a.Apps)),
		})
	}
	a.Impacts = append(a.Impacts, Impact{
		Kind:        ImpactCreateRepository,
		Description: "会在仓库 " + def.DisplayName() + " 里写入一个新快照；不会删除任何已有快照（清理只走显式的 backup.prune）",
	})
	if pol.HasPanelDatabase() {
		a.Impacts = append(a.Impacts, Impact{
			Kind:        ImpactSecretUse,
			Description: "会读取面板数据库并生成一份一致副本放进快照；副本在运行结束后删除",
		})
	}
	if pol.ConsistencyMode == ConsistencyFilesOnly {
		a.warn(CodeConsistencyFilesOnly,
			"files_only 只保证文件字节：正在写入的数据库（容器里的 MySQL、面板以外的应用库）在快照里可能不一致；需要一致性请改用 stop_managed_apps")
	}
	if pol.Retention.Configured() {
		a.warn(CodeRetentionNotEnforced,
			"保留策略已记录，但本阶段不会自动清理：清理要由管理员显式发起 backup.prune（P12.3）")
	}
	return a, nil
}

// 只告警的两种情况。
const (
	// CodeConsistencyFilesOnly 提醒 files_only 只保证文件字节。
	CodeConsistencyFilesOnly = "BACKUP_FILES_ONLY"
	// CodeRetentionNotEnforced 提醒保留策略在本阶段不自动执行。
	CodeRetentionNotEnforced = "BACKUP_RETENTION_NOT_ENFORCED"
	// ImpactAvailability 表示“会短暂影响可用性”（停应用）。
	ImpactAvailability = "availability"
)

// resolveSources 把策略来源解析成绝对路径。
//
// 预检与执行都调它：执行时如果某个来源在半路消失了，结果里要能说出是哪一个。
func (o *Ops) resolveSources(pol Policy, a *RunAssessment) []SourcePath {
	out := make([]SourcePath, 0, len(pol.Sources))
	for _, s := range pol.Sources {
		if s.NormalizedKind() != SourceKindRoot {
			continue
		}
		sp := SourcePath{Source: s}
		rootPath, _, unknown := o.opts.Roots.ResolveRoot(s.RootID)
		if unknown {
			sp.AbsolutePath = ""
			if a != nil {
				a.block(CodeSourceBlocked,
					fmt.Sprintf("文件根 %s 没有登记在这台机器上", s.RootID))
			}
			out = append(out, sp)
			continue
		}
		abs := rootPath
		if s.RelativePath != "" {
			abs = joinRoot(rootPath, s.RelativePath)
		}
		sp.AbsolutePath = abs
		if st, err := os.Stat(abs); err != nil {
			if a != nil {
				a.block(CodeSourceMissing, fmt.Sprintf("来源 %s 不存在（%s）", s.String(), abs))
			}
		} else {
			sp.Exists = true
			sp.IsDir = st.IsDir()
			if !st.IsDir() && a != nil {
				// 来源可以是单个文件，但那通常是路径写错了——说清楚，不拦。
				a.warn(CodeSourceBlocked, fmt.Sprintf("来源 %s 解析到的是一个文件（%s）", s.String(), abs))
			}
		}
		out = append(out, sp)
	}
	return out
}

// assessRepositoryReady 判断目标位置是不是一个可写的仓库。
func (o *Ops) assessRepositoryReady(ctx context.Context, def Definition, a *RunAssessment) {
	switch def.Backend {
	case BackendLocal:
		rootPath, _, unknown := o.opts.Roots.ResolveRoot(def.Location.RootID)
		if unknown {
			a.block(CodeRepositoryNotReady, fmt.Sprintf("仓库所在的文件根 %s 没有登记", def.Location.RootID))
			return
		}
		abs := joinRoot(rootPath, def.Location.RelativePath)
		probe, err := ProbeLocal(abs)
		if err != nil {
			a.block(CodeRepositoryNotReady, err.Error())
			return
		}
		if !probe.RepoConfig {
			a.block(CodeRepositoryNotReady,
				fmt.Sprintf("%s 还不是一个 restic 仓库；先做一次初始化（backup.repository.initialize）", abs))
		}
	case BackendSFTP:
		password, err := o.opts.Secrets.Resolve(ctx, def.PasswordSecretRef)
		if err != nil {
			a.block(CodeSecretUnresolved, "取不到仓库口令: "+err.Error())
			return
		}
		wipe(password)
		if !o.opts.Stat(o.opts.Paths.SSH) {
			a.block(CodeSSHMissing, fmt.Sprintf("找不到 ssh 客户端 %s", o.opts.Paths.SSH))
			return
		}
		client, _, err := o.dialSFTP(ctx, ExecuteRequest{}, def)
		if err != nil {
			a.block(CodeSFTPUnreachable, err.Error())
			return
		}
		defer func() { _ = client.Close() }()
		probe, err := ProbeRemote(ctx, client, def.Location.RemotePath)
		if err != nil {
			a.block(CodeSFTPUnreachable, err.Error())
			return
		}
		if !probe.RepoConfig {
			a.block(CodeRepositoryNotReady,
				fmt.Sprintf("远端 %s 还不是一个 restic 仓库；先做一次初始化", def.Location.RemotePath))
		}
	}
}

// block / warn 与仓库预检同一套语义。
func (a *RunAssessment) block(code, message string) {
	a.Allowed = false
	a.Blockers = append(a.Blockers, Reason{Code: code, Message: message})
}

func (a *RunAssessment) warn(code, message string) {
	for _, w := range a.Warnings {
		if w.Code == code {
			return
		}
	}
	a.Warnings = append(a.Warnings, Reason{Code: code, Message: message})
}

// PolicyLockName 是策略的资源锁名。
//
// 它与仓库锁分开：两条策略指向同一个仓库时，仓库锁保证不并发写；同一条策略被
// 触发两次时，策略锁保证第二次立刻被挡在预检上。
func PolicyLockName(policyID string) string {
	return "backup-policy:" + policyID
}

// RunRequest 是一次备份执行的输入。
type RunRequest struct {
	PolicyID   string
	Policy     Policy
	Repository Repository
	// OperationID 用于审计与临时文件命名。
	OperationID string
	// PanelDatabasePath 是面板库的源路径；Policy 没列面板库时忽略。
	PanelDatabasePath string
	// PanelCopyDir 是副本落盘目录（代理的 StateDir 下，root 独占）。
	PanelCopyDir string
	// Apps 是解析后的应用运行对象（stop_managed_apps 时必需）。
	Apps []AppTarget
	// Controller 是应用控制能力。
	Controller AppController
	// Progress 是进度回调；nil 时什么也不做。
	Progress func(step string, fields map[string]any)
	// Cancel 是可中断点检查（安全取消）。
	Cancel <-chan struct{}
}

// RunResult 是一次备份的结果。
type RunResult struct {
	PolicyID     string
	RepositoryID string
	// SnapshotID 只有真的建成了才有值；它是这次运行唯一算数的证据。
	SnapshotID      string
	ConsistencyMode ConsistencyMode
	StartedAt       time.Time
	FinishedAt      time.Time
	Duration        time.Duration
	Sources         []SourcePath
	PanelCopy       *PanelCopy
	Apps            []AppAction
	Summary         BackupSummary
	// Verified 表示 `restic stats <snapshot>` 读回来了。
	Verified    bool
	VerifyError string
	Warnings    []string
	// AppsRestored 表示停过的应用都已经按原状态拉起（没停过时为 true）。
	AppsRestored bool
}

// Run 执行一次备份。
//
// 返回值是**具名**的：收尾（结束时间、耗时、告警、应用是否拉起）写在 defer 里，
// 用 `return out, nil` 的匿名返回会把 defer 里改过的那份丢掉。
func (o *Ops) Run(ctx context.Context, req RunRequest) (out RunResult, err error) {
	pol, err := req.Policy.Normalize()
	if err != nil {
		return RunResult{}, err
	}
	def, err := req.Repository.Definition.Normalize()
	if err != nil {
		return RunResult{}, err
	}
	progress := req.Progress
	if progress == nil {
		progress = func(string, map[string]any) {}
	}
	out = RunResult{
		PolicyID:        req.PolicyID,
		RepositoryID:    req.Repository.ID,
		ConsistencyMode: pol.ConsistencyMode,
		StartedAt:       o.opts.Now().UTC(),
		AppsRestored:    true,
	}
	var warnings []string
	defer func() {
		out.FinishedAt = o.opts.Now().UTC()
		out.Duration = out.FinishedAt.Sub(out.StartedAt)
		out.Warnings = warnings
	}()

	if ok, why := o.opts.Restic.Available(o.opts.Stat); !ok {
		return out, fmt.Errorf("%s：%s", CodeResticMissing, why)
	}

	// 来源：解析 + 存在性。执行期再核一次，因为预检与执行之间可能过了几分钟。
	//
	// 这里只看 Blockers 而不看 Allowed：执行期没有"预检结论"这一层，一份零值的
	// RunAssessment 里 Allowed 本来就是 false。
	var assess RunAssessment
	out.Sources = o.resolveSources(pol, &assess)
	if len(assess.Blockers) > 0 {
		return out, &sourceBlockedError{reasons: assess.Blockers}
	}
	for _, w := range assess.Warnings {
		warnings = append(warnings, w.Message)
	}
	paths := make([]string, 0, len(out.Sources)+1)
	for _, sp := range out.Sources {
		if sp.AbsolutePath == "" {
			return out, fmt.Errorf("%s：来源 %s 无法解析成路径", CodeSourceBlocked, sp.Source.String())
		}
		paths = append(paths, sp.AbsolutePath)
	}

	if err := ctxCancelled(ctx, req.Cancel); err != nil {
		return out, err
	}

	// 停应用：只有 stop_managed_apps 才有这一步。停之前先读一遍现状——只有"刚才还在跑"
	// 的才需要拉起，"本来就是停的"不该被我们启动。
	var stopped []AppRuntime
	if pol.StopsApps() {
		// 收尾要在停机**之前**登记：停到第三个失败时，前两个已经停了，它们必须被
		// 拉回来。把 defer 写在停机成功之后，会让"停了一半"成为唯一没有收尾的路径。
		defer func() {
			restoreAppsAfterRun(context.WithoutCancel(ctx), req, stopped, &out, &warnings, progress)
			if err == nil && out.Verified && !out.AppsRestored {
				err = errors.New("backup: 快照已建立并核验，但至少一个应用未能恢复原运行状态")
			}
		}()
		if err := stopAppsForRun(ctx, req, &out, &warnings, progress, &stopped); err != nil {
			return out, err
		}
	}

	if err := ctxCancelled(ctx, req.Cancel); err != nil {
		return out, err
	}

	inv, cleanup, err := o.runInvocation(ctx, def, req.OperationID)
	if err != nil {
		return out, err
	}
	if cleanup != nil {
		defer cleanup()
	}

	// 面板数据库的一致副本：放在 restic 之前，因为它是这次快照的一份来源。
	if pol.HasPanelDatabase() {
		copy, err := o.capturePanelDatabase(ctx, req, pol, &out)
		if err != nil {
			return out, err
		}
		if copy != nil {
			paths = append(paths, copy.CopyPath)
			// 副本里有会话与口令哈希，只在本次运行期间存在。
			defer func() { _ = os.RemoveAll(filepath.Dir(copy.CopyPath)) }()
		}
	}

	if err := ctxCancelled(ctx, req.Cancel); err != nil {
		return out, err
	}

	tags := []string{
		tagUPanel,
		tagPolicyPrefix + req.PolicyID,
		tagConsistencyPrefix + string(pol.ConsistencyMode),
	}
	progress("backup", map[string]any{"paths": len(paths)})
	// restic 的 --json 进度密到每几十毫秒一条（24.04 实测 4.6 秒 60 条）。每条进度
	// 落到操作事件里就是一次 SQLite 提交，而那台虚机上单次要 50—150ms——写进度比
	// 做备份还贵，事件表也会被撑成流水账。这里按"百分比走过 1% 或距上次超过 1 秒"
	// 再报一次，100% 那条必报：进度是给人看的，不是结果。
	lastPercent := -1.0
	lastReport := time.Now()
	reportedDone := false
	onResticProgress := func(p BackupProgress) {
		switch {
		case p.PercentDone >= 1:
			if reportedDone {
				return
			}
			reportedDone = true
		case p.PercentDone >= lastPercent+0.01:
		case time.Since(lastReport) >= time.Second:
		default:
			return
		}
		lastPercent = p.PercentDone
		lastReport = time.Now()
		fields := map[string]any{
			"percent_done": p.PercentDone,
			"files_done":   p.FilesDone,
			"total_files":  p.TotalFiles,
			"bytes_done":   p.BytesDone,
			"total_bytes":  p.TotalBytes,
		}
		if p.CurrentFile != "" {
			fields["current_file"] = p.CurrentFile
		}
		progress("backup-progress", fields)
	}
	summary, err := o.opts.Restic.Backup(ctx, inv, BackupOptions{
		Tags:     tags,
		Paths:    paths,
		Excludes: defaultExcludes(),
		Timeout:  RunTimeout,
	}, onResticProgress)
	out.Summary = summary
	if err != nil {
		return out, err
	}
	// 快照 id 可能是短 id（restic 0.12 的 backup summary 只报 8 位）。
	// 短 id 不能作为快照的证据链起点——它是前缀，可能撞上多条——所以先换成完整 id。
	fullID, err := o.resolveSnapshotID(ctx, inv, summary.SnapshotID)
	if err != nil {
		out.VerifyError = failureMessage(err)
		return out, err
	}
	out.SnapshotID = fullID
	summary.SnapshotID = fullID
	out.Summary = summary

	// 读回来核对：退出码不是证据，快照能不能被读出来才是。
	progress("verify", map[string]any{"snapshot_id": fullID})
	inv.Timeout = SnapshotQueryTimeout
	stats, err := o.opts.Restic.SnapshotStat(ctx, inv, fullID)
	if err != nil {
		out.VerifyError = failureMessage(err)
		return out, fmt.Errorf("backup: 快照 %s 建完后读不回来：%s", fullID, out.VerifyError)
	}
	out.Verified = true
	progress("verified", map[string]any{
		"snapshot_id": fullID,
		"total_size":  stats.TotalSize,
		"file_count":  stats.TotalFileCount,
	})
	return out, nil
}

// resolveSnapshotID 把 restic 报出来的快照 id 换成完整的 64 位 id。
//
// 为什么需要这一步：restic 0.12（22.04 自带的版本）的 `backup --json` summary 里
// 只有 8 位短 id，而 0.14 及以后给的是完整 id。短 id 是**前缀**：它可能同时是
// 多条快照的前缀，所以不能直接拿它去 stats 或写进结果——必须回仓库里查一次，
// 要求前缀唯一，命中唯一那条才认。
//
// 完整 id 直接原样返回，不多读一次仓库。
func (o *Ops) resolveSnapshotID(ctx context.Context, inv Invocation, id string) (string, error) {
	id = strings.TrimSpace(id)
	if resticIDPattern.MatchString(id) {
		return id, nil
	}
	if !resticShortIDPattern.MatchString(id) {
		return "", fmt.Errorf("backup: restic 报告的快照 id 形状不合法（%q）", id)
	}
	inv.Timeout = SnapshotQueryTimeout
	raw, err := o.opts.Restic.Snapshots(ctx, inv, nil)
	if err != nil {
		return "", fmt.Errorf("backup: 快照 id 是短 id（%q），回仓库解析完整 id 失败：%w", id, err)
	}
	var matches []string
	for i := range raw {
		if strings.HasPrefix(raw[i].ID, id) {
			matches = append(matches, raw[i].ID)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return "", fmt.Errorf("backup: 快照 id（%q）在仓库里找不到对应记录：这次备份可能没有真正落盘", id)
	default:
		return "", fmt.Errorf("backup: 短快照 id（%q）匹配到 %d 条快照，无法确定是哪一条", id, len(matches))
	}
}

// sourceBlockedError 把"来源不可用"的几条原因合成一个错误。
type sourceBlockedError struct{ reasons []Reason }

func (e *sourceBlockedError) Error() string {
	parts := make([]string, 0, len(e.reasons))
	for _, r := range e.reasons {
		parts = append(parts, r.Code+"："+r.Message)
	}
	return "backup: 备份来源不可用：" + strings.Join(parts, "；")
}

// defaultExcludes 是永远不该进快照的几个系统目录（PLAN 第 17 节）。
//
// 只列这三个：源都在受管理根里，正常情况下根本碰不到它们；列在这里是防止某个根
// 恰好是 /（那时备份 /proc 会读到内核状态、备份 /dev 会读到设备节点）。
// 不列 /tmp、/run：它们可能就是源本身，排除掉会把用户要的东西一起排掉。
func defaultExcludes() []string { return []string{"/proc", "/sys", "/dev"} }

// stopAppsForRun 停掉正在运行的应用，把被停的那些写进 stopped。
//
// 用指针回填而不是返回值：调用点要在**调用之前**登记拉起用的 defer，那之后才能
// 读到"到底停成了哪几个"。
func stopAppsForRun(ctx context.Context, req RunRequest, out *RunResult, warnings *[]string,
	progress func(string, map[string]any), stopped *[]AppRuntime) error {
	if req.Controller == nil {
		return fmt.Errorf("%s：没有应用控制能力", CodePolicyAppsUnavailable)
	}
	if len(req.Apps) == 0 {
		return fmt.Errorf("%s：没有解析出可停的运行对象", CodePolicyAppsUnavailable)
	}
	runtimes, err := req.Controller.Inspect(ctx, req.Apps)
	if err != nil {
		return fmt.Errorf("%s：读不到应用现状：%w", CodePolicyAppsUnavailable, err)
	}
	var running []AppRuntime
	for _, rt := range runtimes {
		switch {
		case !rt.Found:
			// 一个已经不存在的运行对象不该让整个备份失败：它本来就没有数据要停顿。
			// 但它必须出现在结果里，否则"我停了三个应用"这句话就少了一半。
			*warnings = append(*warnings, fmt.Sprintf("应用 %s 的运行对象 %s 不存在，跳过",
				rt.Target.Label, rt.Target.Object))
		case len(rt.Blockers) > 0:
			reasons := make([]string, 0, len(rt.Blockers))
			for _, b := range rt.Blockers {
				reasons = append(reasons, b.Message)
			}
			return fmt.Errorf("%s：应用 %s 不能停：%s", CodePolicyAppsUnavailable,
				rt.Target.Label, strings.Join(reasons, "；"))
		case rt.Running:
			running = append(running, rt)
		}
	}
	if len(running) == 0 {
		progress("apps-stop", map[string]any{"stopped": 0})
		return nil
	}
	progress("apps-stop", map[string]any{"count": len(running)})
	actions, stopErr := req.Controller.Stop(ctx, running)
	out.Apps = append(out.Apps, actions...)
	out.AppsRestored = false
	// 先回填再判失败：控制器会"尽量都停一遍"，成功停掉的那几个必须被收尾拉起。
	*stopped = stoppedFrom(running, actions)
	verified := make(map[string]bool, len(actions))
	for _, act := range actions {
		verified[act.Target.Kind+"|"+act.Target.Object] = act.Verified
		if !act.Verified {
			note := act.Note
			if note == "" {
				note = "没有读回确认"
			}
			return fmt.Errorf("backup: 停止应用 %s 没有确认成功：%s", act.Target.Label, note)
		}
	}
	for _, rt := range running {
		if !verified[rt.Target.Kind+"|"+rt.Target.Object] {
			return fmt.Errorf("backup: 停止应用 %s 没有返回执行结果，不能开始一致性备份", rt.Target.Label)
		}
	}
	if stopErr != nil {
		return stopErr
	}
	return nil
}

// stoppedFrom 从"请求停的"与"实际动作"里取出真的可能被停掉的那些。
//
// 判据是动作的结果而不是意图：Done 为 false 且 Note 说读不到对象的那些，我们
// 没有改变它的状态，也就不该在收尾时去 start 它——那会变成"备份顺手启动了一个
// 本来关着的应用"。Done 为 true 但没确认的，仍然要试着拉起。
func stoppedFrom(running []AppRuntime, actions []AppAction) []AppRuntime {
	changed := make(map[string]bool, len(actions))
	for _, act := range actions {
		if act.Done || act.Verified {
			changed[act.Target.Kind+"|"+act.Target.Object] = true
		}
	}
	out := make([]AppRuntime, 0, len(running))
	for _, rt := range running {
		if changed[rt.Target.Kind+"|"+rt.Target.Object] {
			out = append(out, rt)
		}
	}
	return out
}

// restoreAppsAfterRun 把停过的应用按原状态拉起。
//
// 它必须用 context.WithoutCancel：备份被取消时 ctx 已经到期，而"把机器恢复原状"
// 这件事不该跟着取消一起停下。
func restoreAppsAfterRun(ctx context.Context, req RunRequest, stopped []AppRuntime, out *RunResult,
	warnings *[]string, progress func(string, map[string]any)) {
	if len(stopped) == 0 || req.Controller == nil {
		out.AppsRestored = true
		return
	}
	progress("apps-start", map[string]any{"count": len(stopped)})
	actions, err := req.Controller.Start(ctx, stopped)
	out.Apps = append(out.Apps, actions...)
	if err != nil {
		*warnings = append(*warnings, "拉起应用失败："+err.Error())
	}
	restored := true
	verified := make(map[string]bool, len(actions))
	for _, act := range actions {
		verified[act.Target.Kind+"|"+act.Target.Object] = act.Verified
		if !act.Verified {
			restored = false
			note := act.Note
			if note == "" {
				note = "没有读回确认"
			}
			*warnings = append(*warnings, fmt.Sprintf("应用 %s 没有确认拉起：%s", act.Target.Label, note))
		}
	}
	for _, rt := range stopped {
		if !verified[rt.Target.Kind+"|"+rt.Target.Object] {
			restored = false
			*warnings = append(*warnings, fmt.Sprintf("应用 %s 没有返回拉起结果", rt.Target.Label))
		}
	}
	out.AppsRestored = restored && err == nil
}

// capturePanelDatabase 取一份面板库的一致副本。
func (o *Ops) capturePanelDatabase(ctx context.Context, req RunRequest, pol Policy,
	out *RunResult) (*PanelCopy, error) {
	if req.PanelDatabasePath == "" {
		return nil, fmt.Errorf("%s：没有配置面板数据库路径", CodePanelDatabaseMissing)
	}
	if req.PanelCopyDir == "" {
		return nil, errors.New("backup: 没有指定面板库副本目录")
	}
	dir := filepath.Join(req.PanelCopyDir, req.PolicyID)
	dest := filepath.Join(dir, "upanel.db")
	copy, err := CopyPanelDatabase(ctx, req.PanelDatabasePath, dest)
	if err != nil {
		if errors.Is(err, ErrPanelDatabaseMissing) {
			return nil, fmt.Errorf("%s：%w", CodePanelDatabaseMissing, err)
		}
		return nil, err
	}
	out.PanelCopy = &copy
	return &copy, nil
}

// readInvocation 为只读的仓库查询准备调用参数。
//
// 与执行期不同的是：这里**不写** known_hosts（那是初始化的副作用），SFTP 上缺
// known_hosts 就直接说"本机还没核对过这台主机"。
func (o *Ops) readInvocation(ctx context.Context, def Definition) (Invocation, func(), error) {
	switch def.Backend {
	case BackendLocal:
		rootPath, _, unknown := o.opts.Roots.ResolveRoot(def.Location.RootID)
		if unknown {
			return Invocation{}, nil, fmt.Errorf("%s：文件根 %s 没有登记", CodeRootUnknown, def.Location.RootID)
		}
		password, err := o.opts.Secrets.Resolve(ctx, def.PasswordSecretRef)
		if err != nil {
			return Invocation{}, nil, fmt.Errorf("%s：取不到仓库口令: %w", CodeSecretUnresolved, err)
		}
		inv := Invocation{Repository: def.resticRepository(joinRoot(rootPath, def.Location.RelativePath)),
			Password: string(password)}
		return inv, func() { wipe(password) }, nil
	case BackendSFTP:
		knownHostsPath := o.opts.Restic.knownHostsPath(def.Location.Host, def.Location.Port)
		if !o.opts.Stat(knownHostsPath) {
			return Invocation{}, nil, fmt.Errorf("backup: 本机还没有 %s 的 known_hosts（先做一次初始化预检）",
				def.Location.Host)
		}
		if !o.opts.Stat(o.opts.Paths.SSH) {
			return Invocation{}, nil, fmt.Errorf("%s：找不到 ssh 客户端 %s", CodeSSHMissing, o.opts.Paths.SSH)
		}
		password, err := o.opts.Secrets.Resolve(ctx, def.PasswordSecretRef)
		if err != nil {
			return Invocation{}, nil, fmt.Errorf("%s：取不到仓库口令: %w", CodeSecretUnresolved, err)
		}
		key, err := o.opts.Secrets.Resolve(ctx, def.CredentialSecretRef)
		if err != nil {
			wipe(password)
			return Invocation{}, nil, fmt.Errorf("%s：取不到 SFTP 私钥: %w", CodeSecretUnresolved, err)
		}
		keyPath, err := o.opts.Restic.WritePrivateKey("query:"+def.Location.Host, key)
		wipe(key)
		if err != nil {
			wipe(password)
			return Invocation{}, nil, err
		}
		inv := Invocation{
			Repository: def.resticRepository(""),
			Password:   string(password),
			SFTPCommand: SFTPCommand(o.opts.Paths.SSH, knownHostsPath, keyPath,
				def.Location.Host, def.Location.Port, def.Location.Username),
		}
		return inv, func() {
			RemovePrivateKey(keyPath)
			wipe(password)
		}, nil
	}
	return Invocation{}, nil, fmt.Errorf("backup: 未知后端 %q", def.Backend)
}

// runInvocation 为一次**写**操作准备调用参数。
//
// 与 readInvocation 的差别只有两处，但那两处都是安全边界：执行期要再核对一次主机
// 密钥并**写出** known_hosts（核对通过才写），私钥文件按操作 id 命名。
func (o *Ops) runInvocation(ctx context.Context, def Definition, operationID string) (Invocation, func(), error) {
	if def.Backend == BackendLocal {
		return o.readInvocation(ctx, def)
	}
	if !o.opts.Stat(o.opts.Paths.SSH) {
		return Invocation{}, nil, fmt.Errorf("%s：找不到 ssh 客户端 %s", CodeSSHMissing, o.opts.Paths.SSH)
	}
	registered, err := ParseFingerprint(def.Location.HostKeyFingerprint)
	if err != nil {
		return Invocation{}, nil, err
	}
	password, err := o.opts.Secrets.Resolve(ctx, def.PasswordSecretRef)
	if err != nil {
		return Invocation{}, nil, fmt.Errorf("%s：取不到仓库口令: %w", CodeSecretUnresolved, err)
	}
	key, err := o.opts.Secrets.Resolve(ctx, def.CredentialSecretRef)
	if err != nil {
		wipe(password)
		return Invocation{}, nil, fmt.Errorf("%s：取不到 SFTP 私钥: %w", CodeSecretUnresolved, err)
	}
	client, hostKey, err := Dial(ctx, DialOptions{
		Host:       def.Location.Host,
		Port:       def.Location.Port,
		User:       def.Location.Username,
		PrivateKey: key,
		Timeout:    DefaultDialTimeout,
	}, registered)
	if err != nil {
		wipe(password)
		wipe(key)
		return Invocation{}, nil, err
	}
	_ = client.Close()
	knownHostsPath, err := o.opts.Restic.WriteKnownHosts(def.Location.Host, def.Location.Port,
		KnownHostsLine(def.Location.Host, def.Location.Port, hostKey.PublicKey))
	if err != nil {
		wipe(password)
		wipe(key)
		return Invocation{}, nil, err
	}
	keyPath, err := o.opts.Restic.WritePrivateKey(operationID, key)
	wipe(key)
	if err != nil {
		wipe(password)
		return Invocation{}, nil, err
	}
	inv := Invocation{
		Repository: def.resticRepository(""),
		Password:   string(password),
		SFTPCommand: SFTPCommand(o.opts.Paths.SSH, knownHostsPath, keyPath,
			def.Location.Host, def.Location.Port, def.Location.Username),
	}
	return inv, func() {
		RemovePrivateKey(keyPath)
		wipe(password)
	}, nil
}
