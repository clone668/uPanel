#!/usr/bin/env python3
"""P05.2 载荷校验：把验收 VM 上抓到的真实进程响应，按 CONTRACTS §5/§10.2 与
api/openapi.yaml 检查一遍，并与 /proc 自己的说法逐项对照。

为什么单独一个程序：前端读的就是这几份 JSON。字节能不能被当成数字、
没有 CPU 基线时是不是 null、受保护进程有没有被摆上按钮、身份指纹是不是
PID + starttime 的正常 sha256、预检的结论与阻塞码对不对——都是界面能不能
如实显示的前提。这里只用 Python 标准库，VM 上不需要装任何东西。

用法：
  python3 p05-2-payload-validate.py <payload 目录>

约定：目录里必须有一组固定名字的载荷（见下方 EXPECT_*）；缺文件按 FAIL 处理，
不跳过。

退出码：0 全部通过；1 有失败项。
"""

import hashlib
import json
import os
import re
import sys

PAYLOAD_DIR = sys.argv[1] if len(sys.argv) > 1 else "."

SHA_RE = re.compile(r"^sha256:[0-9a-f]{64}$")
RFC3339_RE = re.compile(r"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?Z$")
BYTE_RE = re.compile(r"^(0|[1-9][0-9]*)$")
PHRASE_RE = re.compile(r"^KILL [A-HJ-NP-Z2-9]{6}$")
PLAN_REF_RE = re.compile(r"^pf_[0-9a-f]{32}$")
JOB_ID_RE = re.compile(r"^job_[0-9a-f]{32}$")

STATE_SET = {"running", "sleeping", "disk_sleep", "zombie", "stopped",
             "tracing", "dead", "idle", "paging", "unknown"}
EXITED_STATES = {"zombie", "dead"}
CATEGORY_SET = {"init", "kernel_thread", "self", "ssh", "systemd_core",
                "network", "other_module"}
SIGNAL_SET = {"TERM", "KILL"}
CAP_ORDER = ["TERM", "KILL"]
BLOCKER_CODES = {"PROCESS_NOT_FOUND", "PROCESS_IDENTITY_MISMATCH",
                 "PROCESS_PROTECTED", "PROCESS_ALREADY_EXITED"}
WARNING_CODES = {"process_terminate", "process_force_kill"}
IMPACT_KINDS = {"process_termination", "process_data_loss", "none"}
TERMINAL = {"succeeded", "failed", "canceled", "interrupted"}

PROCESS_KEYS = {
    "pid", "start_time_ticks", "ppid", "owner", "state", "cpu_percent", "memory_bytes",
    "command", "unit", "protection", "capabilities",
}
SNAPSHOT_KEYS = {"status", "error", "collected_at", "interval_ms", "total", "truncated",
                 "processes"}

# 预检载荷的期望：文件名 → (allowed, 第一个阻塞码或 None, requires_reauth, signal)
#
# requires_reauth 只看信号，不看 allowed：策略在生成预检那一刻按参数算定并落库
# （agent/processaction.go 的 PolicyFor），受保护的 KILL 也一样带着再认证要求与
# 确认短语——拦下它的是 Allowed=false，不是把门槛藏起来。这条差别必须写死在这里，
# 否则界面会以为“被拒的 KILL 不需要再认证”。
EXPECT_PREFLIGHT = {
    "preflight-term-ok.json": (True, None, False, "TERM"),
    "preflight-kill-ok.json": (True, None, True, "KILL"),
    "preflight-kill-ok-2.json": (True, None, True, "KILL"),
    "preflight-protected-init.json": (False, "PROCESS_PROTECTED", False, "TERM"),
    "preflight-protected-self.json": (False, "PROCESS_PROTECTED", True, "KILL"),
    "preflight-notfound.json": (False, "PROCESS_NOT_FOUND", False, "TERM"),
    "preflight-identity-mismatch.json": (False, "PROCESS_IDENTITY_MISMATCH", False, "TERM"),
    "preflight-reused-old.json": (True, None, False, "TERM"),
}
# 可选：这些取决于 VM 上的实际条件，缺文件记 SKIP 而不是 FAIL。
OPTIONAL_PREFLIGHT = {
    # 没有 sshd 进程时脚本不生成。
    "preflight-protected-ssh.json": (False, "PROCESS_PROTECTED", True, "KILL"),
    # 没有内核线程（极端裁剪的内核）时脚本不生成。
    "preflight-protected-kernel.json": (False, "PROCESS_PROTECTED", False, "TERM"),
    # 僵尸进程没构造出来时不生成。
    "preflight-zombie.json": (False, "PROCESS_ALREADY_EXITED", False, "TERM"),
    # ns_last_pid 不可写时内核不会复用 PID，这两份都不生成。
    "preflight-reused-new.json": (True, None, False, "TERM"),
}
# 允许缺失的载荷（理由同上）。
OPTIONAL_PAYLOADS = set(OPTIONAL_PREFLIGHT) | {
    "processes-zombie.json",
    "job-reused-new.json",
    # PID 没被复用就没有这次“拿旧预检提交”的拒绝。
    "job-submit-reused-old.json",
}

