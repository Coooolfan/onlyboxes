<script setup lang="ts">
defineProps<{
  testId: string
}>()
</script>

<template>
  <details :data-testid="testId" class="rounded-md border border-stroke bg-surface">
    <summary
      class="ui-focusable cursor-pointer rounded-md px-3 py-2 text-sm font-medium text-primary transition-colors hover:bg-surface-soft"
    >
      Advanced Configuration
    </summary>
    <div class="grid grid-cols-1 gap-4 border-t border-stroke p-3 md:grid-cols-2">
      <slot />
    </div>
  </details>
</template>
