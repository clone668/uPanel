#!/bin/sh
# P16.2 端到端验收的**宿主侧**驱动：把包与脚本送进验收 VM，按相位跑完，再把读数取回来。
#
# 为什么要有这个脚本：P16.2 的相位之间夹着一次真重启，一条命令跑不完；靠人手一条条
# 敲 ssh 的后果是"哪一步忘了、哪一步跑的是旧脚本"没人说得清，而这一轮要证明的正是
# "安装/升级/卸载在真机上能走通"。所以驱动也进仓库，和验收脚本一起被 `sh -n` 检查。
#
# 用法（在开发宿主上，Git Bash）：
#   sh tests/integration/p16-2-run-on-2204.sh            # 全流程
#   PHASES=install,measure sh tests/integration/p16-2-run-on-2204.sh   # 只跑前两段
#   KEEP_LOG=1 PHASES=boot,upgrade sh …                  # 续跑：接着上一段的 run.log 往后写
#
# 两个平台靠环境变量分开（脚本名带 2204 是历史原因，两边通用）：
#   SSH_PORT=2223 SSH_KEY=…/upanel-2204/id_ed25519 PLATFORM=22.04 EVID=docs/evidence/P16/<日期>-p16.2
#   SSH_PORT=2222 SSH_KEY=…/upanel-2404/id_ed25519 PLATFORM=24.04 EVID=docs/evidence/P16/<日期>-p16.2-2404
#
# 前置：
#   * VM 已经从 cloud image 重建过系统盘并起来了（本脚本**不**替你重建，破坏性动作不藏在这里）；
#     seed server 必须先起着（§8.13 第四条：漏起 seed 会让客户机带 DataSourceNone 白跑一整轮开机）
#   * 宿主能 ssh 到 VM：SSH_KEY / SSH_PORT / SSH_HOST / SSH_USER
#   * dist/ 下已经有发布包：VERSION=... ARCHES=amd64 sh scripts/release.sh
#
# 产物：docs/evidence/P16/<日期>-p16.2/ 下的 run.log、每相位输出、facts/、logs/。
#
# 重启纪律（§8.13）：`boot` 相位之前的重启是**先等客户机下线、再等它回来**（15 分钟上限），
# 不是"探通就算回来"——`systemctl reboot` 返回时 sshd 还活着，立刻探活必然成功。
#
# 退出码：0 各相位都无失败；1 有相位报失败；2 环境/传输问题。

set -u

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
cd "$ROOT" || exit 2

SSH_HOST=${SSH_HOST:-127.0.0.1}
SSH_PORT=${SSH_PORT:-2223}
SSH_USER=${SSH_USER:-upanel}
SSH_KEY=${SSH_KEY:-/h/Code/vms/upanel-2204/id_ed25519}
PKG=${PKG:-$(ls -1t dist/upanel-*-linux-amd64.tar.gz 2>/dev/null | head -n 1)}
REMOTE_DIR=${REMOTE_DIR:-/root/upanel-pkg}
REMOTE_SCRIPT=${REMOTE_SCRIPT:-/root/p16-2-acceptance.sh}
PHASES=${PHASES:-install,measure,boot,upgrade,failup,uninstall,purge}
EVID=${EVID:-docs/evidence/P16/$(date -u +%Y-%m-%d)-p16.2}
PLATFORM=${PLATFORM:-22.04}

if [ -z "$PKG" ] || [ ! -f "$PKG" ]; then
  echo "错误: 找不到发布包（先跑 scripts/release.sh，或用 PKG=... 指定）" >&2
  exit 2
fi

SSH="ssh -i $SSH_KEY -p $SSH_PORT -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o BatchMode=yes -o ConnectTimeout=5 $SSH_USER@$SSH_HOST"
SCP="scp -i $SSH_KEY -P $SSH_PORT -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=5"

mkdir -p "$EVID/facts" "$EVID/logs" || exit 2
RUNLOG=$EVID/run.log
# 默认清空重来；KEEP_LOG=1 时接着上一段写——同一份证据目录分两段跑（比如 VM 重启后
# SSH 掉链子、剩下几相位单独补跑）时用它，否则第一段的读数会被这次调用悄悄抹掉。
if [ "${KEEP_LOG:-0}" = "1" ]; then
  printf '\n===== 续跑追加于 %s =====\n' "$(date -u +%FT%TZ)" >>"$RUNLOG"
else
  : >"$RUNLOG"
fi

say() { printf '%s\n' "$*" | tee -a "$RUNLOG"; }
FAILED_PHASES=""

say "== P16.2 驱动开始 $(date -u +%FT%TZ) 平台=$PLATFORM 包=$PKG"

# ---- 0. 传到 VM 并校验 ----
say "== 传输与校验"
$SCP -q "$PKG" "$ROOT/tests/integration/p16-2-acceptance.sh" "$SSH_USER@$SSH_HOST:/tmp/" >>"$RUNLOG" 2>&1 \
  || { say "传输失败"; exit 2; }
