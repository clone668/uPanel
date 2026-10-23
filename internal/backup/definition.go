package backup

// 备份仓库的定义与校验（P12.1；CONTRACTS.md 第 5、12 节）。
//
// 一条定义只描述“仓库在哪、用哪份秘密”——它**不含**任何明文：仓库口令与 SFTP 私钥
// 都只以 `secret_ref` 的形式出现，代理在执行时自己去解密存储里取。
//
// 后端按 `backend` 判别 `location`：
//   - local：root_id + relative_path，相对受管理文件根（和文件模块同一张根表）；
//   - sftp：host、port、username、remote_path、host_key_fingerprint。
//
// `location` 用一个扁平结构承载两种后端，而不是 `json.RawMessage`：扁平结构的
// 未知字段会被 DecodeParams 的 DisallowUnknownFields 直接拒绝，而 RawMessage 会把
// “解析”推迟到某个调用点，那是第二套宽容度的起点。**不属于该后端的字段一律报错**，
// 不静默忽略——一条带着 `host` 字段的 local 定义说明有人在别的后端上改了半个定义。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
)

// Action / 查询的固定名称（CONTRACTS.md 第 5 节）。
const (
	// ActionRepositoryInitialize 是仓库初始化动作名（冻结）。
	ActionRepositoryInitialize = "backup.repository.initialize"
	// QueryRepositoryState 是仓库现状的只读查询名。
	QueryRepositoryState = "backup.repository.state"
)

// Backend 是仓库后端。1.0 只支持这两种（PLAN 第 17 节）。
type Backend string

const (
	// BackendLocal 是本机目录。
	BackendLocal Backend = "local"
	// BackendSFTP 是 SFTP 服务器上的目录（restic 的 sftp 后端）。
	BackendSFTP Backend = "sftp"
)

// Valid 判断后端是否在闭集内。
func (b Backend) Valid() bool { return b == BackendLocal || b == BackendSFTP }

// Location 是仓库位置的联合体。
//
// 两种后端共用一张结构，字段按 backend 互斥：local 用 RootID/RelativePath，
// sftp 用 Host/Port/Username/RemotePath/HostKeyFingerprint。
type Location struct {
	RootID       string `json:"root_id,omitempty"`
	RelativePath string `json:"relative_path,omitempty"`

	Host               string `json:"host,omitempty"`
	Port               int    `json:"port,omitempty"`
	Username           string `json:"username,omitempty"`
	RemotePath         string `json:"remote_path,omitempty"`
	HostKeyFingerprint string `json:"host_key_fingerprint,omitempty"`
}

// Definition 是一条仓库定义。
type Definition struct {
	Name      string   `json:"name"`
	Backend   Backend  `json:"backend"`
	Location  Location `json:"location"`
	// PasswordSecretRef 指向 backup_repository_password 用途的秘密。
	PasswordSecretRef string `json:"password_secret_ref"`
	// CredentialSecretRef 指向 sftp_private_key 用途的秘密；local 后端必须为空。
	CredentialSecretRef string `json:"credential_secret_ref,omitempty"`
}

// Definition 的长度上限。它们不是“能省就省”的常数：每一条都对应一个真实的
// 落盘/落库位置（仓库名进界面与审计、路径进 restic 的 argv、指纹进 known_hosts）。
const (
	// MaxNameLength 是仓库显示名上限。
	MaxNameLength = 64
	// MaxRootIDLength 是文件根标识上限。
	MaxRootIDLength = 64
	// MaxPathLength 是相对路径/远端路径上限。
	MaxPathLength = 1024
	// MaxHostLength 是主机名长度上限（DNS 名的最长形式是 253）。
	MaxHostLength = 253
	// MaxUsernameLength 是远端用户名上限。
	MaxUsernameLength = 32
	// DefaultSFTPPort 是未给出端口时的 SFTP 端口。
	DefaultSFTPPort = 22
)

