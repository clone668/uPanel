# 测试夹具（tests/fixtures）

存放**脱敏后的真实命令输出样本**，用于解析器单元测试。不是手写的“看起来像”的 JSON。

## 规则

1. 夹具必须来自真实命令，并在同目录的 `PROVENANCE.md` 记录：
   命令、发行版与版本、命令退出码、采集日期、采集环境（VM 编号）、脱敏处理。
2. 不允许写入真实主机名、用户名、IP、MAC、序列号（可用规则化假值，但必须标注“已替换”）。
3. 不允许包含秘密、令牌、私钥、密码哈希。
4. 不提交大文件；单个夹具不超过 512 KiB。
5. 手写的反例（例如损坏的 JSON、空数组、负值计数器）可以存在，
   但文件名必须以 `.synthetic.` 标注，避免被当成真实采集结果。

## 命名

    <tool>-<variant>.<ext>
    lsblk-sata-1disk.json
    lsblk-nvme-2disk.json
    lsblk-lvm-root.json
    lsblk-raid-md0.json
    lsblk-no-serial.json
    lsblk-duplicate-identity.json
    lsblk-loop.json
    lsblk.synthetic.badjson.json
    findmnt-multi-mount.json
    smartctl-sata-healthy.json
    smartctl-nvme-warning.json
    smartctl.synthetic.exit-bitmask.txt
    diskstats-sata.txt
    meminfo.txt
    stat-cpu.txt
    swapon.json

## 各阶段最少需要的夹具

| 阶段 | 必须覆盖 |
|---|---|
| P04 | SATA、NVMe、USB、无序列号/重复身份、SMART 非零退出位掩码、睡眠盘、LVM 根卷、RAID、loop、坏 JSON、空数组、多挂载点 |
| P05 | `systemctl show` 结构化属性、journal JSON 行、进程 `/proc/<pid>/stat` |
| P06 | 目录项属性（mode/owner/nlink）、上传分块、路径穿越输入集合 |
| P07 | `sfdisk --json`、`blkid`、`findmnt --json`、fstab 样本（含外部无关内容） |
| P10 | `nginx -T`、`testparm -s`（P08）、`nginx -t` 失败输出 |

P00 只建立目录与规则；夹具随对应阶段的解析器一起提交，
在此之前不预造“假样本”充数。
