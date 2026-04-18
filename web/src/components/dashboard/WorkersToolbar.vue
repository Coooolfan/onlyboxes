<script setup lang="ts">
import type { WorkerStatus } from '@/types/workers'

defineProps<{
  statusFilter: WorkerStatus
  showDetails: boolean
}>()

const emit = defineEmits<{
  setStatus: [status: WorkerStatus]
  updateShowDetails: [value: boolean]
}>()

function handleShowDetailsChange(event: Event): void {
  emit('updateShowDetails', (event.target as HTMLInputElement).checked)
}
</script>

<template>
  <div
    class="flex items-center justify-between gap-4 px-6 py-4 border-b border-stroke bg-surface-soft max-[720px]:flex-col max-[720px]:items-start"
  >
    <div class="inline-flex gap-1 bg-surface border border-stroke rounded-default p-1">
      <button
        type="button"
        :class="[
          'rounded-[6px] px-4 py-1.5 text-[13px] font-medium transition-all duration-200 border-0 cursor-pointer',
          statusFilter === 'all'
            ? 'ui-on-accent bg-accent'
            : 'text-secondary bg-transparent hover:text-primary hover:bg-surface-soft',
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
            ? 'ui-on-accent bg-accent'
            : 'text-secondary bg-transparent hover:text-primary hover:bg-surface-soft',
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
            ? 'ui-on-accent bg-accent'
            : 'text-secondary bg-transparent hover:text-primary hover:bg-surface-soft',
        ]"
        @click="emit('setStatus', 'offline')"
      >
        Offline
      </button>
    </div>

    <label
      class="inline-flex cursor-pointer items-center gap-2 rounded-md border border-stroke bg-surface px-3 py-2 text-[13px] text-primary"
    >
      <input
        type="checkbox"
        class="h-4 w-4"
        :checked="showDetails"
        @change="handleShowDetailsChange"
      />
      <span>Show Details</span>
    </label>
  </div>
</template>
