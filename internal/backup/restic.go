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
	"io"
	"os"
	"path"
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

// resticIDPattern 是 restic 里 64 位小写十六进制 id 的形状。
// 仓库 id 与快照 id 长得一样，也都只在这两个地方出现，共用一个模式。
var resticIDPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// resticShortIDPattern 是 restic 报的短 id（8 位小写十六进制）。
//
// 它是**前缀**而不是身份：restic 0.12 的 `backup --json` summary 只给短 id，
// 0.14 起给完整 id。短 id 只能用来在仓库里定位一条记录，不能直接写进结果。
var resticShortIDPattern = regexp.MustCompile(`^[0-9a-f]{4,63}$`)

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
	if !resticIDPattern.MatchString(cfg.ID) {
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
	// 时间上的事直接照说：这两句本身已经是完整的话，再加"restic cat config 失败："
	// 只会让人以为是仓库或口令的问题。
	if errors.Is(f.err, ErrBudgetExhausted) || errors.Is(f.err, ErrExecTimeout) {
		return f.err.Error()
	}
	line := lastLine(f.res.Stderr)
	if line == "" {
		// restic 带 --json 时可能把错误写成一行 JSON 混在 stdout 里；
		// 那种情况下"最后一行"往往只是进度，得先把真正的 error 消息捞出来。
		if msg := jsonErrorLine(f.res.Stdout); msg != "" {
			return fmt.Sprintf("restic %s 失败（%s）：%s", f.op, exitSummary(f.res), msg)
		}
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
	// 退出状态与信号名必须带进这句话里。
	//
	// 原来这里只留了输出最后一行，于是"restic 以退出状态 3 结束"和"restic 被 SIGPIPE
	// 杀掉"都被说成同一句"失败：{一行 JSON}"——24.04 上真机撞到的正是这个：
	// restic 明明把快照写成了，代理却报了一句看不出所以然的话，定位只能靠猜。
	return fmt.Sprintf("restic %s 失败（%s）：%s", f.op, exitSummary(f.res), withFirstError(line, f.res.Stderr))
}

// withFirstError 在 restic 只交了汇总行时补上第一条具体错误。
//
// `restic restore` 写满磁盘时 stderr 的最后一行是 `Fatal: There were 10 errors`——
// 它告诉你"错了十次"，却不告诉你为什么。真正的原因在它上面几行（`error: ...:
// no space left on device`）。只留汇总的话，面板上的失败消息对运维没有任何信息量，
// 而这正是"恢复失败"最需要一句话说清的场景。
func withFirstError(summary, stderr string) string {
	if !strings.HasPrefix(summary, "Fatal:") {
		return summary
	}
	cause := firstErrorLine(stderr)
	if cause == "" || strings.Contains(summary, cause) {
		return summary
	}
	return summary + "；首个错误：" + cause
}

// firstErrorLine 找 restic 的第一条具体错误行。
//
// restic 的具体错误不止一种说法：`check` 用 `error: ...`，`restore` 对每个写不下去的
// 文件用 `ignoring error for <path>: <err>`（2026-10-25 在 24.04 / restic 0.16.4 上实地
// 抓到的原文：`ignoring error for /x/d1/f2: write /y/f2: no space left on device`）。
// 只认前者，恢复写满时剩下的就只有 `Fatal: There were 11 errors`。
//
// 跳过 `Fatal:` 那一行：那是汇总，不是原因。两者都不命中时退回第一条非空非汇总行——
// 那仍然是 restic 自己写下的一行，比只剩一个错误计数有用。都没有就返回空，调用方保持原样。
func firstErrorLine(stderr string) string {
	var fallback string
	for _, raw := range strings.Split(stderr, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "Fatal:") {
			continue
		}
		lower := strings.ToLower(line)
		if strings.HasPrefix(lower, "error:") || strings.HasPrefix(lower, "ignoring error for") ||
			strings.Contains(lower, " error: ") {
			return line
		}
		if fallback == "" {
			fallback = line
		}
	}
	return fallback
}

