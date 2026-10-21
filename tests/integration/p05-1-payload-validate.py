#!/usr/bin/env python3
"""P05.1 载荷校验：把验收 VM 上抓到的真实服务响应，按 CONTRACTS §5/§10.1 与
api/openapi.yaml 检查一遍，并和 systemctl 自己的说法逐项对照。

为什么单独一个程序：前端读的就是这几份 JSON。字节能不能被当成数字、
没有主进程时是不是 null、保护单元有没有被摆上按钮、预检的结论与阻塞码对不对——
都是界面能不能如实显示的前提。这里只用 Python 标准库，VM 上不需要装任何东西。

用法：
  python3 p05-1-payload-validate.py <payload 目录>

约定：目录里必须有 services-final.json、unit-target-final.json、
systemctl-union.txt、crosscheck.tsv、unit-map.tsv，以及若干 unit-<n>.json 与
preflight-*.json / job-*.json。缺文件按 FAIL 处理，不跳过。

退出码：0 全部通过；1 有失败项。
"""

import hashlib
import json
import os
import re
import sys

PAYLOAD_DIR = sys.argv[1] if len(sys.argv) > 1 else "."

UNIT_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9:_.@-]*\.service$")
SHA_RE = re.compile(r"^sha256:[0-9a-f]{64}$")
RFC3339_RE = re.compile(r"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?Z$")
BYTE_RE = re.compile(r"^(0|[1-9][0-9]*)$")

LOAD_SET = {"loaded", "not-found", "bad-setting", "error", "merged", "masked"}
ACTIVE_SET = {"active", "reloading", "inactive", "failed", "activating", "deactivating", "maintenance"}
ENABLEMENT_SET = {"enabled", "disabled", "static", "indirect", "masked",
                  "generated", "transient", "alias", "linked", "unknown"}
CAP_ORDER = ["start", "stop", "restart", "enable", "disable"]
CATEGORY_SET = {"self", "ssh", "systemd_core", "network", "other_module"}
BLOCKER_CODES = {"SERVICE_NOT_FOUND", "SERVICE_PROTECTED",
                 "SERVICE_STATE_CONFLICT", "SERVICE_NOT_ENABLEABLE"}
WARNING_CODES = {"service_interruption", "scheduled_start_removed"}
IMPACT_KINDS = {"service_availability", "none"}
TERMINAL = {"succeeded", "failed", "canceled", "interrupted"}

UNIT_KEYS = {
    "unit", "description", "load_state", "active_state", "sub_state", "unit_file_state",
    "enablement", "template", "main_pid", "started_at", "restart_count", "memory_bytes",
    "tasks_current", "fragment_path", "protection", "capabilities", "checked_at",
}

# systemctl is-enabled 的取值 → 归一化 enablement（与 internal/services normalizeEnablement 一致）。
ENABLED_MAP = {
    "enabled": "enabled", "enabled-runtime": "enabled",
    "disabled": "disabled", "static": "static", "indirect": "indirect",
    "masked": "masked", "masked-runtime": "masked", "generated": "generated",
    "transient": "transient", "alias": "alias",
    "linked": "linked", "linked-runtime": "linked",
}

# 预检载荷的期望（allowed、第一个阻塞码或 None）。
EXPECT_PREFLIGHT = {
    "preflight-start-ok.json": ("start", True, None),
    "preflight-start-again.json": ("start", False, "SERVICE_STATE_CONFLICT"),
    "preflight-restart-ok.json": ("restart", True, None),
    "preflight-enable-ok.json": ("enable", True, None),
    "preflight-disable-ok.json": ("disable", True, None),
    "preflight-stop-ok.json": ("stop", True, None),
    "preflight-stop-again.json": ("stop", False, "SERVICE_STATE_CONFLICT"),
    "preflight-restart-inactive.json": ("restart", False, "SERVICE_STATE_CONFLICT"),
    "preflight-protected.json": ("stop", False, "SERVICE_PROTECTED"),
    "preflight-notfound.json": ("start", False, "SERVICE_NOT_FOUND"),
    "preflight-notenableable.json": ("enable", False, "SERVICE_NOT_ENABLEABLE"),
}

