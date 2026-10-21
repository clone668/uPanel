// Package files 是受管理文件根的读取端（P06.1，CONTRACTS.md 第 11 节）。
//
// 它同时扮演两个角色，两边共用同一套 DTO：
//   - 代理侧（manager_linux.go）：在 root 句柄上做路径解析、目录列举、文本读取与下载流；
//   - Web 侧（fetch.go）：通过代理的 files.* 查询与 files.download 流取值，并翻译错误。
//
// 无论哪一侧，“根只能来自 root:root 0600 的配置文件”这条都不变：请求里的 root_id
// 只能命中已登记的根，请求里没有任何字段能让服务去打开一个任意路径。
package files

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// 产品限额。它们不是配置项：CONTRACTS.md 第 11 节把文本上限写成 1 MiB，
// 放宽它需要先改契约，而不是改一个 JSON 字段。
const (
	// MaxTextBytes 是 GET /files/text 的内容上限（1 MiB）。
	MaxTextBytes = 1 << 20
	// MaxPathBytes 是相对路径字节上限。Linux 的 PATH_MAX 是 4096（含结尾 NUL）。
	MaxPathBytes = 4096
	// ListPageSize 是 GET /files/list 的单页目录项数。
	//
	// 它同时也是游标语义的一部分：游标是“上一页最后一个 name”，下一页从它之后开始，
	// 所以页大小固定才能保证“同一个游标总是同一个起点”。
	ListPageSize = 200
	// MaxListScan 是单次列举最多看多少个名字。
	//
	// 它防的是“一个目录塞了上百万项”时把代理的内存吃掉。超限时响应里
	// truncated=true——这是一句实话，不是静默截断：界面必须显示“目录项过多，只列出前 N 项”。
	MaxListScan = 50_000

	// DefaultMaxUploadBytes 是单次上传的默认上限（2 GiB，PLAN.md 第 15 节）。
	// 运维可以在配置里改它，但任何写操作都不接受“这次放宽一点”的参数。
	DefaultMaxUploadBytes int64 = 2 << 30

	// UploadTTL 是未完成/未提交暂存上传的存活时间（CONTRACTS.md 第 11 节：24 小时）。
	UploadTTL = 24 * time.Hour

	// MaxEditBackups 是单个文件保留的编辑备份份数（PLAN.md 第 15 节：最多 10 份）。
	MaxEditBackups = 10

	// MaxEditBackupBytes 是单个文件备份的总量上限（PLAN.md 第 15 节：500 MiB）。
	// 它是按**文件**算的：一个巨大的日志文件反复编辑不该把整个卷吃满。
	MaxEditBackupBytes int64 = 500 << 20

	// MaxTreeEntries 是单次递归操作（删除、复制、统计）最多走过的目录项数。
	//
	// 递归删除与递归复制都是“一次请求动很多文件”，所以必须有上限：撞上它时操作
	// 直接拒绝（而不是删一半停下），让调用方自己缩小范围。
	MaxTreeEntries = 50_000

	// ReservedDirName 是每个根内的保留目录名：暂存与备份都放在这里。
	//
	// 它必须留在根内（不能放 /var/lib）：原子替换要求临时文件与目标在**同一个文件系统**
	// 上，跨挂载的 rename 会变成复制，而复制不是原子替换。这个目录不出现在列举里，
	// 也不接受任何指向它的路径。
	ReservedDirName = ".upanel"
)

// ManagedAccountResolver 把面板的受管理账号/组 id 解析成系统 uid/gid（P06.2，只用于 chown）。
//
// 用接口而不是直接查表：账号模块（P08）还没有落地，现在这一步必须能老实说
// “还没有受管理账号”，而不是接受任意 uid。
type ManagedAccountResolver interface {
	ResolveAccount(ctx context.Context, id int64) (uid uint32, name string, err error)
	ResolveGroup(ctx context.Context, id int64) (gid uint32, name string, err error)
}

// 代理登记表里的名字与流用途名。
//
// 定义在这里是因为两个进程都要用：代理按它登记查询与流用途，Web 侧按它去调用。
// 字符串写错一处就会变成“未登记的查询”，而这个名字是两边唯一的约定。
const (
	// QueryRoots 是列根的查询名。
	QueryRoots = "files.roots"
	// QueryList 是列目录的查询名。
	QueryList = "files.list"
	// QueryText 是读文本的查询名。
	QueryText = "files.text"
	// StreamPurposeDownload 是文件下载的流用途名。
	StreamPurposeDownload = "files.download"
)

