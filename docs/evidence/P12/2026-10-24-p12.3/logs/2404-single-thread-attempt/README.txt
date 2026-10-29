24.04 单线程 TCG 验收机的第一次 P12.3 完整尝试（2026-10-25 01:40–进行中）

结论：这次尝试不作为通过证据。它在第 1 节 SFTP 基线备份的预检处变红：

  FAIL  network-baseline-preflight-allowed: 期望 'True'，实际 'False'
  blocker: BACKUP_SFTP_UNREACHABLE / context deadline exceeded

现场不是“连不上”。sshd 日志（sshd-0152-0154.txt）显示：

  01:52:32  Connection closed by 127.0.0.1 port 56384 [preauth]
  01:53:01  Connection closed by authenticating user p12sftp ... [preauth]
  01:53:04  Accepted publickey for p12sftp ... ED25519
  01:53:29  代理预检返回 allowed=false

前两条连接各在 15 秒后以 [preauth] 关闭；第三条在 01:53:04 已经认证成功，
25 秒后（01:53:29）45 秒的 backupRunPlanBudget 见底，ProbeRemote 只拿到
context deadline exceeded。同一台机器上，root 手动跑一次 `sftp` + `df`
实测 11.7 秒（见 Q361 记录），说明主机可达，只是单次握手就在十几秒一档。

根因是产品把“慢但活着”的机器判成了不可达：拨号上限 15 秒贴着这台 TCG 虚机的
单次握手时间，网络层失败还要再来一轮，45 秒预检预算不够第三次握手后的 statvfs。

修法（本轮源码）：DefaultDialTimeout 15→30 秒，backupPlanBudget / backupRunPlanBudget
40/45→75 秒，backupStateBudget 45→75 秒；仍然小于 Web 的 PreflightAgentTimeout 90 秒，
也仍然只接受登记过的主机密钥指纹。修完后重新构建、重新跑完整验收；本目录只保留
这次失败现场，不混进最终通过结果。
