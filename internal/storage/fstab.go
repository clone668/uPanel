package storage

// /etc/fstab 的托管块（P07.4）。
//
// 这个文件的特殊性在于：它是**开机时被读的文件**。写坏一行的后果不是“这次操作失败”，
// 而是“这台机器起不来”。因此这里不做“替换整个文件”这种简单事，而是：
//
//  1. 只认自己那一块。块外内容（包括别人后来加的行、注释、空行、缩进）在替换后必须
//     逐字节相同。这一条用“块前一刀、块后一刀，中间由代码重渲染”实现，
//     不靠正则替换，也不靠按行拼接——按行拼接几乎必然在某天吃掉一个结尾换行。
//  2. 结构不对就拒绝。BEGIN 没有 END、有多个 BEGIN、块里有解析不了的行，都说明
//     有人手工动过；这时**不猜、不修**，报错让人来看。
//  3. 写之前先比哈希。整个文件的 SHA-256 在预检时算过，执行前再算一次；不一致就是
//     “别人刚改过”，此时覆盖别人的新配置，比失败糟糕得多。
//  4. 写之后先校验再换。候选文件先过 findmnt 的结构校验，通过才 rename 到目标；
//     备份留在代理数据目录里，回滚就是把备份换个名字放回去。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"upanel/internal/syscmd"
)

// 托管块的起始与结束标记。
//
// 用整行精确匹配（忽略行尾空白）而不是“包含子串”：子串匹配会被
// `# BEGIN upanel managed volumes (old)` 这种历史残留骗到。
const (
	fstabBeginMarker = "# BEGIN upanel managed volumes"
	fstabEndMarker   = "# END upanel managed volumes"
	// fstabBlockHeader 是块内的说明行。它由我们渲染，因此每次重写都会重新出现。
	fstabBlockHeader = "# 以下条目由 uPanel 维护（同一块内的手工修改会在下次变更时被覆盖）"
)

// 错误分类。调用方按类型分派：可重试的、要人介入的、配置漂移的，处置方式不同。
var (
	// ErrFstabChanged 表示文件在预检之后被别人改过。绝不覆盖。
	ErrFstabChanged = errors.New("storage: /etc/fstab 已被外部修改")
	// ErrFstabMalformed 表示托管块结构不完整（BEGIN/END 不配对、块内行解析失败）。
	ErrFstabMalformed = errors.New("storage: /etc/fstab 的托管块结构不完整")
	// ErrFstabVerify 表示候选文件没有通过校验。
	ErrFstabVerify = errors.New("storage: 候选 fstab 未通过校验")
)

// FstabEntry 是托管块里的一行。
type FstabEntry struct {
	// UUID 是文件系统的 UUID；fstab 里写作 `UUID=xxx`。
	UUID       string
	MountPoint string
	FSType     string
	Options    []string
	// Dump 是第五列（备份频率），恒为 0；保留字段是为了把读到的值显示出来。
	Dump int
	// Pass 是第六列（fsck 顺序）。
	Pass int
}

// Line 渲染这一行。
//
// 四个值都来自已校验的输入（UUID 是十六进制与中划线、路径由 slug 派生、
// 类型来自闭集、选项由枚举拼出），所以这里不需要再转义任何东西。
func (e FstabEntry) Line() string {
	return fmt.Sprintf("UUID=%s %s %s %s %d %d",
		e.UUID, e.MountPoint, e.FSType, strings.Join(e.Options, ","), e.Dump, e.Pass)
}

// String 便于日志与错误消息。
func (e FstabEntry) String() string { return e.Line() }

// FstabBlock 是一次解析的结果：块外的两段原文 + 块内的条目。
type FstabBlock struct {
	// Prefix 是块起始标记之前的全部原文（已含结尾换行）。
	Prefix string
	// Suffix 是块结束标记之后的全部原文（含它自己的换行）。
	Suffix string
	// Entries 是块内解析出来的条目（按文件顺序）。
	Entries []FstabEntry
	// HasBlock 报告文件里有没有托管块。
	HasBlock bool
}

