<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'

import ErrorBanner from '@/components/common/ErrorBanner.vue'
import ConsoleHeader from '@/components/dashboard/ConsoleHeader.vue'
import PaginationBar from '@/components/dashboard/PaginationBar.vue'
import StatsGrid from '@/components/dashboard/StatsGrid.vue'
import WorkerCreateResultModal from '@/components/dashboard/WorkerCreateResultModal.vue'
import WorkersTable from '@/components/dashboard/WorkersTable.vue'
import WorkersToolbar from '@/components/dashboard/WorkersToolbar.vue'
import { useDismissibleMenu } from '@/composables/useDismissibleMenu'
import { useAuthStore } from '@/stores/auth'
import { useWorkersStore } from '@/stores/workers'
import type { WorkerStartupCommandResponse, WorkerStatus, WorkerType } from '@/types/workers'

const workersStore = useWorkersStore()
const authStore = useAuthStore()
const route = useRoute()
const router = useRouter()
const createdWorkerPayload = ref<WorkerStartupCommandResponse | null>(null)
const refreshControlRef = ref<HTMLElement | null>(null)
const showRefreshControlMenu = ref(false)
const selectedWorkerType = ref<WorkerType>('normal')

function parseStatus(raw: unknown): WorkerStatus {
  return raw === 'online' || raw === 'offline' || raw === 'all' ? raw : 'all'
}

function parsePage(raw: unknown): number {
  if (typeof raw !== 'string') {
    return 1
  }
  const parsed = Number.parseInt(raw, 10)
  if (!Number.isFinite(parsed) || parsed < 1) {
    return 1
  }
  return parsed
}

function syncStoreFromRoute(load: boolean): void {
  const targetStatus = parseStatus(route.query.status)
  const targetPage = parsePage(route.query.page)

  const statusChanged = targetStatus !== workersStore.statusFilter
  const pageChanged = targetPage !== workersStore.page
  if (!statusChanged && !pageChanged) {
    return
  }

  workersStore.setFilter(targetStatus, { load: false })
  workersStore.setPage(targetPage, { load: false })

  if (load) {
    void workersStore.loadDashboard()
  }
}

function syncRouteFromStore(): void {
  const currentStatus = parseStatus(route.query.status)
  const currentPage = parsePage(route.query.page)

  if (currentStatus === workersStore.statusFilter && currentPage === workersStore.page) {
    return
  }

  const query: Record<string, string> = {}
  if (workersStore.statusFilter !== 'all') {
    query.status = workersStore.statusFilter
  }
  if (workersStore.page > 1) {
    query.page = String(workersStore.page)
  }

  void router.replace({
    path: '/workers',
    query,
  })
}

const refreshedAtText = computed(() => {
  if (!workersStore.refreshedAt) {
    return 'never'
  }
  return workersStore.formatDateTime(workersStore.refreshedAt.toISOString())
})

const refreshControlButtonText = computed(() => {
  const statusText = workersStore.autoRefreshEnabled ? 'Auto ON' : 'Auto OFF'
  if (workersStore.loading) {
    return `Refreshing · ${statusText}`
  }
  return `Refresh Controls · ${statusText}`
})

function handleVisibilityChange(): void {
  workersStore.onPageVisibilityChange()
}

async function handleRefresh(): Promise<void> {
  await workersStore.loadDashboard()
}

function toggleRefreshControlMenu(): void {
  showRefreshControlMenu.value = !showRefreshControlMenu.value
}

function closeRefreshControlMenu(): void {
  showRefreshControlMenu.value = false
}

async function handleRefreshFromMenu(): Promise<void> {
  closeRefreshControlMenu()
  await handleRefresh()
}

function handleToggleAutoRefreshFromMenu(): void {
  workersStore.toggleAutoRefresh()
  closeRefreshControlMenu()
}

useDismissibleMenu({
  containerRef: refreshControlRef,
  isOpen: showRefreshControlMenu,
  onClose: closeRefreshControlMenu,
})

async function handleAddWorker(): Promise<void> {
  const workerType: WorkerType = authStore.isAdmin ? selectedWorkerType.value : 'worker-sys'
  const payload = await workersStore.createWorker(workerType)
  if (!payload) {
    return
  }
  createdWorkerPayload.value = payload
}

function closeWorkerCreateResultModal(): void {
  createdWorkerPayload.value = null
}

const createButtonText = computed(() => {
  if (workersStore.creatingWorker) {
    return authStore.isAdmin ? 'Adding...' : 'Creating...'
  }
  if (!authStore.isAdmin) {
    return 'Create Worker-Sys'
  }
  return selectedWorkerType.value === 'normal' ? 'Add Normal Worker' : 'Add Worker-Sys'
})

watch(
  () => route.query,
  () => {
    syncStoreFromRoute(true)
  },
)

watch(
  () => [workersStore.statusFilter, workersStore.page],
  () => {
    syncRouteFromStore()
  },
)

onMounted(async () => {
  syncStoreFromRoute(false)
  await workersStore.loadDashboard()
  workersStore.startAutoRefresh()
  document.addEventListener('visibilitychange', handleVisibilityChange)
})

onBeforeUnmount(() => {
  workersStore.teardown()
  document.removeEventListener('visibilitychange', handleVisibilityChange)
  closeRefreshControlMenu()
})
</script>

