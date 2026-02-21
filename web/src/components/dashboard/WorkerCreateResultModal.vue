<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'

import type { WorkerStartupCommandResponse } from '@/types/workers'
import { writeTextToClipboard } from '@/utils/clipboard'

const props = defineProps<{
  payload: WorkerStartupCommandResponse | null
}>()

const emit = defineEmits<{
  close: []
}>()

const secretVisible = ref(false)
const copyingCommand = ref(false)
const copiedCommand = ref(false)
const copyFailed = ref(false)

let copyFeedbackTimer: ReturnType<typeof setTimeout> | null = null

const commandText = computed(() => props.payload?.command?.trim() ?? '')
const workerSecret = computed(() => extractEnvValue(commandText.value, 'WORKER_SECRET'))

const workerSecretDisplay = computed(() => {
  if (!workerSecret.value) {
    return 'Unavailable'
  }
  if (secretVisible.value) {
    return workerSecret.value
  }
  return maskSecret(workerSecret.value)
})

const copyButtonText = computed(() => {
  if (copyingCommand.value) {
    return 'Copying...'
  }
  if (copiedCommand.value) {
    return 'Copied'
  }
  if (copyFailed.value) {
    return 'Copy Failed'
  }
  return 'Copy Startup Command'
})

function escapeRegExp(value: string): string {
  return value.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
}

function extractEnvValue(command: string, key: string): string {
  const trimmedCommand = command.trim()
  if (!trimmedCommand || !key.trim()) {
    return ''
  }

  const pattern = new RegExp(`(?:^|\\s)${escapeRegExp(key)}=([^\\s]+)`)
  const matched = trimmedCommand.match(pattern)
  if (!matched || matched.length < 2) {
    return ''
  }

  const capturedValue = matched[1]
  if (typeof capturedValue !== 'string') {
    return ''
  }
  let value = capturedValue.trim()
  if (
    (value.startsWith('"') && value.endsWith('"')) ||
    (value.startsWith("'") && value.endsWith("'"))
  ) {
    value = value.slice(1, -1)
  }
  return value
}

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

function resetCopyFeedback(): void {
  if (copyFeedbackTimer) {
    clearTimeout(copyFeedbackTimer)
    copyFeedbackTimer = null
  }
  copyingCommand.value = false
  copiedCommand.value = false
  copyFailed.value = false
}

function scheduleCopyFeedbackReset(): void {
  if (copyFeedbackTimer) {
    clearTimeout(copyFeedbackTimer)
  }
  copyFeedbackTimer = setTimeout(() => {
    copiedCommand.value = false
    copyFailed.value = false
    copyFeedbackTimer = null
  }, 1500)
}

async function copyStartupCommand(): Promise<void> {
  if (!commandText.value || copyingCommand.value) {
    return
  }
  resetCopyFeedback()
  copyingCommand.value = true
  try {
    await writeTextToClipboard(commandText.value, {
      fallbackErrorMessage: 'Failed to copy startup command.',
    })
    copiedCommand.value = true
    scheduleCopyFeedbackReset()
  } catch {
    copyFailed.value = true
    scheduleCopyFeedbackReset()
  } finally {
    copyingCommand.value = false
  }
}

function closeModal(): void {
  secretVisible.value = false
  resetCopyFeedback()
  emit('close')
}

watch(
  () => props.payload,
  () => {
    secretVisible.value = false
    resetCopyFeedback()
  },
)

onBeforeUnmount(() => {
  resetCopyFeedback()
})
</script>

<template>
  <div v-if="payload" class="fixed inset-0 z-1000 bg-black/40 backdrop-blur-xs flex items-center justify-center p-6" @click.self="closeModal">
    <div
      class="w-[min(640px,100%)] rounded-lg border border-stroke bg-surface shadow-modal flex flex-col"
      role="dialog"
      aria-modal="true"
      aria-labelledby="worker-created-dialog-title"
    >
      <div class="flex items-center justify-between px-6 py-5 border-b border-stroke">
        <h3 id="worker-created-dialog-title" class="m-0 text-xl font-semibold">Worker Created</h3>
        <button
          type="button"
          class="rounded-md px-3 py-1.5 text-[13px] font-medium h-8 inline-flex items-center justify-center text-primary bg-surface border border-stroke transition-all duration-200 hover:not-disabled:border-stroke-hover hover:not-disabled:bg-surface-soft disabled:cursor-not-allowed disabled:opacity-50"
          @click="closeModal"
        >
          Done
        </button>
      </div>

      <div class="p-6 grid gap-5">
        <p class="m-0 text-secondary text-sm leading-normal">Startup details are returned only once at creation time. Copy and store them securely now.</p>

        <div class="grid gap-3">
          <p class="m-0 flex items-start gap-3 max-[700px]:flex-wrap max-[700px]:gap-1">
            <span class="shrink-0 w-[120px] text-secondary text-[13px] font-medium max-[700px]:w-full">Node ID</span>
            <code class="flex-1 min-w-0 font-mono text-[13px] bg-surface-soft border border-stroke rounded-default px-2 py-1 break-all whitespace-pre-wrap">{{ payload.node_id }}</code>
          </p>
          <p class="m-0 flex items-start gap-3 max-[700px]:flex-wrap max-[700px]:gap-1">
            <span class="shrink-0 w-[120px] text-secondary text-[13px] font-medium max-[700px]:w-full">Worker Secret</span>
            <code class="flex-1 min-w-0 font-mono text-[13px] bg-surface-soft border border-stroke rounded-default px-2 py-1 break-all whitespace-pre-wrap">{{ workerSecretDisplay }}</code>
            <button
              v-if="workerSecret"
              type="button"
              class="rounded-md px-3 py-1.5 text-[13px] font-medium h-8 inline-flex items-center justify-center text-primary bg-surface border border-stroke transition-all duration-200 hover:not-disabled:border-stroke-hover hover:not-disabled:bg-surface-soft disabled:cursor-not-allowed disabled:opacity-50"
              :disabled="copyingCommand"
              @click="secretVisible = !secretVisible"
            >
              {{ secretVisible ? 'Hide' : 'Show' }}
            </button>
          </p>
        </div>

        <div class="grid gap-2">
          <p class="m-0 text-primary text-sm font-medium">Startup Command</p>
          <code class="block border border-stroke rounded-default bg-black text-white p-4 font-mono text-[13px] leading-[1.6] break-all whitespace-pre-wrap">{{ commandText }}</code>
        </div>
      </div>

      <div class="flex justify-end gap-3 px-6 py-5 border-t border-stroke rounded-b-lg max-[700px]:flex-col-reverse max-[700px]:[&>button]:w-full">
        <button
          type="button"
          class="rounded-md px-3 py-1.5 text-[13px] font-medium h-8 inline-flex items-center justify-center text-primary bg-surface border border-stroke transition-all duration-200 hover:not-disabled:border-stroke-hover hover:not-disabled:bg-surface-soft disabled:cursor-not-allowed disabled:opacity-50"
          :disabled="copyingCommand || !commandText"
          @click="copyStartupCommand"
        >
          {{ copyButtonText }}
        </button>
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
</template>
