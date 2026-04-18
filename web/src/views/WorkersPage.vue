<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'

import ErrorBanner from '@/components/common/ErrorBanner.vue'
import ConsoleHeader from '@/components/dashboard/ConsoleHeader.vue'
import PaginationBar from '@/components/dashboard/PaginationBar.vue'
import StatsGrid from '@/components/dashboard/StatsGrid.vue'
import WorkerCreateResultModal from '@/components/dashboard/WorkerCreateResultModal.vue'
import WorkerTypeSelectModal from '@/components/dashboard/WorkerTypeSelectModal.vue'
import WorkersTable from '@/components/dashboard/WorkersTable.vue'
import WorkersToolbar from '@/components/dashboard/WorkersToolbar.vue'
import { useAuthStore } from '@/stores/auth'
import { useWorkersStore } from '@/stores/workers'
import type { WorkerStartupCommandResponse, WorkerStatus, WorkerType } from '@/types/workers'

const workersStore = useWorkersStore()
const authStore = useAuthStore()
const route = useRoute()
const router = useRouter()
const createdWorkerPayload = ref<WorkerStartupCommandResponse | null>(null)
const showWorkerTypeModal = ref(false)
const showDetails = ref(false)

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

function handleVisibilityChange(): void {
  workersStore.onPageVisibilityChange()
}

async function handleRefresh(): Promise<void> {
  await workersStore.loadDashboard()
}

function handleShowDetailsChange(value: boolean): void {
  showDetails.value = value
}

async function handleAddWorker(): Promise<void> {
  if (authStore.isAdmin) {
    showWorkerTypeModal.value = true
    return
  }

  await handleCreateWorker('worker-sys')
}

async function handleCreateWorker(workerType: WorkerType): Promise<void> {
  const payload = await workersStore.createWorker(workerType)
  if (!payload) {
    return
  }
  showWorkerTypeModal.value = false
  createdWorkerPayload.value = payload
}

function closeWorkerTypeModal(): void {
  if (workersStore.creatingWorker) {
    return
  }
  showWorkerTypeModal.value = false
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
  return 'Add Worker'
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
  closeWorkerTypeModal()
})
</script>

<template>
  <main class="relative z-2 mx-auto w-[min(1240px,100%)] grid gap-6">
    <ConsoleHeader
      eyebrow="Onlyboxes / Worker Registry"
      title="Execution Node Control Panel"
      :loading="workersStore.loading"
      :refreshed-at-text="refreshedAtText"
      @refresh="handleRefresh"
    >
      <template #subtitle>
        Real-time monitoring for worker registration and heartbeat health.
      </template>
      <template #actions>
        <button
          data-testid="create-worker-button"
          class="ui-btn-primary rounded-md px-3.5 py-2 text-sm font-medium h-9 inline-flex items-center justify-center border transition-all duration-200 disabled:cursor-not-allowed disabled:opacity-50"
          type="button"
          :disabled="workersStore.creatingWorker"
          @click="handleAddWorker"
        >
          {{ createButtonText }}
        </button>
      </template>
    </ConsoleHeader>

    <StatsGrid
      :total-workers="workersStore.totalWorkers"
      :online-workers="workersStore.onlineWorkers"
      :offline-workers="workersStore.offlineWorkers"
      :stale-workers="workersStore.staleWorkers"
      :active-sessions="workersStore.activeSessions"
      :stale-workers-label="workersStore.staleWorkersLabel"
    />

    <section
      class="border border-stroke rounded-lg bg-surface shadow-card overflow-hidden animate-[rise-in_620ms_ease-out] max-[620px]:rounded-default"
    >
      <WorkersToolbar
        :status-filter="workersStore.statusFilter"
        :show-details="showDetails"
        @set-status="workersStore.setFilter"
        @update-show-details="handleShowDetailsChange"
      />

      <ErrorBanner
        v-if="workersStore.errorMessage"
        :message="workersStore.errorMessage"
        class="mx-6 mt-4"
      />

      <WorkersTable
        :worker-rows="workersStore.workerRows"
        :inflight-workers="workersStore.inflightData.workers"
        :show-details="showDetails"
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

    <WorkerTypeSelectModal
      :open="showWorkerTypeModal"
      :loading="workersStore.creatingWorker"
      @close="closeWorkerTypeModal"
      @select="handleCreateWorker"
    />

    <WorkerCreateResultModal
      :payload="createdWorkerPayload"
      @close="closeWorkerCreateResultModal"
    />
  </main>
</template>
