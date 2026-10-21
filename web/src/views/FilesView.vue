<script setup lang="ts">
// 文件页（P06.1 读取 + P06.2 写入）。
//
// 这一页的第一责任不是“好用”，而是**不撒谎**。PLAN.md 第 15 节与
// CONTRACTS.md 第 11 节把文件管理的范围限定得很死，页面必须把限定写出来：
//
//   1) 根来自服务端配置（GET /files/roots）。界面没有“输入路径”去找文件的框，
//      也不列服务端没登记的目录。没有登记根时写的是“未登记任何文件根”，
//      不是“浏览服务器”。这一条直接对应验收里那句
//      “文件 UI 不能显示‘可管理整个服务器所有目录’”。
//   2) 路径一律是根内相对路径。向上只能到根为止，前进只能靠点目录项；
//      改名/移动的目标是自己填的，但它是**根内相对路径**，由服务端在句柄上判定，
//      前端不做 `..` 归一化（那会掩盖一个本来该报错的输入）。
//   3) 符号链接不跟随。它们在列表里就是“符号链接”，点不进去，也不下载。
//   4) 写操作没有捷径：新建、改名、删除、权限、所有者、保存文本全部走
//      预检 → 确认 → 任务。界面不自己判断 allowed、不自己生成短语，
//      也不在预检之前预测结论。
//   5) **上传不等于落地**。上传把字节放进代理的暂存区，目标文件一个字节都没变；
//      要变成真文件必须再走 file.commit_upload。界面上这是两步、两句话，
//      不是一次“上传成功”。
//   6) 错误按 code 分支（PATH_OUTSIDE_ROOT / FILE_NOT_TEXT / ROOT_UNAVAILABLE /
//      VERSION_CONFLICT / NOT_IMPLEMENTED / MANAGED_ACCOUNTS_UNAVAILABLE），
//      不按 message 猜。

import { computed, onBeforeUnmount, onMounted, ref, shallowRef } from 'vue'
import { NAlert, NButton, NCard, NCheckbox, NInput, NModal, NSelect, NTag } from 'naive-ui'

import { ApiError } from '@/api/client'
import {
  DEFAULT_MAX_UPLOAD_BYTES,
  FILE_ACTIONS,
  FILE_ACTION_LABELS,
  MAX_TEXT_BYTES,
  baseName,
  breadcrumbs,
  canRead,
  chmodParams,
  chownParams,
  commitUploadParams,
  createFileUpload,
  deleteFileUpload,
  deleteParams,
  downloadFile,
  fetchFileList,
  fetchFileRoots,
  fetchFileText,
  fetchFileUpload,
  fileActionNeedsReauth,
  joinRelative,
  mkdirParams,
  moveParams,
  parentRelative,
  putFileUploadContent,
  uploadStateLabel,
  validMode,
  validRelativePath,
  writeTextParams,
  type FileAction,
  type FileEntry,
  type FileRoot,
  type FileText,
  type FileUpload,
} from '@/api/files'
import {
  JOB_STATUS_LABELS,
  isTerminal,
  jobErrorMessage,
  type JobDTO,
  type JobStatus,
} from '@/api/jobs'
import {
  createPreflight,
  fetchJobStatus,
  isPreflightExpired,
  newIdempotencyKey,
  preflightRemainingSeconds,
  submitJob,
  type PreflightData,
} from '@/api/preflights'
import ApiErrorPanel from '@/components/ApiErrorPanel.vue'
import FileActionConfirm from '@/components/FileActionConfirm.vue'
import FileEntryRow from '@/components/FileEntryRow.vue'
import StatePanel from '@/components/StatePanel.vue'
import { useSessionStore } from '@/stores/session'
import { formatBytes, formatTimestamp } from '@/utils/format'
import { apiPanelState } from '@/utils/viewstate'

type Phase = 'idle' | 'loading' | 'ready' | 'error'

/** 任务轮询：一次写动作在服务端是秒级，这里给 60 秒。 */
const JOB_POLL_MS = 1000
const JOB_POLL_LIMIT = 60

/** 对话框的两步：填表单 → 看预检并确认。 */
type DialogStep = 'form' | 'confirm'

/** 对话框处在哪个动作的表单上。 */
type DialogMode = 'mkdir' | 'writeText' | 'rename' | 'remove' | 'chmod' | 'chown' | 'upload'

/** 一次待提交的写动作。表单收集参数，预检之后才谈“能不能执行”。 */
type PendingAction =
  | { kind: 'mkdir'; path: string }
  | { kind: 'writeText'; path: string; content: string; version: string }
  | { kind: 'rename'; entry: FileEntry; target: string }
  | { kind: 'remove'; entry: FileEntry; recursive: boolean }
  | { kind: 'chmod'; entry: FileEntry; mode: string }
  | { kind: 'chown'; entry: FileEntry; accountId: number; groupId: number }
  | { kind: 'commitUpload'; upload: FileUpload; overwrite: boolean; version: string }

const session = useSessionStore()

// ---------------------------------------------------------------- 根

// 初始就是 loading：进页面第一件事就是读根，空白不比“正在读”更真实。
const rootsPhase = ref<Phase>('loading')
const rootsError = ref<ApiError | null>(null)
const roots = ref<FileRoot[]>([])
const rootId = ref<string | null>(null)

const rootOptions = computed(() =>
  roots.value.map((root) => ({
    label: root.available ? `${root.name}（${root.path}）` : `${root.name}（不可用）`,
    value: root.root_id,
    disabled: !root.available,
  })),
)

const activeRoot = computed<FileRoot | null>(
  () => roots.value.find((root) => root.root_id === rootId.value) ?? null,
)

/** 根可写才给写入口：只读根上的按钮点下去必然 403，画出来只是骗人。 */
const rootWritable = computed(() => activeRoot.value?.writable === true)

const rootsPanelState = computed(() => (rootsError.value ? apiPanelState(rootsError.value) : null))

const rootCapacityText = computed(() => {
  const root = activeRoot.value
  if (root === null) return ''
  if (root.capacity === null) {
    return root.capacity_error ? `容量未读出：${root.capacity_error}` : '容量未读出'
  }
  return `${formatBytes(root.capacity.available_bytes)} 可用 / ${formatBytes(
    root.capacity.total_bytes,
  )}（已用 ${root.capacity.used_percent.toFixed(1)}%）`
})

async function loadRoots(): Promise<void> {
  rootsPhase.value = 'loading'
  rootsError.value = null
  try {
    const result = await fetchFileRoots()
    roots.value = result.data.roots
    rootsPhase.value = 'ready'
    applyDefaultRoot()
  } catch (caught) {
    if (caught instanceof ApiError) {
      rootsError.value = caught
      rootsPhase.value = 'error'
      return
    }
    throw caught
  }
}

/** 默认选第一个可用根：页面一进来就有内容，比空白更容易理解。 */
function applyDefaultRoot(): void {
  // 根还在就保留当前选择（刷新时不要把人弹回别的地方）。
  if (rootId.value !== null && roots.value.some((root) => root.root_id === rootId.value)) return
  const first = roots.value.find((root) => root.available) ?? null
  rootId.value = first === null ? null : first.root_id
  path.value = ''
  if (rootId.value !== null) void loadList('', { replace: true })
}

