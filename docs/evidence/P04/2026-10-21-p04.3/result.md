# P04.3 验收结果 — SMART/NVMe 健康、diskstats 速率、不可达网络挂载隔离

**证据类型**：宿主单元测试（Windows/go1.27.3）＋ Linux VM 集成（22.04 / 24.04 / 带 QEMU NVMe 的 24.04）
＋ 竞态检测器（VM 内 `go test -race`）。**不包含**浏览器渲染证据（P04.4）。

**结论：passed。** 三台 VM 的验收脚本 0 失败；两条 NOT RUN 都不影响本阶段结论，原因逐条写在下面第 6 节。

日期：2026-10-21（宿主本地时间）。脚本版本：`tests/integration/p04-3-storage-health-acceptance.sh`（1058 行）。

---

## 1. 环境

| | upanel-2404 | upanel-2204 | upanel-smart |
|---|---|---|---|
| 系统 | Ubuntu 24.04.5 LTS | Ubuntu 22.04.5 LTS | Ubuntu 24.04.5 LTS |
| 内核 | 6.8.0-147-generic | 5.15.0-198-generic | 6.8.0-147-generic |
| smartctl | 7.4 r5530 | 7.2 r5155（本轮 `apt-get install smartmontools`） | 7.4 r5530 |
| 磁盘 | sda 40G SCSI + sdb 8G + sdc 4G + sdd 4G（全 QEMU HARDDISK） | sda 40G + sdb 8G + sdc 4G + sdd 4G（+ 两个 loop） | sda 40G SCSI + **nvme0n1 4G NVMe** |
| NFS 服务端 | 有（`/usr/sbin/exportfs`） | 有（本轮新装 `nfs-kernel-server`） | **没有** |
| 详情 | `env-2404.txt` | `env-2204.txt` | `env-smart.txt` |

三个入口二进制在三台 VM 上 sha256 逐个相同（`ba1bbc…`/`6b6910…`/`793b40…`）；
原生测试二进制 `storage.test` 为 `0454c41d…`（宿主交叉编译，`-trimpath`）。

## 2. 验收项与结果

对照 STAGES §6、PLAN §8、CONTRACTS §6/§6.2 逐条落地：

| # | 验收项 | 怎么验的（黑盒） | 结果 |
|---|---|---|---|
| 1 | SMART 采集节奏 15 分钟、可配置 5—60，越界是启动错误 | 用 3/61 分钟与超时 0/61 秒启动代理，断言拒绝启动并指出范围；`smartctl_path` 相对路径与含 `?` 都被拒 | pass（三台 VM） |
| 2 | health 是闭集，物理盘只用 smartctl 的结论 | 取快照后枚举所有节点，断言 `status ∈ {healthy,warning,critical,unknown,unsupported,sleeping}`、`source ∈ {smartctl,sysfs,none}` | pass |
| 3 | 分区/loop/rom 是 `unsupported` 而不是 `unknown`，且带理由 | 逐台断言 + `details` 非空 | pass |
| 4 | **与系统工具一致**：自己跑一遍 `smartctl -j -H -A -i` 逐盘比对 | 脚本对每块物理盘单独调用 smartctl，比 `smart_status.passed`、温度、NVMe `critical_warning`/百分比已用 | pass（2204: 4 块全 unknown 且理由一致；smart: NVMe 真实 healthy + 温度 > 0） |
| 5 | 温度不造假：没有读数就是 `null` | 断言全表没有 0 °C，且非物理盘不带温度 | pass |
| 6 | 15 分钟内不重复探测，5 秒循环不碰盘 | 13 秒内连取快照：`collected_at` 前进、`checked_at` 不变；期间 `pgrep smartctl` 为空 | pass |
| 7 | diskstats：每台设备只取自己那一行，计数与 `/proc/diskstats` 对得上 | 先等到「快照的 `sampled_at` ≥ 本机 t0」再在前后各读一次 `/proc/diskstats`，逐字段落进 `[before, after]` 区间，字段是十进制字符串 | pass（三台 VM） |
| 8 | 速率语义：第一次为 `null`，计数器归零/换盘/异常窗口为 `null`，`0` 表示真的没有 I/O | 第二次采样后速率出现；构造归零与异常窗口用单测覆盖 | pass（VM：null→有值；单测：归零/回绕/窗口异常） |
| 9 | io 不改变 `snapshot_version`（P07 预检依赖） | 相隔 11 秒两次取快照，版本相同、期间计数确实变了 | pass |
| 10 | 不可达网络挂载：超时、隔离、不影响其它数据 | 本机 NFS 自导出 + `hard,timeo=600,retrans=2` 挂载 → 停 `nfs-server` → 容量在缓存过期后变 `null`、`reachable=false`、`capacity_error` 同时说明超时与「不再重试」；5 秒扫描仍在前进；整个停服期间最多 1 个 statfs 子进程 | pass（2404、2204；smart 机无 NFS 服务端，见第 6 节） |
| 11 | 隔离到期后自动恢复（5 分钟） | 重启服务后轮询最长 6 分钟，容量回来 | pass（2404、2204） |
| 12 | `--statfs-helper` 只接受一个绝对路径 | 直接执行代理的 helper 模式：绝对路径输出一行 JSON；空/相对/两个参数/带引号一律退出码 2 | pass |
| 13 | **采集失败不会卡住登录** | 在 NFS 停服、`statfs` 挂在内核里的时刻启动 Web 并掐表登录：`2404: 332 ms`、`2204: 271 ms`，且同一时刻该挂载仍是 `reachable=false` | pass（2404、2204） |
| 14 | Web 透传（权限 V、字段原样） | 未登录 401；admin/viewer 200；设备带 `io`（可为 null）、字节字段是字符串、`health` 未被改写；拓扑里 `network_mounts` 是数组且字段完整 | pass |
| 15 | 没有 smartctl 时是 `unknown` 不是 `healthy` | 配置 `/nonexistent/smartctl-for-acceptance` 重启代理：物理盘 `unknown` 且带说明，分区仍是 `unsupported`，恢复配置后代理正常重启 | pass |
| 16 | 日志卫生 | 代理日志里没有命令行回显、没有私钥 | pass |

