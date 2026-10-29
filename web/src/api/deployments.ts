// 受管部署的 DTO、状态翻译与表单草稿（P15.4）。
//
// 两张表回答两个问题，不许混（CONTRACTS.md 第 8.9 节）：
//
//	GET /deployments          观测面：docker 报回来的 compose 项目（第 8.3 节，一直存在）
//	GET /deployments/managed  管理面：/etc/upanel/deploy 的三个文件 + systemd 的两个回答
//
// 两张表按 `deployment_id` 连起来——它与 compose 项目标识 `containers.ProjectID(name)`
// 逐字相同（`dep_` 加名字的 sha256 前 32 位）。观测面说“容器在不在跑”，管理面说
// “这是我们部署的、版本是什么、能不能更新”。一个项目可能出现在观测面而不在管理面
// （有人在面板之外 `docker compose up` 起来的），那种东西页面不替它编造版本。
//
// 这个模块只做四件事：取数据、把现场翻译成人话、把“为什么这个按钮现在按不下去”的
// **提示**算出来、把表单草稿变成可提交的定义。
//
// 判断“能不能做”不在页面里：真正的判决在预检（POST /operations/preflight）。
// 这里的 `updateHint` / `removeHint` 只解释**已知的**不满足条件（比如盘上没有定义记录），
// 不能替代预检结论——先渲染按钮再让点击失败，比不渲染更糟；反过来，页面自己宣布
// “可以更新”也一样糟，因为卷可能刚被卸下、秘密可能刚被轮换。
//
// 这里**没有** compose_yaml、没有 privileged、没有 devices、没有 network_mode、没有
// cap_add：定义是闭集（第 8.2 节），危险选项在数据模型里没有落点，页面也就不给它们
// 留输入框——留一个输入框再把提交拒掉，比不留更糟。

import { get, type ApiResult } from './client'

// ---------------------------------------------------------------- 形状常量

export const MAX_DEPLOYMENT_NAME_LENGTH = 63
/** 部署名同时是 Compose 项目名与容器名前缀。 */
export const DEPLOYMENT_NAME_PATTERN = /^[a-z][a-z0-9_-]{0,62}$/
export const DEPLOYMENT_ID_PATTERN = /^dep_[0-9a-f]{32}$/
export const VOLUME_ID_PATTERN = /^vol_[0-9a-f]{32}$/
export const SECRET_REF_PATTERN = /^sec_[0-9a-f]{32}$/
export const FILE_ROOT_ID_PATTERN = /^[a-z][a-z0-9_-]{0,31}$/
export const CPU_LIMIT_PATTERN = /^[0-9]{1,2}(\.[0-9]{1,3})?$/

export const IMAGE_REFERENCE_MAX_LENGTH = 255
export const MAX_ENV_VARS = 64
export const MAX_ENV_NAME_LENGTH = 128
export const MAX_MOUNTS = 16
export const MAX_VOLUME_DEPENDENCIES = 32
export const ENV_NAME_PATTERN = /^[A-Za-z_][A-Za-z0-9_]*$/

// 用乘法而不是 `<<`：位移在 JS 里是 32 位运算，64 GiB 会被截成 0。
export const MIN_MEMORY_LIMIT_BYTES = 16 * 1024 * 1024
export const MAX_MEMORY_LIMIT_BYTES = 64 * 1024 * 1024 * 1024
export const MIN_CPU_LIMIT = 0.05
export const MAX_CPU_LIMIT = 8

export const MIN_HEALTH_INTERVAL_SECONDS = 5
export const MAX_HEALTH_INTERVAL_SECONDS = 300
export const MIN_HEALTH_TIMEOUT_SECONDS = 1
export const MAX_HEALTH_TIMEOUT_SECONDS = 60
export const MAX_HEALTH_RETRIES = 10
export const MAX_HEALTH_START_SECONDS = 600

/** 容器内不能挂进去的目标（CONTRACTS.md 第 8.2 节）。 */
export const BLOCKED_CONTAINER_PATHS = [
  '/proc',
  '/sys',
  '/dev',
  '/boot',
  '/lib/modules',
  '/run/docker.sock',
  '/var/run/docker.sock',
  '/run/podman/podman.sock',
  '/run/containerd/containerd.sock',
] as const