// FstabState 是一次读取的完整快照：内容、哈希、解析结果与文件属性。
type FstabState struct {
	// Content 是文件的完整原文。
	Content string
	// Hash 是 Content 的 SHA-256（小写十六进制，带 "sha256:" 前缀）。
	Hash string
	// Block 是解析结果。
	Block FstabBlock
	// Mode 是文件权限（替换时沿用，保证 0644 root:root 不被改掉）。
	Mode os.FileMode
	// UID / GID 是文件属主（替换时沿用）。
	UID int
	GID int
	// Missing 表示文件不存在。不存在不是错误：fstab 缺失的机器也能起来
	// （只是没有额外的挂载），我们可以从零开始建一个。
	Missing bool
}

// fstabHash 是给 fstab 内容算版本的方式。
//
// 用它而不是 mtime：mtime 在同样内容被重写时会变，而内容没变就不该让预检失效。
func fstabHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ParseFstab 解析 fstab 文本，切出托管块。
//
// 解析失败只有一种处理：报错。这里没有“尽力解析”——块结构的任何异常都意味着
// 文件不完全是面板写的那份，此时按自己的理解重渲染就是在赌。
func ParseFstab(content string) (FstabBlock, error) {
	lines := splitLines(content)

	beginAt, endAt, err := findBlockMarkers(lines)
	if err != nil {
		return FstabBlock{}, err
	}
	if beginAt < 0 {
		// 没有托管块：整个文件都是“块外”，块内为空。
		return FstabBlock{Prefix: content}, nil
	}

	// 块外的两段用**原文偏移**切出来，不重新拼接行。
	// 重新拼接必须自己决定“末行要不要补换行”，而那个决定总有一种情形是错的
	// （文件本来就以换行结尾 / 本来就不以换行结尾）。按偏移切就没有这个问题：
	// Prefix 与 Suffix 是原文的切片，逐字节相同是定义上的事实，不是约定。
	starts := lineStarts(content)
	block := FstabBlock{HasBlock: true}
	if beginAt < len(starts) {
		block.Prefix = content[:starts[beginAt]]
	}
	if endAt+1 < len(starts) {
		block.Suffix = content[starts[endAt+1]:]
	}
	for i, line := range lines[beginAt+1 : endAt] {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		entry, err := parseFstabLine(trimmed)
		if err != nil {
			return FstabBlock{}, fmt.Errorf("%w: 托管块第 %d 行：%v", ErrFstabMalformed, i+1, err)
		}
		block.Entries = append(block.Entries, entry)
	}
	return block, nil
}

// lineStarts 返回每一行在原文里的起始字节偏移。
//
// splitLines 与 lineStarts 必须对“有多少行”持一致意见，否则按偏移切出来的
// Prefix/Suffix 会错位。两者的共同前提是：末尾的 "\n" 不额外产生一行。
func lineStarts(content string) []int {
	var starts []int
	if content == "" {
		return nil
	}
	starts = append(starts, 0)
	for i := 0; i < len(content); i++ {
		if content[i] == '\n' && i+1 < len(content) {
			starts = append(starts, i+1)
		}
	}
	return starts
}

// findBlockMarkers 找出唯一的 BEGIN/END 对。
func findBlockMarkers(lines []string) (beginAt, endAt int, err error) {
	beginAt, endAt = -1, -1
	for i, line := range lines {
		switch strings.TrimRight(line, " \t\r") {
		case fstabBeginMarker:
			if beginAt >= 0 {
				return 0, 0, fmt.Errorf("%w: 有多个起始标记（第 %d 行与第 %d 行）",
					ErrFstabMalformed, beginAt+1, i+1)
			}
			beginAt = i
		case fstabEndMarker:
			if endAt >= 0 {
				return 0, 0, fmt.Errorf("%w: 有多个结束标记（第 %d 行与第 %d 行）",
					ErrFstabMalformed, endAt+1, i+1)
			}
			endAt = i
		}
	}
	switch {
	case beginAt < 0 && endAt < 0:
		return -1, -1, nil
	case beginAt >= 0 && endAt < 0:
		return 0, 0, fmt.Errorf("%w: 第 %d 行有起始标记，但没有结束标记",
			ErrFstabMalformed, beginAt+1)
	case beginAt < 0:
		return 0, 0, fmt.Errorf("%w: 第 %d 行有结束标记，但没有起始标记",
			ErrFstabMalformed, endAt+1)
	case endAt < beginAt:
		return 0, 0, fmt.Errorf("%w: 结束标记（第 %d 行）在起始标记（第 %d 行）之前",
			ErrFstabMalformed, endAt+1, beginAt+1)
	}
	return beginAt, endAt, nil
}

