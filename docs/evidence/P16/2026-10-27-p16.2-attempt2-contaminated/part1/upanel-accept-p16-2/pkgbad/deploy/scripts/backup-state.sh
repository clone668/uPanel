#!/bin/sh
# uPanel 状态备份（P16.1）：把“这台面板的全部身份”拷成一份可以带走的目录。
#
# 备份什么，为什么是这几样：
#
#   /etc/upanel/            配置 + 面板证书与私钥 + agent.key（秘密密钥）
#                           agent.key 丢了，已保存的秘密（共享口令、证书私钥引用）
#                           永久解不开——所以它跟数据库一起备份，不能分开。
#   面板数据库 upanel.db    账号、会话、审计、任务、计划、告警规则
#   代理数据库 agent.db     秘密密文、共享定义、部署记录、证书台账
#
# 拷数据库时必须先停服务：SQLite 的 WAL 与 shm 是**活的**，服务在跑的时候
# 主库文件本身可能还是旧的（最新的提交还在 -wal 里）。所以：
#
#   1. 停 upanel-web 与 upanel-agent（记下原本哪个在跑）
#   2. 主库 + -wal + -shm 三个文件一起拷
#   3. 起回原本在跑的服务（中途任何失败也要起回来，trap 保证）
#
# 只拷主库、或者不停服务直接拷，都能得到一份“看起来有、恢复后少几条”的备份——
# 那比没有备份更糟，因为没人会去重新准备一份。
#
# 用法（以 root 执行）：
#   sudo sh deploy/scripts/backup-state.sh                       # 默认目录 /var/backups/upanel/<UTC 时间>
#   sudo sh deploy/scripts/backup-state.sh --to /root/upanel-bak
#   sudo sh deploy/scripts/backup-state.sh --no-stop             # 只读快照式拷贝（不推荐，见上）
#
# 参数：
#   --to DIR         目标目录（必须不存在或为空；默认 /var/backups/upanel/<UTC 时间>）
#   --config PATH    配置文件，默认 /etc/upanel/upanel.json
#   --upanelctl PATH upanelctl 的位置，默认 /usr/local/bin/upanelctl
#   --no-stop        不停服务（INVARIANT 会破：拷到的库可能不含最新的 WAL 提交）
#
# 退出码：0 备份完成；2 参数或环境不满足、某一步失败（已停的服务会被起回来）。

set -u

SELF=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)/$(basename -- "$0")
STAMP=$(date -u +%Y%m%dT%H%M%SZ)

TO="/var/backups/upanel/$STAMP"
CONFIG="/etc/upanel/upanel.json"
CTL="/usr/local/bin/upanelctl"
DO_STOP=1

ETC_DIR="/etc/upanel"

TOTAL_STEPS=7
STEP=0

while [ $# -gt 0 ]; do
  case "$1" in
    --to) TO=${2:-}; shift 2 ;;
    --config) CONFIG=${2:-}; shift 2 ;;
    --upanelctl) CTL=${2:-}; shift 2 ;;
    --no-stop) DO_STOP=0; shift ;;
    -h|--help) awk 'NR>1 && /^#/ {sub(/^# ?/,""); print; next} NR>1 {exit}' "$SELF"; exit 0 ;;
    *) echo "错误: 未知参数 $1（--help 看用法）" >&2; exit 2 ;;
  esac
done

log()  { printf '%s\n' "$*"; }
step() { STEP=$((STEP + 1)); printf '\n[%d/%d] %s\n' "$STEP" "$TOTAL_STEPS" "$*"; }
ok()   { printf '      OK   %s\n' "$*"; }
warn() { printf '      注意 %s\n' "$*"; }

STOPPED=""
FAILED=""

die() {
  printf '\n==== 备份没有完成（%s）====\n' "$1" >&2
  restore_services
  if [ -n "$FAILED" ]; then
    printf '已经写进 %s 的东西不完整（目录里留着 .incomplete），**不要**拿它当备份：\n' "$TO" >&2
    printf '%s' "$FAILED" | while IFS= read -r line; do
      [ -n "$line" ] && printf '  - %s\n' "$line" >&2
    done
    printf '修好之后换一个新目录重跑（默认路径带时间戳，直接重跑即可）。\n' >&2
  fi
  exit 2
}

# 起回原本在跑的服务。用它而不是退出前手写一遍：中途任何一处 die 都要起回来，
# 否则一次失败的备份会把面板留在停机状态——为了备份而把服务弄停，比没有备份更糟。
restore_services() {
  [ -n "$STOPPED" ] || return 0
  printf '%s' "$STOPPED" | while IFS= read -r unit; do
    [ -n "$unit" ] || continue
    if [ "$unit" = "upanel-agent.service" ]; then
      systemctl start "$unit" >/dev/null 2>&1 || printf '      警告: 起回 %s 失败，请手工 systemctl start %s\n' "$unit" "$unit" >&2
    fi
  done
  printf '%s' "$STOPPED" | while IFS= read -r unit; do
    [ -n "$unit" ] || continue
    if [ "$unit" = "upanel-web.service" ]; then
      systemctl start "$unit" >/dev/null 2>&1 || printf '      警告: 起回 %s 失败，请手工 systemctl start %s\n' "$unit" "$unit" >&2
    fi
  done
  STOPPED=""
}
trap 'restore_services' EXIT INT TERM

