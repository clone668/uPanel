// Package hostcaps 是“这台机器能做什么”的唯一探测入口（P16.1）。
//
// 为什么要有它：在这之前，“某个功能在这台机器上做不做得了”是在三个地方各答一遍的——
// 代理 main 里逐个模块 `os.Stat` 一次（备份要 restic、部署要 docker、计划要 systemctl），
// 各子系统又各自在自己的包里有 `programPresent`，安装脚本第三步再用 shell 探一遍。
// 三处答案不一致时，界面显示“能做”、点下去 503，而安装器说“缺依赖”。同一条事实三个来源，
// 迟早会互相矛盾。
//
// 本包只回答事实，不做判决：
//   - **找程序只在固定目录里找**（/usr/sbin、/sbin、/usr/bin、/bin），不走 PATH。
//     PATH 里可能有 /usr/local 之类可写目录，而这里要回答的是“系统装没装”，
//     不是“当前 shell 能喊到谁”。
//   - **版本只在命令真的成功跑完之后才填**。填不出来就是空串，并说明为什么
//     （命令超时 / 有输出但认不出 / 这个程序没有可靠的版本开关）。绝不猜一个像样的数字。
//   - **`absent` 与 `unknown` 是两件事**。固定目录读不出来时，找不到程序不能报成
//     “这台机器没有 restic”——那是把“我没看见”说成“没有”。这种情况报 `unknown`
//     并把整个报告标成 `partial`。
//   - 每条缺席都带 `hint`（装哪个包能补上），但 hint 只是建议：装完之后能力是否可用，
//     仍然由各子系统自己的预检回答。
package hostcaps

import (
	"context"
	"errors"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"upanel/internal/syscmd"
)

// State 是探测结果的三态闭集。
type State string

const (
	// StatePresent：程序在这个位置上，且（能探的话）版本也读到了。
	StatePresent State = "present"
	// StateAbsent：固定目录都看了，没有这个程序。
	StateAbsent State = "absent"
	// StateUnknown：说不清。目录读不出来、命令超时且此前也没有读数，
	// 都属于这一态——它不是“没有”，也不是“有”。
	StateUnknown State = "unknown"
)

// Kind 区分“程序在不在”与“系统本身是不是这个形态”。
const (
	// KindProgram 是外部程序。
	KindProgram = "program"
	// KindSystem 是系统形态（目前只有 systemd：不是装没装，而是这一个 PID 1 是不是它）。
	KindSystem = "system"
)

// Item 是一条能力事实。
type Item struct {
	// ID 是稳定标识，界面与文档都按它引用，不随程序改名而变。
	ID string `json:"id"`
	// Kind 见 KindProgram / KindSystem。
	Kind string `json:"kind"`
	// Feature 是人话：补上这一条能做什么。为空表示“面板自身运行需要它”。
	Feature string `json:"feature,omitempty"`
	// State 是三态闭集。
	State State `json:"state"`
	// Path 是找到的绝对路径；没找到为空。
	Path string `json:"path,omitempty"`
	// Version 只在命令真的成功跑完并认出格式时才有值。
	Version string `json:"version,omitempty"`
	// Reason 说明为什么是这个 State。state != present 时必定非空。
	Reason string `json:"reason,omitempty"`
	// Hint 是“装哪个包能补上”，只是建议，不是承诺。
	Hint string `json:"hint,omitempty"`
}

// Report 是一次完整探测的结果。
type Report struct {
	// GeneratedAt 是这次探测开始的时刻（RFC3339 UTC）。
	GeneratedAt string `json:"generated_at"`
	// Items 按 spec 表的固定顺序输出，不排序、不抖动：同一个二进制跑两次，
	// 两份报告应当逐字相同（除了 GeneratedAt）。
	Items []Item `json:"items"`
	// Partial 表示有固定目录读不出来：此时所有 absent 都不能当“没有”用。
	Partial bool `json:"partial"`
	// Issues 列出这次探测本身遇到的问题（目录读不出来、命令超时等）。
	Issues []string `json:"issues,omitempty"`
}

// programDirs 是允许找到程序的目录，顺序固定。
//
// 与 internal/network、internal/agent 里的同一份清单保持一致：它们判的是同一件事，
// 三处（P16.1 之后应当只剩这一处）不能有一处多一个目录。
var programDirs = []string{"/usr/sbin", "/sbin", "/usr/bin", "/bin"}