// 目录项类型闭集。symlink 就是 symlink：面板不跟随它，也不报告它的目标。
const (
	TypeFile    = "file"
	TypeDir     = "dir"
	TypeSymlink = "symlink"
	// TypeOther 覆盖设备、FIFO、socket 等等：它们可以被列出，但绝不被读取。
	TypeOther = "other"
)

// 错误分类。Web 侧按这些错误映射 HTTP 状态码，因此它们必须是**可判定的**，
// 而不是靠匹配错误消息。
var (
	// ErrInvalidParams 表示参数形状不合法（缺 root_id、路径含 ".."、游标过长……）。
	ErrInvalidParams = errors.New("files: 参数不合法")
	// ErrUnknownRoot 表示 root_id 没有登记过。
	ErrUnknownRoot = errors.New("files: 未登记的文件根")
	// ErrRootUnavailable 表示根登记过，但它此刻不可用（目录不存在、被重新挂载、
	// 身份与登记时不一致）。
	ErrRootUnavailable = errors.New("files: 文件根不可用")
	// ErrPathRejected 表示路径越界或不被策略接受：绝对路径、".." 段、符号链接、
	// magic link、跨挂载点、含 NUL 的路径。
	ErrPathRejected = errors.New("files: 路径越界")
	// ErrNotFound 表示根之内没有这个路径。
	ErrNotFound = errors.New("files: 不存在")
	// ErrNotDirectory 表示列举的目标不是目录。
	ErrNotDirectory = errors.New("files: 不是目录")
	// ErrNotRegular 表示目标不是普通文件（目录、符号链接、设备、FIFO、socket）。
	ErrNotRegular = errors.New("files: 不是普通文件")
	// ErrHardLinked 表示目标普通文件有多个硬链接。CONTRACTS.md 第 11 节：
	// 初期拒绝 nlink>1 的文件，因为“这个 inode 在根外还有名字”无法在句柄层判定。
	ErrHardLinked = errors.New("files: 普通文件有多个硬链接")
	// ErrTooLarge 表示文件超过读取上限。
	ErrTooLarge = errors.New("files: 文件超过上限")
	// ErrNotText 表示文件内容不是合法 UTF-8。
	ErrNotText = errors.New("files: 不是 UTF-8 文本")
	// ErrVersionConflict 表示调用方给的 expected_version 与已打开句柄上的版本不一致。
	ErrVersionConflict = errors.New("files: 版本不一致")
	// ErrUnsupported 表示当前平台没有 openat2 这套解析机制。
	//
	// CONTRACTS.md 第 11 节要求“不支持该机制的平台拒绝真实文件操作”——
	// 不是退化成一串字符串比较。
	ErrUnsupported = errors.New("files: 当前平台不支持安全的文件路径解析")
	// ErrNotWritable 表示这个根在配置里是只读的（agent.files.roots[i].writable=false）。
	//
	// 它是**写操作**的门槛，读操作不看它：把根登记成只读是为了让面板不往里写，
	// 而不是让人看不见它。
	ErrNotWritable = errors.New("files: 文件根是只读的")
	// ErrAlreadyExists 表示目标已经存在，而这次操作不允许覆盖。
	ErrAlreadyExists = errors.New("files: 目标已存在")
	// ErrIsDirectory 表示目标是个目录，而这个操作要的是一个文件（或反过来）。
	ErrIsDirectory = errors.New("files: 目标是目录")
	// ErrUploadNotFound 表示没有这个上传（不存在、已过期或已提交）。
	ErrUploadNotFound = errors.New("files: 上传不存在")
	// ErrUploadState 表示上传当前状态不允许这个操作（例如内容已经在传、已接收完整）。
	ErrUploadState = errors.New("files: 上传状态不允许该操作")
	// ErrNoSpace 表示根所在文件系统空间不足（容量预检不通过，或写入时 ENOSPC）。
	ErrNoSpace = errors.New("files: 空间不足")
	// ErrTreeTooLarge 表示递归操作的目录项数超过 MaxTreeEntries。
	ErrTreeTooLarge = errors.New("files: 目录树超过单次操作上限")
	// ErrTreeUnsupported 表示目录树里含有面板不处理的条目（符号链接、特殊文件、
	// 多硬链接文件）。递归复制拒绝整个操作，而不是“跳过几个”。
	ErrTreeUnsupported = errors.New("files: 目录树含有不支持的条目")
)