function selectRoot(value: unknown): void {
  // 只接受字符串：选择器理论上只会回 root_id，但界面不替服务端猜类型。
  if (typeof value !== 'string') return
  if (value === rootId.value) return
  rootId.value = value
  path.value = ''
  resetBrowser()
  void loadList('', { replace: true })
}

/** 换根时要清掉上个根的残留（条目、游标、预览、未落地的上传）。 */
function resetBrowser(): void {
  listGeneration += 1
  entries.value = []
  nextCursor.value = null
  truncated.value = false
  listError.value = null
  listPhase.value = 'idle'
  selectedPath.value = null
  textPhase.value = 'idle'
  text.value = null
  textError.value = null
  versionConflict.value = false
  downloadMessage.value = ''
  downloadError.value = null
  pendingUpload.value = null
  closeDialog()
}

// ---------------------------------------------------------------- 目录列举

const path = ref('')
const listPhase = ref<Phase>('idle')
const listError = ref<ApiError | null>(null)
const entries = ref<FileEntry[]>([])
const nextCursor = ref<string | null>(null)
const truncated = ref(false)
const loadingMore = ref(false)

/** 请求代号：换目录/换根之后，先前那次慢响应不能再往界面上写。 */
let listGeneration = 0

const listPanelState = computed(() => (listError.value ? apiPanelState(listError.value) : null))
const crumbs = computed(() => breadcrumbs(path.value))
const parent = computed(() => parentRelative(path.value))

async function loadList(
  target: string,
  options: { replace?: boolean; cursor?: string } = {},
): Promise<void> {
  const root = rootId.value
  if (root === null) return
  const generation = listGeneration
  const append = options.replace !== true && options.cursor !== undefined
  try {
    if (append) loadingMore.value = true
    else listPhase.value = 'loading'
    const result = await fetchFileList({
      root_id: root,
      path: target,
      ...(options.cursor === undefined ? {} : { cursor: options.cursor }),
    })
    if (generation !== listGeneration) return
    const page = result.data
    entries.value = append ? [...entries.value, ...page.entries] : page.entries
    path.value = page.path
    nextCursor.value = page.next_cursor
    truncated.value = page.truncated
    listError.value = null
    listPhase.value = 'ready'
  } catch (caught) {
    if (generation !== listGeneration) return
    if (caught instanceof ApiError) {
      listError.value = caught
      // 翻页失败不要清掉已经看到的内容：那是用户此刻唯一还留着的东西。
      if (!append) {
        entries.value = []
        nextCursor.value = null
        truncated.value = false
      }
      listPhase.value = 'error'
      return
    }
    throw caught
  } finally {
    loadingMore.value = false
  }
}

function openDir(target: string): void {
  listGeneration += 1
  selectedPath.value = null
  textPhase.value = 'idle'
  text.value = null
  textError.value = null
  versionConflict.value = false
  void loadList(target, { replace: true })
}

function reload(): void {
  openDir(path.value)
}

function loadMore(): void {
  const cursor = nextCursor.value
  if (cursor === null || loadingMore.value) return
  void loadList(path.value, { cursor })
}

// ---------------------------------------------------------------- 文本预览

const selectedPath = ref<string | null>(null)
const textPhase = ref<Phase>('idle')
const textError = ref<ApiError | null>(null)
const text = ref<FileText | null>(null)
/** 服务端回 VERSION_CONFLICT：文件在点开之后被改过。 */
const versionConflict = ref(false)

const textPanelState = computed(() => (textError.value ? apiPanelState(textError.value) : null))

async function openFile(entry: FileEntry): Promise<void> {
  if (!canRead(entry)) return
  // selectedPath 存的是**根内完整相对路径**：预览、重读、下载都用它，
  // 不能只存文件名——那在子目录里会指向另一个文件。
  selectedPath.value = childPath(entry.name)
  text.value = null
  textError.value = null
  versionConflict.value = false
  downloadMessage.value = ''
  downloadError.value = null
  await readText(childPath(entry.name), entry.version)
}

/**
 * 读文本。
 *
 * expectedVersion 只在“拿着列表里的版本去读同一份文件”时才带：它能在
 * “列表与点开之间被换掉”时立刻发现。用户主动点“重新读取”时不带
 * （那是明确的“我要现在的内容”），服务端打开句柄仍会做全部路径约束检查。
 */
async function readText(target: string, expectedVersion?: string): Promise<void> {
  const root = rootId.value
  if (root === null) return
  textPhase.value = 'loading'
  textError.value = null
  try {
    const result = await fetchFileText({
      root_id: root,
      path: target,
      ...(expectedVersion === undefined || expectedVersion === ''
        ? {}
        : { expected_version: expectedVersion }),
    })
    text.value = result.data
    versionConflict.value = false
    textPhase.value = 'ready'
  } catch (caught) {
    if (caught instanceof ApiError) {
      textError.value = caught
      versionConflict.value = caught.code === 'VERSION_CONFLICT'
      textPhase.value = 'error'
      return
    }
    throw caught
  }
}

function reread(): void {
  const target = selectedPath.value
  if (target === null) return
  void readText(target)
}

function closeText(): void {
  selectedPath.value = null
  text.value = null
  textError.value = null
  versionConflict.value = false
  textPhase.value = 'idle'
}

// ---------------------------------------------------------------- 下载

const downloading = ref<string | null>(null)
const downloadError = ref<ApiError | null>(null)
const downloadMessage = ref('')

async function save(entry: FileEntry): Promise<void> {
  const root = rootId.value
  if (root === null || !canRead(entry)) return
  downloading.value = entry.name
  downloadError.value = null
  downloadMessage.value = ''
  try {
    const result = await downloadFile({
      root_id: root,
      path: childPath(entry.name),
      // 用列表给的版本：文件在列表之后被换掉时，宁可失败也不下载另一份内容。
      ...(entry.version === '' ? {} : { expected_version: entry.version }),
    })
    downloadMessage.value = `已开始保存 ${result.fileName}（${formatBytes(
      String(result.sizeBytes),
    )}）`
  } catch (caught) {
    if (caught instanceof ApiError) {
      downloadError.value = caught
      return
    }
    throw caught
  } finally {
    downloading.value = null
  }
}

/** 目录项 → 根内相对路径。只做拼接，不做 `..` 归一化（服务端不接受）。 */
function childPath(name: string): string {
  return joinRelative(path.value, name)
}

// ---------------------------------------------------------------- 写操作（P06.2）

const dialogOpen = ref(false)
const dialogStep = ref<DialogStep>('form')
const dialogMode = ref<DialogMode>('mkdir')
const pendingAction = ref<PendingAction | null>(null)
const preflight = ref<PreflightData | null>(null)
const actionBusy = ref(false)
const actionError = ref<string | null>(null)
const idempotencyKey = ref('')

