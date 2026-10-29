<script setup lang="ts">
// 重启与关机的入口卡片（P11.3）。
//
// 这一块只做两件事：选倒计时多长、选做哪一件。它**不**执行任何东西——两个按钮都只
// emit 一个 `request`，由页面把参数交给那个统一的确认对话框（预检 → 任务）。
//
// 为什么单独一个组件：页面上这几句话是最容易被稀释的部分。抽出来以后可以在渲染
// 用例里逐字锁住——「不会自己开机」「不构成关机成功的证据」必须真的出现在屏幕上，
// 而不是写在注释里。
//
// 一条不能含糊的区分：`reboot_required` 是**包自己**写的标志（/run/reboot-required），
// 它只说明"某些更新需要重启才完全生效"，与本卡片能不能重启无关——所以没这个标志
// 也照样可以重启，有它也照样可以选择不重启。

import { computed, ref } from 'vue'

import { NButton, NCard } from 'naive-ui'

import {
  POWER_DELAY_MAX,
  POWER_DELAY_MIN,
  POWER_DELAY_PRESETS,
  POWER_OPERATION_VERBS,
  POWEROFF_UNCERTAIN_NOTICE,
  type PowerOperation,
} from '@/api/power'
import { formatAge, formatTimestamp } from '@/utils/format'

const props = defineProps<{
  /** 系统是否请求了重启（来自 /system/power-state 的 reboot_required）。 */
  rebootRequired: boolean
  /** 本 boot_id；空串表示读不到。 */
  bootId: string
  /** 这份事实是什么时候采集的；null 表示这次没读到 power-state。 */
  collectedAt: string | null
}>()

const emit = defineEmits<{ request: [operation: PowerOperation, delaySeconds: number] }>()

const delay = ref<number>(POWER_DELAY_PRESETS[0] ?? POWER_DELAY_MIN)

const bootLine = computed(() => {
  if (props.bootId === '') return '本次启动标识未知（读不到 boot_id）：重启是否真的发生过将无法确认。'
  return `本次启动 ${props.bootId.slice(0, 8)}… · 读取于 ${
    props.collectedAt === null ? '未知时间' : `${formatTimestamp(props.collectedAt)}（${formatAge(props.collectedAt)}）`
  }`
})

function request(operation: PowerOperation): void {
  emit('request', operation, delay.value)
}
</script>

<template>
  <NCard size="small" title="重启与关机" data-testid="power-card">
    <p class="power__line">
      <strong>{{ rebootRequired ? '系统请求重启' : '没有重启请求' }}</strong>
      ——没有它也能重启，有它也可以先不重启：这个标志只说明某些更新要重启才完全生效。
      判据与读取时间见「重启标志」卡片。
    </p>
    <p class="power__line power__meta">{{ bootLine }}</p>

    <p class="power__line">
      两个操作都先预检：代理会读 <code>/proc/sys/kernel/random/boot_id</code>、检查
      <code>/run/systemd/system</code>，以及<strong>此刻有没有别的写任务在跑</strong>——
      关键任务运行期间一律拒绝电源操作，不是排队等它跑完。
    </p>
    <p class="power__line power__meta">
      提交要 5 分钟内的密码再认证，还要在对话框里单独勾一次确认。执行时先倒计时
      （{{ POWER_DELAY_MIN }}—{{ POWER_DELAY_MAX }} 秒），倒计时期间可以取消——那时命令还没有交出去。
      窗口一过，命令就下发，连接随即断开。
    </p>

    <div class="power__actions">
      <label class="power__meta" for="power-delay">倒计时</label>
      <select id="power-delay" v-model.number="delay" class="power__select" data-testid="power-delay">
        <option v-for="preset in POWER_DELAY_PRESETS" :key="preset" :value="preset">
          {{ preset }} 秒
        </option>
      </select>
      <NButton size="small" type="warning" data-testid="power-reboot" @click="request('reboot')">
        {{ POWER_OPERATION_VERBS.reboot }}
      </NButton>
      <NButton size="small" type="error" data-testid="power-poweroff" @click="request('poweroff')">
        {{ POWER_OPERATION_VERBS.poweroff }}
      </NButton>
    </div>

    <p class="power__line power__warn" data-testid="power-poweroff-warn">
      关机之后<strong>不会自己开机</strong>：需要有人到现场或通过带外管理上电。面板也无法确认电源是否真的切断。
    </p>
    <p class="power__line power__warn" data-testid="power-uncertain">{{ POWEROFF_UNCERTAIN_NOTICE }}</p>
    <p class="power__line power__meta">
      受理不等于发生：<code>systemctl</code> 返回 0 只证明命令交出去了。重启是否真的发生，
      由任务详情里的电源核对回答（boot_id 变了才算数）；关机之后面板什么都读不到，
      所以永远不会有那一条证据。
    </p>
  </NCard>
</template>

<style scoped>
.power__line {
  margin: 0 0 6px;
  line-height: 1.7;
}

.power__meta {
  font-size: var(--fs-md);
  opacity: 0.8;
  word-break: break-all;
}

.power__warn {
  color: var(--bad);
}

.power__actions {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
  align-items: center;
  margin: 8px 0;
}

.power__select {
  padding: 2px 6px;
  border: 1px solid var(--rule);
  border-radius: var(--r-2);
  background: transparent;
  font: inherit;
}
</style>
