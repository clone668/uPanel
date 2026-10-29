// 共享的 DTO、状态翻译与“客户端怎么连”的说明生成（P08.4）。
//
// 与 api/openapi.yaml 的 ShareCapability / ShareDefinition / ShareRecord /
// ShareAccount / ShareConnections 逐字对应，与 docs/CONTRACTS.md 第 7 节一起看。
//
// 这个模块只做三件事：取数据、把状态翻译成人话、拼客户端连接说明。判断“能不能做”不在
// 这里：动作按钮一律先要一份预检，由预检的 blockers 说话——共享能不能应用取决于卷此刻
// 挂没挂、路径有没有符号链接、账号有没有被禁用，那是现场的事，不是一份 DTO 能替它回答的。
//
// 三个状态必须分开显示，这是 CONTRACTS §7.1 的要求，页面上也照办：
//   config_hash        这份定义渲染出来的是哪一版（sha256）
//   directory_prepared 目录与 POSIX ACL 是否已经按这份定义准备**并核验**过
//   config_applied     这份定义是否已经写进 Samba 配置并被重载
// 把三者合成一个“已生效”的绿灯，就是把“我们写了”当成“它能用”。

import { get, request, type ApiResult } from './client'

// ---------------------------------------------------------------- 阻塞项与告警

/** 代理侧的阻塞/告警条目。message 是人读的，code 是闭集、测试按它断言。 */
export interface ShareNotice {
  code: string
  message: string
}

/**
 * 已知的共享阻塞码与告警码 → 中文短名。
 *
 * 页面**不**用这张表替代 message：message 是代理按现场写的（带着卷名、路径、账号名），
 * 这里只给列表里一个短标签。表里没有的码原样显示 code——未知的码不该被静默吞掉，
 * 那会让“面板不认识这个判决”看起来像“没有判决”。
 */
export const SHARE_NOTICE_LABELS: Record<string, string> = {
  SHARE_NOT_FOUND: '共享不存在',
  SHARE_VERSION_MISMATCH: '版本已变',
  SHARE_NAME_TAKEN: '名字被占用',
  VOLUME_NOT_FOUND: '卷未登记',
  VOLUME_NOT_MOUNTED: '卷未挂载',
  VOLUME_UUID_MISMATCH: '卷的 UUID 不符',
  SHARE_PATH_INVALID: '路径不合法',
  SHARE_PATH_ESCAPES_VOLUME: '路径越出卷',
  SHARE_PATH_SYMLINK: '路径上有符号链接',
  SHARE_PATH_NOT_DIRECTORY: '目标不是目录',
  SHARE_PATH_NOT_TRAVERSABLE: '上级目录不可穿越',
  SHARE_HOSTS_ALLOW_LOOPBACK: '名单管不住本机',
  SHARE_NO_USERS: '账号名单为空',
  ACCOUNT_MISSING: '账号不存在',
  ACCOUNT_NOT_MANAGED: '账号不是面板创建的',
  ACCOUNT_DISABLED: '账号已禁用',
  ACCOUNT_HAS_SHARES: '账号仍被共享引用',
  ACCOUNT_NAME_INVALID: '账号名不合法',
  ACCOUNT_NAME_TAKEN: '账号名已存在',
  ACCOUNT_PASSWORD_KEPT: '沿用上次的密码',
  ACCOUNT_LOOKUP_TIMEOUT: '查账号超时',
  PROTOCOL_UNSUPPORTED: '协议暂不支持',
  SMB_UNAVAILABLE: '本机 Samba 不可用',
  SECRET_INVALID: '密码引用无效',
  ACL_TOOLS_MISSING: '缺 ACL 工具',
  CONFIG_NOT_READABLE: '读不到 smb.conf',
  INCLUDE_DIR_MISSING: '缺托管目录',
  SHARE_CONFIG_NOT_APPLIED: '配置尚未写入',
  SHARE_VOLUME_STATE_UNKNOWN: '卷状态未知',
  SHARE_VOLUME_OFFLINE: '卷已离线',
  SHARE_RUNTIME_REVOKED: '运行时已撤销',
  SHARE_SERVICE_INACTIVE: 'Samba 没在运行',
  SHARE_DIRECTORY_NOT_EMPTY: '目录里已有文件',
  SHARE_ACL_REPLACED: '原有 ACL 会被替换',
  SHARE_CONFIG_WRITTEN: '托管配置已写入',
  SHARE_CONFIG_UNCHANGED: '托管配置没有变化',
  SHARE_RELOAD_FAILED: '重载失败',
  // ---- NFS（P14.4）----
  NFS_UNAVAILABLE: '本机没有 NFS 导出能力',
  NFS_OPTIONS_INVALID: 'NFS 选项不合法',
  NFS_USERS_NOT_ALLOWED: 'NFS 不接受账号名单',
  SHARE_NO_CIDRS: '没有点名允许网段',
  NFS_OPEN_TO_WORLD: '网段覆盖全部地址',
  EXPORT_PATH_CONFLICT: '路径已被别的导出占用',
  EXPORTS_UNPARSABLE: '别人的导出文件读不懂',
  EXPORTS_UNREADABLE: '读不到实际导出表',
  EXPORTS_NOT_APPLIED: '导出尚未生效',
  NFS_SERVICE_INACTIVE: 'NFS 服务没在运行',
}

