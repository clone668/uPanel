package nginx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// 默认路径。允许覆盖是为了让测试能指向临时目录，也因为有的机器把 Nginx 装在别处。
const (
	DefaultConfigRoot = "/etc/nginx"
	DefaultNginxPath  = "/usr/sbin/nginx"
	DefaultUnitName   = "nginx.service"

	// DefaultMaxFileBytes 单份配置文件大小上限。1 MiB 对任何手写配置都富余，
	// 超过这个数说明它不是配置（或者不该由面板去解析它）。
	DefaultMaxFileBytes = 1 << 20

	// DefaultMaxTotalBytes 一次采集读取的总字节上限。挡住“conf.d 里塞了一堆
	// 大文件”这种情况把 root agent 拖死。
	DefaultMaxTotalBytes = 16 << 20

	// DefaultMaxFiles 单次采集的文件数上限。
	DefaultMaxFiles = 4096

	// DefaultCacheTTL 是展示用快照的缓存时长。要拿来做判断时请用 Collect。
	DefaultCacheTTL = 5 * time.Second
)

// VersionProbe 返回 `nginx -v` 报的版本号（"1.24.0"）。读不到就返回错误，不要编。
type VersionProbe func(ctx context.Context) (string, error)

// UnitProbe 返回 nginx.service 的 ActiveState。ok 为 false 表示服务不存在。
type UnitProbe func(ctx context.Context) (state string, ok bool, err error)

// Options 是采集器的配置。零值可用（全部走默认）。
type Options struct {
	ConfigRoot    string
	NginxPath     string
	UnitName      string
	Version       VersionProbe
	Unit          UnitProbe
	Clock         func() time.Time
	MaxFileBytes  int64
	MaxTotalBytes int64
	MaxFiles      int
	CacheTTL      time.Duration
}

// Collector 读 Nginx 现状。并发安全。
type Collector struct {
	opts Options

	collectMu sync.Mutex // 同一个时刻只做一次采集，避免并发读同一批文件

	mu       sync.Mutex
	cached   Facts
	cachedAt time.Time
	hasCache bool
}

// New 构造采集器，补齐默认值。
func New(opts Options) *Collector {
	if opts.ConfigRoot == "" {
		opts.ConfigRoot = DefaultConfigRoot
	}
	if opts.NginxPath == "" {
		opts.NginxPath = DefaultNginxPath
	}
	if opts.UnitName == "" {
		opts.UnitName = DefaultUnitName
	}
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	if opts.MaxFileBytes <= 0 {
		opts.MaxFileBytes = DefaultMaxFileBytes
	}
	if opts.MaxTotalBytes <= 0 {
		opts.MaxTotalBytes = DefaultMaxTotalBytes
	}
	if opts.MaxFiles <= 0 {
		opts.MaxFiles = DefaultMaxFiles
	}
	if opts.CacheTTL <= 0 {
		opts.CacheTTL = DefaultCacheTTL
	}
	return &Collector{opts: opts}
}

// Snapshot 返回缓存内的现状，过期就重新采集。
//
// 给展示用。**不要用快照做写操作的冲突判断**——缓存窗口里的新增配置会被漏掉，
// 而那正好是“用户刚手工加了一条”这种最需要判准的场景。判断用 Collect。
func (c *Collector) Snapshot(ctx context.Context) (Facts, error) {
	c.mu.Lock()
	if c.hasCache && c.opts.Clock().Sub(c.cachedAt) < c.opts.CacheTTL {
		f := c.cached
		c.mu.Unlock()
		return f, nil
	}
	c.mu.Unlock()

	facts, err := c.Collect(ctx)
	if err != nil {
		return Facts{}, err
	}

	c.mu.Lock()
	c.cached = facts
	c.cachedAt = c.opts.Clock()
	c.hasCache = true
	c.mu.Unlock()
	return facts, nil
}

// Invalidate 丢掉缓存。写操作之后调用，免得下一次展示还说着写之前的话。
func (c *Collector) Invalidate() {
	c.mu.Lock()
	c.hasCache = false
	c.cached = Facts{}
	c.cachedAt = time.Time{}
	c.mu.Unlock()
}

// Collect 立刻读一遍，不看缓存。
func (c *Collector) Collect(ctx context.Context) (Facts, error) {
	c.collectMu.Lock()
	defer c.collectMu.Unlock()
	return c.collectLocked(ctx)
}

