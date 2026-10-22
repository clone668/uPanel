package appruntime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"upanel/internal/apps"
	"upanel/internal/containers"
	"upanel/internal/services"
	"upanel/internal/storage"
)

// 测试用的固定标识。卷标识的形状必须和 CONTRACTS.md 一致（vol_ + 32 位十六进制），
// 应用标识同理——校验逻辑会拒掉不合形状的值，测试不该绕过它。
const (
	testVolumeID    = "vol_0123456789abcdef0123456789abcdef"
	testVolume2ID   = "vol_fedcba9876543210fedcba9876543210"
	testContainerID = "aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"
	testOtherCID    = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
)

// fakeServices 是可控的服务快照来源。
type fakeServices struct {
	snap services.Snapshot
	err  error
}

func (f fakeServices) Snapshot(context.Context) (services.Snapshot, error) {
	return f.snap, f.err
}

// fakeContainers 是可控的容器清单来源。
type fakeContainers struct {
	resp containers.ListResponse
	err  error
}

func (f fakeContainers) List(context.Context) (containers.ListResponse, error) {
	return f.resp, f.err
}

// okSnapshot 把一组单元包成一个采集成功的快照。
func okSnapshot(units ...services.Unit) services.Snapshot {
	return services.Snapshot{Status: services.StatusOK, Units: units}
}

// okContainers 把一组容器包成一个可用的清单。
func okContainers(list ...containers.Container) containers.ListResponse {
	return containers.ListResponse{Available: true, Containers: list}
}

// activeUnit 是一个处于 active 的普通单元（能力由 capabilityFor 的规则给出）。
func activeUnit(name string) services.Unit {
	return services.Unit{
		Unit:        name,
		LoadState:   services.LoadLoaded,
		ActiveState: services.ActiveActive,
		SubState:    "running",
		Enablement:  services.EnablementEnabled,
		Capabilities: []string{
			services.OpStop, services.OpRestart, services.OpDisable,
		},
	}
}

// systemdApp 造一条 systemd 应用元数据。
func systemdApp(unit string, deps ...string) apps.App {
	return apps.App{
		ID: "app_0123456789abcdef0123456789abcdef",
		Definition: apps.Definition{
			Name:               "示例服务",
			Type:               apps.TypeSystemd,
			Binding:            apps.Binding{Unit: unit},
			VolumeDependencies: deps,
		},
	}
}

// dockerApp 造一条 docker 应用元数据。
func dockerApp(id string, deps ...string) apps.App {
	return apps.App{
		ID: "app_fedcba9876543210fedcba9876543210",
		Definition: apps.Definition{
			Name:               "示例容器",
			Type:               apps.TypeDocker,
			Binding:            apps.Binding{ContainerID: id},
			VolumeDependencies: deps,
		},
	}
}

func TestLinkHasNoRuntime(t *testing.T) {
	facts := (&Resolver{}).Snapshot(context.Background())
	app := apps.App{Definition: apps.Definition{
		Type:               apps.TypeLink,
		PublicURL:          "https://example.com",
		VolumeDependencies: []string{testVolumeID},
	}}
	info := facts.App(app)
	if info.RuntimeState != apps.RuntimeNotApplicable {
		t.Fatalf("link 的运行时状态 = %q", info.RuntimeState)
	}
	if !info.Capabilities.Open {
		t.Fatal("有入口地址时 open 应当为真")
	}
	if info.Capabilities.Start || info.Capabilities.Stop || info.Capabilities.Restart || info.Capabilities.Logs {
		t.Fatalf("link 不该有任何生命周期能力: %+v", info.Capabilities)
	}
	// 没有运行时就没有依赖可守：link 带依赖卷也是 not_applicable，不是 unmanaged。
	if info.Protection != apps.ProtectionNotApplicable {
		t.Fatalf("link 的依赖保护 = %q", info.Protection)
	}
}

