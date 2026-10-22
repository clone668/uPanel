#!/bin/sh
# 在 Ubuntu 验收虚拟机里跑原生测试二进制（必须在 VM 内以 root 执行：
# 代理数据库要求数据目录属主是 root）。
#
#   sudo BIN_DIR=/tmp/upanel-bin sh run-native-tests.sh
#
# 可选环境变量：UPANEL_TEST_TIMEOUT（默认 3600s，作为每个包的 -test.timeout）。
#
# 只做事：逐个子包跑 -test.v，统计 --- PASS / --- FAIL，最后打印总计。
# 退出码 = 失败项数（0 表示全绿），方便验收脚本判断。
set -u

BIN_DIR="${BIN_DIR:-/tmp/upanel-bin}"
TESTS="$BIN_DIR/tests"
# 每个包的 -test.timeout。验收 VM 是 TCG 软件模拟，api 包要起真 HTTP 服务、跑 cookie/
# 限速/多段上传，默认 10 分钟会先到点后跑完：超时 panic 一条 --- FAIL 都没有，看起来
# 像“什么都没发生”。用环境变量可覆盖（UPANEL_TEST_TIMEOUT=10m 复现默认行为）。
TIMEOUT="${UPANEL_TEST_TIMEOUT:-3600s}"

if [ ! -d "$TESTS" ]; then
    echo "找不到测试二进制目录: $TESTS" >&2
    exit 2
fi

# 测试二进制是从开发宿主传过来的：scp 不带 -p 时按对端 umask 落地成 0644，执行位
# 没了。不补这一步的话，下面每一条都会判成“缺少测试二进制”，看起来像根本没上传。
for f in "$TESTS"/*.test; do
    [ -f "$f" ] && chmod +x "$f" 2>/dev/null
done

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

# journal / logs 也在这里跑：它们的用例全是合成输入（fake runner / 假代理），
# 不需要 journald，但跑在目标平台上能顺带证明“同一份代码在 Linux 上行为一致”。
#
# files 必须在这里：它的用例（openat2 边界、暂存上传、备份清理、保留目录）全都
# 打 //go:build linux，在开发宿主上连编译都不参与（-c 交叉编译只证明它能编过）。
# 这一包的结论只能由本机给出——mock 通过不等于真实 Ubuntu 上通过。
#
# terminal 同理，而且理由更硬：它要真的 fork 一个 PTY、以 upanel-shell 降权、
# 收到 SIGHUP、读回 /proc/<pid>/status 核对 uid/gid。这些在宿主上根本没有对应
# 行为，交叉编译只能证明语法。会话生命周期、断流宽限、空闲/最长超时、帧协议
# 与速率限制的结论全部来自这一行。
for name in agentstore agent agentclient operations syscmd store api files terminal monitoring storage journal logs; do
    bin="$TESTS/$name.test"
    if [ ! -x "$bin" ]; then
        echo "缺少测试二进制: $bin" >&2
        total_fail=$((total_fail + 1))
        continue
    fi
    out=$("$bin" -test.v -test.timeout "$TIMEOUT" 2>&1)
    rc=$?
    pass=$(printf '%s\n' "$out" | grep -c -- '--- PASS')
    fail=$(printf '%s\n' "$out" | grep -c -- '--- FAIL')
    # rc≠0 但一条 --- FAIL 都没有 = 超时 panic / 提前崩溃 / 测试二进制自身报错。
    # 这种包一条 --- FAIL 都不会打印，只数 --- FAIL 会把它算成“0 失败”，于是整个
    # 脚本在最需要报警的时候报绿。所以 rc≠0 一律再记一次失败（不重复计数）。
    if [ "$fail" -eq 0 ] && [ "$rc" -ne 0 ]; then
        fail=1
        echo "!! $name 退出码 $rc 但没有 --- FAIL 行（超时/panic/二进制报错），按 1 次失败计"
        printf '%s\n' "$out" | tail -25
    fi
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
