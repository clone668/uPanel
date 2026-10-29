<script setup lang="ts">
// 备份页（P12.5）。
//
// 这一页把"备份"拆成四块，因为它们是四件不同的事：
//   1. 仓库：这份备份存在哪（本机文件根 / SFTP）、用哪份秘密；登记只是"记下它在哪"，
//      真的在目标位置建出仓库是 backup.repository.initialize 的预检 → 任务。
//   2. 策略：备份什么、停不停应用、留多少份。策略不引用秘密，所以登记与修改只要 CSRF。
//   3. 快照：某个仓库里此刻有哪些快照。**列表不含大小**——每条都要跑一次 restic stats，
//      那是分钟级的代价；要看大小与文件数要点开详情。这一页把这件事写在明面上。
//   4. 恢复/验证/清理：三个动作，每个都是预检 → 任务，没有第二条执行路径。
//
// 三条不肯含糊的口径：
//   - 备份成功 ≠ 恢复已验证。run 的核验是"写完后读回来对得上"；要证明"这份数据读得回来"
//     只能单独做一次 backup.verify，而且要勾上抽样恢复。
//   - 清理真的删数据。它是这一页唯一不可逆的动作，所以给 irreversible 的二次确认。
//   - 仓库定义、策略、快照都读代理里的那一份。代理不在时整组 503，不是空列表。

import { computed, onBeforeUnmount, onMounted, reactive, ref } from 'vue'
import {
  NAlert,
  NButton,
  NCard,
  NCheckbox,
  NInput,
  NSelect,
  NTag,
} from 'naive-ui'

import { reauthPassword } from '@/api/auth'
import {
  BACKUP_INITIALIZE_ACTION,
  BACKUP_PRUNE_ACTION,
  BACKUP_RESTORE_ACTION,
  BACKUP_RUN_ACTION,
  BACKUP_VERIFY_ACTION,
  BACKUP_CONSISTENCY_LABELS,
  BACKUP_CONSISTENCY_MODES,
  backupInitializeParams,
  backupPruneParams,
  backupRestoreParams,
  backupRunParams,
  backupVerifyParams,
  createBackupPolicy,
  createBackupRepository,
  deleteBackupPolicy,
  fetchBackupPolicies,
  fetchBackupRepositories,
  fetchBackupSnapshot,
  fetchBackupSnapshots,
  locationLabel,
  retentionLabel,
  sortSnapshotsNewestFirst,
  sourceLabel,
  updateBackupPolicy,
  updateBackupRepository,
  type BackupPolicy,
  type BackupRepository,
  type BackupSnapshot,
} from '@/api/backup'
import { ApiError } from '@/api/client'
import { fetchFileRoots, type FileRoot } from '@/api/files'
import {
  backendOptions,
  backupRunConfirmLines,
  buildPolicyInput,
  buildRepositoryInput,
  emptyPolicyForm,
  emptyRepositoryForm,
  initializeConfirmLines,
  policyFormOf,
  pruneConfirmLines,
  repositoryFormOf,
  restoreConfirmLines,
  validatePolicyForm,
  validateRepositoryForm,
  verifyConfirmLines,
  type PolicyFormState,
  type RepositoryFormState,
} from '@/components/backup'
import ApiErrorPanel from '@/components/ApiErrorPanel.vue'
import SecondFactorField from '@/components/SecondFactorField.vue'
import StatePanel from '@/components/StatePanel.vue'
import StorageActionDialog from '@/components/StorageActionDialog.vue'
import { newIdempotencyKey } from '@/api/preflights'
import { useSessionStore } from '@/stores/session'
import { formatBytes, formatTimestamp } from '@/utils/format'
import { summarizeBackupJob } from '@/utils/jobresult'
import { createPoller, type Poller } from '@/utils/polling'
import { apiPanelState, retryablePanelState } from '@/utils/viewstate'

type Phase = 'loading' | 'ready' | 'error'

const DEFAULT_INTERVAL_MS = 30_000

const session = useSessionStore()
const phase = ref<Phase>('loading')
const error = ref<ApiError | null>(null)

const repositories = ref<BackupRepository[]>([])
const policies = ref<BackupPolicy[]>([])
const roots = ref<FileRoot[]>([])

// ---------------------------------------------------------------- 仓库表单

const repoFormOpen = ref(false)
const editingRepo = ref<BackupRepository | null>(null)
const repoForm = reactive<RepositoryFormState>(emptyRepositoryForm())
const repoTouched = ref(false)
const repoBusy = ref(false)
const repoMessage = ref<string | null>(null)
const repoProblems = computed(() => validateRepositoryForm(repoForm))
const repoNeedsReauth = ref(false)
const reauthPasswordInput = ref('')
// 启用二次验证的账号要在这一步带上六位数，否则 /auth/reauth 会 401 并要求第二因子。
const reauthCode = ref('')
/** 待重试的那一次保存（再认证成功后用它重发，不重新拼参数）。 */
let pendingRepoWrite: { input: ReturnType<typeof buildRepositoryInput>; key: string } | null = null

