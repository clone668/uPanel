<script setup lang="ts">
// 账号页（P14.5）：受管理 Linux 账号、SSH 公钥与受限 sudo 模板。
//
// 这一页有**两种账号**，它们在同一张接口里出来，但写走两条动作：
//   - `regular`：人能登录的普通账号。建/改/删、加撤公钥、发撤 sudo 全在这一页；
//   - `share`：只给 Samba 认证用的系统账号。它只能禁/启用，密码与共享绑在一起，
//     所以这一页对它是只读的，写按钮指向共享页（`share.account.apply`）。
// 把两者的按钮合并，就是把两条不同的读回核验混成一个。
//
// 页面上每个写操作都只有一条路：预检 → 任务。判决（名字被占、uid 越界、home 是符号
// 链接、共享还在引用、sudoers 文件不属于面板）全部由代理现场算，这一页只负责把
// 阻塞项原样摊开——一个灰掉的按钮后面没有原因，人只会以为面板坏了。
//
// 三处必须分开显示的事实：
//   `shell`          此刻系统上的登录壳（禁用后是 nologin）
//   `shell_profile`  期望的那一档（启用之后回到哪）
//   `has_password`   设过没有（没设过就只能靠公钥登录——没有口令不等于登不上，Q479）
// 只显示其中一个，人就会得出错误结论：把 nologin 读成“这个账号本来就这样”，
// 或者把“有 home”读成“能登录”。

import { computed, onMounted, reactive, ref } from 'vue'
import {
  NAlert,
  NButton,
  NCard,
  NCheckbox,
  NEmpty,
  NForm,
  NFormItem,
  NInput,
  NRadio,
  NRadioGroup,
  NSelect,
  NTag,
} from 'naive-ui'

import { ApiError } from '@/api/client'
import {
  ACCOUNT_ACTIONS,
  accountApplyParams,
  accountDeleteParams,
  accountDisplayLabel,
  accountNameProblem,
  accountShellProfile,
  accountNoticeLabel,
  accountsAvailable,
  buildAccountDefinition,
  createAccountSecret,
  editableAsRegular,
  getAccountKeys,
  getAccounts,
  sshKeyApplyParams,
  sshKeyDeleteParams,
  sudoRuleApplyParams,
  sudoRuleDeleteParams,
  summarizeAccountState,
  SUDO_TEMPLATES,
  SHELL_PROFILE_LABELS,
  ACCOUNT_TYPE_LABELS,
  type AccountCapability,
  type AccountForm,
  type ManagedAccount,
  type ManagedAccountSSHKey,
} from '@/api/accounts'
import { reauthPassword } from '@/api/auth'
import ApiErrorPanel from '@/components/ApiErrorPanel.vue'
import SecondFactorField from '@/components/SecondFactorField.vue'
import StatePanel from '@/components/StatePanel.vue'
import StorageActionDialog from '@/components/StorageActionDialog.vue'
import { useSessionStore } from '@/stores/session'
import { summarizeAccountJob } from '@/utils/jobresult'
import { formatTimestamp } from '@/utils/format'
import { apiPanelState, retryablePanelState } from '@/utils/viewstate'

type Phase = 'loading' | 'ready' | 'error'

const session = useSessionStore()

const phase = ref<Phase>('loading')
const error = ref<ApiError | null>(null)
const capability = ref<AccountCapability | null>(null)
const accounts = ref<ManagedAccount[]>([])

const panelState = computed(() => (error.value ? apiPanelState(error.value) : null))
const available = computed(() => accountsAvailable(capability.value))

// ---------------------------------------------------------------- 公钥

const selectedId = ref<string>('')
const selected = computed(() => accounts.value.find((a) => a.id === selectedId.value) ?? null)
const keys = ref<ManagedAccountSSHKey[]>([])
const keysLoading = ref(false)
const keysError = ref<string | null>(null)

async function loadKeys(account: ManagedAccount): Promise<void> {
  selectedId.value = account.id
  keys.value = []
  keysError.value = null
  if (!editableAsRegular(account)) {
    // 共享账号没有 authorized_keys 可管：它的“钥匙”是 Samba 口令，不在这里。
    return
  }
  keysLoading.value = true
  try {
    const res = await getAccountKeys(account.id)
    keys.value = res.data.ssh_keys
  } catch (cause) {
    // 一把公钥都没有不是错误；读不到才是。这两种都别渲染成“没有钥匙”。
    keysError.value = cause instanceof ApiError ? cause.message : '读取公钥列表失败'
  } finally {
    keysLoading.value = false
  }
}

