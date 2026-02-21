<script setup lang="ts">
import { computed, onBeforeUnmount, ref } from 'vue'

import type { TrustedTokenCreateResponse, TrustedTokenItem } from '@/types/workers'
import { writeTextToClipboard } from '@/utils/clipboard'

const props = defineProps<{
  tokens: TrustedTokenItem[]
  creatingToken: boolean
  deletingTokenId: string
  deleteButtonText: (tokenID: string) => string
  createToken: (payload: { name: string }) => Promise<TrustedTokenCreateResponse>
  formatDateTime: (value: string) => string
}>()

const emit = defineEmits<{
  deleteToken: [tokenID: string]
}>()

const collapsed = ref(true)
const showCreateModal = ref(false)
const nameInput = ref('')
const modalError = ref('')
const createdToken = ref<TrustedTokenCreateResponse | null>(null)
const copyingCreatedToken = ref(false)
const copiedCreatedToken = ref(false)
const copyFailed = ref(false)
const expandedUsageKey = ref('')
const copyingUsageKey = ref('')
const copiedUsageKey = ref('')
const copyUsageFailedKey = ref('')

let createdTokenCopyFeedbackTimer: ReturnType<typeof setTimeout> | null = null
let usageCopyFeedbackTimer: ReturnType<typeof setTimeout> | null = null

type TokenUsageSnippet = {
  key: 'claude-code' | 'http-header' | 'mcp-json'
  label: string
  kind: 'Command' | 'Parameter'
  value: string
}

const createdTokenCopyButtonText = computed(() => {
  if (copyingCreatedToken.value) {
    return 'Copying...'
  }
  if (copiedCreatedToken.value) {
    return 'Copied'
  }
  if (copyFailed.value) {
    return 'Copy Failed'
  }
  return 'Copy Token'
})

const tokenUsageSnippets = computed<TokenUsageSnippet[]>(() => {
  const tokenValue = createdToken.value?.token?.trim() ?? ''
  if (!tokenValue) {
    return []
  }

  const consoleOrigin =
    typeof window !== 'undefined' && window.location?.origin
      ? window.location.origin
      : 'http://127.0.0.1:8089'
  const mcpURL = new URL('/mcp', consoleOrigin).toString()
  const tokenHeader = `X-Onlyboxes-Token: ${tokenValue}`

  return [
    {
      key: 'claude-code',
      label: 'claude code',
      kind: 'Command',
      value: `claude mcp add --transport http onlyboxes "${mcpURL}" --header "${tokenHeader}"`,
    },
    {
      key: 'http-header',
      label: 'http header',
      kind: 'Parameter',
      value: tokenHeader,
    },
    {
      key: 'mcp-json',
      label: 'mcp json',
      kind: 'Parameter',
      value: JSON.stringify(
        {
          mcpServers: {
            onlyboxes: {
              url: mcpURL,
              headers: {
                'X-Onlyboxes-Token': tokenValue,
              },
            },
          },
        },
        null,
        2,
      ),
    },
  ]
})

function usageCopyButtonText(key: string): string {
  if (copyingUsageKey.value === key) {
    return 'Copying...'
  }
  if (copiedUsageKey.value === key) {
    return 'Copied'
  }
  if (copyUsageFailedKey.value === key) {
    return 'Copy Failed'
  }
  return 'Copy'
}

function resetCreatedTokenCopyFeedback(): void {
  if (createdTokenCopyFeedbackTimer) {
    clearTimeout(createdTokenCopyFeedbackTimer)
    createdTokenCopyFeedbackTimer = null
  }
  copyingCreatedToken.value = false
  copiedCreatedToken.value = false
  copyFailed.value = false
}

function resetUsageCopyFeedback(): void {
  if (usageCopyFeedbackTimer) {
    clearTimeout(usageCopyFeedbackTimer)
    usageCopyFeedbackTimer = null
  }
  copyingUsageKey.value = ''
  copiedUsageKey.value = ''
  copyUsageFailedKey.value = ''
}

