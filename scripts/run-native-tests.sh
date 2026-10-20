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

total_pass=0
total_fail=0

for name in agentstore agent agentclient operations syscmd store api; do
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
