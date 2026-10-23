package proxies

import (
	"context"
	"fmt"
	"strings"

	"upanel/internal/nginx"
)

// FactsSource 提供主机上 Nginx 的现状。生产实现是 *nginx.Collector。
//
// 两个方法的分工是本包有意要求的：展示用 Snapshot（有缓存，快），判断用 Collect
// （不吃缓存，准）。写操作前的冲突判断必须走 Collect——缓存窗口里正好可能有人手工
// 加了一条 server_name，而那是唯一需要判准的时刻。
type FactsSource interface {
	Snapshot(ctx context.Context) (nginx.Facts, error)
	Collect(ctx context.Context) (nginx.Facts, error)
	Invalidate()
}

// ConflictState 是一个域名的可用性结论。
type ConflictState string

const (
	// ConflictClear 表示没有任何现有配置声明这个域名。
	ConflictClear ConflictState = "clear"
	// ConflictTaken 表示已经有配置声明了这个域名。
	ConflictTaken ConflictState = "taken"
	// ConflictUnknown 表示判不了：读不到配置、有文件解析失败、或有写法认不出的
	// server_name。判不了不能当“没冲突”用。
	ConflictUnknown ConflictState = "unknown"
)

// ConflictHit 是一条“某个文件的第几行声明了这个域名”的证据。
type ConflictHit struct {
	File string
	Line int
	// Name 是文件里原本写的 server_name 原文。
	Name string
	// Managed 表示这条声明来自面板自己的文件（upanel-*.conf）。
	Managed bool
}

// ConflictReport 是“这个域名现在能不能用”的完整答案。
//
// 除了结论本身，它还带上判断依据：读到几份配置、哪些文件读不了、Nginx 装没装。
// 理由和结论一起返回，是因为用户看到的永远是“为什么不行”，不是“不行”。
type ConflictReport struct {
	// Domain 是归一化后的域名。
	Domain string
	// State 是结论。
	State ConflictState
	// Reason 是给人看的一句话说明（结论为 clear 时可能是空串）。
	Reason string

	// Matches 是占住了这个域名的声明。
	Matches []ConflictHit
	// Undecidable 是写法判不了、可能占住这个域名的声明。
	Undecidable []ConflictHit
	// BrokenFiles 是解析失败的文件：它们里面写了什么不知道。
	BrokenFiles []string
	// ExistingSiteID 是面板自己已有的同域名记录（修改自己时不算冲突）。
	ExistingSiteID string

	// NginxAvailable / NginxReason / NginxVersion / NginxUnitState 是主机现状摘要。
	NginxAvailable bool
	NginxReason    string
	NginxVersion   string
	NginxUnitState string
	// NginxConfigMissing 表示主机上根本没有 Nginx 配置目录。
	NginxConfigMissing bool
	// ManagedFileCount 是面板自己的配置文件数，供“面板接管了几个站点”展示。
	ManagedFileCount int
}

// Blocking 表示这个结论是否应该阻止写入。
func (r ConflictReport) Blocking() bool { return r.State != ConflictClear }

// ConflictError 是“域名不可用”的错误，带完整报告。
type ConflictError struct {
	Report ConflictReport
}

func (e *ConflictError) Error() string {
	r := e.Report
	switch r.State {
	case ConflictTaken:
		return fmt.Sprintf("域名 %s 已被现有配置占用：%s", r.Domain, describeHits(r.Matches))
	case ConflictUnknown:
		return fmt.Sprintf("无法判断域名 %s 是否可用：%s", r.Domain, r.Reason)
	default:
		return fmt.Sprintf("域名 %s 可用", r.Domain)
	}
}

// describeHits 把证据折成“文件第几行”的一句话。
func describeHits(hits []ConflictHit) string {
	parts := make([]string, 0, len(hits))
	for i, h := range hits {
		if i == 4 {
			parts = append(parts, fmt.Sprintf("等 %d 处", len(hits)))
			break
		}
		parts = append(parts, fmt.Sprintf("%s 第 %d 行（%s）", h.File, h.Line, h.Name))
	}
	return strings.Join(parts, "；")
}

// CheckDomain 判断一个域名现在能不能用（selfID 是自己那条记录的 id，新建时传空）。
//
// selfID 存在的意义是修改：把自己的域名从 a.com 改成 a.com 不该报“被自己占用”。
//
// 它做两件 JudgeDomain 不做的事：归一化用户输入、查面板自己有没有同名记录。
// 判断本身在 JudgeDomain 里，因为列表页要拿同一份事实一口气判 N 个域名——
// 每个站点去读一次 Nginx，既慢又会让同一页里前后两条记录的依据不是同一时刻。
func (s *Service) CheckDomain(ctx context.Context, domain, selfID string) (ConflictReport, error) {
	normalized, err := NormalizeDomain(domain)
	if err != nil {
		return ConflictReport{}, err
	}
	report := ConflictReport{Domain: normalized}

	existing, err := s.siteIDByDomain(ctx, normalized)
	if err != nil {
		return ConflictReport{}, err
	}
	if existing != "" && existing != selfID {
		report.ExistingSiteID = existing
	}

	if s.facts == nil {
		report.State = ConflictUnknown
		report.Reason = "面板没有连上代理，读不到 Nginx 现状"
		return report, nil
	}
	facts, err := s.facts.Collect(ctx)
	if err != nil {
		// 读失败要说出来，但不把 io 错误当用户错误：调用方据此返回 503。
		report.State = ConflictUnknown
		report.Reason = "读 Nginx 现状失败：" + err.Error()
		return report, fmt.Errorf("%w: %v", ErrFactsUnavailable, err)
	}

	judged := JudgeDomain(facts, normalized)
	judged.ExistingSiteID = report.ExistingSiteID
	if judged.ExistingSiteID != "" {
		// 面板自己的记录与 Nginx 配置同样是“已占用”：谁先占的不重要，
		// 重要的是这个域名不能再给第二个站点。
		judged.State = ConflictTaken
		judged.Reason = "面板里已经有一条站点记录用了这个域名（" + judged.ExistingSiteID + "）"
	}
	return judged, nil
}