## 3. 三台 VM 的结果

| 检查 | upanel-2404 | upanel-2204 | upanel-smart |
|---|---|---|---|
| `p04-3-storage-health-acceptance.sh` | 通过 **77** / 失败 0 / 未执行 1 | 通过 **77** / 失败 0 / 未执行 1 | 通过 **62** / 失败 0 / 未执行 2 |
| 未执行项 | NVMe 真实 healthy（本机无 NVMe） | NVMe 真实 healthy（本机无 NVMe） | 网络挂载隔离实验、NFS 卡住时登录不受影响（本机无 NFS 服务端） |
| 原生 `storage.test` | PASS 103 / SKIP 0 / FAIL 0 | PASS 103 / SKIP 0 / FAIL 0 | PASS 103 / SKIP 0 / FAIL 0 |

日志：`p04-3-2404.txt`、`p04-3-2204.txt`、`p04-3-smart-2404.txt`；
原生测试：`native-storage-2404.txt`、`native-storage-2204.txt`、`native-storage-smart.txt`。

宿主：`go build ./...`、`go vet ./...`、`gofmt -l .` 全清（`host-gates.txt`）；
`go test ./... -count=1 -v` → PASS 568（含子测试 744）/ FAIL 0 / SKIP 6 / 20 个包 ok（`go-test.txt`、`go-test-verbose.txt`）；
`scripts/check.sh` → PASS 12 / FAIL 0 / NOT RUN 0，`openapi-lint` valid（9 warnings）（`check.txt`）。

竞态：VM 内 `go test -race -count=1 ./...` → **19 个包 ok、0 条 `WARNING: DATA RACE`**
（`race-final-2204.txt`）。唯一失败是 `TestDefaultPasswordParamsTiming`：`-race` 把 Argon2id 单次校验
放到 1.79—2.03 秒，用例上界是 1 秒；同一用例不带 `-race` 时 0.6 秒通过。这是检测器的开销，不是竞态。
这一次是在**装过面板、`/etc/upanel/upanel.json` 存在**的机器上跑的，`cmd/upanel-web` 仍然 ok，
说明 Q103 的测试隔离修复真的生效（修复前同一条件下有 3 条失败）。

## 4. 关键证据摘录

```
PASS  登录耗时 332 毫秒（< 5000，没有被 statfs 拖住）
PASS  登录的那一刻网络挂载仍不可达（不是卡点已过）
PASS  capacity_error 说明是超时
PASS  capacity_error 说明多久之后才重试
PASS  卡住的网络挂载没有拖住 5 秒扫描（collected_at 仍在前进）
PASS  隔离生效：整个停服期间最多只有 1 个 statfs 子进程
PASS  NFS 恢复后容量在隔离到期后自动回来
PASS  API 的健康结论与 smartctl 自身输出逐盘一致
PASS  NVMe 盘给出真实 healthy
PASS  NVMe 盘给出真实温度（> 0）
PASS  每台设备的计数与 /proc/diskstats 自己那一行对得上（且是十进制字符串）
PASS  期间 io 确实变化了，而版本没变（证明 io 不参与指纹）
```

