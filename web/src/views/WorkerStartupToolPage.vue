<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { onBeforeRouteLeave } from 'vue-router'

import ConsoleHeader from '@/components/dashboard/ConsoleHeader.vue'
import WorkerBoxliteConfigForm from '@/components/worker-tool/WorkerBoxliteConfigForm.vue'
import WorkerCommandPreviewPanel from '@/components/worker-tool/WorkerCommandPreviewPanel.vue'
import WorkerDockerConfigForm from '@/components/worker-tool/WorkerDockerConfigForm.vue'
import WorkerProfileSelector from '@/components/worker-tool/WorkerProfileSelector.vue'
import WorkerSysConfigForm from '@/components/worker-tool/WorkerSysConfigForm.vue'
import { consumePrefill } from '@/composables/useWorkerStartupPrefill'
import { useWorkerStartupTool } from '@/composables/useWorkerStartupTool'
import { writeTextToClipboard } from '@/utils/clipboard'

const prefill = consumePrefill()
const openedFromGoToStartupTool = prefill !== null
const leavePromptMessage =
  'WORKER_ID and WORKER_SECRET are already filled in. This is the last time the key will be visible. Please confirm you have saved them before leaving this page.'

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

const copyingCommand = ref(false)
const copiedCommand = ref(false)
const copyFailed = ref(false)

let copyFeedbackTimer: ReturnType<typeof setTimeout> | null = null

function normalizeHeartbeatForAutoTimeout(value: number): number {
  const parsed = Number.isFinite(value) ? Math.floor(value) : Number.NaN
  const heartbeat = parsed > 0 ? parsed : 5
  return Math.floor((heartbeat * 5 + 1) / 2)
}

const dockerAutoCallTimeoutSec = computed(() =>
  normalizeHeartbeatForAutoTimeout(workerDockerConfig.heartbeatIntervalSec),
)

const boxliteAutoCallTimeoutSec = computed(() =>
  normalizeHeartbeatForAutoTimeout(workerBoxliteConfig.heartbeatIntervalSec),
)

const sysAutoCallTimeoutSec = computed(() =>
  normalizeHeartbeatForAutoTimeout(workerSysConfig.heartbeatIntervalSec),
)

const issueItems = computed(() => {
  return [
    ...errorMessages.value.map((message) => ({ level: 'error' as const, message })),
    ...warningMessages.value.map((message) => ({ level: 'warning' as const, message })),
  ]
})

const hasErrors = computed(() => errorMessages.value.length > 0)

const copyButtonText = computed(() => {
  if (copyingCommand.value) {
    return 'Copying...'
  }
  if (copiedCommand.value) {
    return 'Copied'
  }
  if (copyFailed.value) {
    return 'Copy Failed'
  }
  return 'Copy Startup Command'
})

const copyDisabled = computed(() => !canCopyCommand.value || copyingCommand.value)

const whitelistModeDescription = computed(() => {
  if (workerSysConfig.computerUseCommandWhitelistMode === 'prefix') {
    return 'Prefix mode: command must start with one whitelist entry.'
  }
  if (workerSysConfig.computerUseCommandWhitelistMode === 'allow_all') {
    return 'Allow-all mode: whitelist entries are ignored.'
  }
  return 'Exact mode: command must exactly match one whitelist entry.'
})

watch(
  () => [workerDockerConfig.callTimeoutMode, workerDockerConfig.heartbeatIntervalSec],
  () => {
    if (workerDockerConfig.callTimeoutMode !== 'auto') {
      return
    }
    workerDockerConfig.callTimeoutSec = dockerAutoCallTimeoutSec.value
  },
  { immediate: true },
)

watch(
  () => [workerBoxliteConfig.callTimeoutMode, workerBoxliteConfig.heartbeatIntervalSec],
  () => {
    if (workerBoxliteConfig.callTimeoutMode !== 'auto') {
      return
    }
    workerBoxliteConfig.callTimeoutSec = boxliteAutoCallTimeoutSec.value
  },
  { immediate: true },
)

watch(
  () => [workerSysConfig.callTimeoutMode, workerSysConfig.heartbeatIntervalSec],
  () => {
    if (workerSysConfig.callTimeoutMode !== 'auto') {
      return
    }
    workerSysConfig.callTimeoutSec = sysAutoCallTimeoutSec.value
  },
  { immediate: true },
)

