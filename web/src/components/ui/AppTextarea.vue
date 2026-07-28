<script lang="ts">
export default { inheritAttrs: false }
</script>

<script setup lang="ts">
defineProps<{
  modelValue: string
  rows?: number
  placeholder?: string
  disabled?: boolean
}>()

const emit = defineEmits<{ 'update:modelValue': [value: string] }>()
</script>

<template>
  <textarea
    :value="modelValue"
    :rows="rows ?? 3"
    :placeholder="placeholder"
    :disabled="disabled"
    class="ui-input rounded-md border px-3 py-2 text-sm disabled:cursor-not-allowed disabled:opacity-60"
    v-bind="$attrs"
    @input="emit('update:modelValue', ($event.target as HTMLTextAreaElement).value)"
  ></textarea>
</template>
