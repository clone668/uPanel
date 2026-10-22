<script setup lang="ts">
// 受管理共享账号（P08.4）。
//
// 一个账号在这里其实是**两样东西**：一个 POSIX 用户（useradd --system、nologin、无 home）
// 和一条 SMB 密码记录（passdb）。两样都在代理那边成对建、成对删，所以界面上没有
// “只改其中一样”的按钮——那种按钮会做出一个能登录系统但不能连共享、或者反过来的人。
//
// 密码的路径是固定的，也是这个组件最重要的约束：
//   明文 → POST /secrets（purpose=smb_password）→ secret_ref → share.account.apply
// 明文一次都不进任务参数、不进 URL、不进日志；它在这个组件里活到 createSecret 返回为止，
// 然后立刻清空。任务参数里只有 sec_ 开头的引用。

import { computed, reactive, ref } from 'vue'
import { NAlert, NButton, NCard, NInput, NTag } from 'naive-ui'

import { createSecret, type ShareAccount } from '@/api/shares'
import { ApiError } from '@/api/client'
import StorageActionDialog from './StorageActionDialog.vue'
import { summarizeShareJob } from '@/utils/jobresult'
import { formatTimestamp } from '@/utils/format'

const props = defineProps<{
  accounts: readonly ShareAccount[]
  /** 已有共享引用的账号不能删：服务端会拒绝，这里先说清楚为什么按钮是灰的。 */
  referencedIds: readonly string[]
  canAct: boolean
}>()

const emit = defineEmits<{ changed: [] }>()

interface PendingAccount {
  action: string
  title: string
  params: Record<string, unknown>
  lines: string[]
}

const pending = ref<PendingAccount | null>(null)
const dialogShow = ref(false)

const creating = ref(false)
const secretBusy = ref(false)
const formError = ref<string | null>(null)

const form = reactive({
  /** 空串 = 新建；非空 = 给这个账号换密码。 */
  targetId: '',
  name: '',
  password: '',
  confirm: '',
})

const resetting = computed(() => form.targetId !== '')
const target = computed(() => props.accounts.find((a) => a.id === form.targetId) ?? null)

const referenced = computed(() => new Set(props.referencedIds))

const formErrors = computed<string[]>(() => {
  const out: string[] = []
  const name = form.name.trim()
  if (!resetting.value) {
    if (name === '') out.push('账号名不能为空')
    else if (name.length > 32) out.push('账号名过长（上限 32 字节）')
    else if (!/^[a-z_][a-z0-9_-]*$/i.test(name)) out.push('账号名要用字母、数字、下划线、连字符，且不能以数字或连字符开头')
    else if (name.endsWith('-')) out.push('账号名不能以连字符结尾')
  }
  if (form.password === '') out.push('密码不能为空')
  else if (form.password !== form.confirm) out.push('两次输入的密码不一致')
  return out
})

const canWrite = computed(() => props.canAct && formErrors.value.length === 0 && !secretBusy.value)

function startCreate(): void {
  creating.value = true
  form.targetId = ''
  form.name = ''
  form.password = ''
  form.confirm = ''
  formError.value = null
}

function startReset(account: ShareAccount): void {
  creating.value = true
  form.targetId = account.id
  form.name = account.name
  form.password = ''
  form.confirm = ''
  formError.value = null
}

function cancelForm(): void {
  creating.value = false
  form.targetId = ''
  form.name = ''
  form.password = ''
  form.confirm = ''
  formError.value = null
}

function clearPassword(): void {
  form.password = ''
  form.confirm = ''
}

