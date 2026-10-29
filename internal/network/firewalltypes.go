package network

// P14.3：改 UFW 的规则集。这一份只放**类型与判断**，不碰系统：什么算合法规则、
// 规则怎么渲染成 ufw 的命令行、一条规则怎么在 `ufw show added` 的输出里被认出来。
// 真正动手的那一层在 firewallapply.go。
//
// 边界（CONTRACTS.md 13.7）：只做 **UFW**、只做 IPv4、只做"一条规则一个端口一个协议"。
// 规则区间、应用配置（`ufw allow 'Nginx Full'`）、按接口限定、prepend/insert 指定位置、
// 默认策略（`ufw default`）一律不支持——**返回 unsupported 而不是尽力而为**。
//
// 一个必须说清的取舍：面板**只碰带自己标记的规则**。每条面板加的规则都带
// `comment upanel:<id>`，改/删只认这个标记。管理员手写的规则（没有标记）一条都不动，
// 也不去"推断他想要什么"。理由是失败方向：猜错的代价是删掉一条人在用的放行规则，
// 而"没管到"的代价只是他没在面板里看到那条规则。

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// 防火墙写动作名。与 CONTRACTS.md 第 5 节的登记名逐字一致。
const (
	FirewallActionApply    = "firewall.apply"
	FirewallActionConfirm  = "firewall.confirm"
	FirewallActionRollback = "firewall.rollback"
)

// FirewallQueryName 是"防火墙现在是什么样、能不能改"的查询名。
const FirewallQueryName = "firewall.config"

// FirewallLockName 是防火墙写动作的资源锁。与 network-global 分开：两者改的是不同的
// 东西，同时做也不会互相掩盖，但都同样"改完可能让人失去面板"，所以各自串行。
const FirewallLockName = "firewall-global"

// FirewallRollbackWindow 与网络那条一样是 120 秒。
//
// 用同一个数字不是巧合：它们要回答同一个问题——"我把面板弄没了，多久之后它自己回来"。
// 两个窗口不一样长，人在现场读秒时会算错。
const FirewallRollbackWindow = 120 * time.Second

// 面板自管规则的标记前缀。UFW 把 comment 存成十六进制塞进 /etc/ufw/user.rules，
// 取回来时 `ufw show added` 会原样打印成 `comment 'upanel:<id>'`。
const firewallCommentPrefix = "upanel:"

// 回滚记录标识前缀。nb = network，frb = firewall rollback，两者的记录文件在同一个
// 目录下，靠前缀一眼分得清是谁的。
const firewallTokenPrefix = "frb_"

// 规则动作的闭集。与 UFW 自己的四个动作一一对应，**不做同义词**：
// 面板上加一条 reject，读回来就必须还是 reject。
const (
	RuleAllow  = "allow"
	RuleDeny   = "deny"
	RuleReject = "reject"
	RuleLimit  = "limit"
)

// 协议的闭集。只认 tcp/udp：一条规则一个协议，`ufw allow 22`（同时开 tcp+udp）
// 在面板里表达不出来——它读回来是两条规则，而"我加了一条"和"读回两条"之间
// 那点含糊，会在回滚时变成"删了一条还剩一条"。
const (
	ProtoTCP = "tcp"
	ProtoUDP = "udp"
)

