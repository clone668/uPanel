package agent

// P06.2：文件写动作（CONTRACTS.md 第 5 节 action 表）。
//
// 八个动作：file.mkdir、file.commit_upload、file.write_text、file.copy、file.move、
// file.delete、file.chmod、file.chown。它们共用同一套纪律：
//
//  1. **参数名就是契约里的名字**（root_id、relative_path、expected_version……），
//     这里不做“path 也行”的别名。别名会让契约里那张表变成两份。
//
//  2. **预检读一次，执行前复核再读一次，执行后读回验证**。预检给出
//     display_name / identity_fingerprint / state_version；执行前的 revalidate 用
//     相同的 Plan 重算并比对，不一致就是 PREFLIGHT_STALE 或 DEVICE_IDENTITY_CHANGED；
//     执行完读回真实结果，对不上就报 unknown 并给恢复提示。
//
//  3. **阻塞项不等于错误**。目标不存在、根只读、目录非空……都是“这个操作现在不能做，
//     原因是 X”，属于 Allowed=false + Blockers。只有真正的失败（读不到库、底层报错）
//     才让预检失败。界面因此永远能说清楚为什么按钮是灰的。
//
//  4. **锁按资源命名**：file:<root_id>:<path>。复制/移动要同时锁源与目标，
//     两个锁名一起交给 lockManager，它按排序后的顺序取——顺序固定才没有死锁。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"upanel/internal/files"
)

// 文件动作的固定名称（CONTRACTS.md 第 5 节）。
const (
	ActionFileMkdir        = "file.mkdir"
	ActionFileCommitUpload = "file.commit_upload"
	ActionFileWriteText    = "file.write_text"
	ActionFileCopy         = "file.copy"
	ActionFileMove         = "file.move"
	ActionFileDelete       = "file.delete"
	ActionFileChmod        = "file.chmod"
	ActionFileChown        = "file.chown"
)

// 确认短语的首词。
const (
	// PhraseVerbDelete 用于递归删除目录（CONTRACTS.md 第 4 节：“递归删除目录必须要求确认短语”）。
	PhraseVerbDelete = "DELETE"
	// PhraseVerbOverwrite 用于覆盖已存在的文件（同上：“覆盖恢复必须要求确认短语”）。
	PhraseVerbOverwrite = "OVERWRITE"
)

// 阻塞项/告警项的码（给人分类看的固定字符串，不是 HTTP 错误码）。
const (
	noticeRootReadOnly     = "ROOT_READ_ONLY"
	noticeRootUnavailable  = "ROOT_UNAVAILABLE"
	noticeRootUnknown      = "ROOT_UNKNOWN"
	noticeTargetExists     = "TARGET_EXISTS"
	noticeTargetMissing    = "TARGET_MISSING"
	noticeTargetNotRegular = "TARGET_NOT_REGULAR"
	noticeTargetIsDir      = "TARGET_IS_DIRECTORY"
	noticeVersionConflict  = "VERSION_CONFLICT"
	noticeTreeTooLarge     = "TREE_TOO_LARGE"
	noticeTreeUnsupported  = "TREE_UNSUPPORTED"
	noticeDirNotEmpty      = "DIRECTORY_NOT_EMPTY"
	noticeParentMissing    = "PARENT_MISSING"
	noticeUploadNotFound   = "UPLOAD_NOT_FOUND"
	noticeUploadNotReady   = "UPLOAD_NOT_READY"
	noticeAccountsMissing  = "MANAGED_ACCOUNTS_UNAVAILABLE"
	noticeNoSpace          = "INSUFFICIENT_SPACE"
	noticePathRejected     = "PATH_REJECTED"
)

// FileWriterSource 是文件写端在代理层需要的能力（生产实现是 *files.Manager）。
//
// 它比 FilesSource 宽，因为写路径需要“探针（Stat/StatTree）+ 落地 + 暂存”三组能力；
// 但方法名全部是固定动作，没有任何一个接受“任意绝对路径”或“任意命令”。
type FileWriterSource interface {
	Roots(ctx context.Context) files.RootsResult
	Stat(ctx context.Context, rootID, rel string) (files.PathStat, error)
	StatTree(ctx context.Context, rootID, rel string) (files.PathStat, error)
	AccountsAvailable() bool

	Mkdir(ctx context.Context, params files.MkdirParams) (files.WriteResult, error)
	WriteText(ctx context.Context, params files.WriteTextParams) (files.WriteResult, error)
	CommitUpload(ctx context.Context, params files.CommitUploadParams) (files.WriteResult, error)
	Copy(ctx context.Context, params files.CopyParams) (files.WriteResult, error)
	Move(ctx context.Context, params files.CopyParams) (files.WriteResult, error)
	Remove(ctx context.Context, params files.DeleteParams) (files.WriteResult, error)
	Chmod(ctx context.Context, params files.ChmodParams) (files.WriteResult, error)
	Chown(ctx context.Context, params files.ChownParams) (files.WriteResult, error)

	CreateUpload(ctx context.Context, params files.CreateUploadParams) (files.UploadInfo, error)
	WriteUploadContent(ctx context.Context, id string, body io.Reader) (files.UploadInfo, error)
	GetUpload(ctx context.Context, id string) (files.UploadInfo, error)
	DeleteUpload(ctx context.Context, id string) error
	SweepStaging(ctx context.Context, now time.Time) (int, error)
	ReservedBytes(rootID string) int64
	MaxUploadBytes() int64
}

// 编译期断言：生产实现必须同时满足读端与写端接口。
// 它在两种平台上都成立（非 Linux 的 Manager 用桩方法满足同一接口）。
var _ FileWriterSource = (*files.Manager)(nil)

// ───────────────────────────── 参数 DTO ─────────────────────────────

// fileMkdirParams 是 file.mkdir 的参数。
type fileMkdirParams struct {
	RootID string `json:"root_id"`
	Path   string `json:"relative_path"`
}

// fileWriteTextParams 是 file.write_text 的参数。
type fileWriteTextParams struct {
	RootID          string `json:"root_id"`
	Path            string `json:"relative_path"`
	ExpectedVersion string `json:"expected_version"`
	Content         string `json:"content"`
}

// fileCommitUploadParams 是 file.commit_upload 的参数。
type fileCommitUploadParams struct {
	UploadID        string `json:"upload_id"`
	Overwrite       bool   `json:"overwrite"`
	ExpectedVersion string `json:"expected_version"`
}

// fileCopyParams 是 file.copy 与 file.move 的参数。
type fileCopyParams struct {
	SourceRootID    string `json:"source_root_id"`
	SourcePath      string `json:"source_path"`
	TargetRootID    string `json:"target_root_id"`
	TargetPath      string `json:"target_path"`
	ExpectedVersion string `json:"expected_version"`
}

// fileDeleteParams 是 file.delete 的参数。
type fileDeleteParams struct {
	RootID          string `json:"root_id"`
	Path            string `json:"relative_path"`
	ExpectedVersion string `json:"expected_version"`
	Recursive       bool   `json:"recursive"`
}

