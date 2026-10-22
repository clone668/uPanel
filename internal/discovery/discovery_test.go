package discovery

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"upanel/internal/processes"
)

// fakeFS 是一份可以随意摆布的 /proc。测试用它精确控制“内核看起来是什么样”，
// 而不是依赖跑在 Windows 上的开发机真的有 /proc/net/tcp。
type fakeFS struct {
	files map[string]string
	dirs  map[string][]string
	links map[string]string
}

func newFakeFS() *fakeFS {
	return &fakeFS{files: map[string]string{}, dirs: map[string][]string{}, links: map[string]string{}}
}

func (f *fakeFS) ReadFile(name string) ([]byte, error) {
	if v, ok := f.files[name]; ok {
		return []byte(v), nil
	}
	return nil, errors.New("no such file")
}

func (f *fakeFS) ListDir(dir string) ([]string, error) {
	if v, ok := f.dirs[dir]; ok {
		return v, nil
	}
	return nil, errors.New("no such directory")
}

func (f *fakeFS) Readlink(name string) (string, error) {
	if v, ok := f.links[name]; ok {
		return v, nil
	}
	return "", errors.New("no such link")
}

var _ processes.FileSystem = (*fakeFS)(nil)

// newFakeFSWithEmptyNet 是一台“有 /proc/net/tcp 但没有任何监听”的机器。
func newFakeFSWithEmptyNet() *fakeFS {
	fs := newFakeFS()
	fs.files["/proc/net/tcp"] = ""
	fs.files["/proc/net/tcp6"] = ""
	return fs
}

// tcpLine 拼一行 /proc/net/tcp 正文（字段顺序与内核一致）。
func tcpLine(localHex string, state string, inode int) string {
	return fmt.Sprintf("   0: %s 00000000:0000 %s 00000000:00000000 00:00000000 00000000     0        0 %d 1 0000000000000000 100 0 0 10 0",
		localHex, state, inode)
}

func TestParseNetFileKeepsOnlyListeningTCP(t *testing.T) {
	body := strings.Join([]string{
		"  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode",
		tcpLine("0100007F:1F90", "0A", 111), // 127.0.0.1:8080 监听
		tcpLine("00000000:0050", "0A", 222), // 0.0.0.0:80 监听
		tcpLine("0100007F:9C40", "01", 333), // 已建立连接，不是监听
	}, "\n")

	socks, err := parseNetFile([]byte(body), false)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(socks) != 2 {
		t.Fatalf("监听项 = %d，期望 2（只收 st=0A）", len(socks))
	}
	if socks[0].address != "127.0.0.1" || socks[0].port != 8080 || !socks[0].loopback {
		t.Fatalf("第一项 = %+v", socks[0])
	}
	if socks[1].address != "0.0.0.0" || socks[1].port != 80 || !socks[1].loopback {
		t.Fatalf("通配地址应当算回环可达，实际 %+v", socks[1])
	}
	if socks[0].inode != 111 {
		t.Fatalf("inode = %d，期望 111", socks[0].inode)
	}
}

func TestParseNetFileIPv6(t *testing.T) {
	body := strings.Join([]string{
		"  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode",
		tcpLine("00000000000000000000000001000000:1F90", "0A", 1), // ::1:8080
		tcpLine("00000000000000000000000000000000:01BB", "0A", 2), // [::]:443
	}, "\n")
	socks, err := parseNetFile([]byte(body), true)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(socks) != 2 {
		t.Fatalf("监听项 = %d，期望 2", len(socks))
	}
	if socks[0].address != "::1" || socks[0].port != 8080 || !socks[0].loopback {
		t.Fatalf("IPv6 回环 = %+v", socks[0])
	}
	if socks[1].address != "::" || socks[1].port != 443 || !socks[1].loopback {
		t.Fatalf("IPv6 通配 = %+v", socks[1])
	}
}

