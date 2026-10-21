#!/bin/sh
# 采集 P07.1 验收机器的环境事实并跑原生测试二进制（只读；不依赖 Go 工具链）。
set -u
OUT=${OUT:-/tmp/p07-evidence}
mkdir -p "$OUT"

# 两个前置条件，第一次采集就是漏了它们才报出 192 条假失败（见 result.md §4 第 7 条）：
#   1) agent/api 的用例要建 root 属主 0700 的代理数据目录，必须以 root 跑；
#   2) storage/monitoring 的用例要从 tests/fixtures 读真机字节，用 UPANEL_FIXTURES 指过去。
if [ "$(id -u)" -ne 0 ]; then
  echo "collect-native.sh 必须以 root 运行（agentstore 会校验数据目录属主）" >&2
  exit 2
fi
export UPANEL_FIXTURES=${UPANEL_FIXTURES:-/tmp/fixtures}
if [ ! -d "$UPANEL_FIXTURES/storage-2404" ]; then
  echo "缺少夹具目录 $UPANEL_FIXTURES（把 tests/fixtures 传上来）" >&2
  exit 2
fi

{
  echo "# 环境事实（P07.1 验收机）"
  echo "## 采集时间"
  date -u +%Y-%m-%dT%H:%M:%SZ
  echo
  echo "## 内核与发行版"
  uname -a
  sed -n '1,6p' /etc/os-release
  echo
  echo "## CPU（-Accel tcg 的 QEMU 客机）"
  grep -m1 'model name' /proc/cpuinfo
  echo
  echo "## 磁盘布局"
  lsblk -o NAME,TYPE,SIZE,FSTYPE,MOUNTPOINTS,TRAN
  echo
  echo "## 承载 / 的设备"
  findmnt -no SOURCE,TARGET,FSTYPE /
  echo
  echo "## 内存"
  head -3 /proc/meminfo
  echo
  echo "## 验收二进制（sha256 与大小）"
  sha256sum /tmp/upanel-bin/upanel-agent /tmp/upanel-bin/upanel-web /tmp/upanel-bin/upanelctl
  ls -l /tmp/upanel-bin
  echo
  echo "## 测试二进制（宿主 Go 1.27.3 / GOOS=linux GOARCH=amd64 / CGO_ENABLED=0 交叉编译）"
  sha256sum /tmp/storage.test /tmp/agent.test /tmp/api.test /tmp/operations.test /tmp/files.test
  echo
  echo "## 相关工具版本"
  sgdisk --version 2>&1 | head -1
  blkid --version 2>&1 | head -1
  wipefs --version 2>&1 | head -1
  mkswap --version 2>&1 | head -1
  dmsetup --version 2>&1 | head -1
  cryptsetup --version 2>&1 | head -1 || echo "cryptsetup: 未安装"
} >"$OUT/env.txt" 2>&1

: >"$OUT/native-tests.txt"
: >"$OUT/native-tests-summary.txt"
for t in storage agent api operations files; do
  bin=/tmp/$t.test
  if [ ! -x "$bin" ]; then
    printf '== %-10s NOT RUN：测试二进制不存在\n' "$t" >>"$OUT/native-tests-summary.txt"
    continue
  fi
  log=$OUT/native-$t.txt
  # argon2id 登录在 TCG 客机上很慢，api 包 10 分钟跑不完；给 1 小时，超时才算真挂。
  "$bin" -test.v -test.timeout 3600s >"$log" 2>&1
  rc=$?
  npass=$(grep -c '^--- PASS' "$log" 2>/dev/null || true)
  nfail=$(grep -c '^--- FAIL' "$log" 2>/dev/null || true)
  nskip=$(grep -c '^--- SKIP' "$log" 2>/dev/null || true)
  printf '== %-10s exit=%s PASS=%s FAIL=%s SKIP=%s（%s）\n' \
    "$t" "$rc" "${npass:-0}" "${nfail:-0}" "${nskip:-0}" "$log" >>"$OUT/native-tests-summary.txt"
  cat "$log" >>"$OUT/native-tests.txt"
  echo >>"$OUT/native-tests.txt"
done

echo "done: $OUT"
ls -l "$OUT"
cat "$OUT/native-tests-summary.txt"