# 任务载荷的期望（状态、操作、(字段, 期望值)）。
EXPECT_JOB = {
    "job-start-ok.json": ("succeeded", "start", ("active_state", "active")),
    "job-restart-ok.json": ("succeeded", "restart", ("active_state", "active")),
    "job-enable-ok.json": ("succeeded", "enable", ("enablement", "enabled")),
    "job-disable-ok.json": ("succeeded", "disable", ("enablement", "disabled")),
    "job-stop-ok.json": ("succeeded", "stop", ("active_state", "inactive")),
    "job-start-fail.json": ("failed", "start", None),
}

PASSED = 0
FAILED = 0
FAILED_NAMES = []


def ok(name, detail=""):
    global PASSED
    PASSED += 1
    print("PASS  %s%s" % (name, ("（%s）" % detail) if detail else ""))


def bad(name, detail=""):
    global FAILED
    FAILED += 1
    FAILED_NAMES.append(name)
    print("FAIL  %s%s" % (name, ("：" + str(detail)[:300]) if detail else ""))


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


def is_rfc3339(value):
    return isinstance(value, str) and bool(RFC3339_RE.match(value))


def validate_unit(where, u):
    """按 CONTRACT §10.1 校验一个单元，返回 capabilities 列表（便于调用方继续断言）。"""
    if not check(isinstance(u, dict), where + " 是对象"):
        return []
    keys = set(u.keys())
    check(keys == UNIT_KEYS, where + " 字段集合与契约一致",
          "多出 %s，缺少 %s" % (sorted(keys - UNIT_KEYS), sorted(UNIT_KEYS - keys)))
    name = u.get("unit")
    check(isinstance(name, str) and bool(UNIT_RE.match(name)), where + " unit 名合法", name)
    check(isinstance(u.get("description"), str), where + " description 是字符串")
    check(u.get("load_state") in LOAD_SET, where + " load_state 在闭集内", u.get("load_state"))
    check(u.get("active_state") in ACTIVE_SET, where + " active_state 在闭集内", u.get("active_state"))
    check(isinstance(u.get("sub_state"), str) and u.get("sub_state") != "",
          where + " sub_state 非空字符串", u.get("sub_state"))
    check(isinstance(u.get("unit_file_state"), str), where + " unit_file_state 是字符串")
    check(u.get("enablement") in ENABLEMENT_SET, where + " enablement 在闭集内", u.get("enablement"))
    check(isinstance(u.get("template"), bool), where + " template 是布尔")
    check(u.get("template") == ("@." in (name or "")), where + " template 与单元名一致", name)

    pid = u.get("main_pid")
    check(pid is None or (isinstance(pid, int) and pid > 0),
          where + " main_pid 是正数或 null（0 不是进程号）", pid)
    ts = u.get("started_at")
    check(ts is None or is_rfc3339(ts), where + " started_at 是 RFC3339 UTC 或 null", ts)
    rc = u.get("restart_count")
    check(rc is None or isinstance(rc, int), where + " restart_count 是整数或 null", rc)
    mem = u.get("memory_bytes")
    check(mem is None or (isinstance(mem, str) and bool(BYTE_RE.match(mem))),
          where + " memory_bytes 是十进制字符串或 null", mem)
    tasks = u.get("tasks_current")
    check(tasks is None or isinstance(tasks, int), where + " tasks_current 是整数或 null", tasks)
    check(isinstance(u.get("fragment_path"), str), where + " fragment_path 是字符串")
    check(is_rfc3339(u.get("checked_at")), where + " checked_at 是 RFC3339 UTC", u.get("checked_at"))

    prot = u.get("protection")
    if check(isinstance(prot, dict), where + " protection 是对象"):
        check(set(prot.keys()) <= {"protected", "category", "reasons"},
              where + " protection 字段集合正确", sorted(prot.keys()))
        check(isinstance(prot.get("protected"), bool), where + " protection.protected 是布尔")
        reasons = prot.get("reasons")
        check(isinstance(reasons, list), where + " protection.reasons 是数组")
        if prot.get("protected"):
            check(prot.get("category") in CATEGORY_SET,
                  where + " 受保护单元 category 在闭集内", prot.get("category"))
            check(isinstance(reasons, list) and len(reasons) > 0,
                  where + " 受保护单元有理由")
            check(all(isinstance(r, str) and r for r in (reasons or [])),
                  where + " 保护理由都是非空字符串")
        else:
            check("category" not in prot or prot.get("category") in (None, ""),
                  where + " 未保护单元没有 category", prot.get("category"))
            check(reasons == [], where + " 未保护单元 reasons 为空", reasons)

    caps = u.get("capabilities")
    if not check(isinstance(caps, list), where + " capabilities 是数组"):
        return []
    check(set(caps) <= set(CAP_ORDER), where + " capabilities 只含闭集操作", caps)
    check(caps == sorted(caps, key=CAP_ORDER.index), where + " capabilities 顺序固定", caps)
    check(len(caps) == len(set(caps)), where + " capabilities 没有重复", caps)

    protected = isinstance(prot, dict) and prot.get("protected")
    if protected:
        check(caps == [], where + " 受保护单元 capabilities 为空", caps)
    if u.get("load_state") in {"not-found", "error", "bad-setting"} or u.get("template"):
        check(caps == [], where + " 不可操作单元 capabilities 为空", caps)
    if "start" in caps:
        check(u.get("active_state") in {"inactive", "failed"},
              where + " 出现 start 时状态确实未运行", u.get("active_state"))
    if "stop" in caps:
        check(u.get("active_state") in {"active", "reloading", "failed"},
              where + " 出现 stop 时状态确实在运行", u.get("active_state"))
    if "restart" in caps:
        check(u.get("active_state") in {"active", "reloading"},
              where + " 出现 restart 时状态确实在运行", u.get("active_state"))
    if "enable" in caps:
        check(u.get("enablement") == "disabled",
              where + " 出现 enable 时确实未开机启动", u.get("enablement"))
    if "disable" in caps:
        check(u.get("enablement") == "enabled",
              where + " 出现 disable 时确实开机启动", u.get("enablement"))
    return caps


