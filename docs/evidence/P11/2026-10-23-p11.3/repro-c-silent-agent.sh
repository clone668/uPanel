#!/bin/sh
# 只测 C 组：socket 在、有人 accept、但一句都不回。
#
# 上一版这条是无效的：假 socket 由 root 建、权限 0755，upanel-web 连不上（EACCES），
# 于是它 5ms 就报 503——那不是"超时"的证据，是"连不上"的证据。这次 chmod 0777，
# 让 upanel-web 真的连进来，才能量到 web 侧的等待时间与最后的错误。
set -u

BIN=/var/tmp/upanel-bin
SOCK=/run/upanel/agent.sock
BASE=http://127.0.0.1:19506
WORK=/var/tmp/upanel-accept-p11-3
OUT=/var/tmp/p11-3-503-repro
JAR=$OUT/jar-c.txt

log() { printf '%s\n' "$*" | tee -a "$OUT/repro-c.log"; }
: >"$OUT/repro-c.log"

runuser -u upanel-web -- "$BIN/upanel-web" --mode production --db "$WORK/webdata/upanel.db" \
  --listen 127.0.0.1:19506 >"$OUT/web-c.log" 2>&1 &
WEB_PID=$!
for _ in $(seq 1 100); do curl -fsS "$BASE/api/v1/health/live" >/dev/null 2>&1 && break; sleep 0.2; done

code=$(curl -s -c "$JAR" -o "$OUT/login-c.json" -w '%{http_code}' -X POST "$BASE/api/v1/auth/login" \
  -H 'Content-Type: application/json' -H 'X-UPanel-Request: 1' -H "Origin: $BASE" \
  -d '{"username":"admin","password":"vm-correct-horse-9"}')
log "登录：$code"

python3 -c '
import os, socket, sys, time
p = sys.argv[1]
try:
    os.unlink(p)
except FileNotFoundError:
    pass
s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
s.bind(p)
s.listen(8)
os.chmod(p, 0o777)      # upanel-web 要能连进来，否则测的还是 EACCES
while True:
    conn, _ = s.accept()
    time.sleep(300)     # 收下连接，一句都不回
' "$SOCK" &
FAKE=$!
sleep 1
log "socket：$(ls -l "$SOCK" 2>/dev/null | cut -d' ' -f1,3,4)"

log "C 组（socket 在、不回答）开始：$(date -u +%H:%M:%S)"
out=$(curl -s -b "$JAR" -o "$OUT/power-state-c.json" -w '%{http_code} %{time_total}' \
  "$BASE/api/v1/system/power-state")
log "  结果：$out"
log "  正文：$(head -c 300 "$OUT/power-state-c.json")"
log "C 组结束：$(date -u +%H:%M:%S)"

kill "$FAKE" 2>/dev/null
sleep 0.2
rm -f "$SOCK"
kill "$WEB_PID" 2>/dev/null
log "收尾：假 socket 已删，web 已停"
log "web 日志最后一条：$(grep power-state "$OUT/web-c.log" | tail -1)"
