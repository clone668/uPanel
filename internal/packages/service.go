// Package packages 是包管理在**面板侧**的判断层（P11.1）。
//
// 分工（CONTRACTS.md 第 12.1 节第 1 条）：代理给事实——装了什么包、装的哪个版本、
// `apt-cache policy` 说候选是哪个、候选来自哪些 suite；这里给判断——候选比已安装
// 新不新、算不算安全更新、属于哪个口袋、一页取多少条。
//
// 为什么判断放在面板：这些规则会变（发行版加口袋、安全源改名、以后要接 ESM），
// 而事实不该跟着规则一起变。把规则放进代理意味着每次改规则都要重装 root 端的
// 二进制；放在面板只换一个 Web 进程。
//
// 这里**不**判断"这次升级会不会真的升它"。分阶段推送（phased）与被保留（held back）
// 只有 `apt-get -s upgrade` 说得清，那是 P11.2 的模拟。
package packages

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"upanel/internal/apt"
	"upanel/internal/debversion"
)

// 分页上限。CONTRACTS.md 第 12.1 节第 9 条。
const (
	// DefaultLimit 是默认页大小。
	DefaultLimit = 50
	// MaxLimit 是单页上限。676 个包在一页里能塞下，但一台跑了几年的机器上
	// 一两千个包很常见，一次全吐出去既白费带宽又让页面卡。
	MaxLimit = 200
	// MaxUpdates 是 /packages/updates 一次返回的条数上限。
	MaxUpdates = 1000
	// MaxQueryLen 是搜索串上限。
	MaxQueryLen = 100
)

// 候选版本与已安装版本的关系（candidate_relation 的闭集）。
const (
	RelationNewer   = "newer"
	RelationSame    = "same"
	RelationOlder   = "older"
	RelationUnknown = "unknown"
)

// 口袋的闭集，顺序即输出顺序（与 OpenAPI 的 enum 一致）。
const (
	PocketRelease   = "release"
	PocketUpdates   = "updates"
	PocketSecurity  = "security"
	PocketBackports = "backports"
	PocketProposed  = "proposed"
	PocketOther     = "other"
)

var pocketOrder = []string{PocketRelease, PocketUpdates, PocketSecurity, PocketBackports, PocketProposed, PocketOther}

var (
	// ErrInvalidCursor 表示游标不是本服务发出的。
	ErrInvalidCursor = errors.New("packages: 游标不合法")
	// ErrInvalidQuery 表示筛选条件不合法。
	ErrInvalidQuery = errors.New("packages: 筛选条件不合法")
	// ErrFactsUnavailable 表示读不到代理侧的事实（代理没连上、查询被拒）。
	ErrFactsUnavailable = errors.New("packages: 事实来源不可用")
)

// Source 是事实来源。生产实现是 *AgentFacts；测试给假实现。
type Source interface {
	// Packages 返回包事实。fresh=true 绕过代理侧快照缓存。
	Packages(ctx context.Context, fresh bool) (apt.Snapshot, error)
	// PowerState 返回重启标志与本 boot_id。
	PowerState(ctx context.Context) (apt.PowerState, error)
}

// Service 把事实变成判断。
type Service struct {
	source Source
}

// New 构造服务。source 为 nil 时报错：一个"永远空"的包列表比 503 更能骗人。
func New(source Source) (*Service, error) {
	if source == nil {
		return nil, errors.New("packages: 需要事实来源")
	}
	return &Service{source: source}, nil
}

// Entry 是一个包在页面上的样子（PackageEntry）。
type Entry struct {
	Name              string   `json:"name"`
	Architecture      string   `json:"architecture"`
	Status            string   `json:"status"`
	Installed         bool     `json:"installed"`
	InstalledVersion  string   `json:"installed_version"`
	CandidateVersion  *string  `json:"candidate_version"`
	CandidateRelation string   `json:"candidate_relation"`
	Upgradable        bool     `json:"upgradable"`
	Security          bool     `json:"security"`
	Suites            []string `json:"suites"`
	Pockets           []string `json:"pockets"`
}

// Counts 是全量计数（不受分页影响）。
type Counts struct {
	Installed  int `json:"installed"`
	Upgradable int `json:"upgradable"`
	Security   int `json:"security"`
}

// Filters 是本次生效的筛选条件（回显给页面，让它知道自己看到的是什么）。
type Filters struct {
	Query      string `json:"query"`
	Installed  bool   `json:"installed"`
	Upgradable bool   `json:"upgradable"`
	Security   bool   `json:"security"`
}

