package storage

// 设备能力投影的测试（P07.1）。
//
// CONTRACTS.md 第 6.1 节要求 capabilities 从 P07 起按实际情况填充，且**不得返回
// 伪 available**。所以这里主要钉三件事：
//   1. 状态与真实判决一致（能做 / 被什么挡住 / 类型上不适用）；
//   2. 没接线时保持空数组，而不是“全都能做”；
//   3. 一次扫描里同一台设备的探针只跑一遍，且缓存不会跨越拓扑版本。

import (
	"context"
	"strings"
	"testing"
	"time"
)

// capOf 取出某个动作的能力状态。
func capOf(t *testing.T, caps []Capability, action string) Capability {
	t.Helper()
	for _, c := range caps {
		if c.Action == action {
			return c
		}
	}
	t.Fatalf("能力列表里没有 %s：%+v", action, caps)
	return Capability{}
}

// TestDeviceCapabilitiesStates 校验四种设备状态下的能力投影。
func TestDeviceCapabilitiesStates(t *testing.T) {
	ctx := context.Background()
	env, _, _ := testEnv()

	t.Run("没有分区表的空白盘", func(t *testing.T) {
		topo := guardTopo("sha256:v1", []Device{blankDisk("sdb", 8<<30)})
		caps := DeviceCapabilities(ctx, topo, "sdb", env)
		if len(caps) != 4 {
			t.Fatalf("能力项数 = %d，应固定 4 项", len(caps))
		}
		// 顺序固定，界面才能稳定渲染。
		want := []string{ActionPartitionTableInitialize, ActionPartitionCreate,
			ActionPartitionDelete, ActionFilesystemFormat}
		for i, c := range caps {
			if c.Action != want[i] {
				t.Errorf("第 %d 项是 %s，期望 %s", i, c.Action, want[i])
			}
		}
		if got := capOf(t, caps, ActionPartitionTableInitialize); got.State != CapabilityAvailable {
			t.Errorf("初始化 = %s（%s），空白盘应可用", got.State, got.Reason)
		}
		// 还没分区表就得先初始化，所以“建分区”现在是 blocked 而不是 available。
		if got := capOf(t, caps, ActionPartitionCreate); got.State != CapabilityBlocked {
			t.Errorf("建分区 = %s，没有 GPT 时不能被宣传成可用", got.State)
		}
		if got := capOf(t, caps, ActionPartitionDelete); got.State != CapabilityUnsupported {
			t.Errorf("删除 = %s，整盘上没有“删除”这个动作", got.State)
		}
		if got := capOf(t, caps, ActionFilesystemFormat); got.State != CapabilityUnsupported {
			t.Errorf("格式化 = %s，本阶段不支持整盘 mkfs", got.State)
		}
	})

	t.Run("已初始化的 GPT 空白盘", func(t *testing.T) {
		topo := guardTopo("sha256:v1", []Device{gptDisk("sdb", 8<<30)})
		caps := DeviceCapabilities(ctx, topo, "sdb", env)
		if got := capOf(t, caps, ActionPartitionCreate); got.State != CapabilityAvailable {
			t.Errorf("建分区 = %s（%s），GPT 有空闲区应可用", got.State, got.Reason)
		}
		// 已经有分区表了，不能再初始化。
		if got := capOf(t, caps, ActionPartitionTableInitialize); got.State != CapabilityBlocked {
			t.Errorf("初始化 = %s，盘上已有 GPT 应 blocked", got.State)
		}
	})

	t.Run("GPT 里的空白分区", func(t *testing.T) {
		sdb := gptDisk("sdb", 8<<30)
		sdb1 := addPartition(&sdb, "sdb1", 1, mib, 2<<30)
		topo := guardTopo("sha256:v1", []Device{sdb, sdb1})
		caps := DeviceCapabilities(ctx, topo, "sdb1", env)
		if got := capOf(t, caps, ActionFilesystemFormat); got.State != CapabilityAvailable {
			t.Errorf("格式化 = %s（%s），空白分区应可用", got.State, got.Reason)
		}
		if got := capOf(t, caps, ActionPartitionDelete); got.State != CapabilityAvailable {
			t.Errorf("删除 = %s（%s），空白分区应可用", got.State, got.Reason)
		}
		if got := capOf(t, caps, ActionPartitionTableInitialize); got.State != CapabilityUnsupported {
			t.Errorf("初始化 = %s，分区上不能初始化分区表", got.State)
		}
	})

	t.Run("已挂载的分区", func(t *testing.T) {
		sdb := gptDisk("sdb", 8<<30)
		sdb1 := addPartition(&sdb, "sdb1", 1, mib, 2<<30)
		sdb1.Filesystem = &Filesystem{
			Type:   strPtr("ext4"),
			Mounts: []Mount{{Path: "/srv/data", Source: "/dev/sdb1", FSType: "ext4", Options: []string{"rw"}}},
		}
		topo := guardTopo("sha256:v1", []Device{sdb, sdb1})
		caps := DeviceCapabilities(ctx, topo, "sdb1", env)
		for _, action := range []string{ActionFilesystemFormat, ActionPartitionDelete} {
			got := capOf(t, caps, action)
			if got.State != CapabilityBlocked {
				t.Errorf("%s = %s，应被挂载挡住", action, got.State)
			}
			if !strings.Contains(got.Reason, "/srv/data") {
				t.Errorf("%s 的理由没说清是哪个挂载点：%q", action, got.Reason)
			}
		}
	})

	t.Run("受保护的整盘", func(t *testing.T) {
		sda := gptDisk("sda", 40<<30)
		sda.Protection = Protection{SystemDisk: true, Reasons: []string{"承载根文件系统 /"}, Sources: []string{"sda"}}
		topo := guardTopo("sha256:v1", []Device{sda})
		caps := DeviceCapabilities(ctx, topo, "sda", env)
		got := capOf(t, caps, ActionPartitionTableInitialize)
		if got.State != CapabilityBlocked {
			t.Errorf("系统盘初始化 = %s，应为 blocked 而不是 unsupported", got.State)
		}
		if !strings.Contains(got.Reason, "系统") {
			t.Errorf("理由应说明是系统数据：%q", got.Reason)
		}
	})

	t.Run("不存在的设备", func(t *testing.T) {
		topo := guardTopo("sha256:v1", []Device{blankDisk("sdb", 8<<30)})
		caps := DeviceCapabilities(ctx, topo, "sdz", env)
		for _, c := range caps {
			if c.State != CapabilityUnsupported {
				t.Errorf("%s = %s，看不到的设备应是 unsupported", c.Action, c.State)
			}
		}
	})

	// 没接探针时不能是 available：读不到签名 = 证明不了它是空的。
	t.Run("没有探针", func(t *testing.T) {
		topo := guardTopo("sha256:v1", []Device{blankDisk("sdb", 8<<30)})
		caps := DeviceCapabilities(ctx, topo, "sdb", Env{})
		if got := capOf(t, caps, ActionPartitionTableInitialize); got.State != CapabilityBlocked {
			t.Errorf("初始化 = %s，没有探针时应为 blocked（读不出签名）", got.State)
		}
	})
}

