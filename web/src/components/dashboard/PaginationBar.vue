<script setup lang="ts">
defineProps<{
  footerText: string
  page: number
  totalPages: number
  canPrev: boolean
  canNext: boolean
  loading: boolean
}>()

const emit = defineEmits<{
  prev: []
  next: []
}>()
</script>

<template>
  <footer class="panel-footer">
    <span class="footer-meta">Showing {{ footerText }}</span>
    <div class="pager">
      <button
        type="button"
        class="ghost-btn small"
        :disabled="!canPrev || loading"
        @click="emit('prev')"
      >
        Prev
      </button>
      <span class="page-indicator">Page {{ page }} / {{ totalPages }}</span>
      <button
        type="button"
        class="ghost-btn small"
        :disabled="!canNext || loading"
        @click="emit('next')"
      >
        Next
      </button>
    </div>
  </footer>
</template>

<style scoped>
.panel-footer {
  display: flex;
  justify-content: space-between;
  align-items: center;
  gap: 16px;
  padding: 16px 24px;
  background: var(--surface-soft);
  border-top: 1px solid var(--stroke);
}

.footer-meta,
.page-indicator {
  font-family: 'Inter', sans-serif;
  font-size: 13px;
  color: var(--text-secondary);
}

.pager {
  display: inline-flex;
  align-items: center;
  gap: 12px;
}

@media (max-width: 960px) {
  .panel-footer {
    flex-direction: column;
    align-items: flex-start;
  }
}
</style>