const rootOptions = computed(() =>
  roots.value.map((root) => ({
    label: `${root.name} · ${root.path}${root.available ? '' : '（此刻读不到）'}`,
    value: root.root_id,
  })),
)

// ---------------------------------------------------------------- 策略表单

const policyFormOpen = ref(false)
const editingPolicy = ref<BackupPolicy | null>(null)
const policyForm = reactive<PolicyFormState>(emptyPolicyForm())
const policyTouched = ref(false)
const policyBusy = ref(false)
const policyMessage = ref<string | null>(null)
const policyProblems = computed(() => validatePolicyForm(policyForm))

const repositoryOptions = computed(() =>
  repositories.value.map((repo) => ({
    label: `${repo.name}（${locationLabel(repo)}）`,
    value: repo.id,
  })),
)

const consistencyOptions = BACKUP_CONSISTENCY_MODES.map((mode) => ({
  label: BACKUP_CONSISTENCY_LABELS[mode],
  value: mode,
}))

// ---------------------------------------------------------------- 快照

const snapshotRepoId = ref('')
const snapshotPolicyId = ref('')
const snapshotLoading = ref(false)
const snapshotError = ref<string | null>(null)
const snapshots = ref<BackupSnapshot[]>([])
const snapshotCheckedAt = ref('')
const sizeIncluded = ref(false)
const snapshotRegistered = ref(true)
const selectedSnapshot = ref<BackupSnapshot | null>(null)
const snapshotDetailLoading = ref(false)
const snapshotDetailError = ref<string | null>(null)
const detailSize = ref<string | null>(null)
const detailFiles = ref<string | null>(null)
const detailVerifiedAt = ref<string | null>(null)

const restoreRootId = ref('')
const restoreRelativePath = ref('')
const restoreOverwrite = ref(false)
const verifySampleRestore = ref(true)

const snapshotPolicies = computed(() =>
  policies.value.filter((policy) => policy.repository_id === snapshotRepoId.value),
)

// ---------------------------------------------------------------- 动作框

interface ActionDialogState {
  action: string
  title: string
  params: Record<string, unknown>
  lines: string[]
  irreversible: boolean
  needsReauth: boolean
}

const actionDialog = ref<ActionDialogState | null>(null)
const actionShow = ref(false)

function openAction(state: ActionDialogState): void {
  actionDialog.value = state
  actionShow.value = true
}

// ---------------------------------------------------------------- 取数

const panelState = computed(() => (error.value ? apiPanelState(error.value) : null))

async function load(signal?: AbortSignal): Promise<void> {
  try {
    const [repoResult, policyResult] = await Promise.all([
      fetchBackupRepositories(signal),
      fetchBackupPolicies(signal),
    ])
    if (signal?.aborted) return
    repositories.value = repoResult.data.items
    policies.value = policyResult.data.items
    phase.value = 'ready'
    error.value = null
  } catch (caught) {
    if (signal?.aborted) return
    if (caught instanceof ApiError) {
      error.value = caught
      phase.value = 'error'
      return
    }
    throw caught
  }
}

/** 文件根只用于表单里的选择与相对路径提示；读不到不该让整页失败。 */
async function loadRoots(): Promise<void> {
  try {
    const result = await fetchFileRoots()
    roots.value = result.data.roots
  } catch {
    roots.value = []
  }
}

const poller: Poller = createPoller(load, { intervalMs: () => DEFAULT_INTERVAL_MS })

onMounted(() => {
  if (!session.isAdmin) {
    phase.value = 'ready'
    return
  }
  void poller.start()
  void loadRoots()
})

onBeforeUnmount(() => {
  poller.stop()
})

// ---------------------------------------------------------------- 仓库写操作

function openCreateRepo(): void {
  editingRepo.value = null
  Object.assign(repoForm, emptyRepositoryForm(roots.value[0]?.root_id ?? ''))
  repoTouched.value = false
  repoMessage.value = null
  repoNeedsReauth.value = false
  reauthPasswordInput.value = ''
  reauthCode.value = ''
  repoFormOpen.value = true
  if (roots.value.length === 0) void loadRoots()
}

function openEditRepo(repo: BackupRepository): void {
  editingRepo.value = repo
  Object.assign(repoForm, repositoryFormOf(repo))
  repoTouched.value = false
  repoMessage.value = null
  repoNeedsReauth.value = false
  reauthPasswordInput.value = ''
  reauthCode.value = ''
  repoFormOpen.value = true
}

async function saveRepository(): Promise<void> {
  repoTouched.value = true
  repoMessage.value = null
  if (repoProblems.value.length > 0) return
  const current = editingRepo.value
  const input = buildRepositoryInput(repoForm, current?.version ?? 0)
  const key = newIdempotencyKey()
  pendingRepoWrite = { input, key }
  await runRepoWrite()
}

