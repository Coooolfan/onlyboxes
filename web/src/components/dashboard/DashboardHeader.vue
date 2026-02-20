<script setup lang="ts">
defineProps<{
  creatingWorker: boolean
  autoRefreshEnabled: boolean
  loading: boolean
  showCreateAccount?: boolean
}>()

const emit = defineEmits<{
  addWorker: []
  toggleAutoRefresh: []
  refresh: []
  logout: []
  createAccount: []
}>()
</script>

<template>
  <header class="dashboard-header">
    <div>
      <p class="eyebrow">Onlyboxes / Worker Registry</p>
      <h1>Execution Node Control Panel</h1>
      <p class="subtitle">Real-time monitoring for worker registration and heartbeat health.</p>
    </div>

    <div class="header-actions">
      <button
        v-if="showCreateAccount"
        class="ghost-btn"
        type="button"
        @click="emit('createAccount')"
      >
        Create Account
      </button>
      <button
        class="primary-btn"
        type="button"
        :disabled="creatingWorker"
        @click="emit('addWorker')"
      >
        {{ creatingWorker ? 'Adding...' : 'Add Worker' }}
      </button>
      <button class="ghost-btn" type="button" @click="emit('toggleAutoRefresh')">
        {{ autoRefreshEnabled ? 'Auto Refresh: ON' : 'Auto Refresh: OFF' }}
      </button>
      <button class="primary-btn" type="button" :disabled="loading" @click="emit('refresh')">
        {{ loading ? 'Refreshing...' : 'Refresh Now' }}
      </button>
      <button class="ghost-btn" type="button" @click="emit('logout')">Logout</button>
    </div>
  </header>
</template>

<style scoped>
.dashboard-header {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 20px;
  background: var(--surface);
  border: 1px solid var(--stroke);
  border-radius: var(--radius-lg);
  padding: 32px;
  box-shadow: var(--shadow);
  animation: rise-in 500ms ease-out;
}

.eyebrow {
  margin: 0;
  font-family: 'JetBrains Mono', monospace;
  font-size: 12px;
  letter-spacing: 0.05em;
  text-transform: uppercase;
  color: var(--text-secondary);
}

h1 {
  margin: 12px 0 8px;
  font-size: 24px;
  font-weight: 600;
  line-height: 1.2;
  letter-spacing: -0.02em;
}

.subtitle {
  margin: 0;
  color: var(--text-secondary);
  font-size: 14px;
  max-width: 62ch;
  line-height: 1.5;
}

.header-actions {
  display: flex;
  align-items: center;
  gap: 12px;
}

@media (max-width: 960px) {
  .dashboard-header {
    flex-direction: column;
  }

  .header-actions {
    width: 100%;
    flex-wrap: wrap;
  }
}
</style>
