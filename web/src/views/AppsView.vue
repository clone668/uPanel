<script setup lang="ts">
// 应用页（P09.5）。
//
// 这一页把“已经在这台机器上跑着的东西”摆成人能用的形状：卡片、搜索、分组、一键打开、
// 状态区分，以及本机候选的确认入口。它不新增任何接口——读的是 P09.1—P09.4 已有的
// `/apps`、`/app-groups`、`/apps/discovery`、`/apps/{id}/logs`，写的是 `POST/PATCH/DELETE
// /apps`、`PUT /apps/{id}/favorite`，启停走的是 `service.control` / `container.control`
// 那条既有的预检→任务路径（没有 app.control 这个动作，见 api/apps.ts 里的说明）。
//
// 五条显示纪律，每条都对应一条验收要求：
//   1. 运行时状态与入口状态分开显示：容器 running + HTTP 500 必须看起来像“运行中 + 异常”；
//   2. 按钮只按 `capabilities` 渲染：link 没有启停，受保护单元只有日志；
//   3. 依赖保护是观测结果，卡片上写明哪条路径绕开了面板，不写成“一切正常”；
//   4. 本机候选读不到时不说“本机什么都没有”，而是说这一轮没读到；
//   5. 绑定已经不在本机的应用（容器被重建）不假装还在，候选区单独列出来要求重新确认。

import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import {
  NAlert,
  NButton,
  NCard,
  NInput,
  NTag,
} from 'naive-ui'
import { RouterLink } from 'vue-router'

import {
  APP_CONTROL_LABELS,
  bucketApps,
  createApp,
  createAppGroup,
  deleteApp,
  deleteAppGroup,
  draftFromApp,
  draftFromCandidate,
  draftToDefinition,
  draftToUpdate,
  getAppDiscovery,
  getAppGroups,
  getApps,
  setFavorite,
  updateApp,
  updateAppGroup,
  type App,
  type AppAction,
  type AppControlOperation,
  type AppDiscoveryCandidate,
  type AppDiscoveryData,
  type AppDraft,
  type AppGroup,
  controlRequest,
} from '@/api/apps'
import { ApiError } from '@/api/client'
import { fetchVolumes, type Volume } from '@/api/volumes'
import ApiErrorPanel from '@/components/ApiErrorPanel.vue'
import AppCard from '@/components/AppCard.vue'
import AppDiscoveryPanel from '@/components/AppDiscoveryPanel.vue'
import AppForm from '@/components/AppForm.vue'
import AppLogPanel from '@/components/AppLogPanel.vue'
import StatePanel from '@/components/StatePanel.vue'
import StorageActionDialog from '@/components/StorageActionDialog.vue'
import { useSessionStore } from '@/stores/session'
import { summarizeAppJob } from '@/utils/jobresult'
import { createPoller, type Poller } from '@/utils/polling'
import { apiPanelState, retryablePanelState } from '@/utils/viewstate'

type Phase = 'loading' | 'ready' | 'error'

const DEFAULT_INTERVAL_MS = 15_000
/** 一页最多取多少应用。超过时页面会说“还有更多”，而不是悄悄少显示。 */
const PAGE_LIMIT = 200
const MAX_PAGES = 5

const session = useSessionStore()

const phase = ref<Phase>('loading')
const error = ref<ApiError | null>(null)
const apps = ref<App[]>([])
const groups = ref<AppGroup[]>([])
/** 应用数量超过分页上限：列表被截断，不是“没有更多应用”。 */
const appsTruncated = ref(false)

const discovery = ref<AppDiscoveryData | null>(null)
const discoveryError = ref('')

const volumes = ref<Volume[]>([])
const volumesError = ref('')

const query = ref('')

const formOpen = ref(false)
const editingApp = ref<App | null>(null)
const initialDraft = ref<AppDraft | null>(null)
const formBusy = ref(false)
const formError = ref<string | null>(null)

const pendingDelete = ref<App | null>(null)
const deleteBusy = ref(false)
const deleteError = ref<string | null>(null)

const controlApp = ref<App | null>(null)
const controlOperation = ref<AppControlOperation>('start')
const controlShow = ref(false)

const logsApp = ref<App | null>(null)
const logsShow = ref(false)

const groupName = ref('')
const groupBusy = ref(false)
const groupError = ref<string | null>(null)
const groupEditing = ref<Record<string, string>>({})

const panelState = computed(() => (error.value ? apiPanelState(error.value) : null))
const buckets = computed(() => bucketApps(apps.value, groups.value, query.value))