def compute_snapshot_version(units):
    """按 CONTRACTS §10.1 的算法重算快照版本。

    契约说这个版本“只由单元状态内容决定，不含采集时间、PID、内存与重启次数”。
    照着算法重算一遍并通过，才说明这句话是真的（抄错一个字段就重算不出来）。
    ComputeSnapshotVersion 的格式见 internal/services/types.go。
    """
    h = hashlib.sha256()
    h.update(b"services/v1\n")
    for u in sorted(units, key=lambda x: x.get("unit") or ""):
        line = "%s\x00%s\x00%s\x00%s\x00%s\x00%s\n" % (
            u.get("unit"), u.get("load_state"), u.get("active_state"),
            u.get("sub_state"), u.get("enablement"),
            "true" if u.get("template") else "false",
        )
        h.update(line.encode("utf-8"))
    return "sha256:" + h.hexdigest()


def validate_list(name, data):
    if data is None:
        return
    check(data.get("status") in {"ok", "error"}, name + " status 闭集", data.get("status"))
    check(is_rfc3339(data.get("collected_at")), name + " collected_at 是 RFC3339 UTC")
    check(data.get("interval_ms") == 10000, name + " interval_ms 是 10000", data.get("interval_ms"))
    check(bool(SHA_RE.match(str(data.get("snapshot_version")))),
          name + " snapshot_version 是 sha256", data.get("snapshot_version"))
    check(isinstance(data.get("truncated"), bool), name + " truncated 是布尔")
    units = data.get("units")
    if not check(isinstance(units, list) and units, name + " units 非空数组"):
        return
    check(not data.get("truncated"), name + " 本机单元数没有超过上限")
    names = [u.get("unit") for u in units if isinstance(u, dict)]
    check(len(names) == len(set(names)), name + " 单元名不重复")
    check(names == sorted(names), name + " 单元按名字升序", "%d 个" % len(names))
    # 版本必须能按契约的算法从状态内容重算出来：这同时证明了它不含时间与 PID。
    check(compute_snapshot_version(units) == data.get("snapshot_version"),
          name + " snapshot_version 可由状态内容重算", data.get("snapshot_version"))
    # 全量校验每个单元（真实机器上几百个，Python 跑得动；这里不抽样）。
    broken = 0
    for u in units:
        before = FAILED
        validate_unit(name + "[" + str(u.get("unit")) + "]", u)
        if FAILED > before:
            broken += 1
    if broken:
        bad(name + " 每个单元都符合字段契约", "%d 个单元有问题" % broken)
    else:
        ok(name + " 每个单元都符合字段契约", "%d 个单元" % len(units))