// TestGroupsAttachListenersToUnits 确认“有单元的按单元、没单元的按进程”这条分组规则，
// 以及 user@1000.service 这类每用户管理器不会被当成一个服务。
func TestGroupsAttachListenersToUnits(t *testing.T) {
	fs := newFakeFS()
	fs.dirs["/proc"] = []string{"100", "200", "300", "302"}
	fs.dirs["/proc/100/fd"] = []string{"3", "4"}
	fs.dirs["/proc/200/fd"] = []string{"5"}
	fs.dirs["/proc/300/fd"] = []string{"6"}
	fs.dirs["/proc/302/fd"] = []string{"7"}
	// 单元 nginx.service 的主进程 100 监听 80 与 443。
	fs.links["/proc/100/fd/3"] = "socket:[1000]"
	fs.links["/proc/100/fd/4"] = "socket:[1001]"
	// 容器发布端口上的 docker-proxy（200）。
	fs.links["/proc/200/fd/5"] = "socket:[2000]"
	// 两个属于 user@1000.service 的进程各自监听一个端口。
	fs.links["/proc/300/fd/6"] = "socket:[3000]"
	fs.links["/proc/302/fd/7"] = "socket:[3002]"
	fs.files["/proc/100/comm"] = "nginx\n"
	fs.files["/proc/100/cgroup"] = "0::/system.slice/nginx.service\n"
	fs.files["/proc/200/comm"] = "docker-proxy\n"
	fs.files["/proc/200/cgroup"] = "0::/system.slice/docker.service\n"
	fs.files["/proc/300/comm"] = "python3\n"
	fs.files["/proc/300/cgroup"] = "0::/user.slice/user-1000.slice/user@1000.service/app.slice/app-1.scope\n"
	fs.files["/proc/302/comm"] = "node\n"
	fs.files["/proc/302/cgroup"] = "0::/user.slice/user-1000.slice/user@1000.service/app.slice/app-2.scope\n"

	socks := []socket{
		{address: "127.0.0.1", port: 80, inode: 1000, loopback: true},
		{address: "0.0.0.0", port: 443, inode: 1001, loopback: true},
		{address: "0.0.0.0", port: 8080, inode: 2000, loopback: true},
		{address: "127.0.0.1", port: 9000, inode: 3000, loopback: true},
		{address: "127.0.0.1", port: 9001, inode: 3002, loopback: true},
		{address: "10.0.0.5", port: 7000, inode: 0, loopback: false},
	}
	inodes := map[uint64]struct{}{1000: {}, 1001: {}, 2000: {}, 3000: {}, 3002: {}}
	owners := socketOwners(fs, "/proc", inodes)
	groups := groupListeners(listenTable{sockets: socks}, owners)

	byKey := map[string]listenerGroup{}
	for _, g := range groups {
		byKey[g.key] = g
	}
	if g, ok := byKey["unit:nginx.service"]; !ok || len(g.listeners) != 2 {
		t.Fatalf("nginx.service 的监听 = %+v", g.listeners)
	}
	if _, ok := byKey["unit:user@1000.service"]; ok {
		t.Fatal("user@1000.service 不该成为一个候选单元：它的进程应当按裸监听分组")
	}
	if g, ok := byKey["pid:300"]; !ok || g.comm != "python3" {
		t.Fatalf("裸监听应当按进程分组，实际 %+v", g)
	}
	// inode 为 0 的监听（读不到持有进程）仍然出现，按端口自成一组。
	found := false
	for _, g := range groups {
		if g.comm == "" && len(g.listeners) == 1 && g.listeners[0].Port == 7000 {
			found = true
		}
	}
	if !found {
		t.Fatalf("读不到持有进程的监听必须照样列出来，实际分组 %+v", groups)
	}
}

