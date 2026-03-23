<script setup lang="ts">
defineProps<{
  message: string
}>()
</script>

<template>
  <p class="ui-alert ui-alert-error m-0 rounded-default px-4 py-2.5 text-sm">
    {{ message }}
  </p>
</template>
