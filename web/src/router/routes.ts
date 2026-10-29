// 路由表（纯数据）。
//
// 只登记**已实现**的模块页面（module.implementedIn !== null）。
// 未实现模块不进路由表：直接访问其路径会落到 404 页面，而不是一个空白或假成功的页面。

import type { RouteRecordRaw } from 'vue-router'

import { findModule } from '@/modules/registry'

export const appRoutes: readonly RouteRecordRaw[] = [
  {
    path: '/login',
    name: 'login',
    component: () => import('@/views/LoginView.vue'),
    // 登录页不属于任何导航模块：它没有侧栏高亮，也不需要登录。
    meta: { moduleKey: null, public: true },
  },
  {
    // 桌面（Q533）。登录后落在这里，看到的是图标与壁纸，不是某一页数据。
    // 它不属于任何模块：桌面上没有"当前模块"，高亮由图标自己判断。
    // 不登记这一条的话，关窗就没地方可去，只能把地址栏留在刚关掉的那一页上。
    path: '/desk',
    name: 'desk',
    component: () => import('@/views/DeskView.vue'),
    meta: { moduleKey: null },
  },
  {
    path: '/',
    name: 'overview',
    component: () => import('@/views/OverviewView.vue'),
    meta: { moduleKey: 'overview' },
  },
  {
    path: '/storage',
    name: 'storage',
    component: () => import('@/views/StorageView.vue'),
    meta: { moduleKey: 'storage' },
  },
  {
    // 设备 id 是内核设备名，只允许契约里的字符集；非法值直接落 404，
    // 不会把一个奇怪的参数带进页面再让它自己出错。
    path: '/storage/devices/:id([A-Za-z0-9._-]+)',
    name: 'storage-device',
    component: () => import('@/views/StorageDeviceView.vue'),
    meta: { moduleKey: 'storage' },
  },
  {
    path: '/shares',
    name: 'shares',
    component: () => import('@/views/SharesView.vue'),
    meta: { moduleKey: 'shares' },
  },
  {
    // 账号页（P14.5）：受管理 Linux 账号、SSH 公钥与受限 sudo 模板。
    // 导航固定 16 项，账号属于“共享”这一模块（同一张 managed_accounts，DECISIONS Q475），
    // 所以 moduleKey 指回 shares：侧栏高亮与页面标题都据此取。
    path: '/accounts',
    name: 'accounts',
    component: () => import('@/views/AccountsView.vue'),
    meta: { moduleKey: 'shares' },
  },
  {
    path: '/apps',
    name: 'apps',
    component: () => import('@/views/AppsView.vue'),
    meta: { moduleKey: 'apps' },
  },
  {
    // 受管部署页（P15.4）。导航固定 16 项，其中只有“容器”还是 implementedIn: null；
    // 用这一页去占那一格会超承诺——它读的是**受管部署**（/etc/upanel/deploy 下的三个文件
    // 加 systemd 的回答），不是容器与镜像管理。所以它挂在应用模块下，与账号挂在共享下
    // 同一个处置（DECISIONS Q475、Q495）。
    path: '/apps/deployments',
    name: 'deployments',
    component: () => import('@/views/DeploymentsView.vue'),
    meta: { moduleKey: 'apps' },
  },
  {
    path: '/services',
    name: 'services',
    component: () => import('@/views/ServicesView.vue'),
    meta: { moduleKey: 'services' },
  },
  {
    // 单元名限定在契约允许的字符集内：奇怪的名字直接落 404，
    // 不会把一个带斜杠或空格的参数带进页面再让它自己出错。
    path: '/services/:unit([A-Za-z0-9][A-Za-z0-9:_.@-]*)',
    name: 'service-unit',
    component: () => import('@/views/ServiceUnitView.vue'),
    meta: { moduleKey: 'services' },
  },
  {
    // 进程列表属于“服务与进程”模块：meta.moduleKey 指回 services，
    // 导航高亮与页面标题都据此取。
    path: '/processes',
    name: 'processes',
    component: () => import('@/views/ProcessesView.vue'),
    meta: { moduleKey: 'services' },
  },
  {
    path: '/logs',
    name: 'logs',
    component: () => import('@/views/LogsView.vue'),
    meta: { moduleKey: 'logs' },
  },
  {
    // 文件页只管理服务端登记的根；根与路径都是查询参数，不进 URL 路径段，
    // 因此这里没有 :id 之类的动态段——页面内部用相对路径导航。
    path: '/files',
    name: 'files',
    component: () => import('@/views/FilesView.vue'),
    meta: { moduleKey: 'files' },
  },
  {
    // 终端页与会话 id 无关：会话由服务端按当前账号给出（每账号最多 1 个），
    // 因此路径里没有 :id——把会话 id 放进 URL 只会多一个可以乱填的参数。
    path: '/terminal',
    name: 'terminal',
    component: () => import('@/views/TerminalView.vue'),
    meta: { moduleKey: 'terminal' },
  },
  {
    path: '/jobs',
    name: 'jobs',
    component: () => import('@/views/JobsView.vue'),
    meta: { moduleKey: 'jobs' },
  },
  {
    // 更新页（P11.1）只读：包列表、可升级清单与重启标志。
    // 分页游标是查询参数，所以路径里没有 :page 之类的段——游标是不透明的。
    path: '/updates',
    name: 'updates',
    component: () => import('@/views/UpdatesView.vue'),
    meta: { moduleKey: 'updates' },
  },
  {
    // 备份页（P12.5）：仓库、策略、快照与四个动作。
    // 快照 id 只在仓库内部唯一，所以仓库与策略都是页面内的选择，不进路径段。
    path: '/backup',
    name: 'backup',
    component: () => import('@/views/BackupView.vue'),
    meta: { moduleKey: 'backup' },
  },
  {
    // 计划任务页（P12.5）：计划与脚本。定义只能在代理里，页面只读；
    // 改动走 schedule.apply / script.apply 的预检 → 任务。
    path: '/schedules',
    name: 'schedules',
    component: () => import('@/views/SchedulesView.vue'),
    meta: { moduleKey: 'schedules' },
  },
  {
    // 监控告警页（P13.4）：历史曲线、规则、事件与确认、投递账。
    // 时间窗是页面内的选择，不进 URL 段——它是"看多久"，不是可以分享的定位。
    path: '/alerts',
    name: 'alerts',
    component: () => import('@/views/AlertsView.vue'),
    meta: { moduleKey: 'alerts' },
  },
  {
    // 网络页（P14.1）：网卡与地址、路由、监听端口、防火墙现状。
    // 四条都是只读查询，事实来自代理的 /proc、/sys 与 ufw。
    // 改地址与改规则分别是 network.apply / firewall.apply（P14.2/P14.3），
    // 走预检 → 任务，所以这里没有子路由、也没有写入口。
    path: '/network',
    name: 'network',
    component: () => import('@/views/NetworkView.vue'),
    meta: { moduleKey: 'network' },
  },
  {
    // 设置页（P14.6）：第一块内容是本机账号的二次验证（启用、恢复码、关闭）。
    // 系统级设置随各自阶段接入，页面里如实写明"目前只有这一块"。
    path: '/settings',
    name: 'settings',
    component: () => import('@/views/SettingsView.vue'),
    meta: { moduleKey: 'settings' },
  },
  {
    // 外部程序能力页（P16.1）：只读管理面，回答“这台机器装了哪些外部程序、什么版本”。
    // 权限是 admin（这份清单合起来就是主机指纹），所以挂在设置模块下，
    // 与二次验证同一格：它是系统级读数，不属于任何一个业务模块。
    path: '/settings/host',
    name: 'host-capabilities',
    component: () => import('@/views/HostCapabilitiesView.vue'),
    meta: { moduleKey: 'settings' },
  },
  {
    path: '/:pathMatch(.*)*',
    name: 'not-found',
    component: () => import('@/views/NotFoundView.vue'),
    meta: { moduleKey: null },
  },
]

/** 由路由 meta 里的模块键取中文标题；未知键返回 null。 */
export function moduleTitle(moduleKey: unknown): string | null {
  if (typeof moduleKey !== 'string') return null
  return findModule(moduleKey)?.label ?? null
}
