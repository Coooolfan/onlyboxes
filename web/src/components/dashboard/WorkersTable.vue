<script setup lang="ts">
import { computed } from 'vue'

import type { WorkerItem, WorkerInflightItem } from '@/types/workers'

const props = defineProps<{
  workerRows: WorkerItem[]
  inflightWorkers: WorkerInflightItem[]
  showDetails: boolean
  loading: boolean
  deletingNodeId: string
  formatCapabilities: (worker: WorkerItem) => string
  formatDateTime: (value: string) => string
  formatAge: (value: string) => string
  deleteWorkerButtonText: (nodeID: string) => string
}>()

const emit = defineEmits<{
  deleteWorker: [nodeID: string]
}>()

type InflightCapability = WorkerInflightItem['capabilities'][number]
type InflightWorker = WorkerInflightItem

function normalizeCapabilityName(name: string): string {
  return name.trim().toLowerCase()
}

const inflightByWorker = computed(() => {
  const out = new Map<string, Map<string, InflightCapability>>()
  for (const worker of props.inflightWorkers) {
    const capabilities = new Map<string, InflightCapability>()
    for (const capability of worker.capabilities) {
      const normalized = normalizeCapabilityName(capability.name)
      if (!normalized) {
        continue
      }
      capabilities.set(normalized, capability)
    }
    out.set(worker.node_id, capabilities)
  }
  return out
})

const inflightWorkerByNode = computed(() => {
  const out = new Map<string, InflightWorker>()
  for (const worker of props.inflightWorkers) {
    out.set(worker.node_id, worker)
  }
  return out
})

function getInflight(nodeId: string, capName: string): InflightCapability | null {
  const normalized = normalizeCapabilityName(capName)
  if (!normalized) {
    return null
  }
  return inflightByWorker.value.get(nodeId)?.get(normalized) ?? null
}

function getInflightWorker(nodeId: string): InflightWorker | null {
  return inflightWorkerByNode.value.get(nodeId) ?? null
}
</script>

