import { computed, ref } from 'vue'
import { defineStore } from 'pinia'

import { requestConfirm } from '@/composables/useConfirm'
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
import { createRequestGuard, isAbortError, toErrorMessage } from '@/utils/async'
import type {
  WorkerInflightResponse,
  WorkerListResponse,
  WorkerStartupCommandResponse,
  WorkerStatsResponse,
  WorkerStatus,
  WorkerType,
} from '@/types/workers'

export const workersPageSize = 25

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

export const useWorkersStore = defineStore('workers', () => {
  const statusFilter = ref<WorkerStatus>('all')
  const page = ref(1)
  const loading = ref(false)
  const errorMessage = ref('')
  const refreshedAt = ref<Date | null>(null)

  const creatingWorker = ref(false)
  const deletingNodeID = ref('')

  const dashboardStats = ref<WorkerStatsResponse>(emptyStats())
  const currentList = ref<WorkerListResponse | null>(null)
  const inflightData = ref<WorkerInflightResponse>({ workers: [], generated_at: '' })

  const requests = createRequestGuard()
  let timer: ReturnType<typeof setInterval> | null = null

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

  const total = computed(() => currentList.value?.total ?? 0)
  const totalPages = computed(() => Math.max(1, Math.ceil(total.value / workersPageSize)))
  const workerRows = computed(() => currentList.value?.items ?? [])

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
    const token = requests.begin()

    loading.value = true
    errorMessage.value = ''

    try {
      const [statsRes, listRes, inflightRes] = await Promise.all([
        fetchWorkerStatsAPI(staleAfterDefaultSec, token.signal),
        fetchWorkersAPI(statusFilter.value, page.value, workersPageSize, token.signal),
        fetchWorkerInflightAPI(token.signal),
      ])

      if (token.isStale()) {
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
      if (isAbortError(error) || token.isStale()) {
        return
      }
      if (isUnauthorizedError(error)) {
        await handleUnauthorized()
        return
      }
      errorMessage.value = toErrorMessage(error, 'Failed to load workers.')
    } finally {
      if (token.release()) {
        loading.value = false
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
    if (page.value <= 1) {
      return
    }
    page.value -= 1
    void loadDashboard()
  }

  function nextPage(): void {
    if (page.value >= totalPages.value) {
      return
    }
    page.value += 1
    void loadDashboard()
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
      const payload = await createWorkerAPI(workerType)
      await loadDashboard()
      return payload
    } catch (error) {
      if (isUnauthorizedError(error)) {
        await handleUnauthorized()
        return null
      }
      errorMessage.value = toErrorMessage(error, 'Failed to create worker.')
      return null
    } finally {
      creatingWorker.value = false
    }
  }

  async function deleteWorker(nodeID: string): Promise<void> {
    if (!nodeID || deletingNodeID.value === nodeID) {
      return
    }

    const confirmed = await requestConfirm({
      title: 'Delete Worker',
      message: 'The execution node will be unregistered and must be started again to rejoin.',
      detail: nodeID,
      confirmLabel: 'Delete Worker',
      destructive: true,
    })
    if (!confirmed) {
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
      errorMessage.value = toErrorMessage(error, 'Failed to delete worker.')
    } finally {
      if (deletingNodeID.value === nodeID) {
        deletingNodeID.value = ''
      }
    }
  }

  function shouldAutoRefresh(): boolean {
    if (loading.value) {
      return false
    }
    if (typeof document !== 'undefined' && document.visibilityState !== 'visible') {
      return false
    }
    return useAuthStore().isAuthenticated
  }

  function startAutoRefresh(): void {
    stopAutoRefresh()
    timer = setInterval(() => {
      if (shouldAutoRefresh()) {
        void loadDashboard()
      }
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
    if (shouldAutoRefresh()) {
      void loadDashboard()
    }
  }

  function teardown(): void {
    requests.abort()
    stopAutoRefresh()
    loading.value = false
  }

  function reset(): void {
    statusFilter.value = 'all'
    loading.value = false
    errorMessage.value = ''
    refreshedAt.value = null
    creatingWorker.value = false
    deletingNodeID.value = ''
    resetDashboard()
  }

  return {
    pageSize: workersPageSize,
    statusFilter,
    page,
    loading,
    errorMessage,
    refreshedAt,
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
    total,
    totalPages,
    workerRows,
    loadDashboard,
    setFilter,
    setPage,
    previousPage,
    nextPage,
    createWorker,
    deleteWorker,
    startAutoRefresh,
    stopAutoRefresh,
    onPageVisibilityChange,
    teardown,
    reset,
  }
})