watch(
  () => [workerBoxliteConfig.terminalLeaseMinSec, workerBoxliteConfig.terminalLeaseMaxSec],
  () => {
    const min = Number.isFinite(workerBoxliteConfig.terminalLeaseMinSec)
      ? Math.floor(workerBoxliteConfig.terminalLeaseMinSec)
      : Number.NaN
    const max = Number.isFinite(workerBoxliteConfig.terminalLeaseMaxSec)
      ? Math.floor(workerBoxliteConfig.terminalLeaseMaxSec)
      : Number.NaN
    if (min <= 0 || max <= 0) {
      return
    }
    if (max < min) {
      workerBoxliteConfig.terminalLeaseMaxSec = min
    }
  },
)

watch(
  () => [
    workerBoxliteConfig.terminalLeaseMinSec,
    workerBoxliteConfig.terminalLeaseMaxSec,
    workerBoxliteConfig.terminalLeaseDefaultSec,
  ],
  () => {
    const min = Number.isFinite(workerBoxliteConfig.terminalLeaseMinSec)
      ? Math.floor(workerBoxliteConfig.terminalLeaseMinSec)
      : Number.NaN
    const max = Number.isFinite(workerBoxliteConfig.terminalLeaseMaxSec)
      ? Math.floor(workerBoxliteConfig.terminalLeaseMaxSec)
      : Number.NaN
    const currentDefault = Number.isFinite(workerBoxliteConfig.terminalLeaseDefaultSec)
      ? Math.floor(workerBoxliteConfig.terminalLeaseDefaultSec)
      : Number.NaN
    if (min <= 0 || max <= 0 || !Number.isFinite(currentDefault)) {
      return
    }
    if (currentDefault < min) {
      workerBoxliteConfig.terminalLeaseDefaultSec = min
      return
    }
    if (currentDefault > max) {
      workerBoxliteConfig.terminalLeaseDefaultSec = max
    }
  },
)

watch(
  () => [workerDockerConfig.terminalLeaseMinSec, workerDockerConfig.terminalLeaseMaxSec],
  () => {
    const min = Number.isFinite(workerDockerConfig.terminalLeaseMinSec)
      ? Math.floor(workerDockerConfig.terminalLeaseMinSec)
      : Number.NaN
    const max = Number.isFinite(workerDockerConfig.terminalLeaseMaxSec)
      ? Math.floor(workerDockerConfig.terminalLeaseMaxSec)
      : Number.NaN
    if (min <= 0 || max <= 0) {
      return
    }
    if (max < min) {
      workerDockerConfig.terminalLeaseMaxSec = min
    }
  },
)

watch(
  () => [
    workerDockerConfig.terminalLeaseMinSec,
    workerDockerConfig.terminalLeaseMaxSec,
    workerDockerConfig.terminalLeaseDefaultSec,
  ],
  () => {
    const min = Number.isFinite(workerDockerConfig.terminalLeaseMinSec)
      ? Math.floor(workerDockerConfig.terminalLeaseMinSec)
      : Number.NaN
    const max = Number.isFinite(workerDockerConfig.terminalLeaseMaxSec)
      ? Math.floor(workerDockerConfig.terminalLeaseMaxSec)
      : Number.NaN
    const currentDefault = Number.isFinite(workerDockerConfig.terminalLeaseDefaultSec)
      ? Math.floor(workerDockerConfig.terminalLeaseDefaultSec)
      : Number.NaN
    if (min <= 0 || max <= 0 || !Number.isFinite(currentDefault)) {
      return
    }
    if (currentDefault < min) {
      workerDockerConfig.terminalLeaseDefaultSec = min
      return
    }
    if (currentDefault > max) {
      workerDockerConfig.terminalLeaseDefaultSec = max
    }
  },
)

function resetCopyFeedback(): void {
  if (copyFeedbackTimer) {
    clearTimeout(copyFeedbackTimer)
    copyFeedbackTimer = null
  }
  copyingCommand.value = false
  copiedCommand.value = false
  copyFailed.value = false
}

function scheduleCopyFeedbackReset(): void {
  if (copyFeedbackTimer) {
    clearTimeout(copyFeedbackTimer)
  }
  copyFeedbackTimer = setTimeout(() => {
    copiedCommand.value = false
    copyFailed.value = false
    copyFeedbackTimer = null
  }, 1500)
}

