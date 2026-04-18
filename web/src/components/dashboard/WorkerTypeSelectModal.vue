<script setup lang="ts">
import type { WorkerType } from '@/types/workers'

defineProps<{
  open: boolean
  loading?: boolean
}>()

const emit = defineEmits<{
  close: []
  select: [workerType: WorkerType]
}>()

const workerOptions: Array<{
  type: WorkerType
  title: string
  description: string
}> = [
  {
    type: 'normal',
    title: 'Normal Worker',
    description: 'Container-backed execution node for shared sandbox workloads.',
  },
  {
    type: 'worker-sys',
    title: 'Worker-Sys',
    description: 'Host-backed execution node for direct computerUse and readImage access.',
  },
]
</script>

<template>
  <Teleport to="body">
    <div
      v-if="open"
      class="ui-modal-overlay fixed inset-0 z-1000 flex items-center justify-center p-6"
    >
      <div
        class="worker-type-modal w-[min(560px,100%)] rounded-lg border border-stroke bg-surface shadow-modal flex flex-col"
        role="dialog"
        aria-modal="true"
        aria-labelledby="worker-type-dialog-title"
      >
        <div class="flex items-center justify-between px-6 py-5 border-b border-stroke">
          <div class="grid gap-1">
            <h3 id="worker-type-dialog-title" class="m-0 text-xl font-semibold">
              Choose Worker Type
            </h3>
            <p class="m-0 text-sm text-secondary">
              Select the execution profile to generate startup credentials.
            </p>
          </div>
        </div>

        <div class="p-6 grid gap-3">
          <button
            v-for="option in workerOptions"
            :key="option.type"
            type="button"
            class="ui-btn-secondary w-full cursor-pointer rounded-lg border px-4 py-4 text-left transition-all duration-200 hover:border-stroke-hover hover:bg-surface-soft focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/30 disabled:cursor-not-allowed disabled:opacity-50"
            :disabled="loading"
            :data-testid="`worker-type-option-${option.type}`"
            @click="emit('select', option.type)"
          >
            <span class="block text-sm font-semibold text-primary">{{ option.title }}</span>
            <span class="mt-1 block text-sm text-secondary">{{ option.description }}</span>
          </button>
        </div>

        <div
          class="flex justify-end gap-3 px-6 py-5 border-t border-stroke rounded-b-lg max-[600px]:flex-col-reverse max-[600px]:[&>button]:w-full"
        >
          <button
            type="button"
            class="ui-btn-secondary rounded-md px-3 py-1.5 text-[13px] font-medium h-8 inline-flex items-center justify-center border transition-all duration-200 disabled:cursor-not-allowed disabled:opacity-50"
            :disabled="loading"
            @click="emit('close')"
          >
            Cancel
          </button>
        </div>
      </div>
    </div>
  </Teleport>
</template>
