# P04.3 补强（第二轮验收）— 2026-10-21

第一轮验收（`result.md`）判 passed 之后，按 `docs/STAGES.md` §6 的夹具与用例清单逐条回对，
发现三处“文档写了、代码与证据没盖住”的地方。本轮补上，并把三台 VM 的验收从干净状态重跑一遍。

## 1. 为什么要补

| # | 缺口 | 性质 |
|---|---|---|
| 1 | 拓扑里没有一块 **USB 盘**的真实 `smartctl` 输出（只有 SCSI 与 NVMe 夹具） | 证据缺口 |
| 2 | `/proc/self/mountinfo` 里“**同一设备两个挂载点**”没有用例，`scan.go` 的 mount 去重/排序没人守 | 测试缺口 |
| 3 | USB 桥不认识时 `smartctl` 自己给的诊断被吞掉，`details` 只剩“退出状态位 0”，运维看不出为什么没读到 | **产品缺陷** |

第 3 条是去抓真实 USB 输出时才暴露的——不是先想到再补的测试，是先撞上真机输出才发现代码把原话丢了。

## 2. 改了什么

| 文件 | 改动 | 为什么 |
|---|---|---|
| `internal/storage/smart.go` | 新增 `smartctlMessage()`：取 `smartctl.messages` 里 `severity=error` 的那条（没有就取第一条非空的）；位 0 分支、打开失败兜底、最终 `unknown` 兜底都改成“中文原因 + `：` + smartctl 原话” | USB 桥不认识 / `-d sat` 被拒这两条真实路径里，`smart_status` 根本不存在，唯一能解释原因的就是 smartctl 自己的那句话 |
| `internal/storage/smart_test.go` | 新增 `TestEvaluateSmartUSBUnknownBridgeRealCapture`（exit 1 → `unknown`，`details` 含 `Unknown USB bridge`，温度 `nil`）、`TestEvaluateSmartUSBSatOpenFailedRealCapture`（exit 2 → `unknown`，`details` 含“无法打开设备”，温度 `nil`）；位 0 断言文案从“命令行参数被拒绝”改为“退出状态位 0” | 位 0 的语义是“命令行不被接受**或**设备类型无法判定”，只写前者会把 USB 桥这种情况解释错 |
| `tests/fixtures/smart/upanel-smart-usb-unknown-bridge.json` | 真实捕获，`a47cc17b…`，exit 1 | 反例一：没有 `device`、没有 `smart_support`、没有温度 |
| `tests/fixtures/smart/upanel-smart-usb-sat.json` | 真实捕获，`759d891b…`，exit 2 | 反例二：`device.type=sat`、`protocol=ATA`，JSON 里连 `messages` 都没有 |
| `internal/storage/scan_test.go` | 新增 `TestDeviceWithTwoMountPointsKeepsBoth`：从 `storage-2404` 复制一份 `proc/self/mountinfo`，追加一行真实的 `mount --bind` 记录，断言两个 `Mount` 条目都保留、按 path 排序、`fstype` 正确、且 `sda` 仍是 `Protection.SystemDisk` | 一设备多挂载点是最容易被“按设备去重”写错的地方，写错会把另一个挂载点吞掉而不报错 |
| `tests/integration/p04-3-storage-health-acceptance.sh` | 头注释加第 14/15 条；交叉验证加反向规则“API 说 `healthy` 而 `smartctl` 没给通过结论 → 失败”；新增 USB 可见性断言（`transport=usb`，没有则 NOT RUN）；顺手修掉汇总行前面多出来的一个 ` \| `（`FAILED_NAMES`/`NOTRUN_NAMES` 从空串开始拼时总会带一个前导分隔符） | “没读到问题”不能被重新解释成“健康”；USB 盘也必须在拓扑里看得见 |
| `docs/CONTRACTS.md` §6.2 | 两条：`details` 必须带 `smartctl.messages` 原话（优先 `severity=error`）；USB 桥类磁盘只能是 `unknown`，不是 `unsupported` 也不是 `healthy` | 契约层定死，避免以后有人按“没坏消息”实现 |
| `docs/DECISIONS.md` | Q104 | 记录“healthy 必须由 smartctl 自己的通过结论背书”这条判据与代价 |
| `docs/TEST_ENVIRONMENT.md` | VM-E 磁盘布局补 `sdb`（`-WithUsbDisk` 才有的 2 GiB USB 盘）与真实输出说明 | 环境文档跟上，别人复现时知道要加什么参数 |
| `H:\Code\vms\launch-vm-smart.ps1` | `-WithUsbDisk` 开关（`qemu-xhci` + `usb-storage`） | 环境侧脚本，不入库 |