const runningCount = computed(() => apps.value.filter((app) => app.runtime_state === 'running').length)
const failedCount = computed(() => apps.value.filter((app) => app.runtime_state === 'failed').length)
/** 入口异常：启用了健康检查并且结论是 error。unknown 不算异常——那是“还没结论”。 */
const httpErrorCount = computed(() => apps.value.filter((app) => app.http_state === 'error').length)
const unmanagedCount = computed(
  () => apps.value.filter((app) => app.dependency_protection === 'unmanaged').length,
)
const candidateCount = computed(
  () => discovery.value?.candidates.filter((c) => c.bound_app_id === null).length ?? 0,
)

const controlParams = computed<Record<string, unknown> | null>(() => {
  const app = controlApp.value
  if (app === null) return null
  return controlRequest(app, controlOperation.value)?.params ?? null
})
const controlAction = computed(() => {
  const app = controlApp.value
  if (app === null) return 'service.control'
  return controlRequest(app, controlOperation.value)?.action ?? 'service.control'
})
const controlLines = computed<string[]>(() => {
  const app = controlApp.value
  if (app === null) return []
  const lines = [`应用：${app.name}（${app.type}）`]
  if (app.volume_dependencies.length > 0) {
    lines.push(
      `依赖卷 ${app.volume_dependencies.length} 个：启动前预检会核对它们此刻挂没挂、UUID 对不对`,
    )
  }
  if (app.dependency_protection === 'unmanaged') {
    lines.push(
      `依赖未接管：${app.dependency_protection_note ?? '面板管不住它自己的自动启动路径'}——这一步只保证“这一次经面板的启动”的检查`,
    )
  }
  return lines
})

async function loadApps(): Promise<void> {
  const collected: App[] = []
  let cursor: string | undefined
  for (let page = 0; page < MAX_PAGES; page += 1) {
    const result = await getApps({ limit: PAGE_LIMIT, ...(cursor === undefined ? {} : { cursor }) })
    collected.push(...result.data.items)
    const next = result.data.next_cursor
    if (next === null || next === '') {
      cursor = undefined
      appsTruncated.value = false
      break
    }
    cursor = next
    appsTruncated.value = true
  }
  apps.value = collected
}

async function load(): Promise<void> {
  try {
    const [groupList] = await Promise.all([getAppGroups(), loadApps()])
    groups.value = groupList.data.items
    phase.value = 'ready'
    error.value = null
  } catch (caught) {
    if (caught instanceof ApiError) {
      error.value = caught
      phase.value = 'error'
      return
    }
    throw caught
  }
}

/**
 * 本机候选与卷列表都是“锦上添花”的那一半：读不到不影响应用列表本身，
 * 但要把原因留在页面上，因为候选区与健康检查表单都依赖它们。
 */
async function loadSide(): Promise<void> {
  if (session.isAdmin) {
    try {
      const result = await getAppDiscovery()
      discovery.value = result.data
      discoveryError.value = ''
    } catch (caught) {
      discovery.value = null
      discoveryError.value = caught instanceof ApiError ? caught.message : '读取本机候选失败'
    }
  }
  try {
    const result = await fetchVolumes()
    volumes.value = result.data.volumes
    volumesError.value = ''
  } catch (caught) {
    volumes.value = []
    volumesError.value = caught instanceof ApiError ? caught.message : '读取卷列表失败'
  }
}

async function refreshAll(): Promise<void> {
  await Promise.all([load(), loadSide()])
}

const poller: Poller = createPoller(refreshAll, { intervalMs: () => DEFAULT_INTERVAL_MS })

onMounted(() => {
  void poller.start()
})

onBeforeUnmount(() => {
  poller.stop()
})

// ---------------------------------------------------------------- 新建 / 编辑

function openCreate(): void {
  editingApp.value = null
  initialDraft.value = null
  formError.value = null
  formOpen.value = true
}

function openEdit(app: App): void {
  editingApp.value = app
  initialDraft.value = draftFromApp(app)
  formError.value = null
  formOpen.value = true
}

/** 确认一条本机候选：等同新建应用，表单里已经把绑定与探测目标填好。 */
function adoptCandidate(candidate: AppDiscoveryCandidate): void {
  editingApp.value = null
  initialDraft.value = draftFromCandidate(candidate)
  formError.value = null
  formOpen.value = true
}

function closeForm(): void {
  formOpen.value = false
  editingApp.value = null
  initialDraft.value = null
  formError.value = null
}

