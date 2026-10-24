#!/bin/sh
# P11.3「/system/power-state 返回 503」的分类复现（2026-10-23）。
#
# 要分开的是三件事，它们的处置完全不同：
#   A 代理在            → 200，正常数据
#   B 代理不在（socket 也没了）→ 谁的问题？连接层。多久？应当**立刻**，不是超时
#   C socket 在但没人回答   → 这是"web 侧超时"那一类：要等多久、最后报什么
#
# 用的是 P11.3 验收留下的现场（/etc/upanel/upanel.json、P11.3 的 web 库），
# 不新建配置，也不碰数据盘。跑完把假 socket 清掉，代理保持停止。
set -u

BIN=/var/tmp/upanel-bin
SOCK=/run/upanel/agent.sock
BASE=http://127.0.0.1:19506
WORK=/var/tmp/upanel-accept-p11-3
OUT=/var/tmp/p11-3-503-repro

mkdir -p "$OUT"
LOG=$OUT/repro.log
: >"$LOG"
log() { printf '%s\n' "$*" | tee -a "$LOG"; }

query() { # 打一次 power-state，回 "状态码 耗时秒"
  curl -s -b "$OUT/jar" -o "$OUT/power-state-$1.json" -w '%{http_code} %{time_total}' \
    "$BASE/api/v1/system/power-state"
}
body() { head -c 400 "$OUT/power-state-$1.json"; printf '\n'; }

log "== 起代理（配置 /etc/upanel/upanel.json）"
"$BIN/upanel-agent" --config /etc/upanel/upanel.json >"$OUT/agent.log" 2>&1 &
AGENT_PID=$!
for _ in $(seq 1 100); do [ -S "$SOCK" ] && break; sleep 0.1; done
log "   socket：$([ -S "$SOCK" ] && echo 在 || echo 不在)；pid $AGENT_PID"

log "== 起 web（P11.3 的库，127.0.0.1:19506）"
chown upanel-web "$WORK/webdata" 2>/dev/null
for s in "" -wal -shm; do [ -e "$WORK/webdata/upanel.db$s" ] && chown upanel-web "$WORK/webdata/upanel.db$s" 2>/dev/null; done
runuser -u upanel-web -- "$BIN/upanel-web" --mode production --db "$WORK/webdata/upanel.db" \
  --listen 127.0.0.1:19506 >"$OUT/web.log" 2>&1 &
WEB_PID=$!
for _ in $(seq 1 100); do curl -fsS "$BASE/api/v1/health/live" >/dev/null 2>&1 && break; sleep 0.2; done

code=$(curl -s -c "$OUT/jar" -o "$OUT/login.json" -w '%{http_code}' -X POST "$BASE/api/v1/auth/login" \
  -H 'Content-Type: application/json' -H 'X-UPanel-Request: 1' -H "Origin: $BASE" \
  -d '{"username":"admin","password":"vm-correct-horse-9"}')
log "== 登录 admin：$code"

log "== A 代理在"
log "   $(query 1)"
log "   $(body 1)"

log "== 停掉代理（模拟「进程活着但 socket 没了」的反面：进程真没了）"
kill "$AGENT_PID" 2>/dev/null
for _ in $(seq 1 50); do kill -0 "$AGENT_PID" 2>/dev/null || break; sleep 0.1; done
log "   socket：$([ -S "$SOCK" ] && echo 在 || echo 不在)"
log "== B 代理不在"
log "   $(query 2)"
log "   $(body 2)"

log "== 装上假代理：accept 之后一句都不回（socket 在，没人答）"
python3 -c '
import os, socket, time, sys
sock = sys.argv[1]
try:
    os.unlink(sock)
except FileNotFoundError:
    pass
s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
s.bind(sock)
s.listen(8)
while True:
    conn, _ = s.accept()
    time.sleep(600)   # 收下连接，一句都不回
' "$SOCK" >"$OUT/fake.log" 2>&1 &
FAKE=$!
sleep 0.5
log "   socket：$([ -S "$SOCK" ] && echo 在 || echo 不在)"
log "== C 假代理不回答"
log "   $(query 3)"
log "   $(body 3)"

kill "$FAKE" 2>/dev/null
sleep 0.2
rm -f "$SOCK"
kill "$WEB_PID" 2>/dev/null
log "== 收尾：假 socket 已删，web 已停"
log "web 日志里这次的三条："
grep 'power-state' "$OUT/web.log" | tail -3 | tee -a "$LOG"
