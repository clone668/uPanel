# P16.2 端到端验收 · Ubuntu 24.04（2026-10-27）

STAGES §18 的双平台矩阵的 24.04 半边。**同一份发布包、同一份验收脚本、同一套断言**，
换的只有客户机。22.04 那半边见 `../2026-10-27-p16.2/`。

## 环境

| 项 | 值 |
|---|---|
| 机器 | VM-B，Ubuntu **24.04.5 LTS** |
| 内核 | `6.8.0-147-generic` |
| systemd | **255** (`255.4-1ubuntu8.18`) |
| 资源 | 2 vCPU / 3914 MB / 38 G 根盘 |
| 加速后端 | **`-accel tcg,thread=single -cpu max`**（多线程 TCG 在 check_timer 处 panic，见 TEST_ENVIRONMENT §8.9） |
| 起点 | 从 `ubuntu-24.04-server-cloudimg-amd64.img` 重建的新叠层盘；干净镜像里 nginx / restic / docker 均 **absent**，python3 / curl / openssl 在 |
| 包 | 与 22.04 同一份 `dist/upanel-0.1.0-dev.1-linux-amd64.tar.gz`（sha256 `5216cb31…`） |
| 驱动 | `SSH_PORT=2222 SSH_KEY=…/upanel-2404/id_ed25519 PLATFORM=24.04 EVID=… sh tests/integration/p16-2-run-on-2204.sh` |

## 结果

| 相位 | PASS | FAIL | NOT RUN |
|---|---|---|---|
| install | 40 | 0 | 0 |
| measure | 14 | 0 | 3 |
| boot（真重启之后） | 13 | **1** | 0 |
| upgrade | 22 | 0 | 0 |
| failup | 21 | 0 | 0 |
| uninstall | 23 | 0 | 0 |
| purge | 14 | 0 | 0 |
| **合计** | **147** | **1** | **3** |

**唯一那条失败**：`开机到 Web 可用不超过 120s: 152s`。归因见下一节；其余 147 条与 22.04 逐条一致
（含 Q500 的七条 failup 断言、Q499 的 `limit=1000 → 200`、`limit=5000 → 400`）——
**systemd 255 上 Q499 的修法同样成立**，所以那次修不是"为 249 打补丁"，而是写成了两个版本都认的形式。

## 那条 152s 的归因

原始读数与探针的完整数据在 `facts/boot-attribute.txt`。做法：purge 之后重装同一份包、再重启一次，
读 systemd 的时间戳和 `blame`。

| 读数 | 验收那次 boot | 探针那次 boot |
|---|---|---|
| btime → Web active | **152 s** | **97 s** |
| 其中 `upanel-web` 自身（`blame`） | — | **6.255 s**（含 `ExecStartPre check-config`） |
| 其中 `upanel-agent` 自身 | — | 9.021 s，@btime+60.5s 起来 |
| 整机 | — | kernel 17.3 s + userspace 131 s |
| 头号慢项 | — | `snapd.seeded` 42.8 s、`snapd` 39.5 s、`cloud-init` 16.3 s |

**结论**：面板自己在 6.3 秒内就起来了（`Type=simple`，`ActiveEnter` 就是进程被拉起的时刻；
`After=network-online.target` 在这台机上只等了 2.1 s，不是瓶颈）。152 s 里 96% 是客户机在
**单线程 TCG** 下的引导开销，两次重启差出来的 55 s 也全在这一侧（首次装完那轮 snapd/cloud-init 更慢）。

因此：

* **判据不改**。120 s 对裸机是合理上限，这条红**照样留在 `RESULT-boot` 里**，不改写成通过；
* 定性为**判据在单线程 TCG 上不成立**（环境无法给出有意义的读数），"裸机开机到可用"记为**未验证**，
  列进 P16.4 的支持矩阵与已知限制；
* 这条红**不动摇 §18 的"重启"要求**：重启本身验的是两个单元自启且 `enabled`、socket 回来了、
  健康检查 200、证书与配置没被改写、管理员还能登录、代理链路通——`boot` 相位那 13 条全绿；