// 防火墙写路径的错误码。前端按 code 分支，所以是闭集。
const (
	// FirewallCodeUnsupported：这台机器不由 UFW 独占管理，或规则形状越界。
	FirewallCodeUnsupported = "FIREWALL_UNSUPPORTED"
	// FirewallCodeConfigChanged：`expected_config_hash` 与现场不符。
	FirewallCodeConfigChanged = "FIREWALL_CONFIG_CHANGED"
	// FirewallCodeDefinitionInvalid：定义不合法。
	FirewallCodeDefinitionInvalid = "FIREWALL_DEFINITION_INVALID"
	// FirewallCodeRollbackPending：已有一条未确认的回滚。
	FirewallCodeRollbackPending = "FIREWALL_ROLLBACK_PENDING"
	// FirewallCodeManagementPath：改完之后面板或 SSH 会被挡住（**阻塞项**）。
	FirewallCodeManagementPath = "FIREWALL_MANAGEMENT_PATH"
	// FirewallCodeManagementUnknown：读不到自己的监听端口，判不出管理通路（阻塞项）。
	FirewallCodeManagementUnknown = "FIREWALL_MANAGEMENT_UNKNOWN"
	// FirewallCodeUnknownRule：现场有一条带面板标记、但形状我们认不出来的规则（阻塞项）。
	FirewallCodeUnknownRule = "FIREWALL_UNKNOWN_MANAGED_RULE"
	// FirewallCodeConfirmExpired：确认来得太晚。
	FirewallCodeConfirmExpired = "FIREWALL_ROLLBACK_EXPIRED"
	// FirewallCodeNoPending：没有待确认的记录。
	FirewallCodeNoPending = "FIREWALL_NO_PENDING_ROLLBACK"
	// FirewallCodeTokenMismatch：给的 token 不是当前待确认的那条。
	FirewallCodeTokenMismatch = "FIREWALL_TOKEN_MISMATCH"
)

// ErrFirewallDefinitionInvalid 是"定义不在闭集里"的哨兵错误。
var ErrFirewallDefinitionInvalid = errors.New("network: 防火墙定义不合法")

// ManagedRule 是面板要维持的一条规则。
//
// `ID` 是**标识**，不是描述：它进 UFW 的 comment（`upanel:<id>`），改规则时靠它认出
// "这条是我的"。所以它一旦用过就不要再改含义——改 ID 等于"删一条旧的、加一条新的"。
type ManagedRule struct {
	ID     string `json:"id"`
	Action string `json:"action"`
	Port   int    `json:"port"`
	Proto  string `json:"proto"`
	// From 为空表示 `from any`（所有来源）；否则是 IPv4 CIDR 或单个 IPv4。
	From string `json:"from"`
}

// FirewallDefinition 是"这台机器的防火墙应该是什么样"。
//
// `Enabled` 也在定义里：只加规则不启用，等于什么都没有（ufw 未启用时规则不生效），
// 而"启用"恰恰是会把管理员关在门外的那一步——所以它必须和规则一起被预检、被回滚。
type FirewallDefinition struct {
	Enabled bool          `json:"enabled"`
	Rules   []ManagedRule `json:"rules"`
}

// FirewallApplyParams 是 firewall.apply 的参数（CONTRACTS.md 5 + 13.7）。
type FirewallApplyParams struct {
	ExpectedConfigHash string              `json:"expected_config_hash"`
	Definition         *FirewallDefinition `json:"definition"`
}

// FirewallConfirmParams 是 firewall.confirm 的参数。
//
// 与 network.confirm 不同，这里**没有** `arrived_on`：防火墙改动不许把面板挡在外面
// （预检里的 FIREWALL_MANAGEMENT_PATH 就是这道门），所以"你是不是从新地址来"这个
// 问题在这里不存在——能发出这条请求，本身就说明管理通路还在。
type FirewallConfirmParams struct {
	Token string `json:"token"`
}

// FirewallRollbackParams 是 firewall.rollback 的参数。
type FirewallRollbackParams struct {
	Token string `json:"token"`
}

// MaxManagedRules 是一次定义里允许的规则条数上限。
//
// 64 条足够一台单机版服务器的全部对外端口；再多的话，页面上的那份列表会变成一份
// 没人读的规则墙，而"我到底加过什么"正是这个功能存在的理由。
const MaxManagedRules = 64

// ruleIDPattern 是规则标识的形状。小写、数字、连字符：它要进 ufw 的 comment，
// UFW 的 comment 允许空格，但空格会让"按空格切分 argc"这类读法出错，
// 所以面板自己先收窄到不需要引号的形状。
var ruleIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// ValidRuleID 判断规则标识是否可用。
func ValidRuleID(id string) bool { return ruleIDPattern.MatchString(id) }

