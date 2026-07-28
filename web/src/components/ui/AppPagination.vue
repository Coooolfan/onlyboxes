<script setup lang="ts">
import { computed } from 'vue'

import AppButton from '@/components/ui/AppButton.vue'

const props = defineProps<{
  page: number
  totalPages: number
  total: number
  pageSize: number
  loading?: boolean
  /** Noun used in the range summary, e.g. "workers". */
  itemLabel?: string
}>()

const emit = defineEmits<{
  prev: []
  next: []
}>()

const rangeText = computed(() => {
  const start = props.total === 0 ? 0 : (props.page - 1) * props.pageSize + 1
  const end = Math.min(props.page * props.pageSize, props.total)
  return `${start}-${end} / ${props.total}`
})

const canPrev = computed(() => props.page > 1 && !props.loading)
const canNext = computed(() => props.page < props.totalPages && !props.loading)
</script>

<template>
  <nav
    class="flex items-center justify-between gap-4 px-6 py-4 max-[700px]:flex-col max-[700px]:items-stretch"
    aria-label="Pagination"
  >
    <p class="m-0 text-[13px] text-secondary">
      Showing {{ rangeText }}<span v-if="itemLabel"> {{ itemLabel }}</span>
    </p>
    <div class="inline-flex items-center justify-end gap-3">
      <AppButton size="sm" icon="chevron-left" :disabled="!canPrev" @click="emit('prev')">
        Prev
      </AppButton>
      <span class="text-[13px] whitespace-nowrap text-secondary">
        Page {{ page }} / {{ totalPages }}
      </span>
      <AppButton size="sm" :disabled="!canNext" @click="emit('next')">Next</AppButton>
    </div>
  </nav>
</template>
