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
  <div v-if="payload" class="worker-modal-backdrop" @click.self="closeModal">
    <div
      class="worker-modal"
      role="dialog"
      aria-modal="true"
      aria-labelledby="worker-created-dialog-title"
    >
      <div class="worker-modal-header">
        <h3 id="worker-created-dialog-title">Worker Created</h3>
        <button type="button" class="ghost-btn small" @click="closeModal">Done</button>
      </div>

      <div class="worker-modal-content">
        <p class="worker-modal-note">启动信息仅在创建时返回，请立即复制并安全保存。</p>

        <div class="worker-meta">
          <p>
            <span>Node ID</span>
            <code>{{ payload.node_id }}</code>
          </p>
          <p>
            <span>Worker Secret</span>
            <code>{{ workerSecretDisplay }}</code>
            <button
              v-if="workerSecret"
              type="button"
              class="ghost-btn small"
              :disabled="copyingCommand"
              @click="secretVisible = !secretVisible"
            >
              {{ secretVisible ? 'Hide' : 'Show' }}
            </button>
          </p>
        </div>

        <div class="worker-command-block">
          <p>Startup Command</p>
          <code>{{ commandText }}</code>
        </div>
      </div>

      <div class="worker-modal-actions">
        <button
          type="button"
          class="ghost-btn small"
          :disabled="copyingCommand || !commandText"
          @click="copyStartupCommand"
        >
          {{ copyButtonText }}
        </button>
        <button type="button" class="primary-btn small" @click="closeModal">Done</button>
      </div>
    </div>
  </div>
</template>

<style scoped>
.worker-modal-backdrop {
  position: fixed;
  inset: 0;
  z-index: 1000;
  background: rgba(0, 0, 0, 0.4);
  backdrop-filter: blur(4px);
  display: flex;
  align-items: center;
  justify-content: center;
  padding: 24px;
}

.worker-modal {
  width: min(640px, 100%);
  border-radius: var(--radius-lg);
  border: 1px solid var(--stroke);
  background: var(--surface);
  box-shadow: var(--shadow-modal);
  display: flex;
  flex-direction: column;
}

.worker-modal-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 20px 24px;
  border-bottom: 1px solid var(--stroke);
}

.worker-modal-header h3 {
  margin: 0;
  font-size: 20px;
  font-weight: 600;
}

.worker-modal-content {
  padding: 24px;
  display: grid;
  gap: 20px;
}

.worker-modal-note {
  margin: 0;
  color: var(--text-secondary);
  font-size: 14px;
  line-height: 1.5;
}

.worker-meta {
  display: grid;
  gap: 12px;
}

.worker-meta p {
  margin: 0;
  display: flex;
  align-items: flex-start;
  gap: 12px;
}

.worker-meta span {
  flex-shrink: 0;
  width: 120px;
  color: var(--text-secondary);
  font-size: 13px;
  font-weight: 500;
}

.worker-meta code {
  flex: 1;
  min-width: 0;
  font-family: 'JetBrains Mono', monospace;
  font-size: 13px;
  background: var(--surface-soft);
  border: 1px solid var(--stroke);
  border-radius: var(--radius);
  padding: 4px 8px;
  word-break: break-all;
  white-space: pre-wrap;
}

.worker-command-block {
  display: grid;
  gap: 8px;
}

.worker-command-block p {
  margin: 0;
  color: var(--text-primary);
  font-size: 14px;
  font-weight: 500;
}

.worker-command-block code {
  display: block;
  border: 1px solid var(--stroke);
  border-radius: var(--radius);
  background: #000;
  color: #fff;
  padding: 16px;
  font-family: 'JetBrains Mono', monospace;
  font-size: 13px;
  line-height: 1.6;
  word-break: break-all;
  white-space: pre-wrap;
}

.worker-modal-actions {
  display: flex;
  justify-content: flex-end;
  gap: 12px;
  padding: 20px 24px;
  border-top: 1px solid var(--stroke);
  border-bottom-left-radius: var(--radius-lg);
  border-bottom-right-radius: var(--radius-lg);
}

@media (max-width: 700px) {
  .worker-meta p {
    flex-wrap: wrap;
    gap: 4px;
  }

  .worker-meta span {
    width: 100%;
  }

  .worker-modal-actions {
    flex-direction: column-reverse;
  }

  .worker-modal-actions button {
    width: 100%;
  }
}
</style>