// ValidateFirewallDefinition 校验定义并归一（动作与协议转小写、去空白、按 ID 去重）。
//
// 它同时被动作登记表（解码时）与判决层调用：两处必须给出同一份结论。
func ValidateFirewallDefinition(def *FirewallDefinition) (*FirewallDefinition, error) {
	if def == nil {
		return nil, fmt.Errorf("%w: definition 不能为空", ErrFirewallDefinitionInvalid)
	}
	out := &FirewallDefinition{Enabled: def.Enabled, Rules: make([]ManagedRule, 0, len(def.Rules))}
	if len(def.Rules) > MaxManagedRules {
		return nil, fmt.Errorf("%w: 规则最多 %d 条，收到 %d 条",
			ErrFirewallDefinitionInvalid, MaxManagedRules, len(def.Rules))
	}
	seen := map[string]bool{}
	for i, raw := range def.Rules {
		rule, err := validateManagedRule(raw)
		if err != nil {
			return nil, fmt.Errorf("第 %d 条规则：%w", i+1, err)
		}
		if seen[rule.ID] {
			return nil, fmt.Errorf("%w: 规则标识 %q 重复了", ErrFirewallDefinitionInvalid, rule.ID)
		}
		seen[rule.ID] = true
		out.Rules = append(out.Rules, rule)
	}
	return out, nil
}

func validateManagedRule(in ManagedRule) (ManagedRule, error) {
	out := ManagedRule{
		ID:     strings.TrimSpace(in.ID),
		Action: strings.ToLower(strings.TrimSpace(in.Action)),
		Port:   in.Port,
		Proto:  strings.ToLower(strings.TrimSpace(in.Proto)),
		From:   strings.TrimSpace(in.From),
	}
	if !ValidRuleID(out.ID) {
		return ManagedRule{}, fmt.Errorf(
			"%w: 规则标识只能用 a-z、0-9、-，最长 32 个字符，收到 %q",
			ErrFirewallDefinitionInvalid, in.ID)
	}
	switch out.Action {
	case RuleAllow, RuleDeny, RuleReject, RuleLimit:
	default:
		return ManagedRule{}, fmt.Errorf("%w: action 只有 allow/deny/reject/limit，收到 %q",
			ErrFirewallDefinitionInvalid, in.Action)
	}
	if out.Port < 1 || out.Port > 65535 {
		return ManagedRule{}, fmt.Errorf("%w: port 必须在 1—65535 之间，收到 %d",
			ErrFirewallDefinitionInvalid, out.Port)
	}
	switch out.Proto {
	case ProtoTCP, ProtoUDP:
	default:
		return ManagedRule{}, fmt.Errorf("%w: proto 只有 tcp/udp，收到 %q",
			ErrFirewallDefinitionInvalid, in.Proto)
	}
	if out.From != "" {
		prefix, err := parseFirewallSource(out.From)
		if err != nil {
			return ManagedRule{}, err
		}
		out.From = prefix
	}
	return out, nil
}

// parseFirewallSource 把来源归一成 CIDR 字符串。
//
// 单个地址归一成 /32：`from 10.0.0.5` 与 `from 10.0.0.5/32` 在 UFW 里是同一条规则，
// 而面板拿字符串比对"这条是不是我要的那条"，两种写法必须落到同一份上。
// 只认 IPv4：v6 规则在 UFW 里是另一份表（user6.rules），把两件事塞进一个定义，
// 会让"我删了 v4 那条，v6 那条还在"变成一个真实可发生的状态。
func parseFirewallSource(raw string) (string, error) {
	if prefix, err := netip.ParsePrefix(raw); err == nil {
		if !prefix.Addr().Is4() {
			return "", fmt.Errorf("%w: from 只支持 IPv4，收到 %q", ErrFirewallDefinitionInvalid, raw)
		}
		return prefix.Masked().String(), nil
	}
	addr, err := netip.ParseAddr(raw)
	if err != nil {
		return "", fmt.Errorf("%w: from 既不是 IPv4 地址也不是 CIDR，收到 %q",
			ErrFirewallDefinitionInvalid, raw)
	}
	if !addr.Is4() {
		return "", fmt.Errorf("%w: from 只支持 IPv4，收到 %q", ErrFirewallDefinitionInvalid, raw)
	}
	return netip.PrefixFrom(addr, 32).String(), nil
}

