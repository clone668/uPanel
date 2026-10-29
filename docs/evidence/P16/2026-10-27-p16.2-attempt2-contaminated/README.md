# P16.2 第二次尝试（作废）— 两个驱动同时跑同一台 VM，读数互相污染

**结论：这一轮的分相位 PASS/FAIL 计数不能引用。**
它仍然是这一轮最有用的一份证据——两个**产品缺陷**是在这里现形的（Q499、Q500），
而且两条都另有独立确认，不依赖这份污染的计数。

- 日期：2026-10-27
- 环境：QEMU / Ubuntu 22.04.5 / `-accel whpx` / SSH `127.0.0.1:2223`
- 包：`upanel-0.1.0-dev.1-linux-amd64.tar.gz`（sha256 `17ff6518…`）

## 怎么污染的（我的操作失误，不是环境问题）

第一轮驱动在 `measure` 之后要重启 VM 再跑 `boot`。`systemctl reboot` 之后 SSH 进入
"banner exchange 超时"的状态，我等了约 4 分钟没见到回音，判断"客户机卡死了"，
于是：

1. `TaskStop` 停掉驱动 —— **只杀掉了管道里的 `tail`，`sh` 主进程还活着**，
   几分钟后它自己从"VM 回来了"的分支继续往下跑；
2. 强制杀掉 QEMU 并重启 VM —— 把正在跑的 `boot`/`upgrade` 现场打断；
3. 又起了**第二个**驱动实例 —— 它在 `install` 之前会 `rm -rf /root/upanel-pkg`
   再重新解包，而第一个实例的 `failup` 相位正在用这个目录里的包做"恢复安装"。

于是日志里出现了 `sha256sum: bin/upanelctl: No such file or directory`、
`重装退出码 0: 期望 '0'，实际 '2'`、`Connection refused` —— 这些都不是产品行为。

**纪律（写进 TEST_ENVIRONMENT §8.13）**：同一条验收序列任何时刻只能有一个驱动实例；
停后台任务要确认进程真的没了（`TaskStop` 停的是管道末端）；VM 重启后 SSH 的
banner 超时不等于客户机死了，先看 QEMU 是否还在、再等。

## 这份证据里仍然可信的两件事

### 1. Q499：Ubuntu 22.04 上每一条 `/logs` 查询都是 400

`measure` 相位拿到三条红，根因是同一个：

```
FAIL  日志 limit=1000 接受: 期望 '200'，实际 '400'
响应体：{"error":{"code":"VALIDATION_ERROR","message":"journal: 查询参数不合法: Failed to parse timestamp: 2026-10-27T13:29:43.1270959Z"}}
```

被拒的是**服务端自己算出来的默认窗口**（`now-1h`），不是用户给的参数。
随后在 VM 里直接用 `journalctl` 逐条试，得到确定结论（`systemd 249.11-0ubuntu3.22`）：

| `--since=` | 结果 |
|---|---|
| `2026-10-27T13:00:00Z` | Failed to parse timestamp |
| `2026-10-27T13:00:00` | Failed to parse timestamp |
| `2026-10-27 13:00:00` | OK |
| `2026-10-27 13:00:00.123456` | OK |
| `2026-10-27 13:00:00 UTC` | OK |

即 systemd 249 只认空格分隔的日历时间；ISO 的 `T` 要到 251 才被接受。
原代码用 `time.RFC3339Nano` 拼参数，所以在 22.04 上 100% 失败、在 24.04 上 100% 正常。
这条读数与污染无关，且已用独立命令复核。修法与决定见 DECISIONS Q499。

### 2. Q500：坏包升级打印"==== 安装完成 ===="、退出码 0

`failup` 相位造的坏包（`bin/upanel-agent` 换成 `#!/bin/sh` + `exit 1`，重算
`SHA256SUMS` 让第 1 步放行）跑完了 13 步、退出码 0、打出安装完成横幅，只在第 13 步
留了一行 `注意 代理 socket 不存在：… 仍可能正常，但系统功能会报 AGENT_UNAVAILABLE`。

这条读数的可信度比它的相邻断言高：坏包装在 `$WORK/pkgbad`（不是被 `rm -rf` 过的
`/root/upanel-pkg`），它的第 1 步输出是"包内文件与 SHA256SUMS 一致"。根因是
`Type=simple` 让 `systemctl restart` 在 fork 后就返回 0，加上第 13 步只 `warn`。
修法与决定见 DECISIONS Q500。

## 这份证据里**不能**引用的部分

`RESULT-boot` / `RESULT-upgrade` / `RESULT-failup` / `RESULT-uninstall` /
`RESULT-ALL` 与 `logs/*.log`：两个驱动实例交叉写过同一批文件，`purge` 相位没有产出。
`vm-state-part1.tgz` 是 `2026-10-27T14:39Z` 从 VM 上整份取回的现场（600 多 KB 的
facts/logs 与一份 52 MB 的坏包副本），保留原样，不作结论使用。

分相位的有效读数以第三次尝试 `2026-10-27-p16.2` 为准。
