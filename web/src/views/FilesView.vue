<script setup lang="ts">
// 文件页（P06.1）：受管理文件根的浏览、文本预览与受控下载。
//
// 这一页的第一责任不是“好用”，而是**不撒谎**。PLAN.md 第 15 节与
// CONTRACTS.md 第 11 节把文件管理的范围限定得很死，页面必须把限定写出来：
//
//   1) 根来自服务端配置（GET /files/roots）。界面没有“输入路径”的框，
//      也不会列服务端没登记的目录。没有登记根时写的是“未登记任何文件根”，
//      不是“浏览服务器”。这一条直接对应验收里那句
//      “文件 UI 不能显示‘可管理整个服务器所有目录’”。
//   2) 路径一律是根内相对路径。向上只能到根为止，前进只能靠点目录项，
//      没有任何地方把用户输入拼成路径——拼路径就是越界漏洞的温床。
//   3) 符号链接不跟随。它们在列表里就是“符号链接”，点不进去，也不下载；
//      理由写在行内，不是灰按钮不解释。
//   4) 文本预览带 expected_version。服务端在已打开的句柄上比对，
//      VERSION_CONFLICT 说明文件在点开之后被改过，页面显示“文件已改变”
//      并给一次“重新读取”，而不是把旧内容当新内容继续展示。
//   5) 错误按 code 分支（PATH_OUTSIDE_ROOT / FILE_NOT_TEXT /
//      ROOT_UNAVAILABLE / VERSION_CONFLICT / NOT_IMPLEMENTED），不按 message。
//
// P06.1 只有读。上传、改名、删除属于 P06.2/P06.3，需要预检与任务，
// 页面上不预留假入口。

import { computed, onMounted, ref } from 'vue'
import { NAlert, NButton, NCard, NSelect, NTag } from 'naive-ui'

import { ApiError } from '@/api/client'
import {
  MAX_TEXT_BYTES,
  breadcrumbs,
  canRead,
  downloadFile,
  fetchFileList,
  fetchFileRoots,
  fetchFileText,
  joinRelative,
  parentRelative,
  type FileEntry,
  type FileRoot,
  type FileText,
} from '@/api/files'
import ApiErrorPanel from '@/components/ApiErrorPanel.vue'
import FileEntryRow from '@/components/FileEntryRow.vue'
import StatePanel from '@/components/StatePanel.vue'
import { useSessionStore } from '@/stores/session'
import { formatBytes } from '@/utils/format'
import { apiPanelState } from '@/utils/viewstate'

type Phase = 'idle' | 'loading' | 'ready' | 'error'

const session = useSessionStore()

// ---------------------------------------------------------------- 根

// 初始就是 loading：进页面第一件事就是读根，空白不比"正在读"更真实。
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

const rootsPanelState = computed(() =>
  rootsError.value ? apiPanelState(rootsError.value) : null,
)

const rootCapacityText = computed(() => {
  const root = activeRoot.value
  if (root === null) return ''
  if (root.capacity === null) {
    return root.capacity_error ? `容量未读出：${root.capacity_error}` : '容量未读出'
  }
  const percent = root.capacity.used_percent
  return `${formatBytes(root.capacity.available_bytes)} 可用 / ${formatBytes(
    root.capacity.total_bytes,
  )}（已用 ${percent.toFixed(1)}%）`
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
  if (rootId.value !== null) {
    // 根还在就保留当前选择（刷新时不要把人弹回根目录之外的地方）。
    if (roots.value.some((root) => root.root_id === rootId.value)) return
  }
  const first = roots.value.find((root) => root.available) ?? null
  rootId.value = first === null ? null : first.root_id
  path.value = ''
  if (rootId.value !== null) void loadList('', { replace: true })
}

function selectRoot(value: unknown): void {
  // 只接受字符串：选择器理论上只会回 root_id，但界面不替服务端猜类型。
  if (typeof value !== 'string') return
  rootId.value = value
  path.value = ''
  resetBrowser()
  void loadList('', { replace: true })
}

/** 换根时要清掉上个根的残留（条目、游标、预览）。 */
function resetBrowser(): void {
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
/** 上一次读取用的版本：重读时带回去，服务端在句柄上核验。 */
const lastVersion = ref<string>('')
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
 * expectedVersion 只在“同一份文件再读一次”时才带：第一次点开已经拿到了服务端在
 * 列表里给的版本，带上它可以在“列表与点开之间被换掉”时立刻发现。
 * 用户主动点“重新读取”时不带（那是明确的“我要现在的内容”），
 * 但那时服务端打开句柄仍会做全部路径约束检查。
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
    lastVersion.value = result.data.version
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

onMounted(() => {
  if (session.isAdmin) void loadRoots()
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
      P06.1 只提供浏览、文本预览与下载，写操作需要预检与任务，尚未开放。
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
              <NSelect
                :value="rootId"
                :options="rootOptions"
                @update:value="selectRoot"
              />
            </label>
            <div class="files__rootinfo">
              <p class="files__line">
                绝对路径 <code>{{ activeRoot.path }}</code>
                <span v-if="activeRoot.mount_point">
                  · 挂载点 <code>{{ activeRoot.mount_point }}</code>
                </span>
                <span v-if="activeRoot.fs_type"> · {{ activeRoot.fs_type }}</span>
                <NTag size="small" :type="activeRoot.writable ? 'warning' : 'default'">
                  {{ activeRoot.writable ? '配置为可写（写操作未开放）' : '只读' }}
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
            <span class="files__meta">
              当前 <code>{{ path === '' ? '/' : `/${path}` }}</code>
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
                @enter="(item) => openDir(childPath(item.name))"
                @open="openFile"
                @save="save"
              />
            </tbody>
          </table>

          <div v-if="nextCursor" class="files__more">
            <NButton size="small" :loading="loadingMore" @click="loadMore">加载更多</NButton>
            <span class="files__meta">一页最多 {{ entries.length }} 项，说明后面还有条目</span>
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
            <NAlert
              v-if="versionConflict"
              type="warning"
              :bordered="false"
              title="文件已改变"
            >
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
              {{ formatBytes(text.size_bytes) }} · {{ text.encoding }} ·
              版本 <code>{{ text.version }}</code>
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

.files__name {
  max-width: 320px;
  word-break: break-all;
}

.files__ops {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 8px;
}

.files__dim {
  opacity: 0.7;
  font-size: 12px;
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