async function runRepoWrite(): Promise<void> {
  const pending = pendingRepoWrite
  if (pending === null) return
  repoBusy.value = true
  try {
    const current = editingRepo.value
    if (current === null) {
      await createBackupRepository(pending.input, pending.key)
      repoMessage.value = '仓库已登记。它此刻只是一个定义：要真的在目标位置建出仓库，请点“初始化仓库”。'
    } else {
      await updateBackupRepository(current.id, pending.input)
      repoMessage.value = '仓库定义已更新。'
    }
    repoNeedsReauth.value = false
    reauthPasswordInput.value = ''
    reauthCode.value = ''
    pendingRepoWrite = null
    repoFormOpen.value = false
    editingRepo.value = null
    await load()
  } catch (caught) {
    const apiError = caught instanceof ApiError ? caught : null
    if (apiError?.code === 'REAUTH_REQUIRED') {
      // 再认证窗口是 5 分钟：密码只走 /auth/reauth，绝不进这个请求体。
      repoNeedsReauth.value = true
      repoMessage.value = '登记与修改仓库要 5 分钟内的密码再认证：请在下面输入密码后重试（参数已保留，不用重填）'
      return
    }
    repoMessage.value = apiError === null ? '保存失败，请稍后重试' : apiError.message
  } finally {
    repoBusy.value = false
  }
}

async function reauthAndRetry(): Promise<void> {
  if (reauthPasswordInput.value === '') return
  repoBusy.value = true
  repoMessage.value = null
  try {
    await reauthPassword(
      reauthPasswordInput.value,
      reauthCode.value === '' ? undefined : reauthCode.value,
    )
    await session.restore()
    reauthPasswordInput.value = ''
    reauthCode.value = ''
    repoBusy.value = false
    await runRepoWrite()
  } catch (caught) {
    repoBusy.value = false
    repoMessage.value = caught instanceof ApiError ? caught.message : '再认证失败'
  }
}

// ---------------------------------------------------------------- 策略写操作

function openCreatePolicy(): void {
  editingPolicy.value = null
  const draft = emptyPolicyForm(repositories.value[0]?.id ?? '')
  // 第一条来源预填成第一个文件根：绝大多数策略就是从某个根开始的。
  const firstSource = draft.sources[0]
  if (firstSource !== undefined) firstSource.rootId = roots.value[0]?.root_id ?? ''
  Object.assign(policyForm, draft)
  policyTouched.value = false
  policyMessage.value = null
  policyFormOpen.value = true
  if (roots.value.length === 0) void loadRoots()
}

function openEditPolicy(policy: BackupPolicy): void {
  editingPolicy.value = policy
  Object.assign(policyForm, policyFormOf(policy))
  policyTouched.value = false
  policyMessage.value = null
  policyFormOpen.value = true
}

function addSource(): void {
  policyForm.sources.push({ kind: 'root', rootId: roots.value[0]?.root_id ?? '', relativePath: '' })
}

function addPanelDatabaseSource(): void {
  policyForm.sources.push({ kind: 'panel_database', rootId: '', relativePath: '' })
}

function removeSource(index: number): void {
  policyForm.sources.splice(index, 1)
}

async function savePolicy(): Promise<void> {
  policyTouched.value = true
  policyMessage.value = null
  if (policyProblems.value.length > 0) return
  policyBusy.value = true
  const current = editingPolicy.value
  try {
    const input = buildPolicyInput(policyForm, current?.version ?? 0)
    if (current === null) await createBackupPolicy(input)
    else await updateBackupPolicy(current.id, input)
    policyFormOpen.value = false
    editingPolicy.value = null
    policyMessage.value = null
    await load()
  } catch (caught) {
    policyMessage.value = caught instanceof ApiError ? caught.message : '保存失败，请稍后重试'
  } finally {
    policyBusy.value = false
  }
}

const deletingPolicyId = ref<string | null>(null)

async function removePolicy(policy: BackupPolicy): Promise<void> {
  deletingPolicyId.value = policy.id
  policyMessage.value = null
  try {
    await deleteBackupPolicy(policy.id)
    await load()
  } catch (caught) {
    policyMessage.value = caught instanceof ApiError ? caught.message : '删除失败，请稍后重试'
  } finally {
    deletingPolicyId.value = null
  }
}

// ---------------------------------------------------------------- 动作入口

function runBackup(policy: BackupPolicy): void {
  const repo = repositories.value.find((entry) => entry.id === policy.repository_id) ?? null
  openAction({
    action: BACKUP_RUN_ACTION,
    title: `运行备份策略「${policy.name}」`,
    params: backupRunParams(policy),
    lines: backupRunConfirmLines(policy, repo),
    irreversible: false,
    needsReauth: true,
  })
}

function initializeRepository(repo: BackupRepository): void {
  openAction({
    action: BACKUP_INITIALIZE_ACTION,
    title: `初始化仓库「${repo.name}」`,
    params: backupInitializeParams(repo),
    lines: initializeConfirmLines(repo),
    irreversible: false,
    needsReauth: true,
  })
}

function prunePolicy(policy: BackupPolicy): void {
  openAction({
    action: BACKUP_PRUNE_ACTION,
    title: `按保留策略清理「${policy.name}」`,
    params: backupPruneParams(policy),
    lines: pruneConfirmLines(policy),
    irreversible: true,
    needsReauth: true,
  })
}