// exitSummary 把退出状态与信号名合成一句能直接看的话。
//
// 退出码为 -1 表示进程没有正常退出（被杀、被信号终止）；这时有信号名就是全部事实，
// 没有信号名才退回"退出状态 -1"。
func exitSummary(res ExecResult) string {
	if res.Signal != "" {
		return "被 " + res.Signal + " 终止"
	}
	return fmt.Sprintf("退出状态 %d", res.ExitCode)
}

// jsonErrorLine 从 restic 的 JSON 输出里找出一条 error 消息。
//
// restic 带 --json 时，错误可能是 `{"message_type":"error","message":"..."}` 这样的
// 一行（也可能不带 message_type，只有 message）。找不到就返回空串。
func jsonErrorLine(stdout string) string {
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var msg struct {
			MessageType string `json:"message_type"`
			Message     string `json:"message"`
			Error       string `json:"error"`
		}
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}
		if msg.MessageType != "error" && msg.Error == "" {
			continue
		}
		text := msg.Message
		if text == "" {
			text = msg.Error
		}
		if text != "" {
			return text
		}
	}
	return ""
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

// ResticSnapshot 是 `restic snapshots --json` 里的一条。
//
// 只解我们真会用的字段：id 是身份，time 是排序与展示的依据，paths 是"这份备份
// 包含什么"，tags 里有我们自己打的策略与口径标记。其余字段（tree、uid、gid）
// 留在 restic 那边——多解一个字段就多一处可能与版本相关的地方。
type ResticSnapshot struct {
	ID             string   `json:"id"`
	ShortID        string   `json:"short_id"`
	Time           string   `json:"time"`
	Hostname       string   `json:"hostname"`
	Paths          []string `json:"paths"`
	Tags           []string `json:"tags"`
	ProgramVersion string   `json:"program_version"`
}

// BackupOptions 是一次 `restic backup` 的参数。
type BackupOptions struct {
	// Tags 是写进快照的标签（策略与一致性口径都在里面）。
	Tags []string
	// Paths 是要备份的绝对路径（至少一个）。
	Paths []string
	// Excludes 是要排除的绝对路径前缀（/proc、/sys、/dev）。
	Excludes []string
	// Timeout 覆盖默认上限。
	Timeout time.Duration
}

// BackupProgress 是 `restic backup --json` 的 status 消息。
type BackupProgress struct {
	PercentDone float64
	FilesDone   int64
	TotalFiles  int64
	BytesDone   int64
	TotalBytes  int64
	CurrentFile string
}

// BackupSummary 是 `restic backup --json` 的 summary 消息。
//
// SnapshotID 是**唯一**能证明"快照真的建成了"的东西：退出码 0 只说明命令跑完了。
type BackupSummary struct {
	SnapshotID      string
	FilesNew        int64
	FilesChanged    int64
	FilesUnmodified int64
	DataAdded       int64
	TotalFiles      int64
	TotalBytes      int64
	Duration        time.Duration
}

// resticMessage 是 `--json` 输出的一行。
//
// 指针字段是故意的：`percent_done: 0` 与"没有这个键"必须分得开（前者是 0%，
// 后者是消息类型不同）。
type resticMessage struct {
	MessageType  string   `json:"message_type"`
	PercentDone  *float64 `json:"percent_done"`
	FilesDone    *int64   `json:"files_done"`
	TotalFiles   *int64   `json:"total_files"`
	BytesDone    *int64   `json:"bytes_done"`
	TotalBytes   *int64   `json:"total_bytes"`
	CurrentFiles []string `json:"current_files"`

	SnapshotID          string  `json:"snapshot_id"`
	FilesNew            int64   `json:"files_new"`
	FilesChanged        int64   `json:"files_changed"`
	FilesUnmodified     int64   `json:"files_unmodified"`
	DataAdded           int64   `json:"data_added"`
	TotalFilesProcessed int64   `json:"total_files_processed"`
	TotalBytesProcessed int64   `json:"total_bytes_processed"`
	TotalDuration       float64 `json:"total_duration"`

	Error  json.RawMessage `json:"error"`
	Item   string          `json:"item"`
	During string          `json:"during"`
}

