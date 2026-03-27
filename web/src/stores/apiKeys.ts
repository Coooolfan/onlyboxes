import { ref } from 'vue'
import { defineStore } from 'pinia'

import { isUnauthorizedError } from '@/services/http'
import { createAPIKeyAPI, deleteAPIKeyAPI, fetchAPIKeysAPI } from '@/services/workers.api'
import { redirectToLogin } from '@/stores/auth-redirect'
import { formatDateTime } from '@/utils/datetime'
import type { APIKeyCreateResponse, APIKeyItem } from '@/types/workers'

function isAbortError(error: unknown): boolean {
  if (typeof DOMException !== 'undefined' && error instanceof DOMException) {
    return error.name === 'AbortError'
  }
  return error instanceof Error && error.name === 'AbortError'
}

export const useAPIKeysStore = defineStore('apiKeys', () => {
  const loading = ref(false)
  const errorMessage = ref('')
  const apiKeys = ref<APIKeyItem[]>([])
  const creatingAPIKey = ref(false)
  const deletingAPIKeyID = ref('')

  let activeController: AbortController | null = null
  let requestSerial = 0

  async function handleUnauthorized(): Promise<void> {
    await redirectToLogin(() => {
      reset()
    })
  }

  function reset(): void {
    apiKeys.value = []
    loading.value = false
    errorMessage.value = ''
    creatingAPIKey.value = false
    deletingAPIKeyID.value = ''
  }

  function apiKeyDeleteButtonText(apiKeyID: string): string {
    if (deletingAPIKeyID.value === apiKeyID) {
      return 'Deleting...'
    }
    return 'Delete'
  }

  async function loadAPIKeys(): Promise<void> {
    const serial = ++requestSerial
    activeController?.abort()
    const controller = new AbortController()
    activeController = controller

    loading.value = true
    errorMessage.value = ''
    try {
      const payload = await fetchAPIKeysAPI(controller.signal)
      if (controller.signal.aborted || serial !== requestSerial) {
        return
      }
      apiKeys.value = payload.items ?? []
    } catch (error) {
      if (isAbortError(error) || serial !== requestSerial) {
        return
      }
      if (isUnauthorizedError(error)) {
        await handleUnauthorized()
        return
      }
      errorMessage.value = error instanceof Error ? error.message : 'Failed to load API keys.'
    } finally {
      if (serial === requestSerial) {
        loading.value = false
      }
      if (activeController === controller) {
        activeController = null
      }
    }
  }

  async function createAPIKey(name: string): Promise<APIKeyCreateResponse> {
    if (creatingAPIKey.value) {
      throw new Error('API key creation already in progress.')
    }

    const trimmedName = name.trim()
    if (!trimmedName) {
      errorMessage.value = 'name is required'
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
      return {
        ...created,
        key: keyValue,
      }
    } catch (error) {
      if (isUnauthorizedError(error)) {
        await handleUnauthorized()
      }
      errorMessage.value = error instanceof Error ? error.message : 'Failed to create API key.'
      throw error
    } finally {
      creatingAPIKey.value = false
    }
  }

  function confirmDeleteAPIKey(apiKeyID: string): boolean {
    if (typeof window === 'undefined' || typeof window.confirm !== 'function') {
      return true
    }
    return window.confirm(`Delete API key ${apiKeyID}?`)
  }

  async function deleteAPIKey(apiKeyID: string): Promise<void> {
    if (!apiKeyID || deletingAPIKeyID.value === apiKeyID || !confirmDeleteAPIKey(apiKeyID)) {
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
      errorMessage.value = error instanceof Error ? error.message : 'Failed to delete API key.'
    } finally {
      if (deletingAPIKeyID.value === apiKeyID) {
        deletingAPIKeyID.value = ''
      }
    }
  }

  function teardown(): void {
    requestSerial += 1
    activeController?.abort()
    activeController = null
    loading.value = false
  }

  return {
    apiKeys,
    loading,
    creatingAPIKey,
    deletingAPIKeyID,
    errorMessage,
    apiKeyDeleteButtonText,
    formatDateTime,
    loadAPIKeys,
    createAPIKey,
    deleteAPIKey,
    teardown,
    reset,
  }
})
