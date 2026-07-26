<script setup lang="ts">
type ToggleOption = {
  value: string
  label: string
  testId?: string
}

const props = defineProps<{
  modelValue: string
  options: ToggleOption[]
}>()

const emit = defineEmits<{
  'update:modelValue': [value: string]
}>()

function handleSelect(value: string): void {
  emit('update:modelValue', value)
}
</script>

<template>
  <div class="inline-flex w-fit items-center rounded-md border border-stroke bg-surface p-1">
    <button
      v-for="option in props.options"
      :key="option.value"
      :data-testid="option.testId"
      type="button"
      class="rounded px-3 py-1 text-sm transition-colors"
      :class="
        props.modelValue === option.value
          ? 'bg-accent ui-on-accent'
          : 'text-secondary hover:text-primary'
      "
      @click="handleSelect(option.value)"
    >
      {{ option.label }}
    </button>
  </div>
</template>
