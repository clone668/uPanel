<script setup lang="ts">
// 新建共享的表单（P08.4；P14.4 起 smb / nfs 两种协议）。
//
// 校验的规矩：**这里只挡“一眼就不对”的输入**，判决权仍在服务端的预检。
// 规则本身放在 ./shares.ts 的 validateShareForm 里——纯函数，能在没有 DOM 的地方测，
// 也能保证表单与预检用的是同一套说法（名字字符集、相对路径形状、空账号名单、
// NFS 必须点名网段这四条的唯一权威在 internal/shares；那边仍然是否决者）。
//
// 两个协议的字段不是“多几个少几个”，而是**语义不同**：SMB 的访问控制是账号 + ACL，
// NFS 的是网段 + root_squash。所以这里按协议切换整块，而不是把四五个字段灰掉——
// 灰掉的字段会让人以为“填了也许有用”，而它其实根本不会被写下去。

import { computed, reactive, ref } from 'vue'
import { NAlert, NButton, NCard, NInput, NSelect, NSwitch, NTag } from 'naive-ui'

import type { ShareAccess, ShareAccount, ShareDefinition, ShareProtocol } from '@/api/shares'
import { SHARE_ACCESS_LABELS, SHARE_ACCESS_LEVELS, SHARE_PROTOCOL_LABELS } from '@/api/shares'
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
  /**
   * 表单打开时的协议，默认 smb。
   *
   * 留这个口是因为“新建共享”与“新建 NFS 导出”在体验上本就该是两个入口——面板现在只有
   * 一个按钮，所以调用方暂时都不传；但协议必须是**初始状态**而不是内部常量，否则 NFS 那一半
   * 界面就没有任何一条路径能渲染出来，也就没人能测到它到底说了什么。
   */
  initialProtocol?: ShareProtocol
}>()

const emit = defineEmits<{ submit: [ShareDefinition]; cancel: [] }>()

const form = reactive<ShareFormState>({ ...emptyShareForm(), protocol: props.initialProtocol ?? 'smb' })

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

const isNFS = computed(() => form.protocol === 'nfs')

const protocolOptions = (['smb', 'nfs'] as const).map((p) => ({
  label: SHARE_PROTOCOL_LABELS[p],
  value: p,
}))

function selectProtocol(value: string): void {
  form.protocol = value === 'nfs' ? 'nfs' : 'smb'
}

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
      <span class="form__label">协议</span>
      <NSelect
        :value="form.protocol"
        :options="protocolOptions"
        data-testid="share-protocol"
        @update:value="selectProtocol"
      />
      <p class="form__muted">
        <template v-if="isNFS">
          NFS 导出：访问控制是<b>网段 + root_squash</b>，没有账号这一层。
          面板只维护 <code>/etc/exports.d/upanel.exports</code> 一份文件。
        </template>
        <template v-else>
          SMB 共享：访问控制是<b>受管理账号 + POSIX ACL</b>，走 Samba 的托管配置。
        </template>
      </p>
    </div>

    <div class="form__field">
      <label class="form__label" for="share-name">共享名</label>
      <NInput
        id="share-name"
        v-model:value="form.name"
        placeholder="例如 data 或 team-docs"
        data-testid="share-name"
      />
      <p class="form__muted">
        字母、数字、下划线、连字符，以字母或数字开头，最长 64 字节。
        <template v-if="isNFS">
          它只是面板里的名字（导出按<b>目录</b>认，不按名字），但<b>建好之后同样不能改名</b>。
        </template>
        <template v-else>
          它会变成网络上的 <code>\\服务器\名字</code>，所以<b>建好之后不能改名</b>：客户端记的就是这个名字。
        </template>
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
        相对卷的挂载点。留空就是卷根——单卷单共享时最常见的用法。
        <template v-if="isNFS">
          目录不存在会创建，并按只读/读写设成 <code>0755</code> / <code>0777</code>。
        </template>
        <template v-else>目录不存在会创建（0700）。</template>
      </p>
    </div>

    <div class="form__field">
      <span class="form__label">只读</span>
      <NSwitch v-model:value="form.read_only" data-testid="share-read-only" />
      <p v-if="isNFS" class="form__muted">
        打开时导出行写 <code>ro</code>、目录权限落到 <code>0755</code>——两道保证都在，
        客户端连写都写不进去，不必去猜 ro 有没有生效。
        <template v-if="!form.read_only">
          关掉时导出行写 <code>rw</code>、目录是 <code>0777</code>：NFS 走 sec=sys，
          客户端以<b>自己的 uid</b> 落盘，服务端事先不知道是谁连上来，所以读写权限只能
          交给网段管。
        </template>
      </p>
      <p v-else class="form__muted">
        打开时所有账号的 ACL 上限都是 <code>r-x</code>，共享段也写 <code>read only = yes</code>。
        它比账号级授权更宽：任一处只读就是只读。
      </p>
    </div>

    <div v-if="!isNFS" class="form__field">
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

    <div v-if="isNFS" class="form__field">
      <span class="form__label">导出选项</span>
      <div class="form__tags">
        <NTag size="small" type="success">root_squash</NTag>
        <NTag size="small" type="success">sync</NTag>
        <NTag size="small">no_subtree_check</NTag>
      </div>
      <p class="form__muted">
        这几项是固定的，面板不提供开关：没有 <code>no_root_squash</code>、没有
        <code>async</code>、没有 <code>insecure</code>——它们根本不在取值里（PLAN §10）。
        客户端 root 会被 squash 成 <code>nobody</code>。
      </p>
    </div>

    <div class="form__field">
      <label class="form__label" for="share-cidrs">来源网段</label>
      <NInput
        id="share-cidrs"
        v-model:value="form.allowed_cidrs"
        type="textarea"
        :autosize="{ minRows: 2, maxRows: 4 }"
        :placeholder="isNFS ? '必须填，例如 10.0.2.0/24' : '留空不限制，例如 192.168.1.0/24 10.0.0.0/8'"
        data-testid="share-cidrs"
      />
      <p v-if="isNFS" class="form__muted">
        逗号或空格分隔。<b>NFS 至少要有一条</b>，而且不能用 <code>0.0.0.0/0</code>：
        网段是 NFS 唯一的访问控制，不点名等于向全世界导出。
      </p>
      <p v-else class="form__muted">
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
      <NTag size="small">protocol: {{ form.protocol }}</NTag>
      <NTag v-if="isNFS" size="small">{{ cidrs.length }} 个网段</NTag>
      <NTag v-else size="small">{{ smbUsers.length }} 个账号</NTag>
      <NTag v-if="cidrs.length === 0 && !isNFS" size="small" type="warning">不限来源</NTag>
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
  font-size: var(--fs-md);
}

.form__muted {
  margin: 0;
  font-size: var(--fs-md);
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
  font-size: var(--fs-md);
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
