package schedules

// 计划任务的定义与校验（P12.4；CONTRACTS.md 第 12 节，PLAN.md 第 17 节）。
//
// 这一层只回答三件事：**这条计划能不能被接受**（定义是否自洽）、**它在 systemd 里
// 长什么样**（units.go）、**被触发时它要提交哪个动作**（ActionFor）。
//
// 三条刻意的取舍，每一条都对应 PLAN 里一句要求：
//
//	只写自己的单元   uPanel 只渲染 upanel-sch_<id>.service/.timer 这两种名字；
//	                 现有 cron 与别人的 timer 只读展示。单元名由计划 id 派生，
//	                 不接受调用方给名字——名字一旦可写，就能覆盖别人的单元。
//	时区必须显式     OnCalendar 不带时区时按**系统本地时区**解释；同一个表达式在
//	                 两台机器上是两个时刻，而“下次几点跑”是管理员要看的数字。
//	                 因此时区是必填的 IANA 名称，并且总是拼进 OnCalendar 那一行。
//	目标只能是 id    target_id 按种类校验成 pol_/crt_/scr_ 加 32 位小写十六进制。
//	                 “能不能跑 shell 命令”这件事由格式直接否定：命令文本不是 id。
//	                 （CONTRACTS §12：脚本 target 必须指向预先登记、由 upanel-shell
//	                 运行的普通脚本，不能是 shell 命令文本。）

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"upanel/internal/scripts"
)

// 动作名（CONTRACTS.md 第 5 节，冻结）。
const (
	// ActionApply 是新建或修改一条计划的动作名。
	ActionApply = "schedule.apply"
	// ActionDelete 是删除一条计划的动作名。
	ActionDelete = "schedule.delete"
)

// 脚本动作名（CONTRACTS.md 第 5 节，冻结）。
//
// 名字归 internal/scripts 所有，这里只做别名：定时触发要落到的那三个名字与
// 脚本动作自己登记的名字必须是同一个来源，否则改名时漏一处就会静默地永不触发。
const (
	ActionScriptApply  = scripts.ActionScriptApply
	ActionScriptDelete = scripts.ActionScriptDelete
	ActionScriptRun    = scripts.ActionScriptRun
)

// 证书动作名（CONTRACTS.md 第 5 节，冻结）。同一个理由：定时续期要落到的那一个
// 名字必须就是证书模块登记的那一个。
//
// 这里只能写字面量，不能取 internal/certificates 的常量——那个包经 proxies、
// agentclient 又 import 回本包，成环。相等关系由 internal/agent 的一个测试钉住
// （那里能同时 import 两边），改名时漏一处会当场变红，而不是静默地永不触发。
const ActionCertificateRenew = "certificate.renew"

// Kind 是计划任务的种类闭集。
type Kind string

const (
	// KindBackup 定时执行一条备份策略（backup.run）。
	KindBackup Kind = "backup"
	// KindCertificateRenewal 定时续期一张证书（certificate.renew）。
	KindCertificateRenewal Kind = "certificate_renewal"
	// KindUserScript 定时以 upanel-shell 身份运行一条已登记脚本（script.run）。
	KindUserScript Kind = "user_script"
)

// Valid 判断种类是否在闭集内。
func (k Kind) Valid() bool {
	switch k {
	case KindBackup, KindCertificateRenewal, KindUserScript:
		return true
	}
	return false
}

// 上限（都是“给人写的字符串”的长度上限，不是流量控制）。
const (
	// MaxNameLen 是计划名的字符数上限。
	MaxNameLen = 80
	// MaxCalendarLen 是 OnCalendar 表达式的字节上限。
	//
	// systemd 自己的表达式很短（`Mon..Fri *-*-* 04:30:00` 是 24 字节）。128 字节足够
	// 容纳最长的合法写法，又不给“把一段脚本塞进表达式”留下空间。
	MaxCalendarLen = 128
	// MaxTimezoneLen 是时区名的字节上限（最长 IANA 名 32 字节上下）。
	MaxTimezoneLen = 64
	// MaxRuntimeSecondsLimit 是单次运行时长上限（24 小时）。
	//
	// 它同时写进单元文件的 TimeoutStartSec：一次“备份跑了三天”几乎总是挂住了，
	// 而不是真的还在干活；没有上限就等于让一个僵死的服务永远占着定时器。
	MaxRuntimeSecondsLimit = 24 * 60 * 60
)

