# P04.4 验收结果（2026-10-21）

**范围**：首页（概览）、磁盘列表、拓扑/分区详情、健康与容量卡片；**写操作能力此时不启用**
（`capabilities` 恒为空数组，界面明说“当前阶段没有可执行的动作”，不摆点不动的按钮）。
依据：`docs/STAGES.md` §6 P04.4、`docs/CONTRACTS.md` §6/§6.2、`docs/PLAN.md` §7/§8、
`api/openapi.yaml`（`info.version: 0.0.0-p04.4`，`x-upanel-contract-status.stage: P04.4`）。

**结论：passed（宿主 + 一台验收 VM）。** 没有写操作、没有新数据库迁移、
生产动作登记表在 P07 之前仍是空的。

## 做了什么

| 层 | 文件 | 内容 |
|---|---|---|
| 前端页面 | `web/src/views/OverviewView.vue`、`StorageView.vue`、`StorageDeviceView.vue`（新） | 概览页（主机/CPU/内存/负载/网络 + 磁盘概要）、存储页（整盘与分区、loop 背景区、网络挂载）、设备详情页；每页都有加载 / 空 / 错误 / 依赖未安装四种状态 |
| 前端组件 | `web/src/components/{StatTile,CapacityBar,HealthBadge,IoStats,DeviceCard,DeviceDetailPanel,SystemOverviewPanel}.vue`（新） | 全部纯展示、接收 DTO，不自己造数据；字节走十进制字符串 + BigInt，`null` 一律渲染成 `—` |
| 前端 DTO | `web/src/api/system.ts`、`storage.ts`、`utils/format.ts`（新） | 与 `api/openapi.yaml` 同源的 DTO 与封闭集合标签；`healthNeedsAttention()` 只对 warning/critical 为真 |
| 路由 | `web/src/router/routes.ts`、`modules/registry.ts`、`components/AppShell.vue` | `/storage`、`/storage/devices/:id`（id 字符集 `[A-Za-z0-9._-]+`，非法值直接落 404）；子页面高亮所属模块 |
| 契约 | `api/openapi.yaml` | `StorageDevice.io` 进 `required`（P04.3 就已返回、契约漏登记），health 说明改为“由 SMART/NVMe 填充”，补齐四个已实现路径 |
| 测试 | `web/src/{api,utils,components}/*.test.ts`（新） | 16 个文件 / 192 个用例；SSR（`renderToString`）渲染纯组件 + DTO 校验 + 真实数据回归 |
| 真实数据夹具 | `web/src/fixtures/p04-4-vm/*.json`、`README.md`（新） | VM 上抓的四份真实响应，逐字节保存；`realdata.test.ts` 用它渲染 |
| 页面冒烟 | `web/scripts/smoke.mjs`（重写）、`tests/smoke/p04-4-ui-smoke.sh`（新） | 真浏览器（Playwright）：未登录拦截、登录后回到原地址、逐页断言、错误码与空状态；两种模式（dist 预览 / 真 `upanel-web --mode mock`） |
| VM 验收 | `tests/integration/p04-4-ui-acceptance.sh`、`p04-4-payload-validate.py`（新） | 真 Ubuntu 上启动 root 代理 + 非 root Web，抓四份响应，按契约 + lsblk/df/smartctl/`/proc/diskstats` 交叉校验 |
| 闸门 | `scripts/check.sh` | 新增 `web-smoke`（没有 Playwright 浏览器时记 SKIP 并给出安装命令，不假装通过） |

## 实际执行的检查