// 表单字段（每次打开对话框重置）。
const formName = ref('')
const formTarget = ref('')
const formContent = ref('')
const formRecursive = ref(false)
const formMode = ref('')
const formAccountId = ref('')
const formGroupId = ref('')

// 任务进度。
const jobId = ref<string | null>(null)
const jobLabel = ref('')
const jobStatus = ref<JobStatus | null>(null)
const jobStep = ref('')
const jobMessage = ref<string | null>(null)
const jobOutcome = ref<string | null>(null)
/** 轮询本身失败：任务状态未知，去任务中心看。不是“任务失败”。 */
const jobPollFailed = ref(false)

// 上传：浏览器给的 File 对象不做深层响应式（它不可枚举，代理它没有意义）。
const uploadChosen = shallowRef<File | null>(null)
const uploadChosenSize = ref('')
const uploadFileName = ref('')
const uploadTargetPath = ref('')
const uploadPhase = ref<'idle' | 'uploading' | 'received' | 'error'>('idle')
const uploadError = ref<string | null>(null)

// 未落地的暂存上传（用户可能会关掉对话框，所以它挂在页面上而不是对话框里）。
const pendingUpload = ref<FileUpload | null>(null)

const nowMs = ref(Date.now())
let ticker: number | null = null
let disposed = false

const preflightExpired = computed(() =>
  preflight.value === null ? false : isPreflightExpired(preflight.value, new Date(nowMs.value)),
)
const preflightRemaining = computed(() =>
  preflight.value === null
    ? null
    : preflightRemainingSeconds(preflight.value, new Date(nowMs.value)),
)

/** 任务已经给出结论（或轮询失败，状态未知）。结论摆在对话框里，不随重画消失。 */
const jobFinished = computed(
  () => jobStatus.value !== null && (isTerminal(jobStatus.value) || jobPollFailed.value),
)
const jobStatusType = computed<'info' | 'success' | 'error' | 'warning'>(() => {
  if (jobPollFailed.value) return 'warning'
  switch (jobStatus.value) {
    case 'succeeded':
      return 'success'
    case 'failed':
    case 'canceled':
      return 'error'
    case 'interrupted':
      return 'warning'
    default:
      return 'info'
  }
})

const dialogTitle = computed(() => {
  if (dialogStep.value === 'confirm') {
    return pendingAction.value === null ? '确认操作' : actionLabel(pendingAction.value)
  }
  switch (dialogMode.value) {
    case 'mkdir':
      return '新建目录'
    case 'writeText':
      return '保存文本'
    case 'rename':
      return '改名 / 移动'
    case 'remove':
      return '删除'
    case 'chmod':
      return '修改权限'
    case 'chown':
      return '修改所有者'
    case 'upload':
      return '上传文件（先入暂存，不落地）'
    default:
      return '文件操作'
  }
})

const confirmTarget = computed(() => {
  const action = pendingAction.value
  if (action === null) return ''
  switch (action.kind) {
    case 'mkdir':
      return action.path
    case 'writeText':
      return action.path
    case 'rename':
      return `${action.entry.name} → ${action.target}`
    case 'remove':
      return action.entry.name
    case 'chmod':
      return `${action.entry.name}（${action.mode}）`
    case 'chown':
      return action.entry.name
    case 'commitUpload':
      return action.upload.path
    default:
      return ''
  }
})

const confirmDetail = computed(() => {
  const action = pendingAction.value
  if (action === null) return ''
  switch (action.kind) {
    case 'mkdir':
      return '在根内新建一个目录。已存在则拒绝，不会吞掉已有内容。'
    case 'writeText':
      return `用编辑器里的内容替换这个文件（${formatBytes(
        String(new TextEncoder().encode(action.content).length),
      )}）。服务端会先备份旧版本，再原子替换并读回核对。`
    case 'rename':
      return '同根内的改名/移动是一次 rename：要么旧名字还在，要么新名字在，不会出现“两边都没有”的中间态。目标已存在会被拒绝。'
    case 'remove':
      return action.recursive
        ? '递归删除：整棵目录树都会被删除，面板没有回收站。'
        : '删除这个条目。目录只有为空时才能非递归删除。'
    case 'chmod':
      return '只改普通权限位（setuid/setgid/sticky 一律拒绝）。执行后服务端会重新读回核对。'
    case 'chown':
      return '把所有者改成受管理账号/组。面板不接受任意 uid/gid。'
    case 'commitUpload':
      return action.overwrite
        ? '把暂存里的字节覆盖到已存在的目标路径。这一步才真正改文件，并且要确认短语 OVERWRITE。'
        : '把暂存里的字节落到目标路径。这一步才真正改文件；在此之前目标一个字节都没变。'
    default:
      return ''
  }
})

/**
 * 预检还没回来时，按动作表提前说一句“这一步要求再认证”。
 *
 * 这只是提示：真正的结论来自预检的 requires_reauth，
 * commit_upload 也只在覆盖时才要求再认证。
 */
const confirmNeedsReauthHint = computed(() => {
  const action = pendingAction.value
  if (action === null) return false
  if (action.kind === 'commitUpload') return action.overwrite
  return fileActionNeedsReauth(actionName(action))
})

const confirmDanger = computed(() => pendingAction.value?.kind === 'remove')

const uploadProgressText = computed(() => {
  const upload = pendingUpload.value
  if (upload !== null) {
    return `${formatBytes(upload.received_bytes)} / ${formatBytes(upload.size_bytes)}`
  }
  return uploadChosenSize.value === '' ? '' : formatBytes(uploadChosenSize.value)
})

const currentDirLabel = computed(() => (path.value === '' ? '/' : `/${path.value}`))

function startTicker(): void {
  if (typeof window === 'undefined' || ticker !== null) return
  ticker = window.setInterval(() => {
    nowMs.value = Date.now()
  }, 1000)
}

function stopTicker(): void {
  if (ticker !== null) {
    clearInterval(ticker)
    ticker = null
  }
}

function closeDialog(): void {
  if (actionBusy.value) return
  dialogOpen.value = false
  dialogStep.value = 'form'
  pendingAction.value = null
  preflight.value = null
  actionError.value = null
  idempotencyKey.value = ''
  jobId.value = null
  jobLabel.value = ''
  jobStatus.value = null
  jobStep.value = ''
  jobMessage.value = null
  jobOutcome.value = null
  jobPollFailed.value = false
  uploadChosen.value = null
  uploadChosenSize.value = ''
  uploadFileName.value = ''
  uploadPhase.value = 'idle'
  uploadError.value = null
  stopTicker()
}

function resetDialog(): void {
  formName.value = ''
  formTarget.value = ''
  formContent.value = ''
  formRecursive.value = false
  formMode.value = ''
  formAccountId.value = ''
  formGroupId.value = ''
  actionError.value = null
  preflight.value = null
  jobId.value = null
  jobLabel.value = ''
  jobStatus.value = null
  jobStep.value = ''
  jobMessage.value = null
  jobOutcome.value = null
  jobPollFailed.value = false
  dialogStep.value = 'form'
}

