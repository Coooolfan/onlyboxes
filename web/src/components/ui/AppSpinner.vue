<script setup lang="ts">
withDefaults(defineProps<{ size?: number; label?: string }>(), { size: 16 })
</script>

<template>
  <span
    class="ui-spinner inline-block shrink-0 rounded-full border-2 border-current border-r-transparent align-[-2px]"
    :style="{ width: `${size}px`, height: `${size}px` }"
    role="status"
    :aria-label="label"
  />
</template>
