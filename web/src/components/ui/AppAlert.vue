<script setup lang="ts">
import AppIcon from '@/components/ui/AppIcon.vue'
import type { IconName } from '@/components/ui/icons'

type AlertTone = 'error' | 'success' | 'warning'

const props = withDefaults(
  defineProps<{
    tone?: AlertTone
    title?: string
    /** Renders the tone icon in front of the message. */
    withIcon?: boolean
  }>(),
  {
    tone: 'error',
    withIcon: false,
  },
)

const toneClass: Record<AlertTone, string> = {
  error: 'ui-alert-error',
  success: 'ui-alert-success',
  warning: 'ui-alert-warning',
}

const toneIcon: Record<AlertTone, IconName> = {
  error: 'alert-triangle',
  success: 'check',
  warning: 'alert-triangle',
}
</script>

<template>
  <div
    class="ui-alert rounded-default px-4 py-2.5 text-sm"
    :class="toneClass[props.tone]"
    role="alert"
  >
    <div class="flex items-start gap-2">
      <AppIcon v-if="withIcon" :name="toneIcon[props.tone]" :size="15" class="mt-0.5" />
      <div class="min-w-0 flex-1">
        <p v-if="title" class="m-0 mb-1 font-medium">{{ title }}</p>
        <slot />
      </div>
    </div>
  </div>
</template>