## 3. 真实捕获怎么来的

VM-E（`upanel-smart`，24.04.5 / 6.8.0-147）以 `-WithUsbDisk` 启动，多出一块 QEMU 的
`qemu-xhci` + `usb-storage` 2 GiB 盘，guest 里是 `/dev/sdb`、`TRAN=usb`。然后**按产品自己的参数**采集：

```
$ sudo -n smartctl -j -i -H -A -n standby /dev/sdb          # 退出状态 1
/dev/sdb: Unknown USB bridge [0x46f4:0x0001 (0x000)]
Please specify device type with the -d option
  → JSON 里没有 device、没有 smart_support、没有温度

$ sudo -n smartctl -j -i -H -A -d sat /dev/sdb              # 退出状态 2
Read Device Identity failed: scsi error unsupported scsi opcode
  → JSON 里只有 device.type=sat / protocol=ATA，仍然没有健康与温度
```

两块夹具分别是这两次的**原样输出**，sha256 与采集命令写在 `tests/fixtures/PROVENANCE.md`。
第一条尤其重要：产品调用 smartctl 时就是这么调的（`-n standby`，不带 `-d`），所以这条反例是产品的真实路径，
不是事后编一个更漂亮的命令。

`mount --bind` 那一行也是真机记录：VM-E 上 `mount --bind /srv/upanel/mm/a /srv/upanel/mm/b` 之后
`/proc/self/mountinfo` 给出 `85 28 8:1 /srv/upanel/mm/a /srv/upanel/mm/b ... - ext4 /dev/sda1 ...`，
与 `/` 同为 `8:1`。这行原话被追加到 `storage-2404` 的 mountinfo 副本上。

## 4. 产品行为的变化（本轮 VM 上实测）

补强前，同一块 U 盘在 API 里是 `unknown`，理由是“smartctl 拒绝本次调用（退出状态位 0…）”，
到此为止——看不出是桥不认识。补强后（`usb-sdb-and-nvme-health.jsonl`，VM-E 真实响应）：

```json
{"name": "sdb", "transport": "usb",
 "health": {"status": "unknown", "temperature_celsius": null, "source": "smartctl",
            "details": "smartctl 拒绝本次调用（退出状态位 0：命令行未被接受，或设备类型无法判定），本次结果不可信：/dev/sdb: Unknown USB bridge [0x46f4:0x0001 (0x000)]"}}
```

同一份响应里，`nvme0n1` 是真实 `healthy` + 温度 50，`sda`（QEMU SCSI）是 `unsupported` +
`smart_support.available=false` 的理由——三种结论各自有据，没有互相顶替。

## 5. 本轮验收（全部重跑，干净状态）

三台 VM 都是冷启动后重跑，二进制与脚本先核 sha256（三台上 `upanel-web 639293af…`、
`upanel-agent b417e53f…`、`upanelctl c81c73eb…`、`storage.test ea3b2e8f…`、脚本 `0c30e096…` 逐个相同）。