// List 是 GET /packages 的响应体。
type List struct {
	CollectedAt    time.Time         `json:"collected_at"`
	IndexUpdatedAt *time.Time        `json:"index_updated_at"`
	Counts         Counts            `json:"counts"`
	Filters        Filters           `json:"filters"`
	Matched        int               `json:"matched"`
	Limit          int               `json:"limit"`
	NextCursor     *string           `json:"next_cursor"`
	Packages       []Entry           `json:"packages"`
	Errors         []apt.SourceError `json:"errors"`
}

// UpdateEntry 是可升级清单的一行。
type UpdateEntry struct {
	Name             string   `json:"name"`
	Architecture     string   `json:"architecture"`
	InstalledVersion string   `json:"installed_version"`
	CandidateVersion string   `json:"candidate_version"`
	Security         bool     `json:"security"`
	Suites           []string `json:"suites"`
	Pockets          []string `json:"pockets"`
}

// Updates 是 GET /packages/updates 的响应体。
type Updates struct {
	CollectedAt    time.Time         `json:"collected_at"`
	IndexUpdatedAt *time.Time        `json:"index_updated_at"`
	Total          int               `json:"total"`
	Security       int               `json:"security"`
	Truncated      bool              `json:"truncated"`
	Updates        []UpdateEntry     `json:"updates"`
	Errors         []apt.SourceError `json:"errors"`
}

// PowerStateView 是 GET /system/power-state 的响应体。
type PowerStateView struct {
	CollectedAt                 time.Time  `json:"collected_at"`
	RebootRequired              bool       `json:"reboot_required"`
	RebootRequiredSince         *time.Time `json:"reboot_required_since"`
	RebootRequiredPackages      []string   `json:"reboot_required_packages"`
	RebootRequiredPackagesError string     `json:"reboot_required_packages_error"`
	BootID                      string     `json:"boot_id"`
	BootIDError                 string     `json:"boot_id_error"`
}

// ListQuery 是 GET /packages 的筛选与分页参数。
type ListQuery struct {
	Query      string
	Installed  bool
	Upgradable bool
	Security   bool
	Limit      int
	Cursor     string
}

// List 返回一页包。
//
// fresh=false：展示用，吃代理侧 30 秒快照。写操作前的判断（P11.2）会传 true。
func (s *Service) List(ctx context.Context, q ListQuery) (List, error) {
	if err := ValidateListQuery(&q); err != nil {
		return List{}, err
	}
	snap, err := s.source.Packages(ctx, false)
	if err != nil {
		return List{}, err
	}

	entries := make([]Entry, 0, len(snap.Packages))
	counts := Counts{}
	for _, p := range snap.Packages {
		e := judge(p)
		if e.Installed {
			counts.Installed++
		}
		if e.Upgradable {
			counts.Upgradable++
		}
		if e.Security {
			counts.Security++
		}
		entries = append(entries, e)
	}

	// 排序在判断之后再做一遍：代理已经按 (name, arch) 给过，但这一层不能依赖
	// 上游的顺序——游标是"比这一行大的下一行"，顺序一变游标就会跳包。
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Name != entries[j].Name {
			return entries[i].Name < entries[j].Name
		}
		return entries[i].Architecture < entries[j].Architecture
	})

	filtered := make([]Entry, 0, len(entries))
	needle := strings.ToLower(q.Query)
	for _, e := range entries {
		if q.Installed && !e.Installed {
			continue
		}
		if q.Upgradable && !e.Upgradable {
			continue
		}
		if q.Security && !e.Security {
			continue
		}
		if needle != "" && !strings.Contains(strings.ToLower(e.Name), needle) {
			continue
		}
		filtered = append(filtered, e)
	}

	// 游标：从"严格大于上一页最后一行"的位置开始。这样即使在翻页期间有包被装上，
	// 也只会重复漏掉同名的边界行，不会整体错位。
	start := 0
	if q.Cursor != "" {
		cur, err := decodeCursor(q.Cursor)
		if err != nil {
			return List{}, err
		}
		start = sort.Search(len(filtered), func(i int) bool {
			return compareKey(filtered[i].Name, filtered[i].Architecture, cur.name, cur.arch) > 0
		})
	}

	end := start + q.Limit
	if end > len(filtered) {
		end = len(filtered)
	}
	page := filtered[start:end]
	var next *string
	if end < len(filtered) && len(page) > 0 {
		last := page[len(page)-1]
		c := encodeCursor(last.Name, last.Architecture)
		next = &c
	}

	out := List{
		CollectedAt:    snap.CollectedAt,
		IndexUpdatedAt: snap.IndexUpdatedAt,
		Counts:         counts,
		Filters: Filters{
			Query:      q.Query,
			Installed:  q.Installed,
			Upgradable: q.Upgradable,
			Security:   q.Security,
		},
		Matched:    len(filtered),
		Limit:      q.Limit,
		NextCursor: next,
		Packages:   page,
		Errors:     snap.Errors,
	}
	if out.Packages == nil {
		out.Packages = []Entry{}
	}
	if out.Errors == nil {
		out.Errors = []apt.SourceError{}
	}
	return out, nil
}

