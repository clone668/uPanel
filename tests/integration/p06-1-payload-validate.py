#!/usr/bin/env python3
"""P06.1 载荷校验：把验收 VM 上抓到的真实文件响应，按 CONTRACTS §11 与
api/openapi.yaml 逐项检查。

为什么单独一个程序：前端读的就是这几份 JSON，而“根是不是只有登记过的、
size_bytes 是不是字符串（uint64 超出 JS 安全整数）、目录项字段是不是闭集、
符号链接有没有被当成文件给出可读入口、错误码是不是约定的那一个”——
这些靠 curl + grep 看不出来，靠读代码说服不了人。只用 Python 标准库，
VM 上不需要装任何东西。

用法：
  python3 p06-1-payload-validate.py <payload 目录>

约定：目录里必须有一组固定名字的载荷（见下方 EXPECT_ERROR / SUCCESS_PAYLOADS）；
缺文件按 FAIL 处理，不跳过。

退出码：0 全部通过；1 有失败项。
"""

import json
import os
import re
import sys

PAYLOAD_DIR = sys.argv[1] if len(sys.argv) > 1 else "."

RFC3339_RE = re.compile(r"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?Z$")
VERSION_RE = re.compile(r"^v1-[0-9a-f]+-[0-9a-f]+-[0-9a-f]+-[0-9a-f]+$")
MODE_RE = re.compile(r"^[0-7]{4}$")
ROOT_ID_RE = re.compile(r"^[a-z][a-z0-9_-]{0,31}$")

TYPE_SET = {"file", "dir", "symlink", "other"}

ROOT_REQUIRED = {"root_id", "name", "path", "writable", "available"}
ROOT_OPTIONAL = {"unavailable_reason", "mount_point", "fs_type", "capacity", "capacity_error"}

ENTRY_REQUIRED = {"name", "type", "size_bytes", "mode", "owner", "mtime", "version"}

PAGE_REQUIRED = {"root_id", "path", "entries", "next_cursor", "truncated"}
TEXT_REQUIRED = {"root_id", "path", "version", "size_bytes", "encoding", "content"}

MAX_TEXT_BYTES = 1 << 20
PAGE_SIZE = 200

# 错误载荷 → 期望的错误码。每一次拒绝都要能被前端区分开：
# “路径越界”不是“参数写错”，也不是“服务器坏了”。
EXPECT_ERROR = {
    "err-unknown-root.json": "NOT_FOUND",
    "err-path-dotdot.json": "PATH_OUTSIDE_ROOT",
    "err-path-absolute.json": "PATH_OUTSIDE_ROOT",
    "err-path-mid.json": "PATH_OUTSIDE_ROOT",
    "err-path-empty-segment.json": "VALIDATION_ERROR",
    "err-path-dot.json": "VALIDATION_ERROR",
    "err-path-toolong.json": "VALIDATION_ERROR",
    "err-text-empty.json": "VALIDATION_ERROR",
    "err-symlink-text.json": "PATH_OUTSIDE_ROOT",
    "err-symlink-list.json": "PATH_OUTSIDE_ROOT",
    "err-magic-list.json": "PATH_OUTSIDE_ROOT",
    "err-hardlink-text.json": "PATH_OUTSIDE_ROOT",
    "err-fifo-text.json": "VALIDATION_ERROR",
    "err-dir-text.json": "VALIDATION_ERROR",
    "err-root-text.json": "VALIDATION_ERROR",
    "err-version-conflict.json": "VERSION_CONFLICT",
    "err-too-large.json": "VALIDATION_ERROR",
    "err-not-text.json": "FILE_NOT_TEXT",
    "err-nul-text.json": "FILE_NOT_TEXT",
    "err-xdev.json": "PATH_OUTSIDE_ROOT",
    "err-root-unavailable.json": "ROOT_UNAVAILABLE",
    "err-no-auth-roots.json": "UNAUTHENTICATED",
    "err-no-auth-list.json": "UNAUTHENTICATED",
    "err-no-auth-download.json": "UNAUTHENTICATED",
    "err-viewer-roots.json": "FORBIDDEN",
    "err-viewer-list.json": "FORBIDDEN",
    "err-viewer-text.json": "FORBIDDEN",
    "err-viewer-download.json": "FORBIDDEN",
}