// fileChmodParams 是 file.chmod 的参数。
type fileChmodParams struct {
	RootID          string `json:"root_id"`
	Path            string `json:"relative_path"`
	ExpectedVersion string `json:"expected_version"`
	Mode            string `json:"mode"`
}

// fileChownParams 是 file.chown 的参数。
type fileChownParams struct {
	RootID           string `json:"root_id"`
	Path             string `json:"relative_path"`
	ExpectedVersion  string `json:"expected_version"`
	ManagedAccountID int64  `json:"managed_account_id"`
	ManagedGroupID   int64  `json:"managed_group_id"`
}

// ───────────────────────────── 公共辅助 ─────────────────────────────

// fileKey 是锁与身份指纹里用的稳定路径键：根与相对路径合成一个字符串。
func fileKey(rootID, rel string) string {
	if rel == "" {
		return rootID + ":/"
	}
	return rootID + ":/" + rel
}

// fileLock 是文件资源锁的名字。
func fileLock(rootID, rel string) string { return "file:" + fileKey(rootID, rel) }

// fileTarget 从一次探针生成预检目标。
//
// IdentityFingerprint 描述“是哪个对象”（根 + 相对路径，这两个一起才定位一个目录项），
// StateVersion 描述“它此刻是什么”（版本串；不存在时是 "absent"）。
// 执行前的 revalidate 会用同一个函数重算并逐字比对。
func fileTarget(rootID, rel string, stat files.PathStat) Target {
	display := rootID + ":/"
	if rel != "" {
		display = rootID + ":/" + rel
	}
	stateVersion := "absent"
	if stat.Exists && stat.Version != "" {
		stateVersion = stat.Version
	}
	return Target{
		DisplayName:         display,
		IdentityFingerprint: "file:" + fileKey(rootID, rel),
		StateVersion:        stateVersion,
	}
}

// uploadTarget 是 commit_upload 的目标：上传记录本身。
//
// 它的“状态版本”由上传状态与已收字节数组成：两份不同的暂存不该被认为等价，
// 而同一个暂存在被重新上传时也确实换了内容（状态从 received 回到 open 再回 received）。
func uploadTarget(info files.UploadInfo) Target {
	return Target{
		DisplayName:         info.RootID + ":/" + info.Path,
		IdentityFingerprint: "upload:" + info.UploadID,
		StateVersion:        fmt.Sprintf("%s:%d:%d", info.State, info.ReceivedBytes, info.SizeBytes),
	}
}

// rootStatus 查一个根此刻是否可用、是否可写；第二个返回值非空表示不可用。
func rootStatus(roots files.RootsResult, rootID string) (files.RootInfo, string) {
	for _, root := range roots.Roots {
		if root.RootID == rootID {
			if !root.Available {
				reason := root.UnavailableReason
				if strings.TrimSpace(reason) == "" {
					reason = "根当前不可用"
				}
				return root, reason
			}
			return root, ""
		}
	}
	return files.RootInfo{}, fmt.Sprintf("未登记的文件根 %q", rootID)
}

// blockerFor 把 files 的业务拒绝翻成阻塞项；第二个返回值为 false 表示这是真正的失败，
// 预检应当失败而不是给出一份“不允许”的计划。
func blockerFor(err error) (Notice, bool) {
	switch {
	case errors.Is(err, files.ErrNotFound):
		return Notice{Code: noticeTargetMissing, Message: err.Error()}, true
	case errors.Is(err, files.ErrNotWritable):
		return Notice{Code: noticeRootReadOnly, Message: err.Error()}, true
	case errors.Is(err, files.ErrRootUnavailable):
		return Notice{Code: noticeRootUnavailable, Message: err.Error()}, true
	case errors.Is(err, files.ErrUnknownRoot):
		return Notice{Code: noticeRootUnknown, Message: err.Error()}, true
	case errors.Is(err, files.ErrAlreadyExists):
		return Notice{Code: noticeTargetExists, Message: err.Error()}, true
	case errors.Is(err, files.ErrVersionConflict):
		return Notice{Code: noticeVersionConflict, Message: err.Error()}, true
	case errors.Is(err, files.ErrIsDirectory):
		return Notice{Code: noticeTargetIsDir, Message: err.Error()}, true
	case errors.Is(err, files.ErrNotRegular), errors.Is(err, files.ErrHardLinked):
		return Notice{Code: noticeTargetNotRegular, Message: err.Error()}, true
	case errors.Is(err, files.ErrTreeTooLarge):
		return Notice{Code: noticeTreeTooLarge, Message: err.Error()}, true
	case errors.Is(err, files.ErrTreeUnsupported):
		return Notice{Code: noticeTreeUnsupported, Message: err.Error()}, true
	case errors.Is(err, files.ErrNoSpace):
		return Notice{Code: noticeNoSpace, Message: err.Error()}, true
	case errors.Is(err, files.ErrUploadNotFound):
		return Notice{Code: noticeUploadNotFound, Message: err.Error()}, true
	case errors.Is(err, files.ErrUploadState):
		return Notice{Code: noticeUploadNotReady, Message: err.Error()}, true
	case errors.Is(err, files.ErrPathRejected), errors.Is(err, files.ErrNotDirectory),
		errors.Is(err, files.ErrInvalidParams), errors.Is(err, files.ErrNotText),
		errors.Is(err, files.ErrTooLarge):
		return Notice{Code: noticePathRejected, Message: err.Error()}, true
	}
	return Notice{}, false
}

// parentNotices 在目标不存在时核对它所在的父目录。//
// 目标不存在而父目录也不存在（或不是目录），这次“放一个新条目进去”的操作是
// 注定失败的：预检必须把它说成 PARENT_MISSING，而不是给一份 allowed=true 的计划，
// 让任务在落地那一刻才撞 ENOENT——那时界面上已经没有人能看懂发生了什么。
//
// 读父目录失败不是阻塞项：那是代理读不到系统，预检应当失败。
func parentNotices(ctx context.Context, src FileWriterSource, rootID, rel string) ([]Notice, error) {
	parent := files.ParentPath(rel)
	if parent == "" {
		// 根自身就是父目录，不需要再查。
		return nil, nil
	}
	stat, err := src.Stat(ctx, rootID, parent)
	if err != nil {
		// 父目录自己就读不到（它上面还有一级也不存在）时，files 层同样报 ErrNotFound。
		// 这时“目标不存在”是错的：真正缺的是父目录链上的某一环，而父目录是这次
		// 操作要往里放东西的那个对象，所以说 PARENT_MISSING 才指得对方向。
		if errors.Is(err, files.ErrNotFound) {
			return []Notice{{Code: noticeParentMissing,
				Message: fmt.Sprintf("父目录 %s 不存在；先建目录，再往里放东西", fileKey(rootID, parent))}}, nil
		}
		if notice, ok := blockerFor(err); ok {
			return []Notice{notice}, nil
		}
		return nil, err
	}
	switch {
	case !stat.Exists:
		return []Notice{{Code: noticeParentMissing,
			Message: fmt.Sprintf("父目录 %s 不存在；先建目录，再往里放东西", fileKey(rootID, parent))}}, nil
	case stat.Type != files.TypeDir:
		return []Notice{{Code: noticeParentMissing,
			Message: fmt.Sprintf("%s 是 %s，不能在里面创建条目", fileKey(rootID, parent), stat.Type)}}, nil
	}
	return nil, nil
}

