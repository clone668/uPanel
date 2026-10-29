# P15.3 真机验收（VM-A，Ubuntu 22.04.5）

本目录是 **P15.3（受管更新：`deployment.update`）的第一次真机验收**。
脚本 `tests/integration/p15-3-acceptance.sh`，只有一段 `PHASE=main`：铺现场（卷、文件根、
备份仓库与策略）→ 建站 → 正常更新 → 同镜像重放 → 探测失败回切 → 带备份的更新 → 各种拒绝 →
留状态与证据。

## 结论

| 段 | 命令 | PASS | FAIL | NOT RUN |
|---|---|---|---|---|
| main | `DISK=sdd BIN_DIR=/var/tmp/upanel-bin sh /var/tmp/p15-3-acceptance.sh` | **229** | **0** | 1 |

唯一一条 NOT RUN 在 §3，理由写在 `p153-main.log` 里，不是"跑失败了"：

- `初始化 GPT / 建分区 / 格式化`：`/dev/sdd 上已有 gpt，按上一轮夹具复用（要重跑请换一块空盘）`。
  P15.2 已经在那块盘上分好区、格式化过 ext4，这一轮不重复擦盘——但**卷这一侧照旧全跑**：
  独立工具读到文件系统 UUID、导入卷（`sdd1` → `slug=p15data`）、卷标识 `vol_` 形状、
  挂载点由 slug 派生、挂载任务 verified、`findmnt` 按 UUID 核对、marker 落在卷上而不是系统盘。
  换句话说：没验的是 `sgdisk`/`mkfs` 那三步，验了的是"这块盘被当成一块真卷用起来"。

## 环境与二进制

| 项 | 值 |
|---|---|
| VM | `upanel-2204`，Ubuntu 22.04.5 LTS，内核 5.15.0-198-generic，`ssh -p 2223 upanel@127.0.0.1` |
| docker / compose | 29.1.3 / 2.40.3+ds1-0ubuntu1~22.04.1 |
| restic | 0.12.1（`compiled with go1.18.1 on linux/amd64`） |
| 夹具盘 | `/dev/sdd` 4 GiB（沿用 P15.2 的分区表，见上） |
| `upanel-agent` | `bc07798eae1a2d795aeb44cf03381d8d7657a1e56883ced65c718094d53b7947` |
| `upanel-web` | `2647c529acf5ab46c48305752a23c9e8ca53ea31fee1541a3bbf37822183661e` |
| `upanelctl` | `92e2dff5601705329798d6e2a575419086074913a3fb5ba5070b02b769d781c3` |
| 验收脚本 | `5354e92aa836a88ee5ded4f96fd19e1894912149ce83aedf34bd442e8ead693d` |
| 交叉编译口径 | `GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath ./cmd/<t>`，go1.27.3 |
| 机器日期 | 2026-10-27（main 段 12:45:39Z → 12:53:01Z，约 7 分 22 秒） |

上面三份二进制摘要是 **VM 上 `/var/tmp/upanel-bin/` 里实际被执行的那三份**。用表里的交叉编译
口径在宿主重编一次，得到的三份字节与它们逐字相同——所以这串摘要是可复现的，不是"只此一份"。

`capabilities.json` 里 `version=0.0.0-dev`、`commit=unknown`：这一轮没打 ldflags，版本串读数是空的。
验收不依赖版本串，但这一条要写在纸上，免得以后拿它当"版本已钉住"的证据。

## 文件

| 路径 | 内容 |
|---|---|
| `RESULT` | 判决账（`PHASE` / `PASS` / `FAIL` / `NOT RUN` 与失败名、未跑名） |
| `p153-main.log` | 完整控制台输出（267 行：每一条断言的名字、读数、六处时间戳） |
| `state.env` | 这一轮派生出来的标识（卷 `vol_`、部署 `dep_`、仓库 `rep_`、策略 `pol_`、两个配置摘要） |
| `facts/` | 现场读数：`deployment-record-v1.json` / `-v2.json`（记录逐字）、`compose.yaml.v2`（更新后渲染配置）、`compose.yaml.prev`（上一版原样字节）、`compose.yaml.after-rollback`（回切后盘上那份）、`deploy-after.txt`（包装单元 + 目录清单）、`roots.json`、`capabilities.json` |
| `logs/` | 代理日志（`agent.log`，108 行：INFO 106 / WARN 1 / ERROR 1，两条都是预期内的，见下）、Web 日志（`web.log`，INFO 354 / WARN 2：一条是"面板未启用 HTTPS"的固定提示，一条是 §5 里故意不带再认证去打建仓库的 403）、`fstab-before-main.txt`、`capabilities.err` / `roots-query.err`（空）、`stale-cleanup.log`（空） |
| `payloads/` | 每一次预检与提交的请求原文与响应原文，含被拒绝的每一次；`independent-snapshots.json` / `independent-cat-config.json` 是**独立工具**（restic、`cat`）的读数，不经过产品解析器 |

