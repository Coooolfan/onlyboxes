<script setup lang="ts">
import { computed } from 'vue'

import AppField from '@/components/ui/AppField.vue'
import AppInput from '@/components/ui/AppInput.vue'
import AppSegmented from '@/components/ui/AppSegmented.vue'
import AppTextarea from '@/components/ui/AppTextarea.vue'
import WorkerAdvancedSection from '@/components/worker-tool/WorkerAdvancedSection.vue'
import WorkerCoreConfigFields from '@/components/worker-tool/WorkerCoreConfigFields.vue'
import WorkerFieldGrid from '@/components/worker-tool/WorkerFieldGrid.vue'
import { sysAdvancedNumberFields } from '@/components/worker-tool/workerFieldSpecs'
import type {
  WorkerSysStartupConfig,
  WorkerSysStartupPreset,
  WorkerSysWhitelistMode,
} from '@/types/worker-startup-tool'

const props = defineProps<{
  config: WorkerSysStartupConfig
  autoCallTimeoutSec: number
  showPrefilledCredentialHint: boolean
}>()

const emit = defineEmits<{
  applyTemporaryProbe: []
}>()

const presetOptions = [
  { value: 'custom', label: 'Custom' },
  { value: 'temporary-probe', label: 'Temporary Probe' },
] as const satisfies ReadonlyArray<{ value: WorkerSysStartupPreset; label: string }>

const whitelistModeOptions = [
  { value: 'exact', label: 'exact' },
  { value: 'prefix', label: 'prefix' },
  { value: 'allow_all', label: 'allow_all' },
] as const satisfies ReadonlyArray<{ value: WorkerSysWhitelistMode; label: string }>

const isTemporaryProbe = computed(() => props.config.startupPreset === 'temporary-probe')
const allowAllMode = computed(() => props.config.computerUseCommandWhitelistMode === 'allow_all')

const whitelistModeDescription = computed(() => {
  switch (props.config.computerUseCommandWhitelistMode) {
    case 'prefix':
      return 'Prefix mode: command must start with one whitelist entry.'
    case 'allow_all':
      return 'Allow-all mode: whitelist entries are ignored.'
    default:
      return 'Exact mode: command must exactly match one whitelist entry.'
  }
})

function handlePresetUpdate(value: string): void {
  if (value === 'temporary-probe') {
    emit('applyTemporaryProbe')
    return
  }
  if (value === 'custom') {
    props.config.startupPreset = 'custom'
  }
}

function handleWhitelistModeUpdate(value: string): void {
  if (value === 'exact' || value === 'prefix' || value === 'allow_all') {
    props.config.computerUseCommandWhitelistMode = value
  }
}
</script>

<template>
  <div class="grid gap-4">
    <section class="grid gap-2 rounded-md border border-stroke bg-surface-soft p-3">
      <div class="grid gap-1">
        <h2 class="m-0 text-base font-semibold">worker-sys Preset</h2>
        <p class="m-0 text-xs text-secondary">
          Temporary Probe prepares a short installer command for quick host checks.
        </p>
      </div>
      <AppSegmented
        :model-value="props.config.startupPreset"
        :options="presetOptions"
        size="sm"
        aria-label="worker-sys startup preset"
        data-testid="sys-preset-toggle"
        @update:model-value="handlePresetUpdate"
      />
    </section>

    <h2 class="m-0 text-base font-semibold">Core Configuration</h2>

    <WorkerCoreConfigFields
      :config="props.config"
      :auto-call-timeout-sec="props.autoCallTimeoutSec"
      :show-prefilled-credential-hint="props.showPrefilledCredentialHint"
      :show-optional-fields="!isTemporaryProbe"
      binary-path-placeholder="./onlyboxes-worker-sys"
    >
      <AppField
        v-if="isTemporaryProbe"
        label="Temporary Probe release tag"
        hint="Optional installer release tag override. Leave empty to use the latest published release."
        span
      >
        <AppInput
          v-model="props.config.temporaryProbeTag"
          data-testid="temporary-probe-tag-input"
          trim
          placeholder="latest"
        />
      </AppField>
    </WorkerCoreConfigFields>

    <WorkerAdvancedSection v-if="!isTemporaryProbe" test-id="sys-advanced-section">
      <WorkerFieldGrid :config="props.config" :fields="sysAdvancedNumberFields" />

      <div class="grid content-start gap-1.5">
        <span class="text-sm text-secondary">WORKER_COMPUTER_USE_COMMAND_WHITELIST_MODE</span>
        <span class="text-xs text-secondary">{{ whitelistModeDescription }}</span>
        <AppSegmented
          :model-value="props.config.computerUseCommandWhitelistMode"
          :options="whitelistModeOptions"
          size="sm"
          aria-label="computerUse whitelist mode"
          @update:model-value="handleWhitelistModeUpdate"
        />
      </div>

      <AppField
        label="WORKER_COMPUTER_USE_COMMAND_WHITELIST (one entry per line)"
        hint="Serialized as JSON array. In exact/prefix mode, empty list blocks all commands."
        :note="allowAllMode ? 'Disabled because allow_all mode ignores whitelist entries.' : undefined"
        span
      >
        <AppTextarea
          v-model="props.config.computerUseCommandWhitelistText"
          data-testid="sys-whitelist-textarea"
          :rows="3"
          :disabled="allowAllMode"
          placeholder="echo&#10;time"
        />
      </AppField>

      <AppField
        label="WORKER_READ_IMAGE_ALLOWED_PATHS (one path per line)"
        hint="Serialized as JSON array. Empty value means readImage is denied by default."
        span
      >
        <AppTextarea
          v-model="props.config.readImageAllowedPathsText"
          data-testid="sys-paths-textarea"
          :rows="3"
          placeholder="/data/images&#10;/tmp/a.png"
        />
      </AppField>
    </WorkerAdvancedSection>
  </div>
</template>
