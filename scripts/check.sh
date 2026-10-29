#!/bin/sh
# uPanel 统一检查脚本。
#
# 原则：
#   - 只运行当前仓库真实具备的检查；不具备的显式标为 NOT RUN 并给出原因，绝不把未执行当通过。
#   - 任何 FAIL 都以非零退出码结束。
#   - 不在脚本里安装依赖、不修改源码、不删除测试。
#
# 用法：
#   sh scripts/check.sh
#
# 可选环境变量：
#   PATH 前缀需包含锁定的 Go/Node 工具链；Node 精确版本见 .nvmrc。

set -u

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$ROOT" || exit 2

RAN=0
SKIPPED=0
FAILED=0
LABELS=""

label() { LABELS="$LABELS $1"; }

run() {
  name=$1
  shift
  printf '\n==> %s\n' "$name"
  if "$@"; then
    RAN=$((RAN + 1))
    printf '<== PASS  %s\n' "$name"
    label "$name:PASS"
  else
    rc=$?
    FAILED=$((FAILED + 1))
    printf '<== FAIL  %s (exit %s)\n' "$name" "$rc"
    label "$name:FAIL"
  fi
}

skip() {
  name=$1
  reason=$2
  printf '\n==> %s\n' "$name"
  SKIPPED=$((SKIPPED + 1))
  printf '<== NOT RUN  %s: %s\n' "$name" "$reason"
  label "$name:NOT_RUN"
}

info() {
  printf '\n--- %s\n' "$1"
}

# ---------------------------------------------------------------- 仓库结构
info "仓库结构（P00 交付物）"
for f in go.mod .nvmrc .node-version docs/DECISIONS.md docs/DEPENDENCIES.md \
         docs/TEST_ENVIRONMENT.md api/openapi.yaml scripts/check.sh web/package-lock.json; do
  if [ -e "$f" ]; then
    printf '  present  %s\n' "$f"
  else
    printf '  MISSING  %s\n' "$f"
    FAILED=$((FAILED + 1))
    label "structure:$f:MISSING"
  fi
done

# ---------------------------------------------------------------- 换行符
# 为什么单独一关：`.gitattributes` 要求 *.sh / *.sql / *.service 是 LF，但宿主是
# Windows，编辑器与工具会悄悄写出 CRLF。带 CRLF 的 shell 脚本推到 Linux 上是
# "`set: Illegal option -`"，带 CRLF 的迁移文件会让已经建好的库校验和失配——这两种
# 都在真机验收里烧掉过一整轮。gofmt 管得住 .go，管不住这三种，所以在这里拦。
#
# 用 `tr -d` 比字节数，不用 `grep '\r'`：Git Bash 的 grep 把 CRLF 当行尾，会漏报。
check_line_endings() {
  if ! command -v git >/dev/null 2>&1; then
    # 找不到 git 就说找不到，不能因为“没文件可查”而假装通过。
    printf '  找不到 git，无法枚举脚本与迁移文件\n'
    return 1
  fi
  bad=""
  files=$(git ls-files --cached --others --exclude-standard -- '*.sh' '*.sql' '*.service' 2>/dev/null)
  if [ -z "$files" ]; then
    printf '  没有需要检查的脚本与迁移文件\n'
    return 0
  fi
  for f in $files; do
    [ -f "$f" ] || continue
    if [ "$(tr -d '\r' <"$f" | wc -c)" != "$(wc -c <"$f")" ]; then
      printf '  CRLF  %s\n' "$f"
      bad="$bad $f"
    fi
  done
  if [ -n "$bad" ]; then
    printf '以上文件带 CRLF，在 Linux 上会直接跑不起来或有校验和风险：用 `sed -i '\''s/\\r$//'\''` 改成 LF\n'
    return 1
  fi
  printf '  %s 个脚本/迁移文件都是 LF\n' "$(printf '%s\n' "$files" | wc -l | tr -d ' ')"
  return 0
}

run "line-endings" check_line_endings

