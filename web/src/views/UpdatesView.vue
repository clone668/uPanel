<script setup lang="ts">
// 软件包与更新（P11.1，只读）。
//
// 这一页回答四个问题，四个问题的数据来自三条接口：
//   1. 这台机器上装了什么、哪些能升级、哪些是安全更新  → GET /packages
//      （counts 是全量统计；列表是分页的，两回事，不混着用）
//   2. 升级清单是什么样                                  → GET /packages/updates
//   3. 需不需要重启、重启标志是什么时候出现的             → GET /system/power-state
//   4. 哪一路来源读不到                                  → 三条接口里的 errors[]
//
// 三条纪律，每一条都对应一条真机验收：
//
//   - **变了什么就说什么**：`candidate_relation` 的四种情形各有说法。候选比已安装
//     **更旧**是真实情形（pin、回退、第三方源降级），`unknown` 是**读不到**，
//     都不能渲染成"已是最新"。
//   - **降级不是失败**：`errors[]` 非空时，列表照常显示，同时把"哪一路来源读不到、
//     为什么"写在最上面。把这种情况显示成空列表，是这一页最容易犯的谎。
//   - **不替 P11.2 承诺**：升级、安装、卸载、刷新索引都是 P11.2 的动作，有保护名单、
//     资源锁与非交互配置。这一页没有任何写按钮，也不是"忘了接"。
//
// 慢：代理侧 30 秒快照过期后的第一次读取要现采（24.04 实测 8～27 秒）。页面在
// 加载时把这句话说出来，而不是让用户对着转圈猜。

import { computed, onMounted, ref } from 'vue'

import { NAlert, NButton, NCard, NInput, NTag } from 'naive-ui'

import { ApiError } from '@/api/client'
import {
  CANDIDATE_RELATION_TYPES,
  candidateRelationLabel,
  candidateVersionText,
  entryDisplayName,
  entryPocketText,
  entryStatusLabel,
  fetchPackageUpdates,
  fetchPackages,
  fetchPowerState,
  packageSourceLabel,
  type PackageEntry,
  type PackageListData,
  type PackageUpdatesData,
  type PowerStateData,
} from '@/api/packages'
import ApiErrorPanel from '@/components/ApiErrorPanel.vue'
import StatTile from '@/components/StatTile.vue'
import StatePanel from '@/components/StatePanel.vue'
import { formatAge, formatTimestamp } from '@/utils/format'
import { apiPanelState, retryablePanelState } from '@/utils/viewstate'

type Phase = 'loading' | 'ready' | 'error'

/** 一页装多少。与服务端上限（200）留出余量：这一页是给人看的，不是导出。 */
const PAGE_SIZE = 50

const phase = ref<Phase>('loading')
const error = ref<ApiError | null>(null)

const list = ref<PackageListData | null>(null)
const updates = ref<PackageUpdatesData | null>(null)
const power = ref<PowerStateData | null>(null)

/** 搜索词。提交时才查询：边打字边查会把每一次按键都变成一次代理读取。 */
const keywordInput = ref('')
const keyword = ref('')
const filterInstalled = ref(true)
const filterUpgradable = ref(false)
const filterSecurity = ref(false)

/** 游标栈：cursor 是不透明的，只能"下一页"和"退回上一页"，没有跳页。 */
const cursors = ref<string[]>([''])
const pageIndex = ref(0)
const loadingPage = ref(false)
/** 整页三接口一起取的时候为 true（筛选用）。分页另有 loadingPage。 */
const busy = ref(false)

const panelState = computed(() => (error.value ? apiPanelState(error.value) : null))

const counts = computed(() => list.value?.counts ?? { installed: 0, upgradable: 0, security: 0 })
const errors = computed(() => list.value?.errors ?? [])

