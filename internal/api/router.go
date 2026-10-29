package api

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"upanel/internal/audit"
	"upanel/internal/httpx"
)

// 上下文键。只在包内使用，避免与业务键冲突。
const (
	ctxKeyRequestID          = "upanel.request_id"
	ctxKeyRequestIDInherited = "upanel.request_id_inherited"
	// ctxKeyAuditAction 是本路由组的审计动作名。中间件拒绝请求时用它记录“被拒绝”的事件，
	// 因为那时还没有进入 handler，拿不到更精确的动作。
	ctxKeyAuditAction = "upanel.audit_action"
)

// routes 组装路由与中间件。
//
// 路由组织：公共 API 统一挂在 /api/v1 下，按领域分组（health、auth、system、storage……）。
// 本阶段只实现 health 分组；其余分组在对应阶段加入，不在路由表里预埋返回 501 的占位路由。
//
// 未命中的路径分两类处理（P01.4）：
//   - /api 与 /api/... 一律 JSON 404，**绝不**回落到静态页面或 200；
//   - 其余路径交给前端静态处理器（SPA 回退）。没有前端处理器时同样 JSON 404。
func (s *Server) routes() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)

	r := gin.New()
	// 不自动重定向：/api/v1/health/live/ 与 /api/v1/health/live 是不同路径，
	// 重定向会让客户端把 301 当成成功。
	r.RedirectTrailingSlash = false
	r.RedirectFixedPath = false
	r.HandleMethodNotAllowed = true

	r.Use(
		requestIDMiddleware(),
		accessLogMiddleware(s.opts.Logger),
		recoveryMiddleware(s.opts.Logger),
		bodyLimitMiddleware(MaxRequestBodyBytes),
		// HSTS 只在真开了 TLS 时才发：明文部署（SSH 隧道）收到这个头会让浏览器
		// 之后再也连不上 http://127.0.0.1:<port>。
		s.hstsMiddleware(),
	)

	v1 := r.Group("/api/v1")
	v1.Use(s.originGuard())
	{
		health := v1.Group("/health")
		health.GET("/live", s.handleHealthLive)
		health.HEAD("/live", s.handleHealthLive)

		authGroup := v1.Group("/auth")
		authGroup.POST("/login", s.handleLogin)
		authGroup.GET("/session", s.requireSession(), s.handleSession)
		authGroup.POST("/logout", s.requireSession(), s.requireCSRF(), s.handleLogout)
		authGroup.POST("/reauth", s.auditRoute(audit.ActionAuthReauth),
			s.requireSession(), s.requireAdmin(), s.requireCSRF(), s.handleReauth)

		// 二次验证（P14.6）：CONTRACTS.md 第 2 节的 /auth/totp/*。
		//
		// 整组 admin（viewer 没有意义：二次验证保护的是管理权限），并且每一个写入口
		// 都要 admin + 再认证 + CSRF——"给你的账号挂一把新锁"和"把锁拆了"都是危险操作。
		// 组级动作名是 auth.totp.manage：被中间件拒绝时只能记到这一层。
		totpGroup := v1.Group("/auth/totp", s.auditRoute(audit.ActionTotpManage))
		totpGroup.Use(s.requireSession(), s.requireAdmin())
		totpGroup.POST("/setup", s.requireCSRF(), s.requireReauth(), s.handleTotpSetup)
		totpGroup.POST("/confirm", s.requireCSRF(), s.requireReauth(), s.handleTotpConfirm)
		totpGroup.DELETE("", s.requireCSRF(), s.requireReauth(), s.handleTotpDisable)

		// 账号管理（P02.2）：整组 admin；改动类操作额外要求 5 分钟内的再认证。
		users := v1.Group("/users", s.auditRoute(audit.ActionUserManage))
		users.Use(s.requireSession(), s.requireAdmin())
		users.GET("", s.handleListUsers)
		users.POST("", s.requireCSRF(), s.handleCreateUser)
		users.PATCH("/:id", s.requireCSRF(), s.requireReauth(), s.handleUpdateUser)
		users.POST("/:id/password", s.requireCSRF(), s.requireReauth(), s.handleSetUserPassword)
		users.DELETE("/:id", s.requireCSRF(), s.requireReauth(), s.handleDeleteUser)

		// 系统只读信息（P04.1）：权限 V，任何已登录用户都能看系统概况。
		// 采集与缓存都在代理侧，这里只透传最近一次快照，不叠加第二层缓存。
		system := v1.Group("/system")
		system.Use(s.requireSession())
		system.GET("/summary", s.handleSystemSummary)
		// 历史监控（P13.1）：权限 V。查询只读面板自己的样本库，不经过代理；
		// 状态接口回答“采集循环在不在跑、最后一条数据是什么时候”，
		// 与查询分开是因为它的行数统计是全表 COUNT(*)。
		system.GET("/metrics", s.handleMetrics)
		system.GET("/metrics/status", s.handleMetricsStatus)
		// 重启标志（P11.1）：admin。它是一句"这台机器需要重启"，属于主机运维信息；
		// 数据来自包自己写的文件，不是从"装了内核"推出来的。
		system.GET("/power-state", s.requireAdmin(), s.handlePowerState)
		// 外部程序能力（P16.1）：admin。这份清单列的是这台机器装了哪些外部程序、
		// 什么版本、在哪——合起来就是主机指纹；viewer 用不到，泄露出去只对
		// 打算找漏洞的人有用。与容器清单、部署定义同一个判断。
		system.GET("/capabilities", s.requireAdmin(), s.handleSystemCapabilities)

		// 网络与防火墙现状（P14.1）：整组 admin（CONTRACTS.md 第 14.1 节）。
		//
		// 为什么不是 V：网卡、地址、路由、监听端口与防火墙规则合起来就是"这台机器上
		// 开了什么入口、通向哪里"，与容器清单同级的侦察材料。viewer 不需要知道
		// 面板所在机器的内网拓扑。
		//
		// 这一组**一个字节都不改**：改地址是 network.apply（P14.2）、改规则是
		// firewall.apply（P14.3），都走预检 → 任务。因此这里只有 GET，没有
		// "直接 netplan apply"或"直接 ufw allow"的入口——那会绕过确认、回滚与审计。
		//
		// 两个组分开登记，因为审计动作不同（中间件拒绝发生在 handler 之前，
		// 只有组级别能带上正确的动作名）。
		netGroup := v1.Group("/network", s.auditRoute(audit.ActionNetworkManage))
		netGroup.Use(s.requireSession(), s.requireAdmin())
		netGroup.GET("/interfaces", s.handleListNetworkInterfaces)
		netGroup.GET("/routes", s.handleListNetworkRoutes)
		netGroup.GET("/listeners", s.handleListNetworkListeners)
		// P14.2 的第五条，仍然是只读：它给的是"要改的话该带什么"（config_hash）
		// 与"有没有一条还没确认的回滚"。改本身走预检 → 任务。
		netGroup.GET("/config", s.handleGetNetworkConfig)

		fwGroup := v1.Group("/firewall", s.auditRoute(audit.ActionFirewallManage))
		fwGroup.Use(s.requireSession(), s.requireAdmin())
		fwGroup.GET("", s.handleGetFirewall)
		// P14.3 的第二条，仍然只读：它给的是"要改的话该带什么"（config_hash）、
		// 面板管理的规则集，以及有没有一条还没确认的回滚。改本身走预检 → 任务。
		fwGroup.GET("/config", s.handleGetFirewallConfig)

		// 告警（P13.2）：读 V、写 admin + CSRF。
		//
		// 为什么读开给 viewer：告警列表是“这台机器现在有没有事”的一部分，与
		// /system/summary 同级；而改规则会改变“出事时会不会有人知道”，只有 admin。
		//
		// 事件与规则同组：被中间件拒绝时（viewer 越权、会话失效）动作名都归到
		// alert.rule.manage —— 拒绝发生在 handler 之前，只有路由组知道这是哪一域。
		// 静默单独一组：它影响的是“要不要吵醒人”，与规则不是同一件事。
		alertRules := v1.Group("/alerts", s.auditRoute(audit.ActionAlertRuleManage))
		alertRules.Use(s.requireSession())
		alertRules.GET("/rules", s.handleListAlertRules)
		alertRules.GET("/events", s.handleListAlertEvents)
		alertRules.POST("/rules", s.requireAdmin(), s.requireCSRF(), s.handleCreateAlertRule)
		alertRules.PATCH("/rules/:id", s.requireAdmin(), s.requireCSRF(), s.handleUpdateAlertRule)
		alertRules.DELETE("/rules/:id", s.requireAdmin(), s.requireCSRF(), s.handleDeleteAlertRule)

		alertSilence := v1.Group("/alerts", s.auditRoute(audit.ActionAlertSilenceManage))
		alertSilence.Use(s.requireSession())
		alertSilence.GET("/silence", s.handleGetAlertSilence)
		alertSilence.PUT("/silence", s.requireAdmin(), s.requireCSRF(), s.handlePutAlertSilence)

		// 事件确认（P13.4）：admin + CSRF。
		//
		// 单独一组而不是挂进 alertRules：确认不是"管理规则"，被中间件拒绝时
		// 审计里该写的是 alert.event.ack —— 一条 denied 记录说不清是哪种动作，
		// 事后就只能靠猜。这组只有这一条路由，所以没有 ".manage" 的兜底动作名。
		alertEventAck := v1.Group("/alerts", s.auditRoute(audit.ActionAlertEventAck))
		alertEventAck.Use(s.requireSession())
		alertEventAck.POST("/events/:id/ack", s.requireAdmin(), s.requireCSRF(), s.handleAckAlertEvent)

		// 通知通道（P13.3）：全部 admin + CSRF，**读也不例外**。
		//
		// 与 /alerts 的读开给 viewer 不同：通道列表里带着 webhook 地址与收件邮箱，
		// 那是“告警会发到谁的邮箱”的答案。viewer 看得到规则已经足够判断这台机器
		// 有没有人管，不必知道外部投递的落点。
		//
		// 测试发送是独立的一条 POST：它是“现在就往外发一条”的显式操作，
		// 保存配置绝不会顺手触发它（CONTRACTS §13）。
		notifyChannels := v1.Group("/notification-channels",
			s.auditRoute(audit.ActionNotificationChannelManage))
		notifyChannels.Use(s.requireSession(), s.requireAdmin())
		notifyChannels.GET("", s.handleListNotificationChannels)
		notifyChannels.POST("", s.requireCSRF(), s.handleCreateNotificationChannel)
		notifyChannels.PATCH("/:id", s.requireCSRF(), s.handleUpdateNotificationChannel)
		notifyChannels.DELETE("/:id", s.requireCSRF(), s.handleDeleteNotificationChannel)
		notifyChannels.POST("/:id/test", s.requireCSRF(), s.handleTestNotificationChannel)

		// 投递账 / 面板收件箱（P13.4）：admin，只读。
		//
		// 它是**独立的一条路径**而不是 /notification-channels/:id/deliveries：
		// 投递账跨越所有通道（一次告警会往每条启用通道各下一行），挂在某个通道
		// 下面会让人以为"删掉通道就看不到那次投递了"。行里的通道字段是**快照**，
		// 通道被删之后历史照旧读得出来。
		//
		// 被中间件拒绝时借通道的动作名：它回答的是同一个问题（谁在看告警发到哪）。
		deliveries := v1.Group("/notification-deliveries",
			s.auditRoute(audit.ActionNotificationChannelManage))
		deliveries.Use(s.requireSession(), s.requireAdmin())
		deliveries.GET("", s.handleListNotificationDeliveries)

		// 存储只读视图（P04.2）：权限 V（CONTRACTS.md 第 6 节）。
		// 三个路由读的是代理的同一份扫描快照，因此设备列表与拓扑永远一致；
		// 本阶段没有任何写路由——分区/格式化要等 P07 的预检与任务链路。
		st := v1.Group("/storage")
		st.Use(s.requireSession())
		st.GET("/topology", s.handleStorageTopology)
		st.GET("/devices", s.handleListDevices)
		st.GET("/devices/:id", s.handleGetDevice)

		// 受管理卷（P07.4）：列表与详情是 V，占用详情是 admin
		// （CONTRACTS.md 第 6 节：占用里有进程命令行与 PID）。
		// 这些路由**只读**：导入/挂载/卸载/改配置/退出管理都走统一的
		// POST /operations/preflight + POST /jobs（action=storage.volume.*），
		// 不在这里开一个“直接执行”的入口。
		st.GET("/volumes", s.handleListVolumes)
		st.GET("/volumes/:id", s.handleGetVolume)
		st.GET("/volumes/:id/blockers", s.requireAdmin(), s.handleVolumeBlockers)

		// 共享（P08.1）：整组 admin（CONTRACTS.md 第 7 节）。共享定义里有路径、
		// 账号名单与来源网段，合起来就是“谁能访问哪个目录”的完整答案，viewer
		// 只在首页看数量与健康摘要。
		//
		// 三条路由都**只读**，而且都把能力报告一起返回：definition.enabled 是意图，
		// config_applied 是事实，两者分开显示才不会被读成“已生效”。
		// 应用/禁用/删除共享、建删账号都是系统写操作，走 POST /operations/preflight
		// + POST /jobs（action=share.* 与 share.account.*），这里不开“直接执行”的入口。
		sh := v1.Group("/shares")
		sh.Use(s.requireSession(), s.requireAdmin())
		sh.GET("", s.handleListShares)
		sh.GET("/:id", s.handleGetShare)
		// 连接状态（P08.2）：读的是 smbstatus 此刻报出的会话表，不是登记表。
		// 单独一条子路径而不是塞进详情：连接会变，详情不会。
		sh.GET("/:id/connections", s.handleGetShareConnections)

		// 受管理账号（P08.1）：admin。返回的每一条都没有密码、没有哈希，
		// 也没有 secret_ref——DTO 上就没有那些字段。
		sa := v1.Group("/share-accounts")
		sa.Use(s.requireSession(), s.requireAdmin())
		sa.GET("", s.handleListShareAccounts)

		// 受管理 Linux 账号（P14.5）：admin（CONTRACTS.md 第 13 节）。
		//
		// /accounts 与 /share-accounts 读同一份记录（DECISIONS Q475：普通账号与
		// 共享账号是同一张 managed_accounts 的两种 type），旧路由留着是因为共享页
		// 已经用它。/accounts/:id/ssh-keys 读的是另一张表（0014 的 managed_ssh_keys）。
		//
		// 两条都只读：建账号、改壳、改口令、加撤公钥、发撤 sudo 规则都走
		// POST /operations/preflight + POST /jobs（action=account.* / ssh_key.* /
		// sudo_rule.*），这里不开“直接执行”的入口。
		ac := v1.Group("/accounts")
		ac.Use(s.requireSession(), s.requireAdmin())
		ac.GET("", s.handleListAccounts)
		ac.GET("/:id/ssh-keys", s.handleListAccountSSHKeys)

		// 秘密通道（P08.1）：admin + CSRF + 5 分钟内再认证，只进不出。
		//
		// 这是唯一一条“写”但不进任务链路的接口：它不改主机状态，只是把明文交给代理
		// 加密保存并拿回引用。真正的系统变更（设 Samba 密码、写共享配置）仍是预检 +
		// 任务，参数里只有 secret_ref。协议里没有读取明文的路由，这里也没有。
		sec := v1.Group("/secrets", s.auditRoute(audit.ActionSecretCreate))
		sec.Use(s.requireSession(), s.requireAdmin())
		sec.POST("", s.requireCSRF(), s.requireReauth(), s.handleCreateSecret)

		// 应用与分组元数据（P09.1）：读 V、写 admin + CSRF（CONTRACTS.md 第 8 节）。
		//
		// 这一组只改元数据，**不碰主机**：不启动容器、不重启服务、不执行 Compose YAML。
		// 因此它走的是“直接写库 + 审计”，而不是预检 → 任务链路；反过来，任何真正
		// 影响运行中的动作（P09.3/P09.4 的启停、日志）仍然必须走 POST /operations/preflight
		// + POST /jobs，这里不会出现“直接执行”的入口。
		//
		// 收藏是当前用户的偏好：PUT 幂等、不需要 version，viewer 也能改自己的星标。
		ap := v1.Group("/apps", s.auditRoute(audit.ActionAppManage))
		ap.Use(s.requireSession())
		ap.GET("", s.handleListApps)
		// 发现候选（P09.2）：admin。静态段必须与 /:id 并存——gin 的树会先匹配静态段，
		// 因此 /apps/discovery 不会被当成 id=discovery 的应用详情。
		ap.GET("/discovery", s.requireAdmin(), s.handleAppDiscovery)
		ap.GET("/:id", s.handleGetApp)
		// 最近一次健康探测结论（P09.2）：V。探测本身由 Web 进程的调度器按
		// interval_seconds 触发，这里只读库里那一行。
		ap.GET("/:id/health", s.handleGetAppHealth)
		// 关联对象的日志（P09.4）：admin。日志对象由应用的绑定决定（systemd 单元或
		// 容器 ID），请求里不能指定——否则这个接口就变成“读任意单元/任意容器的日志
		// 的通用入口”，绕过 /logs 与 /containers/:id/logs 各自的边界。
		ap.GET("/:id/logs", s.requireAdmin(), s.handleGetAppLogs)
		ap.POST("", s.requireAdmin(), s.requireCSRF(), s.handleCreateApp)
		ap.PATCH("/:id", s.requireAdmin(), s.requireCSRF(), s.handleUpdateApp)
		ap.DELETE("/:id", s.requireAdmin(), s.requireCSRF(), s.handleDeleteApp)
		ap.PUT("/:id/favorite", s.requireCSRF(), s.handleSetAppFavorite)

		// 应用分组（P09.1）：列表 V，写 admin + CSRF。删除时分组里还有应用会被拒绝
		// （422 DEPENDENCY_BLOCKED），不会顺手把应用删掉或移组。
		ag := v1.Group("/app-groups", s.auditRoute(audit.ActionAppGroupManage))
		ag.Use(s.requireSession())
		ag.GET("", s.handleListAppGroups)
		ag.POST("", s.requireAdmin(), s.requireCSRF(), s.handleCreateAppGroup)
		ag.PATCH("/:id", s.requireAdmin(), s.requireCSRF(), s.handleUpdateAppGroup)
		ag.DELETE("/:id", s.requireAdmin(), s.requireCSRF(), s.handleDeleteAppGroup)

		// 软件包（P11.1）：整组 admin（CONTRACTS.md 第 12.1 节）。
		//
		// 全是只读。安装/卸载/升级/刷新索引是 P11.2 的动作，走预检 → 任务链路；
		// 这里没有、也不会有一个"直接执行 apt-get"的入口。
		//
		// /packages/updates 是静态段，gin 的树会先匹配它，不会被当成某个包名。
		pk := v1.Group("/packages")
		pk.Use(s.requireSession(), s.requireAdmin())
		pk.GET("", s.handleListPackages)
		pk.GET("/updates", s.handleListPackageUpdates)

		// 备份管理（P12.2）：整组 admin（CONTRACTS.md 第 12 节）。
		//
		// 这一组编辑的是**代理里那份定义**（面板不存第二份），所以代理不在时整组回
		// 503，而不是"先登记、回头再同步"。它不执行备份，也不删任何快照：执行是
		// backup.run 的预检 → 任务，清理是 backup.prune 的预检 → 任务。
		//
		// 三个组分开登记，因为审计动作不同（中间件拒绝发生在 handler 之前，只有
		// 组级别能带上正确的动作名）：
		//   - 仓库：登记与修改要 CSRF + 再认证（定义引用仓库口令与 SFTP 私钥）；
		//   - 策略：要 CSRF，不再认证（不引用秘密，也不动主机状态）；
		//   - 快照：纯只读查询，没有写入口。
		//
		// /backup/repositories 没有 DELETE：契约里没有这条，而且删掉定义会让
		// 仓库里那些快照"属于谁"无从回答。策略可以删——删策略不动仓库与快照。
		bkRepo := v1.Group("/backup", s.auditRoute(audit.ActionBackupRepositoryManage))
		bkRepo.Use(s.requireSession(), s.requireAdmin())
		bkRepo.GET("/repositories", s.handleListBackupRepositories)
		bkRepo.POST("/repositories", s.requireCSRF(), s.requireReauth(), s.handleCreateBackupRepository)
		bkRepo.GET("/repositories/:id", s.handleGetBackupRepository)
		bkRepo.PATCH("/repositories/:id", s.requireCSRF(), s.requireReauth(), s.handleUpdateBackupRepository)

		bkPolicy := v1.Group("/backup", s.auditRoute(audit.ActionBackupPolicyManage))
		bkPolicy.Use(s.requireSession(), s.requireAdmin())
		bkPolicy.GET("/policies", s.handleListBackupPolicies)
		bkPolicy.POST("/policies", s.requireCSRF(), s.handleCreateBackupPolicy)
		bkPolicy.GET("/policies/:id", s.handleGetBackupPolicy)
		bkPolicy.PATCH("/policies/:id", s.requireCSRF(), s.handleUpdateBackupPolicy)
		bkPolicy.DELETE("/policies/:id", s.requireCSRF(), s.handleDeleteBackupPolicy)

		// 快照查询。repository_id 是查询参数而不是路径段：快照 id 在仓库内部唯一，
		// 而面板没有"快照 → 仓库"的索引（有索引就等于存了第二份真相）。
		bkSnap := v1.Group("/backup")
		bkSnap.Use(s.requireSession(), s.requireAdmin())
		bkSnap.GET("/snapshots", s.handleListBackupSnapshots)
		bkSnap.GET("/snapshots/:id", s.handleGetBackupSnapshot)

		// 计划任务与受管理脚本（P12.4）：整组 admin（CONTRACTS.md 第 12 节）。
		//
		// 四个接口全是只读，读的是代理库里那一份（面板不存第二份）。代理不在时
		// 整组 503，而不是"给一份空列表"——空列表会被读成"这台机器上没有定时任务"。
		//
		// 写路径不在这里：建立/修改/删除是 schedule.apply、schedule.delete、
		// script.apply、script.delete 的预检 → 任务（要再认证、要落任务、要审计）；
		// 立即执行是 script.run。在这里开一个"直接写定义"的路由，等于把那条链路上
		// 的检查全绕过去——所以这里只有 GET。
		//
		// 两个组分开登记，因为审计动作不同（中间件拒绝发生在 handler 之前）。
		sch := v1.Group("/schedules", s.auditRoute(audit.ActionScheduleManage))
		sch.Use(s.requireSession(), s.requireAdmin())
		sch.GET("", s.handleListSchedules)
		sch.GET("/:id", s.handleGetSchedule)

		scr := v1.Group("/scripts", s.auditRoute(audit.ActionScriptManage))
		scr.Use(s.requireSession(), s.requireAdmin())
		scr.GET("", s.handleListScripts)
		scr.GET("/:id", s.handleGetScript)

		// 反向代理站点（P10.1）：整组 admin（CONTRACTS.md 第 9 节）。
		//
		// 这一组只登记站点定义与读取 Nginx 现状，**一个字节都不写进 /etc/nginx**：
		// 生成配置、原子写入、nginx -t、reload 是 P10.2 的 proxy.apply 任务。
		// 因此这里没有 apply 入口，响应里也没有任何“已生效”字段——enabled 是意图，
		// 不是事实。
		//
		// /proxies/nginx 与 /proxies/conflict 是静态段，gin 的树会先匹配它们，
		// 所以不会被当成 id=nginx 或 id=conflict 的详情。
		px := v1.Group("/proxies", s.auditRoute(audit.ActionProxyManage))
		px.Use(s.requireSession(), s.requireAdmin())
		px.GET("", s.handleListProxies)
		px.GET("/nginx", s.handleProxyNginxFacts)
		px.GET("/conflict", s.handleProxyDomainCheck)
		px.GET("/:id", s.handleGetProxySite)
		px.POST("", s.requireCSRF(), s.handleCreateProxySite)
		px.PATCH("/:id", s.requireCSRF(), s.handleUpdateProxySite)
		px.DELETE("/:id", s.requireCSRF(), s.handleDeleteProxySite)

		// 证书（P10.3）：读 admin；上传 admin + CSRF + 再认证（CONTRACTS.md 第 9 节）。
		//
		// 这一组同样**不改主机状态**：列表与详情读的是面板库里的记录（附带一次
		// certificate.state 现场读取），uploads 只把公钥证书暂存在本进程内存里十分钟。
		// 真正把证书装上主机的是 certificate.import / issue / renew 三个动作，
		// 走预检 → 任务链路，这里不提供"直接导入"或"直接签发"的入口。
		cs := v1.Group("/certificates", s.auditRoute(audit.ActionCertificateManage))
		cs.Use(s.requireSession(), s.requireAdmin())
		cs.GET("", s.handleListCertificates)
		cs.GET("/:id", s.handleGetCertificate)
		// 再认证的理由：上传的是一份会被装进 Nginx 的公钥证书，而"我刚登录所以我有权"
		// 与"我此刻确实在场"是两件事。CSRF 头挡的是别的站点代发的表单。
		cs.POST("/uploads", s.requireCSRF(), s.requireReauth(), s.handleUploadCertificate)

		// 服务与进程（P05.1）：权限 V（CONTRACTS.md 第 10 节）。只读路由在这里，
		// 启停/重启/开机启动是写操作，走统一的预检 → 任务链路（POST /operations/preflight
		// + POST /jobs，action=service.control），不在这里开一个“直接执行”的入口。
		sv := v1.Group("/services")
		sv.Use(s.requireSession())
		sv.GET("", s.handleListServices)
		sv.GET("/:unit", s.handleGetService)

		// 进程列表（P05.2）：权限 admin（CONTRACTS.md 第 10 节）。终止进程是写操作，
		// 走预检 → 任务链路（action=process.signal），而且 TERM 与 KILL 是两次独立操作：
		// KILL 的预检要求再认证与确认短语。这里不提供“直接 kill”的入口。
		pr := v1.Group("/processes")
		pr.Use(s.requireSession(), s.requireAdmin())
		pr.GET("", s.handleListProcesses)

		// 日志（P05.3）：权限 admin（CONTRACTS.md 第 10 节）。原始日志里可能有
		// 口令形态的字符串、内网地址、别人的命令行——脱敏是尽力措施，不能作为
		// 向 viewer 开放日志的依据（PLAN.md 第 14 节），所以整组 requireAdmin。
		//
		// /logs/stream 与任务事件流一样是 GET + SSE：没有 requireCSRF
		// （EventSource 无法自定义请求头），靠会话 Cookie + 同源守卫界定来源。
		lg := v1.Group("/logs")
		lg.Use(s.requireSession(), s.requireAdmin())
		lg.GET("", s.handleLogs)
		lg.GET("/stream", s.handleLogsStream)

		// 文件管理（P06.1）：整组 admin（CONTRACTS.md 第 11 节）。
		// 四个只读路由：列根、列目录、读文本、下载。上传与修改在 P06.2/P06.3，
		// 走预检 → 任务链路，这里不提供“直接写文件”的入口。
		//
		// 下载是 GET（浏览器直接下载，同源 Cookie），与日志流一样不要求 CSRF 头：
		// 它不改状态，且由会话 Cookie + 同源守卫界定来源。
		fm := v1.Group("/files")
		fm.Use(s.requireSession(), s.requireAdmin())
		fm.GET("/roots", s.handleFileRoots)
		fm.GET("/list", s.handleFileList)
		fm.GET("/text", s.handleFileText)
		fm.GET("/download", s.handleFileDownload)
		// 暂存上传（P06.2）：内容收进根内 .upanel/staging，目标路径一个字节都没变。
		// 落地必须走 file.commit_upload 的预检 → 任务；所以这里**没有**“直接写文件”的路由，
		// 只有“把字节放到暂存区”的三条 + “放弃这次上传”一条。
		//
		// PUT 是唯一允许的大请求体入口：它不解析 JSON、不缓存内容，边读边转发给代理。
		fm.POST("/uploads", s.requireCSRF(), s.handleCreateUpload)
		fm.PUT("/uploads/:id/content", s.requireCSRF(), s.handleUploadContent)
		fm.GET("/uploads/:id", s.handleGetUpload)
		fm.DELETE("/uploads/:id", s.requireCSRF(), s.handleDeleteUpload)

		// 受限终端（P06.3）：整组 admin（CONTRACTS.md 第 10 节）。开会话不在路由表里：
		// 那是 system 写操作，走 terminal.create 的预检 → 任务链路。这里只有
		// “看自己的会话、领一次性许可、连、关”四条，没有“直接开一个 shell”的入口。
		//
		// 许可必须由 POST 领取（带 CSRF）：WebSocket 握手是 GET，浏览器无法为它加
		// 自定义请求头，许可因此是这次升级的“CSRF 令牌替身”，30 秒、一次性、
		// 只经子协议传递（见 terminalws.go）。
		//
		// 两个组分开登记审计动作（attach / close），因为中间件拒绝发生在 handler
		// 之前，只有组级别能带上正确的动作名。
		tmAttach := v1.Group("/terminal", s.auditRoute(audit.ActionTerminalAttach))
		tmAttach.Use(s.requireSession(), s.requireAdmin())
		tmAttach.GET("/sessions", s.handleTerminalSessions)
		tmAttach.POST("/sessions/:id/connection-grants", s.requireCSRF(), s.handleCreateTerminalGrant)
		tmAttach.GET("/sessions/:id/connect", s.handleTerminalConnect)

		tmClose := v1.Group("/terminal", s.auditRoute(audit.ActionTerminalClose))
		tmClose.Use(s.requireSession(), s.requireAdmin())
		tmClose.DELETE("/sessions/:id", s.requireCSRF(), s.handleTerminalClose)

		// 容器与部署（P09.3）：整组 admin（CONTRACTS.md 第 8 节）。容器清单等于
		// “这台机器上跑了什么、发布了哪些端口、挂了哪些目录”，与日志同理属于侦察
		// 情报，viewer 得到 403。
		//
		// 这里只有读：启停在 /operations/preflight + /jobs（action=container.control）。
		// 危险容器（privileged、host 网络/PID、危险 capability、挂 docker.sock 或
		// 宿主根）的 risk 与 capabilities 由代理给出，界面据此显示“不接管”，
		// Web 侧不自己算一遍。
		//
		// /containers/:id/logs/stream 与 /logs/stream 一样是 GET + SSE：没有 requireCSRF。
		cn := v1.Group("/containers")
		cn.Use(s.requireSession(), s.requireAdmin())
		cn.GET("", s.handleListContainers)
		cn.GET("/:id", s.handleGetContainer)
		cn.GET("/:id/logs", s.handleContainerLogs)
		cn.GET("/:id/logs/stream", s.handleContainerLogsStream)

		im := v1.Group("/images")
		im.Use(s.requireSession(), s.requireAdmin())
		im.GET("", s.handleListImages)

		cnn := v1.Group("/container-networks")
		cnn.Use(s.requireSession(), s.requireAdmin())
		cnn.GET("", s.handleListContainerNetworks)

		cnv := v1.Group("/container-volumes")
		cnv.Use(s.requireSession(), s.requireAdmin())
		cnv.GET("", s.handleListContainerVolumes)

		dp := v1.Group("/deployments")
		dp.Use(s.requireSession(), s.requireAdmin())
		dp.GET("", s.handleListDeployments)
		// 管理面（P15.4）：面板管着哪些部署、它们现在是什么状态。与上面那条
		// **观测面**不同源（一个读 docker，一个读配置目录与 systemd），理由见
		// internal/api/deployments.go 的文件头。
		//
		// 静态段与参数段并存：gin 的树会先匹配静态段（同 /apps/discovery 与
		// /apps/:id 的先例），所以 /deployments/managed 不会被 /deployments/:id
		// 吃掉，而 /deployments/managed/:name 也不会被当成一个两段的 :id。
		dp.GET("/managed", s.handleListManagedDeployments)
		dp.GET("/managed/:name", s.handleGetManagedDeployment)
		dp.GET("/:id", s.handleGetDeployment)

		// 系统写操作的统一入口（P03.1）：先预检，再由 POST /jobs 创建任务（P03.2）。
		// 预检只读不写系统；再认证在**提交任务**时校验（按动作登记策略），不在预检阶段。
		ops := v1.Group("/operations", s.auditRoute(audit.ActionOperationPreflight))
		ops.Use(s.requireSession(), s.requireAdmin())
		ops.POST("/preflight", s.requireCSRF(), s.handleCreatePreflight)

		// 任务（P03.2）。读任务允许 viewer（CONTRACTS.md 4.3 的权限列是 V）：
		// 看自己提交的任务状态不需要管理员权限；提交与取消必须 admin + CSRF。
		jobs := v1.Group("/jobs", s.auditRoute(audit.ActionJobManage))
		jobs.Use(s.requireSession())
		jobs.POST("", s.requireAdmin(), s.requireCSRF(), s.handleCreateJob)
		jobs.GET("", s.handleListJobs)
		jobs.GET("/:id", s.handleGetJob)
		jobs.POST("/:id/cancel", s.requireAdmin(), s.requireCSRF(), s.handleCancelJob)
		// 事件流与日志按契约只有 admin 能看（CONTRACTS.md 4.3：GET /jobs/:id/events、/logs 是 admin，
		// 而 GET /jobs 与 GET /jobs/:id 是 V）。viewer 看到的是摘要，看不到流。
		//
		// 注意这里**没有** requireCSRF：SSE 是 GET，EventSource 无法自定义请求头，
		// 靠会话 Cookie + Origin 校验 + 同源守卫（originGuard）来界定来源。
		jobs.GET("/:id/events", s.requireAdmin(), s.handleJobEvents)
		jobs.GET("/:id/logs", s.requireAdmin(), s.handleJobLogs)
	}

	r.NoRoute(s.handleNoRoute)
	r.NoMethod(s.handleMethodNotAllowed)
	return r
}