/** 取一条条目的短标签；未知码原样返回。 */
export function shareNoticeLabel(code: string): string {
  return SHARE_NOTICE_LABELS[code] ?? code
}

// ---------------------------------------------------------------- 能力

export const SHARE_CAPABILITY_STATUSES = ['available', 'blocked'] as const
export type ShareCapabilityStatus = (typeof SHARE_CAPABILITY_STATUSES)[number]

/**
 * NFS 导出的能力报告（P14.4）。
 *
 * 与上面那份 Samba 报告**彼此独立**：一台机器完全可能“Samba 齐全、但没装
 * nfs-kernel-server”。所以页面不能拿 `capability.status` 去判断能不能建 NFS 共享，
 * 那会把两个互不相干的事实混成一个。
 */
export interface ShareNfsCapability {
  status: ShareCapabilityStatus
  reason?: string
  exportfs?: string
  exports_dir: string
  exports_file: string
  exports_dir_ready: boolean
  exports_present: boolean
  exports_hash?: string
  exported_paths: string[]
  main_exports_readable: boolean
  service_state: 'active' | 'inactive' | 'unknown'
  config_write: 'available' | 'unsupported'
  notices: ShareNotice[]
}

/** NFS 导出能力是否可用（判据只有一条：有没有 exportfs）。 */
export function nfsCapabilityAvailable(cap: ShareCapability | null | undefined): boolean {
  return cap?.nfs?.status === 'available'
}

/** 本机 Samba 的能力报告（只读探针的结论，不是“我们认为应该能怎样”）。 */
export interface ShareCapability {
  status: ShareCapabilityStatus
  reason?: string
  samba_version: string
  tools: Record<string, string>
  config_path: string
  config_readable: boolean
  include_dir: string
  include_ready: boolean
  include_present: boolean
  config_write: 'available' | 'unsupported'
  service_state: 'active' | 'inactive' | 'unknown'
  service_units: string[]
  managed_config: string
  managed_config_present: boolean
  managed_config_hash: string
  managed_shares: string[]
  /** NFS 导出的能力报告（P14.4）。与上面这一整套 Samba 判定彼此独立。 */
  nfs: ShareNfsCapability
  notices?: ShareNotice[]
  collected_at: string
}

/** 能力是否可用。页面与预检用同一个判据，不各自解释 status 字符串。 */
export function capabilityAvailable(cap: ShareCapability | null | undefined): boolean {
  return cap?.status === 'available'
}

// ---------------------------------------------------------------- 定义与记录

