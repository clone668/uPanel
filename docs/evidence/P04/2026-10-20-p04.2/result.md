# P04.2 验收结果（存储拓扑只读：块设备树、设备身份校验、系统盘保护识别）

日期：2026-10-20。
代码版本：`git rev-parse --short HEAD` = 提交时的 HEAD（见 `commands.log`），`VERSION=0.0.0-p04.2`，
`BUILD_DATE=2026-10-20T00:00:00Z`（固定值，三份入口二进制可逐字节重建，见本文末）。

## 结论

**passed。** 代码 + 宿主闸门 + 两台 Ubuntu 验收 VM 全部完成，没有放宽任何断言，
没有 NOT RUN 的验收项。发现的三个真缺陷都由“真机验收”这一环抓出（不是夹具单测）：
块设备被当普通文件读导致代理被 OOM 杀死（Q89）、`snapshot_version` 依赖时间（Q90）、
`/dev/disk/by-*` 链接目标缺少“已知设备”约束（Q91）。另修掉一个测试抖动（Q96）。

| 检查 | 24.04.5 / 6.8.0-147 | 22.04.5 / 5.15.0-198 |
|---|---|---|
| `p04-2-storage-acceptance.sh`（新增，108 项断言） | 通过 **108** / 失败 0 / 未执行 0 | 通过 **108** / 失败 0 / 未执行 0 |
| VM 内核原生测试二进制（9 个） | TOTAL PASS **473** / FAIL 0 | TOTAL PASS **473** / FAIL 0 |
| 全量回归 `run-all.sh`（13 个脚本） | 运行 13 / 失败 0（合计 **1038** 断言） | 运行 13 / 失败 0（合计 **1038** 断言） |

宿主侧：`go build ./...`、`go vet ./...` exit 0，`gofmt -l .` 无输出，
`go test ./... -count=1 -v` 共 **648** 个 `--- PASS`、0 个 `--- FAIL`（另有 4 个平台相关 SKIP：
`TestOpenRejectsNonRootOwnedDir`、`TestOpenTightensDatabaseFileMode`、`TestSyscmdHelper`、
`TestReadDiskLinksUsesSymlinkTarget`——最后一个在 Windows 上没有建符号链接的权限，在 Linux VM 上真实执行并通过，
见 `storage-regression-*`）。`check.sh` PASS 12 / FAIL 0 / NOT RUN 0，`redocly lint` valid（9 warnings，与上阶段同类同量）。

## 二进制一致性

`cross-build-sha256.txt`、`vm-sha256-2404.txt`、`vm-sha256-2204.txt` 三份清单共 12 个文件（3 个入口二进制 +
9 个测试二进制）逐行相同 → “VM 上跑的就是这份代码”有据可查。入口二进制用固定
`BUILD_DATE` 重复构建得到相同 sha256（本轮实测 3/3 REPRODUCED）。

## p04-2 脚本验到的行为（15 段，全部黑盒：curl + 读 `/sys` `/proc` + 挂载临时镜像 + 杀进程）

1. 环境：root 运行、三个二进制可执行、`upanel-web` 账号存在、python3 可用。
2. 启动代理：`-init-key`、socket、`uid=0`；`capabilities` 如实报 `['storage.topology','system.summary']`
   且 `actions == []`；代理在 10 秒内开始应答（不是“文件出现了就算起得来”）。
3. 拓扑快照：`status=ok`、`interval_ms=5000`、`collected_at` 在 15 秒内、`snapshot_version` 是 sha256。
4. 设备集合与 `/sys/block` + 分区**完全一致**（17 台）；每台 `size_bytes`/`read_only` 与 sysfs 精确一致；
   `size_bytes` 是十进制字符串；扇区大小关系成立；`kind=partition` 的集合与内核 `partition` 属性一一对应。
5. 边：两端都是已知设备、`relation` 只在 `partition_of`/`depends_on` 内、无重复、无自环；
   sysfs 认的每个分区都有 `partition_of` 边指回整盘，且不多出边。
6. 身份：只有真读到 WWN/序列号才报 `high`（QEMU 上如实 `medium`）；指纹是 sha256；不同设备不撞车；
   同型号同容量的盘互不相同；同一内容两次查询版本不变、逐台指纹一致（身份稳定）。
7. 诚实性：`health.status` 一律 `unknown`、`temperature_celsius`/`checked_at` 是 `null`；
   每台设备 `capabilities == []`；读不到的字段是 `null` 不是空串。
8. 系统盘保护：从 `/proc/self/mountinfo` **独立**算出承载关键路径的设备（24.04 上是 sda1/sda15/sda16），
   逐个必须被标为系统盘；理由含“承载根文件系统”；保护沿边递归向上；空白盘 sdb/sdc/sdd 不被保护。
9. 临时 loop 镜像：64 MiB `mkfs.ext4` + `losetup` + mount 后，kind 是 `loop`、挂载点出现、
   **不因被挂载而变成系统盘**；容量 `total_bytes` 与 statvfs 精确一致，`available_bytes` 与空闲镜像的
   `f_bavail` **精确相等**（该盘 `f_bfree` 比它大 4 694 016 字节，所以这条断言真能区分取错字段）；
   挂载出现/消失都让 `snapshot_version` 变化，且消失后回到原值 → 指纹只由内容决定。
10. 快照节奏：一个间隔后 `collected_at` 前进 4—30 秒。
11. 容量：`/` 的 `total_bytes` 与 statvfs 精确一致；`available_bytes` 落在实时 `f_bavail` 的 0.5% 内
    （30 秒采集窗口内会漂移，脚本里写明了为什么留这个容差、以及本机 `f_bfree - f_bavail` 只有 16 MiB
    因此**这条区分不了 Bavail/Bfree**，严格对账放在第 9 段）；`used_percent` 在 0—100；容量自带 `collected_at`。
