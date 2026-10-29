# 收尾宿主检查（2026-10-28，Q514/Q515 与文档更正落定之后）

## 读数

```
PASS 15 / FAIL 0 / NOT RUN 0
明细: line-endings:PASS sh-syntax:PASS go-build:PASS go-vet:PASS go-test:PASS
      go-build-linux:PASS go-vet-linux:PASS gofmt:PASS node-version:PASS
      web-typecheck:PASS web-lint:PASS web-test:PASS web-build:PASS
      web-smoke:PASS openapi-lint:PASS
```

完整输出：`check-sh.log`。

## 跑它的环境（这一步不写清楚就会得到一条假红）

工具链必须钉在 `.nvmrc` / `go.mod` 要的版本上，并且**放在 PATH 前面**：

```sh
PATH=/h/Code/.toolchains/node-v22.23.3-win-x64:/h/Code/.toolchains/go/bin:$PATH sh scripts/check.sh
```

同一天先跑过一次没前置的：`PASS 14 / FAIL 1`，红的是 `node-version`——`node -v` 拿到系统那个
`v24.15.0`，而 `.nvmrc` 要 `22.23.3`。那条是环境，不是代码；但**工具链不在 PATH 里也不能当通过**，
所以这一项要么绿灯要么如实记红，不折算。

## 它证明了什么、没证明什么

- 证明：换行符、`sh -n`、Go 构建/vet/测试/交叉编译/gofmt、前端 typecheck/lint/测试/构建/页面冒烟
  （production 10 项）、OpenAPI lint 在这一份工作树上全绿。
- 没证明：任何**真机**行为。真机读数在 `P16/2026-10-28-p16.2-final/`（22.04）与
  `P16/2026-10-28-p16.2-2404-final/`（24.04），两份都跑在同一份交付字节 `3f3b40c1…daf0` 上。
