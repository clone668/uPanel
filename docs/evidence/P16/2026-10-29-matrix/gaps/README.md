# P16 矩阵收尾：APP-02 与 FILE-02 补成真机读数

2026-10-29，upanel-2404（Ubuntu 24.04.5，内核 `6.8.0-147-generic`）。

这一轮只干一件事：把 `docs/ACCEPTANCE_MATRIX.md` §2 里剩下三条"部分"中的**两条**用真机读数闭环。第三条（DISK-02 的 LVM/RAID 根盘）不在这里，见 §7。

脚本：`tests/integration/p16-4-matrix-gaps.sh`（可重跑）。
读数：`2404-matrix-gaps.log`，末行 `PASS 80 / FAIL 0 / NOT RUN 0`。
绑定的字节：`binaries.txt`。

---

## 1. APP-02：上游回 302，面板记什么、跟不跟

原来的状态是"部分"，缺的是**真机读数** —— 代码里写着 `newProbeClient` 不跟随重定向，但没人在真机上把它逼出来过。

这一节造了三个真上游，都是本机 python `http.server`：

| 端口 | 行为 |
|---|---|
| 19491 | 回 `302`，`Location: http://127.0.0.1:19493/stolen` |
| 19492 | 回 `302`，`Location: http://169.254.169.254/latest/meta-data/iam/security-credentials/` |
| 19493 | 金丝雀。**任何**请求都写一行到 `canary.log`，回 200 `STOLEN-SECRET` |

金丝雀是这一节的判据核心：面板若跟随了重定向，它必然被打到，`canary.log` 就不是 0 行。

读数（`2404-matrix-gaps.log`）：

| 断言 | 行 | 结果 |
|---|---|---|
| 302→金丝雀 的 `http_state` | 47 | `ok` |
| 302→金丝雀 的 `status_code` | 48 | `302` |
| 金丝雀**一次都没被请求** | 50 | `0` 行 |
| 上游确实被探过（302 不是编的） | 51 | 命中 |
| 302→云元数据 的 `status_code` | 54 | `302` |
| 302→云元数据 的 `http_state` | 55 | `ok` |
| 健康记录里没有 `169.254` | 57 | 通过 |

存档原文 `payloads/health-redir.json` / `payloads/health-meta.json` 里，`address` 仍然是 `127.0.0.1`、`url` 仍然是 `http://127.0.0.1:19491/` —— Location 里的那个地址**没有**渗进记录。

`payloads/upstream-hit-counts.txt` 是三份日志的行数：金丝雀 0，两个上游各 17（健康检查按 30 秒一拍探了整场）。

**这条现在是"已验"。** 302 落在默认的成功集合里，因此 `http_state=ok` 是**设计如此**，不是漏判：302 是一个能连上、能应答的站点，面板只如实记下状态码，不替人判断跳转好不好。

顺带一条与本条无关但必须写下来的观察：这次发现结果是 `partial`，来源是 `systemd=error`（"服务状态尚未完成第一次采集"，行 39）。这不是缺陷，是采集刚起步时的正常中间态；`local_listen=ok` 已经把要探的两个端口都给出了候选。

---

## 2. FILE-02 之一：写的时候真的撞 ENOSPC

`ErrNoSpace` 此前只在单测里被构造过。真机上要撞 ENOSPC，得让"声明时放得下、写的时候放不下"同时成立。

现场：一块 128 MiB 的 tmpfs 当可写根。为什么不是 16 MiB —— 容量预检除声明字节外还硬留 64 MiB 余量（`internal/files/staging.go` 的 `uploadReserveMargin`），根比余量还小的话，任何上传在**声明阶段**就被 413 掉，走不到写入。这个坑本轮先踩了一次（见 §6）。

步骤与读数：