// spec 是一条能力的定义。
type spec struct {
	// ID 是稳定标识。
	ID string
	// Kind 见 KindProgram / KindSystem。
	Kind string
	// Feature 是人话。
	Feature string
	// Programs 是全都要在的程序名；第一个是“主程序”，版本命令从它跑。
	Programs []string
	// NeedsDir 非空时还要求这个目录存在（systemd 的判据是 PID 1，不是程序）。
	NeedsDir string
	// Version 是版本命令的参数。nil 表示**不探版本**：这些程序没有可靠的
	// 版本开关（比如 exportfs），与其猜一个，不如说“只判存在”。
	Version []string
	// Pattern 是从输出里取版本的锚，第一个捕获组就是版本。
	// 命令成功但没有匹配上时，版本留空并说明“认不出”。
	Pattern string
	// Hint 是装哪个包能补上。
	Hint string
}

// specTable 是这张表本身。它按“有真实消费者”才登记：表里每一条都对应一个
// 已经在用的子系统；没有消费者的条目一律不加——一张越写越长、一半没人读的
// 能力表，会让人以为探测得到的每一条都已经作数。
var specTable = []spec{
	{
		ID: "systemd", Kind: KindSystem, Feature: "服务、计划任务、部署单元、回滚定时器",
		Programs: []string{"systemctl"}, NeedsDir: "/run/systemd/system",
		Version: []string{"--version"}, Pattern: `systemd\s+([0-9][0-9A-Za-z.\-]*)`,
	},
	{
		ID: "journalctl", Kind: KindProgram, Feature: "服务日志",
		Programs: []string{"journalctl"},
		Version:  []string{"--version"}, Pattern: `systemd\s+([0-9][0-9A-Za-z.\-]*)`,
		Hint: "apt install systemd（Ubuntu 自带）",
	},
	{
		ID: "systemd-analyze", Kind: KindProgram, Feature: "计划任务的单元文件校验",
		Programs: []string{"systemd-analyze"},
		Version:  []string{"--version"}, Pattern: `systemd\s+([0-9][0-9A-Za-z.\-]*)`,
		Hint: "apt install systemd（Ubuntu 自带）",
	},
	{
		ID: "docker", Kind: KindProgram, Feature: "容器清单、容器启停、Compose 部署",
		Programs: []string{"docker"},
		Version:  []string{"--version"}, Pattern: `Docker version ([^,]+),`,
		Hint: "apt install docker.io",
	},
	{
		ID: "docker.compose", Kind: KindProgram, Feature: "受管 Compose 部署",
		Programs: []string{"docker"},
		Version:  []string{"compose", "version", "--short"}, Pattern: `^\s*v?([0-9][0-9A-Za-z.\-]*)`,
		Hint: "apt install docker-compose-v2",
	},
	{
		ID: "nginx", Kind: KindProgram, Feature: "反向代理与 HTTPS 站点",
		Programs: []string{"nginx"},
		Version:  []string{"-v"}, Pattern: `nginx/([0-9][0-9A-Za-z.\-]*)`,
		Hint: "apt install nginx",
	},
	{
		ID: "restic", Kind: KindProgram, Feature: "备份与恢复",
		Programs: []string{"restic"},
		Version:  []string{"version"}, Pattern: `restic\s+([0-9][0-9A-Za-z.\-]*)`,
		Hint: "apt install restic",
	},
	{
		ID: "ssh", Kind: KindProgram, Feature: "SFTP 备份仓库、账号公钥登录",
		Programs: []string{"ssh"},
		Version:  []string{"-V"}, Pattern: `OpenSSH_([0-9][0-9A-Za-z.\-]*)`,
		Hint: "apt install openssh-client",
	},
	{
		ID: "ufw", Kind: KindProgram, Feature: "防火墙只读状态与规则写入口",
		Programs: []string{"ufw"},
		Version:  []string{"--version"}, Pattern: `ufw\s+([0-9][0-9A-Za-z.\-]*)`,
		Hint: "apt install ufw",
	},
	{
		ID: "ip", Kind: KindProgram, Feature: "网卡、路由与监听端口",
		Programs: []string{"ip"},
		Version:  []string{"-V"}, Pattern: `iproute2-([0-9][0-9A-Za-z.\-]*)`,
		Hint: "apt install iproute2（Ubuntu 自带）",
	},
	{
		ID: "nfs", Kind: KindProgram, Feature: "NFS 共享",
		Programs: []string{"exportfs"},
		Hint:     "apt install nfs-kernel-server",
	},
	{
		ID: "samba", Kind: KindProgram, Feature: "SMB 共享与 Samba 账号",
		Programs: []string{"smbd", "testparm"},
		Version:  []string{"--version"}, Pattern: `Version\s+([0-9][0-9A-Za-z.\-]*)`,
		Hint: "apt install samba",
	},
	{
		ID: "apt", Kind: KindProgram, Feature: "软件包查询、安装、升级与删除",
		Programs: []string{"apt-get", "dpkg-query"},
		Version:  []string{"--version"}, Pattern: `apt\s+([0-9][0-9A-Za-z.\-~]*)`,
		Hint: "apt install apt（Ubuntu 自带）",
	},
	{
		ID: "certbot", Kind: KindProgram, Feature: "证书签发与续期",
		Programs: []string{"certbot"},
		Version:  []string{"--version"}, Pattern: `certbot\s+([0-9][0-9A-Za-z.\-]*)`,
		Hint: "apt install certbot",
	},
	{
		ID: "smartctl", Kind: KindProgram, Feature: "磁盘 SMART 健康",
		Programs: []string{"smartctl"},
		Version:  []string{"--version"}, Pattern: `smartctl\s+([0-9][0-9A-Za-z.\-]*)`,
		Hint: "apt install smartmontools",
	},
	{
		ID: "sgdisk", Kind: KindProgram, Feature: "分区表写入",
		Programs: []string{"sgdisk"},
		Version:  []string{"--version"}, Pattern: `version\s+([0-9][0-9A-Za-z.\-]*)`,
		Hint: "apt install gdisk",
	},
	{
		ID: "mkfs.ext4", Kind: KindProgram, Feature: "格式化为 ext4",
		Programs: []string{"mkfs.ext4"},
		Version:  []string{"-V"}, Pattern: `mke2fs\s+([0-9][0-9A-Za-z.\-]*)`,
		Hint: "apt install e2fsprogs（Ubuntu 自带）",
	},
	{
		ID: "mkfs.xfs", Kind: KindProgram, Feature: "格式化为 XFS",
		Programs: []string{"mkfs.xfs"},
		Version:  []string{"-V"}, Pattern: `version\s+([0-9][0-9A-Za-z.\-]*)`,
		Hint: "apt install xfsprogs",
	},
	{
		ID: "findmnt", Kind: KindProgram, Feature: "挂载状态与卷身份核对",
		Programs: []string{"findmnt"},
		Version:  []string{"--version"}, Pattern: `util-linux\s+([0-9][0-9A-Za-z.\-]*)`,
		Hint: "apt install util-linux（Ubuntu 自带）",
	},
	{
		ID: "shadow", Kind: KindProgram, Feature: "受管 Linux 账号（建、改、删、设口令）",
		Programs: []string{"useradd", "usermod", "userdel", "chpasswd"},
		Hint:     "apt install passwd（Ubuntu 自带）",
	},
	{
		ID: "sudo", Kind: KindProgram, Feature: "受限 sudo 模板",
		Programs: []string{"sudo", "visudo"},
		Hint:     "apt install sudo",
	},
}