function clearSensitiveToken(): void {
  createdToken.value = null
  expandedUsageKey.value = ''
  resetCreatedTokenCopyFeedback()
  resetUsageCopyFeedback()
}

function openCreateModal(): void {
  showCreateModal.value = true
  modalError.value = ''
  nameInput.value = ''
  clearSensitiveToken()
}

function closeCreateModal(): void {
  showCreateModal.value = false
  modalError.value = ''
  nameInput.value = ''
  clearSensitiveToken()
}

function scheduleCreatedTokenCopyFeedbackReset(): void {
  if (createdTokenCopyFeedbackTimer) {
    clearTimeout(createdTokenCopyFeedbackTimer)
  }
  createdTokenCopyFeedbackTimer = setTimeout(() => {
    copiedCreatedToken.value = false
    copyFailed.value = false
    createdTokenCopyFeedbackTimer = null
  }, 1500)
}

function scheduleUsageCopyFeedbackReset(): void {
  if (usageCopyFeedbackTimer) {
    clearTimeout(usageCopyFeedbackTimer)
  }
  usageCopyFeedbackTimer = setTimeout(() => {
    copiedUsageKey.value = ''
    copyUsageFailedKey.value = ''
    usageCopyFeedbackTimer = null
  }, 1500)
}

function toggleUsageSnippet(key: string): void {
  expandedUsageKey.value = expandedUsageKey.value === key ? '' : key
}

async function submitCreateToken(): Promise<void> {
  if (props.creatingToken) {
    return
  }

  const name = nameInput.value.trim()
  if (!name) {
    modalError.value = 'name is required'
    return
  }

  modalError.value = ''

  try {
    const payload = await props.createToken({ name })
    const tokenValue = payload.token.trim()
    if (!tokenValue) {
      throw new Error('API returned empty token value.')
    }
    createdToken.value = {
      ...payload,
      token: tokenValue,
    }
    expandedUsageKey.value = ''
    resetUsageCopyFeedback()
  } catch (error) {
    modalError.value = error instanceof Error ? error.message : 'Failed to create trusted token.'
  }
}

async function copyCreatedToken(): Promise<void> {
  const tokenValue = createdToken.value?.token?.trim() ?? ''
  if (!tokenValue || copyingCreatedToken.value) {
    return
  }

  resetCreatedTokenCopyFeedback()
  copyingCreatedToken.value = true
  try {
    await writeTextToClipboard(tokenValue, {
      fallbackErrorMessage: 'Failed to copy token.',
    })
    copiedCreatedToken.value = true
    scheduleCreatedTokenCopyFeedbackReset()
  } catch {
    copyFailed.value = true
    scheduleCreatedTokenCopyFeedbackReset()
  } finally {
    copyingCreatedToken.value = false
  }
}

async function copyUsageSnippet(key: string, value: string): Promise<void> {
  const trimmed = value.trim()
  if (!trimmed || copyingUsageKey.value === key) {
    return
  }

  resetUsageCopyFeedback()
  copyingUsageKey.value = key
  try {
    await writeTextToClipboard(trimmed, {
      fallbackErrorMessage: 'Failed to copy template.',
    })
    copiedUsageKey.value = key
    scheduleUsageCopyFeedbackReset()
  } catch {
    copyUsageFailedKey.value = key
    scheduleUsageCopyFeedbackReset()
  } finally {
    if (copyingUsageKey.value === key) {
      copyingUsageKey.value = ''
    }
  }
}

onBeforeUnmount(() => {
  clearSensitiveToken()
})
</script>

