package backup

// 主机密钥核对的真机行为测试（P12.1）。
//
// 这里跑的是真的 SSH 握手：本地起一个同时持有 ECDSA 与 Ed25519 主机密钥的
// ssh.ServerConfig，客户端只登记其中一把的指纹。要点在于——**x/crypto 的默认
// 偏好是 ECDSA 先于 Ed25519**（common.go 的 defaultHostKeyAlgos），所以只登记
// Ed25519 时，默认算法表协商出来的必然是 ECDSA 那把，与登记值不符。真实 Ubuntu
// 上撞到的正是这件事：登记 SHA256:R7OL…（ed25519），握手递过来的却是
// ecdsa-sha2-nistp256，于是在“指纹不一样”上误报，仓库初始化被挡死。
//
// 因此这一组测试锁住五件事：
//  1. 登记的那把钥匙只要服务器有，就一定能连上，不管它是不是首选；
//  2. 服务器首选的正好是登记的那把时，一次握手搞定，不多连；
//  3. 谁都没登记过的指纹 → ErrHostKeyMismatch，且把见过的每把钥匙都报出来；
//  4. 指纹对了但认证被拒 → ErrSSHAuth，且不因为换算法而重试；
//  5. 握手根本走不完（超时/断连）→ 既不是 mismatch 也不是 auth，整轮重来一次
//     就收手，如实报网络层失败；
//  6. 上一轮卡住、下一轮通了 → 连上（一次抖动不等于仓库坏了）。

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// sshFixture 是一台只在测试里存在的 SFTP 主机。
type sshFixture struct {
	Addr    string
	ECDSA   ssh.Signer
	Ed25519 ssh.Signer
	// Attempts 是客户端一共建了几条 TCP 连接（即几次握手尝试）。
	Attempts *atomic.Int32
}

// newSSHFixture 起一台同时持有 ECDSA 与 Ed25519 主机密钥的测试服务器。
// acceptAuth 决定认证是否放行；nil 表示放行任何公钥。
func newSSHFixture(t *testing.T, acceptAuth func(ssh.PublicKey) bool) *sshFixture {
	t.Helper()

	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成 ECDSA 主机密钥失败: %v", err)
	}
	ecSigner, err := ssh.NewSignerFromKey(ecKey)
	if err != nil {
		t.Fatalf("ECDSA 主机密钥转 signer 失败: %v", err)
	}
	_, edKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("生成 Ed25519 主机密钥失败: %v", err)
	}
	edSigner, err := ssh.NewSignerFromKey(edKey)
	if err != nil {
		t.Fatalf("Ed25519 主机密钥转 signer 失败: %v", err)
	}

	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if acceptAuth == nil || acceptAuth(key) {
				return &ssh.Permissions{}, nil
			}
			return nil, errors.New("测试服务器拒绝这把公钥")
		},
	}
	// 顺序照抄 OpenSSH 的常见排布：ECDSA 先加进来。
	cfg.AddHostKey(ecSigner)
	cfg.AddHostKey(edSigner)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	attempts := &atomic.Int32{}
	fx := &sshFixture{Addr: ln.Addr().String(), ECDSA: ecSigner, Ed25519: edSigner, Attempts: attempts}

	go func() {
		for {
			raw, err := ln.Accept()
			if err != nil {
				return
			}
			attempts.Add(1)
			go func(raw net.Conn) {
				defer raw.Close()
				srv, chans, reqs, err := ssh.NewServerConn(raw, cfg)
				if err != nil {
					return
				}
				defer srv.Close()
				go ssh.DiscardRequests(reqs)
				for ch := range chans {
					_ = ch.Reject(ssh.Prohibited, "")
				}
			}(raw)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })

	return fx
}

// clientKeyPEM 生成一把客户端私钥并序列化成 PEM。
func clientKeyPEM(t *testing.T) []byte {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("生成客户端密钥失败: %v", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatalf("序列化客户端私钥失败: %v", err)
	}
	return pem.EncodeToMemory(block)
}