// parseFstabLine 解析块内的一条条目。
//
// 只接受 `UUID=<uuid> <path> <type> <opts> [dump] [pass]` 这一种形状：
// 托管块里的每一行都是我们自己写上去的，出现别的形状就说明有人动了它。
// 用设备路径（/dev/sdb1）而不是 UUID 的条目在这里会被明确拒绝。
func parseFstabLine(line string) (FstabEntry, error) {
	fields := strings.Fields(line)
	if len(fields) < 4 {
		return FstabEntry{}, fmt.Errorf("字段不足（%d 个，至少 4 个）", len(fields))
	}
	if len(fields) > 6 {
		return FstabEntry{}, fmt.Errorf("字段过多（%d 个，最多 6 个）", len(fields))
	}
	src := fields[0]
	if !strings.HasPrefix(src, "UUID=") {
		return FstabEntry{}, fmt.Errorf("来源是 %q，托管块里只允许 UUID=… 形式", src)
	}
	uuid := strings.TrimPrefix(src, "UUID=")
	if !validUUIDShape(uuid) {
		return FstabEntry{}, fmt.Errorf("UUID %q 形状不合法", uuid)
	}
	if !strings.HasPrefix(fields[1], "/") {
		return FstabEntry{}, fmt.Errorf("挂载点 %q 不是绝对路径", fields[1])
	}
	entry := FstabEntry{
		UUID:       uuid,
		MountPoint: fields[1],
		FSType:     fields[2],
		Options:    strings.Split(fields[3], ","),
	}
	if len(fields) >= 5 {
		n, err := strconv.Atoi(fields[4])
		if err != nil {
			return FstabEntry{}, fmt.Errorf("第五列 %q 不是数字", fields[4])
		}
		entry.Dump = n
	}
	if len(fields) == 6 {
		n, err := strconv.Atoi(fields[5])
		if err != nil {
			return FstabEntry{}, fmt.Errorf("第六列 %q 不是数字", fields[5])
		}
		entry.Pass = n
	}
	return entry, nil
}

// validUUIDShape 检查 UUID 的形状（8-4-4-4-12 或 4-2-2-2-2-2-2-2-2-2 等变体）。
//
// 只做形状检查：真正的身份核对是读超级块，那一步在判决与执行里各做一次。
func validUUIDShape(uuid string) bool {
	if len(uuid) < 8 || len(uuid) > 64 {
		return false
	}
	hyphens := 0
	for i := 0; i < len(uuid); i++ {
		c := uuid[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		case c == '-':
			hyphens++
			if i == 0 || i == len(uuid)-1 || uuid[i-1] == '-' {
				return false
			}
		default:
			return false
		}
	}
	return hyphens >= 3
}

// Render 生成完整的候选文本。
//
// 条目按挂载点排序后渲染，保证同样的登记集合总是得到同样的字节（验收与 diff 都靠它）。
func (b FstabBlock) Render(entries []FstabEntry) string {
	sorted := make([]FstabEntry, len(entries))
	copy(sorted, entries)
	sortEntriesByMountPoint(sorted)

	var out strings.Builder
	out.WriteString(b.Prefix)
	// 块前原文若不以换行结尾（文件就是没有结尾换行），补一个：
	// 否则 BEGIN 标记会粘在上一行的末尾，变成一个“看起来像注释”的怪行。
	if out.Len() > 0 && !strings.HasSuffix(out.String(), "\n") {
		out.WriteString("\n")
	}
	out.WriteString(fstabBeginMarker)
	out.WriteString("\n")
	out.WriteString(fstabBlockHeader)
	out.WriteString("\n")
	for _, e := range sorted {
		out.WriteString(e.Line())
		out.WriteString("\n")
	}
	out.WriteString(fstabEndMarker)
	out.WriteString("\n")
	out.WriteString(b.Suffix)
	return out.String()
}