// Owner 是目录项的属主。
//
// 只给数字 uid/gid，不查账号名：名字解析会走 NSS（可能连着 LDAP/SSSD），
// 一次目录列举不该为了显示名字而把请求挂在一个外部服务上。
type Owner struct {
	UID uint32 `json:"uid"`
	GID uint32 `json:"gid"`
}

// Entry 是一个目录项（CONTRACTS.md 第 11 节：name、type、size_bytes、mode、
// owner、mtime、version；不跟随 symlink）。
type Entry struct {
	Name string `json:"name"`
	// Type 是 file/dir/symlink/other（闭集）。
	Type string `json:"type"`
	// SizeBytes 是十进制字符串：目录项大小会出现在前端表格里，而 JS 的 number
	// 在 2^53 之后就丢精度，因此与存储模块保持一致，用字符串传输。
	SizeBytes uint64 `json:"size_bytes,string"`
	// Mode 是权限位的八进制表示（"0644"），不是十进制数字：0400 与 400 差得太多。
	Mode  string    `json:"mode"`
	Owner Owner     `json:"owner"`
	MTime time.Time `json:"mtime"`
	// Version 是 lstat 得到的版本（设备/inode/大小/纳秒 mtime）。
	// symlink 的版本描述链接本身，不描述它指向的东西。
	Version string `json:"version"`
}

// ListPage 是 GET /files/list 的响应体。
type ListPage struct {
	RootID string `json:"root_id"`
	// Path 是规范化后的相对路径；根自身是空串。
	Path    string  `json:"path"`
	Entries []Entry `json:"entries"`
	// NextCursor 为 null 表示没有下一页（CONTRACTS.md 第 1 节：列表统一形态，
	// 与 /jobs、/logs、/users 一致）；否则把它的值原样作为下一次的 cursor 参数。
	//
	// 用 null 而不是空串：空串会让“尾页”和“游标恰好是空”在 JSON 里长得一样，
	// 而前端判断“还有没有更多”只该有一个依据。
	NextCursor *string `json:"next_cursor"`
	// Truncated 表示这次列举撞上了 MaxListScan 上限，列出的不是全部目录项。
	Truncated bool `json:"truncated"`
}

// TextFile 是 GET /files/text 的响应体。
type TextFile struct {
	RootID    string `json:"root_id"`
	Path      string `json:"path"`
	Version   string `json:"version"`
	SizeBytes uint64 `json:"size_bytes,string"`
	Encoding  string `json:"encoding"`
	Content   string `json:"content"`
}

// Capacity 是文件根所在文件系统的容量。字段与存储模块的容量一致（同一个 statfs，
// 同一套字符串化规则）。
type Capacity struct {
	TotalBytes     uint64    `json:"total_bytes,string"`
	AvailableBytes uint64    `json:"available_bytes,string"`
	UsedPercent    float64   `json:"used_percent"`
	CollectedAt    time.Time `json:"collected_at"`
}

// RootInfo 是 GET /files/roots 的一项。
type RootInfo struct {
	RootID   string `json:"root_id"`
	Name     string `json:"name"`
	Path     string `json:"path"`
	Writable bool   `json:"writable"`
	// Available 为 false 时其余读取字段可能为空，UnavailableReason 说明原因。
	// 界面必须把不可用的根显示成不可用，而不是显示成空目录。
	Available         bool   `json:"available"`
	UnavailableReason string `json:"unavailable_reason,omitempty"`
	// MountPoint/FSType 是根实际所在挂载点与文件系统类型（来自 /proc/self/mountinfo）。
	MountPoint string `json:"mount_point,omitempty"`
	FSType     string `json:"fs_type,omitempty"`
	// Capacity 为 null 表示这次没读出来（原因见 CapacityError）。
	// null 不是“0 字节可用”，界面不能把它当成磁盘满了。
	Capacity      *Capacity `json:"capacity"`
	CapacityError string    `json:"capacity_error,omitempty"`
}

// RootsResult 是 files.roots 查询的响应体。
type RootsResult struct {
	Roots []RootInfo `json:"roots"`
}

