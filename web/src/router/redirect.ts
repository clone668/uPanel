// 登录后的跳转目标。
//
// `?redirect=` 是用户输入，必须当成不可信数据处理：只接受站内绝对路径，
// 拒绝协议相对地址（`//evil.example`）和登录页自身（否则登录成功又回到登录页）。
//
// 兜底目标是**桌面**（/desk），不是概览页：桌面式外壳下，登录完应该落在
// 一张摆着应用的桌上，而不是被直接推进某一页数据里（Q533）。

export const HOME_PATH = '/desk'

export function safeRedirect(value: unknown): string {
  if (typeof value !== 'string') return HOME_PATH
  if (!value.startsWith('/')) return HOME_PATH
  if (value.startsWith('//') || value.startsWith('/\\')) return HOME_PATH
  if (value.startsWith('/login')) return HOME_PATH
  return value
}
