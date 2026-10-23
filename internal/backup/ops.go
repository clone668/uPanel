package backup

// 仓库预检与初始化（P12.1）。
//
// 预检回答四个问题，每个都要有证据：
//  1. 这台机器上有没有 restic / ssh？（没有就别让人点）
//  2. 两份秘密还取不取得到、用途对不对？（取不到就没有仓库口令，init 必然失败）
//  3. 目标位置上**有没有已经存在的仓库**？（只允许新空仓库，不覆盖已有数据）
//  4. 目标位置还有多少空间？（容量检测）
//
// 执行只做一件事：真正建仓库，然后**读回来核对**。核对的方式是 `restic cat config`：
// 它能读出仓库 id，而 id 是“这个仓库是谁”的唯一证据。建完不看回去，就等于把
// “restic 退出码 0”当成了“仓库可用”——那是本阶段明确拒绝的表述。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// 阻塞/告警码。稳定字符串，界面按它分类。
const (
	// CodeResticMissing 表示这台机器上没有 restic。
	CodeResticMissing = "BACKUP_RESTIC_MISSING"
	// CodeSSHMissing 表示没有 ssh 客户端，SFTP 后端跑不起来。
	CodeSSHMissing = "BACKUP_SSH_MISSING"
	// CodeSecretUnresolved 表示秘密引用取不到（已销毁、已过期、不存在）。
	CodeSecretUnresolved = "BACKUP_SECRET_UNRESOLVED"
	// CodeSecretPurpose 表示秘密用途不对（例如拿证书私钥当仓库口令）。
	CodeSecretPurpose = "BACKUP_SECRET_PURPOSE"
	// CodeRootUnknown 表示 root_id 不在这台机器的受管理根里。
	CodeRootUnknown = "BACKUP_ROOT_UNKNOWN"
	// CodeRepositoryExists 表示目标位置已经是一个 restic 仓库。
	CodeRepositoryExists = "BACKUP_REPOSITORY_EXISTS"
	// CodePathNotDir 表示目标路径存在但不是目录。
	CodePathNotDir = "BACKUP_PATH_NOT_DIR"
	// CodeHostKeyMismatch 表示 SFTP 主机密钥与登记的指纹不一致。
	CodeHostKeyMismatch = "BACKUP_HOST_KEY_MISMATCH"
	// CodeSFTPUnreachable 表示连不上 SFTP 服务器。
	CodeSFTPUnreachable = "BACKUP_SFTP_UNREACHABLE"
	// CodeSFTPAuth 表示 SSH 认证失败。
	CodeSFTPAuth = "BACKUP_SFTP_AUTH"
	// CodeNoSpace 表示目标文件系统没有可用空间。
	CodeNoSpace = "BACKUP_NO_SPACE"
	// CodeCapacityUnknown 表示读不到容量（不是 0，是不知道）。
	CodeCapacityUnknown = "BACKUP_CAPACITY_UNKNOWN"
	// CodeLowSpace 表示可用空间偏少（只告警）。
	CodeLowSpace = "BACKUP_SPACE_LOW"
	// CodeResticVersionUnknown 表示读不到 restic 版本（只告警：不影响 init）。
	CodeResticVersionUnknown = "BACKUP_RESTIC_VERSION_UNKNOWN"
)

// ImpactKind 是影响的类型。
const (
	// ImpactCreateRepository 表示“会在目标位置新建一个仓库”。
	ImpactCreateRepository = "create_repository"
	// ImpactSecretUse 表示“会读取仓库口令与（SFTP）私钥”。
	ImpactSecretUse = "secret_use"
)

// LowSpaceWarningBytes 是“空间偏少”的告警线（1 GiB）。
//
// 它只是告警：小仓库（几十个配置文件）几百兆就够。真正的阻塞条件是“可用空间为 0”。
const LowSpaceWarningBytes int64 = 1 << 30

// InitializeTimeout 是 restic init 的上限（SFTP 上要建目录、写密钥环与 config）。
const InitializeTimeout = 5 * time.Minute

// Reason 是一条阻塞或告警。
type Reason struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Impact 是这次操作的后果。
type Impact struct {
	Kind        string `json:"kind"`
	Description string `json:"description"`
}

// Target 与代理的 Plan.Target 同形。
type Target struct {
	DisplayName         string
	IdentityFingerprint string
	StateVersion        string
}

