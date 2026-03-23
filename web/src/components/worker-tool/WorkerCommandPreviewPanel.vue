<script setup lang="ts">
type IssueItem = {
  level: 'error' | 'warning'
  message: string
}

const props = defineProps<{
  commandText: string
  issueItems: IssueItem[]
  hasErrors: boolean
  copyButtonText: string
  copyDisabled: boolean
}>()

const emit = defineEmits<{
  copy: []
}>()
</script>

<template>
  <div class="rounded-lg border border-stroke bg-surface shadow-card flex flex-col">
    <div class="px-4 py-3 border-b border-stroke flex items-center justify-between gap-3">
      <div>
        <p class="m-0 text-sm font-medium text-primary">Startup Command Preview</p>
        <p class="m-0 text-xs text-secondary">Rendered in multiline shell format.</p>
      </div>
      <button
        data-testid="copy-startup-command"
        type="button"
        class="ui-btn-primary rounded-md px-3 py-1.5 text-[13px] font-medium h-8 inline-flex items-center justify-center border transition-all duration-200 disabled:cursor-not-allowed disabled:opacity-50"
        :disabled="props.copyDisabled"
        @click="emit('copy')"
      >
        {{ props.copyButtonText }}
      </button>
    </div>

    <div class="p-4 grid gap-3">
      <code
        data-testid="startup-command-preview"
        class="ui-code-block block rounded-default border p-4 font-mono text-[13px] leading-[1.6] whitespace-pre-wrap break-all"
        >{{ props.commandText }}</code
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