<template>
  <section class="token-panel">
    <div class="token-panel-header">
      <div class="token-title-block">
        <h2>Trusted Tokens</h2>
        <p>Total: {{ tokens.length }}</p>
      </div>
      <div class="token-header-actions">
        <button type="button" class="primary-btn small" @click="openCreateModal">New Token</button>
        <button type="button" class="ghost-btn small" @click="collapsed = !collapsed">
          {{ collapsed ? 'Expand' : 'Collapse' }}
        </button>
      </div>
    </div>

    <transition name="expand">
      <div v-show="!collapsed" class="collapsible-content">
        <p v-if="tokens.length === 0" class="empty-hint">
          未配置，MCP 与受保护 HTTP 端点当前全部拒绝。
        </p>

        <ul v-else class="token-list">
          <li v-for="item in tokens" :key="item.id" class="token-item">
            <div class="token-summary">
              <p class="token-name">{{ item.name }}</p>
              <p class="token-meta">
                <span class="token-meta-label">ID</span>
                <code>{{ item.id }}</code>
              </p>
              <p class="token-meta">
                <span class="token-meta-label">Masked</span>
                <code>{{ item.token_masked }}</code>
              </p>
              <p class="token-meta">
                <span class="token-meta-label">Created</span>
                <span>{{ formatDateTime(item.created_at) }}</span>
              </p>
            </div>

            <div class="token-actions">
              <button
                type="button"
                class="ghost-btn small danger"
                :disabled="deletingTokenId === item.id"
                @click="emit('deleteToken', item.id)"
              >
                {{ deleteButtonText(item.id) }}
              </button>
            </div>
          </li>
        </ul>
      </div>
    </transition>
  </section>

  <div v-if="showCreateModal" class="token-modal-backdrop" @click.self="closeCreateModal">
    <div
      class="token-modal"
      role="dialog"
      aria-modal="true"
      aria-labelledby="trusted-token-dialog-title"
    >
      <div class="token-modal-header">
        <h3 id="trusted-token-dialog-title">
          {{ createdToken ? 'Token Created' : 'New Trusted Token' }}
        </h3>
        <button type="button" class="ghost-btn small" @click="closeCreateModal">
          {{ createdToken ? 'Done' : 'Cancel' }}
        </button>
      </div>

      <div class="token-modal-content-wrapper">
        <template v-if="!createdToken">
          <p class="token-modal-copy">创建后会展示一次明文 token，关闭后不可再次查看。</p>
          <form class="token-modal-form" @submit.prevent="submitCreateToken">
            <label class="token-field">
              <span>Name</span>
              <input
                v-model="nameInput"
                type="text"
                maxlength="64"
                required
                placeholder="ci-prod"
              />
            </label>

            <p v-if="modalError" class="token-modal-error">{{ modalError }}</p>

            <div class="token-modal-actions">
              <button
                type="button"
                class="ghost-btn small"
                :disabled="creatingToken"
                @click="closeCreateModal"
              >
                Cancel
              </button>
              <button
                type="submit"
                class="primary-btn small"
                :disabled="creatingToken || nameInput.trim() === ''"
              >
                {{ creatingToken ? 'Creating...' : 'Create Token' }}
              </button>
            </div>
          </form>
        </template>

        <template v-else>
          <p class="token-modal-copy">这是唯一一次展示明文 token，请立即复制并安全保存。</p>
          <code class="token-plain-value">{{ createdToken.token }}</code>
          <div class="token-result-meta">
            <p><span>Name</span>{{ createdToken.name }}</p>
            <p><span>ID</span>{{ createdToken.id }}</p>
            <p><span>Masked</span>{{ createdToken.token_masked }}</p>
          </div>

          <section class="token-usage-guide">
            <p class="token-usage-title">Quick Setup</p>
            <ul class="token-usage-list">
              <li
                v-for="snippet in tokenUsageSnippets"
                :key="snippet.key"
                class="token-usage-item"
              >
                <button
                  type="button"
                  class="token-usage-trigger"
                  @click="toggleUsageSnippet(snippet.key)"
                >
                  <span class="token-usage-label-row">
                    <span class="token-usage-label">{{ snippet.label }}</span>
                    <span class="token-usage-kind">{{ snippet.kind }}</span>
                  </span>
                  <span class="token-usage-toggle">
                    {{ expandedUsageKey === snippet.key ? 'Collapse' : 'Expand' }}
                  </span>
                </button>

                <transition name="expand">
                  <div v-show="expandedUsageKey === snippet.key" class="token-usage-body">
                    <code class="token-usage-value">{{ snippet.value }}</code>
                    <div class="token-usage-actions">
                      <button
                        type="button"
                        class="ghost-btn small"
                        :disabled="copyingUsageKey === snippet.key"
                        @click="copyUsageSnippet(snippet.key, snippet.value)"
                      >
                        {{ usageCopyButtonText(snippet.key) }}
                      </button>
                    </div>
                  </div>
                </transition>
              </li>
            </ul>
          </section>

          <div class="token-modal-actions">
            <button
              type="button"
              class="ghost-btn small"
              :disabled="copyingCreatedToken"
              @click="copyCreatedToken"
            >
              {{ createdTokenCopyButtonText }}
            </button>
            <button type="button" class="primary-btn small" @click="closeCreateModal">Done</button>
          </div>
        </template>
      </div>
    </div>
  </div>