// Deps 是探测的外部依赖。全部可为 nil，nil 时用真实实现；
// 测试靠替换它们来构造“目录读不出来”“命令超时”这些现场。
type Deps struct {
	// ListDir 列出目录下的名字。nil 时读真实文件系统。
	ListDir func(dir string) ([]string, error)
	// StatIsDir 判断路径是不是目录。nil 时用 os.Stat。
	StatIsDir func(path string) (bool, error)
	// Run 跑一条命令。返回 RunResult 而不是裸的四个值：超时与“程序自己非零退出”
	// 是两件事，混成一个 error 之后只能靠比对错误串来区分，那种判据会在换 Go 版本
	// 或换执行器时安静地失效。
	Run func(ctx context.Context, path string, args ...string) RunResult
	// Now 供测试固定时间。nil 时用 time.Now。
	Now func() time.Time
}

// versionTimeout 是单条版本命令的时限。
//
// 版本命令不该慢；慢到超过它，多半是这个程序在等别的东西（比如 docker 在等守护进程）。
// 超时不算“没有这个程序”，只算“版本没读到”。
const versionTimeout = 3 * time.Second

// probeConcurrency 是同时在跑的版本命令上限。20 条一起 fork 出来的瞬时负载
// 会让本来就紧的 1 核机器读出假的超时。
const probeConcurrency = 4