// ---------------------------------------------------------------- 现场（管理面）

/** systemd 对包装单元的读数。`known=false` 时 `reason` 说明为什么没有读数。 */
export interface ManagedDeploymentUnitState {
  known: boolean
  enabled: boolean
  active: boolean
  reason?: string
}

/** 一条环境变量：公开值与秘密引用二选一。明文永远不会出现在这个接口里。 */
export interface ManagedDeploymentEnvVar {
  name: string
  value?: string
  secret_ref?: string
}

/** 一条 bind 挂载：宿主侧只能由“文件根 id + 根内相对路径”表达。 */
export interface ManagedDeploymentMount {
  source_root_id: string
  relative_path?: string
  container_path: string
  read_only: boolean
}

export interface ManagedDeploymentResourceLimits {
  memory_limit_bytes: number
  /** 十进制字符串（"0.5"），不是浮点：0.1 这类值二进制往返后会变得不可比对。 */
  cpu_limit: string
}

export const HEALTH_CHECK_KINDS = ['http', 'tcp'] as const
export type HealthCheckKind = (typeof HEALTH_CHECK_KINDS)[number]

/** 面板侧的探测描述：**不**渲染进 Compose。 */
export interface ManagedDeploymentHealthCheck {
  enabled: boolean
  kind?: HealthCheckKind
  /** 0 表示用 container_port。 */
  port?: number
  path?: string
  interval_seconds?: number
  timeout_seconds?: number
  retries?: number
  start_period_seconds?: number
}

/** 受约束的部署定义：十个字段，一个不多一个不少。 */
export interface ManagedDeploymentDefinition {
  name: string
  image_reference: string
  container_port: number
  host_bind_address: string
  host_port: number
  environment?: ManagedDeploymentEnvVar[]
  mounts?: ManagedDeploymentMount[]
  resource_limits: ManagedDeploymentResourceLimits
  health_check?: ManagedDeploymentHealthCheck
  volume_dependencies?: string[]
}

/**
 * 某一版配置对应哪个镜像。
 *
 * 两个 digest 是两件事：`requested` 是定义里写的（`nginx:1.27`，人话、可变），
 * `digest` 是 `docker image inspect` 的 `.Id`（这份镜像的唯一身份，空串表示没有读数）。
 */
export interface ManagedDeploymentImageRecord {
  requested: string
  digest?: string
  repo_digests?: string[]
  config_digest: string
  resolved_at: string
}

/** `<name>.deployment.json` 的原样内容（没有明文秘密，只有 sec_ 引用）。 */
export interface ManagedDeploymentRecord {
  schema: number
  deployment_id: string
  name: string
  definition: ManagedDeploymentDefinition
  config_digest: string
  image: ManagedDeploymentImageRecord
  previous?: ManagedDeploymentImageRecord
  updated_at: string
}

/** 一份部署此刻的现场。 */
export interface ManagedDeployment {
  name: string
  /** 与 compose 项目标识同一个值；页面据此把观测面与这张表连起来。 */
  deployment_id: string
  project: string
  unit: string
  unit_path: string
  config_path: string
  config_present: boolean
  /** 盘上那份渲染配置的 sha256；没有配置时是空串。 */
  config_digest: string
  /** 更新与删除要带回来的版本令牌（配置摘要，或 `unit-only` / `absent`）；不会为空。 */
  version: string
  previous_config_present: boolean
  record_present: boolean
  /** 记录读不了的原因。与 `record_present=false` 是两件事。 */
  record_error?: string
  record?: ManagedDeploymentRecord
  /** 盘上配置的摘要与记录里的不一致：有人在面板之外动过那份文件。 */
  config_drift: boolean
  unit_state: ManagedDeploymentUnitState
}

export interface ManagedDeploymentSkipped {
  name: string
  reason: string
}

