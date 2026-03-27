<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'

import { useAPIKeysStore } from '@/stores/apiKeys'
import { writeTextToClipboard } from '@/utils/clipboard'
import type { APIKeyCreateResponse } from '@/types/workers'

const emit = defineEmits<{
  close: []
}>()

const apiKeysStore = useAPIKeysStore()

const stage = ref<'list' | 'create-form' | 'create-success'>('list')
const nameInput = ref('')
const modalError = ref('')
const createdAPIKey = ref<APIKeyCreateResponse | null>(null)
const copyingCreatedKey = ref(false)
const copiedCreatedKey = ref(false)
const copyFailed = ref(false)

let copyFeedbackTimer: ReturnType<typeof setTimeout> | null = null

const copyButtonText = computed(() => {
  if (copyingCreatedKey.value) {
    return 'Copying...'
  }
  if (copiedCreatedKey.value) {
    return 'Copied'
  }
  if (copyFailed.value) {
    return 'Copy Failed'
  }
  return 'Copy Key'
})

function resetCopyFeedback(): void {
  if (copyFeedbackTimer) {
    clearTimeout(copyFeedbackTimer)
    copyFeedbackTimer = null
  }
  copyingCreatedKey.value = false
  copiedCreatedKey.value = false
  copyFailed.value = false
}

function clearSensitiveKey(): void {
  createdAPIKey.value = null
  resetCopyFeedback()
}

function openCreateForm(): void {
  stage.value = 'create-form'
  nameInput.value = ''
  modalError.value = ''
  clearSensitiveKey()
}

function returnToList(): void {
  stage.value = 'list'
  nameInput.value = ''
  modalError.value = ''
  clearSensitiveKey()
}

function closeModal(): void {
  emit('close')
}

function scheduleCopyFeedbackReset(): void {
  if (copyFeedbackTimer) {
    clearTimeout(copyFeedbackTimer)
  }
  copyFeedbackTimer = setTimeout(() => {
    copiedCreatedKey.value = false
    copyFailed.value = false
    copyFeedbackTimer = null
  }, 1500)
}

async function submitCreateAPIKey(): Promise<void> {
  if (apiKeysStore.creatingAPIKey) {
    return
  }

  const name = nameInput.value.trim()
  if (!name) {
    modalError.value = 'name is required'
    return
  }

  modalError.value = ''
  try {
    const created = await apiKeysStore.createAPIKey(name)
    createdAPIKey.value = created
    stage.value = 'create-success'
  } catch (error) {
    modalError.value = error instanceof Error ? error.message : 'Failed to create API key.'
  }
}

async function copyCreatedAPIKey(): Promise<void> {
  const keyValue = createdAPIKey.value?.key?.trim() ?? ''
  if (!keyValue || copyingCreatedKey.value) {
    return
  }

  resetCopyFeedback()
  copyingCreatedKey.value = true
  try {
    await writeTextToClipboard(keyValue, {
      fallbackErrorMessage: 'Failed to copy API key.',
    })
    copiedCreatedKey.value = true
    scheduleCopyFeedbackReset()
  } catch {
    copyFailed.value = true
    scheduleCopyFeedbackReset()
  } finally {
    copyingCreatedKey.value = false
  }
}

async function deleteAPIKey(apiKeyID: string): Promise<void> {
  await apiKeysStore.deleteAPIKey(apiKeyID)
}

onMounted(async () => {
  await apiKeysStore.loadAPIKeys()
})

onBeforeUnmount(() => {
  clearSensitiveKey()
  apiKeysStore.teardown()
})
</script>

