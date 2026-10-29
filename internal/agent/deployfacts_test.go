package agent

// 部署的宿主事实适配（P15.2）。
//
// internal/deploy 的测试负责“什么样的定义被拒绝、单元长什么样、落地按什么顺序”；
// 这里只验代理有没有把表翻译错，四件事：
//
//  1. 文件根落在哪个卷上是**最长挂载点优先**，不在任何卷里就留空（不藏起来）；
//  2. 卷“挂着没有、挂的是哪块文件系统”来自拓扑，网络挂载占着挂载点也算“挂上了别的”；
//  3. 读不到拓扑时返回**错误**，不是一份“都没挂”的卷表——那会把一次采集故障
//     显示成“所有卷都没挂”，人会去查盘；
//  4. secret_ref 的用途必须与请求一致，不然一条部署定义就能把别人的私钥塞进容器。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"upanel/internal/deploy"
	"upanel/internal/files"
	"upanel/internal/network"
	"upanel/internal/storage"
)

// fakeDeployRoots 是一份固定的文件根清单。
type fakeDeployRoots struct {
	result files.RootsResult
}

func (f fakeDeployRoots) Roots(context.Context) files.RootsResult { return f.result }

// fakeDeployVolumes 是一份固定的卷登记表。
type fakeDeployVolumes struct {
	records []storage.VolumeRecord
	err     error
}

func (f fakeDeployVolumes) ListVolumes(context.Context) ([]storage.VolumeRecord, error) {
	return f.records, f.err
}

// mountedDevice 造一个“挂在这个挂载点上的设备节点”。
func mountedDevice(mountPoint, uuid string) storage.Device {
	fsUUID := uuid
	return storage.Device{
		ID:   "sdb1",
		Kind: storage.KindPartition,
		Filesystem: &storage.Filesystem{
			UUID:   &fsUUID,
			Mounts: []storage.Mount{{Path: mountPoint, Source: "/dev/sdb1"}},
		},
	}
}

// deployTestVolume 造一条卷记录。
func deployTestVolume(id, slug, uuid string) storage.VolumeRecord {
	return storage.VolumeRecord{
		ID:             id,
		Slug:           slug,
		FilesystemUUID: uuid,
		FilesystemType: "ext4",
		DisplayName:    slug,
	}
}

// TestDeployFactsRootMapsToVolume：文件根的路径被翻译成“它落在哪个受管理卷上”。
//
// 这条翻译是部署门禁的起点：卷依赖与挂载来源必须指向同一个卷，否则卷没挂上时
// 容器照样会被拉起，往系统盘上的空目录里写数据。
func TestDeployFactsRootMapsToVolume(t *testing.T) {
	facts := DeployFacts{
		Volumes: fakeDeployVolumes{records: []storage.VolumeRecord{
			deployTestVolume("vol_media", "media", "uuid-media"),
		}},
		Topology: func(context.Context) storage.Topology {
			return storage.Topology{
				Status: storage.StatusOK,
				Nodes:  []storage.Device{mountedDevice(storage.DefaultVolumeRoot+"/media", "uuid-media")},
			}
		},
		Roots: fakeDeployRoots{result: files.RootsResult{Roots: []files.RootInfo{
			{RootID: "root_media", Path: storage.DefaultVolumeRoot + "/media", Writable: true, Available: true},
			{RootID: "root_media_app", Path: storage.DefaultVolumeRoot + "/media/app", Writable: true, Available: true},
		}}},
	}
	env, err := facts.HostEnv(context.Background())
	if err != nil {
		t.Fatalf("取宿主事实失败：%v", err)
	}
	for _, id := range []string{"root_media", "root_media_app"} {
		root, ok := env.Roots[id]
		if !ok {
			t.Fatalf("根 %s 没有出现在宿主事实里", id)
		}
		if root.VolumeID != "vol_media" {
			t.Fatalf("根 %s 应归 vol_media，实际 %q", id, root.VolumeID)
		}
	}
	if got := env.Volumes["vol_media"]; !got.Mounted || got.MountedUUID != "uuid-media" {
		t.Fatalf("卷事实 = %+v，期望已挂载且 UUID 一致", got)
	}
	if got := env.Volumes["vol_media"].MountPoint; got != storage.DefaultVolumeRoot+"/media" {
		t.Fatalf("挂载点 = %q", got)
	}
}