export interface ManagedDeploymentListData {
  /** 没有部署时是空数组，不是 null。 */
  deployments: ManagedDeployment[]
  /** 配置目录里存在但没能读成部署的条目；非空说明有东西面板不知道。 */
  skipped?: ManagedDeploymentSkipped[]
  truncated: boolean
}

// ---------------------------------------------------------------- 取数据

export function listManagedDeployments(): Promise<ApiResult<ManagedDeploymentListData>> {
  return get<ManagedDeploymentListData>('/deployments/managed')
}

/**
 * 读一份部署的现场。
 *
 * 名字在发请求之前先按形状拒一次：它是 URL 路径段，`../` 这类东西不该由服务端
 * 的路径规范化来兜底（那会落到 404，和“这份部署不存在”长得一样）。
 *
 * 声明成 async 是有意的：那样非法名字是一条**被拒绝的 Promise**，而不是在调用处
 * 同步炸出来——调用方一律 `try { await ... } catch`，两条路径得能一起被接住。
 */
export async function getManagedDeployment(name: string): Promise<ApiResult<ManagedDeployment>> {
  const trimmed = name.trim()
  if (!DEPLOYMENT_NAME_PATTERN.test(trimmed)) {
    throw new RangeError('部署名的形状是小写字母开头、随后是小写字母/数字/下划线/中划线（最多 63 位）')
  }
  return get<ManagedDeployment>(`/deployments/managed/${encodeURIComponent(trimmed)}`)
}

// ---------------------------------------------------------------- 现场翻译

export function isValidDeploymentName(value: string): boolean {
  return DEPLOYMENT_NAME_PATTERN.test(value.trim())
}

/**
 * 这份部署此刻处在什么阶段。
 *
 * 只读 `unit_state`：容器在不在跑是观测面的事（第 8.3 节），管理面回答的是
 * “systemd 怎么看这条包装单元”。两边不一致本身是要显示出来的结论，不是矛盾。
 */
export type DeploymentPhase = 'running' | 'stopped' | 'unit_missing'

export const DEPLOYMENT_PHASE_LABELS: Record<DeploymentPhase, string> = {
  running: '运行中',
  stopped: '已停止',
  unit_missing: '没有单元读数',
}

export function deploymentPhase(deployment: ManagedDeployment): DeploymentPhase {
  const unit = deployment.unit_state
  if (!unit.known) return 'unit_missing'
  return unit.active ? 'running' : 'stopped'
}

/** 阶段的一句话解释。`unit_missing` 的两种原因必须分开说。 */
export function deploymentPhaseDetail(deployment: ManagedDeployment): string {
  const unit = deployment.unit_state
  if (!unit.known) {
    return unit.reason && unit.reason !== ''
      ? unit.reason
      : '没有单元读数：单元文件不在磁盘上，或者 systemctl 没有回答'
  }
  const enablement = unit.enabled ? '开机会自动启动' : '开机不会自动启动'
  return unit.active ? `单元在跑，${enablement}` : `单元没在跑，${enablement}`
}

/**
 * 版本令牌的短形式。
 *
 * 版本正常是 `sha256:…`（64 位十六进制），配置缺失时是 `unit-only` / `absent`。
 * 列表里只显示前 12 位十六进制，完整值留给详情与预检——把人看得懂的读者挡住
 * 在 64 个字符外面没有意义。
 */
export function shortVersion(version: string): string {
  const trimmed = version.trim()
  if (trimmed === '') return '未知'
  const prefix = 'sha256:'
  if (!trimmed.startsWith(prefix)) return trimmed
  const hex = trimmed.slice(prefix.length)
  if (hex.length <= 12) return trimmed
  return `${prefix}${hex.slice(0, 12)}…`
}

/** 镜像 digest 的短形式（12 位），空串表示没有读数。 */
export function shortDigest(digest: string | undefined): string {
  if (digest === undefined || digest === '') return '没有读数'
  const hex = digest.startsWith('sha256:') ? digest.slice('sha256:'.length) : digest
  return hex.length <= 12 ? hex : `${hex.slice(0, 12)}…`
}