| 检查 | 24.04.5 / 6.8.0-147 | 22.04.5 / 5.15.0-198 | 24.04.5 + QEMU NVMe + QEMU USB |
|---|---|---|---|
| 验收脚本 | 通过 **77** / 失败 0 / 未执行 2 | 通过 **77** / 失败 0 / 未执行 2 | 通过 **63** / 失败 0 / 未执行 2 |
| 未执行项 | USB 盘在拓扑里可见（本机无 USB 盘）、NVMe 真实 healthy | 同左 | 网络挂载隔离实验、NFS 卡住时登录（本机无 `nfs-kernel-server`） |
| 原生 `storage.test`（顶层用例） | PASS **106** / FAIL 0 | PASS **106** / FAIL 0 | PASS **106** / FAIL 0 |
| 证据 | `p04-3-2404-h2.txt`、`native-storage-2404-h2.txt` | `p04-3-2204-h2.txt`、`native-storage-2204-h2.txt` | `p04-3-smart-h2.txt`、`native-storage-smart-h2.txt` |

本轮 `upanel-smart` 上是 **63** 项（第一轮 62）：多的就是“USB 盘在拓扑里可见（transport=usb：sdb），
并纳入逐盘对账”。`sdb` 与 `sda`/`nvme0n1` 一起过了逐盘交叉验证，`API 的健康结论与 smartctl 自身输出逐盘一致` 依旧是 PASS。

宿主闸门（`host-gates-h2.txt`、`go-test-verbose-h2.txt`、`check-h2.txt`）：
`go build ./...`、`go vet ./...`、`gofmt -l .` 干净；`go test ./... -count=1 -v` 计 **571** 个顶层
`--- PASS`（含子测试 176 条，合计 747）/ **0 FAIL** / 6 SKIP / 20 包 ok；
`sh scripts/check.sh` **PASS 12 / FAIL 0 / NOT RUN 0**（openapi-lint：valid + 9 warnings）。
注意 `check.sh` 里的 `node-version` 需要把 `/h/Code/.toolchains/node-v22.23.3-win-x64` 放到 `PATH` 前面，
否则报 `期望 v22.23.3，实际 …`——这是宿主 PATH 问题，不是产品问题（第一次跑就是这个原因 FAIL 1 项，
带上 PATH 后重跑 12/0/0）。

竞态：VM-2204（22.04.5，gcc 11.4，Go 1.27.3）内以 **root** 跑 `go test -race -count=1 ./...` →
**19 包 ok、0 条 `WARNING: DATA RACE`**（`race-h2-2204.txt`）。唯一失败是
`TestDefaultPasswordParamsTiming`：`-race` 把 Argon2id 单次校验放大到 **2.39 秒**，用例上界 1 秒
（第一轮同一条在 1.79—2.03 秒之间失败，不带 `-race` 时 0.6 秒通过）——检测器开销，不是产品缺陷。
以普通用户跑会在 `internal/agentstore` 失败（“代理数据目录属主必须是 root”），这是预期行为，所以用 root。

## 6. 环境事故（不记作产品缺陷）

- `upanel-2404` 与 `upanel-2204` 执行 `reboot` 后**在 WHPX 上挂死**：console 停在
  `reboot: Restarting system`，QEMU 进程还在、端口转发还在但并不引导，SSH 一直
  “banner exchange timeout”。冷启动（kill 掉进程 + `launch-vm.ps1` 重新启动）后立刻正常。
  这是宿主虚拟化层的问题，两次都是同一处停住。
- 6.8.0 的两个 guest 今天**都 panic 过**，形态完全一致：`Kernel panic - not syncing:
  Attempted to kill init! exitcode=0x0000008b`，`CPU: … PID: 1 Comm: systemd`，前面没有
  任何 `BUG:`/`Oops`/`Call Trace` 头——即 PID 1 自己吃了 SIGSEGV，内核只好跟着停。
  - `upanel-smart`：uptime 2738 s（约 45.6 分钟）时，console 存为 `console.panic-2026-10-21.log`；
    带 `-WithUsbDisk` 冷启动后重跑，63 项全过，之后又跑了 50 分钟以上没有复发。
  - `upanel-2404`：uptime 2802 s（约 46.7 分钟）时，**正好发生在本轮重跑期间**，日志因此截断，
    console 存为 `console.panic-4-2026-10-21.log`；冷启动后重跑才有结果是有效的。
  - 5.15.0-198 的 `upanel-2204` 今天一次都没 panic（当天累计 uptime 已超过 55 分钟）。
  两次都在 45—47 分钟附近，但 `upanel-smart` 之后连续跑了 50 分钟以上不复发——所以**不是**定时器，
  只能记成“同一族偶发”。这是环境侧的不稳定，与产品代码无关：panic 时 `systemd` 已经崩了，
  和被测二进制没关系。