func TestSystemdRuntimeStates(t *testing.T) {
	cases := []struct {
		name string
		unit services.Unit
		want string
		note string // 期望 note 里出现的关键词；空表示不检查
	}{
		{
			name: "active 就是 running",
			unit: activeUnit("nginx.service"),
			want: apps.RuntimeRunning,
		},
		{
			name: "failed 不能被说成 stopped",
			unit: services.Unit{Unit: "x.service", LoadState: services.LoadLoaded,
				ActiveState: services.ActiveFailed, SubState: "exit-code"},
			want: apps.RuntimeFailed,
			note: "failed",
		},
		{
			name: "过渡态既不是 running 也不是 stopped",
			unit: services.Unit{Unit: "x.service", LoadState: services.LoadLoaded,
				ActiveState: services.ActiveActivating, SubState: "start"},
			want: apps.RuntimeUnknown,
			note: "过渡状态",
		},
		{
			name: "load_state=not-found 是看不到，不是已删除",
			unit: services.Unit{Unit: "x.service", LoadState: services.LoadNotFound,
				ActiveState: services.ActiveInactive},
			want: apps.RuntimeUnknown,
			note: "单元文件读不出来",
		},
		{
			name: "inactive 就是 stopped",
			unit: services.Unit{Unit: "x.service", LoadState: services.LoadLoaded,
				ActiveState: services.ActiveInactive, SubState: "dead"},
			want: apps.RuntimeStopped,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &Resolver{Services: fakeServices{snap: okSnapshot(tc.unit)}}
			info := r.Snapshot(context.Background()).App(systemdApp(tc.unit.Unit))
			if info.RuntimeState != tc.want {
				t.Fatalf("runtime_state = %q，期望 %q（note=%q）", info.RuntimeState, tc.want, info.RuntimeNote)
			}
			if tc.note != "" && !strings.Contains(info.RuntimeNote, tc.note) {
				t.Fatalf("note = %q，应包含 %q", info.RuntimeNote, tc.note)
			}
		})
	}
}

// TestSystemdCapabilitiesFollowUnit 确认能力来自单元自身，而不是类型。
func TestSystemdCapabilitiesFollowUnit(t *testing.T) {
	unit := activeUnit("nginx.service") // active：只有 stop/restart/disable
	r := &Resolver{Services: fakeServices{snap: okSnapshot(unit)}}
	info := r.Snapshot(context.Background()).App(systemdApp("nginx.service"))
	if info.Capabilities.Start {
		t.Fatal("active 的单元不该出现 start 能力")
	}
	if !info.Capabilities.Stop || !info.Capabilities.Restart {
		t.Fatalf("active 的单元应当能 stop/restart: %+v", info.Capabilities)
	}
	if !info.Capabilities.Logs {
		t.Fatal("能读到单元状态就能看日志")
	}

	// 受保护单元（面板自己）能力为空，但日志仍然可读——读日志不是写操作。
	protected := activeUnit("upanel-web.service")
	protected.Protection = services.Protection{Protected: true, Category: services.ProtectSelf,
		Reasons: []string{"面板自身"}}
	protected.Capabilities = []string{}
	r = &Resolver{Services: fakeServices{snap: okSnapshot(protected)}}
	info = r.Snapshot(context.Background()).App(systemdApp("upanel-web.service"))
	if info.Capabilities.Start || info.Capabilities.Stop || info.Capabilities.Restart {
		t.Fatalf("受保护单元不该有能力: %+v", info.Capabilities)
	}
	if !info.Capabilities.Logs {
		t.Fatal("受保护单元仍然可以看日志")
	}
}

// TestSystemdBindingMissingRequiresReconfirm 是“绑定对象不在本机”的判定。
func TestSystemdBindingMissingRequiresReconfirm(t *testing.T) {
	r := &Resolver{Services: fakeServices{snap: okSnapshot(activeUnit("other.service"))}}
	info := r.Snapshot(context.Background()).App(systemdApp("gone.service"))
	if info.RuntimeState != apps.RuntimeUnknown {
		t.Fatalf("runtime_state = %q", info.RuntimeState)
	}
	if !strings.Contains(info.RuntimeNote, "重新确认绑定") {
		t.Fatalf("note = %q，应提示重新确认绑定", info.RuntimeNote)
	}
	if info.Capabilities.Start || info.Capabilities.Logs {
		t.Fatalf("看不到的单元不该有能力: %+v", info.Capabilities)
	}
}

