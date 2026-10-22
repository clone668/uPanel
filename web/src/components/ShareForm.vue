<script setup lang="ts">
// 新建共享的表单（P08.4）。
//
// 校验的规矩：**这里只挡“一眼就不对”的输入**，判决权仍在服务端的预检。
// 规则本身放在 ./shares.ts 的 validateShareForm 里——纯函数，能在没有 DOM 的地方测，
// 也能保证表单与预检用的是同一套说法（名字字符集、相对路径形状、空账号名单的唯一权威
// 在 internal/shares/definition.go；那边仍然是否决者）。

import { computed, reactive, ref } from 'vue'
import { NAlert, NButton, NCard, NInput, NSelect, NSwitch, NTag } from 'naive-ui'

import type { ShareAccess, ShareAccount, ShareDefinition } from '@/api/shares'
import { SHARE_ACCESS_LABELS, SHARE_ACCESS_LEVELS } from '@/api/shares'
import type { Volume } from '@/api/volumes'
import {
  buildShareDefinition,
  emptyShareForm,
  parseCidrs,
  selectedUsers,
  validateShareForm,
  type ShareFormState,
} from './shares'

const props = defineProps<{
  accounts: readonly ShareAccount[]
  volumes: readonly Volume[]
  busy?: boolean
}>()

const emit = defineEmits<{ submit: [ShareDefinition]; cancel: [] }>()

const form = reactive<ShareFormState>(emptyShareForm())

const touched = ref(false)

const volumeOptions = computed(() =>
  props.volumes.map((v) => ({
    label: `${v.display_name} · ${v.mount_path}${v.observed_state === 'mounted' ? '' : `（${v.observed_state}）`}`,
    value: v.id,
  })),
)

const accountOptions = computed(() =>
  props.accounts
    .filter((a) => !a.disabled)
    .map((a) => ({
      label: `${a.name}${a.has_password ? '' : '（未设 SMB 密码）'}`,
      value: a.id,
    })),
)

const selectedVolume = computed(() => props.volumes.find((v) => v.id === form.volume_id) ?? null)

const smbUsers = computed(() => selectedUsers(form))
const cidrs = computed(() => parseCidrs(form.allowed_cidrs))
const errors = computed(() => validateShareForm(form))
const canSubmit = computed(() => errors.value.length === 0 && props.busy !== true)

function submit(): void {
  touched.value = true
  if (!canSubmit.value) return
  emit('submit', buildShareDefinition(form))
}

function toggleAccess(accountId: string, value: string): void {
  form.access[accountId] = value === 'read' || value === 'write' ? (value as ShareAccess) : ''
}

const accessValue = (accountId: string): string => form.access[accountId] ?? ''

const accessOptions = [
  { label: '不授权', value: '' },
  ...SHARE_ACCESS_LEVELS.map((level) => ({ label: SHARE_ACCESS_LABELS[level], value: level })),
]
</script>