function restoreSnapshot(): void {
  const snapshot = selectedSnapshot.value
  const repo = repositories.value.find((entry) => entry.id === snapshotRepoId.value) ?? null
  const root = roots.value.find((entry) => entry.root_id === restoreRootId.value) ?? null
  if (snapshot === null || repo === null || root === null) return
  openAction({
    action: BACKUP_RESTORE_ACTION,
    title: `从快照 ${snapshot.short_id} 恢复`,
    params: backupRestoreParams({
      repositoryId: repo.id,
      snapshotId: snapshot.snapshot_id,
      targetRootId: root.root_id,
      relativePath: restoreRelativePath.value.trim(),
      overwrite: restoreOverwrite.value,
    }),
    lines: restoreConfirmLines({
      repositoryName: repo.name,
      snapshot,
      targetRootName: root.name,
      relativePath: restoreRelativePath.value.trim(),
      overwrite: restoreOverwrite.value,
    }),
    irreversible: restoreOverwrite.value,
    needsReauth: true,
  })
}

function verifySnapshot(): void {
  const snapshot = selectedSnapshot.value
  const repo = repositories.value.find((entry) => entry.id === snapshotRepoId.value) ?? null
  if (snapshot === null || repo === null) return
  openAction({
    action: BACKUP_VERIFY_ACTION,
    title: `验证快照 ${snapshot.short_id}`,
    params: backupVerifyParams({
      repositoryId: repo.id,
      snapshotId: snapshot.snapshot_id,
      sampleRestore: verifySampleRestore.value,
    }),
    lines: verifyConfirmLines(snapshot, verifySampleRestore.value),
    irreversible: false,
    needsReauth: false,
  })
}

function onActionFinished(): void {
  void load()
  if (snapshotRepoId.value !== '') void loadSnapshots()
}

// ---------------------------------------------------------------- 快照读操作

async function loadSnapshots(): Promise<void> {
  const repoId = snapshotRepoId.value
  if (repoId === '') return
  snapshotLoading.value = true
  snapshotError.value = null
  selectedSnapshot.value = null
  try {
    const result = await fetchBackupSnapshots(repoId, snapshotPolicyId.value)
    snapshots.value = sortSnapshotsNewestFirst(result.data.items)
    snapshotCheckedAt.value = result.data.checked_at
    sizeIncluded.value = result.data.size_included
    snapshotRegistered.value = result.data.registered
  } catch (caught) {
    snapshotError.value = caught instanceof ApiError ? caught.message : '读取快照失败'
    snapshots.value = []
  } finally {
    snapshotLoading.value = false
  }
}

async function openSnapshot(snapshot: BackupSnapshot): Promise<void> {
  selectedSnapshot.value = snapshot
  if (restoreRootId.value === '') restoreRootId.value = roots.value[0]?.root_id ?? ''
  detailSize.value = snapshot.size_bytes ?? null
  detailFiles.value = snapshot.file_count ?? null
  detailVerifiedAt.value = snapshot.restore_verified_at
  snapshotDetailError.value = null
  snapshotDetailLoading.value = true
  try {
    const result = await fetchBackupSnapshot(snapshotRepoId.value, snapshot.snapshot_id)
    if (!result.data.found || result.data.snapshot === undefined) {
      // 列表里刚看到的一份快照读不到详情：多半是它刚被清理掉。如实说。
      snapshotDetailError.value = '仓库里已经找不到这份快照了（可能刚被清理或忘记）。刷新列表再看。'
      return
    }
    const detail = result.data.snapshot
    detailSize.value = detail.size_bytes ?? null
    detailFiles.value = detail.file_count ?? null
    detailVerifiedAt.value = detail.restore_verified_at
  } catch (caught) {
    snapshotDetailError.value = caught instanceof ApiError ? caught.message : '读取快照详情失败'
  } finally {
    snapshotDetailLoading.value = false
  }
}

function pickPolicyFilter(value: string): void {
  snapshotPolicyId.value = value
  void loadSnapshots()
}

function pickSnapshotRepository(value: string): void {
  // 先落选择再读：`loadSnapshots` 读的是 ref，不能指望 v-model 的更新顺序。
  snapshotRepoId.value = value
  snapshotPolicyId.value = ''
  snapshots.value = []
  selectedSnapshot.value = null
  void loadSnapshots()
}
</script>

