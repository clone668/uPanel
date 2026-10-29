# PROXY-02 更正：从「部分」改成「已验」，并把全部引用它的地方一起改。
# 只动 .md；每一处替换都断言命中次数，命中不是 1 就整体放弃，不写盘。
import sys

sys.stdout.reconfigure(encoding='utf-8')


def load(p):
    return open(p, encoding='utf-8').read()


def save(p, s):
    open(p, 'w', encoding='utf-8', newline='\n').write(s)


def rep1(s, old, new, tag):
    n = s.count(old)
    assert n == 1, '%s: 命中 %d 次\n%r' % (tag, n, old[:80])
    return s.replace(old, new)


def splice(s, start_marker, end_marker, new_block, tag):
    a = s.index(start_marker)
    b = s.index(end_marker, a) + len(end_marker)
    assert s.count(start_marker) == 1, '%s: 起始标记出现多次' % tag
    return s[:a] + new_block + s[b:]


# ---------------------------------------------------------------- 矩阵
M = 'docs/ACCEPTANCE_MATRIX.md'
s = load(M)

s = rep1(s,
        '| PROXY-02 | WebSocket、证书、私钥访问 | **部分** | 真机（子系统） | '
        '`P10/2026-10-23-p10.3/run.log` §5、§6、§10 | 见 §2.4——**WebSocket 握手实测是 NOT RUN** |',
        '| PROXY-02 | WebSocket、证书、私钥访问 | **已验** | 真机（双平台） | '
        '`P10/2026-10-23-p10.4/logs/{2204,2404}-core-scenario.log` §4（WS）、§6（wss/TLS/指纹）、'
        '§8（重启后仍在）；`P10/2026-10-23-p10.3/run.log` §5、§6、§10（私钥） | '
        '两台都 `PASS WS 握手返回 101（Upgrade 穿过了 nginx）`；wss 101 + 12 条推流 + echo 回得来 + '
        'TLS ≥ 1.2 + 对端证书指纹与账本一致；重启后 wss 仍 12 条；§10 私钥逐出口搜索 0 命中 |',
        'matrix PROXY-02 row')

s = rep1(s, '合计：**已验 27 条 / 部分 4 条 / 未验 0 条**（共 31 条）。',
        '合计：**已验 28 条 / 部分 3 条 / 未验 0 条**（共 31 条）。'
        '（2026-10-30 把 PROXY-02 从「部分」改成「已验」——原判引错了证据，见 §2.4。）',
        'matrix tally')

s = rep1(s, '## 2. 四条"部分"逐条说清楚缺什么',
        '## 2. 三条"部分"逐条说清楚缺什么（原来的第四条见 §2.4，已更正为已验）',
        'matrix §2 heading')

s = rep1(s, '这四条是本文件存在的理由。它们不是失败，是**没验全**——把它们写成"已验"才是问题。',
        '这三条是本文件存在的理由。它们不是失败，是**没验全**——把它们写成"已验"才是问题。',
        'matrix §2 intro')

