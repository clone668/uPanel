#!/usr/bin/env python3
"""P05.3 载荷校验：把验收 VM 上抓到的真实日志响应，按 CONTRACTS §10/§10.3 与
api/openapi.yaml 逐项检查。

为什么单独一个程序：前端读的就是这几份 JSON。“字段名对不对、级别闭集是不是
真的是闭集、binary 的 message 是不是空串、翻页到底时 next_cursor 是不是 null、
viewer 是不是真的拿不到任何条目文本”——这些靠 curl + grep 看不出来，靠读代码
说服不了人。这里只用 Python 标准库，VM 上不需要装任何东西。

用法：
  python3 p05-3-payload-validate.py <payload 目录>

约定：目录里必须有一组固定名字的载荷（见下方 EXPECT_*）；缺文件按 FAIL 处理，
不跳过。翻页页数写在 page-count.txt 里（由验收脚本写出）。

退出码：0 全部通过；1 有失败项。
"""

import datetime as dt
import json
import os
import re
import sys

PAYLOAD_DIR = sys.argv[1] if len(sys.argv) > 1 else "."

RFC3339_RE = re.compile(r"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?Z$")
CURSOR_RE = re.compile(r"^[A-Za-z0-9=;:._+\-/]{1,512}$")

SOURCE_SET = {"journal", "kernel"}
# 结果里可以是 unknown（读不出 PRIORITY），入参里不行——这个区别就是“过滤有没有
# 真的生效”的分界线，载荷校验必须两种都认，而不是把它当成一个级别看待。
LEVEL_SET = {"emergency", "alert", "critical", "error", "warning", "notice",
             "info", "debug", "unknown"}
SEVERE_LEVELS = {"emergency", "alert", "critical", "error"}
TRUNC_REASONS = {"page_bytes", "output_limit"}

PAGE_KEYS = {"source", "unit", "level", "from", "to", "entries", "next_cursor",
             "truncated", "truncated_reason", "scanned", "duration_ms"}
ENTRY_REQUIRED = {"cursor", "timestamp", "level", "message", "truncated", "redacted",
                  "binary", "pid"}
ENTRY_OPTIONAL = {"unit", "identifier", "comm"}

MAX_MESSAGE_BYTES = 8192
MAX_PAGE_BYTES = 1 << 20
MARKERS = 30
SECRETS = ("hunter2secret", "abcdef123456")

# 错误载荷 → 期望的错误码。4xx 里的参数问题一律 VALIDATION_ERROR：
# 参数错了却回 500，会把“你写错了”说成“服务器坏了”。
EXPECT_ERROR = {
    "err-level-unknown.json": "VALIDATION_ERROR",
    "err-level-bogus.json": "VALIDATION_ERROR",
    "err-order.json": "VALIDATION_ERROR",
    "err-span.json": "VALIDATION_ERROR",
    "err-from-format.json": "VALIDATION_ERROR",
    "err-to-format.json": "VALIDATION_ERROR",
    "err-future.json": "VALIDATION_ERROR",
    "err-limit-zero.json": "VALIDATION_ERROR",
    "err-limit-1001.json": "VALIDATION_ERROR",
    "err-limit-text.json": "VALIDATION_ERROR",
    "err-cursor-invalid.json": "VALIDATION_ERROR",
    "err-cursor-charset.json": "VALIDATION_ERROR",
    "err-unit-kernel.json": "VALIDATION_ERROR",
    "err-source.json": "VALIDATION_ERROR",
    "err-unit-shape.json": "VALIDATION_ERROR",
    "err-unit-glob.json": "VALIDATION_ERROR",
    "err-unit-path.json": "VALIDATION_ERROR",
    "err-stream-429.json": "VALIDATION_ERROR",
    "err-no-auth.json": "UNAUTHENTICATED",
    "err-no-auth-stream.json": "UNAUTHENTICATED",
    "logs-viewer.json": "FORBIDDEN",
    "logs-viewer-stream.json": "FORBIDDEN",
}

SUCCESS_PAYLOADS = [
    "logs-basic.json",
    "logs-other-unit.json",
    "logs-level-error.json",
    "logs-level-info.json",
    "logs-from-only.json",
    "logs-default-window.json",
    "logs-limit5.json",
    "logs-page-bytes.json",
    "logs-huge.json",
    "logs-kernel.json",
]

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


def parse_time(value):
    """解析 RFC3339；解析不了返回 None（形状问题由 validate_entry 报，不在这里重复）。"""
    if not isinstance(value, str):
        return None
    try:
        return dt.datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError:
        return None