function actionName(action: PendingAction): FileAction {
  switch (action.kind) {
    case 'mkdir':
      return FILE_ACTIONS.mkdir
    case 'writeText':
      return FILE_ACTIONS.writeText
    case 'rename':
      return FILE_ACTIONS.move
    case 'remove':
      return FILE_ACTIONS.delete
    case 'chmod':
      return FILE_ACTIONS.chmod
    case 'chown':
      return FILE_ACTIONS.chown
    case 'commitUpload':
      return FILE_ACTIONS.commitUpload
    default:
      return FILE_ACTIONS.mkdir
  }
}

/** 待提交动作 → 动作名（用于标签与提示）。 */
function actionLabel(action: PendingAction): string {
  return FILE_ACTION_LABELS[actionName(action)]
}

/** 待提交动作 → 动作参数（字段名与 CONTRACTS 第 5 节逐字一致）。 */
function actionParams(action: PendingAction, root: string): Record<string, unknown> {
  switch (action.kind) {
    case 'mkdir':
      return mkdirParams({ root_id: root, relative_path: action.path })
    case 'writeText':
      return writeTextParams({
        root_id: root,
        relative_path: action.path,
        expected_version: action.version,
        content: action.content,
      })
    case 'rename':
      return moveParams({
        source_root_id: root,
        source_path: childPath(action.entry.name),
        target_root_id: root,
        target_path: action.target,
        expected_version: action.entry.version,
      })
    case 'remove':
      return deleteParams({
        root_id: root,
        relative_path: childPath(action.entry.name),
        expected_version: action.entry.version,
        recursive: action.recursive,
      })
    case 'chmod':
      return chmodParams({
        root_id: root,
        relative_path: childPath(action.entry.name),
        expected_version: action.entry.version,
        mode: action.mode,
      })
    case 'chown':
      return chownParams({
        root_id: root,
        relative_path: childPath(action.entry.name),
        expected_version: action.entry.version,
        managed_account_id: action.accountId,
        managed_group_id: action.groupId,
      })
    case 'commitUpload':
      return commitUploadParams({
        upload_id: action.upload.upload_id,
        overwrite: action.overwrite,
        expected_version: action.version,
      })
    default:
      return {}
  }
}

function openMkdir(): void {
  resetDialog()
  dialogMode.value = 'mkdir'
  dialogOpen.value = true
}

function openRename(entry: FileEntry): void {
  resetDialog()
  dialogMode.value = 'rename'
  formTarget.value = childPath(entry.name)
  pendingAction.value = { kind: 'rename', entry, target: formTarget.value }
  dialogOpen.value = true
}

function openRemove(entry: FileEntry): void {
  resetDialog()
  dialogMode.value = 'remove'
  formRecursive.value = false
  pendingAction.value = { kind: 'remove', entry, recursive: false }
  dialogOpen.value = true
}

function openChmod(entry: FileEntry): void {
  resetDialog()
  dialogMode.value = 'chmod'
  formMode.value = entry.mode
  pendingAction.value = { kind: 'chmod', entry, mode: entry.mode }
  dialogOpen.value = true
}

function openChown(entry: FileEntry): void {
  resetDialog()
  dialogMode.value = 'chown'
  pendingAction.value = { kind: 'chown', entry, accountId: 0, groupId: 0 }
  dialogOpen.value = true
}

/** 打开编辑器：先按列表版本读一次内容，读到了才有东西可改。 */
async function openEditor(entry: FileEntry): Promise<void> {
  resetDialog()
  dialogMode.value = 'writeText'
  dialogOpen.value = true
  actionBusy.value = true
  try {
    const target = childPath(entry.name)
    await readText(target, entry.version)
    if (textError.value !== null || text.value === null) {
      actionError.value = `无法读取内容：${textError.value?.message ?? '服务端未返回内容'}`
      return
    }
    formContent.value = text.value.content
    pendingAction.value = {
      kind: 'writeText',
      path: target,
      content: text.value.content,
      // 版本以这次读取为准：编辑器保存的是一个**读过的**版本，不是列表里那个。
      version: text.value.version,
    }
  } finally {
    actionBusy.value = false
  }
}

function openUpload(): void {
  resetDialog()
  dialogMode.value = 'upload'
  uploadChosen.value = null
  uploadChosenSize.value = ''
  uploadFileName.value = ''
  uploadTargetPath.value = ''
  uploadPhase.value = 'idle'
  uploadError.value = null
  dialogOpen.value = true
}

/** 选择文件后：目标路径默认放在当前目录下，可改（但必须是根内相对路径）。 */
function onPickFile(event: Event): void {
  const input = event.target as HTMLInputElement | null
  const file = input?.files?.[0] ?? null
  if (file === null || file === undefined) {
    uploadChosen.value = null
    uploadChosenSize.value = ''
    return
  }
  uploadChosen.value = file
  uploadChosenSize.value = String(file.size)
  uploadFileName.value = file.name
  uploadTargetPath.value = childPath(file.name)
  uploadPhase.value = 'idle'
  uploadError.value = null
}

/** 名字段校验：单段名字，不含 '/'、不为空、不是 . 或 .. */
function invalidNameProblem(name: string): string | null {
  if (name === '') return '名字不能为空'
  if (name.includes('/')) return '名字不能包含 /'
  if (name === '.' || name === '..') return `名字不能是 ${name}`
  if (new TextEncoder().encode(name).length > 255) return '名字超过 255 字节'
  return null
}

/**
 * 第一步：创建暂存 + 送字节。**目标文件不变。**
 *
 * 收完之后再查一次暂存记录：以服务端的 received_bytes 为准，
 * 不用本地“我以为传完了”。
 */
async function startUpload(): Promise<void> {
  const root = rootId.value
  const file = uploadChosen.value
  if (root === null || file === null) {
    actionError.value = '请先选择要上传的文件'
    return
  }
  const problem = invalidNameProblem(baseName(uploadTargetPath.value))
  if (problem !== null) {
    actionError.value = problem
    return
  }
  if (!validRelativePath(uploadTargetPath.value)) {
    actionError.value = '目标必须是根内相对路径（不能以 / 开头，不能含 . 或 .. 段）'
    return
  }
  actionBusy.value = true
  actionError.value = null
  uploadError.value = null
  uploadPhase.value = 'uploading'
  try {
    // 目标在当前目录里已经有同名条目 → 把它的版本报上去，服务端用它判断“我看的是哪一版”。
    const existing = entries.value.find((item) => item.name === baseName(uploadTargetPath.value))
    const created = await createFileUpload({
      root_id: root,
      path: uploadTargetPath.value,
      size_bytes: String(file.size),
      expected_version: existing?.version ?? '',
    })
    pendingUpload.value = created.data
    const put = await putFileUploadContent(created.data.upload_id, file)
    // 再查一次：PUT 的返回也是暂存记录，重查一次能确保我们看的是服务端此刻的记账。
    const refreshed = await fetchFileUpload(created.data.upload_id)
    pendingUpload.value = refreshed.data
    const state = refreshed.data.state
    uploadPhase.value = state === 'received' ? 'received' : 'error'
    if (state !== 'received') {
      const detail = refreshed.data.error ?? put.data.error
      uploadError.value = `暂存状态是 ${uploadStateLabel(state)}${
        detail === undefined ? '' : `（${detail}）`
      }：字节没有完整进入暂存区，目标文件未改变`
    }
  } catch (caught) {
    uploadPhase.value = 'error'
    uploadError.value = errorText(caught)
  } finally {
    actionBusy.value = false
  }
}