// missingNotice 说明一次“目标读不到”的真正原因：父目录缺失，还是目标自己不存在。
//
// 只在 files 层报 ErrNotFound 时调用：那种情况下目标与它的某级父目录都读不到
// （父目录能读到、目标不存在时，files 正常返回 Exists=false，走的不是错误分支）。
// 把这两种情况说成同一句“目标不存在”，界面就没有下一步可给了。
func missingNotice(ctx context.Context, src FileWriterSource, rootID, rel string) Notice {
	if extra, err := parentNotices(ctx, src, rootID, rel); err == nil && len(extra) > 0 {
		return extra[0]
	}
	return Notice{Code: noticeTargetMissing, Message: fileKey(rootID, rel) + " 不存在"}
}

// executeError 让业务拒绝带上它的 HTTP 判决，执行器失败时调用方才能看到
// VERSION_CONFLICT / PATH_OUTSIDE_ROOT 这类具体原因，而不是一句“动作执行失败”。
//
// 未分类的错误原样返回：一次读不到库的失败不该被包装成一条“业务拒绝”。
func executeError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := files.HTTPStatusOf(err); ok {
		return files.AsHTTPError(err)
	}
	return err
}

// planBlocked 构造一份“不允许”的计划。
func planBlocked(normalized any, target Target, notice Notice, locks []string) Plan {
	return Plan{
		NormalizedParams: normalized,
		Target:           target,
		Allowed:          false,
		Blockers:         []Notice{notice},
		Impacts:          []Impact{{Kind: "none", Description: "被拒绝的操作不会做任何变更"}},
		Locks:            locks,
	}
}

// planCheck 是一次写动作预检的公共骨架：
//
//	根是否登记/可用/可写 → 目标此刻的形状 → 版本是否匹配 → 由 check 补充细节。
//
// check 可以返回错误：目标不存在时它还要查父目录，而一次读不到父目录的失败
// 必须让预检失败，不能被写成一条“父目录不存在”的阻塞项。
//
// 它只读，不写系统；重复调用必须给出同一个结论（revalidate 就是再调一次）。
func planCheck(
	ctx context.Context,
	src FileWriterSource,
	normalized any,
	rootID, rel string,
	expectedVersion string,
	locks []string,
	check func(stat files.PathStat) ([]Notice, error),
) (Plan, error) {
	roots := src.Roots(ctx)
	root, reason := rootStatus(roots, rootID)
	if reason != "" {
		code := noticeRootUnknown
		if root.RootID == rootID {
			code = noticeRootUnavailable
		}
		return planBlocked(normalized, Target{
			DisplayName:         fileKey(rootID, rel),
			IdentityFingerprint: "file:" + fileKey(rootID, rel),
			StateVersion:        "unavailable",
		}, Notice{Code: code, Message: reason}, locks), nil
	}
	if !root.Writable {
		return planBlocked(normalized, Target{
			DisplayName:         fileKey(rootID, rel),
			IdentityFingerprint: "file:" + fileKey(rootID, rel),
			StateVersion:        "read-only",
		}, Notice{Code: noticeRootReadOnly,
			Message: fmt.Sprintf("文件根 %q 登记为只读（writable=false），不能写入", root.Name)}, locks), nil
	}

	stat, err := src.Stat(ctx, rootID, rel)
	if err != nil {
		if notice, ok := blockerFor(err); ok {
			// 目标路径里有一级目录不存在时，files 层报的是 ErrNotFound：打开父目录
			// 就撞上了 ENOENT，根本走不到“目标存不存在”那一步。这时说“父目录不存在”
			// 才是能指导下一步的那句话，也才是契约里 PARENT_MISSING 存在的理由。
			if errors.Is(err, files.ErrNotFound) {
				notice = missingNotice(ctx, src, rootID, rel)
			}
			return planBlocked(normalized, Target{
				DisplayName:         fileKey(rootID, rel),
				IdentityFingerprint: "file:" + fileKey(rootID, rel),
				StateVersion:        "unknown",
			}, notice, locks), nil
		}
		return Plan{}, err
	}
	target := fileTarget(rootID, rel, stat)

	blockers := make([]Notice, 0, 4)
	// 版本核对：调用方给的版本必须与此刻一致，否则“保存”就是覆盖别人的修改。
	if expectedVersion != "" && stat.Exists && stat.Version != expectedVersion {
		blockers = append(blockers, Notice{
			Code: noticeVersionConflict,
			Message: fmt.Sprintf("%s 已被修改（当前版本 %s，你手里的是 %s），"+
				"请重新加载后再操作", fileKey(rootID, rel), stat.Version, expectedVersion),
		})
	}
	extra, err := check(stat)
	if err != nil {
		return Plan{}, err
	}
	blockers = append(blockers, extra...)
	return Plan{
		NormalizedParams: normalized,
		Target:           target,
		Allowed:          len(blockers) == 0,
		Blockers:         blockers,
		Impacts:          []Impact{},
		Locks:            locks,
	}, nil
}

// observedFromWrite 把一次写操作的结果整理成 observed_state。
//
// 这些值全部来自“操作完成后重新读回来的那一份”，不是我们打算写什么。
func observedFromWrite(res files.WriteResult) map[string]any {
	observed := map[string]any{
		"root_id":     res.RootID,
		"path":        res.Path,
		"exists":      res.Version != "",
		"size_bytes":  res.SizeBytes,
		"backup_path": res.BackupPath,
	}
	if res.Type != "" {
		observed["type"] = res.Type
	}
	if res.Mode != "" {
		observed["mode"] = res.Mode
	}
	if res.Version != "" {
		observed["version"] = res.Version
	}
	if res.Entries > 0 {
		observed["entries"] = res.Entries
	}
	if res.Bytes > 0 {
		observed["bytes"] = res.Bytes
	}
	if res.Source != "" {
		observed["source"] = res.Source
	}
	return observed
}

// writeData 是动作的附加信息（data 段）。
func writeData(res files.WriteResult) map[string]any {
	data := map[string]any{"root_id": res.RootID, "path": res.Path}
	if res.BackupPath != "" {
		data["backup_path"] = res.BackupPath
		data["backup_count"] = res.BackupCount
		data["backup_bytes"] = res.BackupBytes
	}
	if res.Source != "" {
		data["source"] = res.Source
	}
	return data
}

// ───────────────────────────── 登记 ─────────────────────────────