def validate_unit_payload(name, data, expect_unit=None):
    if data is None:
        return None
    unit = data.get("unit")
    if not check(isinstance(unit, dict), name + " 有 unit 对象"):
        return None
    validate_unit(name, unit)
    if expect_unit:
        check(unit.get("unit") == expect_unit, name + " 是目标单元", unit.get("unit"))
    return unit


def validate_preflight(name, body):
    data = envelope(name, body)
    if data is None:
        return
    allowed = data.get("allowed")
    check(isinstance(allowed, bool), name + " allowed 是布尔")
    check(data.get("action") == "service.control", name + " action 是 service.control", data.get("action"))
    params = data.get("normalized_params")
    if check(isinstance(params, dict), name + " 有 normalized_params"):
        check(set(params.keys()) == {"unit", "operation"},
              name + " normalized_params 只有 unit 与 operation", sorted(params.keys()))
        check(bool(UNIT_RE.match(str(params.get("unit")))), name + " 规范化后的单元名合法",
              params.get("unit"))
        check(params.get("operation") in CAP_ORDER, name + " 规范化后的操作在闭集内",
              params.get("operation"))
    target = data.get("target")
    if check(isinstance(target, dict), name + " 有 target"):
        check(isinstance(target.get("display_name"), str) and target["display_name"] != "",
              name + " target.display_name 非空")
        check(bool(SHA_RE.match(str(target.get("identity_fingerprint")))),
              name + " target.identity_fingerprint 是 sha256", target.get("identity_fingerprint"))
    check(is_rfc3339(data.get("expires_at")), name + " expires_at 是 RFC3339 UTC")

    blockers = data.get("blockers")
    warnings = data.get("warnings")
    impacts = data.get("impacts")
    check(isinstance(blockers, list), name + " blockers 是数组")
    check(isinstance(warnings, list), name + " warnings 是数组")
    if check(isinstance(impacts, list) and impacts, name + " impacts 非空"):
        for imp in impacts:
            check(imp.get("kind") in IMPACT_KINDS, name + " impact.kind 在闭集内", imp.get("kind"))
            check(isinstance(imp.get("description"), str) and imp["description"] != "",
                  name + " impact.description 非空")
    for notice in (blockers or []) + (warnings or []):
        check(isinstance(notice.get("code"), str) and notice["code"] != "",
              name + " 通知项有 code", notice.get("code"))
        check(isinstance(notice.get("message"), str) and notice["message"] != "",
              name + " 通知项有 message", notice.get("code"))
    for blocker in blockers or []:
        check(blocker.get("code") in BLOCKER_CODES, name + " 阻塞码在闭集内", blocker.get("code"))
    for warning in warnings or []:
        check(warning.get("code") in WARNING_CODES, name + " 告警码在闭集内", warning.get("code"))
    if allowed:
        check(blockers == [], name + " allowed=true 时没有阻塞项", blockers)
    else:
        check(bool(blockers), name + " allowed=false 时有阻塞项")

    # service.control 的属性：不需要再认证、不需要短语、不可取消、单单元锁。
    check(data.get("requires_reauth") is False, name + " 不需要再认证", data.get("requires_reauth"))
    check(data.get("confirmation_phrase") is None, name + " 没有确认短语",
          data.get("confirmation_phrase"))
    check(data.get("cancellable") is False, name + " 不可取消", data.get("cancellable"))
    locks = data.get("locks")
    unit = (params or {}).get("unit") or ""
    check(locks == ["service:" + unit], name + " 锁是 service:<unit>", locks)

    expect = EXPECT_PREFLIGHT.get(os.path.basename(name))
    if expect:
        op, want_allowed, want_code = expect
        check((params or {}).get("operation") == op, name + " 操作符合预期", (params or {}).get("operation"))
        check(allowed == want_allowed, name + " allowed 符合预期", allowed)
        if want_code:
            codes = [b.get("code") for b in blockers or []]
            check(want_code in codes, name + " 阻塞码包含 " + want_code, codes)