// sortEntriesByMountPoint 稳定排序（挂载点相同则按 UUID）。
func sortEntriesByMountPoint(entries []FstabEntry) {
	for i := 1; i < len(entries); i++ {
		for j := i; j > 0; j-- {
			if entries[j-1].MountPoint < entries[j].MountPoint {
				break
			}
			if entries[j-1].MountPoint == entries[j].MountPoint && entries[j-1].UUID <= entries[j].UUID {
				break
			}
			entries[j-1], entries[j] = entries[j], entries[j-1]
		}
	}
}

// splitLines 按 \n 切分且保留行内容（不含换行符）。
func splitLines(content string) []string {
	if content == "" {
		return nil
	}
	lines := strings.Split(content, "\n")
	// strings.Split 对 "a\n" 会给出 ["a", ""]：末尾空串代表“文件以换行结尾”，
	// 不能当成一行内容。
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// FstabStore 读写 /etc/fstab（真实文件系统）。
//
// Path 可注入：单元测试在临时目录里造一个 fstab，绝不碰宿主机的真文件。
type FstabStore struct {
	// Path 是 fstab 路径（生产值 /etc/fstab）。
	Path string
	// BackupDir 是替换前的备份目录。
	BackupDir string
	// Runner 执行 findmnt 校验候选文件。
	Runner CommandRunner
	// Findmnt 是 findmnt 的固定路径。
	Findmnt string
	// TempSuffix 是候选文件的后缀（同目录，便于 rename）。
	TempSuffix string
	// Now 提供时间（备份文件名用），可注入。
	Now func() time.Time

	// rename / fsyncDir / readFile / writeFile 是平台接缝。
	// 它们存在的唯一理由是让“写临时文件 → fsync → rename → fsync 目录”这条
	// 顺序能在单元测试里被逐步验证（例如“rename 之前必须已经 fsync”）。
	rename    func(oldPath, newPath string) error
	fsyncDir  func(dir string) error
	writeFile func(path string, data []byte, mode os.FileMode, uid, gid int) error
}

// fstabPath 返回实际路径。
func (s FstabStore) fstabPath() string {
	if s.Path == "" {
		return DefaultFstabPath
	}
	return s.Path
}

// backupDir 返回实际备份目录。
func (s FstabStore) backupDir() string {
	if s.BackupDir == "" {
		return DefaultFstabBackupDir
	}
	return s.BackupDir
}

// findmntPath 返回实际 findmnt 路径。
func (s FstabStore) findmntPath() string {
	if s.Findmnt == "" {
		return DefaultFindmntPath
	}
	return s.Findmnt
}

// tempSuffix 返回候选文件后缀。
func (s FstabStore) tempSuffix() string {
	if s.TempSuffix == "" {
		return ".upanel-new"
	}
	return s.TempSuffix
}

// Read 读取并解析当前文件。
//
// 文件不存在时返回 Missing=true 与空内容——那不是错误：一台没有 fstab 的机器
// 仍然能被面板接管，我们从零建托管块。
func (s FstabStore) Read() (FstabState, error) {
	path := s.fstabPath()
	st := FstabState{Mode: 0o644}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			st.Missing = true
			st.Content = ""
			st.Hash = fstabHash("")
			return st, nil
		}
		return FstabState{}, fmt.Errorf("storage: 读 %s 失败: %w", path, err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		return FstabState{}, fmt.Errorf("storage: 取 %s 的文件属性失败: %w", path, err)
	}
	st.Content = string(data)
	st.Hash = fstabHash(st.Content)
	st.Mode = fi.Mode().Perm()
	if uid, gid, ok := fileOwner(fi); ok {
		st.UID, st.GID = uid, gid
	}
	block, parseErr := ParseFstab(st.Content)
	if parseErr != nil {
		return FstabState{}, parseErr
	}
	st.Block = block
	return st, nil
}

