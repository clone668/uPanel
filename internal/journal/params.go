// Package journal 是 journald 的结构化日志读取层（P05.3）。
//
// 边界来自 PLAN.md 第 14 节与 CONTRACTS.md 第 10 节：
//   - 按 unit、时间、级别与游标查询；不接受任意 journalctl 参数，也不接受任何
//     “把这段字符串当命令”的入口（argv 由本包按固定顺序拼出，逐项传参）；
//   - 限行（≤1000）、限字节（单页 1 MiB、单条消息 8 KiB）、限时间范围（≤24 小时）；
//   - 实时流断开时释放子进程与订阅资源；
//   - 消息里的秘密按 logging 的规则**尽力**抹掉，并把“抹过的次数”如实标出来。
//
// 本包只读：它执行的唯一程序是 journalctl，唯一允许的子命令是列表与跟随。
package journal

import (
	"errors"
	"fmt"
	"regexp"
	"time"
)

// 数据源闭集。CONTRACTS.md 第 10 节要求 /logs/stream 使用固定数据源白名单：
// 前端只能在这两个值里选，服务端不认识的值一律拒绝。
const (
	// SourceJournal 是 systemd journal 的全部条目（可按 unit 收窄）。
	SourceJournal = "journal"
	// SourceKernel 是内核日志（journalctl -k）。
	SourceKernel = "kernel"
)

// 限额。数值本身就是产品行为，不通过配置放宽（PLAN.md 第 14 节）。
const (
	// DefaultLimit 是未指定 limit 时返回的条数。
	DefaultLimit = 100
	// MaxLimit 是单页最多返回的条数（CONTRACTS.md 第 10 节：最多 1000 行）。
	MaxLimit = 1000
	// DefaultWindow 是只给到 to（或什么都不给）时向前回溯的时间窗口。
	DefaultWindow = time.Hour
	// MaxSpan 是 from—to 允许的最大跨度（CONTRACTS.md 第 10 节：最多 24 小时）。
	MaxSpan = 24 * time.Hour
	// MaxPageBytes 是单页消息字节的软上限（CONTRACTS.md 第 10 节：1 MiB）。
	// 它是**消息内容**的累计，不含 JSON 外壳；用来让“一次请求最多占多少内存”有明确答案。
	MaxPageBytes = 1 << 20
	// MaxMessageBytes 是单条消息保留的字节数，超出部分丢弃并标记 truncated。
	MaxMessageBytes = 8 << 10
	// MaxCursorBytes 是游标长度上限。真实游标约 120 字节，500 已经足够宽松。
	MaxCursorBytes = 512
	// maxUnitBytes 是 unit 名长度上限（systemd 的限制是 256）。
	maxUnitBytes = 256
	// FutureSkew 是 to 允许超出当前时间的容差（时钟微调与请求在途）。
	FutureSkew = 2 * time.Minute
)

// ErrInvalidParams 表示查询参数不合法。上层据此回 400，而不是把它当成内部错误。
var ErrInvalidParams = errors.New("journal: 查询参数不合法")

// Level 是 syslog 级别（journald 的 PRIORITY）在 API 里的名字。
//
// 用完整英文名而不是数字：数字对人不友好，也容易被误当成“第几级”。
// 过滤语义是 journalctl 的 --priority：**该级别及以上**（更严重）都被包含。
type Level string

// 级别闭集，顺序与 syslog 一致（0 最严重）。
const (
	LevelEmergency Level = "emergency"
	LevelAlert     Level = "alert"
	LevelCritical  Level = "critical"
	LevelError     Level = "error"
	LevelWarning   Level = "warning"
	LevelNotice    Level = "notice"
	LevelInfo      Level = "info"
	LevelDebug     Level = "debug"
	// LevelUnknown 表示条目没有可识别的 PRIORITY。
	//
	// 它不是“info”的别名：把读不出来的级别写成 info 会让“只看 error”这类过滤
	// 在页面上看起来生效了，实际却混进了未知级别的条目。
	LevelUnknown Level = "unknown"
)

// levels 是闭集全集，供校验与遍历使用。
var levels = []Level{
	LevelEmergency, LevelAlert, LevelCritical, LevelError,
	LevelWarning, LevelNotice, LevelInfo, LevelDebug,
}

// journalPriority 把级别映射成 journalctl 的 --priority 参数值。
//
// 实测（systemd 255）：journalctl 只认 emerg/alert/crit/err/warning/notice/info/debug，
// 传 "error" 或 "warn" 会以 “Unknown log level” 退出。因此这张表是必需的，
// 不能把 API 名字直接塞给命令行。
var journalPriority = map[Level]string{
	LevelEmergency: "emerg",
	LevelAlert:     "alert",
	LevelCritical:  "crit",
	LevelError:     "err",
	LevelWarning:   "warning",
	LevelNotice:    "notice",
	LevelInfo:      "info",
	LevelDebug:     "debug",
}