# 错误载荷里绝不能出现的词：拒绝响应本身也不能顺手泄露根外的东西。
LEAK_WORDS = ("/etc/shadow", "do-not-leak", "root:x:")

PASSED = 0
FAILED = 0
FAILED_NAMES = []


def ok(name, detail=""):
    global PASSED
    PASSED += 1
    print("PASS  %s%s" % (name, ("（%s）" % (detail,)) if detail != "" else ""))


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


def validate_root(where, r):
    if not check(isinstance(r, dict), where + " 是对象", type(r).__name__):
        return
    keys = set(r)
    check(not (ROOT_REQUIRED - keys), where + " 必填字段齐全", "缺 %s" % sorted(ROOT_REQUIRED - keys))
    check(not (keys - ROOT_REQUIRED - ROOT_OPTIONAL), where + " 没有契约外的字段",
          sorted(keys - ROOT_REQUIRED - ROOT_OPTIONAL))
    check(isinstance(r.get("root_id"), str) and bool(ROOT_ID_RE.match(r["root_id"])),
          where + " 的 root_id 形状合法", r.get("root_id"))
    check(isinstance(r.get("name"), str) and r["name"] != "", where + " 的 name 非空")
    path = r.get("path")
    check(isinstance(path, str) and path.startswith("/"), where + " 的 path 是绝对路径", path)
    # 这一条就是“界面不能显示‘可管理整个服务器所有目录’”的数据来源：
    # 只要有一个根的 path 是 "/"，那句话在界面上就成立了。
    check(path != "/", where + " 的 path 不是 '/'（不是整个服务器）", path)
    check(not (isinstance(path, str) and ".." in path.split("/")),
          where + " 的 path 不含 '..' 段", path)
    check(isinstance(r.get("writable"), bool), where + " 的 writable 是布尔")
    check(isinstance(r.get("available"), bool), where + " 的 available 是布尔")
    if r.get("available") is False:
        check(isinstance(r.get("unavailable_reason"), str) and r["unavailable_reason"] != "",
              where + " 不可用时给出 unavailable_reason")
    cap = r.get("capacity")
    if cap is not None:
        check(isinstance(cap, dict), where + " 的 capacity 是对象或 null", type(cap).__name__)
        if isinstance(cap, dict):
            check(set(cap) == {"total_bytes", "available_bytes", "used_percent", "collected_at"},
                  where + " 的 capacity 字段是闭集", sorted(cap))
            for field in ("total_bytes", "available_bytes"):
                check(isinstance(cap.get(field), str) and cap[field].isdigit(),
                      where + " 的 capacity.%s 是十进制字符串" % field, cap.get(field))
            check(isinstance(cap.get("used_percent"), (int, float))
                  and not isinstance(cap.get("used_percent"), bool),
                  where + " 的 capacity.used_percent 是数字")
            check(is_rfc3339(cap.get("collected_at")),
                  where + " 的 capacity.collected_at 是 RFC3339 UTC", cap.get("collected_at"))
    else:
        check(isinstance(r.get("capacity_error"), str) and r["capacity_error"] != "",
              where + " capacity 为 null 时必须给出 capacity_error")


def validate_entry(where, e):
    if not check(isinstance(e, dict), where + " 是对象", type(e).__name__):
        return
    keys = set(e)
    check(not (ENTRY_REQUIRED - keys), where + " 必填字段齐全", "缺 %s" % sorted(ENTRY_REQUIRED - keys))
    # 闭集：目录项**不**带符号链接目标、不带内容的预览、不带绝对路径。
    check(not (keys - ENTRY_REQUIRED), where + " 没有契约外的字段", sorted(keys - ENTRY_REQUIRED))
    check(isinstance(e.get("name"), str) and e["name"] not in ("", ".", "..")
          and "/" not in e["name"], where + " 的 name 是单个名字段", e.get("name"))
    check(e.get("type") in TYPE_SET, where + " 的 type 在闭集内", e.get("type"))
    check(isinstance(e.get("size_bytes"), str) and e["size_bytes"].isdigit(),
          where + " 的 size_bytes 是十进制字符串（uint64 超出 JS 安全整数）",
          e.get("size_bytes"))
    check(isinstance(e.get("mode"), str) and bool(MODE_RE.match(e["mode"])),
          where + " 的 mode 是四位八进制", e.get("mode"))
    owner = e.get("owner")
    if check(isinstance(owner, dict), where + " 有 owner 对象", type(owner).__name__):
        check(set(owner) == {"uid", "gid"}, where + " 的 owner 字段是闭集", sorted(owner))
        for field in ("uid", "gid"):
            value = owner.get(field)
            check(isinstance(value, int) and not isinstance(value, bool) and value >= 0,
                  where + " 的 owner.%s 是非负整数" % field, value)
    check(is_rfc3339(e.get("mtime")), where + " 的 mtime 是 RFC3339 UTC", e.get("mtime"))
    check(isinstance(e.get("version"), str) and bool(VERSION_RE.match(e["version"])),
          where + " 的 version 形状是 v1-<dev>-<ino>-<size>-<mtime>", e.get("version"))


