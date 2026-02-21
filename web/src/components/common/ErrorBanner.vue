<script setup lang="ts">
defineProps<{
  message: string
}>()
</script>

<template>
  <p class="m-0 border border-[#fca5a5] rounded-default bg-[#fef2f2] text-offline px-4 py-2.5 text-sm">{{ message }}</p>
</template>