/** 第二步：把暂存里的字节落到目标路径（这才是“改文件”）。 */
function beginCommitUpload(): void {
  const upload = pendingUpload.value
  if (upload === null) return
  const existing = entries.value.find((item) => item.name === baseName(upload.path))
  // 只在“当前目录下同名条目确实存在”时报 overwrite=true。报低了不会丢数据
  // （服务端会用 blocker 拦住），报高了只是多要一次短语与再认证。
  const action: PendingAction = {
    kind: 'commitUpload',
    upload,
    overwrite: existing !== undefined,
    version: upload.expected_version ?? '',
  }
  pendingAction.value = action
  dialogMode.value = 'upload'
  dialogOpen.value = true
  void requestPreflight(action)
}

/** 放弃暂存：删掉暂存数据，目标文件不受影响。 */
async function discardUpload(): Promise<void> {
  const upload = pendingUpload.value
  if (upload === null) return
  actionBusy.value = true
  try {
    await deleteFileUpload(upload.upload_id)
    pendingUpload.value = null
    uploadPhase.value = 'idle'
    uploadError.value = null
    actionError.value = null
  } catch (caught) {
    actionError.value = errorText(caught)
  } finally {
    actionBusy.value = false
  }
}

/** 表单 → 预检。预检是只读的：它不改变系统状态，也不保证能提交。 */
async function requestPreflight(action: PendingAction): Promise<void> {
  const root = rootId.value
  if (root === null) return
  actionBusy.value = true
  actionError.value = null
  jobId.value = null
  jobLabel.value = ''
  jobStatus.value = null
  jobStep.value = ''
  jobMessage.value = null
  jobOutcome.value = null
  jobPollFailed.value = false
  // 幂等键在“一次意图”开始时生成，提交重试复用它。
  idempotencyKey.value = newIdempotencyKey()
  try {
    const result = await createPreflight(actionName(action), actionParams(action, root))
    preflight.value = result.data
    nowMs.value = Date.now()
    startTicker()
    dialogStep.value = 'confirm'
  } catch (caught) {
    actionError.value = errorText(caught)
  } finally {
    actionBusy.value = false
  }
}

/** 表单里的“下一步：预检”——先把表单值固化成参数，再请预检。 */
async function submitForm(): Promise<void> {
  const root = rootId.value
  if (root === null) return
  switch (dialogMode.value) {
    case 'mkdir': {
      const problem = invalidNameProblem(formName.value)
      if (problem !== null) {
        actionError.value = problem
        return
      }
      pendingAction.value = { kind: 'mkdir', path: joinRelative(path.value, formName.value) }
      break
    }
    case 'writeText': {
      const current = pendingAction.value
      if (current === null || current.kind !== 'writeText') return
      const bytes = new TextEncoder().encode(formContent.value).length
      if (bytes > MAX_TEXT_BYTES) {
        actionError.value = `内容 ${formatBytes(String(bytes))} 超过上限 ${formatBytes(
          String(MAX_TEXT_BYTES),
        )}，请下载后用其它工具编辑`
        return
      }
      pendingAction.value = { ...current, content: formContent.value }
      break
    }
    case 'rename': {
      const current = pendingAction.value
      if (current === null || current.kind !== 'rename') return
      if (!validRelativePath(formTarget.value)) {
        actionError.value = '目标必须是根内相对路径（不能以 / 开头，不能含 . 或 .. 段）'
        return
      }
      if (formTarget.value === childPath(current.entry.name)) {
        actionError.value = '目标与当前路径相同'
        return
      }
      pendingAction.value = { ...current, target: formTarget.value }
      break
    }
    case 'remove': {
      const current = pendingAction.value
      if (current === null || current.kind !== 'remove') return
      pendingAction.value = { ...current, recursive: formRecursive.value }
      break
    }
    case 'chmod': {
      const current = pendingAction.value
      if (current === null || current.kind !== 'chmod') return
      if (!validMode(formMode.value)) {
        actionError.value = '权限位必须是四位八进制（如 0644），只允许普通权限位'
        return
      }
      pendingAction.value = { ...current, mode: formMode.value }
      break
    }
    case 'chown': {
      const current = pendingAction.value
      if (current === null || current.kind !== 'chown') return
      const accountId = Number(formAccountId.value === '' ? '0' : formAccountId.value)
      const groupId = Number(formGroupId.value === '' ? '0' : formGroupId.value)
      if (!Number.isSafeInteger(accountId) || !Number.isSafeInteger(groupId)) {
        actionError.value = '账号/组 id 必须是整数'
        return
      }
      if (accountId < 0 || groupId < 0) {
        actionError.value = '账号/组 id 不能为负'
        return
      }
      if (accountId === 0 && groupId === 0) {
        actionError.value = '账号 id 与组 id 至少要给一个（0 表示“不改”）'
        return
      }
      pendingAction.value = { ...current, accountId, groupId }
      break
    }
    default:
      return
  }
  const action = pendingAction.value
  if (action === null) return
  await requestPreflight(action)
}

async function confirmAction(phrase: string): Promise<void> {
  const current = preflight.value
  const action = pendingAction.value
  if (current === null || action === null) return
  actionBusy.value = true
  actionError.value = null
  jobLabel.value = actionLabel(action)
  try {
    const submitted = await submitJob({
      preflightId: current.preflight_id,
      ...(phrase === '' ? {} : { confirmationPhrase: phrase }),
      idempotencyKey: idempotencyKey.value,
    })
    jobId.value = submitted.data.job_id
    jobStatus.value = 'queued'
    await trackJob(submitted.data.job_id)
  } catch (caught) {
    actionError.value = errorText(caught)
  } finally {
    actionBusy.value = false
  }
}

/**
 * 轮询任务直到终态。
 *
 * 终态是唯一的“结论”；中间状态只用来显示进度。轮询失败**不等于**任务失败，
 * 所以单独记在 jobPollFailed 里，措辞也是“状态未知，去任务中心看”。
 */
async function trackJob(id: string): Promise<void> {
  for (let attempt = 0; attempt < JOB_POLL_LIMIT; attempt += 1) {
    if (disposed) return
    try {
      const result = await fetchJobStatus(id)
      jobStatus.value = result.data.status
      jobStep.value = result.data.step
      if (isTerminal(result.data.status)) {
        applyJobResult(result.data)
        stopTicker()
        await afterAction(result.data)
        return
      }
    } catch (caught) {
      jobPollFailed.value = true
      jobMessage.value = `无法读取任务状态：${errorText(
        caught,
      )}。任务可能仍在执行，请到任务中心核对。`
      return
    }
    await sleep(JOB_POLL_MS)
  }
  jobPollFailed.value = true
  jobMessage.value = '任务在 60 秒内没有到达终态。它可能仍在执行，请到任务中心核对。'
}

