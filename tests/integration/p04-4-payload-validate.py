#!/usr/bin/env python3
"""P04.4 载荷校验：把验收 VM 上抓到的真实响应，按 CONTRACTS §6/§6.2 与 api/openapi.yaml 检查一遍，
并和系统自带工具（lsblk / df / smartctl / /proc）的结果对照。

为什么单独一个程序：前端读的就是这几份 JSON。字节能不能被当成数字、
未知容量是不是 null、健康是不是闭集、有没有写动作 —— 都是界面能不能如实显示的前提。
这里只用 Python 标准库，VM 上不需要装任何东西。

用法：
  python3 p04-4-payload-validate.py <payload 目录>

约定：目录里必须有 summary.json、topology.json、devices.json，
以及若干 detail-<id>.json。缺文件按 FAIL 处理，不跳过。

退出码：0 全部通过；1 有失败项。
"""

import json
import os
import re
import subprocess
import sys

PAYLOAD_DIR = sys.argv[1] if len(sys.argv) > 1 else "."

BYTE_RE = re.compile(r"^(0|[1-9][0-9]*)$")
SHA_RE = re.compile(r"^sha256:[0-9a-f]{64}$")
RFC3339_RE = re.compile(r"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})$")

HEALTH_SET = {"healthy", "warning", "critical", "unknown", "unsupported", "sleeping"}
SECTION_SET = {"ok", "error", "unavailable"}
RELATION_SET = {"partition_of", "depends_on"}
# transport 是 str|null：来自 sysfs，不按名字猜；分区与逻辑设备的 transport 通常是 null。
TRANSPORT_SET = {"ata", "nvme", "usb", "virtio", "scsi", "mmc", "unknown"}
KIND_SET = {"disk", "partition", "lvm", "raid", "crypt", "loop", "rom", "other"}

DEVICE_REQUIRED = [
    "id", "kind", "name", "size_bytes", "logical_sector_bytes", "physical_sector_bytes",
    "model", "serial", "wwn", "transport", "rotational", "read_only", "online",
    "identity_confidence", "identity_fingerprint", "filesystem", "partition_table",
    "health", "capabilities", "protection", "io", "collected_at",
]
DEVICE_ALLOWED = set(DEVICE_REQUIRED) | {"path", "vendor", "warnings", "swap", "holders"}

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


def check(name, cond, detail=""):
    if cond:
        ok(name)
    else:
        bad(name, detail)
    return cond


def unwrap(body):
    """Web 的响应统一包在 {data, request_id} 里（DataEnvelope）；前端 client 也只读 data。
    这里照同样的口径取 data，不把外壳当成契约的一部分。"""
    if isinstance(body, dict) and "data" in body and "request_id" in body:
        return body["data"]
    return body


def load(name):
    path = os.path.join(PAYLOAD_DIR, name)
    with open(path, "r", encoding="utf-8") as fh:
        return unwrap(json.load(fh))


def is_byte(value):
    return isinstance(value, str) and bool(BYTE_RE.match(value))


def is_int(v):
    return isinstance(v, int) and not isinstance(v, bool)


def sh(cmd):
    out = subprocess.run(cmd, shell=True, capture_output=True, text=True)
    return out.returncode, out.stdout, out.stderr


# ------------------------------------------------------------------ 公共不变量

def check_keys(name, obj, required, allowed):
    missing = [k for k in required if k not in obj]
    extra = [k for k in obj if k not in allowed]
    check("%s 字段齐全" % name, not missing, "缺少 %s" % missing)
    check("%s 没有契约外字段" % name, not extra, "多出 %s" % extra)


