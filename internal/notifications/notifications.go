// Package notifications 是告警通知的唯一入口（P13.3）。
//
// 它替告警事件做三件事：**决定发给谁**（通道）、**记住发没发成**（投递账）、
// 以及**在失败时按上限重试**（调度）。
//
// 三条贯穿整个包的口径：
//
//  1. **秘密只经引用，明文不出代理。** SMTP 口令与 webhook 令牌存在代理的加密库里，
//     面板库这一侧只有 secret_ref。真正把这两样东西拿去用的也是代理（见
//     internal/agent/notify.go）——面板进程（对网络暴露的那一个）从头到尾拿不到明文。
//     这条决定了“谁发送”：不是“面板想发就能发”，而是“面板只能请代理按已校验的参数发”。
//
//  2. **投递账是事实，不是状态机。** 一次投递的成功/失败/重试都记在同一行上（attempts
//     递增、last_error 覆盖、state 迁移），不在事件行上记“最后一次结果”——一条事件要
//     发给 n 个通道，每个通道各自成功各自失败，挂在事件行上就只能记住最后一个。
//
//  3. **发不出去不能无限重试。** max_attempts 是硬上限，到点就 failed 并把原因留在
//     last_error 里。一台配错的 SMTP 会让重试队列永远有活干，那时真正的问题不是
//     “通知没发出去”（人早晚会从面板上看见），而是“日志里天天是同一条错”。
package notifications

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"upanel/internal/store"
)

// 通道类型的闭集，与 migrations/0013_notification_channels.sql 的 CHECK 一致。
const (
	// TypeInApp 是面板内通知：不出门，没有秘密，出现即送达。
	TypeInApp = "in_app"
	// TypeSMTP 是邮件。
	TypeSMTP = "smtp"
	// TypeWebhook 是 HTTPS 回调。
	TypeWebhook = "webhook"
)

// 投递状态的闭集。
const (
	// DeliveryPending 表示等着发（或等着下一次重试）。
	DeliveryPending = "pending"
	// DeliverySent 表示已送达。
	DeliverySent = "sent"
	// DeliveryFailed 表示放弃（重试次数用尽，或这个错误根本不值得重试）。
	DeliveryFailed = "failed"
)

// 投递类型的闭集。
const (
	// KindFired 是“开始超限”。
	KindFired = "fired"
	// KindResolved 是“已经恢复”。
	KindResolved = "resolved"
	// KindTest 是显式测试发送（不参与去重，可以发很多次）。
	KindTest = "test"
)

// TLS 模式的闭集。**没有**“不加密”这一项：明文 SMTP 意味着口令在网线上裸奔。
const (
	// TLSStartTLS 是 587 上先明文握手、再 STARTTLS 升级。
	TLSStartTLS = "starttls"
	// TLSImplicit 是 465 上从第一个字节就是 TLS。
	TLSImplicit = "implicit"
)

// 取值边界。
const (
	// MaxNameLen 是通道名上限，与迁移里的 CHECK 一致。
	MaxNameLen = 120
	// MaxChannels 是通道总数上限。
	//
	// 一条事件要发给每个启用通道，发送是串行的（代理一次一条）。20 条通道已经远超
	// 一台服务器该有的“告警出口”数量；再多说明该做的是合并收件人，不是加通道。
	MaxChannels = 20
	// MaxRecipients 是一封邮件的收件人上限。
	MaxRecipients = 16
	// MaxURLLen 是 webhook 地址上限。
	MaxURLLen = 1024
	// DefaultMaxAttempts 是新建通道的失败重试上限（PLAN.md 第 18 节要求显式定义）。
	DefaultMaxAttempts = 3
	// MaxAttempts 是允许配置的重试上限。
	MaxAttempts = 10
	// MaxErrorLen 是 last_error 上限，与迁移里的 CHECK 一致。
	MaxErrorLen = 500
	// MaxTitleLen 是投递标题上限，与迁移里的 CHECK 一致。
	MaxTitleLen = 200
	// MaxBodyLen 是投递正文上限，与迁移里的 CHECK 一致。
	MaxBodyLen = 4000
	// retryBaseDelay 是第一次重试的等待时间；之后按 attempts 指数退避。
	//
	// 30 秒起步：SMTP 与 webhook 的瞬时失败（连接被拒、对端重启、网络抖动）几乎都在
	// 一两分钟内自愈；一上来就退避十分钟，等于在故障最需要通知的那段时间里最安静。
	retryBaseDelay = 30 * time.Second
	// maxRetryDelay 是退避的上限。
	maxRetryDelay = 10 * time.Minute
	// dispatchInterval 是调度循环的节拍。
	//
	// 5 秒一次是“够快但不空转”：投递不是实时系统，晚 5 秒发出去和立刻发出去的差别
	// 没人能感知，而 1 秒一次会让一台空转的面板每分钟做 60 次空查询。
	dispatchInterval = 5 * time.Second
	// dispatchBatch 是一轮最多处理的投递数。
	dispatchBatch = 20
)

