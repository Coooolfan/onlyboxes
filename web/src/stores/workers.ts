import { computed, ref } from 'vue'
import { defineStore } from 'pinia'

import router from '@/router'
import { isUnauthorizedError } from '@/services/http'
import {
  createTrustedTokenAPI,
  createWorkerAPI,
  deleteTrustedTokenAPI,
  deleteWorkerAPI,
  fetchTrustedTokenValueAPI,
  fetchTrustedTokensAPI,
  fetchWorkersAPI,
  fetchWorkerStartupCommandAPI,
  fetchWorkerStatsAPI,
} from '@/services/workers.api'
import { useAuthStore } from '@/stores/auth'
import type {
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

async function writeTextToClipboard(text: string): Promise<void> {
  if (typeof navigator !== 'undefined' && navigator.clipboard?.writeText) {
    await navigator.clipboard.writeText(text)
    return
  }

  if (typeof document === 'undefined') {
    throw new Error('Clipboard API unavailable.')
  }

  const textarea = document.createElement('textarea')
  textarea.value = text
  textarea.setAttribute('readonly', '')
  textarea.style.position = 'fixed'
  textarea.style.top = '0'
  textarea.style.left = '-9999px'
  textarea.style.opacity = '0'
  document.body.appendChild(textarea)
  textarea.focus()
  textarea.select()

  const copied = document.execCommand('copy')
  document.body.removeChild(textarea)
  if (!copied) {
    throw new Error('Failed to copy startup command.')
  }
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
  const copyingNodeID = ref('')
  const copiedNodeID = ref('')
  const copyFailedNodeID = ref('')
  const creatingTrustedToken = ref(false)
  const deletingTrustedTokenID = ref('')
  const copyingTrustedTokenID = ref('')
  const copiedTrustedTokenID = ref('')

  const dashboardStats = ref<WorkerStatsResponse>(emptyStats())
  const currentList = ref<WorkerListResponse | null>(null)
  const trustedTokens = ref<TrustedTokenItem[]>([])

  let timer: ReturnType<typeof setInterval> | null = null
  let loadRequestSerial = 0
  let activeController: AbortController | null = null
  let copyFeedbackTimer: ReturnType<typeof setTimeout> | null = null
  let trustedTokenCopyFeedbackTimer: ReturnType<typeof setTimeout> | null = null

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

  function scheduleCopyFeedbackReset(): void {
    if (copyFeedbackTimer) {
      clearTimeout(copyFeedbackTimer)
    }
    copyFeedbackTimer = setTimeout(() => {
      copiedNodeID.value = ''
      copyFailedNodeID.value = ''
      copyFeedbackTimer = null
    }, 1500)
  }

  function resetCopyFeedback(): void {
    if (copyFeedbackTimer) {
      clearTimeout(copyFeedbackTimer)
      copyFeedbackTimer = null
    }
    copyingNodeID.value = ''
    copiedNodeID.value = ''
    copyFailedNodeID.value = ''
  }

  function scheduleTrustedTokenCopyFeedbackReset(): void {
    if (trustedTokenCopyFeedbackTimer) {
      clearTimeout(trustedTokenCopyFeedbackTimer)
    }
    trustedTokenCopyFeedbackTimer = setTimeout(() => {
      copiedTrustedTokenID.value = ''
      trustedTokenCopyFeedbackTimer = null
    }, 1500)
  }

  function resetTrustedTokenCopyFeedback(): void {
    if (trustedTokenCopyFeedbackTimer) {
      clearTimeout(trustedTokenCopyFeedbackTimer)
      trustedTokenCopyFeedbackTimer = null
    }
    copyingTrustedTokenID.value = ''
    copiedTrustedTokenID.value = ''
  }

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
    resetCopyFeedback()
    resetTrustedTokenCopyFeedback()
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

  function startupCopyButtonText(nodeID: string): string {
    if (copyingNodeID.value === nodeID) {
      return 'Copying...'
    }
    if (copiedNodeID.value === nodeID) {
      return 'Copied'
    }
    if (copyFailedNodeID.value === nodeID) {
      return 'Copy Failed'
    }
    return 'Copy Start Cmd'
  }

  function deleteWorkerButtonText(nodeID: string): string {
    if (deletingNodeID.value === nodeID) {
      return 'Deleting...'
    }
    return 'Delete'
  }

  function trustedTokenCopyButtonText(tokenID: string): string {
    if (copyingTrustedTokenID.value === tokenID) {
      return 'Copying...'
    }
    if (copiedTrustedTokenID.value === tokenID) {
      return 'Copied'
    }
    return 'Copy'
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

  async function copyWorkerStartupCommand(nodeID: string): Promise<void> {
    if (!nodeID || copyingNodeID.value === nodeID) {
      return
    }

    if (copyFeedbackTimer) {
      clearTimeout(copyFeedbackTimer)
      copyFeedbackTimer = null
    }
    copiedNodeID.value = ''
    copyFailedNodeID.value = ''
    errorMessage.value = ''

    copyingNodeID.value = nodeID
    try {
      const command = await fetchWorkerStartupCommandAPI(nodeID)
      await writeTextToClipboard(command)
      copiedNodeID.value = nodeID
      scheduleCopyFeedbackReset()
    } catch (error) {
      if (isUnauthorizedError(error)) {
        await redirectToLogin()
        return
      }
      copyFailedNodeID.value = nodeID
      errorMessage.value = error instanceof Error ? error.message : 'Failed to copy startup command.'
      scheduleCopyFeedbackReset()
    } finally {
      if (copyingNodeID.value === nodeID) {
        copyingNodeID.value = ''
      }
    }
  }

  async function createWorker(): Promise<void> {
    if (creatingWorker.value) {
      return
    }

    if (copyFeedbackTimer) {
      clearTimeout(copyFeedbackTimer)
      copyFeedbackTimer = null
    }
    copiedNodeID.value = ''
    copyFailedNodeID.value = ''
    errorMessage.value = ''
    creatingWorker.value = true

    try {
      const payload: WorkerStartupCommandResponse = await createWorkerAPI()
      await writeTextToClipboard(payload.command)
      copiedNodeID.value = payload.node_id
      scheduleCopyFeedbackReset()
      await loadDashboard()
    } catch (error) {
      if (isUnauthorizedError(error)) {
        await redirectToLogin()
        return
      }
      errorMessage.value = error instanceof Error ? error.message : 'Failed to create worker.'
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
      if (copiedNodeID.value === nodeID || copyFailedNodeID.value === nodeID || copyingNodeID.value === nodeID) {
        resetCopyFeedback()
      }

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

  async function createTrustedToken(payload: { name: string; token?: string }): Promise<void> {
    if (creatingTrustedToken.value) {
      return
    }

    const name = payload.name.trim()
    const token = payload.token?.trim() ?? ''
    if (!name) {
      errorMessage.value = 'name is required'
      return
    }

    creatingTrustedToken.value = true
    errorMessage.value = ''

    try {
      await createTrustedTokenAPI({
        name,
        token: token === '' ? undefined : token,
      })
      await loadDashboard()
    } catch (error) {
      if (isUnauthorizedError(error)) {
        await redirectToLogin()
        return
      }
      errorMessage.value = error instanceof Error ? error.message : 'Failed to create trusted token.'
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
      if (copiedTrustedTokenID.value === tokenID || copyingTrustedTokenID.value === tokenID) {
        resetTrustedTokenCopyFeedback()
      }
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

  async function copyTrustedToken(tokenID: string): Promise<void> {
    const targetID = tokenID.trim()
    if (!targetID || copyingTrustedTokenID.value === targetID) {
      return
    }

    if (trustedTokenCopyFeedbackTimer) {
      clearTimeout(trustedTokenCopyFeedbackTimer)
      trustedTokenCopyFeedbackTimer = null
    }
    copiedTrustedTokenID.value = ''
    errorMessage.value = ''
    copyingTrustedTokenID.value = targetID

    try {
      const payload = await fetchTrustedTokenValueAPI(targetID)
      await writeTextToClipboard(payload.token)
      copiedTrustedTokenID.value = targetID
      scheduleTrustedTokenCopyFeedbackReset()
    } catch (error) {
      if (isUnauthorizedError(error)) {
        await redirectToLogin()
        return
      }
      errorMessage.value = error instanceof Error ? error.message : 'Failed to copy trusted token.'
    } finally {
      if (copyingTrustedTokenID.value === targetID) {
        copyingTrustedTokenID.value = ''
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
    resetCopyFeedback()
    resetTrustedTokenCopyFeedback()
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
    copyingNodeID,
    copiedNodeID,
    copyFailedNodeID,
    creatingTrustedToken,
    deletingTrustedTokenID,
    copyingTrustedTokenID,
    copiedTrustedTokenID,
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
    startupCopyButtonText,
    deleteWorkerButtonText,
    trustedTokenCopyButtonText,
    trustedTokenDeleteButtonText,
    formatDateTime,
    formatAge,
    formatCapabilities,
    formatLabels,
    copyWorkerStartupCommand,
    createWorker,
    deleteWorker,
    createTrustedToken,
    deleteTrustedToken,
    copyTrustedToken,
    toggleAutoRefresh,
    startAutoRefresh,
    stopAutoRefresh,
    onPageVisibilityChange,
    teardown,
  }
})
