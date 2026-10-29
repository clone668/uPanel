#!/bin/sh
# 取回 P12.4 的 logs/payloads；不把数据库、单元文件之外的系统状态、私钥或整个工作目录带进证据包。
# 用法：sh tools/pull-p124-evidence.sh 2204|2404
set -eu
VM=${1:-2404}
case "$VM" in
  2204) PORT=2223; KEY=/h/Code/vms/upanel-2204/id_ed25519 ;;
  2404) PORT=2222; KEY=/h/Code/vms/upanel-2404/id_ed25519 ;;
  *) echo "用法：sh $0 2204|2404" >&2; exit 2 ;;
esac
SSH="ssh -p $PORT -i $KEY upanel@127.0.0.1"
SCP="scp -P $PORT -i $KEY"
WORK=/var/tmp/upanel-accept-p12-4
DEST=/h/Code/uPanel/docs/evidence/P12/2026-10-24-p12.4
TMP=/tmp/p124-$VM-pull
rm -rf "$TMP"
mkdir -p "$TMP"

# 秘密清单：本轮用到的两个共享口令（管理员沿用 P12.2 的那个）、私钥头、以及 SFTP 仓库那两份。
SECRETS='p12-2-admin-horse-9 p12-4-viewer-pass-1 p12-2-repo-horse-7 BEGIN OPENSSH PRIVATE KEY RESTIC_PASSWORD='

echo "==> VM 侧秘密扫描（只扫描将取回的证据）"
for secret in $SECRETS; do
  if $SSH "sudo grep -rlF '$secret' $WORK/logs $WORK/payloads 2>/dev/null | head -5" | grep -q .; then
    echo "错误：证据含口令或私钥明文（$secret），停止取回" >&2
    exit 1
  fi
done
echo "    零命中"

echo "==> 打包并取回 logs/payloads"
# units/ 是脚本自己复制出来的渲染结果（不含任何秘密），与 logs 一起进包。
# EVID 是脚本之外单独留的一份现场：迁移前后各一次的 schema 版本与 scripts 行
# （解释器从基名 sh 改成 /bin/sh 的真实数据证据）。
EVID=/var/tmp/p12-4-evidence
$SSH "sudo tar czf /var/tmp/p124-pull-$VM.tgz -C $WORK logs payloads && sudo chmod 644 /var/tmp/p124-pull-$VM.tgz"
$SSH "sudo tar czf /var/tmp/p124-evid-$VM.tgz -C $EVID . 2>/dev/null && sudo chmod 644 /var/tmp/p124-evid-$VM.tgz || true"
$SCP upanel@127.0.0.1:/var/tmp/p124-evid-$VM.tgz "$TMP/" >/dev/null 2>&1 || true
$SCP upanel@127.0.0.1:/var/tmp/p124-pull-$VM.tgz "$TMP/" >/dev/null
mkdir -p "$DEST/logs/$VM" "$DEST/payloads/$VM"
tar xzf "$TMP/p124-pull-$VM.tgz" -C "$TMP"
cp -r "$TMP/logs/." "$DEST/logs/$VM/"
cp -r "$TMP/payloads/." "$DEST/payloads/$VM/"
if [ -f "$TMP/p124-evid-$VM.tgz" ]; then
  mkdir -p "$DEST/migration/$VM"
  tar xzf "$TMP/p124-evid-$VM.tgz" -C "$DEST/migration/$VM"
fi

for secret in $SECRETS; do
  if grep -rlF "$secret" "$DEST/logs/$VM" "$DEST/payloads/$VM" "$DEST/migration/$VM" 2>/dev/null | head -5 | grep -q .; then
    echo "错误：宿主证据秘密扫描命中（$secret）" >&2
    exit 1
  fi
done
printf 'PASS %s / FAIL %s / NOT RUN %s\n' \
  "$(grep -c '^PASS' "$DEST/logs/$VM/results.txt" || true)" \
  "$(grep -c '^FAIL' "$DEST/logs/$VM/results.txt" || true)" \
  "$(grep -c '^NOTRUN' "$DEST/logs/$VM/results.txt" || true)"
grep '^FAIL' "$DEST/logs/$VM/results.txt" || true
