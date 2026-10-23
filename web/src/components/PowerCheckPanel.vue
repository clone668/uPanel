<script setup lang="ts">
// 电源任务的事后核对（P11.3）：这次重启/关机到底发生了没有。
//
// 这一块的存在理由只有一个：**受理不是发生**。任务的 result 是 accepted，界面若就此
// 写一句“已重启”，就是在撒谎。唯一的证据是 boot_id 有没有变，而那个比对是服务端做的
// （见 internal/packages/powercheck.go）；这里只负责把结论如实摆出来，不改写、不美化。
//
// 四种状态各有各的说法：
//   confirmed   —— boot_id 变了，机器确实重新启动过（关机同理：它回来过）。
//   unchanged   —— 请求已受理，但还在原来那次开机上；可能仍在倒计时，也可能已被取消。
//   unreachable —— 代理连不上。对关机是预期之内，但**不是**关机成功的证据。
//   unknown     —— 查不清（boot_id 读不出来，或任务里没记下基准）。
//
// 传 null 就整块不渲染：对多数任务这个问题根本不成立，不该出现一个空壳说“未确认”。

import { computed } from 'vue'

import { NTag } from 'naive-ui'

import { POWER_CHECK_LABELS, POWER_CHECK_TYPES } from '@/api/power'
import type { PowerCheckDTO } from '@/api/jobs'
import { formatTimestamp } from '@/utils/format'

const props = defineProps<{
  check: PowerCheckDTO | null
}>()

const bootBefore = computed(() =>
  props.check === null || props.check.boot_id_before === '' ? '没记下' : props.check.boot_id_before,
)
const bootNow = computed(() =>
  props.check === null || props.check.boot_id_now === '' ? '读不到' : props.check.boot_id_now,
)
</script>

<template>
  <section
    v-if="check !== null"
    class="powercheck"
    data-testid="job-power-check"
    :data-state="check.state"
  >
    <h3 class="powercheck__heading">电源核对：这次重启/关机到底发生了没有</h3>
    <p class="powercheck__line">
      <NTag size="small" :type="POWER_CHECK_TYPES[check.state]">
        {{ POWER_CHECK_LABELS[check.state] }}
      </NTag>
      <span>{{ check.message }}</span>
    </p>
    <p class="powercheck__meta">
      核对时间：{{ check.checked_at === '' ? '未知' : formatTimestamp(check.checked_at) }} ·
      下发前 boot_id：{{ bootBefore }} · 现在的 boot_id：{{ bootNow }}
    </p>
    <p class="powercheck__meta">
      这一节的结论只依赖 boot_id：变了才是“机器确实重新启动过”；读不到就是读不到。
      面板不会把“连不上”当成证据——关机之后它本来就连不上。
    </p>
  </section>
</template>

<style scoped>
.powercheck {
  display: flex;
  flex-direction: column;
  gap: 4px;
}

.powercheck__heading {
  margin: 0;
  font-weight: 600;
}

.powercheck__line {
  display: flex;
  align-items: baseline;
  flex-wrap: wrap;
  gap: 6px;
  margin: 0;
  line-height: 1.7;
}

.powercheck__meta {
  margin: 0;
  font-size: 12px;
  opacity: 0.75;
  word-break: break-all;
}
</style>
