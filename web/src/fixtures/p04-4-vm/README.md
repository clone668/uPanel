# p04-4-vm —— 真实 Ubuntu 上的四份响应（原样保存）

这里的 `*.json` **不是造的**，是 P04.4 在独立验收虚拟机上跑真实 `upanel-web`
（production 模式，Web 非 root，代理 root）后抓下来的 HTTP 响应体，一个字节都没改。

保存它们的原因：`src/components/*.test.ts` 里手写的夹具容易“编得比现实干净”——
不会出现读不到序列号的虚拟盘、不会出现 `smart_support.available=false`、
不会出现只用掉 6.4% 的 `/`。真实数据是唯一能钉住“界面有没有把没读到说成 0、
把未知说成正常”的证据。

## 怎么采的

来源：Ubuntu 24.04.5 LTS 验收 VM，主机名 `upanel-2404`，内核 `6.8.0-147-generic`
（见 `summary.json`）。四块盘都是 QEMU 的虚拟盘（`QEMU HARDDISK`），
`smartctl -j -i -H -A` 对它们报 `smart_support.available=false`，
所以 `health.status` 是 `unsupported` —— 这不是缺陷，是这台 VM 的真实情况。

采集命令（VM 内，root）：

```sh
sudo BIN_DIR=/tmp/upanel-bin sh tests/integration/p04-4-ui-acceptance.sh
```

脚本把四份响应写到 `/var/tmp/upanel-accept-p04-4/payloads/`，然后从宿主拷到这里：

| 文件 | 接口 | 说明 |
| --- | --- | --- |
| `summary.json` | `GET /api/v1/system/summary` | 主机、CPU、内存、负载、网卡 |
| `topology.json` | `GET /api/v1/storage/topology` | 17 个节点、4 条边、网络挂载（空） |
| `devices.json` | `GET /api/v1/storage/devices` | 同一份快照的设备列表（含 health 与 io） |
| `detail-sda.json` | `GET /api/v1/storage/devices/sda` | 整盘详情（分区表、空闲区、保护来源） |

证据目录里还有 `detail-sdb|sdc|sdd.json`（空盘）与 `traversal.json`
（路径穿越式 id 被拒的 404）——前端不需要它们，所以没放进夹具。

四份文件来自**同一份快照**：`snapshot_version` 都是
`sha256:fd6bdf50dfd20f25b34b50df6539714fc1e20ad00f8c5c90dc158a15a97da4fa`，
`collected_at` 都是 `2026-10-21T00:19:31Z`（VM 时钟）。`realdata.test.ts` 会校验这一点，
免得以后有人只换其中一份。

这四份文件与 `docs/evidence/P04/2026-10-21-p04.4/payloads/` 下的同名文件**逐字节相同**，
那份证据目录里的 `p04-4-acceptance-2404.txt` 就是对着这批响应跑出
`PASS=856 FAIL=0`（载荷）/ `PASS=31 FAIL=0 NOT RUN=0`（脚本）的那一次。

两件值得留意的旁证：

- 同一份数据抓过三次（冷启动、间隔几分钟），`/` 的使用率在 6.4%—6.6% 之间变、
  `io.*_total` 全在涨、CPU 使用率每次不同，但 `snapshot_version` **没变** ——
  说明 io 计数、容量与采集时间确实没进快照版本，只有身份与结构进（CONTRACTS.md 第 6 节）。
- **只用 `smartctl -H` 会得出相反结论**：同一块盘 `smartctl -H -j /dev/sda` 返回
  `smart_status.passed=true`，而带上产品自己那组参数（`-i -H -A`）时是
  `smart_support.available=false`、没有 `smart_status`。所以健康对照必须两边用同一组参数，
  否则“产品报 unsupported”会被误判成“产品漏读了一块好盘”。

## 格式

和线上完全一样：包着 `{ "data": …, "request_id": "…" }` 外壳，未格式化、未删字段。
读取时按前端 `client.ts` 的方式只取 `data`（见 `realdata.test.ts` 里的 `fixture()`）。

## 怎么用

`web/src/components/realdata.test.ts` 用 `renderToString` 把这些数据喂进
`SystemOverviewPanel`、`DeviceCard`、`DeviceDetailPanel`，断言：

- 容量按真实字节渲染（40 GiB / 8 GiB），悬停显示精确字节；
- `/` 与 `/boot` 的使用率取服务端给的数字（这次是 6.4% 与 14.2%），不四舍五入成好看的整数；
- 整盘没有挂载点就写“未挂载”，不借分区的容量；
- `io.rate` 有采样窗口时显示真实速率（读 `0 B/s` 是真 0），没有窗口时的说明文案由
  `storage.test.ts` 的合成用例守着；
- 温度 `null` 显示 `—`，不出现 `0 °C`；
- `health.status` 是 `unsupported` 时不挂“健康需要处理”、不说“正常”，
  详情页要能看到 `smart_support.available=false` 这句来源原话；
- `capabilities` 为空时明说“当前阶段没有可执行的动作”，不摆按钮；
- 四份响应自洽：同一快照、拓扑节点与设备列表一一对应、关系边两端都存在。

## 不要手改

这些文件的价值就在于“没被美化过”。要更新就重跑上面那条命令重新抓，别顺手改数字——
改过的真实数据和编造的数据没有区别，而且会让这些断言失去意义。

## 这份数据的边界（NOT RUN）

- **只有一台 VM、一种磁盘**：QEMU 虚拟盘，`smartctl` 说它们不支持 SMART。
  真机 SATA/NVMe/USB 盘与睡眠盘的健康结论不在这份数据里，
  覆盖那部分的是 P04.3 的 NVMe/USB 夹具与 `upanel-smart` 验收机。
- **夹具里只有 `sda` 的详情**：`sdb/sdc/sdd` 是空盘，详情在证据目录里（结构与 `sda` 相同）。
- **不含页面渲染证据**：VM 上没有浏览器，像素级证据来自宿主上的
  `tests/smoke/p04-4-ui-smoke.sh`（真实 `upanel-web` + Playwright，mock 模式）。
