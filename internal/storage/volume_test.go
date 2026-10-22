package storage

// 卷模型与参数渲染的测试（P07.4）。
//
// 这里钉的是“哪些输入根本不成立”：卷名的字符集、显示名的长度、挂载选项的集合、
// 卷标识的形状。这些规则的价值在于它们让**后面的所有层都不必再猜**：
// 卷名合法 ⇒ 挂载点一定在 /srv/upanel/volumes 下且是规范路径 ⇒
// fstab 条目一定写得对 ⇒ systemd 单元名一定算得对。

import (
	"errors"
	"path"
	"strings"
	"testing"

	"upanel/internal/httpx"
)

// ---------- 卷名 ----------

func TestValidateSlugAccepts(t *testing.T) {
	for _, slug := range []string{"data", "a", "backup-2026", "ssd1", "x9-y8-z7", strings.Repeat("a", MaxSlugLen)} {
		if err := ValidateSlug(slug); err != nil {
			t.Errorf("%q 应当被接受：%v", slug, err)
		}
	}
}

func TestValidateSlugRejects(t *testing.T) {
	cases := []struct{ slug, want string }{
		{"", "不能为空"},
		{strings.Repeat("a", MaxSlugLen+1), "最长"},
		{"-data", "中划线开头或结尾"},
		{"data-", "中划线开头或结尾"},
		{"a--b", "连续的中划线"},
		{"Data", "小写字母"},
		{"data_1", "小写字母"},
		{"data.1", "小写字母"},
		{"数据", "小写字母"},
		{"data/../etc", "小写字母"},
		{"data\x00", "小写字母"},
	}
	for _, tc := range cases {
		err := ValidateSlug(tc.slug)
		if err == nil {
			t.Errorf("%q 被接受了", tc.slug)
			continue
		}
		mustContain(t, err.Error(), tc.want, "错误消息（"+tc.slug+"）")
	}
}

func TestMountPathOfIsAlwaysUnderVolumeRoot(t *testing.T) {
	for _, slug := range []string{"data", "a", "backup-2026"} {
		got := MountPathOf(slug)
		if !strings.HasPrefix(got, DefaultVolumeRoot+"/") {
			t.Errorf("%q 的挂载点 %q 不在受管目录下", slug, got)
		}
		if cleaned := path.Clean(got); cleaned != got {
			t.Errorf("%q 的挂载点 %q 不是规范路径（%q）", slug, got, cleaned)
		}
	}
	if got := MountPathOf("data"); got != "/srv/upanel/volumes/data" {
		t.Errorf("MountPathOf(data) = %q", got)
	}
}

func TestValidateDisplayName(t *testing.T) {
	if err := ValidateDisplayName(""); err != nil {
		t.Errorf("空显示名应当被接受：%v", err)
	}
	if err := ValidateDisplayName("备份盘 A"); err != nil {
		t.Errorf("中文显示名应当被接受：%v", err)
	}
	if err := ValidateDisplayName(strings.Repeat("名", MaxVolumeDisplayNameRunes)); err != nil {
		t.Errorf("%d 个字符应当被接受：%v", MaxVolumeDisplayNameRunes, err)
	}
	if err := ValidateDisplayName(strings.Repeat("名", MaxVolumeDisplayNameRunes+1)); err == nil {
		t.Error("超长显示名被接受了")
	}
	if err := ValidateDisplayName("a\nb"); err == nil {
		t.Error("带控制字符的显示名被接受了")
	}
}

// ---------- 挂载选项 ----------

func TestMountOptions(t *testing.T) {
	cases := []struct {
		name string
		cfg  VolumeConfig
		want []string
	}{
		{
			name: "默认（不自动挂载、可读写、nodev+nosuid）",
			cfg:  DefaultVolumeConfig(),
			want: []string{"rw", "nodev", "nosuid", "nofail", "x-systemd.device-timeout=30", "noauto"},
		},
		{
			name: "自动挂载 + 只读",
			cfg:  VolumeConfig{Automount: true, ReadOnly: true, Nodev: true, Nosuid: true},
			want: []string{"ro", "nodev", "nosuid", "nofail", "x-systemd.device-timeout=30"},
		},
		{
			name: "关掉 nodev/nosuid",
			cfg:  VolumeConfig{Automount: false, Nodev: false, Nosuid: false},
			want: []string{"rw", "nofail", "x-systemd.device-timeout=30", "noauto"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := MountOptions(tc.cfg)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("选项 = %v，想要 %v", got, tc.want)
			}
			// 渲染出来的每一个选项都必须在 fstab 的允许集合里，
			// 否则“写下去的条目”会被自己的校验拒绝。
			for _, opt := range got {
				if !validMountOption(opt) {
					t.Errorf("选项 %q 不在允许集合里", opt)
				}
			}
		})
	}
}