// fingerprintOf 把一把公钥变成管理员会抄下来的那种指纹。
func fingerprintOf(t *testing.T, key ssh.PublicKey) Fingerprint {
	t.Helper()
	fp, err := ParseFingerprint(SHA256(key))
	if err != nil {
		t.Fatalf("解析指纹失败: %v", err)
	}
	return fp
}

// strangerFingerprint 造一把服务器根本没有的钥匙的指纹。
func strangerFingerprint(t *testing.T) Fingerprint {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("生成陌生密钥失败: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("陌生密钥转 signer 失败: %v", err)
	}
	return fingerprintOf(t, signer.PublicKey())
}

// dialOptionsFor 把 fixture 的地址拆成 DialOptions。
func dialOptionsFor(t *testing.T, fx *sshFixture, pemKey []byte) DialOptions {
	t.Helper()
	host, portStr, err := net.SplitHostPort(fx.Addr)
	if err != nil {
		t.Fatalf("拆分地址失败: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("解析端口失败: %v", err)
	}
	return DialOptions{Host: host, Port: port, User: "backup", PrivateKey: pemKey, Timeout: 5 * time.Second}
}

// TestDialAcceptsRegisteredKeyRegardlessOfNegotiatedPreference 是本轮修复的核心：
// 服务器有 Ed25519，但默认算法表先协商到 ECDSA；登记的只有 Ed25519 的指纹。
// 必须仍然连上，并且报出来的那把钥匙是登记的那把。
func TestDialAcceptsRegisteredKeyRegardlessOfNegotiatedPreference(t *testing.T) {
	fx := newSSHFixture(t, nil)
	pemKey := clientKeyPEM(t)
	registered := fingerprintOf(t, fx.Ed25519.PublicKey())

	client, hostKey, err := Dial(context.Background(), dialOptionsFor(t, fx, pemKey), registered)
	if err != nil {
		t.Fatalf("登记了 Ed25519 指纹却连不上: %v", err)
	}
	defer client.Close()

	if hostKey.Fingerprint != registered.String() {
		t.Fatalf("核对通过的是 %s，登记的是 %s", hostKey.Fingerprint, registered.String())
	}
	if hostKey.Type != ssh.KeyAlgoED25519 {
		t.Fatalf("核对通过的类型是 %s，应为 %s", hostKey.Type, ssh.KeyAlgoED25519)
	}
	if got := fx.Attempts.Load(); got < 2 {
		t.Fatalf("只握手了 %d 次；登记的不是首选时必须重试，否则说明测试没走到这条路径", got)
	}
	// 诊断信息要留下第一次见到的那把（ECDSA），管理员才看得出“服务器是那台，只是先递了另一把”。
	if !strings.Contains(strings.Join(hostKey.Algorithms, " "), ssh.KeyAlgoECDSA256) {
		t.Fatalf("Algorithms 里没有第一次见到的 ECDSA 指纹: %v", hostKey.Algorithms)
	}
}

// TestDialAcceptsPreferredKeyInOneAttempt 保证修复没有把正常路径变成重试路径。
func TestDialAcceptsPreferredKeyInOneAttempt(t *testing.T) {
	fx := newSSHFixture(t, nil)
	pemKey := clientKeyPEM(t)
	registered := fingerprintOf(t, fx.ECDSA.PublicKey())

	client, hostKey, err := Dial(context.Background(), dialOptionsFor(t, fx, pemKey), registered)
	if err != nil {
		t.Fatalf("登记首选指纹却连不上: %v", err)
	}
	defer client.Close()

	if hostKey.Fingerprint != registered.String() {
		t.Fatalf("核对通过的是 %s，登记的是 %s", hostKey.Fingerprint, registered.String())
	}
	if got := fx.Attempts.Load(); got != 1 {
		t.Fatalf("握手了 %d 次，应为 1 次（首选即登记时不该重试）", got)
	}
}

// TestDialRejectsUnregisteredFingerprint 是失败关闭那一侧。
func TestDialRejectsUnregisteredFingerprint(t *testing.T) {
	fx := newSSHFixture(t, nil)
	pemKey := clientKeyPEM(t)
	registered := strangerFingerprint(t)

	client, _, err := Dial(context.Background(), dialOptionsFor(t, fx, pemKey), registered)
	if client != nil {
		client.Close()
		t.Fatalf("指纹不匹配时不该返回可用连接")
	}
	if !errors.Is(err, ErrHostKeyMismatch) {
		t.Fatalf("错误应是 ErrHostKeyMismatch，实际: %v", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, registered.String()) {
		t.Fatalf("错误里没报登记的指纹: %s", msg)
	}
	// 服务器持有的两把都要报出来：管理员据此判断是抄错了还是中间有人。
	if !strings.Contains(msg, ssh.KeyAlgoECDSA256) || !strings.Contains(msg, ssh.KeyAlgoED25519) {
		t.Fatalf("错误里应列出服务器实际提供的两种主机密钥: %s", msg)
	}
	// 重试必须有限：默认一次 + 算法表逐个试。
	if got, max := fx.Attempts.Load(), int32(1+len(hostKeyAlgorithms)); got > max {
		t.Fatalf("握手了 %d 次，上限应为 %d 次（重试不能无界）", got, max)
	}
}

// TestDialReportsAuthFailureWithoutRetrying：指纹对、认证被拒。这是认证问题，
// 换主机密钥算法救不了，必须一次就停。
func TestDialReportsAuthFailureWithoutRetrying(t *testing.T) {
	fx := newSSHFixture(t, func(ssh.PublicKey) bool { return false })
	pemKey := clientKeyPEM(t)
	registered := fingerprintOf(t, fx.ECDSA.PublicKey())

	client, _, err := Dial(context.Background(), dialOptionsFor(t, fx, pemKey), registered)
	if client != nil {
		client.Close()
		t.Fatalf("认证被拒时不该返回可用连接")
	}
	if !errors.Is(err, ErrSSHAuth) {
		t.Fatalf("错误应是 ErrSSHAuth，实际: %v", err)
	}
	if errors.Is(err, ErrHostKeyMismatch) {
		t.Fatalf("指纹是对的，不该报成不匹配: %v", err)
	}
	if got := fx.Attempts.Load(); got != 1 {
		t.Fatalf("握手了 %d 次，应为 1 次（认证失败不该重试）", got)
	}
}

// TestDialRejectsUnreachableHost：连不上就直接失败，不因为"换算法"而反复连。
func TestDialRejectsUnreachableHost(t *testing.T) {
	// 拿一个刚关掉的端口当目标。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	host, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)

	_, _, err = Dial(context.Background(), DialOptions{
		Host: host, Port: port, User: "backup", PrivateKey: clientKeyPEM(t), Timeout: 2 * time.Second,
	}, strangerFingerprint(t))
	if err == nil {
		t.Fatalf("连不上却成功了")
	}
	if errors.Is(err, ErrHostKeyMismatch) || errors.Is(err, ErrSSHAuth) {
		t.Fatalf("连不上应报连接错误，而不是主机密钥/认证错误: %v", err)
	}
}

// TestDialHandshakeTimeoutIsNotAuthFailure：连上了但对面不说话（握手超时）。
//
// 真机上撞到过这一条：两台虚机同时跑验收，CPU 被抢到握手超时，而结论写的是
// "SFTP 认证失败"——把一次网络超时说成钥匙不对，人会先去换钥匙。它必须落到
// "连不上"（BACKUP_SFTP_UNREACHABLE）。重试只能是**整轮有限次**：既不能因为
// "换算法"把超时乘以算法表长度，也不能无限重来把一次请求拖成几分钟。
func TestDialHandshakeTimeoutIsNotAuthFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	defer ln.Close()
	attempts := &atomic.Int32{}
	// 收下的连接要一直挂着，直到测试结束——关掉就变成 EOF，测不到卡住的那条路。
	var (
		mu    sync.Mutex
		conns []net.Conn
	)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			attempts.Add(1)
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		for _, conn := range conns {
			_ = conn.Close()
		}
	})

	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)
	started := time.Now()
	_, _, err = Dial(context.Background(), DialOptions{
		Host: host, Port: port, User: "backup", PrivateKey: clientKeyPEM(t),
		Timeout: 700 * time.Millisecond,
	}, strangerFingerprint(t))
	if err == nil {
		t.Fatalf("对面不说话却成功了")
	}
	if errors.Is(err, ErrSSHAuth) {
		t.Fatalf("握手超时不该报成认证失败: %v", err)
	}
	if errors.Is(err, ErrHostKeyMismatch) {
		t.Fatalf("握手超时不该报成主机密钥不匹配: %v", err)
	}
	if got := attempts.Load(); got != int32(dialRounds) {
		t.Fatalf("连了 %d 次，应为 %d 次（网络失败整轮重来，不逐个算法重试）", got, dialRounds)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("用了 %s 才失败：重试把超时叠起来了", elapsed)
	}
}

