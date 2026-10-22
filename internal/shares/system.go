package shares

// 系统接缝：命令执行、文件系统只读探测、登记表与秘密解析（P08.1）。
//
// 这一层与 internal/storage 的做法一致：把“交给内核的动作”收在一个接口后面，
// 生产实现只有 syscmd 这一条路，单元测试可以喂进一段记录下来的命令序列。
// 这样做不是因为“方便测试”，而是因为这台开发机（macOS）上既没有 smbpasswd 也没有
// setfacl——不把边界画出来，唯一能跑的就是那台测试虚拟机，而生产逻辑的正确性
// 不该只由一次 VM 运行来担保。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"upanel/internal/storage"
	"upanel/internal/syscmd"
)

// CommandRunner 是把命令交给内核的唯一出口（生产实现转给 syscmd.Run）。
//
// syscmd.Run 已经保证了：不经 shell（argv 逐项传递）、环境白名单、标准输入只能是
// 代码里的固定内容、输出有上限、超时连同子进程组一起终止。
type CommandRunner interface {
	Run(ctx context.Context, spec syscmd.Spec, limits syscmd.Limits) (syscmd.Result, error)
}

// 账号与权限工具里的固定路径。
//
// 全部是常量：这些程序不接受配置、不来自 HTTP 参数。setfacl/getfacl 例外，它们由
// 能力探针在固定的候选列表里找到（候选也是常量），找到的绝对路径再传进来。
const (
	ProgramGetent  = "/usr/bin/getent"
	ProgramPasswd  = "/usr/bin/passwd"
	ProgramUseradd = "/usr/sbin/useradd"
	ProgramUserdel = "/usr/sbin/userdel"
)

// 标识前缀。与 vol_ 同一套形状：前缀 + 32 位小写十六进制。
const (
	shareIDPrefix   = "shr_"
	accountIDPrefix = "sac_"
	idHexLen        = 32
)

// SecretResolver 是密码秘密通道的消费端（生产实现是 agentstore.Secrets）。
//
// 只有两个动作：解析一份秘密（返回明文，调用方负责用完后清零）与给它挂上引用
// （把 10 分钟就销毁的临时秘密转成持久秘密）。**没有**“列出秘密”或“读回明文”
// 的通用接口——那会让秘密通道变成一块可以随便翻的存储。
type SecretResolver interface {
	Resolve(ctx context.Context, ref string) ([]byte, error)
	Reference(ctx context.Context, ref, reference string) error
}

// VolumeSource 提供受管理卷的现场视图（生产实现是 storage.Collector）。
//
// 共享目录必须落在一个**正在挂载**的受管理卷里：卷没挂时挂载点只是系统盘上的
// 一个空目录，在那里共享等于把根分区的一部分暴露出去（PLAN.md 第 10 节）。
type VolumeSource interface {
	Volume(ctx context.Context, id string) (storage.Volume, error)
}

// Options 是共享模块的装配参数。
type Options struct {
	// Store 是登记表（必需）。
	Store Store
	// Volumes 是受管理卷的现场视图（必需：没有它就无法判断卷挂没挂）。
	Volumes VolumeSource
	// Secrets 是密码秘密通道（share.account.apply 带 secret_ref 时必需）。
	Secrets SecretResolver
	// Runner 是命令执行器（必需）。
	Runner CommandRunner
	// Prober 是 Samba 能力探针（必需）。
	Prober *Prober
	// Now 提供时间（可注入）。
	Now func() time.Time
	// Lstat / ReadDir 是文件系统接缝（符号链接检查与“目录里有没有东西”）。
	Lstat   func(name string) (os.FileInfo, error)
	ReadDir func(name string) ([]os.DirEntry, error)
	// Mkdir 创建共享目录（生产实现 os.MkdirAll + 固定权限）。
	Mkdir func(name string, perm os.FileMode) error
}

// Engine 是共享模块的总入口。
type Engine struct {
	opts Options
	tool ToolPaths
}

// New 构造 Engine。缺必需项时返回错误：宁可启动时报错，也不要在第一次点击时
// 返回一个说不清原因的 503。
func New(opts Options) (*Engine, error) {
	if opts.Store == nil {
		return nil, errors.New("shares: 缺少登记表")
	}
	if opts.Volumes == nil {
		return nil, errors.New("shares: 缺少受管理卷视图")
	}
	if opts.Runner == nil {
		return nil, errors.New("shares: 缺少命令执行器")
	}
	if opts.Prober == nil {
		return nil, errors.New("shares: 缺少 Samba 能力探针")
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}
	if opts.Lstat == nil {
		opts.Lstat = os.Lstat
	}
	if opts.ReadDir == nil {
		opts.ReadDir = os.ReadDir
	}
	if opts.Mkdir == nil {
		opts.Mkdir = func(name string, perm os.FileMode) error { return os.MkdirAll(name, perm) }
	}
	return &Engine{opts: opts}, nil
}

