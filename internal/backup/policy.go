package backup

// 备份策略的定义与校验（P12.2；CONTRACTS.md 第 12 节）。
//
// 策略回答的是“备份什么、以什么一致性口径备份、留多少”。它**不含**仓库口令、
// 不含主机名、也不含任何路径的绝对形式——源只有“哪个受管理根 + 根下哪段相对路径”，
// 或者“面板自己的数据库”这一个特殊来源。绝对于事无补：执行时根表才是唯一事实。
//
// 三处刻意的取舍：
//
//	一致性   只有两种：files_only（只保证文件字节）与 stop_managed_apps（先停应用）。
//	         没有第五种“近似一致”——PLAN 第 17 节要求把“这份备份一致吗”说清楚，
//	         而不是给一个听起来更好的名字。
//	面板库   以 source 的形式**显式列出**（kind=panel_database），不隐式塞进每次备份。
//	         隐式的话，用户看到的“我选了这两个目录”与实际被打包的内容不一致。
//	保留     本阶段只**存**不执行：清理是 P12.3 的 backup.prune，必须由管理员显式发起
//	         （CONTRACTS 第 12 节：备份定时任务不隐式执行删除操作）。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// ActionRun / QuerySnapshots 是 P12.2 新增的固定名称（CONTRACTS.md 第 5、12 节）。
const (
	// ActionRun 是执行一次备份的动作名（冻结）。
	ActionRun = "backup.run"
	// QuerySnapshots 是仓库快照列表的只读查询名。
	QuerySnapshots = "backup.snapshots"
	// QuerySnapshot 是单个快照详情的只读查询名。
	QuerySnapshot = "backup.snapshot"
)

// SourceKind 是备份来源的种类。
type SourceKind string

const (
	// SourceKindRoot 是受管理文件根下的一段路径。
	SourceKindRoot SourceKind = "root"
	// SourceKindPanelDatabase 是面板自己的 SQLite 数据库（在线一致副本）。
	SourceKindPanelDatabase SourceKind = "panel_database"
)

// ConsistencyMode 是一致性口径。
type ConsistencyMode string

const (
	// ConsistencyFilesOnly 只保证文件字节；运行中的数据库可能不一致。
	ConsistencyFilesOnly ConsistencyMode = "files_only"
	// ConsistencyStopManagedApps 备份前停掉指定应用，备份后按原状态拉起。
	ConsistencyStopManagedApps ConsistencyMode = "stop_managed_apps"
)

// Valid 判断一致性口径是否在闭集内。
func (m ConsistencyMode) Valid() bool {
	return m == ConsistencyFilesOnly || m == ConsistencyStopManagedApps
}

// Source 是一条备份来源。
type Source struct {
	// Kind 为空时按 root 处理（P12.2 之前的记录里没有这个字段）。
	Kind SourceKind `json:"kind,omitempty"`
	// RootID 是受管理文件根标识（kind=root 时必需）。
	RootID string `json:"root_id,omitempty"`
	// RelativePath 是根下相对路径；空串表示根本身。
	RelativePath string `json:"relative_path,omitempty"`
}

// NormalizedKind 返回规范后的来源种类。
func (s Source) NormalizedKind() SourceKind {
	if s.Kind == "" {
		return SourceKindRoot
	}
	return s.Kind
}

// String 是来源的可读形式（不含绝对路径）。
func (s Source) String() string {
	switch s.NormalizedKind() {
	case SourceKindPanelDatabase:
		return "面板数据库（SQLite 在线副本）"
	case SourceKindRoot:
		if s.RelativePath == "" {
			return "root:" + s.RootID + ":/"
		}
		return "root:" + s.RootID + ":/" + s.RelativePath
	}
	return string(s.Kind)
}

// Retention 是保留策略。0 表示“不设这一项”。
type Retention struct {
	KeepLast   int `json:"keep_last,omitempty"`
	KeepDaily  int `json:"keep_daily,omitempty"`
	KeepWeekly int `json:"keep_weekly,omitempty"`
}