export const SHARE_PROTOCOLS = ['smb', 'nfs'] as const
export type ShareProtocol = (typeof SHARE_PROTOCOLS)[number]

/** 协议的中文说法：页面上到处要用，集中一处免得三种叫法。 */
export const SHARE_PROTOCOL_LABELS: Record<ShareProtocol, string> = {
  smb: 'SMB（Windows 共享）',
  nfs: 'NFS 导出',
}

/** NFS 导出选项（P14.4）：初期闭集，两个都必须为 true。 */
export interface ShareNfsOptions {
  root_squash: boolean
  sync: boolean
}

/**
 * 面板唯一会写的那份 NFS 导出选项。
 *
 * 没有开关：`no_root_squash` 与 `async` 根本不在取值里，给一个只能打开的开关只是噪音。
 * 契约原文见 CONTRACTS §7 与 PLAN §10。
 */
export const FIXED_NFS_OPTIONS: ShareNfsOptions = { root_squash: true, sync: true }

export const SHARE_ACCESS_LEVELS = ['read', 'write'] as const
export type ShareAccess = (typeof SHARE_ACCESS_LEVELS)[number]

export const SHARE_ACCESS_LABELS: Record<ShareAccess, string> = {
  read: '只读',
  write: '读写',
}

/** 共享里的一个账号授权。 */
export interface ShareUser {
  account_id: string
  access: ShareAccess
}

/** 共享定义（CONTRACTS §7 的 ShareDefinition）。 */
export interface ShareDefinition {
  name: string
  protocol: ShareProtocol
  volume_id: string
  relative_path: string
  enabled: boolean
  read_only: boolean
  allowed_cidrs: string[]
  smb_users: ShareUser[]
  nfs_options?: ShareNfsOptions | null
}

/** 运行时撤销的现场（P08.3）。为空表示没有被运行时撤销过。 */
export interface RuntimeRevocation {
  at: string
  volume_id: string
  volume_name: string
  volume_state: string
  reason: string
}

export interface ShareRecord {
  id: string
  definition: ShareDefinition
  config_hash: string
  version: string
  directory_prepared: boolean
  config_applied: boolean
  runtime_revoked?: RuntimeRevocation | null
  last_operation_id?: string
  created_at: string
  updated_at: string
}

/**
 * 卷那一侧的事实，页面自己 join 出来。
 *
 * 共享记录里**没有**卷的状态与名字（openapi 的 ShareRecord 就这么些字段）：一份定义记的是
 * “我要共享哪个卷的哪个路径”，而“那个卷此刻挂着没有”是会变的外部事实，属于卷，不属于共享。
 * 把它抄进共享记录只会有两份互相矛盾的真相。所以页面拿 `/storage/volumes` 的
 * `observed_state` 与 `display_name` 按 `volume_id` 对上来。
 *
 * `state` 缺失（还没取到卷列表）时按“不知道”处理，不按“没挂”处理——见 summarizeShareState。
 */
export interface ShareVolumeInfo {
  id: string
  name: string
  state: string
}

/** 受管理账号（没有密码字段，也没有密码哈希：记录里只有秘密引用）。 */
export interface ShareAccount {
  id: string
  name: string
  type: string
  managed: boolean
  disabled: boolean
  has_password: boolean
  uid: number
  gid: number
  shell: string
  home: string
  version: string
  last_operation_id?: string
  created_at: string
  updated_at: string
}

// ---------------------------------------------------------------- 连接状态

export interface ShareConnection {
  share: string
  machine: string
  username: string
  dialect: string
  signing: string
  connected_at: string
  open_files: number
}

/**
 * 连接状态报告。
 *
 * Service 单独给出来是因为**空连接列表有两种含义**：“没人连着”与“smbd 没在跑”。
 * 页面必须把这两种情况分开说，否则“服务停了”会显示成“一切正常，只是没人用”。
 */
export interface ShareConnections {
  share_id: string
  share_name: string
  service: 'active' | 'inactive' | 'unknown'
  connections: ShareConnection[]
  collected_at: string
  notices?: ShareNotice[]
}

