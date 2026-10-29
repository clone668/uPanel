// 按钮的“按不按得动”是一条纯规则，抽出来才测得到。
//
// 为什么不在 SFC 里直接写 disabled || loading：测试环境是 node（没有 DOM，
// 见 vitest.config.ts 与 DECISIONS Q21），点击行为测不了；把判断抽成纯函数，
// 至少让“灰着的按钮不许提交”这条规则有断言，组件只负责把它接到 DOM 上。

export function isInert(disabled = false, loading = false): boolean {
  return disabled || loading
}