func TestParseDockerPS(t *testing.T) {
	body := strings.Join([]string{
		"abc123|blog|ghcr.io/x/blog:1.2|running|0.0.0.0:8080->80/tcp, :::8080->80/tcp",
		"def456|db|postgres:16|exited|",
		"ghi789|only-expose|img:1|running|3000/tcp",
	}, "\n")
	containers, err := parseDockerPS(body)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(containers) != 3 {
		t.Fatalf("容器数 = %d", len(containers))
	}
	if containers[0].Name != "blog" || len(containers[0].Ports) != 2 {
		t.Fatalf("第一个容器 = %+v", containers[0])
	}
	if m := containers[0].Ports[1]; m.HostIP != "::" || m.HostPort != 8080 || m.ContainerPort != 80 {
		t.Fatalf("IPv6 通配映射解析 = %+v", m)
	}
	if len(containers[1].Ports) != 0 {
		t.Fatalf("没有发布的容器不该有映射: %+v", containers[1].Ports)
	}
	if len(containers[2].Ports) != 0 {
		t.Fatalf("只 expose 不 publish 不算映射: %+v", containers[2].Ports)
	}
	if containerState("restarting") != ObservedUnknown {
		t.Fatalf("restarting 必须映射成 unknown，而不是 running")
	}
	if !loopbackReachable("0.0.0.0") || !loopbackReachable("") || loopbackReachable("10.0.0.5") {
		t.Fatal("回环可达判断不对")
	}
}

// TestCollectBuildsCandidatesAndTargets 用一个完整的假现场跑一遍采集。
// TestCollectDropsShadowedSocketActivation 确认 pid 1 代持的 socket 单元不会和真正的
// 守护进程在候选列表里各占一行。
//
// 真机上就是这个样子：ssh.socket 的监听套接字由 pid 1 持有，sshd 启动后自己也持有同一个
// 端口，于是“0.0.0.0:22”在套接字表里出现两次、分属两个 pid。两条一模一样的“端口 22”
// 对确认候选的人没有意义——端口归那个有人认领的进程；pid 1 独占的端口仍然是候选
// （套接字开着、服务还没被激活）。
func TestCollectDropsShadowedSocketActivation(t *testing.T) {
	fs := newFakeFS()
	fs.files["/proc/net/tcp"] = strings.Join([]string{
		"  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode",
		tcpLine("00000000:0016", "0A", 600), // 0.0.0.0:22，pid 1 与 sshd 都持有
		tcpLine("00000000:006F", "0A", 601), // 0.0.0.0:111，只有 pid 1 持有
	}, "\n")
	fs.files["/proc/net/tcp6"] = ""
	fs.dirs["/proc"] = []string{"1", "5"}
	fs.dirs["/proc/1/fd"] = []string{"3", "4"}
	fs.dirs["/proc/5/fd"] = []string{"3"}
	fs.links["/proc/1/fd/3"] = "socket:[600]"
	fs.links["/proc/1/fd/4"] = "socket:[601]"
	fs.links["/proc/5/fd/3"] = "socket:[600]"
	fs.files["/proc/1/comm"] = "systemd\n"
	fs.files["/proc/1/cgroup"] = "0::/init.scope\n"
	fs.files["/proc/5/comm"] = "sshd\n"
	fs.files["/proc/5/cgroup"] = "0::/\n"

	collector := NewCollector(Deps{
		FS:       fs,
		ProcRoot: "/proc",
		Units:    fakeUnits{},
	})
	snap := collector.Collect(context.Background())

	if len(snap.Candidates) != 2 {
		t.Fatalf("候选数 = %d，期望 2（sshd 的 22 + pid 1 独占的 111）；实际 %+v",
			len(snap.Candidates), snap.Candidates)
	}
	seen := map[int]string{}
	for _, c := range snap.Candidates {
		if len(c.Listeners) != 1 {
			t.Fatalf("候选 %s 的监听项 = %+v", c.TargetID, c.Listeners)
		}
		port := c.Listeners[0].Port
		if prev, dup := seen[port]; dup {
			t.Fatalf("端口 %d 出现了两次：%s 与 %s", port, prev, c.TargetID)
		}
		seen[port] = c.TargetID
		if port == 22 && c.Name != "sshd" {
			t.Fatalf("22 应当归 sshd，实际候选 = %+v", c)
		}
		if port == 111 {
			if c.Name != "systemd" {
				t.Fatalf("111 应当归 pid 1，实际候选 = %+v", c)
			}
			if !strings.Contains(c.Reason, "socket 单元") {
				t.Fatalf("pid 1 独占的原因要说清楚是代持的套接字: %q", c.Reason)
			}
		}
	}
}

