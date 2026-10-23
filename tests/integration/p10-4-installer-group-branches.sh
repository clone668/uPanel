#!/bin/sh
# P10.4 安装器「组在、账号不在」两支的显式验证（DECISIONS Q305）。
#
# 这两支是 22.04 上真机抓出来的（useradd: group upanel-web exists - if you want to add
# this user to that group, use -g）：`userdel` 只在组里没有其他成员时才顺手删组，所以
# “用户没了、组还在”随时会出现。安装器要么安全地接住它，要么在装不下的时候说清楚为什么
# ——两种都要有证据，不能只因为主路径跑绿了就算修好。
#
# 用法（root）：sudo sh p10-4-installer-group-branches.sh
# 环境变量：PKG（解开的发布包根目录）、PASSWORD（面板口令，用于最后确认能登录）。
set -u

PKG=${PKG:-/home/upanel/p104/pkg/upanel-0.1.0-linux-amd64}
PASSWORD=${PASSWORD:-install-correct-horse-9}
CA=/etc/upanel/tls/panel.pem
BASE=https://127.0.0.1:9443
P=0
F=0
pass() { P=$((P + 1)); printf 'PASS  %s\n' "$1"; }
fail() { F=$((F + 1)); printf 'FAIL  %s: %s\n' "$1" "${2:-}"; }
note() { printf '      %s\n' "$1"; }

run_install() { # 输出落在 $1，回显退出码
  _out=$1
  sh "$PKG/deploy/scripts/install.sh" --source "$PKG" >"$_out" 2>&1
  printf '%s' "$?"
}

health_code() {
  curl -sS --cacert "$CA" -o /dev/null -w '%{http_code}' --max-time 5 "$BASE/api/v1/health/live"
}

printf '==== 安装器「组在、账号不在」两支 ====\n'
[ -d "$PKG" ] || { printf '缺少发布包目录 %s\n' "$PKG" >&2; exit 2; }

# ---------------------------------------------------------------- B. 组里有成员 → 明确拒绝
printf '\n--- B. 组存在、里面有别的成员（不应该静默复用）---\n'
systemctl stop upanel-web >/dev/null 2>&1
useradd --system --no-create-home --shell /usr/sbin/nologin p104probe 2>/dev/null ||
  note "p104probe 已存在（继续）"
gpasswd -a p104probe upanel-web >/dev/null 2>&1
userdel upanel-web >/dev/null 2>&1
if id -u upanel-web >/dev/null 2>&1; then
  fail "前置：账号 upanel-web 已删除" "userdel 失败"
else
  pass "前置：账号 upanel-web 已删除（组有意保留）"
fi
if getent group upanel-web | grep -q 'p104probe'; then
  pass "前置：组 upanel-web 还在，成员里有 p104probe"
else
  fail "前置：组 upanel-web 里应有 p104probe" "$(getent group upanel-web)"
fi

RC=$(run_install /tmp/p104-branch-b.log)
if [ "$RC" != "0" ]; then
  pass "组里有其他成员时安装器拒绝继续（退出码 $RC）"
else
  fail "组里有其他成员时安装器拒绝继续" "退出码 0：它把危险状态当成正常装完了"
fi
if grep -q 'p104probe' /tmp/p104-branch-b.log; then
  pass "拒绝理由点名了组里的那个成员"
else
  fail "拒绝理由点名了组里的那个成员" "$(tail -n 3 /tmp/p104-branch-b.log | tr '\n' ' ')"
fi
if grep -q 'gpasswd -d' /tmp/p104-branch-b.log; then
  pass "拒绝理由给出了可执行的一行修复命令"
else
  fail "拒绝理由给出了修复命令" "$(tail -n 3 /tmp/p104-branch-b.log | tr '\n' ' ')"
fi
if id -u upanel-web >/dev/null 2>&1; then
  fail "拒绝之后没有偷偷建账号" "upanel-web 竟然被创建了"
else
  pass "拒绝之后没有偷偷建账号（安装器不动别人的组成员关系）"
fi

# 收拾现场，进入 A。
gpasswd -d p104probe upanel-web >/dev/null 2>&1
userdel p104probe >/dev/null 2>&1

# ---------------------------------------------------------------- A. 组是空的 → 安全复用
printf '\n--- A. 组存在且为空（应该复用组、补建账号）---\n'
if getent group upanel-web >/dev/null 2>&1; then
  _members=$(getent group upanel-web | cut -d: -f4)
  if [ -z "$_members" ]; then
    pass "前置：组 upanel-web 存在且没有成员"
  else
    fail "前置：组 upanel-web 应为空" "成员：$_members"
  fi
else
  groupadd --system upanel-web
  pass "前置：重建了一个空组 upanel-web（模拟 userdel 之后的半截状态）"
fi

RC=$(run_install /tmp/p104-branch-a.log)
if [ "$RC" = "0" ]; then
  pass "空组状态下安装器装完了（退出码 0）"
else
  fail "空组状态下安装器装完了" "退出码 $RC：$(tail -n 3 /tmp/p104-branch-a.log | tr '\n' ' ')"
fi
if grep -q '复用已存在的空组' /tmp/p104-branch-a.log; then
  pass "安装器说明了它复用的是已存在的空组"
else
  fail "安装器说明了它复用的是已存在的空组" "$(grep -n 'upanel-web' /tmp/p104-branch-a.log | head -3 | tr '\n' ' ')"
fi
if id -u upanel-web >/dev/null 2>&1; then
  pass "账号 upanel-web 被补建出来"
  _gid_user=$(id -g upanel-web)
  _gid_grp=$(getent group upanel-web | cut -d: -f3)
  if [ "$_gid_user" = "$_gid_grp" ]; then
    pass "补建的账号把 upanel-web 当主组（gid $_gid_user）"
  else
    fail "补建的账号把 upanel-web 当主组" "用户 gid=$_gid_user 组 gid=$_gid_grp"
  fi
else
  fail "账号 upanel-web 被补建出来" "id upanel-web 失败"
fi
if [ "$(systemctl is-active upanel-web.service)" = "active" ]; then
  pass "upanel-web.service 起来并 active"
else
  fail "upanel-web.service 起来并 active" "$(systemctl is-active upanel-web.service)"
fi
if [ "$(systemctl is-active upanel-agent.service)" = "active" ]; then
  pass "upanel-agent.service 起来并 active"
else
  fail "upanel-agent.service 起来并 active" "$(systemctl is-active upanel-agent.service)"
fi
CODE=$(health_code)
if [ "$CODE" = "200" ]; then
  pass "面板 HTTPS health 200（用安装的那张证书，不加 -k）"
else
  fail "面板 HTTPS health 200" "HTTP $CODE"
fi
LOGIN=$(curl -sS --cacert "$CA" -o /dev/null -w '%{http_code}' --max-time 10 \
  -X POST "$BASE/api/v1/auth/login" -H 'Content-Type: application/json' \
  -H "Origin: $BASE" -H 'X-UPanel-Request: 1' \
  -d "{\"username\":\"admin\",\"password\":\"$PASSWORD\"}")
if [ "$LOGIN" = "200" ]; then
  pass "补建账号之后管理员仍然能登录（数据库与权限没被换掉 uid 弄坏）"
else
  fail "补建账号之后管理员仍然能登录" "HTTP $LOGIN"
fi

printf '\n==== 汇总 ====\nPASS=%s FAIL=%s\n' "$P" "$F"
[ "$F" = "0" ] || exit 1
