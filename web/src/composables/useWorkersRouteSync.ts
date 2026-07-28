import { watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'

import { useWorkersStore } from '@/stores/workers'
import type { WorkerStatus } from '@/types/workers'

function parseStatus(raw: unknown): WorkerStatus {
  return raw === 'online' || raw === 'offline' || raw === 'all' ? raw : 'all'
}

function parsePage(raw: unknown): number {
  if (typeof raw !== 'string') {
    return 1
  }
  const parsed = Number.parseInt(raw, 10)
  return Number.isFinite(parsed) && parsed >= 1 ? parsed : 1
}

/**
 * Keeps the worker list filter and page number mirrored between the store and the
 * URL query, so the current view stays shareable and reload-safe.
 */
export function useWorkersRouteSync() {
  const route = useRoute()
  const router = useRouter()
  const workersStore = useWorkersStore()

  function applyRouteToStore(options: { load: boolean }): void {
    const targetStatus = parseStatus(route.query.status)
    const targetPage = parsePage(route.query.page)

    const changed = targetStatus !== workersStore.statusFilter || targetPage !== workersStore.page
    if (!changed) {
      return
    }

    workersStore.setFilter(targetStatus, { load: false })
    workersStore.setPage(targetPage, { load: false })

    if (options.load) {
      void workersStore.loadDashboard()
    }
  }

  function applyStoreToRoute(): void {
    const sameStatus = parseStatus(route.query.status) === workersStore.statusFilter
    const samePage = parsePage(route.query.page) === workersStore.page
    if (sameStatus && samePage) {
      return
    }

    const query: Record<string, string> = {}
    if (workersStore.statusFilter !== 'all') {
      query.status = workersStore.statusFilter
    }
    if (workersStore.page > 1) {
      query.page = String(workersStore.page)
    }

    void router.replace({ path: '/workers', query })
  }

  function start(): void {
    watch(
      () => route.query,
      () => {
        applyRouteToStore({ load: true })
      },
    )

    watch(() => [workersStore.statusFilter, workersStore.page], applyStoreToRoute)
  }

  return { applyRouteToStore, start }
}
