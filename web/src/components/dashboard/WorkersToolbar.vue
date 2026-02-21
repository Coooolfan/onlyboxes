<script setup lang="ts">
import type { WorkerStatus } from '@/types/workers'

defineProps<{
  statusFilter: WorkerStatus
  refreshedAtText: string
}>()

const emit = defineEmits<{
  setStatus: [status: WorkerStatus]
}>()
</script>

<template>
  <div class="flex justify-between items-center gap-4 px-6 py-4 border-b border-stroke bg-surface-soft max-[960px]:flex-col max-[960px]:items-start">
    <div class="inline-flex gap-1 bg-surface border border-stroke rounded-default p-1">
      <button
        type="button"
        :class="[
          'rounded-[6px] px-4 py-1.5 text-[13px] font-medium transition-all duration-200 border-0 cursor-pointer',
          statusFilter === 'all'
            ? 'text-white bg-accent'
            : 'text-secondary bg-transparent hover:text-primary hover:bg-surface-soft'
        ]"
        @click="emit('setStatus', 'all')"
      >
        All
      </button>
      <button
        type="button"
        :class="[
          'rounded-[6px] px-4 py-1.5 text-[13px] font-medium transition-all duration-200 border-0 cursor-pointer',
          statusFilter === 'online'
            ? 'text-white bg-accent'
            : 'text-secondary bg-transparent hover:text-primary hover:bg-surface-soft'
        ]"
        @click="emit('setStatus', 'online')"
      >
        Online
      </button>
      <button
        type="button"
        :class="[
          'rounded-[6px] px-4 py-1.5 text-[13px] font-medium transition-all duration-200 border-0 cursor-pointer',
          statusFilter === 'offline'
            ? 'text-white bg-accent'
            : 'text-secondary bg-transparent hover:text-primary hover:bg-surface-soft'
        ]"
        @click="emit('setStatus', 'offline')"
      >
        Offline
      </button>
    </div>

    <p class="m-0 text-secondary text-[13px]">
      Last refresh:
      <span class="text-primary font-medium">{{ refreshedAtText }}</span>
    </p>
  </div>
</template>
