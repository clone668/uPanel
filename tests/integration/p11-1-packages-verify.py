#!/usr/bin/env python3
"""P11.1 的独立复核：拿面板给的判断，与这台机器上的 dpkg / apt 自己算一遍对账。

为什么要另写一份而不是在 Go 里再算一遍：版本比较是这一段里最容易"自己跟自己
对得上"的地方。internal/debversion 是对 dpkg 算法的移植，移植得对不对，只有
拿 dpkg 自己的比较器问一次才算数。所以这里：

  * 期望的版本关系用 `dpkg --compare-versions` 得到（dpkg 自己的实现）；
  * 期望的候选版本用 `apt-cache policy` 的 `Candidate:` 行得到；
  * 期望的安全更新标记用"候选版本那一块里有没有 -security 源"得到；
  * 已安装/未安装用 `dpkg-query` 的状态位得到。

然后把面板 API 返回的每一个字段逐条对账，对不上就打印并退出非零。

用法：
    python3 p11-1-packages-verify.py <merged-packages.json>

输入 JSON 形状（由 p11-1-apt-acceptance.sh 把翻页结果合并而成）：
    {"entries": [<PackageEntry>...], "counts": {...}, "filters": {...}}

filters 决定"应该出现哪些包"：installed=true（默认）时期望集合恰好是 dpkg 里
状态为 ii 的那些；installed=false 时期望集合是 dpkg 登记的全部。

counts 是**全量统计**，不受 filters 影响（CONTRACTS.md：counts 是这台机器的
总数，filters 只管列表里显示哪几行），所以它单独在全部 dpkg 条目上算一遍。

退出码：0 全部对得上；1 有对不上的；2 环境不满足（没有 dpkg/apt-cache）。
"""

import json
import subprocess
import sys


def run(argv):
    return subprocess.run(argv, capture_output=True, text=True, check=False)


def installed_facts():
    """返回 {name: (status, version, arch)}，直接从 dpkg 数据库读。"""
    res = run([
        "dpkg-query", "-W", "-f",
        "${binary:Package}\t${Version}\t${Architecture}\t${db:Status-Abbrev}\n",
    ])
    if res.returncode != 0:
        print("FATAL: dpkg-query 失败: %s" % res.stderr.strip(), file=sys.stderr)
        sys.exit(2)
    out = {}
    for line in res.stdout.splitlines():
        if not line:
            continue
        name, version, arch, status = line.split("\t")
        out[name] = (status, version, arch)
    return out


def policy_facts(names):
    """返回 {name: (candidate, suites_of_candidate_block)}。

    与 Go 侧同一个坑：`***` 标的是**已安装**版本，不是候选。候选的来源必须从
    版本号等于 `Candidate:` 的那一块里取。这里独立实现一次，好让两边互相咬合。
    """
    out = {}
    chunk = 2000
    names = sorted(names)
    for start in range(0, len(names), chunk):
        res = run(["apt-cache", "policy"] + names[start:start + chunk])
        if res.returncode != 0:
            print("FATAL: apt-cache policy 失败: %s" % res.stderr.strip(), file=sys.stderr)
            sys.exit(2)
        cur = None
        candidate = None
        blocks = {}
        block_version = None

        def flush_block():
            nonlocal block_version
            block_version = None

        def flush_package():
            nonlocal cur, candidate, blocks, block_version
            flush_block()
            if cur is not None:
                suites = blocks.get(candidate, []) if candidate else []
                out[cur] = (candidate, suites)
            cur, candidate, blocks = None, None, {}

        for line in res.stdout.splitlines():
            if not line.strip():
                continue
            if not line.startswith((" ", "\t")):
                flush_package()
                cur = line[:-1] if line.endswith(":") else line
                blocks = {}
                continue
            if cur is None:
                continue
            text = line.strip()
            if text.startswith("Candidate:"):
                value = text[len("Candidate:"):].strip()
                candidate = None if value == "(none)" else value
                continue
            if text.startswith(("Installed:", "Version table:", "WARNING")):
                continue
            fields = text.split()
            if fields and fields[0] == "***":
                fields = fields[1:]
            is_version_line = (
                len(fields) >= 2
                and fields[1].isdigit()
                and (len(fields) == 2 or fields[2].startswith("(phased"))
            )
            if is_version_line:
                flush_block()
                block_version = fields[0]
                blocks.setdefault(block_version, [])
                continue
            if block_version is None:
                continue
            # 来源行：`500 http://host/ubuntu jammy-updates/main amd64 Packages`
            body = fields
            while body and body[-1] == "Packages":
                body = body[:-1]
            if body and body[-1] in ("amd64", "i386", "arm64", "armhf", "all"):
                body = body[:-1]
            if body and body[0].isdigit():
                body = body[1:]
            if len(body) >= 2 and "/var/lib/dpkg/status" not in body[0]:
                suite = body[1].split("/")[0]
                if suite and not suite.startswith(("./", "/")):
                    if suite not in blocks[block_version]:
                        blocks[block_version].append(suite)
        flush_package()

    # `apt-cache policy bind9-libs:amd64` 打出来的表头是 `bind9-libs:`——apt 在表头里
    # 剥掉了 `:arch`，但 `Candidate:` / 版本表给的是请求那个 arch 的内容（真机实测）。
    # 所以按 base 名回退。同名双架构都装着时表头分不清是哪一边的版本，那就不回退：
    # 期望值留空，等于"这条我不判"，而不是拿另一架构的版本冒充。
    wanted = {}
    for name in names:
        wanted.setdefault(name.split(":", 1)[0], []).append(name)
    for base, group in wanted.items():
        if base in out and len(group) == 1:
            out[group[0]] = out[base]
    return out


