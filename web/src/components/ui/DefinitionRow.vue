<script setup lang="ts">
withDefaults(
  defineProps<{
    label: string
    value?: string
    /** Renders the value as an inline code chip instead of plain text. */
    code?: boolean
    labelWidth?: string
  }>(),
  { code: true, labelWidth: '120px' },
)
</script>

<template>
  <div class="flex items-start gap-3 text-[13px] max-[700px]:flex-wrap max-[700px]:gap-1">
    <span
      class="shrink-0 font-medium text-secondary max-[700px]:w-full"
      :style="{ width: labelWidth }"
    >
      {{ label }}
    </span>
    <code
      v-if="code"
      class="ui-code-inline min-w-0 flex-1 rounded-default border px-2 py-1 font-mono break-all whitespace-pre-wrap"
    >
      <slot>{{ value }}</slot>
    </code>
    <span v-else class="min-w-0 flex-1 text-secondary">
      <slot>{{ value }}</slot>
    </span>
    <slot name="action" />
  </div>
</template>
