/// <reference types="vite/client" />

interface ImportMetaEnv {
  /** 前端运行模式来源之一（开发与测试用）。production / mock，缺省按 production。 */
  readonly VITE_UPANEL_MODE?: string
  /** 覆盖 API 前缀，仅用于特殊情况；默认同源 /api/v1。 */
  readonly VITE_UPANEL_API_BASE?: string
}

interface ImportMeta {
  readonly env: ImportMetaEnv
}

interface Window {
  /**
   * 服务端注入的运行模式（P01.4 嵌入前端时写进 index.html）。
   * 只接受 'production' / 'mock'，其他值一律忽略。
   */
  __UPANEL_MODE__?: string
}
