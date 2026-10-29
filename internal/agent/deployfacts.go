package agent

// 部署动作在代理侧需要的宿主事实与秘密读取端（P15.2）。
//
// deploy.Service 不知道代理长什么样：它要的是“文件根在哪、卷此刻挂着没有、本机有哪些
// 地址、这个 secret_ref 解出来是什么”。这份文件把代理手里的表翻译成那份输入。
//
// 翻译规则本身（谁包含这个路径、什么算挂着、什么算同一块盘）留在 deploy 包里，
// 这里只做取值。调用点自己写一遍前缀比较，就会出现“登记说卷 A、门禁查卷 B”
// 这种只有到现场才会发现的错——而那正是 PLAN §11 规则 2 要堵的洞。

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path"
	"strings"

	"upanel/internal/deploy"
	"upanel/internal/files"
	"upanel/internal/network"
	"upanel/internal/storage"
)

// DeployRootSource 是部署需要的根登记表：只要一份清单。
//
// 比 FilesSource 窄，是因为部署从不读文件内容——它只问“这个根在哪、可不可写”。
// 取整个文件接口会让“能部署”与“能读文件”绑成一件事，而它们是两回事。
type DeployRootSource interface {
	Roots(ctx context.Context) files.RootsResult
}

// DeployVolumeSource 是部署需要的卷登记表：只要“列出全部卷”。
//
// 与 VolumeStore 分开声明：部署不改卷、不查单条，只按 UUID 与挂载点核对依赖。
// 取整个 VolumeStore 等于让部署动作也拿到了“保存卷记录”的能力。
type DeployVolumeSource interface {
	ListVolumes(ctx context.Context) ([]storage.VolumeRecord, error)
}

// DeployFacts 是 deployment.apply / deployment.remove 需要的宿主事实来源。
//
// 每个字段都是一条接缝，缺了会缺得很明确：没有登记表的机器上卷表为空（用到卷的
// 定义会被拒），没有拓扑读取端时“卷挂着没有”答不出来——那时**返回错误**，
// 不返回“都没挂”：一次读不到挂载表，不该让所有部署显示成“卷没挂上”。
type DeployFacts struct {
	// Roots 是文件根登记表。nil 表示这台机器没有登记过任何根。
	Roots DeployRootSource
	// Volumes 是受管理卷的持久登记表。nil 表示这台机器不做卷管理。
	Volumes DeployVolumeSource
	// Topology 返回最近一次块设备拓扑（挂载点与文件系统 UUID 都在里面）。
	//
	// 它是函数而不是快照：预检与执行前复核是两个时刻，中间卷可能刚被卸下，
	// 而“卷还在不在”正是这两次都要重新问一遍的东西。
	//
	// 它读的是采集器的 5 秒缓存（storage.Collector.Topology），不是现场重扫：
	// 部署前的那道真门是单元里的 BindsTo/RequiresMountsFor，由内核在启动那一刻执行，
	// 这里只负责在预检时把“现在看着没挂”说出来。
	Topology func(ctx context.Context) storage.Topology
	// Addresses 返回本机此刻配着的地址，用于判断 host_bind_address 是不是本机的。
	Addresses func(ctx context.Context) []net.IP
	// DataBase 是部署数据根：容器的挂载来源必须落在它里面。
	DataBase string
	// AllowWildcard 对应“允许绑 0.0.0.0/::”。默认 false：面板装完的部署只该绑本机。
	AllowWildcard bool
}

// HostEnv 取一份此刻的宿主事实。
func (f DeployFacts) HostEnv(ctx context.Context) (deploy.HostEnv, error) {
	env := deploy.HostEnv{
		DataBase:      strings.TrimSpace(f.DataBase),
		AllowWildcard: f.AllowWildcard,
	}
	volumes, err := f.volumeFacts(ctx)
	if err != nil {
		return deploy.HostEnv{}, err
	}
	env.Volumes = volumes
	if f.Roots != nil {
		env.Roots = f.rootFacts(ctx, volumes)
	}
	if f.Addresses != nil {
		env.LocalAddresses = f.Addresses(ctx)
	}
	return env, nil
}

// volumeFacts 读受管理卷，并给每条记录补上“此刻挂着没有、挂的是哪块文件系统”。
func (f DeployFacts) volumeFacts(ctx context.Context) (map[string]deploy.VolumeFacts, error) {
	if f.Volumes == nil {
		return nil, nil
	}
	records, err := f.Volumes.ListVolumes(ctx)
	if err != nil {
		return nil, fmt.Errorf("读受管理卷登记表失败: %w", err)
	}
	if len(records) == 0 {
		// 没有登记过卷，就没有“它挂着没有”这个问题要回答，因此也不强求拓扑来源。
		return nil, nil
	}
	if f.Topology == nil {
		return nil, errors.New("受管理卷已登记，但这台机器没有挂载状态来源；" +
			"判断不了卷是否挂着，因此不能部署")
	}
	topology := f.Topology(ctx)
	if topology.Status != storage.StatusOK {
		reason := strings.TrimSpace(topology.Error)
		if reason == "" {
			reason = "采集器没有给出原因"
		}
		return nil, fmt.Errorf("读挂载状态失败（%s）；"+
			"卷挂着没有判断不出来，就不在该状态上部署", reason)
	}
	out := make(map[string]deploy.VolumeFacts, len(records))
	for _, rec := range records {
		facts := deploy.VolumeFacts{
			MountPoint:     rec.MountPath(),
			FilesystemUUID: rec.FilesystemUUID,
		}
		facts.Mounted, facts.MountedUUID = mountedAt(topology, facts.MountPoint)
		out[rec.ID] = facts
	}
	return out, nil
}