* 事后给 `phase_boot` 加了一条 `NOTE`（打印 `systemd-analyze blame` 里面板两个单元各自的耗时，
  **只加读数、不加也不减断言**），下一次换平台才不会又要手工补测。
  **这条 NOTE 是本轮收尾时加的：上述 24.04 读数不是由它产出的**，是探针单独测的。

## 与 22.04 的读数对照

| 项 | 22.04 | 24.04 |
|---|---|---|
| 安装耗时 | 30.0 s | **56.3 s** |
| 重启 → Web active | 60 s | **152 s**（判据不成立，见上） |
| 同包重装（=升级） | 49 s | **67 s** |
| RSS | web 288 MB / agent 25 MB | web **290 MB** / agent **27 MB** |
| `health` 延迟 | p50 46 ms / p95 53 ms | p50 **81 ms** / p95 **148 ms** |
| `summary` 延迟 | p50 52 ms / p95 63 ms | p50 **67 ms** / p95 **150 ms** |
| 日志单页（`limit=1000`） | 76 条 / 21039 B | **79 条 / 21614 B** |
| 第 5 条并发 SSE | 429 | **429** |
| 60 秒空闲 journal | 155 → 155 | **161 → 161** |

两台的比值（安装 1.9×、升级 1.4×、延迟 1.7—2.8×）落在"单线程 TCG 跑 24.04 比跑 22.04 更慢"
这个量级上，没有一个读数指向产品侧的版本差异。

## 60 秒空闲的磁盘增长：这次查到是谁了

`measure` 相位新增了 `facts/growth-files.txt`（空闲前后各一份目录清单，只加读数不改断言），
于是 22.04 留下的"多出 313 KB、未查明"在这里有了答案：

```
## before            ## after
364544 upanel.db     364544 upanel.db
32768  upanel.db-shm 32768  upanel.db-shm
490312 upanel.db-wal 877592 upanel.db-wal   ← +387 KB
```

**是 `upanel.db-wal` 在长**：主库文件一页没变、`-shm` 没变、代理库目录没变、journal 行数没变
（161 → 161）。也就是 SQLite 的 WAL 在 60 秒空闲里被写了约 95 页之后**还没有走到检查点**。

这条**没有结案**：

* 它不是泄漏（WAL 会在 `wal_autocheckpoint`（1000 页）或连接空闲时回收），但"空闲 60 秒还在写 WAL"
  说明有周期性写入者，每 30 秒一轮最像 `internal/operations` 的对账循环；
* 本轮**没有抓到是谁写的**——没有开 `-wal` 页级的观测，也没在空闲窗口里比对审计表行数；
* 列进 **P16.3** 专项：空闲 5 分钟，看 WAL 是否停在 1000 页内、`PRAGMA wal_checkpoint` 后是否回落，
  以及"空闲时到底有没有必要写"。

## 浏览器流程（补 `measure` 里那条 NOT RUN）

同机（归因探针重装的那份面板）、宿主 Playwright 经 `ssh -L 9443:127.0.0.1:9443`：**35 PASS / 0 FAIL / 0 SKIP**，
两次。前四次失败的成因（一次是我把隧道开在 9444 造成跨源 403、两次是登录超过前端 15 秒超时）
逐条记在 `../2026-10-27-p16.2-browser/README.md`——那里也记了为什么这是一条**环境边界**而不是产品缺陷
（`argon2id m=128MiB` 宿主 128ms / 这台机器 9.5s CPU），以及它为什么仍要进 P16.4 的已知限制。

## 收尾

* `facts/`、`logs/`、`RESULT-ALL` 是驱动跑完自动取回的原始读数；
* `console.noDatasource-run.log` 是第一次起机失败的现场：seed server 没起，cloud-init 落到
  `DataSourceNone`、没有 `upanel` 用户，白跑 987 秒（TEST_ENVIRONMENT §8.13 第四条）。