# 任务载荷的期望：文件名 → (状态, signal)
EXPECT_JOB = {
    "job-term-ok.json": ("succeeded", "TERM"),
    "job-kill-ok.json": ("succeeded", "KILL"),
    "job-reused-new.json": ("succeeded", "TERM"),
}
# 错误载荷的期望：文件名 → 错误码
EXPECT_ERROR = {
    "preflight-bad-signal.json": "VALIDATION_ERROR",
    "preflight-viewer.json": "FORBIDDEN",
    "job-submit-kill-no-reauth.json": "REAUTH_REQUIRED",
    "job-submit-kill-bad-phrase.json": "CONFIRMATION_MISMATCH",
    "job-submit-kill-replay.json": "PREFLIGHT_STALE",
    "job-submit-protected-init.json": "DEPENDENCY_BLOCKED",
    "job-submit-protected-self.json": "DEPENDENCY_BLOCKED",
    "job-submit-reused-old.json": "DEVICE_IDENTITY_CHANGED",
    "processes-viewer-forbidden.json": "FORBIDDEN",
}

SNAPSHOTS = ["processes-first.json", "processes-cpu.json", "processes-zombie.json"]

PASSED = 0
FAILED = 0
SKIPPED = 0
FAILED_NAMES = []


def ok(name, detail=""):
    global PASSED
    PASSED += 1
    print("PASS  %s%s" % (name, ("（%s）" % (detail,)) if detail != "" else ""))


def skip(name, detail=""):
    global SKIPPED
    SKIPPED += 1
    print("SKIP  %s%s" % (name, ("（%s）" % (detail,)) if detail != "" else ""))


def bad(name, detail=""):
    global FAILED
    FAILED += 1
    FAILED_NAMES.append(name)
    print("FAIL  %s%s" % (name, ("：" + str(detail)[:300]) if detail != "" else ""))


def check(cond, name, detail=""):
    if cond:
        ok(name, detail)
    else:
        bad(name, detail)
    return bool(cond)


def load(name):
    path = os.path.join(PAYLOAD_DIR, name)
    try:
        with open(path, encoding="utf-8") as fh:
            return json.load(fh)
    except FileNotFoundError:
        if name in OPTIONAL_PAYLOADS:
            skip("载荷存在：" + name, "本轮 VM 条件不满足，未生成")
            return None
        bad("载荷存在：" + name, "文件缺失")
    except json.JSONDecodeError as exc:
        bad("载荷是合法 JSON：" + name, exc)
    return None


def envelope(name, body):
    """统一信封：data 与 request_id。"""
    if not check(isinstance(body, dict), name + " 是对象", type(body).__name__):
        return None
    check(isinstance(body.get("request_id"), str) and body["request_id"] != "",
          name + " 带 request_id")
    data = body.get("data")
    if not check(isinstance(data, dict), name + " 有 data 对象", type(data).__name__):
        return None
    return data


def error_envelope(name, body):
    if not check(isinstance(body, dict), name + " 是对象", type(body).__name__):
        return None
    check(isinstance(body.get("request_id"), str) and body["request_id"] != "",
          name + " 带 request_id")
    err = body.get("error")
    if not check(isinstance(err, dict), name + " 有 error 对象", type(err).__name__):
        return None
    return err