// rootFacts 读文件根，并给每个根标上它落在哪个受管理卷里。
//
// 不在任何受管理卷里的根也照实登记（VolumeID 留空）：那样 deploy 的校验会给出
// “这个根不受依赖卷门禁保护”，而不是在这里悄悄把它藏起来——藏起来的结果是
// 界面上那个根凭空消失，人会以为是自己删错了。
func (f DeployFacts) rootFacts(ctx context.Context, volumes map[string]deploy.VolumeFacts) map[string]deploy.RootFacts {
	result := f.Roots.Roots(ctx)
	out := make(map[string]deploy.RootFacts, len(result.Roots))
	for _, root := range result.Roots {
		out[root.RootID] = deploy.RootFacts{
			Path:     root.Path,
			Writable: root.Writable,
			VolumeID: deploy.VolumeOfPath(root.Path, volumes),
		}
	}
	return out
}

// mountedAt 回答“这个挂载点上此刻挂的是什么”。
//
// 第二个返回值是挂在那里的文件系统的 UUID，读不到时为空——空与“不一样”在
// deploy.CheckVolumesReady 里是同一件事：都证明不了“还是原来那块盘”。
// 网络挂载也查：NFS/CIFS 挂在卷的挂载点上同样是“挂着别的东西”，
// 报成“没挂”会让人去查线，而问题其实在 fstab 或手工 mount 上。
func mountedAt(topology storage.Topology, mountPoint string) (bool, string) {
	want := path.Clean(strings.TrimSpace(mountPoint))
	if want == "" || want == "." {
		return false, ""
	}
	for _, device := range topology.Nodes {
		if device.Filesystem == nil {
			continue
		}
		for _, mount := range device.Filesystem.Mounts {
			if path.Clean(mount.Path) != want {
				continue
			}
			uuid := ""
			if device.Filesystem.UUID != nil {
				uuid = strings.TrimSpace(*device.Filesystem.UUID)
			}
			return true, uuid
		}
	}
	for _, mount := range topology.NetworkMounts {
		if path.Clean(mount.Path) == want {
			return true, ""
		}
	}
	return false, ""
}

// InterfaceAddresses 把网络采集器给出的网卡清单压成本机地址表。
//
// 部署的 host_bind_address 只能是本机此刻真的配着的地址（或者回环、或者在明确允许
// 的情况下是通配）。用采集器的结果而不是自己再读一遍网卡：面板上显示的地址与
// “能不能绑这个地址”必须是同一份数据，否则人看到 192.168.1.10 在那儿，部署却报
// “本机没有这个地址”。
func InterfaceAddresses(list network.InterfaceList) []net.IP {
	out := make([]net.IP, 0, len(list.Interfaces))
	for _, iface := range list.Interfaces {
		for _, addr := range iface.Addresses {
			if ip := net.ParseIP(strings.TrimSpace(addr.Address)); ip != nil {
				out = append(out, ip)
			}
		}
	}
	return out
}

// InterfaceAddressesFunc 把采集器包装成 DeployFacts.Addresses 需要的形状。
//
// 每次调用都重新问一次采集器：地址会在 P14.2 改网卡之后变，缓存下来的地址表
// 会让“刚改完的地址”在部署预检里不存在。
func InterfaceAddressesFunc(src NetworkSource) func(ctx context.Context) []net.IP {
	return func(ctx context.Context) []net.IP {
		if src == nil {
			return nil
		}
		return InterfaceAddresses(src.Interfaces(ctx))
	}
}

// DeploySecrets 是部署配置里 secret_ref 需要的秘密能力。//
// 只有核对与解密两个方法：写入不经这条路（秘密由 Web 经 POST /internal/v1/secrets
// 上传），销毁也不经（到期清理是代理自己的循环）。把写入或删除挂上来，等于给
// “渲染一份配置”这件事开了改秘密的口子。
type DeploySecrets interface {
	Binding(ctx context.Context, ref string) (purpose, reference string, ok bool, err error)
	Resolve(ctx context.Context, ref string) ([]byte, error)
}

// DeploySecretResolver 把密钥环适配成 deploy.Resolver。
//
// 用途必须由**存储里记着的那一条**说了算：deploy 传进来的是"我要 app_env"，
// 如果引用实际登记的是 TLS 私钥或备份口令，这里必须拒绝——否则一条部署定义
// 只要把 secret_ref 指向别人的私钥，就能把它塞进容器环境变量里再读出来。
type DeploySecretResolver struct {
	Secrets DeploySecrets
}

// Resolve 核对用途后解密一份秘密。
func (r DeploySecretResolver) Resolve(ctx context.Context, ref, purpose string) ([]byte, error) {
	ref = strings.TrimSpace(ref)
	purpose = strings.TrimSpace(purpose)
	if r.Secrets == nil {
		return nil, errors.New("这台机器没有可用的秘密存储，引用解不开")
	}
	if ref == "" {
		return nil, errors.New("秘密引用为空")
	}
	if purpose == "" {
		return nil, errors.New("没有给出用途，拒绝解密（用途是唯一的核对依据）")
	}
	stored, _, ok, err := r.Secrets.Binding(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("核对秘密 %s 的用途失败: %w", ref, err)
	}
	if !ok {
		return nil, fmt.Errorf("秘密 %s 不存在（已销毁、从未写入，或引用拼错了）", ref)
	}
	if strings.TrimSpace(stored) != purpose {
		return nil, fmt.Errorf("秘密 %s 的用途是 %s，不能当作 %s 使用", ref, stored, purpose)
	}
	plain, err := r.Secrets.Resolve(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("解密秘密 %s 失败: %w", ref, err)
	}
	return plain, nil
}
