<script setup lang="ts">
// 应用的新建 / 编辑表单（P09.5）。
//
// 这个表单不自己拼请求体：草稿 → AppDefinition / AppUpdate 的转换在 api/apps.ts 里，
// 与校对函数放在一起，因此可以被单测直接钉住（尤其是“切了 type 之后多余的绑定字段
// 必须丢掉”这一条，它对应服务端的一律 400）。
//
// 两处刻意的限制：
//   1. **探测目标只能从发现结果里选**。target_id 的形状（local_… / cntr_…）在服务端是
//      结构性约束——这里没有“填一个 IP”的输入框，不是填了再校验，而是没有地方填；
//   2. **入口地址与探测目标分开两个字段**。把 public_url 当探测目标会让面板从一个
//      不可信的对外地址上请求自己；把入口写成 target_id 则没人能点开。
//
// 本机发现取不到（代理不可用）时不假装“本机什么都没有”：健康检查那一块会说明原因并
// 停在“未启用”，别的字段照常可编辑。

import { computed, ref, watch } from 'vue'
import {
  NAlert,
  NButton,
  NCard,
  NInput,
  NInputNumber,
  NSelect,
  NSwitch,
  NTag,
} from 'naive-ui'

import {
  APP_ICON_KEYS,
  APP_ICON_LABELS,
  APP_TYPES,
  APP_TYPE_LABELS,
  DISCOVERY_SOURCE_LABELS,
  cloneDraft,
  emptyDraft,
  draftFromApp,
  validateDraft,
  type App,
  type AppDiscoveryCandidate,
  type AppDraft,
  type AppGroup,
  type AppType,
  type HealthScheme,
} from '@/api/apps'
import type { Volume } from '@/api/volumes'

const props = defineProps<{
  /** 编辑已有应用时给出；null 表示新建。 */
  app: App | null
  /** 新建时的预填草稿（从本机候选确认时用）。 */
  initialDraft?: AppDraft | null
  groups: AppGroup[]
  volumes: Volume[]
  /** 本机发现结果；null 表示还没读到（或读失败，原因见 discoveryNote）。 */
  candidates: AppDiscoveryCandidate[]
  /** 读不到发现结果时的说明；空串表示读到了。 */
  discoveryNote: string
  busy?: boolean
  /** 提交失败时的说明（来自服务端，已翻译成人话）。 */
  errorMessage?: string | null
}>()

const emit = defineEmits<{
  submit: [draft: AppDraft]
  cancel: []
}>()

function startDraft(app: App | null, preset: AppDraft | null | undefined): AppDraft {
  if (app !== null) return draftFromApp(app)
  if (preset !== null && preset !== undefined) return cloneDraft(preset)
  return emptyDraft()
}

const draft = ref<AppDraft>(startDraft(props.app, props.initialDraft))
const localError = ref<string | null>(null)

watch(
  () => [props.app, props.initialDraft] as const,
  ([next]) => {
    draft.value = startDraft(next, props.initialDraft)
    expectedCodesText.value = draft.value.health_check.expected_status_codes.join(', ')
    localError.value = null
  },
)

const isEdit = computed(() => props.app !== null)

/**
 * 三个绑定字段各自的读写器。
 *
 * 读：永远是字符串（没填就是空串），因为输入框的 value 不能是 undefined。
 * 写：**整份替换**绑定对象——一个应用最多只绑一个对象，改其中一格就不该留着另外两格。
 */
const unitValue = computed<string>({
  get: () => draft.value.binding.unit ?? '',
  set: (value) => {
    draft.value.binding = { unit: value }
  },
})
const containerValue = computed<string>({
  get: () => draft.value.binding.container_id ?? '',
  set: (value) => {
    draft.value.binding = { container_id: value }
  },
})
const deploymentValue = computed<string>({
  get: () => draft.value.binding.deployment_id ?? '',
  set: (value) => {
    draft.value.binding = { deployment_id: value }
  },
})

const typeOptions = APP_TYPES.map((type) => ({ label: APP_TYPE_LABELS[type], value: type }))
const iconOptions = APP_ICON_KEYS.map((key) => ({ label: APP_ICON_LABELS[key], value: key }))
const groupOptions = computed(() => [
  { label: '不分组', value: '' },
  ...props.groups.map((group) => ({ label: group.name, value: group.id })),
])
const volumeOptions = computed(() =>
  props.volumes.map((volume) => ({
    label: `${volume.display_name}（${volume.mount_path}）· ${volume.observed_state}`,
    value: volume.id,
  })),
)

// 探测目标的可选集合按应用类型收窄：systemd 应用只能探单元，docker 应用只能探容器，
// link 只能探本机裸监听。混着选会在服务端被判“目标与绑定不匹配”。
const targetKind = computed<'systemd' | 'docker' | 'local'>(() => {
  switch (draft.value.type) {
    case 'systemd':
      return 'systemd'
    case 'docker':
      return 'docker'
    default:
      return 'local'
  }
})