// TestDeviceCapabilitiesReasonTruncated 校验理由最多两条，其余的用计数带过：
// 界面上的 tooltip 不能变成一段日志。
func TestDeviceCapabilitiesReasonTruncated(t *testing.T) {
	env, sigs, busy := testEnv()
	sigs.kinds = map[string]string{"sdb": SignatureLVM2}
	busy.procs = map[string][]BusyProcess{"sdb": {{PID: "9", Comm: "dd"}}}
	dev := blankDisk("sdb", 8<<30)
	dev.ReadOnly = true // 第三条限制，用来触发截断
	topo := guardTopo("sha256:v1", []Device{dev})
	caps := DeviceCapabilities(context.Background(), topo, "sdb", env)
	got := capOf(t, caps, ActionPartitionTableInitialize)
	if got.State != CapabilityBlocked {
		t.Fatalf("初始化 = %s，应 blocked", got.State)
	}
	if strings.Count(got.Reason, "；") > 2 {
		t.Errorf("理由拼了太多条：%q", got.Reason)
	}
	if !strings.Contains(got.Reason, "另有") {
		t.Errorf("被截断时应有计数提示：%q", got.Reason)
	}
}

// TestJoinReasons 校验拼接与截断。
func TestJoinReasons(t *testing.T) {
	bs := []Blocker{{Code: "A", Message: "a"}, {Code: "B", Message: "b"}, {Code: "C", Message: "c"}}
	if got := joinReasons(bs, 2); got != "a；b；另有 1 项限制" {
		t.Errorf("joinReasons = %q", got)
	}
	if got := joinReasons(bs[:1], 2); got != "a" {
		t.Errorf("joinReasons 单项 = %q", got)
	}
	if got := joinReasons(nil, 2); got != "" {
		t.Errorf("joinReasons 空 = %q", got)
	}
}