// TestDeployFactsRootOutsideVolumesStaysEmpty：不在任何卷里的根照实登记，VolumeID 留空。
//
// 藏起来的结果是界面上那个根凭空消失；留空则 deploy 的校验会说清“它不受依赖卷门禁保护”。
func TestDeployFactsRootOutsideVolumes(t *testing.T) {
	facts := DeployFacts{
		Volumes: fakeDeployVolumes{records: []storage.VolumeRecord{
			deployTestVolume("vol_media", "media", "uuid-media"),
		}},
		Topology: func(context.Context) storage.Topology {
			return storage.Topology{Status: storage.StatusOK,
				Nodes: []storage.Device{mountedDevice(storage.DefaultVolumeRoot+"/media", "uuid-media")}}
		},
		Roots: fakeDeployRoots{result: files.RootsResult{Roots: []files.RootInfo{
			{RootID: "root_home", Path: "/home/upanel", Writable: true, Available: true},
		}}},
	}
	env, err := facts.HostEnv(context.Background())
	if err != nil {
		t.Fatalf("取宿主事实失败：%v", err)
	}
	if got := env.Roots["root_home"].VolumeID; got != "" {
		t.Fatalf("不在卷里的根不该带卷标识，实际 %q", got)
	}
	if len(env.Roots) != 1 {
		t.Fatalf("根被藏起来了：%v", env.Roots)
	}
}

// TestDeployFactsUnmountedAndForeignVolume：没挂 / 挂了别的盘 / 网络挂载占位，三种都不同。
func TestDeployFactsUnmountedAndForeignVolume(t *testing.T) {
	topology := storage.Topology{
		Status: storage.StatusOK,
		Nodes:  []storage.Device{mountedDevice(storage.DefaultVolumeRoot+"/media", "uuid-other")},
		NetworkMounts: []storage.NetworkMount{
			{Path: storage.DefaultVolumeRoot + "/share", Source: "nas:/export"},
		},
	}
	facts := DeployFacts{
		Volumes: fakeDeployVolumes{records: []storage.VolumeRecord{
			deployTestVolume("vol_media", "media", "uuid-media"),
			deployTestVolume("vol_share", "share", "uuid-share"),
			deployTestVolume("vol_idle", "idle", "uuid-idle"),
		}},
		Topology: func(context.Context) storage.Topology { return topology },
	}
	env, err := facts.HostEnv(context.Background())
	if err != nil {
		t.Fatalf("取宿主事实失败：%v", err)
	}
	media := env.Volumes["vol_media"]
	if !media.Mounted || media.MountedUUID != "uuid-other" {
		t.Fatalf("挂着别的盘时应报 Mounted=true 且 UUID 是别人的，实际 %+v", media)
	}
	share := env.Volumes["vol_share"]
	if !share.Mounted || share.MountedUUID != "" {
		t.Fatalf("网络挂载占着挂载点时应报已挂载、UUID 读不到，实际 %+v", share)
	}
	idle := env.Volumes["vol_idle"]
	if idle.Mounted {
		t.Fatalf("没挂的卷不该报已挂载：%+v", idle)
	}
}

// TestDeployFactsTopologyUnreadableIsError：读不到挂载表是错误，不是“都没挂”。
func TestDeployFactsTopologyUnreadableIsError(t *testing.T) {
	facts := DeployFacts{
		Volumes: fakeDeployVolumes{records: []storage.VolumeRecord{
			deployTestVolume("vol_media", "media", "uuid-media"),
		}},
		Topology: func(context.Context) storage.Topology {
			return storage.Topology{Status: storage.StatusError, Error: "读不到 /proc/self/mountinfo"}
		},
	}
	if _, err := facts.HostEnv(context.Background()); err == nil {
		t.Fatal("读不到拓扑时必须报错，不能给出一份“都没挂”的卷表")
	} else if !strings.Contains(err.Error(), "mountinfo") {
		t.Fatalf("错误里应带上原因，实际 %v", err)
	}
}

// TestDeployFactsWithoutVolumesNeedsNoTopology：没有登记过卷时不强求拓扑来源。
func TestDeployFactsWithoutVolumesNeedsNoTopology(t *testing.T) {
	facts := DeployFacts{Volumes: fakeDeployVolumes{}}
	env, err := facts.HostEnv(context.Background())
	if err != nil {
		t.Fatalf("没有卷不该报错：%v", err)
	}
	if len(env.Volumes) != 0 {
		t.Fatalf("卷表应为空：%v", env.Volumes)
	}
}