func (d Deps) listDir() func(string) ([]string, error) {
	if d.ListDir != nil {
		return d.ListDir
	}
	return func(dir string) ([]string, error) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, err
		}
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		return names, nil
	}
}

func (d Deps) statIsDir() func(string) (bool, error) {
	if d.StatIsDir != nil {
		return d.StatIsDir
	}
	return func(path string) (bool, error) {
		info, err := os.Stat(path)
		if err != nil {
			return false, err
		}
		return info.IsDir(), nil
	}
}

// RunResult 是一条版本命令的结果。
type RunResult struct {
	Stdout string
	Stderr string
	// ExitCode 是程序自己的退出码；被信号杀死或超时时为 -1。
	ExitCode int
	// TimedOut 表示是本包的时间上限掐断的，不是程序自己失败。
	TimedOut bool
	// Err 是执行器层面的错误（找不到、无法 fork、被取消）。程序非零退出时为空。
	Err error
}

func (d Deps) run() func(context.Context, string, ...string) RunResult {
	if d.Run != nil {
		return d.Run
	}
	return func(ctx context.Context, path string, args ...string) RunResult {
		res, err := syscmd.Run(ctx, syscmd.Spec{Program: path, Args: args}, syscmd.Limits{
			Timeout:   versionTimeout,
			MaxStdout: 64 << 10,
			MaxStderr: 64 << 10,
		})
		out := RunResult{Stdout: res.Stdout, Stderr: res.Stderr, ExitCode: res.ExitCode, TimedOut: res.TimedOut}
		if err != nil {
			// 程序自己非零退出时 syscmd 也返回 *ExitError；那一种不算执行器错误，
			// 退出码已经在 ExitCode 里了。只有“没跑起来/被掐断”才留 Err。
			var exitErr *syscmd.ExitError
			if !errors.As(err, &exitErr) {
				out.Err = err
			}
		}
		return out
	}
}

func (d Deps) now() func() time.Time {
	if d.Now != nil {
		return d.Now
	}
	return time.Now
}

