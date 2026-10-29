#!/bin/sh
# uPanel 卸载脚本（P10.4 起，1.0 发布版）。
#
# 设计原则：**先列出要删什么，再删**；默认保留配置与数据，删数据必须显式要求。
# 绝不 `rm -rf /srv/upanel` 这种一刀切——那里有用户自己的站点与数据。
#
# 用法（以 root 执行）：
#   sudo sh deploy/scripts/uninstall.sh                    # 停服务、删程序与单元，保留配置/数据
#   sudo sh deploy/scripts/uninstall.sh --purge-config     # 同时删 /etc/upanel（含代理密钥！）
#   sudo sh deploy/scripts/uninstall.sh --purge-data       # 同时删面板与代理数据库
#   sudo sh deploy/scripts/uninstall.sh --purge-data --yes-delete-srv
#                                                          # 额外清空 /srv/upanel（要求再次确认）
#
# 参数：
#   --backup-dir DIR   删之前先把两个数据库与 /etc/upanel 备份到 DIR（默认
#                      /var/backups/upanel/uninstall-<UTC 时间>）
#   --no-backup        不备份就删（要删配置或数据时必须显式写这一条）
#
# 为什么卸载也要备份：`--purge-data` 删掉的是账号、审计、任务记录、共享定义与
# 秘密密文；`--purge-config` 删掉 agent.key，那一刻起已保存的秘密永久解不开。
# 这两样合起来就是“这台面板的全部身份”，删掉之后**没有任何自动恢复路径**。
# 所以只要带了 --purge-*，脚本默认先备份；不想要备份就必须把 --no-backup 写出来。
#
# --purge-config 会删掉 /etc/upanel/agent.key：那一刻起，已保存的秘密
# （共享口令、证书私钥引用）永久不可恢复。脚本会单独再问一次。
#
# 系统账号默认保留（可能被别的东西引用）；--purge-accounts 才删。

set -u

SELF=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)/$(basename -- "$0")
SELF_DIR=$(dirname -- "$SELF")

PURGE_CONFIG=0
PURGE_DATA=0
PURGE_ACCOUNTS=0
DELETE_SRV=0
BACKUP_TO=""
DO_BACKUP=1

WEB_USER="upanel-web"
SHELL_USER="upanel-shell"
ETC_DIR="/etc/upanel"
LIB_DIR="/usr/local/lib/upanel"
WEB_DATA="/var/lib/upanel"
AGENT_DATA="/var/lib/upanel-agent"
SHELL_HOME="/var/lib/upanel-shell"
SRV_ROOT="/srv/upanel"
MANUAL_CERT_DIR="/etc/ssl/upanel"
ACME_WEBROOT="/var/www/upanel-acme"
DOC_DIR="/usr/local/share/doc/upanel"

while [ $# -gt 0 ]; do
  case "$1" in
    --purge-config) PURGE_CONFIG=1; shift ;;
    --purge-data) PURGE_DATA=1; shift ;;
    --purge-accounts) PURGE_ACCOUNTS=1; shift ;;
    --backup-dir) BACKUP_TO=${2:-}; shift 2 ;;
    --no-backup) DO_BACKUP=0; shift ;;
    --yes-delete-srv) DELETE_SRV=1; shift ;;
    -h|--help) awk 'NR>1 && /^#/ {sub(/^# ?/,""); print; next} NR>1 {exit}' "$SELF"; exit 0 ;;
    *) echo "错误: 未知参数 $1（--help 看用法）" >&2; exit 2 ;;
  esac
done

[ "$(id -u)" = "0" ] || { echo "错误: 必须以 root 执行（sudo sh $SELF ...）" >&2; exit 2; }