/**
 * 为什么现在更新不了。返回 null 表示“没有已知的拦路条件”——不是“一定成功”。
 *
 * 两条已知条件：
 *  1. 盘上没有定义记录（P15.3 之前创建的部署）：update 要拿记录里那份规范化定义去
 *     渲染新配置，没有它就只能猜“当前钉的是哪个镜像”；
 *  2. 盘上配置与记录不一致：那说明有人在面板之外改过这份文件，版本令牌与记录对不上。
 *     服务端会在预检里以 `record_mismatch` 拒绝，这里提前说出来，省一次往返。
 */
export function updateHint(deployment: ManagedDeployment): string | null {
  if (!deployment.record_present) {
    if (deployment.record_error) {
      return `盘上的定义记录读不了（${deployment.record_error}）：更新要靠它渲染新配置，只能删除后重新部署`
    }
    return '盘上没有定义记录：更新要靠它渲染新配置，只能删除后重新部署'
  }
  if (deployment.config_drift) {
    return '盘上配置与定义记录不一致（有人在面板之外改过它）：更新预检会以 record_mismatch 拒绝'
  }
  return null
}

/** 为什么现在删不了。删除不看定义记录，但版本令牌必须是读得出来的那种。 */
export function removeHint(deployment: ManagedDeployment): string | null {
  if (deployment.version.trim() === '') {
    return '读不到这份部署的版本令牌，删除会被预检拒绝'
  }
  return null
}

/** 列表里那句话：配置在不在、上一版在不在、记录读没读出来。 */
export function describeDeploymentFiles(deployment: ManagedDeployment): string {
  const parts: string[] = []
  parts.push(deployment.config_present ? '配置在盘上' : '盘上没有配置')
  parts.push(deployment.previous_config_present ? '上一版留着' : '没有上一版')
  if (deployment.record_error) {
    parts.push(`定义记录读不了（${deployment.record_error}）`)
  } else {
    parts.push(deployment.record_present ? '定义记录在' : '没有定义记录')
  }
  return parts.join(' · ')
}

// ---------------------------------------------------------------- 预检参数

/**
 * `deployment.apply` 的参数（只创建）。
 *
 * `deployment_id` 是 null、`version` 是空串：这条动作的语义就是“新部署”。
 * 已经存在时服务端会拒（`ErrDeploymentExists`），换镜像要发
 * `deployment.update`——不会在这里被当成“再 apply 一次”悄悄覆盖。
 */
export function applyParams(definition: ManagedDeploymentDefinition): Record<string, unknown> {
  return { deployment_id: null, version: '', definition }
}

/** `deployment.update` 的参数：只换镜像，可选地先备份一次。 */
export function updateParams(
  deployment: ManagedDeployment,
  imageReference: string,
  backupPolicyId: string | null = null,
): Record<string, unknown> {
  const image = imageReference.trim()
  if (image === '') throw new RangeError('镜像引用不能为空')
  if (deployment.version.trim() === '') {
    throw new RangeError('读不到这份部署的版本令牌，不能提交更新')
  }
  const params: Record<string, unknown> = {
    deployment_id: deployment.deployment_id,
    version: deployment.version,
    image_reference: image,
  }
  if (backupPolicyId !== null && backupPolicyId.trim() !== '') {
    params.backup_policy_id = backupPolicyId.trim()
  } else {
    // 显式给 null：与“没给这个键”区分开（预检会把前者读成“不备份”）。
    params.backup_policy_id = null
  }
  return params
}

/**
 * `deployment.remove` 的参数。
 *
 * 只带标识与版本：删除要删的是“你看到的那一份”。数据卷与业务目录一个字节都不动
 * （CONTRACTS 第 5 节），所以参数里没有任何“顺便清理”的开关。
 */
export function removeParams(deployment: ManagedDeployment): Record<string, unknown> {
  if (deployment.version.trim() === '') {
    throw new RangeError('读不到这份部署的版本令牌，不能提交删除')
  }
  return { deployment_id: deployment.deployment_id, version: deployment.version }
}

// ---------------------------------------------------------------- 表单草稿

export interface DeploymentEnvDraft {
  name: string
  /** 公开值与秘密引用二选一；切换时另一侧的值不清空，方便改主意。 */
  kind: 'value' | 'secret'
  value: string
  secret_ref: string
}

