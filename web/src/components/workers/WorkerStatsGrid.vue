<script setup lang="ts">
import { computed } from 'vue'

type StatTone = 'online' | 'offline' | 'neutral' | 'stale'

const props = defineProps<{
  totalWorkers: number
  onlineWorkers: number
  offlineWorkers: number
  staleWorkers: number
  activeSessions: number
  staleWorkersLabel: string
}>()

const toneClass: Record<StatTone, string> = {
  online: 'text-online',
  offline: 'text-offline',
  neutral: 'text-primary',
  stale: 'text-stale',
}

const cards = computed<Array<{ key: string; label: string; value: string; tone: StatTone }>>(() => [
  {
    key: 'online',
    label: 'Online Workers',
    value: `${props.onlineWorkers}/${props.totalWorkers}`,
    tone: 'online',
  },
  { key: 'offline', label: 'Offline', value: String(props.offlineWorkers), tone: 'offline' },
  {
    key: 'sessions',
    label: 'Active Sessions',
    value: String(props.activeSessions),
    tone: 'neutral',
  },
  { key: 'stale', label: props.staleWorkersLabel, value: String(props.staleWorkers), tone: 'stale' },
])
</script>

<template>
  <section
    class="grid grid-cols-4 gap-5 max-[960px]:grid-cols-2 max-[620px]:grid-cols-1 max-[620px]:gap-4"
    aria-label="Worker fleet summary"
  >
    <article
      v-for="(card, index) in cards"
      :key="card.key"
      class="ui-rise flex flex-col justify-center rounded-lg border border-stroke bg-surface p-6 shadow-card transition-[transform,box-shadow,border-color] duration-200 hover:-translate-y-0.5 hover:border-stroke-hover hover:shadow-card-hover"
      :style="{ '--ui-rise-delay': `${index * 40}ms` }"
    >
      <p class="m-0 text-sm font-medium text-secondary">{{ card.label }}</p>
      <p
        class="mt-3 mb-0 font-mono text-[32px] leading-none font-bold tracking-[-0.02em]"
        :class="toneClass[card.tone]"
      >
        {{ card.value }}
      </p>
    </article>
  </section>
</template>
