//go:build linux

package files

// P06.2：文件根内的写操作（CONTRACTS.md 第 5 节 action 表、第 11 节路径边界）。
//
// 所有写操作共用同一条路径，顺序不能变：
//
//	预检（Plan）读目标此刻的形状 → 执行（Execute）重新读一遍 → 写临时文件 → 备份旧版本
//	→ 原子落位（renameat）→ **重新打开读回** → 返回 verified 或 unknown
//
// “重新打开读回”不是形式主义：renameat 成功只说明内核接受了这次改名，不说明落位的
// 是你以为的那些字节。读回比对 size 与 sha256（小文件）或 size + inode（大文件）之后，
// 才有资格说 verified。读不回来就报 unknown 并给恢复提示，不猜成功。
//
// 边界仍然只有一套：根句柄 + openat2 的 RESOLVE_* + 句柄上的 fstat。这里没有一处
// 用字符串比较判断“在不在根内”。

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"time"

	"golang.org/x/sys/unix"
)

// maxTreeDepth 是递归操作的最大深度（防止病态嵌套把栈吃光）。
const maxTreeDepth = 64

// AccountsAvailable 报告受管理账号解析器是否已接入。
//
// 预检用它决定 file.chown 是“允许”还是“阻塞并说明原因”。返回 false 时
// chown 不是“先不管”，而是明确挡在预检那一步——界面因此不会给出一个按下去必然失败的按钮。
func (m *Manager) AccountsAvailable() bool { return m.accounts != nil }

// openParent 打开一个相对路径的父目录，返回父目录 fd 与最后一段名字。
func (m *Manager) openParent(handle *rootHandle, rel string) (int, string, error) {
	parent := ParentPath(rel)
	name := BasePath(rel)
	if name == "" {
		return -1, "", fmt.Errorf("%w: 不能对根自身执行这个操作", ErrInvalidParams)
	}
	fd, err := m.openInRoot(handle.fd, parent, unix.O_PATH|unix.O_DIRECTORY)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return -1, "", fmt.Errorf("%w: 父目录 %q 不存在", ErrNotFound, parent)
		}
		return -1, "", err
	}
	return fd, name, nil
}

// Stat 读取根内一个路径此刻的形状（不跟随符号链接，不做递归统计）。
func (m *Manager) Stat(ctx context.Context, rootID, rel string) (PathStat, error) {
	if err := ctx.Err(); err != nil {
		return PathStat{}, err
	}
	if err := validateRootID(rootID); err != nil {
		return PathStat{}, err
	}
	if err := ValidateRelPath(rel); err != nil {
		return PathStat{}, err
	}
	handle, err := m.openRoot(rootID)
	if err != nil {
		return PathStat{}, err
	}
	defer func() { _ = handle.Close() }()

	if rel == "" {
		var st unix.Stat_t
		if err := unix.Fstat(handle.fd, &st); err != nil {
			return PathStat{}, fmt.Errorf("files: 无法读取根 %q 的 stat: %w", rootID, err)
		}
		return PathStat{
			RootID: rootID, Path: "", Exists: true, Type: TypeDir,
			SizeBytes: uint64(st.Size), Mode: modeString(st.Mode),
			Owner:   Owner{UID: st.Uid, GID: st.Gid},
			MTime:   time.Unix(st.Mtim.Sec, st.Mtim.Nsec).UTC(),
			Version: statVersion(&st),
		}, nil
	}
	parentFD, name, err := m.openParent(handle, rel)
	if err != nil {
		return PathStat{}, err
	}
	defer func() { _ = unix.Close(parentFD) }()
	return m.statAt(handle, parentFD, rel, name)
}

// StatTree 在 Stat 的基础上递归统计目录树（delete / copy 的预检要“删掉多少个、多大”）。
//
// 统计撞上 MaxTreeEntries 时 TreeTruncated=true，数字是“至少这么多”。预检会据此
// 让操作停下来，而不是按一个不完整的数字去删。
//
// 打开目录用 openRead|O_DIRECTORY，**不能**用 O_PATH：O_PATH 的 fd 只表示“树上的
// 位置”，getdents64(2) 对它返回 EBADF，walkTree 会在第一次 readNames 就失败。
// 那会让“删除任何目录”“复制任何目录”在预检阶段就报错——P06.1 的 Q136 是同一件事
// 在列举上犯过一次，这里与 walkTree 的子树打开一起改齐。
func (m *Manager) StatTree(ctx context.Context, rootID, rel string) (PathStat, error) {
	stat, err := m.Stat(ctx, rootID, rel)
	if err != nil || !stat.Exists || stat.Type != TypeDir {
		return stat, err
	}
	handle, err := m.openRoot(rootID)
	if err != nil {
		return PathStat{}, err
	}
	defer func() { _ = handle.Close() }()

	dirFD, err := m.openInRoot(handle.fd, rel, openRead|unix.O_DIRECTORY)
	if err != nil {
		return PathStat{}, err
	}
	defer func() { _ = unix.Close(dirFD) }()

	stat.InTree = true
	if err := m.walkTree(dirFD, &stat, 0); err != nil {
		return PathStat{}, err
	}
	return stat, nil
}

// walkTree 累加目录树统计。
func (m *Manager) walkTree(dirFD int, stat *PathStat, depth int) error {
	if depth > maxTreeDepth {
		stat.TreeTruncated = true
		return nil
	}
	dup, err := unix.Dup(dirFD)
	if err != nil {
		return fmt.Errorf("files: 无法复制目录句柄: %w", err)
	}
	dir := os.NewFile(uintptr(dup), "tree")
	defer func() { _ = dir.Close() }()
	names, _, err := readNames(dir)
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, name := range names {
		if stat.TreeEntries >= MaxTreeEntries {
			stat.TreeTruncated = true
			return nil
		}
		var st unix.Stat_t
		if err := unix.Fstatat(dirFD, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			if errors.Is(err, unix.ENOENT) {
				continue
			}
			return fmt.Errorf("files: 无法读取 %s 的信息: %w", name, err)
		}
		stat.TreeEntries++
		switch st.Mode & unix.S_IFMT {
		case unix.S_IFDIR:
			stat.TreeDirs++
			// 子目录也要用能读的模式打开：下一层 walkTree 仍然要 readNames。
			childFD, err := unix.Openat(dirFD, name,
				unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if err != nil {
				if errors.Is(err, unix.ENOENT) {
					continue
				}
				return fmt.Errorf("files: 无法打开目录 %s: %w", name, err)
			}
			err = m.walkTree(childFD, stat, depth+1)
			_ = unix.Close(childFD)
			if err != nil {
				return err
			}
		case unix.S_IFREG:
			stat.TreeFiles++
			stat.TreeBytes += uint64(st.Size)
		case unix.S_IFLNK:
			stat.TreeSymlinks++
		default:
			stat.TreeOthers++
		}
	}
	return nil
}

// stagingTemp 在保留目录里造一个临时文件，返回 fd 与它的名字。
//
// 临时文件不进用户目录：既不会在界面上闪出一个 `.foo.upanel-tmp-xxxx`，也不会让
// 一次中断的写操作在目标目录里留下垃圾。暂存目录与目标在同一个文件系统上，
// 所以从它 rename 到目标是原子的。
func (m *Manager) stagingTemp(handle *rootHandle, prefix string) (int, string, error) {
	dirFD, err := m.reservedSubdir(handle, stagingSubdir)
	if err != nil {
		return -1, "", err
	}
	defer func() { _ = unix.Close(dirFD) }()
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return -1, "", fmt.Errorf("files: 生成临时文件名失败: %w", err)
	}
	name := prefix + "-" + hex.EncodeToString(buf[:])
	fd, err := unix.Openat(dirFD, name,
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return -1, "", fmt.Errorf("files: 无法创建临时文件: %w", err)
	}
	return fd, ReservedDirName + "/" + stagingSubdir + "/" + name, nil
}

