<script setup lang="ts">
export type SegmentedOption<T extends string> = {
  value: T
  label: string
  testId?: string
}

type Size = 'sm' | 'md'

withDefaults(
  defineProps<{
    modelValue: string
    options: ReadonlyArray<SegmentedOption<string>>
    size?: Size
    ariaLabel?: string
    /** Stretches segments to fill the available width. */
    block?: boolean
  }>(),
  { size: 'md', block: false },
)

const emit = defineEmits<{ 'update:modelValue': [value: string] }>()

const sizeClass: Record<Size, string> = {
  sm: 'px-3 py-1 text-[13px]',
  md: 'px-4 py-1.5 text-[13px]',
}
</script>

<template>
  <div
    class="inline-flex items-center gap-1 rounded-default border border-stroke bg-surface p-1"
    :class="block ? 'w-full' : 'w-fit'"
    role="tablist"
    :aria-label="ariaLabel"
  >
    <button
      v-for="option in options"
      :key="option.value"
      type="button"
      role="tab"
      :aria-selected="modelValue === option.value"
      :data-testid="option.testId"
      class="ui-focusable cursor-pointer rounded-[6px] font-medium transition-[background-color,color] duration-200"
      :class="[
        sizeClass[size],
        block ? 'flex-1' : '',
        modelValue === option.value
          ? 'bg-accent ui-on-accent'
          : 'bg-transparent text-secondary hover:bg-surface-soft hover:text-primary',
      ]"
      @click="emit('update:modelValue', option.value)"
    >
      {{ option.label }}
    </button>
  </div>
</template>