export interface DeploymentMountDraft {
  source_root_id: string
  relative_path: string
  container_path: string
  read_only: boolean
}

export interface DeploymentHealthDraft {
  enabled: boolean
  kind: HealthCheckKind
  /** 0 表示用 container_port。 */
  port: number
  path: string
  interval_seconds: number
  timeout_seconds: number
  retries: number
  start_period_seconds: number
}

export interface DeploymentDraft {
  name: string
  image_reference: string
  container_port: number
  host_bind_address: string
  host_port: number
  environment: DeploymentEnvDraft[]
  mounts: DeploymentMountDraft[]
  /** 内存上限用 MiB 输入（界面上写 512 表示 512 MiB），落盘换成字节。 */
  memory_limit_mib: number
  cpu_limit: string
  health_check: DeploymentHealthDraft
  volume_dependencies: string[]
}

export const MEBIBYTE = 1024 * 1024

/** 新建部署的草稿默认值。默认只绑回环：不对外暴露任何东西。 */
export function emptyDeploymentDraft(): DeploymentDraft {
  return {
    name: '',
    image_reference: '',
    container_port: 80,
    host_bind_address: '127.0.0.1',
    host_port: 8080,
    environment: [],
    mounts: [],
    memory_limit_mib: 512,
    cpu_limit: '0.5',
    health_check: {
      enabled: false,
      kind: 'http',
      port: 0,
      path: '/',
      interval_seconds: 30,
      timeout_seconds: 3,
      retries: 3,
      start_period_seconds: 0,
    },
    volume_dependencies: [],
  }
}

/** 一份校验结论：`field` 与 JSON 字段同名（带下标），页面据此高亮那一行。 */
export interface DeploymentDraftProblem {
  field: string
  message: string
}

function looksLikeIPLiteral(value: string): boolean {
  const trimmed = value.trim()
  if (trimmed === '') return false
  if (trimmed.includes(':')) {
    // IPv6 只做“像不像”的检查：真正的解析在代理侧（net.ParseIP），
    // 而“这个地址是不是本机的”只有代理读得到本机地址表之后才答得出来。
    return /^[0-9A-Fa-f:]+$/.test(trimmed)
  }
  const parts = trimmed.split('.')
  if (parts.length !== 4) return false
  return parts.every((part) => /^\d{1,3}$/.test(part) && Number(part) <= 255)
}

/**
 * 草稿 → 提交前的本地检查。
 *
 * 这只是**立即反馈**：真正的判决在预检（它还要看宿主事实：卷在不在、秘密读不读得到、
 * 端口有没有被占）。这里拦的东西，代理侧一律还会再拦一次——两处都拦不是重复，
 * 因为一次错误的表单不该花掉一次预检往返。
 */
