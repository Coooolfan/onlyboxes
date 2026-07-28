import { computed, ref } from 'vue'
import { defineStore } from 'pinia'

import { requestConfirm } from '@/composables/useConfirm'
import { deleteAccountAPI, fetchAccountsAPI } from '@/services/auth.api'
import { isUnauthorizedError } from '@/services/http'
import { redirectToLogin } from '@/stores/auth-redirect'
import { createRequestGuard, isAbortError, toErrorMessage } from '@/utils/async'
import type { AccountListItem } from '@/types/auth'

export const accountPageSize = 20

export const useAccountsStore = defineStore('accounts', () => {
  const loading = ref(false)
  const errorMessage = ref('')
  const accounts = ref<AccountListItem[]>([])
  const total = ref(0)
  const page = ref(1)
  const refreshedAt = ref<Date | null>(null)
  const deletingAccountID = ref('')

  const requests = createRequestGuard()

  const totalPages = computed(() => Math.max(1, Math.ceil(total.value / accountPageSize)))

  function reset(): void {
    loading.value = false
    errorMessage.value = ''
    accounts.value = []
    total.value = 0
    page.value = 1
    refreshedAt.value = null
    deletingAccountID.value = ''
  }

  async function handleUnauthorized(): Promise<void> {
    await redirectToLogin(reset)
  }

  async function loadAccounts(targetPage = page.value): Promise<void> {
    page.value = Math.max(1, Math.floor(targetPage))

    const token = requests.begin()
    loading.value = true
    errorMessage.value = ''

    try {
      const payload = await fetchAccountsAPI(page.value, accountPageSize, token.signal)
      if (token.isStale()) {
        return
      }

      accounts.value = payload.items ?? []
      total.value = payload.total ?? 0
      const serverPage = typeof payload.page === 'number' ? Math.floor(payload.page) : page.value
      page.value = Math.min(Math.max(1, serverPage), totalPages.value)
      refreshedAt.value = new Date()
    } catch (error) {
      if (isAbortError(error) || token.isStale()) {
        return
      }
      if (isUnauthorizedError(error)) {
        await handleUnauthorized()
        return
      }
      errorMessage.value = toErrorMessage(error, 'Failed to load accounts.')
    } finally {
      if (token.release()) {
        loading.value = false
      }
    }
  }

  function setPage(targetPage: number): void {
    const nextPage = Math.max(1, Math.floor(targetPage))
    if (nextPage === page.value) {
      return
    }
    void loadAccounts(nextPage)
  }

  function previousPage(): void {
    if (page.value <= 1) {
      return
    }
    void loadAccounts(page.value - 1)
  }

  function nextPage(): void {
    if (page.value >= totalPages.value) {
      return
    }
    void loadAccounts(page.value + 1)
  }

  async function deleteAccount(accountID: string): Promise<void> {
    if (!accountID || deletingAccountID.value === accountID) {
      return
    }

    const target = accounts.value.find((item) => item.account_id === accountID)
    const confirmed = await requestConfirm({
      title: 'Delete Account',
      message: 'The account and all of its console sessions are removed permanently.',
      detail: target ? `${target.username} (${accountID})` : accountID,
      confirmLabel: 'Delete Account',
      destructive: true,
    })
    if (!confirmed) {
      return
    }

    deletingAccountID.value = accountID
    errorMessage.value = ''

    try {
      await deleteAccountAPI(accountID)
      const targetPage = accounts.value.length === 1 && page.value > 1 ? page.value - 1 : page.value
      await loadAccounts(targetPage)
    } catch (error) {
      if (isUnauthorizedError(error)) {
        await handleUnauthorized()
        return
      }
      errorMessage.value = toErrorMessage(error, 'Failed to delete account.')
    } finally {
      if (deletingAccountID.value === accountID) {
        deletingAccountID.value = ''
      }
    }
  }

  function teardown(): void {
    requests.abort()
    loading.value = false
  }

  return {
    accountPageSize,
    loading,
    errorMessage,
    accounts,
    total,
    page,
    totalPages,
    refreshedAt,
    deletingAccountID,
    loadAccounts,
    setPage,
    previousPage,
    nextPage,
    deleteAccount,
    teardown,
    reset,
  }
})