def is_rfc3339(value):
    return isinstance(value, str) and bool(RFC3339_RE.match(value))


# ---------------------------------------------------------------- 进程视图

def starttime_of(pid):
    """/proc/<pid>/stat 第 22 字段；读不到返回 None。"""
    try:
        with open("/proc/%d/stat" % pid, encoding="utf-8", errors="replace") as fh:
            raw = fh.read()
    except OSError:
        return None
    rest = raw[raw.rindex(")") + 2:].split()
    if len(rest) <= 19:
        return None
    return int(rest[19])


def validate_process(where, p):
    if not check(isinstance(p, dict), where + " 是对象", type(p).__name__):
        return
    check(set(p.keys()) == PROCESS_KEYS, where + " 字段集合与契约一致",
          "多出 %s，缺少 %s" % (sorted(set(p.keys()) - PROCESS_KEYS),
                              sorted(PROCESS_KEYS - set(p.keys()))))
    check(isinstance(p.get("pid"), int) and 1 <= p["pid"] <= 4194304,
          where + " pid 是正整数", p.get("pid"))
    check(isinstance(p.get("start_time_ticks"), int) and p["start_time_ticks"] >= 1,
          where + " start_time_ticks 是正整数", p.get("start_time_ticks"))
    check(isinstance(p.get("ppid"), int) and p["ppid"] >= 0,
          where + " ppid 是非负整数", p.get("ppid"))
    check(isinstance(p.get("owner"), str) and p["owner"] != "",
          where + " owner 是非空字符串", p.get("owner"))
    check(p.get("state") in STATE_SET, where + " state 在闭集内", p.get("state"))

    cpu = p.get("cpu_percent")
    check(cpu is None or (isinstance(cpu, (int, float)) and cpu >= 0),
          where + " cpu_percent 是 null 或非负数", cpu)
    mem = p.get("memory_bytes")
    check(mem is None or (isinstance(mem, str) and bool(BYTE_RE.match(mem))),
          where + " memory_bytes 是十进制字符串或 null", mem)
    cmd = p.get("command")
    check(isinstance(cmd, str) and cmd != "" and len(cmd) <= 256,
          where + " command 是 1—256 字符的字符串", cmd)
    check(isinstance(p.get("unit"), str), where + " unit 是字符串", p.get("unit"))

    prot = p.get("protection")
    if check(isinstance(prot, dict), where + " protection 是对象", type(prot).__name__):
        check(set(prot.keys()) <= {"protected", "category", "reasons"},
              where + " protection 只有 protected/category/reasons", sorted(prot.keys()))
        check(isinstance(prot.get("protected"), bool),
              where + " protection.protected 是布尔", prot.get("protected"))
        reasons = prot.get("reasons")
        check(isinstance(reasons, list) and all(isinstance(r, str) and r for r in reasons),
              where + " protection.reasons 是字符串数组", reasons)
        if prot.get("protected") is True:
            check(len(reasons or []) >= 1, where + " 受保护时理由非空")
            check(prot.get("category") in CATEGORY_SET,
                  where + " 受保护时 category 在闭集内", prot.get("category"))
        else:
            check(prot.get("category") in (None, ""),
                  where + " 未受保护时没有 category", prot.get("category"))

    caps = p.get("capabilities")
    if check(isinstance(caps, list), where + " capabilities 是数组", type(caps).__name__):
        check(caps == [c for c in CAP_ORDER if c in caps], where + " capabilities 顺序固定", caps)
        check(set(caps) <= set(SIGNAL_SET), where + " capabilities 只含 TERM/KILL", caps)
        if p.get("protection", {}).get("protected") is True or p.get("state") in EXITED_STATES:
            check(caps == [], where + " 受保护或已退出时 capabilities 为空", caps)
        else:
            check(caps == CAP_ORDER, where + " 可操作的进程两个信号都在", caps)