// Backup 执行一次 `restic backup --json`。
//
// onStatus 非空时每次 status 消息都会回调一次（进度进任务事件流）；即使不回调，
// 结果里的 summary 也是从同一份输出里解析出来的——两条路径共用一个解析器，
// 免得"实时进度"与"最终结论"读的不是同一个东西。
func (r *Restic) Backup(ctx context.Context, inv Invocation, opts BackupOptions,
	onStatus func(BackupProgress)) (BackupSummary, error) {
	if len(opts.Paths) == 0 {
		return BackupSummary{}, errors.New("backup: 没有要备份的路径")
	}
	args := r.args(inv, "backup", "--json")
	for _, tag := range opts.Tags {
		args = append(args, "--tag", tag)
	}
	for _, ex := range opts.Excludes {
		args = append(args, "--exclude", ex)
	}
	args = append(args, "--")
	args = append(args, opts.Paths...)

	res, err := r.run(ctx, ExecSpec{
		Program:  r.paths.Restic,
		Args:     args,
		Password: inv.Password,
		CacheDir: r.paths.CacheDir,
		Timeout:  opts.Timeout,
		// 进度要实时：stdout 边读边解析，落盘的那一份留给结论。
		OnStdoutLine: func(line string) {
			if onStatus == nil {
				return
			}
			if p, ok := parseStatusLine(line); ok {
				onStatus(p)
			}
		},
	})
	summary, parseErr := parseBackupSummary(res.Stdout)
	if err != nil {
		if parseErr != nil {
			return summary, fmt.Errorf("%w（输出也无法解析：%v）", err, parseErr)
		}
		return summary, &resticFailure{op: "backup", res: res, err: err}
	}
	if parseErr != nil {
		return summary, parseErr
	}
	if summary.SnapshotID == "" {
		return summary, errors.New("backup: restic 没有报告快照 id（退出码 0 不等于快照建成了）")
	}
	// 这里只挡形状不对的 id（既不是完整 id 也不是短 id）；短 id 要到 run.go 里
	// 回仓库解析成完整 id 才算数——restic 0.12 的 summary 只报 8 位。
	if !resticIDPattern.MatchString(summary.SnapshotID) && !resticShortIDPattern.MatchString(summary.SnapshotID) {
		return summary, fmt.Errorf("backup: restic 报告的快照 id 形状不合法（%q）", summary.SnapshotID)
	}
	return summary, nil
}

// parseStatusLine 解析一行 status 消息。
func parseStatusLine(line string) (BackupProgress, bool) {
	var msg resticMessage
	if err := json.Unmarshal([]byte(line), &msg); err != nil {
		return BackupProgress{}, false
	}
	if msg.MessageType != "status" {
		return BackupProgress{}, false
	}
	p := BackupProgress{}
	if msg.PercentDone != nil {
		p.PercentDone = *msg.PercentDone
	}
	if msg.FilesDone != nil {
		p.FilesDone = *msg.FilesDone
	}
	if msg.TotalFiles != nil {
		p.TotalFiles = *msg.TotalFiles
	}
	if msg.BytesDone != nil {
		p.BytesDone = *msg.BytesDone
	}
	if msg.TotalBytes != nil {
		p.TotalBytes = *msg.TotalBytes
	}
	if len(msg.CurrentFiles) > 0 {
		p.CurrentFile = msg.CurrentFiles[0]
	}
	return p, true
}

// parseBackupSummary 从整份输出里取 summary 与 error 消息。
func parseBackupSummary(stdout string) (BackupSummary, error) {
	var out BackupSummary
	var errs []string
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var msg resticMessage
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}
		switch msg.MessageType {
		case "summary":
			out.SnapshotID = msg.SnapshotID
			out.FilesNew = msg.FilesNew
			out.FilesChanged = msg.FilesChanged
			out.FilesUnmodified = msg.FilesUnmodified
			out.DataAdded = msg.DataAdded
			out.TotalFiles = msg.TotalFilesProcessed
			out.TotalBytes = msg.TotalBytesProcessed
			if msg.TotalDuration > 0 {
				out.Duration = time.Duration(msg.TotalDuration * float64(time.Second))
			}
		case "error":
			text := lastLine(string(msg.Error))
			if text == "" {
				text = "restic 报告了一个错误但没给内容"
			}
			if msg.Item != "" {
				text += "（对象：" + msg.Item + "）"
			}
			errs = append(errs, text)
		}
	}
	if len(errs) > 0 {
		// 有 error 消息时即便退出码是 0 也不能算成功：那些文件没备份进去。
		return out, fmt.Errorf("backup: restic 报告了 %d 个错误：%s", len(errs), strings.Join(errs, "；"))
	}
	return out, nil
}