// ErrInvalid 表示定义本身不合法（参数层错误）。
var ErrInvalid = errors.New("backup: 仓库定义不合法")

func invalid(field, format string, args ...any) error {
	return fmt.Errorf("%w: %s %s", ErrInvalid, field, fmt.Sprintf(format, args...))
}

// rootIDPattern 是文件根标识的形状：来自配置，不是请求方随便起的名字。
var rootIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

// usernamePattern 是 SFTP 用户名的形状（POSIX 可移植字符集的常见子集）。
var usernamePattern = regexp.MustCompile(`^[a-z_][a-z0-9._-]{0,31}$`)

// Normalize 校验并规范化定义。它只做**形状**校验：根是否存在、主机能不能连上、
// 指纹对不对，都是预检才去核的事实（预检读系统，解析只认字段）。
func (d Definition) Normalize() (Definition, error) {
	out := d

	out.Name = strings.TrimSpace(out.Name)
	if out.Name == "" {
		return Definition{}, invalid("name", "不能为空")
	}
	if len(out.Name) > MaxNameLength {
		return Definition{}, invalid("name", "不能超过 %d 字节", MaxNameLength)
	}
	if err := checkPrintable("name", out.Name); err != nil {
		return Definition{}, err
	}

	out.Backend = Backend(strings.TrimSpace(string(out.Backend)))
	if !out.Backend.Valid() {
		return Definition{}, invalid("backend", "只能是 local 或 sftp（收到 %q）", string(out.Backend))
	}

	out.PasswordSecretRef = strings.TrimSpace(out.PasswordSecretRef)
	if out.PasswordSecretRef == "" {
		return Definition{}, invalid("password_secret_ref", "不能为空：仓库口令只能通过秘密引用给出")
	}
	if !validSecretRef(out.PasswordSecretRef) {
		return Definition{}, invalid("password_secret_ref", "不是合法的秘密引用（应形如 sec_ 加 32 位小写十六进制）")
	}

	out.CredentialSecretRef = strings.TrimSpace(out.CredentialSecretRef)

	switch out.Backend {
	case BackendLocal:
		if err := out.Location.normalizeLocal(); err != nil {
			return Definition{}, err
		}
		if out.CredentialSecretRef != "" {
			return Definition{}, invalid("credential_secret_ref", "本地仓库没有远端凭据，该字段必须为空")
		}
	case BackendSFTP:
		if err := out.Location.normalizeSFTP(); err != nil {
			return Definition{}, err
		}
		if out.CredentialSecretRef == "" {
			return Definition{}, invalid("credential_secret_ref",
				"SFTP 仓库必须给出私钥引用：面板不接受口令式 SSH 认证，也不使用代理自己的 SSH 密钥")
		}
		if !validSecretRef(out.CredentialSecretRef) {
			return Definition{}, invalid("credential_secret_ref", "不是合法的秘密引用（应形如 sec_ 加 32 位小写十六进制）")
		}
	}
	return out, nil
}

// normalizeLocal 校验本地位置，并清掉不属于它的字段。
func (l *Location) normalizeLocal() error {
	if strings.TrimSpace(l.Host) != "" || strings.TrimSpace(l.Username) != "" ||
		l.Port != 0 || strings.TrimSpace(l.RemotePath) != "" ||
		strings.TrimSpace(l.HostKeyFingerprint) != "" {
		return invalid("location", "本地后端不接受 host/port/username/remote_path/host_key_fingerprint")
	}
	rootID := strings.TrimSpace(l.RootID)
	if rootID == "" {
		return invalid("location.root_id", "不能为空")
	}
	if len(rootID) > MaxRootIDLength || !rootIDPattern.MatchString(rootID) {
		return invalid("location.root_id", "形状不合法（应以字母或数字开头，只含字母、数字、下划线、点和连字符）")
	}
	rel, err := normalizeRelativePath(l.RelativePath)
	if err != nil {
		return fmt.Errorf("%w: location.relative_path 不合法：%v", ErrInvalid, err)
	}
	l.RootID = rootID
	l.RelativePath = rel
	return nil
}

