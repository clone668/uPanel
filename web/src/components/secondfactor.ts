// 二次验证输入的小工具（P14.6）。
//
// 单独放在这里而不是写进 .vue：这些是纯函数，能被 vitest 直接断言，
// 不需要为了测"会不会把字母当成验证码发上去"去渲染一个组件。

/** 只留数字，最多六位。空串表示"这次没带码"。 */
export function sanitizeCode(raw: string): string {
  return raw.replace(/\D+/g, '').slice(0, 6)
}

/**
 * 判断这串输入像不像一个能发出去的验证码。
 *
 * 要求恰好六位：这不是替服务端做判据（那边会拒绝长度不对的码），而是让界面
 * 在明显没输完的时候就不去发请求——每发一次错码都会计入登录限速，
 * 而限速是共享的：猜验证码的人和输不完的人不该在同一个计数器上。
 */
export function isCodeComplete(raw: string): boolean {
  return /^\d{6}$/.test(raw)
}

/** 恢复码的形状：两组五字符，中间一个连字符（例如 `3F7K9-QM2XP`）。 */
export function isRecoveryCodeShape(raw: string): boolean {
  return /^[A-Za-z0-9]{5}-?[A-Za-z0-9]{5}$/.test(raw.trim())
}