def validate_snapshot(name, data):
    check(set(data.keys()) <= SNAPSHOT_KEYS, name + " 字段集合与契约一致",
          sorted(set(data.keys()) - SNAPSHOT_KEYS))
    check(data.get("status") in {"ok", "error"}, name + " status 在闭集内", data.get("status"))
    if data.get("status") == "error":
        check(isinstance(data.get("error"), str) and data["error"] != "",
              name + " status=error 时给出原因", data.get("error"))
    else:
        check("error" not in data, name + " status=ok 时没有 error 字段")
    check(is_rfc3339(data.get("collected_at")), name + " collected_at 是 RFC3339 UTC",
          data.get("collected_at"))
    check(data.get("interval_ms") == 5000, name + " interval_ms 是 5000", data.get("interval_ms"))
    check(isinstance(data.get("truncated"), bool), name + " truncated 是布尔")
    procs = data.get("processes")
    if not check(isinstance(procs, list), name + " processes 是数组", type(procs).__name__):
        return
    check(isinstance(data.get("total"), int) and data["total"] >= len(procs),
          name + " total 不小于列表长度",
          "%s vs %s" % (data.get("total"), len(procs)))
    if data.get("truncated") is False:
        check(data.get("total") == len(procs), name + " 未截断时 total 等于列表长度",
              "%s vs %s" % (data.get("total"), len(procs)))
    pids = [p.get("pid") for p in procs if isinstance(p, dict)]
    check(pids == sorted(pids), name + " 进程按 pid 升序")
    check(len(pids) == len(set(pids)), name + " pid 不重复")
    for p in procs:
        if isinstance(p, dict):
            validate_process("%s[%s]" % (name, p.get("pid")), p)

    # 与 /proc 对照：还在的进程，starttime 必须逐个相同（身份不是猜的）。
    checked = 0
    mismatched = []
    for p in procs:
        if not isinstance(p, dict):
            continue
        actual = starttime_of(p["pid"])
        if actual is None:
            continue
        checked += 1
        if actual != p.get("start_time_ticks"):
            mismatched.append((p["pid"], p.get("start_time_ticks"), actual))
        if checked >= 40:
            break
    check(not mismatched, name + " starttime 与 /proc 逐个一致（抽样 %d 个）" % checked, mismatched[:5])
    return procs


# ---------------------------------------------------------------- 预检