def check_device(name, dev):
    check_keys(name + " 设备字段", dev, DEVICE_REQUIRED, DEVICE_ALLOWED)

    check("%s kind 在闭集内" % name, dev.get("kind") in KIND_SET, dev.get("kind"))
    transport = dev.get("transport")
    check("%s transport 是已知取值或 null" % name,
          transport is None or transport in TRANSPORT_SET, transport)
    check("%s size_bytes 是十进制字符串" % name, is_byte(dev.get("size_bytes")), dev.get("size_bytes"))
    check("%s collected_at 是 RFC3339" % name,
          isinstance(dev.get("collected_at"), str) and RFC3339_RE.match(dev["collected_at"]),
          dev.get("collected_at"))
    for flag in ("rotational", "read_only", "online"):
        check("%s %s 是布尔" % (name, flag), isinstance(dev.get(flag), bool), dev.get(flag))

    # 身份：没有序列号时 identity_confidence 必须说出来，不能装作确定。
    conf = dev.get("identity_confidence")
    check("%s identity_confidence 在闭集内" % name,
          conf in {"high", "medium", "low", "unknown"}, conf)
    check("%s identity_fingerprint 是 sha256" % name,
          isinstance(dev.get("identity_fingerprint"), str)
          and bool(SHA_RE.match(dev["identity_fingerprint"])),
          dev.get("identity_fingerprint"))

    # 健康：闭集 + 温度不造假 + healthy 必须有依据。
    health = dev.get("health") or {}
    check_keys(name + " health", health,
               ["status", "temperature_celsius", "source", "checked_at", "details"],
               {"status", "temperature_celsius", "source", "checked_at", "details"})
    status = health.get("status")
    check("%s health.status 在闭集内" % name, status in HEALTH_SET, status)
    temp = health.get("temperature_celsius")
    check("%s 温度是 null 或正数（不得为 0）" % name,
          temp is None or (isinstance(temp, (int, float)) and not isinstance(temp, bool) and temp > 0),
          temp)
    if status == "healthy":
        check("%s healthy 有采集来源" % name, health.get("source") in {"smartctl", "nvme"},
              health.get("source"))
        check("%s healthy 有检查时间" % name, isinstance(health.get("checked_at"), str),
              health.get("checked_at"))
    if dev.get("kind") in {"partition", "loop", "rom"}:
        check("%s 非物理盘的 health 是 unsupported" % name, status == "unsupported", status)

    # I/O：每台设备只报自己那一行；字节是字符串；速率要么完整要么 null。
    io = dev.get("io")
    if not isinstance(io, dict):
        bad("%s io 存在" % name, io)
    else:
        check_keys(name + " io", io,
                   ["sampled_at", "read_bytes_total", "write_bytes_total", "reads_total",
                    "writes_total", "rate"],
                   {"sampled_at", "read_bytes_total", "write_bytes_total", "reads_total",
                    "writes_total", "rate"})
        for field in ("read_bytes_total", "write_bytes_total", "reads_total", "writes_total"):
            check("%s io.%s 是十进制字符串" % (name, field), is_byte(io.get(field)), io.get(field))
        rate = io.get("rate")
        if rate is None:
            ok("%s io.rate 允许为 null（本次没有采样窗口）" % name)
        else:
            check_keys(name + " io.rate", rate,
                       ["interval_ms", "read_bytes_per_second", "write_bytes_per_second",
                        "read_iops", "write_iops", "read_latency_ms", "write_latency_ms"],
                       {"interval_ms", "read_bytes_per_second", "write_bytes_per_second",
                        "read_iops", "write_iops", "read_latency_ms", "write_latency_ms"})
            check("%s io.rate.interval_ms ≥ 1" % name,
                  is_int(rate.get("interval_ms")) and rate["interval_ms"] >= 1,
                  rate.get("interval_ms"))
            check("%s 速率用十进制字符串" % name,
                  is_byte(rate.get("read_bytes_per_second")) and is_byte(rate.get("write_bytes_per_second")),
                  rate.get("read_bytes_per_second"))
            for field in ("read_latency_ms", "write_latency_ms"):
                value = rate.get(field)
                check("%s %s 是 null 或非负数" % (name, field),
                      value is None or (isinstance(value, (int, float)) and not isinstance(value, bool) and value >= 0),
                      value)

    # 容量：每一份都是字节字符串；used_percent 与 inode 百分比都在 0—100。
    fs = dev.get("filesystem")
    if fs is None:
        ok("%s 没有文件系统信息（未挂载或读不到就是 null）" % name)
        fs = {}
    else:
        check_keys(name + " filesystem", fs, ["type", "uuid", "label", "mounts"],
                   {"type", "uuid", "label", "mounts"})
    for mount in fs.get("mounts") or []:
        check_keys(name + " mount", mount,
                   ["path", "source", "fstype", "options", "read_only", "capacity"],
                   {"path", "source", "fstype", "options", "read_only", "capacity"})
        cap = mount.get("capacity")
        if cap is None:
            ok("%s 挂载 %s 容量为 null（读不到就是 null，不是 0）" % (name, mount.get("path")))
            continue
        check_keys(name + " capacity", cap,
                   ["total_bytes", "available_bytes", "used_percent", "inode_total",
                    "inode_available", "inode_used_percent", "collected_at"],
                   {"total_bytes", "available_bytes", "used_percent", "inode_total",
                    "inode_available", "inode_used_percent", "collected_at"})
        for field in ("total_bytes", "available_bytes", "inode_total", "inode_available"):
            check("%s capacity.%s 是十进制字符串" % (name, field), is_byte(cap.get(field)), cap.get(field))
        if is_byte(cap.get("total_bytes")) and is_byte(cap.get("available_bytes")):
            check("%s 可用容量不超过总容量" % name,
                  int(cap["available_bytes"]) <= int(cap["total_bytes"]),
                  "%s > %s" % (cap["available_bytes"], cap["total_bytes"]))
        for field in ("used_percent", "inode_used_percent"):
            value = cap.get(field)
            check("%s capacity.%s 在 0—100" % (name, field),
                  isinstance(value, (int, float)) and not isinstance(value, bool) and 0 <= value <= 100,
                  value)

    # 保护：系统盘判定必须给理由。
    protection = dev.get("protection") or {}
    check_keys(name + " protection", protection,
               ["system_disk", "reasons", "sources"], {"system_disk", "reasons", "sources"})
    if protection.get("system_disk"):
        check("%s 系统盘保护给出理由" % name, len(protection.get("reasons") or []) > 0,
              protection.get("reasons"))

    # 本阶段不开放任何写动作：capabilities 必须是空数组。
    caps = dev.get("capabilities")
    check("%s capabilities 是数组" % name, isinstance(caps, list), type(caps).__name__)
    if isinstance(caps, list):
        check("%s 本阶段没有任何可用写动作" % name, caps == [], caps)

    # 分区表：分区不许比整盘还大（粗查“不重复累计/不错位”）。
    table = dev.get("partition_table")
    if table is None:
        pass  # 分区表读不到/不适用（分区、loop、光驱）时为 null，契约允许。
    else:
        check_keys(name + " partition_table", table,
                   ["type", "partitions", "free_regions", "source"],
                   {"type", "partitions", "free_regions", "source", "warning"})
        if dev.get("kind") == "disk" and is_byte(dev.get("size_bytes")):
            total = int(dev["size_bytes"])
            for part in table.get("partitions") or []:
                if is_byte(part.get("size_bytes")):
                    check("%s 分区 %s 不超过整盘" % (name, part.get("device_id")),
                          int(part["size_bytes"]) <= total,
                          "%s > %s" % (part["size_bytes"], total))
                check("%s 分区 start_bytes 是十进制字符串" % name,
                      is_byte(part.get("start_bytes")), part.get("start_bytes"))


