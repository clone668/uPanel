# 界面重做（续）— 25 页逐页收口的看图记录

**日期**：2026-10-30
**性质**：**宿主 Chromium 回放夹具的看图记录**。
**它不是 Ubuntu 真机验收，也不是界面验收。** 页面里的 `/api/v1/**` 响应全部由
`web/src/fixtures/` 里的夹具回放，不是活着的面板。按老规矩：夹具截图不算验收。

上一轮的记录在 `docs/evidence/UI/2026-10-30-shell/`（五张图，只走了概览页一条链路）。
这一轮把 23 个视图全部截了一遍，逐张看，改排版。

---

## 1. 环境与复现

| 项 | 值 |
|---|---|
| 宿主 | Windows（`H:\Code\uPanel`） |
| Node | **v22.23.3**，取自仓外的 `H:\Code\.toolchains\node-v22.23.3-win-x64` |
| Playwright | 1.64.1 |
| 浏览器 | Chromium **156.0.8078.4**（revision 1248），无头 |
| 视口 | 1440×900（桌面）；400×820（窄屏）；暗色靠 `emulateMedia({ colorScheme: 'dark' })` |
| 设备像素比 | 1 |
| dev server | `npm run dev` 起在 `http://127.0.0.1:5173` |

**为什么必须用仓外那份 Node**：系统 node 是 v24，跑 `vitest` / `vite` 会出假红（上一轮已记）。

复现：

```sh
cd web
export PATH="/h/Code/.toolchains/node-v22.23.3-win-x64:$PATH"
npm run dev                                  # 另一个终端
MSYS_NO_PATHCONV=1 node scripts/preview-pages.mjs http://127.0.0.1:5173 \
  ../docs/evidence/UI/2026-10-30-pages > ../docs/evidence/UI/2026-10-30-pages/facts-preview.json
```

`MSYS_NO_PATHCONV=1` 不是可选项：Git Bash 会把 argv 里的 `/h/Code/...` 改写成
`C:/Program Files/Git/h/Code/...`，`page.goto` 直接报 `Cannot navigate to invalid URL`。

逐页原始读数（每页的 `errors` / `unmatched` / `overflow` / `scrollH` / `h1`）在
`facts-preview.json`，本文的表格是它的摘录。

---

## 2. 25 张图的逐页读数

`overflow` 是 `document.documentElement.scrollWidth − clientWidth`，**大于 0 就是横向溢出**。
`unmatched` 是夹具没覆盖、被预览脚本回成 404 的接口**个数**。

