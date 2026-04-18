import { computed, ref } from 'vue'
import { defineStore } from 'pinia'

import { isUnauthorizedError } from '@/services/http'
import {
  createWorkerAPI,
  deleteWorkerAPI,
  fetchWorkerInflightAPI,
  fetchWorkersAPI,
  fetchWorkerStatsAPI,
} from '@/services/workers.api'
import { redirectToLogin } from '@/stores/auth-redirect'
import { useAuthStore } from '@/stores/auth'
import { formatDateTime } from '@/utils/datetime'
import type {
  WorkerInflightResponse,
  WorkerItem,
  WorkerListResponse,
  WorkerStartupCommandResponse,
  WorkerStatsResponse,
  WorkerStatus,
  WorkerType,
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

  const dashboardStats = ref<WorkerStatsResponse>(emptyStats())
  const currentList = ref<WorkerListResponse | null>(null)
  const inflightData = ref<WorkerInflightResponse>({ workers: [], generated_at: '' })

  let timer: ReturnType<typeof setInterval> | null = null
  let loadRequestSerial = 0
  let activeController: AbortController | null = null

  const totalWorkers = computed(() => dashboardStats.value.total)
  const onlineWorkers = computed(() => dashboardStats.value.online)
  const offlineWorkers = computed(() => dashboardStats.value.offline)
  const staleWorkers = computed(() => dashboardStats.value.stale)
  const activeSessions = computed(() =>
    inflightData.value.workers.reduce(
      (total, worker) => total + (worker.active_session_count ?? 0),
      0,
    ),
  )
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
    inflightData.value = { workers: [], generated_at: '' }
    refreshedAt.value = null
    page.value = 1
  }

  async function handleUnauthorized(): Promise<void> {
    await redirectToLogin(() => {
      resetDashboard()
      errorMessage.value = ''
    })
  }

  async function loadDashboard(): Promise<void> {
    const requestSerial = ++loadRequestSerial
    activeController?.abort()
    const controller = new AbortController()
    activeController = controller

    loading.value = true
    errorMessage.value = ''

    try {
      const [statsRes, listRes, inflightRes] = await Promise.all([
        fetchWorkerStatsAPI(staleAfterDefaultSec, controller.signal),
        fetchWorkersAPI(statusFilter.value, page.value, pageSize, controller.signal),
        fetchWorkerInflightAPI(controller.signal),
      ])

      if (requestSerial !== loadRequestSerial || controller.signal.aborted) {
        return
      }

      dashboardStats.value = statsRes
      currentList.value = listRes
      inflightData.value = inflightRes
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
        await handleUnauthorized()
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

  async function createWorker(
    workerType: WorkerType,
  ): Promise<WorkerStartupCommandResponse | null> {
    if (creatingWorker.value) {
      return null
    }

    errorMessage.value = ''
    creatingWorker.value = true

    try {
      const payload: WorkerStartupCommandResponse = await createWorkerAPI(workerType)
      await loadDashboard()
      return payload
    } catch (error) {
      if (isUnauthorizedError(error)) {
        await handleUnauthorized()
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
        await handleUnauthorized()
        return
      }
      errorMessage.value = error instanceof Error ? error.message : 'Failed to delete worker.'
    } finally {
      if (deletingNodeID.value === nodeID) {
        deletingNodeID.value = ''
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

  function reset(): void {
    statusFilter.value = 'all'
    loading.value = false
    errorMessage.value = ''
    refreshedAt.value = null
    autoRefreshEnabled.value = true
    creatingWorker.value = false
    deletingNodeID.value = ''
    resetDashboard()
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
    dashboardStats,
    currentList,
    inflightData,
    totalWorkers,
    onlineWorkers,
    offlineWorkers,
    staleWorkers,
    activeSessions,
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
    formatDateTime,
    formatAge,
    formatCapabilities,
    createWorker,
    deleteWorker,
    toggleAutoRefresh,
    startAutoRefresh,
    stopAutoRefresh,
    onPageVisibilityChange,
    teardown,
    reset,
  }
})
