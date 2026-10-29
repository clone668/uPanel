# P16.2 第一次尝试（作废）— 验收脚本自己的子 shell 缺陷

**结论：这一轮的读数不成立，因为仪器坏了。产品在这个相位没有缺陷。**
留档的原因不是"我们跑过"，而是这 5 条失败长得很像产品缺陷，实际上全是脚本自己的错——
以后谁看到 `FAIL 管理员能登录: 期望 '200'，实际 ''` 时能少走一遍弯路。

- 日期：2026-10-27
- 环境：QEMU / Ubuntu 22.04.5 / kernel 5.15.0-198-generic / `-accel whpx` / SSH `127.0.0.1:2223`
- 包：`upanel-0.1.0-dev.1-linux-amd64.tar.gz`（sha256 `17ff6518…`，commit `8e52b36`）
- 相位：`PHASE=install`，读数 `PASS=34 FAIL=5 NOT RUN=0`
- 原样输出：`phase-install.log`、`RESULT-install`、`run.log`

## 5 条失败与真实原因

| 失败断言 | 真实读数 | 原因 |
|---|---|---|
| 管理员能登录 | 期望 `200`，实际 `''` | `$(do_login …)` 会开**子 shell**。`do_login` 把状态码写进 `LOGIN_CODE`、把令牌写进 `CSRF`，两个赋值都发生在子 shell 里，出了括号就没了。于是外面读到空串。 |
| 登录拿到了 CSRF 令牌 | 期望非空，实际空 | 同上 |
| host_caps.ready 为真 | `ERR:KeyError:'ready'` | 断言写的是 `d["data"]["host_caps"]["ready"]`。实际 `ready` 在**外层** `data` 上，`data.host_caps` 本身就是报告（`generated_at`/`items`/`partial`/`issues`），没有嵌套的 `ready`。 |
| 建了标记账号 | 期望 `201`，实际 `403` | CSRF 为空 → 面板按设计拒绝所有写动作。**这是产品在正常工作**，不是缺陷。 |
| 库里至少有两个账号 | `False` | 标记账号没建成，所以只有一个账号。 |

## 修法

1. `do_login` 改成"写在全局变量里、语句式调用"，5 处调用点全部从 `$(do_login …)` 改成
   `do_login …` + 读 `$LOGIN_CODE`。脚本里留了注释说明这个坑。
2. `ready` 断言改到 `d["data"]["ready"]`，并补一条 `d["data"]["host_caps"] is not None`。

修完重跑（第三次尝试，见同级目录 `2026-10-27-p16.2`）`PHASE=install` 拿到
`PASS=40 FAIL=0`。

## 这一轮仍然有效的读数

不是全部作废：这批断言与登录无关，读数是真的。

- 安装器 `13` 步全走到、退出码 0、打出完成横幅；
- 二进制 `0755 root:root`、配置 `0640 root:upanel-web`、`agent.key 0600`、
  面板库属主是 `upanel-web`、代理数据目录 `0700`、代理 socket `0660 root:upanel-web`；
- 两个单元 `enabled` + `active`，Web 进程不是 root；
- 端口上服务的正是装进去的那张证书（文件指纹与握手指纹逐字相同）；
- 能力清单 21 条，`nginx` 在这台干净机器上是 `absent`（不是 `unknown`）。