// TestMountOptionsAlwaysNofail 单独钉住 nofail：一块数据盘不在不该让机器进不了系统。
func TestMountOptionsAlwaysNofail(t *testing.T) {
	for _, automount := range []bool{true, false} {
		for _, ro := range []bool{true, false} {
			opts := strings.Join(MountOptions(VolumeConfig{Automount: automount, ReadOnly: ro}), ",")
			mustContain(t, opts, "nofail", "挂载选项")
			mustContain(t, opts, "x-systemd.device-timeout=30", "挂载选项")
			if automount && strings.Contains(opts, "noauto") {
				t.Errorf("automount=true 时不该出现 noauto：%s", opts)
			}
			if !automount && !strings.Contains(opts, "noauto") {
				t.Errorf("automount=false 时必须带 noauto：%s", opts)
			}
		}
	}
}

func TestFsckPass(t *testing.T) {
	if got := fsckPass(FilesystemExt4); got != 2 {
		t.Errorf("ext4 的 fsck pass = %d，想要 2", got)
	}
	if got := fsckPass(FilesystemXFS); got != 0 {
		t.Errorf("XFS 的 fsck pass = %d，想要 0", got)
	}
}

// TestRecordFstabEntry 校验记录渲染出来的条目正好是 fstab 该有的那一行。
func TestRecordFstabEntry(t *testing.T) {
	rec := testRecord("vol_0123456789abcdef0123456789abcdef", "data", "sdb1", testUUID)
	rec.Config = VolumeConfig{Automount: true, ReadOnly: false, Nodev: true, Nosuid: true}
	entry := rec.FstabEntry()

	if entry.UUID != testUUID {
		t.Errorf("UUID = %q", entry.UUID)
	}
	if entry.MountPoint != "/srv/upanel/volumes/data" {
		t.Errorf("挂载点 = %q", entry.MountPoint)
	}
	if entry.FSType != FilesystemExt4 || entry.Pass != 2 || entry.Dump != 0 {
		t.Errorf("条目 = %+v", entry)
	}
	if line := entry.Line(); line != "UUID="+testUUID+
		" /srv/upanel/volumes/data ext4 rw,nodev,nosuid,nofail,x-systemd.device-timeout=30 0 2" {
		t.Errorf("行 = %q", line)
	}
	// 自动挂载的卷：条目里不能有 noauto。
	if strings.Contains(entry.Line(), "noauto") {
		t.Error("automount 的卷不该带 noauto")
	}
	// 渲染出来的条目必须能过自己的条目校验（否则写盘时会被拒）。
	if err := checkFstabEntrySet([]FstabEntry{entry}); err != nil {
		t.Errorf("记录渲染的条目没通过校验：%v", err)
	}
}

func TestRecordFstabEntryXFS(t *testing.T) {
	rec := testRecord("vol_0123456789abcdef0123456789abcdef", "logs", "sdc1", testUUID2)
	rec.FilesystemType = FilesystemXFS
	entry := rec.FstabEntry()
	if entry.Pass != 0 {
		t.Errorf("XFS 的 fsck pass = %d，想要 0", entry.Pass)
	}
	if err := checkFstabEntrySet([]FstabEntry{entry}); err != nil {
		t.Errorf("XFS 条目没通过校验：%v", err)
	}
}

// ---------- 卷标识与版本 ----------

func TestNewVolumeID(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 32; i++ {
		id, err := NewVolumeID()
		if err != nil {
			t.Fatalf("生成失败: %v", err)
		}
		if !ValidVolumeID(id) {
			t.Fatalf("生成的卷标识形状不合法：%q", id)
		}
		if seen[id] {
			t.Fatalf("卷标识重复：%q", id)
		}
		seen[id] = true
	}
}

