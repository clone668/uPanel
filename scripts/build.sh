#!/bin/sh
# uPanel 构建脚本（P01.4）。
#
# 做三件事，顺序不能反：
#   1. 构建前端（Vue + Vite），产物 web/dist；
#   2. 把产物同步到 internal/webui/dist，让 go:embed 把它打进二进制；
#   3. 构建三个 Go 二进制到 bin/。
#
# 为什么第 2 步必须发生在第 3 步之前：go:embed 在编译期读取磁盘目录，
# 之后再拷贝产物不会改变已经编译好的二进制。
#
# 用法：
#   sh scripts/build.sh                       # 版本取默认值 0.0.0-dev
#   VERSION=0.1.0 COMMIT=$(git rev-parse --short HEAD) sh scripts/build.sh
#
# 要求：Node 版本与 .nvmrc 一致（否则 Vite/TS 行为不可信），Go 由 go.mod 的 toolchain 保证。

set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$ROOT"

# ---------------------------------------------------------------- 前置检查
if ! command -v node >/dev/null 2>&1 || ! command -v npm >/dev/null 2>&1; then
  echo "错误: 未找到 node/npm；构建前端需要 Node（版本见 .nvmrc）" >&2
  exit 2
fi
if ! command -v go >/dev/null 2>&1; then
  echo "错误: 未找到 go；需要 go.mod 声明的工具链版本" >&2
  exit 2
fi

EXPECTED_NODE=$(tr -d ' \r\n' < .nvmrc)
ACTUAL_NODE=$(node -v | tr -d '\r')
if [ "v$EXPECTED_NODE" != "$ACTUAL_NODE" ]; then
  echo "错误: Node 版本不符，期望 v$EXPECTED_NODE，实际 $ACTUAL_NODE（先把对应版本前置到 PATH）" >&2
  exit 2
fi

if [ ! -d web/node_modules ]; then
  echo "错误: web/node_modules 不存在，先执行 npm --prefix web ci" >&2
  exit 2
fi

VERSION=${VERSION:-0.0.0-dev}
COMMIT=${COMMIT:-unknown}
BUILD_DATE=${BUILD_DATE:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}
LDFLAGS="-X upanel/internal/version.Version=$VERSION -X upanel/internal/version.Commit=$COMMIT -X upanel/internal/version.BuildDate=$BUILD_DATE"

# ---------------------------------------------------------------- 1. 前端
echo "==> 构建前端（Node $ACTUAL_NODE）"
npm --prefix web run build

# ---------------------------------------------------------------- 2. 同步到嵌入目录
echo "==> 同步前端产物到 internal/webui/dist"
rm -rf internal/webui/dist
mkdir -p internal/webui/dist
cp -R web/dist/. internal/webui/dist/
# 占位文件保留在仓库里（.gitignore 只放行它），避免下次 go build 时 go:embed 找不到目录。
printf '占位文件：真正的前端产物由 scripts/build.sh 同步，不入库。\n' > internal/webui/dist/.keep

if [ ! -s internal/webui/dist/index.html ]; then
  echo "错误: 同步后没有 index.html，前端构建产物不完整" >&2
  exit 1
fi

# ---------------------------------------------------------------- 3. Go 二进制
echo "==> 构建 Go 二进制（版本 $VERSION，commit $COMMIT）"
mkdir -p bin
# -buildvcs=false：否则 Go 会把工作树的 VCS 状态（vcs.revision / vcs.modified）写进
# 二进制，于是同一份源码在"脏工作树"与"干净工作树"下构建出的字节不同——P12.1 想把
# 验收用的二进制逐字节复现时撞到了这一条。版本号本来就由 ldflags 注入，这里不缺信息。
go build -buildvcs=false -trimpath -ldflags "$LDFLAGS" -o bin/upanel-web ./cmd/upanel-web
go build -buildvcs=false -trimpath -ldflags "$LDFLAGS" -o bin/upanel-agent ./cmd/upanel-agent
go build -buildvcs=false -trimpath -ldflags "$LDFLAGS" -o bin/upanelctl ./cmd/upanelctl

echo "==> 完成"
ls -l bin
echo "嵌入的前端入口: internal/webui/dist/index.html ($(wc -c < internal/webui/dist/index.html) 字节)"
