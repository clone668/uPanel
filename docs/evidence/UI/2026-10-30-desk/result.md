# 桌面化重做（三）的看图记录 — 宿主预览 + 真机面板

两批图，**口径不同，别混着读**：

| 目录 | 来源 | 数据 | 能不能当验收 |
|---|---|---|---|
| `../2026-10-30-desk/`（本目录） | 宿主 Chromium + `web/scripts/preview-pages.mjs` 回放夹具 | 夹具（2026-10-21 从 VM 抓的响应），**没覆盖的接口一律 404** | **不能**。夹具截图从来不是验收 |
| `../2026-10-30-desk-live/` | 真 Ubuntu 24.04.5 客机上的 `upanel-web`，宿主机经 SSH 隧道打开 | **真数据**（真的 `/proc`、真的 225+/229 个 systemd 单元、真的磁盘） | 也不是验收，但**比夹具强一个档**：这一批图上没有一个是编的 |

两批都只是**截图**。`docs/STAGES.md` 的 31 条验收矩阵里没有一条覆盖界面，所以这些图既不加分、也不改变任何
「已验 / 部分 / 未验」的判定。它们的唯一用途是回答一句人话：**这一版看着是不是像个桌面**。

---

## 一、本目录（宿主预览，5 张）

| 图 | 路由 | 视口 | 未覆盖接口 | `pageerror` | 横向溢出 | `scrollH` | `h1` |
|---|---|---|---|---|---|---|---|
| `page-desk.png` | `/desk` | 1440×900 | 0 | 0 | 0 | 900 | （桌面无 h1） |
| `page-overview.png` | `/overview` | 1440×900 | 0 | 0 | 0 | 900 | 概览 |
| `page-services.png` | `/services` | 1440×900 | 0 | 0 | 0 | 900 | 服务与进程 |
| `page-desk-mobile.png` | `/desk` | **400×820** | 0 | 0 | 0 | 820 | （桌面无 h1） |
| `page-overview-mobile.png` | `/` | **400×820** | 0 | 0 | 0 | 820 | 概览 |

窄屏两张是补拍的，用来验 `DeskWindow` 的 `max-width: 820px` 分支：窗口**铺满**、标题栏与拖拽/缩放把手
都不出现，图标阵列从 1 列变 2 列（列流），程序坞横向滚动，`scrollH` 恰为 820（**没有把整页撑长**），
横向溢出 0。

环境：仓外 `node-v22.23.3-win-x64`；Playwright 1.64.1 / Chromium 156.0.8078.4（rev 1248）；dpr 1。

复现（Git Bash，`H:\Code\.toolchains\node-v22.23.3-win-x64` 是仓外固定版本）：

```sh
cd web && export PATH="/h/Code/.toolchains/node-v22.23.3-win-x64:$PATH"
npm run dev -- --port 5173 &          # 等 5173 起来
node scripts/preview-pages.mjs http://127.0.0.1:5173 ../docs/evidence/UI/2026-10-30-desk page-desk page-overview page-services page-desk-mobile page-overview-mobile
```

---

## 二、`../2026-10-30-desk-live/`（真机，7 张）

**机器**：QEMU 11.1.0 便携版，Ubuntu 24.04.5 LTS，内核 `6.8.0-147-generic`，4 vCPU（`-accel tcg,thread=multi`），
主机名 `upanel-2404`。装的是本轮**重新打的 amd64 包**：

```
upanel-0.0.0-8e52b36-linux-amd64.tar.gz   44176855 字节
sha256 72997c0f82f0dbecaae064611c82347302ed42151ecacda7a4fe0fb51d46634a
```

客机内 `sha256sum -c SHA256SUMS --ignore-missing` → `OK`（先核字节，再解包，再装）。
装完 `upanelctl version` → `upanelctl 0.0.0-8e52b36 (commit 8e52b36-dirty, built 2026-10-29T16:22:54Z, go1.27.3)`；
`-dirty` 是对的：这批改动没有提交（本轮不许 commit），包里的代码不等于 `8e52b36` 的内容。
`systemctl is-active upanel-web upanel-agent` → `active` / `active`；Web 进程 uid=999（不是 root）；
证书指纹 `40c555cc3d7f2a1bbc119937cc434b10879eb880b059caf2845ad8ffb7f69c19`（至 2029-01-31）。

**装的方式**：`install.sh --source …`，**没加** `--write-config` —— 沿用已有配置（`/etc/upanel/upanel.json`
里 `web.allowed_origins` 是本机上一轮为隧道加的，被保留），代理密钥也没被覆盖。安装器自己报了
「管理员账号: (已存在的账号)」。