// priorityLevel 是 PRIORITY 数字到级别的反向映射。
var priorityLevel = map[string]Level{
	"0": LevelEmergency,
	"1": LevelAlert,
	"2": LevelCritical,
	"3": LevelError,
	"4": LevelWarning,
	"5": LevelNotice,
	"6": LevelInfo,
	"7": LevelDebug,
}

// ParseLevel 把字符串解析成级别。空串表示“不按级别过滤”，返回 ok=false 且无错。
func ParseLevel(s string) (Level, bool, error) {
	if s == "" {
		return "", false, nil
	}
	l := Level(s)
	if _, ok := journalPriority[l]; ok {
		return l, true, nil
	}
	return "", false, fmt.Errorf("%w: level %q 不在闭集内（可选 %v）", ErrInvalidParams, s, levels)
}

// Priority 返回该级别对应的 journalctl 参数值。
func (l Level) Priority() string { return journalPriority[l] }

// LevelFromPriority 把 journald 的 PRIORITY 字符串映射成级别；未知值返回 LevelUnknown。
func LevelFromPriority(p string) Level {
	if l, ok := priorityLevel[p]; ok {
		return l
	}
	return LevelUnknown
}

// LevelValid 判断是否是已知级别（LevelUnknown 不算已知）。
func (l Level) LevelValid() bool {
	_, ok := journalPriority[l]
	return ok
}

// unitPattern 是 unit 名的形状：常规名字 + 已知 systemd 后缀。
//
// 刻意不接受 glob、空格、斜杠与任何标点：journalctl 的 -u 会展开通配符，
// 而“让调用方决定匹配模式”正是本模块要避免的自由度过高的输入。
var unitPattern = regexp.MustCompile(
	`^[A-Za-z0-9][A-Za-z0-9:_.@\\-]{0,200}\.(service|socket|timer|mount|automount|swap|target|path|slice|scope|device)$`)

// ValidUnit 判断 unit 名是否可接受。
func ValidUnit(unit string) bool {
	return len(unit) <= maxUnitBytes && unitPattern.MatchString(unit)
}

// cursorPattern 是游标的字符集。journald 的游标形如
// s=<32 位 hex>;i=<hex>;b=<32 位 hex>;m=<hex>;t=<hex>;x=<hex>。
// 只放行这些字符，其余（含换行、控制字符）一律拒绝：它不是 shell 参数，
// 但也不该被当成可以随便塞东西的字符串。
var cursorPattern = regexp.MustCompile(`^[A-Za-z0-9=;:._+\-/]{1,512}$`)

// ValidCursor 判断游标是否可接受。
func ValidCursor(cursor string) bool {
	return cursorPattern.MatchString(cursor)
}

// Params 是 /logs 的查询参数，也是代理 logs.query 的参数：两侧同一份 DTO。
//
// 时间是 RFC3339 字符串而不是 time.Time：它要经过 JSON、URL 与命令行三层，
// 字符串形式只有一个解析点（Normalize），比在各层各自处理时区更不容易出错。
type Params struct {
	// Source 是数据源，闭集 journal|kernel；空值按 journal 处理。
	Source string `json:"source,omitempty"`
	// Unit 是 unit 名，只在 source=journal 时允许。
	Unit string `json:"unit,omitempty"`
	// Level 是 syslog 级别下限，空值表示不按级别过滤。
	Level string `json:"level,omitempty"`
	// From/To 是时间范围（RFC3339，含端点）。两者都可以省略，默认回看 1 小时。
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
	// Cursor 是上一页返回的 next_cursor，语义是“从这里（含）向更早翻”。
	Cursor string `json:"cursor,omitempty"`
	// Limit 是本次最多返回的条数，缺省 100，上限 1000。
	Limit int `json:"limit,omitempty"`
}

// Normalized 是校验并补齐默认值后的参数。命令行只从它构造。
type Normalized struct {
	Source string
	Unit   string
	Level  Level
	// HasLevel 区分“没过滤”与“过滤了一个恰好是空串的级别”。
	HasLevel bool
	From     time.Time
	To       time.Time
	Cursor   string
	Limit    int
}

