<script setup lang="ts">
import type { WorkerStatus } from '@/types/workers'

defineProps<{
  statusFilter: WorkerStatus
  refreshedAtText: string
}>()

const emit = defineEmits<{
  setStatus: [status: WorkerStatus]
}>()
</script>

<template>
  <div class="panel-topbar">
    <div class="tabs">
      <button
        type="button"
        :class="['tab-btn', { active: statusFilter === 'all' }]"
        @click="emit('setStatus', 'all')"
      >
        All
      </button>
      <button
        type="button"
        :class="['tab-btn', { active: statusFilter === 'online' }]"
        @click="emit('setStatus', 'online')"
      >
        Online
      </button>
      <button
        type="button"
        :class="['tab-btn', { active: statusFilter === 'offline' }]"
        @click="emit('setStatus', 'offline')"
      >
        Offline
      </button>
    </div>

    <p class="panel-meta">
      Last refresh:
      <span>{{ refreshedAtText }}</span>
    </p>
  </div>
</template>

<style scoped>
.panel-topbar {
  display: flex;
  justify-content: space-between;
  align-items: center;
  gap: 16px;
  padding: 16px 24px;
  border-bottom: 1px solid var(--stroke);
  background: var(--surface-soft);
}

.tabs {
  display: inline-flex;
  gap: 4px;
  background: var(--surface);
  border: 1px solid var(--stroke);
  border-radius: var(--radius);
  padding: 4px;
}

.tab-btn {
  border-radius: calc(var(--radius) - 2px);
  padding: 6px 16px;
  font-size: 13px;
  font-weight: 500;
  color: var(--text-secondary);
  background: transparent;
  transition: all 0.2s ease;
}

.tab-btn:hover:not(.active) {
  color: var(--text-primary);
  background: var(--surface-soft);
}

.tab-btn.active {
  color: #ffffff;
  background: var(--accent);
}

.panel-meta {
  margin: 0;
  color: var(--text-secondary);
  font-size: 13px;
}

.panel-meta span {
  color: var(--text-primary);
  font-weight: 500;
}

@media (max-width: 960px) {
  .panel-topbar {
    flex-direction: column;
    align-items: flex-start;
  }
}
</style>