// Snapshots 列仓库里的快照（可按标签过滤）。
func (r *Restic) Snapshots(ctx context.Context, inv Invocation, tags []string) ([]ResticSnapshot, error) {
	args := r.args(inv, "snapshots", "--json")
	for _, tag := range tags {
		args = append(args, "--tag", tag)
	}
	res, err := r.run(ctx, ExecSpec{
		Program:   r.paths.Restic,
		Args:      args,
		Password:  inv.Password,
		CacheDir:  r.paths.CacheDir,
		Timeout:   inv.Timeout,
		MaxStdout: 8 << 20,
	})
	if err != nil {
		return nil, &resticFailure{op: "snapshots", res: res, err: err}
	}
	trimmed := strings.TrimSpace(res.Stdout)
	if trimmed == "" {
		// 老版本在空仓库上可能什么都不打印；一个空格子不是错误。
		return nil, nil
	}
	var out []ResticSnapshot
	if err := json.Unmarshal([]byte(trimmed), &out); err != nil {
		return nil, fmt.Errorf("backup: restic snapshots 的输出不是合法 JSON: %w", err)
	}
	return out, nil
}

// SnapshotStat 读单个快照的统计（`restic stats <id> --json`）。
func (r *Restic) SnapshotStat(ctx context.Context, inv Invocation, id string) (SnapshotStats, error) {
	if !resticIDPattern.MatchString(id) {
		return SnapshotStats{}, fmt.Errorf("backup: 快照 id 形状不合法（%q）", id)
	}
	res, err := r.run(ctx, ExecSpec{
		Program:  r.paths.Restic,
		Args:     r.args(inv, "stats", id, "--json"),
		Password: inv.Password,
		CacheDir: r.paths.CacheDir,
		Timeout:  inv.Timeout,
	})
	if err != nil {
		return SnapshotStats{}, &resticFailure{op: "stats", res: res, err: err}
	}
	var stats SnapshotStats
	if err := json.Unmarshal([]byte(strings.TrimSpace(res.Stdout)), &stats); err != nil {
		return SnapshotStats{}, fmt.Errorf("backup: restic stats 的输出不是合法 JSON: %w", err)
	}
	return stats, nil
}

// RestoreNode 是 `restic ls --json` 返回的一个节点。Path 始终是快照里的绝对路径，
// 恢复目录里的名字由 path.Clean 后去掉开头的斜杠得到；绝不把它当宿主绝对路径打开。
type RestoreNode struct {
	Path string `json:"path"`
	Type string `json:"type"`
	Size int64  `json:"size"`
	Mode uint32 `json:"mode"`
	UID  uint32 `json:"uid"`
	GID  uint32 `json:"gid"`
}

type resticListLine struct {
	MessageType string `json:"message_type"`
	StructType  string `json:"struct_type"`
	RestoreNode
}

// ListNodes 读快照的真实目录清单。50,001 个节点即拒绝，和文件模块的遍历边界一致。
func (r *Restic) ListNodes(ctx context.Context, inv Invocation, id string) ([]RestoreNode, error) {
	if !resticIDPattern.MatchString(id) {
		return nil, fmt.Errorf("backup: 快照 id 形状不合法（%q）", id)
	}
	res, err := r.run(ctx, ExecSpec{
		Program: r.paths.Restic, Args: r.args(inv, "ls", id, "--json"),
		Password: inv.Password, CacheDir: r.paths.CacheDir, Timeout: inv.Timeout,
		MaxStdout: 64 << 20,
	})
	if err != nil {
		return nil, &resticFailure{op: "ls", res: res, err: err}
	}
	if res.StdoutTruncated {
		return nil, errors.New("backup: 快照清单超过 64 MiB，拒绝恢复")
	}
	lines := strings.Split(strings.TrimSpace(res.Stdout), "\n")
	out := make([]RestoreNode, 0, len(lines))
	seen := make(map[string]struct{}, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var item resticListLine
		if err := json.Unmarshal([]byte(line), &item); err != nil {
			return nil, fmt.Errorf("backup: restic ls 输出了无法识别的行: %w", err)
		}
		if item.StructType != "node" && item.MessageType != "node" {
			continue
		}
		n := item.RestoreNode
		if !strings.HasPrefix(n.Path, "/") || path.Clean(n.Path) != n.Path || n.Path == "/" {
			return nil, fmt.Errorf("backup: 快照含不安全的节点路径 %q", n.Path)
		}
		if _, ok := seen[n.Path]; ok {
			return nil, fmt.Errorf("backup: 快照含重复节点路径 %q", n.Path)
		}
		seen[n.Path] = struct{}{}
		out = append(out, n)
		if len(out) > 50000 {
			return nil, errors.New("backup: 快照节点超过 50000，拒绝恢复")
		}
	}
	return out, nil
}

