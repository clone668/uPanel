package backup

// restic 的调用层（P12.1）。
//
// 这一层只做“把一次仓库操作翻译成 argv + 环境”，判断在 ops.go：
//   - 仓库口令只走 RESTIC_PASSWORD（不进 argv）；
//   - SFTP 用 `-o sftp.command` 钉死一个 ssh 调用：`-F /dev/null` 忽略 ssh 配置、
//     `StrictHostKeyChecking=yes` + 我们自己写的 known_hosts、`IdentitiesOnly=yes`、
//     `BatchMode=yes`（不会有人去敲密码）。这就是契约里“禁止 StrictHostKeyChecking=no”
//     的落地方式：ssh 每次都必须核对，而对的是我们核对过的那把钥匙；
//   - 私钥文件写在 tmpfs（/run/upanel-agent）上，用完删掉并清零。
//
// 不接受的输入：任何来自请求的 restic 选项、任何 shell 片段。参数由代码拼装。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Paths 是备份模块用到的固定路径。
type Paths struct {
	// Restic 是 restic 可执行文件（绝对路径）。
	Restic string
	// SSH 是 ssh 客户端（restic 的 sftp 后端要靠它）。
	SSH string
	// StateDir 存放 restic 的 known_hosts（持久，root 独占 0700）。
	StateDir string
	// CacheDir 是 RESTIC_CACHE_DIR。
	CacheDir string
	// RunDir 是 tmpfs：SFTP 私钥只在这里短暂存在。
	RunDir string
}

// DefaultPaths 返回 Ubuntu 上的固定位置。
func DefaultPaths() Paths {
	return Paths{
		Restic:   "/usr/bin/restic",
		SSH:      "/usr/bin/ssh",
		StateDir: "/var/lib/upanel-agent/backup",
		CacheDir: "/var/lib/upanel-agent/restic-cache",
		RunDir:   "/run/upanel-agent",
	}
}

// Invocation 是一次 restic 调用的具体参数。
type Invocation struct {
	// Repository 是交给 -r 的值（本地绝对路径或 sftp:user@host:/path）。
	Repository string
	// Password 是仓库口令（进环境，不进 argv）。
	Password string
	// SFTPCommand 非空时作为 `-o sftp.command` 的值。
	SFTPCommand string
	// Timeout 覆盖默认上限。
	Timeout time.Duration
}

// Restic 是 restic 调用器。
type Restic struct {
	paths Paths
	run   func(ctx context.Context, spec ExecSpec) (ExecResult, error)
}

// NewRestic 构造调用器。run 为 nil 时用 RunRestic（测试换成假的）。
func NewRestic(paths Paths, run func(context.Context, ExecSpec) (ExecResult, error)) *Restic {
	if run == nil {
		run = RunRestic
	}
	return &Restic{paths: paths, run: run}
}

// Paths 返回实际使用的路径（诊断用）。
func (r *Restic) Paths() Paths { return r.paths }

// Available 报告 restic 在这一层看来是否可用。
//
// 只检查文件在不在：`restic version` 能不能跑由调用方去跑（那是一次 real 执行，
// 装配层在启动时不做系统调用）。返回的第二项是“为什么不可用”。
func (r *Restic) Available(stat func(string) bool) (bool, string) {
	if stat == nil {
		stat = fileExists
	}
	if !stat(r.paths.Restic) {
		return false, fmt.Sprintf("找不到 %s", r.paths.Restic)
	}
	return true, ""
}

// Version 返回 restic 自己报告的版本串（例如 "restic 0.12.1 compiled with go1.18.1 on linux/amd64"）。
func (r *Restic) Version(ctx context.Context) (string, error) {
	res, err := r.run(ctx, ExecSpec{Program: r.paths.Restic, Args: []string{"version"}, Timeout: 15 * time.Second})
	if err != nil {
		return "", fmt.Errorf("backup: 执行 restic version 失败: %w", err)
	}
	out := strings.TrimSpace(res.Stdout)
	if out == "" {
		return "", errors.New("backup: restic version 没有输出")
	}
	return strings.ReplaceAll(out, "\n", " "), nil
}

// RepoConfig 是 `restic cat config` 的解析结果。
type RepoConfig struct {
	// ID 是仓库标识（64 位十六进制）。它是“这个仓库是谁”的唯一证据。
	ID      string `json:"id"`
	Version int    `json:"version"`
	// ChunkerPolynomial 存在与否用来确认这确实是 restic 的 config。
	ChunkerPolynomial json.RawMessage `json:"chunker_polynomial"`
}

