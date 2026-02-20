<script setup lang="ts">
import { RouterView } from 'vue-router'
</script>

<template>
  <div class="dashboard-shell">
    <RouterView />
  </div>
</template>

<style scoped>
.dashboard-shell {
  position: relative;
  min-height: 100vh;
  padding: 32px 24px;
}

@media (max-width: 620px) {
  .dashboard-shell {
    padding: 24px 16px;
  }
}
</style>
