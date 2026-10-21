//go:build linux

package files

// P06.2：流式暂存上传（CONTRACTS.md 第 11 节）。
//
// 上传分两步，中间隔着一次显式提交：
//
//	POST /files/uploads            → 建一个暂存（声明大小、目标、版本）
//	PUT  /files/uploads/:id/content → 字节流写进暂存，不碰目标
//	POST /jobs {file.commit_upload} → 预检 + 任务把暂存原子落位
//
// 这样拆的理由不是“看起来正规”，而是三件具体的事：
//
//  1. **目标字节只在提交那一刻变**。传输中断、磁盘满、客户端反悔，都只影响暂存，
//     目标文件一个字节都没动过。
//  2. **原子替换要求同一文件系统**。暂存放在根内的保留目录 `.upanel/staging` 里，
//     与目标同挂载，所以落位是 renameat(2)——要么是旧文件，要么是新文件，
//     没有“写了一半的配置文件”。
//  3. **不把整份内容放进内存**。字节从连接直接进暂存文件，代理的内存占用与文件大小
//     无关（2 GiB 上传也只有几十 KiB 的缓冲区）。
//
// 容量预留在并发上传之间共享检查：声明时就预留，收完就释放（那时字节已经真实占着
// 磁盘，statfs 自己会算），失败/删除/过期立刻释放。磁盘真的满了（ENOSPC）时暂存文件
// 被删除并报错——不留一个“半份文件”让别人以为传完了。

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

const (
	// uploadMetaSuffix / uploadDataSuffix 是暂存目录里的两种文件。
	uploadMetaSuffix = ".json"
	uploadDataSuffix = ".part"
	// stagingSubdir / backupSubdir 是保留目录下两个子目录的名字。
	stagingSubdir = "staging"
	backupSubdir  = "backups"
	// maxUploadMetaBytes 是元数据文件的大小上限（读的时候也照它截断）。
	maxUploadMetaBytes = 16 << 10
	// uploadReserveMargin 是容量预检的余量：预检说“够”，不代表刚好够。
	uploadReserveMargin int64 = 64 << 20
	// reservedDirMode 是保留目录的权限：只有 root 能进。
	reservedDirMode uint32 = 0o700
)