// ---------------------------------------------------------------- 读接口

interface ShareListPayload {
  capability: ShareCapability
  shares: ShareRecord[]
}

interface ShareDetailPayload {
  capability: ShareCapability
  share: ShareRecord
}

interface AccountListPayload {
  capability: ShareCapability
  accounts: ShareAccount[]
}

export function getShares(): Promise<ApiResult<ShareListPayload>> {
  return get<ShareListPayload>('/shares')
}

export function getShare(id: string): Promise<ApiResult<ShareDetailPayload>> {
  return get<ShareDetailPayload>(`/shares/${encodeURIComponent(id)}`)
}

export function getShareConnections(id: string): Promise<ApiResult<ShareConnections>> {
  return get<ShareConnections>(`/shares/${encodeURIComponent(id)}/connections`)
}

export function getShareAccounts(): Promise<ApiResult<AccountListPayload>> {
  return get<AccountListPayload>('/share-accounts')
}

// ---------------------------------------------------------------- 秘密通道

export const SECRET_PURPOSE_SMB_PASSWORD = 'smb_password'

export interface SecretCreated {
  secret_ref: string
  purpose: string
  state: string
  expires_at?: string
}

/**
 * 提交一份明文秘密，拿回引用。明文**只进不出**：响应里没有 value，也没有 hash。
 *
 * 面板侧再认证由调用方负责（这个路由要求 admin + 再认证窗口）。页面拿到 secret_ref 之后
 * 只把它放进动作参数里，任何地方都不缓存明文。
 */
export function createSecret(value: string, reference?: string): Promise<ApiResult<SecretCreated>> {
  return request<SecretCreated>('/secrets', {
    method: 'POST',
    body: {
      purpose: SECRET_PURPOSE_SMB_PASSWORD,
      value,
      ...(reference ? { reference } : {}),
    },
  })
}

// ---------------------------------------------------------------- 状态翻译

export type ShareStateKind = 'serving' | 'not_applied' | 'revoked' | 'volume_offline' | 'disabled'

export interface ShareStateSummary {
  kind: ShareStateKind
  label: string
  /** 一句话解释“为什么是这样”，给状态标签下面那行小字用。 */
  detail: string
  tone: 'success' | 'info' | 'warning' | 'error' | 'default'
}

/**
 * 一条共享此刻到底处于哪种状态。
 *
 * 判断顺序是有讲究的，从“最不可用”往“最可用”走：
 *   1. 运行时被撤销——卷掉线导致的撤销，最需要人处理，优先显示；
 *   2. 卷此刻不挂——即使配置还在，内容也是错的（或根本连不上）；
 *   3. 定义被禁用——人主动关掉的；
 *   4. 配置没写入——只准备了目录与 ACL；
 *   5. 其余才是“正在提供访问”。
 *
 * 这里**不猜**：`volume` 没给（卷列表还没取回来）时按“配置是否写入”判断，不把“不知道”
 * 当成“没挂”——把未知说成离线，会让人去查一个没坏的卷。
 */