// 错误。
var (
	// ErrNotFound 表示通道或投递不存在。
	ErrNotFound = errors.New("notifications: 对象不存在")
	// ErrVersionConflict 表示 version 不匹配（通道已被别人改过）。
	ErrVersionConflict = errors.New("notifications: 通道已被修改，请重新读取后再提交")
	// ErrLimitReached 表示通道数量达到上限。
	ErrLimitReached = errors.New("notifications: 通道数量已达上限")
	// ErrSenderUnavailable 表示没有可用的发送端（演示模式没有代理）。
	ErrSenderUnavailable = errors.New("notifications: 发送通道不可用（未连接代理）")
	// ErrNotSendable 表示这类通道不接受“发送”动作（in_app 就不需要发）。
	ErrNotSendable = errors.New("notifications: 面板内通知不需要发送")
)

// InvalidChannelError 是一条通道不合法。Field 用来在接口层指出是哪一项。
type InvalidChannelError struct {
	Field   string
	Message string
}

func (e *InvalidChannelError) Error() string {
	if e.Field == "" {
		return "notifications: " + e.Message
	}
	return "notifications: " + e.Field + " " + e.Message
}

// IsInvalidChannel 判断错误是否属于通道非法。
func IsInvalidChannel(err error) bool {
	var target *InvalidChannelError
	return errors.As(err, &target)
}

func invalid(field, format string, args ...any) error {
	return &InvalidChannelError{Field: field, Message: fmt.Sprintf(format, args...)}
}

// Settings 是受约束的通道参数。
//
// **一个结构体装三种通道**，而不是每类一个接口：这些字段一一对应页面上的输入框，
// 用同一个类型能让“保存前后一致”（解码 → 校验 → 重新序列化）只写一遍。库里因此
// 永远不会留下本进程不认识的键——这一条比建表时校验 JSON 重要得多。
type Settings struct {
	// ── smtp ──
	Host     string   `json:"host,omitempty"`
	Port     int      `json:"port,omitempty"`
	TLS      string   `json:"tls,omitempty"`
	From     string   `json:"from,omitempty"`
	To       []string `json:"to,omitempty"`
	Username string   `json:"username,omitempty"`
	// ── webhook ──
	URL          string `json:"url,omitempty"`
	AllowPrivate bool   `json:"allow_private,omitempty"`
}

// Channel 是一条通知通道。
type Channel struct {
	ID        int64
	Name      string
	Type      string
	Settings  Settings
	SecretRef string
	Enabled   bool
	Version   int64
	// MaxAttempts 是这条通道的失败重试上限（存在 settings 之外，因为它对三类通道
	// 都成立，而 settings 是按类型解释的）。
	MaxAttempts int
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Target 返回“发到哪”的展示串（写进投递账的 target 快照）。
//
// 它永远不含秘密：webhook 的 URL 里如果有 userinfo 就只留主机部分，
// 邮件只列收件人。
func (c Channel) Target() string {
	switch c.Type {
	case TypeSMTP:
		return strings.Join(c.Settings.To, ",")
	case TypeWebhook:
		return c.Settings.URL
	default:
		return ""
	}
}

// ChannelInput 是新建/更新通道的输入。
type ChannelInput struct {
	Name        string
	Type        string
	Settings    Settings
	SecretRef   string
	Enabled     bool
	MaxAttempts int
}

// Service 是通知通道与投递账的唯一入口。接口层不得直接写这两张表。
type Service struct {
	db     *store.DB
	now    func() time.Time
	logger *slog.Logger

	senderMu sync.RWMutex
	sender   Sender

	logOnceMu  sync.Mutex
	lastLogErr string
	lastLogAt  time.Time
}

// Options 是 Service 的构造参数。
type Options struct {
	// Now 注入时钟（测试用）；nil 时用 time.Now。
	Now func() time.Time
	// Logger 可选。调度与投递失败要靠它留痕——通知发不出去时，日志是唯一
	// “有人知道这件事没成”的地方。
	Logger *slog.Logger
}

// New 构造 Service。db 为必需依赖。
func New(db *store.DB, opts Options) (*Service, error) {
	if db == nil {
		return nil, errors.New("notifications: 数据库句柄不能为空")
	}
	s := &Service{db: db, now: opts.Now, logger: opts.Logger}
	if s.now == nil {
		s.now = time.Now
	}
	return s, nil
}

// SetSender 接上发送端（代理客户端）。演示模式下不调用，投递会一直停在 pending
// 并把原因说清楚，而不是“假装发过了”。
func (s *Service) SetSender(sender Sender) {
	s.senderMu.Lock()
	s.sender = sender
	s.senderMu.Unlock()
}

func (s *Service) currentSender() Sender {
	s.senderMu.RLock()
	defer s.senderMu.RUnlock()
	return s.sender
}

func (s *Service) nowUTC() time.Time { return s.now().UTC().Truncate(time.Second) }

// Now 返回服务的当前时刻（UTC）。接口层拼“下次重试在什么时候”要用同一个时钟。
func (s *Service) Now() time.Time { return s.nowUTC() }

// InTransaction 在一个事务里执行 fn，供接口层把“通道变更 + 审计”合成一笔事务。
func (s *Service) InTransaction(ctx context.Context, fn func(ctx context.Context) error) error {
	if fn == nil {
		return errors.New("notifications: InTransaction 需要非空函数")
	}
	return s.db.WithTx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		return fn(ctx)
	})
}