# ------------------------------------------------------------------ 主流程

summary = load("summary.json")
topology = load("topology.json")
devices = load("devices.json")

print("== A. /system/summary ==")
check_keys("/system/summary", summary,
           ["collected_at", "interval_ms", "stale", "degraded", "host", "cpu", "memory", "load", "network"],
           {"collected_at", "interval_ms", "stale", "degraded", "host", "cpu", "memory", "load", "network"})
check("interval_ms ≥ 1", is_int(summary.get("interval_ms")) and summary["interval_ms"] >= 1,
      summary.get("interval_ms"))
for flag in ("stale", "degraded"):
    check("%s 是布尔" % flag, isinstance(summary.get(flag), bool), summary.get(flag))

section_status = {}
for section in ("host", "cpu", "memory", "load", "network"):
    body = summary.get(section) or {}
    section_status[section] = body.get("status")
    check("段 %s.status 在闭集内" % section, body.get("status") in SECTION_SET, body.get("status"))
    if body.get("status") != "ok":
        check("段 %s 失败时带原因" % section, isinstance(body.get("error"), str), body.get("error"))

check("degraded 与各段状态一致",
      bool(summary.get("degraded")) == any(s != "ok" for s in section_status.values()),
      section_status)

memory = summary.get("memory") or {}
if memory.get("status") == "ok":
    total, used, avail = (memory.get("total_bytes"), memory.get("used_bytes"),
                          memory.get("available_bytes"))
    check("内存字节是十进制字符串",
          is_byte(total) and is_byte(used) and is_byte(avail),
          (total, used, avail))
    if is_byte(total) and is_byte(used):
        check("已用内存不超过总量", int(used) <= int(total), "%s > %s" % (used, total))