</template>

<style scoped>
.token-panel {
  border: 1px solid var(--stroke);
  border-radius: var(--radius-lg);
  background: var(--surface);
  box-shadow: var(--shadow);
  padding: 24px;
  animation: rise-in 540ms ease-out;
}

.token-panel-header {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 16px;
}

.token-title-block h2 {
  margin: 0;
  font-size: 18px;
  font-weight: 600;
}

.token-title-block p {
  margin: 4px 0 0;
  color: var(--text-secondary);
  font-size: 14px;
}

.token-header-actions {
  display: flex;
  gap: 12px;
}

.empty-hint {
  margin: 0;
  color: var(--text-secondary);
  font-size: 14px;
  background: var(--surface-soft);
  padding: 12px 16px;
  border-radius: var(--radius);
  border: 1px dashed var(--stroke);
}

.token-list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: grid;
  gap: 12px;
}

.token-item {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 16px;
  border: 1px solid var(--stroke);
  background: var(--surface);
  border-radius: var(--radius-lg);
  padding: 16px 20px;
  transition:
    box-shadow 0.2s ease,
    border-color 0.2s ease;
}

.token-item:hover {
  border-color: var(--stroke-hover);
  box-shadow: var(--shadow-hover);
}

.token-summary {
  min-width: 0;
  display: grid;
  gap: 8px;
}

.token-name {
  margin: 0 0 4px;
  font-size: 15px;
  font-weight: 600;
  color: var(--text-primary);
}

.token-meta {
  margin: 0;
  display: flex;
  align-items: center;
  gap: 12px;
  color: var(--text-primary);
  font-size: 13px;
}

.token-meta-label {
  width: 64px;
  color: var(--text-secondary);
  font-size: 13px;
  font-weight: 500;
}

.token-meta code {
  font-family: 'JetBrains Mono', monospace;
  background: var(--surface-soft);
  border: 1px solid var(--stroke);
  border-radius: var(--radius);
  padding: 2px 6px;
  font-size: 12px;
  word-break: break-all;
  white-space: pre-wrap;
}

.token-meta span:not(.token-meta-label) {
  color: var(--text-secondary);
}

.collapsible-content {
  overflow: hidden;
  padding-top: 20px;
}

.expand-enter-active,
.expand-leave-active {
  transition: all 0.3s cubic-bezier(0.4, 0, 0.2, 1);
  max-height: 1000px;
  opacity: 1;
}

.expand-enter-from,
.expand-leave-to {
  max-height: 0;
  opacity: 0;
}

.token-modal-backdrop {
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

.token-modal {
  width: min(560px, 100%);
  border-radius: var(--radius-lg);
  border: 1px solid var(--stroke);
  background: var(--surface);
  box-shadow: var(--shadow-modal);
  display: flex;
  flex-direction: column;
}

.token-modal-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 20px 24px;
  border-bottom: 1px solid var(--stroke);
}