function applyJobResult(job: JobDTO): void {
  if (job.status !== 'succeeded') {
    const detail = jobErrorMessage(job)
    const message = detail?.message ?? '任务失败（代理未提供说明）'
    jobMessage.value = detail?.hint ? `${message}（${detail.hint}）` : message
    return
  }
  const observed = job.result['observed_state']
  if (observed !== null && typeof observed === 'object') {
    const state = observed as Record<string, unknown>
    const type = typeof state['type'] === 'string' ? state['type'] : ''
    if (state['exists'] === false) {
      jobOutcome.value = '执行后读回：目标已不存在（删除已完成）。'
      return
    }
    if (type !== '') {
      jobOutcome.value = `执行后读回：类型 ${type}，版本 ${String(state['version'] ?? '未知')}。`
      return
    }
  }
  jobOutcome.value = '任务已成功；已重新读取当前目录。'
}

/** 动作结束后刷新这一页：结果以服务端重新读到的为准，不拿任务回执当真相。 */
async function afterAction(job: JobDTO): Promise<void> {
  const action = pendingAction.value
  if (job.status === 'succeeded' && action !== null) {
    if (action.kind === 'commitUpload') {
      pendingUpload.value = null
      uploadPhase.value = 'idle'
    }
    // 被删除/改名/覆盖的文件如果正开着预览，那份内容已经不成立：关掉它。
    if (
      (action.kind === 'remove' || action.kind === 'rename') &&
      selectedPath.value === childPath(action.entry.name)
    ) {
      closeText()
    }
  }
  await loadList(path.value, { replace: true })
}

function errorText(caught: unknown): string {
  if (caught instanceof ApiError) return caught.message
  if (caught instanceof RangeError) return caught.message
  return '请求失败，请稍后重试'
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => {
    setTimeout(resolve, ms)
  })
}

onMounted(() => {
  if (session.isAdmin) void loadRoots()
})

onBeforeUnmount(() => {
  disposed = true
  stopTicker()
})
</script>