// Validate 只做与“现在”无关的校验：数据源、unit、级别、游标、条数与时间**格式**。
//
// 与 Normalize 分开是为了让代理在解码阶段就能回 400（那时还没有“现在”），
// 而不必等到执行时才用 500 报一个参数错误。
//
// 时间格式在这里而不是只在 Normalize 里查：`from=2026-10-21` 这种手写日期
// 必须在**进入读取端之前**就被拒绝。否则它要么一路走到 journalctl 才失败，
// 要么（更糟）被当成“没给时间”而安静地换成默认窗口——页面显示的就不是
// 用户以为的那段时间的日志了。
func (p Params) Validate() error {
	switch p.Source {
	case "", SourceJournal, SourceKernel:
	default:
		return fmt.Errorf("%w: source %q 不在白名单内（可选 %q、%q）",
			ErrInvalidParams, p.Source, SourceJournal, SourceKernel)
	}
	if p.Unit != "" {
		if p.Source == SourceKernel {
			return fmt.Errorf("%w: source=kernel 不接受 unit 过滤", ErrInvalidParams)
		}
		if !ValidUnit(p.Unit) {
			return fmt.Errorf("%w: unit %q 不是合法的单元名（需要形如 ssh.service，不接受通配符与路径）",
				ErrInvalidParams, p.Unit)
		}
	}
	if _, _, err := ParseLevel(p.Level); err != nil {
		return err
	}
	if p.Cursor != "" && !ValidCursor(p.Cursor) {
		return fmt.Errorf("%w: cursor 不是合法游标（应使用上一页返回的 next_cursor）", ErrInvalidParams)
	}
	if p.Limit < 0 || p.Limit > MaxLimit {
		return fmt.Errorf("%w: limit 必须在 1—%d 之间，当前为 %d", ErrInvalidParams, MaxLimit, p.Limit)
	}
	if p.From != "" {
		if _, err := parseTime("from", p.From); err != nil {
			return err
		}
	}
	if p.To != "" {
		if _, err := parseTime("to", p.To); err != nil {
			return err
		}
	}
	// 两端都给时，先后与跨度同样与“现在”无关，可以在这里就判掉。
	// 只给一端时窗口的边界取决于 now，那一半留给 Normalize。
	if p.From != "" && p.To != "" {
		from, _ := parseTime("from", p.From)
		to, _ := parseTime("to", p.To)
		if err := checkWindow(from, to); err != nil {
			return err
		}
	}
	return nil
}

// checkWindow 校验时间窗口的先后与跨度（两个端点都已知时使用）。
func checkWindow(from, to time.Time) error {
	if !from.Before(to) {
		return fmt.Errorf("%w: from（%s）必须早于 to（%s）",
			ErrInvalidParams, from.Format(time.RFC3339), to.Format(time.RFC3339))
	}
	if span := to.Sub(from); span > MaxSpan {
		return fmt.Errorf("%w: 时间范围 %s 超过上限 %s",
			ErrInvalidParams, span.Round(time.Second), MaxSpan)
	}
	return nil
}

// Normalize 校验并补齐默认值：数据源、时间窗口与条数。
//
// 时间规则：
//   - to 缺省取 now；from 缺省取 to-DefaultWindow；
//   - from 必须早于 to，跨度不超过 24 小时；
//   - to 不能明显超过 now（容差 FutureSkew）——未来时间窗永远返回空，
//     与其让人以为“这段时间没有日志”，不如直接说参数写错了。
func (p Params) Normalize(now time.Time) (Normalized, error) {
	if err := p.Validate(); err != nil {
		return Normalized{}, err
	}
	now = now.UTC()

	source := p.Source
	if source == "" {
		source = SourceJournal
	}
	level, hasLevel, err := ParseLevel(p.Level)
	if err != nil {
		return Normalized{}, err
	}

	to := now
	if p.To != "" {
		to, err = parseTime("to", p.To)
		if err != nil {
			return Normalized{}, err
		}
	}
	from := to.Add(-DefaultWindow)
	if p.From != "" {
		from, err = parseTime("from", p.From)
		if err != nil {
			return Normalized{}, err
		}
	}
	if err := checkWindow(from, to); err != nil {
		return Normalized{}, err
	}
	if to.After(now.Add(FutureSkew)) {
		return Normalized{}, fmt.Errorf("%w: to（%s）超过当前时间，请检查时区",
			ErrInvalidParams, to.Format(time.RFC3339))
	}

	limit := p.Limit
	if limit == 0 {
		limit = DefaultLimit
	}
	if limit < 1 || limit > MaxLimit {
		return Normalized{}, fmt.Errorf("%w: limit 必须在 1—%d 之间，当前为 %d", ErrInvalidParams, MaxLimit, limit)
	}

	return Normalized{
		Source:   source,
		Unit:     p.Unit,
		Level:    level,
		HasLevel: hasLevel,
		From:     from,
		To:       to,
		Cursor:   p.Cursor,
		Limit:    limit,
	}, nil
}

// parseTime 解析 RFC3339 时间（允许小数秒）。
func parseTime(field, value string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: %s 必须是 RFC3339 时间（例如 2026-10-21T03:00:00Z），当前为 %q",
			ErrInvalidParams, field, value)
	}
	return t.UTC(), nil
}