// Configured 报告是否至少设了一项。
func (r Retention) Configured() bool {
	return r.KeepLast > 0 || r.KeepDaily > 0 || r.KeepWeekly > 0
}

// Policy 是一条备份策略。
type Policy struct {
	Name string `json:"name"`
	// RepositoryID 指向一条已登记的仓库定义。
	RepositoryID    string          `json:"repository_id"`
	Sources         []Source        `json:"sources"`
	ConsistencyMode ConsistencyMode `json:"consistency_mode"`
	// AppIDs 只在 stop_managed_apps 时出现：要停哪些受管理应用。
	AppIDs    []string  `json:"app_ids,omitempty"`
	Retention Retention `json:"retention"`
}

// 策略的长度上限。每一条都对应一个落库位置或一个 argv 位置。
const (
	// MaxPolicyNameLength 是策略显示名上限。
	MaxPolicyNameLength = 64
	// MaxPolicySources 是来源条数上限。
	MaxPolicySources = 32
	// MaxPolicyApps 是 stop_managed_apps 模式下应用条数上限。
	MaxPolicyApps = 32
	// MaxRetentionCount 是保留条数的上限（再大就没有意义，而且会把清理变成另一种危险）。
	MaxRetentionCount = 1000
	// MaxAppIDLength 是应用标识长度上限。
	MaxAppIDLength = 64
	// MaxRootsPerPolicy 是同一策略里不同文件根的上限。
	MaxRootsPerPolicy = 16
)

// repositoryIDPattern 是仓库 id 的形状（rep_ + 32 位小写十六进制）。
//
// 与 agentstore.ValidRepositoryID 同形；在这里重写一遍是**故意**的：策略校验要在
// 没有数据库的单元测试里跑（理由与 definition.go 里的 secretRefPattern 相同）。
var repositoryIDPattern = regexp.MustCompile(`^rep_[0-9a-f]{32}$`)

// ValidRepositoryID 判断仓库 id 形状。
func ValidRepositoryID(id string) bool { return repositoryIDPattern.MatchString(id) }

// appIDPattern 是受管理应用标识的形状（app_ + 32 位小写十六进制）。
var appIDPattern = regexp.MustCompile(`^app_[0-9a-f]{32}$`)

// ValidAppID 判断应用 id 形状。
func ValidAppID(id string) bool { return appIDPattern.MatchString(id) }

// policyIDPattern 是策略 id 的形状（pol_ + 32 位小写十六进制）。
//
// 放在这里而不是 agentstore：Web 侧也要判它（展开 backup.run 的参数之前，
// /backup/policies/:id 的形状校验，以及快照查询的 policy_id 过滤），而 Web 不该
// 为了一个正则把代理那套 SQLite 存储层拉进自己的二进制。
var policyIDPattern = regexp.MustCompile(`^pol_[0-9a-f]{32}$`)

// ValidPolicyID 判断策略 id 形状。
func ValidPolicyID(id string) bool { return policyIDPattern.MatchString(id) }

// ValidRootID 校验受管理根标识的形状；根是否真实登记由文件模块判定。
func ValidRootID(id string) bool { return rootIDPattern.MatchString(id) }

// ValidSnapshotID 只接受 restic 的完整 64 位快照 id，动作参数不接受模糊前缀。
func ValidSnapshotID(id string) bool { return resticIDPattern.MatchString(id) }

