<script setup lang="ts">
// 一条共享（P08.4）。
//
// 这一块要回答四个问题，而且**分开**回答，不合成一个“状态良好”的绿灯：
//   1. 定义是什么（名字、卷、相对路径、账号与访问级别、来源网段、只读）；
//   2. 三个准备状态各自到哪一步（config_hash / directory_prepared / config_applied）；
//   3. 此刻到底能不能连（运行时撤销、卷此刻挂没挂、配置写没写）。
//
// 卷的状态**不在**共享记录里（openapi 的 ShareRecord 没有这个字段，也不该有：卷是外部事实，
// 两处各存一份就等于两份会互相矛盾的真相）。它由页面从 `/storage/volumes` 的 observed_state
// 按 volume_id join 过来，通过 volume prop 传进这一块。卷列表还没取回来时传 null，
// 这时**不显示**“卷没挂”，只少一行——把未知画成离线会让人去查一个没坏的卷。
//
// 动作仍然只有一条路：预检 → 任务。按钮点下去先看服务端判决，页面不自己判断
// “卷挂着所以能应用”——挂载与 UUID 的核对在代理那边，页面看不到全部现场。

import { computed, ref } from 'vue'
import { NButton, NCard, NTag } from 'naive-ui'

import type { ShareAccount, ShareRecord, ShareVolumeInfo } from '@/api/shares'
import { SHARE_ACCESS_LABELS, summarizeShareState, clientHintsUsable } from '@/api/shares'
import StorageActionDialog from './StorageActionDialog.vue'
import { summarizeShareJob } from '@/utils/jobresult'
import { formatTimestamp } from '@/utils/format'

const props = defineProps<{
  share: ShareRecord
  accounts: readonly ShareAccount[]
  /** 这个共享绑定的卷此刻的现场（页面 join 出来的）；取不到时传 null。 */
  volume?: ShareVolumeInfo | null
  /** 只有 admin 能提交动作（读接口也是 admin，但按钮由调用方决定画不画）。 */
  canAct: boolean
}>()

const emit = defineEmits<{ changed: []; help: [ShareRecord] }>()

interface PendingAction {
  action: string
  title: string
  params: Record<string, unknown>
  lines: string[]
  irreversible: boolean
  needsReauth: boolean
}

const pending = ref<PendingAction | null>(null)
const dialogShow = ref(false)

const def = computed(() => props.share.definition)
const state = computed(() => summarizeShareState(props.share, props.volume ?? null))
const usable = computed(() => clientHintsUsable(props.share, props.volume ?? null))

const accountNames = computed(() => {
  const byID = new Map(props.accounts.map((a) => [a.id, a.name]))
  return def.value.smb_users.map((user) => ({
    name: byID.get(user.account_id) ?? user.account_id,
    access: SHARE_ACCESS_LABELS[user.access] ?? user.access,
  }))
})

const configAppliedLabel = computed(() => (props.share.config_applied ? '已写入并重载' : '未写入'))
const directoryLabel = computed(() => (props.share.directory_prepared ? '已准备并核验' : '未准备'))

function open(action: string, title: string, params: Record<string, unknown>, lines: string[], opts: { irreversible?: boolean; needsReauth?: boolean } = {}): void {
  pending.value = {
    action,
    title,
    params,
    lines,
    irreversible: opts.irreversible === true,
    needsReauth: opts.needsReauth === true,
  }
  dialogShow.value = true
}

function applyAgain(): void {
  open(
    'share.apply',
    `重新应用共享「${def.value.name}」`,
    {
      share_id: props.share.id,
      version: props.share.version,
      definition: { ...def.value, enabled: true },
    },
    [
      `共享：${def.value.name}`,
      `卷：${props.volume?.name || def.value.volume_id}`,
      `相对路径：${def.value.relative_path}`,
      '会把这一段重新渲染进托管配置并重载 Samba（内容没变就不写、不重载）',
      '同时按这份定义重写目录的 POSIX ACL',
    ],
    { needsReauth: true },
  )
}

function disable(): void {
  open(
    'share.disable',
    `停用共享「${def.value.name}」`,
    { share_id: props.share.id, version: props.share.version },
    [
      `共享：${def.value.name}`,
      '会把这一段从托管配置里摘掉并重载 Samba',
      '目录 ACL 会被清空（除属主之外谁都进不去）',
      '**目录与里面的文件不会被删除**',
    ],
  )
}

function remove(): void {
  open(
    'share.delete',
    `删除共享「${def.value.name}」`,
    { share_id: props.share.id, version: props.share.version },
    [
      `共享：${def.value.name}`,
      '会摘掉配置、清空 ACL、删掉面板里的这条登记',
      '**目录与里面的文件永远不会被删除**；同名共享要重新创建才会再出现',
    ],
    { irreversible: true, needsReauth: true },
  )
}