// removeStagingTemp 删除一个临时文件；名字是 stagingTemp 返回的相对路径。
func (m *Manager) removeStagingTemp(handle *rootHandle, rel string) {
	if rel == "" {
		return
	}
	parent, name, err := m.openParent(handle, rel)
	if err != nil {
		return
	}
	defer func() { _ = unix.Close(parent) }()
	_ = unix.Unlinkat(parent, name, 0)
}

// writeTemp 把 bytes 写进 handle 的暂存目录，返回落位用的 (dirFD, name) 之外的信息。
func (m *Manager) writeTemp(handle *rootHandle, prefix string, content []byte) (stagingRel string, sum string, err error) {
	fd, rel, err := m.stagingTemp(handle, prefix)
	if err != nil {
		return "", "", err
	}
	file := os.NewFile(uintptr(fd), rel)
	if _, err := file.Write(content); err != nil {
		_ = file.Close()
		m.removeStagingTemp(handle, rel)
		return "", "", fmt.Errorf("files: 写入临时文件失败: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		m.removeStagingTemp(handle, rel)
		return "", "", fmt.Errorf("files: 同步临时文件失败: %w", err)
	}
	if err := file.Close(); err != nil {
		m.removeStagingTemp(handle, rel)
		return "", "", fmt.Errorf("files: 关闭临时文件失败: %w", err)
	}
	return rel, sha256Hex(content), nil
}

// sha256Hex 返回内容的 sha256 十六进制值。
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// renameWithinRoot 把 handle 内的 oldRel 原子落位成 newRel（替换已存在的目标）。
//
// 两个相对路径都在同一个根、同一个挂载上——这是“原子替换”的前提，
// 也是把暂存放在根内而不是 /var/lib 的唯一原因。
func (m *Manager) renameWithinRoot(handle *rootHandle, oldRel, newRel string) error {
	oldParent, oldName, err := m.openParent(handle, oldRel)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(oldParent) }()
	newParent, newName, err := m.openParent(handle, newRel)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(newParent) }()
	return renameAt(oldParent, oldName, newParent, newName)
}

// renameWithinRootNoReplace 同上，但目标已存在就失败。
func (m *Manager) renameWithinRootNoReplace(handle *rootHandle, oldRel, newRel string) error {
	oldParent, oldName, err := m.openParent(handle, oldRel)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(oldParent) }()
	newParent, newName, err := m.openParent(handle, newRel)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(newParent) }()
	return renameNoReplace(oldParent, oldName, newParent, newName)
}

// ───────────────────────────── 备份 ─────────────────────────────

// backupKey 是“一个文件的备份目录”名：相对路径的哈希。
//
// 用哈希而不是把路径嵌进目录名：路径可能很长（PATH_MAX 4096），也可能含奇怪字符，
// 直接当目录名会在某个意料之外的地方超过 NAME_MAX。
func backupKey(rel string) string {
	sum := sha256.Sum256([]byte(rel))
	return hex.EncodeToString(sum[:8])
}

// backupFile 把目标文件现有的字节复制进备份目录，返回备份的相对路径。
func (m *Manager) backupFile(handle *rootHandle, rel string, expectedVersion string) (string, error) {
	parentFD, name, err := m.openParent(handle, rel)
	if err != nil {
		return "", err
	}
	defer func() { _ = unix.Close(parentFD) }()

	srcFD, err := unix.Openat(parentFD, name, openRead|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", fmt.Errorf("files: 无法打开待备份的文件 %s: %w", rel, err)
	}
	src := os.NewFile(uintptr(srcFD), rel)
	defer func() { _ = src.Close() }()
	st, version, err := m.checkRegular(srcFD, rel, handle.entry.identity.dev)
	if err != nil {
		return "", err
	}
	if err := verifyVersion(version, expectedVersion); err != nil {
		return "", err
	}
	if st.Size > MaxEditBackupBytes {
		return "", fmt.Errorf("%w: %s 有 %d 字节，超过单文件备份上限 %d 字节，拒绝编辑",
			ErrTooLarge, rel, st.Size, MaxEditBackupBytes)
	}

	// 备份目录：<root>/.upanel/backups/<key>/
	rootRes, err := m.reservedSubdir(handle, backupSubdir)
	if err != nil {
		return "", err
	}
	defer func() { _ = unix.Close(rootRes) }()
	key := backupKey(rel)
	if err := unix.Mkdirat(rootRes, key, reservedDirMode); err != nil && !errors.Is(err, unix.EEXIST) {
		return "", fmt.Errorf("files: 无法创建备份目录: %w", err)
	}
	// O_RDONLY 而不是 O_PATH：这个 fd 要拿去做 pruneBackups 的 readNames，
	// 而 getdents64 在 O_PATH 的 fd 上是 EBADF（那会让清理永远“失败但不报错”，
	// 备份只增不减）。
	keyFD, err := unix.Openat(rootRes, key,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", fmt.Errorf("files: 无法打开备份目录: %w", err)
	}
	defer func() { _ = unix.Close(keyFD) }()

	// 备份名按纳秒时间戳排序：清理“最旧的几份”就是删名字最小的几个。
	stamp := strconv.FormatInt(m.now().UTC().UnixNano(), 10)
	backupName := stamp
	dstFD, err := unix.Openat(keyFD, backupName,
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return "", fmt.Errorf("files: 无法创建备份文件: %w", err)
	}
	dst := os.NewFile(uintptr(dstFD), backupName)
	written, copyErr := io.CopyBuffer(dst, io.LimitReader(src, MaxEditBackupBytes+1), make([]byte, 64<<10))
	syncErr := dst.Sync()
	closeErr := dst.Close()
	if copyErr != nil {
		_ = unix.Unlinkat(keyFD, backupName, 0)
		return "", fmt.Errorf("files: 备份 %s 失败: %w", rel, copyErr)
	}
	if syncErr != nil {
		_ = unix.Unlinkat(keyFD, backupName, 0)
		return "", fmt.Errorf("files: 备份 %s 落盘失败: %w", rel, syncErr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("files: 备份 %s 关闭失败: %w", rel, closeErr)
	}
	if written != st.Size {
		_ = unix.Unlinkat(keyFD, backupName, 0)
		return "", fmt.Errorf("%w: 备份 %s 时大小从 %d 变成 %d", ErrVersionConflict, rel, st.Size, written)
	}
	if _, _, err := m.pruneBackups(keyFD); err != nil {
		// 清理失败不影响这次编辑：备份已经写成，最坏结果是多留几份。
		m.logger.Warn("清理旧备份失败", "path", rel, "error", err)
	}
	return ReservedDirName + "/" + backupSubdir + "/" + key + "/" + backupName, nil
}

// pruneBackups 删掉超量/超限的旧备份，返回剩余份数与总字节数。
func (m *Manager) pruneBackups(keyFD int) (int, uint64, error) {
	dup, err := unix.Dup(keyFD)
	if err != nil {
		return 0, 0, fmt.Errorf("files: 无法复制备份目录句柄: %w", err)
	}
	dir := os.NewFile(uintptr(dup), "backups")
	defer func() { _ = dir.Close() }()
	names, _, err := readNames(dir)
	if err != nil {
		return 0, 0, err
	}
	sort.Strings(names)

	type backupEntry struct {
		name string
		size uint64
	}
	entries := make([]backupEntry, 0, len(names))
	var total uint64
	for _, name := range names {
		var st unix.Stat_t
		if err := unix.Fstatat(keyFD, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			continue
		}
		entries = append(entries, backupEntry{name: name, size: uint64(st.Size)})
		total += uint64(st.Size)
	}
	// 从最旧的开始删，直到份数与总量都在限制之内。
	for len(entries) > 0 && (len(entries) > MaxEditBackups || total > uint64(MaxEditBackupBytes)) {
		oldest := entries[0]
		if err := unix.Unlinkat(keyFD, oldest.name, 0); err != nil {
			return len(entries), total, fmt.Errorf("files: 无法删除旧备份 %s: %w", oldest.name, err)
		}
		if total >= oldest.size {
			total -= oldest.size
		} else {
			total = 0
		}
		entries = entries[1:]
	}
	return len(entries), total, nil
}

// ───────────────────────────── mkdir ─────────────────────────────

// mkdirAt 在根内创建一个目录，目标已存在就失败。
//
// mkdirat 只有名字不存在时才成功，所以它天然是“不覆盖”的：EEXIST 是
// “目标已存在”，不是“我没权限”。父目录必须是已有的目录（不递归建父级）。
func (m *Manager) mkdirAt(handle *rootHandle, rel string) error {
	parentFD, name, err := m.openParent(handle, rel)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(parentFD) }()
	if err := unix.Mkdirat(parentFD, name, 0o750); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("%w: %s", ErrAlreadyExists, rel)
		}
		return fmt.Errorf("files: 无法创建目录 %s: %w", rel, err)
	}
	return nil
}