// TestDialRetriesOnceWhenHandshakeStalls 锁住这次修复：第一轮握手卡到超时，
// 第二轮拿到登记的钥匙就算成功。
//
// 依据是 22.04 真机上的那一次：同一台机器、同一个登记指纹，执行前的复核卡了整整
// 15 秒的 `read tcp 127.0.0.1:56442->127.0.0.1:22: i/o timeout`，紧接着手动重放
// 6 次全在 300 毫秒内连上。一次调度打嗝不该等于"仓库坏了"。
func TestDialRetriesOnceWhenHandshakeStalls(t *testing.T) {
	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("生成主机密钥失败: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatalf("主机密钥转 signer 失败: %v", err)
	}
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
			return &ssh.Permissions{}, nil
		},
	}
	cfg.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	defer ln.Close()

	attempts := &atomic.Int32{}
	var (
		mu      sync.Mutex
		stalled []net.Conn
	)
	// 第一条连接只收下、不回一个字节（客户端会读到超时），之后的正常服务。
	go func() {
		for {
			raw, err := ln.Accept()
			if err != nil {
				return
			}
			if attempts.Add(1) == 1 {
				mu.Lock()
				stalled = append(stalled, raw)
				mu.Unlock()
				continue
			}
			go func(raw net.Conn) {
				defer raw.Close()
				srv, chans, reqs, err := ssh.NewServerConn(raw, cfg)
				if err != nil {
					return
				}
				defer srv.Close()
				go ssh.DiscardRequests(reqs)
				for ch := range chans {
					_ = ch.Reject(ssh.Prohibited, "")
				}
			}(raw)
		}
	}()
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		for _, conn := range stalled {
			_ = conn.Close()
		}
	})

	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)
	registered := fingerprintOf(t, signer.PublicKey())
	started := time.Now()
	client, hostKey, err := Dial(context.Background(), DialOptions{
		Host: host, Port: port, User: "backup", PrivateKey: clientKeyPEM(t),
		Timeout: 600 * time.Millisecond,
	}, registered)
	if err != nil {
		t.Fatalf("第一轮卡住、第二轮正常，却判定连不上: %v", err)
	}
	defer client.Close()

	if hostKey.Fingerprint != registered.String() {
		t.Fatalf("核对通过的是 %s，登记的是 %s", hostKey.Fingerprint, registered.String())
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("连了 %d 次，应为 2 次（第一次卡住，第二次成功）", got)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("用了 %s：重试把等待叠得太长了", elapsed)
	}
}
