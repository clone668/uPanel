package nginx

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeFile 写一个测试文件，目录不存在就建。
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写文件失败：%v", err)
	}
}

// fakeHost 造一个装着“Nginx 目录结构”的临时根，返回根目录与采集器。
func fakeHost(t *testing.T, opts Options) (string, *Collector) {
	t.Helper()
	root := t.TempDir()
	// 假的可执行文件：本包只需要它存在，不需要它真能跑。
	bin := filepath.Join(root, "nginx-bin")
	writeFile(t, bin, "")
	opts.ConfigRoot = filepath.Join(root, "etc-nginx")
	opts.NginxPath = bin
	return root, New(opts)
}

// mainConf 是 Ubuntu 上 nginx.conf 的最小真实形状：include 两个标准目录。
const mainConf = `user www-data;
worker_processes auto;

events {
    worker_connections 768;
}

http {
    include /etc/nginx/mime.types;
    include /etc/nginx/conf.d/*.conf;
    include /etc/nginx/sites-enabled/*;

    server {
        listen 80 default_server;
        server_name _;
    }
}
`

// TestCollectReadsStandardLayout 确认三个标准位置都被读到，面板文件被认出来，
// server_name 与 listen 都解析出来了。
func TestCollectReadsStandardLayout(t *testing.T) {
	root, c := fakeHost(t, Options{})
	confRoot := filepath.Join(root, "etc-nginx")
	writeFile(t, filepath.Join(confRoot, "nginx.conf"), mainConf)
	writeFile(t, filepath.Join(confRoot, "conf.d", "shop.conf"), `
server {
    listen 443 ssl;
    server_name shop.example.com;
}
`)
	writeFile(t, filepath.Join(confRoot, "sites-enabled", "blog"), `
server {
    listen 80;
    server_name blog.example.com www.blog.example.com;
}
`)
	writeFile(t, filepath.Join(confRoot, "conf.d", "upanel-api.example.com.conf"), `
server {
    listen 443 ssl;
    server_name api.example.com;
}
`)

	facts, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("采集失败：%v", err)
	}
	if !facts.Available {
		t.Fatalf("应该能读到配置，实际不可用：%s", facts.Reason)
	}
	if len(facts.Files) != 4 {
		t.Fatalf("应读到 4 个文件，实际 %d 个：%v", len(facts.Files), paths(facts.Files))
	}

	byName := map[string]ConfigFile{}
	for _, f := range facts.Files {
		byName[filepath.Base(f.Path)] = f
	}
	if !byName["upanel-api.example.com.conf"].Managed {
		t.Fatal("upanel- 开头的 .conf 应被认成面板文件")
	}
	if byName["shop.conf"].Managed {
		t.Fatal("shop.conf 不是面板文件")
	}
	if byName["nginx.conf"].Managed {
		t.Fatal("nginx.conf 永远不是面板的")
	}

	claims := facts.Lookup("shop.example.com")
	if len(claims.Matches) != 1 {
		t.Fatalf("shop.example.com 应该有一条声明，实际 %d 条", len(claims.Matches))
	}
	// 指 server_name 那一行（第 4 行），不是 server 块的开头（第 2 行）。
	if filepath.Base(claims.Matches[0].File) != "shop.conf" || claims.Matches[0].Line != 4 {
		t.Fatalf("声明应指向 shop.conf 第 4 行，实际 %s 第 %d 行",
			claims.Matches[0].File, claims.Matches[0].Line)
	}

	multi := facts.Lookup("www.blog.example.com")
	if len(multi.Matches) != 1 {
		t.Fatalf("www.blog.example.com 应有一条声明，实际 %d 条", len(multi.Matches))
	}

	// 占位名 `_` 不占任何真实域名。
	if got := facts.Lookup("whatever.example.com"); len(got.Matches) != 0 || len(got.Undecidable) != 0 {
		t.Fatalf("`_` 不应占住具体域名，实际 %+v", got)
	}

	names := facts.ServerNames()
	want := []string{"api.example.com", "blog.example.com", "shop.example.com", "www.blog.example.com"}
	if len(names) != len(want) {
		t.Fatalf("域名清单应为 %v，实际 %v", want, names)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("域名清单应为 %v，实际 %v", want, names)
		}
	}

	// 只列出没跟随的 include（mime.types），两个标准目录不算漏网。
	if len(facts.Includes) != 1 || !strings.Contains(facts.Includes[0].Target, "mime.types") {
		t.Fatalf("漏网 include 应只有 mime.types，实际 %+v", facts.Includes)
	}
	if len(facts.Warnings) != 0 {
		t.Fatalf("这份布局没有该报警的东西，实际：%v", facts.Warnings)
	}
}

