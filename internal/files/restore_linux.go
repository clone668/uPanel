//go:build linux

package files

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

// RestoreSession 持有暂存目录及目标的父目录句柄。Restic 只能拿到 /proc/self/fd/N，
// 即使暂存目录被并发改名，写入仍落在同一个已打开目录里。
type RestoreSession struct {
	m             *Manager
	handle        *rootHandle
	parentFD      int
	stageFD       int
	rootDev       uint64
	path          string
	targetName    string
	stageName     string
	targetVersion string
	targetExists  bool
	overwrite     bool
	published     bool
}

// ProbeRestore 只读核对根、父目录与目标形状，不创建暂存。
func (m *Manager) ProbeRestore(ctx context.Context, params RestoreParams) (RestoreProbe, error) {
	if err := ctx.Err(); err != nil {
		return RestoreProbe{}, err
	}
	if err := validateRootID(params.RootID); err != nil {
		return RestoreProbe{}, err
	}
	if err := ValidateRelPath(params.Path); err != nil || params.Path == "" {
		if err == nil {
			err = fmt.Errorf("%w: 恢复不能覆盖文件根自身", ErrInvalidParams)
		}
		return RestoreProbe{}, err
	}
	handle, err := m.openRoot(params.RootID)
	if err != nil {
		return RestoreProbe{}, err
	}
	defer func() { _ = handle.Close() }()
	if !handle.entry.root.Writable {
		return RestoreProbe{}, fmt.Errorf("%w: %s", ErrNotWritable, params.RootID)
	}
	probe := RestoreProbe{}
	if capacity, _ := m.capacityFor(ctx, handle.entry); capacity != nil {
		probe.CapacityKnown = true
		probe.AvailableBytes = capacity.AvailableBytes
	}
	parentFD, name, err := m.openParent(handle, params.Path)
	if err != nil {
		return RestoreProbe{}, err
	}
	defer func() { _ = unix.Close(parentFD) }()
	var st unix.Stat_t
	err = unix.Fstatat(parentFD, name, &st, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		if params.Overwrite {
			return RestoreProbe{}, fmt.Errorf("%w: 覆盖恢复要求目标目录已经存在", ErrNotFound)
		}
		return probe, nil
	}
	if err != nil {
		return RestoreProbe{}, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return RestoreProbe{}, fmt.Errorf("%w: 恢复目标必须是目录且不能是符号链接", ErrPathRejected)
	}
	probe.Exists = true
	probe.TargetVersion = statVersion(&st)
	if !params.Overwrite {
		names, err := restoreChildNames(parentFD, name)
		if err != nil {
			return RestoreProbe{}, err
		}
		probe.Empty = len(names) == 0
		if !probe.Empty {
			return RestoreProbe{}, fmt.Errorf("%w: 默认恢复只接受不存在或已经为空的目录", ErrAlreadyExists)
		}
	}
	return probe, nil
}

func (m *Manager) BeginRestore(ctx context.Context, params RestoreParams) (*RestoreSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateRootID(params.RootID); err != nil {
		return nil, err
	}
	if err := ValidateRelPath(params.Path); err != nil || params.Path == "" {
		if err == nil {
			err = fmt.Errorf("%w: 恢复不能覆盖文件根自身", ErrInvalidParams)
		}
		return nil, err
	}
	handle, err := m.openRoot(params.RootID)
	if err != nil {
		return nil, err
	}
	s := &RestoreSession{m: m, handle: handle, parentFD: -1, stageFD: -1,
		rootDev: handle.entry.identity.dev, path: params.Path, targetName: BasePath(params.Path), overwrite: params.Overwrite}
	ok := false
	defer func() {
		if !ok {
			_ = s.Abort(context.Background())
		}
	}()
	if !handle.entry.root.Writable {
		return nil, fmt.Errorf("%w: %s", ErrNotWritable, params.RootID)
	}
	s.parentFD, _, err = m.openParent(handle, params.Path)
	if err != nil {
		return nil, err
	}
	var st unix.Stat_t
	err = unix.Fstatat(s.parentFD, s.targetName, &st, unix.AT_SYMLINK_NOFOLLOW)
	switch {
	case err == nil:
		s.targetExists = true
		if st.Mode&unix.S_IFMT != unix.S_IFDIR {
			return nil, fmt.Errorf("%w: 恢复目标必须是目录且不能是符号链接", ErrPathRejected)
		}
		if !params.Overwrite {
			names, err := restoreChildNames(s.parentFD, s.targetName)
			if err != nil {
				return nil, err
			}
			if len(names) != 0 {
				return nil, fmt.Errorf("%w: 默认恢复只接受不存在或已经为空的目录", ErrAlreadyExists)
			}
		}
		s.targetVersion = statVersion(&st)
	case errors.Is(err, unix.ENOENT):
		if params.Overwrite {
			return nil, fmt.Errorf("%w: 覆盖恢复要求目标目录已经存在", ErrNotFound)
		}
	default:
		return nil, fmt.Errorf("files: 无法读取恢复目标: %w", err)
	}
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, fmt.Errorf("files: 生成恢复暂存名失败: %w", err)
	}
	s.stageName = ".upanel-restore-" + hex.EncodeToString(random[:])
	if err := unix.Mkdirat(s.parentFD, s.stageName, 0o700); err != nil {
		return nil, fmt.Errorf("files: 创建同卷恢复暂存目录失败: %w", err)
	}
	s.stageFD, err = unix.Openat(s.parentFD, s.stageName,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("files: 打开恢复暂存目录失败: %w", err)
	}
	ok = true
	return s, nil
}