// normalizeSFTP 校验远端位置。
func (l *Location) normalizeSFTP() error {
	if strings.TrimSpace(l.RootID) != "" || strings.TrimSpace(l.RelativePath) != "" {
		return invalid("location", "SFTP 后端不接受 root_id/relative_path")
	}
	host := strings.TrimSpace(l.Host)
	if err := validateHost(host); err != nil {
		return fmt.Errorf("%w: location.host %v", ErrInvalid, err)
	}
	port := l.Port
	if port == 0 {
		port = DefaultSFTPPort
	}
	if port < 1 || port > 65535 {
		return invalid("location.port", "必须在 1—65535 之间（收到 %d）", l.Port)
	}
	user := strings.TrimSpace(l.Username)
	if user == "" {
		return invalid("location.username", "不能为空")
	}
	if !usernamePattern.MatchString(user) {
		return invalid("location.username", "形状不合法（应以小写字母或下划线开头，只含小写字母、数字、点、下划线、连字符）")
	}
	remote := strings.TrimSpace(l.RemotePath)
	if !strings.HasPrefix(remote, "/") {
		return invalid("location.remote_path", "必须是绝对路径（以 / 开头）")
	}
	normalized, err := normalizeAbsolutePath(remote)
	if err != nil {
		return fmt.Errorf("%w: location.remote_path 不合法：%v", ErrInvalid, err)
	}
	if _, err := ParseFingerprint(l.HostKeyFingerprint); err != nil {
		return fmt.Errorf("%w: location.host_key_fingerprint %v", ErrInvalid, err)
	}
	l.Host = host
	l.Port = port
	l.Username = user
	l.RemotePath = normalized
	l.HostKeyFingerprint = strings.TrimSpace(l.HostKeyFingerprint)
	return nil
}

// validateHost 校验主机名/IP。它拒绝的是**会被当成命令或选项的东西**：
// 空白、引号、斜杠、以 - 开头（会被 ssh 当选项）、shell 元字符。
func validateHost(host string) error {
	if host == "" {
		return errors.New("不能为空")
	}
	if len(host) > MaxHostLength {
		return fmt.Errorf("不能超过 %d 字节", MaxHostLength)
	}
	if strings.HasPrefix(host, "-") {
		return errors.New("不能以 - 开头（会被 ssh 当成选项）")
	}
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		return nil
	}
	for _, r := range host {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '-' || r == '_':
		default:
			return fmt.Errorf("含非法字符 %q（只允许主机名或 IP）", r)
		}
	}
	return nil
}

// normalizeRelativePath 校验受管理根下的相对路径。
//
// 规则与文件模块一致（不允许绝对路径、不允许 ..、不允许空段与尾部斜杠），
// 拒绝的理由也一样：这个字符串最终会拼进 restic 的 argv。
func normalizeRelativePath(p string) (string, error) {
	// 首尾空白按笔误处理（与 name/host/username 一样）：一个带空白的路径是合法目录名，
	// 但它几乎总是复制粘贴带进来的，而它一旦生效，仓库就会落在另一个目录里。
	p = strings.TrimSpace(p)
	if p == "" {
		return "", errors.New("不能为空")
	}
	if len(p) > MaxPathLength {
		return "", fmt.Errorf("不能超过 %d 字节", MaxPathLength)
	}
	if strings.HasPrefix(p, "/") {
		return "", errors.New("不能是绝对路径")
	}
	return cleanPathComponents(p)
}

// normalizeAbsolutePath 校验远端绝对路径。
func normalizeAbsolutePath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if len(p) > MaxPathLength {
		return "", fmt.Errorf("不能超过 %d 字节", MaxPathLength)
	}
	cleaned, err := cleanPathComponents(strings.TrimPrefix(p, "/"))
	if err != nil {
		return "", err
	}
	return "/" + cleaned, nil
}

