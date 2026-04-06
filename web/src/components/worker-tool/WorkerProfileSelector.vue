<script setup lang="ts">
import type { WorkerStartupKind } from '@/types/worker-startup-tool'

const props = defineProps<{
  modelValue: WorkerStartupKind
}>()

const emit = defineEmits<{
  'update:modelValue': [value: WorkerStartupKind]
}>()

function selectKind(kind: WorkerStartupKind): void {
  emit('update:modelValue', kind)
}
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
        data-testid="worker-kind-docker-btn"
        type="button"
        role="tab"
        :aria-selected="props.modelValue === 'worker-docker'"
        class="rounded-md border px-3 py-2 text-left transition-colors grid gap-0.5"
        :class="
          props.modelValue === 'worker-docker'
            ? 'border-accent bg-surface-soft text-primary'
            : 'border-stroke bg-surface text-secondary hover:border-stroke-hover hover:text-primary'
        "
        @click="selectKind('worker-docker')"
      >
        <span class="text-sm font-medium">worker-docker</span>
        <span class="text-xs opacity-80">Container-based execution runtime.</span>
      </button>

      <button
        data-testid="worker-kind-boxlite-btn"
        type="button"
        role="tab"
        :aria-selected="props.modelValue === 'worker-boxlite'"
        class="rounded-md border px-3 py-2 text-left transition-colors grid gap-0.5"
        :class="
          props.modelValue === 'worker-boxlite'
            ? 'border-accent bg-surface-soft text-primary'
            : 'border-stroke bg-surface text-secondary hover:border-stroke-hover hover:text-primary'
        "
        @click="selectKind('worker-boxlite')"
      >
        <span class="text-sm font-medium">worker-boxlite</span>
        <span class="text-xs opacity-80">Boxlite runtime with per-execution Linux kernels.</span>
      </button>

      <button
        data-testid="worker-kind-sys-btn"
        type="button"
        role="tab"
        :aria-selected="props.modelValue === 'worker-sys'"
        class="rounded-md border px-3 py-2 text-left transition-colors grid gap-0.5"
        :class="
          props.modelValue === 'worker-sys'
            ? 'border-accent bg-surface-soft text-primary'
            : 'border-stroke bg-surface text-secondary hover:border-stroke-hover hover:text-primary'
        "
        @click="selectKind('worker-sys')"
      >
        <span class="text-sm font-medium">worker-sys</span>
        <span class="text-xs opacity-80">Host-shell runtime for computerUse/readImage.</span>
      </button>
    </div>
  </div>
</template>
