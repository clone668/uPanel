# P16.1 宿主读数（2026-10-30）

**这份文件只证明宿主上跑过什么。它不证明 Ubuntu 上能用。**
P16.1 的三块真实交付（能力探测的三种问法、备份脚本、安装/升级/卸载接线）在**真实 Ubuntu 上一次
都没有运行过**，见本文件第 4 节。

采集宿主：Windows（Git Bash），`H:\Code\uPanel` 工作树。
宿主时钟读数 `2026-10-27T22:12`（账本用 2026-10-30 记本轮，两者差的是项目日期与宿主时钟，不是两次运行）。

工具链（`go version` / `node -v` / `npm -v`）：

```
go version go1.27.3 windows/amd64
v22.23.3
10.9.9
```

## 1. 统一检查（`sh scripts/check.sh`，最终工作树）

```
=========================== 汇总 ===========================
PASS 15 / FAIL 0 / NOT RUN 0
明细: line-endings:PASS sh-syntax:PASS go-build:PASS go-vet:PASS go-test:PASS
go-build-linux:PASS go-vet-linux:PASS gofmt:PASS node-version:PASS web-typecheck:PASS
web-lint:PASS web-test:PASS web-build:PASS web-smoke:PASS openapi-lint:PASS
```

各关关键读数（同一份日志）：

- `web-test`：`Test Files 62 passed (62)` / `Tests 1175 passed (1175)`
- `web-smoke`：`PASS 10 / FAIL 0 / SKIP 0（共 10 项，production，目标 dist 预览，未登录）`
- `openapi-lint`：`Your API description is valid.` / `You have 6 warnings.`（6 个都是本轮之前就有的）
- `go-test`：`go test ./...` 全部 ok（无 FAIL 行）

`sh-syntax` 是这一轮新增的一关。因为 `backup-state.sh` 的 `.incomplete` 改动是在全量 check **启动
之后**落地的，收尾在最终工作树上把这一关单跑了一遍：

```
解析器: /usr/bin/sh
脚本数=77 失败=[]
```

（名单取 `git ls-files --cached --others --exclude-standard -- 'scripts/*.sh'
'deploy/scripts/*.sh' 'tests/integration/*.sh'`，与 `check_sh_syntax` 同一份。）

**这个读数有天花板**：宿主上的 `sh` 是 bash，`sh -n` 抓不到 bashism（`[[ ]]`、数组、`local -n`、
`echo -e` 的差异）。真机上 `#!/bin/sh` 是 dash，同一段代码可能直接报错。`check.sh` 的输出里已经
打印了这句提醒。

## 2. 构建与打包（真跑，不是 `sh -n`）

```
$ sh scripts/build.sh
嵌入的前端入口: internal/webui/dist/index.html (1535 字节)
$ ls -l bin/upanel-{web,agent} bin/upanelctl
-rwxr-xr-x 27645952 22:11:49 bin/upanel-agent
-rwxr-xr-x 35651584 22:11:46 bin/upanel-web
-rwxr-xr-x 18161152 22:11:50 bin/upanelctl

$ VERSION=0.1.0-dev.1 ARCHES=amd64 sh scripts/release.sh
==> 打包 upanel-0.1.0-dev.1-linux-amd64
    交叉编译（GOOS=linux GOARCH=amd64 CGO_ENABLED=0）
    自检: 包内可执行文件均带执行位
    完成: dist/upanel-0.1.0-dev.1-linux-amd64.tar.gz (44135630 字节)
```

发布包 sha256（`dist/SHA256SUMS` 的内容）：

```
17ff651843e0a7fa999a1939f56c7ad1e7c2b3234087c609823051b9758820dd  upanel-0.1.0-dev.1-linux-amd64.tar.gz
```

包内脚本与执行位（`tar tzvf` 的实际输出，不是推断）：

```
-rwxr-xr-x 34404 deploy/scripts/install.sh
-rwxr-xr-x 10943 deploy/scripts/uninstall.sh
-rwxr-xr-x 12345 deploy/scripts/backup-state.sh
```

这条读数覆盖了两件事：`release.sh` 真的把 `backup-state.sh` 打进去了，执行位在包内是 `0755`
（`--mode=0755` 生效）；`install.sh` 的字节数对应 13 步那版。**它不覆盖**"安装脚本在 Ubuntu 上
能不能跑通"——那要 P16.2。

## 3. 单元测试与类型检查（分层，逐条可查）

- `go test ./cmd/upanelctl/ ./internal/hostcaps/ ./internal/api/` 全绿
  （`internal/api` 单包 159.97s）。
- `deploy/scripts/backup-state.sh` 一类的 shell 逻辑**没有测试**——只有 `sh -n`。这是这一轮最大的
  证据缺口，第 4 节逐条列出。
- 前端：`npx vitest run` 全量 62 文件 / 1175 用例；`vue-tsc --noEmit` 干净；`vite build` 成功。
  本轮新增 `web/src/api/hostcaps.test.ts` **13** 个用例。
- CLI：`cmd/upanelctl/hostcaps_test.go` **6** 个用例（`--json` 的 `DisallowUnknownFields`、`--line`
  的六列与"禁占位符"、`--json --line` 互斥、`--timeout 0` 拒绝、无表头用"第一行第一列等于
  `hostcaps.ItemIDs()[0]`"断言）。
- 接口：`GET /api/v1/system/capabilities` 的 admin 判据、`host_caps: null` 的透传、代理不可用时的
  503，都在 `internal/api` 的用例里。

## 4. NOT RUN（未执行，不得折算成通过）

| 项 | 状态 | 为什么 |
|---|---|---|
| 三个部署脚本的**真实运行**（install / uninstall / backup-state） | **NOT RUN** | 宿主是 Windows，一次都跑不起来；只有 `sh -n` 解析读数 |
| `backup-state.sh` 的备份、恢复、可重入三条路径 | **NOT RUN** | 停/起服务、`-wal`/`-shm` 是否跟着走、`MANIFEST.sha256` 能否 `-c` 过、`.incomplete` 在真实中断下的行为，全部未验 |
| `install.sh` 第 9 步（升级前自动备份） | **NOT RUN** | 同上 |
| `install.sh` 第 3 步在**已装过东西的旧机器**上是否放行 | **NOT RUN** | "兼容已有软件"那半句要求的正是这个；`unknown` 分支会不会误伤，只有单测里的假文件系统走过 |
| `uninstall.sh` 的 `--purge-*` 默认备份 | **NOT RUN** | 同上 |
| 能力探测页（`/settings/host`）的真机读数 | **NOT RUN** | 页面没被人点过，没有 Playwright 用例；`web-smoke` 只走"未登录落到登录页" |
| 24.04 / 22.04 的差异 | **NOT RUN** | `mkfs.xfs` 在 24.04 默认不装、`samba` 的二进制命名、`ufw` 的包名——都只在单测的假文件系统里走过 |
| `host-caps --line` 在真机上的输出 | **NOT RUN** | 宿主上跑的是 Windows 二进制，探的是 Windows 的目录，读数对 Ubuntu 没有意义 |

## 5. 下一轮必须补的

以上 8 条里有 7 条落在 **P16.2（22.04/24.04 双平台端到端验收）** 的范围里：全新安装 → 重启 →
升级 → 失败升级恢复 → 默认保留数据的卸载。`sh -n` 不是这些事的证据，P16.2 的账本里不得引用它。
