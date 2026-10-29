# P16.2 七相位在**最终交付字节**上重跑（2026-10-28，包 `3f3b40c1…`）

## 为什么又跑了一遍

这一天的字节被换了四次，每一次都按 Q510 的规矩重取读数：

| 包 | 变了什么 | 这一份读数的位置 |
|---|---|---|
| `fbad9fc9…` | P16.4 第一轮（文档复核前） | `P16/2026-10-28-p16.4/prelim-docdelta/`（作废，只当旁证） |
| `af6d9102…` | P15.4 的两处页面修复 | 无七相位读数（当时只跑了页面链路） |
| `b1d913e7…` | 随包文档（C12、支持矩阵） | `P16/2026-10-28-p16.2-refreeze/`（**已被本目录取代，留作可重复性旁证**） |
| **`3f3b40c1…`** | **Q514：会话层不再把“浏览器放弃的请求”记成 500“服务内部错误”** | **本目录 = 最终读数** |

最后这次是代码改动（`internal/api/auth.go`），不是文档，`bin/` 变了，所以必须重跑。

## 1. 环境与字节

| 项 | 值 |
|---|---|
| 机器 | Ubuntu 22.04.5 LTS / 5.15.0-198-generic / 2 vCPU / 3911 MB / 单线程 TCG |
| 系统盘 | **从 cloud image 重建**（`qemu-img create -f qcow2 -F qcow2 -b ../images/ubuntu-22.04-server-cloudimg-amd64.img system.qcow2 40G`），数据盘 a/b/c 重建为空白 |
| 基底镜像校验 | `012d81fa…835c`，与 `images/SHA256SUMS-22.04` 一致（`facts-vm-reset.txt`） |
| 重建后、跑之前 | `cloud-init status: done`；`docker` 与 `upanelctl` 都不在；`/etc/upanel`、`/var/lib/upanel`、`/usr/local/share/upanel`、`/opt/upanel` 全不存在 |
| 发布包 | `upanel-1.0.0-linux-amd64.tar.gz` sha256 `3f3b40c11247ea1b15aba024ddb3571fada3a69291465eb7e43242b2e9d6daf0`，44154866 字节；arm64 `33fc5add33befedfeb4429988b35bb1846be6cf1e0f778ae02bdfa0b3e643748` |
| VERSION | `1.0.0` / `commit: 8e52b36-dirty` / `build_date: 2026-10-28T05:23:36Z` / `arch: amd64` |
| 包内事实 | 28 条目（22 文件 + 6 目录）；6 个 0755；`sha256sum -c SHA256SUMS` 在 VM 内通过（`facts-package.txt`） |
| 驱动 | `tests/integration/p16-2-run-on-2204.sh` + `tests/integration/p16-2-acceptance.sh`，未改 |

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

驱动自己打了 `== 全部相位无失败 ==`（`run.log` 末尾）。逐相位输出 `phase-*.log`、汇总 `RESULT-ALL`、
客机现场 `facts/` 与 `logs/`。

3 条 NOT RUN 按设计：`上传 2 GiB 上限`、`文本编辑 1 MiB 上限`、`浏览器完成主要用户流程`——
前两条要真实的 2 GiB 上传与 1 MiB 编辑器交互，第三条属于 `P16/2026-10-27-p16.2-browser/`。

**四份字节、三次七相位，逐相位读数完全一致**（47/14/14/22/21/23/14）。这也说明 P15.4 的页面修复与
Q514 的错误分类修复都没有碰到安装/升级/卸载面。

## 3. 这一轮没有证明什么

- **arm64 只打包没跑**：没有第二台 arm 机器，端到端仍 NOT RUN。
- **掉电语义仍 NOT RUN**（P16 的整条掉电演练没做，这一轮没改变它）。
- **24.04 的读数在另一个目录**：`P16/2026-10-28-p16.2-2404-final/`（同一份字节）。
- `boot` 相位验的是"重启后单元能起来"，不是断电一致性。

## 4. 文件清单

| 文件 | 内容 |
|---|---|
| `run.log` | 驱动全过程（传输、校验、相位切换、重启等待、取回） |
| `RESULT-ALL` / `RESULT-<相位>` | 每相位计数与失败/NOT RUN 名单 |
| `phase-<相位>.log` | 客机侧逐条断言 |
| `facts-package.txt` | 两包 sha256、VERSION、`tar tzvf` 全清单、`SHA256SUMS` 条数 |
| `facts-vm-reset.txt` | 基底镜像校验值、重建命令、重建后客机现场 |
| `facts/` `logs/` | 驱动取回的机器现场与安装/升级/卸载/回切日志 |