<template>
  <Teleport to="body">
    <div class="ui-modal-overlay fixed inset-0 z-1000 flex items-center justify-center p-6">
      <div
        class="api-keys-modal w-[min(720px,100%)] rounded-lg border border-stroke bg-surface shadow-modal flex flex-col"
        role="dialog"
        aria-modal="true"
        aria-labelledby="api-keys-modal-title"
      >
        <div class="flex items-center justify-between px-6 py-5 border-b border-stroke">
          <div>
            <h3 id="api-keys-modal-title" class="m-0 text-xl font-semibold">API Keys</h3>
            <p class="m-0 mt-1 text-sm text-secondary">Manage dashboard Bearer keys.</p>
          </div>
        </div>

        <div class="p-6 grid gap-5">
          <p
            v-if="apiKeysStore.errorMessage && stage !== 'create-form'"
            class="ui-alert ui-alert-error m-0 rounded-default px-3 py-2.5 text-sm"
          >
            {{ apiKeysStore.errorMessage }}
          </p>

          <template v-if="stage === 'list'">
            <div class="flex items-start justify-between gap-4 max-[700px]:flex-col">
              <div>
                <p class="m-0 text-sm text-secondary">Total: {{ apiKeysStore.apiKeys.length }}</p>
              </div>
              <button
                type="button"
                class="ui-btn-primary rounded-md px-3 py-1.5 text-[13px] font-medium h-8 inline-flex items-center justify-center border transition-all duration-200 disabled:cursor-not-allowed disabled:opacity-50"
                :disabled="apiKeysStore.loading"
                @click="openCreateForm"
              >
                New API Key
              </button>
            </div>

            <p
              v-if="apiKeysStore.loading"
              class="m-0 rounded-default border border-dashed border-stroke px-4 py-3 text-sm text-secondary"
            >
              Loading API keys...
            </p>

            <p
              v-else-if="apiKeysStore.apiKeys.length === 0"
              class="m-0 rounded-default border border-dashed border-stroke px-4 py-3 text-sm text-secondary"
            >
              No API keys created yet.
            </p>

            <ul v-else class="m-0 list-none grid gap-3 p-0">
              <li
                v-for="item in apiKeysStore.apiKeys"
                :key="item.id"
                class="rounded-lg border border-stroke bg-surface-soft/40 px-4 py-4"
              >
                <div class="flex items-start justify-between gap-4 max-[700px]:flex-col">
                  <div class="grid gap-2 min-w-0">
                    <p class="m-0 text-[15px] font-semibold text-primary">{{ item.name }}</p>
                    <p class="m-0 text-sm text-secondary break-all">
                      <span class="font-medium text-primary">Key</span>
                      {{ item.key_masked }}
                    </p>
                    <p class="m-0 text-sm text-secondary">
                      <span class="font-medium text-primary">Created</span>
                      {{ apiKeysStore.formatDateTime(item.created_at) }}
                    </p>
                  </div>

                  <div class="api-key-actions">
                    <button
                      type="button"
                      class="ui-btn-secondary rounded-md px-3 py-1.5 text-[13px] font-medium h-8 inline-flex items-center justify-center border transition-all duration-200 disabled:cursor-not-allowed disabled:opacity-50"
                      :disabled="apiKeysStore.deletingAPIKeyID === item.id"
                      @click="deleteAPIKey(item.id)"
                    >
                      {{ apiKeysStore.apiKeyDeleteButtonText(item.id) }}
                    </button>
                  </div>
                </div>
              </li>
            </ul>
          </template>

          <template v-else-if="stage === 'create-form'">
            <p class="m-0 text-sm text-secondary leading-normal">
              Create a long-lived Bearer token for dashboard automation. The plaintext key is shown
              once.
            </p>

            <form id="create-api-key-form" class="grid gap-4" @submit.prevent="submitCreateAPIKey">
              <label class="grid gap-2">
                <span class="text-primary text-sm font-medium">Name</span>
                <input
                  v-model="nameInput"
                  type="text"
                  maxlength="64"
                  autocomplete="off"
                  required
                  class="ui-input border rounded-default px-3 py-2.5 text-sm font-[inherit]"
                />
              </label>

              <p
                v-if="modalError"
                class="ui-alert ui-alert-error m-0 rounded-default px-3 py-2.5 text-sm"
              >
                {{ modalError }}
              </p>
            </form>
          </template>

          <template v-else>
            <p class="m-0 text-sm text-secondary leading-normal">
              Copy this API key now. It will not be shown again after you close this dialog.
            </p>

            <div class="rounded-default border border-stroke bg-surface-soft/40 px-4 py-4 grid gap-3">
              <p class="m-0 text-sm text-secondary">
                <span class="font-medium text-primary">Name</span>
                {{ createdAPIKey?.name }}
              </p>
              <code
                class="api-key-value block rounded-default border border-stroke bg-surface px-3 py-3 text-xs leading-relaxed break-all whitespace-pre-wrap"
                >{{ createdAPIKey?.key }}</code
              >
              <div class="flex justify-end">
                <button
                  type="button"
                  class="ui-btn-primary rounded-md px-3 py-1.5 text-[13px] font-medium h-8 inline-flex items-center justify-center border transition-all duration-200 disabled:cursor-not-allowed disabled:opacity-50"
                  :disabled="copyingCreatedKey"
                  @click="copyCreatedAPIKey"
                >
                  {{ copyButtonText }}
                </button>
              </div>
            </div>
          </template>
        </div>

        <div
          class="flex justify-end gap-3 px-6 py-5 border-t border-stroke rounded-b-lg max-[600px]:flex-col-reverse max-[600px]:[&>button]:w-full"
        >
          <template v-if="stage === 'list'">
            <button
              type="button"
              class="ui-btn-secondary rounded-md px-3 py-1.5 text-[13px] font-medium h-8 inline-flex items-center justify-center border transition-all duration-200"
              @click="closeModal"
            >
              Close
            </button>
          </template>

          <template v-else-if="stage === 'create-form'">
            <button
              type="button"
              class="ui-btn-secondary rounded-md px-3 py-1.5 text-[13px] font-medium h-8 inline-flex items-center justify-center border transition-all duration-200 disabled:cursor-not-allowed disabled:opacity-50"
              :disabled="apiKeysStore.creatingAPIKey"
              @click="returnToList"
            >
              Cancel
            </button>
            <button
              type="submit"
              form="create-api-key-form"
              class="ui-btn-primary rounded-md px-3 py-1.5 text-[13px] font-medium h-8 inline-flex items-center justify-center border transition-all duration-200 disabled:cursor-not-allowed disabled:opacity-50"
              :disabled="apiKeysStore.creatingAPIKey"
            >
              {{ apiKeysStore.creatingAPIKey ? 'Creating...' : 'Create API Key' }}
            </button>
          </template>

          <template v-else>
            <button
              type="button"
              class="ui-btn-primary rounded-md px-3 py-1.5 text-[13px] font-medium h-8 inline-flex items-center justify-center border transition-all duration-200"
              @click="closeModal"
            >
              Close
            </button>
          </template>
        </div>
      </div>
    </div>
  </Teleport>
</template>