// ---------------------------------------------------------------- 新建表单

const accountForm = reactive<AccountForm & { password: string; confirm: string }>({
  name: '',
  display_name: '',
  shell_profile: 'restricted',
  disabled: false,
  password_secret_ref: '',
  password: '',
  confirm: '',
})

/**
 * 表单自己的校验：只挡“明显不该提交”的输入，不假装是安全边界。
 *
 * 名字形状的判据在 `accountNameProblem`（与代理的 `ValidateName` 同一套）；12 个
 * 字符的口令下限跟着面板自己的登录口令口径走（`internal/auth` 的同一档），不是 Linux
 * 的规矩：`chpasswd` 不查长度，代理也不查。真正的判据在代理侧（保留名、uid 是不是
 * 落在 1000–59999、home 是不是符号链接），这里不重复实现。
 */
const formErrors = computed<string[]>(() => {
  const out: string[] = []
  const problem = accountNameProblem(accountForm.name.trim())
  if (problem !== null) out.push(problem)
  if (accountForm.password !== '' && accountForm.password !== accountForm.confirm) {
    out.push('两次输入的口令不一致')
  }
  if (accountForm.password !== '' && accountForm.password.length < 12) {
    out.push('口令至少 12 个字符')
  }
  return out
})

const canCreate = computed(() => available.value && formErrors.value.length === 0)

function resetForm(): void {
  accountForm.name = ''
  accountForm.display_name = ''
  accountForm.shell_profile = 'restricted'
  accountForm.disabled = false
  accountForm.password_secret_ref = ''
  accountForm.password = ''
  accountForm.confirm = ''
  formError.value = null
}

// ---------------------------------------------------------------- 动作

interface PendingAction {
  action: string
  title: string
  params: Record<string, unknown>
  lines: string[]
  needsReauth: boolean
}

const pending = ref<PendingAction | null>(null)
const dialogShow = ref(false)
const busy = ref(false)
const formError = ref<string | null>(null)

// 秘密通道的再认证（POST /secrets 有自己的 5 分钟窗口）。面板密码只在这个输入框里
// 活到一次 /auth/reauth 返回为止；它与被创建的那个账号的口令是两份东西。
const reauthNeeded = ref(false)
const reauthInput = ref('')
// 启用二次验证的账号要多带一个六位数：服务端的两步入口对再认证同样生效。
const reauthCode = ref('')
const reauthBusy = ref(false)

const keyInput = ref('')
const sudoTemplateId = ref<string>(SUDO_TEMPLATES[0]?.id ?? '')

const sudoOptions = SUDO_TEMPLATES.map((t) => ({
  label: `${t.display_name}（${t.id}）`,
  value: t.id,
}))

function openAction(action: PendingAction): void {
  pending.value = action
  dialogShow.value = true
}

function onFinished(): void {
  pending.value = null
  void load()
}

/**
 * 提交口令并拿回引用。
 *
 * 明文只活到 `createAccountSecret` 返回为止：之后表单里立刻清空，任务参数里只有
 * `sec_` 引用。这一步与“提交任务”是两件事，所以它有自己的错误出口——口令不合格时
 * 不该让人以为账号已经建了一半。
 */
async function submitSecret(password: string, reference: string): Promise<string> {
  const res = await createAccountSecret(password, reference)
  return res.data.secret_ref
}

