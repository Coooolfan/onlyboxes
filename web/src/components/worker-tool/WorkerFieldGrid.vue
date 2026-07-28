<script setup lang="ts">
import { computed } from 'vue'

import AppField from '@/components/ui/AppField.vue'
import AppInput from '@/components/ui/AppInput.vue'
import type { WorkerFieldSpec } from '@/components/worker-tool/workerFieldSpecs'

const props = defineProps<{
  /** Reactive runtime config; fields are read and written by `spec.key`. */
  config: object
  fields: ReadonlyArray<WorkerFieldSpec>
}>()

const target = computed(() => props.config as Record<string, unknown>)

function readValue(field: WorkerFieldSpec): string | number {
  const value = target.value[field.key]
  if (field.kind === 'number') {
    return typeof value === 'number' ? value : Number.NaN
  }
  return typeof value === 'string' ? value : ''
}

function writeValue(field: WorkerFieldSpec, value: string | number): void {
  target.value[field.key] = value
}
</script>

<template>
  <AppField
    v-for="field in fields"
    :key="field.key"
    :label="field.label"
    :hint="field.hint"
    :span="field.span"
  >
    <AppInput
      :model-value="readValue(field)"
      :type="field.kind === 'number' ? 'number' : 'text'"
      :placeholder="field.placeholder"
      :min="field.min"
      :max="field.max"
      :step="field.step"
      :trim="field.kind === 'text'"
      @update:model-value="writeValue(field, $event)"
    />
  </AppField>
</template>