def validate_preflight(name, data, expect):
    allowed, first_blocker, requires_reauth, signal = expect
    check(data.get("action") == "process.signal", name + " action 是 process.signal",
          data.get("action"))
    check(isinstance(data.get("preflight_id"), str), name + " 有 preflight_id")
    check(is_rfc3339(data.get("expires_at")), name + " expires_at 是 RFC3339 UTC",
          data.get("expires_at"))
    check(data.get("allowed") is allowed, name + " allowed=%s" % allowed, data.get("allowed"))
    check(data.get("requires_reauth") is requires_reauth,
          name + " requires_reauth=%s" % requires_reauth, data.get("requires_reauth"))
    check(data.get("cancellable") is False, name + " 进程信号不可取消", data.get("cancellable"))

    params = data.get("normalized_params")
    if check(isinstance(params, dict), name + " normalized_params 是对象"):
        check(set(params.keys()) == {"pid", "start_time_ticks", "signal"},
              name + " 规范化参数只有 pid/start_time_ticks/signal", sorted(params.keys()))
        check(params.get("signal") == signal, name + " signal=%s" % signal, params.get("signal"))
        check(isinstance(params.get("start_time_ticks"), int) and params["start_time_ticks"] >= 1,
              name + " 规范化参数带 start_time_ticks", params.get("start_time_ticks"))
        pid = params.get("pid")
        expected_lock = "process:%s:%s" % (pid, params.get("start_time_ticks"))
        check(data.get("locks") == [expected_lock],
              name + " 资源锁是 process:<pid>:<starttime>", data.get("locks"))

    target = data.get("target")
    if check(isinstance(target, dict), name + " target 是对象"):
        check(isinstance(target.get("display_name"), str) and target["display_name"] != "",
              name + " target.display_name 非空", target.get("display_name"))
        check(bool(SHA_RE.match(str(target.get("identity_fingerprint")))),
              name + " 身份指纹是完整 sha256", target.get("identity_fingerprint"))
        if isinstance(params, dict) and isinstance(params.get("pid"), int):
            check(str(params["pid"]) in target.get("display_name", ""),
                  name + " 展示名里带 PID", target.get("display_name"))

    blockers = data.get("blockers")
    check(isinstance(blockers, list), name + " blockers 是数组")
    codes = [b.get("code") for b in blockers or [] if isinstance(b, dict)]
    check(set(codes) <= BLOCKER_CODES, name + " 阻塞码在闭集内", codes)
    if allowed:
        check(codes == [], name + " 允许执行时没有阻塞项", codes)
    else:
        check(bool(codes), name + " 不允许执行时给出阻塞项")
        if first_blocker is not None:
            check(codes[:1] == [first_blocker], name + " 第一个阻塞码是 " + first_blocker, codes)

    warnings = data.get("warnings")
    check(isinstance(warnings, list), name + " warnings 是数组")
    wcodes = [w.get("code") for w in warnings or [] if isinstance(w, dict)]
    check(set(wcodes) <= WARNING_CODES, name + " 告警码在闭集内", wcodes)
    if allowed:
        want = "process_force_kill" if signal == "KILL" else "process_terminate"
        check(wcodes[:1] == [want], name + " 告警是 " + want, wcodes)

    impacts = data.get("impacts")
    check(isinstance(impacts, list), name + " impacts 是数组")
    kinds = [i.get("kind") for i in impacts or [] if isinstance(i, dict)]
    check(set(kinds) <= IMPACT_KINDS, name + " 影响类别在闭集内", kinds)

    phrase = data.get("confirmation_phrase")
    if requires_reauth:
        check(isinstance(phrase, str) and bool(PHRASE_RE.match(phrase)),
              name + " KILL 的确认短语形如 'KILL XXXXXX'", phrase)
    else:
        check(phrase is None, name + " 不需要短语时 confirmation_phrase 为 null", phrase)
    return data


# ---------------------------------------------------------------- 任务

def validate_job(name, data, expect):
    status, signal = expect
    check(isinstance(data.get("job_id"), str) and bool(JOB_ID_RE.match(data["job_id"])),
          name + " job_id 形如 job_<32 位十六进制>", data.get("job_id"))
    check(data.get("action") == "process.signal", name + " action 是 process.signal",
          data.get("action"))
    check(data.get("status") in TERMINAL, name + " 状态是终态", data.get("status"))
    check(data.get("status") == status, name + " 状态是 " + status, data.get("status"))
    check(data.get("cancellable") is False, name + " 任务不可取消", data.get("cancellable"))
    check(is_rfc3339(data.get("finished_at")), name + " finished_at 是 RFC3339 UTC",
          data.get("finished_at"))
    check(data.get("error") in ({}, None), name + " 成功的任务没有 error", data.get("error"))

    result = data.get("result")
    if not check(isinstance(result, dict) and result != {}, name + " result 是非空对象", result):
        return
    check(result.get("outcome") == "verified", name + " outcome=verified", result.get("outcome"))
    check(is_rfc3339(result.get("verified_at")), name + " verified_at 是 RFC3339 UTC")

    observed = result.get("observed_state")
    if check(isinstance(observed, dict), name + " observed_state 是对象"):
        check(set(observed.keys()) == {"pid", "start_time_ticks", "signal", "state_before",
                                       "state_after", "gone"},
              name + " observed_state 字段与契约一致", sorted(observed.keys()))
        check(observed.get("signal") == signal, name + " observed_state.signal=" + signal,
              observed.get("signal"))
        check(observed.get("gone") is True, name + " 读回确认进程已退出", observed.get("gone"))
        check(isinstance(observed.get("state_before"), str) and observed["state_before"] != "",
              name + " state_before 是字符串", observed.get("state_before"))
        check(observed.get("state_before") not in EXITED_STATES,
              name + " 发信号之前进程还没退出", observed.get("state_before"))
        check(isinstance(observed.get("start_time_ticks"), int) and observed["start_time_ticks"] >= 1,
              name + " observed_state 带 start_time_ticks")

    payload = result.get("data")
    if check(isinstance(payload, dict), name + " result.data 是对象"):
        check(payload.get("delivered") is True, name + " 信号确实送达", payload.get("delivered"))
        check(payload.get("signal") == signal, name + " result.data.signal=" + signal)
        check(isinstance(payload.get("duration_ms"), int) and payload["duration_ms"] >= 0,
              name + " duration_ms 是非负整数", payload.get("duration_ms"))
        if isinstance(observed, dict):
            check(payload.get("start_time_ticks") == observed.get("start_time_ticks"),
                  name + " result.data 与 observed_state 指向同一个进程身份")
            check(payload.get("pid") == observed.get("pid"),
                  name + " result.data.pid 与 observed_state.pid 相同")
    return data


