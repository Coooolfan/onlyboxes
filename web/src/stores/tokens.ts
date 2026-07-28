import { ref } from 'vue'
import { defineStore } from 'pinia'

import { requestConfirm } from '@/composables/useConfirm'
import { isUnauthorizedError } from '@/services/http'
import {
  createTrustedTokenAPI,
  deleteTrustedTokenAPI,
  fetchTrustedTokensAPI,
} from '@/services/workers.api'
import { redirectToLogin } from '@/stores/auth-redirect'
import { createRequestGuard, isAbortError, toErrorMessage } from '@/utils/async'
import type { TrustedTokenCreateResponse, TrustedTokenItem } from '@/types/workers'

export const useTokensStore = defineStore('tokens', () => {
  const loading = ref(false)
  const errorMessage = ref('')
  const trustedTokens = ref<TrustedTokenItem[]>([])
  const refreshedAt = ref<Date | null>(null)
  const creatingTrustedToken = ref(false)
  const deletingTrustedTokenID = ref('')

  const requests = createRequestGuard()

  function reset(): void {
    trustedTokens.value = []
    loading.value = false
    errorMessage.value = ''
    refreshedAt.value = null
    deletingTrustedTokenID.value = ''
    creatingTrustedToken.value = false
  }

  async function handleUnauthorized(): Promise<void> {
    await redirectToLogin(reset)
  }

  async function loadTokens(): Promise<void> {
    const token = requests.begin()
    loading.value = true
    errorMessage.value = ''

    try {
      const payload = await fetchTrustedTokensAPI(token.signal)
      if (token.isStale()) {
        return
      }
      trustedTokens.value = payload.items ?? []
      refreshedAt.value = new Date()
    } catch (error) {
      if (isAbortError(error) || token.isStale()) {
        return
      }
      if (isUnauthorizedError(error)) {
        await handleUnauthorized()
        return
      }
      errorMessage.value = toErrorMessage(error, 'Failed to load trusted tokens.')
    } finally {
      if (token.release()) {
        loading.value = false
      }
    }
  }

  async function createTrustedToken(name: string): Promise<TrustedTokenCreateResponse> {
    if (creatingTrustedToken.value) {
      throw new Error('Trusted token creation already in progress.')
    }

    const trimmedName = name.trim()
    if (!trimmedName) {
      throw new Error('name is required')
    }

    creatingTrustedToken.value = true
    errorMessage.value = ''
    try {
      const created = await createTrustedTokenAPI({ name: trimmedName })
      const tokenValue = created.token.trim()
      if (!tokenValue) {
        throw new Error('API returned empty token value.')
      }
      await loadTokens()
      return { ...created, token: tokenValue }
    } catch (error) {
      if (isUnauthorizedError(error)) {
        await handleUnauthorized()
      }
      throw error instanceof Error ? error : new Error('Failed to create trusted token.')
    } finally {
      creatingTrustedToken.value = false
    }
  }

  async function deleteTrustedToken(tokenID: string): Promise<void> {
    if (!tokenID || deletingTrustedTokenID.value === tokenID) {
      return
    }

    const target = trustedTokens.value.find((item) => item.id === tokenID)
    const confirmed = await requestConfirm({
      title: 'Delete Trusted Token',
      message: 'Clients still using this token lose MCP access immediately.',
      detail: target ? `${target.name} (${tokenID})` : tokenID,
      confirmLabel: 'Delete Token',
      destructive: true,
    })
    if (!confirmed) {
      return
    }

    deletingTrustedTokenID.value = tokenID
    errorMessage.value = ''
    try {
      await deleteTrustedTokenAPI(tokenID)
      await loadTokens()
    } catch (error) {
      if (isUnauthorizedError(error)) {
        await handleUnauthorized()
        return
      }
      errorMessage.value = toErrorMessage(error, 'Failed to delete trusted token.')
    } finally {
      if (deletingTrustedTokenID.value === tokenID) {
        deletingTrustedTokenID.value = ''
      }
    }
  }

  function teardown(): void {
    requests.abort()
    loading.value = false
  }

  return {
    loading,
    errorMessage,
    trustedTokens,
    refreshedAt,
    creatingTrustedToken,
    deletingTrustedTokenID,
    loadTokens,
    createTrustedToken,
    deleteTrustedToken,
    teardown,
    reset,
  }
})