const targetOptions = computed(() =>
  props.candidates
    .filter((candidate) => candidate.kind === targetKind.value)
    .map((candidate) => ({
      label: `${candidate.title} · ${DISCOVERY_SOURCE_LABELS[candidate.source]} · ${candidate.observed_state}${candidate.probe_available ? '' : `（不可探测：${candidate.probe_note}）`}`,
      value: candidate.target_id,
      disabled: !candidate.probe_available,
    })),
)

const expectedCodesText = ref(draft.value.health_check.expected_status_codes.join(', '))

watch(
  () => draft.value.health_check.enabled,
  (enabled) => {
    // 关掉健康检查时把目标清空：服务端要求 enabled=false 时 target_id 是空串。
    if (!enabled) {
      draft.value.health_check = { ...draft.value.health_check, target_id: '', port: 0 }
    }
  },
)

function onTypeChange(type: AppType): void {
  draft.value.type = type
  // 切类型后旧的绑定值留在草稿里会被服务端判成“同时绑定两个类型”，这里立刻清掉。
  draft.value.binding = {}
  if (type === 'link' && draft.value.health_check.enabled && targetKind.value === 'local') {
    // link 没有运行时，探测只能是本机监听；已选的单元/容器目标不再适用。
    draft.value.health_check = { ...draft.value.health_check, target_id: '' }
  }
}

function onTargetChange(targetId: string): void {
  draft.value.health_check = { ...draft.value.health_check, target_id: targetId }
  const candidate = props.candidates.find((item) => item.target_id === targetId)
  if (!candidate) return
  // 端口与协议按候选建议值预填：它们是代理算出来的（例如容器的已发布端口），
  // 让人手抄一个端口号只会多一个错。
  draft.value.health_check = {
    ...draft.value.health_check,
    scheme: candidate.suggested_scheme,
    port: candidate.suggested_port,
  }
}

function parseCodes(): number[] {
  const codes = expectedCodesText.value
    .split(/[\s,，]+/)
    .map((part) => part.trim())
    .filter((part) => part !== '')
    .map((part) => Number(part))
    .filter((value) => Number.isInteger(value) && value >= 100 && value <= 599)
  return [...new Set(codes)].sort((a, b) => a - b)
}

function onGroupChange(value: string): void {
  draft.value.group_id = value === '' ? null : value
}

function onSchemeChange(value: string): void {
  draft.value.health_check = { ...draft.value.health_check, scheme: value as HealthScheme }
}

function onSubmit(): void {
  draft.value.health_check = {
    ...draft.value.health_check,
    expected_status_codes: parseCodes(),
  }
  const problem = validateDraft(draft.value)
  localError.value = problem
  if (problem !== null) return
  emit('submit', draft.value)
}
</script>

