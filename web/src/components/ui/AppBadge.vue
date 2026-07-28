<script setup lang="ts">
type BadgeTone =
  | 'soft'
  | 'warning'
  | 'admin'
  | 'member'
  | 'online'
  | 'offline'
  | 'stale'
  | 'neutral'

withDefaults(
  defineProps<{
    tone?: BadgeTone
    /** Pill uses a fully rounded shape, otherwise the default radius is used. */
    pill?: boolean
    mono?: boolean
  }>(),
  {
    tone: 'soft',
    pill: false,
    mono: false,
  },
)

const toneClass: Record<BadgeTone, string> = {
  soft: 'ui-badge-soft',
  warning: 'ui-badge-warning',
  admin: 'ui-role-badge-admin',
  member: 'ui-role-badge-member',
  online: 'ui-status-badge-online',
  offline: 'ui-status-badge-offline',
  stale: 'ui-status-badge-stale',
  neutral: 'border-stroke bg-surface text-tertiary',
}
</script>

<template>
  <span
    class="inline-flex w-fit items-center justify-center gap-1.5 whitespace-nowrap border px-2 py-1 text-[11px] leading-4 font-medium"
    :class="[toneClass[tone], pill ? 'rounded-full' : 'rounded-default', mono ? 'font-mono' : '']"
  >
    <slot />
  </span>
</template>