// Probe 完整探测一次。它不读也不写任何面板数据，只读目录、跑版本命令。
func Probe(ctx context.Context, deps Deps) Report {
	now := deps.now()
	listDir := deps.listDir()
	statIsDir := deps.statIsDir()

	report := Report{
		GeneratedAt: now().UTC().Format(time.RFC3339),
		Items:       make([]Item, len(specTable)),
		Issues:      []string{},
	}

	// 每个目录只列一次：20 条 spec 逐条去列 4 个目录是 80 次系统调用，
	// 而且中途目录变化会让同一份报告自相矛盾。
	present := map[string]string{} // 程序名 → 找到的绝对路径
	unreadable := map[string]bool{}
	for _, dir := range programDirs {
		names, err := listDir(dir)
		if err != nil {
			unreadable[dir] = true
			report.Partial = true
			report.Issues = append(report.Issues,
				"目录 "+dir+" 读不出来："+err.Error()+"——这一轮找不到的程序只能报“说不清”，不能报“没有”")
			continue
		}
		for _, name := range names {
			if _, seen := present[name]; !seen {
				present[name] = dir + "/" + name
			}
		}
	}

	// 先把“在不在”判完，再并发跑版本命令：判定本身不需要子进程。
	type versionAsk struct {
		index int
		id    string
		path  string
		args  []string
		re    *regexp.Regexp
	}
	asks := []versionAsk{}
	for i, sp := range specTable {
		item := Item{ID: sp.ID, Kind: sp.Kind, Feature: sp.Feature, Hint: sp.Hint}

		missing := []string{}
		for _, name := range sp.Programs {
			if _, ok := present[name]; !ok {
				missing = append(missing, name)
			}
		}
		if sp.NeedsDir != "" {
			isDir, err := statIsDir(sp.NeedsDir)
			if err != nil || !isDir {
				missing = append(missing, sp.NeedsDir)
			}
		}

		switch {
		case len(missing) > 0 && report.Partial:
			item.State = StateUnknown
			item.Reason = "找不到 " + strings.Join(missing, "、") +
				"，但有固定目录读不出来：说不清是真的没有，还是这一轮没看见"
			item.Hint = ""
		case len(missing) > 0:
			item.State = StateAbsent
			item.Reason = "在 " + strings.Join(programDirs, "、") + " 里没有 " + strings.Join(missing, "、")
			item.Hint = sp.Hint
		default:
			item.State = StatePresent
			item.Path = present[sp.Programs[0]]
			if sp.NeedsDir != "" {
				// 系统形态类的判据是目录（PID 1 是不是 systemd），路径没有意义。
				item.Path = sp.NeedsDir
			}
			if len(sp.Version) == 0 {
				// 没有可靠的版本开关：说清楚，而不是留一个空字段让人以为漏了。
				item.Reason = "只判存在：这个程序没有可靠的版本开关"
			}
		}
		report.Items[i] = item

		if item.State == StatePresent && len(sp.Version) > 0 {
			asks = append(asks, versionAsk{
				index: i, id: sp.ID, path: item.Path,
				args: sp.Version, re: regexp.MustCompile(sp.Pattern),
			})
		}
	}

	sem := make(chan struct{}, probeConcurrency)
	var mu sync.Mutex
	var wg sync.WaitGroup
	issues := make([]string, 0, len(asks))
	run := deps.run()
	for _, ask := range asks {
		wg.Add(1)
		go func(ask versionAsk) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			res := run(ctx, ask.path, ask.args...)
			out := res.Stdout + "\n" + res.Stderr
			version, ok, why := readVersion(ask.re, out, res)

			mu.Lock()
			defer mu.Unlock()
			item := report.Items[ask.index]
			switch {
			case ok:
				item.Version = version
				// 读到了版本就不再留“只判存在”之类的解释。
				item.Reason = ""
			case version != "":
				// 认出了版本但命令退出码非零：仍然填版本，并把话说清楚。
				item.Version = version
				item.Reason = why
			default:
				item.Version = ""
				item.Reason = "程序在，但版本读不出来：" + why
			}
			report.Items[ask.index] = item
			if !ok {
				issues = append(issues, ask.id+"："+why)
			}
		}(ask)
	}
	wg.Wait()

	if len(issues) > 0 {
		sort.Strings(issues)
		report.Issues = append(report.Issues, issues...)
	}
	if report.Issues == nil {
		report.Issues = []string{}
	}
	return report
}

// readVersion 从命令输出里取版本。
//
// 三个返回值分别是：版本、是否“干净地读到了”、以及读不到（或有保留）时的原因。
// 干净读到要求：命令返回 0，且锚点匹配上。锚点匹配上但退出码非零时，
// 版本照样填（那是真读数），但不算干净。
func readVersion(re *regexp.Regexp, out string, res RunResult) (string, bool, string) {
	switch {
	case res.TimedOut:
		// 超时只说明“没读到”，不能升级成“这个程序有问题”，更不能当成“没有”。
		return "", false, "命令超时（" + versionTimeout.String() + "）：版本没读到，程序本身仍在"
	case res.Err != nil:
		return "", false, res.Err.Error()
	}
	match := re.FindStringSubmatch(out)
	if len(match) < 2 || match[1] == "" {
		return "", false, "命令有输出，但按 " + re.String() + " 认不出版本"
	}
	version := strings.TrimSpace(match[1])
	if res.ExitCode != 0 {
		return version, false, "命令以退出码 " + strconv.Itoa(res.ExitCode) + " 结束；版本取自它打印的那一行"
	}
	return version, true, ""
}

// ItemIDs 返回 spec 表里的全部 ID（按表顺序），供文档与测试核对“表里到底有什么”。
func ItemIDs() []string {
	ids := make([]string, len(specTable))
	for i, sp := range specTable {
		ids[i] = sp.ID
	}
	return ids
}
