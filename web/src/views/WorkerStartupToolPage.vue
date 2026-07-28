<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted } from 'vue'
import { onBeforeRouteLeave } from 'vue-router'

import PageHeader from '@/components/layout/PageHeader.vue'
import AppAlert from '@/components/ui/AppAlert.vue'
import WorkerBoxliteConfigForm from '@/components/worker-tool/WorkerBoxliteConfigForm.vue'
import WorkerCommandPreviewPanel from '@/components/worker-tool/WorkerCommandPreviewPanel.vue'
import WorkerDockerConfigForm from '@/components/worker-tool/WorkerDockerConfigForm.vue'
import WorkerProfileSelector from '@/components/worker-tool/WorkerProfileSelector.vue'
import WorkerSysConfigForm from '@/components/worker-tool/WorkerSysConfigForm.vue'
import { toWorkerIssues } from '@/components/worker-tool/issues'
import { useCopyFeedback } from '@/composables/useCopyFeedback'
import {
  useCallTimeoutSync,
  useTerminalLeaseClamp,
} from '@/composables/useWorkerConfigConstraints'
import { consumePrefill } from '@/composables/useWorkerStartupPrefill'
import { useWorkerStartupTool } from '@/composables/useWorkerStartupTool'

const leavePromptMessage =
  'WORKER_ID and WORKER_SECRET are already filled in. This is the last time the key will be visible. Please confirm you have saved them before leaving this page.'

const prefill = consumePrefill()
const hasPrefilledCredentials = prefill !== null

const {
  workerKind,
  workerDockerConfig,
  workerBoxliteConfig,
  workerSysConfig,
  commandText,
  errorMessages,
  warningMessages,
  canCopyCommand,
  selectTemporaryProbePreset,
} = useWorkerStartupTool(prefill ?? undefined)

const dockerAutoCallTimeoutSec = useCallTimeoutSync(workerDockerConfig)
const boxliteAutoCallTimeoutSec = useCallTimeoutSync(workerBoxliteConfig)
const sysAutoCallTimeoutSec = useCallTimeoutSync(workerSysConfig)

useTerminalLeaseClamp(workerDockerConfig)
useTerminalLeaseClamp(workerBoxliteConfig)

const copyFeedback = useCopyFeedback({ fallbackErrorMessage: 'Failed to copy startup command.' })

const issues = computed(() => toWorkerIssues(errorMessages.value, warningMessages.value))
const copyDisabled = computed(() => !canCopyCommand.value || copyFeedback.busy.value)

function handleBeforeUnload(event: BeforeUnloadEvent): void {
  event.preventDefault()
  event.returnValue = leavePromptMessage
}

onMounted(() => {
  if (hasPrefilledCredentials) {
    window.addEventListener('beforeunload', handleBeforeUnload)
  }
})

onBeforeRouteLeave(() => {
  if (!hasPrefilledCredentials) {
    return true
  }
  return window.confirm(leavePromptMessage)
})

onBeforeUnmount(() => {
  if (hasPrefilledCredentials) {
    window.removeEventListener('beforeunload', handleBeforeUnload)
  }
})
</script>

<template>
  <div class="grid gap-6">
    <PageHeader eyebrow="Onlyboxes / Worker Tool" title="Worker Startup Tool" hide-refresh>
      <template #subtitle>
        Configure startup parameters for worker-docker, worker-boxlite, and worker-sys, then copy a
        ready-to-run startup command.
      </template>
    </PageHeader>

    <section class="grid gap-4">
      <div class="ui-rise grid gap-5 rounded-lg border border-stroke bg-surface p-5 shadow-card">
        <WorkerProfileSelector v-model="workerKind" />

        <AppAlert
          v-if="hasPrefilledCredentials"
          tone="warning"
          with-icon
          data-testid="prefilled-credentials-notice"
        >
          WORKER_ID and WORKER_SECRET are already filled in from the worker creation result.
        </AppAlert>

        <div class="h-px bg-stroke/80"></div>

        <WorkerDockerConfigForm
          v-if="workerKind === 'worker-docker'"
          :config="workerDockerConfig"
          :auto-call-timeout-sec="dockerAutoCallTimeoutSec"
          :show-prefilled-credential-hint="hasPrefilledCredentials"
        />
        <WorkerBoxliteConfigForm
          v-else-if="workerKind === 'worker-boxlite'"
          :config="workerBoxliteConfig"
          :auto-call-timeout-sec="boxliteAutoCallTimeoutSec"
          :show-prefilled-credential-hint="hasPrefilledCredentials"
        />
        <WorkerSysConfigForm
          v-else
          :config="workerSysConfig"
          :auto-call-timeout-sec="sysAutoCallTimeoutSec"
          :show-prefilled-credential-hint="hasPrefilledCredentials"
          @apply-temporary-probe="selectTemporaryProbePreset"
        />
      </div>

      <WorkerCommandPreviewPanel
        :command-text="commandText"
        :issues="issues"
        :copy-status="copyFeedback.status.value"
        :copy-disabled="copyDisabled"
        @copy="copyFeedback.copy(commandText)"
      />
    </section>
  </div>
</template>
