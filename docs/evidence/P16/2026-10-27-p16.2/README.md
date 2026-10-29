# P16.2 端到端验收 · Ubuntu 22.04（2026-10-27，第 3 轮，干净）

STAGES §18 要的是「端到端验收：安装 → 重启自启 → 升级 → 失败升级可救回 → 卸载保留数据 →
带清除卸载删前备份，七个相位在**真机**上跑通，并收真实资源与延迟读数」。
这是 22.04 那半边的结果；24.04 见同目录旁的 `2026-10-27-p16.2-2404/`。

## 环境

| 项 | 值 |
|---|---|
| 机器 | VM-A，Ubuntu **22.04.5 LTS** |
| 内核 | `5.15.0-198-generic` |
| systemd | **249** (`249.11-0ubuntu3.22`) |
| 资源 | 2 vCPU / 3911 MB / 39 G 根盘 |
| 加速后端 | `-accel tcg`（宿主 whpx 在本机不稳，见 TEST_ENVIRONMENT §8.9） |
| 起点 | 从 `ubuntu-22.04-server-cloudimg-amd64.img` **重建**的新叠层盘，干净镜像只有 curl / openssl，**没有** nginx / restic / docker |
| 包 | `dist/upanel-0.1.0-dev.1-linux-amd64.tar.gz`，sha256 `5216cb31712b92855c33be7868c1654c668be8769d3719545394ad2fa9ed3cb8` |
| 驱动 | `tests/integration/p16-2-run-on-2204.sh`（宿主侧） |
| 验收脚本 | `tests/integration/p16-2-acceptance.sh`（VM 内，`sudo PHASE=…`） |

## 结果：七个相位全通，0 失败

| 相位 | PASS | FAIL | NOT RUN |
|---|---|---|---|
| install | 40 | 0 | 0 |
| measure | 14 | 0 | 3 |
| boot（真重启之后） | 14 | 0 | 0 |
| upgrade（同包再装一次） | 22 | 0 | 0 |
| failup（坏包升级失败 → 好包救回） | 21 | 0 | 0 |
| uninstall（默认卸载 → 再装回） | 23 | 0 | 0 |
| purge（`--purge-data --purge-config`） | 14 | 0 | 0 |
| **合计** | **148** | **0** | **3** |

原始文件：`RESULT-ALL`（七行，逐相位）、`RESULT-<phase>`、`phase-<phase>.log`（逐条断言）、
`facts/`（机器读数）、`logs/`（各次 install/uninstall 的完整输出）。

`measure` 的三条 NOT RUN 及处置：

| NOT RUN | 为什么 | 处置 |
|---|---|---|
| 上传 2 GiB 上限 | P06 已有该限额读数；本轮不造 2 GiB 文件 | 保留 NOT RUN，指向 P06 |
| 文本编辑 1 MiB 上限 | 属 P06 范围 | 保留 NOT RUN，指向 P06 |
| 浏览器完成主要用户流程 | 干净 cloud image 里没有浏览器 | **已补测**：`../2026-10-27-p16.2-browser/`，同机同包，宿主机经 SSH 隧道跑 Playwright，**PASS 35 / FAIL 0** |

## 真实读数（不是 mock）

**时间**

| 读数 | 值 |
|---|---|
| 安装耗时（13 步，含等代理起来） | `install_ms=30020`（30.0 s） |
| 重启 → Web 单元 active | `boot_to_web_seconds=60`（btime→ActiveEnterTimestamp） |
| 同包重装 = 升级耗时 | `upgrade_seconds=49` |

**常驻内存**（`RSS: web=288204KiB agent=25148KiB`）

| 进程 | RSS |
|---|---|
| `upanel-web` | 288 MB |
| `upanel-agent` | 25 MB |

**接口延迟**（VM 内 curl，同一台机器上自测，无网络噪声）

| 接口 | p50 | p95 | max |
|---|---|---|---|
| `/api/v1/health/live` | 46.2 ms | 53.2 ms | 54.4 ms |
| `/api/v1/system/summary` | 51.7 ms | 63.1 ms | 83.2 ms |

两条 p95 都远低于 2 s 的门限。注意这是在**单线程 TCG** 上测的，比裸机慢，属于保守读数。

**容量限制（逐条打真接口）**

| 限制 | 判据 | 读数 |
|---|---|---|
| 请求体 1 MiB | 发 >1 MiB 的请求 | **413** |
| 日志单页条数上限 1000 | `limit=5000` | **400**（越界被拒） |
| 同上 | `limit=1000` | **200**，单页 76 条 / 21039 字节（≤1000 条、≤1 MiB） |
| 事件流每用户 4 条 | 并发开第 5 条 SSE | **429**；断开后名额释放，又能开一条 |

**空闲期没有失控增长**（60 秒，`facts/growth-before.txt` / `growth-after.txt`）

| 项 | 前 | 后 |
|---|---|---|
| 面板库文件 `/var/lib/upanel/upanel.db` | 364544 B | **364544 B（没变）** |
| `/var/lib/upanel`（整目录 `du`） | 595080 B | 908200 B |
| `/var/lib/upanel-agent`（整目录） | 634272 B | 634272 B |
| journal 行数 | 155 | 155 |