export function summarizeShareState(
  share: ShareRecord,
  volume?: ShareVolumeInfo | null,
): ShareStateSummary {
  const revoked = share.runtime_revoked ?? null
  if (revoked) {
    const when = formatTime(revoked.at)
    return {
      kind: 'revoked',
      label: '运行时已撤销',
      detail: `卷「${revoked.volume_name || revoked.volume_id}」在 ${when} 掉线（当时观测为 ${revoked.volume_state}），这一段已从 Samba 配置里摘掉。卷回来之后需要重新应用一次。`,
      tone: 'warning',
    }
  }
  const vs = volume?.state ?? ''
  if (vs !== '' && vs !== 'mounted') {
    const name = volume?.name || share.definition.volume_id
    // “没挂”与“读不出来”不是一件事：`unknown`/`error`/`mismatch` 时盘可能好好挂着，
    // 只是面板没读到。渲染层的门对这两类一律不写配置段（不能拿空目录冒充共享），
    // 但话得分开说——说成“没挂”会打发人去查一块没坏的盘。
    const offline = vs === 'unmounted' || vs === 'missing'
    return {
      kind: 'volume_offline',
      label: offline ? '卷此刻没挂' : '卷状态读不出来',
      detail: offline
        ? `卷「${name}」当前观测为 ${vs}；挂载之前这份定义不会写进配置。`
        : `卷「${name}」当前观测为 ${vs}：面板读不到这块卷的真实状态，按没挂处理（这一层同样不写配置段）。先看卷那一页的挂载与设备身份。`,
      tone: offline ? 'error' : 'warning',
    }
  }
  if (!share.definition.enabled) {
    return {
      kind: 'disabled',
      label: '已禁用',
      detail: '定义被停用：目录与文件都还在，ACL 已撤销，配置里没有这一段。',
      tone: 'default',
    }
  }
  if (!share.config_applied) {
    return {
      kind: 'not_applied',
      label: '配置未写入',
      detail: share.directory_prepared
        ? '目录与 ACL 已准备并核验，但这一段还没写进 Samba 配置：此刻不提供访问。'
        : '定义已登记，目录与 ACL 尚未准备。',
      tone: 'info',
    }
  }
  return {
    kind: 'serving',
    label: '已写入配置',
    detail: '这一段已经在托管配置里；客户端能不能连上还取决于 smbd 是否在运行与来源网段。',
    tone: 'success',
  }
}

function formatTime(value: string): string {
  if (!value) return '未知时间'
  const d = new Date(value)
  if (Number.isNaN(d.getTime())) return value
  return d.toLocaleString('zh-CN', { hour12: false })
}

// ---------------------------------------------------------------- 客户端连接说明

export type ClientKind =
  | 'windows'
  | 'macos'
  | 'linux-mount'
  | 'linux-smbclient'
  | 'linux-nfs-mount'
  | 'macos-nfs'
  | 'windows-nfs'

export interface ClientHint {
  kind: ClientKind
  label: string
  /** 图形界面里直接填的地址（Windows 的资源管理器地址栏 / Finder 的“连接服务器”）。 */
  address: string
  /** 命令行，逐行可复制；**不含密码**。 */
  commands: string[]
  notes: string[]
}

/**
 * 生成“客户端怎么连”的说明。
 *
 * host 由调用方给：面板**不知道**自己该用哪个地址被访问（它只有网卡计数器，没有地址表，
 * 也不该猜哪块网卡对外）。页面传 `window.location.hostname`——那是人此刻正在用的地址，
 * 也是这台机器的一个真实入口；如果客户端在别的机器上，说明里会要求换成服务器的内网地址。
 *
 * 三条硬规矩：
 *   1. **命令里永远不出现密码**。Linux 上 `mount.cifs` 的 `password=` 会留在进程列表里，
 *      与“密码不进 argv”是同一条底线；这里给的是 `credentials=<0600 文件>` 的写法。
 *   2. 未挂载/未写入配置的共享照样给说明，但第一条 notes 会写明此刻不可用——说明是给
 *      人准备客户端用的，不是“已可用”的证明。
 *   3. `allowed_cidrs` 原样列出，并带上环回那条事实：Samba 对本机永远放行，名单管不住本机。
 */