# ---------------------------------------------------------------- main

def main():
    print("== 进程快照 ==")
    snapshots = {}
    for name in SNAPSHOTS:
        body = load(name)
        if body is None:
            continue
        data = envelope(name, body)
        if data is None:
            continue
        procs = validate_snapshot(name, data)
        if procs is not None:
            snapshots[name] = (data, procs)

    first = snapshots.get("processes-first.json")
    if first is not None:
        data, procs = first
        by_pid = {p["pid"]: p for p in procs if isinstance(p, dict)}
        check(1 in by_pid, "快照里有 PID 1")
        if 1 in by_pid:
            init = by_pid[1]
            check(init["protection"]["protected"] is True and
                  init["protection"].get("category") == "init",
                  "PID 1 的保护类别是 init", init["protection"])
            check(init["capabilities"] == [], "PID 1 没有能力")
        kthreads = [p for p in procs
                    if isinstance(p, dict) and p["protection"].get("category") == "kernel_thread"]
        check(len(kthreads) > 0, "存在被标注为 kernel_thread 的进程", len(kthreads))
        for p in kthreads[:5]:
            check(p["command"].startswith("[") and p["command"].endswith("]"),
                  "内核线程的命令显示成 [comm]（pid=%s）" % p["pid"], p["command"])
            check(p["memory_bytes"] is None or p["memory_bytes"] == "0",
                  "内核线程没有 VmRSS（pid=%s）" % p["pid"], p["memory_bytes"])
        selves = [p for p in procs
                  if isinstance(p, dict) and p["protection"].get("category") == "self"]
        check(len(selves) >= 2, "面板与代理都被标注为 self", [p["pid"] for p in selves])
        for p in selves:
            check(p["capabilities"] == [], "自身进程没有能力（pid=%s）" % p["pid"])
        # 每个受保护的进程都必须给出理由：只有类别没有理由的界面等于没说原因。
        for p in procs:
            if isinstance(p, dict) and p["protection"]["protected"]:
                check(len(p["protection"]["reasons"]) >= 1,
                      "受保护进程给出理由（pid=%s）" % p["pid"])

    if "processes-cpu.json" in snapshots:
        data, procs = snapshots["processes-cpu.json"]
        busy = [p for p in procs if isinstance(p, dict) and
                p.get("cpu_percent") is not None and p["cpu_percent"] > 50]
        check(len(busy) >= 1, "快照里有 CPU 占用超过 50% 的进程",
              [(p["pid"], p["cpu_percent"]) for p in busy[:3]])
        for p in busy[:3]:
            check(p.get("state") == "running",
                  "高 CPU 的进程状态是 running（pid=%s）" % p["pid"], p.get("state"))
            # 到校验这一步（脚本末尾）这个进程可能早就被清理掉了；还在的话，
            # 身份必须与 /proc 一致——不在就记 SKIP，不把“读不到”当成通过。
            actual = starttime_of(p["pid"])
            if actual is None:
                skip("高 CPU 进程的身份与 /proc 一致（pid=%s）" % p["pid"],
                     "校验时该进程已被清理，无法回读 /proc")
            else:
                check(actual == p["start_time_ticks"],
                      "高 CPU 进程的身份与 /proc 一致（pid=%s）" % p["pid"],
                      (p["start_time_ticks"], actual))

    if "processes-zombie.json" in snapshots:
        data, procs = snapshots["processes-zombie.json"]
        zombies = [p for p in procs if isinstance(p, dict) and p["state"] == "zombie"]
        if check(len(zombies) >= 1, "快照里有僵尸进程", len(zombies)):
            for p in zombies:
                check(p["capabilities"] == [], "僵尸进程没有能力（pid=%s）" % p["pid"])
                check(p["memory_bytes"] is None, "僵尸进程没有内存读数（pid=%s）" % p["pid"],
                      p["memory_bytes"])

    print("\n== 预检 ==")
    preflights = {}
    for name, expect in list(EXPECT_PREFLIGHT.items()) + list(OPTIONAL_PREFLIGHT.items()):
        body = load(name)
        if body is None:
            continue
        data = envelope(name, body)
        if data is None:
            continue
        preflights[name] = validate_preflight(name, data, expect)

    # 身份对比：同一个 PID 被重用之后，新旧两份预检的身份指纹必须不同。
    old = preflights.get("preflight-reused-old.json")
    new = preflights.get("preflight-reused-new.json")
    if old is not None and new is not None:
        old_pid = old["normalized_params"]["pid"]
        new_pid = new["normalized_params"]["pid"]
        check(old_pid == new_pid, "被重用的正是同一个 PID", (old_pid, new_pid))
        check(old["normalized_params"]["start_time_ticks"] !=
              new["normalized_params"]["start_time_ticks"],
              "新旧进程的 starttime 不同（身份不同）",
              (old["normalized_params"]["start_time_ticks"],
               new["normalized_params"]["start_time_ticks"]))
        old_ident = old["target"]["identity_fingerprint"]
        new_ident = new["target"]["identity_fingerprint"]
        check(old_ident != new_ident, "陈旧预检的身份指纹与新进程不同")
        want = "sha256:" + hashlib.sha256(
            ("process/v1|%d|%d" % (new_pid, new["normalized_params"]["start_time_ticks"]))
            .encode()).hexdigest()
        check(new_ident == want, "身份指纹是 sha256('process/v1|pid|starttime')", new_ident)

    # 受保护的三个目标必须各有不同来源：init / self / kernel_thread / ssh。
    categories = {}
    for name in ("preflight-protected-init.json", "preflight-protected-self.json",
                 "preflight-protected-kernel.json", "preflight-protected-ssh.json"):
        data = preflights.get(name)
        if data is None:
            continue
        message = data["blockers"][0]["message"]
        check("受保护的进程" in message, name + " 的阻塞说明写的是“受保护的进程”", message)
    for name, cat in (("preflight-protected-init.json", "PID 1"),
                      ("preflight-protected-self.json", "面板"),
                      ("preflight-protected-kernel.json", "内核线程")):
        data = preflights.get(name)
        if data is None:
            continue
        check(data["blockers"][0]["message"] != "", name + " 有具体理由")
        categories[name] = data["blockers"][0]["message"]
    if len(categories) >= 2:
        check(len(set(categories.values())) == len(categories),
              "不同保护来源给出不同理由", categories)

    print("\n== 任务 ==")
    for name, expect in EXPECT_JOB.items():
        body = load(name)
        if body is None:
            continue
        data = envelope(name, body)
        if data is not None:
            validate_job(name, data, expect)

    print("\n== 错误载荷 ==")
    for name, code in EXPECT_ERROR.items():
        body = load(name)
        if body is None:
            continue
        err = error_envelope(name, body)
        if err is None:
            continue
        check(err.get("code") == code, name + " 错误码是 " + code, err.get("code"))
        check(isinstance(err.get("message"), str) and err["message"] != "",
              name + " 错误消息非空", err.get("message"))

    print("\n== 汇总 ==")
    print("PASS=%d FAIL=%d SKIP=%d" % (PASSED, FAILED, SKIPPED))
    if SKIPPED:
        print("SKIP 表示本轮 VM 上这个条件没有出现（不是通过，也不是失败）")
    if FAILED_NAMES:
        print("失败项: %s" % ", ".join(FAILED_NAMES))
    return 0 if FAILED == 0 else 1


if __name__ == "__main__":
    sys.exit(main())