export function validateDeploymentDraft(draft: DeploymentDraft): DeploymentDraftProblem | null {
  const name = draft.name.trim()
  if (name === '') return { field: 'name', message: '部署名不能为空' }
  if (!DEPLOYMENT_NAME_PATTERN.test(name)) {
    return {
      field: 'name',
      message: '部署名的形状是小写字母开头、随后是小写字母/数字/下划线/中划线（最多 63 位）',
    }
  }

  const image = draft.image_reference.trim()
  if (image === '') return { field: 'image_reference', message: '镜像引用不能为空' }
  if (image.length > IMAGE_REFERENCE_MAX_LENGTH) {
    return { field: 'image_reference', message: `镜像引用最多 ${IMAGE_REFERENCE_MAX_LENGTH} 个字符` }
  }
  if (/\s/.test(image)) return { field: 'image_reference', message: '镜像引用里不能有空白' }

  if (!Number.isInteger(draft.container_port) || draft.container_port < 1 || draft.container_port > 65535) {
    return { field: 'container_port', message: '容器端口必须在 1—65535 之间' }
  }
  if (!looksLikeIPLiteral(draft.host_bind_address)) {
    return {
      field: 'host_bind_address',
      message: '绑定地址必须是字面 IP（例如 127.0.0.1 或 192.0.2.10），不接受主机名',
    }
  }
  if (!Number.isInteger(draft.host_port) || draft.host_port < 1 || draft.host_port > 65535) {
    return { field: 'host_port', message: '宿主端口必须在 1—65535 之间' }
  }

  if (draft.environment.length > MAX_ENV_VARS) {
    return { field: 'environment', message: `环境变量最多 ${MAX_ENV_VARS} 条` }
  }
  const envNames = new Set<string>()
  for (let i = 0; i < draft.environment.length; i += 1) {
    const item = draft.environment[i]
    if (item === undefined) continue
    // 整行全空只当“还没填”的占位行跳过；填了一半的行必须报错——否则那一半
    // 会连同占位行一起被静默丢掉，人以为提交上去了。
    if (item.name.trim() === '' && item.value.trim() === '' && item.secret_ref.trim() === '') continue
    const envName = item.name.trim()
    if (!ENV_NAME_PATTERN.test(envName)) {
      return {
        field: `environment[${i}].name`,
        message: `第 ${i + 1} 条环境变量的名字必须是标识符（字母或下划线开头，只含字母/数字/下划线）`,
      }
    }
    if (envName.length > MAX_ENV_NAME_LENGTH) {
      return { field: `environment[${i}].name`, message: `第 ${i + 1} 条环境变量的名字太长` }
    }
    if (envNames.has(envName)) {
      return { field: `environment[${i}].name`, message: `环境变量 ${envName} 重复了` }
    }
    envNames.add(envName)
    if (item.kind === 'secret') {
      if (!SECRET_REF_PATTERN.test(item.secret_ref.trim())) {
        return {
          field: `environment[${i}].secret_ref`,
          message: `第 ${i + 1} 条要引用一条秘密（sec_ 加 32 位小写十六进制）；明文不能直接填在这里`,
        }
      }
    }
    // 公开值可以是空串（`FOO=` 在某些镜像里有意义），但不能是“没给”。
    // 草稿里“没给”与“给了空串”分不开，所以两种都按空串提交——这正是
    // 服务端 `value` 指针的语义：空串是一条明确的值。
  }

  if (draft.mounts.length > MAX_MOUNTS) {
    return { field: 'mounts', message: `挂载最多 ${MAX_MOUNTS} 条` }
  }
  const containerPaths = new Set<string>()
  for (let i = 0; i < draft.mounts.length; i += 1) {
    const mount = draft.mounts[i]
    if (mount === undefined) continue
    // 同上：整行全空是占位行，填了一半的要报错。
    if (
      mount.source_root_id.trim() === '' &&
      mount.relative_path.trim() === '' &&
      mount.container_path.trim() === ''
    ) {
      continue
    }
    if (!FILE_ROOT_ID_PATTERN.test(mount.source_root_id.trim())) {
      return {
        field: `mounts[${i}].source_root_id`,
        message: `第 ${i + 1} 条挂载要选一个受管理文件根；宿主绝对路径在这一层表达不出来`,
      }
    }
    const relative = mount.relative_path.trim()
    if (relative.startsWith('/')) {
      return { field: `mounts[${i}].relative_path`, message: '根内相对路径不能以 / 开头' }
    }
    if (relative.split('/').includes('..')) {
      return { field: `mounts[${i}].relative_path`, message: '根内相对路径不能包含 ..' }
    }
    const containerPath = mount.container_path.trim()
    if (!containerPath.startsWith('/')) {
      return { field: `mounts[${i}].container_path`, message: '容器内路径必须以 / 开头' }
    }
    if (containerPath === '/') {
      return { field: `mounts[${i}].container_path`, message: '不能把宿主目录挂到容器根目录上' }
    }
    if (containerPaths.has(containerPath)) {
      return { field: `mounts[${i}].container_path`, message: `容器内路径 ${containerPath} 重复了` }
    }
    containerPaths.add(containerPath)
    const blocked = BLOCKED_CONTAINER_PATHS.find(
      (item) => containerPath === item || containerPath.startsWith(`${item}/`),
    )
    if (blocked !== undefined) {
      return { field: `mounts[${i}].container_path`, message: `${blocked} 不允许挂进容器` }
    }
  }

  const memoryBytes = Math.round(draft.memory_limit_mib * MEBIBYTE)
  if (memoryBytes < MIN_MEMORY_LIMIT_BYTES) {
    return { field: 'memory_limit_mib', message: `内存上限至少 ${MIN_MEMORY_LIMIT_BYTES / MEBIBYTE} MiB` }
  }
  if (memoryBytes > MAX_MEMORY_LIMIT_BYTES) {
    return { field: 'memory_limit_mib', message: `内存上限最多 ${MAX_MEMORY_LIMIT_BYTES / MEBIBYTE} MiB` }
  }
  const cpuText = draft.cpu_limit.trim()
  if (!CPU_LIMIT_PATTERN.test(cpuText)) {
    return { field: 'cpu_limit', message: 'CPU 上限写成十进制小数（例如 0.5 或 2）' }
  }
  const cpu = Number(cpuText)
  if (!Number.isFinite(cpu) || cpu < MIN_CPU_LIMIT || cpu > MAX_CPU_LIMIT) {
    return { field: 'cpu_limit', message: `CPU 上限必须在 ${MIN_CPU_LIMIT}—${MAX_CPU_LIMIT} 核之间` }
  }

  const health = draft.health_check
  if (health.enabled) {
    if (!HEALTH_CHECK_KINDS.includes(health.kind)) {
      return { field: 'health_check.kind', message: '探测类型只能是 http 或 tcp' }
    }
    if (!Number.isInteger(health.port) || health.port < 0 || health.port > 65535) {
      return { field: 'health_check.port', message: '探测端口留 0 表示用容器端口' }
    }
    if (health.kind === 'http' && !health.path.trim().startsWith('/')) {
      return { field: 'health_check.path', message: 'http 探测路径必须以 / 开头' }
    }
    if (
      !Number.isInteger(health.interval_seconds) ||
      health.interval_seconds < MIN_HEALTH_INTERVAL_SECONDS ||
      health.interval_seconds > MAX_HEALTH_INTERVAL_SECONDS
    ) {
      return {
        field: 'health_check.interval_seconds',
        message: `探测间隔必须在 ${MIN_HEALTH_INTERVAL_SECONDS}—${MAX_HEALTH_INTERVAL_SECONDS} 秒之间`,
      }
    }
    if (
      !Number.isInteger(health.timeout_seconds) ||
      health.timeout_seconds < MIN_HEALTH_TIMEOUT_SECONDS ||
      health.timeout_seconds > MAX_HEALTH_TIMEOUT_SECONDS
    ) {
      return {
        field: 'health_check.timeout_seconds',
        message: `探测超时必须在 ${MIN_HEALTH_TIMEOUT_SECONDS}—${MAX_HEALTH_TIMEOUT_SECONDS} 秒之间`,
      }
    }
    if (!Number.isInteger(health.retries) || health.retries < 0 || health.retries > MAX_HEALTH_RETRIES) {
      return { field: 'health_check.retries', message: `探测重试最多 ${MAX_HEALTH_RETRIES} 次` }
    }
    if (
      !Number.isInteger(health.start_period_seconds) ||
      health.start_period_seconds < 0 ||
      health.start_period_seconds > MAX_HEALTH_START_SECONDS
    ) {
      return {
        field: 'health_check.start_period_seconds',
        message: `启动宽限期最多 ${MAX_HEALTH_START_SECONDS} 秒`,
      }
    }
  }

  if (draft.volume_dependencies.length > MAX_VOLUME_DEPENDENCIES) {
    return { field: 'volume_dependencies', message: `卷依赖最多 ${MAX_VOLUME_DEPENDENCIES} 条` }
  }
  const seenVolumes = new Set<string>()
  for (const volumeId of draft.volume_dependencies) {
    const trimmed = volumeId.trim()
    if (!VOLUME_ID_PATTERN.test(trimmed)) {
      return { field: 'volume_dependencies', message: `卷标识的形状是 vol_ 加 32 位小写十六进制（收到 ${trimmed}）` }
    }
    if (seenVolumes.has(trimmed)) {
      return { field: 'volume_dependencies', message: `卷 ${trimmed} 重复了` }
    }
    seenVolumes.add(trimmed)
  }

  return null
}