// TestCollectDropsDockerProxyInsideDockerUnit 确认 docker-proxy 即使跑在 docker.service
// 的 cgroup 里（于是会被认成“docker.service 在监听”），也不会变成第二个候选。
//
// 真机上的样子：容器发布 19487 → docker-proxy 监听 19487，它的 cgroup 是 docker.service。
// 先按单元分类的话，候选里会出现“容器 p092-cnt-bad”和“docker.service 在监听 19487”两条。
// 同时 dockerd 自己要是有别的监听端口（比如 2375），那条必须留着——摘的是代理的监听，
// 不是整个单元。
func TestCollectDropsDockerProxyInsideDockerUnit(t *testing.T) {
	fs := newFakeFS()
	fs.files["/proc/net/tcp"] = strings.Join([]string{
		"  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode",
		tcpLine("00000000:4C1F", "0A", 700), // 0.0.0.0:19487，容器发布的端口
		tcpLine("00000000:0947", "0A", 701), // 0.0.0.0:2375，dockerd 自己的 API
	}, "\n")
	fs.files["/proc/net/tcp6"] = ""
	fs.dirs["/proc"] = []string{"30"}
	fs.dirs["/proc/30/fd"] = []string{"5", "6"}
	fs.links["/proc/30/fd/5"] = "socket:[700]"
	fs.links["/proc/30/fd/6"] = "socket:[701]"
	fs.files["/proc/30/comm"] = "docker-proxy\n"
	fs.files["/proc/30/cgroup"] = "0::/system.slice/docker.service\n"

	collector := NewCollector(Deps{
		FS:       fs,
		ProcRoot: "/proc",
		Units: fakeUnits{units: []UnitInfo{
			{Unit: "docker.service", Description: "Docker Application Container Engine", ActiveState: "active", MainPID: ptrInt64(30)},
		}},
		Docker: fakeDocker{available: true, containers: []Container{{
			ID: strings.Repeat("b", 64), Name: "p092-cnt-bad", Image: "alpine:3.20", State: "running",
			Ports: []PortMapping{{HostIP: "0.0.0.0", HostPort: 19487, ContainerPort: 80, Protocol: "tcp"}},
		}}},
	})
	snap := collector.Collect(context.Background())

	seen := map[int][]string{}
	for _, c := range snap.Candidates {
		for _, l := range c.Listeners {
			seen[l.Port] = append(seen[l.Port], c.TargetID)
		}
	}
	if got := len(seen[19487]); got != 1 {
		t.Fatalf("19487 应当只属于一个候选（容器），实际 %v", seen[19487])
	}
	if ids := seen[19487]; len(ids) == 1 && !strings.HasPrefix(ids[0], TargetContainerPrefix) {
		t.Fatalf("19487 应当归容器候选，实际 %v", ids)
	}
	if got := len(seen[2375]); got != 1 {
		t.Fatalf("2375 是 dockerd 自己的监听，必须留着（不该被当成代理监听摘掉），实际 %v", seen[2375])
	}
}

