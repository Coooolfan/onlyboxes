import { computed, ref } from 'vue'
import { defineStore } from 'pinia'

import router from '@/router'
import { isUnauthorizedError } from '@/services/http'
import {
  createTrustedTokenAPI,
  createWorkerAPI,
  deleteTrustedTokenAPI,
  deleteWorkerAPI,
  fetchTrustedTokensAPI,
  fetchWorkersAPI,
  fetchWorkerStatsAPI,
} from '@/services/workers.api'
import { useAuthStore } from '@/stores/auth'
import type {
  TrustedTokenCreateResponse,
  TrustedTokenItem,
  WorkerItem,
  WorkerListResponse,
  WorkerStartupCommandResponse,
  WorkerStatsResponse,
  WorkerStatus,
} from '@/types/workers'

const pageSize = 25
const staleAfterDefaultSec = 30
const autoRefreshMs = 5000

function emptyStats(): WorkerStatsResponse {
  return {
    total: 0,
    online: 0,
    offline: 0,
    stale: 0,
    stale_after_sec: staleAfterDefaultSec,
    generated_at: '',
  }
}

function parseTimestamp(value: string): Date | null {
  const parsed = Date.parse(value)
  if (Number.isNaN(parsed)) {
    return null
  }
  return new Date(parsed)
}

function isAbortError(error: unknown): boolean {
  if (typeof DOMException !== 'undefined' && error instanceof DOMException) {
    return error.name === 'AbortError'
  }
  return error instanceof Error && error.name === 'AbortError'
}

