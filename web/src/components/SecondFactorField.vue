<script setup lang="ts">
// 六位数验证码输入（P14.6）。
//
// 只在两种地方出现：登录页的第二步、以及任何需要再认证的表单里。做成一个小部件
// 是因为"再认证"散在四个页面里（存储动作、账号管理、备份、终端），每个页面各写一份
// 验证码输入框，迟早有一处忘了带 totp 或忘了清空。
//
// 这里**不是**判据：能不能登录、能不能再认证由服务端决定。这个组件只保证两件小事：
// 输入框只收数字（手机上是数字键盘），以及事件名统一。

import { computed } from 'vue'
import { NFormItem, NInput } from 'naive-ui'

import { sanitizeCode } from './secondfactor'

const props = withDefaults(
  defineProps<{
    /** v-model 的六位数（可以为空）。 */
    modelValue: string
    /** false 时整个部件不渲染（账号没启用二次验证）。 */
    show?: boolean
    disabled?: boolean
    label?: string
    testid?: string
  }>(),
  { show: true, disabled: false, label: '验证码', testid: 'totp-code' },
)

const emit = defineEmits<{ 'update:modelValue': [string] }>()

const value = computed({
  get: () => props.modelValue,
  set: (next: string) => emit('update:modelValue', sanitizeCode(next)),
})
</script>

<template>
  <NFormItem v-if="show" :label="label">
    <NInput
      :value="value"
      placeholder="手机上的六位数"
      inputmode="numeric"
      autocomplete="one-time-code"
      :maxlength="6"
      :disabled="disabled"
      :data-testid="testid"
      @update:value="(next: string) => (value = next)"
    />
  </NFormItem>
</template>