`network_mounts` 是**本轮补的产品缺口**：P04.2 的挂载只挂在有块设备的节点上，NFS 的 major:minor
在 `/sys/dev/block` 里查不到，所以「网络挂载独立显示可达性和容量」这条在真机上永远走不到。
现在它是 `Topology` 里与 `nodes`/`edges` 平级的一个数组（CONTRACTS §6.2、DECISIONS Q101），
验收里 viewer 能直接读到。

## 5. 本轮发现并修掉的问题

| 问题 | 处理 | 决策 |
|---|---|---|
| io 的验收断言偶发失败：查询返回的是 5 秒缓存快照，`sampled_at` 可能早于本机读数 | 验收改成「先等到快照的 `sampled_at ≥ 本机 t0`，再读前后两份 `/proc/diskstats`」；三轮不通过后定位到根因是取数窗口没对齐，不是产品错 | Q102 关联 |
| NFS 挂载在拓扑里完全看不到（隔离代码是死代码） | 新增 `network_mounts`；契约、schema、测试、验收同步补 | **Q101** |
| 「停服后容量为 null」在 20 秒断言失败 | 容量有 30 秒缓存，改成等结论（上限 120 秒，实际约 32 秒） | **Q102** |
| `cmd/upanel-web` 的几条用例在装过面板的机器上失败：不传 `--config` 会读 `/etc/upanel/upanel.json`，测试结果取决于机器状态 | 加 `testRun` 助手，缺省补一份 `{}` 空配置；产品代码没改 | **Q103** |
| `smart_test.go` 是 CRLF 行尾（`gofmt -l` 报整文件差异） | 归一化成 LF；`CONTRACTS.md` 同样的行尾问题一并修 | — |
| 验收脚本自己写错两处：创建 viewer 用了 `password`（契约字段是 `initial_password`）、第 10 段把坏配置写到临时文件而代理读的是 `$SYS_CFG` | 脚本修正并重跑 | — |

## 6. NOT RUN（未执行，及为什么）

| 项 | 原因 | 怎么补 |
|---|---|---|
| **睡眠盘不唤醒**（STAGES §6「休眠跳过」的真机证据） | QEMU 的 SCSI/NVMe 盘进不了 standby：`smartctl -s standby,now` 之后 sysfs 的 `state` 仍是 `running`，没有可休眠的盘 | 真机上用一块能进 standby 的 SATA 盘跑一次；行为目前只有单测（`TestSleepingSysfsStates` + 两级探测顺序） |
| **ATA/SATA 的真实 smartctl 输出** | `-device ide-hd` 在 `-accel whpx` 与 `tcg`、pc 与 q35 下都让 guest 卡在固件阶段（console 0 字节），拿不到真实 ATA JSON | 真机或有 AHCI 支持的虚拟化环境；ATA 属性解析目前只有合成输入（`syntheticATA*`） |
| **NVMe 真实健康（2404/2204）** | 两台机器只有 QEMU SCSI 盘 | 已由 `upanel-smart` 覆盖（NVMe 真实 `healthy` + 温度 > 0） |
| **网络挂载隔离（smart 机）** | 该 VM 没装 `nfs-kernel-server`，制造不出「statfs 卡住」 | 已由 2404、2204 覆盖；该机上这两项如实记为 NOT RUN |
| 真实生产服务器 / 真实 SATA、USB、RAID、LVM 拓扑 | 只有 QEMU SCSI/NVMe/loop | 真机复测（P04.2 已记同样限制） |
| 前端渲染（首页、磁盘列表） | P04.4 范围 | P04.4 |
| 宿主 `go test -race` | 宿主没有 gcc/cgo | 已在 VM（22.04, gcc 11.4）完成 |

## 7. 复现

```sh
# 宿主：交叉编译 + 闸门
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 sh -c 'for c in upanel-web upanel-agent upanelctl; do
  go build -trimpath -o bin/linux-amd64/$c ./cmd/$c || exit 1; done'
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -c -trimpath -o bin/linux-amd64/storage.test ./internal/storage/
go build ./... && go vet ./... && gofmt -l . && go test ./... -count=1 && sh scripts/check.sh

# VM：把三个二进制放 /tmp/upanel-bin（chmod 0755），脚本放 /tmp
sudo BIN_DIR=/tmp/upanel-bin sh /tmp/p04-3.sh          # 约 20 分钟（含 5 分钟隔离期）
sudo env WAIT_QUARANTINE=0 BIN_DIR=/tmp/upanel-bin sh /tmp/p04-3.sh   # 跳过隔离期，该项 NOT RUN
```

`/tmp/upanel-bin/fixtures/` 必须是 `tests/fixtures/` 的副本，否则原生测试会以「定位夹具失败」报错。
