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

  let value = matched[1].trim()
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
    <div class="worker-modal" role="dialog" aria-modal="true" aria-labelledby="worker-created-dialog-title">
      <div class="worker-modal-header">
        <h3 id="worker-created-dialog-title">Worker Created</h3>
        <button type="button" class="ghost-btn small" @click="closeModal">Done</button>
      </div>

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

      <div class="worker-modal-actions">
        <button type="button" class="ghost-btn small" :disabled="copyingCommand || !commandText" @click="copyStartupCommand">
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
  background: rgba(10, 14, 22, 0.45);
  display: flex;
  align-items: center;
  justify-content: center;
  padding: 16px;
}

.worker-modal {
  width: min(620px, 100%);
  border-radius: 16px;
  border: 1px solid var(--stroke);
  background: var(--surface);
  box-shadow: 0 24px 80px rgba(10, 14, 22, 0.26);
  padding: 18px;
  display: grid;
  gap: 12px;
}

.worker-modal-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
}

.worker-modal-header h3 {
  margin: 0;
  font-size: 1.1rem;
}

.worker-modal-note {
  margin: 0;
  color: var(--text-secondary);
  font-size: 13px;
}

.worker-meta {
  display: grid;
  gap: 8px;
}

.worker-meta p {
  margin: 0;
  display: flex;
  align-items: center;
  gap: 8px;
}

.worker-meta span {
  width: 96px;
  color: var(--text-secondary);
  font-family: 'IBM Plex Mono', monospace;
  font-size: 11px;
  text-transform: uppercase;
  letter-spacing: 0.03em;
}

.worker-meta code {
  flex: 1;
  min-width: 0;
  font-family: 'IBM Plex Mono', monospace;
  background: #f1f4f8;
  border-radius: 6px;
  padding: 2px 6px;
  word-break: break-all;
}

.worker-command-block {
  display: grid;
  gap: 6px;
}

.worker-command-block p {
  margin: 0;
  color: var(--text-secondary);
  font-size: 12px;
  text-transform: uppercase;
  letter-spacing: 0.03em;
}

.worker-command-block code {
  display: block;
  border: 1px solid var(--stroke);
  border-radius: 10px;
  background: #f8faff;
  padding: 10px;
  font-family: 'IBM Plex Mono', monospace;
  font-size: 13px;
  line-height: 1.5;
  word-break: break-all;
}

.worker-modal-actions {
  display: flex;
  justify-content: flex-end;
  gap: 8px;
}

@media (max-width: 700px) {
  .worker-meta p {
    flex-wrap: wrap;
  }

  .worker-meta span {
    width: 100%;
  }

  .worker-modal-actions {
    justify-content: stretch;
  }

  .worker-modal-actions button {
    flex: 1;
  }
}
</style>
