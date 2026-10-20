# P01.3 结果：前端骨架（Vue + 路由 + Naive UI + 统一 API 客户端）

日期：2026-10-20
范围：STAGES.md P01.3 —— Vue 页面骨架、路由、Naive UI、中文导航、统一 API 客户端、错误组件、mock 横幅、未实现模块隐藏。

## 已执行并通过

| 检查 | 命令 | 结果 | 证据 |
| --- | --- | --- | --- |
| 类型检查 | `npm --prefix web run typecheck` | exit 0，无输出 | typecheck.txt |
| Lint | `npm --prefix web run lint` | exit 0，无输出 | lint.txt |
| 单元测试 | `npm --prefix web run test` | 6 个文件 / 50 个用例全部通过 | test.txt |
| 生产构建 | `npm --prefix web run build` | exit 0，2697 模块，dist 1.28 kB HTML + 413.53 kB JS | build.txt |
| 页面冒烟（production） | `npm --prefix web run smoke` | 16 / 16 | smoke-production.txt |
| 页面冒烟（mock） | `npm --prefix web run smoke:mock` | 17 / 17 | smoke-mock.txt |
| 统一检查 | `sh scripts/check.sh` | PASS 12 / FAIL 0 / NOT RUN 0 | check-run.txt |

`sh scripts/check.sh` 明细：go-build、go-vet、go-test、go-build-linux、go-vet-linux、gofmt、node-version、web-typecheck、web-lint、web-test、web-build、openapi-lint 全部 PASS。这是 P01 三个子任务以来第一次 0 项 NOT RUN。

## 冒烟覆盖的行为（真实 Chromium，非 mock 断言）

- 构建产物可加载：HTTP 200、`lang="zh-CN"`、页面标题 `概览 · uPanel`、`#app` 有内容。
- 导航只渲染已实现模块：菜单文本只有“概览”，不含存储 / 终端 / 设置。
- production 产物无“演示模式”横幅；mock 产物始终显示横幅，切换路由后仍在（不可忽略）。
- 预览环境没有后端，`/api/v1/health/live` 必然失败：页面显示错误码 `INVALID_RESPONSE` 与契约说明，不停留在“加载中”，也不把 404 HTML 当作成功数据。
- 未实现模块路径 `/storage` 落到 404 页面，且 404 页面仍渲染完整外壳。
- 除这一次预期的 API 探测外，没有其他失败请求；无未捕获异常；无其他控制台错误。

## 未执行（不计为通过）

| 项 | 原因 |
| --- | --- |
| 同源 API + 静态页面在 Go Web 二进制中的加载 | 属 P01.4（前端构建产物嵌入二进制） |
| Ubuntu 虚拟机运行验收 | P01.3 无系统写操作、无 Linux 依赖；P01.4 起才需要 VM 证据 |
| mock 服务绑定 127.0.0.1:18881 的验收 | mock 服务端尚未实现（P01.4 起） |
| 浏览器交互用例（点击、键盘、焦点） | 仓库未落地 e2e 用例集；本轮只有 HTTP + 渲染冒烟 |
| `.vue` 文件的 ESLint 检查 | 已冻结依赖中没有 eslint-plugin-vue；SFC 由 vue-tsc + vitest 覆盖（DECISIONS Q21） |

## 证据说明

- 首次运行冒烟时 Playwright 报告 `chromium_headless_shell-1248` 缺失（本机只有 1223），执行 `npx playwright install chromium` 下载匹配 1.64.1 的浏览器后通过；这不是产品缺陷。
- 构建产物 `dist/`、`dist-mock/` 均为本机生成物，不入库（.gitignore）。
- 本轮未修改 Go 代码，Go 侧四项检查为回归证据，不是本轮变更的证明。