// Write 把一组条目写进托管块。
//
// 顺序是固定的：校验外部修改 → 渲染候选 → 写候选文件 → fsync 候选 → findmnt 校验
// → 备份现行文件 → rename → fsync 目录。任何一步失败都不会让目标文件处于
// “写了一半”的状态：目标只被 rename 触碰过，而 rename 是原子的。
//
// 返回值是备份文件的路径（用于回滚与审计）；内容没有变化时返回空串。
func (s FstabStore) Write(ctx context.Context, current FstabState, entries []FstabEntry) (string, error) {
	if err := checkFstabEntrySet(entries); err != nil {
		return "", err
	}
	now, err := s.Read()
	if err != nil {
		if !errors.Is(err, ErrFstabMalformed) {
			return "", err
		}
		// 结构已经坏了：先算哈希，好让调用方仍能报告“读到的版本”。
		now = FstabState{Content: current.Content, Hash: current.Hash}
		if data, readErr := os.ReadFile(s.fstabPath()); readErr == nil {
			now.Content = string(data)
			now.Hash = fstabHash(now.Content)
		}
		return "", err
	}
	if now.Hash != current.Hash {
		return "", fmt.Errorf("%w：读取时是 %s，现在是 %s。请重新预检", ErrFstabChanged, current.Hash, now.Hash)
	}

	candidate := now.Block.Render(entries)
	if candidate == now.Content {
		// 已经是这份内容：不写。写一次会让 mtime 变，让所有并发的预检失效，
		// 却没有任何实际变化。
		return "", nil
	}

	path := s.fstabPath()
	dir := filepath.Dir(path)
	tmp := path + s.tempSuffix()
	if err := s.writeCandidate(tmp, []byte(candidate), now, dir); err != nil {
		return "", err
	}
	// 候选文件先校验再换：校验失败时目标文件仍然一个字节没动。
	if err := s.verify(ctx, tmp); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}

	backup, err := s.backup(now)
	if err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if err := s.doRename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("storage: 替换 %s 失败: %w", path, err)
	}
	if err := s.doFsyncDir(dir); err != nil {
		// rename 已经成功，内容已经是新的；目录没 fsync 意味着掉电后可能是旧内容。
		// 这不能让操作“失败”（盘上确实已经换了），但必须让人知道。
		return backup, fmt.Errorf("storage: %s 已替换，但目录 fsync 失败（掉电后可能回退）：%w", path, err)
	}
	return backup, nil
}

// writeCandidate 写候选文件：先写临时名字 → fsync 文件 → 沿用原文件的权限与属主。
func (s FstabStore) writeCandidate(tmp string, data []byte, st FstabState, dir string) error {
	mode := st.Mode
	if mode == 0 {
		mode = 0o644
	}
	if s.writeFile != nil {
		if err := s.writeFile(tmp, data, mode, st.UID, st.GID); err != nil {
			return fmt.Errorf("storage: 写候选文件 %s 失败: %w", tmp, err)
		}
		return nil
	}
	// os.WriteFile 的权限只在创建时生效；这里先 OpenFile 再 Write 再 Sync，明确 fsync。
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("storage: 写候选文件 %s 失败: %w", tmp, err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("storage: 写候选文件 %s 失败: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("storage: fsync 候选文件 %s 失败: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("storage: 关闭候选文件 %s 失败: %w", tmp, err)
	}
	// 属主要与原文件一致：fstab 必须是 root:root，弱一点的权限不该由我们"顺便"改掉。
	if st.UID != 0 || st.GID != 0 {
		if err := os.Chown(tmp, st.UID, st.GID); err != nil {
			_ = os.Remove(tmp)
			return fmt.Errorf("storage: 设置候选文件属主失败: %w", err)
		}
	}
	if err := os.Chmod(tmp, mode); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("storage: 设置候选文件权限失败: %w", err)
	}
	return nil
}