$SSH "sudo rm -rf $REMOTE_DIR && sudo mkdir -p $REMOTE_DIR && \
      sudo tar xzf /tmp/$(basename -- "$PKG") -C $REMOTE_DIR --strip-components=1 && \
      sudo sh -c 'cd $REMOTE_DIR && sha256sum -c --quiet SHA256SUMS' && \
      sudo install -m 0755 /tmp/p16-2-acceptance.sh $REMOTE_SCRIPT && \
      sudo sh -c 'cat $REMOTE_DIR/VERSION'" >>"$RUNLOG" 2>&1 \
  || { say "VM 内解包/校验失败（看 run.log）"; exit 2; }
say "包在 VM 内校验通过"

# ---- 1. 逐相位跑；install 之后要重启才跑 boot ----
# 等 SSH 可用，最多等 $1 秒（每 5 秒探一次）。
wait_ssh() {
  _limit=${1:-120}
  _i=0
  while [ "$_i" -lt "$_limit" ]; do
    if $SSH 'echo up' >/dev/null 2>&1; then
      return 0
    fi
    _i=$((_i + 5))
    sleep 5
  done
  return 1
}

run_phase() {
  _p=$1
  say "== PHASE=$_p"
  # 相位之间可能夹着重启或网络抖动；先等 ssh 回来（最多 2 分钟），别让一条瞬断的
  # 连接把整相位记成失败。
  wait_ssh 120 || say "警告: 跑 $_p 之前等不到 ssh"
  # 不能写成 `$SSH ... | tee`：管道的退出码是 tee 的，ssh 失败会被吞掉，
  # 结果是"五个断言挂了却报无失败"。先落到文件，再取退出码。
  $SSH "sudo PHASE=$_p PKG_DIR=$REMOTE_DIR sh $REMOTE_SCRIPT" >"$EVID/phase-$_p.log" 2>&1
  _rc=$?
  cat "$EVID/phase-$_p.log" >>"$RUNLOG"
  $SSH "sudo cat /var/tmp/upanel-accept-p16-2/RESULT-$_p" >"$EVID/RESULT-$_p" 2>/dev/null
  cat "$EVID/RESULT-$_p" 2>/dev/null >>"$RUNLOG"
  _fails=$(sed -n 's/^PASS=[0-9]* FAIL=\([0-9]*\).*/\1/p' "$EVID/RESULT-$_p" 2>/dev/null | head -n 1)
  if [ "$_rc" -ne 0 ] || [ "${_fails:-0}" -gt 0 ]; then
    FAILED_PHASES="$FAILED_PHASES $_p"
  fi
  grep -E '^(FAIL|NOT RUN) ' "$EVID/phase-$_p.log" 2>/dev/null || true
  return 0
}

reboot_vm() {
  say "== 重启 VM"
  $SSH 'sudo systemctl reboot' >/dev/null 2>&1
  # 先等它**真的下去**。`systemctl reboot` 返回的时候 sshd 还在，立刻去探活必然成功，
  # 于是驱动以为"重启完成了"就直接跑 boot 相位——而机器正在关机，那一相位连同后面
  # 五个相位的 ssh 全部 banner exchange 超时（P16.2 第一次干净重跑就折在这里：
  # 报"VM 回来了（约 0s 后）"，然后 boot/upgrade/failup/uninstall/purge 全空）。
  _i=0
  _down=0
  while [ "$_i" -lt 60 ]; do
    if ! $SSH 'echo up' >/dev/null 2>&1; then
      _down=1
      say "VM 已下线（约 $((_i * 5))s 后 ssh 不可达）"
      break
    fi
    _i=$((_i + 5))
    sleep 5
  done
  if [ "$_down" -ne 1 ]; then
    say "等不到 VM 下线（systemctl reboot 没生效？）"
    return 1
  fi
  # tcg 下的 22.04 从关机到 sshd 可用实测约 4 分钟（宿主机 CPU 争用时会更多），给 15 分钟。
  if wait_ssh 900; then
    say "VM 回来了"
    return 0
  fi
  say "VM 15 分钟内没有回来"
  return 1
}

for p in $(printf '%s' "$PHASES" | tr ',' ' '); do
  # boot 相位的前置就是"机器刚重启过"，所以重启动作挂在 boot 之前，而不是散在某两个
  # 相位后面——否则相位顺序一变，重启就跟丢。
  if [ "$p" = "boot" ]; then
    reboot_vm || { say "重启失败"; exit 2; }
  fi
  run_phase "$p"
done

# ---- 2. 取回事实与日志 ----
say "== 取回 facts / logs"
$SSH "sudo tar czf /tmp/p162-evidence.tgz -C /var/tmp/upanel-accept-p16-2 facts logs RESULT-ALL 2>/dev/null" \
  >>"$RUNLOG" 2>&1
$SCP -q "$SSH_USER@$SSH_HOST:/tmp/p162-evidence.tgz" "$EVID/" >>"$RUNLOG" 2>&1 \
  && ( cd "$EVID" && tar xzf p162-evidence.tgz && rm -f p162-evidence.tgz )

say ""
if [ -z "$FAILED_PHASES" ]; then
  say "== 全部相位无失败 =="
  exit 0
fi
say "== 有相位报失败:$FAILED_PHASES（看 phase-*.log 与 RESULT-*）=="
exit 1