// Restore 把快照写进调用方已经安全打开的暂存目录。target 可以是 /proc/self/fd/N；
// 这是代理自己持有的目录句柄，不是请求参数。Ubuntu 22.04/24.04 的 restic 都没有
// --no-owner，因此发布前由 files.RestoreSession 在句柄内核验类型并归一所有权。
func (r *Restic) Restore(ctx context.Context, inv Invocation, id, target string, includes []string) error {
	if !resticIDPattern.MatchString(id) {
		return fmt.Errorf("backup: 快照 id 形状不合法（%q）", id)
	}
	if target == "" || !path.IsAbs(target) {
		return errors.New("backup: 恢复暂存目录必须是代理给定的绝对路径")
	}
	spec := ExecSpec{
		Program: r.paths.Restic, Password: inv.Password,
		CacheDir: r.paths.CacheDir, Timeout: inv.Timeout, MaxStdout: 1 << 20,
	}
	var borrowed *os.File
	if strings.HasPrefix(target, "/proc/self/fd/") {
		raw := strings.TrimPrefix(target, "/proc/self/fd/")
		fd, err := strconv.Atoi(raw)
		if err != nil || fd < 0 || strconv.Itoa(fd) != raw {
			return fmt.Errorf("backup: 恢复暂存目录句柄不合法（%q）", target)
		}
		// files 层始终拥有原 fd。这里复制一个独立描述符给本次子进程，
		// 返回时只关闭副本，GC 与 os.File 生命周期都不可能关掉父会话。
		borrowed, err = dupRestoreFD(fd)
		if err != nil {
			return err
		}
		defer func() { _ = borrowed.Close() }()
		spec.ExtraFiles = []*os.File{borrowed}
		target = "/proc/self/fd/3"
	}
	args := []string{"restore", id, "--target", target}
	for _, include := range includes {
		if !strings.HasPrefix(include, "/") || path.Clean(include) != include {
			return fmt.Errorf("backup: 恢复抽样路径不合法（%q）", include)
		}
		args = append(args, "--include", exactIncludePattern(include))
	}
	spec.Args = r.args(inv, args...)
	res, err := r.run(ctx, spec)
	if err != nil {
		return &resticFailure{op: "restore", res: res, err: err}
	}
	return nil
}

