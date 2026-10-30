package services

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"upanel/internal/syscmd"
)

// 时间上限。查询比动作短：列表页不该因为一个卡住的 systemctl 等太久。
const (
	listTimeout    = 20 * time.Second
	showTimeout    = 10 * time.Second
	controlTimeout = 90 * time.Second
)

// maxShowChunk 是单次 systemctl show 传入的单元数上限。
//
// 一次传太多会逼近 argv 上限；分块也把单次失败的影响限制在一小段单元上。
const maxShowChunk = 200

// propertyList 是 show 只取这些属性：够用、输出小、不把整个属性表拉进解析器。
//
// Names 不是给界面看的，是给解析器核对“这一块对应我请求的那个名字”用的：
// 别名单元（例如 dbus-org.freedesktop.ModemManager1.service）的 Id 是规范名，
// 只有 Names 里才出现你请求的那个别名。
var propertyList = []string{
	"Id",
	"Names",
	"LoadState",
	"ActiveState",
	"SubState",
	"UnitFileState",
	"Description",
	"FragmentPath",
	"MainPID",
	"NRestarts",
	"ActiveEnterTimestamp",
	"MemoryCurrent",
	"TasksCurrent",
}

// ErrSystemctlUnavailable 表示本机没有可用的 systemctl（例如容器或非 systemd 系统）。
// 它是运行期状态：报告“依赖缺失”，而不是假装服务列表为空。
var ErrSystemctlUnavailable = errors.New("services: 本机没有可用的 systemctl")

// Systemctl 只按固定 argv 调用 systemctl。
//
// 程序路径在构造时确定并且必须是绝对路径；参数由本包构造，单元名必须来自本包的
// 枚举并通过 ValidateUnitName——HTTP 参数永远不能变成 systemctl 的一个选项。
type Systemctl struct {
	path string
	// runFn 只用于测试：替换真正的进程执行。生产路径永远是 syscmd.Run，
	// 没有任何配置或 HTTP 输入能改写它。
	runFn func(ctx context.Context, timeout time.Duration, args ...string) (syscmd.Result, error)
}

// NewSystemctl 构造调用器。path 必须是绝对路径，且文件名必须是 systemctl（或 systemctl 的
// 绝对软链接目标名），避免有人把别的程序塞进这个字段。
func NewSystemctl(path string) (*Systemctl, error) {
	if path == "" {
		return nil, ErrSystemctlUnavailable
	}
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("services: systemctl 路径必须是绝对路径，当前为 %q", path)
	}
	if filepath.Base(path) != "systemctl" {
		return nil, fmt.Errorf("services: 只接受 systemctl 可执行文件，当前为 %q", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSystemctlUnavailable, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%w: %q 是目录", ErrSystemctlUnavailable, path)
	}
	return &Systemctl{path: path}, nil
}

// Path 返回实际使用的 systemctl 路径（日志与证据用）。
func (s *Systemctl) Path() string { return s.path }

// FindSystemctl 在固定位置里找 systemctl。explicit 非空时只看它。
func FindSystemctl(explicit string) (string, bool) {
	if explicit != "" {
		if _, err := NewSystemctl(explicit); err != nil {
			return "", false
		}
		return explicit, true
	}
	for _, candidate := range []string{"/usr/bin/systemctl", "/bin/systemctl"} {
		if _, err := NewSystemctl(candidate); err == nil {
			return candidate, true
		}
	}
	return "", false
}

// run 执行一次 systemctl。
func (s *Systemctl) run(ctx context.Context, timeout time.Duration, args ...string) (syscmd.Result, error) {
	if s.runFn != nil {
		return s.runFn(ctx, timeout, args...)
	}
	res, err := syscmd.Run(ctx, syscmd.Spec{Program: s.path, Args: args}, syscmd.Limits{
		Timeout: timeout,
		// show 的输出与列表都不该超过 1 MiB；截断时上层能看到 Truncated 并据此报错。
		MaxStdout: syscmd.DefaultMaxStdout,
		MaxStderr: syscmd.DefaultMaxStderr,
	})
	if err != nil {
		return res, err
	}
	return res, nil
}

