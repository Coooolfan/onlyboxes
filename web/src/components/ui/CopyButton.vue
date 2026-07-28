<script setup lang="ts">
import { computed } from 'vue'

import AppButton from '@/components/ui/AppButton.vue'
import type { IconName } from '@/components/ui/icons'
import { copyStatusLabel, type CopyStatus } from '@/composables/useCopyFeedback'

const props = withDefaults(
  defineProps<{
    status: CopyStatus
    label?: string
    variant?: 'primary' | 'secondary' | 'ghost'
    size?: 'sm' | 'md'
    disabled?: boolean
  }>(),
  {
    label: 'Copy',
    variant: 'secondary',
    size: 'sm',
    disabled: false,
  },
)

const emit = defineEmits<{ copy: [] }>()

const text = computed(() => copyStatusLabel(props.status, props.label))
const icon = computed<IconName>(() => (props.status === 'copied' ? 'check' : 'copy'))
</script>

<template>
  <AppButton
    :variant="variant"
    :size="size"
    :icon="icon"
    :loading="status === 'copying'"
    :disabled="disabled"
    @click="emit('copy')"
  >
    {{ text }}
  </AppButton>
</template>