.token-modal-header h3 {
  margin: 0;
  font-size: 20px;
  font-weight: 600;
}

.token-modal-content-wrapper {
  padding: 24px;
  display: grid;
  gap: 20px;
}

.token-modal-copy {
  margin: 0;
  color: var(--text-secondary);
  font-size: 14px;
  line-height: 1.5;
}

.token-modal-form {
  display: grid;
  gap: 16px;
}

.token-field {
  display: grid;
  gap: 8px;
}

.token-field span {
  color: var(--text-primary);
  font-size: 14px;
  font-weight: 500;
}

.token-field input {
  border: 1px solid var(--stroke);
  border-radius: var(--radius);
  padding: 10px 12px;
  font-size: 14px;
  font-family: inherit;
  transition:
    border-color 0.2s ease,
    box-shadow 0.2s ease;
}

.token-field input:focus {
  outline: none;
  border-color: var(--text-secondary);
  box-shadow: 0 0 0 1px var(--text-secondary);
}

.token-modal-error {
  margin: 0;
  border: 1px solid #fca5a5;
  border-radius: var(--radius);
  background: #fef2f2;
  color: #e00;
  padding: 10px 12px;
  font-size: 14px;
}

.token-modal-actions {
  display: flex;
  justify-content: flex-end;
  gap: 12px;
  padding-top: 20px;
}

.token-plain-value {
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

.token-result-meta {
  display: grid;
  gap: 12px;
}

.token-result-meta p {
  margin: 0;
  display: flex;
  align-items: flex-start;
  gap: 12px;
  font-size: 14px;
  word-break: break-all;
}

.token-result-meta span {
  flex-shrink: 0;
  width: 64px;
  color: var(--text-secondary);
  font-size: 13px;
  font-weight: 500;
}

.token-usage-guide {
  display: grid;
  gap: 12px;
}

.token-usage-title {
  margin: 0;
  color: var(--text-primary);
  font-size: 14px;
  font-weight: 600;
}

.token-usage-list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: grid;
  gap: 10px;
}

.token-usage-item {
  border: 1px solid var(--stroke);
  border-radius: var(--radius);
  background: var(--surface-soft);
  overflow: hidden;
}

.token-usage-trigger {
  width: 100%;
  border: 0;
  background: transparent;
  padding: 10px 12px;
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
}

.token-usage-trigger:not(:disabled):hover {
  background: #efefef;
}

.token-usage-label-row {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  min-width: 0;
}

.token-usage-label {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  padding: 4px 8px;
  border-radius: var(--radius);
  border: 1px solid var(--stroke);
  background: var(--surface);
  font-family: 'JetBrains Mono', monospace;
  font-size: 11px;
  color: var(--text-secondary);
  text-transform: lowercase;
}

.token-usage-kind {
  color: var(--text-secondary);
  font-size: 12px;
}

.token-usage-toggle {
  color: var(--text-secondary);
  font-size: 12px;
  font-weight: 500;
}

.token-usage-body {
  border-top: 1px solid var(--stroke);
  padding: 12px;
  display: grid;
  gap: 10px;
}

.token-usage-value {
  display: block;
  border: 1px solid var(--stroke);
  border-radius: var(--radius);
  background: #000;
  color: #fff;
  padding: 12px;
  font-family: 'JetBrains Mono', monospace;
  font-size: 12px;
  line-height: 1.55;
  word-break: break-all;
  white-space: pre-wrap;
}

.token-usage-actions {
  display: flex;
  justify-content: flex-end;
}

@media (max-width: 700px) {
  .token-panel-header {
    flex-direction: column;
  }

  .token-item {
    flex-direction: column;
  }

  .token-modal-actions {
    flex-direction: column-reverse;
  }

  .token-modal-actions button {
    width: 100%;
  }

  .token-usage-trigger {
    align-items: flex-start;
  }
}
</style>