async function submitForm(draft: AppDraft): Promise<void> {
  formBusy.value = true
  formError.value = null
  try {
    const editing = editingApp.value
    if (editing === null) {
      const created = await createApp(draftToDefinition(draft))
      apps.value = [...apps.value, created.data]
    } else {
      // version 用**当前**版本：乐观锁比的是“我改的是不是最新那一版”。
      const updated = await updateApp(editing.id, draftToUpdate(draft, editing.version))
      apps.value = apps.value.map((app) => (app.id === updated.data.id ? updated.data : app))
    }
    closeForm()
    void refreshAll()
  } catch (caught) {
    formError.value = caught instanceof ApiError ? caught.message : '保存失败'
  } finally {
    formBusy.value = false
  }
}

// ---------------------------------------------------------------- 收藏

async function toggleFavorite(app: App, next: boolean): Promise<void> {
  try {
    const result = await setFavorite(app.id, next)
    apps.value = apps.value.map((item) =>
      item.id === app.id ? { ...item, favorite: result.data.favorite } : item,
    )
  } catch (caught) {
    formError.value = caught instanceof ApiError ? caught.message : '收藏失败'
  }
}

// ---------------------------------------------------------------- 删除

function askDelete(app: App): void {
  pendingDelete.value = app
  deleteError.value = null
}

async function confirmDelete(): Promise<void> {
  const app = pendingDelete.value
  if (app === null) return
  deleteBusy.value = true
  deleteError.value = null
  try {
    await deleteApp(app.id, app.version)
    apps.value = apps.value.filter((item) => item.id !== app.id)
    pendingDelete.value = null
  } catch (caught) {
    deleteError.value = caught instanceof ApiError ? caught.message : '删除失败'
  } finally {
    deleteBusy.value = false
  }
}

// ---------------------------------------------------------------- 启停

function onControl(app: App, action: AppAction): void {
  if (action !== 'start' && action !== 'stop' && action !== 'restart') return
  controlApp.value = app
  controlOperation.value = action
  controlShow.value = true
}

function onControlFinished(): void {
  void refreshAll()
}

// ---------------------------------------------------------------- 日志

function onLogs(app: App): void {
  logsApp.value = app
  logsShow.value = true
}

// ---------------------------------------------------------------- 分组

async function submitGroup(): Promise<void> {
  const name = groupName.value.trim()
  if (name === '') return
  groupBusy.value = true
  groupError.value = null
  try {
    const created = await createAppGroup({ name })
    groups.value = [...groups.value, created.data]
    groupName.value = ''
  } catch (caught) {
    groupError.value = caught instanceof ApiError ? caught.message : '新建分组失败'
  } finally {
    groupBusy.value = false
  }
}

function onGroupEdit(groupId: string, value: string): void {
  groupEditing.value = { ...groupEditing.value, [groupId]: value }
}

async function renameGroup(group: AppGroup): Promise<void> {
  const name = (groupEditing.value[group.id] ?? group.name).trim()
  if (name === '' || name === group.name) return
  groupBusy.value = true
  groupError.value = null
  try {
    const updated = await updateAppGroup(group.id, group.version, { name })
    groups.value = groups.value.map((item) => (item.id === updated.data.id ? updated.data : item))
    const next = { ...groupEditing.value }
    delete next[group.id]
    groupEditing.value = next
  } catch (caught) {
    groupError.value = caught instanceof ApiError ? caught.message : '改名失败'
  } finally {
    groupBusy.value = false
  }
}

async function removeGroup(group: AppGroup): Promise<void> {
  groupBusy.value = true
  groupError.value = null
  try {
    await deleteAppGroup(group.id, group.version)
    groups.value = groups.value.filter((item) => item.id !== group.id)
  } catch (caught) {
    // 分组里还有应用时服务端回 422 DEPENDENCY_BLOCKED：删分组不该顺手改别的记录。
    groupError.value = caught instanceof ApiError ? caught.message : '删除分组失败'
  } finally {
    groupBusy.value = false
  }
}
</script>

