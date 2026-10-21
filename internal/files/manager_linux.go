//go:build linux

package files

// P06.1：受管理文件根的句柄级实现（CONTRACTS.md 第 11 节）。
//
// 这个文件里没有一处 `strings.HasPrefix(path, root)`。边界靠三样东西：
//
//  1. 根句柄：配置里的绝对路径只用于在**登记时**打开一次根目录，之后每次请求都重新
//     打开并核对 (dev, ino) 与挂载身份。根被换掉（换挂载、删目录重建、指向别处的
//     符号链接）时，请求得到“根不可用”，而不是悄悄读到另一个目录。
//
//  2. openat2 的 RESOLVE_*：所有根内路径都用 RESOLVE_BENEATH（不允许离开 dirfd 之下，
//     包括通过绝对路径、`..` 与符号链接逃逸）、RESOLVE_NO_MAGICLINKS（拒绝
//     /proc/*/fd 这类 magic link）、RESOLVE_NO_XDEV（拒绝跨挂载点，包括 bind mount）
//     与 RESOLVE_NO_SYMLINKS（一律不跟随符号链接）解析。解析与打开是**同一个系统调用**，
//     中间没有“先检查再打开”的时间窗。
//
//  3. 句柄上的复核：打开之后用 fstat 在**这个句柄**上核对普通文件、nlink==1、
//     设备与根一致、大小上限，并从同一个 stat 生成 version。查询返回的 version
//     描述的就是这次真正交给调用方的那个文件。
//
// 任何一层单独使用都不够：只有第 1 层挡不住 `..`；只有第 2 层在旧内核上不存在；
// 只有第 3 层是“打开之后才发现不对”。三层都在，才有“不能逃逸”这句话。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// Root 是一个已登记的文件根（来自配置，不来自请求）。
type Root struct {
	ID       string
	Name     string
	Path     string
	Writable bool
}

// CapacityReader 读取根所在文件系统的容量。生产实现是存储模块的容量读取器
// （网络挂载走隔离子进程，见 internal/storage/capacity.go）。
type CapacityReader interface {
	Capacity(ctx context.Context, mountPath, fsType string) (*Capacity, error)
}

// Options 是 Manager 的构造参数。零值可用。
type Options struct {
	// Capacity 为 nil 时 roots 里的 capacity 恒为 null，并给出原因。
	Capacity CapacityReader
	// Logger 的默认值是 slog.Default()。
	Logger *slog.Logger
	// Now 可替换时钟；nil 用 time.Now。
	Now func() time.Time
	// MountInfoPath 是挂载表路径；空用 /proc/self/mountinfo。测试指向固定样本。
	MountInfoPath string
}

// Root 的登记状态。
type rootEntry struct {
	root     Root
	identity rootIdentity
	// err 非空表示这个根登记时就没通过核对（目录不存在、不是目录、内核不支持）。
	// 它不会让代理启动失败：一个挂载还没到位的根不该拖垮整台服务器的面板。
	err error
}

// rootIdentity 是根在登记时的身份：它在哪个挂载上、是什么文件系统、是哪个 inode。
type rootIdentity struct {
	mountPoint string
	fsType     string
	dev        uint64
	ino        uint64
}

// Manager 持有已登记的根，并在它们的句柄上完成所有读取。
type Manager struct {
	order  []string
	roots  map[string]*rootEntry
	caps   CapacityReader
	logger *slog.Logger
	now    func() time.Time
	mounts string
	// unsupported 非空表示这台机器的内核没有 openat2（< 5.6）。
	// 此时**所有**读取都以 ErrUnsupported 失败，而不是退回到字符串比较。
	unsupported error
}