| 图 | 路由 | 视口/主题 | 未覆盖接口 | pageerror | 横向溢出 | scrollH | 一级标题 |
|---|---|---|---|---|---|---|---|
| page-overview | `/` | 1440 明 | 0 | 0 | 0 | 1211 | 概览 |
| page-storage | `/storage` | 1440 明 | 0 | 0 | 0 | 2371 | 存储 |
| page-storage-device | `/storage/devices/sda` | 1440 明 | 0 | 0 | 0 | 2367 | sda |
| page-services | `/services` | 1440 明 | 0 | 0 | 0 | 14302 | 服务与进程 |
| page-service-unit | `/services/ssh.service` | 1440 明 | 0 | 0 | 0 | 900 | ssh.service |
| page-processes | `/processes` | 1440 明 | 0 | 0 | 0 | 6065 | 进程 |
| page-files | `/files` | 1440 明 | 0 | 0 | 0 | 900 | 文件 |
| page-shares | `/shares` | 1440 明 | 0 | 0 | 0 | 1478 | 共享 |
| page-apps | `/apps` | 1440 明 | 0 | 0 | 0 | 5127 | 应用 |
| page-updates | `/updates` | 1440 明 | 0 | 0 | 0 | 3544 | 更新 |
| page-logs | `/logs` | 1440 明 | 0 | 0 | 0 | 1331 | 日志 |
| page-jobs | `/jobs` | 1440 明 | 1 | 0 | 0 | 900 | 任务中心 |
| page-backup | `/backup` | 1440 明 | 2 | 0 | 0 | 900 | 备份 |
| page-alerts | `/alerts` | 1440 明 | 5 | 0 | 0 | 1162 | 监控告警 |
| page-schedules | `/schedules` | 1440 明 | 4 | 0 | 0 | 900 | 计划任务 |
| page-network | `/network` | 1440 明 | 6 | 0 | 0 | 900 | 网络 |
| page-settings | `/settings` | 1440 明 | 0 | 0 | 0 | 900 | 设置 |
| page-host | `/settings/host` | 1440 明 | 1 | 0 | 0 | 900 | （无） |
| page-terminal | `/terminal` | 1440 明 | 1 | 0 | 0 | 900 | 终端 |
| page-deployments | `/apps/deployments` | 1440 明 | 2 | 0 | 0 | 900 | 受管部署 |
| page-accounts | `/accounts` | 1440 明 | 1 | 0 | 0 | 900 | （无） |
| page-notfound | `/no-such-page` | 1440 明 | 0 | 0 | 0 | 900 | （无） |
| page-services-mobile | `/services` | 400 明 | 0 | 0 | 0 | 42229 | 服务与进程 |
| page-storage-dark | `/storage` | 1440 暗 | 0 | 0 | 0 | 2371 | 存储 |
| page-apps-dark | `/apps` | 1440 暗 | 0 | 0 | 0 | 5127 | 应用 |

**合计**：25 页 / **pageerror 0** / **横向溢出 0** / 未覆盖接口的页面 9 个 / 最大 scrollH **42229**。

读这张表要注意三件事：

1. **scrollH = 900 不等于"内容只有一屏"**。`scrollH` 是 `documentElement` 的滚动高度，
   内容窗口本身是内部滚动容器，所以这个数只说明**页面没有把桌布撑长**。真正的长页面在表里
   看得出（服务 14302、进程 6065、应用 5127、更新 3544）。
2. **pageerror 0 只说明没有未捕获异常**。9 个页面拿到的是夹具 404，它们正当地显示"读不到"，
   这不算错——但也不证明它们正常状态下的排版对。
3. **`page-services` 14302 与 `page-services-mobile` 42229** 是同一页同一份数据：三栏变单栏，
   高度涨到约 3 倍，符合预期。

---

## 3. 这一轮看图改出来的东西

按"看到的症状 → 成因 → 改法"记。**全部在 `web/src/` 里，后端、契约、路由表零改动。**