func TestValidVolumeID(t *testing.T) {
	ok := []string{
		"vol_0123456789abcdef0123456789abcdef",
		"vol_ffffffffffffffffffffffffffffffff",
	}
	for _, id := range ok {
		if !ValidVolumeID(id) {
			t.Errorf("%q 应当合法", id)
		}
	}
	bad := []string{
		"",
		"vol_",
		"vol_0123456789abcdef0123456789abcde",   // 少一位
		"vol_0123456789abcdef0123456789abcdef0", // 多一位
		"VOL_0123456789abcdef0123456789abcdef",  // 前缀大小写
		"vol_0123456789ABCDEF0123456789ABCDEF",  // 大写十六进制
		"vol_0123456789abcdef0123456789abcdeZ",
		"vol_../../etc/passwd",
		"xol_0123456789abcdef0123456789abcdef",
	}
	for _, id := range bad {
		if ValidVolumeID(id) {
			t.Errorf("%q 不该合法", id)
		}
	}
}

func TestNextVersion(t *testing.T) {
	cases := map[string]string{
		"":    "1",
		"0":   "1",
		"1":   "2",
		"41":  "42",
		" 7 ": "8",
		"x":   "1",
		"-1":  "1",
	}
	for in, want := range cases {
		if got := NextVersion(in); got != want {
			t.Errorf("NextVersion(%q) = %q，想要 %q", in, got, want)
		}
	}
}

func TestIsVolumeAction(t *testing.T) {
	for _, a := range []string{ActionVolumeImport, ActionVolumeMount, ActionVolumeUnmount,
		ActionVolumeConfigure, ActionVolumeForget} {
		if !IsVolumeAction(a) {
			t.Errorf("%q 应当被判为卷动作", a)
		}
	}
	for _, a := range []string{ActionPartitionTableInitialize, ActionFilesystemFormat, "", "storage.volume"} {
		if IsVolumeAction(a) {
			t.Errorf("%q 不该被判为卷动作", a)
		}
	}
}

// ---------- 排序 ----------

func TestSortVolumesAndRecords(t *testing.T) {
	vs := []Volume{
		{ID: "vol_b", MountPath: "/srv/upanel/volumes/zeta"},
		{ID: "vol_c", MountPath: "/srv/upanel/volumes/alpha"},
		{ID: "vol_a", MountPath: "/srv/upanel/volumes/alpha"},
	}
	sortVolumes(vs)
	got := []string{vs[0].ID, vs[1].ID, vs[2].ID}
	if strings.Join(got, ",") != "vol_a,vol_c,vol_b" {
		t.Errorf("排序结果 = %v", got)
	}

	rs := []VolumeRecord{{ID: "vol_b", Slug: "zeta"}, {ID: "vol_a", Slug: "alpha"}}
	sortRecords(rs)
	if rs[0].Slug != "alpha" {
		t.Errorf("记录排序结果 = %v", rs)
	}
}

// ---------- 默认配置 ----------

// TestDefaultVolumeConfigDoesNotChangeBootBehaviour 钉住“导入不改变开机行为”：
// 默认不自动挂载，这是 PLAN 的意图，也是导入不需要再认证的理由。
func TestDefaultVolumeConfigDoesNotChangeBootBehaviour(t *testing.T) {
	cfg := DefaultVolumeConfig()
	if cfg.Automount {
		t.Error("默认配置自动挂载了：导入会悄悄改开机行为")
	}
	if !cfg.Nodev || !cfg.Nosuid {
		t.Error("默认配置没有开 nodev/nosuid：数据盘上的设备节点与 setuid 就是提权的梯子")
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("默认配置通不过校验：%v", err)
	}
}

// TestVolumeConfigValidateNeverErrors 记录一个**已知的缺口**：
// 现在四项都是独立布尔，没有互斥组合，所以 Validate 恒为 nil。
// 这个测试的作用是在选项集合长大时提醒改这里，而不是留一个永远不执行的分支。
func TestVolumeConfigValidateNeverErrors(t *testing.T) {
	for _, cfg := range []VolumeConfig{
		{}, {Automount: true}, {ReadOnly: true, Nodev: true}, {Automount: true, ReadOnly: true, Nodev: true, Nosuid: true},
	} {
		if err := cfg.Validate(); err != nil {
			t.Errorf("%+v 通不过校验：%v", cfg, err)
		}
	}
}

// ---------- 依赖检查 ----------