// exactIncludePattern 把 restic 的 include 模式固定成一个字面路径。
// 快照里的 *、?、[ 不能变成调用方没选择的额外文件，否则抽样空间限额就失效。
func exactIncludePattern(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch r {
		case '\\', '*', '?', '[':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// DumpSHA256 从仓库重新读一个文件并计算哈希。输出流直接进 hash，不把文件放进内存。
func (r *Restic) DumpSHA256(ctx context.Context, inv Invocation, id, name string) (string, int64, error) {
	if !resticIDPattern.MatchString(id) || !strings.HasPrefix(name, "/") || path.Clean(name) != name {
		return "", 0, errors.New("backup: dump 的快照 id 或节点路径不合法")
	}
	h := sha256.New()
	written := &countingWriter{w: h}
	res, err := r.run(ctx, ExecSpec{
		Program: r.paths.Restic, Args: r.args(inv, "dump", id, name), Password: inv.Password,
		CacheDir: r.paths.CacheDir, Timeout: inv.Timeout, MaxStdout: 64 << 10,
		StdoutWriter: written,
	})
	if err != nil {
		return "", 0, &resticFailure{op: "dump", res: res, err: err}
	}
	return hex.EncodeToString(h.Sum(nil)), written.n, nil
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (w *countingWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	w.n += int64(n)
	return n, err
}

// Check 检查仓库索引与快照结构。它不是恢复验证，也不会写 restore_verified_at。
func (r *Restic) Check(ctx context.Context, inv Invocation) error {
	res, err := r.runLocked(ctx, inv, []string{"check"})
	if err != nil {
		return &resticFailure{op: "check", res: res, err: err}
	}
	return nil
}

// Unlock 清掉仓库里的失效锁（`restic unlock`，**不带** --remove-all）。
//
// 安全性由 restic 自己保证：不带 --remove-all 的 unlock 只删"持有者已经不在了"的锁，
// 另一路还在跑的 restic 手里的锁不会被拿走。所以这一步可以在失败后无条件重试一次。
func (r *Restic) Unlock(ctx context.Context, inv Invocation) error {
	res, err := r.run(ctx, ExecSpec{Program: r.paths.Restic, Args: r.args(inv, "unlock"),
		Password: inv.Password, CacheDir: r.paths.CacheDir, Timeout: inv.Timeout})
	if err != nil {
		return &resticFailure{op: "unlock", res: res, err: err}
	}
	return nil
}

// runLocked 跑一次需要锁的 restic 命令；撞上仓库里的残留锁就清掉再重试一次。
//
// 为什么由产品自己清：面板是这台机器上唯一碰得到仓库的东西——口令只经标准输入、
// 私钥只在 tmpfs 里存在一小会儿，运维手上没有能直接跑的 restic。restic 的报错却是
// "the `unlock` command can be used to remove stale locks"，等于让运维去做一件他
// 做不到的事，仓库就此卡死。上一轮真机验收里，一次被掐断的验收留下的残留锁就让
// `backup.verify` 和 `backup.prune` 全线失败，正是这个形状。
func (r *Restic) runLocked(ctx context.Context, inv Invocation, args []string) (ExecResult, error) {
	spec := ExecSpec{Program: r.paths.Restic, Args: r.args(inv, args...),
		Password: inv.Password, CacheDir: r.paths.CacheDir, Timeout: inv.Timeout}
	res, err := r.run(ctx, spec)
	if err == nil || !needsUnlock(res) {
		return res, err
	}
	unlockErr := r.Unlock(ctx, inv)
	if unlockErr != nil {
		// 清不掉就照原样报：把 unlock 的失败盖在原始失败上会丢掉真正的原因。
		return res, err
	}
	return r.run(ctx, spec)
}

// needsUnlock 判断这次失败是不是"仓库里有残留锁"。
//
// 认 restic 的两句话：`repository is already locked ...`（锁还在）和
// `... the unlock command can be used to remove stale locks`（锁已失效）。
// 两句都要出现 unlock 这一味，避免把别的失败误当成锁问题再跑一次——重试本身不是免费的。
func needsUnlock(res ExecResult) bool {
	text := strings.ToLower(res.Stderr + "\n" + res.Stdout)
	if !strings.Contains(text, "unlock") {
		return false
	}
	return strings.Contains(text, "stale lock") || strings.Contains(text, "already locked")
}

// ForgetPrune 只删除显式列出的完整快照 id，并立即清理不再被引用的数据。
func (r *Restic) ForgetPrune(ctx context.Context, inv Invocation, ids []string) error {
	if len(ids) == 0 {
		return errors.New("backup: 没有可清理的快照 id")
	}
	args := []string{"forget", "--prune"}
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if !resticIDPattern.MatchString(id) {
			return fmt.Errorf("backup: 待清理快照 id 形状不合法（%q）", id)
		}
		if _, ok := seen[id]; ok {
			return fmt.Errorf("backup: 待清理快照 id 重复（%s）", id)
		}
		seen[id] = struct{}{}
		args = append(args, id)
	}
	res, err := r.runLocked(ctx, inv, args)
	if err != nil {
		return &resticFailure{op: "forget --prune", res: res, err: err}
	}
	return nil
}