// Verify 用 findmnt 校验候选文件的结构。
//
// 不执行 `mount -a`：PLAN.md 第 9.3 节明确禁止。`mount -a` 会把整台机器的
// 所有条目都挂一遍，那是“顺手改了别的盘的挂载状态”，不属于本动作的权限范围。
func (s FstabStore) verify(ctx context.Context, candidate string) error {
	if s.Runner == nil {
		// 没有校验器就不许写：候选文件直接上盘等于把“开机能不能起来”交给运气。
		return dependencyMissing("没有配置命令执行器，无法校验候选 fstab")
	}
	spec := syscmd.Spec{
		Program: s.findmntPath(),
		Args:    []string{"--verify", "--tab", candidate},
	}
	res, err := s.Runner.Run(ctx, spec, syscmd.Limits{Timeout: DefaultSettleTimeout})
	if err != nil {
		return fmt.Errorf("%w: findmnt --verify 执行失败: %v", ErrFstabVerify, err)
	}
	if res.ExitCode != 0 {
		detail := excerpt(res.Stdout)
		if detail == "" {
			detail = excerpt(res.Stderr)
		}
		return fmt.Errorf("%w（退出码 %d）：%s", ErrFstabVerify, res.ExitCode, detail)
	}
	return nil
}

// backup 把现行文件复制到备份目录，返回备份路径。
//
// 备份必须**先于** rename 存在：rename 之后再备份，等于备份的是新内容。
func (s FstabStore) backup(st FstabState) (string, error) {
	dir := s.backupDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("storage: 创建备份目录 %s 失败: %w", dir, err)
	}
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now().UTC()
	}
	name := fmt.Sprintf("%s-%s.fstab", now.Format("20060102T150405Z"), shortHash(st.Hash))
	full := filepath.Join(dir, name)
	content := st.Content
	if content == "" {
		// 文件原本不存在：备份一个空文件，回滚时能恢复“不存在”这个状态。
		content = ""
	}
	if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
		return "", fmt.Errorf("storage: 写备份 %s 失败: %w", full, err)
	}
	return full, nil
}

// Restore 用一份备份把目标文件换回去。
//
// 与 Write 走同一条原子替换路径（候选 → fsync → rename → fsync 目录），
// 只是跳过 findmnt 校验：备份的内容本来就是这台机器正在用的那份。
func (s FstabStore) Restore(ctx context.Context, backupPath string) error {
	data, err := os.ReadFile(backupPath)
	if err != nil {
		return fmt.Errorf("storage: 读备份 %s 失败: %w", backupPath, err)
	}
	path := s.fstabPath()
	current, readErr := s.Read()
	st := FstabState{Mode: 0o644}
	if readErr == nil {
		st.Mode = current.Mode
		st.UID, st.GID = current.UID, current.GID
	}
	tmp := path + s.tempSuffix()
	if err := s.writeCandidate(tmp, data, st, filepath.Dir(path)); err != nil {
		return err
	}
	if err := s.doRename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("storage: 回滚 %s 失败: %w", path, err)
	}
	if err := s.doFsyncDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("storage: 回滚已替换 %s，但目录 fsync 失败: %w", path, err)
	}
	return nil
}

func (s FstabStore) doRename(oldPath, newPath string) error {
	if s.rename != nil {
		return s.rename(oldPath, newPath)
	}
	return os.Rename(oldPath, newPath)
}