TARGETS=""
add_target() { TARGETS="${TARGETS}$1
"; }
# 只登记“确实存在”的路径，删之前把清单打出来，删完逐条报告。
consider() { if [ -e "$1" ] || [ -L "$1" ]; then add_target "$1"; fi; }

consider /etc/systemd/system/upanel-web.service
consider /etc/systemd/system/upanel-agent.service
consider "$LIB_DIR"
consider /usr/local/bin/upanel-web
consider /usr/local/bin/upanelctl
consider "$DOC_DIR"
[ "$PURGE_CONFIG" = "1" ] && consider "$ETC_DIR"
[ "$PURGE_DATA" = "1" ] && consider "$WEB_DATA"
[ "$PURGE_DATA" = "1" ] && consider "$AGENT_DATA"
# 面板自己的自签名证书在 /etc/upanel/tls 下，随 --purge-config 一起走；
# /etc/ssl/upanel 是手动导入证书与签发证书目录，只在 --purge-data 时才动。
[ "$PURGE_DATA" = "1" ] && consider "$MANUAL_CERT_DIR"

echo '==== 将要执行 ===='
echo '1) 停止并禁用 upanel-web.service、upanel-agent.service'
if [ "$PURGE_CONFIG" = "1" ] || [ "$PURGE_DATA" = "1" ]; then
  if [ "$DO_BACKUP" = "1" ]; then
    echo "0) 先把两个数据库与 /etc/upanel 备份到 ${BACKUP_TO:-/var/backups/upanel/uninstall-<UTC 时间>}"
  else
    echo '0) 不备份（--no-backup）：删掉的东西没有副本'
  fi
fi
echo '2) 删除下面这些路径：'
if [ -n "$TARGETS" ]; then
  printf '%s' "$TARGETS" | while IFS= read -r t; do
    [ -n "$t" ] && printf '     %s\n' "$t"
  done
else
  echo '     （没有需要删除的路径）'
fi
echo '3) systemctl daemon-reload'

echo
echo '==== 默认保留（需要显式参数才删）===='
if [ "$PURGE_CONFIG" = "0" ]; then
  echo "     $ETC_DIR（配置、面板证书、代理密钥）"
else
  echo "     警告: --purge-config 会删 $ETC_DIR/agent.key —— 已保存的秘密永久不可恢复"
fi
if [ "$PURGE_DATA" = "0" ]; then
  echo "     $WEB_DATA（面板数据库：账号、审计、任务记录）"
  echo "     $AGENT_DATA（代理数据库）"
  echo "     $MANUAL_CERT_DIR（签发与导入的证书）"
fi
if [ "$DELETE_SRV" = "1" ]; then
  echo "     警告: --yes-delete-srv 会清空 $SRV_ROOT 里的站点与应用数据"
else
  echo "     $SRV_ROOT（站点与应用数据 —— 永远不会自动删除）"
fi
if [ "$PURGE_ACCOUNTS" = "1" ]; then
  echo "     警告: --purge-accounts 会删除系统账号 $WEB_USER、$SHELL_USER 与它们的 home"
else
  echo "     系统账号 $WEB_USER、$SHELL_USER"
fi
echo "     $ACME_WEBROOT（ACME 校验目录，留着无影响）"

echo
if [ "$PURGE_CONFIG" = "1" ] && [ -s "$ETC_DIR/agent.key" ]; then
  printf '确认删除代理密钥 %s？（它会同时删掉所有已保存秘密的可解性）输入 yes 继续: ' "$ETC_DIR/agent.key"
  IFS= read -r ANSWER
  if [ "$ANSWER" != "yes" ]; then
    echo '已取消（没有删除任何东西）。'
    exit 1
  fi
fi
if [ "$DELETE_SRV" = "1" ]; then
  printf '确认清空 %s 里的全部站点与应用数据？输入 yes 继续: ' "$SRV_ROOT"
  IFS= read -r ANSWER
  if [ "$ANSWER" != "yes" ]; then
    echo '已取消（没有删除任何东西）。'
    exit 1
  fi
fi

echo
echo '==== 执行 ===='

# 删之前先备份：带了 --purge-* 就意味着这次要丢掉的是“这台面板的全部身份”。
# 备份脚本挨着本脚本放（发布包里两个文件在同一个目录）。
if [ "$PURGE_CONFIG" = "1" ] || [ "$PURGE_DATA" = "1" ]; then
  if [ "$DO_BACKUP" = "0" ]; then
    echo '      按参数要求不备份（--no-backup）：下面删掉的东西没有副本'
  elif [ ! -f "$SELF_DIR/backup-state.sh" ]; then
    echo "错误: 同目录里没有 backup-state.sh，无法在删除前备份。" >&2
    echo "      换成发布包里的 uninstall.sh（安装包 deploy/scripts/ 下有它），" >&2
    echo "      或者确认不要备份后再加 --no-backup。" >&2
    exit 2
  elif [ ! -r /etc/upanel/upanel.json ]; then
    # 没有配置就无从知道库在哪里；这时删掉的只是程序，用不着备份。
    echo '      没有 /etc/upanel/upanel.json：没有可备份的状态，直接继续'
  else
    if [ -z "$BACKUP_TO" ]; then
      BACKUP_TO="/var/backups/upanel/uninstall-$(date -u +%Y%m%dT%H%M%SZ)"
    fi
    # 备份脚本自己会停服务、拷完再起回来；这里随后又把服务停掉，多一次起落在
    # 卸载路径上无所谓——顺序上“先备份、后删除”比省那一次 restart 重要得多。
    echo "      先备份到 $BACKUP_TO"
    if sh "$SELF_DIR/backup-state.sh" --to "$BACKUP_TO" 2>&1 | sed 's/^/      /'; then
      echo "      备份完成：$BACKUP_TO"
    else
      echo '错误: 备份没有完成，删除已中止（确认不要备份请加 --no-backup）。' >&2
      exit 2
    fi
  fi
fi

systemctl disable --now upanel-web.service >/dev/null 2>&1 || echo '      upanel-web 未在运行'
systemctl disable --now upanel-agent.service >/dev/null 2>&1 || echo '      upanel-agent 未在运行'

if [ "$DELETE_SRV" = "1" ]; then
  # 只删目录内的内容，不删挂载点本身：/srv/upanel 可能是独立挂载。
  if [ -d "$SRV_ROOT" ]; then
    find "$SRV_ROOT" -mindepth 1 -maxdepth 1 -exec rm -rf -- {} + 2>/dev/null \
      && echo "      已清空 $SRV_ROOT 的内容（目录本身保留）" \
      || echo "      清空 $SRV_ROOT 失败（看上面的错误）"
  fi
fi

if [ -n "$TARGETS" ]; then
  printf '%s' "$TARGETS" | while IFS= read -r t; do
    [ -n "$t" ] || continue
    case "$t" in
      /etc/systemd/system/upanel-*.service) rm -f "$t" && echo "      删除 $t" ;;
      *) rm -rf -- "$t" && echo "      删除 $t" ;;
    esac
  done