// uploadMeta 是暂存记录的持久形态（存在 <root>/.upanel/staging/<id>.json）。
//
// 为什么要落盘而不是只放内存：代理重启之后，“这个 id 还在、已经收了多少字节”
// 必须仍然说得清。用内存表的话，重启后一个 24 小时有效期的暂存就变成了
// “id 忽然不存在”，而磁盘上还留着半份数据。
type uploadMeta struct {
	UploadID        string    `json:"upload_id"`
	RootID          string    `json:"root_id"`
	Path            string    `json:"path"`
	SizeBytes       uint64    `json:"size_bytes"`
	ReceivedBytes   uint64    `json:"received_bytes"`
	State           string    `json:"state"`
	ExpectedVersion string    `json:"expected_version,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	ExpiresAt       time.Time `json:"expires_at"`
	Error           string    `json:"error,omitempty"`
	SHA256          string    `json:"sha256,omitempty"`
}

// info 把元数据转成对外视图。
func (u uploadMeta) info() UploadInfo {
	return UploadInfo{
		UploadID:        u.UploadID,
		RootID:          u.RootID,
		Path:            u.Path,
		SizeBytes:       u.SizeBytes,
		ReceivedBytes:   u.ReceivedBytes,
		State:           u.State,
		ExpectedVersion: u.ExpectedVersion,
		CreatedAt:       u.CreatedAt,
		ExpiresAt:       u.ExpiresAt,
		Error:           u.Error,
	}
}

// newUploadID 生成 up_ + 16 字节随机十六进制。
func newUploadID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("files: 生成上传 id 失败: %w", err)
	}
	return "up_" + hex.EncodeToString(b[:]), nil
}

// reservedSubdir 打开（必要时创建）根内的保留子目录。
//
// 保留目录必须由 root 拥有且不可被其他用户写入：暂存里放的是**还没提交的文件内容**，
// 如果它能被别人改写，那么“提交的是你上传的那份字节”这句话就不成立了。
func (m *Manager) reservedSubdir(handle *rootHandle, name string) (int, error) {
	if err := unix.Mkdirat(handle.fd, ReservedDirName, reservedDirMode); err != nil && !errors.Is(err, unix.EEXIST) {
		return -1, fmt.Errorf("files: 无法创建保留目录 %s: %w", ReservedDirName, err)
	}
	// 单独打开保留目录，再用它做 dirfd：这样 `.upanel` 是符号链接时会在这里被
	// RESOLVE_NO_SYMLINKS 拦下，而不是让后续的 mkdirat 顺着链接跑到根外去。
	resFD, err := m.openInRoot(handle.fd, ReservedDirName, unix.O_PATH|unix.O_DIRECTORY)
	if err != nil {
		return -1, err
	}
	defer func() { _ = unix.Close(resFD) }()
	if err := verifyReservedDir(resFD, ReservedDirName); err != nil {
		return -1, err
	}
	if err := unix.Mkdirat(resFD, name, reservedDirMode); err != nil && !errors.Is(err, unix.EEXIST) {
		return -1, fmt.Errorf("files: 无法创建 %s/%s: %w", ReservedDirName, name, err)
	}
	fd, err := unix.Openat(resFD, name, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("files: 无法打开 %s/%s: %w", ReservedDirName, name, err)
	}
	if err := verifyReservedDir(fd, ReservedDirName+"/"+name); err != nil {
		_ = unix.Close(fd)
		return -1, err
	}
	return fd, nil
}

// verifyReservedDir 核对保留目录是 root 拥有、且组/其他人没有任何权限。
func verifyReservedDir(fd int, label string) error {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return fmt.Errorf("files: 无法读取 %s 的 stat: %w", label, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return fmt.Errorf("%w: %s 不是目录", ErrPathRejected, label)
	}
	if st.Uid != 0 {
		return fmt.Errorf("%w: %s 的属主是 uid %d，面板的保留目录必须属于 root",
			ErrPathRejected, label, st.Uid)
	}
	if st.Mode&0o077 != 0 {
		return fmt.Errorf("%w: %s 的权限是 %s，面板的保留目录必须是 0700（只有 root 可访问）",
			ErrPathRejected, label, FormatMode(uint32(st.Mode)&0o777))
	}
	return nil
}

// openStagingDir 打开已存在的暂存目录；不存在时返回 ErrNotFound。
//
// 查找上传时用它：一个从没上传过的根不该因为“查不到 id”而被创建一个保留目录。
func (m *Manager) openStagingDir(handle *rootHandle) (int, error) {
	return m.openInRoot(handle.fd, ReservedDirName+"/"+stagingSubdir, unix.O_PATH|unix.O_DIRECTORY)
}

// writeAt 把整个 data 写进 dirFD 下的一个文件（创建或截断），并 fsync。
func writeAt(dirFD int, name string, data []byte, mode uint32) error {
	fd, err := unix.Openat(dirFD, name,
		unix.O_WRONLY|unix.O_CREAT|unix.O_TRUNC|unix.O_NOFOLLOW|unix.O_CLOEXEC, mode)
	if err != nil {
		return fmt.Errorf("files: 无法打开 %s: %w", name, err)
	}
	file := os.NewFile(uintptr(fd), name)
	defer func() { _ = file.Close() }()
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("files: 写入 %s 失败: %w", name, err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("files: 同步 %s 失败: %w", name, err)
	}
	return nil
}

// readAt 读取 dirFD 下的一个小文件（上限 maxUploadMetaBytes）。
func readAt(dirFD int, name string, limit int) ([]byte, error) {
	fd, err := unix.Openat(dirFD, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	defer func() { _ = file.Close() }()
	return io.ReadAll(io.LimitReader(file, int64(limit)+1))
}

// writeUploadMeta 原子地更新元数据（临时文件 + renameat）。
func writeUploadMeta(dirFD int, meta uploadMeta) error {
	body, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("files: 上传元数据无法序列化: %w", err)
	}
	tmp := meta.UploadID + uploadMetaSuffix + ".tmp"
	if err := writeAt(dirFD, tmp, body, 0o600); err != nil {
		return err
	}
	return renameAt(dirFD, tmp, dirFD, meta.UploadID+uploadMetaSuffix)
}

// readUploadMeta 读取一个暂存记录。
func readUploadMeta(dirFD int, id string) (uploadMeta, error) {
	raw, err := readAt(dirFD, id+uploadMetaSuffix, maxUploadMetaBytes)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return uploadMeta{}, ErrUploadNotFound
		}
		return uploadMeta{}, fmt.Errorf("files: 读取上传记录失败: %w", err)
	}
	if len(raw) > maxUploadMetaBytes {
		return uploadMeta{}, fmt.Errorf("files: 上传记录异常（超过 %d 字节）", maxUploadMetaBytes)
	}
	var meta uploadMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		return uploadMeta{}, fmt.Errorf("files: 上传记录不是合法 JSON: %w", err)
	}
	if meta.UploadID != id {
		// 记录里的 id 与文件名不一致：要么是别人写坏的，要么有人在构造。
		return uploadMeta{}, fmt.Errorf("files: 上传记录与 id 不一致（%q != %q）", meta.UploadID, id)
	}
	return meta, nil
}

// renameAt 是 renameat2(RENAME_NOREPLACE) 的重试用封装；内核不支持时退回 renameat。
//
// NOREPLACE 用在“目标已存在就必须失败”的场合（例如 copy）。真正要求**替换**的地方
// （提交上传、文本改写）用的是普通 renameat：那正是原子替换的语义。
func renameAt(oldDir int, oldName string, newDir int, newName string) error {
	err := unix.Renameat(oldDir, oldName, newDir, newName)
	if err == nil {
		return nil
	}
	return fmt.Errorf("files: 无法把 %s 改名为 %s: %w", oldName, newName, err)
}

// renameNoReplace 只在目标不存在时落位。
func renameNoReplace(oldDir int, oldName string, newDir int, newName string) error {
	err := unix.Renameat2(oldDir, oldName, newDir, newName, unix.RENAME_NOREPLACE)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, unix.EEXIST):
		return fmt.Errorf("%w: %s", ErrAlreadyExists, newName)
	case errors.Is(err, unix.EINVAL), errors.Is(err, unix.ENOSYS):
		// 老内核/老文件系统不支持 RENAME_NOREPLACE：用一次 fstatat 判断再 rename。
		// 这不是原子的，所以只作为兜底，并在日志里说明——真正的保证是调用方
		// 在预检与执行前各查一次目标存在性。
		if _, statErr := unix.Openat(newDir, newName, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0); statErr == nil {
			return fmt.Errorf("%w: %s", ErrAlreadyExists, newName)
		}
		return renameAt(oldDir, oldName, newDir, newName)
	default:
		return fmt.Errorf("files: 无法把 %s 落位为 %s: %w", oldName, newName, err)
	}
}

// reserveSpace 做容量预检并登记预留。
//
// 读不到容量（没接容量读取器、statfs 失败、网络挂载超时）时**不阻断**上传：那会让
// “容量读不出来”变成“完全不能上传”。真正的兜底是写入时的 ENOSPC——那条路径会删掉
// 暂存并报错。
//
// 但**预留本身照记**：预留计数器记的是“已声明、还没落盘的字节”，这件事不需要知道
// 磁盘还剩多少。容量读不到就跳过比较、继续记账；否则 ReservedBytes 会在没有容量
// 读取器的机器上永远报 0，并发上传的可见性也就跟着一起消失了。
func (m *Manager) reserveSpace(ctx context.Context, entry *rootEntry, want int64) error {
	if want <= 0 {
		return nil
	}
	if m.caps != nil {
		capacity, reason := m.capacityFor(ctx, entry)
		if capacity == nil {
			m.logger.Warn("读不到文件根容量，跳过容量预检（写入时仍会检查 ENOSPC）",
				"root_id", entry.root.ID, "reason", reason)
		} else {
			m.reservedMu.Lock()
			already := m.reserved[entry.root.ID]
			m.reservedMu.Unlock()

			available := int64(capacity.AvailableBytes)
			if available < already+want+uploadReserveMargin {
				return fmt.Errorf("%w: %s 需要 %d 字节，可用 %d 字节（其中 %d 字节已被在途上传预留，另留 %d 字节余量）",
					ErrNoSpace, entry.root.ID, want, available, already, uploadReserveMargin)
			}
		}
	}
	m.reservedMu.Lock()
	m.reserved[entry.root.ID] += want
	m.reservedMu.Unlock()
	return nil
}

// releaseSpace 归还预留。重复归还是无害的（会截到 0 为止）。
func (m *Manager) releaseSpace(rootID string, n int64) {
	if n <= 0 {
		return
	}
	m.reservedMu.Lock()
	defer m.reservedMu.Unlock()
	left := m.reserved[rootID] - n
	if left < 0 {
		left = 0
	}
	m.reserved[rootID] = left
}

// ReservedBytes 返回某个根当前被在途上传预留的字节数（供诊断与测试）。
func (m *Manager) ReservedBytes(rootID string) int64 {
	m.reservedMu.Lock()
	defer m.reservedMu.Unlock()
	return m.reserved[rootID]
}

// MaxUploadBytes 返回配置的单次上传上限。
func (m *Manager) MaxUploadBytes() int64 { return m.maxUpload }

// checkWritableTarget 是“要往这个路径写”的前置检查：根可写、父目录是目录、目标不是
// 目录/符号链接/特殊文件。
//
// 它只读，不创建任何东西：预检要能**多次**调用而不产生副作用。
func (m *Manager) checkWritableTarget(handle *rootHandle, rel string) (PathStat, error) {
	if !handle.entry.root.Writable {
		return PathStat{}, fmt.Errorf("%w: %s", ErrNotWritable, handle.entry.root.ID)
	}
	parent := ParentPath(rel)
	name := BasePath(rel)
	parentFD, err := m.openInRoot(handle.fd, parent, unix.O_PATH|unix.O_DIRECTORY)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return PathStat{}, fmt.Errorf("%w: 父目录 %q 不存在", ErrNotFound, parent)
		}
		if errors.Is(err, ErrNotDirectory) {
			return PathStat{}, fmt.Errorf("%w: %q 不是目录", ErrNotDirectory, parent)
		}
		return PathStat{}, err
	}
	defer func() { _ = unix.Close(parentFD) }()

	stat, err := m.statAt(handle, parentFD, rel, name)
	if err != nil {
		return PathStat{}, err
	}
	switch {
	case !stat.Exists:
	case stat.Type == TypeDir:
		return PathStat{}, fmt.Errorf("%w: %s 是目录", ErrIsDirectory, rel)
	case stat.Type != TypeFile:
		return PathStat{}, fmt.Errorf("%w: %s 是%s，面板不替换非普通文件",
			ErrPathRejected, rel, stat.Type)
	}
	return stat, nil
}

// statAt 用 fstatat 读一个目录项（不跟随符号链接）。
func (m *Manager) statAt(handle *rootHandle, parentFD int, rel, name string) (PathStat, error) {
	stat := PathStat{RootID: handle.entry.root.ID, Path: rel}
	var st unix.Stat_t
	err := unix.Fstatat(parentFD, name, &st, unix.AT_SYMLINK_NOFOLLOW)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return stat, nil
		}
		return stat, fmt.Errorf("files: 无法读取 %s 的信息: %w", rel, err)
	}
	stat.Exists = true
	stat.Type = entryType(st.Mode)
	stat.SizeBytes = uint64(st.Size)
	stat.Mode = modeString(st.Mode)
	stat.Owner = Owner{UID: st.Uid, GID: st.Gid}
	stat.MTime = time.Unix(st.Mtim.Sec, st.Mtim.Nsec).UTC()
	stat.Version = statVersion(&st)
	return stat, nil
}

// CreateUpload 创建一个暂存上传（POST /files/uploads）。
func (m *Manager) CreateUpload(ctx context.Context, params CreateUploadParams) (UploadInfo, error) {
	if err := params.Validate(); err != nil {
		return UploadInfo{}, err
	}
	if err := ctx.Err(); err != nil {
		return UploadInfo{}, err
	}
	if int64(params.SizeBytes) > m.maxUpload {
		return UploadInfo{}, fmt.Errorf("%w: 声明大小 %d 字节超过单次上传上限 %d 字节（配置项 agent.files.max_upload_bytes）",
			ErrTooLarge, params.SizeBytes, m.maxUpload)
	}
	handle, err := m.openRoot(params.RootID)
	if err != nil {
		return UploadInfo{}, err
	}
	defer func() { _ = handle.Close() }()

	// 目标此刻的形状要在建暂存时就说清楚：往一个目录或符号链接上“上传”不会有结果，
	// 让它在第一步就失败，比让人传完 2 GiB 再报错好。
	if _, err := m.checkWritableTarget(handle, params.Path); err != nil {
		return UploadInfo{}, err
	}
	// 版本在这里做一次复核：声明的 expected_version 如果已经对不上，
	// 这次上传注定在提交时被拒绝，不如现在就不接收字节。
	if params.ExpectedVersion != "" {
		stat, err := m.Stat(ctx, params.RootID, params.Path)
		if err != nil {
			return UploadInfo{}, err
		}
		if !stat.Exists {
			return UploadInfo{}, fmt.Errorf("%w: 目标 %s 不存在，不能按版本覆盖", ErrVersionConflict, params.Path)
		}
		if stat.Version != params.ExpectedVersion {
			return UploadInfo{}, fmt.Errorf("%w: %s 已被修改", ErrVersionConflict, params.Path)
		}
	}
	if err := m.reserveSpace(ctx, handle.entry, int64(params.SizeBytes)); err != nil {
		return UploadInfo{}, err
	}
	reserved := int64(params.SizeBytes)

	id, err := newUploadID()
	if err != nil {
		m.releaseSpace(params.RootID, reserved)
		return UploadInfo{}, err
	}
	dirFD, err := m.reservedSubdir(handle, stagingSubdir)
	if err != nil {
		m.releaseSpace(params.RootID, reserved)
		return UploadInfo{}, err
	}
	defer func() { _ = unix.Close(dirFD) }()

	now := m.now().UTC()
	meta := uploadMeta{
		UploadID:        id,
		RootID:          params.RootID,
		Path:            params.Path,
		SizeBytes:       params.SizeBytes,
		State:           UploadStateOpen,
		ExpectedVersion: params.ExpectedVersion,
		CreatedAt:       now,
		ExpiresAt:       now.Add(UploadTTL),
	}
	if err := writeUploadMeta(dirFD, meta); err != nil {
		m.releaseSpace(params.RootID, reserved)
		return UploadInfo{}, err
	}
	m.logger.Info("已创建上传暂存", "upload_id", id, "root_id", params.RootID,
		"path", params.Path, "size_bytes", params.SizeBytes, "expires_at", meta.ExpiresAt)
	return meta.info(), nil
}

// openUpload 找到一张暂存记录并返回它所在的根句柄与暂存目录 fd。
//
// 调用方负责关闭 handle 与 dirFD。查找**只读**：不创建任何目录，所以查一个
// 不存在的 id 不会在根里留下痕迹。
func (m *Manager) openUpload(id string) (*rootHandle, int, uploadMeta, error) {
	if !ValidUploadID(id) {
		return nil, -1, uploadMeta{}, fmt.Errorf("%w: upload_id 形状不合法", ErrInvalidParams)
	}
	for _, rootID := range m.order {
		handle, err := m.openRoot(rootID)
		if err != nil {
			continue
		}
		dirFD, err := m.openStagingDir(handle)
		if err != nil {
			_ = handle.Close()
			continue
		}
		meta, err := readUploadMeta(dirFD, id)
		if err != nil {
			_ = unix.Close(dirFD)
			_ = handle.Close()
			if errors.Is(err, ErrUploadNotFound) {
				continue
			}
			return nil, -1, uploadMeta{}, err
		}
		if err := verifyReservedDir(dirFD, ReservedDirName+"/"+stagingSubdir); err != nil {
			_ = unix.Close(dirFD)
			_ = handle.Close()
			return nil, -1, uploadMeta{}, err
		}
		return handle, dirFD, meta, nil
	}
	return nil, -1, uploadMeta{}, fmt.Errorf("%w: %s", ErrUploadNotFound, id)
}

// WriteUploadContent 把字节流写进暂存（PUT /files/uploads/:id/content）。
//
// 一次 PUT 传完，不支持断点续传：暂存只有在 state=open 时才能写，写完之后状态变成
// received（或 failed）。这样“已经收了一半”这件事不会变成一个可以被两个请求同时
// 续写的文件。
func (m *Manager) WriteUploadContent(ctx context.Context, id string, body io.Reader) (UploadInfo, error) {
	handle, dirFD, meta, err := m.openUpload(id)
	if err != nil {
		return UploadInfo{}, err
	}
	defer func() { _ = handle.Close() }()
	defer func() { _ = unix.Close(dirFD) }()

	now := m.now().UTC()
	if meta.State != UploadStateOpen {
		return meta.info(), fmt.Errorf("%w: %s 当前状态是 %s，不能再写内容", ErrUploadState, id, meta.State)
	}
	if !now.Before(meta.ExpiresAt) {
		_, _ = m.dropUpload(dirFD, meta)
		return UploadInfo{}, fmt.Errorf("%w: %s 的暂存已过期", ErrUploadNotFound, id)
	}
	if !handle.entry.root.Writable {
		return meta.info(), fmt.Errorf("%w: %s", ErrNotWritable, meta.RootID)
	}

	partName := id + uploadDataSuffix
	fd, err := unix.Openat(dirFD, partName,
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return meta.info(), fmt.Errorf("%w: 暂存数据已经存在（可能有一个请求正在写）：%v", ErrUploadState, err)
	}
	file := os.NewFile(uintptr(fd), partName)

	hasher := sha256.New()
	// 多读一个字节：正好读满 size 时无法区分“传输完整”和“传输完整但还多”，
	// 所以上限是 size+1，读到第 size+1 个字节就说明超了。
	limit := int64(meta.SizeBytes) + 1
	written, copyErr := io.CopyBuffer(io.MultiWriter(file, hasher), io.LimitReader(body, limit), make([]byte, 64<<10))
	syncErr := file.Sync()
	closeErr := file.Close()

	fail := func(reason string, noSpace bool) (UploadInfo, error) {
		return m.failUpload(dirFD, meta, reason, noSpace)
	}
	switch {
	case copyErr != nil && errors.Is(copyErr, unix.ENOSPC):
		return fail("磁盘空间不足，暂存已删除", true)
	case copyErr != nil:
		// 客户端断开、读超时等等：这份数据不完整，删掉，让人重新传。
		// 保留半份暂存没有任何用处——契约不支持续传。
		return fail(fmt.Sprintf("传输中断（已写入 %d 字节）：%v", written, copyErr), false)
	case syncErr != nil:
		return fail(fmt.Sprintf("落盘失败：%v", syncErr), false)
	case closeErr != nil:
		return fail(fmt.Sprintf("关闭暂存失败：%v", closeErr), false)
	case written > int64(meta.SizeBytes):
		return fail(fmt.Sprintf("实际数据超过声明大小 %d 字节", meta.SizeBytes), false)
	case written != int64(meta.SizeBytes):
		return fail(fmt.Sprintf("实际数据 %d 字节与声明大小 %d 字节不符", written, meta.SizeBytes), false)
	}

	meta.State = UploadStateReceived
	meta.ReceivedBytes = uint64(written)
	meta.SHA256 = hex.EncodeToString(hasher.Sum(nil))
	meta.Error = ""
	if err := writeUploadMeta(dirFD, meta); err != nil {
		return fail(fmt.Sprintf("写入上传记录失败：%v", err), false)
	}
	// 字节已经真实占着磁盘了，预留到此结束（再算一遍会让并发的下一个上传被无谓拒绝）。
	m.releaseSpace(meta.RootID, int64(meta.SizeBytes))
	m.logger.Info("上传内容已收完", "upload_id", id, "root_id", meta.RootID,
		"path", meta.Path, "bytes", written, "sha256", meta.SHA256)
	return meta.info(), nil
}

// failUpload 把一次上传标成失败并删掉暂存数据。
func (m *Manager) failUpload(dirFD int, meta uploadMeta, reason string, noSpace bool) (UploadInfo, error) {
	_ = unix.Unlinkat(dirFD, meta.UploadID+uploadDataSuffix, 0)
	_ = unix.Unlinkat(dirFD, meta.UploadID+uploadMetaSuffix+".tmp", 0)
	meta.State = UploadStateFailed
	meta.Error = reason
	meta.ReceivedBytes = 0
	if err := writeUploadMeta(dirFD, meta); err != nil {
		m.logger.Error("写入失败上传记录失败", "upload_id", meta.UploadID, "error", err)
	}
	m.releaseSpace(meta.RootID, int64(meta.SizeBytes))
	m.logger.Warn("上传失败，暂存已删除", "upload_id", meta.UploadID,
		"root_id", meta.RootID, "path", meta.Path, "reason", reason)
	// 磁盘满是调用方可以自己处理的条件（清空间再传），传输问题是“这次上传作废”。
	// 两者用不同的错误区分，前端才能给出不同的下一步。
	if noSpace {
		return meta.info(), fmt.Errorf("%w: %s", ErrNoSpace, reason)
	}
	return meta.info(), fmt.Errorf("%w: %s", ErrUploadState, reason)
}

// dropUpload 删除一张暂存（数据 + 记录）。
//
// 删除是幂等的：文件已经不在也算成功（重复清理、并发清理都走这条路）。
func (m *Manager) dropUpload(dirFD int, meta uploadMeta) (int64, error) {
	var freed int64
	if err := unix.Unlinkat(dirFD, meta.UploadID+uploadDataSuffix, 0); err == nil {
		if meta.ReceivedBytes > 0 {
			freed = int64(meta.ReceivedBytes)
		} else {
			freed = int64(meta.SizeBytes)
		}
	}
	// 预留的归还在**这里**做，而不是散在三个调用方：暂存记录一旦删掉，这条上传就不可能
	// 再落位，它声明过的字节必须立刻还给并发上传。收完（received）的那一档早就在收完
	// 时就释放过了，再还一次会去扣别人的额度。
	if meta.State != UploadStateReceived {
		m.releaseSpace(meta.RootID, int64(meta.SizeBytes))
	}
	_ = unix.Unlinkat(dirFD, meta.UploadID+uploadMetaSuffix, 0)
	_ = unix.Unlinkat(dirFD, meta.UploadID+uploadMetaSuffix+".tmp", 0)
	return freed, nil
}

// GetUpload 返回一个暂存的状态。
func (m *Manager) GetUpload(ctx context.Context, id string) (UploadInfo, error) {
	if err := ctx.Err(); err != nil {
		return UploadInfo{}, err
	}
	handle, dirFD, meta, err := m.openUpload(id)
	if err != nil {
		return UploadInfo{}, err
	}
	defer func() { _ = handle.Close() }()
	defer func() { _ = unix.Close(dirFD) }()
	return meta.info(), nil
}

// DeleteUpload 删除一个暂存（DELETE /files/uploads/:id）。目标文件不受影响。
func (m *Manager) DeleteUpload(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	handle, dirFD, meta, err := m.openUpload(id)
	if err != nil {
		return err
	}
	defer func() { _ = handle.Close() }()
	defer func() { _ = unix.Close(dirFD) }()
	if meta.State == UploadStateReceived {
		// 收完但没提交：占的是真实磁盘，删除就是释放它。
		m.logger.Info("删除已收完但未提交的暂存", "upload_id", id, "root_id", meta.RootID, "path", meta.Path)
	}
	// 预留的归还在 dropUpload 里统一做（那条记录一删，这次声明就不存在了）。
	if _, err := m.dropUpload(dirFD, meta); err != nil {
		return err
	}
	m.logger.Info("已删除上传暂存", "upload_id", id, "root_id", meta.RootID, "path", meta.Path)
	return nil
}

// SweepStaging 清理过期暂存，返回清理的条数。
//
// 24 小时的期限是契约（CONTRACTS.md 第 11 节）：一次没提交完的上传不该永远占着磁盘。
// 已经 received 但没提交的也一起清：提交必须发生在 24 小时内，过期的暂存不能等到
// 某天被一次提交唤醒。
func (m *Manager) SweepStaging(ctx context.Context, now time.Time) (int, error) {
	removed := 0
	var firstErr error
	for _, rootID := range m.order {
		handle, err := m.openRoot(rootID)
		if err != nil {
			continue
		}
		// 打开暂存目录必须用“真能读”的访问模式（O_RDONLY|O_DIRECTORY）：O_PATH 的
		// fd 上 getdents64 返回 EBADF，清理会永远扫到 0 条（P06.1 的 Q136 就是这件事）。
		dirFD, err := m.openInRoot(handle.fd, ReservedDirName+"/"+stagingSubdir,
			openRead|unix.O_DIRECTORY)
		if err != nil {
			_ = handle.Close()
			continue
		}
		dir := os.NewFile(uintptr(dirFD), "staging")
		names, _, err := readNames(dir)
		if err != nil {
			_ = dir.Close()
			_ = handle.Close()
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, name := range names {
			if len(name) <= len(uploadMetaSuffix) || name[len(name)-len(uploadMetaSuffix):] != uploadMetaSuffix {
				continue
			}
			id := name[:len(name)-len(uploadMetaSuffix)]
			if !ValidUploadID(id) {
				continue
			}
			meta, err := readUploadMeta(dirFD, id)
			if err != nil {
				continue
			}
			if now.Before(meta.ExpiresAt) {
				continue
			}
			if _, err := m.dropUpload(dirFD, meta); err != nil {
				continue
			}
			removed++
			m.logger.Info("清理过期上传暂存", "upload_id", id, "root_id", rootID,
				"path", meta.Path, "state", meta.State, "expires_at", meta.ExpiresAt)
		}
		_ = dir.Close()
		_ = handle.Close()
	}
	return removed, firstErr
}