func (s FstabStore) doFsyncDir(dir string) error {
	if s.fsyncDir != nil {
		return s.fsyncDir(dir)
	}
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// checkFstabEntrySet 在渲染之前再检查一遍条目集合。
//
// 这一层存在的理由很直白：fstab 的一行写错就是开机失败，而这里的检查是
// “写下去之前”的最后一次机会。重复的挂载点或 UUID 都不接受——同一个挂载点出现
// 两行时，systemd 的行为是“取其中一个”，而“哪一个”不是我们能决定的。
func checkFstabEntrySet(entries []FstabEntry) error {
	seenMount := make(map[string]bool, len(entries))
	seenUUID := make(map[string]bool, len(entries))
	for _, e := range entries {
		if !strings.HasPrefix(e.MountPoint, DefaultVolumeRoot+"/") {
			return fmt.Errorf("%w: 挂载点 %q 不在 %s 下", ErrUnsafeWrite, e.MountPoint, DefaultVolumeRoot)
		}
		// path.Clean（POSIX）而不是 filepath.Clean：fstab 里的路径永远是 POSIX 的，
		// 而本程序也可能在 Windows 上被编译与测试。
		if path_ := path.Clean(e.MountPoint); path_ != e.MountPoint {
			return fmt.Errorf("%w: 挂载点 %q 不是规范形式（应为 %q）", ErrUnsafeWrite, e.MountPoint, path_)
		}
		if !validUUIDShape(e.UUID) {
			return fmt.Errorf("%w: UUID %q 形状不合法", ErrUnsafeWrite, e.UUID)
		}
		if e.FSType != FilesystemExt4 && e.FSType != FilesystemXFS {
			return fmt.Errorf("%w: 文件系统类型 %q 不在支持范围", ErrUnsafeWrite, e.FSType)
		}
		if len(e.Options) == 0 {
			return fmt.Errorf("%w: 条目 %q 没有挂载选项", ErrUnsafeWrite, e.MountPoint)
		}
		for _, opt := range e.Options {
			if !validMountOption(opt) {
				return fmt.Errorf("%w: 挂载选项 %q 不在允许集合里", ErrUnsafeWrite, opt)
			}
		}
		if seenMount[e.MountPoint] {
			return fmt.Errorf("%w: 挂载点 %q 重复", ErrUnsafeWrite, e.MountPoint)
		}
		if seenUUID[e.UUID] {
			return fmt.Errorf("%w: UUID %q 重复", ErrUnsafeWrite, e.UUID)
		}
		seenMount[e.MountPoint] = true
		seenUUID[e.UUID] = true
	}
	return nil
}

// validMountOption 判断选项是否在允许集合里。
//
// 允许集合是闭集（PLAN.md 第 9.3 节：“不允许任意字符串”）。整个条目由代码生成，
// 这条路本来走不到用户输入；检查它是因为“代码生成的”这个前提值得被断言一次，
// 而不是被假定。
func validMountOption(opt string) bool {
	switch opt {
	case "rw", "ro", "nodev", "nosuid", "nofail", "noauto", "defaults":
		return true
	}
	const prefix = "x-systemd.device-timeout="
	if strings.HasPrefix(opt, prefix) {
		n, err := strconv.Atoi(strings.TrimPrefix(opt, prefix))
		return err == nil && n >= 1 && n <= 300
	}
	return false
}

// shortHash 取哈希的前 8 位十六进制（备份文件名用）。
func shortHash(h string) string {
	h = strings.TrimPrefix(h, "sha256:")
	if len(h) > 8 {
		return h[:8]
	}
	if h == "" {
		return "empty"
	}
	return h
}

// FindEntryByUUID 在块内按 UUID 找条目（观察状态用）。
func (b FstabBlock) FindEntryByUUID(uuid string) (FstabEntry, bool) {
	for _, e := range b.Entries {
		if strings.EqualFold(e.UUID, uuid) {
			return e, true
		}
	}
	return FstabEntry{}, false
}

// FindEntryByMountPoint 在块内按挂载点找条目。
func (b FstabBlock) FindEntryByMountPoint(mountPoint string) (FstabEntry, bool) {
	for _, e := range b.Entries {
		if e.MountPoint == mountPoint {
			return e, true
		}
	}
	return FstabEntry{}, false
}

// SameAs 判断两个条目是否等价（忽略第五列 dump，它恒为 0）。
//
// 用于“记录与 fstab 是否一致”的判断：不一致就是配置漂移，此时要么 configure 修，
// 要么拒绝——但绝不能当作没看见。
func (e FstabEntry) SameAs(other FstabEntry) bool {
	if !strings.EqualFold(e.UUID, other.UUID) ||
		e.MountPoint != other.MountPoint ||
		e.FSType != other.FSType ||
		e.Pass != other.Pass {
		return false
	}
	if len(e.Options) != len(other.Options) {
		return false
	}
	for i := range e.Options {
		if e.Options[i] != other.Options[i] {
			return false
		}
	}
	return true
}

// DescribeOptions 把选项渲染成一句人读的说明（错误消息与页面用）。
func DescribeOptions(opts []string) string { return strings.Join(opts, ",") }
