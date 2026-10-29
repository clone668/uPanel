package shares

// smbd 的开机依赖（P08.3）。
//
// 问题很具体：开机时 systemd 按自己的顺序拉起单元。smbd 与“这块卷的挂载单元”之间
// 没有任何关系，所以完全可能 smbd 先起来、卷后挂上——那一小段时间里共享指向的是
// 系统盘上的空目录，客户端连上去看到的是**一个空文件夹**。等卷挂上之后，路径背后的
// 文件系统换了，smbd 手里还是那个路径，于是又正常了。这种“开机后几秒钟内看到空目录”
// 的故障最难被报告：它自己会好，日志里只有一次正常的启动。
//
// 修法不是循环检测，而是把顺序告诉 systemd：给 smbd.service 写一份 drop-in，
// 用 `RequiresMountsFor=` 声明“这些路径所在的挂载单元必须先就位”。systemd 会把
// 它翻译成对相应 `.mount` 单元的 Requires= 与 After=：卷挂不上，smbd 就不启动
// （Requires 的语义），而不是启动了去服务一个空目录。
//
// 这份 drop-in 和托管配置一样归面板独占、整份重写、内容由登记表算出来。

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"upanel/internal/storage"
	"upanel/internal/syscmd"
)

const (
	// StartupDropinPath 是 smbd 的开机依赖 drop-in。
	//
	// 放在 smbd.service.d 下是 systemd 的既定约定：不修改发行版自带的单元文件
	// （那会被包升级覆盖），只在旁边加一份只属于自己的片段。
	StartupDropinPath = "/etc/systemd/system/smbd.service.d/upanel-shares.conf"

	// ExportsStartupDropinPath 是 nfs-server 的开机依赖 drop-in（P14.4）。
	//
	// 同一个理由、另一份单元：nfs-server 与卷的 .mount 单元之间也没有任何先后关系。
	// 它比 smbd 更在意这件事——NFS 的导出是“路径 + 客户端”，路径背后没挂上时
	// 客户端读到的是系统盘上的空目录，而且不会报错。
	ExportsStartupDropinPath = "/etc/systemd/system/nfs-server.service.d/upanel-exports.conf"

	// startupDepDirMode / startupDepFileMode 是 drop-in 的权限。
	// 0644：systemd 以 root 读它，管理员也该能读；没有可写位——只有面板写。
	startupDepDirMode  os.FileMode = 0o755
	startupDepFileMode os.FileMode = 0o644

	// startupDepTimeout 是一次 systemctl daemon-reload 的预算。
	// 写 drop-in 不重载，systemd 根本不知道它存在，所以这一步不是可选的。
	startupDepTimeout = 20 * time.Second
)

// 三种可能的写入结果（与报告里的 startup_deps 一一对应）。
const (
	StartupDepsWritten   = "written"
	StartupDepsUnchanged = "unchanged"
	StartupDepsRemoved   = "removed"
	StartupDepsFailed    = "failed"
	// StartupDepsSkipped 表示当前不需要它（没有要共享的卷），且本来也没有文件。
	StartupDepsSkipped = "skipped"
)

// RenderStartupDropin 渲染 drop-in 的内容。
//
// 排序后输出，理由是渲染必须确定性：同一组路径永远渲染出同一串字节，
// 否则每次应用都会“内容变了”，于是每次都要 daemon-reload 一遍。
//
// 路径来自面板自己按 slug 生成的挂载点，不含空格与引号；但这里仍然拒绝这类取值，
// 因为 `RequiresMountsFor=` 是**按空白切分**的列表——一个带空格的路径会静默变成
// 两个路径，其中一个还不存在，drop-in 于是要求一个永远挂不上的单元。
func RenderStartupDropin(mountPaths []string) (string, error) {
	paths := append([]string(nil), mountPaths...)
	sort.Strings(paths)
	paths = dedupeStrings(paths)
	for _, p := range paths {
		if !strings.HasPrefix(p, "/") {
			return "", fmt.Errorf("开机依赖的路径必须是绝对路径：%q", p)
		}
		if strings.ContainsAny(p, " \t\n\\\"") {
			return "", fmt.Errorf("开机依赖的路径不能含空白或引号（RequiresMountsFor 按空白切分）：%q", p)
		}
		if path.Clean(p) != p {
			return "", fmt.Errorf("开机依赖的路径不是规范形式：%q", p)
		}
	}
	var b strings.Builder
	b.WriteString("# 由 uPanel 生成，每次应用共享时整份重写：手工修改会在下次应用时被覆盖。\n")
	b.WriteString("# 这条 drop-in 只做一件事：让 smbd 等这些挂载点就位再启动。\n")
	b.WriteString("# 卷挂不上时 smbd 不会启动——比启动了去服务系统盘上的空目录要好。\n")
	b.WriteString("[Unit]\n")
	if len(paths) == 0 {
		// 没有要共享的卷时，给出一个空列表而不是删掉整个 [Unit] 段：
		// 文件还在、句意清楚（“没有任何要求”），而且下次加共享时只改一行。
		b.WriteString("RequiresMountsFor=\n")
		return b.String(), nil
	}
	for i := 0; i < len(paths); i += 4 {
		end := i + 4
		if end > len(paths) {
			end = len(paths)
		}
		b.WriteString("RequiresMountsFor=" + strings.Join(paths[i:end], " ") + "\n")
	}
	return b.String(), nil
}

// startupDepResult 是一次开机依赖写入的结果。
type startupDepResult struct {
	Status string
	Paths  []string
	Notes  []Blocker
	// Changed 报告这次真的动过文件（决定要不要 daemon-reload）。
	Changed bool
}