func TestCollectBuildsCandidatesAndTargets(t *testing.T) {
	fs := newFakeFS()
	fs.files["/proc/net/tcp"] = strings.Join([]string{
		"  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode",
		tcpLine("0100007F:0050", "0A", 500),  // nginx 80
		tcpLine("0100007F:2328", "0A", 501),  // 容器发布的 9000（0x2328），由 docker-proxy 监听
		tcpLine("127.0.0.1:0000", "0A", 999), // 占位，不会被解析（格式非法）
	}, "\n")
	fs.files["/proc/net/tcp6"] = ""
	fs.dirs["/proc"] = []string{"10", "20"}
	fs.links["/proc/10/fd/3"] = "socket:[500]"
	fs.links["/proc/20/fd/4"] = "socket:[501]"
	fs.files["/proc/10/comm"] = "nginx\n"
	fs.files["/proc/10/cgroup"] = "0::/system.slice/nginx.service\n"
	fs.files["/proc/20/comm"] = "docker-proxy\n"
	fs.files["/proc/20/cgroup"] = "0::/system.slice/docker.service\n"
	fs.dirs["/proc/10/fd"] = []string{"3"}
	fs.dirs["/proc/20/fd"] = []string{"4"}

	collector := NewCollector(Deps{
		FS:       fs,
		ProcRoot: "/proc",
		Units: fakeUnits{units: []UnitInfo{
			{Unit: "nginx.service", Description: "A high performance web server", ActiveState: "active", MainPID: ptrInt64(10)},
		}},
		Docker: fakeDocker{available: true, containers: []Container{{
			ID: strings.Repeat("a", 64), Name: "blog", Image: "ghcr.io/x/blog:1.2", State: "running",
			Ports: []PortMapping{{HostIP: "0.0.0.0", HostPort: 9000, ContainerPort: 80, Protocol: "tcp"}},
		}}},
	})
	snap := collector.Collect(context.Background())

	if snap.Status != StatusOK {
		t.Fatalf("状态 = %q（%s）", snap.Status, snap.Error)
	}
	if len(snap.Candidates) != 2 {
		t.Fatalf("候选数 = %d，期望 2（nginx + 容器）；实际 %+v", len(snap.Candidates), snap.Candidates)
	}

	var nginx, blog *Candidate
	for i := range snap.Candidates {
		switch snap.Candidates[i].Kind {
		case KindSystemd:
			nginx = &snap.Candidates[i]
		case KindDocker:
			blog = &snap.Candidates[i]
		}
	}
	if nginx == nil || blog == nil {
		t.Fatalf("候选类型不对: %+v", snap.Candidates)
	}
	if nginx.TargetID != "local_nginx" || !ValidTargetID(nginx.TargetID) {
		t.Fatalf("单元候选标识 = %q", nginx.TargetID)
	}
	if nginx.ObservedState != ObservedRunning || nginx.SuggestedPort != 80 || !nginx.ProbeAvailable {
		t.Fatalf("单元候选 = %+v", nginx)
	}
	if !strings.Contains(nginx.Reason, "systemd 单元 nginx.service") {
		t.Fatalf("发现来源说明缺失: %q", nginx.Reason)
	}
	if blog.TargetID != "cntr_blog" || !ValidTargetID(blog.TargetID) {
		t.Fatalf("容器候选标识 = %q", blog.TargetID)
	}
	// 容器发布的 9000 与 docker-proxy 监听的 8080 是两回事：端口映射写的是 9000，
	// 而 docker-proxy 监听在 8080——这里断言面板认的是“映射里写的宿主机端口”。
	target, ok := snap.Targets["cntr_blog"]
	if !ok {
		t.Fatal("容器候选必须解析成探测目标")
	}
	if len(target.PublishedPorts) != 1 || target.PublishedPorts[0] != 9000 {
		t.Fatalf("容器发布端口 = %v", target.PublishedPorts)
	}
	if _, ok := snap.Targets["local_nginx"]; !ok {
		t.Fatal("单元候选必须解析成探测目标")
	}
}