<template>
  <section class="backup">
    <div class="backup__head">
      <h1 class="backup__title">备份</h1>
      <NTag size="small">{{ repositories.length }} 个仓库</NTag>
      <NTag size="small">{{ policies.length }} 条策略</NTag>
      <NButton v-if="session.isAdmin" size="small" :loading="phase === 'loading'" @click="load()">
        刷新
      </NButton>
      <NButton v-if="session.isAdmin" size="small" type="primary" data-testid="repo-new" @click="openCreateRepo">
        登记仓库
      </NButton>
      <NButton
        v-if="session.isAdmin"
        size="small"
        type="primary"
        secondary
        data-testid="policy-new"
        @click="openCreatePolicy"
      >
        新建策略
      </NButton>
    </div>

    <StatePanel
      v-if="!session.isAdmin"
      state="forbidden"
      title="备份只对管理员可见"
      description="仓库定义里有远端地址与秘密引用，策略里有目录与要停的应用，合起来就是“这台机器的数据备份去哪、怎么备份”。只读账号在概览页能看到备份健康摘要。"
    />
    <StatePanel v-else-if="phase === 'loading'" state="loading" title="正在读取仓库与策略" />
    <StatePanel
      v-else-if="panelState && error && panelState !== 'error'"
      :state="panelState"
      :description="error.message"
      :code="error.code"
      :request-id="error.requestId"
      :retryable="retryablePanelState(panelState)"
      @retry="load()"
    />
    <ApiErrorPanel v-else-if="error" :error="error" @retry="load()" />

    <template v-else>
      <!-- 仓库表单 -->
      <NCard v-if="repoFormOpen" size="small" :title="editingRepo ? `修改仓库「${editingRepo.name}」` : '登记仓库'">
        <p class="backup__muted">
          仓库口令与 SFTP 私钥都以 <code>sec_</code> 引用的形式出现：面板从不持有明文，代理在执行时
          从自己的加密存储里取。所以这里填的是引用，不是密码本身。
        </p>

        <div class="backup__field">
          <label class="backup__label" for="repo-name">仓库名</label>
          <NInput id="repo-name" v-model:value="repoForm.name" placeholder="例如 每日备份" data-testid="repo-name" />
        </div>

        <div class="backup__field">
          <label class="backup__label">后端</label>
          <NSelect v-model:value="repoForm.backend" :options="backendOptions()" data-testid="repo-backend" />
        </div>

        <template v-if="repoForm.backend === 'local'">
          <div class="backup__field">
            <label class="backup__label">文件根</label>
            <NSelect
              v-model:value="repoForm.rootId"
              :options="rootOptions"
              placeholder="选一个已登记的文件根"
              data-testid="repo-root"
            />
          </div>
          <div class="backup__field">
            <label class="backup__label">根下相对路径</label>
            <NInput v-model:value="repoForm.relativePath" placeholder="例如 upanel-restic" />
          </div>
        </template>

        <template v-else>
          <div class="backup__field">
            <label class="backup__label">主机</label>
            <NInput v-model:value="repoForm.host" placeholder="主机名或 IP" data-testid="repo-host" />
          </div>
          <div class="backup__field">
            <label class="backup__label">端口</label>
            <NInput v-model:value="repoForm.port" placeholder="22" />
          </div>
          <div class="backup__field">
            <label class="backup__label">用户名</label>
            <NInput v-model:value="repoForm.username" placeholder="小写字母或下划线开头" />
          </div>
          <div class="backup__field">
            <label class="backup__label">远端绝对路径</label>
            <NInput v-model:value="repoForm.remotePath" placeholder="/backup/upanel" />
          </div>
          <div class="backup__field">
            <label class="backup__label">主机公钥指纹</label>
            <NInput
              v-model:value="repoForm.hostKeyFingerprint"
              placeholder="SHA256:… 或 MD5:…（来自 ssh-keyscan 之后的显式核对）"
            />
          </div>
          <div class="backup__field">
            <label class="backup__label">SFTP 私钥引用</label>
            <NInput v-model:value="repoForm.credentialSecretRef" placeholder="sec_ 加 32 位小写十六进制" />
          </div>
        </template>

        <div class="backup__field">
          <label class="backup__label">仓库口令引用</label>
          <NInput
            v-model:value="repoForm.passwordSecretRef"
            placeholder="sec_ 加 32 位小写十六进制"
            data-testid="repo-password-ref"
          />
        </div>

        <ul v-if="repoTouched && repoProblems.length > 0" class="backup__problems">
          <li v-for="problem in repoProblems" :key="problem">{{ problem }}</li>
        </ul>

        <NAlert v-if="repoMessage" :type="repoNeedsReauth ? 'warning' : 'error'" :title="repoMessage" />

        <div v-if="repoNeedsReauth" class="backup__field">
          <label class="backup__label">当前账号密码（只发给 /auth/reauth）</label>
          <NInput
            v-model:value="reauthPasswordInput"
            type="password"
            placeholder="再认证后自动重试这次保存"
            data-testid="repo-reauth"
          />
          <SecondFactorField
            :show="session.totpEnabled"
            :model-value="reauthCode"
            :disabled="repoBusy"
            testid="repo-reauth-code"
            @update:model-value="(value: string) => (reauthCode = value)"
          />
        </div>

        <div class="backup__actions">
          <NButton secondary :disabled="repoBusy" @click="repoFormOpen = false">取消</NButton>
          <NButton v-if="repoNeedsReauth" type="warning" :loading="repoBusy" @click="reauthAndRetry">
            再认证并重试
          </NButton>
          <NButton v-else type="primary" :loading="repoBusy" data-testid="repo-save" @click="saveRepository">
            保存
          </NButton>
        </div>
      </NCard>

      <!-- 策略表单 -->
      <NCard v-if="policyFormOpen" size="small" :title="editingPolicy ? `修改策略「${editingPolicy.name}」` : '新建策略'">
        <div class="backup__field">
          <label class="backup__label">策略名</label>
          <NInput v-model:value="policyForm.name" placeholder="例如 每日全量" data-testid="policy-name" />
        </div>

        <div class="backup__field">
          <label class="backup__label">仓库</label>
          <NSelect
            v-model:value="policyForm.repositoryId"
            :options="repositoryOptions"
            placeholder="选一个已登记的仓库"
            data-testid="policy-repo"
          />
        </div>

        <div class="backup__block">
          <p class="backup__label">备份来源</p>
          <p class="backup__muted">
            来源只写“哪个文件根下的哪个相对路径”，不写绝对路径：绝对路径是代理读出来的事实。
            面板数据库要用 SQLite 在线副本，不是直接拷那个文件。
          </p>
          <div v-for="(row, index) in policyForm.sources" :key="index" class="backup__source">
            <NTag v-if="row.kind === 'panel_database'" size="small">面板数据库</NTag>
            <template v-else>
              <NSelect v-model:value="row.rootId" :options="rootOptions" placeholder="文件根" />
              <NInput v-model:value="row.relativePath" placeholder="相对路径（留空表示根本身）" />
            </template>
            <NButton size="tiny" secondary @click="removeSource(index)">移除</NButton>
          </div>
          <div class="backup__actions">
            <NButton size="tiny" secondary @click="addSource">加一条文件来源</NButton>
            <NButton size="tiny" secondary @click="addPanelDatabaseSource">加面板数据库</NButton>
          </div>
        </div>

        <div class="backup__field">
          <label class="backup__label">一致性口径</label>
          <NSelect v-model:value="policyForm.consistencyMode" :options="consistencyOptions" />
        </div>

        <div v-if="policyForm.consistencyMode === 'stop_managed_apps'" class="backup__field">
          <label class="backup__label">备份前停掉的应用 id（逗号或换行分隔）</label>
          <NInput
            v-model:value="policyForm.appIds"
            type="textarea"
            :autosize="{ minRows: 2, maxRows: 4 }"
            placeholder="app_ 加 32 位小写十六进制"
          />
          <p class="backup__muted">
            面板在预检时把每个应用展开成此刻的运行对象（哪个 systemd 单元、哪个容器 id）。
            有一个解析不出来就整次阻塞——少停一个应用的备份会被打上 stop_managed_apps 的标签，
            而实际是在应用写着的中间态读的，那比失败更糟。
          </p>
        </div>

        <div class="backup__block">
          <p class="backup__label">保留策略（0 或留空表示不设这一项）</p>
          <div class="backup__retention">
            <NInput v-model:value="policyForm.keepLast" placeholder="最近几份" />
            <NInput v-model:value="policyForm.keepDaily" placeholder="每天几份" />
            <NInput v-model:value="policyForm.keepWeekly" placeholder="每周几份" />
          </div>
          <p class="backup__muted">
            保留策略只在清理（backup.prune）时起作用：备份本身从不删东西。
          </p>
        </div>

        <ul v-if="policyTouched && policyProblems.length > 0" class="backup__problems">
          <li v-for="problem in policyProblems" :key="problem">{{ problem }}</li>
        </ul>

        <NAlert v-if="policyMessage" type="error" :title="policyMessage" />

        <div class="backup__actions">
          <NButton secondary :disabled="policyBusy" @click="policyFormOpen = false">取消</NButton>
          <NButton type="primary" :loading="policyBusy" data-testid="policy-save" @click="savePolicy">
            保存
          </NButton>
        </div>
      </NCard>

      <NCard size="small" title="仓库">
        <p v-if="repositories.length === 0" class="backup__muted">
          还没有登记仓库。一条定义先要有一个放备份的地方：本机文件根下的一个目录，或者一台 SFTP 服务器。
        </p>
        <div v-else class="backup__list" data-testid="repo-list">
          <div v-for="repo in repositories" :key="repo.id" class="backup__row">
            <div class="backup__row-main">
              <strong>{{ repo.name }}</strong>
              <NTag size="small">{{ repo.backend === 'sftp' ? 'SFTP' : '本机目录' }}</NTag>
              <span class="backup__meta">{{ locationLabel(repo) }}</span>
              <span class="backup__meta">· 版本 {{ repo.version }}</span>
            </div>
            <div class="backup__actions">
              <NButton size="tiny" secondary data-testid="repo-init" @click="initializeRepository(repo)">
                初始化仓库
              </NButton>
              <NButton size="tiny" secondary @click="openEditRepo(repo)">修改</NButton>
            </div>
          </div>
        </div>
        <p class="backup__muted">
          没有删除仓库的入口：契约里没有这条，而且删掉定义会让仓库里那些快照"属于谁"无从回答。
        </p>
      </NCard>

      <NCard size="small" title="策略">
        <NAlert v-if="policyMessage && !policyFormOpen" type="error" :title="policyMessage" />
        <p v-if="policies.length === 0" class="backup__muted">
          还没有备份策略。一条策略说的是"备份哪几处、停不停应用、留多少份"。
        </p>
        <div v-else class="backup__list" data-testid="policy-list">
          <div v-for="policy in policies" :key="policy.id" class="backup__row">
            <div class="backup__row-main">
              <strong>{{ policy.name }}</strong>
              <NTag size="small">{{ BACKUP_CONSISTENCY_LABELS[policy.consistency_mode] }}</NTag>
              <span class="backup__meta">{{ policy.sources.map(sourceLabel).join('；') }}</span>
              <span class="backup__meta">· 保留：{{ retentionLabel(policy.retention) }}</span>
            </div>
            <div class="backup__actions">
              <NButton size="tiny" type="primary" data-testid="policy-run" @click="runBackup(policy)">
                立即备份
              </NButton>
              <NButton size="tiny" secondary data-testid="policy-prune" @click="prunePolicy(policy)">
                按保留清理
              </NButton>
              <NButton size="tiny" secondary @click="openEditPolicy(policy)">修改</NButton>
              <NButton
                size="tiny"
                secondary
                :loading="deletingPolicyId === policy.id"
                data-testid="policy-delete"
                @click="removePolicy(policy)"
              >
                删除
              </NButton>
            </div>
          </div>
        </div>
        <p class="backup__muted">
          删除策略不动仓库，也不动仓库里已有的快照：清理必须显式发起。
        </p>
      </NCard>

      <NCard size="small" title="快照">
        <div class="backup__query">
          <NSelect
            :value="snapshotRepoId"
            :options="repositoryOptions"
            placeholder="选一个仓库"
            data-testid="snapshot-repo"
            @update:value="pickSnapshotRepository"
          />
          <NSelect
            :value="snapshotPolicyId"
            :options="[{ label: '全部策略', value: '' }, ...snapshotPolicies.map((p) => ({ label: p.name, value: p.id }))]"
            placeholder="按策略过滤"
            @update:value="pickPolicyFilter"
          />
          <NButton size="small" :loading="snapshotLoading" @click="loadSnapshots">读取快照</NButton>
        </div>

        <p v-if="snapshotRepoId === ''" class="backup__muted">选一个仓库再读取：快照 id 只在仓库内部唯一，没有跨仓库的快照列表。</p>
        <NAlert v-if="snapshotError" type="error" :title="snapshotError" />
        <p v-if="snapshots.length > 0" class="backup__muted">
          {{ snapshots.length }} 份快照，读取于 {{ formatTimestamp(snapshotCheckedAt) }}。
          <template v-if="!sizeIncluded">
            列表<b>不含大小</b>：每条都要单独跑一次 restic stats，那是分钟级的代价——要看大小与文件数，点开详情。
          </template>
        </p>
        <NAlert v-if="!snapshotRegistered && snapshotRepoId !== ''" type="warning" :bordered="false">
          这个仓库在代理里没有登记（或刚被改过）：下面的空列表不是“还没有备份”，而是“问不到”。
        </NAlert>

        <div v-if="snapshots.length > 0" class="backup__list" data-testid="snapshot-list">
          <div v-for="snapshot in snapshots" :key="snapshot.snapshot_id" class="backup__row">
            <div class="backup__row-main">
              <code>{{ snapshot.short_id }}</code>
              <span class="backup__meta">{{ formatTimestamp(snapshot.time) }}</span>
              <NTag v-if="snapshot.policy_id" size="small">{{ snapshot.policy_id }}</NTag>
              <NTag v-if="snapshot.consistency_mode" size="small">{{ snapshot.consistency_mode }}</NTag>
              <NTag v-if="snapshot.restore_verified_at" size="small" type="success">
                恢复已验证 {{ formatTimestamp(snapshot.restore_verified_at) }}
              </NTag>
              <NTag v-else size="small" type="warning">没做过恢复验证</NTag>
            </div>
            <div class="backup__actions">
              <NButton size="tiny" secondary data-testid="snapshot-open" @click="openSnapshot(snapshot)">
                详情
              </NButton>
            </div>
          </div>
        </div>

        <div v-if="selectedSnapshot" class="backup__detail">
          <p class="backup__label">快照 {{ selectedSnapshot.short_id }} 的详情</p>
          <p v-if="snapshotDetailLoading" class="backup__muted">正在读详情（这一步会跑 restic stats，可能要几十秒）…</p>
          <NAlert v-if="snapshotDetailError" type="warning" :title="snapshotDetailError" />
          <ul class="backup__facts">
            <li>时间：{{ formatTimestamp(selectedSnapshot.time) }}</li>
            <li>路径：{{ selectedSnapshot.paths.join('、') || '（快照没记录路径）' }}</li>
            <li v-if="selectedSnapshot.tags.length > 0">标签：{{ selectedSnapshot.tags.join('、') }}</li>
            <li v-if="selectedSnapshot.hostname">主机名：{{ selectedSnapshot.hostname }}</li>
            <li>大小：{{ detailSize === null ? '读不到（或还没读到）' : formatBytes(detailSize) }}</li>
            <li>文件数：{{ detailFiles === null ? '读不到（或还没读到）' : detailFiles }}</li>
            <li>
              恢复验证：
              {{ detailVerifiedAt === null ? '没有做过（不是“验证失败”）' : formatTimestamp(detailVerifiedAt) }}
            </li>
          </ul>

          <div class="backup__block">
            <p class="backup__label">验证这份快照</p>
            <NCheckbox v-model:checked="verifySampleRestore">
              抽样恢复几个文件并比对内容（慢，但这才是“读得回来”的证据）
            </NCheckbox>
            <div class="backup__actions">
              <NButton size="small" type="primary" data-testid="snapshot-verify" @click="verifySnapshot">
                验证
              </NButton>
            </div>
          </div>

          <div class="backup__block">
            <p class="backup__label">恢复这份快照</p>
            <div class="backup__field">
              <label class="backup__label">恢复到哪个文件根</label>
              <NSelect v-model:value="restoreRootId" :options="rootOptions" placeholder="选一个文件根" data-testid="restore-root" />
            </div>
            <div class="backup__field">
              <label class="backup__label">根下相对路径（留空表示根本身）</label>
              <NInput v-model:value="restoreRelativePath" placeholder="例如 restored-2026-10-24" />
            </div>
            <NCheckbox v-model:checked="restoreOverwrite">
              覆盖已有文件（会先停掉全部可停的受管理应用，有一个停不了就整次拒绝）
            </NCheckbox>
            <div class="backup__actions">
              <NButton
                size="small"
                type="warning"
                :disabled="restoreRootId === ''"
                data-testid="snapshot-restore"
                @click="restoreSnapshot"
              >
                恢复
              </NButton>
            </div>
          </div>
        </div>
      </NCard>

      <NCard size="small" title="这一页的规矩">
        <p class="backup__line">
          备份、恢复、验证、清理都只有一条路：预检 → 任务。预检会先碰现场——仓库通不通、口令对不对、
          快照还在不在、空间够不够——判决不通过时提交按钮根本不出现。
        </p>
        <p class="backup__line">
          仓库口令只经标准输入交给 restic（<code>RESTIC_PASSWORD</code>），SFTP 私钥只在 tmpfs 上临时存在，
          known_hosts 只来自你填进定义的那份指纹核对。这些东西都不进 argv、不进日志、不进面板数据库。
        </p>
        <p class="backup__line">
          备份成功不等于恢复已验证：<code>backup.run</code> 的核验是"写完读回来对得上"，
          要证明数据读得回来得单独做一次 <code>backup.verify</code>，而且要勾上抽样恢复。
        </p>
        <p class="backup__line">
          清理（<code>backup.prune</code>）是这一页唯一会真的删数据的动作，删的是仓库里的旧快照，
          找不回来。
        </p>
      </NCard>

      <StorageActionDialog
        v-if="actionDialog"
        v-model:show="actionShow"
        :action="actionDialog.action"
        :title="actionDialog.title"
        :params="actionDialog.params"
        :confirm-lines="actionDialog.lines"
        :irreversible="actionDialog.irreversible"
        :needs-reauth="actionDialog.needsReauth"
        :summarizer="summarizeBackupJob"
        @finished="onActionFinished"
      />
    </template>
  </section>
