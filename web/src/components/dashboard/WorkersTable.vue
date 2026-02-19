<script setup lang="ts">
import type { WorkerItem } from '@/types/workers'

defineProps<{
  workerRows: WorkerItem[]
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
</script>

<template>
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
                class="ghost-btn small danger"
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

<style scoped>
.table-wrap {
  overflow: auto;
}

table {
  width: 100%;
  border-collapse: collapse;
  min-width: 920px;
}

th,
td {
  text-align: left;
  padding: 14px 16px;
  border-bottom: 1px solid var(--stroke);
  vertical-align: top;
}

th {
  font-family: 'IBM Plex Mono', monospace;
  font-size: 12px;
  text-transform: uppercase;
  letter-spacing: 0.05em;
  color: var(--text-secondary);
  background: #fafbfd;
  position: sticky;
  top: 0;
  z-index: 1;
}

td {
  font-size: 14px;
  color: #1b2230;
}

.row-actions {
  display: inline-flex;
  gap: 8px;
  align-items: center;
}

.node-main {
  font-weight: 700;
}

.node-sub {
  margin-top: 4px;
  color: var(--text-secondary);
  font-family: 'IBM Plex Mono', monospace;
  font-size: 12px;
}

.status-pill {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  border-radius: 999px;
  padding: 4px 10px;
  font-size: 12px;
  font-weight: 700;
  text-transform: uppercase;
  letter-spacing: 0.04em;
}

.status-pill.online {
  color: #05563d;
  background: #d7f8eb;
}

.status-pill.offline {
  color: #7f1f1f;
  background: #ffe2de;
}

.empty-cell {
  color: var(--text-secondary);
  text-align: center;
  padding: 36px 12px;
}
</style>
