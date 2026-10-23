package proxies

// 443 上**此刻真正在用的**是哪一张证书（P10.3）。
//
// 为什么要问这个：配置的字节和"服务的内容"不是一回事。续期只换证书文件
// （certbot 改的是 live/<域名>/ 指向的 archive/*.pem），渲染出来的配置一个字节都不变，
// 所以"配置没变 → 不用 reload"这条幂等判据在续期之后会得出错误结论：新证书躺在盘上，
// 443 上还是旧的，而报告里会说 skipped_unchanged，看起来一切正常。
//
// 判据不能靠记状态（库里没有"上次 reload 时证书是哪一张"这一列，Q286 也不打算加），
// 只能现场问一句：用 SNI 向本机 443 握手，把叶证书的 SHA-256 拿回来，跟磁盘上那份比。
// 这是**测量**，不是假设；同理，reload 之后要再问一次，否则"我 reload 了"只是动作，
// 不是结果。

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"net"
	"strconv"
	"strings"
	"time"
)

// servedCertProbeTimeout 是一次本地握手的上限。
//
// 127.0.0.1 上的握手正常是毫秒级；给到 2 秒是因为这台机器可能正在忙（Q290 记过
// 代理侧偶发秒级时延）。超时不改结论——配置本身已经通过 nginx -t 了，这里只是
// 判断要不要 reload，问不到就退回"按配置字节判断"的旧行为。
const servedCertProbeTimeout = 2 * time.Second

// errServCertAbsent 表示握手成功但对方没有给证书（理论上不会发生）。
var errServCertAbsent = errors.New("握手没有返回证书")

// servedCertFingerprint 用 SNI=domain 向本机 port 握手，返回叶证书指纹（小写 hex）。
//
// **不做信任判断**：自签、过期、域名不匹配都不影响"这是哪一张证书"这个问题——
// 这张证书该不该被信任由 inspectTLS 判。这里唯一要回答的是"证书换没换"。
func servedCertFingerprint(ctx context.Context, domain string, port int) (string, error) {
	probeCtx, cancel := context.WithTimeout(ctx, servedCertProbeTimeout)
	defer cancel()

	dialer := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: servedCertProbeTimeout},
		Config: &tls.Config{
			ServerName:         domain,
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: true, // 只取指纹，见上。
		},
	}
	conn, err := dialer.DialContext(probeCtx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close() }()

	tlsConn, ok := conn.(*tls.Conn)
	if !ok {
		return "", errors.New("握手连接类型异常")
	}
	certs := tlsConn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return "", errServCertAbsent
	}
	sum := sha256.Sum256(certs[0].Raw)
	return hex.EncodeToString(sum[:]), nil
}

// shortFingerprint 取指纹前 16 位放进报错信息里（短于 16 位就原样给，别 panic）。
func shortFingerprint(fp string) string {
	if len(fp) > 16 {
		return fp[:16] + "…"
	}
	return fp
}

// reload 之后留给新一代 worker 接上的预算。
//
// 为什么需要等：`nginx -s reload` 只是发一个 HUP 就返回，新配置由 master 稍后读、
// 新 worker 稍后接上监听。真机上（P10.3 验收）reload 刚返回就问 443，拿到的还是
// **旧证书**——不是没 reload，是还没轮到。把它当成"reload 没生效"，就会在每一次
// 续期之后报一个假的陈旧告警；不等也不行，那就分不清"还没轮到"和"永远轮不到"。
const (
	servedCertSettleAttempts = 12
	servedCertSettleInterval = 250 * time.Millisecond
)

// waitServedCertMatch 反复问 443，直到拿到的指纹等于 want（或预算用完）。
//
// 返回最后一次问到的指纹、问了几次、以及最后一次的错误。**问不到不等于不匹配**：
// 调用方要用 want 与返回值自己比，别把 err 当成"仍然陈旧"。
func (a *Applier) waitServedCertMatch(ctx context.Context, domain string, port int, want string) (string, int, error) {
	var last string
	var lastErr error
	for attempt := 1; attempt <= servedCertSettleAttempts; attempt++ {
		got, err := a.servedCert(ctx, domain, port)
		if err == nil {
			last, lastErr = got, nil
			if sameFingerprint(want, got) {
				return got, attempt, nil
			}
		} else {
			lastErr = err
		}
		if attempt == servedCertSettleAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return last, attempt, lastErr
		case <-time.After(servedCertSettleInterval):
		}
	}
	return last, servedCertSettleAttempts, lastErr
}

// sameFingerprint 比较磁盘指纹与服务端指纹。
//
// 任一为空都返回 false：空的意思是"不知道"，而"不知道"不能当作"一样"——
// 那正好是这类问题里最容易犯的错（把没测到当成没变化）。
func sameFingerprint(disk, served string) bool {
	if disk == "" || served == "" {
		return false
	}
	return strings.EqualFold(disk, served)
}