// RootSource 把文件根标识解析成绝对路径。
//
// 它由装配层实现（读的是与文件模块同一份配置）：仓库只能落在**已登记的文件根**
// 里面，和文件动作受同一套边界约束。
type RootSource interface {
	// ResolveRoot 返回根的绝对路径；unknown 表示这个 id 没登记。
	ResolveRoot(rootID string) (path string, writable bool, unknown bool)
}

// SecretStore 是秘密存储的读取端。
type SecretStore interface {
	// Resolve 解密一条秘密。
	Resolve(ctx context.Context, ref string) ([]byte, error)
	// Purpose 返回一条秘密的用途；不存在时 found=false。
	Purpose(ctx context.Context, ref string) (purpose string, found bool, err error)
}

// Options 是 Ops 的装配参数。
type Options struct {
	// Restic 必需。
	Restic *Restic
	// Roots 必需（本地后端要它把 root_id 变成路径）。
	Roots RootSource
	// Secrets 必需。
	Secrets SecretStore
	// Paths 覆盖 Restic 里的路径（诊断与测试用）。
	Paths Paths
	// Now 供测试固定时间。
	Now func() time.Time
	// Stat 判断文件存在（默认 os.Stat）。
	Stat func(string) bool
}

// Ops 是仓库动作的实现。
type Ops struct {
	opts Options
}