/** 全量来源错误（三条接口合并去重）：同一条来源失败可能在两处都出现。 */
const sourceErrors = computed(() => {
  const seen = new Set<string>()
  const out: { source: string; message: string }[] = []
  for (const item of [...errors.value, ...(updates.value?.errors ?? []), ...powerErrors.value]) {
    const key = `${item.source}\u0000${item.message}`
    if (seen.has(key)) continue
    seen.add(key)
    out.push(item)
  }
  return out
})

/** 重启标志的两条来源错误（不在 errors[] 里，是 power-state 自己的字段）。 */
const powerErrors = computed(() => {
  const view = power.value
  if (!view) return [] as { source: string; message: string }[]
  const out: { source: string; message: string }[] = []
  if (view.reboot_required_packages_error !== '') {
    out.push({
      source: 'reboot_pkgs',
      message: view.reboot_required_packages_error,
    })
  }
  if (view.boot_id_error !== '') {
    out.push({ source: 'boot_id', message: view.boot_id_error })
  }
  return out
})

const securityRows = computed(() => (updates.value?.updates ?? []).filter((row) => row.security))
const otherRows = computed(() => (updates.value?.updates ?? []).filter((row) => !row.security))

const indexLine = computed(() => {
  const at = list.value?.index_updated_at ?? null
  if (at === null) return '索引时间未知（读不到 /var/lib/apt/lists 的时间戳）'
  return `索引更新于 ${formatTimestamp(at)}（${formatAge(at)}）`
})

const powerLine = computed(() => {
  const view = power.value
  if (!view) return ''
  const boot = view.boot_id === '' ? '本次启动标识未知' : `本次启动 ${view.boot_id.slice(0, 8)}…`
  return `${boot} · 读取于 ${formatTimestamp(view.collected_at)}`
})

const hasNext = computed(() => (list.value?.next_cursor ?? null) !== null)

function entryKey(row: PackageEntry): string {
  return entryDisplayName(row)
}

function relationTagType(row: PackageEntry): 'default' | 'info' | 'success' | 'warning' {
  return CANDIDATE_RELATION_TYPES[row.candidate_relation] ?? 'default'
}

/** 三条接口一起取：任何一个失败都让整页进入错误状态，不给半份数据配一个绿勾。 */
async function load(): Promise<void> {
  phase.value = 'loading'
  error.value = null
  busy.value = true
  try {
    const [listResult, updatesResult, powerResult] = await Promise.all([
      fetchPackages({
        query: keyword.value,
        installed: filterInstalled.value,
        upgradable: filterUpgradable.value,
        security: filterSecurity.value,
        limit: PAGE_SIZE,
      }),
      fetchPackageUpdates(),
      fetchPowerState(),
    ])
    list.value = listResult.data
    updates.value = updatesResult.data
    power.value = powerResult.data
    cursors.value = ['']
    pageIndex.value = 0
    phase.value = 'ready'
  } catch (caught) {
    if (caught instanceof ApiError) {
      error.value = caught
      phase.value = 'error'
      return
    }
    throw caught
  } finally {
    busy.value = false
  }
}

/** 只换一页：不重取升级清单与重启标志，它们和分页无关。 */
async function loadPage(cursor: string, index: number): Promise<void> {
  loadingPage.value = true
  try {
    const result = await fetchPackages({
      query: keyword.value,
      installed: filterInstalled.value,
      upgradable: filterUpgradable.value,
      security: filterSecurity.value,
      limit: PAGE_SIZE,
      cursor,
    })
    list.value = result.data
    pageIndex.value = index
  } catch (caught) {
    if (caught instanceof ApiError) {
      error.value = caught
      phase.value = 'error'
      return
    }
    throw caught
  } finally {
    loadingPage.value = false
  }
}

async function nextPage(): Promise<void> {
  const cursor = list.value?.next_cursor ?? null
  if (cursor === null) return
  const index = pageIndex.value + 1
  cursors.value = [...cursors.value.slice(0, index), cursor]
  await loadPage(cursor, index)
}

