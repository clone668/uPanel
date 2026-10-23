#!/bin/sh
# uPanel 0.1 发布包打包脚本（P10.4）。
#
# 产出（默认两个架构）：
#   dist/upanel-<version>-linux-amd64.tar.gz
#   dist/upanel-<version>-linux-arm64.tar.gz
#   dist/SHA256SUMS                  ← 两个包的校验和，随包一起发给用户
# 每个包里还有一份自己的 SHA256SUMS，覆盖包内每个文件——用户解开后可逐文件核对。
#
# 设计约束（PLAN.md 第 20 节）：
#   - 发布包必须带校验和，不能让用户 `curl | bash`；
#   - 包里放的是**可审阅的脚本**（install.sh / uninstall.sh），不是一行安装器；
#   - 交叉编译 CGO_ENABLED=0：Ubuntu 上不依赖宿主 glibc 版本，22.04 与 24.04 同一个包。
#
# 用法：
#   sh scripts/release.sh                       # 版本 0.0.0-<git short>，两个架构
#   VERSION=0.1.0 ARCHES=amd64 sh scripts/release.sh
#
# 前置：先跑 scripts/build.sh（它会构建前端并同步到 internal/webui/dist，
# go:embed 在编译期读取该目录，跳过这一步打出来的包没有前端）。

set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$ROOT"

if ! command -v go >/dev/null 2>&1; then
  echo "错误: 未找到 go" >&2
  exit 2
fi
if [ ! -s internal/webui/dist/index.html ]; then
  echo "错误: internal/webui/dist/index.html 不存在——先执行 sh scripts/build.sh 构建前端" >&2
  echo "      （发布包里的前端是编译进二进制的，没有它打出来的包打开是 503）" >&2
  exit 2
fi

SHA256=""
if command -v sha256sum >/dev/null 2>&1; then
  SHA256="sha256sum"
elif command -v shasum >/dev/null 2>&1; then
  SHA256="shasum -a 256"
else
  echo "错误: 找不到 sha256sum/shasum，无法生成校验和" >&2
  exit 2
fi

VERSION=${VERSION:-0.0.0-$(git rev-parse --short HEAD 2>/dev/null || echo unknown)}
COMMIT=${COMMIT:-$(git rev-parse --short HEAD 2>/dev/null || echo unknown)}
BUILD_DATE=${BUILD_DATE:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}
ARCHES=${ARCHES:-amd64 arm64}
LDFLAGS="-X upanel/internal/version.Version=$VERSION -X upanel/internal/version.Commit=$COMMIT -X upanel/internal/version.BuildDate=$BUILD_DATE"

OUT=dist
rm -rf "$OUT/upanel-$VERSION-linux-amd64" "$OUT/upanel-$VERSION-linux-arm64"
mkdir -p "$OUT"

