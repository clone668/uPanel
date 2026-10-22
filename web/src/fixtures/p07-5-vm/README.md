# p07-5-vm —— 磁盘页面真正要读的那几份载荷

这些 JSON 不是造的。它们由 `tests/integration/p07-5-storage-page-acceptance.sh` 在
Ubuntu 24.04 验收 VM（upanel-2404，sdb/sdc 两块一次性盘）上跑完一整个生命周期后**原样抓回来**：

    GPT → 建分区 → mkfs ext4 → 纳入管理 → 改配置（开自动挂载）→ 挂载 → 写入文件
    → 占用扫描 → 卸载 → 重挂载 → 退出管理

抓的是页面与任务链真正读到的东西，`{data, request_id}` 外层保持服务端原样，没有裁剪。

## 为什么要这一层

P07.5 要求“磁盘页面完整交互”，而页面画得对不对，取决于服务端**实际发出去**的载荷。
手写夹具天然自洽：`observed_state` 与 `capacity` 一致、`capabilities` 齐全、未挂载的分区
带着 `type: "ext4"`。正是这份自洽让缺字段、空值语义、越界状态永远测不出来——
P07.5 第一轮真机跑就撞出一个：`importCandidates` 要求“未挂载分区也要有文件系统类型”，
而真机上是 `null`（CONTRACTS 第 231 行），于是导入表单在真实机器上永远是空的（DECISIONS Q208）。
前端单测全绿，因为夹具比现实干净。

所以这些文件只有一个用途：**钉住“这份真实载荷被画成了什么”**。断言写的是不变量
（字段形状、闭集成员、空值语义、两源一致），不是某一次运行的字节数、pid 或 UUID。

## 文件

| 文件 | 来自哪个请求 | 钉住的点 |
|---|---|---|
| `page-topology.json` | `GET /api/v1/storage/topology`（页面刚进来时，两块数据盘还是空的） | 拓扑里每台设备的身份、保护原因；未挂载分区 `filesystem` 为 `null` 或 `type` 为 `null`——候选判据就是在这里被喂坏的 |
| `page-device-disk.json` | `GET /api/v1/storage/devices/sdb`（刚建完分区） | 四个动作判定的三态与理由；`disk_entries` 与内核视图两源对应 |
| `page-device-partition.json` | `GET /api/v1/storage/devices/sdb1`（已格式化、**未挂载**） | `filesystem.type` 为 `null`——不是猜的，是契约（CONTRACTS 第 6.1 节，第 231 行）；UUID 也只在 by-uuid 链接存在时才读得到 |
| `page-device-partition-mounted.json` | 同一个分区，挂载之后 | 挂上就能读到 `ext4` 与挂载点：上一行那个 `null` 不是实现偷懒 |
| `page-volumes-empty.json` | `GET /api/v1/storage/volumes`（还没有卷） | 空列表是 `[]`，不是 `null`；页面据此说“还没有受管的卷” |
| `page-volumes-one-mounted.json` | 同上，一个卷挂着 | 列表里 `capacity` 只在该卷真的挂着时非空 |
| `page-volume-mounted.json` | `GET /api/v1/storage/volumes/{id}` | `observed_state` 与 `desired_state` 是两件事；`capacity` 的六个字段都是十进制字符串 |
| `page-volume-unmounted.json` | 同上，卸载之后 | `capacity` 变 `null`（不是 `0`），`observed_state` 跟着现场变 |
| `page-blockers-busy.json` | `GET /api/v1/storage/volumes/{id}/blockers`（有人开着卷里的文件） | `busy=true` 且 `processes[].pid` 是真的——脚本到 `/proc/<pid>/fd` 逐个核对过 |
| `page-blockers-idle.json` | 同上，占用者退出后 | `busy=false`；“挂着但空闲”不算忙（DECISIONS Q192） |
| `agent-capabilities.json` | `GET /internal/v1/capabilities` | 九个存储写动作全部登记；写动作不出现在 `supported_queries` 里 |
| `preflight-*.json` / `submit-*.json` / `job-*.json` | `/operations/preflight`、`/jobs` | 拒绝的理由、确认短语、再认证、锁、`checked_via` 两源核验 |

`external-check-real.txt` 与 `*.expect-wrong.txt` 不是页面载荷，是 `scripts/storage-external-check.sh`
对着真实 `/etc/fstab` 与注入坏的副本跑出来的输出，留在
`docs/evidence/P07/2026-10-22-p07.5/payloads/` 里。

## 怎么用

`web/src/components/realdata-p07-5.test.ts` 读这些文件，把 `data` 交给真实组件做 SSR 渲染，
再断言渲染结果。新增断言时**别把具体 UUID、pid、字节数写死**——那是某一次运行的样子，
不是产品的不变量。