async function copyStartupCommand(): Promise<void> {
  if (copyingCommand.value || !canCopyCommand.value) {
    return
  }
  resetCopyFeedback()
  copyingCommand.value = true
  try {
    await writeTextToClipboard(commandText.value, {
      fallbackErrorMessage: 'Failed to copy startup command.',
    })
    copiedCommand.value = true
    scheduleCopyFeedbackReset()
  } catch {
    copyFailed.value = true
    scheduleCopyFeedbackReset()
  } finally {
    copyingCommand.value = false
  }
}

function handleBeforeUnload(event: BeforeUnloadEvent): void {
  if (!openedFromGoToStartupTool) {
    return
  }
  event.preventDefault()
  event.returnValue = leavePromptMessage
}

onMounted(() => {
  if (!openedFromGoToStartupTool) {
    return
  }
  window.addEventListener('beforeunload', handleBeforeUnload)
})

onBeforeRouteLeave(() => {
  if (!openedFromGoToStartupTool) {
    return true
  }
  return window.confirm(leavePromptMessage)
})

onBeforeUnmount(() => {
  resetCopyFeedback()
  if (openedFromGoToStartupTool) {
    window.removeEventListener('beforeunload', handleBeforeUnload)
  }
})
</script>

<template>
  <main class="relative z-2 mx-auto w-[min(1240px,100%)] grid gap-6">
    <ConsoleHeader eyebrow="Onlyboxes / Worker Tool" title="Worker Startup Tool" hide-refresh>
      <template #subtitle>
        Configure startup parameters for worker-docker, worker-boxlite, and worker-sys, then copy a
        ready-to-run startup command.
      </template>
    </ConsoleHeader>

    <section class="grid gap-4">
      <div
        class="rounded-lg border border-stroke bg-surface shadow-card p-5 overflow-y-auto grid gap-5"
      >
        <WorkerProfileSelector v-model="workerKind" />

        <div
          v-if="openedFromGoToStartupTool"
          data-testid="prefilled-credentials-notice"
          class="ui-alert ui-alert-warning rounded-md px-3 py-2 text-sm"
        >
          WORKER_ID and WORKER_SECRET are already filled in from the worker creation result.
        </div>

        <div class="h-px bg-stroke/80"></div>

        <WorkerDockerConfigForm
          v-if="workerKind === 'worker-docker'"
          :config="workerDockerConfig"
          :auto-call-timeout-sec="dockerAutoCallTimeoutSec"
          :show-prefilled-credential-hint="openedFromGoToStartupTool"
        />
        <WorkerBoxliteConfigForm
          v-else-if="workerKind === 'worker-boxlite'"
          :config="workerBoxliteConfig"
          :auto-call-timeout-sec="boxliteAutoCallTimeoutSec"
          :show-prefilled-credential-hint="openedFromGoToStartupTool"
        />
        <WorkerSysConfigForm
          v-else
          :config="workerSysConfig"
          :auto-call-timeout-sec="sysAutoCallTimeoutSec"
          :whitelist-mode-description="whitelistModeDescription"
          :show-prefilled-credential-hint="openedFromGoToStartupTool"
          @apply-temporary-probe="selectTemporaryProbePreset"
        />

        <div
          v-if="issueItems.length > 0"
          class="ui-alert rounded-md px-3 py-2 text-sm"
          :class="hasErrors ? 'ui-alert-error' : 'ui-alert-warning'"
        >
          <p class="m-0 text-primary font-medium mb-2">Validation</p>
          <ul class="m-0 pl-4 grid gap-1 text-secondary">
            <li
              v-for="item in issueItems"
              :key="`${item.level}-${item.message}`"
              :class="item.level === 'error' ? 'text-offline' : 'text-stale'"
            >
              {{ item.message }}
            </li>
          </ul>
        </div>
      </div>

      <WorkerCommandPreviewPanel
        :command-text="commandText"
        :issue-items="issueItems"
        :has-errors="hasErrors"
        :copy-button-text="copyButtonText"
        :copy-disabled="copyDisabled"
        @copy="copyStartupCommand"
      />
    </section>
  </main>
</template>