fi

systemctl daemon-reload || echo '      警告: daemon-reload 失败'

if [ "$PURGE_ACCOUNTS" = "1" ]; then
  for u in "$WEB_USER" "$SHELL_USER"; do
    if id -u "$u" >/dev/null 2>&1; then
      userdel "$u" >/dev/null 2>&1 && echo "      删除账号 $u" || echo "      删除账号 $u 失败"
    fi
    # 账号删了组常常还在：userdel 只在组里没有其他成员时才顺手删组。留着空组不会立刻
    # 出问题，但下一次安装就会遇到“组在、账号不在”，旧版安装器在那里直接失败（22.04 上
    # 抓到）。所以这里再试一次；删不掉（还有别的成员）就如实说明，不动别人的组。
    if getent group "$u" >/dev/null 2>&1; then
      if groupdel "$u" >/dev/null 2>&1; then
        echo "      删除组 $u"
      else
        echo "      保留组 $u（里面还有其他成员，不是这台机器上本产品独占的组）"
      fi
    fi
  done
  [ -d "$SHELL_HOME" ] && rm -rf -- "$SHELL_HOME" && echo "      删除 $SHELL_HOME"
  [ -d "$WEB_DATA" ] && rm -rf -- "$WEB_DATA" && echo "      删除 $WEB_DATA"
fi

KEEP_NOTE="  $SRV_ROOT         站点与应用数据（除非用了 --yes-delete-srv）
  $ACME_WEBROOT     ACME 校验目录"
[ "$PURGE_CONFIG" = "0" ] && KEEP_NOTE="$KEEP_NOTE
  $ETC_DIR         配置、面板证书、代理密钥"
[ "$PURGE_DATA" = "0" ] && KEEP_NOTE="$KEEP_NOTE
  $WEB_DATA    面板数据库（账号、审计、任务记录）
  $AGENT_DATA    代理数据库
  $MANUAL_CERT_DIR    签发与导入的证书"
[ "$PURGE_ACCOUNTS" = "0" ] && KEEP_NOTE="$KEEP_NOTE
  系统账号 $WEB_USER、$SHELL_USER"

cat <<EOF

==== 卸载完成 ====
上面列出的路径已删除。仍然保留的：
$KEEP_NOTE
EOF

# 备份路径单独打：它只在“真的备份了”的时候存在，写进上面的固定文本就会变成一句谎。
if [ -n "${BACKUP_TO:-}" ] && [ "${DO_BACKUP:-1}" = "1" ]; then
  printf '\n本次删除前的备份: %s\n整份拷走再清理；恢复步骤见 docs/RECOVERY.md。\n' "$BACKUP_TO"
fi

cat <<EOF
如果要重新安装：把发布包解开，再次执行
  sudo sh deploy/scripts/install.sh --source .
安装器是幂等的：已存在的账号、目录、证书、配置、密钥都会沿用。

如果这次把面板删得干净到想恢复数据，只能从删除前的备份里恢复
（安装器与卸载器都会在动数据库之前备份；手工备份用 deploy/scripts/backup-state.sh，
见 docs/RECOVERY.md 与 docs/LOCAL_START.md）。
EOF