<template>
  <section class="files">
    <div class="files__head">
      <h1 class="files__title">文件</h1>
      <NTag v-if="activeRoot" size="small" type="info">{{ activeRoot.name }}</NTag>
      <NTag v-if="activeRoot && !activeRoot.writable" size="small">只读根</NTag>
    </div>
    <p class="files__meta">
      只能访问服务端登记的文件根；路径是根内相对路径，符号链接不跟随。
      写操作（新建、改名、删除、权限、所有者、保存文本）全部走预检 → 任务；
      上传只把字节放进暂存区，落地是单独一步。
    </p>

    <StatePanel
      v-if="!session.isAdmin"
      state="forbidden"
      title="没有权限访问文件"
      description="文件接口对管理员开放。只读账号可以做系统概览、存储与服务查看，但文件内容不在其范围内。"
    />

    <template v-else>
      <StatePanel v-if="rootsPhase === 'loading'" state="loading" title="正在读取文件根" />
      <StatePanel
        v-else-if="rootsPanelState && rootsError && rootsPanelState !== 'error'"
        :state="rootsPanelState"
        :description="rootsError.message"
        :code="rootsError.code"
        :request-id="rootsError.requestId"
        @retry="loadRoots"
      />
      <ApiErrorPanel
        v-else-if="rootsError"
        :error="rootsError"
        title="无法读取文件根"
        @retry="loadRoots"
      />

      <StatePanel
        v-else-if="rootsPhase === 'ready' && roots.length === 0"
        state="empty"
        title="未登记任何文件根"
        description="面板只管理配置中登记过的目录。请在代理配置的 agent.files.roots 里登记根，重启代理后回到本页。"
      />

      <template v-else-if="rootsPhase === 'ready' && activeRoot">
        <NCard size="small" title="文件根">
          <div class="files__root">
            <label class="files__field">
              <span class="files__label">选择根</span>
              <NSelect :value="rootId" :options="rootOptions" @update:value="selectRoot" />
            </label>
            <div class="files__rootinfo">
              <p class="files__line">
                绝对路径 <code>{{ activeRoot.path }}</code>
                <span v-if="activeRoot.mount_point">
                  · 挂载点 <code>{{ activeRoot.mount_point }}</code>
                </span>
                <span v-if="activeRoot.fs_type"> · {{ activeRoot.fs_type }}</span>
                <NTag size="small" :type="activeRoot.writable ? 'warning' : 'default'">
                  {{ activeRoot.writable ? '配置为可写' : '只读（不提供写入口）' }}
                </NTag>
              </p>
              <p class="files__meta">{{ rootCapacityText }}</p>
            </div>
          </div>
          <NAlert
            v-if="!activeRoot.available"
            type="error"
            :bordered="false"
            title="这个根当前不可用"
          >
            {{ activeRoot.unavailable_reason || '服务端未说明原因' }}。
            目录被替换、卸载或身份变化都会导致根不可用，面板不会退回到按路径前缀判断。
          </NAlert>
          <NAlert v-else-if="activeRoot.unavailable_reason" type="warning" :bordered="false">
            {{ activeRoot.unavailable_reason }}
          </NAlert>
        </NCard>

        <NCard size="small" title="浏览">
          <div class="files__bar">
            <NButton size="small" :disabled="parent === null" @click="openDir(parent ?? '')">
              上一级
            </NButton>
            <nav class="files__crumbs" aria-label="路径">
              <template v-for="(crumb, index) in crumbs" :key="crumb.path">
                <span v-if="index > 0" class="files__sep">/</span>
                <NButton
                  text
                  size="small"
                  :disabled="crumb.path === path"
                  @click="openDir(crumb.path)"
                >
                  {{ index === 0 ? activeRoot.name : crumb.label }}
                </NButton>
              </template>
            </nav>
            <NButton size="small" quaternary :disabled="listPhase === 'loading'" @click="reload">
              刷新
            </NButton>
            <span v-if="rootWritable" class="files__ops">
              <NButton size="small" @click="openMkdir">新建目录</NButton>
              <NButton size="small" @click="openUpload">上传文件</NButton>
            </span>
            <span class="files__meta">
              当前 <code>{{ currentDirLabel }}</code>
            </span>
          </div>

          <StatePanel v-if="listPhase === 'loading'" state="loading" title="正在列出目录" />
          <StatePanel
            v-else-if="listPanelState && listError && listPanelState !== 'error'"
            :state="listPanelState"
            :description="listError.message"
            :code="listError.code"
            :request-id="listError.requestId"
            @retry="reload"
          />
          <ApiErrorPanel
            v-else-if="listError"
            :error="listError"
            title="无法列出目录"
            @retry="reload"
          />

          <NAlert v-if="truncated" type="warning" :bordered="false">
            这个目录的条目太多，服务端只扫描了其中一部分。未列出的条目不代表不存在。
          </NAlert>

          <StatePanel
            v-if="listPhase === 'ready' && entries.length === 0"
            state="empty"
            title="这个目录是空的"
            description="没有可列的条目。若是权限不足导致读不到内容，服务端会返回错误而不是空目录。"
          />

          <table v-if="entries.length > 0" class="files__table">
            <caption class="files__caption">
              共 {{ entries.length }} 项；大小与权限位来自服务端在同一次读取中的快照
            </caption>
            <thead>
              <tr>
                <th scope="col">名称</th>
                <th scope="col">类型</th>
                <th scope="col">大小</th>
                <th scope="col">权限</th>
                <th scope="col">属主</th>
                <th scope="col">修改时间</th>
                <th scope="col">操作</th>
              </tr>
            </thead>
            <tbody>
              <FileEntryRow
                v-for="entry in entries"
                :key="entry.name"
                :entry="entry"
                :downloading="downloading === entry.name"
                :can-write="rootWritable"
                @enter="(item) => openDir(childPath(item.name))"
                @open="openFile"
                @save="save"
                @edit="openEditor"
                @rename="openRename"
                @remove="openRemove"
                @chmod="openChmod"
                @chown="openChown"
              />
            </tbody>
          </table>

          <div v-if="nextCursor" class="files__more">
            <NButton size="small" :loading="loadingMore" @click="loadMore">加载更多</NButton>
            <span class="files__meta">一页最多 {{ entries.length }} 项，说明后面还有条目</span>
          </div>
        </NCard>

        <NCard v-if="pendingUpload" size="small" title="未落地的暂存上传">
          <p class="files__line">
            <code>{{ pendingUpload.upload_id }}</code> → <code>{{ pendingUpload.path }}</code>
            <NTag size="small" :type="pendingUpload.state === 'failed' ? 'error' : 'warning'">
              {{ uploadStateLabel(pendingUpload.state) }}
            </NTag>
          </p>
          <p class="files__meta">
            字节已经在代理的暂存区里（{{ uploadProgressText }}），但
            <strong>目标文件一个字节都没变</strong>。落地要单独执行 file.commit_upload；
            放弃只删除暂存，不影响任何文件。暂存到期时间
            {{ formatTimestamp(pendingUpload.expires_at) }}。
          </p>
          <div class="files__ops">
            <NButton
              v-if="pendingUpload.state === 'received'"
              size="small"
              type="primary"
              :loading="actionBusy"
              @click="beginCommitUpload"
            >
              落地上传
            </NButton>
            <NButton size="small" secondary :loading="actionBusy" @click="discardUpload">
              放弃这次上传
            </NButton>
          </div>
        </NCard>

        <NAlert v-if="downloadError" type="error" :bordered="false">
          下载失败：{{ downloadError.message }}（{{ downloadError.code }}）
        </NAlert>
        <NAlert v-else-if="downloadMessage" type="info" :bordered="false">
          {{ downloadMessage }}
        </NAlert>

        <NCard v-if="selectedPath !== null" size="small">
          <template #header>
            <span>预览 {{ selectedPath }}</span>
          </template>
          <template #header-extra>
            <NButton size="tiny" quaternary @click="closeText">关闭</NButton>
          </template>

          <StatePanel v-if="textPhase === 'loading'" state="loading" title="正在读取文件" />
          <template v-else-if="textPhase === 'error' && textError">
            <NAlert v-if="versionConflict" type="warning" :bordered="false" title="文件已改变">
              点开之后这个文件被改过（内容、长度或修改时间不一致），服务端拒绝按旧版本返回。
              下面重读会取当前内容。
            </NAlert>
            <StatePanel
              v-else-if="textPanelState && textPanelState !== 'error'"
              :state="textPanelState"
              :description="textError.message"
              :code="textError.code"
              :request-id="textError.requestId"
            />
            <ApiErrorPanel v-else :error="textError" :title="`无法读取 ${selectedPath}`" />
            <div class="files__ops">
              <NButton size="small" type="primary" @click="reread">重新读取</NButton>
              <NButton size="small" quaternary @click="closeText">关闭</NButton>
            </div>
          </template>
          <template v-else-if="text">
            <p class="files__line">
              {{ formatBytes(text.size_bytes) }} · {{ text.encoding }} · 版本
              <code>{{ text.version }}</code>
            </p>
            <pre class="files__preview">{{ text.content }}</pre>
            <p class="files__meta">
              这是服务端在同一份已打开的句柄上读出的完整内容（上限
              {{ formatBytes(String(MAX_TEXT_BYTES)) }}）。超过上限的文件请下载后查看。
            </p>
          </template>
        </NCard>
      </template>
    </template>

    <NModal
      v-model:show="dialogOpen"
      preset="card"
      :title="dialogTitle"
      class="files__dialog"
      :mask-closable="!actionBusy"
      @close="closeDialog"
    >
      <template #header-extra>
        <NButton size="tiny" quaternary :disabled="actionBusy" @click="closeDialog">关闭</NButton>
      </template>

      <p class="files__meta">当前目录 <code>{{ currentDirLabel }}</code></p>

      <template v-if="jobFinished">
        <NAlert v-if="jobMessage" type="error" :bordered="false">{{ jobMessage }}</NAlert>
        <NAlert v-else-if="jobOutcome" type="success" :bordered="false">{{ jobOutcome }}</NAlert>
        <p v-if="jobId" class="files__meta">
          任务 <code>{{ jobId }}</code> · {{ jobLabel }} ·
          {{ jobStatus === null ? '状态未知' : JOB_STATUS_LABELS[jobStatus] }}
        </p>
        <div class="files__ops">
          <NButton size="small" type="primary" @click="closeDialog">完成</NButton>
        </div>
      </template>

      <template v-else-if="dialogStep === 'form'">
        <div v-if="dialogMode === 'mkdir'" class="files__form">
          <label class="files__field">
            <span class="files__label">目录名（单段，不含 /）</span>
            <NInput v-model:value="formName" placeholder="例如 logs" />
          </label>
        </div>

        <div v-else-if="dialogMode === 'writeText'" class="files__form">
          <label class="files__field">
            <span class="files__label">内容（UTF-8，上限 1 MiB）</span>
            <NInput
              v-model:value="formContent"
              type="textarea"
              :autosize="{ minRows: 12, maxRows: 24 }"
            />
          </label>
          <p class="files__meta">
            保存前服务端先备份旧版本，再原子替换，最后重新打开读回核对大小与摘要。
            版本冲突（别人在这之后改过）会被拒绝——那是保护，不是故障。
          </p>
        </div>

        <div v-else-if="dialogMode === 'rename'" class="files__form">
          <label class="files__field">
            <span class="files__label">新的根内相对路径</span>
            <NInput v-model:value="formTarget" placeholder="例如 conf/app.conf" />
          </label>
          <p class="files__meta">
            同根内的改名/移动是一次 rename：不会出现“两边都没有”的中间态。目标已存在会被拒绝。
          </p>
        </div>

        <div v-else-if="dialogMode === 'remove'" class="files__form">
          <NCheckbox v-model:checked="formRecursive">
            递归删除整个目录树（需要确认短语 DELETE）
          </NCheckbox>
          <p class="files__meta">
            非递归删除只对文件与空目录有效。删除是永久操作：面板没有回收站；
            编辑备份只保存“被覆盖前的旧版本”，不是删除的备份。
          </p>
        </div>

        <div v-else-if="dialogMode === 'chmod'" class="files__form">
          <label class="files__field">
            <span class="files__label">权限位（四位八进制）</span>
            <NInput v-model:value="formMode" placeholder="0644" />
          </label>
          <p class="files__meta">
            只接受普通权限位：setuid/setgid/sticky 一律拒绝——它们改变的是执行语义。
          </p>
        </div>

        <div v-else-if="dialogMode === 'chown'" class="files__form">
          <label class="files__field">
            <span class="files__label">受管理账号 id（0 表示不改）</span>
            <NInput v-model:value="formAccountId" placeholder="例如 1" />
          </label>
          <label class="files__field">
            <span class="files__label">受管理组 id（0 表示不改）</span>
            <NInput v-model:value="formGroupId" placeholder="例如 1" />
          </label>
          <p class="files__meta">
            面板引用的是受管理账号/组的 id，不是任意 uid/gid。账号模块尚未落地时，
            预检会以 <code>MANAGED_ACCOUNTS_UNAVAILABLE</code> 阻塞这次操作——
            这是诚实的结果，不是故障。
          </p>
        </div>

        <div v-else-if="dialogMode === 'upload'" class="files__form">
          <label class="files__field">
            <span class="files__label">选择文件</span>
            <input type="file" data-upload-input @change="onPickFile" />
          </label>
          <label class="files__field">
            <span class="files__label">目标路径（根内相对路径）</span>
            <NInput v-model:value="uploadTargetPath" placeholder="例如 logs/app.log" />
          </label>
          <p v-if="uploadFileName !== ''" class="files__line">
            {{ uploadFileName }} · {{ uploadProgressText }}
            <NTag v-if="uploadPhase === 'uploading'" size="small" type="info">正在送入暂存区</NTag>
            <NTag v-else-if="uploadPhase === 'received'" size="small" type="warning">
              已入暂存，尚未落地
            </NTag>
            <NTag v-else-if="uploadPhase === 'error'" size="small" type="error">传输失败</NTag>
          </p>
          <p class="files__meta">
            单次上限默认 {{ formatBytes(String(DEFAULT_MAX_UPLOAD_BYTES)) }}（以服务端配置为准）。
            上传只把字节放进暂存区，目标文件一个字节都不会变；落地是上传完成之后的单独一步。
          </p>
          <NAlert v-if="uploadError" type="error" :bordered="false">{{ uploadError }}</NAlert>
          <div v-if="uploadPhase === 'received'" class="files__ops">
            <NButton size="small" type="primary" @click="beginCommitUpload">
              下一步：落地上传
            </NButton>
            <NButton size="small" secondary :loading="actionBusy" @click="discardUpload">
              放弃这次上传
            </NButton>
          </div>
        </div>

        <NAlert v-if="actionError" type="error" :bordered="false">{{ actionError }}</NAlert>

        <div class="files__ops">
          <NButton
            v-if="dialogMode === 'upload'"
            size="small"
            type="primary"
            :loading="actionBusy"
            :disabled="uploadPhase === 'uploading' || uploadPhase === 'received'"
            @click="startUpload"
          >
            开始上传
          </NButton>
          <NButton v-else size="small" type="primary" :loading="actionBusy" @click="submitForm">
            下一步：预检
          </NButton>
        </div>
      </template>

      <template v-else>
        <FileActionConfirm
          :action-label="pendingAction ? actionLabel(pendingAction) : '操作'"
          :target-label="confirmTarget"
          :detail="confirmDetail"
          :preflight="preflight"
          :busy="actionBusy"
          :error-message="actionError"
          :expired="preflightExpired"
          :remaining-seconds="preflightRemaining"
          :needs-reauth-hint="confirmNeedsReauthHint"
          :danger="confirmDanger"
          @confirm="confirmAction"
          @cancel="dialogStep = 'form'"
        />

        <NAlert v-if="jobStatus" :type="jobStatusType" :bordered="false">
          任务 {{ jobId }}：{{ JOB_STATUS_LABELS[jobStatus] }}
          <template v-if="jobStep !== ''"> · {{ jobStep }}</template>
        </NAlert>
        <NAlert v-if="jobMessage" type="error" :bordered="false">{{ jobMessage }}</NAlert>
      </template>
    </NModal>
  </section>
