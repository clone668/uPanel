#!/bin/sh
# 取回 P12.3 的 logs/payloads；不把数据库、随机文件、私钥或整个工作目录带进证据包。
# 用法：sh tools/pull-p123-evidence.sh 2204|2404
set -eu
VM=${1:-2404}
case "$VM" in
  2204) PORT=2223; KEY=/h/Code/vms/upanel-2204/id_ed25519 ;;
  2404) PORT=2222; KEY=/h/Code/vms/upanel-2404/id_ed25519 ;;
  *) echo "用法：sh $0 2204|2404" >&2; exit 2 ;;
esac
SSH="ssh -p $PORT -i $KEY upanel@127.0.0.1"
SCP="scp -P $PORT -i $KEY"
WORK=/var/tmp/upanel-accept-p12-3
DEST=/h/Code/uPanel/docs/evidence/P12/2026-10-24-p12.3
TMP=/tmp/p123-$VM-pull
rm -rf "$TMP"
mkdir -p "$TMP"

echo "==> VM 侧秘密扫描（只扫描将取回的证据）"
for secret in 'p12-2-repo-horse-7' 'p12-2-admin-horse-9' 'p12-2-viewer-horse-4' 'BEGIN OPENSSH PRIVATE KEY'; do
  if $SSH "sudo grep -rlF '$secret' $WORK/logs $WORK/payloads 2>/dev/null | head -5" | grep -q .; then
    echo "错误：证据含口令或私钥明文，停止取回" >&2
    exit 1
  fi
done
echo "    零命中"

echo "==> 打包并取回 logs/payloads"
$SSH "sudo tar czf /var/tmp/p123-pull-$VM.tgz -C $WORK logs payloads && sudo chmod 644 /var/tmp/p123-pull-$VM.tgz"
$SCP upanel@127.0.0.1:/var/tmp/p123-pull-$VM.tgz "$TMP/" >/dev/null
mkdir -p "$DEST/logs/$VM" "$DEST/payloads/$VM"
tar xzf "$TMP/p123-pull-$VM.tgz" -C "$TMP"
cp -r "$TMP/logs/." "$DEST/logs/$VM/"
cp -r "$TMP/payloads/." "$DEST/payloads/$VM/"

for secret in 'p12-2-repo-horse-7' 'p12-2-admin-horse-9' 'p12-2-viewer-horse-4' 'BEGIN OPENSSH PRIVATE KEY'; do
  if grep -rlF "$secret" "$DEST/logs/$VM" "$DEST/payloads/$VM" 2>/dev/null | head -5 | grep -q .; then
    echo "错误：宿主证据秘密扫描命中" >&2
    exit 1
  fi
done
printf 'PASS %s / FAIL %s / NOT RUN %s\n' \
  "$(grep -c '^PASS' "$DEST/logs/$VM/results.txt" || true)" \
  "$(grep -c '^FAIL' "$DEST/logs/$VM/results.txt" || true)" \
  "$(grep -c '^NOTRUN' "$DEST/logs/$VM/results.txt" || true)"
grep '^FAIL' "$DEST/logs/$VM/results.txt" || true
