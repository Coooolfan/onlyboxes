<script setup lang="ts">
defineProps<{
  label?: string
  hint?: string
  /** Rendered under the control, e.g. contextual validation feedback. */
  note?: string
  /** Makes the field span both columns of a two-column form grid. */
  span?: boolean
}>()
</script>

<template>
  <label class="grid content-start gap-1.5" :class="span ? 'md:col-span-2' : ''">
    <span v-if="label || $slots.label" class="flex items-center gap-2 text-sm text-secondary">
      <slot name="label">{{ label }}</slot>
    </span>
    <span v-if="hint" class="text-xs text-secondary">{{ hint }}</span>
    <slot />
    <span v-if="note" class="text-xs text-secondary">{{ note }}</span>
  </label>
</template>
