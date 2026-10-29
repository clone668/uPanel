package network

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path"
	"strings"
	"testing"
	"time"
)

// ---- 测试替身 ----

// fakeFS 是一棵内存里的小文件系统。目录用目录项存在来表示（fs 语义：目录能被 ListDir）。
type fakeFS struct {
	files map[string]string
	dirs  map[string][]string
	links map[string]string
	// syncedDirs 记录 rename 之后刷过哪些目录；syncDirErr 让用例模拟"目录刷不下去"。
	syncedDirs []string
	syncDirErr error
}

func newFakeFS() *fakeFS {
	return &fakeFS{files: map[string]string{}, dirs: map[string][]string{}, links: map[string]string{}}
}

func (f *fakeFS) file(name, content string) *fakeFS { f.files[name] = content; return f }

func (f *fakeFS) dir(name string, entries ...string) *fakeFS {
	f.dirs[name] = append(f.dirs[name], entries...)
	return f
}

func (f *fakeFS) link(name, target string) *fakeFS { f.links[name] = target; return f }

// ---- 写能力（P14.2 的写路径测试用） ----
//
// 假的文件系统把"文件名出现在目录里"和"文件有内容"当成同一件事来维护：真机上
// rename 之后目录项与内容一起变，这里若只改其中一半，测出来的"文件在不在"就是假的。

func (f *fakeFS) touchEntry(dir, name string) {
	for _, existing := range f.dirs[dir] {
		if existing == name {
			return
		}
	}
	f.dirs[dir] = append(f.dirs[dir], name)
}

func (f *fakeFS) dropEntry(dir, name string) {
	kept := f.dirs[dir][:0]
	for _, existing := range f.dirs[dir] {
		if existing != name {
			kept = append(kept, existing)
		}
	}
	f.dirs[dir] = kept
}

func (f *fakeFS) WriteFile(name string, data []byte, _ os.FileMode) error {
	f.files[name] = string(data)
	f.touchEntry(path.Dir(name), path.Base(name))
	return nil
}

func (f *fakeFS) Rename(oldpath, newpath string) error {
	content, ok := f.files[oldpath]
	if !ok {
		return &fs.PathError{Op: "rename", Path: oldpath, Err: fs.ErrNotExist}
	}
	f.files[newpath] = content
	delete(f.files, oldpath)
	f.dropEntry(path.Dir(oldpath), path.Base(oldpath))
	f.touchEntry(path.Dir(newpath), path.Base(newpath))
	return nil
}

// SyncDir 记下这一次刷盘，好让用例能断言"rename 之后确实刷了目录"。
func (f *fakeFS) SyncDir(dir string) error {
	if f.syncDirErr != nil {
		return f.syncDirErr
	}
	f.syncedDirs = append(f.syncedDirs, dir)
	return nil
}

func (f *fakeFS) Remove(name string) error {
	if _, ok := f.files[name]; !ok {
		return &fs.PathError{Op: "remove", Path: name, Err: fs.ErrNotExist}
	}
	delete(f.files, name)
	f.dropEntry(path.Dir(name), path.Base(name))
	return nil
}

func (f *fakeFS) MkdirAll(dir string, _ os.FileMode) error {
	if _, ok := f.dirs[dir]; !ok {
		f.dirs[dir] = nil
	}
	return nil
}

// fakeFileInfo 是 Stat 的返回值，只提供写路径用得上的两件事：在不在、是不是目录。
type fakeFileInfo struct {
	name string
	dir  bool
}