def validate_page(name, data, expect_root, expect_path):
    check(data.get("root_id") == expect_root, name + " 的 root_id", data.get("root_id"))
    check(data.get("path") == expect_path, name + " 的 path 是根内相对路径", data.get("path"))
    entries = data.get("entries")
    if not check(isinstance(entries, list), name + " 的 entries 是数组",
                 type(entries).__name__):
        return
    check(isinstance(data.get("truncated"), bool), name + " 的 truncated 是布尔")
    nc = data.get("next_cursor")
    check(nc is None or isinstance(nc, str), name + " 的 next_cursor 是字符串或 null", nc)
    names = [e.get("name") for e in entries if isinstance(e, dict)]
    check(names == sorted(names), name + " 的 entries 按名字有序", names[:3])
    check(len(set(names)) == len(names), name + " 的 entries 没有重名")
    for e in entries:
        validate_entry(name + " 的目录项 %r" % (e.get("name") if isinstance(e, dict) else e,), e)
    return entries


def read_headers(name):
    path = os.path.join(PAYLOAD_DIR, name)
    headers = {}
    order = []
    try:
        with open(path, encoding="latin-1") as fh:
            for line in fh:
                line = line.rstrip("\r\n")
                if ":" not in line:
                    continue
                key, _, value = line.partition(":")
                key = key.strip().lower()
                headers[key] = value.strip()
                order.append(key)
    except FileNotFoundError:
        bad("头部文件存在：" + name, "文件缺失")
        return {}, []
    return headers, order


# ---------------------------------------------------------------- 起点
print("== 载荷目录：%s ==" % os.path.abspath(PAYLOAD_DIR))

# 1. 错误载荷：每一条拒绝都要有自己的错误码，且不泄露根外内容。
print("\n== 错误载荷（%d 份） ==" % len(EXPECT_ERROR))
for name, code in sorted(EXPECT_ERROR.items()):
    body = load(name)
    if body is None:
        continue
    err = error_envelope(name, body)
    if err is None:
        continue
    check(err.get("code") == code, name + " 的错误码是 " + code, err.get("code"))
    message = err.get("message")
    check(isinstance(message, str) and message != "", name + " 有非空 message")
    raw = json.dumps(body, ensure_ascii=False)
    leaked = [w for w in LEAK_WORDS if w in raw]
    check(not leaked, name + " 没有泄露根外内容", leaked)

# 2. 根列表：只有登记过的根。
print("\n== 文件根 ==")
body = load("files-roots.json")
data = envelope("files-roots.json", body) if body else None
if data is not None:
    roots = data.get("roots")
    if check(isinstance(roots, list), "文件根是数组", type(roots).__name__):
        check(len(roots) > 0, "至少登记了一个文件根", len(roots))
        for r in roots:
            validate_root("文件根 %r" % (r.get("root_id") if isinstance(r, dict) else r,), r)
        ids = [r.get("root_id") for r in roots if isinstance(r, dict)]
        check(len(set(ids)) == len(ids), "文件根 id 不重复", ids)
        check("nope" not in ids, "未登记的 root_id 不在列表里")

body = load("files-roots-after.json")
data = envelope("files-roots-after.json", body) if body else None
if data is not None:
    roots = {r.get("root_id"): r for r in (data.get("roots") or []) if isinstance(r, dict)}
    flip = roots.get("flip")
    if check(flip is not None, "flip 根仍在列表里（根不可用不等于消失）"):
        check(flip.get("available") is False, "flip 根被标记为不可用", flip.get("available"))
        check(isinstance(flip.get("unavailable_reason"), str) and flip["unavailable_reason"] != "",
              "flip 根给出不可用原因")