// calendarChars 是 OnCalendar 表达式允许出现的字符集。
//
// 白名单而不是黑名单：这个字符串会原样进入 `systemd-analyze calendar` 的 argv 与
// 单元文件的 OnCalendar 行。白名单同时挡住两种事故——把 `--foo` 当成选项喂给
// systemd-analyze（句首的 `-` 因此也被单独拒绝），以及往单元文件里塞换行注入
// 另一段配置。
var calendarChars = regexp.MustCompile(`^[A-Za-z0-9 :*.,/+~-]+$`)

// timezoneChars 是时区名允许的字符集（IANA 名只有字母数字与 `_ + - /`）。
var timezoneChars = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_+/-]*$`)

// idPatterns 是各类目标 id 的形态。
var idPatterns = map[Kind]*regexp.Regexp{
	KindBackup:             regexp.MustCompile(`^pol_[0-9a-f]{32}$`),
	KindCertificateRenewal: regexp.MustCompile(`^crt_[0-9a-f]{32}$`),
	KindUserScript:         regexp.MustCompile(`^scr_[0-9a-f]{32}$`),
}

// Definition 是一条计划任务的定义（CONTRACTS §12 的 ScheduleDefinition）。
//
// 字段名与顺序即对外形状：Web 的 GET /schedules 原样返回它，代理库里的
// definition_json 也存它。两边同一个结构，不存在“翻译层”。
type Definition struct {
	Name string `json:"name"`
	Kind Kind   `json:"kind"`
	// TargetID 指向被定时的对象：备份策略 id、证书 id 或脚本 id。
	TargetID string `json:"target_id"`
	// Certificate 是证书续期计划的种类参数；只有 KindCertificateRenewal 用它。
	//
	// 参数跟着定义走，是因为**触发发生在 systemd 里，那一刻没有面板可问**：
	// 证书的账本在面板库，而“这张证书用哪个 ACME 环境签的”是它自己的属性。
	// 把它抄进定义，代理就能独立地把 certificate.renew 提交出去；不抄，定时续期
	// 就得在触发那一刻反向依赖面板，而开机时代理可能先于面板起来。
	// 反向的约束同样写在校验里：别的种类带了它就直接报错，不静默忽略。
	Certificate *CertificateParams `json:"certificate,omitempty"`
	// CalendarExpression 是 systemd 的 OnCalendar 表达式本体（不含时区）。
	CalendarExpression string `json:"calendar_expression"`
	// Timezone 是解释这个表达式用的 IANA 时区名；必填。
	Timezone string `json:"timezone"`
	// Persistent 是错过执行策略：true 表示停机期间错过的触发在开机后补跑一次。
	Persistent bool `json:"persistent"`
	// MaxRuntimeSeconds 是单次运行的时长上限；必填。
	MaxRuntimeSeconds int `json:"max_runtime_seconds"`
	// Enabled 是这个 definition 的意图；单元是否存在、是否已 enable 由版本与状态决定。
	Enabled bool `json:"enabled"`
}

// CertificateParams 是证书续期计划的两种事实：续谁、在哪个 ACME 环境续。
//
// `staging` 必须显式记下来：`certificate.renew` 是拿 `--server` 说话的动作，
// staging 证书用正式入口续会换掉一个本不该上生产的对象。这不是可以从主机状态
// 反推的东西，所以写进定义、随定义一起版本化。
type CertificateParams struct {
	Domain  string `json:"domain"`
	Staging bool   `json:"staging"`
}

// Normalize 去掉各字符串字段首尾空白。它**不**做“猜一个默认值”的事：
// 时区与时长都必须由调用方给，缺了就是校验失败。
func (d Definition) Normalize() Definition {
	return Definition{
		Name:               strings.TrimSpace(d.Name),
		Kind:               d.Kind,
		TargetID:           strings.TrimSpace(d.TargetID),
		Certificate:        d.Certificate.normalized(),
		CalendarExpression: strings.TrimSpace(d.CalendarExpression),
		Timezone:           strings.TrimSpace(d.Timezone),
		Persistent:         d.Persistent,
		MaxRuntimeSeconds:  d.MaxRuntimeSeconds,
		Enabled:            d.Enabled,
	}
}

// ValidationError 是一条带字段名的校验失败。
//
// 带字段名是因为调用方要把它变成一条**能照着改**的 400：只说“定义不合法”，
// 管理员面对九个字段无从下手。
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string { return e.Field + ": " + e.Message }

func invalid(field, msg string) error { return &ValidationError{Field: field, Message: msg} }

// Validate 校验定义自洽。调用前应先 Normalize。
//
// 它不校验“这个表达式 systemd 认不认”——那要跑 `systemd-analyze calendar`，
// 属于代理执行端的事（PLAN §17：配置 timer 前用 systemd-analyze calendar 校验）。
// 这里拦住的是形态问题：长度、字符集、必填、id 形态。
func (d Definition) Validate() error {
	// 首尾空白由 Normalize 去掉。这里按去掉后的值判断，是为了让“忘了 Normalize”
	// 不至于变成“一个全空白的计划名通过了校验”。
	name := strings.TrimSpace(d.Name)
	if n := len([]rune(name)); n == 0 {
		return invalid("name", "不能为空")
	} else if n > MaxNameLen {
		return invalid("name", fmt.Sprintf("最多 %d 个字符，收到 %d 个", MaxNameLen, n))
	}
	if strings.ContainsFunc(name, isControl) {
		return invalid("name", "不能含控制字符")
	}
	if !d.Kind.Valid() {
		return invalid("kind", "只能是 backup、certificate_renewal 或 user_script")
	}
	pattern := idPatterns[d.Kind]
	if !pattern.MatchString(d.TargetID) {
		switch d.Kind {
		case KindBackup:
			return invalid("target_id", "备份计划必须指向一条备份策略 id（pol_ 加 32 位小写十六进制），不接受命令或路径")
		case KindCertificateRenewal:
			return invalid("target_id", "证书续期计划必须指向一张证书 id（crt_ 加 32 位小写十六进制）")
		case KindUserScript:
			return invalid("target_id", "脚本计划必须指向一条已登记脚本 id（scr_ 加 32 位小写十六进制），不接受 shell 命令文本")
		}
		return invalid("target_id", "形态不合法")
	}
	switch d.Kind {
	case KindCertificateRenewal:
		if d.Certificate == nil {
			return invalid("certificate", "证书续期计划必须带证书参数（domain 与 staging）：触发发生在 systemd 里，那一刻没有面板可问")
		}
		if err := d.Certificate.validate(); err != nil {
			return err
		}
	default:
		if d.Certificate != nil {
			// 静默忽略一个用户明确写下的字段，是最难查的那类错误：请求里明明有它，
			// 保存成功，触发的却是别人。所以这里直接拒。
			return invalid("certificate", "只有证书续期计划能带证书参数（domain 与 staging）")
		}
	}
	if d.CalendarExpression == "" {
		return invalid("calendar_expression", "不能为空：没有执行时间的计划无法被渲染成 timer")
	}
	if len(d.CalendarExpression) > MaxCalendarLen {
		return invalid("calendar_expression", fmt.Sprintf("最多 %d 字节，收到 %d 字节", MaxCalendarLen, len(d.CalendarExpression)))
	}
	if !calendarChars.MatchString(d.CalendarExpression) {
		return invalid("calendar_expression",
			"只能含字母、数字、空格与 : * . , / + ~ -（这个字符串会进入单元文件与 systemd-analyze 的 argv）")
	}
	if strings.HasPrefix(d.CalendarExpression, "-") {
		// `systemd-analyze calendar --now` 不报错，会当成选项：一条“计划”就变成了
		// 一次没有任何执行时间的校验。句首的 `-` 不是任何合法日历写法的开头。
		return invalid("calendar_expression", "不能以 - 开头")
	}
	if d.Timezone == "" {
		return invalid("timezone", "必须显式指定时区：不带时区的表达式按系统本地时区解释，两台机器上是两个时刻")
	}
	if len(d.Timezone) > MaxTimezoneLen {
		return invalid("timezone", fmt.Sprintf("最多 %d 字节", MaxTimezoneLen))
	}
	if !timezoneChars.MatchString(d.Timezone) {
		return invalid("timezone", "只能含字母、数字与 _ + - /")
	}
	if d.Timezone == "Local" || d.Timezone == "localtime" {
		return invalid("timezone", "必须写 IANA 时区名（例如 Asia/Shanghai），Local 会让“下次几点跑”取决于主机配置")
	}
	if _, err := time.LoadLocation(d.Timezone); err != nil {
		return invalid("timezone", fmt.Sprintf("%q 不是可用的 IANA 时区名", d.Timezone))
	}
	if d.MaxRuntimeSeconds <= 0 {
		return invalid("max_runtime_seconds", "必须给出单次运行时长上限（1—86400 秒）")
	}
	if d.MaxRuntimeSeconds > MaxRuntimeSecondsLimit {
		return invalid("max_runtime_seconds",
			fmt.Sprintf("最多 %d 秒（24 小时）；超过这个时长的运行几乎总是已经挂住了", MaxRuntimeSecondsLimit))
	}
	return nil
}

// normalized 去掉域名首尾空白并转小写（ACME 的域名从来是小写）。
func (p *CertificateParams) normalized() *CertificateParams {
	if p == nil {
		return nil
	}
	return &CertificateParams{
		Domain:  strings.ToLower(strings.TrimSpace(p.Domain)),
		Staging: p.Staging,
	}
}

// certificateDomainChars 是域名允许的字符集。
//
// 白名单：这个字符串会进 `certificate.renew` 的参数，最终出现在 certbot 的 argv 里。
// 通配符（`*.example.com`）单独拒——那需要 DNS-01，而把 DNS 凭据交给面板不在范围内。
var certificateDomainChars = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`)