// RegisterFileActions 把八个文件写动作登记进 reg（P06.2）。
func RegisterFileActions(reg *Registry, src FileWriterSource) error {
	for _, build := range []func(FileWriterSource) Action{
		newFileMkdirAction,
		newFileWriteTextAction,
		newFileCommitUploadAction,
		newFileCopyAction,
		newFileMoveAction,
		newFileDeleteAction,
		newFileChmodAction,
		newFileChownAction,
	} {
		action := build(src)
		if err := reg.RegisterAction(action); err != nil {
			return fmt.Errorf("代理登记 %s 失败: %w", action.Name, err)
		}
	}
	return nil
}

// ───────────────────────────── file.mkdir ─────────────────────────────

// newFileMkdirAction 登记 file.mkdir。
func newFileMkdirAction(src FileWriterSource) Action {
	return Action{
		Name: ActionFileMkdir,
		Decode: func(raw json.RawMessage) (any, error) {
			var p fileMkdirParams
			if err := DecodeParams(raw, &p); err != nil {
				return nil, err
			}
			if err := validateFileActionTarget(p.RootID, p.Path); err != nil {
				return nil, err
			}
			return p, nil
		},
		Plan: func(ctx context.Context, in PlanInput) (Plan, error) {
			p, ok := in.Params.(fileMkdirParams)
			if !ok {
				return Plan{}, fmt.Errorf("参数类型不匹配: %T", in.Params)
			}
			locks := []string{fileLock(p.RootID, p.Path)}
			plan, err := planCheck(ctx, src, p, p.RootID, p.Path, "", locks, func(stat files.PathStat) ([]Notice, error) {
				if stat.Exists {
					return []Notice{{Code: noticeTargetExists,
						Message: fmt.Sprintf("%s 已存在，不能新建同名目录", fileKey(p.RootID, p.Path))}}, nil
				}
				return parentNotices(ctx, src, p.RootID, p.Path)
			})
			if err != nil || !plan.Allowed {
				return plan, err
			}
			plan.Impacts = []Impact{{Kind: "create", Description: "创建一个空目录 " + fileKey(p.RootID, p.Path)}}
			return plan, nil
		},
		Policy: ActionPolicy{
			// CONTRACTS.md 第 5 节：file.mkdir 的 R 是“否”——建空目录可逆且不覆盖任何东西。
			RequiresReauth: false,
			RequiresPhrase: false,
			Cancellable:    false,
		},
		Execute: func(ctx context.Context, in ExecuteInput) (Result, error) {
			p, ok := in.Params.(fileMkdirParams)
			if !ok {
				return Result{}, fmt.Errorf("参数类型不匹配: %T", in.Params)
			}
			res, err := src.Mkdir(ctx, files.MkdirParams{RootID: p.RootID, Path: p.Path})
			if err != nil {
				return Result{}, executeError(err)
			}
			return Result{
				Outcome:       OutcomeVerified,
				VerifiedAt:    time.Now().UTC(),
				ObservedState: observedFromWrite(res),
				Data:          writeData(res),
			}, nil
		},
	}
}

// ───────────────────────────── file.write_text ─────────────────────────────

// newFileWriteTextAction 登记 file.write_text。
func newFileWriteTextAction(src FileWriterSource) Action {
	return Action{
		Name:             ActionFileWriteText,
		ParamsLimitBytes: files.MaxTextBytes + 4<<10,
		Decode: func(raw json.RawMessage) (any, error) {
			var p fileWriteTextParams
			// write_text 的参数里带着文件内容，所以它的上限是内容上限 + 余量，
			// 而不是通用的 64 KiB（CONTRACTS.md 第 15 节：按动作独立限制）。
			if err := DecodeParamsLimit(raw, &p, files.MaxTextBytes+4<<10); err != nil {
				return nil, err
			}
			if err := validateFileActionTarget(p.RootID, p.Path); err != nil {
				return nil, err
			}
			params := files.WriteTextParams{
				RootID:          p.RootID,
				Path:            p.Path,
				ExpectedVersion: p.ExpectedVersion,
				Content:         p.Content,
			}
			if err := params.Validate(); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrParamsInvalid, err)
			}
			return p, nil
		},
		Plan: func(ctx context.Context, in PlanInput) (Plan, error) {
			p, ok := in.Params.(fileWriteTextParams)
			if !ok {
				return Plan{}, fmt.Errorf("参数类型不匹配: %T", in.Params)
			}
			locks := []string{fileLock(p.RootID, p.Path)}
			plan, err := planCheck(ctx, src, p, p.RootID, p.Path, p.ExpectedVersion, locks,
				func(stat files.PathStat) ([]Notice, error) {
					if !stat.Exists {
						return parentNotices(ctx, src, p.RootID, p.Path)
					}
					if stat.Type != files.TypeFile {
						return []Notice{{Code: noticeTargetNotRegular,
							Message: fmt.Sprintf("%s 是 %s，面板只改写普通文本文件",
								fileKey(p.RootID, p.Path), stat.Type)}}, nil
					}
					if stat.SizeBytes > files.MaxTextBytes {
						return []Notice{{Code: noticePathRejected,
							Message: fmt.Sprintf("%s 有 %d 字节，超过文本编辑上限 %d 字节",
								fileKey(p.RootID, p.Path), stat.SizeBytes, files.MaxTextBytes)}}, nil
					}
					// 目标存在而调用方没给版本：拒绝。否则“保存”会变成“用我手里这份
					// 覆盖别人刚改的那份”——这正是阶段验收要挡的事。
					if p.ExpectedVersion == "" {
						return []Notice{{Code: noticeVersionConflict,
							Message: fmt.Sprintf("%s 已存在，覆盖必须给出当前版本（expected_version）",
								fileKey(p.RootID, p.Path))}}, nil
					}
					return nil, nil
				})
			if err != nil || !plan.Allowed {
				return plan, err
			}
			description := "创建 " + fileKey(p.RootID, p.Path)
			if plan.Target.StateVersion != "absent" {
				description = "改写 " + fileKey(p.RootID, p.Path) + "，旧内容进备份"
			}
			plan.Impacts = []Impact{{Kind: "write", Description: description}}
			return plan, nil
		},
		Policy: ActionPolicy{
			// CONTRACTS.md 第 5 节：file.write_text 的 R 是“否”。它改写的是文件内容，
			// 旧版本进备份，因此不需要密码再认证；但目标已存在时必须带版本，见 Plan。
			RequiresReauth: false,
			RequiresPhrase: false,
			Cancellable:    false,
		},
		Execute: func(ctx context.Context, in ExecuteInput) (Result, error) {
			p, ok := in.Params.(fileWriteTextParams)
			if !ok {
				return Result{}, fmt.Errorf("参数类型不匹配: %T", in.Params)
			}
			res, err := src.WriteText(ctx, files.WriteTextParams{
				RootID:          p.RootID,
				Path:            p.Path,
				ExpectedVersion: p.ExpectedVersion,
				Content:         p.Content,
			})
			if err != nil {
				return Result{}, executeError(err)
			}
			return Result{
				Outcome:       OutcomeVerified,
				VerifiedAt:    time.Now().UTC(),
				ObservedState: observedFromWrite(res),
				Data:          writeData(res),
			}, nil
		},
	}
}