// NewOps 构造实现。缺必需项时直接报错——宁可启动失败，也不要第一次点击才说不清。
func NewOps(opts Options) (*Ops, error) {
	if opts.Restic == nil {
		return nil, errors.New("backup: 缺少 restic 调用器")
	}
	if opts.Roots == nil {
		return nil, errors.New("backup: 缺少受管理根登记表")
	}
	if opts.Secrets == nil {
		return nil, errors.New("backup: 缺少秘密存储读取端")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Stat == nil {
		opts.Stat = fileExists
	}
	if opts.Paths.Restic == "" {
		opts.Paths = opts.Restic.Paths()
	}
	return &Ops{opts: opts}, nil
}

// AssessRequest 是一次预检的输入。
type AssessRequest struct {
	RepositoryID string
	Definition   Definition
	// Version 是仓库记录在代理镜像里的版本号；绑定进状态版本。
	Version int64
	// ExcludeOperationID 未使用（保留给“并发备份运行时拒绝”的 P12.2）。
	ExcludeOperationID string
}

// Assessment 是一次预检的结论。
type Assessment struct {
	RepositoryID string
	Definition   Definition
	// AbsolutePath 是本地仓库解析后的绝对路径（SFTP 时为空）。
	AbsolutePath  string
	Allowed       bool
	Blockers      []Reason
	Warnings      []Reason
	Impacts       []Impact
	Locks         []string
	Target        Target
	Capacity      Capacity
	HostKey       HostKey
	ResticVersion string
	CheckedAt     time.Time
}

// Assess 生成预检结论。
func (o *Ops) Assess(ctx context.Context, req AssessRequest) (Assessment, error) {
	def, err := req.Definition.Normalize()
	if err != nil {
		return Assessment{}, err
	}
	now := o.opts.Now().UTC()
	a := Assessment{
		RepositoryID: req.RepositoryID,
		Definition:   def,
		Allowed:      true,
		Locks:        []string{LockName(req.RepositoryID)},
		CheckedAt:    now,
		Impacts: []Impact{
			{Kind: ImpactCreateRepository,
				Description: "在 " + def.Location.String(def.Backend) + " 新建一个加密仓库；不会删除该位置上已有的任何数据"},
			{Kind: ImpactSecretUse,
				Description: "会读取仓库口令" + sftpCredentialNote(def) + "；面板不回传任何明文"},
		},
	}

	fingerprint, err := def.IdentityFingerprint()
	if err != nil {
		return Assessment{}, err
	}
	a.Target = Target{
		DisplayName:         def.DisplayName(),
		IdentityFingerprint: fingerprint,
		StateVersion:        fmt.Sprintf("backup-repository:%s:v%d", req.RepositoryID, req.Version),
	}

	if ok, why := o.opts.Restic.Available(o.opts.Stat); !ok {
		a.block(CodeResticMissing, "这台机器上没有可用的 restic："+why+"（PLAN 第 17 节要求 1.0 统一使用 restic）")
	} else if version, err := o.opts.Restic.Version(ctx); err != nil {
		a.warn(CodeResticVersionUnknown, "读不到 restic 版本，仍可尝试初始化："+err.Error())
	} else {
		a.ResticVersion = version
	}

	if err := o.checkSecrets(ctx, &a); err != nil {
		return Assessment{}, err
	}

	switch def.Backend {
	case BackendLocal:
		o.assessLocal(&a)
	case BackendSFTP:
		o.assessSFTP(ctx, &a)
	}
	return a, nil
}

// sftpCredentialNote 给影响描述补一句（只在 SFTP 后端上有）。
func sftpCredentialNote(def Definition) string {
	if def.Backend == BackendSFTP {
		return "与 SFTP 私钥（私钥只写到 tmpfs 上的临时文件，用完即删）"
	}
	return ""
}

// checkSecrets 校验两份秘密：存在、用途对、能解密。
//
// 三件事必须一起做：只查“能解密”而不查用途，等于允许拿一张 TLS 私钥当仓库口令；
// 只查用途而不查解密，会让一个已销毁的引用一路走到执行阶段才发现。
func (o *Ops) checkSecrets(ctx context.Context, a *Assessment) error {
	def := a.Definition
	if err := o.checkSecret(ctx, a, def.PasswordSecretRef, "backup_repository_password", "仓库口令"); err != nil {
		return err
	}
	if def.Backend == BackendSFTP {
		if err := o.checkSecret(ctx, a, def.CredentialSecretRef, "sftp_private_key", "SFTP 私钥"); err != nil {
			return err
		}
	}
	return nil
}

func (o *Ops) checkSecret(ctx context.Context, a *Assessment, ref, wantPurpose, label string) error {
	purpose, found, err := o.opts.Secrets.Purpose(ctx, ref)
	if err != nil {
		return err
	}
	if !found {
		a.block(CodeSecretUnresolved, fmt.Sprintf("%s的秘密引用 %s 不存在（可能已销毁）", label, ref))
		return nil
	}
	if purpose != wantPurpose {
		a.block(CodeSecretPurpose, fmt.Sprintf("%s的秘密引用 %s 用途是 %s，应为 %s", label, ref, purpose, wantPurpose))
		return nil
	}
	plaintext, err := o.opts.Secrets.Resolve(ctx, ref)
	if err != nil {
		a.block(CodeSecretUnresolved, fmt.Sprintf("%s的秘密引用 %s 取不到：%v", label, ref, err))
		return nil
	}
	wipe(plaintext)
	return ctx.Err()
}

// assessLocal 校验本地位置。
func (o *Ops) assessLocal(a *Assessment) {
	def := a.Definition
	rootPath, writable, unknown := o.opts.Roots.ResolveRoot(def.Location.RootID)
	if unknown {
		a.block(CodeRootUnknown, fmt.Sprintf("文件根 %s 没有登记在这台机器上（仓库只能落在受管理根里）", def.Location.RootID))
		return
	}
	if !writable {
		a.block(CodeRootUnknown, fmt.Sprintf("文件根 %s 不是可写根，不能作为仓库位置", def.Location.RootID))
		return
	}
	abs := joinRoot(rootPath, def.Location.RelativePath)
	a.AbsolutePath = abs

	probe, err := ProbeLocal(abs)
	if err != nil {
		a.block(CodeRootUnknown, err.Error())
		return
	}
	a.Capacity = probe.Capacity
	if probe.Exists && !probe.IsDir {
		a.block(CodePathNotDir, fmt.Sprintf("%s 已存在且不是目录", abs))
		return
	}
	if probe.RepoConfig {
		a.block(CodeRepositoryExists, fmt.Sprintf("%s 已经是一个 restic 仓库；这里只初始化新空仓库，不覆盖已有数据", abs))
	}
	o.capacityVerdict(a)
}

// assessSFTP 校验远端位置：主机密钥、认证、路径、容量。
func (o *Ops) assessSFTP(ctx context.Context, a *Assessment) {
	def := a.Definition
	if !o.opts.Stat(o.opts.Paths.SSH) {
		a.block(CodeSSHMissing, fmt.Sprintf("找不到 ssh 客户端 %s：restic 的 sftp 后端需要它", o.opts.Paths.SSH))
		return
	}
	registered, err := ParseFingerprint(def.Location.HostKeyFingerprint)
	if err != nil {
		a.block(CodeHostKeyMismatch, err.Error())
		return
	}
	key, err := o.opts.Secrets.Resolve(ctx, def.CredentialSecretRef)
	if err != nil {
		// 上面已经报过；这里只是不能继续连。
		return
	}
	defer wipe(key)

	client, hostKey, err := Dial(ctx, DialOptions{
		Host:       def.Location.Host,
		Port:       def.Location.Port,
		User:       def.Location.Username,
		PrivateKey: key,
		Timeout:    DefaultDialTimeout,
	}, registered)
	if err != nil {
		switch {
		case errors.Is(err, ErrHostKeyMismatch):
			a.block(CodeHostKeyMismatch, err.Error())
		case errors.Is(err, ErrSSHAuth):
			a.block(CodeSFTPAuth, err.Error())
		default:
			a.block(CodeSFTPUnreachable, err.Error())
		}
		return
	}
	a.HostKey = hostKey
	defer func() { _ = client.Close() }()

	probe, err := ProbeRemote(ctx, client, def.Location.RemotePath)
	if err != nil {
		a.block(CodeSFTPUnreachable, err.Error())
		return
	}
	a.Capacity = probe.Capacity
	if probe.Exists && !probe.IsDir {
		a.block(CodePathNotDir, fmt.Sprintf("远端 %s 已存在且不是目录", def.Location.RemotePath))
		return
	}
	if probe.RepoConfig {
		a.block(CodeRepositoryExists, fmt.Sprintf("远端 %s 已经是一个 restic 仓库；这里只初始化新空仓库，不覆盖已有数据", def.Location.RemotePath))
	}
	if !probe.Exists {
		a.warn(CodeCapacityUnknown, "远端目录还不存在，初始化时会新建；它的父目录必须可写")
	}
	o.capacityVerdict(a)
}

// capacityVerdict 把容量读数变成阻塞或告警。
func (o *Ops) capacityVerdict(a *Assessment) {
	cap := a.Capacity
	if !cap.Supported {
		reason := cap.Error
		if reason == "" {
			reason = "没有读数"
		}
		a.warn(CodeCapacityUnknown, "读不到目标位置的容量："+reason)
		return
	}
	if cap.AvailableBytes <= 0 {
		a.block(CodeNoSpace, fmt.Sprintf("%s 所在文件系统没有可用空间（总量 %d 字节）", cap.Target, cap.TotalBytes))
		return
	}
	if cap.AvailableBytes < LowSpaceWarningBytes {
		a.warn(CodeLowSpace, fmt.Sprintf("%s 只剩 %d 字节可用（总量 %d 字节）", cap.Target, cap.AvailableBytes, cap.TotalBytes))
	}
}

// block 记一条阻塞项，并把结论置为不允许。
func (a *Assessment) block(code, message string) {
	a.Allowed = false
	a.Blockers = append(a.Blockers, Reason{Code: code, Message: message})
}

// warn 记一条告警项。
func (a *Assessment) warn(code, message string) {
	for _, w := range a.Warnings {
		if w.Code == code {
			return
		}
	}
	a.Warnings = append(a.Warnings, Reason{Code: code, Message: message})
}

// LockName 是仓库的资源锁名。
func LockName(repositoryID string) string {
	return "backup-repo:" + repositoryID
}

// ExecuteRequest 是一次初始化的输入。
type ExecuteRequest struct {
	RepositoryID string
	Definition   Definition
	OperationID  string
	Progress     func(step string, fields map[string]any)
	Cancel       <-chan struct{}
}

// Execution 是一次初始化的结果。
type Execution struct {
	RepositoryID string
	AbsolutePath string
	// RepoID 是 restic 报告的仓库 id；只有它非空才说明仓库真的建成了。
	RepoID         string
	RepoVersion    int
	ResticVersion  string
	HostKey        HostKey
	CapacityBefore Capacity
	CapacityAfter  Capacity
	InitializedAt  time.Time
	Duration       time.Duration
	// Verified 表示 `restic cat config` 真的读回来了。
	Verified bool
	// VerifyError 是核对失败的原因（Verified 为 false 时非空）。
	VerifyError string
	// Command 是执行的 argv（不含口令，审计用）。
	Command []string
	// KnownHostsPath 是这次写出的 known_hosts（SFTP）。
	KnownHostsPath string
}

// Execute 初始化仓库。
func (o *Ops) Execute(ctx context.Context, req ExecuteRequest) (Execution, error) {
	def, err := req.Definition.Normalize()
	if err != nil {
		return Execution{}, err
	}
	out := Execution{
		RepositoryID:  req.RepositoryID,
		RepoVersion:   1,
		InitializedAt: o.opts.Now().UTC(),
	}
	progress := req.Progress
	if progress == nil {
		progress = func(string, map[string]any) {}
	}

	if ok, why := o.opts.Restic.Available(o.opts.Stat); !ok {
		return out, fmt.Errorf("%s：%s", CodeResticMissing, why)
	}
	if version, err := o.opts.Restic.Version(ctx); err == nil {
		out.ResticVersion = version
	}

	password, err := o.opts.Secrets.Resolve(ctx, def.PasswordSecretRef)
	if err != nil {
		return out, fmt.Errorf("%s：取不到仓库口令: %w", CodeSecretUnresolved, err)
	}
	defer wipe(password)

	inv := Invocation{
		Password: string(password),
		Timeout:  InitializeTimeout,
	}

	if err := ctxCancelled(ctx, req.Cancel); err != nil {
		return out, err
	}

	switch def.Backend {
	case BackendLocal:
		rootPath, _, unknown := o.opts.Roots.ResolveRoot(def.Location.RootID)
		if unknown {
			return out, fmt.Errorf("%s：文件根 %s 没有登记", CodeRootUnknown, def.Location.RootID)
		}
		abs := joinRoot(rootPath, def.Location.RelativePath)
		out.AbsolutePath = abs
		if probe, err := ProbeLocal(abs); err == nil {
			out.CapacityBefore = probe.Capacity
		}
		progress("prepare", map[string]any{"path": abs})
		if err := EnsureDir(abs); err != nil {
			return out, err
		}
		inv.Repository = def.resticRepository(abs)
	case BackendSFTP:
		inv, out, err = o.prepareSFTP(ctx, req, def, password, inv, out, progress)
		if err != nil {
			return out, err
		}
		defer RemovePrivateKey(sftpKeyPathOf(inv))
	}

	if err := ctxCancelled(ctx, req.Cancel); err != nil {
		return out, err
	}
	out.Command = o.opts.Restic.args(inv, "init")

	progress("init", map[string]any{"repository": inv.Repository})
	initRes, err := o.opts.Restic.Init(ctx, inv)
	if err != nil {
		return out, err
	}
	progress("init-done", map[string]any{"duration_ms": initRes.Duration.Milliseconds()})

	// 读回来核对：建完不看回去，等于把退出码当成结论。
	progress("verify", nil)
	cfg, err := o.opts.Restic.CatConfig(ctx, inv)
	if err != nil {
		out.VerifyError = failureMessage(err)
		out.CapacityAfter = o.probeCapacity(ctx, req, def, inv)
		return out, nil
	}
	out.Verified = true
	out.RepoID = cfg.ID
	if cfg.Version > 0 {
		out.RepoVersion = cfg.Version
	}
	out.CapacityAfter = o.probeCapacity(ctx, req, def, inv)
	return out, nil
}

// StateRequest 是仓库现状查询的输入。
type StateRequest struct {
	RepositoryID string
	Definition   Definition
	Version      int64
}

// RepositoryState 是仓库在**这台机器上**的现状。
//
// 它只报事实，不报结论：`initialized` 是“目标位置已经有 restic 的 config”，
// `repo_id` 是 `restic cat config` 读回来的仓库标识。两者都读不到时给出原因，
// 而不是把“读不到”显示成“没有”。
type RepositoryState struct {
	RepositoryID  string  `json:"repository_id"`
	Name          string  `json:"name"`
	Backend       Backend `json:"backend"`
	Location      string  `json:"location"`
	Version       int64   `json:"version"`
	AbsolutePath  string  `json:"absolute_path,omitempty"`
	ResticVersion string  `json:"restic_version,omitempty"`
	ResticMissing string  `json:"restic_missing,omitempty"`
	// Initialized 表示目标位置已经是一个 restic 仓库。
	//
	// 三态：true/false 是**读到的事实**，null 是**没读到**（连不上、探测失败、
	// 秘密取不到）。这里不用 false 表示“没读到”——那会让界面把“查不了”显示成
	// “没有仓库”，和把容量写成 0 是同一类谎（DECISIONS Q330/Q333）。
	Initialized     *bool    `json:"initialized"`
	RepoID          string   `json:"repo_id,omitempty"`
	RepoVersion     int      `json:"repo_version,omitempty"`
	RepoConfigError string   `json:"repo_config_error,omitempty"`
	Capacity        Capacity `json:"capacity"`
	// HostKeyVerified 表示这次连接用登记的指纹核对通过（SFTP）。
	HostKeyVerified bool   `json:"host_key_verified"`
	HostKey         string `json:"host_key,omitempty"`
	HostKeyError    string `json:"host_key_error,omitempty"`
	// ProbeError 是“这条定义本身没法拿来探测”的原因（例如受管理根不可用）。
	// 它不写进 server 日志就算了，而是回到调用方——原因被吞掉与读不到是两件事。
	ProbeError string    `json:"probe_error,omitempty"`
	CheckedAt  time.Time `json:"checked_at"`
}

// boolPtr 让“读到的事实”能落进三态字段。
func boolPtr(v bool) *bool { return &v }

// isInitialized 只回答“读到的那个值是不是 true”，没读到一律 false。
func (s RepositoryState) isInitialized() bool {
	return s.Initialized != nil && *s.Initialized
}

// State 读仓库现状（只读，不建、不写）。
func (o *Ops) State(ctx context.Context, req StateRequest) (RepositoryState, error) {
	def, err := req.Definition.Normalize()
	if err != nil {
		return RepositoryState{}, err
	}
	out := RepositoryState{
		RepositoryID: req.RepositoryID,
		Name:         def.Name,
		Backend:      def.Backend,
		Location:     def.Location.String(def.Backend),
		Version:      req.Version,
		CheckedAt:    o.opts.Now().UTC(),
	}
	if ok, why := o.opts.Restic.Available(o.opts.Stat); !ok {
		out.ResticMissing = why
		return out, nil
	}
	if version, err := o.opts.Restic.Version(ctx); err == nil {
		out.ResticVersion = version
	}

	inv, cleanup, err := o.stateInvocation(ctx, &out, def)
	if err != nil {
		// 原因回到调用方，不再吞掉：以前这里 `return out, nil` 把“受管理根不可用”
		// 这句构造好的话直接扔了，界面只会看到 initialized=false。
		out.ProbeError = err.Error()
		return out, nil
	}
	if cleanup != nil {
		defer cleanup()
	}
	if inv.Repository == "" {
		return out, nil
	}
	if out.isInitialized() {
		cfg, err := o.opts.Restic.CatConfig(ctx, inv)
		if err != nil {
			out.RepoConfigError = failureMessage(err)
		} else {
			out.RepoID = cfg.ID
			out.RepoVersion = cfg.Version
		}
	}
	return out, nil
}

// stateInvocation 探测目标位置，并准备好“读仓库 id”所需的调用参数。
//
// 返回的 cleanup 负责删掉 SFTP 的临时私钥；调用方必须 defer。
func (o *Ops) stateInvocation(ctx context.Context, out *RepositoryState, def Definition) (Invocation, func(), error) {
	switch def.Backend {
	case BackendLocal:
		rootPath, writable, unknown := o.opts.Roots.ResolveRoot(def.Location.RootID)
		if unknown || !writable {
			return Invocation{}, nil, fmt.Errorf("%s：文件根 %s 不可用", CodeRootUnknown, def.Location.RootID)
		}
		abs := joinRoot(rootPath, def.Location.RelativePath)
		out.AbsolutePath = abs
		probe, err := ProbeLocal(abs)
		if err != nil {
			return Invocation{}, nil, err
		}
		out.Capacity = probe.Capacity
		out.Initialized = boolPtr(probe.RepoConfig)
		password, err := o.opts.Secrets.Resolve(ctx, def.PasswordSecretRef)
		if err != nil {
			out.RepoConfigError = CodeSecretUnresolved + "：取不到仓库口令"
			return Invocation{}, nil, nil
		}
		defer wipe(password)
		return Invocation{Repository: def.resticRepository(abs), Password: string(password)}, nil, nil
	case BackendSFTP:
		if !o.opts.Stat(o.opts.Paths.SSH) {
			out.HostKeyError = fmt.Sprintf("找不到 ssh 客户端 %s", o.opts.Paths.SSH)
			return Invocation{}, nil, nil
		}
		registered, err := ParseFingerprint(def.Location.HostKeyFingerprint)
		if err != nil {
			out.HostKeyError = err.Error()
			return Invocation{}, nil, nil
		}
		key, err := o.opts.Secrets.Resolve(ctx, def.CredentialSecretRef)
		if err != nil {
			out.HostKeyError = CodeSecretUnresolved + "：取不到 SFTP 私钥"
			return Invocation{}, nil, nil
		}
		defer wipe(key)
		client, hostKey, err := Dial(ctx, DialOptions{
			Host:       def.Location.Host,
			Port:       def.Location.Port,
			User:       def.Location.Username,
			PrivateKey: key,
			Timeout:    DefaultDialTimeout,
		}, registered)
		if err != nil {
			out.HostKeyError = err.Error()
			return Invocation{}, nil, nil
		}
		out.HostKeyVerified = true
		out.HostKey = hostKey.Type + " " + hostKey.Fingerprint
		probe, perr := ProbeRemote(ctx, client, def.Location.RemotePath)
		_ = client.Close()
		if perr != nil {
			out.RepoConfigError = perr.Error()
			return Invocation{}, nil, nil
		}
		out.Capacity = probe.Capacity
		out.Initialized = boolPtr(probe.RepoConfig)

		password, err := o.opts.Secrets.Resolve(ctx, def.PasswordSecretRef)
		if err != nil {
			out.RepoConfigError = CodeSecretUnresolved + "：取不到仓库口令"
			return Invocation{}, nil, nil
		}
		defer wipe(password)
		key2, err := o.opts.Secrets.Resolve(ctx, def.CredentialSecretRef)
		if err != nil {
			out.RepoConfigError = CodeSecretUnresolved + "：取不到 SFTP 私钥"
			return Invocation{}, nil, nil
		}
		defer wipe(key2)
		// 只读查询**不写** known_hosts：那是初始化的副作用。没有它就说明这个仓库
		// 还没被本机初始化过，此时读不到仓库 id 是事实，不是错误。
		knownHostsPath := o.opts.Restic.knownHostsPath(def.Location.Host, def.Location.Port)
		if !o.opts.Stat(knownHostsPath) {
			if out.isInitialized() {
				out.RepoConfigError = "远端已经是仓库，但本机还没有它的 known_hosts（先做一次初始化预检）"
			}
			return Invocation{}, nil, nil
		}
		keyPath, err := o.opts.Restic.WritePrivateKey("state:"+out.RepositoryID, key2)
		if err != nil {
			out.RepoConfigError = err.Error()
			return Invocation{}, nil, nil
		}
		cleanup := func() { RemovePrivateKey(keyPath) }
		return Invocation{
			Repository: def.resticRepository(""),
			Password:   string(password),
			SFTPCommand: SFTPCommand(o.opts.Paths.SSH, knownHostsPath, keyPath,
				def.Location.Host, def.Location.Port, def.Location.Username),
		}, cleanup, nil
	}
	return Invocation{}, nil, fmt.Errorf("backup: 未知后端 %q", def.Backend)
}

// probeCapacity 在初始化之后读一次容量（用于“容量检测”的前后对比）。
func (o *Ops) probeCapacity(ctx context.Context, req ExecuteRequest, def Definition, inv Invocation) Capacity {
	if def.Backend == BackendLocal {
		if probe, err := ProbeLocal(inv.Repository); err == nil {
			return probe.Capacity
		}
		return Capacity{}
	}
	client, _, err := o.dialSFTP(ctx, req, def)
	if err != nil {
		return Capacity{}
	}
	defer func() { _ = client.Close() }()
	if probe, err := ProbeRemote(ctx, client, def.Location.RemotePath); err == nil {
		return probe.Capacity
	}
	return Capacity{}
}

// prepareSFTP 做 SFTP 分支的执行期准备：核对主机密钥、写 known_hosts、写临时私钥。
func (o *Ops) prepareSFTP(ctx context.Context, req ExecuteRequest, def Definition, password []byte,
	inv Invocation, out Execution, progress func(string, map[string]any)) (Invocation, Execution, error) {
	if !o.opts.Stat(o.opts.Paths.SSH) {
		return inv, out, fmt.Errorf("%s：找不到 ssh 客户端 %s", CodeSSHMissing, o.opts.Paths.SSH)
	}
	registered, err := ParseFingerprint(def.Location.HostKeyFingerprint)
	if err != nil {
		return inv, out, err
	}
	key, err := o.opts.Secrets.Resolve(ctx, def.CredentialSecretRef)
	if err != nil {
		return inv, out, fmt.Errorf("%s：取不到 SFTP 私钥: %w", CodeSecretUnresolved, err)
	}
	defer wipe(key)

	// 执行期**再核对一次**：预检到执行之间主机可能换了一把钥匙（或者中间有人）。
	progress("verify-host-key", map[string]any{"host": def.Location.Host, "port": def.Location.Port})
	client, hostKey, err := Dial(ctx, DialOptions{
		Host:       def.Location.Host,
		Port:       def.Location.Port,
		User:       def.Location.Username,
		PrivateKey: key,
		Timeout:    DefaultDialTimeout,
	}, registered)
	if err != nil {
		return inv, out, err
	}
	out.HostKey = hostKey
	if probe, perr := ProbeRemote(ctx, client, def.Location.RemotePath); perr == nil {
		out.CapacityBefore = probe.Capacity
	}
	_ = client.Close()

	knownHostsPath, err := o.opts.Restic.WriteKnownHosts(def.Location.Host, def.Location.Port,
		KnownHostsLine(def.Location.Host, def.Location.Port, hostKey.PublicKey))
	if err != nil {
		return inv, out, err
	}
	out.KnownHostsPath = knownHostsPath

	keyPath, err := o.opts.Restic.WritePrivateKey(req.OperationID, key)
	if err != nil {
		return inv, out, err
	}
	inv.SFTPCommand = SFTPCommand(o.opts.Paths.SSH, knownHostsPath, keyPath,
		def.Location.Host, def.Location.Port, def.Location.Username)
	inv.Repository = def.resticRepository("")
	return inv, out, nil
}

// dialSFTP 是执行后读容量时的一次短连接。
func (o *Ops) dialSFTP(ctx context.Context, req ExecuteRequest, def Definition) (*ssh.Client, HostKey, error) {
	registered, err := ParseFingerprint(def.Location.HostKeyFingerprint)
	if err != nil {
		return nil, HostKey{}, err
	}
	key, err := o.opts.Secrets.Resolve(ctx, def.CredentialSecretRef)
	if err != nil {
		return nil, HostKey{}, err
	}
	defer wipe(key)
	return Dial(ctx, DialOptions{
		Host:       def.Location.Host,
		Port:       def.Location.Port,
		User:       def.Location.Username,
		PrivateKey: key,
		Timeout:    DefaultDialTimeout,
	}, registered)
}

// sftpKeyPathOf 从 sftp.command 里取出私钥路径（用于收尾删除）。
func sftpKeyPathOf(inv Invocation) string {
	fields := strings.Fields(inv.SFTPCommand)
	for i, f := range fields {
		if f == "-i" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return ""
}

// joinRoot 拼接根的绝对路径与相对路径。
func joinRoot(rootPath, relative string) string {
	return strings.TrimSuffix(rootPath, "/") + "/" + relative
}

// wipe 清零秘密字节。
func wipe(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// ctxCancelled 把 ctx 与取消通道合成一个检查点。
// ErrCanceled 表示这次操作在可中断点收到了安全取消请求。
//
// 动作层靠 errors.Is 认出它，把任务收成 canceled 而不是 failed：一次被管理员主动
// 取消的操作不是错误，把它记成失败会让“谁做的”变成“谁弄坏的”。
var ErrCanceled = errors.New("backup: 操作已取消")

func ctxCancelled(ctx context.Context, cancel <-chan struct{}) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: %w", ErrCanceled, err)
	}
	if cancel != nil {
		select {
		case <-cancel:
			return ErrCanceled
		default:
		}
	}
	return nil
}

// failureMessage 把错误翻成一句人话（失败任务的 error.message 用它）。
func failureMessage(err error) string {
	var reason interface{ FailureReason() string }
	if errors.As(err, &reason) {
		if s := strings.TrimSpace(reason.FailureReason()); s != "" {
			return s
		}
	}
	return strings.TrimSpace(err.Error())
}