| 步 | 行 | 结果 |
|---|---|---|
| 声明 8 MiB（此时 tmpfs 可用 131064 KiB） | 61-62 | `201` |
| 声明后把盘填到只剩 512 KiB | 63-65 | 现场成立 |
| PUT 8 MiB | 66 | **413**，`VALIDATION_ERROR`，`files: 空间不足: 磁盘空间不足，暂存已删除` |
| 已有版本没被改写 | 69 | sha256 不变 |
| 暂存里没有残留 `.part` | 70 | `0` |
| 上传记录留着，`state=failed` | 71-74 | 记录在，`received_bytes=0` |
| 代理日志有 WARN | 76-77 | `msg=上传失败，暂存已删除`，`reason=磁盘空间不足，暂存已删除` |

两件事值得单独说：

**一是 413 不是 500。** `http.go` 把 `ErrNoSpace` 映射成 `VALIDATION_ERROR`，前端能给出"清空间再传"的下一步；报 500 会让人去查代码。存档 `payloads/upload-enospc-put.json` 是原始响应体。

**二是"清理暂存"不等于"假装没发生"。** `.part` 删掉了（它占的正是撑爆盘的那部分），记录留下、标成 failed、`received_bytes` 归零。这样"我传过、失败了、为什么"还查得到。

---

## 3. FILE-02 之二：传一半掐断

`curl --limit-rate 1M --max-time 3` 打 8 MiB，连接在 3 秒时被掐。

| 断言 | 行 | 结果 |
|---|---|---|
| 客户端不是正常收尾 | 86-87 | `curl` 退出码 28（超时） |
| 这条上传进 `failed` | 88-89 | 通过 |
| 暂存被清掉 | 89 | 无 `.part` |
| 目标文件不存在 | 90 | 通过 |

代理侧原文（`payloads/agent-warn-lines.log` 第二行）：

```
"msg":"上传失败，暂存已删除","path":"broken.bin","reason":"传输中断（已写入 3145728 字节）：unexpected EOF"
```

3 MiB 这个数字是从掐断时刻长出来的，不是估的。

---

## 4. FILE-02 之三：2 GiB 真的推一遍

这条要回答的不是"能不能传大文件"，是**"是不是流式有界"** —— 把小文件放宽上限不算本事，2 GiB 中途不许把 2 GiB 吃进内存才算。

`max_upload_bytes` 设成 3 GiB，声明并推 2 GiB（全零，`dd` 落盘）：

| 断言 | 行 | 结果 |
|---|---|---|
| PUT 收完 | 95-96 | `200`，耗时 **292 秒** |
| `received_bytes` 等于 2 GiB | 97 | `2147483648` |
| 代理峰值 RSS | 99 | **28740 KiB**（约 28 MiB） |
| 落位 `outcome=verified` | 103-104 | 通过 |
| 落位后 sha256 一致 | 107 | 通过 |
| 这条记录随落位消失、查它是 404 | 108-110 | 通过 |
| 声明 3 GiB+1 被拒 | 111-112 | `413` / `VALIDATION_ERROR` |

RSS 是按 0.5 秒采样取峰值，判据写成"峰值必须小于 2 GiB 的 1/4（512 MiB）"。实测 28 MiB —— 传了 2 GiB，进程涨了不到 1.5 MiB。**这就是"流式有界"的读数**，不是"看起来不高"。

292 秒里有相当一部分是 TCG 软件模拟的开销（2 vCPU，无硬件虚拟化），这个数字**不能**当性能指标用，它只证明这条路径走得完。

---

## 5. 这一轮的三次运行

存档的 `2404-matrix-gaps.log` 是**第三次**运行。前两次都因为脚本自己的缺陷没跑干净，一并记下来，免得以后有人拿"一次全绿"当真：