// DownloadHeader 是 files.download 流的首行（JSON + '\n'），其后是文件原始字节。
//
// 为什么不是把内容也塞进 JSON：base64 会给每个下载都加上三分之一的体积，
// 而下载的场景恰恰是“文件可能很大”。首行之后的字节流不属于任何框架，
// Web 侧按 Content-Length 原样转发。
type DownloadHeader struct {
	Name      string    `json:"name"`
	SizeBytes uint64    `json:"size_bytes,string"`
	Version   string    `json:"version"`
	MTime     time.Time `json:"mtime"`
}

// RootsParams 是 files.roots 的参数（空对象）。
type RootsParams struct{}

// ListParams 是 files.list 的参数。
type ListParams struct {
	RootID string `json:"root_id"`
	// Path 是根内相对路径；空串表示根自身。
	Path   string `json:"path"`
	Cursor string `json:"cursor"`
}

// TextParams 是 files.text 的参数。
type TextParams struct {
	RootID string `json:"root_id"`
	Path   string `json:"path"`
	// ExpectedVersion 非空时，已打开句柄上的版本必须与它一致，否则 ErrVersionConflict。
	// P06.1 只读，但它让“版本是在句柄上核验的”这句话可以被验证，而不是只能被相信。
	ExpectedVersion string `json:"expected_version"`
}

// DownloadParams 是 files.download 流的参数。
type DownloadParams struct {
	RootID          string `json:"root_id"`
	Path            string `json:"path"`
	ExpectedVersion string `json:"expected_version"`
}

// Validate 校验列举参数。
func (p ListParams) Validate() error {
	if err := validateRootID(p.RootID); err != nil {
		return err
	}
	if err := ValidateRelPath(p.Path); err != nil {
		return err
	}
	if len(p.Cursor) > MaxPathBytes {
		return fmt.Errorf("%w: cursor 超过 %d 字节", ErrInvalidParams, MaxPathBytes)
	}
	if p.Cursor != "" {
		if err := validateName(p.Cursor, "cursor"); err != nil {
			return err
		}
	}
	return nil
}

// Validate 校验文本读取参数：路径必须指向根内的**某个文件**，不能是根自身。
func (p TextParams) Validate() error {
	if err := validateRootID(p.RootID); err != nil {
		return err
	}
	if err := validateFilePath(p.Path); err != nil {
		return err
	}
	return validateExpectedVersion(p.ExpectedVersion)
}

// Validate 校验下载参数。
func (p DownloadParams) Validate() error {
	if err := validateRootID(p.RootID); err != nil {
		return err
	}
	if err := validateFilePath(p.Path); err != nil {
		return err
	}
	return validateExpectedVersion(p.ExpectedVersion)
}

func validateRootID(id string) error {
	if id == "" {
		return fmt.Errorf("%w: root_id 不能为空", ErrInvalidParams)
	}
	if len(id) > 32 {
		return fmt.Errorf("%w: root_id 过长", ErrInvalidParams)
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return fmt.Errorf("%w: root_id 含有非法字符 %q", ErrInvalidParams, r)
		}
	}
	return nil
}

func validateExpectedVersion(v string) error {
	if v == "" {
		return nil
	}
	if len(v) > 128 {
		return fmt.Errorf("%w: expected_version 过长", ErrInvalidParams)
	}
	for _, r := range v {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'z', r == '-':
		default:
			return fmt.Errorf("%w: expected_version 含有非法字符 %q", ErrInvalidParams, r)
		}
	}
	return nil
}

// validateFilePath 用于“必须是一个文件”的路径：空串（根自身）不合法。
func validateFilePath(p string) error {
	if strings.TrimSpace(p) == "" {
		return fmt.Errorf("%w: path 不能为空", ErrInvalidParams)
	}
	return ValidateRelPath(p)
}