// TestDockerUnavailableSaysWhy 确认“没装 docker”与“docker 没在跑”分别说清楚。
func TestDockerUnavailableSaysWhy(t *testing.T) {
	collector := NewCollector(Deps{
		FS:       newFakeFSWithEmptyNet(),
		ProcRoot: "/proc",
		Docker:   fakeDocker{available: false, notice: dockerUnavailableNoSocket},
	})
	snap := collector.Collect(context.Background())
	var state *SourceState
	for i := range snap.Sources {
		if snap.Sources[i].Source == SourceDocker {
			state = &snap.Sources[i]
		}
	}
	if state == nil || state.Status != "unavailable" {
		t.Fatalf("容器来源状态 = %+v", snap.Sources)
	}
	if !strings.Contains(state.Error, "守护进程没有运行") {
		t.Fatalf("失败原因没写清楚: %q", state.Error)
	}
	if snap.Status != StatusPartial {
		// 监听来源成功、单元来源不可用、容器来源不可用 → partial
		t.Fatalf("总状态 = %q，期望 partial", snap.Status)
	}
}

// TestProbeStates 覆盖探测的四种结论：ok、状态码不在期望集合、连接失败、目标不存在。
func TestProbeStates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			w.WriteHeader(http.StatusOK)
		case "/boom":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, "boom")
		case "/redirect":
			http.Redirect(w, r, "/ok", http.StatusFound)
		}
	}))
	defer srv.Close()
	_, portStr, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("解析测试端口失败: %v", err)
	}
	port := 0
	fmt.Sscanf(portStr, "%d", &port)

	collector := collectorWithTarget(t, port, Target{ID: "local_app", Kind: KindLocal, Name: "app", Address: "127.0.0.1", State: ObservedRunning})

	ok, err := collector.Probe(context.Background(), ProbeParams{TargetID: "local_app", Port: port, Path: "/ok"})
	if err != nil {
		t.Fatalf("探测 /ok 失败: %v", err)
	}
	if ok.HTTPState != HTTPOK || ok.StatusCode == nil || *ok.StatusCode != 200 || ok.TargetFound != true {
		t.Fatalf("探测 /ok = %+v", ok)
	}
	if !strings.HasPrefix(ok.URL, "http://127.0.0.1:") {
		t.Fatalf("探测地址必须只指向回环，实际 %q", ok.URL)
	}

	boom, err := collector.Probe(context.Background(), ProbeParams{TargetID: "local_app", Port: port, Path: "/boom"})
	if err != nil {
		t.Fatalf("探测 /boom 失败: %v", err)
	}
	if boom.HTTPState != HTTPError || boom.StatusCode == nil || *boom.StatusCode != 500 {
		t.Fatalf("500 必须明确是 error: %+v", boom)
	}
	if !strings.Contains(boom.Error, "500") {
		t.Fatalf("错误说明里应当有状态码: %q", boom.Error)
	}

	// 302 落在默认的 200—399 里；不跟随重定向，看到的就是 302。
	redirect, err := collector.Probe(context.Background(), ProbeParams{TargetID: "local_app", Port: port, Path: "/redirect"})
	if err != nil {
		t.Fatalf("探测 /redirect 失败: %v", err)
	}
	if redirect.HTTPState != HTTPOK || redirect.StatusCode == nil || *redirect.StatusCode != 302 {
		t.Fatalf("302 在默认集合里应当是 ok，且不跟随重定向: %+v", redirect)
	}

	// 只期望 500 时，200 就是 error。
	strict, err := collector.Probe(context.Background(), ProbeParams{
		TargetID: "local_app", Port: port, Path: "/ok", ExpectedStatusCodes: []int{500},
	})
	if err != nil {
		t.Fatalf("探测失败: %v", err)
	}
	if strict.HTTPState != HTTPError {
		t.Fatalf("期望集合只有 500 时，200 必须是 error: %+v", strict)
	}

	// 发现成功了但目标不在：这是结论（它不在），不是“不知道”。
	missing, err := collector.Probe(context.Background(), ProbeParams{TargetID: "local_nothere", Port: port, Path: "/"})
	if err != nil {
		t.Fatalf("探测未知目标失败: %v", err)
	}
	if missing.TargetFound || missing.HTTPState != HTTPError || missing.Error == "" {
		t.Fatalf("未知目标 = %+v", missing)
	}
	// 没发出去的请求没有地址可说。
	if missing.Address != "" || missing.URL != "" {
		t.Fatalf("目标不在快照里时不该有请求地址: address=%q url=%q", missing.Address, missing.URL)
	}
	if !strings.Contains(missing.Error, "不在本机当前的监听/容器列表里") {
		t.Fatalf("缺席原因没写清楚: %q", missing.Error)
	}
}

