# P16.2 七相位在最终交付字节上的 24.04 读数（2026-10-28，包 `3f3b40c1…`）

## 0. 一句话结论

**155 PASS / 0 FAIL / 3 NOT RUN**，与 22.04 逐相位完全相同（install 47、measure 14、boot 14、
upgrade 22、failup 21、uninstall 23、purge 14）。中途 `failup` 出现过一次红，根因是**断言自己的
采样竞态**（Q515），不是产品缺陷；断言收紧后在同一份字节上重跑 `install,failup` 得 47/0/0 + 21/0/0。

## 1. 环境与字节

| 项 | 值 |
|---|---|
| 机器 | Ubuntu 24.04.5 LTS / 6.8.0-147-generic / **systemd 255.4-1ubuntu8.18** / 2 vCPU / 3915 MB / `-accel tcg,thread=multi` |
| 系统盘 | **从 cloud image 重建**（不是接着上一轮跑脏的盘跑）：`qemu-img create -f qcow2 -F qcow2 -b ../images/ubuntu-24.04-server-cloudimg-amd64.img system.qcow2 40G`，数据盘 a/b/c 重建为空白 |
| 基底镜像校验 | `04e8f2461a16a44e78a370284471f29616cdd9baf1893df25807b71f985b6c94`，与 `images/SHA256SUMS` 一致（`facts-vm-reset.txt`） |
| 重建后、跑之前 | `up 2 min` / `cloud-init status: done` / `docker`、`upanelctl` 都不在 / `/etc/upanel` 不存在 |
| 发布包 | `upanel-1.0.0-linux-amd64.tar.gz` sha256 `3f3b40c11247ea1b15aba024ddb3571fada3a69291465eb7e43242b2e9d6daf0`（**与 22.04 那份逐字节同一个包**） |
| 驱动 | `tests/integration/p16-2-run-on-2204.sh`（脚本名带 2204 是历史原因，两平台通用）+ `tests/integration/p16-2-acceptance.sh` |

## 2. 结果

| 相位 | PASS | FAIL | NOT RUN | 备注 |
|---|---|---|---|---|
| install | 47 | 0 | 0 | 首轮；收紧断言后又重跑一次，仍 47/0/0 |
| measure | 14 | 0 | 3 | 三条 NOT RUN 与 22.04 同名：`上传 2 GiB 上限`、`文本编辑 1 MiB 上限`、`浏览器完成主要用户流程` |
| boot（真重启之后） | 14 | 0 | 0 | 25 秒下线 + 真重启；`boot_to_web_seconds=75`、`upanel-agent` 6.160s / `upanel-web` 5.021s（`facts/boot.txt`） |
| upgrade | 22 | 0 | 0 | |
| failup（升级失败回切） | 21 | 0 | 0 | **首轮 20/1**，那条红见第 3 节；收紧断言后重跑 21/0/0 |
| uninstall | 23 | 0 | 0 | |
| purge | 14 | 0 | 0 | |
| **合计** | **155** | **0** | **3** | |

驱动在收紧断言后的那一段结尾自己打了 `== 全部相位无失败 ==`（`run.log`）。
`RESULT-ALL` 里两段读数都在（先 `failup PASS=20 FAIL=1`，后 `failup PASS=21 FAIL=0`），没有覆盖。

## 3. 那条红：断言采样竞态，不是产品缺陷（Q515）

首轮 `failup` 的这一条红了：

```
FAIL  坏包让代理服务起不来（这正是失败现场）: 不该等于 'active'
```

同一相位里另外六条全 PASS——`坏包升级没有成功（退出码非 0）`、`失败发生在第 13 步（代理没起来）`、
`喊停时点名了 panel 会只剩 AGENT_UNAVAILABLE`、`喊停时贴出了代理的 journal`、`没有打出安装完成横幅`、
`不是第 1 步校验失败`。也就是说**安装器判得没错**，是这条断言自己采到了瞬时状态。

从客机 journal 按微秒取同一段窗口（`journal-restart-cycle.txt`、`active-window-analysis.txt`）：
坏包是个 `#!/bin/sh` + `exit 1`，进程活不过几毫秒，但单元是 `Type=simple`，`Started` 一打就进 `active`，
而"主进程 exit 1"要 **0.234~0.400 秒**之后才被 systemd 记账（11 轮实测）；`Restart=on-failure` +
`RestartSec=3s` 又让它每约 3.4 秒重来一轮 → **active 占空比约一成**，单次 `systemctl is-active`
有约 10% 的概率正好落在里面。

修的是断言，不是产品：判据换成两件都不吃这个竞态的事——① `/run/upanel/agent.sock` 不存在
（"面板用不了代理"的直接判据，坏包永远造不出它）；② 隔 1.2 秒采两次 `is-active`，只有**连续两次**
都是 active 才算真活着。断言条数不变（failup 仍是 21，跨平台的逐相位数字才对得上）。
收紧后重跑的那一次输出是：

```
PASS  坏包让代理服务起不来（这正是失败现场）: socket 不在，is-active 采样 'activating'/'activating'
```

两次都采在自动重启的间隙里——正是这个单元该有的样子。

**改动只在 `phase_failup` 这一段里**，其余五相位（measure / boot / upgrade / uninstall / purge）
是同一次七相位里跑出来的，没有因为这条断言重跑，也没有别的改动。

## 4. 这一轮没有证明什么

- **arm64 只打包没跑**：没有第二台 arm 机器，端到端仍 NOT RUN。
- **掉电语义仍 NOT RUN**：`boot` 验的是"重启后单元能起来"，不是断电一致性。
- **single-thread TCG 那条旧红没有复现**：2026-10-27 那轮用的 `-accel 'tcg,thread=single' -cpu max`，
  `boot` 相位红了一条；这一轮用 `-accel tcg,thread=multi`、`-smp 2`，`boot` 全绿。两轮的 QEMU 参数
  不同，所以**不能说"那条旧红已经修好"**，只能说这一种参数下没出现。
- **页面链路没在这台机器上重跑**：43/43 的页面读数在 22.04 上取（`P15/2026-10-28-p15.4/`），
  24.04 的浏览器流程仍是 `P16/2026-10-27-p16.2-browser/` 那一轮。

## 5. 文件清单

| 文件 | 内容 |
|---|---|
| `run.log` | 驱动全过程（传输、校验、相位切换、重启等待、取回），两段：首轮七相位 + 收紧断言后的 `install,failup` |
| `RESULT-ALL` / `RESULT-<相位>` | 每相位计数与失败/NOT RUN 名单（`RESULT-*` 是最后一段的） |
| `phase-<相位>.log` | 客机侧逐条断言 |
| `facts-package.txt` | 两包 sha256、VERSION、`tar tzvf` 全清单、`SHA256SUMS` 条数 |
| `facts-vm-reset.txt` | 基底镜像校验值、重建命令、重建后客机现场 |
| `journal-restart-cycle.txt` | 坏包窗口的 journal（微秒精度，`Started` / `Main process exited` / 重启计数） |
| `active-window-analysis.txt` | 从上面那份算出来的 11 轮 active 时长与结论 |
| `run1-pre-tighten/` | **收紧断言之前**的 failup 读数与日志（那条红就在这里面） |
| `facts/` `logs/` | 驱动取回的机器现场与安装/升级/回切/卸载/清空日志 |