def validate_job(name, body):
    data = envelope(name, body)
    if data is None:
        return
    status = data.get("status")
    check(status in TERMINAL, name + " 任务已到终态", status)
    expect = EXPECT_JOB.get(os.path.basename(name))
    if not expect:
        return
    want_status, op, field = expect
    check(status == want_status, name + " 状态符合预期", status)
    result = data.get("result")
    if want_status == "failed":
        # 契约（openapi Job.result）：没有结果时是空对象 {}。
        # 断言它真的空了，而不是把错误包装成一份“结果”。
        check(result == {}, name + " 失败任务的 result 是空对象（不伪造 observed_state）", result)
        err = data.get("error")
        check(isinstance(err, dict) and isinstance(err.get("code"), str) and err["code"] != "",
              name + " 失败任务有错误码")
        check(isinstance(err, dict) and isinstance(err.get("message"), str) and err["message"] != "",
              name + " 失败任务有可读错误")
        return
    if not check(isinstance(result, dict), name + " 成功任务有 result", type(result).__name__):
        return
    observed = result.get("observed_state")
    if not check(isinstance(observed, dict), name + " 结果里有 observed_state"):
        return
    check(observed.get("verified") is True, name + " observed_state.verified 为 true",
          observed.get("verified"))
    check(observed.get("operation") == op, name + " observed_state.operation 是 " + op,
          observed.get("operation"))
    check(observed.get("expected") not in (None, ""), name + " 写明了期望状态",
          observed.get("expected"))
    if field:
        key, want = field
        check(observed.get(key) == want, name + " observed_state.%s 是 %s" % (key, want),
              observed.get(key))
    pid = observed.get("main_pid")
    check(pid is None or (isinstance(pid, int) and pid > 0),
          name + " observed_state.main_pid 是正数或缺失", pid)
    payload = result.get("data")
    if check(isinstance(payload, dict), name + " 结果里有 data"):
        check(isinstance(payload.get("exit_code"), int), name + " data.exit_code 是整数")
        check(isinstance(payload.get("duration_ms"), int), name + " data.duration_ms 是整数")