// newCapabilityCollector 造一个只用来测能力填充的采集器。
func newCapabilityCollector(env Env) *Collector {
	return NewCollector(Options{Env: env, Interval: time.Hour})
}

// TestFillCapabilitiesWithoutEnvKeepsEmpty 校验没接线时能力保持空数组：
// 这是“代理不知道能做什么”，界面据此不显示破坏性按钮。
func TestFillCapabilitiesWithoutEnvKeepsEmpty(t *testing.T) {
	c := newCapabilityCollector(Env{})
	topo := guardTopo("sha256:v1", []Device{blankDisk("sdb", 8<<30)})
	now := time.Now()
	c.fillCapabilities(context.Background(), &topo, now)
	for _, dev := range topo.Nodes {
		if len(dev.Capabilities) != 0 {
			t.Errorf("%s 的能力不是空数组：%+v", dev.ID, dev.Capabilities)
		}
	}
	if len(c.deviceCaps) != 0 {
		t.Errorf("没接线却写了缓存：%+v", c.deviceCaps)
	}
}

// TestFillCapabilitiesMemoisesProbes 校验四动作共用一次探针：
// 每台设备只读一次签名、走一次 /proc，而不是 4 遍。
func TestFillCapabilitiesMemoisesProbes(t *testing.T) {
	env, sigs, busy := testEnv()
	c := newCapabilityCollector(env)
	topo := guardTopo("sha256:v1", []Device{blankDisk("sdb", 8<<30)})
	c.fillCapabilities(context.Background(), &topo, time.Now())

	if sigs.calls != 1 {
		t.Errorf("签名探测 %d 次，同一台设备应只读一次", sigs.calls)
	}
	if busy.calls != 1 {
		t.Errorf("占用扫描 %d 次，同一台设备应只扫一次", busy.calls)
	}
	if len(topo.Nodes[0].Capabilities) != 4 {
		t.Errorf("能力项 = %d，应为 4", len(topo.Nodes[0].Capabilities))
	}
}

// TestFillCapabilitiesCache 校验缓存：TTL 内不重算，TTL 外与版本变化都重算。
func TestFillCapabilitiesCache(t *testing.T) {
	ctx := context.Background()
	env, sigs, _ := testEnv()
	c := newCapabilityCollector(env)
	now := time.Now()

	topo := guardTopo("sha256:v1", []Device{blankDisk("sdb", 8<<30)})
	c.fillCapabilities(ctx, &topo, now)
	if sigs.calls != 1 {
		t.Fatalf("首次填充探测 %d 次", sigs.calls)
	}

	// TTL 内：直接用缓存。
	topo2 := guardTopo("sha256:v1", []Device{blankDisk("sdb", 8<<30)})
	c.fillCapabilities(ctx, &topo2, now.Add(10*time.Second))
	if sigs.calls != 1 {
		t.Errorf("TTL 内又探测了一次（%d）", sigs.calls)
	}
	if got := capOf(t, topo2.Nodes[0].Capabilities, ActionPartitionTableInitialize); got.State != CapabilityAvailable {
		t.Errorf("缓存的能力变了：%s", got.State)
	}

	// TTL 外：重算。
	topo3 := guardTopo("sha256:v1", []Device{blankDisk("sdb", 8<<30)})
	c.fillCapabilities(ctx, &topo3, now.Add(DefaultCapabilityTTL))
	if sigs.calls != 2 {
		t.Errorf("过了 TTL 没有重算（%d）", sigs.calls)
	}

	// 版本变了：必须立刻重算，不能拿旧拓扑的结论。
	topo4 := guardTopo("sha256:v2", []Device{blankDisk("sdb", 8<<30)})
	c.fillCapabilities(ctx, &topo4, now.Add(DefaultCapabilityTTL+time.Second))
	if sigs.calls != 3 {
		t.Errorf("拓扑版本变了却用了旧缓存（%d）", sigs.calls)
	}
}