// ───────────────────────────── file.commit_upload ─────────────────────────────

// newFileCommitUploadAction 登记 file.commit_upload。
func newFileCommitUploadAction(src FileWriterSource) Action {
	return Action{
		Name: ActionFileCommitUpload,
		Decode: func(raw json.RawMessage) (any, error) {
			var p fileCommitUploadParams
			if err := DecodeParams(raw, &p); err != nil {
				return nil, err
			}
			params := files.CommitUploadParams{
				UploadID:        p.UploadID,
				Overwrite:       p.Overwrite,
				ExpectedVersion: p.ExpectedVersion,
			}
			if err := params.Validate(); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrParamsInvalid, err)
			}
			return p, nil
		},
		Plan: func(ctx context.Context, in PlanInput) (Plan, error) {
			p, ok := in.Params.(fileCommitUploadParams)
			if !ok {
				return Plan{}, fmt.Errorf("参数类型不匹配: %T", in.Params)
			}
			info, err := src.GetUpload(ctx, p.UploadID)
			if err != nil {
				if notice, ok := blockerFor(err); ok {
					return planBlocked(p, Target{
						DisplayName:         "上传 " + p.UploadID,
						IdentityFingerprint: "upload:" + p.UploadID,
						StateVersion:        "absent",
					}, notice, nil), nil
				}
				return Plan{}, err
			}
			locks := []string{fileLock(info.RootID, info.Path), "upload:" + info.UploadID}

			if info.State != files.UploadStateReceived {
				return planBlocked(p, uploadTarget(info), Notice{
					Code:    noticeUploadNotReady,
					Message: fmt.Sprintf("上传 %s 的状态是 %s，只有收完（%s）才能提交", info.UploadID, info.State, files.UploadStateReceived),
				}, locks), nil
			}
			roots := src.Roots(ctx)
			root, reason := rootStatus(roots, info.RootID)
			if reason != "" {
				return planBlocked(p, uploadTarget(info), Notice{Code: noticeRootUnavailable, Message: reason}, locks), nil
			}
			if !root.Writable {
				return planBlocked(p, uploadTarget(info), Notice{Code: noticeRootReadOnly,
					Message: fmt.Sprintf("文件根 %q 登记为只读（writable=false），不能落位", root.Name)}, locks), nil
			}

			stat, err := src.Stat(ctx, info.RootID, info.Path)
			if err != nil {
				if notice, ok := blockerFor(err); ok {
					if errors.Is(err, files.ErrNotFound) {
						notice = missingNotice(ctx, src, info.RootID, info.Path)
					}
					return planBlocked(p, uploadTarget(info), notice, locks), nil
				}
				return Plan{}, err
			}
			target := Target{
				DisplayName:         info.RootID + ":/" + info.Path,
				IdentityFingerprint: "upload:" + info.UploadID,
				// 状态版本同时覆盖“上传内容”与“目标此刻的样子”：任何一边变了，
				// 这份预检都该失效。
				StateVersion: fmt.Sprintf("%s:%d:%d:%s", info.State, info.ReceivedBytes, info.SizeBytes,
					fileTarget(info.RootID, info.Path, stat).StateVersion),
			}

			var blockers []Notice
			var impacts []Impact
			if stat.Exists {
				switch {
				case !p.Overwrite:
					blockers = append(blockers, Notice{Code: noticeTargetExists,
						Message: fmt.Sprintf("%s 已存在；要替换它必须显式选择覆盖", fileKey(info.RootID, info.Path))})
				case stat.Type != files.TypeFile:
					blockers = append(blockers, Notice{Code: noticeTargetNotRegular,
						Message: fmt.Sprintf("%s 是 %s，面板不替换非普通文件", fileKey(info.RootID, info.Path), stat.Type)})
				case p.ExpectedVersion == "" || p.ExpectedVersion != stat.Version:
					blockers = append(blockers, Notice{Code: noticeVersionConflict,
						Message: fmt.Sprintf("%s 已被修改（当前版本 %s），覆盖被拒绝；请重新加载后再操作",
							fileKey(info.RootID, info.Path), stat.Version)})
				}
				impacts = append(impacts, Impact{Kind: "overwrite",
					Description: "替换 " + fileKey(info.RootID, info.Path) + " 的现有内容，旧内容进备份"})
			} else {
				if p.ExpectedVersion != "" {
					blockers = append(blockers, Notice{Code: noticeVersionConflict,
						Message: fmt.Sprintf("%s 已不存在，不能按版本覆盖", fileKey(info.RootID, info.Path))})
				}
				// 父目录可能在“建暂存 → 提交”之间被删掉：落位之前再核一次。
				parentBlockers, perr := parentNotices(ctx, src, info.RootID, info.Path)
				if perr != nil {
					return Plan{}, perr
				}
				blockers = append(blockers, parentBlockers...)
				impacts = append(impacts, Impact{Kind: "create",
					Description: fmt.Sprintf("把上传的 %d 字节落位到 %s", info.SizeBytes, fileKey(info.RootID, info.Path))})
			}
			return Plan{
				NormalizedParams: p,
				Target:           target,
				Allowed:          len(blockers) == 0,
				Blockers:         blockers,
				Impacts:          impacts,
				Locks:            locks,
			}, nil
		},
		PolicyFor: func(params any) (ActionPolicy, error) {
			p, ok := params.(fileCommitUploadParams)
			if !ok {
				return ActionPolicy{}, fmt.Errorf("参数类型不匹配: %T", params)
			}
			if !p.Overwrite {
				return ActionPolicy{Cancellable: false}, nil
			}
			// 覆盖已存在的文件：契约把“覆盖”标为需要再认证，并且 §4 里
			// “覆盖恢复”必须带确认短语。门槛在预检时算定并落库，提交时读的是那份记录。
			return ActionPolicy{
				RequiresReauth: true,
				RequiresPhrase: true,
				PhraseVerb:     PhraseVerbOverwrite,
				Cancellable:    false,
			}, nil
		},
		Execute: func(ctx context.Context, in ExecuteInput) (Result, error) {
			p, ok := in.Params.(fileCommitUploadParams)
			if !ok {
				return Result{}, fmt.Errorf("参数类型不匹配: %T", in.Params)
			}
			res, err := src.CommitUpload(ctx, files.CommitUploadParams{
				UploadID:        p.UploadID,
				Overwrite:       p.Overwrite,
				ExpectedVersion: p.ExpectedVersion,
			})
			if err != nil {
				return Result{}, executeError(err)
			}
			return Result{
				Outcome:       OutcomeVerified,
				VerifiedAt:    time.Now().UTC(),
				ObservedState: observedFromWrite(res),
				Data:          writeData(res),
			}, nil
		},
	}
}

// ───────────────────────────── file.copy / file.move ─────────────────────────────