def validate_entry(where, e):
    """一条日志的形状。返回 True 表示可用。"""
    if not check(isinstance(e, dict), where + " 是对象", type(e).__name__):
        return False
    keys = set(e)
    missing = ENTRY_REQUIRED - keys
    extra = keys - ENTRY_REQUIRED - ENTRY_OPTIONAL
    check(not missing, where + " 必填字段齐全", "缺 %s" % sorted(missing))
    check(not extra, where + " 没有契约外的字段", sorted(extra))
    check(isinstance(e.get("cursor"), str) and bool(CURSOR_RE.match(e.get("cursor") or "")),
          where + " 的 cursor 形状合法", e.get("cursor"))
    check(is_rfc3339(e.get("timestamp")), where + " 的 timestamp 是 RFC3339 UTC",
          e.get("timestamp"))
    check(e.get("level") in LEVEL_SET, where + " 的 level 在闭集内", e.get("level"))
    check(isinstance(e.get("message"), str), where + " 的 message 是字符串",
          type(e.get("message")).__name__)
    check(len((e.get("message") or "").encode()) <= MAX_MESSAGE_BYTES,
          where + " 的 message 不超过 8 KiB",
          len((e.get("message") or "").encode()))
    for flag in ("truncated", "redacted", "binary"):
        check(isinstance(e.get(flag), bool), where + " 的 %s 是布尔" % flag,
              type(e.get(flag)).__name__)
    pid = e.get("pid")
    check(pid is None or (isinstance(pid, int) and not isinstance(pid, bool) and pid >= 1),
          where + " 的 pid 是正整数或 null", pid)
    # binary 的 message 必须是空串：把“没有文本”渲染成一条空白日志行，
    # 读者会以为那条日志本来就是空的。
    if e.get("binary") is True:
        check(e.get("message") == "", where + " binary 条目的 message 是空串",
              repr(e.get("message"))[:40])
    for opt in ("unit", "identifier", "comm"):
        if opt in e:
            check(isinstance(e[opt], str) and e[opt] != "",
                  where + " 的 %s 出现时是非空字符串" % opt, e[opt])
    return True


def validate_page(name, data):
    """一页日志的形状与自洽性。"""
    keys = set(data)
    missing = PAGE_KEYS - {"unit", "level", "truncated_reason"} - keys
    extra = keys - PAGE_KEYS
    check(not missing, name + " 必填字段齐全", "缺 %s" % sorted(missing))
    check(not extra, name + " 没有契约外的字段", sorted(extra))
    check(data.get("source") in SOURCE_SET, name + " 的 source 在闭集内", data.get("source"))
    check(is_rfc3339(data.get("from")) and is_rfc3339(data.get("to")),
          name + " 的 from/to 是 RFC3339 UTC", (data.get("from"), data.get("to")))
    if is_rfc3339(data.get("from")) and is_rfc3339(data.get("to")):
        span = (parse_time(data["to"]) - parse_time(data["from"])).total_seconds()
        check(0 < span <= 24 * 3600, name + " 的窗口跨度在 (0, 24h] 内", span)
    if "level" in data:
        check(data["level"] in LEVEL_SET - {"unknown"},
              name + " 回显的 level 是入参级别（不含 unknown）", data["level"])
    if "unit" in data:
        check(isinstance(data["unit"], str) and data["unit"] != "",
              name + " 回显的 unit 非空", data["unit"])
    check(isinstance(data.get("truncated"), bool), name + " 的 truncated 是布尔",
          type(data.get("truncated")).__name__)
    if "truncated_reason" in data:
        check(data["truncated_reason"] in TRUNC_REASONS,
              name + " 的 truncated_reason 在闭集内", data["truncated_reason"])
        check(data.get("truncated") is True, name + " 有 truncated_reason 时 truncated=true")
    nc = data.get("next_cursor", "missing")
    check(nc is None or (isinstance(nc, str) and nc != ""),
          name + " 的 next_cursor 是非空字符串或 null", nc)
    check(isinstance(data.get("scanned"), int) and not isinstance(data.get("scanned"), bool)
          and data["scanned"] >= 0, name + " 的 scanned 是非负整数", data.get("scanned"))
    check(isinstance(data.get("duration_ms"), int)
          and not isinstance(data.get("duration_ms"), bool) and data["duration_ms"] >= 0,
          name + " 的 duration_ms 是非负整数", data.get("duration_ms"))
    entries = data.get("entries")
    if not check(isinstance(entries, list), name + " 的 entries 是数组",
                 type(entries).__name__):
        return False
    check(isinstance(data.get("scanned"), int) and data["scanned"] >= len(entries),
          name + " 的 scanned 不小于本页条数", (data.get("scanned"), len(entries)))
    good = True
    for i, e in enumerate(entries):
        if not validate_entry("%s.entries[%d]" % (name, i), e):
            good = False
    if entries:
        # 按解析后的时间比较，不能按字符串：小数位宽度不一，
        # “...890676Z” 与 “...8906Z” 字符串比会把顺序判反。
        stamps = [parse_time(e.get("timestamp")) for e in entries]
        if all(st is not None for st in stamps):
            check(all(a >= b for a, b in zip(stamps, stamps[1:])),
                  name + " 的条目按时间倒序（最新在前）",
                  [e.get("timestamp") for e in entries[:2]])
        cursors = [e.get("cursor") for e in entries]
        check(len(cursors) == len(set(cursors)), name + " 本页游标不重复")
    return good