| # | 看到的 | 成因 | 改法 |
|---|---|---|---|
| 1 | 图标位显示成**一片实心蓝块**，文件名看不见 | 全局样式用 `!important` 强写 `--n-color`，压掉了 naive 写在同一元素的**内联**变体变量（文字按钮靠 `--n-color:#0000` 实现透明） | 删掉所有 `!important` 的 `--n-*` 覆盖。颜色一律走 `themeOverrides`；形状走直接属性 |
| 2 | 所有按钮塌成 **11—12px 高**，内边距 0 | 一条 `[style*='--n-color: #0000']` 选择器本想只命中文字按钮，结果**每个按钮**都有这个内联变量 | 整条规则删除；按钮的 26/28px 身高、`0 12px` 内边距、2px 方角移进 `theme.ts` 的 `Button` 覆盖 |
| 3 | 每个内容窗口里有一条 **16px 白带** | `views.css` 写的是 `.win section`，把 `UPanel` 自己的 `<section>` 根也套上了 `gap` | 收窄成 `.win > section, .win > main`；`AlertsView` 根从 `<div>` 改成 `<section>` 以留在覆盖内 |
| 4 | 设置页/任务中心页的说明文字**挤在标题右侧**，不成行 | 想用 `flex: 1 0 100%` 强制换行，但 flexbox 会用 `max-width` 先夹住 hypothetical main size，549px 的说明加标题正好一行放得下 | 加 `.settings__lead` / `.jobs__lead` / `.alerts-view__lead` 包一层，容器宽度由外层决定 |
| 5 | 设置页、任务中心页顶着**两个标题**（NCard 的 title + 页内 h1） | 上一轮给概览页做了真页头，其余页面还是 naive 的卡片标题 | 改成真页头（`h1` + 说明），`NCard` 只留内容 |
| 6 | 进度条只看得到一小截彩色的头，**轨道看不见** | `Progress.railColor` 用了 `--surface-2`，与 `--surface` 只差 7% 亮度，浅色下等于没有 | `theme.ts` 里 `Progress: { railColor: c.rule }` |
| 7 | 服务卡片的名称与描述**连成一行**（`ssh.serviceOpenBSD Secure Shell server`） | 描述是 inline 的 muted span，前面没有间距，也没有换行 | 卡片头部改成列布局（名称一行、标签一行）；描述加 `margin-left: 6px` 并降到 `--fs-sm` / `--ink-3` |
| 8 | 磁盘页、服务详情页右侧有一大片**死白** | 容器写死 `max-width: 1000px` / `1100px` | 删掉死的 max-width，靠 `min-width: 0` 让网格自己分配 |
| 9 | 容量条旁边的说明文字**跟数字一样大**，抢视线 | 缺 muted 规格 | `.capacity__line--muted` → `--fs-sm` / `--ink-3` |
| 10 | 备份页、计划任务页头部**两个主按钮打架** | 两个都用了 `type="primary"` | 第二个改 `secondary` |
| 11 | 404 页用了一个**彩色 emoji** 当图 | `NResult` 默认 `status` 会画 `🙁` | 去掉 `status`，`#icon` 位放 `<span class="nf__code u-num">404</span>`（44px，`--ink-3` 数字，无彩色） |
| 12 | 概览页仪表被**压成 84px 定宽** | 上一轮写死的 `.ov__gauge { width: 84px }` | 改成 `display: block`，跟着容器走 |
| 13 | 更新页"重启标志"卡片里有一行**和卡片标题重复的内容** | 同一件事写了两遍（computed `powerLine` 与后面的说明） | 删掉 `powerLine` 与它的 `<p>`；说明文字重写成"这个标志只说明某些更新要重启才完全生效，判据见「重启标志」卡片"（**测试断言的子串逐字保留**） |
| 14 | 概览页 KPI 的第二行挤在同一行里，长一点就换行成两截 | `UKpi` 只有 `foot` 一个插槽，两样东西往里塞 | 新增 `extra` 插槽，独占一行，`margin-top: 8px` |
| 15 | 页面头部的操作按钮**靠不到右边** | 每个视图各自写 `margin-left: auto`，写漏一个就左对齐 | 加一条统一的 `.win [class$='__head'] > .n-button` 规则，按类名后缀对齐 |

另外两处**不是排版问题，是脚本诚实性**：

* `preview-pages.mjs` 里原先对 `/host/capabilities` 的回放是死路由（那条接口不存在），删掉。
* 补了一条 `/api/v1/health/live`，**在注释里写明它是照 `internal/api/router.go` 的 handler
  抄的，不是抓来的**——抓不到就不编，能抄就标注成抄。

---

## 4. 关于 naive-ui：这一轮踩明白的两条

写在这里是因为下一轮还会碰到，而且**两条都是反直觉的**：

1. **naive 把 `--n-color` / `--n-border` / `--n-height` / `--n-padding` /
   `--n-border-radius` 以 CSS 自定义属性的形式写在元素的 `style` 属性上**（内联）。
   外部样式表里再写同名变量，**只有带 `!important` 才生效**；而带了 `!important`，
   组件靠改同一变量实现的所有变体（文字按钮、链接、默认按钮）**同时被打死**。
   所以：**这些变量不许在外部 CSS 里碰**，它们的正确入口是 `themeOverrides`。