# ---------------------------------------------------------------- 1. 前置
step "前置检查"
[ "$(id -u)" = "0" ] || die "必须以 root 执行（sudo sh $SELF ...）"
[ -r "$CONFIG" ] || die "读不到配置文件 $CONFIG（换路径用 --config）"
[ -x "$CTL" ] || die "找不到可执行的 $CTL（换路径用 --upanelctl；安装器把它装在 /usr/local/bin）"
if [ -e "$TO" ]; then
  if [ -d "$TO" ] && [ -z "$(ls -A "$TO" 2>/dev/null)" ]; then
    ok "目标目录已存在且为空：$TO"
  elif [ -d "$TO" ] && [ -f "$TO/.incomplete" ] && [ ! -f "$TO/MANIFEST.sha256" ]; then
    # 上一次跑到一半失败留下的目录。**不能直接复用**：里面可能是一份自相矛盾的混合体
    # （新的 etc-upanel + 旧的库），比没有更危险。但也不能替人删——那是别人的文件系统。
    die "目标 $TO 是上一次没跑完的备份（有 .incomplete，没有 MANIFEST.sha256）：那份内容不可信。
      删掉它再重跑：rm -rf $TO
      或者换一个 --to（默认路径带时间戳，本来就不会撞上）"
  else
    die "目标 $TO 已经存在且非空：备份不覆盖已有目录（换一个 --to，或用默认的时间戳目录）"
  fi
else
  mkdir -p "$TO" || die "无法创建目标目录 $TO"
  chmod 0700 "$TO"
  ok "已创建 $TO（0700）"
fi
done_target=$(CDPATH= cd -- "$TO" && pwd) || die "目标目录不可用：$TO"
# 走到最后一步才删它。中途失败时留着这个标记，让下一个人一眼看出“这份不完整”。
: > "$done_target/.incomplete"
chmod 0600 "$done_target/.incomplete"
ok "备份目录 $done_target"

# ---------------------------------------------------------------- 2. 生效路径
step "读生效路径（配置是唯一裁判）"
CFG_JSON=$("$CTL" check-config --config "$CONFIG" --json) || die "check-config --json 失败（配置本身有问题？先跑 check-config）"
PANEL_DB=$(printf '%s\n' "$CFG_JSON" | sed -n 's/.*"database_path": *"\([^"]*\)".*/\1/p' | head -n 1)
AGENT_DB=$(printf '%s\n' "$CFG_JSON" | sed -n 's/.*"agent_database_path": *"\([^"]*\)".*/\1/p' | head -n 1)
[ -n "$PANEL_DB" ] || die "配置里没有 database.path，无法确定面板数据库位置"
[ -n "$AGENT_DB" ] || die "配置里没有 agent.database_path，无法确定代理数据库位置"
ok "面板数据库 $PANEL_DB"
ok "代理数据库 $AGENT_DB"

# ---------------------------------------------------------------- 3. 停服务
step "停服务（WAL 与主库必须一起静止）"
if [ "$DO_STOP" = "1" ]; then
  for unit in upanel-agent.service upanel-web.service; do
    state=$(systemctl is-active "$unit" 2>/dev/null || true)
    if [ "$state" = "active" ]; then
      if systemctl stop "$unit" >/dev/null 2>&1; then
        STOPPED="${STOPPED}${unit}
"
        ok "已停 $unit（备份结束后会起回来）"
      else
        die "停 $unit 失败：库文件还在被写，这时候拷出来的是半份数据"
      fi
    else
      ok "$unit 本来就没在跑（$state）"
    fi
  done
else
  warn "按参数要求不停服务：拷到的数据库可能不含最新的 WAL 提交（--no-stop 只用于看一眼）"
fi

# ---------------------------------------------------------------- 4. 配置与密钥
step "拷配置、证书与代理密钥"
[ -d "$ETC_DIR" ] || die "$ETC_DIR 不存在（这台机器上装过 uPanel 吗）"
mkdir -p "$done_target/etc-upanel" || die "无法创建 $done_target/etc-upanel"
cp -a "$ETC_DIR/." "$done_target/etc-upanel/" || die "拷 $ETC_DIR 失败"
chmod -R go-rwx "$done_target/etc-upanel"   # 里面有 agent.key 与面板私钥
if [ -s "$ETC_DIR/agent.key" ]; then
  ok "已拷 $ETC_DIR（含 agent.key，已收紧为仅 root 可读）"
else
  warn "已拷 $ETC_DIR，但里面没有 agent.key（这台机器可能还没生成过秘密密钥）"