// ValidateRelPath 校验根内相对路径。
//
// 这里做的是**形状**校验：绝对路径、".." 段、空段、NUL、超长、非法 UTF-8 一律拒绝。
// 它**不**是边界校验——边界由内核在 root 句柄上按 openat2 的 RESOLVE_* 完成。
// 两层都要有：形状校验让错误变成清晰的 400（而不是让内核报一个 ELOOP），
// 内核解析才是真正的保证。任何一层单独使用都不够。
func ValidateRelPath(p string) error {
	if p == "" {
		// 根自身。列举允许，读取不允许（由调用方用 validateFilePath 区分）。
		return nil
	}
	if len(p) > MaxPathBytes {
		return fmt.Errorf("%w: path 超过 %d 字节", ErrInvalidParams, MaxPathBytes)
	}
	if !utf8.ValidString(p) {
		// JSON 会把非法 UTF-8 变成 U+FFFD 而**不报错**，那样我们就会去解析一个
		// 与请求里写的完全不同的名字。宁可拒绝。
		return fmt.Errorf("%w: path 不是合法 UTF-8", ErrInvalidParams)
	}
	if strings.ContainsRune(p, 0) {
		return fmt.Errorf("%w: path 不能包含 NUL", ErrInvalidParams)
	}
	if strings.HasPrefix(p, "/") {
		return fmt.Errorf("%w: path 必须是根内相对路径，不接受绝对路径 %q", ErrPathRejected, p)
	}
	for _, part := range strings.Split(p, "/") {
		switch part {
		case "":
			return fmt.Errorf("%w: path 含空段（重复的 '/' 或结尾的 '/'）", ErrInvalidParams)
		case ".":
			return fmt.Errorf("%w: path 含 '.' 段", ErrInvalidParams)
		case "..":
			return fmt.Errorf("%w: path 含 '..' 段", ErrPathRejected)
		case ReservedDirName:
			// 保留目录里放的是暂存上传与编辑备份：里面有正在写的半成品和别人改之前的
			// 旧版本。它们不是“服务器上的文件”，所以既不出现在列举里，也不接受路径。
			return fmt.Errorf("%w: %s 是面板的保留目录，不能通过文件接口访问", ErrPathRejected, ReservedDirName)
		}
		if err := validateName(part, "path"); err != nil {
			return err
		}
	}
	return nil
}

// validateName 校验单个名字段：不能是空串、不能含 '/'，长度受 PATH_MAX 约束。
func validateName(name, field string) error {
	if name == "" {
		return fmt.Errorf("%w: %s 含空名字段", ErrInvalidParams, field)
	}
	if strings.ContainsRune(name, '/') {
		return fmt.Errorf("%w: %s 的名字段不能含 '/'", ErrInvalidParams, field)
	}
	if len(name) > 255 {
		// 单个文件名的上限（NAME_MAX）。内核也会拒绝，但提前说清楚更好查。
		return fmt.Errorf("%w: %s 的单个名字段超过 255 字节", ErrInvalidParams, field)
	}
	return nil
}