<template>
  <NCard size="small" :title="isEdit ? `编辑「${app?.name}」` : '新建应用'">
    <div class="form" data-testid="app-form">
      <div class="form__row">
        <label class="form__label">名称</label>
        <NInput v-model:value="draft.name" placeholder="例如 面板入口、Grafana" maxlength="80" show-count />
      </div>

      <div class="form__row">
        <label class="form__label">类型</label>
        <NSelect
          :value="draft.type"
          :options="typeOptions"
          data-testid="app-type"
          @update:value="onTypeChange"
        />
        <p class="form__hint">
          类型决定绑定的唯一合法形状；改类型必须同时改绑定，页面已经为你清空了旧绑定。
        </p>
      </div>

      <div v-if="draft.type === 'systemd'" class="form__row">
        <label class="form__label">systemd 单元</label>
        <NInput v-model:value="unitValue" placeholder="nginx.service" />
        <p class="form__hint">必须以 .service 结尾。可从下面“本机候选”里确认一个，无需手抄。</p>
      </div>

      <div v-else-if="draft.type === 'docker'" class="form__row">
        <label class="form__label">容器 ID</label>
        <NInput v-model:value="containerValue" placeholder="64 位完整容器 ID" />
        <p class="form__hint">
          必须是完整 ID：容器被重建后 ID 会变，旧绑定会要求重新确认。
        </p>
      </div>

      <div v-else-if="draft.type === 'compose'" class="form__row">
        <label class="form__label">部署标识</label>
        <NInput v-model:value="deploymentValue" placeholder="dep_…" />
        <p class="form__hint">Compose 部署能力在 1.0 才落地；现在登记不会带来启停能力。</p>
      </div>

      <div class="form__row">
        <label class="form__label">入口地址</label>
        <NInput
          v-model:value="draft.public_url"
          placeholder="http://192.168.1.10:8080/（可留空）"
        />
        <p class="form__hint">
          {{ draft.type === 'link' ? 'link 类型必填：没有入口的链接点不开。' : '可以不填；填了就是一键打开的目标。' }}
          只接受 http/https，不能含用户名密码。
        </p>
      </div>

      <div class="form__row">
        <label class="form__label">说明</label>
        <NInput
          v-model:value="draft.description"
          type="textarea"
          :autosize="{ minRows: 2, maxRows: 5 }"
          maxlength="1000"
          placeholder="给人看的用途说明（不含秘密）"
        />
      </div>

      <div class="form__row form__row--inline">
        <label class="form__label">图标</label>
        <NSelect v-model:value="draft.icon_key" :options="iconOptions" class="form__inline" />
        <label class="form__label">分组</label>
        <NSelect
          :value="draft.group_id ?? ''"
          :options="groupOptions"
          class="form__inline"
          @update:value="onGroupChange"
        />
      </div>

      <div class="form__row">
        <label class="form__label">依赖卷</label>
        <NSelect
          v-model:value="draft.volume_dependencies"
          multiple
          :options="volumeOptions"
          placeholder="启动这个应用之前，这些卷必须先挂上"
        />
        <p class="form__hint">
          依赖卷掉线时，受管理的那条启动路径会被预检拦下（DEPENDENCY_BLOCKED）；不受管理的
          路径不会，卡片上会写明。
        </p>
      </div>

      <div class="form__row form__row--inline">
        <label class="form__label">健康检查</label>
        <NSwitch v-model:value="draft.health_check.enabled" />
        <span class="form__hint">
          未启用时这一项永远显示“未启用”，不会因为运行时在跑就猜一个“正常”。
        </span>
      </div>

      <template v-if="draft.health_check.enabled">
        <NAlert v-if="discoveryNote !== ''" type="warning" :bordered="false">
          本机发现读不到：{{ discoveryNote }}。探测目标只能从发现结果里选，所以现在选不了；
          可以先不启用健康检查，稍后再改。
        </NAlert>

        <div class="form__row">
          <label class="form__label">探测目标</label>
          <NSelect
            :value="draft.health_check.target_id"
            :options="targetOptions"
            :disabled="targetOptions.length === 0"
            placeholder="只能选本机发现到的目标（没有填 IP 的地方）"
            data-testid="app-target"
            @update:value="onTargetChange"
          />
          <p class="form__hint">
            探测地址由代理拼成 127.0.0.1:端口；主机名、内网其它地址、云元数据地址在这条
            路径上没有出现的位置。
          </p>
        </div>

        <div class="form__row form__row--inline">
          <label class="form__label">协议</label>
          <NSelect
            :value="draft.health_check.scheme"
            :options="[
              { label: 'http', value: 'http' },
              { label: 'https', value: 'https' },
            ]"
            class="form__inline"
            @update:value="onSchemeChange"
          />
          <label class="form__label">端口</label>
          <NInputNumber v-model:value="draft.health_check.port" :min="0" :max="65535" class="form__inline" />
          <label class="form__label">路径</label>
          <NInput v-model:value="draft.health_check.path" placeholder="/" class="form__inline" />
        </div>

        <div class="form__row form__row--inline">
          <label class="form__label">期望状态码</label>
          <NInput
            v-model:value="expectedCodesText"
            placeholder="留空 = 200—399"
            class="form__inline"
          />
          <label class="form__label">间隔(秒)</label>
          <NInputNumber
            v-model:value="draft.health_check.interval_seconds"
            :min="30"
            :max="300"
            class="form__inline"
          />
          <label class="form__label">超时(秒)</label>
          <NInputNumber
            v-model:value="draft.health_check.timeout_seconds"
            :min="1"
            :max="10"
            class="form__inline"
          />
        </div>
      </template>

      <NAlert v-if="localError" type="error" :title="localError" />
      <NAlert v-if="errorMessage" type="warning" :title="errorMessage" />

      <div class="form__buttons">
        <NButton type="primary" :loading="busy === true" @click="onSubmit">
          {{ isEdit ? '保存' : '创建' }}
        </NButton>
        <NButton secondary :disabled="busy === true" @click="emit('cancel')">取消</NButton>
        <NTag v-if="isEdit && app" size="small">当前版本 {{ app.version }}</NTag>
      </div>
    </div>
  </NCard>
</template>

<style scoped>
.form {
  display: flex;
  flex-direction: column;
  gap: 12px;
}

.form__row {
  display: flex;
  flex-direction: column;
  gap: 6px;
}

.form__row--inline {
  flex-direction: row;
  align-items: center;
  flex-wrap: wrap;
  gap: 8px;
}

.form__label {
  font-size: var(--fs-md);
  font-weight: 600;
}

.form__inline {
  min-width: 140px;
  flex: 0 1 auto;
}

.form__hint {
  margin: 0;
  font-size: var(--fs-sm);
  line-height: 1.6;
  opacity: 0.78;
}

.form__buttons {
  display: flex;
  align-items: center;
  gap: 10px;
}
</style>