# 3. 目录页。
print("\n== 目录列举 ==")
body = load("files-list-root.json")
data = envelope("files-list-root.json", body) if body else None
entries = validate_page("files-list-root.json", data, "work", "") if data is not None else None
if entries is not None:
    by_name = {e.get("name"): e for e in entries if isinstance(e, dict)}
    link = by_name.get("link-secret")
    if check(link is not None, "符号链接出现在列表里"):
        check(link.get("type") == "symlink", "符号链接的 type 是 symlink", link.get("type"))
    fifo = by_name.get("fifo")
    if check(fifo is not None, "FIFO 出现在列表里"):
        check(fifo.get("type") == "other", "FIFO 的 type 是 other", fifo.get("type"))
    sub = by_name.get("sub")
    if check(sub is not None, "子目录出现在列表里"):
        check(sub.get("type") == "dir", "子目录的 type 是 dir", sub.get("type"))
    conf = by_name.get("hostname")
    if check(conf is not None, "普通文件出现在列表里"):
        check(conf.get("type") == "file", "普通文件的 type 是 file", conf.get("type"))
    # 目录项里不该出现符号链接的目标：那等于把根外的路径显示出来。
    check(not any("target" in e for e in entries if isinstance(e, dict)),
          "目录项不带符号链接目标字段")

# 分页的语义：page1 满页 200 且给出 next_cursor，page2 是尾页（250 = 200 + 50）
# 且 next_cursor 为 null。尾页给空串会让界面显示一个点不动的“加载更多”。
for name in ("files-list-page1.json", "files-list-page2.json"):
    body = load(name)
    data = envelope(name, body) if body else None
    if data is None:
        continue
    ents = validate_page(name, data, "work", "pages")
    if ents is None:
        continue
    if name.endswith("page1.json"):
        check(len(ents) == PAGE_SIZE, name + " 的条数是一页 %d" % PAGE_SIZE, len(ents))
        check(isinstance(data.get("next_cursor"), str) and data["next_cursor"] != "",
              name + " 满页时给出 next_cursor", data.get("next_cursor"))
    else:
        check(len(ents) == 50, name + " 是尾页（250 - 200 = 50）", len(ents))
        check(data.get("next_cursor") is None, name + " 尾页的 next_cursor 是 null",
              data.get("next_cursor"))

body = load("files-flip-before.json")
data = envelope("files-flip-before.json", body) if body else None
if data is not None:
    validate_page("files-flip-before.json", data, "flip", "")

body = load("files-truncated.json")
data = envelope("files-truncated.json", body) if body else None
if data is not None:
    validate_page("files-truncated.json", data, "shm", "")
    check(data.get("truncated") is True, "超大目录如实标记 truncated")

# 4. 文本。
print("\n== 文本读取 ==")
for name in ("files-text-ok.json", "files-text-version-ok.json"):
    body = load(name)
    data = envelope(name, body) if body else None
    if data is None:
        continue
    check(not (TEXT_REQUIRED - set(data)), name + " 字段齐全",
          sorted(TEXT_REQUIRED - set(data)))
    check(not (set(data) - TEXT_REQUIRED), name + " 没有契约外的字段",
          sorted(set(data) - TEXT_REQUIRED))
    check(data.get("root_id") == "work", name + " 的 root_id")
    check(data.get("path") == "sub/sshd_config", name + " 的 path", data.get("path"))
    check(isinstance(data.get("version"), str) and bool(VERSION_RE.match(data["version"])),
          name + " 的 version 形状合法", data.get("version"))
    check(isinstance(data.get("size_bytes"), str) and data["size_bytes"].isdigit(),
          name + " 的 size_bytes 是十进制字符串", data.get("size_bytes"))
    check(data.get("encoding") == "utf-8", name + " 的 encoding 是 utf-8", data.get("encoding"))
    content = data.get("content")
    check(isinstance(content, str) and len(content.encode()) <= MAX_TEXT_BYTES,
          name + " 的 content 不超过 1 MiB")
    check(isinstance(content, str) and "\x00" not in content, name + " 的 content 不含 NUL")