// Normalize 校验并规范化策略。
//
// 只做形状校验：根是否存在、路径是否真在根里、应用是否还登记着，都是预检去核的
// 事实（预检读系统，解析只认字段）。
func (p Policy) Normalize() (Policy, error) {
	out := p

	out.Name = strings.TrimSpace(out.Name)
	if out.Name == "" {
		return Policy{}, invalid("policy.name", "不能为空")
	}
	if len(out.Name) > MaxPolicyNameLength {
		return Policy{}, invalid("policy.name", "不能超过 %d 字节", MaxPolicyNameLength)
	}
	if err := checkPrintable("policy.name", out.Name); err != nil {
		return Policy{}, err
	}

	out.RepositoryID = strings.TrimSpace(out.RepositoryID)
	if out.RepositoryID == "" {
		return Policy{}, invalid("policy.repository_id", "不能为空")
	}
	if !ValidRepositoryID(out.RepositoryID) {
		return Policy{}, invalid("policy.repository_id", "形状不合法（应为 rep_ 加 32 位小写十六进制）")
	}

	sources, err := normalizeSources(out.Sources)
	if err != nil {
		return Policy{}, err
	}
	out.Sources = sources

	out.ConsistencyMode = ConsistencyMode(strings.TrimSpace(string(out.ConsistencyMode)))
	if out.ConsistencyMode == "" {
		out.ConsistencyMode = ConsistencyFilesOnly
	}
	if !out.ConsistencyMode.Valid() {
		return Policy{}, invalid("policy.consistency_mode",
			"只能是 files_only 或 stop_managed_apps（收到 %q）", string(out.ConsistencyMode))
	}

	apps, err := normalizeAppIDs(out.AppIDs)
	if err != nil {
		return Policy{}, err
	}
	out.AppIDs = apps
	switch out.ConsistencyMode {
	case ConsistencyStopManagedApps:
		if len(out.AppIDs) == 0 {
			return Policy{}, invalid("policy.app_ids",
				"stop_managed_apps 必须列出要停的应用：不说停谁，就等于说“随便停”")
		}
	case ConsistencyFilesOnly:
		if len(out.AppIDs) != 0 {
			return Policy{}, invalid("policy.app_ids",
				"files_only 模式下不接受 app_ids：那一列在这条策略里不会被执行，留着只会让人以为会停")
		}
	}

	if err := out.Retention.validate(); err != nil {
		return Policy{}, err
	}
	return out, nil
}

// normalizeSources 校验来源列表：条数、重复、形状。
func normalizeSources(in []Source) ([]Source, error) {
	if len(in) == 0 {
		return nil, invalid("policy.sources", "至少要有一个备份来源")
	}
	if len(in) > MaxPolicySources {
		return nil, invalid("policy.sources", "不能超过 %d 条", MaxPolicySources)
	}
	out := make([]Source, 0, len(in))
	seen := make(map[string]bool, len(in))
	roots := 0
	panel := 0
	for i, s := range in {
		kind := s.NormalizedKind()
		var n Source
		switch kind {
		case SourceKindRoot:
			rootID := strings.TrimSpace(s.RootID)
			if rootID == "" {
				return nil, invalid(fmt.Sprintf("policy.sources[%d].root_id", i), "不能为空")
			}
			if len(rootID) > MaxRootIDLength || !rootIDPattern.MatchString(rootID) {
				return nil, invalid(fmt.Sprintf("policy.sources[%d].root_id", i),
					"形状不合法（应以字母或数字开头，只含字母、数字、下划线、点和连字符）")
			}
			rel := strings.TrimSpace(s.RelativePath)
			if rel != "" {
				cleaned, err := normalizeRelativePath(rel)
				if err != nil {
					return nil, fmt.Errorf("%w: policy.sources[%d].relative_path 不合法：%v", ErrInvalid, i, err)
				}
				rel = cleaned
			}
			n = Source{Kind: SourceKindRoot, RootID: rootID, RelativePath: rel}
			roots++
		case SourceKindPanelDatabase:
			if strings.TrimSpace(s.RootID) != "" || strings.TrimSpace(s.RelativePath) != "" {
				return nil, invalid(fmt.Sprintf("policy.sources[%d]", i),
					"面板数据库来源不接受 root_id/relative_path")
			}
			panel++
			n = Source{Kind: SourceKindPanelDatabase}
		default:
			return nil, invalid(fmt.Sprintf("policy.sources[%d].kind", i),
				"只能是 root 或 panel_database（收到 %q）", string(kind))
		}
		key := n.String()
		if seen[key] {
			return nil, invalid(fmt.Sprintf("policy.sources[%d]", i), "与前一条重复：%s", key)
		}
		seen[key] = true
		out = append(out, n)
	}
	if panel > 1 {
		return nil, invalid("policy.sources", "面板数据库只能列一次")
	}
	if roots > MaxRootsPerPolicy {
		return nil, invalid("policy.sources", "涉及的受管理根不能超过 %d 个", MaxRootsPerPolicy)
	}
	return out, nil
}

