<script setup lang="ts">
import AppButton from '@/components/ui/AppButton.vue'
import AppModal from '@/components/ui/AppModal.vue'
import type { WorkerType } from '@/types/workers'

defineProps<{
  open: boolean
  loading?: boolean
}>()

const emit = defineEmits<{
  close: []
  select: [workerType: WorkerType]
}>()

const workerOptions: ReadonlyArray<{
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
  <AppModal
    :open="open"
    title="Choose Worker Type"
    description="Select the execution profile to generate startup credentials."
    :persistent="loading"
    @close="emit('close')"
  >
    <div class="grid gap-3">
      <button
        v-for="option in workerOptions"
        :key="option.type"
        type="button"
        class="ui-btn-secondary ui-focusable w-full cursor-pointer rounded-lg border px-4 py-4 text-left transition-[background-color,border-color,transform] duration-200 hover:border-stroke-hover active:scale-[0.995] disabled:cursor-not-allowed disabled:opacity-50"
        :disabled="loading"
        :data-testid="`worker-type-option-${option.type}`"
        @click="emit('select', option.type)"
      >
        <span class="block text-sm font-semibold text-primary">{{ option.title }}</span>
        <span class="mt-1 block text-sm text-secondary">{{ option.description }}</span>
      </button>
    </div>

    <template #footer>
      <AppButton size="sm" :disabled="loading" @click="emit('close')">Cancel</AppButton>
    </template>
  </AppModal>
</template>
