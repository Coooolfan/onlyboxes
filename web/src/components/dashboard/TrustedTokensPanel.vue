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
  <section class="border border-stroke rounded-lg bg-surface shadow-card p-6 animate-[rise-in_540ms_ease-out]">
    <div class="flex items-start justify-between gap-4 max-[700px]:flex-col">
      <div>
        <h2 class="m-0 text-lg font-semibold">Trusted Tokens</h2>
        <p class="mt-1 mb-0 text-secondary text-sm">Total: {{ tokens.length }}</p>
      </div>
      <div class="flex gap-3">
        <button
          type="button"
          class="rounded-md px-3 py-1.5 text-[13px] font-medium h-8 inline-flex items-center justify-center text-white bg-accent border border-accent transition-all duration-200 hover:not-disabled:bg-[#333] hover:not-disabled:border-[#333] disabled:cursor-not-allowed disabled:opacity-50"
          @click="openCreateModal"
        >
          New Token
        </button>
        <button
          type="button"
          class="rounded-md px-3 py-1.5 text-[13px] font-medium h-8 inline-flex items-center justify-center text-primary bg-surface border border-stroke transition-all duration-200 hover:not-disabled:border-stroke-hover hover:not-disabled:bg-surface-soft disabled:cursor-not-allowed disabled:opacity-50"
          @click="collapsed = !collapsed"
        >
          {{ collapsed ? 'Expand' : 'Collapse' }}
        </button>
      </div>
    </div>

    <transition name="expand">
      <div v-show="!collapsed" class="overflow-hidden pt-5">
        <p v-if="tokens.length === 0" class="m-0 text-secondary text-sm bg-surface-soft px-4 py-3 rounded-default border border-dashed border-stroke">
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
                <code class="font-mono bg-surface-soft border border-stroke rounded-default px-1.5 py-0.5 text-xs break-all whitespace-pre-wrap">{{ item.id }}</code>
              </p>
              <p class="m-0 flex items-center gap-3 text-primary text-[13px]">
                <span class="w-16 text-secondary text-[13px] font-medium">Masked</span>
                <code class="font-mono bg-surface-soft border border-stroke rounded-default px-1.5 py-0.5 text-xs break-all whitespace-pre-wrap">{{ item.token_masked }}</code>
              </p>
              <p class="m-0 flex items-center gap-3 text-primary text-[13px]">
                <span class="w-16 text-secondary text-[13px] font-medium">Created</span>
                <span class="text-secondary">{{ formatDateTime(item.created_at) }}</span>
              </p>
            </div>

            <div>
              <button
                type="button"
                class="rounded-md px-3 py-1.5 text-[13px] font-medium h-8 inline-flex items-center justify-center text-offline bg-white border border-[#fca5a5] transition-all duration-200 hover:not-disabled:bg-[#fef2f2] hover:not-disabled:border-[#f87171] disabled:cursor-not-allowed disabled:opacity-50"
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

  <div v-if="showCreateModal" class="fixed inset-0 z-1000 bg-black/40 backdrop-blur-xs flex items-center justify-center p-6" @click.self="closeCreateModal">
    <div
      class="w-[min(560px,100%)] rounded-lg border border-stroke bg-surface shadow-modal flex flex-col"
      role="dialog"
      aria-modal="true"
      aria-labelledby="trusted-token-dialog-title"
    >
      <div class="flex items-center justify-between px-6 py-5 border-b border-stroke">
        <h3 id="trusted-token-dialog-title" class="m-0 text-xl font-semibold">
          {{ createdToken ? 'Token Created' : 'New Trusted Token' }}
        </h3>
        <button
          type="button"
          class="rounded-md px-3 py-1.5 text-[13px] font-medium h-8 inline-flex items-center justify-center text-primary bg-surface border border-stroke transition-all duration-200 hover:not-disabled:border-stroke-hover hover:not-disabled:bg-surface-soft disabled:cursor-not-allowed disabled:opacity-50"
          @click="closeCreateModal"
        >
          {{ createdToken ? 'Done' : 'Cancel' }}
        </button>
      </div>

      <div class="p-6 grid gap-5">
        <template v-if="!createdToken">
          <p class="m-0 text-secondary text-sm leading-normal">The plaintext token is shown only once after creation and cannot be viewed again after closing this dialog.</p>
          <form class="grid gap-4" @submit.prevent="submitCreateToken">
            <label class="grid gap-2">
              <span class="text-primary text-sm font-medium">Name</span>
              <input
                v-model="nameInput"
                type="text"
                maxlength="64"
                required
                placeholder="ci-prod"
                class="border border-stroke rounded-default px-3 py-2.5 text-sm font-[inherit] transition-[border-color,box-shadow] duration-200 outline-none focus:border-secondary focus:shadow-[0_0_0_1px_var(--color-secondary)]"
              />
            </label>

            <p v-if="modalError" class="m-0 border border-[#fca5a5] rounded-default bg-[#fef2f2] text-offline px-3 py-2.5 text-sm">{{ modalError }}</p>

            <div class="flex justify-end gap-3 pt-5 max-[700px]:flex-col-reverse max-[700px]:[&>button]:w-full">
              <button
                type="button"
                class="rounded-md px-3 py-1.5 text-[13px] font-medium h-8 inline-flex items-center justify-center text-primary bg-surface border border-stroke transition-all duration-200 hover:not-disabled:border-stroke-hover hover:not-disabled:bg-surface-soft disabled:cursor-not-allowed disabled:opacity-50"
                :disabled="creatingToken"
                @click="closeCreateModal"
              >
                Cancel
              </button>
              <button
                type="submit"
                class="rounded-md px-3 py-1.5 text-[13px] font-medium h-8 inline-flex items-center justify-center text-white bg-accent border border-accent transition-all duration-200 hover:not-disabled:bg-[#333] hover:not-disabled:border-[#333] disabled:cursor-not-allowed disabled:opacity-50"
                :disabled="creatingToken || nameInput.trim() === ''"
              >
                {{ creatingToken ? 'Creating...' : 'Create Token' }}
              </button>
            </div>
          </form>
        </template>

        <template v-else>
          <p class="m-0 text-secondary text-sm leading-normal">This is the only time the plaintext token is shown. Copy and store it securely now.</p>
          <code class="block border border-stroke rounded-default bg-black text-white p-4 font-mono text-[13px] leading-[1.6] break-all whitespace-pre-wrap">{{ createdToken.token }}</code>
          <div class="grid gap-3">
            <p class="m-0 flex items-start gap-3 text-sm break-all"><span class="shrink-0 w-16 text-secondary text-[13px] font-medium">Name</span>{{ createdToken.name }}</p>
            <p class="m-0 flex items-start gap-3 text-sm break-all"><span class="shrink-0 w-16 text-secondary text-[13px] font-medium">ID</span>{{ createdToken.id }}</p>
            <p class="m-0 flex items-start gap-3 text-sm break-all"><span class="shrink-0 w-16 text-secondary text-[13px] font-medium">Masked</span>{{ createdToken.token_masked }}</p>
          </div>

          <section class="grid gap-3">
            <p class="m-0 text-primary text-sm font-semibold">Quick Setup</p>
            <ul class="list-none m-0 p-0 grid gap-2.5">
              <li
                v-for="snippet in tokenUsageSnippets"
                :key="snippet.key"
                class="border border-stroke rounded-default bg-surface-soft overflow-hidden"
              >
                <button
                  type="button"
                  class="w-full border-0 bg-transparent px-3 py-2.5 flex items-center justify-between gap-3 cursor-pointer transition-all duration-200 hover:bg-[#efefef] max-[700px]:items-start"
                  @click="toggleUsageSnippet(snippet.key)"
                >
                  <span class="inline-flex items-center gap-2 min-w-0">
                    <span class="inline-flex items-center justify-center px-2 py-1 rounded-default border border-stroke bg-surface font-mono text-[11px] text-secondary lowercase">{{ snippet.label }}</span>
                    <span class="text-secondary text-xs">{{ snippet.kind }}</span>
                  </span>
                  <span class="text-secondary text-xs font-medium">
                    {{ expandedUsageKey === snippet.key ? 'Collapse' : 'Expand' }}
                  </span>
                </button>

                <transition name="expand">
                  <div v-show="expandedUsageKey === snippet.key" class="border-t border-stroke p-3 grid gap-2.5">
                    <code class="block border border-stroke rounded-default bg-black text-white p-3 font-mono text-xs leading-[1.55] break-all whitespace-pre-wrap">{{ snippet.value }}</code>
                    <div class="flex justify-end">
                      <button
                        type="button"
                        class="rounded-md px-3 py-1.5 text-[13px] font-medium h-8 inline-flex items-center justify-center text-primary bg-surface border border-stroke transition-all duration-200 hover:not-disabled:border-stroke-hover hover:not-disabled:bg-surface-soft disabled:cursor-not-allowed disabled:opacity-50"
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

          <div class="flex justify-end gap-3 pt-5 max-[700px]:flex-col-reverse max-[700px]:[&>button]:w-full">
            <button
              type="button"
              class="rounded-md px-3 py-1.5 text-[13px] font-medium h-8 inline-flex items-center justify-center text-primary bg-surface border border-stroke transition-all duration-200 hover:not-disabled:border-stroke-hover hover:not-disabled:bg-surface-soft disabled:cursor-not-allowed disabled:opacity-50"
              :disabled="copyingCreatedToken"
              @click="copyCreatedToken"
            >
              {{ createdTokenCopyButtonText }}
            </button>
            <button
              type="button"
              class="rounded-md px-3 py-1.5 text-[13px] font-medium h-8 inline-flex items-center justify-center text-white bg-accent border border-accent transition-all duration-200 hover:not-disabled:bg-[#333] hover:not-disabled:border-[#333] disabled:cursor-not-allowed disabled:opacity-50"
              @click="closeCreateModal"
            >
              Done
            </button>
          </div>
        </template>
      </div>
    </div>
  </div>
</template>

<style scoped>
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
</style>
