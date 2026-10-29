# P15.2 真机验收（VM-A，Ubuntu 22.04.5）

本目录是 **P15.2（受管部署：`deployment.apply` / `deployment.remove`）的第一次真机验收**。
脚本 `tests/integration/p15-2-acceptance.sh`，两段：`PHASE=main`（准备现场、部署、重放、
判决层、停止与卷依赖、留好重启状态并重启）与 `PHASE=boot`（重启后：不自启、依赖恢复后起来、
删除保留数据）。

## 结论

| 段 | 命令 | PASS | FAIL | NOT RUN |
|---|---|---|---|---|
| main | `DISK=sdd REBOOT=1 BIN_DIR=/var/tmp/upanel-bin sh /var/tmp/p15-2-acceptance.sh` | **199** | **0** | 2 |
| boot | `PHASE=boot BIN_DIR=/var/tmp/upanel-bin sh /var/tmp/p15-2-acceptance.sh` | **51** | **0** | 0 |

两条 NOT RUN 都在 main 段，理由写在 `p152-main.log` 里，不是"跑失败了"：

- `容器里读到的 marker`：镜像是 `traefik/whoami`（scratch），容器里没有 `cat`/`sh`，读不到
  文件就是读不到。挂载由 `docker inspect` 的 `Source` 与 `findmnt` 交叉核对（另有
  `facts/container-marker.txt`）。
- `运行中卷失联 → systemd 自动停单元`：容器把那块卷 bind 进了自己的 mount namespace，内核里
  是 busy；要造这个现场只能 `umount -l`，那是假现场，读数不能当证据。卷不可用时"起不来"这一半
  由故障注入验了（`systemctl mask <mount unit>`，见下）。

## 环境与二进制

| 项 | 值 |
|---|---|
| VM | `upanel-2204`，Ubuntu 22.04.5 LTS，内核 5.15.0-198-generic，`ssh -p 2223 upanel@127.0.0.1` |
| docker / compose | 29.1.3 / 2.40.3+ds1-0ubuntu1~22.04.1 |
| 夹具盘 | `/dev/sdd` 4 GiB（本轮 `sgdisk --zap-all` 清空后由脚本自己分区与格式化） |
| `upanel-agent` | `96871ee3a9d4ad709b344861f810d8e1426a6ccf0d8487924e21c7cb014ce58c` |
| `upanel-web` | `2647c529acf5ab46c48305752a23c9e8ca53ea31fee1541a3bbf37822183661e` |
| `upanelctl` | `92e2dff5601705329798d6e2a575419086074913a3fb5ba5070b02b769d781c3` |
| 验收脚本 | `e0c03b2dcb2a30609356c7c78e75bdbfb32711f80429725822638520f73348dd` |
| 机器日期 | 2026-10-27（main 段 11:37→11:45 重启，boot 段 11:45 之后） |

## 文件

| 路径 | 内容 |
|---|---|
| `RESULT` / `RESULT-boot` | 两段的判决账（`PHASE` / `PASS` / `FAIL` / `NOT RUN` 与失败名） |
| `p152-main.log` / `p152-boot.log` | 两段的完整控制台输出（含每一条断言的名字与读数） |
| `state.env` | main 段留给 boot 段的令牌（卷标识、部署标识、配置摘要、marker 摘要…） |
| `facts/` | 现场读数：`unit.service`（包装单元逐字）、`compose.yaml`（渲染配置）、`roots.json`、`capabilities.json`、`boot.txt`、`status-after-boot.txt`、`journal-after-boot.txt`、`mount-after-boot.txt`、`diag/`、`start-blocked.log` … |
| `logs/` | 代理与 Web 的日志、每次 systemctl 的原始输出、起点清理与 mask/unmask 记录 |
| `payloads/` | 每一次预检与提交的请求/响应原文（含被拒绝的那几次） |

`facts/` `logs/` `payloads/` 是**工作目录在两段跑完之后的样子**：两段共用
`/var/tmp/upanel-accept-p15-2`，boot 段改写了其中与重启相关的几份（`boot.txt`、
`journal-after-boot.txt`、`diag/`）。main 段的读数以 `p152-main.log` 为准。

## 关键读数

- **部署真的起来了**：`部署结果是 verified`，`项目下有 1 个容器在跑=1`，`container-marker`
  与 `diag/docker-ps-a.txt` 里有 `webapp-webapp-1 Up … webapp`；`宿主端口 18087 上的容器应答
  HTTP 200`，应答里看得见 `Host: 127.0.0.1:18087`。