// New 登记文件根并核对它们的身份。
//
// 单个根核对失败不会返回错误：错误记在该根的 Available/UnavailableReason 上。
// 只有“连登记表都构造不出来”这种编程错误才返回 error。
func New(roots []Root, opts Options) (*Manager, error) {
	m := &Manager{
		roots:  make(map[string]*rootEntry, len(roots)),
		caps:   opts.Capacity,
		logger: opts.Logger,
		now:    opts.Now,
		mounts: opts.MountInfoPath,
	}
	if m.logger == nil {
		m.logger = slog.Default()
	}
	if m.now == nil {
		m.now = time.Now
	}
	if m.mounts == "" {
		m.mounts = "/proc/self/mountinfo"
	}
	for _, r := range roots {
		if r.ID == "" {
			return nil, fmt.Errorf("files: 文件根缺少 id（path=%q）", r.Path)
		}
		if _, dup := m.roots[r.ID]; dup {
			return nil, fmt.Errorf("files: 文件根 id 重复 %q", r.ID)
		}
		if !path.IsAbs(r.Path) || path.Clean(r.Path) != r.Path || r.Path == "/" {
			return nil, fmt.Errorf("files: 文件根 %q 的路径 %q 不是可用的绝对目录路径", r.ID, r.Path)
		}
		entry := &rootEntry{root: r}
		identity, err := m.probeRoot(r.Path)
		if err != nil {
			entry.err = err
			m.logger.Warn("文件根登记失败，它会在 files.roots 里显示为不可用",
				"root_id", r.ID, "path", r.Path, "error", err)
		} else {
			entry.identity = identity
		}
		m.roots[r.ID] = entry
		m.order = append(m.order, r.ID)
	}
	return m, nil
}

// probeRoot 打开根并记录它的身份。
func (m *Manager) probeRoot(rootPath string) (rootIdentity, error) {
	fd, err := m.openPath(rootPath)
	if err != nil {
		return rootIdentity{}, err
	}
	defer func() { _ = unix.Close(fd) }()

	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return rootIdentity{}, fmt.Errorf("files: 无法读取根 %q 的 stat: %w", rootPath, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return rootIdentity{}, fmt.Errorf("files: 根 %q 不是目录", rootPath)
	}
	identity := rootIdentity{dev: uint64(st.Dev), ino: uint64(st.Ino)}
	mountPoint, fsType, err := m.mountOf(rootPath)
	if err != nil {
		// 挂载表读不到时仍然可以工作：边界靠 RESOLVE_* 与 dev 核对，挂载信息只是
		// 给界面和容量用的。但要说清楚“这次没读到”。
		m.logger.Warn("读不到挂载表，文件根的挂载信息留空", "path", rootPath, "error", err)
	} else {
		identity.mountPoint = mountPoint
		identity.fsType = fsType
	}
	return identity, nil
}

// openPath 打开配置里的绝对路径本身。
//
// 与根内路径不同，这里**允许**路径上的符号链接：运维把 /srv/docs 指向 /mnt/data
// 是合理配置。禁止的是 magic link（PROC_SUPER_MAGIC 的 /proc/*/fd），
// 因为它会让“配置里的路径”与实际打开的东西之间出现一次不可见的重定向。
func (m *Manager) openPath(absPath string) (int, error) {
	if m.unsupported != nil {
		return -1, m.unsupported
	}
	how := unix.OpenHow{
		Flags:   uint64(unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC),
		Resolve: unix.RESOLVE_NO_MAGICLINKS,
	}
	fd, err := unix.Openat2(unix.AT_FDCWD, absPath, &how)
	if err != nil {
		if errors.Is(err, unix.ENOSYS) {
			m.unsupported = fmt.Errorf("%w: 内核不支持 openat2（需要 5.6 及以上）", ErrUnsupported)
			return -1, m.unsupported
		}
		return -1, err
	}
	return fd, nil
}

// resolveFlags 是根内一切路径解析必须带的策略。见文件头的注释。
const resolveFlags = unix.RESOLVE_BENEATH | unix.RESOLVE_NO_MAGICLINKS |
	unix.RESOLVE_NO_XDEV | unix.RESOLVE_NO_SYMLINKS

// openRead 是“打开一个文件来读它的内容”的访问模式。
//
// 必须带 O_NONBLOCK：根里完全可能有 FIFO。open("pipe", O_RDONLY) 会**阻塞**
// 到有写端打开为止——那会把代理的一个请求处理器永久挂住（验收脚本里的
// `fifo` 用例就是照这个情形造的）。O_NONBLOCK 让 FIFO 立刻打开成功，随后
// checkRegular 的 fstat 发现它不是普通文件，回 400。
// 对普通文件 O_NONBLOCK 在 read(2) 上没有作用（内核忽略它），所以不必摘掉。
//
// 顺带解决设备节点：打开 /dev 里某些字符设备在 O_RDONLY 下也可能卡住。
const openRead = unix.O_RDONLY | unix.O_NONBLOCK