// TestProbeMissingTargetVerdict 确认“目标不在快照里”按快照状态分开记：
// 真没看成本机（error / 相关来源失败）→ unknown；看成了 → error。
func TestProbeMissingTargetVerdict(t *testing.T) {
	cases := []struct {
		name    string
		snap    Snapshot
		target  string
		want    string
		wantSub string
	}{
		{
			name:    "发现整体失败",
			snap:    Snapshot{Status: StatusError, Error: "没有任何来源可用"},
			target:  "local_nginx",
			want:    HTTPUnknown,
			wantSub: "本轮发现没有成功",
		},
		{
			name: "容器来源失败，缺的是容器目标",
			snap: Snapshot{Status: StatusPartial, Sources: []SourceState{
				{Source: SourceLocalListen, Status: StatusOK},
				{Source: SourceDocker, Status: "unavailable", Error: "守护进程没有运行"},
			}},
			target:  "cntr_blog",
			want:    HTTPUnknown,
			wantSub: "来源 docker 本轮没有采集成功",
		},
		{
			name: "容器来源失败，缺的是本机目标",
			snap: Snapshot{Status: StatusPartial, Sources: []SourceState{
				{Source: SourceLocalListen, Status: StatusOK},
				{Source: SourceDocker, Status: "unavailable", Error: "守护进程没有运行"},
			}},
			target:  "local_nginx",
			want:    HTTPError,
			wantSub: "不在本机当前的监听/容器列表里",
		},
		{
			name:    "全部成功，目标就是不在",
			snap:    Snapshot{Status: StatusOK},
			target:  "local_gone",
			want:    HTTPError,
			wantSub: "不在本机当前的监听/容器列表里",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := NewCollector(Deps{FS: newFakeFS(), ProcRoot: "/proc", TTL: 1 << 30})
			c.snap = tc.snap
			c.snap.Targets = map[string]Target{}
			c.snap.CollectedAt = c.deps.Now()
			c.has = true

			result, err := c.Probe(context.Background(), ProbeParams{TargetID: tc.target, Port: 8080, Path: "/"})
			if err != nil {
				t.Fatalf("探测失败: %v", err)
			}
			if result.HTTPState != tc.want {
				t.Fatalf("结论 = %q（%s），期望 %q", result.HTTPState, result.Error, tc.want)
			}
			if !strings.Contains(result.Error, tc.wantSub) {
				t.Fatalf("原因 = %q，期望包含 %q", result.Error, tc.wantSub)
			}
			if result.Address != "" || result.URL != "" {
				t.Fatalf("没发请求不该填地址: address=%q url=%q", result.Address, result.URL)
			}
		})
	}
}

// TestProbeContainerNoPublishedPort 确认容器没发布端口时不发请求、地址留空。
func TestProbeContainerNoPublishedPort(t *testing.T) {
	c := collectorWithTarget(t, 8080, Target{
		ID: "cntr_blog", Kind: KindDocker, Name: "blog", Address: "127.0.0.1",
		State: ObservedRunning,
	})
	result, err := c.Probe(context.Background(), ProbeParams{TargetID: "cntr_blog", Port: 8080, Path: "/"})
	if err != nil {
		t.Fatalf("探测失败: %v", err)
	}
	if result.HTTPState != HTTPError || !strings.Contains(result.Error, "没有把端口发布到宿主机回环上") {
		t.Fatalf("容器未发布端口的结论 = %+v", result)
	}
	if result.Address != "" || result.URL != "" {
		t.Fatalf("没发请求不该填地址: address=%q url=%q", result.Address, result.URL)
	}
}