| 检查 | 结果 | 证据 |
|---|---|---|
| VM 载荷校验（契约 + lsblk/df/smartctl/diskstats） | **PASS=856 FAIL=0** | `p04-4-acceptance-2404.txt` |
| VM 验收脚本自身断言（身份、socket、抓取、遍历式 id） | **PASS=31 FAIL=0 NOT RUN=0** | 同上 |
| 宿主 `sh scripts/check.sh` | **PASS 13 / FAIL 0 / NOT RUN 0** | `check.txt` |
| 宿主 `go test ./...`（含在 check.sh 内） | PASS | `check.txt` |
| 前端 `vue-tsc -b` / `eslint` / `vitest run` | 0 error / 0 problem / **192 passed（16 文件）** | `web-gates.txt`、`check.txt` |
| 页面冒烟：真实 `upanel-web --mode mock` + Playwright（已登录） | **PASS 31 / FAIL 0 / SKIP 5** | `p04-4-ui-smoke.txt` |
| 页面冒烟：dist 预览（未登录守卫） | **PASS 10 / FAIL 0 / SKIP 0** | `check.txt` 的 `web-smoke` 段 |
| 契约 lint（redocly） | valid，9 warning（未使用组件 7 + 4xx 说明 2，历史遗留） | `check.txt` |

VM 侧的关键断言（`p04-4-acceptance-2404.txt` 内可逐条对照）：

- 监听 19448 的进程是 `upanel-web`（uid 999），代理是 root；`agent.sock` 是 `root:upanel-web`、非 0666；
- 四份响应结构与闭集一致，`io` 已在 `required` 内，字节全是十进制字符串，未知值一律 `null`；
- `lsblk -P` 的 17 台块设备与 DTO 一一对应，`sda/sdb/sdc/sdd` 的字节数与 `lsblk -b` 相同；
- `/`、`/boot`、`/boot/efi` 的容量与 `df -B1` 相同；
- 16 台设备的 `io.reads_total` 都不超过 `/proc/diskstats` 的对应行（每台只报自己那一行，不相加）；
- 4 块盘的 `health` 与 `smartctl -j -i -H -A` 的结论一致：`smart_support.available=false` → `unsupported`，
  且 `checked_at` 非空；
- `/storage/devices/..%2F..%2Fetc%2Fshadow` 返回 404，响应体里没有 `root:`；
- `web.log` / `agent.log` 里没有管理员明文密码。

## 与 STAGES §6 P04.4 验收条目逐条对应

| 验收条目 | 证据 |
|---|---|
| 未挂载盘可见 | 真实数据里 `sda` 没有挂载点，卡片写“未挂载”且不借用分区容量（`realdata.test.ts`）；合成用例 `未挂载的盘也照实显示，不编造容量` |
| 两块同容量磁盘不混淆 | 真实数据里 `sdc` 与 `sdd` 都是 4 GiB、`serial` 都是 `null`，两张卡片的名字、链接、稳定标识各不相同（`realdata.test.ts`）；合成用例 `两块容量完全相同的盘不会互相顶替` |
| 多层 dm/LVM/RAID 拓扑不重复累计 | 合成用例 `dm/LVM 盘与它下面的分区各自只显示自己的容量，从不相加`（两层各 931.51 GiB，界面里不能出现 1.82 TiB）；VM 上没有 dm/LVM/RAID，真实拓扑这一条见下方 NOT RUN |
| 根分区底层整盘被保护 | 真实数据：`sda` 与 `sda1/sda15/sda16` 都带“系统盘（受保护）”，详情页列出理由（`承载根文件系统`、`承载 /boot（启动所需）`）与保护来源 |
| SMART 缺失显示未知/不支持 | 真实 VM：4 块盘 `smart_support.available=false` → `unsupported` + 原话；合成用例：`unknown` 显示“未知”、温度 `—`、绝不出现“正常” |
| 计数器归零不产生负速率 | 后端规则与用例在 P04.3（`internal/storage/diskstats_test.go`）；前端只渲染 `rate`，`null` 时写“没有可用采样窗口，不是 0” |
| 采集失败不会卡住登录 | 页面冒烟（真实 `upanel-web --mode mock`，代理未配置）：登录成功、逐页显示依赖未安装 + `AGENT_UNAVAILABLE`，没有停在“加载中” |

## 本轮发现并修掉的问题