// ErrRepoExists 表示目标位置已经是一个 restic 仓库。
var ErrRepoExists = errors.New("backup: 目标位置已经是一个 restic 仓库")

// repoIDPattern 是 restic 仓库 id 的形状（64 位小写十六进制）。
var repoIDPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// CatConfig 读仓库 config。仓库不存在或口令不对时返回明确错误。
func (r *Restic) CatConfig(ctx context.Context, inv Invocation) (RepoConfig, error) {
	res, err := r.run(ctx, ExecSpec{
		Program:  r.paths.Restic,
		Args:     r.args(inv, "cat", "config"),
		Password: inv.Password,
		CacheDir: r.paths.CacheDir,
		Timeout:  inv.Timeout,
	})
	if err != nil {
		return RepoConfig{}, &resticFailure{op: "cat config", res: res, err: err}
	}
	var cfg RepoConfig
	if err := json.Unmarshal([]byte(res.Stdout), &cfg); err != nil {
		return RepoConfig{}, fmt.Errorf("backup: restic cat config 的输出不是合法 JSON: %w", err)
	}
	if !repoIDPattern.MatchString(cfg.ID) {
		return RepoConfig{}, fmt.Errorf("backup: restic 报告的仓库 id 形状不合法（%q）", cfg.ID)
	}
	return cfg, nil
}

// Init 初始化一个**新**仓库。
//
// 调用前 ops 已经确认目标位置没有 config 文件；restic 自己也会在仓库已存在时
// 拒绝（错误里带 "config file already exists"），两条一起才算“不覆盖已有数据”。
func (r *Restic) Init(ctx context.Context, inv Invocation) (ExecResult, error) {
	res, err := r.run(ctx, ExecSpec{
		Program:  r.paths.Restic,
		Args:     r.args(inv, "init"),
		Password: inv.Password,
		CacheDir: r.paths.CacheDir,
		Timeout:  inv.Timeout,
	})
	if err != nil {
		return res, &resticFailure{op: "init", res: res, err: err}
	}
	return res, nil
}

// SnapshotStats 是 `restic stats --json` 的结果（P12.2 起用得多，这里先留一条读数）。
type SnapshotStats struct {
	TotalSize      int64 `json:"total_size"`
	TotalFileCount int64 `json:"total_file_count"`
	SnapshotsCount int64 `json:"snapshots_count"`
}

// Stats 读仓库当前统计。
func (r *Restic) Stats(ctx context.Context, inv Invocation) (SnapshotStats, error) {
	res, err := r.run(ctx, ExecSpec{
		Program:  r.paths.Restic,
		Args:     r.args(inv, "stats", "--json"),
		Password: inv.Password,
		CacheDir: r.paths.CacheDir,
		Timeout:  inv.Timeout,
	})
	if err != nil {
		return SnapshotStats{}, &resticFailure{op: "stats", res: res, err: err}
	}
	var stats SnapshotStats
	if err := json.Unmarshal([]byte(res.Stdout), &stats); err != nil {
		return SnapshotStats{}, fmt.Errorf("backup: restic stats 的输出不是合法 JSON: %w", err)
	}
	return stats, nil
}

// args 拼装一次调用的 argv：仓库地址、可选选项、子命令。
func (r *Restic) args(inv Invocation, sub ...string) []string {
	out := []string{"-r", inv.Repository}
	if inv.SFTPCommand != "" {
		out = append(out, "-o", "sftp.command="+inv.SFTPCommand)
	}
	return append(out, sub...)
}

// resticFailure 把一次子进程失败翻成一句人话，并保留原始输出供审计。
//
// restic 的报错形状是 `Fatal: unable to open config file: Stat: ...` 这类一行文本，
// 认得出的几种翻成中文并带上码；认不出的原样保留最后一行（不含 argv、不含口令）。
type resticFailure struct {
	op  string
	res ExecResult
	err error
}

func (f *resticFailure) Error() string  { return f.FailureReason() }
func (f *resticFailure) Unwrap() error  { return f.err }
func (f *resticFailure) Reason() string { return f.FailureReason() }
func (f *resticFailure) FailureReason() string {
	line := lastLine(f.res.Stderr)
	if line == "" {
		line = lastLine(f.res.Stdout)
	}
	lower := strings.ToLower(line)
	switch {
	case strings.Contains(lower, "config file already exists"):
		return "目标位置已经是一个 restic 仓库（不会覆盖已有数据）"
	case strings.Contains(lower, "wrong password") || strings.Contains(lower, "invalid password"):
		return "仓库口令不正确（RESTIC 拒绝了这次解密）"
	case strings.Contains(lower, "no such file") && strings.Contains(lower, "config"):
		return "目标位置还不是一个 restic 仓库"
	case strings.Contains(lower, "permission denied"):
		return "没有权限读写目标位置：" + line
	}
	if line == "" {
		if f.err != nil {
			return fmt.Sprintf("restic %s 失败：%v", f.op, f.err)
		}
		return fmt.Sprintf("restic %s 失败", f.op)
	}
	return fmt.Sprintf("restic %s 失败：%s", f.op, line)
}

