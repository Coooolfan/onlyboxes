<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useRouter } from 'vue-router'

import { setPrefill } from '@/composables/useWorkerStartupPrefill'
import type { WorkerStartupKind } from '@/types/worker-startup-tool'
import type { WorkerStartupCommandResponse, WorkerType } from '@/types/workers'

const workerTypeToStartupKind: Record<WorkerType, WorkerStartupKind> = {
  normal: 'worker-docker',
  'worker-sys': 'worker-sys',
}

const router = useRouter()

const props = defineProps<{
  payload: WorkerStartupCommandResponse | null
}>()

const emit = defineEmits<{
  close: []
}>()

const secretVisible = ref(false)

const workerSecret = computed(() => props.payload?.worker_secret ?? '')

const workerSecretDisplay = computed(() => {
  if (!workerSecret.value) {
    return 'Unavailable'
  }
  if (secretVisible.value) {
    return workerSecret.value
  }
  return maskSecret(workerSecret.value)
})

function maskSecret(secret: string): string {
  const trimmed = secret.trim()
  if (!trimmed) {
    return 'Unavailable'
  }
  if (trimmed.length <= 8) {
    return '*'.repeat(trimmed.length)
  }
  const middleMaskLength = Math.max(4, trimmed.length - 8)
  return `${trimmed.slice(0, 4)}${'*'.repeat(middleMaskLength)}${trimmed.slice(-4)}`
}

async function goToStartupTool(): Promise<void> {
  if (!props.payload) {
    return
  }
  const workerKind = workerTypeToStartupKind[props.payload.type] ?? 'worker-docker'
  setPrefill({
    workerKind,
    workerID: props.payload.node_id,
    workerSecret: workerSecret.value,
  })
  const failure = await router.push('/tools/worker-startup')
  if (!failure) {
    closeModal()
  }
}

function closeModal(): void {
  secretVisible.value = false
  emit('close')
}

watch(
  () => props.payload,
  () => {
    secretVisible.value = false
  },
)
</script>

<template>
  <Teleport to="body">
    <div
      v-if="payload"
      class="fixed inset-0 z-1000 bg-black/40 backdrop-blur-xs flex items-center justify-center p-6"
      @click.self="closeModal"
    >
      <div
        class="worker-modal w-[min(640px,100%)] rounded-lg border border-stroke bg-surface shadow-modal flex flex-col"
        role="dialog"
        aria-modal="true"
        aria-labelledby="worker-created-dialog-title"
      >
        <div class="flex items-center justify-between px-6 py-5 border-b border-stroke">
          <h3 id="worker-created-dialog-title" class="m-0 text-xl font-semibold">Worker Created</h3>
        </div>

        <div class="p-6 grid gap-5">
          <p class="m-0 text-secondary text-sm leading-normal">
            Startup details are returned only once at creation time. Copy and store them securely
            now.
          </p>

          <div class="grid gap-3">
            <p class="m-0 flex items-start gap-3 max-[700px]:flex-wrap max-[700px]:gap-1">
              <span
                class="shrink-0 w-[120px] text-secondary text-[13px] font-medium max-[700px]:w-full"
                >Node ID</span
              >
              <code
                class="flex-1 min-w-0 font-mono text-[13px] bg-surface-soft border border-stroke rounded-default px-2 py-1 break-all whitespace-pre-wrap"
                >{{ payload.node_id }}</code
              >
            </p>
            <p class="m-0 flex items-start gap-3 max-[700px]:flex-wrap max-[700px]:gap-1">
              <span
                class="shrink-0 w-[120px] text-secondary text-[13px] font-medium max-[700px]:w-full"
                >Worker Type</span
              >
              <code
                class="flex-1 min-w-0 font-mono text-[13px] bg-surface-soft border border-stroke rounded-default px-2 py-1 break-all whitespace-pre-wrap"
                >{{ payload.type }}</code
              >
            </p>
            <p class="m-0 flex items-start gap-3 max-[700px]:flex-wrap max-[700px]:gap-1">
              <span
                class="shrink-0 w-[120px] text-secondary text-[13px] font-medium max-[700px]:w-full"
                >Worker Secret</span
              >
              <code
                class="flex-1 min-w-0 font-mono text-[13px] bg-surface-soft border border-stroke rounded-default px-2 py-1 break-all whitespace-pre-wrap"
                >{{ workerSecretDisplay }}</code
              >
              <button
                v-if="workerSecret"
                type="button"
                class="rounded-md px-3 py-1.5 text-[13px] font-medium h-8 inline-flex items-center justify-center text-primary bg-surface border border-stroke transition-all duration-200 hover:not-disabled:border-stroke-hover hover:not-disabled:bg-surface-soft disabled:cursor-not-allowed disabled:opacity-50"
                @click="secretVisible = !secretVisible"
              >
                {{ secretVisible ? 'Hide' : 'Show' }}
              </button>
            </p>
          </div>

          <div class="mt-1 bg-surface-soft border border-stroke rounded-xl p-5">
            <div class="flex items-start gap-4">
              <div
                class="bg-surface p-2.5 rounded-lg border border-stroke text-secondary shrink-0 mt-0.5"
              >
                <svg
                  class="w-5 h-5"
                  xmlns="http://www.w3.org/2000/svg"
                  viewBox="0 0 24 24"
                  fill="none"
                  stroke="currentColor"
                  stroke-width="2"
                  stroke-linecap="round"
                  stroke-linejoin="round"
                >
                  <polyline points="4 17 10 11 4 5" />
                  <line x1="12" x2="20" y1="19" y2="19" />
                </svg>
              </div>
              <div class="flex-1">
                <h4 class="text-sm font-semibold text-primary m-0 mb-1">Quick Startup</h4>
                <p class="text-[13px] text-secondary m-0 mb-3.5 leading-normal">
                  Use the startup tool to quickly configure and generate a startup script for this
                  worker.
                </p>
                <button
                  type="button"
                  class="inline-flex items-center gap-2 px-4 py-2.5 bg-surface border border-stroke text-primary rounded-lg text-sm font-medium transition-all duration-200 hover:border-stroke-hover hover:bg-surface-soft active:scale-95 group"
                  data-testid="open-in-startup-tool"
                  @click="goToStartupTool"
                >
                  Open in Startup Tool with Id and Secret
                  <svg
                    class="w-4 h-4 text-secondary group-hover:text-primary transition-colors"
                    xmlns="http://www.w3.org/2000/svg"
                    viewBox="0 0 24 24"
                    fill="none"
                    stroke="currentColor"
                    stroke-width="2"
                    stroke-linecap="round"
                    stroke-linejoin="round"
                  >
                    <path d="M15 3h6v6" />
                    <path d="M10 14 21 3" />
                    <path d="M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h6" />
                  </svg>
                </button>
              </div>
            </div>
          </div>
        </div>

        <div class="flex justify-end gap-3 px-6 py-5 border-t border-stroke rounded-b-lg">
          <button
            type="button"
            class="rounded-md px-3 py-1.5 text-[13px] font-medium h-8 inline-flex items-center justify-center text-white bg-accent border border-accent transition-all duration-200 hover:not-disabled:bg-[#333] hover:not-disabled:border-[#333] disabled:cursor-not-allowed disabled:opacity-50"
            @click="closeModal"
          >
            Done
          </button>
        </div>
      </div>
    </div>
  </Teleport>
</template>