// TestSourceFailureIsUnknownNotStopped 确认“读不到”不会被写成“停了”。
func TestSourceFailureIsUnknownNotStopped(t *testing.T) {
	r := &Resolver{
		Services:   fakeServices{err: errors.New("连不上代理")},
		Containers: fakeContainers{err: errors.New("连不上代理")},
	}
	facts := r.Snapshot(context.Background())
	for _, app := range []apps.App{systemdApp("nginx.service"), dockerApp(testContainerID)} {
		info := facts.App(app)
		if info.RuntimeState != apps.RuntimeUnknown {
			t.Fatalf("%s 的运行状态 = %q，读不到时只能是 unknown", app.Type, info.RuntimeState)
		}
		if !strings.Contains(info.RuntimeNote, "没有读到") {
			t.Fatalf("note = %q，应说明这一轮没有读到", info.RuntimeNote)
		}
		if info.Capabilities.Start || info.Capabilities.Stop || info.Capabilities.Restart || info.Capabilities.Logs {
			t.Fatalf("读不到状态时不该提供能力: %+v", info.Capabilities)
		}
	}
}

// TestDockerRebuiltContainerRequiresReconfirm 是 P09 验收里“容器重建 ID 变化后
// 旧绑定要求重新确认”的判定：按 ID 匹配，不做名字匹配合并。
func TestDockerRebuiltContainerRequiresReconfirm(t *testing.T) {
	// 同名、同镜像但 ID 已经变了的新容器在列表里；旧 ID 不在。
	newContainer := containers.Container{ID: testOtherCID, Name: "web", State: containers.StateRunning}
	newContainer.Capabilities = newContainer.ControlCapabilities()
	r := &Resolver{Containers: fakeContainers{resp: okContainers(newContainer)}}
	info := r.Snapshot(context.Background()).App(dockerApp(testContainerID, testVolumeID))
	if info.RuntimeState != apps.RuntimeUnknown {
		t.Fatalf("runtime_state = %q", info.RuntimeState)
	}
	if !strings.Contains(info.RuntimeNote, "重新确认绑定") {
		t.Fatalf("note = %q，应提示重新确认绑定", info.RuntimeNote)
	}
	if info.Capabilities.Start || info.Capabilities.Logs {
		t.Fatalf("旧 ID 不该有任何能力: %+v", info.Capabilities)
	}
	if info.Protection != apps.ProtectionUnmanaged {
		t.Fatalf("容器状态读不到时依赖保护 = %q", info.Protection)
	}
}

// TestDockerProtectionFollowsRestartPolicy 是 managed 的唯一来源：
// 容器不会自己启动时，面板才是受管理启动路径上唯一的自动启动者。
func TestDockerProtectionFollowsRestartPolicy(t *testing.T) {
	mk := func(policy string) containers.Container {
		c := containers.Container{ID: testContainerID, Name: "web", State: containers.StateRunning,
			RestartPolicy: policy}
		c.Capabilities = c.ControlCapabilities()
		return c
	}
	cases := []struct {
		policy string
		want   string
		note   string
	}{
		{"no", apps.ProtectionManaged, "不会自己启动"},
		{"", apps.ProtectionManaged, "不会自己启动"},
		{"always", apps.ProtectionUnmanaged, "自行拉起"},
		{"unless-stopped", apps.ProtectionUnmanaged, "自行拉起"},
		{"on-failure", apps.ProtectionUnmanaged, "自行拉起"},
	}
	for _, tc := range cases {
		t.Run("restart="+tc.policy, func(t *testing.T) {
			r := &Resolver{Containers: fakeContainers{resp: okContainers(mk(tc.policy))}}
			info := r.Snapshot(context.Background()).App(dockerApp(testContainerID, testVolumeID))
			if info.Protection != tc.want {
				t.Fatalf("依赖保护 = %q，期望 %q（note=%q）", info.Protection, tc.want, info.ProtectionNote)
			}
			if !strings.Contains(info.ProtectionNote, tc.note) {
				t.Fatalf("note = %q，应包含 %q", info.ProtectionNote, tc.note)
			}
		})
	}

	// 没有依赖卷时这件事不适用：不能标成 managed 去显得“保护得很好”。
	r := &Resolver{Containers: fakeContainers{resp: okContainers(mk("no"))}}
	info := r.Snapshot(context.Background()).App(dockerApp(testContainerID))
	if info.Protection != apps.ProtectionNotApplicable {
		t.Fatalf("没有依赖卷时依赖保护 = %q", info.Protection)
	}
}