// ruleSpec 是这条规则在 ufw 命令行里的**规范写法**（不含 comment）。
//
// 这个字符串有两个用途：拼命令、以及和 `ufw show added` 的输出逐字比对。所以它必须
// 只有一种写法——一旦这里和 ufw 打印的写法不一致，"规则已经在了"就会被读成"不在"，
// 然后每次 apply 都重复加一条。
func (r ManagedRule) spec() string {
	if r.From == "" {
		return fmt.Sprintf("%s %d/%s", r.Action, r.Port, r.Proto)
	}
	return fmt.Sprintf("%s from %s to any port %d proto %s", r.Action, r.From, r.Port, r.Proto)
}

// comment 是 UFW 里认这条规则用的标记。
func (r ManagedRule) comment() string { return firewallCommentPrefix + r.ID }

// describe 把规则说成一句人话，用于影响说明与计划显示名。
func (r ManagedRule) describe() string {
	from := "所有来源"
	if r.From != "" {
		from = r.From
	}
	return fmt.Sprintf("%s %d/%s（%s）", strings.ToUpper(r.Action), r.Port, r.Proto, from)
}

// sameRule 判断两条规则是不是同一条（除 ID 外的全部字段）。
func sameRule(a, b ManagedRule) bool {
	return a.Action == b.Action && a.Port == b.Port && a.Proto == b.Proto && a.From == b.From
}

// describeFirewallDefinition 把整份定义说成一句人话。
func describeFirewallDefinition(def FirewallDefinition) string {
	state := "保持关闭"
	if def.Enabled {
		state = "启用"
	}
	if len(def.Rules) == 0 {
		return fmt.Sprintf("防火墙%s；面板管理的规则清空", state)
	}
	return fmt.Sprintf("防火墙%s；面板管理 %d 条规则", state, len(def.Rules))
}

// firewallConfigHash 是现场指纹：**参与比较的是启用状态与带面板标记的规则集**。
//
// 为什么不把全部规则（含管理员手写的）算进去：那些规则面板不改，把它们算进指纹，
// 会让"管理员在别处加了一条规则"变成"面板这边的预检作废"——预检的指纹要防的是
// "有人动了我要改的东西"，不是"现场跟五分钟前一模一样"。
// 与 P14.2 的 netplan 指纹口径一致：只算自己管的那部分。
func firewallConfigHash(state FirewallState, managed []ManagedRule) string {
	var b strings.Builder
	if state.Enabled {
		b.WriteString("enabled=1\n")
	} else {
		b.WriteString("enabled=0\n")
	}
	for _, rule := range managed {
		b.WriteString(rule.ID)
		b.WriteString(" ")
		b.WriteString(rule.spec())
		b.WriteString("\n")
	}
	return "sha256:" + sha256Hex([]byte(b.String()))
}

// ValidFirewallToken 判断回滚记录标识的形状。
//
// 与 ValidRollbackToken 一样，这是路径安全的一部分：token 会拼进文件名，
// 所以它必须是一段不可能穿越目录的字符串。两个前缀分开认——把 `nrb_` 传进防火墙
// 回滚里，读到的会是另一件事的记录。
func ValidFirewallToken(token string) bool {
	if len(token) != len(firewallTokenPrefix)+32 {
		return false
	}
	if !strings.HasPrefix(token, firewallTokenPrefix) {
		return false
	}
	for _, c := range token[len(firewallTokenPrefix):] {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// formatPort 只在错误信息里用：把端口说成一个不会被误读成八进制的十进制串。
func formatPort(port int) string { return strconv.Itoa(port) }

// sha256Hex 是这一包共用的短哈希。写成函数而不是内联，是为了让"指纹怎么算的"
// 只有一处——防火墙与 netplan 两侧的指纹都必须是"同样输入必得同样输出"。
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