// Updates 返回可升级清单：安全更新在前，其余按包名升序。
func (s *Service) Updates(ctx context.Context) (Updates, error) {
	snap, err := s.source.Packages(ctx, false)
	if err != nil {
		return Updates{}, err
	}
	all := make([]UpdateEntry, 0, len(snap.Packages))
	total, security := 0, 0
	for _, p := range snap.Packages {
		e := judge(p)
		if !e.Upgradable {
			continue
		}
		total++
		if e.Security {
			security++
		}
		all = append(all, UpdateEntry{
			Name:             e.Name,
			Architecture:     e.Architecture,
			InstalledVersion: e.InstalledVersion,
			CandidateVersion: *e.CandidateVersion,
			Security:         e.Security,
			Suites:           e.Suites,
			Pockets:          e.Pockets,
		})
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].Security != all[j].Security {
			return all[i].Security
		}
		if all[i].Name != all[j].Name {
			return all[i].Name < all[j].Name
		}
		return all[i].Architecture < all[j].Architecture
	})
	truncated := false
	if len(all) > MaxUpdates {
		all = all[:MaxUpdates]
		truncated = true
	}
	out := Updates{
		CollectedAt:    snap.CollectedAt,
		IndexUpdatedAt: snap.IndexUpdatedAt,
		Total:          total,
		Security:       security,
		Truncated:      truncated,
		Updates:        all,
		Errors:         snap.Errors,
	}
	if out.Updates == nil {
		out.Updates = []UpdateEntry{}
	}
	if out.Errors == nil {
		out.Errors = []apt.SourceError{}
	}
	return out, nil
}

// PowerState 返回重启标志。不缓存：这几个值必须就是此刻的。
func (s *Service) PowerState(ctx context.Context) (PowerStateView, error) {
	st, err := s.source.PowerState(ctx)
	if err != nil {
		return PowerStateView{}, err
	}
	out := PowerStateView{
		CollectedAt:                 st.CollectedAt,
		RebootRequired:              st.RebootRequired,
		RebootRequiredSince:         st.RebootRequiredSince,
		RebootRequiredPackages:      st.RebootRequiredPackages,
		RebootRequiredPackagesError: st.RebootRequiredPackagesError,
		BootID:                      st.BootID,
		BootIDError:                 st.BootIDError,
	}
	if out.RebootRequiredPackages == nil {
		out.RebootRequiredPackages = []string{}
	}
	return out, nil
}

// ValidateListQuery 校验并补齐默认值（Limit/Cursor/Query）。
func ValidateListQuery(q *ListQuery) error {
	if q.Limit == 0 {
		q.Limit = DefaultLimit
	}
	if q.Limit < 0 || q.Limit > MaxLimit {
		return fmt.Errorf("%w: limit 必须在 1..%d 之间", ErrInvalidQuery, MaxLimit)
	}
	if len(q.Query) > MaxQueryLen {
		return fmt.Errorf("%w: query 最长 %d 字节", ErrInvalidQuery, MaxQueryLen)
	}
	for i := 0; i < len(q.Query); i++ {
		c := q.Query[i]
		if c < 0x20 || c > 0x7e {
			return fmt.Errorf("%w: query 只允许可打印 ASCII", ErrInvalidQuery)
		}
	}
	if q.Cursor != "" {
		if _, err := decodeCursor(q.Cursor); err != nil {
			return err
		}
	}
	return nil
}