// TestFillCapabilitiesCacheFollowsState 校验缓存过期后能力会跟着真实状态变：
// 盘上多了一份签名，能力必须从 available 变成 blocked。
func TestFillCapabilitiesCacheFollowsState(t *testing.T) {
	ctx := context.Background()
	env, sigs, _ := testEnv()
	c := newCapabilityCollector(env)
	now := time.Now()

	topo := guardTopo("sha256:v1", []Device{blankDisk("sdb", 8<<30)})
	c.fillCapabilities(ctx, &topo, now)
	if got := capOf(t, topo.Nodes[0].Capabilities, ActionPartitionTableInitialize); got.State != CapabilityAvailable {
		t.Fatalf("空白盘初始化 = %s", got.State)
	}

	// 有人往盘上写了 ext4，拓扑版本随之改变（真实扫描是内容哈希）。
	sigs.kinds = map[string]string{"sdb": SignatureExt}
	topo2 := guardTopo("sha256:v2", []Device{blankDisk("sdb", 8<<30)})
	c.fillCapabilities(ctx, &topo2, now.Add(time.Second))
	got := capOf(t, topo2.Nodes[0].Capabilities, ActionPartitionTableInitialize)
	if got.State != CapabilityBlocked {
		t.Errorf("盘上已有签名，初始化仍是 %s", got.State)
	}
	if !strings.Contains(got.Reason, "签名") && !strings.Contains(got.Reason, "ext4") {
		t.Errorf("理由没提到签名：%q", got.Reason)
	}
}

// TestPruneCapabilities 校验设备消失后缓存条目被清掉，不会无限增长也不会
// 在设备回来时给出过期结论。
func TestPruneCapabilities(t *testing.T) {
	ctx := context.Background()
	env, _, _ := testEnv()
	c := newCapabilityCollector(env)
	now := time.Now()

	both := guardTopo("sha256:v1", []Device{blankDisk("sdb", 8<<30), blankDisk("sdc", 4<<30)})
	c.fillCapabilities(ctx, &both, now)
	if len(c.deviceCaps) != 2 {
		t.Fatalf("缓存条目 = %d，应为 2", len(c.deviceCaps))
	}

	one := guardTopo("sha256:v1", []Device{blankDisk("sdb", 8<<30)})
	c.fillCapabilities(ctx, &one, now.Add(time.Second))
	if len(c.deviceCaps) != 1 {
		t.Fatalf("缓存条目 = %d，拔掉的盘应被清掉", len(c.deviceCaps))
	}
	if _, ok := c.deviceCaps["sdc"]; ok {
		t.Error("sdc 已经不在拓扑里，缓存还在")
	}
}

// TestCapabilityActionsAreRegisteredActions 校验投影的动作名与判决函数认识的
// 动作是同一组——否则这里会静默返回四个 unsupported，界面永远没有按钮。
func TestCapabilityActionsAreRegisteredActions(t *testing.T) {
	known := map[string]bool{
		ActionPartitionTableInitialize: true,
		ActionPartitionCreate:          true,
		ActionPartitionDelete:          true,
		ActionFilesystemFormat:         true,
	}
	if len(capabilityActions) != len(known) {
		t.Fatalf("能力动作 %d 个，判决动作 %d 个", len(capabilityActions), len(known))
	}
	for _, a := range capabilityActions {
		if !known[a] {
			t.Errorf("能力列表里的 %q 不是判决函数认识的动作", a)
		}
	}
}