func (s *RestoreSession) Target() string {
	return fmt.Sprintf("/proc/self/fd/%d", s.stageFD)
}

func restoreChildNames(parentFD int, name string) ([]string, error) {
	fd, err := unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("files: 打开目录 %s 失败: %w", name, err)
	}
	defer func() { _ = unix.Close(fd) }()
	dup, err := unix.Dup(fd)
	if err != nil {
		return nil, err
	}
	dir := os.NewFile(uintptr(dup), name)
	defer func() { _ = dir.Close() }()
	names, truncated, err := readNames(dir)
	if err != nil {
		return nil, err
	}
	if truncated {
		return nil, fmt.Errorf("%w: 目录 %s 条目过多", ErrTreeTooLarge, name)
	}
	return names, nil
}

// Inspect 不跟随任何归档符号链接；特殊文件、setuid/setgid 和多链接普通文件全部拒绝。
// restic 0.12/0.16 没有 --no-owner，因此在已打开句柄上把普通节点归一为 root:root；
// 归一发生在暂存树内，所有权未核验前绝不发布。哈希不经过可变的绝对路径。
func (s *RestoreSession) Inspect(ctx context.Context) ([]RestoreEntry, error) {
	if s == nil || s.stageFD < 0 || s.published {
		return nil, errors.New("files: 恢复暂存会话不可用")
	}
	out := make([]RestoreEntry, 0)
	if err := s.inspectDir(ctx, s.stageFD, "", &out); err != nil {
		return nil, err
	}
	// getdents 的顺序不是恢复契约；rename 前后必须比较同一组节点而不是目录枚举巧合。
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func (s *RestoreSession) inspectDir(ctx context.Context, fd int, prefix string, out *[]RestoreEntry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if depth := strings.Count(prefix, "/"); depth > maxTreeDepth {
		return fmt.Errorf("%w: 恢复目录嵌套超过 %d 层", ErrTreeTooLarge, maxTreeDepth)
	}
	// dup 与原目录 fd 共享文件偏移。每次遍历都从 0 开始，否则发布后第二次 Inspect
	// 会继承上次 getdents 的 EOF，把非空目录误读为空。
	if _, err := unix.Seek(fd, 0, 0); err != nil {
		return fmt.Errorf("files: 重置恢复目录游标失败: %w", err)
	}
	dup, err := unix.Dup(fd)
	if err != nil {
		return err
	}
	dir := os.NewFile(uintptr(dup), "restore")
	names, truncated, readErr := readNames(dir)
	_ = dir.Close()
	if readErr != nil {
		return readErr
	}
	if truncated {
		return fmt.Errorf("%w: 恢复目录条目过多", ErrTreeTooLarge)
	}
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		rel := name
		if prefix != "" {
			rel = path.Join(prefix, name)
		}
		var st unix.Stat_t
		if err := unix.Fstatat(fd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return fmt.Errorf("files: 读取恢复节点 %s 失败: %w", rel, err)
		}
		if st.Mode&0o6000 != 0 {
			return fmt.Errorf("%w: %s 含 setuid/setgid 权限", ErrPathRejected, rel)
		}
		entry := RestoreEntry{Path: rel, Mode: uint32(st.Mode) & 0o777}
		switch st.Mode & unix.S_IFMT {
		case unix.S_IFDIR:
			entry.Type = "dir"
			*out = append(*out, entry)
			child, err := unix.Openat(fd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
			if err != nil {
				return fmt.Errorf("files: 打开恢复目录 %s 失败: %w", rel, err)
			}
			if err := unix.Fchown(child, 0, 0); err != nil {
				_ = unix.Close(child)
				return fmt.Errorf("files: 无法归一恢复目录 %s 的所有权: %w", rel, err)
			}
			err = s.inspectDir(ctx, child, rel, out)
			_ = unix.Close(child)
			if err != nil {
				return err
			}
		case unix.S_IFREG:
			entry.Type = "file"
			entry.Size = st.Size
			child, err := s.m.openInRoot(fd, name, openRead)
			if err != nil {
				return err
			}
			_, _, err = s.m.checkRegular(child, rel, s.rootDev)
			if err == nil {
				err = unix.Fchown(child, 0, 0)
			}
			if err == nil {
				h := sha256.New()
				file := os.NewFile(uintptr(child), rel)
				var n int64
				n, err = io.CopyBuffer(h, file, make([]byte, 128<<10))
				_ = file.Close()
				if err == nil && n != st.Size {
					err = fmt.Errorf("%w: %s 在哈希期间改变大小", ErrVersionConflict, rel)
				}
				entry.SHA256 = hex.EncodeToString(h.Sum(nil))
			} else {
				_ = unix.Close(child)
			}
			if err != nil {
				return fmt.Errorf("files: 校验恢复文件 %s 失败: %w", rel, err)
			}
			*out = append(*out, entry)
		default:
			return fmt.Errorf("%w: %s 是符号链接或特殊文件，拒绝发布", ErrPathRejected, rel)
		}
		if len(*out) > MaxListScan {
			return fmt.Errorf("%w: 恢复节点超过 %d", ErrTreeTooLarge, MaxListScan)
		}
	}
	return nil
}

// Publish 原子落位并重新读取整棵树。已存在的目标用 RENAME_EXCHANGE 交换，
// 不存在时用 RENAME_NOREPLACE；验证失败会恢复到发布前的名字布局。
func (s *RestoreSession) Publish(ctx context.Context, expected []RestoreEntry) (RestoreReceipt, error) {
	receipt := RestoreReceipt{RootID: s.handle.entry.root.ID, Path: s.path}
	if s.published {
		return receipt, errors.New("files: 恢复已经发布")
	}
	var st unix.Stat_t
	err := unix.Fstatat(s.parentFD, s.targetName, &st, unix.AT_SYMLINK_NOFOLLOW)
	if s.targetExists {
		if err != nil || statVersion(&st) != s.targetVersion {
			return receipt, fmt.Errorf("%w: 恢复目标在预检后发生变化", ErrVersionConflict)
		}
		if err := unix.Renameat2(s.parentFD, s.stageName, s.parentFD, s.targetName, unix.RENAME_EXCHANGE); err != nil {
			return receipt, fmt.Errorf("files: 文件系统不支持安全原子覆盖，或覆盖失败: %w", err)
		}
	} else {
		if err == nil || !errors.Is(err, unix.ENOENT) {
			return receipt, fmt.Errorf("%w: 新恢复目标已经出现或无法确认", ErrAlreadyExists)
		}
		if err := renameNoReplace(s.parentFD, s.stageName, s.parentFD, s.targetName); err != nil {
			return receipt, err
		}
	}
	rollback := func() error {
		if s.targetExists {
			return unix.Renameat2(s.parentFD, s.targetName, s.parentFD, s.stageName, unix.RENAME_EXCHANGE)
		}
		return renameAt(s.parentFD, s.targetName, s.parentFD, s.stageName)
	}
	actual, verifyErr := s.Inspect(ctx)
	if verifyErr == nil && !EqualRestoreEntries(expected, actual) {
		verifyErr = fmt.Errorf("%w: 发布后的目录与发布前哈希清单不一致", ErrVersionConflict)
	}
	if verifyErr != nil {
		if err := rollback(); err != nil {
			return receipt, fmt.Errorf("files: 发布后验证失败（%v），且回滚失败（%w）；目标状态未知", verifyErr, err)
		}
		return receipt, fmt.Errorf("files: 发布后验证失败，已恢复原目标: %w", verifyErr)
	}
	s.published = true
	receipt.Entries = len(actual)
	for _, entry := range actual {
		receipt.Bytes += entry.Size
	}
	receipt.VerifiedAt = s.m.now().UTC()
	if s.targetExists {
		if err := s.m.removeTree(s.parentFD, s.stageName, 0); err != nil {
			receipt.CleanupError = fmt.Sprintf("新目录已验证并保留；旧目录 %s 删除失败: %v", s.stageName, err)
		}
	}
	return receipt, nil
}

// Abort 只清理尚未发布的暂存。发布后的目标绝不由 defer 顺手删掉。
func (s *RestoreSession) Abort(ctx context.Context) error {
	if s == nil {
		return nil
	}
	var errs []error
	if s.parentFD >= 0 && s.stageName != "" && !s.published {
		if err := s.m.removeTree(s.parentFD, s.stageName, 0); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	if s.stageFD >= 0 {
		if err := unix.Close(s.stageFD); err != nil {
			errs = append(errs, err)
		}
		s.stageFD = -1
	}
	if s.parentFD >= 0 {
		if err := unix.Close(s.parentFD); err != nil {
			errs = append(errs, err)
		}
		s.parentFD = -1
	}
	if s.handle != nil {
		if err := s.handle.Close(); err != nil {
			errs = append(errs, err)
		}
		s.handle = nil
	}
	if ctx.Err() != nil && len(errs) == 0 {
		return nil // 清理必须在取消后仍然完成，不返回取消来掩盖清理结果。
	}
	return errors.Join(errs...)
}
