package certificates

// 证书现场事实的读取端（P10.3）。
//
// 面板库里存的是"导入那天从证书里解析出来的事实"。这张盘上的文件却可能已经换了：
// certbot 自带的 systemd timer 会在后台续期 /etc/letsencrypt/renewal 下的谱系，
// 有人也可能手工替换了 /etc/ssl/upanel 里的文件。所以详情页需要一句"盘上现在是什么样"，
// 而这句话只有 root 能回答——它就是代理的一条只读查询 certificate.state。
//
// 这里刻意**不把代理返回的 map 原样转给接口层**。代理的响应里带着 cert_path 与
// key_path（它自己排查问题时有用），而 CONTRACTS.md 第 9 节要求接口不返回私钥路径。
// 用一份显式的结构体接住它，多出来的字段自然被丢掉——比在每个调用点记着删一个键可靠。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// StateQueryName 是代理侧只读查询的名字（与 internal/agent 逐字一致）。
const StateQueryName = ActionState

// QueryAgent 是读取证书事实所需的代理能力。生产实现是 *agentclient.Client。
type QueryAgent interface {
	Query(ctx context.Context, name string, params any) (json.RawMessage, error)
}

// Inspected 是盘上那张证书的事实。
//
// Present=false 有两种意思，靠 Reason 区分：文件不在（还没签发/还没导入），或者
// 读得到但解析不了。"不在"不是错误——刚建好记录还没执行过任务时它就是不在。
// 同理 KeyPresent 只报告"私钥在不在、mode 是什么"，从不报告内容，也不报告路径。
type Inspected struct {
	Domain      string   `json:"domain"`
	Source      string   `json:"source"`
	Present     bool     `json:"present"`
	Reason      string   `json:"reason,omitempty"`
	Issuer      string   `json:"issuer,omitempty"`
	Subject     string   `json:"subject,omitempty"`
	Domains     []string `json:"domains,omitempty"`
	NotBefore   string   `json:"not_before,omitempty"`
	NotAfter    string   `json:"not_after,omitempty"`
	Fingerprint string   `json:"fingerprint,omitempty"`
	ChainLength int      `json:"chain_length,omitempty"`
	Staging     bool     `json:"staging"`
	SelfSigned  bool     `json:"self_signed"`
	Expired     bool     `json:"expired"`
	Expiring    bool     `json:"expiring"`
	// KeyPresent 表示私钥文件存在；KeyMode 是它的八进制权限位（例如 "0600"）。
	// 这两个字段的用途是让"私钥权限被人改松了"这件事在界面上看得见。
	KeyPresent bool   `json:"key_present"`
	KeyMode    string `json:"key_mode,omitempty"`
}

// AgentInspector 是 Inspector 的生产实现：每次调用都向代理要 certificate.state。
//
// 不缓存：这是展示路径上的"现场"，缓存会让页面显示一个比库里的到期时间更新的
// 到期时间，而用户以为自己看到的是此刻。代理侧本身也没有缓存这条查询。
type AgentInspector struct {
	agent QueryAgent
}

// NewAgentInspector 构造读取端。agent 为 nil 时报错，理由与 NewAgentFacts 相同：
// 不拿一个"什么都不知道"的假读取端顶上。
func NewAgentInspector(agent QueryAgent) (*AgentInspector, error) {
	if agent == nil {
		return nil, errors.New("certificates: 证书状态读取端需要代理客户端")
	}
	return &AgentInspector{agent: agent}, nil
}

// Inspect 读一个域名对应证书的现场事实。
func (i *AgentInspector) Inspect(ctx context.Context, domain, source string) (Inspected, error) {
	params := StateParams{Domain: domain, Source: source}
	normalized, err := params.Normalize()
	if err != nil {
		return Inspected{}, err
	}
	raw, err := i.agent.Query(ctx, StateQueryName, normalized)
	if err != nil {
		return Inspected{}, fmt.Errorf("certificates: 读取证书状态失败: %w", err)
	}
	var out Inspected
	if err := json.Unmarshal(raw, &out); err != nil {
		return Inspected{}, fmt.Errorf("certificates: 证书状态响应无法解析: %w", err)
	}
	if out.KeyMode != "" && !strings.HasPrefix(out.KeyMode, "0") {
		out.KeyMode = "0" + out.KeyMode
	}
	return out, nil
}