// normalizeAppIDs 校验应用列表：形状、重复、排序。
func normalizeAppIDs(in []string) ([]string, error) {
	if len(in) > MaxPolicyApps {
		return nil, invalid("policy.app_ids", "不能超过 %d 条", MaxPolicyApps)
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for i, id := range in {
		id = strings.TrimSpace(id)
		if !ValidAppID(id) {
			return nil, invalid(fmt.Sprintf("policy.app_ids[%d]", i),
				"形状不合法（应为 app_ 加 32 位小写十六进制）")
		}
		if seen[id] {
			return nil, invalid(fmt.Sprintf("policy.app_ids[%d]", i), "重复出现")
		}
		seen[id] = true
		out = append(out, id)
	}
	// 排序让同一组应用的身份指纹稳定：顺序不承载语义，但它出现在指纹里。
	sort.Strings(out)
	return out, nil
}

// validate 校验保留策略的每一项。
func (r Retention) validate() error {
	for _, item := range []struct {
		field string
		value int
	}{
		{"policy.retention.keep_last", r.KeepLast},
		{"policy.retention.keep_daily", r.KeepDaily},
		{"policy.retention.keep_weekly", r.KeepWeekly},
	} {
		if item.value < 0 || item.value > MaxRetentionCount {
			return invalid(item.field, "必须在 0—%d 之间（0 表示不设这一项，收到 %d）",
				MaxRetentionCount, item.value)
		}
	}
	return nil
}

// IdentityFingerprint 是策略的身份指纹（sha256，十六进制）。
//
// 预检绑定它、执行前再核对一次：预检之后有人改了来源或一致性口径，旧预检必须失效。
func (p Policy) IdentityFingerprint() (string, error) {
	raw, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("backup: 计算策略身份指纹失败: %w", err)
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// DisplayName 是给人看的一句话：策略名 + 仓库。
func (p Policy) DisplayName() string {
	return p.Name + "（" + p.RepositoryID + "）"
}

// HasPanelDatabase 报告这条策略是否要备份面板数据库。
func (p Policy) HasPanelDatabase() bool {
	for _, s := range p.Sources {
		if s.NormalizedKind() == SourceKindPanelDatabase {
			return true
		}
	}
	return false
}

// RootSources 返回需要解析成路径的来源（不含面板数据库）。
func (p Policy) RootSources() []Source {
	out := make([]Source, 0, len(p.Sources))
	for _, s := range p.Sources {
		if s.NormalizedKind() == SourceKindRoot {
			out = append(out, s)
		}
	}
	return out
}

// StopsApps 报告这条策略是否需要停应用。
func (p Policy) StopsApps() bool { return p.ConsistencyMode == ConsistencyStopManagedApps }

// PolicyRecord 是代理镜像里的一条策略（带乐观并发令牌）。
//
// 与 Repository 同形：动作参数里只有 id 与 version，其余全是代理自己库里的事实。
type PolicyRecord struct {
	ID      string
	Name    string
	Policy  Policy
	Version int64
}

// PolicySource 提供策略镜像（生产实现是 agentstore.BackupPolicyStore）。
type PolicySource interface {
	GetPolicy(ctx context.Context, id string) (PolicyRecord, bool, error)
}
