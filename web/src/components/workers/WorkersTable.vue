<script setup lang="ts">
import { computed } from 'vue'

import AppBadge from '@/components/ui/AppBadge.vue'
import AppButton from '@/components/ui/AppButton.vue'
import { formatAge, formatDateTime } from '@/utils/datetime'
import type { WorkerInflightItem, WorkerItem } from '@/types/workers'

const props = defineProps<{
  workerRows: WorkerItem[]
  inflightWorkers: WorkerInflightItem[]
  showDetails: boolean
  loading: boolean
  deletingNodeId: string
}>()

const emit = defineEmits<{
  deleteWorker: [nodeID: string]
}>()

type InflightCapability = WorkerInflightItem['capabilities'][number]

const columns = computed(() => [
  { key: 'node', label: 'Node' },
  { key: 'runtime', label: 'Runtime' },
  { key: 'capabilities', label: 'Capabilities' },
  { key: 'sessions', label: 'Active Sessions' },
  ...(props.showDetails ? [{ key: 'labels', label: 'Labels' }] : []),
  { key: 'status', label: 'Status' },
  { key: 'heartbeat', label: 'Registered / Last Heartbeat' },
  { key: 'actions', label: 'Actions' },
])

const inflightByNode = computed(() => {
  const out = new Map<
    string,
    { worker: WorkerInflightItem; capabilities: Map<string, InflightCapability> }
  >()
  for (const worker of props.inflightWorkers) {
    const capabilities = new Map<string, InflightCapability>()
    for (const capability of worker.capabilities) {
      const normalized = capability.name.trim().toLowerCase()
      if (normalized) {
        capabilities.set(normalized, capability)
      }
    }
    out.set(worker.node_id, { worker, capabilities })
  }
  return out
})

function capabilityLoad(nodeId: string, capabilityName: string): InflightCapability | null {
  const normalized = capabilityName.trim().toLowerCase()
  if (!normalized) {
    return null
  }
  return inflightByNode.value.get(nodeId)?.capabilities.get(normalized) ?? null
}

function activeSessionCount(nodeId: string): string {
  const count = inflightByNode.value.get(nodeId)?.worker.active_session_count
  return count === undefined ? '--' : String(count)
}

function loadTone(load: InflightCapability): 'stale' | 'soft' | 'neutral' {
  if (load.inflight >= load.max_inflight) {
    return 'stale'
  }
  return load.inflight > 0 ? 'soft' : 'neutral'
}

const showSkeleton = computed(() => props.loading && props.workerRows.length === 0)
</script>

<template>
  <div class="overflow-auto">
    <table class="w-full min-w-[1040px] border-collapse">
      <thead>
        <tr>
          <th v-for="column in columns" :key="column.key" scope="col" class="ui-th">
            {{ column.label }}
          </th>
        </tr>
      </thead>
      <tbody>
        <template v-if="showSkeleton">
          <tr v-for="row in 3" :key="`skeleton-${row}`" aria-hidden="true">
            <td v-for="column in columns" :key="column.key" class="ui-td">
              <span class="ui-skeleton block h-4 w-[70%] rounded-default" />
            </td>
          </tr>
        </template>

        <tr v-else-if="workerRows.length === 0">
          <td :colspan="columns.length" class="ui-td text-center text-secondary">
            <span class="block py-8">No workers found in current filter.</span>
          </td>
        </tr>

        <tr
          v-for="worker in workerRows"
          :key="worker.node_id"
          class="transition-colors duration-200 hover:bg-surface-soft"
        >
          <td class="ui-td">
            <div class="font-semibold">{{ worker.node_name || worker.node_id }}</div>
            <div v-if="showDetails" class="mt-1 font-mono text-xs text-secondary">
              {{ worker.node_id }}
            </div>
          </td>

          <td class="ui-td">
            <div>{{ worker.executor_kind || '--' }}</div>
            <div class="mt-1 font-mono text-xs text-secondary">
              version: {{ worker.version || '--' }}
            </div>
          </td>

          <td class="ui-td">
            <div
              v-if="worker.capabilities && worker.capabilities.length > 0"
              class="flex flex-col items-start gap-1.5"
            >
              <AppBadge
                v-for="capability in worker.capabilities"
                :key="capability.name"
                class="capability-badge"
                mono
              >
                {{ capability.name }}
                <AppBadge
                  v-if="capabilityLoad(worker.node_id, capability.name)"
                  :tone="loadTone(capabilityLoad(worker.node_id, capability.name)!)"
                  class="px-1 py-px text-[10px]"
                  mono
                >
                  {{ capabilityLoad(worker.node_id, capability.name)!.inflight }}/{{
                    capabilityLoad(worker.node_id, capability.name)!.max_inflight
                  }}
                </AppBadge>
              </AppBadge>
            </div>
            <span v-else>--</span>
          </td>

          <td class="ui-td">
            <span class="font-mono">{{ activeSessionCount(worker.node_id) }}</span>
          </td>

          <td v-if="showDetails" class="ui-td">
            <div
              v-if="worker.labels && Object.keys(worker.labels).length > 0"
              class="worker-label-list flex max-h-24 flex-col gap-1 overflow-y-auto pr-1"
            >
              <AppBadge
                v-for="(value, key) in worker.labels"
                :key="`${worker.node_id}-${key}`"
                class="worker-label-badge"
                mono
              >
                {{ key }}={{ value }}
              </AppBadge>
            </div>
            <span v-else>--</span>
          </td>

          <td class="ui-td">
            <AppBadge :tone="worker.status === 'online' ? 'online' : 'offline'" class="capitalize">
              {{ worker.status }}
            </AppBadge>
          </td>

          <td class="ui-td">
            <div>{{ formatDateTime(worker.registered_at) }}</div>
            <div class="mt-1 font-mono text-xs text-secondary">
              {{ formatAge(worker.last_seen_at) }}
            </div>
          </td>

          <td class="ui-td">
            <AppButton
              variant="danger"
              size="sm"
              icon="trash"
              :loading="deletingNodeId === worker.node_id"
              @click="emit('deleteWorker', worker.node_id)"
            >
              {{ deletingNodeId === worker.node_id ? 'Deleting...' : 'Delete' }}
            </AppButton>
          </td>
        </tr>
      </tbody>
    </table>
  </div>
</template>