// knownHostsPath 是某个远端主机（含端口）的 known_hosts 文件路径。
//
// 按 host:port 分文件而不是共用一个：一个文件里放着多台主机的钥匙时，
// “核对失败”会发生在 ssh 内部，而我们要在写出之前就知道自己在给谁写。
func (r *Restic) knownHostsPath(host string, port int) string {
	sum := sha256.Sum256([]byte(host + ":" + strconv.Itoa(port)))
	return filepath.Join(r.paths.StateDir, "known_hosts", hex.EncodeToString(sum[:8]))
}

// WriteKnownHosts 把核对过的主机公钥写进我们自己的 known_hosts（0600，root 独占）。
//
// 这一步是**执行阶段**的写盘；预检不写。写之前必须已经核对过指纹，否则就是
// 把一台没核对过的主机钉进来——那比不钉还糟。
func (r *Restic) WriteKnownHosts(host string, port int, pubLine string) (string, error) {
	if strings.TrimSpace(pubLine) == "" {
		return "", errors.New("backup: known_hosts 行为空")
	}
	dir := filepath.Join(r.paths.StateDir, "known_hosts")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("backup: 创建 known_hosts 目录失败: %w", err)
	}
	path := r.knownHostsPath(host, port)
	tmp := path + ".tmp"
	content := strings.TrimSpace(pubLine) + "\n"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		return "", fmt.Errorf("backup: 写 known_hosts 失败: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("backup: 落位 known_hosts 失败: %w", err)
	}
	return path, nil
}

// SFTPCommand 拼出 `-o sftp.command` 的值。
//
// 逐项都是我们自己算出来的常量或已校验的字段（host 只允许主机名/IP 字符集、
// username 只允许 POSIX 用户名字符集、路径里没有控制字符）。空格只出现在选项之间，
// 因此无论 restic 用词法切分还是交给 sh，结果都一样。
func SFTPCommand(sshPath, knownHostsPath, keyPath string, host string, port int, user string) string {
	parts := []string{
		sshPath,
		"-F", "/dev/null", // 不读任何 ssh 配置：别人写进 /root/.ssh/config 的选项不生效
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=yes",
		"-o", "UserKnownHostsFile=" + knownHostsPath,
		"-o", "GlobalKnownHostsFile=/dev/null",
		"-o", "IdentitiesOnly=yes",
		"-o", "PasswordAuthentication=no",
		"-o", "KbdInteractiveAuthentication=no",
		"-o", "ConnectTimeout=15",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=4",
		"-i", keyPath,
		"-p", strconv.Itoa(port),
		user + "@" + host,
		"-s", "sftp",
	}
	return strings.Join(parts, " ")
}

// WritePrivateKey 把 SFTP 私钥写到 tmpfs 上的一个临时文件（0600），返回路径。
//
// 为什么在 /run 里：私钥是秘密材料，落盘就意味着它可能被留在磁盘快照、
// 被某次备份带走。/run 是 tmpfs，进程结束后随内存消失；再加上调用方读后立即清零
// 并删除，暴露窗口被压到一次调用的长度。
func (r *Restic) WritePrivateKey(name string, key []byte) (string, error) {
	if len(key) == 0 {
		return "", errors.New("backup: SFTP 私钥为空")
	}
	dir := filepath.Join(r.paths.RunDir, "backup-keys")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("backup: 创建临时目录失败: %w", err)
	}
	sum := sha256.Sum256([]byte(name))
	path := filepath.Join(dir, hex.EncodeToString(sum[:8])+".key")
	// 先删再建：同名残留（上次崩溃留下的）不允许被复用。
	_ = os.Remove(path)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("backup: 创建私钥临时文件失败: %w", err)
	}
	if _, err := f.Write(key); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return "", fmt.Errorf("backup: 写入私钥临时文件失败: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("backup: 关闭私钥临时文件失败: %w", err)
	}
	return path, nil
}

// RemovePrivateKey 删除临时私钥文件。
func RemovePrivateKey(path string) {
	if path != "" {
		_ = os.Remove(path)
	}
}

// fileExists 是 os.Stat 的一层封装。
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