// Capability 返回 Samba 能力报告（30 秒缓存）。
func (e *Engine) Capability(ctx context.Context) Capability {
	return e.opts.Prober.Capability(ctx)
}

// Shares 返回全部共享登记（按名字排序）。
func (e *Engine) Shares(ctx context.Context) ([]ShareRecord, error) {
	recs, err := e.opts.Store.ListShares(ctx)
	if err != nil {
		return nil, err
	}
	if recs == nil {
		recs = []ShareRecord{}
	}
	sortRecords(recs)
	return recs, nil
}

// Accounts 返回全部受管理账号（按名字排序）。
func (e *Engine) Accounts(ctx context.Context) ([]AccountRecord, error) {
	recs, err := e.opts.Store.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	if recs == nil {
		recs = []AccountRecord{}
	}
	sortAccounts(recs)
	return recs, nil
}

// Share 返回单条共享。
func (e *Engine) Share(ctx context.Context, id string) (ShareRecord, bool, error) {
	return e.opts.Store.GetShare(ctx, id)
}

// Account 返回单条账号。
func (e *Engine) Account(ctx context.Context, id string) (AccountRecord, bool, error) {
	return e.opts.Store.GetAccount(ctx, id)
}

// tools 返回当前探到的工具路径。
func (e *Engine) tools(ctx context.Context) ToolPaths {
	return e.Capability(ctx).Tools
}

// run 执行一条命令并返回结果。非零退出不算“执行失败”，由调用方按语义解释。
func (e *Engine) run(ctx context.Context, spec syscmd.Spec, timeout time.Duration) (syscmd.Result, error) {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return e.opts.Runner.Run(ctx, spec, syscmd.Limits{Timeout: timeout})
}

// volumeFor 取受管理卷的现场视图。
func (e *Engine) volumeFor(ctx context.Context, id string) (storage.Volume, bool, error) {
	vol, err := e.opts.Volumes.Volume(ctx, id)
	if err != nil {
		if errors.Is(err, storage.ErrVolumeNotFound) {
			return storage.Volume{}, false, nil
		}
		return storage.Volume{}, false, err
	}
	return vol, true, nil
}

// NewShareID 生成一个共享标识。
func NewShareID() (string, error) { return newID(shareIDPrefix) }

// NewAccountID 生成一个账号标识。
func NewAccountID() (string, error) { return newID(accountIDPrefix) }

func newID(prefix string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("生成 %s 标识失败: %w", prefix, err)
	}
	return prefix + hex.EncodeToString(b[:]), nil
}

// ValidShareID 判断共享标识的形状。它会进 URL、日志与审计，因此只接受固定形状。
func ValidShareID(id string) bool { return validID(id, shareIDPrefix) }

// ValidAccountID 判断账号标识的形状。
func ValidAccountID(id string) bool { return validID(id, accountIDPrefix) }

func validID(id, prefix string) bool {
	if len(id) != len(prefix)+idHexLen || !strings.HasPrefix(id, prefix) {
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

// sortRecords 按名字排序（同名再按 id，保证顺序稳定）。
func sortRecords(recs []ShareRecord) {
	for i := 1; i < len(recs); i++ {
		for j := i; j > 0; j-- {
			if recs[j].Definition.Name < recs[j-1].Definition.Name ||
				(recs[j].Definition.Name == recs[j-1].Definition.Name && recs[j].ID < recs[j-1].ID) {
				recs[j], recs[j-1] = recs[j-1], recs[j]
				continue
			}
			break
		}
	}
}

// sortAccounts 按名字排序。
func sortAccounts(recs []AccountRecord) {
	for i := 1; i < len(recs); i++ {
		for j := i; j > 0; j-- {
			if recs[j].Name < recs[j-1].Name ||
				(recs[j].Name == recs[j-1].Name && recs[j].ID < recs[j-1].ID) {
				recs[j], recs[j-1] = recs[j-1], recs[j]
				continue
			}
			break
		}
	}
}

// sameOrInside 判断 p 是否等于 root 或在 root 之内（两者都已 Clean）。
func sameOrInside(root, p string) bool {
	if p == root {
		return true
	}
	return strings.HasPrefix(p, root+"/")
}
