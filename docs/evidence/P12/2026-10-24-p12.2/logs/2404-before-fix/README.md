# 2404-before-fix：这次失败不是"没过"，是一次真机抓到的产品缺陷

这一组是 **2026-10-24 24.04 那轮失败的原样留档**，放在这里是为了让"为什么改代码"
这件事以后不用重新猜。修复后的完整结果见上一级的 `result.md`。

## 现象

`backup.run` 全部失败，但**快照其实写进了仓库**。失败任务的原因原文（`job-failed.json`）：

```
restic backup 失败（退出状态 0）：{"message_type":"status","seconds_elapsed":2,
  "percent_done":1,"total_files":3,"files_done":3,"total_bytes":4124,"bytes_done":4124}
```

"退出状态 0"却报失败；而同一时刻脚本独立跑一次 `restic snapshots --json`，
仓库里躺着这次运行写出来的快照（id `505ef50d…`，tags 里有
`upanel:consistency:files_only`）。整轮 **PASS=97 / FAIL=21 / NOT RUN=7**，
21 条失败全是这一个失败的下游（任务没成功 → 结果里没有 snapshot_id →
后面拿 id 去恢复、去查详情全部跟着红）。

## 根因

**进度回调跑在管道的读协程上，而回调要同步写一次 SQLite。**

- restic 的 `--json` 进度密到每几十毫秒一条：`agent-events-backup-run.txt` 里
  `op_8b50f6010` 那一次运行有 **60 条 `backup-progress` 事件**，跨度只有 4.6 秒。
- 每条事件一次 SQLite 提交，同一台虚机上次间间隔 **50—150 毫秒**——写进度比做备份还贵。
- `os/exec` 的 `cmd.Stdout` 是普通 writer 时，由读协程把管道拷过去；回调挂在这条
  路径上，读协程就被拖住，管道排不空。进程已经按 0 退出，`cmd.WaitDelay = 2s`
  到点，`Run` 返回 `exec.ErrWaitDelay`。
- `runOutcome` 当时把"不是 *exec.ExitError 的错"一律翻成"启动失败"，
  `resticFailure` 再把它压成"restic backup 失败（退出状态 0）：最后一行进度"。

于是"成功"被写成了"失败"，而且那句话会把人带去查仓库、查口令、查磁盘——全都查不到。

## 修法（三处）

1. `internal/backup/exec.go`：进度行改由**独立协程**消费（有界队列，满则丢最老一条），
   读协程只负责尽快排空管道。丢的只会是进度行；结论留在缓冲区那一份里。
2. `internal/backup/run.go`：restic 进度按"百分比走过 1% 或距上次超过 1 秒"再报一次，
   100% 必报——不再给事件表写流水账。
3. `internal/backup/exec.go`：`runOutcome` 单独认 `exec.ErrWaitDelay`，给出
   `ErrOutputIncomplete`（"输出没有读完，进程已按 0 退出，结论不完整，不等于它失败了"），
   不再混进"启动失败"。

附带一处同层隐患：`cappedBuffer` 从"保留开头、丢弃后续"改成"保留尾部"。
结论（summary / error）在最后一行，1 MiB 的进度六七分钟就写满，保开头等于把
"快照建成了"那句话丢掉——成功会变成"restic 没有报告快照 id"。

## 回归测试怎么证明它抓得住这条

`TestSlowProgressCallbackDoesNotFailTheCommand`（unix-only）真起一个子进程：
10 行进度 + 一行结论，每条回调故意睡 250ms（合计 2.5 秒，超过 WaitDelay 的 2 秒）。

- 新代码：**PASS**，命令报成功、结论行在、进度行一条不少。
- 把回调**塞回读协程**重跑同一个测试：**FAIL**，报的正是
  `backup: restic 的输出没有读完（进程已按 0 退出…）`。

两次都在 24.04 那台 VM 上用交叉编译的 linux/amd64 测试二进制实跑过。
