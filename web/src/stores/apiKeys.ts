import { ref } from 'vue'
import { defineStore } from 'pinia'

import { requestConfirm } from '@/composables/useConfirm'
import { isUnauthorizedError } from '@/services/http'
import { createAPIKeyAPI, deleteAPIKeyAPI, fetchAPIKeysAPI } from '@/services/workers.api'
import { redirectToLogin } from '@/stores/auth-redirect'
import { createRequestGuard, isAbortError, toErrorMessage } from '@/utils/async'
import type { APIKeyCreateResponse, APIKeyItem } from '@/types/workers'

export const useAPIKeysStore = defineStore('apiKeys', () => {
  const loading = ref(false)
  const errorMessage = ref('')
  const apiKeys = ref<APIKeyItem[]>([])
  const creatingAPIKey = ref(false)
  const deletingAPIKeyID = ref('')

  const requests = createRequestGuard()

  function reset(): void {
    apiKeys.value = []
    loading.value = false
    errorMessage.value = ''
    creatingAPIKey.value = false
    deletingAPIKeyID.value = ''
  }

  async function handleUnauthorized(): Promise<void> {
    await redirectToLogin(reset)
  }

  async function loadAPIKeys(): Promise<void> {
    const token = requests.begin()
    loading.value = true
    errorMessage.value = ''

    try {
      const payload = await fetchAPIKeysAPI(token.signal)
      if (token.isStale()) {
        return
      }
      apiKeys.value = payload.items ?? []
    } catch (error) {
      if (isAbortError(error) || token.isStale()) {
        return
      }
      if (isUnauthorizedError(error)) {
        await handleUnauthorized()
        return
      }
      errorMessage.value = toErrorMessage(error, 'Failed to load API keys.')
    } finally {
      if (token.release()) {
        loading.value = false
      }
    }
  }

  async function createAPIKey(name: string): Promise<APIKeyCreateResponse> {
    if (creatingAPIKey.value) {
      throw new Error('API key creation already in progress.')
    }

    const trimmedName = name.trim()
    if (!trimmedName) {
      throw new Error('name is required')
    }

    creatingAPIKey.value = true
    errorMessage.value = ''
    try {
      const created = await createAPIKeyAPI({ name: trimmedName })
      const keyValue = created.key.trim()
      if (!keyValue) {
        throw new Error('API returned empty api key value.')
      }
      await loadAPIKeys()
      return { ...created, key: keyValue }
    } catch (error) {
      if (isUnauthorizedError(error)) {
        await handleUnauthorized()
      }
      throw error instanceof Error ? error : new Error('Failed to create API key.')
    } finally {
      creatingAPIKey.value = false
    }
  }

  async function deleteAPIKey(apiKeyID: string): Promise<void> {
    if (!apiKeyID || deletingAPIKeyID.value === apiKeyID) {
      return
    }

    const target = apiKeys.value.find((item) => item.id === apiKeyID)
    const confirmed = await requestConfirm({
      title: 'Delete API Key',
      message: 'Automation using this key stops working immediately.',
      detail: target ? `${target.name} (${apiKeyID})` : apiKeyID,
      confirmLabel: 'Delete API Key',
      destructive: true,
    })
    if (!confirmed) {
      return
    }

    deletingAPIKeyID.value = apiKeyID
    errorMessage.value = ''
    try {
      await deleteAPIKeyAPI(apiKeyID)
      await loadAPIKeys()
    } catch (error) {
      if (isUnauthorizedError(error)) {
        await handleUnauthorized()
        return
      }
      errorMessage.value = toErrorMessage(error, 'Failed to delete API key.')
    } finally {
      if (deletingAPIKeyID.value === apiKeyID) {
        deletingAPIKeyID.value = ''
      }
    }
  }

  function teardown(): void {
    requests.abort()
    loading.value = false
  }

  return {
    apiKeys,
    loading,
    creatingAPIKey,
    deletingAPIKeyID,
    errorMessage,
    loadAPIKeys,
    createAPIKey,
    deleteAPIKey,
    teardown,
    reset,
  }
})