// JudgeDomain 用一份已经采集到的 Nginx 事实判断域名可用性。它不做任何 I/O。
//
// 三种结论的处理方式不同，这是本函数唯一的重点：
//   - clear：可以写。
//   - taken：拒绝，并指出是谁占的（文件、行、原文）。
//   - unknown：也拒绝。读不到 Nginx、有文件解析失败、有正则 server_name——这三种
//     情况下“没找到”和“没有”是两件事。nginx reload 不会因为 server_name 撞车而失败，
//     它只会安静地用第一个块，所以这里必须比 nginx 更严格。
func JudgeDomain(facts nginx.Facts, domain string) ConflictReport {
	return JudgeDomainIgnoring(facts, domain, "")
}

// JudgeDomainIgnoring 与 JudgeDomain 相同，但忽略某一份文件里的声明（P10.2）。
//
// 用途只有一个：把一条站点写进它自己的托管文件时，那份文件里已经有同名
// `server_name`——那是上一次写下去的我们自己的成果，不是“被别人占用”。
// 忽略的是**路径精确相等**的那一份文件，不是“所有 managed 文件”：两份面板文件
// 抢同一个域名依然是冲突，那个判断和 P10.1 一样拦。
func JudgeDomainIgnoring(facts nginx.Facts, domain, ignoreFile string) ConflictReport {
	report := ConflictReport{
		Domain:             domain,
		NginxAvailable:     facts.Available,
		NginxReason:        facts.Reason,
		NginxVersion:       facts.Version,
		NginxUnitState:     facts.UnitState,
		NginxConfigMissing: facts.ConfigMissing,
		BrokenFiles:        facts.BrokenFiles(),
	}
	if ignoreFile != "" {
		// 自己那份文件坏了也不能把自己的重写挡死：坏文件正是这次要整份覆盖掉的
		// 东西（上一次写到一半、或者有人手工改坏了）。别人的坏文件照旧让结论变成
		// unknown——那不是我们能修的东西。
		kept := report.BrokenFiles[:0]
		for _, f := range report.BrokenFiles {
			if f != ignoreFile {
				kept = append(kept, f)
			}
		}
		report.BrokenFiles = kept
	}
	for _, f := range facts.Files {
		if f.Managed {
			report.ManagedFileCount++
		}
	}

	claims := facts.Lookup(domain)
	for _, c := range claims.Matches {
		if ignoreFile != "" && c.File == ignoreFile {
			continue
		}
		report.Matches = append(report.Matches, ConflictHit{
			File: c.File, Line: c.Line, Name: c.Name.Raw, Managed: c.Managed,
		})
	}
	for _, c := range claims.Undecidable {
		if ignoreFile != "" && c.File == ignoreFile {
			continue
		}
		report.Undecidable = append(report.Undecidable, ConflictHit{
			File: c.File, Line: c.Line, Name: c.Name.Raw, Managed: c.Managed,
		})
	}

	switch {
	case len(report.Matches) > 0:
		report.State = ConflictTaken
		report.Reason = "现有配置已经声明了这个域名：" + describeHits(report.Matches)
	case len(report.BrokenFiles) > 0:
		report.State = ConflictUnknown
		report.Reason = "这些配置解析失败，里面写了什么不知道：" + strings.Join(report.BrokenFiles, "、")
	case len(report.Undecidable) > 0:
		report.State = ConflictUnknown
		report.Reason = "有无法判定的 server_name（正则或不支持的写法），请人工确认：" +
			describeHits(report.Undecidable)
	case !facts.Available && !facts.ConfigMissing:
		report.State = ConflictUnknown
		report.Reason = facts.Reason
	case !facts.Available:
		// 配置目录不存在：主机上确实没有 Nginx 配置，因此确实没有域名冲突。
		report.State = ConflictClear
		report.Reason = facts.Reason
	default:
		report.State = ConflictClear
	}
	return report
}

// requireDomainFree 是写入前的门禁：域名不可用就返回 *ConflictError。
func (s *Service) requireDomainFree(ctx context.Context, domain, selfID string) error {
	if s.facts == nil {
		// 没有代理就没有事实。这不是“域名不可用”，是“这台面板现在判断不了”，
		// 所以回 503 而不是 400——前者让用户重试，后者让他去改一个没问题的域名。
		return fmt.Errorf("%w: 面板没有连上代理", ErrFactsUnavailable)
	}
	report, err := s.CheckDomain(ctx, domain, selfID)
	if err != nil {
		return err
	}
	if report.Blocking() {
		return &ConflictError{Report: report}
	}
	return nil
}
