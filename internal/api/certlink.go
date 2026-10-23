package api

// 站点 → 证书材料的解析（P10.3）。
//
// 渲染器是纯函数：给它一份定义和一份证书材料，它吐出字节。所以"这条站点现在应该
// 长什么样"这个问题在这里必须先变成一次查库——证书路径、指纹、签发者、到期时间
// 都在 certificates 表里，不在 proxies 表里。
//
// 一个刻意的取舍：**证书读不到时不要退回"只监听的 80 的渲染结果"**。那份字节当然
// 算得出来，但它是另一份配置的哈希，拿去和磁盘上那份 443 配置比，得到的是 stale——
// 一个假结论。这里的规则是：要渲染 443 就必须有材料，没有就如实说算不出来。

import (
	"context"

	"upanel/internal/certificates"
	"upanel/internal/proxies"
)

// tlsMaterialsOfSites 为一批站点解析证书材料（一次查询，N 次使用）。
//
// 返回的 map 以站点标识为键。**没有键**表示这个站点不该有 443：
// 要么它本来就没绑证书（CertificateID 为空），要么证书服务根本没连、或者库里
// 那张证书已经不在了。三种情况在 appliedStateOf 里靠 site.CertificateID 区分。
func (s *Server) tlsMaterialsOfSites(ctx context.Context, sites []proxies.Site) map[string]proxies.TLSMaterial {
	out := make(map[string]proxies.TLSMaterial, len(sites))
	if s.opts.Certificates == nil {
		return out
	}
	for _, site := range sites {
		if site.CertificateID == "" {
			continue
		}
		material, err := s.tlsMaterial(ctx, site.CertificateID)
		if err != nil {
			// 读不到就是读不到：调用方会用 CertificateID 非空 + 没有材料来判断
			// "这条站点算不出来"，并把原因写成一句人能读懂的话。
			continue
		}
		out[site.ID] = material
	}
	return out
}

// tlsMaterial 读一张证书记录并转成渲染材料。
//
// 这里唯一接受的失败理由是"库里没有这条记录"或"读库出错"；证书文件本身还在不在
// 不在这里判断——那是代理侧执行时的事（inspectTLS）。展示路径不该去 stat 一个
// 只有 root 能读的目录。
func (s *Server) tlsMaterial(ctx context.Context, certID string) (proxies.TLSMaterial, error) {
	cert, err := s.opts.Certificates.Get(ctx, certID)
	if err != nil {
		return proxies.TLSMaterial{}, err
	}
	return cert.Material(), nil
}

// certificateRecord 是"这个站点绑的证书"的可选视图，供详情页展示。
//
// 返回 nil 有三种情况，且都不算错误：没有绑定、证书服务没连、记录已被删（外键本来
// 会挡住，但读的时候仍可能撞上一个刚被删掉的标识）。页面据此显示"未绑定 TLS"。
func (s *Server) certificateRecord(ctx context.Context, certID string) *certificates.Certificate {
	if certID == "" || s.opts.Certificates == nil {
		return nil
	}
	cert, err := s.opts.Certificates.Get(ctx, certID)
	if err != nil {
		return nil
	}
	return &cert
}
