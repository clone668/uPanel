// 模块清单：导航的唯一来源。
//
// PLAN.md 第 7 节把导航固定为 16 项，顺序不可改：
//   概览、应用、存储、共享、容器、服务与进程、文件、终端、日志、备份、
//   计划任务、网络、更新、监控告警、任务中心、设置。
//
// 规则：未实现的模块**隐藏**，不摆放可点击但无效的入口（PLAN.md 第 7 节）。
// 因此导航渲染的是 implementedIn !== null 的项；其余保留在清单里，作为路线图，
// 由对应阶段把 implementedIn 填上才会出现在界面上。

export interface ModuleDef {
  /** 稳定的内部键，用于路由与菜单标记，不随中文名变化。 */
  key: string
  /** 中文导航名，必须与 PLAN.md 第 7 节完全一致。 */
  label: string
  /** 前端路由路径。 */
  path: string
  /** 该模块的页面在本项目哪个阶段可用；null 表示尚未实现（导航隐藏）。 */
  implementedIn: string | null
  /** 一句话说明当前页面能做什么，用于空页面提示，不承诺未实现的能力。 */
  summary: string
}

export const MODULES: readonly ModuleDef[] = [
  {
    key: 'overview',
    label: '概览',
    path: '/',
    implementedIn: 'P04.4',
    summary: '主机、CPU、内存、负载、网络与磁盘概要；服务、应用与更新在后续阶段接入。',
  },
  {
    key: 'apps',
    label: '应用',
    path: '/apps',
    implementedIn: 'P09.5',
    summary:
      '应用卡片与一键打开：运行时状态与入口状态分开显示，按分组与搜索整理，启停走预检与任务；本机候选确认成应用，依赖卷接管情况逐条写明。',
  },
  {
    key: 'storage',
    label: '存储',
    path: '/storage',
    implementedIn: 'P04.4',
    summary: '磁盘、分区、文件系统、拓扑详情、健康与容量；写操作自 P07 起开放。',
  },
  {
    key: 'shares',
    label: '共享',
    path: '/shares',
    implementedIn: 'P08.4',
    summary: 'SMB 共享定义、受管理账号与客户端连接说明；共享与卷的绑定、配置写入与运行时脱盘撤销自 P08 起接通。',
  },
  { key: 'containers', label: '容器', path: '/containers', implementedIn: null, summary: '容器与镜像' },
  {
    key: 'services',
    label: '服务与进程',
    path: '/services',
    implementedIn: 'P05.1',
    summary:
      'systemd 服务单元与进程列表：状态、保护名单、启停/重启、开机启动与终止（经预检与任务）。',
  },
  {
    key: 'files',
    label: '文件',
    path: '/files',
    implementedIn: 'P06.1',
    summary:
      '受管理文件根内的目录浏览、文本预览与受控下载：路径限定在根内，符号链接不跟随，写操作待后续阶段。',
  },
  {
    key: 'terminal',
    label: '终端',
    path: '/terminal',
    implementedIn: 'P06.3',
    summary:
      '受限 Web 终端：以 upanel-shell 降权运行（非 root、无 sudo），经预检与再认证创建，一次性许可建立连接，时长与数量均有上限。',
  },
  {
    key: 'logs',
    label: '日志',
    path: '/logs',
    implementedIn: 'P05.3',
    summary:
      '按数据源、单元、级别与时间窗口查询 journald（单页 ≤1000 行 / 1 MiB，窗口 ≤24 小时），并实时订阅；原始日志仅管理员可读。',
  },
  {
    key: 'backup',
    label: '备份',
    path: '/backup',
    implementedIn: 'P12.5',
    summary:
      'restic 仓库定义与策略、快照浏览与详情、备份/恢复/验证/清理；全部经预检与任务，备份成功不等于恢复已验证。',
  },
  {
    key: 'schedules',
    label: '计划任务',
    path: '/schedules',
    implementedIn: 'P12.5',
    summary:
      'systemd timer 计划与受管理脚本：定义只读、改动走预检与任务；定义的意图、单元是否装上、是否 enable、timer 是否在跑分开显示。',
  },
  {
    key: 'network',
    label: '网络',
    path: '/network',
    implementedIn: 'P14.1',
    summary:
      '网卡与地址、路由与默认网关、监听端口与占用进程、防火墙规则；全部只读，改地址与改规则自 P14.2 起开放，且会先给出"能不能改"的判定与原因。',
  },
  {
    key: 'updates',
    label: '更新',
    path: '/updates',
    implementedIn: 'P11.1',
    summary:
      '已安装软件包、候选版本与来源、可升级与安全更新、重启标志；全部只读，变更操作自 P11.2 起开放。',
  },
  {
    key: 'alerts',
    label: '监控告警',
    path: '/alerts',
    implementedIn: 'P13.4',
    summary:
      '历史曲线（按粒度保留，缺失画成断开而不是 0）、告警规则与事件、确认已知，以及通知投递账：哪一次告警到底发没发出去。',
  },
  {
    key: 'jobs',
    label: '任务中心',
    path: '/jobs',
    implementedIn: 'P03.3',
    summary: '任务列表、状态机、事件流与脱敏日志',
  },
  {
    key: 'settings',
    label: '设置',
    path: '/settings',
    implementedIn: 'P14.6',
    summary:
      '本机账号安全：二次验证（TOTP）的启用确认、一次性恢复码展示与关闭；系统级设置随各自阶段接入。',
  },
] as const

/** 导航可见项：只包含已实现的模块。 */
export function visibleModules(modules: readonly ModuleDef[] = MODULES): ModuleDef[] {
  return modules.filter((module) => module.implementedIn !== null)
}

/** 按 key 查找模块；找不到返回 undefined（路由守卫据此判断是否为已知模块）。 */
export function findModule(key: string): ModuleDef | undefined {
  return MODULES.find((module) => module.key === key)
}