// −−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−
// 小工具
// −−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−−

// scanner 是 *sql.Row 与 *sql.Rows 的共同读法。
type scanner interface {
	Scan(dest ...any) error
}

func (s *Service) queryRow(ctx context.Context, query string, args ...any) *sql.Row {
	if tx, ok := store.TxFromContext(ctx); ok {
		return tx.QueryRowContext(ctx, query, args...)
	}
	return s.db.SQL().QueryRowContext(ctx, query, args...)
}

func (s *Service) query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if tx, ok := store.TxFromContext(ctx); ok {
		return tx.QueryContext(ctx, query, args...)
	}
	return s.db.SQL().QueryContext(ctx, query, args...)
}

func (s *Service) exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if tx, ok := store.TxFromContext(ctx); ok {
		return tx.ExecContext(ctx, query, args...)
	}
	return s.db.SQL().ExecContext(ctx, query, args...)
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func parseTime(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("notifications: 时间格式无效: %w", err)
	}
	return t.UTC(), nil
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

// truncate 把字符串截到 n 个字符（按 rune，不是字节），并在截断处加省略号。
func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n-1]) + "…"
}

// validateURL 校验 webhook 地址：**只接受 https**。
//
// 这条不是“建议”，是 PLAN.md 第 18 节“1.0 支持 SMTP 邮件和 HTTPS webhook”的
// 直接后果：webhook 里带的是告警正文（哪台机器、什么指标、什么时候），明文 HTTP
// 等于把内网拓扑广播到路径上的每一跳。拒绝 userinfo 是因为它会被写进日志与投递账，
// 而“把口令塞进 URL”是最常见的走捷径方式。拒绝 fragment 是因为它不会被发出去，
// 留着只会让配置看起来生效了其实没有。
func validateURL(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, invalid("settings.url", "不能为空")
	}
	if len(raw) > MaxURLLen {
		return nil, invalid("settings.url", "长度不能超过 %d", MaxURLLen)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, invalid("settings.url", "不是合法的地址：%v", err)
	}
	if u.Scheme != "https" {
		return nil, invalid("settings.url", "只接受 https（收到 %q）：告警正文不能走明文", u.Scheme)
	}
	if u.Host == "" {
		return nil, invalid("settings.url", "缺少主机名")
	}
	if u.User != nil {
		return nil, invalid("settings.url", "不接受地址里的用户名口令：令牌请用 secret_ref 单独保存")
	}
	if u.Fragment != "" {
		return nil, invalid("settings.url", "不接受 #片段（它不会被发出去）")
	}
	return u, nil
}

// looksLikeAddress 做邮件地址的最低限度检查：有且只有一个 @，两侧非空，不含空白与
// 换行。**不追求 RFC 5322 全覆盖**——那件工作没有一个实现的边界是对的，而这里要挡的是
// 两类真实错误：拼错（少了 @）与头注入（换行后面跟 Bcc）。
func looksLikeAddress(addr string) bool {
	if addr == "" || len(addr) > 254 {
		return false
	}
	if strings.ContainsAny(addr, " \t\r\n<>\"") {
		return false
	}
	at := strings.IndexByte(addr, '@')
	if at <= 0 || at != strings.LastIndexByte(addr, '@') {
		return false
	}
	if at == len(addr)-1 {
		return false
	}
	return true
}

// cleanHeaderValue 去掉会破坏邮件头的字符。标题里出现换行就等于可以注入任意头。
func cleanHeaderValue(v string) string {
	v = strings.ReplaceAll(v, "\r", " ")
	v = strings.ReplaceAll(v, "\n", " ")
	return strings.TrimSpace(v)
}