async function submit(): Promise<void> {
  if (!canWrite.value) return
  secretBusy.value = true
  formError.value = null
  const name = resetting.value ? (target.value?.name ?? form.name.trim()) : form.name.trim()
  try {
    // 第一步：把明文交给秘密通道，拿回一个引用。明文到此为止。
    const secret = await createSecret(form.password)
    clearPassword()
    const ref = secret.data.secret_ref
    if (resetting.value) {
      pending.value = {
        action: 'share.account.apply',
        title: `给账号「${name}」换 SMB 密码`,
        params: {
          account_id: form.targetId,
          name,
          secret_ref: ref,
          disabled: target.value?.disabled === true,
        },
        lines: [
          `账号：${name}`,
          '会先 smbpasswd -x 摘掉旧密码，再用新密码重建一条',
          '已经用旧密码连着的会话不会立刻断开，但它们下次认证会用新密码',
          '密码明文已经交给秘密通道，任务参数里只有引用',
        ],
      }
    } else {
      pending.value = {
        action: 'share.account.apply',
        title: `新建共享账号「${name}」`,
        params: { account_id: null, name, secret_ref: ref, disabled: false },
        lines: [
          `账号：${name}`,
          '会建一个系统用户（--system、nologin、无 home）：它只能用来连共享，不能登录这台机器',
          '同时在 Samba 的 passdb 里建一条同名记录并设密码',
          '建完会用 getent passwd、passwd -S、pdbedit -L -v 读回核验',
          '密码明文已经交给秘密通道，任务参数里只有引用',
        ],
      }
    }
    creating.value = false
    dialogShow.value = true
  } catch (caught) {
    formError.value =
      caught instanceof ApiError ? `密码没有存进秘密存储：${caught.message}` : '密码没有存进秘密存储，请重试'
  } finally {
    secretBusy.value = false
  }
}

function removeAccount(account: ShareAccount): void {
  pending.value = {
    action: 'share.account.delete',
    title: `删除共享账号「${account.name}」`,
    params: { account_id: account.id },
    lines: [
      `账号：${account.name}`,
      '会先 smbpasswd -x 摘掉 SMB 记录，再 userdel 删系统用户',
      '这个账号在所有共享里的授权会一起消失（它会从那些共享的名单里被摘掉）',
      '账号名下已经产生的文件不会删除，它们的属主 UID 会变成一个没有名字的数字',
    ],
  }
  dialogShow.value = true
}

function toggleDisabled(account: ShareAccount): void {
  const next = !account.disabled
  pending.value = {
    action: 'share.account.apply',
    title: `${next ? '停用' : '启用'}账号「${account.name}」`,
    params: { account_id: account.id, name: account.name, secret_ref: '', disabled: next },
    lines: next
      ? [`账号：${account.name}`, '会在 passdb 里把这个账号标成停用；共享名单不变', '密码不动，重新启用时不需要重设']
      : [`账号：${account.name}`, '会重新启用 passdb 里的这条记录', '密码不动'],
  }
  dialogShow.value = true
}

function onFinished(): void {
  emit('changed')
}
</script>

