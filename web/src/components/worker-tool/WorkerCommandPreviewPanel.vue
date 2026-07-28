<script setup lang="ts">
import SegmentedToggle from '@/components/worker-tool/SegmentedToggle.vue'

type IssueItem = {
  level: 'error' | 'warning'
  message: string
}

type PreviewMode = 'command' | 'config-file'

const props = defineProps<{
  commandText: string
  configTomlText: string
  previewMode: PreviewMode
  issueItems: IssueItem[]
  hasErrors: boolean
  copyButtonText: string
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
]

function handlePreviewModeUpdate(value: string): void {
  emit('update:previewMode', value === 'config-file' ? 'config-file' : 'command')
}
</script>

<template>
  <div class="rounded-lg border border-stroke bg-surface shadow-card flex flex-col">
    <div class="px-4 py-3 border-b border-stroke flex items-center justify-between gap-3 flex-wrap">
      <div>
        <p class="m-0 text-sm font-medium text-primary">
          {{ props.previewMode === 'command' ? 'Startup Command Preview' : 'config.toml Preview' }}
        </p>
        <p class="m-0 text-xs text-secondary">
          {{
            props.previewMode === 'command'
              ? 'Rendered in multiline shell format.'
              : 'Place the file next to the worker binary. Environment variables override it.'
          }}
        </p>
      </div>
      <div class="flex items-center gap-2">
        <SegmentedToggle
          :model-value="props.previewMode"
          :options="previewModeOptions"
          @update:model-value="handlePreviewModeUpdate"
        />
        <button
          v-if="props.previewMode === 'command'"
          data-testid="copy-startup-command"
          type="button"
          class="ui-btn-primary rounded-md px-3 py-1.5 text-[13px] font-medium h-8 inline-flex items-center justify-center border transition-all duration-200 disabled:cursor-not-allowed disabled:opacity-50"
          :disabled="props.copyDisabled"
          @click="emit('copy')"
        >
          {{ props.copyButtonText }}
        </button>
        <button
          v-else
          data-testid="download-config-file"
          type="button"
          class="ui-btn-primary rounded-md px-3 py-1.5 text-[13px] font-medium h-8 inline-flex items-center justify-center border transition-all duration-200 disabled:cursor-not-allowed disabled:opacity-50"
          :disabled="props.downloadDisabled"
          @click="emit('download')"
        >
          Download config.toml
        </button>
      </div>
    </div>

    <div class="p-4 grid gap-3">
      <code
        v-if="props.previewMode === 'command'"
        data-testid="startup-command-preview"
        class="ui-code-block block rounded-default border p-4 font-mono text-[13px] leading-[1.6] whitespace-pre-wrap break-all"
        >{{ props.commandText }}</code
      >
      <code
        v-else
        data-testid="config-file-preview"
        class="ui-code-block block rounded-default border p-4 font-mono text-[13px] leading-[1.6] whitespace-pre-wrap break-all"
        >{{ props.configTomlText || 'config.toml is not available for this startup preset.' }}</code
      >

      <div
        v-if="props.issueItems.length > 0"
        class="ui-alert rounded-md px-3 py-2 text-xs"
        :class="props.hasErrors ? 'ui-alert-error' : 'ui-alert-warning'"
      >
        <p class="m-0 text-primary font-medium mb-1">Command Notice</p>
        <ul class="m-0 pl-4 grid gap-1 text-secondary">
          <li
            v-for="item in props.issueItems"
            :key="`preview-${item.level}-${item.message}`"
            :class="item.level === 'error' ? 'text-offline' : 'text-stale'"
          >
            {{ item.message }}
          </li>
        </ul>
      </div>
    </div>
  </div>
</template>