/**
 * 草稿 → 可提交的定义。
 *
 * 调用前先跑 `validateDeploymentDraft`：这里不再校验，只做搬运（含空条目过滤，
 * 因为空行是“还没填”，不是“要提交一条空记录”）。
 */
export function definitionFromDraft(draft: DeploymentDraft): ManagedDeploymentDefinition {
  const environment: ManagedDeploymentEnvVar[] = []
  for (const item of draft.environment) {
    const name = item.name.trim()
    if (name === '') continue
    if (item.kind === 'secret') {
      environment.push({ name, secret_ref: item.secret_ref.trim() })
    } else {
      environment.push({ name, value: item.value })
    }
  }

  const mounts: ManagedDeploymentMount[] = []
  for (const item of draft.mounts) {
    const sourceRootID = item.source_root_id.trim()
    const containerPath = item.container_path.trim()
    if (sourceRootID === '' && containerPath === '') continue
    mounts.push({
      source_root_id: sourceRootID,
      relative_path: item.relative_path.trim(),
      container_path: containerPath,
      read_only: item.read_only,
    })
  }

  const definition: ManagedDeploymentDefinition = {
    name: draft.name.trim(),
    image_reference: draft.image_reference.trim(),
    container_port: draft.container_port,
    host_bind_address: draft.host_bind_address.trim(),
    host_port: draft.host_port,
    resource_limits: {
      memory_limit_bytes: Math.round(draft.memory_limit_mib * MEBIBYTE),
      cpu_limit: draft.cpu_limit.trim(),
    },
    volume_dependencies: [...new Set(draft.volume_dependencies.map((item) => item.trim()))].filter(
      (item) => item !== '',
    ),
  }
  if (environment.length > 0) definition.environment = environment
  if (mounts.length > 0) definition.mounts = mounts

  const health = draft.health_check
  definition.health_check = health.enabled
    ? {
        enabled: true,
        kind: health.kind,
        port: health.port,
        path: health.kind === 'http' ? health.path.trim() : '',
        interval_seconds: health.interval_seconds,
        timeout_seconds: health.timeout_seconds,
        retries: health.retries,
        start_period_seconds: health.start_period_seconds,
      }
    : { enabled: false }

  return definition
}

