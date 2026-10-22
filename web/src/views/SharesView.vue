<script setup lang="ts">
// 共享页（P08.4）。
//
// 这一页把“共享”拆成四块，因为它们是四件不同的事：
//   1. 能力：这台机器能不能跑 Samba、托管文件能不能写（不能写时下面全是解释，不是空列表）；
//   2. 共享定义与状态：每条共享此刻处在哪一步（配置没写 / 卷掉线 / 被运行时撤销 / 已写入）；
//   3. 受管理账号：能连进来的人；
//   4. 客户端怎么连：给客户端用的地址与命令（永远不含密码）。
//
// 状态不是一句话的绿灯：config_hash / directory_prepared / config_applied 三个字段
// 各答一个问题，页面上分开显示（CONTRACTS §7.1）。连接列表也是分开的：
// “没人连着”与“smbd 没在跑”必须看起来不一样。

import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { NButton, NCard, NCollapse, NCollapseItem, NTag } from 'naive-ui'

import { ApiError } from '@/api/client'
import { fetchVolumes, type Volume } from '@/api/volumes'
import {
  capabilityAvailable,
  getShareAccounts,
  getShareConnections,
  getShares,
  shareNoticeLabel,
  type ShareAccount,
  type ShareCapability,
  type ShareConnections,
  type ShareRecord,
} from '@/api/shares'
import ApiErrorPanel from '@/components/ApiErrorPanel.vue'
import ShareAccountPanel from '@/components/ShareAccountPanel.vue'
import ShareCard from '@/components/ShareCard.vue'
import ShareClientHelp from '@/components/ShareClientHelp.vue'
import ShareForm from '@/components/ShareForm.vue'
import StatePanel from '@/components/StatePanel.vue'
import StorageActionDialog from '@/components/StorageActionDialog.vue'
import { useSessionStore } from '@/stores/session'
import { summarizeShareJob } from '@/utils/jobresult'
import { formatTimestamp } from '@/utils/format'
import { createPoller, type Poller } from '@/utils/polling'
import { apiPanelState, retryablePanelState } from '@/utils/viewstate'
import type { ShareDefinition, ShareVolumeInfo } from '@/api/shares'

type Phase = 'loading' | 'ready' | 'error'

const DEFAULT_INTERVAL_MS = 10_000

const session = useSessionStore()
const phase = ref<Phase>('loading')
const error = ref<ApiError | null>(null)

const capability = ref<ShareCapability | null>(null)
const shares = ref<ShareRecord[]>([])
const accounts = ref<ShareAccount[]>([])
const volumes = ref<Volume[]>([])

const formOpen = ref(false)
const pendingCreate = ref<ShareDefinition | null>(null)
const createShow = ref(false)

const helpShare = ref<ShareRecord | null>(null)
const helpShow = ref(false)

/** 主机名取自人此刻正在用的地址，不从网卡表里猜（见 clientHints 的说明）。 */
const host = ref(typeof window === 'undefined' ? '' : window.location.hostname)

const connections = ref<Record<string, ShareConnections | null>>({})
const connectionErrors = ref<Record<string, string>>({})
const connectionsBusy = ref<Record<string, boolean>>({})

const panelState = computed(() => (error.value ? apiPanelState(error.value) : null))
const available = computed(() => capabilityAvailable(capability.value))
const managedCount = computed(() => accounts.value.length)
const enabledCount = computed(() => shares.value.filter((s) => s.definition.enabled).length)
const servingCount = computed(() => shares.value.filter((s) => s.config_applied).length)

/**
 * 共享记录里没有卷的状态（openapi 的 ShareRecord 没有这个字段），页面按 volume_id join。
 * 取不到就是 null：卡片会少一行“观测”，而不是把未知画成离线。
 */
const volumeById = computed(() => {
  const map = new Map<string, ShareVolumeInfo>()
  for (const v of volumes.value) {
    map.set(v.id, { id: v.id, name: v.display_name, state: v.observed_state })
  }
  return map
})

function volumeOf(share: ShareRecord): ShareVolumeInfo | null {
  return volumeById.value.get(share.definition.volume_id) ?? null
}

