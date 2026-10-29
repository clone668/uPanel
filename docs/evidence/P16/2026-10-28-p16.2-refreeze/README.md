# P16.2 七相位在**重新冻结的 1.0 字节**上重跑（2026-10-28）

> **这份读数已被取代（2026-10-28 晚些时候）。** 它跑在 `b1d913e7…` 上，之后 P15.4 的
> 最终字节验收抓到一个日志误判缺陷（浏览器 abort 的请求被记成 500“服务内部错误”，
> Q514），修它动了 `internal/api/auth.go`，`bin/` 因此重打。按 Q510 的规矩，读数只对
> 跑它的那份字节成立：**最终读数在 `docs/evidence/P16/2026-10-28-p16.2-final/`（包
> `3f3b40c1…`）**。这一份留着当**可重复性旁证**——同一套相位在两份相邻字节上都是
> 155/0/3，逐相位一致。

## 为什么要再跑一遍

P16.4 的 155/0/3 读数是**旧字节**上的。之后 P15.4 的真机验收抓出两个前端缺陷
（`StatePanel` 丢掉空状态标题、`DeploymentsView.afterSuccess` 删完还去读那份部署）
并修掉，前端产物随 `internal/webui/dist` 进了 `upanel-web` 二进制，发布包字节因此变了。
Q510 立的规矩是：**读数只对跑它的那份字节成立**。所以文档改完再打一次包，
在最终字节上把七相位原样重跑——不重跑就没资格说 1.0 这个包能装、能升、能卸。

## 1. 环境与字节

| 项 | 值 |
|---|---|
| 机器 | Ubuntu 22.04.5 LTS / 5.15.0-198-generic / 2 vCPU / 3911 MB / 单线程 TCG（tcg, max） |
| 系统盘 | **从 cloud image 重建**，不是上一轮跑脏的盘接着用：`qemu-img create -f qcow2 -F qcow2 -b ../images/ubuntu-22.04-server-cloudimg-amd64.img system.qcow2 40G`；数据盘 a/b/c 重建为空白（8G/4G/4G） |
| 基底镜像校验 | `012d81fade7e8ff4428fc35dfeee711d66d1f47d5ea9c5c8ced96a85b959835c`，与 `images/SHA256SUMS-22.04` 一致（`facts-vm-reset.txt`） |
| 重建后、跑之前 | `up 5 min`、`cloud-init status: done`、`/etc/upanel` `/var/lib/upanel` `/usr/local/share/upanel` `/opt/upanel` 全不存在、`docker` 与 `upanelctl` 都不在 |
| 发布包（amd64） | `upanel-1.0.0-linux-amd64.tar.gz`，sha256 `b1d913e7d415022eb5f5b0b1661a489e5ced73c1ad87dcd2f1b1f9b6b7ba7311`，44155044 字节 |
| 发布包（arm64） | sha256 `17f6de3c1ad00600d97aaec3285ac292fd4e89b5f5679ff737f122a958a58119`，40945682 字节（本轮**没跑**，见第 4 节） |
| 包内事实 | 28 条目 = 22 文件 + 6 目录；6 个 0755（3 个二进制 + 3 个脚本，权限取自 `tar tzvf`）；`sha256sum -c SHA256SUMS` 在 VM 内通过（`facts-package.txt`） |
| VERSION | `1.0.0` / `commit: 8e52b36-dirty` / `build_date: 2026-10-28T04:48:48Z` / `arch: amd64` |
| 驱动 | `tests/integration/p16-2-run-on-2204.sh`（宿主侧）+ `tests/integration/p16-2-acceptance.sh`（客机侧），原样未改 |

## 2. 结果：155 / 0 / 3

| 相位 | PASS | FAIL | NOT RUN |
|---|---|---|---|
| install | 47 | 0 | 0 |
| measure | 14 | 0 | 3 |
| boot（真重启之后） | 14 | 0 | 0 |
| upgrade | 22 | 0 | 0 |
| failup（升级失败回切） | 21 | 0 | 0 |
| uninstall | 23 | 0 | 0 |
| purge | 14 | 0 | 0 |
| **合计** | **155** | **0** | **3** |

驱动自己打了 `== 全部相位无失败 ==`。逐相位输出在 `phase-*.log`，汇总在 `RESULT-ALL`，
`facts/` 与 `logs/` 是驱动取回来的客机现场。

3 条 NOT RUN 是设计如此，不是漏跑：`上传 2 GiB 上限`、`文本编辑 1 MiB 上限`、
`浏览器完成主要用户流程`——前两条要有真实的 2 GiB 上传与 1 MiB 编辑器交互，
第三条属于 P16.2-browser 那一轮（`P16/2026-10-27-p16.2-browser/`），不在本驱动的相位里。

**与 P16.4（旧字节）逐相位一致**：47/14/14/22/21/23/14 完全相同，合计同样 155/0/3。
这说明两件事：那三个前端修复没有碰到安装/升级/卸载面；这套相位在单线程 TCG 上也是可重复的。

## 3. 这一轮没有证明什么

* **arm64 只打包，没跑。** arm64 包打出且带执行位自检通过，但没有第二台 arm 机器装机，
  端到端仍是 NOT RUN（沿用 P16.4 的口径）。
* **掉电语义没验。** 整个 P16 的掉电演练仍是 NOT RUN，这一轮没有改变它。
* **24.04 的读数不在这份目录里。** 24.04 是单独一轮（同一天、同一份字节），
  见 `P16/2026-10-28-p16.2-2404-final/`。
* **`boot` 相位验的是"重启后单元能起来"**，不是断电后的一致性。

## 4. 文件清单

| 文件 | 内容 |
|---|---|
| `run.log` | 驱动全过程（含传输、校验、每相位切换、取回） |
| `RESULT-ALL` / `RESULT-<相位>` | 每相位 PASS/FAIL/NOT RUN 计数与失败名单 |
| `phase-<相位>.log` | 客机侧逐条断言 |
| `facts-package.txt` | 发布包 sha256、VERSION、`tar tzvf` 全清单、`SHA256SUMS` 条数 |
| `facts-vm-reset.txt` | 基底镜像校验值、重建命令、重建后客机现场 |
| `facts/` `logs/` | 驱动取回的机器现场与日志（权限、单元、journal 片段） |
