<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'

import PageHeader from '@/components/layout/PageHeader.vue'
import AppAlert from '@/components/ui/AppAlert.vue'
import AppButton from '@/components/ui/AppButton.vue'
import AppPagination from '@/components/ui/AppPagination.vue'
import WorkerCreateResultModal from '@/components/workers/WorkerCreateResultModal.vue'
import WorkerStatsGrid from '@/components/workers/WorkerStatsGrid.vue'
import WorkerTypeSelectModal from '@/components/workers/WorkerTypeSelectModal.vue'
import WorkersTable from '@/components/workers/WorkersTable.vue'
import WorkersToolbar from '@/components/workers/WorkersToolbar.vue'
import { useRefreshedAtText } from '@/composables/useRefreshedAtText'
import { useWorkersRouteSync } from '@/composables/useWorkersRouteSync'
import { useAuthStore } from '@/stores/auth'
import { useWorkersStore } from '@/stores/workers'
import type { WorkerStartupCommandResponse, WorkerType } from '@/types/workers'

const workersStore = useWorkersStore()
const authStore = useAuthStore()
const routeSync = useWorkersRouteSync()

const createdWorkerPayload = ref<WorkerStartupCommandResponse | null>(null)
const showWorkerTypeModal = ref(false)
const showDetails = ref(false)

const refreshedAtText = useRefreshedAtText(computed(() => workersStore.refreshedAt))

const createButtonText = computed(() => {
  if (workersStore.creatingWorker) {
    return authStore.isAdmin ? 'Adding...' : 'Creating...'
  }
  return authStore.isAdmin ? 'Add Worker' : 'Create Worker-Sys'
})

function handleVisibilityChange(): void {
  workersStore.onPageVisibilityChange()
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

routeSync.start()

onMounted(async () => {
  routeSync.applyRouteToStore({ load: false })
  await workersStore.loadDashboard()
  workersStore.startAutoRefresh()
  document.addEventListener('visibilitychange', handleVisibilityChange)
})

onBeforeUnmount(() => {
  workersStore.teardown()
  document.removeEventListener('visibilitychange', handleVisibilityChange)
  showWorkerTypeModal.value = false
})
</script>

<template>
  <div class="grid gap-6">
    <PageHeader
      eyebrow="Onlyboxes / Worker Registry"
      title="Execution Node Control Panel"
      :loading="workersStore.loading"
      :refreshed-at-text="refreshedAtText"
      @refresh="workersStore.loadDashboard"
    >
      <template #subtitle>
        Real-time monitoring for worker registration and heartbeat health.
      </template>
      <template #actions>
        <AppButton
          variant="primary"
          icon="plus"
          data-testid="create-worker-button"
          :loading="workersStore.creatingWorker"
          @click="handleAddWorker"
        >
          {{ createButtonText }}
        </AppButton>
      </template>
    </PageHeader>

    <WorkerStatsGrid
      :total-workers="workersStore.totalWorkers"
      :online-workers="workersStore.onlineWorkers"
      :offline-workers="workersStore.offlineWorkers"
      :stale-workers="workersStore.staleWorkers"
      :active-sessions="workersStore.activeSessions"
      :stale-workers-label="workersStore.staleWorkersLabel"
    />

    <section
      class="ui-rise overflow-hidden rounded-lg border border-stroke bg-surface shadow-card max-[620px]:rounded-default"
    >
      <WorkersToolbar
        :status-filter="workersStore.statusFilter"
        :show-details="showDetails"
        @set-status="workersStore.setFilter"
        @update:show-details="showDetails = $event"
      />

      <AppAlert v-if="workersStore.errorMessage" tone="error" with-icon class="mx-6 mt-4">
        {{ workersStore.errorMessage }}
      </AppAlert>

      <WorkersTable
        :worker-rows="workersStore.workerRows"
        :inflight-workers="workersStore.inflightData.workers"
        :show-details="showDetails"
        :loading="workersStore.loading"
        :deleting-node-id="workersStore.deletingNodeID"
        @delete-worker="workersStore.deleteWorker"
      />

      <div class="border-t border-stroke bg-surface-soft">
        <AppPagination
          :page="workersStore.page"
          :total-pages="workersStore.totalPages"
          :total="workersStore.total"
          :page-size="workersStore.pageSize"
          :loading="workersStore.loading"
          item-label="workers"
          @prev="workersStore.previousPage"
          @next="workersStore.nextPage"
        />
      </div>
    </section>

    <WorkerTypeSelectModal
      :open="showWorkerTypeModal"
      :loading="workersStore.creatingWorker"
      @close="closeWorkerTypeModal"
      @select="handleCreateWorker"
    />

    <WorkerCreateResultModal :payload="createdWorkerPayload" @close="createdWorkerPayload = null" />
  </div>
</template>
