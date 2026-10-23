# P11.1 真机载荷夹具

这里的每一个文件都是**真的从验收 VM 上抓回来的**，不是手写的。抓取者是
`tests/integration/p11-1-apt-acceptance.sh`（第 2、4—7、9 节里的 `$FIXDIR/*.json`）；
消费者是 `web/src/components/realdata-p11-1.test.ts`。

## 为什么要回放而不是渲染

验收 VM 上没有浏览器，`realdata-*.test.ts` 的跑法又是 `node` 环境（DECISIONS Q21：
没有 jsdom）。所以这份测试**不渲染组件**——SSR 出来的只会是加载中的骨架屏，
断言它等于断言一个空壳。它做的事是：把真机载荷喂给 `web/src/api/packages.ts` 里
那些“照着事实说人话”的纯函数，证明**页面上的一句话是从哪几个字段来的**。

## 这批夹具要证明的四件事

1. **候选读得到**。`packages-bind9.json` 里有 `bind9-libs:amd64`——`dpkg` 的
   `${binary:Package}` 带架构限定，而 `apt-cache policy bind9-libs:amd64` 打出来的
   表头是 `bind9-libs:`（apt 自己把 `:arch` 剥掉了）。按请求名查表必然落空，于是整条
   包被谎报成“候选读不到”。这一条在真机上先错、后修，夹具留下来钉死它。
2. **展示名不再补一次 `:arch`**。`entryDisplayName()` 看到 `name` 已经以 `:amd64`
   结尾就不再拼，否则页面上会出现 `bind9-libs:amd64:amd64`。
3. **`security` / `pockets` 是两个字段**。`packages-security.json` 里那一条
   `zlib1g:amd64`，`suites` 有 `noble-security` 与 `noble-updates`，`pockets` 是
   `["updates","security"]`——页面的“安全更新”标签读 `security`，“来源”读 `pockets`。
4. **`counts` 与 `matched` 不是一回事**。`packages-search.json` 的 `matched=2`，
   而包总数还是 731；筛选不改全量统计。

## 一句提醒：`***` 标的是已安装版本

`apt-cache policy` 输出里的 `***` 指的是**已安装**那一行，不是候选。候选的 suite
只能从版本号等于 `Candidate:` 的那一块里取。夹具里的 `suites` 就是照这个规则算出来的；
如果把 `***` 那块的 suite 当成候选来源，`packages-updates.json` 里
`cloud-init`（`noble-updates`，非安全）就会被误标成安全更新。

## 别手改

改这里的任何一个字节，等于把“手写夹具天然自洽”那类假象请回来。要更新夹具，
重跑验收脚本，再从 VM 上原样取回。

## 清单

抓取时刻 `2026-10-23T10:55:09Z` 起（以载荷自带的 `collected_at` 为准），VM
`upanel-2404`，Ubuntu 24.04.5，apt 2.8.3，dpkg 登记的包 731 个。
那次运行：**PASS=64 / FAIL=0 / NOT RUN=7**（`docs/evidence/P11/2026-10-23-p11.1/logs/2404-p11-1-acceptance.log`）。

| 文件 | 接口 | 内容 |
| --- | --- | --- |
| `capabilities.json` | `GET /internal/v1/capabilities`（代理 socket） | 29 条 query / 30 条 action，含 `apt.packages` 与 `system.power_state`，**没有任何 `package.*` 动作** |
| `packages-page1.json` | `GET /api/v1/packages` | `matched=731`，首页 50 条，`limit=50`，`next_cursor` 有值，`collected_at` / `index_updated_at` 各自独立 |
| `packages-search.json` | `GET /api/v1/packages?query=bash&installed=true&limit=5` | `matched=2`，两条都真的叫 bash* |
| `packages-security.json` | `GET /api/v1/packages?security=true&limit=20` | `matched=1`：`zlib1g:amd64`，`security=true`，`suites` 同时有 updates 与 security |
| `packages-updates.json` | `GET /api/v1/packages/updates` | `total=9`、`security=1`、`truncated=false`、`errors=[]`；9 条逐条带 `installed_version` / `candidate_version` / `pockets` |
| `packages-bind9.json` | `GET /api/v1/packages?query=bind9&limit=20` | 3 条：`bind9-dnsutils`、`bind9-host`、`bind9-libs:amd64`，候选都是 `1:9.18.39-0ubuntu0.24.04.8` |
| `power-state.json` | `GET /api/v1/system/power-state` | `reboot_required=false`（这台机器没有 `/run/reboot-required`）、`boot_id` 与内核一致、`*_error` 都是空 |

## 这些文件里没有秘密

管理员密码在脚本里只经 stdin 走一次，载荷里没有出现；`capabilities.json` 只有版本、
模式、依赖状态，没有 token 或 socket 路径之外的任何东西。

## 回放断言在哪

`web/src/components/realdata-p11-1.test.ts`：字段集合必须是 openapi `PackageEntry` /
`PackageListData` / `PackageUpdatesData` / `PowerStateData` 的子集；`counts` 与筛选、
翻页无关；`matched` 是翻页前、`total` 是全量；候选为 `null` 时 `candidate_relation`
只能是 `unknown`，不许说成“已是最新”；`bind9-libs:amd64` 的候选**不是** `null`；
展示名不重复补 `:arch`；`capabilities` 里没有 `package.*` 动作（只读就是只读）。