// applyStartupDeps 把 smbd 的 drop-in 写成“这些路径必须先就位”。
//
// 传空路径 = 现在没有任何共享需要一个挂载点：文件若存在就删掉，并把
// daemon-reload 跑一遍，让 systemd 忘掉旧的要求。
//
// 失败**不**回滚配置：配置本身是对的，缺的只是开机顺序这一层额外保护。说清楚，
// 不假装成功，也不把一份正确的配置丢掉。
func (e *Engine) applyStartupDeps(ctx context.Context, mountPaths []string) startupDepResult {
	return e.applyStartupDropin(ctx, e.paths().StartupDep, "smbd", mountPaths)
}

// applyStartupDropin 是两种协议共用的那一份实现。
//
// unit 是日志与提示里点名的服务名（smbd / nfs-server）。它不参与渲染——drop-in 的
// 内容只有 RequiresMountsFor，服务是谁由**文件放在哪个 .d 目录**决定。
func (e *Engine) applyStartupDropin(ctx context.Context, dep, unit string, mountPaths []string) startupDepResult {
	fs := e.fs()
	res := startupDepResult{Status: StartupDepsSkipped, Paths: dedupeStrings(append([]string(nil), mountPaths...))}
	sort.Strings(res.Paths)

	desired, err := RenderStartupDropin(res.Paths)
	if err != nil {
		res.Status = StartupDepsFailed
		res.Notes = append(res.Notes, Blocker{Code: NoticeStartupDepFailed, Message: "开机依赖没有写入：" + err.Error()})
		return res
	}

	existing, readErr := fs.ReadFile(dep)
	present := readErr == nil
	if readErr != nil && !os.IsNotExist(readErr) {
		res.Status = StartupDepsFailed
		res.Notes = append(res.Notes, Blocker{
			Code:    NoticeStartupDepFailed,
			Message: "读不到开机依赖 " + dep + "：没有改动它，因此 " + unit + " 的开机顺序仍然按原来的来",
		})
		return res
	}

	if len(res.Paths) == 0 && !present {
		res.Status = StartupDepsSkipped
		return res
	}

	if present && string(existing) == desired {
		res.Status = StartupDepsUnchanged
		return res
	}

	if len(res.Paths) == 0 {
		if err := fs.Remove(dep); err != nil {
			res.Status = StartupDepsFailed
			res.Notes = append(res.Notes, Blocker{
				Code:    NoticeStartupDepFailed,
				Message: "删除开机依赖 " + dep + " 失败：" + err.Error(),
			})
			return res
		}
		res.Changed = true
		res.Status = StartupDepsRemoved
		return e.reloadSystemd(ctx, res)
	}

	if err := fs.MkdirAll(filepath.Dir(dep), startupDepDirMode); err != nil {
		res.Status = StartupDepsFailed
		res.Notes = append(res.Notes, Blocker{
			Code:    NoticeStartupDepFailed,
			Message: "创建 " + filepath.Dir(dep) + " 失败：" + err.Error(),
		})
		return res
	}
	candidate := dep + ".upanel-new"
	_ = fs.Remove(candidate)
	if err := fs.WriteFileSync(candidate, []byte(desired), startupDepFileMode); err != nil {
		res.Status = StartupDepsFailed
		res.Notes = append(res.Notes, Blocker{
			Code:    NoticeStartupDepFailed,
			Message: "写候选开机依赖 " + candidate + " 失败：" + err.Error(),
		})
		return res
	}
	if err := fs.Rename(candidate, dep); err != nil {
		_ = fs.Remove(candidate)
		res.Status = StartupDepsFailed
		res.Notes = append(res.Notes, Blocker{
			Code:    NoticeStartupDepFailed,
			Message: "替换开机依赖 " + dep + " 失败：" + err.Error(),
		})
		return res
	}
	if err := fs.FsyncDir(filepath.Dir(dep)); err != nil {
		// 目录没 fsync 只影响掉电后是否残留，不影响现在 systemd 读到的内容。
		res.Notes = append(res.Notes, Blocker{
			Code:    NoticeStartupDepFailed,
			Message: "开机依赖已替换，但目录 fsync 失败（掉电后可能回退）：" + err.Error(),
		})
	}
	res.Changed = true
	res.Status = StartupDepsWritten
	return e.reloadSystemd(ctx, res)
}

// reloadSystemd 让 systemd 重新读一遍单元片段。
//
// 写文件不重载等于没写：systemd 缓存着单元定义，daemon-reload 之前它不知道
// smbd.service.d 下多了一份片段。
func (e *Engine) reloadSystemd(ctx context.Context, res startupDepResult) startupDepResult {
	if _, err := e.run(ctx, syscmd.Spec{
		Program: storage.DefaultSystemctlPath,
		Args:    []string{"daemon-reload"},
	}, startupDepTimeout); err != nil {
		res.Status = StartupDepsFailed
		res.Notes = append(res.Notes, Blocker{
			Code: NoticeStartupDepFailed,
			Message: "开机依赖已写入 " + e.paths().StartupDep + "，但 systemctl daemon-reload 失败：" +
				err.Error() + "；文件在那里，但要等下一次重载才会生效",
		})
		return res
	}
	return res
}

// sharesMountPaths 收集一组托管共享用到的挂载点（去重、排序）。
func sharesMountPaths(shares []ManagedShare) []string {
	out := make([]string, 0, len(shares))
	for _, s := range shares {
		if s.MountPath != "" {
			out = append(out, s.MountPath)
		}
	}
	sort.Strings(out)
	return dedupeStrings(out)
}