load = summary.get("load") or {}
if load.get("status") == "ok":
    for field in ("load1", "load5", "load15"):
        value = load.get(field)
        check("%s 是非负数" % field,
              isinstance(value, (int, float)) and not isinstance(value, bool) and value >= 0, value)

cpu = summary.get("cpu") or {}
if cpu.get("status") == "ok":
    check("logical_cores ≥ 1", is_int(cpu.get("logical_cores")) and cpu["logical_cores"] >= 1,
          cpu.get("logical_cores"))
    per_cpu = cpu.get("per_cpu") or []
    check("per_cpu 数量与 logical_cores 一致", len(per_cpu) == cpu.get("logical_cores"),
          "%s vs %s" % (len(per_cpu), cpu.get("logical_cores")))
    for core in per_cpu:
        check("核 %s 的 index 是整数" % core.get("index"), is_int(core.get("index")), core.get("index"))
        usage = core.get("usage_percent")
        check("核 %s 使用率是 null 或 0—100" % core.get("index"),
              usage is None or (isinstance(usage, (int, float)) and not isinstance(usage, bool)
                                and 0 <= usage <= 100),
              usage)

print()
print("== B. /storage/topology 与 /storage/devices 是同一份快照 ==")
check("topology.snapshot_version 形如 sha256:<64hex>",
      isinstance(topology.get("snapshot_version"), str) and bool(SHA_RE.match(topology["snapshot_version"])),
      topology.get("snapshot_version"))
check("devices.snapshot_version 与 topology 相同",
      devices.get("snapshot_version") == topology.get("snapshot_version"),
      (devices.get("snapshot_version"), topology.get("snapshot_version")))
check("devices.collected_at 与 topology 相同",
      devices.get("collected_at") == topology.get("collected_at"),
      (devices.get("collected_at"), topology.get("collected_at")))
check("devices.interval_ms 与 topology 相同",
      devices.get("interval_ms") == topology.get("interval_ms"),
      (devices.get("interval_ms"), topology.get("interval_ms")))
check("topology.status 为 ok", topology.get("status") == "ok", topology.get("error"))

nodes = topology.get("nodes") or []
listed = devices.get("devices") or []
check("拓扑节点数 = 设备列表条数", len(nodes) == len(listed), "%s vs %s" % (len(nodes), len(listed)))
ids = [d.get("id") for d in listed]
check("设备 id 唯一", len(ids) == len(set(ids)), [i for i in ids if ids.count(i) > 1])

print()
print("== C. 每台设备（字段、健康、I/O、容量、保护、写动作） ==")
for dev in listed:
    check_device(dev.get("id") or "?", dev)

edges = topology.get("edges") or []
for edge in edges:
    check_keys("边 %s→%s" % (edge.get("from_device_id"), edge.get("to_device_id")), edge,
               ["from_device_id", "to_device_id", "relation"],
               {"from_device_id", "to_device_id", "relation"})
    check("边关系在闭集内", edge.get("relation") in RELATION_SET, edge.get("relation"))
    check("边的两端都在节点里",
          edge.get("from_device_id") in ids and edge.get("to_device_id") in ids,
          edge.get("from_device_id"))