// openInRoot 在根句柄上解析一个相对路径。
//
// flags 必须显式给出访问模式（unix.O_PATH 拿句柄，或 openRead 读取）：
// O_RDONLY 的值是 0，因此不能靠“零值表示默认用 O_PATH”来区分——那样读取会被
// 悄悄降级成 O_PATH，然后 read(2) 报 EBADF。
// 无论如何都带 O_NOFOLLOW：即使 RESOLVE_NO_SYMLINKS 由于内核差异被忽略，
// 最后一段的符号链接也不会被跟随。
func (m *Manager) openInRoot(rootFD int, rel string, flags int) (int, error) {
	if m.unsupported != nil {
		return -1, m.unsupported
	}
	if rel == "" {
		rel = "."
	}
	how := unix.OpenHow{
		Flags:   uint64(flags | unix.O_CLOEXEC | unix.O_NOFOLLOW),
		Resolve: resolveFlags,
	}
	// openat2 在 RESOLVE_BENEATH 下遇到并发 rename 会返回 EAGAIN：重试几次，
	// 之后如实报错，而不是把一次竞争说成“路径越界”。
	for attempt := 0; attempt < 3; attempt++ {
		fd, err := unix.Openat2(rootFD, rel, &how)
		if err == nil {
			return fd, nil
		}
		switch {
		case errors.Is(err, unix.EAGAIN):
			continue
		case errors.Is(err, unix.ENOSYS):
			m.unsupported = fmt.Errorf("%w: 内核不支持 openat2（需要 5.6 及以上）", ErrUnsupported)
			return -1, m.unsupported
		case errors.Is(err, unix.ENOENT):
			return -1, fmt.Errorf("%w: %s", ErrNotFound, rel)
		case errors.Is(err, unix.EXDEV):
			return -1, fmt.Errorf("%w: %s 跨出了文件根（符号链接指向根外、或根内还有未登记的子挂载）", ErrPathRejected, rel)
		case errors.Is(err, unix.ELOOP):
			return -1, fmt.Errorf("%w: %s 是符号链接，面板不跟随符号链接", ErrPathRejected, rel)
		case errors.Is(err, unix.EACCES):
			return -1, fmt.Errorf("%w: 没有权限读取 %s", ErrPathRejected, rel)
		case errors.Is(err, unix.ENOTDIR):
			// ENOTDIR 有两个来源，错误码必须分清：
			//   - 路径里的某个中间段不是目录（"hostname/x"）→ ErrNotDirectory（400）；
			//   - 最后一段是符号链接，被 O_DIRECTORY|O_NOFOLLOW 撞成了 ENOTDIR
			//     → 那是“面板不跟随符号链接”，属于越界拒绝。
			// 用一次 O_PATH 重试把两者问清楚。这条重试不授予任何访问（句柄立刻关掉），
			// 所以它不是“先检查再打开”的窗口：判定的仍然是内核的解析结果。
			if flags&unix.O_DIRECTORY != 0 {
				if code := m.classifyNotDir(rootFD, rel); code != nil {
					return -1, code
				}
			}
			return -1, fmt.Errorf("%w: %s 的路径段不是目录", ErrNotDirectory, rel)
		default:
			return -1, fmt.Errorf("files: 打开 %s 失败: %w", rel, err)
		}
	}
	return -1, fmt.Errorf("%w: %s 的解析在并发修改下反复失败，请重试", ErrPathRejected, rel)
}

