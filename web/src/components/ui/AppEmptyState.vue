<script setup lang="ts">
import AppIcon from '@/components/ui/AppIcon.vue'
import type { IconName } from '@/components/ui/icons'

withDefaults(
  defineProps<{
    title: string
    description?: string
    icon?: IconName
    /** Dashed variant is used for inline lists, solid for table bodies. */
    dashed?: boolean
  }>(),
  { dashed: false },
)
</script>

<template>
  <div
    class="ui-empty-state grid justify-items-center gap-2 rounded-default border px-6 py-10 text-center"
    :class="dashed ? 'border-dashed' : ''"
  >
    <AppIcon v-if="icon" :name="icon" :size="22" class="text-tertiary" />
    <p class="m-0 text-sm font-medium text-primary">{{ title }}</p>
    <p v-if="description" class="m-0 max-w-[46ch] text-[13px] text-secondary">
      {{ description }}
    </p>
    <div v-if="$slots.default" class="mt-2">
      <slot />
    </div>
  </div>
</template>
