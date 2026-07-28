<script setup lang="ts">
withDefaults(
  defineProps<{
    title?: string
    subtitle?: string
    /** Removes the default body padding, e.g. for edge-to-edge tables. */
    flush?: boolean
  }>(),
  { flush: false },
)
</script>

<template>
  <section
    class="ui-rise overflow-hidden rounded-lg border border-stroke bg-surface shadow-card max-[620px]:rounded-default"
  >
    <header
      v-if="title || $slots.header || $slots.actions"
      class="flex items-start justify-between gap-4 border-b border-stroke bg-surface-soft px-6 py-5 max-[700px]:flex-col"
    >
      <slot name="header">
        <div class="grid gap-1">
          <h2 class="m-0 text-lg font-semibold">{{ title }}</h2>
          <p v-if="subtitle" class="m-0 text-sm text-secondary">{{ subtitle }}</p>
        </div>
      </slot>
      <div v-if="$slots.actions" class="flex items-center gap-3">
        <slot name="actions" />
      </div>
    </header>

    <div :class="flush ? '' : 'p-6'">
      <slot />
    </div>

    <footer v-if="$slots.footer" class="border-t border-stroke bg-surface-soft">
      <slot name="footer" />
    </footer>
  </section>
</template>
