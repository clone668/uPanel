package api

// "这条记录现在是否已经落在 Nginx 上"（P10.2）。
//
// P10.1 的响应里刻意没有 applied_* 字段：那时一个字节都没写过，任何值都是编的。
// P10.2 写了配置，于是这个问题第一次有了真答案——但答案仍然**不是数据库里的一列**：
// 它由"面板此刻渲染出来的字节"与"配置树里那份文件的字节"比对得到。库里存一个
// applied=true 只会多出一个会漂移的事实来源：有人手工改了文件、或者文件被删了，
// 库里的 true 依然在那儿。
//
// 四种状态，闭集：
//
//	applied      文件在，内容与渲染结果逐字节相同
//	stale        文件在，但内容不同（被改过，或记录改过还没应用）
//	not_applied  应该生效却没有文件；或者已停用且文件确实不在了
//	unknown      读不到 Nginx 现状（或这份定义渲染不出来）
//
// stale 与 not_applied 的区别在于"要不要先清掉一份旧的"：前者有文件要替换，
// 后者只是缺一步。混成一个值，页面就解释不了"我明明停用了，它为什么还说没应用"。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	// 这里是 nginx 侧的路径：它要拼进渲染结果，再和磁盘上那份文件比字节。
	// 用 path 而不是 filepath，理由与 internal/proxies/configfs.go 相同——
	// 让“在哪台机器上编译”不参与决定配置里的分隔符。
	"path"

	"upanel/internal/nginx"
	"upanel/internal/proxies"
)

// 状态闭集。
const (
	proxyStateApplied    = "applied"
	proxyStateStale      = "stale"
	proxyStateNotApplied = "not_applied"
	proxyStateUnknown    = "unknown"
)

// proxyAppliedDTO 是一条站点的落地状态。
type proxyAppliedDTO struct {
	State      string `json:"state"`
	ConfigFile string `json:"config_file"`
	ConfigHash string `json:"config_hash,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

// proxyAppliedStates 为一批站点算出落地状态（一次采集，N 次渲染）。
//
// 用 Snapshot 而不是 Collect：这是展示路径，不是写入门禁。写入的冲突判断在代理
// 那一侧用的是不吃缓存的 Collect（P10.1 的规则没有变）。
func (s *Server) proxyAppliedStates(ctx context.Context, sites []proxies.Site) map[string]proxyAppliedDTO {
	out := make(map[string]proxyAppliedDTO, len(sites))
	if s.opts.NginxFacts == nil {
		for _, site := range sites {
			out[site.ID] = proxyAppliedDTO{
				State:  proxyStateUnknown,
				Reason: "面板没有连上代理，读不到 Nginx 现状",
			}
		}
		return out
	}
	facts, err := s.opts.NginxFacts.Snapshot(ctx)
	if err != nil {
		for _, site := range sites {
			out[site.ID] = proxyAppliedDTO{
				State:  proxyStateUnknown,
				Reason: "读 Nginx 现状失败：" + err.Error(),
			}
		}
		return out
	}
	for _, site := range sites {
		out[site.ID] = appliedStateOf(facts, site)
	}
	return out
}

// appliedStateOf 算一条站点的落地状态。它不返回 error：展示路径上"算不出来"
// 本身就是一种状态（unknown），而不是一次请求失败。
func appliedStateOf(facts nginx.Facts, site proxies.Site) proxyAppliedDTO {
	root := facts.ConfigRoot
	if root == "" {
		root = nginx.DefaultConfigRoot
	}
	fileName, err := nginx.ManagedFileName(site.Domain)
	if err != nil {
		return proxyAppliedDTO{State: proxyStateUnknown, Reason: err.Error()}
	}
	confPath := path.Join(root, "conf.d", fileName)
	dto := proxyAppliedDTO{ConfigFile: confPath}

	if !facts.Available {
		dto.State = proxyStateUnknown
		if facts.ConfigMissing {
			dto.Reason = "这台机器上没有 Nginx 配置目录：" + facts.Reason
		} else {
			dto.Reason = "读不到 Nginx 现状：" + facts.Reason
		}
		return dto
	}

	var found *nginx.ConfigFile
	for i := range facts.Files {
		if facts.Files[i].Path == confPath {
			found = &facts.Files[i]
			break
		}
	}

	// 已停用：正确状态是"文件不存在"。
	if !site.Enabled {
		if found != nil {
			dto.State = proxyStateStale
			dto.Reason = "站点已停用，但托管配置还在：需要应用一次（enabled=false）把它移除"
			return dto
		}
		dto.State = proxyStateNotApplied
		dto.Reason = "站点已停用，托管配置不存在"
		return dto
	}

	def := site.Definition
	rendered, rerr := proxies.RenderSite(site.ID, def, path.Join(root, proxies.AuthDirName,
		proxies.AuthFileNameOf(site.ID)))
	if rerr != nil {
		dto.State = proxyStateUnknown
		dto.Reason = "这份定义渲染不出配置：" + rerr.Error()
		return dto
	}
	expected := hashString(rendered)
	dto.ConfigHash = expected

	if found == nil {
		dto.State = proxyStateNotApplied
		dto.Reason = "托管配置还不存在：应用一次才会写下去"
		return dto
	}
	if found.Error != "" {
		dto.State = proxyStateUnknown
		dto.Reason = "托管配置解析失败：" + found.Error
		return dto
	}
	if found.SHA256 != expected {
		dto.State = proxyStateStale
		dto.Reason = "配置文件与这条记录不一致（文件被改过，或记录改完还没应用）"
		return dto
	}
	dto.State = proxyStateApplied
	return dto
}

// managedConfigExists 判断某个域名的托管配置是否还在配置树里。
//
// 删除记录之前要问这一句：记录删了而文件还在，就会留下一份**没有任何界面能解释**
// 的 server 块——它还在生效、还在代理流量，但面板上再也找不到它。
func (s *Server) managedConfigExists(ctx context.Context, site proxies.Site) (bool, error) {
	if s.opts.NginxFacts == nil {
		return false, errNoNginxFacts
	}
	facts, err := s.opts.NginxFacts.Snapshot(ctx)
	if err != nil {
		return false, err
	}
	if !facts.Available {
		if facts.ConfigMissing {
			return false, nil
		}
		return false, errNoNginxFacts
	}
	root := facts.ConfigRoot
	if root == "" {
		root = nginx.DefaultConfigRoot
	}
	fileName, err := nginx.ManagedFileName(site.Domain)
	if err != nil {
		return false, nil
	}
	confPath := path.Join(root, "conf.d", fileName)
	for _, f := range facts.Files {
		if f.Path == confPath {
			return true, nil
		}
	}
	return false, nil
}

// errNoNginxFacts 表示读不到 Nginx 现状，因此无法回答"文件还在不在"。
var errNoNginxFacts = errors.New("读不到 Nginx 现状，无法确认托管配置是否还在")

// hashString 返回内容的 sha256（小写十六进制），与 internal/proxies 的渲染哈希同一套。
func hashString(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