1. **验收脚本抓得太早**（harness 缺陷，不是产品缺陷）：健康的第一次探测在独立 goroutine 里，
   脚本启动 Web 后立刻抓响应，拿到的是 `health.details = "SMART 采集尚未完成"` 的占位
   （`status=unknown`、`checked_at=null`）。“健康字段对不对”这一整块因此变成了空转，
   而且旧日志还把 SKIP 的原因写成“本机没有能被 smartctl 读到的盘”——那不是真实原因。
   现在脚本先等所有整盘都有 `checked_at`（`HEALTH_WAIT`，默认 120 秒，实测 5 秒），
   超时则如实记 `NOT RUN`。
2. **健康对照两边参数不一致**（harness 缺陷）：旧的校验程序用 `smartctl -H -j` 比对，
   而产品用的是 `-j -i -H -A -n standby`。同一块 QEMU 盘，前者给
   `smart_status.passed=true`，后者给 `smart_support.available=false`、没有 `smart_status`。
   按旧写法一旦把 virtio 盘纳入对照，就会把“产品正确判为不支持”误报成失败。
   现在校验程序用产品同一组参数，并把 `available=false → unsupported` 这条也变成断言
   （4 块盘全部 PASS）。
3. **界面把“未知/不支持”说成“健康未通过”**（产品缺陷）：`DeviceCard` 原先对
   `unknown` 与 `unsupported` 都挂 `健康未通过` 标签，于是每一个分区、每一个 loop 设备
   都在报警。改为只对 warning/critical 挂“健康需要处理”，并补了用例。

## 未验证（NOT RUN，含原因）

- **VM 上的浏览器渲染**：验收 VM 没有安装浏览器，像素级证据只有宿主上的 Playwright 冒烟
  （真实 `upanel-web --mode mock`，已登录，31 项）。VM 上只证明“前端拿到的数据是真的”。
- **真机 SATA / NVMe / USB / 睡眠盘的健康显示**：这台 VM 的盘全是 QEMU 虚拟盘，
  `smartctl` 说它们不支持 SMART，所以真实数据里只有 `unsupported` 这一条分支；
  `healthy` 与 `warning` 的显示由合成夹具（`storage.test.ts`）覆盖，
  真实 `healthy` 的采集证据在 P04.3 的 `upanel-smart` 验收里（不是这一阶段的范围）。
- **真实 LVM/RAID/dm 拓扑**：验收 VM 是单盘 + 三个空盘，没有 dm/md 设备，
  所以“多层拓扑不重复累计”只有合成用例（P04.2/P04.3 的 VM-C 记录同样是 NOT RUN）。
- **写操作**：按计划不启用，界面上没有按钮，`capabilities` 恒为空 —— 这是本阶段的验收条件，
  不是未完成项。
- **`viewer` 角色看这些页面**：本轮验收只用 `admin` 登录；读取类接口对两种角色都开放，
  没有单独的“无权限”界面可验。

## 环境事故（不记作产品缺陷）

`upanel-2404` 在 uptime 3096 s 时再次 `Kernel panic - not syncing: Attempted to kill init! exitcode=0x0000008b`
（PID 1 自己吃 SIGSEGV，与 P04.3 记录的形态一致），`upanel-smart` 在 3200 s 时同样。
冷启动后立即恢复正常，本轮的验收数据是在冷启动后的干净实例上采集的。
console 存为 `console.panic-5-2026-10-21.log`。

## 这份证据怎么复核

```sh
# 宿主
sh scripts/check.sh
sh tests/smoke/p04-4-ui-smoke.sh        # 需要 Playwright chromium

# VM（root，二进制先交叉编译后拷进去）
sudo BIN_DIR=/tmp/upanel-bin sh tests/integration/p04-4-ui-acceptance.sh
```

载荷目录 `/var/tmp/upanel-accept-p04-4/payloads/` 下的文件就是本目录 `payloads/` 的来源；
其中 `summary.json`、`devices.json`、`topology.json`、`detail-sda.json` 同时是
`web/src/fixtures/p04-4-vm/` 里的前端回归夹具（逐字节相同）。