const referencedIds = computed(() => {
  const ids = new Set<string>()
  for (const share of shares.value) {
    for (const user of share.definition.smb_users) ids.add(user.account_id)
  }
  return [...ids]
})

const createLines = computed<string[]>(() => {
  const def = pendingCreate.value
  if (!def) return []
  const volume = volumes.value.find((v) => v.id === def.volume_id)
  return [
    `共享名：${def.name}（它会变成网络上的 \\\\<主机>\\${def.name}，建好之后不能改名）`,
    `卷：${volume?.display_name ?? def.volume_id}（${volume?.mount_path ?? '?'}）`,
    `目录：${def.relative_path === '' ? '卷根' : def.relative_path}`,
    `${def.smb_users.length} 个授权账号${def.read_only ? '，全部只读' : ''}`,
    def.allowed_cidrs.length === 0
      ? '不限来源网段'
      : `只允许 ${def.allowed_cidrs.join('、')}（Samba 对本机永远放行）`,
    '会建目录（0700）、写 POSIX ACL 并读回核验，然后把这一段写进托管配置并重载 Samba',
  ]
})

async function load(signal?: AbortSignal): Promise<void> {
  try {
    const [list, accountList] = await Promise.all([getShares(), getShareAccounts()])
    if (signal?.aborted) return
    capability.value = list.data.capability
    shares.value = list.data.shares
    accounts.value = accountList.data.accounts
    phase.value = 'ready'
    error.value = null
    // 卷的挂载状态是共享卡片上“观测”那一行的来源，每轮一起刷；读不到卷不影响这一页。
    void loadVolumes()
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

/** 卷列表用于表单里的选择、以及每张卡片上“卷此刻挂没挂”；读不到卷不该让整页失败。 */
async function loadVolumes(): Promise<void> {
  try {
    const result = await fetchVolumes()
    volumes.value = result.data.volumes
  } catch {
    volumes.value = []
  }
}

const poller: Poller = createPoller(load, {
  intervalMs: () => DEFAULT_INTERVAL_MS,
})

onMounted(() => {
  host.value = window.location.hostname
  // 共享的读接口是 admin-only（定义里有路径、账号名单与来源网段，合起来就是
  // “谁能访问哪个目录”的完整答案）。viewer 不去打一个注定 403 的请求，
  // 直接在页面上说清楚为什么看不到。
  if (!session.isAdmin) {
    phase.value = 'ready'
    return
  }
  void poller.start()
  void loadVolumes()
})

onBeforeUnmount(() => {
  poller.stop()
})

function openForm(): void {
  formOpen.value = true
  if (volumes.value.length === 0) void loadVolumes()
}

function onCreate(def: ShareDefinition): void {
  pendingCreate.value = def
  formOpen.value = false
  createShow.value = true
}

function onCreateFinished(): void {
  pendingCreate.value = null
  void load()
}

function onHelp(share: ShareRecord): void {
  helpShare.value = share
  helpShow.value = true
}

async function loadConnections(share: ShareRecord): Promise<void> {
  connectionsBusy.value = { ...connectionsBusy.value, [share.id]: true }
  const nextErrors = { ...connectionErrors.value }
  delete nextErrors[share.id]
  connectionErrors.value = nextErrors
  try {
    const result = await getShareConnections(share.id)
    connections.value = { ...connections.value, [share.id]: result.data }
  } catch (caught) {
    connectionErrors.value = {
      ...connectionErrors.value,
      [share.id]: caught instanceof ApiError ? caught.message : '读连接状态失败',
    }
  } finally {
    connectionsBusy.value = { ...connectionsBusy.value, [share.id]: false }
  }
}

function noticesOf(cap: ShareCapability | null): string[] {
  return (cap?.notices ?? []).map((n) => `${shareNoticeLabel(n.code)}（${n.code}）：${n.message}`)
}

function serviceLabel(service: ShareConnections['service']): string {
  switch (service) {
    case 'active':
      return 'smbd 在运行'
    case 'inactive':
      return 'smbd 没在运行'
    default:
      return 'smbd 状态未知'
  }
}
</script>

<template>
  <section class="shares">
    <div class="shares__head">
      <h1 class="shares__title">共享</h1>
      <NTag v-if="capability" size="small" :type="available ? 'success' : 'error'">
        Samba {{ available ? '可用' : '不可用' }}
      </NTag>
      <NTag v-if="capability" size="small">配置：{{ capability.config_path }}</NTag>
      <NTag v-if="capability" size="small" :type="capability.config_write === 'available' ? 'success' : 'warning'">
        写入：{{ capability.config_write }}
      </NTag>
      <NTag size="small">{{ shares.length }} 条共享</NTag>
      <NTag size="small">{{ managedCount }} 个受管理账号</NTag>
      <NButton v-if="session.isAdmin && available" size="small" type="primary" data-testid="share-new" @click="openForm">
        新建共享
      </NButton>
    </div>

    <StatePanel
      v-if="!session.isAdmin"
      state="forbidden"
      title="共享配置只对管理员可见"
      description="共享定义里带着目录路径、账号名单与来源网段，合起来就是“谁能访问哪个目录”的完整答案。只读账号在概览页能看到共享数量与健康摘要。"
    />
    <StatePanel v-else-if="phase === 'loading'" state="loading" title="正在读取共享与能力报告" />
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
      <NCard v-if="capability && !available" size="small" title="Samba 能力不可用">
        <p class="shares__line">
          原因：{{ capability.reason || '代理没有给出原因' }}。下面的共享定义仍然可读，
          但在能力恢复之前，应用动作会在预检阶段被拒（DEPENDENCY_BLOCKED）。
        </p>
        <ul v-if="noticesOf(capability).length > 0" class="shares__notices">
          <li v-for="n in noticesOf(capability)" :key="n">{{ n }}</li>
        </ul>
        <p class="shares__meta">采集于 {{ formatTimestamp(capability.collected_at) }}</p>
      </NCard>

      <NCard v-if="capability && available && capability.config_write !== 'available'" size="small" title="配置写入未启用">
        <p class="shares__line">
          能力报告里 <code>config_write = {{ capability.config_write }}</code>：
          这台机器能跑 Samba，但托管配置这一段写不进去。共享定义可以登记与准备目录，
          不会有任何一段真的写进 Samba 配置。
        </p>
      </NCard>

      <ShareForm
        v-if="formOpen"
        :accounts="accounts"
        :volumes="volumes"
        @submit="onCreate"
        @cancel="formOpen = false"
      />

      <NCard size="small" title="共享定义">
        <p v-if="shares.length === 0" class="shares__meta">
          还没有共享。新建一条之后，它会先准备目录与 ACL，再把这一段写进 Samba 配置。
        </p>
        <div v-else class="shares__list" data-testid="share-list">
          <ShareCard
            v-for="share in shares"
            :key="share.id"
            :share="share"
            :accounts="accounts"
            :volume="volumeOf(share)"
            :can-act="session.isAdmin"
            @changed="load()"
            @help="onHelp"
          />
        </div>
      </NCard>

      <ShareAccountPanel
        :accounts="accounts"
        :referenced-ids="referencedIds"
        :can-act="session.isAdmin"
        @changed="load()"
      />

      <NCard size="small" title="谁正连着">
        <p class="shares__meta">
          连接状态是某一瞬间的快照。<b>空列表有两种含义</b>：没人连着，或者 smbd 根本没在跑——
          下面把服务状态单独写出来，不用“0 个连接”冒充“一切正常”。
        </p>
        <NCollapse>
          <NCollapseItem v-for="share in shares" :key="share.id" :name="share.id" :title="share.definition.name">
            <div class="shares__conn">
              <NButton size="tiny" secondary :loading="connectionsBusy[share.id] === true" @click="loadConnections(share)">
                读取连接状态
              </NButton>

              <p v-if="connectionErrors[share.id]" class="shares__line shares__line--warn">
                {{ connectionErrors[share.id] }}
              </p>

              <template v-if="connections[share.id]">
                <p class="shares__line">
                  {{ serviceLabel(connections[share.id]!.service) }} ·
                  {{ connections[share.id]!.connections.length }} 个连接 ·
                  采集于 {{ formatTimestamp(connections[share.id]!.collected_at) }}
                </p>
                <ul v-if="connections[share.id]!.connections.length > 0" class="shares__conn-list">
                  <li v-for="c in connections[share.id]!.connections" :key="`${c.machine}-${c.username}-${c.connected_at}`">
                    <code>{{ c.machine }}</code> · 账号 {{ c.username }} · {{ c.dialect }} ·
                    签名 {{ c.signing }} · {{ c.open_files }} 个打开的文件 ·
                    自 {{ formatTimestamp(c.connected_at) }}
                  </li>
                </ul>
                <p v-else class="shares__meta">
                  此刻没有客户端连着这条共享<template v-if="connections[share.id]!.service !== 'active'">
                    ——但 smbd 也不是 active，所以这更像“服务没起来”而不是“没人用”</template>。
                </p>
              </template>
            </div>
          </NCollapseItem>
        </NCollapse>
        <p v-if="shares.length === 0" class="shares__meta">没有共享就没有连接。</p>
      </NCard>

      <NCard size="small" title="这一页的规矩">
        <p class="shares__line">
          共享的每一处写入都只有一条路：预检 → 任务。预检会核对卷此刻是不是挂着、
          UUID 对不对、同名共享是不是已经存在；判决不通过时按钮根本不出现。
        </p>
        <p class="shares__line">
          配置写在托管文件 <code>{{ capability?.config_path ?? '—' }}</code> 里，
          <code>smb.conf</code> 只多一行 include。每次写入都会先跑 testparm 校验候选文件、
          再备份、再替换、再重载，然后逐个参数读回。
        </p>
        <p class="shares__line">
          密码不进命令行、不进日志、不进数据库：<code>share.apply</code> 只带秘密引用，
          <code>smbpasswd</code> 从标准输入读明文。客户端说明里的挂载命令用
          <code>credentials=</code> 文件（0600），因为 <code>password=</code> 会留在进程列表里。
        </p>
        <p class="shares__line">
          共享到服务端的状态已经写出 {{ enabledCount }} 条启用、{{ servingCount }} 条已写入配置；
          两个数字不同是正常的——定义启用不等于那一段已经落进 Samba 配置。
        </p>
      </NCard>

      <StorageActionDialog
        v-if="pendingCreate"
        v-model:show="createShow"
        action="share.apply"
        :title="`新建共享「${pendingCreate.name}」`"
        :params="{ share_id: null, version: null, definition: pendingCreate }"
        :confirm-lines="createLines"
        :needs-reauth="true"
        :summarizer="summarizeShareJob"
        @finished="onCreateFinished"
      />

      <ShareClientHelp
        v-if="helpShare"
        v-model:show="helpShow"
        :share="helpShare"
        :host="host"
        :capability="capability"
      />
    </template>
  </section>
</template>

<style scoped>
.shares {
  display: flex;
  flex-direction: column;
  gap: 16px;
  max-width: 1100px;
}

.shares__head {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 8px;
}

.shares__title {
  margin: 0;
  font-size: 20px;
  margin-right: 6px;
}

.shares__line {
  margin: 0;
  line-height: 1.7;
}

.shares__line--warn {
  color: #d03050;
}

.shares__meta {
  font-size: 13px;
  line-height: 1.7;
  opacity: 0.8;
  word-break: break-all;
}

.shares__notices {
  margin: 8px 0 0;
  padding-left: 18px;
  font-size: 13px;
  line-height: 1.7;
}

.shares__list {
  display: flex;
  flex-direction: column;
  gap: 12px;
  margin-top: 8px;
}

.shares__conn {
  display: flex;
  flex-direction: column;
  gap: 8px;
  align-items: flex-start;
}

.shares__conn-list {
  margin: 0;
  padding-left: 18px;
  font-size: 13px;
  line-height: 1.7;
}
</style>
