<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'

import ErrorBanner from '@/components/common/ErrorBanner.vue'
import DashboardHeader from '@/components/dashboard/DashboardHeader.vue'
import PaginationBar from '@/components/dashboard/PaginationBar.vue'
import StatsGrid from '@/components/dashboard/StatsGrid.vue'
import TrustedTokensPanel from '@/components/dashboard/TrustedTokensPanel.vue'
import WorkerCreateResultModal from '@/components/dashboard/WorkerCreateResultModal.vue'
import CreateAccountModal from '@/components/dashboard/CreateAccountModal.vue'
import WorkersTable from '@/components/dashboard/WorkersTable.vue'
import WorkersToolbar from '@/components/dashboard/WorkersToolbar.vue'
import { useAuthStore } from '@/stores/auth'
import { useTokensStore } from '@/stores/tokens'
import { useWorkersStore } from '@/stores/workers'
import type { WorkerStartupCommandResponse, WorkerStatus } from '@/types/workers'

const authStore = useAuthStore()
const tokensStore = useTokensStore()
const workersStore = useWorkersStore()
const route = useRoute()
const router = useRouter()
const createdWorkerPayload = ref<WorkerStartupCommandResponse | null>(null)
const showCreateAccountModal = ref(false)

const showCreateAccountPanel = computed(() => authStore.isAdmin && authStore.registrationEnabled)

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

async function handleLogout(): Promise<void> {
  await authStore.logout()
  workersStore.teardown()
  tokensStore.teardown()
  tokensStore.reset()
  await router.replace('/login')
}

async function handleRefresh(): Promise<void> {
  await Promise.all([workersStore.loadDashboard(), tokensStore.loadTokens()])
}

async function handleAddWorker(): Promise<void> {
  const payload = await workersStore.createWorker()
  if (!payload) {
    return
  }
  createdWorkerPayload.value = payload
}

function closeWorkerCreateResultModal(): void {
  createdWorkerPayload.value = null
}

function openCreateAccountModal(): void {
  showCreateAccountModal.value = true
}

function closeCreateAccountModal(): void {
  showCreateAccountModal.value = false
}

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
  await Promise.all([workersStore.loadDashboard(), tokensStore.loadTokens()])
  workersStore.startAutoRefresh()
  document.addEventListener('visibilitychange', handleVisibilityChange)
})

onBeforeUnmount(() => {
  workersStore.teardown()
  tokensStore.teardown()
  document.removeEventListener('visibilitychange', handleVisibilityChange)
})
</script>

<template>
  <main class="relative z-2 mx-auto w-[min(1240px,100%)] grid gap-6">
    <DashboardHeader
      :creating-worker="workersStore.creatingWorker"
      :auto-refresh-enabled="workersStore.autoRefreshEnabled"
      :loading="workersStore.loading"
      :show-create-account="showCreateAccountPanel"
      @add-worker="handleAddWorker"
      @toggle-auto-refresh="workersStore.toggleAutoRefresh"
      @refresh="handleRefresh"
      @logout="handleLogout"
      @create-account="openCreateAccountModal"
    />

    <StatsGrid
      :total-workers="workersStore.totalWorkers"
      :online-workers="workersStore.onlineWorkers"
      :offline-workers="workersStore.offlineWorkers"
      :stale-workers="workersStore.staleWorkers"
      :stale-workers-label="workersStore.staleWorkersLabel"
    />

    <TrustedTokensPanel
      :tokens="tokensStore.trustedTokens"
      :creating-token="tokensStore.creatingTrustedToken"
      :deleting-token-id="tokensStore.deletingTrustedTokenID"
      :delete-button-text="tokensStore.trustedTokenDeleteButtonText"
      :create-token="tokensStore.createTrustedToken"
      :format-date-time="tokensStore.formatDateTime"
      @delete-token="tokensStore.deleteTrustedToken"
    />

    <section class="border border-stroke rounded-lg bg-surface shadow-card overflow-hidden animate-[rise-in_620ms_ease-out] max-[620px]:rounded-default">
      <WorkersToolbar
        :status-filter="workersStore.statusFilter"
        :refreshed-at-text="refreshedAtText"
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
        :format-labels="workersStore.formatLabels"
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

    <CreateAccountModal v-if="showCreateAccountModal" @close="closeCreateAccountModal" />
  </main>
</template>