</template>

<style scoped>
.files {
  display: flex;
  flex-direction: column;
  gap: 16px;
  max-width: 1200px;
}

.files__head {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 10px;
}

.files__title {
  margin: 0;
  font-size: 20px;
}

.files__meta {
  font-size: 13px;
  opacity: 0.8;
  margin: 0;
  line-height: 1.7;
}

.files__line {
  margin: 0;
  line-height: 2;
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 8px;
}

.files__root {
  display: grid;
  grid-template-columns: minmax(220px, 320px) 1fr;
  gap: 16px;
  align-items: start;
}

.files__field {
  display: flex;
  flex-direction: column;
  gap: 4px;
}

.files__form {
  display: flex;
  flex-direction: column;
  gap: 10px;
  margin: 10px 0;
}

.files__label {
  font-size: 12px;
  opacity: 0.8;
}

.files__rootinfo {
  display: flex;
  flex-direction: column;
  gap: 4px;
  font-size: 13px;
}

.files__bar {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 10px;
  margin-bottom: 10px;
}

.files__crumbs {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 2px;
}

.files__sep {
  opacity: 0.5;
}

.files__table {
  width: 100%;
  border-collapse: collapse;
  font-size: 13px;
}

.files__caption {
  text-align: left;
  font-size: 12px;
  opacity: 0.75;
  padding-bottom: 6px;
}

.files__table th,
.files__table td {
  text-align: left;
  padding: 6px 8px;
  border-bottom: 1px solid rgba(128, 128, 128, 0.25);
  vertical-align: top;
}

.files__table th {
  font-weight: 600;
  font-size: 12px;
  opacity: 0.85;
}

.files__ops {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 8px;
  margin-top: 8px;
}

.files__more {
  display: flex;
  align-items: center;
  gap: 10px;
  margin-top: 10px;
}

.files__preview {
  max-height: 60vh;
  overflow: auto;
  margin: 10px 0 0;
  padding: 12px;
  border-radius: 4px;
  background: rgba(128, 128, 128, 0.12);
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  font-size: 12px;
  line-height: 1.6;
  white-space: pre;
}

.files__dialog {
  max-width: 760px;
  width: calc(100vw - 32px);
}

@media (max-width: 720px) {
  .files__root {
    grid-template-columns: 1fr;
  }

  .files__table {
    display: block;
    overflow-x: auto;
  }
}
</style>
