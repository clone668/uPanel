package agent

import (
	"context"
	"encoding/json"

	"upanel/internal/discovery"
	"upanel/internal/monitoring"
	"upanel/internal/nginx"
	"upanel/internal/processes"
	"upanel/internal/services"
	"upanel/internal/storage"
)

// ProductionDeps 是生产登记表的依赖。
//
// 登记表按“给什么才登记什么”的原则构造：没有对应实现就不登记那条能力，
// capabilities 因此只会报告真实可用的查询。反过来（先登记一个永远失败的查询、
// 再指望调用方不调用它）会让 capabilities 说谎，而界面上会显示一条点开就报错的入口。
type ProductionDeps struct {
	// Collector 是系统基础资源采集器（P04.1）。为 nil 时不登记 system.summary。
	Collector *monitoring.Collector
	// Storage 是块设备拓扑采集器（P04.2）。为 nil 时不登记 storage.topology。
	Storage *storage.Collector
	// Services 是服务单元采集器（P05.1）。为 nil 时不登记 services.list/services.unit，
	// 也不登记 service.control 动作——capabilities 因此不会报告一条没有实现的能力。
	Services *services.Collector
	// Processes 是进程采集器（P05.2）。为 nil 时不登记 processes.list，
	// 也不登记 process.signal 动作。
	Processes *processes.Collector
	// Logs 是日志读取端（P05.3）。为 nil 时不登记 logs.query，也不接受日志流许可。
	Logs LogsSource
	// Files 是文件读取端（P06.1）。为 nil 时不登记 files.roots/list/text，
	// 也不接受 files.download 流许可——没有根登记表时这三条查询只能是 404。
	Files FilesSource
	// FileWrites 是文件写端（P06.2）。为 nil 时不登记 file.* 写动作，
	// 也不提供 /internal/v1/uploads 路由。
	FileWrites FileWriterSource
	// Terminal 是受限终端的会话层（P06.3）。为 nil 时不登记 terminal.create 与
	// terminal.sessions，也不接受 terminal.io 流许可——本机没有可用的终端账号
	// （或不是 Linux）时，capabilities 就如实报告“没有终端”，
	// 而不是留下一个点了报错的入口。
	Terminal TerminalManager
	// Shares 是共享与受管理账号的模块（P08.1）。为 nil 时不登记 share.* 动作与查询：
	// 没有 Samba 判决与执行端的接线不该报告一个点开就报错的入口。
	Shares sharesSource
	// Containers 是容器读取与启停（P09.3）。为 nil 时不登记 containers.* 查询、
	// 不接受容器日志流许可，也不登记 container.control：本机没有 docker 时，
	// capabilities 就如实报告“没有容器能力”，而不是留一个点开就 503 的入口。
	Containers ContainersSource
	// Discovery 是应用发现与健康探测（P09.2）。为 nil 时不登记 apps.discovery /
	// apps.probe：没有 /proc 与容器读取的接线时，一个“空候选列表”会被读成
	// “这台机器上什么都没有”，那是假话。
	Discovery *discovery.Collector
	// Nginx 是 Nginx 现状采集器（P10.1）。为 nil 时不登记 nginx.state：
	// 读不到配置时，一个“没有 server_name”的空结果会被读成“这个域名没人用”，
	// 而那正是把面板写到别人站点上的前一步。
	Nginx *nginx.Collector
	// Proxies 是反向代理的写入路径（P10.2）。为 nil 时不登记 proxy.apply：
	// 没有 Nginx 现状来源或命令执行器时，“写配置并 reload”这件事根本做不到，
	// 报告一个一点就报错的入口比不报告更糟。
	Proxies proxySource
	// Certificates 是证书动作的实现（P10.3）。为 nil 时不登记 certificate.* 动作
	// 与 certificate.state：没有秘密存储读取端，手动导入装不上私钥；没有命令执行端，
	// 签发根本跑不起来——两种情况下“证书”都会是一个点了才报错的入口。
	Certificates certificateSource
	// Apt 是 APT 事实读取端（P11.1）。为 nil 时不登记 apt.packages 与
	// system.power_state：没有 dpkg 的机器上，一条“空包列表”会被读成
	// “什么包都没装”，一个“没有重启标志”会被读成“不用重启”——
	// 这两种假话都比一个诚实的 503 糟得多。
	Apt AptFacts
	// AptWrites 是 APT 写事务的实现端（P11.2）。为 nil 时不登记 package.refresh /
	// package.install / package.remove / package.upgrade：没有 apt-get 与锁探测的接线时，
	// 一个"点了才报错"的卸载按钮比没有按钮更危险（人会以为这次操作已经在排队）。
	AptWrites PackageWrites
	// Power 是系统电源的实现端（P11.3）。为 nil 时不登记 system.power：
	// 没有 systemctl、没有 boot_id 读取端的机器上，“重启”这个按钮只能做到
	// “点了以后面板说自己已经受理了”，而那正是 PLAN §16 禁止的表述。
	Power PowerSource
	// Backup 是备份仓库的实现端（P12.1）。为 nil 时不登记
	// backup.repository.initialize 与 backup.repository.state：没有 restic、
	// 没有秘密读取端、没有文件根登记表的机器上，“初始化仓库”只能做到
	// “在某个不知道是什么的目录里跑一条命令”，而备份建立在那之上的每一次
	// 快照与恢复都会跟着错。
	//
	// 注意它不控制 /internal/v1/backup/repositories：那条路由由
	// Options.BackupRepositories 决定，因为镜像的写入（Web 登记定义）与执行能力
	// 是两件事——一台机器可以先接受定义，等管理员装好 restic 再允许初始化。
	Backup backupSource
}

// newSystemSummaryQuery 登记 system.summary：无参数，返回采集器最近一次快照。
//
// 返回的是**快照**而不是现场采集：采集按 5 秒轮询在后台跑，查询只读缓存，
// 所以同一个 5 秒窗口内反复刷新页面拿到的是同一份数据与同一个采集时间，
// 不会因为刷新快慢得到互相矛盾的数。
func newSystemSummaryQuery(collector *monitoring.Collector) Query {
	return Query{
		Name: monitoring.QueryName,
		Decode: func(raw json.RawMessage) (any, error) {
			// 这条查询没有参数。空结构 + DisallowUnknownFields 让任何多余字段
			// （包括 {"command":"..."}）都被拒绝，而不是被静默忽略。
			var params struct{}
			if err := DecodeParams(raw, &params); err != nil {
				return nil, err
			}
			return params, nil
		},
		Run: func(ctx context.Context, _ any) (any, error) {
			return collector.Summary(), nil
		},
	}
}
