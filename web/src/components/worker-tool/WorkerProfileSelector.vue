<script setup lang="ts">
import type { WorkerStartupKind } from '@/types/worker-startup-tool'

const props = defineProps<{
  modelValue: WorkerStartupKind
}>()

const emit = defineEmits<{
  'update:modelValue': [value: WorkerStartupKind]
}>()

const profiles: ReadonlyArray<{
  kind: WorkerStartupKind
  testId: string
  description: string
}> = [
  {
    kind: 'worker-docker',
    testId: 'worker-kind-docker-btn',
    description: 'Container-based execution runtime.',
  },
  {
    kind: 'worker-boxlite',
    testId: 'worker-kind-boxlite-btn',
    description: 'Boxlite runtime with per-execution Linux kernels.',
  },
  {
    kind: 'worker-sys',
    testId: 'worker-kind-sys-btn',
    description: 'Host-shell runtime for computerUse/readImage.',
  },
]
</script>

<template>
  <div class="grid gap-2">
    <p class="m-0 text-sm font-medium text-primary">Target Worker Profile</p>
    <div
      role="tablist"
      aria-label="Worker profile selector"
      class="grid grid-cols-1 gap-2 md:grid-cols-3"
    >
      <button
        v-for="profile in profiles"
        :key="profile.kind"
        type="button"
        role="tab"
        :data-testid="profile.testId"
        :aria-selected="props.modelValue === profile.kind"
        class="ui-focusable grid cursor-pointer gap-0.5 rounded-md border px-3 py-2 text-left transition-[background-color,border-color,color] duration-200"
        :class="
          props.modelValue === profile.kind
            ? 'border-accent bg-surface-soft text-primary'
            : 'border-stroke bg-surface text-secondary hover:border-stroke-hover hover:text-primary'
        "
        @click="emit('update:modelValue', profile.kind)"
      >
        <span class="text-sm font-medium">{{ profile.kind }}</span>
        <span class="text-xs opacity-80">{{ profile.description }}</span>
      </button>
    </div>
  </div>
</template>