for ARCH in $ARCHES; do
  NAME="upanel-$VERSION-linux-$ARCH"
  STAGE="$OUT/$NAME"
  echo "==> 打包 $NAME"
  mkdir -p "$STAGE/bin" "$STAGE/deploy/systemd" "$STAGE/deploy/scripts" "$STAGE/docs"

  # 1) 三个二进制。upanel-web 把前端一起带走了，所以生产机不需要 Node.js。
  echo "    交叉编译（GOOS=linux GOARCH=$ARCH CGO_ENABLED=0）"
  CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" go build -trimpath -ldflags "$LDFLAGS" \
    -o "$STAGE/bin/upanel-web" ./cmd/upanel-web
  CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" go build -trimpath -ldflags "$LDFLAGS" \
    -o "$STAGE/bin/upanel-agent" ./cmd/upanel-agent
  CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" go build -trimpath -ldflags "$LDFLAGS" \
    -o "$STAGE/bin/upanelctl" ./cmd/upanelctl
  # 打包机上可能是 Windows（没有 POSIX 权限位），tar 会把权限记成 0644：
  # 那样的包解到 Ubuntu 上每个可执行文件都指向"Permission denied"，而
  # 校验和却全是 OK —— 这正是 P10.4 验收第一次跑出来的问题。权限位是交付物的一部分，
  # 所以在这里显式给，不指望文件系统替我们记住。
  chmod 0755 "$STAGE/bin/upanel-web" "$STAGE/bin/upanel-agent" "$STAGE/bin/upanelctl"

  # 2) 单元、配置样例与脚本（可审阅：用户先看再跑）。
  cp deploy/systemd/upanel-agent.service deploy/systemd/upanel-web.service "$STAGE/deploy/systemd/"
  cp deploy/upanel.example.json "$STAGE/deploy/"
  cp deploy/scripts/install.sh deploy/scripts/uninstall.sh "$STAGE/deploy/scripts/"
  chmod 0755 "$STAGE/deploy/scripts/install.sh" "$STAGE/deploy/scripts/uninstall.sh"

  # 3) 文档：装完之后机器上要有同一份，不能在面板不可用时无处可查。
  #    顺序不重要，但 STORAGE_RECOVERY.md 必须带上——RECOVERY.md 里直接引用它。
  for doc in INSTALL.md ACCESS.md RECOVERY.md STORAGE_RECOVERY.md SHARES_CLIENT.md LOCAL_START.md; do
    if [ -f "docs/$doc" ]; then
      cp "docs/$doc" "$STAGE/docs/"
    fi
  done
  cp README.md "$STAGE/"

  printf '%s\n' "$VERSION" > "$STAGE/VERSION"
  printf 'commit: %s\nbuild_date: %s\narch: %s\n' "$COMMIT" "$BUILD_DATE" "$ARCH" >> "$STAGE/VERSION"

  # 4) 包内校验和：解开之后可以先 `sha256sum -c SHA256SUMS` 再动手。
  ( cd "$STAGE" && find . -type f ! -name SHA256SUMS | LC_ALL=C sort | while read -r f; do
      echo "$($SHA256 "$f" | cut -d' ' -f1)  ${f#./}"
    done > SHA256SUMS )

  # 打包：权限位显式写进归档，不依赖打包机的文件系统。
  #
  # P10.4 验收第一次跑出来的是：包是好的、校验和是好的，解到 Ubuntu 上三个二进制
  # 全是 0644 —— 安装器第一句人话就是"跑不起来"。原因是打包机是 Windows，chmod 对
  # 没有扩展名的文件不生效，tar 于是按 0644 记录。所以这里让 tar 自己写模式：
  # 先按“文件 0644 / 目录 0755”收全部内容，再用 0755 追加那 5 个可执行文件。
  # 每个成员只出现一次，解压出来就是它该有的样子。
  TAR="$OUT/$NAME.tar"
  rm -f "$TAR" "$TAR.gz"
  tar cf "$TAR" --mode='u=rwX,go=rX' --owner=0 --group=0 --numeric-owner \
    -C "$OUT" --exclude="$NAME/bin/*" --exclude="$NAME/deploy/scripts/*" "$NAME"
  tar rf "$TAR" --mode=0755 --owner=0 --group=0 --numeric-owner -C "$OUT" \
    "$NAME/bin/upanel-web" "$NAME/bin/upanel-agent" "$NAME/bin/upanelctl" \
    "$NAME/deploy/scripts/install.sh" "$NAME/deploy/scripts/uninstall.sh"
  gzip -9 -f "$TAR"
  # 自检：包里的可执行文件必须带执行位。在上一次验收里，包是好的、校验和是好的，
  # 但解到 Ubuntu 上每个二进制都是 0644 —— 安装器的第一句人话就是"跑不起来"。
  # 这一条在打包机上就能拦住它。
  BAD=$(tar tzvf "$OUT/$NAME.tar.gz" | awk '$1 ~ /^-/ && $1 !~ /x/ {print $NF}' |
    grep -E '/(bin/upanel-(web|agent|ctl)|deploy/scripts/(install|uninstall)\.sh)$' | tr '\n' ' ')
  if [ -n "$BAD" ]; then
    printf '错误：包内这些文件没有执行权限：%s\n' "$BAD" >&2
    exit 1
  fi
  echo "    自检: 包内可执行文件均带执行位"
  echo "    完成: $OUT/$NAME.tar.gz ($(wc -c < "$OUT/$NAME.tar.gz" | tr -d ' ') 字节)"
done

# 5) 发布级校验和：用户核对的是这两个包。
( cd "$OUT" && find . -maxdepth 1 -name "upanel-$VERSION-linux-*.tar.gz" | LC_ALL=C sort | while read -r f; do
    echo "$($SHA256 "$f" | cut -d' ' -f1)  ${f#./}"
  done > SHA256SUMS )

echo
echo "==> 发布包已就绪"
cat "$OUT/SHA256SUMS"
echo
echo "发出去的时候把 tar.gz 与 SHA256SUMS 一起给用户；安装说明见 docs/INSTALL.md："
echo "  sha256sum -c SHA256SUMS"
echo "  tar xzf upanel-$VERSION-linux-amd64.tar.gz"
echo "  sudo sh upanel-$VERSION-linux-amd64/deploy/scripts/install.sh --source upanel-$VERSION-linux-amd64"