async function createAccount(): Promise<void> {
  if (!canCreate.value || busy.value) return
  busy.value = true
  formError.value = null
  try {
    let secretRef = ''
    if (accountForm.password !== '') {
      secretRef = await submitSecret(accountForm.password, `account:${accountForm.name.trim()}`)
      accountForm.password = ''
      accountForm.confirm = ''
    }
    const definition = buildAccountDefinition({ ...accountForm, password_secret_ref: secretRef })
    const lines = [
      `新建普通账号 ${definition.name}（uid 由系统分配，必须落在 1000–59999）`,
      `登录壳：${SHELL_PROFILE_LABELS[definition.shell_profile]}`,
      definition.disabled ? '建好之后立刻是禁用状态（nologin）' : '建好之后可以登录',
      secretRef === '' ? '这一次不设口令：这个账号之后只能靠已配的公钥登录' : '口令经标准输入交给 chpasswd，不进命令行、不进日志',
    ]
    resetForm()
    openAction({
      action: ACCOUNT_ACTIONS.apply,
      title: `新建账号「${definition.name}」`,
      params: accountApplyParams(definition),
      lines,
      needsReauth: true,
    })
  } catch (cause) {
    if (cause instanceof ApiError && cause.code === 'REAUTH_REQUIRED') {
      // 秘密通道自己也要 5 分钟内的再认证（POST /secrets 带 requireReauth）。
      // 这一步在确认框之前，所以不能指望确认框里的密码框——把它摊到表单这里，
      // 账号口令不用重输：它还在表单里，成功之后才清。
      reauthNeeded.value = true
      formError.value = '秘密通道需要 5 分钟内的密码再认证：在下面输入面板密码后继续'
      return
    }
    formError.value = cause instanceof ApiError ? cause.message : '提交口令失败'
  } finally {
    busy.value = false
  }
}

/** 完成再认证后接着把刚才被打断的创建做完，人不用重新填一遍表单。 */
async function finishReauth(): Promise<void> {
  if (reauthInput.value === '' || reauthBusy.value) return
  reauthBusy.value = true
  formError.value = null
  try {
    // 面板密码只发给 /auth/reauth，不参与任务参数，也不碰账号口令那一份。
    await reauthPassword(reauthInput.value, reauthCode.value === '' ? undefined : reauthCode.value)
    reauthInput.value = ''
    reauthCode.value = ''
    await session.restore()
    reauthNeeded.value = false
    await createAccount()
  } catch (cause) {
    formError.value = cause instanceof ApiError ? cause.message : '再认证失败'
  } finally {
    reauthBusy.value = false
  }
}

function definitionOf(account: ManagedAccount, overrides: Partial<AccountForm> = {}) {
  return buildAccountDefinition({
    name: account.name,
    display_name: account.display_name,
    shell_profile: accountShellProfile(account),
    disabled: account.disabled,
    password_secret_ref: '',
    ...overrides,
  })
}

function toggleDisabled(account: ManagedAccount): void {
  const next = !account.disabled
  openAction({
    action: ACCOUNT_ACTIONS.apply,
    title: `${next ? '禁用' : '启用'}账号「${account.name}」`,
    params: accountApplyParams(definitionOf(account, { disabled: next }), {
      id: account.id,
      version: account.version,
    }),
    lines: next
      ? [
          `把 ${account.name} 的登录壳改成 /usr/sbin/nologin：这个账号立刻登不上`,
          '不用 usermod -L：-U 对从未设过口令的账号会写一个空口令，那比 nologin 危险',
          `期望档（${accountShellProfile(account)}）会记着，启用时回到那一档`,
        ]
      : [
          `把 ${account.name} 的登录壳改回 ${accountShellProfile(account)} 对应的一档`,
          account.has_password ? '这个账号有口令，启用之后就能登录' : '这个账号没有口令：启用之后能用已配的公钥登录；要口令登录，先去设一个',
        ],
    needsReauth: true,
  })
}

function deleteAccount(account: ManagedAccount): void {
  openAction({
    action: ACCOUNT_ACTIONS.delete,
    title: `删除账号「${account.name}」`,
    params: accountDeleteParams(account),
    lines: [
      `删除 ${account.name} 的系统账号与面板记录，它的 sudo 规则一并撤销`,
      `home 目录 ${account.home} 不会被删（userdel 不带 -r）：里面的文件留着`,
      '如果还有共享在引用这个账号，预检会拒绝',
    ],
    needsReauth: true,
  })
}