// TestSystemdProtectionIsAlwaysUnmanaged 确认面板不吹牛：
// P09.4 没有改写单元文件，systemd 应用的启动顺序仍然不经过面板。
func TestSystemdProtectionIsAlwaysUnmanaged(t *testing.T) {
	unit := activeUnit("nginx.service")
	r := &Resolver{Services: fakeServices{snap: okSnapshot(unit)}}
	info := r.Snapshot(context.Background()).App(systemdApp("nginx.service", testVolumeID))
	if info.Protection != apps.ProtectionUnmanaged {
		t.Fatalf("依赖保护 = %q，P09.4 下只能是 unmanaged", info.Protection)
	}
	if !strings.Contains(info.ProtectionNote, "enabled") {
		t.Fatalf("note 应说明单元处于 enabled：%q", info.ProtectionNote)
	}
}

// TestHighRiskContainerHasNoControl 确认高风险容器的三项能力为空，并且理由可见。
func TestHighRiskContainerHasNoControl(t *testing.T) {
	c := containers.Container{ID: testContainerID, Name: "web",
		State: containers.StateRunning, RestartPolicy: "no",
		Risk: containers.Risk{Level: containers.RiskHigh, Reasons: []containers.RiskReason{{
			Code: "privileged", Message: "容器以 privileged 运行",
		}}}}
	c.Capabilities = c.ControlCapabilities()
	r := &Resolver{Containers: fakeContainers{resp: okContainers(c)}}
	info := r.Snapshot(context.Background()).App(dockerApp(testContainerID))
	if info.Capabilities.Stop || info.Capabilities.Restart {
		t.Fatalf("高风险容器不该有能力: %+v", info.Capabilities)
	}
	if !strings.Contains(info.RuntimeNote, "高风险") {
		t.Fatalf("note 应说明高风险: %q", info.RuntimeNote)
	}
	// 看日志不是控制：高风险容器照样能读日志（/containers/:id/logs 也是 admin 只读）。
	if !info.Capabilities.Logs {
		t.Fatal("高风险容器仍然可以看日志")
	}
}

// TestNoFactsExplainsItself 确认演示模式的空事实带着理由。
func TestNoFactsExplainsItself(t *testing.T) {
	info := NoFacts("未配置代理连接").App(systemdApp("nginx.service"))
	if info.RuntimeState != apps.RuntimeUnknown {
		t.Fatalf("runtime_state = %q", info.RuntimeState)
	}
	if !strings.Contains(info.RuntimeNote, "未配置代理连接") {
		t.Fatalf("note = %q", info.RuntimeNote)
	}
}

// --- 门禁 ---

// fakeVolumes 是可控的受管理卷来源。
type fakeVolumes struct {
	list []storage.Volume
	err  error
}

func (f fakeVolumes) Volumes(context.Context) ([]storage.Volume, error) {
	return f.list, f.err
}

// volume 造一条卷记录。
func volume(id, state string) storage.Volume {
	return storage.Volume{
		ID:             id,
		DisplayName:    "数据盘",
		MountPath:      "/srv/upanel/volumes/data",
		FilesystemUUID: "1111-2222",
		ObservedState:  state,
	}
}

func TestGateBlocksStartWhenVolumeOffline(t *testing.T) {
	gate := &Gate{Volumes: fakeVolumes{list: []storage.Volume{
		volume(testVolumeID, storage.VolumeStateUnmounted),
	}}}
	app := systemdApp("nginx.service", testVolumeID)
	blockers, err := gate.volumeBlockers(context.Background(), app)
	if err != nil {
		t.Fatalf("门禁返回错误: %v", err)
	}
	if len(blockers) != 1 || blockers[0].Code != CodeVolumeOffline {
		t.Fatalf("阻塞项 = %+v", blockers)
	}
	if !strings.Contains(blockers[0].Message, "未挂载") {
		t.Fatalf("消息 = %q", blockers[0].Message)
	}
}