s = splice(s, '### 2.4 PROXY-02（WebSocket、证书、私钥访问）',
           '要补：换一个支持 Upgrade 的上游（`websocat` 或几行 Go），跑一次真握手。',
           '''### 2.4 PROXY-02：不是"部分"，是本文件当初引错了证据（2026-10-30 更正）

初版把 PROXY-02 定成**部分**，依据是 `P10/2026-10-23-p10.2/run.log` 的 NOT RUN 段：
「WebSocket 握手实测：测试上游（python http.server）不支持 Upgrade；只验配置里 Upgrade/Connection 成对出现」。
**那句话本身没错，错在拿它当这一条的终点。** 同阶段的 P10.4 换过了上游，在两台机器上跑完了真握手：

| 子句 | 初版判定 | 更正后 | 证据 |
|---|---|---|---|
| WebSocket 正常代理 | 未验（NOT RUN） | **已验** | `P10/2026-10-23-p10.4/logs/{2204,2404}-core-scenario.log` §4「WebSocket 不断流」：`PASS WS 握手返回 101（Upgrade 穿过了 nginx）` |
| 证书能正常代理 | 已验 | 已验 | 同文件 §6：wss 101、`TLS 1.2 及以上`、对端指纹与账本一致 |
| 私钥不泄露 | 已验 | 已验 | `P10/2026-10-23-p10.3/run.log` §10「私钥保护：所有出口都搜一遍」，176 项 0 失败 |
| 重启之后仍然成立 | —（初版没列这个子句） | 已验 | 同 P10.4 文件 §8：重启两个单元之后 wss 仍 12 条不断流 |

**当初为什么会漏，要写清楚**：P10.2 的 NOT RUN 是真的——那一轮的测试上游确实不支持 Upgrade。
但那句话是**关于 P10.2 那个脚本**的，抄进账本之后，后面几轮只检查"有没有 NOT RUN 段"，
没有检查"下一轮有没有把它补上"。P10.4 的 WS 上游与客户端就在
`tests/integration/p10-4-ws-upstream.py` 与 `tests/integration/p10-4-ws-through-proxy.py`。

结论：PROXY-02 是**已验**。教训留在这里，不删——**NOT RUN 是一张待办，不是一条定论。**''',
           'matrix §2.4 block')

s = rep1(s, '27 条有完整的真机读数，\n4 条有部分读数且缺口写清楚了',
        '28 条有完整的真机读数，\n3 条有部分读数且缺口写清楚了',
        'matrix §4 proven')

s = rep1(s, '''* **"部分"的四条不能加总成"已验"**。DISK-02 的 LVM/RAID 根盘、FILE-02 的 ENOSPC、
  APP-02 的重定向、PROXY-02 的 WS 握手——四条都是**现场没造出来**，不是"造出来了但过了"。''',
        '''* **"部分"的三条不能加总成"已验"**。DISK-02 的 LVM/RAID 根盘、FILE-02 的 ENOSPC、
  APP-02 的重定向——三条都是**现场没造出来**，不是"造出来了但过了"。
  （PROXY-02 曾经也在这份名单里，是引错证据，已更正，见 §2.4。）''',
        'matrix §4 not-proven')

save(M, s)

# ---------------------------------------------------------------- PROGRESS
P = 'docs/PROGRESS.md'
s = load(P)

s = rep1(s, '''- 本轮子任务（2026-10-29）：**核心验收矩阵逐项对照建立**——新增 `docs/ACCEPTANCE_MATRIX.md`，
  STAGES §19 的 31 条逐行给状态与"目录 + 节"级证据指针，合计 **已验 27 / 部分 4 / 未验 0**；
  45 条指针用脚本核过存在性，0 缺失。**没有产生任何新的真机读数**（索引与定档工作）。
  四条"部分"：DISK-02（LVM/RAID 根盘的现场从未造出来，`P00-VM-C` 仍 NOT RUN）、
  FILE-02（ENOSPC 无真机证据）、APP-02（无真机 302）、PROXY-02（WS 握手实测 NOT RUN）。
- **下一个子任务：没有。** P16 是最后阶段；剩下的是`docs/ACCEPTANCE_MATRIX.md` §2 里那四条
  "部分"的补验现场（各需要一台/一个夹具，不需要新代码能力），以及 `docs/KNOWN_LIMITATIONS.md`
  里已登记的限制。**它们不阻塞"1.0 已实现并验收"这个结论，但也不能被读成已验。**''',
        '''- 本轮子任务（2026-10-29）：**核心验收矩阵逐项对照建立**——新增 `docs/ACCEPTANCE_MATRIX.md`，
  STAGES §19 的 31 条逐行给状态与"目录 + 节"级证据指针，合计 **已验 28 / 部分 3 / 未验 0**
  （10-30 更正：PROXY-02 从「部分」改「已验」，原判引错了证据，见矩阵 §2.4）；
  45 条指针用脚本核过存在性，0 缺失。**没有产生任何新的真机读数**（索引与定档工作）。
  剩下三条"部分"：DISK-02（LVM/RAID 根盘的现场从未造出来，`P00-VM-C` 仍 NOT RUN）、
  FILE-02（ENOSPC 无真机证据）、APP-02（无真机 302）。
- **下一个子任务：没有。** P16 是最后阶段；剩下的是`docs/ACCEPTANCE_MATRIX.md` §2 里那三条
  "部分"的补验现场（各需要一台/一个夹具，不需要新代码能力），以及 `docs/KNOWN_LIMITATIONS.md`
  里已登记的限制。**它们不阻塞"1.0 已实现并验收"这个结论，但也不能被读成已验。**''',
        'progress §1')