// classifyNotDir 在 ENOTDIR 之后问一句“到底为什么不是目录”。
//
// 返回 nil 表示“确实不是目录”（交给调用方回 ErrNotDirectory），返回错误表示
// 这其实是一次越界拒绝（符号链接、magic link、跨挂载）。
//
// 只做一次 O_PATH 解析并立刻关闭：它不读内容、不跟随符号链接，也不影响后续判决。
func (m *Manager) classifyNotDir(rootFD int, rel string) error {
	how := unix.OpenHow{
		Flags:   uint64(unix.O_PATH | unix.O_CLOEXEC | unix.O_NOFOLLOW),
		Resolve: resolveFlags,
	}
	fd, err := unix.Openat2(rootFD, rel, &how)
	if err == nil {
		// O_PATH|O_NOFOLLOW 打开一个符号链接是**成功**的：拿到的是链接本身，
		// 不是它指向的东西。所以这里必须看一眼 fstat：S_IFLNK 就是“这是符号链接”。
		defer func() { _ = unix.Close(fd) }()
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err == nil && st.Mode&unix.S_IFMT == unix.S_IFLNK {
			return fmt.Errorf("%w: %s 是符号链接，面板不跟随符号链接", ErrPathRejected, rel)
		}
		return nil
	}
	switch {
	case errors.Is(err, unix.ELOOP):
		return fmt.Errorf("%w: %s 是符号链接（或含有符号链接段），面板不跟随符号链接", ErrPathRejected, rel)
	case errors.Is(err, unix.EXDEV):
		return fmt.Errorf("%w: %s 跨出了文件根（符号链接指向根外、或根内还有未登记的子挂载）", ErrPathRejected, rel)
	case errors.Is(err, unix.ENOENT):
		return fmt.Errorf("%w: %s", ErrNotFound, rel)
	default:
		return nil
	}
}

// rootHandle 是一次请求里使用的根句柄。Close 之后所有 fd 都归还。
type rootHandle struct {
	entry *rootEntry
	fd    int
}

func (h *rootHandle) Close() error {
	if h == nil || h.fd < 0 {
		return nil
	}
	err := unix.Close(h.fd)
	h.fd = -1
	return err
}

// openRoot 打开一个已登记的根，并核对它还是登记时那个根。
func (m *Manager) openRoot(rootID string) (*rootHandle, error) {
	entry, ok := m.roots[rootID]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownRoot, rootID)
	}
	if m.unsupported != nil {
		return nil, m.unsupported
	}
	if entry.err != nil {
		return nil, fmt.Errorf("%w: %q 登记时就不通过：%v", ErrRootUnavailable, rootID, entry.err)
	}
	fd, err := m.openPath(entry.root.Path)
	if err != nil {
		return nil, fmt.Errorf("%w: 无法打开根 %q: %v", ErrRootUnavailable, rootID, err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("%w: 无法读取根 %q 的 stat: %v", ErrRootUnavailable, rootID, err)
	}
	if uint64(st.Dev) != entry.identity.dev || uint64(st.Ino) != entry.identity.ino {
		_ = unix.Close(fd)
		// 根被换掉了：删目录重建、换挂载、或者配置里的路径现在指向别处。
		// 这里拒绝，而不是按“新出现的目录”继续服务——那等于让一次本地文件操作
		// 悄悄改掉面板可以管理的范围。
		return nil, fmt.Errorf("%w: 根 %q 的身份与登记时不一致（dev/inode 变了），"+
			"请确认挂载后重新载入代理配置", ErrRootUnavailable, rootID)
	}
	if mountPoint, _, err := m.mountOf(entry.root.Path); err == nil && entry.identity.mountPoint != "" &&
		mountPoint != entry.identity.mountPoint {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("%w: 根 %q 的挂载点从 %s 变成了 %s，请重新载入代理配置",
			ErrRootUnavailable, rootID, entry.identity.mountPoint, mountPoint)
	}
	return &rootHandle{entry: entry, fd: fd}, nil
}

// Roots 返回已登记的根（含可用性与容量）。
//
// 它不返回错误：一个根不可用是**这个根的事实**，不是整次调用的失败。
// 让整个列表因为一个根掉了而报错，会让界面看不到其余可用的根。
func (m *Manager) Roots(ctx context.Context) RootsResult {
	out := RootsResult{Roots: make([]RootInfo, 0, len(m.order))}
	for _, id := range m.order {
		entry := m.roots[id]
		info := RootInfo{
			RootID:     entry.root.ID,
			Name:       entry.root.Name,
			Path:       entry.root.Path,
			Writable:   entry.root.Writable,
			MountPoint: entry.identity.mountPoint,
			FSType:     entry.identity.fsType,
		}
		switch {
		case m.unsupported != nil:
			info.UnavailableReason = m.unsupported.Error()
		case entry.err != nil:
			info.UnavailableReason = entry.err.Error()
		default:
			// 每次列根都重新打开一次：容量与可用性必须是此刻的事实。
			handle, err := m.openRoot(id)
			if err != nil {
				info.UnavailableReason = err.Error()
				break
			}
			info.Available = true
			info.Capacity, info.CapacityError = m.capacityFor(ctx, handle.entry)
			_ = handle.Close()
		}
		out.Roots = append(out.Roots, info)
	}
	return out
}

