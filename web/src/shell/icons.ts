// 模块图标：一条 24×24 的 SVG 路径，描边、不填色、颜色吃 currentColor。
//
// 为什么不用字符/emoji：字符的字宽随字体变化，图标格对不齐；emoji 在不同系统
// 是彩色的，会和"一个强调色 + 语义色"的规则打架。这里只存路径数据，
// 渲染交给 ui/ModuleIcon.vue，颜色与线宽全部由令牌和组件控制。
//
// 覆盖要求：visibleModules() 里每个 key 都要有一条。漏了会在
// src/shell/icons.test.ts 红——新增模块必须同时给图标，不会静默掉到方块兜底。

const FALLBACK = 'M4 4h16v16H4z M4 9h16'

export const MODULE_ICONS: Readonly<Record<string, string>> = {
  // 概览：一块屏幕（这是"这台机器现在怎么样"的意思，不是四宫格）
  overview: 'M3 5h18v11H3z M9 20h6 M12 16v4',
  // 应用：四宫格（应用清单）
  apps: 'M4 4h7v7H4z M13 4h7v7h-7z M4 13h7v7H4z M13 13h7v7h-7z',
  // 存储：圆柱（磁盘/卷）
  storage:
    'M4 6.5c0-1.5 3.6-2.5 8-2.5s8 1 8 2.5-3.6 2.5-8 2.5-8-1-8-2.5z M4 6.5v11c0 1.5 3.6 2.5 8 2.5s8-1 8-2.5v-11',
  // 共享：文件夹带出去的箭头
  shares: 'M3 6h6l2 2h10v10H3z M9 13h7 M13 10l3 3-3 3',
  // 服务与进程：一条活动波形（服务在跑）
  services: 'M3 12h4l3-7 4 14 3-7h4',
  // 文件：带折角的页
  files: 'M6 3h8l4 4v14H6z M14 3v4h4',
  // 终端：窗口加提示符
  terminal: 'M3 4h18v16H3z M7 10l3 3-3 3 M13 16h4',
  // 日志：行
  logs: 'M4 6h16 M4 12h16 M4 18h10',
  // 备份：盒子加向下箭头（收进去）
  backup: 'M4 15v4h16v-4 M12 3v10 M8 9l4 4 4-4',
  // 计划任务：钟
  schedules: 'M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18z M12 7.5V12l3.5 2',
  // 网络：树形连接
  network: 'M9 3h6v4H9z M3 17h6v4H3z M15 17h6v4h-6z M12 7v5 M6 12h12 M6 12v5 M18 12v5',
  // 更新：圈内向上箭头
  updates: 'M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18z M12 16.5V8 M8.5 11.5 12 8l3.5 3.5',
  // 监控告警：铃
  alerts: 'M6 9a6 6 0 0 1 12 0v5l2 3H4l2-3z M10 20.5h4',
  // 任务中心：清单打勾
  jobs: 'M4 5h4v4H4z M11 7h9 M4 15h4v4H4z M11 17h9',
  // 设置：两条滑轨带滑块
  settings: 'M4 7h16 M4 17h16 M9 5.5h3.5v3H9z M13.5 15.5H17v3h-3.5z',
  // 容器（implementedIn 为 null，导航里不出现；图标先备好，接上时不用再改这里）
  containers: 'M4 8l8-4 8 4-8 4-8-4z M4 8v8l8 4 8-4V8 M12 12v8',
}

/** 取模块图标路径；未知模块返回一个中性方块，不会渲染成空白。 */
export function moduleIconPath(key: string): string {
  return MODULE_ICONS[key] ?? FALLBACK
}