<template>
  <div class="overflow-auto">
    <table class="w-full border-collapse min-w-[1040px]">
      <thead>
        <tr>
          <th
            class="text-left px-6 py-4 border-b border-stroke text-[13px] font-medium text-secondary bg-surface-soft sticky top-0 z-1 align-middle"
          >
            Node
          </th>
          <th
            class="text-left px-6 py-4 border-b border-stroke text-[13px] font-medium text-secondary bg-surface-soft sticky top-0 z-1 align-middle"
          >
            Runtime
          </th>
          <th
            class="text-left px-6 py-4 border-b border-stroke text-[13px] font-medium text-secondary bg-surface-soft sticky top-0 z-1 align-middle"
          >
            Capabilities
          </th>
          <th
            class="text-left px-6 py-4 border-b border-stroke text-[13px] font-medium text-secondary bg-surface-soft sticky top-0 z-1 align-middle"
          >
            Active Sessions
          </th>
          <th
            v-if="showDetails"
            class="text-left px-6 py-4 border-b border-stroke text-[13px] font-medium text-secondary bg-surface-soft sticky top-0 z-1 align-middle"
          >
            Labels
          </th>
          <th
            class="text-left px-6 py-4 border-b border-stroke text-[13px] font-medium text-secondary bg-surface-soft sticky top-0 z-1 align-middle"
          >
            Status
          </th>
          <th
            class="text-left px-6 py-4 border-b border-stroke text-[13px] font-medium text-secondary bg-surface-soft sticky top-0 z-1 align-middle"
          >
            Registered / Last Heartbeat
          </th>
          <th
            class="text-left px-6 py-4 border-b border-stroke text-[13px] font-medium text-secondary bg-surface-soft sticky top-0 z-1 align-middle"
          >
            Actions
          </th>
        </tr>
      </thead>
      <tbody>
        <tr v-if="!loading && workerRows.length === 0">
          <td
            :colspan="showDetails ? 8 : 7"
            class="text-secondary text-center px-6 py-12 text-sm border-b border-stroke align-middle"
          >
            No workers found in current filter.
          </td>
        </tr>
        <tr
          v-for="worker in workerRows"
          :key="worker.node_id"
          class="transition-colors duration-200 hover:bg-surface-soft"
        >
          <td class="text-left px-6 py-4 border-b border-stroke text-sm text-primary align-middle">
            <div class="font-semibold">{{ worker.node_name || worker.node_id }}</div>
            <div v-if="showDetails" class="mt-1 text-secondary font-mono text-xs">
              {{ worker.node_id }}
            </div>
          </td>
          <td class="text-left px-6 py-4 border-b border-stroke text-sm text-primary align-middle">
            <div>{{ worker.executor_kind || '--' }}</div>
            <div class="mt-1 text-secondary font-mono text-xs">
              version: {{ worker.version || '--' }}
            </div>
          </td>
          <td class="text-left px-6 py-4 border-b border-stroke text-sm text-primary align-middle">
            <div
              class="flex flex-col items-start gap-1.5"
              v-if="worker.capabilities && worker.capabilities.length > 0"
            >
              <span
                class="ui-badge-soft capability-badge inline-flex items-center justify-center whitespace-nowrap px-2 py-1 border rounded-default font-mono text-[11px] gap-1.5"
                v-for="cap in worker.capabilities"
                :key="cap.name"
              >
                {{ cap.name }}
                <span
                  v-if="getInflight(worker.node_id, cap.name)"
                  :class="[
                    'text-[10px] px-1 py-px rounded-[4px] border',
                    getInflight(worker.node_id, cap.name)!.inflight >=
                    getInflight(worker.node_id, cap.name)!.max_inflight
                      ? 'ui-status-badge-stale'
                      : getInflight(worker.node_id, cap.name)!.inflight > 0
                        ? 'text-primary border-stroke-hover bg-surface-soft'
                        : 'text-tertiary bg-surface border-stroke',
                  ]"
                >
                  {{ getInflight(worker.node_id, cap.name)!.inflight }}/{{
                    getInflight(worker.node_id, cap.name)!.max_inflight
                  }}
                </span>
              </span>
            </div>
            <span v-else>--</span>
          </td>
          <td class="text-left px-6 py-4 border-b border-stroke text-sm text-primary align-middle">
            <span class="font-mono">
              {{ getInflightWorker(worker.node_id)?.active_session_count ?? '--' }}
            </span>
          </td>
          <td
            v-if="showDetails"
            class="text-left px-6 py-4 border-b border-stroke text-sm text-primary align-middle"
          >
            <div
              v-if="worker.labels && Object.keys(worker.labels).length > 0"
              class="worker-label-list flex max-h-24 flex-col gap-1 overflow-y-auto pr-1"
            >
              <span
                v-for="(value, key) in worker.labels"
                :key="`${worker.node_id}-${key}`"
                class="ui-badge-soft worker-label-badge inline-flex w-fit items-center rounded-default border px-2 py-1 font-mono text-[11px] leading-4"
              >
                {{ key }}={{ value }}
              </span>
            </div>
            <span v-else>--</span>
          </td>
          <td class="text-left px-6 py-4 border-b border-stroke text-sm text-primary align-middle">
            <span
              :class="[
                'inline-flex items-center justify-center rounded-default px-2.5 py-1 text-xs font-medium capitalize border',
                worker.status === 'online' ? 'ui-status-badge-online' : 'ui-status-badge-offline',
              ]"
              >{{ worker.status }}</span
            >
          </td>
          <td class="text-left px-6 py-4 border-b border-stroke text-sm text-primary align-middle">
            <div>{{ formatDateTime(worker.registered_at) }}</div>
            <div class="mt-1 text-secondary font-mono text-xs">
              {{ formatAge(worker.last_seen_at) }}
            </div>
          </td>
          <td class="text-left px-6 py-4 border-b border-stroke text-sm text-primary align-middle">
            <div class="inline-flex gap-2 items-center">
              <button
                type="button"
                class="ui-btn-danger rounded-md px-3 py-1.5 text-[13px] font-medium h-8 inline-flex items-center justify-center border transition-all duration-200 disabled:cursor-not-allowed disabled:opacity-50"
                :disabled="deletingNodeId === worker.node_id"
                @click="emit('deleteWorker', worker.node_id)"
              >
                {{ deleteWorkerButtonText(worker.node_id) }}
              </button>
            </div>
          </td>
        </tr>
      </tbody>
    </table>
  </div>
</template>