fi

# ---------------------------------------------------------------- 5. 两个库
step "拷面板库与代理库（主库 + -wal + -shm）"
copy_db() {
  # $1 数据库路径  $2 子目录名  $3 说明
  src=$1
  dest="$done_target/$2"
  if [ ! -f "$src" ]; then
    warn "$3 不存在：$src（本次没有可备份的内容，不是失败）"
    return 0
  fi
  mkdir -p "$dest" || { FAILED="${FAILED}无法创建 $dest
"; return 1; }
  n=0
  for f in "$src" "$src-wal" "$src-shm"; do
    if [ -f "$f" ]; then
      if cp -a "$f" "$dest/" 2>/dev/null; then
        n=$((n + 1))
      else
        FAILED="${FAILED}拷 $f 失败
"
        return 1
      fi
    fi
  done
  chmod -R go-rwx "$dest"
  ok "$3：$n 个文件 → $dest"
  return 0
}
copy_db "$PANEL_DB" panel-db "面板数据库" || die "面板数据库拷贝失败"
copy_db "$AGENT_DB" agent-db "代理数据库" || die "代理数据库拷贝失败"

# ---------------------------------------------------------------- 6. 起回服务
step "起回原本在跑的服务"
if [ -z "$STOPPED" ]; then
  ok "没有需要起回的服务"
else
  restore_services
  for unit in upanel-agent.service upanel-web.service; do
    if systemctl is-active "$unit" >/dev/null 2>&1; then
      ok "$unit 已恢复运行"
    fi
  done
fi

# ---------------------------------------------------------------- 7. 校验
step "校验备份"
# 清单先写：后面一旦发现校验不过，至少能指着清单说“这份别用”。
# `.incomplete` 不进清单——它在成功之后会被删掉，进清单就成了“清单里有个不存在的文件”。
( cd "$done_target" && find . -type f ! -name MANIFEST.sha256 ! -name .incomplete -exec sha256sum {} + ) \
  > "$done_target/MANIFEST.sha256" 2>/dev/null || die "生成 MANIFEST.sha256 失败"
chmod 0600 "$done_target/MANIFEST.sha256"
FILE_COUNT=$(wc -l < "$done_target/MANIFEST.sha256" | tr -d ' ')
ok "已写 MANIFEST.sha256（$FILE_COUNT 个文件）"

# 逐份校验数据库副本：sha256 一致只证明“拷得完整”，不证明“库本身是好的”。
# 后者要 sqlite3 才能答；机器上没有 sqlite3 就如实说没校验，不假装。
if command -v sqlite3 >/dev/null 2>&1; then
  for pair in "panel-db:面板数据库:$PANEL_DB" "agent-db:代理数据库:$AGENT_DB"; do
    sub=${pair%%:*}
    rest=${pair#*:}
    label=${rest%%:*}
    src=${rest#*:}
    copy="$done_target/$sub/$(basename -- "$src")"
    [ -f "$copy" ] || continue
    if [ "$(sqlite3 "$copy" 'PRAGMA integrity_check;' 2>/dev/null)" = "ok" ]; then
      ok "$label 副本 integrity_check = ok"
    else
      FAILED="${FAILED}$label 副本没通过 integrity_check
"
      die "$label 副本没通过 integrity_check：这份备份不能用"
    fi
  done
else
  warn "机器上没有 sqlite3：数据库副本的完整性未校验（只有 sha256 一致的证明）"
fi

# 这一份现在是完整的：去掉那句“别用”的标记。
rm -f "$done_target/.incomplete"

# ---------------------------------------------------------------- 完成
printf '\n==== 备份完成 ====\n'
cat <<EOF
备份目录:   $done_target
面板数据库: $PANEL_DB
代理数据库: $AGENT_DB
清单:       $done_target/MANIFEST.sha256（$FILE_COUNT 个文件）

这份备份里有两个东西不能分开用：
  1) etc-upanel/agent.key  丢了，库里保存的秘密（共享口令、证书私钥引用）永久解不开；
  2) 两个数据库           少了任何一个，恢复出来的面板与代理会对不上。

整份拷走再删原件：
  tar -C $(dirname -- "$done_target") -czf /root/upanel-backup-$(basename -- "$done_target").tar.gz $(basename -- "$done_target")

恢复（在服务停止的前提下）：
  systemctl stop upanel-web upanel-agent
  install -d -m 0755 -o root -g root /etc/upanel
  cp -a $done_target/etc-upanel/. /etc/upanel/
  systemctl start upanel-agent upanel-web      # 让代理先建 socket
  # 面板库要按原属主放回（root 建出来的文件 Web 进程写不进去）：
  #   install -o upanel-web -g upanel-web -m 0640 <副本> $PANEL_DB
  # 代理库：install -o root -g root -m 0600 <副本> $AGENT_DB
  # 详细步骤与判断依据见 docs/RECOVERY.md
EOF
