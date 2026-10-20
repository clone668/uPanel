// 登录后的跳转目标。
//
// `?redirect=` 是用户输入，必须当成不可信数据处理：只接受站内绝对路径，
// 拒绝协议相对地址（`//evil.example`）和登录页自身（否则登录成功又回到登录页）。

export function safeRedirect(value: unknown): string {
  if (typeof value !== 'string') return '/'
  if (!value.startsWith('/')) return '/'
  if (value.startsWith('//') || value.startsWith('/\\')) return '/'
  if (value.startsWith('/login')) return '/'
  return value
}