- VM-2204 上以普通用户（`upanel`）跑 `-race` 时 `internal/agentstore` 失败：
  “代理数据目录不可用：目录 … 的属主必须是 root，实际为 1000”。这是**预期行为**（代理数据目录必须 root 所有），
  按第一轮的做法用 `sudo` 以 root 重跑。

## 7. NOT RUN（本轮仍未验证，没有用合成输入冒充）

- **真实 USB 盘**（桥被 smartctl 认识、能读出 SMART）——QEMU 的 `usb-storage` 桥 `0x46f4:0x0001`
  不在 smartctl 的桥表里，这轮拿到的是两个真实**反例**，不是一块能读出健康的 U 盘。
- **睡眠盘不唤醒**的真机行为——QEMU 的 SCSI/NVMe 进不了 standby
  （`smartctl -s standby,now` 之后 sysfs 仍是 `running`），只有单测。
- **ATA/SATA 的真实输出**——`-device ide-hd` / `ich9-ahci` 会让 guest 卡在固件阶段，ATA 分支只有合成输入。
- 真实 SATA/USB-RAID/LVM 拓扑、真实生产服务器、前端页面渲染（P04.4）——同第一轮。

## 8. 复现命令

```sh
# 宿主：把交叉编译产物（GOOS=linux GOARCH=amd64 CGO_ENABLED=0）与夹具打包；bin/linux-amd64 里的
# 四个文件的 sha256 与三台 VM 上部署的完全一致（639293af… / b417e53f… / c81c73eb… / ea3b2e8f…）
mkdir -p /tmp/p043-stage && cp bin/linux-amd64/{upanel-web,upanel-agent,upanelctl,storage.test} /tmp/p043-stage/ \
  && cp -a tests/fixtures /tmp/p043-stage/fixtures
tar czf /tmp/upanel-p043-hardening2.tgz -C /tmp/p043-stage .   # 本次分发件 sha256 = 32af5962…（重打包不会逐字节相同）

# 推到 VM（tar 里带的是 0644，落盘后必须 chmod，否则 upanelctl 起不来）
scp -i <key> -P <port> /tmp/upanel-p043-hardening2.tgz /tmp/p04-3.sh upanel@127.0.0.1:/tmp/
ssh -i <key> -p <port> upanel@127.0.0.1 'rm -rf /tmp/deploy-p043 && mkdir -p /tmp/deploy-p043 && \
  tar xzf /tmp/upanel-p043-hardening2.tgz -C /tmp/deploy-p043 && chmod 0755 /tmp/deploy-p043/* && \
  rm -rf /tmp/upanel-bin && cp -a /tmp/deploy-p043 /tmp/upanel-bin'

# VM 内（root；脚本 0c30e096…）
sudo BIN_DIR=/tmp/upanel-bin sh /tmp/p04-3.sh                       # 两台 NFS 机：77/0/2
sudo BIN_DIR=/tmp/upanel-bin WAIT_QUARANTINE=0 sh /tmp/p04-3.sh     # VM-E：63/0/2
cd /tmp/upanel-bin && ./storage.test -test.v -test.count=1          # 106 顶层用例

# 竞态（VM-2204，root，需要 gcc + CGO；普通用户跑会在 agentstore 因“目录属主必须是 root”失败）
sudo env CGO_ENABLED=1 PATH=/usr/local/go/bin:$PATH go test -race -count=1 ./...
```