// newFileCopyAction 登记 file.copy。
func newFileCopyAction(src FileWriterSource) Action {
	return newFileTransferAction(src, ActionFileCopy, false)
}

// newFileMoveAction 登记 file.move（重命名由它承担：契约里没有 file.rename）。
func newFileMoveAction(src FileWriterSource) Action {
	return newFileTransferAction(src, ActionFileMove, true)
}

// newFileTransferAction 是 copy 与 move 的共用实现。
//
// 两者的预检与执行只差一件事：要不要删源。其余（两个根都要核对、目标存在则拒绝、
// 两个锁一起拿）完全相同，所以它们共用一份实现，避免“copy 有这项检查、move 忘了”。
func newFileTransferAction(src FileWriterSource, name string, move bool) Action {
	return Action{
		Name: name,
		Decode: func(raw json.RawMessage) (any, error) {
			var p fileCopyParams
			if err := DecodeParams(raw, &p); err != nil {
				return nil, err
			}
			params := files.CopyParams{
				SourceRootID:    p.SourceRootID,
				SourcePath:      p.SourcePath,
				TargetRootID:    p.TargetRootID,
				TargetPath:      p.TargetPath,
				ExpectedVersion: p.ExpectedVersion,
			}
			if err := params.Validate(); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrParamsInvalid, err)
			}
			return p, nil
		},
		Plan: func(ctx context.Context, in PlanInput) (Plan, error) {
			p, ok := in.Params.(fileCopyParams)
			if !ok {
				return Plan{}, fmt.Errorf("参数类型不匹配: %T", in.Params)
			}
			// 两个锁一起拿，顺序由 lockManager 排序保证；这里只给出去重后的名字。
			locks := []string{fileLock(p.SourceRootID, p.SourcePath), fileLock(p.TargetRootID, p.TargetPath)}
			sort.Strings(locks)

			roots := src.Roots(ctx)
			for _, rootID := range []string{p.TargetRootID, p.SourceRootID} {
				root, reason := rootStatus(roots, rootID)
				if reason != "" {
					code := noticeRootUnknown
					if root.RootID == rootID {
						code = noticeRootUnavailable
					}
					return planBlocked(p, Target{
						DisplayName:         fileKey(rootID, ""),
						IdentityFingerprint: "file:" + fileKey(rootID, ""),
						StateVersion:        "unavailable",
					}, Notice{Code: code, Message: reason}, locks), nil
				}
				// move 会删除源，所以源根也必须可写；copy 只读源。
				if !root.Writable {
					return planBlocked(p, Target{
						DisplayName:         fileKey(rootID, ""),
						IdentityFingerprint: "file:" + fileKey(rootID, ""),
						StateVersion:        "read-only",
					}, Notice{Code: noticeRootReadOnly,
						Message: fmt.Sprintf("文件根 %q 登记为只读（writable=false）", root.Name)}, locks), nil
				}
			}

			source, err := src.StatTree(ctx, p.SourceRootID, p.SourcePath)
			if err != nil {
				if notice, ok := blockerFor(err); ok {
					return planBlocked(p, Target{
						DisplayName:         fileKey(p.SourceRootID, p.SourcePath),
						IdentityFingerprint: "file:" + fileKey(p.SourceRootID, p.SourcePath),
						StateVersion:        "unknown",
					}, notice, locks), nil
				}
				return Plan{}, err
			}
			if !source.Exists {
				return planBlocked(p, Target{
					DisplayName:         fileKey(p.SourceRootID, p.SourcePath),
					IdentityFingerprint: "file:" + fileKey(p.SourceRootID, p.SourcePath),
					StateVersion:        "absent",
				}, Notice{Code: noticeTargetMissing,
					Message: "源 " + fileKey(p.SourceRootID, p.SourcePath) + " 不存在"}, locks), nil
			}

			target, err := src.Stat(ctx, p.TargetRootID, p.TargetPath)
			if err != nil {
				if notice, ok := blockerFor(err); ok {
					return planBlocked(p, fileTarget(p.TargetRootID, p.TargetPath, files.PathStat{}), notice, locks), nil
				}
				return Plan{}, err
			}

			var blockers []Notice
			var impacts []Impact
			if target.Exists {
				blockers = append(blockers, Notice{Code: noticeTargetExists,
					Message: fmt.Sprintf("目标 %s 已存在；%s 不覆盖已存在的东西",
						fileKey(p.TargetRootID, p.TargetPath), name)})
			} else {
				parentBlockers, perr := parentNotices(ctx, src, p.TargetRootID, p.TargetPath)
				if perr != nil {
					return Plan{}, perr
				}
				blockers = append(blockers, parentBlockers...)
			}
			if source.Type != files.TypeFile && source.Type != files.TypeDir {
				blockers = append(blockers, Notice{Code: noticeTreeUnsupported,
					Message: fmt.Sprintf("源 %s 是 %s，面板不复制/移动它",
						fileKey(p.SourceRootID, p.SourcePath), source.Type)})
			}
			if source.Type == files.TypeDir {
				if source.TreeSymlinks > 0 || source.TreeOthers > 0 {
					blockers = append(blockers, Notice{Code: noticeTreeUnsupported,
						Message: fmt.Sprintf("源目录里有 %d 个符号链接、%d 个特殊文件，整棵树不能搬",
							source.TreeSymlinks, source.TreeOthers)})
				}
				if source.TreeTruncated {
					blockers = append(blockers, Notice{Code: noticeTreeTooLarge,
						Message: fmt.Sprintf("源目录超过 %d 项，拒绝整棵树操作", files.MaxTreeEntries)})
				}
				impacts = append(impacts, Impact{Kind: "tree",
					Description: fmt.Sprintf("涉及 %d 个条目、%d 字节", source.TreeEntries, source.TreeBytes)})
			}
			crossRoot := p.SourceRootID != p.TargetRootID
			if move && crossRoot {
				// 跨根 move 不是原子操作：复制 → 校验 → 落位 → 删源。必须说清楚。
				impacts = append(impacts, Impact{Kind: "not_atomic",
					Description: "跨根移动不是原子操作：先复制并校验，再删除源；中途失败会保留源"})
			} else if move {
				impacts = append(impacts, Impact{Kind: "rename",
					Description: "同根内是原子改名（renameat），没有中间状态"})
			}

			return Plan{
				NormalizedParams: p,
				// 复制/移动的目标是**目标路径**：执行前复核它没被别人占掉。
				Target: func() Target {
					t := fileTarget(p.TargetRootID, p.TargetPath, target)
					t.DisplayName = fileKey(p.SourceRootID, p.SourcePath) + " → " + fileKey(p.TargetRootID, p.TargetPath)
					return t
				}(),
				Allowed:  len(blockers) == 0,
				Blockers: blockers,
				Impacts:  impacts,
				Locks:    locks,
			}, nil
		},
		Policy: ActionPolicy{
			// CONTRACTS.md 第 5 节：file.copy / file.move 的 R 是“否”，且“目标存在则拒绝”
			// 已经由预检挡住，所以不需要再认证。
			RequiresReauth: false,
			RequiresPhrase: false,
			Cancellable:    false,
		},
		Execute: func(ctx context.Context, in ExecuteInput) (Result, error) {
			p, ok := in.Params.(fileCopyParams)
			if !ok {
				return Result{}, fmt.Errorf("参数类型不匹配: %T", in.Params)
			}
			params := files.CopyParams{
				SourceRootID:    p.SourceRootID,
				SourcePath:      p.SourcePath,
				TargetRootID:    p.TargetRootID,
				TargetPath:      p.TargetPath,
				ExpectedVersion: p.ExpectedVersion,
			}
			var res files.WriteResult
			var err error
			if move {
				res, err = src.Move(ctx, params)
			} else {
				res, err = src.Copy(ctx, params)
			}
			if err != nil {
				return Result{}, executeError(err)
			}
			return Result{
				Outcome:       OutcomeVerified,
				VerifiedAt:    time.Now().UTC(),
				ObservedState: observedFromWrite(res),
				Data:          writeData(res),
			}, nil
		},
	}
}