mounts = topology.get("network_mounts") or []
for mount in mounts:
    check_keys("网络挂载 %s" % mount.get("path"), mount,
               ["path", "source", "fstype", "read_only", "reachable", "capacity"],
               {"path", "source", "fstype", "read_only", "reachable", "capacity", "capacity_error"})
    reachable = mount.get("reachable")
    check("网络挂载 reachable 只能是 true/false/null",
          reachable is None or isinstance(reachable, bool), reachable)
    if reachable is False:
        check("不可达时必须说明原因", isinstance(mount.get("capacity_error"), str),
              mount.get("capacity_error"))
    if reachable is not True:
        check("不是 true 时容量必须是 null", mount.get("capacity") is None, mount.get("capacity"))

print()
print("== D. 设备详情 ==")
detail_files = sorted(f for f in os.listdir(PAYLOAD_DIR) if f.startswith("detail-") and f.endswith(".json"))
if not detail_files:
    bad("详情载荷存在", "没有 detail-*.json")
for filename in detail_files:
    with open(os.path.join(PAYLOAD_DIR, filename), "r", encoding="utf-8") as fh:
        detail = unwrap(json.load(fh))
    dev = detail.get("device") or {}
    label = "详情 %s" % dev.get("id")
    check_keys(label, detail,
               ["status", "snapshot_version", "collected_at", "interval_ms", "device", "edges"],
               {"status", "snapshot_version", "collected_at", "interval_ms", "device", "edges", "error"})
    check("%s snapshot_version 与列表一致" % label,
          detail.get("snapshot_version") == devices.get("snapshot_version"),
          detail.get("snapshot_version"))
    check_device(label, dev)
    for edge in detail.get("edges") or []:
        check("%s 的边与这台设备直接相关" % label,
              dev.get("id") in (edge.get("from_device_id"), edge.get("to_device_id")),
              (edge.get("from_device_id"), edge.get("to_device_id")))

print()
print("== E. 与系统自带工具对照 ==")
# 在开发宿主（Windows/Git Bash）上跑同一份校验时没有这些工具：如实标 SKIP，
# 而不是判 FAIL——判 FAIL 会把“工具不存在”和“数据不对”混在一起。
HAVE_LSBLK = sh("command -v lsblk")[1].strip() != ""
HAVE_DF = sh("command -v df")[1].strip() != "" and os.path.exists("/proc/self/mountinfo")
if not (HAVE_LSBLK and HAVE_DF):
    print("SKIP  lsblk/df 交叉验证：本机没有 %s（只做契约检查）"
          % ", ".join(n for n, okk in (("lsblk", HAVE_LSBLK), ("df", HAVE_DF)) if not okk))

# E1 磁盘集合：DTO 里 kind=disk 的 id，应该等于 lsblk 里既不是 loop 也不是 rom 的整盘。
# 用 -P（key="value" 成对输出）而不是默认的树形输出：树形会把分区画成 "├─sda1"，
# 也会因为默认隐藏 loop 设备而漏掉它们（-a 才会列出来）。两种都会让对照失真。
rc, out, err = (1, "", "skip") if not HAVE_LSBLK else sh("lsblk -a -P -o NAME,TYPE")
if rc != 0:
    if HAVE_LSBLK:
        bad("lsblk 可用", err)