body = load("files-text-ok.json")
data = envelope("files-text-ok.json", body) if body else None
if data is not None:
    content = data.get("content") or ""
    check(content == "Port 22\nPermitRootLogin no\n", "文本内容与磁盘上一致", repr(content)[:80])
    check(str(len(content.encode())) == data.get("size_bytes"),
          "size_bytes 与 content 的字节数一致", (data.get("size_bytes"), len(content.encode())))

# 5. 下载：头部自洽，字节数与 Content-Length 一致。
print("\n== 下载 ==")
headers, order = read_headers("files-download.headers")
if headers:
    ctype = headers.get("content-type", "")
    check(ctype == "application/octet-stream", "Content-Type 是 octet-stream", ctype)
    check(headers.get("x-content-type-options") == "nosniff", "带 nosniff",
          headers.get("x-content-type-options"))
    check("no-store" in headers.get("cache-control", ""), "带 no-store",
          headers.get("cache-control"))
    version = headers.get("x-upanel-file-version", "")
    check(bool(VERSION_RE.match(version)), "带合法 X-UPanel-File-Version", version)
    cd = headers.get("content-disposition", "")
    check(cd.startswith("attachment;"), "Content-Disposition 是 attachment", cd[:60])
    check("filename*=UTF-8''" in cd, "Content-Disposition 带 RFC 5987 文件名", cd[:120])
    check("\n" not in cd and "\r" not in cd, "Content-Disposition 没有换行")
    # ASCII 退化名只允许可打印 ASCII：中文名不能变成裸字节（那会破坏头部）。
    ascii_part = cd.split("filename=", 1)[-1].split(";", 1)[0].strip().strip('"')
    check(all(0x20 <= ord(ch) < 0x7f for ch in ascii_part),
          "Content-Disposition 的 ASCII 退化名只含可打印 ASCII", ascii_part)
    length = headers.get("content-length", "")
    path = os.path.join(PAYLOAD_DIR, "files-download.bin")
    try:
        size = os.path.getsize(path)
    except OSError:
        size = -1
    check(length.isdigit() and int(length) == size,
          "Content-Length 与响应体字节数一致", (length, size))
    try:
        with open(path, "rb") as fh:
            raw = fh.read()
    except OSError:
        raw = b""
    check(raw == b"Port 22\nPermitRootLogin no\n", "下载的字节与磁盘文件逐字节一致",
          repr(raw)[:80])

headers, order = read_headers("err-download-symlink.headers")
if headers:
    ctype = headers.get("content-type", "")
    check(ctype.startswith("application/json"), "越界下载回的是 JSON，不是文件流", ctype)
    check("octet-stream" not in ctype, "越界下载没有 octet-stream", ctype)
body = load("err-download-symlink.json")
if body is not None:
    err = error_envelope("err-download-symlink.json", body)
    if err is not None:
        check(err.get("code") == "PATH_OUTSIDE_ROOT", "越界下载的错误码",
              err.get("code"))

# 6. 代理能力表：给什么就报什么，不多报。
print("\n== 代理能力表 ==")
body = load("agent-capabilities.json")
data = envelope("agent-capabilities.json", body) if body else None
if data is not None:
    queries = set(data.get("supported_queries") or [])
    for q in ("files.roots", "files.list", "files.text"):
        check(q in queries, "能力表登记了 " + q)
    # 文件模块只有“读”：能力表里不该出现写操作。
    check(not any(q.startswith("files.") and not q.split(".")[1] in
                  ("roots", "list", "text") for q in queries),
          "能力表里没有 files 的写操作", sorted(q for q in queries if q.startswith("files.")))

# 7. 配置拒绝 "/" 根的真实输出。
print("\n== 配置守卫 ==")
path = os.path.join(PAYLOAD_DIR, "root-slash-rejected.txt")
try:
    with open(path, encoding="utf-8") as fh:
        text = fh.read()
except FileNotFoundError:
    bad("载荷存在：root-slash-rejected.txt", "文件缺失")
    text = ""
if text:
    check("path" in text and "/" in text, "拒绝消息指向 path=\"/\"", text.strip()[:120])
    check("文件根" in text or "root" in text.lower(), "拒绝消息说的是文件根", text.strip()[:120])

print("\n通过 %d 项，失败 %d 项" % (PASSED, FAILED))
if FAILED:
    print("失败项：%s" % " | ".join(FAILED_NAMES))
    sys.exit(1)
sys.exit(0)
