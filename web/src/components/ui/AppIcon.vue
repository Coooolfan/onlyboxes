<script setup lang="ts">
import { computed } from 'vue'

import { filledIcons, iconShapes, type IconName } from '@/components/ui/icons'

const props = withDefaults(
  defineProps<{
    name: IconName
    size?: number
    strokeWidth?: number
  }>(),
  {
    size: 18,
    strokeWidth: 2,
  },
)

const filled = computed(() => filledIcons.has(props.name))
const shape = computed(() => iconShapes[props.name] ?? '')
</script>

<template>
  <svg
    xmlns="http://www.w3.org/2000/svg"
    :width="size"
    :height="size"
    viewBox="0 0 24 24"
    :fill="filled ? 'currentColor' : 'none'"
    :stroke="filled ? 'none' : 'currentColor'"
    :stroke-width="filled ? undefined : strokeWidth"
    stroke-linecap="round"
    stroke-linejoin="round"
    aria-hidden="true"
    focusable="false"
    class="shrink-0"
    v-html="shape"
  />
</template>