// ───────────────────────────── file.delete ─────────────────────────────

// newFileDeleteAction 登记 file.delete。
func newFileDeleteAction(src FileWriterSource) Action {
	return Action{
		Name: ActionFileDelete,
		Decode: func(raw json.RawMessage) (any, error) {
			var p fileDeleteParams
			if err := DecodeParams(raw, &p); err != nil {
				return nil, err
			}
			params := files.DeleteParams{
				RootID:          p.RootID,
				Path:            p.Path,
				ExpectedVersion: p.ExpectedVersion,
				Recursive:       p.Recursive,
			}
			if err := params.Validate(); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrParamsInvalid, err)
			}
			return p, nil
		},
		Plan: func(ctx context.Context, in PlanInput) (Plan, error) {
			p, ok := in.Params.(fileDeleteParams)
			if !ok {
				return Plan{}, fmt.Errorf("参数类型不匹配: %T", in.Params)
			}
			locks := []string{fileLock(p.RootID, p.Path)}
			plan, err := planCheck(ctx, src, p, p.RootID, p.Path, p.ExpectedVersion, locks,
				func(stat files.PathStat) ([]Notice, error) {
					if !stat.Exists {
						return []Notice{{Code: noticeTargetMissing,
							Message: fileKey(p.RootID, p.Path) + " 不存在"}}, nil
					}
					if stat.Type != files.TypeDir {
						return nil, nil
					}
					tree, err := src.StatTree(ctx, p.RootID, p.Path)
					if err != nil {
						if notice, ok := blockerFor(err); ok {
							return []Notice{notice}, nil
						}
						return []Notice{{Code: noticePathRejected, Message: err.Error()}}, nil
					}
					if tree.TreeTruncated {
						return []Notice{{Code: noticeTreeTooLarge,
							Message: fmt.Sprintf("目录树超过 %d 项，拒绝递归删除", files.MaxTreeEntries)}}, nil
					}
					if !p.Recursive && tree.TreeEntries > 0 {
						return []Notice{{Code: noticeDirNotEmpty,
							Message: fmt.Sprintf("%s 里有 %d 项，非空目录不能直接删除；"+
								"递归删除需要 recursive=true 并输入确认短语",
								fileKey(p.RootID, p.Path), tree.TreeEntries)}}, nil
					}
					return nil, nil
				})
			if err != nil {
				return Plan{}, err
			}
			if !plan.Allowed {
				return plan, nil
			}
			stat, err := src.StatTree(ctx, p.RootID, p.Path)
			if err != nil {
				if notice, ok := blockerFor(err); ok {
					return planBlocked(p, plan.Target, notice, locks), nil
				}
				return Plan{}, err
			}
			if stat.Type == files.TypeDir {
				plan.Impacts = []Impact{{Kind: "delete",
					Description: fmt.Sprintf("删除目录 %s 及其中的 %d 个条目、%d 字节",
						fileKey(p.RootID, p.Path), stat.TreeEntries, stat.TreeBytes)}}
				if p.Recursive {
					plan.Warnings = []Notice{{Code: "RECURSIVE_DELETE",
						Message: "递归删除不可撤销：目录里的文件不会进备份"}}
				}
			} else {
				plan.Impacts = []Impact{{Kind: "delete",
					Description: fmt.Sprintf("删除 %s（%d 字节）", fileKey(p.RootID, p.Path), stat.SizeBytes)}}
			}
			return plan, nil
		},
		PolicyFor: func(params any) (ActionPolicy, error) {
			p, ok := params.(fileDeleteParams)
			if !ok {
				return ActionPolicy{}, fmt.Errorf("参数类型不匹配: %T", params)
			}
			policy := ActionPolicy{
				// CONTRACTS.md 第 5 节：file.delete 的 R 是“是”。
				RequiresReauth: true,
				Cancellable:    false,
			}
			if p.Recursive {
				// §4：“递归删除目录必须要求确认短语”。
				policy.RequiresPhrase = true
				policy.PhraseVerb = PhraseVerbDelete
			}
			return policy, nil
		},
		Execute: func(ctx context.Context, in ExecuteInput) (Result, error) {
			p, ok := in.Params.(fileDeleteParams)
			if !ok {
				return Result{}, fmt.Errorf("参数类型不匹配: %T", in.Params)
			}
			res, err := src.Remove(ctx, files.DeleteParams{
				RootID:          p.RootID,
				Path:            p.Path,
				ExpectedVersion: p.ExpectedVersion,
				Recursive:       p.Recursive,
			})
			if err != nil {
				return Result{}, executeError(err)
			}
			observed := observedFromWrite(res)
			// 删除的验证是“读回不存在”；observed 里 exists=false 就是那句话。
			observed["entries"] = res.Entries
			observed["bytes"] = res.Bytes
			return Result{
				Outcome:       OutcomeVerified,
				VerifiedAt:    time.Now().UTC(),
				ObservedState: observed,
				Data:          writeData(res),
			}, nil
		},
	}
}

// ───────────────────────────── file.chmod ─────────────────────────────