| 次 | 结果 | 缺陷 | 修法 |
|---|---|---|---|
| 1 | `PASS 53 / FAIL 23` | ①`systemctl is-active` 的退出码让 `\|\| echo inactive` 又追加一行，两个单元断言假红；②Web 的 uid 读的是 `runuser` 外壳（root），不是真进程；③应用 id 取了 `data.app_id`（实际是 `data.id`），健康接口 404 被当成"探到了"；④ENOSPC 现场用了 16 MiB tmpfs，撞上 64 MiB 余量；⑤2 GiB 用 `--data-binary`，curl 直接 OOM | 见 §6 |
| 2 | `PASS 78 / FAIL 2` | ①停单元是异步的，`activating` 被当成没停；②`grep -c` 的结果拿去跟字符串比 | 等落定再断言；改比计数 |
| 3 | `PASS 80 / FAIL 0` | —— | 存档 |

第 1 次里那条"健康接口 404 被当成探到了"值得单独提：`jget` 取不到键时返回 `ERR:KeyError:...`，而原来的轮询判据是"值非空且不是 None"。**错误字符串满足了这个条件。** 现在的判据显式排除 `ERR:` 前缀 —— 这类缺陷的危险在于它让断言变绿，而不是变红。

---

## 6. 本轮从代码里读出来的两件事实

1. **`uploadReserveMargin = 64 MiB`**（`internal/files/staging.go:52`）。容量预检的判据是 `available < already + want + 64MiB`。推论：任何小于 64 MiB 的文件根**永远**声明不了上传。这是设计取舍（"预检说够，不代表刚好够"），不是 bug，但它决定了 ENOSPC 现场的最小尺寸 —— 记在这里，省得下一个人再从 16 MiB 试起。
2. **2 GiB 载荷不能用 `curl --data-binary @file`。** curl 会先把它读进内存，直接 `out of memory`。`-T file` 才是流式。这条对"验证流式"尤其要紧：用 `--data-binary` 的测试根本测不出服务端是不是流式。

---

## 7. 明确没验的

| 项 | 为什么 |
|---|---|
| **DISK-02：LVM/RAID 根盘上的递归保护与回避** | 本脚本不覆盖。它需要一台根文件系统在 LVM 上的机器（`P00-VM-C`），而 `H:\Code\vms\images\` 只有 cloud image、没有 server installer ISO，`layout: lvm` 那条安装路径现在走不通 —— 路线见 PROGRESS 的下一步。**同一天由 `../disk02/` 关掉**（LVM `91/0/3`、RAID1 `113/0/3`），此后矩阵里没有"部分"。 |
| 并发上传的真竞态 | 单机顺序脚本制造不出可靠的并发窗口（与 P06.2 同一取舍）。 |
| 性能 | 292 秒是 TCG 数字，不作性能结论。 |

---

## 8. 怎么重跑

```
# 主机侧
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -o /tmp/upanel-bin/upanel-agent ./cmd/upanel-agent
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -o /tmp/upanel-bin/upanel-web   ./cmd/upanel-web
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -o /tmp/upanel-bin/upanelctl     ./cmd/upanelctl
scp /tmp/upanel-bin/* tests/integration/p16-4-matrix-gaps.sh upanel@127.0.0.1:/var/tmp/

# VM 侧（root）
mkdir -p /var/tmp/upanel-bin && mv /var/tmp/upanel-{agent,web,ctl} /var/tmp/upanel-bin/
sudo BIN_DIR=/var/tmp/upanel-bin sh /var/tmp/p16-4-matrix-gaps.sh
```

参考运行时长：约 8 分钟（其中 2 GiB 上传占 5 分钟）。会自己停掉再还原 `upanel-agent` / `upanel-web` 两个 systemd 单元，会在 `/var/tmp/upanel-accept-p16-4/` 里留下现场（`KEEP_BIG=1` 可保留 2 GiB 载荷）。

还原之后的实际状态见 `binaries.txt` 末节：收尾那一刻代理是 `active`、Web 是 `activating`（脚本退出时清掉了自己的 socket，装回来的代理才刚起）。这一轮的所有读数都来自脚本自己起的进程，与这两个单元无关；要用回装的实例就先 `systemctl restart upanel-web`。