# ---------------------------------------------------------------- 脚本语法
# 为什么单独一关：部署脚本与验收脚本在 Windows 宿主上**一次都跑不起来**（没有 systemd、
# 没有 /etc/upanel、没有 root）。它们唯一的宿主侧证据就是“解析得动”——而一个多打的
# `fi`、一处没收尾的引号，在真机上烧掉的是一整轮验收。所以至少把这一层拦住。
#
# 诚实边界：宿主是 Windows 时 `sh` 是 Git Bash（bash），能抓语法错误，抓不到 bashism；
# 在 Ubuntu 上 /bin/sh 是 dash，同一个 -n 才等价于真机解析。这一点写进输出，不假装等价。
check_sh_syntax() {
  if ! command -v sh >/dev/null 2>&1; then
    printf '  找不到 sh，无法检查脚本语法\n'
    return 1
  fi
  files=$(git ls-files --cached --others --exclude-standard -- \
    'scripts/*.sh' 'deploy/scripts/*.sh' 'tests/integration/*.sh' 2>/dev/null)
  if [ -z "$files" ]; then
    printf '  没有需要检查的 shell 脚本\n'
    return 0
  fi
  printf '  解析器: %s\n' "$(command -v sh)"
  case "$(uname -s 2>/dev/null)" in
    MINGW*|MSYS*|CYGWIN*) printf '  注意: 宿主是 Windows，这里的 sh 是 bash（抓不到 bashism）\n' ;;
    *) : ;;
  esac
  bad=""
  n=0
  for f in $files; do
    [ -f "$f" ] || continue
    n=$((n + 1))
    if ! sh -n "$f" 2>&1; then
      bad="$bad $f"
    fi
  done
  if [ -n "$bad" ]; then
    printf '以上脚本解析失败（上面是 sh -n 的输出）：%s\n' "$bad"
    return 1
  fi
  printf '  %d 个脚本解析通过\n' "$n"
  return 0
}

run "sh-syntax" check_sh_syntax

# ---------------------------------------------------------------- Go
info "Go 工具链"
PINNED_GO=$(sed -n 's/^go \([0-9][0-9.]*\).*/\1/p' go.mod 2>/dev/null | head -n 1)
printf '  go.mod 约束: %s\n' "${PINNED_GO:-未读取到}"

GO_FILE_COUNT=0
if [ -d cmd ] || [ -d internal ]; then
  GO_FILE_COUNT=$(find cmd internal -name '*.go' 2>/dev/null | wc -l | tr -d ' ')
fi

if ! command -v go >/dev/null 2>&1; then
  printf '  本机 go 版本: 未安装\n'
  skip "go-build" "未找到 go 可执行文件"
  skip "go-vet" "未找到 go 可执行文件"
  skip "go-test" "未找到 go 可执行文件"
  skip "go-build-linux" "未找到 go 可执行文件"
  skip "go-vet-linux" "未找到 go 可执行文件"
  skip "gofmt" "未找到 go 可执行文件"
else
  printf '  本机 go 版本: %s\n' "$(go version 2>&1 | tr -d '\r')"
  if [ "$GO_FILE_COUNT" -eq 0 ]; then
    skip "go-build" "仓库尚无 Go 源码（P01.1 建立入口后启用）"
    skip "go-vet" "仓库尚无 Go 源码（P01.1 建立入口后启用）"
    skip "go-test" "仓库尚无 Go 源码（P01.1 建立入口后启用）"
    skip "go-build-linux" "仓库尚无 Go 源码（P01.1 建立入口后启用）"
    skip "go-vet-linux" "仓库尚无 Go 源码（P01.1 建立入口后启用）"
    skip "gofmt" "仓库尚无 Go 源码（P01.1 建立入口后启用）"
  else
    run "go-build" go build ./...
    run "go-vet" go vet ./...
    run "go-test" go test ./...
    # Linux 目标交叉编译：确保带 //go:build linux 的实现与本机实现都能编译。
    # 开发宿主不是 Linux，这一项是“Linux 实现可编译”的最低证据，不等于 Linux 运行验收。
    run "go-build-linux" env GOOS=linux GOARCH=amd64 go build ./...
    run "go-vet-linux" env GOOS=linux GOARCH=amd64 go vet ./...
    if [ -n "$(gofmt -l cmd internal 2>/dev/null)" ]; then
      printf '\n==> gofmt\n未格式化的文件：\n%s\n' "$(gofmt -l cmd internal)"
      FAILED=$((FAILED + 1))
      label "gofmt:FAIL"
    else
      RAN=$((RAN + 1))
      printf '\n==> gofmt\n<== PASS  gofmt\n'
      label "gofmt:PASS"
    fi
  fi
fi

# ---------------------------------------------------------------- 前端
info "Node 工具链"
EXPECTED_NODE=$(tr -d ' \r\n' < .nvmrc 2>/dev/null)
printf '  .nvmrc 约束: %s\n' "${EXPECTED_NODE:-未读取到}"