// newFileChmodAction 登记 file.chmod。
func newFileChmodAction(src FileWriterSource) Action {
	return Action{
		Name: ActionFileChmod,
		Decode: func(raw json.RawMessage) (any, error) {
			var p fileChmodParams
			if err := DecodeParams(raw, &p); err != nil {
				return nil, err
			}
			if err := validateFileActionTarget(p.RootID, p.Path); err != nil {
				return nil, err
			}
			if _, err := files.ParseMode(p.Mode); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrParamsInvalid, err)
			}
			return p, nil
		},
		Plan: func(ctx context.Context, in PlanInput) (Plan, error) {
			p, ok := in.Params.(fileChmodParams)
			if !ok {
				return Plan{}, fmt.Errorf("参数类型不匹配: %T", in.Params)
			}
			locks := []string{fileLock(p.RootID, p.Path)}
			plan, err := planCheck(ctx, src, p, p.RootID, p.Path, p.ExpectedVersion, locks,
				func(stat files.PathStat) ([]Notice, error) {
					if !stat.Exists {
						return []Notice{{Code: noticeTargetMissing,
							Message: fileKey(p.RootID, p.Path) + " 不存在"}}, nil
					}
					if stat.Type != files.TypeFile && stat.Type != files.TypeDir {
						return []Notice{{Code: noticeTargetNotRegular,
							Message: fmt.Sprintf("%s 是 %s，面板不修改它的权限", fileKey(p.RootID, p.Path), stat.Type)}}, nil
					}
					return nil, nil
				})
			if err != nil || !plan.Allowed {
				return plan, err
			}
			stat, err := src.Stat(ctx, p.RootID, p.Path)
			if err != nil {
				return Plan{}, err
			}
			if stat.Mode == p.Mode {
				plan.Warnings = append(plan.Warnings, Notice{Code: "NO_CHANGE",
					Message: fmt.Sprintf("%s 的权限已经是 %s，这次操作不会改变任何东西",
						fileKey(p.RootID, p.Path), p.Mode)})
			}
			plan.Impacts = []Impact{{Kind: "mode",
				Description: fmt.Sprintf("把 %s 的权限从 %s 改为 %s", fileKey(p.RootID, p.Path), stat.Mode, p.Mode)}}
			return plan, nil
		},
		Policy: ActionPolicy{
			// CONTRACTS.md 第 5 节：file.chmod 的 R 是“是”。
			RequiresReauth: true,
			RequiresPhrase: false,
			Cancellable:    false,
		},
		Execute: func(ctx context.Context, in ExecuteInput) (Result, error) {
			p, ok := in.Params.(fileChmodParams)
			if !ok {
				return Result{}, fmt.Errorf("参数类型不匹配: %T", in.Params)
			}
			res, err := src.Chmod(ctx, files.ChmodParams{
				RootID:          p.RootID,
				Path:            p.Path,
				ExpectedVersion: p.ExpectedVersion,
				Mode:            p.Mode,
			})
			if err != nil {
				return Result{}, executeError(err)
			}
			return Result{
				Outcome:       OutcomeVerified,
				VerifiedAt:    time.Now().UTC(),
				ObservedState: observedFromWrite(res),
				Data:          writeData(res),
			}, nil
		},
	}
}

// ───────────────────────────── file.chown ─────────────────────────────

// newFileChownAction 登记 file.chown。
//
// 账号模块（P08）还没落地，所以现在这个动作在预检阶段就是**阻塞**的：
// allowed=false + MANAGED_ACCOUNTS_UNAVAILABLE。这不是占位实现——它拒绝得明明白白，
// 也不会因为“暂时没有账号表”就退化成接受任意 uid/gid。
func newFileChownAction(src FileWriterSource) Action {
	return Action{
		Name: ActionFileChown,
		Decode: func(raw json.RawMessage) (any, error) {
			var p fileChownParams
			if err := DecodeParams(raw, &p); err != nil {
				return nil, err
			}
			if err := validateFileActionTarget(p.RootID, p.Path); err != nil {
				return nil, err
			}
			params := files.ChownParams{
				RootID:           p.RootID,
				Path:             p.Path,
				ExpectedVersion:  p.ExpectedVersion,
				ManagedAccountID: p.ManagedAccountID,
				ManagedGroupID:   p.ManagedGroupID,
			}
			if err := params.Validate(); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrParamsInvalid, err)
			}
			return p, nil
		},
		Plan: func(ctx context.Context, in PlanInput) (Plan, error) {
			p, ok := in.Params.(fileChownParams)
			if !ok {
				return Plan{}, fmt.Errorf("参数类型不匹配: %T", in.Params)
			}
			locks := []string{fileLock(p.RootID, p.Path)}
			if !src.AccountsAvailable() {
				return planBlocked(p, Target{
					DisplayName:         fileKey(p.RootID, p.Path),
					IdentityFingerprint: "file:" + fileKey(p.RootID, p.Path),
					StateVersion:        "unavailable",
				}, Notice{Code: noticeAccountsMissing,
					Message: "受管理账号模块尚未落地（P08），面板现在不能修改文件所有者；" +
						"这条动作在预检阶段就被拒绝，而不是接受任意 uid/gid"}, locks), nil
			}
			plan, err := planCheck(ctx, src, p, p.RootID, p.Path, p.ExpectedVersion, locks,
				func(stat files.PathStat) ([]Notice, error) {
					if !stat.Exists {
						return []Notice{{Code: noticeTargetMissing,
							Message: fileKey(p.RootID, p.Path) + " 不存在"}}, nil
					}
					if stat.Type != files.TypeFile && stat.Type != files.TypeDir {
						return []Notice{{Code: noticeTargetNotRegular,
							Message: fmt.Sprintf("%s 是 %s，面板不修改它的所有者", fileKey(p.RootID, p.Path), stat.Type)}}, nil
					}
					return nil, nil
				})
			if err != nil || !plan.Allowed {
				return plan, err
			}
			plan.Impacts = []Impact{{Kind: "owner",
				Description: fmt.Sprintf("修改 %s 的所有者（受管理账号 %d / 组 %d）",
					fileKey(p.RootID, p.Path), p.ManagedAccountID, p.ManagedGroupID)}}
			return plan, nil
		},
		Policy: ActionPolicy{
			// CONTRACTS.md 第 5 节：file.chown 的 R 是“是”。
			RequiresReauth: true,
			RequiresPhrase: false,
			Cancellable:    false,
		},
		Execute: func(ctx context.Context, in ExecuteInput) (Result, error) {
			p, ok := in.Params.(fileChownParams)
			if !ok {
				return Result{}, fmt.Errorf("参数类型不匹配: %T", in.Params)
			}
			res, err := src.Chown(ctx, files.ChownParams{
				RootID:           p.RootID,
				Path:             p.Path,
				ExpectedVersion:  p.ExpectedVersion,
				ManagedAccountID: p.ManagedAccountID,
				ManagedGroupID:   p.ManagedGroupID,
			})
			if err != nil {
				return Result{}, executeError(err)
			}
			return Result{
				Outcome:       OutcomeVerified,
				VerifiedAt:    time.Now().UTC(),
				ObservedState: observedFromWrite(res),
				Data:          writeData(res),
			}, nil
		},
	}
}

// validateFileActionTarget 做参数形状的最后一道检查：root_id 与相对路径必须能通过
// files 侧的同一套校验。
//
// 这里提前校验的理由与读取端一致：一次 `..` 或绝对路径应该在**解码**时就被拒绝，
// 而不是等它走到代理的句柄层。两边用同一组函数，所以不会出现两套宽容度。
func validateFileActionTarget(rootID, rel string) error {
	params := files.MkdirParams{RootID: rootID, Path: rel}
	if err := params.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrParamsInvalid, err)
	}
	return nil
}