func TestGateVolumeVerdicts(t *testing.T) {
	cases := []struct {
		name string
		list []storage.Volume
		want string
	}{
		{"卷不在列表里", nil, CodeVolumeMissing},
		{"UUID 不符", []storage.Volume{volume(testVolumeID, storage.VolumeStateMismatch)}, CodeVolumeIdentityChanged},
		{"读不到超级块", []storage.Volume{volume(testVolumeID, storage.VolumeStateError)}, CodeVolumeOffline},
		{"未知状态", []storage.Volume{volume(testVolumeID, storage.VolumeStateUnknown)}, CodeVolumeOffline},
		{"挂好了", []storage.Volume{volume(testVolumeID, storage.VolumeStateMounted)}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gate := &Gate{Volumes: fakeVolumes{list: tc.list}}
			blockers, err := gate.volumeBlockers(context.Background(),
				dockerApp(testContainerID, testVolumeID))
			if err != nil {
				t.Fatalf("门禁返回错误: %v", err)
			}
			if tc.want == "" {
				if len(blockers) != 0 {
					t.Fatalf("挂好的卷不该拦: %+v", blockers)
				}
				return
			}
			if len(blockers) != 1 || blockers[0].Code != tc.want {
				t.Fatalf("阻塞项 = %+v，期望 %s", blockers, tc.want)
			}
		})
	}
}

// TestGateFailsClosedWithoutVolumeSource 确认“没检查”不会被说成“检查通过”。
func TestGateFailsClosedWithoutVolumeSource(t *testing.T) {
	gate := &Gate{}
	blockers, err := gate.volumeBlockers(context.Background(), systemdApp("nginx.service", testVolumeID))
	if err != nil {
		t.Fatalf("门禁返回错误: %v", err)
	}
	if len(blockers) != 1 || blockers[0].Code != CodeVolumeCheckUnavailable {
		t.Fatalf("阻塞项 = %+v", blockers)
	}

	gate = &Gate{Volumes: fakeVolumes{err: errors.New("连不上代理")}}
	blockers, err = gate.volumeBlockers(context.Background(), systemdApp("nginx.service", testVolumeID))
	if err != nil {
		t.Fatalf("门禁返回错误: %v", err)
	}
	if len(blockers) != 1 || blockers[0].Code != CodeVolumeCheckUnavailable {
		t.Fatalf("读不到卷状态时的阻塞项 = %+v", blockers)
	}
}

// TestGateIgnoresNonStartOperations 确认门禁只拦启动，不拦停机。
func TestGateIgnoresNonStartOperations(t *testing.T) {
	for _, op := range []string{"stop", "enable", "disable"} {
		typ, _, parsedOp, ok := controlTarget("service.control",
			[]byte(`{"unit":"nginx.service","operation":"`+op+`"}`))
		if !ok || typ != apps.TypeSystemd || parsedOp != op {
			t.Fatalf("解析 %s 失败: %v %v %v", op, typ, parsedOp, ok)
		}
	}
	// stop 不该产生阻塞：一个起在空目录上的应用必须能被停下来。
	gate := &Gate{Volumes: fakeVolumes{list: []storage.Volume{
		volume(testVolumeID, storage.VolumeStateUnmounted),
	}}}
	blockers, err := gate.Blockers(context.Background(), "service.control",
		[]byte(`{"unit":"nginx.service","operation":"stop"}`))
	if err != nil {
		t.Fatalf("门禁返回错误: %v", err)
	}
	if len(blockers) != 0 {
		t.Fatalf("stop 不该被拦: %+v", blockers)
	}
}

// TestControlTargetOnlyUnderstandsControlActions 确认门禁不碰别的动作。
func TestControlTargetOnlyUnderstandsControlActions(t *testing.T) {
	if _, _, _, ok := controlTarget("storage.filesystem.format",
		[]byte(`{"device":"/dev/sdb","operation":"start"}`)); ok {
		t.Fatal("格式化不是控制动作")
	}
	if _, _, _, ok := controlTarget("service.control", []byte(`{"operation":"start"}`)); ok {
		t.Fatal("缺少 unit 时不该解析成功")
	}
	if _, _, _, ok := controlTarget("container.control", []byte(`{"container_id":"web","operation":"start"}`)); ok {
		t.Fatal("容器名不是完整 ID，不该解析成功")
	}
	typ, binding, op, ok := controlTarget("container.control",
		[]byte(`{"container_id":"`+testContainerID+`","operation":"restart"}`))
	if !ok || typ != apps.TypeDocker || binding.ContainerID != testContainerID || op != "restart" {
		t.Fatalf("解析容器控制失败: %v %v %v %v", typ, binding, op, ok)
	}
}