export function clientHints(share: ShareRecord, host: string): ClientHint[] {
  const def = share.definition
  if (def.protocol === 'nfs') return nfsClientHints(share, host)

  const name = def.name
  const address = `\\\\${host}\\${name}`
  const url = `smb://${host}/${name}`
  const remote = `/mnt/${name}`
  const users = def.smb_users.map((u) => u.access)
  const writable = !def.read_only && users.includes('write')

  const common: string[] = []
  if (!share.config_applied || share.runtime_revoked) {
    common.push('这条共享此刻没有写在 Samba 配置里（见状态标签），下面的地址要等它应用之后才能连。')
  }
  if (def.read_only) {
    common.push('这份共享是只读的：所有账号的权限上限都是 r-x。')
  } else if (!writable) {
    common.push('这份共享没有授权任何读写账号：即使连上也只能读。')
  }
  if (def.allowed_cidrs.length > 0) {
    common.push(
      `只有这些来源网段能连：${def.allowed_cidrs.join('、')}。注意 Samba 对本机（127.0.0.1）永远放行，这份名单管不住本机。`,
    )
  } else {
    common.push('这份共享没有限制来源网段：任何能连到本机 445 端口的来源都能尝试认证。')
  }
  common.push('账号密码就是共享账号的密码，由管理员在面板里设置；面板不会把它显示出来。')

  return [
    {
      kind: 'windows',
      label: 'Windows 资源管理器',
      address,
      commands: [],
      notes: [...common, '在地址栏直接粘贴上面的地址；提示凭据时填共享账号与密码。'],
    },
    {
      kind: 'macos',
      label: 'macOS Finder',
      address: url,
      commands: [],
      notes: [...common, 'Finder →「前往」→「连接服务器…」（⌘K），粘贴上面的地址。'],
    },
    {
      kind: 'linux-mount',
      label: 'Linux 挂载（mount.cifs）',
      address: url,
      commands: [
        `sudo install -d -m 0700 /root/.smb`,
        `# 凭据从标准输入写进只有 root 能读的文件：下面是 here-doc，密码不进 argv`,
        `sudo sh -c 'umask 077; cat > /root/.smb/${name}.cred' <<'CRED'`,
        `username=<共享账号>`,
        `password=<密码>`,
        `CRED`,
        `sudo chmod 0600 /root/.smb/${name}.cred`,
        `sudo install -d -m 0755 ${remote}`,
        `sudo mount -t cifs //${host}/${name} ${remote} -o credentials=/root/.smb/${name}.cred,vers=3.0,uid=$(id -u),gid=$(id -g)`,
        `mount | grep ${name}`,
        `sudo umount ${remote}`,
      ],
      notes: [
        ...common,
        '`mount.cifs` 的 `password=` 选项会出现在进程列表里，所以这里用 `credentials=` 文件（0600）。',
        '凭据文件用 here-doc 从标准输入写：`printf "password=…"` 那种写法虽然不挂在 mount 的 argv 上，却会把密码留在 `sh` 自己的 argv 里，同样能被别的进程读到。',
      ],
    },
    {
      kind: 'linux-smbclient',
      label: 'Linux 命令行（smbclient）',
      address: url,
      commands: [
        `smbclient //${host}/${name} -U <共享账号> -m SMB3`,
        `# 回车后输入密码；也可以用 -A <凭据文件> 从文件读`,
      ],
      notes: [...common, '`smbclient` 的 `-U` 只给账号名，密码在交互里输入，不进 argv。'],
    },
  ]
}

/**
 * 这份说明是否**当前**可用（用于在页面顶部提醒）。
 *
 * 与 summarizeShareState 分开：状态是“这条共享怎样了”，这里是“说明里的地址现在能不能连”。
 */
export function clientHintsUsable(share: ShareRecord, volume?: ShareVolumeInfo | null): boolean {
  if (share.runtime_revoked) return false
  if (!share.definition.enabled) return false
  if (!share.config_applied) return false
  const vs = volume?.state ?? ''
  return vs === '' || vs === 'mounted'
}

/**
 * NFS 的客户端说明（P14.4）。
 *
 * 与 SMB 那三块最大的不同：**没有账号**。命令里没有用户名、没有密码、没有凭据文件——
 * 能不能连只取决于来源网段，连上之后能做什么取决于 uid 与 root_squash。
 *
 * 导出目录写的是 `<卷挂载点>/<相对路径>`，而不是一个猜出来的绝对路径：面板这边知道卷的
 * 挂载点，但这份 DTO 里没有它（ShareRecord 不带卷的现场）。与其编一个像模像样的路径，
 * 不如给出**能问出真路径**的命令：`showmount -e` 打印的就是服务端此刻的导出表。
 */