`facts/` `logs/` `payloads/` 是工作目录 `/var/tmp/upanel-accept-p15-3` 跑完之后的样子。

## 关键读数

- **建站与记录**（§6）：`建站结果是 verified`；记录 `/etc/upanel/deploy/webapp.deployment.json`
  权限 `0640`、属主 root；记录里的镜像 digest 与 `docker image inspect -f '{{.Id}}'` **逐字相同**
  （`sha256:43a68d10b9dfcfc3ffbfe4dd42100dc9aeaf29b3a5636c856337a5940f1b4f1c`）；
  记录里除 `sec_` 引用外没有秘密残留；`还没有更新过，所以没有 .prev`。
- **正常更新**（§7，`traefik/whoami:v1.10.3` → `nginx:1.27-alpine`）：`结果是 verified`、
  `verified_at` 有；配置摘要 `sha256:ab9215e4…` → `sha256:e406bf01…`，观测里的
  `previous_config_digest` 就是更新前那个；`config_changed=true` / `idempotent=false` /
  `unit_restarted=true` / `unit_active=true`；`health_checked=true`、`health_ok=true`、
  `rolled_back=false`；镜像 digest `sha256:65645c7b…`（与 docker 读数一致，且与上一版不同）。
  盘上三个文件：`webapp.compose.yaml` 0600、`webapp.compose.yaml.prev` 0600（sha256 与字节数都与
  更新前那份**逐字相同**）、`webapp.deployment.json` 0640；容器运行时读回 `nginx:1.27-alpine`，
  `com.docker.compose.project=webapp`，宿主端口 18088 上 HTTP 200（独立 `curl`）。
- **同镜像重放**（§8）：`config_changed=false`、`idempotent=true`、**`unit_restarted=true`**——
  这就是 Q485 那条教训在更新路上的形态，`start` 对 active 的 oneshot 是空操作，必须是 `restart`；
  预检给的是 `config_unchanged` 告警（预检的 `warnings[]`，不是执行期 `observed_state.warnings`）。
- **探测失败 → 回切**（§9，换成 `busybox:1.36.1`）：`12:49:46Z` 开始、`12:51:30Z` 结束，**104 秒**
  （4 次探测尝试真等满了超时）。终态仍是 `succeeded`，`outcome=rolled_back`、`error` 为空；
  `health_ok=false`、`rolled_back=true`、`rollback_verified=true`、`rollback_health_ok=true`；
  盘上配置 sha256 回到更新前那份、`.prev` 没被回切改掉、容器回到 `nginx` 并在跑、HTTP 又通；
  **记录没被改**（新版本从来没被认下来过）。恢复提示原文：

  > 新版本健康探测失败（4 次尝试；请求 http://127.0.0.1:18088/ 失败：…dial tcp 127.0.0.1:18088:
  > connect: connection refused…）；已回切到上一版配置并重新拉起（上一版重新探测通过）。
  > 数据没有回切：如果新版本跑过数据库迁移，旧版本可能读不了这份数据——这不是一次无损回滚

- **按选择备份的更新**（§10，带 `files_only` 策略）：`unit_stopped=true`（先停单元再备份）、
  `backup_verified=true`、`backup_policy_id=pol_3a18a5c7…`、`backup_repository_id=rep_5eb19d5c…`、
  快照 `d5458d5149956652df7a757d7d5c20faec86a2ecd180d938acc377b1e41f44b3`；**独立 `restic snapshots`
  也看得见这个快照**（`short_id=d5458d51`，`paths=["/srv/upanel/volumes/p15data/appdata"]`，
  `tags=["upanel","upanel:policy:pol_3a18a5c7…","upanel:consistency:files_only"]`）；换完镜像探测通、HTTP 200。
- **拒绝**（§11、§12）：`backup_policy_needs_apps`（策略带 `app_ids`，说明里指路去单独跑 `backup.run`）、
  `stale`（拿旧版本令牌提交）、`record_mismatch`（盘上配置被手工改过）、`no_definition_record`
  （记录被删）、`update_not_implemented`（带 `deployment_id` 的 apply）、`deployment_exists`
  （同名但渲染摘要不同）。每一次拒绝之后，盘上配置与容器**一个字节都没动**（§11、§12 各有独立断言）。