func (c *Collector) collectLocked(ctx context.Context) (Facts, error) {
	now := c.opts.Clock()
	facts := Facts{
		ConfigRoot:  c.opts.ConfigRoot,
		CollectedAt: now,
	}

	if err := ctx.Err(); err != nil {
		return Facts{}, err
	}

	rootInfo, err := os.Stat(c.opts.ConfigRoot)
	switch {
	case err != nil && os.IsNotExist(err):
		facts.ConfigMissing = true
		facts.Reason = "主机上没有 " + c.opts.ConfigRoot + " 目录，读不到 Nginx 配置"
		return facts, nil
	case err != nil:
		facts.Reason = "读不到配置目录 " + c.opts.ConfigRoot + "：" + err.Error()
		return facts, nil
	case !rootInfo.IsDir():
		facts.Reason = c.opts.ConfigRoot + " 不是目录"
		return facts, nil
	}
	facts.Available = true

	if _, err := os.Stat(c.opts.NginxPath); err != nil {
		facts.Warnings = append(facts.Warnings,
			"找不到 nginx 可执行文件 "+c.opts.NginxPath+"（配置能读，但版本与服务状态可能不准）")
	}
	if c.opts.Version != nil {
		if v, err := c.opts.Version(ctx); err == nil {
			facts.Version = v
		} else {
			facts.Warnings = append(facts.Warnings, "读 nginx 版本失败："+err.Error())
		}
	}
	if c.opts.Unit != nil {
		state, ok, err := c.opts.Unit(ctx)
		switch {
		case err != nil:
			facts.Warnings = append(facts.Warnings, "读 "+c.opts.UnitName+" 状态失败："+err.Error())
		case ok:
			facts.UnitState = state
		default:
			facts.Warnings = append(facts.Warnings, "没有 "+c.opts.UnitName+" 这个服务")
		}
	}

	c.scan(ctx, &facts)
	sort.Slice(facts.Files, func(i, j int) bool { return facts.Files[i].Path < facts.Files[j].Path })
	sort.Slice(facts.Includes, func(i, j int) bool {
		if facts.Includes[i].File != facts.Includes[j].File {
			return facts.Includes[i].File < facts.Includes[j].File
		}
		return facts.Includes[i].Line < facts.Includes[j].Line
	})
	return facts, nil
}

// scan 读主配置与两个标准目录。
//
// 只在这三个位置找 server 块：nginx.conf、conf.d/*.conf、sites-enabled/*（和 nginx
// 的默认 include 一致）。故意不递归、不跟随其它 include——跟着跟着就会读到用户放
// 在任意位置的证书私钥和乱七八糟的东西，而那份收益并不值得。
func (c *Collector) scan(ctx context.Context, facts *Facts) {
	seen := make(map[string]struct{})
	var total int64

	addFile := func(displayPath, realPath string) {
		if len(facts.Files) >= c.opts.MaxFiles {
			facts.Warnings = append(facts.Warnings,
				fmt.Sprintf("配置文件太多，只读了前 %d 个", c.opts.MaxFiles))
			return
		}
		if _, dup := seen[realPath]; dup {
			return
		}
		seen[realPath] = struct{}{}

		file := ConfigFile{Path: displayPath, Managed: IsManagedName(filepath.Base(displayPath))}
		info, err := os.Stat(realPath)
		if err != nil {
			file.Error = "读不到这个文件：" + err.Error()
			facts.Files = append(facts.Files, file)
			return
		}
		file.SizeBytes = info.Size()
		if info.Size() > c.opts.MaxFileBytes {
			file.Error = fmt.Sprintf("文件 %d 字节，超过 %d 字节上限，没有解析", info.Size(), c.opts.MaxFileBytes)
			facts.Files = append(facts.Files, file)
			facts.Warnings = append(facts.Warnings, displayPath+"："+file.Error)
			return
		}
		if total+info.Size() > c.opts.MaxTotalBytes {
			file.Error = "本次采集已达总字节上限，没有解析"
			facts.Files = append(facts.Files, file)
			facts.Warnings = append(facts.Warnings,
				fmt.Sprintf("%s：本次采集总读取量已达 %d 字节上限，之后不再解析", displayPath, c.opts.MaxTotalBytes))
			return
		}
		data, err := os.ReadFile(realPath)
		if err != nil {
			file.Error = "读不到这个文件：" + err.Error()
			facts.Files = append(facts.Files, file)
			return
		}
		total += int64(len(data))

		nodes, perr := parseConfig(string(data))
		if perr != nil {
			file.Error = perr.Error()
			facts.Files = append(facts.Files, file)
			facts.Warnings = append(facts.Warnings, displayPath+" 解析失败："+perr.Error())
			return
		}
		collectNodes(displayPath, nodes, &file, &facts.Includes)
		facts.Files = append(facts.Files, file)
	}

	mainConf := filepath.Join(c.opts.ConfigRoot, "nginx.conf")
	if _, err := os.Stat(mainConf); err == nil {
		addFile(mainConf, mainConf)
	} else if !os.IsNotExist(err) {
		facts.Warnings = append(facts.Warnings, "读不到 "+mainConf+"："+err.Error())
	}

	// conf.d 只认 *.conf（nginx 的默认 include 就是 *.conf）；
	// sites-enabled 认全部普通文件（默认 include 是 *）。
	for _, dir := range []struct {
		path   string
		onlyCo bool
	}{
		{filepath.Join(c.opts.ConfigRoot, "conf.d"), true},
		{filepath.Join(c.opts.ConfigRoot, "sites-enabled"), false},
	} {
		if err := ctx.Err(); err != nil {
			return
		}
		entries, err := os.ReadDir(dir.path)
		if err != nil {
			if !os.IsNotExist(err) {
				facts.Warnings = append(facts.Warnings, "读不到目录 "+dir.path+"："+err.Error())
			}
			continue
		}
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			name := e.Name()
			if strings.HasPrefix(name, ".") || strings.HasSuffix(name, "~") {
				continue
			}
			if dir.onlyCo && !strings.HasSuffix(name, ".conf") {
				continue
			}
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			display := filepath.Join(dir.path, name)
			real, err := filepath.EvalSymlinks(display)
			if err != nil {
				facts.Files = append(facts.Files, ConfigFile{
					Path:    display,
					Managed: IsManagedName(name),
					Error:   "符号链接解析失败：" + err.Error(),
				})
				continue
			}
			info, err := os.Stat(real)
			if err != nil {
				facts.Files = append(facts.Files, ConfigFile{
					Path:    display,
					Managed: IsManagedName(name),
					Error:   "读不到链接指向的文件：" + err.Error(),
				})
				continue
			}
			if info.IsDir() {
				// nginx 的 * 会把目录匹配进来然后报错；那是 nginx 的事，这里跳过。
				continue
			}
			addFile(display, real)
		}
	}
}

