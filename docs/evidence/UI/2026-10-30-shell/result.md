# 界面重做（桌面式外壳 + 设计系统）— 证据

日期：2026-10-30
类型：**宿主（开发机）+ 预览夹具**，`docs/EVIDENCE_INDEX.md` 里的「宿主」档。
覆盖：`web/` 前端外壳、令牌与基础组件、概览页版式。

## 这份证据能证明什么，不能证明什么

能：
1. 前端工程自洽——单测 1219 条全过、类型检查 0 错、lint 0 错、生产构建成功且 `dist/` 只含
   `index.html` + `assets`（设计与预览入口不进生产包）。
2. 五个版面在 Chromium 里真渲染出来了，横向不溢出（每张都记了 `scrollWidth == clientWidth`）。

不能：
1. **不是 Ubuntu 真机验收**。没有一台装了 `upanel-web` + `upanel-agent` 的 Ubuntu 在跑这套界面。
2. **页面里的数据不是真机读数**。`/api/v1/**` 的响应由 `web/scripts/preview-shell.mjs` 拦截并
   回放 `web/src/fixtures/p04-4-vm/` 里 2026-10-21 抓下来的真实 VM 报文（`request_id` 是抓取时那次的），
   未覆盖的接口一律返回 404，所以「常用入口 / 面板服务」两张卡显示的是**失败态**——那正是有意的：
   看一眼失败态长什么样，而不是给它编一个成功响应。
3. 深色与浅色只在这台机器的 Chromium（`prefers-color-scheme`）下看过，没在真机上、没在
   Safari/Firefox 上看过。
4. 触摸、读屏、键盘焦点只在代码层面处理过，**没有做无障碍实测**。

## 文件

| 文件 | 内容 |
|---|---|
| `facts-host-checks.txt` | 钉住工具链（node v22.23.3 / npm 10.9.9）上跑的单测、类型检查、lint、构建与 `dist/` 清单 |
| `shell-overview.png` | 概览：顶部面板条 + 四格指标 + 左「系统概况」右窄栏，1440×900 |
| `shell-launcher.png` | 应用抽屉（全部已接入模块 + 一句话说明），1440×900 |
| `shell-dark.png` | 同上版面的夜间主题（点两次主题按钮走到「夜间」，不从外部硬写 `data-theme`） |
| `shell-storage.png` | 存储页 + 应用坞当前项标记，1440×900 |
| `shell-mobile.png` | 400×820：指标改两列、应用坞横向滚动、不出现横向溢出 |

## 复现命令

```bash
cd web
node scripts/preview-shell.mjs http://127.0.0.1:5173 /tmp/shots   # 需要先 npm run dev
```

脚本会打印每张图的 `{errors, info:{scrollW, clientW}}`；本轮五张都是 0 条 `pageerror`、
`scrollW == clientW`。控制台里的 `404` 全部来自脚本自己给未覆盖接口回的那条
`{"error":{"code":"NOT_FOUND","message":"预览夹具未覆盖该接口"}}`，不是页面缺陷。

## 仓库宿主门禁（`sh scripts/check.sh`）

改完之后整跑一遍，与上一轮同一个脚本、同一个钉住的 node：**PASS 15 / FAIL 0 / NOT RUN 0**
（读数在 `facts-check.txt`，两次独立跑同值）。

要读准它证明到哪：

* `web-typecheck` / `web-lint` / `web-test`（1219 条）/ `web-build` 覆盖的是**新外壳的代码本身**。
* `web-smoke` 会真起一个 `dist` 预览、用 Playwright 打开——但走的是**未登录**路径，
  断言落在登录页（语言、标题、`#app` 有内容、品牌副标题、无页面异常、无多余失败请求）。
  也就是说它**没有覆盖新的顶栏 / 程序坞 / 启动台 / 概览页**，那部分只有本目录的夹具截图。
  "冒烟过了"不等于"外壳验过了"。
* `go-*` 与 `openapi-lint` 全绿只说明**后端一个字节没被碰坏**（这轮没动 `cmd/`、`internal/`、
  `api/openapi.yaml`），不是界面的读数。

仍然**不是 Ubuntu 真机验收**；这一条不改。
