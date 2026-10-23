#!/bin/sh
# 交叉构建 Linux/amd64 二进制（验收用）。
#
# 为什么需要这个脚本：开发宿主是 Windows，`scripts/build.sh` 产出的是 PE，推到
# Ubuntu 上一律 `Exec format error`。而"验收跑的是哪一份二进制"必须能被复现，
# 所以构建参数固定在这里，不接受随手 `go build`。
#
# 复现性：
#   - `-trimpath` 去掉宿主绝对路径；
#   - `-buildvcs=false` 不让 Go 把工作树的 VCS 状态（vcs.revision / vcs.modified）
#     写进二进制。少了它，同一份源码在脏工作树与干净工作树下的产物字节不同，
#     "重建得到同一个 sha256"这句话就不成立。版本与 commit 由 ldflags 注入，
#     信息不因此缺失。
#   - BUILD_DATE 不给就用当前时间，那是唯一允许变化的部分；要逐字节复现就显式给值。
#
# 用法：
#   sh scripts/build-linux.sh                       # commit 取 HEAD，构建时间取现在
#   VERSION=0.0.0-dev COMMIT=d99998c BUILD_DATE=2026-10-23T20:46:06Z sh scripts/build-linux.sh
#
# 产物：bin/linux-amd64/{upanel-web,upanel-agent,upanelctl}

set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$ROOT"

VERSION=${VERSION:-0.0.0-dev}
COMMIT=${COMMIT:-$(git rev-parse --short HEAD 2>/dev/null || echo unknown)}
BUILD_DATE=${BUILD_DATE:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}
LDFLAGS="-X upanel/internal/version.Version=$VERSION -X upanel/internal/version.Commit=$COMMIT -X upanel/internal/version.BuildDate=$BUILD_DATE"

mkdir -p bin/linux-amd64

echo "==> 交叉构建 linux/amd64（版本 $VERSION，commit $COMMIT，构建 $BUILD_DATE）"
for pkg in upanel-web upanel-agent upanelctl; do
  GOOS=linux GOARCH=amd64 go build -buildvcs=false -trimpath -ldflags "$LDFLAGS" \
    -o "bin/linux-amd64/$pkg" "./cmd/$pkg"
done

# 守卫：产物必须是 ELF。宿主上把 PE 当成 Linux 二进制推上去是最费时间的一类错，
# 在这里就拦住比在虚机上 `Exec format error` 好。
for pkg in upanel-web upanel-agent upanelctl; do
  magic=$(od -An -tx1 -N4 "bin/linux-amd64/$pkg" | tr -d ' \n')
  if [ "$magic" != "7f454c46" ]; then
    echo "错误: bin/linux-amd64/$pkg 不是 ELF（魔术数 $magic）" >&2
    exit 1
  fi
done

echo "==> sha256"
sha256sum bin/linux-amd64/upanel-web bin/linux-amd64/upanel-agent bin/linux-amd64/upanelctl