else:
    lsblk_type = {}
    for line in out.splitlines():
        match = re.match(r'NAME="([^"]+)"\s+TYPE="([^"]+)"', line.strip())
        if match:
            lsblk_type[match.group(1)] = match.group(2)
    dto_kind = {d["id"]: d.get("kind") for d in listed}
    seen = set(lsblk_type)
    ok("lsblk 看到 %d 台块设备（其中 %d 块整盘）"
       % (len(seen), sum(1 for t in lsblk_type.values() if t == "disk")))

    # 1) 代理说它是整盘的，lsblk 必须也认为是 disk。
    wrong = {i: dto_kind[i] for i in dto_kind
             if dto_kind[i] == "disk" and lsblk_type.get(i) != "disk"}
    check("kind=disk 的设备在 lsblk 里也是 disk", not wrong, wrong)

    # 2) 覆盖：lsblk 的每台块设备（含分区与 loop）都要出现在 DTO 里——漏设备是硬伤。
    missing = sorted(seen - set(dto_kind))
    check("lsblk 的每台块设备都在 DTO 里", not missing, "缺少 %s" % missing)

    # 3) 反向：DTO 里的 id 必须是内核真的有的设备。
    extra = sorted(set(dto_kind) - seen)
    check("DTO 里没有内核不认识的设备", not extra, "多出 %s" % extra)

    # 4) 类型对得上：分区就是 part，整盘就是 disk（fd0 这类软驱代理按 other 归类，不强求）。
    kind_to_lsblk = {"partition": "part", "disk": "disk"}
    mismatched = {i: (dto_kind[i], lsblk_type.get(i)) for i in dto_kind
                  if i in lsblk_type and i in kind_to_lsblk
                  and kind_to_lsblk[dto_kind[i]] != lsblk_type[i]}
    check("分区/整盘的类型与 lsblk 一致", not mismatched, mismatched)

# E2 整盘字节数：与 lsblk -b 比，不能差。
rc, out, _ = (1, "", "") if not HAVE_LSBLK else sh("lsblk -bdn -o NAME,SIZE")
if rc == 0:
    sizes = {}
    for line in out.splitlines():
        parts = line.split()
        if len(parts) >= 2 and parts[1].isdigit():
            sizes[parts[0]] = int(parts[1])
    for dev in listed:
        if dev.get("kind") != "disk" or not is_byte(dev.get("size_bytes")):
            continue
        want = sizes.get(dev["id"])
        check("%s size_bytes 与 lsblk -b 一致" % dev["id"], want == int(dev["size_bytes"]),
              "DTO %s / lsblk %s" % (dev["size_bytes"], want))

# E3 挂载容量：与 df -B1 比（同一个挂载点、同一份字节数）。
for dev in listed:
    for mount in (dev.get("filesystem") or {}).get("mounts") or []:
        cap = mount.get("capacity")
        path = mount.get("path")
        if not isinstance(cap, dict) or not path or not os.path.isdir(path) or not HAVE_DF:
            continue
        rc, out, _ = sh("df -B1 --output=size %s" % json.dumps(path))
        if rc != 0:
            bad("%s 的 df 读数" % path, "df 失败")
            continue
        lines = [ln.strip() for ln in out.splitlines() if ln.strip()]
        if len(lines) < 2 or not lines[1].isdigit():
            bad("%s 的 df 读数" % path, out)
            continue
        check("%s 容量与 df -B1 一致" % path, int(lines[1]) == int(cap["total_bytes"]),
              "DTO %s / df %s" % (cap["total_bytes"], lines[1]))

# E4 健康：与 smartctl 自己的结论比。
#
# 必须用**产品自己那组参数**去读，否则比的不是同一件事：只给 `-H` 时，
# QEMU 的 SCSI 盘会返回 `smart_status.passed=true`；加上 `-i -A` 后同一块盘
# 是 `smart_support.available=false`、没有 smart_status。要判“产品有没有
# 把读不到说成正常”，两边就得看同一份输出。
smartctl = sh("command -v smartctl")[1].strip()
if not smartctl:
    print("SKIP  与 smartctl 交叉验证：本机没有 smartctl（记为未执行）")
