<script setup lang="ts">
const props = defineProps<{
  eyebrow: string
  title: string
  loading?: boolean
  refreshLabel?: string
  hideRefresh?: boolean
  refreshedAtText?: string
}>()

const emit = defineEmits<{
  refresh: []
}>()
</script>

<template>
  <div class="grid gap-2">
    <header
      class="flex items-start justify-between gap-5 bg-surface border border-stroke rounded-lg p-8 shadow-card animate-rise-in max-[960px]:flex-col"
    >
      <div>
        <p class="m-0 font-mono text-xs tracking-[0.05em] uppercase text-secondary">
          {{ eyebrow }}
        </p>
        <h1 class="mt-3 mb-2 text-2xl font-semibold leading-[1.2] tracking-[-0.02em]">
          {{ title }}
        </h1>
        <div class="m-0 text-secondary text-sm leading-normal">
          <slot name="subtitle"></slot>
        </div>
      </div>

      <div class="flex items-center gap-3 max-[960px]:w-full max-[960px]:flex-wrap">
        <slot name="actions"></slot>

        <button
          v-if="!hideRefresh"
          class="ui-btn-primary rounded-md px-3.5 py-2 text-sm font-medium h-9 inline-flex items-center justify-center border transition-all duration-200 disabled:cursor-not-allowed disabled:opacity-50"
          type="button"
          :disabled="loading"
          :aria-label="loading ? 'Refreshing...' : props.refreshLabel || 'Refresh'"
          @click="emit('refresh')"
        >
          <span class="grid">
            <span
              aria-hidden="true"
              class="[grid-area:1/1] transition-opacity duration-200"
              :class="loading ? 'opacity-100' : 'opacity-0'"
            >
              Refreshing...
            </span>
            <span
              aria-hidden="true"
              class="[grid-area:1/1] transition-opacity duration-200"
              :class="loading ? 'opacity-0' : 'opacity-100'"
            >
              {{ props.refreshLabel || 'Refresh' }}
            </span>
          </span>
        </button>
      </div>
    </header>

    <div v-if="refreshedAtText !== undefined" class="flex justify-end items-center animate-rise-in">
      <p class="m-0 text-secondary text-[13px]">
        Last refresh:
        <span class="text-primary font-medium">{{ refreshedAtText }}</span>
      </p>
    </div>
  </div>
</template>