<template>
  <NCard size="small" title="新建共享" class="form">
    <p class="form__muted">
      共享把卷里的一个目录挂到网络上。这里填的每一条都会先交给服务端预检；
      预检说不行就不会有任何写入。
    </p>

    <div class="form__field">
      <label class="form__label" for="share-name">共享名</label>
      <NInput
        id="share-name"
        v-model:value="form.name"
        placeholder="例如 data 或 team-docs"
        data-testid="share-name"
      />
      <p class="form__muted">
        字母、数字、下划线、连字符，以字母或数字开头，最长 64 字节。它会变成网络上的
        <code>\\服务器\名字</code>，所以<b>建好之后不能改名</b>：客户端记的就是这个名字。
      </p>
    </div>

    <div class="form__field">
      <label class="form__label" for="share-volume">卷</label>
      <NSelect
        id="share-volume"
        :value="form.volume_id"
        :options="volumeOptions"
        placeholder="选一个受管理卷"
        data-testid="share-volume"
        @update:value="(v: string) => (form.volume_id = v)"
      />
      <p v-if="selectedVolume" class="form__muted">
        目录会建在 <code>{{ selectedVolume.mount_path }}</code> 下。
        卷此刻是 <b>{{ selectedVolume.observed_state }}</b>；
        <template v-if="selectedVolume.observed_state !== 'mounted'">
          不是 mounted 时应用会被拒（DEPENDENCY_BLOCKED）——先把卷挂上。
        </template>
      </p>
    </div>

    <div class="form__field">
      <label class="form__label" for="share-path">相对路径</label>
      <NInput
        id="share-path"
        v-model:value="form.relative_path"
        placeholder="留空表示整个卷根，例如 docs/sub"
        data-testid="share-path"
      />
      <p class="form__muted">
        相对卷的挂载点。留空就是卷根——单卷单共享时最常见的用法。目录不存在会创建（0700）。
      </p>
    </div>

    <div class="form__field">
      <span class="form__label">只读</span>
      <NSwitch v-model:value="form.read_only" data-testid="share-read-only" />
      <p class="form__muted">
        打开时所有账号的 ACL 上限都是 <code>r-x</code>，共享段也写 <code>read only = yes</code>。
        它比账号级授权更宽：任一处只读就是只读。
      </p>
    </div>

    <div class="form__field">
      <span class="form__label">授权账号</span>
      <p v-if="accountOptions.length === 0" class="form__muted">
        还没有可用的共享账号。先到下面的“受管理账号”里建一个（它会同时建 POSIX 用户和 SMB 密码）。
      </p>
      <div v-else class="form__accounts">
        <div v-for="acc in accountOptions" :key="acc.value" class="form__account">
          <span class="form__account-name">{{ acc.label }}</span>
          <NSelect
            :value="accessValue(acc.value)"
            :options="accessOptions"
            size="small"
            class="form__account-access"
            :data-account="acc.value"
            @update:value="(v: string) => toggleAccess(acc.value, v)"
          />
        </div>
      </div>
      <p class="form__muted">
        空名单是硬拒绝（SHARE_NO_USERS）：没有授权账号的共享在 Samba 里会退化成 guest，
        而 guest 访问在本项目里是禁止的。
      </p>
    </div>

    <div class="form__field">
      <label class="form__label" for="share-cidrs">来源网段</label>
      <NInput
        id="share-cidrs"
        v-model:value="form.allowed_cidrs"
        type="textarea"
        :autosize="{ minRows: 2, maxRows: 4 }"
        placeholder="留空不限制，例如 192.168.1.0/24 10.0.0.0/8"
        data-testid="share-cidrs"
      />
      <p class="form__muted">
        逗号或空格分隔。<b>名单管不住本机</b>：Samba 对 127.0.0.1 永远放行，
        所以面板自己的连通性检查才能做。
      </p>
    </div>

    <NAlert v-if="touched && errors.length > 0" type="warning" :bordered="false" title="还不能提交">
      <ul class="form__errors">
        <li v-for="err in errors" :key="err">{{ err }}</li>
      </ul>
    </NAlert>

    <div class="form__tags">
      <NTag size="small">protocol: smb</NTag>
      <NTag size="small">{{ smbUsers.length }} 个账号</NTag>
      <NTag v-if="cidrs.length === 0" size="small" type="warning">不限来源</NTag>
    </div>

    <div class="form__actions">
      <NButton secondary :disabled="busy === true" @click="emit('cancel')">取消</NButton>
      <NButton
        type="primary"
        :disabled="!canSubmit"
        data-testid="share-submit"
        @click="submit"
      >
        去预检
      </NButton>
    </div>
  </NCard>
</template>

<style scoped>
.form {
  display: flex;
  flex-direction: column;
  gap: 14px;
}

.form__field {
  display: flex;
  flex-direction: column;
  gap: 6px;
}

.form__label {
  font-weight: 600;
  font-size: 13px;
}

.form__muted {
  margin: 0;
  font-size: 13px;
  line-height: 1.7;
  opacity: 0.8;
}

.form__accounts {
  display: flex;
  flex-direction: column;
  gap: 6px;
}

.form__account {
  display: flex;
  align-items: center;
  gap: 10px;
}

.form__account-name {
  flex: 1;
  font-size: 13px;
  word-break: break-all;
}

.form__account-access {
  width: 110px;
  flex: none;
}

.form__errors {
  margin: 0;
  padding-left: 18px;
  line-height: 1.7;
}

.form__tags {
  display: flex;
  gap: 6px;
  flex-wrap: wrap;
}

.form__actions {
  display: flex;
  justify-content: flex-end;
  gap: 8px;
}
</style>