else:
    unchecked = [d["id"] for d in listed
                 if d.get("kind") == "disk" and not (d.get("health") or {}).get("checked_at")]
    if unchecked:
        bad("有 smartctl 的情况下整盘都做过一次健康采集（checked_at 不为 null）",
            "还是没有 checked_at：%s（这几块盘的 health 只是占位）" % " ".join(unchecked))
    else:
        ok("有 smartctl 的情况下整盘都做过一次健康采集（checked_at 不为 null）")
    compared = 0
    readable = []
    for dev in listed:
        if dev.get("kind") != "disk":
            continue
        did = dev["id"]
        # NVMe 不接受 -n standby（smartctl 会以命令行错误退出），其余盘带上它以免唤醒休眠盘。
        args = "%s -j -i -H -A" % smartctl
        if not did.startswith("nvme"):
            args += " -n standby"
        rc, out, _ = sh("%s /dev/%s" % (args, did))
        if rc not in (0, 4):
            print("SKIP  %s 的 smartctl 读数（退出码 %s：桥接不支持/休眠/无权限）" % (did, rc))
            continue
        try:
            report = json.loads(out)
        except ValueError:
            bad("%s 的 smartctl 输出可解析" % did, out[:200])
            continue
        health = dev.get("health") or {}
        status = health.get("status")
        support = (report.get("smart_support") or {})
        passed = (report.get("smart_status") or {}).get("passed")
        nvme = (report.get("nvme_smart_health_information_log") or {})
        messages = (report.get("smartctl") or {}).get("messages") or []
        verdicts = [m for m in messages if (m.get("severity") or "") == "error"]
        if verdicts:
            # 例如 USB 桥不认识：没有结论可对照，这不是“盘没问题”。
            compared += 1
            readable.append(did)
            check("%s 没有结论时 DTO 不声称 healthy" % did, status != "healthy", status)
            print("SKIP  %s 的 smartctl 没给出健康结论：%s" % (did, verdicts[0].get("string", "")[:70]))
            continue
        if passed is True or (nvme and not nvme.get("critical_warning")):
            compared += 1
            readable.append(did)
            check("%s 的 smartctl 说通过，DTO 就是 healthy" % did, status == "healthy", status)
            check("%s 的 healthy 带检查时间" % did,
                  isinstance(health.get("checked_at"), str), health.get("checked_at"))
        elif passed is False:
            compared += 1
            readable.append(did)
            check("%s 的 smartctl 判定为失败时 DTO 不是 healthy" % did,
                  status in {"critical", "warning"}, status)
        elif support.get("available") is False:
            # 这块盘自己说没有 SMART：产品必须报 unsupported，不能报 healthy。
            compared += 1
            readable.append(did)
            check("%s 的 smart_support.available=false 时 DTO 是 unsupported" % did,
                  status == "unsupported", status)
            check("%s 的 unsupported 有检查时间" % did,
                  isinstance(health.get("checked_at"), str), health.get("checked_at"))
        else:
            print("SKIP  %s 的 smartctl 输出里没有可判定的健康字段" % did)
    if compared == 0:
        print("SKIP  与 smartctl 逐盘对照：本机没有一块盘能被 smartctl 读出可判定的结论")
    else:
        print("INFO  与 smartctl 逐盘对照了 %d 块盘：%s" % (compared, " ".join(readable)))

# E5 计数与 /proc/diskstats 对得上（只比完成次数，字节数由内核按 512 扇区折算）。
try:
    if not os.path.exists("/proc/diskstats"):
        raise FileNotFoundError("/proc/diskstats 不存在（不是 Linux 宿主）")
    diskstats = {}
    with open("/proc/diskstats", "r", encoding="utf-8") as fh:
        for line in fh:
            parts = line.split()
            if len(parts) >= 14:
                diskstats[parts[2]] = (int(parts[3]), int(parts[7]))  # 读完成, 写完成
    for dev in listed:
        stats = diskstats.get(dev.get("id"))
        io = dev.get("io") or {}
        if not stats or not is_byte(io.get("reads_total")):
            continue
        # 采样在几秒之前，计数器只会变大：DTO 不能比现在的 /proc 更大。
        check("%s io.reads_total 不超过 /proc/diskstats" % dev["id"],
              int(io["reads_total"]) <= stats[0],
              "DTO %s / proc %s" % (io["reads_total"], stats[0]))
except FileNotFoundError:
    print("SKIP  与 /proc/diskstats 对照：本机没有这个文件（不是 Linux 宿主）")
except OSError as error:
    bad("读取 /proc/diskstats", error)

print()
print("结果: PASS=%d FAIL=%d" % (PASSED, FAILED))
if FAILED:
    print("失败项: %s" % ", ".join(FAILED_NAMES))
sys.exit(0 if FAILED == 0 else 1)
