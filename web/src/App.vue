<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'

type WorkerStatus = 'all' | 'online' | 'offline'
type AuthState = 'loading' | 'unauthenticated' | 'authenticated'

interface CapabilityDeclaration {
  name: string
}

interface WorkerItem {
  node_id: string
  node_name: string
  executor_kind: string
  capabilities: CapabilityDeclaration[]
  labels: Record<string, string>
  version: string
  status: Exclude<WorkerStatus, 'all'>
  registered_at: string
  last_seen_at: string
}

interface WorkerListResponse {
  items: WorkerItem[]
  total: number
  page: number
  page_size: number
}

interface WorkerStatsResponse {
  total: number
  online: number
  offline: number
  stale: number
  stale_after_sec: number
  generated_at: string
}

interface WorkerStartupCommandResponse {
  node_id: string
  command: string
}

const pageSize = 25
const staleAfterDefaultSec = 30
const statusFilter = ref<WorkerStatus>('all')
const page = ref(1)
const loading = ref(false)
const errorMessage = ref('')
const refreshedAt = ref<Date | null>(null)
const autoRefreshEnabled = ref(true)
const autoRefreshMs = 5000

const authState = ref<AuthState>('loading')
const loginUsername = ref('')
const loginPassword = ref('')
const loginErrorMessage = ref('')
const loginSubmitting = ref(false)
const creatingWorker = ref(false)
const deletingNodeID = ref('')
const copyingNodeID = ref('')
const copiedNodeID = ref('')
const copyFailedNodeID = ref('')

const dashboardStats = ref<WorkerStatsResponse>(emptyStats())
const currentList = ref<WorkerListResponse | null>(null)

let timer: ReturnType<typeof setInterval> | null = null
let loadRequestSerial = 0
let activeController: AbortController | null = null
let copyFeedbackTimer: ReturnType<typeof setTimeout> | null = null

class UnauthorizedError extends Error {
  constructor() {
    super('authentication required')
    this.name = 'UnauthorizedError'
  }
}

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