<template>
  <section class="apps">
    <div class="apps__head">
      <h1 class="apps__title">应用</h1>
      <NTag size="small">{{ apps.length }} 个应用</NTag>
      <NTag v-if="runningCount > 0" size="small" type="success">运行中 {{ runningCount }}</NTag>
      <NTag v-if="failedCount > 0" size="small" type="error">启动失败 {{ failedCount }}</NTag>
      <NTag v-if="httpErrorCount > 0" size="small" type="error">入口异常 {{ httpErrorCount }}</NTag>
      <NTag v-if="unmanagedCount > 0" size="small" type="warning">依赖未接管 {{ unmanagedCount }}</NTag>
      <NTag v-if="session.isAdmin && candidateCount > 0" size="small" type="info">
        待确认候选 {{ candidateCount }}
      </NTag>
      <NButton v-if="session.isAdmin" size="small" type="primary" data-testid="app-new" @click="openCreate">
        新建应用
      </NButton>
      <NButton size="small" secondary @click="poller.trigger()">刷新</NButton>
      <!-- 受管部署（P15.4）不是“应用”：这一页是面板里的入口与绑定记录，
           部署页是面板自己部署出去的那几份东西（单元 + 渲染配置 + 版本）。
           两个列表按 deployment_id 能连起来，但读的不是同一张表。 -->
      <RouterLink to="/apps/deployments" class="apps__deploy-link">
        <NButton size="small" secondary data-testid="apps-deployments-link">受管部署</NButton>
      </RouterLink>
    </div>

    <NInput
      v-model:value="query"
      clearable
      placeholder="搜索名称、说明或绑定的单元/容器（只过滤当前列表，不向服务端传条件）"
      data-testid="app-search"
    />

    <StatePanel v-if="phase === 'loading'" state="loading" title="正在读取应用列表" />
    <StatePanel
      v-else-if="panelState && error && panelState !== 'error'"
      :state="panelState"
      :description="error.message"
      :code="error.code"
      :request-id="error.requestId"
      :retryable="retryablePanelState(panelState)"
      @retry="refreshAll()"
    />
    <ApiErrorPanel v-else-if="error" :error="error" @retry="refreshAll()" />

    <template v-else>
      <NAlert v-if="appsTruncated" type="warning" :bordered="false">
        应用数量超过了单次读取上限（{{ MAX_PAGES }} × {{ PAGE_LIMIT }}），列表被截断；
        没有显示出来的应用不代表不存在。用搜索定位是一个办法，但它也只过滤已读到的这一部分。
      </NAlert>
      <NAlert v-if="volumesError !== ''" type="warning" :bordered="false">
        读不到受管理卷列表（{{ volumesError }}）：依赖卷那一栏的选择是空的，但已有应用的依赖关系照常显示。
      </NAlert>

      <NAlert v-if="pendingDelete" type="warning" :bordered="false" data-testid="app-delete-confirm">
        <p class="apps__line">
          删除「{{ pendingDelete.name }}」只删除面板里的<b>入口与绑定记录</b>：不停服务、不删容器、
          不卸载卷、不碰依赖卷里的数据。删掉之后这一条就不在列表里了。
        </p>
        <p v-if="deleteError" class="apps__warn">{{ deleteError }}</p>
        <div class="apps__actions">
          <NButton size="small" type="error" :loading="deleteBusy" @click="confirmDelete">确认删除</NButton>
          <NButton size="small" secondary :disabled="deleteBusy" @click="pendingDelete = null">取消</NButton>
        </div>
      </NAlert>

      <AppForm
        v-if="formOpen"
        :app="editingApp"
        :initial-draft="initialDraft"
        :groups="groups"
        :volumes="volumes"
        :candidates="discovery?.candidates ?? []"
        :discovery-note="discoveryError"
        :busy="formBusy"
        :error-message="formError"
        @submit="submitForm"
        @cancel="closeForm"
      />

      <StatePanel
        v-if="apps.length === 0 && !formOpen"
        state="empty"
        title="还没有登记任何应用"
        description="可以从本机候选里确认一个（下面是 discovery 的结果），也可以手动新建一条。"
      />

      <StatePanel
        v-else-if="apps.length > 0 && buckets.length === 0"
        state="empty"
        title="没有匹配的应用"
        description="换一个关键字试试；搜索只作用于已经读到的这一部分列表。"
      />

      <NCard
        v-for="bucket in buckets"
        :key="bucket.key"
        size="small"
        :title="`${bucket.label}（${bucket.apps.length}）`"
      >
        <p v-if="bucket.apps.length === 0" class="apps__meta">
          这个分组还没有应用。分组存在但空，与“没有这个分组”是两件事。
        </p>
        <div v-else class="apps__grid" :data-bucket="bucket.key">
          <AppCard
            v-for="app in bucket.apps"
            :key="app.id"
            :app="app"
            :can-act="session.isAdmin"
            @control="onControl"
            @logs="onLogs"
            @edit="openEdit"
            @favorite="toggleFavorite"
            @remove="askDelete"
          />
        </div>
      </NCard>

      <AppDiscoveryPanel
        v-if="session.isAdmin"
        :data="discovery"
        :error-message="discoveryError"
        :can-act="session.isAdmin"
        @adopt="adoptCandidate"
      />

      <NCard v-if="session.isAdmin" size="small" title="分组管理">
        <div class="apps__group-new">
          <NInput
            v-model:value="groupName"
            placeholder="新分组名（1—40 个字符，不能重名）"
            maxlength="40"
            data-testid="app-group-name"
          />
          <NButton size="small" :loading="groupBusy" @click="submitGroup">新建分组</NButton>
        </div>
        <p v-if="groupError" class="apps__warn">{{ groupError }}</p>
        <p v-if="groups.length === 0" class="apps__meta">还没有分组；不分组的应用会落在「未分组」。</p>
        <ul v-else class="apps__group-list">
          <li v-for="group in groups" :key="group.id" class="apps__group-row">
            <NInput
              :value="groupEditing[group.id] ?? group.name"
              size="small"
              class="apps__group-input"
              @update:value="onGroupEdit(group.id, $event)"
            />
            <NButton size="tiny" quaternary :disabled="groupBusy" @click="renameGroup(group)">改名</NButton>
            <NButton size="tiny" quaternary :disabled="groupBusy" @click="removeGroup(group)">删除</NButton>
            <span class="apps__meta">排序 {{ group.sort_order }} · 版本 {{ group.version }}</span>
          </li>
        </ul>
        <p class="apps__meta">分组里还有应用时删除会得到 422 DEPENDENCY_BLOCKED：删分组不会顺手把应用移出去。</p>
      </NCard>

      <NCard size="small" title="这一页的规矩">
        <p class="apps__line">
          <b>运行时状态与入口状态是两件事。</b>卡片上“运行中”来自 systemd/容器的观测，
          “入口正常/异常”来自面板自己的探测：容器在跑而 HTTP 返回 500 时，两个徽标会同时出现，
          页面不会把它合成一句“正常”。
        </p>
        <p class="apps__line">
          <b>按钮来自服务端的能力判决。</b>link 类型没有启停（它没有运行时）；受保护单元只有日志；
          绑定对象不在本机时什么按钮都没有。先渲染再让点击失败，比不渲染更糟。
        </p>
        <p class="apps__line">
          <b>启停只有一条路：</b>预检 → 任务。依赖卷掉线时，受管理的那条启动路径会被预检拦下
          （DEPENDENCY_BLOCKED）；不受管理的路径拦不住，卡片上会写明是哪一条绕开了面板。
        </p>
        <p class="apps__line">
          <b>入口地址与探测目标是两个字段：</b>前者给浏览器点，后者给面板探测（只能是
          <code>127.0.0.1:端口</code>，且只能是本机发现到的目标）。
        </p>
      </NCard>
    </template>

    <StorageActionDialog
      v-if="controlApp"
      v-model:show="controlShow"
      :action="controlAction"
      :title="`${APP_CONTROL_LABELS[controlOperation]}「${controlApp.name}」`"
      :params="controlParams"
      :confirm-lines="controlLines"
      :summarizer="summarizeAppJob"
      @finished="onControlFinished"
    />

    <AppLogPanel
      v-if="logsApp"
      v-model:show="logsShow"
      :app-id="logsApp.id"
      :app-name="logsApp.name"
    />
  </section>
</template>

<style scoped>

/* 链接只用来包住按钮：RouterLink 默认的下划线在工具栏里很吵。 */
.apps__deploy-link {
  text-decoration: none;
}

.apps__warn {
  margin: 4px 0 0;
  line-height: 1.7;
  color: var(--bad);
}

.apps__actions {
  display: flex;
  gap: 8px;
  margin-top: 8px;
}

.apps__grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(340px, 1fr));
  gap: 12px;
}

.apps__group-new {
  display: flex;
  align-items: center;
  gap: 8px;
}

.apps__group-list {
  margin: 10px 0 0;
  padding: 0;
  list-style: none;
  display: flex;
  flex-direction: column;
  gap: 8px;
}

.apps__group-row {
  display: flex;
  align-items: center;
  gap: 8px;
}

.apps__group-input {
  max-width: 260px;
}
</style>
