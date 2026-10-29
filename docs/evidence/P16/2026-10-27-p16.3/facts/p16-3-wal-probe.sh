#!/bin/sh
# P16.3：空闲写放大探针（guest 侧，以 root 跑）。
#
# 要回答三件在 P16.2 里只剩"未查明"的事：
#   1. 空闲时到底是谁在写 upanel.db（哪个进程、哪张表）；
#   2. WAL 会不会一直长（是"泄漏"还是"到检查点就回落"）；
#   3. PRAGMA wal_checkpoint 之后 WAL 是否真的回落。
#
# 只读 db 用 mode=ro 连接，避免探针自己制造写入；最后那次 checkpoint 是唯一一次写。
set -u

WORK=/var/tmp/upanel-wal-probe
DB=/var/lib/upanel/upanel.db
ADB=/var/lib/upanel-agent/agent.db
REPORT=$WORK/report.txt
SIZES=$WORK/sizes.txt

mkdir -p "$WORK"
: >"$REPORT"
: >"$SIZES"

say() { printf '%s\n' "$*" | tee -a "$REPORT"; }

say "== 环境"
{
  . /etc/os-release
  echo "os=$PRETTY_NAME"
  echo "kernel=$(uname -r)"
  echo "started_at=$(date -u +%FT%TZ)"
  echo "web_binary_mtime=$(stat -c %y /usr/local/bin/upanel-web 2>/dev/null || echo 缺)"
} >>"$REPORT"

say "== 单元状态"
for u in upanel-web upanel-agent; do
  printf '%s active=%s since=%s\n' "$u" \
    "$(systemctl is-active $u 2>/dev/null)" \
    "$(systemctl show -p ActiveEnterTimestamp --value $u 2>/dev/null)" >>"$REPORT"
done

# 谁的进程持有这个库：不是靠猜，靠 lsof 看打开的文件。
say "== 打开 upanel.db 的进程"
{
  for p in /proc/[0-9]*; do
    pid=${p#/proc/}
    if ls -l "$p/fd" 2>/dev/null | grep -q "upanel\.db"; then
      printf 'pid=%s comm=%s cmd=%s\n' "$pid" "$(cat $p/comm 2>/dev/null)" \
        "$(tr '\0' ' ' <$p/cmdline 2>/dev/null | cut -c1-80)"
    fi
  done
} >>"$REPORT"

say "== 找 sqlite3 解释器"
if command -v python3 >/dev/null 2>&1; then
  PY=python3
  say "用 python3（$(python3 -V 2>&1)）的 sqlite3 模块"
elif command -v sqlite3 >/dev/null 2>&1; then
  PY=sqlite3
  say "用 sqlite3 命令行"
else
  say "既没有 python3 也没有 sqlite3：后面的表级读数做不了"
  PY=""
fi

# 表计数：每张表的行数。用只读连接，探针自己不改库。
counts() {
  [ -n "$PY" ] || return 0
  if [ "$PY" = "python3" ]; then
    python3 - "$DB" <<'PY'
import sqlite3, sys
path = sys.argv[1]
con = sqlite3.connect("file:%s?mode=ro" % path, uri=True)
tabs = [r[0] for r in con.execute(
    "select name from sqlite_master where type='table' order by name")]
for t in tabs:
    n = con.execute('select count(*) from "%s"' % t).fetchone()[0]
    print("%s\t%d" % (t, n))
con.close()
PY
  else
    for t in $(sqlite3 "file:$DB?mode=ro" "select name from sqlite_master where type='table' order by name"); do
      printf '%s\t%s\n' "$t" "$(sqlite3 "file:$DB?mode=ro" "select count(*) from \"$t\"")"
    done
  fi
}

# 一行的尺寸快照：主库 / WAL / shm / 代理库，外加当时的表总行数。
sample() {
  _total=""
  if [ -n "$PY" ]; then
    _total=$(counts | awk -F'\t' '{s+=$2} END {print s+0}')
  fi
  printf '%s\tdb=%s\twal=%s\tshm=%s\tadb_wal=%s\trows_total=%s\tnote=%s\n' \
    "$(date -u +%FT%TZ)" \
    "$(stat -c %s "$DB" 2>/dev/null || echo -1)" \
    "$(stat -c %s "$DB-wal" 2>/dev/null || echo -1)" \
    "$(stat -c %s "$DB-shm" 2>/dev/null || echo -1)" \
    "$(stat -c %s "$ADB-wal" 2>/dev/null || echo -1)" \
    "${_total:-n/a}" "$1" >>"$SIZES"
}

say "== 表清单（空闲开始前）"
counts >"$WORK/tables-before.txt"
cat "$WORK/tables-before.txt" >>"$REPORT"

sample before-idle

# 五分半钟，每 30 秒一次：够看清"每 5 秒一个采样点"的锯齿，也够触发一次自动检查点。
say "== 空闲采样开始 $(date -u +%FT%TZ)"
i=1
while [ "$i" -le 11 ]; do
  sleep 30
  sample "t=$((i * 30))s"
  i=$((i + 1))
done
say "== 空闲采样结束 $(date -u +%FT%TZ)"

counts >"$WORK/tables-after.txt"
say "== 表行数变化（空闲 5 分半）"
awk -F'\t' '
  NR == FNR { before[$1] = $2; next }
  { d = $2 - (before[$1] + 0); if (d != 0) printf "%s\t+%d\t(%d → %d)\n", $1, d, before[$1] + 0, $2 }
' "$WORK/tables-before.txt" "$WORK/tables-after.txt" | tee -a "$REPORT"

say "== 采样明细（date db wal shm adb_wal rows_total note）"
cat "$SIZES" >>"$REPORT"

# 唯一一次写：主动检查点。TRUNCATE 会把 WAL 截到 0，证明"文件里那些帧是可以被回收的"，
# 也就把"泄漏"与"还没到检查点"两件事分开。
say "== PRAGMA wal_checkpoint(TRUNCATE)"
if [ -n "$PY" ] && [ "$PY" = "python3" ]; then
  python3 - "$DB" <<'PY' | tee -a "$REPORT"
import sqlite3, sys
con = sqlite3.connect(sys.argv[1])
row = con.execute("PRAGMA wal_checkpoint(TRUNCATE)").fetchone()
print("busy=%s log=%s checkpointed=%s" % row)
print("journal_mode=%s" % con.execute("PRAGMA journal_mode").fetchone()[0])
print("autocheckpoint=%s" % con.execute("PRAGMA wal_autocheckpoint").fetchone()[0])
con.close()
PY
elif [ -n "$PY" ]; then
  sqlite3 "$DB" "PRAGMA wal_checkpoint(TRUNCATE); PRAGMA journal_mode; PRAGMA wal_autocheckpoint;" | tee -a "$REPORT"
fi
sample after-checkpoint

say "== 完 $(date -u +%FT%TZ)"