function nfsClientHints(share: ShareRecord, host: string): ClientHint[] {
  const def = share.definition
  const name = def.name
  const exportPath = `<卷挂载点>/${def.relative_path === '' ? '' : def.relative_path}`
  const remote = `/mnt/${name}`

  const common: string[] = []
  if (!share.config_applied || share.runtime_revoked) {
    common.push('这条导出此刻不在托管导出文件里（见状态标签），要等应用之后才能连。')
  }
  common.push(
    `只有这些来源网段能连：${def.allowed_cidrs.join('、')}。NFS 没有账号这一层——` +
      '网段之外连不上，网段之内不需要密码。',
  )
  if (def.read_only) {
    common.push('这份导出是只读的：导出行写 ro，目录权限 0755——客户端连写都写不进去。')
  } else {
    common.push(
      '这份导出是读写的：导出行写 rw，目录权限 0777。客户端以**自己的 uid** 落盘，' +
        '所以服务端文件属主就是那个 uid。',
    )
  }
  common.push(
    'root_squash 是固定的：客户端 root 会被 squash 成 nobody（落盘属主 nobody:nogroup），' +
      '所以用 root 往里写往往会被拒——换普通用户，或者接受产物属主是 nobody。',
  )
  if (def.nfs_options !== undefined && def.nfs_options !== null) {
    common.push(
      `导出选项固定为 root_squash=${String(def.nfs_options.root_squash)}、sync=${String(def.nfs_options.sync)}；` +
        '没有 no_root_squash、没有 async，面板不提供这些开关。',
    )
  }

  return [
    {
      kind: 'linux-nfs-mount',
      label: 'Linux 挂载（mount.nfs）',
      address: `${host}:${exportPath}`,
      commands: [
        `showmount -e ${host}`,
        `sudo install -d -m 0755 ${remote}`,
        `sudo mount -t nfs -o vers=3,proto=tcp ${host}:${exportPath} ${remote}`,
        `findmnt -T ${remote}`,
        `sudo umount ${remote}`,
      ],
      notes: [
        ...common,
        '`showmount -e` 打印的就是服务端此刻的导出表，导出目录以上面那一条为准。',
        '挂完之后先 `findmnt` 确认真的挂上了：NFS 挂载失败时客户端可能**静默写本地目录**，' +
          '而且 mount 的退出码仍然是 0——没确认就写，文件会落在自己机器的系统盘上。',
        'NFSv4 如果被别处的 `fsid=0` 导出挡住，用 `vers=3,proto=tcp` 先挂上；面板不假设 v4。',
      ],
    },
    {
      kind: 'macos-nfs',
      label: 'macOS（mount_nfs）',
      address: `${host}:${exportPath}`,
      commands: [
        `showmount -e ${host}`,
        `sudo mkdir -p ${remote}`,
        `sudo mount -t nfs -o vers=3,proto=tcp ${host}:${exportPath} ${remote}`,
        `sudo umount ${remote}`,
      ],
      notes: [...common, 'macOS 自带的 `mount_nfs` 与 Linux 的选项写法基本一致，`vers=3` 最省事。'],
    },
    {
      kind: 'windows-nfs',
      label: 'Windows（NFS 客户端，可选装）',
      address: `\\\\${host}${exportPath.replaceAll('/', '\\')}`,
      commands: [`mount -o anon ${host}:${exportPath} Z:`],
      notes: [
        ...common,
        'Windows 的家庭版默认没有 NFS 客户端；「启用或关闭 Windows 功能」里装「NFS 服务」之后才有 `mount`。',
        '`anon` 表示用匿名身份挂载：root_squash 之后落盘的就是 nobody，与 Linux 客户端一致。',
      ],
    },
  ]
}
