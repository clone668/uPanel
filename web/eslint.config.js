// 前端 lint 配置（ESLint 10 flat config）。
//
// 范围说明：依赖清单里没有 eslint-plugin-vue，所以 .vue 文件不经过 ESLint；
// SFC 的类型与模板错误由 vue-tsc（npm run typecheck）覆盖，
// 组件渲染分支由 vitest 的 SSR 断言覆盖。见 docs/DECISIONS.md Q21。
//
// 不添加计划外依赖：配置只用 package.json 已冻结的 eslint 与 typescript-eslint。

import tseslint from 'typescript-eslint'

export default tseslint.config(
  { ignores: ['dist/**', 'dist-mock/**', 'node_modules/**', 'coverage/**', 'test-results/**'] },
  tseslint.configs.recommended,
  {
    files: ['src/**/*.ts', '*.config.ts', 'env.d.ts'],
    rules: {
      '@typescript-eslint/consistent-type-imports': ['error', { prefer: 'type-imports' }],
      'no-console': ['error', { allow: ['warn', 'error'] }],
    },
  },
)