// TestProbeNeverAcceptsArbitraryHost 确认探测参数里没有任何地方能塞进一个 host。
func TestProbeNeverAcceptsArbitraryHost(t *testing.T) {
	collector := collectorWithTarget(t, 1, Target{ID: "local_app", Address: "127.0.0.1"})
	bad := []ProbeParams{
		{TargetID: "169.254.169.254", Port: 80, Path: "/"},          // 云元数据地址
		{TargetID: "http://local_1", Port: 80, Path: "/"},           // 完整 URL
		{TargetID: "local_app", Port: 80, Path: "http://evil/x"},    // path 里塞 URL
		{TargetID: "local_app", Port: 80, Path: "//evil.example/x"}, // 协议相对 URL
		{TargetID: "local_app", Port: 0, Path: "/"},                 // 没有端口
		{TargetID: "local_app", Port: 70000, Path: "/"},             // 端口越界
		{TargetID: "local_app", Port: 80, Path: "/", Scheme: "ftp"},
		{TargetID: "local_app", Port: 80, Path: "/", TimeoutSeconds: 60},
	}
	for _, params := range bad {
		if _, err := collector.Probe(context.Background(), params); !errors.Is(err, ErrProbeParams) {
			t.Fatalf("参数 %+v 应当被判为不合法，实际 err=%v", params, err)
		}
	}
}

// TestProbeContainerRequiresPublishedPort 确认容器端口配错时说清楚是端口没发布。
func TestProbeContainerRequiresPublishedPort(t *testing.T) {
	collector := collectorWithTarget(t, 8080, Target{
		ID: "cntr_blog", Kind: KindDocker, Name: "blog", Address: "127.0.0.1",
		PublishedPorts: []int{8080}, State: ObservedRunning,
	})
	result, err := collector.Probe(context.Background(), ProbeParams{TargetID: "cntr_blog", Port: 9999, Path: "/"})
	if err != nil {
		t.Fatalf("探测失败: %v", err)
	}
	if result.HTTPState != HTTPError || !strings.Contains(result.Error, "没有把宿主机端口 9999 发布出来") {
		t.Fatalf("容器端口未发布的结论 = %+v", result)
	}
}

// collectorWithTarget 造一个只有固定快照的收集器（不读 /proc、不跑 docker）。
func collectorWithTarget(t *testing.T, port int, target Target) *Collector {
	t.Helper()
	c := NewCollector(Deps{FS: newFakeFS(), ProcRoot: "/proc", TTL: 1 << 30})
	if target.ID == "" {
		target.ID = "local_app"
	}
	c.snap = Snapshot{
		Status:      StatusOK,
		CollectedAt: c.deps.Now(),
		Candidates:  []Candidate{{TargetID: target.ID, Kind: target.Kind, Name: target.Name}},
		Targets:     map[string]Target{target.ID: target},
	}
	c.has = true
	return c
}

type fakeUnits struct {
	units []UnitInfo
	fail  bool
}

func (f fakeUnits) Units() ([]UnitInfo, bool, string) {
	if f.fail {
		return nil, false, "systemctl 读不出来"
	}
	return f.units, true, ""
}

type fakeDocker struct {
	containers []Container
	available  bool
	notice     string
	err        error
}

func (f fakeDocker) Containers(context.Context) ([]Container, bool, string, error) {
	return f.containers, f.available, f.notice, f.err
}

func ptrInt64(v int64) *int64 { return &v }
