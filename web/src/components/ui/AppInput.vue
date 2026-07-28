<script lang="ts">
export default { inheritAttrs: false }
</script>

<script setup lang="ts">
import { computed, ref } from 'vue'

import AppButton from '@/components/ui/AppButton.vue'

type InputType = 'text' | 'password' | 'number' | 'email'

const props = withDefaults(
  defineProps<{
    modelValue: string | number
    type?: InputType
    placeholder?: string
    disabled?: boolean
    required?: boolean
    autocomplete?: string
    spellcheck?: boolean
    maxlength?: number
    min?: number | string
    max?: number | string
    step?: number | string
    /** Trims surrounding whitespace on input for text-like fields. */
    trim?: boolean
    /** Adds a show/hide toggle for password fields. */
    revealable?: boolean
    /** Constrains the control width, e.g. for short numeric inputs. */
    width?: string
  }>(),
  {
    type: 'text',
    disabled: false,
    required: false,
    trim: false,
    revealable: false,
  },
)

const emit = defineEmits<{ 'update:modelValue': [value: string | number] }>()

const revealed = ref(false)

const resolvedType = computed(() => {
  if (props.type === 'password' && props.revealable && revealed.value) {
    return 'text'
  }
  return props.type
})

function handleInput(event: Event): void {
  const target = event.target as HTMLInputElement
  if (props.type === 'number') {
    emit('update:modelValue', target.value === '' ? Number.NaN : Number(target.value))
    return
  }
  emit('update:modelValue', props.trim ? target.value.trim() : target.value)
}
</script>

<template>
  <span class="flex min-w-0 items-center gap-2">
    <input
      :value="modelValue"
      :type="resolvedType"
      :placeholder="placeholder"
      :disabled="disabled"
      :required="required"
      :autocomplete="autocomplete"
      :spellcheck="spellcheck"
      :maxlength="maxlength"
      :min="min"
      :max="max"
      :step="step"
      class="ui-input h-9 min-w-0 flex-1 rounded-md border px-3 text-sm disabled:cursor-not-allowed disabled:opacity-70"
      :style="width ? { flex: '0 0 auto', width } : undefined"
      v-bind="$attrs"
      @input="handleInput"
    />
    <AppButton
      v-if="type === 'password' && revealable"
      size="sm"
      :icon="revealed ? 'eye-off' : 'eye'"
      icon-only
      :aria-label="revealed ? 'Hide value' : 'Show value'"
      @click="revealed = !revealed"
    />
  </span>
</template>