**怎么打开的**：面板只听回环 `127.0.0.1:9443`（产品默认，没为看图放宽暴露面），宿主机建
`ssh -N -L 127.0.0.1:8443:127.0.0.1:9443`，浏览器开 `https://127.0.0.1:8443`。
8443 这个源在本轮之前已显式写进 `web.allowed_origins`（产品支持的字段），不是绕过 CSRF。

| 图 | 路由 | 看到了什么 |
|---|---|---|
| `live-after-login.png` | 登录后落地 | **落在 `/desk`**（不是某个功能页）：桌面图标 + 底部应用坞，坞里「全部」高亮 |
| `live-desk.png` | `/desk` | 同一张桌面，直接访问也对 |
| `live-overview.png` | `/`（重定向到窗口） | 窗口有标题栏，内容是真读数：CPU 53.8% / 4 逻辑核（`QEMU TCG CPU`）、内存 16.5%（645.71 MiB / 3.82 GiB）、整盘 4 块共 56 GiB、运行 25 分钟 |
| `live-services.png` | `/services` | 真 systemd：**229 个单元**，运行 59 / 未运行 168 / 失败 0 / 受保护 90 / 不在本机 22。坞里「服务与进程」高亮 |
| `live-storage.png` | `/storage` | 真块设备：`sda` 40 GiB（系统盘，受保护）/ `sdb` 8 GiB / `sdc` 4 GiB / `sdd` 4 GiB + 分区 `sda1`、`sda14`。健康一律「未知」或「不支持检测」并写明原因（**拿不到读数就不说正常**） |
| `live-shares.png` | `/shares` | 能力缺失的诚实态：`Samba 不可用`、`NFS 不可用`，逐条给出缺什么（`/etc/samba/smb.conf` 不存在、`testparm` 没装、`exportfs` 没装、`nfs-server inactive`）以及为什么因此**不许写配置**；0 条共享、0 个受管账号 |
| `live-apps.png` | `/apps` | `0 个应用`、`待确认候选 4`；候选是真的发现结果（`containerd.service` 在跑、主进程 839 监听 `127.0.0.1:40215/tcp`），不是编的 |

**`pageerror` 7 次导航合计 0 条**，登录 200 且 `waitForURL` 在 60 秒内自己离开 `/login`（TCG 下慢，
固定 2.5 秒等待会误判"登录没跳转"，脚本因此改成等 URL）。

复现：

```sh
# 宿主机
cd web && export PATH="/h/Code/.toolchains/node-v22.23.3-win-x64:$PATH"
ssh -N -L 127.0.0.1:8443:127.0.0.1:9443 -p 2222 upanel@127.0.0.1 &
node scripts/shoot-live.mjs https://127.0.0.1:8443 ../docs/evidence/UI/2026-10-30-desk-live admin <密码>
```

---

## 三、这批图**不能**说明什么

* **不是验收。** 没有任何断言。31 条矩阵一条都没动，`sh scripts/check.sh` 的 15/0/0 里也没有一条能判
  "这一页看着对不对"（`web-smoke` 走未登录路径，落在登录页）。
* **只看了一个浏览器**：Chromium 156。Firefox / Safari / Edge 没开。
* **窄屏只在宿主夹具上看过两张**（`/desk`、`/`，400×820）：真实数据下的窄屏、以及窄屏上的九页都没看过。
* **没有无障碍读数**：没跑 axe、没量对比度、没做屏幕阅读器实测。
* **没有触摸/拖动实测**：窗口拖动与缩放是鼠标事件，键盘可达性（焦点环、Tab 顺序）没试过。
  桌面图标那一格有个反例：截图里「设置」图标带一圈焦点环，是 Playwright 点击后 `:focus-visible` 留下的，
  **不是设计缺陷**。
* **窗口内滚动没量**：本轮只量 `documentElement` 的横向溢出，窗口 `.win__body` 内部的横向溢出没有断言。
* **9 个页面在真机上"正常状态"仍然没看全**：真机上 `/jobs`、`/backup`、`/alerts`、`/schedules`、
  `/network`、`/settings/host`、`/terminal`、`/apps/deployments`、`/accounts` 这次点进去了，但这台机器
  没有任务、没有备份仓库、没有计划任务、没有网络改动，所以看到的多是空态——**空态对，非空态未知**。

## 四、NOT RUN

* Firefox / Safari / Edge
* 真机（非夹具数据）上的窄屏
* axe / 对比度测量
* 键盘与触摸操作
* 窗口内横向溢出断言
* 任何基于断言的界面验收