async function prevPage(): Promise<void> {
  if (pageIndex.value === 0) return
  const index = pageIndex.value - 1
  await loadPage(cursors.value[index] ?? '', index)
}

async function applyFilters(): Promise<void> {
  keyword.value = keywordInput.value.trim()
  await load()
}

onMounted(() => {
  void load()
})
</script>

<template>
  <section class="updates">
    <div class="updates__head">
      <h1 class="updates__title">更新</h1>
      <NTag v-if="counts.security > 0" size="small" type="error"> {{ counts.security }} 个安全更新 </NTag>
      <NTag v-if="counts.upgradable > 0" size="small" type="warning"> {{ counts.upgradable }} 个可升级 </NTag>
      <NTag v-if="power?.reboot_required" size="small" type="warning">需要重启</NTag>
    </div>

    <StatePanel
      v-if="phase === 'loading'"
      state="loading"
      title="正在读取软件包事实"
      description="首次读取（或索引刚更新后）要现采一次候选版本，在 24.04 上实测 8～27 秒；之后走 30 秒快照，通常不到 1 秒。"
    />
    <StatePanel
      v-else-if="panelState && error && panelState !== 'error'"
      :state="panelState"
      :description="error.message"
      :code="error.code"
      :request-id="error.requestId"
      :retryable="retryablePanelState(panelState)"
      @retry="load"
    />
    <ApiErrorPanel v-else-if="error" :error="error" @retry="load" />

    <template v-else-if="list">
      <p class="updates__meta">
        采集于 {{ formatTimestamp(list.collected_at) }}（{{ formatAge(list.collected_at) }}） ·
        {{ indexLine }}
      </p>

      <!-- 降级来源：列表照出，但把"哪一路读不到"写在最前面。 -->
      <NAlert v-if="sourceErrors.length > 0" type="warning" :bordered="false">
        <p class="updates__alert-line">有来源读不到，下面显示的是<strong>降级后的</strong>事实：</p>
        <ul class="updates__alert-list">
          <li v-for="item in sourceErrors" :key="item.source + item.message">
            {{ packageSourceLabel(item.source) }}：{{ item.message }}
          </li>
        </ul>
        <p class="updates__alert-line">读不到的包不会拿已安装版本冒充候选版本，它们的候选关系显示为「候选读不到」。</p>
      </NAlert>

      <div class="updates__tiles">
        <StatTile
          label="已安装"
          :value="String(counts.installed)"
          hint="dpkg 数据库里状态为 ii 的包；不受下方筛选影响"
        />
        <StatTile
          label="可升级"
          :value="String(counts.upgradable)"
          hint="候选版本比已安装新；不代表升级时一定会升"
          :tone="counts.upgradable > 0 ? 'warning' : 'default'"
        />
        <StatTile
          label="安全更新"
          :value="String(counts.security)"
          hint="候选版本本身来自 -security 源且更新"
          :tone="counts.security > 0 ? 'error' : 'default'"
        />
      </div>

      <!-- 重启标志：面板只报告文件，不推断"装了内核所以要重启"。 -->
      <NCard size="small" title="重启标志">
        <p class="updates__line">
          <strong>{{ power?.reboot_required ? '系统请求重启' : '没有重启请求' }}</strong>
          —— 判据是
          <code>/run/reboot-required</code> 这个文件在不在，不是面板猜的。
        </p>
        <p v-if="power?.reboot_required" class="updates__line">
          标志出现于 {{ formatTimestamp(power.reboot_required_since) }}（{{
            formatAge(power.reboot_required_since)
          }}）。
        </p>
        <p v-if="power && power.reboot_required_packages.length > 0" class="updates__line">
          涉及的包（{{ power.reboot_required_packages.length }} 个，来自 <code>/run/reboot-required.pkgs</code>）：
          <span class="updates__suites">{{ power.reboot_required_packages.join('、') }}</span>
        </p>
        <p v-if="power" class="updates__line updates__meta">{{ powerLine }}</p>
      </NCard>

      <!-- 升级清单 -->
      <NCard size="small" title="可升级清单">
        <p class="updates__line">
          共 {{ updates?.total ?? 0 }} 个包有更新的候选版本，其中 {{ updates?.security ?? 0 }} 个是安全更新。
        </p>
        <NAlert v-if="updates?.truncated" type="warning" :bordered="false">
          超过 1000 个，清单只列了前 1000 条；未显示的部分不代表不存在。
        </NAlert>
        <StatePanel
          v-if="(updates?.updates.length ?? 0) === 0"
          state="empty"
          title="没有可升级的包"
          description="所有已安装包的候选版本都不比安装的新。索引若很久没更新，这份结论只对当时那份索引成立。"
        />
        <template v-else>
          <div v-if="securityRows.length > 0" class="updates__group">
            <h3 class="updates__subtitle">安全更新（{{ securityRows.length }}）</h3>
            <ul class="updates__rows">
              <li v-for="row in securityRows" :key="`sec-${row.name}:${row.architecture}`">
                <span class="updates__name">{{ entryDisplayName(row) }}</span>
                <span class="updates__versions"> {{ row.installed_version }} → {{ row.candidate_version }} </span>
                <span class="updates__suites">{{ row.suites.join(' ') }}</span>
              </li>
            </ul>
          </div>
          <div v-if="otherRows.length > 0" class="updates__group">
            <h3 class="updates__subtitle">其他更新（{{ otherRows.length }}）</h3>
            <ul class="updates__rows">
              <li v-for="row in otherRows" :key="`upd-${row.name}:${row.architecture}`">
                <span class="updates__name">{{ entryDisplayName(row) }}</span>
                <span class="updates__versions"> {{ row.installed_version }} → {{ row.candidate_version }} </span>
                <span class="updates__suites">{{ row.suites.join(' ') }}</span>
              </li>
            </ul>
          </div>
        </template>
      </NCard>

      <!-- 全部软件包 -->
      <NCard size="small" title="全部软件包">
        <div class="updates__filters">
          <NInput
            v-model:value="keywordInput"
            clearable
            placeholder="按包名筛选"
            @keyup.enter="applyFilters"
            @clear="applyFilters"
          />
          <NButton size="small" :loading="busy" @click="applyFilters">查询</NButton>
          <NButton
            size="small"
            :type="filterInstalled ? 'primary' : 'default'"
            @click="filterInstalled = !filterInstalled; applyFilters()"
          >
            只看已安装
          </NButton>
          <NButton
            size="small"
            :type="filterUpgradable ? 'primary' : 'default'"
            @click="filterUpgradable = !filterUpgradable; applyFilters()"
          >
            只看可升级
          </NButton>
          <NButton
            size="small"
            :type="filterSecurity ? 'primary' : 'default'"
            @click="filterSecurity = !filterSecurity; applyFilters()"
          >
            只看安全更新
          </NButton>
        </div>
        <p class="updates__line updates__meta">
          符合筛选的共 {{ list.matched }} 个，本页 {{ list.packages.length }} 个（每页
          {{ list.limit }}）。计数卡片是<strong>全量</strong>统计，不是本页统计。
        </p>

        <StatePanel
          v-if="list.packages.length === 0"
          state="empty"
          title="没有匹配的包"
          description="换一个包名，或取消筛选条件。"
        />

        <div v-else class="updates__table" data-testid="package-table">
          <div class="updates__row updates__row--head">
            <span>包名</span>
            <span>状态</span>
            <span>已安装版本</span>
            <span>候选版本</span>
            <span>关系</span>
            <span>来源</span>
          </div>
          <div
            v-for="row in list.packages"
            :key="entryKey(row)"
            class="updates__row"
            :data-relation="row.candidate_relation"
          >
            <span class="updates__name">{{ entryDisplayName(row) }}</span>
            <span>{{ entryStatusLabel(row) }}</span>
            <span class="updates__versions">{{ row.installed_version }}</span>
            <span class="updates__versions">{{ candidateVersionText(row) }}</span>
            <span>
              <NTag size="small" :type="relationTagType(row)">
                {{ candidateRelationLabel(row.candidate_relation) }}
              </NTag>
              <NTag v-if="row.security" size="small" type="error" class="updates__tag">安全更新</NTag>
            </span>
            <span class="updates__suites">{{ entryPocketText(row) }}</span>
          </div>
        </div>

        <div v-if="list.packages.length > 0" class="updates__pager">
          <NButton size="small" :disabled="pageIndex === 0 || loadingPage" @click="prevPage"> 上一页 </NButton>
          <span class="updates__meta">第 {{ pageIndex + 1 }} 页</span>
          <NButton size="small" :disabled="!hasNext || loadingPage" @click="nextPage"> 下一页 </NButton>
        </div>
      </NCard>

      <NAlert type="info" :bordered="false">
        这一页<strong>只读</strong>：安装、卸载、升级与刷新索引在 P11.2 开放，走预检 → 任务，
        带保护名单、资源锁与非交互配置。<strong>可升级</strong>的含义是「候选比已安装新」，
        不是「这次升级会升它」——分阶段推送与被保留的包要
        <code>apt-get -s upgrade</code> 的模拟才判得出，那也是 P11.2。
      </NAlert>
    </template>
  </section>