- **拉不到镜像**（§12，钉 `traefik/whoami@sha256:1111…1111`）：预检允许（拉镜像是执行阶段的事），
  任务终态 `failed`、`COMMAND_FAILED`，盘上配置与 `.prev` 都没动（pull 在写盘之前）、
  容器还在跑原来那个镜像、单元仍是 active、服务仍在应答——**失败没有把应用留在停掉的状态**。
- **日志里两条非 INFO 都是预期内的**：`12:52:51Z ERROR 动作执行失败` 就是上面那次故意拉不到的
  digest（`failed to resolve reference … not found`）；`12:53:03Z WARN 服务采集失败，保留上一次成功快照`
  是收尾时代理被停、`systemctl` 采集 `context canceled`。两条都能对上现场，不是没消化的噪声。

- **秘密**：`payloads/secret-repopass.json` 里只有 `{"secret_ref":"sec_7e7e7a98…"}`，没有明文；
  用仓库口令的前缀扫过整个证据目录，零命中——口令没有进任何证据文件（这条 README 里也不写它）。

## 本轮修掉的东西（复核对齐时改掉的脚本缺陷）

真机这一轮没有暴露新的产品缺陷（0 FAIL）。下面这些是**写脚本时把断言对齐到产品代码的过程中**
改掉的，记在这里是因为每一条都是"按记忆写就会假绿或假红"的地方：

| 类型 | 症状 | 去处 |
|---|---|---|
| 脚本缺陷 | `$WORK/logs` 在 §0 拉镜像时还不存在，重定向失败被读成"镜像没拉到" | 目录整块提前到 `run_main` 开头 |
| 脚本缺陷 | `update_params` 读了一个从未被赋值的版本串 → 所有 `version` 都是空串，`stale` 分支永远不触发 | 改成按盘上配置现算 `sha256:` 摘要 |
| 脚本缺陷 | `previous` 猜成 `image.previous`（实际 `previous` 直接是 `ImageRecord`） | 改 `d.get("previous",{}).get("requested","")` |
| 脚本缺陷 | 把 `config_unchanged` 当执行期告警读（它只在预检 `warnings[]` 里） | 改读预检那一份 |
| 脚本缺陷 | 一条断言两侧是同一个常量（自比自的假绿） | 删除 |
| 脚本缺陷 | `health_ok` 在"根本没探"时**键不存在**，`jget` 抛 `KeyError` | 新增 `expect_falsy`：不是 `True` 就算过 |
| 脚本缺陷 | §12 拿"同名同内容 apply"当"创建语义"的判据（那份定义渲染出的字节与盘上一模一样，会走幂等路径） | 改成两条真边界：带 `deployment_id` → `update_not_implemented`；同名换端口 → `deployment_exists` |
| 脚本缺陷 | `$TMPD` 是 0700，`web_curl` 以 upanel-web 身份写响应全部拿到 000 | `chmod 1777` |
| 脚本缺陷 | `wait_job` 180s 上限短于 9 分钟更新预算，回切用例会被自己的轮询判死 | 提到 600s |
| 脚本缺陷 | 夹具卷里残留 P15.2 的数据会混进快照 | 挂载后先清空业务目录 |
| 脚本缺陷 | §11 拿 `compose.yaml.v2` 比盘上 sha，而 §10 已经把盘上换成了另一个镜像 | §11 开头取 `DISK_NOW_SHA` |
| 脚本缺陷 | restic 各版本 `snapshots --json` 的 `id` 长度不一致 | 比快照 id 前 8 位 |
| 环境事实 | `RESULT` 是 0600，宿主 `cat` 读不了 | 取回时用 `sudo` |

## 仍然未验证

- `image_digest_mismatch`（记录里的 digest 与现场不一致）在真机上造不出来：注册表按 digest 寻址，
  钉错 digest 会让 `docker pull` 先失败，走不到那个判定。只有单测读数。
- 更新预算切段（`UpdateBudget=9min`、`UpdateBackupBudget=360s`、`UpdateStopBudget`、
  `UpdateRollbackReserve=120s`、`UpdateMinStep=5s`）没有在真机上把时间耗到边界上逼出 `partial`。
- 回切时 `.prev` 反向还原的"第二次回切"（连续两次失败）没有造现场。
- 面板页面上的部署与更新（P15.4）。
- 只在一台 VM（22.04.5）上跑过；24.04 的 systemd/docker 差异没有读数。
