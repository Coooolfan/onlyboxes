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

const showCreateModal = ref(false)
const nameInput = ref('')
const modalError = ref('')
const createdToken = ref<TrustedTokenCreateResponse | null>(null)
const copyingCreatedToken = ref(false)
const copiedCreatedToken = ref(false)
const copyFailed = ref(false)
const activeUsageKey = ref<TokenUsageSnippet['key']>('claude-code')
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
  const tokenHeader = `Authorization: Bearer ${tokenValue}`

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
                Authorization: `Bearer ${tokenValue}`,
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

const activeUsageSnippet = computed<TokenUsageSnippet | null>(() => {
  const snippets = tokenUsageSnippets.value
  if (snippets.length === 0) {
    return null
  }

  return snippets.find((snippet) => snippet.key === activeUsageKey.value) ?? snippets[0] ?? null
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
  activeUsageKey.value = 'claude-code'
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

function selectUsageSnippet(key: TokenUsageSnippet['key']): void {
  if (activeUsageKey.value === key) {
    return
  }
  activeUsageKey.value = key
  resetUsageCopyFeedback()
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
    activeUsageKey.value = 'claude-code'
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
  <section
    class="token-panel border border-stroke rounded-lg bg-surface shadow-card p-6 animate-[rise-in_540ms_ease-out]"
  >
    <div class="flex items-start justify-between gap-4 max-[700px]:flex-col">
      <div>
        <h2 class="m-0 text-lg font-semibold">Trusted Tokens</h2>
        <p class="mt-1 mb-0 text-secondary text-sm">Total: {{ tokens.length }}</p>
      </div>
      <div class="flex gap-3">
        <button
          type="button"
          class="ui-btn-primary rounded-md px-3 py-1.5 text-[13px] font-medium h-8 inline-flex items-center justify-center border transition-all duration-200 disabled:cursor-not-allowed disabled:opacity-50"
          @click="openCreateModal"
        >
          New Token
        </button>
      </div>
    </div>

    <div class="pt-5">
      <p
        v-if="tokens.length === 0"
        class="ui-empty-state m-0 text-sm px-4 py-3 rounded-default border border-dashed"
      >
        No tokens configured. All MCP and protected HTTP endpoints are currently rejected.
      </p>

      <ul v-else class="list-none m-0 p-0 grid gap-3">
        <li
          v-for="item in tokens"
          :key="item.id"
          class="flex items-start justify-between gap-4 border border-stroke bg-surface rounded-lg px-5 py-4 transition-[box-shadow,border-color] duration-200 hover:border-stroke-hover hover:shadow-card-hover max-[700px]:flex-col"
        >
          <div class="min-w-0 grid gap-2">
            <p class="m-0 mb-1 text-[15px] font-semibold text-primary">{{ item.name }}</p>
            <p class="m-0 flex items-center gap-3 text-primary text-[13px]">
              <span class="w-16 text-secondary text-[13px] font-medium">ID</span>
              <code
                class="ui-code-inline font-mono border rounded-default px-1.5 py-0.5 text-xs break-all whitespace-pre-wrap"
                >{{ item.id }}</code
              >
            </p>
            <p class="m-0 flex items-center gap-3 text-primary text-[13px]">
              <span class="w-16 text-secondary text-[13px] font-medium">Masked</span>
              <code
                class="ui-code-inline font-mono border rounded-default px-1.5 py-0.5 text-xs break-all whitespace-pre-wrap"
                >{{ item.token_masked }}</code
              >
            </p>
            <p class="m-0 flex items-center gap-3 text-primary text-[13px]">
              <span class="w-16 text-secondary text-[13px] font-medium">Created</span>
              <span class="text-secondary">{{ formatDateTime(item.created_at) }}</span>
            </p>
          </div>

          <div class="token-actions">
            <button
              type="button"
              class="ui-btn-danger rounded-md px-3 py-1.5 text-[13px] font-medium h-8 inline-flex items-center justify-center border transition-all duration-200 disabled:cursor-not-allowed disabled:opacity-50"
              :disabled="deletingTokenId === item.id"
              @click="emit('deleteToken', item.id)"
            >
              {{ deleteButtonText(item.id) }}
            </button>
          </div>
        </li>
      </ul>
    </div>
  </section>

  <Teleport to="body">
    <div
      v-if="showCreateModal"
      class="ui-modal-overlay fixed inset-0 z-1000 flex items-center justify-center p-6"
      @click.self="closeCreateModal"
    >
      <div
        class="token-modal w-[min(560px,100%)] rounded-lg border border-stroke bg-surface shadow-modal flex flex-col"
        role="dialog"
        aria-modal="true"
        aria-labelledby="trusted-token-dialog-title"
      >
        <div class="flex items-center justify-between px-6 py-5 border-b border-stroke">
          <h3 id="trusted-token-dialog-title" class="m-0 text-xl font-semibold">
            {{ createdToken ? 'Token Created' : 'New Trusted Token' }}
          </h3>
        </div>

        <div class="p-6 grid gap-5">
          <template v-if="!createdToken">
            <p class="m-0 text-secondary text-sm leading-normal">
              The plaintext token is shown only once after creation and cannot be viewed again after
              closing this dialog.
            </p>
            <form class="token-modal-form grid gap-4" @submit.prevent="submitCreateToken">
              <label class="grid gap-2">
                <span class="text-primary text-sm font-medium">Name</span>
                <input
                  v-model="nameInput"
                  type="text"
                  maxlength="64"
                  required
                  placeholder="ci-prod"
                  class="ui-input border rounded-default px-3 py-2.5 text-sm font-[inherit]"
                />
              </label>

              <p
                v-if="modalError"
                class="ui-alert ui-alert-error m-0 rounded-default px-3 py-2.5 text-sm"
              >
                {{ modalError }}
              </p>

              <div
                class="flex justify-end gap-3 pt-5 max-[700px]:flex-col-reverse max-[700px]:[&>button]:w-full"
              >
                <button
                  type="button"
                  class="ui-btn-secondary rounded-md px-3 py-1.5 text-[13px] font-medium h-8 inline-flex items-center justify-center border transition-all duration-200 disabled:cursor-not-allowed disabled:opacity-50"
                  :disabled="creatingToken"
                  @click="closeCreateModal"
                >
                  Cancel
                </button>
                <button
                  type="submit"
                  class="ui-btn-primary rounded-md px-3 py-1.5 text-[13px] font-medium h-8 inline-flex items-center justify-center border transition-all duration-200 disabled:cursor-not-allowed disabled:opacity-50"
                  :disabled="creatingToken || nameInput.trim() === ''"
                >
                  {{ creatingToken ? 'Creating...' : 'Create Token' }}
                </button>
              </div>
            </form>
          </template>

          <template v-else>
            <p class="m-0 text-secondary text-sm leading-normal">
              This is the only time the plaintext token is shown. Copy and store it securely now.
            </p>
            <code
              class="ui-code-block block border rounded-default p-4 font-mono text-[13px] leading-[1.6] break-all whitespace-pre-wrap"
              >{{ createdToken.token }}</code
            >
            <div class="grid gap-3">
              <p class="m-0 flex items-start gap-3 text-sm break-all">
                <span class="shrink-0 w-16 text-secondary text-[13px] font-medium">Name</span
                >{{ createdToken.name }}
              </p>
              <p class="m-0 flex items-start gap-3 text-sm break-all">
                <span class="shrink-0 w-16 text-secondary text-[13px] font-medium">ID</span
                >{{ createdToken.id }}
              </p>
              <p class="m-0 flex items-start gap-3 text-sm break-all">
                <span class="shrink-0 w-16 text-secondary text-[13px] font-medium">Masked</span
                >{{ createdToken.token_masked }}
              </p>
            </div>

            <section class="grid gap-3">
              <p class="m-0 text-primary text-sm font-semibold">Quick Setup</p>
              <div class="token-usage-tabs grid gap-3">
                <div
                  role="tablist"
                  aria-label="Token quick setup snippets"
                  class="ui-inset-surface grid grid-cols-3 gap-2 rounded-default border p-2 max-[700px]:grid-cols-1"
                >
                  <button
                    v-for="snippet in tokenUsageSnippets"
                    :key="snippet.key"
                    type="button"
                    role="tab"
                    :aria-selected="activeUsageKey === snippet.key"
                    class="rounded-default border px-3 py-2 text-left transition-all duration-200"
                    :class="activeUsageKey === snippet.key ? 'ui-tab-active' : 'ui-tab-inactive'"
                    @click="selectUsageSnippet(snippet.key)"
                  >
                    <span class="block font-mono text-[11px] lowercase">{{ snippet.label }}</span>
                    <span class="block text-xs mt-1">{{ snippet.kind }}</span>
                  </button>
                </div>

                <div
                  v-if="activeUsageSnippet"
                  class="ui-inset-surface token-usage-item border rounded-default p-3 grid gap-2.5"
                >
                  <code
                    class="ui-code-block token-usage-value block border rounded-default p-3 font-mono text-xs leading-[1.55] break-all whitespace-pre-wrap"
                    >{{ activeUsageSnippet.value }}</code
                  >
                  <div class="flex justify-end">
                    <button
                      type="button"
                      class="ui-btn-secondary rounded-md px-3 py-1.5 text-[13px] font-medium h-8 inline-flex items-center justify-center border transition-all duration-200 disabled:cursor-not-allowed disabled:opacity-50"
                      :disabled="copyingUsageKey === activeUsageSnippet.key"
                      @click="copyUsageSnippet(activeUsageSnippet.key, activeUsageSnippet.value)"
                    >
                      {{ usageCopyButtonText(activeUsageSnippet.key) }}
                    </button>
                  </div>
                </div>
              </div>
            </section>

            <div
              class="flex justify-end gap-3 pt-5 max-[700px]:flex-col-reverse max-[700px]:[&>button]:w-full"
            >
              <button
                type="button"
                class="ui-btn-secondary rounded-md px-3 py-1.5 text-[13px] font-medium h-8 inline-flex items-center justify-center border transition-all duration-200 disabled:cursor-not-allowed disabled:opacity-50"
                :disabled="copyingCreatedToken"
                @click="copyCreatedToken"
              >
                {{ createdTokenCopyButtonText }}
              </button>
              <button
                type="button"
                class="ui-btn-primary rounded-md px-3 py-1.5 text-[13px] font-medium h-8 inline-flex items-center justify-center border transition-all duration-200 disabled:cursor-not-allowed disabled:opacity-50"
                @click="closeCreateModal"
              >
                Done
              </button>
            </div>
          </template>
        </div>
      </div>
    </div>
  </Teleport>
</template>