function addKey(account: ManagedAccount): void {
  const publicKey = keyInput.value.trim()
  if (publicKey === '') {
    formError.value = '公钥内容不能为空'
    return
  }
  openAction({
    action: ACCOUNT_ACTIONS.keyApply,
    title: `给「${account.name}」加一把公钥`,
    params: sshKeyApplyParams(account, publicKey),
    lines: [
      `只重写 ${account.home}/.ssh/authorized_keys 里的**托管区**，托管区外的手写行逐字节不动`,
      '写完整份重读：托管区里的指纹必须等于登记的集合，文件权限 0600、目录 0700',
      '公钥不是秘密，它会原样显示在列表里；私钥永远不进面板',
    ],
    needsReauth: true,
  })
  keyInput.value = ''
}

function removeKey(account: ManagedAccount, key: ManagedAccountSSHKey): void {
  openAction({
    action: ACCOUNT_ACTIONS.keyDelete,
    title: `撤掉「${account.name}」的公钥 ${key.fingerprint}`,
    params: sshKeyDeleteParams(account, key),
    lines: [`从托管区移除 ${key.fingerprint}（${key.type}${key.comment ? '，' + key.comment : ''}）`, '托管区外的行不动'],
    needsReauth: true,
  })
}

function grantSudo(account: ManagedAccount): void {
  const template = SUDO_TEMPLATES.find((t) => t.id === sudoTemplateId.value)
  if (!template) {
    formError.value = '先选一个 sudo 模板'
    return
  }
  openAction({
    action: ACCOUNT_ACTIONS.sudoApply,
    title: `给「${account.name}」发 sudo（${template.display_name}）`,
    params: sudoRuleApplyParams(account, template.id),
    lines: [
      `写入 /etc/sudoers.d/upanel-${account.name}，只含这几条命令：`,
      ...template.commands.map((c) => `  ${c}`),
      '候选文件先过 visudo -c -f，通过才替换；替换后整份 visudo -c、再 sudo -l -U 逐条读回',
      '这条通道只有只读命令：启停服务、改系统状态都不在这里',
    ],
    needsReauth: true,
  })
}

function revokeSudo(account: ManagedAccount): void {
  openAction({
    action: ACCOUNT_ACTIONS.sudoDelete,
    title: `撤销「${account.name}」的 sudo`,
    params: sudoRuleDeleteParams(account),
    lines: [`删除 /etc/sudoers.d/upanel-${account.name}`, '删完整份 visudo -c 再读回，确认这个账号没有任何 sudo 授权'],
    needsReauth: true,
  })
}

// ---------------------------------------------------------------- 载入

async function load(): Promise<void> {
  phase.value = 'loading'
  error.value = null
  try {
    const res = await getAccounts()
    capability.value = res.data.capability
    accounts.value = res.data.accounts
    phase.value = 'ready'
    // 刷新之后原来的选中项可能已经不存在（删掉了），别对着一个空记录渲染公钥区。
    const still = accounts.value.find((a) => a.id === selectedId.value)
    if (still) void loadKeys(still)
    else {
      selectedId.value = ''
      keys.value = []
    }
  } catch (cause) {
    error.value = cause instanceof ApiError ? cause : null
    phase.value = 'error'
  }
}

onMounted(() => {
  void load()
})
</script>