// capacityFor 读取根所在挂载的容量。读不出来时返回 nil 与原因，绝不返回 0。
func (m *Manager) capacityFor(ctx context.Context, entry *rootEntry) (*Capacity, string) {
	if m.caps == nil {
		return nil, "未接入容量读取"
	}
	if entry.identity.mountPoint == "" {
		return nil, "读不到挂载信息，无法判断容量查询通道"
	}
	capv, err := m.caps.Capacity(ctx, entry.identity.mountPoint, entry.identity.fsType)
	if err != nil {
		return nil, err.Error()
	}
	if capv == nil {
		return nil, "容量读取没有返回结果"
	}
	return capv, ""
}

// verstat 从一次 stat 生成 version。
//
// 四个字段都要：dev+ino 定位 inode，size 与纳秒 mtime 定位内容。
// 只用 mtime 会在“同一纳秒内写两次”时失效；只用 inode 会在原文件被改后继续有效。
func verstat(dev, ino uint64, size int64, mtimeNS int64) string {
	return "v1-" + strings.Join([]string{
		strconv.FormatUint(dev, 16),
		strconv.FormatUint(ino, 16),
		strconv.FormatUint(uint64(size), 16),
		strconv.FormatInt(mtimeNS, 16),
	}, "-")
}

// statVersion 从 unix.Stat_t 生成 version。
func statVersion(st *unix.Stat_t) string {
	return verstat(uint64(st.Dev), uint64(st.Ino), st.Size, st.Mtim.Nano())
}

// entryType 把文件类型映射到 DTO 闭集。
func entryType(mode uint32) string {
	switch mode & unix.S_IFMT {
	case unix.S_IFREG:
		return TypeFile
	case unix.S_IFDIR:
		return TypeDir
	case unix.S_IFLNK:
		return TypeSymlink
	default:
		return TypeOther
	}
}

// modeString 把权限位写成八进制四位数（"0644"）。
func modeString(mode uint32) string {
	return "0" + strconv.FormatUint(uint64(mode&0o7777), 8)
}

// checkRegular 在已打开的句柄上核对“可以读的普通文件”。
//
// 三个拒绝都是 CONTRACTS.md 第 11 节明写的：
//   - 特殊文件不读写（设备、FIFO、socket：读它们会阻塞或触发副作用）；
//   - nlink>1 的普通文件不读（这个 inode 在根外还有名字，越界与否无法在句柄层判定）；
//   - 设备必须与根一致（RESOLVE_NO_XDEV 已经挡了挂载跨越，这里是第二道）。
func (m *Manager) checkRegular(fd int, rel string, rootDev uint64) (unix.Stat_t, string, error) {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return st, "", fmt.Errorf("files: 无法读取 %s 的 stat: %w", rel, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return st, "", fmt.Errorf("%w: %s 是%s，面板只读取普通文件", ErrNotRegular, rel, describeMode(st.Mode))
	}
	if st.Nlink > 1 {
		return st, "", fmt.Errorf("%w: %s 有 %d 个硬链接，无法确认它没有指向根外",
			ErrHardLinked, rel, st.Nlink)
	}
	if uint64(st.Dev) != rootDev {
		return st, "", fmt.Errorf("%w: %s 不在根所在设备上", ErrPathRejected, rel)
	}
	return st, statVersion(&st), nil
}

// describeMode 给错误消息用的类型说明（中文，面向运维）。
func describeMode(mode uint32) string {
	switch mode & unix.S_IFMT {
	case unix.S_IFDIR:
		return "目录"
	case unix.S_IFLNK:
		return "符号链接"
	case unix.S_IFBLK:
		return "块设备"
	case unix.S_IFCHR:
		return "字符设备"
	case unix.S_IFIFO:
		return "命名管道"
	case unix.S_IFSOCK:
		return "socket"
	default:
		return "特殊文件"
	}
}