<template>
  <main class="relative z-2 mx-auto w-[min(1240px,100%)] grid gap-6">
    <ConsoleHeader
      eyebrow="Onlyboxes / Worker Registry"
      title="Execution Node Control Panel"
      :loading="workersStore.loading"
      :refreshed-at-text="refreshedAtText"
      hide-refresh
    >
      <template #subtitle>
        Real-time monitoring for worker registration and heartbeat health.
      </template>
      <template #actions>
        <label
          v-if="authStore.isAdmin"
          class="inline-flex items-center gap-2 rounded-md border border-stroke bg-surface px-2.5 py-1.5 text-sm text-secondary"
        >
          <span>Type</span>
          <select
            v-model="selectedWorkerType"
            data-testid="create-worker-type-select"
            class="rounded border border-stroke bg-surface px-2 py-1 text-sm text-primary outline-none focus:border-stroke-hover"
            :disabled="workersStore.creatingWorker"
          >
            <option value="normal">normal</option>
            <option value="worker-sys">worker-sys</option>
          </select>
        </label>
        <button
          data-testid="create-worker-button"
          class="rounded-md px-3.5 py-2 text-sm font-medium h-9 inline-flex items-center justify-center text-white bg-accent border border-accent transition-all duration-200 hover:not-disabled:bg-[#333] hover:not-disabled:border-[#333] disabled:cursor-not-allowed disabled:opacity-50"
          type="button"
          :disabled="workersStore.creatingWorker"
          @click="handleAddWorker"
        >
          {{ createButtonText }}
        </button>
        <div ref="refreshControlRef" class="relative">
          <button
            class="rounded-md px-3.5 py-2 text-sm font-medium h-9 inline-flex items-center justify-center text-primary bg-surface border border-stroke transition-all duration-200 hover:not-disabled:border-stroke-hover hover:not-disabled:bg-surface-soft disabled:cursor-not-allowed disabled:opacity-50"
            type="button"
            aria-haspopup="menu"
            :aria-expanded="showRefreshControlMenu"
            @click="toggleRefreshControlMenu"
          >
            {{ refreshControlButtonText }}
          </button>

          <div
            v-if="showRefreshControlMenu"
            role="menu"
            aria-label="Refresh controls"
            class="absolute right-0 top-[calc(100%+8px)] z-20 w-[230px] rounded-default border border-stroke bg-surface shadow-card p-1.5 grid gap-1"
          >
            <button
              type="button"
              role="menuitem"
              class="rounded-default border border-transparent px-3 py-2 text-left text-sm text-primary transition-all duration-200 hover:border-stroke hover:bg-surface-soft disabled:cursor-not-allowed disabled:opacity-50"
              :disabled="workersStore.loading"
              @click="handleRefreshFromMenu"
            >
              Refresh Now
            </button>
            <button
              type="button"
              role="menuitemcheckbox"
              :aria-checked="workersStore.autoRefreshEnabled"
              class="rounded-default border border-transparent px-3 py-2 text-left text-sm text-primary transition-all duration-200 hover:border-stroke hover:bg-surface-soft"
              @click="handleToggleAutoRefreshFromMenu"
            >
              {{ workersStore.autoRefreshEnabled ? 'Auto Refresh: ON' : 'Auto Refresh: OFF' }}
            </button>
          </div>
        </div>
      </template>
    </ConsoleHeader>

    <StatsGrid
      :total-workers="workersStore.totalWorkers"
      :online-workers="workersStore.onlineWorkers"
      :offline-workers="workersStore.offlineWorkers"
      :stale-workers="workersStore.staleWorkers"
      :stale-workers-label="workersStore.staleWorkersLabel"
    />

    <section
      class="border border-stroke rounded-lg bg-surface shadow-card overflow-hidden animate-[rise-in_620ms_ease-out] max-[620px]:rounded-default"
    >
      <WorkersToolbar
        :status-filter="workersStore.statusFilter"
        @set-status="workersStore.setFilter"
      />

      <ErrorBanner
        v-if="workersStore.errorMessage"
        :message="workersStore.errorMessage"
        class="mx-6 mt-4"
      />

      <WorkersTable
        :worker-rows="workersStore.workerRows"
        :inflight-workers="workersStore.inflightData.workers"
        :loading="workersStore.loading"
        :deleting-node-id="workersStore.deletingNodeID"
        :format-capabilities="workersStore.formatCapabilities"
        :format-date-time="workersStore.formatDateTime"
        :format-age="workersStore.formatAge"
        :delete-worker-button-text="workersStore.deleteWorkerButtonText"
        @delete-worker="workersStore.deleteWorker"
      />

      <PaginationBar
        :footer-text="workersStore.footerText"
        :page="workersStore.page"
        :total-pages="workersStore.totalPages"
        :can-prev="workersStore.canPrev"
        :can-next="workersStore.canNext"
        :loading="workersStore.loading"
        @prev="workersStore.previousPage"
        @next="workersStore.nextPage"
      />
    </section>

    <WorkerCreateResultModal
      :payload="createdWorkerPayload"
      @close="closeWorkerCreateResultModal"
    />
  </main>
</template>
