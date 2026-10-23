package backup

// SFTP 连接与主机密钥核对（P12.1）。
//
// 这一层**自己**完成 SSH 握手与文件系统探测，而不是把判断交给 restic 的选项：
// restic 会用 `-o sftp.command` 里的 ssh 再连一次，那次连接也钉死了同一个指纹
// （known_hosts 由我们在核对通过后写出来），但“这台主机是不是那一台”这个判断
// 必须有一次性、可测试、能给出两个指纹的实现——它就在这里。
//
// 失败关闭的三处：
//  1. 指纹不匹配 → 立刻断开，不回退到 TOFU，不读 /root/.ssh/known_hosts；
//  2. 私钥解析失败或认证被拒 → 如实报错，不退化成口令认证；
//  3. 握手阶段没有公钥 → 视为失败（没有可比的东西就等于没核对）。

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// DialOptions 是一次远端连接的参数。
type DialOptions struct {
	Host string
	Port int
	User string
	// PrivateKey 是 PEM 私钥内容。调用方负责在连接结束后清零自己的副本。
	PrivateKey []byte
	// Timeout 是 TCP 连接与握手的总时长上限。
	Timeout time.Duration
}

// HostKey 是握手时核对过的主机身份。
type HostKey struct {
	// Type 是协商到的主机密钥类型（ssh-ed25519、rsa-sha2-512…）。
	Type string
	// Fingerprint 是这把公钥的 SHA256 指纹。
	Fingerprint string
	// Algorithms 是本次握手提供的全部主机密钥算法的 SHA256 指纹（多份便于诊断）。
	Algorithms []string
	// PublicKey 是核对通过的那把公钥。
	PublicKey ssh.PublicKey
}

// ErrHostKeyMismatch 表示连到的主机不是登记的那一台。
//
// 这是本模块最不能含糊的错误：出现它就意味着“要么管理员抄错了指纹，
// 要么中间有人”。两种情况都必须停下来让人判断。
var ErrHostKeyMismatch = errors.New("backup: SFTP 主机密钥与登记的指纹不匹配")

// ErrSSHAuth 表示认证阶段失败（私钥不对、账户被锁、服务器拒绝该密钥）。
var ErrSSHAuth = errors.New("backup: SFTP 认证失败")

// Dial 建立一条 SSH 连接，并在握手阶段核对主机密钥指纹。
//
// 返回的连接由调用方关闭。**核对失败时连也会被关掉**：一个已经握过手的连接即使
// 不用于传数据，也已经把“我们愿意和它说话”这件事说出去了，没有理由留着。
func Dial(ctx context.Context, opts DialOptions, registered Fingerprint) (*ssh.Client, HostKey, error) {
	if opts.Host == "" {
		return nil, HostKey{}, errors.New("backup: 缺少主机")
	}
	port := opts.Port
	if port == 0 {
		port = DefaultSFTPPort
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultDialTimeout
	}
	if len(opts.PrivateKey) == 0 {
		return nil, HostKey{}, fmt.Errorf("%w：没有私钥", ErrSSHAuth)
	}
	signer, err := ssh.ParsePrivateKey(opts.PrivateKey)
	if err != nil {
		// 错误里不带私钥内容，也不带它的哈希：这句话只说明“解不开”。
		var missing *ssh.PassphraseMissingError
		if errors.As(err, &missing) {
			return nil, HostKey{}, fmt.Errorf("%w：这把私钥带口令，面板不支持带口令的私钥", ErrSSHAuth)
		}
		return nil, HostKey{}, fmt.Errorf("%w：私钥无法解析（PEM 或 OpenSSH 格式）", ErrSSHAuth)
	}

	seen := make([]string, 0, 4)
	var matched ssh.PublicKey
	hostKeyCallback := func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		fp := SHA256(key)
		seen = append(seen, key.Type()+" "+fp)
		if !registered.Matches(key) {
			return fmt.Errorf("%w：登记 %s，实际 %s %s（%s）",
				ErrHostKeyMismatch, registered.String(), key.Type(), fp, hostname)
		}
		matched = key
		return nil
	}

	cfg := &ssh.ClientConfig{
		User: opts.User,
		Auth: []ssh.AuthMethod{
			// 只要公钥：不回退到口令、不回退到 keyboard-interactive。
			ssh.PublicKeys(signer),
		},
		HostKeyCallback: hostKeyCallback,
		Timeout:         opts.Timeout,
		// 客户端版本串固定，便于在远端的 auth.log 里认出这次连接。
		ClientVersion: "SSH-2.0-upanel-backup",
	}

	dialer := &net.Dialer{Timeout: opts.Timeout}
	addr := net.JoinHostPort(opts.Host, strconv.Itoa(port))
	rawConn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, HostKey{}, fmt.Errorf("backup: 连接 %s 失败: %w", addr, err)
	}
	// 握手的截止时间靠 deadline 兜底：DialContext 只管 TCP，握手可能更久。
	deadline := time.Now().Add(opts.Timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = rawConn.SetDeadline(deadline)

	sshConn, chans, reqs, err := ssh.NewClientConn(rawConn, addr, cfg)
	if err != nil {
		_ = rawConn.Close()
		if errors.Is(err, ErrHostKeyMismatch) {
			return nil, HostKey{}, err
		}
		if matched == nil {
			// 走到这里且没有匹配：要么握手在主机密钥之前就失败了，要么指纹不对
			// 但错误被 ssh 包成了别的形状。后者按“密钥不匹配”报，因为那是
			// 失败关闭的一侧。
			if len(seen) > 0 {
				return nil, HostKey{}, fmt.Errorf("%w：登记 %s，实际 %s",
					ErrHostKeyMismatch, registered.String(), strings.Join(seen, "；"))
			}
		}
		return nil, HostKey{}, fmt.Errorf("%w: %v", ErrSSHAuth, err)
	}
	// 握手完成：清掉 deadline，否则长传输会被它断开。
	_ = rawConn.SetDeadline(time.Time{})

	client := ssh.NewClient(sshConn, chans, reqs)
	if matched == nil {
		_ = client.Close()
		return nil, HostKey{}, fmt.Errorf("%w：握手没有提供可比的主机公钥", ErrHostKeyMismatch)
	}
	return client, HostKey{
		Type:        matched.Type(),
		Fingerprint: SHA256(matched),
		Algorithms:  seen,
		PublicKey:   matched,
	}, nil
}

