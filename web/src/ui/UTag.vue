<script setup lang="ts">
// 标签：给短标识用（文件系统、协议、状态机取值），不承担解释。
//
// 和 UState 的分工：UState 说“好不好”，UTag 说“是什么”。
// 这点分开很重要——把 tag 堆成一片彩色，状态就淹了。
// 默认等宽字体：这些值大多是 sda2 / ext4 / GET / http 这种原样照抄的东西。

withDefaults(
  defineProps<{
    tone?: 'neutral' | 'accent' | 'good' | 'warn' | 'bad'
    /** 原样展示的机器值用等宽；中文词组（“受保护”）用正文体。 */
    mono?: boolean
  }>(),
  { tone: 'neutral', mono: true },
)
</script>

<template>
  <span class="u-tag" :class="{ 'u-tag--mono': mono }" :data-tone="tone"><slot /></span>
</template>

<style scoped>
.u-tag {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  font-size: var(--fs-xs);
  line-height: 1.5;
  white-space: nowrap;
  border: 1px solid var(--rule-strong);
  border-radius: var(--r-1);
  padding: 0 5px;
  color: var(--ink-2);
}

.u-tag--mono {
  font-family: var(--font-mono);
  font-variant-numeric: tabular-nums;
}

.u-tag[data-tone='accent'] {
  border-color: var(--accent);
  color: var(--accent);
}

.u-tag[data-tone='good'] {
  border-color: var(--good);
  color: var(--good);
}

.u-tag[data-tone='warn'] {
  border-color: var(--warn);
  color: var(--warn);
}

.u-tag[data-tone='bad'] {
  border-color: var(--bad);
  color: var(--bad);
}
</style>
