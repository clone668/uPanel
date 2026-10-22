<script setup lang="ts">
// “客户端怎么连”的说明（P08.4）。
//
// 三条底线在这一块是可见的：
//   1. 命令里没有密码。Linux 那条挂载命令用 `credentials=` 指向 0600 文件，
//      因为 `password=` 会留在 `ps` 里——和“秘密不进 argv”是同一条规矩。
//   2. 地址里的主机名取自**人此刻正在用的地址**（window.location.hostname，由调用方传），
//      不从网卡表里猜。客户端在别的机器上时，说明里会要求换成服务器的内网地址。
//   3. 这份说明不是“可用”的证明：配置没写、卷没挂的时候照样显示，
//      但第一条注意就会写明此刻连不上。

import { computed, ref, watch } from 'vue'
import { NAlert, NButton, NModal, NTag } from 'naive-ui'

import { capabilityAvailable, clientHints, type ShareCapability, type ShareRecord } from '@/api/shares'

const props = defineProps<{
  show: boolean
  share: ShareRecord
  /** 面板此刻被访问用的主机名（页面传 window.location.hostname）。 */
  host: string
  capability: ShareCapability | null
}>()

const emit = defineEmits<{ 'update:show': [boolean] }>()

const copied = ref<string>('')
const copyError = ref<string | null>(null)

const hints = computed(() => clientHints(props.share, props.host))
const available = computed(() => capabilityAvailable(props.capability))

watch(
  () => props.show,
  (open) => {
    if (open) {
      copied.value = ''
      copyError.value = null
    }
  },
)

async function copy(text: string, key: string): Promise<void> {
  copyError.value = null
  try {
    await navigator.clipboard.writeText(text)
    copied.value = key
  } catch {
    // 浏览器不给剪贴板权限时不要假装复制成功：让人自己选中复制。
    copied.value = ''
    copyError.value = '浏览器拒绝了剪贴板访问，请手动选中复制。'
  }
}
</script>

<template>
  <NModal
    :show="show"
    preset="card"
    :title="`客户端怎么连「${share.definition.name}」`"
    class="help"
    @update:show="(v: boolean) => emit('update:show', v)"
  >
    <div class="help__body">
      <NAlert v-if="!available" type="warning" :bordered="false">
        Samba 能力此刻不可用（{{ capability?.reason || '未报告原因' }}）：下面的地址与命令仍然是对的，
        但在这台机器能把 Samba 跑起来之前连不上。
      </NAlert>
      <NAlert v-if="!share.config_applied || share.runtime_revoked" type="warning" :bordered="false">
        这条共享此刻没有写在 Samba 配置里。地址先收着，等配置应用之后再连。
      </NAlert>

      <p class="help__muted">
        主机名取自你此刻访问面板用的地址：<code>{{ host }}</code>。客户端在别的机器上时，
        把命令里的 <code>{{ host }}</code> 换成服务器的内网地址。
      </p>

      <section v-for="hint in hints" :key="hint.kind" class="help__section" :data-client="hint.kind">
        <h3 class="help__title">{{ hint.label }}</h3>

        <p v-if="hint.address !== ''" class="help__address">
          <code>{{ hint.address }}</code>
          <NButton size="tiny" secondary @click="copy(hint.address, `${hint.kind}-address`)">
            {{ copied === `${hint.kind}-address` ? '已复制' : '复制地址' }}
          </NButton>
        </p>

        <div v-if="hint.commands.length > 0" class="help__commands">
          <div v-for="(cmd, i) in hint.commands" :key="`${hint.kind}-${i}`" class="help__command">
            <code class="help__code">{{ cmd }}</code>
            <NButton size="tiny" tertiary @click="copy(cmd, `${hint.kind}-cmd-${i}`)">
              {{ copied === `${hint.kind}-cmd-${i}` ? '已复制' : '复制' }}
            </NButton>
          </div>
        </div>

        <ul class="help__notes">
          <li v-for="note in hint.notes" :key="note">{{ note }}</li>
        </ul>
      </section>

      <NAlert v-if="copyError" type="warning" :bordered="false" :title="copyError" />

      <div class="help__safety">
        <NTag size="small">命令里不含密码</NTag>
        <NTag size="small">凭据文件 0600</NTag>
        <NTag size="small">名单管不住本机 127.0.0.1</NTag>
      </div>
    </div>
  </NModal>
</template>

<style scoped>
.help__body {
  display: flex;
  flex-direction: column;
  gap: 14px;
  max-height: 70vh;
  overflow: auto;
}

.help__muted {
  margin: 0;
  font-size: 13px;
  line-height: 1.7;
  opacity: 0.8;
}

.help__section {
  display: flex;
  flex-direction: column;
  gap: 8px;
  padding-bottom: 12px;
  border-bottom: 1px solid rgba(128, 128, 128, 0.2);
}

.help__title {
  margin: 0;
  font-size: 14px;
  font-weight: 600;
}

.help__address {
  display: flex;
  align-items: center;
  gap: 8px;
  margin: 0;
  flex-wrap: wrap;
}

.help__commands {
  display: flex;
  flex-direction: column;
  gap: 6px;
}

.help__command {
  display: flex;
  align-items: flex-start;
  gap: 8px;
}

.help__code {
  flex: 1;
  font-size: 12px;
  line-height: 1.6;
  padding: 6px 8px;
  border-radius: 4px;
  background: rgba(128, 128, 128, 0.12);
  word-break: break-all;
  white-space: pre-wrap;
}

.help__notes {
  margin: 0;
  padding-left: 18px;
  font-size: 13px;
  line-height: 1.7;
  opacity: 0.85;
}

.help__safety {
  display: flex;
  gap: 6px;
  flex-wrap: wrap;
}
</style>