s = rep1(s, '### P16 收尾补强（2026-10-29）— 核心验收矩阵逐项对照建立：31 条，已验 27 / 部分 4 / 未验 0',
        '### P16 收尾补强（2026-10-29）— 核心验收矩阵逐项对照建立：31 条，已验 28 / 部分 3 / 未验 0',
        'progress §5 heading')

s = rep1(s, '''4. **PROXY-02 的 WebSocket 是明写的 NOT RUN**。`P10/2026-10-23-p10.2/run.log` 的 NOT RUN 段原话：
   "测试上游（python http.server）不支持 Upgrade；只验配置里 Upgrade/Connection 成对出现"。
   证书与私钥两半是已验（含"私钥保护：所有出口都搜一遍"）。''',
        '''4. **PROXY-02 起初被判成"部分"，是引错了证据（2026-10-30 更正为已验）**。当时依据是
   `P10/2026-10-23-p10.2/run.log` 的 NOT RUN 段原话「测试上游（python http.server）不支持 Upgrade」。
   那句话是**关于 P10.2 那个脚本**的，而 **P10.4 换了上游，在两台机器上都跑完了真握手**：
   `P10/2026-10-23-p10.4/logs/{2204,2404}-core-scenario.log` §4 各有
   `PASS WS 握手返回 101（Upgrade 穿过了 nginx）`，§6 wss 也是 101 + 12 条推流 + echo +
   TLS ≥ 1.2 + 对端指纹与账本一致，§8 重启之后 wss 仍 12 条。私钥那半仍是 P10.3 §10。
   **教训**：NOT RUN 是一张待办，不是一条定论——只查"有没有 NOT RUN 段"就会漏掉后来补上的读数。''',
        'progress §5 item 4')

s = rep1(s, '''"目录 + 节"级证据指针，合计 **已验 27 / 部分 4 / 未验 0**。四条"部分"是 **DISK-02**（LVM/RAID 根盘的
现场从没造出来，`P00-VM-C` 始终 NOT RUN）、**FILE-02**（ENOSPC 无真机证据）、**APP-02**
（没有真机 302 现场）、**PROXY-02**（WebSocket 握手实测 NOT RUN，测试上游不支持 Upgrade）。''',
        '''"目录 + 节"级证据指针，合计 **已验 28 / 部分 3 / 未验 0**（PROXY-02 已于 10-30 更正为已验：
P10.4 在两台机器上都跑出 WS/wss 的 101，初版只引了 P10.2 的 NOT RUN）。剩下三条"部分"是
**DISK-02**（LVM/RAID 根盘的现场从没造出来，`P00-VM-C` 始终 NOT RUN）、**FILE-02**（ENOSPC
无真机证据）、**APP-02**（没有真机 302 现场）。''',
        'progress §7 matrix prose')

