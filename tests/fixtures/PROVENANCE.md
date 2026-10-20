# 夹具来源（tests/fixtures）

本目录的 P04 夹具记录命令、环境、退出码与采集日期。凡是 `*.synthetic.*` 的文件都是
**手写反例**，不是真实采集结果。

## 真实采集（VM-B，`upanel-2404`）

| 项 | 值 |
|---|---|
| 采集日期 | 2026-10-21（宿主本地时间） |
| 虚拟机 | `upanel-2404`（VM-B），127.0.0.1:2222 |
| 发行版 | Ubuntu 24.04.5 LTS（Noble） |
| 内核 | 见 `os-release-2404.txt` 同机 `uname -r`；本次采集未单独保存 |
| 登录身份 | `upanel`（uid 1000，非 root），只读 `cat` |
| 退出码 | 全部 0 |
| 脱敏 | 无需替换：这些文件不含主机名、用户名、IP、MAC、序列号 |

| 文件 | 命令 |
|---|---|
| `stat-cpu-2404.txt` | `cat /proc/stat` |
| `meminfo-2404.txt` | `cat /proc/meminfo` |
| `loadavg-2404.txt` | `cat /proc/loadavg` |
| `netdev-2404.txt` | `cat /proc/net/dev` |
| `os-release-2404.txt` | `cat /etc/os-release` |
| `uptime-2404.txt` | `cat /proc/uptime` |

`netdev-2404.txt` 只有表头、`lo` 与 `ens4` 三行数据；网卡名是内核分配的名字，
其中的 MAC 与 IP 不在 `/proc/net/dev` 里，因此不需要脱敏。

## 手写反例

| 文件 | 用途 |
|---|---|
| `stat.synthetic.truncated.txt` | cpu 行只有 5 个分量（少于内核固定提供的至少 8 个），必须被判为格式错误，而不是按缺省 0 补齐 |
| `stat.synthetic.no-agg.txt` | 只有 `cpu0`，没有合计 `cpu` 行：没有合计就无从算总使用率，必须报错 |
| `meminfo.synthetic.missing-available.txt` | 删掉 `MemAvailable`：缺必需字段时必须整段失败，不能拿 `MemFree` 顶替 |
| `netdev.synthetic.badcols.txt` | 冒号后只有 15 列（少一列），必须报错 |
| `netdev.synthetic.empty.txt` | 只有表头、没有网卡：必须报错，不能返回空列表冒充“没有网卡” |
| `os-release.synthetic.no-id.txt` | 没有 `ID` 也没有 `NAME`：不是可识别的 os-release，必须报错 |
| `loadavg.synthetic.bad.txt` | 第三个负载值不是数字，必须报错 |

## 存储拓扑夹具（P04.2）

`storage-2404/` 与 `storage-2204/` 是**整棵目录**，不是单个文件：`storage.Scan` 读的是目录树
（`sys/block`、`sys/class/block/<dev>/…`、`dev/disk/by-*`、`proc/self/mountinfo`、`proc/swaps`）
外加盘头部切片。它们的来源、采集时间、宿主机与逐文件 sha256 写在各自的 `MANIFEST.txt` 里，
采集脚本是 `scripts/capture-storage-fixtures.sh`（在验收 VM 内原样拷贝，不改写内容）。

| 目录 | 来源 | 用途 |
|---|---|---|
| `storage-2404/` | Ubuntu 24.04.5 / 6.8.0-147（upanel-2404），2026-10-20T17:33:19Z 采集 | 真机样本：sda 40 GiB + sda1(`/`)/sda14/sda15(`/boot/efi`)/sda16(`/boot`)、sdb 8 GiB、sdc/sdd 4 GiB、8 个 loop、fd0 |
| `storage-2204/` | Ubuntu 22.04.5 / 5.15.0-198（upanel-2204），2026-10-20 采集 | 同一形态的另一代内核（snap loop 挂载、分区号不同） |

两条注意：

- `dev/disk/by-*` 里的条目在真机上是**符号链接**（指向块设备），夹具里是写着目标路径的**普通文件**。
  这正是 P04.2 那个 OOM 缺陷（DECISIONS Q89）逃过夹具单测的原因——只有真机会按符号链接读；
  补上的 `TestReadDiskLinksUsesSymlinkTarget` 用**真的**相对符号链接把两条路径区分开。
- 内容不含主机名以外的敏感信息；`MANIFEST.txt` 里的 UUID 是虚拟机的一次性文件系统标识，不是生产数据。
