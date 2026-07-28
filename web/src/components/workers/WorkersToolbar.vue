<script setup lang="ts">
import AppSegmented from '@/components/ui/AppSegmented.vue'
import type { WorkerStatus } from '@/types/workers'

defineProps<{
  statusFilter: WorkerStatus
  showDetails: boolean
}>()

const emit = defineEmits<{
  setStatus: [status: WorkerStatus]
  'update:showDetails': [value: boolean]
}>()

const statusOptions = [
  { value: 'all', label: 'All' },
  { value: 'online', label: 'Online' },
  { value: 'offline', label: 'Offline' },
] as const satisfies ReadonlyArray<{ value: WorkerStatus; label: string }>

function handleStatusUpdate(value: string): void {
  emit('setStatus', value as WorkerStatus)
}
</script>

<template>
  <div
    class="flex items-center justify-between gap-4 border-b border-stroke bg-surface-soft px-6 py-4 max-[720px]:flex-col max-[720px]:items-start"
  >
    <AppSegmented
      :model-value="statusFilter"
      :options="statusOptions"
      aria-label="Filter workers by status"
      @update:model-value="handleStatusUpdate"
    />

    <label
      class="inline-flex cursor-pointer items-center gap-2 rounded-md border border-stroke bg-surface px-3 py-2 text-[13px] text-primary transition-colors hover:border-stroke-hover"
    >
      <input
        type="checkbox"
        class="ui-checkbox h-4 w-4 rounded"
        :checked="showDetails"
        @change="emit('update:showDetails', ($event.target as HTMLInputElement).checked)"
      />
      <span>Show Details</span>
    </label>
  </div>
</template>
