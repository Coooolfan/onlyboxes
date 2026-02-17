<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'

type WorkerStatus = 'all' | 'online' | 'offline'

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

const pageSize = 25
const staleAfterDefaultSec = 30
const statusFilter = ref<WorkerStatus>('all')
const page = ref(1)
const loading = ref(false)
const errorMessage = ref('')
const refreshedAt = ref<Date | null>(null)
const autoRefreshEnabled = ref(true)
const autoRefreshMs = 5000

const dashboardStats = ref<WorkerStatsResponse>({
  total: 0,
  online: 0,
  offline: 0,
  stale: 0,
  stale_after_sec: staleAfterDefaultSec,
  generated_at: '',
})
const currentList = ref<WorkerListResponse | null>(null)

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
  } catch (error) {
    if (isAbortError(error) || requestSerial !== loadRequestSerial) {
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
    signal,
  })

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
    signal,
  })

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
    if (!autoRefreshEnabled.value || loading.value) {
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
})
</script>

<template>
  <div class="dashboard-shell">
    <div class="gradient-orb orb-left" />
    <div class="gradient-orb orb-right" />

    <main class="dashboard-content">
      <header class="dashboard-header">
        <div>
          <p class="eyebrow">Onlyboxes / Worker Registry</p>
          <h1>Execution Node Control Panel</h1>
          <p class="subtitle">Real-time monitoring for worker registration and heartbeat health.</p>
        </div>

        <div class="header-actions">
          <button class="ghost-btn" type="button" @click="toggleAutoRefresh">
            {{ autoRefreshEnabled ? 'Auto Refresh: ON' : 'Auto Refresh: OFF' }}
          </button>
          <button class="primary-btn" type="button" :disabled="loading" @click="loadDashboard">
            {{ loading ? 'Refreshing...' : 'Refresh Now' }}
          </button>
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
              </tr>
            </thead>
            <tbody>
              <tr v-if="!loading && workerRows.length === 0">
                <td colspan="7" class="empty-cell">No workers found in current filter.</td>
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
    </main>
  </div>
</template>
