#!/bin/sh
# 临时排查脚本（不入库）：复现 §8 “整盘被打开时建分区预检返回错误信封”。
set -u
BIN_DIR=/tmp/upanel-bin
AGENT=$BIN_DIR/upanel-agent
WEB=$BIN_DIR/upanel-web
SYS_CFG=/etc/upanel/upanel.json
SOCK=/run/upanel/agent.sock
PORT=19471
BASE=http://127.0.0.1:$PORT
DIR=/var/tmp/upanel-repro
JAR=$DIR/admin.jar
BLANK=sdd

rm -rf "$DIR"; mkdir -p "$DIR"
pkill -f upanel-agent 2>/dev/null
pkill -f 'upanel-web --mode production' 2>/dev/null
sleep 0.5

"$AGENT" --config "$SYS_CFG" >"$DIR/agent.log" 2>&1 &
for _ in $(seq 1 100); do [ -S "$SOCK" ] && break; sleep 0.1; done
runuser -u upanel-web -- "$WEB" --mode production --db /var/tmp/upanel-accept-p07-1/webdata/upanel.db \
  --listen "127.0.0.1:$PORT" >"$DIR/web.log" 2>&1 &
for _ in $(seq 1 100); do curl -fsS "$BASE/api/v1/health/live" >/dev/null 2>&1 && break; sleep 0.2; done

ORIGIN=$BASE
CODE=$(curl -s -c "$JAR" -o "$DIR/login.json" -w '%{http_code}' -X POST "$BASE/api/v1/auth/login" \
  -H 'Content-Type: application/json' -H 'X-UPanel-Request: 1' -H "Origin: $ORIGIN" \
  -d '{"username":"admin","password":"vm-correct-horse-9"}')
echo "login=$CODE"
CSRF=$(python3 -c 'import json;print(json.load(open("'"$DIR"'/login.json"))["data"]["csrf_token"])')

preflight() {
  curl -s -b "$JAR" -o "$DIR/pre.json" -w '%{http_code}' -X POST "$BASE/api/v1/operations/preflight" \
    -H 'Content-Type: application/json' -H "Origin: $ORIGIN" -H "X-CSRF-Token: $CSRF" -d "$1"
}

sgdisk --clear --new=1:0:+1024M --new=2:0:+512M "/dev/$BLANK" >/dev/null 2>&1
udevadm settle --timeout=5 >/dev/null 2>&1
for _ in $(seq 1 40); do
  curl -s -b "$JAR" -o "$DIR/dev.json" "$BASE/api/v1/storage/devices/$BLANK" -H "Origin: $ORIGIN"
  T=$(python3 -c 'import json;d=json.load(open("'"$DIR"'/dev.json"));print(d["data"]["device"]["partition_table"]["type"])')
  [ "$T" = "gpt" ] && break
  sleep 0.5
done
echo "table=$T"
START=$(python3 -c 'import json;d=json.load(open("'"$DIR"'/dev.json"));print(max(d["data"]["device"]["partition_table"]["free_regions"],key=lambda r:int(r["size_bytes"]))["start_bytes"])')
echo "start=$START"

echo "--- 1) 无进程占用"
echo "code=$(preflight "{\"action\":\"storage.partition.create\",\"params\":{\"device_id\":\"$BLANK\",\"start_bytes\":\"$START\",\"size_bytes\":\"1048576\"}}")"
cat "$DIR/pre.json"; echo

python3 - "/dev/$BLANK" <<'PY' &
import sys, time
f = open(sys.argv[1], "rb", buffering=0)
time.sleep(120)
PY
BUSY=$!
for _ in $(seq 1 40); do
  [ -n "$(ls -l "/proc/$BUSY/fd" 2>/dev/null | grep -F "/dev/$BLANK")" ] && break
  sleep 0.25
done
ls -l "/proc/$BUSY/fd" | grep -F "/dev/$BLANK"
echo "--- 2) 整盘被进程 $BUSY 打开"
echo "code=$(preflight "{\"action\":\"storage.partition.create\",\"params\":{\"device_id\":\"$BLANK\",\"start_bytes\":\"$START\",\"size_bytes\":\"1048576\"}}")"
cat "$DIR/pre.json"; echo
echo "--- 3) 同一时刻的 format 预检（对照）"
echo "code=$(preflight "{\"action\":\"storage.filesystem.format\",\"params\":{\"device_id\":\"${BLANK}1\",\"filesystem\":\"ext4\"}}")"
cat "$DIR/pre.json"; echo
kill "$BUSY" 2>/dev/null
wipefs -a "/dev/$BLANK" >/dev/null 2>&1
sgdisk --zap-all "/dev/$BLANK" >/dev/null 2>&1
dd if=/dev/zero of="/dev/$BLANK" bs=1M count=4 conv=fsync >/dev/null 2>&1
echo "--- agent 日志尾部"
tail -5 "$DIR/agent.log"