12. 查询边界：`storage.topology` 不接受任何参数（含 `command`、`path`）、未知查询名 400 且说明“未登记”、
    信封里夹带未知字段 400。
13. Web 透传：未登录 401；管理员与 viewer 都 200（权限 V）；Web 与代理给出**同一个** `snapshot_version`
    且没有重排边、没有改保护判定；设备详情只带与该设备相关的边；未知设备 404；
    `devices/sda;id`、`devices/..%2f..%2fetc%2fshadow`、`devices/sda%20--force` 都不是命令入口。
14. 代理停机：`/storage/topology` 与 `/storage/devices` 503 且**不返回任何设备**；代理恢复后回到 200 且仍是同一份真实拓扑。
15. 日志卫生：代理日志无 `sh -c` 痕迹、无私钥泄漏、无 panic；Web 日志无 panic。

## 证明“测试有牙”

- `TestReadDiskLinksUsesSymlinkTarget`：真符号链接 `../../sda1` 必须解析成 `sda1`；指向 1 MiB 普通文件的链接
  必须被忽略。**这个用例在 VM 上真跑**（Windows 上因缺符号链接权限 SKIP），`storage-regression-*.txt` 有 `--- PASS`。
- `TestSnapshotVersionIgnoresTimeAndCapacity`：相隔 37 秒的两次扫描指纹相同；补上容量后仍相同；改挂载路径后不同。
- 验收第 9 段的 `available_bytes` 与 `f_bavail` 精确相等 + `f_bfree` 差 4.69 MB，是“取错字段就红”的硬证据。
- 第 8 段保护的判定由脚本用 shell + mountinfo **独立重算**后逐台比对，不是拿被测代码验证被测代码。

## 本轮修掉的问题

| 问题 | 处理 | 决策 |
|---|---|---|
| 代理在真实 24.04 上约 20 秒后被 OOM 杀掉（夹具单测全绿） | `linkTarget` 先 Lstat 再 Readlink，普通文件分支只认 `/dev/` 绝对路径；新增回归用例 | Q89 |
| `snapshot_version` 每 5 秒就变（违反 CONTRACTS §6 预检语义） | 哈希前归一化 `collected_at` / `checked_at` / 容量 | Q90 |
| `/dev/disk/by-*` 指向非设备文件时被当成设备名 | 只对本次扫描枚举到的设备名生效；`devNameIn` 按 devDir 相对化 | Q91 |
| 22.04 / 24.04 上 `capabilities` 断言过期（只列 `system.summary`） | `p02-3`、`p04-1` 改成精确列表 + 注释说明少报多报都算失败 | Q92 |
| 验收脚本 `jget`/`jgetx` 把 `None` 也打印、`NameError` 变空串静默失败 | 抽出 `_probe`：截 stdout，语句块用它的 print，单表达式用返回值；命名空间补 `json`/`os` | Q95 |
| `TestSweepExpiredSecrets` 高负载下偶发失败（TTL 50ms 上做边界断言） | 拆成“长 TTL 断言不误杀 + 短 TTL 断言会销毁”，语义不变 | Q96 |

## 环境事实（不记作产品缺陷）

24.04 验收 VM 在本轮**第五次** `Attempted to kill init!` 内核 panic（exitcode=0x8b）：发生在
全量回归跑到一半时，`console.log` 有完整栈。重启后 `/tmp` 清空，重新上传二进制/脚本/夹具、
逐个核 sha256（12/12 一致）后，**先跑原生测试（473/0）、再跑 p04-2（108/0）、最后跑全量回归（13 脚本 0 失败）**，
本文所有 24.04 结论都来自这次重启后的那一轮。五次 panic 全在 24.04，22.04 一次没有。

## NOT RUN

- 真实生产服务器（只有两台 QEMU 虚拟机）。
- LVM / LUKS / 软 RAID / NVMe / 多路径的真实拓扑：两台 VM 只有 SCSI 盘与 loop，`dmsetup`/`mdadm` 未构造。
- 休眠盘（`/sys/block/*/device/power` 处于 sleep）不唤醒的行为：验收机上没有可休眠的盘，只有单测覆盖。
- SMART 与温度：属 P04.3。
- `go test -race`：宿主与 VM 都没开 cgo。
- 前端页面渲染（P04.4 的首页）。

## 复现方式

```sh
# 宿主
export PATH="/c/Program Files/Go/bin:/c/Users/huxi/AppData/Local/nvm/v22.23.3:$PATH"
go build ./... && go vet ./... && gofmt -l . && go test ./... -count=1
sh scripts/check.sh

# 交叉编译（固定 BUILD_DATE 可逐字节重建）
VERSION=0.0.0-p04.2 COMMIT=$(git rev-parse --short HEAD) BUILD_DATE=2026-10-20T00:00:00Z
L="-X upanel/internal/version.Version=$VERSION -X upanel/internal/version.Commit=$COMMIT -X upanel/internal/version.BuildDate=$BUILD_DATE"
for c in upanel-web upanel-agent upanelctl; do GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$L" -o dist/linux-amd64/$c ./cmd/$c; done
for p in agentstore agent agentclient operations syscmd store api monitoring storage; do
  GOOS=linux GOARCH=amd64 go test -c -o dist/linux-amd64/tests/$p.test ./internal/$p
done

# VM 内（root）
sudo BIN_DIR=/tmp/upanel-bin sh /tmp/upanel-src/scripts/run-native-tests.sh
sudo BIN_DIR=/tmp/upanel-bin sh /tmp/upanel-src/tests/integration/p04-2-storage-acceptance.sh
sudo BIN_DIR=/tmp/upanel-bin sh /tmp/upanel-src/tests/integration/run-all.sh
```