// List 列举根内的一个目录（CONTRACTS.md 第 11 节）。
//
// 排序与游标：目录项按名字的字节序排列，页大小固定 ListPageSize，游标是“上一页最后
// 一个名字”，下一页严格从它之后开始。这样同一个游标永远指向同一个起点，
// 不会因为两次调用之间目录变化而重复或漏掉整页（新出现的项按名字顺序落位）。
func (m *Manager) List(ctx context.Context, params ListParams) (ListPage, error) {
	if err := params.Validate(); err != nil {
		return ListPage{}, err
	}
	if err := ctx.Err(); err != nil {
		return ListPage{}, err
	}
	handle, err := m.openRoot(params.RootID)
	if err != nil {
		return ListPage{}, err
	}
	defer func() { _ = handle.Close() }()

	target := params.Path
	// 列举目录**不能**用 O_PATH：O_PATH 的 fd 只表示“树上的位置”，
	// getdents64(2) 对它返回 EBADF，ReadDir 会直接失败（每次列举都 500）。
	// 这里要真的读目录内容，所以是 O_RDONLY|O_DIRECTORY；O_NONBLOCK 是给
	// “路径最后一段其实是 FIFO”那种情况准备的——见 openRead。
	fd, err := m.openInRoot(handle.fd, target, openRead|unix.O_DIRECTORY)
	if err != nil {
		if errors.Is(err, ErrNotFound) && target == "" {
			return ListPage{}, fmt.Errorf("%w: 根目录不在了", ErrRootUnavailable)
		}
		return ListPage{}, err
	}
	dir := os.NewFile(uintptr(fd), "dir")
	defer func() { _ = dir.Close() }()

	names, truncated, err := readNames(dir)
	if err != nil {
		return ListPage{}, err
	}
	sort.Strings(names)

	start := 0
	if params.Cursor != "" {
		start = sort.SearchStrings(names, params.Cursor)
		// 游标指向的项本身**不**重复返回：它已经在上一次的结果里了。
		if start < len(names) && names[start] == params.Cursor {
			start++
		}
	}
	end := start + ListPageSize
	if end > len(names) {
		end = len(names)
	}

	page := ListPage{RootID: params.RootID, Path: target, Entries: make([]Entry, 0, end-start), Truncated: truncated}
	for _, name := range names[start:end] {
		var st unix.Stat_t
		// fstatat 相对**已打开的目录 fd**：路径段不再参与解析，因此既不会被
		// 并发 rename 带偏，也不会因为某个中间段被换成符号链接而 stat 到根外。
		if err := unix.Fstatat(fd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			if errors.Is(err, unix.ENOENT) {
				// 列举与 stat 之间被删掉了：这不是错误，跳过即可。
				continue
			}
			return ListPage{}, fmt.Errorf("files: 无法读取 %s 的信息: %w", JoinPath(target, name), err)
		}
		page.Entries = append(page.Entries, Entry{
			Name:      name,
			Type:      entryType(st.Mode),
			SizeBytes: uint64(st.Size),
			Mode:      modeString(st.Mode),
			Owner:     Owner{UID: st.Uid, GID: st.Gid},
			MTime:     time.Unix(st.Mtim.Sec, st.Mtim.Nsec).UTC(),
			Version:   statVersion(&st),
		})
	}
	if end < len(names) && len(page.Entries) > 0 {
		next := page.Entries[len(page.Entries)-1].Name
		page.NextCursor = &next
	}
	return page, nil
}

