#!/bin/sh
# 把某台 VM 上这一轮 P12.2 验收的证据取回宿主，放进 docs/evidence/P12/2026-10-24-p12.2/。
#
# 用法：sh tools/pull-p122-evidence.sh 2204|2404
#
# 取三样：整轮输出（run.log）、logs/ 与 payloads/（口令明文已由验收脚本就地替换）。
# 取回之前先在 VM 上做一次秘密扫描，命中就报错退出；数据库、随机源文件和测试私钥
# 不进中间压缩包，证据目录不能带明文口令或私钥。
set -eu

VM=${1:-2404}
case "$VM" in
  2204) PORT=2223; KEY=/h/Code/vms/upanel-2204/id_ed25519 ;;
  2404) PORT=2222; KEY=/h/Code/vms/upanel-2404/id_ed25519 ;;
  *) echo "用法：sh $0 2204|2404" >&2; exit 2 ;;
esac

SSH="ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -p $PORT -i $KEY upanel@127.0.0.1"
SCP="scp -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -P $PORT -i $KEY"
WORK=/var/tmp/upanel-accept-p12-2
DEST=/h/Code/uPanel/docs/evidence/P12/2026-10-24-p12.2
TMP=/tmp/p122-$VM-pull
REPO_PASSWORD='p12-2-repo-horse-7'
PASSWORD='p12-2-admin-horse-9'

rm -rf "$TMP"
mkdir -p "$TMP"

echo "==> 秘密扫描（VM 侧，仅将取回的证据）"
for secret in "$REPO_PASSWORD" "$PASSWORD" 'BEGIN OPENSSH PRIVATE KEY'; do
  if $SSH "sudo grep -rlF '$secret' $WORK/logs $WORK/payloads /var/tmp/p12-2-run.log 2>/dev/null | head -5" | grep -q .; then
    echo "错误：证据里还有口令或私钥明文，停止取回" >&2
    exit 1
  fi
done
echo "    零命中"

echo "==> 打包（只含 logs 与 payloads）"
$SSH "sudo tar czf /var/tmp/p122-pull-$VM.tgz -C $WORK logs payloads && sudo cp /var/tmp/p12-2-run.log /var/tmp/p122-pull-$VM.runlog && sudo chmod 644 /var/tmp/p122-pull-$VM.tgz /var/tmp/p122-pull-$VM.runlog"

echo "==> 取回"
$SCP upanel@127.0.0.1:/var/tmp/p122-pull-$VM.tgz "$TMP/" >/dev/null
$SCP upanel@127.0.0.1:/var/tmp/p122-pull-$VM.runlog "$TMP/run.log" >/dev/null

echo "==> 落盘"
mkdir -p "$DEST/logs/$VM" "$DEST/payloads/$VM"
tar xzf "$TMP/p122-pull-$VM.tgz" -C "$TMP"
cp -r "$TMP/logs/." "$DEST/logs/$VM/"
cp -r "$TMP/payloads/." "$DEST/payloads/$VM/"
cp "$TMP/run.log" "$DEST/logs/$VM-run.log"

echo "==> 校验"
printf '    logs/%s 文件数：%s\n' "$VM" "$(find "$DEST/logs/$VM" -type f | wc -l)"
printf '    payloads/%s 文件数：%s\n' "$VM" "$(find "$DEST/payloads/$VM" -type f | wc -l)"
if [ -f "$DEST/logs/$VM/results.txt" ]; then
  printf '    PASS %s / FAIL %s / NOT RUN %s\n' \
    "$(grep -c '^PASS' "$DEST/logs/$VM/results.txt" || true)" \
    "$(grep -c '^FAIL' "$DEST/logs/$VM/results.txt" || true)" \
    "$(grep -c '^NOTRUN' "$DEST/logs/$VM/results.txt" || true)"
  grep '^FAIL' "$DEST/logs/$VM/results.txt" || true
fi