// cleanPathComponents 做“形状”层面的清理：拒绝空段、`.`、`..`、控制字符与反斜杠。
//
// 它**不**做 filepath.Clean 那种静默归一（`a/../b` → `b`）：把用户写的路径悄悄改成
// 另一个路径，正是“预检绑定 A、执行落在 B”的来源。
func cleanPathComponents(p string) (string, error) {
	if strings.ContainsAny(p, "\\") {
		return "", errors.New("不能包含反斜杠")
	}
	if err := checkPrintable("path", p); err != nil {
		return "", errors.New("含控制字符或不可打印字符")
	}
	parts := strings.Split(p, "/")
	for _, part := range parts {
		switch part {
		case "":
			return "", errors.New("不能含空路径段（连续斜杠或尾部斜杠）")
		case ".", "..":
			return "", fmt.Errorf("不能含路径段 %q", part)
		}
		if len(part) > 255 {
			return "", errors.New("单段路径不能超过 255 字节")
		}
	}
	return strings.Join(parts, "/"), nil
}

// checkPrintable 拒绝控制字符。
func checkPrintable(field, s string) error {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return invalid(field, "含控制字符")
		}
	}
	return nil
}

// validSecretRef 与 agentstore.ValidSecretRef 是同一条形状（sec_ + 32 位小写十六进制）。
//
// 这里重写一遍是**故意**的：backup 包要在没有数据库的单元测试里校验定义，
// 而不是把 agentstore 拖进每个测试。形状本身是契约，不是实现细节。
var secretRefPattern = regexp.MustCompile(`^sec_[0-9a-f]{32}$`)

func validSecretRef(ref string) bool { return secretRefPattern.MatchString(ref) }

// DisplayName 是给人看的一句话：仓库名 + 位置。
func (d Definition) DisplayName() string {
	return d.Name + "（" + d.Location.String(d.Backend) + "）"
}

// String 返回位置的可读形式（不含任何秘密）。
func (l Location) String(backend Backend) string {
	switch backend {
	case BackendLocal:
		return "local:" + l.RootID + ":/" + l.RelativePath
	case BackendSFTP:
		port := l.Port
		if port == 0 {
			port = DefaultSFTPPort
		}
		return "sftp://" + l.Username + "@" + l.Host + ":" + strconv.Itoa(port) + l.RemotePath
	}
	return string(backend)
}

// IdentityFingerprint 是这份定义的身份指纹（sha256，十六进制）。
//
// 预检绑定它、执行前再核对一次：预检之后有人把仓库位置从 A 改到 B，
// 旧预检必须失效，而不是在 B 上执行 A 的计划。
func (d Definition) IdentityFingerprint() (string, error) {
	raw, err := json.Marshal(d)
	if err != nil {
		return "", fmt.Errorf("backup: 计算仓库身份指纹失败: %w", err)
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// resticRepository 返回交给 restic 的仓库地址。
//
// local 用绝对路径（由执行层解析根后填入 AbsolutePath 字段），sftp 用
// `sftp:user@host:/path`。端口不进这个 URL——restic 的 sftp 后端把端口交给
// `-o sftp.command` 里的 ssh 去处理，这也是唯一能同时钉住 known_hosts 的地方。
func (d Definition) resticRepository(absolutePath string) string {
	if d.Backend == BackendLocal {
		return absolutePath
	}
	// SFTP 的仓库地址里**不写端口**：restic 在 `sftp:user@host:path` 这种形式里
	// 没有端口字段（带端口的是 sftp:// URL 形式，而那会绕开我们给它的
	// `-o sftp.command`）。端口由 SFTPCommand 里的 `-p` 决定，两份事实只有一个来源。
	return "sftp:" + d.Location.Username + "@" + d.Location.Host + ":" + d.Location.RemotePath
}
