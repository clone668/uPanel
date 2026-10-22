#!/bin/sh
# 真机事实记录：Samba 的 hosts allow 对环回 127.0.0.1 永远放行（DECISIONS Q234）。
#
# 这一段不是产品代码，是在**一次性测试虚拟机**上把“为什么那条断言错了”变成可复现的记录：
#   1. 往 smb.conf 末尾临时加一段 `hosts allow = 10.0.0.0/24` 的共享（nstest）；
#   2. 从 network namespace 里的 10.99.0.2 连进来 —— 期望被拒；
#   3. 从 127.0.0.1 连进来 —— 期望**连得上**（这就是那条断言错在哪）；
#   4. 还原 smb.conf 并重载。
#
# 用法（VM 内 root）：
#   sudo sh probe5.sh
# 依赖：samba 已装、有一个受管理 Samba 账号（默认 smbp08rw），密码经环境变量给：
#   sudo SMB_USER=smbp08rw SMB_PASS='…' sh probe5.sh
# 密码只用于临时 authfile（0600，用完删掉），不进 argv。
set -u

SMB_USER=${SMB_USER:?需要 SMB_USER（一个能登录的 Samba 账号）}
SMB_PASS=${SMB_PASS:?需要 SMB_PASS（该账号的密码，只经环境变量传）}
SMB_CONF=/etc/samba/smb.conf
BAK=/tmp/probe5-smb.conf.bak
AUTH=/tmp/probe5-auth.txt
TESTDIR=/tmp/probe5-dir
NETNS=probe5ns
VETH_H=probe5v0
VETH_G=probe5v1
NS_HOST=10.99.0.1
NS_CLIENT=10.99.0.2

say() { printf '\n== %s\n' "$1"; }

restore() {
  ip netns del "$NETNS" >/dev/null 2>&1 || true
  ip link del "$VETH_H" >/dev/null 2>&1 || true
  if [ -f "$BAK" ]; then
    cp "$BAK" "$SMB_CONF"
    smbcontrol all reload-config >/dev/null 2>&1 || true
    rm -f "$BAK"
  fi
  rm -rf "$TESTDIR" "$AUTH"
}
trap restore EXIT INT TERM

say "环境"
smbd --version 2>&1 | head -1
echo "smb.conf: $SMB_CONF"

say "夹具：目录、临时共享段、netns 客户端"
mkdir -p "$TESTDIR"
chmod 0777 "$TESTDIR"
printf 'username=%s\npassword=%s\ndomain=\n' "$SMB_USER" "$SMB_PASS" >"$AUTH"
chmod 0600 "$AUTH"
cp "$SMB_CONF" "$BAK"

ip netns add "$NETNS"
ip link add "$VETH_H" type veth peer name "$VETH_G"
ip link set "$VETH_G" netns "$NETNS"
ip addr add "$NS_HOST/24" dev "$VETH_H"
ip link set "$VETH_H" up
ip netns exec "$NETNS" ip addr add "$NS_CLIENT/24" dev "$VETH_G"
ip netns exec "$NETNS" ip link set "$VETH_G" up
ip netns exec "$NETNS" ip link set lo up
echo "netns 客户端地址：$(ip netns exec "$NETNS" ip -4 -o addr show dev "$VETH_G" | awk '{print $4}')"

cat >>"$SMB_CONF" <<EOF

[nstest]
   path = $TESTDIR
   read only = no
   valid users = $SMB_USER
   hosts allow = 10.0.0.0/24
EOF
smbcontrol all reload-config
sleep 1
echo "testparm 读回的 hosts allow：$(testparm -s --suppress-prompt --section-name=nstest \
  --parameter-name='hosts allow' "$SMB_CONF" 2>/dev/null)"

say "从 $NS_CLIENT 连进来（不在 10.0.0.0/24 里）"
ip netns exec "$NETNS" smbclient "//$NS_HOST/nstest" -A "$AUTH" -m SMB3 -c 'ls' 2>&1 | head -3

say "从 127.0.0.1 连进来（Samba 文档说永远放行）"
smbclient //127.0.0.1/nstest -A "$AUTH" -m SMB3 -c 'ls' 2>&1 | head -5

say "还原"
restore
trap - EXIT INT TERM
echo "smb.conf 里还有 nstest 吗：$(grep -c '\[nstest\]' "$SMB_CONF" 2>/dev/null || echo 0)"
