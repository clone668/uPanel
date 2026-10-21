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
)

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