def compare(a, b):
    """用 dpkg 自己的比较器判版本关系：返回 'newer' | 'same' | 'older'。"""
    if run(["dpkg", "--compare-versions", a, "lt", b]).returncode == 0:
        return "older"
    if run(["dpkg", "--compare-versions", a, "gt", b]).returncode == 0:
        return "newer"
    return "same"


def main():
    if len(sys.argv) != 2:
        print(__doc__, file=sys.stderr)
        sys.exit(2)
    with open(sys.argv[1], encoding="utf-8") as handle:
        api = json.load(handle)

    entries = api.get("entries") or []
    filters = api.get("filters") or {}
    scope_installed = filters.get("installed", True)

    dpkg = installed_facts()
    policy = policy_facts(dpkg.keys())

    if scope_installed:
        expected = {n: v for n, v in dpkg.items() if v[0].strip().startswith("ii")}
    else:
        expected = dict(dpkg)

    checked = 0
    mismatches = 0

    def bad(pkg, field, expect, actual):
        nonlocal mismatches
        mismatches += 1
        print("MISMATCH %s.%s 期望 %r 实际 %r" % (pkg, field, expect, actual))

    for entry in entries:
        name = entry["name"]
        if name not in dpkg:
            bad(name, "exists", True, False)
            continue
        status, version, arch = dpkg[name]
        checked += 1

        expect_installed = status.strip().startswith("ii")
        if entry["installed"] != expect_installed:
            bad(name, "installed", expect_installed, entry["installed"])
        if entry["status"] != status:
            bad(name, "status", status, entry["status"])
        if entry["installed_version"] != version:
            bad(name, "installed_version", version, entry["installed_version"])
        if entry["architecture"] != arch:
            bad(name, "architecture", arch, entry["architecture"])

        candidate, suites = policy.get(name, (None, []))
        if (entry["candidate_version"] or None) != candidate:
            bad(name, "candidate_version", candidate, entry["candidate_version"])
        if sorted(entry["suites"]) != sorted(suites):
            bad(name, "suites", sorted(suites), sorted(entry["suites"]))

        if candidate is None:
            expect_relation = "unknown"
            expect_upgradable = False
            expect_security = False
        else:
            expect_relation = compare(candidate, version)
            expect_upgradable = expect_relation == "newer"
            expect_security = expect_upgradable and any(
                s.endswith("-security") for s in suites
            )
        if entry["candidate_relation"] != expect_relation:
            bad(name, "candidate_relation", expect_relation, entry["candidate_relation"])
        if entry["upgradable"] != expect_upgradable:
            bad(name, "upgradable", expect_upgradable, entry["upgradable"])
        if entry["security"] != expect_security:
            bad(name, "security", expect_security, entry["security"])

    # 双向查漏：该出现的都出现了，不该出现的没混进来。
    got = {e["name"] for e in entries}
    missing = sorted(set(expected) - got)
    extra = sorted(got - set(expected))
    for name in missing[:10]:
        bad(name, "present", True, False)
    for name in extra[:10]:
        bad(name, "present", False, True)
    if missing:
        print("MISSING %d 个包没有出现在面板结果里" % len(missing))
    if extra:
        print("EXTRA %d 个包本不该出现在这一份列表里" % len(extra))

    # counts 是全量统计：在全部 dpkg 条目上独立算一遍。
    exp_upgradable = 0
    exp_security = 0
    for name, (status, version, arch) in dpkg.items():
        candidate, suites = policy.get(name, (None, []))
        if candidate is None:
            continue
        if compare(candidate, version) != "newer":
            continue
        exp_upgradable += 1
        if any(s.endswith("-security") for s in suites):
            exp_security += 1
    counts = api.get("counts") or {}
    if counts.get("upgradable") != exp_upgradable:
        bad("counts", "upgradable", exp_upgradable, counts.get("upgradable"))
    if counts.get("security") != exp_security:
        bad("counts", "security", exp_security, counts.get("security"))
    if counts.get("installed") != sum(
        1 for v in dpkg.values() if v[0].strip().startswith("ii")
    ):
        bad("counts", "installed",
            sum(1 for v in dpkg.values() if v[0].strip().startswith("ii")),
            counts.get("installed"))

    print("SCOPE=%s ENTRIES=%d EXPECTED=%d CHECKED=%d MISMATCH=%d"
          % ("installed" if scope_installed else "all", len(entries), len(expected),
             checked, mismatches))
    return 0 if mismatches == 0 else 1


if __name__ == "__main__":
    sys.exit(main())
