#!/bin/sh
# P12.5 的一次性测量：在这台验收虚拟机上，一次完整的 certbot 签发要多少秒。
#
# 为什么要它：产品给 certbot 的上限是 120 秒（internal/certificates/certbot.go 的
# certbotTimeout，注释写明"正常 30 秒内结束"）。真机验收第 4、7 两轮都撞到
# "certbot 超过 2m0s 没有结束"，而第 6 轮同一步骤十几秒就过了，第 7 轮紧接着的另一次
# 签发也只用了 17 秒（Pebble 日志 14:09:24 → 14:09:41）。所以要么是这台 2 vCPU 的
# TCG 模拟机偶尔卡住，要么是某个确定的慢点——不量清楚就分不出是环境还是产品。
#
# 量的是**签发本身**：Pebble 与 HTTP-01 校验口先起好、ACME 账号先注册好，计时只包住
# `certbot certonly` 那一条命令。连续量三次，用不同的域名与不同的订单。
#
# 用法：sudo sh tests/integration/p125-certbot-timing.sh
# 退出码：0 三次都成功；1 有失败；2 环境不满足。
set -u

WORK=/tmp/p125-t3
PEBBLE_BIN=${PEBBLE_BIN:-/usr/local/bin/pebble}
PEBBLE_CFG_SRC=${PEBBLE_CFG_SRC:-/opt/p103/pebble/config.json}
ACME_URL=https://127.0.0.1:14000/dir
WEBROOT=/var/www/p125-t3
API_CERT=$WORK/certs/cert.pem
API_KEY=$WORK/certs/key.pem
CA_FILE=/usr/local/share/ca-certificates/p125-acme-api.crt
EMAIL=ops@example.test
SUFFIX=$(openssl rand -hex 3)
PEBBLE_PID=""
HTTP_PID=""

cleanup() {
  [ -n "$HTTP_PID" ] && kill "$HTTP_PID" 2>/dev/null
  [ -n "$PEBBLE_PID" ] && kill "$PEBBLE_PID" 2>/dev/null
  for i in 1 2 3; do
    sed -i "/t3-$i-$SUFFIX.example.test/d" /etc/hosts 2>/dev/null
  done
}
trap cleanup EXIT INT TERM

[ -x "$PEBBLE_BIN" ] || { echo "没有 $PEBBLE_BIN" >&2; exit 2; }
[ -r "$PEBBLE_CFG_SRC" ] || { echo "没有 $PEBBLE_CFG_SRC" >&2; exit 2; }
command -v certbot >/dev/null 2>&1 || { echo "没有 certbot" >&2; exit 2; }

rm -rf "$WORK"
mkdir -p "$WORK/certs" "$WORK/logs" "$WEBROOT/.well-known/acme-challenge"

# Pebble 的 API 证书：现生成一张十年自签，既当监听证书也当信任锚。
openssl req -x509 -newkey rsa:2048 -nodes -sha256 -days 3650 \
  -subj "/CN=pebble-acme-api" \
  -addext "subjectAltName=IP:127.0.0.1,DNS:localhost" \
  -addext "basicConstraints=critical,CA:TRUE" \
  -keyout "$API_KEY" -out "$API_CERT" >"$WORK/logs/cert.log" 2>&1

python3 - "$PEBBLE_CFG_SRC" "$WORK/config.json" "$API_CERT" "$API_KEY" <<'PY'
import json, sys
src, dst, cert, key = sys.argv[1:5]
cfg = json.load(open(src))
cfg.setdefault('pebble', {})['certificate'] = cert
cfg['pebble']['privateKey'] = key
json.dump(cfg, open(dst, 'w'), indent=2)
PY

# HTTP-01：Pebble 的 httpPort 是它**去连**的端口，所以校验口由这里提供。
( cd "$WEBROOT" && python3 -m http.server 5002 ) >"$WORK/logs/http5002.log" 2>&1 &
HTTP_PID=$!

"$PEBBLE_BIN" -config "$WORK/config.json" >"$WORK/logs/pebble.log" 2>&1 &
PEBBLE_PID=$!
UP=no
for _ in $(seq 1 150); do
  if curl -fsS --cacert "$API_CERT" "$ACME_URL" >/dev/null 2>&1; then UP=yes; break; fi
  sleep 0.2
done
[ "$UP" = yes ] || { echo "Pebble 没起来" >&2; exit 2; }

cp "$API_CERT" "$CA_FILE"
update-ca-certificates >/dev/null 2>&1
TRUSTED=no
for _ in $(seq 1 30); do
  if curl -fsS "$ACME_URL" >/dev/null 2>&1; then TRUSTED=yes; break; fi
  sleep 0.2
done
[ "$TRUSTED" = yes ] || { echo "系统信任库没有接受这张 API 证书" >&2; exit 2; }

# Pebble 的账号只在它自己的内存里：清掉指向这台测试 ACME 的那一份，重新注册。
ACCT_DIR=/etc/letsencrypt/accounts/127.0.0.1:14000
rm -rf "$ACCT_DIR"
T0=$(date +%s%3N)
certbot register --server "$ACME_URL" --email "$EMAIL" --agree-tos \
  --non-interactive --no-eff-email >"$WORK/logs/register.log" 2>&1
RC=$?
T1=$(date +%s%3N)
printf 'register\t%d ms\trc=%d\n' "$((T1 - T0))" "$RC"
[ "$RC" -eq 0 ] || { tail -5 "$WORK/logs/register.log" >&2; exit 1; }

FAILED=0
for i in 1 2 3; do
  DOM="t3-$i-$SUFFIX.example.test"
  printf '127.0.0.1 %s\n' "$DOM" >>/etc/hosts
  S0=$(date +%s%3N)
  timeout 300 certbot certonly --webroot -w "$WEBROOT" -d "$DOM" \
    --server "$ACME_URL" --email "$EMAIL" --agree-tos --non-interactive \
    --no-eff-email --keep-until-expiring \
    >"$WORK/logs/issue-$i.log" 2>&1
  RC=$?
  S1=$(date +%s%3N)
  printf 'issue-%d\t%d ms\trc=%d\n' "$i" "$((S1 - S0))" "$RC"
  if [ "$RC" -ne 0 ]; then
    FAILED=1
    tail -6 "$WORK/logs/issue-$i.log" >&2
  fi
done

exit "$FAILED"
