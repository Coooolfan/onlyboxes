<script setup lang="ts">
import { computed, ref } from 'vue'

import CopyButton from '@/components/ui/CopyButton.vue'
import { useCopyFeedback } from '@/composables/useCopyFeedback'
import type { MCPSnippet, MCPSnippetKey } from '@/composables/useMCPSnippets'

const props = defineProps<{
  snippets: MCPSnippet[]
}>()

const activeKey = ref<MCPSnippetKey>('claude-code')
const copyFeedback = useCopyFeedback({ fallbackErrorMessage: 'Failed to copy template.' })

const activeSnippet = computed<MCPSnippet | null>(() => {
  if (props.snippets.length === 0) {
    return null
  }
  return props.snippets.find((snippet) => snippet.key === activeKey.value) ?? props.snippets[0]!
})

function selectSnippet(key: MCPSnippetKey): void {
  if (activeKey.value === key) {
    return
  }
  activeKey.value = key
  copyFeedback.reset()
}
</script>

<template>
  <section class="grid gap-3">
    <p class="m-0 text-sm font-semibold text-primary">Quick Setup</p>

    <div
      role="tablist"
      aria-label="Token quick setup snippets"
      class="ui-inset-surface grid grid-cols-3 gap-2 rounded-default border p-2 max-[700px]:grid-cols-1"
    >
      <button
        v-for="snippet in snippets"
        :key="snippet.key"
        type="button"
        role="tab"
        :aria-selected="activeKey === snippet.key"
        class="ui-focusable cursor-pointer rounded-default border px-3 py-2 text-left transition-[background-color,border-color,color,box-shadow] duration-200"
        :class="activeKey === snippet.key ? 'ui-tab-active' : 'ui-tab-inactive'"
        @click="selectSnippet(snippet.key)"
      >
        <span class="block font-mono text-[11px] lowercase">{{ snippet.label }}</span>
        <span class="mt-1 block text-xs">{{ snippet.kind }}</span>
      </button>
    </div>

    <div v-if="activeSnippet" class="ui-inset-surface grid gap-2.5 rounded-default border p-3">
      <code
        class="ui-code-block token-usage-value block rounded-default border p-3 font-mono text-xs leading-[1.55] break-all whitespace-pre-wrap"
        >{{ activeSnippet.value }}</code
      >
      <div class="flex justify-end">
        <CopyButton
          :status="copyFeedback.statusFor(activeSnippet.key)"
          @copy="copyFeedback.copy(activeSnippet.value, activeSnippet.key)"
        />
      </div>
    </div>
  </section>
</template>