// TestCollectMissingRoot 确认“读不到配置”和“没有冲突”是两件不同的事。
func TestCollectMissingRoot(t *testing.T) {
	_, c := fakeHost(t, Options{})
	facts, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("采集失败：%v", err)
	}
	if facts.Available {
		t.Fatal("配置目录不存在时不该报可用")
	}
	if !facts.ConfigMissing {
		t.Fatal("配置目录不存在时要标明“确实没有”，而不是“读不了”")
	}
	if facts.Reason == "" {
		t.Fatal("不可用时必须说明原因")
	}
	if len(facts.Files) != 0 {
		t.Fatalf("不可用时不该有文件，实际 %d 个", len(facts.Files))
	}
}

// TestCollectBrokenFileIsNotSilence 确认解析失败的文件既报出来，也不被当成空的。
//
// 这是本包最容易犯的错：一个解析不了的文件，它的 server_name 一个都没读出来。
// 如果此时 Lookup 说“没冲突”，面板就会把配置写到别人已经占住的域名上。
func TestCollectBrokenFileIsNotSilence(t *testing.T) {
	root, c := fakeHost(t, Options{})
	confRoot := filepath.Join(root, "etc-nginx")
	writeFile(t, filepath.Join(confRoot, "nginx.conf"), "http { include /etc/nginx/conf.d/*.conf; }\n")
	broken := filepath.Join(confRoot, "conf.d", "broken.conf")
	// 少一个 `}`：整份文件解析不了，里面的 server_name 一个都读不出来。
	writeFile(t, broken, "server {\n  server_name taken.example.com;\n  listen 80;\n")

	facts, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("采集失败：%v", err)
	}
	if !facts.Available {
		t.Fatalf("主配置可读，应该算可用：%s", facts.Reason)
	}
	brokenFiles := facts.BrokenFiles()
	if len(brokenFiles) != 1 || brokenFiles[0] != broken {
		t.Fatalf("应报出 broken.conf，实际 %v", brokenFiles)
	}
	found := false
	for _, w := range facts.Warnings {
		if strings.Contains(w, "broken.conf") && strings.Contains(w, "解析失败") {
			found = true
		}
	}
	if !found {
		t.Fatalf("警告里应点名 broken.conf 解析失败，实际 %v", facts.Warnings)
	}
	// 解析不出来，就查不到——但不能据此说没有。
	if claims := facts.Lookup("taken.example.com"); len(claims.Matches) != 0 {
		t.Fatalf("解析失败的文件不该给出声明，实际 %+v", claims.Matches)
	}
}

// TestCollectOversizeFile 确认过大的文件不解析、并且说出来。
func TestCollectOversizeFile(t *testing.T) {
	root, c := fakeHost(t, Options{MaxFileBytes: 64})
	confRoot := filepath.Join(root, "etc-nginx")
	writeFile(t, filepath.Join(confRoot, "nginx.conf"), "http { include /etc/nginx/conf.d/*.conf; }\n")
	writeFile(t, filepath.Join(confRoot, "conf.d", "big.conf"),
		"server {\n"+strings.Repeat("    # 填充\n", 100)+"}\n")

	facts, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("采集失败：%v", err)
	}
	var big *ConfigFile
	for i := range facts.Files {
		if filepath.Base(facts.Files[i].Path) == "big.conf" {
			big = &facts.Files[i]
		}
	}
	if big == nil {
		t.Fatal("应列出 big.conf")
	}
	if !strings.Contains(big.Error, "上限") {
		t.Fatalf("应说明超过上限，实际 %q", big.Error)
	}
	if len(big.Servers) != 0 {
		t.Fatal("没有解析的文件不该有 server 块")
	}
}

// TestCollectSymlinkedSiteIsReadOnce 确认 sites-enabled 的符号链接被当成一份文件读，
// 而不是“链接一份、目标一份”报两遍。
//
// Windows 上建符号链接要权限，拿不到就跳过——跳过也要说清楚是为什么跳的。
func TestCollectSymlinkedSiteIsReadOnce(t *testing.T) {
	root, c := fakeHost(t, Options{})
	confRoot := filepath.Join(root, "etc-nginx")
	writeFile(t, filepath.Join(confRoot, "nginx.conf"), "http { include /etc/nginx/sites-enabled/*; }\n")
	target := filepath.Join(confRoot, "sites-available", "site")
	writeFile(t, target, "server {\n  listen 80;\n  server_name linked.example.com;\n}\n")
	if err := os.MkdirAll(filepath.Join(confRoot, "sites-enabled"), 0o755); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	link := filepath.Join(confRoot, "sites-enabled", "site")
	if err := os.Symlink(target, link); err != nil {
		t.Skip("当前系统建不了符号链接（" + err.Error() + "），这一条在 Linux 上跑")
	}

	facts, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("采集失败：%v", err)
	}
	if len(facts.Files) != 2 {
		t.Fatalf("应读到主配置与链接指向的文件各一次，实际 %d 个：%v", len(facts.Files), paths(facts.Files))
	}
	claims := facts.Lookup("linked.example.com")
	if len(claims.Matches) != 1 {
		t.Fatalf("应该只有一条声明，实际 %d 条", len(claims.Matches))
	}
	if filepath.Base(filepath.Dir(claims.Matches[0].File)) != "sites-enabled" {
		t.Fatalf("报的应是 sites-enabled 下的路径（用户看到的名字），实际 %s", claims.Matches[0].File)
	}
}