// ListUnitNames 枚举本机所有 service 单元名（含未加载的单元文件、transient 单元）。
//
// 两条命令合并：
//   - list-unit-files 给出“磁盘上有单元文件”的；
//   - list-units --all 给出“systemd 已装载（含 inactive/failed）”的。
//
// 只用 --no-legend --plain 去掉列标题与 ● 标记，解析的是**固定位置的第一列**
// （单元名），不是本地化的文本列宽。
func (s *Systemctl) ListUnitNames(ctx context.Context) ([]string, error) {
	type call struct {
		args []string
		// aliasColumn 表示这条命令的输出第二列是单元文件状态，可以据此剔掉别名。
		aliasColumn bool
	}
	calls := []call{
		{args: []string{"list-unit-files", "--type=service", "--no-legend", "--no-pager", "--plain"}, aliasColumn: true},
		{args: []string{"list-units", "--type=service", "--all", "--no-legend", "--no-pager", "--plain"}},
	}
	seen := make(map[string]struct{}, 128)
	names := make([]string, 0, 128)
	var firstErr error
	okCalls := 0
	var errDetail string
	for _, c := range calls {
		res, err := s.run(ctx, listTimeout, c.args...)
		if err != nil {
			if firstErr == nil {
				firstErr = err
				errDetail = strings.TrimSpace(res.Stderr)
			}
			continue
		}
		okCalls++
		for _, line := range strings.Split(res.Stdout, "\n") {
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			name := fields[0]
			if name == "0" || !strings.HasSuffix(name, ".service") {
				continue
			}
			if err := ValidateUnitName(name); err != nil {
				continue
			}
			// 别名单元（UnitFileState=alias）不进入列表：它就是另一个单元的名字。
			// 列出来会有两个后果，而且都是错的：
			//   - 同一份单元出现两行，两个名字各拿一把 service:<unit> 锁，
			//     锁不住同一个对象；
			//   - 保护名单按基名判定，别名（例如 dbus-org.freedesktop.login1.service）
			//     不在表里，于是它看起来“没受保护”，而 systemctl 会把它解析成
			//     systemd-logind.service——保护就这么被绕过去了。
			if c.aliasColumn && len(fields) > 1 && fields[1] == "alias" {
				continue
			}
			// 模板单元（`foo@.service`，没有实例）在这里就剔除，理由是实测出来的：
			// systemctl show 不接受这种名字——stderr 打一行
			// "Unit name foo@.service is neither a valid invocation ID nor unit name."
			// 然后**丢掉剩下的名字**，退出码 1（原始报错见
			// testdata/show_template_rejected.stderr.txt）。模板本身也没有“已加载/未加载”
			// 的真相可以填 load_state，所以列表里不出现它；实例单元
			// （`foo@bar.service`）照常枚举。
			if IsTemplate(name) {
				continue
			}
			if _, dup := seen[name]; dup {
				continue
			}
			seen[name] = struct{}{}
			names = append(names, name)
		}
	}
	if okCalls == 0 {
		if errDetail != "" {
			return nil, fmt.Errorf("services: 枚举服务单元失败：%s", errDetail)
		}
		return nil, fmt.Errorf("services: 枚举服务单元失败: %w", firstErr)
	}
	return names, nil
}

// RawUnit 是一次 show 的原始属性。
type RawUnit struct {
	Properties map[string]string
}

// Get 取一个属性（不存在时返回空串）。
func (r RawUnit) Get(key string) string { return r.Properties[key] }

// Names 返回这个单元能被请求的全部名字：Id 加上 Names 里的别名。
func (r RawUnit) Names() []string {
	out := make([]string, 0, 4)
	if id := strings.TrimSpace(r.Get("Id")); id != "" {
		out = append(out, id)
	}
	for _, name := range strings.Fields(r.Get("Names")) {
		if name != "" {
			out = append(out, name)
		}
	}
	return out
}

// Show 批量读取单元属性。
//
// 返回值按请求顺序对应；某个单元不存在时 systemctl 仍然会打印一个块
// （LoadState=not-found），我们不把它当错误——调用方需要区分“没有这个单元”
// 与“systemctl 读失败”。
func (s *Systemctl) Show(ctx context.Context, names []string) ([]RawUnit, error) {
	out := make([]RawUnit, 0, len(names))
	if len(names) == 0 {
		return out, nil
	}
	args := []string{"show", "--no-pager"}
	for _, prop := range propertyList {
		args = append(args, "--property="+prop)
	}
	// `--` 之后才是单元名：单元名以字母数字开头（ValidateUnitName 保证），
	// 这里再显式分隔一次，避免任何名字被当成选项。
	prefix := args
	for start := 0; start < len(names); start += maxShowChunk {
		end := start + maxShowChunk
		if end > len(names) {
			end = len(names)
		}
		blocks, err := s.showChunk(ctx, prefix, names[start:end])
		if err != nil {
			return nil, err
		}
		out = append(out, blocks...)
	}
	return out, nil
}