<template>
  <section class="accounts">
    <div class="accounts__head">
      <h2 class="accounts__title">账号</h2>
      <NTag :type="available ? 'success' : 'warning'" size="small">
        {{ available ? '账号管理可用' : '账号管理不可用' }}
      </NTag>
      <NButton size="small" @click="load">刷新</NButton>
    </div>

    <StatePanel v-if="phase === 'loading'" state="loading" title="正在读取账号列表" />
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

    <template v-else>
      <NAlert v-if="!available" type="warning" :bordered="false">
        这台机器不支持账号管理：{{ capability?.reason ?? '代理没给出原因' }}
      </NAlert>

      <NAlert
        v-if="(capability?.notices ?? []).length > 0"
        type="info"
        :bordered="false"
        title="能力报告"
      >
        <ul class="accounts__notices">
          <li v-for="notice in capability?.notices ?? []" :key="notice.code">
            <strong>{{ accountNoticeLabel(notice.code) }}</strong>：{{ notice.message }}
          </li>
        </ul>
      </NAlert>

      <NCard size="small" title="受管理账号">
        <NEmpty v-if="accounts.length === 0" description="还没有账号。用下面那张卡建一个。" />
        <div v-else class="accounts__list">
          <div v-for="account in accounts" :key="account.id" class="accounts__row">
            <div class="accounts__row-head">
              <strong>{{ accountDisplayLabel(account) }}</strong>
              <span class="accounts__meta">{{ account.name }}</span>
              <NTag size="small" :type="account.type === 'regular' ? 'info' : 'default'">
                {{ ACCOUNT_TYPE_LABELS[account.type] ?? account.type }}
              </NTag>
              <NTag size="small" :type="summarizeAccountState(account).tone">
                {{ summarizeAccountState(account).label }}
              </NTag>
              <NTag v-if="!account.has_password" size="small" type="warning">没有口令</NTag>
            </div>
            <p class="accounts__meta">{{ summarizeAccountState(account).detail }}</p>
            <p class="accounts__meta">
              uid {{ account.uid }} · gid {{ account.gid }} · 此刻的壳
              <code>{{ account.shell }}</code> · 期望档
              {{ SHELL_PROFILE_LABELS[accountShellProfile(account)] }} · home
              <code>{{ account.home }}</code> · 更新于 {{ formatTimestamp(account.updated_at) }}
            </p>
            <div class="accounts__actions">
              <template v-if="editableAsRegular(account)">
                <NButton size="tiny" @click="loadKeys(account)">公钥</NButton>
                <NButton size="tiny" @click="toggleDisabled(account)">
                  {{ account.disabled ? '启用' : '禁用' }}
                </NButton>
                <NButton size="tiny" @click="grantSudo(account)">发 sudo</NButton>
                <NButton size="tiny" @click="revokeSudo(account)">撤 sudo</NButton>
                <NButton size="tiny" type="error" @click="deleteAccount(account)">删除</NButton>
              </template>
              <p v-else class="accounts__meta">
                共享账号的密码与共享绑在一起，改动在共享页（<code>share.account.apply</code>）。
              </p>
            </div>
          </div>
        </div>
      </NCard>

      <NCard
        v-if="selected && editableAsRegular(selected)"
        size="small"
        :title="`「${accountDisplayLabel(selected)}」的 SSH 公钥`"
      >
        <p class="accounts__meta">
          这里列的是<strong>面板登记的</strong>公钥（<code>managed_ssh_keys</code>），不是此刻
          <code>{{ selected.home }}/.ssh/authorized_keys</code> 的内容。两者不同时，
          以执行器的读回为准——每次写入都会整份重读比对。
        </p>
        <NAlert v-if="keysError" type="warning" :bordered="false">
          {{ keysError }}<br />
          读不到这张列表不影响加公钥：加的时候执行器会整份重读并比对，不以这张列表为准。
        </NAlert>
        <StatePanel v-else-if="keysLoading" state="loading" title="正在读取公钥" />
        <NEmpty v-else-if="keys.length === 0" description="这个账号还没有配公钥" />
        <ul v-else class="accounts__keys">
          <li v-for="key in keys" :key="key.id" class="accounts__key">
            <code>{{ key.fingerprint }}</code>
            <span class="accounts__meta">
              {{ key.type }}<template v-if="key.comment"> · {{ key.comment }}</template> · 加于
              {{ formatTimestamp(key.created_at) }}
            </span>
            <NButton size="tiny" type="error" @click="removeKey(selected, key)">撤掉</NButton>
          </li>
        </ul>
        <NForm label-placement="top">
          <NFormItem label="加一把公钥（整行 ssh-ed25519 / ssh-rsa …）">
            <NInput
              v-model:value="keyInput"
              type="textarea"
              :rows="3"
              placeholder="ssh-ed25519 AAAAC3Nza… alice@laptop"
            />
          </NFormItem>
        </NForm>
        <NButton size="small" :disabled="!available" @click="addKey(selected)">加入</NButton>
      </NCard>

      <NCard size="small" title="新建普通账号">
        <NForm label-placement="left" :label-width="90">
          <NFormItem label="账号名">
            <NInput v-model:value="accountForm.name" placeholder="alice" />
          </NFormItem>
          <NFormItem label="展示名">
            <NInput v-model:value="accountForm.display_name" placeholder="可选，只是标签" />
          </NFormItem>
          <NFormItem label="登录壳">
            <NRadioGroup v-model:value="accountForm.shell_profile">
              <NRadio value="restricted">受限登录（rbash）</NRadio>
              <NRadio value="disabled">禁用登录（nologin）</NRadio>
            </NRadioGroup>
          </NFormItem>
          <NFormItem label="建好即禁用">
            <NCheckbox v-model:checked="accountForm.disabled" />
          </NFormItem>
          <NFormItem label="口令">
            <NInput
              v-model:value="accountForm.password"
              type="password"
              show-password-on="click"
              placeholder="可留空；留空就建一个没有口令的账号"
            />
          </NFormItem>
          <NFormItem label="再输一次">
            <NInput
              v-model:value="accountForm.confirm"
              type="password"
              show-password-on="click"
              placeholder="确认口令"
            />
          </NFormItem>
        </NForm>
        <NAlert v-if="formErrors.length > 0" type="warning" :bordered="false">
          <ul class="accounts__notices">
            <li v-for="item in formErrors" :key="item">{{ item }}</li>
          </ul>
        </NAlert>
        <NAlert v-if="formError" type="error" :bordered="false">{{ formError }}</NAlert>
        <NForm v-if="reauthNeeded" label-placement="left" :label-width="90">
          <NFormItem label="面板密码">
            <NInput
              v-model:value="reauthInput"
              type="password"
              show-password-on="click"
              placeholder="当前登录账号的密码，只发给 /auth/reauth"
              @keyup.enter="finishReauth"
            />
          </NFormItem>
          <SecondFactorField
            :show="session.totpEnabled"
            :model-value="reauthCode"
            :disabled="reauthBusy"
            testid="accounts-reauth-code"
            @update:model-value="(value: string) => (reauthCode = value)"
            @keyup.enter="finishReauth"
          />
        </NForm>
        <NButton v-if="reauthNeeded" size="small" :disabled="reauthBusy" @click="finishReauth">
          验证并继续
        </NButton>
        <p class="accounts__meta">
          没有 uid、没有 gid、没有 groups、没有 home 可以填：这四项由系统分配，面板只读回、
          只展示、只据此对账。能提交它们就等于能提交 <code>uid=0</code>。
        </p>
        <NButton type="primary" size="small" :disabled="!canCreate || busy" @click="createAccount">
          新建
        </NButton>
      </NCard>

      <NCard size="small" title="sudo 模板">
        <p class="accounts__meta">
          只有下面这两个模板，都是只读。模板里没有通配符，也不接受单元名参数：sudoers 的
          <code>*</code> 按单参数 glob 匹配，<code>journalctl -u *</code> 会连
          <code>--vacuum-size</code> 一起放行。
        </p>
        <ul class="accounts__notices">
          <li v-for="template in SUDO_TEMPLATES" :key="template.id">
            <strong>{{ template.display_name }}</strong>（<code>{{ template.id }}</code>）：
            {{ template.purpose }}
            <ul>
              <li v-for="command in template.commands" :key="command">
                <code>{{ command }}</code>
              </li>
            </ul>
          </li>
        </ul>
        <NFormItem label="发 sudo 时用">
          <NSelect v-model:value="sudoTemplateId" :options="sudoOptions" />
        </NFormItem>
      </NCard>
    </template>

    <StorageActionDialog
      v-if="pending"
      v-model:show="dialogShow"
      :action="pending.action"
      :title="pending.title"
      :params="pending.params"
      :confirm-lines="pending.lines"
      :needs-reauth="pending.needsReauth"
      :summarizer="summarizeAccountJob"
      @finished="onFinished"
    />
  </section>
</template>

<style scoped>

.accounts__list {
  display: flex;
  flex-direction: column;
  gap: 14px;
  margin-top: 8px;
}

.accounts__row {
  display: flex;
  flex-direction: column;
  gap: 4px;
  padding-bottom: 10px;
  border-bottom: 1px solid var(--rule);
}

.accounts__row-head {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 8px;
}

.accounts__actions {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 8px;
  margin-top: 4px;
}

.accounts__keys {
  margin: 0;
  padding: 0;
  list-style: none;
  display: flex;
  flex-direction: column;
  gap: 6px;
}

.accounts__key {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 8px;
  font-size: var(--fs-md);
}

.accounts__notices {
  margin: 4px 0 0;
  padding-left: 18px;
  font-size: var(--fs-md);
  line-height: 1.7;
}
</style>