export const useWorkersStore = defineStore('workers', () => {
  const statusFilter = ref<WorkerStatus>('all')
  const page = ref(1)
  const loading = ref(false)
  const errorMessage = ref('')
  const refreshedAt = ref<Date | null>(null)

  const autoRefreshEnabled = ref(true)
  const creatingWorker = ref(false)
  const deletingNodeID = ref('')
  const creatingTrustedToken = ref(false)
  const deletingTrustedTokenID = ref('')

  const dashboardStats = ref<WorkerStatsResponse>(emptyStats())
  const currentList = ref<WorkerListResponse | null>(null)
  const trustedTokens = ref<TrustedTokenItem[]>([])

  let timer: ReturnType<typeof setInterval> | null = null
  let loadRequestSerial = 0
  let activeController: AbortController | null = null

  const totalWorkers = computed(() => dashboardStats.value.total)
  const onlineWorkers = computed(() => dashboardStats.value.online)
  const offlineWorkers = computed(() => dashboardStats.value.offline)
  const staleWorkers = computed(() => dashboardStats.value.stale)
  const staleWorkersLabel = computed(() => `Heartbeat > ${dashboardStats.value.stale_after_sec}s`)

  const totalPages = computed(() => {
    const total = currentList.value?.total ?? 0
    return Math.max(1, Math.ceil(total / pageSize))
  })

  const workerRows = computed(() => currentList.value?.items ?? [])
  const canPrev = computed(() => page.value > 1)
  const canNext = computed(() => page.value < totalPages.value)

  const footerText = computed(() => {
    const total = currentList.value?.total ?? 0
    const start = total === 0 ? 0 : (page.value - 1) * pageSize + 1
    const end = Math.min(page.value * pageSize, total)
    return `${start}-${end} / ${total}`
  })

  function resetDashboard(): void {
    currentList.value = null
    dashboardStats.value = emptyStats()
    trustedTokens.value = []
    refreshedAt.value = null
    page.value = 1
  }

  async function redirectToLogin(): Promise<void> {
    const authStore = useAuthStore()
    authStore.logoutLocal()
    resetDashboard()
    errorMessage.value = ''

    if (router.currentRoute.value.path !== '/login') {
      await router.replace({
        path: '/login',
        query: { redirect: router.currentRoute.value.fullPath },
      })
    }
  }

  async function loadDashboard(): Promise<void> {
    const requestSerial = ++loadRequestSerial
    activeController?.abort()
    const controller = new AbortController()
    activeController = controller

    loading.value = true
    errorMessage.value = ''

    try {
      const [statsRes, listRes, trustedTokensRes] = await Promise.all([
        fetchWorkerStatsAPI(staleAfterDefaultSec, controller.signal),
        fetchWorkersAPI(statusFilter.value, page.value, pageSize, controller.signal),
        fetchTrustedTokensAPI(controller.signal),
      ])

      if (requestSerial !== loadRequestSerial || controller.signal.aborted) {
        return
      }

      dashboardStats.value = statsRes
      currentList.value = listRes
      trustedTokens.value = trustedTokensRes.items ?? []
      refreshedAt.value = parseTimestamp(statsRes.generated_at) ?? new Date()

      if (page.value > totalPages.value) {
        page.value = totalPages.value
        await loadDashboard()
      }
    } catch (error) {
      if (isAbortError(error) || requestSerial !== loadRequestSerial) {
        return
      }
      if (isUnauthorizedError(error)) {
        await redirectToLogin()
        return
      }
      errorMessage.value = error instanceof Error ? error.message : 'Failed to load workers.'
    } finally {
      if (requestSerial === loadRequestSerial) {
        loading.value = false
      }
      if (activeController === controller) {
        activeController = null
      }
    }
  }

  function setFilter(status: WorkerStatus, options: { load?: boolean } = {}): void {
    if (status === statusFilter.value) {
      return
    }
    statusFilter.value = status
    page.value = 1
    if (options.load !== false) {
      void loadDashboard()
    }
  }

  function setPage(targetPage: number, options: { load?: boolean } = {}): void {
    const nextPage = Math.max(1, Math.floor(targetPage))
    if (nextPage === page.value) {
      return
    }

    page.value = nextPage
    if (options.load !== false) {
      void loadDashboard()
    }
  }

  function previousPage(): void {
    if (!canPrev.value) {
      return
    }
    page.value -= 1
    void loadDashboard()
  }

  function nextPage(): void {
    if (!canNext.value) {
      return
    }
    page.value += 1
    void loadDashboard()
  }

  function deleteWorkerButtonText(nodeID: string): string {
    if (deletingNodeID.value === nodeID) {
      return 'Deleting...'
    }
    return 'Delete'
  }

  function trustedTokenDeleteButtonText(tokenID: string): string {
    if (deletingTrustedTokenID.value === tokenID) {
      return 'Deleting...'
    }
    return 'Delete'
  }

  function formatDateTime(value: string): string {
    const parsed = Date.parse(value)
    if (Number.isNaN(parsed)) {
      return '--'
    }
    return new Intl.DateTimeFormat('en-US', {
      month: 'short',
      day: '2-digit',
      hour: '2-digit',
      minute: '2-digit',
      second: '2-digit',
    }).format(new Date(parsed))
  }

  function ageSeconds(value: string): number {
    const parsed = Date.parse(value)
    if (Number.isNaN(parsed)) {
      return Number.POSITIVE_INFINITY
    }
    return Math.max(0, Math.floor((Date.now() - parsed) / 1000))
  }

  function formatAge(value: string): string {
    const seconds = ageSeconds(value)
    if (!Number.isFinite(seconds)) {
      return '--'
    }
    if (seconds < 60) {
      return `${seconds}s ago`
    }

    const minutes = Math.floor(seconds / 60)
    if (minutes < 60) {
      return `${minutes}m ago`
    }

    const hours = Math.floor(minutes / 60)
    return `${hours}h ago`
  }

  function formatCapabilities(worker: WorkerItem): string {
    if (!worker.capabilities || worker.capabilities.length === 0) {
      return '--'
    }
    return worker.capabilities.map((item) => item.name).join(', ')
  }

  function formatLabels(worker: WorkerItem): string {
    const entries = Object.entries(worker.labels ?? {})
    if (entries.length === 0) {
      return '--'
    }
    return entries.map(([key, value]) => `${key}=${value}`).join(' · ')
  }

  async function createWorker(): Promise<WorkerStartupCommandResponse | null> {
    if (creatingWorker.value) {
      return null
    }

    errorMessage.value = ''
    creatingWorker.value = true

    try {
      const payload: WorkerStartupCommandResponse = await createWorkerAPI()
      await loadDashboard()
      return payload
    } catch (error) {
      if (isUnauthorizedError(error)) {
        await redirectToLogin()
        return null
      }
      errorMessage.value = error instanceof Error ? error.message : 'Failed to create worker.'
      return null
    } finally {
      creatingWorker.value = false
    }
  }

  function confirmDeleteWorker(nodeID: string): boolean {
    if (typeof window === 'undefined' || typeof window.confirm !== 'function') {
      return true
    }
    return window.confirm(`Delete worker ${nodeID}?`)
  }

  async function deleteWorker(nodeID: string): Promise<void> {
    if (!nodeID || deletingNodeID.value === nodeID || !confirmDeleteWorker(nodeID)) {
      return
    }

    errorMessage.value = ''
    deletingNodeID.value = nodeID

    try {
      await deleteWorkerAPI(nodeID)
      await loadDashboard()
      if (page.value > totalPages.value) {
        page.value = totalPages.value
        await loadDashboard()
      }
    } catch (error) {
      if (isUnauthorizedError(error)) {
        await redirectToLogin()
        return
      }
      errorMessage.value = error instanceof Error ? error.message : 'Failed to delete worker.'
    } finally {
      if (deletingNodeID.value === nodeID) {
        deletingNodeID.value = ''
      }
    }
  }

  async function createTrustedToken(payload: { name: string }): Promise<TrustedTokenCreateResponse> {
    if (creatingTrustedToken.value) {
      throw new Error('Trusted token creation already in progress.')
    }

    const name = payload.name.trim()
    if (!name) {
      errorMessage.value = 'name is required'
      throw new Error('name is required')
    }

    creatingTrustedToken.value = true
    errorMessage.value = ''

    try {
      const created = await createTrustedTokenAPI({
        name,
      })
      const tokenValue = created.token.trim()
      if (!tokenValue) {
        throw new Error('API returned empty token value.')
      }
      await loadDashboard()
      return {
        ...created,
        token: tokenValue,
      }
    } catch (error) {
      if (isUnauthorizedError(error)) {
        await redirectToLogin()
      }
      errorMessage.value = error instanceof Error ? error.message : 'Failed to create trusted token.'
      throw error
    } finally {
      creatingTrustedToken.value = false
    }
  }

  function confirmDeleteTrustedToken(tokenID: string): boolean {
    if (typeof window === 'undefined' || typeof window.confirm !== 'function') {
      return true
    }
    return window.confirm(`Delete token ${tokenID}?`)
  }

  async function deleteTrustedToken(tokenID: string): Promise<void> {
    if (!tokenID || deletingTrustedTokenID.value === tokenID || !confirmDeleteTrustedToken(tokenID)) {
      return
    }

    deletingTrustedTokenID.value = tokenID
    errorMessage.value = ''
    try {
      await deleteTrustedTokenAPI(tokenID)
      await loadDashboard()
    } catch (error) {
      if (isUnauthorizedError(error)) {
        await redirectToLogin()
        return
      }
      errorMessage.value = error instanceof Error ? error.message : 'Failed to delete trusted token.'
    } finally {
      if (deletingTrustedTokenID.value === tokenID) {
        deletingTrustedTokenID.value = ''
      }
    }
  }

  function toggleAutoRefresh(): void {
    autoRefreshEnabled.value = !autoRefreshEnabled.value
  }

  function startAutoRefresh(): void {
    stopAutoRefresh()

    timer = setInterval(() => {
      if (!autoRefreshEnabled.value || loading.value) {
        return
      }
      if (typeof document !== 'undefined' && document.visibilityState !== 'visible') {
        return
      }
      const authStore = useAuthStore()
      if (!authStore.isAuthenticated) {
        return
      }
      void loadDashboard()
    }, autoRefreshMs)
  }

  function stopAutoRefresh(): void {
    if (!timer) {
      return
    }
    clearInterval(timer)
    timer = null
  }

  function onPageVisibilityChange(): void {
    if (typeof document !== 'undefined' && document.visibilityState !== 'visible') {
      return
    }
    if (!autoRefreshEnabled.value || loading.value) {
      return
    }
    void loadDashboard()
  }

  function teardown(): void {
    activeController?.abort()
    stopAutoRefresh()
  }

  return {
    pageSize,
    statusFilter,
    page,
    loading,
    errorMessage,
    refreshedAt,
    autoRefreshEnabled,
    creatingWorker,
    deletingNodeID,
    creatingTrustedToken,
    deletingTrustedTokenID,
    dashboardStats,
    currentList,
    trustedTokens,
    totalWorkers,
    onlineWorkers,
    offlineWorkers,
    staleWorkers,
    staleWorkersLabel,
    totalPages,
    workerRows,
    canPrev,
    canNext,
    footerText,
    loadDashboard,
    setFilter,
    setPage,
    previousPage,
    nextPage,
    deleteWorkerButtonText,
    trustedTokenDeleteButtonText,
    formatDateTime,
    formatAge,
    formatCapabilities,
    formatLabels,
    createWorker,
    deleteWorker,
    createTrustedToken,
    deleteTrustedToken,
    toggleAutoRefresh,
    startAutoRefresh,
    stopAutoRefresh,
    onPageVisibilityChange,
    teardown,
  }
})