// TestCollectVersionAndUnit 确认版本与服务状态是探来的，探不到就带警告留空。
func TestCollectVersionAndUnit(t *testing.T) {
	root, _ := fakeHost(t, Options{})
	confRoot := filepath.Join(root, "etc-nginx")
	writeFile(t, filepath.Join(confRoot, "nginx.conf"), "events {}\n")

	ok := New(Options{
		ConfigRoot: confRoot,
		NginxPath:  filepath.Join(root, "nginx-bin"),
		Version:    func(context.Context) (string, error) { return "1.24.0", nil },
		Unit:       func(context.Context) (string, bool, error) { return "active", true, nil },
	})
	facts, err := ok.Collect(context.Background())
	if err != nil {
		t.Fatalf("采集失败：%v", err)
	}
	if facts.Version != "1.24.0" || facts.UnitState != "active" {
		t.Fatalf("版本与服务状态应是 1.24.0/active，实际 %q/%q", facts.Version, facts.UnitState)
	}

	bad := New(Options{
		ConfigRoot: confRoot,
		NginxPath:  filepath.Join(root, "nginx-bin"),
		Version:    func(context.Context) (string, error) { return "", errFake("exec 失败") },
		Unit:       func(context.Context) (string, bool, error) { return "", false, nil },
	})
	facts, err = bad.Collect(context.Background())
	if err != nil {
		t.Fatalf("采集失败：%v", err)
	}
	if facts.Version != "" {
		t.Fatalf("探不到版本时不该有版本，实际 %q", facts.Version)
	}
	joined := strings.Join(facts.Warnings, " | ")
	if !strings.Contains(joined, "读 nginx 版本失败") || !strings.Contains(joined, "没有 nginx.service") {
		t.Fatalf("两条都要警告，实际 %v", facts.Warnings)
	}
}

type errFake string

func (e errFake) Error() string { return string(e) }

// TestSnapshotCachesButCollectDoesNot 确认展示用快照会缓存、判断用采集不会。
//
// 写操作前的冲突判断必须看见刚手工加进去的配置，所以它不能吃缓存。
func TestSnapshotCachesButCollectDoesNot(t *testing.T) {
	root := t.TempDir()
	confRoot := filepath.Join(root, "etc-nginx")
	writeFile(t, filepath.Join(confRoot, "nginx.conf"), "events {}\n")

	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	c := New(Options{
		ConfigRoot: confRoot,
		NginxPath:  filepath.Join(root, "nginx-bin"),
		Clock:      func() time.Time { return now },
		CacheTTL:   5 * time.Second,
	})
	writeFile(t, filepath.Join(root, "nginx-bin"), "")

	if f, _ := c.Snapshot(context.Background()); len(f.Files) != 1 {
		t.Fatalf("起初应只有主配置，实际 %d 个", len(f.Files))
	}

	// 缓存窗口里有人手工加了一个文件。
	writeFile(t, filepath.Join(confRoot, "conf.d", "new.conf"), "server { server_name new.example.com; }\n")

	if f, _ := c.Snapshot(context.Background()); len(f.Files) != 1 {
		t.Fatalf("缓存期内快照应保持一份旧的，实际 %d 个", len(f.Files))
	}
	if f, _ := c.Collect(context.Background()); len(f.Files) != 2 {
		t.Fatalf("Collect 应看见新文件，实际 %d 个", len(f.Files))
	}

	now = now.Add(6 * time.Second)
	if f, _ := c.Snapshot(context.Background()); len(f.Files) != 2 {
		t.Fatalf("缓存过期后快照应重新采集，实际 %d 个", len(f.Files))
	}

	// Invalidate 之后即使没过期也要重读。
	writeFile(t, filepath.Join(confRoot, "conf.d", "third.conf"), "server { server_name t.example.com; }\n")
	c.Invalidate()
	if f, _ := c.Snapshot(context.Background()); len(f.Files) != 3 {
		t.Fatalf("Invalidate 之后应重读，实际 %d 个", len(f.Files))
	}
}

func paths(files []ConfigFile) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, filepath.Base(f.Path))
	}
	return out
}