function onFinished(): void {
  emit('changed')
}
</script>

<template>
  <NCard size="small" :data-share="def.name">
    <template #header>
      <span class="share__title">
        {{ def.name }}
        <NTag size="small" :type="state.tone">{{ state.label }}</NTag>
        <NTag v-if="def.read_only" size="small" type="info">只读</NTag>
        <NTag v-if="!def.enabled" size="small">已停用</NTag>
        <NTag v-if="!usable && state.kind !== 'disabled'" size="small" type="warning">
          客户端此刻连不上
        </NTag>
      </span>
    </template>

    <div class="share__rows">
      <p class="share__row">
        <span class="share__label">卷</span>
        <span>{{ volume?.name || def.volume_id }}</span>
        <code class="share__meta">{{ def.volume_id }}</code>
        <NTag v-if="volume" size="small" :type="volume.state === 'mounted' ? 'success' : 'warning'">
          观测：{{ volume.state }}
        </NTag>
      </p>
      <p class="share__row">
        <span class="share__label">路径</span>
        <code>{{ def.relative_path || '.' }}</code>
        <span class="share__meta">（相对卷的挂载点）</span>
      </p>
      <p class="share__row">
        <span class="share__label">账号</span>
        <span v-if="accountNames.length === 0" class="share__meta">没有账号（这份定义不该能应用）</span>
        <template v-else>
          <NTag v-for="a in accountNames" :key="a.name" size="small">
            {{ a.name }} · {{ a.access }}
          </NTag>
        </template>
      </p>
      <p class="share__row">
        <span class="share__label">来源</span>
        <span v-if="def.allowed_cidrs.length === 0" class="share__meta">没有限制来源网段</span>
        <template v-else>
          <code v-for="cidr in def.allowed_cidrs" :key="cidr" class="share__meta">{{ cidr }}</code>
        </template>
      </p>
      <p class="share__row">
        <span class="share__label">准备状态</span>
        <code class="share__meta">{{ share.config_hash.slice(0, 16) }}…</code>
        <span class="share__meta">目录与 ACL：{{ directoryLabel }}</span>
        <span class="share__meta">配置：{{ configAppliedLabel }}</span>
      </p>
    </div>

    <p class="share__hint">{{ state.detail }}</p>

    <p v-if="share.runtime_revoked" class="share__revoked" data-testid="share-revoked">
      撤销现场：{{ share.runtime_revoked.reason }}（{{ formatTimestamp(share.runtime_revoked.at) }}，
      当时卷状态 {{ share.runtime_revoked.volume_state }}）。卷回来之后不会自动恢复——
      点「重新应用」才会把它写回配置。
    </p>

    <p class="share__meta">最后更新 {{ formatTimestamp(share.updated_at) }}</p>

    <div v-if="canAct" class="share__actions">
      <NButton size="small" type="primary" data-testid="share-apply" @click="applyAgain">
        {{ share.config_applied ? '重新应用' : '应用' }}
      </NButton>
      <NButton size="small" secondary :disabled="!def.enabled" @click="disable">停用</NButton>
      <NButton size="small" secondary type="error" data-testid="share-delete" @click="remove">
        删除
      </NButton>
      <NButton size="small" tertiary @click="emit('help', share)">客户端怎么连</NButton>
    </div>
    <p v-else class="share__meta">只读账号可以看共享与连接状态；应用、停用、删除都要求管理员。</p>

    <StorageActionDialog
      v-if="pending"
      v-model:show="dialogShow"
      :action="pending.action"
      :title="pending.title"
      :params="pending.params"
      :confirm-lines="pending.lines"
      :irreversible="pending.irreversible"
      :needs-reauth="pending.needsReauth"
      :summarizer="summarizeShareJob"
      @finished="onFinished"
    />
  </NCard>
</template>

<style scoped>
.share__title {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}

.share__rows {
  display: flex;
  flex-direction: column;
  gap: 4px;
}

.share__row {
  display: flex;
  align-items: baseline;
  gap: 8px;
  margin: 0;
  line-height: 1.7;
  flex-wrap: wrap;
}

.share__label {
  min-width: 72px;
  font-size: var(--fs-md);
  opacity: 0.7;
}

.share__meta {
  font-size: var(--fs-md);
  opacity: 0.8;
  word-break: break-all;
}

.share__hint {
  margin: 8px 0 0;
  font-size: var(--fs-md);
  line-height: 1.7;
}

.share__revoked {
  margin: 8px 0 0;
  padding: 8px 10px;
  font-size: var(--fs-md);
  line-height: 1.7;
  border-left: 3px solid var(--warn);
  background: var(--warn-soft);
}

.share__actions {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
  margin-top: 10px;
}
</style>