// validate 校验域名形态。
func (p *CertificateParams) validate() error {
	domain := p.Domain
	if domain == "" {
		return invalid("certificate.domain", "不能为空：续期要知道续哪一张证书")
	}
	if strings.HasPrefix(domain, "*.") || strings.Contains(domain, "*") {
		return invalid("certificate.domain", "不支持通配符域名：通配符需要 DNS-01，面板不接管 DNS 凭据")
	}
	if len(domain) > 253 {
		return invalid("certificate.domain", fmt.Sprintf("最多 253 字节，收到 %d 字节", len(domain)))
	}
	if !strings.Contains(domain, ".") {
		return invalid("certificate.domain", "必须是带点的完整域名：ACME 不给单标签名字签发")
	}
	if !certificateDomainChars.MatchString(domain) {
		return invalid("certificate.domain", "只能含小写字母、数字、- 与 .，且首尾必须是字母或数字")
	}
	for _, label := range strings.Split(domain, ".") {
		if label == "" {
			return invalid("certificate.domain", "域名里有空的段落（连续的 . 或首尾的 .）")
		}
		if len(label) > 63 {
			return invalid("certificate.domain", "单个段落最多 63 字节")
		}
		if strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return invalid("certificate.domain", "段落不能以 - 开头或结尾")
		}
	}
	return nil
}