if ! command -v node >/dev/null 2>&1; then
  printf '  本机 node: 未安装\n'
  for s in node-version web-typecheck web-lint web-test web-build openapi-lint; do
    skip "$s" "未找到 node 可执行文件"
  done
else
  ACTUAL_NODE=$(node -v | tr -d '\r')
  printf '  本机 node 版本: %s\n' "$ACTUAL_NODE"
  if [ "v$EXPECTED_NODE" = "$ACTUAL_NODE" ]; then
    RAN=$((RAN + 1))
    printf '\n==> node-version\n<== PASS  node-version\n'
    label "node-version:PASS"
  else
    printf '\n==> node-version\n'
    printf '<== FAIL  node-version: 期望 v%s，实际 %s（请先按 .nvmrc 切换工具链）\n' \
      "$EXPECTED_NODE" "$ACTUAL_NODE"
    FAILED=$((FAILED + 1))
    label "node-version:FAIL"
  fi

  SCRIPT_KEYS=""
  if [ -f web/package.json ]; then
    SCRIPT_KEYS=$(node -e 'const p=require("./web/package.json");process.stdout.write(Object.keys(p.scripts||{}).join(" "))' 2>/dev/null)
  fi

  has_script() {
    for k in $SCRIPT_KEYS; do
      [ "$k" = "$1" ] && return 0
    done
    return 1
  }

  if [ ! -d web/node_modules ]; then
    skip "web-typecheck" "web/node_modules 不存在，先运行 npm --prefix web ci"
    skip "web-lint" "web/node_modules 不存在，先运行 npm --prefix web ci"
    skip "web-test" "web/node_modules 不存在，先运行 npm --prefix web ci"
    skip "web-build" "web/node_modules 不存在，先运行 npm --prefix web ci"
  else
    if has_script typecheck; then
      run "web-typecheck" npm --prefix web run --silent typecheck
    else
      skip "web-typecheck" "package.json 尚无 typecheck 脚本（P01.3 建立）"
    fi
    if has_script lint; then
      run "web-lint" npm --prefix web run --silent lint
    else
      skip "web-lint" "package.json 尚无 lint 脚本（P01.3 建立）"
    fi
    if has_script test; then
      run "web-test" npm --prefix web run --silent test
    else
      skip "web-test" "package.json 尚无 test 脚本（P01.3 建立）"
    fi
    if has_script build; then
      run "web-build" npm --prefix web run --silent build
    else
      skip "web-build" "package.json 尚无 build 脚本（P01.3 建立）"
    fi
    # 页面冒烟：preview 托管刚构建的 web/dist，用 Playwright 走一遍“未登录必须落到登录页”。
    # 需要 Playwright 的浏览器二进制（npx playwright install chromium）；没有就如实标 NOT RUN。
    if has_script smoke; then
      PLAYWRIGHT_BROWSERS="${PLAYWRIGHT_BROWSERS_PATH:-}"
      if [ -z "$PLAYWRIGHT_BROWSERS" ]; then
        case "$(uname -s 2>/dev/null)" in
          MINGW*|MSYS*|CYGWIN*) PLAYWRIGHT_BROWSERS="${LOCALAPPDATA:-$HOME/AppData/Local}/ms-playwright" ;;
          *) PLAYWRIGHT_BROWSERS="$HOME/.cache/ms-playwright" ;;
        esac
      fi
      if [ -d "$PLAYWRIGHT_BROWSERS" ]; then
        run "web-smoke" npm --prefix web run --silent smoke
      else
        skip "web-smoke" "未找到 Playwright 浏览器（$PLAYWRIGHT_BROWSERS），先运行 npx --prefix web playwright install chromium"
      fi
    else
      skip "web-smoke" "package.json 尚无 smoke 脚本（P01.3 建立）"
    fi
  fi

  if [ -x web/node_modules/.bin/redocly ]; then
    run "openapi-lint" web/node_modules/.bin/redocly lint --format=summary api/openapi.yaml
  else
    skip "openapi-lint" "web/node_modules/.bin/redocly 不存在，先运行 npm --prefix web ci"
  fi
fi

# ---------------------------------------------------------------- 汇总
printf '\n=========================== 汇总 ===========================\n'
printf 'PASS %s / FAIL %s / NOT RUN %s\n' "$RAN" "$FAILED" "$SKIPPED"
printf '明细:%s\n' "$LABELS"
printf 'NOT RUN 不等于通过；发布前必须按 docs/STAGES.md 补齐对应证据。\n'
printf '============================================================\n'

[ "$FAILED" -eq 0 ] || exit 1
exit 0