</template>

<style scoped>
.updates {
  display: flex;
  flex-direction: column;
  gap: 16px;
  max-width: 1200px;
}

.updates__head {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 10px;
}

.updates__title {
  margin: 0;
  font-size: 20px;
}

.updates__line {
  margin: 0;
  line-height: 1.7;
}

.updates__meta {
  font-size: 13px;
  opacity: 0.8;
  word-break: break-all;
}

.updates__alert-line {
  margin: 0 0 4px;
  line-height: 1.6;
}

.updates__alert-list {
  margin: 0 0 4px;
  padding-left: 20px;
  line-height: 1.6;
}

.updates__tiles {
  display: flex;
  flex-wrap: wrap;
  gap: 12px;
}

.updates__group {
  margin-top: 12px;
}

.updates__subtitle {
  margin: 0 0 6px;
  font-size: 14px;
  font-weight: 600;
}

.updates__rows {
  margin: 0;
  padding-left: 0;
  list-style: none;
  display: flex;
  flex-direction: column;
  gap: 4px;
}

.updates__rows li {
  display: flex;
  flex-wrap: wrap;
  gap: 10px;
  align-items: baseline;
  font-size: 13px;
}

.updates__name {
  font-family: var(--upanel-font-mono, monospace);
  min-width: 180px;
}

.updates__versions {
  font-family: var(--upanel-font-mono, monospace);
  opacity: 0.9;
}

.updates__suites {
  font-size: 12px;
  opacity: 0.7;
  word-break: break-all;
}

.updates__filters {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
  align-items: center;
  margin-bottom: 8px;
}

.updates__pager {
  display: flex;
  align-items: center;
  gap: 10px;
  margin-top: 8px;
}

.updates__table {
  display: flex;
  flex-direction: column;
  font-size: 13px;
  overflow-x: auto;
}

.updates__row {
  display: grid;
  grid-template-columns:
    minmax(200px, 1.4fr) 96px minmax(160px, 1fr) minmax(160px, 1fr)
    160px minmax(120px, 0.8fr);
  gap: 10px;
  align-items: center;
  padding: 6px 4px;
  border-bottom: 1px solid rgba(128, 128, 128, 0.16);
}

.updates__row--head {
  font-weight: 600;
  opacity: 0.75;
  font-size: 12px;
}

.updates__row[data-relation='newer'] {
  background: rgba(208, 139, 0, 0.06);
}

.updates__row[data-relation='older'] {
  background: rgba(64, 128, 255, 0.05);
}

.updates__tag {
  margin-left: 6px;
}
</style>