// fakeDeploySecrets 是秘密存储的假实现。
type fakeDeploySecrets struct {
	purpose string
	found   bool
	bindErr error
	plain   []byte
	resErr  error
	resolve int
}

func (f *fakeDeploySecrets) Binding(context.Context, string) (string, string, bool, error) {
	return f.purpose, "ref", f.found, f.bindErr
}

func (f *fakeDeploySecrets) Resolve(context.Context, string) ([]byte, error) {
	f.resolve++
	return f.plain, f.resErr
}

// TestDeploySecretResolverChecksPurpose：用途不符就不解密。
//
// 这一条是“定义不能读别人的秘密”的唯一依靠：代理的秘密表里躺着 TLS 私钥、备份口令、
// TOTP 种子，它们都由同一个 Resolve 解密——用途核对漏一次，一条部署定义就能把
// 别人的私钥塞进容器环境变量再读出来。
func TestDeploySecretResolverChecksPurpose(t *testing.T) {
	src := &fakeDeploySecrets{purpose: "tls_private_key", found: true, plain: []byte("PRIVATE")}
	resolver := DeploySecretResolver{Secrets: src}
	_, err := resolver.Resolve(context.Background(), "sec_abc", deploy.SecretPurposeAppEnv)
	if err == nil {
		t.Fatal("用途不符时必须拒绝")
	}
	if src.resolve != 0 {
		t.Fatal("用途不符时不该解密")
	}
	if !strings.Contains(err.Error(), "tls_private_key") {
		t.Fatalf("错误里应说清用途，实际 %v", err)
	}

	src.purpose = deploy.SecretPurposeAppEnv
	plain, err := resolver.Resolve(context.Background(), "sec_abc", deploy.SecretPurposeAppEnv)
	if err != nil {
		t.Fatalf("用途一致时应解密：%v", err)
	}
	if string(plain) != "PRIVATE" {
		t.Fatalf("明文 = %q", plain)
	}
}

// TestDeploySecretResolverMissing：不存在的引用要说“不存在”，不是一句解密失败。
func TestDeploySecretResolverMissing(t *testing.T) {
	resolver := DeploySecretResolver{Secrets: &fakeDeploySecrets{found: false}}
	_, err := resolver.Resolve(context.Background(), "sec_gone", deploy.SecretPurposeAppEnv)
	if err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("期望“不存在”，实际 %v", err)
	}
	if _, err := resolver.Resolve(context.Background(), "  ", deploy.SecretPurposeAppEnv); err == nil {
		t.Fatal("空引用必须拒绝")
	}
	var nilResolver DeploySecretResolver
	if _, err := nilResolver.Resolve(context.Background(), "sec_abc", deploy.SecretPurposeAppEnv); err == nil {
		t.Fatal("没有秘密存储时必须拒绝，而不是返回空明文")
	}
}

// TestInterfaceAddressesCollectsEveryFamily：网卡清单里的地址都进表，解析不了的跳过。
func TestInterfaceAddressesCollectsEveryFamily(t *testing.T) {
	list := network.InterfaceList{Interfaces: []network.Interface{
		{Name: "lo", Addresses: []network.Address{{Address: "127.0.0.1", PrefixLen: 8}}},
		{Name: "ens3", Addresses: []network.Address{
			{Address: "192.168.70.21", PrefixLen: 24},
			{Address: "fe80::1", PrefixLen: 64},
			{Address: "not-an-ip"},
		}},
	}}
	got := InterfaceAddresses(list)
	if len(got) != 3 {
		t.Fatalf("地址数 = %d（%v）", len(got), got)
	}
	found := false
	for _, ip := range got {
		if ip.String() == "192.168.70.21" {
			found = true
		}
	}
	if !found {
		t.Fatalf("IPv4 没进表：%v", got)
	}
	if addresses := InterfaceAddressesFunc(nil)(context.Background()); addresses != nil {
		t.Fatalf("没有采集器时不该编造地址：%v", addresses)
	}
}

// TestDeployFactsVolumeListError：读不到卷登记表同样报错，不是“没有卷”。
func TestDeployFactsVolumeListError(t *testing.T) {
	facts := DeployFacts{Volumes: fakeDeployVolumes{err: errors.New("database is locked")}}
	if _, err := facts.HostEnv(context.Background()); err == nil {
		t.Fatal("读不到卷表时必须报错")
	}
}
