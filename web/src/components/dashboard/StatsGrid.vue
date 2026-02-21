<script setup lang="ts">
defineProps<{
  totalWorkers: number
  onlineWorkers: number
  offlineWorkers: number
  staleWorkers: number
  staleWorkersLabel: string
}>()
</script>

<template>
  <section class="grid grid-cols-4 gap-5 max-[960px]:grid-cols-2 max-[620px]:grid-cols-1 max-[620px]:gap-4">
    <article class="bg-surface border border-stroke rounded-lg p-6 shadow-card flex flex-col justify-center transition-[transform,box-shadow] duration-200 animate-[rise-in_550ms_ease-out] hover:-translate-y-0.5 hover:shadow-card-hover hover:border-stroke-hover">
      <p class="m-0 text-sm text-secondary font-medium">Total Workers</p>
      <p class="mt-3 mb-0 text-[32px] font-bold leading-none text-primary font-mono tracking-[-0.02em]">{{ totalWorkers }}</p>
    </article>
    <article class="bg-surface border border-stroke rounded-lg p-6 shadow-card flex flex-col justify-center transition-[transform,box-shadow] duration-200 animate-[rise-in_550ms_ease-out] hover:-translate-y-0.5 hover:shadow-card-hover hover:border-stroke-hover">
      <p class="m-0 text-sm text-secondary font-medium">Online</p>
      <p class="mt-3 mb-0 text-[32px] font-bold leading-none text-online font-mono tracking-[-0.02em]">{{ onlineWorkers }}</p>
    </article>
    <article class="bg-surface border border-stroke rounded-lg p-6 shadow-card flex flex-col justify-center transition-[transform,box-shadow] duration-200 animate-[rise-in_550ms_ease-out] hover:-translate-y-0.5 hover:shadow-card-hover hover:border-stroke-hover">
      <p class="m-0 text-sm text-secondary font-medium">Offline</p>
      <p class="mt-3 mb-0 text-[32px] font-bold leading-none text-offline font-mono tracking-[-0.02em]">{{ offlineWorkers }}</p>
    </article>
    <article class="bg-surface border border-stroke rounded-lg p-6 shadow-card flex flex-col justify-center transition-[transform,box-shadow] duration-200 animate-[rise-in_550ms_ease-out] hover:-translate-y-0.5 hover:shadow-card-hover hover:border-stroke-hover">
      <p class="m-0 text-sm text-secondary font-medium">{{ staleWorkersLabel }}</p>
      <p class="mt-3 mb-0 text-[32px] font-bold leading-none text-stale font-mono tracking-[-0.02em]">{{ staleWorkers }}</p>
    </article>
  </section>
</template>