- **包装单元逐字**（`facts/unit.service`）：`Type=oneshot`、`RemainAfterExit=yes`、
  `Requires=docker.service`、逐卷 `BindsTo=`/`After=`/`RequiresMountsFor=`、
  `ExecStart=/usr/bin/docker compose -p webapp -f /etc/upanel/deploy/webapp.compose.yaml up -d --remove-orphans --no-build`、
  `ExecStop=… down --remove-orphans`，单元里没有 `/bin/sh`。
- **幂等重放**：`重放没有重写文件=False`（`written=false`）、`idempotent=true`、
  `重放没有重启单元`（`unit_restarted=false`）、容器 ID 与 `StartedAt` 都没变、HTTP 仍 200。
- **判决层**：带 `deployment_id` 的预检 → `update_not_implemented`（理由指向 P15.3）；
  同名换端口 → `deployment_exists` 且磁盘上两份文件一个字没动。
- **卷依赖**（故障注入 `systemctl mask srv-upanel-volumes-p15data.mount`）：
  `systemctl start` 失败，原文 `Failed to start upanel-deploy-webapp.service: Unit
  srv-upanel-volumes-p15data.mount is masked.`；单元不是 active、项目里没有容器、卷没挂上、
  marker 没落到系统盘（`appdata_entries=0`）。预检同时给出 `volume_not_ready`。
- **重启之后**（`facts/boot.txt`）：`uptime=64`、`is-active=inactive`、`is-enabled=enabled`
  ——容器 `restart=no`、没人拉它，单元被 masked 的 mount unit 挡在事务之外（
  `facts/journal-after-boot.txt` 是 `-- No entries --`，`facts/mount-after-boot.txt` 里
  `LoadState=masked`），系统盘上也没有多出数据。
- **依赖恢复之后**：`systemctl unmask` → `start mount` → `start unit` 成功，容器回来、HTTP 200、
  `容器 restart 策略仍是 no`。
- **删除**：`删除结果是 verified`、`unit_removed`/`config_removed` 都是 `True`，单元文件与渲染
  配置都不在了、systemd 报告 `not-found`；**数据一个字节没动**——业务目录还在、`marker.txt`
  逐字节一致、卷还挂在原地、卷上文件数没变。重复删除的预检被 `no_such_deployment` 挡住。

## 本轮修掉的东西（真机才暴露的）

| 类型 | 症状 | 去处 |
|---|---|---|
| 产品缺陷 | 新建部署的预检 100% 500（`缺少目标状态版本`） | DECISIONS Q484，`deploy.deploymentStateVersion` |
| 产品缺陷 | 带 `deployment_id` 的 apply 预检 500（`缺少规范化参数`） | `deployPlanFromError(err, params)` |
| 产品缺陷 | apply 落在**已有的 active 单元**上时 `start` 是空操作，容器一个都没起 | DECISIONS Q485，`Applier.Apply` 按现场选 `restart` |
| 产品缺陷 | `deployment.remove` 在真机上永远删不掉：`is-enabled` 的退出码 1 被当成执行失败 | DECISIONS Q486，`systemctlAnswer` + 假件补齐真实形状 |
| 脚本缺陷 | 起点没清干净（上一轮的 active 单元、fstab 条目、残留容器） | `tests/integration/p15-2-acceptance.sh` §1 |
| 脚本缺陷 | `findmnt --mountpoint` 对子目录返回空 → 假红 | 改 `--target` |
| 脚本缺陷 | `runuser` 写不进 0700 的 root 目录 → facts 缺文件 | 请求体内联 + root 重定向 |
| 脚本缺陷 | 删除后仍用 `-f <已删掉的配置>` 数容器 → compose 的报错行被数成 1 | 改用 docker 标签（`project_all`） |
| 脚本缺陷 | 假绿：容器 ID / `StartedAt` 两侧都为空也算"没变" | 先断言非空前提 |

## 仍然未验证

- 运行中卷失联时 systemd 自动停单元（造不出干净现场，见上）。
- 真实**更新**（`deployment.update`）、镜像 digest、按选择备份、健康探测与回切：P15.3。
- 面板页面上的部署与更新（P15.4）。
- 另外只在一台 VM（22.04.5）上跑过；24.04 的 systemd/docker 差异没有读数。