// readNames 读目录项名字，最多 MaxListScan 项。
//
// 只取名字：类型与大小要按页逐个 stat（d_type 不可靠：有些文件系统返回 DT_UNKNOWN，
// 而我们要的是**此刻**的 size/mtime/version，那必须来自一次真实的 stat）。
func readNames(dir *os.File) ([]string, bool, error) {
	entries, err := dir.ReadDir(MaxListScan + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, false, fmt.Errorf("files: 无法读取目录项: %w", err)
	}
	truncated := false
	if len(entries) > MaxListScan {
		entries = entries[:MaxListScan]
		truncated = true
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names, truncated, nil
}

// Text 读取根内一个普通文件的 UTF-8 文本（上限 MaxTextBytes）。
func (m *Manager) Text(ctx context.Context, params TextParams) (TextFile, error) {
	if err := params.Validate(); err != nil {
		return TextFile{}, err
	}
	if err := ctx.Err(); err != nil {
		return TextFile{}, err
	}
	handle, err := m.openRoot(params.RootID)
	if err != nil {
		return TextFile{}, err
	}
	defer func() { _ = handle.Close() }()

	fd, err := m.openInRoot(handle.fd, params.Path, openRead)
	if err != nil {
		return TextFile{}, err
	}
	file := os.NewFile(uintptr(fd), params.Path)
	defer func() { _ = file.Close() }()

	st, version, err := m.checkRegular(fd, params.Path, handle.entry.identity.dev)
	if err != nil {
		return TextFile{}, err
	}
	if err := verifyVersion(version, params.ExpectedVersion); err != nil {
		return TextFile{}, err
	}
	if st.Size > MaxTextBytes {
		return TextFile{}, fmt.Errorf("%w: %s 有 %d 字节，文本读取上限 %d 字节",
			ErrTooLarge, params.Path, st.Size, MaxTextBytes)
	}
	// 读的时候再设一次上限：stat 与 read 之间文件可能长大。
	raw, err := io.ReadAll(io.LimitReader(file, MaxTextBytes+1))
	if err != nil {
		return TextFile{}, fmt.Errorf("files: 读取 %s 失败: %w", params.Path, err)
	}
	if len(raw) > MaxTextBytes {
		return TextFile{}, fmt.Errorf("%w: %s 超过了文本读取上限 %d 字节",
			ErrTooLarge, params.Path, MaxTextBytes)
	}
	if !isText(raw) {
		return TextFile{}, fmt.Errorf("%w: %s 不是 UTF-8 文本（或含 NUL 字节）", ErrNotText, params.Path)
	}
	return TextFile{
		RootID:    params.RootID,
		Path:      params.Path,
		Version:   version,
		SizeBytes: uint64(len(raw)),
		Encoding:  "utf-8",
		Content:   string(raw),
	}, nil
}

// isText 判断内容是否是可以交给界面的文本。
//
// 除了合法 UTF-8，还拒绝 NUL：一个含 NUL 的文件在浏览器里会被截断显示，
// 而用户会以为“文件就到这里”。宁可说“这不是文本”。
func isText(raw []byte) bool {
	if len(raw) == 0 {
		return true
	}
	if !utf8.Valid(raw) {
		return false
	}
	for _, b := range raw {
		if b == 0 {
			return false
		}
	}
	return true
}

// Download 是一个已打开、已核验的下载源。
type Download struct {
	// File 是已打开的句柄；调用方负责 Close。
	File   *os.File
	Header DownloadHeader
	handle *rootHandle
}

// Close 关闭文件与根句柄。
func (d *Download) Close() error {
	if d == nil {
		return nil
	}
	var firstErr error
	if d.File != nil {
		firstErr = d.File.Close()
	}
	if d.handle != nil {
		if err := d.handle.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// OpenDownload 打开根内一个普通文件用于流式下载。
//
// 返回时文件已经通过 checkRegular 与版本核对；剩下的只有按字节转发。
func (m *Manager) OpenDownload(ctx context.Context, params DownloadParams) (*Download, error) {
	if err := params.Validate(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	handle, err := m.openRoot(params.RootID)
	if err != nil {
		return nil, err
	}
	fd, err := m.openInRoot(handle.fd, params.Path, openRead)
	if err != nil {
		_ = handle.Close()
		return nil, err
	}
	file := os.NewFile(uintptr(fd), params.Path)
	st, version, err := m.checkRegular(fd, params.Path, handle.entry.identity.dev)
	if err != nil {
		_ = file.Close()
		_ = handle.Close()
		return nil, err
	}
	if err := verifyVersion(version, params.ExpectedVersion); err != nil {
		_ = file.Close()
		_ = handle.Close()
		return nil, err
	}
	return &Download{
		File: file,
		Header: DownloadHeader{
			Name:      path.Base(params.Path),
			SizeBytes: uint64(st.Size),
			Version:   version,
			MTime:     time.Unix(st.Mtim.Sec, st.Mtim.Nsec).UTC(),
		},
		handle: handle,
	}, nil
}

// verifyVersion 核对调用方持有的版本与已打开句柄上的版本。
func verifyVersion(actual, expected string) error {
	if expected == "" || expected == actual {
		return nil
	}
	return fmt.Errorf("%w: 期望 %s，实际 %s", ErrVersionConflict, expected, actual)
}
