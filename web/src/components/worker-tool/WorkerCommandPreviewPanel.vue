<script setup lang="ts">
import CopyButton from '@/components/ui/CopyButton.vue'
import WorkerIssueList from '@/components/worker-tool/WorkerIssueList.vue'
import type { WorkerIssue } from '@/components/worker-tool/issues'
import type { CopyStatus } from '@/composables/useCopyFeedback'

defineProps<{
  commandText: string
  issues: WorkerIssue[]
  copyStatus: CopyStatus
  copyDisabled: boolean
}>()

const emit = defineEmits<{ copy: [] }>()
</script>

<template>
  <div class="flex flex-col rounded-lg border border-stroke bg-surface shadow-card">
    <div class="flex items-center justify-between gap-3 border-b border-stroke px-4 py-3">
      <div>
        <p class="m-0 text-sm font-medium text-primary">Startup Command Preview</p>
        <p class="m-0 text-xs text-secondary">Rendered in multiline shell format.</p>
      </div>
      <CopyButton
        variant="primary"
        label="Copy Startup Command"
        data-testid="copy-startup-command"
        :status="copyStatus"
        :disabled="copyDisabled"
        @copy="emit('copy')"
      />
    </div>

    <div class="grid gap-3 p-4">
      <code
        data-testid="startup-command-preview"
        class="ui-code-block block rounded-default border p-4 font-mono text-[13px] leading-[1.6] break-all whitespace-pre-wrap"
        >{{ commandText }}</code
      >

      <WorkerIssueList :issues="issues" title="Command Notice" compact />
    </div>
  </div>
</template>