s = rep1(s, '| P16-MATRIX | P16 | 已产出（STAGES §19 的 **31 条**逐项对照：**已验 27 / 部分 4 / 未验 0**，'
           '每条给"目录 + 节"级指针与关键读数；四条"部分"逐条写明缺哪个子句要什么现场——'
           'DISK-02 LVM/RAID 根盘从未造过（`P00-VM-C` 仍 NOT RUN）、FILE-02 ENOSPC 无真机证据、'
           'APP-02 无真机 302、PROXY-02 WS 握手 NOT RUN。',
        '| P16-MATRIX | P16 | 已产出（STAGES §19 的 **31 条**逐项对照：**已验 28 / 部分 3 / 未验 0**，'
        '每条给"目录 + 节"级指针与关键读数；三条"部分"逐条写明缺哪个子句要什么现场——'
        'DISK-02 LVM/RAID 根盘从未造过（`P00-VM-C` 仍 NOT RUN）、FILE-02 ENOSPC 无真机证据、'
        'APP-02 无真机 302（**PROXY-02 经复核是引错证据，已改成已验**，见矩阵 §2.4）。',
        'progress §7 P16-MATRIX row')

save(P, s)

# ---------------------------------------------------------------- EVIDENCE_INDEX
E = 'docs/EVIDENCE_INDEX.md'
s = load(E)
s = rep1(s, '：**已验 27 / 部分 4 / 未验 0**；RELEASE-01 按 P16 七相位展开；四条「部分」逐条写明缺哪个子句、要造什么现场才能补',
        '：**已验 28 / 部分 3 / 未验 0**；RELEASE-01 按 P16 七相位展开；三条「部分」逐条写明缺哪个子句、要造什么现场才能补'
        '（PROXY-02 10-30 复核为引错证据，已改已验）',
        'evidence index row')
save(E, s)

# ---------------------------------------------------------------- KNOWN_LIMITATIONS
K = 'docs/KNOWN_LIMITATIONS.md'
s = load(K)
s = rep1(s, '（已验 27 / 部分 4 / 未验 0）', '（已验 28 / 部分 3 / 未验 0）', 'known limitations count')
old_row = ('| **反向代理的 WebSocket 握手** | **部分**（PROXY-02） | 只验了配置里 `Upgrade`/`Connection` '
           '成对出现（`P10/2026-10-23-p10.2/run.log` 的 NOT RUN 段：测试上游 `python http.server` '
           '不支持 Upgrade）。证书与私钥两半是真机读数 |\n')
assert s.count(old_row) == 1, 'KNOWN_LIMITATIONS: WebSocket 行没找到'
s = s.replace(old_row, '')
save(K, s)

# ---------------------------------------------------------------- 证据 README
R = 'docs/evidence/P16/2026-10-29-matrix/README.md'
s = load(R)
s = rep1(s, '## 2. 结论：31 条，已验 27 / 部分 4 / 未验 0',
        '## 2. 结论：31 条，已验 28 / 部分 3 / 未验 0', 'readme §2 heading')
s = rep1(s, '**没有任何一条是"从来没被碰过"。** 27 条有完整的真机读数，4 条有部分读数。',
        '**没有任何一条是"从来没被碰过"。** 28 条有完整的真机读数，3 条有部分读数。\n\n'
        '（10-30 更正：PROXY-02 起初被判成"部分"，复核后是**引错了证据**——初版只引了 P10.2 的 NOT RUN，'
        '而 P10.4 在两台机器上都跑出了 WS/wss 的 101。详见 `docs/ACCEPTANCE_MATRIX.md` §2.4。）',
        'readme §2 intro')
s = splice(s, '| **PROXY-02** WebSocket、证书、私钥 | **WebSocket 握手** |',
           '| 换一个支持 Upgrade 的上游（`websocat` 或几行 Go） |',
           '''| ~~**PROXY-02** WebSocket、证书、私钥~~（**已更正为已验，见矩阵 §2.4**） | 初版说缺"WebSocket 握手"，其实 P10.4 已经跑过 | `P10/2026-10-23-p10.2/run.log` 的 NOT RUN 段只说 P10.2 那一轮，P10.4 换了上游并在两台机器上得到 `PASS WS 握手返回 101` | —（不需要补） |''',
           'readme §2 table row')
s = rep1(s, '### 四条"部分"，缺口逐条写清', '### 三条"部分"，缺口逐条写清', 'readme §2 sub-heading')
save(R, s)

print('全部替换命中 1 次，已写盘。')
