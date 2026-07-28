<script setup lang="ts">
import { computed } from 'vue'

import AppAlert from '@/components/ui/AppAlert.vue'
import type { WorkerIssue } from '@/components/worker-tool/issues'

const props = withDefaults(
  defineProps<{
    issues: WorkerIssue[]
    title?: string
    /** Uses the smaller type scale for the command preview panel. */
    compact?: boolean
  }>(),
  { title: 'Validation', compact: false },
)

const hasErrors = computed(() => props.issues.some((issue) => issue.level === 'error'))
</script>

<template>
  <AppAlert
    v-if="issues.length > 0"
    :tone="hasErrors ? 'error' : 'warning'"
    :title="title"
    :class="compact ? 'text-xs' : 'text-sm'"
  >
    <ul class="m-0 grid list-disc gap-1 pl-4">
      <li
        v-for="issue in issues"
        :key="`${issue.level}-${issue.message}`"
        :class="issue.level === 'error' ? 'text-offline' : 'text-stale'"
      >
        {{ issue.message }}
      </li>
    </ul>
  </AppAlert>
</template>
