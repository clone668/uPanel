#!/bin/sh
# P10.4 附加取证：SSH 隧道（docs/ACCESS.md 第 1 节那条命令的形状）。
#
# 起点是**开发机（Windows/git-bash）**，不是虚机内部：从本机 ssh 到虚机的 22 口，
# 把虚机回环上的 9443 转发到本机的 19443，然后在本机用浏览器/curl 该看到的地址取面板。
# 这正是文档里“你从自己的电脑建一条 SSH 隧道进来，浏览器看到 https://127.0.0.1:9443”的机制。
#
# 局限（写进证据，也写进 result.md 的 NOT RUN 一节）：这是脚本化的 curl，不是人点浏览器；
# 也没覆盖 Windows 原生 ssh.exe 与手机客户端这两种端点。
#
# 与另外两个脚本不同，这个**在宿主机上跑**（Windows/git-bash 也行），不在虚机里跑：
# 隧道的意义就是“从外面进来”，在虚机内部自己连自己证明不了转发。
#
# 用法：sh p10-4-ssh-tunnel.sh <标签> <ssh 端口> <ssh 私钥路径>
#   例：sh tests/integration/p10-4-ssh-tunnel.sh 22.04 2223 ~/vms/upanel-2204/id_ed25519
set -u
TAG=${1:-vm}
SSH_PORT=${2:?ssh port}
KEY=${3:?ssh key}
PANEL_PORT=9443
LOCAL_PORT=19443
WORK=$(mktemp -d)
ok=0; bad=0
chk() {
  if [ "$2" = "$3" ]; then printf 'PASS  %s\n' "$1"; ok=$((ok+1))
  else printf 'FAIL  %s（期望 %s，实际 %s）\n' "$1" "$2" "$3"; bad=$((bad+1)); fi
}
cleanup() { [ -n "${TUNNEL_PID:-}" ] && kill "$TUNNEL_PID" 2>/dev/null; }
trap cleanup EXIT INT TERM

# code <url>：只取 HTTP 状态码；连不上时 curl 不给数字（或给 000），统一成 000。
code() {
  c=$(curl -sS -o /dev/null -m 3 -w '%{http_code}' "$1" 2>/dev/null)
  case "$c" in ''|000) printf 000 ;; *) printf '%s' "$c" ;; esac
}

printf '== %s：取面板证书（走 ssh，不经隧道）==\n' "$TAG"
# /etc/upanel/tls 是 0750 root:upanel-web（站点私钥同目录，P10.3 的保护要求），
# 所以 SSH 用户读不到，必须 sudo——docs/ACCESS.md 第 1 节核对指纹用的也是 `sudo openssl`。
# 这里取的只是**证书**（公钥材料），私钥一个字节都不碰。
ssh -o BatchMode=yes -o StrictHostKeyChecking=no -i "$KEY" -p "$SSH_PORT" \
  upanel@127.0.0.1 'sudo -n cat /etc/upanel/tls/panel.pem' >"$WORK/panel.pem" 2>/dev/null
chk "拿到面板证书" 'yes' "$([ -s "$WORK/panel.pem" ] && echo yes || echo no)"
chk "证书里 127.0.0.1 在 SAN 里（隧道地址能通过主机名校验）" 'yes' \
  "$(openssl x509 -in "$WORK/panel.pem" -noout -text 2>/dev/null | grep -q 'IP Address:127.0.0.1' && echo yes || echo no)"

printf '\n== %s：隧道建立前，本机 %s 上没有服务 ==\n' "$TAG" "$LOCAL_PORT"
chk "隧道前本机 $LOCAL_PORT 连不上" '000' "$(code "https://127.0.0.1:$LOCAL_PORT/")"

printf '\n== %s：ssh -N -L %s:127.0.0.1:%s（同 ACCESS.md）==\n' "$TAG" "$LOCAL_PORT" "$PANEL_PORT"
ssh -o BatchMode=yes -o StrictHostKeyChecking=no -o ExitOnForwardFailure=yes \
    -i "$KEY" -p "$SSH_PORT" -N -L "127.0.0.1:$LOCAL_PORT:127.0.0.1:$PANEL_PORT" \
    upanel@127.0.0.1 &
TUNNEL_PID=$!
READY=no
for _ in $(seq 1 40); do
  if curl -sS -o /dev/null -m 2 --cacert "$WORK/panel.pem" \
      "https://127.0.0.1:$LOCAL_PORT/api/v1/health/live" 2>/dev/null; then
    READY=yes; break
  fi
  sleep 0.25
done
chk "隧道打通（经隧道取到面板响应）" 'yes' "$READY"
chk "隧道上 /api/v1/health/live 返回 200" '200' \
  "$(curl -sS -o /dev/null -m 5 --cacert "$WORK/panel.pem" -w '%{http_code}' "https://127.0.0.1:$LOCAL_PORT/api/v1/health/live" 2>/dev/null)"
curl -sS -o /dev/null -m 5 --cacert "$WORK/panel.pem" "https://127.0.0.1:$LOCAL_PORT/api/v1/health/live" >/dev/null 2>&1
chk "隧道上的证书通过校验（链 + 主机名，未加 -k）" '0' "$?"
chk "隧道响应带 HSTS" 'yes' \
  "$(curl -sS -D - -o /dev/null -m 5 --cacert "$WORK/panel.pem" "https://127.0.0.1:$LOCAL_PORT/api/v1/health/live" 2>/dev/null | grep -qi '^strict-transport-security:' && echo yes || echo no)"
PL=$(curl -sS -m 5 "http://127.0.0.1:$LOCAL_PORT/api/v1/health/live" 2>/dev/null || true)
chk "经隧道用明文 HTTP 说话拿不到业务响应" 'yes' \
  "$(printf '%s' "$PL" | grep -q '"status"' && echo no || echo yes)"

printf '\n== %s：收尾 ==\n' "$TAG"
kill "$TUNNEL_PID" 2>/dev/null; wait "$TUNNEL_PID" 2>/dev/null; TUNNEL_PID=
SLEPT=0
while [ "$SLEPT" -lt 40 ]; do
  [ "$(code "https://127.0.0.1:$LOCAL_PORT/")" = "000" ] && break
  SLEPT=$((SLEPT+1)); sleep 0.25
done
chk "隧道关闭后本机 $LOCAL_PORT 不再可用" '000' "$(code "https://127.0.0.1:$LOCAL_PORT/")"

rm -rf "$WORK"
printf '\nPASS=%s FAIL=%s\n' "$ok" "$bad"
[ "$bad" = "0" ]