// collectNodes 从指令树里挖出 server 块与 include。
func collectNodes(file string, nodes []directive, out *ConfigFile, includes *[]Include) {
	for _, node := range nodes {
		switch node.Name {
		case "server":
			if !node.HasBlock {
				continue
			}
			srv := ServerBlock{Line: node.Line}
			for _, child := range node.Block {
				switch child.Name {
				case "server_name":
					if srv.NamesLine == 0 {
						srv.NamesLine = child.Line
					}
					for _, arg := range child.Args {
						srv.Names = append(srv.Names, Classify(arg))
					}
				case "listen":
					srv.Listen = append(srv.Listen, ParseListen(child.Args))
				}
			}
			out.Servers = append(out.Servers, srv)
			collectNodes(file, node.Block, out, includes)
		case "include":
			if len(node.Args) > 0 && !isFollowedInclude(node.Args[0]) {
				*includes = append(*includes, Include{
					File:   file,
					Line:   node.Line,
					Target: strings.Join(node.Args, " "),
				})
			}
			collectNodes(file, node.Block, out, includes)
		default:
			if node.HasBlock {
				collectNodes(file, node.Block, out, includes)
			}
		}
	}
}

// isFollowedInclude 判断一个 include 目标是否已经在本包的扫描范围里。
//
// 只按路径前缀判断，不做通配展开：conf.d 和 sites-enabled 这两个目录整体都被扫了，
// 所以指向它们的 include（不论写绝对路径还是相对路径）不算漏网。
func isFollowedInclude(target string) bool {
	t := strings.TrimSpace(target)
	if t == "" {
		return true
	}
	// 去掉通配后缀，只看目录部分。
	if i := strings.IndexAny(t, "*?["); i >= 0 {
		t = t[:i]
	}
	clean := filepath.ToSlash(filepath.Clean(t))
	clean = strings.TrimSuffix(clean, "/")
	for _, dir := range []string{"conf.d", "sites-enabled"} {
		if clean == dir || strings.HasSuffix(clean, "/"+dir) {
			return true
		}
	}
	return false
}

// ManagedPrefix 是面板自己写的配置文件名前缀。
const ManagedPrefix = "upanel-"

// IsManagedName 判断文件名是不是面板自己的（upanel-<domain>.conf）。
//
// 判据只有文件名，不看内容、不看目录：面板只写这种名字的文件，也只删这种名字的文件。
// 用户把面板的文件改名成别的，面板就当它变成用户的文件——那正好是“松手”。
func IsManagedName(name string) bool {
	return strings.HasPrefix(name, ManagedPrefix) && strings.HasSuffix(name, ".conf")
}

// ManagedFileName 返回某个域名对应的面板文件名。
//
// 域名在这里已经是归一化后的 A-label（由 internal/proxies 保证），因此可以直接
// 当文件名用。仍然做一次白名单检查：只要有一个字符落在这个集合外，就不生成文件名，
// 免得把路径分隔符之类的东西拼进去。
func ManagedFileName(host string) (string, error) {
	if host == "" || len(host) > 253 {
		return "", errors.New("域名长度不合法")
	}
	for i := 0; i < len(host); i++ {
		c := host[i]
		if !(c >= 'a' && c <= 'z') && !(c >= '0' && c <= '9') && c != '-' && c != '.' && c != '_' {
			return "", errors.New("域名里有不能当文件名用的字符")
		}
	}
	if strings.HasPrefix(host, ".") || strings.Contains(host, "..") {
		return "", errors.New("域名形状不合法")
	}
	return ManagedPrefix + host + ".conf", nil
}