// TestRequireVolumeDeps 校验“缺什么就拒什么”，而不是执行到一半才发现。
//
// 四样依赖各缺一次：命令执行器、拓扑扫描、登记表、超级块探针。
// 每一次都必须报 DEPENDENCY_MISSING，而不是跳过某一步继续往下走。
func TestRequireVolumeDeps(t *testing.T) {
	full := func() *writer {
		return newWriter(WriteOptions{
			Scanner:     scannerOnly{},
			Runner:      newFakeVolumeRunner(),
			Superblocks: newFakeSuperblocks(),
			Store:       newFakeVolumeStore(),
			Fstab:       FstabStore{Path: "/etc/fstab"},
		})
	}
	if err := full().requireVolumeDeps(); err != nil {
		t.Fatalf("依赖齐全却拒绝了：%v", err)
	}
	for name, breakIt := range map[string]func(*writer){
		"命令执行器": func(w *writer) { w.runner = nil },
		"拓扑扫描":  func(w *writer) { w.scanner = nil },
		"登记表":   func(w *writer) { w.store = nil },
		"超级块探针": func(w *writer) { w.superblocks = nil },
	} {
		w := full()
		breakIt(w)
		err := w.requireVolumeDeps()
		if err == nil {
			t.Errorf("缺%s却没有拒绝", name)
			continue
		}
		var httpErr *HTTPError
		if !errors.As(err, &httpErr) || httpErr.Code != httpx.CodeDependencyMissing {
			t.Errorf("缺%s 的错误码不对：%v", name, err)
		}
	}
}

// ---------- 观察状态的闭集 ----------

// TestVolumeStateValuesAreClosed 校验观察状态只用了 CONTRACTS 第 6 节允许的六个值。
// “挂载在别处”不是第七个状态，而是 observation 上的一个布尔——这一点被
// stateKey 显式表达出来（它只是给 StateVersion 用的区分键，不是新状态）。
func TestVolumeStateValuesAreClosed(t *testing.T) {
	allowed := map[string]bool{
		VolumeStateMounted: true, VolumeStateUnmounted: true, VolumeStateMissing: true,
		VolumeStateMismatch: true, VolumeStateError: true, VolumeStateUnknown: true,
	}
	for v := range allowed {
		if strings.Contains(v, ":") {
			t.Errorf("状态值 %q 里不该带修饰", v)
		}
	}

	o := VolumeObservation{State: VolumeStateMounted}
	if key := o.stateKey(); key != VolumeStateMounted {
		t.Errorf("stateKey = %q", key)
	}
	o.MountedElsewhere = true
	if key := o.stateKey(); key == o.State {
		t.Error("挂载在别处时 stateKey 必须与“挂在正确位置”区分开")
	} else if !strings.HasPrefix(key, VolumeStateMounted+":") {
		t.Errorf("stateKey = %q，必须以原状态为前缀", key)
	}
}

// TestVolumeStateVersionBindsEveryInput 校验 StateVersion 把拓扑、记录、fstab
// 与观察状态四样都绑进去了：任何一样变了，预检就必须失效。
func TestVolumeStateVersionBindsEveryInput(t *testing.T) {
	fst := FstabState{Hash: "sha256:aaaa"}
	rec := testRecord("vol_0123456789abcdef0123456789abcdef", "data", "sdb1", testUUID)
	base := volumeStateVersion(guardTopo("sha256:topo1", nil), rec, fst, VolumeStateUnmounted)

	if same := volumeStateVersion(guardTopo("sha256:topo1", nil), rec, fst, VolumeStateUnmounted); same != base {
		t.Fatal("同样的输入得到不同的 StateVersion")
	}
	if v := volumeStateVersion(guardTopo("sha256:topo2", nil), rec, fst, VolumeStateUnmounted); v == base {
		t.Error("拓扑版本变了，StateVersion 没变")
	}
	rec2 := rec
	rec2.Version = "2"
	if v := volumeStateVersion(guardTopo("sha256:topo1", nil), rec2, fst, VolumeStateUnmounted); v == base {
		t.Error("记录版本变了，StateVersion 没变")
	}
	if v := volumeStateVersion(guardTopo("sha256:topo1", nil), rec,
		FstabState{Hash: "sha256:bbbb"}, VolumeStateUnmounted); v == base {
		t.Error("fstab 变了，StateVersion 没变")
	}
	if v := volumeStateVersion(guardTopo("sha256:topo1", nil), rec, fst, VolumeStateMounted); v == base {
		t.Error("观察状态变了，StateVersion 没变")
	}
}
