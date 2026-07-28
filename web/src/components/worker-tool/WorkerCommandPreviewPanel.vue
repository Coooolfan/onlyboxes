<script setup lang="ts">
import { computed } from 'vue'

import AppButton from '@/components/ui/AppButton.vue'
import AppSegmented from '@/components/ui/AppSegmented.vue'
import CopyButton from '@/components/ui/CopyButton.vue'
import WorkerIssueList from '@/components/worker-tool/WorkerIssueList.vue'
import type { WorkerIssue } from '@/components/worker-tool/issues'
import type { CopyStatus } from '@/composables/useCopyFeedback'

export type PreviewMode = 'command' | 'config-file'

const props = defineProps<{
  commandText: string
  configTomlText: string
  previewMode: PreviewMode
  issues: WorkerIssue[]
  copyStatus: CopyStatus
  copyDisabled: boolean
  downloadDisabled: boolean
}>()

const emit = defineEmits<{
  copy: []
  download: []
  'update:previewMode': [value: PreviewMode]
}>()

const previewModeOptions = [
  { value: 'command', label: 'Startup Command', testId: 'preview-mode-command-btn' },
  { value: 'config-file', label: 'config.toml', testId: 'preview-mode-config-file-btn' },
] as const satisfies ReadonlyArray<{ value: PreviewMode; label: string; testId: string }>

const isCommandMode = computed(() => props.previewMode === 'command')

const title = computed(() =>
  isCommandMode.value ? 'Startup Command Preview' : 'config.toml Preview',
)

const description = computed(() =>
  isCommandMode.value
    ? 'Rendered in multiline shell format.'
    : 'Place the file next to the worker binary. Environment variables override it.',
)

function handlePreviewModeUpdate(value: string): void {
  emit('update:previewMode', value === 'config-file' ? 'config-file' : 'command')
}
</script>

<template>
  <div class="flex flex-col rounded-lg border border-stroke bg-surface shadow-card">
    <div
      class="flex flex-wrap items-center justify-between gap-3 border-b border-stroke px-4 py-3"
    >
      <div>
        <p class="m-0 text-sm font-medium text-primary">{{ title }}</p>
        <p class="m-0 text-xs text-secondary">{{ description }}</p>
      </div>
      <div class="flex items-center gap-2">
        <AppSegmented
          :model-value="previewMode"
          :options="previewModeOptions"
          size="sm"
          aria-label="Preview mode"
          @update:model-value="handlePreviewModeUpdate"
        />
        <CopyButton
          v-if="isCommandMode"
          variant="primary"
          label="Copy Startup Command"
          data-testid="copy-startup-command"
          :status="copyStatus"
          :disabled="copyDisabled"
          @copy="emit('copy')"
        />
        <AppButton
          v-else
          variant="primary"
          size="sm"
          icon="download"
          data-testid="download-config-file"
          :disabled="downloadDisabled"
          @click="emit('download')"
        >
          Download config.toml
        </AppButton>
      </div>
    </div>

    <div class="grid gap-3 p-4">
      <code
        v-if="isCommandMode"
        data-testid="startup-command-preview"
        class="ui-code-block block rounded-default border p-4 font-mono text-[13px] leading-[1.6] break-all whitespace-pre-wrap"
        >{{ commandText }}</code
      >
      <code
        v-else
        data-testid="config-file-preview"
        class="ui-code-block block rounded-default border p-4 font-mono text-[13px] leading-[1.6] break-all whitespace-pre-wrap"
        >{{ configTomlText || 'config.toml is not available for this startup preset.' }}</code
      >

      <WorkerIssueList :issues="issues" compact />
    </div>
  </div>
</template>