def main():
    # 1. 列表载荷
    final = load("services-final.json")
    final_data = envelope("services-final.json", final) if final else None
    validate_list("services-final", final_data)
    after = load("services-after.json")
    if after:
        validate_list("services-after", envelope("services-after.json", after))
    raw = load("services.json")
    if raw:
        validate_list("services", envelope("services.json", raw))

    # 2. 目标单元：停止之后的状态必须自洽
    target = load("unit-target-final.json")
    unit = validate_unit_payload("unit-target-final", envelope("unit-target-final.json", target)
                                 if target else None, expect_unit="upanel-accept.service")
    if unit:
        check(unit.get("active_state") == "inactive", "目标单元最终是 inactive",
              unit.get("active_state"))
        check(unit.get("enablement") == "disabled", "目标单元最终是 disabled",
              unit.get("enablement"))
        check(unit.get("capabilities") == ["start", "enable"], "目标单元 capabilities 是交付预期的子集",
              unit.get("capabilities"))
        check(unit.get("protection") == {"protected": False, "reasons": []},
              "目标单元不受保护", unit.get("protection"))
    stopped = load("unit-target-stopped.json")
    if stopped:
        su = validate_unit_payload("unit-target-stopped", envelope("unit-target-stopped.json", stopped))
        if su:
            check(su.get("main_pid") is None, "停止后 main_pid 是 null（不是 0）", su.get("main_pid"))

    # 3. 抽样单元与 systemctl 的说法对照
    unit_map = {}
    map_path = os.path.join(PAYLOAD_DIR, "unit-map.tsv")
    if os.path.exists(map_path):
        for line in open(map_path, encoding="utf-8"):
            parts = line.rstrip("\n").split("\t")
            if len(parts) == 2:
                unit_map[parts[1]] = "unit-%s.json" % parts[0]
    else:
        bad("载荷存在：unit-map.tsv", "文件缺失")

    cross_path = os.path.join(PAYLOAD_DIR, "crosscheck.tsv")
    if os.path.exists(cross_path):
        seen = 0
        for line in open(cross_path, encoding="utf-8"):
            parts = line.rstrip("\n").split("\t")
            if len(parts) != 3:
                continue
            name, is_active, is_enabled = parts
            payload = unit_map.get(name)
            if not payload:
                bad("对照单元有载荷：" + name, "unit-map.tsv 里没有它")
                continue
            body = load(payload)
            if not body:
                continue
            data = body.get("data") or {}
            u = data.get("unit") or {}
            seen += 1
            validate_unit(name, u)
            check(u.get("unit") == name, "%s 载荷单元名一致" % name, u.get("unit"))
            check(u.get("active_state") == is_active,
                  "%s active_state 与 systemctl is-active 一致" % name,
                  "接口 %s / systemctl %s" % (u.get("active_state"), is_active))
            want = ENABLED_MAP.get(is_enabled)
            if want:
                check(u.get("enablement") == want,
                      "%s enablement 与 systemctl is-enabled 一致" % name,
                      "接口 %s / systemctl %s → %s" % (u.get("enablement"), is_enabled, want))
            else:
                ok("%s enablement 无法对照（systemctl 输出 %r）" % (name, is_enabled))
        if seen:
            ok("抽样单元逐项对照完成", "%d 个单元" % seen)
        else:
            bad("抽样单元逐项对照完成", "crosscheck.tsv 为空")
    else:
        bad("载荷存在：crosscheck.tsv", "文件缺失")

    # 4. 受保护单元
    protected_files = [f for f in sorted(os.listdir(PAYLOAD_DIR)) if f.startswith("protected-")]
    if not protected_files:
        bad("载荷里有受保护单元", "没有 protected-*.json")
    for f in protected_files:
        body = load(f)
        u = validate_unit_payload(f, envelope(f, body) if body else None)
        if not u:
            continue
        prot = u.get("protection") or {}
        check(prot.get("protected") is True, f + " 标记为受保护", prot.get("protected"))
        check(u.get("capabilities") == [], f + " 受保护单元没有可执行动作", u.get("capabilities"))
        check(bool(prot.get("reasons")), f + " 有保护理由")

    # 5. 不存在的单元
    nf = load("notfound.json")
    if nf:
        err = nf.get("error") or {}
        check(err.get("code") == "NOT_FOUND", "不存在的单元错误码是 NOT_FOUND", err.get("code"))
        check(err.get("message") not in (None, ""), "不存在的单元有可读消息", err.get("message"))

    # 6. 列表 ⊇ systemctl 枚举（缓存最多 10 秒旧，只可能多不可能少）
    union_path = os.path.join(PAYLOAD_DIR, "systemctl-union.txt")
    if os.path.exists(union_path) and final_data:
        sys_names = {line.strip() for line in open(union_path, encoding="utf-8") if line.strip()}
        api_names = {u.get("unit") for u in final_data.get("units", [])}
        missing = sorted(n for n in sys_names if n not in api_names)
        check(not missing, "systemctl 枚举到的单元都在列表里",
              "缺少 %d 个：%s" % (len(missing), missing[:8]))
        ok("列表规模与 systemctl 对照", "systemctl %d / 接口 %d" % (len(sys_names), len(api_names)))

    # 7. 预检与任务载荷
    for f in sorted(os.listdir(PAYLOAD_DIR)):
        if f.startswith("preflight-") and f.endswith(".json") and f in EXPECT_PREFLIGHT:
            body = load(f)
            if body:
                validate_preflight(f, body)
        elif f.startswith("job-") and f.endswith(".json") and f in EXPECT_JOB:
            body = load(f)
            if body:
                validate_job(f, body)

    print()
    print("载荷校验: PASS=%d FAIL=%d" % (PASSED, FAILED))
    if FAILED_NAMES:
        print("失败项: %s" % " | ".join(FAILED_NAMES[:20]))
    return 1 if FAILED else 0


if __name__ == "__main__":
    sys.exit(main())
