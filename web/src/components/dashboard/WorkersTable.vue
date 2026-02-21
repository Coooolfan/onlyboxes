<script setup lang="ts">
import { computed } from 'vue'

import type { WorkerItem, WorkerInflightItem } from '@/types/workers'

const props = defineProps<{
  workerRows: WorkerItem[]
  inflightWorkers: WorkerInflightItem[]
  loading: boolean
  deletingNodeId: string
  formatCapabilities: (worker: WorkerItem) => string
  formatLabels: (worker: WorkerItem) => string
  formatDateTime: (value: string) => string
  formatAge: (value: string) => string
  deleteWorkerButtonText: (nodeID: string) => string
}>()

const emit = defineEmits<{
  deleteWorker: [nodeID: string]
}>()

type InflightCapability = WorkerInflightItem['capabilities'][number]

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

function getInflight(nodeId: string, capName: string): InflightCapability | null {
  const normalized = normalizeCapabilityName(capName)
  if (!normalized) {
    return null
  }
  return inflightByWorker.value.get(nodeId)?.get(normalized) ?? null
}
</script>

<template>
  <div class="overflow-auto">
    <table class="w-full border-collapse min-w-[920px]">
      <thead>
        <tr>
          <th class="text-left px-6 py-4 border-b border-stroke text-[13px] font-medium text-secondary bg-surface-soft sticky top-0 z-1 align-middle">Node</th>
          <th class="text-left px-6 py-4 border-b border-stroke text-[13px] font-medium text-secondary bg-surface-soft sticky top-0 z-1 align-middle">Runtime</th>
          <th class="text-left px-6 py-4 border-b border-stroke text-[13px] font-medium text-secondary bg-surface-soft sticky top-0 z-1 align-middle">Capabilities</th>
          <th class="text-left px-6 py-4 border-b border-stroke text-[13px] font-medium text-secondary bg-surface-soft sticky top-0 z-1 align-middle">Labels</th>
          <th class="text-left px-6 py-4 border-b border-stroke text-[13px] font-medium text-secondary bg-surface-soft sticky top-0 z-1 align-middle">Status</th>
          <th class="text-left px-6 py-4 border-b border-stroke text-[13px] font-medium text-secondary bg-surface-soft sticky top-0 z-1 align-middle">Registered / Last Heartbeat</th>
          <th class="text-left px-6 py-4 border-b border-stroke text-[13px] font-medium text-secondary bg-surface-soft sticky top-0 z-1 align-middle">Actions</th>
        </tr>
      </thead>
      <tbody>
        <tr v-if="!loading && workerRows.length === 0">
          <td colspan="7" class="text-secondary text-center px-6 py-12 text-sm border-b border-stroke align-middle">No workers found in current filter.</td>
        </tr>
        <tr v-for="worker in workerRows" :key="worker.node_id" class="transition-colors duration-200 hover:bg-surface-soft">
          <td class="text-left px-6 py-4 border-b border-stroke text-sm text-primary align-middle">
            <div class="font-semibold">{{ worker.node_name || worker.node_id }}</div>
            <div class="mt-1 text-secondary font-mono text-xs">{{ worker.node_id }}</div>
          </td>
          <td class="text-left px-6 py-4 border-b border-stroke text-sm text-primary align-middle">
            <div>{{ worker.executor_kind || '--' }}</div>
            <div class="mt-1 text-secondary font-mono text-xs">version: {{ worker.version || '--' }}</div>
          </td>
          <td class="text-left px-6 py-4 border-b border-stroke text-sm text-primary align-middle">
            <div
              class="flex flex-wrap gap-1.5"
              v-if="worker.capabilities && worker.capabilities.length > 0"
            >
              <span
                class="inline-flex items-center justify-center px-2 py-1 bg-surface-soft border border-stroke rounded-default font-mono text-[11px] text-secondary gap-1.5"
                v-for="cap in worker.capabilities"
                :key="cap.name"
              >
                {{ cap.name }}
                <span
                  v-if="getInflight(worker.node_id, cap.name)"
                  :class="[
                    'text-[10px] px-1 py-px rounded-[4px] border',
                    getInflight(worker.node_id, cap.name)!.inflight >= getInflight(worker.node_id, cap.name)!.max_inflight
                      ? 'text-[#b45309] bg-[#fffbeb] border-[#fcd34d]'
                      : getInflight(worker.node_id, cap.name)!.inflight > 0
                        ? 'text-primary border-stroke-hover bg-surface-soft'
                        : 'text-(--text-tertiary) bg-surface border-stroke'
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
          <td class="text-left px-6 py-4 border-b border-stroke text-sm text-primary align-middle">{{ formatLabels(worker) }}</td>
          <td class="text-left px-6 py-4 border-b border-stroke text-sm text-primary align-middle">
            <span :class="[
              'inline-flex items-center justify-center rounded-default px-2.5 py-1 text-xs font-medium capitalize',
              worker.status === 'online'
                ? 'text-[#166534] bg-[#f0fdf4] border border-[#bbf7d0]'
                : 'text-[#991b1b] bg-[#fef2f2] border border-[#fecaca]'
            ]">{{ worker.status }}</span>
          </td>
          <td class="text-left px-6 py-4 border-b border-stroke text-sm text-primary align-middle">
            <div>{{ formatDateTime(worker.registered_at) }}</div>
            <div class="mt-1 text-secondary font-mono text-xs">{{ formatAge(worker.last_seen_at) }}</div>
          </td>
          <td class="text-left px-6 py-4 border-b border-stroke text-sm text-primary align-middle">
            <div class="inline-flex gap-2 items-center">
              <button
                type="button"
                class="rounded-md px-3 py-1.5 text-[13px] font-medium h-8 inline-flex items-center justify-center text-offline bg-white border border-[#fca5a5] transition-all duration-200 hover:not-disabled:bg-[#fef2f2] hover:not-disabled:border-[#f87171] disabled:cursor-not-allowed disabled:opacity-50"
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
