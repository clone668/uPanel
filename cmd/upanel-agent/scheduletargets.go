package main

// 计划目标的现场解析（P12.4 起，P12.5 补上证书）。
//
// 计划定义里只存 target_id（`pol_…` / `scr_…`），不存版本、也不存名字。这是故意的：
// 一条一年前建的计划不能拿着一年前的版本去提交，否则每次触发都撞版本冲突；
// 名字与版本只能问"此刻的代理库"。
//
// 这里回答三件事：这个对象还在不在、它此刻的版本是多少、它现在能不能被执行。
// 判断"能不能"只用了库里的事实——仓库初始化没初始化要真去拨一次 restic 才知道，
// 那是执行时 backup.run 自己的预检该说的话，不该在这一层猜。
//
// 证书续期是个例外，它的目标事实不在任何一张表里，而在**盘上**：`/etc/letsencrypt/live/<domain>`
// 在不在。代理库没有 certificates 表（证书账本在面板库），所以这里既不能查库也不能猜，
// 只能看盘——而这正是判"这条续期计划还有没有意义"最硬的那个事实：证书被删了、
// 或者当初是手动导入的（certbot 根本不管它），计划指向的东西就不存在，宁可给出
// 一句"盘上没有"让人看见，也不要让它每晚安静地失败一次。

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"upanel/internal/agentstore"
	"upanel/internal/certificates"
	"upanel/internal/schedules"
)

// certificateLiveDir 是判"这张证书还在不在盘上"时看的目录。
//
// 与 certificates.Ops 的默认 LiveDir 是同一个常量：配置层没有暴露 live 目录
// （覆盖它没有任何真实用途，只会让账本与盘对不上），所以这里直接取默认值。
var certificateLiveDir = certificates.DefaultLiveDir

// certificateLiveStat 单独拎出来是为了能在单测里替掉：判读的是盘，测试不该
// 依赖宿主机真的有 /etc/letsencrypt。
var certificateLiveStat = os.Stat

// scheduleTargetLookup 把计划目标接到代理库的两个仓储上，并把证书接到盘上。
func scheduleTargetLookup(
	policies *agentstore.BackupPolicyStore,
	scripts *agentstore.ScriptStore,
) schedules.TargetLookup {
	return func(ctx context.Context, def schedules.Definition) (schedules.Target, bool, error) {
		switch def.Kind {
		case schedules.KindBackup:
			if policies == nil {
				return schedules.Target{}, false, nil
			}
			rec, found, err := policies.GetPolicy(ctx, def.TargetID)
			if err != nil {
				return schedules.Target{}, false, err
			}
			if !found {
				return schedules.Target{}, false, nil
			}
			return schedules.Target{
				Name:    rec.Name,
				Version: rec.Version,
				Ready:   true,
			}, true, nil

		case schedules.KindUserScript:
			if scripts == nil {
				return schedules.Target{}, false, nil
			}
			rec, found, err := scripts.Get(ctx, def.TargetID)
			if err != nil {
				return schedules.Target{}, false, err
			}
			if !found {
				return schedules.Target{}, false, nil
			}
			// 落盘文件与库里指纹对不上时不在这一层判"不能跑"：那件事由脚本模块在
			// 运行前自己核（它会拒绝，并且原因写进任务错误）。在这里提前说"不 ready"
			// 会让一条只是需要重新 apply 的脚本连带把计划也判死。
			return schedules.Target{
				Name:    rec.Name,
				Version: rec.Version,
				Ready:   true,
			}, true, nil

		case schedules.KindCertificateRenewal:
			return certificateRenewalTarget(def)

		default:
			// 未识别的种类就当"找不到"，由模块给 SCHEDULE_KIND_UNSUPPORTED。
			return schedules.Target{}, false, nil
		}
	}
}

// certificateRenewalTarget 用盘上的 live 目录回答"这张证书还在不在"。
//
// 域名来自定义的种类参数（Certificate），不是 target_id：证书账本在面板库，
// 代理侧没有 crt_ 到域名的映射，也就没法从 id 反查。定义里必须自带域名，
// 这正是 CertificateParams 存在的理由。
func certificateRenewalTarget(def schedules.Definition) (schedules.Target, bool, error) {
	if def.Certificate == nil || def.Certificate.Domain == "" {
		// Validate 已经拦过这种定义；到这里只可能是库里躺着一条老数据。
		// 没有域名就没法判盘，报"找不到"，理由由模块统一给。
		return schedules.Target{}, false, nil
	}
	domain := def.Certificate.Domain
	dir := filepath.Join(certificateLiveDir, domain)

	info, err := certificateLiveStat(dir)
	switch {
	case err == nil && !info.IsDir():
		// 同名文件占着 live 目录的位置：certbot 写不进去，续期一定失败。
		return schedules.Target{
			Name:    domain,
			Version: 1,
			Note:    dir + " 不是目录：certbot 无法把证书写在这里，先处理掉它再启用这条计划",
		}, true, nil
	case err == nil:
		return schedules.Target{Name: domain, Version: 1, Ready: true}, true, nil
	case errors.Is(err, fs.ErrNotExist):
		return schedules.Target{
			Name:    domain,
			Version: 1,
			Note: "盘上没有 " + dir + "：这张证书不是 certbot 签的（手动导入的证书得手动换），" +
				"或者已经被删掉了；指向它的续期计划只会每晚失败一次",
		}, true, nil
	default:
		// 权限不足、IO 错误之类：这不是"没有证书"，是"读不出来"。把错误交上去，
		// 让调用方看到真实原因，而不是被伪装成一句"证书不在"。
		return schedules.Target{}, false, err
	}
}