/** 定义 → 草稿（详情页拿它预填“换个镜像”与“照着再来一份”的表单）。 */
export function draftFromDefinition(definition: ManagedDeploymentDefinition): DeploymentDraft {
  const base = emptyDeploymentDraft()
  const health = definition.health_check
  return {
    ...base,
    name: definition.name,
    image_reference: definition.image_reference,
    container_port: definition.container_port,
    host_bind_address: definition.host_bind_address,
    host_port: definition.host_port,
    environment: (definition.environment ?? []).map((item) => ({
      name: item.name,
      kind: item.secret_ref !== undefined && item.secret_ref !== '' ? 'secret' : 'value',
      value: item.value ?? '',
      secret_ref: item.secret_ref ?? '',
    })),
    mounts: (definition.mounts ?? []).map((item) => ({
      source_root_id: item.source_root_id,
      relative_path: item.relative_path ?? '',
      container_path: item.container_path,
      read_only: item.read_only,
    })),
    memory_limit_mib: Math.round(definition.resource_limits.memory_limit_bytes / MEBIBYTE),
    cpu_limit: definition.resource_limits.cpu_limit,
    health_check: {
      enabled: health?.enabled ?? false,
      kind: health?.kind ?? 'http',
      port: health?.port ?? 0,
      path: health?.path ?? '/',
      interval_seconds: health?.interval_seconds ?? base.health_check.interval_seconds,
      timeout_seconds: health?.timeout_seconds ?? base.health_check.timeout_seconds,
      retries: health?.retries ?? base.health_check.retries,
      start_period_seconds: health?.start_period_seconds ?? 0,
    },
    volume_dependencies: [...(definition.volume_dependencies ?? [])],
  }
}