// sftpClient 打开一个 SFTP 子系统会话。
func sftpClient(client *ssh.Client) (*sftp.Client, error) {
	c, err := sftp.NewClient(client)
	if err != nil {
		return nil, fmt.Errorf("backup: 打开 SFTP 子系统失败: %w", err)
	}
	return c, nil
}

// RemoteProbe 是远端路径的一次探测结果。
type RemoteProbe struct {
	// Exists 表示远端路径本身存在。
	Exists bool
	// IsDir 表示它是一个目录（Exists 为 false 时无意义）。
	IsDir bool
	// RepoConfig 表示该路径下已经有 restic 的 config 文件——也就是“这里已经是一个仓库”。
	RepoConfig bool
	// Capacity 是最近一个存在祖先的容量读数。
	Capacity Capacity
}

// ProbeRemote 探测远端路径：存不存在、是不是目录、里面有没有仓库、所在文件系统还有多少空间。
//
// 只读：不做 mkdir、不写探测文件。预检不许改系统（CONTRACTS.md 4.1），
// 所以“能不能建”只能靠 stat 祖先目录来推断，推断的结果会作为告警说清楚。
func ProbeRemote(ctx context.Context, client *ssh.Client, remotePath string) (RemoteProbe, error) {
	var probe RemoteProbe
	c, err := sftpClient(client)
	if err != nil {
		return probe, err
	}
	defer func() { _ = c.Close() }()

	if err := ctx.Err(); err != nil {
		return probe, err
	}
	info, err := c.Stat(remotePath)
	switch {
	case err == nil:
		probe.Exists = true
		probe.IsDir = info.IsDir()
	case isNotExist(err):
		probe.Exists = false
	default:
		return probe, fmt.Errorf("backup: 读取远端路径 %s 失败: %w", remotePath, err)
	}
	if probe.Exists && probe.IsDir {
		if _, err := c.Stat(remotePath + "/config"); err == nil {
			probe.RepoConfig = true
		} else if !isNotExist(err) {
			return probe, fmt.Errorf("backup: 读取远端 %s/config 失败: %w", remotePath, err)
		}
	}

	capacity, err := remoteCapacity(ctx, c, remotePath)
	if err != nil {
		// 容量读不到不是探测失败：仓库能不能建不取决于这一个数字，
		// 但界面必须看到“没读到”以及原因（不折成 0）。
		probe.Capacity = Capacity{Supported: false, Target: remotePath, Error: err.Error(), CheckedAt: nowUTC()}
		return probe, nil
	}
	probe.Capacity = capacity
	return probe, nil
}

// remoteCapacity 用 SFTP 的 statvfs 扩展读远端文件系统的容量。
//
// 从目标路径向上找第一个存在的祖先：仓库目录通常还不存在，而“父目录所在文件系统
// 的空间”正是它将要占用的空间。
func remoteCapacity(ctx context.Context, c *sftp.Client, remotePath string) (Capacity, error) {
	path := remotePath
	for {
		if err := ctx.Err(); err != nil {
			return Capacity{}, err
		}
		st, err := c.StatVFS(path)
		if err == nil {
			bsize := int64(st.Bsize)
			return Capacity{
				Supported:      true,
				Target:         path,
				TotalBytes:     bsize * int64(st.Blocks),
				AvailableBytes: bsize * int64(st.Bavail),
				CheckedAt:      nowUTC(),
			}, nil
		}
		if !isNotExist(err) {
			return Capacity{}, fmt.Errorf("远端 %s 的容量读不到: %w", path, err)
		}
		parent := parentPath(path)
		if parent == path || parent == "" {
			return Capacity{}, fmt.Errorf("远端 %s 及其所有祖先都不存在", remotePath)
		}
		path = parent
	}
}

// parentPath 返回远端路径的父目录（根目录的父目录是它自己）。
func parentPath(p string) string {
	trimmed := strings.TrimSuffix(p, "/")
	idx := strings.LastIndex(trimmed, "/")
	if idx <= 0 {
		return "/"
	}
	return trimmed[:idx]
}

// isNotExist 判断 sftp 的“不存在”。
//
// sftp 客户端把服务端的 SSH_FX_NO_SUCH_FILE 归一成 os.ErrNotExist（normaliseError），
// 所以常见路径走 errors.Is；StatusError 直接比较码，覆盖归一化没生效的调用点。
func isNotExist(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errNotExistSentinel) {
		return true
	}
	var status *sftp.StatusError
	if errors.As(err, &status) {
		return uint32(status.Code) == uint32(sftp.ErrSSHFxNoSuchFile)
	}
	return false
}