func (i fakeFileInfo) Name() string       { return i.name }
func (i fakeFileInfo) Size() int64        { return 0 }
func (i fakeFileInfo) Mode() fs.FileMode  { return 0 }
func (i fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (i fakeFileInfo) IsDir() bool        { return i.dir }
func (i fakeFileInfo) Sys() any           { return nil }

func (f *fakeFS) Stat(name string) (os.FileInfo, error) {
	if _, ok := f.files[name]; ok {
		return fakeFileInfo{name: path.Base(name)}, nil
	}
	if _, ok := f.dirs[name]; ok {
		return fakeFileInfo{name: path.Base(name), dir: true}, nil
	}
	return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
}

func (f *fakeFS) ReadFile(name string) ([]byte, error) {
	if content, ok := f.files[name]; ok {
		return []byte(content), nil
	}
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

func (f *fakeFS) ListDir(dir string) ([]string, error) {
	if entries, ok := f.dirs[dir]; ok {
		return entries, nil
	}
	return nil, &fs.PathError{Op: "open", Path: dir, Err: fs.ErrNotExist}
}

func (f *fakeFS) Readlink(name string) (string, error) {
	if target, ok := f.links[name]; ok {
		return target, nil
	}
	return "", &fs.PathError{Op: "readlink", Path: name, Err: fs.ErrNotExist}
}

// fakeRunner 按 "程序 参数..." 匹配预设输出。
type fakeRunner struct {
	replies map[string]fakeReply
	calls   []string
}

type fakeReply struct {
	stdout string
	code   int
	err    error
}

func newFakeRunner() *fakeRunner { return &fakeRunner{replies: map[string]fakeReply{}} }

func (r *fakeRunner) reply(key, stdout string) *fakeRunner {
	r.replies[key] = fakeReply{stdout: stdout}
	return r
}

func (r *fakeRunner) fail(key string, err error) *fakeRunner {
	r.replies[key] = fakeReply{err: err}
	return r
}

func (r *fakeRunner) exit(key string, code int) *fakeRunner {
	r.replies[key] = fakeReply{code: code}
	return r
}

func (r *fakeRunner) Run(_ context.Context, program string, args ...string) ([]byte, int, error) {
	key := strings.TrimSpace(program + " " + strings.Join(args, " "))
	r.calls = append(r.calls, key)
	reply, ok := r.replies[key]
	if !ok {
		return nil, 0, ErrProgramMissing
	}
	return []byte(reply.stdout), reply.code, reply.err
}

func testDeps(filesystem *fakeFS, runner *fakeRunner) Deps {
	return Deps{
		FS:         filesystem,
		Runner:     runner,
		ProcRoot:   "/proc",
		SysRoot:    "/sys",
		NetplanDir: "/etc/netplan",
		RunDir:     "/run",
		Timeout:    time.Second,
		Now:        func() time.Time { return time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC) },
	}
}

// 一台最小的、netplan + networkd 的单网卡机器。
func simpleHost() (*fakeFS, *fakeRunner) {
	f := newFakeFS()
	f.dir("/sys/class/net", "lo", "ens4")
	f.dir("/sys/class/net/lo")
	f.file("/sys/class/net/lo/type", "772\n")
	f.dir("/sys/class/net/ens4")
	f.file("/sys/class/net/ens4/type", "1\n")
	f.file("/sys/class/net/ens4/mtu", "1500\n")
	f.file("/sys/class/net/ens4/address", "52:54:00:AA:BB:CC\n")
	f.file("/sys/class/net/ens4/carrier", "1\n")
	f.link("/sys/class/net/ens4/device", "../../../0000:00:03.0")
	f.dir("/etc/netplan", "50-cloud-init.yaml")
	f.dir("/run/systemd/network", "10-netplan-ens4.network")
	f.dir("/proc")

	r := newFakeRunner()
	r.reply("ip -j addr show", `[
	  {"ifindex":1,"ifname":"lo","mtu":65536,"operstate":"UNKNOWN","address":"00:00:00:00:00:00",
	   "addr_info":[{"family":"inet","local":"127.0.0.1","prefixlen":8,"scope":"host"},
	                {"family":"inet6","local":"::1","prefixlen":128,"scope":"host"}]},
	  {"ifindex":2,"ifname":"ens4","mtu":1500,"operstate":"UP","address":"52:54:00:aa:bb:cc",
	   "addr_info":[{"family":"inet","local":"192.168.1.10","prefixlen":24,"scope":"global","type":"primary"},
	                {"family":"inet6","local":"fe80::5054:ff:feaa:bbcc","prefixlen":64,"scope":"link"}]}
	]`)
	r.reply("ip -j route show", `[{"dst":"default","gateway":"192.168.1.1","dev":"ens4","protocol":"dhcp","metric":100},
	  {"dst":"192.168.1.0/24","dev":"ens4","protocol":"kernel","scope":"link","prefsrc":"192.168.1.10"}]`)
	r.reply("ip -j -6 route show", `[{"dst":"default","gateway":"fe80::1","dev":"ens4","metric":1024},
	  {"dst":"fe80::/64","dev":"ens4","protocol":"kernel"}]`)
	return f, r
}

// ---- 网卡与地址 ----

func TestInterfacesReadsAddressesAndSysfsFacts(t *testing.T) {
	f, r := simpleHost()
	list := Interfaces(context.Background(), testDeps(f, r))

	if len(list.Issues) != 0 {
		t.Fatalf("不该有 issue，得到 %+v", list.Issues)
	}
	if len(list.Interfaces) != 2 {
		t.Fatalf("网卡数 = %d，期望 2", len(list.Interfaces))
	}
	ens4, lo := list.Interfaces[0], list.Interfaces[1] // 按名字排序：ens4 < lo

	if ens4.Name != "ens4" || lo.Name != "lo" {
		t.Fatalf("排序不对：%s, %s", ens4.Name, lo.Name)
	}
	if ens4.Kind != KindEthernet || !ens4.Physical {
		t.Errorf("ens4 应是物理以太网卡，得到 kind=%s physical=%v", ens4.Kind, ens4.Physical)
	}
	if ens4.State != "up" {
		t.Errorf("operstate 应被转成小写 up，得到 %q", ens4.State)
	}
	if ens4.Carrier == nil || !*ens4.Carrier {
		t.Errorf("carrier 应为 true，得到 %v", ens4.Carrier)
	}
	if ens4.MAC != "52:54:00:aa:bb:cc" {
		t.Errorf("MAC 应小写，得到 %q", ens4.MAC)
	}
	if len(ens4.Addresses) != 2 {
		t.Fatalf("ens4 地址数 = %d，期望 2（v4 + v6 链路本地）", len(ens4.Addresses))
	}
	if ens4.Addresses[0].Family != FamilyIPv4 || ens4.Addresses[0].Address != "192.168.1.10" ||
		ens4.Addresses[0].PrefixLen != 24 {
		t.Errorf("v4 地址读错：%+v", ens4.Addresses[0])
	}
	if ens4.Addresses[1].Family != FamilyIPv6 || ens4.Addresses[1].Scope != "link" {
		t.Errorf("v6 地址读错：%+v", ens4.Addresses[1])
	}
	if lo.Kind != KindLoopback || lo.Physical {
		t.Errorf("lo 应是回环且非物理，得到 kind=%s physical=%v", lo.Kind, lo.Physical)
	}
}

func TestInterfacesFallsBackToSysfsWhenIPMissing(t *testing.T) {
	f, r := simpleHost()
	r.fail("ip -j addr show", ErrProgramMissing)

	list := Interfaces(context.Background(), testDeps(f, r))

	if len(list.Interfaces) != 2 {
		t.Fatalf("ip 不在时仍应从 sysfs 列出 2 张网卡，得到 %d", len(list.Interfaces))
	}
	if len(list.Issues) != 1 || list.Issues[0].Source != "ip" || list.Issues[0].Code != IssueMissing {
		t.Fatalf("应有一条 ip/missing 记录，得到 %+v", list.Issues)
	}
	for _, iface := range list.Interfaces {
		if len(iface.Addresses) != 0 {
			t.Errorf("%s 不该有地址（ip 没跑起来）：%+v", iface.Name, iface.Addresses)
		}
	}
	// sysfs 的事实仍然要读出来：地址读不到不等于网卡信息全丢。
	if list.Interfaces[0].MTU != 1500 {
		t.Errorf("ens4 的 MTU 应从 sysfs 读出 1500，得到 %d", list.Interfaces[0].MTU)
	}
}

func TestInterfacesReportsUnparsableIPOutput(t *testing.T) {
	f, r := simpleHost()
	r.reply("ip -j addr show", "{not json")

	list := Interfaces(context.Background(), testDeps(f, r))
	if len(list.Issues) == 0 || list.Issues[0].Code != IssueUnparsable {
		t.Fatalf("JSON 坏掉应报 unparsable，得到 %+v", list.Issues)
	}
	if len(list.Interfaces) == 0 {
		t.Fatal("JSON 坏掉时仍应从 sysfs 列出网卡")
	}
}

func TestDetectKindUsesKernelFactsNotNames(t *testing.T) {
	f := newFakeFS()
	f.dir("/sys/class/net", "br0", "bond0", "wlan0", "eth0.10", "tun0", "weird")
	// 名字叫 br0 但没有任何网桥目录：不能靠名字猜。
	f.dir("/sys/class/net/br0")
	f.file("/sys/class/net/br0/type", "1\n")
	f.dir("/sys/class/net/bond0")
	f.dir("/sys/class/net/bond0/bonding")
	f.dir("/sys/class/net/wlan0")
	f.dir("/sys/class/net/wlan0/wireless")
	f.dir("/sys/class/net/eth0.10")
	f.file("/proc/net/vlan/eth0.10", "")
	f.dir("/sys/class/net/tun0")
	f.file("/sys/class/net/tun0/tun_flags", "0x1001\n")
	f.dir("/sys/class/net/weird")
	f.file("/sys/class/net/weird/type", "778\n")
	deps := testDeps(f, newFakeRunner())

	cases := map[string]string{
		"br0":     KindEthernet, // 名字像网桥，但没有 bridge 目录
		"bond0":   KindBond,
		"wlan0":   KindWifi,
		"eth0.10": KindVLAN,
		"tun0":    KindTun,
		"weird":   KindOther,
	}
	for name, want := range cases {
		got := detectKind(deps, name, path.Join("/sys/class/net", name))
		if got != want {
			t.Errorf("%s 的类型 = %s，期望 %s", name, got, want)
		}
	}
}

// ---- 能力判定 ----

func TestCapabilityEditableOnSinglePhysicalNetplanHost(t *testing.T) {
	f, r := simpleHost()
	list := Interfaces(context.Background(), testDeps(f, r))

	cap := list.Capability
	if cap.Backend != BackendNetplanNetworkd {
		t.Fatalf("后端应为 %s，得到 %s", BackendNetplanNetworkd, cap.Backend)
	}
	if cap.Target != "ens4" {
		t.Errorf("可改目标应是 ens4，得到 %q", cap.Target)
	}
	if !cap.Editable {
		t.Errorf("单物理网卡 + netplan + networkd 应可编辑，得到 reasons=%v", cap.Reasons)
	}
}

func TestCapabilityBlockedByNetworkManagerRunning(t *testing.T) {
	f, r := simpleHost()
	f.dir("/run/NetworkManager", "NetworkManager.pid")
	list := Interfaces(context.Background(), testDeps(f, r))

	if list.Capability.Backend != BackendNetplanNetworkd {
		t.Fatalf("后端仍应是 networkd，得到 %s", list.Capability.Backend)
	}
	if list.Capability.Editable {
		t.Fatal("NetworkManager 在跑时不该可编辑")
	}
	if !hasReason(list.Capability.Reasons, "NetworkManager") {
		t.Errorf("原因里应提到 NetworkManager，得到 %v", list.Capability.Reasons)
	}
}

func TestCapabilityBlockedByBridge(t *testing.T) {
	f, r := simpleHost()
	f.dir("/sys/class/net", "br0")
	f.dir("/sys/class/net/br0")
	f.dir("/sys/class/net/br0/bridge")
	f.file("/sys/class/net/br0/type", "1\n")
	// 网桥在 sysfs 里也一定出现在 `ip addr` 里：两个来源互相印证，能力判定只吃运行时清单。
	r.reply("ip -j addr show", ipShow(
		ipJSON(1, "lo", "UNKNOWN", "00:00:00:00:00:00", ""),
		ipJSON(2, "ens4", "UP", "52:54:00:aa:bb:cc", ""),
		ipJSON(3, "br0", "DOWN", "52:54:00:aa:bb:cc", ""),
	))
	list := Interfaces(context.Background(), testDeps(f, r))

	if list.Capability.Editable {
		t.Fatal("有网桥时不该可编辑（PLAN §19：桥接先显示不支持）")
	}
	if !hasReason(list.Capability.Reasons, "网桥") {
		t.Errorf("原因里应提到网桥，得到 %v", list.Capability.Reasons)
	}
}

func TestCapabilityBlockedByMultiplePhysicalNICs(t *testing.T) {
	f, r := simpleHost()
	f.dir("/sys/class/net", "ens5")
	f.dir("/sys/class/net/ens5")
	f.file("/sys/class/net/ens5/type", "1\n")
	f.link("/sys/class/net/ens5/device", "../../../0000:00:04.0")
	r.reply("ip -j addr show", ipShow(
		ipJSON(1, "lo", "UNKNOWN", "00:00:00:00:00:00", ""),
		ipJSON(2, "ens4", "UP", "52:54:00:aa:bb:cc", ""),
		ipJSON(3, "ens5", "DOWN", "52:54:00:aa:bb:dd", ""),
	))
	list := Interfaces(context.Background(), testDeps(f, r))

	if list.Capability.Editable {
		t.Fatal("两张物理网卡时不该可编辑（1.0 只支持单网卡）")
	}
	if !hasReason(list.Capability.Reasons, "多张物理网卡") {
		t.Errorf("原因里应说明是多网卡，得到 %v", list.Capability.Reasons)
	}
}

func TestCapabilityNetworkManagerWithoutNetplan(t *testing.T) {
	f := newFakeFS()
	f.dir("/sys/class/net", "lo", "ens4")
	f.dir("/sys/class/net/lo")
	f.file("/sys/class/net/lo/type", "772\n")
	f.dir("/sys/class/net/ens4")
	f.file("/sys/class/net/ens4/type", "1\n")
	f.link("/sys/class/net/ens4/device", "dev")
	f.dir("/etc/netplan")
	f.dir("/run/NetworkManager", "NetworkManager.pid")
	f.dir("/proc")
	r := newFakeRunner().reply("ip -j addr show", `[{"ifindex":2,"ifname":"ens4","operstate":"UP","address":"aa:bb:cc:dd:ee:ff"}]`)

	list := Interfaces(context.Background(), testDeps(f, r))
	if list.Capability.Backend != BackendNetworkManager {
		t.Fatalf("没有 netplan + NM 在跑 → backend 应为 networkmanager，得到 %s", list.Capability.Backend)
	}
	if list.Capability.Editable {
		t.Fatal("NetworkManager 管的机器不该可编辑")
	}
}

func TestCapabilityUnknownWhenNetplanUnreadable(t *testing.T) {
	f := newFakeFS()
	f.dir("/sys/class/net", "lo")
	f.dir("/sys/class/net/lo")
	f.file("/sys/class/net/lo/type", "772\n")
	f.dir("/proc")
	// /etc/netplan 完全不存在：不知道，不当作"没有配置"。
	r := newFakeRunner().reply("ip -j addr show", `[{"ifindex":1,"ifname":"lo","operstate":"UNKNOWN"}]`)

	list := Interfaces(context.Background(), testDeps(f, r))
	if list.Capability.Backend != BackendUnknown {
		t.Fatalf("读不到 /etc/netplan 时 backend 应为 unknown，得到 %s", list.Capability.Backend)
	}
	if !hasReason(list.Capability.Reasons, "读不到 /etc/netplan") {
		t.Errorf("原因应说明读不到 netplan 目录，得到 %v", list.Capability.Reasons)
	}
}

// ---- 路由 ----

func TestRoutesBothFamiliesAndDefaultGateways(t *testing.T) {
	_, r := simpleHost()
	list := Routes(context.Background(), testDeps(newFakeFS(), r))

	if len(list.Issues) != 0 {
		t.Fatalf("不该有 issue：%+v", list.Issues)
	}
	if len(list.Routes) != 4 {
		t.Fatalf("路由数 = %d，期望 4", len(list.Routes))
	}
	defaults := 0
	for _, route := range list.Routes {
		if route.Default {
			defaults++
			if route.Gateway == "" {
				t.Errorf("默认路由应有网关：%+v", route)
			}
		}
	}
	if defaults != 2 {
		t.Errorf("默认路由应有 2 条（v4 + v6），得到 %d", defaults)
	}
	if list.Routes[0].Destination != "0.0.0.0/0" || list.Routes[1].Destination != "::/0" {
		t.Errorf("默认路由应排在最前且 v4 在 v6 前：%s, %s",
			list.Routes[0].Destination, list.Routes[1].Destination)
	}
	if len(list.Gateways) != 2 {
		t.Fatalf("网关数 = %d，期望 2", len(list.Gateways))
	}
	if list.Gateways[0].Address != "192.168.1.1" || list.Gateways[1].Address != "fe80::1" {
		t.Errorf("网关读错：%+v", list.Gateways)
	}
	// metric 缺省不是 0：直连路由没有 metric 字段，PrefSet 必须是 false。
	for _, route := range list.Routes {
		if route.Destination == "192.168.1.0/24" && route.PrefSet {
			t.Errorf("没有 metric 字段的路由不该标成 PrefSet：%+v", route)
		}
	}
}

// 主机路由在 `ip -j route` 里不带前缀（`10.0.2.2`、`::1`），契约说 destination 一律写成
// 网段：补齐 /32 与 /128。现场证据来自 22.04 验收机（`10.0.2.2`、`10.0.2.3`、`::1` 三条
// 主机路由），不补的话同一个字段在页面上一会儿是 "10.0.2.2"、一会儿是 "10.0.2.0/24"。
func TestRoutesNormalizeHostRoutesToPrefix(t *testing.T) {
	_, r := simpleHost()
	r.reply("ip -j route show", `[{"dst":"default","gateway":"10.0.2.2","dev":"ens4","metric":100},
	  {"dst":"10.0.2.2","dev":"ens4","scope":"link"},
	  {"dst":"10.0.2.0/24","dev":"ens4","scope":"link"}]`)
	r.reply("ip -j -6 route show", `[{"dst":"::1","dev":"lo","scope":"host"}]`)

	list := Routes(context.Background(), testDeps(newFakeFS(), r))
	want := []string{"0.0.0.0/0", "10.0.2.0/24", "10.0.2.2/32", "::1/128"}
	got := make([]string, 0, len(list.Routes))
	for _, route := range list.Routes {
		got = append(got, route.Destination)
	}
	if len(got) != len(want) {
		t.Fatalf("路由数 = %d（%v），期望 %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("路由[%d] = %s，期望 %s（全量 %v）", i, got[i], want[i], got)
		}
	}
}

func TestRoutesReportsMissingIP(t *testing.T) {
	r := newFakeRunner().fail("ip -j route show", ErrProgramMissing)
	list := Routes(context.Background(), testDeps(newFakeFS(), r))
	if len(list.Issues) == 0 || list.Issues[0].Source != "ip" {
		t.Fatalf("应记录 ip 读不到，得到 %+v", list.Issues)
	}
	if len(list.Routes) != 0 {
		t.Fatalf("读不到就不该有路由，得到 %d 条", len(list.Routes))
	}
}

// ---- 监听端口 ----

func TestListenersParseTCPAndUDPMinusNoise(t *testing.T) {
	f := newFakeFS()
	// TCP：0100007F:0016 = 127.0.0.1:22、00000000:1F90 = 0.0.0.0:8080；ESTABLISHED 那行要被跳过。
	f.file("/proc/net/tcp", `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:0016 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 11111 1 0 100 0 0 10 0
   1: 00000000:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 22222 1 0 100 0 0 10 0
   2: 0100007F:1F90 0100007F:C350 01 00000000:00000000 00:00000000 00000000     0        0 33333 1 0 100 0 0 10 0
`)
	f.file("/proc/net/tcp6", `  sl  local_address rem_address   st
   0: 00000000000000000000000000000000:0016 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 44444 1 0 100 0 0 10 0
`)
	f.file("/proc/net/udp", `  sl  local_address rem_address   st
   0: 00000000:0035 00000000:0000 07 00000000:00000000 00:00000000 00000000     0        0 55555 1 0 100 0 0 10 0
`)
	f.file("/proc/net/udp6", `  sl  local_address rem_address   st
`)
	f.dir("/proc", "1")
	f.dir("/proc/1/fd", "3")
	f.link("/proc/1/fd/3", "socket:[22222]")
	f.file("/proc/1/comm", "nginx\n")
	f.file("/proc/1/cgroup", "0::/system.slice/nginx.service\n")

	list := Listeners(testDeps(f, newFakeRunner()))

	if len(list.Issues) != 0 {
		t.Fatalf("不该有 issue：%+v", list.Issues)
	}
	if len(list.Listeners) != 4 {
		t.Fatalf("监听数 = %d，期望 4：%+v", len(list.Listeners), list.Listeners)
	}
	byPort := map[string]Listener{}
	for _, l := range list.Listeners {
		byPort[string(l.Family)+"/"+itoa(l.Port)] = l
	}
	ssh, ok := byPort["ipv4/22"]
	if !ok {
		t.Fatal("应看到 127.0.0.1:22")
	}
	if ssh.Family != FamilyIPv4 || ssh.Protocol != "tcp" || ssh.Wildcard || !ssh.Loopback {
		t.Errorf("127.0.0.1:22 的标记不对：%+v", ssh)
	}
	if ssh.Address != "127.0.0.1" {
		t.Errorf("v4 地址应从小端 32 位还原成 127.0.0.1，得到 %q", ssh.Address)
	}
	if ssh.PID != 0 {
		t.Errorf("127.0.0.1:22 没有对应的 fd，PID 应为 0：%+v", ssh)
	}
	web, ok := byPort["ipv4/8080"]
	if !ok {
		t.Fatal("应看到 8080 端口")
	}
	if !web.Wildcard || web.Loopback {
		t.Errorf("0.0.0.0:8080 应是通配且非回环：%+v", web)
	}
	if web.PID != 1 || web.Process != "nginx" || web.Unit != "nginx.service" {
		t.Errorf("8080 的进程归属读错：%+v", web)
	}
	dns, ok := byPort["ipv4/53"]
	if !ok || dns.Protocol != "udp" {
		t.Fatalf("应看到 udp 53：%+v", dns)
	}
	// 同一个端口在不同协议栈上是两条：v6 那条 [::]:22 不能被 v4 的回环那条盖掉。
	v6, ok := byPort["ipv6/22"]
	if !ok {
		t.Fatal("应看到 [::]:22")
	}
	if v6.Family != FamilyIPv6 || !v6.Wildcard {
		t.Errorf("[::]:22 应判成 v6 通配：%+v", v6)
	}
	if len(list.Listeners) != 4 {
		t.Errorf("总条数 = %d，期望 4", len(list.Listeners))
	}
}

func TestListenersTruncatesAndSaysSo(t *testing.T) {
	f := newFakeFS()
	var builder strings.Builder
	builder.WriteString("  sl  local_address rem_address   st\n")
	for i := 0; i < maxListeners+5; i++ {
		builder.WriteString("   0: 00000000:" + hexPort(10000+i) + " 00000000:0000 0A 00000000:00000000 00:00000000 00000000 0 0 " +
			itoa(1000+i) + " 1 0 100 0 0 10 0\n")
	}
	f.file("/proc/net/tcp", builder.String())
	f.file("/proc/net/tcp6", "")
	f.file("/proc/net/udp", "")
	f.file("/proc/net/udp6", "")
	f.dir("/proc")

	list := Listeners(testDeps(f, newFakeRunner()))
	if !list.Truncated {
		t.Error("超过上限时 Truncated 必须为 true")
	}
	if len(list.Listeners) != maxListeners {
		t.Errorf("截断后应为 %d 条，得到 %d", maxListeners, len(list.Listeners))
	}
}

func TestListenersReportsUnreadableProcNet(t *testing.T) {
	f := newFakeFS()
	f.dir("/proc")
	list := Listeners(testDeps(f, newFakeRunner()))
	if len(list.Issues) != 4 {
		t.Fatalf("四个协议栈都读不到时应各记一条，得到 %d：%+v", len(list.Issues), list.Issues)
	}
	if list.Issues[0].Code != IssueMissing {
		t.Errorf("读不到的原因码应为 missing，得到 %s", list.Issues[0].Code)
	}
	if len(list.Listeners) != 0 {
		t.Errorf("读不到时不该有监听，得到 %d 条", len(list.Listeners))
	}
}

// ---- 防火墙 ----

const ufwSample = `Status: active
Logging: on (low)
Default: deny (incoming), allow (outgoing), disabled (routed)
New profiles: skip

To                         Action      From
--                         ------      ----
[ 1] 22/tcp                     ALLOW IN    Anywhere
[ 2] 443/tcp                    ALLOW IN    Anywhere
[ 3] 8080                       ALLOW IN    192.168.1.0/24
[ 4] 22/tcp (v6)                ALLOW IN    Anywhere (v6)
`

func TestFirewallParsesUFWNumberedVerbose(t *testing.T) {
	f := newFakeFS()
	f.dir("/usr/sbin", "ufw")
	f.dir("/etc/netplan")
	f.dir("/run")
	r := newFakeRunner().reply("ufw status verbose numbered", ufwSample)

	state := Firewall(context.Background(), testDeps(f, r))

	if state.Manager != ManagerUFW || !state.Installed {
		t.Fatalf("应判定为 ufw：manager=%s installed=%v", state.Manager, state.Installed)
	}
	if !state.Enabled {
		t.Error("Status: active 应读成 enabled")
	}
	if state.DefaultIncoming != "deny" || state.DefaultOutgoing != "allow" || state.DefaultRouted != "disabled" {
		t.Errorf("默认策略读错：%+v", state)
	}
	if state.Logging != "on (low)" {
		t.Errorf("日志级别读错：%q", state.Logging)
	}
	if len(state.Rules) != 4 {
		t.Fatalf("规则数 = %d，期望 4：%+v", len(state.Rules), state.Rules)
	}
	first := state.Rules[0]
	if first.Number != 1 || first.Action != "ALLOW" || first.Direction != "IN" || first.To != "22/tcp" || first.From != "Anywhere" {
		t.Errorf("第一条规则读错：%+v", first)
	}
	third := state.Rules[2]
	if third.To != "8080" || third.From != "192.168.1.0/24" {
		t.Errorf("带来源网段的规则读错：%+v", third)
	}
	if !state.Editable {
		t.Errorf("ufw 独占时应可编辑，reasons=%v", state.Reasons)
	}
}

func TestFirewallInactiveStillReadsDefaults(t *testing.T) {
	f := newFakeFS()
	f.dir("/usr/sbin", "ufw")
	f.dir("/run")
	r := newFakeRunner().reply("ufw status verbose numbered",
		"Status: inactive\n\nNOTE: ufw is not currently enabled.\n")
	state := Firewall(context.Background(), testDeps(f, r))
	if state.Enabled {
		t.Error("Status: inactive 不该读成启用")
	}
	if state.Manager != ManagerUFW || !state.Installed {
		t.Errorf("装了但没启用仍是 ufw：%+v", state)
	}
	if len(state.Rules) != 0 {
		t.Errorf("没启用时不该有规则，得到 %d", len(state.Rules))
	}
}

func TestFirewallMissingProgramIsNotNone(t *testing.T) {
	f := newFakeFS()
	f.dir("/usr/sbin")
	f.dir("/run")
	state := Firewall(context.Background(), testDeps(f, newFakeRunner()))

	if state.Installed {
		t.Error("没有 ufw 二进制时 Installed 必须是 false")
	}
	if state.Manager != ManagerNone {
		t.Errorf("没有 ufw 也没有别的管理器时 manager 应为 none，得到 %s", state.Manager)
	}
	if len(state.Issues) == 0 || state.Issues[0].Source != "ufw" || state.Issues[0].Code != IssueMissing {
		t.Errorf("应记一条 ufw/missing，说明是读不到而不是没有防火墙：%+v", state.Issues)
	}
	if state.Editable {
		t.Error("没有 ufw 时不可编辑")
	}
}

func TestFirewallDetectsForeignManagerAndRefusesWrite(t *testing.T) {
	f := newFakeFS()
	f.dir("/usr/sbin", "ufw")
	f.dir("/run/firewalld")
	state := Firewall(context.Background(), testDeps(f, newFakeRunner()))

	if state.Manager != ManagerUnknown {
		t.Fatalf("ufw 与 firewalld 同存时判不出谁在生效，manager 应为 unknown，得到 %s", state.Manager)
	}
	if state.Editable {
		t.Fatal("两套管理器同存时绝不能可编辑")
	}
	if !hasReason(state.ManagerReasons, ManagerFirewalld) {
		t.Errorf("判定依据里应提到 firewalld：%v", state.ManagerReasons)
	}
}

// 每一台 Ubuntu 都有 /etc/nftables.conf（nftables 包自带），而 nftables.service 默认
// disabled。只看文件在不在会把标准机器判成"ufw 与 nftables 同存"，P14.3 的写路径就永远
// 打不开。22.04 验收机的现场就是这个形状：ufw 独占、nftables 有一份默认规则文件。
func TestFirewallUbuntuDefaultNftablesConfIsNotAManager(t *testing.T) {
	f := newFakeFS()
	f.dir("/usr/sbin", "ufw")
	f.dir("/run")
	f.file("/etc/nftables.conf", "#!/usr/sbin/nft -f\nflush ruleset\n")
	f.dir("/etc/systemd/system", "multi-user.target.wants")
	f.dir("/etc/systemd/system/multi-user.target.wants") // 里面没有 nftables.service
	state := Firewall(context.Background(), testDeps(f, newFakeRunner()))

	if state.Manager != ManagerUFW {
		t.Fatalf("只有一份没人加载的 /etc/nftables.conf 时 manager 应是 ufw，得到 %s（reasons=%v）",
			state.Manager, state.ManagerReasons)
	}
	if !state.Editable {
		t.Errorf("ufw 独占时 P14.3 的写路径应当打开，得到 editable=false（reasons=%v）", state.Reasons)
	}
}

// enable 过的 nftables 才是"开机时会把规则装进内核"的那一个。
func TestFirewallNftablesEnabledCountsAsForeignManager(t *testing.T) {
	f := newFakeFS()
	f.dir("/usr/sbin", "ufw")
	f.dir("/run")
	f.file("/etc/nftables.conf", "#!/usr/sbin/nft -f\nflush ruleset\n")
	f.dir("/etc/systemd/system", "multi-user.target.wants")
	f.dir("/etc/systemd/system/multi-user.target.wants", "nftables.service")
	state := Firewall(context.Background(), testDeps(f, newFakeRunner()))

	if state.Manager != ManagerUnknown {
		t.Fatalf("ufw 与 enable 过的 nftables 同存时 manager 应为 unknown，得到 %s", state.Manager)
	}
	if state.Editable {
		t.Error("两套规则同存时绝不能可编辑")
	}
	if !hasReason(state.ManagerReasons, ManagerNftables) {
		t.Errorf("判定依据里应提到 nftables：%v", state.ManagerReasons)
	}
}

// P16.3：原子替换只保证"看不看得见"，不保证"掉电后还在不在"。rename 之后必须刷目录，
// 刷不下去就要报错——调用方据此在 apply 之前停手，而不是以为已经写稳了。
func TestWriteFileAtomicSyncsDirectoryAfterRename(t *testing.T) {
	fsys := newFakeFS().dir("/etc/netplan")
	engine := &ApplyEngine{fs: fsys}

	const target = "/etc/netplan/90-upanel.yaml"
	if err := engine.writeFileAtomic(target, []byte("network:\n"), 0o600); err != nil {
		t.Fatalf("写文件失败：%v", err)
	}
	if fsys.files[target] != "network:\n" {
		t.Fatalf("内容没换上去：%q", fsys.files[target])
	}
	if len(fsys.syncedDirs) != 1 || fsys.syncedDirs[0] != "/etc/netplan" {
		t.Fatalf("rename 之后应刷一次父目录，实际刷了 %v", fsys.syncedDirs)
	}

	fsys.syncDirErr = errors.New("input/output error")
	if err := engine.writeFileAtomic(target, []byte("network:\n  version: 2\n"), 0o600); err == nil {
		t.Fatal("目录刷不下去时必须报错，否则调用方会以为写稳了")
	} else if !strings.Contains(err.Error(), "/etc/netplan") {
		t.Errorf("错误里应带上刷不下去的目录，实际：%v", err)
	}
}

// ---- 小工具 ----

// ipShow 把若干个网卡元素拼成一份 `ip -j addr show` 的输出。
func ipShow(items ...string) string { return "[" + strings.Join(items, ",") + "]" }

// ipJSON 造一个网卡元素；addr 为空表示这张卡没有地址（`ip` 也会列出它，只是 addr_info 为空）。
func ipJSON(index int, name, state, mac, addr string) string {
	out := `{"ifindex":` + itoa(index) + `,"ifname":"` + name + `","mtu":1500,` +
		`"operstate":"` + state + `","address":"` + mac + `"`
	if addr != "" {
		out += `,"addr_info":[{"family":"inet","local":"` + addr + `","prefixlen":24,"scope":"global"}]`
	}
	return out + "}"
}

func hasReason(reasons []string, needle string) bool {
	for _, reason := range reasons {
		if strings.Contains(reason, needle) {
			return true
		}
	}
	return false
}

func hexPort(port int) string {
	const digits = "0123456789ABCDEF"
	return string([]byte{digits[(port>>12)&0xF], digits[(port>>8)&0xF], digits[(port>>4)&0xF], digits[port&0xF]})
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

var _ = errors.Is