// Mkdir 在根内创建一个目录（file.mkdir）。
func (m *Manager) Mkdir(ctx context.Context, params MkdirParams) (WriteResult, error) {
	if err := params.Validate(); err != nil {
		return WriteResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return WriteResult{}, err
	}
	handle, err := m.openRoot(params.RootID)
	if err != nil {
		return WriteResult{}, err
	}
	defer func() { _ = handle.Close() }()
	if !handle.entry.root.Writable {
		return WriteResult{}, fmt.Errorf("%w: %s", ErrNotWritable, params.RootID)
	}
	if err := m.mkdirAt(handle, params.Path); err != nil {
		return WriteResult{}, err
	}
	// 读回：重新解析这个路径，核对它确实是目录、确实在根所在设备上。
	dirFD, err := m.openInRoot(handle.fd, params.Path, unix.O_PATH|unix.O_DIRECTORY)
	if err != nil {
		return WriteResult{}, fmt.Errorf("files: 目录已创建但读不回来: %w", err)
	}
	defer func() { _ = unix.Close(dirFD) }()
	var st unix.Stat_t
	if err := unix.Fstat(dirFD, &st); err != nil {
		return WriteResult{}, fmt.Errorf("files: 无法读取新目录的 stat: %w", err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR || uint64(st.Dev) != handle.entry.identity.dev {
		return WriteResult{}, fmt.Errorf("files: 新目录的身份与预期不符（读到 %s）", describeMode(st.Mode))
	}
	return WriteResult{
		RootID: params.RootID,
		Path:   params.Path,
		Type:   TypeDir,
		Mode:   modeString(st.Mode),
		// 目录的版本描述它自己（inode/大小/mtime），不描述内容。
		Version: statVersion(&st),
	}, nil
}

// ───────────────────────────── 文本改写 ─────────────────────────────

// WriteText 用新内容替换一个文本文件（file.write_text）。
//
// expected_version 的语义在这里是**强制的**：目标已存在而调用方没给版本，操作被拒绝。
// 否则“保存”会变成“用我手里这份覆盖别人刚改的那份”，而这正是阶段验收里
// “版本冲突不覆盖他人修改”要挡的事。
func (m *Manager) WriteText(ctx context.Context, params WriteTextParams) (WriteResult, error) {
	if err := params.Validate(); err != nil {
		return WriteResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return WriteResult{}, err
	}
	handle, err := m.openRoot(params.RootID)
	if err != nil {
		return WriteResult{}, err
	}
	defer func() { _ = handle.Close() }()
	if !handle.entry.root.Writable {
		return WriteResult{}, fmt.Errorf("%w: %s", ErrNotWritable, params.RootID)
	}

	current, err := m.Stat(ctx, params.RootID, params.Path)
	if err != nil {
		return WriteResult{}, err
	}
	if current.Exists {
		if current.Type == TypeDir {
			return WriteResult{}, fmt.Errorf("%w: %s 是目录", ErrIsDirectory, params.Path)
		}
		if current.Type != TypeFile {
			return WriteResult{}, fmt.Errorf("%w: %s 是%s，面板不修改非普通文件",
				ErrPathRejected, params.Path, current.Type)
		}
		if params.ExpectedVersion == "" {
			return WriteResult{}, fmt.Errorf("%w: %s 已存在，必须给出当前版本才能覆盖", ErrVersionConflict, params.Path)
		}
		if current.SizeBytes > MaxTextBytes {
			return WriteResult{}, fmt.Errorf("%w: %s 有 %d 字节，文本编辑上限 %d 字节",
				ErrTooLarge, params.Path, current.SizeBytes, MaxTextBytes)
		}
	}

	content := []byte(params.Content)
	stagingRel, sum, err := m.writeTemp(handle, "wt", content)
	if err != nil {
		return WriteResult{}, err
	}
	committed := false
	defer func() {
		if !committed {
			m.removeStagingTemp(handle, stagingRel)
		}
	}()

	var backupRel string
	if current.Exists {
		backupRel, err = m.backupFile(handle, params.Path, params.ExpectedVersion)
		if err != nil {
			return WriteResult{}, err
		}
	}
	// 落位前再核一次版本：备份与 rename 之间可能有别人改了同名文件。
	if current.Exists {
		again, statErr := m.Stat(ctx, params.RootID, params.Path)
		if statErr == nil && again.Version != current.Version {
			return WriteResult{}, fmt.Errorf("%w: %s 在我们准备落位时被修改", ErrVersionConflict, params.Path)
		}
	}
	if err := m.renameWithinRoot(handle, stagingRel, params.Path); err != nil {
		return WriteResult{}, err
	}
	committed = true

	result, err := m.verifyWritten(ctx, handle, params.Path, sum, int64(len(content)))
	if err != nil {
		return WriteResult{}, err
	}
	result.BackupPath = backupRel
	if backupRel != "" {
		key := backupKey(params.Path)
		if keyFD, openErr := m.openBackupKey(handle, key); openErr == nil {
			count, bytes, pruneErr := m.pruneBackups(keyFD)
			_ = unix.Close(keyFD)
			if pruneErr != nil {
				m.logger.Warn("清理旧备份失败", "path", params.Path, "error", pruneErr)
			}
			result.BackupCount = count
			result.BackupBytes = bytes
		}
	}
	m.logger.Info("已改写文本文件", "root_id", params.RootID, "path", params.Path,
		"bytes", len(content), "sha256", sum, "backup", backupRel)
	return result, nil
}

// openBackupKey 打开某个文件的备份目录。
//
// 打开的必须是**能读**的目录 fd：调用方拿它去 pruneBackups 数份数与字节数，
// 而 O_PATH 的 fd 上 getdents64 会返回 EBADF——那会让 backup_count 永远是 0，
// 界面上看到“留了备份，但一份都没有”。
func (m *Manager) openBackupKey(handle *rootHandle, key string) (int, error) {
	resFD, err := m.openInRoot(handle.fd, ReservedDirName+"/"+backupSubdir, openRead|unix.O_DIRECTORY)
	if err != nil {
		return -1, err
	}
	defer func() { _ = unix.Close(resFD) }()
	return unix.Openat(resFD, key,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
}

// verifyWritten 重新打开刚落位的文件，核对大小与内容摘要。
//
// 这是“结果验证”的实现：renameat 成功不算完成，读回来一致才算。
func (m *Manager) verifyWritten(ctx context.Context, handle *rootHandle, rel, wantSum string, wantSize int64) (WriteResult, error) {
	if err := ctx.Err(); err != nil {
		return WriteResult{}, err
	}
	fd, err := m.openInRoot(handle.fd, rel, openRead)
	if err != nil {
		return WriteResult{}, fmt.Errorf("files: 写入后读不回来: %w", err)
	}
	file := os.NewFile(uintptr(fd), rel)
	defer func() { _ = file.Close() }()
	st, version, err := m.checkRegular(fd, rel, handle.entry.identity.dev)
	if err != nil {
		return WriteResult{}, err
	}
	if st.Size != wantSize {
		return WriteResult{}, fmt.Errorf("%w: 写入后大小是 %d，预期 %d", ErrVersionConflict, st.Size, wantSize)
	}
	if wantSum != "" {
		raw, err := io.ReadAll(io.LimitReader(file, wantSize+1))
		if err != nil {
			return WriteResult{}, fmt.Errorf("files: 读回 %s 失败: %w", rel, err)
		}
		if got := sha256Hex(raw); got != wantSum {
			return WriteResult{}, fmt.Errorf("%w: 写入后内容摘要不一致（%s != %s）",
				ErrVersionConflict, got, wantSum)
		}
	}
	return WriteResult{
		RootID:    handle.entry.root.ID,
		Path:      rel,
		Type:      TypeFile,
		SizeBytes: uint64(st.Size),
		Mode:      modeString(st.Mode),
		Version:   version,
	}, nil
}

// ───────────────────────────── 提交上传 ─────────────────────────────

// CommitUpload 把一份已收完的暂存原子落位到目标路径（file.commit_upload）。
//
// 这是“上传”与“修改服务器文件”之间那道显式的门：预检与任务都在这里，
// 目标字节只在这一步改变。
func (m *Manager) CommitUpload(ctx context.Context, params CommitUploadParams) (WriteResult, error) {
	if err := params.Validate(); err != nil {
		return WriteResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return WriteResult{}, err
	}
	handle, dirFD, meta, err := m.openUpload(params.UploadID)
	if err != nil {
		return WriteResult{}, err
	}
	defer func() { _ = handle.Close() }()
	defer func() { _ = unix.Close(dirFD) }()

	if meta.State != UploadStateReceived {
		return WriteResult{}, fmt.Errorf("%w: 上传 %s 的当前状态是 %s，不能提交", ErrUploadState, meta.UploadID, meta.State)
	}
	if !m.now().UTC().Before(meta.ExpiresAt) {
		return WriteResult{}, fmt.Errorf("%w: 上传 %s 的暂存已过期", ErrUploadState, meta.UploadID)
	}
	if !handle.entry.root.Writable {
		return WriteResult{}, fmt.Errorf("%w: %s", ErrNotWritable, meta.RootID)
	}

	current, err := m.Stat(ctx, meta.RootID, meta.Path)
	if err != nil {
		return WriteResult{}, err
	}
	if current.Exists {
		if !params.Overwrite {
			return WriteResult{}, fmt.Errorf("%w: %s 已存在（未选择覆盖）", ErrAlreadyExists, meta.Path)
		}
		if current.Type == TypeDir {
			return WriteResult{}, fmt.Errorf("%w: %s 是目录", ErrIsDirectory, meta.Path)
		}
		if current.Type != TypeFile {
			return WriteResult{}, fmt.Errorf("%w: %s 是%s，面板不替换非普通文件",
				ErrPathRejected, meta.Path, current.Type)
		}
		if params.ExpectedVersion == "" || params.ExpectedVersion != current.Version {
			return WriteResult{}, fmt.Errorf("%w: %s 已被修改，覆盖被拒绝", ErrVersionConflict, meta.Path)
		}
	} else if params.ExpectedVersion != "" {
		return WriteResult{}, fmt.Errorf("%w: %s 已不存在，不能按版本覆盖", ErrVersionConflict, meta.Path)
	}

	// 记下落位前的 inode：rename 保留 inode，因此落位后读到同一个 inode
	// 就说明“目标路径上的这个文件，就是刚刚那份暂存”。
	partFD, err := unix.Openat(dirFD, meta.UploadID+uploadDataSuffix,
		unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return WriteResult{}, fmt.Errorf("%w: 暂存数据不在（可能已被清理）：%v", ErrUploadState, err)
	}
	var partStat unix.Stat_t
	if err := unix.Fstat(partFD, &partStat); err != nil {
		_ = unix.Close(partFD)
		return WriteResult{}, fmt.Errorf("files: 无法读取暂存数据的 stat: %w", err)
	}
	_ = unix.Close(partFD)
	if uint64(partStat.Size) != meta.SizeBytes {
		return WriteResult{}, fmt.Errorf("%w: 暂存数据大小 %d 与声明 %d 不符",
			ErrUploadState, partStat.Size, meta.SizeBytes)
	}

	var backupRel string
	if current.Exists {
		backupRel, err = m.backupFile(handle, meta.Path, params.ExpectedVersion)
		if err != nil {
			return WriteResult{}, err
		}
	}
	// 落位前再核一次目标版本：备份与 rename 之间别人可能动了同一个文件。
	if current.Exists {
		again, statErr := m.Stat(ctx, meta.RootID, meta.Path)
		if statErr == nil && again.Version != current.Version {
			return WriteResult{}, fmt.Errorf("%w: %s 在提交过程中被修改", ErrVersionConflict, meta.Path)
		}
	}

	parentFD, name, err := m.openParent(handle, meta.Path)
	if err != nil {
		return WriteResult{}, err
	}
	// renameat 在同一个文件系统内是原子的：目标要么是旧文件，要么是这份上传，
	// 不存在“写了一半的配置文件”。
	renameErr := renameAt(dirFD, meta.UploadID+uploadDataSuffix, parentFD, name)
	_ = unix.Close(parentFD)
	if renameErr != nil {
		return WriteResult{}, renameErr
	}
	// 暂存记录可以走了：数据已经不在暂存目录里。
	_ = unix.Unlinkat(dirFD, meta.UploadID+uploadMetaSuffix, 0)

	result, err := m.verifyCommitted(ctx, handle, meta, partStat)
	if err != nil {
		return WriteResult{}, err
	}
	result.BackupPath = backupRel
	if backupRel != "" {
		if keyFD, openErr := m.openBackupKey(handle, backupKey(meta.Path)); openErr == nil {
			count, bytes, pruneErr := m.pruneBackups(keyFD)
			_ = unix.Close(keyFD)
			if pruneErr != nil {
				m.logger.Warn("清理旧备份失败", "path", meta.Path, "error", pruneErr)
			}
			result.BackupCount = count
			result.BackupBytes = bytes
		}
	}
	m.logger.Info("已提交上传", "upload_id", meta.UploadID, "root_id", meta.RootID,
		"path", meta.Path, "bytes", meta.SizeBytes, "sha256", meta.SHA256, "backup", backupRel)
	return result, nil
}

// verifyCommitted 读回刚落位的文件，核对 inode、大小与摘要。
//
// 大文件不做整份 sha256（那会把一次提交变成读一遍全盘）：用“同一个 inode +
// 同一个大小”来证明落位的就是那份暂存。暂存写入时算的 sha256 记在日志里
// （“已提交上传”那条），它证明的是“收进来的字节没变”，不是“落位后又读了一遍”。
func (m *Manager) verifyCommitted(ctx context.Context, handle *rootHandle, meta uploadMeta, partStat unix.Stat_t) (WriteResult, error) {
	if err := ctx.Err(); err != nil {
		return WriteResult{}, err
	}
	fd, err := m.openInRoot(handle.fd, meta.Path, openRead)
	if err != nil {
		return WriteResult{}, fmt.Errorf("files: 提交后读不回来: %w", err)
	}
	file := os.NewFile(uintptr(fd), meta.Path)
	defer func() { _ = file.Close() }()
	st, version, err := m.checkRegular(fd, meta.Path, handle.entry.identity.dev)
	if err != nil {
		return WriteResult{}, err
	}
	if st.Ino != partStat.Ino || st.Dev != partStat.Dev {
		return WriteResult{}, fmt.Errorf("%w: 落位后的 inode 与暂存不一致（可能被并发替换）", ErrVersionConflict)
	}
	if uint64(st.Size) != meta.SizeBytes {
		return WriteResult{}, fmt.Errorf("%w: 落位后大小 %d 与声明 %d 不符", ErrVersionConflict, st.Size, meta.SizeBytes)
	}
	return WriteResult{
		RootID:    meta.RootID,
		Path:      meta.Path,
		Type:      TypeFile,
		SizeBytes: uint64(st.Size),
		Mode:      modeString(st.Mode),
		Version:   version,
	}, nil
}

// ───────────────────────────── 删除 ─────────────────────────────

// Remove 删除根内的一个文件/符号链接/目录（file.delete）。
func (m *Manager) Remove(ctx context.Context, params DeleteParams) (WriteResult, error) {
	if err := params.Validate(); err != nil {
		return WriteResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return WriteResult{}, err
	}
	handle, err := m.openRoot(params.RootID)
	if err != nil {
		return WriteResult{}, err
	}
	defer func() { _ = handle.Close() }()
	if !handle.entry.root.Writable {
		return WriteResult{}, fmt.Errorf("%w: %s", ErrNotWritable, params.RootID)
	}
	current, err := m.Stat(ctx, params.RootID, params.Path)
	if err != nil {
		return WriteResult{}, err
	}
	if !current.Exists {
		return WriteResult{}, fmt.Errorf("%w: %s", ErrNotFound, params.Path)
	}
	if err := verifyVersion(current.Version, params.ExpectedVersion); err != nil {
		return WriteResult{}, err
	}
	parentFD, name, err := m.openParent(handle, params.Path)
	if err != nil {
		return WriteResult{}, err
	}

	result := WriteResult{RootID: params.RootID, Path: params.Path, Type: current.Type}
	if current.Type == TypeDir {
		tree, err := m.StatTree(ctx, params.RootID, params.Path)
		if err != nil {
			_ = unix.Close(parentFD)
			return WriteResult{}, err
		}
		if tree.TreeTruncated {
			_ = unix.Close(parentFD)
			return WriteResult{}, fmt.Errorf("%w: %s 的目录树超过 %d 项，拒绝递归删除",
				ErrTreeTooLarge, params.Path, MaxTreeEntries)
		}
		if !params.Recursive && tree.TreeEntries > 0 {
			_ = unix.Close(parentFD)
			return WriteResult{}, fmt.Errorf("%w: 目录 %s 不是空的（%d 项），递归删除需要 recursive=true",
				ErrInvalidParams, params.Path, tree.TreeEntries)
		}
		result.Entries = tree.TreeEntries + 1
		result.Bytes = tree.TreeBytes
		if params.Recursive {
			if err := m.removeTree(parentFD, name, 0); err != nil {
				_ = unix.Close(parentFD)
				return WriteResult{}, err
			}
		} else if err := unix.Unlinkat(parentFD, name, unix.AT_REMOVEDIR); err != nil {
			_ = unix.Close(parentFD)
			return WriteResult{}, fmt.Errorf("files: 无法删除目录 %s: %w", params.Path, err)
		}
	} else {
		// 普通文件、符号链接、特殊文件都用 unlinkat 删名字：
		// 删符号链接删的是链接本身，不会碰到它指向的东西。
		result.Entries = 1
		result.Bytes = current.SizeBytes
		if err := unix.Unlinkat(parentFD, name, 0); err != nil {
			_ = unix.Close(parentFD)
			return WriteResult{}, fmt.Errorf("files: 无法删除 %s: %w", params.Path, err)
		}
	}
	// 读回确认：还在就是失败。
	var st unix.Stat_t
	err = unix.Fstatat(parentFD, name, &st, unix.AT_SYMLINK_NOFOLLOW)
	_ = unix.Close(parentFD)
	if err == nil {
		return WriteResult{}, fmt.Errorf("%w: 删除后 %s 仍然存在", ErrVersionConflict, params.Path)
	}
	if !errors.Is(err, unix.ENOENT) {
		return WriteResult{}, fmt.Errorf("files: 删除后无法确认 %s 的状态: %w", params.Path, err)
	}
	m.logger.Info("已删除文件", "root_id", params.RootID, "path", params.Path,
		"type", current.Type, "entries", result.Entries, "bytes", result.Bytes)
	return result, nil
}

// removeTree 递归删除一个目录（不跟随符号链接）。
func (m *Manager) removeTree(parentFD int, name string, depth int) error {
	if depth > maxTreeDepth {
		return fmt.Errorf("%w: 目录嵌套超过 %d 层", ErrTreeTooLarge, maxTreeDepth)
	}
	dirFD, err := unix.Openat(parentFD, name,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return fmt.Errorf("files: 无法打开目录 %s: %w", name, err)
	}
	defer func() { _ = unix.Close(dirFD) }()
	// ReadDir 会推进文件偏移，所以用一个 dup 出来的句柄读名字，原来的 dirFD
	// 留着给 unlinkat/fstatat 用（关掉它就没法再对子项操作了）。
	dup, err := unix.Dup(dirFD)
	if err != nil {
		return fmt.Errorf("files: 无法复制目录句柄: %w", err)
	}
	dir := os.NewFile(uintptr(dup), name)
	names, truncated, readErr := readNames(dir)
	_ = dir.Close()
	if readErr != nil {
		return readErr
	}
	if truncated {
		return fmt.Errorf("%w: 目录 %s 超过 %d 项", ErrTreeTooLarge, name, MaxListScan)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	for _, child := range names {
		var st unix.Stat_t
		if err := unix.Fstatat(dirFD, child, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			if errors.Is(err, unix.ENOENT) {
				continue
			}
			return fmt.Errorf("files: 无法读取 %s 的信息: %w", child, err)
		}
		if st.Mode&unix.S_IFMT == unix.S_IFDIR {
			if err := m.removeTree(dirFD, child, depth+1); err != nil {
				return err
			}
			continue
		}
		if err := unix.Unlinkat(dirFD, child, 0); err != nil {
			if errors.Is(err, unix.ENOENT) {
				continue
			}
			return fmt.Errorf("files: 无法删除 %s: %w", child, err)
		}
	}
	if err := unix.Unlinkat(parentFD, name, unix.AT_REMOVEDIR); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return fmt.Errorf("files: 无法删除目录 %s: %w", name, err)
	}
	return nil
}

// ───────────────────────────── 复制 / 移动 ─────────────────────────────

// Copy 复制（file.copy）。目标存在则拒绝：覆盖要走显式的删除或提交上传。
func (m *Manager) Copy(ctx context.Context, params CopyParams) (WriteResult, error) {
	return m.copyOrMove(ctx, params, false)
}

// Move 移动（file.move）。
//
// 同根内是 rename（原子）；跨根是“复制到目标暂存 → 校验 → 原子落位 → 删除源”。
// 后者**不是**原子操作：中途失败会保留源、并如实报告实际状态。
func (m *Manager) Move(ctx context.Context, params CopyParams) (WriteResult, error) {
	return m.copyOrMove(ctx, params, true)
}

func (m *Manager) copyOrMove(ctx context.Context, params CopyParams, move bool) (WriteResult, error) {
	if err := params.Validate(); err != nil {
		return WriteResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return WriteResult{}, err
	}
	srcHandle, err := m.openRoot(params.SourceRootID)
	if err != nil {
		return WriteResult{}, err
	}
	defer func() { _ = srcHandle.Close() }()
	dstHandle, err := m.openRoot(params.TargetRootID)
	if err != nil {
		return WriteResult{}, err
	}
	defer func() { _ = dstHandle.Close() }()
	if !dstHandle.entry.root.Writable {
		return WriteResult{}, fmt.Errorf("%w: %s", ErrNotWritable, params.TargetRootID)
	}
	if move && !srcHandle.entry.root.Writable {
		return WriteResult{}, fmt.Errorf("%w: %s", ErrNotWritable, params.SourceRootID)
	}

	source, err := m.StatTree(ctx, params.SourceRootID, params.SourcePath)
	if err != nil {
		return WriteResult{}, err
	}
	if !source.Exists {
		return WriteResult{}, fmt.Errorf("%w: 源 %s", ErrNotFound, params.SourcePath)
	}
	if source.Type != TypeFile && source.Type != TypeDir {
		return WriteResult{}, fmt.Errorf("%w: 源 %s 是%s，面板不复制它",
			ErrTreeUnsupported, params.SourcePath, source.Type)
	}
	target, err := m.Stat(ctx, params.TargetRootID, params.TargetPath)
	if err != nil {
		return WriteResult{}, err
	}
	if target.Exists {
		return WriteResult{}, fmt.Errorf("%w: 目标 %s", ErrAlreadyExists, params.TargetPath)
	}
	if params.ExpectedVersion != "" && !target.Exists {
		return WriteResult{}, fmt.Errorf("%w: 目标不存在却给了 expected_version", ErrInvalidParams)
	}
	if source.Type == TypeDir {
		if source.TreeSymlinks > 0 || source.TreeOthers > 0 {
			return WriteResult{}, fmt.Errorf("%w: 源目录里有 %d 个符号链接、%d 个特殊文件，拒绝整棵树复制",
				ErrTreeUnsupported, source.TreeSymlinks, source.TreeOthers)
		}
		if source.TreeTruncated {
			return WriteResult{}, fmt.Errorf("%w: 源目录超过 %d 项", ErrTreeTooLarge, MaxTreeEntries)
		}
	}

	sameRoot := params.SourceRootID == params.TargetRootID
	if sameRoot {
		// 同根：rename 就是移动（原子）。复制则要真的复制字节。
		if move {
			if err := m.renameWithinRootNoReplace(srcHandle, params.SourcePath, params.TargetPath); err != nil {
				return WriteResult{}, err
			}
			after, err := m.Stat(ctx, params.TargetRootID, params.TargetPath)
			if err != nil {
				return WriteResult{}, err
			}
			if !after.Exists || after.Version != source.Version {
				return WriteResult{}, fmt.Errorf("%w: 移动后读回的目标与源不一致", ErrVersionConflict)
			}
			m.logger.Info("已移动文件（同根 rename）", "root_id", params.SourceRootID,
				"source", params.SourcePath, "target", params.TargetPath)
			return WriteResult{
				RootID: params.TargetRootID, Path: params.TargetPath, Type: after.Type,
				SizeBytes: after.SizeBytes, Mode: after.Mode, Version: after.Version,
				Source: params.SourceRootID + ":" + params.SourcePath,
			}, nil
		}
		if source.Type == TypeFile {
			if err := m.reserveSpace(ctx, dstHandle.entry, int64(source.SizeBytes)); err != nil {
				return WriteResult{}, err
			}
			defer m.releaseSpace(params.TargetRootID, int64(source.SizeBytes))
		}
		result, err := m.copyEntry(ctx, srcHandle, dstHandle, params.SourcePath, params.TargetPath, source)
		if err != nil {
			return WriteResult{}, err
		}
		result.Source = params.SourceRootID + ":" + params.SourcePath
		return result, nil
	}

	// 跨根：复制 → 校验 → 原子落位（单文件）或逐项建立（目录）→ 删除源。
	if source.Type == TypeFile {
		if err := m.reserveSpace(ctx, dstHandle.entry, int64(source.SizeBytes)); err != nil {
			return WriteResult{}, err
		}
		m.releaseSpace(params.TargetRootID, int64(source.SizeBytes))
	}
	result, err := m.copyEntry(ctx, srcHandle, dstHandle, params.SourcePath, params.TargetPath, source)
	if err != nil {
		// 失败保留源：这是契约要求的“不声称原子完成，保留源并显示实际状态”。
		return WriteResult{}, err
	}
	result.Source = params.SourceRootID + ":" + params.SourcePath
	if !move {
		return result, nil
	}
	removed, err := m.Remove(ctx, DeleteParams{
		RootID: params.SourceRootID, Path: params.SourcePath,
		ExpectedVersion: source.Version, Recursive: source.Type == TypeDir,
	})
	if err != nil {
		// 复制已经成功、删除源失败：如实报错，让调用方看到“两份都在”。
		return WriteResult{}, fmt.Errorf("files: 目标已复制成功，但删除源失败（源仍在 %s:%s）：%w",
			params.SourceRootID, params.SourcePath, err)
	}
	m.logger.Info("已跨根移动文件", "source_root", params.SourceRootID, "source", params.SourcePath,
		"target_root", params.TargetRootID, "target", params.TargetPath)
	result.Entries = removed.Entries
	return result, nil
}

// copyEntry 把源条目复制到目标，并读回验证。
func (m *Manager) copyEntry(ctx context.Context, src, dst *rootHandle, srcRel, dstRel string, source PathStat) (WriteResult, error) {
	if source.Type == TypeFile {
		return m.copyRegularFile(ctx, src, dst, srcRel, dstRel)
	}
	return m.copyDirectory(ctx, src, dst, srcRel, dstRel)
}

// copyRegularFile 复制一个普通文件：读到目标暂存 → fsync → 原子落位 → 读回比对摘要。
func (m *Manager) copyRegularFile(ctx context.Context, src, dst *rootHandle, srcRel, dstRel string) (WriteResult, error) {
	srcParent, srcName, err := m.openParent(src, srcRel)
	if err != nil {
		return WriteResult{}, err
	}
	srcFD, err := unix.Openat(srcParent, srcName, openRead|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	_ = unix.Close(srcParent)
	if err != nil {
		return WriteResult{}, fmt.Errorf("files: 无法打开源文件 %s: %w", srcRel, err)
	}
	srcFile := os.NewFile(uintptr(srcFD), srcRel)
	defer func() { _ = srcFile.Close() }()
	st, _, err := m.checkRegular(srcFD, srcRel, src.entry.identity.dev)
	if err != nil {
		return WriteResult{}, err
	}

	tmpFD, tmpRel, err := m.stagingTemp(dst, "cp")
	if err != nil {
		return WriteResult{}, err
	}
	tmpFile := os.NewFile(uintptr(tmpFD), tmpRel)
	hasher := sha256.New()
	copied, copyErr := io.CopyBuffer(io.MultiWriter(tmpFile, hasher), srcFile, make([]byte, 64<<10))
	syncErr := tmpFile.Sync()
	closeErr := tmpFile.Close()
	if copyErr != nil || syncErr != nil || closeErr != nil {
		m.removeStagingTemp(dst, tmpRel)
		return WriteResult{}, fmt.Errorf("files: 复制 %s 失败: %v %v %v", srcRel, copyErr, syncErr, closeErr)
	}
	if copied != st.Size {
		m.removeStagingTemp(dst, tmpRel)
		return WriteResult{}, fmt.Errorf("%w: 复制 %s 时大小从 %d 变成 %d", ErrVersionConflict, srcRel, st.Size, copied)
	}
	if err := m.renameWithinRootNoReplace(dst, tmpRel, dstRel); err != nil {
		m.removeStagingTemp(dst, tmpRel)
		return WriteResult{}, err
	}
	result, err := m.verifyWritten(ctx, dst, dstRel, hex.EncodeToString(hasher.Sum(nil)), copied)
	if err != nil {
		return WriteResult{}, err
	}
	return result, nil
}

// copyDirectory 复制一个目录树（目标必须不存在）。
//
// 它不是原子的：目录一个个建、文件一个个落位。中途失败会留下“复制了一部分”的目标，
// 结果里报的是实际状态，不会说“已完成”。
func (m *Manager) copyDirectory(ctx context.Context, src, dst *rootHandle, srcRel, dstRel string) (WriteResult, error) {
	if err := m.mkdirAt(dst, dstRel); err != nil {
		return WriteResult{}, err
	}
	if err := m.copyDirContents(ctx, src, dst, srcRel, dstRel, 0); err != nil {
		return WriteResult{}, err
	}
	after, err := m.Stat(ctx, dst.entry.root.ID, dstRel)
	if err != nil {
		return WriteResult{}, err
	}
	return WriteResult{
		RootID: dst.entry.root.ID, Path: dstRel, Type: TypeDir,
		SizeBytes: after.SizeBytes, Mode: after.Mode, Version: after.Version,
	}, nil
}

// copyDirContents 递归复制目录内容。
func (m *Manager) copyDirContents(ctx context.Context, src, dst *rootHandle, srcRel, dstRel string, depth int) error {
	if depth > maxTreeDepth {
		return fmt.Errorf("%w: 目录嵌套超过 %d 层", ErrTreeTooLarge, maxTreeDepth)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	srcFD, err := m.openInRoot(src.fd, srcRel, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NONBLOCK)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(srcFD) }()
	dup, err := unix.Dup(srcFD)
	if err != nil {
		return fmt.Errorf("files: 无法复制目录句柄: %w", err)
	}
	dir := os.NewFile(uintptr(dup), srcRel)
	names, truncated, readErr := readNames(dir)
	_ = dir.Close()
	if readErr != nil {
		return readErr
	}
	if truncated {
		return fmt.Errorf("%w: 目录 %s 超过 %d 项", ErrTreeTooLarge, srcRel, MaxListScan)
	}
	sort.Strings(names)
	for _, name := range names {
		childSrc := JoinPath(srcRel, name)
		childDst := JoinPath(dstRel, name)
		var st unix.Stat_t
		if err := unix.Fstatat(srcFD, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			if errors.Is(err, unix.ENOENT) {
				continue
			}
			return fmt.Errorf("files: 无法读取 %s: %w", childSrc, err)
		}
		switch st.Mode & unix.S_IFMT {
		case unix.S_IFDIR:
			if err := m.mkdirAt(dst, childDst); err != nil {
				return err
			}
			if err := m.copyDirContents(ctx, src, dst, childSrc, childDst, depth+1); err != nil {
				return err
			}
		case unix.S_IFREG:
			if st.Nlink > 1 {
				return fmt.Errorf("%w: %s 有 %d 个硬链接", ErrTreeUnsupported, childSrc, st.Nlink)
			}
			if _, err := m.copyRegularFile(ctx, src, dst, childSrc, childDst); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%w: %s 是%s", ErrTreeUnsupported, childSrc, describeMode(st.Mode))
		}
	}
	return nil
}

// ───────────────────────────── 权限 / 所有者 ─────────────────────────────

// Chmod 修改权限位（file.chmod）。只接受普通权限位。
func (m *Manager) Chmod(ctx context.Context, params ChmodParams) (WriteResult, error) {
	if err := params.Validate(); err != nil {
		return WriteResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return WriteResult{}, err
	}
	mode, _ := ParseMode(params.Mode) // Validate 已经检查过
	handle, err := m.openRoot(params.RootID)
	if err != nil {
		return WriteResult{}, err
	}
	defer func() { _ = handle.Close() }()
	if !handle.entry.root.Writable {
		return WriteResult{}, fmt.Errorf("%w: %s", ErrNotWritable, params.RootID)
	}
	current, err := m.Stat(ctx, params.RootID, params.Path)
	if err != nil {
		return WriteResult{}, err
	}
	if !current.Exists {
		return WriteResult{}, fmt.Errorf("%w: %s", ErrNotFound, params.Path)
	}
	if current.Type != TypeFile && current.Type != TypeDir {
		return WriteResult{}, fmt.Errorf("%w: %s 是%s，面板不修改它的权限",
			ErrPathRejected, params.Path, describeType(current.Type))
	}
	if err := verifyVersion(current.Version, params.ExpectedVersion); err != nil {
		return WriteResult{}, err
	}

	// 用**已打开的句柄**做 fchmod：路径在这一步不再参与解析，所以不会被并发换成一个
	// 符号链接，也不存在“先 lstat 再 chmod”的窗口（chmod 是跟随符号链接的）。
	fd, err := m.openHandleForMode(handle, params.Path, current.Type)
	if err != nil {
		return WriteResult{}, err
	}
	defer func() { _ = unix.Close(fd) }()
	if err := unix.Fchmod(fd, mode); err != nil {
		return WriteResult{}, fmt.Errorf("files: 无法修改 %s 的权限: %w", params.Path, err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return WriteResult{}, fmt.Errorf("files: 无法读取修改后的 stat: %w", err)
	}
	if uint32(st.Mode)&0o777 != mode {
		return WriteResult{}, fmt.Errorf("%w: 修改后权限是 %s，预期 %s",
			ErrVersionConflict, FormatMode(uint32(st.Mode)&0o777), params.Mode)
	}
	m.logger.Info("已修改权限", "root_id", params.RootID, "path", params.Path,
		"from", current.Mode, "to", params.Mode)
	return WriteResult{
		RootID: params.RootID, Path: params.Path, Type: current.Type,
		SizeBytes: current.SizeBytes, Mode: modeString(st.Mode), Version: statVersion(&st),
	}, nil
}

// openHandleForMode 打开一个“用于改属性”的句柄。
//
// 目录用 O_RDONLY|O_DIRECTORY，普通文件用 openRead（O_NONBLOCK 防止最后一段其实是
// FIFO 时把请求挂住）；两者都带 O_NOFOLLOW。
func (m *Manager) openHandleForMode(handle *rootHandle, rel, entryTypeName string) (int, error) {
	flags := openRead
	if entryTypeName == TypeDir {
		flags |= unix.O_DIRECTORY
	}
	return m.openInRoot(handle.fd, rel, flags)
}

// describeType 把目录项类型翻译成中文（用于错误消息）。
func describeType(t string) string {
	switch t {
	case TypeDir:
		return "目录"
	case TypeFile:
		return "普通文件"
	case TypeSymlink:
		return "符号链接"
	default:
		return "特殊文件"
	}
}

// Chown 修改所有者（file.chown），只能改成受管理账号/组。
func (m *Manager) Chown(ctx context.Context, params ChownParams) (WriteResult, error) {
	if err := params.Validate(); err != nil {
		return WriteResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return WriteResult{}, err
	}
	if m.accounts == nil {
		// 账号模块还没有落地（P08）。这里不能“先接一个 uid 用着”：
		// 那会把“受管理账号”这个边界换成“任意数字”。
		return WriteResult{}, fmt.Errorf("%w: 受管理账号模块尚未落地，file.chown 暂不可用", ErrUnsupported)
	}
	handle, err := m.openRoot(params.RootID)
	if err != nil {
		return WriteResult{}, err
	}
	defer func() { _ = handle.Close() }()
	current, err := m.Stat(ctx, params.RootID, params.Path)
	if err != nil {
		return WriteResult{}, err
	}
	if !current.Exists {
		return WriteResult{}, fmt.Errorf("%w: %s", ErrNotFound, params.Path)
	}
	if current.Type != TypeFile && current.Type != TypeDir {
		return WriteResult{}, fmt.Errorf("%w: %s 是%s，面板不修改它的所有者",
			ErrPathRejected, params.Path, describeType(current.Type))
	}
	if err := verifyVersion(current.Version, params.ExpectedVersion); err != nil {
		return WriteResult{}, err
	}

	uid := -1
	gid := -1
	if params.ManagedAccountID > 0 {
		resolved, _, err := m.accounts.ResolveAccount(ctx, params.ManagedAccountID)
		if err != nil {
			return WriteResult{}, fmt.Errorf("files: 受管理账号 %d 无法解析: %w", params.ManagedAccountID, err)
		}
		uid = int(resolved)
	}
	if params.ManagedGroupID > 0 {
		resolved, _, err := m.accounts.ResolveGroup(ctx, params.ManagedGroupID)
		if err != nil {
			return WriteResult{}, fmt.Errorf("files: 受管理组 %d 无法解析: %w", params.ManagedGroupID, err)
		}
		gid = int(resolved)
	}
	fd, err := m.openHandleForMode(handle, params.Path, current.Type)
	if err != nil {
		return WriteResult{}, err
	}
	defer func() { _ = unix.Close(fd) }()
	if err := unix.Fchown(fd, uid, gid); err != nil {
		return WriteResult{}, fmt.Errorf("files: 无法修改 %s 的所有者: %w", params.Path, err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return WriteResult{}, fmt.Errorf("files: 无法读取修改后的 stat: %w", err)
	}
	if uid >= 0 && uint32(st.Uid) != uint32(uid) {
		return WriteResult{}, fmt.Errorf("%w: 修改后属主是 %d，预期 %d", ErrVersionConflict, st.Uid, uid)
	}
	if gid >= 0 && uint32(st.Gid) != uint32(gid) {
		return WriteResult{}, fmt.Errorf("%w: 修改后属组是 %d，预期 %d", ErrVersionConflict, st.Gid, gid)
	}
	return WriteResult{
		RootID: params.RootID, Path: params.Path, Type: current.Type,
		SizeBytes: current.SizeBytes, Mode: modeString(st.Mode), Version: statVersion(&st),
	}, nil
}
