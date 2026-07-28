<script setup lang="ts">
import AppButton from '@/components/ui/AppButton.vue'

withDefaults(
  defineProps<{
    eyebrow: string
    title: string
    loading?: boolean
    refreshLabel?: string
    /** Hides the refresh control for pages without a remote data source. */
    hideRefresh?: boolean
    /** Formatted timestamp of the last successful refresh. */
    refreshedAtText?: string
  }>(),
  {
    loading: false,
    refreshLabel: 'Refresh',
    hideRefresh: false,
  },
)

const emit = defineEmits<{ refresh: [] }>()
</script>

<template>
  <div class="grid gap-2">
    <header
      class="ui-rise flex items-start justify-between gap-5 rounded-lg border border-stroke bg-surface p-8 shadow-card max-[960px]:flex-col max-[620px]:p-6"
    >
      <div class="min-w-0">
        <p class="m-0 font-mono text-xs tracking-[0.05em] text-secondary uppercase">
          {{ eyebrow }}
        </p>
        <h1 class="mt-3 mb-2 text-2xl leading-[1.2] font-semibold tracking-[-0.02em]">
          {{ title }}
        </h1>
        <p v-if="$slots.subtitle" class="m-0 text-sm leading-normal text-secondary">
          <slot name="subtitle" />
        </p>
      </div>

      <div class="flex items-center gap-3 max-[960px]:w-full max-[960px]:flex-wrap">
        <slot name="actions" />
        <AppButton
          v-if="!hideRefresh"
          variant="primary"
          icon="refresh"
          :loading="loading"
          :aria-label="refreshLabel"
          @click="emit('refresh')"
        >
          {{ loading ? 'Refreshing...' : refreshLabel }}
        </AppButton>
      </div>
    </header>

    <p
      v-if="refreshedAtText !== undefined"
      class="ui-rise m-0 flex items-center justify-end text-[13px] text-secondary"
      aria-live="polite"
    >
      Last refresh:
      <span class="ml-1 font-medium text-primary">{{ refreshedAtText }}</span>
    </p>
  </div>
</template>
