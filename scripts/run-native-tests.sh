#!/bin/sh
# 在 Ubuntu 验收虚拟机里跑原生测试二进制（必须在 VM 内以 root 执行：
# 代理数据库要求数据目录属主是 root）。
#
#   sudo BIN_DIR=/tmp/upanel-bin sh run-native-tests.sh
#
# 只做事：逐个子包跑 -test.v，统计 --- PASS / --- FAIL，最后打印总计。
# 退出码 = 失败项数（0 表示全绿），方便验收脚本判断。
set -u

BIN_DIR="${BIN_DIR:-/tmp/upanel-bin}"
TESTS="$BIN_DIR/tests"

if [ ! -d "$TESTS" ]; then
    echo "找不到测试二进制目录: $TESTS" >&2
    exit 2
fi

# 解析器测试要读 tests/fixtures 里的真实采集样本。测试二进制跑在 /tmp，进程 cwd 不是
# 源码树，"../../tests/fixtures" 在那里没有意义；所以先把夹具拷到二进制旁边，
# 再用 UPANEL_FIXTURES 指给测试（查找逻辑见 internal/testsupport/fixtures.go）。
FIXTURES="${UPANEL_FIXTURES:-$BIN_DIR/fixtures}"
# 目录存在但空着也要重拷：上一轮 mkdir 成功、cp 因为夹具还没上传而白跑过一次，
# 空目录会让后面二十几条夹具用例全部失败，却看不出是“拷贝没做”。
NEED_COPY=0
if [ ! -d "$FIXTURES" ]; then
    NEED_COPY=1
elif [ -z "$(ls -A "$FIXTURES" 2>/dev/null)" ]; then
    NEED_COPY=1
fi
if [ "$NEED_COPY" -eq 1 ]; then
    # 脚本在仓库里的位置是 scripts/，但验收时也可能被直接放在源码树根，
    # 两个位置都试一次，别让“上传结构不同”变成二十几条夹具失败。
    SCRIPT_DIR=$(cd "$(dirname "$0")" 2>/dev/null && pwd)
    SRC_FIXTURES=""
    for cand in "$SCRIPT_DIR/../tests/fixtures" "$SCRIPT_DIR/tests/fixtures"; do
        if [ -d "$cand" ]; then SRC_FIXTURES=$cand; break; fi
    done
    if [ -n "$SRC_FIXTURES" ]; then
        mkdir -p "$BIN_DIR/fixtures"
        cp "$SRC_FIXTURES"/* "$BIN_DIR/fixtures"/
        FIXTURES="$BIN_DIR/fixtures"
        echo "夹具: $SRC_FIXTURES -> $FIXTURES（$(ls -1 "$FIXTURES" | wc -l) 个文件）"
    else
        echo "警告: 找不到夹具目录（试过 $SCRIPT_DIR/../tests/fixtures、$SCRIPT_DIR/tests/fixtures），monitoring/agent 的夹具用例会失败" >&2
    fi
fi
export UPANEL_FIXTURES="$FIXTURES"

total_pass=0
total_fail=0

for name in agentstore agent agentclient operations syscmd store api monitoring; do
    bin="$TESTS/$name.test"
    if [ ! -x "$bin" ]; then
        echo "缺少测试二进制: $bin" >&2
        total_fail=$((total_fail + 1))
        continue
    fi
    out=$("$bin" -test.v 2>&1)
    rc=$?
    pass=$(printf '%s\n' "$out" | grep -c -- '--- PASS')
    fail=$(printf '%s\n' "$out" | grep -c -- '--- FAIL')
    total_pass=$((total_pass + pass))
    total_fail=$((total_fail + fail))
    echo "== $name rc=$rc PASS=$pass FAIL=$fail"
    if [ "$fail" -ne 0 ]; then
        printf '%s\n' "$out" | grep -A4 -- '--- FAIL' | head -40
    fi
done

echo "TOTAL PASS=$total_pass FAIL=$total_fail"
[ "$total_fail" -eq 0 ] || exit 1
exit 0
