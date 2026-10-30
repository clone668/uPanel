package certificates

// 证书对站点的可用性判断（P10.3）。
//
// 站点绑定证书（proxies.Definition.CertificateID）之前必须过这一关。判断只用面板
// 库里已经存下的事实：覆盖范围、有效期。它**不**去 stat 主机上的文件——那是执行
// 层的事（proxies.inspectTLS 在写配置前会再读一遍实际文件），两层都要有，因为
// 它们的失败时刻不同：这里挡住"明显绑不上"，那里挡住"文件已经不在了"。

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// CheckCertificate 判断一张证书能不能给某个域名用。返回 nil 表示能用。
//
// 返回的是错误而不是布尔值：调用方要把"不存在""不覆盖""过期了"分别讲清楚。
func (s *Service) CheckCertificate(ctx context.Context, certID, domain string) error {
	cert, err := s.Get(ctx, certID)
	if err != nil {
		return err
	}
	want := strings.TrimSpace(domain)
	if want == "" {
		return invalid("domain", "绑定证书时必须给出站点域名")
	}
	if !cert.CoversDomain(want) {
		return fmt.Errorf("%w：%s 覆盖的是 %s", ErrNotCovered, want, strings.Join(cert.Domains, "、"))
	}
	switch cert.State(s.nowUTC()) {
	case RenewalExpired:
		return fmt.Errorf("%w（到期时间 %s）：绑上去只会得到一个浏览器警告",
			ErrExpired, cert.NotAfter.UTC().Format(time.RFC3339))
	case RenewalUnknown:
		// 事实不完整（not_after 零值或指纹缺失，只在数据被手工改坏时出现）。
		// 这种记录不能绑：判不了有效期，"不知道"不能当"有效"用。
		return fmt.Errorf("%w：重新导入或续期一次让记录恢复完整，再绑定", ErrStateUnknown)
	}
	return nil
}

// AttemptWindow 是同一域名两次 ACME 尝试之间必须间隔的时间（PLAN.md 第 13 节：
// 不要在循环重试中消耗正式签发限额）。
const AttemptWindow = time.Hour

// TooSoon 判断上一次尝试是否还在冷却窗口内。
//
// 传零值表示"没试过"，返回 false。这个函数不查库：调用方各自拿得到自己的那份时间
// （记录里的 last_attempt_at，或者任务表里最近一次尝试）。
func TooSoon(last time.Time, now time.Time) bool {
	if last.IsZero() {
		return false
	}
	return now.UTC().Sub(last.UTC()) < AttemptWindow
}