async function loadDashboard(): Promise<void> {
  const requestSerial = ++loadRequestSerial
  activeController?.abort()
  const controller = new AbortController()
  activeController = controller

  loading.value = true
  errorMessage.value = ''

  try {
    const [statsRes, listRes] = await Promise.all([
      fetchStats(staleAfterDefaultSec, controller.signal),
      fetchWorkers(statusFilter.value, page.value, pageSize, controller.signal),
    ])

    if (requestSerial !== loadRequestSerial || controller.signal.aborted) {
      return
    }

    dashboardStats.value = statsRes
    currentList.value = listRes
    refreshedAt.value = parseTimestamp(statsRes.generated_at) ?? new Date()
    authState.value = 'authenticated'
  } catch (error) {
    if (isAbortError(error) || requestSerial !== loadRequestSerial) {
      return
    }
    if (isUnauthorizedError(error)) {
      switchToUnauthenticatedState()
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

async function submitLogin(): Promise<void> {
  if (loginSubmitting.value) {
    return
  }

  loginErrorMessage.value = ''
  if (loginUsername.value.trim() === '' || loginPassword.value === '') {
    loginErrorMessage.value = '请输入账号和密码。'
    return
  }

  loginSubmitting.value = true
  try {
    const response = await fetch('/api/v1/console/login', {
      method: 'POST',
      headers: {
        Accept: 'application/json',
        'Content-Type': 'application/json',
      },
      credentials: 'same-origin',
      body: JSON.stringify({
        username: loginUsername.value,
        password: loginPassword.value,
      }),
    })

    if (response.status === 401) {
      loginErrorMessage.value = '账号或密码错误'
      return
    }
    if (!response.ok) {
      throw new Error(`API ${response.status}: ${response.statusText}`)
    }

    authState.value = 'authenticated'
    page.value = 1
    await loadDashboard()
  } catch (error) {
    loginErrorMessage.value = error instanceof Error ? error.message : '登录失败，请稍后重试。'
  } finally {
    loginSubmitting.value = false
  }
}

async function logout(): Promise<void> {
  activeController?.abort()
  loadRequestSerial++

  try {
    await fetch('/api/v1/console/logout', {
      method: 'POST',
      headers: {
        Accept: 'application/json',
      },
      credentials: 'same-origin',
    })
  } catch {
    // ignore network errors and force local logout state
  }

  loginPassword.value = ''
  switchToUnauthenticatedState()
}

async function fetchWorkers(
  status: WorkerStatus,
  targetPage: number,
  targetPageSize: number,
  signal: AbortSignal,
): Promise<WorkerListResponse> {
  const query = new URLSearchParams({
    page: String(targetPage),
    page_size: String(targetPageSize),
    status,
  })

  const response = await fetch(`/api/v1/workers?${query.toString()}`, {
    headers: {
      Accept: 'application/json',
    },
    credentials: 'same-origin',
    signal,
  })

  if (response.status === 401) {
    throw new UnauthorizedError()
  }
  if (!response.ok) {
    throw new Error(`API ${response.status}: ${response.statusText}`)
  }

  const payload = (await response.json()) as WorkerListResponse
  return {
    items: payload.items ?? [],
    total: payload.total ?? 0,
    page: payload.page ?? targetPage,
    page_size: payload.page_size ?? targetPageSize,
  }
}

async function fetchStats(staleAfterSec: number, signal: AbortSignal): Promise<WorkerStatsResponse> {
  const query = new URLSearchParams({
    stale_after_sec: String(staleAfterSec),
  })
  const response = await fetch(`/api/v1/workers/stats?${query.toString()}`, {
    headers: {
      Accept: 'application/json',
    },
    credentials: 'same-origin',
    signal,
  })

  if (response.status === 401) {
    throw new UnauthorizedError()
  }
  if (!response.ok) {
    throw new Error(`API ${response.status}: ${response.statusText}`)
  }

  const payload = (await response.json()) as WorkerStatsResponse
  return {
    total: payload.total ?? 0,
    online: payload.online ?? 0,
    offline: payload.offline ?? 0,
    stale: payload.stale ?? 0,
    stale_after_sec: payload.stale_after_sec ?? staleAfterSec,
    generated_at: payload.generated_at ?? '',
  }
}

async function fetchWorkerStartupCommand(nodeID: string): Promise<string> {
  const response = await fetch(`/api/v1/workers/${encodeURIComponent(nodeID)}/startup-command`, {
    headers: {
      Accept: 'application/json',
    },
    credentials: 'same-origin',
  })

  if (response.status === 401) {
    throw new UnauthorizedError()
  }
  if (!response.ok) {
    const apiError = await parseAPIError(response)
    throw new Error(apiError)
  }

  const payload = (await response.json()) as WorkerStartupCommandResponse
  const command = payload.command?.trim()
  if (!command) {
    throw new Error('API returned empty startup command.')
  }
  return command
}

async function createWorkerWithStartupCommand(): Promise<WorkerStartupCommandResponse> {
  const response = await fetch('/api/v1/workers', {
    method: 'POST',
    headers: {
      Accept: 'application/json',
      'Content-Type': 'application/json',
    },
    credentials: 'same-origin',
    body: '{}',
  })

  if (response.status === 401) {
    throw new UnauthorizedError()
  }
  if (!response.ok) {
    const apiError = await parseAPIError(response)
    throw new Error(apiError)
  }

  const payload = (await response.json()) as WorkerStartupCommandResponse
  const nodeID = payload.node_id?.trim()
  const command = payload.command?.trim()
  if (!nodeID || !command) {
    throw new Error('API returned invalid worker startup payload.')
  }
  return {
    node_id: nodeID,
    command,
  }
}

async function deleteWorkerByID(nodeID: string): Promise<void> {
  const response = await fetch(`/api/v1/workers/${encodeURIComponent(nodeID)}`, {
    method: 'DELETE',
    headers: {
      Accept: 'application/json',
    },
    credentials: 'same-origin',
  })

  if (response.status === 401) {
    throw new UnauthorizedError()
  }
  if (response.status === 204) {
    return
  }
  if (!response.ok) {
    const apiError = await parseAPIError(response)
    throw new Error(apiError)
  }
}

async function parseAPIError(response: Response): Promise<string> {
  try {
    const payload = (await response.json()) as { error?: string }
    if (typeof payload.error === 'string' && payload.error.trim() !== '') {
      return payload.error
    }
  } catch {
    // ignore json parsing errors and use default status text
  }
  return `API ${response.status}: ${response.statusText}`
}

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

function resetDashboard(): void {
  currentList.value = null
  dashboardStats.value = emptyStats()
  refreshedAt.value = null
  page.value = 1
}

function switchToUnauthenticatedState(): void {
  resetCopyFeedback()
  authState.value = 'unauthenticated'
  loginErrorMessage.value = ''
  loginPassword.value = ''
  errorMessage.value = ''
  resetDashboard()
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
    const command = await fetchWorkerStartupCommand(nodeID)
    await writeTextToClipboard(command)
    copiedNodeID.value = nodeID
    scheduleCopyFeedbackReset()
  } catch (error) {
    if (isUnauthorizedError(error)) {
      switchToUnauthenticatedState()
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

function confirmDeleteWorker(nodeID: string): boolean {
  if (typeof window === 'undefined' || typeof window.confirm !== 'function') {
    return true
  }
  return window.confirm(`Delete worker ${nodeID}?`)
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
    const payload = await createWorkerWithStartupCommand()
    await writeTextToClipboard(payload.command)
    copiedNodeID.value = payload.node_id
    scheduleCopyFeedbackReset()
    await loadDashboard()
  } catch (error) {
    if (isUnauthorizedError(error)) {
      switchToUnauthenticatedState()
      return
    }
    errorMessage.value = error instanceof Error ? error.message : 'Failed to create worker.'
  } finally {
    creatingWorker.value = false
  }
}

async function deleteWorker(nodeID: string): Promise<void> {
  if (!nodeID || deletingNodeID.value === nodeID || !confirmDeleteWorker(nodeID)) {
    return
  }

  errorMessage.value = ''
  deletingNodeID.value = nodeID
  try {
    await deleteWorkerByID(nodeID)
    if (copiedNodeID.value === nodeID || copyFailedNodeID.value === nodeID || copyingNodeID.value === nodeID) {
      resetCopyFeedback()
    }
    await loadDashboard()
  } catch (error) {
    if (isUnauthorizedError(error)) {
      switchToUnauthenticatedState()
      return
    }
    errorMessage.value = error instanceof Error ? error.message : 'Failed to delete worker.'
  } finally {
    if (deletingNodeID.value === nodeID) {
      deletingNodeID.value = ''
    }
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

function isUnauthorizedError(error: unknown): boolean {
  return error instanceof UnauthorizedError
}

function setStatusFilter(status: WorkerStatus): void {
  if (status === statusFilter.value) {
    return
  }
  statusFilter.value = status
  page.value = 1
  void loadDashboard()
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
  return worker.capabilities
    .map((item) => item.name)
    .join(', ')
}

function formatLabels(worker: WorkerItem): string {
  const labels = worker.labels ?? {}
  const entries = Object.entries(labels)
  if (entries.length === 0) {
    return '--'
  }
  return entries.map(([key, value]) => `${key}=${value}`).join(' · ')
}

function startAutoRefresh(): void {
  stopAutoRefresh()
  timer = setInterval(() => {
    if (authState.value !== 'authenticated' || !autoRefreshEnabled.value || loading.value) {
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

function toggleAutoRefresh(): void {
  autoRefreshEnabled.value = !autoRefreshEnabled.value
}

onMounted(() => {
  void loadDashboard()
  startAutoRefresh()
})

onBeforeUnmount(() => {
  activeController?.abort()
  stopAutoRefresh()
  resetCopyFeedback()
})
</script>

<template>
  <div class="dashboard-shell">
    <div class="gradient-orb orb-left" />
    <div class="gradient-orb orb-right" />

    <main class="dashboard-content">
      <section v-if="authState === 'loading'" class="auth-panel">
        <p class="eyebrow">Onlyboxes / Console Login</p>
        <h1>Checking Session</h1>
        <p class="subtitle">Verifying existing dashboard session...</p>
      </section>

      <section v-else-if="authState === 'unauthenticated'" class="auth-panel">
        <p class="eyebrow">Onlyboxes / Console Login</p>
        <h1>Sign In to Control Panel</h1>
        <p class="subtitle">Use the dashboard username and password printed in the console startup logs.</p>

        <form class="login-form" @submit.prevent="submitLogin">
          <label class="field-label" for="dashboard-username">Username</label>
          <input
            id="dashboard-username"
            v-model="loginUsername"
            class="field-input"
            type="text"
            name="username"
            autocomplete="username"
            spellcheck="false"
          />

          <label class="field-label" for="dashboard-password">Password</label>
          <input
            id="dashboard-password"
            v-model="loginPassword"
            class="field-input"
            type="password"
            name="password"
            autocomplete="current-password"
          />

          <p v-if="loginErrorMessage" class="auth-error">{{ loginErrorMessage }}</p>

          <button class="primary-btn" type="submit" :disabled="loginSubmitting">
            {{ loginSubmitting ? 'Signing In...' : 'Sign In' }}
          </button>
        </form>
      </section>

      <template v-else>
        <header class="dashboard-header">
          <div>
            <p class="eyebrow">Onlyboxes / Worker Registry</p>
            <h1>Execution Node Control Panel</h1>
            <p class="subtitle">Real-time monitoring for worker registration and heartbeat health.</p>
          </div>

          <div class="header-actions">
            <button class="primary-btn" type="button" :disabled="creatingWorker" @click="createWorker">
              {{ creatingWorker ? 'Adding...' : 'Add Worker' }}
            </button>
            <button class="ghost-btn" type="button" @click="toggleAutoRefresh">
              {{ autoRefreshEnabled ? 'Auto Refresh: ON' : 'Auto Refresh: OFF' }}
            </button>
            <button class="primary-btn" type="button" :disabled="loading" @click="loadDashboard">
              {{ loading ? 'Refreshing...' : 'Refresh Now' }}
            </button>
            <button class="ghost-btn" type="button" @click="logout">Logout</button>
          </div>
        </header>

        <section class="stat-grid">
          <article class="stat-card">
            <p class="stat-label">Total Workers</p>
            <p class="stat-value">{{ totalWorkers }}</p>
          </article>
          <article class="stat-card online">
            <p class="stat-label">Online</p>
            <p class="stat-value">{{ onlineWorkers }}</p>
          </article>
          <article class="stat-card offline">
            <p class="stat-label">Offline</p>
            <p class="stat-value">{{ offlineWorkers }}</p>
          </article>
          <article class="stat-card stale">
            <p class="stat-label">{{ staleWorkersLabel }}</p>
            <p class="stat-value">{{ staleWorkers }}</p>
          </article>
        </section>

        <section class="board-panel">
          <div class="panel-topbar">
            <div class="tabs">
              <button type="button" :class="['tab-btn', { active: statusFilter === 'all' }]" @click="setStatusFilter('all')">
                All
              </button>
              <button type="button" :class="['tab-btn', { active: statusFilter === 'online' }]" @click="setStatusFilter('online')">
                Online
              </button>
              <button type="button" :class="['tab-btn', { active: statusFilter === 'offline' }]" @click="setStatusFilter('offline')">
                Offline
              </button>
            </div>

            <p class="panel-meta">
              Last refresh:
              <span>{{ refreshedAt ? formatDateTime(refreshedAt.toISOString()) : 'never' }}</span>
            </p>
          </div>

          <p v-if="errorMessage" class="error-banner">{{ errorMessage }}</p>

          <div class="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>Node</th>
                  <th>Runtime</th>
                  <th>Capabilities</th>
                  <th>Labels</th>
                  <th>Status</th>
                  <th>Registered</th>
                  <th>Last Heartbeat</th>
                  <th>Actions</th>
                </tr>
              </thead>
              <tbody>
                <tr v-if="!loading && workerRows.length === 0">
                  <td colspan="8" class="empty-cell">No workers found in current filter.</td>
                </tr>
                <tr v-for="worker in workerRows" :key="worker.node_id">
                  <td>
                    <div class="node-main">{{ worker.node_name || worker.node_id }}</div>
                    <div class="node-sub">{{ worker.node_id }}</div>
                  </td>
                  <td>{{ worker.executor_kind || '--' }}</td>
                  <td>{{ formatCapabilities(worker) }}</td>
                  <td>{{ formatLabels(worker) }}</td>
                  <td>
                    <span :class="['status-pill', worker.status]">{{ worker.status }}</span>
                  </td>
                  <td>{{ formatDateTime(worker.registered_at) }}</td>
                  <td>{{ formatAge(worker.last_seen_at) }}</td>
                  <td>
                    <div class="row-actions">
                      <button
                        type="button"
                        class="ghost-btn small"
                        :disabled="copyingNodeID === worker.node_id || deletingNodeID === worker.node_id"
                        @click="copyWorkerStartupCommand(worker.node_id)"
                      >
                        {{ startupCopyButtonText(worker.node_id) }}
                      </button>
                      <button
                        type="button"
                        class="ghost-btn small danger"
                        :disabled="deletingNodeID === worker.node_id"
                        @click="deleteWorker(worker.node_id)"
                      >
                        {{ deleteWorkerButtonText(worker.node_id) }}
                      </button>
                    </div>
                  </td>
                </tr>
              </tbody>
            </table>
          </div>

          <footer class="panel-footer">
            <span class="footer-meta">Showing {{ footerText }}</span>
            <div class="pager">
              <button type="button" class="ghost-btn small" :disabled="!canPrev || loading" @click="previousPage">Prev</button>
              <span class="page-indicator">Page {{ page }} / {{ totalPages }}</span>
              <button type="button" class="ghost-btn small" :disabled="!canNext || loading" @click="nextPage">Next</button>
            </div>
          </footer>
        </section>
      </template>
    </main>
  </div>
</template>
