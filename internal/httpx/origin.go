package httpx

import (
	"fmt"
	"net/url"
	"strings"
)

// Origin 处理跨站请求防护用的 Origin 值。
//
// 依据 CONTRACTS.md 第 2 节：登录等状态变更请求必须携带与本地配置完全一致的 Origin，
// 且**不得**从 Host 头或 X-Forwarded-* 推导期望值——那是攻击者可影响的数据。
// 因此这里的规则是“规范化后逐字比较”，不做通配、不做后缀匹配。

// NormalizeOrigin 把 Origin/配置项规范化成 "scheme://host[:port]"。
//
// 规则：
//   - 只接受 http/https；
//   - 不接受 userinfo、查询串、片段；
//   - 路径只能是空或 "/"；
//   - scheme、host 小写；默认端口（http 80、https 443）去掉；
//   - IPv6 字面量保留方括号。
//
// 无法规范化的值一律返回错误：宁可在配置校验时报错，也不要放行一个模糊的源。
func NormalizeOrigin(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", fmt.Errorf("origin 不能为空")
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("origin %q 不是合法 URL: %w", raw, err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("origin %q 的协议必须是 http 或 https，当前为 %q", raw, u.Scheme)
	}
	if u.User != nil {
		return "", fmt.Errorf("origin %q 不能包含用户信息", raw)
	}
	if u.RawQuery != "" || u.ForceQuery {
		return "", fmt.Errorf("origin %q 不能包含查询串", raw)
	}
	if u.Fragment != "" {
		return "", fmt.Errorf("origin %q 不能包含片段", raw)
	}
	if u.Path != "" && u.Path != "/" {
		return "", fmt.Errorf("origin %q 只能包含主机部分，不能带路径 %q", raw, u.Path)
	}
	if u.Opaque != "" {
		return "", fmt.Errorf("origin %q 不是正常的层级 URL", raw)
	}

	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", fmt.Errorf("origin %q 缺少主机名", raw)
	}
	port := u.Port()
	if port != "" {
		n, err := parsePort(port)
		if err != nil {
			return "", fmt.Errorf("origin %q 的端口不合法: %w", raw, err)
		}
		if (scheme == "http" && n == 80) || (scheme == "https" && n == 443) {
			port = ""
		} else {
			port = fmt.Sprint(n)
		}
	}

	hostPart := host
	if strings.Contains(host, ":") {
		hostPart = "[" + host + "]"
	}
	if port != "" {
		return scheme + "://" + hostPart + ":" + port, nil
	}
	return scheme + "://" + hostPart, nil
}

func parsePort(port string) (int, error) {
	if len(port) == 0 || len(port) > 5 {
		return 0, fmt.Errorf("端口 %q 超出范围", port)
	}
	n := 0
	for _, c := range port {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("端口 %q 含非数字字符", port)
		}
		n = n*10 + int(c-'0')
	}
	if n < 1 || n > 65535 {
		return 0, fmt.Errorf("端口 %d 不在 1—65535 之间", n)
	}
	return n, nil
}

// OriginSet 是一组已规范化的允许源。零值不可用，必须用 NewOriginSet 构造。
type OriginSet struct {
	origins []string
	set     map[string]struct{}
}

// NewOriginSet 规范化并去重一组源。任一项非法即返回错误（不跳过、不猜测）。
func NewOriginSet(raw []string) (*OriginSet, error) {
	set := make(map[string]struct{}, len(raw))
	ordered := make([]string, 0, len(raw))
	for _, item := range raw {
		norm, err := NormalizeOrigin(item)
		if err != nil {
			return nil, err
		}
		if _, dup := set[norm]; dup {
			continue
		}
		set[norm] = struct{}{}
		ordered = append(ordered, norm)
	}
	return &OriginSet{origins: ordered, set: set}, nil
}

// Empty 表示没有任何允许源（此时所有带 Origin 的状态变更请求都应被拒绝）。
func (o *OriginSet) Empty() bool {
	return o == nil || len(o.set) == 0
}

// List 返回规范化后的源，顺序与输入一致（已去重）。
func (o *OriginSet) List() []string {
	if o == nil {
		return nil
	}
	out := make([]string, len(o.origins))
	copy(out, o.origins)
	return out
}

// Contains 判断给定的 Origin 头值是否在允许集合内。
//
// 空值、多次出现的头（"a, b" 这种拼接结果）、无法规范化的值一律不匹配。
func (o *OriginSet) Contains(origin string) bool {
	if o.Empty() {
		return false
	}
	norm, err := NormalizeOrigin(origin)
	if err != nil {
		return false
	}
	_, ok := o.set[norm]
	return ok
}
