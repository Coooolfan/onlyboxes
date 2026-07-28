<script setup lang="ts">
import { computed, useSlots } from 'vue'

import AppIcon from '@/components/ui/AppIcon.vue'
import AppSpinner from '@/components/ui/AppSpinner.vue'
import type { IconName } from '@/components/ui/icons'

type ButtonVariant = 'primary' | 'secondary' | 'danger' | 'ghost'
type ButtonSize = 'sm' | 'md'

const props = withDefaults(
  defineProps<{
    variant?: ButtonVariant
    size?: ButtonSize
    type?: 'button' | 'submit' | 'reset'
    loading?: boolean
    disabled?: boolean
    block?: boolean
    icon?: IconName
    iconOnly?: boolean
  }>(),
  {
    variant: 'secondary',
    size: 'md',
    type: 'button',
    loading: false,
    disabled: false,
    block: false,
    iconOnly: false,
  },
)

const slots = useSlots()

const variantClass: Record<ButtonVariant, string> = {
  primary: 'ui-btn-primary border',
  secondary: 'ui-btn-secondary border',
  danger: 'ui-btn-danger border',
  ghost: 'border border-transparent text-secondary hover:text-primary hover:bg-surface-soft',
}

const sizeClass: Record<ButtonSize, string> = {
  sm: 'h-8 text-[13px] gap-1.5',
  md: 'h-9 text-sm gap-2',
}

const paddingClass = computed(() => {
  if (props.iconOnly) {
    return props.size === 'sm' ? 'w-8 px-0' : 'w-9 px-0'
  }
  return props.size === 'sm' ? 'px-3' : 'px-3.5'
})

const isDisabled = computed(() => props.disabled || props.loading)

const showLeadingIcon = computed(() => Boolean(props.icon) && !props.loading)
const hasLabel = computed(() => !props.iconOnly && Boolean(slots.default))
</script>

<template>
  <button
    :type="type"
    :disabled="isDisabled"
    :aria-busy="loading || undefined"
    class="ui-focusable inline-flex shrink-0 cursor-pointer items-center justify-center rounded-md font-medium transition-[background-color,border-color,color,box-shadow,transform] duration-200 select-none active:scale-[0.98] disabled:pointer-events-none disabled:cursor-not-allowed disabled:opacity-50"
    :class="[
      variantClass[variant],
      sizeClass[size],
      paddingClass,
      block ? 'w-full' : '',
      loading ? 'cursor-progress' : '',
    ]"
  >
    <AppSpinner v-if="loading" :size="size === 'sm' ? 13 : 15" />
    <AppIcon v-else-if="showLeadingIcon" :name="icon!" :size="size === 'sm' ? 14 : 16" />
    <span v-if="hasLabel" class="whitespace-nowrap"><slot /></span>
  </button>
</template>