func (s *Server) handleHealthLive(c *gin.Context) {
	httpx.WriteData(c.Writer, c.Request, http.StatusOK, healthLive{Status: "ok"})
}

// handleNoRoute 处理未注册的路径。
//
// 关键约束（P01 验收）：未知的 API 路径不能返回 index.html，也不能返回 200。
// 因此 API 前缀先判，命中即 JSON 404，不再往下走静态回退。
func (s *Server) handleNoRoute(c *gin.Context) {
	if isAPIPath(c.Request.URL.Path) || s.opts.Frontend == nil {
		writeError(c, http.StatusNotFound, httpx.CodeNotFound, "资源不存在")
		return
	}
	s.opts.Frontend.ServeHTTP(c.Writer, c.Request)
}

// isAPIPath 判断路径是否属于 API 命名空间（/api 本身与 /api/... 都算）。
func isAPIPath(urlPath string) bool {
	return urlPath == "/api" || strings.HasPrefix(urlPath, "/api/")
}

// handleMethodNotAllowed 只覆盖“路径已注册、方法不支持”的情况（例如 POST /api/v1/health/live）。
// 未注册路径上的任何方法都走 NoRoute：API 前缀给 JSON 404，页面路径交给静态处理器（它自己回 405）。
func (s *Server) handleMethodNotAllowed(c *gin.Context) {
	writeError(c, http.StatusMethodNotAllowed, httpx.CodeValidationError, "该路径不支持此请求方法")
}