2. 需要在外部 CSS 里提高特异性时，用**双类选择器**（`body .n-x.n-x` = 0-2-1），
   不要动 `!important`。颜色和几何之外的**结构性直接属性**（`border-radius`、
   `padding`、`font-size`、`letter-spacing`）这样写是安全的。

`web/src/styles/naive-skin.css` 头部把这两条写成了注释，按钮那一格**故意空着**，
并注明了原因——避免以后有人在这儿补一条高度规则，把文字按钮又打死一次。

---

## 5. 夹具没覆盖的 9 个页面（重点）

这 9 页在预览里只能看到"读不到"，**它们正常状态的排版这一轮没有被任何人看过**：

`/jobs`、`/backup`、`/alerts`、`/schedules`、`/network`、`/settings/host`、`/terminal`、
`/apps/deployments`、`/accounts`

要补夹具不是抓一张图的事：这几页的接口要么要写动作的预检上下文，要么要真机上的 docker /
netplan / samba / restic 现场。**做法就一个：在 Ubuntu 验收 VM 上真登录、真点。**
按 Q510，那要等下一版重打字节（改文档措辞不重打，`BuildDate` 由 `scripts/release.sh` 注入）。

---

## 6. 已跑过的检查（跑在最终字节上）

| 检查 | 命令 | 读数 |
|---|---|---|
| 类型 | `npx vue-tsc --noEmit` | exit 0，无输出 |
| 单测 | `npx vitest run` | **66 文件 / 1219 用例全过** |
| 语法 | `npx eslint .` | exit 0（**只覆盖 `.ts`/`.mjs`**，`.vue` 被忽略，见下） |
| 构建 | `npm run build` | 成功，9.11 s，`dist/` 只有 `index.html` + `assets` |
| 全量 | `sh scripts/check.sh` | **PASS 15 / FAIL 0 / NOT RUN 0** |

`check.sh` 的 15 条明细：line-endings、sh-syntax、go-build、go-vet、go-test、go-build-linux、
go-vet-linux、gofmt、node-version、web-typecheck、web-lint、web-test、web-build、web-smoke、
openapi-lint。

**别把这串读大**：

* `web-smoke` 走的是**未登录**路径、断言落在登录页，**没覆盖**这一轮改的任何一页。
* `web-lint` 的 eslint 只配了 `.ts`/`.mjs`，**模板里的类型错误抓不到**（`vue-tsc` 补一部分）。
* 15 条里没有任何一条能把"这一页看着对不对"变成红或绿。**版面是看图看的，不是测出来的。**

---

## 7. 未验证 / NOT RUN（与上一节分开记）

* **零个字节跑在 Ubuntu 真机上。** 这 25 张全是宿主 Chromium 回放夹具。
* **只看了 Chromium 156。** Firefox / Safari / Edge 一次没开。`backdrop-filter`、
  `100dvh`、`background-attachment: fixed` 三处最可能在别家不一样，无读数。
* **无障碍没有读数。** 这一轮改了颜色与尺寸，但**没有跑 axe**、没有屏幕阅读器实测、
  对比度没有量。暗色模式下 `--ink-3` 在 `--surface` 上的比值依然是个未知数。
* **没碰过触摸屏。** 400px 只是把窗口缩窄，和真手指是两件事；程序坞的横向滚动没试过惯性。
* **9 个页面（见第 5 节）的正常状态完全没看过。**

---

## 8. 已知问题（不阻塞）

* **`scrollH` 里看不出窗口内部滚动。** 这一轮没有对内容窗口内部的溢出做断言——
  横向溢出只在 `documentElement` 上量。内容窗口如果内部横向溢出，这张表看不出来。
* **预览脚本的夹具是"某一刻"的。** 夹具来自 P04.4 / P09.5 等轮次在真机抓的响应，
  字段集合与现在一致，但**数值是当时的**。哪天字段变了，页面会显示旧字段，不会报错。
* **`design.html` 还在仓库里**（`web/design.html`），没进 `dist`，是留给下一轮调色用的草稿页。