func isControl(r rune) bool { return r < 0x20 || r == 0x7f }

// Key 是这个定义的内容指纹，用于“定义变了才重写单元”。
//
// 它只覆盖会进入单元文件的字段：Name 只影响 Description，改了也该重写，所以一并算进来。
// Enabled 也在里面：`systemctl enable --now` 与 `disable --now` 是两种落地动作，
// “同一个定义、只有 enabled 变了”必须被认成一次真实变更。
func (d Definition) Key() (string, error) {
	raw, err := json.Marshal(d)
	if err != nil {
		return "", fmt.Errorf("schedules: 序列化定义失败: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// TriggerID 是“这一次触发”的稳定标识。
//
// 稳定性来自 slot：触发落在哪个 UTC 分钟，就是哪一次。systemd 因为 Restart= 或
// 手工 systemctl start 在同一分钟内再进来一次，得到的是同一个 trigger id，于是
// 代理能认出“这是上一次的重试”，而不是把同一次计划跑两遍。
//
// 代价写在 DECISIONS Q365 里：秒级粒度的日历（`*:*:00/10`）会被折叠成每分钟一次。
// 计划任务本来就不该是秒级的；把这一点说清楚，比给一个能被重放打穿的“稳定 id”好。
func TriggerID(scheduleID string, now time.Time) string {
	slot := SlotAt(now)
	sum := sha256.Sum256([]byte("trigger\n" + scheduleID + "\n" + slot.Format(time.RFC3339)))
	return "trg_" + hex.EncodeToString(sum[:])[:32]
}

// OperationID 由 trigger id 派生，因此也稳定：同一次触发无论重试几次，
// 落到代理库里的都是同一个 operation_id（CONTRACTS §4.2 的幂等前提）。
func OperationID(triggerID string) string {
	sum := sha256.Sum256([]byte("operation\n" + triggerID))
	return "op_" + hex.EncodeToString(sum[:])[:32]
}

// SlotAt 把时刻截断到“触发槽”（UTC 分钟）。
func SlotAt(now time.Time) time.Time { return now.UTC().Truncate(time.Minute) }

// ActionRequest 是“这条计划这次要提交哪个动作、带什么参数”。
//
// 参数是按定义里的 target_id 与**代理库里此刻的版本**现算的：计划里不保存
// `version`，否则一条一年前建的计划会拿着一年前的版本去提交，每次都撞版本冲突。
// 版本只能来自代理自己的镜像（CONTRACTS §15：“从代理已批准配置取动作”）。
type ActionRequest struct {
	Action string
	Params json.RawMessage
}

// ResolvedTarget 是触发时需要的那一点点运行期事实。
type ResolvedTarget struct {
	// Version 是目标的当前版本（备份策略、脚本）。证书没有版本。
	Version int64
}

// ActionFor 把一条定义翻译成一次动作提交。
//
// 允许的动作是闭集里的三条（CONTRACTS §15）：backup.run、certificate.renew、
// script.run。别的任何动作都不会从这里出去——一条能指向任意动作的“计划任务”
// 就是绕过全部预检与确认的后门。
func ActionFor(d Definition, resolved ResolvedTarget) (ActionRequest, error) {
	switch d.Kind {
	case KindBackup:
		if resolved.Version <= 0 {
			return ActionRequest{}, fmt.Errorf("schedules: 备份策略 %s 在代理镜像里没有版本，拒绝提交", d.TargetID)
		}
		return marshal("backup.run", map[string]any{
			"backup_policy_id": d.TargetID,
			"version":          resolved.Version,
		})
	case KindUserScript:
		if resolved.Version <= 0 {
			return ActionRequest{}, fmt.Errorf("schedules: 脚本 %s 在代理镜像里没有版本，拒绝提交", d.TargetID)
		}
		return marshal("script.run", map[string]any{
			"script_id":           d.TargetID,
			"version":             resolved.Version,
			"max_runtime_seconds": d.MaxRuntimeSeconds,
		})
	case KindCertificateRenewal:
		if d.Certificate == nil {
			// Validate 已经拦过；走到这里说明调用方绕过了校验，仍要拒绝——参数不全的
			// 续期提交出去，certbot 拿到的是空域名。
			return ActionRequest{}, fmt.Errorf("schedules: 证书续期计划缺少证书参数（domain 与 staging）")
		}
		// 只给三样：域名、ACME 环境，以及"这是定时触发"这个事实。
		//
		// 不给证书 id 与邮箱，因为**那个时刻根本没有面板可问**：账本在面板库，
		// 触发发生在 systemd 里。域名定了续的是哪张证书（/etc/letsencrypt/live/<domain>），
		// ACME 账号在机器上（certbot 自己找）；staging 必须跟着定义走，用正式入口
		// 续一张 staging 证书会把一个本不该上生产的对象换掉。
		//
		// scheduled 让执行侧知道这不是"人在面板上点的那一下"：它决定不强制重签
		// （每晚 --force-renewal 一周就能把 Let's Encrypt 的速率限额烧光）。
		return marshal(ActionCertificateRenew, map[string]any{
			"domain":    d.Certificate.Domain,
			"staging":   d.Certificate.Staging,
			"scheduled": true,
		})
	}
	return ActionRequest{}, fmt.Errorf("schedules: 未知的计划种类 %q", d.Kind)
}

func marshal(action string, fields map[string]any) (ActionRequest, error) {
	raw, err := json.Marshal(fields)
	if err != nil {
		return ActionRequest{}, fmt.Errorf("schedules: 序列化 %s 参数失败: %w", action, err)
	}
	return ActionRequest{Action: action, Params: raw}, nil
}