<template>
  <NCard size="small" title="受管理账号">
    <p class="acct__muted">
      共享账号不会登录这台机器（<code>nologin</code>、无 home），只用来连共享。
      密码不落面板数据库、不进日志、不进命令行：它只经秘密通道交给代理，最后由
      <code>smbpasswd</code> 从标准输入读走。
    </p>

    <div class="acct__toolbar">
      <NButton v-if="canAct && !creating" size="small" type="primary" data-testid="account-new" @click="startCreate">
        新建账号
      </NButton>
      <NButton v-if="creating" size="small" secondary @click="cancelForm">取消</NButton>
    </div>

    <div v-if="creating" class="acct__form">
      <p class="acct__label">{{ resetting ? `给 ${form.name} 换密码` : '新账号' }}</p>
      <NInput
        v-if="!resetting"
        v-model:value="form.name"
        placeholder="账号名，例如 teamdocs"
        data-testid="account-name"
      />
      <NInput
        v-model:value="form.password"
        type="password"
        show-password-on="click"
        placeholder="SMB 密码"
        data-testid="account-password"
      />
      <NInput
        v-model:value="form.confirm"
        type="password"
        show-password-on="click"
        placeholder="再输一次"
        data-testid="account-password-confirm"
      />
      <p class="acct__muted">
        密码强度由 Samba 的 <code>min password length</code> 与客户端的策略决定；面板不替它定规则，
        也不把密码写进任何日志或任务参数。取消掉这次确认的话，这条秘密会在 10 分钟后自行销毁。
      </p>
      <div v-if="creating && formErrors.length > 0 && (form.password !== '' || form.name !== '')" class="acct__errors">
        <p v-for="err in formErrors" :key="err">{{ err }}</p>
      </div>
      <NAlert v-if="formError" type="error" :title="formError" />
      <div class="acct__actions">
        <NButton type="primary" :disabled="!canWrite" data-testid="account-submit" @click="submit">
          {{ secretBusy ? '正在交给秘密通道…' : resetting ? '换密码' : '建账号' }}
        </NButton>
      </div>
    </div>

    <p v-if="props.accounts.length === 0" class="acct__muted">还没有受管理账号。</p>

    <ul v-else class="acct__list">
      <li v-for="account in props.accounts" :key="account.id" class="acct__row" :data-account="account.name">
        <div class="acct__row-main">
          <span class="acct__name">{{ account.name }}</span>
          <NTag size="small" :type="account.disabled ? 'warning' : 'success'">
            {{ account.disabled ? '已停用' : '可用' }}
          </NTag>
          <NTag v-if="account.has_password" size="small">已设 SMB 密码</NTag>
          <NTag v-else size="small" type="warning">没有 SMB 密码</NTag>
          <NTag v-if="referenced.has(account.id)" size="small">被共享引用</NTag>
        </div>
        <p class="acct__meta">
          uid {{ account.uid }} · gid {{ account.gid }} · shell <code>{{ account.shell || '—' }}</code> ·
          更新于 {{ formatTimestamp(account.updated_at) }}
        </p>
        <div v-if="canAct" class="acct__row-actions">
          <NButton size="tiny" secondary @click="startReset(account)">换密码</NButton>
          <NButton size="tiny" secondary @click="toggleDisabled(account)">
            {{ account.disabled ? '启用' : '停用' }}
          </NButton>
          <NButton
            size="tiny"
            secondary
            type="error"
            :disabled="referenced.has(account.id)"
            :title="referenced.has(account.id) ? '还有共享引用这个账号，先从共享名单里摘掉' : undefined"
            @click="removeAccount(account)"
          >
            删除
          </NButton>
        </div>
      </li>
    </ul>

    <StorageActionDialog
      v-if="pending"
      v-model:show="dialogShow"
      :action="pending.action"
      :title="pending.title"
      :params="pending.params"
      :confirm-lines="pending.lines"
      :needs-reauth="true"
      :summarizer="summarizeShareJob"
      @finished="onFinished"
    />
  </NCard>
</template>

<style scoped>
.acct__muted {
  margin: 0;
  font-size: 13px;
  line-height: 1.7;
  opacity: 0.8;
}

.acct__toolbar {
  display: flex;
  gap: 8px;
  margin: 10px 0;
}

.acct__form {
  display: flex;
  flex-direction: column;
  gap: 8px;
  padding: 10px;
  margin-bottom: 12px;
  border: 1px solid rgba(128, 128, 128, 0.25);
  border-radius: 6px;
}

.acct__label {
  margin: 0;
  font-weight: 600;
  font-size: 13px;
}

.acct__errors {
  font-size: 13px;
  line-height: 1.6;
  color: #d03050;
}

.acct__errors p {
  margin: 0;
}

.acct__actions {
  display: flex;
  justify-content: flex-end;
}

.acct__list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 10px;
}

.acct__row {
  padding: 8px 0;
  border-bottom: 1px solid rgba(128, 128, 128, 0.2);
}

.acct__row-main {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}

.acct__name {
  font-weight: 600;
}

.acct__meta {
  margin: 4px 0 0;
  font-size: 13px;
  opacity: 0.8;
  word-break: break-all;
}

.acct__row-actions {
  display: flex;
  gap: 6px;
  margin-top: 6px;
}
</style>
