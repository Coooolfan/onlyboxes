<script setup lang="ts">
defineProps<{
  message: string
}>()
</script>

<template>
  <p class="error-banner">{{ message }}</p>
</template>

<style scoped>
.error-banner {
  margin: 0;
  border: 1px solid #fca5a5;
  border-radius: var(--radius);
  background: #fef2f2;
  color: #e00;
  padding: 10px 16px;
  font-size: 14px;
}
</style>