// JoinPath 把根内相对路径与目录项名拼成新的相对路径（供界面与测试使用）。
//
// 它只做拼接，不做校验：拼接结果仍会被 ValidateRelPath 与内核解析各自检查一遍。
func JoinPath(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

// ParentPath 返回相对路径的父目录（根自身的父目录是空串）。
func ParentPath(p string) string {
	i := strings.LastIndexByte(p, '/')
	if i <= 0 {
		return ""
	}
	return p[:i]
}

// BasePath 返回相对路径的最后一段（空串表示根自身）。
func BasePath(p string) string {
	if p == "" {
		return ""
	}
	return p[strings.LastIndexByte(p, '/')+1:]
}

// ─────────────────────────── P06.2 写操作与上传 ───────────────────────────

// 上传状态闭集（GET /files/uploads/:id 的 state）。
//
// 三个状态描述的是**暂存**，不是目标文件：目标文件只有在 file.commit_upload
// 成功之后才会改变，而那时上传记录已经不存在了。
const (
	// UploadStateOpen 表示暂存已创建，内容还没收完。
	UploadStateOpen = "open"
	// UploadStateReceived 表示内容已收完且大小与声明的 size_bytes 一致。
	UploadStateReceived = "received"
	// UploadStateFailed 表示传输失败（大小不符、磁盘满、超限）。暂存数据已删除，
	// 记录留到过期时间，让调用方能看到“为什么失败”，而不是一个忽然消失的 id。
	UploadStateFailed = "failed"
)

// CreateUploadParams 是 POST /files/uploads 的参数（CONTRACTS.md 第 11 节）。
//
// 没有 upload_id（由代理生成）、没有 staging 路径（不允许调用方指定暂存位置）、
// 没有 overwrite（覆盖是提交时的决定，不是创建暂存时的决定）。
type CreateUploadParams struct {
	RootID string `json:"root_id"`
	Path   string `json:"path"`
	// SizeBytes 是声明的大小。声明与实收必须一致：声明值用来做容量预留与上限检查，
	// 实收值用来判断传输是否完整——两者不一致就不提交。
	SizeBytes       uint64 `json:"size_bytes,string"`
	ExpectedVersion string `json:"expected_version"`
}

// Validate 校验创建上传的参数。
func (p CreateUploadParams) Validate() error {
	if err := validateRootID(p.RootID); err != nil {
		return err
	}
	if err := validateFilePath(p.Path); err != nil {
		return err
	}
	if err := validateExpectedVersion(p.ExpectedVersion); err != nil {
		return err
	}
	if p.SizeBytes == 0 {
		return fmt.Errorf("%w: size_bytes 必须大于 0", ErrInvalidParams)
	}
	return nil
}

// UploadInfo 是上传暂存的视图（POST /files/uploads 与 GET /files/uploads/:id 的 data）。
type UploadInfo struct {
	UploadID        string    `json:"upload_id"`
	RootID          string    `json:"root_id"`
	Path            string    `json:"path"`
	SizeBytes       uint64    `json:"size_bytes,string"`
	ReceivedBytes   uint64    `json:"received_bytes,string"`
	State           string    `json:"state"`
	ExpectedVersion string    `json:"expected_version,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	ExpiresAt       time.Time `json:"expires_at"`
	// Error 只在 state=failed 时出现，说明失败原因（大小不符、空间不足、超限）。
	Error string `json:"error,omitempty"`
}

// PathStat 是一次规划探针的读数：目标此刻是什么。
//
// Plan 用它算阻塞项与状态版本，Execution 之后用同样形状的读数验证结果——
// 两处读同一份结构，才不会出现“预检说 A、执行后验证 B”的错位。
type PathStat struct {
	RootID string `json:"root_id"`
	Path   string `json:"path"`
	Exists bool   `json:"exists"`
	Type   string `json:"type,omitempty"`
	// SizeBytes 对目录是 st_size（目录项本身的大小），条目总大小见 TreeBytes。
	SizeBytes uint64    `json:"size_bytes,string"`
	Mode      string    `json:"mode,omitempty"`
	Owner     Owner     `json:"owner"`
	MTime     time.Time `json:"mtime,omitempty"`
	Version   string    `json:"version,omitempty"`
	// InTree 为 true 表示这次探针还递归统计了目录树（delete / copy 的预检需要
	// “删掉多少个文件、多少字节”这句话）。统计到上限时 TreeTruncated=true，
	// 且这些数字是“至少这么多”，不是全部。
	InTree        bool   `json:"in_tree"`
	TreeEntries   int    `json:"tree_entries"`
	TreeBytes     uint64 `json:"tree_bytes,string"`
	TreeFiles     int    `json:"tree_files"`
	TreeDirs      int    `json:"tree_dirs"`
	TreeSymlinks  int    `json:"tree_symlinks"`
	TreeOthers    int    `json:"tree_others"`
	TreeTruncated bool   `json:"tree_truncated"`
}

// WriteResult 是一次写操作的读回结果。
//
// 每个字段都是执行之后**重新打开**读出来的，不是“我们打算写什么”：
// outcome=verified 的意思是“读回来的东西与预期一致”，而不是“调用没报错”。
type WriteResult struct {
	RootID string `json:"root_id"`
	Path   string `json:"path"`
	// Version 是操作完成后读回的版本；空表示读回时不存在（删除）。
	Version   string `json:"version,omitempty"`
	Type      string `json:"type,omitempty"`
	SizeBytes uint64 `json:"size_bytes,string"`
	Mode      string `json:"mode,omitempty"`
	// BackupPath 是这次修改留下的备份（相对路径，位于保留目录内）。
	BackupPath string `json:"backup_path,omitempty"`
	// BackupCount/BackupBytes 是备份清理之后该文件仍然保留的份数与总字节数。
	BackupCount int    `json:"backup_count,omitempty"`
	BackupBytes uint64 `json:"backup_bytes,string"`
	// Entries/Bytes 是删除操作实际删掉的条目数与字节数（递归删除时是整棵树）。
	Entries int    `json:"entries,omitempty"`
	Bytes   uint64 `json:"bytes,string"`
	// Source 是跨根复制/移动时源的位置；同根操作不填。
	Source string `json:"source,omitempty"`
}

// MkdirParams 是 file.mkdir 的参数（CONTRACTS.md 第 5 节）。
type MkdirParams struct {
	RootID string `json:"root_id"`
	Path   string `json:"path"`
}

// Validate 校验建目录参数。
func (p MkdirParams) Validate() error {
	if err := validateRootID(p.RootID); err != nil {
		return err
	}
	return validateFilePath(p.Path)
}

// CommitUploadParams 是 file.commit_upload 的参数。
type CommitUploadParams struct {
	UploadID string `json:"upload_id"`
	// Overwrite 表示允许替换已存在的目标。它不绕过版本检查：要覆盖就必须给出
	// expected_version，否则“覆盖”等于“谁先写完算谁”。
	Overwrite       bool   `json:"overwrite"`
	ExpectedVersion string `json:"expected_version"`
}

// Validate 校验提交上传参数。
func (p CommitUploadParams) Validate() error {
	if !ValidUploadID(p.UploadID) {
		return fmt.Errorf("%w: upload_id 必须是 up_ 加 32 位小写十六进制", ErrInvalidParams)
	}
	return validateExpectedVersion(p.ExpectedVersion)
}

// WriteTextParams 是 file.write_text 的参数。
type WriteTextParams struct {
	RootID          string `json:"root_id"`
	Path            string `json:"path"`
	ExpectedVersion string `json:"expected_version"`
	Content         string `json:"content"`
}

// Validate 校验文本写入参数。
func (p WriteTextParams) Validate() error {
	if err := validateRootID(p.RootID); err != nil {
		return err
	}
	if err := validateFilePath(p.Path); err != nil {
		return err
	}
	if err := validateExpectedVersion(p.ExpectedVersion); err != nil {
		return err
	}
	if len(p.Content) > MaxTextBytes {
		return fmt.Errorf("%w: 内容有 %d 字节，文本编辑上限 %d 字节", ErrTooLarge, len(p.Content), MaxTextBytes)
	}
	if !utf8.ValidString(p.Content) {
		return fmt.Errorf("%w: 内容不是合法 UTF-8", ErrNotText)
	}
	if strings.ContainsRune(p.Content, 0) {
		return fmt.Errorf("%w: 内容不能包含 NUL 字节", ErrNotText)
	}
	return nil
}

// CopyParams 是 file.copy 与 file.move 的参数。
//
// 源与目标各有一套 root_id/path：跨根复制/移动是契约里明确写过的场景
// （跨根 move = 复制到目标暂存 → 校验 → 原子落位 → 删除源）。
type CopyParams struct {
	SourceRootID string `json:"source_root_id"`
	SourcePath   string `json:"source_path"`
	TargetRootID string `json:"target_root_id"`
	TargetPath   string `json:"target_path"`
	// ExpectedVersion 描述的是**目标**：目标已存在时它必须匹配，否则拒绝覆盖。
	// 目标不存在时它必须为空——对一个不存在的东西给版本，说明调用方看的是别的对象。
	ExpectedVersion string `json:"expected_version"`
}

// Validate 校验复制/移动参数。
func (p CopyParams) Validate() error {
	if err := validateRootID(p.SourceRootID); err != nil {
		return err
	}
	if err := validateFilePath(p.SourcePath); err != nil {
		return err
	}
	if err := validateRootID(p.TargetRootID); err != nil {
		return err
	}
	if err := validateFilePath(p.TargetPath); err != nil {
		return err
	}
	if p.SourceRootID == p.TargetRootID && p.SourcePath == p.TargetPath {
		return fmt.Errorf("%w: 源与目标是同一个路径", ErrInvalidParams)
	}
	if p.TargetPath == p.SourcePath && p.TargetRootID == p.SourceRootID {
		return fmt.Errorf("%w: 源与目标相同", ErrInvalidParams)
	}
	return validateExpectedVersion(p.ExpectedVersion)
}

// DeleteParams 是 file.delete 的参数。
type DeleteParams struct {
	RootID          string `json:"root_id"`
	Path            string `json:"path"`
	ExpectedVersion string `json:"expected_version"`
	// Recursive 只对目录有意义：非递归时目录必须为空。删整棵树需要确认短语。
	Recursive bool `json:"recursive"`
}

// Validate 校验删除参数。
func (p DeleteParams) Validate() error {
	if err := validateRootID(p.RootID); err != nil {
		return err
	}
	if err := validateFilePath(p.Path); err != nil {
		return err
	}
	return validateExpectedVersion(p.ExpectedVersion)
}

// ChmodParams 是 file.chmod 的参数。
type ChmodParams struct {
	RootID          string `json:"root_id"`
	Path            string `json:"path"`
	ExpectedVersion string `json:"expected_version"`
	// Mode 是四位八进制字符串（"0644"）。只允许普通权限位：见 ParseMode。
	Mode string `json:"mode"`
}

// Validate 校验权限修改参数。
func (p ChmodParams) Validate() error {
	if err := validateRootID(p.RootID); err != nil {
		return err
	}
	if err := validateFilePath(p.Path); err != nil {
		return err
	}
	if err := validateExpectedVersion(p.ExpectedVersion); err != nil {
		return err
	}
	_, err := ParseMode(p.Mode)
	return err
}

// ChownParams 是 file.chown 的参数。
//
// 它引用的是**受管理账号/组**的 id，不是 uid/gid：面板不接受任意数字所有者，
// 因为“把文件给 uid 0”这种事没有任何一条产品需求需要它。
type ChownParams struct {
	RootID           string `json:"root_id"`
	Path             string `json:"path"`
	ExpectedVersion  string `json:"expected_version"`
	ManagedAccountID int64  `json:"managed_account_id"`
	ManagedGroupID   int64  `json:"managed_group_id"`
}

// Validate 校验所有者修改参数。两者都为 0 时是“什么都不改”，直接拒绝。
func (p ChownParams) Validate() error {
	if err := validateRootID(p.RootID); err != nil {
		return err
	}
	if err := validateFilePath(p.Path); err != nil {
		return err
	}
	if err := validateExpectedVersion(p.ExpectedVersion); err != nil {
		return err
	}
	if p.ManagedAccountID < 0 || p.ManagedGroupID < 0 {
		return fmt.Errorf("%w: managed_account_id / managed_group_id 不能为负", ErrInvalidParams)
	}
	if p.ManagedAccountID == 0 && p.ManagedGroupID == 0 {
		return fmt.Errorf("%w: managed_account_id 与 managed_group_id 至少要给一个", ErrInvalidParams)
	}
	return nil
}

// ParseMode 解析四位八进制权限字符串。
//
// 只接受普通权限位（0o777）：setuid/setgid/sticky 一律拒绝。理由不是“界面用不到”，
// 而是它们改变的是**执行语义**（setuid 让进程以文件所有者身份运行），
// 一个文件管理面板没有资格顺手把这种位写上去。
func ParseMode(mode string) (uint32, error) {
	if len(mode) != 4 || mode[0] != '0' {
		return 0, fmt.Errorf("%w: mode 必须是四位八进制字符串（如 \"0644\"）", ErrInvalidParams)
	}
	var v uint32
	for i := 1; i < 4; i++ {
		c := mode[i]
		if c < '0' || c > '7' {
			return 0, fmt.Errorf("%w: mode 含有非八进制字符 %q", ErrInvalidParams, c)
		}
		v = v<<3 | uint32(c-'0')
	}
	// 第一位已经被固定成 '0'，所以 v 不可能超过 0o777；这里再断言一次，
	// 免得以后有人把长度校验改成 5 位却忘了这个函数。
	if v > 0o777 {
		return 0, fmt.Errorf("%w: mode 只能包含普通权限位（禁止 setuid/setgid/sticky）", ErrInvalidParams)
	}
	return v, nil
}

// FormatMode 把权限位格式化成四位八进制（与 Entry.Mode 一致）。
func FormatMode(mode uint32) string {
	const digits = "01234567"
	return string([]byte{
		'0',
		digits[(mode>>6)&7],
		digits[(mode>>3)&7],
		digits[mode&7],
	})
}

// ValidUploadID 判断上传 id 的形状（up_ + 32 位小写十六进制）。
//
// 形状检查放在入口：id 会被拼进暂存文件名，一个含 '/' 或 '..' 的 id
// 就是一次路径注入，不能等到打开文件时才发现。
func ValidUploadID(id string) bool {
	const prefix = "up_"
	if len(id) != len(prefix)+32 || !strings.HasPrefix(id, prefix) {
		return false
	}
	for i := len(prefix); i < len(id); i++ {
		c := id[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