def main():
    print("== 能力表 ==")
    body = load("agent-capabilities.json")
    caps = (body or {}).get("data") or {}
    if body is not None:
        check(isinstance(caps.get("supported_queries"), list), "能力表的 supported_queries 是数组")
        check("logs.query" in (caps.get("supported_queries") or []),
              "能力表登记了 logs.query", caps.get("supported_queries"))
        check("processes.list" in (caps.get("supported_queries") or []),
              "能力表仍登记着 processes.list（没有因为这一轮少报）",
              caps.get("supported_queries"))
        check("service.control" in (caps.get("supported_actions") or []),
              "能力表仍登记着 service.control", caps.get("supported_actions"))

    print("\n== 日志页 ==")
    pages = {}
    for name in SUCCESS_PAYLOADS:
        body = load(name)
        if body is None:
            continue
        data = envelope(name, body)
        if data is None:
            continue
        if validate_page(name, data):
            pages[name] = data

    basic = pages.get("logs-basic.json")
    if basic is not None:
        unit = basic.get("unit")
        entries = basic["entries"]
        check(len(entries) >= MARKERS + 3,
              "基本查询拿到全部标记行", len(entries))
        # systemd 自己写的 Starting/Finished 行 _SYSTEMD_UNIT 是 init.scope，
        # 而 journalctl -u 仍然把它们算进该单元——API 如实回传原始字段。
        check(all(e.get("unit") == unit or
                  (e.get("unit") == "init.scope" and unit in (e.get("message") or ""))
                  for e in entries),
              "基本查询的每条都属于所查 unit（允许 systemd 的 init.scope 生命周期行）", unit)
        check(any(e.get("identifier") for e in entries),
              "条目带 identifier（写入者自报的名字）")
        check(sum(1 for e in entries if "marker-i=" in e["message"]) >= MARKERS,
              "30 个 marker-i 标记都在", sum(1 for e in entries if "marker-i=" in e["message"]))
        check(any(e["level"] == "error" for e in entries),
              "有一条 error 级别的条目（level 过滤才有对象）")
        check(any(e["level"] == "warning" for e in entries),
              "有一条 warning 级别的条目")
        # 秘密过滤：抹掉的是值，不是整行；redacted 是“抹过”的事实。
        pw = [e for e in entries if "password=" in e["message"]]
        tk = [e for e in entries if "token=" in e["message"]]
        check(len(pw) == 1, "含 password= 的条目存在", len(pw))
        if pw:
            check(pw[0]["redacted"] is True, "含 password= 的条目被标成 redacted")
        check(len(tk) == 1, "含 token= 的条目存在", len(tk))
        if tk:
            check(tk[0]["redacted"] is True, "含 token= 的条目被标成 redacted")
        blob = json.dumps(basic, ensure_ascii=False)
        for secret in SECRETS:
            check(secret not in blob, "响应里没有明文秘密 %s" % secret)
        check(any(not e["redacted"] for e in entries if "marker-i=" in e["message"]),
              "普通条目没有被误标成 redacted")

    other = pages.get("logs-other-unit.json")
    if other is not None:
        check(all("marker-i=" not in e["message"] and "big-" not in e["message"]
                  for e in other["entries"]),
              "unit 过滤是限制性的：别的 unit 里没有我们的标记",
              len(other["entries"]))

    lev_err = pages.get("logs-level-error.json")
    if lev_err is not None:
        check(all(e["level"] in SEVERE_LEVELS for e in lev_err["entries"]),
              "level=error 只给 error 及更严重的条目",
              sorted({e["level"] for e in lev_err["entries"]}))
        check(all("marker-i=" not in e["message"] for e in lev_err["entries"]),
              "level=error 里没有 info 行")
        check(any("marker-err-1" in e["message"] for e in lev_err["entries"]),
              "level=error 里有我们写的 error 行")
        check(lev_err.get("level") == "error", "level 回显是 error", lev_err.get("level"))

    lev_info = pages.get("logs-level-info.json")
    if lev_info is not None:
        blob = " ".join(e["message"] for e in lev_info["entries"])
        check("marker-i=" in blob and "marker-err-1" in blob,
              "level=info 是下限语义：info 与 error 都在")

    from_only = pages.get("logs-from-only.json")
    if from_only is not None:
        # 只给 from 时 to 取“现在”，所以跨度是 now-from（只会更短，不会超 1 小时）。
        to = parse_time(from_only["to"])
        span = (to - parse_time(from_only["from"])).total_seconds()
        now = dt.datetime.now(dt.timezone.utc)
        check(0 < span <= 3600 and abs((now - to).total_seconds()) <= 120,
              "只给 from 时 to 贴当前时间且跨度不超过 1 小时", span)

    default_win = pages.get("logs-default-window.json")
    if default_win is not None:
        span = (parse_time(default_win["to"]) - parse_time(default_win["from"])).total_seconds()
        check(span == 3600, "默认窗口是 1 小时", span)
        now = dt.datetime.now(dt.timezone.utc)
        skew = abs((now - parse_time(default_win["to"])).total_seconds())
        check(skew <= 120, "默认窗口的 to 贴着当前时间", skew)

    limit5 = pages.get("logs-limit5.json")
    if limit5 is not None:
        check(len(limit5["entries"]) == 5, "limit=5 只回 5 条", len(limit5["entries"]))
        check(limit5["truncated"] is True,
              "limit=5 时 truncated=true（页不是全部，而是被 limit 截断）")
        check(isinstance(limit5.get("next_cursor"), str),
              "limit=5 时给了 next_cursor", limit5.get("next_cursor"))

    page_bytes = pages.get("logs-page-bytes.json")
    if page_bytes is not None:
        total = sum(len(e["message"].encode()) for e in page_bytes["entries"])
        check(page_bytes["truncated"] is True, "1 MiB 上限触发 truncated")
        check(page_bytes.get("truncated_reason") == "page_bytes",
              "截断原因是 page_bytes", page_bytes.get("truncated_reason"))
        check(total <= MAX_PAGE_BYTES + 4096,
              "单页消息累计不超过 1 MiB", total)
        check(len(page_bytes["entries"]) < 1000,
              "字节上限先于条数上限触发", len(page_bytes["entries"]))

    huge = pages.get("logs-huge.json")
    if huge is not None:
        binary = [e for e in huge["entries"] if e["binary"]]
        check(len(binary) >= 1, "超长消息被标成 binary（journald 没给字符串）", len(binary))
        check(all(e["message"] == "" for e in binary),
              "binary 条目的 message 是空串，不是一条空白日志行")

    kernel = pages.get("logs-kernel.json")
    if kernel is not None:
        check(kernel.get("source") == "kernel", "source 回显 kernel", kernel.get("source"))
        check(all(not e.get("unit") for e in kernel["entries"]),
              "内核条目没有 unit 字段（不是空串）")
        check(any("upanel-accept-kmsg-marker-1" in e["message"] for e in kernel["entries"]),
              "注入的内核日志能在 source=kernel 里读到")

    print("\n== 翻页 ==")
    try:
        with open(os.path.join(PAYLOAD_DIR, "page-count.txt"), encoding="utf-8") as fh:
            page_count = int(fh.read().strip() or "0")
    except (OSError, ValueError):
        bad("页数文件 page-count.txt 存在", "缺失或不是整数")
        page_count = 0
    if page_count > 0:
        all_cursors = []
        markers = {}
        coherent = True
        prev_cursor = None
        for i in range(1, page_count + 1):
            name = "logs-page-%d.json" % i
            body = load(name)
            if body is None:
                coherent = False
                continue
            data = envelope(name, body)
            if data is None or not validate_page(name, data):
                coherent = False
                continue
            entries = data["entries"]
            if prev_cursor is not None:
                check(not any(e["cursor"] == prev_cursor for e in entries),
                      "%s 没有重复上一页的边界条目" % name)
            prev_cursor = data.get("next_cursor")
            for e in entries:
                all_cursors.append(e["cursor"])
                if e["message"].startswith("marker-i="):
                    markers[e["message"]] = markers.get(e["message"], 0) + 1
        check(coherent, "全部 %d 页都是合法日志页" % page_count)
        check(len(all_cursors) == len(set(all_cursors)), "跨页游标全局唯一",
              len(all_cursors))
        missing = [k for k in ("marker-i=%d" % n for n in range(1, MARKERS + 1))
                   if k not in markers]
        dup = [k for k, n in markers.items() if n != 1]
        check(not missing, "翻页翻不丢条目", missing[:3])
        check(not dup, "同一个标记不会在两页里各出现一次", dup[:3])
        check(prev_cursor is None, "最后一页的 next_cursor 是 null", prev_cursor)

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
