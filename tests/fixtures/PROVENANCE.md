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
