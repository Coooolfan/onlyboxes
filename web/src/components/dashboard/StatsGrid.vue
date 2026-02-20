<script setup lang="ts">
defineProps<{
  totalWorkers: number
  onlineWorkers: number
  offlineWorkers: number
  staleWorkers: number
  staleWorkersLabel: string
}>()
</script>

<template>
  <section class="stats-grid">
    <article class="stat-card">
      <p class="stat-title">Total Workers</p>
      <p class="stat-value">{{ totalWorkers }}</p>
    </article>
    <article class="stat-card">
      <p class="stat-title">Online</p>
      <p class="stat-value online">{{ onlineWorkers }}</p>
    </article>
    <article class="stat-card">
      <p class="stat-title">Offline</p>
      <p class="stat-value offline">{{ offlineWorkers }}</p>
    </article>
    <article class="stat-card">
      <p class="stat-title">{{ staleWorkersLabel }}</p>
      <p class="stat-value stale">{{ staleWorkers }}</p>
    </article>
  </section>
</template>

<style scoped>
.stats-grid {
  display: grid;
  grid-template-columns: repeat(4, 1fr);
  gap: 20px;
}

.stat-card {
  background: var(--surface);
  border: 1px solid var(--stroke);
  border-radius: var(--radius-lg);
  padding: 24px;
  box-shadow: var(--shadow);
  display: flex;
  flex-direction: column;
  justify-content: center;
  transition:
    transform 0.2s ease,
    box-shadow 0.2s ease;
  animation: rise-in 550ms ease-out;
}

.stat-card:hover {
  transform: translateY(-2px);
  box-shadow: var(--shadow-hover);
  border-color: var(--stroke-hover);
}

.stat-title {
  margin: 0;
  font-size: 14px;
  color: var(--text-secondary);
  font-weight: 500;
}

.stat-value {
  margin: 12px 0 0;
  font-size: 32px;
  font-weight: 700;
  line-height: 1;
  color: var(--text-primary);
  font-family: 'JetBrains Mono', monospace;
  letter-spacing: -0.02em;
}

.stat-value.online {
  color: var(--online);
}

.stat-value.offline {
  color: var(--offline);
}

.stat-value.stale {
  color: var(--stale);
}

@media (max-width: 960px) {
  .stats-grid {
    grid-template-columns: repeat(2, 1fr);
  }
}

@media (max-width: 620px) {
  .stats-grid {
    grid-template-columns: 1fr;
    gap: 16px;
  }
}
</style>
