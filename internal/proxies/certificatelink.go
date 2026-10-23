package proxies

// 站点与证书的绑定（P10.3）。
//
// 这里只做两件事：判断“这张证书能不能给这个域名用”，以及“这张证书现在被哪些站点
// 用着”。真正的证书内容、私钥、签发与续期都在 internal/certificates 与代理侧：
// proxies 不该知道 PEM 长什么样，正如它不知道 htpasswd 的哈希长什么样。

import (
	"context"
	"fmt"
	"sort"
)

// CertificateChecker 是证书服务在这一侧的投影。
//
// 只有两个回答：能用（nil），或者为什么不能用（错误）。返回错误而不是布尔值，
// 是为了让“证书不存在”“证书不覆盖这个域名”“证书过期了”这几种情况各自带着人
// 能读懂的理由，而不是一个 false 之后又重新猜一遍。
type CertificateChecker interface {
	CheckCertificate(ctx context.Context, certID, domain string) error
}

// checkCertificate 在写入前确认证书可用。
//
// 只在真的动了 certificate_id 或 domain 时才问：一次改超时不该因为证书服务临时
// 不可用而失败。反过来说，只要这两个字段之一出现，就必须问——绑定一张不覆盖本域名
// 的证书，结果是一个 443 上证书名字对不上的站点，浏览器直接报错，而面板看起来
// “绑定成功”。
func (s *Service) checkCertificate(ctx context.Context, certID, domain string) error {
	if certID == "" {
		return nil
	}
	if s.certs == nil {
		return fmt.Errorf("%w: 面板没有连上证书服务，无法确认 certificate_id", ErrFactsUnavailable)
	}
	if err := s.certs.CheckCertificate(ctx, certID, domain); err != nil {
		return err
	}
	return nil
}

// SitesUsingCertificate 返回绑着某张证书的站点标识（按 id 排序）。
//
// 返回的是**标识**而不是域名：删除证书前的 409 要告诉用户“先去改哪两条记录”，
// 而标识是记录的唯一句柄。（域名更友好，但那条更友好的信息在接口层拼得出来，
// 这里不该为了好看去 join 一次。）
func (s *Service) SitesUsingCertificate(ctx context.Context, certID string) ([]string, error) {
	if certID == "" {
		return nil, nil
	}
	rows, err := s.query(ctx, `SELECT id FROM proxies WHERE certificate_id = ? ORDER BY id`, certID)
	if err != nil {
		return nil, fmt.Errorf("proxies: 查询证书占用失败: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("proxies: 读取证书占用失败: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("proxies: 读取证书占用失败: %w", err)
	}
	sort.Strings(out)
	return out, nil
}