// showChunk 读一段单元，并逐个核对“请求的名字是不是都回来了”。
//
// 为什么要核对：`systemctl show` 碰到它不接受的名字（实测是模板单元
// `foo@.service`）只打一行 stderr 就**放弃剩下的名字**，输出里只剩前面那几个块，
// 退出码 1。按“解析出至少一个块就算成功”处理，282 个单元的机器上会安静地返回
// 7 个，并且 truncated=false——数据是错的，但看起来完全正常。这类错误必须让
// 采集失败（保留上一次成功快照），而不是给出一份短列表。
//
// 补救方式是单独重问缺的那个名字：一次只问一个，坏名字不会连累别人。
func (s *Systemctl) showChunk(ctx context.Context, prefix, chunk []string) ([]RawUnit, error) {
	callArgs := make([]string, 0, len(prefix)+1+len(chunk))
	callArgs = append(callArgs, prefix...)
	callArgs = append(callArgs, "--")
	callArgs = append(callArgs, chunk...)

	res, err := s.run(ctx, showTimeout, callArgs...)
	parsed := parseShowOutput(res.Stdout)
	byName := indexShowBlocks(parsed)

	out := make([]RawUnit, 0, len(chunk))
	for _, name := range chunk {
		if u, ok := byName[name]; ok {
			out = append(out, u)
			continue
		}
		oneArgs := make([]string, 0, len(prefix)+2)
		oneArgs = append(oneArgs, prefix...)
		oneArgs = append(oneArgs, "--", name)
		one, oneErr := s.run(ctx, showTimeout, oneArgs...)
		if oneErr != nil {
			return nil, fmt.Errorf("services: 读取单元 %s 状态失败：%s", name, showFailureDetail(one, res, oneErr, err))
		}
		u, ok := indexShowBlocks(parseShowOutput(one.Stdout))[name]
		if !ok {
			return nil, fmt.Errorf("services: 读取单元 %s 状态失败：systemctl show 没有返回这个单元", name)
		}
		out = append(out, u)
	}
	return out, nil
}

// indexShowBlocks 把解析出来的块按“能被请求的名字”建索引。
//
// 光认 Id 不够：别名单元的 Id 是规范名。实测
// `systemctl show -- dbus-org.freedesktop.ModemManager1.service` 第一行就是
// `Id=ModemManager.service`，只在 `Names` 里才有你请求的那个别名。
// 只按 Id 匹配会把这种单元当成“读不到”，进而让整次采集失败。
func indexShowBlocks(blocks []RawUnit) map[string]RawUnit {
	index := make(map[string]RawUnit, len(blocks))
	for _, u := range blocks {
		for _, name := range u.Names() {
			if _, dup := index[name]; !dup {
				index[name] = u
			}
		}
	}
	return index
}

// showFailureDetail 挑一条最贴近原因的错误文本：先看这次单独调用的 stderr，
// 再看整批调用的 stderr，最后才用 Go 的进程错误。
func showFailureDetail(one syscmd.Result, batch syscmd.Result, oneErr, batchErr error) string {
	for _, candidate := range []string{one.Stderr, batch.Stderr} {
		if detail := strings.TrimSpace(candidate); detail != "" {
			return detail
		}
	}
	if oneErr != nil {
		return oneErr.Error()
	}
	if batchErr != nil {
		return batchErr.Error()
	}
	return "systemctl 没有返回可解析的输出"
}

// ShowOne 读取单个单元。found=false 表示 systemctl 明确说没有这个单元。
func (s *Systemctl) ShowOne(ctx context.Context, name string) (RawUnit, bool, error) {
	units, err := s.Show(ctx, []string{name})
	if err != nil {
		return RawUnit{}, false, err
	}
	for _, u := range units {
		if u.Get("Id") == name || u.Get("Id") == "" || slices.Contains(u.Names(), name) {
			if u.Get("LoadState") == LoadNotFound {
				return u, false, nil
			}
			return u, true, nil
		}
	}
	return RawUnit{}, false, nil
}

// parseShowOutput 解析 `systemctl show` 的输出：块之间用空行分隔，块内是 Key=Value。
//
// 只按第一个 '=' 切分：Description 里可以出现 '='，那不是字段边界。
func parseShowOutput(stdout string) []RawUnit {
	blocks := strings.Split(strings.ReplaceAll(stdout, "\r\n", "\n"), "\n\n")
	out := make([]RawUnit, 0, len(blocks))
	for _, block := range blocks {
		props := make(map[string]string, 16)
		for _, line := range strings.Split(block, "\n") {
			if line == "" {
				continue
			}
			key, value, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			props[key] = value
		}
		if len(props) == 0 {
			continue
		}
		out = append(out, RawUnit{Properties: props})
	}
	return out
}

// Control 执行一次启停/重启/开机启动动作。
//
// 参数只有三个固定形态：动词、--no-ask-password、单元名。没有任何自由参数，
// 也不允许调用方塞额外选项。
func (s *Systemctl) Control(ctx context.Context, op, unit string) (syscmd.Result, error) {
	if !ValidOperation(op) {
		return syscmd.Result{}, fmt.Errorf("services: 不支持的操作 %q", op)
	}
	if err := ValidateUnitName(unit); err != nil {
		return syscmd.Result{}, err
	}
	return s.run(ctx, controlTimeout, op, "--no-ask-password", unit)
}
