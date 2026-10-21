// 把 ApiError 翻译成用户能读懂的说明。
//
// 原则：
//   - 按 code 分支，不按 message 文本匹配（message 会变，code 不变）；
//   - 不把客户端传输故障说成服务端故障；
//   - 不承诺做不到的动作（没有登录页之前，401 只说“需要重新登录”，不放假链接）；
//   - 未知码不隐藏，原样展示，方便对照 DECISIONS/契约。

import type { ApiError } from './client'

export interface ErrorPresentation {
  title: string
  /** 给用户看的下一步建议。 */
  hint: string
  /** 是否值得原样重试（写操作不在此列：重试要用户重新确认）。 */
  canRetry: boolean
  /** 是否需要重新登录。 */
  needsLogin: boolean
}

const SERVER_HINTS: Record<string, ErrorPresentation> = {
  UNAUTHENTICATED: {
    title: '会话已失效',
    hint: '请重新登录后再操作。',
    canRetry: false,
    needsLogin: true,
  },
  FORBIDDEN: { title: '权限不足', hint: '当前账号无权执行该操作。', canRetry: false, needsLogin: false },
  REAUTH_REQUIRED: {
    title: '需要再次验证身份',
    hint: '该操作需要重新输入密码。',
    canRetry: false,
    needsLogin: false,
  },
  CSRF_FAILED: {
    title: '请求被安全校验拒绝',
    hint: '刷新页面后重试；反复出现说明会话异常。',
    canRetry: true,
    needsLogin: false,
  },
  NOT_FOUND: { title: '对象不存在', hint: '它可能已被删除或被移动。', canRetry: false, needsLogin: false },
  VERSION_CONFLICT: {
    title: '数据已被修改',
    hint: '页面数据不是最新的，请刷新后重新操作。',
    canRetry: false,
    needsLogin: false,
  },
  RESOURCE_BUSY: {
    title: '资源正被占用',
    hint: '有其他操作正在进行，稍后再试。',
    canRetry: true,
    needsLogin: false,
  },
  DEPENDENCY_MISSING: {
    title: '依赖未安装',
    hint: '该功能依赖的软件尚未安装或启用。',
    canRetry: false,
    needsLogin: false,
  },
  AGENT_UNAVAILABLE: {
    title: '特权代理不可用',
    hint: '系统操作暂时无法执行，面板的只读功能不受影响。',
    canRetry: true,
    needsLogin: false,
  },
  NOT_IMPLEMENTED: {
    title: '功能尚未实现',
    hint: '该能力在后续阶段提供，当前不会执行任何操作。',
    canRetry: false,
    needsLogin: false,
  },
  PATH_OUTSIDE_ROOT: {
    title: '路径越界',
    hint: '面板只访问已登记文件根内的普通路径：不跟随符号链接、不跨挂载点、不接受绝对路径与 ".."。',
    canRetry: false,
    needsLogin: false,
  },
  ROOT_UNAVAILABLE: {
    title: '文件根不可用',
    hint: '根目录不存在、身份变了，或它所在的挂载已不在。检查挂载后重新载入代理配置。',
    canRetry: true,
    needsLogin: false,
  },
  FILE_NOT_TEXT: {
    title: '不是文本文件',
    hint: '这个文件不是 UTF-8 文本（含 NUL 字节也算），请改用下载查看原始内容。',
    canRetry: false,
    needsLogin: false,
  },
  VALIDATION_ERROR: {
    title: '参数不合法',
    hint: '请检查输入内容后重试。',
    canRetry: false,
    needsLogin: false,
  },
}

export function describeApiError(error: ApiError): ErrorPresentation {
  if (error.source === 'client') {
    switch (error.code) {
      case 'ABORTED':
        return { title: '请求已取消', hint: '操作被主动取消，没有产生任何变更。', canRetry: true, needsLogin: false }
      case 'NETWORK_ERROR':
        return {
          title: '无法连接面板服务',
          hint: '请确认面板服务正在运行、网络正常，然后重试。',
          canRetry: true,
          needsLogin: false,
        }
      default:
        return {
          title: '服务响应异常',
          hint: '响应不符合接口契约，刷新后重试；持续出现请查看服务端日志。',
          canRetry: true,
          needsLogin: false,
        }
    }
  }

  const known = SERVER_HINTS[error.code]
  if (known) return known

  return {
    title: '请求失败',
    hint: '服务端返回了未在界面登记的失败原因，请把错误码与 request_id 一并反馈。',
    canRetry: error.status >= 500,
    needsLogin: false,
  }
}