// judge 把一个包的**事实**变成判断。
//
// 三个判断，每个都能举出一台真机上真实的反例：
//
//   - candidate_relation：Debian 版本序（epoch → upstream → revision，`~` 排在空之前）。
//     见过 `2:12.3.5-3~ubuntu0.22.04.3` → `.4` 这种光看数字会判错的字符串。
//   - security：只看**候选版本自己**的 suite。22.04 上 apparmor 的候选来自
//     jammy-updates，而 jammy-security 里放的是更旧的那个版本；拿"security 源里
//     有没有这个包"来判，会把一票非安全更新算成安全更新。
//   - pockets：由 suite 名判定，认不出的形状落 other，不猜成 release。
func judge(p apt.Package) Entry {
	e := Entry{
		Name:             p.Name,
		Architecture:     p.Architecture,
		Status:           p.Status,
		Installed:        isInstalledStatus(p.Status),
		InstalledVersion: p.InstalledVersion,
		Suites:           p.CandidateSuites,
	}
	if e.Suites == nil {
		e.Suites = []string{}
	}
	e.Pockets = pocketsOf(p.CandidateSuites)

	if p.CandidateVersion != "" {
		v := p.CandidateVersion
		e.CandidateVersion = &v
		if cmp, err := debversion.CompareStrings(p.CandidateVersion, p.InstalledVersion); err != nil {
			// 版本串解析不了（第三方包常见），关系就是"不知道"，
			// 不能猜成 newer——那会让一个根本没升级的包显示成可升级。
			e.CandidateRelation = RelationUnknown
		} else {
			switch {
			case cmp > 0:
				e.CandidateRelation = RelationNewer
			case cmp < 0:
				e.CandidateRelation = RelationOlder
			default:
				e.CandidateRelation = RelationSame
			}
		}
	} else {
		// 候选读不到，或者 APT 明确说没有候选（本地 dpkg -i 装的包）。
		// 两种都不是"候选等于已安装"，所以不给 candidate_version。
		e.CandidateRelation = RelationUnknown
	}

	e.Upgradable = e.CandidateRelation == RelationNewer
	e.Security = e.Upgradable && containsPocket(e.Pockets, PocketSecurity)
	return e
}

// isInstalledStatus 判断 dpkg 的三字符状态码。
//
// `${db:Status-Abbrev}` 的第一位是期望状态、第二位是当前状态；`ii` 才是
// "要装着、确实装着"。`rc`（已删除、只剩配置文件）不是已安装。
func isInstalledStatus(status string) bool {
	s := strings.TrimSpace(status)
	if len(s) < 2 {
		return false
	}
	return s[0] == 'i' && s[1] == 'i'
}

// pocketsOf 把 suite 名映射成口袋。
//
// 规则只看最后一段：
//
//	jammy              → release（没有后缀的本身就发行版名）
//	jammy-updates      → updates
//	jammy-security     → security
//	jammy-backports    → backports
//	jammy-proposed     → proposed
//	bookworm-fasttrack → other（认得形状但不是已知口袋，绝不猜成 release）
func pocketsOf(suites []string) []string {
	seen := map[string]bool{}
	for _, s := range suites {
		seen[pocketOfSuite(s)] = true
	}
	out := make([]string, 0, len(seen))
	for _, name := range pocketOrder {
		if seen[name] {
			out = append(out, name)
		}
	}
	return out
}

func pocketOfSuite(suite string) string {
	if suite == "" {
		return PocketOther
	}
	// 带 `/` 或 `.` 的不是 suite 名，是仓库路径（flat 仓库的 `./`、`stable/`），
	// 这种源没有口袋概念，落 other，不猜成 release。
	if strings.ContainsAny(suite, "/.") {
		return PocketOther
	}
	i := strings.LastIndexByte(suite, '-')
	if i < 0 {
		return PocketRelease
	}
	switch suite[i+1:] {
	case "updates":
		return PocketUpdates
	case "security":
		return PocketSecurity
	case "backports":
		return PocketBackports
	case "proposed":
		return PocketProposed
	}
	return PocketOther
}

func containsPocket(hay []string, needle string) bool {
	for _, p := range hay {
		if p == needle {
			return true
		}
	}
	return false
}

// 游标编解码。名字里可能有 `:`（`libssl3:amd64`），所以不用分隔符拼接后
// 直接暴露给前端——base64url 里塞 name\0arch，解码时校验形状。
type cursorKey struct {
	name string
	arch string
}

func encodeCursor(name, arch string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(name + "\x00" + arch))
}

func decodeCursor(s string) (cursorKey, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return cursorKey{}, fmt.Errorf("%w: 不是 base64url", ErrInvalidCursor)
	}
	parts := strings.SplitN(string(raw), "\x00", 2)
	if len(parts) != 2 || parts[0] == "" {
		return cursorKey{}, fmt.Errorf("%w: 解出来的不是 (包名, 架构)", ErrInvalidCursor)
	}
	return cursorKey{name: parts[0], arch: parts[1]}, nil
}

// compareKey 按 (name, architecture) 的字典序比较，返回 -1/0/1。
func compareKey(name, arch, otherName, otherArch string) int {
	if name != otherName {
		if name < otherName {
			return -1
		}
		return 1
	}
	if arch != otherArch {
		if arch < otherArch {
			return -1
		}
		return 1
	}
	return 0
}