60 秒空闲里 journal 一行没长、代理库目录没长、**面板库文件本身也没长**（页数没变）。
整目录多出来的 313 KB 是 `upanel.db` **之外**的东西在长大——最可能是 `-wal` 文件，
也就是 SQLite WAL 尚未检查点回到主库。**这一条我没查到根因就当没结束**：本轮没有留下
`ls -l /var/lib/upanel` 的快照，无法证明它是 `-wal` 而不是别的文件，所以记为**未查明**，
列进 P16.3 的专项复核（`measure` 相位该顺带落一份目录清单）。它不构成本轮失败：断言
只要求"60 秒内没有失控增长"，量级 313 KB 不是泄漏，但"没查明"就是没查明。

## 这一轮抓到的两个**产品缺陷**（都已修，都有回归）

### Q499 — 22.04 上每一条 `/logs` 查询都 400

`queryArgs` 用 `time.RFC3339Nano` 拼 `journalctl --since/--until`，而 systemd **249** 的时间
解析器只认空格分隔的 `YYYY-MM-DD HH:MM:SS`，ISO 8601 的 `T` 要 systemd 251 才接受。
被拒的是**服务端自算的默认窗口**，所以 22.04 上日志页一条都读不出来，24.04 上全好——
这种"只跟客户机版本有关"的差异，只测一个版本永远看不见。

修法：`journalTimestamp()` 输出 `2006-01-02 15:04:05.000000 UTC`（微秒精度，显式 UTC 后缀），
回归测试钉住字符串形状与非 UTC 输入的归一化。**本轮在 22.04 上验证通过**：
`limit=1000` 拿到 200 与 76 条真实日志，`limit=5000` 仍然 400。

### Q500 — 坏包升级打出了"安装完成"，退出码 0

代理单元是 `Type=simple`，`systemctl restart` 在 fork 之后就返回 0，于是一个立刻 `exit 1`
的二进制照样让第 12 步显示"已 enable 并启动"；第 13 步原来只 `warn` 缺 socket，于是
`==== 安装完成 ====` 照打、退出码 0。这是 STAGES §1.8 明令禁止的**伪成功**。

修法：第 13 步等 `/run/upanel/agent.sock` 与单元 active（最多 30 秒），等不到就 `die`，
并把代理 journal 贴出来。**本轮在 22.04 上验证**：`failup` 相位的七条断言全绿——

* 坏包升级退出码非 0；
* 打出 `安装没有完成` 横幅；
* 失败点在第 13 步（`代理 30 秒内没有起来`），不是第 1 步校验失败（坏包是自洽的）；
* 喊停时点名 `AGENT_UNAVAILABLE` 并贴出 `journalctl -u upanel-agent`；
* **没有**打出 `==== 安装完成 ====`；
* 失败前备份已完成且可自校验（`MANIFEST.sha256` 校验通过），重跑好包能救回，数据与密钥不变。

## 过程中的两次作废（诚实记录）

| 目录 | 作废原因 |
|---|---|
| `../2026-10-27-p16.2-attempt1-scriptbug/` | 验收脚本自身的缺陷（`$(do_login …)` 开子 shell 丢了 CSRF；`host_caps.ready` 字段位置读错）。产品无缺陷，5 条失败全是脚本 bug，已修 |
| `../2026-10-27-p16.2-attempt2-contaminated/` | 停任务只杀掉管道末端的 `tail`，主驱动仍在跑，我又起了第二个驱动，两个驱动在同一台 VM 上撞车。分相位计数整份作废，但 Q499/Q500 两条读数可信 |

这一轮（第 3 轮）是唯一一次干净全通。驱动的两次调用（install+measure 一段；boot..purge
一段，`KEEP_LOG=1` 续写同一个 `run.log`）在 `run.log` 里能看到分隔行——第一段末尾那次重启
竞态导致 `boot` 之后的五个相位 ssh 全部 `banner exchange` 超时，`RESULT-*` 为 0 字节；
补跑前把驱动改成"先等客户机掉线、再等它回来"（TEST_ENVIRONMENT §8.13），随后一次过。

## 这份证据**不能**证明什么

* 不能证明 24.04 上也一样——那份单独跑、单独收（`../2026-10-27-p16.2-2404/`）；
* 单线程 TCG 的时间读数是**下界**，不能当裸机性能指标；
* 上传 2 GiB、1 MiB 文本编辑、磁盘分区/格式化、网络断连这些破坏性/大体积项不在本轮，
  各有自己的阶段；
* 浏览器那条是宿主机经隧道跑的（见 `../2026-10-27-p16.2-browser/README.md` 的边界说明），
  不是"VM 内浏览器"。

## 复现

```sh
# 宿主上（Git Bash），VM 已用 cloud image 重建并起来
PHASES=install,measure,boot,upgrade,failup,uninstall,purge \
  sh tests/integration/p16-2-run-on-2204.sh
```

驱动会自己重启 VM（在 `boot` 相位之前），跑完把 `facts/`、`logs/`、`RESULT-ALL` 取回本地。