</template>

<style scoped>

.backup__muted {
  margin: 0;
  font-size: var(--fs-md);
  line-height: 1.7;
  opacity: 0.8;
}

.backup__list {
  display: flex;
  flex-direction: column;
  gap: 8px;
  margin: 8px 0;
}

.backup__row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  flex-wrap: wrap;
  padding: 8px 0;
  border-bottom: 1px solid var(--rule);
}

.backup__row-main {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}

.backup__field {
  display: flex;
  flex-direction: column;
  gap: 4px;
  margin-bottom: 10px;
}

.backup__block {
  display: flex;
  flex-direction: column;
  gap: 6px;
  margin: 10px 0;
}

.backup__label {
  margin: 0;
  font-size: var(--fs-md);
  font-weight: 600;
}

.backup__actions {
  display: flex;
  gap: 8px;
  justify-content: flex-end;
  flex-wrap: wrap;
  margin-top: 8px;
}

.backup__problems {
  margin: 8px 0;
  padding-left: 18px;
  font-size: var(--fs-md);
  line-height: 1.7;
  color: var(--bad);
}

.backup__source {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
  margin-bottom: 6px;
}

.backup__retention {
  display: flex;
  gap: 8px;
  flex-wrap: wrap;
}

.backup__query {
  display: flex;
  gap: 8px;
  flex-wrap: wrap;
  align-items: center;
  margin-bottom: 8px;
}

.backup__detail {
  margin-top: 12px;
  padding-top: 12px;
  border-top: 1px solid var(--rule);
}

.backup__facts {
  margin: 8px 0;
  padding-left: 18px;
  font-size: var(--fs-md);
  line-height: 1.7;
}
</style>
