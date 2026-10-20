// 该文件不是 Go 模块的构建目标。
//
// 存在原因：npm 依赖中可能夹带 .go 源码（例如 flatted 的 golang 实现），
// 若 web/ 不是独立模块，根模块的 `go build ./...` 会递归扫到 web/node_modules，
// 把第三方包当作本项目代码编译和测试。
//
// 在 Go 的规则里，位于子目录中的嵌套模块不会被执行 `./...` 的父模块匹配，
// 因此这个边界标记能让根模块只包含 cmd/、internal/ 等真实代码。
// 前端依赖与构建完全由 web/package.json 与 package-lock.json 管理，与此文件无关。
module upanel/web

go 1.27.3
