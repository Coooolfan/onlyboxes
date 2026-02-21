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
  <footer class="flex justify-between items-center gap-4 px-6 py-4 bg-surface-soft border-t border-stroke max-[960px]:flex-col max-[960px]:items-start">
    <span class="font-sans text-[13px] text-secondary">Showing {{ footerText }}</span>
    <div class="inline-flex items-center gap-3">
      <button
        type="button"
        class="rounded-md px-3 py-1.5 text-[13px] font-medium h-8 inline-flex items-center justify-center text-primary bg-surface border border-stroke transition-all duration-200 hover:not-disabled:border-stroke-hover hover:not-disabled:bg-surface-soft disabled:cursor-not-allowed disabled:opacity-50"
        :disabled="!canPrev || loading"
        @click="emit('prev')"
      >
        Prev
      </button>
      <span class="font-sans text-[13px] text-secondary">Page {{ page }} / {{ totalPages }}</span>
      <button
        type="button"
        class="rounded-md px-3 py-1.5 text-[13px] font-medium h-8 inline-flex items-center justify-center text-primary bg-surface border border-stroke transition-all duration-200 hover:not-disabled:border-stroke-hover hover:not-disabled:bg-surface-soft disabled:cursor-not-allowed disabled:opacity-50"
        :disabled="!canNext || loading"
        @click="emit('next')"
      >
        Next
      </button>
    </div>
  </footer>
</template>
